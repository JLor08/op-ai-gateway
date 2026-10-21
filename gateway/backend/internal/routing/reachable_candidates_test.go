// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package routing

import (
	"context"
	"errors"
	"op-ai-gateway/internal/auth"
	"reflect"
	"sort"
	"testing"
	"time"
)

// errGate is a ProvisioningGate that always fails.
type errGate struct{ err error }

func (g errGate) AllowedServerIDs(context.Context, auth.Token, []string) (map[string]bool, error) {
	return nil, g.err
}

// reachableMappingIDs is the sorted mapping ids of cands, so a test states the
// set it expects without depending on the order members were walked in.
func reachableMappingIDs(cands []MappingCandidate) []string {
	ids := make([]string, 0, len(cands))
	for _, c := range cands {
		ids = append(ids, c.Mapping.ID)
	}
	sort.Strings(ids)
	return ids
}

var reachablePrincipal = auth.Token{ID: "tok_dev", UserID: "usr_dev", Active: true}

// A plain model name reaches its own active mappings, and a fine flavor is
// folded to its coarse one exactly as dispatch does it.
func TestReachableCandidatesForAPlainModel(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	store := seededGroupStore(t, now)
	resolver := NewResolver(store, func() time.Time { return now }, nil)
	resolver.SetGroupResolver(&fakeGroups{})

	got, err := resolver.ReachableCandidates(context.Background(), reachablePrincipal, "coder-a", "openai_chat")
	if err != nil {
		t.Fatalf("ReachableCandidates: %v", err)
	}
	if want := []string{"map_a"}; !reflect.DeepEqual(reachableMappingIDs(got), want) {
		t.Fatalf("mappings = %v, want %v", reachableMappingIDs(got), want)
	}

	// The fixture's applications serve text and images, not audio: a model no
	// application serves the flavor for is no candidate, as in dispatch.
	got, err = resolver.ReachableCandidates(context.Background(), reachablePrincipal, "coder-a", APIFlavorOpenAIAudio)
	if err != nil {
		t.Fatalf("ReachableCandidates: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("audio candidates = %v, want none", reachableMappingIDs(got))
	}

	got, err = resolver.ReachableCandidates(context.Background(), reachablePrincipal, "no-such-model", APIFlavorOpenAI)
	if err != nil {
		t.Fatalf("ReachableCandidates: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("unknown model candidates = %v, want none", reachableMappingIDs(got))
	}
}

// A group name is expanded to its members' mappings, each member counted once.
func TestReachableCandidatesExpandsAGroup(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	store := seededGroupStore(t, now)
	resolver := NewResolver(store, func() time.Time { return now }, nil)
	resolver.SetGroupResolver(&fakeGroups{groups: map[string]fakeGroup{
		"coder-group": {members: []GroupMember{
			{MemberGatewayName: "coder-a", Priority: 0},
			{MemberGatewayName: "coder-b", Priority: 1},
			{MemberGatewayName: "coder-a", Priority: 2},
		}},
	}})

	got, err := resolver.ReachableCandidates(context.Background(), reachablePrincipal, "coder-group", APIFlavorOpenAI)
	if err != nil {
		t.Fatalf("ReachableCandidates: %v", err)
	}
	if want := []string{"map_a", "map_b"}; !reflect.DeepEqual(reachableMappingIDs(got), want) {
		t.Fatalf("mappings = %v, want %v", reachableMappingIDs(got), want)
	}
}

// "locked" refuses a DIRECT request: the locked model is not reachable by its own
// name, is reachable through a group that lists it, and a locked GROUP is not
// reachable at all.
func TestReachableCandidatesHonoursTheLockedRule(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	store := seededGroupStore(t, now)
	resolver := NewResolver(store, func() time.Time { return now }, nil)
	resolver.SetGroupResolver(&fakeGroups{
		groups: map[string]fakeGroup{
			"open-group":   {members: []GroupMember{{MemberGatewayName: "coder-a"}}},
			"locked-group": {members: []GroupMember{{MemberGatewayName: "coder-b"}}},
		},
		locked: map[string]bool{"coder-a": true, "locked-group": true},
	})
	ctx := context.Background()

	for name, want := range map[string][]string{
		"coder-a":      {},
		"open-group":   {"map_a"},
		"locked-group": {},
		"coder-b":      {"map_b"},
	} {
		got, err := resolver.ReachableCandidates(ctx, reachablePrincipal, name, APIFlavorOpenAI)
		if err != nil {
			t.Fatalf("ReachableCandidates(%q): %v", name, err)
		}
		if ids := reachableMappingIDs(got); !reflect.DeepEqual(ids, want) {
			t.Fatalf("ReachableCandidates(%q) = %v, want %v", name, ids, want)
		}
	}
}

// The provisioning gate cuts candidates for a plain model and member by member
// for a group, and a gate failure is the caller's error, never a silent allow.
func TestReachableCandidatesAppliesTheProvisioningGate(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	store := seededGroupStore(t, now)
	groups := &fakeGroups{groups: map[string]fakeGroup{
		"coder-group": {members: []GroupMember{{MemberGatewayName: "coder-a"}, {MemberGatewayName: "coder-b"}}},
	}}
	ctx := context.Background()

	resolver := NewResolver(store, func() time.Time { return now }, nil)
	resolver.SetGroupResolver(groups)
	resolver.SetProvisioningGate(fakeGate{allow: map[string]bool{"srv_b": true}})

	got, err := resolver.ReachableCandidates(ctx, reachablePrincipal, "coder-a", APIFlavorOpenAI)
	if err != nil || len(got) != 0 {
		t.Fatalf("coder-a on an unprovisioned server = %v, %v; want none", reachableMappingIDs(got), err)
	}
	got, err = resolver.ReachableCandidates(ctx, reachablePrincipal, "coder-group", APIFlavorOpenAI)
	if err != nil {
		t.Fatalf("ReachableCandidates: %v", err)
	}
	if want := []string{"map_b"}; !reflect.DeepEqual(reachableMappingIDs(got), want) {
		t.Fatalf("group mappings = %v, want %v", reachableMappingIDs(got), want)
	}

	boom := errors.New("gate down")
	resolver.SetProvisioningGate(errGate{err: boom})
	if _, err := resolver.ReachableCandidates(ctx, reachablePrincipal, "coder-a", APIFlavorOpenAI); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the gate's error", err)
	}
}

// A resolver without groups or a gate (the no-op invariant) and a nil resolver
// both answer without panicking.
func TestReachableCandidatesIsNilSafe(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	store := seededGroupStore(t, now)
	resolver := NewResolver(store, func() time.Time { return now }, nil)

	got, err := resolver.ReachableCandidates(context.Background(), reachablePrincipal, "coder-b", APIFlavorOpenAI)
	if err != nil {
		t.Fatalf("ReachableCandidates: %v", err)
	}
	if want := []string{"map_b"}; !reflect.DeepEqual(reachableMappingIDs(got), want) {
		t.Fatalf("mappings = %v, want %v", reachableMappingIDs(got), want)
	}

	var none *Resolver
	if got, err := none.ReachableCandidates(context.Background(), reachablePrincipal, "coder-a", APIFlavorOpenAI); err != nil || got != nil {
		t.Fatalf("nil resolver = %v, %v; want nil, nil", got, err)
	}
}
