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

// TestServerPerfHistoryAuthorizesAndDecimates: an owner reading a window of
// densely-sampled telemetry gets an owner-gated, ascending, window-bounded slice
// decimated to serverPerfHistoryCap by the store.
func TestServerPerfHistoryAuthorizesAndDecimates(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	svc, routeStore := newServerTestService(t, now)
	server := createTestServer(t, svc, "S", "s.example.test")

	ctx := context.Background()
	const total = 2500
	span := 30 * time.Minute // all samples fall within the last 30 minutes
	start := now.Add(-span)
	for i := 0; i < total; i++ {
		reportedAt := start.Add(time.Duration(int64(i) * int64(span) / int64(total-1)))
		if err := routeStore.InsertTelemetrySample(ctx, routing.TelemetrySample{
			ServerID:   server.ID,
			ReportedAt: reportedAt,
			CPUUtilPct: float64(i % 100),
			GPUs:       []routing.GPUSample{{Index: 0, Name: "RTX 4090", UtilPct: 88, TempC: 71}},
			Net:        []routing.NetSample{{Name: "eth0", RxBytes: 1000, TxBytes: 2000}},
		}); err != nil {
			t.Fatalf("InsertTelemetrySample[%d]: %v", i, err)
		}
	}

	got, err := svc.ServerPerfHistory(ctx, ownerToken(), server.ID, time.Hour)
	if err != nil {
		t.Fatalf("ServerPerfHistory (owner): %v", err)
	}
	// 2500 in-window samples decimated to the cap must yield EXACTLY the cap, not
	// a loose "<= cap" bound: an endpoints-only regression would return 2 and
	// still pass "<= cap". The window endpoints are preserved (first == oldest
	// == start, last == newest == now).
	if len(got) != serverPerfHistoryCap {
		t.Fatalf("len(got) = %d, want exactly serverPerfHistoryCap (%d)", len(got), serverPerfHistoryCap)
	}
	if !got[0].ReportedAt.Equal(start) {
		t.Fatalf("decimated first is not oldest: got %v want %v", got[0].ReportedAt, start)
	}
	if !got[len(got)-1].ReportedAt.Equal(now) {
		t.Fatalf("decimated last is not newest: got %v want %v", got[len(got)-1].ReportedAt, now)
	}
	from := now.Add(-time.Hour)
	for i, sample := range got {
		if sample.ReportedAt.Before(from) || sample.ReportedAt.After(now) {
			t.Fatalf("sample[%d].ReportedAt %v outside [%v, %v]", i, sample.ReportedAt, from, now)
		}
		if i > 0 && !sample.ReportedAt.After(got[i-1].ReportedAt) {
			t.Fatalf("samples not strictly ascending at %d: %v not after %v", i, sample.ReportedAt, got[i-1].ReportedAt)
		}
	}
}

// TestServerPerfHistoryNonOwnerNotFound: a plain gateway:use principal who is
// neither owner nor admin gets ErrServerNotFound (no existence leak).
func TestServerPerfHistoryNonOwnerNotFound(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	svc, _ := newServerTestService(t, now)
	server := createTestServer(t, svc, "S", "s.example.test")

	if _, err := svc.ServerPerfHistory(context.Background(), otherToken(), server.ID, time.Hour); !errors.Is(err, ErrServerNotFound) {
		t.Fatalf("non-owner ServerPerfHistory = %v, want ErrServerNotFound", err)
	}
}

// TestServerPerfHistoryAdminAllowed: a SYSTEM-scope principal is authorized
// even without ownership (Phase B, spec 2026-08-10: the system bypass is
// unconditional; a plain admin with no ownership/group link is not -- see
// TestAuthorizeServerMatrix in service_test.go).
func TestServerPerfHistoryAdminAllowed(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	svc, _ := newServerTestService(t, now)
	server := createTestServer(t, svc, "S", "s.example.test")

	if _, err := svc.ServerPerfHistory(context.Background(), systemAdminToken(), server.ID, time.Hour); err != nil {
		t.Fatalf("system-admin ServerPerfHistory = %v, want nil", err)
	}
}
