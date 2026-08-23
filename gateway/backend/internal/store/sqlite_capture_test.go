// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package store

import (
	"bytes"
	"context"
	"errors"
	"op-ai-gateway/internal/usage"
	"testing"
	"time"
)

func TestSQLiteSaveCaptureRoundTripResolvesOwner(t *testing.T) {
	ctx := context.Background()
	st := openMigratedTestSQLite(t)
	defer st.Close()

	// The captures FK requires the usage_events row to exist first.
	st.Record(testUsageEvent("req_cap", "usr_owner", "tok_1", "success"))

	created := time.Date(2026, 7, 15, 9, 0, 0, 0, time.UTC)
	blob := []byte{0x00, 0x01, 0x02, 0xff}
	// OwnerUserID/APIFlavor/HTTPStatus are additive write-DTO fields (SP-C+ P4)
	// that SQLiteStore.SaveCapture must ignore: it deliberately writes values
	// here that DISAGREE with the seeded usage_events row, then asserts below
	// that Capture() still resolves the real values via its JOIN, not these.
	if err := st.SaveCapture(ctx, Capture{
		UsageEventID: "req_cap",
		OwnerUserID:  "usr_ignored_by_sqlite",
		APIFlavor:    "ignored_by_sqlite",
		HTTPStatus:   999,
		KeyVersion:   1,
		Blob:         blob,
		CreatedAt:    created,
	}); err != nil {
		t.Fatalf("SaveCapture: %v", err)
	}

	row, err := st.Capture(ctx, "req_cap")
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if row.OwnerUserID != "usr_owner" {
		t.Fatalf("OwnerUserID = %q, want usr_owner (JOIN usage_events, Capture.OwnerUserID must be ignored)", row.OwnerUserID)
	}
	if row.KeyVersion != 1 {
		t.Fatalf("KeyVersion = %d, want 1", row.KeyVersion)
	}
	if !bytes.Equal(row.Blob, blob) {
		t.Fatalf("Blob = %v, want %v", row.Blob, blob)
	}
	if !row.CreatedAt.Equal(created) {
		t.Fatalf("CreatedAt = %v, want %v", row.CreatedAt, created)
	}
}

func TestSQLiteSaveCaptureSecretRoundTrip(t *testing.T) {
	ctx := context.Background()
	st := openMigratedTestSQLite(t)
	defer st.Close()

	st.Record(testUsageEvent("req_secret", "usr_owner", "tok_1", "success"))
	st.Record(testUsageEvent("req_plain", "usr_owner", "tok_1", "success"))
	created := time.Date(2026, 7, 16, 9, 0, 0, 0, time.UTC)
	if err := st.SaveCapture(ctx, Capture{UsageEventID: "req_secret", KeyVersion: 1, Blob: []byte("x"), CreatedAt: created, Secret: true}); err != nil {
		t.Fatalf("SaveCapture secret: %v", err)
	}
	if err := st.SaveCapture(ctx, Capture{UsageEventID: "req_plain", KeyVersion: 1, Blob: []byte("y"), CreatedAt: created}); err != nil {
		t.Fatalf("SaveCapture plain: %v", err)
	}

	secretRow, err := st.Capture(ctx, "req_secret")
	if err != nil {
		t.Fatalf("Capture secret: %v", err)
	}
	if !secretRow.Secret {
		t.Fatalf("secret row Secret=%v, want true", secretRow.Secret)
	}
	plainRow, err := st.Capture(ctx, "req_plain")
	if err != nil {
		t.Fatalf("Capture plain: %v", err)
	}
	if plainRow.Secret {
		t.Fatalf("plain row Secret=%v, want false (default)", plainRow.Secret)
	}
}

func TestSQLiteCaptureNotFound(t *testing.T) {
	ctx := context.Background()
	st := openMigratedTestSQLite(t)
	defer st.Close()
	if _, err := st.Capture(ctx, "does-not-exist"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Capture(unknown) err = %v, want ErrNotFound", err)
	}
}

func TestSQLitePruneCapturesRemovesOnlyOlder(t *testing.T) {
	ctx := context.Background()
	st := openMigratedTestSQLite(t)
	defer st.Close()

	st.Record(testUsageEvent("req_old", "usr_1", "tok_1", "success"))
	st.Record(testUsageEvent("req_new", "usr_1", "tok_1", "success"))

	old := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	recent := time.Date(2026, 7, 15, 0, 0, 0, 0, time.UTC)
	if err := st.SaveCapture(ctx, Capture{UsageEventID: "req_old", KeyVersion: 1, Blob: []byte("a"), CreatedAt: old}); err != nil {
		t.Fatalf("SaveCapture old: %v", err)
	}
	if err := st.SaveCapture(ctx, Capture{UsageEventID: "req_new", KeyVersion: 1, Blob: []byte("b"), CreatedAt: recent}); err != nil {
		t.Fatalf("SaveCapture new: %v", err)
	}

	cutoff := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	n, err := st.PruneCaptures(ctx, cutoff)
	if err != nil {
		t.Fatalf("PruneCaptures: %v", err)
	}
	if n != 1 {
		t.Fatalf("PruneCaptures removed = %d, want 1", n)
	}
	if _, err := st.Capture(ctx, "req_old"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("old capture err = %v, want ErrNotFound (pruned)", err)
	}
	if _, err := st.Capture(ctx, "req_new"); err != nil {
		t.Fatalf("new capture err = %v, want it retained", err)
	}
}

func TestSQLiteHasCapturesEmptyIDsReturnsEmptyMapWithoutLookup(t *testing.T) {
	ctx := context.Background()
	st := openMigratedTestSQLite(t)
	defer st.Close()

	got, err := st.HasCaptures(ctx, nil)
	if err != nil {
		t.Fatalf("HasCaptures(nil): %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("HasCaptures(nil) = %#v, want empty map", got)
	}

	got, err = st.HasCaptures(ctx, []string{})
	if err != nil {
		t.Fatalf("HasCaptures(empty slice): %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("HasCaptures(empty slice) = %#v, want empty map", got)
	}
}

func TestSQLiteHasCapturesMixedKnownAndUnknownIDs(t *testing.T) {
	ctx := context.Background()
	st := openMigratedTestSQLite(t)
	defer st.Close()

	st.Record(testUsageEvent("req_cap", "usr_1", "tok_1", "success"))
	st.Record(testUsageEvent("req_nocap", "usr_1", "tok_1", "success"))
	if err := st.SaveCapture(ctx, Capture{UsageEventID: "req_cap", KeyVersion: 1, Blob: []byte("x"), CreatedAt: time.Date(2026, 7, 15, 10, 0, 0, 0, time.UTC)}); err != nil {
		t.Fatalf("SaveCapture: %v", err)
	}

	got, err := st.HasCaptures(ctx, []string{"req_cap", "req_nocap", "req_missing"})
	if err != nil {
		t.Fatalf("HasCaptures: %v", err)
	}
	if _, ok := got["req_cap"]; !ok {
		t.Fatalf("req_cap missing from presence map, want present")
	}
	if _, ok := got["req_nocap"]; ok {
		t.Fatalf("req_nocap present in presence map, want absent")
	}
	if _, ok := got["req_missing"]; ok {
		t.Fatalf("req_missing present in presence map, want absent")
	}
}

func TestSQLiteHasCapturesReturnsSecretAndOwner(t *testing.T) {
	ctx := context.Background()
	st := openMigratedTestSQLite(t)
	defer st.Close()

	st.Record(testUsageEvent("req_secret", "usr_owner", "tok_1", "success"))
	st.Record(testUsageEvent("req_plain", "usr_owner", "tok_1", "success"))
	created := time.Date(2026, 7, 16, 10, 0, 0, 0, time.UTC)
	if err := st.SaveCapture(ctx, Capture{UsageEventID: "req_secret", KeyVersion: 1, Blob: []byte("x"), CreatedAt: created, Secret: true}); err != nil {
		t.Fatalf("SaveCapture secret: %v", err)
	}
	if err := st.SaveCapture(ctx, Capture{UsageEventID: "req_plain", KeyVersion: 1, Blob: []byte("y"), CreatedAt: created}); err != nil {
		t.Fatalf("SaveCapture plain: %v", err)
	}

	got, err := st.HasCaptures(ctx, []string{"req_secret", "req_plain"})
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

func TestSQLiteDeleteCaptureRemovesRowKeepsUsageEvent(t *testing.T) {
	ctx := context.Background()
	st := openMigratedTestSQLite(t)
	defer st.Close()

	st.Record(testUsageEvent("req_del", "usr_1", "tok_1", "success"))
	if err := st.SaveCapture(ctx, Capture{UsageEventID: "req_del", KeyVersion: 1, Blob: []byte("x"), CreatedAt: time.Date(2026, 7, 15, 9, 0, 0, 0, time.UTC)}); err != nil {
		t.Fatalf("SaveCapture: %v", err)
	}

	if err := st.DeleteCapture(ctx, "req_del"); err != nil {
		t.Fatalf("DeleteCapture: %v", err)
	}
	if _, err := st.Capture(ctx, "req_del"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Capture after delete err = %v, want ErrNotFound", err)
	}
	// DeleteCapture must NOT touch usage_events — captures and usage_events are
	// separate tables linked only by the FK; the metadata row stays.
	page, err := st.Query(usage.Query{UserID: "usr_1"})
	if err != nil {
		t.Fatalf("Query returned err: %v", err)
	}
	found := false
	for _, r := range page.Data {
		if r.ID == "req_del" {
			found = true
		}
	}
	if !found {
		t.Fatal("usage_events row for req_del was removed by DeleteCapture, want it kept")
	}
}

func TestSQLiteSetCaptureSecret(t *testing.T) {
	ctx := context.Background()
	st := openMigratedTestSQLite(t)
	defer st.Close()

	st.Record(testUsageEvent("req_toggle", "usr_1", "tok_1", "success"))
	if err := st.SaveCapture(ctx, Capture{UsageEventID: "req_toggle", KeyVersion: 1, Blob: []byte("x"), CreatedAt: time.Date(2026, 7, 16, 9, 0, 0, 0, time.UTC)}); err != nil {
		t.Fatalf("SaveCapture: %v", err)
	}

	if err := st.SetCaptureSecret(ctx, "req_toggle", true); err != nil {
		t.Fatalf("SetCaptureSecret true: %v", err)
	}
	row, err := st.Capture(ctx, "req_toggle")
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if !row.Secret {
		t.Fatalf("after SetCaptureSecret(true) Secret=%v, want true", row.Secret)
	}

	if err := st.SetCaptureSecret(ctx, "req_toggle", false); err != nil {
		t.Fatalf("SetCaptureSecret false: %v", err)
	}
	row, _ = st.Capture(ctx, "req_toggle")
	if row.Secret {
		t.Fatalf("after SetCaptureSecret(false) Secret=%v, want false", row.Secret)
	}
}

func TestSQLiteSetCaptureSecretNotFound(t *testing.T) {
	ctx := context.Background()
	st := openMigratedTestSQLite(t)
	defer st.Close()
	if err := st.SetCaptureSecret(ctx, "does-not-exist", true); !errors.Is(err, ErrNotFound) {
		t.Fatalf("SetCaptureSecret(unknown) err = %v, want ErrNotFound", err)
	}
}

func TestSQLiteDeleteCaptureNotFound(t *testing.T) {
	ctx := context.Background()
	st := openMigratedTestSQLite(t)
	defer st.Close()
	if err := st.DeleteCapture(ctx, "does-not-exist"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("DeleteCapture(unknown) err = %v, want ErrNotFound", err)
	}
}
