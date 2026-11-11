// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/store"
	"slices"
	"strings"
	"time"
)

// Cluster head save refusals. Every one is 400 except the two noted.
var (
	ErrClusterMemberModeInvalid            = errors.New("runtime_spec.cluster_member_mode_invalid")
	ErrClusterNetworkInvalid               = errors.New("runtime_spec.cluster_network_invalid")
	ErrClusterTransportInvalid             = errors.New("runtime_spec.cluster_transport_invalid")
	ErrClusterTypeInvalid                  = errors.New("runtime_spec.cluster_type_invalid")
	ErrClusterFlagEqualsForm               = errors.New("runtime_spec.cluster_flag_equals_form")
	ErrClusterRPCPlaceholderMissing        = errors.New("runtime_spec.cluster_rpc_placeholder_missing")
	ErrClusterRPCTwice                     = errors.New("runtime_spec.cluster_rpc_twice")
	ErrClusterRPCPlaceholderWithoutCluster = errors.New("runtime_spec.cluster_rpc_placeholder_without_cluster")
	ErrClusterDeviceListMissingRPC         = errors.New("runtime_spec.cluster_device_list_missing_rpc")
	ErrClusterDeviceOrder                  = errors.New("runtime_spec.cluster_device_order")
	ErrClusterMembersRequired              = errors.New("runtime_spec.cluster_members_required")
	ErrClusterHeadReplicable               = errors.New("runtime_spec.cluster_head_replicable")
	ErrClusterStartupTimeoutRequired       = errors.New("runtime_spec.cluster_startup_timeout_required")
	ErrClusterMemberServerTwice            = errors.New("runtime_spec.cluster_member_server_twice")
	ErrClusterMemberOnHeadServer           = errors.New("runtime_spec.cluster_member_on_head_server")
	// ErrClusterMemberSpecTaken is 409: the manual member spec serves another
	// head already.
	ErrClusterMemberSpecTaken        = errors.New("runtime_spec.cluster_member_spec_taken")
	ErrClusterMemberSpecDisabled     = errors.New("runtime_spec.cluster_member_spec_disabled")
	ErrClusterMemberRPCDisabled      = errors.New("runtime_spec.cluster_member_rpc_disabled")
	ErrClusterMemberNoMatchingBinary = errors.New("runtime_spec.cluster_member_no_matching_binary")
	ErrClusterVersionMismatch        = errors.New("runtime_spec.cluster_version_mismatch")
	ErrClusterMeshUnavailable        = errors.New("runtime_spec.cluster_mesh_unavailable")
	ErrClusterLANAddressRequired     = errors.New("runtime_spec.cluster_lan_address_required")
	ErrClusterDirectUnavailable      = errors.New("runtime_spec.cluster_direct_unavailable")
	ErrClusterLANAckRequired         = errors.New("runtime.cluster_lan_ack_required")
	ErrClusterMemberBudgetRequired   = errors.New("runtime_spec.cluster_member_budget_required")
	ErrClusterTensorSplitRequired    = errors.New("runtime_spec.cluster_tensor_split_required")
	// ErrClusterPortExhausted is 409: the member server has no free port left
	// in the RPC range.
	ErrClusterPortExhausted = errors.New("runtime_spec.cluster_port_exhausted")
	// ErrRuntimeSpecPortConflictsRPCMember refuses a fixed listen_port that a
	// cluster member on the same server already uses as its RPC port.
	ErrRuntimeSpecPortConflictsRPCMember = errors.New("runtime_spec.port_conflicts_rpc_member")
	// ErrClusterHeadBinaryVersionUnknown refuses a head with automatic
	// members whose binary has no known version: the members' matching
	// ggml-rpc-server cannot be chosen without it. The newest save code, so
	// the last refusal of the write (writeRuntimeSpecAs).
	ErrClusterHeadBinaryVersionUnknown = errors.New("runtime_spec.cluster_head_binary_version_unknown")
)

// Advisory cluster warning codes (RuntimeSpecDTO.ClusterWarnings). They never
// refuse a save.
const (
	ClusterWarnRPCDevicesNotFirst          = "cluster_rpc_devices_not_first"
	ClusterWarnMMDevMissing                = "cluster_mmdev_missing"
	ClusterWarnCPUMemberWithoutTensorSplit = "cluster_cpu_member_without_tensor_split"
	ClusterWarnSharedHostWithoutEstimate   = "cluster_member_shared_host_without_estimate"
	ClusterWarnBudgetBelowDevice           = "cluster_member_budget_below_device"
	ClusterWarnGPUOverlap                  = "cluster_member_gpu_overlap"
	ClusterWarnRelayMeshDenyOff            = "cluster_relay_mesh_deny_by_default_off"
	ClusterWarnMemberVersionUnknown        = "cluster_member_version_unknown"
)

// The cluster enum values.
const (
	memberModeAutomatic = "automatic"
	memberModeManual    = "manual"
	clusterNetworkMesh  = "mesh"
	clusterNetworkLAN   = "lan"
	rpcTransportDirect  = "direct"
	rpcTransportRelay   = "relay"
	deviceBackendAuto   = "auto"
)

// RPC port range a member server's member processes are allocated from. It
// lies below every default ephemeral port range (Linux 32768-60999, Windows
// and macOS 49152-65535): the agent's model children listen on ports the
// kernel picks, and one of those must never hold a member's port, which a
// member binding every interface would then fail to take.
const (
	rpcPortFirst = 26000
	rpcPortLast  = 26999
)

// netbirdDefaultPolicyStateKey is the system setting holding what the last
// NetBird policy pass saw of NetBird's built-in "Default" policy, as JSON
// {"state": "enabled"|"disabled"|"unknown", "at": RFC 3339}. A direct mesh
// cluster relies on that catch-all policy being off, and only the policy pass
// talks to NetBird, so the save check reads the pass's last observation.
const netbirdDefaultPolicyStateKey = "netbird_default_policy_state"

// The netbird_default_policy_state states.
const (
	NetbirdDefaultPolicyEnabled  = "enabled"
	NetbirdDefaultPolicyDisabled = "disabled"
	NetbirdDefaultPolicyUnknown  = "unknown"
)

// ClusterMemberGPUDTO is one GPU of a cluster member: its host index, the
// operator's VRAM estimate and the measured VRAM (read-only).
type ClusterMemberGPUDTO struct {
	Index          int `json:"index"`
	VRAMEstimateMB int `json:"vram_estimate_mb"`
	VRAMMeasuredMB int `json:"vram_measured_mb"`
}

// ClusterMemberDTO is one member of a cluster head, in member order. On a
// write the inputs are ServerID and, by member mode, MemberSpecID (manual) or
// GPUs (ordered; empty = CPU member), Threads, CacheEnabled, DeviceBackend
// and RAMEstimateMB (automatic). Everything else is read-only: ServerName,
// the measured values with MeasuredAt and Stale (a value kept from before a
// change of the distribution), and Port, the member's RPC or relay port.
// Visible is false for a member on a server the caller may not manage: the
// entry then carries only its Position and is counted in the totals.
type ClusterMemberDTO struct {
	Position      int                   `json:"position"`
	ServerID      string                `json:"server_id"`
	ServerName    string                `json:"server_name"`
	MemberSpecID  string                `json:"member_spec_id"`
	GPUs          []ClusterMemberGPUDTO `json:"gpus"`
	Threads       int                   `json:"threads"`
	CacheEnabled  bool                  `json:"cache_enabled"`
	DeviceBackend string                `json:"device_backend"`
	RAMEstimateMB int                   `json:"ram_estimate_mb"`
	RAMMeasuredMB int                   `json:"ram_measured_mb"`
	MeasuredAt    *time.Time            `json:"measured_at,omitempty"`
	Stale         bool                  `json:"stale"`
	Port          int                   `json:"port"`
	Visible       bool                  `json:"visible"`
}

// ClusterCandidateDTO is a server a head may add as a member. In automatic
// mode RPCBinary/RPCBinaryVersion name the server's ggml-rpc-server that
// matches the head's binary; in manual mode MemberSpecs lists the server's
// free member specs.
type ClusterCandidateDTO struct {
	ServerID         string             `json:"server_id"`
	ServerName       string             `json:"server_name"`
	RPCBinary        string             `json:"rpc_binary,omitempty"`
	RPCBinaryVersion string             `json:"rpc_binary_version,omitempty"`
	MemberSpecs      []RPCMemberSpecDTO `json:"member_specs,omitempty"`
	// Compatible: in automatic mode, the server has a ggml-rpc-server of the
	// head binary's version (RPCBinary names it); false when it offers
	// membership but has none, or the head binary's version is unknown. In
	// manual mode, at least one listed member spec is not in
	// IncompatibleMemberSpecs.
	Compatible bool `json:"compatible"`
	// IncompatibleMemberSpecs lists, in manual mode, the member specs whose
	// binary's known version differs from the head binary's: the save
	// refuses them with runtime_spec.cluster_version_mismatch.
	IncompatibleMemberSpecs []string `json:"incompatible_member_specs,omitempty"`
}

// ClusterCandidateList is ClusterCandidates' answer: the candidates and
// whether the head binary's version is known (automatic members are matched
// on it).
type ClusterCandidateList struct {
	Candidates      []ClusterCandidateDTO
	HeadBinaryKnown bool
}

// clusterPlan is the cluster part of one runtime-spec write: the columns of
// the spec row, the membership rows to store when writeMembers is set, and
// every member server (before and after) whose document the write changes.
type clusterPlan struct {
	enabled       bool
	memberMode    string
	network       string
	transport     string
	lanAck        bool
	deviceBackend string
	writeMembers  bool
	members       []routing.ClusterMember
	gpus          []routing.ClusterMemberGPU
	memberServers []string
	// allocated lists the members whose port this write allocated (indices
	// into members); the others keep the port their server's row had.
	allocated []int
	// headVersionUnknown: the membership checks found automatic members and
	// no known version of the head binary to match them on. The write is
	// refused for it after every other check (ErrClusterHeadBinaryVersionUnknown).
	headVersionUnknown bool
	// readMembers and readGPUs are the stored rows the plan was built from:
	// the membership is replaced only while it still holds them, so measured
	// values the gateway wrote since are never overwritten with an older
	// read (see writeClusterMembers).
	readMembers []routing.ClusterMember
	readGPUs    []routing.ClusterMemberGPU
	// replan plans the rows again from a fresh read of the stored rows; its
	// members, gpus and allocated replace this plan's. nil when the plan
	// stores no members.
	replan func(ctx context.Context, prevMembers []routing.ClusterMember, prevGPUs []routing.ClusterMemberGPU) (clusterPlan, error)
}

func (p clusterPlan) applyTo(spec *routing.RuntimeSpec) {
	spec.ClusterEnabled = p.enabled
	spec.MemberMode = p.memberMode
	spec.ClusterNetwork = p.network
	spec.RPCTransport = p.transport
	spec.LANExposureAck = p.lanAck
	spec.DeviceBackend = p.deviceBackend
}

// keepStoredCluster is the plan of a write that leaves the cluster
// configuration as stored (a benchmark writer's).
func keepStoredCluster(prior runtimeSpecPrior, f runtimeSpecFields) clusterPlan {
	if !prior.ok {
		return clusterPlan{memberMode: f.memberMode, deviceBackend: f.deviceBackend}
	}
	return clusterPlan{
		enabled:       prior.spec.ClusterEnabled,
		memberMode:    normalizeMemberMode(prior.spec.MemberMode),
		network:       prior.spec.ClusterNetwork,
		transport:     prior.spec.RPCTransport,
		lanAck:        prior.spec.LANExposureAck,
		deviceBackend: normalizeDeviceBackend(prior.spec.DeviceBackend),
	}
}

func normalizeDeviceBackend(b string) string {
	if b == "" {
		return deviceBackendAuto
	}
	return b
}

func normalizeMemberMode(m string) string {
	if m == "" {
		return memberModeAutomatic
	}
	return m
}

// validateClusterEnums validates and defaults the request's device backend
// (every spec has one) and the three cluster enums, in that order. An empty
// network or transport stays empty here; validateClusterHead resolves it
// from the membership.
func validateClusterEnums(req PutRuntimeSpecRequest) (backend, mode, network, transport string, err error) {
	backend = normalizeDeviceBackend(strings.TrimSpace(req.DeviceBackend))
	if !slices.Contains(rpcMemberDeviceBackends, backend) {
		return "", "", "", "", ErrRPCMemberDeviceBackendInvalid
	}
	mode = normalizeMemberMode(strings.TrimSpace(req.MemberMode))
	if mode != memberModeAutomatic && mode != memberModeManual {
		return "", "", "", "", ErrClusterMemberModeInvalid
	}
	network = strings.TrimSpace(req.ClusterNetwork)
	if network != "" && network != clusterNetworkMesh && network != clusterNetworkLAN {
		return "", "", "", "", ErrClusterNetworkInvalid
	}
	transport = strings.TrimSpace(req.RPCTransport)
	if transport != "" && transport != rpcTransportDirect && transport != rpcTransportRelay {
		return "", "", "", "", ErrClusterTransportInvalid
	}
	return backend, mode, network, transport, nil
}

// clusterEntry is one requested member with what its checks read: the
// server, the manual spec and its GPU rows (manual mode), and the ordered GPU
// selection with its estimates.
type clusterEntry struct {
	manual bool
	// visible: the caller manages the entry's server. Only an unchanged
	// membership (validateClusterHead) can hold an entry that is not.
	visible  bool
	req      ClusterMemberDTO
	server   routing.AIServer
	spec     routing.RPCMemberSpec
	gpus     []int
	estimate map[int]int
}

// specID is the manual member spec the entry uses, "" for an automatic one.
func (e clusterEntry) specID() string { return e.spec.ID }

// refusal is sentinel with detail about the entry, for a caller who may see
// its server; for any other caller it is the bare sentinel, so the refusal
// says nothing about a server it hides.
func (e clusterEntry) refusal(sentinel error, format string, args ...any) error {
	if !e.visible {
		return sentinel
	}
	return fmt.Errorf("%w: "+format, append([]any{sentinel}, args...)...)
}

// validateClusterHead runs the cluster checks of an operator's write and
// plans the rows it stores. They run after validateRuntimeSpecRequest -- every
// one is newer than the request validations -- and in this order, which is
// part of the API like that function's:
//
//  1. cluster off: ${RPC_SERVERS}/${RPC_DEVICES} anywhere in args, env or
//     a config file's content (the agent resolves them in all three);
//     nothing else. Dropping the membership only frees the member hosts, so
//     the head's rights suffice, as for deleting the head.
//  2. cluster on, the head's own document: effective type llama_cpp, no
//     "=" form of the scanned options (llama-server refuses it),
//     ${RPC_SERVERS} as the value of --rpc or LLAMA_ARG_RPC, exactly once
//     and through one of the two, a device list that names the RPC devices,
//     a device list llama-server parses after the RPC servers, at least one
//     member, not replicable, an explicit startup timeout.
//  3. the member list: each entry's settings, one entry per server, no
//     member on the head's own server. An entry without a server id keeps
//     the stored member at its position (the read view hides members on
//     servers the caller may not manage). When the membership, the member
//     mode, the network, the transport and the head binary are all as
//     stored, steps 4 to 8 are skipped: such a write changes no member
//     server's document, so the head's own rights suffice and a member the
//     caller cannot see does not lock it out of its head. The binary belongs
//     to that list because it decides which member builds are compatible
//     (step 6 and 7); a switch, even to the same build at another path, runs
//     every member check.
//  4. rights: the caller manages every member server the write keeps or
//     adds (a member it drops needs no rights: removal only frees the host),
//     and every member consents to the head (its member spec's
//     allowed_head_groups in manual mode, the server's rpc_allowed_head_groups
//     in automatic mode, intersected with the head server's admin groups; an
//     empty list means the member server's own admin groups; consent binds
//     every caller, system scope included, since the runtime document
//     re-checks it on every derivation). Both answer server-not-found, so a
//     caller learns nothing about a server it may not use. They run before
//     the member checks below, which would otherwise tell such a caller
//     about that server's settings.
//  5. manual specs: exists on that server, enabled, not used by another head.
//  6. automatic members: RPC membership enabled on the server, a
//     ggml-rpc-server whose version matches the head binary's. When the head
//     binary's version is unknown there is nothing to match; that refusal
//     (ErrClusterHeadBinaryVersionUnknown) is the newest save code and comes
//     last of the whole write, after the listen-port check.
//  7. manual members: a member binary whose known version differs from the
//     head's (unknown only warns).
//  8. network (mesh: NetBird with tracking group and mesh IP everywhere; lan:
//     a LAN address everywhere), then transport (direct mesh: the NetBird
//     policy preconditions; direct lan: the exposure acknowledgement).
//  9. member GPUs: an explicit budget for Apple GPUs, an estimate for known
//     non-NVIDIA GPUs of an automatic member, and --tensor-split on the head
//     for any member with known non-NVIDIA GPUs. These rules also hold for a
//     member the caller cannot see (a head edit such as dropping
//     --tensor-split would otherwise leave an invalid head stored); the
//     refusal then carries no detail about that member.
func (s *Service) validateClusterHead(ctx context.Context, p auth.Token, app routing.Application, req PutRuntimeSpecRequest, f runtimeSpecFields, prior runtimeSpecPrior) (clusterPlan, error) {
	plan := clusterPlan{
		enabled: req.ClusterEnabled, memberMode: f.memberMode, network: f.clusterNetwork, transport: f.rpcTransport,
		lanAck: req.LANExposureAck, deviceBackend: f.deviceBackend,
	}
	prevMembers, prevGPUs, err := s.priorClusterMembers(ctx, prior)
	if err != nil {
		return clusterPlan{}, err
	}
	for _, m := range prevMembers {
		plan.memberServers = appendUnique(plan.memberServers, m.ServerID)
	}
	plan.readMembers, plan.readGPUs = prevMembers, prevGPUs
	scan := scanClusterArgs(req.Args, req.Env)
	if !req.ClusterEnabled {
		if scan.clusterPlaceholder || configFilesHaveClusterPlaceholder(req.ConfigFiles) {
			return clusterPlan{}, ErrClusterRPCPlaceholderWithoutCluster
		}
		plan.writeMembers = len(prevMembers) > 0
		return plan, nil
	}
	if err := validateClusterHeadDocument(req, f, scan); err != nil {
		return clusterPlan{}, err
	}
	entries, err := clusterEntries(req.ClusterMembers, f.memberMode, app.ServerID, prevMembers, prevGPUs)
	if err != nil {
		return clusterPlan{}, err
	}
	headServer, err := s.routes.AIServerByID(ctx, app.ServerID)
	if err != nil {
		return clusterPlan{}, err
	}
	w := clusterHeadWrite{p: p, req: req, f: f, prior: prior, entries: entries, headServer: headServer}
	if err := s.checkClusterMembershipOrKeep(ctx, w, prevMembers, prevGPUs, &plan); err != nil {
		return clusterPlan{}, err
	}
	if err := s.checkClusterMemberGPUs(ctx, entries, scan); err != nil {
		return clusterPlan{}, err
	}
	if err := s.planClusterRows(ctx, &plan, w, prevMembers, prevGPUs); err != nil {
		return clusterPlan{}, err
	}
	plan.replan = func(ctx context.Context, prevMembers []routing.ClusterMember, prevGPUs []routing.ClusterMemberGPU) (clusterPlan, error) {
		var fresh clusterPlan
		err := s.planClusterRows(ctx, &fresh, w, prevMembers, prevGPUs)
		return fresh, err
	}
	return plan, nil
}

// clusterHeadWrite is one operator head write as the membership checks and
// the row planning read it: the caller, the request and its validated
// fields, the stored spec, the requested members and the head's server.
type clusterHeadWrite struct {
	p          auth.Token
	req        PutRuntimeSpecRequest
	f          runtimeSpecFields
	prior      runtimeSpecPrior
	entries    []clusterEntry
	headServer routing.AIServer
}

// priorClusterMembers reads the stored membership of the spec a write
// replaces; none for a new spec.
func (s *Service) priorClusterMembers(ctx context.Context, prior runtimeSpecPrior) ([]routing.ClusterMember, []routing.ClusterMemberGPU, error) {
	if !prior.ok {
		return nil, nil, nil
	}
	return s.routes.ClusterMembers(ctx, prior.spec.ID)
}

// clusterSettingsUnchanged reports whether a write keeps the stored head's
// cluster settings that decide its members' documents: still a head, the
// same member mode, network, transport and binary.
func clusterSettingsUnchanged(prior runtimeSpecPrior, f runtimeSpecFields) bool {
	if !prior.ok || !prior.spec.ClusterEnabled {
		return false
	}
	return normalizeMemberMode(prior.spec.MemberMode) == f.memberMode && prior.spec.ClusterNetwork == f.clusterNetwork &&
		prior.spec.RPCTransport == f.rpcTransport && prior.spec.Binary == f.binary
}

// checkClusterMembershipOrKeep runs steps 4 to 8 of validateClusterHead, or,
// for a write that keeps the stored cluster settings and membership, only
// loads the stored entries and checks the head's own acknowledgement.
func (s *Service) checkClusterMembershipOrKeep(ctx context.Context, w clusterHeadWrite, prevMembers []routing.ClusterMember, prevGPUs []routing.ClusterMemberGPU, plan *clusterPlan) error {
	unchanged := clusterSettingsUnchanged(w.prior, w.f) && membershipUnchanged(w.entries, prevMembers, prevGPUs)
	if unchanged {
		// A manual member whose spec is gone runs the full checks instead,
		// which refuse it: kept as stored it would turn into an automatic
		// member.
		var err error
		if unchanged, err = s.loadStoredClusterEntries(ctx, w.p, w.entries); err != nil {
			return err
		}
	}
	if !unchanged {
		return s.checkClusterMembership(ctx, w, plan)
	}
	// The acknowledgement is the head's own setting: withdrawing it from a
	// direct lan cluster is refused like a missing one.
	if plan.transport == rpcTransportDirect && plan.network == clusterNetworkLAN && !w.req.LANExposureAck {
		return ErrClusterLANAckRequired
	}
	return nil
}

// validateClusterHeadDocument is step 2 of validateClusterHead: what the
// head's own document must say, read from the request alone.
func validateClusterHeadDocument(req PutRuntimeSpecRequest, f runtimeSpecFields, scan clusterArgScan) error {
	if routing.EffectiveRuntimeSpecType(routing.RuntimeSpec{Type: f.specType, Binary: f.binary}) != routing.RuntimeSpecTypeLlamaCpp {
		return ErrClusterTypeInvalid
	}
	if err := validateClusterHeadArgs(scan); err != nil {
		return err
	}
	if len(req.ClusterMembers) == 0 {
		return ErrClusterMembersRequired
	}
	if req.Replicable {
		return ErrClusterHeadReplicable
	}
	// A cold cluster start waits for every member and then the head, so the
	// head's own bound is the operator's to set, never the default.
	if req.StartupTimeoutSeconds <= 0 {
		return ErrClusterStartupTimeoutRequired
	}
	return nil
}

// validateClusterHeadArgs is the part of step 2 that the head's args and env
// decide: the options' form, ${RPC_SERVERS} and the device list.
func validateClusterHeadArgs(scan clusterArgScan) error {
	if scan.equalsForm {
		return ErrClusterFlagEqualsForm
	}
	if scan.rpcPlaceholderCount == 0 || !scan.rpcValueHasPlaceholder {
		return ErrClusterRPCPlaceholderMissing
	}
	// --rpc in env and argv runs llama-server's handler twice and registers
	// every server again, as does a second --rpc in argv.
	if scan.rpcPlaceholderCount > 1 || scan.rpcArgCount > 1 || (scan.rpcInArgs && scan.rpcInEnv) {
		return ErrClusterRPCTwice
	}
	if scan.hasDeviceList() && !scan.deviceHasRPC {
		return ErrClusterDeviceListMissingRPC
	}
	// llama-server parses every environment variable first, then argv left
	// to right, and --device resolves its names while it is parsed: RPC<n>
	// only exists once --rpc has been handled.
	if (scan.deviceInArgs && scan.rpcInArgs && !scan.rpcInEnv && scan.deviceArgIndex < scan.rpcArgIndex) ||
		(scan.deviceInEnv && scan.rpcInArgs) {
		return ErrClusterDeviceOrder
	}
	return nil
}

// clusterEntries is step 3 of validateClusterHead: each entry's settings,
// one entry per server and none on the head's server. Settings the member
// mode does not use are dropped (a manual member's settings live on its
// spec; an automatic member has no spec).
func clusterEntries(members []ClusterMemberDTO, mode, headServerID string, prevMembers []routing.ClusterMember, prevGPUs []routing.ClusterMemberGPU) ([]clusterEntry, error) {
	entries := make([]clusterEntry, 0, len(members))
	seen := map[string]bool{}
	for i, m := range members {
		m.ServerID = strings.TrimSpace(m.ServerID)
		m.MemberSpecID = strings.TrimSpace(m.MemberSpecID)
		if m.ServerID == "" && i < len(prevMembers) {
			m = storedMemberDTO(prevMembers[i], prevGPUs)
		}
		e, err := newClusterEntry(m, mode)
		if err != nil {
			return nil, err
		}
		if seen[m.ServerID] {
			return nil, ErrClusterMemberServerTwice
		}
		seen[m.ServerID] = true
		entries = append(entries, e)
	}
	for _, e := range entries {
		if e.req.ServerID == headServerID {
			return nil, ErrClusterMemberOnHeadServer
		}
	}
	return entries, nil
}

// newClusterEntry builds one requested member's entry in mode: an automatic
// member's settings are validated and its GPU selection read; a manual
// member's own settings live on its spec and are dropped.
func newClusterEntry(m ClusterMemberDTO, mode string) (clusterEntry, error) {
	e := clusterEntry{manual: mode == memberModeManual, req: m, estimate: map[int]int{}}
	if mode != memberModeAutomatic {
		e.req.Threads, e.req.CacheEnabled, e.req.DeviceBackend, e.req.RAMEstimateMB, e.req.GPUs = 0, false, deviceBackendAuto, 0, nil
		return e, nil
	}
	if m.Threads < 0 || m.RAMEstimateMB < 0 {
		return clusterEntry{}, ErrRuntimeSpecTuningInvalid
	}
	gpus := make([]RuntimeSpecGPUDTO, 0, len(m.GPUs))
	for _, g := range m.GPUs {
		gpus = append(gpus, RuntimeSpecGPUDTO{Index: g.Index, VRAMEstimateMB: g.VRAMEstimateMB})
		e.gpus = append(e.gpus, g.Index)
		e.estimate[g.Index] = g.VRAMEstimateMB
	}
	if err := validateRuntimeSpecGPUs(gpus); err != nil {
		return clusterEntry{}, err
	}
	e.req.DeviceBackend = normalizeDeviceBackend(strings.TrimSpace(m.DeviceBackend))
	if !slices.Contains(rpcMemberDeviceBackends, e.req.DeviceBackend) {
		return clusterEntry{}, ErrRPCMemberDeviceBackendInvalid
	}
	e.req.MemberSpecID = ""
	return e, nil
}

// storedMemberDTO is the request entry that keeps a stored member as it is.
func storedMemberDTO(row routing.ClusterMember, gpus []routing.ClusterMemberGPU) ClusterMemberDTO {
	m := ClusterMemberDTO{
		ServerID: row.ServerID, MemberSpecID: row.MemberSpecID, Threads: row.Threads, CacheEnabled: row.CacheEnabled,
		DeviceBackend: row.DeviceBackend, RAMEstimateMB: row.RAMEstimateMB, GPUs: []ClusterMemberGPUDTO{},
	}
	for _, g := range gpus {
		if g.ServerID == row.ServerID {
			m.GPUs = append(m.GPUs, ClusterMemberGPUDTO{Index: g.GPUIndex, VRAMEstimateMB: g.VRAMEstimateMB})
		}
	}
	return m
}

// membershipUnchanged reports whether entries are the stored membership:
// the same servers in the same order with the same member spec, and for an
// automatic member the same settings and GPU selection with the same
// estimates.
func membershipUnchanged(entries []clusterEntry, prevMembers []routing.ClusterMember, prevGPUs []routing.ClusterMemberGPU) bool {
	if len(entries) != len(prevMembers) {
		return false
	}
	for i, e := range entries {
		prev := prevMembers[i]
		if e.req.ServerID != prev.ServerID || e.req.MemberSpecID != prev.MemberSpecID {
			return false
		}
		if !e.manual && !automaticMemberUnchanged(e, prev, prevGPUs) {
			return false
		}
	}
	return true
}

// automaticMemberUnchanged reports whether an automatic entry keeps the
// stored member's settings and its GPU selection with the same estimates.
func automaticMemberUnchanged(e clusterEntry, prev routing.ClusterMember, prevGPUs []routing.ClusterMemberGPU) bool {
	stored := storedMemberDTO(prev, prevGPUs)
	if e.req.Threads != prev.Threads || e.req.CacheEnabled != prev.CacheEnabled || e.req.RAMEstimateMB != prev.RAMEstimateMB ||
		e.req.DeviceBackend != normalizeDeviceBackend(prev.DeviceBackend) || len(e.gpus) != len(stored.GPUs) {
		return false
	}
	for j, g := range stored.GPUs {
		if e.gpus[j] != g.Index || e.estimate[g.Index] != g.VRAMEstimateMB {
			return false
		}
	}
	return true
}

// loadStoredClusterEntries fills the entries of an unchanged membership
// without authorizing them: each entry's server, its manual spec and whether
// the caller may see the server. complete is false when a manual member's
// spec is gone (the reference has no foreign key); the entries are then not
// usable as stored.
func (s *Service) loadStoredClusterEntries(ctx context.Context, p auth.Token, entries []clusterEntry) (complete bool, err error) {
	for i := range entries {
		e := &entries[i]
		server, err := s.routes.AIServerByID(ctx, e.req.ServerID)
		if err != nil {
			return false, ErrServerNotFound
		}
		e.server, e.visible = server, s.callerManagesServer(ctx, p, server.ID)
		if !e.manual {
			continue
		}
		spec, ok, err := s.routes.RPCMemberSpecByID(ctx, e.req.MemberSpecID)
		if err != nil {
			return false, err
		}
		if !ok || spec.ServerID != server.ID {
			return false, nil
		}
		if err := s.loadEntrySpec(ctx, e, spec); err != nil {
			return false, err
		}
	}
	return true, nil
}

// loadEntrySpec sets a manual entry's spec and takes its GPU selection from
// the spec's GPU rows.
func (s *Service) loadEntrySpec(ctx context.Context, e *clusterEntry, spec routing.RPCMemberSpec) error {
	gpuRows, err := s.routes.RPCMemberSpecGPUs(ctx, spec.ID)
	if err != nil {
		return err
	}
	e.spec, e.gpus = spec, nil
	for _, g := range gpuRows {
		e.gpus = append(e.gpus, g.GPUIndex)
		e.estimate[g.GPUIndex] = g.VRAMEstimateMB
	}
	return nil
}

// checkClusterMembership is steps 4 to 8 of validateClusterHead, for a write
// that changes the membership, the member mode, the network or the
// transport. It resolves plan's network and transport.
func (s *Service) checkClusterMembership(ctx context.Context, w clusterHeadWrite, plan *clusterPlan) error {
	if err := s.authorizeClusterMembers(ctx, w.p, w.headServer.ID, w.entries); err != nil {
		return err
	}
	headSpecID := ""
	if w.prior.ok {
		headSpecID = w.prior.spec.ID
	}
	if err := s.checkClusterMemberSpecs(ctx, w.entries, headSpecID); err != nil {
		return err
	}
	var err error
	if plan.headVersionUnknown, err = s.checkClusterMemberBinaries(ctx, w.headServer.ID, w.f.binary, w.entries); err != nil {
		return err
	}
	plan.network, plan.transport, err = s.checkClusterNetwork(ctx, w.headServer, w.entries, plan.network, plan.transport, w.req.LANExposureAck)
	return err
}

// authorizeClusterMembers is step 4 of validateClusterHead. It fills each
// entry's server and, in manual mode, its member spec.
func (s *Service) authorizeClusterMembers(ctx context.Context, p auth.Token, headServerID string, entries []clusterEntry) error {
	for i := range entries {
		if entries[i].req.ServerID == "" {
			return ErrServerNotFound
		}
		server, err := s.authorizeServer(ctx, p, entries[i].req.ServerID)
		if err != nil {
			return err
		}
		entries[i].server, entries[i].visible = server, true
	}
	headGroups, err := s.routes.ServerAdminGroups(ctx, headServerID)
	if err != nil {
		return err
	}
	for i := range entries {
		if err := s.checkEntryConsent(ctx, &entries[i], headGroups); err != nil {
			return err
		}
	}
	return nil
}

// checkEntryConsent loads a manual entry's spec and requires the member's
// consent for a head linked to headGroups: the spec's consent list in manual
// mode, the server's in automatic mode. A missing consent is
// ErrServerNotFound, like missing rights.
func (s *Service) checkEntryConsent(ctx context.Context, e *clusterEntry, headGroups []string) error {
	consentRaw := e.server.RPCAllowedHeadGroups
	if e.manual {
		spec, err := s.ownedRPCMemberSpec(ctx, e.server.ID, e.req.MemberSpecID)
		if err != nil {
			return err
		}
		if err := s.loadEntrySpec(ctx, e, spec); err != nil {
			return err
		}
		consentRaw = spec.AllowedHeadGroups
	}
	admits, err := s.memberConsentAdmits(ctx, consentRaw, e.server.ID, headGroups)
	if err != nil {
		return err
	}
	if !admits {
		return ErrServerNotFound
	}
	return nil
}

// memberConsentAdmits reports whether a member's stored consent list admits
// a head whose server is linked to headGroups. An empty list means the
// member server's own admin groups (the default consent). Consent is the
// member side's standing permission and binds every caller, system scope
// included: the runtime document re-checks it on every derivation, so a
// save past it would only store a head that is never pushed.
func (s *Service) memberConsentAdmits(ctx context.Context, consentRaw, memberServerID string, headGroups []string) (bool, error) {
	consent := decodeRPCHeadGroups(consentRaw)
	if len(consent) == 0 {
		var err error
		if consent, err = s.routes.ServerAdminGroups(ctx, memberServerID); err != nil {
			return false, err
		}
	}
	return consentAdmits(consent, headGroups), nil
}

// consentAdmits reports whether a member's consent groups admit a head whose
// server is linked to headGroups.
func consentAdmits(consent, headGroups []string) bool {
	for _, g := range consent {
		if slices.Contains(headGroups, g) {
			return true
		}
	}
	return false
}

// checkClusterMemberSpecs is step 5 of validateClusterHead: a manual member's
// spec is enabled and serves no other head (one head per member, since
// ggml-rpc-server serves one client at a time).
func (s *Service) checkClusterMemberSpecs(ctx context.Context, entries []clusterEntry, headSpecID string) error {
	for _, e := range entries {
		if !e.manual {
			continue
		}
		if !e.spec.Enabled {
			return fmt.Errorf("%w: member spec %s on server %q is disabled", ErrClusterMemberSpecDisabled, e.spec.ID, e.server.Name)
		}
		used, ok, err := s.routes.ClusterMemberBySpec(ctx, e.spec.ID)
		if err != nil {
			return err
		}
		if ok && used.HeadSpecID != headSpecID {
			return fmt.Errorf("%w: member spec %s on server %q serves another cluster head", ErrClusterMemberSpecTaken, e.spec.ID, e.server.Name)
		}
	}
	return nil
}

// binaryVersionKnown reports whether b carries a commit or a build to match
// on.
func binaryVersionKnown(b routing.ServerBinary) bool {
	commit, build := binaryIdentity(b)
	return commit != "" || build != ""
}

// binaryVersionsComparable reports whether a and b carry the same kind of
// identity (both a commit, or both a build), so a difference is a real one.
func binaryVersionsComparable(a, b routing.ServerBinary) bool {
	ac, ab := binaryIdentity(a)
	bc, bb := binaryIdentity(b)
	return (ac != "" && bc != "") || (ab != "" && bb != "")
}

// checkClusterMemberBinaries is steps 6 and 7 of validateClusterHead. Head
// and members must run the same llama.cpp build (same RPC protocol and graph
// cache limits), so automatic members get the ggml-rpc-server matching the
// head binary's version and need the head's version known; a manual member's
// binary is the operator's choice and refused only when both versions are
// known and differ. headVersionUnknown reports automatic members with no
// head version to match them on; the caller refuses that last of all.
func (s *Service) checkClusterMemberBinaries(ctx context.Context, headServerID, headBinary string, entries []clusterEntry) (headVersionUnknown bool, err error) {
	head, headListed, err := s.BinaryVersionFor(ctx, headServerID, headBinary)
	if err != nil {
		return false, err
	}
	headKnown := headListed && binaryVersionKnown(head)
	if err := requireRPCMembership(entries); err != nil {
		return false, err
	}
	if !headKnown {
		// Nothing to match automatic members on; the caller refuses that
		// last of all.
		return slices.ContainsFunc(entries, func(e clusterEntry) bool { return !e.manual }), nil
	}
	for _, e := range entries {
		if e.manual {
			continue
		}
		if err := s.checkAutomaticMemberBinary(ctx, e, head); err != nil {
			return false, err
		}
	}
	for _, e := range entries {
		if !e.manual {
			continue
		}
		if err := s.checkManualMemberBinary(ctx, e, head); err != nil {
			return false, err
		}
	}
	return false, nil
}

// requireRPCMembership refuses an automatic entry whose server does not
// offer RPC membership.
func requireRPCMembership(entries []clusterEntry) error {
	for _, e := range entries {
		if !e.manual && !e.server.RPCMemberEnabled {
			return fmt.Errorf("%w: server %q does not offer RPC membership in its runtime settings", ErrClusterMemberRPCDisabled, e.server.Name)
		}
	}
	return nil
}

// checkAutomaticMemberBinary requires a ggml-rpc-server of the head binary's
// version on an automatic entry's server.
func (s *Service) checkAutomaticMemberBinary(ctx context.Context, e clusterEntry, head routing.ServerBinary) error {
	_, ok, err := s.MatchingRPCBinary(ctx, e.server.ID, head)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: server %q has no ggml-rpc-server of version %s", ErrClusterMemberNoMatchingBinary, e.server.Name, head.EffectiveVersion())
	}
	return nil
}

// checkManualMemberBinary refuses a manual entry's binary whose known
// version differs from the head binary's; an unknown one only warns.
func (s *Service) checkManualMemberBinary(ctx context.Context, e clusterEntry, head routing.ServerBinary) error {
	member, mismatch, err := s.memberBinaryMismatch(ctx, e.server.ID, e.spec.Binary, head)
	if err != nil {
		return err
	}
	if mismatch {
		return fmt.Errorf("%w: member binary %s on server %q is version %s, the head's is %s",
			ErrClusterVersionMismatch, e.spec.Binary, e.server.Name, member.EffectiveVersion(), head.EffectiveVersion())
	}
	return nil
}

// mismatchedMemberSpecs lists the ids of specs on serverID whose binary's
// known version differs from head's, reading the server's inventory once.
func (s *Service) mismatchedMemberSpecs(ctx context.Context, serverID string, specs []RPCMemberSpecDTO, head routing.ServerBinary) ([]string, error) {
	rows, err := s.routes.ServerBinaries(ctx, serverID)
	if err != nil {
		return nil, err
	}
	byPath := make(map[string]routing.ServerBinary, len(rows))
	for _, b := range rows {
		if _, seen := byPath[b.Path]; !seen {
			byPath[b.Path] = b
		}
	}
	var out []string
	for _, spec := range specs {
		member, listed := byPath[spec.Binary]
		if memberVersionDiffers(member, listed, head) {
			out = append(out, spec.ID)
		}
	}
	return out, nil
}

// memberBinaryMismatch reads a manual member's binary on serverID and reports
// whether its known version differs from head's (memberVersionDiffers).
func (s *Service) memberBinaryMismatch(ctx context.Context, serverID, memberBinary string, head routing.ServerBinary) (routing.ServerBinary, bool, error) {
	member, listed, err := s.BinaryVersionFor(ctx, serverID, memberBinary)
	if err != nil {
		return member, false, err
	}
	return member, memberVersionDiffers(member, listed, head), nil
}

// memberVersionDiffers is the manual member rule the save check and the
// candidates listing share: a listed member binary whose known version
// differs from head's. An unlisted binary or an unknown version is no
// mismatch (it only warns).
func memberVersionDiffers(member routing.ServerBinary, listed bool, head routing.ServerBinary) bool {
	return listed && binaryVersionKnown(member) && binaryVersionsComparable(head, member) && !binariesSameVersion(head, member)
}

// netbirdMeshReady reports whether a server can take part in a mesh
// cluster: NetBird on, a tracking group and a known mesh IP.
func netbirdMeshReady(s routing.AIServer) bool {
	return s.NetbirdEnabled && s.NetbirdGroupID != "" && s.NetbirdIP != ""
}

// lanAddressKnown reports whether a server has a LAN address for a lan
// cluster: its lan_address, else its Domain -- except on a NetBird-enabled
// server, whose Domain the peer sync rewrites to the NetBird DNS name.
func lanAddressKnown(s routing.AIServer) bool {
	return s.LANAddress != "" || (!s.NetbirdEnabled && strings.TrimSpace(s.Domain) != "")
}

// defaultClusterNetwork is mesh when every server is mesh-ready, else lan.
func defaultClusterNetwork(servers []routing.AIServer) string {
	for _, s := range servers {
		if !netbirdMeshReady(s) {
			return clusterNetworkLAN
		}
	}
	return clusterNetworkMesh
}

// directMeshAvailable reports the NetBird preconditions of a direct mesh
// cluster: the gateway manages policies, deny-by-default is on and enforced,
// and the last policy pass saw NetBird's catch-all Default policy disabled.
// Without them every mesh peer could reach a member's unauthenticated RPC
// port. A failed settings read is an error, never every switch read as off:
// the runtime documents derive from this, and a guessed "off" would withhold
// and drain every running direct mesh cluster.
func (s *Service) directMeshAvailable(ctx context.Context) (bool, error) {
	values, err := s.readSystemSettings(ctx)
	if err != nil {
		return false, err
	}
	return NetbirdManagePolicies(values) && NetbirdDenyByDefault(values) && NetbirdDenyByDefaultEnforce(values) &&
		defaultPolicyDisabledIn(values), nil
}

// defaultRPCTransport: in mesh, direct where its NetBird preconditions hold,
// else relay; in lan, direct.
func (s *Service) defaultRPCTransport(ctx context.Context, network string) (string, error) {
	if network != clusterNetworkMesh {
		return rpcTransportDirect, nil
	}
	direct, err := s.directMeshAvailable(ctx)
	if err != nil || !direct {
		return rpcTransportRelay, err
	}
	return rpcTransportDirect, nil
}

// checkClusterNetwork is step 8 of validateClusterHead; it returns the
// network and transport the write stores, defaults resolved.
func (s *Service) checkClusterNetwork(ctx context.Context, headServer routing.AIServer, entries []clusterEntry, network, transport string, lanAck bool) (string, string, error) {
	servers := []routing.AIServer{headServer}
	for _, e := range entries {
		servers = append(servers, e.server)
	}
	if network == "" {
		network = defaultClusterNetwork(servers)
	}
	for _, srv := range servers {
		if err := checkServerInNetwork(srv, network); err != nil {
			return "", "", err
		}
	}
	if transport == "" {
		var err error
		if transport, err = s.defaultRPCTransport(ctx, network); err != nil {
			return "", "", err
		}
	}
	if err := s.checkClusterTransport(ctx, network, transport, lanAck); err != nil {
		return "", "", err
	}
	return network, transport, nil
}

// checkServerInNetwork requires what network needs of one server: NetBird
// with a tracking group and a known mesh IP in mesh, a LAN address in lan.
func checkServerInNetwork(srv routing.AIServer, network string) error {
	if network == clusterNetworkMesh && !netbirdMeshReady(srv) {
		return fmt.Errorf("%w: server %q needs NetBird with a tracking group and a known mesh IP", ErrClusterMeshUnavailable, srv.Name)
	}
	if network == clusterNetworkLAN && !lanAddressKnown(srv) {
		return fmt.Errorf("%w: server %q needs a LAN address", ErrClusterLANAddressRequired, srv.Name)
	}
	return nil
}

// checkClusterTransport requires what a direct transport needs: the NetBird
// policy preconditions in mesh, the exposure acknowledgement in lan.
func (s *Service) checkClusterTransport(ctx context.Context, network, transport string, lanAck bool) error {
	if transport != rpcTransportDirect {
		return nil
	}
	if network == clusterNetworkMesh {
		direct, err := s.directMeshAvailable(ctx)
		if err != nil {
			return err
		}
		if !direct {
			return ErrClusterDirectUnavailable
		}
	}
	if network == clusterNetworkLAN && !lanAck {
		return ErrClusterLANAckRequired
	}
	return nil
}

// hostGPU is what a server's hardware report says about one GPU.
type hostGPU struct {
	nvidia  bool
	apple   bool
	totalMB int
}

// serverHostGPUs reads serverID's hardware report by GPU index. A server
// without a (readable) report answers an empty map: its vendors are unknown,
// and no vendor-dependent rule applies to it.
func (s *Service) serverHostGPUs(ctx context.Context, serverID string) (map[int]hostGPU, error) {
	out := map[int]hostGPU{}
	hw, ok, err := s.routes.ServerHardwareByServer(ctx, serverID)
	if err != nil || !ok {
		return out, err
	}
	var report struct {
		OS   string `json:"os"`
		GPUs []struct {
			Index            int    `json:"index"`
			Name             string `json:"name"`
			PCIBusID         string `json:"pci_bus_id"`
			MemoryTotalBytes int64  `json:"memory_total_bytes"`
		} `json:"gpus"`
	}
	if json.Unmarshal([]byte(hw.ReportJSON), &report) != nil {
		return out, nil
	}
	for _, g := range report.GPUs {
		apple := report.OS == "darwin" || strings.Contains(strings.ToLower(g.Name), "apple")
		out[g.Index] = hostGPU{
			nvidia:  !apple && gpuLooksNVIDIA(g.Name, g.PCIBusID),
			apple:   apple,
			totalMB: int(g.MemoryTotalBytes / (1024 * 1024)),
		}
	}
	return out, nil
}

// checkClusterMemberGPUs is step 9 of validateClusterHead. An Apple GPU gets
// no auto-detected budget on its agent, so a member on one needs the
// operator's. A known non-NVIDIA GPU is never measured per process: an
// automatic member needs the operator's estimate for it, and the head needs
// --tensor-split, since the member's real share would stay invisible to its
// agent.
func (s *Service) checkClusterMemberGPUs(ctx context.Context, entries []clusterEntry, scan clusterArgScan) error {
	hosts := make([]map[int]hostGPU, len(entries))
	for i, e := range entries {
		var err error
		if hosts[i], err = s.serverHostGPUs(ctx, e.server.ID); err != nil {
			return err
		}
	}
	for i, e := range entries {
		if err := s.checkAppleMemberBudgets(ctx, e, hosts[i]); err != nil {
			return err
		}
	}
	for i, e := range entries {
		if !e.manual && slices.ContainsFunc(e.gpus, func(idx int) bool { return knownNonNVIDIA(hosts[i], idx) && e.estimate[idx] == 0 }) {
			return ErrRPCMemberVRAMEstimateRequired
		}
	}
	for i, e := range entries {
		if !scan.tensorSplit && slices.ContainsFunc(e.gpus, func(idx int) bool { return knownNonNVIDIA(hosts[i], idx) }) {
			return e.refusal(ErrClusterTensorSplitRequired, "server %q has non-NVIDIA GPUs", e.server.Name)
		}
	}
	return nil
}

// knownNonNVIDIA reports whether the hardware report lists GPU idx and it is
// not an NVIDIA card.
func knownNonNVIDIA(host map[int]hostGPU, idx int) bool {
	g, known := host[idx]
	return known && !g.nvidia
}

// checkAppleMemberBudgets requires an operator GPU budget for every Apple GPU
// an entry selects.
func (s *Service) checkAppleMemberBudgets(ctx context.Context, e clusterEntry, host map[int]hostGPU) error {
	if !slices.ContainsFunc(e.gpus, func(idx int) bool { return host[idx].apple }) {
		return nil
	}
	budgets, err := s.routes.ServerGPUBudgets(ctx, e.server.ID)
	if err != nil {
		return err
	}
	for _, idx := range e.gpus {
		if host[idx].apple && !slices.ContainsFunc(budgets, func(b routing.ServerGPUBudget) bool { return b.GPUIndex == idx && b.BudgetMB > 0 }) {
			return e.refusal(ErrClusterMemberBudgetRequired, "GPU %d of server %q needs a GPU budget", idx, e.server.Name)
		}
	}
	return nil
}

// planClusterRows builds the membership rows of a valid head write. A member
// that stays on its server (same server, and in manual mode the same spec)
// keeps its measured values per kept GPU index; a member on a server with a
// row before keeps that row's port, any other gets a new one. When the write
// changes the distribution of the model over the devices -- members added,
// removed or reordered, a member's GPU selection, or the head's args, env or
// GPU rows -- the kept values become a stale floor until the next formed run
// replaces them.
func (s *Service) planClusterRows(ctx context.Context, plan *clusterPlan, w clusterHeadWrite, prevMembers []routing.ClusterMember, prevGPUs []routing.ClusterMemberGPU) error {
	stored := indexStoredMembership(prevMembers, prevGPUs)
	changed, err := s.clusterDistributionChanged(ctx, w, prevMembers, stored.gpuOrder)
	if err != nil {
		return err
	}
	plan.writeMembers = true
	for i, e := range w.entries {
		row, gpus, allocated, err := s.planMemberRow(ctx, i, e, stored, changed)
		if err != nil {
			return err
		}
		if allocated {
			plan.allocated = append(plan.allocated, i)
		}
		plan.gpus = append(plan.gpus, gpus...)
		plan.members = append(plan.members, row)
		plan.memberServers = appendUnique(plan.memberServers, e.server.ID)
	}
	return nil
}

// storedMembership is a head's stored membership indexed for planning:
// the rows by server, each server's GPU indices in device order, and the GPU
// rows by server and index.
type storedMembership struct {
	byServer map[string]routing.ClusterMember
	gpuOrder map[string][]int
	gpuByKey map[string]map[int]routing.ClusterMemberGPU
}

func indexStoredMembership(prevMembers []routing.ClusterMember, prevGPUs []routing.ClusterMemberGPU) storedMembership {
	out := storedMembership{
		byServer: map[string]routing.ClusterMember{},
		gpuOrder: map[string][]int{},
		gpuByKey: map[string]map[int]routing.ClusterMemberGPU{},
	}
	for _, m := range prevMembers {
		out.byServer[m.ServerID] = m
	}
	for _, g := range prevGPUs {
		out.gpuOrder[g.ServerID] = append(out.gpuOrder[g.ServerID], g.GPUIndex)
		if out.gpuByKey[g.ServerID] == nil {
			out.gpuByKey[g.ServerID] = map[int]routing.ClusterMemberGPU{}
		}
		out.gpuByKey[g.ServerID][g.GPUIndex] = g
	}
	return out
}

// planMemberRow builds the membership row and GPU rows of the entry at
// position i; allocated reports a port allocated by this write (a server
// without a row before, or one without a port).
//
// A change of the distribution keeps the measured values as a stale floor
// and ends the member's current run: a run that began on the old split must
// never replace them (its peak describes that split) nor clear the stale
// mark. The gateway's usage writes are bound to the run they read, so the
// reset also refuses any write of a value read before this save.
func (s *Service) planMemberRow(ctx context.Context, i int, e clusterEntry, stored storedMembership, changed bool) (row routing.ClusterMember, gpus []routing.ClusterMemberGPU, allocated bool, err error) {
	row = routing.ClusterMember{
		Position: i, ServerID: e.server.ID, MemberSpecID: e.specID(),
		Threads: e.req.Threads, CacheEnabled: e.req.CacheEnabled, DeviceBackend: e.req.DeviceBackend, RAMEstimateMB: e.req.RAMEstimateMB,
	}
	prev, hadRow := stored.byServer[e.server.ID]
	kept := hadRow && prev.MemberSpecID == e.specID()
	if hadRow && prev.Port != 0 {
		row.Port = prev.Port
	} else {
		if row.Port, err = s.allocateRPCPort(ctx, e.server.ID); err != nil {
			return routing.ClusterMember{}, nil, false, err
		}
		allocated = true
	}
	measured := false
	if kept {
		row.RAMMeasuredMB, row.MeasuredAt, row.Stale = prev.RAMMeasuredMB, prev.MeasuredAt, prev.Stale
		if !changed {
			row.RunPeakRAMMB, row.RunHeadInstance, row.RunStartedAt = prev.RunPeakRAMMB, prev.RunHeadInstance, prev.RunStartedAt
		}
		measured = prev.RAMMeasuredMB > 0
	}
	gpus, gpusMeasured := memberGPURows(e, stored.gpuByKey[e.server.ID], kept, changed)
	if changed && (measured || gpusMeasured) {
		row.Stale = true
	}
	return row, gpus, allocated, nil
}

// memberGPURows builds an entry's GPU rows in device order. A kept member
// carries each kept GPU's measured value (and, without a change of the
// distribution, its run peak) from prevGPUs; measured reports whether any
// carried value is known.
func memberGPURows(e clusterEntry, prevGPUs map[int]routing.ClusterMemberGPU, kept, changed bool) (gpus []routing.ClusterMemberGPU, measured bool) {
	for pos, idx := range e.gpus {
		g := routing.ClusterMemberGPU{ServerID: e.server.ID, GPUIndex: idx, Position: pos}
		if !e.manual {
			// A manual member's estimates live on its spec.
			g.VRAMEstimateMB = e.estimate[idx]
		}
		if prevGPU, ok := prevGPUs[idx]; kept && ok {
			g.VRAMMeasuredMB = prevGPU.VRAMMeasuredMB
			if !changed {
				g.RunPeakVRAMMB = prevGPU.RunPeakVRAMMB
			}
			measured = measured || prevGPU.VRAMMeasuredMB > 0
		}
		gpus = append(gpus, g)
	}
	return gpus, measured
}

// clusterDistributionChanged reports whether a head write changes how the
// model is split over the devices (see planClusterRows): the members, their
// order, specs or GPU selections, or the head's args, env or GPU rows.
func (s *Service) clusterDistributionChanged(ctx context.Context, w clusterHeadWrite, prevMembers []routing.ClusterMember, prevGPUOrder map[string][]int) (bool, error) {
	if membersMoved(w.entries, prevMembers, prevGPUOrder) {
		return true, nil
	}
	if !w.prior.ok {
		return false, nil
	}
	if w.prior.spec.Args != w.f.args || w.prior.spec.Env != w.f.env {
		return true, nil
	}
	headGPUs, err := s.routes.RuntimeSpecGPUs(ctx, w.prior.spec.ID)
	if err != nil {
		return false, err
	}
	if len(headGPUs) != len(w.req.GPUs) {
		return true, nil
	}
	for i, g := range headGPUs {
		if g.GPUIndex != w.req.GPUs[i].Index {
			return true, nil
		}
	}
	return false, nil
}

// membersMoved reports whether entries differ from the stored members in
// count, order, server, spec or GPU selection.
func membersMoved(entries []clusterEntry, prevMembers []routing.ClusterMember, prevGPUOrder map[string][]int) bool {
	if len(prevMembers) != len(entries) {
		return true
	}
	for i, e := range entries {
		if prevMembers[i].ServerID != e.server.ID || prevMembers[i].MemberSpecID != e.specID() || !slices.Equal(prevGPUOrder[e.server.ID], e.gpus) {
			return true
		}
	}
	return false
}

func appendUnique(ids []string, id string) []string {
	if slices.Contains(ids, id) {
		return ids
	}
	return append(ids, id)
}

// clusterRewriteAttempts bounds how often a membership rewrite starts over
// because the gateway wrote measured values between its read and its write.
const clusterRewriteAttempts = 5

// errClusterMembersChanged ends a membership rewrite whose stored rows kept
// changing under it; nothing of the rewrite is stored.
var errClusterMembersChanged = errors.New("cluster membership changed during the write")

// writeClusterMembers stores plan's membership rows for headSpecID, but only
// over the rows the plan was built from (SetClusterMembersIf): the plan
// carries measured values over, and the gateway may have raised them since.
// Then the stored rows are read again, the plan is rebuilt from them, and the
// write starts over. The store refuses with a conflict when, since the
// checks ran, another head took one of the manual member specs (one head per
// spec) or another gateway process allocated one of the new ports on the
// same server (one member per port and server; the allocation lock is per
// process). The first is ErrClusterMemberSpecTaken. For the second the new
// ports are allocated once more from the store's current rows and the write
// is repeated; a second conflict is ErrClusterPortExhausted.
func (s *Service) writeClusterMembers(ctx context.Context, headSpecID string, plan *clusterPlan) error {
	portsRetried := false
	for attempt := 0; attempt < clusterRewriteAttempts; {
		applied, err := s.routes.SetClusterMembersIf(ctx, headSpecID, plan.readMembers, plan.readGPUs, plan.members, plan.gpus)
		switch {
		case errors.Is(err, store.ErrConflict):
			if err := s.reallocateAfterConflict(ctx, headSpecID, plan, portsRetried); err != nil {
				return err
			}
			portsRetried = true
			continue
		case errors.Is(err, store.ErrNotFound):
			return s.clusterMembersNotFound(ctx, headSpecID)
		case err != nil:
			return err
		case applied:
			return nil
		}
		attempt++
		if err := s.replanFromStore(ctx, headSpecID, plan); err != nil {
			return err
		}
	}
	return errClusterMembersChanged
}

// reallocateAfterConflict handles a membership write the store refused with
// a conflict: ErrClusterMemberSpecTaken when another head took a manual
// member spec, else the ports this write allocated are allocated once more
// (ErrClusterPortExhausted when they already were, or when it allocated
// none).
func (s *Service) reallocateAfterConflict(ctx context.Context, headSpecID string, plan *clusterPlan, portsRetried bool) error {
	if err := s.checkClusterMemberSpecsFree(ctx, headSpecID, plan.members); err != nil {
		return err
	}
	if portsRetried || len(plan.allocated) == 0 {
		return ErrClusterPortExhausted
	}
	for _, i := range plan.allocated {
		port, err := s.allocateRPCPort(ctx, plan.members[i].ServerID)
		if err != nil {
			return err
		}
		plan.members[i].Port = port
	}
	return nil
}

// clusterMembersNotFound maps a membership write the store refused as not
// found: the head row went away under the write (a concurrent delete) unless
// a member server did.
func (s *Service) clusterMembersNotFound(ctx context.Context, headSpecID string) error {
	if _, ok, err := s.routes.RuntimeSpecByID(ctx, headSpecID); err == nil && !ok {
		return ErrRuntimeSpecNotFound
	}
	return ErrServerNotFound
}

// replanFromStore reads the stored membership again and, for a plan that
// stores members, plans its rows anew from it.
func (s *Service) replanFromStore(ctx context.Context, headSpecID string, plan *clusterPlan) error {
	var err error
	if plan.readMembers, plan.readGPUs, err = s.routes.ClusterMembers(ctx, headSpecID); err != nil {
		return err
	}
	if plan.replan == nil {
		return nil
	}
	fresh, err := plan.replan(ctx, plan.readMembers, plan.readGPUs)
	if err != nil {
		return err
	}
	plan.members, plan.gpus, plan.allocated = fresh.members, fresh.gpus, fresh.allocated
	return nil
}

// checkClusterMemberSpecsFree is ErrClusterMemberSpecTaken when another head
// uses one of members' manual member specs.
func (s *Service) checkClusterMemberSpecsFree(ctx context.Context, headSpecID string, members []routing.ClusterMember) error {
	for _, m := range members {
		if m.MemberSpecID == "" {
			continue
		}
		used, ok, err := s.routes.ClusterMemberBySpec(ctx, m.MemberSpecID)
		if err != nil {
			return err
		}
		if ok && used.HeadSpecID != headSpecID {
			return ErrClusterMemberSpecTaken
		}
	}
	return nil
}

// checkListenPortFree refuses a fixed listen_port that a cluster member on
// serverID uses as its RPC port. Only a port the write sets or changes is
// checked, so a re-save of an existing spec is never refused for a member
// allocated later. The member allocator skips fixed listen ports already
// stored, which covers the other direction.
func (s *Service) checkListenPortFree(ctx context.Context, serverID string, port int, prior runtimeSpecPrior) error {
	if port == 0 || (prior.ok && prior.spec.ListenPort == port) {
		return nil
	}
	taken, err := s.rpcMemberPortTaken(ctx, serverID, port)
	if err != nil {
		return err
	}
	if taken {
		return ErrRuntimeSpecPortConflictsRPCMember
	}
	return nil
}

// rpcMemberPortTaken reports whether a cluster member on serverID uses port.
func (s *Service) rpcMemberPortTaken(ctx context.Context, serverID string, port int) (bool, error) {
	if port == 0 {
		return false, nil
	}
	memberships, err := s.routes.ClusterMembershipsByServer(ctx, serverID)
	if err != nil {
		return false, err
	}
	return slices.ContainsFunc(memberships, func(m routing.ClusterMember) bool { return m.Port == port }), nil
}

// allocateRPCPort returns the lowest port in the RPC range that nothing on
// serverID uses: an application's port or TLS proxy port (the server_agent
// application's port is the agent's router_listen), a fixed listen_port of
// one of the server's runtime specs, or another cluster member's port on the
// server. ErrClusterPortExhausted when the range is full.
func (s *Service) allocateRPCPort(ctx context.Context, serverID string) (int, error) {
	taken := map[int]bool{}
	apps, err := s.routes.ApplicationsByServer(ctx, serverID)
	if err != nil {
		return 0, err
	}
	for _, app := range apps {
		taken[app.Port], taken[app.ProxyListenPort] = true, true
		if app.Type != routing.ProviderServerAgent {
			continue
		}
		specs, err := s.routes.RuntimeSpecsByApplication(ctx, app.ID)
		if err != nil {
			return 0, err
		}
		for _, spec := range specs {
			taken[spec.ListenPort] = true
		}
	}
	memberships, err := s.routes.ClusterMembershipsByServer(ctx, serverID)
	if err != nil {
		return 0, err
	}
	for _, m := range memberships {
		taken[m.Port] = true
	}
	for port := rpcPortFirst; port <= rpcPortLast; port++ {
		if !taken[port] {
			return port, nil
		}
	}
	return 0, ErrClusterPortExhausted
}

// systemSettingValues reads the system settings; nil (no store, or a failed
// read) reads every switch as off. Only for a reader that may guess: the
// cluster derivation and the save checks use readSystemSettings.
func (s *Service) systemSettingValues(ctx context.Context) map[string]string {
	values, err := s.readSystemSettings(ctx)
	if err != nil {
		return nil
	}
	return values
}

// readSystemSettings reads the system settings; without a store every switch
// is off (nil, no error), and a failed read is returned.
func (s *Service) readSystemSettings(ctx context.Context) (map[string]string, error) {
	if s.settings == nil {
		return nil, nil
	}
	return s.settings.SystemSettings(ctx)
}

// netbirdDefaultPolicyState is the stored netbird_default_policy_state.
type netbirdDefaultPolicyState struct {
	State string    `json:"state"`
	At    time.Time `json:"at"`
}

// parseNetbirdDefaultPolicyState reads the stored state: the JSON form, or a
// bare state name; anything else is unknown.
func parseNetbirdDefaultPolicyState(raw string) netbirdDefaultPolicyState {
	raw = strings.TrimSpace(raw)
	var st netbirdDefaultPolicyState
	if json.Unmarshal([]byte(raw), &st) == nil && st.State != "" {
		return st
	}
	switch raw {
	case NetbirdDefaultPolicyEnabled, NetbirdDefaultPolicyDisabled:
		return netbirdDefaultPolicyState{State: raw}
	}
	return netbirdDefaultPolicyState{State: NetbirdDefaultPolicyUnknown}
}

// defaultPolicyDisabledIn reports whether the settings values record that the
// last NetBird policy pass saw the Default policy disabled. Unknown -- never
// observed, or no settings -- is not disabled, so a direct mesh cluster is
// refused until a pass has seen it.
func defaultPolicyDisabledIn(values map[string]string) bool {
	return parseNetbirdDefaultPolicyState(values[netbirdDefaultPolicyStateKey]).State == NetbirdDefaultPolicyDisabled
}

// clusterMemberState is one stored member with what the read view needs.
type clusterMemberState struct {
	// visible: the reader may manage the member's server.
	visible  bool
	row      routing.ClusterMember
	gpuRows  []routing.ClusterMemberGPU
	server   routing.AIServer
	found    bool
	spec     routing.RPCMemberSpec
	specGPUs []routing.RPCMemberSpecGPU
}

// gpuParts are the member's GPUs in device order with estimate and measured
// value: the selection on the membership row (automatic) or the spec's GPU
// rows with the measured values from the membership row (manual).
func (m clusterMemberState) gpuParts() []ClusterMemberGPUDTO {
	if m.row.MemberSpecID == "" {
		out := make([]ClusterMemberGPUDTO, 0, len(m.gpuRows))
		for _, g := range m.gpuRows {
			out = append(out, ClusterMemberGPUDTO{Index: g.GPUIndex, VRAMEstimateMB: g.VRAMEstimateMB, VRAMMeasuredMB: g.VRAMMeasuredMB})
		}
		return out
	}
	measured := map[int]int{}
	for _, g := range m.gpuRows {
		measured[g.GPUIndex] = g.VRAMMeasuredMB
	}
	out := make([]ClusterMemberGPUDTO, 0, len(m.specGPUs))
	for _, g := range m.specGPUs {
		out = append(out, ClusterMemberGPUDTO{Index: g.GPUIndex, VRAMEstimateMB: g.VRAMEstimateMB, VRAMMeasuredMB: measured[g.GPUIndex]})
	}
	return out
}

// ramEstimate is the member's RAM estimate: the membership row's
// (automatic) or the spec's (manual).
func (m clusterMemberState) ramEstimate() int {
	if m.row.MemberSpecID != "" {
		return m.spec.RAMEstimateMB
	}
	return m.row.RAMEstimateMB
}

// loadClusterMembers reads headSpecID's membership with each member's
// server and manual spec. A server or spec that is gone leaves found false
// or the spec empty; the view still counts the member.
func (s *Service) loadClusterMembers(ctx context.Context, headSpecID string) ([]clusterMemberState, error) {
	rows, gpus, err := s.routes.ClusterMembers(ctx, headSpecID)
	if err != nil {
		return nil, err
	}
	out := make([]clusterMemberState, 0, len(rows))
	for _, row := range rows {
		st, err := s.loadClusterMember(ctx, row, gpus)
		if err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, nil
}

// loadClusterMember reads one membership row's GPU rows (from gpus), its
// server and its manual spec with the spec's GPU rows. A server that is gone
// leaves found false; any other failed read is an error, never a missing
// member (the runtime document withholds a cluster with a missing member).
func (s *Service) loadClusterMember(ctx context.Context, row routing.ClusterMember, gpus []routing.ClusterMemberGPU) (clusterMemberState, error) {
	st := clusterMemberState{row: row}
	for _, g := range gpus {
		if g.ServerID == row.ServerID {
			st.gpuRows = append(st.gpuRows, g)
		}
	}
	srv, err := s.routes.AIServerByID(ctx, row.ServerID)
	switch {
	case err == nil:
		st.server, st.found = srv, true
	case !errors.Is(err, store.ErrNotFound):
		return clusterMemberState{}, err
	}
	if row.MemberSpecID == "" {
		return st, nil
	}
	spec, ok, err := s.routes.RPCMemberSpecByID(ctx, row.MemberSpecID)
	if err != nil || !ok {
		return st, err
	}
	st.spec = spec
	if st.specGPUs, err = s.routes.RPCMemberSpecGPUs(ctx, spec.ID); err != nil {
		return clusterMemberState{}, err
	}
	return st, nil
}

// withClusterView fills a head document's cluster read view: the members (a
// member on a server p may not manage shows only its position), the cluster
// totals, the network and transport defaults and the warnings. A spec that
// is no head and has no membership is returned unchanged.
func (s *Service) withClusterView(ctx context.Context, p auth.Token, headServer routing.AIServer, dto RuntimeSpecDTO) (RuntimeSpecDTO, error) {
	if !dto.Configured || dto.ID == "" {
		return dto, nil
	}
	members, err := s.loadClusterMembers(ctx, dto.ID)
	if err != nil {
		return dto, err
	}
	if !dto.ClusterEnabled && len(members) == 0 {
		return dto, nil
	}
	hidden := s.fillClusterMemberDTOs(ctx, p, &dto, members)
	dto.ClusterVRAMTotalMB, dto.ClusterRAMTotalMB, dto.ClusterVRAMTotalComplete, dto.ClusterRAMTotalComplete = clusterTotals(dto, members)
	dto.ClusterTotalsComplete = dto.ClusterVRAMTotalComplete && dto.ClusterRAMTotalComplete
	network := withClusterNetworkDefault(&dto, headServer, members, len(hidden) == 0)
	if network != "" {
		if dto.ClusterTransportDefault, err = s.defaultRPCTransport(ctx, network); err != nil {
			return dto, err
		}
	}
	if !dto.ClusterEnabled {
		return dto, nil
	}
	return s.withClusterHeadState(ctx, dto, headServer, members, network, hidden)
}

// fillClusterMemberDTOs sets each member's visibility for p and dto's member
// list, and returns the positions of the members p may not see.
func (s *Service) fillClusterMemberDTOs(ctx context.Context, p auth.Token, dto *RuntimeSpecDTO, members []clusterMemberState) (hidden []int) {
	dto.ClusterMembers = make([]ClusterMemberDTO, 0, len(members))
	for i, m := range members {
		members[i].visible = m.found && (isSystem(p) || s.callerManagesServer(ctx, p, m.server.ID))
		if !members[i].visible {
			hidden = append(hidden, m.row.Position)
		}
		dto.ClusterMembers = append(dto.ClusterMembers, clusterMemberDTO(m, members[i].visible))
	}
	return hidden
}

// withClusterNetworkDefault sets dto's network default and returns the
// network the view resolves. The default turns on every member server's
// NetBird readiness, so it would tell a caller something about a server it
// may not see: with a hidden member (allVisible false) it stays empty, and
// the network with it unless it is stored.
func withClusterNetworkDefault(dto *RuntimeSpecDTO, headServer routing.AIServer, members []clusterMemberState, allVisible bool) string {
	network := dto.ClusterNetwork
	if !allVisible {
		return network
	}
	servers := []routing.AIServer{headServer}
	for _, m := range members {
		servers = append(servers, m.server)
	}
	dto.ClusterNetworkDefault = defaultClusterNetwork(servers)
	if network == "" {
		network = dto.ClusterNetworkDefault
	}
	return network
}

// withClusterHeadState fills an enabled head's withheld reason, its status
// with the stored peaks above budget, and its warnings.
func (s *Service) withClusterHeadState(ctx context.Context, dto RuntimeSpecDTO, headServer routing.AIServer, members []clusterMemberState, network string, hidden []int) (RuntimeSpecDTO, error) {
	var err error
	if dto.ClusterWithheldReason, err = s.clusterWithheldReason(ctx, dto.ID, headServer); err != nil {
		return dto, err
	}
	dto.ClusterStatus = s.headClusterStatus(dto.ID, dto.ClusterWithheldReason, hidden)
	if dto.ClusterStatus.PeaksAboveBudget, err = s.peaksAboveBudget(ctx, members); err != nil {
		return dto, err
	}
	transport := dto.RPCTransport
	if transport == "" {
		transport = dto.ClusterTransportDefault
	}
	if dto.ClusterWarnings, err = s.clusterWarnings(ctx, headServer.ID, dto, members, network, transport); err != nil {
		return dto, err
	}
	return dto, nil
}

// callerManagesServer is authorizeServer as a yes/no.
func (s *Service) callerManagesServer(ctx context.Context, p auth.Token, serverID string) bool {
	_, err := s.authorizeServer(ctx, p, serverID)
	return err == nil
}

func clusterMemberDTO(m clusterMemberState, visible bool) ClusterMemberDTO {
	if !visible {
		return ClusterMemberDTO{Position: m.row.Position, GPUs: []ClusterMemberGPUDTO{}}
	}
	dto := ClusterMemberDTO{
		Position: m.row.Position, ServerID: m.row.ServerID, ServerName: m.server.Name, MemberSpecID: m.row.MemberSpecID,
		GPUs: m.gpuParts(), Threads: m.row.Threads, CacheEnabled: m.row.CacheEnabled,
		DeviceBackend: normalizeDeviceBackend(m.row.DeviceBackend), RAMEstimateMB: m.ramEstimate(),
		RAMMeasuredMB: m.row.RAMMeasuredMB, Stale: m.row.Stale, Port: m.row.Port, Visible: true,
	}
	if m.row.MemberSpecID != "" {
		dto.CacheEnabled, dto.DeviceBackend = m.spec.CacheEnabled, normalizeDeviceBackend(m.spec.DeviceBackend)
	}
	if !m.row.MeasuredAt.IsZero() {
		at := m.row.MeasuredAt
		dto.MeasuredAt = &at
	}
	return dto
}

// clusterTotals sums the head's own effective VRAM and RAM and every
// member's, each part max(measured, estimate). A part that is 0 is unknown:
// it adds nothing and makes its total a lower bound (complete false for that
// axis). A CPU member has no VRAM part, so it never touches the VRAM total.
func clusterTotals(dto RuntimeSpecDTO, members []clusterMemberState) (vram, ram int, vramComplete, ramComplete bool) {
	vramComplete, ramComplete = true, true
	add := func(total *int, complete *bool, measured, estimate int) {
		v := max(measured, estimate)
		if v == 0 {
			*complete = false
		}
		*total += v
	}
	for _, g := range dto.GPUs {
		add(&vram, &vramComplete, g.VRAMMeasuredMB, g.VRAMEstimateMB)
	}
	add(&ram, &ramComplete, dto.RAMMeasuredMB, dto.RAMEstimateMB)
	for _, m := range members {
		for _, g := range m.gpuParts() {
			add(&vram, &vramComplete, g.VRAMMeasuredMB, g.VRAMEstimateMB)
		}
		add(&ram, &ramComplete, m.row.RAMMeasuredMB, m.ramEstimate())
	}
	return vram, ram, vramComplete, ramComplete
}

// peaksAboveBudget names every stored member peak above what its host admits:
// a GPU's stored VRAM above the GPU's ceiling (the operator's budget, else the
// card), a member's stored RAM above the host's memory. Such a peak is the
// member's demand, so its next start is refused as not permitted until the
// budget is raised, the split changed, or the stored usage reset. A stale
// value is not named: as demand it is held at that same ceiling. A member the
// reader may not see keeps only its position (visible false), since the
// figures describe its server. Members in member order, then RAM before the
// GPUs in device order.
func (s *Service) peaksAboveBudget(ctx context.Context, members []clusterMemberState) ([]ClusterPeakAboveBudgetDTO, error) {
	var out []ClusterPeakAboveBudgetDTO
	for _, m := range members {
		if !m.found || m.row.Stale {
			continue
		}
		parts := m.gpuParts()
		measured := m.row.RAMMeasuredMB > 0 || slices.ContainsFunc(parts, func(g ClusterMemberGPUDTO) bool { return g.VRAMMeasuredMB > 0 })
		if !measured {
			continue
		}
		gpuCeilings, ramCeiling, err := s.serverMemoryCeilings(ctx, m.server.ID)
		if err != nil {
			return nil, err
		}
		out = append(out, memberPeaksAboveBudget(m, parts, gpuCeilings, ramCeiling)...)
	}
	return out, nil
}

// memberPeaksAboveBudget names one member's stored peaks above its host's
// ceilings, RAM first, then its GPUs in device order; a member the reader
// may not see keeps only its position.
func memberPeaksAboveBudget(m clusterMemberState, parts []ClusterMemberGPUDTO, gpuCeilings map[int]int, ramCeiling int) []ClusterPeakAboveBudgetDTO {
	var found []ClusterPeakAboveBudgetDTO
	if ramCeiling > 0 && m.row.RAMMeasuredMB > ramCeiling {
		found = append(found, ClusterPeakAboveBudgetDTO{
			Position: m.row.Position, Resource: ClusterPeakResourceRAM, StoredMB: m.row.RAMMeasuredMB, CeilingMB: ramCeiling,
		})
	}
	for _, g := range parts {
		if c := gpuCeilings[g.Index]; c > 0 && g.VRAMMeasuredMB > c {
			index := g.Index
			found = append(found, ClusterPeakAboveBudgetDTO{
				Position: m.row.Position, Resource: ClusterPeakResourceVRAM, GPUIndex: &index, StoredMB: g.VRAMMeasuredMB, CeilingMB: c,
			})
		}
	}
	if len(found) > 0 && !m.visible {
		found = []ClusterPeakAboveBudgetDTO{{Position: m.row.Position}}
	}
	return found
}

// clusterWarnings derives the advisory warnings of a cluster head, in a
// fixed order, each code at most once. A member the reader may not see adds
// none: each member warning tells something about the member's server.
func (s *Service) clusterWarnings(ctx context.Context, headServerID string, dto RuntimeSpecDTO, members []clusterMemberState, network, transport string) ([]string, error) {
	out := []string{}
	add := func(code string) {
		if !slices.Contains(out, code) {
			out = append(out, code)
		}
	}
	scan := scanClusterArgs(dto.Args, dto.Env)
	if code := deviceListWarning(scan, len(dto.GPUs) > 0); code != "" {
		add(code)
	}
	head, headListed, err := s.BinaryVersionFor(ctx, headServerID, dto.Binary)
	if err != nil {
		return nil, err
	}
	for _, m := range members {
		if !m.visible {
			continue
		}
		if err := s.memberWarnings(ctx, dto.ID, m, scan, add); err != nil {
			return nil, err
		}
		unknown, err := s.manualMemberVersionUnknown(ctx, m, head, headListed)
		if err != nil {
			return nil, err
		}
		if unknown {
			add(ClusterWarnMemberVersionUnknown)
		}
	}
	relayOpen, err := s.relayMeshWithoutDenyByDefault(ctx, network, transport)
	if err != nil {
		return nil, err
	}
	if relayOpen {
		add(ClusterWarnRelayMeshDenyOff)
	}
	return out, nil
}

// relayMeshWithoutDenyByDefault reports a relay cluster on the mesh while
// NetBird's deny-by-default is not on and enforced: the relay's allowlist is
// then the member's only gate.
func (s *Service) relayMeshWithoutDenyByDefault(ctx context.Context, network, transport string) (bool, error) {
	values, err := s.readSystemSettings(ctx)
	if err != nil {
		return false, err
	}
	return network == clusterNetworkMesh && transport == rpcTransportRelay && !(NetbirdDenyByDefault(values) && NetbirdDenyByDefaultEnforce(values)), nil
}

// deviceListWarning is the head's device list warning, "" for none. The
// output layer lands on the last device and --fit fills back to front, so
// the RPC devices belong first; the projector then defaults to RPC0 unless
// -mmdev names a local device.
func deviceListWarning(scan clusterArgScan, headHasGPUs bool) string {
	switch {
	case !scan.hasDeviceList():
		return ""
	case !scan.deviceListStartsWithRPC(headHasGPUs):
		return ClusterWarnRPCDevicesNotFirst
	case !scan.mmdev:
		return ClusterWarnMMDevMissing
	}
	return ""
}

// manualMemberVersionUnknown reports whether a manual member's binary
// version cannot be compared with the head's: either is unlisted or carries
// no version, or they carry different kinds of identity. An automatic member
// is never reported (its binary matches the head's by construction).
func (s *Service) manualMemberVersionUnknown(ctx context.Context, m clusterMemberState, head routing.ServerBinary, headListed bool) (bool, error) {
	if m.row.MemberSpecID == "" {
		return false, nil
	}
	member, ok, err := s.BinaryVersionFor(ctx, m.server.ID, m.spec.Binary)
	if err != nil {
		return false, err
	}
	return !headListed || !ok || !binaryVersionKnown(head) || !binaryVersionKnown(member) || !binaryVersionsComparable(head, member), nil
}

// memberWarnings adds one member's warnings: a CPU member without
// --tensor-split (its share follows the memory its CPU device reports), a
// member on a host with other work and no estimate before its first
// measurement, an operator budget below a member GPU's total without
// --tensor-split (the split ignores budgets), and GPUs another spec or
// member on that server also uses.
func (s *Service) memberWarnings(ctx context.Context, headSpecID string, m clusterMemberState, scan clusterArgScan, add func(string)) error {
	parts := m.gpuParts()
	if len(parts) == 0 && !scan.tensorSplit {
		add(ClusterWarnCPUMemberWithoutTensorSplit)
	}
	others, err := s.otherGPUUses(ctx, headSpecID, m)
	if err != nil {
		return err
	}
	if others.shared && m.row.MeasuredAt.IsZero() && memberEstimateMissing(m, parts) {
		add(ClusterWarnSharedHostWithoutEstimate)
	}
	if slices.ContainsFunc(parts, func(g ClusterMemberGPUDTO) bool { return others.gpus[g.Index] }) {
		add(ClusterWarnGPUOverlap)
	}
	if scan.tensorSplit || len(parts) == 0 {
		return nil
	}
	below, err := s.budgetBelowDevice(ctx, m.server.ID, parts)
	if err != nil {
		return err
	}
	if below {
		add(ClusterWarnBudgetBelowDevice)
	}
	return nil
}

// memberEstimateMissing reports whether a member lacks an estimate: a CPU
// member without a RAM estimate, or a GPU without a VRAM estimate.
func memberEstimateMissing(m clusterMemberState, parts []ClusterMemberGPUDTO) bool {
	if len(parts) == 0 {
		return m.ramEstimate() == 0
	}
	return slices.ContainsFunc(parts, func(g ClusterMemberGPUDTO) bool { return g.VRAMEstimateMB == 0 })
}

// budgetBelowDevice reports whether an operator GPU budget on serverID is
// below the total memory of one of the member's GPUs.
func (s *Service) budgetBelowDevice(ctx context.Context, serverID string, parts []ClusterMemberGPUDTO) (bool, error) {
	budgets, err := s.routes.ServerGPUBudgets(ctx, serverID)
	if err != nil {
		return false, err
	}
	hosts, err := s.serverHostGPUs(ctx, serverID)
	if err != nil {
		return false, err
	}
	for _, g := range parts {
		if slices.ContainsFunc(budgets, func(b routing.ServerGPUBudget) bool {
			return b.GPUIndex == g.Index && b.BudgetMB > 0 && hosts[g.Index].totalMB > b.BudgetMB
		}) {
			return true, nil
		}
	}
	return false, nil
}

// gpuUses is what else runs on a member's server: whether anything does
// (shared) and which GPU indices it uses.
type gpuUses struct {
	shared bool
	gpus   map[int]bool
}

// otherGPUUses collects the other work on m's server: the runtime specs of
// its server_agent applications, the members other heads have there, and
// the other manual member specs on it.
func (s *Service) otherGPUUses(ctx context.Context, headSpecID string, m clusterMemberState) (gpuUses, error) {
	uses := gpuUses{gpus: map[int]bool{}}
	if err := s.addRuntimeSpecGPUUses(ctx, m.server.ID, &uses); err != nil {
		return uses, err
	}
	if err := s.addOtherMembershipGPUUses(ctx, headSpecID, m.server.ID, &uses); err != nil {
		return uses, err
	}
	if err := s.addOtherMemberSpecGPUUses(ctx, m, &uses); err != nil {
		return uses, err
	}
	return uses, nil
}

// addRuntimeSpecGPUUses adds the runtime specs of serverID's server_agent
// applications.
func (s *Service) addRuntimeSpecGPUUses(ctx context.Context, serverID string, uses *gpuUses) error {
	apps, err := s.routes.ApplicationsByServer(ctx, serverID)
	if err != nil {
		return err
	}
	for _, app := range apps {
		if app.Type != routing.ProviderServerAgent {
			continue
		}
		specs, err := s.routes.RuntimeSpecsByApplication(ctx, app.ID)
		if err != nil {
			return err
		}
		for _, spec := range specs {
			uses.shared = true
			rows, err := s.routes.RuntimeSpecGPUs(ctx, spec.ID)
			if err != nil {
				return err
			}
			for _, g := range rows {
				uses.gpus[g.GPUIndex] = true
			}
		}
	}
	return nil
}

// addOtherMembershipGPUUses adds the members heads other than headSpecID
// have on serverID.
func (s *Service) addOtherMembershipGPUUses(ctx context.Context, headSpecID, serverID string, uses *gpuUses) error {
	memberships, err := s.routes.ClusterMembershipsByServer(ctx, serverID)
	if err != nil {
		return err
	}
	for _, other := range memberships {
		if other.HeadSpecID == headSpecID {
			continue
		}
		uses.shared = true
		_, gpus, err := s.routes.ClusterMembers(ctx, other.HeadSpecID)
		if err != nil {
			return err
		}
		for _, g := range gpus {
			if g.ServerID == serverID {
				uses.gpus[g.GPUIndex] = true
			}
		}
	}
	return nil
}

// addOtherMemberSpecGPUUses adds the enabled manual member specs on m's
// server other than m's own.
func (s *Service) addOtherMemberSpecGPUUses(ctx context.Context, m clusterMemberState, uses *gpuUses) error {
	specs, err := s.routes.RPCMemberSpecsByServer(ctx, m.server.ID)
	if err != nil {
		return err
	}
	for _, spec := range specs {
		if spec.ID == m.row.MemberSpecID || !spec.Enabled {
			continue
		}
		uses.shared = true
		rows, err := s.routes.RPCMemberSpecGPUs(ctx, spec.ID)
		if err != nil {
			return err
		}
		for _, g := range rows {
			uses.gpus[g.GPUIndex] = true
		}
	}
	return nil
}

// ClusterCandidates lists the servers the head on headMappingID may add as
// members in mode ("automatic" when empty): servers other than the head's
// that the caller manages and whose consent admits the head (for every
// caller, system scope included; an empty consent list means the member
// server's own admin groups). In automatic mode a server must offer RPC
// membership; it is Compatible when it has a ggml-rpc-server matching the
// head binary's version. In manual mode it must have enabled member specs
// that serve no other head, which are listed with the ones whose binary's
// version differs from the head binary's. headBinary is the head binary to
// match against, as the operator's form has it; empty means the stored
// spec's. Its version is read from the head server's inventory exactly as the
// save checks read it, so the list and the save agree. Sorted by server name.
func (s *Service) ClusterCandidates(ctx context.Context, p auth.Token, headMappingID, mode, headBinary string) (ClusterCandidateList, error) {
	mapping, _, headServer, err := s.authorizeMapping(ctx, p, headMappingID)
	if err != nil {
		return ClusterCandidateList{}, err
	}
	if mode, err = candidateMode(mode); err != nil {
		return ClusterCandidateList{}, err
	}
	headSpec, configured, err := s.routes.RuntimeSpecByMapping(ctx, mapping.ID)
	if err != nil {
		return ClusterCandidateList{}, err
	}
	binary := strings.TrimSpace(headBinary)
	if binary == "" && configured {
		binary = headSpec.Binary
	}
	return s.listClusterCandidates(ctx, p, headServer.ID, headSpec.ID, mode, binary)
}

// ApplicationClusterCandidates is ClusterCandidates for a head that is not
// stored yet: a new runtime spec on appID, whose first write stores the spec
// and its members together. It needs the application's rights, as creating
// the spec does; the head binary is headBinary alone (no stored spec), and
// no member spec is this head's own.
func (s *Service) ApplicationClusterCandidates(ctx context.Context, p auth.Token, appID, mode, headBinary string) (ClusterCandidateList, error) {
	_, headServer, err := s.authorizeApplication(ctx, p, appID)
	if err != nil {
		return ClusterCandidateList{}, err
	}
	if mode, err = candidateMode(mode); err != nil {
		return ClusterCandidateList{}, err
	}
	return s.listClusterCandidates(ctx, p, headServer.ID, "", mode, strings.TrimSpace(headBinary))
}

// candidateMode normalizes a listing's member mode; "" is automatic.
func candidateMode(mode string) (string, error) {
	mode = normalizeMemberMode(strings.TrimSpace(mode))
	if mode != memberModeAutomatic && mode != memberModeManual {
		return "", ErrClusterMemberModeInvalid
	}
	return mode, nil
}

// listClusterCandidates is both listings' shared body: the head on
// headServerID with the stored spec headSpecID ("" for a new one) and the
// head binary binary ("" when there is none to match on).
func (s *Service) listClusterCandidates(ctx context.Context, p auth.Token, headServerID, headSpecID, mode, binary string) (ClusterCandidateList, error) {
	head, err := s.candidateHeadFor(ctx, headServerID, headSpecID, binary)
	if err != nil {
		return ClusterCandidateList{}, err
	}
	out, err := s.clusterCandidatesFor(ctx, p, headServerID, mode, head)
	if err != nil {
		return ClusterCandidateList{}, err
	}
	return ClusterCandidateList{Candidates: out, HeadBinaryKnown: head.known}, nil
}

// clusterCandidatesFor builds the candidate entries of every server other
// than the head's that the caller manages, sorted by server name.
func (s *Service) clusterCandidatesFor(ctx context.Context, p auth.Token, headServerID, mode string, head candidateHead) ([]ClusterCandidateDTO, error) {
	servers, err := s.routes.AIServers(ctx)
	if err != nil {
		return nil, err
	}
	out := []ClusterCandidateDTO{}
	for _, srv := range servers {
		if srv.ID == headServerID || !s.callerManagesServer(ctx, p, srv.ID) {
			continue
		}
		c, ok, err := s.clusterCandidate(ctx, p, srv, mode, head)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, c)
		}
	}
	slices.SortFunc(out, func(a, b ClusterCandidateDTO) int {
		if a.ServerName != b.ServerName {
			return strings.Compare(a.ServerName, b.ServerName)
		}
		return strings.Compare(a.ServerID, b.ServerID)
	})
	return out, nil
}

// candidateHead is what ClusterCandidates reads of the head: its stored
// spec's id, its binary's inventory row and whether that carries a version,
// and its server's admin groups.
type candidateHead struct {
	specID string
	binary routing.ServerBinary
	known  bool
	groups []string
}

// candidateHeadFor reads what the listing needs of the head: its binary's
// inventory row on headServerID (none when binary is "") and its server's
// admin groups.
func (s *Service) candidateHeadFor(ctx context.Context, headServerID, headSpecID, binary string) (candidateHead, error) {
	h := candidateHead{specID: headSpecID}
	if binary != "" {
		var listed bool
		var err error
		if h.binary, listed, err = s.BinaryVersionFor(ctx, headServerID, binary); err != nil {
			return h, err
		}
		h.known = listed && binaryVersionKnown(h.binary)
	}
	var err error
	h.groups, err = s.routes.ServerAdminGroups(ctx, headServerID)
	return h, err
}

// clusterCandidate builds srv's candidate entry in mode; ok is false when
// srv cannot serve the head as a member.
func (s *Service) clusterCandidate(ctx context.Context, p auth.Token, srv routing.AIServer, mode string, head candidateHead) (ClusterCandidateDTO, bool, error) {
	c := ClusterCandidateDTO{ServerID: srv.ID, ServerName: srv.Name}
	if mode != memberModeAutomatic {
		return s.manualCandidate(ctx, p, c, head)
	}
	if !srv.RPCMemberEnabled {
		return c, false, nil
	}
	admits, err := s.memberConsentAdmits(ctx, srv.RPCAllowedHeadGroups, srv.ID, head.groups)
	if err != nil || !admits {
		return c, false, err
	}
	if !head.known {
		return c, true, nil
	}
	match, ok, err := s.MatchingRPCBinary(ctx, srv.ID, head.binary)
	if err != nil {
		return c, false, err
	}
	if ok {
		c.RPCBinary, c.RPCBinaryVersion, c.Compatible = match.Path, match.EffectiveVersion(), true
	}
	return c, true, nil
}

// manualCandidate fills c's free member specs and marks the ones whose
// binary the save would refuse for its version; ok is false without any.
func (s *Service) manualCandidate(ctx context.Context, p auth.Token, c ClusterCandidateDTO, head candidateHead) (ClusterCandidateDTO, bool, error) {
	specs, err := s.freeMemberSpecs(ctx, p, c.ServerID, head.specID, head.groups)
	if err != nil {
		return c, false, err
	}
	c.MemberSpecs = specs
	if head.known {
		if c.IncompatibleMemberSpecs, err = s.mismatchedMemberSpecs(ctx, c.ServerID, specs, head.binary); err != nil {
			return c, false, err
		}
	}
	c.Compatible = len(specs) > len(c.IncompatibleMemberSpecs)
	return c, len(specs) > 0, nil
}

// freeMemberSpecs lists serverID's enabled member specs that admit the head
// and serve no head other than headSpecID.
func (s *Service) freeMemberSpecs(ctx context.Context, p auth.Token, serverID, headSpecID string, headGroups []string) ([]RPCMemberSpecDTO, error) {
	specs, err := s.routes.RPCMemberSpecsByServer(ctx, serverID)
	if err != nil {
		return nil, err
	}
	out := []RPCMemberSpecDTO{}
	for _, spec := range specs {
		if !spec.Enabled {
			continue
		}
		admits, err := s.memberConsentAdmits(ctx, spec.AllowedHeadGroups, serverID, headGroups)
		if err != nil {
			return nil, err
		}
		if !admits {
			continue
		}
		used, ok, err := s.routes.ClusterMemberBySpec(ctx, spec.ID)
		if err != nil {
			return nil, err
		}
		if ok && used.HeadSpecID != headSpecID {
			continue
		}
		dto, err := s.rpcMemberSpecDTO(ctx, p, spec)
		if err != nil {
			return nil, err
		}
		out = append(out, dto)
	}
	return out, nil
}
