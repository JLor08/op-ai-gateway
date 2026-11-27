// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"errors"
	"op-ai-gateway/internal/routing"
	"testing"
	"time"
)

// TestDismissServerGPUVendorNote proves the dismissal lowers the note only,
// keeps the legacy vendor, shows in the server DTO, and is gated like every
// other server write: a principal with no rights on the server gets the
// no-leak ErrServerNotFound and the note stays.
func TestDismissServerGPUVendorNote(t *testing.T) {
	ctx := context.Background()
	svc, routes := newServerTestService(t, time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC))
	server := createTestServer(t, svc, "S", "s.example.test")
	// A legacy empty-vendor row, so the backfill rewrites something and raises
	// the note.
	if err := routes.SetServerGPUBudgets(ctx, server.ID, []routing.ServerGPUBudget{{ServerID: server.ID, GPUIndex: 0, BudgetMB: 1000}}); err != nil {
		t.Fatalf("SetServerGPUBudgets: %v", err)
	}
	if n, err := routes.BackfillServerGPUVendor(ctx, server.ID, "nvidia", true); err != nil || n != 1 {
		t.Fatalf("BackfillServerGPUVendor = %d, %v, want 1 row", n, err)
	}

	dto, err := svc.GetServer(ctx, ownerToken(), server.ID)
	if err != nil {
		t.Fatalf("GetServer: %v", err)
	}
	if dto.GPULegacyVendor != "nvidia" || !dto.GPUVendorNote {
		t.Fatalf("DTO before = vendor %q note %v, want nvidia true", dto.GPULegacyVendor, dto.GPUVendorNote)
	}

	if err := svc.DismissServerGPUVendorNote(ctx, otherToken(), server.ID); !errors.Is(err, ErrServerNotFound) {
		t.Fatalf("foreign dismiss = %v, want ErrServerNotFound", err)
	}
	if err := svc.DismissServerGPUVendorNote(ctx, ownerToken(), "srv_nope"); !errors.Is(err, ErrServerNotFound) {
		t.Fatalf("unknown server dismiss = %v, want ErrServerNotFound", err)
	}
	if got, _ := routes.AIServerByID(ctx, server.ID); !got.GPUVendorNote {
		t.Fatal("a refused dismissal lowered the note")
	}

	if err := svc.DismissServerGPUVendorNote(ctx, ownerToken(), server.ID); err != nil {
		t.Fatalf("DismissServerGPUVendorNote: %v", err)
	}
	dto, err = svc.GetServer(ctx, ownerToken(), server.ID)
	if err != nil {
		t.Fatalf("GetServer after: %v", err)
	}
	if dto.GPULegacyVendor != "nvidia" || dto.GPUVendorNote {
		t.Fatalf("DTO after = vendor %q note %v, want nvidia false", dto.GPULegacyVendor, dto.GPUVendorNote)
	}
}
