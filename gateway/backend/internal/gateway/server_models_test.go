// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestHandlePortalServerModelsManagerSeesOfferedModels proves the new GET
// /api/portal/servers/{id}/models route returns the server's distinct
// offered gateway models for a caller who manages it (here via ServerOwners,
// reusing the real ownership graph seeded by newServerOverrideGatewayEnv —
// same env the T4 server_override HTTP tests use).
func TestHandlePortalServerModelsManagerSeesOfferedModels(t *testing.T) {
	env := newServerOverrideGatewayEnv(t)

	rec := httptest.NewRecorder()
	env.srv.ServeHTTP(rec, newJSONRequest(http.MethodGet, "/api/portal/servers/"+env.srvHealthy+"/models", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Data []struct {
			ID          string `json:"id"`
			DisplayName string `json:"display_name"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v, body=%s", err, rec.Body.String())
	}
	if len(body.Data) != 1 || body.Data[0].ID != "override-model" || body.Data[0].DisplayName != "override-model" {
		t.Fatalf("data = %+v, want exactly one {override-model, override-model}", body.Data)
	}
}

// TestHandlePortalServerModelsNonManagerReturns404NoLeak proves a caller who
// does NOT manage the server (usr_dev never owns srvUnowned's sibling once
// ownership is stripped, mirroring the T4 outlived-grant scenario) gets the
// same 404 server.not_found as an unknown id — never a distinguishable 403.
func TestHandlePortalServerModelsNonManagerReturns404NoLeak(t *testing.T) {
	env := newServerOverrideGatewayEnv(t)
	if err := env.srv.Routes.SetServerOwners(t.Context(), env.srvUnowned, nil); err != nil {
		t.Fatalf("revoke owners: %v", err)
	}

	rec := httptest.NewRecorder()
	env.srv.ServeHTTP(rec, newJSONRequest(http.MethodGet, "/api/portal/servers/"+env.srvUnowned+"/models", ""))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, body = %s, want 404", rec.Code, rec.Body.String())
	}
	if code := errorBodyOf(t, rec); code != "server.not_found" {
		t.Fatalf("error code = %q, want server.not_found", code)
	}
}

// TestHandlePortalServerModelsUnknownServerReturns404 mirrors the no-leak
// property for an id that never existed at all.
func TestHandlePortalServerModelsUnknownServerReturns404(t *testing.T) {
	env := newServerOverrideGatewayEnv(t)

	rec := httptest.NewRecorder()
	env.srv.ServeHTTP(rec, newJSONRequest(http.MethodGet, "/api/portal/servers/srv_does_not_exist/models", ""))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, body = %s, want 404", rec.Code, rec.Body.String())
	}
	if code := errorBodyOf(t, rec); code != "server.not_found" {
		t.Fatalf("error code = %q, want server.not_found", code)
	}
}

// TestHandlePortalServerModelsRejectsNonGet proves the route is GET-only.
func TestHandlePortalServerModelsRejectsNonGet(t *testing.T) {
	env := newServerOverrideGatewayEnv(t)

	rec := httptest.NewRecorder()
	env.srv.ServeHTTP(rec, newJSONRequest(http.MethodPost, "/api/portal/servers/"+env.srvHealthy+"/models", ""))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, body = %s, want 405", rec.Code, rec.Body.String())
	}
}
