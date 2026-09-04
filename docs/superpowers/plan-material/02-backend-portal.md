# GPU Selection — Backend Portal material (Area 02)

Scope: the `visible_devices_mode` field, args-mode placeholder validation, the
agent-wire DTO, GPU array-order capture (`Position = i`), and surfacing the
agent host OS. All paths are under the worktree
`/Users/jlor08/Developer/codex/op-ai-gateway/.worktrees/gpu-selection`.

All excerpts below were read from the actual files on branch `gpu-selection`
(current origin/main content). Line numbers are as of this reading.

---

## 0. Cross-area coordination summary (read first)

This area **produces** two persisted things that Part A / the store area also
touches, and **consumes** one thing the store area must land first:

- I **own**: `routing.VisibleDevicesMode` type, `RuntimeSpec.VisibleDevicesMode`
  field, the sqlite CRUD for that column (cols list + INSERT + scan), all three
  portal DTOs' `visible_devices_mode` json field, the two new error sentinels +
  their HTTP-400 rows, `validateRuntimeSpecVisibleDevices` extension, the
  device-placeholder tokens the portal scans for, `putRuntimeSpec` setting both
  `VisibleDevicesMode` and `Position = i`, and `AgentOS` on the runtime-report
  DTO.
- I **consume from the store area (Part A)**: `routing.RuntimeSpecGPU.Position int`,
  the `agent_runtime_spec_gpus.position` column (migration 73), the sqlite
  `RuntimeSpecGPUs` read changing `ORDER BY gpu_index` → `ORDER BY position`,
  and the memory store `RuntimeSpecGPUs` sort changing `GPUIndex` → `Position`.
  My `putRuntimeSpec` change (`Position = i`) is inert until those land, so the
  **GPU-order round-trip test (Task B4) must be sequenced after the store area**.
- Migration 73 (the `visible_devices_mode text not null default 'env'` column +
  the `position` column + both backfills) is the **store/migration area's**
  file (`internal/store/migrate.go`). I only add the CRUD read/write of the
  `visible_devices_mode` column; I flag the exact migration coordination in
  Task B0.

---

## 1. CURRENT STATE (exact excerpts + file:line)

### 1.1 `routing.RuntimeSpec` — the struct that gains `VisibleDevicesMode`
`gateway/backend/internal/routing/store.go:1246-1293`. The relevant tail
(`store.go:1266-1293`):

```go
	AdminState                  string // "" | "force_running" | "force_stopped"
	VRAMLocked                  bool
	// SetVisibleDevices asks the agent to CONSTRAIN the child to the cards in
	// this spec's GPU rows ...
	SetVisibleDevices bool
	// APIFlavors / ResponsesMode / MessagesMode ...
	APIFlavors    []string
	ResponsesMode EndpointMode
	MessagesMode  EndpointMode
	CreatedAt     time.Time
	UpdatedAt     time.Time
}
```

`SetVisibleDevices bool` is at `store.go:1281`. `ResponsesMode`/`MessagesMode`
are typed `EndpointMode` (a `type EndpointMode string`) at `store.go:1289-1290`
— the exact pattern to mirror for `VisibleDevicesMode`.

### 1.2 `routing.RuntimeSpecGPU` — Part A adds `Position`; I set it in the portal
`gateway/backend/internal/routing/store.go:1300-1305`:

```go
type RuntimeSpecGPU struct {
	SpecID         string
	GPUIndex       int
	VRAMEstimateMB int
	VRAMMeasuredMB int
}
```

`Position int` is added here by the store area; I consume it (`Position = i`) in
`putRuntimeSpec`.

### 1.3 `EndpointMode` — the exact typed-string enum pattern to mirror
`gateway/backend/internal/routing/endpoint_mode.go:17-23`:

```go
type EndpointMode string

const (
	EndpointModeDisabled    EndpointMode = "disabled"
	EndpointModeTranslate   EndpointMode = "translate"
	EndpointModePassthrough EndpointMode = "passthrough"
)
```

Its DTO-edge validator, `gateway/backend/internal/portal/service_applications.go:1272-1279`:

```go
func validEndpointMode(raw string) (routing.EndpointMode, bool) {
	switch m := routing.EndpointMode(strings.TrimSpace(raw)); m {
	case routing.EndpointModeDisabled, routing.EndpointModeTranslate, routing.EndpointModePassthrough:
		return m, true
	default:
		return "", false
	}
}
```

### 1.4 `ServerTelemetry.OS` — the agent host OS is ALREADY persisted
`gateway/backend/internal/routing/store.go:231-251` has `OS string` at
`store.go:235`. It is written by the telemetry ingest
(`gateway/backend/internal/gateway/agent_ingest.go:1248`:
`OS: strings.TrimSpace(req.OS)`) and round-trips through the sqlite store
(`gateway/backend/internal/store/sqlite_routes.go:557` write, `:845` scan). The
memory store copies the whole struct. **Conclusion: no new store column is
needed for agent OS** — `ServerRuntimeReportView` already reads the telemetry
row and can fill a new `AgentOS` DTO field from `telemetry.OS`.

### 1.5 Portal error sentinels (the block to extend)
`gateway/backend/internal/portal/service_runtime.go:27-76`. The visible-devices
ones (`service_runtime.go:39-50`):

```go
	ErrRuntimeSpecVisibleDevicesNoGPUs = errors.New("runtime_spec.visible_devices_no_gpus")
	// ...
	ErrRuntimeSpecVisibleDevicesConflict = errors.New("runtime_spec.visible_devices_conflict")
```

The `var (...)` block closes at `service_runtime.go:76` with
`ErrRuntimeSpecFlavorInvalid` (`:75`). New sentinels go into this block.

### 1.6 The three DTOs that each gain `visible_devices_mode`
- `RuntimeSpecDTO` at `service_runtime.go:315-349`; `SetVisibleDevices bool json:"set_visible_devices"` at `:339`, tail is `APIFlavors/ResponsesMode/MessagesMode` `:346-348`.
- `PutRuntimeSpecRequest` at `service_runtime.go:355-382`; `SetVisibleDevices` at `:373`.
- `AgentRuntimeSpecDTO` (agent wire) at `service_runtime.go:1296-1318`; `SetVisibleDevices bool json:"set_visible_devices"` at `:1316`, `AdminState string json:"admin_state"` at `:1317`.

Agent-wire counterpart (agent side, another area, must stay field-for-field):
`server-agent/internal/runtime/types.go:38-63`, `Spec` struct. Its doc at
`types.go:36-37` says it "Mirrors the gateway's AgentRuntimeSpecDTO
... field-for-field." `SetVisibleDevices bool json:"set_visible_devices"` at
`types.go:61`. The agent area adds `VisibleDevicesMode` there with the SAME json
key `visible_devices_mode`.

### 1.7 `GetRuntimeSpec` not-configured default branch
`service_runtime.go:399-401`:

```go
	if !ok {
		return RuntimeSpecDTO{MappingID: mapping.ID, Args: []string{}, Env: map[string]string{}, GPUs: []RuntimeSpecGPUDTO{}, APIFlavors: []string{}}, nil
	}
```

`VisibleDevicesMode` is a string, zero value `""` — fine here, or set it
explicitly to `string(routing.VisibleDevicesModeEnv)` for a stable default (the
frontend reads `env` when not configured). Recommend setting it explicitly.

### 1.8 `putRuntimeSpec` — where the spec + GPU rows are built
Validation call at `service_runtime.go:500`:

```go
	if err := validateRuntimeSpecVisibleDevices(req); err != nil {
		return RuntimeSpecDTO{}, err
	}
```

Endpoint-mode defaulting block (the pattern to mirror), `service_runtime.go:508-523`:

```go
	respMode := routing.EndpointModePassthrough
	if strings.TrimSpace(req.ResponsesMode) != "" {
		m, ok := validEndpointMode(req.ResponsesMode)
		if !ok {
			return RuntimeSpecDTO{}, ErrRuntimeSpecEndpointModeInvalid
		}
		respMode = m
	}
	msgMode := routing.EndpointModePassthrough
	if strings.TrimSpace(req.MessagesMode) != "" {
		// ... same shape ...
	}
```

Spec build, `service_runtime.go:574-597` (`SetVisibleDevices: req.SetVisibleDevices` at `:591`):

```go
	spec := routing.RuntimeSpec{
		ID:  "rspec_" + compactRandomHex(16),
		// ...
		SetVisibleDevices:           req.SetVisibleDevices,
		APIFlavors:                  flavors,
		ResponsesMode:               respMode,
		MessagesMode:                msgMode,
		CreatedAt:                   now,
		UpdatedAt:                   now,
	}
```

GPU row build + persist + re-read, `service_runtime.go:605-622`:

```go
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
	// ...
	return runtimeSpecDTO(spec, storedGPUs)
```

The loop at `:606` is where `Position: i` is set (needs an index: `for i, g := range req.GPUs`).

### 1.9 `runtimeSpecVisibleDevicesVars` + `validateRuntimeSpecVisibleDevices`
`service_runtime.go:672-676`:

```go
var runtimeSpecVisibleDevicesVars = []string{
	"CUDA_VISIBLE_DEVICES",
	"ROCR_VISIBLE_DEVICES",
	"HIP_VISIBLE_DEVICES",
}
```

`service_runtime.go:699-716`:

```go
func validateRuntimeSpecVisibleDevices(req PutRuntimeSpecRequest) error {
	if !req.SetVisibleDevices {
		return nil
	}
	// Trap 3 before trap 1, matching the agent's order ...
	for k := range req.Env {
		if slices.Contains(runtimeSpecVisibleDevicesVars, strings.ToUpper(strings.TrimSpace(k))) {
			return ErrRuntimeSpecVisibleDevicesConflict
		}
	}
	// An empty GPU list is not "no restriction" ...
	if len(req.GPUs) == 0 {
		return ErrRuntimeSpecVisibleDevicesNoGPUs
	}
	return nil
}
```

`slices` and `strings` are already imported (`service_runtime.go:6-18`).

### 1.10 `runtimeSpecDTO()` — emits the mode
`service_runtime.go:742-789`. `SetVisibleDevices: spec.SetVisibleDevices` at
`:783`; the DTO literal tail `:785-787`:

```go
		SetVisibleDevices:           spec.SetVisibleDevices,
		GPUs:                        gpuDTOs,
		APIFlavors:                  append([]string{}, spec.APIFlavors...),
		ResponsesMode:               string(spec.ResponsesMode),
		MessagesMode:                string(spec.MessagesMode),
	}, nil
```

### 1.11 `agentRuntimeSpecDTO()` — the agent-wire builder; emits the mode + GPU order
`service_runtime.go:1543-1588`. The GPU loop (`:1558-1568`) iterates the
`gpus []routing.RuntimeSpecGPU` slice **in the order the store returned it** —
so once the store reads `ORDER BY position`, the agent wire is in position
order automatically. `SetVisibleDevices: spec.SetVisibleDevices` at `:1585`:

```go
	gpuDTOs := make([]AgentRuntimeSpecGPUDTO, 0, len(gpus))
	for _, g := range gpus {
		// ...
		gpuDTOs = append(gpuDTOs, AgentRuntimeSpecGPUDTO{Index: g.GPUIndex, VRAMMB: vram})
	}
	return AgentRuntimeSpecDTO{
		// ...
		Pinned:                      spec.Pinned,
		SetVisibleDevices:           spec.SetVisibleDevices,
		AdminState:                  spec.AdminState,
	}, nil
```

The GPUs handed here come from `AgentRuntimeConfig` at `service_runtime.go:1484`
(`gpus, err := s.routes.RuntimeSpecGPUs(ctx, spec.ID)`), i.e. store order.

**Guard note:** `TestAgentRuntimeConfigOmitsFlavorsAndModes`
(`service_runtime_test.go:343-372`) marshals the agent config and fails if the
raw JSON `strings.Contains` `"api_flavors"`, `"responses_mode"`, or
`"messages_mode"`. The new key `visible_devices_mode` does **not** collide with
any of those substrings, so adding it to the agent wire is safe. (Unlike the
endpoint modes, the agent NEEDS `visible_devices_mode` to decide env injection.)

### 1.12 `ServerRuntimeReportViewDTO` + `ServerRuntimeReportView()` — agent OS
`service_runtime.go:1640-1647`:

```go
type ServerRuntimeReportViewDTO struct {
	Available     bool            `json:"available"`
	CollectedAt   string          `json:"collected_at,omitempty"`
	UpdatedAt     string          `json:"updated_at,omitempty"`
	Report        json.RawMessage `json:"report,omitempty"`
	AgentVersion  string          `json:"agent_version"`
	AgentFeatures []string        `json:"agent_features"`
}
```

`service_runtime.go:1659-1665` (telemetry read; add `dto.AgentOS = telemetry.OS`):

```go
	dto := ServerRuntimeReportViewDTO{AgentFeatures: []string{}}
	if telemetry, ok, err := s.routes.TelemetryByServer(ctx, server.ID); err != nil {
		return ServerRuntimeReportViewDTO{}, err
	} else if ok {
		dto.AgentVersion = telemetry.AgentVersion
		dto.AgentFeatures = parseRuntimeReportAgentFeatures(telemetry.Capabilities)
	}
```

### 1.13 HTTP 400 mapping table
`gateway/backend/internal/gateway/portal_runtime_endpoints.go:68-86`,
`portalRuntimeSpecErrRows`. Existing visible-devices rows `:76-77`:

```go
	{err: portal.ErrRuntimeSpecVisibleDevicesNoGPUs, status: http.StatusBadRequest, code: "runtime_spec.visible_devices_no_gpus", msg: "set_visible_devices requires at least one gpu row: an empty visible-devices value hides every gpu from the model"},
	{err: portal.ErrRuntimeSpecVisibleDevicesConflict, status: http.StatusBadRequest, code: "runtime_spec.visible_devices_conflict", msg: "set_visible_devices conflicts with a gpu visibility variable set by hand in env"},
```

Mapper is `writePortalRuntimeSpecError` (`portal_runtime_endpoints.go:88-90`).

### 1.14 sqlite CRUD for `agent_runtime_specs` (the column read/write)
`gateway/backend/internal/store/sqlite_runtime.go`. FOUR exact spots:

1. **INSERT + on-conflict**, `UpsertRuntimeSpec` (`sqlite_runtime.go:20-66`):
   column list `:26-31` currently ends `... vram_locked, set_visible_devices, api_flavors, responses_mode, messages_mode, created_at, updated_at`; the VALUES tuple is 22 `?` at `:32`; the on-conflict `do update set` at `:33-46`; the exec args at `:47-52` (note `string(spec.ResponsesMode), string(spec.MessagesMode)` at `:51`).
2. **`runtimeSpecCols` const**, `sqlite_runtime.go:68-72`:
   ```go
   const runtimeSpecCols = `id, mapping_id, enabled, binary_path, args, env, work_dir,
   	listen_port, health_path, health_timeout_seconds, startup_timeout_seconds,
   	idle_timeout_seconds, admission_wait_timeout_seconds, pinned, admin_state,
   	vram_locked, set_visible_devices, api_flavors, responses_mode, messages_mode,
   	created_at, updated_at`
   ```
3. **`runtimeSpecColsPrefixed` const**, `sqlite_runtime.go:78-82` (same list, `s.`-qualified, used by the `RuntimeSpecsByApplication` join).
4. **`scanRuntimeSpec`**, `sqlite_runtime.go:339-364`:
   ```go
   err := row.Scan(&spec.ID, &spec.MappingID, &enabled, &spec.Binary, &spec.Args,
   	&spec.Env, &spec.WorkDir, &spec.ListenPort, &spec.HealthPath,
   	&spec.HealthTimeoutSeconds, &spec.StartupTimeoutSeconds,
   	&spec.IdleTimeoutSeconds, &spec.AdmissionWaitTimeoutSeconds, &pinned,
   	&spec.AdminState, &vramLocked, &setVisibleDevices,
   	&apiFlavors, &spec.ResponsesMode, &spec.MessagesMode,
   	&spec.CreatedAt, &spec.UpdatedAt)
   ```
   `&spec.ResponsesMode` scans a text column straight into a `routing.EndpointMode`
   (database/sql's reflect fallback assigns a string to any String-kind type), so
   `&spec.VisibleDevicesMode` (a `*routing.VisibleDevicesMode`) scans identically —
   no `sql.Scanner`, no intermediate string var needed.

The **column list order must match the scan order.** Append
`visible_devices_mode` in the SAME position in all three (cols const, prefixed
const, scan) — recommend right after `messages_mode` and before
`created_at, updated_at`, and `&spec.VisibleDevicesMode` right after
`&spec.MessagesMode`. The SELECTs use explicit column lists (never `select *`),
so the physical column position from the ALTER does not matter — only
cols-list ↔ scan-args correspondence.

The memory store needs **no change for the mode**: `UpsertRuntimeSpec`
(`memory_store.go:2251`) stores the struct via `copyRuntimeSpec`
(`memory_store.go:2140-2143`), which only special-cases the `APIFlavors` slice;
`VisibleDevicesMode` (a value string) is copied by the plain struct copy.

### 1.15 GPU-row store reads (Part A territory, cited for coordination)
- sqlite `RuntimeSpecGPUs` `ORDER BY gpu_index`: `sqlite_runtime.go:180-182`.
- sqlite `SetRuntimeSpecGPUs` INSERT (no `position` yet): `sqlite_runtime.go:161-163`.
- memory `RuntimeSpecGPUs` `sort.Slice ... GPUIndex`: `memory_store.go:2360-2366` (sort at `:2364`).

### 1.16 Migration numbering (store area; cited so the plan can pin 73)
`internal/store/migrate.go` migration list ends at
`{version: 72, name: "application_endpoint_modes", up: migration72Up}` (`migrate.go:105`).
**Next number is 73.** The base `agent_runtime_specs` CREATE
(`migrate.go:2860-2878`) is migration 65 and does NOT list `set_visible_devices`
or the mode columns — every post-65 column arrives via an appended ALTER
migration. Closest analog to copy for the `visible_devices_mode text not null
default 'env'` column: `migration72Up` (`migrate.go:3137+`) which does
`addColumnIfMissing(... "responses_mode text not null default ''")` then a
guarded backfill `update ... where responses_mode = ''`. The mode column's
default `'env'` means the backfill is a no-op (fresh installs and upgrades both
get `'env'` from the column default), so migration 73 only needs the two
`addColumnIfMissing` calls plus the `position` backfill (Part A).

---

## 2. PROPOSED TDD TASKS (ordered, real code)

Test framework: **Go stdlib `testing`**, table-driven, no testify. Portal
service tests use the memory store via `newServerTestService(t, now)`
(`service_test.go:1574`) → `(*Service, *routing.MemoryStore)`, principal
`ownerToken()` (`service_test.go:1610`), fixtures `createTestServer`
(`service_applications_test.go:21`), `seedServerAgentApplication`
(`service_runtime_test.go:30`), `seedVisibleDevicesMapping`
(`service_runtime_test.go:1792`). HTTP tests use `NewTestServer()` +
`newJSONRequest` + `errorBodyOf(t, rec)` in package `gateway`.

Run commands:
- Portal:  `cd gateway/backend && go test ./internal/portal/...`
- HTTP:    `cd gateway/backend && go test ./internal/gateway/...`
- Store:   `cd gateway/backend && go test ./internal/store/... ./internal/routing/...`

---

### Task B0 — `routing.VisibleDevicesMode` type + `RuntimeSpec` field + sqlite CRUD

**Scaffolding folded in.** No behavioural test of its own; verified end-to-end
by B1's round-trip. Add a routing-level store round-trip assertion too.

**New file** `gateway/backend/internal/routing/visible_devices_mode.go`:

```go
// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package routing

// VisibleDevicesMode is the per-spec choice of HOW set_visible_devices is
// enforced when it is on: via the vendor visibility ENVIRONMENT variable
// (today's mechanism) or via a backend device placeholder the operator writes
// into the spec's ARGS (llama.cpp --device). Only meaningful when
// SetVisibleDevices is on; the default, and every pre-feature row, is "env".
// Serialized as its lowercase string, stored text (mirrors EndpointMode).
//
// Validated at the DTO edge (portal.validVisibleDevicesMode) rather than by a
// method here, exactly like EndpointMode.
type VisibleDevicesMode string

const (
	VisibleDevicesModeEnv  VisibleDevicesMode = "env"
	VisibleDevicesModeArgs VisibleDevicesMode = "args"
)
```

**Edit** `routing/store.go` — add the field to `RuntimeSpec` after
`SetVisibleDevices bool` (`store.go:1281`):

```go
	SetVisibleDevices bool
	// VisibleDevicesMode selects HOW SetVisibleDevices is enforced: "env"
	// (inject the vendor visibility variable, today's behavior) or "args" (the
	// agent expands a ${..._DEVICES} placeholder in Args and injects no env
	// var). Only meaningful when SetVisibleDevices is on; default "env".
	VisibleDevicesMode VisibleDevicesMode
```

**Edit** `internal/store/sqlite_runtime.go` — the four spots from §1.14:
- `runtimeSpecCols` (`:68-72`): insert `visible_devices_mode` after `messages_mode`.
- `runtimeSpecColsPrefixed` (`:78-82`): insert `s.visible_devices_mode` after `s.messages_mode`.
- `UpsertRuntimeSpec` INSERT column list (`:26-31`): add `visible_devices_mode` after `messages_mode`; add one `?` to the VALUES tuple (`:32`, 22 → 23); add `visible_devices_mode = excluded.visible_devices_mode` to `do update set` (`:45` area); add `string(spec.VisibleDevicesMode)` to the exec args after `string(spec.MessagesMode)` (`:51`).
- `scanRuntimeSpec` (`:343-349`): add `&spec.VisibleDevicesMode,` right after `&spec.MessagesMode,`.

**Store-area coordination for the failing test to compile/pass:** migration 73
must add the `visible_devices_mode text not null default 'env'` column, or an
upgraded DB scan hits a missing column. For a fresh in-memory sqlite (what the
conformance test opens) the ALTER runs on boot. Pin the round-trip in the
**store conformance test** (`internal/store/conformance_test.go`) by extending
the existing RuntimeSpec block (`conformance_test.go:7681-7704`): add
`VisibleDevicesMode: routing.VisibleDevicesModeArgs,` to the seeded `spec`
(`:7681-7691`) and to the mismatch assertion (`:7699-7703`) add
`got.VisibleDevicesMode != routing.VisibleDevicesModeArgs`. This runs on
memory + sqlite (+ postgres when the DSN is set).

Run: `cd gateway/backend && go test ./internal/store/... -run RuntimeSpec`
Expect: fail before the CRUD edit (`got.VisibleDevicesMode == ""`), pass after.

---

### Task B1 — three DTOs carry `visible_devices_mode`; `putRuntimeSpec` sets it; both builders emit it

**Failing test** — append to `internal/portal/service_runtime_test.go`:

```go
// TestRuntimeSpecDTOCarriesVisibleDevicesMode pins that RuntimeSpecDTO /
// PutRuntimeSpecRequest / the agent-wire AgentRuntimeSpecDTO all carry
// visible_devices_mode, that PutRuntimeSpec+GetRuntimeSpec round-trip it, and
// that an omitted mode defaults to "env".
func TestRuntimeSpecDTOCarriesVisibleDevicesMode(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	svc, routeStore := newServerTestService(t, now)
	server := createTestServer(t, svc, "S", "s.example.test")
	app := seedServerAgentApplication(t, routeStore, server.ID, now)
	mapping, err := svc.CreateMapping(ctx, ownerToken(), app.ID, CreateMappingRequest{GatewayModelName: "qwen", AppModelName: "qwen-upstream"})
	if err != nil {
		t.Fatalf("CreateMapping: %v", err)
	}

	// args mode round-trips (with a placeholder present so validation passes).
	dto, err := svc.PutRuntimeSpec(ctx, ownerToken(), mapping.ID, PutRuntimeSpecRequest{
		Enabled:            true,
		Binary:             "/usr/local/bin/llama-server",
		Args:               []string{"--device", "${CUDA_DEVICES}"},
		SetVisibleDevices:  true,
		VisibleDevicesMode: string(routing.VisibleDevicesModeArgs),
		GPUs:               []RuntimeSpecGPUDTO{{Index: 2, VRAMEstimateMB: 8000}},
	})
	if err != nil {
		t.Fatalf("PutRuntimeSpec: %v", err)
	}
	if dto.VisibleDevicesMode != string(routing.VisibleDevicesModeArgs) {
		t.Fatalf("dto.VisibleDevicesMode = %q, want args", dto.VisibleDevicesMode)
	}
	got, err := svc.GetRuntimeSpec(ctx, ownerToken(), mapping.ID)
	if err != nil {
		t.Fatalf("GetRuntimeSpec: %v", err)
	}
	if got.VisibleDevicesMode != string(routing.VisibleDevicesModeArgs) {
		t.Fatalf("GetRuntimeSpec().VisibleDevicesMode = %q, want args", got.VisibleDevicesMode)
	}

	// The agent-wire document carries the mode too (the agent needs it to
	// decide whether to inject the visibility env var).
	cfg, err := svc.AgentRuntimeConfig(ctx, server.ID)
	if err != nil {
		t.Fatalf("AgentRuntimeConfig: %v", err)
	}
	if len(cfg.Specs) != 1 || cfg.Specs[0].VisibleDevicesMode != string(routing.VisibleDevicesModeArgs) {
		t.Fatalf("agent wire dropped the mode: %#v", cfg.Specs)
	}

	// An omitted mode defaults to env.
	def, err := svc.PutRuntimeSpec(ctx, ownerToken(), mapping.ID, PutRuntimeSpecRequest{
		Enabled: true, Binary: "/usr/local/bin/llama-server",
	})
	if err != nil {
		t.Fatalf("PutRuntimeSpec (default): %v", err)
	}
	if def.VisibleDevicesMode != string(routing.VisibleDevicesModeEnv) {
		t.Fatalf("default mode = %q, want env", def.VisibleDevicesMode)
	}
}
```

Run: `cd gateway/backend && go test ./internal/portal/... -run TestRuntimeSpecDTOCarriesVisibleDevicesMode`
Expect: fail (field does not exist / defaults to "") before impl, pass after.

**Minimal impl** — `internal/portal/service_runtime.go`:

1. `RuntimeSpecDTO` (after `MessagesMode string json:"messages_mode"` at `:348`):
   ```go
   	// VisibleDevicesMode is "env" | "args": how set_visible_devices is
   	// enforced. Only meaningful when SetVisibleDevices is on; default "env".
   	VisibleDevicesMode string `json:"visible_devices_mode"`
   ```
2. `PutRuntimeSpecRequest` (after `MessagesMode string json:"messages_mode"` at `:381`): same line.
3. `AgentRuntimeSpecDTO` (after `SetVisibleDevices bool json:"set_visible_devices"` at `:1316`):
   ```go
   	// VisibleDevicesMode tells the agent whether to enforce visibility via the
   	// env variable ("env") or to leave it to a ${..._DEVICES} placeholder the
   	// operator put in Args ("args"). Unlike api_flavors/responses_mode, the
   	// agent NEEDS this, so it DOES cross the wire.
   	VisibleDevicesMode string `json:"visible_devices_mode"`
   ```
4. `GetRuntimeSpec` not-configured branch (`:400`): add
   `VisibleDevicesMode: string(routing.VisibleDevicesModeEnv),` to the returned literal.
5. `putRuntimeSpec` — add a defaulting block mirroring the endpoint modes,
   after the endpoint-mode block (`:508-523`) and before the spec build:
   ```go
   	visibleMode := routing.VisibleDevicesModeEnv
   	if strings.TrimSpace(req.VisibleDevicesMode) != "" {
   		// Already validated by validateRuntimeSpecVisibleDevices above; this
   		// only resolves the stored typed value.
   		visibleMode = routing.VisibleDevicesMode(strings.TrimSpace(req.VisibleDevicesMode))
   	}
   ```
   and set it in the spec literal (`:591` area):
   ```go
   		SetVisibleDevices:           req.SetVisibleDevices,
   		VisibleDevicesMode:          visibleMode,
   ```
6. `runtimeSpecDTO()` return literal (`:783-787`): add
   `VisibleDevicesMode: string(spec.VisibleDevicesMode),`.
7. `agentRuntimeSpecDTO()` return literal (`:1585` area): add
   `VisibleDevicesMode: string(spec.VisibleDevicesMode),`.

Depends on Task B0 (the `routing.VisibleDevicesMode` type + store field).

---

### Task B2 — mode validation + args-mode placeholder requirement

**Failing test** — append to `service_runtime_test.go` (table-driven, the
`TestPutRuntimeSpecValidation` shape at `:138-230`):

```go
// TestPutRuntimeSpecVisibleDevicesModeValidation pins the two new mode traps:
// an invalid mode value, and args mode with no ${..._DEVICES} placeholder in
// args. The conflict + no-gpus traps still fire in BOTH modes.
func TestPutRuntimeSpecVisibleDevicesModeValidation(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	svc, routeStore := newServerTestService(t, now)
	mappingID := seedVisibleDevicesMapping(t, svc, routeStore, now)

	base := func() PutRuntimeSpecRequest {
		return PutRuntimeSpecRequest{
			Binary:            "/usr/local/bin/llama-server",
			SetVisibleDevices: true,
			GPUs:              []RuntimeSpecGPUDTO{{Index: 0, VRAMEstimateMB: 8000}},
		}
	}
	cases := []struct {
		name    string
		mutate  func(PutRuntimeSpecRequest) PutRuntimeSpecRequest
		wantErr error
	}{
		{
			name:    "bad mode value",
			mutate:  func(r PutRuntimeSpecRequest) PutRuntimeSpecRequest { r.VisibleDevicesMode = "bogus"; return r },
			wantErr: ErrRuntimeSpecVisibleDevicesModeInvalid,
		},
		{
			name: "args mode with no device placeholder",
			mutate: func(r PutRuntimeSpecRequest) PutRuntimeSpecRequest {
				r.VisibleDevicesMode = string(routing.VisibleDevicesModeArgs)
				r.Args = []string{"--ctx-size", "4096"}
				return r
			},
			wantErr: ErrRuntimeSpecVisibleDevicesArgsNoPlaceholder,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := svc.PutRuntimeSpec(ctx, ownerToken(), mappingID, tc.mutate(base())); !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
		})
	}

	// Each of the three placeholder tokens satisfies args mode.
	for _, ph := range []string{"${CUDA_DEVICES}", "${VULKAN_DEVICES}", "${METAL_DEVICES}"} {
		t.Run("accepts "+ph, func(t *testing.T) {
			r := base()
			r.VisibleDevicesMode = string(routing.VisibleDevicesModeArgs)
			r.Args = []string{"--device", ph}
			if _, err := svc.PutRuntimeSpec(ctx, ownerToken(), mappingID, r); err != nil {
				t.Fatalf("args mode with %s must save: %v", ph, err)
			}
		})
	}

	// The conflict trap still fires in args mode (a hand-set CUDA_VISIBLE_DEVICES
	// remaps the CUDA namespace and would break --device numbering).
	t.Run("conflict trap holds in args mode", func(t *testing.T) {
		r := base()
		r.VisibleDevicesMode = string(routing.VisibleDevicesModeArgs)
		r.Args = []string{"--device", "${CUDA_DEVICES}"}
		r.Env = map[string]string{"CUDA_VISIBLE_DEVICES": "0,1"}
		if _, err := svc.PutRuntimeSpec(ctx, ownerToken(), mappingID, r); !errors.Is(err, ErrRuntimeSpecVisibleDevicesConflict) {
			t.Fatalf("err = %v, want ErrRuntimeSpecVisibleDevicesConflict", err)
		}
	})

	// Mode is inert when the option is OFF: a bogus mode with the flag off is
	// still rejected as a malformed enum, but args-with-no-placeholder is NOT
	// (the placeholder requirement only applies with the flag on).
	t.Run("args no-placeholder ok when flag off", func(t *testing.T) {
		r := base()
		r.SetVisibleDevices = false
		r.VisibleDevicesMode = string(routing.VisibleDevicesModeArgs)
		r.Args = []string{"--ctx-size", "4096"}
		if _, err := svc.PutRuntimeSpec(ctx, ownerToken(), mappingID, r); err != nil {
			t.Fatalf("args mode with flag off must save: %v", err)
		}
	})
}
```

Run: `cd gateway/backend && go test ./internal/portal/... -run TestPutRuntimeSpecVisibleDevicesModeValidation`
Expect: fail (sentinels undefined) before impl, pass after.

**Minimal impl** — `internal/portal/service_runtime.go`:

1. Sentinels, into the `var (...)` block near `:75` (after `ErrRuntimeSpecFlavorInvalid`):
   ```go
   	// ErrRuntimeSpecVisibleDevicesModeInvalid rejects a visible_devices_mode
   	// that is not "env" or "args". HTTP 400.
   	ErrRuntimeSpecVisibleDevicesModeInvalid = errors.New("runtime_spec.visible_devices_mode_invalid")
   	// ErrRuntimeSpecVisibleDevicesArgsNoPlaceholder rejects set_visible_devices
   	// in args mode when none of the three device placeholders
   	// (${CUDA_DEVICES}/${VULKAN_DEVICES}/${METAL_DEVICES}) appears in args: the
   	// agent would inject no visibility and the selection would be lost. HTTP 400.
   	ErrRuntimeSpecVisibleDevicesArgsNoPlaceholder = errors.New("runtime_spec.visible_devices_args_no_placeholder")
   ```
2. Device-placeholder tokens + helper, near `runtimeSpecVisibleDevicesVars` (`:672-676`):
   ```go
   // runtimeSpecDevicePlaceholders are the three llama.cpp --device placeholders
   // the agent expands (mirrors the agent's own token set in policy_local.go).
   // In args mode at least one must appear in the spec's args, else the
   // selection would silently vanish.
   var runtimeSpecDevicePlaceholders = []string{
   	"${CUDA_DEVICES}",
   	"${VULKAN_DEVICES}",
   	"${METAL_DEVICES}",
   }

   func argsHaveDevicePlaceholder(args []string) bool {
   	for _, a := range args {
   		for _, ph := range runtimeSpecDevicePlaceholders {
   			if strings.Contains(a, ph) {
   				return true
   			}
   		}
   	}
   	return false
   }

   // validVisibleDevicesMode validates a raw visible_devices_mode at the DTO
   // edge (mirrors validEndpointMode). Empty is valid and defaults to "env" in
   // putRuntimeSpec; any other non-env/args value is rejected.
   func validVisibleDevicesMode(raw string) (routing.VisibleDevicesMode, bool) {
   	switch m := routing.VisibleDevicesMode(strings.TrimSpace(raw)); m {
   	case "", routing.VisibleDevicesModeEnv, routing.VisibleDevicesModeArgs:
   		return m, true
   	default:
   		return "", false
   	}
   }
   ```
3. Extend `validateRuntimeSpecVisibleDevices` (`:699-716`) — validate the mode
   value ALWAYS (a malformed enum is a malformed request), then the gated traps,
   then the args-mode placeholder trap:
   ```go
   func validateRuntimeSpecVisibleDevices(req PutRuntimeSpecRequest) error {
   	// The mode value is validated regardless of the flag: a malformed enum is
   	// a malformed request. Empty defaults to "env" (resolved in putRuntimeSpec).
   	mode, ok := validVisibleDevicesMode(req.VisibleDevicesMode)
   	if !ok {
   		return ErrRuntimeSpecVisibleDevicesModeInvalid
   	}
   	if !req.SetVisibleDevices {
   		return nil
   	}
   	// Trap 3 before trap 1, matching the agent's order ...
   	for k := range req.Env {
   		if slices.Contains(runtimeSpecVisibleDevicesVars, strings.ToUpper(strings.TrimSpace(k))) {
   			return ErrRuntimeSpecVisibleDevicesConflict
   		}
   	}
   	if len(req.GPUs) == 0 {
   		return ErrRuntimeSpecVisibleDevicesNoGPUs
   	}
   	// args mode: at least one device placeholder must be present, else the
   	// agent injects nothing and the selection is silently lost.
   	if mode == routing.VisibleDevicesModeArgs && !argsHaveDevicePlaceholder(req.Args) {
   		return ErrRuntimeSpecVisibleDevicesArgsNoPlaceholder
   	}
   	return nil
   }
   ```

Note: `mode == VisibleDevicesModeArgs` treats empty (`""`) as env, so an
env-mode/default spec never hits the placeholder trap. Depends on B0 + B1.

---

### Task B3 — HTTP 400 mapping for the two new sentinels

**Failing test** — append to
`internal/gateway/portal_runtime_endpoints_test.go` (mirrors
`TestHandlePortalMappingRuntimeSpecPutBadEndpointModeReturns400` at `:117-129`):

```go
func TestHandlePortalMappingRuntimeSpecPutBadVisibleDevicesModeReturns400(t *testing.T) {
	srv := NewTestServer()
	mappingID := seedRuntimeSpecMapping(t, srv)
	body := `{"binary":"/usr/local/bin/llama-server","set_visible_devices":true,"visible_devices_mode":"bogus","gpus":[{"index":0}]}`
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, newJSONRequest(http.MethodPut, "/api/portal/mappings/"+mappingID+"/runtime-spec", body))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body = %s", rec.Code, rec.Body.String())
	}
	if code := errorBodyOf(t, rec); code != "runtime_spec.visible_devices_mode_invalid" {
		t.Fatalf("error code = %q, want runtime_spec.visible_devices_mode_invalid", code)
	}
}

func TestHandlePortalMappingRuntimeSpecPutArgsModeNoPlaceholderReturns400(t *testing.T) {
	srv := NewTestServer()
	mappingID := seedRuntimeSpecMapping(t, srv)
	body := `{"binary":"/usr/local/bin/llama-server","set_visible_devices":true,"visible_devices_mode":"args","args":["--ctx-size","4096"],"gpus":[{"index":0}]}`
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, newJSONRequest(http.MethodPut, "/api/portal/mappings/"+mappingID+"/runtime-spec", body))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body = %s", rec.Code, rec.Body.String())
	}
	if code := errorBodyOf(t, rec); code != "runtime_spec.visible_devices_args_no_placeholder" {
		t.Fatalf("error code = %q, want runtime_spec.visible_devices_args_no_placeholder", code)
	}
}
```

Run: `cd gateway/backend && go test ./internal/gateway/... -run TestHandlePortalMappingRuntimeSpecPut`
Expect: fail (falls through to 500 `runtime_spec.request_failed`) before impl,
pass after.

**Minimal impl** — add two rows to `portalRuntimeSpecErrRows`
(`gateway/backend/internal/gateway/portal_runtime_endpoints.go:68-86`), after
the existing visible-devices rows (`:77`):

```go
	{err: portal.ErrRuntimeSpecVisibleDevicesModeInvalid, status: http.StatusBadRequest, code: "runtime_spec.visible_devices_mode_invalid", msg: "visible_devices_mode must be \"env\" or \"args\""},
	{err: portal.ErrRuntimeSpecVisibleDevicesArgsNoPlaceholder, status: http.StatusBadRequest, code: "runtime_spec.visible_devices_args_no_placeholder", msg: "args mode requires one of ${CUDA_DEVICES}/${VULKAN_DEVICES}/${METAL_DEVICES} in args"},
```

Depends on B2 (the sentinels). `portal` is already imported here.

---

### Task B4 — GPU array-order capture (`Position = i`) + agent-wire order round-trip

**DEPENDS ON the store area (Part A)**: `routing.RuntimeSpecGPU.Position int`,
sqlite `RuntimeSpecGPUs` `ORDER BY position`, memory `RuntimeSpecGPUs` sort by
`Position`, migration 73 `position` column. Sequence this task AFTER those.

**Failing test** — append to `service_runtime_test.go`:

```go
// TestPutRuntimeSpecPreservesGPUArrayOrder pins that the request array order of
// gpus is the stored + response + agent-wire order (not re-sorted by index).
func TestPutRuntimeSpecPreservesGPUArrayOrder(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	svc, routeStore := newServerTestService(t, now)
	server := createTestServer(t, svc, "S", "s.example.test")
	app := seedServerAgentApplication(t, routeStore, server.ID, now)
	mapping, err := svc.CreateMapping(ctx, ownerToken(), app.ID, CreateMappingRequest{GatewayModelName: "m", AppModelName: "m"})
	if err != nil {
		t.Fatalf("CreateMapping: %v", err)
	}
	// Deliberately NOT ascending by index.
	dto, err := svc.PutRuntimeSpec(ctx, ownerToken(), mapping.ID, PutRuntimeSpecRequest{
		Enabled: true, Binary: "/usr/local/bin/llama-server",
		GPUs: []RuntimeSpecGPUDTO{{Index: 5, VRAMEstimateMB: 1}, {Index: 2, VRAMEstimateMB: 2}, {Index: 3, VRAMEstimateMB: 3}},
	})
	if err != nil {
		t.Fatalf("PutRuntimeSpec: %v", err)
	}
	wantOrder := []int{5, 2, 3}
	gotOrder := func(gs []RuntimeSpecGPUDTO) []int {
		out := make([]int, len(gs))
		for i, g := range gs {
			out[i] = g.Index
		}
		return out
	}
	if o := gotOrder(dto.GPUs); !slicesEqualInt(o, wantOrder) {
		t.Fatalf("response gpu order = %v, want %v", o, wantOrder)
	}
	got, err := svc.GetRuntimeSpec(ctx, ownerToken(), mapping.ID)
	if err != nil {
		t.Fatalf("GetRuntimeSpec: %v", err)
	}
	if o := gotOrder(got.GPUs); !slicesEqualInt(o, wantOrder) {
		t.Fatalf("read-back gpu order = %v, want %v", o, wantOrder)
	}
	cfg, err := svc.AgentRuntimeConfig(ctx, server.ID)
	if err != nil {
		t.Fatalf("AgentRuntimeConfig: %v", err)
	}
	agentOrder := make([]int, len(cfg.Specs[0].GPUs))
	for i, g := range cfg.Specs[0].GPUs {
		agentOrder[i] = g.Index
	}
	if !slicesEqualInt(agentOrder, wantOrder) {
		t.Fatalf("agent-wire gpu order = %v, want %v", agentOrder, wantOrder)
	}
}

func slicesEqualInt(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
```

(If a shared int-slice-equal helper already exists in the package, drop the
local `slicesEqualInt` and use it.)

Run: `cd gateway/backend && go test ./internal/portal/... -run TestPutRuntimeSpecPreservesGPUArrayOrder`
Expect: fail (order comes back `2,3,5` — re-sorted) before both the store
change AND the `Position = i` edit; pass once both land.

**Minimal impl (this area's half)** — `putRuntimeSpec` GPU loop
(`service_runtime.go:605-613`): add the index and set `Position`:

```go
	gpuRows := make([]routing.RuntimeSpecGPU, 0, len(req.GPUs))
	for i, g := range req.GPUs {
		gpuRows = append(gpuRows, routing.RuntimeSpecGPU{
			SpecID:         spec.ID,
			GPUIndex:       g.Index,
			Position:       i, // request array order becomes the stored order
			VRAMEstimateMB: g.VRAMEstimateMB,
			VRAMMeasuredMB: measuredByIndex[g.Index],
		})
	}
```

The other half (`RuntimeSpecGPU.Position` field, both store reads' ordering,
migration 73) is the store area's. This test is the integration checkpoint that
both halves are in.

---

### Task B5 — surface the agent host OS on the runtime-report DTO

`ServerTelemetry.OS` is already persisted (§1.4) — this is pure DTO plumbing, no
store change.

**Failing test** — append to `service_runtime_test.go` (mirrors
`TestServerRuntimeReportViewFound` at `:1531-1566`):

```go
// TestServerRuntimeReportViewCarriesAgentOS pins that the runtime-report view
// surfaces the agent host OS from the latest telemetry row (used by the portal
// "${METAL_DEVICES} only on macOS" hint).
func TestServerRuntimeReportViewCarriesAgentOS(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	svc, routeStore := newServerTestService(t, now)
	server := createTestServer(t, svc, "S", "s.example.test")
	if err := routeStore.UpsertTelemetry(ctx, routing.ServerTelemetry{
		ServerID: server.ID, ReportedAt: now, AgentVersion: "0.4.0", OS: "linux",
		Capabilities: `{"features":["gpu_selection"]}`, ProviderHealth: "{}", RawSummary: "{}", UpdatedAt: now,
	}); err != nil {
		t.Fatalf("UpsertTelemetry: %v", err)
	}
	dto, err := svc.ServerRuntimeReportView(ctx, ownerToken(), server.ID)
	if err != nil {
		t.Fatalf("ServerRuntimeReportView: %v", err)
	}
	if dto.AgentOS != "linux" {
		t.Fatalf("agent_os = %q, want linux", dto.AgentOS)
	}
}
```

Run: `cd gateway/backend && go test ./internal/portal/... -run TestServerRuntimeReportViewCarriesAgentOS`
Expect: fail (field missing) before impl, pass after.

**Minimal impl** — `internal/portal/service_runtime.go`:
1. `ServerRuntimeReportViewDTO` (after `AgentVersion string json:"agent_version"` at `:1645`):
   ```go
   	AgentOS       string          `json:"agent_os"`
   ```
2. `ServerRuntimeReportView` telemetry branch (`:1662-1665`): add
   `dto.AgentOS = telemetry.OS` alongside `dto.AgentVersion = telemetry.AgentVersion`.

Frontend area then adds `agent_os` to `RuntimeReport`
(`gateway/frontend/src/api/runtime.ts:305-311`) and the Metal-on-non-macOS hint.

---

## 3. INTERFACES

### PRODUCES (other areas consume these exact names)
- `routing.VisibleDevicesMode` (`type VisibleDevicesMode string`) with consts
  `VisibleDevicesModeEnv = "env"` / `VisibleDevicesModeArgs = "args"`
  (`gateway/backend/internal/routing/visible_devices_mode.go`, new).
- `routing.RuntimeSpec.VisibleDevicesMode VisibleDevicesMode`
  (`routing/store.go`, after `SetVisibleDevices`).
- Persisted column `agent_runtime_specs.visible_devices_mode text not null
  default 'env'` (column read/written by this area; the ALTER lands in
  migration 73 in the store/migration area).
- DTO json key `visible_devices_mode` (string) on all three portal DTOs:
  `RuntimeSpecDTO`, `PutRuntimeSpecRequest`, `AgentRuntimeSpecDTO`.
- **Agent-wire contract:** `AgentRuntimeSpecDTO.VisibleDevicesMode string
  json:"visible_devices_mode"` — the agent area must add the same field/key to
  `runtime.Spec` (`server-agent/internal/runtime/types.go:38-63`), which is
  documented to mirror `AgentRuntimeSpecDTO` field-for-field. Agent-wire GPUs
  are already in store order → position order once Part A lands.
- Error sentinels (portal, in `service_runtime.go`) + their API codes:
  - `ErrRuntimeSpecVisibleDevicesModeInvalid` → `"runtime_spec.visible_devices_mode_invalid"` → HTTP 400.
  - `ErrRuntimeSpecVisibleDevicesArgsNoPlaceholder` → `"runtime_spec.visible_devices_args_no_placeholder"` → HTTP 400.
- Device-placeholder tokens the portal scans for (must match the agent's expand
  tokens exactly): `${CUDA_DEVICES}`, `${VULKAN_DEVICES}`, `${METAL_DEVICES}`
  (`runtimeSpecDevicePlaceholders` in `service_runtime.go`).
- `ServerRuntimeReportViewDTO.AgentOS string json:"agent_os"` (filled from
  `telemetry.OS`) — frontend area consumes it for the Metal/non-macOS hint.

### CONSUMES (this area depends on these being provided)
- `routing.RuntimeSpecGPU.Position int` + migration 73 `position` column +
  sqlite `RuntimeSpecGPUs` `ORDER BY position` + memory `RuntimeSpecGPUs` sort
  by `Position` — all from the **store area (Part A)**. My `putRuntimeSpec`
  sets `Position = i`; it is inert until those land.
- `routing.ServerTelemetry.OS` (already exists, `store.go:235`, populated by
  ingest) — consumed read-only for `AgentOS`.
- Agent capability flag `{Name:"gpu_selection", Since:"0.4.0"}` in
  `agent.Features` and `const Version = "0.4.0"` — agent area; not touched here,
  but the frontend hint gating (agent-too-old) reads `agent_features`, which
  `ServerRuntimeReportView` already surfaces.

---

## 4. GOTCHAS

- **Framework:** Go stdlib `testing`, table-driven, `errors.Is` for sentinels,
  no testify. Portal tests: memory store via `newServerTestService`; HTTP tests:
  `NewTestServer()` + `newJSONRequest` + `errorBodyOf(t, rec)` (returns the API
  error `code` string) in package `gateway`.
- **Typed-string scan:** `scanRuntimeSpec` already scans `&spec.ResponsesMode`
  (a `routing.EndpointMode`) directly from a text column. `&spec.VisibleDevicesMode`
  works identically — database/sql's reflect fallback assigns a DB string to any
  String-kind Go type. No `sql.Scanner`, no temp var. INSERT passes
  `string(spec.VisibleDevicesMode)` for symmetry with `string(spec.ResponsesMode)`.
- **Cols-list ↔ scan-args correspondence:** add `visible_devices_mode` in the
  SAME ordinal position in `runtimeSpecCols`, `runtimeSpecColsPrefixed`, the
  INSERT column list, AND `scanRuntimeSpec` (recommend right after
  `messages_mode` / `&spec.MessagesMode`). The VALUES tuple gains one `?`
  (22 → 23). Miss any one and reads/writes shear silently.
- **Memory store needs no mode change:** `copyRuntimeSpec` only deep-copies
  `APIFlavors`; the value-string `VisibleDevicesMode` rides the struct copy. Only
  the **GPU sort** in the memory store changes, and that is Part A.
- **Agent-wire leak guard:** `TestAgentRuntimeConfigOmitsFlavorsAndModes`
  (`service_runtime_test.go:367-370`) fails if the marshaled agent config
  contains `"api_flavors"`, `"responses_mode"`, or `"messages_mode"`. The new
  key `visible_devices_mode` does not collide with those substrings — safe. This
  field is DELIBERATELY on the agent wire (the agent needs it), unlike the
  endpoint modes.
- **Migration 73 is the store area's file** (`internal/store/migrate.go`, next
  version after 72). It adds BOTH `visible_devices_mode text not null default
  'env'` and the `position` column + backfills. Because the mode default is
  `'env'`, no mode backfill is needed (fresh + upgrade both default to env). Do
  NOT edit `migration65Up`'s CREATE or the frozen `baselineCreateStatements`
  (see `migrate.go:3026-3032`, `:3074`, `:3133` for the append-only rationale).
- **Cross-driver / parity:** the store round-trip must hold on memory + sqlite +
  postgres. Extend the existing conformance RuntimeSpec block
  (`internal/store/conformance_test.go:7681-7704`) with `VisibleDevicesMode`
  (seed + assert). Postgres leg runs only when `OP_AI_GATEWAY_TEST_POSTGRES_DSN`
  is set (`conformance_test.go:40`).
- **Validation ordering resolved:** mode value is validated ALWAYS (even flag
  off) — a bad enum is a malformed request; the args-placeholder trap and the
  conflict/no-gpus traps only when `SetVisibleDevices` is on. Trap order kept as
  conflict (trap 3) → no-gpus (trap 1) → args-placeholder, matching the agent
  and the existing "trap 3 before trap 1" comment (`service_runtime.go:703-704`).
- **Conflict trap in args mode (spec §10 open item, resolved yes):** a hand-set
  `CUDA_VISIBLE_DEVICES` in `env` is still refused in args mode — it remaps the
  CUDA namespace and would break `--device` numbering. The existing conflict
  trap already fires on `SetVisibleDevices` regardless of mode, so no change is
  needed to keep it active in both — the test just pins it.
- **Ambiguity resolved — where mode defaulting lives:** validation
  (`validateRuntimeSpecVisibleDevices`) is the single source of truth for
  rejecting a bad mode; `putRuntimeSpec` only RESOLVES the stored typed value
  (empty → env) trusting that validation already ran (mirrors how `adminState`
  and the endpoint modes are validated-then-stored). `validVisibleDevicesMode`
  treats `""` as valid (returns `("", true)`) so the default path is clean; this
  is a deliberate deviation from `validEndpointMode` (which rejects `""`) because
  the mode is optional with a default, whereas endpoint modes there are handled
  by a caller-side empty check.
- **B4 sequencing:** the GPU-order test cannot pass until the store area lands
  `Position` + `ORDER BY position` + the memory sort. Order the plan so Part A's
  store tasks precede B4; B0/B1/B2/B3/B5 are independent of Part A.
