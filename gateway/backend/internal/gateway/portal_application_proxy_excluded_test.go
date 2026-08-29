// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// newProxyExclusionTestServer POSTs an ordinary AI server the proxy-exclusion
// handler tests can hang applications off, and returns its id.
func newProxyExclusionTestServer(t *testing.T, srv *Server, domain string) string {
	t.Helper()
	body := `{"name":"Proxy Host","domain":"` + domain + `","owner_ids":["usr_dev"],"admin_group_ids":["` + testAdminGroupID + `"]}`
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, newJSONRequest(http.MethodPost, "/api/portal/servers", body))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create server status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("unmarshal server: %v, body = %s", err, rec.Body.String())
	}
	return created.ID
}

// TestPortalApplicationProxyErrorsReachTheWire covers FOUR sentinels, two of
// which are a PRE-EXISTING defect this change fixes in passing.
//
// ErrApplicationProxyListenPortInvalid and ErrApplicationProxyListenPortConflict
// have been returned by the application service since migration 59 and appeared
// in NEITHER portalApplicationErrRows NOR sharedErrorMap, so both fell straight
// through to the 500 "application.request_failed" fallback: a caller's own bad
// port reported as a server fault, with no code to act on. They are asserted
// here as 400 and 409 respectively.
//
// The two new ones are 409 rather than 400 for the reason
// ErrServerManagedRuntimeOnly already records: the request SHAPE is fine, it
// conflicts with the target's own state.
func TestPortalApplicationProxyErrorsReachTheWire(t *testing.T) {
	cases := []struct {
		name       string
		port       int
		appBody    string
		wantStatus int
		wantCode   string
	}{
		{
			name:       "proxy listen port out of range",
			port:       8000,
			appBody:    `{"type":"vllm","port":8000,"scheme":"http","proxy_listen_port":70000}`,
			wantStatus: http.StatusBadRequest,
			wantCode:   "application.proxy_listen_port_invalid",
		},
		{
			name:       "excluded application may not hold a proxy listen port",
			port:       8002,
			appBody:    `{"type":"vllm","port":8002,"scheme":"http","proxy_excluded":true,"proxy_listen_port":9000}`,
			wantStatus: http.StatusConflict,
			wantCode:   "application.proxy_excluded_port_conflict",
		},
		{
			name:       "a participating application must serve plaintext on its own port",
			port:       8003,
			appBody:    `{"type":"vllm","port":8003,"scheme":"https","proxy_excluded":false}`,
			wantStatus: http.StatusConflict,
			wantCode:   "application.proxy_entry_scheme",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := NewTestServerWithGroups([]string{"gateway:use", "admin"})
			serverID := newProxyExclusionTestServer(t, srv, "proxy-err.example.test")
			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, newJSONRequest(http.MethodPost, "/api/portal/servers/"+serverID+"/applications", c.appBody))
			if rec.Code != c.wantStatus {
				t.Fatalf("status = %d, want %d, body = %s", rec.Code, c.wantStatus, rec.Body.String())
			}
			if code := errorBodyOf(t, rec); code != c.wantCode {
				t.Fatalf("error code = %q, want %q", code, c.wantCode)
			}
		})
	}

	// The conflict case needs a SECOND application holding the port, so it gets
	// its own body rather than a table row.
	t.Run("proxy listen port already taken on this server", func(t *testing.T) {
		srv := NewTestServerWithGroups([]string{"gateway:use", "admin"})
		serverID := newProxyExclusionTestServer(t, srv, "proxy-conflict.example.test")
		first := httptest.NewRecorder()
		srv.ServeHTTP(first, newJSONRequest(http.MethodPost, "/api/portal/servers/"+serverID+"/applications",
			`{"type":"vllm","port":8100,"scheme":"http","proxy_listen_port":8600}`))
		if first.Code != http.StatusCreated {
			t.Fatalf("seed application status = %d, body = %s", first.Code, first.Body.String())
		}
		second := httptest.NewRecorder()
		srv.ServeHTTP(second, newJSONRequest(http.MethodPost, "/api/portal/servers/"+serverID+"/applications",
			`{"type":"vllm","port":8101,"scheme":"http","proxy_listen_port":8600}`))
		if second.Code != http.StatusConflict {
			t.Fatalf("status = %d, want 409, body = %s", second.Code, second.Body.String())
		}
		if code := errorBodyOf(t, second); code != "application.proxy_listen_port_conflict" {
			t.Fatalf("error code = %q, want application.proxy_listen_port_conflict", code)
		}
	})
}

// TestPortalApplicationProxyExcludedRoundTripsOnTheWire pins the field on the
// wire in both directions, including the part a pointer-vs-bool mistake would
// break silently: a PATCH that omits proxy_excluded must LEAVE the stored
// decision alone, which is what the portal form's seed-diff depends on.
func TestPortalApplicationProxyExcludedRoundTripsOnTheWire(t *testing.T) {
	srv := NewTestServerWithGroups([]string{"gateway:use", "admin"})
	serverID := newProxyExclusionTestServer(t, srv, "proxy-roundtrip.example.test")

	createRec := httptest.NewRecorder()
	srv.ServeHTTP(createRec, newJSONRequest(http.MethodPost, "/api/portal/servers/"+serverID+"/applications",
		`{"type":"vllm","port":8200,"scheme":"http","proxy_excluded":true}`))
	if createRec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body = %s", createRec.Code, createRec.Body.String())
	}
	var created struct {
		ID              string `json:"id"`
		Scheme          string `json:"scheme"`
		ProxyExcluded   bool   `json:"proxy_excluded"`
		ProxyListenPort int    `json:"proxy_listen_port"`
		Endpoint        string `json:"endpoint"`
	}
	if err := json.Unmarshal(createRec.Body.Bytes(), &created); err != nil {
		t.Fatalf("unmarshal: %v, body = %s", err, createRec.Body.String())
	}
	if !created.ProxyExcluded || created.Scheme != "http" || created.ProxyListenPort != 0 {
		t.Fatalf("created = %#v, want an excluded http application with no listener", created)
	}
	if created.Endpoint != "http://proxy-roundtrip.example.test:8200" {
		t.Fatalf("endpoint = %q, want the application's own plaintext port", created.Endpoint)
	}

	// An unrelated PATCH: proxy_excluded absent must mean "keep".
	patchRec := httptest.NewRecorder()
	srv.ServeHTTP(patchRec, newJSONRequest(http.MethodPatch, "/api/portal/applications/"+created.ID, `{"weight":5}`))
	if patchRec.Code != http.StatusOK {
		t.Fatalf("patch status = %d, body = %s", patchRec.Code, patchRec.Body.String())
	}
	var patched struct {
		ProxyExcluded bool `json:"proxy_excluded"`
		Weight        int  `json:"weight"`
	}
	if err := json.Unmarshal(patchRec.Body.Bytes(), &patched); err != nil {
		t.Fatalf("unmarshal patched: %v, body = %s", err, patchRec.Body.String())
	}
	if !patched.ProxyExcluded {
		t.Fatalf("an unrelated PATCH cleared proxy_excluded: %#v", patched)
	}
	if patched.Weight != 5 {
		t.Fatalf("weight = %d, want 5", patched.Weight)
	}

	// And back in, explicitly, with the scheme that makes it legal.
	backRec := httptest.NewRecorder()
	srv.ServeHTTP(backRec, newJSONRequest(http.MethodPatch, "/api/portal/applications/"+created.ID,
		`{"proxy_excluded":false,"scheme":"http"}`))
	if backRec.Code != http.StatusOK {
		t.Fatalf("re-entry status = %d, body = %s", backRec.Code, backRec.Body.String())
	}
	var back struct {
		ProxyExcluded bool `json:"proxy_excluded"`
	}
	if err := json.Unmarshal(backRec.Body.Bytes(), &back); err != nil {
		t.Fatalf("unmarshal re-entry: %v, body = %s", err, backRec.Body.String())
	}
	if back.ProxyExcluded {
		t.Fatalf("proxy_excluded = true after an explicit false")
	}
}
