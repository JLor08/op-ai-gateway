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
)

var (
	// ErrRPCMemberSpecNotFound is returned for an unknown member spec id and
	// for a spec that belongs to a different server than the one addressed,
	// so a caller cannot probe other servers' spec ids.
	ErrRPCMemberSpecNotFound = errors.New("rpc_member_spec.not_found")
	// ErrRPCMemberFlagReserved rejects a member arg that would override a
	// setting the agent owns (bind address, port, device selection, cache).
	ErrRPCMemberFlagReserved = errors.New("runtime_spec.member_flag_reserved")
	// ErrRPCMemberDeviceBackendInvalid rejects a device_backend outside the
	// known set.
	ErrRPCMemberDeviceBackendInvalid = errors.New("runtime_spec.device_backend_invalid")
	// ErrRPCMemberVRAMEstimateRequired rejects a GPU row without a VRAM
	// estimate on a server whose hardware report shows no NVIDIA GPU: only
	// NVIDIA devices can be measured, so admission needs the operator's
	// number.
	ErrRPCMemberVRAMEstimateRequired = errors.New("runtime_spec.member_vram_estimate_required")
	// ErrRPCMemberInUse rejects disabling or deleting a member spec that a
	// cluster head still references.
	ErrRPCMemberInUse = errors.New("runtime.cluster_member_in_use")
)

// rpcMemberReservedFlags are the member flags the agent injects itself and
// refuses on a member, in every spelling the agent also refuses.
var (
	rpcMemberReservedShort = []string{"-H", "-p", "-d", "-c"}
	rpcMemberReservedLong  = []string{"--host", "--port", "--device", "--cache"}
)

// rpcMemberDeviceBackends is the set of device_backend values the agent
// understands; "" means "auto".
var rpcMemberDeviceBackends = []string{"auto", "cuda", "rocm", "vulkan", "metal", "sycl"}

// RPCMemberSpecDTO is the portal view of a manual RPC member spec.
// UsedByHeadSpecID is set only when the caller may see the head's server.
type RPCMemberSpecDTO struct {
	ID                string              `json:"id"`
	ServerID          string              `json:"server_id"`
	Enabled           bool                `json:"enabled"`
	Binary            string              `json:"binary"`
	Args              []string            `json:"args"`
	Env               map[string]string   `json:"env"`
	GPUs              []RuntimeSpecGPUDTO `json:"gpus"`
	CacheEnabled      bool                `json:"cache_enabled"`
	DeviceBackend     string              `json:"device_backend"`
	RAMEstimateMB     int                 `json:"ram_estimate_mb"`
	AllowedHeadGroups []string            `json:"allowed_head_groups"`
	UsedByHeadSpecID  string              `json:"used_by_head_spec_id,omitempty"`
}

// PutRPCMemberSpecRequest is the full document of a member spec. Enabled
// defaults to true. AllowedHeadGroups nil keeps the stored consent on update
// and defaults to the server's own admin groups on create; an explicit empty
// list is stored as empty and, like the default, admits heads linked to the
// member server's own admin groups. GPU rows keep their request order
// as the member's device order; vram_measured_mb is ignored.
type PutRPCMemberSpecRequest struct {
	Enabled           *bool               `json:"enabled,omitempty"`
	Binary            string              `json:"binary"`
	Args              []string            `json:"args"`
	Env               map[string]string   `json:"env"`
	GPUs              []RuntimeSpecGPUDTO `json:"gpus"`
	CacheEnabled      bool                `json:"cache_enabled"`
	DeviceBackend     string              `json:"device_backend"`
	RAMEstimateMB     int                 `json:"ram_estimate_mb"`
	AllowedHeadGroups *[]string           `json:"allowed_head_groups,omitempty"`
}

// RPCMemberSpecs lists serverID's manual member specs.
func (s *Service) RPCMemberSpecs(ctx context.Context, p auth.Token, serverID string) ([]RPCMemberSpecDTO, error) {
	server, err := s.authorizeServer(ctx, p, serverID)
	if err != nil {
		return nil, err
	}
	specs, err := s.routes.RPCMemberSpecsByServer(ctx, server.ID)
	if err != nil {
		return nil, err
	}
	out := make([]RPCMemberSpecDTO, 0, len(specs))
	for _, spec := range specs {
		dto, err := s.rpcMemberSpecDTO(ctx, p, spec)
		if err != nil {
			return nil, err
		}
		out = append(out, dto)
	}
	return out, nil
}

// PutRPCMemberSpec creates (specID "") or replaces a manual member spec on
// serverID. The request is validated in full before anything is written.
func (s *Service) PutRPCMemberSpec(ctx context.Context, p auth.Token, serverID, specID string, req PutRPCMemberSpecRequest) (RPCMemberSpecDTO, error) {
	server, err := s.authorizeServer(ctx, p, serverID)
	if err != nil {
		return RPCMemberSpecDTO{}, err
	}
	var prior routing.RPCMemberSpec
	if specID != "" {
		prior, err = s.ownedRPCMemberSpec(ctx, server.ID, specID)
		if err != nil {
			return RPCMemberSpecDTO{}, err
		}
	}
	binary := strings.TrimSpace(req.Binary)
	if !runtimeSpecBinaryIsAbsolute(binary) {
		return RPCMemberSpecDTO{}, ErrRuntimeSpecBinaryRequired
	}
	if req.RAMEstimateMB < 0 {
		return RPCMemberSpecDTO{}, ErrRuntimeSpecTuningInvalid
	}
	if err := validateRuntimeSpecGPUs(req.GPUs); err != nil {
		return RPCMemberSpecDTO{}, err
	}
	for k := range req.Env {
		if !runtimeSpecEnvKeyPattern.MatchString(k) {
			return RPCMemberSpecDTO{}, ErrRuntimeSpecEnvInvalid
		}
		// Members always select devices with -d, so a visibility variable
		// would filter a second time and hide the cards from the member.
		if slices.Contains(runtimeSpecVisibleDevicesVars, strings.ToUpper(strings.TrimSpace(k))) {
			return RPCMemberSpecDTO{}, ErrRuntimeSpecVisibleDevicesConflict
		}
	}
	if err := validateRPCMemberArgs(req.Args); err != nil {
		return RPCMemberSpecDTO{}, err
	}
	backend := strings.TrimSpace(req.DeviceBackend)
	if backend == "" {
		backend = "auto"
	}
	if !slices.Contains(rpcMemberDeviceBackends, backend) {
		return RPCMemberSpecDTO{}, ErrRPCMemberDeviceBackendInvalid
	}
	groups, err := s.rpcMemberHeadGroups(ctx, server.ID, req.AllowedHeadGroups, prior)
	if err != nil {
		return RPCMemberSpecDTO{}, err
	}
	if err := s.requireRPCMemberVRAMEstimates(ctx, server.ID, req.GPUs); err != nil {
		return RPCMemberSpecDTO{}, err
	}
	enabled := req.Enabled == nil || *req.Enabled
	if specID != "" && prior.Enabled && !enabled {
		if err := s.rpcMemberInUseError(ctx, p, specID); err != nil {
			return RPCMemberSpecDTO{}, err
		}
	}
	args := req.Args
	if args == nil {
		args = []string{}
	}
	env := req.Env
	if env == nil {
		env = map[string]string{}
	}
	argsJSON, err := json.Marshal(args)
	if err != nil {
		return RPCMemberSpecDTO{}, ErrRuntimeSpecArgsInvalid
	}
	envJSON, err := json.Marshal(env)
	if err != nil {
		return RPCMemberSpecDTO{}, ErrRuntimeSpecEnvInvalid
	}
	now := s.clock().UTC()
	row := routing.RPCMemberSpec{
		ID:                specID,
		ServerID:          server.ID,
		Enabled:           enabled,
		Binary:            binary,
		Args:              string(argsJSON),
		Env:               string(envJSON),
		CacheEnabled:      req.CacheEnabled,
		DeviceBackend:     backend,
		RAMEstimateMB:     req.RAMEstimateMB,
		AllowedHeadGroups: encodeRPCHeadGroups(groups),
		CreatedAt:         now,
		UpdatedAt:         now,
	}
	if specID == "" {
		row.ID = "rpcm_" + compactRandomHex(8)
	} else {
		row.CreatedAt = prior.CreatedAt
	}
	if err := s.routes.UpsertRPCMemberSpec(ctx, row); err != nil {
		return RPCMemberSpecDTO{}, err
	}
	gpuRows := make([]routing.RPCMemberSpecGPU, len(req.GPUs))
	for i, g := range req.GPUs {
		gpuRows[i] = routing.RPCMemberSpecGPU{SpecID: row.ID, GPUIndex: g.Index, Position: i, VRAMEstimateMB: g.VRAMEstimateMB}
	}
	if err := s.routes.SetRPCMemberSpecGPUs(ctx, row.ID, gpuRows); err != nil {
		s.notifyRPCMemberChanged(ctx, server.ID, row.ID)
		return RPCMemberSpecDTO{}, err
	}
	// The membership of the head using the spec holds the measured usage per
	// GPU, so it follows the spec's GPU rows. Serialized with the head writes,
	// which replace the same membership.
	s.clusterWriteMu.Lock()
	_, err = s.resyncManualMemberGPUs(ctx, row.ID, gpuRows)
	s.clusterWriteMu.Unlock()
	s.notifyRPCMemberChanged(ctx, server.ID, row.ID)
	if err != nil {
		return RPCMemberSpecDTO{}, err
	}
	stored, _, err := s.routes.RPCMemberSpecByID(ctx, row.ID)
	if err != nil {
		return RPCMemberSpecDTO{}, err
	}
	return s.rpcMemberSpecDTO(ctx, p, stored)
}

// DeleteRPCMemberSpec removes a manual member spec. A spec a head still
// references is ErrRPCMemberInUse.
func (s *Service) DeleteRPCMemberSpec(ctx context.Context, p auth.Token, serverID, specID string) error {
	server, err := s.authorizeServer(ctx, p, serverID)
	if err != nil {
		return err
	}
	if _, err := s.ownedRPCMemberSpec(ctx, server.ID, specID); err != nil {
		return err
	}
	if err := s.rpcMemberInUseError(ctx, p, specID); err != nil {
		return err
	}
	if err := s.routes.DeleteRPCMemberSpec(ctx, specID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return ErrRPCMemberSpecNotFound
		}
		return err
	}
	s.notifyRuntimeChanged(server.ID)
	return nil
}

// ownedRPCMemberSpec loads specID and requires it to live on serverID.
func (s *Service) ownedRPCMemberSpec(ctx context.Context, serverID, specID string) (routing.RPCMemberSpec, error) {
	spec, ok, err := s.routes.RPCMemberSpecByID(ctx, specID)
	if err != nil {
		return routing.RPCMemberSpec{}, err
	}
	if !ok || spec.ServerID != serverID {
		return routing.RPCMemberSpec{}, ErrRPCMemberSpecNotFound
	}
	return spec, nil
}

// validateRPCMemberArgs refuses the flags the agent owns on a member: bind
// host, port, device selection and the cache switch. Both the separate-value
// and the "=" forms are refused, and a short flag with its value attached
// ("-p8080") counts as the flag.
func validateRPCMemberArgs(args []string) error {
	for _, raw := range args {
		a := strings.TrimSpace(raw)
		if strings.HasPrefix(a, "--") {
			name, _, _ := strings.Cut(a, "=")
			if slices.Contains(rpcMemberReservedLong, name) {
				return ErrRPCMemberFlagReserved
			}
			continue
		}
		for _, f := range rpcMemberReservedShort {
			if strings.HasPrefix(a, f) {
				return ErrRPCMemberFlagReserved
			}
		}
	}
	return nil
}

// rpcMemberHeadGroups resolves the member's consent list: the request when
// given, else the stored one on update, else the server's own admin groups.
func (s *Service) rpcMemberHeadGroups(ctx context.Context, serverID string, requested *[]string, prior routing.RPCMemberSpec) ([]string, error) {
	switch {
	case requested != nil:
		return s.validateRPCHeadGroups(ctx, *requested)
	case prior.ID != "":
		return decodeRPCHeadGroups(prior.AllowedHeadGroups), nil
	default:
		return s.routes.ServerAdminGroups(ctx, serverID)
	}
}

// requireRPCMemberVRAMEstimates enforces the estimate for every declared GPU
// that has none and is known to be non-NVIDIA. A GPU whose vendor cannot be
// determined (no hardware report, an unreadable one, or an index the report
// does not list) keeps the estimate optional.
func (s *Service) requireRPCMemberVRAMEstimates(ctx context.Context, serverID string, gpus []RuntimeSpecGPUDTO) error {
	missing := false
	for _, g := range gpus {
		if g.VRAMEstimateMB == 0 {
			missing = true
		}
	}
	if !missing {
		return nil
	}
	hw, ok, err := s.routes.ServerHardwareByServer(ctx, serverID)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	var report struct {
		GPUs []struct {
			Index    int    `json:"index"`
			Name     string `json:"name"`
			PCIBusID string `json:"pci_bus_id"`
		} `json:"gpus"`
	}
	if err := json.Unmarshal([]byte(hw.ReportJSON), &report); err != nil {
		return nil
	}
	for _, g := range gpus {
		if g.VRAMEstimateMB != 0 {
			continue
		}
		for _, rg := range report.GPUs {
			if rg.Index == g.Index && !gpuLooksNVIDIA(rg.Name, rg.PCIBusID) {
				return ErrRPCMemberVRAMEstimateRequired
			}
		}
	}
	return nil
}

// gpuLooksNVIDIA recognises an NVIDIA card in a hardware report, which has no
// vendor field: the collector fills the PCI bus id only for NVIDIA devices,
// and the name is the fallback.
func gpuLooksNVIDIA(name, pciBusID string) bool {
	n := strings.ToLower(name)
	return pciBusID != "" || strings.Contains(n, "nvidia") || strings.Contains(n, "geforce") ||
		strings.Contains(n, "quadro") || strings.Contains(n, "tesla")
}

// rpcMemberInUseError returns ErrRPCMemberInUse when a head references the
// spec. The message names the head's server only when the caller may see it.
func (s *Service) rpcMemberInUseError(ctx context.Context, p auth.Token, specID string) error {
	head, ok, err := s.routes.ClusterMemberBySpec(ctx, specID)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	if name := s.visibleHeadServerName(ctx, p, head.HeadSpecID); name != "" {
		return &RPCMemberInUseError{HeadServerName: name}
	}
	return ErrRPCMemberInUse
}

// clusterMemberServerInUse returns ErrRPCMemberInUse when serverID is a
// member of a cluster head; the message names the head's server only when
// the caller may see it.
func (s *Service) clusterMemberServerInUse(ctx context.Context, p auth.Token, serverID string) error {
	memberships, err := s.routes.ClusterMembershipsByServer(ctx, serverID)
	if err != nil {
		return err
	}
	if len(memberships) == 0 {
		return nil
	}
	if name := s.visibleHeadServerName(ctx, p, memberships[0].HeadSpecID); name != "" {
		return &RPCMemberInUseError{HeadServerName: name}
	}
	return ErrRPCMemberInUse
}

// RPCMemberInUseError is ErrRPCMemberInUse carrying the name of the head's
// server; it is only built for callers who may see that server.
type RPCMemberInUseError struct{ HeadServerName string }

func (e *RPCMemberInUseError) Error() string {
	return fmt.Sprintf("%s: used by a cluster head on server %q", ErrRPCMemberInUse.Error(), e.HeadServerName)
}

func (e *RPCMemberInUseError) Unwrap() error { return ErrRPCMemberInUse }

// headServerID resolves head spec -> mapping -> application -> server.
func (s *Service) headServerID(ctx context.Context, headSpecID string) (string, bool) {
	spec, ok, err := s.routes.RuntimeSpecByID(ctx, headSpecID)
	if err != nil || !ok {
		return "", false
	}
	mapping, err := s.routes.MappingByID(ctx, spec.MappingID)
	if err != nil {
		return "", false
	}
	app, err := s.routes.ApplicationByID(ctx, mapping.ApplicationID)
	if err != nil {
		return "", false
	}
	return app.ServerID, true
}

// visibleHeadServerName returns the head server's name when p may manage it,
// else "".
func (s *Service) visibleHeadServerName(ctx context.Context, p auth.Token, headSpecID string) string {
	serverID, ok := s.headServerID(ctx, headSpecID)
	if !ok {
		return ""
	}
	server, err := s.authorizeServer(ctx, p, serverID)
	if err != nil {
		return ""
	}
	return server.Name
}

// notifyRPCMemberChanged tells the member's agent and, when a head uses the
// spec, every server of that head's cluster that their runtime document
// changed: the spec's settings move the head's generation and device count,
// and its consent or its enabled flag decide whether the cluster is pushed
// at all, which every member's document follows.
func (s *Service) notifyRPCMemberChanged(ctx context.Context, serverID, specID string) {
	s.notifyRuntimeChanged(serverID)
	head, ok, err := s.routes.ClusterMemberBySpec(ctx, specID)
	if err != nil || !ok {
		return
	}
	s.reconcileRPCPolicies(ctx)
	for _, id := range s.clusterServers(ctx, head.HeadSpecID) {
		if id != serverID {
			s.notifyRuntimeChanged(id)
		}
	}
}

func (s *Service) rpcMemberSpecDTO(ctx context.Context, p auth.Token, spec routing.RPCMemberSpec) (RPCMemberSpecDTO, error) {
	args := []string{}
	if err := json.Unmarshal([]byte(spec.Args), &args); err != nil && strings.TrimSpace(spec.Args) != "" {
		return RPCMemberSpecDTO{}, fmt.Errorf("rpc member spec %s: stored args: %w", spec.ID, err)
	}
	env := map[string]string{}
	if err := json.Unmarshal([]byte(spec.Env), &env); err != nil && strings.TrimSpace(spec.Env) != "" {
		return RPCMemberSpecDTO{}, fmt.Errorf("rpc member spec %s: stored env: %w", spec.ID, err)
	}
	if args == nil {
		args = []string{}
	}
	if env == nil {
		env = map[string]string{}
	}
	rows, err := s.routes.RPCMemberSpecGPUs(ctx, spec.ID)
	if err != nil {
		return RPCMemberSpecDTO{}, err
	}
	gpus := make([]RuntimeSpecGPUDTO, 0, len(rows))
	for _, r := range rows {
		gpus = append(gpus, RuntimeSpecGPUDTO{Index: r.GPUIndex, VRAMEstimateMB: r.VRAMEstimateMB})
	}
	backend := spec.DeviceBackend
	if backend == "" {
		backend = "auto"
	}
	dto := RPCMemberSpecDTO{
		ID:                spec.ID,
		ServerID:          spec.ServerID,
		Enabled:           spec.Enabled,
		Binary:            spec.Binary,
		Args:              args,
		Env:               env,
		GPUs:              gpus,
		CacheEnabled:      spec.CacheEnabled,
		DeviceBackend:     backend,
		RAMEstimateMB:     spec.RAMEstimateMB,
		AllowedHeadGroups: decodeRPCHeadGroups(spec.AllowedHeadGroups),
	}
	head, ok, err := s.routes.ClusterMemberBySpec(ctx, spec.ID)
	if err != nil {
		return RPCMemberSpecDTO{}, err
	}
	if ok && s.visibleHeadServerName(ctx, p, head.HeadSpecID) != "" {
		dto.UsedByHeadSpecID = head.HeadSpecID
	}
	return dto, nil
}
