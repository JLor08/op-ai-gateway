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
	"time"
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

// Task 6 sentinels: the co-residency matrix, per-GPU VRAM budgets, the
// managed-runtime-only application-create gate, and the server-level
// runtime-process-limit validation. Grouped here (rather than split across
// service.go/service_applications.go, where the request fields they validate
// physically live) because they are one feature cut, mirroring how every
// RuntimeSpec sentinel above lives in this file regardless of which method
// returns it.
var (
	// ErrCoResidencyPairInvalid rejects a SetCoResidency pair for any of:
	// the two mapping ids being identical, either id not belonging to THIS
	// application's own mappings (verified locally, not a global existence
	// check -- a pair naming a mapping from a different application is
	// invalid the same way), or two pairs colliding after canonical
	// (mapping_a_id < mapping_b_id) normalization.
	ErrCoResidencyPairInvalid = errors.New("runtime_coresidency.pair_invalid")
	// ErrGPUBudgetInvalid rejects a SetServerGPUBudgets entry with a negative
	// index, a negative budget_mb, or an index repeated across entries.
	ErrGPUBudgetInvalid = errors.New("server.gpu_budget_invalid")
	// ErrServerManagedRuntimeOnly rejects CreateApplication when the target
	// server is ManagedRuntimeOnly and the requested type is not
	// routing.ProviderServerAgent. HTTP 409 (a state conflict with the
	// server's own configuration, not a malformed request).
	ErrServerManagedRuntimeOnly = errors.New("application.managed_runtime_only")
	// ErrServerRuntimeLimitInvalid rejects a negative
	// CreateServerRequest/UpdateServerRequest.RuntimeMaxProcesses.
	ErrServerRuntimeLimitInvalid = errors.New("server.runtime_limit_invalid")
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
// when none exists. Deliberately does NOT gate on the owning application's
// type the way PutRuntimeSpec does: UpdateApplication lets an operator
// retype a server_agent application to something else with no check against
// its current type, and DeleteApplication does not cascade-clean runtime
// specs — so a spec can end up on a non-server_agent application through
// ordinary API use, not just seeded test state. Removal must always be
// possible regardless of how a dependency became orphaned; only the
// creation of a NEW dependency on server_agent semantics is gated. (An
// earlier version of this method gated DELETE the same way as PUT; that was
// a defect, not a deliberate symmetry — see the fix-round-1 note in the
// task-5 report.)
func (s *Service) DeleteRuntimeSpec(ctx context.Context, principal auth.Token, mappingID string) error {
	mapping, _, server, err := s.authorizeMapping(ctx, principal, mappingID)
	if err != nil {
		return err
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

// --- Task 6: co-residency matrix --------------------------------------------

// CoResidencyDTO is the portal-facing pairwise co-residency matrix for one
// application: every ALLOWED pair of its own mappings that may be loaded on
// the same AI server at the same time. Each pair is always canonical
// (Pairs[i][0] < Pairs[i][1] lexicographically) -- see SetCoResidency.
// Always a non-nil Pairs slice, even when empty.
type CoResidencyDTO struct {
	Pairs [][2]string `json:"pairs"`
}

// SetCoResidencyRequest is a full-document replace (like
// SetRuntimeSpecGPUs/SetServerGPUBudgets): every pair supplied here IS the
// new set. Pair ordering within each element does not matter -- SetCoResidency
// sorts each pair server-side before storing/comparing.
type SetCoResidencyRequest struct {
	Pairs [][2]string `json:"pairs"`
}

// GetCoResidency returns appID's allowed co-residency pairs. authorizeApplication
// gates it (404-no-leak, same collapse as every other application read).
func (s *Service) GetCoResidency(ctx context.Context, principal auth.Token, appID string) (CoResidencyDTO, error) {
	app, _, err := s.authorizeApplication(ctx, principal, appID)
	if err != nil {
		return CoResidencyDTO{}, err
	}
	rules, err := s.routes.CoResidencyRulesByApplication(ctx, app.ID)
	if err != nil {
		return CoResidencyDTO{}, err
	}
	return coResidencyDTO(rules), nil
}

// SetCoResidency validates and atomically replaces appID's whole co-residency
// set. Every pair is validated BEFORE any store call (validate-before-mutate):
// the two ids must be distinct, both must name a mapping belonging to appID
// itself (checked against s.routes.MappingsByApplication, never a bare global
// existence check -- a mapping id that exists but belongs to a DIFFERENT
// application is exactly as invalid as one that does not exist at all), each
// pair is canonicalized by sorting it (mapping_a_id < mapping_b_id
// lexicographically) so the client never has to submit pairs in a particular
// order, and duplicate pairs are rejected AFTER that normalization -- so
// [["a","b"],["b","a"]] is a duplicate, not two distinct entries. The store
// itself (SetCoResidencyRules) is a dumb pair table by design and performs
// none of this; it is entirely this method's job.
func (s *Service) SetCoResidency(ctx context.Context, principal auth.Token, appID string, req SetCoResidencyRequest) (CoResidencyDTO, error) {
	app, server, err := s.authorizeApplication(ctx, principal, appID)
	if err != nil {
		return CoResidencyDTO{}, err
	}
	ownMappings, err := s.routes.MappingsByApplication(ctx, app.ID)
	if err != nil {
		return CoResidencyDTO{}, err
	}
	belongsToApp := make(map[string]bool, len(ownMappings))
	for _, m := range ownMappings {
		belongsToApp[m.ID] = true
	}
	seenPairs := make(map[[2]string]bool, len(req.Pairs))
	rules := make([]routing.CoResidencyRule, 0, len(req.Pairs))
	now := s.clock().UTC()
	for _, pair := range req.Pairs {
		a, b := strings.TrimSpace(pair[0]), strings.TrimSpace(pair[1])
		if a == "" || b == "" || a == b {
			return CoResidencyDTO{}, ErrCoResidencyPairInvalid
		}
		if !belongsToApp[a] || !belongsToApp[b] {
			return CoResidencyDTO{}, ErrCoResidencyPairInvalid
		}
		if a > b {
			a, b = b, a
		}
		key := [2]string{a, b}
		if seenPairs[key] {
			return CoResidencyDTO{}, ErrCoResidencyPairInvalid
		}
		seenPairs[key] = true
		rules = append(rules, routing.CoResidencyRule{ApplicationID: app.ID, MappingAID: a, MappingBID: b, CreatedAt: now})
	}
	if err := s.routes.SetCoResidencyRules(ctx, app.ID, rules); err != nil {
		return CoResidencyDTO{}, err
	}
	s.notifyRuntimeChanged(server.ID)
	stored, err := s.routes.CoResidencyRulesByApplication(ctx, app.ID)
	if err != nil {
		return CoResidencyDTO{}, err
	}
	return coResidencyDTO(stored), nil
}

// coResidencyDTO builds the wire DTO from stored rules; always a non-nil
// Pairs slice (a collection-shaped return must never serialize to JSON
// null -- see SetRuntimeSpecGPUs's equivalent contract).
func coResidencyDTO(rules []routing.CoResidencyRule) CoResidencyDTO {
	pairs := make([][2]string, 0, len(rules))
	for _, r := range rules {
		pairs = append(pairs, [2]string{r.MappingAID, r.MappingBID})
	}
	return CoResidencyDTO{Pairs: pairs}
}

// --- Task 6: per-GPU VRAM budgets -------------------------------------------

// GPUBudgetDTO is one per-GPU VRAM budget row on the wire. ExpectedUUID/
// ExpectedName are a purely descriptive drift detector, snapshotted
// server-side from live telemetry -- see SetServerGPUBudgets; a client's
// request value for either is always ignored (never trusted on the wire),
// both on first creation and on every later PUT.
type GPUBudgetDTO struct {
	Index        int    `json:"index"`
	BudgetMB     int    `json:"budget_mb"`
	ExpectedUUID string `json:"expected_uuid"`
	ExpectedName string `json:"expected_name"`
}

// SetGPUBudgetsRequest is a full-document replace, mirroring SetCoResidencyRequest.
type SetGPUBudgetsRequest struct {
	Budgets []GPUBudgetDTO `json:"budgets"`
}

// GetServerGPUBudgets returns serverID's per-GPU VRAM budgets. authorizeServer
// gates it (404-no-leak).
func (s *Service) GetServerGPUBudgets(ctx context.Context, principal auth.Token, serverID string) ([]GPUBudgetDTO, error) {
	server, err := s.authorizeServer(ctx, principal, serverID)
	if err != nil {
		return nil, err
	}
	budgets, err := s.routes.ServerGPUBudgets(ctx, server.ID)
	if err != nil {
		return nil, err
	}
	return gpuBudgetDTOs(budgets), nil
}

// SetServerGPUBudgets validates and atomically replaces serverID's whole
// per-GPU budget set. index must be >= 0 and unique across the request;
// budget_mb must be >= 0 (both ErrGPUBudgetInvalid, checked BEFORE any store
// call).
//
// expected_uuid/expected_name ownership rule (mirrors PutRuntimeSpec's VRAM
// ownership rule): they are a purely descriptive drift detector, never
// client-writable. For an index that already has a stored row, this method
// PRESERVES its expected_* verbatim regardless of what the request carries --
// drift detection is only meaningful against the ORIGINAL snapshot. For a
// brand-new index, expected_* is snapshotted from the latest telemetry
// sample's GPU list (see latestGPUSnapshotByIndex); when no sample exists
// yet, it is left empty rather than failing the write.
func (s *Service) SetServerGPUBudgets(ctx context.Context, principal auth.Token, serverID string, req SetGPUBudgetsRequest) ([]GPUBudgetDTO, error) {
	server, err := s.authorizeServer(ctx, principal, serverID)
	if err != nil {
		return nil, err
	}
	seenIndex := make(map[int]bool, len(req.Budgets))
	for _, b := range req.Budgets {
		if b.Index < 0 || b.BudgetMB < 0 {
			return nil, ErrGPUBudgetInvalid
		}
		if seenIndex[b.Index] {
			return nil, ErrGPUBudgetInvalid
		}
		seenIndex[b.Index] = true
	}
	existing, err := s.routes.ServerGPUBudgets(ctx, server.ID)
	if err != nil {
		return nil, err
	}
	existingByIndex := make(map[int]routing.ServerGPUBudget, len(existing))
	for _, b := range existing {
		existingByIndex[b.GPUIndex] = b
	}
	now := s.clock().UTC()
	rows := make([]routing.ServerGPUBudget, 0, len(req.Budgets))
	var snapshot map[int]routing.GPUSample // lazily loaded only if a brand-new index needs it
	for _, b := range req.Budgets {
		row := routing.ServerGPUBudget{ServerID: server.ID, GPUIndex: b.Index, BudgetMB: b.BudgetMB, UpdatedAt: now}
		if prior, ok := existingByIndex[b.Index]; ok {
			row.ExpectedUUID = prior.ExpectedUUID
			row.ExpectedName = prior.ExpectedName
			row.CreatedAt = prior.CreatedAt
		} else {
			if snapshot == nil {
				snapshot = s.latestGPUSnapshotByIndex(ctx, server.ID)
			}
			if g, ok := snapshot[b.Index]; ok {
				row.ExpectedUUID = g.UUID
				row.ExpectedName = g.Name
			}
			row.CreatedAt = now
		}
		rows = append(rows, row)
	}
	if err := s.routes.SetServerGPUBudgets(ctx, server.ID, rows); err != nil {
		return nil, err
	}
	s.notifyRuntimeChanged(server.ID)
	stored, err := s.routes.ServerGPUBudgets(ctx, server.ID)
	if err != nil {
		return nil, err
	}
	return gpuBudgetDTOs(stored), nil
}

// latestGPUSnapshotByIndex reads serverID's single most recent telemetry
// sample (TelemetrySamples with limit=1 returns exactly the newest row --
// DecimateTelemetrySamples' limit==1 case) and indexes its GPU list by
// index, for SetServerGPUBudgets' new-row expected_uuid/expected_name
// snapshot. Best-effort: any read error or the absence of a sample yields an
// empty map (the caller then leaves expected_* empty), mirroring the
// "leave the fields empty rather than failing" contract.
func (s *Service) latestGPUSnapshotByIndex(ctx context.Context, serverID string) map[int]routing.GPUSample {
	out := map[int]routing.GPUSample{}
	samples, err := s.routes.TelemetrySamples(ctx, serverID, time.Time{}, s.clock().UTC(), 1)
	if err != nil || len(samples) == 0 {
		return out
	}
	for _, g := range samples[0].GPUs {
		out[g.Index] = g
	}
	return out
}

// gpuBudgetDTOs builds the wire DTOs from stored rows; always a non-nil
// slice.
func gpuBudgetDTOs(budgets []routing.ServerGPUBudget) []GPUBudgetDTO {
	out := make([]GPUBudgetDTO, 0, len(budgets))
	for _, b := range budgets {
		out = append(out, GPUBudgetDTO{Index: b.GPUIndex, BudgetMB: b.BudgetMB, ExpectedUUID: b.ExpectedUUID, ExpectedName: b.ExpectedName})
	}
	return out
}

// --- Task 6: runtime timeout warning ----------------------------------------

// runtimeTimeoutBelowStartupWarning is the sole warning code RuntimeWarnings
// currently emits: the application's gateway-side upstream deadline
// (TimeoutMS) keeps running while the agent is still starting a model
// process, so a TimeoutMS below the slowest enabled spec's startup timeout
// silently kills every cold start.
const runtimeTimeoutBelowStartupWarning = "timeout_ms_below_startup_timeout"

// RuntimeWarnings is a pure derivation (no store write) of operator-facing
// warnings about appID's current runtime configuration. authorizeApplication
// gates it (404-no-leak). Always a non-nil slice, even when empty.
func (s *Service) RuntimeWarnings(ctx context.Context, principal auth.Token, appID string) ([]string, error) {
	app, _, err := s.authorizeApplication(ctx, principal, appID)
	if err != nil {
		return nil, err
	}
	specs, err := s.routes.RuntimeSpecsByApplication(ctx, app.ID)
	if err != nil {
		return nil, err
	}
	maxStartupSeconds := 0
	for _, spec := range specs {
		if !spec.Enabled {
			continue
		}
		if spec.StartupTimeoutSeconds > maxStartupSeconds {
			maxStartupSeconds = spec.StartupTimeoutSeconds
		}
	}
	warnings := make([]string, 0)
	if maxStartupSeconds > 0 && app.TimeoutMS < maxStartupSeconds*1000 {
		warnings = append(warnings, runtimeTimeoutBelowStartupWarning)
	}
	return warnings, nil
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
