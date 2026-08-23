# Design: Requested (pre-override) model in the activity list

**Issue:** [#7 — See premapping name of requested model in activity list](https://github.com/JLor08/op-ai-gateway/issues/7)
**Date:** 2026-08-23
**Status:** approved (user), pending implementation

## Problem

A client's request body names a model (e.g. `claude-sonnet`). Before routing,
`resolveModelOverride` (`internal/gateway/inference_handlers.go`) may replace it
via the API token's model override (catch-all `ModelOverride` or per-model
`ModelOverrideMap`). `recordUsage` stores only the post-override name
(`usage.Event.Model`) plus the post-mapping app model name
(`Event.ProviderModel`). When an override fires, the name the client actually
sent is lost — the operator cannot see in the activity list what was originally
requested.

Requirement (clarified with the issue author): all three stages must be
traceable per activity row:

1. **Requested model** — exactly as sent by the client (pre-override) — NEW
2. **Model** — effective gateway model (post-override) — exists (`model`)
3. **Server model** — app model name (post model_mapping) — exists
   (`provider_model` column)

## Decision

Persist a new `requested_model` field on every usage event, captured at parse
time, and show it as a **new, default-visible, sortable column** in the portal
activity list, placed directly before "Model".

Rejected alternatives:
- *Derive at read time from token override config*: wrong once the override
  configuration changes; historical truth must be captured at request time.
- *Store only when different from `model`*: sparse semantics complicate
  sorting/searching for no meaningful saving.

## Backend changes

- **`inference.Request`** (`internal/inference/types.go`): new field
  `RequestedModel string` — the raw model string from the parsed client body.
  Set wherever `inference.Request` is built for an inference call:
  - `buildInferenceRequest` (`inference_handlers.go`): `RequestedModel: shape.model`
    (the value BEFORE `resolveModelOverride` runs).
  - Native passthrough (`native_passthrough.go`): the `inference.Request` built
    there carries the body's model likewise.
- **`usage.Event`** (`internal/usage/recorder.go`): new field
  `RequestedModel string \`json:"requested_model"\`` , populated by
  `recordUsage` from `req.RequestedModel`. Always stored (also when equal to
  `Model`).
- **Search/sort** (`internal/usage`):
  - free-text `q` needle additionally matches `requested_model`
    (recorder.go matchesQuery),
  - `NormalizeSort` whitelist + the sort comparator accept `requested_model`.
- **Store drivers** (memory/sqlite/postgres, `internal/store`):
  - append-only migration **61** `usage_requested_model`:
    `ALTER TABLE usage_events ADD COLUMN requested_model TEXT NOT NULL DEFAULT ''`
    (sqlite + postgres; memory driver needs only the struct field).
  - insert/scan column lists extended; conformance test covers round-trip.
  - Not a "wide" numeric type — ADR-005 not applicable; TEXT suffices.
- **Portal DTO** (`internal/portal`): `UsageEvent` DTO passes
  `requested_model` through to the API (shape: same as `model`).

Existing rows read back `""` (column default) — meaning "unknown", not "same
as model" (we cannot know whether an override fired historically).

## Frontend changes

- **`api.ts`**: `UsageEvent` type gains `requested_model: string`.
- **`activityColumns.ts`**: new column `requested_model`, labelKey
  `tableRequestedModel`, `defaultVisible: true`, `sortable: true`, placed
  immediately BEFORE `model`.
- **`i18n.ts`**: `tableRequestedModel` — de: "Angefragt", en: "Requested"
  (both added together; the type-checked build enforces parity).
- **Rendering** (`ActivityTable.tsx` / `activityColumns` cell logic): plain
  text cell; empty value (legacy rows) renders "—".

## Out of scope (YAGNI, may come later)

- No `group_by requested_model` in usage groups.
- No dedicated exact-match filter parameter (free-text `q` covers search).
- No changes to CaptureDialog, dashboard tiles, or usage timeseries.

## Testing (TDD)

- **Store conformance** (`internal/store/conformance_test.go`): round-trip of
  `RequestedModel` across memory/sqlite/postgres; legacy row reads back `""`.
- **Gateway**: with a token whose override maps `a → b`, the recorded event
  has `RequestedModel == "a"` and `Model == "b"`; without override both equal.
  Same assertion once for the native-passthrough path.
- **Usage query**: `q` matches on requested_model; sort by `requested_model`
  accepted by NormalizeSort.
- **Frontend**: column present + default visible (activityColumns test),
  cell renders value and "—" fallback (ActivityTable test), i18n parity via
  type-checked build.

## Documentation updates (same branch)

- `docs/architecture/reference/data-model.md`: usage_events column.
- `docs/architecture/cross-cutting/telemetry-usage-observability.md`: the
  three-stage model-name trace (requested → model → provider_model).
- `docs/architecture/cross-cutting/compatibility-and-inference.md`:
  `inference.Request` field table gains `RequestedModel`.
