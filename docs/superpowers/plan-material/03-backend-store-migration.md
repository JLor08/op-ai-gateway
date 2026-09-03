# Plan material — Area 03: Backend persistence (schema, migrations, CRUD, parity, all three drivers)

Spec: `docs/superpowers/specs/2026-09-03-api-variant-endpoint-modes-design.md` (§3, §4, §7, §10).
All paths below are under the worktree
`/Users/jlor08/Developer/codex/op-ai-gateway/.worktrees/api-variant-modes/` (origin/main content).
Module path: `op-ai-gateway`. Tests run from `gateway/backend`.

Three store drivers per spec §7:
- **memory** = `routing.MemoryStore` (`internal/routing/memory_store.go`) — pure in-memory, **never runs SQL migrations**.
- **sqlite** + **postgres** = `store.SQLStore` (`SQLiteStore = SQLStore` alias, `internal/store/sqlite.go:20,28`) selected by the `dialect` seam (`internal/store/dialect.go`).

---

## 0. CANONICAL NAMES (produced by this area; keep consistent across tasks)

Go (defined in `internal/routing/store.go`):
```go
type EndpointMode string

const (
	EndpointModeDisabled    EndpointMode = "disabled"
	EndpointModeTranslate   EndpointMode = "translate"
	EndpointModePassthrough EndpointMode = "passthrough"
)
```
- `Application.ResponsesMode EndpointMode` / `Application.MessagesMode EndpointMode` (replace `NativeResponses`/`NativeMessages bool`).
- `RuntimeSpec.APIFlavors []string`, `RuntimeSpec.ResponsesMode EndpointMode`, `RuntimeSpec.MessagesMode EndpointMode` (new).
- Columns (all `text`): `applications.responses_mode`, `applications.messages_mode`;
  `agent_runtime_specs.api_flavors`, `agent_runtime_specs.responses_mode`, `agent_runtime_specs.messages_mode`.
- **Next migration number = 72** (`migration72Up`, name e.g. `application_endpoint_modes`). Current highest is **71** (`internal/store/migrate.go:104`, `migration71Up`).
- Existing flavor constants reused: `routing.APIFlavorOpenAI = "openai"`, `routing.APIFlavorAnthropic = "anthropic"` (`internal/routing/store.go:46-47`).
- Reused JSON codecs: `store.encodeAPIFlavors([]string) (string,error)` (`internal/store/sqlite_routes.go:757`), `store.decodeAPIFlavors(string) ([]string,error)` (`internal/store/sqlite_applications.go:731`).

Consumed from other areas: the DTO/portal area (`internal/portal/service_applications.go`, `service_runtime.go`) maps DTO enum strings ↔ these struct fields; the routing-enforcement area consumes `Application.ResponsesMode/MessagesMode` + `RuntimeSpec.*` on the `Target` (`internal/routing/resolver.go` `targetFrom`, line 890). This area only produces the struct fields + persisted columns + migration; it does **not** own `Target`, `native_passthrough.go`, or the DTOs.

---

## 1. CURRENT STATE (exact excerpts + file:line)

### 1.1 Routing model — `internal/routing/store.go`

`Application` struct (`:496`), native fields to REPLACE (`:519-524`):
```go
	// NativeResponses / NativeMessages enable per-application native passthrough:
	// when set, a client request to /v1/responses (Codex) resp. /v1/messages
	// (Claude Code / Anthropic) is proxied raw to the upstream's same native path
	// instead of being translated through the internal inference representation.
	NativeResponses bool
	NativeMessages  bool
```

`RuntimeSpec` struct (`:1242`) — currently has **no** flavor/mode fields; `SetVisibleDevices bool` (`:1277`) then `CreatedAt`/`UpdatedAt` (`:1278-1279`). The doc comments at the memory store (below) explicitly assert "RuntimeSpec has no slice/pointer fields" — adding `APIFlavors []string` **breaks that assumption**.

Flavor constant block (`:46-47`); `applicationHasAPIFlavor(app, flavor)` (`:1454`).

### 1.2 Baseline create-table (FROZEN v60 — DO NOT EDIT) — `internal/store/migrate.go`

`applications` baseline (`:517-543`) keeps the inert bools (`:528-529`):
```go
			native_responses integer not null default 0,
			native_messages integer not null default 0,
```
Per spec §7 these columns **stay** (inert). `baselineCreateStatements` is frozen as of v60 (`:266-279` doc comment) — the new migration must **not** add anything here.

### 1.3 Migration list + runner — `internal/store/migrate.go`

- Ordered `migrations` slice ends at `{version: 71, name: "model_mapping_benchmarks_vram_json", up: migration71Up}` (`:104`). Append `{version: 72, ...}` at `:105`.
- Runner `Migrate` (`:109-167`) wraps each `up` in its own tx and records `schema_migrations` (`:156-161`). Forward-only: "NEVER edit or reorder an already-shipped migration" (`:31-32`).
- `addColumnIfMissing(ctx, tx, dl, table, colDef)` (`:197-209`) — cross-driver ADD COLUMN: postgres rewrites first `add column ` → `add column if not exists `; sqlite runs as-is and swallows `duplicate column name`. This **confirms ADD COLUMN is supported on sqlite+postgres** and is the intended helper for new migrations (`:193-196`).
- `execTx(ctx, tx, dl, q)` (`:170-173`) rebinds placeholders and execs one statement.

**Data-backfill migrations are an existing pattern.** Two examples to cite:
- `migration20Up` (`:1173`+) — ADD COLUMN then a one-time backfill (`netbird_peer_managed` from `netbird_setup_key_id`); test `TestMigration20BackfillPeerManaged` (`internal/store/sqlite_migration_test.go:272`).
- `migration70Up` (`:3076-3083`) — ADD COLUMN then deterministic backfill UPDATE, **aborts the boot on failure** (its doc `:3066-3071` explains why abort, not skip). This is the model migration 72 should follow:
```go
func migration70Up(ctx context.Context, tx *sql.Tx, dl dialect) error {
	if err := addColumnIfMissing(ctx, tx, dl, "applications",
		"proxy_excluded integer not null default 0"); err != nil {
		return err
	}
	return execTx(ctx, tx, dl, `update applications set proxy_excluded = 1
		where scheme = 'https' and proxy_listen_port = 0`)
}
```

### 1.4 `agent_runtime_specs` schema — `internal/store/migrate.go`

Created in `migration65Up` (`:2856-2899`), table body (`:2859-2877`) — **no** `api_flavors`/`responses_mode`/`messages_mode` today. Later spec column added append-only in `migration69Up` (`:3036-3039`) via `addColumnIfMissing` — this is the pattern to follow (append, never fold into migration65Up; doc `:3025-3031`). The `agent_runtime_specs.mapping_id` FK → `model_mappings(id)` (`:2861`); `model_mappings.application_id` FK → `applications(id)` (baseline `:546`). This is the join path for the snapshot backfill.

### 1.5 Application CRUD — `internal/store/sqlite_applications.go`

Every spot referencing the native bools (must switch to mode columns):
- **INSERT** column list `:22-31` includes `native_responses, native_messages` (`:25`); binds `app.NativeResponses, app.NativeMessages` (`:48-49`). 32 columns / 32 `?`.
- **UPDATE** set-list `native_responses = ?, native_messages = ?` (`:89`); binds (`:113-114`).
- **ApplicationByID** SELECT `:144-151` incl. `native_responses, native_messages` (`:146`).
- **ApplicationsByServer** SELECT `:159-166` incl. `native_responses, native_messages` (`:161`).
- **ActiveMappingsForModel** SELECT `:472-494` incl. `a.native_responses, a.native_messages` (`:479`).
- **scanApplication** (`:598-657`): locals `var nativeResponses, nativeMessages int64` (`:602`), scanned (`:622-623`), assigned `app.NativeResponses = nativeResponses != 0` / `app.NativeMessages = nativeMessages != 0` (`:646-647`).
- **scanMappingCandidate** (`:516-579`): locals `nativeResponses int64` / `nativeMessages int64` (`:527-528`), scanned `&nativeResponses, &nativeMessages` (`:539`), assigned (`:557-558`).
- `decodeAPIFlavors` (`:731-737`) reused for the mode-agnostic flavors.

### 1.6 Runtime-spec CRUD — `internal/store/sqlite_runtime.go`

- **UpsertRuntimeSpec** INSERT column list (`:22-27`), `on conflict(mapping_id) do update set` (`:28-39`), binds (`:40-44`) — no flavor/mode.
- `runtimeSpecCols` (`:60-63`) and `runtimeSpecColsPrefixed` (`:69-72`) — the two shared SELECT lists (used by `RuntimeSpecByMapping :75`, `RuntimeSpecByID :90`, `RuntimeSpecsByApplication :103`).
- **scanRuntimeSpec** (`:329-346`): scans 19 columns into `spec` + 4 int64 bool locals (`:331`).

### 1.7 Memory store — `internal/routing/memory_store.go`

- `CreateApplication` (`:786`) / `UpdateApplication` (`:805`) store `copyApplication(app)`.
- `copyApplication` (`:2133-2136`) clones only `APIFlavors`; modes are plain strings so **no change needed for the app modes**, but see RuntimeSpec below.
- `UpsertRuntimeSpec` (`:2244-2263`) stores `spec` by value; `RuntimeSpecByMapping` (`:2267`), `RuntimeSpecByID` (`:2281`), `RuntimeSpecsByApplication` (`:2290`) return the stored value directly. Doc comments `:2265-2266` and `:2278-2280` assert **"RuntimeSpec has no slice/pointer fields, so the map value is already a safe copy."** Adding `APIFlavors []string` makes this false → aliasing bug unless a `copyRuntimeSpec` helper is introduced.

### 1.8 Parity test — `internal/store/application_column_parity_test.go`

- `applicationParityRows = 3` (`:22`), derived from `2^r >= 6` (six bool columns) (`:20-21`).
- `applicationParityBools [6][3]bool` (`:39-46`) rows are, in SELECT order, `always_reachable, native_responses, native_messages, benchmark_schedule_enabled, opportunistic_metrics_enabled, proxy_excluded`.
- `want` fixture sets `NativeResponses: applicationParityBools[1][i]` (`:124`) and `NativeMessages: applicationParityBools[2][i]` (`:125`).
- `TestApplicationParityFixtureDistinguishesEverySameTypedPair` names array (`:218-221`) lists the same six.
- Comparison via `normalizeApplicationForCompare` + `reflect.DeepEqual` (`:194,202,260`).

Once the bools are replaced by `EndpointMode` (strings), only **4** bool columns remain, so `applicationParityBools` shrinks `[6]→[4]` and the two `names` lists drop `native_responses`/`native_messages`. `ResponsesMode`/`MessagesMode` become varied **string** fields in `want` (checked by the DeepEqual, like the other text columns).

### 1.9 Runtime-spec conformance — `internal/store/routing_store_conformance_test.go`

`TestRoutingStoreRuntimeSpecs` (`:483`) runs on **all three drivers** via `forEachRoutingStore` (`:32-34` → `forEachRoutingStoreSeeded :41-59`: memory + sqlite, and postgres when the SQL leg is exercised). Its `spec` fixture (`:529-537`) and round-trip assertion (`:545-549`) are where the new fields get exercised for parity.

### 1.10 Dialect seam + postgres harness

- `dialect` interface — `internal/store/dialect.go:16-24` (`name()`, `rebind()`, `timestampType()`, `isUniqueViolation`, `isForeignKeyViolation`). sqlite `name()="sqlite"` (`:28`), postgres `name()="postgres"` (`:47`).
- `forEachDialect(t, run func(t, s *SQLStore))` — `internal/store/conformance_test.go:27-59`: sqlite always (temp file), postgres only when `OP_AI_GATEWAY_TEST_POSTGRES_DSN` is set (`:40-42`), schema-dropped + freshly migrated each subtest.
- sqlite-only migration tests use `openMigratedTestSQLite(t)` (`internal/store/sqlite_migration_test.go:338`).

---

## 2. PROPOSED TDD TASKS (ordered, bite-sized)

Run everything from `gateway/backend`. Store package tests:
```
go test ./internal/store/... ./internal/routing/...
```
Postgres leg (dialect-specific backfill SQL) additionally requires:
```
OP_AI_GATEWAY_TEST_POSTGRES_DSN='postgres://user:pass@localhost:5432/gwtest?sslmode=disable' \
  go test ./internal/store/...
```

### Task 1 — `EndpointMode` type + struct fields (compile scaffolding)

Add to `internal/routing/store.go` (near the flavor consts `:46-47`):
```go
// EndpointMode is the per-endpoint serving decision for a coding-agent API
// (Codex /v1/responses, Claude Code /v1/messages): disabled (not served),
// translate (compat path to /v1/chat/completions), or passthrough (raw proxy
// to the upstream's native path). Stored as lowercase text; replaces the old
// native_responses / native_messages booleans.
type EndpointMode string

const (
	EndpointModeDisabled    EndpointMode = "disabled"
	EndpointModeTranslate   EndpointMode = "translate"
	EndpointModePassthrough EndpointMode = "passthrough"
)
```
Replace `Application` `:519-524`:
```go
	// ResponsesMode / MessagesMode are the per-endpoint EndpointMode for the
	// Codex /v1/responses resp. Claude Code /v1/messages endpoint (was the
	// native_responses / native_messages booleans). passthrough == the old
	// native_*=true (raw proxy); translate == native_*=false (compat path);
	// disabled == the endpoint is not served by this application.
	ResponsesMode EndpointMode
	MessagesMode  EndpointMode
```
Add to `RuntimeSpec` (before `CreatedAt` `:1278`):
```go
	// APIFlavors / ResponsesMode / MessagesMode are the per-spec snapshot of the
	// same controls the parent application carries — the resolved spec is the
	// sole authority for a server_agent model's flavors + endpoint modes. Never
	// null after backfill/create. Gateway-side only; NOT on the agent wire type.
	APIFlavors    []string
	ResponsesMode EndpointMode
	MessagesMode  EndpointMode
```
This task is compile-only scaffolding folded into Task 2 (nothing reads the fields yet). No standalone test.

### Task 2 — Application CRUD reads/writes mode columns (behavioural, needs Task 4's migration for the column, but the sqlite/postgres column comes from the migration; write the migration test first — see Task 4. To keep TDD honest, do Task 4 before Task 2's round-trip assertion, or land migration + CRUD together and let the conformance suite be the failing test.)

Failing test — add to `internal/store/routing_store_conformance_test.go` (extend the existing `TestRoutingStoreRuntimeSpecs` sibling, or the application round-trip; simplest is a new focused conformance test that runs all drivers):
```go
// TestRoutingStoreApplicationEndpointModes proves all three drivers round-trip
// ResponsesMode / MessagesMode on an application.
func TestRoutingStoreApplicationEndpointModes(t *testing.T) {
	forEachRoutingStore(t, func(t *testing.T, s routing.Store) {
		ctx := context.Background()
		now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
		if err := s.CreateAIServer(ctx, routing.AIServer{
			ID: "srv_em", Name: "EM", Domain: "em.local", Provider: routing.ProviderVLLM,
			Endpoint: "http://em:8000", Status: routing.ServerStatusActive,
			HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("create server: %v", err)
		}
		if err := s.CreateApplication(ctx, routing.Application{
			ID: "app_em", ServerID: "srv_em", Type: routing.ProviderVLLM, Port: 8100, Scheme: "http",
			APIFlavors:    []string{routing.APIFlavorOpenAI, routing.APIFlavorAnthropic},
			ResponsesMode: routing.EndpointModeDisabled, MessagesMode: routing.EndpointModePassthrough,
			Status: routing.ServerStatusActive, HealthCheckMode: routing.HealthCheckModeAlwaysReachable,
			CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("create application: %v", err)
		}
		got, err := s.ApplicationByID(ctx, "app_em")
		if err != nil {
			t.Fatalf("by id: %v", err)
		}
		if got.ResponsesMode != routing.EndpointModeDisabled || got.MessagesMode != routing.EndpointModePassthrough {
			t.Fatalf("modes: got responses=%q messages=%q, want disabled/passthrough", got.ResponsesMode, got.MessagesMode)
		}
	})
}
```
Run (fails to compile until struct fields exist, then fails on `''` round-trip until CRUD updated):
```
go test ./internal/store/ -run TestRoutingStoreApplicationEndpointModes -v
```
Expected: FAIL (compile, then value mismatch) → PASS after implementation.

Minimal implementation — `internal/store/sqlite_applications.go`, mechanical column swaps (`native_responses`→`responses_mode`, `native_messages`→`messages_mode`) in all five SQL lists (`:25, :89, :146, :161, :479`) and the binds. Modes are `text`, scanned directly into a Go string via `EndpointMode` — **drop the int64 locals**:

INSERT binds (`:48-49`) become:
```go
		app.ResponsesMode,
		app.MessagesMode,
```
UPDATE set-list (`:89`) → `responses_mode = ?, messages_mode = ?,`; binds (`:113-114`) → `app.ResponsesMode, app.MessagesMode,`.

`scanApplication` — delete `var nativeResponses, nativeMessages int64` (`:602`), scan straight into the struct fields (`:622-623` → `&app.ResponsesMode, &app.MessagesMode,`), delete the `!= 0` assignments (`:646-647`). Note `row.Scan` accepts `*EndpointMode` because its underlying type is `string`; `database/sql` scans a `text` column into any `~string` dest.

`scanMappingCandidate` — delete the two int64 locals (`:527-528`), scan `&c.Application.ResponsesMode, &c.Application.MessagesMode` (`:539`), delete assignments (`:557-558`).

### Task 3 — Runtime-spec CRUD reads/writes api_flavors + modes (behavioural)

Failing test — extend `TestRoutingStoreRuntimeSpecs` (`internal/store/routing_store_conformance_test.go:529`) `spec` fixture and assertion:
```go
		spec := routing.RuntimeSpec{
			ID: "rspec_rt2", MappingID: "map_rt2", Enabled: true,
			Binary: "/usr/bin/llama-server", Args: `["--port","${PORT}"]`,
			Env: `{"HF_TOKEN":"${AGENT_ENV:HF_TOKEN}"}`, WorkDir: "/srv/models",
			HealthPath: "/health", HealthTimeoutSeconds: 5, StartupTimeoutSeconds: 180,
			IdleTimeoutSeconds: 900, AdmissionWaitTimeoutSeconds: 30, Pinned: true,
			AdminState: "force_running", VRAMLocked: true, SetVisibleDevices: true,
			APIFlavors:    []string{routing.APIFlavorOpenAI, routing.APIFlavorAnthropic},
			ResponsesMode: routing.EndpointModeTranslate, MessagesMode: routing.EndpointModeDisabled,
			CreatedAt: now, UpdatedAt: now,
		}
```
Extend the round-trip assertion (`:545-549`) with:
```go
		if got.ResponsesMode != routing.EndpointModeTranslate || got.MessagesMode != routing.EndpointModeDisabled ||
			!reflect.DeepEqual(got.APIFlavors, []string{routing.APIFlavorOpenAI, routing.APIFlavorAnthropic}) {
			t.Fatalf("flavor/mode round-trip mismatch: %+v", got)
		}
```
Run:
```
go test ./internal/store/ -run TestRoutingStoreRuntimeSpecs -v
```
Expected: FAIL (memory sub-test aliases / SQL sub-test returns empty) → PASS after implementation.

Minimal implementation:

`internal/store/sqlite_runtime.go`:
- `UpsertRuntimeSpec` INSERT column list (`:22-27`) add `api_flavors, responses_mode, messages_mode` (before `created_at`); add 3 placeholders; `on conflict` set-clause (`:28-39`) add `api_flavors = excluded.api_flavors, responses_mode = excluded.responses_mode, messages_mode = excluded.messages_mode,`; binds (`:40-44`) add `apiFlavors, string(spec.ResponsesMode), string(spec.MessagesMode)` where `apiFlavors, err := encodeAPIFlavors(spec.APIFlavors)` at the top of the method (mirror `CreateApplication :17`).
- `runtimeSpecCols` (`:60-63`) and `runtimeSpecColsPrefixed` (`:69-72`) append `, api_flavors, responses_mode, messages_mode` (prefixed with `s.` in the second).
- `scanRuntimeSpec` (`:329-346`): add `var apiFlavors string`; scan `&apiFlavors, &spec.ResponsesMode, &spec.MessagesMode` at the end (before/after `CreatedAt`/`UpdatedAt` — keep column order matching the SELECT list, i.e. after `set_visible_devices` and before `created_at`, so insert them in that position in BOTH the cols consts and the scan). Then `spec.APIFlavors, err = decodeAPIFlavors(apiFlavors)` with an error return.

  Column order note: put the three new columns immediately after `set_visible_devices` and before `created_at, updated_at` in the INSERT list, the two `runtimeSpecCols` consts, and the scan — one consistent position everywhere.

`internal/routing/memory_store.go` — add a deep-copy helper and use it (RuntimeSpec now has a slice):
```go
// copyRuntimeSpec deep-copies spec.APIFlavors so a stored spec cannot be
// aliased by a caller (RuntimeSpec gained a slice field with the endpoint-mode
// work; the "no slice/pointer fields" shortcut no longer holds).
func copyRuntimeSpec(spec RuntimeSpec) RuntimeSpec {
	spec.APIFlavors = append([]string(nil), spec.APIFlavors...)
	return spec
}
```
- `UpsertRuntimeSpec` (`:2244`): store `copyRuntimeSpec(spec)` in all three write positions (`:2254, :2261`), and `spec = copyRuntimeSpec(spec)` once at entry is simplest.
- `RuntimeSpecByMapping` (`:2267`), `RuntimeSpecByID` (`:2281`), `RuntimeSpecsByApplication` (`:2290`): return `copyRuntimeSpec(...)` and update the two "no slice/pointer fields" doc comments (`:2265-2266, :2278-2280`) to note the deep copy.

### Task 4 — Migration 72 (add columns + backfill), the core behavioural task

Failing tests first — new file `internal/store/migration72_endpoint_modes_test.go`. Use `forEachDialect` so the **postgres** `UPDATE ... FROM` path is exercised under DSN (the sqlite path uses a correlated subquery — they are different SQL and both must be covered):

```go
// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package store

import (
	"context"
	"op-ai-gateway/internal/routing"
	"reflect"
	"testing"
	"time"
)

// reinvokeMigration72 re-runs migration72Up in its own tx (idempotent: the
// ADD COLUMNs are duplicate-tolerant and the backfill UPDATEs are guarded on
// '' so already-set rows are untouched) after seeding legacy rows, mirroring
// TestMigration20BackfillPeerManaged.
func reinvokeMigration72(ctx context.Context, t *testing.T, s *SQLStore) {
	t.Helper()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	if err := migration72Up(ctx, tx, s.dl); err != nil {
		_ = tx.Rollback()
		t.Fatalf("migration72Up: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

// TestMigration72BackfillApplicationModes proves the bool→mode backfill:
// native_responses=1 -> passthrough, 0 -> translate (same for messages).
func TestMigration72BackfillApplicationModes(t *testing.T) {
	forEachDialect(t, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		now := time.Now().UTC().Truncate(time.Second)
		if err := s.CreateAIServer(ctx, routing.AIServer{
			ID: "srv_m72", Name: "M72", Provider: routing.ProviderVLLM, Endpoint: "http://m:8000",
			Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("create server: %v", err)
		}
		for _, id := range []string{"app_pt", "app_tp"} {
			if err := s.CreateApplication(ctx, routing.Application{
				ID: id, ServerID: "srv_m72", Type: routing.ProviderVLLM,
				Port: map[string]int{"app_pt": 9001, "app_tp": 9002}[id], Scheme: "http",
				APIFlavors: []string{routing.APIFlavorOpenAI, routing.APIFlavorAnthropic},
				Status: routing.ServerStatusActive, HealthCheckMode: routing.HealthCheckModeAlwaysReachable,
				CreatedAt: now, UpdatedAt: now,
			}); err != nil {
				t.Fatalf("create app %s: %v", id, err)
			}
		}
		// Force the pre-72 legacy shape: set the inert bools, blank the modes.
		mustExec(ctx, t, s, `update applications set native_responses = 1, native_messages = 0,
			responses_mode = '', messages_mode = '' where id = ?`, "app_pt")
		mustExec(ctx, t, s, `update applications set native_responses = 0, native_messages = 1,
			responses_mode = '', messages_mode = '' where id = ?`, "app_tp")

		reinvokeMigration72(ctx, t, s)

		pt, _ := s.ApplicationByID(ctx, "app_pt")
		if pt.ResponsesMode != routing.EndpointModePassthrough || pt.MessagesMode != routing.EndpointModeTranslate {
			t.Fatalf("app_pt: got %q/%q want passthrough/translate", pt.ResponsesMode, pt.MessagesMode)
		}
		tp, _ := s.ApplicationByID(ctx, "app_tp")
		if tp.ResponsesMode != routing.EndpointModeTranslate || tp.MessagesMode != routing.EndpointModePassthrough {
			t.Fatalf("app_tp: got %q/%q want translate/passthrough", tp.ResponsesMode, tp.MessagesMode)
		}
	})
}

// TestMigration72SnapshotRuntimeSpec proves each spec is snapshotted from its
// parent app via spec.mapping_id -> model_mappings -> applications.
func TestMigration72SnapshotRuntimeSpec(t *testing.T) {
	forEachDialect(t, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		now := time.Now().UTC().Truncate(time.Second)
		if err := s.CreateAIServer(ctx, routing.AIServer{
			ID: "srv_sp", Name: "SP", Provider: routing.ProviderMock, Endpoint: "mock://sp",
			Status: routing.ServerStatusActive, HealthStatus: routing.HealthUnknown, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("create server: %v", err)
		}
		if err := s.CreateApplication(ctx, routing.Application{
			ID: "app_sp", ServerID: "srv_sp", Type: routing.ProviderServerAgent, Port: 8090, Scheme: "http",
			APIFlavors:    []string{routing.APIFlavorOpenAI, routing.APIFlavorAnthropic},
			ResponsesMode: routing.EndpointModePassthrough, MessagesMode: routing.EndpointModeTranslate,
			Status: routing.ServerStatusActive, HealthCheckMode: routing.HealthCheckModeAlwaysReachable,
			CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("create app: %v", err)
		}
		if err := s.CreateMapping(ctx, routing.ModelMapping{
			ID: "map_sp", ApplicationID: "app_sp", GatewayModelName: "sp-model", AppModelName: "up-sp",
			Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("create mapping: %v", err)
		}
		if err := s.UpsertRuntimeSpec(ctx, routing.RuntimeSpec{
			ID: "spec_sp", MappingID: "map_sp", Enabled: true, Binary: "/usr/bin/llama-server",
			Args: "[]", Env: "{}", CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("upsert spec: %v", err)
		}
		// Force the pre-72 legacy shape on the spec.
		mustExec(ctx, t, s, `update agent_runtime_specs set api_flavors = '[]',
			responses_mode = '', messages_mode = '' where id = ?`, "spec_sp")

		reinvokeMigration72(ctx, t, s)

		got, ok, err := s.RuntimeSpecByMapping(ctx, "map_sp")
		if err != nil || !ok {
			t.Fatalf("read back spec: ok=%v err=%v", ok, err)
		}
		if got.ResponsesMode != routing.EndpointModePassthrough || got.MessagesMode != routing.EndpointModeTranslate {
			t.Fatalf("spec modes: got %q/%q want passthrough/translate", got.ResponsesMode, got.MessagesMode)
		}
		if !reflect.DeepEqual(got.APIFlavors, []string{routing.APIFlavorOpenAI, routing.APIFlavorAnthropic}) {
			t.Fatalf("spec flavors: got %#v want [openai anthropic]", got.APIFlavors)
		}
	})
}
```
Add a tiny raw-exec helper (or inline `s.db.ExecContext(ctx, s.dl.rebind(q), args...)`):
```go
func mustExec(ctx context.Context, t *testing.T, s *SQLStore, q string, args ...any) {
	t.Helper()
	if _, err := s.db.ExecContext(ctx, s.dl.rebind(q), args...); err != nil {
		t.Fatalf("exec %q: %v", q, err)
	}
}
```
Run:
```
go test ./internal/store/ -run 'TestMigration72' -v
OP_AI_GATEWAY_TEST_POSTGRES_DSN=... go test ./internal/store/ -run 'TestMigration72' -v
```
Expected: FAIL (`migration72Up` undefined, then no backfill) → PASS after implementation.

Minimal implementation — `internal/store/migrate.go`:

Register at `:105` (after the v71 entry):
```go
	{version: 72, name: "application_endpoint_modes", up: migration72Up},
```
Add the function (place near migration70/71):
```go
// migration72Up replaces the two native-passthrough booleans with per-endpoint
// EndpointMode columns and snapshots them onto every existing runtime spec.
//
// applications: add responses_mode / messages_mode (text, not null default ''
// so an upgrade's existing rows are non-NULL immediately, matching every other
// text column here) and backfill from the inert native_responses /
// native_messages booleans -- 1 -> 'passthrough' (raw proxy, as native_*=true
// today), 0 -> 'translate' (compat path, as native_*=false today). No app
// becomes 'disabled' on upgrade, so behaviour is preserved. The old bool
// columns are left in place (append-only discipline, three drivers) and are no
// longer read or written by any code.
//
// agent_runtime_specs: add api_flavors / responses_mode / messages_mode and
// backfill each spec from its parent application (spec.mapping_id ->
// model_mappings.application_id -> applications), copying the app's api_flavors
// and its just-backfilled modes -- the "snapshot from app" decision. Existing
// specs become explicit and independent, and the upgrade changes no behaviour.
// Applications are backfilled FIRST so the spec join reads their set modes.
//
// It ABORTS the boot on failure (deterministic UPDATEs, no possibly-dirty
// pre-check), following migration70Up rather than migration68Up's skip policy.
// It does NOT touch baselineCreateStatements (frozen v60) or migration65Up's
// create-table (append-only): a fresh install gets the columns by replaying
// this migration, and every backfill is a no-op there because the tables are
// empty. All backfills are guarded on '' so a re-run is idempotent.
func migration72Up(ctx context.Context, tx *sql.Tx, dl dialect) error {
	// applications: two mode columns.
	for _, col := range []string{
		"responses_mode text not null default ''",
		"messages_mode text not null default ''",
	} {
		if err := addColumnIfMissing(ctx, tx, dl, "applications", col); err != nil {
			return err
		}
	}
	if err := execTx(ctx, tx, dl, `update applications
		set responses_mode = case when native_responses <> 0 then 'passthrough' else 'translate' end
		where responses_mode = ''`); err != nil {
		return err
	}
	if err := execTx(ctx, tx, dl, `update applications
		set messages_mode = case when native_messages <> 0 then 'passthrough' else 'translate' end
		where messages_mode = ''`); err != nil {
		return err
	}
	// agent_runtime_specs: the trio. api_flavors defaults to a valid empty JSON
	// array so a transient pre-backfill read still decodes.
	for _, col := range []string{
		"api_flavors text not null default '[]'",
		"responses_mode text not null default ''",
		"messages_mode text not null default ''",
	} {
		if err := addColumnIfMissing(ctx, tx, dl, "agent_runtime_specs", col); err != nil {
			return err
		}
	}
	// Snapshot each spec from its parent application. Cross-driver: postgres
	// UPDATE ... FROM a multi-table join; sqlite correlated subqueries (modernc
	// sqlite supports UPDATE ... FROM only since 3.33 and not across this join
	// shape reliably -- correlated subqueries are universally supported and
	// identical semantics). Guarded on '' for idempotency.
	if dl.name() == "postgres" {
		return execTx(ctx, tx, dl, `update agent_runtime_specs s
			set api_flavors = a.api_flavors,
			    responses_mode = a.responses_mode,
			    messages_mode = a.messages_mode
			from model_mappings m
			join applications a on a.id = m.application_id
			where m.id = s.mapping_id
			  and (s.responses_mode = '' or s.messages_mode = '')`)
	}
	return execTx(ctx, tx, dl, `update agent_runtime_specs
		set api_flavors = coalesce((select a.api_flavors from model_mappings m
			join applications a on a.id = m.application_id
			where m.id = agent_runtime_specs.mapping_id), api_flavors),
		    responses_mode = coalesce((select a.responses_mode from model_mappings m
			join applications a on a.id = m.application_id
			where m.id = agent_runtime_specs.mapping_id), responses_mode),
		    messages_mode = coalesce((select a.messages_mode from model_mappings m
			join applications a on a.id = m.application_id
			where m.id = agent_runtime_specs.mapping_id), messages_mode)
		where responses_mode = '' or messages_mode = ''`)
}
```
Notes on the SQL:
- `native_responses <> 0` works on both dialects — the column is `integer` on postgres too (migration5Up added it as integer, `:790-807`), so no boolean cast is needed.
- The `where ... = ''` guards make a re-invoke (the test path) and any re-run a no-op on already-set rows; on a fresh/upgraded DB the first pass matches every row.
- Every spec's `mapping_id` FK → a live mapping, whose `application_id` FK → a live app, so the join yields exactly one row (no orphans); `coalesce(...)` is belt-and-braces against a NULL that the FKs already forbid.

### Task 5 — Extend the application column-parity fixture

`internal/store/application_column_parity_test.go`:
- Shrink `applicationParityBools` from `[6][3]bool` to `[4][3]bool` (drop the `native_responses` / `native_messages` rows), keeping distinct patterns for the four survivors:
```go
// order: always_reachable, benchmark_schedule_enabled, opportunistic_metrics_enabled, proxy_excluded
var applicationParityBools = [4][applicationParityRows]bool{
	{true, true, false},
	{false, true, true},
	{false, true, false},
	{false, false, true},
}
```
  `applicationParityRows = 3` stays valid (`2^3 = 8 >= 4`; update the `:20-21` comment: four bool columns → `2^r >= 4`).
- Update the `want` fixture: replace `NativeResponses`/`NativeMessages` (`:124-125`) with mode strings that vary per row and differ from each other (so a swapped select-list pair is observable via the DeepEqual):
```go
	responsesModes := [applicationParityRows]routing.EndpointMode{
		routing.EndpointModeDisabled, routing.EndpointModeTranslate, routing.EndpointModePassthrough,
	}
	messagesModes := [applicationParityRows]routing.EndpointMode{
		routing.EndpointModeTranslate, routing.EndpointModePassthrough, routing.EndpointModeDisabled,
	}
	// ... in the loop:
	AlwaysReachable:             applicationParityBools[0][i],
	// (native lines removed)
	ResponsesMode:               responsesModes[i],
	MessagesMode:                messagesModes[i],
	BenchmarkScheduleEnabled:    applicationParityBools[1][i],
	OpportunisticMetricsEnabled: applicationParityBools[2][i],
	ProxyExcluded:               applicationParityBools[3][i],
```
- Update both `names` lists (`:27` comment block and `:218-221`) to drop `native_responses` / `native_messages` (four names) and the doc-comment count references (`:20-27`).

Run:
```
go test ./internal/store/ -run 'TestConformanceApplicationReadersAgreeOnEveryColumn|TestApplicationParityFixtureDistinguishesEverySameTypedPair' -v
```
Expected: PASS (this test guards that all three application readers agree — after Task 2 the modes flow through all three lists).

### Task 6 — Full store + routing suite green (verification)

```
go test ./internal/store/... ./internal/routing/...
OP_AI_GATEWAY_TEST_POSTGRES_DSN=... go test ./internal/store/...
```
Expected: PASS on all drivers. Watch for other tests that constructed `routing.Application{NativeResponses:...}` or read `.NativeResponses` — those are compile errors that must be swept to the new field names (this area owns the store/routing tests; the portal/gateway tests are their areas).

---

## 3. INTERFACES

**PRODUCES** (other areas consume):
- `routing.EndpointMode` + `routing.EndpointModeDisabled/Translate/Passthrough` (canonical, matches spec).
- `routing.Application.ResponsesMode` / `.MessagesMode EndpointMode`.
- `routing.RuntimeSpec.APIFlavors []string` / `.ResponsesMode` / `.MessagesMode EndpointMode`.
- Persisted columns: `applications.responses_mode`, `applications.messages_mode`; `agent_runtime_specs.api_flavors`, `agent_runtime_specs.responses_mode`, `agent_runtime_specs.messages_mode` (all `text`).
- Migration `72` (`application_endpoint_modes`) with the documented bool→mode + snapshot backfill.

**CONSUMES** (from other areas; not built here):
- Portal DTO area: JSON `responses_mode` / `messages_mode` (applications) and `api_flavors` / `responses_mode` / `messages_mode` (runtime specs), validated to the three enum strings; maps DTO ↔ the struct fields above. This area only requires that the fields hold one of the three values (or `""` transiently pre-migration) — validation/defaulting lives in the portal (`internal/portal/service_applications.go:128-129,195-196,234-235`, `service_runtime.go`).
- Routing-enforcement area: `targetFrom` (`internal/routing/resolver.go:890`) and `Target.NativeResponses/NativeMessages` (`:64-69`) — that area replaces those with the effective mode; **this area does not touch resolver.go / native_passthrough.go**.
- New stable error codes `responses.endpoint_disabled` / `messages.endpoint_disabled` — owned by the gateway/enforcement area, not persistence.

Deviations from the canonical names: **none.** All canonical identifiers map cleanly (`EndpointMode`, the three consts, `ResponsesMode`/`MessagesMode`, `APIFlavors`, the four columns). `EndpointMode`'s underlying type is `string`, so `database/sql` scans a `text` column straight into `*EndpointMode` and binds it via `string(mode)` — no custom `Scanner`/`Valuer` needed.

---

## 4. GOTCHAS

1. **Confirmed — old columns kept inert (spec §7).** `applications.native_responses` / `native_messages` remain in the baseline create-table (`migrate.go:528-529`, frozen v60) and are never removed. Migration 72 does **not** DROP them and does **not** touch `baselineCreateStatements`. After Task 2, no Go code reads or writes them; the migration reads them once (the backfill) and then they are dead weight by design (physical removal is spec §12 out-of-scope).

2. **Confirmed — ADD COLUMN + data-backfill is an established, cross-driver pattern.** `addColumnIfMissing` (`migrate.go:197-209`) does ADD COLUMN on both sqlite (swallow `duplicate column name`) and postgres (`add column if not exists`). Backfill precedent: **`migration20Up`** (`:1173`+, backfills `netbird_peer_managed`) and **`migration70Up`** (`:3076-3083`, backfills `proxy_excluded`, aborts on failure). Migration 72 follows migration70Up's abort-not-skip stance.

3. **Cross-driver backfill SQL differs and BOTH legs must be tested.** Postgres uses `UPDATE agent_runtime_specs s SET ... FROM model_mappings m JOIN applications a ...`; sqlite uses correlated subqueries (modernc sqlite's `UPDATE ... FROM` support is version-gated and awkward across this join — correlated subqueries are universally safe). The migration branches on `dl.name() == "postgres"`. Because the two paths are literally different SQL, the migration tests **must** run under `forEachDialect` and the postgres leg **must** be exercised with `OP_AI_GATEWAY_TEST_POSTGRES_DSN` set — a sqlite-only test would never touch the `UPDATE ... FROM`.

4. **`text not null default ''` (not nullable).** Adding the columns as nullable would leave pre-existing rows NULL until the same-migration backfill, and a NULL `text` scanned into a Go `string`/`EndpointMode` **errors** (`converting NULL to string is unsupported`). Every text column in this schema uses `not null default ''`; follow that. `api_flavors` defaults to `'[]'` (valid JSON) so a transient read before backfill still `decodeAPIFlavors`-es.

5. **Memory store has NO migration — confirmed, and there is nothing to backfill there.** `routing.MemoryStore` never calls `Migrate` (only `SQLStore.Migrate` exists, `migrate.go:109`); it is constructed empty (`routing.NewMemoryStore()`) every time. So the "memory-store backfill" is a non-task: the memory driver simply round-trips the new fields through `CreateApplication`/`UpsertRuntimeSpec`. The **only** memory-store code change is deep-copying `RuntimeSpec.APIFlavors` (Task 3's `copyRuntimeSpec`) because the "RuntimeSpec has no slice/pointer fields" shortcut (`memory_store.go:2265-2266,2278-2280`) is now false. `copyApplication` already deep-copies `APIFlavors` and needs no change for the string modes.

6. **RuntimeSpec aliasing bug is easy to miss.** Without `copyRuntimeSpec`, a caller mutating a returned spec's `APIFlavors` slice would corrupt the map-stored value. The `TestRoutingStoreRuntimeSpecs` round-trip won't catch aliasing on its own — but it's the same class of bug the existing "always non-nil" comments guard against; add the deep copy and update those comments.

7. **Column ORDER must stay identical across every reader.** `TestConformanceApplicationReadersAgreeOnEveryColumn` exists precisely because the applications SELECT list is hand-maintained in three queries feeding two scan funcs (`application_column_parity_test.go:48-79`). When swapping `native_*` → `*_mode`, change all five SQL lists AND both scan funcs to the same position; the two new mode columns are same-typed (`text`) with the other text columns, so a mis-ordered pair is silent except through this test. Keep the runtime-spec trio in one fixed position (after `set_visible_devices`, before `created_at`) across the INSERT list, both `runtimeSpecCols*` consts, and `scanRuntimeSpec`.

8. **Test commands.** From `gateway/backend`: `go test ./internal/store/... ./internal/routing/...`; postgres leg `OP_AI_GATEWAY_TEST_POSTGRES_DSN='postgres://...?sslmode=disable' go test ./internal/store/...`. Repo-wide gate: `make test-go` (`Makefile:48`, runs `cd gateway/backend && go test ./...`). ADR-005 wide-type rule (`text` is unbounded on postgres) is satisfied — all five new columns are `text`.

9. **Sweep for compile breaks.** Replacing the two struct fields breaks any `routing.Application{NativeResponses: ...}` / `.NativeMessages` reference. In non-test code these live in `internal/portal/service_applications.go`, `internal/routing/resolver.go`, `internal/gateway/native_passthrough.go` (other areas). Within this area, grep `internal/store` + `internal/routing` tests for `NativeResponses`/`NativeMessages` and migrate them (parity test `:124-125,220`, plus any routing memory-store tests).
