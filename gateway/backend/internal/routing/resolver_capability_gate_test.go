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
