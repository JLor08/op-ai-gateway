// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// putPreference PUTs a raw JSON value under key for the cookied user and returns
// the recorder. It sets the CSRF header so the session-authenticated PUT passes
// requireWebScope's CSRF gate.
func putPreference(t *testing.T, srv *Server, cookie *http.Cookie, key, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPut, "/api/portal/preferences/"+key, strings.NewReader(body))
	req.AddCookie(cookie)
	req.Header.Set(csrfHeaderName, "1")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

func getPreferences(t *testing.T, srv *Server, cookie *http.Cookie) (*httptest.ResponseRecorder, map[string]json.RawMessage) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/portal/preferences", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	var out map[string]json.RawMessage
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("unmarshal preferences: %v (%s)", err, rec.Body.String())
		}
	}
	return rec, out
}

func TestPortalPreferencesGetEmptyReturnsObject(t *testing.T) {
	srv, dir := newAuthTestServer(t)
	seedLoginUser(t, dir, "usr_p", "p@example.test", "password-1", "user")
	cookie := loginCookie(t, srv, "p@example.test", "password-1")

	rec, out := getPreferences(t, srv, cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET preferences status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if len(out) != 0 {
		t.Fatalf("fresh user preferences = %#v, want empty object", out)
	}
	// Must be an object literal, never null.
	if got := strings.TrimSpace(rec.Body.String()); got != "{}" {
		t.Fatalf("empty preferences body = %q, want {}", got)
	}
}

func TestPortalPreferencesPutThenGetRoundTrip(t *testing.T) {
	srv, dir := newAuthTestServer(t)
	seedLoginUser(t, dir, "usr_p", "p@example.test", "password-1", "user")
	cookie := loginCookie(t, srv, "p@example.test", "password-1")

	// The key deliberately contains a dot to prove dotted keys survive the path.
	rec := putPreference(t, srv, cookie, "table.activity", `{"cols":["a","b"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT preference status = %d, body = %s", rec.Code, rec.Body.String())
	}
	// The frontend request() helper treats an empty body as an error: the PUT must
	// answer 200 with a non-empty JSON body.
	if body := strings.TrimSpace(rec.Body.String()); body == "" {
		t.Fatal("PUT preference returned an empty body; frontend requires a non-empty JSON body")
	} else if !strings.Contains(body, "\"ok\":true") {
		t.Fatalf("PUT preference body = %q, want a JSON object containing \"ok\":true", body)
	}

	_, out := getPreferences(t, srv, cookie)
	raw, ok := out["table.activity"]
	if !ok {
		t.Fatalf("stored key missing from GET result: %#v", out)
	}
	if string(raw) != `{"cols":["a","b"]}` {
		t.Fatalf("stored value = %s, want %s", string(raw), `{"cols":["a","b"]}`)
	}
}

func TestPortalPreferencesPutUpsertOverwrites(t *testing.T) {
	srv, dir := newAuthTestServer(t)
	seedLoginUser(t, dir, "usr_p", "p@example.test", "password-1", "user")
	cookie := loginCookie(t, srv, "p@example.test", "password-1")

	if rec := putPreference(t, srv, cookie, "theme", `"dark"`); rec.Code != http.StatusOK {
		t.Fatalf("first PUT status = %d", rec.Code)
	}
	if rec := putPreference(t, srv, cookie, "theme", `"light"`); rec.Code != http.StatusOK {
		t.Fatalf("upsert PUT status = %d", rec.Code)
	}
	_, out := getPreferences(t, srv, cookie)
	if len(out) != 1 || string(out["theme"]) != `"light"` {
		t.Fatalf("after upsert preferences = %#v, want single theme=light", out)
	}
}

func TestPortalPreferencesPutEmptyKeyRejected(t *testing.T) {
	srv, dir := newAuthTestServer(t)
	seedLoginUser(t, dir, "usr_p", "p@example.test", "password-1", "user")
	cookie := loginCookie(t, srv, "p@example.test", "password-1")

	// Trailing slash with no segment => empty key => 400.
	rec := putPreference(t, srv, cookie, "", `1`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("PUT empty key status = %d, want 400 (body %s)", rec.Code, rec.Body.String())
	}
}

func TestPortalPreferencesPutInvalidJSONRejected(t *testing.T) {
	srv, dir := newAuthTestServer(t)
	seedLoginUser(t, dir, "usr_p", "p@example.test", "password-1", "user")
	cookie := loginCookie(t, srv, "p@example.test", "password-1")

	rec := putPreference(t, srv, cookie, "table.activity", `{not-json`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("PUT invalid JSON status = %d, want 400 (body %s)", rec.Code, rec.Body.String())
	}
}
