// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"encoding/json"
	"errors"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/routing"
	"regexp"
	"strings"
)

// CodeRuntimeSpecNotFound is ErrRuntimeSpecNotFound's API error code,
// exported so the gateway's error mapper (portal_runtime_endpoints.go) can
// share the exact value instead of re-hardcoding it (mirrors
// CodeApplicationNotFound/CodeMappingNotFound in service_applications.go).
const CodeRuntimeSpecNotFound = "runtime_spec.not_found"

var (
	// ErrRuntimeSpecNotFound is returned by DeleteRuntimeSpec when the
	// mapping has no spec row to delete. GetRuntimeSpec never returns it
	// (an absent spec there is Configured:false, not an error) — see the
	// package's GetRuntimeSpec doc comment.
	ErrRuntimeSpecNotFound          = errors.New(CodeRuntimeSpecNotFound)
	ErrRuntimeSpecBinaryRequired    = errors.New("runtime_spec.binary_required")
	ErrRuntimeSpecArgsInvalid       = errors.New("runtime_spec.args_invalid")
	ErrRuntimeSpecEnvInvalid        = errors.New("runtime_spec.env_invalid")
	ErrRuntimeSpecGPUInvalid        = errors.New("runtime_spec.gpu_invalid")
	ErrRuntimeSpecTuningInvalid     = errors.New("runtime_spec.tuning_invalid")
	ErrRuntimeSpecAdminStateInvalid = errors.New("runtime_spec.admin_state_invalid")
	// ErrRuntimeSpecNotServerAgent rejects a write (PUT or DELETE) targeting
	// a mapping whose owning application is not of type
	// routing.ProviderServerAgent — a runtime spec only makes sense for an
	// agent-managed model process. GET is deliberately permissive (see
	// GetRuntimeSpec) so the portal can ask about any mapping without
	// special-casing the application type.
	ErrRuntimeSpecNotServerAgent = errors.New("runtime_spec.application_not_server_agent")
)

const (
	defaultRuntimeSpecHealthPath            = "/health"
	defaultRuntimeSpecHealthTimeoutSeconds  = 5
	defaultRuntimeSpecStartupTimeoutSeconds = 180
)

// runtimeSpecEnvKeyPattern matches a shell-style environment variable name:
// upper-case letters, digits, underscore, not starting with a digit. Values
// are unrestricted (they legitimately carry ${AGENT_ENV:NAME}/${PORT}
// placeholders the agent resolves at launch time — never validated or
// rewritten here; see the RuntimeSpecDTO.Env doc below).
var runtimeSpecEnvKeyPattern = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)

// RuntimeSpecGPUDTO is one per-GPU VRAM demand row on the wire.
// VRAMEstimateMB is operator-owned (round-tripped from PutRuntimeSpecRequest
// verbatim); VRAMMeasuredMB is agent-owned and read-only on this API — see
// the VRAM ownership rule on PutRuntimeSpec.
type RuntimeSpecGPUDTO struct {
	Index          int `json:"index"`
	VRAMEstimateMB int `json:"vram_estimate_mb"`
	VRAMMeasuredMB int `json:"vram_measured_mb"`
}

// RuntimeSpecDTO is the portal-facing representation of a mapping's
// agent-managed launch spec (routing.RuntimeSpec + its GPU rows).
type RuntimeSpecDTO struct {
	// Configured is false when the mapping has no runtime spec row yet — the
	// only signal GetRuntimeSpec ever uses for "not configured"; every other
	// field is then a zero value (GPUs/Args/Env still non-nil empty).
	Configured                  bool                `json:"configured"`
	ID                          string              `json:"id,omitempty"`
	MappingID                   string              `json:"mapping_id"`
	Enabled                     bool                `json:"enabled"`
	Binary                      string              `json:"binary"`
	Args                        []string            `json:"args"`
	Env                         map[string]string   `json:"env"`
	WorkDir                     string              `json:"work_dir"`
	ListenPort                  int                 `json:"listen_port"`
	HealthPath                  string              `json:"health_path"`
	HealthTimeoutSeconds        int                 `json:"health_timeout_seconds"`
	StartupTimeoutSeconds       int                 `json:"startup_timeout_seconds"`
	IdleTimeoutSeconds          int                 `json:"idle_timeout_seconds"`
	AdmissionWaitTimeoutSeconds int                 `json:"admission_wait_timeout_seconds"`
	Pinned                      bool                `json:"pinned"`
	AdminState                  string              `json:"admin_state"`
	VRAMLocked                  bool                `json:"vram_locked"`
	GPUs                        []RuntimeSpecGPUDTO `json:"gpus"`
}

// PutRuntimeSpecRequest is a full-document upsert (no pointer-patch): every
// field is applied verbatim (after validation/defaulting), never merged
// against the stored row — except VRAMMeasuredMB on each GPU entry, which is
// ALWAYS ignored (agent-owned; see PutRuntimeSpec's VRAM ownership rule).
type PutRuntimeSpecRequest struct {
	Enabled                     bool                `json:"enabled"`
	Binary                      string              `json:"binary"`
	Args                        []string            `json:"args"`
	Env                         map[string]string   `json:"env"`
	WorkDir                     string              `json:"work_dir"`
	ListenPort                  int                 `json:"listen_port"`
	HealthPath                  string              `json:"health_path"`
	HealthTimeoutSeconds        int                 `json:"health_timeout_seconds"`
	StartupTimeoutSeconds       int                 `json:"startup_timeout_seconds"`
	IdleTimeoutSeconds          int                 `json:"idle_timeout_seconds"`
	AdmissionWaitTimeoutSeconds int                 `json:"admission_wait_timeout_seconds"`
	Pinned                      bool                `json:"pinned"`
	AdminState                  string              `json:"admin_state"`
	VRAMLocked                  bool                `json:"vram_locked"`
	GPUs                        []RuntimeSpecGPUDTO `json:"gpus"`
}

// GetRuntimeSpec returns mappingID's runtime spec, or Configured:false when
// none has been created yet (not an error). Deliberately permissive about
// the owning application's type — unlike PutRuntimeSpec/DeleteRuntimeSpec,
// it never returns ErrRuntimeSpecNotServerAgent, so the portal can query any
// mapping's runtime configuration without special-casing non-server_agent
// applications.
func (s *Service) GetRuntimeSpec(ctx context.Context, principal auth.Token, mappingID string) (RuntimeSpecDTO, error) {
	mapping, _, _, err := s.authorizeMapping(ctx, principal, mappingID)
	if err != nil {
		return RuntimeSpecDTO{}, err
	}
	spec, ok, err := s.routes.RuntimeSpecByMapping(ctx, mapping.ID)
	if err != nil {
		return RuntimeSpecDTO{}, err
	}
	if !ok {
		return RuntimeSpecDTO{MappingID: mapping.ID, Args: []string{}, Env: map[string]string{}, GPUs: []RuntimeSpecGPUDTO{}}, nil
	}
	gpus, err := s.routes.RuntimeSpecGPUs(ctx, spec.ID)
	if err != nil {
		return RuntimeSpecDTO{}, err
	}
	return runtimeSpecDTO(spec, gpus)
}

// PutRuntimeSpec validates and upserts mappingID's runtime spec (create on
// first write, full-document replace thereafter).
//
// VRAM ownership rule (one rule, both directions): vram_estimate_mb is
// operator-owned — only this method ever writes it. vram_measured_mb is
// agent-owned — only the telemetry write-back (a later task) writes it, so
// this method PRESERVES the stored measured values verbatim for every GPU
// index that already had a row, regardless of what the request carries, and
// starts a brand-new index at 0. vram_locked only gates the agent's future
// write-back; it never blocks this write.
func (s *Service) PutRuntimeSpec(ctx context.Context, principal auth.Token, mappingID string, req PutRuntimeSpecRequest) (RuntimeSpecDTO, error) {
	mapping, app, server, err := s.authorizeMapping(ctx, principal, mappingID)
	if err != nil {
		return RuntimeSpecDTO{}, err
	}
	if app.Type != routing.ProviderServerAgent {
		return RuntimeSpecDTO{}, ErrRuntimeSpecNotServerAgent
	}
	// Validate everything that can fail BEFORE mutating/persisting anything.
	binary := strings.TrimSpace(req.Binary)
	if binary == "" || !strings.HasPrefix(binary, "/") {
		return RuntimeSpecDTO{}, ErrRuntimeSpecBinaryRequired
	}
	if req.ListenPort < 0 || req.HealthTimeoutSeconds < 0 || req.StartupTimeoutSeconds < 0 ||
		req.IdleTimeoutSeconds < 0 || req.AdmissionWaitTimeoutSeconds < 0 {
		return RuntimeSpecDTO{}, ErrRuntimeSpecTuningInvalid
	}
	adminState := strings.TrimSpace(req.AdminState)
	switch adminState {
	case "", "force_running", "force_stopped":
	default:
		return RuntimeSpecDTO{}, ErrRuntimeSpecAdminStateInvalid
	}
	if err := validateRuntimeSpecGPUs(req.GPUs); err != nil {
		return RuntimeSpecDTO{}, err
	}
	for k := range req.Env {
		if !runtimeSpecEnvKeyPattern.MatchString(k) {
			return RuntimeSpecDTO{}, ErrRuntimeSpecEnvInvalid
		}
	}
	args := req.Args
	if args == nil {
		args = []string{}
	}
	argsJSON, err := json.Marshal(args)
	if err != nil {
		return RuntimeSpecDTO{}, ErrRuntimeSpecArgsInvalid
	}
	env := req.Env
	if env == nil {
		env = map[string]string{}
	}
	envJSON, err := json.Marshal(env)
	if err != nil {
		return RuntimeSpecDTO{}, ErrRuntimeSpecEnvInvalid
	}
	healthPath := strings.TrimSpace(req.HealthPath)
	if healthPath == "" {
		healthPath = defaultRuntimeSpecHealthPath
	}
	healthTimeout := req.HealthTimeoutSeconds
	if healthTimeout == 0 {
		healthTimeout = defaultRuntimeSpecHealthTimeoutSeconds
	}
	startupTimeout := req.StartupTimeoutSeconds
	if startupTimeout == 0 {
		startupTimeout = defaultRuntimeSpecStartupTimeoutSeconds
	}
	// Read-then-upsert: an existing spec's id/created_at are preserved, and
	// its stored GPU rows are the source of truth for VRAMMeasuredMB (the
	// VRAM ownership rule above) — never what the request sent.
	existing, hadExisting, err := s.routes.RuntimeSpecByMapping(ctx, mapping.ID)
	if err != nil {
		return RuntimeSpecDTO{}, err
	}
	measuredByIndex := map[int]int{}
	if hadExisting {
		existingGPUs, err := s.routes.RuntimeSpecGPUs(ctx, existing.ID)
		if err != nil {
			return RuntimeSpecDTO{}, err
		}
		for _, g := range existingGPUs {
			measuredByIndex[g.GPUIndex] = g.VRAMMeasuredMB
		}
	}
	now := s.clock().UTC()
	spec := routing.RuntimeSpec{
		ID:                          "rspec_" + compactRandomHex(16),
		MappingID:                   mapping.ID,
		Enabled:                     req.Enabled,
		Binary:                      binary,
		Args:                        string(argsJSON),
		Env:                         string(envJSON),
		WorkDir:                     strings.TrimSpace(req.WorkDir),
		ListenPort:                  req.ListenPort,
		HealthPath:                  healthPath,
		HealthTimeoutSeconds:        healthTimeout,
		StartupTimeoutSeconds:       startupTimeout,
		IdleTimeoutSeconds:          req.IdleTimeoutSeconds,
		AdmissionWaitTimeoutSeconds: req.AdmissionWaitTimeoutSeconds,
		Pinned:                      req.Pinned,
		AdminState:                  adminState,
		VRAMLocked:                  req.VRAMLocked,
		CreatedAt:                   now,
		UpdatedAt:                   now,
	}
	if hadExisting {
		spec.ID = existing.ID
		spec.CreatedAt = existing.CreatedAt
	}
	if err := s.routes.UpsertRuntimeSpec(ctx, spec); err != nil {
		return RuntimeSpecDTO{}, err
	}
	gpuRows := make([]routing.RuntimeSpecGPU, 0, len(req.GPUs))
	for _, g := range req.GPUs {
		gpuRows = append(gpuRows, routing.RuntimeSpecGPU{
			SpecID:         spec.ID,
			GPUIndex:       g.Index,
			VRAMEstimateMB: g.VRAMEstimateMB,
			VRAMMeasuredMB: measuredByIndex[g.Index], // 0 for a brand-new index; preserved otherwise
		})
	}
	if err := s.routes.SetRuntimeSpecGPUs(ctx, spec.ID, gpuRows); err != nil {
		return RuntimeSpecDTO{}, err
	}
	s.notifyRuntimeChanged(server.ID)
	storedGPUs, err := s.routes.RuntimeSpecGPUs(ctx, spec.ID)
	if err != nil {
		return RuntimeSpecDTO{}, err
	}
	return runtimeSpecDTO(spec, storedGPUs)
}

// DeleteRuntimeSpec removes mappingID's runtime spec. ErrRuntimeSpecNotFound
// when none exists; ErrRuntimeSpecNotServerAgent when the owning
// application is not server_agent (mirrors PutRuntimeSpec's write gate — see
// GetRuntimeSpec's doc for why GET alone skips this check).
func (s *Service) DeleteRuntimeSpec(ctx context.Context, principal auth.Token, mappingID string) error {
	mapping, app, server, err := s.authorizeMapping(ctx, principal, mappingID)
	if err != nil {
		return err
	}
	if app.Type != routing.ProviderServerAgent {
		return ErrRuntimeSpecNotServerAgent
	}
	spec, ok, err := s.routes.RuntimeSpecByMapping(ctx, mapping.ID)
	if err != nil {
		return err
	}
	if !ok {
		return ErrRuntimeSpecNotFound
	}
	if err := s.routes.DeleteRuntimeSpec(ctx, spec.ID); err != nil {
		return err
	}
	s.notifyRuntimeChanged(server.ID)
	return nil
}

// validateRuntimeSpecGPUs rejects a negative GPU index, a negative VRAM
// estimate, or a GPU index repeated across entries — all ErrRuntimeSpecGPUInvalid,
// checked BEFORE any store call (the store's own duplicate-index guard
// returns ErrConflict, a storage-layer concern this method never surfaces).
func validateRuntimeSpecGPUs(gpus []RuntimeSpecGPUDTO) error {
	seen := make(map[int]struct{}, len(gpus))
	for _, g := range gpus {
		if g.Index < 0 || g.VRAMEstimateMB < 0 {
			return ErrRuntimeSpecGPUInvalid
		}
		if _, dup := seen[g.Index]; dup {
			return ErrRuntimeSpecGPUInvalid
		}
		seen[g.Index] = struct{}{}
	}
	return nil
}

// runtimeSpecDTO builds the wire DTO from a stored spec + its GPU rows.
// Args/Env are opaque JSON strings at the store layer (the netbird_group_ids
// pattern) — an unmarshal failure here means the stored row is corrupt, not
// a client-input problem, but still surfaces as the matching domain sentinel
// (ErrRuntimeSpecArgsInvalid / ErrRuntimeSpecEnvInvalid) rather than a raw
// JSON error or a 500.
func runtimeSpecDTO(spec routing.RuntimeSpec, gpus []routing.RuntimeSpecGPU) (RuntimeSpecDTO, error) {
	var args []string
	if err := json.Unmarshal([]byte(spec.Args), &args); err != nil {
		return RuntimeSpecDTO{}, ErrRuntimeSpecArgsInvalid
	}
	if args == nil {
		args = []string{}
	}
	env := map[string]string{}
	if err := json.Unmarshal([]byte(spec.Env), &env); err != nil {
		return RuntimeSpecDTO{}, ErrRuntimeSpecEnvInvalid
	}
	gpuDTOs := make([]RuntimeSpecGPUDTO, 0, len(gpus))
	for _, g := range gpus {
		gpuDTOs = append(gpuDTOs, RuntimeSpecGPUDTO{
			Index:          g.GPUIndex,
			VRAMEstimateMB: g.VRAMEstimateMB,
			VRAMMeasuredMB: g.VRAMMeasuredMB,
		})
	}
	return RuntimeSpecDTO{
		Configured:                  true,
		ID:                          spec.ID,
		MappingID:                   spec.MappingID,
		Enabled:                     spec.Enabled,
		Binary:                      spec.Binary,
		Args:                        args,
		Env:                         env,
		WorkDir:                     spec.WorkDir,
		ListenPort:                  spec.ListenPort,
		HealthPath:                  spec.HealthPath,
		HealthTimeoutSeconds:        spec.HealthTimeoutSeconds,
		StartupTimeoutSeconds:       spec.StartupTimeoutSeconds,
		IdleTimeoutSeconds:          spec.IdleTimeoutSeconds,
		AdmissionWaitTimeoutSeconds: spec.AdmissionWaitTimeoutSeconds,
		Pinned:                      spec.Pinned,
		AdminState:                  spec.AdminState,
		VRAMLocked:                  spec.VRAMLocked,
		GPUs:                        gpuDTOs,
	}, nil
}

// notifyRuntimeChanged best-effort notifies the runtime-config-changed hook
// (ServiceDeps.OnRuntimeConfigChanged / SetRuntimeConfigChangedHook) after a
// successful runtime-spec write. nil-safe: unset in every test that doesn't
// care, and in any driver that predates Task 8's real push wiring.
func (s *Service) notifyRuntimeChanged(serverID string) {
	if s.runtimeChanged != nil {
		s.runtimeChanged(serverID)
	}
}
