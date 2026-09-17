# Usage Billing Unit Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give `usage_events` a non-token billable unit as a `(billing_unit, billing_quantity)` pair, so image/speech/transcription requests can later be metered truthfully — storage, energy semantics, aggregate honesty, portal presentation and the recorded decision, with no producer yet.

**Architecture:** Two additive columns land in migration v81 with defaults `''`/0 that make the change a truthful no-op over all history. `billing_unit == ""` means token-metered (the five token columns are the measure); a non-empty unit means `billing_quantity` is the measure and all seven token-denominated columns are 0. `modeledEnergy` gates on the unit and stamps a fourth terminal `energy_source` value instead of a "modeled" zero. Aggregates gain one count (`NonTokenRequests`) rather than a `GROUP BY` widening, so a mixed population is expressible without overloading `""`.

**Tech Stack:** Go 1.26 (backend module `op-ai-gateway`), React + TypeScript + Vite + MUI (portal), SQLite (`modernc.org/sqlite`) / PostgreSQL (`jackc/pgx/v5`) / in-memory store triple, Vitest, Playwright.

## Global Constraints

- Design source of truth: `docs/superpowers/specs/2026-09-17-usage-billing-unit-design.md`. Read it before starting; it carries the reasoning this plan only executes.
- **Never commit to or merge into `main`.** All work is on branch `usage-billing-unit` in worktree `.worktrees/usage-billing-unit`. The merge is a human's, via pull request.
- Repo-facing text (commits, PR, issues, code comments, docs) is **English**.
- Commit message bodies must be substantive: this repo's GitHub squash-merge pre-fills the squash description from the commit body, not the PR body.
- `billing_quantity` is `double precision`, never `real` — a separate invariant asserts no `real` column survives a full chain.
- Never touch `baselineCreateStatements` (frozen as of v60) or `migration43FloatColumns` (frozen to v43's columns).
- Schema changes are append-only versioned migrations. `usage_events` has **no nullable columns**; every post-baseline column carries a default.
- The write path **never normalizes** `billing_unit`. There is no clamp-to-`""`: `""` is a positive assertion of token-metering, so a clamp would silently relabel.
- The unit comes from **endpoint identity**, never from a `provider.Response` (7 of the 10 `recordUsage` call sites pass a zero response).
- `internal/gateway/principal_limits.go` stays **byte-identical**. No second `admitPrincipal` call site — there is exactly one (`inference_handlers.go:517`).
- No endpoint is registered, no `sessionEndpoint` iota member is added. Those belong to #71/#68/#69.
- Portal localization: German and English keys added **together** in `gateway/frontend/src/i18n.ts`; the type-checked build enforces parity.
- Store changes must be verified with `OP_AI_GATEWAY_TEST_POSTGRES_DSN` set — the postgres subtests skip **silently** without it.
- Architecture docs are updated in this same branch (Task 6). `docs/superpowers/**` and `docs/implementation-status.md` are removed before the PR.
- All commands run from the worktree root `/Users/jlor08/Developer/codex/op-ai-gateway/.worktrees/usage-billing-unit` unless a step says otherwise.

---

## File Structure

**Backend — storage**
- `gateway/backend/internal/store/migrate.go` — the v81 registry entry and `migration81Up`.
- `gateway/backend/internal/usage/recorder.go` — `Event`'s two new fields (the wire + storage contract).
- `gateway/backend/internal/store/sqlite_usage.go` — eight coordinated SQL/scan sites, plus the two aggregate readers.
- `gateway/backend/internal/store/sqlite_usage_test.go` — the position-pinning round trip.
- `gateway/backend/internal/store/schema_column_coverage_test.go` — `usage_events` enrolment (the durable guard).
- `gateway/backend/internal/store/conformance_test.go` — the dual-dialect migration no-op and the mixed-unit aggregate twin.

**Backend — contract and producer seam**
- `gateway/backend/internal/usage/billing.go` *(new)* — the unit vocabulary, `ValidBillingUnit`, `ValidateBillingXOR`. One responsibility: what a billable unit is and what makes a row self-consistent. Kept out of `stats.go` because `stats.go` owns query normalization, not the storage contract.
- `gateway/backend/internal/usage/billing_test.go` *(new)*.
- `gateway/backend/internal/gateway/inference_complete.go` — `usageMeta`'s two new fields, the `usage.Event` literal, the XOR observability call.

**Backend — energy**
- `gateway/backend/internal/gateway/energy_engine.go` — the `energySourceUnpriceable` const and the `modeledEnergy` guard.
- `gateway/backend/internal/gateway/energy_reconciler.go` — the tightened calibration guard.

**Backend — aggregates**
- `gateway/backend/internal/usage/query.go` — `NonTokenRequests` on `StatTotals` and `GroupBucket`.
- `gateway/backend/internal/usage/recorder.go` — the memory `Stats`/`UsageGroups` accumulation.
- `gateway/backend/internal/portal/service_usage_groups.go` — `UsageGroupDTO` and the fold.
- `gateway/backend/internal/portal/service_projects.go` — the project rollups.
- `gateway/backend/internal/portal/service.go` — the Dashboard's 24h metrics.

**Frontend**
- `gateway/frontend/src/components/billingUnit.ts` *(new)* — the three-state aggregate rule and the two tooltip tables. One focused module so five surfaces share one implementation instead of five copies of the same conditional.
- `gateway/frontend/src/components/billingUnit.test.ts` *(new)*.
- `gateway/frontend/src/api/usage.ts` — `UsageEvent`, `UsageGroupRow`, `StatTotals`, `DashboardMetrics` types.
- `gateway/frontend/src/components/activityColumns.ts` — two new column definitions.
- `gateway/frontend/src/components/ActivityTable.tsx` — `renderCell`: five token cells, two new cells, the `energy_source` tooltip.
- `gateway/frontend/src/components/ActivityGroups.tsx` — `GroupColId`, `GROUP_COLUMNS`, `cellValue`, the expanded member table.
- `gateway/frontend/src/components/StatTiles.tsx` — four token tiles, `formatEnergyWh(0)`.
- `gateway/frontend/src/components/Dashboard.tsx` — the `tokens24h` tile.
- `gateway/frontend/src/i18n.ts` — de + en keys.

**Docs (Task 6)**
- `docs/architecture/cross-cutting/telemetry-usage-observability.md` (§8.4.1, §8.4.4, §8.4.5)
- `docs/architecture/reference/data-model.md` (field list, migration count, v81 row)
- `docs/architecture/09-architecture-decisions.md` (ADR-041)
- `docs/architecture/cross-cutting/persistence.md` (migration count)
- `docs/architecture/11-risks-and-technical-debt.md` (§11.1, §11.4)
- `docs/architecture/cross-cutting/compatibility-and-inference.md` (the recording-on-rejection correction)

---

## Task 1: Storage — the column pair and the eight-site lockstep

**Files:**
- Modify: `gateway/backend/internal/store/migrate.go:115` (registry) and end of file (`migration81Up`)
- Modify: `gateway/backend/internal/usage/recorder.go:86-88` (after `EnergySource`)
- Modify: `gateway/backend/internal/store/sqlite_usage.go` — `:60-65`, `:66`, `:67-105`, `:202-208`, `:227-231`, `:335-389`, `:393-397`, `:666-721`
- Test: `gateway/backend/internal/store/sqlite_usage_test.go`, `gateway/backend/internal/store/conformance_test.go`, `gateway/backend/internal/store/schema_column_coverage_test.go`

**Interfaces:**
- Consumes: nothing (first task).
- Produces: `usage.Event.BillingUnit string` (json `billing_unit`) and `usage.Event.BillingQuantity float64` (json `billing_quantity`); migration version 81 named `usage_events_billing_unit`. Every later task relies on these exact names.

- [ ] **Step 1: Write the failing round-trip test**

Add to `gateway/backend/internal/store/sqlite_usage_test.go`. It seeds **distinct** values into the new pair *and* its same-kind neighbours, because the silent failure mode is a swap against a same-kind column (a `float64` into `energy_wh`'s slot, a `string` into `token_name`'s), not between the two new columns:

```go
func TestSQLiteUsageStorePersistsBillingPairByPosition(t *testing.T) {
	store := newTestSQLiteStore(t)

	tokenMetered := usage.Event{
		ID: "req_tok", UserID: "u1", TokenID: "t1", Model: "m1", Host: "h1",
		TokenName: "token-name-sentinel", ServerName: "server-name-sentinel",
		InputTokens: 11, OutputTokens: 22, TotalTokens: 33,
		CachedTokens: 44, CacheWriteTokens: 55,
		PromptPerSecond: 1.5, TokensPerSecond: 2.5,
		EnergyWh: 3.5, EnergyMarginalWh: 4.5, EnergySource: "measured",
		LatencyMS: 120, Status: "success", HTTPStatus: 200,
		CreatedAt: time.Now().UTC().Truncate(time.Second),
	}
	nonToken := usage.Event{
		ID: "req_img", UserID: "u1", TokenID: "t1", Model: "m1", Host: "h1",
		TokenName: "other-token-name", ServerName: "other-server-name",
		PromptPerSecond: 0, TokensPerSecond: 0,
		EnergyWh: 6.5, EnergyMarginalWh: 7.5, EnergySource: "estimated",
		BillingUnit: "image", BillingQuantity: 3,
		LatencyMS: 900, Status: "success", HTTPStatus: 200,
		CreatedAt: time.Now().UTC().Truncate(time.Second),
	}

	for _, ev := range []usage.Event{tokenMetered, nonToken} {
		if err := store.Record(ev); err != nil {
			t.Fatalf("record %s: %v", ev.ID, err)
		}
	}

	byID := map[string]usage.Event{}
	for _, got := range store.All() {
		byID[got.ID] = got
	}
	// Whole-struct equality: any mis-positioned column shows up as a diff on a
	// NEIGHBOUR field, which is the failure this test exists to catch.
	if got := byID["req_tok"]; got != tokenMetered {
		t.Errorf("token-metered round trip mismatch:\n got %+v\nwant %+v", got, tokenMetered)
	}
	if got := byID["req_img"]; got != nonToken {
		t.Errorf("non-token round trip mismatch:\n got %+v\nwant %+v", got, nonToken)
	}

	// The same values must survive the Query path, whose SELECT concatenates
	// userNameExpr AFTER usageEventColumns -- the ordering trap in scanUsageRows.
	page, err := store.Query(usage.Query{ScopeAll: true, Limit: 25, Page: 1})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	for _, row := range page.Data {
		want := byID[row.Event.ID]
		if row.Event != want {
			t.Errorf("query row %s mismatch:\n got %+v\nwant %+v", row.Event.ID, row.Event, want)
		}
	}
}
```

If `newTestSQLiteStore` is not the helper name in this file, use whatever helper `TestSQLiteUsageStorePersistsProjectAttribution` (around `sqlite_usage_test.go:1021`) uses — read that test first and mirror its setup exactly.

- [ ] **Step 2: Run the test to verify it fails**

```bash
cd gateway/backend && go test ./internal/store/ -run TestSQLiteUsageStorePersistsBillingPairByPosition -v
```

Expected: FAIL to **compile** — `unknown field BillingUnit in struct literal of type usage.Event`.

- [ ] **Step 3: Add the two fields to `usage.Event`**

In `gateway/backend/internal/usage/recorder.go`, immediately after `EnergySource` and **before** `CreatedAt`:

```go
	// BillingUnit/BillingQuantity are the non-token billable measure, and they are
	// an XOR with the token columns: BillingUnit == "" means TOKEN-METERED (the
	// five token columns are the measure and BillingQuantity is meaningless),
	// while a non-empty BillingUnit means BillingQuantity is the measure and all
	// seven token-denominated columns (the five token counts plus PromptPerSecond
	// and TokensPerSecond) are 0. See ValidateBillingXOR.
	//
	// Both ARE real usage_events columns (unlike CostEUR below, which never is).
	// No producer writes a non-empty unit yet -- every event today carries ""/0,
	// which is what makes migration v81's defaults a truthful no-op over all
	// history. "" is a POSITIVE assertion of token-metering and is never inferred:
	// a unit may only ever come from endpoint identity, never from an upstream
	// response.
	BillingUnit     string  `json:"billing_unit"`
	BillingQuantity float64 `json:"billing_quantity"`
```

- [ ] **Step 4: Run the test to verify it now fails on data, not compilation**

```bash
cd gateway/backend && go test ./internal/store/ -run TestSQLiteUsageStorePersistsBillingPairByPosition -v
```

Expected: FAIL with a `no such column` / `SQLSTATE 42703` error from `Record`, or a round-trip mismatch showing `BillingUnit:""` where `"image"` was written. Either proves the storage layer is genuinely untouched.

- [ ] **Step 5: Add migration v81**

In `gateway/backend/internal/store/migrate.go`, append to the `migrations` slice after the v80 entry at `:115`:

```go
	{version: 81, name: "usage_events_billing_unit", up: migration81Up},
```

Then add `migration81Up` at the end of the file, following `migration72Up`'s two-columns-one-table loop shape:

```go
// migration81Up adds the non-token billable measure to usage_events as a
// (unit, quantity) PAIR: billing_unit says HOW to read the row, billing_quantity
// is the measure. The defaults are load-bearing rather than incidental --
// billing_unit '' means TOKEN-METERED, so every pre-existing row is backfilled
// truthfully and no LLM path changes at all.
//
// billing_quantity is 'double precision', not 'real': migration43FloatColumns is
// frozen to v43's columns and its own comment puts the type obligation on the
// later migration, and a separate invariant asserts no 'real' column survives a
// full chain. baselineCreateStatements is NOT touched (frozen as of v60), so the
// columns live only here -- the same discipline migration61Up follows.
func migration81Up(ctx context.Context, tx *sql.Tx, dl dialect) error {
	for _, col := range []string{
		"billing_unit text not null default ''",
		"billing_quantity double precision not null default 0",
	} {
		if err := addColumnIfMissing(ctx, tx, dl, "usage_events", col); err != nil {
			return err
		}
	}
	return nil
}
```

- [ ] **Step 6: Wire the eight lockstep sites in `sqlite_usage.go`**

All eight take the pair **last**, after `created_at`. Exact edits:

1. `Record`'s column list (`:60-65`) — the final line becomes:

```
			server_name, service_id, service_name, project_id, project_name, energy_wh, energy_marginal_wh, energy_source, created_at,
			billing_unit, billing_quantity
```

2. `Record`'s placeholder line (`:66`) — 39 `?` become **41**. Count them; a wrong count fails loudly.

3. `Record`'s positional args (`:67-105`) — after `event.CreatedAt,` add:

```go
		event.BillingUnit,
		event.BillingQuantity,
```

4. `ByUser`'s SELECT list (`:202-208`) — append `, billing_unit, billing_quantity` after `created_at`.

5. `All`'s SELECT list (`:227-231`) — the identical append. These two lists are textual duplicates of `usageEventColumns`, and nothing enforces that the three agree, so change all three in this step.

6. `usageEventColumns` (`:393-397`) — append `, e.billing_unit, e.billing_quantity` after `e.created_at`.

7. `scanUsageEvents` (`:335-389`) — after `&event.CreatedAt,` add:

```go
			&event.BillingUnit,
			&event.BillingQuantity,
```

8. `scanUsageRows` (`:666-721`) — after `&row.CreatedAt,` and **strictly before `&row.UserName`** add:

```go
			&row.BillingUnit,
			&row.BillingQuantity,
```

`Query` builds `usageEventColumns + ", " + userNameExpr`, so `&row.UserName` must remain the **last** scan target. Appending after it swaps the new columns with `user_name` on the `Query` path only — and that swap is silent for `billing_unit`, because both are strings.

`TimeSeries` (`:289-333`) builds its own 5-column SELECT and needs **no** change. `usageSortColumns` and `usageWhere` need no change: neither new column is sortable or filterable here.

- [ ] **Step 7: Run the test to verify it passes**

```bash
cd gateway/backend && go test ./internal/store/ -run TestSQLiteUsageStorePersistsBillingPairByPosition -v
```

Expected: PASS.

- [ ] **Step 8: Write the failing dual-dialect migration no-op test**

Add to `gateway/backend/internal/store/conformance_test.go`, using whatever `forEachDialect` helper the file already uses (read a neighbouring test to copy its harness exactly):

```go
// Migration v81's defaults must be a TRUTHFUL no-op: a row written before the
// billing pair existed reads back as token-metered, not as an unknown unit.
func TestMigration81BillingPairDefaultsAreANoOp(t *testing.T) {
	forEachDialect(t, func(t *testing.T, st *SQLStore) {
		if err := st.migrateTo(context.Background(), 80); err != nil {
			t.Fatalf("migrate to v80: %v", err)
		}
		if _, err := st.exec(context.Background(), `insert into usage_events
			(id, request_id, user_id, token_id, model, host, status, http_status,
			 input_tokens, output_tokens, total_tokens, latency_ms, created_at)
			values (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			"legacy1", "legacy1", "u1", "t1", "m1", "h1", "success", 200,
			10, 20, 30, 100, time.Now().UTC()); err != nil {
			t.Fatalf("seed pre-v81 row: %v", err)
		}

		if err := st.Migrate(context.Background()); err != nil {
			t.Fatalf("migrate to head: %v", err)
		}

		events := st.All()
		if len(events) != 1 {
			t.Fatalf("want 1 event, got %d", len(events))
		}
		got := events[0]
		if got.BillingUnit != "" {
			t.Errorf("BillingUnit = %q, want \"\" (token-metered)", got.BillingUnit)
		}
		if got.BillingQuantity != 0 {
			t.Errorf("BillingQuantity = %v, want 0", got.BillingQuantity)
		}
		if got.TotalTokens != 30 {
			t.Errorf("TotalTokens = %d, want 30 (the pre-existing measure must survive)", got.TotalTokens)
		}
	})
}
```

If the pre-v81 `insert` column list does not match the v80 schema exactly, read `baselineCreateStatements` plus the post-baseline `add column` calls and name only columns that exist at v80 — every `usage_events` column is `not null` **with a default**, so an explicit subset is legal.

- [ ] **Step 9: Run it on both dialects**

```bash
cd gateway/backend && go test ./internal/store/ -run TestMigration81BillingPairDefaultsAreANoOp -v
```

Then with the postgres leg actually provisioned — **the postgres subtests skip silently without this**, and a two-column migration is exactly the change that needs it:

```bash
docker run -d --name op-pg-test -e POSTGRES_PASSWORD=test -e POSTGRES_DB=optest -p 55432:5432 postgres:17
```

```bash
cd gateway/backend && OP_AI_GATEWAY_TEST_POSTGRES_DSN='postgres://postgres:test@127.0.0.1:55432/optest?sslmode=disable' go test ./internal/store/ -run 'TestMigration81BillingPairDefaultsAreANoOp|TestMigration43WidensRealToDoublePrecision' -v
```

Expected: PASS, and the output must show the postgres subtest **running**, not skipping. If it skips, the DSN is not reaching the test — fix that before continuing.

- [ ] **Step 10: Enrol `usage_events` in the column-coverage guard**

`usage_events` is not currently covered by `assertColumnCoverage` (`schema_column_coverage_test.go:49-91`, called only for `applications`, `ai_servers`, `agent_runtime_specs`), and no test anywhere asserts a `usage_events` column count. This is the durable half of the fix: it is the mechanism that makes the *next* column added to this table unable to silently miss a reader.

Read `assertColumnCoverage`'s signature and the three existing call sites, then add a `usage_events` call following the same shape. The readers to declare are `usageEventColumns` plus `Record`'s INSERT list; `request_id` is written and never read back, so it must be declared as a **known write-only** column in whatever mechanism that helper offers for exclusions. If the helper has no exclusion mechanism, add the narrowest one it needs (a documented allow-list parameter), because silently widening the helper's contract for other tables is out of scope.

- [ ] **Step 11: Run the store suite and the linters**

```bash
cd gateway/backend && go test ./internal/store/ ./internal/usage/ 2>&1 | tail -20
```

```bash
cd gateway/backend && golangci-lint fmt --diff && golangci-lint run ./internal/store/... ./internal/usage/...
```

Expected: all PASS, and `golangci-lint fmt --diff` prints nothing. CI runs `fmt --diff` and `run` per Go module (gofumpt, gocritic) — `gofmt`/`go vet` do not cover them.

- [ ] **Step 12: Commit**

```bash
git add gateway/backend/internal/store/migrate.go gateway/backend/internal/usage/recorder.go gateway/backend/internal/store/sqlite_usage.go gateway/backend/internal/store/sqlite_usage_test.go gateway/backend/internal/store/conformance_test.go gateway/backend/internal/store/schema_column_coverage_test.go
```

```bash
git commit -m "feat(store): Add the usage_events billing unit pair in migration v81

usage_events gains billing_unit (text not null default '') and
billing_quantity (double precision not null default 0), plus the matching
BillingUnit/BillingQuantity fields on usage.Event.

The defaults are the design, not an implementation detail: billing_unit ''
means TOKEN-METERED, so every pre-existing row is backfilled truthfully and
no LLM path changes. A dual-dialect test pins that no-op by seeding a row at
v80 and asserting it reads back ''/0 with its token measure intact.

double precision rather than real, because migration43FloatColumns is frozen
to v43's columns and a separate invariant asserts no real column survives a
full chain. baselineCreateStatements is untouched (frozen as of v60).

The write side is 39 columns wide and the read side 38 (request_id is
written and never selected), and ByUser's and All's lists are textual
duplicates of usageEventColumns that nothing enforces agreement between, so
all eight sites move together. In scanUsageRows the pair must precede
&row.UserName, because Query concatenates userNameExpr after the column
const -- appending after it would swap the new columns with user_name on the
Query path only, silently for the text column.

usage_events is also enrolled in the derive-from-live-schema column-coverage
test, which it was not part of before: no test asserted a usage_events
column count, so a column that missed a reader had nothing to fail."
```

---

## Task 2: The vocabulary, the producer seam, and the XOR invariant

**Files:**
- Create: `gateway/backend/internal/usage/billing.go`
- Create: `gateway/backend/internal/usage/billing_test.go`
- Modify: `gateway/backend/internal/gateway/inference_complete.go:630-634` (`usageMeta`) and `:663` (the `usage.Event` literal)
- Test: `gateway/backend/internal/gateway/inference_complete_test.go` (or the existing test file that exercises `recordUsage` — find it with `grep -rn 'recordUsage' gateway/backend/internal/gateway/*_test.go`)

**Interfaces:**
- Consumes: `usage.Event.BillingUnit`, `usage.Event.BillingQuantity` (Task 1).
- Produces: `usage.BillingUnitTokens` (`""`), `usage.BillingUnitImage` (`"image"`), `usage.BillingUnitAudioSecond` (`"audio_second"`), `func usage.ValidBillingUnit(string) bool`, `func usage.ValidateBillingXOR(Event) error`, and `usageMeta.BillingUnit string` / `usageMeta.BillingQuantity float64`. Task 3 depends on the constants; #71/#68/#69 depend on `usageMeta`.

- [ ] **Step 1: Write the failing vocabulary and XOR tests**

Create `gateway/backend/internal/usage/billing_test.go`:

```go
package usage

import (
	"errors"
	"testing"
)

func TestValidBillingUnit(t *testing.T) {
	for _, tc := range []struct {
		unit string
		want bool
	}{
		{BillingUnitTokens, true},
		{BillingUnitImage, true},
		{BillingUnitAudioSecond, true},
		{"images", false},
		{"IMAGE", false},
		{"character", false}, // no producer in this system: audio.cpp meters duration, not characters
		{"audio_minute", false},
	} {
		if got := ValidBillingUnit(tc.unit); got != tc.want {
			t.Errorf("ValidBillingUnit(%q) = %v, want %v", tc.unit, got, tc.want)
		}
	}
}

func TestValidateBillingXORAcceptsTokenMeteredAndNonToken(t *testing.T) {
	tokenMetered := Event{
		InputTokens: 10, OutputTokens: 20, TotalTokens: 30,
		CachedTokens: 1, CacheWriteTokens: 2,
		PromptPerSecond: 5, TokensPerSecond: 6,
	}
	if err := ValidateBillingXOR(tokenMetered); err != nil {
		t.Errorf("token-metered event rejected: %v", err)
	}

	nonToken := Event{BillingUnit: BillingUnitImage, BillingQuantity: 3}
	if err := ValidateBillingXOR(nonToken); err != nil {
		t.Errorf("non-token event rejected: %v", err)
	}
}

func TestValidateBillingXORRejectsAllSevenTokenColumns(t *testing.T) {
	// The five token counts PLUS the two per-second rates. The rates are NOT
	// among "the five token columns", and ComputeHistogram drops zeros by
	// design, so a stray non-zero rate on a non-token row would silently enter
	// the speed histograms with nothing to fail.
	for name, mutate := range map[string]func(*Event){
		"input_tokens":       func(e *Event) { e.InputTokens = 1 },
		"output_tokens":      func(e *Event) { e.OutputTokens = 1 },
		"total_tokens":       func(e *Event) { e.TotalTokens = 1 },
		"cached_tokens":      func(e *Event) { e.CachedTokens = 1 },
		"cache_write_tokens": func(e *Event) { e.CacheWriteTokens = 1 },
		"prompt_per_second":  func(e *Event) { e.PromptPerSecond = 0.1 },
		"tokens_per_second":  func(e *Event) { e.TokensPerSecond = 0.1 },
	} {
		ev := Event{BillingUnit: BillingUnitImage, BillingQuantity: 3}
		mutate(&ev)
		if err := ValidateBillingXOR(ev); !errors.Is(err, ErrBillingXORViolated) {
			t.Errorf("%s: err = %v, want ErrBillingXORViolated", name, err)
		}
	}
}

func TestValidateBillingXORRejectsUnknownUnitAndTokenMeteredQuantity(t *testing.T) {
	if err := ValidateBillingXOR(Event{BillingUnit: "images", BillingQuantity: 1}); !errors.Is(err, ErrBillingUnitUnknown) {
		t.Error("an unknown unit must be rejected, never clamped to \"\" -- \"\" is a positive assertion of token-metering")
	}
	if err := ValidateBillingXOR(Event{BillingQuantity: 5}); !errors.Is(err, ErrBillingXORViolated) {
		t.Error("a token-metered row with a quantity must be rejected")
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

```bash
cd gateway/backend && go test ./internal/usage/ -run 'TestValidBillingUnit|TestValidateBillingXOR' -v
```

Expected: FAIL to compile — `undefined: BillingUnitTokens`.

- [ ] **Step 3: Write `billing.go`**

```go
// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package usage

import (
	"errors"
	"fmt"
)

// The billable-unit vocabulary. An Event's measure is an XOR: BillingUnitTokens
// ("") means the five token columns are the measure, and any other value means
// BillingQuantity is the measure while every token-denominated column is 0.
//
// Two values, not three. "character" is deliberately absent: it comes from
// OpenAI's price list, not from this system -- audio.cpp's metering signal is an
// audio DURATION, so speech and transcription share BillingUnitAudioSecond.
const (
	// BillingUnitTokens is the historical default and a POSITIVE assertion that
	// the row is token-metered. It is never inferred and never defaulted TO from
	// an unknown value.
	BillingUnitTokens      = ""
	BillingUnitImage       = "image"
	BillingUnitAudioSecond = "audio_second"
)

var (
	// ErrBillingUnitUnknown reports a unit outside the vocabulary. Note that this
	// is an ERROR and not a clamp: unlike NormalizeSort's harmless default,
	// clamping to "" would assert token-metering about a request that is not
	// token-metered, which is the exact lie the (unit, quantity) pair exists to
	// prevent.
	ErrBillingUnitUnknown = errors.New("usage: unknown billing unit")
	// ErrBillingXORViolated reports a row carrying both a measure and a
	// contradicting one.
	ErrBillingXORViolated = errors.New("usage: billing unit/quantity XOR violated")
)

// ValidBillingUnit reports whether unit is in the vocabulary (including the
// token-metered ""). Producers use the constants above; this is for validating
// data that crossed a boundary.
func ValidBillingUnit(unit string) bool {
	switch unit {
	case BillingUnitTokens, BillingUnitImage, BillingUnitAudioSecond:
		return true
	default:
		return false
	}
}

// ValidateBillingXOR reports whether ev's measure is self-consistent.
//
// A token-metered row (BillingUnit == "") must carry no BillingQuantity. A
// non-token row must carry 0 in all SEVEN token-denominated columns: the five
// token counts plus PromptPerSecond and TokensPerSecond. The two rates are
// included deliberately -- they are not "token columns" in the narrow sense, but
// ComputeHistogram drops zeros by design, so a stray non-zero rate on a
// non-token row would enter the speed histograms with nothing to fail.
//
// This is the enforcement half of the contract. Without it, the energy engine's
// unit guard only picks a different wrong answer for a violating row instead of
// catching the violation.
func ValidateBillingXOR(ev Event) error {
	if !ValidBillingUnit(ev.BillingUnit) {
		return fmt.Errorf("%w: %q", ErrBillingUnitUnknown, ev.BillingUnit)
	}
	if ev.BillingUnit == BillingUnitTokens {
		if ev.BillingQuantity != 0 {
			return fmt.Errorf("%w: token-metered row carries billing_quantity %v", ErrBillingXORViolated, ev.BillingQuantity)
		}
		return nil
	}
	tokenColumns := []struct {
		name string
		zero bool
	}{
		{"input_tokens", ev.InputTokens == 0},
		{"output_tokens", ev.OutputTokens == 0},
		{"total_tokens", ev.TotalTokens == 0},
		{"cached_tokens", ev.CachedTokens == 0},
		{"cache_write_tokens", ev.CacheWriteTokens == 0},
		{"prompt_per_second", ev.PromptPerSecond == 0},
		{"tokens_per_second", ev.TokensPerSecond == 0},
	}
	for _, col := range tokenColumns {
		if !col.zero {
			return fmt.Errorf("%w: unit %q row carries non-zero %s", ErrBillingXORViolated, ev.BillingUnit, col.name)
		}
	}
	return nil
}
```

- [ ] **Step 4: Run the tests to verify they pass**

```bash
cd gateway/backend && go test ./internal/usage/ -run 'TestValidBillingUnit|TestValidateBillingXOR' -v
```

Expected: PASS (all four).

- [ ] **Step 5: Write the failing structural-no-op test for `recordUsage`**

Create `gateway/backend/internal/gateway/usage_billing_unit_test.go`. The harness is `NewTestServer()` plus `srv.Usage`, exactly as `service_usage_attribution_test.go` uses it — that file is the closest precedent, because it pins the same kind of no-op invariant for `ServiceID`/`ServiceName`. Note `provider.Response.Usage` is an **`inference.Usage`**, not a `provider.Usage`.

The point is that the no-op is **structural** — `usageMeta` is a keyed literal at all ten call sites, so every existing path writes `""`/0 by Go zero value — and this test pins it:

```go
// Every existing recordUsage path must write a token-metered row: usageMeta is
// built as a KEYED literal at all ten call sites, so the pair is the Go zero
// value unless a producer sets it. This is what makes the whole change a no-op
// for LLM traffic, independently of migration v81's column defaults.
func TestRecordUsageWritesTokenMeteredByDefault(t *testing.T) {
	srv := NewTestServer()

	srv.recordUsage(
		time.Now().Add(-100*time.Millisecond),
		auth.Token{ID: "tok_user", UserID: "usr_billing"},
		inference.Request{Model: "qwen-coder"},
		routing.Target{ServerID: "mock-host-comp"},
		provider.Response{Usage: inference.Usage{InputTokens: 10, OutputTokens: 20, TotalTokens: 30}},
		"", "success",
		usageMeta{ReqPath: "/v1/chat/completions", HTTPStatus: 200, ContentType: "application/json"},
		"req_billing_default", nil,
	)

	events := srv.Usage.ByUser("usr_billing")
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	got := events[0]
	if got.BillingUnit != usage.BillingUnitTokens {
		t.Errorf("BillingUnit = %q, want %q", got.BillingUnit, usage.BillingUnitTokens)
	}
	if got.BillingQuantity != 0 {
		t.Errorf("BillingQuantity = %v, want 0", got.BillingQuantity)
	}
	if err := usage.ValidateBillingXOR(got); err != nil {
		t.Errorf("recorded event violates the XOR: %v", err)
	}
}

// A usageMeta carrying a unit reaches the row unchanged -- the seam #71/#68/#69
// will use.
func TestRecordUsageCarriesBillingPairFromUsageMeta(t *testing.T) {
	srv := NewTestServer()

	srv.recordUsage(
		time.Now().Add(-2*time.Second),
		auth.Token{ID: "tok_user", UserID: "usr_billing_img"},
		inference.Request{Model: "sd-turbo"},
		routing.Target{ServerID: "mock-host-comp"},
		provider.Response{},
		"", "success",
		usageMeta{
			ReqPath:         "/v1/images/generations",
			HTTPStatus:      200,
			ContentType:     "application/json",
			BillingUnit:     usage.BillingUnitImage,
			BillingQuantity: 2,
		},
		"req_billing_image", nil,
	)

	events := srv.Usage.ByUser("usr_billing_img")
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	got := events[0]
	if got.BillingUnit != usage.BillingUnitImage || got.BillingQuantity != 2 {
		t.Errorf("got (%q, %v), want (%q, 2)", got.BillingUnit, got.BillingQuantity, usage.BillingUnitImage)
	}
	if err := usage.ValidateBillingXOR(got); err != nil {
		t.Errorf("recorded event violates the XOR: %v", err)
	}
}
```

`recordUsage`'s signature is
`(start time.Time, token auth.Token, req inference.Request, target routing.Target, resp provider.Response, errorCode, status string, meta usageMeta, id string, capture *captureInput)`.
`NewTestServer()` seeds the mock routes (`seedGatewayTestRoutes`), which is why `mock-host-comp` and `qwen-coder` resolve.

- [ ] **Step 6: Run to verify the second test fails**

```bash
cd gateway/backend && go test ./internal/gateway/ -run 'TestRecordUsage.*Billing|TestRecordUsageWritesTokenMetered' -v
```

Expected: the first test PASSES already (the Go zero value does the work — that is the point), the second FAILS to compile on `unknown field BillingUnit in struct literal of type usageMeta`.

- [ ] **Step 7: Extend `usageMeta` and the event literal**

In `gateway/backend/internal/gateway/inference_complete.go`, extend the `usageMeta` struct (`:630-634`):

```go
type usageMeta struct {
	ReqPath     string
	HTTPStatus  int
	ContentType string
	// BillingUnit/BillingQuantity are the non-token billable measure for this
	// request, and they must come from ENDPOINT IDENTITY -- never from resp.
	// 7 of the 10 call sites pass a zero provider.Response, so a
	// response-derived unit would record a FAILED image request as ""
	// (token-metered with a zero measure), the exact lie the pair exists to
	// prevent. Left as the zero value by every token-metered call site, which is
	// what makes the pair a structural no-op for all existing traffic; see
	// usage.ValidateBillingXOR for the contract.
	BillingUnit     string
	BillingQuantity float64
}
```

In the `usage.Event` literal (`:663`), after `CreatedAt: time.Now().UTC(),`:

```go
		BillingUnit:     meta.BillingUnit,
		BillingQuantity: meta.BillingQuantity,
```

- [ ] **Step 8: Run to verify both tests pass**

```bash
cd gateway/backend && go test ./internal/gateway/ -run 'TestRecordUsage.*Billing|TestRecordUsageWritesTokenMetered' -v
```

Expected: PASS.

- [ ] **Step 9: Make an XOR violation observable**

The critique this addresses is concrete: nothing anywhere observes a reconciler or accounting anomaly today — every failure path in the energy reconciler is Debug-only. A contract violation must not be silent.

In `recordUsage`, immediately before the `s.Usage.Record(...)` call, add:

```go
	// The XOR is an invariant of the row, not a suggestion. A violation means a
	// producer is wrong, so it is logged at Error and the row is written
	// UNMODIFIED: silently repairing the data would destroy the evidence, and
	// dropping the row would lose a request from billing entirely.
	if err := usage.ValidateBillingXOR(event); err != nil {
		slog.Error("usage billing contract violated", "id", id, "req_path", meta.ReqPath, "err", err)
	}
```

This requires hoisting the event literal into a local `event := usage.Event{...}` and passing `event` to `s.Usage.Record(event)`. Make that mechanical change and nothing else.

- [ ] **Step 10: Test the observability branch**

Add to the same test file:

```go
// A violating row is still recorded -- unmodified -- because dropping it would
// lose a request from billing and repairing it would destroy the evidence.
func TestRecordUsageRecordsAnXORViolationUnmodified(t *testing.T) {
	srv := NewTestServer()

	srv.recordUsage(
		time.Now().Add(-time.Second),
		auth.Token{ID: "tok_user", UserID: "usr_billing_bad"},
		inference.Request{Model: "sd-turbo"},
		routing.Target{ServerID: "mock-host-comp"},
		provider.Response{Usage: inference.Usage{OutputTokens: 7, TotalTokens: 7}},
		"", "success",
		usageMeta{ReqPath: "/v1/images/generations", HTTPStatus: 200, BillingUnit: usage.BillingUnitImage, BillingQuantity: 1},
		"req_billing_violation", nil,
	)

	events := srv.Usage.ByUser("usr_billing_bad")
	if len(events) != 1 {
		t.Fatalf("want the violating row recorded, got %d events", len(events))
	}
	if events[0].OutputTokens != 7 || events[0].BillingUnit != usage.BillingUnitImage {
		t.Errorf("the row must be recorded unmodified, got %+v", events[0])
	}
}
```

```bash
cd gateway/backend && go test ./internal/gateway/ -run TestRecordUsageRecordsAnXORViolationUnmodified -v
```

Expected: PASS.

- [ ] **Step 11: Run the full backend suite and the linters**

```bash
cd gateway/backend && go test ./... 2>&1 | grep -v '^ok' | head -20
```

```bash
cd gateway/backend && golangci-lint fmt --diff && golangci-lint run
```

Expected: no failures; `fmt --diff` prints nothing. Provider adapter tests use `httptest.NewServer`, so a constrained sandbox may need permission to open loopback listeners.

- [ ] **Step 12: Commit**

```bash
git add gateway/backend/internal/usage/billing.go gateway/backend/internal/usage/billing_test.go gateway/backend/internal/gateway/inference_complete.go gateway/backend/internal/gateway/
```

```bash
git commit -m "feat(usage): Define the billing-unit vocabulary and enforce its XOR

Adds usage.BillingUnitTokens/Image/AudioSecond, ValidBillingUnit and
ValidateBillingXOR, and threads the pair through usageMeta -- the
purpose-built seam for per-request facts only the HTTP call site knows.

Two values, not three. 'character' comes from OpenAI's price list rather
than from this system: audio.cpp's metering signal is an audio duration, so
speech and transcription share audio_second.

There is deliberately NO normalizer. Unlike NormalizeSort's harmless
default, clamping an unknown unit to '' would assert token-metering about a
request that is not token-metered -- the exact lie the pair exists to
prevent -- so an unknown unit is an error instead.

The XOR covers SEVEN columns, not five: prompt_per_second and
tokens_per_second are rates outside 'the five token columns', and
ComputeHistogram drops zeros by design, so a stray non-zero rate on a
non-token row would enter the speed histograms with nothing to fail.

recordUsage validates before Record and logs a violation at Error, then
writes the row UNMODIFIED: repairing it silently would destroy the evidence
and dropping it would lose a request from billing. This matters because
nothing else in this subsystem observes an anomaly -- every energy
reconciler failure path is Debug-only.

Because usageMeta is a keyed literal at all ten call sites, every existing
path writes ''/0 by Go zero value, so the no-op for LLM traffic is
structural rather than a consequence of the migration defaults. A test pins
that."
```

---

## Task 3: Energy — gate Tier 3 and stamp a terminal provenance

**Files:**
- Modify: `gateway/backend/internal/gateway/energy_engine.go:20` (const) and `:430-437` (`modeledEnergy`)
- Modify: `gateway/backend/internal/gateway/energy_reconciler.go:208` (the calibration guard)
- Test: `gateway/backend/internal/gateway/energy_engine_test.go`, `gateway/backend/internal/gateway/energy_reconciler_test.go`

**Interfaces:**
- Consumes: `usage.Event.BillingUnit` (Task 1), `usage.BillingUnitImage` (Task 2).
- Produces: the exported-by-value wire string `"unpriceable"` on `energy_source`. Task 5 renders it and Task 6 documents it.

- [ ] **Step 1: Write the failing energy tests**

Add to `gateway/backend/internal/gateway/energy_engine_test.go`:

```go
// Tier 3 is coeff * OutputTokens, so it is structurally inapplicable to a row
// whose measure is not tokens. It must stamp a terminal provenance rather than a
// "modeled" zero: "modeled" claims a derivation that did not happen, and a
// zero-Wh "modeled" row is byte-identical to a legitimately-modeled token row on
// a zero-coefficient mapping -- once the two share a shape, no later query,
// backfill or audit can separate them.
func TestEnergyComputeNonTokenRowIsUnpriceableNotModeled(t *testing.T) {
	ev := usage.Event{
		LatencyMS:       1500,
		CreatedAt:       time.Now().UTC(),
		BillingUnit:     usage.BillingUnitImage,
		BillingQuantity: 2,
	}
	// No telemetry and EstimatedWatts unset: a Tier-3-only host.
	res := ComputeEnergy(ev, nil, nil, ServerEnergyConfig{}, 0, 0.5, 0.25)

	if res.Source != "unpriceable" {
		t.Errorf("Source = %q, want \"unpriceable\"", res.Source)
	}
	if res.WhTotal != 0 || res.WhMarginal != 0 {
		t.Errorf("Wh = (%v, %v), want (0, 0)", res.WhTotal, res.WhMarginal)
	}
}

// The guard keys on the UNIT, not on the token count, so it RESPECTS the XOR
// rather than trusting it: a producer bug that leaves OutputTokens non-zero can
// never be multiplied by a Wh-per-token coefficient.
func TestEnergyComputeNonTokenRowWithStrayTokensStillUnpriceable(t *testing.T) {
	ev := usage.Event{
		LatencyMS:       1500,
		CreatedAt:       time.Now().UTC(),
		BillingUnit:     usage.BillingUnitImage,
		BillingQuantity: 2,
		OutputTokens:    9999, // contract violation; must not become a Wh figure
	}
	res := ComputeEnergy(ev, nil, nil, ServerEnergyConfig{}, 0, 0.5, 0.25)

	if res.Source != "unpriceable" || res.WhTotal != 0 {
		t.Errorf("got (%q, %v), want (\"unpriceable\", 0)", res.Source, res.WhTotal)
	}
}

// Tiers 1 and 2 never read a token count, so a non-token request on a host with
// telemetry or a configured wattage gets a REAL figure. The guard must not
// change that -- it is the whole reason the gap is narrow.
func TestEnergyComputeNonTokenRowStillReachesTier2(t *testing.T) {
	now := time.Now().UTC()
	ev := usage.Event{
		LatencyMS:       2000,
		CreatedAt:       now,
		BillingUnit:     usage.BillingUnitImage,
		BillingQuantity: 1,
	}
	res := ComputeEnergy(ev, nil, nil, ServerEnergyConfig{EstimatedWatts: 300}, 0, 0, 0)

	if res.Source != "estimated" {
		t.Errorf("Source = %q, want \"estimated\"", res.Source)
	}
	if res.WhTotal <= 0 {
		t.Errorf("WhTotal = %v, want a positive time-based figure", res.WhTotal)
	}
}
```

Read the existing `TestEnergyComputeTier3CoeffZeroStillModeled` (`energy_engine_test.go:531`) first and match its `ComputeEnergy` call shape exactly; the signature is
`ComputeEnergy(ev usage.Event, samples []routing.TelemetrySample, siblings []usage.Event, cfg ServerEnergyConfig, idleW, mappingCoeff, sysDefaultWhPerToken float64)`.

- [ ] **Step 2: Run to verify they fail**

```bash
cd gateway/backend && go test ./internal/gateway/ -run 'TestEnergyComputeNonToken' -v
```

Expected: the first two FAIL with `Source = "modeled", want "unpriceable"`. The third should already PASS — confirming Tier 2 is token-blind today.

- [ ] **Step 3: Add the const and the guard**

In `gateway/backend/internal/gateway/energy_engine.go`, beside `maxSampleGap` near `:20`:

```go
// energySourceUnpriceable is a FOURTH energy_source value, and the only one that
// is not a tier: it means no tier could apply, because the row's measure is not
// tokens and the host offered neither telemetry (Tier 1) nor a configured
// wattage (Tier 2). It is terminal by design -- a stamped row leaves
// UnpricedUsageEvents' energy_source='' predicate, which is what keeps the
// reconciler idempotent.
//
// A stamp is not optional. Leaving such a row at '' would not merely build a
// backlog: UnpricedUsageEvents is oldest-first with LIMIT 500 over a 168h
// horizon, so never-priceable rows occupy the oldest end of every batch and the
// reconciler stops reaching newer events -- including token rows Tier 1 would
// have measured. The choice is only WHICH non-empty string, and "modeled" would
// claim a derivation that did not happen.
//
// The operator remedy is to set that server's estimated_watts: Tier 2 then
// prices every endpoint kind at once, dimensionally honestly, with no new
// column. A later per-unit coefficient can re-stamp these rows --
// UpdateUsageEventEnergy writes by id with no source predicate.
const energySourceUnpriceable = "unpriceable"
```

Then the guard, as the first statement of `modeledEnergy`:

```go
func modeledEnergy(ev usage.Event, mappingCoeff, sysDefaultWhPerToken float64) EnergyResult {
	// Keyed on the UNIT, deliberately not on ev.OutputTokens == 0. That confines
	// the new value to rows that cannot exist before migration v81, leaves every
	// token path byte-identical (including the zero-coefficient row
	// TestEnergyComputeTier3CoeffZeroStillModeled pins as desirable), and
	// RESPECTS the XOR instead of trusting it: a producer bug leaving
	// OutputTokens non-zero can then never be multiplied by a Wh/token
	// coefficient.
	if ev.BillingUnit != usage.BillingUnitTokens {
		return EnergyResult{Source: energySourceUnpriceable}
	}
	coeff := mappingCoeff
	if coeff <= 0 {
		coeff = sysDefaultWhPerToken
	}
	wh := cleanNonNeg(coeff * float64(ev.OutputTokens))
	return EnergyResult{WhTotal: wh, WhMarginal: wh, Source: "modeled"}
}
```

**The guard must live here, not at `ComputeEnergy`'s call site.** Tier 3 has two entry points: `energy_engine.go:231` and `reconcileEnergyEvent`'s deleted-server shortcut at `energy_reconciler.go:150`. Gating at the call site would leave the deleted-server path stamping `modeled` on a non-token row — and that is precisely the case where the gap is permanent, since a deleted server can never regain telemetry.

Also extend the `EnergyResult.Source` doc comment at `:52-55` and `ComputeEnergy`'s tier list at `:196-200` to name the fourth value and say that it is a provenance, not a tier.

- [ ] **Step 4: Run to verify they pass and nothing regressed**

```bash
cd gateway/backend && go test ./internal/gateway/ -run 'TestEnergy' -v 2>&1 | tail -30
```

Expected: the three new tests PASS, and every existing energy test — in particular `TestEnergyComputeTier3CoeffZeroStillModeled` — still passes unchanged.

- [ ] **Step 5: Write the failing test for the deleted-server entry point**

Add to `gateway/backend/internal/gateway/energy_reconciler_test.go`, using the file's `newEnergyFixture` harness. This mirrors `TestReconcileEnergyEventForDeletedServerStillFinalized` (`:425`), which is the existing test for this exact entry point:

```go
// reconcileEnergyEvent's deleted-server shortcut calls modeledEnergy directly,
// bypassing ComputeEnergy. It is the one case where the gap is PERMANENT -- a
// deleted server can never regain telemetry -- so it must produce the terminal
// provenance too, and it must still STAMP, or the row never leaves the unpriced
// queue and starves newer events behind it.
func TestReconcileEnergyDeletedServerNonTokenRowStampsUnpriceable(t *testing.T) {
	f := newEnergyFixture(t, 0, 0)
	// Deliberately do NOT create the server "srv-gone".
	f.usage.Record(usage.Event{
		ID: "evt_img", Host: "srv-gone", RouteID: "",
		CreatedAt: time.Now().Add(-time.Minute), LatencyMS: 1000,
		BillingUnit: usage.BillingUnitImage, BillingQuantity: 2,
	})

	f.srv.reconcileEnergyOnce(context.Background(), time.Now())

	got := f.event("evt_img")
	if got.EnergySource != "unpriceable" {
		t.Fatalf("EnergySource = %q, want %q", got.EnergySource, "unpriceable")
	}
	if got.EnergyWh != 0 || got.EnergyMarginalWh != 0 {
		t.Fatalf("Wh = (%v, %v), want (0, 0)", got.EnergyWh, got.EnergyMarginalWh)
	}

	// Idempotency: a stamped row is terminal, so a second pass must not touch it.
	f.srv.reconcileEnergyOnce(context.Background(), time.Now())
	if again := f.event("evt_img"); again.EnergySource != "unpriceable" {
		t.Fatalf("second pass changed EnergySource to %q", again.EnergySource)
	}
}
```

- [ ] **Step 6: Run to verify it fails, then passes**

```bash
cd gateway/backend && go test ./internal/gateway/ -run TestReconcileEnergyDeletedServerNonTokenRowStampsUnpriceable -v
```

Expected: PASS once the guard from Step 3 is in place — because the guard lives inside `modeledEnergy`, this entry point is covered without a second edit. If it FAILS, the guard was put in the wrong place; move it into `modeledEnergy`.

- [ ] **Step 7: Tighten the EWMA calibration guard**

In `gateway/backend/internal/gateway/energy_reconciler.go:208`, change:

```go
	if res.Source == "measured" && ev.OutputTokens > 0 && mappingID != "" {
```

to:

```go
	if res.Source == "measured" && ev.BillingUnit == usage.BillingUnitTokens && ev.OutputTokens > 0 && mappingID != "" {
```

and extend the comment above it:

```go
	// Calibration: only on a genuine per-request power MEASUREMENT, only on a
	// TOKEN-METERED row, only with a resolvable mapping to calibrate, and only
	// with real output tokens to divide by.
	//
	// The billing-unit conjunct is explicit rather than implied. The divide at
	// the next line is guarded today only by OutputTokens > 0, which holds only
	// because a producer in another package honours an unenforced convention --
	// and dividing WhMarginal by a billing_quantity here is one of the two
	// surfaces that would tempt a unit-to-token conversion, writing a per-image
	// number into energy_wh_per_token. See ADR-041.
```

- [ ] **Step 8: Write the failing calibration test**

Mirror `TestReconcileEnergyMeasuredCalibratesMapping` (`:291`) — same fixture, same 36 samples, same mapping — but make the row non-token:

```go
// A measured non-token row must never calibrate energy_wh_per_token: a per-image
// figure written into a per-token coefficient is exactly the unit-to-token
// conversion ADR-041 forbids.
//
// OutputTokens is deliberately non-zero (a contract violation), to prove the new
// billing-unit conjunct is what stops the calibration -- not the pre-existing
// OutputTokens > 0 guard, which would let this through.
func TestReconcileEnergyMeasuredNonTokenRowDoesNotCalibrate(t *testing.T) {
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	start := base
	end := base.Add(36 * time.Second)

	f := newEnergyFixture(t, 0, 0)
	f.createServer("srv1", routing.AIServer{})
	for i := 0; i <= 36; i++ {
		f.insertSample("srv1", sampleAt(start, i, 100))
	}
	f.createMapping("map1", "app1", "srv1", 8080, routing.ModelMapping{GatewayModelName: "gw", AppModelName: "upstream"})
	f.usage.Record(usage.Event{
		ID: "evt_img", Host: "srv1", RouteID: "map1", CreatedAt: end, LatencyMS: 36000,
		BillingUnit: usage.BillingUnitImage, BillingQuantity: 4,
		OutputTokens: 1000, // contract violation, on purpose
	})

	f.srv.reconcileEnergyOnce(context.Background(), end.Add(20*time.Second))

	// Tier 1 still applies: a non-token row on a telemetry-covered host gets a
	// REAL measured figure. Only the calibration must be refused.
	ev := f.event("evt_img")
	if ev.EnergySource != "measured" {
		t.Fatalf("EnergySource = %q, want %q (Tier 1 is token-blind and must still apply)", ev.EnergySource, "measured")
	}
	if ev.EnergyWh <= 0 {
		t.Fatalf("EnergyWh = %v, want > 0", ev.EnergyWh)
	}

	m := f.mapping("map1")
	if m.EnergyWhPerToken != 0 {
		t.Fatalf("mapping EnergyWhPerToken = %v, want 0 (a non-token row must never calibrate a per-token coefficient)", m.EnergyWhPerToken)
	}
	if m.MetricsSource == "energy" {
		t.Fatalf("MetricsSource = %q: calibration fired on a non-token row", m.MetricsSource)
	}
}
```

```bash
cd gateway/backend && go test ./internal/gateway/ -run 'TestReconcileEnergy' -v 2>&1 | tail -25
```

Expected: PASS, and every existing reconciler test unchanged — in particular `TestReconcileEnergyMeasuredCalibratesMapping` (`:291`), which must still calibrate, since its row is token-metered.

- [ ] **Step 9: Run the full gateway package and the linters**

```bash
cd gateway/backend && go test ./internal/gateway/ ./internal/usage/ ./internal/store/ 2>&1 | tail -10
```

```bash
cd gateway/backend && golangci-lint fmt --diff && golangci-lint run
```

- [ ] **Step 10: Commit**

```bash
git add gateway/backend/internal/gateway/energy_engine.go gateway/backend/internal/gateway/energy_engine_test.go gateway/backend/internal/gateway/energy_reconciler.go gateway/backend/internal/gateway/energy_reconciler_test.go
```

```bash
git commit -m "feat(energy): Stamp a non-token row unpriceable instead of modeled zero

Tier 3 is coeff * OutputTokens, so it is structurally inapplicable to a row
whose measure is not tokens. modeledEnergy now gates on the billing unit and
returns a fourth energy_source value, 'unpriceable', with 0 Wh.

Why not leave the row unpriced for the reconciler to retry: that is not a
backlog, it is head-of-line starvation. UnpricedUsageEvents is oldest-first
with LIMIT 500 over a 168h horizon on a 15s tick, so once more than 500
never-priceable rows sit in the window, every pass re-selects the same dead
500 and ordinary token events that Tier 1 WOULD have measured are never
reached. reconcileEnergyOnce's own contract is that every event a pass looks
at is unconditionally stamped. So the fork is only which non-empty string.

Why not 'modeled': it claims a derivation that did not happen, and a
zero-Wh 'modeled' row is byte-identical to a legitimately-modeled token row
on a zero-coefficient mapping -- a state an existing test pins as
desirable. Once the two share a row shape, nothing downstream can ever
separate 'modeled as zero' from 'no basis exists'.

The guard lives INSIDE modeledEnergy, not at ComputeEnergy's call site,
because Tier 3 has two entry points: that call site and
reconcileEnergyEvent's deleted-server shortcut -- the case where the gap is
permanent, since a deleted server can never regain telemetry.

It keys on the unit, not on OutputTokens == 0, which leaves every token path
byte-identical and respects the XOR rather than trusting it: a producer bug
leaving stray tokens can never be multiplied by a Wh-per-token coefficient.

Tiers 1 and 2 are untouched and remain token-blind, so a non-token request
on a host with telemetry or estimated_watts still gets a real figure and
CostBudget still bites. Only a Tier-3-only host sees 'unpriceable', and the
remedy is to set that server's estimated_watts.

The EWMA calibration guard gains an explicit billing-unit conjunct: its
divide was protected only by OutputTokens > 0, which held only because a
producer in another package honoured an unenforced convention."
```

---

## Task 4: Aggregates — one count, no `GROUP BY` widening

**Files:**
- Modify: `gateway/backend/internal/usage/query.go:145-157` (`GroupBucket`), `:200-216` (`StatTotals`)
- Modify: `gateway/backend/internal/store/sqlite_usage.go:740-792` (`Stats`), `:1006-1048` (`UsageGroups`)
- Modify: `gateway/backend/internal/usage/recorder.go:322-353` (`Stats`), `:397-436` (`UsageGroups`)
- Modify: `gateway/backend/internal/portal/service_usage_groups.go:23-37` (DTO), `:85-117` (fold)
- Modify: `gateway/backend/internal/portal/service_projects.go:1040-1075` (rollups)
- Modify: `gateway/backend/internal/portal/service.go:1965-1985` (Dashboard)
- Test: `gateway/backend/internal/store/conformance_test.go`, `gateway/backend/internal/usage/recorder_test.go`, `gateway/backend/internal/portal/service_usage_groups_test.go`

**Interfaces:**
- Consumes: `usage.Event.BillingUnit` (Task 1), `usage.BillingUnitImage` (Task 2).
- Produces: `usage.StatTotals.NonTokenRequests int` (json `non_token_requests`), `usage.GroupBucket.NonTokenRequests int`, `portal.UsageGroupDTO.NonTokenRequests int` (json `non_token_requests`), the project DTO and Dashboard equivalents. Task 5 consumes all of these over the wire.

**Design note the implementer must not "improve":** the `GROUP BY` does **not** gain `billing_unit`. The portal folds buckets by `Key` alone, so a split bucket is immediately recombined; it would collide with the 500-group truncation; and the frontend's `exactFilter` has no unit dimension to drill into. It buys cardinality cost with no benefit. Likewise `billing_quantity` is **not** summed at group level — adding images to audio seconds would be exactly the lie a scalar unit would have been.

- [ ] **Step 1: Write the failing twin aggregate tests**

There is **no** memory-vs-SQL conformance suite for `usage.Store`: `conformance_test.go` is dual-**dialect**, and memory parity rests on hand-mirrored twin tests. So the same scenario must be written **twice** — once in `gateway/backend/internal/usage/recorder_test.go` against the `Recorder`, once in `gateway/backend/internal/store/conformance_test.go` against the SQL stores. Existing twins to mirror: `conformance_test.go:3332` and `recorder_test.go:748`.

The scenario, identical in both:

```go
// A mixed-unit population: the token sums must exclude the non-token row, and
// NonTokenRequests must disclose it. Without the count, a non-token row's "0
// tokens" is indistinguishable from a token-metered request whose upstream
// reported no usage object.
func seedMixedUnitEvents(now time.Time) []usage.Event {
	return []usage.Event{
		{
			ID: "ev_tok", UserID: "u1", TokenID: "t1", Model: "m1", Host: "h1",
			InputTokens: 10, OutputTokens: 20, TotalTokens: 30,
			CachedTokens: 5, CacheWriteTokens: 2,
			PromptPerSecond: 50, TokensPerSecond: 25,
			EnergyWh: 4, LatencyMS: 1000, Status: "success", HTTPStatus: 200,
			CreatedAt: now.Add(-2 * time.Minute),
		},
		{
			ID: "ev_img", UserID: "u1", TokenID: "t1", Model: "m1", Host: "h1",
			BillingUnit: usage.BillingUnitImage, BillingQuantity: 3,
			// All seven token-denominated columns stay 0, per the XOR. The two
			// per-second columns are set EXPLICITLY to 0 to pin the contract
			// rather than rely on the zero value.
			PromptPerSecond: 0, TokensPerSecond: 0,
			EnergyWh: 6, LatencyMS: 3000, Status: "success", HTTPStatus: 200,
			CreatedAt: now.Add(-1 * time.Minute),
		},
	}
}
```

Assertions (write them in both twins):

```go
	stats, err := store.Stats(usage.Query{ScopeAll: true, From: now.Add(-time.Hour), To: now})
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if stats.Totals.TotalRequests != 2 {
		t.Errorf("TotalRequests = %d, want 2 (a non-token request is a real request)", stats.Totals.TotalRequests)
	}
	if stats.Totals.NonTokenRequests != 1 {
		t.Errorf("NonTokenRequests = %d, want 1", stats.Totals.NonTokenRequests)
	}
	if stats.Totals.InputTokens != 10 || stats.Totals.OutputTokens != 20 ||
		stats.Totals.CachedTokens != 5 || stats.Totals.CacheWriteTokens != 2 {
		t.Errorf("token sums polluted by the non-token row: %+v", stats.Totals)
	}
	if stats.Totals.TotalEnergyWh != 10 {
		t.Errorf("TotalEnergyWh = %v, want 10 (a non-token row's energy IS real)", stats.Totals.TotalEnergyWh)
	}
	// ComputeHistogram drops zeros by design, so the non-token row contributes no
	// bin. Assert it explicitly rather than relying on that behaviour silently.
	if got := totalHistogramCount(stats.TokensPerSecond); got != 1 {
		t.Errorf("tokens/s histogram counted %d values, want 1 (the non-token row's 0 must not bin)", got)
	}

	buckets, err := store.UsageGroups(context.Background(), usage.Query{ScopeAll: true, From: now.Add(-time.Hour), To: now}, "model")
	if err != nil {
		t.Fatalf("usage groups: %v", err)
	}
	var count, nonToken, input int
	for _, b := range buckets {
		count += b.Count
		nonToken += b.NonTokenRequests
		input += b.InputTokens
	}
	if count != 2 || nonToken != 1 || input != 10 {
		t.Errorf("groups: count=%d nonToken=%d input=%d, want 2/1/10", count, nonToken, input)
	}

	// The time series is the SECOND token-per-second aggregate, and it is safe
	// for a different reason than the histogram: the divisor is bucket seconds,
	// not a row count. A non-token row is genuinely IN its input set -- it bumps
	// Connections, Concurrency and EnergyWh -- and contributes nothing to the
	// rate numerators. Assert that, rather than trusting it.
	series, err := store.TimeSeries(usage.Query{ScopeAll: true, From: now.Add(-time.Hour), To: now}, 3600)
	if err != nil {
		t.Fatalf("time series: %v", err)
	}
	var conns int
	var energy float64
	for _, pt := range series.Points {
		conns += pt.Connections
		energy += pt.EnergyWh
	}
	if conns != 2 {
		t.Errorf("Connections total = %d, want 2 (a non-token request is a real connection)", conns)
	}
	if energy != 10 {
		t.Errorf("EnergyWh total = %v, want 10 (a non-token row's energy IS real)", energy)
	}
```

Add a small `totalHistogramCount(h usage.Histogram) int` helper in the test file that sums `h.Bins[i].Count`.

- [ ] **Step 2: Run to verify they fail**

```bash
cd gateway/backend && go test ./internal/usage/ ./internal/store/ -run 'MixedUnit' -v
```

Expected: FAIL to compile — `stats.Totals.NonTokenRequests undefined`.

- [ ] **Step 3: Add the fields**

In `gateway/backend/internal/usage/query.go`, `StatTotals` (after `TotalRequests`):

```go
	// NonTokenRequests is how many of TotalRequests carry a non-empty
	// billing_unit, i.e. are NOT token-metered. It exists so a consumer can tell
	// a not-applicable from a measured zero: without it, a non-token row's 0
	// tokens is indistinguishable from a token-metered request whose upstream
	// reported no usage object. 0 == the whole population is token-metered;
	// == TotalRequests == none of it is; in between == a mixed population, where
	// the token sums are correct for the token-metered SUBSET.
	NonTokenRequests int `json:"non_token_requests"`
```

And `GroupBucket` (after `ErrorCount`):

```go
	// NonTokenRequests is the count of non-token-metered rows in this bucket.
	// See StatTotals.NonTokenRequests. Note there is deliberately no quantity
	// sum: adding images to audio seconds would be exactly the lie the
	// (unit, quantity) pair exists to prevent.
	NonTokenRequests int
```

- [ ] **Step 4: Implement the SQL `Stats` accumulation**

In `sqlite_usage.go`'s `Stats` (`:740`), add `e.billing_unit` to the SELECT:

```go
		"select e.status, e.http_status, e.cached_tokens, e.cache_write_tokens, e.input_tokens, e.output_tokens, e.prompt_per_second, e.tokens_per_second, e.energy_wh, e.billing_unit from usage_events as e"+where,
```

widen the scan locals and the `Scan` call:

```go
		var (
			status                         string
			httpStatus                     int
			cached, cacheWrite, input, out int
			pps, tps, energyWh             float64
			billingUnit                    string
		)
		if err := rows.Scan(&status, &httpStatus, &cached, &cacheWrite, &input, &out, &pps, &tps, &energyWh, &billingUnit); err != nil {
```

and accumulate, right after `totals.TotalRequests++`:

```go
		if billingUnit != "" {
			totals.NonTokenRequests++
		}
```

- [ ] **Step 5: Implement the SQL `UsageGroups` aggregation**

In `UsageGroups` (`:1006`), add the conditional count to the SELECT — `<>` is valid on both dialects:

```go
	sqlText := "select e." + col + " as gkey, e.host," +
		" count(*)," +
		" sum(case when e.status = 'error' or e.http_status >= 400 then 1 else 0 end)," +
		" sum(case when e.billing_unit <> '' then 1 else 0 end)," +
		" sum(e.input_tokens), sum(e.output_tokens), sum(e.cached_tokens), sum(e.cache_write_tokens)," +
		" sum(e.energy_wh), min(e.created_at), max(e.created_at)" +
		usageEventsFromClause + where +
		" group by e." + col + ", e.host"
```

widen the scan locals and the `Scan`, keeping the **same order**:

```go
		var count, errCount, nonToken, in, outTok, cached, cacheWrite int64
		...
		if err := rows.Scan(&b.Key, &b.Host, &count, &errCount, &nonToken, &in, &outTok, &cached, &cacheWrite, &energy, &first, &last); err != nil {
```

and assign:

```go
		b.Count, b.ErrorCount, b.NonTokenRequests = int(count), int(errCount), int(nonToken)
```

- [ ] **Step 6: Implement the memory twins**

In `gateway/backend/internal/usage/recorder.go`, `Stats` (`:322`) — after `totals.TotalRequests++`:

```go
		if e.BillingUnit != BillingUnitTokens {
			totals.NonTokenRequests++
		}
```

and `UsageGroups` (`:397`) — after `b.Count++`:

```go
		if e.BillingUnit != BillingUnitTokens {
			b.NonTokenRequests++
		}
```

- [ ] **Step 7: Run the twins to verify they pass on all three implementations**

```bash
cd gateway/backend && go test ./internal/usage/ ./internal/store/ -run 'MixedUnit' -v
```

```bash
cd gateway/backend && OP_AI_GATEWAY_TEST_POSTGRES_DSN='postgres://postgres:test@127.0.0.1:55432/optest?sslmode=disable' go test ./internal/store/ -run 'MixedUnit' -v
```

Expected: PASS, with the postgres subtest visibly **running**. Both store implementations must be behaviour-identical here; an all-`''` seed would pass either way, which is exactly why the mixed seed is the point.

- [ ] **Step 8: Carry the count through the portal fold**

In `gateway/backend/internal/portal/service_usage_groups.go`, add to `UsageGroupDTO` after `ErrorCount`:

```go
	NonTokenRequests int `json:"non_token_requests"`
```

and in the fold (`:96`, beside `a.dto.ErrorCount += b.ErrorCount`):

```go
		a.dto.NonTokenRequests += b.NonTokenRequests
```

`TotalTokens` stays derived as `InputTokens + OutputTokens + CachedTokens + CacheWriteTokens` — that derivation is **forced, not chosen**, because `GroupBucket` carries no stored total. Do not change it.

- [ ] **Step 9: Carry it through the project rollups and the Dashboard**

In `gateway/backend/internal/portal/service_projects.go`, the accumulator struct gains a `nonToken int` field; accumulate `all.nonToken += b.NonTokenRequests` alongside `all.count += b.Count` (`:1050`), and set `NonTokenRequests` on both the per-token rows (`:1063-1067`) and the total DTO (`:1070-1075`). Add the matching `NonTokenRequests int \`json:"non_token_requests"\`` field to `ProjectTokenUsageTotalDTO` and the per-token DTO.

In `gateway/backend/internal/portal/service.go`, the 24h loop (`:1969-1976`):

```go
	nonTokenRequests := 0
	for _, event := range events {
		if event.CreatedAt.Before(cutoff) {
			continue
		}
		requests++
		tokens += event.TotalTokens
		if event.BillingUnit != usage.BillingUnitTokens {
			nonTokenRequests++
		}
		latencies = append(latencies, event.LatencyMS)
	}
```

and add `NonTokenRequests24h: nonTokenRequests,` to `DashboardMetrics` (declare the field as `NonTokenRequests24h int \`json:"non_token_requests_24h"\``).

- [ ] **Step 10: Test the portal layer**

Add to `gateway/backend/internal/portal/service_usage_groups_test.go`, mirroring the existing test whose seeds deliberately leave `Event.TotalTokens` at 0 (`:75-77`):

```go
// The folded DTO must disclose a mixed population, and TotalTokens must stay the
// FORCED derivation over the four component columns -- GroupBucket carries no
// stored total, so there is nothing else it could be.
func TestUsageGroupsFoldCarriesNonTokenRequests(t *testing.T) {
	now := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)
	rec := usage.NewRecorder()
	// Same session key, DIFFERENT hosts, so the portal fold genuinely combines
	// two buckets rather than passing one through.
	rec.Record(usage.Event{
		ID: "ev_tok", UserID: "usr_1", SessionID: "sess_x", Host: "srv_1", CreatedAt: now,
		InputTokens: 10, OutputTokens: 7, CachedTokens: 2, CacheWriteTokens: 1,
	})
	rec.Record(usage.Event{
		ID: "ev_img", UserID: "usr_1", SessionID: "sess_x", Host: "srv_2", CreatedAt: now,
		BillingUnit: usage.BillingUnitImage, BillingQuantity: 5,
	})

	svc := NewService(ServiceDeps{Usage: rec, Clock: func() time.Time { return now }})

	groups, err := svc.UsageGroups(
		auth.Token{UserID: "usr_1", Scopes: []string{"gateway:use"}},
		UsageGroupsQuery{GroupBy: "session"},
	)
	if err != nil {
		t.Fatalf("usage groups: %v", err)
	}
	if len(groups) != 1 {
		t.Fatalf("groups = %d, want 1 (both hosts fold under one session key)", len(groups))
	}
	g := groups[0]
	if g.Count != 2 {
		t.Fatalf("Count = %d, want 2 (a non-token request is a real request)", g.Count)
	}
	if g.NonTokenRequests != 1 {
		t.Fatalf("NonTokenRequests = %d, want 1", g.NonTokenRequests)
	}
	if g.TotalTokens != 20 {
		t.Fatalf("TotalTokens = %d, want 20 (10+7+2+1; the non-token row contributes nothing)", g.TotalTokens)
	}
}
```

Match `svc.UsageGroups`'s real second argument and the `GroupBy` field name to the neighbouring tests in this file — `TestUsageGroupsScopedToOwnUser` (around `:88`) is the closest precedent; copy its call shape verbatim rather than the sketch above if they differ.

```bash
cd gateway/backend && go test ./internal/portal/ -run 'NonToken' -v
```

- [ ] **Step 11: Prove the quota path is correct for a non-token request**

`principal_limits.go` is not touched, so "unchanged for token requests" is discharged by the existing suite passing. What needs its own test is the other half of the spec's requirement: that a non-token request is counted correctly by the quota source of truth. `UsageAggregateSince` is that source — its signature is
`UsageAggregateSince(ctx context.Context, principalType, principalID string, since time.Time) (int64, int64, float64, error)`
returning `(requests, tokens, cost)`.

Add to `gateway/backend/internal/store/conformance_test.go` (and mirror it against the `Recorder` in `gateway/backend/internal/usage/recorder_test.go`):

```go
// A non-token request must consume RequestQuota and contribute 0 to TokenQuota.
// This falls out of the XOR rather than needing limiter changes -- count(*) has
// no unit filter, and sum(total_tokens) reads 0 from the row -- but it is the
// operator-visible story, so it gets a test rather than an assumption.
func TestUsageAggregateSinceCountsANonTokenRequestWithZeroTokens(t *testing.T) {
	forEachDialect(t, func(t *testing.T, st *SQLStore) {
		now := time.Now().UTC()
		since := now.Add(-time.Hour)
		for _, ev := range []usage.Event{
			{
				ID: "ev_tok", UserID: "usr_1", TokenID: "tok_1", Host: "h1", Model: "m1",
				InputTokens: 10, OutputTokens: 20, TotalTokens: 30, EnergyWh: 4,
				Status: "success", HTTPStatus: 200, CreatedAt: now.Add(-2 * time.Minute),
			},
			{
				ID: "ev_img", UserID: "usr_1", TokenID: "tok_1", Host: "h1", Model: "m1",
				BillingUnit: usage.BillingUnitImage, BillingQuantity: 3, EnergyWh: 6,
				Status: "success", HTTPStatus: 200, CreatedAt: now.Add(-1 * time.Minute),
			},
		} {
			if err := st.Record(ev); err != nil {
				t.Fatalf("record %s: %v", ev.ID, err)
			}
		}

		requests, tokens, _, err := st.UsageAggregateSince(context.Background(), "token", "tok_1", since)
		if err != nil {
			t.Fatalf("usage aggregate since: %v", err)
		}
		if requests != 2 {
			t.Errorf("requests = %d, want 2 (RequestQuota must count a non-token request)", requests)
		}
		if tokens != 30 {
			t.Errorf("tokens = %d, want 30 (TokenQuota must be a truthful no-op for the non-token row)", tokens)
		}
	})
}
```

Check the real `principalType` string the limiter passes (`grep -n 'UsageAggregateSince' gateway/backend/internal/gateway/principal_limits.go`) and use that value rather than the literal `"token"` if it differs.

```bash
cd gateway/backend && OP_AI_GATEWAY_TEST_POSTGRES_DSN='postgres://postgres:test@127.0.0.1:55432/optest?sslmode=disable' go test ./internal/store/ ./internal/usage/ -run 'UsageAggregateSinceCounts' -v
```

Expected: PASS on both dialects and on the memory recorder.

- [ ] **Step 12: Run the full backend suite with postgres, plus linters**

```bash
cd gateway/backend && OP_AI_GATEWAY_TEST_POSTGRES_DSN='postgres://postgres:test@127.0.0.1:55432/optest?sslmode=disable' go test ./... 2>&1 | grep -v '^ok' | head -20
```

```bash
cd gateway/backend && golangci-lint fmt --diff && golangci-lint run
```

- [ ] **Step 13: Commit**

```bash
git add gateway/backend/internal/usage/ gateway/backend/internal/store/ gateway/backend/internal/portal/
```

```bash
git commit -m "feat(usage): Disclose a mixed-unit population with NonTokenRequests

StatTotals and GroupBucket gain NonTokenRequests, carried through
UsageGroupDTO, the project rollups and the Dashboard's 24h metrics.

Mixed units are not an arithmetic problem here, and that is structural
rather than lucky: no aggregate on the group/tile path reads the stored
total_tokens at all -- StatTotals and GroupBucket carry only the four
component columns, so UsageGroupDTO's derived TotalTokens is forced, not
chosen -- and a non-token row contributes 0 to each of them by the XOR.
Request counts, error counts, concurrency, energy sums and latency
percentiles are correct by construction, because a non-token request
genuinely belongs in all of them.

What was missing is disclosure: a non-token row's 0 tokens is
indistinguishable from a token-metered request whose upstream reported no
usage object. One count makes three states expressible -- all
token-metered, none, and mixed -- without overloading '' or reserving a
'mixed' sentinel in the unit namespace.

The GROUP BY deliberately does NOT gain billing_unit: the portal folds
buckets by Key alone so a split bucket is immediately recombined, it would
collide with the 500-group truncation, and the frontend's exactFilter has no
unit dimension to drill into. And billing_quantity is deliberately not
summed at group level: adding images to audio seconds would be exactly the
lie the (unit, quantity) pair exists to prevent.

There is no memory-vs-SQL conformance suite for usage.Store --
conformance_test.go is dual-dialect and memory parity rests on hand-mirrored
twins -- so the mixed-unit scenario is written twice. An all-'' seed would
have passed either implementation silently, which is the whole point of
seeding a mixed population."
```

---

## Task 5: Portal — never show a not-applicable as a zero

**Files:**
- Create: `gateway/frontend/src/components/billingUnit.ts`, `gateway/frontend/src/components/billingUnit.test.ts`
- Modify: `gateway/frontend/src/api/usage.ts:7-63` (`UsageEvent`), `:112-127` (`UsageGroupRow`), the `StatTotals` type around `:228`, and `DashboardMetrics`
- Modify: `gateway/frontend/src/components/activityColumns.ts:32` (`ColumnId`), `:220-235` (definitions)
- Modify: `gateway/frontend/src/components/ActivityTable.tsx:67-136` (`renderCell`)
- Modify: `gateway/frontend/src/components/ActivityGroups.tsx:37-73`, `:216-239`, `:379`
- Modify: `gateway/frontend/src/components/StatTiles.tsx:13-16`, `:44-61`
- Modify: `gateway/frontend/src/components/Dashboard.tsx:41-45`
- Modify: `gateway/frontend/src/i18n.ts` (de ~`:1846`, en ~`:4088`)
- Test: `billingUnit.test.ts`, `ActivityTable.test.tsx`, `ActivityGroups.test.tsx`, `StatTiles.test.tsx`, `activityColumns.test.ts`

**Interfaces:**
- Consumes: the wire fields `billing_unit`, `billing_quantity` on `UsageEvent` (Task 1, auto-exposed because `usage.Row` embeds `Event`); `non_token_requests` on the group/stat/project/dashboard payloads (Task 4); the `energy_source` value `"unpriceable"` (Task 3).
- Produces: `isTokenMetered`, `tokenAggregate`, `ENERGY_SOURCE_HELP_KEYS`, `BILLING_UNIT_HELP_KEYS` from `billingUnit.ts`.

- [ ] **Step 1: Write the failing helper tests**

Create `gateway/frontend/src/components/billingUnit.test.ts`:

```ts
// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { describe, expect, it } from 'vitest';
import {
  BILLING_UNIT_HELP_KEYS,
  ENERGY_SOURCE_HELP_KEYS,
  isTokenMetered,
  tokenAggregate,
} from './billingUnit';

describe('isTokenMetered', () => {
  it('treats an empty or absent unit as token-metered', () => {
    expect(isTokenMetered({ billing_unit: '' })).toBe(true);
    expect(isTokenMetered({})).toBe(true);
  });

  it('treats any non-empty unit as not token-metered', () => {
    expect(isTokenMetered({ billing_unit: 'image' })).toBe(false);
    expect(isTokenMetered({ billing_unit: 'audio_second' })).toBe(false);
    // Forward compatibility: an unknown unit from a newer backend is still not
    // token-metered. Falling back to "token-metered" would print a zero as if it
    // were a measurement.
    expect(isTokenMetered({ billing_unit: 'video_second' })).toBe(false);
  });
});

describe('tokenAggregate', () => {
  it('renders the number when the whole population is token-metered', () => {
    expect(tokenAggregate(300, 0, 10)).toEqual({ text: '300', mixed: false, applicable: true });
  });

  it('renders an em dash when none of the population is token-metered', () => {
    expect(tokenAggregate(0, 10, 10)).toEqual({ text: '—', mixed: false, applicable: false });
  });

  it('renders the number and flags mixed when the population is mixed', () => {
    // The sum is arithmetically correct for the token-metered subset; the flag is
    // what lets the caller say so instead of implying it covers everything.
    expect(tokenAggregate(300, 4, 10)).toEqual({ text: '300', mixed: true, applicable: true });
  });

  it('renders the number for an empty population rather than an em dash', () => {
    expect(tokenAggregate(0, 0, 0)).toEqual({ text: '0', mixed: false, applicable: true });
  });
});

describe('help key tables', () => {
  it('keys on the FULL wire enum value for every energy source', () => {
    expect(Object.keys(ENERGY_SOURCE_HELP_KEYS).sort()).toEqual([
      '',
      'estimated',
      'measured',
      'modeled',
      'unpriceable',
    ]);
  });

  it('has no entry for an unknown value, so no tooltip is invented for it', () => {
    expect(ENERGY_SOURCE_HELP_KEYS['made_up']).toBeUndefined();
    expect(BILLING_UNIT_HELP_KEYS['made_up']).toBeUndefined();
  });

  it('keys billing units on the full wire value', () => {
    expect(Object.keys(BILLING_UNIT_HELP_KEYS).sort()).toEqual(['audio_second', 'image']);
  });
});
```

- [ ] **Step 2: Run to verify it fails**

```bash
cd gateway/frontend && npx vitest run src/components/billingUnit.test.ts
```

Expected: FAIL — cannot resolve `./billingUnit`.

- [ ] **Step 3: Write `billingUnit.ts`**

```ts
// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

// The portal side of the usage_events (billing_unit, billing_quantity) pair.
//
// The doctrine this implements is the repo's own, inverted. telemetry-usage-
// observability.md states that a measured value must never be indistinguishable
// from a measured zero (implemented in formatLiveTps). Mixed billable units add
// the converse: a NOT-APPLICABLE must never be indistinguishable from a zero.
// A non-token row's five token columns are 0 because tokens do not apply to it,
// not because nothing was measured -- so they render as an em dash.

/** A row is token-metered iff its billing_unit is empty or absent. */
export function isTokenMetered(row: { billing_unit?: string }): boolean {
  return !row.billing_unit;
}

export type TokenAggregate = {
  /** Ready-to-render cell text. */
  text: string;
  /** True when the population mixes token-metered and non-token rows: the number is correct for the token-metered SUBSET only. */
  mixed: boolean;
  /** False when tokens do not apply to any row in the population. */
  applicable: boolean;
};

/**
 * The three-state rule for a token-denominated aggregate over a population that
 * may mix units. Shared by the group table, the stat tiles, the project rollups
 * and the dashboard so all four agree.
 *
 * An empty population (totalRequests === 0) renders the number, not a dash:
 * "no rows at all" is not "tokens do not apply here".
 */
export function tokenAggregate(
  value: number,
  nonTokenRequests: number,
  totalRequests: number,
): TokenAggregate {
  if (totalRequests > 0 && nonTokenRequests >= totalRequests) {
    return { text: '—', mixed: false, applicable: false };
  }
  return { text: String(value), mixed: nonTokenRequests > 0, applicable: true };
}

// Tooltip copy for the two opaque wire enums this view renders as chips.
//
// The chip keeps showing the RAW wire value -- that is the portal's
// forward-compatibility convention for every opaque wire enum, and an
// unrecognised value from a newer backend must never be replaced by a
// misleading label. The tooltip is layered on top, and a value with no entry
// here gets NO tooltip: silence is honest, a wrong explanation is not.
//
// Both tables key on the FULL enum value. theming-and-i18n.md records the naming
// trap this invites: a table keyed on a truncated prefix compiles, type-checks,
// and shows a raw string to operators.
export const ENERGY_SOURCE_HELP_KEYS: Record<string, string> = {
  measured: 'energySourceHelpMeasured',
  estimated: 'energySourceHelpEstimated',
  modeled: 'energySourceHelpModeled',
  unpriceable: 'energySourceHelpUnpriceable',
  '': 'energySourceHelpPending',
};

export const BILLING_UNIT_HELP_KEYS: Record<string, string> = {
  image: 'billingUnitHelpImage',
  audio_second: 'billingUnitHelpAudioSecond',
};
```

- [ ] **Step 4: Run to verify it passes**

```bash
cd gateway/frontend && npx vitest run src/components/billingUnit.test.ts
```

Expected: PASS (all eight).

- [ ] **Step 5: Add the i18n keys, German and English together**

In `gateway/frontend/src/i18n.ts`, in the **de** block near the other `activityCol*` keys (~`:1848`):

```ts
  activityColBillingUnit: 'Abrechnungseinheit',
  activityColBillingQuantity: 'Abrechnungsmenge',
  activityNotTokenMetered: 'Nicht in Token abgerechnet',
  activityMixedUnitsHint: (n: number) =>
    `Summe ohne ${n} nicht in Token abgerechnete Anfrage(n).`,
  energySourceHelpMeasured:
    'Gemessen: echte Leistungstelemetrie des Servers deckt das Zeitfenster dieser Anfrage lückenlos ab.',
  energySourceHelpEstimated:
    'Geschätzt: die konfigurierte Wattzahl des Servers (estimated_watts), über das Anfragefenster integriert und mit gleichzeitigen Anfragen geteilt.',
  energySourceHelpModeled:
    'Modelliert: ein Koeffizient für Wh pro Ausgabe-Token wurde angewandt.',
  energySourceHelpUnpriceable:
    'Keine Grundlage: diese Anfrage wird nicht in Token abgerechnet, und der Server hat weder Telemetrie noch eine konfigurierte Wattzahl. Setze estimated_watts für diesen Server, um sie zu bepreisen.',
  energySourceHelpPending:
    'Noch nicht bepreist: der Energie-Reconciler hat diese Zeile noch nicht verarbeitet.',
  billingUnitHelpImage: 'Diese Anfrage wird pro Bild abgerechnet, nicht in Token.',
  billingUnitHelpAudioSecond:
    'Diese Anfrage wird pro Audiosekunde abgerechnet, nicht in Token.',
```

and the matching **en** block (~`:4090`):

```ts
  activityColBillingUnit: 'Billing unit',
  activityColBillingQuantity: 'Billing quantity',
  activityNotTokenMetered: 'Not token-metered',
  activityMixedUnitsHint: (n: number) => `Sum excludes ${n} request(s) that are not token-metered.`,
  energySourceHelpMeasured:
    'Measured: real power telemetry from the server covers this request’s window without gaps.',
  energySourceHelpEstimated:
    'Estimated: the server’s configured wattage (estimated_watts), integrated over the request window and shared with concurrent requests.',
  energySourceHelpModeled: 'Modeled: a Wh-per-output-token coefficient was applied.',
  energySourceHelpUnpriceable:
    'No basis: this request is not token-metered, and the server has neither telemetry nor a configured wattage. Set estimated_watts on this server to price it.',
  energySourceHelpPending: 'Not yet priced: the energy reconciler has not processed this row.',
  billingUnitHelpImage: 'This request is billed per image, not in tokens.',
  billingUnitHelpAudioSecond: 'This request is billed per second of audio, not in tokens.',
```

The type-checked build enforces parity, so a key present in one block only fails `npm run build`.

- [ ] **Step 6: Extend the API types**

In `gateway/frontend/src/api/usage.ts`, add to `UsageEvent` (after `energy_source`):

```ts
  // The non-token billable measure, an XOR with the token fields: billing_unit
  // "" (or absent) means TOKEN-METERED and the token fields are the measure;
  // any other value means billing_quantity is the measure and every token field
  // is 0 because tokens do not APPLY -- which is why those cells render an em
  // dash rather than a number. No producer writes a unit yet.
  billing_unit?: string;
  billing_quantity?: number;
```

to `UsageGroupRow`:

```ts
  // How many of `count` are not token-metered. 0 = all token-metered,
  // == count = none, in between = a mixed population whose token sums are
  // correct for the token-metered subset only.
  non_token_requests: number;
```

to the `StatTotals` type (near `total_requests`, ~`:228`):

```ts
  non_token_requests: number;
```

and to `DashboardMetrics`:

```ts
  non_token_requests_24h: number;
```

Mark the two `StatTotals`/`DashboardMetrics` additions optional (`?`) **only** if the neighbouring energy/cost fields are optional in that type; match the file's existing convention for additive fields.

- [ ] **Step 7: Add the two activity columns**

In `activityColumns.ts`, add to the `ColumnId` union (`:32`, beside `'energy_source'`):

```ts
  | 'billing_unit'
  | 'billing_quantity'
```

and the definitions, immediately after the `cost_eur` entry:

```ts
  // The non-token billable measure (#70). No producer writes a unit yet, so
  // every row shows "—/—" today; hidden by default. billing_unit copies
  // energy_source's shape exactly -- a free-text wire enum rendered as a chip,
  // non-sortable, em dash when empty.
  {
    id: 'billing_unit',
    labelKey: 'activityColBillingUnit',
    defaultVisible: false,
    sortable: false,
    numeric: false,
  },
  {
    id: 'billing_quantity',
    labelKey: 'activityColBillingQuantity',
    defaultVisible: false,
    sortable: false,
    numeric: true,
  },
```

- [ ] **Step 8: Update `activityColumns.test.ts` and run it**

`:105` asserts `DEFAULT_HIDDEN_COLUMNS` has length 19 — a real tripwire, now 21. Edit that assertion and add, next to the `energy_source` assertions (`:96-100`):

```ts
    expect(DEFAULT_HIDDEN_COLUMNS).toContain('billing_unit');
    expect(DEFAULT_HIDDEN_COLUMNS).toContain('billing_quantity');
    // The billing pair is grouped immediately after the cost column, the way
    // cost_eur is grouped after energy_source.
    expect(ids.indexOf('billing_unit')).toBe(ids.indexOf('cost_eur') + 1);
    expect(ids.indexOf('billing_quantity')).toBe(ids.indexOf('billing_unit') + 1);
```

```bash
cd gateway/frontend && npx vitest run src/components/activityColumns.test.ts
```

Expected: PASS.

- [ ] **Step 9: Write the failing `ActivityTable` tests**

Add to `gateway/frontend/src/components/ActivityTable.test.tsx`, inside the existing `for (const locale of ['de', 'en'])` loop so both locales are covered. The file's harness is `makeRow(overrides)` plus `renderTable({rows, columns})`:

```tsx
  // Every column visible, so the billing pair and the token cells are all in the DOM.
  const allColumns = ACTIVITY_COLUMNS;

  it('renders an em dash for every token cell of a non-token row', () => {
    // A non-token row's token columns are 0 because tokens do not APPLY.
    // Printing "0" five times would make a not-applicable indistinguishable
    // from a token-metered request whose upstream reported no usage object.
    renderTable({
      rows: [
        makeRow({
          billing_unit: 'image',
          billing_quantity: 3,
          input_tokens: 0,
          output_tokens: 0,
          total_tokens: 0,
          cached_tokens: 0,
          cache_write_tokens: 0,
          prompt_per_second: 0,
          tokens_per_second: 0,
        }),
      ],
      columns: allColumns,
    });

    const row = screen.getAllByRole('row')[1];
    // Five token cells, each an em dash rather than a bare 0.
    expect(within(row).getAllByText('—').length).toBeGreaterThanOrEqual(5);
    expect(within(row).getByText('image')).toBeInTheDocument();
    expect(within(row).getByText('3')).toBeInTheDocument();
  });

  it('renders token cells as numbers for a token-metered row', () => {
    renderTable({
      rows: [makeRow({ billing_unit: '', total_tokens: 30, input_tokens: 10, output_tokens: 20 })],
      columns: allColumns,
    });

    const row = screen.getAllByRole('row')[1];
    expect(within(row).getByText('30')).toBeInTheDocument();
    expect(within(row).getByText('10')).toBeInTheDocument();
    expect(within(row).getByText('20')).toBeInTheDocument();
  });

  it('explains a known energy source in a tooltip and invents none for an unknown value', async () => {
    renderTable({
      rows: [
        makeRow({ id: 'req_m', energy_source: 'measured' }),
        makeRow({ id: 'req_u', energy_source: 'unpriceable' }),
        makeRow({ id: 'req_x', energy_source: 'future_tier' }),
      ],
      columns: allColumns,
    });

    // The chip label is ALWAYS the raw wire value -- including the unknown one.
    expect(screen.getByText('measured')).toBeInTheDocument();
    expect(screen.getByText('unpriceable')).toBeInTheDocument();
    expect(screen.getByText('future_tier')).toBeInTheDocument();

    // A known value gets its explanation on hover; MUI renders the Tooltip
    // content into a portal, so wait for it.
    fireEvent.mouseOver(screen.getByText('unpriceable'));
    expect(await screen.findByText(t.energySourceHelpUnpriceable)).toBeInTheDocument();

    // An unknown value gets no tooltip at all: a wrong explanation is worse
    // than none.
    fireEvent.mouseOver(screen.getByText('future_tier'));
    await waitFor(() => {
      expect(screen.queryByRole('tooltip')).not.toBeInTheDocument();
    });
  });
```

`waitFor` is not currently imported in this file — add it to the `@testing-library/react` import. If the tooltip assertion proves flaky under MUI's enter delay, assert on the trigger's `aria-describedby`/`title` attribute instead of the portal content; the requirement is that a known value carries its explanation and an unknown one carries none.

- [ ] **Step 10: Run to verify they fail**

```bash
cd gateway/frontend && npx vitest run src/components/ActivityTable.test.tsx
```

Expected: FAIL — token cells render `0`, no `billing_unit` case exists, no tooltip.

- [ ] **Step 11: Implement `renderCell`**

In `ActivityTable.tsx`, import the helpers:

```tsx
import { BILLING_UNIT_HELP_KEYS, ENERGY_SOURCE_HELP_KEYS, isTokenMetered } from './billingUnit';
```

Replace the five bare token cases so each one guards on the unit. The five are `total_tokens`, `input_tokens`, `output_tokens`, `cached_tokens`, `cache_write_tokens`, and none has a fallback today:

```tsx
    case 'total_tokens':
      return tokenCell(row, row.total_tokens);
    case 'input_tokens':
      return tokenCell(row, row.input_tokens);
    case 'output_tokens':
      return tokenCell(row, row.output_tokens);
    case 'cached_tokens':
      return tokenCell(row, row.cached_tokens);
    case 'cache_write_tokens':
      return tokenCell(row, row.cache_write_tokens);
```

with a module-level helper beside `renderCell` — the em dash carries a tooltip so the reason is one hover away rather than a mystery:

```tsx
// A token count on a row that is not token-metered is NOT APPLICABLE, not zero.
function tokenCell(row: UsageEvent, value: number): ReactNode {
  if (isTokenMetered(row)) return value;
  return <Tooltip title={t.activityNotTokenMetered}><span>—</span></Tooltip>;
}
```

`renderCell` is a module-level function that does not close over `t`, so either move `tokenCell` inside the component where `t` is in scope, or give it a `t: Translation` parameter and thread it — read how the file already handles this for `t.captureLocked` at `:266` and follow the same approach.

Add the two new cases:

```tsx
    case 'billing_unit':
      return chipWithHelp(row.billing_unit ?? '', BILLING_UNIT_HELP_KEYS);
    case 'billing_quantity':
      return row.billing_quantity ? row.billing_quantity : '—';
```

and rework the existing `energy_source` case (`:114-119`) through the same helper:

```tsx
    case 'energy_source':
      return chipWithHelp(row.energy_source ?? '', ENERGY_SOURCE_HELP_KEYS);
```

```tsx
// An opaque wire enum, rendered as a chip with its RAW value plus an explanatory
// tooltip. The raw label is the portal's forward-compatibility convention: an
// unrecognised value from a newer backend must show as itself rather than as a
// misleading label -- and it gets NO tooltip, because a wrong explanation is
// worse than none.
function chipWithHelp(value: string, helpKeys: Record<string, string>): ReactNode {
  const body = value ? <Chip size="small" variant="outlined" label={value} /> : <span>—</span>;
  const helpKey = helpKeys[value];
  if (!helpKey) return body;
  return <Tooltip title={t[helpKey] as string}>{body}</Tooltip>;
}
```

Thread `t` the same way as `tokenCell`. If indexing `t` by a computed key fights the `Translation` type, add a narrow typed lookup rather than an `any` cast — this repo's build is type-checked and a cast here would let a missing key ship silently.

- [ ] **Step 12: Run to verify they pass**

```bash
cd gateway/frontend && npx vitest run src/components/ActivityTable.test.tsx
```

Expected: PASS.

- [ ] **Step 13: Implement the group table's three states**

In `ActivityGroups.tsx`, add to `GroupColId` (`:37-47`) and `GROUP_COLUMNS` (`:56-72`) a non-token count column, placed after `error_count`:

```tsx
  | 'non_token_requests'
```

```tsx
  {
    id: 'non_token_requests',
    labelKey: 'activityColBillingUnit',
    align: 'right',
    defaultVisible: false,
  },
```

Then make `cellValue` (`:216-239`) apply the three-state rule to the five token ids. `cellValue` returns `string | number`, so widen it to `ReactNode` (or return the `text` and render the `mixed` marker at the call site — read how the cell is rendered and pick whichever needs fewer changes; the requirement is that a mixed cell is visibly marked and its tooltip says what it excludes):

```tsx
      case 'total_tokens':
        return groupTokenCell(row, row.total_tokens);
      case 'input_tokens':
        return groupTokenCell(row, row.input_tokens);
      case 'cached_tokens':
        return groupTokenCell(row, row.cached_tokens);
      case 'cache_write_tokens':
        return groupTokenCell(row, row.cache_write_tokens);
      case 'output_tokens':
        return groupTokenCell(row, row.output_tokens);
      case 'non_token_requests':
        return row.non_token_requests;
```

```tsx
  // '' already means "token-metered" on the wire, so a GROUP needs a third state
  // the unit field cannot carry. non_token_requests supplies it as a count:
  // none, all, or some -- and in the "some" case the sum is genuinely correct
  // for the token-metered subset, so it is shown WITH a marker rather than
  // suppressed.
  function groupTokenCell(row: UsageGroupRow, value: number): ReactNode {
    const agg = tokenAggregate(value, row.non_token_requests, row.count);
    if (!agg.applicable) {
      return <Tooltip title={t.activityNotTokenMetered}><span>—</span></Tooltip>;
    }
    if (agg.mixed) {
      return (
        <Tooltip title={t.activityMixedUnitsHint(row.non_token_requests)}>
          <span>{agg.text}*</span>
        </Tooltip>
      );
    }
    return agg.text;
  }
```

Also fix the **expanded member table's** Tokens cell (`:379`), which is a bare `{m.total_tokens}` over a `UsageEvent`:

```tsx
                <TableCell align="right">
                  {isTokenMetered(m) ? (
                    m.total_tokens
                  ) : (
                    <Tooltip title={t.activityNotTokenMetered}><span>—</span></Tooltip>
                  )}
                </TableCell>
```

And resolve `formatEnergyWh(0)` at `:233` — see Step 15.

- [ ] **Step 14: Test the group table**

Add to `ActivityGroups.test.tsx`:

First add the new required field to `makeGroup`'s defaults (`ActivityGroups.test.tsx:20-35`), or every existing test in the file fails to type-check:

```tsx
    non_token_requests: 0,
```

Then the test, using the file's own `makeGroup` + render harness:

```tsx
  it('marks a mixed-unit group and dashes an all-non-token group', async () => {
    renderGroups({
      groups: [
        makeGroup({ key: 'all-tokens', key_label: 'All tokens', count: 10, non_token_requests: 0, total_tokens: 300 }),
        makeGroup({ key: 'mixed', key_label: 'Mixed', count: 10, non_token_requests: 4, total_tokens: 300 }),
        makeGroup({ key: 'no-tokens', key_label: 'No tokens', count: 10, non_token_requests: 10, total_tokens: 0 }),
      ],
    });

    const rowFor = (label: string) => screen.getByText(label).closest('tr') as HTMLElement;

    // Entirely token-metered: a plain number.
    expect(within(rowFor('All tokens')).getByText('300')).toBeInTheDocument();

    // Mixed: the sum is CORRECT for the token-metered subset, so it is shown
    // with a marker rather than suppressed, and the tooltip names what it excludes.
    expect(within(rowFor('Mixed')).getByText('300*')).toBeInTheDocument();
    fireEvent.mouseOver(within(rowFor('Mixed')).getByText('300*'));
    expect(await screen.findByText(t.activityMixedUnitsHint(4))).toBeInTheDocument();

    // Entirely non-token: tokens do not apply.
    expect(within(rowFor('No tokens')).getByText('—')).toBeInTheDocument();
  });
```

Use whatever this file's render helper is actually called (read the harness below `makeMember` — it takes a `PortalApi` stub whose `usageGroups` resolves the rows) and add `within` to the `@testing-library/react` import if it is absent.

```bash
cd gateway/frontend && npx vitest run src/components/ActivityGroups.test.tsx
```

- [ ] **Step 15: Resolve `formatEnergyWh(0)` and the four token tiles**

`formatEnergyWh(0)` returns `'0.0 Wh'` where the row table prints `'—'` — an asserted zero on two surfaces with no source column near them (`ActivityGroups.tsx:233`, `StatTiles.tsx:56`). Resolve it in `StatTiles.tsx:13-16`:

```tsx
// Watt-hours -> a compact display string: kWh above the 1000 Wh threshold (2
// decimals), else Wh (1 decimal). 0 renders an em dash, matching the row table's
// energy_wh cell and the cost formatter: an un-priced or unpriceable row has NO
// energy figure, and printing "0.0 Wh" asserts a measurement that was never
// made. Pure, exported for unit testing.
export function formatEnergyWh(wh: number): string {
  if (!wh) return '—';
  if (wh >= 1000) return `${(wh / 1000).toFixed(2)} kWh`;
  return `${wh.toFixed(1)} Wh`;
}
```

An existing test's **title** pins the old behaviour as intended (`StatTiles.test.tsx:182`: "defaults the energy tile to zero and the cost tile to an em dash when the totals omit them"). Rewrite that test — title included — rather than deleting it:

```tsx
      it('defaults both the energy and the cost tile to an em dash when the totals omit them', () => {
        render(<StatTiles t={t} totals={totals} {...showAll} {...defaultCostProps} />);
        // An absent energy total is UNKNOWN, not zero. It now matches
        // formatCost's long-standing 0/undefined convention instead of
        // contradicting it. Assert per TILE rather than counting em dashes, so
        // this stays as specific as the assertion it replaces.
        const energyTile = screen.getByText(t.activityEnergyTile).closest('div') as HTMLElement;
        expect(within(energyTile).getByText('—')).toBeInTheDocument();
        const costTile = screen.getByText(t.activityCostTile).closest('div') as HTMLElement;
        expect(within(costTile).getByText('—')).toBeInTheDocument();
      });
```

Then apply the three-state rule to the four token tiles in `StatTiles.tsx:44-61`. `byId` values are already `string`, so the dash needs no type change:

```tsx
  const tokenTile = (label: string, value: number) => {
    const agg = tokenAggregate(value, totals.non_token_requests ?? 0, totals.total_requests);
    return { label, value: agg.mixed ? `${agg.text}*` : agg.text };
  };
```

```tsx
    cached_tokens: tokenTile(t.activityCachedTokens, totals.cached_tokens),
    cache_write_tokens: tokenTile(t.activityCacheWriteTokens, totals.cache_write_tokens),
    input_tokens: tokenTile(t.activityInputTokens, totals.input_tokens),
    output_tokens: tokenTile(t.activityOutputTokens, totals.output_tokens),
```

`StatTile`'s DOM shape decides whether `.closest('div')` reaches the tile wrapper — read `shared/StatTile.tsx` first and use whatever container query actually scopes to one tile (a `role`/`aria-label`, or `closest('[class*=...]')`). The requirement is that each assertion is scoped to its own tile rather than counting occurrences on the page.

Add a `StatTiles.test.tsx` case:

```tsx
      it('dashes the token tiles when nothing in the population is token-metered', () => {
        render(
          <StatTiles
            t={t}
            totals={{ ...totals, total_requests: 10, non_token_requests: 10 }}
            {...showAll}
            {...defaultCostProps}
          />,
        );
        // Assert per tile, not by counting em dashes on the page.
        for (const label of [
          t.activityCachedTokens,
          t.activityCacheWriteTokens,
          t.activityInputTokens,
          t.activityOutputTokens,
        ]) {
          const tile = screen.getByText(label).closest('div') as HTMLElement;
          expect(within(tile).getByText('—')).toBeInTheDocument();
        }
      });

      it('marks the token tiles when the population is mixed', () => {
        render(
          <StatTiles
            t={t}
            totals={{ ...totals, total_requests: 10, non_token_requests: 4, input_tokens: 100 }}
            {...showAll}
            {...defaultCostProps}
          />,
        );
        expect(screen.getByText('100*')).toBeInTheDocument();
      });
```

If the file's `totals` fixture is typed as `StatTotals`, adding `non_token_requests` to it may be required for every existing case to type-check — set it to 0 there.

- [ ] **Step 16: The Dashboard tile**

In `Dashboard.tsx:41-45`:

```tsx
        {
          labelKey: 'tokens24h',
          value: tokenAggregate(
            dashboard.metrics.tokens_24h,
            dashboard.metrics.non_token_requests_24h ?? 0,
            dashboard.metrics.requests_24h,
          ).text,
          detailKey: 'tokens24hDetail',
        },
```

- [ ] **Step 17: Run the whole frontend gate**

```bash
cd gateway/frontend && npm test
```

```bash
cd gateway/frontend && npm run build
```

```bash
cd gateway/frontend && npm run format:check
```

Expected: all three PASS. `format:check` is a separate CI job that `npm test` and `npm run build` do **not** cover — run `npm run format` if it fails, then re-run the check.

```bash
cd gateway/frontend && npm audit --audit-level=moderate
```

- [ ] **Step 18: Commit**

```bash
git add gateway/frontend/src/
```

```bash
git commit -m "feat(portal): Never render a not-applicable token count as a zero

The Activity surfaces learn the (billing_unit, billing_quantity) pair. This
is the repo's own doctrine inverted: a measured value must never be
indistinguishable from a measured zero (formatLiveTps), and mixed billable
units add the converse -- a NOT-APPLICABLE must never be indistinguishable
from a zero.

Row level: the five token cells had no fallback at all and always printed
the raw number, so a non-token row would have printed '0' five times. They
now render an em dash with a tooltip saying why. Two new default-hidden
columns copy energy_source's shape for the unit chip and the numeric
sibling shape for the quantity.

Group level is the genuinely hard case, because '' already means
'token-metered' on the wire and a group needs a THIRD state it cannot carry.
non_token_requests supplies it as a count: none, all, or some -- and in the
'some' case the sum is correct for the token-metered subset, so it is shown
with a marker and a tooltip naming what it excludes rather than suppressed.
The same rule drives the stat tiles, the expanded member table and the
dashboard, through one shared helper instead of five copies.

energy_source chips gain explanatory tooltips, including for the new
'unpriceable' value, which needs one most: it tells the operator the remedy
is to set that server's estimated_watts. The chip still shows the RAW wire
value -- the portal's forward-compatibility convention -- and an
unrecognised value gets no tooltip, because a wrong explanation is worse
than none. Both tables key on the full enum value, the naming trap
theming-and-i18n.md warns about.

This needs per-value i18n keys, a narrow and deliberate exception to the
opaque-wire-enum convention: that convention is about LABELS, and the labels
here stay raw. A tooltip is an explanation, and it cannot exist without
per-value text.

formatEnergyWh(0) now renders an em dash instead of '0.0 Wh', matching the
row table and formatCost. An existing test's TITLE pinned the inconsistency
as intended, so that test is rewritten rather than deleted."
```

---

## Task 6: Documentation, the ADR, and the correction found in passing

**Files:**
- Modify: `docs/architecture/cross-cutting/telemetry-usage-observability.md` (§8.4.1 at `:514-560`, §8.4.4 at `:2057-2094`, §8.4.5 at `:2103`)
- Modify: `docs/architecture/reference/data-model.md:29` (field list), `:245` (heading count), `:467` (migration table)
- Modify: `docs/architecture/09-architecture-decisions.md` (append ADR-041)
- Modify: `docs/architecture/cross-cutting/persistence.md:148` (migration count)
- Modify: `docs/architecture/11-risks-and-technical-debt.md` (§11.1 table, §11.4 table)
- Modify: `docs/architecture/cross-cutting/compatibility-and-inference.md:740-741`

**Interfaces:**
- Consumes: every name landed in Tasks 1–5.
- Produces: ADR-041, cited from the code comments added in Task 3.

- [ ] **Step 1: Document the contract in §8.4.1**

In `telemetry-usage-observability.md` §8.4.1 ("The usage event"), add prose covering: the `(billing_unit, billing_quantity)` pair; the XOR with **all seven** token-denominated columns and why the two per-second rates are in it (`ComputeHistogram` drops zeros, so a stray rate would enter the histograms with nothing to fail); that `''` means token-metered and is a **one-way positive assertion**, unlike `energy_source`'s re-processable `''`; that a unit may only come from endpoint identity because 7 of 10 `recordUsage` call sites pass a zero `provider.Response`; and that there is deliberately **no** normalizer, because clamping an unknown unit to `''` would assert token-metering about a request that is not token-metered.

Note: §8.4.1 and §8.4.2 currently say **nothing** about a never-guess policy or about energy tiers, and no "never guess" statement exists anywhere near the usage event today. This is new prose, not an edit of existing text — do not go looking for a sentence to amend.

- [ ] **Step 2: Document the energy decision in §8.4.4 and §8.4.5**

In §8.4.4, next to the Tier 3 `modeled` branch: that the formula is a **token-only** surface; that a non-token row is stamped `unpriceable` instead, terminal by design, and why a stamp is mandatory (the oldest-first `LIMIT 500` selection over a 168h horizon means unstamped rows starve the reconciler); that the operator remedy is `estimated_watts`, which prices every endpoint kind at once through the token-blind Tier 2; and the **shape any future per-unit coefficient must take** — it carries its own declared unit and applies only on an exact match against the row's `billing_unit`, a mismatch yielding 0 Wh and never a wrong Wh. A bare coefficient without a declared unit is the same lie a scalar `units` column would have been.

In §8.4.5, name `UsageAggregateSince` as the CostBudget source of truth. It is currently undocumented anywhere in `docs/` — verify with `grep -rn UsageAggregateSince docs/` before and after.

- [ ] **Step 3: Update `data-model.md`**

Extend the `usage_events` field description at `:29`; bump the `:245` heading from "(80 migrations)" to "(81 migrations)"; append the v81 row after `:467`, matching the table's exact 3-column shape and citing ADR-005 for the wide type the way the migration-64 row at `:401` does:

```
| 81 | `usage_events_billing_unit` | Adds `usage_events.billing_unit` (`text not null default ''`) and `billing_quantity` (`double precision not null default 0`): the non-token billable measure as a (unit, quantity) pair. `''` means token-metered, so the defaults backfill all history truthfully and no LLM path changes. `double precision`, not `real`, per ADR-005. |
```

- [ ] **Step 4: Write ADR-041**

Find the highest existing ADR number and the exact template (`grep -n '^## ADR-' docs/architecture/09-architecture-decisions.md | tail -3`, then read the last one verbatim). Append one in that shape recording: **a billable unit is a (unit, quantity) pair, never a scalar** — because the natural unit differs per family, so one scalar would be a lie in three directions at once.

It must also name, explicitly, the two surfaces that would tempt a unit→token conversion:

1. `modeledEnergy`'s `coeff * OutputTokens` — the least-effort way to make Tier 3 produce a number for an image is to synthesize `OutputTokens`.
2. The EWMA calibration divisor in `energy_reconciler.go` — dividing `WhMarginal` by `billing_quantity` would write a per-image number into `energy_wh_per_token`.

And why neither is acceptable: a fabricated token count poisons `sum(total_tokens)`, which is the quota source of truth read by `PrincipalLimiter.Admit`.

- [ ] **Step 5: Update `persistence.md` and the risk/acceptance tables**

`persistence.md:148`: bump "an append-only, ordered slice `migrations` of 80 entries" to 81.

`11-risks-and-technical-debt.md` **§11.1** (Risk/Impact/Mitigation — verified to be the right format for this, unlike §11.4's Structure/Decision shape): a row for "CostBudget does not bite for a non-token request on a Tier-3-only host", mitigation "set that server's `estimated_watts`; Tier 2 is time-based and prices every endpoint kind".

**§11.4** (structures reviewed and deliberately kept): a row for "a non-token request consumes RequestQuota and CostBudget but never TokenQuota, and image volume cannot be capped separately from chat volume — same rate bucket, same request quota", with the note that the missing capability is owned by issue #119 and that #70 deliberately forbids a second limiter dimension.

Do **not** put "the `''` default is a one-way positive assertion" in either table — it is a semantic invariant of the column and belongs in the ADR text and the migration-table row's prose, which Steps 1, 3 and 4 already cover.

- [ ] **Step 6: Correct the recording-on-rejection claim**

`compatibility-and-inference.md:740-741` states that a pre-Resolve rejection is recorded as a usage event. Verified false for model-not-allowed, `PrincipalLimiter` admission-denied and server-override-forbidden — none of those paths reaches `recordUsage`; it is true only for a genuine `Resolver.Resolve`-stage failure. The two documents also use "admission" for two different gates: the CP4 capacity queue and the principal limiter.

Rewrite the sentence to say which rejections are recorded and which are not, and disambiguate the two "admission" gates by name. The new endpoints will make an operator ask "is my 429'd image request in usage?", which is why this belongs in the same pass.

- [ ] **Step 7: Run the docs gate**

```bash
make lint-docs
```

Expected: PASS. `scripts/check-docs.sh` enforces that intra-repo markdown links and `#anchors` resolve, that every file under `docs/architecture/` is reachable from its `README.md` index, and that `openapi.yaml` reads with no dangling `$ref`. No new doc file is added, so `README.md` needs no change — but any new `#anchor` you link to must exist.

`openapi.yaml` needs **no** edit: every portal JSON response is an opaque `{type: object}` (`:446-452`, `:1566-1582`), so there is no per-field schema to update. Verify rather than assume.

- [ ] **Step 8: Commit**

```bash
git add docs/architecture/
```

```bash
git commit -m "docs: Record the (unit, quantity) billable-measure decision as ADR-041

§8.4.1 gains the pair, the seven-column XOR and the reasons behind both:
why the two per-second rates are inside the invariant (ComputeHistogram
drops zeros, so a stray rate would enter the speed histograms with nothing
to fail), why '' is a one-way positive assertion unlike energy_source's
re-processable '', why a unit may only come from endpoint identity (7 of 10
recordUsage call sites pass a zero provider.Response, so a response-derived
unit would record a FAILED image request as token-metered), and why there is
deliberately no normalizer.

§8.4.4 gains the 'unpriceable' provenance, why a stamp is mandatory rather
than optional, the estimated_watts remedy, and the shape any future
per-unit coefficient must take: it carries its own declared unit and applies
only on an exact match, a mismatch yielding 0 Wh and never a wrong Wh.

§8.4.5 now names UsageAggregateSince as the CostBudget source of truth,
which was undocumented anywhere.

ADR-041 records that a billable unit is a (unit, quantity) pair and never a
scalar, and names both surfaces that would tempt a unit-to-token
conversion: modeledEnergy's coeff * OutputTokens, and the EWMA calibration
divisor. A fabricated token count would poison sum(total_tokens), the quota
source of truth.

§11.1 records the Tier-3-only cost gap with its mitigation; §11.4 records
that a non-token request consumes RequestQuota and CostBudget but never
TokenQuota, with the missing per-unit quota owned by #119.

Also corrects a claim found while tracing these paths:
compatibility-and-inference.md said a pre-Resolve rejection is recorded as a
usage event. That is false for model-not-allowed, limiter admission-denied
and server-override-forbidden -- none reaches recordUsage -- and true only
for a Resolve-stage failure. The two documents also used 'admission' for two
different gates, the CP4 capacity queue and the principal limiter, which the
new endpoints would have made actively confusing."
```

---

## Final verification (before the pull request)

- [ ] **Full gates, with the postgres leg provisioned**

```bash
make test-go
```

```bash
make test
```

```bash
cd gateway/backend && OP_AI_GATEWAY_TEST_POSTGRES_DSN='postgres://postgres:test@127.0.0.1:55432/optest?sslmode=disable' go test ./... 2>&1 | grep -v '^ok' | head -20
```

```bash
make lint
```

```bash
cd gateway/frontend && npm test && npm run build && npm run format:check
```

```bash
make test-e2e
```

- [ ] **Sonar**

```bash
make sonar-gate
```

```bash
make sonar-findings && make sonar-branch-findings
```

The `sonar-gate` verdict is **advisory** — SonarQube Community Build cannot compare a branch against main, and it never sees blame in a linked worktree. The authoritative "did my branch introduce this?" gate is `sonar-findings` + `sonar-branch-findings`: judge by branch-attributed findings, not the legacy total. If Docker or the server is unavailable, say so explicitly in the pull request rather than staying silent about a skipped gate.

- [ ] **Working-file cleanup — the last step before the PR**

```bash
git rm -r docs/superpowers && git rm -f docs/implementation-status.md 2>/dev/null; git status --short
```

```bash
git diff --name-only main...HEAD | grep -E 'docs/superpowers|docs/implementation-status' && echo "STOP: working files still in the PR diff" || echo "clean"
```

Everything durable from the spec and this plan must already live in `docs/architecture/` (Task 6) before these files are removed.

- [ ] **Push and open the pull request. Do not merge it.**

```bash
git push -u origin usage-billing-unit
```

Then open the PR with `gh pr create`, in English, summarising: the pair and its XOR, the v81 migration and the no-op proof, the `unpriceable` energy decision with the starvation argument, the `NonTokenRequests` disclosure instead of a `GROUP BY` widening, the portal rule, ADR-041, and the three corrections this branch makes to existing docs and issue claims. State the Sonar and postgres verification status explicitly.

## Notes for the implementer: what NOT to do

- Do **not** register an endpoint, add a `sessionEndpoint` iota member, or touch `principal_limits.go`. #71/#68/#69 own the endpoints; #119 owns per-unit quotas.
- Do **not** add a normalizer that clamps an unknown `billing_unit` to `''`.
- Do **not** widen the `GROUP BY` with `billing_unit`, and do **not** sum `billing_quantity` across a mixed population.
- Do **not** add a per-unit energy coefficient, a new `MappingStore` method, or change `ComputeEnergy`'s signature — 25 call sites and a generated tracing decorator hang off it.
- Do **not** touch `baselineCreateStatements` or `migration43FloatColumns`.
- Do **not** "fix" `UsageGroupDTO.TotalTokens`'s derivation from the four component columns; it is forced, because `GroupBucket` carries no stored total.
- Do **not** repair or drop a row that violates the XOR — log it and write it unmodified.
