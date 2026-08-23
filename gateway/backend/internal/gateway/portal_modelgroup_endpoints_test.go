// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// This file targets portal_modelgroup_endpoints.go's request-shape/dispatch
// branches carrying NEW uncovered lines: each handler's own
// json.Unmarshal-into-typed-struct failure (a syntactically valid `[]` body,
// see auth_error_branches_test.go's file comment), the two method-not-allowed
// defaults, and handlePortalModelGroupItem's empty-id 404. NewTestServer's
// default token carries "admin" (no "system" needed — model-group management
// is gated on plain admin), and none of these branches need a REAL group id.

func TestHandlePortalModelGroupsInvalidJSONReturns400(t *testing.T) {
	srv := NewTestServer()
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, newJSONRequest(http.MethodPost, "/api/portal/model-groups", `[]`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body = %s", rec.Code, rec.Body.String())
	}
	if code := errorBodyOf(t, rec); code != "request.invalid_json" {
		t.Fatalf("error code = %q, want request.invalid_json", code)
	}
}

func TestHandlePortalModelGroupsMethodNotAllowed(t *testing.T) {
	srv := NewTestServer()
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, newJSONRequest(http.MethodDelete, "/api/portal/model-groups", ""))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405, body = %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Allow"); got != "GET, POST" {
		t.Fatalf("Allow header = %q, want %q", got, "GET, POST")
	}
	if code := errorBodyOf(t, rec); code != "request.method_not_allowed" {
		t.Fatalf("error code = %q, want request.method_not_allowed", code)
	}
}

func TestHandlePortalModelGroupItemEmptyIDReturns404(t *testing.T) {
	srv := NewTestServer()
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, newJSONRequest(http.MethodGet, "/api/portal/model-groups/", ""))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body = %s", rec.Code, rec.Body.String())
	}
	if code := errorBodyOf(t, rec); code != "model_group.not_found" {
		t.Fatalf("error code = %q, want model_group.not_found", code)
	}
}

func TestHandlePortalModelGroupItemPutInvalidJSONReturns400(t *testing.T) {
	srv := NewTestServer()
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, newJSONRequest(http.MethodPut, "/api/portal/model-groups/any-id", `[]`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body = %s", rec.Code, rec.Body.String())
	}
	if code := errorBodyOf(t, rec); code != "request.invalid_json" {
		t.Fatalf("error code = %q, want request.invalid_json", code)
	}
}

func TestHandlePortalModelGroupItemMethodNotAllowed(t *testing.T) {
	srv := NewTestServer()
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, newJSONRequest(http.MethodPost, "/api/portal/model-groups/any-id", ""))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405, body = %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Allow"); got != "GET, PUT, DELETE" {
		t.Fatalf("Allow header = %q, want %q", got, "GET, PUT, DELETE")
	}
	if code := errorBodyOf(t, rec); code != "request.method_not_allowed" {
		t.Fatalf("error code = %q, want request.method_not_allowed", code)
	}
}

func TestHandlePortalModelSettingItemInvalidJSONReturns400(t *testing.T) {
	srv := NewTestServer()
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, newJSONRequest(http.MethodPut, "/api/portal/model-settings/qwen-coder", `[]`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body = %s", rec.Code, rec.Body.String())
	}
	if code := errorBodyOf(t, rec); code != "request.invalid_json" {
		t.Fatalf("error code = %q, want request.invalid_json", code)
	}
}
