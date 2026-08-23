// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"errors"
	"fmt"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/usage"
	"reflect"
	"testing"
	"time"
)

// offeringTime is the fixed clock used by the offering tests.
var offeringTime = time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)

// offerModel seeds a server + application + one active mapping so that the
// gateway model NAME `gateway` (upstream `appModel`) is currently OFFERABLE
// (survives activeMappingViews). Each call uses distinct server/app ids so a
// member's loaded-state can be attributed to a specific server.
func offerModel(t *testing.T, rs *routing.MemoryStore, srvID, srvName, appID string, flavors []string, gateway, appModel string, mappingStatus string) {
	t.Helper()
	ctx := context.Background()
	if err := rs.CreateAIServer(ctx, routing.AIServer{ID: srvID, Name: srvName, Domain: srvID + ".test", Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: offeringTime, UpdatedAt: offeringTime}); err != nil {
		t.Fatalf("CreateAIServer %s: %v", srvID, err)
	}
	if err := rs.CreateApplication(ctx, routing.Application{ID: appID, ServerID: srvID, Type: routing.ProviderVLLM, Port: 8000, Scheme: "https", APIFlavors: flavors, Status: routing.ServerStatusActive, CreatedAt: offeringTime, UpdatedAt: offeringTime}); err != nil {
		t.Fatalf("CreateApplication %s: %v", appID, err)
	}
	if err := rs.CreateMapping(ctx, routing.ModelMapping{ID: appID + "_map", ApplicationID: appID, GatewayModelName: gateway, AppModelName: appModel, Status: mappingStatus, CreatedAt: offeringTime, UpdatedAt: offeringTime}); err != nil {
		t.Fatalf("CreateMapping %s: %v", gateway, err)
	}
}

// offerGroup seeds an active model group with the given members in priority order.
func offerGroup(t *testing.T, rs *routing.MemoryStore, id, name string, members ...string) {
	t.Helper()
	ctx := context.Background()
	if err := rs.CreateModelGroup(ctx, routing.ModelGroup{ID: id, GatewayModelName: name, DisplayName: name, Status: routing.ServerStatusActive, FailoverMode: "sticky", CreatedAt: offeringTime, UpdatedAt: offeringTime}); err != nil {
		t.Fatalf("CreateModelGroup %s: %v", name, err)
	}
	ms := make([]routing.GroupMember, 0, len(members))
	for i, m := range members {
		ms = append(ms, routing.GroupMember{ID: fmt.Sprintf("%s_m%d", id, i), GroupID: id, MemberGatewayName: m, Priority: i, CreatedAt: offeringTime})
	}
	if err := rs.SetGroupMembers(ctx, id, ms); err != nil {
		t.Fatalf("SetGroupMembers %s: %v", id, err)
	}
}

// offerVisibility upserts a model_settings visibility row.
func offerVisibility(t *testing.T, rs *routing.MemoryStore, name, vis string) {
	t.Helper()
	if err := rs.UpsertModelSetting(context.Background(), routing.ModelSetting{GatewayModelName: name, Visibility: vis, CreatedAt: offeringTime, UpdatedAt: offeringTime}); err != nil {
		t.Fatalf("UpsertModelSetting %s: %v", name, err)
	}
}

func offerSvc(rs routing.Store, reader LoadedModelReader) *Service {
	return NewService(ServiceDeps{Usage: usage.NewRecorder(), Routes: rs, LoadedModels: reader, Clock: func() time.Time { return offeringTime }})
}

func modelsByID(resp ModelsResponse) map[string]ModelDTO {
	m := make(map[string]ModelDTO, len(resp.Data))
	for _, d := range resp.Data {
		m[d.ID] = d
	}
	return m
}

func containsString(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// TestModelsOverlayGroupUnionFlavors: a group with two offerable members
// (openai + anthropic) is offered as a synthetic model with the UNION of flavors
// and IsGroup=true, and appears under BOTH flavors.
func TestModelsOverlayGroupUnionFlavors(t *testing.T) {
	ctx := context.Background()
	rs := routing.NewMemoryStore()
	offerModel(t, rs, "srv_o", "OpenAIBox", "app_o", []string{routing.APIFlavorOpenAI}, "m1", "m1-up", routing.ServerStatusActive)
	offerModel(t, rs, "srv_a", "AnthropicBox", "app_a", []string{routing.APIFlavorAnthropic}, "m2", "m2-up", routing.ServerStatusActive)
	offerGroup(t, rs, "grp_1", "coder", "m1", "m2")

	svc := offerSvc(rs, nil)
	byID := modelsByID(svc.Models(ctx, auth.Token{UserID: "usr_1"}))

	grp, ok := byID["coder"]
	if !ok {
		t.Fatalf("group 'coder' missing from Models(): %#v", byID)
	}
	if !grp.IsGroup {
		t.Fatalf("group 'coder' IsGroup=false, want true: %#v", grp)
	}
	if !reflect.DeepEqual(grp.Flavors, []string{routing.APIFlavorAnthropic, routing.APIFlavorOpenAI}) {
		t.Fatalf("group flavors = %#v, want [anthropic openai]", grp.Flavors)
	}
	if grp.Visibility != "shown" {
		t.Fatalf("group visibility = %q, want shown", grp.Visibility)
	}
	// Standalone members remain, and are NOT groups.
	if m1, ok := byID["m1"]; !ok || m1.IsGroup {
		t.Fatalf("m1 = %#v, want present non-group", m1)
	}
	// Per-flavor discovery includes the group under both flavors.
	if got := svc.ModelsForFlavor(ctx, auth.Token{UserID: "usr_1"}, routing.APIFlavorOpenAI); !containsString(got, "coder") {
		t.Fatalf("openai flavor = %#v, want to contain coder", got)
	}
	if got := svc.ModelsForFlavor(ctx, auth.Token{UserID: "usr_1"}, routing.APIFlavorAnthropic); !containsString(got, "coder") {
		t.Fatalf("anthropic flavor = %#v, want to contain coder", got)
	}
}

// TestModelsOverlayGroupLoadedTopMember: group Loaded/LoadedOn reflect ONLY the
// highest-priority OFFERABLE member.
func TestModelsOverlayGroupLoadedTopMember(t *testing.T) {
	ctx := context.Background()

	t.Run("top member loaded", func(t *testing.T) {
		rs := routing.NewMemoryStore()
		offerModel(t, rs, "srv_1", "Box1", "app_1", []string{routing.APIFlavorOpenAI}, "m1", "m1-up", routing.ServerStatusActive)
		offerModel(t, rs, "srv_2", "Box2", "app_2", []string{routing.APIFlavorOpenAI}, "m2", "m2-up", routing.ServerStatusActive)
		offerGroup(t, rs, "grp_1", "coder", "m1", "m2")
		reader := fakeLoadedReader{byKey: map[string][]string{"app_1|srv_1": {"m1-up"}}}
		svc := offerSvc(rs, reader)
		byID := modelsByID(svc.Models(ctx, auth.Token{UserID: "usr_1"}))
		grp := byID["coder"]
		if !grp.Loaded {
			t.Fatalf("group should be loaded (m1 loaded): %#v", grp)
		}
		if !reflect.DeepEqual(grp.LoadedOn, []string{"Box1"}) {
			t.Fatalf("group loaded_on = %#v, want [Box1]", grp.LoadedOn)
		}
	})

	t.Run("only lower-priority member loaded", func(t *testing.T) {
		rs := routing.NewMemoryStore()
		offerModel(t, rs, "srv_1", "Box1", "app_1", []string{routing.APIFlavorOpenAI}, "m1", "m1-up", routing.ServerStatusActive)
		offerModel(t, rs, "srv_2", "Box2", "app_2", []string{routing.APIFlavorOpenAI}, "m2", "m2-up", routing.ServerStatusActive)
		offerGroup(t, rs, "grp_1", "coder", "m1", "m2")
		// m2 (lower priority) is loaded; m1 (top) is NOT.
		reader := fakeLoadedReader{byKey: map[string][]string{"app_2|srv_2": {"m2-up"}}}
		svc := offerSvc(rs, reader)
		byID := modelsByID(svc.Models(ctx, auth.Token{UserID: "usr_1"}))
		grp := byID["coder"]
		if grp.Loaded {
			t.Fatalf("group should NOT be loaded (only lower-priority m2 loaded): %#v", grp)
		}
		if len(grp.LoadedOn) != 0 {
			t.Fatalf("group loaded_on = %#v, want empty", grp.LoadedOn)
		}
	})
}

// TestModelsOverlayVisibilitySuppression: hidden/locked models drop from the
// offered listing, but a hidden model is STILL a full group member (its flavors
// and loaded-state contribute to the group).
func TestModelsOverlayVisibilitySuppression(t *testing.T) {
	ctx := context.Background()
	rs := routing.NewMemoryStore()
	// hiddenModel: top-priority member, openai, hidden, and LOADED.
	offerModel(t, rs, "srv_h", "HiddenBox", "app_h", []string{routing.APIFlavorOpenAI}, "hiddenModel", "hidden-up", routing.ServerStatusActive)
	// shownModel: lower-priority member, anthropic, no setting (default shown).
	offerModel(t, rs, "srv_s", "ShownBox", "app_s", []string{routing.APIFlavorAnthropic}, "shownModel", "shown-up", routing.ServerStatusActive)
	// lockedStandalone: not in any group, locked.
	offerModel(t, rs, "srv_l", "LockedBox", "app_l", []string{routing.APIFlavorOpenAI}, "lockedStandalone", "locked-up", routing.ServerStatusActive)
	offerGroup(t, rs, "grp_1", "coder", "hiddenModel", "shownModel")
	offerVisibility(t, rs, "hiddenModel", "hidden")
	offerVisibility(t, rs, "lockedStandalone", "locked")

	reader := fakeLoadedReader{byKey: map[string][]string{"app_h|srv_h": {"hidden-up"}}}
	svc := offerSvc(rs, reader)
	byID := modelsByID(svc.Models(ctx, auth.Token{UserID: "usr_1"}))

	if _, ok := byID["hiddenModel"]; ok {
		t.Fatalf("hiddenModel should be suppressed from Models(): %#v", byID)
	}
	if _, ok := byID["lockedStandalone"]; ok {
		t.Fatalf("lockedStandalone should be suppressed from Models(): %#v", byID)
	}
	shown, ok := byID["shownModel"]
	if !ok {
		t.Fatalf("shownModel should be present: %#v", byID)
	}
	if shown.Visibility != "shown" {
		t.Fatalf("shownModel visibility = %q, want shown", shown.Visibility)
	}
	// The group is still offered; its flavor union includes the hidden member's
	// openai, and its loaded-state comes from the hidden top-priority member.
	grp, ok := byID["coder"]
	if !ok {
		t.Fatalf("group 'coder' should still be offered despite a hidden member: %#v", byID)
	}
	if !reflect.DeepEqual(grp.Flavors, []string{routing.APIFlavorAnthropic, routing.APIFlavorOpenAI}) {
		t.Fatalf("group flavors = %#v, want [anthropic openai] (hidden member still contributes)", grp.Flavors)
	}
	if !grp.Loaded || !reflect.DeepEqual(grp.LoadedOn, []string{"HiddenBox"}) {
		t.Fatalf("group loaded = %v loaded_on = %#v, want loaded on [HiddenBox] (hidden member still counts)", grp.Loaded, grp.LoadedOn)
	}

	// Per-flavor discovery hides suppressed names but keeps the group.
	openai := svc.ModelsForFlavor(ctx, auth.Token{UserID: "usr_1"}, routing.APIFlavorOpenAI)
	if containsString(openai, "hiddenModel") || containsString(openai, "lockedStandalone") {
		t.Fatalf("openai flavor leaked a suppressed model: %#v", openai)
	}
	if !containsString(openai, "coder") {
		t.Fatalf("openai flavor = %#v, want to contain coder", openai)
	}
	anthropic := svc.ModelsForFlavor(ctx, auth.Token{UserID: "usr_1"}, routing.APIFlavorAnthropic)
	if !containsString(anthropic, "coder") || !containsString(anthropic, "shownModel") {
		t.Fatalf("anthropic flavor = %#v, want coder + shownModel", anthropic)
	}
}

// TestDashboardRouteDataHiddenLockedSuppression: dashboardRouteData() (backing
// GET /api/portal/dashboard's "Live Model Routes" table) drops a hidden or
// locked model's route for a non-admin caller, mirroring
// modelsResponse(suppress=true)'s drop from Models() -- the security fix
// closing the gap the VISIBILITY-SURFACE MATRIX doc-comment (on
// visibleMappingViews in service.go) used to flag as a TODO. A shown model's
// route is unaffected, and an admin sees every route unfiltered (same as
// ManageModels()).
func TestDashboardRouteDataHiddenLockedSuppression(t *testing.T) {
	ctx := context.Background()
	rs := routing.NewMemoryStore()
	offerModel(t, rs, "srv_h_dash", "HiddenDashBox", "app_h_dash", []string{routing.APIFlavorOpenAI}, "hiddenDashModel", "hidden-up", routing.ServerStatusActive)
	offerModel(t, rs, "srv_l_dash", "LockedDashBox", "app_l_dash", []string{routing.APIFlavorOpenAI}, "lockedDashModel", "locked-up", routing.ServerStatusActive)
	offerModel(t, rs, "srv_s_dash", "ShownDashBox", "app_s_dash", []string{routing.APIFlavorOpenAI}, "shownDashModel", "shown-up", routing.ServerStatusActive)
	offerVisibility(t, rs, "hiddenDashModel", "hidden")
	offerVisibility(t, rs, "lockedDashModel", "locked")

	svc := offerSvc(rs, nil)
	nonAdmin := auth.Token{UserID: "usr_plain_dash", Scopes: []string{"gateway:use"}}

	_, routesNonAdmin := svc.dashboardRouteData(ctx, nonAdmin)
	gotNonAdmin := make(map[string]bool, len(routesNonAdmin))
	for _, r := range routesNonAdmin {
		gotNonAdmin[r.Model] = true
	}
	if gotNonAdmin["hiddenDashModel"] {
		t.Fatalf("dashboardRouteData(non-admin) leaked hiddenDashModel: %+v", routesNonAdmin)
	}
	if gotNonAdmin["lockedDashModel"] {
		t.Fatalf("dashboardRouteData(non-admin) leaked lockedDashModel: %+v", routesNonAdmin)
	}
	if !gotNonAdmin["shownDashModel"] {
		t.Fatalf("dashboardRouteData(non-admin) missing shownDashModel: %+v", routesNonAdmin)
	}

	admin := adminToken()
	_, routesAdmin := svc.dashboardRouteData(ctx, admin)
	gotAdmin := make(map[string]bool, len(routesAdmin))
	for _, r := range routesAdmin {
		gotAdmin[r.Model] = true
	}
	if !gotAdmin["hiddenDashModel"] || !gotAdmin["lockedDashModel"] || !gotAdmin["shownDashModel"] {
		t.Fatalf("dashboardRouteData(admin) = %+v, want all three routes unfiltered", routesAdmin)
	}
}

// TestManageModelsUnsuppressed: the admin management listing (ManageModels)
// INCLUDES hidden/locked models — each carrying its true Visibility — plus the
// active group with IsGroup, while Models() still suppresses them (regression).
func TestManageModelsUnsuppressed(t *testing.T) {
	ctx := context.Background()
	rs := routing.NewMemoryStore()
	// hiddenModel: top-priority member, openai, hidden, and LOADED.
	offerModel(t, rs, "srv_h", "HiddenBox", "app_h", []string{routing.APIFlavorOpenAI}, "hiddenModel", "hidden-up", routing.ServerStatusActive)
	// shownModel: lower-priority member, anthropic, no setting (default shown).
	offerModel(t, rs, "srv_s", "ShownBox", "app_s", []string{routing.APIFlavorAnthropic}, "shownModel", "shown-up", routing.ServerStatusActive)
	// lockedStandalone: not in any group, locked.
	offerModel(t, rs, "srv_l", "LockedBox", "app_l", []string{routing.APIFlavorOpenAI}, "lockedStandalone", "locked-up", routing.ServerStatusActive)
	offerGroup(t, rs, "grp_1", "coder", "hiddenModel", "shownModel")
	offerVisibility(t, rs, "hiddenModel", "hidden")
	offerVisibility(t, rs, "lockedStandalone", "locked")

	reader := fakeLoadedReader{byKey: map[string][]string{"app_h|srv_h": {"hidden-up"}}}
	svc := offerSvc(rs, reader)
	token := auth.Token{UserID: "usr_1"}

	// Models() (the inference/chat path) SUPPRESSES — regression guard.
	suppressed := modelsByID(svc.Models(ctx, token))
	if _, ok := suppressed["hiddenModel"]; ok {
		t.Fatalf("Models() must still suppress hiddenModel: %#v", suppressed)
	}
	if _, ok := suppressed["lockedStandalone"]; ok {
		t.Fatalf("Models() must still suppress lockedStandalone: %#v", suppressed)
	}

	// ManageModels() (the admin management path) INCLUDES them with visibility.
	manage := modelsByID(svc.ManageModels(ctx, token))
	hidden, ok := manage["hiddenModel"]
	if !ok {
		t.Fatalf("ManageModels must include hiddenModel: %#v", manage)
	}
	if hidden.Visibility != "hidden" {
		t.Fatalf("hiddenModel visibility = %q, want hidden", hidden.Visibility)
	}
	// Retained (not suppressed) → its loaded-state is preserved.
	if !hidden.Loaded || !reflect.DeepEqual(hidden.LoadedOn, []string{"HiddenBox"}) {
		t.Fatalf("hiddenModel loaded = %v loaded_on = %#v, want loaded on [HiddenBox]", hidden.Loaded, hidden.LoadedOn)
	}
	locked, ok := manage["lockedStandalone"]
	if !ok {
		t.Fatalf("ManageModels must include lockedStandalone: %#v", manage)
	}
	if locked.Visibility != "locked" {
		t.Fatalf("lockedStandalone visibility = %q, want locked", locked.Visibility)
	}
	if locked.IsGroup {
		t.Fatalf("lockedStandalone should not be a group: %#v", locked)
	}
	// The shown standalone member is present with default visibility.
	if shown, ok := manage["shownModel"]; !ok || shown.Visibility != "shown" {
		t.Fatalf("shownModel = %#v, want present shown", shown)
	}
	// The active group is still offered in the management listing, with IsGroup.
	grp, ok := manage["coder"]
	if !ok || !grp.IsGroup {
		t.Fatalf("group 'coder' = %#v, want present IsGroup", grp)
	}
}

// TestModelsOverlayGroupVisibility: a GROUP's own visibility (shown/hidden/locked)
// controls whether the group NAME is offered, exactly like a model — a hidden/locked
// group is dropped from Models()/ModelsForFlavor but present in ManageModels with its
// true Visibility; a hidden/locked group's still-shown members stay offered under
// their own names.
func TestModelsOverlayGroupVisibility(t *testing.T) {
	ctx := context.Background()
	token := auth.Token{UserID: "usr_1"}

	seed := func(t *testing.T) *routing.MemoryStore {
		rs := routing.NewMemoryStore()
		offerModel(t, rs, "srv_1", "Box1", "app_1", []string{routing.APIFlavorOpenAI}, "m1", "m1-up", routing.ServerStatusActive)
		offerModel(t, rs, "srv_2", "Box2", "app_2", []string{routing.APIFlavorOpenAI}, "m2", "m2-up", routing.ServerStatusActive)
		offerGroup(t, rs, "grp_1", "coder-group", "m1", "m2")
		return rs
	}

	t.Run("shown group offered", func(t *testing.T) {
		rs := seed(t)
		svc := offerSvc(rs, nil)
		byID := modelsByID(svc.Models(ctx, token))
		grp, ok := byID["coder-group"]
		if !ok || !grp.IsGroup || grp.Visibility != "shown" {
			t.Fatalf("shown group = %#v, want present IsGroup shown", grp)
		}
		if !containsString(svc.ModelsForFlavor(ctx, auth.Token{UserID: "usr_1"}, routing.APIFlavorOpenAI), "coder-group") {
			t.Fatalf("shown group missing from flavor discovery")
		}
	})

	for _, vis := range []string{"hidden", "locked"} {
		vis := vis
		t.Run(vis+" group dropped from Models but in ManageModels", func(t *testing.T) {
			rs := seed(t)
			offerVisibility(t, rs, "coder-group", vis)
			svc := offerSvc(rs, nil)

			// Absent from the offered (inference/chat) listing + flavor discovery.
			byID := modelsByID(svc.Models(ctx, token))
			if _, ok := byID["coder-group"]; ok {
				t.Fatalf("%s group must be dropped from Models(): %#v", vis, byID)
			}
			if containsString(svc.ModelsForFlavor(ctx, auth.Token{UserID: "usr_1"}, routing.APIFlavorOpenAI), "coder-group") {
				t.Fatalf("%s group must not appear in flavor discovery", vis)
			}
			// Its still-shown members remain offered under their own names (a
			// hidden/locked GROUP does not affect its members' own visibility).
			if _, ok := byID["m1"]; !ok {
				t.Fatalf("%s group's shown member m1 must remain offered: %#v", vis, byID)
			}
			if _, ok := byID["m2"]; !ok {
				t.Fatalf("%s group's shown member m2 must remain offered: %#v", vis, byID)
			}
			// Present in the admin management listing WITH its true visibility.
			manage := modelsByID(svc.ManageModels(ctx, token))
			grp, ok := manage["coder-group"]
			if !ok || !grp.IsGroup || grp.Visibility != vis {
				t.Fatalf("ManageModels %s group = %#v, want present IsGroup %s", vis, grp, vis)
			}
		})
	}
}

// TestModelsOverlayGroupUnionsNestedFlavors: a group G whose ONLY member is a
// subgroup H is itself offered with the UNION of H's flattened (nested) leaf
// members' flavors, and its ordered offerable members are the flattened
// leaves in H's priority order — verified via the group's loaded-state, which
// tracks the highest-priority OFFERABLE member (Task 5, offering overlay
// flattening).
func TestModelsOverlayGroupUnionsNestedFlavors(t *testing.T) {
	ctx := context.Background()
	rs := routing.NewMemoryStore()
	offerModel(t, rs, "srv_1", "Box1", "app_1", []string{routing.APIFlavorOpenAI}, "m1", "m1-up", routing.ServerStatusActive)
	offerModel(t, rs, "srv_2", "Box2", "app_2", []string{routing.APIFlavorAnthropic}, "m2", "m2-up", routing.ServerStatusActive)
	// H (sub-h) is a subgroup over m1, m2 (m1 highest priority).
	offerGroup(t, rs, "grp_h", "sub-h", "m1", "m2")
	// G's (top-g) ONLY member is the subgroup H.
	offerGroup(t, rs, "grp_g", "top-g", "sub-h")

	t.Run("union flavors of the flattened nested members", func(t *testing.T) {
		svc := offerSvc(rs, nil)
		byID := modelsByID(svc.Models(ctx, auth.Token{UserID: "usr_1"}))

		// The subgroup itself is ALSO offered standalone (unchanged — group
		// offering is independent per group).
		if _, ok := byID["sub-h"]; !ok {
			t.Fatalf("subgroup 'sub-h' should be offered standalone: %#v", byID)
		}

		grp, ok := byID["top-g"]
		if !ok {
			t.Fatalf("group 'top-g' (nested over sub-h) missing from Models(): %#v", byID)
		}
		if !grp.IsGroup {
			t.Fatalf("group 'top-g' IsGroup=false, want true: %#v", grp)
		}
		// UNION of the FLATTENED (nested) leaf members' flavors: m1=openai, m2=anthropic.
		if !reflect.DeepEqual(grp.Flavors, []string{routing.APIFlavorAnthropic, routing.APIFlavorOpenAI}) {
			t.Fatalf("nested group flavors = %#v, want [anthropic openai] (union of flattened leaves)", grp.Flavors)
		}
	})

	t.Run("loaded state follows the flattened leaf priority order (top leaf loaded)", func(t *testing.T) {
		// m1 is the flattened top-priority leaf (sub-h's first member, and top-g's
		// only member expands to sub-h's list) — loading m1 loads the nested group.
		reader := fakeLoadedReader{byKey: map[string][]string{"app_1|srv_1": {"m1-up"}}}
		svc := offerSvc(rs, reader)
		byID := modelsByID(svc.Models(ctx, auth.Token{UserID: "usr_1"}))
		grp := byID["top-g"]
		if !grp.Loaded || !reflect.DeepEqual(grp.LoadedOn, []string{"Box1"}) {
			t.Fatalf("nested group loaded = %v loaded_on = %#v, want loaded on [Box1] (flattened leaf m1 is top priority)", grp.Loaded, grp.LoadedOn)
		}
	})

	t.Run("only lower-priority flattened leaf loaded -> group not loaded", func(t *testing.T) {
		reader := fakeLoadedReader{byKey: map[string][]string{"app_2|srv_2": {"m2-up"}}}
		svc := offerSvc(rs, reader)
		byID := modelsByID(svc.Models(ctx, auth.Token{UserID: "usr_1"}))
		grp := byID["top-g"]
		if grp.Loaded {
			t.Fatalf("nested group should NOT be loaded (only lower-priority flattened leaf m2 loaded): %#v", grp)
		}
	})
}

// TestModelsOverlayGroupNoOfferableMembers: a group whose only member has an
// inactive mapping (not offerable) is NOT offered.
func TestModelsOverlayGroupNoOfferableMembers(t *testing.T) {
	ctx := context.Background()
	rs := routing.NewMemoryStore()
	// Member 'dead' has a DISABLED mapping → not in activeMappingViews.
	offerModel(t, rs, "srv_d", "DeadBox", "app_d", []string{routing.APIFlavorOpenAI}, "dead", "dead-up", routing.ServerStatusDisabled)
	offerGroup(t, rs, "grp_1", "coder", "dead")

	svc := offerSvc(rs, nil)
	byID := modelsByID(svc.Models(ctx, auth.Token{UserID: "usr_1"}))
	if _, ok := byID["coder"]; ok {
		t.Fatalf("group with no offerable member should NOT be offered: %#v", byID)
	}
	if containsString(svc.ModelsForFlavor(ctx, auth.Token{UserID: "usr_1"}, routing.APIFlavorOpenAI), "coder") {
		t.Fatalf("group with no offerable member leaked into flavor discovery")
	}
}

// groupErrStore wraps a MemoryStore but makes ModelGroups error, simulating an
// overlay store failure. All other methods are promoted from the embedded store.
type groupErrStore struct {
	*routing.MemoryStore
}

func (groupErrStore) ModelGroups(context.Context) ([]routing.ModelGroup, error) {
	return nil, errors.New("boom")
}

// TestModelsOverlayFailOpen: when the group/settings read errors, Models() and
// ModelsForFlavor() fall back to today's behavior — every standalone model is
// returned, with NO group additions and NO visibility suppression.
func TestModelsOverlayFailOpen(t *testing.T) {
	ctx := context.Background()
	rs := routing.NewMemoryStore()
	offerModel(t, rs, "srv_x", "BoxX", "app_x", []string{routing.APIFlavorOpenAI}, "wouldHide", "hide-up", routing.ServerStatusActive)
	offerGroup(t, rs, "grp_1", "coder", "wouldHide")
	offerVisibility(t, rs, "wouldHide", "hidden")

	svc := offerSvc(groupErrStore{rs}, nil)
	byID := modelsByID(svc.Models(ctx, auth.Token{UserID: "usr_1"}))

	// Overlay failed → the "hidden" model is NOT suppressed (fail open).
	if _, ok := byID["wouldHide"]; !ok {
		t.Fatalf("fail-open: standalone model must still be offered: %#v", byID)
	}
	// Overlay failed → no group entry added.
	if _, ok := byID["coder"]; ok {
		t.Fatalf("fail-open: no group should be added on overlay error: %#v", byID)
	}
	if containsString(svc.ModelsForFlavor(ctx, auth.Token{UserID: "usr_1"}, routing.APIFlavorOpenAI), "coder") {
		t.Fatalf("fail-open: no group in flavor discovery on overlay error")
	}
	if !containsString(svc.ModelsForFlavor(ctx, auth.Token{UserID: "usr_1"}, routing.APIFlavorOpenAI), "wouldHide") {
		t.Fatalf("fail-open: standalone model must remain in flavor discovery")
	}
}

// TestModelsOverlayNoGroupsNoOp: with no groups and no settings the overlay is a
// pure no-op — Models() returns exactly the standalone models (IsGroup unset,
// default visibility).
func TestModelsOverlayNoGroupsNoOp(t *testing.T) {
	ctx := context.Background()
	rs := routing.NewMemoryStore()
	offerModel(t, rs, "srv_1", "Box1", "app_1", []string{routing.APIFlavorOpenAI}, "solo", "solo-up", routing.ServerStatusActive)
	svc := offerSvc(rs, nil)
	resp := svc.Models(ctx, auth.Token{UserID: "usr_1"})
	if len(resp.Data) != 1 {
		t.Fatalf("models = %#v, want single 'solo'", resp.Data)
	}
	m := resp.Data[0]
	if m.ID != "solo" || m.IsGroup || m.Visibility != "shown" {
		t.Fatalf("model = %#v, want solo non-group shown", m)
	}
}
