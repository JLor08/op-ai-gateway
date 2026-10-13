// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/provider"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/store"
	"op-ai-gateway/internal/usage"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

const (
	ioSecret = "io-owner-secret"
	ioModel  = "shared/flux" // a gateway model name may contain '/'
)

var ioNow = time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)

// settingsFailStore fails the model_settings read that backs the portal's
// hidden/locked suppression, which fails CLOSED for a non-admin.
type settingsFailStore struct {
	routing.Store
	err error
}

func (f settingsFailStore) ModelSettings(context.Context) ([]routing.ModelSetting, error) {
	return nil, f.err
}

// imageOptionsFixture is a *Server over an in-memory routing store with one
// non-admin gateway:use token (ioSecret). The two option caches are the
// server's own (New creates both), so a test fills them through s.AppImageOptions
// and s.RuntimeStatus exactly as the app-health loop and the agent ingest do.
type imageOptionsFixture struct {
	s      *Server
	routes *routing.MemoryStore
}

func newImageOptionsFixture(t *testing.T) *imageOptionsFixture {
	t.Helper()
	return newImageOptionsFixtureWrapped(t, func(st routing.Store) routing.Store { return st })
}

func newImageOptionsFixtureWrapped(t *testing.T, wrap func(routing.Store) routing.Store) *imageOptionsFixture {
	t.Helper()
	ctx := context.Background()
	tokens := auth.NewTokenStore()
	dir := portal.NewMemoryDirectory(tokens)
	dir.AddUser(store.User{ID: "usr_io", Email: "io@example.test", DisplayName: "IO User", Role: "user", Status: store.UserStatusActive, PreferredLanguage: "de", CreatedAt: ioNow, UpdatedAt: ioNow})
	if err := dir.CreatePlainToken(ctx, store.TokenRecord{ID: "tok_io", UserID: "usr_io", Name: "IO Token", Status: store.TokenStatusActive, Scopes: `["gateway:use"]`, CreatedAt: ioNow, UpdatedAt: ioNow}, ioSecret); err != nil {
		t.Fatalf("CreatePlainToken: %v", err)
	}
	mem := routing.NewMemoryStore()
	routes := wrap(mem)
	recorder := usage.NewRecorder()
	svc := portal.NewService(portal.ServiceDeps{Users: dir, Tokens: dir, Usage: recorder, Routes: routes})
	s := New(ServerDeps{Tokens: tokens, Usage: recorder, Routes: routes, Portal: svc})
	return &imageOptionsFixture{s: s, routes: mem}
}

// offering seeds an active server with one active application of appType and
// one active mapping offering ioModel, and returns the application and mapping
// ids. A server_agent application additionally gets an enabled
// stable_diffusion_cpp runtime spec (id "spec-"+mappingID), which is what makes
// the portal classify it as an agent-launched sd-server.
func (f *imageOptionsFixture) offering(t *testing.T, serverID, appType string) (appID, mappingID, specID string) {
	t.Helper()
	ctx := context.Background()
	appID, mappingID = "app_"+serverID, "map_"+serverID
	if err := f.routes.CreateAIServer(ctx, routing.AIServer{ID: serverID, Name: serverID, Domain: serverID + ".example.test", Provider: routing.ProviderMock, Endpoint: "mock://" + serverID, Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: ioNow, UpdatedAt: ioNow}); err != nil {
		t.Fatalf("CreateAIServer %s: %v", serverID, err)
	}
	if err := f.routes.CreateApplication(ctx, routing.Application{ID: appID, ServerID: serverID, Type: appType, Port: 8000, Scheme: "http", APIFlavors: []string{routing.APIFlavorOpenAI, routing.APIFlavorOpenAIImages}, Status: routing.ServerStatusActive, CreatedAt: ioNow, UpdatedAt: ioNow}); err != nil {
		t.Fatalf("CreateApplication %s: %v", appID, err)
	}
	if err := f.routes.CreateMapping(ctx, routing.ModelMapping{ID: mappingID, ApplicationID: appID, GatewayModelName: ioModel, AppModelName: "flux", Status: routing.ServerStatusActive, CreatedAt: ioNow, UpdatedAt: ioNow}); err != nil {
		t.Fatalf("CreateMapping %s: %v", mappingID, err)
	}
	if appType == routing.ProviderServerAgent {
		specID = "spec_" + mappingID
		if err := f.routes.UpsertRuntimeSpec(ctx, routing.RuntimeSpec{ID: specID, MappingID: mappingID, Enabled: true, Type: string(routing.RuntimeSpecTypeStableDiffusionCpp), Binary: "/opt/sd/sd-server", Args: "[]", Env: "{}"}); err != nil {
			t.Fatalf("UpsertRuntimeSpec %s: %v", specID, err)
		}
	}
	return appID, mappingID, specID
}

// get issues GET /api/portal/model-image-options?name=<name> with the fixture token.
func (f *imageOptionsFixture) get(name string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/api/portal/model-image-options?name="+url.QueryEscape(name), nil)
	req.Header.Set("Authorization", "Bearer "+ioSecret)
	rec := httptest.NewRecorder()
	f.s.ServeHTTP(rec, req)
	return rec
}

// decodeImageOptions asserts a 200 and returns the {"data":...} payload.
func decodeImageOptions(t *testing.T, rec *httptest.ResponseRecorder) ModelImageOptionsDTO {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		Data ModelImageOptionsDTO `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body.String())
	}
	return out.Data
}

// externalSdOptions and agentSdOptions overlap on purpose: their intersection is
// not equal to either, so a test can tell which inputs reached the aggregation.
func externalSdOptions() provider.SdcppImageOptions {
	return provider.SdcppImageOptions{
		Samplers:      []string{"euler", "dpm++2m", "heun"},
		Schedulers:    []string{"karras", "simple"},
		OutputFormats: []string{"png", "jpeg"},
		Limits:        provider.SdcppImageLimits{MinWidth: 64, MaxWidth: 2048, MinHeight: 64, MaxHeight: 2048, MaxBatchCount: 8},
		Defaults:      provider.SdcppImageDefaults{Sampler: "euler", Steps: 20, Width: 512, Height: 512},
	}
}

func agentSdOptions() provider.SdcppImageOptions {
	return provider.SdcppImageOptions{
		Samplers:      []string{"dpm++2m", "euler"},
		Schedulers:    []string{"karras"},
		OutputFormats: []string{"png"},
		Limits:        provider.SdcppImageLimits{MinWidth: 128, MaxWidth: 1024, MinHeight: 64, MaxHeight: 1536, MaxBatchCount: 4},
		Defaults:      provider.SdcppImageDefaults{Sampler: "euler", Steps: 30, Width: 512, Height: 512},
	}
}

// An external stable_diffusion_cpp model reads its options from the live
// per-application cache; a single source passes through unchanged.
func TestModelImageOptionsExternalOnlyReadsAppCache(t *testing.T) {
	f := newImageOptionsFixture(t)
	appID, _, _ := f.offering(t, "srv_ext", routing.ProviderStableDiffusionCpp)
	f.s.AppImageOptions.Set(appID, externalSdOptions())

	got := decodeImageOptions(t, f.get(ioModel))
	want := ModelImageOptionsDTO{
		Samplers:      []string{"euler", "dpm++2m", "heun"},
		Schedulers:    []string{"karras", "simple"},
		OutputFormats: []string{"png", "jpeg"},
		SizeLimits:    &SizeLimitsDTO{MinWidth: 64, MaxWidth: 2048, MinHeight: 64, MaxHeight: 2048},
		MaxBatchCount: 8,
		Defaults:      &ImageDefaultsDTO{Sampler: "euler", Steps: 20, Width: 512, Height: 512},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("data = %+v (limits %+v, defaults %+v), want %+v", got, got.SizeLimits, got.Defaults, want)
	}
}

// An agent-launched model reads its options from the runtime-status snapshot,
// picking the frame by spec id: a sibling spec on the same server, and another
// server's frame, must not leak in.
func TestModelImageOptionsAgentOnlyReadsRuntimeSnapshotBySpecID(t *testing.T) {
	f := newImageOptionsFixture(t)
	_, _, specID := f.offering(t, "srv_agent", routing.ProviderServerAgent)
	agent := agentSdOptions()
	sibling := externalSdOptions() // a different spec on the SAME server
	f.s.RuntimeStatus.publish("srv_agent", []RuntimeStatusDTO{
		{SpecID: "spec_other", State: "running", ImageOptions: &sibling},
		{SpecID: specID, State: "running", ImageOptions: &agent},
	})
	foreign := externalSdOptions() // the same spec id on ANOTHER server
	f.s.RuntimeStatus.publish("srv_elsewhere", []RuntimeStatusDTO{{SpecID: specID, ImageOptions: &foreign}})

	got := decodeImageOptions(t, f.get(ioModel))
	if !reflect.DeepEqual(got.Samplers, []string{"dpm++2m", "euler"}) ||
		!reflect.DeepEqual(got.Schedulers, []string{"karras"}) ||
		!reflect.DeepEqual(got.OutputFormats, []string{"png"}) ||
		got.MaxBatchCount != 4 {
		t.Fatalf("data = %+v, want the agent spec's own options (samplers dpm++2m,euler / karras / png / batch 4)", got)
	}
	if want := (&SizeLimitsDTO{MinWidth: 128, MaxWidth: 1024, MinHeight: 64, MaxHeight: 1536}); !reflect.DeepEqual(got.SizeLimits, want) {
		t.Fatalf("size_limits = %+v, want %+v", got.SizeLimits, want)
	}
}

// One external and one agent-launched server offering the same model: the
// answer is what BOTH can do (intersected lists, tightest envelope, smallest
// batch), not either one's own options.
func TestModelImageOptionsMixedIntersectsAcrossBothCaches(t *testing.T) {
	f := newImageOptionsFixture(t)
	extApp, _, _ := f.offering(t, "srv_ext", routing.ProviderStableDiffusionCpp)
	_, _, specID := f.offering(t, "srv_agent", routing.ProviderServerAgent)
	f.s.AppImageOptions.Set(extApp, externalSdOptions())
	agent := agentSdOptions()
	f.s.RuntimeStatus.publish("srv_agent", []RuntimeStatusDTO{{SpecID: specID, State: "running", ImageOptions: &agent}})

	got := decodeImageOptions(t, f.get(ioModel))
	// The intersection's ORDER follows the first non-empty list, i.e. the first
	// view (aggregateImageOptions' own contract); this test is about WHICH
	// values survive, so compare the samplers as a sorted set.
	slices.Sort(got.Samplers)
	want := ModelImageOptionsDTO{
		Samplers:      []string{"dpm++2m", "euler"},
		Schedulers:    []string{"karras"},
		OutputFormats: []string{"png"},
		SizeLimits:    &SizeLimitsDTO{MinWidth: 128, MaxWidth: 1024, MinHeight: 64, MaxHeight: 1536},
		MaxBatchCount: 4,
		// Steps disagree (20 vs 30) and so drop out; sampler and size agree.
		Defaults: &ImageDefaultsDTO{Sampler: "euler", Width: 512, Height: 512},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("data = %+v (limits %+v, defaults %+v), want %+v", got, got.SizeLimits, got.Defaults, want)
	}
}

// A mapping with no cached options (an external app the probe has not read yet,
// an agent whose frame carries none or that has not reported at all) contributes
// nothing: it neither fails the request nor constrains the aggregation.
func TestModelImageOptionsMappingWithoutCachedOptionsContributesNothing(t *testing.T) {
	f := newImageOptionsFixture(t)
	extApp, _, _ := f.offering(t, "srv_ext_known", routing.ProviderStableDiffusionCpp)
	f.offering(t, "srv_ext_unprobed", routing.ProviderStableDiffusionCpp)        // no registry entry
	f.offering(t, "srv_agent_silent", routing.ProviderServerAgent)               // never reported a status frame
	_, _, nilSpec := f.offering(t, "srv_agent_nil", routing.ProviderServerAgent) // frame without image options
	f.s.AppImageOptions.Set(extApp, externalSdOptions())
	f.s.RuntimeStatus.publish("srv_agent_nil", []RuntimeStatusDTO{{SpecID: nilSpec, State: "running"}})

	got := decodeImageOptions(t, f.get(ioModel))
	if !reflect.DeepEqual(got.Samplers, []string{"euler", "dpm++2m", "heun"}) || got.MaxBatchCount != 8 {
		t.Fatalf("data = %+v, want exactly the one known mapping's options", got)
	}
}

// Nothing cached anywhere is an empty DTO and a 200, not an error.
func TestModelImageOptionsNoCachedOptionsIsEmptyObject(t *testing.T) {
	f := newImageOptionsFixture(t)
	f.offering(t, "srv_ext", routing.ProviderStableDiffusionCpp)
	f.offering(t, "srv_agent", routing.ProviderServerAgent)

	rec := f.get(ioModel)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if body := strings.TrimSpace(rec.Body.String()); body != `{"data":{}}` {
		t.Fatalf("body = %s, want {\"data\":{}}", body)
	}
}

// A model with no sd-server mapping at all (unknown name, or a plain openai
// application) is the same empty answer.
func TestModelImageOptionsUnknownAndNonSdModelsAreEmpty(t *testing.T) {
	f := newImageOptionsFixture(t)
	f.offering(t, "srv_vllm", routing.ProviderVLLM)
	for _, name := range []string{"does/not-exist", ioModel} {
		rec := f.get(name)
		if rec.Code != http.StatusOK {
			t.Fatalf("name %q: status = %d, body=%s", name, rec.Code, rec.Body.String())
		}
		if body := strings.TrimSpace(rec.Body.String()); body != `{"data":{}}` {
			t.Fatalf("name %q: body = %s, want {\"data\":{}}", name, body)
		}
	}
}

// A model the token cannot route to (hidden for a non-admin) resolves to no
// views, so neither cache is consulted and nothing about the model leaks:
// both caches are full, the answer is still empty.
func TestModelImageOptionsUnroutableModelLeaksNothing(t *testing.T) {
	f := newImageOptionsFixture(t)
	extApp, _, _ := f.offering(t, "srv_ext", routing.ProviderStableDiffusionCpp)
	_, _, specID := f.offering(t, "srv_agent", routing.ProviderServerAgent)
	f.s.AppImageOptions.Set(extApp, externalSdOptions())
	agent := agentSdOptions()
	f.s.RuntimeStatus.publish("srv_agent", []RuntimeStatusDTO{{SpecID: specID, ImageOptions: &agent}})
	if err := f.routes.UpsertModelSetting(context.Background(), routing.ModelSetting{GatewayModelName: ioModel, Visibility: "hidden", CreatedAt: ioNow, UpdatedAt: ioNow}); err != nil {
		t.Fatalf("UpsertModelSetting: %v", err)
	}

	rec := f.get(ioModel)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if body := strings.TrimSpace(rec.Body.String()); body != `{"data":{}}` {
		t.Fatalf("body = %s, want {\"data\":{}} (hidden model must not leak its options)", body)
	}
}

// Both caches are optional on the Server (a bare one in a test, or a deployment
// that never wired them): a nil registry just contributes nothing.
func TestModelImageOptionsNilRegistriesAreSafe(t *testing.T) {
	f := newImageOptionsFixture(t)
	f.offering(t, "srv_ext", routing.ProviderStableDiffusionCpp)
	f.offering(t, "srv_agent", routing.ProviderServerAgent)
	f.s.AppImageOptions = nil
	f.s.RuntimeStatus = nil

	rec := f.get(ioModel)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if body := strings.TrimSpace(rec.Body.String()); body != `{"data":{}}` {
		t.Fatalf("body = %s, want {\"data\":{}}", body)
	}
}

// The handler only reads the caches: the registry's snapshot (whose
// ImageOptions the runtime registry shares with every reader) is unchanged by a
// request that aggregates it.
func TestModelImageOptionsDoesNotMutateCachedOptions(t *testing.T) {
	f := newImageOptionsFixture(t)
	extApp, _, _ := f.offering(t, "srv_ext", routing.ProviderStableDiffusionCpp)
	_, _, specID := f.offering(t, "srv_agent", routing.ProviderServerAgent)
	f.s.AppImageOptions.Set(extApp, externalSdOptions())
	agent := agentSdOptions()
	f.s.RuntimeStatus.publish("srv_agent", []RuntimeStatusDTO{{SpecID: specID, ImageOptions: &agent}})

	decodeImageOptions(t, f.get(ioModel))

	if got := f.s.RuntimeStatus.statusSnapshot("srv_agent")[0].ImageOptions; !reflect.DeepEqual(*got, agentSdOptions()) {
		t.Fatalf("runtime snapshot options = %+v, want unchanged %+v", *got, agentSdOptions())
	}
	if got, _ := f.s.AppImageOptions.Get(extApp); !reflect.DeepEqual(got, externalSdOptions()) {
		t.Fatalf("app cache options = %+v, want unchanged %+v", got, externalSdOptions())
	}
}

// Only GET is served; the Allow header names it.
func TestModelImageOptionsRejectsNonGet(t *testing.T) {
	f := newImageOptionsFixture(t)
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		req := httptest.NewRequest(method, "/api/portal/model-image-options?name="+url.QueryEscape(ioModel), nil)
		req.Header.Set("Authorization", "Bearer "+ioSecret)
		rec := httptest.NewRecorder()
		f.s.ServeHTTP(rec, req)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s: status = %d, want 405 (body=%s)", method, rec.Code, rec.Body.String())
		}
		if allow := rec.Header().Get("Allow"); allow != http.MethodGet {
			t.Fatalf("%s: Allow = %q, want GET", method, allow)
		}
	}
}

// A missing or empty ?name= is a 400 with an error body, not an empty 200 (a
// caller that forgot the parameter would otherwise read "no options").
func TestModelImageOptionsRequiresName(t *testing.T) {
	f := newImageOptionsFixture(t)
	for _, target := range []string{"/api/portal/model-image-options", "/api/portal/model-image-options?name="} {
		req := httptest.NewRequest(http.MethodGet, target, nil)
		req.Header.Set("Authorization", "Bearer "+ioSecret)
		rec := httptest.NewRecorder()
		f.s.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: status = %d, want 400 (body=%s)", target, rec.Code, rec.Body.String())
		}
		if code := errorBodyOf(t, rec); code != "model.name_required" {
			t.Fatalf("%s: error code = %q, want model.name_required", target, code)
		}
	}
}

// The endpoint needs a gateway:use session like every portal model endpoint.
func TestModelImageOptionsRequiresAuth(t *testing.T) {
	f := newImageOptionsFixture(t)
	req := httptest.NewRequest(http.MethodGet, "/api/portal/model-image-options?name="+url.QueryEscape(ioModel), nil)
	rec := httptest.NewRecorder()
	f.s.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (body=%s)", rec.Code, rec.Body.String())
	}
}

// A portal failure (here the fail-closed settings read) is a 500 with an error
// body, as model-servers answers one, and never an options payload.
func TestModelImageOptionsPortalErrorIs500(t *testing.T) {
	f := newImageOptionsFixtureWrapped(t, func(st routing.Store) routing.Store {
		return settingsFailStore{Store: st, err: errors.New("settings table unavailable")}
	})
	f.offering(t, "srv_ext", routing.ProviderStableDiffusionCpp)

	rec := f.get(ioModel)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (body=%s)", rec.Code, rec.Body.String())
	}
	if code := errorBodyOf(t, rec); code != "model.image_options_failed" {
		t.Fatalf("error code = %q, want model.image_options_failed", code)
	}
}
