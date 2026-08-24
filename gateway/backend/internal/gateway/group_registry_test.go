// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"op-ai-gateway/internal/routing"
	"reflect"
	"strings"
	"testing"
	"time"
)

// groupSeedStore seeds two ACTIVE groups (one climb_up with two priority-ordered
// members, one sticky with a single member), a DISABLED group (must be skipped), and
// two model_settings rows (one "locked", one "shown").
func groupSeedStore(t *testing.T) *routing.MemoryStore {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	mem := routing.NewMemoryStore()
	mustGroup := func(g routing.ModelGroup) {
		if err := mem.CreateModelGroup(ctx, g); err != nil {
			t.Fatalf("CreateModelGroup %s: %v", g.ID, err)
		}
	}
	mustGroup(routing.ModelGroup{
		ID: "grp_1", GatewayModelName: "group-one", DisplayName: "Group One",
		Status: routing.ServerStatusActive, FailoverMode: "climb_up",
		LoadedOnly: true, MemberOrder: routing.MemberOrderSpeed,
		ClimbSpeedMarginPercent: 35, MinTokensPerSecond: 12.5,
		MinSpeedFallback: routing.MinSpeedFallbackIgnore,
		CreatedAt:        now, UpdatedAt: now,
	})
	mustGroup(routing.ModelGroup{
		ID: "grp_2", GatewayModelName: "Group-Two", DisplayName: "Group Two",
		Status: routing.ServerStatusActive, FailoverMode: "sticky",
		// Garbage enum values: the registry must fail these open to the defaults.
		MemberOrder: "bogus-order", MinSpeedFallback: "bogus-fallback",
		CreatedAt: now, UpdatedAt: now,
	})
	mustGroup(routing.ModelGroup{ID: "grp_3", GatewayModelName: "group-disabled", DisplayName: "Disabled", Status: routing.ServerStatusDisabled, FailoverMode: "sticky", CreatedAt: now, UpdatedAt: now})
	if err := mem.SetGroupMembers(ctx, "grp_1", []routing.GroupMember{
		{MemberGatewayName: "m-a", Priority: 0},
		{MemberGatewayName: "m-b", Priority: 1},
	}); err != nil {
		t.Fatalf("SetGroupMembers grp_1: %v", err)
	}
	if err := mem.SetGroupMembers(ctx, "grp_2", []routing.GroupMember{{MemberGatewayName: "only", Priority: 0}}); err != nil {
		t.Fatalf("SetGroupMembers grp_2: %v", err)
	}
	if err := mem.UpsertModelSetting(ctx, routing.ModelSetting{GatewayModelName: "locked-model", Visibility: "locked", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("UpsertModelSetting locked: %v", err)
	}
	if err := mem.UpsertModelSetting(ctx, routing.ModelSetting{GatewayModelName: "shown-model", Visibility: "shown", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("UpsertModelSetting shown: %v", err)
	}
	// A hidden model is delisted but still DIRECTLY requestable — only "locked" refuses.
	if err := mem.UpsertModelSetting(ctx, routing.ModelSetting{GatewayModelName: "hidden-model", Visibility: "hidden", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("UpsertModelSetting hidden: %v", err)
	}
	return mem
}

func TestGroupRegistryRefreshBuildsSnapshot(t *testing.T) {
	reg := NewGroupRegistry(groupSeedStore(t))
	if err := reg.RefreshGroups(context.Background()); err != nil {
		t.Fatalf("RefreshGroups: %v", err)
	}

	members, policy, ok := reg.Group("group-one")
	if !ok {
		t.Fatal("Group(group-one) not found")
	}
	if policy.FailoverMode != "climb_up" {
		t.Fatalf("FailoverMode = %q, want climb_up", policy.FailoverMode)
	}
	if !policy.LoadedOnly || policy.MemberOrder != routing.MemberOrderSpeed ||
		policy.ClimbSpeedMarginPercent != 35 || policy.MinTokensPerSecond != 12.5 ||
		policy.MinSpeedFallback != routing.MinSpeedFallbackIgnore {
		t.Fatalf("policy = %+v, want all six non-default values carried through", policy)
	}
	if len(members) != 2 || members[0].MemberGatewayName != "m-a" || members[1].MemberGatewayName != "m-b" {
		t.Fatalf("members = %+v, want [m-a, m-b] in priority order", members)
	}

	// Case-insensitive lookup (a stored mixed-case name is keyed lowercased).
	if _, _, ok := reg.Group("GROUP-ONE"); !ok {
		t.Fatal("case-insensitive Group(GROUP-ONE) failed")
	}
	twoMembers, twoPolicy, ok := reg.Group("group-two")
	if !ok {
		t.Fatal("Group(group-two) failed (stored as Group-Two)")
	}
	_ = twoMembers
	if twoPolicy.MemberOrder != routing.MemberOrderPriority {
		t.Fatalf("unknown MemberOrder should fail open to %q, got %q", routing.MemberOrderPriority, twoPolicy.MemberOrder)
	}
	if twoPolicy.MinSpeedFallback != routing.MinSpeedFallbackError {
		t.Fatalf("unknown MinSpeedFallback should fail open to %q, got %q", routing.MinSpeedFallbackError, twoPolicy.MinSpeedFallback)
	}

	// A disabled group is not offered.
	if _, _, ok := reg.Group("group-disabled"); ok {
		t.Fatal("disabled group should not be in the snapshot")
	}
	// An unknown name.
	if _, _, ok := reg.Group("nope"); ok {
		t.Fatal("Group(nope) should report ok == false for an unknown name")
	}

	// Group returns a COPY: mutating it must not leak into the registry.
	members[0].MemberGatewayName = "MUTATED"
	again, _, _ := reg.Group("group-one")
	if again[0].MemberGatewayName != "m-a" {
		t.Fatal("Group returned an aliased slice; a caller mutation leaked into the registry")
	}
}

func TestGroupRegistryDirectAllowed(t *testing.T) {
	reg := NewGroupRegistry(groupSeedStore(t))
	if err := reg.RefreshGroups(context.Background()); err != nil {
		t.Fatalf("RefreshGroups: %v", err)
	}
	if reg.DirectAllowed("locked-model") {
		t.Fatal("DirectAllowed(locked-model) = true, want false (locked)")
	}
	if reg.DirectAllowed("LOCKED-MODEL") {
		t.Fatal("DirectAllowed should be case-insensitive for a locked name")
	}
	if !reg.DirectAllowed("shown-model") {
		t.Fatal("DirectAllowed(shown-model) = false, want true")
	}
	if !reg.DirectAllowed("hidden-model") {
		t.Fatal("DirectAllowed(hidden-model) = false, want true (hidden is delisted but still directly requestable; only locked refuses)")
	}
	if !reg.DirectAllowed("no-setting-at-all") {
		t.Fatal("DirectAllowed(missing) = false, want true (nothing locked)")
	}
}

// TestGroupRegistryGroupNameVisibility proves the registry treats a GROUP name's
// model_settings row exactly like a model's: a "locked" group name is refused a
// direct request (DirectAllowed=false), a "hidden" group name is not (only "locked"
// refuses). A group name is itself a gateway_model_name, so RefreshGroups — which
// iterates ALL model_settings — already picks it up without any group-specific code.
func TestGroupRegistryGroupNameVisibility(t *testing.T) {
	ctx := context.Background()
	mem := groupSeedStore(t)
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	// group-one → locked; Group-Two → hidden (both are ACTIVE group names).
	if err := mem.UpsertModelSetting(ctx, routing.ModelSetting{GatewayModelName: "group-one", Visibility: "locked", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("lock group-one: %v", err)
	}
	if err := mem.UpsertModelSetting(ctx, routing.ModelSetting{GatewayModelName: "Group-Two", Visibility: "hidden", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("hide Group-Two: %v", err)
	}
	reg := NewGroupRegistry(mem)
	if err := reg.RefreshGroups(ctx); err != nil {
		t.Fatalf("RefreshGroups: %v", err)
	}
	// A locked group NAME is refused a direct request (case-insensitive).
	if reg.DirectAllowed("group-one") {
		t.Fatal("DirectAllowed(group-one) = true, want false (locked group name)")
	}
	if reg.DirectAllowed("GROUP-ONE") {
		t.Fatal("locked group DirectAllowed should be case-insensitive")
	}
	// It is still a known active group (the resolver refuses via DirectAllowed BEFORE
	// resolveGroup, not by dropping the group).
	if _, _, ok := reg.Group("group-one"); !ok {
		t.Fatal("Group(group-one) should still be a known active group")
	}
	// A hidden group NAME is delisted but still directly requestable.
	if !reg.DirectAllowed("Group-Two") {
		t.Fatal("DirectAllowed(Group-Two) = false, want true (hidden group is not locked)")
	}
	if _, _, ok := reg.Group("group-two"); !ok {
		t.Fatal("Group(group-two) should still be a known active group")
	}
}

func TestGroupRegistryNilSafe(t *testing.T) {
	var reg *GroupRegistry
	if err := reg.RefreshGroups(context.Background()); err != nil {
		t.Fatalf("nil RefreshGroups returned %v, want nil", err)
	}
	if _, _, ok := reg.Group("x"); ok {
		t.Fatal("nil Group should return ok=false")
	}
	if !reg.DirectAllowed("x") {
		t.Fatal("nil DirectAllowed should return true (a nil registry locks nothing)")
	}
}

// nestedGroupSeedStore seeds group G (round_robin) = [A, H, B, I] where H (depth) =
// [C, D] and I (depth) = [E, F, J]. H and I are themselves ACTIVE groups (subgroups),
// while A/B/C/D/E/F/J are bare names absent from the group graph (leaf models — no
// real mapping needed, since FlattenGroup/the registry only cares whether a member
// name is itself a known group).
func nestedGroupSeedStore(t *testing.T) *routing.MemoryStore {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
	mem := routing.NewMemoryStore()
	mustGroup := func(id, name, traversal string) {
		if err := mem.CreateModelGroup(ctx, routing.ModelGroup{
			ID: id, GatewayModelName: name, DisplayName: name,
			Status: routing.ServerStatusActive, FailoverMode: "climb_up",
			Traversal: traversal, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("CreateModelGroup %s: %v", id, err)
		}
	}
	mustMembers := func(id string, names ...string) {
		members := make([]routing.GroupMember, len(names))
		for i, n := range names {
			members[i] = routing.GroupMember{MemberGatewayName: n, Priority: i}
		}
		if err := mem.SetGroupMembers(ctx, id, members); err != nil {
			t.Fatalf("SetGroupMembers %s: %v", id, err)
		}
	}
	mustGroup("grp_g", "G", "round_robin")
	mustGroup("grp_h", "H", "depth")
	mustGroup("grp_i", "I", "depth")
	mustMembers("grp_g", "A", "H", "B", "I")
	mustMembers("grp_h", "C", "D")
	mustMembers("grp_i", "E", "F", "J")
	return mem
}

// TestGroupRegistryFlattensNestedGroups proves RefreshGroups precomputes each active
// group's FLATTENED leaf-model list (via routing.FlattenGroup) so Group() returns the
// already-expanded, priority-ordered candidate list the resolver's failover consumes
// unchanged — no group names should ever reach the resolver.
func TestGroupRegistryFlattensNestedGroups(t *testing.T) {
	reg := NewGroupRegistry(nestedGroupSeedStore(t))
	if err := reg.RefreshGroups(context.Background()); err != nil {
		t.Fatalf("RefreshGroups: %v", err)
	}

	members, policy, ok := reg.Group("G")
	if !ok {
		t.Fatal("Group(G) not found")
	}
	if policy.FailoverMode != "climb_up" {
		t.Fatalf("FailoverMode = %q, want climb_up", policy.FailoverMode)
	}
	want := []string{"A", "C", "B", "E", "D", "F", "J"}
	got := make([]string, len(members))
	for i, m := range members {
		got[i] = m.MemberGatewayName
		if m.Priority != i {
			t.Fatalf("members[%d].Priority = %d, want %d (flattened order IS the priority)", i, m.Priority, i)
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Group(G) flattened order = %v, want %v", got, want)
	}

	// A subgroup name itself must not leak into the resolver's candidate list — only
	// leaf models. H and I never appear in the flattened list.
	for _, name := range got {
		if strings.EqualFold(name, "H") || strings.EqualFold(name, "I") {
			t.Fatalf("subgroup name %q leaked into the flattened member list %v", name, got)
		}
	}
}

// TestGroupRegistryFlattenCycleSafe proves a cyclic group pair does not hang
// RefreshGroups (returns nil, no error) and that Group() returns the cycle-safe
// flattened list (the back-edge contributes nothing, per routing.FlattenGroup).
func TestGroupRegistryFlattenCycleSafe(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
	mem := routing.NewMemoryStore()
	mustGroup := func(id, name string) {
		if err := mem.CreateModelGroup(ctx, routing.ModelGroup{
			ID: id, GatewayModelName: name, DisplayName: name,
			Status: routing.ServerStatusActive, FailoverMode: "sticky",
			Traversal: "depth", CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("CreateModelGroup %s: %v", id, err)
		}
	}
	mustMembers := func(id string, names ...string) {
		members := make([]routing.GroupMember, len(names))
		for i, n := range names {
			members[i] = routing.GroupMember{MemberGatewayName: n, Priority: i}
		}
		if err := mem.SetGroupMembers(ctx, id, members); err != nil {
			t.Fatalf("SetGroupMembers %s: %v", id, err)
		}
	}
	// G -> [A, H, G] (self-ref + indirect via H); H -> [B, G] (back to G).
	mustGroup("grp_cyc_g", "cyc-g")
	mustGroup("grp_cyc_h", "cyc-h")
	mustMembers("grp_cyc_g", "A", "cyc-h", "cyc-g")
	mustMembers("grp_cyc_h", "B", "cyc-g")

	reg := NewGroupRegistry(mem)
	done := make(chan error, 1)
	go func() { done <- reg.RefreshGroups(ctx) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RefreshGroups: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RefreshGroups hung on a cyclic group graph")
	}

	members, _, ok := reg.Group("cyc-g")
	if !ok {
		t.Fatal("Group(cyc-g) not found")
	}
	got := make([]string, len(members))
	for i, m := range members {
		got[i] = m.MemberGatewayName
	}
	want := []string{"A", "B"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Group(cyc-g) cycle-safe flattened order = %v, want %v", got, want)
	}
}

func TestGroupRegistryRefreshReflectsStoreChange(t *testing.T) {
	ctx := context.Background()
	mem := groupSeedStore(t)
	reg := NewGroupRegistry(mem)
	if err := reg.RefreshGroups(ctx); err != nil {
		t.Fatalf("RefreshGroups: %v", err)
	}

	// Disable group-one and lock shown-model, then refresh — both changes must show.
	g, err := mem.ModelGroupByID(ctx, "grp_1")
	if err != nil {
		t.Fatalf("ModelGroupByID: %v", err)
	}
	g.Status = routing.ServerStatusDisabled
	if err := mem.UpdateModelGroup(ctx, g); err != nil {
		t.Fatalf("UpdateModelGroup: %v", err)
	}
	if err := mem.UpsertModelSetting(ctx, routing.ModelSetting{GatewayModelName: "shown-model", Visibility: "locked"}); err != nil {
		t.Fatalf("UpsertModelSetting: %v", err)
	}

	if err := reg.RefreshGroups(ctx); err != nil {
		t.Fatalf("RefreshGroups #2: %v", err)
	}
	if _, _, ok := reg.Group("group-one"); ok {
		t.Fatal("group-one should be gone from the snapshot after disable + refresh")
	}
	if reg.DirectAllowed("shown-model") {
		t.Fatal("shown-model should be locked (DirectAllowed=false) after refresh")
	}
}
