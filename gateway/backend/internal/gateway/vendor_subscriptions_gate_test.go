// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/portal"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
)

const (
	vendorSubscriptionsDisabledCode = "vendor_accounts.subscriptions_disabled"
	vendorSubscriptionsKey          = "vendor_subscriptions_enabled"
)

// subscriptionsPortalStub reports the vendor_subscriptions_enabled switch and
// nothing else (the nil-embedded-interface trick of cachingPortalStub). reads
// counts the accessor calls so a test can prove the resolver's hot path is served
// from the gateway-side cache rather than the store; the value can be flipped
// between reads.
type subscriptionsPortalStub struct {
	portal.API
	on    *atomic.Bool
	reads *atomic.Int32
}

func (f subscriptionsPortalStub) VendorSubscriptionsEnabled(context.Context) bool {
	f.reads.Add(1)
	return f.on.Load()
}

// The resolver's per-resolve subscriptions read is served from a TTL cache (the
// inference hot path must not issue a system_settings read per request),
// invalidateVendorSettingsCache drops it, and a Server with no portal reports ON
// (the setting is default-on, so an unreadable state must not disable anything).
func TestVendorSubscriptionsEnabledIsReadThroughTheCache(t *testing.T) {
	on, reads := &atomic.Bool{}, &atomic.Int32{}
	on.Store(true)
	s := &Server{Portal: subscriptionsPortalStub{on: on, reads: reads}}
	ctx := context.Background()

	for range 5 {
		if !s.vendorSubscriptionsEnabledCached(ctx) {
			t.Fatal("setting on: want true")
		}
	}
	if got := reads.Load(); got != 1 {
		t.Fatalf("portal reads = %d after 5 cached reads, want 1", got)
	}

	// An out-of-band flip is not visible inside the TTL ...
	on.Store(false)
	if !s.vendorSubscriptionsEnabledCached(ctx) {
		t.Fatal("flip inside the TTL: want the cached true")
	}
	// ... but the PUT handler's invalidation makes it visible on the very next read.
	s.invalidateVendorSettingsCache()
	if s.vendorSubscriptionsEnabledCached(ctx) {
		t.Fatal("after invalidate: want the fresh false")
	}
	if got := reads.Load(); got != 2 {
		t.Fatalf("portal reads = %d after invalidate, want 2 (one re-read)", got)
	}

	if !(&Server{}).vendorSubscriptionsEnabledCached(ctx) {
		t.Fatal("nil portal must default to on (never silently disable subscriptions)")
	}
}

// vendor_subscriptions_enabled round-trips through the system settings endpoint:
// the default is ON, a PUT takes effect on the very next GET, a PUT that omits it
// leaves it alone, and only a system principal can write it.
func TestSystemSettingsVendorSubscriptionsEnabled(t *testing.T) {
	srv, _, settings := newVendorAccountSettingsTestServer(t, true)

	get := func() map[string]any {
		t.Helper()
		rec := vaDo(t, srv, http.MethodGet, "/api/system/settings", vaSystemSecret, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("GET settings = %d, body = %s", rec.Code, rec.Body.String())
		}
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		return body
	}
	if got := get()[vendorSubscriptionsKey]; got != true {
		t.Fatalf("default %s = %v, want true (opt-out)", vendorSubscriptionsKey, got)
	}

	rec := vaDo(t, srv, http.MethodPut, "/api/system/settings", vaSystemSecret, `{"`+vendorSubscriptionsKey+`":false}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT off = %d, body = %s", rec.Code, rec.Body.String())
	}
	var putBody map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &putBody); err != nil {
		t.Fatalf("unmarshal PUT: %v", err)
	}
	if putBody[vendorSubscriptionsKey] != false {
		t.Fatalf("PUT response %s = %v, want false", vendorSubscriptionsKey, putBody[vendorSubscriptionsKey])
	}
	if got := get()[vendorSubscriptionsKey]; got != false {
		t.Fatalf("GET after PUT %s = %v, want false", vendorSubscriptionsKey, got)
	}
	if values, err := settings.SystemSettings(context.Background()); err != nil || values[vendorSubscriptionsKey] != "false" {
		t.Fatalf("stored = %v, %v, want %s=false", values, err, vendorSubscriptionsKey)
	}

	if rec := vaDo(t, srv, http.MethodPut, "/api/system/settings", vaSystemSecret, `{"vendor_accounts_enabled":true}`); rec.Code != http.StatusOK {
		t.Fatalf("PUT other field = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := get()[vendorSubscriptionsKey]; got != false {
		t.Fatalf("GET after an unrelated PUT %s = %v, want the kept false", vendorSubscriptionsKey, got)
	}

	if rec := vaDo(t, srv, http.MethodPut, "/api/system/settings", vaOwnerSecret, `{"`+vendorSubscriptionsKey+`":true}`); rec.Code != http.StatusForbidden {
		t.Fatalf("PUT as a plain user = %d, want 403", rec.Code)
	}
	if got := get()[vendorSubscriptionsKey]; got != false {
		t.Fatalf("a forbidden PUT changed %s to %v", vendorSubscriptionsKey, got)
	}
}

func putVendorSubscriptionsEnabled(t *testing.T, srv *Server, enabled bool) {
	t.Helper()
	body := `{"` + vendorSubscriptionsKey + `":false}`
	if enabled {
		body = `{"` + vendorSubscriptionsKey + `":true}`
	}
	if rec := vaDo(t, srv, http.MethodPut, "/api/system/settings", vaSystemSecret, body); rec.Code != http.StatusOK {
		t.Fatalf("PUT %s = %d, body = %s", body, rec.Code, rec.Body.String())
	}
}

const vaSubscriptionCreateBody = `{"vendor":"anthropic","auth_type":"subscription","name":"Claude Max"}`

// With the subscriptions setting OFF, creating or connecting a SUBSCRIPTION account
// is a 409 vendor_accounts.subscriptions_disabled (persisting nothing), an api_key
// account is untouched, reads and deletes of the existing subscription account still
// work (nothing is deleted by the toggle), and switching it back on restores create.
func TestVendorSubscriptionAccountEndpointsAre409WhileSubscriptionsAreOff(t *testing.T) {
	srv, routeStore, _ := newVendorAccountSettingsTestServer(t, true)
	enableVendorAccountsFlag(t, srv)

	// An existing subscription account, created while the setting is still ON.
	rec := vaDo(t, srv, http.MethodPost, "/api/portal/vendor-accounts", vaOwnerSecret, vaSubscriptionCreateBody)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create subscription while on = %d, want 201, body = %s", rec.Code, rec.Body.String())
	}
	existing := vaDecode(t, rec)

	putVendorSubscriptionsEnabled(t, srv, false)

	rec = vaDo(t, srv, http.MethodPost, "/api/portal/vendor-accounts", vaOwnerSecret, vaSubscriptionCreateBody)
	if rec.Code != http.StatusConflict || perfErrorCode(t, rec.Body.Bytes()) != vendorSubscriptionsDisabledCode {
		t.Fatalf("create subscription while off = %d %s, want 409 %s", rec.Code, rec.Body.String(), vendorSubscriptionsDisabledCode)
	}
	connects := []struct{ name, path, body string }{
		{"connect import", "/connect/import", `{"access_token":"t"}`},
		{"connect begin", "/connect/begin", ""},
		{"connect complete", "/connect/complete", `{"code":"c"}`},
		{"connect device begin", "/connect/device/begin", ""},
		{"connect device poll", "/connect/device/poll", ""},
	}
	for _, tc := range connects {
		t.Run(tc.name, func(t *testing.T) {
			rec := vaDo(t, srv, http.MethodPost, "/api/portal/vendor-accounts/"+existing.ID+tc.path, vaOwnerSecret, tc.body)
			if rec.Code != http.StatusConflict || perfErrorCode(t, rec.Body.Bytes()) != vendorSubscriptionsDisabledCode {
				t.Fatalf("%s while off = %d %s, want 409 %s", tc.name, rec.Code, rec.Body.String(), vendorSubscriptionsDisabledCode)
			}
		})
	}

	// Only the one pre-existing account is stored; the refusals persisted nothing.
	if rows, err := routeStore.VendorAccounts(context.Background()); err != nil || len(rows) != 1 || rows[0].ID != existing.ID {
		t.Fatalf("rows after refused calls = %#v, %v, want only %s", rows, err, existing.ID)
	}
	// The existing account is still listed and readable (no deletion, no 409).
	if rec := vaDo(t, srv, http.MethodGet, "/api/portal/vendor-accounts/"+existing.ID, vaOwnerSecret, ""); rec.Code != http.StatusOK {
		t.Fatalf("get existing subscription while off = %d, want 200, body = %s", rec.Code, rec.Body.String())
	}
	// An api_key account is not gated.
	apiKey := vaCreate(t, srv, vaOwnerSecret, "Key account")

	putVendorSubscriptionsEnabled(t, srv, true)
	rec = vaDo(t, srv, http.MethodPost, "/api/portal/vendor-accounts", vaOwnerSecret, vaSubscriptionCreateBody)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create subscription after switching back on = %d, want 201, body = %s", rec.Code, rec.Body.String())
	}
	if rows, err := routeStore.VendorAccounts(context.Background()); err != nil || len(rows) != 3 {
		t.Fatalf("rows = %d, %v, want existing + api_key (%s) + new subscription", len(rows), err, apiKey.ID)
	}
}

// listedModelIDs returns the model ids GET /v1/models serves the owner.
func listedModelIDs(t *testing.T, srv *Server) []string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+vaOwnerSecret)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/models = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal /v1/models: %v (%s)", err, rec.Body.String())
	}
	ids := make([]string, 0, len(body.Data))
	for _, m := range body.Data {
		ids = append(ids, m.ID)
	}
	return ids
}

// The whole path, live: an owner's Anthropic SUBSCRIPTION account is served and
// advertised; a settings PUT that turns the subscriptions setting off takes effect
// on the very next request (the resolver's cached accessor is invalidated by the
// PUT, though a successful request just warmed it ON) -- the model disappears from
// /v1/models AND stops routing, never reaching the upstream -- and a PUT back on
// restores both. The account is never deleted.
func TestVendorSubscriptionsTogglePutAppliesLiveToRoutingAndListing(t *testing.T) {
	e := newSubscriptionMessagesE2E(t)

	post := func() *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(apiKeyMessagesBody))
		req.Header.Set("Authorization", "Bearer "+vaOwnerSecret)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("anthropic-version", "1999-01-01")
		rec := httptest.NewRecorder()
		e.srv.ServeHTTP(rec, req)
		return rec
	}

	// ON (default): served (this warms the resolver's cached accessor) and advertised.
	if rec := post(); rec.Code != http.StatusOK {
		t.Fatalf("request with subscriptions on = %d, want 200, body = %s", rec.Code, rec.Body.String())
	}
	if !slices.Contains(listedModelIDs(t, e.srv), subMessagesModel) {
		t.Fatalf("/v1/models with subscriptions on does not list %q", subMessagesModel)
	}
	upstreamCalls := len(e.transport.gotHosts)
	if upstreamCalls != 1 {
		t.Fatalf("upstream calls = %d after one request, want 1", upstreamCalls)
	}

	// OFF: not routed, never forwarded upstream, and not advertised.
	putVendorSubscriptionsEnabled(t, e.srv, false)
	if rec := post(); rec.Code == http.StatusOK {
		t.Fatalf("request with subscriptions off = 200, want a refusal (no route), body = %s", rec.Body.String())
	}
	if got := len(e.transport.gotHosts); got != upstreamCalls {
		t.Fatalf("upstream calls = %d with subscriptions off, want %d (the request must not reach the vendor)", got, upstreamCalls)
	}
	if slices.Contains(listedModelIDs(t, e.srv), subMessagesModel) {
		t.Fatalf("/v1/models with subscriptions off still lists %q (listing and routing must move together)", subMessagesModel)
	}

	// ON again: served and advertised again (the account survived).
	putVendorSubscriptionsEnabled(t, e.srv, true)
	if rec := post(); rec.Code != http.StatusOK {
		t.Fatalf("request after switching subscriptions back on = %d, want 200, body = %s", rec.Code, rec.Body.String())
	}
	if !slices.Contains(listedModelIDs(t, e.srv), subMessagesModel) {
		t.Fatalf("/v1/models after switching subscriptions back on does not list %q", subMessagesModel)
	}
}
