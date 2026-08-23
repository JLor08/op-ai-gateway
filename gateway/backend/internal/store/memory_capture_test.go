// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package store

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"
)

func TestMemoryCaptureSaveAndReadRoundTrip(t *testing.T) {
	ctx := context.Background()
	m := NewMemoryCaptureStore(0)
	created := time.Date(2026, 7, 15, 9, 0, 0, 0, time.UTC)

	if err := m.SaveCapture(ctx, Capture{
		UsageEventID: "req_1",
		OwnerUserID:  "usr_owner",
		APIFlavor:    "openai_chat_completions",
		HTTPStatus:   200,
		KeyVersion:   0,
		Blob:         []byte("gzipped-plaintext"),
		CreatedAt:    created,
	}); err != nil {
		t.Fatalf("SaveCapture: %v", err)
	}

	row, err := m.Capture(ctx, "req_1")
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if row.OwnerUserID != "usr_owner" || row.APIFlavor != "openai_chat_completions" || row.HTTPStatus != 200 {
		t.Fatalf("row meta = %+v", row)
	}
	if !bytes.Equal(row.Blob, []byte("gzipped-plaintext")) {
		t.Fatalf("Blob = %v", row.Blob)
	}
	if !row.CreatedAt.Equal(created) {
		t.Fatalf("CreatedAt = %v, want %v", row.CreatedAt, created)
	}
}

func TestMemoryCaptureSecretRoundTrip(t *testing.T) {
	ctx := context.Background()
	m := NewMemoryCaptureStore(0)
	if err := m.SaveCapture(ctx, Capture{UsageEventID: "req_secret", Blob: []byte("x"), Secret: true}); err != nil {
		t.Fatalf("SaveCapture secret: %v", err)
	}
	if err := m.SaveCapture(ctx, Capture{UsageEventID: "req_plain", Blob: []byte("y")}); err != nil {
		t.Fatalf("SaveCapture plain: %v", err)
	}
	secretRow, err := m.Capture(ctx, "req_secret")
	if err != nil {
		t.Fatalf("Capture secret: %v", err)
	}
	if !secretRow.Secret {
		t.Fatalf("secret row Secret=%v, want true", secretRow.Secret)
	}
	plainRow, err := m.Capture(ctx, "req_plain")
	if err != nil {
		t.Fatalf("Capture plain: %v", err)
	}
	if plainRow.Secret {
		t.Fatalf("plain row Secret=%v, want false (default)", plainRow.Secret)
	}
}

func TestMemorySetCaptureSecret(t *testing.T) {
	ctx := context.Background()
	m := NewMemoryCaptureStore(0)
	if err := m.SaveCapture(ctx, Capture{UsageEventID: "req_toggle", Blob: []byte("x")}); err != nil {
		t.Fatalf("SaveCapture: %v", err)
	}
	if err := m.SetCaptureSecret(ctx, "req_toggle", true); err != nil {
		t.Fatalf("SetCaptureSecret true: %v", err)
	}
	row, err := m.Capture(ctx, "req_toggle")
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if !row.Secret {
		t.Fatalf("after SetCaptureSecret(true) Secret=%v, want true", row.Secret)
	}
	if err := m.SetCaptureSecret(ctx, "req_toggle", false); err != nil {
		t.Fatalf("SetCaptureSecret false: %v", err)
	}
	row, _ = m.Capture(ctx, "req_toggle")
	if row.Secret {
		t.Fatalf("after SetCaptureSecret(false) Secret=%v, want false", row.Secret)
	}
}

func TestMemorySetCaptureSecretNotFound(t *testing.T) {
	m := NewMemoryCaptureStore(0)
	if err := m.SetCaptureSecret(context.Background(), "does-not-exist", true); !errors.Is(err, ErrNotFound) {
		t.Fatalf("SetCaptureSecret(unknown) err = %v, want ErrNotFound", err)
	}
}

func TestMemoryCaptureNotFound(t *testing.T) {
	m := NewMemoryCaptureStore(0)
	if _, err := m.Capture(context.Background(), "does-not-exist"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Capture(unknown) err = %v, want ErrNotFound", err)
	}
}

func TestMemoryCaptureSaveConflictOnDuplicateID(t *testing.T) {
	ctx := context.Background()
	m := NewMemoryCaptureStore(0)
	if err := m.SaveCapture(ctx, Capture{UsageEventID: "req_dup", Blob: []byte("aaaa")}); err != nil {
		t.Fatalf("first SaveCapture: %v", err)
	}
	if err := m.SaveCapture(ctx, Capture{UsageEventID: "req_dup", Blob: []byte("bbbbbbbb")}); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate SaveCapture err = %v, want ErrConflict", err)
	}
	// The rejected duplicate must NOT double-count: the budget must still
	// reflect only the first (4-byte) blob, not 4+8=12.
	if m.bytes != 4 {
		t.Fatalf("bytes = %d, want 4 (rejected duplicate must not double-count)", m.bytes)
	}
}

func TestMemoryCaptureEvictsOldestFirstOnOverflow(t *testing.T) {
	ctx := context.Background()
	m := NewMemoryCaptureStore(10)                                                                    // 10-byte budget
	if err := m.SaveCapture(ctx, Capture{UsageEventID: "req_a", Blob: []byte("aaaaa")}); err != nil { // 5 bytes
		t.Fatalf("save a: %v", err)
	}
	if err := m.SaveCapture(ctx, Capture{UsageEventID: "req_b", Blob: []byte("bbbbb")}); err != nil { // +5 = 10, at budget
		t.Fatalf("save b: %v", err)
	}
	if err := m.SaveCapture(ctx, Capture{UsageEventID: "req_c", Blob: []byte("ccccc")}); err != nil { // +5 = 15 -> evict oldest (a)
		t.Fatalf("save c: %v", err)
	}

	if _, err := m.Capture(ctx, "req_a"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("req_a err = %v, want ErrNotFound (evicted, oldest)", err)
	}
	if _, err := m.Capture(ctx, "req_b"); err != nil {
		t.Fatalf("req_b should survive: %v", err)
	}
	if _, err := m.Capture(ctx, "req_c"); err != nil {
		t.Fatalf("req_c should survive: %v", err)
	}
	if m.bytes > m.maxBytes {
		t.Fatalf("bytes = %d, want <= maxBytes %d", m.bytes, m.maxBytes)
	}
}

func TestMemoryCaptureSingleOversizedBlobKeptAsSoleEntry(t *testing.T) {
	ctx := context.Background()
	m := NewMemoryCaptureStore(10) // 10-byte budget
	big := bytes.Repeat([]byte("x"), 100)
	if err := m.SaveCapture(ctx, Capture{UsageEventID: "req_big", Blob: big}); err != nil {
		t.Fatalf("save big: %v", err)
	}
	// Kept as the SOLE entry despite exceeding the whole budget — never evicted
	// to empty.
	row, err := m.Capture(ctx, "req_big")
	if err != nil {
		t.Fatalf("req_big should be kept as the sole entry: %v", err)
	}
	if len(row.Blob) != 100 {
		t.Fatalf("Blob len = %d, want 100 (not truncated)", len(row.Blob))
	}
	if len(m.order) != 1 {
		t.Fatalf("order len = %d, want 1 (sole entry)", len(m.order))
	}
}

func TestMemoryCaptureDeleteAccountsBytesAndOrder(t *testing.T) {
	ctx := context.Background()
	m := NewMemoryCaptureStore(0)
	if err := m.SaveCapture(ctx, Capture{UsageEventID: "req_x", Blob: []byte("12345")}); err != nil {
		t.Fatalf("save: %v", err)
	}
	if err := m.DeleteCapture(ctx, "req_x"); err != nil {
		t.Fatalf("DeleteCapture: %v", err)
	}
	if _, err := m.Capture(ctx, "req_x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("post-delete Capture err = %v, want ErrNotFound", err)
	}
	if m.bytes != 0 {
		t.Fatalf("bytes after delete = %d, want 0", m.bytes)
	}
	if len(m.order) != 0 {
		t.Fatalf("order after delete = %v, want empty", m.order)
	}
}

func TestMemoryCaptureDeleteNotFound(t *testing.T) {
	m := NewMemoryCaptureStore(0)
	if err := m.DeleteCapture(context.Background(), "does-not-exist"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("DeleteCapture(unknown) err = %v, want ErrNotFound", err)
	}
}

func TestMemoryCaptureHasCapturesEmptyIDsReturnsEmptyMap(t *testing.T) {
	m := NewMemoryCaptureStore(0)
	got, err := m.HasCaptures(context.Background(), nil)
	if err != nil {
		t.Fatalf("HasCaptures: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("HasCaptures(empty) = %v, want empty map", got)
	}
}

func TestMemoryCaptureHasCapturesUnknownIDsAreFalse(t *testing.T) {
	ctx := context.Background()
	m := NewMemoryCaptureStore(0)
	if err := m.SaveCapture(ctx, Capture{UsageEventID: "req_known", Blob: []byte("x")}); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := m.HasCaptures(ctx, []string{"req_known", "req_unknown"})
	if err != nil {
		t.Fatalf("HasCaptures: %v", err)
	}
	if _, ok := got["req_known"]; !ok {
		t.Fatalf("HasCaptures[req_known] missing, want present")
	}
	if _, ok := got["req_unknown"]; ok {
		t.Fatalf("HasCaptures[req_unknown] present, want absent")
	}
}

func TestMemoryCaptureHasCapturesReturnsSecretAndOwner(t *testing.T) {
	ctx := context.Background()
	m := NewMemoryCaptureStore(0)
	if err := m.SaveCapture(ctx, Capture{UsageEventID: "req_secret", OwnerUserID: "usr_owner", Blob: []byte("x"), Secret: true}); err != nil {
		t.Fatalf("save secret: %v", err)
	}
	if err := m.SaveCapture(ctx, Capture{UsageEventID: "req_plain", OwnerUserID: "usr_owner", Blob: []byte("y")}); err != nil {
		t.Fatalf("save plain: %v", err)
	}
	got, err := m.HasCaptures(ctx, []string{"req_secret", "req_plain"})
	if err != nil {
		t.Fatalf("HasCaptures: %v", err)
	}
	if p := got["req_secret"]; !p.Secret || p.OwnerUserID != "usr_owner" {
		t.Fatalf("req_secret presence = %#v, want {Secret:true OwnerUserID:usr_owner}", p)
	}
	if p := got["req_plain"]; p.Secret || p.OwnerUserID != "usr_owner" {
		t.Fatalf("req_plain presence = %#v, want {Secret:false OwnerUserID:usr_owner}", p)
	}
}

func TestMemoryCaptureNewDefaultsMaxBytes(t *testing.T) {
	for _, v := range []int{0, -1, -100} {
		m := NewMemoryCaptureStore(v)
		if m.maxBytes != defaultCaptureMemoryMaxBytes {
			t.Fatalf("NewMemoryCaptureStore(%d).maxBytes = %d, want default %d", v, m.maxBytes, defaultCaptureMemoryMaxBytes)
		}
	}
}
