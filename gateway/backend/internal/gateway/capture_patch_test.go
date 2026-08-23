// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/capture"
	"op-ai-gateway/internal/store"
	"strings"
	"testing"
)

func TestPortalUsageCapturePatchSetsSecret(t *testing.T) {
	cipher, err := capture.New(captureDetailKey)
	if err != nil {
		t.Fatalf("capture.New: %v", err)
	}
	var set bool
	// The seeded dev principal (usr_dev) owns the row, so the owner-only toggle passes.
	srv := newCaptureDetailServer(t, stubCaptureReader{row: store.CaptureRow{OwnerUserID: "usr_dev"}, setSecret: &set}, cipher)

	req := httptest.NewRequest(http.MethodPatch, "/api/portal/usage/captures/req_1", strings.NewReader(`{"secret":true}`))
	req.Header.Set("Authorization", "Bearer dev-secret")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !set {
		t.Fatalf("SetCaptureSecret not called with secret=true")
	}
}

func TestPortalUsageCapturePatchNonOwner404(t *testing.T) {
	cipher, err := capture.New(captureDetailKey)
	if err != nil {
		t.Fatalf("capture.New: %v", err)
	}
	// Row owned by someone else -> owner-only -> 404 (no existence leak), even for an admin.
	srv := newCaptureDetailServer(t, stubCaptureReader{row: store.CaptureRow{OwnerUserID: "usr_other"}}, cipher)

	req := httptest.NewRequest(http.MethodPatch, "/api/portal/usage/captures/req_1", strings.NewReader(`{"secret":true}`))
	req.Header.Set("Authorization", "Bearer dev-secret")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body = %s", rec.Code, rec.Body.String())
	}
}
