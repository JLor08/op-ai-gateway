// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/store"
	"slices"
	"strconv"
	"strings"
	"time"
)

// AgentFeatureRPCCluster is the feature an agent declares when it runs
// llama.cpp RPC cluster heads and rpc_server members. A cluster head and its
// members reach a runtime document only when every agent involved declares
// it: an older agent would run the head without its endpoints, or the member
// as a model.
const AgentFeatureRPCCluster = "rpc_cluster"

// agentRPCClusterSince is the first agent version that declares
// AgentFeatureRPCCluster.
var agentRPCClusterSince = [3]int{0, 18, 0}

// AgentSpecKindRPCServer is the runtime-document kind of a cluster member
// spec; a model spec has no kind.
const AgentSpecKindRPCServer = "rpc_server"

// Reasons a cluster head is withheld from its server's runtime document
// (RuntimeSpecDTO.ClusterWithheldReason). The head and every member spec
// derived from it are withheld together, so an agent never receives a
// partial endpoint list. ClusterWithheldFeaturesPending withholds the
// cluster while the declaration of an agent of it is unknown and its last
// reported version does not tell either; a server whose OWN agent's
// declaration is unknown has its whole document held instead
// (AgentRuntimeConfigDTO.AwaitsAgentFeatures), while the other servers'
// documents go out without that cluster's head or member.
const (
	ClusterWithheldFeaturesPending   = "runtime_config.agent_features_pending"
	ClusterWithheldAgentTooOld       = "runtime.cluster_agent_too_old"
	ClusterWithheldMemberMissing     = "runtime.cluster_member_missing"
	ClusterWithheldMemberDisabled    = "runtime.cluster_member_disabled"
	ClusterWithheldMemberConsent     = "runtime.cluster_member_consent_withdrawn"
	ClusterWithheldMemberUnmanaged   = "runtime.cluster_member_unmanaged"
	ClusterWithheldNoMatchingBinary  = "runtime.cluster_member_no_matching_binary"
	ClusterWithheldAddressUnknown    = "runtime.cluster_address_unknown"
	ClusterWithheldPortUnknown       = "runtime.cluster_port_unknown"
	ClusterWithheldMeshUnavailable   = "runtime.cluster_mesh_unavailable"
	ClusterWithheldDirectUnavailable = "runtime.cluster_direct_unavailable"
	ClusterWithheldLANAckRequired    = "runtime.cluster_lan_ack_required"
	// ClusterWithheldLANInterfaceMissing: a member that listens on its LAN
	// address only (lan_bind_only) has chosen an interface its agent no
	// longer reports; it is never moved to a fallback interface.
	ClusterWithheldLANInterfaceMissing = "runtime.cluster_lan_interface_missing"
	// ClusterWithheldMemberSpecInvalid: a manual member spec's stored args or
	// env cannot be read (store data, not operator input).
	ClusterWithheldMemberSpecInvalid = "runtime.cluster_member_spec_invalid"
)

// Member process defaults: a member has no startup timeout of its own (a
// ggml-rpc-server is TCP-ready within seconds), and its idle timeout is short
// because a member is only ever held while its head runs.
const (
	memberStartupTimeoutSeconds = 60
	memberIdleTimeoutSeconds    = 60
)

// AgentClusterEndpointDTO is one member of a head's cluster block: "host:port"
// as the head's llama-server receives it in --rpc, and the number of devices
// that member exposes (RPC<n> names).
type AgentClusterEndpointDTO struct {
	Address     string `json:"address"`
	DeviceCount int    `json:"device_count"`
}

// AgentClusterHeadDTO makes a model spec a cluster head: its members in
// member order (the order fixes the RPC<n> numbering), the transport and the
// network. Generation is a digest of every member launch input the
// endpoints do not show (binaries and their versions, args, env, the cache
// switch, device backends, GPU selections, bind addresses), so whatever
// relaunches a member relaunches the head.
type AgentClusterHeadDTO struct {
	Endpoints  []AgentClusterEndpointDTO `json:"endpoints"`
	Transport  string                    `json:"transport"`
	Network    string                    `json:"network"`
	Generation string                    `json:"generation,omitempty"`
}

// AgentRPCMemberDTO is an rpc_server spec's network settings: the head it
// serves, the address and port it (or its relay) listens on, and who the
// head is -- its addresses, or a name the member agent resolves.
type AgentRPCMemberDTO struct {
	HeadSpecID   string   `json:"head_spec_id"`
	Transport    string   `json:"transport"`
	BindIP       string   `json:"bind_ip"`
	Port         int      `json:"port"`
	HeadIPs      []string `json:"head_ips"`
	HeadDomain   string   `json:"head_domain"`
	CacheEnabled bool     `json:"cache_enabled"`
}

// DerivesIdleTimeout reports whether spec's pushed idle timeout is derived
// from its measured startup and its mapping's benchmark cold start
// (effectiveIdleTimeoutSeconds) rather than pushed as stored: a replicable
// spec, and a cluster head, whose cold start waits for every member and is
// worth keeping warm for a multiple of it. A writer of either figure outside
// this service pushes the document for such a spec.
func DerivesIdleTimeout(spec routing.RuntimeSpec) bool {
	return spec.Replicable || spec.ClusterEnabled
}

// clusterDerivation is a head's cluster as the runtime documents carry it:
// the head's cluster block and one rpc_server spec per member, or the reason
// none of it is pushed.
type clusterDerivation struct {
	reason string
	head   *AgentClusterHeadDTO
	// members are the members in member order (index = position), so two
	// members of the head on one server are two entries.
	members []derivedMemberSpec
	// headIdentity is how the members know the head: its addresses, or the
	// name they resolve (loopback when every member runs on the head's
	// server). The plan carries it (ClusterPlan).
	headIdentity []string
	// hold: the viewing server's own agent declaration is unknown, so its
	// document is held rather than served without the cluster.
	hold bool
}

func withheld(reason string) clusterDerivation { return clusterDerivation{reason: reason} }

// derivedMemberSpec is one member of a derivable cluster: its server, its
// rpc_server spec and its dial address (the host of its endpoint).
type derivedMemberSpec struct {
	serverID string
	spec     AgentRuntimeSpecDTO
	address  string
}

// derivedMember is one member with what its spec is built from. loopback: the
// member runs on the head's server (sameHostMember).
type derivedMember struct {
	clusterMemberState
	binary   routing.ServerBinary
	address  string
	bindIP   string
	loopback bool
}

// loopbackAddr is where a member on the head's own server is dialed and
// bound, and how it knows its head: nothing of it leaves the host, so it
// needs neither NetBird nor a LAN address and is outside both exposures.
const loopbackAddr = "127.0.0.1"

// sameHostMember reports whether m runs on the head's server headServerID,
// where it is addressed on loopback.
func sameHostMember(headServerID string, m routing.ClusterMember) bool {
	return m.ServerID == headServerID
}

// clusterHeadBlock returns head's cluster block for headServer's document,
// or the reason it is withheld; hold reports that headServer's own agent
// declaration is unknown, which holds its document. head must be an enabled
// cluster head.
func (s *Service) clusterHeadBlock(ctx context.Context, head routing.RuntimeSpec, headServer routing.AIServer) (block *AgentClusterHeadDTO, reason string, hold bool, err error) {
	d, err := s.deriveCluster(ctx, head, headServer, headServer.ID)
	return d.head, d.reason, d.hold, err
}

// deriveCluster derives head's cluster from its stored membership -- never
// from whether a member is up: the endpoints are the configuration, and the
// gateway starts the members before the head. Any member that cannot be
// derived withholds the whole cluster, in this order: an agent whose
// features are unknown or lack rpc_cluster; the membership (a missing or
// disabled member, a gap in the member order, consent withdrawn, a member
// server without a server_agent application, a port not allocated); the
// member binaries (an automatic member needs a ggml-rpc-server of the head
// binary's version); the network (every address known, in mesh NetBird on
// every server) and the transport (direct mesh needs the NetBird policy
// preconditions, direct lan the operator's exposure acknowledgement).
//
// viewerID is the server whose document the derivation is for; only its own
// unknown declaration holds that document (clusterDerivation.hold).
func (s *Service) deriveCluster(ctx context.Context, head routing.RuntimeSpec, headServer routing.AIServer, viewerID string) (clusterDerivation, error) {
	states, err := s.loadClusterMembers(ctx, head.ID)
	if err != nil {
		return clusterDerivation{}, err
	}
	if len(states) == 0 {
		return withheld(ClusterWithheldMemberMissing), nil
	}
	serverIDs := []string{headServer.ID}
	for _, m := range states {
		serverIDs = append(serverIDs, m.row.ServerID)
	}
	reason, hold, err := s.clusterFeatureReason(ctx, viewerID, serverIDs)
	if err != nil {
		return clusterDerivation{}, err
	}
	if reason != "" {
		d := withheld(reason)
		d.hold = hold
		return d, nil
	}
	if reason := s.sameHostFeatureReason(headServer.ID, states); reason != "" {
		return withheld(reason), nil
	}
	members, reason, err := s.checkDerivedMembers(ctx, headServer.ID, states)
	if err != nil || reason != "" {
		return withheld(reason), err
	}
	if reason, err := s.resolveMemberBinaries(ctx, head, headServer, members); err != nil || reason != "" {
		return withheld(reason), err
	}
	cn, reason, err := s.deriveClusterNetwork(ctx, head, headServer, members)
	if err != nil {
		return clusterDerivation{}, err
	}
	if reason != "" {
		return withheld(reason), nil
	}
	return s.buildCluster(ctx, head, members, cn.network, cn.transport, cn.headIPs, cn.headDomain)
}

// checkDerivedMembers is the membership stage of deriveCluster: each member
// in member order, the first reason found withholding the cluster.
func (s *Service) checkDerivedMembers(ctx context.Context, headServerID string, states []clusterMemberState) ([]derivedMember, string, error) {
	headGroups, err := s.routes.ServerAdminGroups(ctx, headServerID)
	if err != nil {
		return nil, "", err
	}
	members := make([]derivedMember, len(states))
	for i, st := range states {
		members[i].clusterMemberState = st
		reason, err := s.checkDerivedMember(ctx, i, st, headGroups)
		if err != nil || reason != "" {
			return nil, reason, err
		}
	}
	return members, "", nil
}

// derivedNetwork is a derivable cluster's network, transport and the head's
// identity as its members see it.
type derivedNetwork struct {
	network, transport string
	headIPs            []string
	headDomain         string
}

// deriveClusterNetwork is the address and transport stages of deriveCluster:
// the resolved network and transport, the head's identity, each member's
// address and bind address (set on members), then the transport's
// preconditions. A reason withholds the cluster. A member on the head's
// server is on loopback (sameHostMember): it has no network reason, and a
// cluster whose members all are needs neither the head's identity nor the
// transport's preconditions.
func (s *Service) deriveClusterNetwork(ctx context.Context, head routing.RuntimeSpec, headServer routing.AIServer, members []derivedMember) (derivedNetwork, string, error) {
	servers := []routing.AIServer{headServer}
	remote := false
	for i, m := range members {
		servers = append(servers, m.server)
		members[i].loopback = sameHostMember(headServer.ID, m.row)
		remote = remote || !members[i].loopback
	}
	var cn derivedNetwork
	var err error
	if cn.network, cn.transport, err = s.resolveClusterNetwork(ctx, head, servers); err != nil {
		return cn, "", err
	}
	if !remote {
		cn.headIPs = []string{loopbackAddr}
		for i := range members {
			members[i].bindIP, members[i].address = loopbackAddr, loopbackAddr
		}
		return cn, "", nil
	}
	var reason string
	if cn.headIPs, cn.headDomain, reason = clusterHeadIdentity(headServer, cn.network); reason != "" {
		return cn, reason, nil
	}
	for i := range members {
		if members[i].loopback {
			members[i].bindIP, members[i].address = loopbackAddr, loopbackAddr
			continue
		}
		// The bind address first: a bind-only member's missing interface is
		// the more specific reason when it also leaves no address to dial.
		if members[i].bindIP, reason = memberBindIP(members[i].server, cn.network); reason != "" {
			return cn, reason, nil
		}
		if members[i].address, reason = clusterAddress(members[i].server, cn.network); reason != "" {
			return cn, reason, nil
		}
	}
	reason, err = s.derivedTransportReason(ctx, head, cn.network, cn.transport)
	return cn, reason, err
}

// derivedTransportReason is the transport stage of deriveCluster: direct
// mesh needs the NetBird policy preconditions, direct lan the head's
// exposure acknowledgement.
func (s *Service) derivedTransportReason(ctx context.Context, head routing.RuntimeSpec, network, transport string) (string, error) {
	if transport != rpcTransportDirect {
		return "", nil
	}
	if network == clusterNetworkMesh {
		direct, err := s.directMeshAvailable(ctx)
		if err != nil {
			return "", err
		}
		if !direct {
			return ClusterWithheldDirectUnavailable, nil
		}
	}
	if network == clusterNetworkLAN && !head.LANExposureAck {
		return ClusterWithheldLANAckRequired, nil
	}
	return "", nil
}

// clusterFeatureReason is the feature part of deriveCluster over every
// server of the cluster: pending while any agent's declaration is unknown,
// too old when one is known to lack rpc_cluster. Without a feature reader
// every agent counts as known and without the feature.
//
// hold is set only when viewerID's own declaration is unknown. That is the
// gateway restart case the hold exists for: until the agent fetching the
// document has declared, the gateway cannot tell what it runs. A REMOTE
// unknown agent only withholds the cluster from the viewer's document --
// holding the whole document for it would freeze every other model on the
// viewer for as long as that other host stays away. viewerID "" is no
// document's: every server is a peer.
//
// A peer's live declaration is unknown after every gateway start until that
// host reports, and one host always reports first. So a peer counts by the
// agent version it last reported (peerFeatureReason): withholding the
// cluster until every host has reported would make the first host's agent
// drain a running head or member on every gateway restart.
func (s *Service) clusterFeatureReason(ctx context.Context, viewerID string, serverIDs []string) (reason string, hold bool, err error) {
	if s.agentFeatures == nil {
		return ClusterWithheldAgentTooOld, false, nil
	}
	if viewerID != "" && !s.agentFeatures.Known(viewerID) {
		return ClusterWithheldFeaturesPending, true, nil
	}
	pending, old := false, false
	for _, id := range serverIDs {
		r, err := s.serverFeatureReason(ctx, id)
		if err != nil {
			return "", false, err
		}
		pending = pending || r == ClusterWithheldFeaturesPending
		old = old || r == ClusterWithheldAgentTooOld
	}
	if pending {
		return ClusterWithheldFeaturesPending, false, nil
	}
	if old {
		return ClusterWithheldAgentTooOld, false, nil
	}
	return "", false, nil
}

// sameHostFeatureReason re-checks, on every derivation, what the save
// required of a same-host cluster (clusterEntries): every server that runs a
// member on the head's server, or more than one member of the head, has an
// agent that declares AgentFeatureRPCClusterSameHost. One known to lack it
// (an agent downgraded since the save) withholds the cluster as too old: it
// would read the head's loopback connection as its own probe, or the second
// member as the first. An agent whose declaration is unknown is left to the
// rpc_cluster check before this one, which holds its own document until it
// declares; once it does, its declaration change notifies the cluster again.
func (s *Service) sameHostFeatureReason(headServerID string, states []clusterMemberState) string {
	count := make(map[string]int, len(states))
	for _, st := range states {
		count[st.row.ServerID]++
	}
	for serverID, n := range count {
		if serverID != headServerID && n < 2 {
			continue
		}
		if s.agentFeatures.Known(serverID) && !s.agentFeatures.Has(serverID, AgentFeatureRPCClusterSameHost) {
			return ClusterWithheldAgentTooOld
		}
	}
	return ""
}

// serverFeatureReason is one server's feature reason: by its live
// declaration when known, else by its last reported version.
func (s *Service) serverFeatureReason(ctx context.Context, serverID string) (string, error) {
	if !s.agentFeatures.Known(serverID) {
		return s.peerFeatureReason(ctx, serverID)
	}
	if !s.agentFeatures.Has(serverID, AgentFeatureRPCCluster) {
		return ClusterWithheldAgentTooOld, nil
	}
	return "", nil
}

// peerFeatureReason judges a server whose live declaration is unknown by the
// agent version of its last telemetry: one that declares rpc_cluster counts
// as declaring it, an older one is too old, and none (or one that does not
// parse) leaves the declaration pending. The host's next report replaces the
// guess with its declaration.
func (s *Service) peerFeatureReason(ctx context.Context, serverID string) (string, error) {
	telemetry, ok, err := s.routes.TelemetryByServer(ctx, serverID)
	if err != nil {
		return "", err
	}
	if !ok {
		return ClusterWithheldFeaturesPending, nil
	}
	version, parsed := parseAgentVersion(telemetry.AgentVersion)
	switch {
	case !parsed:
		return ClusterWithheldFeaturesPending, nil
	case compareAgentVersions(version, agentRPCClusterSince) < 0:
		return ClusterWithheldAgentTooOld, nil
	}
	return "", nil
}

// parseAgentVersion reads "MAJOR.MINOR.PATCH" with an optional "v" prefix
// and an optional pre-release or build suffix, which it ignores.
func parseAgentVersion(raw string) ([3]int, bool) {
	var out [3]int
	v := strings.TrimPrefix(strings.TrimSpace(raw), "v")
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return out, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return out, false
		}
		out[i] = n
	}
	return out, true
}

func compareAgentVersions(a, b [3]int) int {
	for i := range a {
		if a[i] != b[i] {
			return a[i] - b[i]
		}
	}
	return 0
}

// checkDerivedMember is the membership part of deriveCluster for the member
// at index i. Consent is the member side's standing permission: withdrawing
// it takes the member away from the head.
func (s *Service) checkDerivedMember(ctx context.Context, i int, m clusterMemberState, headGroups []string) (string, error) {
	// Positions are stored 0..n-1; a gap is a member that went away.
	if m.row.Position != i || !m.found {
		return ClusterWithheldMemberMissing, nil
	}
	consentRaw := m.server.RPCAllowedHeadGroups
	if m.row.MemberSpecID != "" {
		if m.spec.ID == "" || m.spec.ServerID != m.server.ID {
			return ClusterWithheldMemberMissing, nil
		}
		if !m.spec.Enabled {
			return ClusterWithheldMemberDisabled, nil
		}
		consentRaw = m.spec.AllowedHeadGroups
	} else if !m.server.RPCMemberEnabled {
		return ClusterWithheldMemberDisabled, nil
	}
	admits, err := s.memberConsentAdmits(ctx, consentRaw, m.server.ID, headGroups)
	if err != nil {
		return "", err
	}
	if !admits {
		return ClusterWithheldMemberConsent, nil
	}
	managed, err := s.serverRunsAgentApplication(ctx, m.server.ID)
	if err != nil {
		return "", err
	}
	if !managed {
		return ClusterWithheldMemberUnmanaged, nil
	}
	if m.row.Port <= 0 || m.row.Port > 65535 {
		return ClusterWithheldPortUnknown, nil
	}
	return "", nil
}

// serverRunsAgentApplication reports whether serverID has a server_agent
// application: without one its agent gets the empty document and no router,
// so a member there could neither receive its spec nor be started.
func (s *Service) serverRunsAgentApplication(ctx context.Context, serverID string) (bool, error) {
	apps, err := s.routes.ApplicationsByServer(ctx, serverID)
	if err != nil {
		return false, err
	}
	return slices.ContainsFunc(apps, func(a routing.Application) bool { return a.Type == routing.ProviderServerAgent }), nil
}

// resolveMemberBinaries sets each member's binary: an automatic member runs
// its server's ggml-rpc-server the head binary can drive (MatchingRPCBinary:
// the same build or RPC protocol, read now, so an inventory change moves
// it), a manual
// member its spec's binary with whatever version the inventory knows.
func (s *Service) resolveMemberBinaries(ctx context.Context, head routing.RuntimeSpec, headServer routing.AIServer, members []derivedMember) (string, error) {
	headBin := lazyHeadBinary{serverID: headServer.ID, path: head.Binary}
	for i := range members {
		m := &members[i]
		if m.row.MemberSpecID != "" {
			if err := s.resolveManualMemberBinary(ctx, m); err != nil {
				return "", err
			}
			continue
		}
		if reason, err := s.resolveAutomaticMemberBinary(ctx, m, &headBin); err != nil || reason != "" {
			return reason, err
		}
	}
	return "", nil
}

// lazyHeadBinary is the head binary's inventory row, read on first use: only
// an automatic member needs it.
type lazyHeadBinary struct {
	serverID, path string
	row            routing.ServerBinary
	known, read    bool
}

func (s *Service) headBinaryRow(ctx context.Context, h *lazyHeadBinary) (routing.ServerBinary, bool, error) {
	if !h.read {
		row, listed, err := s.BinaryVersionFor(ctx, h.serverID, h.path)
		if err != nil {
			return routing.ServerBinary{}, false, err
		}
		h.row, h.known, h.read = row, listed && binaryVersionKnown(row), true
	}
	return h.row, h.known, nil
}

// resolveManualMemberBinary sets a manual member's binary: its spec's, with
// whatever version the inventory knows.
func (s *Service) resolveManualMemberBinary(ctx context.Context, m *derivedMember) error {
	b, ok, err := s.BinaryVersionFor(ctx, m.server.ID, m.spec.Binary)
	if err != nil {
		return err
	}
	if !ok {
		b = routing.ServerBinary{ServerID: m.server.ID, Path: m.spec.Binary}
	}
	m.binary = b
	return nil
}

// resolveAutomaticMemberBinary sets an automatic member's binary: its
// server's ggml-rpc-server the head binary can drive (MatchingRPCBinary).
// ClusterWithheldNoMatchingBinary when the head's version is unknown or no
// such binary is listed.
func (s *Service) resolveAutomaticMemberBinary(ctx context.Context, m *derivedMember, headBin *lazyHeadBinary) (string, error) {
	headRow, headKnown, err := s.headBinaryRow(ctx, headBin)
	if err != nil {
		return "", err
	}
	if !headKnown {
		return ClusterWithheldNoMatchingBinary, nil
	}
	b, ok, err := s.MatchingRPCBinary(ctx, m.server.ID, headRow)
	if err != nil {
		return "", err
	}
	if !ok {
		return ClusterWithheldNoMatchingBinary, nil
	}
	m.binary = b
	return "", nil
}

// clusterAddress is the address a head dials srv's member at: in mesh the
// mesh IPv4 (NetBird must be on with a tracking group), in lan the server's
// effective LAN address (LANResolutionOf), else its Domain -- except on a
// NetBird-enabled server, whose Domain the peer sync rewrites to the NetBird
// name.
func clusterAddress(srv routing.AIServer, network string) (addr, reason string) {
	if network == clusterNetworkMesh {
		if !srv.NetbirdEnabled || srv.NetbirdGroupID == "" {
			return "", ClusterWithheldMeshUnavailable
		}
		if srv.NetbirdIP == "" {
			return "", ClusterWithheldAddressUnknown
		}
		return srv.NetbirdIP, ""
	}
	res := LANResolutionOf(srv)
	switch {
	case res.Address != "":
		return res.Address, ""
	case res.Source == LANSourceDomain:
		return strings.TrimSpace(srv.Domain), ""
	}
	return "", ClusterWithheldAddressUnknown
}

// memberBindIP is the address srv's member (or its relay) listens on: the
// mesh IPv4 exactly in mesh; in lan every interface, or only the effective
// LAN address when the server asks to listen there alone. That address must
// then be known and must not be a fallback: a missing chosen interface
// withholds the member (ClusterWithheldLANInterfaceMissing) rather than move
// it to another network, and an unknown address is never silently widened to
// every interface.
func memberBindIP(srv routing.AIServer, network string) (ip, reason string) {
	if network == clusterNetworkMesh {
		return srv.NetbirdIP, ""
	}
	if !srv.LANBindOnly {
		return "0.0.0.0", ""
	}
	res := LANResolutionOf(srv)
	if res.Fallback {
		return "", ClusterWithheldLANInterfaceMissing
	}
	if res.Address == "" {
		return "", ClusterWithheldAddressUnknown
	}
	return res.Address, ""
}

// clusterHeadIdentity is how a member recognises its head: the head's mesh
// IPv4 in mesh; in lan its LANResolution.IdentityIPs (the manual address, or
// every reported IPv4 of a head on an interface source, fallback included:
// a multi-homed head may reach a member through another NIC), else its Domain
// for the member agent to resolve.
func clusterHeadIdentity(headServer routing.AIServer, network string) (ips []string, domain, reason string) {
	if network != clusterNetworkLAN {
		addr, reason := clusterAddress(headServer, network)
		if reason != "" {
			return nil, "", reason
		}
		return []string{addr}, "", ""
	}
	res := LANResolutionOf(headServer)
	switch {
	case res.Address != "":
		return res.IdentityIPs, "", ""
	case res.Source == LANSourceDomain:
		return []string{}, strings.TrimSpace(headServer.Domain), ""
	}
	return nil, "", ClusterWithheldAddressUnknown
}

// memberGenerationPart is one member's share of the head's generation:
// every input of the member's launch. Whatever relaunches a member must
// relaunch its head as well, since a head whose member restarts under it
// aborts mid-request; beyond that it records what decides which devices the
// head sees and whether the member's build talks to the head's.
type memberGenerationPart struct {
	Kind          string            `json:"kind"`
	ID            string            `json:"id"`
	Server        string            `json:"server"`
	Binary        string            `json:"binary"`
	Args          []string          `json:"args"`
	Env           map[string]string `json:"env"`
	CacheEnabled  bool              `json:"cache_enabled"`
	DeviceBackend string            `json:"device_backend"`
	Version       string            `json:"version"`
	Commit        string            `json:"commit"`
	Build         string            `json:"build"`
	DeviceCount   int               `json:"device_count"`
	GPUs          []int             `json:"gpus"`
	Transport     string            `json:"transport"`
	Network       string            `json:"network"`
	Bind          string            `json:"bind"`
	Port          int               `json:"port"`
}

// buildCluster assembles a derivable cluster: the head's block and each
// member's rpc_server spec.
func (s *Service) buildCluster(ctx context.Context, head routing.RuntimeSpec, members []derivedMember, network, transport string, headIPs []string, headDomain string) (clusterDerivation, error) {
	block := &AgentClusterHeadDTO{Endpoints: make([]AgentClusterEndpointDTO, 0, len(members)), Transport: transport, Network: network}
	parts := make([]memberGenerationPart, 0, len(members))
	out := clusterDerivation{
		head: block, members: make([]derivedMemberSpec, 0, len(members)), headIdentity: slices.Clone(headIPs),
	}
	if headDomain != "" {
		out.headIdentity = append(out.headIdentity, headDomain)
	}
	rows := make([]routing.ClusterMember, len(members))
	for i, m := range members {
		rows[i] = m.row
	}
	for _, m := range members {
		spec, err := s.memberAgentSpec(ctx, head.ID, m, MemberAgentSpecID(m.row, rows), transport, headIPs, headDomain)
		if errors.Is(err, ErrRuntimeSpecArgsInvalid) || errors.Is(err, ErrRuntimeSpecEnvInvalid) {
			// One member's unreadable row withholds its cluster; it must not
			// fail the documents (and the read view) the cluster is part of.
			return withheld(ClusterWithheldMemberSpecInvalid), nil
		}
		if err != nil {
			return clusterDerivation{}, err
		}
		indices := make([]int, 0, len(spec.GPUs))
		for _, g := range spec.GPUs {
			indices = append(indices, g.Index)
		}
		devices := len(slices.Compact(slices.Sorted(slices.Values(indices))))
		if devices == 0 {
			devices = 1 // a CPU member exposes its CPU device
		}
		block.Endpoints = append(block.Endpoints, AgentClusterEndpointDTO{
			Address: net.JoinHostPort(m.address, strconv.Itoa(m.row.Port)), DeviceCount: devices,
		})
		commit, build := binaryIdentity(m.binary)
		kind := memberModeAutomatic
		if m.row.MemberSpecID != "" {
			kind = memberModeManual
		}
		parts = append(parts, memberGenerationPart{
			Kind: kind, ID: spec.ID, Server: m.server.ID, Binary: m.binary.Path, Args: spec.Args, Env: spec.Env,
			CacheEnabled: spec.RPC.CacheEnabled, DeviceBackend: spec.DeviceBackend,
			Version: m.binary.EffectiveVersion(),
			Commit:  commit, Build: build, DeviceCount: devices, GPUs: indices, Transport: spec.RPC.Transport, Network: network,
			Bind: m.bindIP, Port: m.row.Port,
		})
		out.members = append(out.members, derivedMemberSpec{serverID: m.server.ID, spec: spec, address: m.address})
	}
	raw, err := json.Marshal(parts)
	if err != nil {
		return clusterDerivation{}, err
	}
	sum := sha256.Sum256(raw)
	block.Generation = hex.EncodeToString(sum[:8])
	return out, nil
}

// memberAgentSpec builds m's rpc_server spec with the agent spec id id
// (MemberAgentSpecID): the manual spec's id, or for an automatic member one
// derived from the head, the server and its number there, so a server
// serving two heads, or one head twice, runs distinct members. The demand per
// GPU and for RAM is max(stored usage, estimate), the stored usage held at
// the memory ceiling while it is stale (memberDemand). A member on the head's
// server (loopback) runs direct and knows its head as loopback, whatever the
// cluster's transport: no relay is needed where nothing leaves the host.
func (s *Service) memberAgentSpec(ctx context.Context, headSpecID string, m derivedMember, id, transport string, headIPs []string, headDomain string) (AgentRuntimeSpecDTO, error) {
	var gpuCeiling map[routing.GPUKey]int
	ramCeiling := 0
	if m.row.Stale {
		var err error
		if gpuCeiling, ramCeiling, err = s.serverMemoryCeilings(ctx, m.server.ID, m.server.GPULegacyVendor); err != nil {
			return AgentRuntimeSpecDTO{}, err
		}
	}
	parts := m.gpuParts()
	gpus := make([]AgentRuntimeSpecGPUDTO, 0, len(parts))
	for _, g := range parts {
		gpus = append(gpus, AgentRuntimeSpecGPUDTO{
			Vendor: g.Vendor, Index: g.Index,
			VRAMMB: memberDemand(g.VRAMMeasuredMB, g.VRAMEstimateMB, m.row.Stale, gpuCeiling[resolveLegacyGPUKey(routing.GPUKey{Vendor: g.Vendor, Index: g.Index}, m.server.GPULegacyVendor)]),
		})
	}
	spec := AgentRuntimeSpecDTO{
		ID:                    id,
		Kind:                  AgentSpecKindRPCServer,
		Binary:                m.binary.Path,
		Args:                  []string{},
		Env:                   map[string]string{},
		GPUs:                  gpus,
		RAMMB:                 memberDemand(m.row.RAMMeasuredMB, m.ramEstimate(), m.row.Stale, ramCeiling),
		StartupTimeoutSeconds: memberStartupTimeoutSeconds,
		IdleTimeoutSeconds:    memberIdleTimeoutSeconds,
		// A member runs no model: the resolved type is the generic one and
		// its role "other", so nothing type-specific probes it.
		Type:          string(routing.RuntimeSpecTypeCustom),
		Role:          string(routing.RoleOther),
		ConfigFiles:   []ConfigFileDTO{},
		DeviceBackend: normalizeDeviceBackend(m.row.DeviceBackend),
		RPC: &AgentRPCMemberDTO{
			HeadSpecID: headSpecID, Transport: transport, BindIP: m.bindIP, Port: m.row.Port,
			HeadIPs: headIPs, HeadDomain: headDomain, CacheEnabled: m.row.CacheEnabled,
		},
	}
	if m.loopback {
		spec.RPC.Transport, spec.RPC.HeadIPs, spec.RPC.HeadDomain = rpcTransportDirect, []string{loopbackAddr}, ""
	}
	if m.row.MemberSpecID == "" {
		if m.row.Threads > 0 {
			spec.Args = []string{"-t", strconv.Itoa(m.row.Threads)}
		}
		return spec, nil
	}
	spec.DeviceBackend = normalizeDeviceBackend(m.spec.DeviceBackend)
	spec.RPC.CacheEnabled = m.spec.CacheEnabled
	if strings.TrimSpace(m.spec.Args) != "" {
		if err := json.Unmarshal([]byte(m.spec.Args), &spec.Args); err != nil {
			return AgentRuntimeSpecDTO{}, ErrRuntimeSpecArgsInvalid
		}
	}
	if strings.TrimSpace(m.spec.Env) != "" {
		if err := json.Unmarshal([]byte(m.spec.Env), &spec.Env); err != nil {
			return AgentRuntimeSpecDTO{}, ErrRuntimeSpecEnvInvalid
		}
	}
	if spec.Args == nil {
		spec.Args = []string{}
	}
	if spec.Env == nil {
		spec.Env = map[string]string{}
	}
	return spec, nil
}

// memberDemand is a member's demand for one GPU or for RAM: max(stored,
// estimate), 0 unknown. A stale stored value -- kept from before a change of
// the distribution as a floor until the next formed run -- counts only up to
// ceiling (0: none known), so a new split that needs less can start and
// replace it.
func memberDemand(stored, estimate int, stale bool, ceiling int) int {
	if stale && ceiling > 0 {
		stored = min(stored, ceiling)
	}
	return max(stored, estimate)
}

// serverMemoryCeilings reads what a stale member value is held at on
// serverID: per GPU the operator's budget, else the card's memory from the
// hardware report; for RAM the host's memory. A figure that is not known is
// absent (0). GPUs are keyed by (vendor, index) with an empty vendor read as
// the server's legacy vendor legacy (resolveLegacyGPUKey), on the report's
// and the budgets' side alike, so a caller looks a member GPU up by its
// resolved key and gets its own card's ceiling, never another vendor's at
// that index.
func (s *Service) serverMemoryCeilings(ctx context.Context, serverID, legacy string) (map[routing.GPUKey]int, int, error) {
	gpus := map[routing.GPUKey]int{}
	hosts, err := s.serverHostGPUs(ctx, serverID)
	if err != nil {
		return nil, 0, err
	}
	for key, g := range hosts {
		if g.totalMB > 0 {
			gpus[resolveLegacyGPUKey(key, legacy)] = g.totalMB
		}
	}
	budgets, err := s.routes.ServerGPUBudgets(ctx, serverID)
	if err != nil {
		return nil, 0, err
	}
	for _, b := range budgets {
		if b.BudgetMB > 0 {
			gpus[resolveLegacyGPUKey(routing.GPUKey{Vendor: b.GPUVendor, Index: b.GPUIndex}, legacy)] = b.BudgetMB
		}
	}
	ram := 0
	hw, ok, err := s.routes.ServerHardwareByServer(ctx, serverID)
	if err != nil {
		return nil, 0, err
	}
	if ok {
		var report struct {
			Memory struct {
				TotalBytes int64 `json:"total_bytes"`
			} `json:"memory"`
		}
		if json.Unmarshal([]byte(hw.ReportJSON), &report) == nil {
			ram = int(report.Memory.TotalBytes / (1024 * 1024))
		}
	}
	return gpus, ram, nil
}

// memberSpecsForServer derives the rpc_server specs serverID runs for the
// cluster heads it is a member of, in head spec id order and, within a head,
// in member order (a head may have several members on serverID). A head that
// is disabled or withheld contributes nothing; hold reports that serverID's
// own agent declaration is unknown while it is a member of an enabled head,
// which holds serverID's document.
func (s *Service) memberSpecsForServer(ctx context.Context, serverID string) (specs []AgentRuntimeSpecDTO, hold bool, err error) {
	memberships, err := s.routes.ClusterMembershipsByServer(ctx, serverID)
	if err != nil {
		return nil, false, err
	}
	specs = []AgentRuntimeSpecDTO{}
	var heads []string
	for _, row := range memberships {
		heads = appendUnique(heads, row.HeadSpecID)
	}
	for _, headSpecID := range heads {
		d, ok, err := s.deriveHeadOf(ctx, headSpecID, serverID)
		if err != nil {
			return nil, false, err
		}
		if !ok {
			continue
		}
		hold = hold || d.hold
		if d.reason != "" {
			continue
		}
		for _, m := range d.members {
			if m.serverID == serverID {
				specs = append(specs, m.spec)
			}
		}
	}
	return specs, hold, nil
}

// deriveHeadOf derives the cluster of the head headSpecID for viewerID's
// document; ok is false for a head that is gone, disabled, no longer a
// cluster head, or whose server cannot be resolved.
func (s *Service) deriveHeadOf(ctx context.Context, headSpecID, viewerID string) (clusterDerivation, bool, error) {
	head, ok, err := s.routes.RuntimeSpecByID(ctx, headSpecID)
	if err != nil || !ok || !head.Enabled || !head.ClusterEnabled {
		return clusterDerivation{}, false, err
	}
	headServer, ok, err := s.specServer(ctx, head)
	if err != nil || !ok {
		return clusterDerivation{}, false, err
	}
	d, err := s.deriveCluster(ctx, head, headServer, viewerID)
	return d, err == nil, err
}

// specServer resolves spec -> mapping -> application -> server; ok is false
// when a link is gone or the application is no server_agent one (its specs
// run nowhere). Any other store error is returned.
func (s *Service) specServer(ctx context.Context, spec routing.RuntimeSpec) (routing.AIServer, bool, error) {
	mapping, err := s.routes.MappingByID(ctx, spec.MappingID)
	if err != nil {
		return routing.AIServer{}, false, ignoreNotFound(err)
	}
	app, err := s.routes.ApplicationByID(ctx, mapping.ApplicationID)
	if err != nil {
		return routing.AIServer{}, false, ignoreNotFound(err)
	}
	if app.Type != routing.ProviderServerAgent {
		return routing.AIServer{}, false, nil
	}
	server, err := s.routes.AIServerByID(ctx, app.ServerID)
	if err != nil {
		return routing.AIServer{}, false, ignoreNotFound(err)
	}
	return server, true, nil
}

func ignoreNotFound(err error) error {
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	return err
}

// clusterWithheldReason is the read view's echo of why an enabled cluster
// head is not pushed; "" when it is, or when the spec is no enabled head.
func (s *Service) clusterWithheldReason(ctx context.Context, headSpecID string, headServer routing.AIServer) (string, error) {
	head, ok, err := s.routes.RuntimeSpecByID(ctx, headSpecID)
	if err != nil || !ok || !head.Enabled || !head.ClusterEnabled {
		return "", err
	}
	d, err := s.deriveCluster(ctx, head, headServer, headServer.ID)
	return d.reason, err
}

// resyncManualMemberGPUs follows a manual member spec's GPU rows onto the
// membership of the head that uses it: the membership GPU rows hold the
// measured usage per GPU, so they list exactly the spec's GPUs in the spec's
// order, a kept GPU keeps its measured values and a removed one drops them.
// A changed device order or set changes the distribution, so kept values
// become a stale floor and every member's run of that head ends, as on a
// head edit (planClusterRows).
//
// The rewrite applies only over the rows it read (SetClusterMembersIf), so a
// measured value the gateway wrote in between is read again, never
// overwritten.
func (s *Service) resyncManualMemberGPUs(ctx context.Context, specID string, rows []routing.RPCMemberSpecGPU) (headSpecID string, err error) {
	used, ok, err := s.routes.ClusterMemberBySpec(ctx, specID)
	if err != nil || !ok {
		return "", err
	}
	legacy, err := s.serverLegacyGPUVendor(ctx, used.ServerID)
	if err != nil {
		return "", err
	}
	for range clusterRewriteAttempts {
		read, readGPUs, err := s.routes.ClusterMembers(ctx, used.HeadSpecID)
		if err != nil {
			return "", err
		}
		// The member as read now: its position is what its GPU rows hang on.
		i := slices.IndexFunc(read, func(m routing.ClusterMember) bool { return m.MemberSpecID == specID })
		if i < 0 {
			return used.HeadSpecID, nil // no longer a member of the head
		}
		members, gpus := resyncedManualMember(read[i], rows, read, readGPUs, legacy)
		applied, err := s.routes.SetClusterMembersIf(ctx, used.HeadSpecID, read, readGPUs, members, gpus)
		if err != nil || applied {
			return used.HeadSpecID, err
		}
	}
	return used.HeadSpecID, errClusterMembersChanged
}

// resyncedManualMember is resyncManualMemberGPUs' rewrite of the membership
// rows read: used's GPU rows (by its position, so another member of the head
// on the same server keeps its own) follow the spec's rows, matched by GPU
// key. An empty vendor and legacy -- the member server's legacy vendor --
// name one card (lookupGPUKey, sameGPUSelection), so a backfill that wrote
// the vendor onto one side only keeps the measured values and ends no run.
func resyncedManualMember(used routing.ClusterMember, rows []routing.RPCMemberSpecGPU, read []routing.ClusterMember, gpus []routing.ClusterMemberGPU, legacy string) ([]routing.ClusterMember, []routing.ClusterMemberGPU) {
	members := slices.Clone(read)
	prev := map[routing.GPUKey]routing.ClusterMemberGPU{}
	var prevOrder []routing.GPUKey
	next := make([]routing.ClusterMemberGPU, 0, len(gpus)+len(rows))
	for _, g := range gpus {
		if g.MemberPosition == used.Position {
			key := routing.GPUKey{Vendor: g.GPUVendor, Index: g.GPUIndex}
			prev[key] = g
			prevOrder = append(prevOrder, key)
			continue
		}
		next = append(next, g)
	}
	nextOrder := make([]routing.GPUKey, 0, len(rows))
	measured := false
	for pos, r := range rows {
		g := routing.ClusterMemberGPU{
			HeadSpecID: used.HeadSpecID, MemberPosition: used.Position, ServerID: used.ServerID,
			GPUVendor: r.GPUVendor, GPUIndex: r.GPUIndex, Position: pos,
		}
		key := routing.GPUKey{Vendor: r.GPUVendor, Index: r.GPUIndex}
		if p, ok := lookupGPUKey(prev, key, legacy); ok {
			g.VRAMMeasuredMB, g.RunPeakVRAMMB = p.VRAMMeasuredMB, p.RunPeakVRAMMB
			measured = measured || p.VRAMMeasuredMB > 0
		}
		nextOrder = append(nextOrder, key)
		next = append(next, g)
	}
	if !sameGPUSelection(prevOrder, nextOrder, legacy) {
		endClusterRuns(members, next, used.Position, measured)
	}
	return members, next
}

// endClusterRuns is a change of the distribution on a head's membership:
// every member's run ends with its peaks, and the kept values of the member
// at position (measured: a kept GPU value is known) become a stale floor.
func endClusterRuns(members []routing.ClusterMember, gpus []routing.ClusterMemberGPU, position int, measured bool) {
	for i := range gpus {
		gpus[i].RunPeakVRAMMB = 0
	}
	for i := range members {
		members[i].RunPeakRAMMB, members[i].RunHeadInstance, members[i].RunStartedAt = 0, 0, time.Time{}
		if members[i].Position == position && (measured || members[i].RAMMeasuredMB > 0) {
			members[i].Stale = true
		}
	}
}

// clusterServers is headSpecID's head server and every member server, each
// once; a link that cannot be read is skipped. A write that touches a cluster
// notifies all of them: each one's document carries a part of the cluster,
// and whether any part is pushed depends on every server of it.
func (s *Service) clusterServers(ctx context.Context, headSpecID string) []string {
	var ids []string
	if id, ok := s.headServerID(ctx, headSpecID); ok {
		ids = append(ids, id)
	}
	members, _, err := s.routes.ClusterMembers(ctx, headSpecID)
	if err != nil {
		slog.Debug("cluster notification: membership read failed", "head_spec_id", headSpecID, "err", err)
	}
	for _, m := range members {
		ids = appendUnique(ids, m.ServerID)
	}
	return ids
}

// clusterServersOfServer lists every server that shares a cluster with
// serverID (as a head or a member), serverID included when it is in one.
func (s *Service) clusterServersOfServer(ctx context.Context, serverID string) []string {
	var ids []string
	for _, head := range s.clusterHeadsOfServer(ctx, serverID) {
		for _, id := range s.clusterServers(ctx, head) {
			ids = appendUnique(ids, id)
		}
	}
	return ids
}

// clusterHeadsOfServer lists the cluster heads serverID takes part in: its
// own heads and the heads it is a member of. Best-effort: a failed read
// leaves those out.
func (s *Service) clusterHeadsOfServer(ctx context.Context, serverID string) []string {
	var heads []string
	memberships, err := s.routes.ClusterMembershipsByServer(ctx, serverID)
	if err != nil {
		slog.Debug("cluster notification: memberships read failed", "server_id", serverID, "err", err)
	}
	for _, m := range memberships {
		heads = appendUnique(heads, m.HeadSpecID)
	}
	apps, err := s.routes.ApplicationsByServer(ctx, serverID)
	if err != nil {
		slog.Debug("cluster notification: applications read failed", "server_id", serverID, "err", err)
	}
	for _, app := range apps {
		if app.Type != routing.ProviderServerAgent {
			continue
		}
		specs, err := s.routes.RuntimeSpecsByApplication(ctx, app.ID)
		if err != nil {
			slog.Debug("cluster notification: specs read failed", "server_id", serverID, "err", err)
		}
		for _, spec := range specs {
			if spec.ClusterEnabled {
				heads = appendUnique(heads, spec.ID)
			}
		}
	}
	return heads
}

// NotifyClustersOfServer notifies every server that shares a cluster with
// serverID, serverID included: a write to serverID's row, its admin groups,
// its NetBird state or its binary inventory, or a change of what its agent
// declares, can change whether and how each of those clusters is pushed
// (addresses, bind addresses, consent, member binaries, the feature gate).
// Best-effort, like every notification.
func (s *Service) NotifyClustersOfServer(ctx context.Context, serverID string) {
	if s.runtimeChanged == nil || s.routes == nil || serverID == "" {
		return
	}
	for _, id := range s.clusterServersOfServer(ctx, serverID) {
		s.notifyRuntimeChanged(id)
	}
}

// notifyAllClusterServers notifies every server of every cluster: the
// NetBird policy settings decide whether any direct mesh cluster may be
// pushed. Each head's servers are read once, and a fleet without a cluster
// notifies nobody. Best-effort.
func (s *Service) notifyAllClusterServers(ctx context.Context) {
	if s.runtimeChanged == nil || s.routes == nil {
		return
	}
	servers, err := s.routes.AIServers(ctx)
	if err != nil {
		slog.Debug("cluster notification: server list failed", "err", err)
		return
	}
	var heads []string
	for _, srv := range servers {
		for _, head := range s.clusterHeadsOfServer(ctx, srv.ID) {
			heads = appendUnique(heads, head)
		}
	}
	var ids []string
	for _, head := range heads {
		for _, id := range s.clusterServers(ctx, head) {
			ids = appendUnique(ids, id)
		}
	}
	for _, id := range ids {
		s.notifyRuntimeChanged(id)
	}
}
