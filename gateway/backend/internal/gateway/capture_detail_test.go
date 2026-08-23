// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/capture"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/provider"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/store"
	"op-ai-gateway/internal/usage"
	"testing"
	"time"
)

const captureDetailKey = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

type stubCaptureReader struct {
	row          store.CaptureRow
	err          error
	deleteErr    error
	setSecretErr error
	setSecret    *bool // sink: records the value passed to SetCaptureSecret
}

func (s stubCaptureReader) Capture(ctx context.Context, usageEventID string) (store.CaptureRow, error) {
	return s.row, s.err
}

func (s stubCaptureReader) HasCaptures(ctx context.Context, ids []string) (map[string]store.CapturePresence, error) {
	return map[string]store.CapturePresence{}, nil
}

func (s stubCaptureReader) DeleteCapture(ctx context.Context, usageEventID string) error {
	return s.deleteErr
}

func (s stubCaptureReader) SetCaptureSecret(ctx context.Context, usageEventID string, secret bool) error {
	if s.setSecret != nil {
		*s.setSecret = secret
	}
	return s.setSecretErr
}

func sealDetailEnvelope(t *testing.T, c *capture.Cipher) []byte {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"req_headers":  map[string][]string{"Content-Type": {"application/json"}},
		"req_body":     `{"model":"m"}`,
		"resp_headers": map[string][]string{"Content-Type": {"application/json"}},
		"resp_body":    `{"choices":[]}`,
		"truncated":    false,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if _, err := gz.Write(raw); err != nil {
		t.Fatalf("gzip: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return c.Seal(buf.Bytes())
}

func newCaptureDetailServer(t *testing.T, captures portal.CaptureReader, cipher *capture.Cipher) *Server {
	t.Helper()
	tokens := auth.NewTokenStore()
	directory := portal.NewMemoryDirectory(tokens)
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	directory.AddUser(store.User{ID: "usr_dev", Email: "dev@example.test", DisplayName: "Dev User", Role: "admin", Status: store.UserStatusActive, PreferredLanguage: "de", CreatedAt: now, UpdatedAt: now})
	if err := directory.CreatePlainToken(context.Background(), store.TokenRecord{ID: "tok_dev", UserID: "usr_dev", Name: "Dev Token", Status: store.TokenStatusActive, Scopes: `["gateway:use","admin"]`, CreatedAt: now, UpdatedAt: now}, "dev-secret"); err != nil {
		t.Fatalf("seed token: %v", err)
	}
	recorder := usage.NewRecorder()
	routeStore := routing.NewMemoryStore()
	return New(ServerDeps{
		Tokens:   tokens,
		Usage:    recorder,
		Provider: provider.NewMock(),
		Routes:   routeStore,
		Portal: portal.NewService(portal.ServiceDeps{
			Users: directory, Tokens: directory, Usage: recorder, Routes: routeStore,
			ModelLister: provider.NewMock(), Captures: captures, Cipher: cipher,
		}),
	})
}

func TestPortalUsageCaptureReturnsDetail(t *testing.T) {
	cipher, err := capture.New(captureDetailKey)
	if err != nil {
		t.Fatalf("capture.New: %v", err)
	}
	row := store.CaptureRow{OwnerUserID: "usr_dev", APIFlavor: "openai_chat_completions", HTTPStatus: 200, KeyVersion: 1, Blob: sealDetailEnvelope(t, cipher), CreatedAt: time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)}
	srv := newCaptureDetailServer(t, stubCaptureReader{row: row}, cipher)

	req := httptest.NewRequest(http.MethodGet, "/api/portal/usage/captures/req_1", nil)
	req.Header.Set("Authorization", "Bearer dev-secret")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var detail portal.CaptureDetail
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatalf("unmarshal: %v (%s)", err, rec.Body.String())
	}
	if detail.ID != "req_1" || detail.HTTPStatus != 200 || detail.APIFlavor != "openai_chat_completions" || detail.ReqBody != `{"model":"m"}` {
		t.Fatalf("detail = %#v", detail)
	}
}

func TestPortalUsageCaptureMissingReturns404(t *testing.T) {
	cipher, err := capture.New(captureDetailKey)
	if err != nil {
		t.Fatalf("capture.New: %v", err)
	}
	srv := newCaptureDetailServer(t, stubCaptureReader{err: store.ErrNotFound}, cipher)

	req := httptest.NewRequest(http.MethodGet, "/api/portal/usage/captures/nope", nil)
	req.Header.Set("Authorization", "Bearer dev-secret")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

func TestPortalUsageCaptureRequiresAuth(t *testing.T) {
	cipher, err := capture.New(captureDetailKey)
	if err != nil {
		t.Fatalf("capture.New: %v", err)
	}
	srv := newCaptureDetailServer(t, stubCaptureReader{err: store.ErrNotFound}, cipher)

	req := httptest.NewRequest(http.MethodGet, "/api/portal/usage/captures/req_1", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauth status = %d, body = %s", rec.Code, rec.Body.String())
	}
}
