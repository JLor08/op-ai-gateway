// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/store"
	"op-ai-gateway/internal/usage"
	"testing"
	"time"
)

// --- Resource Groups Phase 2 -- Task 4: principal-aware model visibility ---
//
// Models()/ModelsForFlavor() (the usage-facing listings) must not surface a
// model exclusively offered by a server the caller is not provisioned to use
// (see AllowedServerIDs, Task 3); ManageModels() (the admin management
// listing) stays UNFILTERED. Extension: the model-server DETAIL/ranking
// surface (ModelServers) must return an EMPTY slice for a restricted model
// to a non-provisioned caller, not just omit it from the top-level list.

// visibilityFixture is server X (RESTRICTED, offers "model-m", a member of a
// provisioned resource group) and server Y (UNRESTRICTED, offers "model-n",
// not a member of any resource group). The resource group RG_VIS hangs off
// system group SG and is provisioned for admin group AG; usr_u is a member of
// AG (provisioned), usr_v is a member of SG only, NOT of AG (not provisioned).
type visibilityFixture struct {
	modelM, modelN string
	srvX, srvY     routing.AIServer
}

func setupVisibilityFixture(e *groupTestEnv) visibilityFixture {
	e.t.Helper()
	e.createUser("usr_admin_vis", "system_admin")
	e.createUser("usr_u", "user")
	e.createUser("usr_v", "user")
	sysAdmin := token("usr_admin_vis", "system", "admin")

	sg := e.mustCreateGroup(sysAdmin, CreateGroupInput{Tier: store.GroupTierSystem, Name: "SG_VIS"})
	e.mustAddMembers(sysAdmin, sg.ID, "usr_u", "usr_v")
	ag := e.mustCreateGroup(sysAdmin, CreateGroupInput{Tier: store.GroupTierAdmin, Name: "AG_VIS", ParentGroupID: sg.ID})
	e.mustAddMembers(sysAdmin, ag.ID, "usr_u") // usr_v deliberately NOT a member of AG

	rg := e.mustCreateResourceGroup(routing.ResourceGroup{
		ID: "rgrp_vis", Name: "RG_VIS", Status: routing.ServerStatusActive,
		SystemGroupID: sg.ID, CreatedAt: e.now, UpdatedAt: e.now,
	})

	srvX := e.mustCreateServer("srv_x_vis", "Server X")
	srvY := e.mustCreateServer("srv_y_vis", "Server Y")
	e.mustLinkResourceGroupServer(rg.ID, srvX.ID)
	e.mustProvisionResourceGroup(rg.ID, routing.ProvisionKindAdminGroup, ag.ID)

	// Server X offers model M (RESTRICTED -- a member of the provisioned RG_VIS).
	if err := e.routes.CreateApplication(e.ctx, routing.Application{
		ID: "app_x_vis", ServerID: srvX.ID, Type: routing.ProviderVLLM, Port: 8000, Scheme: "https",
		APIFlavors: []string{routing.APIFlavorOpenAI}, Status: routing.ServerStatusActive,
		CreatedAt: e.now, UpdatedAt: e.now,
	}); err != nil {
		e.t.Fatalf("CreateApplication X: %v", err)
	}
	if err := e.routes.CreateMapping(e.ctx, routing.ModelMapping{
		ID: "map_x_vis", ApplicationID: "app_x_vis", GatewayModelName: "model-m", AppModelName: "model-m",
		Status: routing.ServerStatusActive, CreatedAt: e.now, UpdatedAt: e.now,
	}); err != nil {
		e.t.Fatalf("CreateMapping X: %v", err)
	}

	// Server Y offers model N (UNRESTRICTED -- not a member of ANY resource group).
	if err := e.routes.CreateApplication(e.ctx, routing.Application{
		ID: "app_y_vis", ServerID: srvY.ID, Type: routing.ProviderVLLM, Port: 8000, Scheme: "https",
		APIFlavors: []string{routing.APIFlavorOpenAI}, Status: routing.ServerStatusActive,
		CreatedAt: e.now, UpdatedAt: e.now,
	}); err != nil {
		e.t.Fatalf("CreateApplication Y: %v", err)
	}
	if err := e.routes.CreateMapping(e.ctx, routing.ModelMapping{
		ID: "map_y_vis", ApplicationID: "app_y_vis", GatewayModelName: "model-n", AppModelName: "model-n",
		Status: routing.ServerStatusActive, CreatedAt: e.now, UpdatedAt: e.now,
	}); err != nil {
		e.t.Fatalf("CreateMapping Y: %v", err)
	}

	return visibilityFixture{modelM: "model-m", modelN: "model-n", srvX: srvX, srvY: srvY}
}

// TestModelsVisibilityProvisioningFilter: Models() drops a model exclusively
// offered by a restricted server the caller is not provisioned to use, but
// keeps an unrestricted model; ManageModels() (the admin management path)
// always returns BOTH regardless of caller, unfiltered.
func TestModelsVisibilityProvisioningFilter(t *testing.T) {
	e := newGroupTestEnv(t)
	f := setupVisibilityFixture(e)
	tokenU := token("usr_u")
	tokenV := token("usr_v")
	admin := token("usr_admin_vis", "system", "admin")

	byIDU := modelIDs(e.svc.Models(e.ctx, tokenU))
	if !byIDU[f.modelM] {
		t.Fatalf("Models(usr_u) missing %s, want present (provisioned via admin group AG)", f.modelM)
	}
	if !byIDU[f.modelN] {
		t.Fatalf("Models(usr_u) missing %s, want present (unrestricted)", f.modelN)
	}

	byIDV := modelIDs(e.svc.Models(e.ctx, tokenV))
	if byIDV[f.modelM] {
		t.Fatalf("Models(usr_v) includes %s, want excluded (not provisioned into RG_VIS)", f.modelM)
	}
	if !byIDV[f.modelN] {
		t.Fatalf("Models(usr_v) missing %s, want present (unrestricted)", f.modelN)
	}

	// ManageModels (admin, management surface) is UNFILTERED -- both present
	// regardless of the admin's own provisioning state.
	manage := modelIDs(e.svc.ManageModels(e.ctx, admin))
	if !manage[f.modelM] || !manage[f.modelN] {
		t.Fatalf("ManageModels(admin) = %v, want both %s and %s present (unfiltered)", manage, f.modelM, f.modelN)
	}

	// A non-provisioned caller's own ManageModels call (were it ever routed
	// through the admin surface) is likewise unfiltered -- the split is by
	// CALLER SURFACE (suppress flag), never by the caller's own provisioning.
	manageV := modelIDs(e.svc.ManageModels(e.ctx, tokenV))
	if !manageV[f.modelM] || !manageV[f.modelN] {
		t.Fatalf("ManageModels(usr_v) = %v, want both present (unfiltered regardless of caller)", manageV)
	}
}

// TestModelsForFlavorVisibilityProvisioningFilter: the per-flavor discovery
// list (ModelsForFlavor, the source for /v1/models et al.) applies the exact
// same filter as Models().
func TestModelsForFlavorVisibilityProvisioningFilter(t *testing.T) {
	e := newGroupTestEnv(t)
	f := setupVisibilityFixture(e)
	tokenU := token("usr_u")
	tokenV := token("usr_v")

	gotV := e.svc.ModelsForFlavor(e.ctx, tokenV, routing.APIFlavorOpenAI)
	if containsString(gotV, f.modelM) {
		t.Fatalf("ModelsForFlavor(usr_v) = %v, want %s excluded (not provisioned)", gotV, f.modelM)
	}
	if !containsString(gotV, f.modelN) {
		t.Fatalf("ModelsForFlavor(usr_v) = %v, want %s present (unrestricted)", gotV, f.modelN)
	}

	gotU := e.svc.ModelsForFlavor(e.ctx, tokenU, routing.APIFlavorOpenAI)
	if !containsString(gotU, f.modelM) {
		t.Fatalf("ModelsForFlavor(usr_u) = %v, want %s present (provisioned)", gotU, f.modelM)
	}
	if !containsString(gotU, f.modelN) {
		t.Fatalf("ModelsForFlavor(usr_u) = %v, want %s present (unrestricted)", gotU, f.modelN)
	}
}

// TestModelServersVisibilityProvisioningFilter: the model-server DETAIL
// surface (ModelServers, backing GET /api/portal/model-servers + its SSE)
// returns an EMPTY slice for a restricted model to a non-provisioned caller
// -- not merely omitted from the top-level Models() list, but genuinely
// empty even for a direct, by-name lookup -- and a non-empty slice for a
// provisioned caller.
func TestModelServersVisibilityProvisioningFilter(t *testing.T) {
	e := newGroupTestEnv(t)
	f := setupVisibilityFixture(e)
	tokenU := token("usr_u")
	tokenV := token("usr_v")

	rowsV, err := e.svc.ModelServers(e.ctx, tokenV, f.modelM)
	if err != nil {
		t.Fatalf("ModelServers(usr_v, %s): %v", f.modelM, err)
	}
	if len(rowsV) != 0 {
		t.Fatalf("ModelServers(usr_v, %s) = %+v, want empty (not provisioned)", f.modelM, rowsV)
	}

	rowsU, err := e.svc.ModelServers(e.ctx, tokenU, f.modelM)
	if err != nil {
		t.Fatalf("ModelServers(usr_u, %s): %v", f.modelM, err)
	}
	if len(rowsU) == 0 {
		t.Fatalf("ModelServers(usr_u, %s) = empty, want non-empty (provisioned)", f.modelM)
	}
	for _, row := range rowsU {
		if row.ServerID != f.srvX.ID {
			t.Fatalf("ModelServers(usr_u, %s) row %+v, want server %s", f.modelM, row, f.srvX.ID)
		}
	}

	// The unrestricted model N stays visible to both regardless of provisioning.
	rowsNU, err := e.svc.ModelServers(e.ctx, tokenU, f.modelN)
	if err != nil {
		t.Fatalf("ModelServers(usr_u, %s): %v", f.modelN, err)
	}
	if len(rowsNU) == 0 {
		t.Fatalf("ModelServers(usr_u, %s) = empty, want non-empty (unrestricted)", f.modelN)
	}
	rowsNV, err := e.svc.ModelServers(e.ctx, tokenV, f.modelN)
	if err != nil {
		t.Fatalf("ModelServers(usr_v, %s): %v", f.modelN, err)
	}
	if len(rowsNV) == 0 {
		t.Fatalf("ModelServers(usr_v, %s) = empty, want non-empty (unrestricted)", f.modelN)
	}
}

// --- Fix round 1 (review follow-up) -----------------------------------------
//
// (1) The Dashboard "Live Model Routes" table (GET /api/portal/dashboard,
// reachable to ANY gateway:use token -- NOT admin-gated) read the token-less
// activeMappingViews and so leaked exactly the {model, host} pair the other
// surfaces above hide. dashboardRouteData() now goes through
// visibleMappingViews(ctx, token) like Models(); this test proves the leak is
// closed.
//
// (2) A model offered by BOTH a restricted and an unrestricted server must
// stay VISIBLE to a non-provisioned caller (it has at least one allowed
// offering server) -- and ModelServers() must return ONLY the unrestricted
// server's row for that caller, never all-or-nothing over-denial of the whole
// model. See TestModelVisibleViaEitherServerNoOverDenial below.

// TestDashboardRouteDataVisibilityProvisioningFilter: Dashboard()'s route
// table applies the exact same provisioning filter as Models() -- a
// non-provisioned caller never sees the restricted model's route (which would
// leak its gateway model NAME and its offering server's HOST), while the
// unrestricted model's route is unaffected.
func TestDashboardRouteDataVisibilityProvisioningFilter(t *testing.T) {
	e := newGroupTestEnv(t)
	f := setupVisibilityFixture(e)
	// Dashboard() also reads s.usage, which the shared groupTestEnv helper
	// leaves unwired (many OTHER tests over this helper never call Dashboard
	// and rely on it staying that way) -- build a second Service over the
	// SAME store/dir the fixture just populated, with Usage wired, purely for
	// this call.
	svc := NewService(ServiceDeps{
		Users: e.dir, Groups: e.dir, Routes: e.routes, Usage: usage.NewRecorder(),
		Clock: func() time.Time { return e.now },
	})
	tokenU := token("usr_u")
	tokenV := token("usr_v")

	routeModels := func(routes []RouteDTO) map[string]bool {
		out := make(map[string]bool, len(routes))
		for _, r := range routes {
			out[r.Model] = true
		}
		return out
	}

	dashU := svc.Dashboard(e.ctx, tokenU)
	gotU := routeModels(dashU.Routes)
	if !gotU[f.modelM] || !gotU[f.modelN] {
		t.Fatalf("Dashboard(usr_u).Routes = %+v, want both %s and %s (provisioned)", dashU.Routes, f.modelM, f.modelN)
	}

	dashV := svc.Dashboard(e.ctx, tokenV)
	gotV := routeModels(dashV.Routes)
	if gotV[f.modelM] {
		t.Fatalf("Dashboard(usr_v).Routes = %+v, leaked %s (restricted server, not provisioned)", dashV.Routes, f.modelM)
	}
	if !gotV[f.modelN] {
		t.Fatalf("Dashboard(usr_v).Routes = %+v, want %s present (unrestricted)", dashV.Routes, f.modelN)
	}
}

// TestModelVisibleViaEitherServerNoOverDenial: a model offered by BOTH a
// RESTRICTED server (a member of a provisioned resource group) and an
// UNRESTRICTED server stays visible in Models() to a non-provisioned caller
// (it has at least one allowed offering server), and ModelServers() returns
// ONLY the unrestricted server's row for that caller -- per-row filtering,
// never all-or-nothing over-denial of the whole model. A provisioned caller
// sees both offering servers.
func TestModelVisibleViaEitherServerNoOverDenial(t *testing.T) {
	e := newGroupTestEnv(t)
	e.createUser("usr_admin_both", "system_admin")
	e.createUser("usr_u2", "user")
	e.createUser("usr_v2", "user")
	sysAdmin := token("usr_admin_both", "system", "admin")

	sg := e.mustCreateGroup(sysAdmin, CreateGroupInput{Tier: store.GroupTierSystem, Name: "SG_BOTH"})
	e.mustAddMembers(sysAdmin, sg.ID, "usr_u2", "usr_v2")
	ag := e.mustCreateGroup(sysAdmin, CreateGroupInput{Tier: store.GroupTierAdmin, Name: "AG_BOTH", ParentGroupID: sg.ID})
	e.mustAddMembers(sysAdmin, ag.ID, "usr_u2") // usr_v2 deliberately NOT a member of AG_BOTH

	rg := e.mustCreateResourceGroup(routing.ResourceGroup{
		ID: "rgrp_both", Name: "RG_BOTH", Status: routing.ServerStatusActive,
		SystemGroupID: sg.ID, CreatedAt: e.now, UpdatedAt: e.now,
	})
	srvRestricted := e.mustCreateServer("srv_r_both", "Server Restricted")
	srvUnrestricted := e.mustCreateServer("srv_u_both", "Server Unrestricted")
	e.mustLinkResourceGroupServer(rg.ID, srvRestricted.ID)
	e.mustProvisionResourceGroup(rg.ID, routing.ProvisionKindAdminGroup, ag.ID)

	// BOTH servers offer the SAME gateway model name "model-both".
	if err := e.routes.CreateApplication(e.ctx, routing.Application{
		ID: "app_r_both", ServerID: srvRestricted.ID, Type: routing.ProviderVLLM, Port: 8000, Scheme: "https",
		APIFlavors: []string{routing.APIFlavorOpenAI}, Status: routing.ServerStatusActive,
		CreatedAt: e.now, UpdatedAt: e.now,
	}); err != nil {
		t.Fatalf("CreateApplication restricted: %v", err)
	}
	if err := e.routes.CreateMapping(e.ctx, routing.ModelMapping{
		ID: "map_r_both", ApplicationID: "app_r_both", GatewayModelName: "model-both", AppModelName: "model-both",
		Status: routing.ServerStatusActive, CreatedAt: e.now, UpdatedAt: e.now,
	}); err != nil {
		t.Fatalf("CreateMapping restricted: %v", err)
	}
	if err := e.routes.CreateApplication(e.ctx, routing.Application{
		ID: "app_u_both", ServerID: srvUnrestricted.ID, Type: routing.ProviderVLLM, Port: 8000, Scheme: "https",
		APIFlavors: []string{routing.APIFlavorOpenAI}, Status: routing.ServerStatusActive,
		CreatedAt: e.now, UpdatedAt: e.now,
	}); err != nil {
		t.Fatalf("CreateApplication unrestricted: %v", err)
	}
	if err := e.routes.CreateMapping(e.ctx, routing.ModelMapping{
		ID: "map_u_both", ApplicationID: "app_u_both", GatewayModelName: "model-both", AppModelName: "model-both",
		Status: routing.ServerStatusActive, CreatedAt: e.now, UpdatedAt: e.now,
	}); err != nil {
		t.Fatalf("CreateMapping unrestricted: %v", err)
	}

	tokenU2 := token("usr_u2")
	tokenV2 := token("usr_v2")

	// Non-provisioned V2 still sees the model: it has an allowed offering
	// server (the unrestricted one) -- no over-denial of the whole model.
	byIDV2 := modelIDs(e.svc.Models(e.ctx, tokenV2))
	if !byIDV2["model-both"] {
		t.Fatalf("Models(usr_v2) missing model-both, want present (visible via the unrestricted server)")
	}

	// ModelServers for V2 returns ONLY the unrestricted server's row.
	rowsV2, err := e.svc.ModelServers(e.ctx, tokenV2, "model-both")
	if err != nil {
		t.Fatalf("ModelServers(usr_v2, model-both): %v", err)
	}
	if len(rowsV2) != 1 {
		t.Fatalf("ModelServers(usr_v2, model-both) = %+v, want exactly 1 row (only the unrestricted server)", rowsV2)
	}
	if rowsV2[0].ServerID != srvUnrestricted.ID {
		t.Fatalf("ModelServers(usr_v2, model-both) row = %+v, want server %s", rowsV2[0], srvUnrestricted.ID)
	}

	// Provisioned U2 sees BOTH offering servers.
	rowsU2, err := e.svc.ModelServers(e.ctx, tokenU2, "model-both")
	if err != nil {
		t.Fatalf("ModelServers(usr_u2, model-both): %v", err)
	}
	if len(rowsU2) != 2 {
		t.Fatalf("ModelServers(usr_u2, model-both) = %+v, want 2 rows (provisioned -- sees both servers)", rowsU2)
	}
}

// TestOfferingCallableAppliesTheProvisioningFilter is the safety half of
// ModelOffering.Callable. Callable exists because model_settings visibility is
// a DISPLAY switch that must not count as unusable — but resource-group
// provisioning is the opposite, a genuine reachability boundary, and Callable
// has to keep applying it. Were it not, the Task-5 redirect would treat a
// restricted model as a legitimate target for a token that cannot route to it.
// The name still EXISTS, though, which is what keeps narrow mode's "not yours"
// refusal a refusal rather than a redirect.
func TestOfferingCallableAppliesTheProvisioningFilter(t *testing.T) {
	e := newGroupTestEnv(t)
	f := setupVisibilityFixture(e)

	offV := e.svc.ModelOfferingFor(e.ctx, token("usr_v"), routing.APIFlavorOpenAI)
	if _, ok := offV.Callable[f.modelM]; ok {
		t.Fatalf("Callable(usr_v) includes %s, want excluded (not provisioned into RG_VIS)", f.modelM)
	}
	if _, ok := offV.Existing[f.modelM]; !ok {
		t.Fatalf("Existing(usr_v) missing %s: a restricted model still exists", f.modelM)
	}
	if _, ok := offV.Callable[f.modelN]; !ok {
		t.Fatalf("Callable(usr_v) missing %s, want present (unrestricted)", f.modelN)
	}

	// The provisioned caller reaches both.
	offU := e.svc.ModelOfferingFor(e.ctx, token("usr_u"), routing.APIFlavorOpenAI)
	if _, ok := offU.Callable[f.modelM]; !ok {
		t.Fatalf("Callable(usr_u) missing %s, want present (provisioned)", f.modelM)
	}
	if _, ok := offU.Callable[f.modelN]; !ok {
		t.Fatalf("Callable(usr_u) missing %s, want present (unrestricted)", f.modelN)
	}
}
