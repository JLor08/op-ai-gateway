// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package routing

import (
	"context"
	"errors"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/inference"
	"reflect"
	"testing"
	"time"
)

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

// The default single-model path's sentinel has exactly one producer
// (Resolve's `if len(candidates) == 0 && len(req.RequiredCapabilities) > 0`
// check, immediately after filterCapable). Nothing else in the suite pins
// it: deleting that check leaves every other test in this package green
// (confirmed by deliberately removing the check and re-running the suite)
// while silently turning a capability refusal back into an indistinguishable
// "unknown model". This test exists so that regression is caught.
func TestResolveReturnsModelNotCapableForIncapableSoleCandidate(t *testing.T) {
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	store := seededGroupStore(t, now) // coder-a (map_a) has no image row at all
	r := NewResolver(store, func() time.Time { return now }, nil)

	_, err := r.Resolve(context.Background(), auth.Token{}, imagesReq("coder-a"))
	if !errors.Is(err, ErrModelNotCapable) {
		t.Fatalf("err = %v, want ErrModelNotCapable", err)
	}
	if errors.Is(err, ErrNoModelRoute) {
		t.Fatalf("err = %v also matches ErrNoModelRoute -- the two facts must stay distinguishable", err)
	}
}

// The group path's capability gate must stay a per-member SOFT filter (drop
// the member, keep walking) and never a hard error that aborts
// firstAvailable before it reaches a later, capable member. This is the
// exact regression brief Step 5 would have caused if applied literally to
// eligibleCandidates (return ErrModelNotCapable there instead of the
// existing `nil, false, nil`): coder-a (priority 0) has no image row at
// all, coder-b (priority 1) is image-capable, and this test requires the
// walk to reach coder-b rather than failing the whole group resolve on
// coder-a's refusal. Confirmed by deliberately reverting eligibleCandidates
// to return ErrModelNotCapable for an incapable member: this test fails
// under that exact mutation.
func TestGroupCapabilityGateSkipsIncapableFirstMemberForCapableSecond(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	mem := seededGroupStore(t, now) // coder-a (prio 0, map_a), coder-b (prio 1, map_b)
	if err := mem.UpsertMappingCapabilities(ctx, "map_b", []CapabilityRow{
		{Capability: CapabilityImage, Verdict: CapabilityYes, Source: CapabilitySourceManual, CheckedAt: now},
	}); err != nil {
		t.Fatalf("seed map_b: %v", err)
	}
	// map_a deliberately gets NO image row: the first-priority member is
	// image-incapable.
	r := NewResolver(mem, func() time.Time { return now }, nil)
	r.SetGroupResolver(twoMemberGroup("sticky"))

	target, err := r.Resolve(ctx, auth.Token{}, imagesReq("coder-group"))
	if err != nil {
		t.Fatalf("Resolve: %v, want the capable second member served, not a group-wide failure", err)
	}
	if target.ServerID != "srv_b" || target.Model != "coder-b" {
		t.Fatalf("target = {%q,%q}, want {srv_b, coder-b} (coder-a is image-incapable, coder-b is the only capable member)", target.ServerID, target.Model)
	}
}

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

// --- The affinity branch: read gate + write guards --------------------------
//
// resolveAffinity is the one Resolve branch with no candidate filter at all: its
// mapping comes from MappingsByApplication (via activeMappingForApplication),
// which never joins model_mapping_capabilities. filterCapable never sees it. The
// tests below cover its own gate, plus the write-side leak the read gate cannot
// fix: AffinityKey.APIFlavor is COARSE (NormalizeAPIFlavor in Resolve), so an
// image request and a chat request from the same token/model/session collide on
// the same affinity row.

// affinityGateStore counts affinity writes so the write-side leak can be
// asserted directly, and can fail the keyed capability read on demand.
type affinityGateStore struct {
	resolverStore
	upserts   int
	failKeyed bool
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

// seedPinnedAffinity writes a RouteAffinity pinning key to (appID, serverID),
// unexpired at `now`. Mirrors what a prior successful Resolve would have
// written -- the exact shape resolveAffinity reads back.
func seedPinnedAffinity(t *testing.T, mem *MemoryStore, key AffinityKey, appID, serverID string, now time.Time) {
	t.Helper()
	if err := mem.UpsertAffinity(context.Background(), RouteAffinity{
		ID:            affinityID(key),
		APITokenID:    key.APITokenID,
		Model:         key.Model,
		APIFlavor:     key.APIFlavor,
		SessionID:     key.SessionID,
		ApplicationID: appID,
		ServerID:      serverID,
		ExpiresAt:     now.Add(1800 * time.Second),
		LastUsedAt:    now,
		CreatedAt:     now,
		UpdatedAt:     now,
	}); err != nil {
		t.Fatalf("seed affinity: %v", err)
	}
}

// resolveAffinity builds a synthetic candidate from MappingsByApplication, which
// never joins the capability table, so it gates off its own keyed read. The
// refusal is NON-DESTRUCTIVE: it falls through to the fresh-candidate path and
// leaves the pin intact. Every other rejection in that function deletes the row,
// and that would be wrong here -- AffinityKey.APIFlavor is coarse, so under a key
// a capability-carrying request shares with a text endpoint, deleting would
// destroy that endpoint's working pin.
//
// The pin is seeded under the images request's OWN key (coarse flavor
// openai_images), because that is the only key Resolve looks up for it: a pin
// under the chat key would never be found, and the test would pass without the
// gate running at all. The resolver writes no such pin itself (see
// TestImageResolveWritesNoAffinityPin), so the seed stands in for one that
// exists anyway -- the gate is defence in depth for images, and the shared-key
// case it was built for is a future capability-carrying endpoint's.
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
	key := AffinityKey{APITokenID: "tok_1", Model: "coder-a", APIFlavor: APIFlavorOpenAIImages, SessionID: "sess_1"}
	seedPinnedAffinity(t, mem, key, "app_a", "srv_a", now)
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
	if _, ok, affErr := mem.Affinity(ctx, key); affErr != nil || !ok {
		t.Fatalf("the affinity row was deleted by an image request (ok=%v, err=%v)", ok, affErr)
	}
}

// A capability read error on this path refuses too: failing open here holds for
// the whole AffinityTTLSeconds, unlike live-progress's advisory "" degradation.
//
// This calls resolveAffinity directly rather than going through Resolve: map_a's
// OWN stored verdict is "yes" (deliberately, so a fail-open bug that ignores
// capErr and trusts a zero-value `caps` would still serve it), and it is the
// ONLY mapping for gateway model "coder-a". A store error is injected on the
// KEYED read only (MappingCapabilities), not the BULK read
// (MappingCapabilitiesForMappings) filterCapable uses -- so if the test instead
// asserted on the full Resolve() outcome, a correct implementation refusing the
// STALE PIN would still see Resolve() fall through and legitimately re-serve
// app_a via the fresh-candidate path (whose independent, unbroken bulk read
// correctly finds it capable) -- an outcome indistinguishable, at the Resolve()
// boundary, from the fail-open bug this test exists to catch. Calling
// resolveAffinity directly tests the one thing that differs: whether THIS
// function's own gate trusts an errored read.
func TestAffinityRefusesOnCapabilityReadError(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	mem := seededGroupStore(t, now)
	if err := mem.UpsertMappingCapabilities(ctx, "map_a", []CapabilityRow{
		{Capability: CapabilityImage, Verdict: CapabilityYes, Source: CapabilitySourceManual, CheckedAt: now},
	}); err != nil {
		t.Fatalf("seed map_a: %v", err)
	}
	key := AffinityKey{APITokenID: "tok_1", Model: "coder-a", APIFlavor: APIFlavorOpenAI, SessionID: "sess_1"}
	seedPinnedAffinity(t, mem, key, "app_a", "srv_a", now)
	store := &affinityGateStore{resolverStore: mem, failKeyed: true}
	r := NewResolver(store, func() time.Time { return now }, nil)

	// Derived from imagesReq rather than hardcoded, so a future change to the
	// endpoint's flavor or capability list cannot silently leave this direct
	// resolveAffinity call on stale values while the other three tests move on.
	req := imagesReq("coder-a")
	target, ok, err := r.resolveAffinity(ctx, key, req.APIFlavor, req.RequiredCapabilities, now)
	// map_a IS capable (per its own stored row), so trusting the errored read
	// would serve srv_a directly off the pin. Refusing means ok=false even
	// though the (unreachable) verdict would have allowed it -- and the refusal
	// is the shared non-destructive shape, not a propagated error.
	if err != nil {
		t.Fatalf("resolveAffinity returned an error, want a non-destructive refusal: %v", err)
	}
	if ok {
		t.Fatalf("a failed capability read served the pin anyway (failed open): target=%+v", target)
	}
	if _, rowOK, affErr := mem.Affinity(ctx, key); affErr != nil || !rowOK {
		t.Fatalf("the affinity row was deleted on a read error (ok=%v, err=%v)", rowOK, affErr)
	}
}

// The leak a read-side gate cannot fix, for a request whose coarse flavor a text
// endpoint shares: a pin it wrote would repoint that endpoint's client. An image
// key is its own (openai_images) and cannot collide with a chat client's, so for
// images the capability guard is what keeps them pin-free -- and it is keyed on
// the capability list, so a future endpoint that DOES share a key inherits it.
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
		t.Fatalf("affinity writes = %d, want 0: a capability-carrying resolve must not pin", store.upserts)
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

// --- What the sentinel must NOT claim -------------------------------------
//
// ErrModelNotCapable asserts that candidates EXISTED and none was capable. The
// check that produces it therefore measures the pool immediately before
// filterCapable runs (`capableFrom`), because filterCapable early-returns on
// empty input: without that measurement every upstream reason the pool is
// already empty arrives at the check indistinguishable from a capability
// refusal, and the client is told "this model is not capable of this endpoint"
// about a model that does not exist, or one that carries `image: yes`. The two
// tests below are the two reachable shapes of that, and both must read
// ErrNoModelRoute.
//
// Mutation evidence (see final-fix-report.md): dropping the `capableFrom > 0`
// guard from resolver.go's check fails both of them and nothing else in the
// package.

// A model nothing in the store offers must stay "no such model" for an images
// request, exactly as it is for a chat request -- it is the one case where the
// capability sentinel would be a statement about a model that does not exist.
func TestResolveUnknownModelIsNoModelRouteForAnImagesRequest(t *testing.T) {
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	store := seededGroupStore(t, now)
	r := NewResolver(store, func() time.Time { return now }, nil)

	_, err := r.Resolve(context.Background(), auth.Token{}, imagesReq("this-model-does-not-exist"))
	if !errors.Is(err, ErrNoModelRoute) {
		t.Fatalf("err = %v, want ErrNoModelRoute for a model that does not exist", err)
	}
	if errors.Is(err, ErrModelNotCapable) {
		t.Fatalf("err = %v also matches ErrModelNotCapable -- the model does not exist, so nothing is known about its capabilities", err)
	}
}

// A provisioning denial must not be reported as a capability refusal either,
// and here the model is genuinely image-CAPABLE -- so the capability sentinel
// would be affirmatively false about a mapping carrying `image: yes`.
// ErrNoModelRoute (404) is also the no-leak answer the codebase gives a
// provisioning denial everywhere else: the principal learns nothing about a
// server it may not use.
func TestResolveProvisioningDenialIsNoModelRouteNotIncapable(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	mem := seededGroupStore(t, now)
	if err := mem.UpsertMappingCapabilities(ctx, "map_a", []CapabilityRow{
		{Capability: CapabilityImage, Verdict: CapabilityYes, Source: CapabilitySourceManual, CheckedAt: now},
	}); err != nil {
		t.Fatalf("seed map_a: %v", err)
	}
	r := NewResolver(mem, func() time.Time { return now }, nil)
	r.SetProvisioningGate(fakeGate{allow: map[string]bool{}}) // every server denied

	_, err := r.Resolve(ctx, auth.Token{ID: "tok_1"}, imagesReq("coder-a"))
	if !errors.Is(err, ErrNoModelRoute) {
		t.Fatalf("err = %v, want ErrNoModelRoute when provisioning denies every server", err)
	}
	if errors.Is(err, ErrModelNotCapable) {
		t.Fatalf("err = %v also matches ErrModelNotCapable -- map_a carries image:yes, so that claim is false", err)
	}
}

// --- Site 2: the server-override branch's gate ----------------------------
//
// resolveServerOverride short-circuits everything else in Resolve, so its own
// filterCapable call is the ONLY capability check a server-override request
// meets. Before these two tests, deleting that call left both
// ./internal/routing/... and ./internal/gateway/... fully green -- the one gate
// site of the four with no coverage at all, and reachable in production
// (inferencePreflight -> applyServerOverride stamps req.ServerOverrideID for an
// images request like any other).

// An override naming a server that DOES offer the model, via a mapping with no
// image verdict, is refused: ErrServerOverrideModelUnavailable, the override's
// own sentinel for "that server cannot serve this model" (the capability case
// shares it deliberately -- see the sentinel's doc comment).
func TestServerOverrideRefusesIncapableMapping(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	// map_over offers qwen-coder on srv_over and carries NO image verdict.
	store := maintenanceOverrideStore(t, now, ServerStatusActive, HealthHealthy)
	r := NewResolver(store, func() time.Time { return now }, nil)

	req := imagesReq("qwen-coder")
	req.ServerOverrideID = "srv_over"
	_, err := r.Resolve(ctx, auth.Token{}, req)
	if !errors.Is(err, ErrServerOverrideModelUnavailable) {
		t.Fatalf("err = %v, want ErrServerOverrideModelUnavailable for an override whose mapping has no image verdict", err)
	}
}

// The contrast that proves the refusal above is the GATE and not the fixture:
// the same store, the same override, one `image: yes` row added -> served.
func TestServerOverrideServesCapableMapping(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	store := maintenanceOverrideStore(t, now, ServerStatusActive, HealthHealthy)
	if err := store.UpsertMappingCapabilities(ctx, "map_over", []CapabilityRow{
		{Capability: CapabilityImage, Verdict: CapabilityYes, Source: CapabilitySourceManual, CheckedAt: now},
	}); err != nil {
		t.Fatalf("seed map_over: %v", err)
	}
	r := NewResolver(store, func() time.Time { return now }, nil)

	req := imagesReq("qwen-coder")
	req.ServerOverrideID = "srv_over"
	target, err := r.Resolve(ctx, auth.Token{}, req)
	if err != nil {
		t.Fatalf("Resolve: %v, want the image-capable override target served", err)
	}
	if target.ServerID != "srv_over" || target.RouteID != "map_over" {
		t.Fatalf("target = {%q,%q}, want {srv_over, map_over}", target.ServerID, target.RouteID)
	}
}

// --- The speech endpoint through the same gate ------------------------------
//
// /v1/audio/speech adds no gate of its own: it declares
// RequiredCapabilities = [CapabilitySpeech] and meets the one filterCapable /
// resolveAffinity / resolveServerOverride gate every capability-carrying
// request meets. The tests below pin that the SPEECH capability flows through
// each of the four Resolve branches the image gate is pinned on: a model
// without a (speech, yes) verdict is refused, whether the verdict is absent (an
// unprobed model, which must never fail open), an explicit "no" (an audio
// model whose task is not speech) or another capability's "yes".

// speechReq is a speech request: the endpoint's identity, expressed as the
// required-capability list handleOpenAISpeech sets.
func speechReq(model string) inference.Request {
	return inference.Request{
		Model:                model,
		APIFlavor:            "openai_audio",
		RequiredCapabilities: []string{CapabilitySpeech},
	}
}

// speechRow is a speech verdict row as the audio.cpp prober writes it.
func speechRow(verdict string, now time.Time) CapabilityRow {
	return CapabilityRow{Capability: CapabilitySpeech, Verdict: verdict, Source: CapabilitySourceAudioCppModels, CheckedAt: now}
}

// declareSpeechFlavor turns the named applications of a seeded store into
// audio.cpp applications declaring openai_audio: speech candidacy admits only
// an application declaring that flavor, and the shared seeds declare the text
// and images ones.
func declareSpeechFlavor(t *testing.T, mem *MemoryStore, appIDs ...string) {
	t.Helper()
	ctx := context.Background()
	for _, id := range appIDs {
		app, err := mem.ApplicationByID(ctx, id)
		if err != nil {
			t.Fatalf("ApplicationByID %s: %v", id, err)
		}
		app.Type = ProviderAudioCpp
		app.APIFlavors = []string{APIFlavorOpenAIAudio}
		if err := mem.UpdateApplication(ctx, app); err != nil {
			t.Fatalf("UpdateApplication %s: %v", id, err)
		}
	}
}

// seededSpeechStore is seededGroupStore with both members declaring openai_audio.
func seededSpeechStore(t *testing.T, now time.Time) *MemoryStore {
	t.Helper()
	mem := seededGroupStore(t, now)
	declareSpeechFlavor(t, mem, "app_a", "app_b")
	return mem
}

// speechUnservedVerdicts are the rows a mapping can carry without being
// speech-capable. The nil case matters most: an unprobed model (audio.cpp lists
// loaded models only) has no row, and the gate refuses on unknown (ADR-042).
func speechUnservedVerdicts(now time.Time) map[string][]CapabilityRow {
	return map[string][]CapabilityRow{
		"unknown (no row)":     nil,
		"explicit no":          {speechRow(CapabilityNo, now)},
		"another capability's": {{Capability: CapabilityImage, Verdict: CapabilityYes, Source: CapabilitySourceManual, CheckedAt: now}},
	}
}

// seedSpeechRows writes rows on a mapping, a nil set meaning "leave it with no
// row at all".
func seedSpeechRows(t *testing.T, mem *MemoryStore, mappingID string, rows []CapabilityRow) {
	t.Helper()
	if rows == nil {
		return
	}
	if err := mem.UpsertMappingCapabilities(context.Background(), mappingID, rows); err != nil {
		t.Fatalf("seed %s: %v", mappingID, err)
	}
}

// The fresh-candidate branch: an unknown, an explicit no and a foreign yes are
// all refused with the capability sentinel (not "no such model"), and the same
// store with a (speech, yes) row serves the model -- the contrast that proves
// the refusal is the gate and not the fixture.
func TestSpeechGateFreshPathRefusesWithoutAYesVerdict(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	for name, rows := range speechUnservedVerdicts(now) {
		t.Run(name, func(t *testing.T) {
			mem := seededSpeechStore(t, now)
			seedSpeechRows(t, mem, "map_a", rows)
			r := NewResolver(mem, func() time.Time { return now }, nil)

			_, err := r.Resolve(ctx, auth.Token{}, speechReq("coder-a"))
			if !errors.Is(err, ErrModelNotCapable) {
				t.Fatalf("err = %v, want ErrModelNotCapable", err)
			}
			if errors.Is(err, ErrNoModelRoute) {
				t.Fatalf("err = %v also matches ErrNoModelRoute -- the two facts must stay distinguishable", err)
			}
		})
	}

	mem := seededSpeechStore(t, now)
	seedSpeechRows(t, mem, "map_a", []CapabilityRow{speechRow(CapabilityYes, now)})
	r := NewResolver(mem, func() time.Time { return now }, nil)
	target, err := r.Resolve(ctx, auth.Token{}, speechReq("coder-a"))
	if err != nil {
		t.Fatalf("Resolve: %v, want the speech-capable mapping served", err)
	}
	if target.ServerID != "srv_a" || target.RouteID != "map_a" {
		t.Fatalf("target = {%q,%q}, want {srv_a, map_a}", target.ServerID, target.RouteID)
	}
}

// The group branch stays a per-member SOFT filter for speech as for images: the
// first-priority member has no (speech, yes) verdict, the second does, and the
// walk must reach it instead of failing the whole group on the first refusal.
func TestSpeechGroupGateSkipsIncapableFirstMemberForCapableSecond(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	for name, rows := range speechUnservedVerdicts(now) {
		t.Run(name, func(t *testing.T) {
			mem := seededSpeechStore(t, now)
			seedSpeechRows(t, mem, "map_a", rows)
			seedSpeechRows(t, mem, "map_b", []CapabilityRow{speechRow(CapabilityYes, now)})
			r := NewResolver(mem, func() time.Time { return now }, nil)
			r.SetGroupResolver(twoMemberGroup("sticky"))

			target, err := r.Resolve(ctx, auth.Token{}, speechReq("coder-group"))
			if err != nil {
				t.Fatalf("Resolve: %v, want the speech-capable second member served", err)
			}
			if target.ServerID != "srv_b" || target.Model != "coder-b" {
				t.Fatalf("target = {%q,%q}, want {srv_b, coder-b}", target.ServerID, target.Model)
			}
		})
	}
}

// The affinity branch: a pin on an application whose mapping has no (speech,
// yes) verdict must not be reused, whatever kind of non-yes it is, and the
// refusal is non-destructive (the pin survives). The pin sits under the speech
// request's OWN key (coarse flavor openai_audio), the only key Resolve looks
// up for it -- a pin under another flavor would never be found and the test
// would pass without the gate running.
func TestSpeechAffinityRefusesAPinnedMappingWithoutAYesVerdict(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	for name, rows := range speechUnservedVerdicts(now) {
		t.Run(name, func(t *testing.T) {
			mem := seededSpeechStore(t, now)
			seedSpeechRows(t, mem, "map_a", rows)
			seedSpeechRows(t, mem, "map_b", []CapabilityRow{speechRow(CapabilityYes, now)})
			key := AffinityKey{APITokenID: "tok_1", Model: "coder-a", APIFlavor: APIFlavorOpenAIAudio, SessionID: "sess_1"}
			seedPinnedAffinity(t, mem, key, "app_a", "srv_a", now)
			r := NewResolver(&affinityGateStore{resolverStore: mem}, func() time.Time { return now }, nil)

			req := speechReq("coder-a")
			req.ClientSessionID = "sess_1"
			target, err := r.Resolve(ctx, auth.Token{ID: "tok_1"}, req)

			// coder-a is the only mapping for that gateway name, so the resolve
			// refuses outright; what must NOT happen is serving the pinned app.
			if err == nil && target.ServerID == "srv_a" {
				t.Fatal("served the pinned but speech-incapable application")
			}
			if !errors.Is(err, ErrModelNotCapable) {
				t.Fatalf("err = %v, want ErrModelNotCapable", err)
			}
			if _, ok, affErr := mem.Affinity(ctx, key); affErr != nil || !ok {
				t.Fatalf("the affinity row was deleted by a speech request (ok=%v, err=%v)", ok, affErr)
			}
		})
	}
}

// A capability read error on the affinity path refuses for speech too. The
// mapping's own stored verdict is "yes", so a fail-open bug that trusts an
// errored read would serve the pin; resolveAffinity is called directly for the
// reason TestAffinityRefusesOnCapabilityReadError gives.
func TestSpeechAffinityRefusesOnCapabilityReadError(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	mem := seededSpeechStore(t, now)
	seedSpeechRows(t, mem, "map_a", []CapabilityRow{speechRow(CapabilityYes, now)})
	key := AffinityKey{APITokenID: "tok_1", Model: "coder-a", APIFlavor: APIFlavorOpenAIAudio, SessionID: "sess_1"}
	seedPinnedAffinity(t, mem, key, "app_a", "srv_a", now)
	r := NewResolver(&affinityGateStore{resolverStore: mem, failKeyed: true}, func() time.Time { return now }, nil)

	req := speechReq("coder-a")
	target, ok, err := r.resolveAffinity(ctx, key, req.APIFlavor, req.RequiredCapabilities, now)
	if err != nil {
		t.Fatalf("resolveAffinity returned an error, want a non-destructive refusal: %v", err)
	}
	if ok {
		t.Fatalf("a failed capability read served the pin anyway (failed open): target=%+v", target)
	}
	if _, rowOK, affErr := mem.Affinity(ctx, key); affErr != nil || !rowOK {
		t.Fatalf("the affinity row was deleted on a read error (ok=%v, err=%v)", rowOK, affErr)
	}
}

// The contrast for the affinity refusals: a pin on a speech-capable mapping IS
// reused, so the gate refuses the incapable pin and not every pin.
func TestSpeechAffinityServesAPinnedCapableMapping(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	mem := seededSpeechStore(t, now)
	seedSpeechRows(t, mem, "map_a", []CapabilityRow{speechRow(CapabilityYes, now)})
	key := AffinityKey{APITokenID: "tok_1", Model: "coder-a", APIFlavor: APIFlavorOpenAIAudio, SessionID: "sess_1"}
	seedPinnedAffinity(t, mem, key, "app_a", "srv_a", now)
	r := NewResolver(&affinityGateStore{resolverStore: mem}, func() time.Time { return now }, nil)

	req := speechReq("coder-a")
	target, ok, err := r.resolveAffinity(ctx, key, req.APIFlavor, req.RequiredCapabilities, now)
	if err != nil || !ok {
		t.Fatalf("resolveAffinity = (%+v, %v, %v), want the pinned speech-capable mapping served", target, ok, err)
	}
	if target.ServerID != "srv_a" {
		t.Fatalf("ServerID = %q, want srv_a", target.ServerID)
	}
}

// A speech resolve writes no pin, on the main or the group path. The key's
// coarse flavor is its own (openai_audio), so it cannot collide with a text
// client's; the capability guard (keyed on the list, not the flavor) keeps it
// pin-free all the same.
func TestSpeechResolveWritesNoAffinityPin(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	mem := seededSpeechStore(t, now)
	for _, id := range []string{"map_a", "map_b"} {
		seedSpeechRows(t, mem, id, []CapabilityRow{speechRow(CapabilityYes, now)})
	}
	store := &affinityGateStore{resolverStore: mem}
	r := NewResolver(store, func() time.Time { return now }, nil)
	r.SetGroupResolver(twoMemberGroup("sticky"))

	for _, model := range []string{"coder-a", "coder-group"} {
		req := speechReq(model)
		req.ClientSessionID = "sess_1"
		if _, err := r.Resolve(ctx, auth.Token{ID: "tok_1"}, req); err != nil {
			t.Fatalf("Resolve(%s): %v", model, err)
		}
	}
	if store.upserts != 0 {
		t.Fatalf("affinity writes = %d, want 0: a capability-carrying resolve must not pin", store.upserts)
	}
}

// The server-override branch short-circuits everything else in Resolve, so its
// own filterCapable call is the only speech check an override request meets: an
// override naming a server that offers the model through a mapping without a
// (speech, yes) verdict is refused, and the same override with one is served.
func TestSpeechServerOverrideGate(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	for name, rows := range speechUnservedVerdicts(now) {
		t.Run("refuses "+name, func(t *testing.T) {
			mem := maintenanceOverrideStore(t, now, ServerStatusActive, HealthHealthy)
			declareSpeechFlavor(t, mem, "app_over")
			seedSpeechRows(t, mem, "map_over", rows)
			r := NewResolver(mem, func() time.Time { return now }, nil)

			req := speechReq("qwen-coder")
			req.ServerOverrideID = "srv_over"
			_, err := r.Resolve(ctx, auth.Token{}, req)
			if !errors.Is(err, ErrServerOverrideModelUnavailable) {
				t.Fatalf("err = %v, want ErrServerOverrideModelUnavailable for an override whose mapping has no (speech, yes) verdict", err)
			}
		})
	}

	mem := maintenanceOverrideStore(t, now, ServerStatusActive, HealthHealthy)
	declareSpeechFlavor(t, mem, "app_over")
	seedSpeechRows(t, mem, "map_over", []CapabilityRow{speechRow(CapabilityYes, now)})
	r := NewResolver(mem, func() time.Time { return now }, nil)
	req := speechReq("qwen-coder")
	req.ServerOverrideID = "srv_over"
	target, err := r.Resolve(ctx, auth.Token{}, req)
	if err != nil {
		t.Fatalf("Resolve: %v, want the speech-capable override target served", err)
	}
	if target.ServerID != "srv_over" || target.RouteID != "map_over" {
		t.Fatalf("target = {%q,%q}, want {srv_over, map_over}", target.ServerID, target.RouteID)
	}
}

// The vendor-account branch declines a speech request. Nothing in
// vendorAccountRoutingEligible names the openai_audio flavor (the images flavor
// is excluded by name); a speech request avoids the branch because it carries
// RequiredCapabilities = [speech], which the predicate does exclude. That makes
// the handler's capability list load-bearing twice: it feeds the gate AND keeps
// the request off a vendor account, whose model match reads the model NAME and
// never the flavor, so a vendor model sharing a speech request's model name
// would otherwise be served by a vendor with no speech endpoint here. The
// handler's side of this pin is TestSpeechInferenceShapeIsTheSpeechEndpointsIdentity
// in internal/gateway.
func TestVendorAccountSkippedForSpeech(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	store := NewMemoryStore()
	seedVendorAccount(t, store, now, "acc_openai", vendorOwner, VendorOpenAI, vendorKey, "", "gpt-4o", APIFlavorOpenAI)
	resolver := vendorResolver(store, now, true, vendorRoutingModeVendorFirst)

	req := speechReq("gpt-4o")
	if resolver.vendorAccountRoutingEligible(ownerToken(), req, NormalizeAPIFlavor(req.APIFlavor)) {
		t.Fatal("a speech request is eligible for the vendor-account path, want it declined")
	}
	if _, err := resolver.Resolve(ctx, ownerToken(), req); !errors.Is(err, ErrNoModelRoute) {
		t.Fatalf("Resolve(speech) = %v, want ErrNoModelRoute (the vendor branch declines a capability-carrying request)", err)
	}
}

// --- The transcription capability through the same gate ---------------------
//
// CapabilityTranscription means the model TRANSCRIBES audio to text (it may be
// the target of POST /v1/audio/transcriptions). It is a plain literal name:
// capabilityRowsSatisfy needs no derivation for it, exactly as for speech, so a
// (transcription, yes) row satisfies the requirement and anything else refuses.
// The name must also stay distinct from CapabilityAudio (a model that ACCEPTS
// audio as input) and CapabilitySpeech (a model that SYNTHESISES it): advertising
// one as another fails far from its cause.
func TestCapabilityRowsSatisfyTranscriptionIsALiteralYes(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	row := func(capability, verdict string) CapabilityRow {
		return CapabilityRow{Capability: capability, Verdict: verdict, Source: CapabilitySourceManual, CheckedAt: now}
	}
	required := []string{CapabilityTranscription}

	if CapabilityTranscription != "transcription" {
		t.Fatalf("CapabilityTranscription = %q, want %q", CapabilityTranscription, "transcription")
	}
	for _, other := range []string{CapabilityAudio, CapabilitySpeech} {
		if CapabilityTranscription == other {
			t.Fatalf("CapabilityTranscription collides with %q -- the three audio names are orthogonal", other)
		}
	}

	for _, tc := range []struct {
		name string
		rows []CapabilityRow
		want bool
	}{
		{"a yes row satisfies", []CapabilityRow{row(CapabilityTranscription, CapabilityYes)}, true},
		{"an explicit no refuses", []CapabilityRow{row(CapabilityTranscription, CapabilityNo)}, false},
		{"an absent row refuses (unknown never fails open)", nil, false},
		{"speech:yes is not a grant", []CapabilityRow{row(CapabilitySpeech, CapabilityYes)}, false},
		{"audio:yes is not a grant", []CapabilityRow{row(CapabilityAudio, CapabilityYes)}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := capabilityRowsSatisfy(tc.rows, required); got != tc.want {
				t.Fatalf("capabilityRowsSatisfy(%+v, %v) = %v, want %v", tc.rows, required, got, tc.want)
			}
		})
	}
	// And the other direction: a transcription yes must not leak into the
	// neighbouring audio names.
	rows := []CapabilityRow{row(CapabilityTranscription, CapabilityYes)}
	for _, other := range []string{CapabilityAudio, CapabilitySpeech} {
		if capabilityRowsSatisfy(rows, []string{other}) {
			t.Fatalf("transcription:yes satisfied a %q requirement", other)
		}
	}
}
