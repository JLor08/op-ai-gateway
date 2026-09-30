// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/inference"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/provider"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/store"
	"op-ai-gateway/internal/usage"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	rrOwnerSecret   = "rr-owner-secret"
	rrOtherSecret   = "rr-other-secret"
	rrAdminSecret   = "rr-admin-secret"
	rrManagerSecret = "rr-manager-secret"
	rrServerID      = "srv_rr"
	rrAppID         = "app_rr"
	rrMappingID     = "map_rr"
	rrModel         = "rr/model"
	rrAppModel      = "up-rr"
	rrSpecID        = "rspec_rr"
)

var rrNow = time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)

// rowRefusalProvider completes every Load at once: a one-token stream for a
// Load that generates, and a successful ensure for one that does not.
type rowRefusalProvider struct{}

func (rowRefusalProvider) Complete(context.Context, routing.Target, inference.Request) (provider.Response, error) {
	return provider.Response{}, nil
}

func (rowRefusalProvider) CompleteStream(_ context.Context, _ routing.Target, _ inference.Request, emit provider.StreamEmit) error {
	if err := emit(inference.StreamEvent{Type: inference.StreamEventTextDelta, Text: "ok"}); err != nil {
		return err
	}
	return emit(inference.StreamEvent{Type: inference.StreamEventCompleted, Usage: &inference.Usage{OutputTokens: 1}})
}

func (rowRefusalProvider) EnsureRuntimeModel(context.Context, routing.Target) error {
	return nil
}

// newRowRefusalFixture builds a *Server with one active server owned by the
// rrOwnerSecret principal and linked to an admin group the rrManagerSecret
// principal owns, one application of appType declaring flavors, and one
// mapping offering rrModel. rrOtherSecret's principal owns and manages
// nothing, and rrAdminSecret's is a plain admin (the admin scope, no system
// scope) who neither owns nor manages the server.
func newRowRefusalFixture(t *testing.T, appType string, flavors []string) *Server {
	t.Helper()
	ctx := context.Background()
	tokens := auth.NewTokenStore()
	dir := portal.NewMemoryDirectory(tokens)
	for _, u := range []struct{ id, secret, role, scopes string }{
		{"usr_rr", rrOwnerSecret, "user", `["gateway:use"]`},
		{"usr_rr_other", rrOtherSecret, "user", `["gateway:use"]`},
		{"usr_rr_admin", rrAdminSecret, "admin", `["gateway:use","admin"]`},
		{"usr_rr_manager", rrManagerSecret, "admin", `["gateway:use","admin"]`},
	} {
		dir.AddUser(store.User{ID: u.id, Email: u.id + "@example.test", DisplayName: u.id, Role: u.role, Status: store.UserStatusActive, PreferredLanguage: "de", CreatedAt: rrNow, UpdatedAt: rrNow})
		if err := dir.CreatePlainToken(ctx, store.TokenRecord{ID: "tok_" + u.id, UserID: u.id, Name: u.id, Status: store.TokenStatusActive, Scopes: u.scopes, CreatedAt: rrNow, UpdatedAt: rrNow}, u.secret); err != nil {
			t.Fatalf("CreatePlainToken %s: %v", u.id, err)
		}
	}
	if err := dir.CreateUserGroup(ctx, store.UserGroup{ID: "ugrp_rr_sys", Tier: store.GroupTierSystem, Name: "RR System", CreatedAt: rrNow, UpdatedAt: rrNow}); err != nil {
		t.Fatalf("CreateUserGroup(system): %v", err)
	}
	if err := dir.CreateUserGroup(ctx, store.UserGroup{ID: "ugrp_rr_admin", Tier: store.GroupTierAdmin, Name: "RR Admin", ParentGroupID: "ugrp_rr_sys", OwnerUserID: "usr_rr_manager", CreatedAt: rrNow, UpdatedAt: rrNow}); err != nil {
		t.Fatalf("CreateUserGroup(admin): %v", err)
	}
	if err := dir.SetUserGroupMember(ctx, "ugrp_rr_admin", "usr_rr_manager", store.GroupStateMember, ""); err != nil {
		t.Fatalf("SetUserGroupMember: %v", err)
	}
	routeStore := routing.NewMemoryStore()
	if err := routeStore.CreateAIServer(ctx, routing.AIServer{ID: rrServerID, Name: "RR Host", Domain: "rr.example.test", Provider: routing.ProviderMock, Endpoint: "mock://rr", Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: rrNow, UpdatedAt: rrNow}); err != nil {
		t.Fatalf("CreateAIServer: %v", err)
	}
	if err := routeStore.SetServerOwners(ctx, rrServerID, []string{"usr_rr"}); err != nil {
		t.Fatalf("SetServerOwners: %v", err)
	}
	if err := routeStore.SetServerAdminGroup(ctx, rrServerID, "ugrp_rr_admin"); err != nil {
		t.Fatalf("SetServerAdminGroup: %v", err)
	}
	if err := routeStore.CreateApplication(ctx, routing.Application{ID: rrAppID, ServerID: rrServerID, Type: appType, Port: 8000, Scheme: "http", APIFlavors: flavors, TimeoutMS: 30000, Status: routing.ServerStatusActive, CreatedAt: rrNow, UpdatedAt: rrNow}); err != nil {
		t.Fatalf("CreateApplication: %v", err)
	}
	if err := routeStore.CreateMapping(ctx, routing.ModelMapping{ID: rrMappingID, ApplicationID: rrAppID, GatewayModelName: rrModel, AppModelName: rrAppModel, Status: routing.ServerStatusActive, CreatedAt: rrNow, UpdatedAt: rrNow}); err != nil {
		t.Fatalf("CreateMapping: %v", err)
	}
	reg := NewLoadedModelRegistry()
	recorder := usage.NewRecorder()
	svc := portal.NewService(portal.ServiceDeps{Users: dir, Tokens: dir, Groups: dir, Usage: recorder, Routes: routeStore, LoadedModels: reg})
	s := New(ServerDeps{Tokens: tokens, Usage: recorder, Routes: routeStore, Portal: svc, LoadedModels: reg})
	s.Provider = rowRefusalProvider{}
	return s
}

// putRowRefusalSpec stores rrMappingID's runtime spec with flavors and
// adminState.
func putRowRefusalSpec(t *testing.T, s *Server, flavors []string, adminState string) {
	t.Helper()
	if err := s.Routes.UpsertRuntimeSpec(context.Background(), routing.RuntimeSpec{
		ID: rrSpecID, MappingID: rrMappingID, Enabled: true, Binary: "/usr/bin/model-server", Args: "[]", Env: "{}",
		APIFlavors: flavors, AdminState: adminState, CreatedAt: rrNow, UpdatedAt: rrNow,
	}); err != nil {
		t.Fatalf("UpsertRuntimeSpec: %v", err)
	}
}

// rowRefusalListRow lists rrModel's servers as secret's principal and returns
// the one row with the raw response body.
func rowRefusalListRow(t *testing.T, s *Server, secret string) (portal.ModelServerDTO, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/portal/model-servers?name="+url.QueryEscape(rrModel), nil)
	req.Header.Set("Authorization", "Bearer "+secret)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		Data []portal.ModelServerDTO `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body.String())
	}
	if len(out.Data) != 1 {
		t.Fatalf("len(data) = %d, want 1 (%+v)", len(out.Data), out.Data)
	}
	return out.Data[0], rec.Body.String()
}

// postRowRefusalLoad starts a Load of rrMappingID as secret's principal and
// returns the status and the error code ("" for a 202). An admitted Load is
// waited out, so no run outlives the test.
func postRowRefusalLoad(t *testing.T, s *Server, secret string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/portal/mappings/"+rrMappingID+"/load", nil)
	req.Header.Set("Authorization", "Bearer "+secret)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		return rec.Code, errorCodeOf(t, rec.Body.Bytes())
	}
	deadline := time.Now().Add(10 * time.Second)
	for s.Benchmarks.ServerBusy(rrServerID) {
		if time.Now().After(deadline) {
			t.Fatal("the admitted Load did not finish within 10s")
		}
		time.Sleep(5 * time.Millisecond)
	}
	return rec.Code, ""
}

// TestModelServersLoadRefusalAgreesWithTheLoadStarter pins load_refusal per
// reason and, for every case, that the Load starter answers exactly what the
// row predicts: 409 with "benchmark." + the reason, or 202 when the row has
// none. No case publishes a runtime status, so the agent rows are rows before
// any published status.
func TestModelServersLoadRefusalAgreesWithTheLoadStarter(t *testing.T) {
	text := []string{routing.APIFlavorOpenAI}
	images := []string{routing.APIFlavorOpenAIImages}
	both := []string{routing.APIFlavorOpenAI, routing.APIFlavorOpenAIImages}
	type specCase struct {
		flavors    []string
		adminState string
	}
	cases := []struct {
		name       string
		appType    string
		appFlavors []string
		spec       *specCase
		ensure     bool
		want       string
	}{
		{name: "non-agent images-only row without a spec", appType: routing.ProviderStableDiffusionCpp, appFlavors: images, want: "images_only"},
		{name: "agent images-only spec, agent without runtime_ensure", appType: routing.ProviderServerAgent, appFlavors: both, spec: &specCase{flavors: images}, want: "agent_ensure_unsupported"},
		{name: "agent images-only application without a spec, agent without runtime_ensure", appType: routing.ProviderServerAgent, appFlavors: images, want: "agent_ensure_unsupported"},
		{name: "agent images-only spec, agent with runtime_ensure", appType: routing.ProviderServerAgent, appFlavors: both, spec: &specCase{flavors: images}, ensure: true, want: ""},
		{name: "force-stopped agent text spec", appType: routing.ProviderServerAgent, appFlavors: text, spec: &specCase{flavors: text, adminState: "force_stopped"}, ensure: true, want: "spec_force_stopped"},
		{name: "force-stopped agent images-only spec with runtime_ensure", appType: routing.ProviderServerAgent, appFlavors: both, spec: &specCase{flavors: images, adminState: "force_stopped"}, ensure: true, want: "spec_force_stopped"},
		{name: "force-stopped agent images-only spec without runtime_ensure", appType: routing.ProviderServerAgent, appFlavors: both, spec: &specCase{flavors: images, adminState: "force_stopped"}, want: "agent_ensure_unsupported"},
		{name: "agent text spec", appType: routing.ProviderServerAgent, appFlavors: both, spec: &specCase{flavors: text}, want: ""},
		{name: "non-agent text row", appType: routing.ProviderVLLM, appFlavors: text, want: ""},
		{name: "non-agent text row with a force-stopped spec row", appType: routing.ProviderVLLM, appFlavors: text, spec: &specCase{flavors: images, adminState: "force_stopped"}, want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newRowRefusalFixture(t, tc.appType, tc.appFlavors)
			if tc.spec != nil {
				putRowRefusalSpec(t, s, tc.spec.flavors, tc.spec.adminState)
			}
			if tc.ensure {
				s.AgentFeatures.Set(rrServerID, []string{runtimeEnsureFeature})
			}
			assertRowAgreesWithTheLoadStarter(t, s, tc.want)
		})
	}
}

// assertRowAgreesWithTheLoadStarter checks, as the owner, that the row carries
// want as load_refusal -- in the raw body too, which pins the wire name, since
// a decoded DTO shares its tag and cannot see a rename -- and that the Load
// starter answers what the row predicts: 409 "benchmark." + want, or 202 when
// want is "" and the row omits the key.
func assertRowAgreesWithTheLoadStarter(t *testing.T, s *Server, want string) {
	t.Helper()
	row, body := rowRefusalListRow(t, s, rrOwnerSecret)
	if row.LoadRefusal != want {
		t.Fatalf("row load_refusal = %q, want %q", row.LoadRefusal, want)
	}
	status, code := postRowRefusalLoad(t, s, rrOwnerSecret)
	if want == "" {
		if strings.Contains(body, `"load_refusal"`) {
			t.Fatalf("a row without a reason must omit load_refusal, body=%s", body)
		}
		if status != http.StatusAccepted {
			t.Fatalf("Load status = %d (%s), want 202: the row predicted no refusal", status, code)
		}
		return
	}
	if !strings.Contains(body, `"load_refusal":"`+want+`"`) {
		t.Fatalf("body = %s, want the key load_refusal carrying %q", body, want)
	}
	if status != http.StatusConflict || code != "benchmark."+want {
		t.Fatalf("Load = %d %q, want 409 %q: the row and the starter disagree", status, code, "benchmark."+want)
	}
}

// TestModelServersLoadRefusalIsEmptyWhileTheServerIsBusy: while a run holds
// the server the row carries no reason, and the starter answers
// benchmark.already_running instead of the images-only refusal.
func TestModelServersLoadRefusalIsEmptyWhileTheServerIsBusy(t *testing.T) {
	s := newRowRefusalFixture(t, routing.ProviderStableDiffusionCpp, []string{routing.APIFlavorOpenAIImages})
	if _, ok := s.Benchmarks.TryStart(rrServerID, "vram-probe", "vram", 1, time.Now().UTC(), func() {}); !ok {
		t.Fatal("TryStart failed")
	}
	defer s.Benchmarks.Release(rrServerID)
	if row, _ := rowRefusalListRow(t, s, rrOwnerSecret); row.LoadRefusal != "" {
		t.Fatalf("row load_refusal while busy = %q, want empty", row.LoadRefusal)
	}
	if status, code := postRowRefusalLoad(t, s, rrOwnerSecret); status != http.StatusConflict || code != codeBenchmarkAlreadyRunning {
		t.Fatalf("Load while busy = %d %q, want 409 %q", status, code, codeBenchmarkAlreadyRunning)
	}
}

// TestModelServersLoadRefusalIsEmptyForARowTheCallerCannotLoad: a principal
// who may not Load the mapping gets can_load false and no reason on its row,
// and the starter answers 404. That holds for a plain admin too: the admin
// scope alone is not the starter's authorization, so the row does not tell
// them about a spec's admin override either.
func TestModelServersLoadRefusalIsEmptyForARowTheCallerCannotLoad(t *testing.T) {
	for _, principal := range []struct{ name, secret string }{
		{"non-owner", rrOtherSecret},
		{"plain admin", rrAdminSecret},
	} {
		t.Run(principal.name, func(t *testing.T) {
			s := newRowRefusalFixture(t, routing.ProviderServerAgent, []string{routing.APIFlavorOpenAI})
			putRowRefusalSpec(t, s, []string{routing.APIFlavorOpenAI}, "force_stopped")
			if row, _ := rowRefusalListRow(t, s, rrOwnerSecret); row.LoadRefusal != "spec_force_stopped" {
				t.Fatalf("owner row load_refusal = %q, want spec_force_stopped (the fixture's refusal)", row.LoadRefusal)
			}
			row, body := rowRefusalListRow(t, s, principal.secret)
			if row.CanLoad || row.LoadRefusal != "" || strings.Contains(body, `"load_refusal"`) {
				t.Fatalf("row = (can_load=%v, load_refusal=%q), want (false, \"\") with no load_refusal key; body=%s", row.CanLoad, row.LoadRefusal, body)
			}
			if status, code := postRowRefusalLoad(t, s, principal.secret); status != http.StatusNotFound {
				t.Fatalf("Load = %d %q, want 404", status, code)
			}
		})
	}
}

// TestModelServersLoadRefusalForAServerManager: a can_manage_servers manager
// of the server's admin group -- here its owner -- is authorized by the Load
// starter although they do not own the server, so the row says can_load true,
// carries the reason the starter answers, and a row without one Loads.
func TestModelServersLoadRefusalForAServerManager(t *testing.T) {
	for _, tc := range []struct {
		name    string
		appType string
		flavors []string
		want    string
	}{
		{"refused", routing.ProviderStableDiffusionCpp, []string{routing.APIFlavorOpenAIImages}, "images_only"},
		{"admitted", routing.ProviderVLLM, []string{routing.APIFlavorOpenAI}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newRowRefusalFixture(t, tc.appType, tc.flavors)
			row, _ := rowRefusalListRow(t, s, rrManagerSecret)
			if !row.CanLoad || row.LoadRefusal != tc.want {
				t.Fatalf("manager row = (can_load=%v, load_refusal=%q), want (true, %q)", row.CanLoad, row.LoadRefusal, tc.want)
			}
			wantStatus, wantCode := http.StatusAccepted, ""
			if tc.want != "" {
				wantStatus, wantCode = http.StatusConflict, "benchmark."+tc.want
			}
			if status, code := postRowRefusalLoad(t, s, rrManagerSecret); status != wantStatus || code != wantCode {
				t.Fatalf("manager Load = %d %q, want %d %q", status, code, wantStatus, wantCode)
			}
		})
	}
}

// TestModelServersLoadRefusalWithNilRuntimeStatus: a gateway that has never
// received a runtime-status frame (RuntimeStatus nil, e.g. right after
// start) still computes LoadRefusal -- the computation reads the row's
// application and runtime spec, never the status registry -- so restoring
// injectRuntimeModelState's old "s.RuntimeStatus == nil" early return, which
// skipped the whole fill, would silently drop this refusal instead.
func TestModelServersLoadRefusalWithNilRuntimeStatus(t *testing.T) {
	s := newRowRefusalFixture(t, routing.ProviderServerAgent, []string{routing.APIFlavorOpenAI, routing.APIFlavorOpenAIImages})
	putRowRefusalSpec(t, s, []string{routing.APIFlavorOpenAIImages}, "")
	s.RuntimeStatus = nil
	row, _ := rowRefusalListRow(t, s, rrOwnerSecret)
	if row.LoadRefusal != "agent_ensure_unsupported" {
		t.Fatalf("row load_refusal with nil RuntimeStatus = %q, want agent_ensure_unsupported", row.LoadRefusal)
	}
}

// TestModelServersEventsCarryTheLoadRefusal: the SSE snapshot carries the
// same load_refusal as the plain GET.
func TestModelServersEventsCarryTheLoadRefusal(t *testing.T) {
	s := newRowRefusalFixture(t, routing.ProviderStableDiffusionCpp, []string{routing.APIFlavorOpenAIImages})
	ts := httptest.NewServer(s)
	defer ts.Close()
	req, err := http.NewRequest(http.MethodGet, ts.URL+"/api/portal/model-servers/events?name="+url.QueryEscape(rrModel), nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+rrOwnerSecret)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	event, data := readPerfSSEFrame(t, bufio.NewReader(resp.Body), 3*time.Second)
	if event != "snapshot" {
		t.Fatalf("first event = %q, want snapshot", event)
	}
	var snap struct {
		Data []portal.ModelServerDTO `json:"data"`
	}
	if err := json.Unmarshal([]byte(data), &snap); err != nil {
		t.Fatalf("unmarshal snapshot: %v (%s)", err, data)
	}
	if len(snap.Data) != 1 || snap.Data[0].LoadRefusal != "images_only" {
		t.Fatalf("snapshot data = %+v, want one row with load_refusal images_only", snap.Data)
	}
	if !strings.Contains(data, `"load_refusal":"images_only"`) {
		t.Fatalf("snapshot data = %s, want the key load_refusal carrying images_only", data)
	}
}

// rowRefusalRoutes wraps a routing.Store and instruments the two reads the
// load-refusal computation makes: it counts ApplicationByID per application
// and RuntimeSpecByMapping per mapping and, with appErr or specErr set, fails
// them. Every other method is the wrapped store's own.
type rowRefusalRoutes struct {
	routing.Store
	appErr    error
	specErr   error
	mu        sync.Mutex
	appReads  map[string]int
	specReads map[string]int
}

func wrapRowRefusalRoutes(s *Server) *rowRefusalRoutes {
	w := &rowRefusalRoutes{Store: s.Routes, appReads: map[string]int{}, specReads: map[string]int{}}
	s.Routes = w
	return w
}

func (w *rowRefusalRoutes) ApplicationByID(ctx context.Context, id string) (routing.Application, error) {
	w.mu.Lock()
	w.appReads[id]++
	w.mu.Unlock()
	// Returns the STORED application even when appErr is set (rather than a
	// zero routing.Application{}), so a guard that forgets to check the error
	// cannot coincidentally pass by acting on an empty application: an empty
	// one is never images-only, so a dropped guard would go undetected. With
	// appErr unset the wrapped store's own error comes back, so a fixture id
	// that names no application fails the read instead of reading as one.
	app, err := w.Store.ApplicationByID(ctx, id)
	if w.appErr != nil {
		return app, w.appErr
	}
	return app, err
}

func (w *rowRefusalRoutes) RuntimeSpecByMapping(ctx context.Context, mappingID string) (routing.RuntimeSpec, bool, error) {
	w.mu.Lock()
	w.specReads[mappingID]++
	w.mu.Unlock()
	if w.specErr != nil {
		return routing.RuntimeSpec{}, false, w.specErr
	}
	return w.Store.RuntimeSpecByMapping(ctx, mappingID)
}

// TestModelServersLoadRefusalReadsEachApplicationOnce: one listing reads each
// distinct application once, and each row's spec once -- the read that also
// serves the runtime-status injection.
func TestModelServersLoadRefusalReadsEachApplicationOnce(t *testing.T) {
	ctx := context.Background()
	s := newRowRefusalFixture(t, routing.ProviderStableDiffusionCpp, []string{routing.APIFlavorOpenAIImages})
	if err := s.Routes.CreateMapping(ctx, routing.ModelMapping{ID: "map_rr_2", ApplicationID: rrAppID, GatewayModelName: rrModel, AppModelName: "up-rr-2", Status: routing.ServerStatusActive, CreatedAt: rrNow, UpdatedAt: rrNow}); err != nil {
		t.Fatalf("CreateMapping: %v", err)
	}
	if err := s.Routes.CreateApplication(ctx, routing.Application{ID: "app_rr_agent", ServerID: rrServerID, Type: routing.ProviderServerAgent, Port: 9000, Scheme: "http", APIFlavors: []string{routing.APIFlavorOpenAI, routing.APIFlavorOpenAIImages}, Status: routing.ServerStatusActive, CreatedAt: rrNow, UpdatedAt: rrNow}); err != nil {
		t.Fatalf("CreateApplication: %v", err)
	}
	if err := s.Routes.CreateMapping(ctx, routing.ModelMapping{ID: "map_rr_agent", ApplicationID: "app_rr_agent", GatewayModelName: rrModel, AppModelName: "up-rr-agent", Status: routing.ServerStatusActive, CreatedAt: rrNow, UpdatedAt: rrNow}); err != nil {
		t.Fatalf("CreateMapping: %v", err)
	}
	if err := s.Routes.UpsertRuntimeSpec(ctx, routing.RuntimeSpec{ID: "rspec_rr_agent", MappingID: "map_rr_agent", Enabled: true, Binary: "/usr/bin/sd-server", Args: "[]", Env: "{}", APIFlavors: []string{routing.APIFlavorOpenAIImages}, CreatedAt: rrNow, UpdatedAt: rrNow}); err != nil {
		t.Fatalf("UpsertRuntimeSpec: %v", err)
	}
	s.RuntimeStatus.publish(rrServerID, []RuntimeStatusDTO{{SpecID: "rspec_rr_agent", Model: "up-rr-agent", State: "running"}})
	w := wrapRowRefusalRoutes(s)

	rows := []portal.ModelServerDTO{
		{ServerID: rrServerID, ApplicationID: rrAppID, MappingID: rrMappingID, CanLoad: true},
		{ServerID: rrServerID, ApplicationID: rrAppID, MappingID: "map_rr_2", CanLoad: true},
		{ServerID: rrServerID, ApplicationID: "app_rr_agent", MappingID: "map_rr_agent", CanLoad: true},
	}
	s.injectRuntimeModelState(ctx, rows)

	if w.appReads[rrAppID] != 1 || w.appReads["app_rr_agent"] != 1 {
		t.Fatalf("application reads = %v, want one per distinct application", w.appReads)
	}
	for _, id := range []string{rrMappingID, "map_rr_2", "map_rr_agent"} {
		if w.specReads[id] != 1 {
			t.Fatalf("spec reads = %v, want exactly one per row", w.specReads)
		}
	}
	if rows[0].LoadRefusal != "images_only" || rows[1].LoadRefusal != "images_only" {
		t.Fatalf("sd rows load_refusal = (%q, %q), want images_only twice", rows[0].LoadRefusal, rows[1].LoadRefusal)
	}
	if rows[2].LoadRefusal != "agent_ensure_unsupported" || rows[2].State != "running" {
		t.Fatalf("agent row = (load_refusal=%q, state=%q), want (agent_ensure_unsupported, running)", rows[2].LoadRefusal, rows[2].State)
	}
}

// TestModelServersLoadRefusalFailedSpecRead: an agent row whose spec cannot be
// read carries no reason, and the starter's own fail-closed read answers 500.
// A non-agent row needs no spec, so a failing spec read leaves its reason, and
// the starter's, unchanged.
func TestModelServersLoadRefusalFailedSpecRead(t *testing.T) {
	images := []string{routing.APIFlavorOpenAIImages}
	t.Run("agent row", func(t *testing.T) {
		s := newRowRefusalFixture(t, routing.ProviderServerAgent, images)
		wrapRowRefusalRoutes(s).specErr = errors.New("spec store unavailable")
		if row, _ := rowRefusalListRow(t, s, rrOwnerSecret); row.LoadRefusal != "" {
			t.Fatalf("row load_refusal = %q, want empty on a failed spec read", row.LoadRefusal)
		}
		logs := withCapturedSlogAtTheDefaultLevel(t)
		status, code := postRowRefusalLoad(t, s, rrOwnerSecret)
		if status != http.StatusInternalServerError || code != "benchmark.request_failed" {
			t.Fatalf("Load = %d %q, want 500 benchmark.request_failed", status, code)
		}
		logged := false
		for _, r := range logs.Snapshot() {
			if mappingID, _ := r.Attrs["mapping_id"].(string); r.Level == "WARN" && r.Msg == "benchmark: runtime spec read failed; run refused" && mappingID == rrMappingID {
				logged = true
			}
		}
		if !logged {
			t.Fatalf("no WARN naming the refused spec read of %s; records = %+v", rrMappingID, logs.Snapshot())
		}
	})
	t.Run("non-agent row", func(t *testing.T) {
		s := newRowRefusalFixture(t, routing.ProviderStableDiffusionCpp, images)
		wrapRowRefusalRoutes(s).specErr = errors.New("spec store unavailable")
		if row, _ := rowRefusalListRow(t, s, rrOwnerSecret); row.LoadRefusal != "images_only" {
			t.Fatalf("row load_refusal = %q, want images_only: a non-agent row reads no spec", row.LoadRefusal)
		}
		if status, code := postRowRefusalLoad(t, s, rrOwnerSecret); status != http.StatusConflict || code != "benchmark.images_only" {
			t.Fatalf("Load = %d %q, want 409 benchmark.images_only", status, code)
		}
	})
}

// TestModelServersLoadRefusalFailedApplicationRead: rows whose application
// cannot be read carry no reason, and the failed read is not repeated for the
// application's next row.
func TestModelServersLoadRefusalFailedApplicationRead(t *testing.T) {
	ctx := context.Background()
	s := newRowRefusalFixture(t, routing.ProviderStableDiffusionCpp, []string{routing.APIFlavorOpenAIImages})
	w := wrapRowRefusalRoutes(s)
	w.appErr = errors.New("application store unavailable")
	rows := []portal.ModelServerDTO{
		{ServerID: rrServerID, ApplicationID: rrAppID, MappingID: rrMappingID, CanLoad: true},
		{ServerID: rrServerID, ApplicationID: rrAppID, MappingID: "map_rr_2", CanLoad: true},
	}
	logs := withCapturedSlogAtTheDefaultLevel(t)
	s.injectRuntimeModelState(ctx, rows)
	if rows[0].LoadRefusal != "" || rows[1].LoadRefusal != "" {
		t.Fatalf("load_refusal = (%q, %q), want empty on a failed application read", rows[0].LoadRefusal, rows[1].LoadRefusal)
	}
	if w.appReads[rrAppID] != 1 {
		t.Fatalf("application reads = %d, want 1: a failed read is cached for the listing", w.appReads[rrAppID])
	}
	logged := false
	for _, r := range logs.Snapshot() {
		if appID, _ := r.Attrs["app_id"].(string); r.Level == "WARN" && r.Msg == "model-servers: application read failed; load_refusal left empty for its rows" && appID == rrAppID {
			logged = true
		}
	}
	if !logged {
		t.Fatalf("no WARN naming the failed application read of %s; records = %+v", rrAppID, logs.Snapshot())
	}
}
