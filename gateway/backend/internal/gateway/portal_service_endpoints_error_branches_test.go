// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// This file targets portal_service_endpoints.go's request-shape/dispatch
// branches carrying NEW uncovered lines, reusing services_endpoints_test.go's
// newServicesTestFixture/svcAuthedRequest/assertErrorCode helpers: each
// handler's own json.Unmarshal-into-typed-struct failure (a syntactically
// valid `[]` body, see auth_error_branches_test.go's file comment),
// handlePortalServiceItem's empty-id 404, and the standalone
// handleServiceAdminGroupCandidates endpoint (which — like its server-group
// sibling — has never been exercised at all, so even its first line,
// resolving the principal, shows as uncovered new code).

func TestHandlePortalServicesInvalidJSONReturns400(t *testing.T) {
	s := newServicesTestFixture(t)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, svcAuthedRequest(http.MethodPost, "/api/portal/services", `[]`, svcAdminSecret))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body = %s", rec.Code, rec.Body.String())
	}
	assertErrorCode(t, rec, "request.invalid_json")
}

func TestHandlePortalServiceItemEmptyIDReturns404(t *testing.T) {
	s := newServicesTestFixture(t)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, svcAuthedRequest(http.MethodGet, "/api/portal/services/", "", svcAdminSecret))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body = %s", rec.Code, rec.Body.String())
	}
	assertErrorCode(t, rec, "service.not_found")
}

func TestHandlePortalServiceSinglePutInvalidJSONReturns400(t *testing.T) {
	s := newServicesTestFixture(t)
	id := createServiceWithDelegates(t, s)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, svcAuthedRequest(http.MethodPut, "/api/portal/services/"+id, `[]`, svcAdminSecret))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body = %s", rec.Code, rec.Body.String())
	}
	assertErrorCode(t, rec, "request.invalid_json")
}

func TestHandlePortalServiceTokensPostInvalidJSONReturns400(t *testing.T) {
	s := newServicesTestFixture(t)
	id := createServiceWithDelegates(t, s)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, svcAuthedRequest(http.MethodPost, "/api/portal/services/"+id+"/tokens", `[]`, svcAdminSecret))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body = %s", rec.Code, rec.Body.String())
	}
	assertErrorCode(t, rec, "request.invalid_json")
}

// TestHandlePortalServiceTokensPostInvalidModelSettingReturns400 is the
// service-token twin of the user-token create case: a model-valued setting the
// principal cannot route to (an override target, or the unknown-model
// redirect's fallback) is a 400 naming the setting, not a 500 claiming the
// gateway broke. portalServiceErrRows had no row for the error, so it fell
// through to the generic service.token_create_failed.
func TestHandlePortalServiceTokensPostInvalidModelSettingReturns400(t *testing.T) {
	s := newServicesTestFixture(t)
	id := createServiceWithDelegates(t, s)
	rec := httptest.NewRecorder()
	body := `{"name":"t","unknown_model_redirect":true,"unknown_model_fallback":"no-such-model"}`
	s.ServeHTTP(rec, svcAuthedRequest(http.MethodPost, "/api/portal/services/"+id+"/tokens", body, svcAdminSecret))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body = %s", rec.Code, rec.Body.String())
	}
	assertErrorCode(t, rec, "portal.token_model_override_invalid")
}

func TestHandlePortalServiceAdminGroupsInvalidJSONReturns400(t *testing.T) {
	s := newServicesTestFixture(t)
	id := createServiceWithDelegates(t, s)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, svcAuthedRequest(http.MethodPut, "/api/portal/services/"+id+"/admin-groups", `[]`, svcAdminSecret))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body = %s", rec.Code, rec.Body.String())
	}
	assertErrorCode(t, rec, "request.invalid_json")
}

// TestHandleServiceAdminGroupCandidatesReturnsSeededAdminGroup proves the
// standalone GET /api/portal/service-admin-group-candidates endpoint is wired
// end to end for a system-scoped caller: it returns every admin-tier group,
// including svcTestAdminGroupID that newServicesTestFixture seeds.
func TestHandleServiceAdminGroupCandidatesReturnsSeededAdminGroup(t *testing.T) {
	s := newServicesTestFixture(t)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, svcAuthedRequest(http.MethodGet, "/api/portal/service-admin-group-candidates", "", svcAdminSecret))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body = %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v, body = %s", err, rec.Body.String())
	}
	var found bool
	for _, g := range body.Data {
		if g.ID == svcTestAdminGroupID {
			found = true
		}
	}
	if !found {
		t.Fatalf("seeded admin group %q not in candidates: %+v", svcTestAdminGroupID, body.Data)
	}
}
