// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/store"
	"op-ai-gateway/internal/usage"
	"testing"
	"time"
)

func TestActiveRegistryAddRemoveSnapshot(t *testing.T) {
	reg := newActiveRegistry(nil)

	if snap := reg.Snapshot(); len(snap) != 0 {
		t.Fatalf("fresh registry Snapshot() = %d rows, want 0", len(snap))
	}

	now := time.Now().UTC()
	reg.Add(ActiveRequest{ID: "a", UserID: "usr_1", Model: "m1", StartedAt: now})
	reg.Add(ActiveRequest{ID: "b", UserID: "usr_2", Model: "m2", StartedAt: now.Add(time.Second)})

	snap := reg.Snapshot()
	if len(snap) != 2 {
		t.Fatalf("Snapshot() after two Add = %d rows, want 2", len(snap))
	}
	byID := map[string]ActiveRequest{}
	for _, row := range snap {
		byID[row.ID] = row
	}
	if byID["a"].UserID != "usr_1" || byID["a"].Model != "m1" {
		t.Fatalf("row a not stored correctly: %#v", byID["a"])
	}
	if byID["b"].UserID != "usr_2" || byID["b"].Model != "m2" {
		t.Fatalf("row b not stored correctly: %#v", byID["b"])
	}

	reg.Remove("a")
	snap = reg.Snapshot()
	if len(snap) != 1 || snap[0].ID != "b" {
		t.Fatalf("after Remove(a) Snapshot() = %#v, want only b", snap)
	}

	// Snapshot returns a copy: mutating it must not affect the registry.
	snap[0].Model = "mutated"
	if reg.Snapshot()[0].Model != "m2" {
		t.Fatal("Snapshot() must return a copy, registry was mutated through the returned slice")
	}
}

func TestActiveRegistryNilSafe(t *testing.T) {
	var reg *activeRegistry // nil

	// None of these must panic.
	reg.Add(ActiveRequest{ID: "x"})
	reg.Remove("x")
	if snap := reg.Snapshot(); len(snap) != 0 {
		t.Fatalf("nil registry Snapshot() = %d rows, want 0", len(snap))
	}
	if n := reg.CountByServerName("srv"); n != 0 {
		t.Fatalf("nil registry CountByServerName() = %d, want 0", n)
	}
}

func TestActiveRegistryCountByServerName(t *testing.T) {
	reg := newActiveRegistry(nil)
	now := time.Now().UTC()
	reg.Add(ActiveRequest{ID: "a", ServerName: "srv-1", StartedAt: now})
	reg.Add(ActiveRequest{ID: "b", ServerName: "srv-1", StartedAt: now})
	reg.Add(ActiveRequest{ID: "c", ServerName: "srv-2", StartedAt: now})

	if n := reg.CountByServerName("srv-1"); n != 2 {
		t.Fatalf("CountByServerName(srv-1) = %d, want 2", n)
	}
	if n := reg.CountByServerName("srv-2"); n != 1 {
		t.Fatalf("CountByServerName(srv-2) = %d, want 1", n)
	}
	if n := reg.CountByServerName("srv-absent"); n != 0 {
		t.Fatalf("CountByServerName(srv-absent) = %d, want 0", n)
	}
	reg.Remove("a")
	if n := reg.CountByServerName("srv-1"); n != 1 {
		t.Fatalf("CountByServerName(srv-1) after Remove(a) = %d, want 1", n)
	}
}

func TestActiveRegistryAddPublishes(t *testing.T) {
	broker := usage.NewBroker()
	ch := broker.Register()
	defer broker.Unregister(ch)
	reg := newActiveRegistry(broker)

	reg.Add(ActiveRequest{ID: "a", StartedAt: time.Now().UTC()})
	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal("Add did not publish a broker signal")
	}
}

func TestActiveRegistryRemovePublishes(t *testing.T) {
	broker := usage.NewBroker()
	reg := newActiveRegistry(broker)
	reg.Add(ActiveRequest{ID: "a", StartedAt: time.Now().UTC()})

	// Register AFTER the Add so we only observe the Remove signal.
	ch := broker.Register()
	defer broker.Unregister(ch)

	reg.Remove("a")
	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal("Remove did not publish a broker signal")
	}
}

func TestActiveRegistryNilBrokerNoPublishPanic(t *testing.T) {
	// A registry built without a broker must still Add/Remove without panicking.
	reg := newActiveRegistry(nil)
	reg.Add(ActiveRequest{ID: "a", StartedAt: time.Now().UTC()})
	reg.Remove("a")
	if len(reg.Snapshot()) != 0 {
		t.Fatal("registry not empty after Add+Remove")
	}
}

func TestActiveRegistryServerActivityInFlight(t *testing.T) {
	r := newActiveRegistry(nil)
	r.Add(ActiveRequest{ID: "a", ServerID: "srv1"})
	r.Add(ActiveRequest{ID: "b", ServerID: "srv1"})
	r.Add(ActiveRequest{ID: "c", ServerID: "srv2"})

	if n, _ := r.ServerActivity("srv1"); n != 2 {
		t.Fatalf("srv1 in-flight = %d, want 2", n)
	}
	if n, _ := r.ServerActivity("srv2"); n != 1 {
		t.Fatalf("srv2 in-flight = %d, want 1", n)
	}
	if n, last := r.ServerActivity("srv-unknown"); n != 0 || !last.IsZero() {
		t.Fatalf("unknown server = (%d,%v), want (0, zero)", n, last)
	}
}

func TestActiveRegistryServerActivityLastCompleted(t *testing.T) {
	fixed := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
	r := newActiveRegistry(nil)
	r.now = func() time.Time { return fixed }

	r.Add(ActiveRequest{ID: "a", ServerID: "srv1"})
	if _, last := r.ServerActivity("srv1"); !last.IsZero() {
		t.Fatalf("before completion last = %v, want zero", last)
	}
	r.Remove("a")

	n, last := r.ServerActivity("srv1")
	if n != 0 {
		t.Fatalf("after completion in-flight = %d, want 0", n)
	}
	if !last.Equal(fixed) {
		t.Fatalf("lastCompletedAt = %v, want %v", last, fixed)
	}
}

func TestActiveRegistryServerActivityNilSafe(t *testing.T) {
	var r *activeRegistry
	if n, last := r.ServerActivity("srv1"); n != 0 || !last.IsZero() {
		t.Fatalf("nil registry = (%d,%v), want (0, zero)", n, last)
	}
}

func getActive(t *testing.T, srv *Server, cookie *http.Cookie, scope string) []activeRequestDTO {
	t.Helper()
	url := "/api/portal/usage/active"
	if scope != "" {
		url += "?scope=" + scope
	}
	req := httptest.NewRequest(http.MethodGet, url, nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s status = %d, body = %s", url, rec.Code, rec.Body.String())
	}
	var body struct {
		Data []activeRequestDTO `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v (%s)", err, rec.Body.String())
	}
	return body.Data
}

func TestPortalUsageActiveScopeAndShape(t *testing.T) {
	srv, dir := newAuthTestServer(t)
	seedLoginUser(t, dir, "usr_owner", "owner@example.test", "password-1", "user")
	seedLoginUser(t, dir, "usr_adm", "adm@example.test", "password-1", "admin")

	base := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	// Seed out of chronological order so the ascending sort is actually exercised.
	srv.Active.Add(ActiveRequest{ID: "req_other", UserID: "usr_other", TokenID: "tok_o", TokenName: "Other", Model: "m2", APIFlavor: "openai", ReqPath: "/v1/chat/completions", Stream: true, StartedAt: base.Add(2 * time.Second)})
	srv.Active.Add(ActiveRequest{ID: "req_owner", UserID: "usr_owner", TokenID: "tok_w", TokenName: "Mine", ServerName: "srv-mine", Model: "m1", APIFlavor: "anthropic", ReqPath: "/v1/messages", ProviderPath: "/v1/chat/completions", ProviderModel: "upstream-m1", Stream: false, StartedAt: base.Add(1 * time.Second)})

	ownerCookie := loginCookie(t, srv, "owner@example.test", "password-1")
	admCookie := loginCookie(t, srv, "adm@example.test", "password-1")

	// Owner without scope sees only their own row.
	if rows := getActive(t, srv, ownerCookie, ""); len(rows) != 1 || rows[0].ID != "req_owner" {
		t.Fatalf("owner (no scope) rows = %#v, want only req_owner", rows)
	}

	// Non-admin with scope=all is still confined to their own row.
	if rows := getActive(t, srv, ownerCookie, "all"); len(rows) != 1 || rows[0].ID != "req_owner" {
		t.Fatalf("non-admin scope=all rows = %#v, want only req_owner", rows)
	}

	// Admin with scope=all sees both, sorted by started_at ascending.
	all := getActive(t, srv, admCookie, "all")
	if len(all) != 2 {
		t.Fatalf("admin scope=all rows = %d, want 2 (%#v)", len(all), all)
	}
	if all[0].ID != "req_owner" || all[1].ID != "req_other" {
		t.Fatalf("admin scope=all not sorted ascending by started_at: %#v", all)
	}

	// Admin without scope=all is confined to their own rows (none here).
	if rows := getActive(t, srv, admCookie, ""); len(rows) != 0 {
		t.Fatalf("admin (no scope) rows = %#v, want none (admin owns no active request)", rows)
	}

	// DTO shape: the owner row is mapped field-for-field with an RFC3339 timestamp.
	// UserName is resolved best-effort; seedLoginUser sets DisplayName to the id.
	got := all[0]
	want := activeRequestDTO{
		ID: "req_owner", UserID: "usr_owner", UserName: "usr_owner", TokenID: "tok_w", TokenName: "Mine",
		ServerName: "srv-mine", Model: "m1", APIFlavor: "anthropic", ReqPath: "/v1/messages",
		ProviderPath: "/v1/chat/completions", ProviderModel: "upstream-m1", Stream: false,
		StartedAt: base.Add(time.Second).Format(time.RFC3339),
	}
	if got != want {
		t.Fatalf("DTO shape mismatch:\n got  %#v\n want %#v", got, want)
	}
}

func TestPortalUsageActiveIncludesUserName(t *testing.T) {
	srv, dir := newAuthTestServer(t)
	seedLoginUser(t, dir, "usr_adm", "adm@example.test", "password-1", "admin")
	// A user with a distinct display name (differs from the id) so the assertion
	// proves the name is resolved from the directory, not echoed from user_id.
	if err := dir.CreateUser(context.Background(), store.User{
		ID: "usr_named", Email: "named@example.test", DisplayName: "Named User",
		Role: "user", Status: store.UserStatusActive, PreferredLanguage: "de",
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("seed named user: %v", err)
	}

	base := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	// A known user resolves to a display name; an unknown user yields "".
	srv.Active.Add(ActiveRequest{ID: "req_named", UserID: "usr_named", TokenID: "tok_n", TokenName: "N", Model: "m1", APIFlavor: "openai", ReqPath: "/v1/chat/completions", Stream: false, StartedAt: base.Add(time.Second)})
	srv.Active.Add(ActiveRequest{ID: "req_ghost", UserID: "usr_ghost", TokenID: "tok_g", TokenName: "G", Model: "m2", APIFlavor: "openai", ReqPath: "/v1/chat/completions", Stream: false, StartedAt: base.Add(2 * time.Second)})

	admCookie := loginCookie(t, srv, "adm@example.test", "password-1")
	rows := getActive(t, srv, admCookie, "all")
	byID := map[string]activeRequestDTO{}
	for _, row := range rows {
		byID[row.ID] = row
	}
	if got := byID["req_named"].UserName; got != "Named User" {
		t.Fatalf("req_named user_name = %q, want %q", got, "Named User")
	}
	if got := byID["req_ghost"].UserName; got != "" {
		t.Fatalf("req_ghost (unknown user) user_name = %q, want empty", got)
	}
}

func TestPortalUsageActiveEmptyReturnsArray(t *testing.T) {
	srv := NewTestServer()
	req := httptest.NewRequest(http.MethodGet, "/api/portal/usage/active", nil)
	req.Header.Set("Authorization", "Bearer dev-secret")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	// "data" must be an empty array, never null.
	if got := rec.Body.String(); got != "{\"data\":[]}\n" {
		t.Fatalf("empty active list body = %q, want {\"data\":[]}", got)
	}
}

func TestPortalUsageActiveRejectsNonGet(t *testing.T) {
	srv := NewTestServer()
	req := httptest.NewRequest(http.MethodPost, "/api/portal/usage/active", nil)
	req.Header.Set("Authorization", "Bearer dev-secret")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST status = %d, want 405", rec.Code)
	}
}

func TestPortalUsageActiveTokenAndUserFilter(t *testing.T) {
	srv := NewTestServerWithTokenScopes([]string{"gateway:use", "admin"})
	now := time.Now().UTC()
	srv.Active.Add(ActiveRequest{ID: "a1", UserID: "usr_1", TokenID: "tok_a", StartedAt: now})
	srv.Active.Add(ActiveRequest{ID: "a2", UserID: "usr_1", TokenID: "", StartedAt: now.Add(time.Second)}) // chat
	srv.Active.Add(ActiveRequest{ID: "a3", UserID: "usr_2", TokenID: "tok_b", StartedAt: now.Add(2 * time.Second)})

	ids := func(url string) []string {
		req := httptest.NewRequest(http.MethodGet, url, nil)
		req.Header.Set("Authorization", "Bearer dev-secret")
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("active %s = %d body=%s", url, rec.Code, rec.Body.String())
		}
		var body struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		out := make([]string, 0, len(body.Data))
		for _, d := range body.Data {
			out = append(out, d.ID)
		}
		return out
	}

	// scope=all + specific token.
	if got := ids("/api/portal/usage/active?scope=all&token_id=tok_a"); len(got) != 1 || got[0] != "a1" {
		t.Fatalf("token=tok_a ids=%v, want [a1]", got)
	}
	// scope=all + chat sentinel (empty token).
	if got := ids("/api/portal/usage/active?scope=all&token_id=" + NoTokenWire); len(got) != 1 || got[0] != "a2" {
		t.Fatalf("token=__none__ ids=%v, want [a2]", got)
	}
	// scope=all + specific user.
	if got := ids("/api/portal/usage/active?scope=all&user_id=usr_2"); len(got) != 1 || got[0] != "a3" {
		t.Fatalf("user=usr_2 ids=%v, want [a3]", got)
	}
}
