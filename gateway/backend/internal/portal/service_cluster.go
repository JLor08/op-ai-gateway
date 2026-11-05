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

// RPC port range a member server's member processes are allocated from.
const (
	rpcPortFirst = 47000
	rpcPortLast  = 47999
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
//  1. cluster off: ${RPC_SERVERS}/${RPC_DEVICES} anywhere in args or env, then
//     the caller's rights on every former member server (dropping the
//     membership changes their documents); nothing else.
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
//  4. rights: the caller manages every member server, former ones included,
//     and every member consents to the head (its member spec's
//     allowed_head_groups in manual mode, the server's rpc_allowed_head_groups
//     in automatic mode, intersected with the head server's admin groups; an
//     empty list means the member server's own admin groups; system scope
//     needs no consent). Both answer server-not-found, so a
//     caller learns nothing about a server it may not use. They run before
//     the member checks below, which would otherwise tell such a caller
//     about that server's settings.
//  5. manual specs: exists on that server, enabled, not used by another head.
//  6. automatic members: RPC membership enabled on the server, a
//     ggml-rpc-server whose version matches the head binary's.
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
	var prevMembers []routing.ClusterMember
	var prevGPUs []routing.ClusterMemberGPU
	if prior.ok {
		var err error
		if prevMembers, prevGPUs, err = s.routes.ClusterMembers(ctx, prior.spec.ID); err != nil {
			return clusterPlan{}, err
		}
	}
	for _, m := range prevMembers {
		plan.memberServers = appendUnique(plan.memberServers, m.ServerID)
	}
	scan := scanClusterArgs(req.Args, req.Env)
	if !req.ClusterEnabled {
		if scan.clusterPlaceholder {
			return clusterPlan{}, ErrClusterRPCPlaceholderWithoutCluster
		}
		if err := s.authorizeFormerMembers(ctx, p, prevMembers, nil); err != nil {
			return clusterPlan{}, err
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
	if prior.ok && prior.spec.ClusterEnabled && normalizeMemberMode(prior.spec.MemberMode) == f.memberMode &&
		prior.spec.ClusterNetwork == f.clusterNetwork && prior.spec.RPCTransport == f.rpcTransport &&
		prior.spec.Binary == f.binary &&
		membershipUnchanged(entries, prevMembers, prevGPUs) {
		if err := s.loadStoredClusterEntries(ctx, p, entries); err != nil {
			return clusterPlan{}, err
		}
		// The acknowledgement is the head's own setting: withdrawing it from a
		// direct lan cluster is refused like a missing one.
		if plan.transport == rpcTransportDirect && plan.network == clusterNetworkLAN && !req.LANExposureAck {
			return clusterPlan{}, ErrClusterLANAckRequired
		}
	} else if err := s.checkClusterMembership(ctx, p, headServer, f, req, prior, entries, prevMembers, &plan); err != nil {
		return clusterPlan{}, err
	}
	if err := s.checkClusterMemberGPUs(ctx, entries, scan); err != nil {
		return clusterPlan{}, err
	}
	if err := s.planClusterRows(ctx, &plan, entries, prior, f, req, prevMembers, prevGPUs); err != nil {
		return clusterPlan{}, err
	}
	return plan, nil
}

// validateClusterHeadDocument is step 2 of validateClusterHead: what the
// head's own document must say, read from the request alone.
func validateClusterHeadDocument(req PutRuntimeSpecRequest, f runtimeSpecFields, scan clusterArgScan) error {
	if routing.EffectiveRuntimeSpecType(routing.RuntimeSpec{Type: f.specType, Binary: f.binary}) != routing.RuntimeSpecTypeLlamaCpp {
		return ErrClusterTypeInvalid
	}
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
		e := clusterEntry{manual: mode == memberModeManual, req: m, estimate: map[int]int{}}
		if mode == memberModeAutomatic {
			if m.Threads < 0 || m.RAMEstimateMB < 0 {
				return nil, ErrRuntimeSpecTuningInvalid
			}
			gpus := make([]RuntimeSpecGPUDTO, 0, len(m.GPUs))
			for _, g := range m.GPUs {
				gpus = append(gpus, RuntimeSpecGPUDTO{Index: g.Index, VRAMEstimateMB: g.VRAMEstimateMB})
				e.gpus = append(e.gpus, g.Index)
				e.estimate[g.Index] = g.VRAMEstimateMB
			}
			if err := validateRuntimeSpecGPUs(gpus); err != nil {
				return nil, err
			}
			e.req.DeviceBackend = normalizeDeviceBackend(strings.TrimSpace(m.DeviceBackend))
			if !slices.Contains(rpcMemberDeviceBackends, e.req.DeviceBackend) {
				return nil, ErrRPCMemberDeviceBackendInvalid
			}
			e.req.MemberSpecID = ""
		} else {
			e.req.Threads, e.req.CacheEnabled, e.req.DeviceBackend, e.req.RAMEstimateMB, e.req.GPUs = 0, false, deviceBackendAuto, 0, nil
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
		if e.manual {
			continue
		}
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
	}
	return true
}

// loadStoredClusterEntries fills the entries of an unchanged membership
// without authorizing them: each entry's server, its manual spec and whether
// the caller may see the server.
func (s *Service) loadStoredClusterEntries(ctx context.Context, p auth.Token, entries []clusterEntry) error {
	for i := range entries {
		e := &entries[i]
		server, err := s.routes.AIServerByID(ctx, e.req.ServerID)
		if err != nil {
			return ErrServerNotFound
		}
		e.server, e.visible = server, s.callerManagesServer(ctx, p, server.ID)
		if e.manual {
			spec, ok, err := s.routes.RPCMemberSpecByID(ctx, e.req.MemberSpecID)
			if err != nil {
				return err
			}
			if ok {
				if err := s.loadEntrySpec(ctx, e, spec); err != nil {
					return err
				}
			}
		}
	}
	return nil
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
func (s *Service) checkClusterMembership(ctx context.Context, p auth.Token, headServer routing.AIServer, f runtimeSpecFields, req PutRuntimeSpecRequest, prior runtimeSpecPrior, entries []clusterEntry, prevMembers []routing.ClusterMember, plan *clusterPlan) error {
	if err := s.authorizeClusterMembers(ctx, p, headServer.ID, entries, prevMembers); err != nil {
		return err
	}
	headSpecID := ""
	if prior.ok {
		headSpecID = prior.spec.ID
	}
	if err := s.checkClusterMemberSpecs(ctx, entries, headSpecID); err != nil {
		return err
	}
	if err := s.checkClusterMemberBinaries(ctx, headServer.ID, f.binary, entries); err != nil {
		return err
	}
	var err error
	plan.network, plan.transport, err = s.checkClusterNetwork(ctx, headServer, entries, plan.network, plan.transport, req.LANExposureAck)
	return err
}

// authorizeClusterMembers is step 4 of validateClusterHead. It fills each
// entry's server and, in manual mode, its member spec.
func (s *Service) authorizeClusterMembers(ctx context.Context, p auth.Token, headServerID string, entries []clusterEntry, prevMembers []routing.ClusterMember) error {
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
	if err := s.authorizeFormerMembers(ctx, p, prevMembers, entries); err != nil {
		return err
	}
	headGroups, err := s.routes.ServerAdminGroups(ctx, headServerID)
	if err != nil {
		return err
	}
	for i := range entries {
		e := &entries[i]
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
		admits, err := s.memberConsentAdmits(ctx, p, consentRaw, e.server.ID, headGroups)
		if err != nil {
			return err
		}
		if !admits {
			return ErrServerNotFound
		}
	}
	return nil
}

// memberConsentAdmits reports whether a member's stored consent list admits
// a head whose server is linked to headGroups. An empty list means the
// member server's own admin groups (the default consent).
func (s *Service) memberConsentAdmits(ctx context.Context, p auth.Token, consentRaw, memberServerID string, headGroups []string) (bool, error) {
	if isSystem(p) {
		return true, nil
	}
	consent := decodeRPCHeadGroups(consentRaw)
	if len(consent) == 0 {
		var err error
		if consent, err = s.routes.ServerAdminGroups(ctx, memberServerID); err != nil {
			return false, err
		}
	}
	return consentAdmits(p, consent, headGroups), nil
}

// authorizeFormerMembers requires the caller's rights on every former member
// server the write drops: removing a member changes that server's document
// as much as adding one does.
func (s *Service) authorizeFormerMembers(ctx context.Context, p auth.Token, prevMembers []routing.ClusterMember, entries []clusterEntry) error {
	for _, m := range prevMembers {
		kept := slices.ContainsFunc(entries, func(e clusterEntry) bool { return e.req.ServerID == m.ServerID })
		if kept {
			continue
		}
		if _, err := s.authorizeServer(ctx, p, m.ServerID); err != nil {
			return err
		}
	}
	return nil
}

// consentAdmits reports whether a member's consent groups admit a head whose
// server is linked to headGroups. System scope needs no consent.
func consentAdmits(p auth.Token, consent, headGroups []string) bool {
	if isSystem(p) {
		return true
	}
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
// known and differ.
func (s *Service) checkClusterMemberBinaries(ctx context.Context, headServerID, headBinary string, entries []clusterEntry) error {
	head, headListed, err := s.BinaryVersionFor(ctx, headServerID, headBinary)
	if err != nil {
		return err
	}
	headKnown := headListed && binaryVersionKnown(head)
	for _, e := range entries {
		if e.manual || e.server.RPCMemberEnabled {
			continue
		}
		return fmt.Errorf("%w: server %q does not offer RPC membership in its runtime settings", ErrClusterMemberRPCDisabled, e.server.Name)
	}
	for _, e := range entries {
		if e.manual {
			continue
		}
		if !headKnown {
			return fmt.Errorf("%w: the version of the head binary %s is unknown; add it in the Binaries tab", ErrClusterMemberNoMatchingBinary, headBinary)
		}
		_, ok, err := s.MatchingRPCBinary(ctx, e.server.ID, head)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("%w: server %q has no ggml-rpc-server of version %s", ErrClusterMemberNoMatchingBinary, e.server.Name, head.EffectiveVersion())
		}
	}
	for _, e := range entries {
		if !e.manual || !headKnown {
			continue
		}
		member, ok, err := s.BinaryVersionFor(ctx, e.server.ID, e.spec.Binary)
		if err != nil {
			return err
		}
		if ok && binaryVersionKnown(member) && binaryVersionsComparable(head, member) && !binariesSameVersion(head, member) {
			return fmt.Errorf("%w: member binary %s on server %q is version %s, the head's is %s",
				ErrClusterVersionMismatch, e.spec.Binary, e.server.Name, member.EffectiveVersion(), head.EffectiveVersion())
		}
	}
	return nil
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
// port.
func (s *Service) directMeshAvailable(ctx context.Context) bool {
	values := s.systemSettingValues(ctx)
	return NetbirdManagePolicies(values) && NetbirdDenyByDefault(values) && NetbirdDenyByDefaultEnforce(values) &&
		s.netbirdDefaultPolicyDisabled(ctx)
}

// defaultRPCTransport: in mesh, direct where its NetBird preconditions hold,
// else relay; in lan, direct.
func (s *Service) defaultRPCTransport(ctx context.Context, network string) string {
	if network == clusterNetworkMesh && !s.directMeshAvailable(ctx) {
		return rpcTransportRelay
	}
	return rpcTransportDirect
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
		if network == clusterNetworkMesh && !netbirdMeshReady(srv) {
			return "", "", fmt.Errorf("%w: server %q needs NetBird with a tracking group and a known mesh IP", ErrClusterMeshUnavailable, srv.Name)
		}
		if network == clusterNetworkLAN && !lanAddressKnown(srv) {
			return "", "", fmt.Errorf("%w: server %q needs a LAN address", ErrClusterLANAddressRequired, srv.Name)
		}
	}
	if transport == "" {
		transport = s.defaultRPCTransport(ctx, network)
	}
	if transport == rpcTransportDirect && network == clusterNetworkMesh && !s.directMeshAvailable(ctx) {
		return "", "", ErrClusterDirectUnavailable
	}
	if transport == rpcTransportDirect && network == clusterNetworkLAN && !lanAck {
		return "", "", ErrClusterLANAckRequired
	}
	return network, transport, nil
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
		if !slices.ContainsFunc(e.gpus, func(idx int) bool { return hosts[i][idx].apple }) {
			continue
		}
		budgets, err := s.routes.ServerGPUBudgets(ctx, e.server.ID)
		if err != nil {
			return err
		}
		for _, idx := range e.gpus {
			if !hosts[i][idx].apple {
				continue
			}
			if !slices.ContainsFunc(budgets, func(b routing.ServerGPUBudget) bool { return b.GPUIndex == idx && b.BudgetMB > 0 }) {
				return e.refusal(ErrClusterMemberBudgetRequired, "GPU %d of server %q needs a GPU budget", idx, e.server.Name)
			}
		}
	}
	nonNVIDIA := func(i, idx int) bool {
		g, known := hosts[i][idx]
		return known && !g.nvidia
	}
	for i, e := range entries {
		for _, idx := range e.gpus {
			if !e.manual && nonNVIDIA(i, idx) && e.estimate[idx] == 0 {
				return ErrRPCMemberVRAMEstimateRequired
			}
		}
	}
	for i, e := range entries {
		if !scan.tensorSplit && slices.ContainsFunc(e.gpus, func(idx int) bool { return nonNVIDIA(i, idx) }) {
			return e.refusal(ErrClusterTensorSplitRequired, "server %q has non-NVIDIA GPUs", e.server.Name)
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
func (s *Service) planClusterRows(ctx context.Context, plan *clusterPlan, entries []clusterEntry, prior runtimeSpecPrior, f runtimeSpecFields, req PutRuntimeSpecRequest, prevMembers []routing.ClusterMember, prevGPUs []routing.ClusterMemberGPU) error {
	prevByServer := map[string]routing.ClusterMember{}
	for _, m := range prevMembers {
		prevByServer[m.ServerID] = m
	}
	prevGPUOrder := map[string][]int{}
	prevGPUByKey := map[string]map[int]routing.ClusterMemberGPU{}
	for _, g := range prevGPUs {
		prevGPUOrder[g.ServerID] = append(prevGPUOrder[g.ServerID], g.GPUIndex)
		if prevGPUByKey[g.ServerID] == nil {
			prevGPUByKey[g.ServerID] = map[int]routing.ClusterMemberGPU{}
		}
		prevGPUByKey[g.ServerID][g.GPUIndex] = g
	}
	changed, err := s.clusterDistributionChanged(ctx, entries, prior, f, req, prevMembers, prevGPUOrder)
	if err != nil {
		return err
	}
	plan.writeMembers = true
	for i, e := range entries {
		row := routing.ClusterMember{
			Position: i, ServerID: e.server.ID, MemberSpecID: e.specID(),
			Threads: e.req.Threads, CacheEnabled: e.req.CacheEnabled, DeviceBackend: e.req.DeviceBackend, RAMEstimateMB: e.req.RAMEstimateMB,
		}
		prev, hadRow := prevByServer[e.server.ID]
		kept := hadRow && prev.MemberSpecID == e.specID()
		if hadRow && prev.Port != 0 {
			row.Port = prev.Port
		} else {
			if row.Port, err = s.allocateRPCPort(ctx, e.server.ID); err != nil {
				return err
			}
			plan.allocated = append(plan.allocated, i)
		}
		measured := false
		if kept {
			row.RAMMeasuredMB, row.MeasuredAt, row.Stale = prev.RAMMeasuredMB, prev.MeasuredAt, prev.Stale
			row.RunPeakRAMMB, row.RunHeadInstance, row.RunStartedAt = prev.RunPeakRAMMB, prev.RunHeadInstance, prev.RunStartedAt
			measured = prev.RAMMeasuredMB > 0
		}
		for pos, idx := range e.gpus {
			g := routing.ClusterMemberGPU{ServerID: e.server.ID, GPUIndex: idx, Position: pos}
			if !e.manual {
				// A manual member's estimates live on its spec.
				g.VRAMEstimateMB = e.estimate[idx]
			}
			if prevGPU, ok := prevGPUByKey[e.server.ID][idx]; kept && ok {
				g.VRAMMeasuredMB, g.RunPeakVRAMMB = prevGPU.VRAMMeasuredMB, prevGPU.RunPeakVRAMMB
				measured = measured || prevGPU.VRAMMeasuredMB > 0
			}
			plan.gpus = append(plan.gpus, g)
		}
		if changed && measured {
			row.Stale = true
		}
		plan.members = append(plan.members, row)
		plan.memberServers = appendUnique(plan.memberServers, e.server.ID)
	}
	return nil
}

// clusterDistributionChanged reports whether a head write changes how the
// model is split over the devices (see planClusterRows).
func (s *Service) clusterDistributionChanged(ctx context.Context, entries []clusterEntry, prior runtimeSpecPrior, f runtimeSpecFields, req PutRuntimeSpecRequest, prevMembers []routing.ClusterMember, prevGPUOrder map[string][]int) (bool, error) {
	if len(prevMembers) != len(entries) {
		return true, nil
	}
	for i, e := range entries {
		if prevMembers[i].ServerID != e.server.ID || prevMembers[i].MemberSpecID != e.specID() || !slices.Equal(prevGPUOrder[e.server.ID], e.gpus) {
			return true, nil
		}
	}
	if !prior.ok {
		return false, nil
	}
	if prior.spec.Args != f.args || prior.spec.Env != f.env {
		return true, nil
	}
	headGPUs, err := s.routes.RuntimeSpecGPUs(ctx, prior.spec.ID)
	if err != nil {
		return false, err
	}
	if len(headGPUs) != len(req.GPUs) {
		return true, nil
	}
	for i, g := range headGPUs {
		if g.GPUIndex != req.GPUs[i].Index {
			return true, nil
		}
	}
	return false, nil
}

func appendUnique(ids []string, id string) []string {
	if slices.Contains(ids, id) {
		return ids
	}
	return append(ids, id)
}

// writeClusterMembers stores plan's membership rows for headSpecID. The
// store refuses with a conflict when, since the checks ran, another head
// took one of the manual member specs (one head per spec) or another gateway
// process allocated one of the new ports on the same server (one member per
// port and server; the allocation lock is per process). The first is
// ErrClusterMemberSpecTaken. For the second the new ports are allocated once
// more from the store's current rows and the write is repeated; a second
// conflict is ErrClusterPortExhausted.
func (s *Service) writeClusterMembers(ctx context.Context, headSpecID string, plan *clusterPlan) error {
	err := s.routes.SetClusterMembers(ctx, headSpecID, plan.members, plan.gpus)
	if !errors.Is(err, store.ErrConflict) {
		return clusterMembersStoreError(err)
	}
	for _, m := range plan.members {
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
	if len(plan.allocated) == 0 {
		return ErrClusterPortExhausted
	}
	for _, i := range plan.allocated {
		if plan.members[i].Port, err = s.allocateRPCPort(ctx, plan.members[i].ServerID); err != nil {
			return err
		}
	}
	err = s.routes.SetClusterMembers(ctx, headSpecID, plan.members, plan.gpus)
	if errors.Is(err, store.ErrConflict) {
		return ErrClusterPortExhausted
	}
	return clusterMembersStoreError(err)
}

// clusterMembersStoreError maps a membership write the store refused for a
// reason other than a conflict: a server deleted in the meantime.
func clusterMembersStoreError(err error) error {
	if errors.Is(err, store.ErrNotFound) {
		return ErrServerNotFound
	}
	return err
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
// read) reads every switch as off.
func (s *Service) systemSettingValues(ctx context.Context) map[string]string {
	if s.settings == nil {
		return nil
	}
	values, err := s.settings.SystemSettings(ctx)
	if err != nil {
		return nil
	}
	return values
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

// netbirdDefaultPolicyDisabled reports whether the last NetBird policy pass
// saw the Default policy disabled. Unknown -- never observed, unreadable, or
// no settings store -- is not disabled, so a direct mesh cluster is refused
// until a pass has seen it.
func (s *Service) netbirdDefaultPolicyDisabled(ctx context.Context) bool {
	return parseNetbirdDefaultPolicyState(s.systemSettingValues(ctx)[netbirdDefaultPolicyStateKey]).State == NetbirdDefaultPolicyDisabled
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
		st := clusterMemberState{row: row}
		for _, g := range gpus {
			if g.ServerID == row.ServerID {
				st.gpuRows = append(st.gpuRows, g)
			}
		}
		if srv, err := s.routes.AIServerByID(ctx, row.ServerID); err == nil {
			st.server, st.found = srv, true
		}
		if row.MemberSpecID != "" {
			spec, ok, err := s.routes.RPCMemberSpecByID(ctx, row.MemberSpecID)
			if err != nil {
				return nil, err
			}
			if ok {
				st.spec = spec
				if st.specGPUs, err = s.routes.RPCMemberSpecGPUs(ctx, spec.ID); err != nil {
					return nil, err
				}
			}
		}
		out = append(out, st)
	}
	return out, nil
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
	dto.ClusterMembers = make([]ClusterMemberDTO, 0, len(members))
	allVisible := true
	for i, m := range members {
		members[i].visible = m.found && (isSystem(p) || s.callerManagesServer(ctx, p, m.server.ID))
		allVisible = allVisible && members[i].visible
		dto.ClusterMembers = append(dto.ClusterMembers, clusterMemberDTO(m, members[i].visible))
	}
	dto.ClusterVRAMTotalMB, dto.ClusterRAMTotalMB, dto.ClusterTotalsComplete = clusterTotals(dto, members)
	// The network default turns on every member server's NetBird readiness,
	// so it would tell a caller something about a server it may not see:
	// with a hidden member it stays empty (and the transport default with it
	// unless the network is stored).
	network := dto.ClusterNetwork
	if allVisible {
		servers := []routing.AIServer{headServer}
		for _, m := range members {
			servers = append(servers, m.server)
		}
		dto.ClusterNetworkDefault = defaultClusterNetwork(servers)
		if network == "" {
			network = dto.ClusterNetworkDefault
		}
	}
	if network != "" {
		dto.ClusterTransportDefault = s.defaultRPCTransport(ctx, network)
	}
	if dto.ClusterEnabled {
		transport := dto.RPCTransport
		if transport == "" {
			transport = dto.ClusterTransportDefault
		}
		if dto.ClusterWarnings, err = s.clusterWarnings(ctx, headServer.ID, dto, members, network, transport); err != nil {
			return dto, err
		}
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
// it adds nothing and makes the totals lower bounds (complete false). A CPU
// member has no VRAM part.
func clusterTotals(dto RuntimeSpecDTO, members []clusterMemberState) (vram, ram int, complete bool) {
	complete = true
	add := func(total *int, measured, estimate int) {
		v := max(measured, estimate)
		if v == 0 {
			complete = false
		}
		*total += v
	}
	for _, g := range dto.GPUs {
		add(&vram, g.VRAMMeasuredMB, g.VRAMEstimateMB)
	}
	add(&ram, dto.RAMMeasuredMB, dto.RAMEstimateMB)
	for _, m := range members {
		for _, g := range m.gpuParts() {
			add(&vram, g.VRAMMeasuredMB, g.VRAMEstimateMB)
		}
		add(&ram, m.row.RAMMeasuredMB, m.ramEstimate())
	}
	return vram, ram, complete
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
	// The output layer lands on the last device and --fit fills back to
	// front, so the RPC devices belong first; the projector then defaults to
	// RPC0 unless -mmdev names a local device.
	if scan.hasDeviceList() {
		if !scan.deviceListStartsWithRPC(len(dto.GPUs) > 0) {
			add(ClusterWarnRPCDevicesNotFirst)
		} else if !scan.mmdev {
			add(ClusterWarnMMDevMissing)
		}
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
		if m.row.MemberSpecID != "" {
			member, ok, err := s.BinaryVersionFor(ctx, m.server.ID, m.spec.Binary)
			if err != nil {
				return nil, err
			}
			if !headListed || !ok || !binaryVersionKnown(head) || !binaryVersionKnown(member) || !binaryVersionsComparable(head, member) {
				add(ClusterWarnMemberVersionUnknown)
			}
		}
	}
	values := s.systemSettingValues(ctx)
	if network == clusterNetworkMesh && transport == rpcTransportRelay && !(NetbirdDenyByDefault(values) && NetbirdDenyByDefaultEnforce(values)) {
		add(ClusterWarnRelayMeshDenyOff)
	}
	return out, nil
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
	if others.shared && m.row.MeasuredAt.IsZero() {
		missing := len(parts) == 0 && m.ramEstimate() == 0
		for _, g := range parts {
			missing = missing || g.VRAMEstimateMB == 0
		}
		if missing {
			add(ClusterWarnSharedHostWithoutEstimate)
		}
	}
	for _, g := range parts {
		if others.gpus[g.Index] {
			add(ClusterWarnGPUOverlap)
		}
	}
	if scan.tensorSplit || len(parts) == 0 {
		return nil
	}
	budgets, err := s.routes.ServerGPUBudgets(ctx, m.server.ID)
	if err != nil {
		return err
	}
	hosts, err := s.serverHostGPUs(ctx, m.server.ID)
	if err != nil {
		return err
	}
	for _, g := range parts {
		for _, b := range budgets {
			if b.GPUIndex == g.Index && b.BudgetMB > 0 && hosts[g.Index].totalMB > b.BudgetMB {
				add(ClusterWarnBudgetBelowDevice)
			}
		}
	}
	return nil
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
	apps, err := s.routes.ApplicationsByServer(ctx, m.server.ID)
	if err != nil {
		return uses, err
	}
	for _, app := range apps {
		if app.Type != routing.ProviderServerAgent {
			continue
		}
		specs, err := s.routes.RuntimeSpecsByApplication(ctx, app.ID)
		if err != nil {
			return uses, err
		}
		for _, spec := range specs {
			uses.shared = true
			rows, err := s.routes.RuntimeSpecGPUs(ctx, spec.ID)
			if err != nil {
				return uses, err
			}
			for _, g := range rows {
				uses.gpus[g.GPUIndex] = true
			}
		}
	}
	memberships, err := s.routes.ClusterMembershipsByServer(ctx, m.server.ID)
	if err != nil {
		return uses, err
	}
	for _, other := range memberships {
		if other.HeadSpecID == headSpecID {
			continue
		}
		uses.shared = true
		_, gpus, err := s.routes.ClusterMembers(ctx, other.HeadSpecID)
		if err != nil {
			return uses, err
		}
		for _, g := range gpus {
			if g.ServerID == m.server.ID {
				uses.gpus[g.GPUIndex] = true
			}
		}
	}
	specs, err := s.routes.RPCMemberSpecsByServer(ctx, m.server.ID)
	if err != nil {
		return uses, err
	}
	for _, spec := range specs {
		if spec.ID == m.row.MemberSpecID || !spec.Enabled {
			continue
		}
		uses.shared = true
		rows, err := s.routes.RPCMemberSpecGPUs(ctx, spec.ID)
		if err != nil {
			return uses, err
		}
		for _, g := range rows {
			uses.gpus[g.GPUIndex] = true
		}
	}
	return uses, nil
}

// ClusterCandidates lists the servers the head on headMappingID may add as
// members in mode ("automatic" when empty): servers other than the head's
// that the caller manages and whose consent admits the head (system scope
// needs no consent; an empty consent list means the member server's own
// admin groups). In automatic mode a server must offer RPC membership
// and have a ggml-rpc-server matching the head binary's version, which needs
// the stored head spec's version known; in manual mode it must have enabled
// member specs that serve no other head, which are listed. Sorted by server
// name.
func (s *Service) ClusterCandidates(ctx context.Context, p auth.Token, headMappingID, mode string) ([]ClusterCandidateDTO, error) {
	mapping, _, headServer, err := s.authorizeMapping(ctx, p, headMappingID)
	if err != nil {
		return nil, err
	}
	mode = normalizeMemberMode(strings.TrimSpace(mode))
	if mode != memberModeAutomatic && mode != memberModeManual {
		return nil, ErrClusterMemberModeInvalid
	}
	headSpec, configured, err := s.routes.RuntimeSpecByMapping(ctx, mapping.ID)
	if err != nil {
		return nil, err
	}
	var head routing.ServerBinary
	headKnown := false
	if configured {
		var listed bool
		if head, listed, err = s.BinaryVersionFor(ctx, headServer.ID, headSpec.Binary); err != nil {
			return nil, err
		}
		headKnown = listed && binaryVersionKnown(head)
	}
	headGroups, err := s.routes.ServerAdminGroups(ctx, headServer.ID)
	if err != nil {
		return nil, err
	}
	servers, err := s.routes.AIServers(ctx)
	if err != nil {
		return nil, err
	}
	out := []ClusterCandidateDTO{}
	for _, srv := range servers {
		if srv.ID == headServer.ID || !s.callerManagesServer(ctx, p, srv.ID) {
			continue
		}
		c := ClusterCandidateDTO{ServerID: srv.ID, ServerName: srv.Name}
		if mode == memberModeAutomatic {
			if !srv.RPCMemberEnabled || !headKnown {
				continue
			}
			admits, err := s.memberConsentAdmits(ctx, p, srv.RPCAllowedHeadGroups, srv.ID, headGroups)
			if err != nil {
				return nil, err
			}
			if !admits {
				continue
			}
			match, ok, err := s.MatchingRPCBinary(ctx, srv.ID, head)
			if err != nil {
				return nil, err
			}
			if !ok {
				continue
			}
			c.RPCBinary, c.RPCBinaryVersion = match.Path, match.EffectiveVersion()
		} else {
			if c.MemberSpecs, err = s.freeMemberSpecs(ctx, p, srv.ID, headSpec.ID, headGroups); err != nil {
				return nil, err
			}
			if len(c.MemberSpecs) == 0 {
				continue
			}
		}
		out = append(out, c)
	}
	slices.SortFunc(out, func(a, b ClusterCandidateDTO) int {
		if a.ServerName != b.ServerName {
			return strings.Compare(a.ServerName, b.ServerName)
		}
		return strings.Compare(a.ServerID, b.ServerID)
	})
	return out, nil
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
		admits, err := s.memberConsentAdmits(ctx, p, spec.AllowedHeadGroups, serverID, headGroups)
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
