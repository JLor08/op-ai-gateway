// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/certissue"
	"op-ai-gateway/internal/portal"
	"strings"
	"testing"
)

// This file targets certificates.go's remaining NEW uncovered lines: three
// handlers' own json.Unmarshal-into-typed-struct failures (a syntactically
// valid `[]` body, see auth_error_branches_test.go's file comment), and the
// portal.ErrPrincipalForbidden -> 403 mapping in
// handleSystemCertificateRenew/ReissueAllCertificates/RotateCertificateCA.
//
// The three ErrPrincipalForbidden branches are DEFENSE-IN-DEPTH: the HTTP
// layer already gates every one of these handlers on requireWebScope(...,
// "system") using the exact same token later passed straight through to the
// portal.Service call, and the service's own guard is the identical
// isSystem() == HasScope("system") check on that SAME token — so through the
// real HTTP path the two checks can never diverge, and the branch is
// unreachable black-box. forcedPrincipalForbiddenPortal makes the portal
// layer return ErrPrincipalForbidden anyway, so this test verifies the
// GATEWAY's own error-code mapping for that outcome (exactly the contract
// these lines encode) rather than trying to fabricate a real divergence.

// forcedPrincipalForbiddenPortal wraps a real portal.API and forces the
// three certificate write methods gated on isSystem() to return
// portal.ErrPrincipalForbidden unconditionally, regardless of the caller's
// actual scope, so the GATEWAY's own error mapping can be exercised in
// isolation from the (unreachable through HTTP) service-level guard.
type forcedPrincipalForbiddenPortal struct {
	portal.API
}

func (forcedPrincipalForbiddenPortal) RenewCertificateNow(context.Context, auth.Token, string) error {
	return portal.ErrPrincipalForbidden
}

func (forcedPrincipalForbiddenPortal) ReissueAllCertificates(context.Context, auth.Token) error {
	return portal.ErrPrincipalForbidden
}

func (forcedPrincipalForbiddenPortal) RotateCertificateCA(context.Context, auth.Token) error {
	return portal.ErrPrincipalForbidden
}

func TestHandleSystemCertificateRenewInvalidJSONReturns400(t *testing.T) {
	srv, _ := newTestServerWithACME(t, certissue.NewMemoryChallengeStore())
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, certSystemRequest(t, http.MethodPost, "/api/system/certificates/renew", strings.NewReader(`[]`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "request.invalid_json") {
		t.Fatalf("body = %s, want request.invalid_json", rec.Body.String())
	}
}

func TestHandleSystemCertificateRenewPrincipalForbiddenMapsTo403(t *testing.T) {
	srv, _ := newTestServerWithACME(t, certissue.NewMemoryChallengeStore())
	srv.Portal = forcedPrincipalForbiddenPortal{API: srv.Portal}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, certSystemRequest(t, http.MethodPost, "/api/system/certificates/renew", strings.NewReader(`{"domain":"example.test"}`)))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403, body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), portal.CodePrincipalForbidden) {
		t.Fatalf("body = %s, want %s", rec.Body.String(), portal.CodePrincipalForbidden)
	}
}

func TestHandlePortalServerCertificateInvalidJSONReturns400(t *testing.T) {
	srv, _ := newTestServerWithACME(t, certissue.NewMemoryChallengeStore())
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, certUserRequest(t, http.MethodPut, "/api/portal/servers/any-id/certificate", strings.NewReader(`[]`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "request.invalid_json") {
		t.Fatalf("body = %s, want request.invalid_json", rec.Body.String())
	}
}

func TestHandlePortalServerHTTPSSwitchOverrideInvalidJSONReturns400(t *testing.T) {
	srv, _ := newTestServerWithACME(t, certissue.NewMemoryChallengeStore())
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, certUserRequest(t, http.MethodPut, "/api/portal/servers/any-id/https-switch", strings.NewReader(`[]`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "request.invalid_json") {
		t.Fatalf("body = %s, want request.invalid_json", rec.Body.String())
	}
}

func TestHandleSystemCertificateReissueAllPrincipalForbiddenMapsTo403(t *testing.T) {
	srv, _ := newTestServerWithACME(t, certissue.NewMemoryChallengeStore())
	srv.Portal = forcedPrincipalForbiddenPortal{API: srv.Portal}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, certSystemRequest(t, http.MethodPost, "/api/system/certificates/reissue-all", nil))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403, body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), portal.CodePrincipalForbidden) {
		t.Fatalf("body = %s, want %s", rec.Body.String(), portal.CodePrincipalForbidden)
	}
}

func TestHandleSystemCertificateCARotatePrincipalForbiddenMapsTo403(t *testing.T) {
	srv, _ := newTestServerWithACME(t, certissue.NewMemoryChallengeStore())
	srv.Portal = forcedPrincipalForbiddenPortal{API: srv.Portal}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, certSystemRequest(t, http.MethodPost, "/api/system/certificates/ca/rotate", nil))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403, body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), portal.CodePrincipalForbidden) {
		t.Fatalf("body = %s, want %s", rec.Body.String(), portal.CodePrincipalForbidden)
	}
}
