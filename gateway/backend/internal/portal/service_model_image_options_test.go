// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"errors"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/routing"
	"slices"
	"testing"
	"time"
)

// seedImageOptionOffering creates an active server (name == serverID) with one
// active application of appType and one active mapping (gatewayModel) under it.
// A server_agent application declares the openai_images flavor like a real one;
// its mapping carries no runtime spec until seedImageOptionSpec adds one.
func seedImageOptionOffering(t *testing.T, rs routing.Store, now time.Time, serverID, appType, gatewayModel string) (appID, mappingID string) {
	t.Helper()
	ctx := context.Background()
	appID, mappingID = "app-"+serverID, "map-"+serverID
	if err := rs.CreateAIServer(ctx, routing.AIServer{ID: serverID, Name: serverID, Domain: serverID + ".test", Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateAIServer %s: %v", serverID, err)
	}
	seedImageOptionApp(t, rs, now, serverID, appID, mappingID, appType, gatewayModel, 8000)
	return appID, mappingID
}

// seedImageOptionApp adds an active application of appType, listening on port,
// and one active mapping (gatewayModel) under it to the existing server serverID
// (a server's applications must each use their own port).
func seedImageOptionApp(t *testing.T, rs routing.Store, now time.Time, serverID, appID, mappingID, appType, gatewayModel string, port int) {
	t.Helper()
	ctx := context.Background()
	if err := rs.CreateApplication(ctx, routing.Application{ID: appID, ServerID: serverID, Type: appType, Port: port, Scheme: "http", APIFlavors: []string{routing.APIFlavorOpenAI, routing.APIFlavorOpenAIImages}, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateApplication %s: %v", appID, err)
	}
	if err := rs.CreateMapping(ctx, routing.ModelMapping{ID: mappingID, ApplicationID: appID, GatewayModelName: gatewayModel, AppModelName: gatewayModel, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateMapping %s: %v", mappingID, err)
	}
}

// seedImageOptionSpec stores a runtime spec for mappingID with the given
// explicit type and launched binary (either may be empty: the effective type
// is the explicit one, else auto-detected from the binary).
func seedImageOptionSpec(t *testing.T, rs routing.Store, mappingID, specType, binary string) {
	t.Helper()
	if err := rs.UpsertRuntimeSpec(context.Background(), routing.RuntimeSpec{ID: "spec-" + mappingID, MappingID: mappingID, Type: specType, Binary: binary, Args: "[]", Env: "{}"}); err != nil {
		t.Fatalf("UpsertRuntimeSpec %s: %v", mappingID, err)
	}
}

// viewMappingIDs is the mapping ids of views, in order.
func viewMappingIDs(views []ImageOptionMapping) []string {
	ids := make([]string, 0, len(views))
	for _, v := range views {
		ids = append(ids, v.MappingID)
	}
	return ids
}

// A hidden or locked model resolves to nothing for a non-admin, exactly as in
// ModelServers; a shown model is unaffected and an admin bypasses the check.
func TestModelImageOptionViewsHiddenLockedSuppression(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	svc, rs := newModelServersTestService(t, now, fakeLoadedModels{})
	seedImageOptionOffering(t, rs, now, "srv-hidden", routing.ProviderStableDiffusionCpp, "hidden-img")
	seedImageOptionOffering(t, rs, now, "srv-locked", routing.ProviderStableDiffusionCpp, "locked-img")
	seedImageOptionOffering(t, rs, now, "srv-shown", routing.ProviderStableDiffusionCpp, "shown-img")
	for name, visibility := range map[string]string{"hidden-img": "hidden", "locked-img": "locked"} {
		if err := rs.UpsertModelSetting(ctx, routing.ModelSetting{GatewayModelName: name, Visibility: visibility, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("UpsertModelSetting %s: %v", name, err)
		}
	}
	nonAdmin := auth.Token{UserID: "usr_plain_io", Scopes: []string{"gateway:use"}}

	for _, name := range []string{"hidden-img", "locked-img"} {
		views, err := svc.ModelImageOptionViews(ctx, nonAdmin, name)
		if err != nil {
			t.Fatalf("ModelImageOptionViews(non-admin, %s): %v", name, err)
		}
		if len(views) != 0 {
			t.Fatalf("ModelImageOptionViews(non-admin, %s) = %v, want empty (suppressed)", name, viewMappingIDs(views))
		}
		adminViews, err := svc.ModelImageOptionViews(ctx, adminToken(), name)
		if err != nil {
			t.Fatalf("ModelImageOptionViews(admin, %s): %v", name, err)
		}
		if len(adminViews) != 1 {
			t.Fatalf("ModelImageOptionViews(admin, %s) = %v, want 1 (admin bypasses suppression)", name, viewMappingIDs(adminViews))
		}
	}
	shown, err := svc.ModelImageOptionViews(ctx, nonAdmin, "shown-img")
	if err != nil {
		t.Fatalf("ModelImageOptionViews(non-admin, shown-img): %v", err)
	}
	if len(shown) != 1 {
		t.Fatalf("ModelImageOptionViews(non-admin, shown-img) = %v, want 1 (unaffected)", viewMappingIDs(shown))
	}
}

// failingModelSettingsStore fails the model_settings read that backs the
// hidden/locked suppression.
type failingModelSettingsStore struct {
	*routing.MemoryStore
	err error
}

func (f *failingModelSettingsStore) ModelSettings(context.Context) ([]routing.ModelSetting, error) {
	return nil, f.err
}

// The suppression read fails CLOSED: a store blip must surface as an error for
// a non-admin, never as a leaked hidden/locked mapping.
func TestModelImageOptionViewsFailsClosedOnSettingsError(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	boom := errors.New("settings table unavailable")
	routes := &failingModelSettingsStore{MemoryStore: routing.NewMemoryStore(), err: boom}
	svc := newModelServersTestServiceWithRoutes(t, now, fakeLoadedModels{}, routes)
	seedImageOptionOffering(t, routes, now, "srv-a", routing.ProviderStableDiffusionCpp, "img")

	views, err := svc.ModelImageOptionViews(ctx, auth.Token{UserID: "usr_plain_io", Scopes: []string{"gateway:use"}}, "img")
	if !errors.Is(err, boom) {
		t.Fatalf("ModelImageOptionViews(non-admin) error = %v, want %v", err, boom)
	}
	if len(views) != 0 {
		t.Fatalf("ModelImageOptionViews(non-admin) = %v alongside the error, want none", viewMappingIDs(views))
	}
	// An admin never reads the settings, so the failure does not touch it.
	adminViews, err := svc.ModelImageOptionViews(ctx, adminToken(), "img")
	if err != nil || len(adminViews) != 1 {
		t.Fatalf("ModelImageOptionViews(admin) = (%v, %v), want 1 view and no error", viewMappingIDs(adminViews), err)
	}
}

// A server inside a provisioned resource group the caller is not provisioned
// into is excluded, even though the model is offered there; a provisioned
// caller sees it. A model only the restricted server offers is empty.
func TestModelImageOptionViewsResourceGroupFilter(t *testing.T) {
	e := newGroupTestEnv(t)
	f := setupVisibilityFixture(e) // srvX restricted (provisioned for usr_u), srvY unrestricted
	// Both servers also offer the same sd-server image model, and only the
	// restricted one offers a second.
	seedImageOptionApp(t, e.routes, e.now, f.srvX.ID, "app_x_img", "map_x_img", routing.ProviderStableDiffusionCpp, "img-shared", 8100)
	seedImageOptionApp(t, e.routes, e.now, f.srvY.ID, "app_y_img", "map_y_img", routing.ProviderStableDiffusionCpp, "img-shared", 8100)
	seedImageOptionApp(t, e.routes, e.now, f.srvX.ID, "app_x_only", "map_x_only", routing.ProviderStableDiffusionCpp, "img-restricted-only", 8101)

	provisioned, err := e.svc.ModelImageOptionViews(e.ctx, token("usr_u"), "img-shared")
	if err != nil {
		t.Fatalf("ModelImageOptionViews(usr_u): %v", err)
	}
	if got := viewMappingIDs(provisioned); !slices.Equal(got, []string{"map_x_img", "map_y_img"}) {
		t.Fatalf("ModelImageOptionViews(usr_u, img-shared) = %v, want both servers", got)
	}

	outsider, err := e.svc.ModelImageOptionViews(e.ctx, token("usr_v"), "img-shared")
	if err != nil {
		t.Fatalf("ModelImageOptionViews(usr_v): %v", err)
	}
	if got := viewMappingIDs(outsider); !slices.Equal(got, []string{"map_y_img"}) {
		t.Fatalf("ModelImageOptionViews(usr_v, img-shared) = %v, want only the unrestricted server", got)
	}

	onlyRestricted, err := e.svc.ModelImageOptionViews(e.ctx, token("usr_v"), "img-restricted-only")
	if err != nil {
		t.Fatalf("ModelImageOptionViews(usr_v, img-restricted-only): %v", err)
	}
	if len(onlyRestricted) != 0 {
		t.Fatalf("ModelImageOptionViews(usr_v, img-restricted-only) = %v, want empty (the model's only server is restricted)", viewMappingIDs(onlyRestricted))
	}
}

// Only sd-server mappings survive: a plain openai (vllm) application, a
// server_agent whose spec launches a different server kind, and a server_agent
// with no spec at all are dropped, though they offer the very same name.
func TestModelImageOptionViewsDropsNonSDServerMappings(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	svc, rs := newModelServersTestService(t, now, fakeLoadedModels{})
	const name = "mixed"
	seedImageOptionOffering(t, rs, now, "srv-a-vllm", routing.ProviderVLLM, name)
	_, llamaMap := seedImageOptionOffering(t, rs, now, "srv-b-llama-agent", routing.ProviderServerAgent, name)
	seedImageOptionSpec(t, rs, llamaMap, "", "/opt/llama/llama-server")
	_, explicitMap := seedImageOptionOffering(t, rs, now, "srv-c-explicit-llama", routing.ProviderServerAgent, name)
	// An explicit non-sd type wins over a binary that looks like sd-server.
	seedImageOptionSpec(t, rs, explicitMap, string(routing.RuntimeSpecTypeLlamaCpp), "/opt/sd-server/sd-server")
	seedImageOptionOffering(t, rs, now, "srv-d-no-spec-agent", routing.ProviderServerAgent, name)
	seedImageOptionOffering(t, rs, now, "srv-e-sd", routing.ProviderStableDiffusionCpp, name)

	views, err := svc.ModelImageOptionViews(ctx, adminToken(), name)
	if err != nil {
		t.Fatalf("ModelImageOptionViews: %v", err)
	}
	if got := viewMappingIDs(views); !slices.Equal(got, []string{"map-srv-e-sd"}) {
		t.Fatalf("ModelImageOptionViews = %v, want only the external stable_diffusion_cpp mapping", got)
	}
}

// An external stable_diffusion_cpp application AND a server_agent mapping whose
// effective runtime-spec type is stable_diffusion_cpp (explicit, or detected
// from the binary) are both returned as exported ImageOptionMappings, sorted by
// server name then mapping id. Each carries the identity a caller outside this
// package needs (server, application, application type, mapping); an agent
// mapping also carries its runtime spec's id and effective type, an external
// application none.
func TestModelImageOptionViewsIncludesExternalAndAgentSDServer(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	svc, rs := newModelServersTestService(t, now, fakeLoadedModels{})
	const name = "flux"
	// Seeded out of name order on purpose.
	seedImageOptionOffering(t, rs, now, "srv-c-external", routing.ProviderStableDiffusionCpp, name)
	_, detectedMap := seedImageOptionOffering(t, rs, now, "srv-a-agent-detected", routing.ProviderServerAgent, name)
	seedImageOptionSpec(t, rs, detectedMap, "", "/opt/stable-diffusion.cpp/build/bin/sd-server")
	_, explicitMap := seedImageOptionOffering(t, rs, now, "srv-b-agent-explicit", routing.ProviderServerAgent, name)
	seedImageOptionSpec(t, rs, explicitMap, string(routing.RuntimeSpecTypeStableDiffusionCpp), "/opt/bin/custom-launcher")

	got, err := svc.ModelImageOptionViews(ctx, adminToken(), name)
	if err != nil {
		t.Fatalf("ModelImageOptionViews: %v", err)
	}
	want := []ImageOptionMapping{
		{ServerID: "srv-a-agent-detected", ServerName: "srv-a-agent-detected", ApplicationID: "app-srv-a-agent-detected", AppType: routing.ProviderServerAgent, MappingID: "map-srv-a-agent-detected", SpecID: "spec-map-srv-a-agent-detected", SpecType: "stable_diffusion_cpp"},
		{ServerID: "srv-b-agent-explicit", ServerName: "srv-b-agent-explicit", ApplicationID: "app-srv-b-agent-explicit", AppType: routing.ProviderServerAgent, MappingID: "map-srv-b-agent-explicit", SpecID: "spec-map-srv-b-agent-explicit", SpecType: "stable_diffusion_cpp"},
		{ServerID: "srv-c-external", ServerName: "srv-c-external", ApplicationID: "app-srv-c-external", AppType: routing.ProviderStableDiffusionCpp, MappingID: "map-srv-c-external"},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("ModelImageOptionViews =\n%+v\nwant\n%+v", got, want)
	}
}

// An unknown name, or a known name with no sd-server mapping, is an empty
// (non-nil) slice and not an error.
func TestModelImageOptionViewsUnknownOrNonSDNameIsEmpty(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	svc, rs := newModelServersTestService(t, now, fakeLoadedModels{})
	seedImageOptionOffering(t, rs, now, "srv-text", routing.ProviderVLLM, "chat-model")

	for _, name := range []string{"does-not-exist", "chat-model", ""} {
		views, err := svc.ModelImageOptionViews(ctx, adminToken(), name)
		if err != nil {
			t.Fatalf("ModelImageOptionViews(%q): %v", name, err)
		}
		if views == nil || len(views) != 0 {
			t.Fatalf("ModelImageOptionViews(%q) = %#v, want an empty non-nil slice", name, views)
		}
	}
}

// failingRuntimeSpecStore fails the per-application runtime-spec read.
type failingRuntimeSpecStore struct {
	*routing.MemoryStore
	err error
}

func (f *failingRuntimeSpecStore) RuntimeSpecsByApplication(context.Context, string) ([]routing.RuntimeSpec, error) {
	return nil, f.err
}

// A server_agent application whose spec cannot be read cannot be shown to
// serve an sd-server, so its mapping is withheld (fail-closed) while an
// external sd-server mapping for the same name is unaffected.
func TestModelImageOptionViewsWithholdsAgentWhenSpecReadFails(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	routes := &failingRuntimeSpecStore{MemoryStore: routing.NewMemoryStore(), err: errors.New("spec table unavailable")}
	svc := newModelServersTestServiceWithRoutes(t, now, fakeLoadedModels{}, routes)
	_, agentMap := seedImageOptionOffering(t, routes, now, "srv-agent", routing.ProviderServerAgent, "img")
	seedImageOptionSpec(t, routes, agentMap, "", "/opt/sd-server/sd-server")
	seedImageOptionOffering(t, routes, now, "srv-external", routing.ProviderStableDiffusionCpp, "img")

	views, err := svc.ModelImageOptionViews(ctx, adminToken(), "img")
	if err != nil {
		t.Fatalf("ModelImageOptionViews: %v", err)
	}
	if got := viewMappingIDs(views); !slices.Equal(got, []string{"map-srv-external"}) {
		t.Fatalf("ModelImageOptionViews = %v, want only the external mapping (agent withheld: spec unreadable)", got)
	}
}

// countingRuntimeSpecStore counts RuntimeSpecsByApplication reads per application.
type countingRuntimeSpecStore struct {
	*routing.MemoryStore
	reads map[string]int
}

func (c *countingRuntimeSpecStore) RuntimeSpecsByApplication(ctx context.Context, appID string) ([]routing.RuntimeSpec, error) {
	if c.reads == nil {
		c.reads = make(map[string]int)
	}
	c.reads[appID]++
	return c.MemoryStore.RuntimeSpecsByApplication(ctx, appID)
}

// Runtime specs are read only for mappings the caller may see: the visibility
// gates run BEFORE the sd-server classification, so a suppressed (hidden or
// locked) model's agent mapping never costs a spec read. The admin call on the
// same mapping is the positive control: it proves the counter sees a read when
// one is made.
func TestModelImageOptionViewsReadsNoSpecForSuppressedMapping(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	routes := &countingRuntimeSpecStore{MemoryStore: routing.NewMemoryStore()}
	svc := newModelServersTestServiceWithRoutes(t, now, fakeLoadedModels{}, routes)
	hiddenApp, hiddenMap := seedImageOptionOffering(t, routes, now, "srv-hidden-agent", routing.ProviderServerAgent, "hidden-img")
	seedImageOptionSpec(t, routes, hiddenMap, "", "/opt/sd-server/sd-server")
	shownApp, shownMap := seedImageOptionOffering(t, routes, now, "srv-shown-agent", routing.ProviderServerAgent, "shown-img")
	seedImageOptionSpec(t, routes, shownMap, "", "/opt/sd-server/sd-server")
	if err := routes.UpsertModelSetting(ctx, routing.ModelSetting{GatewayModelName: "hidden-img", Visibility: "hidden", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("UpsertModelSetting: %v", err)
	}
	nonAdmin := auth.Token{UserID: "usr_plain_io", Scopes: []string{"gateway:use"}}

	views, err := svc.ModelImageOptionViews(ctx, nonAdmin, "hidden-img")
	if err != nil || len(views) != 0 {
		t.Fatalf("ModelImageOptionViews(non-admin, hidden-img) = (%v, %v), want empty and no error", viewMappingIDs(views), err)
	}
	if n := routes.reads[hiddenApp]; n != 0 {
		t.Fatalf("spec reads for the suppressed mapping's application = %d, want 0 (specs are read only for visible mappings)", n)
	}

	if views, err = svc.ModelImageOptionViews(ctx, nonAdmin, "shown-img"); err != nil || len(views) != 1 {
		t.Fatalf("ModelImageOptionViews(non-admin, shown-img) = (%v, %v), want 1 view", viewMappingIDs(views), err)
	}
	if n := routes.reads[shownApp]; n != 1 {
		t.Fatalf("spec reads for a visible agent application = %d, want 1 (the counter must see a read when one is made)", n)
	}
	if views, err = svc.ModelImageOptionViews(ctx, adminToken(), "hidden-img"); err != nil || len(views) != 1 {
		t.Fatalf("ModelImageOptionViews(admin, hidden-img) = (%v, %v), want 1 view (admin bypasses suppression)", viewMappingIDs(views), err)
	}
	if n := routes.reads[hiddenApp]; n != 1 {
		t.Fatalf("spec reads for the hidden mapping's application after the admin call = %d, want 1", n)
	}
}

// The same invariant for the resource-group gate: an agent mapping on a server
// the caller is not provisioned to use costs no spec read, while the one on a
// server the caller may use does.
func TestModelImageOptionViewsReadsNoSpecForRestrictedServer(t *testing.T) {
	e := newGroupTestEnv(t)
	f := setupVisibilityFixture(e) // srvX restricted (provisioned for usr_u), srvY unrestricted
	routes := &countingRuntimeSpecStore{MemoryStore: e.routes}
	svc := NewService(ServiceDeps{Users: e.dir, Groups: e.dir, Routes: routes, Clock: func() time.Time { return e.now }})
	seedImageOptionApp(t, routes, e.now, f.srvX.ID, "app_x_agent", "map_x_agent", routing.ProviderServerAgent, "img-agent", 8100)
	seedImageOptionSpec(t, routes, "map_x_agent", "", "/opt/sd-server/sd-server")
	seedImageOptionApp(t, routes, e.now, f.srvY.ID, "app_y_agent", "map_y_agent", routing.ProviderServerAgent, "img-agent", 8100)
	seedImageOptionSpec(t, routes, "map_y_agent", "", "/opt/sd-server/sd-server")

	views, err := svc.ModelImageOptionViews(e.ctx, token("usr_v"), "img-agent")
	if err != nil {
		t.Fatalf("ModelImageOptionViews(usr_v): %v", err)
	}
	if got := viewMappingIDs(views); !slices.Equal(got, []string{"map_y_agent"}) {
		t.Fatalf("ModelImageOptionViews(usr_v, img-agent) = %v, want only the unrestricted server's mapping", got)
	}
	if n := routes.reads["app_x_agent"]; n != 0 {
		t.Fatalf("spec reads for the restricted server's application = %d, want 0 (specs are read only for visible mappings)", n)
	}
	if n := routes.reads["app_y_agent"]; n != 1 {
		t.Fatalf("spec reads for the visible server's application = %d, want 1", n)
	}
}
