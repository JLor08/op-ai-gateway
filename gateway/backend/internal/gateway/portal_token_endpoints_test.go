// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/routing"
	"strings"
	"testing"
	"time"
)

// This file targets portal_token_endpoints.go's request-shape/dispatch
// branches carrying NEW uncovered lines: the collection's own
// json.Unmarshal-into-typed-struct failure and method-not-allowed default,
// handlePortalTokenItem's unknown-subpath 404, handlePortalTokenSingle's
// empty-id 404 and PATCH-invalid-JSON/method-not-allowed branches, and
// handlePortalTokenRotate's empty-id 404. None need a REAL token id.

func TestHandlePortalTokensInvalidJSONReturns400(t *testing.T) {
	srv := NewTestServer()
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, newJSONRequest(http.MethodPost, "/api/portal/tokens", `[]`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body = %s", rec.Code, rec.Body.String())
	}
	if code := errorBodyOf(t, rec); code != "request.invalid_json" {
		t.Fatalf("error code = %q, want request.invalid_json", code)
	}
}

func TestHandlePortalTokensMethodNotAllowed(t *testing.T) {
	srv := NewTestServer()
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, newJSONRequest(http.MethodDelete, "/api/portal/tokens", ""))
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

// TestHandlePortalTokenItemUnknownSubPathReturns404 proves a sub-path other
// than the bare id or "/{id}/rotate" 404s at the dispatcher itself, before
// ever resolving a principal.
func TestHandlePortalTokenItemUnknownSubPathReturns404(t *testing.T) {
	srv := NewTestServer()
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, newJSONRequest(http.MethodGet, "/api/portal/tokens/any-id/bogus", ""))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body = %s", rec.Code, rec.Body.String())
	}
	if code := errorBodyOf(t, rec); code != "portal.token_not_found" {
		t.Fatalf("error code = %q, want portal.token_not_found", code)
	}
}

// TestHandlePortalTokenSingleEmptyIDReturns404 proves a trailing-slash path
// with no id segment (GET /api/portal/tokens/) reaches handlePortalTokenSingle
// with id=="" and 404s AFTER resolving the principal (requireWebScope runs
// first), not before.
func TestHandlePortalTokenSingleEmptyIDReturns404(t *testing.T) {
	srv := NewTestServer()
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, newJSONRequest(http.MethodPatch, "/api/portal/tokens/", `{}`))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body = %s", rec.Code, rec.Body.String())
	}
	if code := errorBodyOf(t, rec); code != "portal.token_not_found" {
		t.Fatalf("error code = %q, want portal.token_not_found", code)
	}
}

func TestHandlePortalTokenSinglePatchInvalidJSONReturns400(t *testing.T) {
	srv := NewTestServer()
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, newJSONRequest(http.MethodPatch, "/api/portal/tokens/any-id", `[]`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body = %s", rec.Code, rec.Body.String())
	}
	if code := errorBodyOf(t, rec); code != "request.invalid_json" {
		t.Fatalf("error code = %q, want request.invalid_json", code)
	}
}

func TestHandlePortalTokenSingleMethodNotAllowed(t *testing.T) {
	srv := NewTestServer()
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, newJSONRequest(http.MethodGet, "/api/portal/tokens/any-id", ""))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405, body = %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Allow"); got != "PATCH, DELETE" {
		t.Fatalf("Allow header = %q, want %q", got, "PATCH, DELETE")
	}
	if code := errorBodyOf(t, rec); code != "request.method_not_allowed" {
		t.Fatalf("error code = %q, want request.method_not_allowed", code)
	}
}

// TestHandlePortalTokenRotateEmptyIDReturns404 proves an empty id segment
// before "/rotate" 404s AFTER the method check, mirroring
// handlePortalTokenSingle's ordering. This calls handlePortalTokenRotate
// directly with id=="" rather than routing a literal "//rotate" URL through
// ServeHTTP: net/http.ServeMux cleans a double-slash path and 307-redirects
// before the handler ever runs, which would test the mux's path-cleaning
// behavior instead of this handler's own empty-id branch.
func TestHandlePortalTokenRotateEmptyIDReturns404(t *testing.T) {
	srv := NewTestServer()
	rec := httptest.NewRecorder()
	srv.handlePortalTokenRotate(rec, newJSONRequest(http.MethodPost, "/api/portal/tokens//rotate", ""), "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body = %s", rec.Code, rec.Body.String())
	}
	if code := errorBodyOf(t, rec); code != "portal.token_not_found" {
		t.Fatalf("error code = %q, want portal.token_not_found", code)
	}
}

// TestHandlePortalTokensCreateInvalidModelSettingReturns400 pins that a
// rejected model-valued setting reaches the client as the SAME 400 +
// portal.token_model_override_invalid the PATCH path already returns. The
// collection handler maps its errors inline instead of through
// writePortalTokenError, and this case was missing from that list — so a
// typo'd override target (and now a typo'd redirect fallback, which shares the
// error deliberately) surfaced as a 500 "token could not be created" on create
// only, telling the operator the gateway broke rather than that the name is
// wrong.
func TestHandlePortalTokensCreateInvalidModelSettingReturns400(t *testing.T) {
	srv := NewTestServer()
	rec := httptest.NewRecorder()
	body := `{"name":"redirect-token","unknown_model_redirect":true,"unknown_model_fallback":"no-such-model"}`
	srv.ServeHTTP(rec, newJSONRequest(http.MethodPost, "/api/portal/tokens", body))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body = %s", rec.Code, rec.Body.String())
	}
	if code := errorBodyOf(t, rec); code != "portal.token_model_override_invalid" {
		t.Fatalf("error code = %q, want portal.token_model_override_invalid", code)
	}
}

// seedTokenVendorAccounts gives the test server's dev user (usr_dev, the owner of
// every token these requests create) two vendor accounts, acc_a ("a/") and acc_b
// ("b/"), that each serve upstream model gpt-4o.
func seedTokenVendorAccounts(t *testing.T, srv *Server) {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	for _, prefix := range []string{"a", "b"} {
		id := "acc_" + prefix
		if err := srv.Routes.CreateVendorAccount(ctx, routing.VendorAccount{
			ID: id, OwnerUserID: "usr_dev", Vendor: routing.VendorOpenAI, AuthType: routing.VendorAuthAPIKey,
			Name: id, Status: routing.VendorAccountStatusActive, ModelPrefix: prefix + "/", CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("CreateVendorAccount %s: %v", id, err)
		}
		if err := srv.Routes.SetVendorAccountModels(ctx, id, []routing.VendorAccountModel{
			{GatewayModel: prefix + "/gpt-4o", UpstreamModel: "gpt-4o", APIFlavor: routing.APIFlavorOpenAI},
		}); err != nil {
			t.Fatalf("SetVendorAccountModels %s: %v", id, err)
		}
	}
}

// TestPortalTokensVendorAccessErrorsMapToBadRequest pins that both new vendor-
// access sentinels reach the client as a 400 with their own code on BOTH ladders:
// the hand-inlined one in handlePortalTokens (create) and portalTokenErrRows
// (PATCH). A sentinel missing from either one falls through to a generic 500.
func TestPortalTokensVendorAccessErrorsMapToBadRequest(t *testing.T) {
	invalid := `{"vendor_access":{"accounts":[{"account_id":"acc_not_mine"}]}}`
	conflict := `{"vendor_access":{"accounts":[` +
		`{"account_id":"acc_a","prefix_override":{"enabled":true,"value":""}},` +
		`{"account_id":"acc_b","prefix_override":{"enabled":true,"value":""}}]}}`
	cases := []struct {
		name, body, code string
	}{
		{"invalid", invalid, "portal.token_vendor_access_invalid"},
		{"conflict", conflict, "portal.token_vendor_access_conflict"},
	}
	for _, tc := range cases {
		t.Run("create/"+tc.name, func(t *testing.T) {
			srv := NewTestServer()
			seedTokenVendorAccounts(t, srv)
			body := `{"name":"va-` + tc.name + `","scopes":["gateway:use"],` + tc.body[1:]
			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, newJSONRequest(http.MethodPost, "/api/portal/tokens", body))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400, body = %s", rec.Code, rec.Body.String())
			}
			if code := errorBodyOf(t, rec); code != tc.code {
				t.Fatalf("error code = %q, want %q", code, tc.code)
			}
		})
		t.Run("patch/"+tc.name, func(t *testing.T) {
			srv := NewTestServer()
			seedTokenVendorAccounts(t, srv)
			id := createEditableToken(t, srv, "va-patch-"+tc.name)
			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, newJSONRequest(http.MethodPatch, "/api/portal/tokens/"+id, tc.body))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400, body = %s", rec.Code, rec.Body.String())
			}
			if code := errorBodyOf(t, rec); code != tc.code {
				t.Fatalf("error code = %q, want %q", code, tc.code)
			}
		})
	}
}

// TestPortalTokensVendorAccessRoundTrip drives the happy path over HTTP: create
// with a policy, read it back from the list, PATCH an unrelated field (policy
// kept), then PATCH it to the strict default.
func TestPortalTokensVendorAccessRoundTrip(t *testing.T) {
	srv := NewTestServer()
	seedTokenVendorAccounts(t, srv)

	type vendorAccess struct {
		All      bool `json:"all"`
		Accounts []struct {
			AccountID      string `json:"account_id"`
			PrefixOverride *struct {
				Enabled bool   `json:"enabled"`
				Value   string `json:"value"`
			} `json:"prefix_override"`
		} `json:"accounts"`
	}
	decodeToken := func(rec *httptest.ResponseRecorder, wrapped bool) (string, *vendorAccess) {
		t.Helper()
		var tok struct {
			ID           string        `json:"id"`
			VendorAccess *vendorAccess `json:"vendor_access"`
		}
		if wrapped {
			var body struct {
				Token struct {
					ID           string        `json:"id"`
					VendorAccess *vendorAccess `json:"vendor_access"`
				} `json:"token"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("unmarshal: %v, body = %s", err, rec.Body.String())
			}
			return body.Token.ID, body.Token.VendorAccess
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &tok); err != nil {
			t.Fatalf("unmarshal: %v, body = %s", err, rec.Body.String())
		}
		return tok.ID, tok.VendorAccess
	}

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, newJSONRequest(http.MethodPost, "/api/portal/tokens",
		`{"name":"va-ok","scopes":["gateway:use"],"vendor_access":{"accounts":[`+
			`{"account_id":"acc_a","prefix_override":{"enabled":true,"value":""}},{"account_id":"acc_b"}]}}`))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body = %s", rec.Code, rec.Body.String())
	}
	id, va := decodeToken(rec, true)
	if va == nil || len(va.Accounts) != 2 || va.Accounts[0].PrefixOverride == nil || !va.Accounts[0].PrefixOverride.Enabled || va.Accounts[1].PrefixOverride != nil {
		t.Fatalf("created policy = %+v", va)
	}

	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, newJSONRequest(http.MethodPatch, "/api/portal/tokens/"+id, `{"name":"va-ok-renamed"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("patch status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if _, va = decodeToken(rec, false); va == nil || len(va.Accounts) != 2 {
		t.Fatalf("policy after an unrelated PATCH = %+v, want it kept", va)
	}

	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, newJSONRequest(http.MethodPatch, "/api/portal/tokens/"+id, `{"vendor_access":{"all":false,"accounts":[]}}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("reset status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if _, va = decodeToken(rec, false); va != nil {
		t.Fatalf("policy after reset = %+v, want omitted", va)
	}
}

// claudeCodeSecretPrefix is the JWT-looking prefix a claude_code_usage_display
// token's secret starts with (header + payload segments and their dots).
const claudeCodeSecretPrefix = "eyJhbGciOiJub25lIiwidHlwIjoiSldUIn0.eyJhdWQiOiJjbGF1ZGUtZ2F0ZXdheSJ9.opaigw_"

func rotateTokenSecretOf(t *testing.T, srv *Server, id, body string) (int, string, portalTokenRotateBody) {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, newJSONRequest(http.MethodPost, "/api/portal/tokens/"+id+"/rotate", body))
	var out portalTokenRotateBody
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode rotate response: %v (%s)", err, rec.Body.String())
		}
	}
	return rec.Code, rec.Body.String(), out
}

type portalTokenRotateBody struct {
	Secret string `json:"secret"`
	Token  struct {
		ClaudeCodeUsageDisplay bool `json:"claude_code_usage_display"`
	} `json:"token"`
}

// TestPortalTokenRotateClaudeCodeUsageDisplay pins the rotate endpoint's
// optional body (#407): {"claude_code_usage_display":true} switches the token to
// the Claude Code secret shape, an empty body keeps the current shape, a
// malformed body is a 400, and a PATCH that would change the flag is refused
// with the stable code (the same value is accepted).
func TestPortalTokenRotateClaudeCodeUsageDisplay(t *testing.T) {
	srv := NewTestServer()
	id := createEditableToken(t, srv, "cc-rotate")

	code, body, got := rotateTokenSecretOf(t, srv, id, `{"claude_code_usage_display": true}`)
	if code != http.StatusOK || !strings.HasPrefix(got.Secret, claudeCodeSecretPrefix) || !got.Token.ClaudeCodeUsageDisplay {
		t.Fatalf("rotate(true) = %d %s", code, body)
	}

	// An empty body is today's behaviour: the shape is kept.
	code, body, got = rotateTokenSecretOf(t, srv, id, "")
	if code != http.StatusOK || !strings.HasPrefix(got.Secret, claudeCodeSecretPrefix) || !got.Token.ClaudeCodeUsageDisplay {
		t.Fatalf("rotate(empty) = %d %s", code, body)
	}

	// Malformed JSON is a 400.
	code, body, _ = rotateTokenSecretOf(t, srv, id, `{"claude_code_usage_display":`)
	if code != http.StatusBadRequest || !strings.Contains(body, "request.invalid_json") {
		t.Fatalf("rotate(malformed) = %d %s, want 400 request.invalid_json", code, body)
	}

	// PATCH with the stored value is accepted, a different one is refused.
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, newJSONRequest(http.MethodPatch, "/api/portal/tokens/"+id, `{"claude_code_usage_display": true}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH same value = %d %s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, newJSONRequest(http.MethodPatch, "/api/portal/tokens/"+id, `{"claude_code_usage_display": false}`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("PATCH different value = %d, want 400 (%s)", rec.Code, rec.Body.String())
	}
	if c := errorBodyOf(t, rec); c != "portal.token_claude_code_usage_display_requires_rotation" {
		t.Fatalf("PATCH error code = %q", c)
	}

	// Rotating back to false returns an opaque secret.
	code, body, got = rotateTokenSecretOf(t, srv, id, `{"claude_code_usage_display": false}`)
	if code != http.StatusOK || !strings.HasPrefix(got.Secret, "opaigw_") || got.Token.ClaudeCodeUsageDisplay {
		t.Fatalf("rotate(false) = %d %s", code, body)
	}
}

// TestPortalTokenRotateWithoutBodyKeepsOpaqueShape: a plain rotate of an
// ordinary token still returns an opaque opaigw_ secret.
func TestPortalTokenRotateWithoutBodyKeepsOpaqueShape(t *testing.T) {
	srv := NewTestServer()
	id := createEditableToken(t, srv, "plain-rotate")
	code, body, got := rotateTokenSecretOf(t, srv, id, "")
	if code != http.StatusOK || !strings.HasPrefix(got.Secret, "opaigw_") || got.Token.ClaudeCodeUsageDisplay {
		t.Fatalf("rotate(empty) = %d %s", code, body)
	}
}
