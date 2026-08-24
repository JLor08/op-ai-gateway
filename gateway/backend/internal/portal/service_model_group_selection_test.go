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

// TestModelGroupSelectionSettingsDefaults proves a group created without any
// of the four selection settings reads back the documented no-op defaults --
// including climb_speed_margin_percent, which must be the SHIPPED default
// (routing.DefaultClimbSpeedMarginPercent), not zero.
func TestModelGroupSelectionSettingsDefaults(t *testing.T) {
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	svc, _ := newGroupTestService(t, now, "fast")
	ctx := context.Background()

	created, err := svc.CreateModelGroup(ctx, adminToken(), CreateModelGroupRequest{
		GatewayModelName: "g",
		Members:          []GroupMemberInput{{MemberGatewayName: "fast"}},
	})
	if err != nil {
		t.Fatalf("CreateModelGroup: %v", err)
	}
	if created.LoadedOnly {
		t.Fatalf("default loaded_only = true, want false")
	}
	if created.MemberOrder != routing.MemberOrderPriority {
		t.Fatalf("default member_order = %q, want priority", created.MemberOrder)
	}
	if created.ClimbSpeedMarginPercent != routing.DefaultClimbSpeedMarginPercent {
		t.Fatalf("default climb_speed_margin_percent = %d, want shipped default %d", created.ClimbSpeedMarginPercent, routing.DefaultClimbSpeedMarginPercent)
	}
	if created.MinTokensPerSecond != 0 {
		t.Fatalf("default min_tokens_per_second = %v, want 0", created.MinTokensPerSecond)
	}
	if created.MinSpeedFallback != routing.MinSpeedFallbackError {
		t.Fatalf("default min_speed_fallback = %q, want error", created.MinSpeedFallback)
	}
}

// TestModelGroupSelectionSettingsRoundTrip proves all five fields round-trip
// through create, get, and update.
func TestModelGroupSelectionSettingsRoundTrip(t *testing.T) {
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	svc, _ := newGroupTestService(t, now, "fast", "slow")
	ctx := context.Background()

	margin := 35
	created, err := svc.CreateModelGroup(ctx, adminToken(), CreateModelGroupRequest{
		GatewayModelName:        "coder",
		Members:                 []GroupMemberInput{{MemberGatewayName: "fast"}, {MemberGatewayName: "slow"}},
		LoadedOnly:              true,
		MemberOrder:             routing.MemberOrderSpeed,
		ClimbSpeedMarginPercent: &margin,
		MinTokensPerSecond:      12.5,
		MinSpeedFallback:        routing.MinSpeedFallbackIgnore,
	})
	if err != nil {
		t.Fatalf("CreateModelGroup: %v", err)
	}
	if !created.LoadedOnly || created.MemberOrder != routing.MemberOrderSpeed ||
		created.ClimbSpeedMarginPercent != 35 || created.MinTokensPerSecond != 12.5 ||
		created.MinSpeedFallback != routing.MinSpeedFallbackIgnore {
		t.Fatalf("created selection settings = %#v", created)
	}

	got, err := svc.GetModelGroup(ctx, adminToken(), created.ID)
	if err != nil {
		t.Fatalf("GetModelGroup: %v", err)
	}
	if !got.LoadedOnly || got.MemberOrder != routing.MemberOrderSpeed ||
		got.ClimbSpeedMarginPercent != 35 || got.MinTokensPerSecond != 12.5 ||
		got.MinSpeedFallback != routing.MinSpeedFallbackIgnore {
		t.Fatalf("get selection settings = %#v", got)
	}

	// Update: flip every setting to its opposite/default.
	loadedOnly := false
	order := routing.MemberOrderPriority
	newMargin := 0
	minTPS := 0.0
	fallback := routing.MinSpeedFallbackError
	upd, err := svc.UpdateModelGroup(ctx, adminToken(), created.ID, UpdateModelGroupRequest{
		LoadedOnly:              &loadedOnly,
		MemberOrder:             &order,
		ClimbSpeedMarginPercent: &newMargin,
		MinTokensPerSecond:      &minTPS,
		MinSpeedFallback:        &fallback,
	})
	if err != nil {
		t.Fatalf("UpdateModelGroup: %v", err)
	}
	if upd.LoadedOnly || upd.MemberOrder != routing.MemberOrderPriority ||
		upd.ClimbSpeedMarginPercent != 0 || upd.MinTokensPerSecond != 0 ||
		upd.MinSpeedFallback != routing.MinSpeedFallbackError {
		t.Fatalf("updated selection settings = %#v", upd)
	}

	// An update that touches none of these fields (nil pointers) leaves them
	// unchanged.
	dn := "Coder Group"
	upd2, err := svc.UpdateModelGroup(ctx, adminToken(), created.ID, UpdateModelGroupRequest{DisplayName: &dn})
	if err != nil {
		t.Fatalf("UpdateModelGroup (no selection fields): %v", err)
	}
	if upd2.LoadedOnly || upd2.MemberOrder != routing.MemberOrderPriority ||
		upd2.ClimbSpeedMarginPercent != 0 || upd2.MinTokensPerSecond != 0 ||
		upd2.MinSpeedFallback != routing.MinSpeedFallbackError {
		t.Fatalf("selection settings after unrelated update = %#v, want unchanged", upd2)
	}
}

// TestModelGroupClimbMarginOmittedVsExplicitZero is the cross-task-risk test:
// DefaultClimbSpeedMarginPercent=20 is applied nowhere except this write
// path, so omitting the field on create must persist 20 and an EXPLICIT 0
// must persist as 0 (0 is a legitimate "no margin required" policy, not an
// unset sentinel -- the store deliberately does not substitute a default for
// exactly this reason).
func TestModelGroupClimbMarginOmittedVsExplicitZero(t *testing.T) {
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	svc, _ := newGroupTestService(t, now, "fast")
	ctx := context.Background()

	// Omitted (nil ClimbSpeedMarginPercent) -> the shipped default, NOT zero.
	omitted, err := svc.CreateModelGroup(ctx, adminToken(), CreateModelGroupRequest{
		GatewayModelName: "omitted",
		Members:          []GroupMemberInput{{MemberGatewayName: "fast"}},
	})
	if err != nil {
		t.Fatalf("create omitted: %v", err)
	}
	if omitted.ClimbSpeedMarginPercent != routing.DefaultClimbSpeedMarginPercent {
		t.Fatalf("omitted margin = %d, want shipped default %d", omitted.ClimbSpeedMarginPercent, routing.DefaultClimbSpeedMarginPercent)
	}
	storedOmitted, err := svc.routes.ModelGroupByID(ctx, omitted.ID)
	if err != nil {
		t.Fatalf("ModelGroupByID(omitted): %v", err)
	}
	if storedOmitted.ClimbSpeedMarginPercent != routing.DefaultClimbSpeedMarginPercent {
		t.Fatalf("stored omitted margin = %d, want shipped default %d", storedOmitted.ClimbSpeedMarginPercent, routing.DefaultClimbSpeedMarginPercent)
	}

	// Explicit 0 -> persisted as 0, not silently replaced by the default.
	zero := 0
	explicit, err := svc.CreateModelGroup(ctx, adminToken(), CreateModelGroupRequest{
		GatewayModelName:        "explicit-zero",
		Members:                 []GroupMemberInput{{MemberGatewayName: "fast"}},
		ClimbSpeedMarginPercent: &zero,
	})
	if err != nil {
		t.Fatalf("create explicit zero: %v", err)
	}
	if explicit.ClimbSpeedMarginPercent != 0 {
		t.Fatalf("explicit-zero margin = %d, want 0", explicit.ClimbSpeedMarginPercent)
	}
	storedExplicit, err := svc.routes.ModelGroupByID(ctx, explicit.ID)
	if err != nil {
		t.Fatalf("ModelGroupByID(explicit-zero): %v", err)
	}
	if storedExplicit.ClimbSpeedMarginPercent != 0 {
		t.Fatalf("stored explicit-zero margin = %d, want 0", storedExplicit.ClimbSpeedMarginPercent)
	}

	// On UPDATE, nil means "leave unchanged" (existing convention) -- it must
	// NOT reset the margin back to the shipped default.
	dn := "renamed"
	unchanged, err := svc.UpdateModelGroup(ctx, adminToken(), explicit.ID, UpdateModelGroupRequest{DisplayName: &dn})
	if err != nil {
		t.Fatalf("update without margin: %v", err)
	}
	if unchanged.ClimbSpeedMarginPercent != 0 {
		t.Fatalf("margin after nil-margin update = %d, want unchanged 0", unchanged.ClimbSpeedMarginPercent)
	}
}

// TestModelGroupSelectionSettingsValidation proves each of the four write-time
// rejections uses the existing portal.*_invalid error shape, on both create
// and update, and that a rejected update leaves the group's settings
// unchanged (validate-before-mutate).
func TestModelGroupSelectionSettingsValidation(t *testing.T) {
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	svc, _ := newGroupTestService(t, now, "fast")
	ctx := context.Background()

	if _, err := svc.CreateModelGroup(ctx, adminToken(), CreateModelGroupRequest{
		GatewayModelName: "bad-order", Members: []GroupMemberInput{{MemberGatewayName: "fast"}}, MemberOrder: "bogus",
	}); !errors.Is(err, ErrModelGroupMemberOrderInvalid) {
		t.Fatalf("create bad member_order = %v, want ErrModelGroupMemberOrderInvalid", err)
	}
	if _, err := svc.CreateModelGroup(ctx, adminToken(), CreateModelGroupRequest{
		GatewayModelName: "bad-fallback", Members: []GroupMemberInput{{MemberGatewayName: "fast"}}, MinSpeedFallback: "bogus",
	}); !errors.Is(err, ErrModelGroupMinSpeedFallbackInvalid) {
		t.Fatalf("create bad min_speed_fallback = %v, want ErrModelGroupMinSpeedFallbackInvalid", err)
	}
	if _, err := svc.CreateModelGroup(ctx, adminToken(), CreateModelGroupRequest{
		GatewayModelName: "bad-tps", Members: []GroupMemberInput{{MemberGatewayName: "fast"}}, MinTokensPerSecond: -1,
	}); !errors.Is(err, ErrModelGroupMinTokensPerSecondInvalid) {
		t.Fatalf("create negative min_tokens_per_second = %v, want ErrModelGroupMinTokensPerSecondInvalid", err)
	}
	negMargin := -5
	if _, err := svc.CreateModelGroup(ctx, adminToken(), CreateModelGroupRequest{
		GatewayModelName: "bad-margin", Members: []GroupMemberInput{{MemberGatewayName: "fast"}}, ClimbSpeedMarginPercent: &negMargin,
	}); !errors.Is(err, ErrModelGroupClimbSpeedMarginInvalid) {
		t.Fatalf("create negative margin = %v, want ErrModelGroupClimbSpeedMarginInvalid", err)
	}

	// Same four rejections on UPDATE, against a freshly created base group.
	base, err := svc.CreateModelGroup(ctx, adminToken(), CreateModelGroupRequest{
		GatewayModelName: "base", Members: []GroupMemberInput{{MemberGatewayName: "fast"}},
	})
	if err != nil {
		t.Fatalf("create base: %v", err)
	}
	bogusOrder := "bogus"
	if _, err := svc.UpdateModelGroup(ctx, adminToken(), base.ID, UpdateModelGroupRequest{MemberOrder: &bogusOrder}); !errors.Is(err, ErrModelGroupMemberOrderInvalid) {
		t.Fatalf("update bad member_order = %v, want ErrModelGroupMemberOrderInvalid", err)
	}
	bogusFallback := "bogus"
	if _, err := svc.UpdateModelGroup(ctx, adminToken(), base.ID, UpdateModelGroupRequest{MinSpeedFallback: &bogusFallback}); !errors.Is(err, ErrModelGroupMinSpeedFallbackInvalid) {
		t.Fatalf("update bad min_speed_fallback = %v, want ErrModelGroupMinSpeedFallbackInvalid", err)
	}
	negTPS := -1.0
	if _, err := svc.UpdateModelGroup(ctx, adminToken(), base.ID, UpdateModelGroupRequest{MinTokensPerSecond: &negTPS}); !errors.Is(err, ErrModelGroupMinTokensPerSecondInvalid) {
		t.Fatalf("update negative min_tokens_per_second = %v, want ErrModelGroupMinTokensPerSecondInvalid", err)
	}
	negMargin2 := -1
	if _, err := svc.UpdateModelGroup(ctx, adminToken(), base.ID, UpdateModelGroupRequest{ClimbSpeedMarginPercent: &negMargin2}); !errors.Is(err, ErrModelGroupClimbSpeedMarginInvalid) {
		t.Fatalf("update negative margin = %v, want ErrModelGroupClimbSpeedMarginInvalid", err)
	}

	// The base group's settings must be unchanged after every rejected update.
	got, err := svc.GetModelGroup(ctx, adminToken(), base.ID)
	if err != nil {
		t.Fatalf("GetModelGroup: %v", err)
	}
	if got.MemberOrder != routing.MemberOrderPriority || got.MinSpeedFallback != routing.MinSpeedFallbackError ||
		got.MinTokensPerSecond != 0 || got.ClimbSpeedMarginPercent != routing.DefaultClimbSpeedMarginPercent {
		t.Fatalf("settings after rejected updates = %#v, want unchanged defaults", got)
	}
}
