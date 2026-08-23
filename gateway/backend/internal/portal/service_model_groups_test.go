// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"errors"
	"op-ai-gateway/internal/routing"
	"testing"
	"time"
)

// seedGroupModel creates an active mapping so gateway model NAME `gateway` exists
// (and is offered by Models()). gateway == upstream for simplicity.
func seedGroupModel(t *testing.T, svc *Service, appID, gateway string) {
	t.Helper()
	if _, err := svc.CreateMapping(context.Background(), systemAdminToken(), appID, CreateMappingRequest{
		GatewayModelName: gateway,
		AppModelName:     gateway,
	}); err != nil {
		t.Fatalf("seed mapping %s: %v", gateway, err)
	}
}

// newGroupTestService builds a Service with a server + app + the named active
// mappings, so those names are real gateway models available as group members.
func newGroupTestService(t *testing.T, now time.Time, models ...string) (*Service, string) {
	t.Helper()
	svc, _ := newServerTestService(t, now)
	server := createTestServer(t, svc, "S", "s.example.test")
	app, err := svc.CreateApplication(context.Background(), systemAdminToken(), server.ID, CreateApplicationRequest{
		Type: routing.ProviderVLLM, Port: 8000, Scheme: "https",
	})
	if err != nil {
		t.Fatalf("create app: %v", err)
	}
	for _, m := range models {
		seedGroupModel(t, svc, app.ID, m)
	}
	return svc, app.ID
}

func TestModelGroupCRUDRoundTrip(t *testing.T) {
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	svc, _ := newGroupTestService(t, now, "fast", "mid", "slow")
	ctx := context.Background()

	created, err := svc.CreateModelGroup(ctx, adminToken(), CreateModelGroupRequest{
		GatewayModelName: "coder",
		DisplayName:      "Coder Failover",
		Members:          []GroupMemberInput{{MemberGatewayName: "fast"}, {MemberGatewayName: "mid"}, {MemberGatewayName: "slow"}},
	})
	if err != nil {
		t.Fatalf("CreateModelGroup: %v", err)
	}
	if created.ID == "" || created.GatewayModelName != "coder" || created.DisplayName != "Coder Failover" {
		t.Fatalf("created = %#v", created)
	}
	if created.Status != routing.ServerStatusActive {
		t.Fatalf("default status = %q, want active", created.Status)
	}
	if created.FailoverMode != "sticky" {
		t.Fatalf("default failover mode = %q, want sticky", created.FailoverMode)
	}
	if created.Traversal != "round_robin" {
		t.Fatalf("default traversal = %q, want round_robin", created.Traversal)
	}
	if len(created.Members) != 3 || created.Members[0].MemberGatewayName != "fast" || created.Members[2].MemberGatewayName != "slow" {
		t.Fatalf("members (priority order) = %#v", created.Members)
	}

	// List
	list, err := svc.ListModelGroups(ctx, adminToken())
	if err != nil || len(list.Data) != 1 {
		t.Fatalf("ListModelGroups = %#v err=%v", list, err)
	}

	// Get
	got, err := svc.GetModelGroup(ctx, adminToken(), created.ID)
	if err != nil || got.GatewayModelName != "coder" || len(got.Members) != 3 {
		t.Fatalf("GetModelGroup = %#v err=%v", got, err)
	}

	// Update: rename, switch mode, switch traversal, reorder + drop a member.
	newName := "coder-v2"
	mode := "climb_up"
	traversal := "depth"
	members := []GroupMemberInput{{MemberGatewayName: "slow"}, {MemberGatewayName: "fast"}}
	upd, err := svc.UpdateModelGroup(ctx, adminToken(), created.ID, UpdateModelGroupRequest{
		GatewayModelName: &newName,
		FailoverMode:     &mode,
		Traversal:        &traversal,
		Members:          &members,
	})
	if err != nil {
		t.Fatalf("UpdateModelGroup: %v", err)
	}
	if upd.GatewayModelName != "coder-v2" || upd.FailoverMode != "climb_up" {
		t.Fatalf("updated = %#v", upd)
	}
	if upd.Traversal != "depth" {
		t.Fatalf("updated traversal = %q, want depth", upd.Traversal)
	}
	if len(upd.Members) != 2 || upd.Members[0].MemberGatewayName != "slow" || upd.Members[1].MemberGatewayName != "fast" {
		t.Fatalf("updated members = %#v", upd.Members)
	}

	// Delete
	if err := svc.DeleteModelGroup(ctx, adminToken(), created.ID); err != nil {
		t.Fatalf("DeleteModelGroup: %v", err)
	}
	if _, err := svc.GetModelGroup(ctx, adminToken(), created.ID); !errors.Is(err, ErrModelGroupNotFound) {
		t.Fatalf("GetModelGroup after delete = %v, want ErrModelGroupNotFound", err)
	}
	if err := svc.DeleteModelGroup(ctx, adminToken(), created.ID); !errors.Is(err, ErrModelGroupNotFound) {
		t.Fatalf("double delete = %v, want ErrModelGroupNotFound", err)
	}
}

func TestModelGroupNameRequiredAndModeInvalid(t *testing.T) {
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	svc, _ := newGroupTestService(t, now, "fast")
	ctx := context.Background()

	if _, err := svc.CreateModelGroup(ctx, adminToken(), CreateModelGroupRequest{GatewayModelName: "   "}); !errors.Is(err, ErrModelGroupNameRequired) {
		t.Fatalf("blank name = %v, want ErrModelGroupNameRequired", err)
	}
	if _, err := svc.CreateModelGroup(ctx, adminToken(), CreateModelGroupRequest{GatewayModelName: "g", FailoverMode: "bogus"}); !errors.Is(err, ErrModelGroupModeInvalid) {
		t.Fatalf("bad mode = %v, want ErrModelGroupModeInvalid", err)
	}
	if _, err := svc.CreateModelGroup(ctx, adminToken(), CreateModelGroupRequest{GatewayModelName: "g", Status: "nope", Members: []GroupMemberInput{{MemberGatewayName: "fast"}}}); !errors.Is(err, ErrMappingStatusInvalid) {
		t.Fatalf("bad status = %v, want ErrMappingStatusInvalid", err)
	}
}

func TestNormalizeTraversal(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", "round_robin"},
		{"  ", "round_robin"},
		{"depth", "depth"},
		{"breadth", "breadth"},
		{"round_robin", "round_robin"},
		{"xyz", "round_robin"},
		{" depth ", "depth"},
	}
	for _, c := range cases {
		if got := normalizeTraversal(c.in); got != c.want {
			t.Errorf("normalizeTraversal(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestModelGroupNameGlobalUniqueness(t *testing.T) {
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	svc, _ := newGroupTestService(t, now, "fast", "slow")
	ctx := context.Background()

	// Conflict against an existing model name (case-insensitive).
	if _, err := svc.CreateModelGroup(ctx, adminToken(), CreateModelGroupRequest{GatewayModelName: "FAST", Members: []GroupMemberInput{{MemberGatewayName: "slow"}}}); !errors.Is(err, ErrModelGroupNameConflict) {
		t.Fatalf("group name == model = %v, want ErrModelGroupNameConflict", err)
	}

	// Create a valid group, then conflict against ANOTHER group's name.
	if _, err := svc.CreateModelGroup(ctx, adminToken(), CreateModelGroupRequest{GatewayModelName: "coder", Members: []GroupMemberInput{{MemberGatewayName: "fast"}}}); err != nil {
		t.Fatalf("create coder: %v", err)
	}
	if _, err := svc.CreateModelGroup(ctx, adminToken(), CreateModelGroupRequest{GatewayModelName: "Coder", Members: []GroupMemberInput{{MemberGatewayName: "slow"}}}); !errors.Is(err, ErrModelGroupNameConflict) {
		t.Fatalf("group name == other group = %v, want ErrModelGroupNameConflict", err)
	}

	// Update that would rename onto a model name conflicts; renaming to its own
	// (excluded) name is allowed.
	g2, err := svc.CreateModelGroup(ctx, adminToken(), CreateModelGroupRequest{GatewayModelName: "helper", Members: []GroupMemberInput{{MemberGatewayName: "fast"}}})
	if err != nil {
		t.Fatalf("create helper: %v", err)
	}
	rename := "slow"
	if _, err := svc.UpdateModelGroup(ctx, adminToken(), g2.ID, UpdateModelGroupRequest{GatewayModelName: &rename}); !errors.Is(err, ErrModelGroupNameConflict) {
		t.Fatalf("update onto model name = %v, want ErrModelGroupNameConflict", err)
	}
	same := "helper"
	if _, err := svc.UpdateModelGroup(ctx, adminToken(), g2.ID, UpdateModelGroupRequest{GatewayModelName: &same}); err != nil {
		t.Fatalf("update to own name should succeed: %v", err)
	}
}

func TestModelGroupMemberValidation(t *testing.T) {
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	svc, _ := newGroupTestService(t, now, "fast", "slow")
	ctx := context.Background()

	// Unknown member (no such model).
	if _, err := svc.CreateModelGroup(ctx, adminToken(), CreateModelGroupRequest{GatewayModelName: "g", Members: []GroupMemberInput{{MemberGatewayName: "ghost"}}}); !errors.Is(err, ErrModelGroupMemberInvalid) {
		t.Fatalf("unknown member = %v, want ErrModelGroupMemberInvalid", err)
	}
	// Blank member.
	if _, err := svc.CreateModelGroup(ctx, adminToken(), CreateModelGroupRequest{GatewayModelName: "g", Members: []GroupMemberInput{{MemberGatewayName: "  "}}}); !errors.Is(err, ErrModelGroupMemberInvalid) {
		t.Fatalf("blank member = %v, want ErrModelGroupMemberInvalid", err)
	}

	// A group name IS now a valid member (nested groups — Phase 1).
	if _, err := svc.CreateModelGroup(ctx, adminToken(), CreateModelGroupRequest{GatewayModelName: "grp1", Members: []GroupMemberInput{{MemberGatewayName: "fast"}}}); err != nil {
		t.Fatalf("create grp1: %v", err)
	}
	if _, err := svc.CreateModelGroup(ctx, adminToken(), CreateModelGroupRequest{GatewayModelName: "grp2", Members: []GroupMemberInput{{MemberGatewayName: "grp1"}}}); err != nil {
		t.Fatalf("group-as-member should be accepted: %v", err)
	}
}

// TestModelGroupMemberAcceptsAnotherGroup proves a member that is another EXISTING
// group is accepted on both create and update (nested groups, Phase 1).
func TestModelGroupMemberAcceptsAnotherGroup(t *testing.T) {
	now := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
	svc, _ := newGroupTestService(t, now, "fast", "slow")
	ctx := context.Background()

	if _, err := svc.CreateModelGroup(ctx, adminToken(), CreateModelGroupRequest{GatewayModelName: "sub", Members: []GroupMemberInput{{MemberGatewayName: "fast"}}}); err != nil {
		t.Fatalf("create sub: %v", err)
	}
	// Create: a group whose member is another group.
	top, err := svc.CreateModelGroup(ctx, adminToken(), CreateModelGroupRequest{GatewayModelName: "top", Members: []GroupMemberInput{{MemberGatewayName: "sub"}}})
	if err != nil {
		t.Fatalf("create top with group member: %v", err)
	}
	if len(top.Members) != 1 || top.Members[0].MemberGatewayName != "sub" {
		t.Fatalf("top members = %#v", top.Members)
	}

	// Update: another group's members updated to include a group name.
	another, err := svc.CreateModelGroup(ctx, adminToken(), CreateModelGroupRequest{GatewayModelName: "another", Members: []GroupMemberInput{{MemberGatewayName: "slow"}}})
	if err != nil {
		t.Fatalf("create another: %v", err)
	}
	newMembers := []GroupMemberInput{{MemberGatewayName: "sub"}, {MemberGatewayName: "slow"}}
	upd, err := svc.UpdateModelGroup(ctx, adminToken(), another.ID, UpdateModelGroupRequest{Members: &newMembers})
	if err != nil {
		t.Fatalf("update with group member: %v", err)
	}
	if len(upd.Members) != 2 || upd.Members[0].MemberGatewayName != "sub" {
		t.Fatalf("updated members = %#v", upd.Members)
	}
}

// TestModelGroupCycleDirectSelfReference proves a group cannot list itself as a
// member (direct self-reference). A true self-reference is only reachable via
// UPDATE: on CREATE the group's own name doesn't exist yet, so a member equal to
// the not-yet-created name is simply an unknown member (ErrModelGroupMemberInvalid,
// verified separately in TestModelGroupMemberStillRejectsUnknown-style cases) — it
// never reaches the cycle check.
func TestModelGroupCycleDirectSelfReference(t *testing.T) {
	now := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
	svc, _ := newGroupTestService(t, now, "fast")
	ctx := context.Background()

	g, err := svc.CreateModelGroup(ctx, adminToken(), CreateModelGroupRequest{GatewayModelName: "g", Members: []GroupMemberInput{{MemberGatewayName: "fast"}}})
	if err != nil {
		t.Fatalf("create g: %v", err)
	}

	// Update: G's members updated to include "g" itself.
	selfMembers := []GroupMemberInput{{MemberGatewayName: "g"}}
	if _, err := svc.UpdateModelGroup(ctx, adminToken(), g.ID, UpdateModelGroupRequest{Members: &selfMembers}); !errors.Is(err, ErrModelGroupCycle) {
		t.Fatalf("self-reference on update = %v, want ErrModelGroupCycle", err)
	}
	// The group's members must be unchanged (rejected before persisting).
	got, err := svc.GetModelGroup(ctx, adminToken(), g.ID)
	if err != nil {
		t.Fatalf("GetModelGroup: %v", err)
	}
	if len(got.Members) != 1 || got.Members[0].MemberGatewayName != "fast" {
		t.Fatalf("members after rejected self-ref = %#v, want unchanged [fast]", got.Members)
	}
}

// TestModelGroupCycleIndirect proves an indirect cycle (G contains H, then updating
// H to contain G) is rejected.
func TestModelGroupCycleIndirect(t *testing.T) {
	now := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
	svc, _ := newGroupTestService(t, now, "fast", "slow")
	ctx := context.Background()

	g, err := svc.CreateModelGroup(ctx, adminToken(), CreateModelGroupRequest{GatewayModelName: "g", Members: []GroupMemberInput{{MemberGatewayName: "fast"}}})
	if err != nil {
		t.Fatalf("create g: %v", err)
	}
	h, err := svc.CreateModelGroup(ctx, adminToken(), CreateModelGroupRequest{GatewayModelName: "h", Members: []GroupMemberInput{{MemberGatewayName: "g"}}})
	if err != nil {
		t.Fatalf("create h with member g: %v", err)
	}

	// Now update G to include H as a member → G -> H -> G, an indirect cycle.
	members := []GroupMemberInput{{MemberGatewayName: "fast"}, {MemberGatewayName: "h"}}
	if _, err := svc.UpdateModelGroup(ctx, adminToken(), g.ID, UpdateModelGroupRequest{Members: &members}); !errors.Is(err, ErrModelGroupCycle) {
		t.Fatalf("indirect cycle = %v, want ErrModelGroupCycle", err)
	}
	// G's members must be unchanged.
	got, err := svc.GetModelGroup(ctx, adminToken(), g.ID)
	if err != nil {
		t.Fatalf("GetModelGroup: %v", err)
	}
	if len(got.Members) != 1 || got.Members[0].MemberGatewayName != "fast" {
		t.Fatalf("g members after rejected indirect cycle = %#v, want unchanged [fast]", got.Members)
	}
	// Sanity: h is untouched too.
	gotH, err := svc.GetModelGroup(ctx, adminToken(), h.ID)
	if err != nil {
		t.Fatalf("GetModelGroup h: %v", err)
	}
	if len(gotH.Members) != 1 || gotH.Members[0].MemberGatewayName != "g" {
		t.Fatalf("h members = %#v, want unchanged [g]", gotH.Members)
	}
}

// TestModelGroupMemberStillRejectsUnknown proves a member that is neither a model
// nor a group is still ErrModelGroupMemberInvalid (the relaxation only adds groups
// as an alternative to models, it doesn't accept arbitrary names).
func TestModelGroupMemberStillRejectsUnknown(t *testing.T) {
	now := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
	svc, _ := newGroupTestService(t, now, "fast")
	ctx := context.Background()

	if _, err := svc.CreateModelGroup(ctx, adminToken(), CreateModelGroupRequest{GatewayModelName: "g", Members: []GroupMemberInput{{MemberGatewayName: "totally-unknown"}}}); !errors.Is(err, ErrModelGroupMemberInvalid) {
		t.Fatalf("unknown member = %v, want ErrModelGroupMemberInvalid", err)
	}
}

func TestModelGroupMemberPriorityAndDedup(t *testing.T) {
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	svc, _ := newGroupTestService(t, now, "a", "b", "c")
	ctx := context.Background()

	created, err := svc.CreateModelGroup(ctx, adminToken(), CreateModelGroupRequest{
		GatewayModelName: "g",
		// duplicate "a" (case-insensitive) should be dropped, keep first
		Members: []GroupMemberInput{{MemberGatewayName: "a"}, {MemberGatewayName: "b"}, {MemberGatewayName: "A"}, {MemberGatewayName: "c"}},
	})
	if err != nil {
		t.Fatalf("CreateModelGroup: %v", err)
	}
	if len(created.Members) != 3 {
		t.Fatalf("dedup failed, members = %#v", created.Members)
	}
	want := []string{"a", "b", "c"}
	for i, m := range created.Members {
		if m.MemberGatewayName != want[i] {
			t.Fatalf("member[%d] = %q, want %q (order = priority)", i, m.MemberGatewayName, want[i])
		}
	}
	// Priorities are the slice index in the store.
	members, err := svc.routes.GroupMembersByGroup(ctx, created.ID)
	if err != nil {
		t.Fatalf("GroupMembersByGroup: %v", err)
	}
	for i, m := range members {
		if m.Priority != i {
			t.Fatalf("member %q priority = %d, want %d", m.MemberGatewayName, m.Priority, i)
		}
	}
}

func TestSetModelVisibilityUpsertAndSurface(t *testing.T) {
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	svc, _ := newGroupTestService(t, now, "shown-model", "hidden-model")
	ctx := context.Background()

	// Default: no setting → "shown".
	if v := modelVisibility(t, svc, "shown-model"); v != "shown" {
		t.Fatalf("default visibility = %q, want shown", v)
	}

	// Set hidden, then update to locked (upsert-by-name).
	if err := svc.SetModelVisibility(ctx, adminToken(), "hidden-model", "hidden"); err != nil {
		t.Fatalf("SetModelVisibility hidden: %v", err)
	}
	if v := modelVisibility(t, svc, "hidden-model"); v != "hidden" {
		t.Fatalf("visibility = %q, want hidden", v)
	}
	if err := svc.SetModelVisibility(ctx, adminToken(), "hidden-model", "locked"); err != nil {
		t.Fatalf("SetModelVisibility locked: %v", err)
	}
	if v := modelVisibility(t, svc, "hidden-model"); v != "locked" {
		t.Fatalf("visibility after upsert = %q, want locked", v)
	}

	// Invalid visibility rejected.
	if err := svc.SetModelVisibility(ctx, adminToken(), "shown-model", "bogus"); !errors.Is(err, ErrModelVisibilityInvalid) {
		t.Fatalf("bad visibility = %v, want ErrModelVisibilityInvalid", err)
	}
	// Unknown model rejected.
	if err := svc.SetModelVisibility(ctx, adminToken(), "ghost", "hidden"); !errors.Is(err, ErrModelGroupMemberInvalid) {
		t.Fatalf("unknown model = %v, want ErrModelGroupMemberInvalid", err)
	}
}

// modelVisibility reads a model's stored visibility from the source of truth
// (model_settings). It intentionally does NOT read Models(), because Task 5
// suppresses hidden/locked models from the offered listing.
func modelVisibility(t *testing.T, svc *Service, id string) string {
	t.Helper()
	setting, ok, err := svc.routes.ModelSettingByName(context.Background(), id)
	if err != nil {
		t.Fatalf("ModelSettingByName %q: %v", id, err)
	}
	if !ok || setting.Visibility == "" {
		return "shown"
	}
	return setting.Visibility
}

// TestModelGroupVisibilityCRUD proves the group NAME's visibility persists via
// create + update, defaults to "shown", round-trips on Get, and rejects invalid.
func TestModelGroupVisibilityCRUD(t *testing.T) {
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	svc, _ := newGroupTestService(t, now, "fast", "slow")
	ctx := context.Background()

	// Create WITHOUT visibility → default "shown", no setting row written.
	def, err := svc.CreateModelGroup(ctx, adminToken(), CreateModelGroupRequest{
		GatewayModelName: "grp-default",
		Members:          []GroupMemberInput{{MemberGatewayName: "fast"}},
	})
	if err != nil {
		t.Fatalf("create default: %v", err)
	}
	if def.Visibility != "shown" {
		t.Fatalf("default group visibility = %q, want shown", def.Visibility)
	}
	if _, ok, _ := svc.routes.ModelSettingByName(ctx, "grp-default"); ok {
		t.Fatalf("create without visibility should NOT write a setting row")
	}

	// Create WITH visibility=hidden → persisted; round-trips via Get + model_settings.
	created, err := svc.CreateModelGroup(ctx, adminToken(), CreateModelGroupRequest{
		GatewayModelName: "coder",
		Visibility:       "hidden",
		Members:          []GroupMemberInput{{MemberGatewayName: "fast"}, {MemberGatewayName: "slow"}},
	})
	if err != nil {
		t.Fatalf("create hidden: %v", err)
	}
	if created.Visibility != "hidden" {
		t.Fatalf("created visibility = %q, want hidden", created.Visibility)
	}
	if got, err := svc.GetModelGroup(ctx, adminToken(), created.ID); err != nil || got.Visibility != "hidden" {
		t.Fatalf("Get visibility = %q err=%v, want hidden", got.Visibility, err)
	}
	if v := modelVisibility(t, svc, "coder"); v != "hidden" {
		t.Fatalf("stored group setting = %q, want hidden", v)
	}

	// Update visibility → locked.
	locked := "locked"
	upd, err := svc.UpdateModelGroup(ctx, adminToken(), created.ID, UpdateModelGroupRequest{Visibility: &locked})
	if err != nil || upd.Visibility != "locked" {
		t.Fatalf("update visibility = %q err=%v, want locked", upd.Visibility, err)
	}
	if v := modelVisibility(t, svc, "coder"); v != "locked" {
		t.Fatalf("stored group setting after update = %q, want locked", v)
	}

	// Update WITHOUT visibility (nil) leaves it unchanged.
	dn := "Coder Group"
	if _, err := svc.UpdateModelGroup(ctx, adminToken(), created.ID, UpdateModelGroupRequest{DisplayName: &dn}); err != nil {
		t.Fatalf("update no-visibility: %v", err)
	}
	if v := modelVisibility(t, svc, "coder"); v != "locked" {
		t.Fatalf("visibility should be unchanged = %q, want locked", v)
	}

	// Invalid visibility on create + update is rejected.
	if _, err := svc.CreateModelGroup(ctx, adminToken(), CreateModelGroupRequest{GatewayModelName: "bad", Visibility: "bogus", Members: []GroupMemberInput{{MemberGatewayName: "fast"}}}); !errors.Is(err, ErrModelVisibilityInvalid) {
		t.Fatalf("create bad visibility = %v, want ErrModelVisibilityInvalid", err)
	}
	bogus := "bogus"
	if _, err := svc.UpdateModelGroup(ctx, adminToken(), created.ID, UpdateModelGroupRequest{Visibility: &bogus}); !errors.Is(err, ErrModelVisibilityInvalid) {
		t.Fatalf("update bad visibility = %v, want ErrModelVisibilityInvalid", err)
	}
}

// TestSetModelVisibilityAcceptsGroupName proves SetModelVisibility now accepts a
// GROUP name (a group name is a gateway_model_name) and still rejects an unknown one.
func TestSetModelVisibilityAcceptsGroupName(t *testing.T) {
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	svc, _ := newGroupTestService(t, now, "fast")
	ctx := context.Background()
	g, err := svc.CreateModelGroup(ctx, adminToken(), CreateModelGroupRequest{GatewayModelName: "mygroup", Members: []GroupMemberInput{{MemberGatewayName: "fast"}}})
	if err != nil {
		t.Fatalf("create group: %v", err)
	}
	if err := svc.SetModelVisibility(ctx, adminToken(), "mygroup", "locked"); err != nil {
		t.Fatalf("SetModelVisibility(group) = %v, want nil", err)
	}
	if v := modelVisibility(t, svc, "mygroup"); v != "locked" {
		t.Fatalf("group visibility = %q, want locked", v)
	}
	if got, err := svc.GetModelGroup(ctx, adminToken(), g.ID); err != nil || got.Visibility != "locked" {
		t.Fatalf("group DTO visibility = %q err=%v, want locked", got.Visibility, err)
	}
	// A name that is neither a model nor a group is still rejected.
	if err := svc.SetModelVisibility(ctx, adminToken(), "nonexistent", "hidden"); !errors.Is(err, ErrModelGroupMemberInvalid) {
		t.Fatalf("unknown name = %v, want ErrModelGroupMemberInvalid", err)
	}
}

// TestModelGroupDeleteResetsVisibility proves deleting a group best-effort resets
// its name's stale hidden/locked setting to "shown" so a reused name is clean.
func TestModelGroupDeleteResetsVisibility(t *testing.T) {
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	svc, _ := newGroupTestService(t, now, "fast")
	ctx := context.Background()
	g, err := svc.CreateModelGroup(ctx, adminToken(), CreateModelGroupRequest{GatewayModelName: "mygroup", Visibility: "locked", Members: []GroupMemberInput{{MemberGatewayName: "fast"}}})
	if err != nil {
		t.Fatalf("create group: %v", err)
	}
	if v := modelVisibility(t, svc, "mygroup"); v != "locked" {
		t.Fatalf("pre-delete visibility = %q, want locked", v)
	}
	if err := svc.DeleteModelGroup(ctx, adminToken(), g.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if v := modelVisibility(t, svc, "mygroup"); v != "shown" {
		t.Fatalf("post-delete visibility = %q, want shown (reset)", v)
	}
}

func TestMappingCreateBlockedByGroupName(t *testing.T) {
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	svc, appID := newGroupTestService(t, now, "fast")
	ctx := context.Background()

	if _, err := svc.CreateModelGroup(ctx, adminToken(), CreateModelGroupRequest{GatewayModelName: "mygroup", Members: []GroupMemberInput{{MemberGatewayName: "fast"}}}); err != nil {
		t.Fatalf("create group: %v", err)
	}
	// A manual mapping must not take a group name (case-insensitive).
	if _, err := svc.CreateMapping(ctx, systemAdminToken(), appID, CreateMappingRequest{GatewayModelName: "MyGroup", AppModelName: "x"}); !errors.Is(err, ErrMappingGatewayNameConflict) {
		t.Fatalf("create mapping onto group name = %v, want ErrMappingGatewayNameConflict", err)
	}
}

func TestMappingUpdateBlockedByGroupName(t *testing.T) {
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	svc, appID := newGroupTestService(t, now, "fast")
	ctx := context.Background()
	m, err := svc.CreateMapping(ctx, systemAdminToken(), appID, CreateMappingRequest{GatewayModelName: "editable", AppModelName: "up"})
	if err != nil {
		t.Fatalf("create mapping: %v", err)
	}
	if _, err := svc.CreateModelGroup(ctx, adminToken(), CreateModelGroupRequest{GatewayModelName: "grp", Members: []GroupMemberInput{{MemberGatewayName: "fast"}}}); err != nil {
		t.Fatalf("create group: %v", err)
	}
	rename := "GRP"
	if _, err := svc.UpdateMapping(ctx, systemAdminToken(), m.ID, UpdateMappingRequest{GatewayModelName: &rename}); !errors.Is(err, ErrMappingGatewayNameConflict) {
		t.Fatalf("update mapping onto group name = %v, want ErrMappingGatewayNameConflict", err)
	}
}

func TestReconcileDisablesGroupNameCollision(t *testing.T) {
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	// model_sync discovers "collide" (a group name) and "fresh".
	svc, routeStore := newServerTestServiceWithLister(t, now, &fakeLister{models: []string{"collide", "fresh"}})
	server := createTestServer(t, svc, "S", "s.example.test")
	app, err := svc.CreateApplication(context.Background(), systemAdminToken(), server.ID, CreateApplicationRequest{Type: routing.ProviderVLLM, Port: 8000, Scheme: "https"})
	if err != nil {
		t.Fatalf("create app: %v", err)
	}
	// A group named "collide" (its member must be a real model — seed one first).
	if _, err := svc.CreateMapping(context.Background(), systemAdminToken(), app.ID, CreateMappingRequest{GatewayModelName: "member1", AppModelName: "member1"}); err != nil {
		t.Fatalf("seed member: %v", err)
	}
	if _, err := svc.CreateModelGroup(context.Background(), adminToken(), CreateModelGroupRequest{GatewayModelName: "collide", Members: []GroupMemberInput{{MemberGatewayName: "member1"}}}); err != nil {
		t.Fatalf("create group collide: %v", err)
	}

	result, err := svc.SyncApplicationModels(context.Background(), systemAdminToken(), app.ID)
	if err != nil {
		t.Fatalf("SyncApplicationModels: %v", err)
	}
	if result.Added != 1 || result.Conflicted != 1 {
		t.Fatalf("result = %#v, want Added=1 Conflicted=1", result)
	}
	// The discovered "collide" mapping must be disabled (not shadowing the group).
	mappings, err := routeStore.MappingsByApplication(context.Background(), app.ID)
	if err != nil {
		t.Fatalf("MappingsByApplication: %v", err)
	}
	var collide *routing.ModelMapping
	for i := range mappings {
		if mappings[i].GatewayModelName == "collide" {
			collide = &mappings[i]
		}
	}
	if collide == nil {
		t.Fatalf("collide mapping not created")
	}
	if collide.Status != routing.ServerStatusDisabled {
		t.Fatalf("collide mapping status = %q, want disabled", collide.Status)
	}
}

// TestModelGroupRefreshesGroupCache proves a successful write refreshes the wired
// GroupCache (best-effort hook).
func TestModelGroupRefreshesGroupCache(t *testing.T) {
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	svc, _ := newGroupTestService(t, now, "fast")
	cache := &countingGroupCache{}
	svc.groupCache = cache
	ctx := context.Background()

	g, err := svc.CreateModelGroup(ctx, adminToken(), CreateModelGroupRequest{GatewayModelName: "g", Members: []GroupMemberInput{{MemberGatewayName: "fast"}}})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := svc.SetModelVisibility(ctx, adminToken(), "fast", "hidden"); err != nil {
		t.Fatalf("set visibility: %v", err)
	}
	if err := svc.DeleteModelGroup(ctx, adminToken(), g.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if cache.calls != 3 {
		t.Fatalf("RefreshGroups calls = %d, want 3 (create+visibility+delete)", cache.calls)
	}
}

type countingGroupCache struct{ calls int }

func (c *countingGroupCache) RefreshGroups(context.Context) error { c.calls++; return nil }
