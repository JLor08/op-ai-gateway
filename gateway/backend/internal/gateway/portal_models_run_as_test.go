// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/account"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/store"
	"op-ai-gateway/internal/usage"
	"testing"
	"time"
)

const runAsBearerSecret = "run-as-bearer-secret"

type runAsModelsWorld struct {
	srv    *Server
	dir    *portal.MemoryDirectory
	cookie *http.Cookie
}

// newRunAsModelsWorld is a session user (usr_ra) owning one vendor account
// (acc_a, "a/gpt-4o") next to the seeded local model qwen-coder, a second user
// (usr_ra_other), and a plain bearer token of usr_ra.
func newRunAsModelsWorld(t *testing.T) *runAsModelsWorld {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	ts := auth.NewTokenStore()
	dir := portal.NewMemoryDirectory(ts)
	acct := account.NewService(account.Deps{Users: dir, Sessions: dir, SetPasswordTokens: dir, SettingsVolatile: true}, account.Config{
		IdleTTL: time.Hour, MaxTTL: 24 * time.Hour, InviteTTL: 72 * time.Hour, DefaultLanguage: "de",
	})
	recorder := usage.NewRecorder()
	routeStore := routing.NewMemoryStore()
	seedGatewayTestRoutes(routeStore, now)
	settings := portal.NewMemorySystemSettings()
	if err := settings.SetSystemSetting(ctx, "vendor_accounts_enabled", "true", time.Time{}); err != nil {
		t.Fatal(err)
	}
	svc := portal.NewService(portal.ServiceDeps{Users: dir, Tokens: dir, Usage: recorder, Routes: routeStore, Groups: dir, Projects: dir, SystemSettings: settings, UIPrefs: portal.NewMemoryUIPreferences()})
	srv := New(ServerDeps{
		Tokens: ts, Usage: recorder, Portal: svc, Account: acct, Routes: routeStore,
		CookieSecure: false, SessionMaxAge: 24 * time.Hour, PublicURL: "http://localhost:8080",
	})
	seedLoginUser(t, dir, "usr_ra", "ra@example.test", "password-1", "user")
	seedLoginUser(t, dir, "usr_ra_other", "ra-other@example.test", "password-1", "user")
	if err := routeStore.CreateVendorAccount(ctx, routing.VendorAccount{
		ID: "acc_a", OwnerUserID: "usr_ra", Vendor: routing.VendorOpenAI, AuthType: routing.VendorAuthAPIKey,
		Name: "acc_a", Status: routing.VendorAccountStatusActive, ModelPrefix: "a/", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := routeStore.SetVendorAccountModels(ctx, "acc_a", []routing.VendorAccountModel{
		{GatewayModel: "a/gpt-4o", UpstreamModel: "gpt-4o", APIFlavor: routing.APIFlavorOpenAI},
	}); err != nil {
		t.Fatal(err)
	}
	if err := dir.CreatePlainToken(ctx, store.TokenRecord{ID: "tok_ra_bearer", UserID: "usr_ra", Name: "bearer", Status: store.TokenStatusActive, Scopes: `["gateway:use"]`, CreatedAt: now, UpdatedAt: now}, runAsBearerSecret); err != nil {
		t.Fatal(err)
	}
	return &runAsModelsWorld{srv: srv, dir: dir, cookie: loginAs(t, srv, "ra@example.test", "password-1")}
}

func (w *runAsModelsWorld) addToken(t *testing.T, rec store.TokenRecord) {
	t.Helper()
	now := time.Now().UTC()
	rec.CreatedAt, rec.UpdatedAt = now, now
	if rec.Status == "" {
		rec.Status = store.TokenStatusActive
	}
	if rec.Scopes == "" {
		rec.Scopes = `["gateway:use"]`
	}
	if err := w.dir.CreatePlainToken(context.Background(), rec, "secret-"+rec.ID); err != nil {
		t.Fatal(err)
	}
}

// get issues GET /api/portal/models as the session, optionally with the run-as header.
func (w *runAsModelsWorld) get(path, runAs string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.AddCookie(w.cookie)
	if runAs != "" {
		req.Header.Set(runAsHeaderName, runAs)
	}
	rec := httptest.NewRecorder()
	w.srv.ServeHTTP(rec, req)
	return rec
}

func modelRows(t *testing.T, rec *httptest.ResponseRecorder) map[string]portal.ModelDTO {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		Data []portal.ModelDTO `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	rows := map[string]portal.ModelDTO{}
	for _, m := range out.Data {
		rows[m.ID] = m
	}
	return rows
}

func TestPortalModelsRunAsVendorPrefixOverride(t *testing.T) {
	w := newRunAsModelsWorld(t)
	w.addToken(t, store.TokenRecord{
		ID: "tok_ra_prefix", UserID: "usr_ra", Name: "prefix",
		VendorProviderAccess: store.EncodeVendorAccess(auth.VendorAccess{Accounts: []auth.VendorAccessEntry{{AccountID: "acc_a", OverrideEnabled: true, OverridePrefix: "work/"}}}),
	})

	session := modelRows(t, w.get("/api/portal/models", ""))
	if _, ok := session["a/gpt-4o"]; !ok {
		t.Fatalf("session must see the account prefix a/gpt-4o: %v", session)
	}
	if _, ok := session["work/gpt-4o"]; ok {
		t.Fatalf("session must not see the token's override name")
	}
	asToken := modelRows(t, w.get("/api/portal/models", "tok_ra_prefix"))
	if _, ok := asToken["work/gpt-4o"]; !ok {
		t.Fatalf("run-as token must see work/gpt-4o: %v", asToken)
	}
	if _, ok := asToken["a/gpt-4o"]; ok {
		t.Fatalf("run-as token must not see the native account prefix")
	}
}

func TestPortalModelsRunAsClaudeAliases(t *testing.T) {
	w := newRunAsModelsWorld(t)
	w.addToken(t, store.TokenRecord{ID: "tok_ra_alias", UserID: "usr_ra", Name: "alias", ClaudeModelAliases: true})

	hasAlias := func(rows map[string]portal.ModelDTO) bool {
		for _, m := range rows {
			if m.OriginalModelID == "qwen-coder" {
				return true
			}
		}
		return false
	}
	if hasAlias(modelRows(t, w.get("/api/portal/models", ""))) {
		t.Fatal("the session principal must get no generated alias rows")
	}
	if !hasAlias(modelRows(t, w.get("/api/portal/models", "tok_ra_alias"))) {
		t.Fatal("the run-as token must get an alias row with original_model_id")
	}
}

func TestPortalModelsRunAsRefusalsAreIndistinguishable(t *testing.T) {
	w := newRunAsModelsWorld(t)
	w.addToken(t, store.TokenRecord{ID: "tok_other_user", UserID: "usr_ra_other", Name: "theirs"})
	w.addToken(t, store.TokenRecord{ID: "tok_ra_disabled", UserID: "usr_ra", Name: "off", Status: store.TokenStatusDisabled})

	var bodies []string
	for _, id := range []string{"tok_other_user", "tok_does_not_exist", "tok_ra_disabled"} {
		rec := w.get("/api/portal/models", id)
		if rec.Code != http.StatusForbidden || errorBodyOf(t, rec) != "portal.token_forbidden" {
			t.Fatalf("%s: %d %s", id, rec.Code, rec.Body.String())
		}
		bodies = append(bodies, rec.Body.String())
	}
	if bodies[0] != bodies[1] || bodies[1] != bodies[2] {
		t.Fatalf("refusal bodies differ (existence leak): %q", bodies)
	}
}

func TestPortalModelsRunAsBearerIsRefused(t *testing.T) {
	w := newRunAsModelsWorld(t)
	w.addToken(t, store.TokenRecord{ID: "tok_ra_ok", UserID: "usr_ra", Name: "ok"})
	// The id names a perfectly valid own token: a lookup would succeed, so a 403
	// proves the bearer is refused before any lookup.
	req := httptest.NewRequest(http.MethodGet, "/api/portal/models", nil)
	req.Header.Set("Authorization", "Bearer "+runAsBearerSecret)
	req.Header.Set(runAsHeaderName, "tok_ra_ok")
	rec := httptest.NewRecorder()
	w.srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden || errorBodyOf(t, rec) != "portal.token_forbidden" {
		t.Fatalf("bearer + header: %d %s", rec.Code, rec.Body.String())
	}
	// Without the header the same bearer still lists.
	req = httptest.NewRequest(http.MethodGet, "/api/portal/models", nil)
	req.Header.Set("Authorization", "Bearer "+runAsBearerSecret)
	rec = httptest.NewRecorder()
	w.srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("bearer without header: %d %s", rec.Code, rec.Body.String())
	}
}

func TestPortalModelsRunAsConflictsWithManage(t *testing.T) {
	w := newRunAsModelsWorld(t)
	w.addToken(t, store.TokenRecord{ID: "tok_ra_ok", UserID: "usr_ra", Name: "ok"})
	rec := w.get("/api/portal/models?manage=1", "tok_ra_ok")
	if rec.Code != http.StatusBadRequest || errorBodyOf(t, rec) != "portal.models_run_as_conflict" {
		t.Fatalf("manage + header: %d %s", rec.Code, rec.Body.String())
	}
}

// TestPortalModelsRunAsLoadingCountUsesTheTokensOverrideRules: a manual alias row
// offered by the run-as token's rules takes its target's loading count.
func TestPortalModelsRunAsLoadingCountUsesTheTokensOverrideRules(t *testing.T) {
	w := newRunAsModelsWorld(t)
	ctx := context.Background()
	now := time.Now().UTC()
	w.addToken(t, store.TokenRecord{
		ID: "tok_ra_rules", UserID: "usr_ra", Name: "rules",
		ModelOverrideMap: store.EncodeModelOverrideRules(map[string]store.ModelOverrideRule{"team-fast": {To: "qwen-coder", Offer: true}}),
	})
	const specID = "rspec_ra"
	if err := w.srv.Routes.UpsertRuntimeSpec(ctx, routing.RuntimeSpec{
		ID: specID, MappingID: "route_mock_qwen", Enabled: true, Binary: "/usr/bin/vllm", Args: "[]", Env: "{}", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	w.srv.RuntimeStatus.publish("mock-host-comp", []RuntimeStatusDTO{{SpecID: specID, Model: "qwen-coder", State: "starting"}})

	if _, ok := modelRows(t, w.get("/api/portal/models", ""))["team-fast"]; ok {
		t.Fatal("the session must not see the token's alias row")
	}
	rows := modelRows(t, w.get("/api/portal/models", "tok_ra_rules"))
	target, ok := rows["qwen-coder"]
	if !ok || target.LoadingOnCount != 1 {
		t.Fatalf("target row = %+v (present=%v), want loading_on_count 1", target, ok)
	}
	alias, ok := rows["team-fast"]
	if !ok {
		t.Fatalf("alias row team-fast missing: %v", rows)
	}
	if alias.LoadingOnCount != 1 {
		t.Fatalf("alias loading_on_count = %d, want 1 (keyed through the run-as token's rules)", alias.LoadingOnCount)
	}
}
