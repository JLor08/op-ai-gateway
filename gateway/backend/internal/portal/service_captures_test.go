// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/capture"
	"op-ai-gateway/internal/store"
	"testing"
	"time"
)

// 64 hex chars = 32 bytes = AES-256 key.
const captureTestKey = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

type fakeCaptureReader struct {
	row          store.CaptureRow
	err          error
	deleteErr    error
	setSecretErr error
	setSecret    *bool // sink: records the value passed to SetCaptureSecret
}

func (f fakeCaptureReader) Capture(ctx context.Context, usageEventID string) (store.CaptureRow, error) {
	return f.row, f.err
}

func (f fakeCaptureReader) HasCaptures(ctx context.Context, ids []string) (map[string]store.CapturePresence, error) {
	return map[string]store.CapturePresence{}, nil
}

func (f fakeCaptureReader) DeleteCapture(ctx context.Context, usageEventID string) error {
	return f.deleteErr
}

func (f fakeCaptureReader) SetCaptureSecret(ctx context.Context, usageEventID string, secret bool) error {
	if f.setSecret != nil {
		*f.setSecret = secret
	}
	return f.setSecretErr
}

func sealCaptureEnvelope(t *testing.T, c *capture.Cipher, env map[string]any) []byte {
	t.Helper()
	raw, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if _, err := gz.Write(raw); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return c.Seal(buf.Bytes())
}

func newCaptureFixture(t *testing.T, owner string) (*capture.Cipher, store.CaptureRow) {
	t.Helper()
	cipher, err := capture.New(captureTestKey)
	if err != nil {
		t.Fatalf("capture.New: %v", err)
	}
	// Envelope is exactly 5 fields; api_flavor/http_status ride on the CaptureRow.
	blob := sealCaptureEnvelope(t, cipher, map[string]any{
		"req_headers":  map[string][]string{"Content-Type": {"application/json"}},
		"req_body":     `{"model":"m"}`,
		"resp_headers": map[string][]string{"Content-Type": {"application/json"}},
		"resp_body":    `{"choices":[]}`,
		"truncated":    false,
	})
	row := store.CaptureRow{
		OwnerUserID: owner,
		APIFlavor:   "openai_chat_completions",
		HTTPStatus:  200,
		KeyVersion:  1,
		Blob:        blob,
		CreatedAt:   time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC),
	}
	return cipher, row
}

func TestServiceCaptureDetailOwnerSeesOwn(t *testing.T) {
	cipher, row := newCaptureFixture(t, "usr_owner")
	svc := NewService(ServiceDeps{Captures: fakeCaptureReader{row: row}, Cipher: cipher})
	got, err := svc.CaptureDetail(auth.Token{UserID: "usr_owner", Scopes: []string{"gateway:use"}}, "req_1")
	if err != nil {
		t.Fatalf("owner CaptureDetail err = %v", err)
	}
	if got.ID != "req_1" || got.HTTPStatus != 200 || got.APIFlavor != "openai_chat_completions" {
		t.Fatalf("detail meta = %#v", got)
	}
	if got.ReqBody != `{"model":"m"}` || got.RespBody != `{"choices":[]}` || got.Truncated {
		t.Fatalf("detail body = %#v", got)
	}
	if got.CreatedAt != "2026-07-10T12:00:00Z" {
		t.Fatalf("created_at = %q", got.CreatedAt)
	}
}

func TestServiceCaptureDetailAdminSeesForeign(t *testing.T) {
	cipher, row := newCaptureFixture(t, "usr_owner")
	svc := NewService(ServiceDeps{Captures: fakeCaptureReader{row: row}, Cipher: cipher})
	// A plain admin (admin scope, different user) may read any capture.
	got, err := svc.CaptureDetail(auth.Token{UserID: "usr_admin", Scopes: []string{"gateway:use", "admin"}}, "req_1")
	if err != nil {
		t.Fatalf("admin CaptureDetail err = %v", err)
	}
	if got.APIFlavor != "openai_chat_completions" {
		t.Fatalf("admin detail = %#v", got)
	}
}

func TestServiceCaptureDetailNonOwnerNonAdmin404(t *testing.T) {
	cipher, row := newCaptureFixture(t, "usr_owner")
	svc := NewService(ServiceDeps{Captures: fakeCaptureReader{row: row}, Cipher: cipher})
	_, err := svc.CaptureDetail(auth.Token{UserID: "usr_other", Scopes: []string{"gateway:use"}}, "req_1")
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("non-owner non-admin err = %v, want ErrNotFound", err)
	}
}

func TestServiceCaptureDetailPlainUser404(t *testing.T) {
	cipher, row := newCaptureFixture(t, "usr_owner")
	svc := NewService(ServiceDeps{Captures: fakeCaptureReader{row: row}, Cipher: cipher})
	// gateway:use only, not the owner -> 404 (no existence leak).
	_, err := svc.CaptureDetail(auth.Token{UserID: "usr_plain", Scopes: []string{"gateway:use"}}, "req_1")
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("plain user err = %v, want ErrNotFound", err)
	}
}

func TestServiceCaptureDetailMissing404(t *testing.T) {
	cipher, _ := newCaptureFixture(t, "usr_owner")
	svc := NewService(ServiceDeps{Captures: fakeCaptureReader{err: store.ErrNotFound}, Cipher: cipher})
	_, err := svc.CaptureDetail(auth.Token{UserID: "usr_owner", Scopes: []string{"gateway:use", "admin"}}, "missing")
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing err = %v, want ErrNotFound", err)
	}
}

// gzipOnlyEnvelope gzips (no seal) the same 5-field envelope shape used by
// sealCaptureEnvelope. It is the RAM-fallback wire format (KeyVersion 0).
func gzipOnlyEnvelope(t *testing.T, env map[string]any) []byte {
	t.Helper()
	raw, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if _, err := gz.Write(raw); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}

func TestServiceCaptureDetailKeyVersionZeroNoCipherOwnerSees(t *testing.T) {
	// RAM-fallback capture (no cipher was ever configured): KeyVersion 0 means
	// gunzip-only, and the owner must still see their own capture.
	blob := gzipOnlyEnvelope(t, map[string]any{
		"req_headers":  map[string][]string{"Content-Type": {"application/json"}},
		"req_body":     `{"model":"m"}`,
		"resp_headers": map[string][]string{"Content-Type": {"application/json"}},
		"resp_body":    `{"choices":[]}`,
		"truncated":    false,
	})
	row := store.CaptureRow{
		OwnerUserID: "usr_owner",
		APIFlavor:   "openai_chat_completions",
		HTTPStatus:  200,
		KeyVersion:  0,
		Blob:        blob,
		CreatedAt:   time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC),
	}
	svc := NewService(ServiceDeps{Captures: fakeCaptureReader{row: row}})
	got, err := svc.CaptureDetail(auth.Token{UserID: "usr_owner", Scopes: []string{"gateway:use"}}, "req_1")
	if err != nil {
		t.Fatalf("owner CaptureDetail err = %v", err)
	}
	if got.ReqBody != `{"model":"m"}` || got.RespBody != `{"choices":[]}` {
		t.Fatalf("detail body = %#v", got)
	}
}

func TestServiceCaptureDetailKeyVersionPositiveNoCipher500(t *testing.T) {
	// A sealed capture (KeyVersion > 0) with no cipher configured is a
	// misconfiguration, not "no capture" -> a distinct error, never ErrNotFound
	// (must not look like a missing/absent capture).
	_, row := newCaptureFixture(t, "usr_owner")
	svc := NewService(ServiceDeps{Captures: fakeCaptureReader{row: row}})
	_, err := svc.CaptureDetail(auth.Token{UserID: "usr_owner", Scopes: []string{"gateway:use"}}, "req_1")
	if !errors.Is(err, ErrCaptureCipherMissing) {
		t.Fatalf("keyversion>0 no-cipher err = %v, want ErrCaptureCipherMissing", err)
	}
}

func TestServiceCaptureDetailSecretOwnerSeesWithToggle(t *testing.T) {
	cipher, row := newCaptureFixture(t, "usr_owner")
	row.Secret = true
	svc := NewService(ServiceDeps{Captures: fakeCaptureReader{row: row}, Cipher: cipher})
	got, err := svc.CaptureDetail(auth.Token{UserID: "usr_owner", Scopes: []string{"gateway:use"}}, "req_1")
	if err != nil {
		t.Fatalf("owner CaptureDetail err = %v", err)
	}
	if !got.Secret {
		t.Fatalf("detail Secret = false, want true")
	}
	if !got.CanToggleSecret {
		t.Fatalf("owner CanToggleSecret = false, want true")
	}
}

func TestServiceCaptureDetailSecretForeignAdmin404(t *testing.T) {
	cipher, row := newCaptureFixture(t, "usr_owner")
	row.Secret = true
	svc := NewService(ServiceDeps{Captures: fakeCaptureReader{row: row}, Cipher: cipher})
	// A secret capture is owner-only: even an admin gets 404 (no existence leak).
	_, err := svc.CaptureDetail(auth.Token{UserID: "usr_admin", Scopes: []string{"gateway:use", "admin"}}, "req_1")
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("admin on secret err = %v, want ErrNotFound (owner-only)", err)
	}
}

func TestServiceCaptureDetailNonSecretForeignAdminCannotToggle(t *testing.T) {
	cipher, row := newCaptureFixture(t, "usr_owner") // Secret defaults false
	svc := NewService(ServiceDeps{Captures: fakeCaptureReader{row: row}, Cipher: cipher})
	got, err := svc.CaptureDetail(auth.Token{UserID: "usr_admin", Scopes: []string{"gateway:use", "admin"}}, "req_1")
	if err != nil {
		t.Fatalf("admin on non-secret err = %v", err)
	}
	if got.Secret {
		t.Fatalf("detail Secret = true, want false")
	}
	if got.CanToggleSecret {
		t.Fatalf("foreign admin CanToggleSecret = true, want false (owner-only toggle)")
	}
}

func TestServiceDeleteCaptureOwnerDeletesOwnSecret(t *testing.T) {
	_, row := newCaptureFixture(t, "usr_owner")
	row.Secret = true
	svc := NewService(ServiceDeps{Captures: fakeCaptureReader{row: row}})
	if err := svc.DeleteCapture(auth.Token{UserID: "usr_owner", Scopes: []string{"gateway:use"}}, "req_1"); err != nil {
		t.Fatalf("owner delete own secret err = %v", err)
	}
}

func TestServiceDeleteCaptureForeignAdminCannotDeleteSecret(t *testing.T) {
	_, row := newCaptureFixture(t, "usr_owner")
	row.Secret = true
	// deleteErr is a sentinel that must NEVER surface: the secret gate must
	// reject the admin before the store's DeleteCapture is ever called.
	svc := NewService(ServiceDeps{Captures: fakeCaptureReader{row: row, deleteErr: errors.New("must not be called")}})
	err := svc.DeleteCapture(auth.Token{UserID: "usr_admin", Scopes: []string{"gateway:use", "admin"}}, "req_1")
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("admin delete of foreign secret err = %v, want ErrNotFound", err)
	}
}

func TestServiceSetCaptureSecretOwnerFlips(t *testing.T) {
	_, row := newCaptureFixture(t, "usr_owner")
	var set bool
	svc := NewService(ServiceDeps{Captures: fakeCaptureReader{row: row, setSecret: &set}})
	if err := svc.SetCaptureSecret(auth.Token{UserID: "usr_owner", Scopes: []string{"gateway:use"}}, "req_1", true); err != nil {
		t.Fatalf("owner SetCaptureSecret err = %v", err)
	}
	if !set {
		t.Fatalf("store SetCaptureSecret not called with secret=true")
	}
}

func TestServiceSetCaptureSecretForeignAdmin404(t *testing.T) {
	_, row := newCaptureFixture(t, "usr_owner")
	// Owner-only: even an admin cannot toggle another's capture. The sentinel
	// error must never surface (gate rejects before the store is called).
	svc := NewService(ServiceDeps{Captures: fakeCaptureReader{row: row, setSecretErr: errors.New("must not be called")}})
	err := svc.SetCaptureSecret(auth.Token{UserID: "usr_admin", Scopes: []string{"gateway:use", "admin"}}, "req_1", true)
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("non-owner admin SetCaptureSecret err = %v, want ErrNotFound (owner-only)", err)
	}
}

func TestServiceSetCaptureSecretMissing404(t *testing.T) {
	svc := NewService(ServiceDeps{Captures: fakeCaptureReader{err: store.ErrNotFound}})
	err := svc.SetCaptureSecret(auth.Token{UserID: "usr_owner", Scopes: []string{"gateway:use"}}, "missing", true)
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing SetCaptureSecret err = %v, want ErrNotFound", err)
	}
}

func TestServiceSetCaptureSecretDisabledWhenStoreNil(t *testing.T) {
	svc := NewService(ServiceDeps{})
	if err := svc.SetCaptureSecret(auth.Token{UserID: "usr_owner"}, "req_1", true); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("nil-store SetCaptureSecret err = %v, want ErrNotFound", err)
	}
}

func TestServiceDeleteCaptureOwnerDeletesOwn(t *testing.T) {
	_, row := newCaptureFixture(t, "usr_owner")
	svc := NewService(ServiceDeps{Captures: fakeCaptureReader{row: row}})
	if err := svc.DeleteCapture(auth.Token{UserID: "usr_owner", Scopes: []string{"gateway:use"}}, "req_1"); err != nil {
		t.Fatalf("owner DeleteCapture err = %v", err)
	}
}

func TestServiceDeleteCaptureAdminDeletesForeign(t *testing.T) {
	_, row := newCaptureFixture(t, "usr_owner")
	svc := NewService(ServiceDeps{Captures: fakeCaptureReader{row: row}})
	// A plain admin (admin scope, different user) may delete any capture.
	if err := svc.DeleteCapture(auth.Token{UserID: "usr_admin", Scopes: []string{"gateway:use", "admin"}}, "req_1"); err != nil {
		t.Fatalf("admin DeleteCapture err = %v", err)
	}
}

func TestServiceDeleteCaptureNonOwnerNonAdmin404(t *testing.T) {
	_, row := newCaptureFixture(t, "usr_owner")
	// deleteErr is a sentinel that must NEVER surface: a non-owner/non-admin
	// principal must be rejected before the store's DeleteCapture is ever called.
	svc := NewService(ServiceDeps{Captures: fakeCaptureReader{row: row, deleteErr: errors.New("must not be called")}})
	err := svc.DeleteCapture(auth.Token{UserID: "usr_other", Scopes: []string{"gateway:use"}}, "req_1")
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("non-owner non-admin err = %v, want ErrNotFound", err)
	}
}

func TestServiceDeleteCaptureMissing404(t *testing.T) {
	svc := NewService(ServiceDeps{Captures: fakeCaptureReader{err: store.ErrNotFound}})
	err := svc.DeleteCapture(auth.Token{UserID: "usr_owner", Scopes: []string{"gateway:use", "admin"}}, "missing")
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing err = %v, want ErrNotFound", err)
	}
}

func TestServiceDeleteCaptureDisabledWhenStoreNil(t *testing.T) {
	svc := NewService(ServiceDeps{})
	err := svc.DeleteCapture(auth.Token{UserID: "usr_owner", Scopes: []string{"gateway:use", "admin"}}, "req_1")
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("nil-store err = %v, want ErrNotFound", err)
	}
}

func TestServiceDeleteCapturePropagatesStoreError(t *testing.T) {
	_, row := newCaptureFixture(t, "usr_owner")
	boom := errors.New("disk full")
	svc := NewService(ServiceDeps{Captures: fakeCaptureReader{row: row, deleteErr: boom}})
	err := svc.DeleteCapture(auth.Token{UserID: "usr_owner", Scopes: []string{"gateway:use"}}, "req_1")
	if !errors.Is(err, boom) {
		t.Fatalf("owner delete err = %v, want %v propagated", err, boom)
	}
}
