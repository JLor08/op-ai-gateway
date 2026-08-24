# Model-Group Selection Settings Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give a `ModelGroup` four new combinable settings — serve only already-loaded members, order members by measured generation speed, require a minimum speed, and a margin before a speed-ordered `climb_up` leaves its pin.

**Architecture:** Both filters become **candidate-level filters inside `selectMember`**, which is what the pin check and the walk both call — so member availability, the walk's candidate choice, and pin gating all follow from one place. The speed order is a **re-sort of the `members` slice** before the existing pin/walk logic, because `firstAvailable` walks that slice and `memberIndex` reads position from it. Each filter's "nothing left" fallback is a retry of group resolution with that filter disabled. Spec: `docs/superpowers/specs/2026-08-24-model-group-selection-design.md`.

**Tech Stack:** Go 1.25 (backend, stdlib-only), React/TypeScript/Vite + Vitest (frontend), SQLite/Postgres/memory store drivers.

## Global Constraints

- SPDX header as the FIRST lines of every new file: `// SPDX-License-Identifier: AGPL-3.0-only` then `// Copyright (C) 2026 OnPrem AI Gateway contributors`.
- Backend is stdlib-only; no new dependencies. `golangci-lint run` (from `gateway/backend`) must report 0 issues.
- Migrations are append-only; **62** is the next free version (61 is the latest shipped). New columns must use the existing `addColumnIfMissing(ctx, tx, dl, table, colDef)` helper — do not paste the dialect block.
- All three store drivers (memory/sqlite/postgres) keep working. Postgres needs `OP_AI_GATEWAY_TEST_POSTGRES_DSN`; if unset, say so rather than claiming it was verified.
- **The no-op invariant is binding:** with every new setting at its default (`loaded_only=false`, `member_order="priority"`, `climb_speed_margin_percent=20`, `min_tokens_per_second=0`, `min_speed_fallback="error"`), resolution behavior and per-request store-read counts must be byte-identical to today. Unknown/garbage enum values fail open to the default, like the existing `Traversal`/`FailoverMode` handling.
- The speed metric is `effectiveGenTPS` (`internal/routing/scorer.go`) — the load-aware value, never the raw `GenTokensPerSecond`.
- Frontend i18n keys are added in German AND English together; the type-checked build enforces parity. `npm run lint` and `npm run format:check` must stay clean.
- `docs/architecture/` is updated in this same branch (Task 8).

---

### Task 1: store columns, migration 62, `ModelGroup` fields

**Files:**
- Modify: `gateway/backend/internal/routing/store.go` (`ModelGroup` struct ~line 398)
- Modify: `gateway/backend/internal/store/migrate.go` (migrations list; new `migration62Up`)
- Modify: the SQL store's `model_groups` insert/update/scan sites (find with `grep -rn "model_groups" gateway/backend/internal/store/ | grep -v _test`)
- Test: `gateway/backend/internal/store/conformance_test.go`

**Interfaces:**
- Consumes: nothing new.
- Produces: `routing.ModelGroup` fields `LoadedOnly bool`, `MemberOrder string`, `ClimbSpeedMarginPercent int`, `MinTokensPerSecond float64`, `MinSpeedFallback string`, plus exported constants. Every later task depends on these exact names.

- [ ] **Step 1: Write the failing test.** Extend the store conformance case that round-trips a model group (find it: `grep -n "ModelGroup" gateway/backend/internal/store/conformance_test.go`). Set all five fields to non-default values on the created group and assert them on read-back — plus one assertion that a group created without them reads back the documented defaults:

```go
	// Non-default values must round-trip through every driver.
	grp := routing.ModelGroup{ /* existing fields as the case already sets them */
		LoadedOnly: true, MemberOrder: routing.MemberOrderSpeed,
		ClimbSpeedMarginPercent: 35, MinTokensPerSecond: 12.5,
		MinSpeedFallback: routing.MinSpeedFallbackIgnore,
	}
	// ... create, read back as got ...
	if !got.LoadedOnly || got.MemberOrder != routing.MemberOrderSpeed ||
		got.ClimbSpeedMarginPercent != 35 || got.MinTokensPerSecond != 12.5 ||
		got.MinSpeedFallback != routing.MinSpeedFallbackIgnore {
		t.Fatalf("group settings did not round-trip: %+v", got)
	}
```

- [ ] **Step 2: Run it and watch it fail**

Run: `cd gateway/backend && go test ./internal/store -run Conformance 2>&1 | head -20`
Expected: compile failure — `unknown field LoadedOnly in struct literal`.

- [ ] **Step 3: Implement.**

`internal/routing/store.go`, on `ModelGroup` (keep the existing field comments' style):

```go
	// LoadedOnly restricts selection to members with an already-loaded candidate,
	// so serving a request does not trigger a model load. When nothing is loaded
	// the restriction is dropped for that request (never a dead end).
	LoadedOnly bool
	// MemberOrder is how the group's members are ordered for the walk:
	// MemberOrderPriority (the manual order) or MemberOrderSpeed (fastest
	// effective generation speed first). Unknown values fail open to priority.
	MemberOrder string
	// ClimbSpeedMarginPercent is how much faster a member must be before a
	// SPEED-ordered climb_up leaves an available pin. Priority-ordered groups
	// ignore it (no fluctuating measurement is involved).
	ClimbSpeedMarginPercent int
	// MinTokensPerSecond is the minimum effective generation speed a candidate
	// must reach to count as available; 0 disables the floor. An unmeasured
	// candidate (0) never satisfies a floor.
	MinTokensPerSecond float64
	// MinSpeedFallback is what happens when no candidate reaches the floor:
	// MinSpeedFallbackError (503) or MinSpeedFallbackIgnore (retry without it).
	MinSpeedFallback string
```

and the constants next to the existing group constants:

```go
const (
	MemberOrderPriority = "priority"
	MemberOrderSpeed    = "speed"

	MinSpeedFallbackError  = "error"
	MinSpeedFallbackIgnore = "ignore"

	// DefaultClimbSpeedMarginPercent is the shipped default margin.
	DefaultClimbSpeedMarginPercent = 20
)
```

`internal/store/migrate.go` — append to the migrations list and add the function, copying `migration61Up`'s shape:

```go
	{version: 62, name: "model_group_selection_settings", up: migration62Up},
```

```go
// migration62Up adds the model-group selection settings: serve only loaded
// members, order by measured speed, a minimum-speed floor with its fallback,
// and the climb margin. Defaults reproduce the pre-feature behavior exactly.
func migration62Up(ctx context.Context, tx *sql.Tx, dl dialect) error {
	cols := []string{
		"loaded_only integer not null default 0",
		"member_order text not null default 'priority'",
		"climb_speed_margin_percent integer not null default 20",
		"min_tokens_per_second real not null default 0",
		"min_speed_fallback text not null default 'error'",
	}
	for _, col := range cols {
		if err := addColumnIfMissing(ctx, tx, dl, "model_groups", col); err != nil {
			return err
		}
	}
	return nil
}
```

(Verify `migration61Up`'s real signature and `addColumnIfMissing`'s real parameter order before writing — copy them, do not trust this snippet over the code. If the sqlite driver stores bools as integers elsewhere in this table, follow that.)

Then extend the SQL store's `model_groups` INSERT column list + placeholders + args, its UPDATE set-list, and **every** SELECT column list with its matching `rows.Scan(&...)` order. Column order must agree across all of those — re-read each list/scan pair side by side before committing; a mismatch silently shifts values.

- [ ] **Step 4: Run tests**

Run: `cd gateway/backend && go test ./internal/store ./internal/routing` and, if a DSN is available, the store package again with `OP_AI_GATEWAY_TEST_POSTGRES_DSN` set (confirm from `-v` that the postgres subtest RAN, not skipped).
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add gateway/backend/internal/routing/store.go gateway/backend/internal/store
git commit -m "feat: model-group selection settings columns (migration 62)"
```

---

### Task 2: the `GroupPolicy` seam

**Files:**
- Modify: `gateway/backend/internal/routing/resolver.go` (`GroupResolver` interface ~line 128; the `Group(...)` call site in `Resolve`; `resolveGroup`'s `mode string` parameter ~line 1025)
- Modify: `gateway/backend/internal/gateway/group_registry.go` (the `Group` method and whatever entry struct it holds)
- Modify: test doubles implementing `GroupResolver` (find with `grep -rn "Group(name string)" gateway/backend --include="*_test.go"`)

**Interfaces:**
- Consumes: Task 1's `ModelGroup` fields.
- Produces:

```go
// GroupPolicy is a group's selection policy: how members are ordered, which are
// eligible, and how failover behaves. Passed as one value so the seam stays
// readable and extensible.
type GroupPolicy struct {
	FailoverMode            string
	MemberOrder             string
	LoadedOnly              bool
	ClimbSpeedMarginPercent int
	MinTokensPerSecond      float64
	MinSpeedFallback        string
}
```
and `GroupResolver.Group(name string) (members []GroupMember, policy GroupPolicy, ok bool)`.

- [ ] **Step 1: Write the failing test.** In `internal/gateway`, extend the group-registry test that asserts what `Group()` returns (find it: `grep -rn "func Test.*GroupRegistry" gateway/backend/internal/gateway/*_test.go`) so it seeds a group with non-default settings and asserts the returned `GroupPolicy` carries all six values, and that an unknown `MemberOrder`/`MinSpeedFallback` in the store fails open to `priority`/`error`.

- [ ] **Step 2: Run it and watch it fail**

Run: `cd gateway/backend && go test ./internal/gateway -run GroupRegistry 2>&1 | head -20`
Expected: compile failure — `Group` returns `(members, string, bool)`, not a policy.

- [ ] **Step 3: Implement.** Add the struct + the interface change, thread it through `GroupRegistry` (its snapshot entry gains the fields; `RefreshGroups` fills them from the store row, normalising unknown enums to the defaults), and change `resolveGroup`'s signature from `mode string` to `policy GroupPolicy`. Inside `resolveGroup`, replace the two `mode == modeClimbUp` reads with `policy.FailoverMode == modeClimbUp`. **No behavior change in this task** — it is a pure widening of the seam.

- [ ] **Step 4: Run tests**

Run: `cd gateway/backend && go test ./internal/routing ./internal/gateway` then `golangci-lint run`.
Expected: PASS, 0 issues. Any behavior difference here is a bug in this task.

- [ ] **Step 5: Commit**

```bash
git add gateway/backend/internal/routing gateway/backend/internal/gateway
git commit -m "refactor: carry the model-group selection policy through the resolver seam"
```

---

### Task 3: the minimum-speed floor

**Files:**
- Modify: `gateway/backend/internal/routing/resolver.go` (`selectMember` ~line 897, `resolveGroup` ~line 1025)
- Test: `gateway/backend/internal/routing/` — a new `group_min_speed_test.go`

**Interfaces:**
- Consumes: `GroupPolicy` (Task 2).
- Produces: a `Resolver.candidateEffectiveGenTPS(ctx, c, model) (float64, error)` helper reused by Task 5, and a floor parameter threaded into `selectMember`.

- [ ] **Step 1: Write the failing tests** in a new `group_min_speed_test.go` (SPDX header first). Follow the existing resolver tests' fixture style (`grep -n "func Test.*Group" gateway/backend/internal/routing/*_test.go` for the closest neighbours). Four cases:
  1. floor 20: a member whose only candidate measures 10 tok/s is not served; the member measuring 50 is.
  2. **candidate-level**: one member with a 50 tok/s and a 10 tok/s candidate, floor 20, the 50 candidate made unavailable (at capacity) → the member must NOT be served on the 10 (this is the hole the spec calls out).
  3. floor unreachable + `MinSpeedFallbackError` → `ErrNoHealthyHost`.
  4. floor unreachable + `MinSpeedFallbackIgnore` → the slow member is served.
  Plus: an unmeasured (0 tok/s) candidate never satisfies a floor, and with `MinTokensPerSecond == 0` the selection is identical to a run without the setting (no-op invariant).

- [ ] **Step 2: Run them and watch them fail**

Run: `cd gateway/backend && go test ./internal/routing -run MinSpeed -v 2>&1 | tail -20`
Expected: failures showing the slow member/candidate being served.

- [ ] **Step 3: Implement.**

Add the helper next to `argmaxByScore` (which is where the telemetry+load assembly already lives — mirror it exactly so the two agree):

```go
// candidateEffectiveGenTPS is a candidate's load-aware effective generation speed,
// assembled exactly as argmaxByScore assembles its scoring route so the floor and
// the ordering agree with the scorer. An unmeasured mapping yields 0.
func (r *Resolver) candidateEffectiveGenTPS(ctx context.Context, c MappingCandidate, model string) (float64, error) {
	telemetry, ok, err := r.store.TelemetryByServer(ctx, c.Server.ID)
	if err != nil {
		return 0, fmt.Errorf("load server telemetry: %w", err)
	}
	k := telemetry.ActiveRequests
	if r.activity != nil {
		inflight, _ := r.activity.ServerActivity(c.Server.ID)
		k = inflight
	}
	return effectiveGenTPS(scoringRoute(c, telemetry, ok, k, model)), nil
}
```

Thread the floor into `selectMember` (new parameter, e.g. `minTPS float64`), applied immediately after `filterProvisioned` and before the `len(cands) == 0` check, so an emptied set becomes `memberNoMapping` exactly like a member with no mappings:

```go
	if minTPS > 0 {
		kept := make([]MappingCandidate, 0, len(cands))
		for _, c := range cands {
			tps, tErr := r.candidateEffectiveGenTPS(ctx, c, name)
			if tErr != nil {
				return MappingCandidate{}, memberUnavailable, nil, 0, tErr
			}
			if tps >= minTPS {
				kept = append(kept, c)
			}
		}
		cands = kept
	}
```

Update every `selectMember` call site (the pin check in `resolveGroup`, `firstAvailable`, and any other) to pass `policy.MinTokensPerSecond` — or `0` for the non-group path, which must stay untouched.

Now make the fallback explicit. Rename the existing body to `resolveGroupOnce(ctx, token, req, key, apiFlavor, members, policy, now)` — an unchanged copy — and make `resolveGroup` a thin driver over an ordered list of attempts. Task 4 adds its attempt to the SAME list, which is what keeps the spec's precedence (drop the loaded filter first, keep the floor) readable and testable:

```go
// resolveGroup runs resolveGroupOnce under progressively relaxed eligibility
// filters. The order encodes the spec's precedence: the loaded-only filter is
// dropped before the speed floor is (Task 4 appends that first relaxation), and
// the floor is dropped only when min_speed_fallback says so. The first attempt
// that resolves wins; the LAST attempt's error is the one returned, so the
// caller still sees ErrNoModelRoute vs ErrNoHealthyHost from a real walk.
func (r *Resolver) resolveGroup(ctx context.Context, token auth.Token, req inference.Request, key AffinityKey, apiFlavor string, members []GroupMember, policy GroupPolicy, now time.Time) (Target, error) {
	attempts := []GroupPolicy{policy}
	// (Task 4 inserts the loaded-filter relaxation here.)
	if policy.MinTokensPerSecond > 0 && policy.MinSpeedFallback == MinSpeedFallbackIgnore {
		relaxed := policy
		relaxed.MinTokensPerSecond = 0
		attempts = append(attempts, relaxed)
	}
	var lastErr error
	for _, attempt := range attempts {
		target, err := r.resolveGroupOnce(ctx, token, req, key, apiFlavor, members, attempt, now)
		if err == nil {
			return target, nil
		}
		if !errors.Is(err, ErrNoHealthyHost) && !errors.Is(err, ErrNoModelRoute) {
			return Target{}, err // a real failure (store error, admission timeout) — never retried
		}
		lastErr = err
	}
	return Target{}, lastErr
}
```

Only "nothing eligible" outcomes are retried — a store error or an admission-queue timeout must propagate immediately, never silently trigger a relaxed retry.

Note for the implementer: the floor now also gates the pin, because the pin check goes through `selectMember` — that is intended (spec: "the filters must gate the pin"), and case 2 above is what proves the candidate-level behavior.

- [ ] **Step 4: Run tests**

Run: `cd gateway/backend && go test ./internal/routing -run MinSpeed -v` then the whole package `go test ./internal/routing`, then `golangci-lint run`.
Expected: PASS, 0 issues.

- [ ] **Step 5: Commit**

```bash
git add gateway/backend/internal/routing
git commit -m "feat: minimum-speed floor for model groups"
```

---

### Task 4: `loaded_only`

**Files:**
- Modify: `gateway/backend/internal/routing/resolver.go` (`selectMember`, `resolveGroup`'s climb branch ~line 1055)
- Test: `gateway/backend/internal/routing/group_loaded_only_test.go` (new)

**Interfaces:**
- Consumes: `GroupPolicy` (Task 2), the floor plumbing (Task 3) — the two filters compose in `selectMember`.
- Produces: no new exported names.

- [ ] **Step 1: Write the failing tests** (new file, SPDX header first):
  1. top member cold, a lower-priority member loaded → the loaded one is served.
  2. nothing loaded → the fallback runs the ordinary walk and the top member is served (a load is allowed).
  3. `climb_up` + `loaded_only`: `ModelWarmer.Warm` is NEVER called (use a recording warmer double; assert zero calls).
  4. a pinned member that is no longer loaded is abandoned in favour of a loaded one (pin gating).
  5. no-op invariant: with `LoadedOnly=false` the outcome is identical to a run without the setting, and `Warm` behaves as today.

- [ ] **Step 2: Run them and watch them fail**

Run: `cd gateway/backend && go test ./internal/routing -run LoadedOnly -v 2>&1 | tail -20`
Expected: the cold top member is served; the warmer is called.

- [ ] **Step 3: Implement.** In `selectMember`, after the floor filter, add the loaded filter (same shape, using the existing `modelLoadedOn`):

```go
	if loadedOnly && r.loaded != nil {
		kept := make([]MappingCandidate, 0, len(cands))
		for _, c := range cands {
			if modelLoadedOn(r.loaded, c) {
				kept = append(kept, c)
			}
		}
		cands = kept
	}
```

Fallback: insert this attempt into the list Task 3 built, at the marked spot — FIRST, so dropping the loaded filter keeps the floor applied (the spec's precedence):

```go
	attempts := []GroupPolicy{policy}
	if policy.LoadedOnly {
		relaxed := policy
		relaxed.LoadedOnly = false
		attempts = append(attempts, relaxed) // keeps MinTokensPerSecond as-is
	}
	if policy.MinTokensPerSecond > 0 && policy.MinSpeedFallback == MinSpeedFallbackIgnore {
		relaxed := policy
		relaxed.MinTokensPerSecond = 0
		attempts = append(attempts, relaxed)
	}
```

Note the resulting behavior for a group with BOTH settings and `min_speed_fallback=error`: attempt 1 (floor + loaded), attempt 2 (floor only), then the error — the floor is never dropped, which is what `error` asks for. Add a test for exactly that combination.

In the climb branch, suppress warming for such a group:

```go
					if r.warmer != nil && !policy.LoadedOnly {
						r.warmer.Warm(ctx, best)
					}
```

- [ ] **Step 4: Run tests**

Run: `cd gateway/backend && go test ./internal/routing -run 'LoadedOnly|MinSpeed' -v` then the whole package, then `golangci-lint run`.
Expected: PASS, 0 issues.

- [ ] **Step 5: Commit**

```bash
git add gateway/backend/internal/routing
git commit -m "feat: loaded-only member selection for model groups"
```

---

### Task 5: speed ordering + climb margin

**Files:**
- Modify: `gateway/backend/internal/routing/resolver.go` (`resolveGroup`: order the member slice before the pin check; the climb comparison ~line 1055)
- Test: `gateway/backend/internal/routing/group_speed_order_test.go` (new)

**Interfaces:**
- Consumes: `candidateEffectiveGenTPS` (Task 3), `GroupPolicy` (Task 2).
- Produces: no new exported names.

- [ ] **Step 1: Write the failing tests** (new file, SPDX header first):
  1. `member_order=speed`: the fastest member is served even though it is last in the manual order.
  2. a member with an unmeasured (0) candidate sorts last.
  3. two members of equal speed keep their manual relative order (stable tie-break).
  4. `climb_up` + speed, candidate only 10% faster with a 20% margin → the pin is kept; 50% faster → it climbs (and only when the target is already loaded — the existing free-climb rule).
  5. combination with `loaded_only`: the fastest **loaded** member is served, not the faster cold one.
  6. no-op invariant: with `member_order=priority` the served member and the store-read count are unchanged.

- [ ] **Step 2: Run them and watch them fail**

Run: `cd gateway/backend && go test ./internal/routing -run 'SpeedOrder' -v 2>&1 | tail -20`
Expected: the manual-order member is served instead of the fastest.

- [ ] **Step 3: Implement.** In `resolveGroup`, before the pin check, reorder when asked:

```go
	// Speed order: re-sort the member slice, because everything downstream reads
	// order from it (firstAvailable walks it; memberIndex reads position). A
	// member's speed is its fastest currently-eligible candidate's effective
	// speed; unmeasured (0) sorts last and ties keep the manual order, so the
	// order is total and a group with no measurements behaves exactly as before.
	if policy.MemberOrder == MemberOrderSpeed {
		var err error
		members, err = r.orderMembersBySpeed(ctx, token, members, apiFlavor, req, now, policy)
		if err != nil {
			return Target{}, err
		}
	}
```

```go
// orderMembersBySpeed returns members re-sorted by their fastest ELIGIBLE
// candidate's effective generation speed, descending. Eligibility uses the same
// filters selectMember applies, so the order can never rank a member the walk
// would refuse. Unmeasured members score 0 and therefore sort last; SliceStable
// keeps the manual order among equals, so ties (and an all-unmeasured group)
// behave exactly as before this feature.
//
// Cost: this reads EVERY member's mappings, where a priority-ordered walk stops
// at the first available member. That is the price of the ordering and is paid
// only by groups that opt into it.
func (r *Resolver) orderMembersBySpeed(ctx context.Context, token auth.Token, members []GroupMember, apiFlavor string, req inference.Request, now time.Time, policy GroupPolicy) ([]GroupMember, error) {
	speed := make(map[string]float64, len(members))
	for _, m := range members {
		best := 0.0
		cands, err := r.eligibleCandidates(ctx, token, m.MemberGatewayName, apiFlavor, policy)
		if err != nil {
			return nil, err
		}
		for _, c := range cands {
			tps, tErr := r.candidateEffectiveGenTPS(ctx, c, m.MemberGatewayName)
			if tErr != nil {
				return nil, tErr
			}
			if tps > best {
				best = tps
			}
		}
		speed[m.MemberGatewayName] = best
	}
	out := append([]GroupMember(nil), members...) // never reorder the caller's slice
	sort.SliceStable(out, func(i, j int) bool {
		return speed[out[i].MemberGatewayName] > speed[out[j].MemberGatewayName]
	})
	return out, nil
}
```

`eligibleCandidates` is the candidate-fetch-and-filter part of `selectMember` (store read → `filterProvisioned` → floor → loaded filter), extracted in this task so `selectMember` and the ordering share ONE definition of eligibility instead of two that can drift. Extract it, have `selectMember` call it, and confirm `./internal/routing` stays green — that extraction is behavior-preserving.

For the margin, extend the climb comparison. Today it is `memberIndex(members, best) < memberIndex(members, pinned)`; for a speed-ordered group also require the margin:

```go
				if best != "" && best != pinned && memberIndex(members, best) < memberIndex(members, pinned) {
					if policy.MemberOrder == MemberOrderSpeed {
						met, mErr := r.speedMarginMet(ctx, bestSel, pinSel, best, pinned, policy)
						if mErr != nil {
							return Target{}, mErr
						}
						if !met {
							return serve(pinned, pinSel) // not materially faster: keep the session where it is
						}
					}
					// ... existing free-climb / warm logic unchanged ...
```

```go
// speedMarginMet reports whether the candidate is enough faster than the pinned
// one to justify moving a session: strictly more than the margin above it. An
// unmeasured pin (<= 0) always yields true — anything measurable beats unknown.
func (r *Resolver) speedMarginMet(ctx context.Context, best, pinned MappingCandidate, bestModel, pinnedModel string, policy GroupPolicy) (bool, error) {
	pinnedTPS, err := r.candidateEffectiveGenTPS(ctx, pinned, pinnedModel)
	if err != nil {
		return false, err
	}
	if pinnedTPS <= 0 {
		return true, nil
	}
	bestTPS, err := r.candidateEffectiveGenTPS(ctx, best, bestModel)
	if err != nil {
		return false, err
	}
	margin := policy.ClimbSpeedMarginPercent
	if margin < 0 {
		margin = 0
	}
	return bestTPS > pinnedTPS*(1+float64(margin)/100), nil
}
```

Document the cost honestly in a comment on `orderMembersBySpeed`: a speed-ordered group reads every member's mappings each request, where a priority-ordered group stops at the first available member. That is the price of the ordering and applies only to groups that opt in.

- [ ] **Step 4: Run tests**

Run: `cd gateway/backend && go test ./internal/routing -v -run 'SpeedOrder|LoadedOnly|MinSpeed'` then the whole package plus `./internal/gateway`, then `golangci-lint run`.
Expected: PASS, 0 issues.

- [ ] **Step 5: Commit**

```bash
git add gateway/backend/internal/routing
git commit -m "feat: speed-ordered model groups with a climb margin"
```

---

### Task 6: portal — validation and the group loaded-state rule

**Files:**
- Modify: `gateway/backend/internal/portal/` — the model-group create/update service + DTO (find with `grep -rn "ModelGroup" gateway/backend/internal/portal/*.go | grep -v _test | head`)
- Modify: `gateway/backend/internal/portal/service.go` (the `groupLoaded` map, ~line 1818-1828)
- Test: the portal package's existing model-group tests + the models-listing tests

**Interfaces:**
- Consumes: Task 1's fields.
- Produces: JSON fields `loaded_only`, `member_order`, `climb_speed_margin_percent`, `min_tokens_per_second`, `min_speed_fallback` on the group DTO and its create/update requests — Task 7 consumes these exact names.

- [ ] **Step 1: Write the failing tests.**
  1. create/update round-trips all five fields; an unknown `member_order` or `min_speed_fallback`, a negative `min_tokens_per_second`, or a negative margin is rejected with the existing `portal.*_invalid` error shape (copy the shape from a neighbouring group validation test).
  2. `groupLoaded`: for a `loaded_only` group, the group reports loaded when **any** offerable member is loaded (and `LoadedOn` is the union of those members' servers); for a normal group the existing top-member rule is unchanged. Assert both in one test file so the difference is visible.

- [ ] **Step 2: Run them and watch them fail**

Run: `cd gateway/backend && go test ./internal/portal -run 'ModelGroup|Models' 2>&1 | tail -20`
Expected: unknown-field/compile failures, and the loaded-state assertion failing for the `loaded_only` group.

- [ ] **Step 3: Implement.** Add the fields to the DTO and the create/update request structs, validate them (normalise unknown enums to defaults on READ per the fail-open rule, but REJECT them on write so an operator learns about a typo), and persist through the existing store call. Then extend `groupLoaded`:

```go
				// A group is "loaded" iff its highest-priority OFFERABLE member is
				// loaded -- except for a loaded_only group, where ANY loaded member
				// is what will actually be served, so any of them makes it loaded
				// (LoadedOn is then the union of those members' servers).
				groupLoaded := make(map[string]map[string]struct{}, len(entries))
				for _, e := range entries {
					if len(e.OrderedOfferableMembers) == 0 {
						continue
					}
					if e.LoadedOnly {
						union := make(map[string]struct{})
						for _, member := range e.OrderedOfferableMembers {
							for srv := range loadedOn[member] {
								union[srv] = struct{}{}
							}
						}
						if len(union) > 0 {
							groupLoaded[e.Name] = union
						}
						continue
					}
					if servers := loadedOn[e.OrderedOfferableMembers[0]]; len(servers) > 0 {
						groupLoaded[e.Name] = servers
					}
				}
```

This needs `LoadedOnly` on the overlay entry type that `modelGroupOverlay` produces — carry it through from the group row there, next to the existing per-entry fields. Remember `RefreshGroups` must run after a group write (it already does) so the resolver sees the new settings on the next request.

- [ ] **Step 4: Run tests**

Run: `cd gateway/backend && go test ./internal/portal ./internal/gateway` then `golangci-lint run`.
Expected: PASS, 0 issues.

- [ ] **Step 5: Commit**

```bash
git add gateway/backend/internal/portal
git commit -m "feat: model-group selection settings in the portal API"
```

---

### Task 7: frontend controls

**Files:**
- Modify: `gateway/frontend/src/api/` (the model-group type + create/update payloads; find with `grep -rn "model-groups\|ModelGroup" gateway/frontend/src/api/*.ts`)
- Modify: `gateway/frontend/src/components/ModelGroupSection.tsx`
- Modify: `gateway/frontend/src/i18n.ts` (both language blocks)
- Test: `gateway/frontend/src/components/ModelGroupSection.test.tsx`

**Interfaces:**
- Consumes: Task 6's JSON field names.
- Produces: no names other tasks depend on.

- [ ] **Step 1: Write the failing tests.** Extend `ModelGroupSection.test.tsx` in its existing idiom: the editor renders a "loaded only" checkbox, an order select with both options, a minimum-speed number field and a fallback select; editing them and saving sends the five fields in the update payload; and a group loaded from the API shows its stored values. Assert on rendered controls and the payload, not on internals.

- [ ] **Step 2: Run them and watch them fail**

Run: `cd gateway/frontend && npx vitest run src/components/ModelGroupSection.test.tsx 2>&1 | tail -10`
Expected: the controls are not found.

- [ ] **Step 3: Implement.** Add the controls following the section's existing form idiom (look at how `failover_mode`/`traversal` are rendered and wired — mirror them). i18n keys, German and English together:

```ts
  groupLoadedOnly: 'Nur geladene Modelle',            // en: 'Loaded models only'
  groupMemberOrder: 'Reihenfolge',                    // en: 'Order'
  groupMemberOrderPriority: 'Priorität',              // en: 'Priority'
  groupMemberOrderSpeed: 'Geschwindigkeit',           // en: 'Speed'
  groupClimbSpeedMargin: 'Wechsel-Schwelle (%)',      // en: 'Switch threshold (%)'
  groupMinTokensPerSecond: 'Mindestgeschwindigkeit (Tokens/s)', // en: 'Minimum speed (tokens/s)'
  groupMinSpeedFallback: 'Wenn nichts erreicht wird', // en: 'When nothing qualifies'
  groupMinSpeedFallbackError: 'Fehler melden',        // en: 'Report an error'
  groupMinSpeedFallbackIgnore: 'Ohne Mindestgeschwindigkeit fortfahren', // en: 'Continue without the minimum'
```

(Use the file's real key-naming convention — check the neighbouring group keys first and follow them; the names above are the intent, not necessarily the exact prefixes.)

- [ ] **Step 4: Run tests + build**

Run: `cd gateway/frontend && npx vitest run src/components/ModelGroupSection.test.tsx && npm test && npm run build && npm run lint && npm run format:check`
Expected: targeted tests PASS, full suite PASS, type-checked build green (proves i18n parity), lint/format clean.

- [ ] **Step 5: Commit**

```bash
git add gateway/frontend/src
git commit -m "feat: model-group selection controls in the portal"
```

---

### Task 8: docs + full verification

**Files:**
- Modify: `docs/architecture/cross-cutting/routing-and-model-selection.md` (§1 group data model, §5 model groups)
- Modify: `docs/architecture/reference/data-model.md` (the five columns + migration 62)
- Modify: `docs/architecture/reference/api-surface.md` (only if the group endpoints' description enumerates settings)

- [ ] **Step 1: Update the documents** in their existing idiom. §5 gains the four settings, the evaluation order (pin → floor → loaded → order → walk), each filter's fallback, and — stated, not implied — these three consequences: the floor is load-aware so a busy member can drop below it; a session loses its pin when its model is evicted under `loaded_only` or falls below the floor; a speed-ordered group reads every member's mappings per request while a priority-ordered group stops at the first available member. Also record that the group loaded-state rule differs for `loaded_only` groups.

- [ ] **Step 2: Full verification**

Run from the worktree root: `make test-go`, `make lint`, then in `gateway/frontend`: `npm test` and `npm run build`. If a Postgres DSN is available, run `./internal/store` with it and confirm the postgres subtest ran.
Expected: all green. Report actual results; a failure is a finding, not something to paper over.

- [ ] **Step 3: Commit**

```bash
git add docs/architecture
git commit -m "docs: model-group selection settings"
```

---

### Task 9 (finish): pre-PR cleanup and push

- [ ] Remove the branch-local working files per AGENTS.md: `git rm -r docs/superpowers`, commit as `chore: remove branch-local working files before PR`.
- [ ] Verify: `git diff --name-only origin/main...HEAD | grep -E 'docs/superpowers|implementation-status'` returns nothing.
- [ ] Push and hand the user the PR URL (title: `feat: model-group selection settings — loaded-only, speed order, minimum speed`), with a description covering the four settings, the evaluation order, the three stated consequences, and the verification results.
