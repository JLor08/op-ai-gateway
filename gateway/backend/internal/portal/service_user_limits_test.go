// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"errors"
	"op-ai-gateway/internal/store"
	"op-ai-gateway/internal/usage"
	"path/filepath"
	"testing"
	"time"
)

// newUserLimitsTestService wires a Service backed by a real *MemoryDirectory
// (Users) and a real *routing.MemoryStore (Routes) — MemoryStore's
// PrincipalLimits/SetPrincipalLimits are real (in-memory) round-trip
// implementations; only its UsageAggregateSince is a permanent honest zero
// (see limits_test.go's fakeAggregateStore doc), which is fine for the
// gate/validation/round-trip tests here — the usage-reflects-real-aggregate
// proof uses a real *store.SQLStore instead (below).
func newUserLimitsTestService(t *testing.T, now time.Time) (*Service, string) {
	t.Helper()
	svc, _, _, _ := newServiceAccountsTestService(t, now)
	return svc, "usr_full"
}

func TestUserLimitsUnknownUserNotFound(t *testing.T) {
	now := time.Now().UTC()
	svc, _ := newUserLimitsTestService(t, now)

	if _, err := svc.UserLimits(context.Background(), "no-such-user"); !errors.Is(err, ErrLimitUserNotFound) {
		t.Fatalf("UserLimits(unknown) err = %v, want ErrLimitUserNotFound", err)
	}
	if _, err := svc.SetUserLimits(context.Background(), adminToken(), "no-such-user", LimitConfigDTO{}); !errors.Is(err, ErrLimitUserNotFound) {
		t.Fatalf("SetUserLimits(unknown) err = %v, want ErrLimitUserNotFound", err)
	}
}

// TestSetUserLimitsForbidsNonAdmin proves the PT-2 Part 2 internal authz
// guard: a principal without the "admin" scope is rejected with
// ErrPrincipalForbidden and the limits are NOT persisted (UserLimits, read
// with no gate of its own, reads back zero afterward) -- the HTTP-level gate
// (requireWebScope("admin")) is defense-in-depth on TOP of this, not instead
// of it.
func TestSetUserLimitsForbidsNonAdmin(t *testing.T) {
	now := time.Now().UTC()
	svc, userID := newUserLimitsTestService(t, now)

	req := LimitConfigDTO{RequestQuota: 100, RequestQuotaPeriod: "day"}
	if _, err := svc.SetUserLimits(context.Background(), ownerToken(), userID, req); !errors.Is(err, ErrPrincipalForbidden) {
		t.Fatalf("SetUserLimits(non-admin) err = %v, want ErrPrincipalForbidden", err)
	}
	got, err := svc.UserLimits(context.Background(), userID)
	if err != nil {
		t.Fatalf("UserLimits: %v", err)
	}
	if got.Limits != (LimitConfigDTO{}) {
		t.Fatalf("Limits after forbidden SetUserLimits = %+v, want zero (no mutation)", got.Limits)
	}
}

// TestSetUserLimitsAllowsAdmin proves the flip side of the same guard: an
// admin-scoped principal succeeds exactly as before the PT-2 Part 2 guard
// was added.
func TestSetUserLimitsAllowsAdmin(t *testing.T) {
	now := time.Now().UTC()
	svc, userID := newUserLimitsTestService(t, now)

	req := LimitConfigDTO{RequestQuota: 100, RequestQuotaPeriod: "day"}
	set, err := svc.SetUserLimits(context.Background(), adminToken(), userID, req)
	if err != nil {
		t.Fatalf("SetUserLimits(admin): %v", err)
	}
	if set.Limits != req {
		t.Fatalf("SetUserLimits response = %+v, want %+v", set.Limits, req)
	}
}

func TestUserLimitsEmptyIDNotFound(t *testing.T) {
	now := time.Now().UTC()
	svc, _ := newUserLimitsTestService(t, now)

	if _, err := svc.UserLimits(context.Background(), ""); !errors.Is(err, ErrLimitUserNotFound) {
		t.Fatalf("UserLimits(\"\") err = %v, want ErrLimitUserNotFound", err)
	}
}

func TestUserLimitsDefaultsToZeroBeforeAnySet(t *testing.T) {
	now := time.Now().UTC()
	svc, userID := newUserLimitsTestService(t, now)

	got, err := svc.UserLimits(context.Background(), userID)
	if err != nil {
		t.Fatalf("UserLimits: %v", err)
	}
	if got.Limits != (LimitConfigDTO{}) {
		t.Fatalf("Limits = %+v, want zero (no limits set yet)", got.Limits)
	}
	if got.Usage != (LimitUsageDTO{}) {
		t.Fatalf("Usage = %+v, want zero", got.Usage)
	}
}

func TestSetUserLimitsRejectsInvalidConfig(t *testing.T) {
	now := time.Now().UTC()
	svc, userID := newUserLimitsTestService(t, now)

	cases := map[string]LimitConfigDTO{
		"negative rate requests":   {RateRequests: -1, RateWindowSeconds: 10},
		"unknown period":           {RequestQuota: 5, RequestQuotaPeriod: "yearly"},
		"threshold without period": {TokenQuota: 5},
		"period without threshold": {CostBudgetPeriod: "day"},
	}
	for name, req := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := svc.SetUserLimits(context.Background(), adminToken(), userID, req); !errors.Is(err, ErrLimitValidation) {
				t.Fatalf("err = %v, want ErrLimitValidation", err)
			}
		})
	}
}

func TestSetUserLimitsPersistsAndRoundTrips(t *testing.T) {
	now := time.Now().UTC()
	svc, userID := newUserLimitsTestService(t, now)

	req := LimitConfigDTO{
		RateRequests: 5, RateWindowSeconds: 10,
		RequestQuota: 100, RequestQuotaPeriod: "day",
		TokenQuota: 1_000_000, TokenQuotaPeriod: "month",
		CostBudget: 25.5, CostBudgetPeriod: "week",
	}
	set, err := svc.SetUserLimits(context.Background(), adminToken(), userID, req)
	if err != nil {
		t.Fatalf("SetUserLimits: %v", err)
	}
	if set.Limits != req {
		t.Fatalf("SetUserLimits response = %+v, want %+v", set.Limits, req)
	}

	got, err := svc.UserLimits(context.Background(), userID)
	if err != nil {
		t.Fatalf("UserLimits: %v", err)
	}
	if got.Limits != req {
		t.Fatalf("UserLimits after set = %+v, want %+v", got.Limits, req)
	}
}

// TestSetUserLimitsZeroConfigClears proves the Task 4 "zero-config-clears"
// decision: setting a fully-zero config after a non-zero one leaves the
// principal with NO effective limits (UserLimits reads back all-zero) —
// SetUserLimits always persists via SetPrincipalLimits regardless of
// zero-ness, and a zero-value routing.LimitConfig is documented as a full
// no-op, so a stored all-zero row is behaviorally identical to no row at all.
func TestSetUserLimitsZeroConfigClears(t *testing.T) {
	now := time.Now().UTC()
	svc, userID := newUserLimitsTestService(t, now)

	if _, err := svc.SetUserLimits(context.Background(), adminToken(), userID, LimitConfigDTO{
		RequestQuota: 10, RequestQuotaPeriod: "day",
	}); err != nil {
		t.Fatalf("initial SetUserLimits: %v", err)
	}
	cleared, err := svc.SetUserLimits(context.Background(), adminToken(), userID, LimitConfigDTO{})
	if err != nil {
		t.Fatalf("clearing SetUserLimits: %v", err)
	}
	if cleared.Limits != (LimitConfigDTO{}) {
		t.Fatalf("cleared Limits = %+v, want zero", cleared.Limits)
	}
	got, err := svc.UserLimits(context.Background(), userID)
	if err != nil {
		t.Fatalf("UserLimits after clear: %v", err)
	}
	if got.Limits != (LimitConfigDTO{}) {
		t.Fatalf("UserLimits after clear = %+v, want zero", got.Limits)
	}
}

// TestUserLimitsUsageReflectsRealAggregate wires a real *store.SQLStore
// (both Routes and Users) so UsageAggregateSince runs against genuinely
// persisted usage_events — the end-to-end proof that LimitUsageDTO reflects
// the store's real aggregate, not just a mocked one (limits_test.go already
// covers the pure computation with a fake). It also proves the calendar
// PERIOD boundary is honored: an event from over a day ago is excluded from a
// "day" quota's usage.
func TestUserLimitsUsageReflectsRealAggregate(t *testing.T) {
	ctx := context.Background()
	st, err := store.OpenSQLite(filepath.Join(t.TempDir(), "gateway.db"))
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	now := time.Now().UTC()
	userID := "usr_1"
	if err := st.CreateUser(ctx, store.User{ID: userID, Email: "u1@example.test", DisplayName: "U1", Role: "user", Status: store.UserStatusActive, PreferredLanguage: "de", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	// Two IN-period events (this "day") + one OUT-of-period event (>24h ago) —
	// only the two in-period ones must be counted.
	st.Record(usage.Event{ID: "req_in_1", UserID: userID, Status: "success", HTTPStatus: 200, TotalTokens: 100, CreatedAt: now})
	st.Record(usage.Event{ID: "req_in_2", UserID: userID, Status: "success", HTTPStatus: 200, TotalTokens: 250, CreatedAt: now})
	st.Record(usage.Event{ID: "req_out", UserID: userID, Status: "success", HTTPStatus: 200, TotalTokens: 999_999, CreatedAt: now.Add(-30 * time.Hour)})
	if err := st.LastUsageError(); err != nil {
		t.Fatalf("Record: %v", err)
	}

	svc := NewService(ServiceDeps{Users: st, Tokens: st, Usage: st, Routes: st, Clock: func() time.Time { return now }})

	req := LimitConfigDTO{RequestQuota: 100, RequestQuotaPeriod: "day", TokenQuota: 100_000, TokenQuotaPeriod: "day"}
	if _, err := svc.SetUserLimits(ctx, adminToken(), userID, req); err != nil {
		t.Fatalf("SetUserLimits: %v", err)
	}

	got, err := svc.UserLimits(ctx, userID)
	if err != nil {
		t.Fatalf("UserLimits: %v", err)
	}
	if got.Usage.RequestsThisPeriod != 2 {
		t.Fatalf("RequestsThisPeriod = %d, want 2 (the out-of-period event must be excluded)", got.Usage.RequestsThisPeriod)
	}
	if got.Usage.TokensThisPeriod != 350 {
		t.Fatalf("TokensThisPeriod = %d, want 350 (100+250, excluding the out-of-period 999999)", got.Usage.TokensThisPeriod)
	}
}
