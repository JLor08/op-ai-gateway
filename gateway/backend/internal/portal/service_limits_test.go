// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"errors"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/store"
	"op-ai-gateway/internal/usage"
	"path/filepath"
	"testing"
	"time"
)

// newSQLiteServiceTestService wires a Service backed by a real *store.SQLStore
// (Users/Tokens/Usage/Routes/Groups all the same store) and creates one
// service with svcAdminToken() — used for the end-to-end proof that
// LimitsUsage reflects genuinely persisted usage_events (real
// UsageAggregateSince), which newServiceAccountsTestService's
// routing.MemoryStore cannot exercise (its UsageAggregateSince is a
// permanent honest zero — see limits_test.go). svcAdminToken() carries
// "system" scope, so CreateService's admin-group-linkage requirement (Phase
// C, spec 2026-08-10) only needs a REAL, existing admin-tier group to
// reference — no ownership/membership row is needed (system-scope skips
// validateServiceAdminGroupIDs's manage-check entirely), so the seeded
// group pair below has no owner.
func newSQLiteServiceTestService(t *testing.T) (*Service, *store.SQLStore, string) {
	t.Helper()
	ctx := context.Background()
	st, err := store.OpenSQLite(filepath.Join(t.TempDir(), "gateway.db"))
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	now := time.Now().UTC()
	sysGroupID := "ugrp_sqlitetest_sys"
	adminGroupID := "ugrp_sqlitetest_admin"
	if err := st.CreateUserGroup(ctx, store.UserGroup{ID: sysGroupID, Tier: store.GroupTierSystem, Name: "SQLite Test System", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("seed system group: %v", err)
	}
	if err := st.CreateUserGroup(ctx, store.UserGroup{ID: adminGroupID, Tier: store.GroupTierAdmin, Name: "SQLite Test Admin", ParentGroupID: sysGroupID, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("seed admin group: %v", err)
	}
	svc := NewService(ServiceDeps{Users: st, Tokens: st, Usage: st, Routes: st, Groups: st, Clock: func() time.Time { return now }})
	created, err := svc.CreateService(ctx, svcAdminToken(), CreateServiceRequest{Name: "SQLite Service", AdminGroupIDs: []string{adminGroupID}})
	if err != nil {
		t.Fatalf("CreateService: %v", err)
	}
	return svc, st, created.ID
}

// usageEventForService builds a minimal successful usage.Event attributed to
// serviceID (the field UsageAggregateSince's PrincipalTypeService branch
// matches against).
func usageEventForService(serviceID, id string, at time.Time) usage.Event {
	return usage.Event{ID: id, ServiceID: serviceID, Status: "success", HTTPStatus: 200, CreatedAt: at}
}

// --- Service (§7.1) limits: gate reuse -------------------------------------

// TestUpdateServiceLimitsRequiresSettingsGate proves service limits ride the
// EXISTING *Settings* object-gate (design spec §7.1: "Gate Settings aus
// Phase 1 §6.1 — Admin oder Voll-Delegierter"): a Token-Delegate (has Read/
// Tokens access but NOT CanManageSettings) is refused exactly like any other
// Settings-gated field, an admin and a Full-Delegate both succeed.
func TestUpdateServiceLimitsRequiresSettingsGate(t *testing.T) {
	ctx := context.Background()
	svc, _, _, _ := newServiceAccountsTestService(t, time.Now().UTC())
	created := createTestService(t, ctx, svc)

	limits := LimitConfigDTO{RateRequests: 5, RateWindowSeconds: 10}
	if _, err := svc.UpdateService(ctx, svcTokenDelToken(), created.ID, UpdateServiceRequest{Limits: &limits}); !errors.Is(err, ErrServiceNotFound) {
		t.Fatalf("Token-Delegate UpdateService(Limits) = %v, want ErrServiceNotFound (same 404-no-leak as any Settings field)", err)
	}
	if _, err := svc.UpdateService(ctx, svcStrangerToken(), created.ID, UpdateServiceRequest{Limits: &limits}); !errors.Is(err, ErrServiceNotFound) {
		t.Fatalf("stranger UpdateService(Limits) = %v, want ErrServiceNotFound", err)
	}
	if _, err := svc.UpdateService(ctx, svcFullToken(), created.ID, UpdateServiceRequest{Limits: &limits}); err != nil {
		t.Fatalf("Full-Delegate UpdateService(Limits) = %v, want success", err)
	}
	limits2 := LimitConfigDTO{RequestQuota: 10, RequestQuotaPeriod: "day"}
	if _, err := svc.UpdateService(ctx, svcAdminToken(), created.ID, UpdateServiceRequest{Limits: &limits2}); err != nil {
		t.Fatalf("admin UpdateService(Limits) = %v, want success", err)
	}
}

// --- Validation --------------------------------------------------------

func TestUpdateServiceLimitsRejectsInvalidConfig(t *testing.T) {
	ctx := context.Background()
	svc, _, _, _ := newServiceAccountsTestService(t, time.Now().UTC())
	created := createTestService(t, ctx, svc)

	bad := LimitConfigDTO{CostBudget: -1}
	if _, err := svc.UpdateService(ctx, svcAdminToken(), created.ID, UpdateServiceRequest{Limits: &bad}); !errors.Is(err, ErrLimitValidation) {
		t.Fatalf("err = %v, want ErrLimitValidation", err)
	}
	bad2 := LimitConfigDTO{TokenQuotaPeriod: "day"} // period without a threshold
	if _, err := svc.UpdateService(ctx, svcAdminToken(), created.ID, UpdateServiceRequest{Limits: &bad2}); !errors.Is(err, ErrLimitValidation) {
		t.Fatalf("err = %v, want ErrLimitValidation", err)
	}
}

func TestCreateServiceRejectsInvalidLimits(t *testing.T) {
	ctx := context.Background()
	svc, _, _, _ := newServiceAccountsTestService(t, time.Now().UTC())
	bad := LimitConfigDTO{RateRequests: 5} // no window
	if _, err := svc.CreateService(ctx, svcAdminToken(), CreateServiceRequest{Name: "Bad", Limits: &bad}); !errors.Is(err, ErrLimitValidation) {
		t.Fatalf("CreateService(invalid limits) = %v, want ErrLimitValidation", err)
	}
}

// --- Round trip + display ------------------------------------------------

func TestCreateServiceWithLimitsRoundTrips(t *testing.T) {
	ctx := context.Background()
	svc, _, _, _ := newServiceAccountsTestService(t, time.Now().UTC())
	limits := LimitConfigDTO{
		RateRequests: 5, RateWindowSeconds: 10,
		RequestQuota: 100, RequestQuotaPeriod: "day",
	}
	created, err := svc.CreateService(ctx, svcAdminToken(), CreateServiceRequest{Name: "Limited", Limits: &limits, AdminGroupIDs: []string{testServiceAdminGroupID}})
	if err != nil {
		t.Fatalf("CreateService: %v", err)
	}
	if created.Limits != limits {
		t.Fatalf("created.Limits = %+v, want %+v", created.Limits, limits)
	}

	got, err := svc.GetService(ctx, svcAdminToken(), created.ID)
	if err != nil {
		t.Fatalf("GetService: %v", err)
	}
	if got.Limits != limits {
		t.Fatalf("GetService.Limits = %+v, want %+v", got.Limits, limits)
	}
}

func TestCreateServiceWithoutLimitsLeavesThemZero(t *testing.T) {
	ctx := context.Background()
	svc, _, _, _ := newServiceAccountsTestService(t, time.Now().UTC())
	created, err := svc.CreateService(ctx, svcAdminToken(), CreateServiceRequest{Name: "NoLimits", AdminGroupIDs: []string{testServiceAdminGroupID}})
	if err != nil {
		t.Fatalf("CreateService: %v", err)
	}
	if created.Limits != (LimitConfigDTO{}) {
		t.Fatalf("Limits = %+v, want zero (Limits omitted from the request)", created.Limits)
	}
}

func TestUpdateServiceLimitsOmittedLeavesExistingUnchanged(t *testing.T) {
	ctx := context.Background()
	svc, _, _, _ := newServiceAccountsTestService(t, time.Now().UTC())
	limits := LimitConfigDTO{RequestQuota: 50, RequestQuotaPeriod: "month"}
	created, err := svc.CreateService(ctx, svcAdminToken(), CreateServiceRequest{Name: "Sticky", Limits: &limits, AdminGroupIDs: []string{testServiceAdminGroupID}})
	if err != nil {
		t.Fatalf("CreateService: %v", err)
	}

	// A status-only update (Limits nil) must not touch the stored limits.
	name := "Sticky Renamed"
	updated, err := svc.UpdateService(ctx, svcAdminToken(), created.ID, UpdateServiceRequest{Name: &name})
	if err != nil {
		t.Fatalf("UpdateService: %v", err)
	}
	if updated.Limits != limits {
		t.Fatalf("Limits after an unrelated update = %+v, want unchanged %+v", updated.Limits, limits)
	}
}

// TestUpdateServiceLimitsZeroConfigClears mirrors the User-limits decision
// (Task 4): a fully-zero Limits block clears every previously-configured
// limit.
func TestUpdateServiceLimitsZeroConfigClears(t *testing.T) {
	ctx := context.Background()
	svc, _, _, _ := newServiceAccountsTestService(t, time.Now().UTC())
	limits := LimitConfigDTO{CostBudget: 10, CostBudgetPeriod: "day"}
	created, err := svc.CreateService(ctx, svcAdminToken(), CreateServiceRequest{Name: "Clearable", Limits: &limits, AdminGroupIDs: []string{testServiceAdminGroupID}})
	if err != nil {
		t.Fatalf("CreateService: %v", err)
	}

	zero := LimitConfigDTO{}
	updated, err := svc.UpdateService(ctx, svcAdminToken(), created.ID, UpdateServiceRequest{Limits: &zero})
	if err != nil {
		t.Fatalf("UpdateService(clear): %v", err)
	}
	if updated.Limits != (LimitConfigDTO{}) {
		t.Fatalf("Limits after clearing = %+v, want zero", updated.Limits)
	}
}

// TestDeleteServiceCleansUpLimits proves the design spec §4 best-effort
// cleanup: deleting a service also removes its principal_limits row, so a
// later service that happens to reuse... (n/a, ids are random) — the direct
// assertion is that the routing store no longer has a row for the deleted
// service id.
func TestDeleteServiceCleansUpLimits(t *testing.T) {
	ctx := context.Background()
	svc, _, _, routeStore := newServiceAccountsTestService(t, time.Now().UTC())
	limits := LimitConfigDTO{RateRequests: 5, RateWindowSeconds: 10}
	created, err := svc.CreateService(ctx, svcAdminToken(), CreateServiceRequest{Name: "ToDelete", Limits: &limits, AdminGroupIDs: []string{testServiceAdminGroupID}})
	if err != nil {
		t.Fatalf("CreateService: %v", err)
	}
	if _, ok, err := routeStore.PrincipalLimits(ctx, routing.PrincipalTypeService, created.ID); err != nil || !ok {
		t.Fatalf("PrincipalLimits before delete: ok=%v err=%v, want a stored row", ok, err)
	}

	if err := svc.DeleteService(ctx, svcAdminToken(), created.ID); err != nil {
		t.Fatalf("DeleteService: %v", err)
	}
	if _, ok, err := routeStore.PrincipalLimits(ctx, routing.PrincipalTypeService, created.ID); err != nil || ok {
		t.Fatalf("PrincipalLimits after delete: ok=%v err=%v, want no row (best-effort cleanup)", ok, err)
	}
}

// TestServiceLimitsUsageReflectsAggregate wires a real *store.SQLStore so
// LimitsUsage is checked end-to-end against genuinely persisted usage_events
// attributed via ServiceID (mirrors
// TestUserLimitsUsageReflectsRealAggregate for the User principal).
func TestServiceLimitsUsageReflectsAggregate(t *testing.T) {
	ctx := context.Background()
	svc, st, serviceID := newSQLiteServiceTestService(t)

	limits := LimitConfigDTO{RequestQuota: 100, RequestQuotaPeriod: "day"}
	if _, err := svc.UpdateService(ctx, svcAdminToken(), serviceID, UpdateServiceRequest{Limits: &limits}); err != nil {
		t.Fatalf("UpdateService: %v", err)
	}

	now := time.Now().UTC()
	st.Record(usageEventForService(serviceID, "svc_req_1", now))
	st.Record(usageEventForService(serviceID, "svc_req_2", now))
	if err := st.LastUsageError(); err != nil {
		t.Fatalf("Record: %v", err)
	}

	got, err := svc.GetService(ctx, svcAdminToken(), serviceID)
	if err != nil {
		t.Fatalf("GetService: %v", err)
	}
	if got.LimitsUsage.RequestsThisPeriod != 2 {
		t.Fatalf("RequestsThisPeriod = %d, want 2", got.LimitsUsage.RequestsThisPeriod)
	}
}
