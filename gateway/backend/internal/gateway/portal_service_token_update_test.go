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

// PATCH /api/portal/services/{id}/tokens/{tid} (#421): the field-level edit of a
// service token. Reuses services_endpoints_test.go's fixture and helpers.

// createServiceTokenForPatch mints a token on serviceID as the admin and returns
// its id and plaintext secret.
func createServiceTokenForPatch(t *testing.T, s *Server, serviceID, body string) (id, secret string) {
	t.Helper()
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, svcAuthedRequest(http.MethodPost, "/api/portal/services/"+serviceID+"/tokens", body, svcAdminSecret))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create token status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var created struct {
		Token struct {
			ID string `json:"id"`
		} `json:"token"`
		Secret string `json:"secret"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("unmarshal create: %v", err)
	}
	return created.Token.ID, created.Secret
}

func TestPortalServiceTokenPatchReturnsTheUpdatedToken(t *testing.T) {
	s := newServicesTestFixture(t)
	id := createServiceWithDelegates(t, s)
	tokID, secret := createServiceTokenForPatch(t, s, id, `{"name":"first","claude_model_aliases":false}`)

	rec := httptest.NewRecorder()
	body := `{"name":"renamed","claude_model_aliases":true,"send_usage_limits":true,"log_communication":true}`
	s.ServeHTTP(rec, svcAuthedRequest(http.MethodPatch, "/api/portal/services/"+id+"/tokens/"+tokID, body, svcTokenSecret))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body = %s", rec.Code, rec.Body.String())
	}
	var raw map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if raw["id"] != tokID || raw["service_id"] != id || raw["name"] != "renamed" {
		t.Fatalf("response = %s", rec.Body.String())
	}
	if raw["claude_model_aliases"] != true || raw["send_usage_limits"] != true || raw["log_communication"] != true {
		t.Fatalf("response switches = %s", rec.Body.String())
	}
	if _, leaked := raw["secret_hash"]; leaked {
		t.Fatalf("response exposes secret_hash: %s", rec.Body.String())
	}
	if secret == "" || strings.Contains(rec.Body.String(), secret) {
		t.Fatalf("no secret minted, or the response leaks it: %s", rec.Body.String())
	}
	if _, wrapped := raw["token"]; wrapped {
		t.Fatalf("PATCH returns the bare ServiceTokenDTO, not a create-style envelope: %s", rec.Body.String())
	}
}

func TestPortalServiceTokenPatchInvalidJSONReturns400(t *testing.T) {
	s := newServicesTestFixture(t)
	id := createServiceWithDelegates(t, s)
	tokID, _ := createServiceTokenForPatch(t, s, id, `{"name":"t"}`)
	for _, body := range []string{`{not json`, `[]`} {
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, svcAuthedRequest(http.MethodPatch, "/api/portal/services/"+id+"/tokens/"+tokID, body, svcAdminSecret))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("body %q: status = %d, want 400, body = %s", body, rec.Code, rec.Body.String())
		}
		assertErrorCode(t, rec, "request.invalid_json")
	}
}

func TestPortalServiceTokenPatchCrossServiceAndUnknownReturn404(t *testing.T) {
	s := newServicesTestFixture(t)
	idA := createServiceWithDelegates(t, s)
	idB := createServiceWithDelegates(t, s)
	tokID, _ := createServiceTokenForPatch(t, s, idA, `{"name":"a"}`)

	for name, path := range map[string]string{
		"cross-service": "/api/portal/services/" + idB + "/tokens/" + tokID,
		"unknown token": "/api/portal/services/" + idA + "/tokens/tok_missing",
	} {
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, svcAuthedRequest(http.MethodPatch, path, `{"name":"x"}`, svcAdminSecret))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s: status = %d, want 404, body = %s", name, rec.Code, rec.Body.String())
		}
		assertErrorCode(t, rec, "service.token_not_found")
	}
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, svcAuthedRequest(http.MethodPatch, "/api/portal/services/"+idA+"/tokens/"+tokID, `{"name":"x"}`, svcStrangerSecret))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("stranger: status = %d, want 404, body = %s", rec.Code, rec.Body.String())
	}
	assertErrorCode(t, rec, "service.not_found")
}

// TestPortalServiceTokenPatchEffortFlagFromInferenceBearerReturns403: the fixture's
// delegate principals are inference-only bearers, so any claude_effort_compatibility
// value is the shared 403 portal.principal_forbidden; the admin-scope bearer may.
func TestPortalServiceTokenPatchEffortFlagFromInferenceBearerReturns403(t *testing.T) {
	s := newServicesTestFixture(t)
	id := createServiceWithDelegates(t, s)
	tokID, _ := createServiceTokenForPatch(t, s, id, `{"name":"e"}`)
	path := "/api/portal/services/" + id + "/tokens/" + tokID

	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, svcAuthedRequest(http.MethodPatch, path, `{"claude_effort_compatibility":false}`, svcFullSecret))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("delegate bearer: status = %d, want 403, body = %s", rec.Code, rec.Body.String())
	}
	assertErrorCode(t, rec, "portal.principal_forbidden")

	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, svcAuthedRequest(http.MethodPatch, path, `{"claude_effort_compatibility":true}`, svcAdminSecret))
	if rec.Code != http.StatusOK {
		t.Fatalf("admin bearer: status = %d, want 200, body = %s", rec.Code, rec.Body.String())
	}
}

// TestPortalServiceTokenPatchValidationErrorsMapLikeCreate: every client error the
// update can return maps to the status and code the create path uses.
func TestPortalServiceTokenPatchValidationErrorsMapLikeCreate(t *testing.T) {
	s := newServicesTestFixture(t)
	id := createServiceWithDelegates(t, s)
	tokID, _ := createServiceTokenForPatch(t, s, id, `{"name":"v"}`)
	cases := []struct {
		name, body, code string
	}{
		{"blank name", `{"name":"  "}`, "service.validation_failed"},
		{"unknown fallback", `{"unknown_model_redirect":true,"unknown_model_fallback":"no-such-model"}`, "portal.token_model_override_invalid"},
		{"unknown override", `{"model_override":"no-such-model"}`, "portal.token_model_override_invalid"},
		{"unreachable vendor account", `{"vendor_access":{"accounts":[{"account_id":"acc_not_reachable"}]}}`, "portal.token_vendor_access_invalid"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			s.ServeHTTP(rec, svcAuthedRequest(http.MethodPatch, "/api/portal/services/"+id+"/tokens/"+tokID, tc.body, svcAdminSecret))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400, body = %s", rec.Code, rec.Body.String())
			}
			assertErrorCode(t, rec, tc.code)
		})
	}
}
