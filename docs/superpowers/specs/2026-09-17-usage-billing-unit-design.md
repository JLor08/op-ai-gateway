# A non-token billable unit for `usage_events`: a (unit, quantity) pair

Design for issue #70. Branch `usage-billing-unit`, cut from `main` @ `e1ada24`.

## 1. Goal and scope

Give `usage_events` a non-token billable unit as a **(unit, quantity) pair**, so
that image, speech and transcription requests can later be metered truthfully
instead of recording zeros into token columns.

**This is infrastructure with no producer.** No image, speech or transcription
endpoint is registered today — a repo-wide grep over `gateway/backend` for
`images/generations`, `images/edits`, `audio/speech` and `audio/transcriptions`
returns zero non-test hits — and none is added here. (The issue and its
reconnaissance comment both cite `internal/gateway/server.go:1146-1157` for
this; that range is `accessLogResponseWriter`, unrelated. The claim is true, the
citation is not.) The first producer arrives with #71 (`/v1/images/generations`),
#68 (`/v1/audio/speech`) or #69 (the multipart path). What #70 delivers is the
storage, the Go contract, the energy semantics, the aggregate honesty, the
portal presentation and the recorded decision — so that the first producer needs
only to fill in a unit and a quantity.

Consequence for verification: nothing in this change can be tested through an
HTTP path. Every behavioural test seeds `usage.Event` values directly.

## 2. The contract

`billing_unit == ""` means **token-metered**: the five token columns are the
measure and `billing_quantity` is meaningless. A non-empty `billing_unit` means
**not token-metered**: `billing_quantity` is the measure, and every token column
is 0.

This is deliberately `energy_source`'s existing shape — a value readable only
once its companion text column says how to read it. `""` must mean
"token-metered (the historical default)", so the migration's own default
backfills all history truthfully and no LLM path changes at all.

**The XOR extends past the five token columns.** `prompt_per_second` and
`tokens_per_second` are rates outside the five, and nothing in the contract as
originally worded forces them to 0. `ComputeHistogram` drops zeros by design
(`internal/usage/stats.go:106-120`), so a stray non-zero rate on a non-token row
would silently enter the speed histograms. Both columns are therefore part of
the invariant: a non-token row carries 0 in all seven.

**`""` is a one-way assertion.** Unlike `energy_source`'s `""` — a re-processable
sentinel — `billing_unit == ""` is a positive claim that gets no second chance.
Two things follow:

- The write path **never normalizes**. There is no clamp-to-`""`, because a
  clamp would silently relabel a non-token request as token-metered, which is
  the exact lie this issue exists to prevent. Unknown units are a programming
  error caught by the type system and by an invariant test, not smoothed over at
  runtime.
- The unit comes from **endpoint identity**, never from the upstream response.
  7 of the 10 `recordUsage` call sites pass a zero `provider.Response`, so a
  response-derived unit would record a *failed* image request as `""` —
  token-metered with a zero measure.

## 3. Storage

### 3.1 Migration v81

The chain head is v80 (`internal/store/migrate.go:115`,
`application_responses_live_timings`, from #82), so the pair lands in **v81**,
named `usage_events_billing_unit`.

```go
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

Shape template is **`migration72Up`** (two columns, one table, loop over
`addColumnIfMissing`) — not `migration80Up`, which is one column across two
tables.

Two frozen structures stay untouched:

- `baselineCreateStatements` is a point-in-time snapshot frozen as of v60
  (`migrate.go:404-411`); the double-write convention into it is retired. The
  new columns live only in their own migration.
- `migration43FloatColumns` is frozen to exactly v43's columns
  (`migrate.go:2137-2146`), which explicitly puts the type obligation on the
  later migration. `billing_quantity` is `double precision`, not `real`, so it
  satisfies `TestMigration43WidensRealToDoublePrecision`
  (`conformance_test.go:2356`) by construction rather than by enrolment. Note
  that migration43Up is postgres-only, so that invariant is only actually
  exercised with `OP_AI_GATEWAY_TEST_POSTGRES_DSN` provisioned.

`double precision` is written literally on both dialects (SQLite accepts it via
type affinity), so the dialect seam needs nothing.

### 3.2 The lockstep

`usage_events` has 39 columns on the write side and 38 on the read side —
`request_id` is written (`sqlite_usage.go:61`, from `event.ID`) and never
selected anywhere. Both new columns go **last**, after `created_at`, in every
list. Sites, in dependency order:

| Site | File | Arity 39→41 / 38→40 |
|---|---|---|
| `Record` column names | `sqlite_usage.go:60-65` | 39 → 41 |
| `Record` placeholders | `sqlite_usage.go:66` | 39 → 41 |
| `Record` positional args | `sqlite_usage.go:67-105` | 39 → 41 |
| `ByUser` select list | `sqlite_usage.go:202-208` | 38 → 40 |
| `All` select list | `sqlite_usage.go:227-231` | 38 → 40 |
| `usageEventColumns` | `sqlite_usage.go:393-397` | 38 → 40 |
| `scanUsageEvents` | `sqlite_usage.go:335-389` | 38 → 40 |
| `scanUsageRows` | `sqlite_usage.go:666-721` | 38 → 40, **before `&row.UserName`** |

`ByUser`'s and `All`'s lists are textual duplicates of `usageEventColumns`, not
uses of it, and nothing enforces that the three agree.

`scanUsageRows` is the one ordering trap: `Query` builds
`usageEventColumns + ", " + userNameExpr` (`sqlite_usage.go:640-641`), so
`&row.UserName` must stay **last** and the two new scan targets go before it.

`TimeSeries` builds its own 5-column SELECT (`sqlite_usage.go:306`) and scans
partial Events — it needs **no** change, since the XOR contract makes a
non-token row contribute 0 to both token sums it reads.

`usageSortColumns` (`sqlite_usage.go:399+`) and `usageWhere` need no change:
neither column is sortable or filterable in this change.

The memory store needs **no** field-level change — `Record`, `All` and `ByUser`
in `internal/usage/recorder.go` copy whole `Event` values.

### 3.3 Position pinning

A swap between the two *new* columns is loud: `database/sql`'s `convertAssign`
rejects a `float64` driver value into `*string`. The silent risk is a swap
against a **same-kind neighbour** — `billing_quantity` landing where
`prompt_per_second`/`energy_wh` belong, or `billing_unit` where `token_name`
belongs. `usage_events` has no nullable columns and 22 of 39 are `text` with a
`''` default, so such a swap scans cleanly into the wrong field.

There is no column-count guard today: `usage_events` is **not** enrolled in
`assertColumnCoverage` (`schema_column_coverage_test.go:49-91`, called only for
`applications`, `ai_servers`, `agent_runtime_specs`), and the only de-facto
full-field guard is one whole-struct `!=` comparison at
`sqlite_usage_test.go:1043`.

Two responses, both in scope:

1. A **position-pinning round trip**: seed distinct, non-zero/non-empty values
   into the new pair *and* its float/text neighbours, `Record`, then read back
   via `All` and `Query` and compare whole structs.
2. **Enrol `usage_events` in `assertColumnCoverage`**, so the next column added
   to this table cannot silently miss a reader. This is the mechanism that would
   have caught a dropped billing column automatically, and it is the durable
   half of the fix.

## 4. The Go contract and the producer seam

`usage.Event` (`internal/usage/recorder.go:21-98`) gains:

```go
BillingUnit     string  `json:"billing_unit"`
BillingQuantity float64 `json:"billing_quantity"`
```

Doc comment states the XOR explicitly, and states that no producer writes them
yet — the same shape as the existing `EnergyWh`/`EnergySource` comment
(`recorder.go:79-88`). It must not read like the adjacent `CostEUR` comment,
which emphatically says "never a DB column"; this pair is the opposite.

`usage.Row` embeds `Event` and is marshalled directly, so the pair appears on
the Activity list API with no DTO work.

Vocabulary is **two values**, as exported constants beside the existing
`Normalize*` helpers in `internal/usage/stats.go`:

```go
const (
	BillingUnitTokens      = ""            // token-metered: the five token columns are the measure
	BillingUnitImage       = "image"
	BillingUnitAudioSecond = "audio_second"
)

func ValidBillingUnit(s string) bool
```

`character` is dropped: it has no producer in this system. #68's metering signal
is `X-AudioCPP-Audio-Duration-Ms`, i.e. audio seconds — the same unit as
transcription. `character` comes from OpenAI's price list, not from here.

`ValidBillingUnit` is used by the invariant test and by any future query filter.
It is **not** used to coerce a write — see §2.

The insertion point for a producer is `usageMeta`
(`internal/gateway/inference_complete.go:630-634`), documented as "the
per-request facts that only the HTTP call site knows about the response it
actually wrote". It gains `BillingUnit string` and `BillingQuantity float64`,
copied straight into the `usage.Event` literal at `:663`. Because all ten call
sites build `usageMeta` as a **keyed** literal, every existing path writes
`""`/0 by Go zero value — the no-op is structural, not merely a migration
default.

## 5. Energy semantics

### 5.1 What already works

Tier 1 (telemetry) and Tier 2 (`EstimatedWatts`) are **purely time-based and
never read a token count** — `measuredEnergy` and `estimatedEnergy` do not even
receive `ev` (`energy_engine.go:223-231`). Two existing tests pin this with
zero-token events. So on any host with telemetry or `estimated_watts`, a
non-token request already gets a real Wh figure, and CostBudget already bites.
**Nothing about Tiers 1 and 2 changes.**

The gap is exactly one case: a **Tier-3-only host** (no telemetry coverage *and*
`estimated_watts` unset). There, `modeledEnergy` is literally
`coeff * ev.OutputTokens` (`energy_engine.go:430-437`) = 0 Wh, stamped
`Source: "modeled"`.

### 5.2 Why "leave it unpriced and retry" is disqualified

`energy_source = ''` is the reconciler's re-processing gate, so the tempting
option is to leave a non-token row unstamped until a time-based tier appears.
That is not a backlog — it is **head-of-line starvation of the whole
reconciler**. `UnpricedUsageEvents` is
`where energy_source = '' and created_at between ? and ? order by created_at asc, id asc limit ?`
(`sqlite_usage.go:144-150`; the memory store is identical,
`recorder.go:192-215`) with limit 500 (`energy_reconciler.go:21`), a 15 s tick
and a 168 h backfill horizon (`:32`). Once more than 500 never-priceable rows
sit inside that window they occupy the oldest end of every batch, and the
reconciler never reaches a newer event again — including ordinary token events
that Tier 1 would have measured. The age bound caps how long a row is retried,
not how much of the batch dead rows own.

`reconcileEnergyOnce`'s own doc states the invariant this would violate: every
event a pass looks at is *unconditionally* stamped.

So the fork is not "stamp vs don't stamp". It is **which non-empty string**.

### 5.3 The decision: a fourth, terminal `energy_source` value

`modeledEnergy` gains a leading guard, and a new const beside `maxSampleGap`:

```go
const energySourceUnpriceable = "unpriceable"

func modeledEnergy(ev usage.Event, mappingCoeff, sysDefaultWhPerToken float64) EnergyResult {
	if ev.BillingUnit != "" {
		// No Wh-per-output-token coefficient can apply to a row whose measure is
		// not tokens. Stamp a terminal provenance instead of a "modeled" zero:
		// the row leaves UnpricedUsageEvents' predicate, and the 0 is labelled as
		// "no model applies" rather than "the model says 0".
		return EnergyResult{Source: energySourceUnpriceable}
	}
	...
}
```

Four properties make this the right shape:

- **The guard must live inside `modeledEnergy`, not at `ComputeEnergy`'s call
  site.** Tier 3 has **two** entry points: `energy_engine.go:231` and
  `reconcileEnergyEvent`'s deleted-server shortcut
  (`energy_reconciler.go:150`). Gating at the call site would leave the
  deleted-server path stamping `modeled` on a non-token row — and that is the
  case where the gap is *permanent*, since a deleted server can never regain
  telemetry. One branch inside `modeledEnergy` covers both.
- **It keys on `ev.BillingUnit != ""`, deliberately not on
  `ev.OutputTokens == 0`.** That confines the new value to rows that cannot
  exist before v81, leaves every token path byte-identical — including the
  zero-coefficient row that `TestEnergyComputeTier3CoeffZeroStillModeled`
  (`energy_engine_test.go:531-541`) pins as desirable — and gives the same
  provable no-op-over-all-history property the migration has. It also respects
  the XOR rather than trusting it: a producer bug leaving `OutputTokens`
  non-zero can then never be multiplied by a Wh/token coefficient.
- **`energy_source` is open free text.** `migrate.go` contains zero `check (`
  clauses and declares the column `text not null default ''`; the only three
  producers are `energy_engine.go:272`, `:421`, `:436`; no consumer switches on
  it apart from the `== "measured"` calibration comparison; and the portal
  renders it verbatim into a Chip with an em-dash fallback
  (`ActivityTable.tsx:114-119`), described in `activityColumns.ts:203` as "a
  free-text provenance field rendered like an enum chip". A fourth value breaks
  no schema or wire contract.
- **It is reversible.** `UpdateUsageEventEnergy` (`sqlite_usage.go:121-132`)
  writes by id with **no** source predicate and is a benign no-op on 0 rows, so
  a future per-unit coefficient can re-stamp `unpriceable` rows with no schema
  change.

The string is `unpriceable` rather than `none` because it says *why* there is no
figure, not merely that there is none.

**A second-order gain:** with a terminal sentinel, `energy_source = ''` recovers
its real meaning. An em-dash then means "genuinely still settling" and nothing
else, which makes the unpriced set usable as a health signal for the first time.

### 5.4 The calibration guard, tightened

`energy_reconciler.go:208` currently reads
`res.Source == "measured" && ev.OutputTokens > 0 && mappingID != ""`. The
divide-by-zero at `:209` (`res.WhMarginal / float64(ev.OutputTokens)`) is
prevented today only by `OutputTokens > 0`, which holds only because a producer
in another package honours an unenforced convention. Add the explicit conjunct:

```go
if res.Source == "measured" && ev.BillingUnit == "" && ev.OutputTokens > 0 && mappingID != "" {
```

This is one of the two surfaces the ADR must name as a unit→token conversion
temptation: dividing `WhMarginal` by `billing_quantity` here would write a
per-image number into `energy_wh_per_token`.

### 5.5 What is accepted, not solved

On a Tier-3-only host a non-token request costs €0.00 forever, and all four cost
derivations read `energy_wh` with no `energy_source` predicate
(`sqlite_usage.go:884` — the CostBudget path, `:794`, `recorder.go:344`, and the
portal derivations). The sentinel **relabels** the row; it does not correct the
zero. This is recorded as a §11.1 risk row, not hidden.

A per-unit coefficient is explicitly **not** built here: it would be dead code
on the day it lands (nothing writes `BillingQuantity` yet, and the vocabulary a
coefficient would validate against is defined by issues that do not exist), it
changes `ComputeEnergy`'s signature across 25 call sites, and it adds a
`routing.MappingStore` method — which breaks the generated tracing decorator at
`internal/tracing/routingstore_gen.go:1291` until regenerated. The ADR records
the **shape** any later coefficient must take instead: it carries its own
declared unit and is applied only on an exact match against the event's
`billing_unit`; a mismatch yields 0 Wh, never a wrong Wh. A bare coefficient
without a declared unit is the same lie a scalar `units` column would have been.

The documented remedy for an unpriceable row is operational, and already
shipped: set that server's `estimated_watts`. Tier 2 then prices every endpoint
kind at once, dimensionally honestly, with no new column.

## 6. Aggregates

**Arithmetic needs no partitioning.** No aggregate on the group/tile path reads
the stored `total_tokens` at all: `StatTotals` (`usage/query.go:200-216`) and
`GroupBucket` (`:145-157`) carry only the four component columns, so
`UsageGroupDTO`'s derived `TotalTokens = input+output+cached+write`
(`service_usage_groups.go:117`) is **forced, not chosen**. Only
`UsageAggregateSince` (`sqlite_usage.go:877`) and the Dashboard
(`portal/service.go:1974`) read the stored total, and both correctly read 0 from
a non-token row. Request counts, error counts, concurrency, energy sums and
latency percentiles are correct by construction — a non-token request genuinely
belongs in all of them.

**The `GROUP BY` does not change.** A store-side key widening is invisible at
the API, because the portal folds by `Key` alone
(`service_usage_groups.go:89-95`); it would collide with the 500-group
truncation; and `exactFilter` (`ActivityGroups.tsx:86-121`) has no unit
dimension to drill down with. It buys cardinality cost with no benefit.

**What is missing is disclosure, not arithmetic.** A non-token row's "0 tokens"
is indistinguishable from a token-metered request whose upstream reported no
usage object. So one count is added, and `""` is not overloaded:

```sql
sum(case when billing_unit <> '' then 1 else 0 end)
```

carried as `NonTokenRequests int` on `StatTotals` and `GroupBucket`, and as
`non_token_requests` on `UsageGroupDTO`. Three states become expressible
without a reserved `"mixed"` sentinel in the unit namespace:

- `0` → the population is entirely token-metered; render numbers as today.
- `== total_requests` → entirely non-token; render `—`.
- in between → mixed; render the token sum **with a marker**, because that sum
  is arithmetically correct for the token-metered subset and the marker says
  what it excludes.

`billing_quantity` is **not** summed at group level. Summing images and audio
seconds into one number would be exactly the lie a scalar unit would have been.
Per-unit group volume belongs to whichever issue ships a producer.

The project rollups (`service_projects.go:1050-1075`) already consume
`GroupBucket`, so they carry the count for free: one accumulator field and one
DTO field. The Dashboard iterates events in Go (`portal/service.go:1969-1976`),
so it needs two lines.

There is **no** memory-vs-SQL conformance suite for `usage.Store`:
`conformance_test.go` is dual-**dialect** (sqlite + postgres), and memory parity
rests on hand-mirrored twin tests that both assert exact bucket counts over
all-`''` seeds. A mixed-unit seed must be added to **both** twins, or the new
count is unverified on one implementation.

## 7. Portal

The governing doctrine is this repo's own, inverted. `telemetry-usage-observability.md`
states that a measured value must never be indistinguishable from a measured
zero (implemented in `formatLiveTps`). Mixed units add the converse: **a
not-applicable must never be indistinguishable from a zero.**

### 7.1 Row-level

`renderCell` (`ActivityTable.tsx:67`) already receives the whole row, so no
signature change. The five token cells (`total_tokens`, `input_tokens`,
`output_tokens`, `cached_tokens`, `cache_write_tokens`) have **no** fallback
today — they always render the raw number — so this adds conditional rendering
rather than extending an existing pattern: `row.billing_unit ? '—' : row.X`.

Two new columns in `activityColumns.ts`, both hidden by default, copying
`energy_source`'s shape (`:220-225`) for the chip and the numeric siblings for
the amount:

- `billing_unit` — Chip with the **raw wire value**, `—` when empty.
- `billing_quantity` — numeric, `—` when 0.

### 7.2 Group-level, tiles, rollups

`ActivityGroups.tsx`: `cellValue` (`:216-239`) is exhaustive over `GroupColId`
with no default case, and the token fields are pure sums (`:222-231`). The
expanded-group member table's Tokens cell (`:379`) is a second surface. Both
follow the three-state rule from §6, driven by `non_token_requests`. A new
`GroupColId` + `GROUP_COLUMNS` entry carries the count.

`StatTiles.tsx:49-55` (four token tiles), `Dashboard.tsx:43` (`tokens_24h`) and
the project rollup renderings get the same three-state treatment.

`formatEnergyWh(0)` printing `0.0 Wh` where the row table prints `—` is resolved
to `—`, at both call sites (`ActivityGroups.tsx:233`, `StatTiles.tsx:56`). An
existing test's **title** pins the inconsistency as intended
(`StatTiles.test.tsx:182`), so this is a deliberate test edit, not a silent fix.

`activityColumns.test.ts:105` asserts `DEFAULT_HIDDEN_COLUMNS` has length 19 and
will need updating; `:96-100` pins `energy_source`'s position and is the pattern
to extend.

The charts need no change: the two t/s series are rate-over-bucket-seconds
(divisor is bucket seconds, not a row count) and the histograms drop zeros.

### 7.3 `energy_source` tooltips

Requested explicitly, and it is the natural completion of the sentinel: the
chip's value is only useful to an operator who knows what the tier means.

The chip keeps rendering the **raw wire value** — that is the repo's
forward-compatibility convention for every opaque wire enum, and an unrecognised
value from a newer backend must never be replaced by a misleading label. The
explanation is layered on top as a `Tooltip` (already imported and used in
`ActivityTable.tsx:20,266,394`), driven by a table keyed on the **full enum
value**:

| wire value | tooltip says |
|---|---|
| `measured` | real per-server power telemetry covered this request's window |
| `estimated` | the server's configured `estimated_watts`, integrated over the window and shared with concurrent requests |
| `modeled` | a Wh-per-output-token coefficient was applied |
| `unpriceable` | this request is not token-metered and the host has neither telemetry nor a configured wattage, so no energy basis exists — set the server's `estimated_watts` to price it |
| `""` (`—`) | not yet priced: the energy reconciler has not processed this row |

An unknown value renders the chip **without** a tooltip rather than with a
generic one — silence is honest, a wrong explanation is not. `theming-and-i18n.md`
records the naming trap this invites: the table must key on the full enum value,
because a table keyed on a truncated prefix compiles, type-checks and shows a
raw string to operators.

The same mechanism covers `billing_unit`'s chip (`image`, `audio_second`), since
#70 introduces that enum and the doctrine is identical.

### 7.4 i18n

German and English together, in `i18n.ts` (de block ~`:1846`, en block
~`:4088`); the type-checked build enforces parity.

Per-value keys are needed **here specifically**, which is a narrow and stated
exception to the opaque-wire-enum convention: the convention says a wire enum
needs no per-value *label* translation, and the chips indeed keep their raw
labels. A tooltip is not a label — it is an explanation, and it cannot exist
without per-value text.

Keys: `activityColBillingUnit`, `activityColBillingQuantity`,
`energySourceHelpMeasured`, `energySourceHelpEstimated`,
`energySourceHelpModeled`, `energySourceHelpUnpriceable`,
`energySourceHelpPending`, `billingUnitHelpImage`, `billingUnitHelpAudioSecond`,
and the mixed-population marker's label plus its tooltip.

## 8. Enforcement: descriptive column, not a limiter dimension

`internal/gateway/principal_limits.go` stays **byte-identical**. The limiter is
unit-blind and must remain so:

- `Admit` (`:205-230`) checks the rate bucket then `RequestQuota`, `TokenQuota`,
  `CostBudget`, fed by `UsageAggregateSince` (`sqlite_usage.go:869`) — which is
  two queries: `count(*), coalesce(sum(total_tokens), 0)` with no status or unit
  filter, plus a per-host `coalesce(sum(energy_wh), 0)` for the cost figure.
- `s.Limiter.Record(p, int64(resp.Usage.TotalTokens), 0)` stays unchanged. Its
  cost argument is architecturally always 0 at the one real call site
  (`inference_complete.go:774`), because pricing is asynchronous; CostBudget's
  responsiveness comes from the aggregate cache reloading once the reconciler
  has priced the row.
- Persistent request-quota accounting comes from the `usage_events` **row**, not
  from `Limiter.Record` — so a future handler that writes its own response gets
  rate limiting and `Admit` but no durable quota consumption. That is a note for
  #71/#68/#69.

The operator-visible story that falls out: a non-token request consumes
**RequestQuota and CostBudget, never TokenQuota**, and image volume cannot be
capped separately from chat volume — same rate bucket, same request quota. This
is recorded explicitly rather than left emergent, and the missing capability now
has an owner: **#119** (a per-unit quota dimension).

No second `admitPrincipal` call site is added. There is exactly one
(`inference_handlers.go:391`), and its comment records what broke when there
were four.

## 9. Documentation

- `telemetry-usage-observability.md` §8.4.1 ("The usage event", `:514-560`) —
  the pair, the XOR contract including the two per-second columns, and the
  never-guess policy for the unit. Note: §8.4.1/§8.4.2 do **not** currently
  contain a never-guess statement or anything about energy tiers; this is new
  prose, not an edit of existing text.
- `telemetry-usage-observability.md` §8.4.4 (energy attribution, `:2057-2094`) —
  the `unpriceable` tier value, that Tier 3's formula is a token-only surface,
  and the shape any future per-unit coefficient must take.
- `telemetry-usage-observability.md` §8.4.5 — name `UsageAggregateSince` as the
  CostBudget source of truth. It is currently undocumented anywhere.
- `reference/data-model.md:29` — extend the `usage_events` field list; `:245` —
  bump "(80 migrations)" to 81; `:467` — append the v81 row, citing ADR-005 for
  the wide type the way row 64 does.
- `09-architecture-decisions.md` — **ADR-041**: a billable unit is a (unit,
  quantity) pair, never a scalar. Names both unit→token conversion temptations:
  `modeledEnergy`'s `coeff * OutputTokens`, and the EWMA calibration divisor.
- `cross-cutting/persistence.md:148` — bump the migration count to 81.
- `11-risks-and-technical-debt.md` §11.1 — a Risk/Impact/Mitigation row for
  "CostBudget does not bite for non-token requests on a Tier-3-only host"
  (mitigation: set `estimated_watts`). §11.4 — a Structure/Decision row for
  "RequestQuota + CostBudget but never TokenQuota for non-token requests".
  §11.1's format, not §11.4's, for the first — verified against both tables.
- `cross-cutting/compatibility-and-inference.md:740-741` — **a factual
  correction found in passing**: the doc claims a pre-Resolve rejection is
  recorded as a usage event. That is false for model-not-allowed, limiter
  admission-denied and server-override-forbidden (none reaches `recordUsage`);
  it is true only for a `Resolver.Resolve`-stage failure. The two documents also
  use "admission" for two different gates (the CP4 capacity queue vs the
  principal limiter). Both get disambiguated, because the new endpoints will
  make an operator ask "is my 429'd image request in usage?".

`openapi.yaml` needs **no** change: every portal JSON response is an opaque
`{type: object}` (`reference/openapi.yaml:446-452`, `:1566-1582`), so there is
no per-field schema to update.

## 10. Verification

- Full migration chain on **SQLite and PostgreSQL**. The postgres subtests skip
  *silently* without `OP_AI_GATEWAY_TEST_POSTGRES_DSN`; a two-column migration
  is exactly the change that needs that leg provisioned like CI.
- **No-op invariant:** against a DB seeded with pre-existing token-metered rows,
  every row reads back `''`/0 after v81, and every aggregate
  (`Stats`/`UsageGroups`/`TimeSeries`/`UsageAggregateSince`/Dashboard) produces
  identical output before and after.
- **XOR invariant:** a row never carries both a non-empty unit and a non-zero
  value in any of the seven columns — the five token columns **plus**
  `prompt_per_second` and `tokens_per_second`.
- **Position pinning:** the round trip of §3.3, plus `usage_events` enrolled in
  `assertColumnCoverage`.
- **Energy:** a non-token event with a positive mapping coefficient *and* a
  non-zero `OutputTokens` (a contract violation) still yields `unpriceable`, not
  a fabricated figure; both Tier 3 entry points, including the deleted-server
  shortcut, produce it; a non-token event with telemetry still gets Tier 1, and
  with `estimated_watts` still gets Tier 2; every existing token-path energy
  test passes unchanged, including `TestEnergyComputeTier3CoeffZeroStillModeled`.
- **Aggregates, twinned:** one token row plus one non-token row seeded in both
  the memory twin and the SQL conformance test — `Stats.Totals` sums only the
  token row's components, `ComputeHistogram` excludes the non-token row,
  `UsageGroups` reports the right `NonTokenRequests`, `TimeSeries` still bumps
  `Connections`/`Concurrency`/`EnergyWh` for the non-token row.
- **Quota/budget:** unchanged for token requests, correct for a zero-token
  request.
- **Portal:** token cells render `—` for a non-token row and numbers for a
  token-metered one; group cells cover all three states; each `energy_source`
  value shows its tooltip and an unknown value shows none.
- **Gates:** `make lint` (Go + docs), `golangci-lint fmt --diff` and `run` per Go
  module, `npm test`, `npm run build`, `npm run format:check`, `make test-go`,
  `make test`, and the local Sonar gate plus `make sonar-findings` /
  `make sonar-branch-findings`.

## 11. Out of scope

Handed to #71/#68/#69, and to be recorded as comments on them:

- Registering any endpoint, adding a `sessionEndpoint` iota member, or calling
  `inferencePreflight` with a new api flavor.
- **Multipart cannot reach the gate today.** `inferencePreflight` requires a
  non-empty model resolved before it runs, and the only pre-parse resolver,
  `sniffRoutingModel` (`native_passthrough.go:147-151`), is a `json.Unmarshal`.
- **`session_extract.go`'s two switches have no default branch** (`:69-79`,
  `:92-122`), so adding an iota member alone leaves `session_id`/`session_source`
  empty on every new-endpoint row and silently disables session affinity.

Handed to #119: any quota or budget denominated in `billing_quantity`.

Deferred by decision: a per-unit energy coefficient (§5.5); per-unit
`billing_quantity` sums in group aggregates (§6); any `billing_unit` query
filter or sort.

## 12. Task breakdown

1. Storage — migration v81, the `usage.Event` fields, the eight lockstep sites,
   the position-pinning round trip, `assertColumnCoverage` enrolment.
2. Producer seam — `usageMeta` fields, the vocabulary constants,
   `ValidBillingUnit`, the structural no-op test and the XOR invariant test.
3. Energy — the `modeledEnergy` guard, the `unpriceable` const, the tightened
   calibration guard, tests for both Tier 3 entry points.
4. Aggregates — `NonTokenRequests` through `StatTotals`, `GroupBucket`,
   `UsageGroupDTO`, both store implementations, the project rollups, the
   Dashboard, and the twinned mixed-unit tests.
5. Portal — the two columns, the five token cells, the three-state group/tile
   rendering, `formatEnergyWh(0)`, the tooltips, de+en i18n, and the frontend
   test edits.
6. Docs — §8.4.1/§8.4.4/§8.4.5, `data-model.md`, ADR-041, `persistence.md`,
   §11.1/§11.4, and the `compatibility-and-inference.md` correction.
