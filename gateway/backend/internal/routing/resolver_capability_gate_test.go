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
