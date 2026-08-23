// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/inference"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/provider"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/store"
	"op-ai-gateway/internal/usage"
	"testing"
	"time"
)

// fakePortalServerOverrideAuth is a minimal portal.API stand-in (embeds a nil
// interface, per the established internal/gateway test pattern — see
// agent_reactivation_test.go/energy_reconciler_test.go) that lets these unit
// tests control AuthorizeServerManage's verdict directly, without building a
// real routing.Store + ownership graph. It also COUNTS calls so the no-op
// invariant (an ordinary request without any server_override configured must
// never even call AuthorizeServerManage) is directly provable, not just
// inferred from the returned request.
type fakePortalServerOverrideAuth struct {
	portal.API
	calls int
	allow map[string]bool
}

func (f *fakePortalServerOverrideAuth) AuthorizeServerManage(_ context.Context, _ auth.Token, serverID string) error {
	f.calls++
	if f.allow[serverID] {
		return nil
	}
	return portal.ErrServerNotFound
}

func newServerOverrideRequest(headerID, headerForce string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	if headerID != "" {
		r.Header.Set(serverOverrideHeaderName, headerID)
	}
	if headerForce != "" {
		r.Header.Set(serverOverrideForceHeaderName, headerForce)
	}
	return r
}

// TestApplyServerOverrideNoOpWhenUnset proves the strict no-op invariant: with
// no header and no token.ServerOverride, applyServerOverride returns req
// byte-identical and — critically — never calls AuthorizeServerManage at all,
// so an ordinary request pays zero extra cost.
func TestApplyServerOverrideNoOpWhenUnset(t *testing.T) {
	fp := &fakePortalServerOverrideAuth{allow: map[string]bool{}}
	s := &Server{Portal: fp}
	req := inference.Request{Model: "some-model"}
	out, err := s.applyServerOverride(context.Background(), newServerOverrideRequest("", ""), auth.Token{ID: "tok_dev", UserID: "usr_dev"}, req)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	// inference.Request embeds a slice (Messages), so it is not comparable via
	// != -- applyServerOverride only ever touches ServerOverrideID/
	// ServerOverrideForceUnreachable, so asserting those two are unchanged is
	// exactly equivalent to "req returned unchanged" for this function.
	if out.Model != req.Model || out.ServerOverrideID != "" || out.ServerOverrideForceUnreachable {
		t.Fatalf("req mutated: got %+v, want unchanged %+v", out, req)
	}
	if fp.calls != 0 {
		t.Fatalf("AuthorizeServerManage calls = %d, want 0 (no-op)", fp.calls)
	}
}

// TestApplyServerOverrideTokenOverrideApplied proves a token-configured
// override is re-authorized and, on success, stamped onto req.
func TestApplyServerOverrideTokenOverrideApplied(t *testing.T) {
	fp := &fakePortalServerOverrideAuth{allow: map[string]bool{"srv_managed": true}}
	s := &Server{Portal: fp}
	token := auth.Token{ID: "tok_dev", UserID: "usr_dev", ServerOverride: "srv_managed", ServerOverrideForceUnreachable: true}
	out, err := s.applyServerOverride(context.Background(), newServerOverrideRequest("", ""), token, inference.Request{Model: "m"})
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if out.ServerOverrideID != "srv_managed" {
		t.Fatalf("ServerOverrideID = %q, want srv_managed", out.ServerOverrideID)
	}
	if !out.ServerOverrideForceUnreachable {
		t.Fatal("ServerOverrideForceUnreachable = false, want true (copied from token)")
	}
	if fp.calls != 1 {
		t.Fatalf("AuthorizeServerManage calls = %d, want 1", fp.calls)
	}
}

// TestApplyServerOverrideTokenTakesPrecedenceOverHeader proves the token's own
// configured override wins over an explicit chat header when both are set —
// the documented (post-flip) precedence: a run-as token's server override is a
// deliberate pin that governs the request, and the chat's own override is
// ignored whenever the effective token carries one of its own.
func TestApplyServerOverrideTokenTakesPrecedenceOverHeader(t *testing.T) {
	fp := &fakePortalServerOverrideAuth{allow: map[string]bool{"srv_from_header": true, "srv_from_token": true}}
	s := &Server{Portal: fp}
	token := auth.Token{ID: "tok_dev", UserID: "usr_dev", ServerOverride: "srv_from_token", ServerOverrideForceUnreachable: false}
	out, err := s.applyServerOverride(context.Background(), newServerOverrideRequest("srv_from_header", ""), token, inference.Request{Model: "m"})
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if out.ServerOverrideID != "srv_from_token" {
		t.Fatalf("ServerOverrideID = %q, want srv_from_token (token must win)", out.ServerOverrideID)
	}
}

// TestApplyServerOverrideHeaderForceFlag proves the explicit chat
// X-OP-Server-Override-Force: 1 header sets ServerOverrideForceUnreachable
// even when the token's own force flag is false.
func TestApplyServerOverrideHeaderForceFlag(t *testing.T) {
	fp := &fakePortalServerOverrideAuth{allow: map[string]bool{"srv_from_header": true}}
	s := &Server{Portal: fp}
	token := auth.Token{ID: "tok_dev", UserID: "usr_dev", ServerOverride: "", ServerOverrideForceUnreachable: false}
	out, err := s.applyServerOverride(context.Background(), newServerOverrideRequest("srv_from_header", "1"), token, inference.Request{Model: "m"})
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if !out.ServerOverrideForceUnreachable {
		t.Fatal("ServerOverrideForceUnreachable = false, want true (from the Force header)")
	}
}

// TestApplyServerOverrideForbiddenWhenNotManageable proves the runtime
// re-authorization boundary: a caller (or its token) requesting an override
// for a server it may not manage RIGHT NOW is rejected with
// errServerOverrideForbidden and req is returned UNCHANGED (ServerOverrideID
// never stamped) — this is the actual security gate, independent of whether
// the header/token value is well-formed.
func TestApplyServerOverrideForbiddenWhenNotManageable(t *testing.T) {
	fp := &fakePortalServerOverrideAuth{allow: map[string]bool{}}
	s := &Server{Portal: fp}
	token := auth.Token{ID: "tok_dev", UserID: "usr_dev", ServerOverride: "srv_not_managed"}
	req := inference.Request{Model: "m"}
	out, err := s.applyServerOverride(context.Background(), newServerOverrideRequest("", ""), token, req)
	if err != errServerOverrideForbidden {
		t.Fatalf("err = %v, want errServerOverrideForbidden", err)
	}
	if out.Model != req.Model || out.ServerOverrideForceUnreachable {
		t.Fatalf("req mutated on forbidden: got %+v, want unchanged %+v", out, req)
	}
	if out.ServerOverrideID != "" {
		t.Fatalf("ServerOverrideID = %q on forbidden, want empty (never stamped)", out.ServerOverrideID)
	}
}

// TestApplyServerOverrideHeaderForbiddenWhenNotManageable is the header-driven
// counterpart of the above: a chat-supplied override for an unmanageable
// server is rejected exactly the same way, precedence notwithstanding.
func TestApplyServerOverrideHeaderForbiddenWhenNotManageable(t *testing.T) {
	fp := &fakePortalServerOverrideAuth{allow: map[string]bool{}}
	s := &Server{Portal: fp}
	token := auth.Token{ID: "tok_dev", UserID: "usr_dev"}
	_, err := s.applyServerOverride(context.Background(), newServerOverrideRequest("srv_not_managed", ""), token, inference.Request{Model: "m"})
	if err != errServerOverrideForbidden {
		t.Fatalf("err = %v, want errServerOverrideForbidden", err)
	}
}

// --- End-to-end gateway HTTP wiring -----------------------------------------
//
// The suite below drives real HTTP requests through handleOpenAIChat and
// tryProxyNative against a REAL routing.MemoryStore + portal.Service (no fake
// Portal — this exercises the genuine AuthorizeServerManage ownership gate),
// proving the injector is wired at each of the required call sites and that
// the routing.Resolver's server-override branch (a distinct, sentinel-erroring
// code path unreachable via normal routing) is actually reached.

// serverOverrideGatewayEnv seeds five AI-servers, all via app+mapping api
// flavors "openai"/"anthropic", for exercising applyServerOverride's runtime
// re-authorization at the gateway HTTP layer. Every server except
// srvModelGap offers gatewayModel "override-model"; srvModelGap offers a
// DIFFERENT model, so overriding onto it for "override-model" is a live
// server with no matching mapping (routing.ErrServerOverrideModelUnavailable).
type serverOverrideGatewayEnv struct {
	srv *Server

	srvHealthy   string // active, healthy, owned by usr_dev, offers override-model
	srvDisabled  string // status=disabled, owned by usr_dev, offers override-model
	srvUnhealthy string // active, health=unhealthy, owned by usr_dev, offers override-model
	// srvUnowned starts OWNED by usr_dev (so a test can legitimately configure
	// a token override onto it via portal.Service.UpdateToken's own write-time
	// self-heal), then a test revokes ownership afterward -- simulating a
	// token that OUTLIVES the server-management grant that set its override,
	// which is exactly what applyServerOverride's runtime re-check exists for.
	srvUnowned  string
	srvModelGap string // active, healthy, owned by usr_dev, offers a DIFFERENT model
}

func newServerOverrideGatewayEnv(t *testing.T) *serverOverrideGatewayEnv {
	t.Helper()
	ctx := context.Background()
	tokens := auth.NewTokenStore()
	directory := portal.NewMemoryDirectory(tokens)
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	// Role "user" (NOT admin/system_admin): AuthorizeServerManage can ONLY
	// succeed via ServerOwners for this principal, so these tests exercise
	// the real ownership gate rather than a role-scope bypass.
	directory.AddUser(store.User{ID: "usr_dev", Email: "dev@example.test", DisplayName: "Dev User", Role: "user", Status: store.UserStatusActive, PreferredLanguage: "de", CreatedAt: now, UpdatedAt: now})
	if err := directory.CreatePlainToken(ctx, store.TokenRecord{ID: "tok_dev", UserID: "usr_dev", Name: "Dev Token", Status: store.TokenStatusActive, Scopes: `["gateway:use"]`, CreatedAt: now, UpdatedAt: now}, "dev-secret"); err != nil {
		t.Fatalf("create token: %v", err)
	}

	routeStore := routing.NewMemoryStore()
	seed := func(serverID, appID, mapID, status, health, model string, owned bool) {
		if err := routeStore.CreateAIServer(ctx, routing.AIServer{ID: serverID, Name: serverID, Domain: serverID + ".example.test", Provider: routing.ProviderMock, Endpoint: "mock://" + serverID, Status: status, HealthStatus: health, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("create server %s: %v", serverID, err)
		}
		if err := routeStore.CreateApplication(ctx, routing.Application{ID: appID, ServerID: serverID, Type: routing.ProviderMock, Port: 8100, Scheme: "http", APIFlavors: []string{routing.APIFlavorOpenAI, routing.APIFlavorAnthropic}, Priority: 10, Weight: 50, TimeoutMS: 30000, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("create app %s: %v", appID, err)
		}
		if err := routeStore.CreateMapping(ctx, routing.ModelMapping{ID: mapID, ApplicationID: appID, GatewayModelName: model, AppModelName: model, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("create mapping %s: %v", mapID, err)
		}
		if owned {
			if err := routeStore.SetServerOwners(ctx, serverID, []string{"usr_dev"}); err != nil {
				t.Fatalf("set owners %s: %v", serverID, err)
			}
		}
	}

	env := &serverOverrideGatewayEnv{
		srvHealthy:   "srv_override_healthy",
		srvDisabled:  "srv_override_disabled",
		srvUnhealthy: "srv_override_unhealthy",
		srvUnowned:   "srv_override_unowned",
		srvModelGap:  "srv_override_model_gap",
	}
	seed(env.srvHealthy, "app_override_healthy", "map_override_healthy", routing.ServerStatusActive, routing.HealthHealthy, "override-model", true)
	seed(env.srvDisabled, "app_override_disabled", "map_override_disabled", routing.ServerStatusDisabled, routing.HealthHealthy, "override-model", true)
	seed(env.srvUnhealthy, "app_override_unhealthy", "map_override_unhealthy", routing.ServerStatusActive, routing.HealthUnhealthy, "override-model", true)
	seed(env.srvUnowned, "app_override_unowned", "map_override_unowned", routing.ServerStatusActive, routing.HealthHealthy, "override-model", true)
	seed(env.srvModelGap, "app_override_gap", "map_override_gap", routing.ServerStatusActive, routing.HealthHealthy, "other-model", true)

	recorder := usage.NewRecorder()
	env.srv = New(ServerDeps{
		Tokens:   tokens,
		Usage:    recorder,
		Provider: provider.NewMock(),
		Routes:   routeStore,
		Portal:   portal.NewService(portal.ServiceDeps{Users: directory, Tokens: directory, Usage: recorder, Routes: routeStore, Clock: func() time.Time { return now }, ModelLister: provider.NewMock()}),
	})
	return env
}

// setTokenServerOverride sets the token's ServerOverride/ForceUnreachable via
// the REAL portal.Service.UpdateToken path (so its own write-time self-heal,
// validateServerOverride, applies exactly as it does in production) —
// distinct from the gateway's own RUNTIME re-check under test
// (applyServerOverride), which every test below still exercises when the
// request is fired.
func setTokenServerOverride(t *testing.T, s *Server, tokenID, serverID string, force bool) {
	t.Helper()
	owner := auth.Token{UserID: "usr_dev"}
	if _, err := s.Portal.UpdateToken(context.Background(), owner, tokenID, portal.UpdateTokenRequest{
		ServerOverride:                 &serverID,
		ServerOverrideForceUnreachable: &force,
	}); err != nil {
		t.Fatalf("seed token server_override: %v", err)
	}
}

func chatCompletionsRequestWithOverride(model, headerID, headerForce string) *http.Request {
	body := `{"model":"` + model + `","messages":[{"role":"user","content":"hi"}]}`
	req := newJSONRequest(http.MethodPost, "/v1/chat/completions", body)
	if headerID != "" {
		req.Header.Set(serverOverrideHeaderName, headerID)
	}
	if headerForce != "" {
		req.Header.Set(serverOverrideForceHeaderName, headerForce)
	}
	return req
}

func errorBodyOf(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal error body: %v, body=%s", err, rec.Body.String())
	}
	return body.Error.Code
}

// TestGatewayServerOverrideTokenOutlivedGrantForbidden403s proves the runtime
// re-authorization boundary end-to-end through handleOpenAIChat: a token's
// STORED server_override was valid when set (usr_dev owned the server), but
// usr_dev subsequently lost management of it — the gateway's own re-check
// (not the write-time self-heal) must catch this and 403, never routing.
func TestGatewayServerOverrideTokenOutlivedGrantForbidden403s(t *testing.T) {
	env := newServerOverrideGatewayEnv(t)
	setTokenServerOverride(t, env.srv, "tok_dev", env.srvUnowned, false)
	// Simulate the token outliving the management grant: usr_dev loses
	// ownership of the server AFTER the override was legitimately configured.
	if err := env.srv.Routes.SetServerOwners(context.Background(), env.srvUnowned, []string{}); err != nil {
		t.Fatalf("revoke owners: %v", err)
	}

	rec := httptest.NewRecorder()
	env.srv.ServeHTTP(rec, chatCompletionsRequestWithOverride("override-model", "", ""))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if code := errorBodyOf(t, rec); code != "server_override.forbidden" {
		t.Fatalf("error code = %q, want server_override.forbidden", code)
	}
}

// TestGatewayServerOverrideRoutesToDisabledServerSentinel proves the injector
// actually reaches routing.Resolver's server-override branch: overriding to a
// DISABLED (but owned) server yields the DISTINCT
// routing.ErrServerOverrideServerUnavailable sentinel (502
// server_override.server_unavailable) — a code path only reachable when
// req.ServerOverrideID was genuinely stamped and consumed by the resolver.
func TestGatewayServerOverrideRoutesToDisabledServerSentinel(t *testing.T) {
	env := newServerOverrideGatewayEnv(t)
	setTokenServerOverride(t, env.srv, "tok_dev", env.srvDisabled, false)

	rec := httptest.NewRecorder()
	env.srv.ServeHTTP(rec, chatCompletionsRequestWithOverride("override-model", "", ""))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if code := errorBodyOf(t, rec); code != "server_override.server_unavailable" {
		t.Fatalf("error code = %q, want server_override.server_unavailable", code)
	}
}

// TestGatewayServerOverrideModelGapReturns404 proves the model_unavailable
// sentinel maps to 404: the target server exists + is manageable + is
// healthy, but has no live mapping for the requested model.
func TestGatewayServerOverrideModelGapReturns404(t *testing.T) {
	env := newServerOverrideGatewayEnv(t)
	setTokenServerOverride(t, env.srv, "tok_dev", env.srvModelGap, false)

	rec := httptest.NewRecorder()
	env.srv.ServeHTTP(rec, chatCompletionsRequestWithOverride("override-model", "", ""))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if code := errorBodyOf(t, rec); code != "server_override.model_unavailable" {
		t.Fatalf("error code = %q, want server_override.model_unavailable", code)
	}
}

// TestGatewayServerOverrideTokenWinsOverChatHeader proves the documented
// (post-flip) precedence end-to-end: the run-as token's own override points
// at the DISABLED, manageable server, while an explicit chat header overrides
// to a DIFFERENT, HEALTHY, manageable server (which would succeed on its
// own) -- the response must reflect the TOKEN's target (the disabled-server
// sentinel), proving the token governs and the chat header is ignored
// whenever the effective token carries an override of its own.
func TestGatewayServerOverrideTokenWinsOverChatHeader(t *testing.T) {
	env := newServerOverrideGatewayEnv(t)
	setTokenServerOverride(t, env.srv, "tok_dev", env.srvDisabled, false)

	rec := httptest.NewRecorder()
	env.srv.ServeHTTP(rec, chatCompletionsRequestWithOverride("override-model", env.srvHealthy, ""))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, body = %s (want the disabled-server sentinel, proving the token won)", rec.Code, rec.Body.String())
	}
	if code := errorBodyOf(t, rec); code != "server_override.server_unavailable" {
		t.Fatalf("error code = %q, want server_override.server_unavailable", code)
	}
}

// TestGatewayServerOverrideForceFollowsSource proves force is resolved
// TOGETHER with id, from whichever single source (token or chat header)
// governs -- never mixed across sources. (i) When the token carries an
// override, the token governs BOTH id and force even though the chat header
// explicitly targets a DIFFERENT server with Force:0 -- the pre-flip OR-based
// force computation (`header=="1" || (id==token.ServerOverride &&
// token.Force)`) could leak the token's force flag onto a header-chosen id;
// the post-flip resolution instead ties id and force to the SAME winning
// source atomically, so this proves the token's force applies to the
// token's OWN id, not the header's. (ii) With NO token override at all, the
// chat header is the sole source for BOTH id and force -- proving the
// fallback branch still resolves force from the SAME source as id, with no
// token contamination.
func TestGatewayServerOverrideForceFollowsSource(t *testing.T) {
	env := newServerOverrideGatewayEnv(t)

	// (i) token source: id=srvUnhealthy (force-bypassable), force=true from
	// the token, while the chat header targets the DISABLED server (which
	// errors unconditionally, force or not) with an explicit Force:0. A 200
	// here is possible ONLY if id resolved to the token's srvUnhealthy AND
	// force resolved to the token's own true -- either the header's id or
	// the header's Force:0 leaking in would prevent success.
	setTokenServerOverride(t, env.srv, "tok_dev", env.srvUnhealthy, true)
	rec := httptest.NewRecorder()
	env.srv.ServeHTTP(rec, chatCompletionsRequestWithOverride("override-model", env.srvDisabled, "0"))
	if rec.Code != http.StatusOK {
		t.Fatalf("token-sourced id+force: status = %d, body = %s, want 200 (token id=%s force=true must win over header id=%s force=0)", rec.Code, rec.Body.String(), env.srvUnhealthy, env.srvDisabled)
	}

	// (ii) no token override: the chat header is the sole source for BOTH id
	// and force. It targets the SAME srvUnhealthy, but this time with
	// Force:1 -- succeeds only because the header's OWN force applies, with
	// no token value present to contaminate it.
	setTokenServerOverride(t, env.srv, "tok_dev", "", false)
	rec = httptest.NewRecorder()
	env.srv.ServeHTTP(rec, chatCompletionsRequestWithOverride("override-model", env.srvUnhealthy, "1"))
	if rec.Code != http.StatusOK {
		t.Fatalf("header-sourced id+force: status = %d, body = %s, want 200 (header id+force must apply with no token override)", rec.Code, rec.Body.String())
	}
}

// TestGatewayServerOverrideForceHeaderBypassesUnhealthy proves the Force
// header flips an unhealthy-but-owned server from rejected to routable: the
// SAME override target (srvUnhealthy) 502s without the force header and
// succeeds (200) with it.
func TestGatewayServerOverrideForceHeaderBypassesUnhealthy(t *testing.T) {
	env := newServerOverrideGatewayEnv(t)

	rec := httptest.NewRecorder()
	env.srv.ServeHTTP(rec, chatCompletionsRequestWithOverride("override-model", env.srvUnhealthy, ""))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("without force: status = %d, body = %s, want 502", rec.Code, rec.Body.String())
	}
	if code := errorBodyOf(t, rec); code != "server_override.server_unavailable" {
		t.Fatalf("without force: error code = %q, want server_override.server_unavailable", code)
	}

	rec = httptest.NewRecorder()
	env.srv.ServeHTTP(rec, chatCompletionsRequestWithOverride("override-model", env.srvUnhealthy, "1"))
	if rec.Code != http.StatusOK {
		t.Fatalf("with force: status = %d, body = %s, want 200", rec.Code, rec.Body.String())
	}
}

// TestGatewayNativePassthroughServerOverrideForbidden403s proves the injector
// is ALSO wired into tryProxyNative (the native-passthrough path used by
// /v1/responses and /v1/messages): an outlived-grant override 403s there too,
// without ever falling through to the translate path.
func TestGatewayNativePassthroughServerOverrideForbidden403s(t *testing.T) {
	env := newServerOverrideGatewayEnv(t)
	setTokenServerOverride(t, env.srv, "tok_dev", env.srvUnowned, false)
	if err := env.srv.Routes.SetServerOwners(context.Background(), env.srvUnowned, []string{}); err != nil {
		t.Fatalf("revoke owners: %v", err)
	}

	body := `{"model":"override-model","input":[{"role":"user","content":[{"type":"input_text","text":"hi"}]}]}`
	req := newJSONRequest(http.MethodPost, "/v1/responses", body)
	rec := httptest.NewRecorder()
	env.srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if code := errorBodyOf(t, rec); code != "server_override.forbidden" {
		t.Fatalf("error code = %q, want server_override.forbidden", code)
	}
}
