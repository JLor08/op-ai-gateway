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
- **Request shapes take a POINTER, never a plain `bool`.** Absent and false are the same value for a plain `bool`, so the kind-dependent create default would be dead code for any client that sends the key — and the portal's own forms always send the whole body. The house precedent, with its own justification, is `ProxyExcluded *bool \`json:"proxy_excluded,omitempty"\`` at `internal/portal/service_applications.go:259-265`. `*bool` on `CreateApplicationRequest`, on `UpdateApplicationRequest`, and on `PutRuntimeSpecRequest`. A plain `bool` on the two **DTOs** (a DTO always states the stored value). The pointer carries a **second** load now: it is what separates an ASSERTION from a NON-MENTION, which is what decision (e) rests on — an explicit `true` on an incapable resulting type is refused, an absent field on one is a clear. With a plain `bool` the two are the same request and neither rule can be written. `UpdateApplicationRequest.Type *string` carries a third: whether the request NAMED the type is what selects the refusal's status (the next bullet), and it is read as `req.Type != nil`, never by comparing the value to the stored one.
- **Decision (e) is REJECTION, not normalisation, and the rejection has TWO statuses** (settled 2026-09-11, superseding the earlier "a type change clears it" and the single-400 wording that replaced it). A write may not store `true` for a row whose resulting kind cannot honour it, and it may not *quietly rewrite* such a request either. An explicit `responses_live_timings_enabled: true` against an incapable **resulting** type is refused, naming the kind, with the status split by where that type came from:
  - **400** when the REQUEST ITSELF supplies the incapable type alongside the `true` — the body is internally contradictory and can be judged without consulting stored state at all;
  - **409** when the incapable type comes from STORED STATE and the request does not change it — the request is well-formed and collides with the target's own state.

  An **absent** field is never refused on either status — create takes the kind-dependent default, update **clears** a stored `true` when the resulting type is incapable. A 200 that stores something other than what it was asked to store is a write that lies about its result; a 400 on a request that is well-formed is a second, smaller lie, and this tree already draws that line — `ErrApplicationProxyExcludedPortConflict` carries "Conflict" in its own name, and it plus the two proxy sentinels beside it answer 409 for exactly "the request SHAPE is fine, it conflicts with the target's own state", the reading `ErrServerManagedRuntimeOnly` established for the group (`internal/gateway/portal_application_endpoints.go:203-206`, `:220-221`). What tipped it: no API client consumes this field yet, so the correct version costs one extra sentinel and one extra error-row **today**, while moving a status code later is a breaking change for something a client may by then branch on.

  **409 reaches exactly ONE shape** — an application PATCH that asserts `true` and does **not** send `type`. Create has no prior state, so every rejection in Task 6's create path is a 400; a runtime-spec PUT is a full document that always carries its own resulting type, so every rejection in Task 7 is a 400 and Task 7 adds no 409 at all. The whole implementation addition is one branch, `req.Type != nil`. The invariant all of it buys: a stored `true` always means "this will inject once part 2's verdict allows", so there is no "on but inert" state for a later operator control to have to explain. Tasks 6 and 7 own this; Tasks 1–5 are unaffected (the column, the structs and the predicate say nothing about request shapes).
- **`putRequestFromDTO` is a hand-written spread that compiles without the new field** (`internal/portal/service_runtime_benchmark.go:30-58`). Its own doc (`:16-22`) records that this exact class of defect was already paid for once: a spec write assembled from a hand-picked field list quietly reset the operator's binary path, args, timeouts and GPU rows while a narrow test passed. `TestPutRequestFromDTOCoversEveryWritableField` (`service_runtime_benchmark_test.go:78-204`) catches it by comparing JSON **tag names** in both directions — so the `bool` DTO field and the `*bool` request field pass that half, and the `reflect.DeepEqual` half then needs the pointer conversion to be right.
- **The DTO mappers are hand-written and the application side has no coverage test.** `applicationDTO` (`service_applications.go:836-870`) and `runtimeSpecDTO` (`service_runtime.go:1158-1194`) are literals; a field added to a DTO but not to its mapper is the Go zero value on the wire, with no compile error and a 200 OK. The spec side is guarded by the reflection test above; the application side must be walked by hand and pinned by an explicit assertion.
- **One name in every layer.** Column `responses_live_timings_enabled`; Go field `ResponsesLiveTimingsEnabled` on `routing.Application`, `routing.RuntimeSpec`, `routing.Target`, `ApplicationDTO`, `CreateApplicationRequest`, `UpdateApplicationRequest`, `RuntimeSpecDTO`, `PutRuntimeSpecRequest`; JSON tag `responses_live_timings_enabled`. `Target.OpportunisticMetrics` drops its `Enabled` suffix, but uniformity is worth more here than that one precedent — do not shorten the name in any layer.
- **Not on `routing.ModelMapping`.** Its doc block (`internal/routing/store.go:607-649`) is an explicit prohibition — "A CAPABILITY IS NOT A FIELD HERE" — and migration 79 dropped the eleven columns that used to be. It is also not a `model_mapping_capabilities` row: those are observed/probed verdicts with ranked provenance, and this is an operator-owned setting. `MappingCandidate` needs no field either — the boolean rides inside the embedded `Application` value.
- **SQLite has no boolean.** Every integer-boolean column needs THREE edits per scan function: an `int64` local, a `&local` destination in positional order, and a `!= 0` conversion afterwards. Passing `&app.SomeBool` compiles and fails at runtime on one driver.
- **No gate, no injection, no retry, no frontend.** Nothing in part 1 may read the boolean for a routing or request decision. No file under `gateway/frontend/` is touched: no operator control, no TypeScript type, no i18n key. The survey's TypeScript hazards — `PutRuntimeSpecRequest` being derived by `Omit<RuntimeSpec, …>` (`gateway/frontend/src/api/runtime.ts:132-149`), which would silently make a new `RuntimeSpec` field a *required* request field, and `applicationTypeDefaults.ts:14-23` claiming per-type authority that would kill the backend's kind-dependent default — **belong to part 2**, together with the control itself, and so does the new 400's consequence for the portal form (spelled out under "What part 1 deliberately leaves on the table"). They are not oversights in this plan.
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
- `gateway/backend/internal/portal/service_applications.go` — **two** new sentinels, `ErrApplicationResponsesLiveTimingsUnsupported` (400: the request supplied the incapable type) and `ErrApplicationResponsesLiveTimingsConflict` (409: the incapable type is the stored one and the request does not touch it); the field on `ApplicationDTO` (`bool`), on `CreateApplicationRequest` (`*bool`) and on `UpdateApplicationRequest` (`*bool`); the kind-dependent create default; the explicit-`true`-on-an-incapable-resulting-type refusal on both write paths, with the update path choosing between the two sentinels on `req.Type != nil`; the update arm and the absent-field clear; the `applicationDTO` mapper line.
- `gateway/backend/internal/portal/service_runtime.go` — the new sentinel `ErrRuntimeSpecResponsesLiveTimingsUnsupported`; the field on `RuntimeSpecDTO` (`bool`) and `PutRuntimeSpecRequest` (`*bool`); the `!hadExisting` kind-dependent default; the same refusal, over the spec's *effective* kind; the absent-field clear; the `runtimeSpecDTO` mapper line.
- `gateway/backend/internal/portal/service_runtime_benchmark.go` — `putRequestFromDTO`'s spread gains the pointer conversion.
- `gateway/backend/internal/gateway/portal_application_endpoints.go` — **two** `errRow`s, one per application sentinel: 400 for `…Unsupported`, 409 for `…Conflict`, both with `msgFn` so the message names the offending type. `errRow.status` is a plain `int` (`internal/gateway/error_map.go:21`) written verbatim by `writeMappedError` (`:75`), and `msgFn` (`:24`, honoured at `:72-74`) is independent of it, so the table expresses 409-with-a-dynamic-message with no change to the mechanism. Without a row a sentinel falls through to the 500 `application.request_failed` fallback — the pre-existing defect two rows in that same table already record.
- `gateway/backend/internal/gateway/portal_runtime_endpoints.go` — one row for the single runtime-spec sentinel in `portalRuntimeSpecErrRows`, 400. No 409 on this side: a spec PUT is a full document and always carries the type it is judged against.

Tests:

- `gateway/backend/internal/store/migration80_responses_live_timings_test.go` (new) — the column's presence and declared type on both tables and both dialects, the backfill join, replay idempotency.
- `gateway/backend/internal/store/conformance_test.go` — a new `TestConformanceApplicationResponsesLiveTimings` (create/read/update/routing-join, both dialects).
- `gateway/backend/internal/store/application_column_parity_test.go` — the `[5][3]bool` fixture, the seeded field, the `names` list, the arithmetic comment.
- `gateway/backend/internal/store/routing_store_conformance_test.go` — the spec-side round-trip inside `TestRoutingStoreRuntimeSpecs` (memory + sqlite + postgres).
- `gateway/backend/internal/routing/resolver_live_timings_test.go` (new) — `targetFrom`'s precedence, all four cases.
- `gateway/backend/internal/routing/live_timings_test.go` (new) — the capable-kind predicate over every provider constant.
- `gateway/backend/internal/provider/live_progress_kind_parity_test.go` (new) — the drift tripwire: `liveProgressUpstreams`' keys and `routing.LiveTimingsCapableKind` must agree. Test-only; **no** production change in `internal/provider`.
- `gateway/backend/internal/portal/service_applications_test.go` — create default per kind, explicit pointer both ways, absent-is-not-false, PATCH keep-if-nil, the retype clear, the three refusals (create, retype-plus-assert, assert-against-stored-state) each with nothing stored and each asserting WHICH of the two sentinels came back, DTO echo.
- `gateway/backend/internal/gateway/portal_application_live_timings_test.go` (new) — the refusals' WIRE contract (status + code + the type named in the message) on POST and on both PATCH shapes, plus one subtest pinning that the two PATCH shapes do not share a status. Modelled on `portal_application_endpoint_mode_test.go`.
- `gateway/backend/internal/portal/service_runtime_test.go` — spec create default per effective kind, no-inheritance, update-preserves, absent-is-not-false, the incapable-kind refusal, the absent-field clear, DTO echo.
- `gateway/backend/internal/gateway/portal_runtime_endpoints_test.go` — the spec refusal's wire contract, beside the existing `…PutBadTypeReturns400`.
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

### Task 6: The portal's application surface — accept, default, refuse, clear, return

**Files:**
- Modify: `gateway/backend/internal/portal/service_applications.go` (the `"fmt"` import; the **two** new sentinels after `ErrApplicationProxyEntryScheme` at `:76`; `ApplicationDTO` field after `:205`; `CreateApplicationRequest` field after `:255`; `UpdateApplicationRequest` field after `:297`; the create refusal + default after the `messagesMode` block at `:381`, assigned in the literal after `:471`; the update refusal in the validate-before-mutate block after `:609`; the update arm + clear after `:733`; the `applicationDTO` mapper line after `:862`)
- Modify: `gateway/backend/internal/gateway/portal_application_endpoints.go` (**two** `errRow`s in `portalApplicationErrRows`, after `:223`)
- Modify: `gateway/backend/internal/portal/service_applications_test.go` (new tests; leave `TestApplicationBenchmarkModesCreatePatchAndValidation` at `:353` and `TestCreateApplicationEndpointModeDefaultsToPassthrough` at `:546` untouched and passing)
- Create: `gateway/backend/internal/gateway/portal_application_live_timings_test.go`

**Interfaces:**
- Consumes: `routing.LiveTimingsCapableKind(kind string) bool` (Task 5, `internal/routing/live_timings.go:44`, a case- and whitespace-sensitive map hit over exactly `"llama_cpp"` and `"vllm"`); `routing.Application.ResponsesLiveTimingsEnabled bool` (Task 2); `normalizeApplicationType(raw string) (string, error)` (`service_applications.go:1000-1017`, the closed six-value set — `ollama`, `vllm`, `llama_cpp`, `llama_swap`, `litellm`, `server_agent` — resolved at `:347` before every other normalization). Note what that set does **not** contain: **`mock` is not an accepted application type**, so `routing.ProviderMock` is unreachable from this path — a body with `"type":"mock"` dies on `ErrApplicationTypeInvalid` at `:347`, long before any live-timings check. Task 5's predicate answers `false` for it, correctly, but no message or test in THIS task may imply an operator can send it.
- Produces: JSON key `responses_live_timings_enabled` — `bool` on `ApplicationDTO`, `*bool` on `CreateApplicationRequest` and on `UpdateApplicationRequest`; and **two new error sentinels**:
  - `portal.ErrApplicationResponsesLiveTimingsUnsupported` (`errors.New("application.responses_live_timings_unsupported")`) → HTTP **400**, for a request that supplied the incapable type itself;
  - `portal.ErrApplicationResponsesLiveTimingsConflict` (`errors.New("application.responses_live_timings_conflict")`) → HTTP **409**, for a request that asserts `true` against a type it did not send and therefore did not change.

  Both carry a message naming the offending type. Two sentinels rather than one, because **one validation computes both outcomes** and the HTTP layer maps by sentinel identity (`errors.Is` against `errRow.err`, `internal/gateway/error_map.go:68-78`) and has nothing else to branch on.

**The rule this task implements, stated once.** `responses_live_timings_enabled` is stored `true` only on a row whose upstream kind is live-timings capable — and a request that *asks* for `true` on a kind that is not is **refused**, never rewritten. The pointer is what makes that possible: it separates an **assertion** from a **non-mention**, and the two get opposite treatment.

- **Create**: absent → `routing.LiveTimingsCapableKind(appType)` (decision (d): a newly created llama.cpp or vLLM application gets it on); explicit `false` → honoured on every kind; explicit `true` on a kind that is not capable → **400** (`…Unsupported`), naming the type, with nothing persisted. **Create is 400 and only ever 400**: `CreateApplicationRequest.Type` is a plain `string` (`:230`), so the type always comes from the request, and there is no prior state for anything to conflict with. Do not add a stored-state branch to the create path — there is nothing for it to read.
- **Update**: the *resulting* type is the authority — `appType` when this PATCH retypes, the stored `app.Type` otherwise. An explicit `true` against an incapable resulting type is refused, and the **status depends on where that type came from**:
  - `req.Type != nil` → the request named the type, so the body is self-contradictory on its own terms: **400** (`…Unsupported`). This is the retype-and-assert-in-one-breath body, judged on the new type, not the old one.
  - `req.Type == nil` → the incapable type is the one already stored, and this request does not change it: the body is well-formed and collides with the application's own state: **409** (`…Conflict`).

  Absent flag → keep the stored value, except that an incapable resulting type **clears** it.
- The status branch reads **`req.Type != nil`, never a value comparison.** A PATCH that restates the same incapable type it already had still *supplied* that type, so it is the 400 shape; deriving the status from `*req.Type != app.Type` instead would make it depend on whether the operator's form happened to change a field the client cannot see — and would put the portal's own saves in the 409 bucket, since `ApplicationSection.tsx`'s `buildBody()` restates `type` on every save (`gateway/frontend/src/components/ApplicationSection.tsx:391`).
- Why the split at all: a 400 on a well-formed request is factually wrong, and this table already draws the line — `ErrServerManagedRuntimeOnly` answers 409 for "the request shape is fine, it is simply refused given the server's current state" (`internal/gateway/portal_application_endpoints.go:203-206`), and the three proxy sentinels for "the request SHAPE is fine, it conflicts with the target's own state" (`:220-221`). It costs one extra sentinel and one extra error-row now; it would be a breaking change to any client branching on the status later.
- Why a clear is legitimate where a silent rewrite is not: a retype that says nothing about the flag has asserted nothing, so clearing overrides nothing the operator said. An explicit `true` that cannot hold *is* an assertion, and answering 200 while storing `false` would be a write that lies about its result — the argument `ErrApplicationProxyExcludedPortConflict`'s own doc already makes at `:64-69` ("silently zeroing what the caller asked for in the same breath would be a lie").
- The **clear** is gated on the resulting **type**, not on `req.Type != nil`, deliberately: the invariant is a property of the resolved row, not of the request's shape. That is the same reasoning `applyProxyExclusion`'s RULE 4 spells out at `:1100-1125`, where a request-shape-only check was found to be exactly the hole through which the bad state arrived. Note that the two rules read the request differently on purpose, and this is the one place in the task where that is easy to get wrong: **whether to refuse** is a property of the row (the resulting type), **which status to refuse with** is a property of the request (did it name the type), and **whether to clear** is again a property of the row.
- The invariant obtained, in one sentence: **a stored `true` always means the row's kind can honour it.** No "on but inert" state exists, which is what lets part 2 ship a switch with no indicator explaining why it is doing nothing.

**An option this task MAY take, not a requirement.** Task 5 shipped its capable-kind set unexported, with `TestLiveTimingsCapableKindsSizeIsPinned` (`internal/routing/live_timings_test.go:116-124`) as a deliberate stand-in: a size pin forces a *look* at `internal/provider`'s `liveProgressUpstreams` when the set changes, but it proves no agreement, and both that test's doc (`:102-115`) and the provider-side tripwire's (`internal/provider/live_progress_kind_parity_test.go:63-72`) record why it stopped there — closing the gap needed "an exported accessor for this set, i.e. production API whose only caller is a test". That objection expires with this task: Task 6 adds the **first real production caller** of the predicate. So if Step 1's tests end up enumerating kinds anyway, exporting the set from `routing` (e.g. a `LiveTimingsCapableKinds() []string` beside the predicate) would have a production caller and a test caller, and `internal/provider`'s `capable ⇒ gate` direction could range over the real set instead of a hand-listed enumeration — which would let the size pin be deleted rather than maintained. Do it or do not; if not, change nothing about Task 5, and do not weaken the size pin.

- [ ] **Step 1: Write the failing service tests.**

In `service_applications_test.go`, add these, using the fixture the neighbouring application tests use — `svc, routeStore := newServerTestService(t, now)` plus `server := createTestServer(t, svc, "S", "s.example.test")` and `ownerToken()` (they run on `routing.NewMemoryStore()`, so no DSN is needed here):

1. `TestCreateApplicationResponsesLiveTimingsDefaultsByUpstreamKind` — create with the key **absent** for each of the six types `normalizeApplicationType` accepts; assert the returned DTO's `ResponsesLiveTimingsEnabled` is `true` for `routing.ProviderLlamaCPP` and `routing.ProviderVLLM` and `false` for `routing.ProviderOllama`, `routing.ProviderLlamaSwap`, `routing.ProviderLiteLLM` and `routing.ProviderServerAgent`. Two fixture facts the service enforces and the table must respect: at most **one** `server_agent` application per server (`serverAgentApplicationExistsOnServer`, called from the create path at `service_applications.go:427-434`, backed by `idx_applications_single_server_agent` from migration 68 at `migrate.go:3149`), and a server with `ManagedRuntimeOnly` set refuses every other type (`:344`) — so seed an ordinary server and put the `server_agent` case on a server of its own, or run each case against a fresh server. The comment must say why `server_agent` is `false` and cannot be anything else: at create time a `server_agent` application has no binary, no spec type and no mappings — specs are per-mapping and created later by `PutRuntimeSpec` — so the real per-kind default for a managed runtime is applied on the spec path (Task 7), not here. **Six rows, not seven**: there is no `routing.ProviderMock` row, because `normalizeApplicationType` does not accept `mock` — such a row would fail on `ErrApplicationTypeInvalid` before reaching anything this task added, and would prove nothing about the default. (`mock`'s `false` is pinned where it is reachable, in Task 5's predicate table.)
2. `TestCreateApplicationResponsesLiveTimingsAbsentIsNotFalse` — create a `llama_cpp` application with the key **absent** and assert `true`; create a second `llama_cpp` application (different port) with `ResponsesLiveTimingsEnabled: boolPtr(false)` and assert `false`; reload both through `GetApplication` to assert the values were **stored**, not just echoed. This is the pointer's whole purpose in one test: with a plain `bool` these are the same request and one of the two assertions must fail. Say that in the doc comment.
3. `TestCreateApplicationResponsesLiveTimingsHonoursAnExplicitFalseOnAnIncapableKind` — create a `litellm` application with `boolPtr(false)`; assert no error and a stored `false`. An explicit `false` is never refused on any kind: it asks for nothing the kind cannot do.
4. `TestCreateApplicationResponsesLiveTimingsRejectsTrueOnAnIncapableKind` — create a `litellm` application with `boolPtr(true)`. Assert four things: `errors.Is(err, ErrApplicationResponsesLiveTimingsUnsupported)`; **`!errors.Is(err, ErrApplicationResponsesLiveTimingsConflict)`**, because the create path has no prior state to conflict with and must never reach for the 409 sentinel; `strings.Contains(err.Error(), routing.ProviderLiteLLM)` (the message must name the kind, which is the point of refusing rather than rewriting); and that **nothing was stored** — `svc.ListApplications(ctx, ownerToken(), server.ID)` reports zero rows for that server. The last assertion is the one that distinguishes a refusal from a rewrite, so it is not optional. The doc comment must say that **create is always the 400 sentinel**: `CreateApplicationRequest.Type` is a plain `string`, so the type is always the request's own, on every create, for every kind.
5. `TestUpdateApplicationResponsesLiveTimingsKeepsIfNil` — create `llama_cpp` with the flag on; PATCH with only `Port` set; assert the flag survives and `Port` changed. (The house keep-if-nil discipline, mirroring `TestUpdateApplicationPartialUpdatePreservesOtherFields` at `:1067`.)
6. `TestUpdateApplicationResponsesLiveTimingsFlipsBothWays` — PATCH `boolPtr(false)` then `boolPtr(true)` on a `llama_cpp` application; assert each is stored.
7. `TestUpdateApplicationRetypeAwayFromACapableKindClearsResponsesLiveTimings` — create `llama_cpp` with the flag on, then PATCH `Type: strPtr(routing.ProviderLiteLLM)` **and nothing else**. Assert the reload reports `ResponsesLiveTimingsEnabled == false` **and** `Type == routing.ProviderLiteLLM`, and assert it was `true` before the PATCH — a clearing test must show the stored value actually *changed*, otherwise it passes against a field that was never set.
8. `TestUpdateApplicationRetypeAndAssertTrueInOneBodyIsRejected` — the same `llama_cpp` application with the flag on, PATCHed with `Type: strPtr(routing.ProviderLiteLLM)` **together with** `ResponsesLiveTimingsEnabled: boolPtr(true)`. Assert `errors.Is(err, ErrApplicationResponsesLiveTimingsUnsupported)` — the **400** sentinel, because the request supplied the offending type itself — and `!errors.Is(err, ErrApplicationResponsesLiveTimingsConflict)`; that the message names `litellm` (the **new** type — the refusal is judged on the resulting type, not the stored one); and that the reload still reports `Type == routing.ProviderLlamaCPP` **and** the flag still `true`: a refused PATCH writes nothing at all, not even the type.
9. `TestUpdateApplicationResponsesLiveTimingsRejectsTrueOnAStoredIncapableKind` — create `ollama` (so the flag is off), then PATCH `ResponsesLiveTimingsEnabled: boolPtr(true)` with **no** `Type`. Assert `errors.Is(err, ErrApplicationResponsesLiveTimingsConflict)` — the **409** sentinel, because the offending type is the stored one and this request does not change it — and `!errors.Is(err, ErrApplicationResponsesLiveTimingsUnsupported)`; that the message names `ollama`; and that the reload still reports the flag `false`, so nothing was written. This is the leg that proves two separate things at once, and the doc comment must name both: that the rule is phrased over the resulting type rather than over a retype (there is no transition here at all), and that the two refusal shapes return **different** sentinels — the single-sentinel implementation is the likely slip, and this pair of `errors.Is` assertions with test 8's is what catches it inside the service, before the wire test sees it.
10. `TestUpdateApplicationResponsesLiveTimingsRestatingTheSameIncapableTypeIsTheRequestsOwnAssertion` — create `ollama`, then PATCH `Type: strPtr(routing.ProviderOllama)` (the **same** type it already has) **together with** `ResponsesLiveTimingsEnabled: boolPtr(true)`. Assert the **`…Unsupported`** sentinel, not `…Conflict`. Nothing changed about the row, yet the request still supplied the type it is being judged against, so it is the 400 shape. This is the test that fails if the status branch is written as `req.Type != nil && *req.Type != app.Type` instead of `req.Type != nil` — and nothing else in this list would notice, because every other leg either omits `type` or changes it.
11. `TestUpdateApplicationRetypeToACapableKindDoesNotSwitchItOn` — create `ollama` (flag off), PATCH `Type: strPtr(routing.ProviderLlamaCPP)`; assert the flag is still `false`. Decision (d) scopes the `1` to CREATE; a retype must not silently switch a feature on.
12. `TestApplicationDTOCarriesResponsesLiveTimings` — store an application with the flag on directly through the routes store (`routeStore.CreateApplication(ctx, routing.Application{ID: "app_dto_lt", ServerID: server.ID, Type: routing.ProviderLlamaCPP, Port: 8300, Scheme: "http", APIFlavors: []string{routing.APIFlavorOpenAI}, Status: routing.ServerStatusActive, ResponsesLiveTimingsEnabled: true, CreatedAt: now, UpdatedAt: now})`, the same direct-store seeding `seedServerAgentApplication` does in `service_runtime_test.go:32-48` and for the same reason), then read it through `ListApplications` **and** `GetApplication`; assert both report `true`. This is the only guard on the hand-written `applicationDTO` mapper: a field on the DTO but not in the mapper is the Go zero value with no compile error.

The package already has the two pointer helpers these tests need — `func strPtr(s string) *string` and `func boolPtr(b bool) *bool`, both at `internal/portal/service_test.go:1345-1346`, and both already used by `TestApplicationBenchmarkModesCreatePatchAndValidation` (`:394-409`). Use them; do not add a second pair. `errors` and `strings` are already imported by this test file.

- [ ] **Step 2: Write the failing wire test.**

The service returning an error is only half the contract: a sentinel that is not in `portalApplicationErrRows` falls through to the 500 `application.request_failed` fallback, which two rows in that table already record as a defect paid for once (`portal_application_endpoints.go:212-219`). Pin the wire contract before writing either half.

Create `gateway/backend/internal/gateway/portal_application_live_timings_test.go` (`package gateway`, SPDX header as in `portal_application_endpoint_mode_test.go:1-2`, imports `net/http`, `net/http/httptest`, `strings`, `testing`), modelled on `TestPortalApplicationEndpointModeErrorsReachTheWire` (`portal_application_endpoint_mode_test.go:26-66`) — including its reuse of `newProxyExclusionTestServer` (`portal_application_proxy_excluded_test.go:14`), which POSTs a plain AI server and returns its id. Do **not** add a second server helper.

Three of the four subtests need an existing application to PATCH. **Do not write a helper for that**: `createTestApplication(t *testing.T, srv *Server, serverID string, body string) string` already exists in this package (`server_test.go:4281-4295`) and does exactly the POST-and-return-the-id dance, asserting 201 on the way. The endpoint-mode test next door inlines it by hand; reuse the helper instead — which is why the import list above has no `encoding/json` in it. The one subtest that does **not** use the helper is the create refusal, because the helper asserts 201 and that request must answer 400.

```go
// TestPortalApplicationLiveTimingsRefusalReachesTheWire pins the WIRE
// contract (status + code + the offending type in the MESSAGE) of BOTH
// live-timings refusal sentinels, across the three request shapes that can
// produce one, plus a fourth subtest pinning the two PATCH shapes apart.
//
// The two statuses are the point of the test, not an incidental detail:
//
//   - 400 application.responses_live_timings_unsupported when the REQUEST
//     supplied the incapable type -- a create (whose "type" is always the
//     request's own) and a PATCH that sends "type" alongside the flag. Such a
//     body is self-contradictory and needs no stored row to judge it.
//   - 409 application.responses_live_timings_conflict when the PATCH asserts
//     the flag and does NOT send "type" -- the request is well-formed and
//     collides with the application's own stored state. That is the
//     distinction ErrServerManagedRuntimeOnly already records in the same
//     table ("the request shape is fine, it is simply refused given the
//     server's current state", portal_application_endpoints.go:203-206).
//
// The last subtest compares the two PATCH statuses directly, because the
// likely implementation slip is ONE sentinel returned for both shapes: that
// version passes every service-level errors.Is check written loosely, and it
// passes two of the three shape subtests here as well.
//
// The message assertion is not decoration either: the whole reason this
// request is refused instead of quietly stored as false is that the caller
// asked for something this row cannot do, and a refusal that does not say
// WHICH kind cannot do it leaves them no better off than the silent rewrite
// would have.
func TestPortalApplicationLiveTimingsRefusalReachesTheWire(t *testing.T) {
	srv := NewTestServerWithGroups([]string{"gateway:use", "admin"})
	serverID := newProxyExclusionTestServer(t, srv, "live-timings.example.test")

	t.Run("create true on an incapable kind is 400", func(t *testing.T) {
		rec := httptest.NewRecorder()
		body := `{"type":"litellm","port":8100,"scheme":"http","responses_live_timings_enabled":true}`
		srv.ServeHTTP(rec, newJSONRequest(http.MethodPost, "/api/portal/servers/"+serverID+"/applications", body))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400, body = %s", rec.Code, rec.Body.String())
		}
		if code := errorBodyOf(t, rec); code != "application.responses_live_timings_unsupported" {
			t.Fatalf("error code = %q, want application.responses_live_timings_unsupported, body = %s", code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "litellm") {
			t.Fatalf("the refusal does not name the offending kind: %s", rec.Body.String())
		}
	})

	t.Run("retype plus true on the new incapable kind is 400", func(t *testing.T) {
		appID := createTestApplication(t, srv, serverID, `{"type":"llama_cpp","port":8101,"scheme":"http"}`)
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, newJSONRequest(http.MethodPatch, "/api/portal/applications/"+appID,
			`{"type":"litellm","responses_live_timings_enabled":true}`))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400, body = %s", rec.Code, rec.Body.String())
		}
		if code := errorBodyOf(t, rec); code != "application.responses_live_timings_unsupported" {
			t.Fatalf("error code = %q, want application.responses_live_timings_unsupported, body = %s", code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "litellm") {
			t.Fatalf("the refusal does not name the offending kind: %s", rec.Body.String())
		}
	})

	t.Run("true against a stored incapable kind, no type sent, is 409", func(t *testing.T) {
		appID := createTestApplication(t, srv, serverID, `{"type":"ollama","port":8102,"scheme":"http"}`)
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, newJSONRequest(http.MethodPatch, "/api/portal/applications/"+appID,
			`{"responses_live_timings_enabled":true}`))
		if rec.Code != http.StatusConflict {
			t.Fatalf("status = %d, want 409, body = %s", rec.Code, rec.Body.String())
		}
		if code := errorBodyOf(t, rec); code != "application.responses_live_timings_conflict" {
			t.Fatalf("error code = %q, want application.responses_live_timings_conflict, body = %s", code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "ollama") {
			t.Fatalf("the refusal does not name the offending kind: %s", rec.Body.String())
		}
	})

	// The anti-slip pin. One sentinel returned for both PATCH shapes gives
	// them the SAME status, and the two subtests above would then disagree
	// about which one is wrong; this one says plainly what the invariant is.
	t.Run("the two PATCH shapes do not share a status", func(t *testing.T) {
		retypeID := createTestApplication(t, srv, serverID, `{"type":"llama_cpp","port":8103,"scheme":"http"}`)
		retypeRec := httptest.NewRecorder()
		srv.ServeHTTP(retypeRec, newJSONRequest(http.MethodPatch, "/api/portal/applications/"+retypeID,
			`{"type":"litellm","responses_live_timings_enabled":true}`))

		storedID := createTestApplication(t, srv, serverID, `{"type":"ollama","port":8104,"scheme":"http"}`)
		storedRec := httptest.NewRecorder()
		srv.ServeHTTP(storedRec, newJSONRequest(http.MethodPatch, "/api/portal/applications/"+storedID,
			`{"responses_live_timings_enabled":true}`))

		if retypeRec.Code == storedRec.Code {
			t.Fatalf("both refusal shapes answered %d: the request-supplied type (400) and the stored type (409) must not collapse into one status -- check that the service returns TWO sentinels and that both have their own errRow",
				retypeRec.Code)
		}
	})
}
```

Note on the two statuses, so nobody "fixes" one into the other. The split is the settled decision: 400 where the request itself supplies the incapable type (create, and a PATCH that sends `type`), 409 where the incapable type is the stored one and the request leaves it alone. `errRow.status` is a plain `int` written verbatim by `writeMappedError`, and `msgFn` is honoured regardless of status (`internal/gateway/error_map.go:21`, `:24`, `:72-75`), so both rows are ordinary entries in the same table — nothing about the mechanism has to change to express a 409 with a dynamic message, and no row does it yet (the three `msgFn` rows in this package are all 400: `agent_runtime.go:479`, `agent_ingest.go:1774`, `:1912`).

- [ ] **Step 3: Run them and watch them fail.**

```bash
cd /Users/jlor08/Developer/codex/op-ai-gateway/.worktrees/responses-live-timings/gateway/backend
go test ./internal/portal/ -run 'ResponsesLiveTimings' -count=1
go test ./internal/gateway/ -run 'LiveTimingsRefusalReachesTheWire' -count=1
```
Expected: compile failures on the unknown DTO/request fields and on both unknown sentinels.

- [ ] **Step 4: The DTO field.**

In `ApplicationDTO`, after `OpportunisticMetricsEnabled bool \`json:"opportunistic_metrics_enabled"\`` (`:205`) — before the `ProxyListenPort` doc block:

```go
	// ResponsesLiveTimingsEnabled is the operator's opt-in to asking a capable
	// upstream for mid-stream timings on a passthrough /v1/responses stream
	// (migration 80). Set from the RAW column and always the STORED value --
	// which, given the write paths' refusal rule, is also always a value this
	// application's TYPE can honour: a create/update that asks for true on a
	// kind that cannot is refused -- 400 when the request supplied that kind,
	// 409 when it is the stored one -- and never stored as false.
	ResponsesLiveTimingsEnabled bool `json:"responses_live_timings_enabled"`
```

- [ ] **Step 5: The two request fields, both pointers.**

In `CreateApplicationRequest`, after `OpportunisticMetricsEnabled bool` (`:255`) — i.e. above the `ProxyListenPort` block:

```go
	// ResponsesLiveTimingsEnabled opts this application in to the
	// live-timings request parameter on its passthrough /v1/responses streams.
	//
	// A POINTER, like ProxyExcluded below, and for both of that field's
	// reasons at once. First, absent must be distinguishable from an explicit
	// false, because absent is what gets the kind-dependent default (ON for
	// llama_cpp and vllm) and false is a deliberate off; with a plain bool the
	// default could never fire for any client that sends the key -- which
	// includes every portal form, since they send one whole body for create
	// and update alike. Second, absent is what distinguishes a NON-MENTION
	// from an ASSERTION: an explicit true on a kind that cannot honour it is
	// refused (ErrApplicationResponsesLiveTimingsUnsupported), while an absent
	// field on such a kind simply takes the default. A plain bool collapses
	// those into one request and neither rule can be written.
	ResponsesLiveTimingsEnabled *bool `json:"responses_live_timings_enabled,omitempty"`
```

In `UpdateApplicationRequest`, after `OpportunisticMetricsEnabled *bool` (`:297`):

```go
	// ResponsesLiveTimingsEnabled: nil = keep the stored value, the house
	// sentinel -- except that a nil against a resulting type that cannot
	// honour the flag CLEARS it, and a non-nil true against such a type is
	// refused. Whether the request ALSO sends Type above decides which of the
	// two refusal sentinels comes back (400 vs 409). See UpdateApplication.
	ResponsesLiveTimingsEnabled *bool `json:"responses_live_timings_enabled,omitempty"`
```

- [ ] **Step 6: The two sentinels.**

One validation computes both refusal outcomes, and the HTTP layer maps by sentinel identity and has nothing else to branch on (`errors.Is` against `errRow.err`, `internal/gateway/error_map.go:68-78`), so the split needs two sentinels, not one with a status argument. In the `var (…)` block, after `ErrApplicationProxyEntryScheme` (`:76`) and before the blank line that precedes `CodeMappingNotFound`'s comment:

```go
	// ErrApplicationResponsesLiveTimingsUnsupported rejects an EXPLICIT
	// responses_live_timings_enabled:true whose RESULTING application type is
	// not a live-timings-capable kind (routing.LiveTimingsCapableKind), in the
	// case where the REQUEST ITSELF supplied that type: every create (whose
	// "type" is always the request's own) and any PATCH that sends "type"
	// alongside the flag. HTTP 400 -- such a body is internally contradictory
	// and can be judged without reading the stored row at all.
	//
	// The message names the offending type: the caller asserted something
	// about this row that its kind cannot carry, and storing false while
	// answering 200 would be a write that lies about its result. That is
	// ErrApplicationProxyExcludedPortConflict's argument above, applied to a
	// second field: "silently zeroing what the caller asked for in the same
	// breath would be a lie."
	//
	// An ABSENT field is never this error. It is the kind-dependent default on
	// create, and on update it is a CLEAR when the resulting type cannot
	// honour the flag -- clearing overrides nothing the operator said, because
	// a request that does not mention the field says nothing about it.
	ErrApplicationResponsesLiveTimingsUnsupported = errors.New("application.responses_live_timings_unsupported")
	// ErrApplicationResponsesLiveTimingsConflict rejects the SAME explicit
	// true when the incapable type is the one already STORED and the request
	// does not send "type" at all. HTTP 409, not 400: the request is
	// well-formed -- it would be accepted verbatim against a capable
	// application -- and it collides with this application's own state, which
	// is exactly the line ErrServerManagedRuntimeOnly and the three proxy
	// sentinels above already draw ("the request SHAPE is fine, it conflicts
	// with the target's own state", gateway/portal_application_endpoints.go:220-221).
	//
	// The distinction is drawn on req.Type != nil, NOT on whether the type
	// changed: a PATCH restating the same incapable type still supplied the
	// type it is judged against, so that is the 400 above. It reaches exactly
	// this one shape -- there is no create path to it, since a create always
	// carries its own type.
	ErrApplicationResponsesLiveTimingsConflict = errors.New("application.responses_live_timings_conflict")
```

Each sentinel's error text is its API code, the convention every sentinel in this block follows. The message detail is wrapped on at the call sites with `fmt.Errorf("%w: …", …)` — the idiom `service_system_settings.go:3013` uses for `ErrCertInvalid` — so **add `"fmt"` to this file's import block**, between `"errors"` and `"log/slog"` (the block is one alphabetically sorted list with the `op-ai-gateway/...` paths mixed in; gofumpt will reject any other position).

- [ ] **Step 7: The create path — refuse, then default.**

In `CreateApplication`, after the `messagesMode` block (`:374-381`) and before the `routing.Application` literal at `:443` — `appType` has been in scope since `:347`:

```go
	// Decision (d): a newly created application on an upstream kind whose
	// request schema tolerates the parameter starts with the opt-in ON; every
	// other kind gets the DDL default. A server_agent application is NOT such a
	// kind at this point and cannot be: it has no binary, no spec type and no
	// mappings yet -- runtime specs are per-mapping and are created later by
	// PutRuntimeSpec, which applies the per-kind default from the spec's own
	// resolved type.
	//
	// Decision (e), create half: an EXPLICIT true on a kind that cannot honour
	// it is REFUSED, not stored as false. Nothing has been persisted at this
	// point -- the first store write is s.routes.CreateApplication below -- so
	// a refused create leaves nothing behind. An explicit false is honoured on
	// every kind (it asks for nothing the kind cannot do), and an ABSENT field
	// is never refused: that is what the pointer buys, and it is why a caller
	// who says nothing gets the default rather than an error.
	//
	// ALWAYS the 400 sentinel here, never the 409 one. req.Type is a plain
	// string on this request, so the type this refusal names is always the
	// caller's own, and there is no prior state for anything to conflict with:
	// a create body that pairs an incapable type with true is contradictory on
	// its own terms. The stored-state shape exists only on UpdateApplication.
	liveTimingsCapable := routing.LiveTimingsCapableKind(appType)
	liveTimings := liveTimingsCapable
	if req.ResponsesLiveTimingsEnabled != nil {
		if *req.ResponsesLiveTimingsEnabled && !liveTimingsCapable {
			return ApplicationDTO{}, fmt.Errorf("%w: responses_live_timings_enabled cannot be true for an application of type %q",
				ErrApplicationResponsesLiveTimingsUnsupported, appType)
		}
		liveTimings = *req.ResponsesLiveTimingsEnabled
	}
```

`appType` has already been through `normalizeApplicationType`, so the `%q` can only ever print one of its six accepted values. Keep the message a statement about the type the caller sent; do **not** grow it into a list of capable-versus-incapable kinds, because such a list would have to mention `mock`, which is a `routing` provider constant but **not** an accepted application type — a body with `"type":"mock"` is already dead at `:347` with `application.type_invalid`, and a message implying an operator could send it would be actively wrong.

Then in the `routing.Application` literal, after `OpportunisticMetricsEnabled: req.OpportunisticMetricsEnabled,` (`:471`):

```go
		ResponsesLiveTimingsEnabled:      liveTimings,
```

Do **not** put this in a trailing normalizer beside `applyProxyExclusion` (`:479`). That call is last because it establishes a cross-field invariant with `ProxyListenPort`; this field's only relationship is with the type, which is settled at `:347` and cannot change afterwards on this path.

- [ ] **Step 8: The update path — refuse before mutating, with the status branch, then the arm and the clear.**

Two insertions, in this order.

First, in the **validate-before-mutate** block, immediately after the two endpoint-mode blocks (`:591-609`) and before `if req.BenchmarkScheduleIntervalSeconds != nil` (`:610`). This is the only place in the task where the two refusal sentinels are chosen between, so the branch lives here and nowhere else:

```go
	// Validate-before-mutate for the live-timings opt-in, judged over the
	// RESULTING type: appType when this PATCH retypes, the stored app.Type
	// otherwise. The type is the authority on whether the flag can be honest,
	// so a body that retypes AND asserts true in one breath is refused on the
	// NEW type rather than accepted against the old one. Refused here, before
	// the mutation block, so a rejected PATCH writes nothing at all -- not the
	// flag and not the type -- which is the same discipline the two mode
	// blocks above and applyProxyExclusion's RULE 2 follow.
	//
	// WHETHER to refuse is a property of the resulting ROW; WHICH sentinel to
	// refuse with is a property of the REQUEST, and the two must not be
	// conflated:
	//
	//   - the request sent "type", so it supplied the incapable type itself
	//     and the body is contradictory on its own terms -> ...Unsupported,
	//     400;
	//   - the request did not, so the incapable type is the stored one and
	//     this PATCH leaves it exactly as it was -> ...Conflict, 409. The
	//     request is well-formed; it collides with this application's state.
	//
	// Read off req.Type != nil, deliberately NOT off *req.Type != app.Type: a
	// PATCH restating the type it already had still SUPPLIED the type it is
	// being judged against, so it belongs in the 400 arm. A value comparison
	// would also drop the portal's own saves into the 409 arm, since
	// ApplicationSection.tsx's buildBody() restates "type" on every save.
	resultingType := app.Type
	liveTimingsRefusal := ErrApplicationResponsesLiveTimingsConflict
	if req.Type != nil {
		resultingType = appType
		liveTimingsRefusal = ErrApplicationResponsesLiveTimingsUnsupported
	}
	if req.ResponsesLiveTimingsEnabled != nil && *req.ResponsesLiveTimingsEnabled &&
		!routing.LiveTimingsCapableKind(resultingType) {
		return ApplicationDTO{}, fmt.Errorf("%w: responses_live_timings_enabled cannot be true for an application of type %q",
			liveTimingsRefusal, resultingType)
	}
```

One `fmt.Errorf` with a sentinel chosen above it, rather than two formatted returns: the message is identical either way (it names the resulting type, which is all a caller needs), and only the wrapped sentinel differs. Whichever way it is written, the mutation in Step 12(g) must fail — one sentinel for both shapes is the slip this task is most likely to ship.

Second, in the mutation block, immediately after the `OpportunisticMetricsEnabled` arm (`:732-733`):

```go
	// Decision (e), update half. The pointer separates an ASSERTION from a
	// NON-MENTION and the two get opposite treatment:
	//
	//   - non-nil: the caller's value, already validated above -- a true here
	//     implies a capable resulting type, or the function has returned;
	//   - nil with an incapable resulting type: the stored value is CLEARED,
	//     so a retype away from llama_cpp/vllm cannot leave a stale true
	//     behind for a kind that can never honour it (LiteLLM, for one,
	//     forwards unknown body keys downstream and OpenAI/Azure answer 400).
	//     This overrides nothing the operator said in THIS request -- they
	//     said nothing about the flag -- which is exactly why a clear is
	//     legitimate here while a silent rewrite of an explicit true is not.
	//
	// It reads app.Type, the POST-mutation type (assigned at :647 above), for
	// the same reason normalizeApplicationTimeoutMS reads it at :671. That
	// also makes the condition a property of the RESULTING ROW rather than of
	// the request's shape -- the distinction applyProxyExclusion's RULE 4
	// spells out at :1100-1125, where a request-shape-only check turned out to
	// be the hole the bad state arrived through. A retype TO a capable kind
	// deliberately does NOT switch it on: decision (d) scopes the
	// kind-dependent 1 to create.
	switch {
	case req.ResponsesLiveTimingsEnabled != nil:
		app.ResponsesLiveTimingsEnabled = *req.ResponsesLiveTimingsEnabled
	case !routing.LiveTimingsCapableKind(app.Type):
		app.ResponsesLiveTimingsEnabled = false
	}
```

Verify the placement assumption before relying on it: `app.Type` is assigned once, at `:647`, and nothing between there and the end of the function reassigns it (`:668-671` reads it, `:776` reads it) — re-check with
```bash
cd /Users/jlor08/Developer/codex/op-ai-gateway/.worktrees/responses-live-timings/gateway/backend
awk 'NR>640 && NR<800 && /app\.Type *=/ {print NR": "$0}' internal/portal/service_applications.go
```
which must print only line 647's assignment.

- [ ] **Step 9: The DTO mapper.**

In `applicationDTO`, after `OpportunisticMetricsEnabled: app.OpportunisticMetricsEnabled,` (`:862`):

```go
		ResponsesLiveTimingsEnabled:      app.ResponsesLiveTimingsEnabled,
```

This mapper is the single one for every application read path — `ListApplications` (`:311`), `GetApplication` (`:505`), and the returns of both writes (`:493`, `:779`) — and it has no reflection guard on this side of the portal, which is why Step 1's test 12 exists.

- [ ] **Step 10: The status mapping — two rows, two statuses.**

In `internal/gateway/portal_application_endpoints.go`, append **both** rows to `portalApplicationErrRows` after `ErrApplicationProxyEntryScheme`'s (`:223`) and before `store.ErrNotFound`'s (`:224`). `errRow.status` is a plain `int` (`error_map.go:21`) that `writeMappedError` writes verbatim (`:75`), and `msgFn` (`:24`, honoured at `:72-74`) is independent of the status — so a 409 with a dynamic message is an ordinary row and needs no change to the mechanism. It is, however, the first row in the package to combine the two (the three existing `msgFn` rows are all 400: `agent_runtime.go:479`, `agent_ingest.go:1774`, `:1912`), so write both rows in one edit and let the wire test confirm the pair:

```go
	// The first two rows in this table with a DYNAMIC message: the service
	// wraps the sentinel with the offending type (fmt.Errorf("%w: ...")), and
	// naming the kind is the entire point of refusing the write instead of
	// storing something else. errRow.msgFn exists for exactly this ("a row
	// that must surface the underlying error's own text", error_map.go:15-18).
	// Each sentinel's own text IS its API code -- the convention every row here
	// follows -- so it is trimmed off rather than repeated inside the message.
	//
	// TWO rows because there are two sentinels and they answer with DIFFERENT
	// statuses. 400 when the request supplied the incapable type itself (every
	// create, and a PATCH that sends "type"): the body is contradictory on its
	// own terms. 409 when the type came from the stored row and the request
	// left it alone: the request SHAPE is fine, it conflicts with the target's
	// own state -- the same reading ErrServerManagedRuntimeOnly (:203-206) and
	// the three proxy sentinels (:220-221) already get. Collapsing them into
	// one row would report a well-formed request as malformed.
	{
		err:    portal.ErrApplicationResponsesLiveTimingsUnsupported,
		status: http.StatusBadRequest,
		code:   "application.responses_live_timings_unsupported",
		msgFn: func(err error) string {
			return strings.TrimPrefix(err.Error(), portal.ErrApplicationResponsesLiveTimingsUnsupported.Error()+": ")
		},
	},
	{
		err:    portal.ErrApplicationResponsesLiveTimingsConflict,
		status: http.StatusConflict,
		code:   "application.responses_live_timings_conflict",
		msgFn: func(err error) string {
			return strings.TrimPrefix(err.Error(), portal.ErrApplicationResponsesLiveTimingsConflict.Error()+": ")
		},
	},
```

`strings` and `net/http` are already imported by this file. Note that `writeMappedError` returns on the **first** matching row and the two sentinels are distinct `errors.New` values that wrap nothing, so their order in the table does not matter; do not add a `sharedErrorMap` entry for either — both are application-specific, and `sharedErrorMap` rows are written without `msgFn` at all (`error_map.go:79-84`).

- [ ] **Step 11: Run the new tests and watch them pass.**

```bash
cd /Users/jlor08/Developer/codex/op-ai-gateway/.worktrees/responses-live-timings/gateway/backend
go test ./internal/portal/ -run 'ResponsesLiveTimings' -count=1 -v
go test ./internal/gateway/ -run 'LiveTimingsRefusalReachesTheWire' -count=1 -v
go test ./internal/portal/ ./internal/gateway/ -count=1
```
Both packages must be green — in particular `TestCreateApplicationEndpointModeDefaultsToPassthrough`, `TestUpdateApplicationEndpointModes` and `TestUpdateApplicationPartialUpdatePreservesOtherFields`, none of which may be disturbed by a new type-conditional default or by a new refusal on a path they exercise.

- [ ] **Step 12: Mutations.**

(a) Delete the `ResponsesLiveTimingsEnabled:` line from `applicationDTO` — test 12 (and several others) must fail; this is the silent-zero-value defect, demonstrated. Restore.
(b) Change `CreateApplicationRequest`'s field from `*bool` to `bool` and adjust the create block to read it directly — test 2's two halves can no longer both pass, and test 1's `llama_cpp`/`vllm` rows fail as well, because an absent key now arrives as `false` and the kind-dependent default never fires. Restore.
(c) Replace the refusal with the silent normalisation this task replaced — on the create path, `if !liveTimingsCapable { liveTimings = false }` and no error: test 4 must fail on the error assertion **and** on "nothing stored", because the create then succeeds. Then the same on the update path, dropping the validate-before-mutate refusal and letting the clear swallow the explicit `true`: tests 8, 9 and 10 must fail the same way, and test 8's "the type did not change either" assertion must fail too. Restore both. This mutation is the decision itself, so record its output verbatim in the commit body.
(d) Delete the `case !routing.LiveTimingsCapableKind(app.Type):` arm — test 7 fails and only test 7. Restore.
(e) Change the update refusal to read `app.Type` instead of `resultingType` — test 8 fails (the retype-plus-true body is accepted against the OLD capable type) while test 9 still passes. Restore.
(f) Delete the 400 `errRow` from Step 10, then (separately) the 409 one — each time the corresponding wire subtest's code assertion fails with `application.request_failed` and a 500, which is the fall-through defect that table's own comment records. Note that deleting the 409 row leaves the "do not share a status" subtest failing too, since both shapes then answer 500. Restore.
(g) **The status-split mutation, and the most important one in this task.** Collapse the two sentinels into one: in the update path's validate-before-mutate block, replace the `liveTimingsRefusal` branch with an unconditional `ErrApplicationResponsesLiveTimingsUnsupported`. Service test 9 must fail (it gets `…Unsupported` where it asserted `…Conflict`, and its `!errors.Is(…Unsupported)` assertion fails too), and the wire test's 409 subtest **and** its "the two PATCH shapes do not share a status" subtest must fail with both shapes answering 400. Then do the inverse — unconditional `…Conflict` — and tests 4, 8 and 10 must fail while 9 passes, with the create's refusal now arriving as a 409 on a request that had no prior state at all. Restore. Record both outputs verbatim: a single helper returning one sentinel for both shapes is the implementation this task exists to rule out, and the two halves of this mutation are the evidence that the branch is real rather than decorative.
(h) Change the status branch from `req.Type != nil` to a value comparison, `req.Type != nil && *req.Type != app.Type` — test 10 fails (a PATCH restating the same incapable type is answered 409 when the request did supply that type) and nothing else does. Restore. This is the mutation that pins *why* the branch is written off the pointer.
Record which mutation broke which test.

- [ ] **Step 13: Gates and commit.**

`fmt --diff` (a new struct field re-aligns the whole tag column — expect gofumpt to have an opinion), `run`, `go test ./... -count=1`. Commit body: the pointer's three loads; the assertion-versus-non-mention rule in one sentence; why the refusal is judged over the resulting type while the status is judged over the request's shape; why create is 400-only; the two sentinels and the two error rows with their statuses; and the eight mutations, with (c) and (g) quoted verbatim.

---

### Task 7: The portal's runtime-spec surface, the spread, and the wire docs

**Files:**
- Modify: `gateway/backend/internal/portal/service_runtime.go` (the `"fmt"` import; the new sentinel after `ErrRuntimeSpecContextProbePathInvalid` at `:119`; `RuntimeSpecDTO` field after `:392`; `PutRuntimeSpecRequest` field after `:459`; the effective-kind refusal LAST in the validate-before-mutate block, after `normalizeRuntimeSpecFlavors`' error return at `:669-672`; the value resolution after the `existing, hadExisting` read at `:711`; the `routing.RuntimeSpec` literal after `:801`; the `runtimeSpecDTO` mapper line after `:1180`)
- Modify: `gateway/backend/internal/gateway/portal_runtime_endpoints.go` (one `errRow` in `portalRuntimeSpecErrRows`, inserted immediately after `:84` — **one**, at 400; this task adds no 409)
- Modify: `gateway/backend/internal/portal/service_runtime_benchmark.go` (`putRequestFromDTO`, after `MessagesMode: dto.MessagesMode,` at `:51`)
- Modify: `gateway/backend/internal/portal/service_runtime_benchmark_test.go` (the fully-populated DTO at `:138-172` and the `want` literal at `:174-201`)
- Modify: `gateway/backend/internal/portal/service_runtime_test.go` (new tests)
- Modify: `gateway/backend/internal/gateway/portal_runtime_endpoints_test.go` (**two** wire tests, beside `TestHandlePortalMappingRuntimeSpecPutBadTypeReturns400` at `:148`)
- Modify: `docs/architecture/reference/api-surface.md` (`:494-555`), `docs/architecture/cross-cutting/agent-runtime-manager.md` (`:3733-3759`)

**Interfaces:**
- Consumes: `routing.LiveTimingsCapableKind(kind string) bool` (Task 5); `routing.RuntimeSpec.ResponsesLiveTimingsEnabled bool` (Task 3); `routing.EffectiveRuntimeSpecType(spec RuntimeSpec) RuntimeSpecType` (`internal/routing/runtime_spec_type.go:56`); `routing.DetectRuntimeSpecType(binary string) RuntimeSpecType` (`:36`, what the former falls back to); `binary := strings.TrimSpace(req.Binary)` (`service_runtime.go:600`); `specType := strings.TrimSpace(req.Type)` (`:631`); `validRuntimeSpecType(s string) bool` (`:997`, doc `:990-996`) — which **accepts `""` as a first-class value**, meaning "auto-detect from `Binary`", not a default that has already collapsed into one of the five kinds; `existing, hadExisting, err := s.routes.RuntimeSpecByMapping(ctx, mapping.ID)` (`:711`).
- Produces: JSON key `responses_live_timings_enabled` — `bool` on `RuntimeSpecDTO`, `*bool` on `PutRuntimeSpecRequest`; carried across by `putRequestFromDTO`; and **one new error sentinel**, `portal.ErrRuntimeSpecResponsesLiveTimingsUnsupported` (`errors.New("runtime_spec.responses_live_timings_unsupported")`), mapped to HTTP **400** with a message that names the offending effective kind. **One sentinel and one status on this side.** The application surface (Task 6) splits its refusal into 400 and 409 by whether the request supplied the offending type; a runtime-spec write cannot reach the 409 shape at all, for the reason spelled out two paragraphs below — it is a full document that always carries the type it is judged against. Do not add a second sentinel here, and do not import Task 6's `…Conflict`.

**The rule, stated once.** `PutRuntimeSpec` is a full-document upsert ("create on first write, full-document replace thereafter", `:534-535`), so `!hadExisting` is what "create" means here, and the kind is the spec's **own** `routing.EffectiveRuntimeSpecType(routing.RuntimeSpec{Type: specType, Binary: binary})` — the explicit type when set, else detected from the binary's basename, which is the same resolver the inference path consults through `Target.LiveProgressSpecType`. So:

- absent + no existing spec → `routing.LiveTimingsCapableKind(string(effectiveKind))` (the kind-dependent create default);
- absent + an existing spec → the existing spec's stored value (a later save must not silently re-enable a flag the operator turned off, and must not silently clear one either) — **unless** this document's effective kind cannot honour it, in which case the stored value is **cleared**;
- present `false` → honoured, on any kind;
- present `true` on an effective kind that is not capable → **400**, naming that kind, with nothing written.

The **effective** kind, not `req.Type`, and this is not a nicety: `validRuntimeSpecType` accepts `""` as a first-class value (auto-detect from `Binary`), `routing.LiveTimingsCapableKind("")` is **false**, and an empty `type` with a `llama-server` binary is the common managed configuration. A refusal or a default written against the raw `req.Type` would therefore reject an operator's perfectly capable auto-detect spec *and* silently default every such spec **off**. Every live-timings question in this task is asked of `routing.EffectiveRuntimeSpecType(routing.RuntimeSpec{Type: specType, Binary: binary})` — both the refusal in Step 7 and the create default in Step 8.

**Why the rule is phrased over the RESULTING type and not over a transition, and why that makes the status 400 in every shape** — read this before looking for a retype signal, because there is none to find. A spec PUT is a full document: it restates `Type` and `Binary` on every save and carries no "the type used to be X" input. `hadExisting` tells you whether a row was there, not whether its kind changed, and comparing `existing.Type` to `req.Type` would answer a question the rule does not ask (a *first* write of an incapable spec asserting `true` must be refused too, and there is no transition in it at all). Judging the resulting document is therefore the only faithful reading — and it is the same shape decision (e) takes on the application side, where the clear reads the post-mutation `app.Type` rather than `req.Type != nil`.

That same property settles the status. Task 6's application surface answers **409** on exactly one shape — a PATCH that asserts the flag while leaving the stored type untouched, so the refusal turns on state the request never mentioned. **No spec write can be that shape.** The request always carries `Type` and `Binary`, so the kind a spec refusal names is always one the caller supplied in this very body, whether by typing it or by naming the binary it is detected from; such a body is contradictory on its own terms and 400 is the honest answer. So: one sentinel, one status, and **no stored-state branch anywhere in this task** — `hadExisting` is consulted for the default-versus-preserve decision only, never for the refusal.

What changed since the first draft of this plan: a `true` that cannot hold is now **refused** rather than quietly stored as `false`. An **absent** field is still never refused; on an incapable kind it is cleared, which overrides nothing the caller said because the caller said nothing.

- The spec never inherits the parent application's value. That is `PutRuntimeSpec`'s standing "no backend inheritance" contract (`:648-652`, pinned by `TestPutRuntimeSpecDoesNotInheritAppModes`), and it matters doubly here: a `server_agent` parent's own flag is necessarily `false` — the application path refuses to store anything else for that kind — so inheriting would default every managed spec off.

- [ ] **Step 1: Write the failing service tests.**

In `service_runtime_test.go`, following the shape of the existing `PutRuntimeSpec` tests (`svc, routeStore := newServerTestService(t, now)`, `createTestServer`, `seedServerAgentApplication(t, routeStore, server.ID, now)`, `svc.CreateMapping(…)`, then `svc.PutRuntimeSpec(ctx, ownerToken(), mapping.ID, PutRuntimeSpecRequest{…})`):

**Assertion style, before test 1.** House style in both files this task touches is `t.Fatalf`, and it stays that way for every single-fact assertion. But a test that must report **two** independent facts about one call — the error shape *and* what was (not) written — uses `t.Errorf` for the error-shape legs, and guards every `err.Error()` with `err == nil ||`. With `t.Fatalf` the first failing leg aborts and the state assertion goes unverified in exactly the run that needs it, and a `nil` `err` **panics** on `err.Error()` instead of failing. This is not a preference: Task 6 measured it and shipped that shape (`service_applications_test.go:3126-3138` and `:3302-3322` are the two models to copy). It applies to test 6's two legs and test 9's three. Mutation 13(c) is the run that proves it.

1. `TestPutRuntimeSpecResponsesLiveTimingsDefaultsFromTheSpecsOwnKind` — first write (no existing spec), key absent, three cases, each on its own mapping: `Type: string(routing.RuntimeSpecTypeLlamaCpp)` → `true`; `Type: ""` with `Binary: "/usr/bin/llama-server"` (so `routing.DetectRuntimeSpecType` resolves `llama_cpp`) → `true`; `Type: string(routing.RuntimeSpecTypeOllama)` → `false`. The second case is the one that pins *which* resolver decides.
2. `TestPutRuntimeSpecResponsesLiveTimingsDoesNotInheritTheParentApplication` — the pin is a **capable** spec kind under a parent whose flag differs from the kind default, and that is the only direction that can fail. A `server_agent` parent whose own flag is **`false`** — its only reachable value, and what `seedServerAgentApplication` already writes — then a first spec write with `Type: string(routing.RuntimeSpecTypeLlamaCpp)` and the key absent → **`true`**, the spec's own kind default. An implementation seeding from `app.ResponsesLiveTimingsEnabled` (`app` is a `putRuntimeSpec` parameter, `:595`, so it is right there to be misused) answers `false` here and the test fails.

   Add the incapable leg too — `Type: string(routing.RuntimeSpecTypeOllama)` under a parent whose flag is `true`, written with `routeStore.UpdateApplication` (`routing/memory_store.go:818`) because the API refuses that state — but the doc comment must call it a **coherence** leg, not the pin: on an incapable kind Step 8's `case !liveTimingsCapable:` arm forces `false` whatever the seed was, so `false` there is satisfied by the kind default, by the clear, *and* by a complete inheritance defect alike. Do not write the incapable leg as if it proved the contract; it cannot.

   The comment must also name the no-inheritance contract (`:648-652`, `TestPutRuntimeSpecDoesNotInheritAppModes`) and say why the `true` seed has to bypass the service: `CreateApplication`/`UpdateApplication` refuse to store `true` on a `server_agent` application at all, so that fixture state is unreachable through the API — which is itself the invariant this task establishes, not a gap in the test.
3. `TestPutRuntimeSpecResponsesLiveTimingsPreservesAnExistingValueWhenAbsent` — first write with `boolPtr(false)` on a `llama_cpp` spec; second write with the key absent → still `false`. Then the same spec written with `boolPtr(true)` and saved again with the key absent → still `true`. The defect this pins, in both directions: the kind-dependent default firing on every save would re-enable a flag the operator turned off, and an over-eager clear would drop one they turned on.
4. `TestPutRuntimeSpecResponsesLiveTimingsAbsentIsNotFalseOnAFirstWrite` — two fresh mappings, both first writes on `Type: string(routing.RuntimeSpecTypeLlamaCpp)`: one with the key absent → `true`, one with `boolPtr(false)` → `false`. With a plain `bool` on the request these would be the same call: the absent-key leg would arrive as `false` and the kind-dependent default could never fire. Say **that** in the doc comment — do not predict an assertion failure from swapping the field's type, because that swap is a compile failure in this very test file (`boolPtr(...)` into a `bool` field) and the discipline forbids editing tests to measure a mutation. Task 6 hit exactly this and had to measure the behavioural equivalent instead; mutation 13(i) is that equivalent, written as a production-only edit. This is the pointer's whole purpose on the spec side.
5. `TestPutRuntimeSpecResponsesLiveTimingsHonoursAnExplicitValue` — `boolPtr(true)` then `boolPtr(false)` on a `llama_cpp` spec; each is stored and echoed by the returned DTO. Add a third leg on its own mapping: `Type: ""` with `Binary: "/usr/bin/llama-server"` and `boolPtr(true)` → **accepted** and stored `true`. That leg is the accepting direction of the effective-kind resolution: a refusal written against the raw `req.Type` would reject it, because `routing.LiveTimingsCapableKind("")` is false, and nothing else in this list would notice.
6. `TestPutRuntimeSpecResponsesLiveTimingsRejectsTrueOnAnIncapableKind` — two legs, each asserting `errors.Is(err, ErrRuntimeSpecResponsesLiveTimingsUnsupported)`, the effective kind named in `err.Error()`, and that **nothing was written**. The two error-shape assertions in each leg are `t.Errorf` with an `err == nil ||` guard before the `err.Error()`, per the assertion-style note above: mutation 13(c) makes `err` nil, and with `t.Fatalf` the "nothing was written" leg would never run in the one run that needs it (and the unguarded `err.Error()` would panic).
   - an existing `llama_cpp` spec with the flag on — seeded with an **explicit** `Type: string(routing.RuntimeSpecTypeLlamaCpp)`, *not* by auto-detect, because the assertion below reads the DTO's raw stored `Type`: `runtimeSpecDTO` echoes `spec.Type` (`:1188`) and reports the resolved kind separately as `EffectiveType` (`:1191`), so a `Type: ""` + `llama-server` seed stores `""` and `Type == "llama_cpp"` would fail for a reason with nothing to do with this feature — PUT with `Type: string(routing.RuntimeSpecTypeOllama)` **and** `boolPtr(true)` → refused naming `ollama`; then `svc.GetRuntimeSpec` must still report `Type == "llama_cpp"` **and** the flag still `true`. A refused PUT writes nothing, not even the type.
   - a *first* write with `Type: ""` and `Binary: "/usr/local/bin/ollama"` plus `boolPtr(true)` → refused naming `ollama`, the kind the caller never typed. Then `GetRuntimeSpec` must still report the unconfigured DTO (`Configured == false`), proving no row was created. The comment must say that the message names the **effective** kind precisely because an empty `Type` means the binary decided.

   The doc comment must also say what these two legs are jointly evidence for, because it is the reason this task has no second sentinel: the first leg has a stored row and the second has none, and **both** are the same refusal, because in both the caller supplied the offending kind in this very body. There is no spec shape where the refused kind comes from state the request left alone, which is the shape the application surface answers 409 on (Task 6).
7. `TestPutRuntimeSpecResponsesLiveTimingsClearsAStoredTrueWhenTheDocumentsKindCannotHonourIt` — an existing `llama_cpp` spec with the flag on (assert `true` first, so the change is visible), then a PUT with `Type: string(routing.RuntimeSpecTypeOllama)` and the key **absent** → the DTO and a `GetRuntimeSpec` reload both report `false`. This is the clear, and the assertion that it *changed* from `true` is what makes the test mean anything.
8. `TestRuntimeSpecDTOCarriesResponsesLiveTimings` — upsert a spec with the flag on through the routes store (`routeStore.UpsertRuntimeSpec`), read it through `GetRuntimeSpec`; assert `true`. What it is *for*: it is the only test in this list that reads a spec written **outside** the service, with no `putRuntimeSpec` in the path, so it still speaks if the whole resolution in Step 8 is gone. It is **not** the only guard on the hand-written `runtimeSpecDTO` mapper, and its doc comment must not claim to be — `putRuntimeSpec` returns `runtimeSpecDTO(spec, storedGPUs, app)` (`:833`), so every test above reads that mapper too, which is why mutation 13(f) breaks all eight and not only this one.
9. `TestPutRuntimeSpecResponsesLiveTimingsRefusalRunsAfterThePreExistingValidations` — three legs, each a body that is invalid for a **shipped** reason *and* carries an impossible `boolPtr(true)`, each asserting that the shipped sentinel came back and that the new one did **not** (both halves `t.Errorf`, per the note above, so one leg reports both facts). All three on `Binary: "/usr/local/bin/ollama"` with `Type: string(routing.RuntimeSpecTypeOllama)`, so the live-timings refusal really is armed: `MetricsPath: "//evil"` → `ErrRuntimeSpecMetricsPathInvalid`; `ResponsesMode: "bogus"` → `ErrRuntimeSpecEndpointModeInvalid`; `APIFlavors: []string{"nope"}` → `ErrRuntimeSpecFlavorInvalid`. This is the test that pins Step 7's POSITION and the only one that can — every other test in this list passes wherever in the validate block the refusal sits. Mutation 13(j) is the run that proves it. Modelled on Task 6's `TestCreateApplicationLiveTimingsRefusalRunsAfterThePreExistingValidations` (`service_applications_test.go:3521`), which exists because Task 6 shipped this defect once and had to repair it.

Also extend `TestPutRequestFromDTOCoversEveryWritableField` in `service_runtime_benchmark_test.go`: add `ResponsesLiveTimingsEnabled: true,` to the fully-populated `dto` literal and `ResponsesLiveTimingsEnabled: &dto.ResponsesLiveTimingsEnabled,` to the `want` literal. `reflect.DeepEqual` compares pointers by pointee, so a pointer to an equal value passes; a `nil` from a forgotten spread line does not. The two tag-name loops at `:126-135` stay **green** through all of this and must not be predicted to fire: they compare the *struct field sets* of `RuntimeSpecDTO` and `PutRuntimeSpecRequest` by reflection (`:108-124`) and never look at `putRequestFromDTO` at all. What they guard is Step 4 without Step 5 or the reverse — which in this task is not reachable by a production-only mutation, because both fields have production consumers (Steps 7, 8, 9, 11) and deleting either is a compile failure rather than a test failure. The `DeepEqual` (`:202-204`) is therefore the whole measurable guard on the spread, and 13(a) is how it is measured. (That DTO's `Type` is already `string(routing.RuntimeSpecTypeVLLM)` — a capable kind — so `true` is a coherent value for it; leave the `Type` alone.)

- [ ] **Step 2: Write the two failing wire tests.**

In `internal/gateway/portal_runtime_endpoints_test.go`, after `TestHandlePortalMappingRuntimeSpecPutBadTypeReturns400` (`:148-161`), add its two siblings. `seedRuntimeSpecMapping(t, srv)` (`:22`) already builds the server → `server_agent` application → mapping chain, through `srv.Routes.CreateAIServer`/`CreateApplication`/`CreateMapping` **directly** (`:29-40`) rather than over HTTP; the mapping id it returns is all these tests need, so do not go looking for HTTP seeding that is not there.

Read this before writing the second test, because it decides its shape: **this endpoint drops unknown JSON keys silently.** The PUT handler decodes with a plain `json.Unmarshal` (`portal_runtime_endpoints.go:41-45`) and `DisallowUnknownFields` appears nowhere in this module, so before Step 5 lands the request field a body carrying `responses_live_timings_enabled` is accepted exactly as if the key were absent. A test that asserts only "200" therefore passes before the production change **and** after it, and earns nothing. The second test must assert the **effect** — the value echoed in the response DTO — which is also what makes it a real red at Step 3.

```go
// TestHandlePortalMappingRuntimeSpecPutLiveTimingsOnIncapableKindReturns400
// pins the WIRE contract of
// portal.ErrRuntimeSpecResponsesLiveTimingsUnsupported, mirroring
// TestHandlePortalMappingRuntimeSpecPutBadTypeReturns400 above -- and the
// MESSAGE too, not only the code: the request is refused rather than quietly
// stored as false so that the caller is told which kind cannot honour it, and
// a 400 that withholds the kind gives them nothing the silent rewrite would
// not have.
//
// The status assertion is exact on purpose. The application surface splits
// this refusal into 400 and 409 by whether the request supplied the offending
// type; a spec PUT is a full document and always does supply it, so 400 is the
// only answer a spec write can give. This is the test that says so: a 409 here
// would mean somebody carried Task 6's stored-state branch into a path that
// has no such shape.
func TestHandlePortalMappingRuntimeSpecPutLiveTimingsOnIncapableKindReturns400(t *testing.T) {
	srv := NewTestServer()
	mappingID := seedRuntimeSpecMapping(t, srv)
	body := `{"binary":"/usr/local/bin/ollama","type":"ollama","responses_live_timings_enabled":true}`
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, newJSONRequest(http.MethodPut, "/api/portal/mappings/"+mappingID+"/runtime-spec", body))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body = %s", rec.Code, rec.Body.String())
	}
	if code := errorBodyOf(t, rec); code != "runtime_spec.responses_live_timings_unsupported" {
		t.Fatalf("error code = %q, want runtime_spec.responses_live_timings_unsupported", code)
	}
	if !strings.Contains(rec.Body.String(), "ollama") {
		t.Fatalf("the 400 does not name the offending kind: %s", rec.Body.String())
	}
}
```

Then add its auto-detect sibling in the same file, because the wire is where an operator's real body arrives and the common managed body types no `type` at all:

```go
// TestHandlePortalMappingRuntimeSpecPutLiveTimingsOnAnAutoDetectCapableKindIsAccepted
// is the accepting direction of the same resolution, over HTTP: no "type" at
// all, a llama-server binary, and the flag asserted true. It must be a 200
// whose BODY says true.
//
// The service asks routing.LiveTimingsCapableKind about the spec's EFFECTIVE
// type (the explicit one when set, else detected from the binary), and this
// is the body that proves it: validRuntimeSpecType accepts "" as a real value
// and LiveTimingsCapableKind("") is FALSE, so a refusal written against the
// raw req.Type would answer 400 here -- to the single most common managed
// llama.cpp configuration there is.
//
// The echo assertion, not the status, is this test's whole content. This
// endpoint decodes with a plain json.Unmarshal and nothing in this module
// sets DisallowUnknownFields, so 200 is what it already answered to this body
// before responses_live_timings_enabled existed as a field at all -- the key
// was simply discarded. Reading the value back out of the response is the
// only way the test can tell "accepted and stored" from "ignored", and it
// doubles as the spec side's JSON-tag pin: the key is decoded BY NAME out of
// a real success body, which is the one thing no internal/portal test can do.
func TestHandlePortalMappingRuntimeSpecPutLiveTimingsOnAnAutoDetectCapableKindIsAccepted(t *testing.T) {
	srv := NewTestServer()
	mappingID := seedRuntimeSpecMapping(t, srv)
	body := `{"binary":"/usr/bin/llama-server","responses_live_timings_enabled":true}`
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, newJSONRequest(http.MethodPut, "/api/portal/mappings/"+mappingID+"/runtime-spec", body))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body = %s", rec.Code, rec.Body.String())
	}
	var dto portal.RuntimeSpecDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &dto); err != nil {
		t.Fatalf("unmarshal: %v, body = %s", err, rec.Body.String())
	}
	if !dto.ResponsesLiveTimingsEnabled {
		t.Fatalf("responses_live_timings_enabled = false, want true: a 200 that did not store what it was asked to store, body = %s", rec.Body.String())
	}
}
```

One import to add: the file's block (`:6-16`) has `context`, `encoding/json`, `net/http`, `net/http/httptest`, `op-ai-gateway/internal/portal`, `op-ai-gateway/internal/routing`, `strconv`, `testing`, `time` — but **not `strings`**, which the message assertion needs. Add it between `strconv` and `testing` (one alphabetically sorted list; gofumpt will reject any other position). `encoding/json` (`:8`) and `op-ai-gateway/internal/portal` (`:11`) the second test needs are already there, and the decode-the-DTO pattern to copy is `TestHandlePortalMappingRuntimeSpecGetUnconfigured` (`:52-58`). The `http.StatusOK` in the second test is this endpoint's own happy-path status, not a guess: the existing PUT tests assert it, including one whose whole body is `{"binary":"/usr/local/bin/llama-server"}` (`:286-287`).

- [ ] **Step 3: Run them and watch them fail.**

```bash
cd /Users/jlor08/Developer/codex/op-ai-gateway/.worktrees/responses-live-timings/gateway/backend
go test ./internal/portal/ -run 'RuntimeSpecResponsesLiveTimings|RuntimeSpecDTOCarriesResponsesLiveTimings|PutRequestFromDTO' -count=1
go test ./internal/gateway/ -run 'RuntimeSpecPutLiveTimings' -count=1
```

Expected, per package, and they are **not** the same failure:

- `./internal/portal/` — compile failures on the unknown `RuntimeSpecDTO` field, the unknown `PutRuntimeSpecRequest` field and the unknown sentinel.
- `./internal/gateway/` — a compile failure too, but on exactly **one** line: `dto.ResponsesLiveTimingsEnabled` in the auto-detect test. Note what that means, because it is easy to misread as coverage: Go builds the test package as a whole, so neither wire test runs, and the incapable-kind test's own red is not yet visible. That test names no new Go symbol at all — only string literals, `errorBodyOf`, `seedRuntimeSpecMapping` and `http.Status*` — so it would compile on its own and answer **200**, the unknown key having been discarded by the plain `json.Unmarshal`. To see it go red, re-run this one command after Step 4 lands the DTO field (which is all the gateway package needs to build) and before Step 7 lands the refusal. **Both** wire tests are red at that point, each on its own line: `status = 200, want 400` for the incapable-kind one, and `responses_live_timings_enabled = false, want true` for the auto-detect one (Step 9's mapper line is not in yet either). Record that intermediate run — it is the only point at which the incapable-kind test's own failure mode is observable.

- [ ] **Step 4: The DTO field.**

In `RuntimeSpecDTO`, after `MessagesMode  string \`json:"messages_mode"\`` (`:392`):

```go
	// ResponsesLiveTimingsEnabled is this spec's own copy of the live-timings
	// opt-in (migration 80) -- stored explicitly on the spec rather than
	// inherited from the parent server_agent application, exactly like the trio
	// above, and for a server_agent model the RESOLVED spec's copy is the one
	// the request path reads. Always the STORED value, which is always a value
	// this spec's EFFECTIVE kind can honour: a PUT that asks for true on a kind
	// that cannot is refused with 400, never stored as false.
	ResponsesLiveTimingsEnabled bool `json:"responses_live_timings_enabled"`
```

- [ ] **Step 5: The request field, a pointer.**

In `PutRuntimeSpecRequest`, after `MessagesMode  string \`json:"messages_mode"\`` (`:459`):

```go
	// ResponsesLiveTimingsEnabled: a POINTER inside an otherwise
	// apply-verbatim full-document request, the same exception APIToken below
	// already makes (nil = keep the stored value), and it carries two loads.
	// nil must mean "no opinion", so that a FIRST write can get the
	// kind-dependent default (ON for a llama_cpp/vllm spec) while a later save
	// of an existing spec keeps whatever the operator last stored; a plain
	// bool would arrive as false on every full-document PUT and the default
	// could never fire. And nil is what separates a NON-MENTION from an
	// ASSERTION: an explicit true on an effective kind that cannot honour it
	// is refused (ErrRuntimeSpecResponsesLiveTimingsUnsupported), while an
	// absent field on such a kind is simply cleared.
	ResponsesLiveTimingsEnabled *bool `json:"responses_live_timings_enabled,omitempty"`
```

- [ ] **Step 6: The sentinel.**

In the `var (…)` block, after `ErrRuntimeSpecContextProbePathInvalid` (`:119`):

```go
	// ErrRuntimeSpecResponsesLiveTimingsUnsupported rejects an EXPLICIT
	// responses_live_timings_enabled:true on a spec whose EFFECTIVE kind
	// (routing.EffectiveRuntimeSpecType: the explicit Type when set, else
	// detected from the binary's basename) is not live-timings capable. HTTP
	// 400, and the message names that effective kind -- which may be a kind
	// the caller never typed, because an empty Type means the binary decided.
	//
	// Refused rather than stored as false because a 200 that stores something
	// other than what it was asked to store is a write that lies about its
	// result. An ABSENT field is never this error: on a first write it takes
	// the kind-dependent default, on a later save it keeps the stored value,
	// and on a document whose kind cannot honour the flag it is CLEARED --
	// which overrides nothing the caller said, since they said nothing.
	//
	// 400 in EVERY shape, and one sentinel is therefore enough here. The
	// application surface splits its refusal (ErrApplication...Unsupported,
	// 400, vs ErrApplication...Conflict, 409) by whether the request supplied
	// the offending type; a spec write is a full document that always states
	// its own Type and Binary, so the refused kind is always one this body
	// supplied -- typed, or named by the binary it is detected from. There is
	// no spec shape whose refusal rests on state the request left alone.
	ErrRuntimeSpecResponsesLiveTimingsUnsupported = errors.New("runtime_spec.responses_live_timings_unsupported")
```

The error text is the API code, the convention every sentinel in this block follows; the detail is wrapped on at the call site with `fmt.Errorf("%w: …", …)`. **Add `"fmt"` to this file's import block**, between `"errors"` and `"op-ai-gateway/internal/auth"` (one alphabetically sorted list; gofumpt will reject any other position).

- [ ] **Step 7: Refuse an impossible `true` — LAST in the validate-before-mutate block, asking the EFFECTIVE kind, not `req.Type`.**

Read this before writing the condition, because it is the one line in the task that has a silent-wrong version. **Ask `routing.LiveTimingsCapableKind` about `routing.EffectiveRuntimeSpecType(...)`, never about `req.Type` or `specType`.** `validRuntimeSpecType` (`:997`, doc `:990-996`) accepts `""` as a legitimate value meaning "auto-detect from `Binary`" — "not a default that collapses into one of the five kinds" — and `routing.LiveTimingsCapableKind("")` is **false**. A condition written against the raw type therefore refuses an explicit `true` on `{"binary":"/usr/bin/llama-server"}`, and Step 8's default written the same way stores `false` for it: the single most common managed llama.cpp spec there is, silently defaulted off, with every incapable-kind test in this task still green. Step 1's test 1 second case, test 5's third leg and Step 2's auto-detect wire test are the three that fail if this is got wrong — and so are five benchmark tests and two reserved-path tests, for the reason Step 12 spells out. Mutation 13(e) is where that is demonstrated.

**WHERE it goes, and that is a decision rather than a detail.** `putRuntimeSpec` opens with "Validate everything that can fail BEFORE mutating/persisting anything" (`:599`), and the refusal belongs in that block rather than beside the value resolution in Step 8. But it goes at the **end** of the block, not beside the type check: insert it immediately after `normalizeRuntimeSpecFlavors`' error return (`:669-672`). `specType` (`:631`) and `binary` (`:600`) are both still in scope there, nothing has been read from or written to the store yet (the first store touch is the read at `:711`), and every check a body could already fail on for a **shipped** reason now runs first.

Placed where the first draft of this plan put it — after `:634` — the refusal sits ahead of `safeRelativeProbePath`'s two checks (`:642-646`), both endpoint-mode checks (`:653-668`) and `normalizeRuntimeSpecFlavors` (`:669`), so a body invalid in two ways answers `runtime_spec.responses_live_timings_unsupported` where it used to answer `runtime_spec.metrics_path_invalid`, `runtime_spec.endpoint_mode_invalid` or `runtime_spec.flavor_invalid`. That is a behaviour change to three shipped codes dressed up as an addition. Task 6 shipped exactly that defect on both application paths and its fix round had to move both blocks to be "LAST of the request validations", with a `POSITION, deliberately:` paragraph in each comment saying so; Task 6's report handed this decision to Task 7 explicitly. Step 1's test 9 pins the position and mutation 13(j) measures it.

**One residue, and the comment must state it** — the same one Task 6 accepted. The refusal still precedes `capture.SealSecret`, whose keyless-disk-store rejection (`capture.ErrKeyRequired` → 400 `runtime_spec.api_token_key_required`) is fifty lines of write-path preparation rather than a request validation; Task 6's create refusal sits immediately before its own `capture.SealSecret` call for the same reason. So a body pairing an impossible `true` with an `api_token` on a keyless store reports the live-timings 400. Say so and do not chase it — the request-shape validation of the token pair, `validateRuntimeSpecAPIToken` (`:628`), already runs above.

```go
	// The spec's OWN effective kind decides whether the live-timings opt-in
	// can be honest here: the explicit Type when set, else detected from the
	// binary's basename. Same resolver the inference path consults through
	// Target.LiveProgressSpecType, so the portal and the gateway cannot
	// disagree about what actually serves.
	//
	// EffectiveRuntimeSpecType, NOT specType: validRuntimeSpecType accepts ""
	// as a real value ("auto-detect from Binary", not a collapsed default) and
	// LiveTimingsCapableKind("") is false, so asking about the raw type would
	// refuse an explicit true on a {"binary": ".../llama-server"} spec -- the
	// commonest managed configuration there is -- and, in the resolution
	// below, silently default it OFF. It would also refuse the VRAM
	// benchmark's own deferred restore, which replays a stored Type of ""
	// verbatim through putRequestFromDTO alongside an explicit true.
	//
	// An EXPLICIT true on a kind that cannot honour it is refused, naming that
	// kind. Judged over THIS document's kind rather than over a type change:
	// a PUT is a full document and carries no retype signal -- hadExisting
	// says a row was there, never that its kind changed -- and a first write
	// asserting true on an incapable kind has to be refused too, though no
	// transition is involved in it at all.
	//
	// POSITION, deliberately: LAST of the request validations. Every check a
	// body could already fail on -- the binary, the tuning values,
	// admin_state, the GPU rows, the env keys, the visible-devices pair, the
	// four api_token shapes, validRuntimeSpecType, both probe paths, both
	// endpoint modes and the flavors -- runs above this and RETURNS rather
	// than falling through, so this brand-new check can never rewrite the
	// answer to a body that was already invalid for a SHIPPED reason. Placed
	// beside the type check instead, a doubly-invalid body reported this 400
	// where it used to report runtime_spec.metrics_path_invalid,
	// runtime_spec.endpoint_mode_invalid or runtime_spec.flavor_invalid. The
	// one check it still precedes is capture.SealSecret's keyless-store
	// rejection, which is write-path preparation rather than a request
	// validation -- the same line the application write paths draw.
	//
	// One sentinel, 400 in every shape. Because the document always carries
	// the Type/Binary the kind is resolved from, the refused kind is always one
	// THIS request supplied, so there is no well-formed-but-conflicting shape
	// here for the application side's 409 sentinel to answer. hadExisting is
	// read below for the default-versus-preserve decision only, never here.
	effectiveSpecKind := routing.EffectiveRuntimeSpecType(routing.RuntimeSpec{Type: specType, Binary: binary})
	liveTimingsCapable := routing.LiveTimingsCapableKind(string(effectiveSpecKind))
	if req.ResponsesLiveTimingsEnabled != nil && *req.ResponsesLiveTimingsEnabled && !liveTimingsCapable {
		return RuntimeSpecDTO{}, fmt.Errorf("%w: responses_live_timings_enabled cannot be true for a runtime spec of effective type %q",
			ErrRuntimeSpecResponsesLiveTimingsUnsupported, string(effectiveSpecKind))
	}
```

- [ ] **Step 8: Resolve the stored value.**

The `existing, hadExisting, err := s.routes.RuntimeSpecByMapping(ctx, mapping.ID)` read is at `:711`. Immediately after the `measuredByIndex` block that consumes `hadExisting` — anywhere after `:711` and before the `spec := routing.RuntimeSpec{…}` literal at `:776` — add. (`liveTimingsCapable` is Step 7's local: `putRuntimeSpec` is one function, so it is still in scope here, and the refusal deliberately stays up in the validate-before-mutate block while the resolution lives down beside the `hadExisting` read it needs.)

```go
	// The live-timings opt-in. !hadExisting is what "create" means on a
	// full-document upsert, so a first write takes the kind-dependent default
	// while a later save of an existing spec that omits the key keeps the
	// stored value -- re-saving a llama.cpp spec never re-enables a flag the
	// operator turned off, nor clears one they turned on.
	//
	// The two arms below are the pointer's whole point. A non-nil value is the
	// caller's, already validated above (a true here implies a capable kind,
	// or putRuntimeSpec has already returned). A nil against a document whose
	// kind cannot honour the flag CLEARS it, so a PUT that retypes an
	// llama_cpp spec to ollama cannot leave a stale true behind -- and since
	// the caller said nothing about the flag, the clear contradicts nothing
	// they asked for. That asymmetry is decision (e): the assertion is
	// refused, the non-mention is normalised.
	//
	// liveTimingsCapable is Step 7's local, computed from the EFFECTIVE kind.
	// Do not recompute it from specType here: that is how an auto-detect
	// llama-server spec ends up defaulting off.
	liveTimings := liveTimingsCapable
	if hadExisting {
		liveTimings = existing.ResponsesLiveTimingsEnabled
	}
	switch {
	case req.ResponsesLiveTimingsEnabled != nil:
		liveTimings = *req.ResponsesLiveTimingsEnabled
	case !liveTimingsCapable:
		liveTimings = false
	}
```

Then in the `routing.RuntimeSpec` literal, after `MessagesMode: msgMode,` (`:801`):

```go
		ResponsesLiveTimingsEnabled: liveTimings,
```

- [ ] **Step 9: The DTO mapper.**

In `runtimeSpecDTO`, after `MessagesMode: string(spec.MessagesMode),` (`:1180`):

```go
		ResponsesLiveTimingsEnabled: spec.ResponsesLiveTimingsEnabled,
```

`GetRuntimeSpec`'s not-yet-configured DTO literal (`:511-525`) needs **no** entry: it sets neither `ResponsesMode` nor `MessagesMode` either, and the new boolean's zero value there is the honest answer for a spec that does not exist yet.

- [ ] **Step 10: The status mapping — ONE row, at 400.**

In `internal/gateway/portal_runtime_endpoints.go`, insert one row into `portalRuntimeSpecErrRows` **immediately after** `ErrRuntimeSpecTypeInvalid`'s (`:84`) — not appended at the table's end (`:105`, the `ErrRuntimeSpecServerBenchmarking` row, with `:106` its closing `}`). The table is not sorted, so either position works mechanically; pick this one so the row sits beside the other type-shaped refusal. One row, because this side has one sentinel: unlike Task 6's table this one gets **no** 409 entry, since no spec write can reach the stored-state shape. (There is already an unrelated 409 in this table — `ErrRuntimeSpecServerBenchmarking` at `:105`, refused because a benchmark run is holding the server — so a 409 here is not novel; it is simply not this sentinel's answer.)

```go
	// The one row in this table with a dynamic message. The service wraps the
	// sentinel with the offending EFFECTIVE kind (fmt.Errorf("%w: ...")), and
	// naming it is the entire point of refusing the write instead of storing
	// something else -- doubly so here, where an empty "type" means the kind
	// was detected from the binary and the caller never typed it. msgFn exists
	// for exactly this ("a row that must surface the underlying error's own
	// text", error_map.go:15-18); the sentinel's own text IS the API code, per
	// the convention above, so it is trimmed rather than repeated.
	//
	// 400 and never 409: the application table splits this refusal in two
	// (portal_application_endpoints.go) because a PATCH can assert the flag
	// against a type it never sent, and the request is then well-formed. A
	// spec PUT always states its own Type and Binary, so the kind named here
	// always came from this body.
	{
		err:    portal.ErrRuntimeSpecResponsesLiveTimingsUnsupported,
		status: http.StatusBadRequest,
		code:   "runtime_spec.responses_live_timings_unsupported",
		msgFn: func(err error) string {
			return strings.TrimPrefix(err.Error(), portal.ErrRuntimeSpecResponsesLiveTimingsUnsupported.Error()+": ")
		},
	},
```

`strings` (`:14`) and `net/http` (`:9`) are already imported by this file; `errRow`'s `msgFn` field is at `error_map.go:24` and is honoured at `:72-74` (`:71` is the static-`msg` default it displaces). Without this row the sentinel falls through to the 500 `runtime_spec.request_failed` fallback.

- [ ] **Step 11: `putRequestFromDTO` — the spread.**

In `service_runtime_benchmark.go`, after `MessagesMode: dto.MessagesMode,` (`:51`):

```go
		// A *bool on the request against a bool on the DTO, so this is a
		// conversion rather than a copy -- the one line in this spread that is
		// not a straight assignment. A loaded document always HAS an opinion,
		// so an explicit pointer is the honest spread of it; a nil would be
		// this mapper inventing a non-mention the document does not contain.
		//
		// In the ORDINARY case the pointer changes nothing, and the comment
		// says so rather than claiming a danger it averts: this mapper's only
		// caller resolved the spec by id before calling it
		// (SetBenchmarkRuntimeSpecAdminState, which returns
		// ErrRuntimeSpecNotFound when there is no such row), so putRuntimeSpec
		// always finds an existing spec and a nil would take the PRESERVE
		// branch -- putting back exactly what was read. The create-time default
		// is unreachable from here.
		//
		// What the pointer changes is the ONE pathological case: a stored true
		// on a kind that cannot honour it. A nil would let the clear arm wipe
		// it silently; the explicit pointer re-ASSERTS it and earns the 400
		// instead -- a failure worth hearing about rather than papering over.
		// Re-asserting is safe because the spread restates Type and Binary
		// from the same document (SetBenchmarkRuntimeSpecAdminState re-reads
		// the whole document through this spread and then replaces
		// AdminState alone), so the kind the value was stored under is the
		// kind it is re-asserted against, and a 400 out of the restore means
		// the STORED row already violated the invariant.
		ResponsesLiveTimingsEnabled: &liveTimings,
```

and, at the top of the function body, before the `return`:

```go
	liveTimings := dto.ResponsesLiveTimingsEnabled
```

(A local rather than `&dto.ResponsesLiveTimingsEnabled`, so the returned request does not alias the caller's argument.) The function is currently a single `return` statement; adding one statement before it is fine and keeps it a pure spread.

- [ ] **Step 12: Run the tests and watch them pass.**

```bash
cd /Users/jlor08/Developer/codex/op-ai-gateway/.worktrees/responses-live-timings/gateway/backend
go test ./internal/portal/ -run 'RuntimeSpec|PutRequestFromDTO' -count=1 -v
go test ./internal/portal/ ./internal/gateway/ -count=1
```
The first command's `-run` filter is what makes this step mean something: besides this task's nine new tests and `TestPutRuntimeSpecDoesNotInheritAppModes` (`service_runtime_test.go:344`) it also runs, by name, the eight tests that drive `putRequestFromDTO` — the five `TestSetBenchmarkRuntimeSpecAdminState*` (`service_runtime_benchmark_test.go:229`, `:273`, `:306`, `:321`, `:364`) and the three `TestPutRuntimeSpec*` in `service_runtime_reserved_test.go` (`:32`, `:67`, `:81`). All must be green, and two of them are load-bearing rather than incidental:

- `TestSetBenchmarkRuntimeSpecAdminStateIsAFullDocumentWriteThatNotifies` `reflect.DeepEqual`s the **whole** returned DTO against the stored one (`service_runtime_benchmark_test.go:249`), so it is the test that reports a restore which *changes* the flag — an `&false` out of Step 11's conversion, say. It does **not** report a `nil`: `benchmarkSpecFixture` (`:18-58`) writes its spec with `Binary: "/usr/local/bin/llama-server"` and no `Type`, so the stored value is `true` (capable kind, create default), `hadExisting` is true on the restore, and Step 8's preserve branch answers the same `true` that a `nil` and an explicit `&true` both produce. That is precisely why 13(a) has to be measured through `TestPutRequestFromDTOCoversEveryWritableField`'s `DeepEqual` and not here.
- `TestPutRuntimeSpecIsUngatedOnAnotherServersRun` and `…IsUngatedWithNoHook` PUT that same spread back and require **no error at all**, so they are where a refusal asked about the raw `req.Type` announces itself: the fixture's stored `Type` is `""`, `putRequestFromDTO` restates it verbatim alongside the `&true`, and `LiveTimingsCapableKind("")` is false. (`TestPutRuntimeSpecRefusesWhileABenchmarkHoldsTheServer` is *not* in this group — its reservation gate lives in `PutRuntimeSpec`, above the shared body, so it never reaches the refusal.) Mutation 13(e) fails these two plus all five benchmark tests, which is a far louder signal than the three auto-detect tests alone.

- [ ] **Step 13: Mutations.**

(a) Delete **both** of Step 11's lines — the `ResponsesLiveTimingsEnabled: &liveTimings,` entry *and* the `liveTimings := dto.ResponsesLiveTimingsEnabled` local above it. Dropping only the entry leaves the local unused, which is a compile error and not a test failure. Expected: `TestPutRequestFromDTOCoversEveryWritableField` fails **once**, on the `reflect.DeepEqual` (`service_runtime_benchmark_test.go:202-204`), `nil` against `&true`. Not twice: the two tag-name loops (`:126-135`) compare the two structs' field sets by reflection and a missing mapper line changes neither struct, so they stay green. Nothing else in the suite fails either — the benchmark restore's `hadExisting` preserve branch answers the same value a `nil` produces (Step 12) — which is exactly why this `DeepEqual` is the guard the applications side does not have. Restore.
(b) Delete the `if hadExisting { liveTimings = existing.… }` branch — test 3 fails, and only test 3 (its first leg: an absent key after an explicit `false` on a capable kind re-reads the `true` default). Restore.
(c) Delete the refusal from Step 7 **and reorder Step 8's two switch arms** so `case !liveTimingsCapable:` runs ahead of `case req.ResponsesLiveTimingsEnabled != nil:` — the silent normalisation this task replaced. Without the reorder the value arm wins and the explicit `true` is *stored as `true`*, which is a third, different wrong behaviour rather than the one being ruled out; Task 6's implementer had to make exactly this reorder to measure its own equivalent. Expected either way: test 6 reports **two** failures per leg in one run — the error assertion (`err = <nil>`) and the state assertion (leg 1's reload says `Type == "ollama"` and the flag was overwritten; leg 2's says `Configured == true`) — which is what the `t.Errorf` shape mandated in Step 1 buys, and the reason it is mandated. Restore. This mutation is the decision itself; record its output verbatim in the commit body.
(d) Delete the `case !liveTimingsCapable:` arm — test 7 fails and only test 7 (test 1's `ollama` case and test 2's coherence leg still answer `false` from the seed). Restore. With (c) this is a pair proving the two halves are independent: (c) shows the assertion must be refused, (d) shows the non-mention must still be cleared.
(e) **The resolver mutation, and the one most worth recording.** Replace both uses of the effective kind with the raw `specType` — `routing.LiveTimingsCapableKind(specType)` in Step 7's condition and in Step 8's `liveTimings := …` seed. Test 5's third leg fails (an explicit `true` on a `Type: ""` llama-server spec is wrongly refused, since `LiveTimingsCapableKind("")` is false), test 1's second case fails (the same spec's absent-key default comes out `false`), and Step 2's `…OnAnAutoDetectCapableKindIsAccepted` wire test answers a 400 where a 200 belongs. **And seven tests this task did not write fail with them**, which is the loudest half of the signal: all five `TestSetBenchmarkRuntimeSpecAdminState*` plus `TestPutRuntimeSpecIsUngatedOnAnotherServersRun` and `…IsUngatedWithNoHook`, because `benchmarkSpecFixture`'s stored `Type` is `""` and `putRequestFromDTO` replays it alongside an explicit `true` (Step 12). Note what does **not** fail: every explicitly-typed case, capable and incapable alike, and every refusal test — which is exactly why this needs a mutation rather than a code review. Restore.
(f) Delete the `runtimeSpecDTO` line — **all eight** of Step 1's DTO-reading tests fail (1 through 8). The mapper is on every DTO echo *and* on every `GetRuntimeSpec` reload, because `putRuntimeSpec` returns `runtimeSpecDTO(spec, storedGPUs, app)` (`:833`), so every assertion of `true` in the list goes `false` — including test 7's and test 6's preconditions. Restore. This is deliberately a broad signal, unlike (d); test 8's own claim is only that it is the one test reading a spec written outside the service.
(g) Delete the `errRow` from Step 10 — the incapable-kind wire test fails on its **status** assertion: `status = 500, want 400, body = {"error":{"code":"runtime_spec.request_failed",…}}`. Not on the code assertion — `rec.Code` is checked two lines above it with `t.Fatalf`, so the test aborts before the code is ever read; the code is visible only in the body dump that status line prints. Restore.
(h) Change Step 10's row from `http.StatusBadRequest` to `http.StatusConflict` — that same status assertion fails, `status = 409, want 400`. Restore. This is the pin on "a spec refusal is 400, not 409": the code would still match, so only the status catches it.
(i) **The pointer's semantics, measured with a production-only edit.** Swapping `*bool` for `bool` on `PutRuntimeSpecRequest` is a compile failure *in the test file* (`boolPtr(...)` into a `bool` field), which the discipline forbids — Task 6 measured that and recorded it. Replace Step 8's whole resolution with the plain-`bool` equivalent instead: `liveTimings := req.ResponsesLiveTimingsEnabled != nil && *req.ResponsesLiveTimingsEnabled`, dropping the `hadExisting` branch and both switch arms, so absent ⇒ `false` and neither the default nor the preserve can ever fire. Expected: test 1's `llama_cpp` and auto-detect cases, test 2's pin leg, test 3's second preserve leg and test 4's absent leg fail; tests 5, 6, 7, 8 and 9 and every explicit-`false` assertion stay green — which is the point, because absent and `false` have become the same request. (`liveTimingsCapable` stays used by Step 7's condition, so this compiles.) Restore.
(j) **The position.** Move Step 7's block back to immediately after the `validRuntimeSpecType` check (`:634`) — test 9 fails on all three legs, and only test 9; every other test in the suite stays green, which is what makes the position invisible to a code review. Task 6 measured the same shape on the application paths: "only the two new precedence tests; everything else green". Restore.
(k) Drop one character from `RuntimeSpecDTO`'s `json:"responses_live_timings_enabled"` tag — only Step 2's auto-detect wire test fails, on its echo assertion, because it is the only test that decodes the key **by name** out of a real response body. The whole `internal/portal` package still prints `ok`: those tests read the Go field, which the typo does not touch. Restore. (Task 6's fix round added the application-side equivalent for exactly this reason.)
Record each.

- [ ] **Step 14: The wire contract in `api-surface.md`.**

Under `#### API-variant endpoint modes (responses_mode / messages_mode)` (`:494`), after the three existing "Wire notes a client must know" bullets (`:510-529`), add one bullet block for the new field. It must state: the JSON key and that it appears on `ApplicationDTO`/`CreateApplicationRequest`/`UpdateApplicationRequest` **and** `RuntimeSpecDTO`/`PutRuntimeSpecRequest`; that it is a `*bool` on all three request shapes, where **absent is not the same as false** — absent on a create (or a first spec write) gets the kind-dependent default, `true` for `llama_cpp`/`vllm` and `false` for every other kind, while an explicit `false` is a deliberate off; that absent on an update keeps the stored value, except that it is **cleared** whenever the *resulting* type cannot honour the flag — which is a property of the row the write leaves behind, not of a retype: an application PATCH that sends no `type` at all still clears a stored `true` when the application's own type is incapable (Task 6's clear arm reads the post-mutation `app.Type`, unguarded by `req.Type != nil`, and `TestUpdateApplicationLiveTimingsClearIsAPropertyOfTheStoredRowNotTheRequest` pins exactly that), and a spec PUT clears one whenever its effective type is not capable; that an explicit `true` against such a resulting type is **refused**, not stored as `false`, so a caller always ends up with the value it asked for or an error saying which kind refused it; that the refusal's status depends on where the offending type came from — **400** when the request supplied it (every create, an application PATCH that also sends `type`, and every runtime-spec PUT, which always restates its own `type`/`binary`) and **409** on the one shape where it did not, an application PATCH that asserts the flag while sending no `type`; that it is **orthogonal** to `responses_mode` rather than a fourth value of it; and that it is offered on the Responses side only and never for `/v1/messages`.

Then add **three** new rows to the "New stable error codes" table (`:533-539`: header `:533`, separator `:534`, and **five** existing rows at `:535-539`), appending **after** the `runtime_spec.flavor_invalid` row at `:539`. "After `:538`" would split the existing rows and land the new three between `runtime_spec.endpoint_mode_invalid` and `runtime_spec.flavor_invalid`. It is a `| Code | Status | Source |` table — keep that column shape and that one-sentence-in-the-Source-cell style:

- `application.responses_live_timings_unsupported` — **400** — the request sent an incapable `type` alongside `responses_live_timings_enabled: true` (every create, whose `type` is always the request's own; a PATCH that sends both). The message names the type.
- `application.responses_live_timings_conflict` — **409** — a PATCH asserts `responses_live_timings_enabled: true`, sends no `type`, and the application's **stored** type cannot honour it: the request is well-formed and conflicts with the application's own state. The message names that stored type.
- `runtime_spec.responses_live_timings_unsupported` — **400** — a spec PUT asserts `true` and its **effective** type — the explicit `type` when set, else detected from `binary` — cannot honour it. The message names the effective type, which may be one the caller never typed. There is no 409 on this surface: a spec PUT is a full document and always carries the type it is judged against.

Then fix the two sentences the new rows falsify — this is not optional tidying, it is the same hazard as `data-model.md`'s "(79 migrations)" heading in Task 1, a number in prose that a change silently turns false:

- `:541` reads "All five codes above are wired end to end and answer the listed status." Three new rows make that eight; make it "All eight codes above".
- `:542-546` names the three validation sentinels (`ErrApplicationEndpointModeInvalid`, `ErrRuntimeSpecEndpointModeInvalid`, `ErrRuntimeSpecFlavorInvalid`) and says where each is mapped — a passage whose subject is the original five. Add one sentence after `:546` for the new three: `internal/portal.Service` returns `ErrApplicationResponsesLiveTimingsUnsupported` and `ErrApplicationResponsesLiveTimingsConflict` (Task 6) and `ErrRuntimeSpecResponsesLiveTimingsUnsupported`, mapped in `portalApplicationErrRows` (400 and 409) and `portalRuntimeSpecErrRows` (400) — each with `msgFn`, so the message names the offending type rather than a static string.

`./scripts/check-docs.sh` checks no counts and no completeness (its own header puts prose style out of scope), so nothing but a reader catches "five".

**Anchors.** `check-docs.sh`'s check 1 resolves every intra-repo markdown link **including its `#anchor`**, slugged from the target document's own headings. This passage is dense with exactly that kind of link (`:504-507`, `:527-528`), so prose written in the surrounding style will very likely contain one — and a mis-slugged anchor fails the gate. Any cross-reference the new text adds must resolve file **and** anchor.

(An earlier draft of this plan said no error code was needed at all — that was the silent-normalisation variant — and the draft after it put both application rows at 400; both are superseded.)

- [ ] **Step 15: The spec-snapshot note in `agent-runtime-manager.md`.**

The passage runs `:3733-3759` and has two natural seams: `:3737-3744` is where `RuntimeAdminSection` is said to render `spec.api_flavors`/`responses_mode`/`messages_mode` as the **sole** authority for that model's Codex/Claude Code endpoints, and `:3745-3759` is the "**Snapshot, not inheritance**" paragraph. The first sentence belongs at the end of the first; the write-rule sentence after it. Add one sentence: `responses_live_timings_enabled` joins that per-spec set (migration 80), and for a `server_agent` model the resolved spec's copy is what the request path reads — with the reason: the flag qualifies `responses_mode`, which for a managed model comes from the spec, so reading the parent application's copy would attach the flag to a decision the application never made. Add a second sentence for the write rule, because this is the page an operator-facing reader lands on: a PUT that sets it `true` on a spec whose **effective** type (the explicit `type`, else detected from `binary`) is not `llama_cpp`/`vllm` is refused with **400** — never 409, because the document always carries the type it is judged against — and a PUT that omits it on such a type clears any stored `true`. State also that no operator control for it ships in this cut. Same anchor rule as Step 14 — this passage's own links (`:3743-3744`, `:3746-3747`) show the style, and a mis-slugged `#anchor` fails `check-docs.sh`'s check 1.

- [ ] **Step 16: Docs and Go gates, then commit.**

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
(The first grep excludes `_test.go`, so this task's own wire test does not trip it; the three `errRow`s added across Tasks 6 and 7 name the **sentinel**, not the field, so they do not either. Nothing in `internal/gateway` reads the boolean for a request decision, which is what the check is for.)

Commit body: the four-way resolution, why `!hadExisting` is what "create" means here, why the rule is judged over the resulting document rather than a transition, why that same property makes every spec refusal a 400 and leaves this side with one sentinel where the application side has two, why the predicate is asked about the effective type and not `req.Type`, the no-inheritance contract, why the refusal is LAST in the validate block and what that costs (the one residue above `capture.SealSecret`), the pointer conversion in the spread and the one pathological case it earns its place on, the eleven mutations, and the two doc files.

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
| A plain `bool` kills the kind-dependent create default AND the assertion-vs-non-mention rule | Task 6 Steps 5, 12(b′); Task 7 Steps 1(test 4), 5, 13(i) — a type swap is a compile failure in the test file, so both tasks measure the behavioural equivalent with a production-only edit |
| `putRequestFromDTO` compiles without the new field | Task 7 Steps 1, 11, 13(a) |
| Hand-written DTO mappers, no coverage test on the application side | Task 6 Steps 1(test 12), 9, 12(a); Task 7 Steps 1(test 8), 9, 13(f) |
| A `server_agent` application's create-time default can only be `false` | Task 5 Step 1 (the predicate row); Task 6 Step 1(test 1), 7; Task 7 Step 1(test 2) |
| The retype path is undefined | Task 6 Steps 1(tests 7, 8, 9, 10, 11), 8; Task 7 Steps 1(tests 6, 7), 7, 8 |
| An impossible `true` must be REFUSED, not silently stored as `false` (decision (e), settled 2026-09-11) | Global Constraints; Task 6 Steps 1(tests 4, 8, 9, 10), 6, 7, 8, 12(c); Task 7 Steps 1(test 6), 6, 7, 13(c) |
| The refusal's status is SPLIT: 400 when the request supplied the incapable type, 409 when it came from stored state (refined 2026-09-11, superseding the single-400 wording) | Global Constraints; Task 6 Steps 1(tests 4, 8, 9, 10), 2, 6, 8, 10, 12(g), 12(h); Task 7's "why the rule is phrased over the RESULTING type" (why no 409 exists there), Steps 2, 6, 10, 13(h) |
| `mock` is not an accepted application type, so it is unreachable from create and must not be implied by the 400's message | Task 6 Interfaces; Steps 1(test 1), 7 |
| `validRuntimeSpecType` accepts `""` and the capable predicate is false for it, so a spec check must ask `EffectiveRuntimeSpecType` | Task 7 Interfaces; the rule block; Steps 2, 7, 8, 13(e) |
| A new sentinel absent from the error-row table falls through to a 500 | Task 6 Steps 2, 10, 12(f); Task 7 Steps 2, 10, 13(g) |
| A spec PUT has no retype signal, so the rule must read the resulting document | Task 7's "why the rule is phrased over the RESULTING type"; Steps 7, 13(e) |
| Two hand-maintained "79 migrations" prose counts, no test | Task 1 Step 9 |
| `data-model.md` §4 heading rename breaks seven anchors | Task 1 Steps 9, 10, 11 |
| gofumpt re-aligns a struct-tag column on every field insert | Global Constraints; every task's gate step |
| A second hand-written capable-kind list would drift from the gate's | Task 5 Steps 5, 6 |
| `PutRuntimeSpecRequest` derived by TS `Omit` → a required request field | **Part 2** — declared out of scope in Global Constraints |
| Frontend and backend defaults both claiming authority (`applicationTypeDefaults.ts`) | **Part 2** — declared out of scope in Global Constraints |
| The portal form restates every field, so a retype would earn decision (e)'s 400 (always the 400, never the 409: `buildBody()` restates `type` on every save) | **Part 2** — see "What part 1 deliberately leaves on the table" |
| Task 5's capable-set size pin forces a look at the gate's list but proves no agreement; Task 6 is the first production caller, so an exported set would stop being test-only API | **Optional in Task 6** — recorded under its rule block, deliberately not a requirement |

## What part 1 deliberately leaves on the table

These are named so a later reader does not mistake them for oversights:

- **No operator control, no TypeScript, no i18n.** A visible toggle that does nothing would be worse than the blank cell it promises to fix.
- **Decision (e)'s 400 has a consequence part 2 must design around, and this is the note that says so.** `gateway/frontend/src/components/ApplicationSection.tsx`'s `buildBody()` (`:381-434`) is ONE literal reused verbatim for create and update, so a field added there is restated on every save — and a save that retypes the application would then *assert* `responses_live_timings_enabled` against the new type and earn a `400 application.responses_live_timings_unsupported`. Always that one, never the 409 sibling: the same literal restates `type` on every save (`:391`), so the portal's request always supplies the type it is judged against, which is precisely the 400 shape. The control must therefore gate **what it sends**, not only what it renders. The shape to copy is already in that file, for `proxy_excluded` and for exactly this class of reason: the create path sends the key only when the control was rendered (`...(showProxyControls ? { proxy_excluded: proxyExcluded } : {})`, `:425`) and the update path deletes it when it has not changed from the value captured as the form OPENED (`if (proxyExcluded === proxyExcludedSeed) delete body.proxy_excluded;`, `:461`, whose own comment explains why sending it unconditionally "would compile, pass a 'the switch works' test, and still be a defect"). `RuntimeAdminSection.tsx`'s spec form needs the same care for a different reason: a spec PUT is a full document, so it always restates `type`/`binary` alongside the flag. The gain that pays for this care: because a stored `true` can never be inert, the control needs no second indicator explaining why a switch that is on is doing nothing.
- **No gate, no injection, no retry, no capture change.** `wantsLiveProgress` and `liveProgressMemo` are unexported in `internal/provider` and the memo hangs off `*OpenAICompatibleClient` while the gateway holds a `provider.Client`; that package-boundary decision is part 2's and is not prejudged here.
- **The non-injection prose stays as written.** `telemetry-usage-observability.md` §8.4.3 states it twice — at `:755-766` ("`timings_per_token` is READ when the client set it, and never injected", framed as one of "Two rules on this path must survive any later change") and again in the three-layer-rule passage further down the same long section, at `:1536-1550` ("Native passthrough gets neither parameter") — and `compatibility-and-inference.md` §6 states it a third time, at `:263-267` and `:370-386`. After part 1 all three are still **true**: nothing injects anything. Reversing them belongs in the commit that reverses the behaviour, together with the ADR (next number: ADR-041; ADR-030 at `09-architecture-decisions.md:306` is its closest structural precedent) if one is written. Note for part 2: `docs/architecture/reference/data-model.md` §4's anchor will by then read `#4-migration-history-80-migrations`.
- **The two vacuous "must not grow a `timings_per_token` flag" assertions** (`internal/gateway/passthrough_progress_test.go:292`, `:623`) are left exactly as they are. They pass today and will pass after part 1; part 2 owns re-documenting which one is the negative case and why its fixture is incapable, and adding a genuinely capable fixture for the positive.
- **`predicted_n`** (an exact mid-stream count off the partials) stays out of scope and an open question on #81.
