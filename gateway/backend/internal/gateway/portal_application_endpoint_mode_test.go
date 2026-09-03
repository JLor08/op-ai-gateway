// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestPortalApplicationEndpointModeErrorsReachTheWire pins the WIRE contract
// (status + code) of portal.ErrApplicationEndpointModeInvalid on both the
// application create (POST) and update (PATCH) paths.
//
// This sentinel has existed at the internal/portal service level since the
// responses_mode/messages_mode variant-endpoint work landed, and was
// unit-tested there with errors.Is -- but it was never added to
// portalApplicationErrRows (portal_application_endpoints.go), so it fell
// straight through to the 500 "application.request_failed" fallback: a
// caller's own bad responses_mode/messages_mode reported as a server fault.
// Its sibling portal.ErrApplicationFlavorInvalid IS wired to 400
// "application.flavor_invalid" in that same table, which is the precedent
// this row follows.
func TestPortalApplicationEndpointModeErrorsReachTheWire(t *testing.T) {
	srv := NewTestServerWithGroups([]string{"gateway:use", "admin"})
	serverID := newProxyExclusionTestServer(t, srv, "endpoint-mode.example.test")

	t.Run("create with bad responses_mode", func(t *testing.T) {
		rec := httptest.NewRecorder()
		body := `{"type":"vllm","port":8000,"scheme":"http","responses_mode":"bogus"}`
		srv.ServeHTTP(rec, newJSONRequest(http.MethodPost, "/api/portal/servers/"+serverID+"/applications", body))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400, body = %s", rec.Code, rec.Body.String())
		}
		if code := errorBodyOf(t, rec); code != "application.endpoint_mode_invalid" {
			t.Fatalf("error code = %q, want application.endpoint_mode_invalid, body = %s", code, rec.Body.String())
		}
	})

	t.Run("update with bad messages_mode", func(t *testing.T) {
		createRec := httptest.NewRecorder()
		srv.ServeHTTP(createRec, newJSONRequest(http.MethodPost, "/api/portal/servers/"+serverID+"/applications",
			`{"type":"vllm","port":8001,"scheme":"http"}`))
		if createRec.Code != http.StatusCreated {
			t.Fatalf("create application status = %d, body = %s", createRec.Code, createRec.Body.String())
		}
		var created struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(createRec.Body.Bytes(), &created); err != nil {
			t.Fatalf("unmarshal application: %v, body = %s", err, createRec.Body.String())
		}

		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, newJSONRequest(http.MethodPatch, "/api/portal/applications/"+created.ID,
			`{"messages_mode":"bogus"}`))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400, body = %s", rec.Code, rec.Body.String())
		}
		if code := errorBodyOf(t, rec); code != "application.endpoint_mode_invalid" {
			t.Fatalf("error code = %q, want application.endpoint_mode_invalid, body = %s", code, rec.Body.String())
		}
	})
}
