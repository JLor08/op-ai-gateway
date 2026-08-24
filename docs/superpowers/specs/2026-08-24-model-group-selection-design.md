# Design: model-group selection — loaded-only, speed order, minimum speed

**Date:** 2026-08-24
**Status:** approved (user), pending implementation

## Problem

A `ModelGroup` today walks its members in one fixed manual **priority** order.
Three operator needs are unmet:

1. Serving a request must be able to avoid **triggering a model load**: prefer a
   member that is already resident, and only load something when nothing is.
2. The member choice should be able to follow **measured generation speed**
   instead of a hand-maintained order — "serve on the fastest model".
3. A group should be able to demand a **minimum generation speed**, so a
   too-slow model does not count as available at all.

All three must be combinable ("the fastest currently-loaded member"), and a
conversation must keep landing on the same model — switching only when another
is *materially* faster, and only when the group's failover mode allows it.

## Decisions

Four new, independent, combinable settings on `ModelGroup`. All default to
today's behavior, so every existing group is unaffected.

| Setting | Type / values | Default | Meaning |
|---|---|---|---|
| `loaded_only` | bool | `false` | Only currently-loaded members count as available |
| `member_order` | `priority` \| `speed` | `priority` | Member ordering for the walk |
| `climb_speed_margin_percent` | int (0–1000) | `20` | How much faster a member must be before a speed-ordered `climb_up` leaves the pin |
| `min_tokens_per_second` | float ≥ 0 | `0` (off) | Minimum generation speed a candidate must reach to be available |
| `min_speed_fallback` | `error` \| `ignore` | `error` | What happens when nothing reaches the minimum |

(Five rows, four features: the last two belong together.)

### The speed metric

Both speed features use **`effectiveGenTPS`** (`internal/routing/scorer.go`),
the existing load-aware effective speed that interpolates a mapping's
`GenTokensPerSecond` toward its `GenTokensPerSecondAtCapacity` by current load,
collapsing to the flat measured value on an idle server and to exactly 0 when
unmeasured. Reasons to reuse it rather than the raw `GenTokensPerSecond`:

- It is the codebase's existing, documented notion of "how fast will this
  actually generate right now", already used by `Score()`.
- It is load-aware, so a fast but saturated server cannot win "fastest"
  dishonestly.
- Unmeasured collapses to 0, which lines up with the agreed rules below
  (unknown sorts last, and unknown never satisfies the floor).

**Consequence to document:** because the metric is load-aware, a member can drop
below `min_tokens_per_second` purely because it is busy. That is intended — the
floor is a promise about the request being served — and `min_speed_fallback`
covers the case where it empties the set.

### 1. `loaded_only`

- Only members with at least one **loaded** candidate are available.
  "Loaded" is the existing `LoadedModelChecker` / `modelLoadedOn` the resolver
  already uses for the prefer-loaded partition and `climb_up`'s free climb.
- **Fallback:** if no member is loaded, the filter is dropped for this request
  and the ordinary walk runs (which may load a model). Never a dead end.
- **No warming.** With `loaded_only`, `climb_up` must NOT call
  `ModelWarmer.Warm`: warming exists to prepare a cold higher-priority member,
  which is precisely what this flag is meant to avoid. For such a group the
  preferred member is by definition an already-loaded one.
- **Reported loaded state changes for these groups only.** Today
  (`internal/portal/service.go`, the `groupLoaded` map) a group is "loaded" iff
  its highest-priority offerable member is loaded. For a `loaded_only` group it
  becomes **at least one** offerable member loaded, and `LoadedOn` is the union
  of those members' servers. Rationale: in such a group any loaded member is
  what will actually be served, so "at least one" is the honest signal; in a
  normal group the top member is what will be served, so the existing rule stays
  honest there. Non-`loaded_only` groups keep today's rule unchanged.

### 2. `member_order = speed`

- Members are ordered by their **fastest currently-eligible candidate's**
  `effectiveGenTPS`, descending. A member's speed is that maximum, because the
  member will in fact be served on one of those candidates.
- The manual priority order is **ignored** for ordering.
- **Unknown (0) sorts last**, and ties — including an all-unmeasured group —
  fall back to the manual priority order, so the order is always total and
  deterministic and a group with no measurements behaves exactly like today.

### 3. `climb_speed_margin_percent`

- Applies to **speed-ordered** groups only. With `member_order = priority`,
  `climb_up` keeps climbing on priority with no margin — there is no fluctuating
  measurement involved.
- A speed-ordered `climb_up` leaves an available pin only for a member whose
  speed exceeds the pin's by at least the margin:
  `candidate > pinned × (1 + margin/100)`.
- Session stickiness is otherwise untouched: the pin
  (`RouteAffinity{APITokenID, Model, APIFlavor, SessionID}`) is checked **before**
  any ordering, `sticky` never leaves an available pin, and the existing
  free-climb rule still applies — `climb_up` switches immediately only to an
  **already-loaded** target, otherwise it warms (unless `loaded_only`) and keeps
  serving. A fluctuating measurement therefore cannot start a cold model.

### 4. `min_tokens_per_second` + `min_speed_fallback`

- The floor filters **candidates**, not just members: a member counts as
  available iff it has at least one candidate whose `effectiveGenTPS` reaches the
  floor. Filtering only members would leave a hole — a member with a 50 tok/s and
  a 10 tok/s server would qualify at floor 20, and if the fast server is at
  capacity the 10 tok/s one would be served, breaking the promise.
- An **unmeasured** candidate (0) never satisfies the floor: only demonstrably
  fast candidates count. A newly added model is considered once it has been
  benchmarked.
- **When nothing reaches the floor:** `error` → `ErrNoHealthyHost` (503, the
  existing "members exist but all are gated" mapping); `ignore` → the request is
  resolved again with the floor dropped.
- Available for **both** orderings: the floor filters, the order sorts. Keeping
  it independent avoids a state-dependent UI and a special rule to explain.

### Evaluation order

Per request, inside the group path:

1. **Pin check** — session stickiness wins first, but **both filters also gate
   the pin's availability** (see below).
2. **Floor filter** (`min_tokens_per_second`). Empty → `min_speed_fallback`
   decides: error, or continue without the floor.
3. **Loaded filter** (`loaded_only`). Empty → drop this filter only; the floor
   (if it survived step 2) stays applied.
4. **Order** — `priority` (manual) or `speed`.
5. **Existing walk**: sticky/climb_up, capacity cap, admission queue, affinity
   pinning — all untouched.

Each filter has its own, clearly ordered fallback, and the existing selection
machinery is not modified.

**The filters must gate the pin, not only the walk.** The pin is checked before
the ordering, so a pin that escaped the filters would defeat them:

- A pinned candidate below `min_tokens_per_second` would be served and break the
  very guarantee the floor states. So a pin that no longer reaches the floor
  counts as **not available** and falls through to the walk — the same treatment
  the pin already gets when it is down or at capacity. If the walk then finds
  nothing above the floor, `min_speed_fallback` applies as usual (and under
  `ignore` the pin can be chosen again).
- Under `loaded_only`, a pinned member that is no longer loaded would be served
  and thereby trigger exactly the load the flag exists to avoid. So such a pin
  also counts as not available and falls through. This does mean a session loses
  its pin when its model is evicted — which is the honest outcome: the model is
  gone, and serving it means loading it. If nothing at all is loaded, the
  `loaded_only` fallback lets the walk load one, pin included.

`sticky` still never leaves an *available* pin; these two rules only change what
"available" means for a group that opted into a filter.

## Implementation outline

- **Store** (`internal/routing/store.go`, `internal/store/*`): five columns on
  `model_groups` via append-only **migration 62** (`61` is the latest shipped):
  `loaded_only` (bool/int, default 0), `member_order` (text, default
  `'priority'`), `climb_speed_margin_percent` (int, default 20),
  `min_tokens_per_second` (real, default 0), `min_speed_fallback` (text, default
  `'error'`). New fields on `routing.ModelGroup` with constants for the enum
  values. Unknown/garbage enum values fail open to the default (the codebase's
  established pattern for `Traversal`/`FailoverMode`).
- **GroupResolver seam** (`internal/routing/resolver.go`): `Group(name)` returns
  only `(members, mode, ok)` today. Replace `mode string` with a
  `GroupPolicy` struct carrying the failover mode plus the four new settings —
  five return values would be unreadable and the struct keeps the seam
  extensible. `*gateway.GroupRegistry` (and the resolver's test doubles) adapt.
- **Resolver** (`resolveGroup`): the filters and the ordering act on the flat
  member list `FlattenGroup` already produces, before the pin/walk logic.
- **Portal** (`internal/portal`): model-group create/update accept and validate
  the new settings (`admin` scope as today; reject an unknown enum value and a
  negative number with the existing `portal.*_invalid` error shape). The
  `groupLoaded` computation gains the `loaded_only` branch.
- **Frontend** (`ModelGroupSection.tsx`, `i18n.ts`): a checkbox, an order
  select, a number field, and a fallback select in the group editor; de/en labels
  added together (the type-checked build enforces parity).

## Testing

- **Resolver** (`internal/routing`): `loaded_only` picks a loaded lower-priority
  member over a cold top one; with nothing loaded it falls back and serves the
  ordinary walk; `speed` order picks the fastest, sorts unmeasured last, and
  breaks ties by manual priority; the combination picks the fastest **loaded**
  member; a speed-ordered `climb_up` stays on the pin below the margin and climbs
  above it, and only to an already-loaded target; `loaded_only` suppresses
  `Warm`; the floor excludes a slow **candidate** even when its member has a fast
  one; `error` vs `ignore` on an empty floor result; every default reproduces
  today's behavior exactly (the no-op invariant).
- **Store conformance**: the five columns round-trip on memory/sqlite/postgres.
- **Portal**: endpoint accepts/validates the settings; `groupLoaded` is
  at-least-one for `loaded_only` and unchanged (top member) otherwise.
- **Pin gating**: a pinned candidate that has fallen below the floor is
  abandoned; a pinned member that is no longer loaded is abandoned under
  `loaded_only`; and neither happens while the corresponding setting is off.
- **Frontend**: the controls render, round-trip their values, and the group
  editor keeps working with the defaults.

## Out of scope

Global (non-group) minimum speed; a per-model speed override; changing `Score()`
or the candidate scoring itself; changing prompt-throughput or MTP handling;
automatic re-benchmarking triggered by these settings.

## Documentation to update (same branch)

`docs/architecture/cross-cutting/routing-and-model-selection.md` (§5 model
groups, and §1's group data model), `docs/architecture/reference/data-model.md`
(the new columns + migration 62), `docs/architecture/reference/api-surface.md`
(if the group endpoints' description enumerates settings). The load-aware
consequence of the floor and the "two requests without a session can land
differently" property must be stated, not implied.
