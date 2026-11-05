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
)

// AgentFeatureRPCCluster is the feature an agent declares when it runs
// llama.cpp RPC cluster heads and rpc_server members. A cluster head and its
// members reach a runtime document only when every agent involved declares
// it: an older agent would run the head without its endpoints, or the member
// as a model.
const AgentFeatureRPCCluster = "rpc_cluster"

// AgentSpecKindRPCServer is the runtime-document kind of a cluster member
// spec; a model spec has no kind.
const AgentSpecKindRPCServer = "rpc_server"

// Reasons a cluster head is withheld from its server's runtime document
// (RuntimeSpecDTO.ClusterWithheldReason). The head and every member spec
// derived from it are withheld together, so an agent never receives a
// partial endpoint list. ClusterWithheldFeaturesPending withholds the
// cluster while the declaration of an agent of it is unknown; a server whose
// OWN agent's declaration is unknown has its whole document held instead
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
// network. Generation is a digest of the member configuration that the
// endpoints do not show (binaries and their versions, args, device backends,
// GPU selections, bind addresses), so a member-side change relaunches the
// head.
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
// the head's cluster block and one rpc_server spec per member server, or the
// reason none of it is pushed.
type clusterDerivation struct {
	reason  string
	head    *AgentClusterHeadDTO
	members map[string]AgentRuntimeSpecDTO
	// hold: the viewing server's own agent declaration is unknown, so its
	// document is held rather than served without the cluster.
	hold bool
}

func withheld(reason string) clusterDerivation { return clusterDerivation{reason: reason} }

// derivedMember is one member with what its spec is built from.
type derivedMember struct {
	clusterMemberState
	binary  routing.ServerBinary
	address string
	bindIP  string
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
	if reason, hold := s.clusterFeatureReason(viewerID, serverIDs); reason != "" {
		d := withheld(reason)
		d.hold = hold
		return d, nil
	}
	headGroups, err := s.routes.ServerAdminGroups(ctx, headServer.ID)
	if err != nil {
		return clusterDerivation{}, err
	}
	members := make([]derivedMember, len(states))
	for i, st := range states {
		members[i].clusterMemberState = st
		reason, err := s.checkDerivedMember(ctx, i, st, headGroups)
		if err != nil || reason != "" {
			return withheld(reason), err
		}
	}
	if reason, err := s.resolveMemberBinaries(ctx, head, headServer, members); err != nil || reason != "" {
		return withheld(reason), err
	}
	network, transport := head.ClusterNetwork, head.RPCTransport
	if network == "" {
		servers := []routing.AIServer{headServer}
		for _, m := range members {
			servers = append(servers, m.server)
		}
		network = defaultClusterNetwork(servers)
	}
	if transport == "" {
		transport = s.defaultRPCTransport(ctx, network)
	}
	headIPs, headDomain, reason := clusterHeadIdentity(headServer, network)
	if reason != "" {
		return withheld(reason), nil
	}
	for i := range members {
		if members[i].address, reason = clusterAddress(members[i].server, network); reason != "" {
			return withheld(reason), nil
		}
		if members[i].bindIP, reason = memberBindIP(members[i].server, network); reason != "" {
			return withheld(reason), nil
		}
	}
	if transport == rpcTransportDirect && network == clusterNetworkMesh && !s.directMeshAvailable(ctx) {
		return withheld(ClusterWithheldDirectUnavailable), nil
	}
	if transport == rpcTransportDirect && network == clusterNetworkLAN && !head.LANExposureAck {
		return withheld(ClusterWithheldLANAckRequired), nil
	}
	return s.buildCluster(ctx, head, members, network, transport, headIPs, headDomain)
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
// viewer for as long as that other host stays away.
func (s *Service) clusterFeatureReason(viewerID string, serverIDs []string) (reason string, hold bool) {
	if s.agentFeatures == nil {
		return ClusterWithheldAgentTooOld, false
	}
	if !s.agentFeatures.Known(viewerID) {
		return ClusterWithheldFeaturesPending, true
	}
	pending, old := false, false
	for _, id := range serverIDs {
		switch {
		case !s.agentFeatures.Known(id):
			pending = true
		case !s.agentFeatures.Has(id, AgentFeatureRPCCluster):
			old = true
		}
	}
	if pending {
		return ClusterWithheldFeaturesPending, false
	}
	if old {
		return ClusterWithheldAgentTooOld, false
	}
	return "", false
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
// its server's ggml-rpc-server whose version matches the head binary's
// (MatchingRPCBinary, read now, so an inventory change moves it), a manual
// member its spec's binary with whatever version the inventory knows.
func (s *Service) resolveMemberBinaries(ctx context.Context, head routing.RuntimeSpec, headServer routing.AIServer, members []derivedMember) (string, error) {
	var headRow routing.ServerBinary
	headKnown, headRead := false, false
	for i := range members {
		m := &members[i]
		if m.row.MemberSpecID != "" {
			b, ok, err := s.BinaryVersionFor(ctx, m.server.ID, m.spec.Binary)
			if err != nil {
				return "", err
			}
			if !ok {
				b = routing.ServerBinary{ServerID: m.server.ID, Path: m.spec.Binary}
			}
			m.binary = b
			continue
		}
		if !headRead {
			row, listed, err := s.BinaryVersionFor(ctx, headServer.ID, head.Binary)
			if err != nil {
				return "", err
			}
			headRow, headKnown, headRead = row, listed && binaryVersionKnown(row), true
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
	}
	return "", nil
}

// clusterAddress is the address a head dials srv's member at: in mesh the
// mesh IPv4 (NetBird must be on with a tracking group), in lan the server's
// lan_address, else its Domain -- except on a NetBird-enabled server, whose
// Domain the peer sync rewrites to the NetBird name.
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
	if srv.LANAddress != "" {
		return srv.LANAddress, ""
	}
	domain := strings.TrimSpace(srv.Domain)
	if srv.NetbirdEnabled || domain == "" {
		return "", ClusterWithheldAddressUnknown
	}
	return domain, ""
}

// memberBindIP is the address srv's member (or its relay) listens on: the
// mesh IPv4 exactly in mesh; in lan every interface, or only lan_address
// when the server asks to listen there alone (and then that address must be
// known, never silently widened to every interface).
func memberBindIP(srv routing.AIServer, network string) (ip, reason string) {
	if network == clusterNetworkMesh {
		return srv.NetbirdIP, ""
	}
	if srv.LANBindOnly {
		if srv.LANAddress == "" {
			return "", ClusterWithheldAddressUnknown
		}
		return srv.LANAddress, ""
	}
	return "0.0.0.0", ""
}

// clusterHeadIdentity is how a member recognises its head: the head's mesh
// IPv4 in mesh; in lan its lan_address, else its Domain for the member agent
// to resolve.
func clusterHeadIdentity(headServer routing.AIServer, network string) (ips []string, domain, reason string) {
	addr, reason := clusterAddress(headServer, network)
	if reason != "" {
		return nil, "", reason
	}
	if network == clusterNetworkLAN && headServer.LANAddress == "" {
		return []string{}, addr, ""
	}
	return []string{addr}, "", ""
}

// memberGenerationPart is one member's share of the head's generation:
// everything of the member's launch that decides what devices the head sees
// and whether its build talks to the head's.
type memberGenerationPart struct {
	Kind          string   `json:"kind"`
	ID            string   `json:"id"`
	Server        string   `json:"server"`
	Binary        string   `json:"binary"`
	Args          []string `json:"args"`
	DeviceBackend string   `json:"device_backend"`
	Version       string   `json:"version"`
	Commit        string   `json:"commit"`
	Build         string   `json:"build"`
	DeviceCount   int      `json:"device_count"`
	GPUs          []int    `json:"gpus"`
	Transport     string   `json:"transport"`
	Network       string   `json:"network"`
	Bind          string   `json:"bind"`
	Port          int      `json:"port"`
}

// buildCluster assembles a derivable cluster: the head's block and each
// member's rpc_server spec.
func (s *Service) buildCluster(ctx context.Context, head routing.RuntimeSpec, members []derivedMember, network, transport string, headIPs []string, headDomain string) (clusterDerivation, error) {
	block := &AgentClusterHeadDTO{Endpoints: make([]AgentClusterEndpointDTO, 0, len(members)), Transport: transport, Network: network}
	parts := make([]memberGenerationPart, 0, len(members))
	out := clusterDerivation{head: block, members: make(map[string]AgentRuntimeSpecDTO, len(members))}
	for _, m := range members {
		spec, err := s.memberAgentSpec(ctx, head.ID, m, transport, headIPs, headDomain)
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
			Kind: kind, ID: spec.ID, Server: m.server.ID, Binary: m.binary.Path, Args: spec.Args, DeviceBackend: spec.DeviceBackend,
			Version: m.binary.EffectiveVersion(),
			Commit:  commit, Build: build, DeviceCount: devices, GPUs: indices, Transport: transport, Network: network,
			Bind: m.bindIP, Port: m.row.Port,
		})
		out.members[m.server.ID] = spec
	}
	raw, err := json.Marshal(parts)
	if err != nil {
		return clusterDerivation{}, err
	}
	sum := sha256.Sum256(raw)
	block.Generation = hex.EncodeToString(sum[:8])
	return out, nil
}

// memberAgentSpec builds m's rpc_server spec. Its id is the manual spec's id,
// or for an automatic member one derived from the head and the server, so a
// server serving two heads runs two distinct members. The demand per GPU and
// for RAM is max(stored usage, estimate), the stored usage held at the
// memory ceiling while it is stale (memberDemand).
func (s *Service) memberAgentSpec(ctx context.Context, headSpecID string, m derivedMember, transport string, headIPs []string, headDomain string) (AgentRuntimeSpecDTO, error) {
	var gpuCeiling map[int]int
	ramCeiling := 0
	if m.row.Stale {
		var err error
		if gpuCeiling, ramCeiling, err = s.serverMemoryCeilings(ctx, m.server.ID); err != nil {
			return AgentRuntimeSpecDTO{}, err
		}
	}
	parts := m.gpuParts()
	gpus := make([]AgentRuntimeSpecGPUDTO, 0, len(parts))
	for _, g := range parts {
		gpus = append(gpus, AgentRuntimeSpecGPUDTO{
			Index: g.Index, VRAMMB: memberDemand(g.VRAMMeasuredMB, g.VRAMEstimateMB, m.row.Stale, gpuCeiling[g.Index]),
		})
	}
	spec := AgentRuntimeSpecDTO{
		ID:                    "rpca_" + headSpecID + "_" + m.server.ID,
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
	if m.row.MemberSpecID == "" {
		if m.row.Threads > 0 {
			spec.Args = []string{"-t", strconv.Itoa(m.row.Threads)}
		}
		return spec, nil
	}
	spec.ID = m.spec.ID
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
// absent (0).
func (s *Service) serverMemoryCeilings(ctx context.Context, serverID string) (map[int]int, int, error) {
	gpus := map[int]int{}
	hosts, err := s.serverHostGPUs(ctx, serverID)
	if err != nil {
		return nil, 0, err
	}
	for idx, g := range hosts {
		if g.totalMB > 0 {
			gpus[idx] = g.totalMB
		}
	}
	budgets, err := s.routes.ServerGPUBudgets(ctx, serverID)
	if err != nil {
		return nil, 0, err
	}
	for _, b := range budgets {
		if b.BudgetMB > 0 {
			gpus[b.GPUIndex] = b.BudgetMB
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
// cluster heads it is a member of, in head spec id order. A head that is
// disabled or withheld contributes nothing; hold reports that serverID's own
// agent declaration is unknown while it is a member of an enabled head,
// which holds serverID's document.
func (s *Service) memberSpecsForServer(ctx context.Context, serverID string) (specs []AgentRuntimeSpecDTO, hold bool, err error) {
	memberships, err := s.routes.ClusterMembershipsByServer(ctx, serverID)
	if err != nil {
		return nil, false, err
	}
	specs = []AgentRuntimeSpecDTO{}
	for _, row := range memberships {
		head, ok, err := s.routes.RuntimeSpecByID(ctx, row.HeadSpecID)
		if err != nil {
			return nil, false, err
		}
		if !ok || !head.Enabled || !head.ClusterEnabled {
			continue
		}
		headServer, ok, err := s.specServer(ctx, head)
		if err != nil {
			return nil, false, err
		}
		if !ok {
			continue
		}
		d, err := s.deriveCluster(ctx, head, headServer, serverID)
		if err != nil {
			return nil, false, err
		}
		hold = hold || d.hold
		if spec, ok := d.members[serverID]; ok && d.reason == "" {
			specs = append(specs, spec)
		}
	}
	return specs, hold, nil
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
// become a stale floor, as on a head edit.
func (s *Service) resyncManualMemberGPUs(ctx context.Context, specID string, rows []routing.RPCMemberSpecGPU) (headSpecID string, err error) {
	used, ok, err := s.routes.ClusterMemberBySpec(ctx, specID)
	if err != nil || !ok {
		return "", err
	}
	members, gpus, err := s.routes.ClusterMembers(ctx, used.HeadSpecID)
	if err != nil {
		return "", err
	}
	prev := map[int]routing.ClusterMemberGPU{}
	var prevOrder, nextOrder []int
	next := make([]routing.ClusterMemberGPU, 0, len(gpus)+len(rows))
	for _, g := range gpus {
		if g.ServerID == used.ServerID {
			prev[g.GPUIndex] = g
			prevOrder = append(prevOrder, g.GPUIndex)
			continue
		}
		next = append(next, g)
	}
	measured := false
	for pos, r := range rows {
		g := routing.ClusterMemberGPU{HeadSpecID: used.HeadSpecID, ServerID: used.ServerID, GPUIndex: r.GPUIndex, Position: pos}
		if p, ok := prev[r.GPUIndex]; ok {
			g.VRAMMeasuredMB, g.RunPeakVRAMMB = p.VRAMMeasuredMB, p.RunPeakVRAMMB
			measured = measured || p.VRAMMeasuredMB > 0
		}
		nextOrder = append(nextOrder, r.GPUIndex)
		next = append(next, g)
	}
	for i := range members {
		if members[i].ServerID == used.ServerID && !slices.Equal(prevOrder, nextOrder) && (measured || members[i].RAMMeasuredMB > 0) {
			members[i].Stale = true
		}
	}
	return used.HeadSpecID, s.routes.SetClusterMembers(ctx, used.HeadSpecID, members, next)
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
// pushed. Best-effort.
func (s *Service) notifyAllClusterServers(ctx context.Context) {
	if s.runtimeChanged == nil || s.routes == nil {
		return
	}
	servers, err := s.routes.AIServers(ctx)
	if err != nil {
		slog.Debug("cluster notification: server list failed", "err", err)
		return
	}
	var ids []string
	for _, srv := range servers {
		for _, id := range s.clusterServersOfServer(ctx, srv.ID) {
			ids = appendUnique(ids, id)
		}
	}
	for _, id := range ids {
		s.notifyRuntimeChanged(id)
	}
}
