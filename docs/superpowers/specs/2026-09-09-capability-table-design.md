# Capabilities become their own table — design

**Goal:** one row per (mapping, capability) carrying the verdict, **who established it**, and when — replacing the wide columns migration 77 just added, the legacy `vision_capable` bool, the `is_mtp` flag, and the `live_progress_support` verdict. After this change **no capability lives in a column**. Per-capability provenance is what makes the operator's rule expressible: **a manual or benchmark verdict is never overridden by a probe.**

**Issue:** follow-up to #49 sub-project 2 (PR #66). Blocks PR B (Ollama capabilities), which would otherwise land more wide columns.

## 1. Why the wide-column shape has to go

PR #66 shipped `cap_vision/cap_video/cap_audio/cap_tools` plus a **shared** `capabilities_source`/`capabilities_checked_at`, and synced a definitive vision verdict onto the legacy `vision_capable` bool. Three things went wrong, and they are all the same thing:

1. **Provenance is shared, so it cannot answer "who said this".** The operator's requirement — manual and benchmark verdicts must win over a probe — needs to know who set a *specific* capability. One column per capability group cannot say it.
2. **`metrics_source` is mapping-wide and every writer overwrites it.** The obvious workaround (skip the sync when `metrics_source` is `manual` or `vision`) is broken: the context probe stamps `probe` on its own ~30s cadence and erases the marker, after which a probe would override a manual value anyway.
3. **The bool duplicates the truth, so it needs a sync — and the sync is where the bugs were.** PR #66's review found the sync could not repair a drifted bool; the fix made it re-assert on every sample; that re-assertion is exactly what overrides the operator. Meanwhile the write stamps `metrics_source = "vision"`, so a probe-derived verdict is **indistinguishable in the portal from one established by actually sending an image**. The column lies today.

A child table fixes all three at once, and it is the shape the repo already uses twice (`agent_runtime_spec_gpus`, `user_ui_preferences`).

Two further facts settled the remaining design questions:

- **Measured, not assumed:** with the natural primary key `(mapping_id, capability)`, adding a `LEFT JOIN … and capability = 'mtp'` to the resolver's per-request candidate query costs **≈ +6 µs** (17 µs → 23 µs at 5 000 mappings / 40 000 capability rows / 5 candidates per model; the plan confirms one index seek per candidate row). Two such filtered joins — the two capabilities this path needs, `mtp` and `live_progress` — are therefore ≈ +12 µs and still yield **one row per mapping**. Fetching *all* capabilities in one unfiltered join costs +79 µs and multiplies rows; it is not needed until many capabilities reach the request path. Against an inference request of tens of milliseconds upward either figure is noise, so **performance is not a reason to keep a derived column** — and without that reason, a derived column only reintroduces the duplication and the sync.
- **Nothing outside this feature reads the columns being dropped.** The seven `cap_*`/`capabilities_*` columns are read only by the portal DTO and the two write-back diff helpers. `vision_capable` is read by exactly one aggregation. `is_mtp` is read by exactly one scorer term.

## 2. The table

Migration **78**, following `agent_runtime_spec_gpus`' shape and `user_ui_preferences`' semantics:

```sql
create table if not exists model_mapping_capabilities (
  mapping_id text not null references model_mappings(id) on delete cascade,
  capability text not null,
  verdict    text not null,
  source     text not null,
  checked_at <dl.timestampType()> not null,
  primary key (mapping_id, capability)
)
```

- **`verdict`** is `'yes'` or `'no'`. **Absence of a row is "unknown"** — the three-state string becomes two states plus row presence, which is what the never-overwrite-with-unknown discipline actually wanted all along. `''` is never stored.
- **`capability`** is an open vocabulary, not an enum: `vision`, `video`, `audio`, `tools`, `mtp`, `live_progress` today; Ollama's `thinking`, `insert`, `embedding`, `image` and whatever ships next are rows, not migrations. That is the whole point. Go names constants only for the capabilities it reasons about; every other name is carried and displayed verbatim.
- **`source`** is `manual` | `vision_benchmark` | `llama_cpp_props` | `legacy` (a value inherited from a pre-78 column whose real origin is unknowable). PR B adds `ollama_show`. The `live_progress` verdict is written by the same two probe paths as the other `/props` capabilities, so it needs no source of its own.
- **`checked_at`** is `not null` — a row exists only because someone established it, so there is always a time. Both nullable `*_checked_at` columns disappear with the row-presence model, and `live_progress_checked_at`'s standing rule carries over unchanged: **diagnostics and the portal tooltip only, no decision logic reads it**.
- No extra index: the PK serves both the point lookup `(mapping_id, capability)` and the `mapping_id` prefix scan. A `(capability, verdict)` index would only serve a "which mappings can do X" query nobody asks — YAGNI.
- Deletion rides the SQL FK cascade plus the `foreign_keys(1)` pragma, mirrored by hand in `MemoryStore.deleteMappingLocked`. That mirror is enforced by `TestRoutingStoreDeleteApplicationAndMappingCascade`, whose comment demands a reader per per-mapping table — this change must extend it.

## 3. The precedence rule

> A probe writes a capability row only when there is no row for it, or the existing row's source is itself a probe source. A row whose source is `manual` or `vision_benchmark` is never overwritten by a probe.

Consequences worth stating:

- The operator's manual verdict is permanent until they change it. No lock needed, no `metrics_locked` interaction, no escape hatch to document — the rule *is* the mechanism.
- A benchmark verdict (an actual image sent to the model, in `accept` or `verify` mode) outranks `modalities.vision`, which only answers *acceptance*. That was the operator's reason: `verify` mode exists to filter models that accept an image without understanding it.
- A probe still repairs **its own** drift (source `llama_cpp_props` → probe may rewrite), so the convergence property PR #66 fought for survives where it was legitimate.
- `legacy` rows are probe-writable. They came from columns whose origin we cannot prove, so treating them as manual would freeze in a guess; treating them as probe-owned lets the truth re-establish itself on the next cycle.
- **There is no manual capability writer today** — `capabilities_source` only ever received `"llama_cpp_props"`. The one manual surface that exists is the mapping form's `vision_capable` checkbox, which must now write a `vision` row with source `manual` (see §6).
- The rule gives the operator something they do **not** have today: `live_progress_support` is probe-only, so there is currently no way to say "never send the live-progress parameters to this upstream" — for a build that accepts the field but misbehaves, the only lever is nothing at all. A `manual` `live_progress` row is that lever, for free, with no extra mechanism. (No UI for it in this change; the row is settable through the store and the rule honours it.)

## 4. Migration 78: backfill, then drop

Backfill runs before the drops, in the same migration.

| From | To |
|---|---|
| `cap_vision`/`cap_video`/`cap_audio`/`cap_tools` = `yes`/`no` | a row per non-empty verdict, source `llama_cpp_props` (the only value the column ever held), `checked_at` = `capabilities_checked_at` |
| `cap_extra` (JSON array of names) | one row per name, verdict `yes`, source `llama_cpp_props` — a reported extra capability is a positive assertion. Empty in practice today (llama.cpp never populates it) |
| `vision_capable = 1` | a `vision` row, verdict `yes`, source from `metrics_source`: `vision` → `vision_benchmark`, `manual` → `manual`, anything else → `legacy` |
| `vision_capable = 0` | a `vision` row, verdict `no`, **only** when `metrics_source ∈ {vision, manual}`; otherwise **no row**. A never-probed mapping must not enter history as "measured: no" — `false` conflated "no" and "unknown", and only those two provenances prove a measurement happened |
| `is_mtp = 1` | an `mtp` row, verdict `yes`, source `legacy` (the column is seeded from a *name heuristic* at creation and also operator-settable; the two are indistinguishable after the fact) |
| `is_mtp = 0` | **no row.** The heuristic's `false` means "the name did not match", i.e. unknown — not a measured no |
| `live_progress_support` = `supported`/`unsupported` | a `live_progress` row, verdict `yes`/`no`, source `llama_cpp_props` (the only writers are the two probe paths), `checked_at` = `live_progress_checked_at`, falling back to the migration's own timestamp when that column is null |
| `live_progress_support` = `''` | **no row** — the column's documented zero-value-means-never-determined, which is exactly row absence |

Behaviour preservation, checked per reader:

- Scorer: `is_mtp = 1` → `mtp` row `yes` → `+30`. `is_mtp = 0` → no row → unknown → no bonus. Identical.
- Models list: `vision_capable = false` → not advertised. No row → unknown → **fail-closed, still not advertised**. Identical. (The fold gains the ability to distinguish, but must not change what it advertises.)
- Dispatch: `wantsLiveProgress`' three-layer rule is fed by `Target.LiveProgressSupport`, whose vocabulary is unchanged — `supported` ⇔ a `yes` row, `unsupported` ⇔ a `no` row, `''` ⇔ no row. The rule's own logic (negative memo, then verdict, then shape clause) is **not touched**, and #52's existing tests pin it: they must keep passing with no assertion changes, which is this change's safety net on the one capability with request-shaping consequence.

Then dropped from `model_mappings`: `cap_vision`, `cap_video`, `cap_audio`, `cap_tools`, `cap_extra`, `capabilities_source`, `capabilities_checked_at`, `vision_capable`, `is_mtp`, `live_progress_support`, `live_progress_checked_at` — eleven columns, and with them every capability column on the table.

`model_mapping_benchmarks.vision_capable` is **untouched** — that is a benchmark run's historical record of what it measured, not current state.

Mechanics: the SQLite driver is `modernc.org/sqlite v1.53.0` (SQLite 3.53.x), which supports `alter table … drop column` but **not** `drop column if exists`; Postgres supports both. So the migration needs a `dropColumnIfPresent` helper mirroring `addColumnIfMissing`'s swallow-benign-error shape. SQLite additionally refuses to drop a column that is indexed or named in a PK/view/trigger — verified clear here: the only index on `model_mappings` is `idx_model_mappings_application on (application_id)`, and none of the eleven columns appears in it. `rawUp` remains the documented table-rebuild escape hatch if a future drop is blocked.

### This departs from a documented practice, deliberately

The repo's written rule (`persistence.md`) is "forward-only, append-only: an already-shipped migration is never edited or reordered" — a *new* migration that drops columns does not violate it. But migration 72 and ADR-033 record a **practice** of leaving superseded columns permanently inert ("append-only migration discipline (three store drivers)").

The difference that justifies the departure: `native_responses`/`native_messages` were long-established with consumers beyond their own feature. The columns dropped here shipped **three days ago** in PR #66 and are read by nothing outside the feature being rewritten; `vision_capable` and `is_mtp` have exactly one reader each, both moved in this change. **ADR-039 records the drop and draws that line explicitly**, so the next reader does not conclude that drops are now routine.

## 5. Store API

Replacing `UpdateMappingCapabilities(ctx, id, CapabilityVerdicts, at)`:

```go
// CapabilityRow is one (capability, verdict, source, checked_at).
type CapabilityRow struct {
	Capability string
	Verdict    string // "yes" | "no"
	Source     string
	CheckedAt  time.Time
}

MappingCapabilities(ctx, mappingID string) ([]CapabilityRow, error)
MappingCapabilitiesForMappings(ctx, mappingIDs []string) (map[string][]CapabilityRow, error)
UpsertMappingCapabilities(ctx, mappingID string, rows []CapabilityRow) error
DeleteMappingCapability(ctx, mappingID, capability string) error
```

- The upsert is `insert … on conflict(mapping_id, capability) do update set verdict = excluded.verdict, source = excluded.source, checked_at = excluded.checked_at` — one statement shape, both dialects, exactly `user_ui_preferences`' precedent. No `metrics_locked` guard anywhere: a capability is not a pinned metric (the #52 principle, now uniform because there is no bool left to lock).
- **`MappingCapabilitiesForMappings` is a batch reader — a new pattern for this repo**, which today has no batch child-collection reader at all (child collections are N+1 loops in Go). It is justified rather than gratuitous: the model-servers listing already costs ~31 queries at the documented scale of ~30 mappings, and two multipliers make a per-row read genuinely risky — the SSE stream recomputes the whole listing on every loaded-registry change, and model-group-servers calls the full listing **once per group member**. One `where mapping_id in (…)` keeps all of those flat. Chunk the `IN` list (1 000) so a pathological deployment cannot exceed a driver's parameter limit.
- `DeleteMappingCapability` is how an operator returns a capability to "unknown" — the state the old bool could not express.

## 6. Everything that changes

**Resolver / decision path.** `ActiveMappingsForModel` gains **two** filtered joins — one for `capability = 'mtp'`, one for `capability = 'live_progress'` — and drops the eleven columns from its select list. `ModelMapping.IsMTP`, `ModelMapping.VisionCapable` and `ModelMapping.LiveProgressSupport` are **removed from the struct** — deliberately not kept as sometimes-populated fields, which would be a footgun the moment a mapping loaded via `MappingByID` reported `false`/`''` while the resolver's reported the truth. Instead `MappingCandidate` carries `IsMTP bool` and `LiveProgressSupport string` filled from the joins; `scoringRoute` reads the first, `targetFrom` copies the second onto `Target` exactly as today. **Neither the scorer nor `wantsLiveProgress` is touched** — same `Route.IsMTP`, same `mtpBonus = 30.0`, same three-layer rule reading the same `Target` field.

**Write-backs.** `writeBackRuntimeCapabilities` (+ `changedCapabilityVerdicts`, `capabilityResolution`, `resolvedCapabilities`, `applyWritten`) and `applyCapabilityWrite` (+ `diffCapabilityVerdicts`) are rewritten against rows. `writeBackRuntimeLiveProgress` and the two `UpdateMappingLiveProgressSupport` call sites in the app-health pass fold into them: the `live_progress` verdict is just another row from the same `/props` document, so it stops needing a writer, a column and a compare-to-stored of its own. `UpdateMappingLiveProgressSupport` is deleted — and with it a sweep the implementer must not skip: that method is cited **by name** as the canonical rationale for "a capability is not a metric an operator pins numbers against" in doc comments across eight files (`routing/store.go`, `store/sqlite_applications.go`, `routing/memory_store.go`, `cmd/gateway/app_health.go`, `internal/gateway/agent_ingest.go`, `store/migrate.go`, `provider/model_info.go`, `portal/service_model_servers.go`). Those citations are repointed at the new table's own rationale, not merely deleted: the argument is still load-bearing — it is now the reason the table carries no `metrics_locked` guard — and a comment pointing at a removed method is the kind of drift this repo treats as a defect. `syncVisionCapable` is **deleted outright** — with one source of truth there is nothing to sync, which is the cleanest possible answer to the operator's requirement. Everything else those functions guard is preserved: the runtimes cap, per-`spec_id` ownership resolution with the cross-server `slog.Warn` rejection, the per-sample `runtime_model_probe` feature gate, best-effort (never reject a telemetry sample), and compare-to-stored so an unchanged verdict issues no write. The precedence rule from §3 is the one new guard.

**Detectors are untouched.** `detectCapabilities` is a pure `[]byte → Capabilities` function on both sides; only the shape of what happens to its output changes. The byte-identity requirement between the two module copies stands.

**Wire.** `sample.RuntimeSample.Capabilities` becomes an open-vocabulary keyed set rather than four fixed fields plus an unused `Extra`. `map[string]string` (capability → verdict) is the natural shape and matches the repo's existing keyed-set precedents; `nil` still means "an agent that predates capability detection", an empty map still means "detection ran, determined nothing". The gateway mirror follows. The agent's `collector.Capabilities` → wire conversion becomes a small mapping function.

**Portal.** `ModelServerDTO`'s seven `cap_*` fields become one `capabilities` array of `{capability, verdict, source, checked_at}`, filled from the batch reader. The frontend's `capabilityChips` renders one chip per `yes` — the four known capabilities first in fixed order, then unknown-vocabulary names verbatim with the neutral status (PR #66's decision) — the em-dash when there is nothing, and **a per-capability tooltip that finally names who established each verdict and when**. That is a real gain over the shared-provenance tooltip. `api/models.ts`'s `ModelServerRow` follows.

**Models list.** `modelsResponse`'s `visionOn` AND-fold reads the `vision` row instead of the bool, treating absence as not-capable (fail-closed, unchanged behaviour). This is the one reader that keeps the chat image gate working.

**Manual write path.** The mapping form's `vision_capable` checkbox must now write a `vision` row with source `manual` (and clear it — `DeleteMappingCapability` — when the operator sets it back to indeterminate). The `Service.UpdateMapping`/`CreateMapping` metric-provenance stamping (`metricValuesPresent`/`metricValueChanged`) drops its `VisionCapable`/`IsMTP` participation, since neither is a mapping column any more. A general capability editor for the other capabilities is **out of scope** — noted as future work; today only vision has a manual surface and this change preserves exactly that.

**Docs.** ADR-038 is rewritten (it currently enshrines the probe override that this change removes). New **ADR-039** for the shape change and the drop precedent. `data-model.md` gets the migration-78 row and a rewritten capability-fields section. `telemetry-usage-observability.md` §8.4.3's capability paragraphs, including the vision-override paragraph PR #66 added, are rewritten. `api-surface.md`'s DTO fields.

**`live_progress_support` is included, and here is the reasoning trail.** It was scoped out of the first draft on the grounds that it sits on #52's dispatch decision. That was caution about blast radius dressed up as a technical argument, and the facts do not support it: the stored verdict has exactly four readers, and the only one on a request path is `targetFrom` copying it onto `Target` — **once per request, out of the same `ActiveMappingsForModel` join that already runs**, i.e. precisely the shape `is_mtp` has. `wantsLiveProgress` then reads the copied `Target` field with no store access at all. Three things argue for inclusion: leaving one capability in a column defeats the purpose and guarantees a migration 79 repeating this dance for one field; its vocabulary maps onto the row model with no special handling; and it hands the operator a manual override they do not have today (§3). The residual risk is real but bounded — the rule's logic does not change, only where its input is read from, and #52's tests are the net.

**Out of scope, on purpose:** a general capability editor in the portal. Today only vision has a manual surface, and this change preserves exactly that; `manual` rows for the other capabilities are settable through the store and honoured by the precedence rule, but get no UI here.

## 7. Verification

- **Store conformance, both drivers** (`forEachRoutingStore`): upsert/read round-trip; `on conflict` replaces rather than duplicating; absence-is-unknown; `DeleteMappingCapability`; the batch reader returns exactly the requested parents and nothing else; the FK cascade drops a deleted mapping's rows (extending `TestRoutingStoreDeleteApplicationAndMappingCascade`, whose comment requires it).
- **The precedence rule, mutation-proven:** a probe upsert leaves a `manual` row and a `vision_benchmark` row untouched, and *does* replace a `llama_cpp_props` or `legacy` row. Removing the precedence check must fail a test.
- **Migration 78** on a seeded pre-78 database: every backfill row of the table in §4, including the two cases that must produce **no** row (`vision_capable = 0` without a measuring provenance; `is_mtp = 0`), and the columns actually gone afterwards on both dialects.
- **Behaviour preservation:** an `is_mtp = 1` mapping still scores `+30` after migration (assert through the scorer, not the row); a `vision_capable = false` mapping is still not advertised to the chat gate.
- **Resolver:** the joined `mtp` verdict reaches `Route.IsMTP` and the joined `live_progress` verdict reaches `Target.LiveProgressSupport`; a mapping with neither row gets no bonus and an empty verdict. All revert-verified.
- **#52's dispatch tests pass with no assertion changes** — the explicit gate on the one capability with request-shaping consequence. A `yes` row must make `wantsLiveProgress` behave exactly as a stored `supported` did, a `no` row as `unsupported`, and no row as `''`.
- **N+1 guard:** a query-counting test pins that the model-servers listing issues **one** capability query regardless of row count — the regression this batch reader exists to prevent.
- **Frontend:** column-scoped (`cellForColumn`) tests for chip order, the neutral status for unknown-vocabulary names, the em-dash, and the per-capability tooltip naming source and date.
- Every new or changed test revert- or mutation-verified; both Go modules' lint + tests, the frontend's `format:check`/lint/build/test, `check-docs.sh`, and Sonar `Attributed: 0`.
