// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package store

import (
	"bytes"
	"context"
	"testing"
	"time"
)

// TestCrossStoreCaptureParity proves MemoryCaptureStore and SQLiteStore are
// interchangeable from CaptureReader's point of view: given the same
// store.Capture write DTO for the same recorded usage event, Capture()
// returns an identical CaptureRow (owner/flavor/status/key version/blob) from
// both stores.
func TestCrossStoreCaptureParity(t *testing.T) {
	ctx := context.Background()
	created := time.Date(2026, 7, 15, 9, 30, 0, 0, time.UTC)
	write := Capture{
		UsageEventID: "req_parity",
		OwnerUserID:  "usr_owner",
		APIFlavor:    "openai",
		HTTPStatus:   0,
		KeyVersion:   0,
		Blob:         []byte("gzipped-plaintext"),
		CreatedAt:    created,
	}

	st := openMigratedTestSQLite(t)
	defer st.Close()
	st.Record(testUsageEvent("req_parity", "usr_owner", "tok_1", "success"))
	if err := st.SaveCapture(ctx, write); err != nil {
		t.Fatalf("sqlite SaveCapture: %v", err)
	}
	sqlRow, err := st.Capture(ctx, "req_parity")
	if err != nil {
		t.Fatalf("sqlite Capture: %v", err)
	}

	mem := NewMemoryCaptureStore(0)
	if err := mem.SaveCapture(ctx, write); err != nil {
		t.Fatalf("memory SaveCapture: %v", err)
	}
	memRow, err := mem.Capture(ctx, "req_parity")
	if err != nil {
		t.Fatalf("memory Capture: %v", err)
	}

	if sqlRow.OwnerUserID != memRow.OwnerUserID {
		t.Fatalf("OwnerUserID: sqlite=%q memory=%q", sqlRow.OwnerUserID, memRow.OwnerUserID)
	}
	if sqlRow.APIFlavor != memRow.APIFlavor {
		t.Fatalf("APIFlavor: sqlite=%q memory=%q", sqlRow.APIFlavor, memRow.APIFlavor)
	}
	if sqlRow.HTTPStatus != memRow.HTTPStatus {
		t.Fatalf("HTTPStatus: sqlite=%d memory=%d", sqlRow.HTTPStatus, memRow.HTTPStatus)
	}
	if sqlRow.KeyVersion != memRow.KeyVersion {
		t.Fatalf("KeyVersion: sqlite=%d memory=%d", sqlRow.KeyVersion, memRow.KeyVersion)
	}
	if !bytes.Equal(sqlRow.Blob, memRow.Blob) {
		t.Fatalf("Blob: sqlite=%v memory=%v", sqlRow.Blob, memRow.Blob)
	}
}
