// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/portal"
	"strings"
	"testing"
)

const vagPath = "/api/portal/vendor-account-groups"

func vagDecode(t *testing.T, rec *httptest.ResponseRecorder) portal.VendorAccountGroupDTO {
	t.Helper()
	var dto portal.VendorAccountGroupDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &dto); err != nil {
		t.Fatalf("unmarshal group: %v (%s)", err, rec.Body.String())
	}
	return dto
}

func vagBody(prefix string, ids ...string) string {
	quoted := make([]string, len(ids))
	for i, id := range ids {
		quoted[i] = `"` + id + `"`
	}
	return `{"name":"Pool","vendor":"openai","model_prefix":"` + prefix + `","account_ids":[` + strings.Join(quoted, ",") + `]}`
}

func vagCreate(t *testing.T, srv *Server, prefix string, ids ...string) portal.VendorAccountGroupDTO {
	t.Helper()
	rec := vaDo(t, srv, http.MethodPost, vagPath, vaOwnerSecret, vagBody(prefix, ids...))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create group = %d, body = %s", rec.Code, rec.Body.String())
	}
	return vagDecode(t, rec)
}

func TestVendorGroupEndpointsLifecycle(t *testing.T) {
	srv, _ := newVendorAccountTestServer(t, true)
	a, b := vaCreate(t, srv, vaOwnerSecret, "one"), vaCreate(t, srv, vaOwnerSecret, "two")

	rec := vaDo(t, srv, http.MethodPost, vagPath, vaOwnerSecret, vagBody("pool/", a.ID, b.ID))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d, body = %s", rec.Code, rec.Body.String())
	}
	// The wire field names.
	var raw map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"id", "vendor", "name", "model_prefix", "status", "thresholds", "active_member_id", "members", "created_at", "updated_at"} {
		if _, ok := raw[k]; !ok {
			t.Errorf("group body lacks %q: %s", k, rec.Body.String())
		}
	}
	th, _ := raw["thresholds"].(map[string]any)
	if th["five_hour_pct"] != float64(99) || th["weekly_pct"] != float64(99) || th["credits_pct"] != float64(99) {
		t.Errorf("thresholds = %v", th)
	}
	members, _ := raw["members"].([]any)
	m0, _ := members[0].(map[string]any)
	for _, k := range []string{"account_id", "account_name", "auth_type", "owner_name", "position", "state", "reason", "cooldowns"} {
		if _, ok := m0[k]; !ok {
			t.Errorf("member body lacks %q: %v", k, m0)
		}
	}
	g := vagDecode(t, rec)
	if g.ActiveMemberID != a.ID || len(g.Members) != 2 || g.Members[1].State != "available" {
		t.Errorf("group = %+v", g)
	}

	item := vagPath + "/" + g.ID
	rec = vaDo(t, srv, http.MethodGet, vagPath, vaOwnerSecret, "")
	var list portal.VendorAccountGroupListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil || rec.Code != http.StatusOK || len(list.Groups) != 1 {
		t.Fatalf("list = %d %s", rec.Code, rec.Body.String())
	}
	if rec = vaDo(t, srv, http.MethodGet, item, vaOwnerSecret, ""); rec.Code != http.StatusOK || vagDecode(t, rec).ID != g.ID {
		t.Fatalf("get = %d %s", rec.Code, rec.Body.String())
	}
	rec = vaDo(t, srv, http.MethodPatch, item, vaOwnerSecret, `{"name":"Renamed","status":"disabled","threshold_weekly_pct":50}`)
	if got := vagDecode(t, rec); rec.Code != http.StatusOK || got.Name != "Renamed" || got.Status != "disabled" || got.Thresholds.WeeklyPct != 50 {
		t.Fatalf("patch = %d %s", rec.Code, rec.Body.String())
	}
	rec = vaDo(t, srv, http.MethodPut, item+"/members", vaOwnerSecret, `{"account_ids":["`+b.ID+`","`+a.ID+`"]}`)
	if got := vagDecode(t, rec); rec.Code != http.StatusOK || got.Members[0].AccountID != b.ID || got.ActiveMemberID != b.ID {
		t.Fatalf("put members = %d %s", rec.Code, rec.Body.String())
	}
	if rec = vaDo(t, srv, http.MethodDelete, item, vaOwnerSecret, ""); rec.Code != http.StatusOK {
		t.Fatalf("delete = %d %s", rec.Code, rec.Body.String())
	}
	requireStatusCode(t, "get after delete", vaDo(t, srv, http.MethodGet, item, vaOwnerSecret, ""), http.StatusNotFound, "vendor_account_group.not_found")
}

func TestVendorGroupEndpointsStrangerAndAuth(t *testing.T) {
	srv, _ := newVendorAccountTestServer(t, true)
	a := vaCreate(t, srv, vaOwnerSecret, "one")
	g := vagCreate(t, srv, "pool/", a.ID)
	item := vagPath + "/" + g.ID
	for _, id := range []string{g.ID, "vag_missing"} {
		p := vagPath + "/" + id
		requireStatusCode(t, "stranger get", vaDo(t, srv, http.MethodGet, p, vaOtherSecret, ""), http.StatusNotFound, "vendor_account_group.not_found")
		requireStatusCode(t, "stranger patch", vaDo(t, srv, http.MethodPatch, p, vaOtherSecret, `{"name":"x"}`), http.StatusNotFound, "vendor_account_group.not_found")
		requireStatusCode(t, "stranger delete", vaDo(t, srv, http.MethodDelete, p, vaOtherSecret, ""), http.StatusNotFound, "vendor_account_group.not_found")
		requireStatusCode(t, "stranger put members", vaDo(t, srv, http.MethodPut, p+"/members", vaOtherSecret, `{"account_ids":[]}`), http.StatusNotFound, "vendor_account_group.not_found")
	}
	if rec := vaDo(t, srv, http.MethodGet, vagPath, vaOtherSecret, ""); rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), g.ID) {
		t.Errorf("stranger list = %d %s", rec.Code, rec.Body.String())
	}
	for _, p := range []string{vagPath, item, item + "/members"} {
		if rec := vaDo(t, srv, http.MethodGet, p, "", ""); rec.Code != http.StatusUnauthorized {
			t.Errorf("GET %s without credentials = %d, want 401", p, rec.Code)
		}
	}
	cases := []struct{ path, method, allow string }{
		{vagPath, http.MethodDelete, "GET, POST"},
		{item, http.MethodPost, "GET, PATCH, DELETE"},
		{item + "/members", http.MethodGet, "PUT"},
		{item + "/other", http.MethodGet, ""},
	}
	for _, c := range cases {
		rec := vaDo(t, srv, c.method, c.path, vaOwnerSecret, "")
		want := http.StatusMethodNotAllowed
		if c.allow == "" {
			want = http.StatusNotFound
		}
		if rec.Code != want || rec.Header().Get("Allow") != c.allow {
			t.Errorf("%s %s = %d (Allow %q), want %d (%q)", c.method, c.path, rec.Code, rec.Header().Get("Allow"), want, c.allow)
		}
	}
	requireStatusCode(t, "malformed body", vaDo(t, srv, http.MethodPost, vagPath, vaOwnerSecret, `{"name":`), http.StatusBadRequest, codeRequestInvalidJSON)
}

func TestVendorGroupEndpointsValidationCodes(t *testing.T) {
	srv, _ := newVendorAccountTestServer(t, true)
	a := vaCreate(t, srv, vaOwnerSecret, "one")
	other := vaCreate(t, srv, vaOtherSecret, "theirs")
	post := func(label, body string, status int, code string) {
		t.Helper()
		requireStatusCode(t, label, vaDo(t, srv, http.MethodPost, vagPath, vaOwnerSecret, body), status, code)
	}
	post("name", `{"name":"","vendor":"openai"}`, http.StatusBadRequest, "vendor_account_group.name_required")
	post("vendor", `{"name":"x","vendor":"nope"}`, http.StatusBadRequest, "vendor_account_group.vendor_invalid")
	post("prefix", `{"name":"x","vendor":"openai","model_prefix":"a b"}`, http.StatusBadRequest, "vendor_account_group.model_prefix_invalid")
	post("status", `{"name":"x","vendor":"openai","status":"bogus"}`, http.StatusBadRequest, "vendor_account_group.status_invalid")
	post("threshold", `{"name":"x","vendor":"openai","threshold_credits_pct":0}`, http.StatusBadRequest, "vendor_account_group.threshold_invalid")
	post("unreachable", vagBody("p/", other.ID), http.StatusBadRequest, "vendor_account_group.member_unreachable")
	post("duplicate", vagBody("p/", a.ID, a.ID), http.StatusBadRequest, "vendor_account_group.member_duplicate")
	post("first empty-prefix group", vagBody("", a.ID), http.StatusCreated, "")
	// A second group under the same empty prefix over the same names clashes.
	b := vaCreate(t, srv, vaOwnerSecret, "two")
	post("prefix conflict", vagBody("", b.ID), http.StatusBadRequest, "vendor_account_group.prefix_conflict")
}

func TestVendorGroupEndpointsModuleFlagOff(t *testing.T) {
	srv, _, _ := newVendorAccountSettingsTestServer(t, true)
	for _, c := range []struct{ method, path, body string }{
		{http.MethodGet, vagPath, ""},
		{http.MethodPost, vagPath, vagBody("p/")},
		{http.MethodGet, vagPath + "/vag_x", ""},
		{http.MethodPatch, vagPath + "/vag_x", `{}`},
		{http.MethodDelete, vagPath + "/vag_x", ""},
		{http.MethodPut, vagPath + "/vag_x/members", `{"account_ids":[]}`},
	} {
		requireStatusCode(t, c.method+" "+c.path, vaDo(t, srv, c.method, c.path, vaOwnerSecret, c.body), http.StatusConflict, "vendor_accounts.module_disabled")
	}
}

// A cookie session write needs the CSRF header like every portal write; with it the
// request reaches the handler (here: the module flag, off in this server).
func TestVendorGroupEndpointsSessionWritesNeedCSRF(t *testing.T) {
	srv, dir := newAuthTestServer(t)
	seedLoginUser(t, dir, "usr_1", "a@example.test", "password-1", "user")
	cookie := loginAs(t, srv, "a@example.test", "password-1")
	for _, c := range []struct{ method, path, body string }{
		{http.MethodPost, vagPath, vagBody("p/")},
		{http.MethodPatch, vagPath + "/vag_x", `{}`},
		{http.MethodDelete, vagPath + "/vag_x", ""},
		{http.MethodPut, vagPath + "/vag_x/members", `{"account_ids":[]}`},
	} {
		req := httptest.NewRequest(c.method, c.path, strings.NewReader(c.body))
		req.AddCookie(cookie)
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s %s without the CSRF header = %d, want 403", c.method, c.path, rec.Code)
		}
		if rec := doJSON(t, srv, cookie, c.method, c.path, c.body); rec.Code != http.StatusConflict {
			t.Errorf("%s %s with the CSRF header = %d (%s), want 409 from the handler", c.method, c.path, rec.Code, rec.Body.String())
		}
	}
	if rec := doJSON(t, srv, cookie, http.MethodGet, vagPath, ""); rec.Code != http.StatusConflict {
		t.Errorf("GET = %d, want 409 module_disabled", rec.Code)
	}
}
