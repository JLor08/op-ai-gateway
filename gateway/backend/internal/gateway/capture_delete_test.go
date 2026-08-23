// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/capture"
	"op-ai-gateway/internal/store"
	"testing"
)

func TestPortalUsageCaptureDeleteOK(t *testing.T) {
	cipher, err := capture.New(captureDetailKey)
	if err != nil {
		t.Fatalf("capture.New: %v", err)
	}
	srv := newCaptureDetailServer(t, stubCaptureReader{row: store.CaptureRow{OwnerUserID: "usr_dev"}}, cipher)

	req := httptest.NewRequest(http.MethodDelete, "/api/portal/usage/captures/req_1", nil)
	req.Header.Set("Authorization", "Bearer dev-secret")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body map[string]bool
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !body["ok"] {
		t.Fatalf("body = %v, want ok:true", body)
	}
}

func TestPortalUsageCaptureDeleteMissingReturns404(t *testing.T) {
	cipher, err := capture.New(captureDetailKey)
	if err != nil {
		t.Fatalf("capture.New: %v", err)
	}
	srv := newCaptureDetailServer(t, stubCaptureReader{err: store.ErrNotFound}, cipher)

	req := httptest.NewRequest(http.MethodDelete, "/api/portal/usage/captures/nope", nil)
	req.Header.Set("Authorization", "Bearer dev-secret")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body = %s", rec.Code, rec.Body.String())
	}
}

func TestPortalUsageCaptureDeleteStoreErrorReturns500(t *testing.T) {
	cipher, err := capture.New(captureDetailKey)
	if err != nil {
		t.Fatalf("capture.New: %v", err)
	}
	srv := newCaptureDetailServer(t, stubCaptureReader{row: store.CaptureRow{OwnerUserID: "usr_dev"}, deleteErr: errors.New("disk full")}, cipher)

	req := httptest.NewRequest(http.MethodDelete, "/api/portal/usage/captures/req_1", nil)
	req.Header.Set("Authorization", "Bearer dev-secret")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500, body = %s", rec.Code, rec.Body.String())
	}
}

func TestPortalUsageCaptureUnsupportedMethodReturns405(t *testing.T) {
	cipher, err := capture.New(captureDetailKey)
	if err != nil {
		t.Fatalf("capture.New: %v", err)
	}
	srv := newCaptureDetailServer(t, stubCaptureReader{row: store.CaptureRow{OwnerUserID: "usr_dev"}}, cipher)

	// POST is none of GET/PATCH/DELETE -> the handler's default (method-not-allowed) arm.
	req := httptest.NewRequest(http.MethodPost, "/api/portal/usage/captures/req_1", nil)
	req.Header.Set("Authorization", "Bearer dev-secret")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405, body = %s", rec.Code, rec.Body.String())
	}
	wantAllow := http.MethodGet + ", " + http.MethodPatch + ", " + http.MethodDelete
	if allow := rec.Header().Get("Allow"); allow != wantAllow {
		t.Fatalf("Allow header = %q, want %q", allow, wantAllow)
	}
}
