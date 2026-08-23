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

// fakeAggregateStore wraps a *routing.MemoryStore, overriding ONLY
// UsageAggregateSince so tests can assert LimitUsageDTO against a
// deterministic, controlled aggregate. routing.MemoryStore's own
// UsageAggregateSince is an honest, permanent zero (the memory driver holds
// no usage_events at all — see routing.Store's doc comment), which makes it
// unsuitable for testing the DISPLAY of a non-zero aggregate; a real
// *store.SQLStore is used elsewhere (service_user_limits_test.go,
// service_services_test.go) for the end-to-end wiring proof against genuine
// persisted usage_events.
type fakeAggregateStore struct {
	*routing.MemoryStore
	requests int64
	tokens   int64
	cost     float64
	err      error
	calls    []string // one entry per UsageAggregateSince call (its `since` argument)
}

func (f *fakeAggregateStore) UsageAggregateSince(_ context.Context, _, _ string, since time.Time) (int64, int64, float64, error) {
	// The test fixtures below always call with a period-aligned `since`
	// derived from routing.PeriodStart, so recovering the period string isn't
	// possible from `since` alone here — instead every call is counted, and
	// per-period tests instead assert on the RETURNED values / call count.
	f.calls = append(f.calls, since.String())
	if f.err != nil {
		return 0, 0, 0, f.err
	}
	return f.requests, f.tokens, f.cost, nil
}

func newFakeAggregateStore() *fakeAggregateStore {
	return &fakeAggregateStore{MemoryStore: routing.NewMemoryStore()}
}

func TestLimitConfigDTORoundTrip(t *testing.T) {
	cfg := routing.LimitConfig{
		RateRequests: 5, RateWindowSeconds: 10,
		RequestQuota: 100, RequestQuotaPeriod: "day",
		TokenQuota: 1_000_000, TokenQuotaPeriod: "month",
		CostBudget: 12.5, CostBudgetPeriod: "week",
	}
	dto := limitConfigDTO(cfg)
	got, err := validateLimitConfig(dto)
	if err != nil {
		t.Fatalf("validateLimitConfig round-trip: %v", err)
	}
	if got != cfg {
		t.Fatalf("round-trip = %+v, want %+v", got, cfg)
	}
}

func TestValidateLimitConfigAllZeroIsValid(t *testing.T) {
	got, err := validateLimitConfig(LimitConfigDTO{})
	if err != nil {
		t.Fatalf("all-zero config should validate (it is the 'clear all limits' input): %v", err)
	}
	if got != (routing.LimitConfig{}) {
		t.Fatalf("all-zero config = %+v, want zero LimitConfig", got)
	}
}

func TestValidateLimitConfigRejectsNegativeValues(t *testing.T) {
	cases := []LimitConfigDTO{
		{RateRequests: -1, RateWindowSeconds: 10},
		{RateRequests: 5, RateWindowSeconds: -1},
		{RequestQuota: -1, RequestQuotaPeriod: "day"},
		{TokenQuota: -1, TokenQuotaPeriod: "day"},
		{CostBudget: -1, CostBudgetPeriod: "day"},
	}
	for i, dto := range cases {
		if _, err := validateLimitConfig(dto); !errors.Is(err, ErrLimitValidation) {
			t.Fatalf("case %d: err = %v, want ErrLimitValidation", i, err)
		}
	}
}

func TestValidateLimitConfigRejectsUnknownPeriod(t *testing.T) {
	cases := []LimitConfigDTO{
		{RequestQuota: 10, RequestQuotaPeriod: "yearly"},
		{TokenQuota: 10, TokenQuotaPeriod: "fortnight"},
		{CostBudget: 10, CostBudgetPeriod: "Day"}, // wrong case is not on the whitelist
	}
	for i, dto := range cases {
		if _, err := validateLimitConfig(dto); !errors.Is(err, ErrLimitValidation) {
			t.Fatalf("case %d: err = %v, want ErrLimitValidation", i, err)
		}
	}
}

func TestValidateLimitConfigRejectsThresholdPeriodPairMismatch(t *testing.T) {
	cases := map[string]LimitConfigDTO{
		"rate requests without window": {RateRequests: 5},
		"rate window without requests": {RateWindowSeconds: 10},
		"request quota without period": {RequestQuota: 10},
		"request period without quota": {RequestQuotaPeriod: "day"},
		"token quota without period":   {TokenQuota: 10},
		"token period without quota":   {TokenQuotaPeriod: "day"},
		"cost budget without period":   {CostBudget: 10},
		"cost period without budget":   {CostBudgetPeriod: "day"},
	}
	for name, dto := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := validateLimitConfig(dto); !errors.Is(err, ErrLimitValidation) {
				t.Fatalf("err = %v, want ErrLimitValidation", err)
			}
		})
	}
}

func TestLimitUsageSkipsReadForUnsetPeriods(t *testing.T) {
	store := newFakeAggregateStore()
	// No period configured at all: zero reads, zero-value usage.
	usage := limitUsage(context.Background(), store, routing.PrincipalTypeUser, "u1", routing.LimitConfig{}, time.Now())
	if usage != (LimitUsageDTO{}) {
		t.Fatalf("usage = %+v, want zero value", usage)
	}
	if len(store.calls) != 0 {
		t.Fatalf("UsageAggregateSince called %d times, want 0 (no period configured)", len(store.calls))
	}
}

func TestLimitUsageReflectsAggregate(t *testing.T) {
	store := newFakeAggregateStore()
	store.requests, store.tokens, store.cost = 42, 12345, 9.75
	cfg := routing.LimitConfig{
		RequestQuota: 100, RequestQuotaPeriod: "day",
		TokenQuota: 1_000_000, TokenQuotaPeriod: "day",
		CostBudget: 50, CostBudgetPeriod: "day",
	}
	usage := limitUsage(context.Background(), store, routing.PrincipalTypeUser, "u1", cfg, time.Now())
	want := LimitUsageDTO{RequestsThisPeriod: 42, TokensThisPeriod: 12345, CostThisPeriod: 9.75}
	if usage != want {
		t.Fatalf("usage = %+v, want %+v", usage, want)
	}
	// All three limits share the SAME period ("day") -> exactly one aggregate
	// read, not three.
	if len(store.calls) != 1 {
		t.Fatalf("UsageAggregateSince called %d times, want 1 (shared period reused)", len(store.calls))
	}
}

func TestLimitUsageDistinctPeriodsEachRead(t *testing.T) {
	store := newFakeAggregateStore()
	store.requests, store.tokens, store.cost = 1, 2, 3
	cfg := routing.LimitConfig{
		RequestQuota: 10, RequestQuotaPeriod: "day",
		TokenQuota: 10, TokenQuotaPeriod: "month",
	}
	limitUsage(context.Background(), store, routing.PrincipalTypeUser, "u1", cfg, time.Now())
	if len(store.calls) != 2 {
		t.Fatalf("UsageAggregateSince called %d times, want 2 (distinct periods each get their own read)", len(store.calls))
	}
}

func TestLimitUsageFailsOpenOnAggregateError(t *testing.T) {
	store := newFakeAggregateStore()
	store.err = errors.New("db unavailable")
	cfg := routing.LimitConfig{CostBudget: 50, CostBudgetPeriod: "day"}
	usage := limitUsage(context.Background(), store, routing.PrincipalTypeUser, "u1", cfg, time.Now())
	if usage.CostThisPeriod != 0 {
		t.Fatalf("CostThisPeriod = %v, want 0 (fail-open on a store error)", usage.CostThisPeriod)
	}
}

func TestPrincipalLimitsDefaultsToZeroWhenAbsent(t *testing.T) {
	store := routing.NewMemoryStore()
	cfg, err := principalLimits(context.Background(), store, routing.PrincipalTypeUser, "no-such-user")
	if err != nil {
		t.Fatalf("principalLimits: %v", err)
	}
	if cfg != (routing.LimitConfig{}) {
		t.Fatalf("cfg = %+v, want zero LimitConfig for an absent row", cfg)
	}
}
