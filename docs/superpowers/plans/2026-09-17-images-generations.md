# `POST /v1/images/generations` Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Serve `POST /v1/images/generations` against stable-diffusion.cpp's `sd-server`, and with it give the gateway its first candidate filter that actually **excludes** a model from routing.

**Architecture:** The gate keys on a required **capability**, not on a new API flavor: `inference.Request` gains `RequiredCapabilities []string`, and one filter — `filterCapable` — is applied at all four `Resolve` branches. The bulk capability read it needs already exists in every driver, so there is no store work. The endpoint itself is a native-passthrough relay with its own error normalisation; the launch shape reuses runtime kind `custom`.

**Tech Stack:** Go 1.26 (backend module `op-ai-gateway`), SQLite / PostgreSQL / in-memory store triple, React + TypeScript portal (untouched by this plan), Playwright.

## Global Constraints

- Design source of truth: `docs/superpowers/specs/2026-09-17-images-generations-design.md`. Read it before starting; it carries the reasoning this plan only executes.
- **Never commit to or merge into `main`.** All work is on branch `images-generations` in worktree `.worktrees/images-generations`. The merge is a human's, via pull request.
- Repo-facing text (commits, PR, issues, code comments, docs) is **English**.
- Commit message bodies must be substantive: this repo's GitHub squash-merge pre-fills the squash description from the commit body, not the PR body.
- **The chat path must stay bit-identical.** `filterCapable` early-returns on a nil required list; no SQL changes anywhere.
- **Gate on the capability, never on a new API flavor.** Do NOT introduce `openai_images` as a fine flavor: `applicationServesEndpoint`'s default branch folds any `openai*` flavor to the coarse `openai` (`internal/routing/store.go:2095`, `resolver.go:675`), so a fine flavor refuses nothing and costs a join, a scanner, a mirror and two conformance suites — repeated for #68 and #69.
- **Do NOT add a second `admitPrincipal` call site.** There is exactly one, reached from `inferencePreflight`, and its comment records what broke when there were four.
- **Do NOT put the gate in `affinityApplicationStale`.** Every stale verdict there leads to `DeleteAffinity`, and `AffinityKey.APIFlavor` is coarse, so an image request would delete the chat client's pin.
- **Do NOT add `image` to `reservedAgentCapabilityNames`** (`internal/gateway/agent_ingest.go:1038`). That list is for names no probe can observe; an `sd-server` probe will observe exactly this one, and listing it would block the follow-up writer.
- No new runtime kind, no per-application `images_mode`, no `route_affinity` migration, no widening of `captureMaxBytes`.
- Store changes must be verified with `OP_AI_GATEWAY_TEST_POSTGRES_DSN` set — the postgres subtests skip **silently** without it.
- Architecture docs are updated in this same branch (Task 9). `docs/superpowers/**` is removed before the PR.
- All commands run from the worktree root `/Users/jlor08/Developer/codex/op-ai-gateway/.worktrees/images-generations` unless a step says otherwise.

---

## File Structure

**Routing — the gate (Tasks 1–3)**
- `gateway/backend/internal/inference/types.go` — `Request.RequiredCapabilities`, the carrier.
- `gateway/backend/internal/routing/store.go` — `CapabilityImage`, `ErrModelNotCapable`.
- `gateway/backend/internal/routing/resolver.go` — `filterCapable`, its four application points, the affinity read gate, the two affinity write guards.
- `gateway/backend/internal/gateway/inference_complete.go` — the refusal's code, message and status, plus the folded-in `ErrNoModelRoute`/`ErrNoHealthyHost` status cases.

**Capability policy (Task 4)**
- `gateway/backend/internal/portal/service_applications.go` — `(image, no)` in `reservedManualVerdicts`.

**The endpoint (Tasks 5–7)**
- `gateway/backend/internal/gateway/images_handler.go` *(new)* — the handler, its own request shape and validation, and the error normalisation. A new file rather than more of `inference_handlers.go`: this endpoint shares the gate but none of the chat/translate machinery, and that file is already large.
- `gateway/backend/internal/gateway/server.go` — two route registrations.
- `gateway/backend/internal/gateway/session_extract.go` — the iota member, handled explicitly in both switches.
- `gateway/backend/internal/gateway/native_passthrough.go` — `endpointModeFor` and `upstreamPath` cases, and `proxyNative`'s endpoint selection.

**Launch shape (Task 8)**
- `docs/architecture/cross-cutting/agent-runtime-manager.md` — the `sd-server` launch example and the `HealthPath` obligation. No code: `RuntimeSpec` already expresses it.

**Docs (Task 9)**
- `docs/architecture/cross-cutting/routing-and-model-selection.md`, `agent-runtime-manager.md`, `compatibility-and-inference.md`, `telemetry-usage-observability.md`, `reference/api-surface.md`, `reference/openapi.yaml`, `09-architecture-decisions.md` (ADR-042).

---

## Task 1: The carrier, the capability, and the filter

**Files:**
- Modify: `gateway/backend/internal/inference/types.go` (the `Request` struct, after `APIFlavor`)
- Modify: `gateway/backend/internal/routing/store.go:1103-1109` (capability constants)
- Modify: `gateway/backend/internal/routing/resolver.go` — add `filterCapable` after `filterProvisioned` (ends `:656`), extend the `resolverStore` interface (`:311-329`), and apply at `:501`, `:601`, `:1230`
- Test: `gateway/backend/internal/routing/resolver_capability_gate_test.go` *(new)*

**Interfaces:**
- Consumes: nothing (first task).
- Produces: `inference.Request.RequiredCapabilities []string`; `routing.CapabilityImage = "image"`; `(*Resolver).filterCapable(ctx context.Context, cands []MappingCandidate, required []string) ([]MappingCandidate, error)`. Tasks 2, 3, 5 and 7 depend on these exact names.

- [ ] **Step 1: Write the failing test**

Create `gateway/backend/internal/routing/resolver_capability_gate_test.go`. Read an existing resolver test first (`resolver_test.go`) and copy its fixture helpers verbatim — the two resolver test fakes embed the `resolverStore` interface itself, so extending that interface does not break them.

```go
// capabilityGateStore counts the bulk capability read so the chat-path no-op can
// be asserted as "never reached the store" rather than merely "same result".
// The embed-the-interface idiom is this package's own (see
// telemetryCountingStore in group_min_speed_test.go).
type capabilityGateStore struct {
	resolverStore
	bulkCalls int
	failBulk  bool
}

func (s *capabilityGateStore) MappingCapabilitiesForMappings(ctx context.Context, ids []string) (map[string][]CapabilityRow, error) {
	s.bulkCalls++
	if s.failBulk {
		return nil, errors.New("capability store down")
	}
	return s.resolverStore.MappingCapabilitiesForMappings(ctx, ids)
}

// imageGateFixture seeds the package's standard two-member store and puts the
// given verdict on map_a. An empty verdict seeds NO row at all, which is the
// "unknown" case.
func imageGateFixture(t *testing.T, now time.Time, verdictA string) *capabilityGateStore {
	t.Helper()
	mem := seededGroupStore(t, now)
	if verdictA != "" {
		if err := mem.UpsertMappingCapabilities(context.Background(), "map_a", []CapabilityRow{
			{Capability: CapabilityImage, Verdict: verdictA, Source: CapabilitySourceManual, CheckedAt: now},
		}); err != nil {
			t.Fatalf("seed map_a capability: %v", err)
		}
	}
	if err := mem.UpsertMappingCapabilities(context.Background(), "map_b", []CapabilityRow{
		{Capability: CapabilityImage, Verdict: CapabilityYes, Source: CapabilitySourceManual, CheckedAt: now},
	}); err != nil {
		t.Fatalf("seed map_b capability: %v", err)
	}
	return &capabilityGateStore{resolverStore: mem}
}

func imageGateCandidates() []MappingCandidate {
	return []MappingCandidate{
		{Server: AIServer{ID: "srv_a"}, Application: Application{ID: "app_a"}, Mapping: ModelMapping{ID: "map_a"}},
		{Server: AIServer{ID: "srv_b"}, Application: Application{ID: "app_b"}, Mapping: ModelMapping{ID: "map_b"}},
	}
}

// A candidate whose mapping has no `image` row must be dropped: an absent row
// means UNKNOWN, and this gateway refuses on unknown (design spec §4, ADR-042).
// Extra-sourced rows are yes-only and no probe can emit "no", so a lenient gate
// would refuse nothing in a real fleet.
func TestFilterCapableDropsMappingWithoutVerdict(t *testing.T) {
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	store := imageGateFixture(t, now, "") // map_a: no row at all
	r := NewResolver(store, func() time.Time { return now }, nil)

	got, err := r.filterCapable(context.Background(), imageGateCandidates(), []string{CapabilityImage})
	if err != nil {
		t.Fatalf("filterCapable: %v", err)
	}
	if len(got) != 1 || got[0].Mapping.ID != "map_b" {
		t.Fatalf("candidates = %+v, want only map_b (map_a has no image verdict)", got)
	}
}

// An `image: no` row is dropped for the same reason an absent row is.
func TestFilterCapableDropsExplicitNo(t *testing.T) {
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	store := imageGateFixture(t, now, CapabilityNo)
	r := NewResolver(store, func() time.Time { return now }, nil)

	got, err := r.filterCapable(context.Background(), imageGateCandidates(), []string{CapabilityImage})
	if err != nil {
		t.Fatalf("filterCapable: %v", err)
	}
	if len(got) != 1 || got[0].Mapping.ID != "map_b" {
		t.Fatalf("candidates = %+v, want only map_b (map_a is image:no)", got)
	}
}

// The nil required list is the chat path. It must not even reach the store --
// asserted by the call count, not by the result, because "same result" would
// also hold for a gate that queried and then ignored the answer.
func TestFilterCapableIsANoOpForChat(t *testing.T) {
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	store := imageGateFixture(t, now, CapabilityYes)
	r := NewResolver(store, func() time.Time { return now }, nil)
	cands := imageGateCandidates()

	got, err := r.filterCapable(context.Background(), cands, nil)
	if err != nil {
		t.Fatalf("filterCapable: %v", err)
	}
	if len(got) != len(cands) {
		t.Fatalf("candidates = %d, want %d unchanged", len(got), len(cands))
	}
	if store.bulkCalls != 0 {
		t.Fatalf("bulk capability reads = %d, want 0 on the chat path", store.bulkCalls)
	}
}

// A store error refuses rather than failing open: routing an image request to a
// chat model is the defect this gate exists to prevent.
func TestFilterCapableRefusesOnStoreError(t *testing.T) {
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	store := imageGateFixture(t, now, CapabilityYes)
	store.failBulk = true
	r := NewResolver(store, func() time.Time { return now }, nil)

	if _, err := r.filterCapable(context.Background(), imageGateCandidates(), []string{CapabilityImage}); err == nil {
		t.Fatal("filterCapable must return an error when the capability read fails, not fail open")
	}
}
```

The file needs `context`, `errors`, `testing` and `time` imported. `seededGroupStore` lives in `resolver_group_test.go` and seeds `srv_a/app_a/map_a/coder-a` plus `srv_b/app_b/map_b/coder-b` into a real `*MemoryStore`, so these tests exercise the MemoryStore capability mirror rather than a hand-written fake.

- [ ] **Step 2: Run the test to verify it fails**

```bash
cd gateway/backend && go test ./internal/routing/ -run TestFilterCapable -v
```

Expected: FAIL to compile — `undefined: routing.CapabilityImage` and `r.filterCapable undefined`.

- [ ] **Step 3: Add the capability constant**

In `gateway/backend/internal/routing/store.go`, in the constant block at `:1103-1109`:

```go
	// CapabilityImage means the model GENERATES images. It is orthogonal to
	// CapabilityVision, which means the model ACCEPTS them, and the two can
	// co-occur. Never map one onto the other: advertising a generator as a
	// consumer fails far from its cause.
	//
	// The string is "image" rather than "image_generation" because that is what
	// already has live data -- cmd/gateway/app_health.go writes it verbatim as a
	// yes-row for every Extra name, migration 78 backfilled it, and the agent's
	// probe already pins it to generation. It also equals usage.BillingUnitImage,
	// so the capability and #70's billable unit speak one vocabulary.
	CapabilityImage = "image"
```

- [ ] **Step 4: Add the carrier**

In `gateway/backend/internal/inference/types.go`, in `Request`, immediately after `APIFlavor`:

```go
	// RequiredCapabilities are capability names a candidate mapping MUST carry a
	// "yes" verdict for, or it is not a routing candidate at all. Nil for chat,
	// responses and messages -- which is what keeps this a no-op on every
	// existing path -- and set by an endpoint handler from its own identity,
	// never from the request body.
	//
	// This is the axis the capability gate keys on, deliberately instead of a new
	// fine API flavor: NormalizeAPIFlavor folds any openai* flavor to the coarse
	// "openai", so a flavor cannot refuse anything, while this list needs no
	// store change at all (routing.Store already has
	// MappingCapabilitiesForMappings). See ADR-042.
	RequiredCapabilities []string `json:"-"`
```

- [ ] **Step 5: Extend the resolver's store interface**

In `gateway/backend/internal/routing/resolver.go`, add to the `resolverStore` interface beside `MappingCapabilities` (`:322`):

```go
	// MappingCapabilitiesForMappings is filterCapable's bulk read -- one query
	// for the whole candidate list rather than one per candidate. It is already
	// implemented on every driver (chunked in SQLite, mirrored in MemoryStore,
	// wrapped in the generated tracing decorator) and already carries an N+1
	// guard test in internal/portal, so adding it here costs nothing but this
	// line.
	MappingCapabilitiesForMappings(ctx context.Context, mappingIDs []string) (map[string][]CapabilityRow, error)
```

- [ ] **Step 6: Write `filterCapable`**

In `resolver.go`, immediately after `filterProvisioned` (which ends at `:656`), copying its contract and its `cands[:0:0]` idiom:

```go
// filterCapable drops every candidate whose mapping does not carry a "yes"
// verdict for each of the required capabilities. It is this gateway's FIRST
// filter that genuinely excludes a model for lacking a capability -- the scorer
// ranks, wantsLiveProgress annotates and the models-list fold advertises, but
// none of them refuses.
//
// An ABSENT row means unknown, not no, and this filter treats unknown as a
// refusal. That direction is chosen on evidence rather than taste: Extra-sourced
// rows are written yes-only (cmd/gateway/app_health.go) and the Ollama detector
// can structurally never emit "no", so treating unknown as permission would
// refuse nothing in a real fleet -- reproducing the exact defect the gate exists
// to fix. See ADR-042 for the cost this accepts on day one.
//
// A store error REFUSES rather than failing open, for the same reason.
func (r *Resolver) filterCapable(ctx context.Context, cands []MappingCandidate, required []string) ([]MappingCandidate, error) {
	if len(required) == 0 || len(cands) == 0 {
		return cands, nil
	}
	ids := make([]string, 0, len(cands))
	seen := map[string]bool{}
	for _, c := range cands {
		if !seen[c.Mapping.ID] {
			seen[c.Mapping.ID] = true
			ids = append(ids, c.Mapping.ID)
		}
	}
	caps, err := r.store.MappingCapabilitiesForMappings(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("capability gate: %w", err)
	}
	out := cands[:0:0]
	for _, c := range cands {
		if capabilityRowsSatisfy(caps[c.Mapping.ID], required) {
			out = append(out, c)
		}
	}
	return out, nil
}

// capabilityRowsSatisfy reports whether rows carry a "yes" verdict for every
// required capability. Shared by filterCapable and resolveAffinity's own gate,
// which reads its rows from a different place.
func capabilityRowsSatisfy(rows []CapabilityRow, required []string) bool {
	byName := CapabilityRowsByName(rows)
	for _, name := range required {
		if byName[name].Verdict != CapabilityYes {
			return false
		}
	}
	return true
}
```

- [ ] **Step 7: Apply the filter at the three candidate sites**

All three sit immediately after an existing `filterServesEndpoint` call. Add, at `resolver.go:501` (the default fresh-candidate path):

```go
	candidates, err = r.filterCapable(ctx, candidates, req.RequiredCapabilities)
	if err != nil {
		return Target{}, err
	}
```

at `:601` (inside `resolveServerOverride` — its empty case already returns `ErrServerOverrideModelUnavailable`, which already has a 404, so no new error plumbing here):

```go
	mine, err = r.filterCapable(ctx, mine, req.RequiredCapabilities)
	if err != nil {
		return Target{}, err
	}
```

and at `:1230` (inside `eligibleCandidates`). Match each site's existing error-return shape — read the surrounding lines; the three functions have different signatures and `eligibleCandidates` returns `(cands, live, err)`.

Gate **before** `live` is taken in `eligibleCandidates`, so an all-chat group is an unknown model rather than a gated one. That follows the existing precedent: endpoint non-service already runs before `live`.

- [ ] **Step 8: Run the tests**

```bash
cd gateway/backend && go test ./internal/routing/ -run TestFilterCapable -v
```

Expected: PASS (all four).

- [ ] **Step 9: Prove the chat path is bit-identical**

Add to the same test file:

```go
// The load-bearing constraint: chat routing must be unchanged. Asserted across
// all three capability states, because "the filter early-returns" is a claim
// about code while this is a claim about behaviour.
func TestChatRoutingUnchangedByCapabilityRows(t *testing.T) {
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	targets := map[string]Target{}
	for _, verdict := range []string{"", CapabilityYes, CapabilityNo} {
		store := imageGateFixture(t, now, verdict)
		r := NewResolver(store, func() time.Time { return now }, nil)
		// RequiredCapabilities deliberately nil: this is a chat request.
		target, err := r.Resolve(context.Background(), auth.Token{}, inference.Request{
			Model:     "coder-a",
			APIFlavor: "openai_chat_completions",
		})
		if err != nil {
			t.Fatalf("verdict %q: Resolve: %v", verdict, err)
		}
		targets[verdict] = target
	}
	if !reflect.DeepEqual(targets[""], targets[CapabilityYes]) || !reflect.DeepEqual(targets[""], targets[CapabilityNo]) {
		t.Fatalf("chat routing differs by capability row:\n absent=%+v\n yes=%+v\n no=%+v",
			targets[""], targets[CapabilityYes], targets[CapabilityNo])
	}
}
```

Add `op-ai-gateway/internal/auth`, `op-ai-gateway/internal/inference` and `reflect` to the imports. If `Target` turns out to contain a field that varies per resolve independently of routing (a timestamp, a request id), compare the routing-identifying fields explicitly instead of `reflect.DeepEqual` and say so in the report — do not weaken the test to `ServerID` alone.

```bash
cd gateway/backend && go test ./internal/routing/ -v 2>&1 | tail -20
```

Expected: PASS, and every pre-existing resolver test still passes unedited.

- [ ] **Step 10: Run the linters and commit**

```bash
cd gateway/backend && golangci-lint fmt --diff && golangci-lint run ./internal/routing/... ./internal/inference/...
```

```bash
git add gateway/backend/internal/inference/types.go gateway/backend/internal/routing/
```

```bash
git commit -m "feat(routing): Add the first candidate filter that excludes a model

filterCapable drops every candidate whose mapping lacks a yes verdict for
each required capability, applied at the three candidate-filter sites. This
is the gateway's first filter that genuinely REFUSES for a missing
capability: the scorer ranks, wantsLiveProgress annotates and the
models-list fold advertises, but none of them says no.

The gate keys on a required-capability list carried on inference.Request,
deliberately NOT on a new fine API flavor. applicationServesEndpoint's
default branch is applicationHasAPIFlavor(app, NormalizeAPIFlavor(flavor))
and NormalizeAPIFlavor folds any openai* flavor to the coarse openai, so a
fine flavor passes for every OpenAI application in the fleet and refuses
nothing -- it would buy only the trigger, and buy it for a second filtered
LEFT JOIN, a scan target, a candidate field, the MemoryStore mirror, a
verdict-conversion twin and assertions in two conformance suites, paid again
for the speech and multipart endpoints. The capability list buys the same
trigger for one additive field, because Resolve already takes the whole
request and has exactly one call site, and the bulk read already exists on
every driver under an N+1 guard test. No store change at all.

An absent capability row means unknown, and this filter refuses on unknown.
That direction is evidence-driven: Extra-sourced rows are written yes-only
and the Ollama detector can structurally never emit no, so treating unknown
as permission would refuse nothing in a real fleet. A store error refuses
too rather than failing open.

The chat path stays bit-identical -- the filter early-returns on a nil
required list -- and a test pins that across all three capability states
rather than asserting it from the code shape."
```

---

## Task 2: The affinity branch — read gate and write guards

**Files:**
- Modify: `gateway/backend/internal/routing/resolver.go` — `resolveAffinity` (`:700`, its `caps` local at `:768`), the `UpsertAffinity` at `:560`, and `upsertGroupPin`'s at `:1449`
- Test: `gateway/backend/internal/routing/resolver_capability_gate_test.go`

**Interfaces:**
- Consumes: `routing.CapabilityImage`, `capabilityRowsSatisfy`, `inference.Request.RequiredCapabilities` (Task 1).
- Produces: no new exported names. Task 3 depends on the refusal reaching `Resolve`'s caller as a fall-through, not an error.

**Why this is its own task:** the affinity path is the one branch with no candidate filter, and it needs two different things — a read gate with unusual semantics, and two write guards for a leak the read gate cannot fix.

- [ ] **Step 1: Write the failing tests**

```go
// affinityGateStore counts affinity writes so the write-side leak can be
// asserted directly, and can fail the keyed capability read on demand.
type affinityGateStore struct {
	resolverStore
	upserts     int
	failKeyed   bool
}

func (s *affinityGateStore) UpsertAffinity(ctx context.Context, a RouteAffinity) error {
	s.upserts++
	return s.resolverStore.UpsertAffinity(ctx, a)
}

func (s *affinityGateStore) MappingCapabilities(ctx context.Context, mappingID string) ([]CapabilityRow, error) {
	if s.failKeyed {
		return nil, errors.New("capability store down")
	}
	return s.resolverStore.MappingCapabilities(ctx, mappingID)
}

// imagesReq is an image request: the endpoint's identity, expressed as the
// required-capability list the handler sets.
func imagesReq(model string) inference.Request {
	return inference.Request{
		Model:                model,
		APIFlavor:            "openai_images",
		RequiredCapabilities: []string{CapabilityImage},
	}
}

// resolveAffinity builds a synthetic candidate from MappingsByApplication, which
// never joins the capability table, so it gates off its own keyed read. The
// refusal is NON-DESTRUCTIVE: it falls through to the fresh-candidate path and
// leaves the pin intact. Every other rejection in that function deletes the row,
// and that would be wrong here -- AffinityKey.APIFlavor is coarse, so deleting
// would destroy the pin of a chat client sharing that key.
func TestAffinityRefusesIncapableMappingWithoutDeletingThePin(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	mem := seededGroupStore(t, now)
	// map_a is pinned but has no image verdict; map_b is image-capable.
	if err := mem.UpsertMappingCapabilities(ctx, "map_b", []CapabilityRow{
		{Capability: CapabilityImage, Verdict: CapabilityYes, Source: CapabilitySourceManual, CheckedAt: now},
	}); err != nil {
		t.Fatalf("seed map_b: %v", err)
	}
	key := AffinityKey{APITokenID: "tok_1", Model: "coder-a", APIFlavor: APIFlavorOpenAI, SessionID: "sess_1"}
	if err := mem.UpsertAffinity(ctx, RouteAffinity{ /* fields per RouteAffinity: key parts + ApplicationID app_a + ServerID srv_a + timestamps */ }); err != nil {
		t.Fatalf("seed affinity: %v", err)
	}
	store := &affinityGateStore{resolverStore: mem}
	r := NewResolver(store, func() time.Time { return now }, nil)

	req := imagesReq("coder-a")
	req.ClientSessionID = "sess_1"
	target, err := r.Resolve(ctx, auth.Token{ID: "tok_1"}, req)

	// The pinned (incapable) application must not be served. Either a capable
	// candidate is chosen instead or the resolve refuses -- both are correct; what
	// is NOT correct is serving app_a.
	if err == nil && target.ServerID == "srv_a" {
		t.Fatal("served the pinned but image-incapable application")
	}
	// And the pin itself must survive: the refusal is non-destructive.
	if _, ok, affErr := mem.AffinityByKey(ctx, key); affErr != nil || !ok {
		t.Fatalf("the affinity row was deleted by an image request (ok=%v, err=%v)", ok, affErr)
	}
}

// A capability read error on this path refuses too: failing open here holds for
// the whole AffinityTTLSeconds, unlike live-progress's advisory "" degradation.
func TestAffinityRefusesOnCapabilityReadError(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	mem := seededGroupStore(t, now)
	if err := mem.UpsertMappingCapabilities(ctx, "map_a", []CapabilityRow{
		{Capability: CapabilityImage, Verdict: CapabilityYes, Source: CapabilitySourceManual, CheckedAt: now},
	}); err != nil {
		t.Fatalf("seed map_a: %v", err)
	}
	if err := mem.UpsertAffinity(ctx, RouteAffinity{ /* same shape as above, pinning app_a */ }); err != nil {
		t.Fatalf("seed affinity: %v", err)
	}
	store := &affinityGateStore{resolverStore: mem, failKeyed: true}
	r := NewResolver(store, func() time.Time { return now }, nil)

	req := imagesReq("coder-a")
	req.ClientSessionID = "sess_1"
	target, err := r.Resolve(ctx, auth.Token{ID: "tok_1"}, req)
	// map_a IS capable, so a fail-open would serve srv_a. Refusing means the pin
	// is not taken even though its verdict would have allowed it.
	if err == nil && target.ServerID == "srv_a" {
		t.Fatal("a failed capability read served the pin anyway (failed open)")
	}
}

// The leak a read-side gate cannot fix: the affinity key is coarse, so an image
// resolve writing a pin would repoint the chat client sharing that key at an
// image server.
func TestImageResolveWritesNoAffinityPin(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	mem := seededGroupStore(t, now)
	if err := mem.UpsertMappingCapabilities(ctx, "map_a", []CapabilityRow{
		{Capability: CapabilityImage, Verdict: CapabilityYes, Source: CapabilitySourceManual, CheckedAt: now},
	}); err != nil {
		t.Fatalf("seed map_a: %v", err)
	}
	store := &affinityGateStore{resolverStore: mem}
	r := NewResolver(store, func() time.Time { return now }, nil)

	req := imagesReq("coder-a")
	req.ClientSessionID = "sess_1"
	if _, err := r.Resolve(ctx, auth.Token{ID: "tok_1"}, req); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if store.upserts != 0 {
		t.Fatalf("affinity writes = %d, want 0: an image resolve must not pin under the coarse key", store.upserts)
	}
}

// Same leak, group path -- upsertGroupPin's own doc says it mirrors the main pin,
// so it needs the same guard.
func TestImageResolveWritesNoGroupPin(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	mem := seededGroupStore(t, now)
	for _, id := range []string{"map_a", "map_b"} {
		if err := mem.UpsertMappingCapabilities(ctx, id, []CapabilityRow{
			{Capability: CapabilityImage, Verdict: CapabilityYes, Source: CapabilitySourceManual, CheckedAt: now},
		}); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}
	store := &affinityGateStore{resolverStore: mem}
	r := NewResolver(store, func() time.Time { return now }, nil)
	r.SetGroupResolver(twoMemberGroup("sticky"))

	req := imagesReq("coder-group")
	req.ClientSessionID = "sess_1"
	if _, err := r.Resolve(ctx, auth.Token{ID: "tok_1"}, req); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if store.upserts != 0 {
		t.Fatalf("affinity writes = %d, want 0 on the group path too", store.upserts)
	}
}
```

Three things to resolve against the real code while filling these in, and to
report:

1. **`RouteAffinity`'s exact fields.** The two seeding calls above are marked with
   a comment rather than guessed: read the struct and set the key parts plus the
   pinned `ApplicationID`/`ServerID` and whatever timestamps it requires.
2. **The affinity read-back helper.** `AffinityByKey` is the expected name; use
   whatever `routing.Store` actually exposes for reading one affinity by key, so
   the non-destructive assertion reads the real row.
3. **Whether the group path needs a session id to pin at all.** If
   `upsertGroupPin` only fires under conditions this fixture does not meet, the
   test would pass vacuously. Verify it fires for a *chat* request with the same
   fixture first (a temporary check you then delete), and say in the report that
   you did — otherwise the guard is untested.

- [ ] **Step 2: Run to verify they fail**

```bash
cd gateway/backend && go test ./internal/routing/ -run 'TestAffinity|TestImageResolveWrites' -v
```

Expected: FAIL — the pinned target is returned, and pins are written.

- [ ] **Step 3: Gate the affinity read**

In `resolveAffinity`, the function already fetches capability rows for live progress at `:767-771`. Reuse that **same local** — this is the one place the capability axis is strictly cheaper than a join:

```go
	liveProgressSupport := ""
	caps, capErr := r.store.MappingCapabilities(ctx, mapping.ID)
	if capErr == nil {
		if row, ok := CapabilityRowsByName(caps)[CapabilityLiveProgress]; ok {
			liveProgressSupport = LiveProgressSupportFromVerdict(row.Verdict)
		}
	}
	// The capability gate on the pinned mapping. Two things differ from the
	// live-progress read directly above, both deliberate:
	//
	//  1. A read ERROR is not-satisfied here, where live progress degrades to ""
	//     (advisory). Live progress failing open costs one annotation; this
	//     failing open would route an image request to a chat model for the
	//     whole AffinityTTLSeconds.
	//  2. The refusal is NON-DESTRUCTIVE -- it returns (Target{}, false, nil) and
	//     lets the caller fall through to the fresh-candidate path. Every other
	//     rejection in this function deletes the affinity row, and that would be
	//     wrong here: AffinityKey.APIFlavor is COARSE, so an image request
	//     declaring the pin stale would delete the chat client's pin. For the
	//     same reason the gate is not in affinityApplicationStale.
	if len(required) > 0 && (capErr != nil || !capabilityRowsSatisfy(caps, required)) {
		return Target{}, false, nil
	}
```

`resolveAffinity` takes only `fineFlavor` today; thread `required []string` into its signature and pass `req.RequiredCapabilities` from the one call site at `:469`.

- [ ] **Step 4: Guard the two affinity writes**

At `resolver.go:560` (the main pin) and `:1449` (`upsertGroupPin`), guard on the capability list rather than on a flavor string, so the speech and multipart endpoints inherit the guard:

```go
	// A capability-carrying request never writes a pin. AffinityKey.APIFlavor is
	// COARSE (NormalizeAPIFlavor at :430), so an image resolve would write its
	// pin under the same key a chat client uses and repoint that client at an
	// image server. The read-side gate does not help -- it is the WRITE that
	// does the damage. Keyed on the capability list, not on a flavor, so #68 and
	// #69 inherit this.
	//
	// Guarding the affinity READ makes the third UpsertAffinity call site (the
	// in-place refresh inside resolveAffinity) unreachable for such a request,
	// so these two writes plus that gate are exhaustive.
	if len(req.RequiredCapabilities) == 0 {
		// ... the existing UpsertAffinity call, unchanged ...
	}
```

Read both call sites first: `:1449` is inside `upsertGroupPin`, which may not have `req` in scope — thread the list in if so, matching how it already receives the flavor.

- [ ] **Step 5: Run to verify they pass**

```bash
cd gateway/backend && go test ./internal/routing/ -v 2>&1 | tail -25
```

Expected: PASS, every pre-existing resolver and affinity test unedited.

- [ ] **Step 6: Commit**

```bash
git add gateway/backend/internal/routing/
```

```bash
git commit -m "feat(routing): Gate the affinity branch, and stop it writing a foreign pin

The affinity branch is the one Resolve path with no candidate filter: its
mapping comes from MappingsByApplication, which never joins the capability
table. It gates off the capability rows it ALREADY fetches for live progress,
so this costs no extra store call -- the one place the capability axis is
strictly cheaper than a join would be.

Two semantics differ from the live-progress read beside it, deliberately. A
read error is not-satisfied rather than an advisory empty string, because
failing open here holds for the whole AffinityTTLSeconds. And the refusal is
non-destructive: it falls through to the fresh-candidate path instead of
deleting the row, because AffinityKey.APIFlavor is COARSE and deleting would
destroy the pin of the chat client that shares that key. For the same reason
the gate is not in affinityApplicationStale, where every rejection deletes.

The write side is a separate leak the read gate cannot fix: an image resolve
would WRITE its pin under that same coarse key and repoint the chat client at
an image server. Both writes -- the main pin and the group pin -- are now
guarded on the required-capability list rather than on a flavor string, so
the speech and multipart endpoints inherit the guard. Guarding the read makes
the third UpsertAffinity site, the in-place refresh, unreachable for such a
request, so those three points are exhaustive."
```

---

## Task 3: Refusal, legibly

**Files:**
- Modify: `gateway/backend/internal/routing/store.go` (the error block near `ErrNoModelRoute`)
- Modify: `gateway/backend/internal/gateway/inference_complete.go` — `completionErrorResponse` (`:888`), `completionHTTPStatus` (`:921`), `completionErrorCode` (`:937`)
- Test: `gateway/backend/internal/gateway/inference_complete_test.go` (or the file that already tests these three — find it with `grep -rln completionHTTPStatus gateway/backend/internal/gateway/*_test.go`)

**Interfaces:**
- Consumes: Task 1's filter (the error it returns).
- Produces: `routing.ErrModelNotCapable`, error code `routing.model_not_capable`, HTTP 404.

- [ ] **Step 1: Write the failing test**

```go
// A capability refusal must not look like an unknown model, and a group-path
// refusal must not look like an upstream outage.
func TestCompletionErrorMappingForCapabilityRefusal(t *testing.T) {
	if got := completionErrorCode(routing.ErrModelNotCapable); got != "routing.model_not_capable" {
		t.Errorf("code = %q, want routing.model_not_capable", got)
	}
	if got := completionHTTPStatus(routing.ErrModelNotCapable); got != http.StatusNotFound {
		t.Errorf("status = %d, want 404", got)
	}
}

// Folded in: both of these fall through to 502 today, so a group-path
// capability refusal would be indistinguishable from an upstream outage.
func TestCompletionStatusForRoutingRefusals(t *testing.T) {
	if got := completionHTTPStatus(routing.ErrNoModelRoute); got != http.StatusNotFound {
		t.Errorf("ErrNoModelRoute status = %d, want 404", got)
	}
	if got := completionHTTPStatus(routing.ErrNoHealthyHost); got != http.StatusServiceUnavailable {
		t.Errorf("ErrNoHealthyHost status = %d, want 503", got)
	}
}
```

The two statuses for the folded-in cases are a judgement this plan makes explicitly: a model with no route at all is a 404 (the client asked for something that does not exist here), while a model whose hosts are all unhealthy is a 503 (it exists and is temporarily unserviceable). If the implementer disagrees after reading how clients treat these, raise it rather than silently choosing differently — coding agents depend on predictable failures, and AGENTS.md makes stable error codes a rule.

- [ ] **Step 2: Run to verify it fails**

```bash
cd gateway/backend && go test ./internal/gateway/ -run 'TestCompletionError|TestCompletionStatus' -v
```

Expected: FAIL to compile on `routing.ErrModelNotCapable`, then FAIL on the two 502s.

- [ ] **Step 3: Add the sentinel**

In `gateway/backend/internal/routing/store.go`, beside `ErrNoModelRoute`:

```go
	// ErrModelNotCapable reports that candidates existed for the requested model
	// but none carries a "yes" verdict for a capability the endpoint requires.
	// It is deliberately NOT ErrNoModelRoute: "this model cannot do that" and
	// "there is no such model" are different facts, and a client that cannot
	// tell them apart cannot act on either.
	ErrModelNotCapable = errors.New("routing.model_not_capable")
```

- [ ] **Step 4: Wire the three mapping functions**

`completionErrorCode` — a new case before the default:

```go
	case errors.Is(err, routing.ErrModelNotCapable):
		return routing.ErrModelNotCapable.Error()
```

`completionErrorResponse` — a new message:

```go
	if errors.Is(err, routing.ErrModelNotCapable) {
		message = "the requested model is not capable of this endpoint"
	}
```

`completionHTTPStatus` — three new cases, before the `StatusBadGateway` default:

```go
	// ErrModelNotCapable, ErrNoModelRoute and ErrNoHealthyHost had no cases here
	// and all three fell through to 502, which made a routing refusal
	// indistinguishable from an upstream outage. A capability gate that refuses
	// legibly on one branch and illegibly on another is not a legible gate, so
	// all three are mapped in the same change.
	if errors.Is(err, routing.ErrModelNotCapable) || errors.Is(err, routing.ErrNoModelRoute) {
		return http.StatusNotFound
	}
	if errors.Is(err, routing.ErrNoHealthyHost) {
		return http.StatusServiceUnavailable
	}
```

- [ ] **Step 5: Return the sentinel from the gate**

In `resolver.go`, the default path at `:501` and `eligibleCandidates` at `:1230` must distinguish "candidates existed but none was capable" from "no candidates at all". At the default path, after the filter:

```go
	if len(candidates) == 0 && len(req.RequiredCapabilities) > 0 {
		return Target{}, ErrModelNotCapable
	}
```

placed **before** the existing empty-candidates check so the more specific fact wins. Read that existing check first and keep its behaviour for the nil-required case byte-identical.

- [ ] **Step 6: Run the tests**

```bash
cd gateway/backend && go test ./internal/gateway/ ./internal/routing/ 2>&1 | tail -15
```

Expected: PASS. If a pre-existing test asserted 502 for `ErrNoModelRoute` or `ErrNoHealthyHost`, that test encodes the defect being fixed — update it and say so in the commit body rather than working around it.

- [ ] **Step 7: Commit**

```bash
git add gateway/backend/internal/routing/store.go gateway/backend/internal/gateway/
```

```bash
git commit -m "feat(gateway): Make a capability refusal legible, and fix two 502s

ErrModelNotCapable gets its own code and a 404. 'This model cannot do that'
and 'there is no such model' are different facts and a client that cannot
tell them apart cannot act on either -- which matters here because AGENTS.md
makes stable error codes a rule precisely so coding agents can depend on
predictable failures.

Folded in, because the gate would otherwise refuse legibly on one branch and
illegibly on another: ErrNoModelRoute and ErrNoHealthyHost had no cases in
completionHTTPStatus at all and both fell through to StatusBadGateway, so a
group-path capability refusal would have been a 502 indistinguishable from an
upstream outage. ErrNoModelRoute is now 404 (the client asked for something
that does not exist here) and ErrNoHealthyHost is 503 (it exists and is
temporarily unserviceable)."
```

---

## Task 4: `(image, no)` joins the reserved manual verdicts

**Files:**
- Modify: `gateway/backend/internal/portal/service_applications.go:276-282`
- Test: the file that already tests `reservedManualVerdict` — find it with `grep -rln reservedManualVerdict gateway/backend/internal/portal/*_test.go`

**Interfaces:**
- Consumes: `routing.CapabilityImage` (Task 1).
- Produces: nothing new.

- [ ] **Step 1: Write the failing test**

```go
// (image, no) is reserved once `image` is a routing veto. The argument is
// asymmetric: it costs the operator nothing, because an ABSENT row already
// refuses (ADR-042), so "this model cannot generate images" is fully expressed
// by not writing a yes. And it prevents a permanent veto: a manual row is rank 3,
// outranks every automated source and nothing re-derives it, so a `no` written
// before the sd-server capability writer lands would outrank it forever against
// a genuinely capable model.
func TestImageNoIsAReservedManualVerdict(t *testing.T) {
	if !reservedManualVerdict(routing.CapabilityImage, routing.CapabilityNo) {
		t.Error("(image, no) must be reserved")
	}
	// yes stays writable -- it is the operator's day-one enablement path.
	if reservedManualVerdict(routing.CapabilityImage, routing.CapabilityYes) {
		t.Error("(image, yes) must stay writable: it is the only enablement path until the writer lands")
	}
	// The reset is never refused, for any pair.
	if reservedManualVerdict(routing.CapabilityImage, "") {
		t.Error("the reset must never be refused")
	}
}
```

- [ ] **Step 2: Run to verify it fails**

```bash
cd gateway/backend && go test ./internal/portal/ -run TestImageNoIsAReserved -v
```

Expected: FAIL on the first assertion.

- [ ] **Step 3: Add the entry**

```go
var reservedManualVerdicts = map[string]map[string]bool{
	routing.CapabilityLiveProgress: {routing.CapabilityNo: true},
	routing.CapabilitySpeculationObserved: {
		routing.CapabilityYes: true,
		routing.CapabilityNo:  true,
	},
	// routing.CapabilityImage + CapabilityNo is reserved BEFORE its writer
	// exists, which is unusual for this list and is bought at zero cost. Under
	// ADR-042 an absent row already refuses, so an operator loses no
	// expressiveness: "cannot generate images" is saying nothing. What it
	// prevents is a permanent veto -- a manual row is rank 3, outranks every
	// automated source, and nothing re-derives it, so a `no` written before the
	// sd-server capability writer lands would outrank that writer forever
	// against a genuinely capable model. Reserving now is cheaper than a later
	// migration that has to find and clear such rows. (image, yes) stays
	// writable: it is the operator's only enablement path until the writer ships.
	routing.CapabilityImage: {routing.CapabilityNo: true},
}
```

Then extend that `var`'s own doc comment (`:178-190`) so the list's stated criterion covers this third entry — its current wording is about overriding "the only writer entitled to establish it", which for `image` is a *future* writer.

- [ ] **Step 4: Run and commit**

```bash
cd gateway/backend && go test ./internal/portal/ 2>&1 | tail -5 && golangci-lint run ./internal/portal/...
```

```bash
git add gateway/backend/internal/portal/
```

```bash
git commit -m "feat(portal): Reserve the (image, no) manual verdict

Once \`image\` is a routing veto, a manual (image, no) row would be a
permanent one: manual is rank 3, it outranks every automated source and
nothing re-derives it, so a \`no\` written before the sd-server capability
writer lands would outrank that writer forever against a genuinely capable
model.

Reserving it costs the operator nothing, and that asymmetry is the whole
argument: under ADR-042 an absent row already refuses, so 'this model cannot
generate images' is fully expressed by not writing a yes. (image, yes) stays
writable -- it is the only enablement path until the writer ships -- and the
reset is never refused, so there is a way back either way.

This is the first entry added to the list before its writer exists. The
list's doc comment is widened to say so rather than leaving the criterion
reading as though a present writer were being protected."
```

---

## Task 5: The endpoint — route, preflight, session, relay

**Files:**
- Create: `gateway/backend/internal/gateway/images_handler.go`
- Modify: `gateway/backend/internal/gateway/server.go:1165` (two registrations)
- Modify: `gateway/backend/internal/gateway/session_extract.go:26-30` and both switches (`:71-84`, `:91-125`)
- Modify: `gateway/backend/internal/gateway/native_passthrough.go` — `endpointModeFor` (`:60`), `upstreamPath` (`:109`), `proxyNative`'s endpoint selection (`:267`)
- Test: `gateway/backend/internal/gateway/images_handler_test.go` *(new)*

**Interfaces:**
- Consumes: `routing.CapabilityImage`, `inference.Request.RequiredCapabilities` (Task 1); `routing.ErrModelNotCapable` (Task 3).
- Produces: `(*Server).handleOpenAIImages(w http.ResponseWriter, r *http.Request)`; `endpointImages` in the `sessionEndpoint` iota.

**Why a new file:** this endpoint shares the gate but none of the chat/translate machinery, and `inference_handlers.go` is already large.

- [ ] **Step 1: Write the failing tests**

```go
// postImages sends an images request through the real mux, the way
// active_requests_test.go drives the server.
func postImages(t *testing.T, srv *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer dev-secret")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

// The endpoint runs the ONE existing gate. A model with no image verdict is
// refused with the capability code -- not served, and not mislabelled as an
// unknown model.
func TestImagesRefusesIncapableModel(t *testing.T) {
	srv := NewTestServer() // seedGatewayTestRoutes seeds qwen-coder with no image verdict

	rec := postImages(t, srv, `{"model":"qwen-coder","prompt":"a cat","n":1}`)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "routing.model_not_capable") {
		t.Fatalf("body = %s, want the capability code, not an unknown-model code", rec.Body.String())
	}
}

// An images request has no messages, so inference.Request.Validate would reject
// every one of them. The handler validates its own shape instead, and the code
// proves which validator ran.
func TestImagesRejectsMissingPrompt(t *testing.T) {
	srv := NewTestServer()

	rec := postImages(t, srv, `{"model":"qwen-coder","n":1}`)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "request.messages_required") {
		t.Fatal("the chat validator ran: an images request has no messages and must not be judged by it")
	}
}

// The routing model comes from our own mapping via the same tolerant JSON probe
// every native-passthrough endpoint uses. sd-server itself IGNORES the model
// field -- one process serves one model -- so a body with no model must not
// resolve to something arbitrary.
func TestImagesRejectsMissingModel(t *testing.T) {
	srv := NewTestServer()

	rec := postImages(t, srv, `{"prompt":"a cat"}`)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for a body with no model; body = %s", rec.Code, rec.Body.String())
	}
}

// The usage row carries the endpoint's own path. Without an upstreamPath case,
// its default branch labels every image request /v1/chat/completions on the row
// and in Activity.
func TestImagesUsageRowCarriesItsOwnPath(t *testing.T) {
	srv := NewTestServer()

	postImages(t, srv, `{"model":"qwen-coder","prompt":"a cat","n":1}`)

	events := srv.Usage.All()
	if len(events) == 0 {
		t.Fatal("the refusal recorded no usage event")
	}
	got := events[len(events)-1]
	if got.ReqPath != "/v1/images/generations" {
		t.Fatalf("ReqPath = %q, want /v1/images/generations", got.ReqPath)
	}
	if got.ProviderPath == "/v1/chat/completions" {
		t.Fatal("ProviderPath fell through to the chat default: upstreamPath needs its own case")
	}
}
```

The file needs `net/http`, `net/http/httptest`, `strings` and `testing`.
`NewTestServer()` (`server_test.go:4593`) seeds `seedGatewayTestRoutes` and a
token whose secret is `dev-secret`.

Two things to check while filling these in, and to report:

1. **Whether a refused request records a usage event at all.** A limiter denial
   writes no row, and a pre-`Resolve` rejection may not either. If
   `TestImagesUsageRowCarriesItsOwnPath` finds no event, the refusal path does not
   record one — then assert the path on a *successful* relay against a stub
   upstream instead, and say in the report which path you asserted. Do NOT delete
   the assertion: the mislabelling it guards against is real either way.
2. **Whether the seeded `qwen-coder` mapping is reachable for this flavor at
   all.** If the refusal turns out to be "no route" rather than "not capable",
   the gate is not being reached and the test would pass for the wrong reason.
   Assert the exact code, as above, rather than only the status.

- [ ] **Step 2: Run to verify they fail**

```bash
cd gateway/backend && go test ./internal/gateway/ -run TestImages -v
```

Expected: FAIL — 404 from the mux, because no route exists.

- [ ] **Step 3: Add the session endpoint member, handled in both switches**

In `session_extract.go`:

```go
const (
	endpointChat      sessionEndpoint = iota // /v1/chat/completions (generic OpenAI)
	endpointResponses                        // /v1/responses (Codex)
	endpointMessages                         // /v1/messages (Claude Code / Anthropic)
	endpointImages                           // /v1/images/generations (OpenAI images)
)
```

Both switches — the per-endpoint header switch and `sessionFromBody` — have **no default branch**, so adding the member alone would leave `session_id`/`session_source` empty on every images row and silently disable session affinity. Handle it explicitly in both, and the explicit handling is *"this endpoint has no session signal"*, written down rather than left to fall through:

```go
	case endpointImages:
		// /v1/images/generations carries no session signal: OpenAI's images
		// request has no prompt_cache_key, no user and no metadata field, and
		// image requests deliberately do not take an affinity pin at all (the
		// affinity key is coarse — see the write guards in resolver.go). Stated
		// as a case rather than left to the switch's absent default, so a reader
		// can tell "considered, and there is none" from "nobody thought about it".
```

Add that case to **both** switches.

- [ ] **Step 4: Add the path label and the endpoint mode**

`endpointModeFor` (`native_passthrough.go:60`) returns `("", "")` for an unrecognised flavor, which every caller treats as translate — wrong for images, which has no translate path. And `upstreamPath`'s default returns `/v1/chat/completions`, which would mislabel every images row.

In `upstreamPath`, before the provider fallbacks:

```go
	if apiFlavor == apiFlavorImages {
		return "/v1/images/generations"
	}
```

with the flavor constant declared beside the handler. Do **not** add an `endpointModeFor` case: there is no per-application images mode by design, so there is no mode to read — but add a comment there saying images is deliberately absent, because the function's doc currently implies any flavor without a case is a translate flavor.

In `proxyNative` (`:267`), the endpoint is currently inferred from the coarse flavor. Thread the `sessionEndpoint` in from the caller instead of extending that inference — read the function and pick the smaller change; if threading it means touching the two existing callers, do that rather than adding a third coarse-flavor branch.

- [ ] **Step 5: Write the handler**

Create `images_handler.go`. The shape follows `handleOpenAIResponses` (`inference_handlers.go:86`) minus the translate fallback:

```go
// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

// apiFlavorImages is the images endpoint's flavor string. It folds to the coarse
// "openai" through NormalizeAPIFlavor like every other openai* value, which is
// exactly why it cannot carry the capability gate — see ADR-042. It exists for
// labelling, not for filtering.
const apiFlavorImages = "openai_images"

// handleOpenAIImages serves POST /v1/images/generations by relaying to a
// natively OpenAI-shaped image backend. There is no translate path: the gateway
// proxies image requests and does not synthesize them, so an application that
// does not serve this shape is simply not a candidate.
//
// The capability gate is what makes that true, and it runs inside the ONE
// existing admission gate: the request declares
// RequiredCapabilities = [routing.CapabilityImage] and inferencePreflight ->
// Resolve refuses a model without a yes verdict. No second admitPrincipal call
// site is added here; there is exactly one in this package and its comment
// records what broke when there were four.
func (s *Server) handleOpenAIImages(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	token, ok := s.requireAnyScope(w, r, scopeGatewayUse, scopeLLMInvoke)
	if !ok {
		return
	}
	liftInferenceDeadlines(w)
	raw, ok := readRawJSONUnlimited(w, r)
	if !ok {
		return
	}
	// sd-server IGNORES the request's model field -- one process serves one
	// model -- so the model here is purely OUR routing input, read with the same
	// tolerant probe every native-passthrough endpoint uses.
	model, _ := sniffRoutingModel(raw)
	if err := validateImagesRequest(raw, model); err != nil {
		writeAPIError(w, http.StatusBadRequest, err)
		return
	}
	pf, handled := s.inferencePreflight(w, r, token, raw, inferenceShape{
		apiFlavor: apiFlavorImages,
		endpoint:  endpointImages,
		model:     model,
	})
	if handled {
		return
	}
	pf.Req.RequiredCapabilities = []string{routing.CapabilityImage}
	// ... resolve + proxyNative, following tryProxyNative's resolve-then-relay
	// shape but without the translate fallback ...
}
```

Two things the implementer must resolve against the real code, and report in the report file:

1. **Where `RequiredCapabilities` is set.** It must be on the `inference.Request` that reaches `Resolve`. If `inferencePreflight` builds that request internally (it does — it constructs `inference.Request` at its top), setting it on `pf.Req` afterwards is too late for a resolve that happens *inside* preflight, and too early is impossible. Read `inferencePreflight` and `inference_resolve.go:51`, and either add the field to `inferenceShape` so preflight sets it, or set it on `pf.Req` before the resolve if the resolve is genuinely after preflight. **Prefer adding it to `inferenceShape`** — that keeps endpoint identity as the single source, which is the rule the spec states. Whichever you choose, say why in the report.
2. **The error-response helper.** `writeAPIError` above is a placeholder name; use whatever this package actually has for a 400 with an `apierror.Body` (grep for `apierror.Response` in the gateway package and copy a neighbouring 400 path).

Also write `validateImagesRequest(raw []byte, model string) error`: `prompt` required and non-empty, `model` non-empty. Do **not** call `inference.Request.Validate()` — it requires `len(Messages) > 0` and an images request has none.

- [ ] **Step 6: Register the routes**

In `server.go:routes()`, beside the others:

```go
	s.mux.HandleFunc("/openai/v1/images/generations", s.handleOpenAIImages)
	s.mux.HandleFunc("/v1/images/generations", s.handleOpenAIImages)
```

- [ ] **Step 7: Run the tests**

```bash
cd gateway/backend && go test ./internal/gateway/ -run TestImages -v
```

Expected: PASS.

- [ ] **Step 8: Commit**

```bash
git add gateway/backend/internal/gateway/
```

```bash
git commit -m "feat(gateway): Serve POST /v1/images/generations

A native relay with no translate path: the gateway proxies image requests
rather than synthesizing them, so an application that does not serve the
shape is simply not a candidate. The capability gate is what makes that
true, and it runs inside the ONE existing admission gate -- the request
declares RequiredCapabilities and Resolve refuses a model without a yes
verdict. No second admitPrincipal call site.

sd-server ignores the request's model field (one process serves one model),
so the model read from the body is purely our routing input. The handler
validates its own shape instead of calling inference.Request.Validate, which
requires at least one message.

The sessionEndpoint member is handled explicitly in BOTH switches in
session_extract.go rather than added to the iota alone: neither switch has a
default branch, so a bare member would leave session_id and session_source
empty on every images row and silently disable session affinity. The
explicit case says 'this endpoint has no session signal', so a reader can
tell a considered absence from an oversight.

upstreamPath gets its own case; without one, its default branch labels every
image request /v1/chat/completions on the usage row and in Activity."
```

---

## Task 6: Error normalisation

**Files:**
- Modify: `gateway/backend/internal/gateway/images_handler.go`
- Test: `gateway/backend/internal/gateway/images_handler_test.go`

**Interfaces:**
- Consumes: the handler (Task 5).
- Produces: `normalizeImagesUpstreamError(status int, body []byte) (apierror.Body, bool)`.

- [ ] **Step 1: Write the failing test**

```go
// sd-server returns {"error": "<plain string>"}, not OpenAI's
// {"error":{message,type,code}}. The relay normalises it, and the mapping is
// pinned so a client contract change is a test failure rather than a surprise.
func TestNormalizeImagesUpstreamError(t *testing.T) {
	body, ok := normalizeImagesUpstreamError(500, []byte(`{"error":"out of memory"}`))
	if !ok {
		t.Fatal("a plain-string error body must normalise")
	}
	// The upstream string reaches the client verbatim as the message...
	if !strings.Contains(body.Error.Message, "out of memory") {
		t.Fatalf("message = %q, want the upstream string verbatim", body.Error.Message)
	}
	// ...and type/code are OURS. Pinned so a later change cannot quietly start
	// presenting a gateway-authored value as an upstream statement.
	if body.Error.Type == "" || body.Error.Code == "" {
		t.Fatalf("type/code = %q/%q, want gateway-authored values", body.Error.Type, body.Error.Code)
	}
}

// An upstream body that is ALREADY an OpenAI error object passes through
// untouched -- normalising it twice would restate our own type as the
// upstream's.
func TestNormalizeImagesLeavesOpenAIShapeAlone(t *testing.T) {
	raw := []byte(`{"error":{"message":"bad request","type":"invalid_request_error","code":"bad_prompt"}}`)
	body, ok := normalizeImagesUpstreamError(400, raw)
	if !ok {
		t.Fatal("an OpenAI-shaped body must still be accepted")
	}
	if body.Error.Type != "invalid_request_error" || body.Error.Code != "bad_prompt" {
		t.Fatalf("type/code = %q/%q, want the upstream's own values preserved", body.Error.Type, body.Error.Code)
	}
}

// A body that is neither shape must not be silently swallowed.
func TestNormalizeImagesUnrecognisedBody(t *testing.T) {
	if _, ok := normalizeImagesUpstreamError(500, []byte(`<html>502 Bad Gateway</html>`)); ok {
		t.Fatal("an unrecognised body must report ok=false so the caller relays the upstream bytes unchanged")
	}
}
```

- [ ] **Step 2: Run to verify it fails, then implement**

```bash
cd gateway/backend && go test ./internal/gateway/ -run TestNormalizeImages -v
```

`apierror.Body`'s exact field names decide these assertions — read
`internal/apierror` first and use the real ones (`body.Error.Message` above is
the expected shape; if the type nests differently, adjust the assertions, not
the requirement).

Then:

```go
// normalizeImagesUpstreamError converts sd-server's error shape into OpenAI's.
// sd-server returns {"error": "<plain string>"}; OpenAI clients expect
// {"error": {message, type, code}}.
//
// The `type` and `code` in the result are OURS, not the backend's: sd-server
// states neither, and nothing downstream may read them as an upstream
// statement. Recorded here because the alternative -- inventing a plausible
// upstream code -- would make a gateway-authored value indistinguishable from
// an attested one, which is the same class of error as reporting an unmeasured
// zero as a measurement.
//
// ok is false when the body is neither shape, and the caller then relays the
// upstream bytes unchanged rather than guessing.
func normalizeImagesUpstreamError(status int, body []byte) (apierror.Body, bool) {
```

Wire it into the relay's non-2xx path.

- [ ] **Step 3: Run and commit**

```bash
cd gateway/backend && go test ./internal/gateway/ 2>&1 | tail -8
```

```bash
git add gateway/backend/internal/gateway/
git commit -m "feat(gateway): Normalise sd-server's error shape into OpenAI's

sd-server returns {\"error\": \"<plain string>\"} where an OpenAI client
expects {\"error\": {message, type, code}}. The relay converts it.

The type and code in the converted object are OURS and the code says so.
sd-server states neither, so a plausible-looking upstream code would make a
gateway-authored value indistinguishable from an attested one -- the same
class of error as reporting an unmeasured zero as a measurement. A body that
is already an OpenAI error object passes through untouched, and a body that
is neither shape is relayed unchanged rather than guessed at."
```

---

## Task 7: Usage — the billable unit, sourced from the response

**Files:**
- Modify: `gateway/backend/internal/gateway/images_handler.go`
- Test: `gateway/backend/internal/gateway/images_handler_test.go`

**Interfaces:**
- Consumes: `usage.BillingUnitImage`, `usageMeta.BillingUnit`/`BillingQuantity` (both from #70, already on `main`).
- Produces: nothing new.

- [ ] **Step 1: Write the failing test**

```go
// The quantity comes from the RESPONSE, not the request. n states what was asked
// for; data[] states what was produced, and a partial failure makes those
// differ. Metering the ask would be the same class of error as counting a
// measured zero as a measurement.
func TestImagesUsageQuantityComesFromTheResponse(t *testing.T) {
	// Stub upstream: two images back for a request that asked for four.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"created":1,"output_format":"png","data":[{"b64_json":"AA=="},{"b64_json":"BB=="}]}`))
	}))
	defer upstream.Close()

	srv := newImageCapableTestServer(t, upstream.URL)
	postImages(t, srv, `{"model":"sd-turbo","prompt":"a cat","n":4}`)

	got := lastUsageEvent(t, srv)
	if got.BillingUnit != usage.BillingUnitImage {
		t.Fatalf("BillingUnit = %q, want %q", got.BillingUnit, usage.BillingUnitImage)
	}
	if got.BillingQuantity != 2 {
		t.Fatalf("BillingQuantity = %v, want 2 (what the response produced, not the n=4 that was asked for)", got.BillingQuantity)
	}
}

// The XOR: all seven token-denominated columns must be zero on a non-token row.
func TestImagesUsageRowSatisfiesTheBillingXOR(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"created":1,"output_format":"png","data":[{"b64_json":"AA=="}]}`))
	}))
	defer upstream.Close()

	srv := newImageCapableTestServer(t, upstream.URL)
	postImages(t, srv, `{"model":"sd-turbo","prompt":"a cat"}`)

	if err := usage.ValidateBillingXOR(lastUsageEvent(t, srv)); err != nil {
		t.Fatalf("the recorded image row violates the billing XOR: %v", err)
	}
}

// A failed image request is still a NON-TOKEN row: the unit is endpoint
// identity, never response-derived, so a 500 must not be recorded as
// token-metered with a zero measure -- the exact lie the pair exists to prevent.
func TestImagesUsageOnFailureIsStillImageUnit(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"out of memory"}`))
	}))
	defer upstream.Close()

	srv := newImageCapableTestServer(t, upstream.URL)
	postImages(t, srv, `{"model":"sd-turbo","prompt":"a cat"}`)

	got := lastUsageEvent(t, srv)
	if got.BillingUnit != usage.BillingUnitImage {
		t.Fatalf("BillingUnit = %q on a FAILED image request, want %q", got.BillingUnit, usage.BillingUnitImage)
	}
	if got.BillingQuantity != 0 {
		t.Fatalf("BillingQuantity = %v, want 0: nothing was produced", got.BillingQuantity)
	}
	if err := usage.ValidateBillingXOR(got); err != nil {
		t.Fatalf("the failed image row violates the billing XOR: %v", err)
	}
}
```

Two helpers these three need, both to be written in this task:

- `newImageCapableTestServer(t *testing.T, upstreamURL string) *Server` — a
  `NewTestServer()` whose route store additionally carries an application
  pointing at `upstreamURL` with a mapping `sd-turbo` **and** an
  `image: yes` capability row, so the gate admits it. Build it by seeding the
  server's `routing.MemoryStore` the way `seedGatewayTestRoutes` does; read that
  function and mirror its shape rather than inventing a second seeding style.
- `lastUsageEvent(t *testing.T, srv *Server) usage.Event` — the last element of
  `srv.Usage.All()`, failing the test when there is none.

Import `op-ai-gateway/internal/usage`.

- [ ] **Step 2: Run to verify it fails, then implement**

Set `usageMeta.BillingUnit = usage.BillingUnitImage` on **every** `recordUsage` call the images path makes — success and failure alike, because the unit is endpoint identity. Set `BillingQuantity` from the parsed response's `data` length on success and leave it 0 otherwise.

Counting `data[]` means parsing the relayed response. The relay streams and must not re-marshal, so count during the tee rather than buffering the whole body a second time — read `nativeCopier` and use the capture tee's existing bounded buffer if the count can be taken from it, and if it cannot, count by scanning the streamed bytes for the `b64_json` key rather than holding a multi-megabyte body in memory. State which you did and why in the report.

- [ ] **Step 3: Run and commit**

```bash
cd gateway/backend && OP_AI_GATEWAY_TEST_POSTGRES_DSN='postgres://postgres:test@127.0.0.1:55432/optest?sslmode=disable' go test ./internal/gateway/ ./internal/usage/ 2>&1 | tail -8
```

```bash
git add gateway/backend/internal/gateway/
git commit -m "feat(gateway): Meter an image request in images, not in tokens

The usage row carries usage.BillingUnitImage and a quantity, on #70's rails.
Every token-denominated column stays zero, so ValidateBillingXOR passes.

The quantity comes from the RESPONSE, not the request. n states what was
asked for; data[] states what was produced, and a partial failure makes those
differ -- metering the ask would be the same class of error as counting a
measured zero as a measurement.

The unit is set on every recordUsage call the path makes, success and failure
alike, because it is endpoint identity. A failed image request is still a
non-token request, and recording it as token-metered with a zero measure is
the exact lie the (unit, quantity) pair exists to prevent."
```

---

## Task 8: The `sd-server` launch shape

**Files:**
- Modify: `docs/architecture/cross-cutting/agent-runtime-manager.md`
- Test: none — no code changes. `RuntimeSpec` already expresses this launch.

- [ ] **Step 1: Confirm the health route before documenting one**

The reconnaissance behind this plan could **not** verify `sd-server`'s liveness route from upstream source; it relied on the issue's reading. Do not document a value you have not confirmed.

Determine `sd-server`'s actual 2xx-returning GET. If one exists, document it as the mandatory `HealthPath`. **If none exists**, stop and report: `pollHealth`'s single-GET model does not fit, and that is a design question rather than a documentation gap.

- [ ] **Step 2: Document the launch shape**

Add a worked `RuntimeSpec` example to `agent-runtime-manager.md`, carrying:

- `Type: custom` — and *why*: `DeriveProbePaths`' `custom` branch returns empty strings, which is correct because `sd-server` has neither a metrics nor a context axis. A dedicated `sd_cpp` kind would need a constant, a `DeriveProbePaths` case, a `DetectRuntimeSpecType` case, a `validRuntimeSpecType` case and a review of the live-timings and benchmark paths that assume an LLM-shaped provider — eight-plus files — for auto-detection from the binary name and a friendlier dropdown label.
- `HealthPath` set **explicitly**, because `pollHealth` defaults it to `/health`, which `sd-server` does not serve. A spec that omits it fails at startup.
- The weights path written **literally** in `Args`: the `MODEL` placeholder expands to the spec's upstream model *name*, and `sd-server`'s `-m` takes a filesystem *path*. The `PORT` placeholder works.
- A timeout well above `defaultApplicationTimeoutMS` (30 s), which will abort most real generations; `server_agent` applications already default to 600 s.
- A note that `sd-server` has **no authentication** and CORS is wide open, so it belongs behind the agent or a network boundary — the `APIToken` placeholder has nothing to bind to.
- That live progress is legitimately empty for this runtime: the library has a progress callback but the server never wires it.

- [ ] **Step 3: Run the docs gate and commit**

```bash
make lint-docs
```

```bash
git add docs/architecture/cross-cutting/agent-runtime-manager.md
git commit -m "docs: Document the sd-server launch shape under runtime kind custom

RuntimeSpec already expresses an sd-server launch with no schema change --
Binary plus an opaque Args array covers a multi-file model whose UNet, VAE
and text encoders are separate flags -- so this is documentation, not code.

Type custom rather than a new sd_cpp kind: DeriveProbePaths' custom branch
returns empty strings, which is correct because sd-server has neither a
metrics nor a context axis, while a dedicated kind would need cases in
runtime_spec_type.go, validRuntimeSpecType and a review of the live-timings
and benchmark paths that assume an LLM-shaped provider -- eight-plus files
for auto-detection from the binary name and a nicer dropdown label.

Three obligations an sd-server spec carries and the doc now states: HealthPath
must be set explicitly because pollHealth defaults it to /health, which
sd-server does not serve; the weights path is written literally in Args
because the MODEL placeholder expands to a model NAME while -m takes a PATH;
and the timeout must exceed the 30s application default, which would abort
most real generations."
```

---

## Task 9: Documentation and ADR-042

**Files:**
- Modify: `docs/architecture/cross-cutting/routing-and-model-selection.md`, `compatibility-and-inference.md`, `telemetry-usage-observability.md`, `reference/api-surface.md`, `reference/openapi.yaml`, `09-architecture-decisions.md`

- [ ] **Step 1: Routing**

Document the first **excluding** filter: what `filterCapable` refuses, that an absent row refuses, the four branches it is applied at, the affinity branch's two deviating semantics (read error refuses; refusal is non-destructive), the two affinity **write** guards and why the coarse key makes them necessary, and the recorded rejection of gating inside `affinityApplicationStale`.

- [ ] **Step 2: Compatibility**

The endpoint, that only natively OpenAI-shaped backends qualify and there is no translate path, the error normalisation with `type`/`code` being ours, the capture clipping for large base64 bodies, and the timeout obligation.

- [ ] **Step 3: Telemetry**

The `image` billable unit on #70's rails and that the quantity is **response-sourced**, with the reason.

- [ ] **Step 4: API surface and OpenAPI**

Add the endpoint. Check how the **compatibility** endpoints are described — #70 established that *portal* responses use an opaque `{type: object}`, but the compatibility endpoints may be documented with real schemas; match whatever is actually there.

- [ ] **Step 5: ADR-042**

The highest ADR is **041** (#70's), so this is **042**. Read ADR-041 for the template. Record:

1. The gate keys on a **required capability**, not on an API flavor, and why the flavor cannot refuse (`NormalizeAPIFlavor` folds it) — with the cost comparison, since the issue proposed the flavor.
2. An **absent row refuses**, the evidence (yes-only Extra rows; the Ollama detector cannot emit `no`), and the day-one cost: nothing carries an `image` row, so every image request 404s until an operator writes `{"capability_verdicts":{"image":"yes"}}` — which already works today with no schema or API change.
3. `(image, no)` is **reserved before its writer exists**, why that costs nothing, and that the reset is never refused.
4. **`/v1/models` keeps advertising what the router refuses.** `ModelOfferingFor` is called with the coarse flavor, so an images-only model is still offered to chat clients. Axis-independent, recorded as a known gap rather than fixed here, because fixing it means teaching the offering path a capability dimension.
5. The automatic capability writer is a **separate issue**, and `image` must **not** be added to `reservedAgentCapabilityNames` or that writer is blocked before it is written.

- [ ] **Step 6: Run the docs gate and commit**

```bash
make lint-docs
```

```bash
git add docs/architecture/
git commit -m "docs: Record the capability-gate decision as ADR-042

Documents the gateway's first excluding filter across routing, compatibility
and telemetry, and records five decisions.

The gate keys on a required capability rather than an API flavor, against
issue #71's own proposal, because a fine flavor cannot refuse anything --
NormalizeAPIFlavor folds any openai* value to the coarse openai -- and would
have cost a join, a scanner, a mirror and two conformance suites, repeated
for each further endpoint.

An absent capability row refuses. The evidence is in the code: Extra-sourced
rows are written yes-only and the Ollama detector can structurally never
emit no, so treating unknown as permission would refuse nothing in a real
fleet. The day-one cost is stated rather than left to be discovered -- no
mapping carries an image row, so every image request 404s until an operator
writes one, which already works today with no schema or API change.

(image, no) is reserved before its writer exists, at zero cost, because an
absent row already refuses.

Two gaps are recorded rather than fixed: /v1/models keeps advertising an
images-only model to chat clients, because ModelOfferingFor receives the
coarse flavor; and the automatic capability writer is a separate issue, with
the note that image must NOT join reservedAgentCapabilityNames or that
writer is blocked before it is written."
```

---

## Final verification (before the pull request)

- [ ] **Full gates, with the postgres leg provisioned**

```bash
docker run -d --name op-pg-test -e POSTGRES_PASSWORD=test -e POSTGRES_DB=optest -p 55432:5432 postgres:17
```

```bash
make test-go && make test && make lint
```

```bash
cd gateway/backend && OP_AI_GATEWAY_TEST_POSTGRES_DSN='postgres://postgres:test@127.0.0.1:55432/optest?sslmode=disable' go test -count=1 ./internal/... 2>&1 | grep -v '^ok' | head -20
```

Confirm the postgres subtests show `PASS`, not `SKIP`.

- [ ] **Sonar**

```bash
make sonar-gate && make sonar-findings && make sonar-branch-findings
```

`sonar-gate` is advisory; `sonar-branch-findings` is authoritative — judge by findings attributed to lines this branch changed.

- [ ] **e2e, with a baseline**

`make test-e2e` runs the **default** Playwright suite, which no CI job covers and which is **red on `main`** (tracked separately). Run it, and if it fails, run the same suite on the merge-base in a detached worktree and compare failure sets before attributing anything to this branch. A fresh worktree needs `npm install` in `gateway/frontend` first or the webServer dies with `tsc: command not found` and no test runs.

- [ ] **Working-file cleanup — the last step before the PR**

```bash
git rm -r docs/superpowers && git diff --name-only main...HEAD | grep -E 'docs/superpowers' && echo "STOP: still in the PR diff" || echo "clean"
```

- [ ] **Push and open the pull request. Do not merge it.**

## Notes for the implementer: what NOT to do

- Do **not** introduce `openai_images` as a fine API flavor for gating. It folds to the coarse `openai` and refuses nothing. It exists in this plan only as a label string.
- Do **not** add a second `admitPrincipal` call site.
- Do **not** gate inside `affinityApplicationStale` — it deletes the pin, and the key is coarse.
- Do **not** add `image` to `reservedAgentCapabilityNames`.
- Do **not** add a runtime kind, an `images_mode` application column, a `route_affinity` migration, or a wider `captureMaxBytes`.
- Do **not** call `inference.Request.Validate()` from the images handler.
- Do **not** re-marshal the relayed response body.
