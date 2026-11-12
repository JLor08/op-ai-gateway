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

// TestPortalApplicationDecisionModesReachTheWire pins the decisions_mode and
// systemone_mode wire contract: defaults on create, the existing
// application.endpoint_mode_invalid for a bad value, and the llama_cpp
// passthrough guard (400 on create, 409 on an update that keeps the type).
func TestPortalApplicationDecisionModesReachTheWire(t *testing.T) {
	srv := NewTestServerWithGroups([]string{"gateway:use", "admin"})
	serverID := newProxyExclusionTestServer(t, srv, "decision-modes.example.test")
	post := func(body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, newJSONRequest(http.MethodPost, "/api/portal/servers/"+serverID+"/applications", body))
		return rec
	}

	rec := post(`{"type":"vllm","port":8400,"scheme":"http"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var created struct {
		ID            string `json:"id"`
		DecisionsMode string `json:"decisions_mode"`
		SystemOneMode string `json:"systemone_mode"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if created.DecisionsMode != "auto" || created.SystemOneMode != "passthrough" {
		t.Fatalf("defaults = %q/%q", created.DecisionsMode, created.SystemOneMode)
	}

	for _, tc := range []struct {
		name, body, code string
		status           int
	}{
		{"bad decisions_mode", `{"type":"vllm","port":8401,"scheme":"http","decisions_mode":"bogus"}`, "application.endpoint_mode_invalid", http.StatusBadRequest},
		{"systemone translate", `{"type":"vllm","port":8402,"scheme":"http","systemone_mode":"translate"}`, "application.endpoint_mode_invalid", http.StatusBadRequest},
		{"llama_cpp passthrough", `{"type":"llama_cpp","port":8403,"scheme":"http","decisions_mode":"passthrough"}`, "application.decisions_passthrough_unsupported", http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := post(tc.body)
			if rec.Code != tc.status || errorBodyOf(t, rec) != tc.code {
				t.Fatalf("status = %d code = %q, want %d %s, body = %s", rec.Code, errorBodyOf(t, rec), tc.status, tc.code, rec.Body.String())
			}
		})
	}

	t.Run("update stored llama_cpp to passthrough is 409", func(t *testing.T) {
		rec := post(`{"type":"llama_cpp","port":8404,"scheme":"http"}`)
		var llama struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &llama); err != nil || llama.ID == "" {
			t.Fatalf("create llama_cpp: %v, body = %s", err, rec.Body.String())
		}
		patch := httptest.NewRecorder()
		srv.ServeHTTP(patch, newJSONRequest(http.MethodPatch, "/api/portal/applications/"+llama.ID, `{"decisions_mode":"passthrough"}`))
		if patch.Code != http.StatusConflict || errorBodyOf(t, patch) != "application.decisions_passthrough_conflict" {
			t.Fatalf("status = %d code = %q, body = %s", patch.Code, errorBodyOf(t, patch), patch.Body.String())
		}
	})
}
