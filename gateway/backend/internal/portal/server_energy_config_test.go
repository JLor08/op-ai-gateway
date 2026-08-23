// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestCreateUpdateServerEnergyConfig proves the four additive per-server
// energy-config fields (estimated_watts/idle_watts/price_per_kwh/pue — all
// float64, 0 = "unset / use default") ride create + update and round-trip on
// the returned DTO. Purely additive — no engine consumes these yet.
func TestCreateUpdateServerEnergyConfig(t *testing.T) {
	now := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)
	svc, _ := newServerTestService(t, now)

	watts, idle, price, pue := 350.5, 40.25, 0.32, 1.4
	dto, err := svc.CreateServer(context.Background(), adminToken(), CreateServerRequest{
		Name: "S", Domain: "s.example.test",
		EstimatedWatts: &watts, IdleWatts: &idle, PricePerKwh: &price, Pue: &pue,
		AdminGroupIDs: []string{testAdminGroupID},
	})
	if err != nil {
		t.Fatalf("CreateServer: %v", err)
	}
	if dto.EstimatedWatts != 350.5 || dto.IdleWatts != 40.25 || dto.PricePerKwh != 0.32 || dto.Pue != 1.4 {
		t.Fatalf("energy config on create = %+v, want 350.5/40.25/0.32/1.4", dto)
	}

	// A no-touch create (all nil) defaults every field to 0 (unset).
	other, err := svc.CreateServer(context.Background(), adminToken(), CreateServerRequest{
		Name: "S2", Domain: "s2.example.test", AdminGroupIDs: []string{testAdminGroupID},
	})
	if err != nil {
		t.Fatalf("CreateServer(no energy): %v", err)
	}
	if other.EstimatedWatts != 0 || other.IdleWatts != 0 || other.PricePerKwh != 0 || other.Pue != 0 {
		t.Fatalf("energy config defaulted to %+v, want all-0", other)
	}

	newWatts, newIdle, newPrice, newPue := 500.0, 60.0, 0.45, 1.6
	got, err := svc.UpdateServer(context.Background(), systemAdminToken(), dto.ID, UpdateServerRequest{
		EstimatedWatts: &newWatts, IdleWatts: &newIdle, PricePerKwh: &newPrice, Pue: &newPue,
	})
	if err != nil {
		t.Fatalf("UpdateServer: %v", err)
	}
	if got.EstimatedWatts != 500 || got.IdleWatts != 60 || got.PricePerKwh != 0.45 || got.Pue != 1.6 {
		t.Fatalf("energy config after update = %+v, want 500/60/0.45/1.6", got)
	}

	// Resetting to 0 (unset) round-trips too.
	zero := 0.0
	reset, err := svc.UpdateServer(context.Background(), systemAdminToken(), dto.ID, UpdateServerRequest{
		EstimatedWatts: &zero, IdleWatts: &zero, PricePerKwh: &zero, Pue: &zero,
	})
	if err != nil {
		t.Fatalf("UpdateServer(0): %v", err)
	}
	if reset.EstimatedWatts != 0 || reset.IdleWatts != 0 || reset.PricePerKwh != 0 || reset.Pue != 0 {
		t.Fatalf("energy config after reset = %+v, want all-0", reset)
	}
}

// TestCreateServerRejectsNegativeEnergyConfig proves each of the four
// energy-config fields is independently rejected when negative on create.
func TestCreateServerRejectsNegativeEnergyConfig(t *testing.T) {
	now := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)
	svc, _ := newServerTestService(t, now)
	neg := -1.0

	cases := []struct {
		name string
		req  CreateServerRequest
	}{
		{"estimated_watts", CreateServerRequest{Name: "S", Domain: "s.example.test", EstimatedWatts: &neg, AdminGroupIDs: []string{testAdminGroupID}}},
		{"idle_watts", CreateServerRequest{Name: "S", Domain: "s.example.test", IdleWatts: &neg, AdminGroupIDs: []string{testAdminGroupID}}},
		{"price_per_kwh", CreateServerRequest{Name: "S", Domain: "s.example.test", PricePerKwh: &neg, AdminGroupIDs: []string{testAdminGroupID}}},
		{"pue", CreateServerRequest{Name: "S", Domain: "s.example.test", Pue: &neg, AdminGroupIDs: []string{testAdminGroupID}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := svc.CreateServer(context.Background(), adminToken(), tc.req); err == nil {
				t.Fatalf("CreateServer with negative %s should be rejected", tc.name)
			}
		})
	}
}

// TestUpdateServerRejectsNegativeEnergyConfig mirrors the create-path
// rejection on update, and proves a rejected update never persists.
func TestUpdateServerRejectsNegativeEnergyConfig(t *testing.T) {
	now := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)
	svc, _ := newServerTestService(t, now)
	created, err := svc.CreateServer(context.Background(), adminToken(), CreateServerRequest{
		Name: "S", Domain: "s.example.test", AdminGroupIDs: []string{testAdminGroupID},
	})
	if err != nil {
		t.Fatalf("CreateServer: %v", err)
	}

	neg := -5.0
	if _, err := svc.UpdateServer(context.Background(), systemAdminToken(), created.ID, UpdateServerRequest{EstimatedWatts: &neg}); err == nil {
		t.Fatal("UpdateServer with negative estimated_watts should be rejected")
	}
	if _, err := svc.UpdateServer(context.Background(), systemAdminToken(), created.ID, UpdateServerRequest{IdleWatts: &neg}); err == nil {
		t.Fatal("UpdateServer with negative idle_watts should be rejected")
	}
	if _, err := svc.UpdateServer(context.Background(), systemAdminToken(), created.ID, UpdateServerRequest{PricePerKwh: &neg}); err == nil {
		t.Fatal("UpdateServer with negative price_per_kwh should be rejected")
	}
	if _, err := svc.UpdateServer(context.Background(), systemAdminToken(), created.ID, UpdateServerRequest{Pue: &neg}); err == nil {
		t.Fatal("UpdateServer with negative pue should be rejected")
	}

	got, err := svc.GetServer(context.Background(), systemAdminToken(), created.ID)
	if err != nil {
		t.Fatalf("GetServer: %v", err)
	}
	if got.EstimatedWatts != 0 || got.IdleWatts != 0 || got.PricePerKwh != 0 || got.Pue != 0 {
		t.Fatalf("a rejected update must not persist, got %+v", got)
	}
}

// TestSetServerEnergyConfig covers the dedicated energy-save endpoint's service
// method: a full-replace of the five columns that persists, rejects any negative
// numeric value (ErrServerEnergyConfigInvalid, not persisted), normalizes an
// unknown price_unit to the default, and returns a no-leak ErrServerNotFound for
// an unknown id.
func TestSetServerEnergyConfig(t *testing.T) {
	now := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)
	svc, _ := newServerTestService(t, now)
	created, err := svc.CreateServer(context.Background(), adminToken(), CreateServerRequest{
		Name: "S", Domain: "s.example.test", AdminGroupIDs: []string{testAdminGroupID},
	})
	if err != nil {
		t.Fatalf("CreateServer: %v", err)
	}

	// Happy path: sets all five (and the returned DTO + a fresh GetServer agree).
	dto, err := svc.SetServerEnergyConfig(context.Background(), systemAdminToken(), created.ID, 250, 40, 0.28, 1.35, "usd")
	if err != nil {
		t.Fatalf("SetServerEnergyConfig: %v", err)
	}
	if dto.EstimatedWatts != 250 || dto.IdleWatts != 40 || dto.PricePerKwh != 0.28 || dto.Pue != 1.35 || dto.PriceUnit != "usd" {
		t.Fatalf("returned DTO = %+v, want 250/40/0.28/1.35/usd", dto)
	}
	got, err := svc.GetServer(context.Background(), systemAdminToken(), created.ID)
	if err != nil {
		t.Fatalf("GetServer: %v", err)
	}
	if got.EstimatedWatts != 250 || got.IdleWatts != 40 || got.PricePerKwh != 0.28 || got.Pue != 1.35 || got.PriceUnit != "usd" {
		t.Fatalf("persisted = %+v, want 250/40/0.28/1.35/usd", got)
	}

	// An unknown price_unit normalizes to the default (eur_cent) rather than
	// erroring or being stored verbatim.
	normalized, err := svc.SetServerEnergyConfig(context.Background(), systemAdminToken(), created.ID, 250, 40, 0.28, 1.35, "not_a_unit")
	if err != nil {
		t.Fatalf("SetServerEnergyConfig(unknown unit): %v", err)
	}
	if normalized.PriceUnit != UnitEURCent {
		t.Fatalf("unknown price_unit normalized to %q, want %q", normalized.PriceUnit, UnitEURCent)
	}
	// Restore "usd" for the remaining assertions below.
	if _, err := svc.SetServerEnergyConfig(context.Background(), systemAdminToken(), created.ID, 250, 40, 0.28, 1.35, "usd"); err != nil {
		t.Fatalf("SetServerEnergyConfig(restore usd): %v", err)
	}

	// Each numeric field is independently rejected when negative, and a rejected
	// save must not touch the stored (valid) values.
	negCases := []struct {
		name             string
		est, idle, pr, p float64
	}{
		{"estimated_watts", -1, 40, 0.28, 1.35},
		{"idle_watts", 250, -1, 0.28, 1.35},
		{"price_per_kwh", 250, 40, -1, 1.35},
		{"pue", 250, 40, 0.28, -1},
	}
	for _, tc := range negCases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := svc.SetServerEnergyConfig(context.Background(), systemAdminToken(), created.ID, tc.est, tc.idle, tc.pr, tc.p, "usd"); !errors.Is(err, ErrServerEnergyConfigInvalid) {
				t.Fatalf("negative %s = %v, want ErrServerEnergyConfigInvalid", tc.name, err)
			}
		})
	}
	after, err := svc.GetServer(context.Background(), systemAdminToken(), created.ID)
	if err != nil {
		t.Fatalf("GetServer after rejects: %v", err)
	}
	if after.EstimatedWatts != 250 || after.IdleWatts != 40 || after.PricePerKwh != 0.28 || after.Pue != 1.35 || after.PriceUnit != "usd" {
		t.Fatalf("a rejected save must not persist, got %+v", after)
	}

	// Unknown id → no-leak ErrServerNotFound.
	if _, err := svc.SetServerEnergyConfig(context.Background(), systemAdminToken(), "nope", 1, 1, 1, 1, "usd"); !errors.Is(err, ErrServerNotFound) {
		t.Fatalf("unknown id = %v, want ErrServerNotFound", err)
	}
}
