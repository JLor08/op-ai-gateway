# Requested-Model Activity Column Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Persist the client's original (pre-token-override) model name on every usage event and show it as a default-visible, sortable "Requested" column in the portal activity list (issue #7).

**Architecture:** The raw model string is captured in `inferencePreflight` (the single shared gate all four inference edges pass through) as a new `inference.Request.RequestedModel` field, stored by `recordUsage` into a new `usage_events.requested_model` column (append-only migration 61, all three store drivers), and rendered by a new activity column placed before "Model". Spec: `docs/superpowers/specs/2026-08-23-requested-model-activity-design.md`.

**Tech Stack:** Go 1.25 (backend, stdlib-only), React/TypeScript/Vite + Vitest (frontend), SQLite/Postgres/memory store drivers.

## Global Constraints

- AGPL SPDX header on every new file: `// SPDX-License-Identifier: AGPL-3.0-only` + `// Copyright (C) 2026 OnPrem AI Gateway contributors` (first lines).
- Migrations are append-only; migration 61 is the next free version (60 is the latest shipped).
- All three store drivers (memory/sqlite/postgres) must keep working; postgres verification needs `OP_AI_GATEWAY_TEST_POSTGRES_DSN` set (skip locally if unavailable, CI covers it).
- i18n keys are added in German AND English together (`gateway/frontend/src/i18n.ts`); the type-checked build enforces parity.
- Frontend format/lint must stay clean: `npm run format:check`, `npm run lint`.
- Go lint: `golangci-lint run` per module must stay at 0 issues.
- Work happens in the worktree `.claude/worktrees/requested-model-activity` (branch `worktree-requested-model-activity`).

---

### Task 1: `usage.Event.RequestedModel` + memory-driver search/sort

**Files:**
- Modify: `gateway/backend/internal/usage/recorder.go` (Event struct ~line 29; `usageMatchesText` ~line 586; `compareUsage` ~line 608)
- Modify: `gateway/backend/internal/usage/stats.go` (sortWhitelist map, ~line 20)
- Test: `gateway/backend/internal/usage/recorder_requested_model_test.go` (new file)

**Interfaces:**
- Consumes: nothing new.
- Produces: `usage.Event.RequestedModel string` with JSON tag `requested_model`; sort key `"requested_model"` accepted by `usage.NormalizeSort`; free-text `q` matches it. Tasks 2 and 3 rely on this exact field name.

- [ ] **Step 1: Write the failing test** — new file `gateway/backend/internal/usage/recorder_requested_model_test.go`:

```go
// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package usage

import (
	"context"
	"testing"
	"time"
)

// The requested (pre-token-override) model is a first-class event field: it
// round-trips through the memory recorder, is reachable by the free-text q
// search, and is an accepted sort key (issue #7).
func TestRequestedModelRoundTripSearchAndSort(t *testing.T) {
	r := NewRecorder()
	base := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	events := []Event{
		{ID: "req_a", UserID: "u1", Model: "qwen-coder", RequestedModel: "gpt-oss-20b", CreatedAt: base},
		{ID: "req_b", UserID: "u1", Model: "qwen-coder", RequestedModel: "claude-sonnet", CreatedAt: base.Add(time.Minute)},
	}
	for _, e := range events {
		if err := r.Record(e); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}

	// Round-trip: the field survives storage.
	page, err := r.Query(context.Background(), Query{Limit: 25})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(page.Data) != 2 {
		t.Fatalf("events = %d, want 2", len(page.Data))
	}

	// Free-text q matches requested_model.
	page, err = r.Query(context.Background(), Query{Q: "gpt-oss", Limit: 25})
	if err != nil {
		t.Fatalf("Query(q): %v", err)
	}
	if len(page.Data) != 1 || page.Data[0].ID != "req_a" {
		t.Fatalf("q=gpt-oss matched %#v, want exactly req_a", page.Data)
	}

	// requested_model is a whitelisted sort key and sorts ascending.
	if got := NormalizeSort("requested_model"); got != "requested_model" {
		t.Fatalf("NormalizeSort(requested_model) = %q, want it whitelisted", got)
	}
	page, err = r.Query(context.Background(), Query{Sort: "requested_model", Order: "asc", Limit: 25})
	if err != nil {
		t.Fatalf("Query(sort): %v", err)
	}
	if page.Data[0].RequestedModel != "claude-sonnet" || page.Data[1].RequestedModel != "gpt-oss-20b" {
		t.Fatalf("sort order = [%s %s], want [claude-sonnet gpt-oss-20b]",
			page.Data[0].RequestedModel, page.Data[1].RequestedModel)
	}
}
```

Note: check `Recorder.Record`'s actual signature in recorder.go before writing (it may or may not take a context / return an error) and adjust the two call sites in the test accordingly — the assertions stay the same.

- [ ] **Step 2: Run test to verify it fails**

Run: `cd gateway/backend && go test ./internal/usage -run TestRequestedModelRoundTripSearchAndSort -v`
Expected: compile FAIL — `unknown field RequestedModel in struct literal`.

- [ ] **Step 3: Implement** — three edits:

In `recorder.go`, Event struct, directly under `Model string ...`:

```go
	// RequestedModel is the model name exactly as the client sent it, BEFORE
	// resolveModelOverride applied any token model override. Equal to Model when
	// no override fired. "" on rows recorded before migration 61 (unknown).
	RequestedModel string `json:"requested_model"`
```

In `recorder.go`, `usageMatchesText`, add one line to the OR chain:

```go
		strings.Contains(strings.ToLower(e.RequestedModel), needle) ||
```

In `recorder.go`, `compareUsage`, add a case next to `case "model":`:

```go
	case "requested_model":
		return cmp.Compare(a.RequestedModel, b.RequestedModel)
```

In `stats.go`, sortWhitelist map, add:

```go
	"requested_model":   true,
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd gateway/backend && go test ./internal/usage -v -run TestRequestedModelRoundTripSearchAndSort` then the whole package `go test ./internal/usage`.
Expected: PASS, package green.

- [ ] **Step 5: Commit**

```bash
git add gateway/backend/internal/usage
git commit -m "feat: requested (pre-override) model field on usage events"
```

---

### Task 2: store column — migration 61, SQL driver, conformance

**Files:**
- Modify: `gateway/backend/internal/store/migrate.go` (append migration 61 after version 60, ~line 93)
- Modify: `gateway/backend/internal/store/sqlite_usage.go` (insert column list ~line 60; scan/select lists ~lines 206, 229, 394; `usageSortColumns` ~line 397; free-text `q.Q` block ~line 493)
- Test: `gateway/backend/internal/store/conformance_test.go` (extend the existing usage-event round-trip)

**Interfaces:**
- Consumes: `usage.Event.RequestedModel` (Task 1).
- Produces: `usage_events.requested_model TEXT NOT NULL DEFAULT ''` readable/writable through the shared `SQLStore` (sqlite + postgres via the dialect seam); SQL-side sort key `requested_model` → column `e.requested_model`; SQL-side `q` LIKE includes the column.

- [ ] **Step 1: Write the failing test** — extend the store conformance suite. In `conformance_test.go`, find the usage-events round-trip case (the test that Records an event and asserts fields after Query). Add `RequestedModel: "gpt-oss-20b"` to its inserted fixture event and an assertion on read-back:

```go
	if got.RequestedModel != "gpt-oss-20b" {
		t.Fatalf("RequestedModel = %q, want gpt-oss-20b", got.RequestedModel)
	}
```

Also add two assertions mirroring Task 1 against the SQL driver (same conformance function, so all drivers run it): a `Query{Q: "gpt-oss"}` match and a `Query{Sort: "requested_model", Order: "asc"}` ordering — copy the assertion shapes from Task 1's test. If the conformance suite has a per-field table for usage events, extend that table instead of hand-writing assertions; follow the file's existing pattern.

- [ ] **Step 2: Run test to verify it fails**

Run: `cd gateway/backend && go test ./internal/store -run TestConformance -v 2>&1 | head -40`
(check the actual conformance test function name in the file and adjust `-run`).
Expected: FAIL — sqlite: `no such column: requested_model` (or a scan-count mismatch).

- [ ] **Step 3: Implement**

`migrate.go` — append to the migrations list after version 60:

```go
	{version: 61, name: "usage_requested_model", up: migration61Up},
```

and add the function next to the other small ALTER migrations (copy the style of the nearest single-column ALTER migration in the file):

```go
// migration61Up adds the requested (pre-token-override) model name to usage
// events (issue #7). TEXT NOT NULL DEFAULT '' so pre-existing rows read back
// "" = "unknown" (whether an override fired historically is not recorded).
func migration61Up(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx,
		`alter table usage_events add column requested_model text not null default ''`)
	return err
}
```

(Match the file's actual migration-func signature — copy it from `migration60Up`.)

`sqlite_usage.go` — four mechanical edits, keeping column order consistent between INSERT list, placeholders, and every SELECT/scan list:
1. INSERT: add `requested_model` to the column list (~line 60-64) and one `?` placeholder + `e.RequestedModel` arg in the matching position.
2. Every SELECT column list + `rows.Scan(...)` for usage events (~lines 206, 229, 394): add `requested_model` / `&e.RequestedModel` at the same position.
3. `usageSortColumns`: add `"requested_model": "e.requested_model",`.
4. `q.Q` block (~line 493): extend to `(... or e.requested_model `+il+` ?)` and append one more `like` arg.

- [ ] **Step 4: Run tests**

Run: `cd gateway/backend && go test ./internal/store`
Expected: PASS (memory + sqlite). If `OP_AI_GATEWAY_TEST_POSTGRES_DSN` is set locally, run again with it; otherwise note that CI covers postgres.

- [ ] **Step 5: Commit**

```bash
git add gateway/backend/internal/store
git commit -m "feat: persist requested_model on usage_events (migration 61)"
```

---

### Task 3: gateway plumbing — capture the pre-override name end-to-end

**Files:**
- Modify: `gateway/backend/internal/inference/types.go` (Request struct, next to `Model` ~line 111)
- Modify: `gateway/backend/internal/gateway/inference_handlers.go` (`inferencePreflight` ~line 488; `mergeInto` ~line 432)
- Modify: `gateway/backend/internal/gateway/native_passthrough.go` (the `inference.Request{...}` literal ~line 186)
- Modify: `gateway/backend/internal/gateway/inference_complete.go` (`recordUsage` Event literal ~line 664)
- Test: `gateway/backend/internal/gateway/server_test.go` (`assertOverrideDroveRouting` ~line 4889)

**Interfaces:**
- Consumes: `usage.Event.RequestedModel` (Task 1).
- Produces: `inference.Request.RequestedModel string` set for ALL inference paths (chat/responses/messages, translated and native passthrough); `recordUsage` stores it.

- [ ] **Step 1: Write the failing test** — extend the existing shared helper `assertOverrideDroveRouting` (server_test.go ~line 4899), which every override test (chat, responses, messages, map + catch-all variants) already funnels through. Replace its event assertion with:

```go
	events := recorder.All()
	if len(events) != 1 || events[0].Model != "qwen-coder" {
		t.Fatalf("usage events = %#v (want one with Model qwen-coder)", events)
	}
	// Issue #7: the PRE-override name the client actually sent must be kept.
	if events[0].RequestedModel != "gpt-oss-20b" {
		t.Fatalf("RequestedModel = %q, want gpt-oss-20b (the client's original request)", events[0].RequestedModel)
	}
```

Also add one non-override sanity check as a new test right below `TestChatCompletionAppliesTokenModelOverride` — requested == model when no override fires. Look at how the plain (non-override) chat tests seed their server (a token WITHOUT ModelOverride/ModelOverrideMap, e.g. the fixture used by the surrounding server_test.go chat tests) and reuse that fixture:

```go
// TestChatCompletionRecordsRequestedModelWithoutOverride: with no token
// override, requested and effective model are identical on the event.
func TestChatCompletionRecordsRequestedModelWithoutOverride(t *testing.T) {
	// reuse the nearest existing no-override fixture from server_test.go;
	// POST /v1/chat/completions with {"model":"qwen-coder", ...} and assert:
	//   events[0].Model == "qwen-coder" && events[0].RequestedModel == "qwen-coder"
}
```

(Write the body against the real fixture — the comment above states the required assertions; do not leave it as a stub.)

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd gateway/backend && go test ./internal/gateway -run 'Override|RequestedModel' -v 2>&1 | tail -20`
Expected: compile FAIL (`events[0].RequestedModel` undefined) is NOT possible — the field exists since Task 1 — so expected: assertion FAIL `RequestedModel = "", want gpt-oss-20b` for every override test.

- [ ] **Step 3: Implement** — four one-line-ish edits:

`internal/inference/types.go`, Request struct, directly under `Model`:

```go
	// RequestedModel is the model name exactly as the client sent it, before
	// any token model override rewrote Model. Recorded on the usage event so
	// the activity list can show the pre-override name (issue #7).
	RequestedModel string `json:"-"`
```

`inference_handlers.go`, `inferencePreflight` (line 488):

```go
	req := inference.Request{Model: resolveModelOverride(token, shape.model), RequestedModel: shape.model, APIFlavor: shape.apiFlavor, Stream: shape.stream}
```

`inference_handlers.go`, `mergeInto` (after `req.Model = pf.Req.Model`):

```go
	req.RequestedModel = pf.Req.RequestedModel
```

`native_passthrough.go`, the `inference.Request{...}` literal (~line 186), add:

```go
		RequestedModel:  pfReq.RequestedModel,
```

`inference_complete.go`, `recordUsage`'s `usage.Event{...}` literal, directly under `Model: req.Model,`:

```go
		RequestedModel:   req.RequestedModel,
```

- [ ] **Step 4: Run tests**

Run: `cd gateway/backend && go test ./internal/gateway -run 'Override|RequestedModel' -v 2>&1 | tail -8` then the full package `go test ./internal/gateway` (slow, ~2 min) and `golangci-lint run`.
Expected: all PASS, 0 lint issues.

- [ ] **Step 5: Commit**

```bash
git add gateway/backend/internal/inference gateway/backend/internal/gateway
git commit -m "feat: record the client's pre-override model on usage events"
```

---

### Task 4: frontend — API type, column, i18n, rendering

**Files:**
- Modify: `gateway/frontend/src/api/usage.ts` (UsageEvent shape, next to `provider_model` ~line 32)
- Modify: `gateway/frontend/src/components/activityColumns.ts` (column id union ~line 9; column list — insert BEFORE the `model` entry ~line 78)
- Modify: `gateway/frontend/src/components/ActivityTable.tsx` (sortable-ids list ~line 60 if `requested_model` should be sortable there — mirror how `model` is registered; cell switch ~line 105)
- Modify: `gateway/frontend/src/i18n.ts` (both language blocks: ~line 1128 area for de, ~line 2732 area for en)
- Test: `gateway/frontend/src/components/activityColumns.test.ts` and `gateway/frontend/src/components/ActivityTable.test.tsx` (extend existing suites)

**Interfaces:**
- Consumes: backend field `requested_model` on activity event rows (Tasks 1–3).
- Produces: column id `'requested_model'`, labelKey `tableRequestedModel` (de: `"Angefragt"`, en: `"Requested"`), default visible, sortable, rendered before `model`; empty value renders `"—"`.

- [ ] **Step 1: Write the failing tests.** In `activityColumns.test.ts`, follow the file's existing assertion style and add:

```ts
it('has requested_model as a default-visible, sortable column directly before model', () => {
  const ids = ACTIVITY_COLUMNS.map((c) => c.id);
  const reqIdx = ids.indexOf('requested_model');
  const modelIdx = ids.indexOf('model');
  expect(reqIdx).toBeGreaterThan(-1);
  expect(reqIdx).toBe(modelIdx - 1);
  const col = ACTIVITY_COLUMNS[reqIdx];
  expect(col.defaultVisible).toBe(true);
  expect(col.sortable).toBe(true);
  expect(col.labelKey).toBe('tableRequestedModel');
});
```

(Adjust the exported-array name to the file's actual export.) In `ActivityTable.test.tsx`, extend an existing row-rendering test's fixture with `requested_model: 'gpt-oss-20b'` and assert the cell text appears; add a second case with `requested_model: ''` asserting the cell shows `'—'`. Copy the query style (e.g. `screen.getByRole('cell', ...)`) from the surrounding tests.

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd gateway/frontend && npx vitest run src/components/activityColumns.test.ts src/components/ActivityTable.test.tsx 2>&1 | tail -6`
Expected: FAIL (column missing / cell missing).

- [ ] **Step 3: Implement**

`api/usage.ts` — in the usage-event type, next to `provider_model: string;`:

```ts
  requested_model: string;
```

(Also check the other two `provider_model` occurrences at ~lines 82 and 139 — one is likely a query/filter params type, one the event type; mirror `requested_model` ONLY where the shape represents an event row. Line 82's optional filter shape is out of scope.)

`activityColumns.ts` — add `| 'requested_model'` to the id union, and insert BEFORE the `model` entry:

```ts
  {
    id: 'requested_model',
    labelKey: 'tableRequestedModel',
    defaultVisible: true,
    sortable: true,
    numeric: false,
  },
```

`ActivityTable.tsx` — register `'requested_model'` wherever `'model'`/`'provider_model'` appear for sorting (line ~60 array) and rendering; cell case:

```ts
    case 'requested_model':
      return row.requested_model || '—';
```

(Match the file's exact cell-return idiom — if other text columns return raw strings and blank-handling happens elsewhere, follow that pattern but keep the visible `'—'` for the empty legacy value; the tests pin the behavior.)

`i18n.ts` — de block: `tableRequestedModel: 'Angefragt',` (near `activityColProviderModel`); en block: `tableRequestedModel: 'Requested',`.

- [ ] **Step 4: Run tests + build**

Run: `cd gateway/frontend && npx vitest run src/components/activityColumns.test.ts src/components/ActivityTable.test.tsx && npm test && npm run build && npm run lint && npm run format:check`
Expected: targeted tests PASS, full suite PASS (1334+ tests), type-checked build green (proves i18n parity), lint/format clean.

- [ ] **Step 5: Commit**

```bash
git add gateway/frontend/src
git commit -m "feat: requested-model column in the activity list"
```

---

### Task 5: architecture docs + full verification

**Files:**
- Modify: `docs/architecture/reference/data-model.md` (usage_events columns)
- Modify: `docs/architecture/cross-cutting/telemetry-usage-observability.md` (usage-event field description)
- Modify: `docs/architecture/cross-cutting/compatibility-and-inference.md` (§1 `inference.Request` field table)

**Interfaces:** none — documentation of Tasks 1–4.

- [ ] **Step 1: Update the three documents.**
  - `data-model.md`: add `requested_model` to the usage_events column list, one line, marked "since migration 61; '' on older rows".
  - `telemetry-usage-observability.md`: where usage-event fields/columns are described, add the three-stage trace sentence: requested_model (client, pre-override) → model (gateway, post-override) → provider_model (app name, post-mapping).
  - `compatibility-and-inference.md` §1 field table: add a `RequestedModel` row: "the client's original model name, pre-override; recorded on the usage event".
  Follow each file's existing table/list formatting exactly.

- [ ] **Step 2: Full verification**

Run from the worktree root: `make test-go` and `cd gateway/frontend && npm test && npm run build`, plus `make lint`.
Expected: everything green, 0 lint issues.

- [ ] **Step 3: Commit**

```bash
git add docs/architecture
git commit -m "docs: requested-model field in data model, telemetry, and inference docs"
```

---

### Task 6 (finish): pre-PR cleanup and push

- [ ] Remove branch-local working files per AGENTS.md: `git rm -r docs/superpowers` (and `docs/implementation-status.md` if present), commit `chore: remove branch-local working files`.
- [ ] Verify: `git diff --name-only origin/main...HEAD | grep -E 'docs/superpowers|implementation-status'` returns nothing.
- [ ] Push as a nicely named branch: `git push -u origin HEAD:refs/heads/feature/requested-model-activity` and hand the user the PR URL (PR title: `feat: requested-model column in the activity list (#7)`; description summarizes Tasks 1–5 and links issue #7 with "Closes #7").
