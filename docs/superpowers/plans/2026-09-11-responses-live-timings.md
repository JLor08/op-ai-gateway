# Responses Live Timings — Part 1 (the wiring) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Carry one new operator-owned boolean, `responses_live_timings_enabled`, from a new migration through every hand-maintained store path, the three routing structs and the portal's application + runtime-spec write paths, so that part 2 has a value on `routing.Target` to read — and nothing in part 1 reads it.

**Architecture:** The boolean is orthogonal to the three-state `responses_mode` enum, exactly as `proxy_excluded` is orthogonal to `scheme` (ADR-030): a new `integer not null default 0` column on **both** `applications` and `agent_runtime_specs`, because for a `server_agent` mapping the resolved runtime spec — not the parent application — is the authority for the endpoint behaviour this flag qualifies. It reaches request time on `routing.Target` through `targetFrom`, with the spec's value overriding the app's wherever a spec exists, mirroring `ResponsesMode`/`MessagesMode` line for line. The portal accepts it as a **pointer** on both request shapes so "absent" and "false" stay distinguishable, which is what lets the create path write `1` for a live-timings-capable upstream kind.

**Tech Stack:** Go (`gateway/backend`), SQLite + PostgreSQL through the one `SQLStore`, `routing.MemoryStore` for free (whole-struct copies), no new dependencies, **no frontend file at all**.

## Global Constraints

- **Migration 80, appended.** The ledger `var migrations = []migration{` opens at `internal/store/migrate.go:35` and closes at `:115`; its head is `{version: 79, name: "model_mappings_drop_capability_columns", up: migration79Up},` at `:114`. **The next free version is 80.** Append after 79 — never insert between 78 and 79: `migrate.go:131-133` and `migration79_drop_capability_columns_test.go:134-156` both record that the 78→79 order is load-bearing and that nothing may ever go between them.
- **`baselineCreateStatements` is FROZEN as of v60** (`migrate.go:403-415`: "a new migration that adds a column must NOT also add it here"). The new column must **NOT** be added to the baseline `create table if not exists applications (...)` block at `migrate.go:653-677`, nor to `migration65Up`'s `create table if not exists agent_runtime_specs (...)` at `migrate.go:2995-3012`. A fresh install gets the column by replaying the duplicate-tolerant chain. *(The site survey for this issue contains one entry claiming the column "must appear BOTH in the baseline DDL and as an additive migration" — that entry is wrong, and contradicted by the frozen-baseline doc at `migrate.go:403-415` and by migration 70's and 72's own doc comments. Do not follow it.)*
- **`addColumnIfMissing` is the helper** (`migrate.go:258`, doc `:236-257`). The exact call form to copy is migration 70's, verbatim from `migrate.go:3213-3215`:
  ```go
  if err := addColumnIfMissing(ctx, tx, dl, "applications",
      "proxy_excluded integer not null default 0"); err != nil {
      return err
  }
  ```
- **The spec backfill follows migration 72's join, in both dialects.** `agent_runtime_specs.mapping_id → model_mappings.application_id → applications`. PostgreSQL uses `update … from` with the join; SQLite uses correlated subqueries, because modernc SQLite does not support `update … from` across this join shape reliably. Copy the structure at `migrate.go:3305-3330` — including the `if dl.name() == "postgres" { … }` split and the idempotency guard on the target column.
- **The PostgreSQL leg is mandatory and skips SILENTLY.** `forEachDialect` (`internal/store/conformance_test.go:27-58`) and `forEachRoutingStoreSeeded` (`internal/store/routing_store_conformance_test.go:41-96`) both call `t.Skip("set OP_AI_GATEWAY_TEST_POSTGRES_DSN to run postgres conformance tests")`, not `t.Fatal`, so the whole store suite reports PASS having exercised SQLite only. This is a store change, so every store task must run with the DSN set and **record the pass/skip counts**:
  ```bash
  docker run -d --rm --name oag-pg -e POSTGRES_PASSWORD=postgres -e POSTGRES_DB=op_test \
    -p 5432:5432 postgres:17-alpine        # the image CI uses
  export OP_AI_GATEWAY_TEST_POSTGRES_DSN='postgres://postgres:postgres@127.0.0.1:5432/op_test?sslmode=disable'
  LOG="${TMPDIR:-/tmp}/oag81-store.log"
  cd /Users/jlor08/Developer/codex/op-ai-gateway/.worktrees/responses-live-timings/gateway/backend
  go test ./internal/store/ -run '<the task's test names>' -count=1 -v 2>&1 | tee "$LOG"
  echo "PASS=$(grep -c -- '--- PASS' "$LOG") SKIP=$(grep -c -- '--- SKIP' "$LOG")"
  grep -E -- '--- (PASS|SKIP): .*(sqlite|postgres)' "$LOG"
  ```
  A run whose SKIP count is non-zero for a `postgres` subtest has **not** tested PostgreSQL — fix the DSN and re-run. Paste both counts into the task's commit body.
- **`ActiveMappingsForModel` is the dangerous reader.** `applications` has THREE independently hand-maintained select lists (`internal/store/sqlite_applications.go:140-151`, `:159-166`, `:453-461`) feeding TWO scan functions, and `ActiveMappingsForModel` is the one that decides where live traffic goes. A column missed *there* reads back as a clean zero while every memory-backed portal test still passes (`docs/architecture/cross-cutting/persistence.md:425-435` records this). A test must fail if the column is missing from **that** query specifically — `TestConformanceApplicationReadersAgreeOnEveryColumn` (`application_column_parity_test.go:80`) is that test, and it only sees the new column once the column is seeded into `applicationParityBools`.
- **A reordered select list does not fail loudly.** Both scan functions have fixed arity, so an *omitted* column errors at `Scan` ("expected N destination arguments"), but two same-typed columns **swapped** read each other's values silently. The guard is `applicationParityBools` (`application_column_parity_test.go:41-46`): one distinct, non-all-false bit pattern per integer-boolean column across three seeded rows. This plan widens it from `[4][3]bool` to `[5][3]bool` (a compile error if forgotten), adds the fifth pattern, seeds the field from it, appends the column name to the `names` list at `:228-231`, and fixes the `2^r >= 4` arithmetic comment at `:15-23` to `>= 5`. On the spec side the two copies of the column order — `runtimeSpecCols` (`sqlite_runtime.go:78-84`) and `runtimeSpecColsPrefixed` (`:86-96`) — must be edited **together**; a divergence produces a positionally wrong read only on `RuntimeSpecsByApplication`, the list read, so a spot check through `RuntimeSpecByMapping` looks fine.
- **Request shapes take a POINTER, never a plain `bool`.** Absent and false are the same value for a plain `bool`, so the kind-dependent create default would be dead code for any client that sends the key — and the portal's own forms always send the whole body. The house precedent, with its own justification, is `ProxyExcluded *bool \`json:"proxy_excluded,omitempty"\`` at `internal/portal/service_applications.go:259-265`. `*bool` on `CreateApplicationRequest`, on `UpdateApplicationRequest`, and on `PutRuntimeSpecRequest`. A plain `bool` on the two **DTOs** (a DTO always states the stored value).
- **`putRequestFromDTO` is a hand-written spread that compiles without the new field** (`internal/portal/service_runtime_benchmark.go:30-58`). Its own doc (`:16-22`) records that this exact class of defect was already paid for once: a spec write assembled from a hand-picked field list quietly reset the operator's binary path, args, timeouts and GPU rows while a narrow test passed. `TestPutRequestFromDTOCoversEveryWritableField` (`service_runtime_benchmark_test.go:78-204`) catches it by comparing JSON **tag names** in both directions — so the `bool` DTO field and the `*bool` request field pass that half, and the `reflect.DeepEqual` half then needs the pointer conversion to be right.
- **The DTO mappers are hand-written and the application side has no coverage test.** `applicationDTO` (`service_applications.go:836-870`) and `runtimeSpecDTO` (`service_runtime.go:1158-1194`) are literals; a field added to a DTO but not to its mapper is the Go zero value on the wire, with no compile error and a 200 OK. The spec side is guarded by the reflection test above; the application side must be walked by hand and pinned by an explicit assertion.
- **One name in every layer.** Column `responses_live_timings_enabled`; Go field `ResponsesLiveTimingsEnabled` on `routing.Application`, `routing.RuntimeSpec`, `routing.Target`, `ApplicationDTO`, `CreateApplicationRequest`, `UpdateApplicationRequest`, `RuntimeSpecDTO`, `PutRuntimeSpecRequest`; JSON tag `responses_live_timings_enabled`. `Target.OpportunisticMetrics` drops its `Enabled` suffix, but uniformity is worth more here than that one precedent — do not shorten the name in any layer.
- **Not on `routing.ModelMapping`.** Its doc block (`internal/routing/store.go:607-649`) is an explicit prohibition — "A CAPABILITY IS NOT A FIELD HERE" — and migration 79 dropped the eleven columns that used to be. It is also not a `model_mapping_capabilities` row: those are observed/probed verdicts with ranked provenance, and this is an operator-owned setting. `MappingCandidate` needs no field either — the boolean rides inside the embedded `Application` value.
- **SQLite has no boolean.** Every integer-boolean column needs THREE edits per scan function: an `int64` local, a `&local` destination in positional order, and a `!= 0` conversion afterwards. Passing `&app.SomeBool` compiles and fails at runtime on one driver.
- **No gate, no injection, no retry, no frontend.** Nothing in part 1 may read the boolean for a routing or request decision. No file under `gateway/frontend/` is touched: no operator control, no TypeScript type, no i18n key. The survey's TypeScript hazards — `PutRuntimeSpecRequest` being derived by `Omit<RuntimeSpec, …>` (`gateway/frontend/src/api/runtime.ts:132-149`), which would silently make a new `RuntimeSpec` field a *required* request field, and `applicationTypeDefaults.ts:14-23` claiming per-type authority that would kill the backend's kind-dependent default — **belong to part 2**, together with the control itself. They are not oversights in this plan.
- **Renaming `docs/architecture/reference/data-model.md:245` ("## 4. Migration history (79 migrations)") breaks SEVEN anchor links** — `09-architecture-decisions.md:510`, `:566`, `:735`, `:896`, `:1051` and `cross-cutting/telemetry-usage-observability.md:895`, `:1297` all point at `#4-migration-history-79-migrations`. `./scripts/check-docs.sh` resolves anchors against the target document's own headings, so a partial update fails the gate. Update the heading and all seven links in the same commit.
- **Every new test must FAIL with its production change reverted.** Revert **production files only** — reverting a test makes `go test -run X` print "ok" with zero tests, a fake pass. Record which mutation broke which test in the commit body.
- **Per-module gates before every commit:** from `gateway/backend` (and `server-agent` if it were touched, which it is not): `~/go/bin/golangci-lint fmt --diff` (must print **nothing**), `~/go/bin/golangci-lint run`, `go test ./... -count=1`. Docs: `./scripts/check-docs.sh` and `bash ./scripts/check-docs.test.sh`. Inserting one field re-aligns a whole struct-tag column under gofumpt, so `fmt --diff` is not optional.
- **Branch `responses-live-timings`**, worktree `/Users/jlor08/Developer/codex/op-ai-gateway/.worktrees/responses-live-timings`. Never commit to or merge into `main`; never bare `git stash`; work only inside the worktree and always with absolute paths.
- `docs/superpowers/` is branch-local and is removed before the pull request.

---

## File Structure

Production:

- `gateway/backend/internal/store/migrate.go` — the ledger entry `{version: 80, name: "application_responses_live_timings", up: migration80Up}` and `migration80Up`: two `addColumnIfMissing` calls plus the spec-from-parent-application backfill in both dialects. **Nothing else in this file changes** (the frozen baseline and migration 65's create-table stay untouched).
- `gateway/backend/internal/store/sqlite_applications.go` — the five hand-maintained `applications` column lists (insert, update `set`, three selects) plus the two positional scans. Owns the column's position in every list.
- `gateway/backend/internal/store/sqlite_runtime.go` — the `agent_runtime_specs` upsert (column list, placeholder row, `on conflict … do update set` list, arg list), the two column-order consts, and `scanRuntimeSpec`.
- `gateway/backend/internal/routing/store.go` — `Application.ResponsesLiveTimingsEnabled` and `RuntimeSpec.ResponsesLiveTimingsEnabled`, the fields both SQL drivers round-trip and `MemoryStore` carries for free.
- `gateway/backend/internal/routing/resolver.go` — `Target.ResponsesLiveTimingsEnabled` and the spec-precedence seeding in `targetFrom`.
- `gateway/backend/internal/routing/live_timings.go` (new) — `LiveTimingsCapableKind(kind string) bool`, the exported two-kind set the portal's create default reads. The only new production file.
- `gateway/backend/internal/portal/service_applications.go` — the field on `ApplicationDTO` (`bool`), on `CreateApplicationRequest` (`*bool`) and on `UpdateApplicationRequest` (`*bool`); the kind-dependent create default; the update arm; the retype reset; the `applicationDTO` mapper line.
- `gateway/backend/internal/portal/service_runtime.go` — the field on `RuntimeSpecDTO` (`bool`) and `PutRuntimeSpecRequest` (`*bool`); the `!hadExisting` kind-dependent default; the incapable-kind clamp; the `runtimeSpecDTO` mapper line.
- `gateway/backend/internal/portal/service_runtime_benchmark.go` — `putRequestFromDTO`'s spread gains the pointer conversion.

Tests:

- `gateway/backend/internal/store/migration80_responses_live_timings_test.go` (new) — the column's presence and declared type on both tables and both dialects, the backfill join, replay idempotency.
- `gateway/backend/internal/store/conformance_test.go` — a new `TestConformanceApplicationResponsesLiveTimings` (create/read/update/routing-join, both dialects).
- `gateway/backend/internal/store/application_column_parity_test.go` — the `[5][3]bool` fixture, the seeded field, the `names` list, the arithmetic comment.
- `gateway/backend/internal/store/routing_store_conformance_test.go` — the spec-side round-trip inside `TestRoutingStoreRuntimeSpecs` (memory + sqlite + postgres).
- `gateway/backend/internal/routing/resolver_live_timings_test.go` (new) — `targetFrom`'s precedence, all four cases.
- `gateway/backend/internal/routing/live_timings_test.go` (new) — the capable-kind predicate over every provider constant.
- `gateway/backend/internal/provider/live_progress_kind_parity_test.go` (new) — the drift tripwire: `liveProgressUpstreams`' keys and `routing.LiveTimingsCapableKind` must agree. Test-only; **no** production change in `internal/provider`.
- `gateway/backend/internal/portal/service_applications_test.go` — create default per kind, explicit pointer both ways, PATCH keep-if-nil, retype reset, DTO echo.
- `gateway/backend/internal/portal/service_runtime_test.go` — spec create default per effective kind, no-inheritance, update-preserves, incapable clamp, DTO echo.
- `gateway/backend/internal/portal/service_runtime_benchmark_test.go` — the fully-populated DTO in `TestPutRequestFromDTOCoversEveryWritableField` gains the field.

Docs:

- `docs/architecture/reference/data-model.md` — §4's heading (79→80 migrations), the seven inbound anchors' target, a new `### Responses live timings` sub-section with the migration-80 row, and the new column named in the `applications` and `agent_runtime_specs` table rows (`:39`, `:52`).
- `docs/architecture/cross-cutting/persistence.md:151`, `:176` — the two hand-maintained "79" counts.
- `docs/architecture/09-architecture-decisions.md`, `docs/architecture/cross-cutting/telemetry-usage-observability.md` — the seven anchor links only.
- `docs/architecture/reference/api-surface.md` — the wire contract for the new boolean under the endpoint-modes sub-section (`:494-538`).
- `docs/architecture/cross-cutting/agent-runtime-manager.md:3738-3748` — one sentence: the spec carries its own copy, and the resolved spec wins.

Explicitly **not** changed in part 1: anything under `gateway/frontend/`, `internal/gateway/native_passthrough.go`, `internal/provider/live_progress.go`, `internal/provider/openai_compatible.go`, `docs/architecture/cross-cutting/compatibility-and-inference.md`, and `telemetry-usage-observability.md`'s §8.4.3 non-injection rule. The rule is still true after part 1 — nothing injects anything — and reversing its prose belongs with the code that reverses it.

---

### Task 1: Migration 80 — the column on both tables, with the spec backfill

**Files:**
- Modify: `gateway/backend/internal/store/migrate.go` (append one ledger entry after line 114; add `migration80Up` at the end of the migration bodies, after `migration79Up`)
- Create: `gateway/backend/internal/store/migration80_responses_live_timings_test.go`
- Modify: `docs/architecture/reference/data-model.md` (`:39`, `:52`, `:245`, and a new sub-section at the end of §4 after the `### The per-model capability table` table), `docs/architecture/cross-cutting/persistence.md` (`:151`, `:176`), `docs/architecture/09-architecture-decisions.md` (`:510`, `:566`, `:735`, `:896`, `:1051`), `docs/architecture/cross-cutting/telemetry-usage-observability.md` (`:895`, `:1297`)

**Interfaces:**
- Consumes: `addColumnIfMissing(ctx context.Context, tx *sql.Tx, dl dialect, table, colDef string) error` (`migrate.go:258`); `execTx(ctx context.Context, tx *sql.Tx, dl dialect, q string) error` (`migrate.go:232`); `dl.name() string`; test helpers `forEachDialect(t *testing.T, run func(t *testing.T, s *SQLStore))` (`conformance_test.go:27`), `tableColumnTypes(ctx context.Context, t *testing.T, s *SQLStore, table string) map[string]string` (`migration79_drop_capability_columns_test.go:45`), `mustExec(ctx context.Context, t *testing.T, s *SQLStore, q string, args ...any)` (`migration72_endpoint_modes_test.go:36`).
- Produces: `func migration80Up(ctx context.Context, tx *sql.Tx, dl dialect) error`, and the column `responses_live_timings_enabled integer not null default 0` on `applications` and on `agent_runtime_specs`. No Go struct field yet — this task's tests read the column through raw SQL only.

- [ ] **Step 1: Add the failing test file.**

Create `gateway/backend/internal/store/migration80_responses_live_timings_test.go` with the SPDX header used by every file in the package (copy the two comment lines from `migration72_endpoint_modes_test.go:1-2`), `package store`, and these three tests.

```go
// reinvokeMigration80 re-runs migration80Up in its own tx after seeding rows,
// mirroring reinvokeMigration72: the ADD COLUMNs are duplicate-tolerant and
// the backfill UPDATE is guarded on the target column still being 0, so a
// replay is a no-op on an already-backfilled row.
func reinvokeMigration80(ctx context.Context, t *testing.T, s *SQLStore) {
	t.Helper()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	if err := migration80Up(ctx, tx, s.dl); err != nil {
		_ = tx.Rollback()
		t.Fatalf("migration80Up: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

// specLiveTimings reads the raw column for one spec id. Raw SQL on purpose:
// this task adds no Go struct field, so the column has no other reader yet.
func specLiveTimings(ctx context.Context, t *testing.T, s *SQLStore, specID string) int64 {
	t.Helper()
	var v int64
	q := `select responses_live_timings_enabled from agent_runtime_specs where id = ?`
	if err := s.db.QueryRowContext(ctx, s.dl.rebind(q), specID).Scan(&v); err != nil {
		t.Fatalf("read spec column: %v", err)
	}
	return v
}

// TestMigration80AddsTheColumnToBothTablesWithThePrecedentType proves the
// column exists on BOTH tables on BOTH dialects after a plain Migrate, and
// that its declared type matches an existing integer boolean on the same
// table -- comparing against a sibling column rather than a literal keeps the
// assertion dialect-independent while still catching ADR-005's
// narrow-vs-wide hazard, which is live here because the column lands on a
// table whose baseline CREATE is frozen.
func TestMigration80AddsTheColumnToBothTablesWithThePrecedentType(t *testing.T) {
	forEachDialect(t, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		appCols := tableColumnTypes(ctx, t, s, "applications")
		got, ok := appCols["responses_live_timings_enabled"]
		if !ok {
			t.Fatalf("applications has no responses_live_timings_enabled column; got %v", sortedColumnNames(appCols))
		}
		if want := appCols["opportunistic_metrics_enabled"]; got != want {
			t.Fatalf("applications.responses_live_timings_enabled type = %q, want %q (the precedent integer boolean on this table)", got, want)
		}
		specCols := tableColumnTypes(ctx, t, s, "agent_runtime_specs")
		gotSpec, ok := specCols["responses_live_timings_enabled"]
		if !ok {
			t.Fatalf("agent_runtime_specs has no responses_live_timings_enabled column; got %v", sortedColumnNames(specCols))
		}
		if want := specCols["enabled"]; gotSpec != want {
			t.Fatalf("agent_runtime_specs.responses_live_timings_enabled type = %q, want %q", gotSpec, want)
		}
	})
}
```

Then the backfill test. It seeds a `server_agent` application, a mapping and a spec through the store API (none of which knows the column yet), forces the pre-80 shape with raw SQL, re-invokes the migration and reads the column back:

```go
// TestMigration80SnapshotsTheSpecFromItsParentApplication proves the
// spec-from-parent-application join, in whichever dialect's SQL is under test.
//
// Why this test has to seed a non-zero application value by hand: on a real
// upgrade BOTH columns are new and default 0, so the backfill is provably a
// no-op on every existing row. That is exactly the decided requirement (an
// upgrade changes no deployment's behaviour) -- and it is also why a wrong
// join would never be noticed without this fixture.
func TestMigration80SnapshotsTheSpecFromItsParentApplication(t *testing.T) {
	forEachDialect(t, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		now := time.Now().UTC().Truncate(time.Second)
		if err := s.CreateAIServer(ctx, routing.AIServer{
			ID: "srv_m80", Name: "M80", Provider: routing.ProviderVLLM, Endpoint: "http://m80:8000",
			Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy,
			CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("create server: %v", err)
		}
		if err := s.CreateApplication(ctx, routing.Application{
			ID: "app_m80", ServerID: "srv_m80", Type: routing.ProviderServerAgent, Port: 8091, Scheme: "http",
			APIFlavors: []string{routing.APIFlavorOpenAI}, Status: routing.ServerStatusActive,
			HealthCheckMode: routing.HealthCheckModeAlwaysReachable, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("create app: %v", err)
		}
		if err := s.CreateMapping(ctx, routing.ModelMapping{
			ID: "map_m80", ApplicationID: "app_m80", GatewayModelName: "m80-model", AppModelName: "up-m80",
			Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("create mapping: %v", err)
		}
		for _, id := range []string{"spec_m80", "spec_m80_on"} {
			mappingID := "map_m80"
			if id == "spec_m80_on" {
				// A second mapping so both specs can exist (mapping_id is unique).
				mappingID = "map_m80_b"
				if err := s.CreateMapping(ctx, routing.ModelMapping{
					ID: mappingID, ApplicationID: "app_m80", GatewayModelName: "m80-model-b", AppModelName: "up-m80-b",
					Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now,
				}); err != nil {
					t.Fatalf("create mapping b: %v", err)
				}
			}
			if err := s.UpsertRuntimeSpec(ctx, routing.RuntimeSpec{
				ID: id, MappingID: mappingID, Enabled: true, Binary: "/usr/bin/llama-server",
				Args: "[]", Env: "{}", CreatedAt: now, UpdatedAt: now,
			}); err != nil {
				t.Fatalf("upsert spec %s: %v", id, err)
			}
		}
		// The pre-80 shape the backfill exists for: the parent application is
		// ON, the first spec is still 0, and the second is already 1 so the
		// idempotency guard has something to protect.
		mustExec(ctx, t, s, `update applications set responses_live_timings_enabled = 1 where id = ?`, "app_m80")
		mustExec(ctx, t, s, `update agent_runtime_specs set responses_live_timings_enabled = 0 where id = ?`, "spec_m80")
		mustExec(ctx, t, s, `update agent_runtime_specs set responses_live_timings_enabled = 1 where id = ?`, "spec_m80_on")

		reinvokeMigration80(ctx, t, s)

		if got := specLiveTimings(ctx, t, s, "spec_m80"); got != 1 {
			t.Fatalf("spec_m80 = %d, want 1 snapshotted from its parent application through mapping_id -> application_id", got)
		}
		if got := specLiveTimings(ctx, t, s, "spec_m80_on"); got != 1 {
			t.Fatalf("spec_m80_on = %d, want 1 left untouched", got)
		}

		// Replay: a second invocation changes nothing, and an application
		// turned OFF after the first backfill does not drag its spec back.
		mustExec(ctx, t, s, `update applications set responses_live_timings_enabled = 0 where id = ?`, "app_m80")
		reinvokeMigration80(ctx, t, s)
		if got := specLiveTimings(ctx, t, s, "spec_m80"); got != 1 {
			t.Fatalf("after replay spec_m80 = %d, want 1: the backfill must be guarded on the column still being 0", got)
		}
	})
}
```

Imports: `context`, `op-ai-gateway/internal/routing`, `testing`, `time`.

- [ ] **Step 2: Run the new tests and watch them fail.**

```bash
cd /Users/jlor08/Developer/codex/op-ai-gateway/.worktrees/responses-live-timings/gateway/backend
go test ./internal/store/ -run 'TestMigration80' -count=1
```
Expected: a **compile** failure — `undefined: migration80Up`. That is the correct first failure; the test file references the function the next step writes.

- [ ] **Step 3: Append the ledger entry.**

In `internal/store/migrate.go`, immediately after line 114 (`{version: 79, …}`) and before the closing `}` at line 115, add exactly one line:

```go
	{version: 80, name: "application_responses_live_timings", up: migration80Up},
```

The name mirrors `application_endpoint_modes` (migration 72), which likewise touched both tables.

- [ ] **Step 4: Write `migration80Up`.**

Add it after `migration79Up`'s body (the last migration body in the file), with a doc comment in the house style. Body:

```go
func migration80Up(ctx context.Context, tx *sql.Tx, dl dialect) error {
	for _, table := range []string{"applications", "agent_runtime_specs"} {
		if err := addColumnIfMissing(ctx, tx, dl, table,
			"responses_live_timings_enabled integer not null default 0"); err != nil {
			return err
		}
	}
	// Snapshot each spec from its parent application, migration72Up's join:
	// agent_runtime_specs.mapping_id -> model_mappings.application_id ->
	// applications. Cross-driver: postgres UPDATE ... FROM a multi-table
	// join; sqlite correlated subqueries (modernc sqlite supports UPDATE ...
	// FROM only since 3.33 and not across this join shape reliably).
	// Guarded on the target still being 0 so a replay never drags a spec the
	// operator has since turned on back to its parent's value.
	if dl.name() == "postgres" {
		return execTx(ctx, tx, dl, `update agent_runtime_specs s
			set responses_live_timings_enabled = a.responses_live_timings_enabled
			from model_mappings m
			join applications a on a.id = m.application_id
			where m.id = s.mapping_id
			  and s.responses_live_timings_enabled = 0`)
	}
	return execTx(ctx, tx, dl, `update agent_runtime_specs
		set responses_live_timings_enabled = coalesce((select a.responses_live_timings_enabled
			from model_mappings m
			join applications a on a.id = m.application_id
			where m.id = agent_runtime_specs.mapping_id), responses_live_timings_enabled)
		where responses_live_timings_enabled = 0`)
}
```

The doc comment must state, in the house voice: the DDL default `0` preserves every running deployment's behaviour on upgrade (decision (d)); the backfill is provably a no-op on a real upgrade because **both** columns are new, and exists so the spec column's provenance is the same as `responses_mode`'s rather than an accident; it **ABORTS the boot on failure** (a deterministic UPDATE with no possibly-dirty pre-check, following `migration70Up` rather than `migration68Up`'s skip policy); and it does **NOT** touch `baselineCreateStatements` (frozen v60) or `migration65Up`'s create-table (append-only), so a fresh install gets both columns by replaying the chain.

- [ ] **Step 5: Run the new tests and watch them pass.**

```bash
cd /Users/jlor08/Developer/codex/op-ai-gateway/.worktrees/responses-live-timings/gateway/backend
go test ./internal/store/ -run 'TestMigration80' -count=1 -v
```

- [ ] **Step 6: Run the PostgreSQL leg and record the counts.**

Run the block from Global Constraints with `-run 'TestMigration80'`. Both `sqlite` and `postgres` subtests must report `--- PASS`; a `--- SKIP: .*postgres` means the DSN is not reaching the harness. Note the two numbers for the commit body.

- [ ] **Step 7: Prove the tests fail with production reverted.**

```bash
cd /Users/jlor08/Developer/codex/op-ai-gateway/.worktrees/responses-live-timings
git stash push -- gateway/backend/internal/store/migrate.go   # production file only
cd gateway/backend && go test ./internal/store/ -run 'TestMigration80' -count=1 ; cd ..
git stash pop
```
Expected: a compile failure (`undefined: migration80Up`). Then a second, sharper mutation: keep the ledger entry and the `applications` column but delete the `agent_runtime_specs` entry from the `for` loop's slice, re-run, and confirm `TestMigration80AddsTheColumnToBothTablesWithThePrecedentType` fails on the spec table. Restore. Record both.

- [ ] **Step 8: Confirm the applied-count assertion still passes.**

```bash
cd /Users/jlor08/Developer/codex/op-ai-gateway/.worktrees/responses-live-timings/gateway/backend
go test ./internal/store/ -run 'TestSQLiteMigrations|TestMigration79' -count=1
```
`sqlite_migration_test.go:247` compares against `len(migrations)`, so it self-updates; `TestMigration79FreshInstallMatchesUpgradedSchema` must stay green (migration 80 is purely additive and replays on both paths).

- [ ] **Step 9: Update the schema docs.**

In `docs/architecture/reference/data-model.md`:
1. `:245` — `## 4. Migration history (79 migrations)` → `## 4. Migration history (80 migrations)`.
2. Add a new sub-section at the end of §4, after the `### The per-model capability table` table:
   ```markdown
   ### Responses live timings

   | # | Migration | Purpose |
   |---|---|---|
   | 80 | `application_responses_live_timings` | Adds `responses_live_timings_enabled integer not null default 0` to `applications` **and** `agent_runtime_specs` through `addColumnIfMissing`, and snapshots each spec from its parent application (`agent_runtime_specs.mapping_id → model_mappings.application_id → applications`, migration 72's join, `update … from` on PostgreSQL and correlated subqueries on SQLite, guarded on the target still being `0` so a replay never overwrites a later operator change). The operator's per-endpoint opt-in to asking a capable upstream for mid-stream timings on a `passthrough` `/v1/responses` stream — **orthogonal** to `responses_mode`, not a fourth value of it. The DDL default of `0` is the whole of the upgrade story: both columns are new, so the backfill is a no-op on every existing row and no running deployment's behaviour changes (the same argument migration 74 makes). The column lives on `applications` and on the spec because for a `server_agent` mapping the resolved spec — not the parent application — is the authority for the endpoint behaviour the flag qualifies, exactly as `responses_mode` is. Nothing in this migration's cut READS the column. Does not touch `baselineCreateStatements` (frozen at v60) or migration 65's create-table. |
   ```
3. `:39` — name the new column in the `applications` row, after `proxy_excluded`'s parenthetical: `, and \`responses_live_timings_enabled\` (migration 80: the operator's per-application opt-in to asking a capable upstream for mid-stream timings on a passthrough \`/v1/responses\` stream — orthogonal to \`responses_mode\`, default off)`.
4. `:52` — the same for the `agent_runtime_specs` row, beside its `api_flavors`/`responses_mode`/`messages_mode` snapshot clause.

In `docs/architecture/cross-cutting/persistence.md`: `:151` "of 79 entries" → "of 80 entries"; `:176` "runs all 79 migrations" → "runs all 80 migrations".

- [ ] **Step 10: Fix the seven anchor links.**

```bash
cd /Users/jlor08/Developer/codex/op-ai-gateway/.worktrees/responses-live-timings
grep -rn '4-migration-history-79-migrations' docs/
```
Expect exactly seven hits: `docs/architecture/09-architecture-decisions.md:510,566,735,896,1051` and `docs/architecture/cross-cutting/telemetry-usage-observability.md:895,1297`. Replace `#4-migration-history-79-migrations` with `#4-migration-history-80-migrations` in all seven, then confirm the grep returns nothing.

- [ ] **Step 11: Docs gates.**

```bash
cd /Users/jlor08/Developer/codex/op-ai-gateway/.worktrees/responses-live-timings
./scripts/check-docs.sh
bash ./scripts/check-docs.test.sh
```
Both must pass. `check-docs.sh` resolves every anchor against the target's real headings, so a missed link fails here.

- [ ] **Step 12: Go gates and commit.**

```bash
cd /Users/jlor08/Developer/codex/op-ai-gateway/.worktrees/responses-live-timings/gateway/backend
~/go/bin/golangci-lint fmt --diff    # must print nothing
~/go/bin/golangci-lint run
go test ./... -count=1
```
Then commit on `responses-live-timings` with a substantive body: the version number and why it is appended, the frozen-baseline rule, the two-dialect backfill and why it is a no-op on a real upgrade, the PostgreSQL pass/skip counts, and the mutation evidence.

---

### Task 2: The `applications` column through every reader

**Files:**
- Modify: `gateway/backend/internal/routing/store.go` (add the field to `type Application struct`, beside `OpportunisticMetricsEnabled` at `:563`)
- Modify: `gateway/backend/internal/store/sqlite_applications.go` (`:29` insert column list, `:31` placeholder row, `:61` insert args, `:95` update `set` list, `:126` update args, `:150` and `:165` select lists, `:460` routing-join select list, `:517`/`:540`/`:556` `scanMappingCandidate`, `:603`/`:634`/`:647` `scanApplication`)
- Modify: `gateway/backend/internal/store/application_column_parity_test.go` (`:15-23` comment, `:41-46` fixture, the seeded literal at `:120-150`, the `names` list at `:228-231`)
- Modify: `gateway/backend/internal/store/conformance_test.go` (new test after `TestConformanceApplicationBenchmarkModes`, which ends at `:834`)

**Interfaces:**
- Consumes: `migration80Up`'s column (Task 1); `forEachDialect(t *testing.T, run func(t *testing.T, s *SQLStore))`.
- Produces: `routing.Application.ResponsesLiveTimingsEnabled bool`, round-tripping through `CreateApplication`, `UpdateApplication`, `ApplicationByID`, `ApplicationsByServer` and `ActiveMappingsForModel` on both dialects. `routing.MemoryStore` carries it for free — `copyApplication` (`memory_store.go:2241`) takes the struct by value and deep-copies only `APIFlavors`, so **no `MemoryStore` change is needed**.

- [ ] **Step 1: Widen the parity fixture and seed the field — the failing test.**

In `application_column_parity_test.go`:

1. `:41` — `var applicationParityBools = [4][applicationParityRows]bool{` → `[5][applicationParityRows]bool{`, and add the fifth row after `proxy_excluded`'s:
   ```go
   	{true, false, false}, // responses_live_timings_enabled
   ```
   It is distinct from all four existing patterns (`{T,T,F}`, `{F,T,T}`, `{F,T,F}`, `{F,F,T}`) and true in row 0 — whose seeded `Type` is `routing.ProviderVLLM`, a live-timings-capable kind, so the fixture is also semantically plausible.
2. `:26-29` — extend the row-order comment's column list to `(always_reachable, benchmark_schedule_enabled, opportunistic_metrics_enabled, proxy_excluded, responses_live_timings_enabled)`.
3. `:15-23` — "the applications select lists carry **FOUR** integer-boolean columns" → **FIVE**, and "`r` must satisfy `2^r >= 4`" → `>= 5` (three rows still suffice: `2^3 = 8`).
4. In the seeded `routing.Application` literal, after `ProxyExcluded: applicationParityBools[3][i],` (`:147`):
   ```go
   				ResponsesLiveTimingsEnabled:      applicationParityBools[4][i],
   ```
5. `:228-231` — append `"responses_live_timings_enabled",` to `names`, in the same order as the bit-pattern rows.

- [ ] **Step 2: Add the cross-driver round-trip — the second failing test.**

In `conformance_test.go`, after `TestConformanceApplicationBenchmarkModes` (ends `:834`), add `TestConformanceApplicationResponsesLiveTimings`, modelled on it: seed a server; create `app_def` with the field unset and assert `ApplicationByID` reads back **false** (the DDL default); create `app_lt` with `ResponsesLiveTimingsEnabled: true` and assert it reads back true; flip it to false via `UpdateApplication` and assert the update; flip it back to true, then create a mapping and assert
```go
		candidates, err := s.ActiveMappingsForModel(ctx, "lt-model", routing.APIFlavorOpenAI)
		if err != nil || len(candidates) != 1 {
			t.Fatalf("active mappings: err=%v n=%d", err, len(candidates))
		}
		if !candidates[0].Application.ResponsesLiveTimingsEnabled {
			t.Fatalf("the ROUTING join lost responses_live_timings_enabled: a column missed in ActiveMappingsForModel reads back as a clean zero while every memory-backed portal test still passes")
		}
```
The doc comment must name `ActiveMappingsForModel` as the reason this test exists and cross-reference `persistence.md:425-435`.

- [ ] **Step 3: Run both and watch them fail.**

```bash
cd /Users/jlor08/Developer/codex/op-ai-gateway/.worktrees/responses-live-timings/gateway/backend
go test ./internal/store/ -run 'TestConformanceApplicationReadersAgreeOnEveryColumn|TestApplicationParityFixtureDistinguishesEverySameTypedPair|TestConformanceApplicationResponsesLiveTimings' -count=1
```
Expected: a compile failure — `routing.Application has no field ResponsesLiveTimingsEnabled`.

- [ ] **Step 4: Add the struct field.**

In `internal/routing/store.go`, inside `type Application struct`, immediately after `OpportunisticMetricsEnabled bool` (`:563`) — i.e. before the `ProxyListenPort` doc block — add:

```go
	// ResponsesLiveTimingsEnabled is the operator's per-application opt-in to
	// asking a capable upstream for MID-STREAM timings on a passthrough
	// /v1/responses stream (migration 80). It is ORTHOGONAL to ResponsesMode,
	// not a fourth value of it: passthrough and telemetry are two axes, and an
	// unknown EndpointMode is served as translate by tryProxyNative's default
	// branch, so a fourth enum value would silently downgrade Codex traffic on a
	// rollback while a boolean defaulting off cannot. Default false = off, so an
	// upgrade changes no running deployment's behaviour. For a server_agent
	// mapping the RESOLVED RuntimeSpec's own copy of this field wins -- see
	// RuntimeSpec.ResponsesLiveTimingsEnabled and Resolver.targetFrom.
	ResponsesLiveTimingsEnabled bool
```

- [ ] **Step 5: Re-run and watch the failure move to the store.**

Same command as Step 3. Expected now: the parity test fails at `Scan` ("expected 32 destination arguments in Scan, not 33" or the mirror), or the round-trip reads back false — either way the SQL has not been touched yet.

- [ ] **Step 6: `CreateApplication` — three coordinated edits.**

In `sqlite_applications.go`:
- `:29` — `proxy_listen_port, proxy_excluded,` → `proxy_listen_port, proxy_excluded, responses_live_timings_enabled,`
- `:31` — add **one** `?` to the placeholder row: **32 → 33**. Count them by hand after editing:
  ```bash
  cd /Users/jlor08/Developer/codex/op-ai-gateway/.worktrees/responses-live-timings/gateway/backend
  sed -n '31p' internal/store/sqlite_applications.go | tr -cd '?' | wc -c   # must print 33
  ```
- after `app.ProxyExcluded,` (`:61`) and **before** `app.CreatedAt,` — add `app.ResponsesLiveTimingsEnabled,`. Position is everything: appending it after `app.UpdatedAt` would compile and write the wrong column.

- [ ] **Step 7: `UpdateApplication` — two coordinated edits.**

- `:95` — `proxy_listen_port = ?, proxy_excluded = ?,` → `proxy_listen_port = ?, proxy_excluded = ?, responses_live_timings_enabled = ?,`
- after `app.ProxyExcluded,` (`:126`) and **before** `app.UpdatedAt,` / `app.ID,` — add `app.ResponsesLiveTimingsEnabled,`. The arg list ends `app.UpdatedAt, app.ID`; anything appended after those maps to the wrong placeholder.

- [ ] **Step 8: The three select lists.**

Append `responses_live_timings_enabled,` to the `proxy_listen_port, proxy_excluded,` line in all three, keeping `created_at, updated_at` last:
- `ApplicationByID`, `:150`
- `ApplicationsByServer`, `:165`
- `ActiveMappingsForModel`, `:460` — here the columns are `a.`-qualified: `a.proxy_listen_port, a.proxy_excluded, a.responses_live_timings_enabled,`

These are three separate copies of the same text, not a shared const. Edit each; then confirm all three agree:
```bash
cd /Users/jlor08/Developer/codex/op-ai-gateway/.worktrees/responses-live-timings/gateway/backend
grep -c 'responses_live_timings_enabled' internal/store/sqlite_applications.go   # expect 5: insert, update set, and the three selects (the Go identifier does not match this pattern)
```

- [ ] **Step 9: `scanMappingCandidate` — the three-spot integer-boolean edit.**

- `:517` — in the `var (…)` block, after `proxyExcluded        int64`, add `liveTimings          int64` (gofumpt will re-align the block; run `fmt` rather than aligning by hand).
- `:540` — after `&c.Application.ProxyListenPort, &proxyExcluded,` and before `&c.Application.CreatedAt,`, add `&liveTimings,`.
- `:556` — after `c.Application.ProxyExcluded = proxyExcluded != 0`, add `c.Application.ResponsesLiveTimingsEnabled = liveTimings != 0`.

- [ ] **Step 10: `scanApplication` — the same three spots.**

- `:603` — after `var proxyExcluded int64`, add `var liveTimings int64`.
- `:634` — after `&proxyExcluded,` and before `&app.CreatedAt,`, add `&liveTimings,`.
- `:647` — after `app.ProxyExcluded = proxyExcluded != 0`, add `app.ResponsesLiveTimingsEnabled = liveTimings != 0`.

- [ ] **Step 11: Run the tests and watch them pass.**

```bash
cd /Users/jlor08/Developer/codex/op-ai-gateway/.worktrees/responses-live-timings/gateway/backend
go test ./internal/store/ -run 'TestConformanceApplication|TestApplicationParity' -count=1 -v
go test ./internal/store/ -count=1
```

- [ ] **Step 12: Run the PostgreSQL leg and record the counts.**

The Global Constraints block with `-run 'TestConformanceApplication|TestApplicationParity'`. Both dialect subtests must PASS for each test.

- [ ] **Step 13: The mutation that matters — drop the column from the routing join only.**

```bash
cd /Users/jlor08/Developer/codex/op-ai-gateway/.worktrees/responses-live-timings/gateway/backend
# Remove ONLY the a.responses_live_timings_enabled token from the ActiveMappingsForModel list (line ~460),
# leave the two other select lists and the scan alone, then:
go test ./internal/store/ -run 'TestConformanceApplicationResponsesLiveTimings|TestConformanceApplicationReadersAgreeOnEveryColumn' -count=1
```
Both must fail (arity at `Scan`). Restore. Then the silent-swap mutation: swap `a.proxy_excluded` and `a.responses_live_timings_enabled` in the routing-join list only, re-run — `TestConformanceApplicationReadersAgreeOnEveryColumn` must fail on a value mismatch, which is the whole point of the fifth bit pattern. Restore. Record both.

- [ ] **Step 14: Gates and commit.**

`~/go/bin/golangci-lint fmt --diff` (nothing), `run`, `go test ./... -count=1`. Commit body: the five list sites and two scans, the 32→33 placeholder count, the fifth bit pattern and why it must be distinct, the PostgreSQL counts, and the two mutations.

---

### Task 3: The `agent_runtime_specs` column through its upsert and both read paths

**Files:**
- Modify: `gateway/backend/internal/routing/store.go` (`type RuntimeSpec struct`, beside `ResponsesMode`/`MessagesMode` at `:1809-1810`)
- Modify: `gateway/backend/internal/store/sqlite_runtime.go` (`:32` insert column list, `:34` placeholder row, `:35-54` the `on conflict … do update set` list, `:61` insert args, `:83` `runtimeSpecCols`, `:95` `runtimeSpecColsPrefixed`, `:355`/`:365`/`:374` `scanRuntimeSpec`)
- Modify: `gateway/backend/internal/store/routing_store_conformance_test.go` (`TestRoutingStoreRuntimeSpecs`, seed at `:1091-1101`, assertion at `:1116-1118`)

**Interfaces:**
- Consumes: `migration80Up`'s `agent_runtime_specs` column (Task 1); `forEachRoutingStore` / `forEachRoutingStoreSeeded(t *testing.T, seedSQL func(*testing.T, *SQLStore), run func(*testing.T, routing.Store))`.
- Produces: `routing.RuntimeSpec.ResponsesLiveTimingsEnabled bool`, round-tripping through `UpsertRuntimeSpec`, `RuntimeSpecByMapping`, `RuntimeSpecByID` and `RuntimeSpecsByApplication` on memory, sqlite and postgres.

- [ ] **Step 1: Extend the conformance test — the failing test.**

In `routing_store_conformance_test.go`, inside `TestRoutingStoreRuntimeSpecs`:
- add `ResponsesLiveTimingsEnabled: true,` to the seeded `routing.RuntimeSpec` literal (`:1091-1101`), on the line after `ResponsesMode: …, MessagesMode: …`;
- extend the flavor/mode assertion at `:1116-1118` to cover it:
  ```go
  		if got.ResponsesMode != routing.EndpointModeTranslate || got.MessagesMode != routing.EndpointModeDisabled ||
  			!got.ResponsesLiveTimingsEnabled ||
  			!reflect.DeepEqual(got.APIFlavors, []string{routing.APIFlavorOpenAI, routing.APIFlavorAnthropic}) {
  			t.Fatalf("flavor/mode/live-timings round-trip mismatch: %+v", got)
  		}
  ```
- and, because `runtimeSpecCols` and `runtimeSpecColsPrefixed` are two independent copies of the same order, add an assertion through the **list** reader as well — that is the only reader `runtimeSpecColsPrefixed` serves, so a divergence between the two consts is invisible to `RuntimeSpecByMapping`. After the existing `RuntimeSpecsByApplication` assertions in that test, add:
  ```go
  		listed, err := s.RuntimeSpecsByApplication(ctx, "app_rt2")
  		if err != nil || len(listed) == 0 {
  			t.Fatalf("RuntimeSpecsByApplication: err=%v n=%d", err, len(listed))
  		}
  		if !listed[0].ResponsesLiveTimingsEnabled {
  			t.Fatalf("the s.-qualified column list (runtimeSpecColsPrefixed) disagrees with runtimeSpecCols: the list read lost responses_live_timings_enabled while the point read kept it")
  		}
  ```
  (Place it where an `app_rt2` spec is already known to exist; if the surrounding test has renamed or moved that spec, use whatever id it upserts and adjust the message, not the assertion.)

- [ ] **Step 2: Run it and watch it fail.**

```bash
cd /Users/jlor08/Developer/codex/op-ai-gateway/.worktrees/responses-live-timings/gateway/backend
go test ./internal/store/ -run 'TestRoutingStoreRuntimeSpecs' -count=1
```
Expected: compile failure — `routing.RuntimeSpec has no field ResponsesLiveTimingsEnabled`.

- [ ] **Step 3: Add the struct field.**

In `internal/routing/store.go`, inside `type RuntimeSpec struct`, after `MessagesMode  EndpointMode` (`:1810`) and before `CreatedAt`:

```go
	// ResponsesLiveTimingsEnabled is this spec's own copy of the
	// per-application live-timings opt-in (migration 80), snapshotted from the
	// parent application at migration time and operator-owned thereafter. It
	// sits beside ResponsesMode for the same reason ResponsesMode does: for a
	// server_agent mapping the RESOLVED spec is the sole authority for its
	// model's endpoint behaviour, and a flag that qualifies "passthrough" must
	// be read from whichever row said "passthrough". Gateway-side only -- never
	// added to AgentRuntimeSpecDTO or the agent wire type.
	ResponsesLiveTimingsEnabled bool
```

- [ ] **Step 4: Re-run — the failure moves to the SQL.**

Same command. The `memory` subtest now passes (whole-struct storage; `copyRuntimeSpec` at `memory_store.go:2248` needs no change), while `sqlite` fails on the round-trip or at `Scan`. That split is itself the proof that the SQL paths are the work.

- [ ] **Step 5: `UpsertRuntimeSpec` — four coordinated edits.**

In `sqlite_runtime.go`:
- `:32` — `type, metrics_path, context_probe_path,` → `type, metrics_path, context_probe_path, responses_live_timings_enabled,`
- `:34` — add one `?`: **30 → 31**. Verify: `sed -n '34p' internal/store/sqlite_runtime.go | tr -cd '?' | wc -c` must print 31.
- `:53` — after `context_probe_path = excluded.context_probe_path,` add `responses_live_timings_enabled = excluded.responses_live_timings_enabled,` (so it lands between `:53` and `updated_at = excluded.updated_at` at `:54`). Miss this and the column is written on a first insert but silently ignored on every subsequent save of the same spec.
- `:61` — after `spec.Type, spec.MetricsPath, spec.ContextProbePath,` and **before** `spec.CreatedAt, spec.UpdatedAt,`, add `spec.ResponsesLiveTimingsEnabled,`.

- [ ] **Step 6: Both column-order consts, together.**

- `runtimeSpecCols` (`:83`) — `type, metrics_path, context_probe_path,` → `type, metrics_path, context_probe_path, responses_live_timings_enabled,`
- `runtimeSpecColsPrefixed` (`:95`) — `s.type, s.metrics_path, s.context_probe_path,` → `s.type, s.metrics_path, s.context_probe_path, s.responses_live_timings_enabled,`

Then check they still describe the same order:
```bash
cd /Users/jlor08/Developer/codex/op-ai-gateway/.worktrees/responses-live-timings/gateway/backend
sed -n '78,96p' internal/store/sqlite_runtime.go | tr -d '\t ' | sed 's/s\.//g'
```
The two blocks must read identically apart from the const names and the leading/trailing lines.

- [ ] **Step 7: `scanRuntimeSpec` — the three-spot edit.**

- `:355` — `var enabled, pinned, vramLocked, setVisibleDevices int64` → `var enabled, pinned, vramLocked, setVisibleDevices, liveTimings int64`
- `:365` — after `&spec.Type, &spec.MetricsPath, &spec.ContextProbePath,` and before `&spec.CreatedAt, &spec.UpdatedAt`, add `&liveTimings,`
- `:374` — after `spec.SetVisibleDevices = setVisibleDevices != 0`, add `spec.ResponsesLiveTimingsEnabled = liveTimings != 0`

- [ ] **Step 8: Run it and watch it pass.**

```bash
cd /Users/jlor08/Developer/codex/op-ai-gateway/.worktrees/responses-live-timings/gateway/backend
go test ./internal/store/ -run 'TestRoutingStoreRuntimeSpecs' -count=1 -v
go test ./internal/store/ -count=1
```

- [ ] **Step 9: PostgreSQL leg, counts recorded.**

The Global Constraints block with `-run 'TestRoutingStoreRuntimeSpecs'`. Three subtests — `memory`, `sqlite`, `postgres` — all PASS.

- [ ] **Step 10: Mutations.**

(a) Remove the column from `runtimeSpecColsPrefixed` only — the new `RuntimeSpecsByApplication` assertion must fail while the point-read assertion still passes; that asymmetry is the divergence hazard, demonstrated. (b) Remove `responses_live_timings_enabled = excluded.…` from the `do update set` list — re-upsert an existing spec with the flag flipped and confirm the read-back keeps the old value. Restore both; record.

- [ ] **Step 11: Gates and commit.**

`fmt --diff`, `run`, `go test ./... -count=1`. Commit body: the four upsert sub-lists, the 30→31 count, both consts, the three scan spots, the PostgreSQL counts, the two mutations.

---

### Task 4: `routing.Target` and `targetFrom`'s spec precedence

**Files:**
- Modify: `gateway/backend/internal/routing/resolver.go` (`type Target struct` at `:48-86`; `targetFrom` at `:1081-1135`)
- Create: `gateway/backend/internal/routing/resolver_live_timings_test.go`

**Interfaces:**
- Consumes: `routing.Application.ResponsesLiveTimingsEnabled` (Task 2), `routing.RuntimeSpec.ResponsesLiveTimingsEnabled` (Task 3); `(*Resolver).targetFrom(ctx context.Context, c MappingCandidate, apiFlavor string) (Target, error)`; `routing.NewMemoryStore()`.
- Produces: `routing.Target.ResponsesLiveTimingsEnabled bool` — the resolved spec's value for a `server_agent` mapping that has a spec, the application's otherwise. **Read by nothing in part 1.**

- [ ] **Step 1: Write the failing precedence test.**

Create `gateway/backend/internal/routing/resolver_live_timings_test.go` (`package routing`, SPDX header as in `resolver_endpoint_mode_test.go`). Model the fixture on `resolver_endpoint_mode_test.go`, which already builds a resolver over `NewMemoryStore()` and drives a `server_agent` application with and without a spec — read it first and reuse its seeding helper if it has one rather than writing a second.

Five table cases, all resolving one model through `Resolve` (or `targetFrom` directly if the existing test does):

| case | app | spec | want `Target.ResponsesLiveTimingsEnabled` |
|---|---|---|---|
| ordinary application, flag on | `Type: routing.ProviderLlamaCPP`, flag `true` | none | `true` |
| ordinary application, flag off | `Type: routing.ProviderLlamaCPP`, flag `false` | none | `false` |
| `server_agent`, spec overrides app OFF→ON | flag `false` | spec present, flag `true` | `true` |
| `server_agent`, spec overrides app ON→OFF | flag `true` | spec present, flag `false` | `false` |
| `server_agent`, no spec at all | flag `true` | none | `true` (the app is the documented no-spec fallback) |

The last two rows are the load-bearing ones: they are what distinguishes "the spec wins" from "the app always wins", and a `Target` field seeded from `app.…` unconditionally (the `OpportunisticMetrics` pattern at `:1121`) passes the first three and fails the fourth. The test's doc comment must say so.

- [ ] **Step 2: Run it and watch it fail.**

```bash
cd /Users/jlor08/Developer/codex/op-ai-gateway/.worktrees/responses-live-timings/gateway/backend
go test ./internal/routing/ -run 'LiveTimings' -count=1
```
Expected: compile failure — `Target has no field ResponsesLiveTimingsEnabled`.

- [ ] **Step 3: Add the `Target` field.**

In `resolver.go`, inside `type Target struct`, after the `OpportunisticMetrics bool` block (`:74-77`) and before the `LiveProgressSupport` block:

```go
	// ResponsesLiveTimingsEnabled is the EFFECTIVE per-endpoint live-timings
	// opt-in for this request: the resolved RuntimeSpec's value for a
	// server_agent mapping that has a spec, the resolved application's
	// otherwise -- the same precedence ResponsesMode/MessagesMode use above,
	// and deliberately NOT OpportunisticMetrics' app-only shape, because this
	// flag qualifies a decision (ResponsesMode == passthrough) that comes from
	// the spec for a server_agent child. Nothing reads it yet; the gate, the
	// injection and the retry are part 2 of issue #81.
	ResponsesLiveTimingsEnabled bool
```

- [ ] **Step 4: Seed it beside the modes in `targetFrom`.**

Mirror the modes exactly. At `:1083` the seed line today is

```go
	flavors, responsesMode, messagesMode := app.APIFlavors, app.ResponsesMode, app.MessagesMode
```

and at `:1093`, inside `if ok {`, the override is

```go
			flavors, responsesMode, messagesMode = spec.APIFlavors, spec.ResponsesMode, spec.MessagesMode
```

Extend both, as a separate statement rather than a fourth element (keeping the existing tuple lines untouched keeps the diff and the blame readable):

```go
	flavors, responsesMode, messagesMode := app.APIFlavors, app.ResponsesMode, app.MessagesMode
	// Same precedence as the modes on the line above, for the same reason: the
	// flag qualifies ResponsesMode, so it must be read from whichever row said
	// what ResponsesMode says. Kept as its own statement so the mode tuple
	// stays the three values it has always been.
	liveTimings := app.ResponsesLiveTimingsEnabled
```

and, inside the `if ok {` block, immediately after the tuple override at `:1093`:

```go
			liveTimings = spec.ResponsesLiveTimingsEnabled
```

Then in the returned `Target` literal, after `OpportunisticMetrics: app.OpportunisticMetricsEnabled,` (`:1121`):

```go
		ResponsesLiveTimingsEnabled: liveTimings,
```

Note for the implementer: `app.Type == ProviderServerAgent` with `ok == false` leaves `spec` zero-valued and `liveTimings` at the application's value — which is the documented no-spec fallback (`targetFrom`'s own doc at `:1075-1080`), and is why the fifth test row expects `true`.

- [ ] **Step 5: Run it and watch it pass.**

```bash
cd /Users/jlor08/Developer/codex/op-ai-gateway/.worktrees/responses-live-timings/gateway/backend
go test ./internal/routing/ -run 'LiveTimings' -count=1 -v
go test ./internal/routing/ -count=1
```

- [ ] **Step 6: Prove nothing reads it.**

```bash
cd /Users/jlor08/Developer/codex/op-ai-gateway/.worktrees/responses-live-timings/gateway/backend
grep -rn 'ResponsesLiveTimingsEnabled' --include='*.go' internal/gateway/ internal/provider/
```
Must print nothing. Part 1 ships a value no request path reads; a hit here means the gate leaked out of part 2.

- [ ] **Step 7: Mutation.**

Replace `ResponsesLiveTimingsEnabled: liveTimings,` with `ResponsesLiveTimingsEnabled: app.ResponsesLiveTimingsEnabled,` — the two `server_agent`-with-spec rows must fail, and only those. Restore. Then delete the `liveTimings = spec.…` line inside `if ok {` — the same two rows fail. Restore. Record both; they are the evidence that the precedence, not just the plumbing, is tested.

- [ ] **Step 8: Gates and commit.**

`fmt --diff`, `run`, `go test ./... -count=1`. Commit body: which precedence was chosen and the two lines it mirrors, the no-spec fallback, and the grep proving no reader.

---

### Task 5: The capable-kind predicate, and a tripwire against drift

**Files:**
- Create: `gateway/backend/internal/routing/live_timings.go`
- Create: `gateway/backend/internal/routing/live_timings_test.go`
- Create: `gateway/backend/internal/provider/live_progress_kind_parity_test.go`

**Interfaces:**
- Consumes: the provider/spec-type constants in `internal/routing/store.go:15-24` (`ProviderMock`, `ProviderOllama`, `ProviderVLLM`, `ProviderLlamaCPP`, `ProviderLlamaSwap`, `ProviderLiteLLM`, `ProviderServerAgent`) and `internal/routing/runtime_spec_type.go:20-26` (`RuntimeSpecTypeVLLM`, `RuntimeSpecTypeLlamaCpp`, `RuntimeSpecTypeTGI`, `RuntimeSpecTypeOllama`, `RuntimeSpecTypeCustom`).
- Produces: `func routing.LiveTimingsCapableKind(kind string) bool` — true for exactly `"llama_cpp"` and `"vllm"`. Consumed by Task 6 (application create) and Task 7 (spec upsert). **No production file in `internal/provider` changes**; the new file there is a test.

- [ ] **Step 1: Write the failing predicate test.**

Create `internal/routing/live_timings_test.go` with a table over every provider constant and every `RuntimeSpecType` constant, asserting `true` for `ProviderLlamaCPP`, `ProviderVLLM`, `string(RuntimeSpecTypeLlamaCpp)` and `string(RuntimeSpecTypeVLLM)`, and `false` for `ProviderMock`, `ProviderOllama`, `ProviderLlamaSwap`, `ProviderLiteLLM`, `ProviderServerAgent`, `string(RuntimeSpecTypeTGI)`, `string(RuntimeSpecTypeOllama)`, `string(RuntimeSpecTypeCustom)`, `""` and `"LLAMA_CPP"` (the comparison is case-sensitive, like `portal.validEndpointMode`). `ProviderServerAgent → false` is the load-bearing row: a `server_agent` application's type says nothing about what actually serves.

- [ ] **Step 2: Run it and watch it fail** — `undefined: LiveTimingsCapableKind`.

```bash
cd /Users/jlor08/Developer/codex/op-ai-gateway/.worktrees/responses-live-timings/gateway/backend
go test ./internal/routing/ -run 'LiveTimingsCapable' -count=1
```

- [ ] **Step 3: Write the predicate.**

Create `internal/routing/live_timings.go` (SPDX header, `package routing`):

```go
// liveTimingsCapableKinds is the closed set of upstream kinds whose request
// schema is known to tolerate a live-progress request parameter, and therefore
// the set for which a NEWLY CREATED application or runtime spec gets the
// responses live-timings opt-in switched on by default.
//
// It is keyed on the string value BOTH vocabularies share: ProviderLlamaCPP ==
// "llama_cpp" == RuntimeSpecTypeLlamaCpp, and likewise ProviderVLLM == "vllm"
// == RuntimeSpecTypeVLLM. So one lookup serves an ordinary application's Type
// and a server_agent child's EffectiveRuntimeSpecType alike -- which is exactly
// the trick internal/provider's liveProgressUpstreams already relies on, and
// why TestLiveProgressUpstreamsMatchesRoutingCapableKinds pins the two sets
// together rather than letting a second hand-written list drift.
//
// Deliberately NOT listed: server_agent (not an inference server at all -- ask
// EffectiveRuntimeSpecType what actually serves), llama_swap and litellm (both
// resolve a model to an arbitrary downstream that can be api.openai.com, which
// answers 400 on an unrecognized body key), ollama, tgi, custom, mock. The
// default is OFF, so a kind added later never opts in silently.
var liveTimingsCapableKinds = map[string]struct{}{
	ProviderLlamaCPP: {},
	ProviderVLLM:     {},
}

// LiveTimingsCapableKind reports whether kind -- an Application.Type or the
// string form of an EffectiveRuntimeSpecType -- is one of the kinds above.
// Exported because the portal's create paths are its callers and they live in
// another package; it is a statement about a KIND, not a read of any stored
// flag, so nothing about it belongs to the request path.
func LiveTimingsCapableKind(kind string) bool {
	_, ok := liveTimingsCapableKinds[kind]
	return ok
}
```

- [ ] **Step 4: Run it and watch it pass.**

```bash
cd /Users/jlor08/Developer/codex/op-ai-gateway/.worktrees/responses-live-timings/gateway/backend
go test ./internal/routing/ -run 'LiveTimingsCapable' -count=1 -v
```

- [ ] **Step 5: Write the drift tripwire, in package `provider`.**

Create `internal/provider/live_progress_kind_parity_test.go` (`package provider`) with one test:

```go
// TestLiveProgressUpstreamsMatchesRoutingCapableKinds pins the two
// hand-written kind lists together: liveProgressUpstreams (live_progress.go,
// the GATE's shape clause -- which upstreams may be SENT the parameters) and
// routing.LiveTimingsCapableKind (which upstreams a newly created application
// or spec gets the opt-in switched ON for). They answer different questions and
// must not be merged, but they are the same set for the same reason, and a
// divergence would mean the portal defaults an opt-in ON for a kind the gate
// will never honour -- or leaves it OFF for one it would.
//
// This test is the only thing that would notice. It lives in package provider
// because liveProgressUpstreams is unexported; it changes no production code
// here, and part 1 of issue #81 deliberately touches none of this package's
// behaviour.
func TestLiveProgressUpstreamsMatchesRoutingCapableKinds(t *testing.T) {
	for kind := range liveProgressUpstreams {
		if !routing.LiveTimingsCapableKind(kind) {
			t.Errorf("liveProgressUpstreams has %q but routing.LiveTimingsCapableKind(%q) is false", kind, kind)
		}
	}
	for _, kind := range []string{
		routing.ProviderMock, routing.ProviderOllama, routing.ProviderVLLM, routing.ProviderLlamaCPP,
		routing.ProviderLlamaSwap, routing.ProviderLiteLLM, routing.ProviderServerAgent,
		string(routing.RuntimeSpecTypeTGI), string(routing.RuntimeSpecTypeCustom),
	} {
		_, inGate := liveProgressUpstreams[kind]
		if want := routing.LiveTimingsCapableKind(kind); inGate != want {
			t.Errorf("%q: liveProgressUpstreams=%v routing.LiveTimingsCapableKind=%v -- the two kind lists have drifted", kind, inGate, want)
		}
	}
}
```

- [ ] **Step 6: Run it and watch it pass; then mutate it.**

```bash
cd /Users/jlor08/Developer/codex/op-ai-gateway/.worktrees/responses-live-timings/gateway/backend
go test ./internal/provider/ -run 'MatchesRoutingCapableKinds' -count=1 -v
```
Then add `ProviderOllama: {},` to `liveTimingsCapableKinds` in `internal/routing/live_timings.go`, re-run — both this test and the Task 5 predicate table must fail. Restore. Record. (This is the one mutation in the plan that edits a production file the task itself added, which is fine: the test, not the production file, is what is being validated.)

- [ ] **Step 7: Gates and commit.**

`fmt --diff`, `run`, `go test ./... -count=1`. Commit body: what the predicate is for, what it deliberately excludes and why `server_agent` is excluded, and the tripwire's reasoning.

---

### Task 6: The portal's application surface — accept, default, reset, return

**Files:**
- Modify: `gateway/backend/internal/portal/service_applications.go` (`ApplicationDTO` field after `:205`; `CreateApplicationRequest` field after `:255`; `UpdateApplicationRequest` field after `:297`; the create default computed after `:348` and assigned in the literal after `:471`; the update arm after `:733`; the retype reset immediately after it; the `applicationDTO` mapper line after `:862`)
- Modify: `gateway/backend/internal/portal/service_applications_test.go` (new tests; leave `TestApplicationBenchmarkModesCreatePatchAndValidation` at `:353` and `TestCreateApplicationEndpointModeDefaultsToPassthrough` at `:546` untouched and passing)

**Interfaces:**
- Consumes: `routing.LiveTimingsCapableKind(kind string) bool` (Task 5); `routing.Application.ResponsesLiveTimingsEnabled bool` (Task 2); `normalizeApplicationType(raw string) (string, error)` (`service_applications.go:1000-1017`, the closed six-value set, resolved at `:348` before every other normalization).
- Produces: JSON key `responses_live_timings_enabled` — `bool` on `ApplicationDTO`, `*bool` on `CreateApplicationRequest` and on `UpdateApplicationRequest`. No new error sentinel, no new error code: a `*bool` has no invalid non-nil value.

**The rule this task implements, stated once.** `responses_live_timings_enabled` is stored `true` only on a row whose upstream kind is live-timings capable. Concretely:
- **Create**: absent → `routing.LiveTimingsCapableKind(appType)` (decision (d): a newly created llama.cpp or vLLM application gets it on); present → the caller's value; then, either way, forced to `false` if the kind is not capable.
- **Update**: absent → keep the stored value; present → the caller's value; then, **only when `req.Type != nil`** (a retype), forced to `false` if the new kind is not capable (decision (e)).
- A refused value is never an error. The DTO echoes what was **stored**, so a caller who sent `true` for a LiteLLM application gets `false` back and can see it. This is the same display-only discipline `ApiVariantControls` already applies to a mode whose flavor is unchecked, and it is why no `application.live_timings_invalid` sentinel exists.
- Note that the create-path clamp is a small strengthening of decision (e): the design states the reset for a retype, and clamping on create closes the one other way a "stale true for an incapable kind" could be stored (an explicit `true` in a create body). Keeping it total makes the invariant one sentence and a reviewer's check one question.

- [ ] **Step 1: Write the failing tests.**

In `service_applications_test.go`, add these, using whatever service fixture the neighbouring application tests use (they run on `routing.NewMemoryStore()`, so no DSN is needed here):

1. `TestCreateApplicationResponsesLiveTimingsDefaultsByUpstreamKind` — create with the key **absent** for each of the six types `normalizeApplicationType` accepts; assert the returned DTO's `ResponsesLiveTimingsEnabled` is `true` for `routing.ProviderLlamaCPP` and `routing.ProviderVLLM` and `false` for `routing.ProviderOllama`, `routing.ProviderLlamaSwap`, `routing.ProviderLiteLLM` and `routing.ProviderServerAgent`. Two fixture facts the store enforces and the table must respect: `idx_applications_single_server_agent` (migration 68, `migrate.go:3149`) allows **at most one** `server_agent` application per server, and a server with `ManagedRuntimeOnly` set refuses every other type (`service_applications.go:344`) — so seed an ordinary server and put the `server_agent` case on a server of its own, or run each case against a fresh server. The comment must say why `server_agent` is `false` and cannot be anything else: at create time a `server_agent` application has no binary, no spec type and no mappings — specs are per-mapping and created later by `PutRuntimeSpec` — so the real per-kind default for a managed runtime is applied on the spec path (Task 7), not here.
2. `TestCreateApplicationResponsesLiveTimingsHonoursAnExplicitFalse` — create a `llama_cpp` application with `ResponsesLiveTimingsEnabled: boolPtr(false)`; assert the DTO reads `false`, **and** reload through `GetApplication` to assert it was stored, not just echoed. This is the test a plain `bool` would make unwritable: with a plain `bool` this case and case 1 are the same request.
3. `TestCreateApplicationResponsesLiveTimingsRefusesAnIncapableKind` — create a `litellm` application with `ResponsesLiveTimingsEnabled: boolPtr(true)`; assert the DTO reads `false`, no error is returned, and the reload agrees.
4. `TestUpdateApplicationResponsesLiveTimingsKeepsIfNil` — create `llama_cpp` with the flag on; PATCH with only `Port` set; assert the flag survives and `Port` changed. (The house keep-if-nil discipline, mirroring `TestUpdateApplicationPartialUpdatePreservesOtherFields` at `:1067`.)
5. `TestUpdateApplicationResponsesLiveTimingsFlipsBothWays` — PATCH `boolPtr(false)` then `boolPtr(true)` on a `llama_cpp` application; assert each is stored.
6. `TestUpdateApplicationRetypeAwayFromACapableKindClearsResponsesLiveTimings` — create `llama_cpp` with the flag on, then PATCH `Type: strPtr(routing.ProviderLiteLLM)` **and nothing else**; assert the reloaded flag is `false` and the type is `litellm`. Then a second leg: PATCH `Type: strPtr(routing.ProviderLiteLLM)` **together with** `ResponsesLiveTimingsEnabled: boolPtr(true)`; assert the flag is still `false` — the type is the authority on whether the flag can be honest, and the operator can set it again once the type can carry it.
7. `TestUpdateApplicationRetypeToACapableKindDoesNotSwitchItOn` — create `ollama` (flag off), PATCH `Type: strPtr(routing.ProviderLlamaCPP)`; assert the flag is still `false`. Decision (d) scopes the `1` to CREATE; a retype must not silently switch a feature on.
8. `TestApplicationDTOCarriesResponsesLiveTimings` — store an application with the flag on directly through the routes store (bypassing the service), then read it through `ListApplications` **and** `GetApplication`; assert both report `true`. This is the only guard on the hand-written `applicationDTO` mapper: a field on the DTO but not in the mapper is the Go zero value with no compile error.

The package already has the two pointer helpers these tests need — `func strPtr(s string) *string` and `func boolPtr(b bool) *bool`, both at `internal/portal/service_test.go:1345-1346`, and both already used by `TestApplicationBenchmarkModesCreatePatchAndValidation` (`:394-409`). Use them; do not add a second pair.

- [ ] **Step 2: Run them and watch them fail.**

```bash
cd /Users/jlor08/Developer/codex/op-ai-gateway/.worktrees/responses-live-timings/gateway/backend
go test ./internal/portal/ -run 'ResponsesLiveTimings' -count=1
```
Expected: compile failures on the unknown DTO/request fields.

- [ ] **Step 3: The DTO field.**

In `ApplicationDTO`, after `OpportunisticMetricsEnabled bool \`json:"opportunistic_metrics_enabled"\`` (`:205`) — before the `ProxyListenPort` doc block:

```go
	// ResponsesLiveTimingsEnabled is the operator's opt-in to asking a capable
	// upstream for mid-stream timings on a passthrough /v1/responses stream
	// (migration 80). Set from the RAW column and always the STORED value: a
	// create/update that asks for it on an upstream kind that cannot honour it
	// is stored as false, and this field is how the caller sees that.
	ResponsesLiveTimingsEnabled bool `json:"responses_live_timings_enabled"`
```

- [ ] **Step 4: The two request fields, both pointers.**

In `CreateApplicationRequest`, after `OpportunisticMetricsEnabled bool` (`:255`) — i.e. above the `ProxyListenPort` block:

```go
	// ResponsesLiveTimingsEnabled opts this application in to the
	// live-timings request parameter on its passthrough /v1/responses streams.
	//
	// A POINTER, like ProxyExcluded below and for the same reason: absent must
	// be distinguishable from an explicit false, because absent is what gets
	// the kind-dependent default (ON for llama_cpp and vllm) and false is a
	// deliberate off. With a plain bool the default could never fire for any
	// client that sends the key -- which includes every portal form, since they
	// send one whole body for create and update alike.
	ResponsesLiveTimingsEnabled *bool `json:"responses_live_timings_enabled,omitempty"`
```

In `UpdateApplicationRequest`, after `OpportunisticMetricsEnabled *bool` (`:297`):

```go
	// ResponsesLiveTimingsEnabled: nil = keep the stored value, the house
	// sentinel. A retype away from a capable kind clears it regardless -- see
	// UpdateApplication.
	ResponsesLiveTimingsEnabled *bool `json:"responses_live_timings_enabled,omitempty"`
```

Add **no** entry to the validate-before-mutate block at `:594-609`: a `*bool` has no invalid non-nil value, so there is no `ErrApplication…Invalid` for it, and no cross-field refusal is wanted — the flag is inert, not refused, on `translate`/`disabled`.

- [ ] **Step 5: The create default and the create clamp.**

In `CreateApplication`, after the `messagesMode` block (`:374-381`) and before the `routing.Application` literal at `:443` — `appType` has been in scope since `:348`:

```go
	// Decision (d): a newly created application on an upstream kind whose
	// request schema tolerates the parameter starts with the opt-in ON; every
	// other kind gets the DDL default. A server_agent application is NOT such a
	// kind at this point and cannot be: it has no binary, no spec type and no
	// mappings yet -- runtime specs are per-mapping and are created later by
	// PutRuntimeSpec, which applies the per-kind default from the spec's own
	// resolved type.
	liveTimings := routing.LiveTimingsCapableKind(appType)
	if req.ResponsesLiveTimingsEnabled != nil {
		liveTimings = *req.ResponsesLiveTimingsEnabled
	}
	// Decision (e), create half: the stored flag never claims a kind that
	// cannot honour it. An explicit true on such a kind is stored as false and
	// echoed back as false rather than rejected -- a *bool has no invalid
	// non-nil value, and the DTO showing what was actually stored is the honest
	// answer. Same predicate as the default above, deliberately.
	if !routing.LiveTimingsCapableKind(appType) {
		liveTimings = false
	}
```

Then in the `routing.Application` literal, after `OpportunisticMetricsEnabled: req.OpportunisticMetricsEnabled,` (`:471`):

```go
		ResponsesLiveTimingsEnabled:      liveTimings,
```

Do **not** put this in a trailing normalizer beside `applyProxyExclusion` (`:479`). That call is last because it establishes a cross-field invariant with `ProxyListenPort`; this field establishes none, and putting it there would imply one that does not exist.

- [ ] **Step 6: The update arm and the retype reset.**

In `UpdateApplication`, immediately after the `OpportunisticMetricsEnabled` arm (`:732-733`):

```go
	if req.ResponsesLiveTimingsEnabled != nil {
		app.ResponsesLiveTimingsEnabled = *req.ResponsesLiveTimingsEnabled
	}
	// Decision (e): a RETYPE away from a capable kind clears the flag rather
	// than leaving a stale true behind for a kind that can never honour it
	// (LiteLLM, for one, forwards unknown body keys downstream and OpenAI/Azure
	// answer 400). Gated on req.Type != nil, so an ordinary edit never
	// renormalizes a stored value the operator did not touch -- and reading
	// app.Type rather than appType is what makes it the POST-mutation type, the
  // same reason normalizeApplicationTimeoutMS reads app.Type at :671. It runs
	// AFTER the arm above so a PATCH that retypes and sets the flag in one body
	// still ends honest; the operator can set it again once the type can carry
	// it. A retype TO a capable kind deliberately does not switch it on:
	// decision (d) scopes the kind-dependent 1 to create.
	if req.Type != nil && !routing.LiveTimingsCapableKind(app.Type) {
		app.ResponsesLiveTimingsEnabled = false
	}
```

Verify the placement assumption before relying on it: `app.Type` is assigned once, at `:647`, and nothing between there and the end of the function reassigns it (`:668-671` reads it, `:776` reads it) — re-check with
```bash
cd /Users/jlor08/Developer/codex/op-ai-gateway/.worktrees/responses-live-timings/gateway/backend
awk 'NR>640 && NR<800 && /app\.Type *=/ {print NR": "$0}' internal/portal/service_applications.go
```
which must print only line 647's assignment.

- [ ] **Step 7: The DTO mapper.**

In `applicationDTO`, after `OpportunisticMetricsEnabled: app.OpportunisticMetricsEnabled,` (`:862`):

```go
		ResponsesLiveTimingsEnabled:      app.ResponsesLiveTimingsEnabled,
```

This mapper is the single one for every application read path — `ListApplications` (`:311`), `GetApplication` (`:505`), and the returns of both writes (`:493`, `:779`) — and it has no reflection guard on this side of the portal, which is why Step 1's test 8 exists.

- [ ] **Step 8: Run the new tests and watch them pass.**

```bash
cd /Users/jlor08/Developer/codex/op-ai-gateway/.worktrees/responses-live-timings/gateway/backend
go test ./internal/portal/ -run 'ResponsesLiveTimings' -count=1 -v
go test ./internal/portal/ -count=1
```
The whole package must be green — in particular `TestCreateApplicationEndpointModeDefaultsToPassthrough`, `TestUpdateApplicationEndpointModes` and `TestUpdateApplicationPartialUpdatePreservesOtherFields`, none of which may be disturbed by a new type-conditional default.

- [ ] **Step 9: Mutations.**

(a) Delete the `ResponsesLiveTimingsEnabled:` line from `applicationDTO` — test 8 (and several others) must fail; this is the silent-zero-value defect, demonstrated. Restore.
(b) Change `CreateApplicationRequest`'s field from `*bool` to `bool` and adjust the create block to read it directly — test 2 (explicit false) and test 1 (llama_cpp default on) can no longer both pass. Restore.
(c) Delete the retype reset — tests 6 fail and only those. Restore.
(d) Move the retype reset **above** the `if req.ResponsesLiveTimingsEnabled != nil` arm — test 6's second leg fails. Restore.
Record which mutation broke which test.

- [ ] **Step 10: Gates and commit.**

`fmt --diff` (a new struct field re-aligns the whole tag column — expect gofumpt to have an opinion), `run`, `go test ./... -count=1`. Commit body: the pointer rationale, the invariant in one sentence, where the reset sits and why it reads `app.Type`, and the four mutations.

---

### Task 7: The portal's runtime-spec surface, the spread, and the wire docs

**Files:**
- Modify: `gateway/backend/internal/portal/service_runtime.go` (`RuntimeSpecDTO` field after `:392`; `PutRuntimeSpecRequest` field after `:459`; the kind-dependent default computed after the `existing, hadExisting` read at `:711`; the `routing.RuntimeSpec` literal after `:801`; the `runtimeSpecDTO` mapper line after `:1180`)
- Modify: `gateway/backend/internal/portal/service_runtime_benchmark.go` (`putRequestFromDTO`, after `MessagesMode: dto.MessagesMode,` at `:51`)
- Modify: `gateway/backend/internal/portal/service_runtime_benchmark_test.go` (the fully-populated DTO at `:137-175` and the `want` literal at `:174-201`)
- Modify: `gateway/backend/internal/portal/service_runtime_test.go` (new tests)
- Modify: `docs/architecture/reference/api-surface.md` (`:494-538`), `docs/architecture/cross-cutting/agent-runtime-manager.md` (`:3738-3748`)

**Interfaces:**
- Consumes: `routing.LiveTimingsCapableKind(kind string) bool` (Task 5); `routing.RuntimeSpec.ResponsesLiveTimingsEnabled bool` (Task 3); `routing.EffectiveRuntimeSpecType(spec RuntimeSpec) RuntimeSpecType` (`internal/routing/runtime_spec_type.go:57`); `binary := strings.TrimSpace(req.Binary)` (`service_runtime.go:600`); `specType := strings.TrimSpace(req.Type)` (`:631`); `existing, hadExisting, err := s.routes.RuntimeSpecByMapping(ctx, mapping.ID)` (`:711`).
- Produces: JSON key `responses_live_timings_enabled` — `bool` on `RuntimeSpecDTO`, `*bool` on `PutRuntimeSpecRequest`; carried across by `putRequestFromDTO`.

**The rule, stated once.** `PutRuntimeSpec` is a full-document upsert ("create on first write, full-document replace thereafter", `:534-535`), so `!hadExisting` is what "create" means here and the kind is the spec's own `routing.EffectiveRuntimeSpecType(routing.RuntimeSpec{Type: specType, Binary: binary})` — the explicit type when set, else detected from the binary's basename, which is the same resolver the inference path consults through `Target.LiveProgressSpecType`. So:
- absent + no existing spec → `routing.LiveTimingsCapableKind(string(effectiveKind))`;
- absent + an existing spec → the existing spec's stored value (a later save must not silently re-enable a flag the operator turned off, and must not silently clear one either);
- present → the caller's value;
- then, either way, forced to `false` when the effective kind is not capable. On a full-document replace every PUT restates the `Type`/`Binary`, so "a type change away from a capable kind" and "this PUT's kind is not capable" are the *same* predicate here — there is no separate retype signal to gate on, and this is the only faithful reading of decision (e) on an upsert.
- The spec never inherits the parent application's value. That is `PutRuntimeSpec`'s standing "no backend inheritance" contract (`:648-652`, pinned by `TestPutRuntimeSpecDoesNotInheritAppModes`), and it matters doubly here: a `server_agent` parent's own flag is necessarily `false`, so inheriting would default every managed spec off.

- [ ] **Step 1: Write the failing tests.**

In `service_runtime_test.go`, following the shape of the existing `PutRuntimeSpec` tests:

1. `TestPutRuntimeSpecResponsesLiveTimingsDefaultsFromTheSpecsOwnKind` — first write (no existing spec), key absent, three cases: `Type: string(routing.RuntimeSpecTypeLlamaCpp)` → `true`; `Type: ""` with `Binary: "/usr/bin/llama-server"` (so `DetectRuntimeSpecType` resolves `llama_cpp`) → `true`; `Type: string(routing.RuntimeSpecTypeOllama)` → `false`.
2. `TestPutRuntimeSpecResponsesLiveTimingsDoesNotInheritTheParentApplication` — a `server_agent` parent application whose own flag is `true` (write it through the routes store directly), then a first spec write with `Type: string(routing.RuntimeSpecTypeOllama)` and the key absent → `false`. Names the contract in its comment.
3. `TestPutRuntimeSpecResponsesLiveTimingsPreservesAnExistingValueWhenAbsent` — first write with `boolPtr(false)` on an `llama_cpp` spec; second write with the key absent → still `false`. The defect this pins: the kind-dependent default firing on every save would re-enable a flag the operator turned off.
4. `TestPutRuntimeSpecResponsesLiveTimingsHonoursAnExplicitValue` — `boolPtr(true)` then `boolPtr(false)` on an `llama_cpp` spec; each is stored and echoed.
5. `TestPutRuntimeSpecResponsesLiveTimingsRefusesAnIncapableKind` — an existing `llama_cpp` spec with the flag on, then a PUT changing `Type` to `string(routing.RuntimeSpecTypeOllama)` (key absent, and again with `boolPtr(true)`) → `false` both times, no error.
6. `TestRuntimeSpecDTOCarriesResponsesLiveTimings` — upsert a spec with the flag on through the routes store, read it through `GetRuntimeSpec`; assert `true`. Guards the hand-written `runtimeSpecDTO` mapper.

Also extend `TestPutRequestFromDTOCoversEveryWritableField` in `service_runtime_benchmark_test.go`: add `ResponsesLiveTimingsEnabled: true,` to the fully-populated `dto` literal and `ResponsesLiveTimingsEnabled: &dto.ResponsesLiveTimingsEnabled,` to the `want` literal. `reflect.DeepEqual` compares pointers by pointee, so a pointer to an equal value passes; a `nil` from a forgotten spread line does not.

- [ ] **Step 2: Run them and watch them fail.**

```bash
cd /Users/jlor08/Developer/codex/op-ai-gateway/.worktrees/responses-live-timings/gateway/backend
go test ./internal/portal/ -run 'RuntimeSpecResponsesLiveTimings|RuntimeSpecDTOCarriesResponsesLiveTimings|PutRequestFromDTO' -count=1
```
Expected: compile failures on the unknown DTO/request fields.

- [ ] **Step 3: The DTO field.**

In `RuntimeSpecDTO`, after `MessagesMode  string \`json:"messages_mode"\`` (`:392`):

```go
	// ResponsesLiveTimingsEnabled is this spec's own copy of the live-timings
	// opt-in (migration 80) -- stored explicitly on the spec rather than
	// inherited from the parent server_agent application, exactly like the trio
	// above, and for a server_agent model the RESOLVED spec's copy is the one
	// the request path reads. Always the STORED value: a PUT that asks for it on
	// a spec kind that cannot honour it is stored as false and read back as
	// false.
	ResponsesLiveTimingsEnabled bool `json:"responses_live_timings_enabled"`
```

- [ ] **Step 4: The request field, a pointer.**

In `PutRuntimeSpecRequest`, after `MessagesMode  string \`json:"messages_mode"\`` (`:459`):

```go
	// ResponsesLiveTimingsEnabled: a POINTER inside an otherwise
	// apply-verbatim full-document request, the same exception APIToken above
	// already makes and for a related reason -- nil must mean "no opinion", so
	// that a FIRST write can get the kind-dependent default (ON for a
	// llama_cpp/vllm spec) while a later save of an existing spec keeps
	// whatever the operator last stored. A plain bool would arrive as false on
	// every full-document PUT and the default could never fire.
	ResponsesLiveTimingsEnabled *bool `json:"responses_live_timings_enabled,omitempty"`
```

- [ ] **Step 5: Resolve the value in `putRuntimeSpec`.**

The `existing, hadExisting, err := s.routes.RuntimeSpecByMapping(ctx, mapping.ID)` read is at `:711`; `specType` (`:631`) and `binary` (`:600`) are already in scope. Immediately after the `measuredByIndex` block that consumes `hadExisting` — anywhere after `:711` and before the `spec := routing.RuntimeSpec{…}` literal at `:776` — add:

```go
	// The live-timings opt-in, resolved from THIS spec's own kind -- the
	// explicit Type when set, else detected from the binary's basename. That is
	// the same routing.EffectiveRuntimeSpecType the inference path consults
	// through Target.LiveProgressSpecType, so the portal and the gateway cannot
	// disagree about what actually serves.
	//
	// !hadExisting is what "create" means on a full-document upsert: a later
	// save of an existing spec that omits the key keeps the stored value, so
	// re-saving an llama.cpp spec never re-enables a flag the operator turned
	// off -- nor clears one they turned on.
	effectiveSpecKind := routing.EffectiveRuntimeSpecType(routing.RuntimeSpec{Type: specType, Binary: binary})
	liveTimings := routing.LiveTimingsCapableKind(string(effectiveSpecKind))
	if hadExisting {
		liveTimings = existing.ResponsesLiveTimingsEnabled
	}
	if req.ResponsesLiveTimingsEnabled != nil {
		liveTimings = *req.ResponsesLiveTimingsEnabled
	}
	// Decision (e) on an upsert: every PUT restates Type/Binary, so "a type
	// change away from a capable kind" and "this document's kind is not
	// capable" are the same predicate, and there is no separate retype signal
	// to gate on. Stored false, echoed false, never an error.
	if !routing.LiveTimingsCapableKind(string(effectiveSpecKind)) {
		liveTimings = false
	}
```

Then in the `routing.RuntimeSpec` literal, after `MessagesMode: msgMode,` (`:801`):

```go
		ResponsesLiveTimingsEnabled: liveTimings,
```

- [ ] **Step 6: The DTO mapper.**

In `runtimeSpecDTO`, after `MessagesMode: string(spec.MessagesMode),` (`:1180`):

```go
		ResponsesLiveTimingsEnabled: spec.ResponsesLiveTimingsEnabled,
```

`GetRuntimeSpec`'s not-yet-configured DTO (`:508-530`) needs **no** entry: it sets neither `ResponsesMode` nor `MessagesMode` either, and the new boolean's zero value there is the honest answer for a spec that does not exist yet.

- [ ] **Step 7: `putRequestFromDTO` — the spread.**

In `service_runtime_benchmark.go`, after `MessagesMode: dto.MessagesMode,` (`:51`):

```go
		// A *bool on the request against a bool on the DTO, so this is a
		// conversion rather than a copy -- the one line in this spread that is
		// not a straight assignment. A loaded document always HAS an opinion,
		// so the pointer is never nil here: the benchmark's deferred restore
		// must put back exactly what it read, not fall through to the
		// create-time default.
		ResponsesLiveTimingsEnabled: &liveTimings,
```

and, at the top of the function body, before the `return`:

```go
	liveTimings := dto.ResponsesLiveTimingsEnabled
```

(A local rather than `&dto.ResponsesLiveTimingsEnabled`, so the returned request does not alias the caller's argument.) The function is currently a single `return` statement; adding one statement before it is fine and keeps it a pure spread.

- [ ] **Step 8: Run the tests and watch them pass.**

```bash
cd /Users/jlor08/Developer/codex/op-ai-gateway/.worktrees/responses-live-timings/gateway/backend
go test ./internal/portal/ -run 'RuntimeSpec|PutRequestFromDTO' -count=1 -v
go test ./internal/portal/ -count=1
```
`TestPutRuntimeSpecDoesNotInheritAppModes` and the VRAM-benchmark restore tests must be green.

- [ ] **Step 9: Mutations.**

(a) Delete the `ResponsesLiveTimingsEnabled: &liveTimings,` line from `putRequestFromDTO` — `TestPutRequestFromDTOCoversEveryWritableField` must fail **twice over**: once on the tag-name comparison (`:128`/`:133`) and once on the `DeepEqual`. That is the guard the applications side does not have, so prove it fires.
(b) Delete the `if hadExisting { liveTimings = existing.… }` branch — test 3 fails. Restore.
(c) Delete the incapable-kind clamp — test 5 fails. Restore.
(d) Delete the `runtimeSpecDTO` line — test 6 fails. Restore.
Record each.

- [ ] **Step 10: The wire contract in `api-surface.md`.**

Under `#### API-variant endpoint modes (responses_mode / messages_mode)` (`:494`), after the three existing "Wire notes a client must know" bullets (`:510-529`), add one bullet block for the new field. It must state: the JSON key and that it appears on `ApplicationDTO`/`CreateApplicationRequest`/`UpdateApplicationRequest` **and** `RuntimeSpecDTO`/`PutRuntimeSpecRequest`; that it is a `*bool` on all three request shapes, where **absent is not the same as false** — absent on a create (or a first spec write) gets the kind-dependent default, `true` for `llama_cpp`/`vllm` and `false` for every other kind, while an explicit `false` is a deliberate off; that absent on an update keeps the stored value; that the stored value is forced to `false` whenever the row's kind cannot honour it (an application retype away from a capable kind, or a spec PUT whose effective type is not capable), returned as the stored `false` rather than as an error; that it is **orthogonal** to `responses_mode` rather than a fourth value of it; that it is offered on the Responses side only and never for `/v1/messages`; and that it adds **no new error code** — there is no `application.live_timings_invalid` row to add to the table at `:533-538`, because a `*bool` has no invalid non-nil value.

- [ ] **Step 11: The spec-snapshot note in `agent-runtime-manager.md`.**

At `:3738-3748`, where the `api_flavors`/`responses_mode`/`messages_mode` spec snapshot is described, add one sentence: `responses_live_timings_enabled` joins that per-spec set (migration 80), and for a `server_agent` model the resolved spec's copy is what the request path reads — with the reason: the flag qualifies `responses_mode`, which for a managed model comes from the spec, so reading the parent application's copy would attach the flag to a decision the application never made. State also that no operator control for it ships in this cut.

- [ ] **Step 12: Docs and Go gates, then commit.**

```bash
cd /Users/jlor08/Developer/codex/op-ai-gateway/.worktrees/responses-live-timings
./scripts/check-docs.sh && bash ./scripts/check-docs.test.sh
cd gateway/backend && ~/go/bin/golangci-lint fmt --diff && ~/go/bin/golangci-lint run && go test ./... -count=1
```
Finally, the whole-tree confirmation that part 1 ships no reader and no frontend change:
```bash
cd /Users/jlor08/Developer/codex/op-ai-gateway/.worktrees/responses-live-timings
grep -rn 'ResponsesLiveTimingsEnabled' --include='*.go' gateway/backend/internal/gateway/ gateway/backend/internal/provider/ | grep -v '_test.go'   # must print nothing
git diff --name-only main... -- gateway/frontend/                                                                                                  # must print nothing
```
Commit body: the upsert's four-way resolution, why `!hadExisting` is what "create" means here, the no-inheritance contract, the pointer conversion in the spread and the two ways its guard fires, and the two doc files.

---

## Hazard → task map

| Hazard (from the design §4 and the site survey) | Where it is addressed |
|---|---|
| Migration chain head / append after 79, never insert | Global Constraints; Task 1 Steps 3, 8 |
| `baselineCreateStatements` frozen at v60 (and one survey entry that says otherwise) | Global Constraints; Task 1 Step 4 |
| Spec backfill must follow migration 72's join in both dialects | Task 1 Steps 1, 4 |
| PostgreSQL leg skips silently | Global Constraints; Tasks 1/2/3, the "record the counts" steps |
| `ActiveMappingsForModel` is the dangerous reader | Task 2 Steps 2, 8, 13 |
| A reordered select list is silent (applications) | Task 2 Steps 1, 13 (the fifth bit pattern + the swap mutation) |
| A reordered select list is silent (spec: two column-order consts) | Task 3 Steps 1, 6, 10(a) |
| SQLite has no bool — three spots per scan | Task 2 Steps 9, 10; Task 3 Step 7 |
| Placeholder rows counted by hand (32→33, 30→31) | Task 2 Step 6; Task 3 Step 5 |
| Spec-vs-app authority split for a `server_agent` mapping | Task 3 Step 3 (the field); Task 4 Steps 1, 4, 7 |
| Not on `routing.ModelMapping`; no `MappingCandidate` field | Global Constraints; Task 2 Interfaces |
| A plain `bool` kills the kind-dependent create default | Task 6 Steps 4, 9(b); Task 7 Steps 4, 9 |
| `putRequestFromDTO` compiles without the new field | Task 7 Steps 1, 7, 9(a) |
| Hand-written DTO mappers, no coverage test on the application side | Task 6 Steps 1(test 8), 7, 9(a); Task 7 Steps 1(test 6), 6, 9(d) |
| A `server_agent` application's create-time default can only be `false` | Task 5 Step 1 (the predicate row); Task 6 Step 1(test 1), 5; Task 7 Step 1(test 2) |
| The retype path is undefined | Task 6 Steps 1(tests 6, 7), 6; Task 7 Steps 1(test 5), 5 |
| Two hand-maintained "79 migrations" prose counts, no test | Task 1 Step 9 |
| `data-model.md` §4 heading rename breaks seven anchors | Task 1 Steps 9, 10, 11 |
| gofumpt re-aligns a struct-tag column on every field insert | Global Constraints; every task's gate step |
| A second hand-written capable-kind list would drift from the gate's | Task 5 Steps 5, 6 |
| `PutRuntimeSpecRequest` derived by TS `Omit` → a required request field | **Part 2** — declared out of scope in Global Constraints |
| Frontend and backend defaults both claiming authority (`applicationTypeDefaults.ts`) | **Part 2** — declared out of scope in Global Constraints |

## What part 1 deliberately leaves on the table

These are named so a later reader does not mistake them for oversights:

- **No operator control, no TypeScript, no i18n.** A visible toggle that does nothing would be worse than the blank cell it promises to fix.
- **No gate, no injection, no retry, no capture change.** `wantsLiveProgress` and `liveProgressMemo` are unexported in `internal/provider` and the memo hangs off `*OpenAICompatibleClient` while the gateway holds a `provider.Client`; that package-boundary decision is part 2's and is not prejudged here.
- **The non-injection prose stays as written.** `telemetry-usage-observability.md` §8.4.3 states it twice — at `:755-766` ("`timings_per_token` is READ when the client set it, and never injected", framed as one of "Two rules on this path must survive any later change") and again in the three-layer-rule passage further down the same long section, at `:1536-1550` ("Native passthrough gets neither parameter") — and `compatibility-and-inference.md` §6 states it a third time, at `:263-267` and `:370-386`. After part 1 all three are still **true**: nothing injects anything. Reversing them belongs in the commit that reverses the behaviour, together with the ADR (next number: ADR-041; ADR-030 at `09-architecture-decisions.md:306` is its closest structural precedent) if one is written. Note for part 2: `docs/architecture/reference/data-model.md` §4's anchor will by then read `#4-migration-history-80-migrations`.
- **The two vacuous "must not grow a `timings_per_token` flag" assertions** (`internal/gateway/passthrough_progress_test.go:292`, `:623`) are left exactly as they are. They pass today and will pass after part 1; part 2 owns re-documenting which one is the negative case and why its fixture is incapable, and adding a genuinely capable fixture for the positive.
- **`predicted_n`** (an exact mid-stream count off the partials) stays out of scope and an open question on #81.
