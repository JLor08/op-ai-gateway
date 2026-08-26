# Agent Runtime Manager (T1+T2) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** The server-agent starts/stops model-serving processes on demand behind a single router port (llama-swap replacement), governed by a portal-maintained per-GPU co-residency model, with feature negotiation, live lifecycle states, and an optional local-file config source.

**Architecture:** Gateway stores launch specs 1:1 on model mappings plus a pair matrix and per-GPU budgets (migrations 65–67); a new agent package `internal/runtime` supervises children with a serialized admission owner and proxies requests by model; the existing WS channel pushes the full runtime-config document; lifecycle states ride the 1 s telemetry sample into a volatile gateway registry and a portal SSE stream. The routing resolver is untouched.

**Tech Stack:** Go 1.25/1.26 (two modules), net/http + coder/websocket, SQLite/PostgreSQL behind one dialect seam, React 19 + TS + MUI + Vite, vitest, Playwright.

**Spec:** `docs/superpowers/specs/2026-08-25-agent-runtime-manager-design.md` (approved). Read it before starting any task.

## Global Constraints

- Never commit to `main`; all work happens on branch `agent-runtime-manager` in `.worktrees/agent-runtime-manager` (already created; Go baseline green).
- Every new file starts with the two-line header: `// SPDX-License-Identifier: AGPL-3.0-only` + `// Copyright (C) 2026 OnPrem AI Gateway contributors`.
- Stable error codes are a contract: code string == sentinel text, `<domain>.<snake_case>` naming.
- Migrations are append-only; **65 is the next free version**; never touch `baselineCreateStatements`; float64-backed columns are `double precision`, 64-bit ints `bigint` (ADR-005; CI fails a `real` column via `TestMigration43WidensRealToDoublePrecision`).
- All VRAM values are **MB as `integer`** end to end.
- The server-agent module (`server-agent/`, module `op-ai-server-agent`) imports nothing from the gateway; archtest freezes its import graph — every new edge lands in `allowedDeps` in the same change.
- No shell interpreter for child processes: direct exec with an argv array.
- Secrets never reach the gateway: `env` values use `${AGENT_ENV:NAME}` placeholders; the file-mode report masks all env values.
- New portal strings land in `i18n.ts` in German AND English together (tsc-enforced parity).
- Feature gating by named flags only (string equality), never version comparison. T1's only flag: `runtime_manager`.
- Verify store changes with `OP_AI_GATEWAY_TEST_POSTGRES_DSN` set at least once before the PR.
- Update `docs/implementation-status.md` (branch-local) after each task.
- Test commands: gateway Go `cd gateway/backend && go test ./internal/...`; agent Go `cd server-agent && go test ./...`; frontend `cd gateway/frontend && npm test`; whole repo `make test-go` from root.

## File Structure (what gets created/modified where)

```
gateway/backend/
  internal/routing/store.go                 [M] domain types RuntimeSpec/RuntimeSpecGPU/CoResidencyRule/
                                                ServerGPUBudget/ServerRuntimeReport, ProviderServerAgent,
                                                AIServer.RuntimeMaxProcesses/.ManagedRuntimeOnly, RuntimeStore iface
  internal/routing/memory_store.go          [M] memory impls of RuntimeStore
  internal/store/migrate.go                 [M] migrations 65, 66, 67
  internal/store/sqlite_runtime.go          [C] SQL repo (serves sqlite AND postgres via dialect)
  internal/store/conformance_test.go        [M] forEachDialect conformance tests
  internal/store/routing_store_conformance_test.go [M] memory-vs-SQL suites
  internal/portal/service_runtime.go        [C] spec/matrix/budget CRUD + AgentRuntimeConfig assembly
  internal/portal/service_runtime_test.go   [C]
  internal/portal/api.go                    [M] hand-add new methods (interfacer regen is broken!)
  internal/portal/api_tracing_gen.go        [M] regen: go generate ./internal/tracing/... + scripts/add-license-headers.sh
  internal/gateway/agent_features.go        [C] GET /api/agent/v1/features
  internal/gateway/agent_runtime.go         [C] GET runtime-config, POST runtime-report, ingest helpers
  internal/gateway/runtime_registry.go      [C] volatile runtime-status + agent-features registries
  internal/gateway/portal_runtime_endpoints.go [C] portal CRUD + SSE /runtime/events
  internal/gateway/agent_stream_registry.go [M] NotifyRuntimeConfig
  internal/gateway/agent_ingest.go          [M] RuntimeSample + capabilities-features ingest
  internal/gateway/server.go                [M] route registration
  cmd/gateway/main.go                       [M] server_agent provider dispatch, runtime-changed hook wiring
server-agent/
  internal/runtime/types.go                 [C] Config/Spec/GPUBudget/State/Status + parse/validate
  internal/runtime/policy.go                [C] pure admission + eviction functions
  internal/runtime/policy_local.go          [C] binary/dir allowlist, ${AGENT_ENV}/${PORT} expansion
  internal/runtime/manager.go               [C] process supervisor (serialized owner)
  internal/runtime/logs.go                  [C] per-process ring buffer + stderr tail
  internal/runtime/router.go                [C] router port HTTP handler
  internal/runtime/config_client.go         [C] gateway source (ETag+disk fallback) & file source
  internal/runtime/features_client.go       [C] GET /api/agent/v1/features + intersection
  internal/runtime/report.go                [C] file-mode upward report builder (redacted)
  internal/runtime/driver.go                [C] Sync/Status driver the agent loop consumes
  internal/agent/agent.go                   [M] Deps.RuntimeDriver, run-loop wiring, sample fields
  internal/agent/features.go                [C] feature registry + Version bump 0.2.0
  internal/sample/sample.go                 [M] Runtimes []RuntimeSample, Capabilities fill helper
  internal/client/ws.go                     [M] runtime_config frame → latest-wins payload channel
  internal/client/client.go                 [M] PostRuntimeReport
  internal/config/config.go                 [M] OP_AGENT_RUNTIME_* settings
  internal/archtest/arch_test.go            [M] allowedDeps entries
  main.go                                   [M] runtime wiring
gateway/frontend/src/
  api/models.ts                             [M] 'server_agent' in ApplicationType union
  api/runtime.ts                            [C] runtime api module (+ wire into api.ts barrel)
  components/RuntimeAdminSection.tsx        [C] four areas
  components/RuntimeMatrix.tsx              [C] lower-triangle matrix
  components/ApplicationSection.tsx         [M] type-conditional drill-down
  components/ServerList.tsx                 [M] managed_runtime_only entry
  components/shared/applicationTypeDefaults.ts [M] server_agent entry + timeoutMs in TypeDefaults
  i18n.ts                                   [M] de+en keys
gateway/e2e/                                [C] e2e-runtime suite + stub model server fixture
docs/architecture/                          [M] new cross-cutting doc + 5 updates (final task)
```

Dependency order: Tasks 1→4 (store) before 5→10 (gateway services/endpoints); 11→18 (agent) independent of 5–10 except the wire contract; 19→22 (frontend) after 5–10; 23–24 last.

---

### Task 1: Migration 65 + RuntimeSpec store (SQL, memory, conformance)

**Files:**
- Modify: `gateway/backend/internal/routing/store.go` (domain types + `RuntimeStore` sub-interface + compose into `Store`)
- Modify: `gateway/backend/internal/store/migrate.go` (migration 65)
- Create: `gateway/backend/internal/store/sqlite_runtime.go`
- Modify: `gateway/backend/internal/routing/memory_store.go`
- Test: `gateway/backend/internal/store/conformance_test.go`, `gateway/backend/internal/store/routing_store_conformance_test.go`, `gateway/backend/internal/store/sqlite_migration_test.go`

**Interfaces:**
- Produces (used by Tasks 2–9):

```go
// internal/routing/store.go
const ProviderServerAgent = "server_agent" // next to ProviderLiteLLM

type RuntimeSpec struct {
	ID                          string
	MappingID                   string
	Enabled                     bool
	Binary                      string
	Args                        string // opaque JSON array string (the netbird_group_ids pattern)
	Env                         string // opaque JSON object string; values are ${AGENT_ENV:NAME} placeholders, never secrets
	WorkDir                     string
	ListenPort                  int // 0 = agent picks a free loopback port
	HealthPath                  string
	HealthTimeoutSeconds        int
	StartupTimeoutSeconds       int
	IdleTimeoutSeconds          int // 0 = never unload
	AdmissionWaitTimeoutSeconds int // 0 = wait until the client disconnects
	Pinned                      bool
	AdminState                  string // "" | "force_running" | "force_stopped"
	VRAMLocked                  bool
	CreatedAt                   time.Time
	UpdatedAt                   time.Time
}

type RuntimeSpecGPU struct {
	SpecID         string
	GPUIndex       int
	VRAMEstimateMB int
	VRAMMeasuredMB int
}

type RuntimeStore interface {
	UpsertRuntimeSpec(ctx context.Context, spec RuntimeSpec) error
	RuntimeSpecByMapping(ctx context.Context, mappingID string) (RuntimeSpec, bool, error)
	RuntimeSpecsByApplication(ctx context.Context, appID string) ([]RuntimeSpec, error)
	DeleteRuntimeSpec(ctx context.Context, id string) error
	SetRuntimeSpecGPUs(ctx context.Context, specID string, gpus []RuntimeSpecGPU) error
	RuntimeSpecGPUs(ctx context.Context, specID string) ([]RuntimeSpecGPU, error)
	// UpdateRuntimeSpecGPUMeasured writes back one agent measurement; ErrNotFound
	// when the (spec,gpu) row does not exist. Callers skip specs with VRAMLocked.
	UpdateRuntimeSpecGPUMeasured(ctx context.Context, specID string, gpuIndex int, measuredMB int) error
	// Task 2 adds the coresidency methods, Task 3 budgets, Task 4 reports.
}
```

- `Store` interface (store.go:1196) gains `RuntimeStore` in its embed list.
- Migration 65 creates all three tables (specs, spec GPUs, coresidency) in one migration; Task 2 only adds repo methods.

**Steps:**

- [ ] **Step 1: Write the failing conformance tests**

In `gateway/backend/internal/store/conformance_test.go` append (model: `TestConformanceServerHardware`):

```go
func TestConformanceRuntimeSpecs(t *testing.T) {
	forEachDialect(t, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		now := time.Now().UTC().Truncate(time.Second)
		seedRuntimeParents(t, s, now) // helper below: server srv_rt + app app_rt + mapping map_rt
		// Absent -> (zero, false, nil)
		if _, ok, err := s.RuntimeSpecByMapping(ctx, "map_rt"); err != nil || ok {
			t.Fatalf("absent spec: ok=%v err=%v", ok, err)
		}
		spec := routing.RuntimeSpec{
			ID: "rspec_1", MappingID: "map_rt", Enabled: true,
			Binary: "/usr/bin/llama-server", Args: `["--port","${PORT}"]`,
			Env: `{"HF_TOKEN":"${AGENT_ENV:HF_TOKEN}"}`, WorkDir: "/srv/models",
			ListenPort: 0, HealthPath: "/health", HealthTimeoutSeconds: 5,
			StartupTimeoutSeconds: 180, IdleTimeoutSeconds: 900,
			AdmissionWaitTimeoutSeconds: 30, Pinned: true,
			AdminState: "force_running", VRAMLocked: true,
			CreatedAt: now, UpdatedAt: now,
		}
		if err := s.UpsertRuntimeSpec(ctx, spec); err != nil {
			t.Fatalf("upsert: %v", err)
		}
		got, ok, err := s.RuntimeSpecByMapping(ctx, "map_rt")
		if err != nil || !ok {
			t.Fatalf("read back: ok=%v err=%v", ok, err)
		}
		if got.Binary != spec.Binary || got.Args != spec.Args || got.Env != spec.Env ||
			!got.Enabled || !got.Pinned || !got.VRAMLocked ||
			got.AdminState != "force_running" || got.AdmissionWaitTimeoutSeconds != 30 ||
			!got.CreatedAt.Equal(now) {
			t.Fatalf("round-trip mismatch: %+v", got)
		}
		// Upsert on the same mapping overwrites (1 spec per mapping)
		spec.Binary = "/usr/bin/vllm"
		spec.UpdatedAt = now.Add(time.Minute)
		if err := s.UpsertRuntimeSpec(ctx, spec); err != nil {
			t.Fatalf("upsert overwrite: %v", err)
		}
		got, _, _ = s.RuntimeSpecByMapping(ctx, "map_rt")
		if got.Binary != "/usr/bin/vllm" || !got.CreatedAt.Equal(now) {
			t.Fatalf("overwrite must keep created_at, got %+v", got)
		}
		// List by application
		specs, err := s.RuntimeSpecsByApplication(ctx, "app_rt")
		if err != nil || len(specs) != 1 {
			t.Fatalf("by application: %v %d", err, len(specs))
		}
		// GPU rows: atomic replace + ordered read
		gpus := []routing.RuntimeSpecGPU{
			{SpecID: "rspec_1", GPUIndex: 1, VRAMEstimateMB: 21500},
			{SpecID: "rspec_1", GPUIndex: 0, VRAMEstimateMB: 22000},
		}
		if err := s.SetRuntimeSpecGPUs(ctx, "rspec_1", gpus); err != nil {
			t.Fatalf("set gpus: %v", err)
		}
		gotGPUs, err := s.RuntimeSpecGPUs(ctx, "rspec_1")
		if err != nil || len(gotGPUs) != 2 || gotGPUs[0].GPUIndex != 0 || gotGPUs[1].GPUIndex != 1 {
			t.Fatalf("gpus must read ordered by index: %v %+v", err, gotGPUs)
		}
		// Measurement write-back
		if err := s.UpdateRuntimeSpecGPUMeasured(ctx, "rspec_1", 0, 21800); err != nil {
			t.Fatalf("measured: %v", err)
		}
		gotGPUs, _ = s.RuntimeSpecGPUs(ctx, "rspec_1")
		if gotGPUs[0].VRAMMeasuredMB != 21800 || gotGPUs[0].VRAMEstimateMB != 22000 {
			t.Fatalf("measured must not clobber estimate: %+v", gotGPUs[0])
		}
		if err := s.UpdateRuntimeSpecGPUMeasured(ctx, "rspec_1", 7, 1); err != ErrNotFound {
			t.Fatalf("measured on absent gpu row: want ErrNotFound, got %v", err)
		}
		// FK: spec on a missing mapping -> ErrNotFound
		orphan := spec
		orphan.ID, orphan.MappingID = "rspec_orphan", "map_missing"
		if err := s.UpsertRuntimeSpec(ctx, orphan); err != ErrNotFound {
			t.Fatalf("orphan spec: want ErrNotFound, got %v", err)
		}
		// Delete + cascade of GPU rows
		if err := s.DeleteRuntimeSpec(ctx, "rspec_1"); err != nil {
			t.Fatalf("delete: %v", err)
		}
		if err := s.DeleteRuntimeSpec(ctx, "rspec_1"); err != ErrNotFound {
			t.Fatalf("double delete: want ErrNotFound, got %v", err)
		}
		if gotGPUs, err = s.RuntimeSpecGPUs(ctx, "rspec_1"); err != nil || len(gotGPUs) != 0 {
			t.Fatalf("gpu rows must cascade: %v %d", err, len(gotGPUs))
		}
	})
}

// seedRuntimeParents creates server srv_rt, server_agent application app_rt, and
// mapping map_rt (+ a second mapping map_rt2 for later tasks).
func seedRuntimeParents(t *testing.T, s *SQLStore, now time.Time) {
	t.Helper()
	ctx := context.Background()
	if err := s.CreateAIServer(ctx, routing.AIServer{ID: "srv_rt", Name: "RT", Domain: "rt.example.test", Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("seed server: %v", err)
	}
	if err := s.CreateApplication(ctx, routing.Application{ID: "app_rt", ServerID: "srv_rt", Type: routing.ProviderServerAgent, Port: 8081, Scheme: "http", APIFlavors: []string{"openai"}, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("seed app: %v", err)
	}
	for _, mid := range []string{"map_rt", "map_rt2"} {
		if err := s.CreateMapping(ctx, routing.ModelMapping{ID: mid, ApplicationID: "app_rt", GatewayModelName: mid + "-model", AppModelName: mid + "-upstream", Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("seed mapping %s: %v", mid, err)
		}
	}
}
```

Note: check the exact field/method names of `CreateAIServer`/`CreateApplication`/`CreateMapping` and status constants against `internal/routing/store.go` when writing — adjust the seed helper to the real signatures, not the other way round.

In `gateway/backend/internal/store/routing_store_conformance_test.go` append a memory-vs-SQL test running the same assertions through `routing.Store` using `forEachRoutingStoreSeeded(t, seedSQL, run)` (seedSQL seeds the FK parents on the SQL store; the memory store enforces no FKs, so skip the orphan assertion there by checking `s.(*routing.MemoryStore)` — follow how existing suites handle this, e.g. grep for `MemoryStore` type switches in that file).

In `gateway/backend/internal/store/sqlite_migration_test.go` append:

```go
func TestMigration65RuntimeTables(t *testing.T) {
	s := openMigratedTestSQLite(t)
	defer s.Close()
	for _, table := range []string{"agent_runtime_specs", "agent_runtime_spec_gpus", "agent_coresidency_rules"} {
		if !sqliteTableExists(t, s.db, table) {
			t.Fatalf("table %s missing after migrate", table)
		}
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd gateway/backend && go test ./internal/store/ -run 'TestConformanceRuntimeSpecs|TestMigration65' -v`
Expected: compile FAIL (`routing.RuntimeSpec` undefined).

- [ ] **Step 3: Add domain types + interface (routing/store.go), migration 65, SQL repo, memory repo**

`internal/routing/store.go`: add `ProviderServerAgent` next to the provider consts (store.go:14–19), the three structs and `RuntimeStore` (Interfaces block above), embed `RuntimeStore` into `Store`.

`internal/store/migrate.go`: append `{version: 65, name: "agent_runtime_manager", up: migration65Up},` and at the bottom:

```go
// migration65Up creates the agent-runtime-manager tables (T1): launch specs
// (1:1 per mapping), per-GPU VRAM demand rows, and the pairwise co-residency
// matrix (row present = pair allowed; mapping_a_id < mapping_b_id canonical).
// Defaults reproduce the pre-feature behavior exactly: no rows, nothing starts.
func migration65Up(ctx context.Context, tx *sql.Tx, dl dialect) error {
	ts := dl.timestampType()
	stmts := []string{
		`create table if not exists agent_runtime_specs (
			id text primary key,
			mapping_id text not null unique references model_mappings(id) on delete cascade,
			enabled integer not null default 0,
			binary text not null default '',
			args text not null default '[]',
			env text not null default '{}',
			work_dir text not null default '',
			listen_port integer not null default 0,
			health_path text not null default '',
			health_timeout_seconds integer not null default 0,
			startup_timeout_seconds integer not null default 0,
			idle_timeout_seconds integer not null default 0,
			admission_wait_timeout_seconds integer not null default 0,
			pinned integer not null default 0,
			admin_state text not null default '',
			vram_locked integer not null default 0,
			created_at ` + ts + ` not null, updated_at ` + ts + ` not null
		)`,
		`create table if not exists agent_runtime_spec_gpus (
			spec_id text not null references agent_runtime_specs(id) on delete cascade,
			gpu_index integer not null,
			vram_estimate_mb integer not null default 0,
			vram_measured_mb integer not null default 0,
			primary key (spec_id, gpu_index)
		)`,
		`create table if not exists agent_coresidency_rules (
			application_id text not null references applications(id) on delete cascade,
			mapping_a_id text not null references model_mappings(id) on delete cascade,
			mapping_b_id text not null references model_mappings(id) on delete cascade,
			created_at ` + ts + ` not null,
			primary key (application_id, mapping_a_id, mapping_b_id)
		)`,
	}
	for _, stmt := range stmts {
		if err := execTx(ctx, tx, dl, stmt); err != nil {
			return err
		}
	}
	return nil
}
```

`internal/store/sqlite_runtime.go` (new; serves both dialects — `type SQLiteStore = SQLStore`):

```go
// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

// Runtime-manager persistence (T1): launch specs, per-GPU demand rows, the
// co-residency matrix (Task 2), GPU budgets (Task 3), runtime reports (Task 4).
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"op-ai-gateway/internal/routing"
)

func (s *SQLiteStore) UpsertRuntimeSpec(ctx context.Context, spec routing.RuntimeSpec) error {
	_, err := s.exec(ctx, `
		insert into agent_runtime_specs (
			id, mapping_id, enabled, binary, args, env, work_dir, listen_port,
			health_path, health_timeout_seconds, startup_timeout_seconds,
			idle_timeout_seconds, admission_wait_timeout_seconds, pinned,
			admin_state, vram_locked, created_at, updated_at
		) values (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		on conflict(mapping_id) do update set
			enabled = excluded.enabled, binary = excluded.binary,
			args = excluded.args, env = excluded.env, work_dir = excluded.work_dir,
			listen_port = excluded.listen_port, health_path = excluded.health_path,
			health_timeout_seconds = excluded.health_timeout_seconds,
			startup_timeout_seconds = excluded.startup_timeout_seconds,
			idle_timeout_seconds = excluded.idle_timeout_seconds,
			admission_wait_timeout_seconds = excluded.admission_wait_timeout_seconds,
			pinned = excluded.pinned, admin_state = excluded.admin_state,
			vram_locked = excluded.vram_locked, updated_at = excluded.updated_at`,
		spec.ID, spec.MappingID, spec.Enabled, spec.Binary, spec.Args, spec.Env,
		spec.WorkDir, spec.ListenPort, spec.HealthPath, spec.HealthTimeoutSeconds,
		spec.StartupTimeoutSeconds, spec.IdleTimeoutSeconds,
		spec.AdmissionWaitTimeoutSeconds, spec.Pinned, spec.AdminState,
		spec.VRAMLocked, spec.CreatedAt, spec.UpdatedAt,
	)
	if err != nil {
		// FK before unique: sqlite's FK error text also matches the
		// unique-violation substring (see sqlite_projects.go).
		if s.dl.isForeignKeyViolation(err) {
			return ErrNotFound
		}
		if s.dl.isUniqueViolation(err) {
			return ErrConflict
		}
		return fmt.Errorf("upsert runtime spec: %w", err)
	}
	return nil
}

const runtimeSpecCols = `id, mapping_id, enabled, binary, args, env, work_dir,
	listen_port, health_path, health_timeout_seconds, startup_timeout_seconds,
	idle_timeout_seconds, admission_wait_timeout_seconds, pinned, admin_state,
	vram_locked, created_at, updated_at`

func (s *SQLiteStore) RuntimeSpecByMapping(ctx context.Context, mappingID string) (routing.RuntimeSpec, bool, error) {
	row := s.queryRow(ctx, `select `+runtimeSpecCols+` from agent_runtime_specs where mapping_id = ?`, mappingID)
	spec, err := scanRuntimeSpec(row)
	if errors.Is(err, ErrNotFound) {
		return routing.RuntimeSpec{}, false, nil
	}
	if err != nil {
		return routing.RuntimeSpec{}, false, err
	}
	return spec, true, nil
}

func (s *SQLiteStore) RuntimeSpecsByApplication(ctx context.Context, appID string) ([]routing.RuntimeSpec, error) {
	rows, err := s.query(ctx, `
		select `+prefixCols("s", runtimeSpecCols)+`
		from agent_runtime_specs s
		join model_mappings m on m.id = s.mapping_id
		where m.application_id = ?
		order by s.id`, appID)
	if err != nil {
		return nil, fmt.Errorf("list runtime specs: %w", err)
	}
	defer rows.Close()
	out := make([]routing.RuntimeSpec, 0)
	for rows.Next() {
		spec, err := scanRuntimeSpec(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, spec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate runtime specs: %w", err)
	}
	return out, nil
}
```

(`prefixCols` may not exist — if it doesn't, write the prefixed column list out literally, following the `projectColsPrefixed` convention in sqlite_projects.go:14-19.)

```go
func (s *SQLiteStore) DeleteRuntimeSpec(ctx context.Context, id string) error {
	result, err := s.exec(ctx, `delete from agent_runtime_specs where id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete runtime spec: %w", err)
	}
	return requireAffected(result)
}

func (s *SQLiteStore) SetRuntimeSpecGPUs(ctx context.Context, specID string, gpus []routing.RuntimeSpecGPU) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin set spec gpus: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var exists int
	if err := tx.QueryRowContext(ctx, s.dl.rebind(`select count(*) from agent_runtime_specs where id = ?`), specID).Scan(&exists); err != nil {
		return fmt.Errorf("check spec: %w", err)
	}
	if exists == 0 {
		return ErrNotFound
	}
	if _, err := tx.ExecContext(ctx, s.dl.rebind(`delete from agent_runtime_spec_gpus where spec_id = ?`), specID); err != nil {
		return fmt.Errorf("clear spec gpus: %w", err)
	}
	for _, g := range gpus {
		if _, err := tx.ExecContext(ctx, s.dl.rebind(`
			insert into agent_runtime_spec_gpus (spec_id, gpu_index, vram_estimate_mb, vram_measured_mb)
			values (?, ?, ?, ?)`), specID, g.GPUIndex, g.VRAMEstimateMB, g.VRAMMeasuredMB); err != nil {
			return fmt.Errorf("insert spec gpu: %w", err)
		}
	}
	return tx.Commit()
}

func (s *SQLiteStore) RuntimeSpecGPUs(ctx context.Context, specID string) ([]routing.RuntimeSpecGPU, error) {
	rows, err := s.query(ctx, `
		select spec_id, gpu_index, vram_estimate_mb, vram_measured_mb
		from agent_runtime_spec_gpus where spec_id = ? order by gpu_index`, specID)
	if err != nil {
		return nil, fmt.Errorf("list spec gpus: %w", err)
	}
	defer rows.Close()
	out := make([]routing.RuntimeSpecGPU, 0)
	for rows.Next() {
		var g routing.RuntimeSpecGPU
		if err := rows.Scan(&g.SpecID, &g.GPUIndex, &g.VRAMEstimateMB, &g.VRAMMeasuredMB); err != nil {
			return nil, fmt.Errorf("scan spec gpu: %w", err)
		}
		out = append(out, g)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate spec gpus: %w", err)
	}
	return out, nil
}

func (s *SQLiteStore) UpdateRuntimeSpecGPUMeasured(ctx context.Context, specID string, gpuIndex int, measuredMB int) error {
	result, err := s.exec(ctx, `
		update agent_runtime_spec_gpus set vram_measured_mb = ?
		where spec_id = ? and gpu_index = ?`, measuredMB, specID, gpuIndex)
	if err != nil {
		return fmt.Errorf("update measured vram: %w", err)
	}
	return requireAffected(result)
}

func scanRuntimeSpec(row rowScanner) (routing.RuntimeSpec, error) {
	var spec routing.RuntimeSpec
	var enabled, pinned, vramLocked int64
	err := row.Scan(&spec.ID, &spec.MappingID, &enabled, &spec.Binary, &spec.Args,
		&spec.Env, &spec.WorkDir, &spec.ListenPort, &spec.HealthPath,
		&spec.HealthTimeoutSeconds, &spec.StartupTimeoutSeconds,
		&spec.IdleTimeoutSeconds, &spec.AdmissionWaitTimeoutSeconds, &pinned,
		&spec.AdminState, &vramLocked, &spec.CreatedAt, &spec.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return routing.RuntimeSpec{}, ErrNotFound
	}
	if err != nil {
		return routing.RuntimeSpec{}, fmt.Errorf("scan runtime spec: %w", err)
	}
	spec.Enabled, spec.Pinned, spec.VRAMLocked = enabled != 0, pinned != 0, vramLocked != 0
	return spec, nil
}
```

Memory store (`internal/routing/memory_store.go`): add maps `runtimeSpecs map[string]RuntimeSpec` (by id), `runtimeSpecGPUs map[string][]RuntimeSpecGPU` to the struct AND the ctor; implement the seven methods mirroring the SQL semantics with comments (hand-rolled FK existence check against `m.mappings`, upsert-by-mapping preserving CreatedAt, cascade delete of GPU rows, sort by GPUIndex, deep-copy slices on return, `storeerr.ErrNotFound` sentinels — routing cannot import store).

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd gateway/backend && go test ./internal/store/ ./internal/routing/ -run 'Runtime|Migration65' -v`
Expected: PASS (postgres leg skips without DSN — run once with `OP_AI_GATEWAY_TEST_POSTGRES_DSN` before the PR).

- [ ] **Step 5: Regenerate the routing-store tracing decorator**

Run: `cd gateway/backend && go generate ./internal/tracing/... && ../../scripts/add-license-headers.sh`
Then: `go build ./...`
Expected: builds; `internal/tracing/routingstore_gen.go` gains the new methods.

- [ ] **Step 6: Commit**

```bash
git add -A && git commit -m "feat(store): agent runtime spec tables and repos (migration 65)"
```

---

### Task 2: Co-residency matrix store

**Files:**
- Modify: `gateway/backend/internal/routing/store.go`, `gateway/backend/internal/store/sqlite_runtime.go`, `gateway/backend/internal/routing/memory_store.go`
- Test: `gateway/backend/internal/store/conformance_test.go`, `routing_store_conformance_test.go`

**Interfaces:**
- Consumes: Task 1's tables/types.
- Produces:

```go
type CoResidencyRule struct {
	ApplicationID string
	MappingAID    string // canonical: MappingAID < MappingBID, enforced by the caller (portal)
	MappingBID    string
	CreatedAt     time.Time
}
// on RuntimeStore:
SetCoResidencyRules(ctx context.Context, appID string, rules []CoResidencyRule) error // atomic full replace
CoResidencyRulesByApplication(ctx context.Context, appID string) ([]CoResidencyRule, error)
```

Full-replace (the `SetGroupMembers` pattern) — the matrix UI always holds the complete pair set, and atomic replace avoids lost-update races between two admins.

**Steps:**

- [ ] **Step 1: Write the failing conformance test**

```go
func TestConformanceCoResidencyRules(t *testing.T) {
	forEachDialect(t, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		now := time.Now().UTC().Truncate(time.Second)
		seedRuntimeParents(t, s, now)
		// Empty by default
		rules, err := s.CoResidencyRulesByApplication(ctx, "app_rt")
		if err != nil || len(rules) != 0 {
			t.Fatalf("default: %v %d", err, len(rules))
		}
		// Set + ordered read (order by mapping_a_id, mapping_b_id)
		want := []routing.CoResidencyRule{
			{ApplicationID: "app_rt", MappingAID: "map_rt", MappingBID: "map_rt2", CreatedAt: now},
		}
		if err := s.SetCoResidencyRules(ctx, "app_rt", want); err != nil {
			t.Fatalf("set: %v", err)
		}
		rules, _ = s.CoResidencyRulesByApplication(ctx, "app_rt")
		if len(rules) != 1 || rules[0].MappingAID != "map_rt" || rules[0].MappingBID != "map_rt2" {
			t.Fatalf("read back: %+v", rules)
		}
		// Full replace with empty clears
		if err := s.SetCoResidencyRules(ctx, "app_rt", nil); err != nil {
			t.Fatalf("clear: %v", err)
		}
		if rules, _ = s.CoResidencyRulesByApplication(ctx, "app_rt"); len(rules) != 0 {
			t.Fatalf("clear must empty the set: %+v", rules)
		}
		// Unknown application -> ErrNotFound
		if err := s.SetCoResidencyRules(ctx, "app_missing", nil); err != ErrNotFound {
			t.Fatalf("unknown app: want ErrNotFound, got %v", err)
		}
		// FK: rule naming a missing mapping -> ErrNotFound
		bad := []routing.CoResidencyRule{{ApplicationID: "app_rt", MappingAID: "map_missing", MappingBID: "map_rt", CreatedAt: now}}
		if err := s.SetCoResidencyRules(ctx, "app_rt", bad); err != ErrNotFound {
			t.Fatalf("missing mapping: want ErrNotFound, got %v", err)
		}
	})
}
```

Plus the memory-vs-SQL twin in `routing_store_conformance_test.go` (skip FK assertions on memory or hand-enforce them — the memory impl SHOULD hand-check mapping existence, mirroring SQL).

- [ ] **Step 2: Run to verify failure** — `go test ./internal/store/ -run TestConformanceCoResidency -v` → compile FAIL.

- [ ] **Step 3: Implement**

SQL (in sqlite_runtime.go, the SetGroupMembers tx shape): existence check on `applications`, `delete from agent_coresidency_rules where application_id = ?`, per-rule insert classifying FK violations as ErrNotFound (FK-before-unique order!), commit. Read: `select application_id, mapping_a_id, mapping_b_id, created_at from agent_coresidency_rules where application_id = ? order by mapping_a_id, mapping_b_id`, non-nil empty slice. Memory: `coresidency map[string][]CoResidencyRule` + ctor entry; hand-check app and mapping existence; sort; deep-copy on return. Note: the store does NOT enforce `a < b` — canonical ordering is portal-level validation (Task 6); the store stays a dumb pair table.

- [ ] **Step 4: Run to verify pass** — same command, plus `go generate ./internal/tracing/... && ../../scripts/add-license-headers.sh && go build ./...`.

- [ ] **Step 5: Commit** — `git add -A && git commit -m "feat(store): co-residency matrix rules"`

---

### Task 3: Migration 66 — GPU budgets + server runtime columns

**Files:**
- Modify: `gateway/backend/internal/routing/store.go` (ServerGPUBudget type, AIServer fields, RuntimeStore methods)
- Modify: `gateway/backend/internal/store/migrate.go` (migration 66), `sqlite_runtime.go`, `internal/store/sqlite_routes.go` (AIServer column plumbing), `internal/routing/memory_store.go`
- Test: conformance + migration tests as before

**Interfaces:**
- Produces:

```go
type ServerGPUBudget struct {
	ServerID     string
	GPUIndex     int
	BudgetMB     int
	ExpectedUUID string
	ExpectedName string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}
// on RuntimeStore:
SetServerGPUBudgets(ctx context.Context, serverID string, budgets []ServerGPUBudget) error // atomic full replace
ServerGPUBudgets(ctx context.Context, serverID string) ([]ServerGPUBudget, error)
// on AIServer:
RuntimeMaxProcesses int  // 0 = unlimited
ManagedRuntimeOnly  bool
```

**Steps:**

- [ ] **Step 1: Failing tests** — `TestConformanceServerGPUBudgets` (same shape as Task 2: default empty, set two budgets for indexes 1 and 0, read ordered by index, replace, clear, unknown server → ErrNotFound) and extend an existing AIServer round-trip conformance test (find the one covering `AgentPresenceTimeoutSeconds` in `conformance_test.go` / `routing_store_conformance_test.go`) with `RuntimeMaxProcesses: 3, ManagedRuntimeOnly: true` assertions.

- [ ] **Step 2: Run to verify failure.**

- [ ] **Step 3: Implement**

Migration 66 (`server_runtime_limits`):

```go
func migration66Up(ctx context.Context, tx *sql.Tx, dl dialect) error {
	ts := dl.timestampType()
	if err := execTx(ctx, tx, dl, `create table if not exists ai_server_gpu_budgets (
		server_id text not null references ai_servers(id) on delete cascade,
		gpu_index integer not null,
		budget_mb integer not null default 0,
		expected_uuid text not null default '',
		expected_name text not null default '',
		created_at `+ts+` not null, updated_at `+ts+` not null,
		primary key (server_id, gpu_index)
	)`); err != nil {
		return err
	}
	cols := []string{
		"runtime_max_processes integer not null default 0",
		"managed_runtime_only integer not null default 0",
	}
	for _, col := range cols {
		if err := addColumnIfMissing(ctx, tx, dl, "ai_servers", col); err != nil {
			return err
		}
	}
	return nil
}
```

Budgets repo: same tx full-replace + ordered read as Task 2. AIServer columns: thread through **all four statements + scanner** in `sqlite_routes.go` (`CreateAIServer` insert list, `UpdateAIServer` set list, both selects, `scanAIServer` — bools via int64). Memory store: struct copy is automatic; add `gpuBudgets map[string][]ServerGPUBudget` + methods.

- [ ] **Step 4: Run to verify pass** (+ tracing regen + build).
- [ ] **Step 5: Commit** — `git commit -m "feat(store): per-GPU budgets and server runtime limits (migration 66)"`

---

### Task 4: Migration 67 — server runtime reports

**Files:** as Task 3 (types in routing/store.go, migration + repo in store, memory impl, conformance).

**Interfaces:**
- Produces (mirror of `ServerHardware`):

```go
type ServerRuntimeReport struct {
	ServerID    string
	CollectedAt time.Time
	ReportJSON  string // validated canonical JSON blob, opaque to the store
	UpdatedAt   time.Time
}
// on RuntimeStore:
UpsertServerRuntimeReport(ctx context.Context, report ServerRuntimeReport) error
ServerRuntimeReportByServer(ctx context.Context, serverID string) (ServerRuntimeReport, bool, error)
```

**Steps:**

- [ ] **Step 1: Failing test** — `TestConformanceServerRuntimeReports`: copy `TestConformanceServerHardware`'s shape verbatim (absent → `(zero,false,nil)`; insert; round-trip; upsert overwrites; orphan server → ErrNotFound).
- [ ] **Step 2: Run to verify failure.**
- [ ] **Step 3: Implement** — migration 67 `server_runtime_reports` (copy the `server_hardware` DDL: `server_id text primary key references ai_servers(id) on delete cascade, collected_at <ts> not null, report_json text not null default '', updated_at <ts> not null`); repo methods are a rename-level copy of `UpsertServerHardware`/`ServerHardwareByServer`/`scanServerHardware`; memory map + methods.
- [ ] **Step 4: Run to verify pass** (+ tracing regen + build + `make test-go` once as a store-layer checkpoint).
- [ ] **Step 5: Commit** — `git commit -m "feat(store): file-mode runtime reports (migration 67)"`

---

### Task 5: Portal service — runtime spec CRUD

**Files:**
- Create: `gateway/backend/internal/portal/service_runtime.go`
- Modify: `gateway/backend/internal/portal/api.go` (HAND-EDIT — interfacer regen is broken, see docs/architecture/11-risks-and-technical-debt.md:52), then regen `api_tracing_gen.go`
- Modify: `gateway/backend/internal/gateway/portal_runtime_endpoints.go` (create), `internal/gateway/server.go` (route registration)
- Test: `gateway/backend/internal/portal/service_runtime_test.go`, `gateway/backend/internal/gateway/portal_runtime_endpoints_test.go`

**Interfaces:**
- Consumes: Task 1 store methods; existing `authorizeMapping(ctx, principal, mappingID)` (service_applications.go:1361), `compactRandomHex(16)` (service.go:3870), `isSystem` etc.
- Produces (used by Tasks 7, 19–22):

```go
// service_runtime.go — sentinels (code string == sentinel text)
const CodeRuntimeSpecNotFound = "runtime_spec.not_found"
var (
	ErrRuntimeSpecNotFound          = errors.New(CodeRuntimeSpecNotFound)
	ErrRuntimeSpecBinaryRequired    = errors.New("runtime_spec.binary_required")
	ErrRuntimeSpecArgsInvalid       = errors.New("runtime_spec.args_invalid")
	ErrRuntimeSpecEnvInvalid        = errors.New("runtime_spec.env_invalid")
	ErrRuntimeSpecGPUInvalid        = errors.New("runtime_spec.gpu_invalid")
	ErrRuntimeSpecTuningInvalid     = errors.New("runtime_spec.tuning_invalid")
	ErrRuntimeSpecAdminStateInvalid = errors.New("runtime_spec.admin_state_invalid")
	ErrRuntimeSpecNotServerAgent    = errors.New("runtime_spec.application_not_server_agent")
)

type RuntimeSpecGPUDTO struct {
	Index          int `json:"index"`
	VRAMEstimateMB int `json:"vram_estimate_mb"`
	VRAMMeasuredMB int `json:"vram_measured_mb"`
}
type RuntimeSpecDTO struct {
	Configured                  bool                `json:"configured"` // false = no spec row exists yet
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
type PutRuntimeSpecRequest struct { // PUT = upsert; full document, no pointer-patch
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
	GPUs                        []RuntimeSpecGPUDTO `json:"gpus"` // VRAMMeasuredMB is ALWAYS ignored on write (agent-owned; see below)
}

// Service methods (add to api.go by hand, keep neighbor ordering):
GetRuntimeSpec(ctx context.Context, principal auth.Token, mappingID string) (RuntimeSpecDTO, error)
PutRuntimeSpec(ctx context.Context, principal auth.Token, mappingID string, req PutRuntimeSpecRequest) (RuntimeSpecDTO, error)
DeleteRuntimeSpec(ctx context.Context, principal auth.Token, mappingID string) error
```

- HTTP (registered in the mappings item dispatcher `handlePortalMappingItem`): `GET/PUT/DELETE /api/portal/mappings/{id}/runtime-spec`. Same authorization as mapping writes — `authorizeMapping` gives exactly that.
- Defaults applied in `PutRuntimeSpec` when 0/empty: `HealthPath "/health"`, `HealthTimeoutSeconds 5`, `StartupTimeoutSeconds 180`. Validation: Binary required + absolute path; all int fields >= 0 (`ErrRuntimeSpecTuningInvalid`); `AdminState` ∈ {"", "force_running", "force_stopped"}; GPU indexes >= 0 and unique; env keys must match `^[A-Z_][A-Z0-9_]*$` and values must NOT look like raw secrets is NOT checkable — instead: any env value is allowed but the DTO stores it verbatim; document `${AGENT_ENV:NAME}` in the UI (Task 20). Application must be `server_agent` type (`ErrRuntimeSpecNotServerAgent`) — a spec on an ollama app is a config error.
- **VRAM ownership rule** (one rule, both directions): `vram_estimate_mb` is operator-owned and only the portal writes it; `vram_measured_mb` is agent-owned and only the telemetry write-back (Task 9) writes it. A PUT preserves the stored measured values verbatim and ignores whatever the client sent for them. `vram_locked` gates only the agent's write-back, never the portal's estimate write.
- Storage mapping: `Args []string` ⇄ opaque JSON string via `json.Marshal`/`Unmarshal` (unmarshal failure on read → `ErrRuntimeSpecArgsInvalid`); same for Env.
- ID: `"rspec_" + compactRandomHex(16)` on first create; PUT on an existing spec keeps the ID and CreatedAt (read-then-upsert).
- After every successful write call `s.notifyRuntimeChanged(server.ID)` (nil-safe). The hook is settable two ways: a `ServiceDeps.OnRuntimeConfigChanged func(serverID string)` field (used by tests) AND an exported setter `func (s *Service) SetRuntimeConfigChangedHook(fn func(serverID string))` — the setter exists because main.go builds the gateway Server AFTER the portal service and wires `srv.PushRuntimeConfig` late (Task 8).

**Steps:**

- [ ] **Step 1: Write failing service tests** (`service_runtime_test.go`, using `newServerTestService(t, now)` + `createTestServer` + a `server_agent` application + mapping created through the service — see service_applications_test.go helpers):

```go
func TestPutRuntimeSpecRoundTrip(t *testing.T) { /* owner creates spec on a server_agent app's mapping; read back; assert defaults applied (health_path "/health", startup 180) and args/env round-trip */ }
func TestPutRuntimeSpecValidation(t *testing.T) { /* table: empty binary -> ErrRuntimeSpecBinaryRequired; relative binary -> same; negative idle -> ErrRuntimeSpecTuningInvalid; bad admin_state -> ErrRuntimeSpecAdminStateInvalid; duplicate gpu index -> ErrRuntimeSpecGPUInvalid; spec on non-server_agent app -> ErrRuntimeSpecNotServerAgent */ }
func TestRuntimeSpecAuthorization(t *testing.T) { /* otherToken() gets ErrMappingNotFound on GET/PUT/DELETE (404-no-leak); ownerToken() and admin-group manager succeed; adminToken() alone (no delegation) gets not-found */ }
func TestGetRuntimeSpecUnconfigured(t *testing.T) { /* mapping without spec -> Configured:false, GPUs: non-nil empty */ }
func TestPutRuntimeSpecFiresRuntimeChangedHook(t *testing.T) { /* ServiceDeps.OnRuntimeConfigChanged records server IDs; assert called once with the server id on PUT and DELETE */ }
```

- [ ] **Step 2: Run to verify failure** — `go test ./internal/portal/ -run RuntimeSpec -v` → compile FAIL.

- [ ] **Step 3: Implement service methods**

`service_runtime.go`: follow the `CreateApplication` body shape — `authorizeMapping` first; validate everything before mutating; encode args/env; read-existing for ID/CreatedAt preservation; `s.routes.UpsertRuntimeSpec` + `s.routes.SetRuntimeSpecGPUs`; DTO builder `runtimeSpecDTO(spec routing.RuntimeSpec, gpus []routing.RuntimeSpecGPU) (RuntimeSpecDTO, error)`. Add `runtimeChanged` field on Service + `OnRuntimeConfigChanged` on ServiceDeps (nil-safe call helper `func (s *Service) notifyRuntimeChanged(serverID string)`). Hand-add the three methods to `api.go`, regen tracing (`go generate ./internal/tracing/... && ../../scripts/add-license-headers.sh`).

- [ ] **Step 4: Run service tests to verify pass.**

- [ ] **Step 5: Write failing handler tests** (`portal_runtime_endpoints_test.go`, `NewTestServer()` + `newJSONRequest` + `srv.ServeHTTP`): GET unconfigured 200 `{"configured":false,...}`; PUT valid 200; PUT bad admin_state 400 code `runtime_spec.admin_state_invalid`; DELETE 200 `{"ok":true}`; 405 sets Allow; unknown mapping 404 `mapping.not_found`.

- [ ] **Step 6: Implement endpoints**

Create `portal_runtime_endpoints.go` with `portalRuntimeSpecErrRows []errRow` (one row per sentinel from the Interfaces block, status 400 except NotFound 404) + `writePortalRuntimeSpecError`. Extend `handlePortalMappingItem` (portal_mapping_endpoints.go) with the sub-path guard BEFORE its pathID fallthrough:

```go
if len(parts) == 2 && parts[1] == "runtime-spec" && parts[0] != "" {
	s.handlePortalMappingRuntimeSpec(w, r, token, parts[0])
	return
}
```

`handlePortalMappingRuntimeSpec(w, r, token, mappingID)` switches GET/PUT/DELETE exactly like `handlePortalApplicationItem` (200/200/200-ok-true, 405+Allow default).

- [ ] **Step 7: Run all gateway tests** — `go test ./internal/gateway/ ./internal/portal/ -run Runtime -v` → PASS.

- [ ] **Step 8: Commit** — `git commit -m "feat(portal): runtime spec CRUD on mappings"`

---

### Task 6: Portal service — matrix, GPU budgets, server flags, timeout guard

**Files:**
- Modify: `gateway/backend/internal/portal/service_runtime.go`, `api.go` (+ tracing regen), `internal/portal/service.go` (UpdateServerRequest fields), `internal/gateway/portal_runtime_endpoints.go`, `portal_server_endpoints.go` (budget route + server errRows), `internal/gateway/server.go` if needed
- Test: mirrors of Task 5's test files

**Interfaces:**
- Produces:

```go
var (
	ErrCoResidencyPairInvalid = errors.New("runtime_coresidency.pair_invalid")
	ErrGPUBudgetInvalid       = errors.New("server.gpu_budget_invalid")
	ErrServerManagedRuntimeOnly = errors.New("application.managed_runtime_only")
	ErrServerRuntimeLimitInvalid = errors.New("server.runtime_limit_invalid")
)
type CoResidencyDTO struct { Pairs [][2]string `json:"pairs"` } // mapping-ID pairs, each canonical a<b
type SetCoResidencyRequest struct { Pairs [][2]string `json:"pairs"` }
type GPUBudgetDTO struct {
	Index        int    `json:"index"`
	BudgetMB     int    `json:"budget_mb"`
	ExpectedUUID string `json:"expected_uuid"`
	ExpectedName string `json:"expected_name"`
}
type SetGPUBudgetsRequest struct { Budgets []GPUBudgetDTO `json:"budgets"` }

// Service methods:
GetCoResidency(ctx context.Context, principal auth.Token, appID string) (CoResidencyDTO, error)
SetCoResidency(ctx context.Context, principal auth.Token, appID string, req SetCoResidencyRequest) (CoResidencyDTO, error)
GetServerGPUBudgets(ctx context.Context, principal auth.Token, serverID string) ([]GPUBudgetDTO, error)
SetServerGPUBudgets(ctx context.Context, principal auth.Token, serverID string, req SetGPUBudgetsRequest) ([]GPUBudgetDTO, error)
// RuntimeSpecWarnings is pure derivation for the portal (no store write):
// returns e.g. ["timeout_ms_below_startup_timeout"] when app.TimeoutMS <
// max(spec.StartupTimeoutSeconds)*1000 across the app's enabled specs.
RuntimeWarnings(ctx context.Context, principal auth.Token, appID string) ([]string, error)
```

- HTTP: `GET/PUT /api/portal/applications/{id}/runtime/coresidency`, `GET/PUT /api/portal/servers/{id}/gpu-budgets`, `GET /api/portal/applications/{id}/runtime/warnings`.
- `UpdateServerRequest`/`CreateServerRequest` gain `RuntimeMaxProcesses *int` / `ManagedRuntimeOnly *bool` (`json:"runtime_max_processes,omitempty"` / `json:"managed_runtime_only,omitempty"`); `ServerDTO` exposes both. Validation: negative max processes → `ErrServerRuntimeLimitInvalid`.
- `CreateApplication` gains the managed-runtime-only gate: when `server.ManagedRuntimeOnly && req.Type != routing.ProviderServerAgent` → `ErrServerManagedRuntimeOnly` (409).
- SetCoResidency validation: each pair's two IDs distinct (`ErrCoResidencyPairInvalid`), both mappings belong to this application, and the pair is stored canonically — sort each pair (a<b lexicographic) server-side so the client never has to care; reject duplicate pairs after normalization.
- Budgets validation: index >= 0 unique, budget_mb >= 0. `expected_uuid/expected_name` are snapshotted server-side from the latest telemetry sample's GPUs (via `s.routes` telemetry read — find the existing method the hardware panel uses) when a budget row is new; a PUT never overwrites an existing row's expected_* (drift detection needs the original).
- Every successful write fires `s.notifyRuntimeChanged(serverID)`.

**Steps:**

- [ ] **Step 1: Failing service tests** — canonicalization (`[["b","a"]]` stored/returned as `["a","b"]`), duplicate pair rejected, foreign mapping rejected, budgets round-trip + validation, server flags patch round-trip, managed-runtime-only gate on CreateApplication (sentinel + still allows `server_agent` type), `RuntimeWarnings` returns the timeout warning exactly when `TimeoutMS < StartupTimeoutSeconds*1000`, hook fired per write.
- [ ] **Step 2: Run to verify failure.**
- [ ] **Step 3: Implement** (service + api.go + tracing regen). Authorization: coresidency/warnings through `authorizeApplication`; budgets through `authorizeServer`.
- [ ] **Step 4: Run to verify pass.**
- [ ] **Step 5: Failing handler tests** (routes above; `server.gpu_budget_invalid` 400; managed-only create → 409 `application.managed_runtime_only`).
- [ ] **Step 6: Implement handlers** — budgets route into `handlePortalServerItem` (`len(parts)==2 && parts[1]=="gpu-budgets"`); coresidency/warnings into `handlePortalApplicationItem` (`parts[1]=="runtime"` with `parts[2]` switch).
- [ ] **Step 7: Run gateway+portal tests** → PASS.
- [ ] **Step 8: Commit** — `git commit -m "feat(portal): co-residency matrix, GPU budgets, managed-runtime-only"`

---

### Task 7: Agent endpoints — features + runtime-config (ETag)

**Files:**
- Create: `gateway/backend/internal/gateway/agent_features.go`, `gateway/backend/internal/gateway/agent_runtime.go`
- Modify: `gateway/backend/internal/portal/service_runtime.go` (+api.go+tracing) — `AgentRuntimeConfig` assembly
- Modify: `gateway/backend/internal/gateway/server.go` (two new `agentRoutes` rows)
- Test: `agent_features_test.go`, `agent_runtime_test.go` (gateway pkg), service test for assembly

**Interfaces:**
- Produces (the wire contract Tasks 16/17 consume — MUST match spec §11):

```go
// portal
type AgentRuntimeSpecGPUDTO struct {
	Index  int `json:"index"`
	VRAMMB int `json:"vram_mb"` // measured value when known, else the operator estimate; 0 = unknown
}
type AgentRuntimeSpecDTO struct {
	ID                          string                   `json:"id"`
	Model                       string                   `json:"model"`          // gateway_model_name (display)
	UpstreamModel               string                   `json:"upstream_model"` // app_model_name (router match key)
	Binary                      string                   `json:"binary"`
	Args                        []string                 `json:"args"`
	Env                         map[string]string        `json:"env"`
	WorkDir                     string                   `json:"work_dir,omitempty"`
	GPUs                        []AgentRuntimeSpecGPUDTO `json:"gpus"`
	ListenPort                  int                      `json:"listen_port"`
	HealthPath                  string                   `json:"health_path"`
	HealthTimeoutSeconds        int                      `json:"health_timeout_seconds"`
	StartupTimeoutSeconds       int                      `json:"startup_timeout_seconds"`
	IdleTimeoutSeconds          int                      `json:"idle_timeout_seconds"`
	AdmissionWaitTimeoutSeconds int                      `json:"admission_wait_timeout_seconds"`
	Pinned                      bool                     `json:"pinned"`
	AdminState                  string                   `json:"admin_state"`
}
type AgentRuntimeConfigDTO struct {
	RouterListen int                   `json:"router_listen"` // the server_agent application's Port
	MaxProcesses int                   `json:"max_processes"`
	GPUBudgets   []AgentGPUBudgetDTO   `json:"gpu_budgets"`
	Specs        []AgentRuntimeSpecDTO `json:"specs"` // enabled specs only
	Coresident   [][2]string           `json:"coresident"` // SPEC-ID pairs (not mapping ids!)
	ETag         string                `json:"etag"`
}
type AgentGPUBudgetDTO struct {
	Index    int `json:"index"`
	BudgetMB int `json:"budget_mb"`
}
// Service method (agent-auth path, no principal — mirrors AgentProxyRoutes):
AgentRuntimeConfig(ctx context.Context, serverID string) (AgentRuntimeConfigDTO, error)
```

- Coresidency pairs are translated mapping-ID → spec-ID during assembly; pairs whose either spec is missing/disabled are dropped.
- ETag: `sha256(json.Marshal(dto-with-empty-ETag))` hex — the `agentProxyRoutesETag` pattern; empty config has a stable etag.
- `GET /api/agent/v1/runtime-config`: exactly the `handleAgentProxyRoutes` skeleton (Cache-Control no-store on every path, `requireMethod`, `authenticateAgent`, nil-Portal → empty valid DTO, static error bodies, quoted ETag header + `etagMatches` + 304-no-body). A server with no server_agent application returns the empty DTO (specs `[]`), NOT an error.
- `GET /api/agent/v1/features`: static list, in `agent_features.go`:

```go
// gatewayAgentFeatures is the gateway's declared feature set for agents.
// A feature is ACTIVE iff both sides declare it (spec §9). Append-only.
var gatewayAgentFeatures = []string{"runtime_manager"}
type agentFeaturesDTO struct {
	Features []string `json:"features"`
}
```

Handler: same skeleton, ETag over the marshaled body, agent-token auth, registered in `agentRoutes` (both muxes, NetbirdOnly gate).

**Steps:**

- [ ] **Step 1: Failing tests**
  - Service: `TestAgentRuntimeConfigAssembly` — server with server_agent app (port 8081), two mappings with enabled specs + one disabled, coresidency pair, budgets, max processes → DTO has RouterListen 8081, 2 specs, pair translated to spec IDs, stable ETag; disabled spec absent and its pairs dropped; a server without server_agent app → empty DTO, `Specs: []` non-nil.
  - Handler: `TestAgentRuntimeConfigEndpoint` — seedTestAgentToken; GET 200 + quoted ETag header + in-body etag; replay with If-None-Match → 304 empty body; no bearer → 401; `TestAgentFeaturesEndpoint` — 200 `{"features":["runtime_manager"]}` + ETag + 304 replay; `TestAgentRuntimeRoutesOnAgentMux` — both endpoints respond via `srv.AgentHandler()` (the dual-mux proof, model: `TestAgentStreamRegisteredOnAgentMux`).
- [ ] **Step 2: Run to verify failure.**
- [ ] **Step 3: Implement** (service assembly + both handlers + two `agentRoutes` rows + api.go/tracing regen).
- [ ] **Step 4: Run to verify pass.**
- [ ] **Step 5: Commit** — `git commit -m "feat(gateway): agent features and runtime-config endpoints"`

---

### Task 8: WS runtime_config push + feature-gated delivery

**Files:**
- Modify: `gateway/backend/internal/gateway/agent_stream_registry.go` (NotifyRuntimeConfig), `agent_ingest.go` (capabilities-features parse → volatile registry), `internal/gateway/runtime_registry.go` (create: `agentFeaturesRegistry`), `internal/gateway/server.go` (Server fields + ServerDeps), `cmd/gateway/main.go` (hook wiring)
- Test: `agent_stream_registry_test.go`, `agent_ingest` tests, `cmd/gateway` wiring source-scan test

**Interfaces:**
- Consumes: `AgentRuntimeConfigDTO` (Task 7), `OnRuntimeConfigChanged` hook (Task 5).
- Produces:

```go
// agent_stream_registry.go — same shape as NotifyCertUpdate: marshal → RLock
// snapshot → non-blocking enqueue, best-effort, no error return.
func (r *AgentStreamRegistry) NotifyRuntimeConfig(serverID string, payload json.RawMessage)
// frame on the wire: {"type":"runtime_config","data":<full AgentRuntimeConfigDTO JSON>}

// runtime_registry.go
type agentFeaturesRegistry struct{ mu sync.RWMutex; features map[string][]string }
func (r *agentFeaturesRegistry) Set(serverID string, features []string) // nil-safe
func (r *agentFeaturesRegistry) Has(serverID, feature string) bool      // nil-safe
// Server method — the push path (called from the portal hook via main.go):
func (s *Server) PushRuntimeConfig(serverID string) // goroutine-safe, best-effort
```

- `PushRuntimeConfig`: `go func(){ if !s.AgentFeatures.Has(serverID, "runtime_manager") || s.RuntimeStatus.IsFileMode(serverID) { return }; dto, err := s.Portal.AgentRuntimeConfig(context.Background(), serverID); if err != nil { slog.Debug(...); return }; b, _ := json.Marshal(dto); s.AgentStreams.NotifyRuntimeConfig(serverID, b) }()` — asynchronous because the portal hook contract is "synchronous but guaranteed fast" (it is called under certMu-like write paths).
- Capabilities parse in `ingestTelemetrySample` AFTER store writes succeed (the registry-update convention): parse `req.Capabilities` as `{"features":[...],"agent_version":"..."}` tolerantly (bad JSON → empty), `s.AgentFeatures.Set(serverID, features)`.
- main.go wiring: `OnRuntimeConfigChanged: srv.PushRuntimeConfig` cannot work (portal service is built before the gateway Server) — follow the cert pattern instead: wire a small indirection `var runtimePush func(string)` closure… NO. Copy the EXACT cert pattern: `OnCertificateIssued: agentStreams.NotifyCertUpdate` is registry-direct. Here assembly needs the portal itself, so: create the portal service first (hook left nil), build the gateway Server, then set the hook via a portal setter `portalService.SetRuntimeConfigChangedHook(srv.PushRuntimeConfig)` added in Task 5 (exported setter, one line, called in all three driver wirings in main.go). Check how `cmd/gateway` builds per-driver deps (`sqlDeps`/`memoryDeps`) and place the setter after `gateway.New`.

**Steps:**

- [ ] **Step 1: Failing tests**
  - `TestNotifyRuntimeConfigDeliversFullPayload` (registry test with the fake `agentSocket` used by existing registry tests): enqueue → writer sends `{"type":"runtime_config","data":{...}}`; full queue drops without blocking.
  - `TestIngestTelemetryParsesFeatures`: sample with `capabilities: {"features":["runtime_manager"],"agent_version":"0.2.0"}` → `srv.AgentFeatures.Has("mock-host-qwen","runtime_manager")` true; malformed capabilities → false, sample still accepted.
  - `TestPushRuntimeConfigSkipsUndeclaredAgent`: no features reported → no frame enqueued; declared → frame with correct etag payload arrives (poll with waitFor-style loop since push is async).
  - Wiring: extend `cmd/gateway/agent_stream_wiring_test.go` with a source-scan assertion that `SetRuntimeConfigChangedHook` is wired for all three drivers.
- [ ] **Step 2: Run to verify failure.**
- [ ] **Step 3: Implement** (registry method + features registry + PushRuntimeConfig + ingest parse + setter + main.go wiring).
- [ ] **Step 4: Run to verify pass** — `go test ./internal/gateway/ ./cmd/... -run 'Runtime|Features|Wiring' -v`.
- [ ] **Step 5: Commit** — `git commit -m "feat(gateway): push runtime-config over the agent stream, feature-gated"`

---

### Task 9: Runtime status ingest + report ingest + portal SSE

**Files:**
- Modify: `gateway/backend/internal/gateway/agent_ingest.go` (RuntimeSample wire structs + ingest), `agent_runtime.go` (report ingest + POST handler + WS case), `agent_stream.go` (WS `runtime_report` case), `runtime_registry.go` (status registry), `portal_runtime_endpoints.go` (SSE + status/report reads), `server.go` (routes), portal `service_runtime.go` (+api.go+tracing: `ServerRuntimeReport` read + `RuntimeStatusView` authz helper)
- Test: gateway ingest/SSE/report tests, portal service tests

**Interfaces:**
- Consumes: Task 4 store, Task 7 DTOs. Produces (wire contract for agent Tasks 17/18; portal contract for Task 22):

```go
// agent_ingest.go — additive, optional (legacy payloads still decode)
type agentRuntimeGPUSample struct {
	Index          int `json:"index"`
	VRAMMeasuredMB int `json:"vram_measured_mb"`
}
type agentRuntimeError struct {
	Message    string    `json:"message"`
	At         time.Time `json:"at"`
	ExitCode   int       `json:"exit_code"`
	Failures   int       `json:"failures"`
	StderrTail string    `json:"stderr_tail,omitempty"` // clamped to 2048 bytes on ingest
}
type agentRuntimeSample struct {
	SpecID   string                  `json:"spec_id"`
	Model    string                  `json:"model"` // upstream model name
	State    string                  `json:"state"`
	Since    time.Time               `json:"since"`
	PID      int                     `json:"pid,omitempty"`
	Port     int                     `json:"port,omitempty"`
	InFlight int                     `json:"in_flight"`
	Restarts int                     `json:"restarts"`
	GPUs     []agentRuntimeGPUSample `json:"gpus,omitempty"`
	LastError *agentRuntimeError     `json:"last_error,omitempty"`
}
// on agentTelemetryRequest: Runtimes []agentRuntimeSample `json:"runtimes"`

// runtime_registry.go — the serverPerfRegistry shape (atomic snapshot+subscribe,
// non-blocking delivery, nil-safe):
type runtimeStatusRegistry struct{ ... }
func (r *runtimeStatusRegistry) publish(serverID string, statuses []RuntimeStatusDTO)
func (r *runtimeStatusRegistry) subscribe(serverID string) ([]RuntimeStatusDTO, <-chan []RuntimeStatusDTO, func())
type RuntimeStatusDTO struct { // mirrors agentRuntimeSample, json tags identical
	SpecID string `json:"spec_id"`; Model string `json:"model"`; State string `json:"state"`
	Since time.Time `json:"since"`; PID int `json:"pid,omitempty"`; Port int `json:"port,omitempty"`
	InFlight int `json:"in_flight"`; Restarts int `json:"restarts"`
	LastError *RuntimeErrorDTO `json:"last_error,omitempty"`
}
type RuntimeErrorDTO struct { Message string `json:"message"`; At time.Time `json:"at"`; ExitCode int `json:"exit_code"`; Failures int `json:"failures"`; StderrTail string `json:"stderr_tail,omitempty"` }
```

- Ingest additions in `ingestTelemetrySample`, all AFTER the store writes (the "evidence only after persistence" convention): (a) `s.RuntimeStatus.publish(serverID, statuses)`; (b) VRAM write-back: for each sample GPU with `VRAMMeasuredMB > 0`, load the spec (`RuntimeSpecByMapping` is by mapping — need by-ID; the sample carries spec_id, so add nothing: `UpdateRuntimeSpecGPUMeasured(ctx, specID, index, measured)` directly, ignoring ErrNotFound) — but ONLY when the spec is not `VRAMLocked`; resolving lock state needs a read: keep a small per-cycle guard — read the spec once per distinct spec_id via `RuntimeSpecsByApplication`… simpler: add `RuntimeSpecByID(ctx, id) (RuntimeSpec, bool, error)` to RuntimeStore in this task (SQL+memory+conformance one-liner) and consult `VRAMLocked` before writing. Write-backs are best-effort (log Debug on error, never reject the sample).
- Report ingest (`ingestRuntimeReport(ctx, serverID string, raw json.RawMessage) error` in agent_runtime.go, mirroring `ingestSystemReport`): unmarshal into a gateway-side mirror struct `agentRuntimeReport{ Source string `json:"source"`; CollectedAt time.Time `json:"collected_at"`; ParseError string `json:"parse_error,omitempty"`; Config json.RawMessage `json:"config"` }`; sanitize: clamp strings, and **defense in depth: re-parse Config, overwrite every env value with "•••", re-marshal** — even a buggy agent cannot make the gateway store a secret; canonical re-marshal; `UpsertServerRuntimeReport`. POST handler `handleAgentRuntimeReport` (the `handleAgentSystemReport` skeleton, error writer with 400 `agent.runtime_report_invalid` / 404 `agent.unknown_server` / 500 `agent.runtime_report_failed`); WS `case "runtime_report":` in agent_stream.go's dispatch with the standard 3-way error mapping. New `agentRoutes` row `/api/agent/v1/runtime-report`.
- Report ingest also flips a volatile per-server flag: `s.RuntimeStatus.SetFileMode(serverID, report.Source == "file")` (nil-safe; consulted by Task 8's push so the gateway stops sending `runtime_config` frames to file-mode agents — the agent discards them anyway, doubly harmless).
- Portal reads: `GET /api/portal/servers/{id}/runtime/report` → portal method `ServerRuntimeReportView(ctx, principal, serverID)` (authorizeServer; absent → `{"available":false}` DTO, the hardware-panel model). The view DTO also carries `agent_version string` and `agent_features []string` read from the latest persisted telemetry row (`server_telemetry.agent_version` / parsed `capabilities`), so Task 22 can render the feature-mismatch banner without a new endpoint; `GET /api/portal/servers/{id}/runtime/events` → SSE handler `handleRuntimeEvents(w,r,token,serverID)`: EXACTLY the `handleBenchmarkEvents` skeleton (ownership check via `s.Portal.GetServer` first, flusher check, headers, `SetWriteDeadline(time.Time{})`, atomic snapshot+subscribe, event names `snapshot`/`update`, 25 s `: ping` heartbeat).

**Steps:**

- [ ] **Step 1: Failing tests** — ingest publishes to registry + subscriber gets update frame; stderr tail clamped to 2048; VRAM write-back happens (assert store row), skipped when locked, tolerated when spec unknown; runtime report POST 200 + stored canonical blob has all env values masked even when the agent sent plaintext; WS runtime_report frame ingests (model: existing system_report WS test); SSE endpoint streams snapshot then update on `publish` (model: perf SSE test with `readPerfSSEFrame`); report view returns available:false when absent; 404-no-leak for foreign users.
- [ ] **Step 2: Run to verify failure.**
- [ ] **Step 3: Implement** (+ `RuntimeSpecByID` store method with conformance line + tracing regen + api.go).
- [ ] **Step 4: Run to verify pass** — `go test ./internal/gateway/ ./internal/portal/ ./internal/store/ -run 'Runtime' -v`.
- [ ] **Step 5: Commit** — `git commit -m "feat(gateway): runtime status ingest, report ingest, portal SSE"`

---

### Task 10: server_agent application type + timeout default

**Files:**
- Modify: `gateway/backend/internal/portal/service_applications.go` (type switch + type-aware timeout default), `cmd/gateway/main.go` (providerClients), tests alongside

**Interfaces:**
- Consumes: `routing.ProviderServerAgent` (Task 1).
- Produces: `server_agent` is a valid `Application.Type` everywhere; its `TimeoutMS` default is **600000** (10 min) instead of 30000.

**Steps:**

- [ ] **Step 1: Failing tests** — portal: `CreateApplication` with type `server_agent` succeeds; with `TimeoutMS: 0` the stored value is 600000; existing types still default 30000; PATCH `TimeoutMS: 0` on a server_agent app re-applies 600000. cmd/gateway: extend the existing providerClients test (grep `TestBuildGatewayServerDispatchesConfiguredProviderRoutes`) so a `server_agent` application dispatches through the OpenAI-compatible client.
- [ ] **Step 2: Run to verify failure.**
- [ ] **Step 3: Implement** — `normalizeApplicationType` gains `case routing.ProviderServerAgent`; change `normalizeApplicationTimeoutMS(timeoutMS int)` to `normalizeApplicationTimeoutMS(appType string, timeoutMS int)` returning 600000 for server_agent / 30000 otherwise (update BOTH call sites: create + update; on update pass the app's type); add `const defaultServerAgentTimeoutMS = 600000` next to the existing consts; main.go providerClients map gains `routing.ProviderServerAgent: openAICompatible`.
- [ ] **Step 4: Run to verify pass** — `go test ./internal/portal/ ./cmd/... -v` (focused runs), then full `cd gateway/backend && go test ./...`.
- [ ] **Step 5: Commit** — `git commit -m "feat: server_agent application type with cold-load-aware timeout default"`

---

### Task 11: Agent — feature registry, version bump, capabilities

**Files:**
- Create: `server-agent/internal/agent/features.go`, `server-agent/internal/agent/features_test.go`
- Modify: `server-agent/internal/agent/agent.go` (Version + capabilities fill in collectOnce), `server-agent/internal/sample/sample.go` (helper)
- Test: `server-agent/internal/agent/agent_test.go` additions

**Interfaces:**
- Produces (consumed by Tasks 16–18 and main.go):

```go
// features.go
// Feature is one named capability this agent binary ships. Behavior gates on
// NAME EQUALITY against the gateway's declared set — never on version compare.
type Feature struct {
	Name  string // snake_case, unique
	Since string // SemVer at which the feature shipped; must be <= Version
}
// Features is the append-only registry. Adding an entry REQUIRES a MINOR
// version bump (enforced by TestFeatureRegistry).
var Features = []Feature{
	{Name: "runtime_manager", Since: "0.2.0"},
}
func FeatureNames() []string
// ActiveFeatures returns the intersection with the gateway's declared set.
func ActiveFeatures(gateway []string) []string
// capabilitiesJSON builds the sample's capabilities object:
// {"features":[...],"agent_version":Version}
func capabilitiesJSON() json.RawMessage
```

- `agent.go`: `const Version = "0.2.0"` (MINOR bump: new feature flag). `collectOnce` sets `s.Capabilities = capabilitiesJSON()` (today it is left empty and normalized to `{}`).

**Steps:**

- [ ] **Step 1: Write failing tests** (`features_test.go`):

```go
func TestFeatureRegistry(t *testing.T) {
	seen := map[string]bool{}
	for _, f := range Features {
		if !regexp.MustCompile(`^[a-z][a-z0-9_]*$`).MatchString(f.Name) {
			t.Errorf("feature name %q is not snake_case", f.Name)
		}
		if seen[f.Name] {
			t.Errorf("duplicate feature %q", f.Name)
		}
		seen[f.Name] = true
		if !semverLTE(f.Since, Version) { // helper in the test file: parse "x.y.z", compare
			t.Errorf("feature %q Since %s > Version %s — bump agent.Version", f.Name, f.Since, Version)
		}
	}
}
func TestActiveFeaturesIsIntersection(t *testing.T) {
	got := ActiveFeatures([]string{"runtime_manager", "unknown_future"})
	if len(got) != 1 || got[0] != "runtime_manager" { t.Fatalf("got %v", got) }
	if len(ActiveFeatures(nil)) != 0 { t.Fatal("empty gateway set must disable everything") }
}
func TestCapabilitiesJSONShape(t *testing.T) {
	var v struct {
		Features     []string `json:"features"`
		AgentVersion string   `json:"agent_version"`
	}
	if err := json.Unmarshal(capabilitiesJSON(), &v); err != nil { t.Fatal(err) }
	if v.AgentVersion != Version || len(v.Features) == 0 { t.Fatalf("%+v", v) }
}
```

Plus in agent_test.go: assert a collected sample's `Capabilities` round-trips as that shape (find the existing collectOnce test that inspects the posted sample and extend it).

- [ ] **Step 2: Run to verify failure** — `cd server-agent && go test ./internal/agent/ -run 'Feature|Capabilities' -v` → compile FAIL.
- [ ] **Step 3: Implement** features.go + Version bump + collectOnce fill.
- [ ] **Step 4: Run to verify pass** — `go test ./internal/agent/ ./internal/sample/ -v`.
- [ ] **Step 5: Commit** — `git commit -m "feat(agent): feature registry, capabilities reporting, version 0.2.0"`

---

### Task 12: Agent — runtime types + admission policy (pure functions)

**Files:**
- Create: `server-agent/internal/runtime/types.go`, `server-agent/internal/runtime/policy.go`
- Modify: `server-agent/internal/archtest/arch_test.go` (add `"internal/runtime": {"internal/gwapi"}` — alphabetically between proxy and sample; every package must be listed even as a leaf)
- Test: `server-agent/internal/runtime/types_test.go`, `policy_test.go`

**Interfaces:**
- Produces (the package's core vocabulary; Tasks 13–18 build on it):

```go
// types.go — wire mirror of the gateway's AgentRuntimeConfigDTO (spec §11).
type SpecGPU struct {
	Index  int `json:"index"`
	VRAMMB int `json:"vram_mb"` // 0 = unknown
}
type Spec struct {
	ID                          string            `json:"id"`
	Model                       string            `json:"model"`
	UpstreamModel               string            `json:"upstream_model"`
	Binary                      string            `json:"binary"`
	Args                        []string          `json:"args"`
	Env                         map[string]string `json:"env"`
	WorkDir                     string            `json:"work_dir"`
	GPUs                        []SpecGPU         `json:"gpus"`
	ListenPort                  int               `json:"listen_port"`
	HealthPath                  string            `json:"health_path"`
	HealthTimeoutSeconds        int               `json:"health_timeout_seconds"`
	StartupTimeoutSeconds       int               `json:"startup_timeout_seconds"`
	IdleTimeoutSeconds          int               `json:"idle_timeout_seconds"`
	AdmissionWaitTimeoutSeconds int               `json:"admission_wait_timeout_seconds"`
	Pinned                      bool              `json:"pinned"`
	AdminState                  string            `json:"admin_state"` // "" | "force_running" | "force_stopped"
}
type GPUBudget struct {
	Index    int `json:"index"`
	BudgetMB int `json:"budget_mb"`
}
type Config struct {
	RouterListen int         `json:"router_listen"`
	MaxProcesses int         `json:"max_processes"`
	GPUBudgets   []GPUBudget `json:"gpu_budgets"`
	Specs        []Spec      `json:"specs"`
	Coresident   [][2]string `json:"coresident"` // spec-ID pairs
	ETag         string      `json:"etag"`
}
// ParseConfig unmarshals + validates. Tolerant of unknown fields (forward
// compat); intolerant of structural nonsense (duplicate spec IDs, a pair
// naming an unknown spec — dropped with a warning, not an error).
func ParseConfig(raw []byte) (Config, error)

// State machine (spec §7)
type State string
const (
	StateStopped           State = "stopped"
	StateStarting          State = "starting"
	StateRunning           State = "running"
	StateDraining          State = "draining"
	StateBackoff           State = "backoff"
	StateStartFailed       State = "start_failed"
	StateCrashed           State = "crashed"
	StatePendingVRAMUnknown State = "pending_vram_unknown"
	StateNotPermitted      State = "not_permitted"
)
type LastError struct {
	Message    string
	At         time.Time
	ExitCode   int
	Failures   int
	StderrTail string // bounded, ~2 KiB
}
type Status struct {
	SpecID    string
	Model     string // upstream model name
	State     State
	Since     time.Time
	PID       int
	Port      int
	InFlight  int
	Restarts  int
	MeasuredVRAM map[int]int // gpu index -> MB, when measured
	LastError *LastError
}

// policy.go — pure functions over snapshots. NO clocks, NO I/O.
type RunningProc struct {
	SpecID   string
	GPUs     map[int]int // index -> effective VRAM MB (measured if known, else estimate)
	InFlight int
	Pinned   bool
	LastUsed time.Time // for idle-victim ordering
}
type PolicySnapshot struct {
	Running      []RunningProc
	MaxProcesses int              // 0 = unlimited
	Budgets      map[int]int      // gpu index -> budget MB; missing index = no budget for that GPU
	Allowed      map[[2]string]bool // canonical (a<b) spec-ID pair set
}
type Decision struct {
	OK      bool
	Reason  State    // when !OK && !Wait: StatePendingVRAMUnknown | StateNotPermitted-like blockers
	Evict   []string // spec IDs to drain-stop first (idle victims, oldest LastUsed first)
	Wait    bool     // blocked only by busy/pinned processes → queue and retry on completion
}
// Admit answers: may spec start next to Running? It applies spec §5's three
// rules + the unknown-VRAM-alone rule, and computes the cheapest idle-victim
// set when eviction unblocks the start.
func Admit(snap PolicySnapshot, spec Spec) Decision
func PairKey(a, b string) [2]string // canonical ordering helper
```

- Admit semantics, precisely:
  1. Every running proc must be pair-allowed with spec (`Allowed[PairKey(spec.ID, r.SpecID)]`); disallowed + idle → eviction candidate; disallowed + busy/pinned → wait.
  2. Process-count: `len(Running)+1 > MaxProcesses (>0)` → evict idle (oldest first) or wait.
  3. Per-GPU arithmetic: for each GPU g in spec.GPUs with `VRAMMB > 0`: sum over running procs touching g + spec ≤ `Budgets[g]` (a g absent from Budgets = unbudgeted = no constraint). Overflow → evict idle procs touching g (oldest first) or wait.
  4. Unknown VRAM (`any spec.GPUs[i].VRAMMB == 0`): spec may start only if NO running proc touches any of its GPUs → else `Decision{OK:false, Reason:StatePendingVRAMUnknown}` when the blockers are pinned, or Evict/Wait when evictable/busy.
  5. Pinned running procs are never in `Evict`.

**Steps:**

- [ ] **Step 1: Write the failing tests** — this task is the correctness core; the tests are the deliverable:

```go
func TestParseConfigRoundTrip(t *testing.T)        // spec §11 example JSON parses; unknown top-level fields ignored
func TestParseConfigDropsUnknownPairSpecs(t *testing.T)
func TestParseConfigRejectsDuplicateSpecIDs(t *testing.T)
func TestPairKeyCanonical(t *testing.T)

func TestAdmit(t *testing.T) {
	// Table-driven over PolicySnapshot × Spec → Decision. Cases (minimum set):
	// 1 empty running set, budget fits            -> OK
	// 2 pair not allowed, blocker idle            -> Evict [blocker]
	// 3 pair not allowed, blocker busy            -> Wait
	// 4 pair not allowed, blocker pinned+idle     -> Wait (pinned never evicted)
	// 5 pair allowed, budget exceeded on gpu 0    -> Evict oldest idle toucher of gpu 0
	// 6 pair allowed, budget exceeded, all busy   -> Wait
	// 7 disjoint GPUs never compete               -> OK (two full GPUs, spec on gpu 1 only)
	// 8 process limit reached, idle victim exists -> Evict oldest
	// 9 process limit 0                           -> unlimited, OK
	// 10 unknown VRAM, gpu empty                  -> OK (measure-alone rule)
	// 11 unknown VRAM, gpu occupied by idle       -> Evict that proc
	// 12 unknown VRAM, gpu occupied by pinned     -> Reason: pending_vram_unknown
	// 13 multi-gpu spec (tensor parallel), one gpu over budget -> evicts on that gpu only
	// 14 eviction must pick OLDEST LastUsed among idle candidates
	// 15 measured VRAM (in RunningProc.GPUs) is what counts, not estimates
	// 16 budget missing for a touched gpu -> that gpu unconstrained
}
```

Every case asserts the full Decision (OK, Wait, Reason, exact Evict slice in order).

- [ ] **Step 2: Run to verify failure** — `cd server-agent && go test ./internal/runtime/ -v` → compile FAIL (package does not exist). The archtest reverse check will also fail until arch_test.go lists the package — add the `allowedDeps` entry in this task.
- [ ] **Step 3: Implement** types.go + policy.go (pure; the only imports are stdlib). Keep `Admit` a small pipeline: collect blockers per rule → partition evictable/busy/pinned → build the decision.
- [ ] **Step 4: Run to verify pass** — `go test ./internal/runtime/ ./internal/archtest/ -v`.
- [ ] **Step 5: Commit** — `git commit -m "feat(agent): runtime config types and admission policy"`

---

### Task 13: Agent — local policy + placeholder expansion

**Files:**
- Create: `server-agent/internal/runtime/policy_local.go`, `policy_local_test.go`
- Modify: `server-agent/internal/config/config.go` (+ config_test.go) — the OP_AGENT_RUNTIME_* settings

**Interfaces:**
- Produces:

```go
// policy_local.go
type LocalPolicy struct {
	AllowedBinaries []string // absolute paths; EMPTY = nothing may start (spec decision 2)
	AllowedDirs     []string // permitted work_dir prefixes; empty = any work_dir
}
// Permit returns nil, or an error naming the violated rule (surfaced as
// state not_permitted + last_error message).
func (p LocalPolicy) Permit(spec Spec) error
// ExpandPlaceholders resolves ${AGENT_ENV:NAME} (from getenv) and ${PORT}
// (the chosen listen port) in args and env values. Unknown ${AGENT_ENV:X}
// (empty getenv result) is an error — a missing secret must fail loudly at
// start time, not launch a child with an empty token.
func ExpandPlaceholders(spec Spec, port int, getenv func(string) string) (args []string, env []string, err error)
```

- `env` result is `os/exec`-shaped `KEY=value` strings; the child gets ONLY the expanded spec env plus a minimal base (`PATH`, `HOME` from the agent's own env) — never the agent's full environment (the agent's env contains its bearer token).
- Config additions (tri-source pattern from config.go, flag defaults EMPTY, precedence flag > env > file):
  - `RuntimeSource string` — `-runtime-source` / `OP_AGENT_RUNTIME_SOURCE` / `runtime_source`; "" → `RuntimeSourceGateway`; enum `gateway|file` validated in `Validate()` with the exact error format `fmt.Errorf("runtime-source must be %q or %q, got %q", ...)`.
  - `RuntimeConfigPath string` — `-runtime-config` / `OP_AGENT_RUNTIME_CONFIG` / `runtime_config`; required when source=file (Validate).
  - `RuntimeAllowedBinaries []string` — env `OP_AGENT_RUNTIME_ALLOWED_BINARIES` (comma-separated) / file `runtime_allowed_binaries` (JSON array); no flag (structured, the CertProxyRoutes precedent).
  - `RuntimeAllowedDirs []string` — same pattern, `OP_AGENT_RUNTIME_ALLOWED_DIRS` / `runtime_allowed_dirs`.
  - `RuntimeCachePath string` — `-runtime-cache` / `OP_AGENT_RUNTIME_CACHE` / `runtime_cache`; default `server-agent-runtime.cache.json` next to the binary (the defaultConfigName precedent).

**Steps:**

- [ ] **Step 1: Failing tests**
  - policy_local_test.go: empty allowlist rejects everything (message mentions the allowlist); listed binary passes; unlisted rejects; work_dir outside AllowedDirs rejects; `../` traversal in work_dir rejects (filepath.Clean + prefix check); ExpandPlaceholders: `${PORT}` in args → port string; `${AGENT_ENV:HF_TOKEN}` resolves via injected getenv; missing env var → error naming the variable; child env NEVER contains agent-only variables (assert exact env set).
  - config_test.go: `TestRuntimeSourcePrecedence` (file → env-over-file → flag-over-env, the TestCertModePrecedence skeleton), `TestRuntimeSourceDefaultsToGateway`, `TestValidateRuntimeSourceEnum`, `TestRuntimeFileModeRequiresConfigPath`, `TestRuntimeAllowedBinariesFromEnvCommaSeparated`.
- [ ] **Step 2: Run to verify failure.**
- [ ] **Step 3: Implement** (policy_local.go; config.go fields + fileConfig mirrors + resolution closures + Validate additions — remember: all flag defaults empty, Validate must accept a zero-value Config).
- [ ] **Step 4: Run to verify pass** — `go test ./internal/runtime/ ./internal/config/ -v`.
- [ ] **Step 5: Commit** — `git commit -m "feat(agent): local runtime policy and runtime config settings"`

---

### Task 14: Agent — process manager (serialized owner)

**Files:**
- Create: `server-agent/internal/runtime/manager.go`, `logs.go`, `manager_test.go`, `logs_test.go`

**Interfaces:**
- Consumes: Tasks 12–13 (`Spec`, `Admit`, `LocalPolicy`, `ExpandPlaceholders`).
- Produces (Tasks 15/18 consume):

```go
type ManagerOptions struct {
	Policy  LocalPolicy
	Getenv  func(string) string // os.Getenv in prod; injected in tests
	// Test seams (package-level var pattern like client.backoffBase):
	// drainGrace, killGrace, backoffBase/Cap, healthPollInterval are package vars.
}
func NewManager(opts ManagerOptions) *Manager

// Apply reconciles desired state: new/changed specs, removed specs (drain),
// pinned/force_running starts, force_stopped drains. Safe to call repeatedly
// with the same Config (idempotent; ETag-equal configs are a no-op).
func (m *Manager) Apply(cfg Config)

// EnsureRunning is the router's entry point: returns the child's loopback
// base URL once the spec is Running, starting/evicting/queueing per policy.
// Blocks up to the spec's admission-wait + startup timeouts. The returned
// release func decrements the in-flight counter and stamps LastUsed.
func (m *Manager) EnsureRunning(ctx context.Context, upstreamModel string) (endpoint string, release func(), err error)
// Typed errors the router maps to HTTP (spec §6.5):
var (
	ErrModelNotManaged  = errors.New("runtime.model_not_managed")
	ErrStartFailed      = errors.New("runtime.start_failed")
	ErrStartTimeout     = errors.New("runtime.start_timeout")
	ErrAdmissionBlocked = errors.New("runtime.admission_blocked")
	ErrNotPermitted     = errors.New("runtime.not_permitted")
)

func (m *Manager) Status() []Status                 // snapshot for samples/portal
func (m *Manager) LoadedModels() []string           // upstream names in StateRunning ONLY
func (m *Manager) Transitions() <-chan struct{}     // buffered(1) coalesced wake on any state change
func (m *Manager) SetMeasurer(f func(pids []int) map[int]map[int]int) // pid -> gpu index -> MB (Task 18 wires nvidia-smi)
func (m *Manager) Close()                           // drain-stop everything, bounded
```

- **The serialized owner** (spec §6.3): one goroutine owns ALL state (specs, running procs, states, queue). Public methods communicate via a command channel; `EnsureRunning` sends an admission request and waits on a per-request reply channel. Only proxying (after EnsureRunning returns) is concurrent — release() sends a fire-and-forget completion command that also wakes queued waiters. Compute-and-start is one atomic step inside the owner: `Admit` → mark victims Draining + target Starting BEFORE any goroutine is spawned.
- Child lifecycle: `exec.Command(binary, args...)` (NO CommandContext — the manager owns termination), `Setpgid`, stdout+stderr both into the spec's ring buffer (logs.go); listen port: spec.ListenPort or grab-and-release a `127.0.0.1:0` ephemeral port; `${PORT}` expansion; health wait: poll `GET http://127.0.0.1:<port><HealthPath>` every healthPollInterval (500 ms) with per-probe timeout HealthTimeoutSeconds until 2xx or StartupTimeoutSeconds → `ErrStartTimeout` (child killed). Wait goroutine per child reports exit to the owner via command — the *runningProc pointer is the generation identity (proxy.Manager's retire discipline: a stale exit for a superseded generation is a no-op).
- Drain-stop: state Draining (router stops selecting it), wait for InFlight==0 up to drainGrace (10 s package var), then SIGTERM the process group, killGrace (5 s) later SIGKILL. Crash (exit while Running): record LastError{ExitCode, StderrTail: ring.Tail(2048), Failures++}, state Crashed, then Backoff with exponential delay (backoffBase 1 s, cap 60 s, stable-run ≥ 60 s resets failures — the WSSender discipline); a queued/incoming request during Backoff waits for the retry, it does not bypass it.
- `last_error` cleared ONLY by a successful start (spec §7). Idle ticker inside the owner loop: every 15 s scan for `IdleTimeoutSeconds>0 && InFlight==0 && now-LastUsed > idle` → drain-stop (never pinned/force_running).
- `logs.go`: `type ringBuffer struct{...}` fixed 64 KiB per process, `Write` (io.Writer, safe for concurrent stdout+stderr via internal mutex), `Tail(n int) string`.

**Steps:**

- [ ] **Step 1: Failing tests.** No re-exec helper exists in this repo (verified) — the house pattern is real short commands + tiny scripts in `t.TempDir()` (certinstall precedent). Build a `fakeChild(t)` helper that writes an executable shell script (unix-only tests; guard `runtime.GOOS != "windows"` skip like certinstall's) implementing a minimal HTTP model server in… shell is too weak for HTTP. Instead: compile a tiny helper binary ONCE per test run with `exec.Command("go", "build", "-o", dir+"/stubchild", "./testdata/stubchild")` — `testdata/stubchild/main.go` (~40 lines): flags `-port`, `-health-delay`, `-crash-after`, `-exit-code`; serves `GET /health` 200 after health-delay and `POST /v1/echo` echoing the body. testdata is excluded from the build and archtest (test-only). Tests:

```go
func TestManagerStartsAndServes(t *testing.T)            // Apply cfg, EnsureRunning -> endpoint answers /v1/echo; Status shows running, LoadedModels contains model
func TestManagerRespectsAllowlist(t *testing.T)          // binary not allowlisted -> ErrNotPermitted, state not_permitted
func TestManagerStartTimeout(t *testing.T)               // -health-delay 10s, startup_timeout 1 -> ErrStartTimeout, state start_failed, last_error set
func TestManagerEvictsIdleForIncompatible(t *testing.T)  // A running idle, B not pair-allowed -> EnsureRunning(B) drains A, starts B
func TestManagerNeverEvictsPinned(t *testing.T)          // A pinned -> EnsureRunning(B) with admission_wait 1 -> ErrAdmissionBlocked
func TestManagerQueueWakesOnCompletion(t *testing.T)     // A busy (in-flight held), B waits; release A -> B starts
func TestManagerDrainWaitsForInFlight(t *testing.T)      // in-flight request survives a drain-stop; process ends after release
func TestManagerCrashBackoffAndLastError(t *testing.T)   // -crash-after 100ms -exit-code 3 -> state crashed->backoff, last_error{exit_code:3, stderr tail}, restart after backoff; last_error survives until next SUCCESSFUL start
func TestManagerIdleTimeoutUnloads(t *testing.T)         // idle_timeout 1s (shrink ticker var) -> stopped
func TestManagerForceStoppedBlocksEnsure(t *testing.T)   // admin_state force_stopped -> ErrAdmissionBlocked immediately, no start
func TestManagerPinnedStartsOnApply(t *testing.T)        // pinned spec starts without a request
func TestManagerConcurrentEnsureSingleStart(t *testing.T) // 10 concurrent EnsureRunning for one cold spec -> exactly 1 process (count via stubchild pidfile)
func TestManagerTransitionsCoalesce(t *testing.T)        // Transitions() fires on state change; buffered(1)
func TestRingBufferTail(t *testing.T)
```

Shrink the package-var timings (drainGrace, backoffBase, healthPollInterval, idleTickInterval) in tests with save/restore (the hookTimeout precedent).

- [ ] **Step 2: Run to verify failure** — `go test ./internal/runtime/ -run TestManager -v` → compile FAIL.
- [ ] **Step 3: Implement** manager.go + logs.go. Keep the owner loop a single `for { select { case cmd := <-m.cmds: ...; case <-idleTick.C: ...; case <-m.closed: ... } }`; every state mutation happens there.
- [ ] **Step 4: Run to verify pass** — `go test ./internal/runtime/ -v -race` (the -race run is mandatory here).
- [ ] **Step 5: Commit** — `git commit -m "feat(agent): runtime process manager with serialized admission"`

---

### Task 15: Agent — router port

**Files:**
- Create: `server-agent/internal/runtime/router.go`, `router_test.go`

**Interfaces:**
- Consumes: `Manager.EnsureRunning/LoadedModels`, error sentinels (Task 14).
- Produces (Task 18 serves this on cfg.RouterListen):

```go
// NewRouter returns the router-port handler. The router serves BOTH
// "/health" and "/v1/health" as always-200 liveness paths (the agent does not
// know which HealthCheckPath the gateway application row configures, so it
// answers both; the portal's server_agent type default is "/v1/health").
func NewRouter(m *Manager) http.Handler
```

- Routes (spec §6.1):
  - `GET /health`, `GET /v1/health` → `200 {"status":"ok"}` unconditionally (liveness = the router accepts; never blocks on loads).
  - `GET /running` → llama-swap shape `{"running":[{"model":"<upstream>","state":"ready"}]}` from `m.LoadedModels()`; never blocks.
  - `GET /v1/models` → OpenAI shape `{"object":"list","data":[{"id":"<upstream>","object":"model"}]}` for ALL managed (enabled) specs — the gateway's model_sync health mode and model listing both hit this; it must answer instantly and include cold models (they are servable on demand).
  - everything else → model-routed proxy.
- Proxy flow: read body up to `maxBodyBytes = 32 << 20` (413 beyond); extract `model` and `stream` via a minimal `struct{ Model string; Stream bool }` unmarshal (unparseable body or empty model → `runtime.model_not_managed` 404); `m.EnsureRunning(r.Context(), model)`; forward with method/path/query/headers preserved (strip hop-by-hop headers), `http.Transport` WITHOUT timeouts (the gateway owns deadlines), stream the response with per-chunk `Flush` (never buffer).
- Heartbeats (spec §8.3): when `stream==true` and the child is not yet serving bytes: commit `200` + `Content-Type: text/event-stream` + flush, write `: keepalive\n\n` every `heartbeatInterval = 10 * time.Second` (package var) while EnsureRunning + the upstream request are pending; on upstream first bytes, splice the upstream body through verbatim (the upstream's own SSE lines — do NOT re-frame). Upstream non-2xx or error after heartbeats began → terminal SSE frame `data: {"error":{"code":"runtime.start_failed","message":"..."}}\n\n` + close. When `stream==false`: no heartbeats; plain proxy; error mapping per sentinel table.
- Error mapping (all pre-forward failures, JSON envelope `{"error":{"code","message"}}`): ErrModelNotManaged→404, ErrStartFailed→502, ErrStartTimeout→504, ErrAdmissionBlocked→503, ErrNotPermitted→502, upstream mid-request death→502 `runtime.upstream_gone`.

**Steps:**

- [ ] **Step 1: Failing tests** (httptest + real Manager with stubchild from Task 14):

```go
func TestRouterHealthAlwaysAnswers(t *testing.T)        // both paths 200 while a slow start is in progress
func TestRouterRunningLlamaSwapShape(t *testing.T)
func TestRouterModelsListsAllManagedSpecs(t *testing.T) // cold specs included
func TestRouterProxiesByModel(t *testing.T)             // POST /v1/chat/completions {"model":"m1"} lands on m1's child (echo asserts body+path)
func TestRouterUnknownModel404(t *testing.T)            // code runtime.model_not_managed
func TestRouterStreamHeartbeats(t *testing.T)           // stream:true + slow health-delay: client sees ": keepalive" comment(s) before data; heartbeatInterval shrunk to 20ms
func TestRouterStreamSplicesUpstream(t *testing.T)      // upstream SSE lines arrive verbatim after heartbeats
func TestRouterStreamStartFailureTerminalFrame(t *testing.T) // bad binary: 200 already committed, terminal {"error":{"code":"runtime.start_failed"...}} frame
func TestRouterNonStreamErrorCodes(t *testing.T)        // table over the sentinel->status map
func TestRouterNoResponseBuffering(t *testing.T)        // upstream writes two chunks with a gap; client Read sees the first before the second is written (flush proof)
```

- [ ] **Step 2: Run to verify failure.**
- [ ] **Step 3: Implement router.go.**
- [ ] **Step 4: Run to verify pass** — `go test ./internal/runtime/ -v -race`.
- [ ] **Step 5: Commit** — `git commit -m "feat(agent): model router port with cold-load heartbeats"`

---

### Task 16: Agent — config sources (gateway ETag + disk fallback; local file)

**Files:**
- Create: `server-agent/internal/runtime/config_client.go`, `config_client_test.go`

**Interfaces:**
- Consumes: `gwapi.Endpoint` (base+bearer+conditional GET), `ParseConfig` (Task 12), `Config.ETag`.
- Produces (Task 18's driver consumes):

```go
// Source is where the desired runtime config comes from. Both
// implementations return the SAME document type — the mode is a source
// switch, not a second code path (spec §10.2).
type Source interface {
	// Load returns the current config. changed=false means "same as last
	// Load" (ETag match / mtime unchanged). A transport error returns the
	// last known-good config with changed=false — transient gateway errors
	// keep current state (the RoutesClient discipline).
	Load(ctx context.Context) (cfg Config, changed bool, err error)
}

// GatewaySource: GET /api/agent/v1/runtime-config with If-None-Match, plus a
// disk cache so the agent starts (and keeps running) without the gateway.
func NewGatewaySource(gatewayURL, token string, client *http.Client, cachePath string) *GatewaySource
// ApplyPushed hands a WS-pushed full document (Task 17) to the source: it
// validates, persists the cache, updates the ETag, and returns the parsed
// config; a stale/equal ETag returns changed=false.
func (s *GatewaySource) ApplyPushed(raw []byte) (Config, bool, error)

// FileSource: operator-authored local file, same JSON schema; mtime-polled.
func NewFileSource(path string) *FileSource
```

- GatewaySource.Load: conditional GET via `gwapi.Endpoint.GetConditional` at `const runtimeConfigPath = "/api/agent/v1/runtime-config"`; 304 → cached config, changed=false; 404 → **feature absent on the gateway** (old build): keep the last known-good config, changed=false, log Debug once — a transiently downgraded gateway must not tear down a running set; 200 → `ParseConfig`, write cache, changed = etag differs.
- Disk cache: single-file atomic write in the certinstall `saveETagSidecar` style (temp file in the target dir, dot-prefixed name, chmod 0600 — the doc may carry env placeholder names; 0600 is cheap caution — rename; failure leaves the previous file untouched). On construction, load the cache if present so the agent can start processes before first gateway contact.
- FileSource.Load: `os.Stat` mtime vs last seen; unchanged → changed=false; changed → read+ParseConfig; parse error → keep last good, return it with changed=false AND record the error (exposed via `func (s *FileSource) LastParseError() (string, time.Time)` for the report, Task 17).

**Steps:**

- [ ] **Step 1: Failing tests** (httptest gateway fakes, the ws_test fakeGateway style):

```go
func TestGatewaySourceFetchAndCache(t *testing.T)        // 200 -> parsed cfg, cache file exists; new source from same cachePath starts with the cached cfg
func TestGatewaySourceETag304(t *testing.T)              // second Load sends If-None-Match, 304 -> changed=false
func TestGatewaySourceTransientErrorKeepsCurrent(t *testing.T) // 500/conn refused -> last good, changed=false, err=nil
func TestGatewaySource404KeepsCurrent(t *testing.T)      // old gateway
func TestGatewaySourceApplyPushed(t *testing.T)          // pushed doc applies + persists cache; same-etag push -> changed=false
func TestGatewaySourceCacheWriteFailureKeepsPrevious(t *testing.T) // fault-inject via package-level writeCacheFile var
func TestFileSourceMtimePoll(t *testing.T)               // unchanged mtime -> changed=false; edit -> new cfg
func TestFileSourceParseErrorKeepsLastGood(t *testing.T) // broken JSON -> last good + LastParseError set
```

- [ ] **Step 2: Run to verify failure.**
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run to verify pass** — `go test ./internal/runtime/ -v`.
- [ ] **Step 5: Commit** — `git commit -m "feat(agent): runtime config sources (gateway ETag + local file)"`

---

### Task 17: Agent — WS runtime frames, features client, report sender

**Files:**
- Modify: `server-agent/internal/client/ws.go` (+ ws_test.go), `server-agent/internal/client/client.go` (+ client_test.go)
- Create: `server-agent/internal/runtime/features_client.go`, `report.go` (+ tests)
- Modify: `server-agent/internal/archtest/arch_test.go` if new edges appear (runtime already has gwapi)

**Interfaces:**
- Produces:

```go
// ws.go — gateway->agent full-config push (spec §10.1). Latest-wins
// buffered(1) payload channel: a burst coalesces to the newest document
// (each frame is the FULL config, so dropping an older one is correct).
// nil payload = "resync via HTTP" (sent on every reconnect).
func (s *WSSender) RuntimeUpdates() <-chan json.RawMessage
// internal, called from readLoop case "runtime_config" (payload f.Data) and
// from maybeDial's connect hook (payload nil):
func (s *WSSender) wakeRuntimeConfig(data json.RawMessage) {
	select { case <-s.runtimeUpdates: default: } // drop the stale pending doc
	select { case s.runtimeUpdates <- data: default: }
}

// client.go + ws.go — the upward report (file mode), both transports:
// POST /api/agent/v1/runtime-report resp. frame {"type":"runtime_report",...}
func (c *Client) PostRuntimeReport(ctx context.Context, raw json.RawMessage) error
func (s *WSSender) PostRuntimeReport(ctx context.Context, raw json.RawMessage) error // cached like sysReport, re-sent on reconnect

// features_client.go
const featuresPath = "/api/agent/v1/features"
// FetchGatewayFeatures: ETag-conditional; 404 -> empty set, nil error (old
// gateway); transient error -> last known set, nil error.
type FeaturesClient struct{ ep *gwapi.Endpoint /* etag, last []string */ }
func NewFeaturesClient(gatewayURL, token string, client *http.Client) *FeaturesClient
func (c *FeaturesClient) Fetch(ctx context.Context) ([]string, error)

// report.go — file-mode upward report. REDACTION IS HERE: every env value
// is replaced by "•••" before marshal; keys survive (spec §10.2).
type Report struct {
	Source      string          `json:"source"` // "file" | "gateway"
	CollectedAt time.Time       `json:"collected_at"`
	ParseError  string          `json:"parse_error,omitempty"`
	Config      json.RawMessage `json:"config"`
}
func BuildReport(cfg Config, source string, parseErr string, at time.Time) ([]byte, error)
```

**Steps:**

- [ ] **Step 1: Failing tests**
  - ws_test.go: `TestRuntimeConfigFramePayloadDelivered` (serverPusher pushes `{"type":"runtime_config","data":{...}}` → RuntimeUpdates() yields exactly that data; remember to drain the connect-triggered nil wake first — the existing cert_update push test documents this gotcha); `TestRuntimeUpdatesLatestWins` (two pushes back-to-back → reader sees only the second); `TestReconnectWakesRuntimeNil` (reconnect → a nil payload arrives).
  - client_test.go: `TestPostRuntimeReport` (POST path, bearer, retry-on-5xx like telemetry).
  - features_client_test.go: 200+ETag → list; 304 → cached; 404 → empty, nil.
  - report_test.go: `TestBuildReportRedactsEnvValues` — a Config whose spec env contains `{"HF_TOKEN":"hf_secret_123"}` produces JSON where `hf_secret_123` does NOT appear anywhere (grep the bytes) and the key `HF_TOKEN` does; parse_error and source round-trip.
- [ ] **Step 2: Run to verify failure.**
- [ ] **Step 3: Implement.** readLoop gains `case "runtime_config": s.wakeRuntimeConfig(f.Data)`; NEVER a conn write from readLoop (one-writer rule); `maybeDial` adds `s.wakeRuntimeConfig(nil)` next to the existing cert/trust wakes.
- [ ] **Step 4: Run to verify pass** — `go test ./internal/client/ ./internal/runtime/ -v -race`.
- [ ] **Step 5: Commit** — `git commit -m "feat(agent): runtime config push channel, features client, redacted report"`

---

### Task 18: Agent — driver + run-loop + sample + main wiring

**Files:**
- Create: `server-agent/internal/runtime/driver.go` (+ test)
- Modify: `server-agent/internal/agent/agent.go` (+ agent_test.go), `server-agent/internal/sample/sample.go` (+ test), `server-agent/main.go`, `server-agent/internal/archtest/arch_test.go` (edges: `internal/agent` += `internal/runtime`; `mainPkgKey` += `internal/runtime`)
- Modify: `server-agent/internal/collector/nvidia.go` (+ test) — the compute-apps measurer

**Interfaces:**
- Consumes: everything from Tasks 11–17.
- Produces:

```go
// runtime/driver.go — what the agent loop sees (mirrors certProxyDriver):
type Driver struct{ /* manager, source, features, router server, reporter */ }
func NewDriver(m *Manager, src Source, features *FeaturesClient, reporter RuntimeReporter) *Driver
type RuntimeReporter interface{ PostRuntimeReport(ctx context.Context, raw json.RawMessage) error } // both clients satisfy it
// Sync: (1) fetch gateway features (gateway mode; cached/conditional);
// runtime_manager not mutually active -> stop everything, set a visible
// blocked note, return. (2) source.Load — or GatewaySource.ApplyPushed when
// the wake carried a payload; a FileSource IGNORES pushed payloads (spec
// §10.2: in file mode the gateway document is not consumed). (3) changed ->
// manager.Apply + (file mode) send the redacted report. Single-flight is the
// AGENT's trigger pattern, not ours.
func (d *Driver) Sync(ctx context.Context, pushed json.RawMessage)
func (d *Driver) Status() []Status
func (d *Driver) StartRouter(listen int) error // (re)binds the router listener when cfg.RouterListen changes
func (d *Driver) Close()

// sample/sample.go — additive, omitempty (byte-neutral for old agents):
type RuntimeGPUSample struct {
	Index          int `json:"index"`
	VRAMMeasuredMB int `json:"vram_measured_mb"`
}
type RuntimeErrorSample struct {
	Message    string    `json:"message"`
	At         time.Time `json:"at"`
	ExitCode   int       `json:"exit_code"`
	Failures   int       `json:"failures"`
	StderrTail string    `json:"stderr_tail,omitempty"`
}
type RuntimeSample struct {
	SpecID    string              `json:"spec_id"`
	Model     string              `json:"model"`
	State     string              `json:"state"`
	Since     time.Time           `json:"since"`
	PID       int                 `json:"pid,omitempty"`
	Port      int                 `json:"port,omitempty"`
	InFlight  int                 `json:"in_flight"`
	Restarts  int                 `json:"restarts"`
	GPUs      []RuntimeGPUSample  `json:"gpus,omitempty"`
	LastError *RuntimeErrorSample `json:"last_error,omitempty"`
}
// on Sample: Runtimes []RuntimeSample `json:"runtimes,omitempty"`

// agent.go — symmetric with certProxyDriver:
type runtimeDriver interface {
	Sync(ctx context.Context, pushed json.RawMessage)
	Status() []runtime.Status
}
type runtimeWaker interface{ RuntimeUpdates() <-chan json.RawMessage }
// Deps gains RuntimeDriver runtimeDriver (nil = feature absent: no ticker,
// no sample fields, no behavior — the no-op invariant).
```

- Run-loop additions (the nil-channel select discipline): `runtimeTicker` (`runtimePollInterval = 60 * time.Second` const; nil when no driver) + `case data := <-a.runtimeWake: a.triggerRuntimeSync(ctx, data)` + `case <-a.runtimeTransitions: a.collectOnce(ctx)` (immediate sample on state transitions, spec §7). `triggerRuntimeSync` copies the single-flight CompareAndSwap pattern of `triggerCertSync` verbatim (a pending payload lost to single-flight coalescing is safe: the next tick re-Loads and the source holds the latest pushed doc anyway).
- collectOnce: when driver non-nil, map `driver.Status()` → `s.Runtimes` (including measured VRAM per GPU) and set `s.LoadedModels` **authoritatively from the manager** (running states only), overriding the `Loaded` lister when the runtime feature is active.
- Measurement (spec §5): `collector.NewNvidiaComputeApps()` in nvidia.go — runs `nvidia-smi --query-compute-apps=pid,gpu_uuid,used_memory --format=csv,noheader,nounits`, maps gpu_uuid→index via the existing `--query-gpu` output, returns `map[pid]map[gpuIndex]usedMB`; wired into the manager via `SetMeasurer` in main.go (nil on hosts without nvidia-smi — Apple/AMD keep operator estimates, spec §5). Parse function pure + unit-tested with canned CSV.
- main.go wiring (after the poster switch, the proxy-driver pattern):

```go
if runtimeActive { // fetched features intersected with agent.Features at startup;
                   // also require cfg.RuntimeSource validation passed
	mgr := runtimectl.NewManager(runtimectl.ManagerOptions{Policy: localPolicy, Getenv: os.Getenv})
	defer mgr.Close()
	var src runtimectl.Source
	if cfg.RuntimeSource == config.RuntimeSourceFile {
		src = runtimectl.NewFileSource(cfg.RuntimeConfigPath)
	} else {
		src = runtimectl.NewGatewaySource(cfg.GatewayURL, cfg.Token, trustStore.HTTPClient(30*time.Second), cfg.RuntimeCachePath)
	}
	var reporter runtimectl.RuntimeReporter // nil when the poster cannot report (never assert unchecked)
	if r, ok := deps.Poster.(runtimectl.RuntimeReporter); ok {
		reporter = r // typed-nil discipline: assign only inside the ok branch
	}
	drv := runtimectl.NewDriver(mgr, src, featuresClient, reporter)
	deps.RuntimeDriver = drv
}
```

(import alias `runtimectl "op-ai-server-agent/internal/runtime"` — the package name `runtime` collides with the stdlib import already present in agent.go; alias at every import site.)

**Steps:**

- [ ] **Step 1: Failing tests** — driver_test.go (fake Source/Manager: Sync applies only on changed; pushed payload routes to ApplyPushed; feature-inactive stops the manager; file mode sends the report on change); agent_test.go (fake runtimeDriver: nil driver → no `runtimes` key in the marshaled sample (byte-neutrality); non-nil → Runtimes present + LoadedModels overridden; a Transitions wake produces an immediate extra Post — reuse `waitUntil`); sample_test.go (Normalize keeps Runtimes nil-absent); nvidia parse test with canned compute-apps CSV.
- [ ] **Step 2: Run to verify failure.**
- [ ] **Step 3: Implement** (driver.go, agent.go seams, sample fields, nvidia measurer, main.go, archtest entries — same change).
- [ ] **Step 4: Run to verify pass** — `cd server-agent && go test ./... -race`.
- [ ] **Step 5: Commit** — `git commit -m "feat(agent): runtime driver wired into the agent loop and samples"`

---

### Task 19: Frontend — api module, type union, i18n

**Files:**
- Modify: `gateway/frontend/src/api/models.ts` (union), `gateway/frontend/src/api.ts` (barrel)
- Create: `gateway/frontend/src/api/runtime.ts`
- Modify: `gateway/frontend/src/components/shared/applicationTypeDefaults.ts` (+ test), `gateway/frontend/src/i18n.ts`, `gateway/frontend/src/components/shared/format.ts` (errorLabelByCode)
- Test: `gateway/frontend/src/api/runtime.test.ts` (if the api modules have tests — check; otherwise covered via component tests), `applicationTypeDefaults.test.ts`

**Interfaces:**
- Produces (Tasks 20–22 consume; JSON mirrors the Go DTOs from Tasks 5–9 byte-for-byte):

```ts
// api/models.ts
export type ApplicationType = 'ollama' | 'vllm' | 'llama_cpp' | 'llama_swap' | 'litellm' | 'server_agent';

// api/runtime.ts
export interface RuntimeSpecGPU { index: number; vram_estimate_mb: number; vram_measured_mb: number }
export interface RuntimeSpec {
  configured: boolean; id?: string; mapping_id: string; enabled: boolean;
  binary: string; args: string[]; env: Record<string, string>; work_dir: string;
  listen_port: number; health_path: string; health_timeout_seconds: number;
  startup_timeout_seconds: number; idle_timeout_seconds: number;
  admission_wait_timeout_seconds: number; pinned: boolean; admin_state: string;
  vram_locked: boolean; gpus: RuntimeSpecGPU[];
}
export type PutRuntimeSpecRequest = Omit<RuntimeSpec, 'configured' | 'id' | 'mapping_id'>;
export interface CoResidency { pairs: [string, string][] }
export interface GPUBudget { index: number; budget_mb: number; expected_uuid: string; expected_name: string }
export interface RuntimeStatus {
  spec_id: string; model: string; state: string; since: string;
  pid?: number; port?: number; in_flight: number; restarts: number;
  last_error?: { message: string; at: string; exit_code: number; failures: number; stderr_tail?: string };
}
export interface RuntimeReport { available: boolean; source?: string; collected_at?: string; parse_error?: string; config?: unknown; agent_version?: string; agent_features?: string[] }

export function runtimeApi(fetcher: Fetcher) {
  return {
    runtimeSpec: (mappingId: string) => request<RuntimeSpec>(fetcher, `/api/portal/mappings/${encodeURIComponent(mappingId)}/runtime-spec`),
    putRuntimeSpec: (mappingId: string, body: PutRuntimeSpecRequest) => request<RuntimeSpec>(fetcher, `/api/portal/mappings/${encodeURIComponent(mappingId)}/runtime-spec`, { method: 'PUT', body }),
    deleteRuntimeSpec: (mappingId: string) => request<{ ok: boolean }>(fetcher, `/api/portal/mappings/${encodeURIComponent(mappingId)}/runtime-spec`, { method: 'DELETE' }),
    runtimeCoresidency: (appId: string) => request<CoResidency>(fetcher, `/api/portal/applications/${encodeURIComponent(appId)}/runtime/coresidency`),
    putRuntimeCoresidency: (appId: string, body: CoResidency) => request<CoResidency>(fetcher, `/api/portal/applications/${encodeURIComponent(appId)}/runtime/coresidency`, { method: 'PUT', body }),
    runtimeWarnings: (appId: string) => request<{ warnings: string[] }>(fetcher, `/api/portal/applications/${encodeURIComponent(appId)}/runtime/warnings`),
    gpuBudgets: (serverId: string) => request<{ budgets: GPUBudget[] }>(fetcher, `/api/portal/servers/${encodeURIComponent(serverId)}/gpu-budgets`),
    putGpuBudgets: (serverId: string, body: { budgets: GPUBudget[] }) => request<{ budgets: GPUBudget[] }>(fetcher, `/api/portal/servers/${encodeURIComponent(serverId)}/gpu-budgets`, { method: 'PUT', body }),
    runtimeReport: (serverId: string) => request<RuntimeReport>(fetcher, `/api/portal/servers/${encodeURIComponent(serverId)}/runtime/report`),
    subscribeRuntimeStatus: (
      serverId: string,
      onData: (rows: RuntimeStatus[]) => void,
      onStatus?: (status: 'open' | 'error') => void,
    ): (() => void) => {
      const handle = (e: MessageEvent) => {
        try { onData((JSON.parse(e.data) as { data?: RuntimeStatus[] }).data ?? []); } catch { /* ignore a malformed frame */ }
      };
      return subscribeSSE(`/api/portal/servers/${encodeURIComponent(serverId)}/runtime/events`, { snapshot: handle, update: handle }, { onOpen: () => onStatus?.('open'), onError: () => onStatus?.('error') });
    },
  };
}
```

- `api.ts`: import + `export *` + spread `...runtimeApi(fetcher)` into `createPortalApi`.
- `applicationTypeDefaults.ts` — **choose option (a)** from the pattern brief (preservation contract): add `timeoutMs: number` to `TypeDefaults`, `timeoutMs: 30000` on all five existing entries, and the new entry:

```ts
  server_agent: { port: 8081, scheme: 'http', nativeResponses: false, nativeMessages: false,
    loadedModelsPath: '/running', loadedModelsFormat: 'llama_swap', contextProbePath: '', timeoutMs: 600000 },
```

`handleTypeChange` in ApplicationSection gains `if (patch.timeoutMs !== undefined) setTimeoutMs(patch.timeoutMs);`; `openCreate` seeds `setTimeoutMs(applicationTypeDefaults.ollama.timeoutMs)`; update the file's "deliberately excluded" comment. The tsc `Record<ApplicationType, TypeDefaults>` makes a forgotten entry a compile error. Add `'server_agent'` to `applicationTypeOptions` in ApplicationSection.tsx (the dropdown array — forgetting it silently hides the type).
- i18n: add the full key block to BOTH `de` and `en` at the same relative position (prefix `runtime…`): `runtimeAdmin`, `runtimeSpecs`, `runtimeSpecEdit`, `runtimeSpecBinary`, `runtimeSpecArgs`, `runtimeSpecEnv`, `runtimeSpecWorkDir`, `runtimeSpecGpus`, `runtimeSpecVram`, `runtimeSpecIdleTimeout`, `runtimeSpecStartupTimeout`, `runtimeSpecPinned`, `runtimeSpecEnabled`, `runtimeMatrix`, `runtimeMatrixHint`, `runtimeLimits`, `runtimeGpuBudget`, `runtimeMaxProcesses`, `runtimeLiveStatus`, `runtimeStateStopped/Starting/Running/Draining/Backoff/StartFailed/Crashed/PendingVram/NotPermitted`, `runtimeLastError`, `runtimeForceStart`, `runtimeForceStop`, `runtimeClearOverride`, `runtimeManagedLocally`, `runtimeManagedOnlyBanner`, `runtimeIneffectiveSpecs`, `runtimeTimeoutWarning`, `errorRuntimeSpecBinaryRequired`, `errorRuntimeSpecTuningInvalid`, `errorRuntimeCoresidencyPairInvalid`, `errorServerGpuBudgetInvalid`, `errorApplicationManagedRuntimeOnly` (+ `errorLabelByCode` entries mapping the backend codes to these keys).

**Steps:**

- [ ] **Step 1: Failing tests** — `applicationTypeDefaults.test.ts`: `expect(applicationTypeDefaults.server_agent).toEqual({...})` + a `migrateTypeFields` case proving a CUSTOMIZED timeout survives a type switch and a default one follows it; i18n.test.ts block asserting the new keys are non-empty strings in both locales.
- [ ] **Step 2: Run to verify failure** — `cd gateway/frontend && npm test` → tsc/compile FAIL.
- [ ] **Step 3: Implement** all files above.
- [ ] **Step 4: Run to verify pass** — `npm test && npm run build` (the build proves i18n parity).
- [ ] **Step 5: Commit** — `git commit -m "feat(portal): runtime api module, server_agent type, i18n"`

---

### Task 20: Frontend — RuntimeAdminSection (specs area) + entry wiring

**Files:**
- Create: `gateway/frontend/src/components/RuntimeAdminSection.tsx`, `RuntimeAdminSection.test.tsx`
- Modify: `gateway/frontend/src/components/ApplicationSection.tsx` (drill-down branch + managed-only banner), `gateway/frontend/src/components/ServerList.tsx` (api Pick + managed_runtime_only auto-drill)

**Interfaces:**
- Consumes: Task 19 api + shared components (Panel, Field, SelectField, ListTable, RowAction, StatusChip, ConfirmDialog, Breadcrumbs, useResource, useToast).
- Produces:

```ts
export function RuntimeAdminSection({ t, api, server, application, trail = [] }: Readonly<{
  t: Translation;
  api: Pick<PortalApi,
    | 'mappings' | 'createMapping' | 'updateMapping' | 'deleteMapping'
    | 'runtimeSpec' | 'putRuntimeSpec' | 'deleteRuntimeSpec'
    | 'runtimeCoresidency' | 'putRuntimeCoresidency' | 'runtimeWarnings'
    | 'gpuBudgets' | 'putGpuBudgets' | 'runtimeReport' | 'subscribeRuntimeStatus'
    | 'updateServer' | 'server'>;
  server: PortalServer;
  application: PortalApplication;
  trail?: BreadcrumbItem[];
}>)
```

- Entry wiring (the pattern brief's recipe, §1): in ApplicationSection's mappings drill-down branch, render `RuntimeAdminSection` instead of `MappingSection` when `app.type === 'server_agent'` (same `key`, fresh-row lookup, `trail` append). The row action label stays `t.mappingManage` — the runtime admin IS this application's model view (spec decision 5). No `views.tsx` change.
- `managed_runtime_only`: in ApplicationSection's list view, when `server.managed_runtime_only`: hide the create button unless no `server_agent` app exists yet, show an info banner (`t.runtimeManagedOnlyBanner` — add key in this task, both locales); when exactly one `server_agent` application exists, auto-drill: `useEffect` that `setMode({ kind: 'mappings', app })` on first load. Backend enforcement already exists (Task 6) — the UI mirrors it.
- The section (this task: layout + area 1): tab-like sub-navigation via MUI `Tabs` (specs / matrix / limits / status; matrix+limits+status get stubs rendered in Tasks 21–22 — the stub is an empty `Panel` with the heading, NOT `SectionStub` unless it fits). Area 1 "Launch specs": `ListTable` over mappings joined with their runtime specs (lazy: `useResource(() => api.mappings(application.id)...)` + per-row `api.runtimeSpec(mapping.id)` loaded once into a map). Columns: gateway model, upstream model, enabled, binary (basename), GPUs (e.g. "0: 22000 MB"), pinned, idle timeout, status badge placeholder (filled by Task 22's live map). Row actions: edit spec, delete spec. Form (create/edit sub-mode like ApplicationSection's): mapping fields (gateway/app model name — creating a mapping and its spec in one form: create mapping first via `api.createMapping`, then `putRuntimeSpec`) + all spec fields; args as a multiline textarea (one arg per line ⇄ string[]); env as key=value lines with a hint (`t.runtimeSpecEnvHint`: mentions `${AGENT_ENV:NAME}` und `${PORT}`); GPU rows editable (index + VRAM estimate). Timeout warning: `api.runtimeWarnings` → warning banner when non-empty.
- File-mode read-only comes in Task 22 (needs the report); this task renders assuming gateway mode.

**Steps:**

- [ ] **Step 1: Failing component tests** (the house skeleton: `messages.de`, hand-rolled fakeApi of vi.fn covering the FULL Pick, ToastProvider wrap):

```ts
it('renders the specs list with spec data')            // mapping + configured spec -> row shows binary basename, pinned chip
it('creates a mapping+spec through the form')          // fill fields, submit; assert createMapping then putRuntimeSpec bodies (args split, env parsed)
it('shows the timeout warning banner when the backend reports one')
it('opens RuntimeAdminSection instead of MappingSection for server_agent apps') // in ApplicationSection.test.tsx
it('auto-drills and hides create for managed_runtime_only servers')             // in ApplicationSection.test.tsx
```

- [ ] **Step 2: Run to verify failure** — `npm test` → FAIL. Expect tsc churn: adding api methods to ApplicationSection's Pick means EVERY existing fakeApi in ApplicationSection.test.tsx / ServerList.test.tsx / ManagementView.test.tsx needs the new `vi.fn`s — budget for it.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run to verify pass** — `npm test && npm run build`.
- [ ] **Step 5: Commit** — `git commit -m "feat(portal): runtime admin section with launch specs"`

---

### Task 21: Frontend — triangle matrix + server limits

**Files:**
- Create: `gateway/frontend/src/components/RuntimeMatrix.tsx`, `RuntimeMatrix.test.tsx`
- Modify: `RuntimeAdminSection.tsx` (areas 2+3)

**Interfaces:**
- Produces:

```ts
export function RuntimeMatrix({ t, specs, pairs, onToggle, budgets, disabled = false }: Readonly<{
  t: Translation;
  specs: { id: string; model: string; gpus: { index: number; vramMb: number }[] }[]; // display order = given order
  pairs: [string, string][];        // canonical a<b spec-id pairs
  onToggle: (a: string, b: string) => void; // fires with canonical order
  budgets: Record<number, number>;  // gpu index -> budget MB (for the tooltip sum)
  disabled?: boolean;               // file-mode read-only
}>)
```

- Rendering: an HTML table inside `Box sx={{ overflowX: 'auto' }}`; row i (specs[1..n-1]) × column j (specs[0..i-1]) — strictly lower triangle, diagonal and upper cells empty. Cell = `IconButton` (aria-label `` `${t.runtimeMatrixCell}: ${a.model} + ${b.model}` ``) showing check (allowed) / blocked icon; `Tooltip` with the per-GPU VRAM sum vs budget (`"GPU 0: 44000 / 46000 MB"`, red text when over — advisory only, the agent's arithmetic vetoes). Clicking calls `onToggle` with the canonical pair.
- Area 3 "Server limits" in RuntimeAdminSection: GPU budget rows (index, budget MB fields; prefill new rows from the server's latest telemetry GPUs — `PortalServer` carries GPU data via the hardware/telemetry DTO the HardwareSection uses; check what `api.server(serverId)` or the servers list exposes and reuse) + `runtime_max_processes` number field; saves via `putGpuBudgets` + `updateServer`. UUID-drift warning: compare budget `expected_uuid` with live GPU UUID when both non-empty → warning icon + tooltip (never blocks anything).
- Matrix data flow in the section: `useResource` for `runtimeCoresidency`; `onToggle` computes the new full pair list and `putRuntimeCoresidency` (full replace), optimistic `setData`.

**Steps:**

- [ ] **Step 1: Failing tests** — RuntimeMatrix.test.tsx: 3 specs render exactly 3 lower-triangle cells (n(n-1)/2); toggle fires canonical order even when visual row/col is (b,a); tooltip shows the summed VRAM per shared GPU and flags over-budget; disabled blocks clicks. Section tests: toggling persists the FULL replaced pair list.
- [ ] **Step 2: Run to verify failure.**
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run to verify pass** — `npm test && npm run build`.
- [ ] **Step 5: Commit** — `git commit -m "feat(portal): co-residency matrix and server limits UI"`

---

### Task 22: Frontend — live status, overrides, file-mode read-only

**Files:**
- Modify: `RuntimeAdminSection.tsx` (+ tests), `gateway/frontend/src/components/ModelList.tsx` if the model list exposes loaded badges for agent-managed mappings (check `ModelList.tsx` for the loaded indicator; extend its state rendering ONLY if a `RuntimeStatus` for the mapping's model is available via props already flowing there — otherwise leave ModelList untouched and note it in the PR as follow-up).

**Interfaces:**
- Consumes: `subscribeRuntimeStatus`, `runtimeReport`, `putRuntimeSpec` (admin_state).

- Area 4 "Live status": `useEffect` keyed `[api, server.id]` calling `api.subscribeRuntimeStatus(server.id, setRows, setStreamStatus)` returning the unsubscribe (the PerformanceSection pattern). `ListTable` columns: model, state (StatusChip: map runtime states → BadgeStatus — running→'active', starting→'watch', draining/backoff→'standby', crashed/start_failed/not_permitted→'error', stopped→'disabled', pending_vram_unknown→'watch'; label from the `runtimeState*` keys), since (formatDate), PID, port, in-flight, restarts, last_error (tooltip with message + `<pre>` stderr tail). Row actions per state: `runtimeForceStart` (sets admin_state force_running via putRuntimeSpec with the current spec body), `runtimeForceStop` (force_stopped), `runtimeClearOverride` (""). Restart = the UI sequence: force_stopped → wait for state stopped in the SSE rows → clear override (implement as a small state machine in the component with a visible progress chip).
- Status badges also flow into area 1's spec list (join rows by spec_id).
- File mode: `useResource(() => api.runtimeReport(server.id))`; when `report.available && report.source === 'file'`: banner `t.runtimeManagedLocally`, ALL edit affordances disabled (specs form, matrix `disabled`, budgets, no override buttons — spec §10.2), and the specs/matrix render from `report.config` instead of the gateway CRUD data; gateway-side specs existing anyway → warning `t.runtimeIneffectiveSpecs`.
- Feature mismatch: when gateway-side specs exist but the live status stream stays empty AND the server's reported agent features (exposed on the runtime report endpoint response — add `agent_features`/`agent_version` fields to that DTO in Task 9 if not already there; verify) lack `runtime_manager` → explanatory banner (spec §9 visibility). 

**Steps:**

- [ ] **Step 1: Failing tests** — SSE rows render with correct badges (fake subscribeRuntimeStatus invoking the callback synchronously); force-stop button PUTs admin_state=force_stopped preserving all other spec fields; restart sequence issues the three calls in order as states arrive; file-mode report flips everything read-only and renders report config; mismatch banner appears when features lack runtime_manager.
- [ ] **Step 2: Run to verify failure.**
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run to verify pass** — `npm test && npm run build`.
- [ ] **Step 5: Commit** — `git commit -m "feat(portal): live runtime status, overrides, file-mode read-only"`

---

### Task 23: E2E — the full circle

**Files:**
- Create: `gateway/e2e/playwright.runtime.config.ts`, `gateway/e2e/e2e-runtime/runtime.spec.ts`, `gateway/e2e/e2e-runtime/fixtures/stubserver/main.go`
- Modify: `gateway/e2e/package.json` (`"e2e:runtime": "playwright test -c playwright.runtime.config.ts"`), root `Makefile`/docs if the e2e catalog lists suites (check `docs/architecture/cross-cutting/development-and-quality.md` — update in Task 24)

**Steps:**

- [ ] **Step 1: Study `playwright.agent.config.ts`** (builds gateway + the REAL agent, fake `nvidia-smi` on PATH, memory-mode gateway with seeded agent token `dev-agent-secret`). Copy its global-setup approach.
- [ ] **Step 2: Write the stub model server** (`fixtures/stubserver/main.go`, own tiny go.mod or built via `go build` with the agent's module — simplest: a standalone main.go compiled with `go build -o /tmp/op-e2e-stubserver ./e2e-runtime/fixtures/stubserver` using the repo Go toolchain, no module needed via `GO111MODULE=off`? No — give it a 3-line go.mod). Behavior: `-port` flag; `GET /health` 200; `POST /v1/chat/completions` returns a minimal OpenAI JSON completion echoing the prompt; SPDX header.
- [ ] **Step 3: Write the spec** (`runtime.spec.ts`), asserting the full circle:
  1. Login (dev user), create AI server via portal UI, create application type `server_agent` port 8081.
  2. Open its model view → RuntimeAdminSection; create a spec: gateway model `e2e-model`, upstream `stub-model`, binary = the built stubserver path (the agent's allowlist env `OP_AGENT_RUNTIME_ALLOWED_BINARIES` must include it — set in the agent spawn env), args `["-port","${PORT}"]`.
  3. Global setup spawned the real agent (WS transport) with `OP_AGENT_RUNTIME_SOURCE=gateway`.
  4. Assert the live status badge reaches `stopped` (spec delivered), then POST an inference through the gateway (`/openai/v1/chat/completions`, model `e2e-model`, bearer dev token) → 200 with the echo.
  5. Assert the badge flips to running (SSE), and `loaded_models` shows on the server.
  6. Force-stop via the portal → badge `stopped`; a new inference re-starts it (on-demand proof).
- [ ] **Step 4: Run** — `cd gateway/e2e && npm run e2e:runtime` → PASS.
- [ ] **Step 5: Commit** — `git commit -m "test(e2e): runtime manager full-circle suite"`

---

### Task 24: Documentation + version/process rules

**Files:**
- Create: `docs/architecture/cross-cutting/agent-runtime-manager.md`
- Modify: `docs/architecture/05-building-block-view.md` (agent's `runtime` package + gateway additions), `06-runtime-view.md` (the on-demand start sequence), `docs/architecture/cross-cutting/compatibility-and-inference.md` (timeout-behavior facts from spec §8, incl. the documented heartbeat limitation), `docs/architecture/cross-cutting/telemetry-usage-observability.md` (RuntimeSample, capabilities), `docs/architecture/reference/api-surface.md` (all new endpoints incl. the three agent ones), `reference/data-model.md` (four tables + server columns), `reference/config-env.md` (OP_AGENT_RUNTIME_*), `docs/architecture/README.md` (index entry), `AGENTS.md` (agent version-bump rule: MINOR for a new feature flag — test-enforced; PATCH for any other agent change — PR-checklist item), `README.md` (feature claim + regenerate affected screenshots per the AGENTS.md screenshot rule: `make dev` stack, English UI, dark mode, 1600×1000 @2x)
- Then: run the Sonar gate (`make sonar-up` once, `make sonar-gate`; `make sonar-branch-findings` to narrow) and act on findings.

**Steps:**

- [ ] **Step 1: Write the new cross-cutting doc** — condense the spec's durable content (architecture, data model, admission rule, lifecycle states, timeout behavior, feature negotiation, config sources, error codes). This doc is what survives on `main`; the spec file will be deleted before the PR.
- [ ] **Step 2: Update the listed documents** — each gets only its own concern (no duplication; link to the new doc).
- [ ] **Step 3: Regenerate README screenshots** if the models/server views changed visibly (they did: new admin) — follow the existing capture process.
- [ ] **Step 4: Full verification** — from root: `make test-go && make test && (cd gateway/frontend && npm run build && npm audit --audit-level=moderate) && make test-e2e`, plus one store run with `OP_AI_GATEWAY_TEST_POSTGRES_DSN` set. Sonar gate.
- [ ] **Step 5: Commit** — `git commit -m "docs: agent runtime manager architecture documentation"`

**After Task 24** (not part of this plan's checkboxes): superpowers:requesting-code-review, then superpowers:finishing-a-development-branch — which includes deleting `docs/superpowers/` and `docs/implementation-status.md` from the branch, verifying `git diff --name-only main...HEAD` shows neither, pushing, and opening the PR (never merging).

---

## Execution notes

- Tasks 1–10 and 11–18 are two independent tracks after Task 1 (the wire contract in Tasks 7/9 is fixed by this plan, so the agent side can build against it without the gateway side merged) — but run them sequentially unless parallel worktrees are explicitly set up; the e2e task needs both.
- After every task: update `docs/implementation-status.md` (task done, verification result, next step).
- If a documented signature in this plan does not match the real code (the plan was written against 2026-08-25 HEAD `2003d87`), the real code wins — adapt the plan step and note the deviation in implementation-status.md.
