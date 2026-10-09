// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package routing

import (
	"testing"
	"time"
)

func TestEffectiveHealthCheckIntervalSeconds(t *testing.T) {
	const (
		systemDefault = 30
		min           = 5
		max           = 3600
	)
	cases := []struct {
		name string
		app  Application
		want int
	}{
		{
			name: "unset (0) follows the system default",
			app:  Application{HealthCheckIntervalSeconds: 0},
			want: systemDefault,
		},
		{
			name: "custom value within [min,max] is used as-is",
			app:  Application{HealthCheckIntervalSeconds: 45},
			want: 45,
		},
		{
			name: "custom value below min clamps up to min",
			app:  Application{HealthCheckIntervalSeconds: min - 1},
			want: min,
		},
		{
			name: "custom value above max clamps down to max",
			app:  Application{HealthCheckIntervalSeconds: max + 1},
			want: max,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := EffectiveHealthCheckIntervalSeconds(tc.app, systemDefault, min, max); got != tc.want {
				t.Fatalf("EffectiveHealthCheckIntervalSeconds = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestEffectiveAgentPresenceTimeoutSeconds(t *testing.T) {
	const (
		systemDefault = 15
		min           = 3
		max           = 3600
	)
	cases := []struct {
		name   string
		server AIServer
		want   int
	}{
		{
			name:   "unset (0) follows the system default",
			server: AIServer{AgentPresenceTimeoutSeconds: 0},
			want:   systemDefault,
		},
		{
			name:   "custom value within [min,max] is used as-is",
			server: AIServer{AgentPresenceTimeoutSeconds: 7},
			want:   7,
		},
		{
			name:   "custom value below min clamps up to min",
			server: AIServer{AgentPresenceTimeoutSeconds: 1},
			want:   min,
		},
		{
			name:   "custom value above max clamps down to max",
			server: AIServer{AgentPresenceTimeoutSeconds: max + 100},
			want:   max,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := EffectiveAgentPresenceTimeoutSeconds(tc.server, systemDefault, min, max); got != tc.want {
				t.Fatalf("EffectiveAgentPresenceTimeoutSeconds = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestEffectiveHealthCheckMode(t *testing.T) {
	cases := []struct {
		name string
		app  Application
		want string
	}{
		{
			name: "explicit model_sync wins",
			app:  Application{HealthCheckMode: HealthCheckModeModelSync, AlwaysReachable: false},
			want: HealthCheckModeModelSync,
		},
		{
			name: "explicit always_reachable wins",
			app:  Application{HealthCheckMode: HealthCheckModeAlwaysReachable},
			want: HealthCheckModeAlwaysReachable,
		},
		{
			name: "explicit health_path wins even if AlwaysReachable is set",
			app:  Application{HealthCheckMode: HealthCheckModeHealthPath, AlwaysReachable: true},
			want: HealthCheckModeHealthPath,
		},
		{
			name: "legacy row: empty mode + always_reachable derives to always",
			app:  Application{HealthCheckMode: "", AlwaysReachable: true},
			want: HealthCheckModeAlwaysReachable,
		},
		{
			name: "legacy row: empty mode + not always_reachable derives to health_path",
			app:  Application{HealthCheckMode: "", AlwaysReachable: false},
			want: HealthCheckModeHealthPath,
		},
		{
			name: "unknown stored mode falls back to legacy derivation",
			app:  Application{HealthCheckMode: "bogus", AlwaysReachable: true},
			want: HealthCheckModeAlwaysReachable,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := EffectiveHealthCheckMode(tc.app); got != tc.want {
				t.Fatalf("EffectiveHealthCheckMode = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestTelemetrySampleValueRoundTrip(t *testing.T) {
	reported := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	s := TelemetrySample{
		ServerID:       "srv_1",
		ReportedAt:     reported,
		CPUUtilPct:     37.5,
		MemUsedBytes:   8_000_000_000,
		MemTotalBytes:  16_000_000_000,
		SwapUsedBytes:  1_000_000,
		SwapTotalBytes: 2_000_000,
		Load1:          1.5,
		Load5:          1.2,
		Load15:         0.9,
		ActiveRequests: 4,
		QueueDepth:     2,
		GPUs: []GPUSample{
			{
				Index:         0,
				Name:          "RTX 4090",
				UUID:          "gpu-uuid-0",
				UtilPct:       88,
				MemUsedBytes:  12_000_000_000,
				MemTotalBytes: 24_000_000_000,
				TempC:         71,
				VRAMTempC:     80,
				PowerW:        320.5,
				FanPct:        55,
			},
			{
				Index:         1,
				Name:          "RTX 4080",
				UUID:          "gpu-uuid-1",
				UtilPct:       42,
				MemUsedBytes:  6_000_000_000,
				MemTotalBytes: 16_000_000_000,
				TempC:         65,
				VRAMTempC:     74,
				PowerW:        250,
				FanPct:        40,
			},
		},
		Net: []NetSample{
			{Name: "eth0", RxBytes: 1000, TxBytes: 2000},
		},
	}

	if s.CPUUtilPct != 37.5 {
		t.Fatalf("CPUUtilPct = %v, want 37.5", s.CPUUtilPct)
	}
	if s.ActiveRequests != 4 {
		t.Fatalf("ActiveRequests = %d, want 4", s.ActiveRequests)
	}
	if s.GPUs[0].UtilPct != 88 {
		t.Fatalf("GPUs[0].UtilPct = %v, want 88", s.GPUs[0].UtilPct)
	}
	if s.GPUs[1].VRAMTempC != 74 {
		t.Fatalf("GPUs[1].VRAMTempC = %d, want 74", s.GPUs[1].VRAMTempC)
	}
	if s.Net[0].RxBytes != 1000 {
		t.Fatalf("Net[0].RxBytes = %d, want 1000", s.Net[0].RxBytes)
	}
}

func TestAssignProxyListenPort(t *testing.T) {
	const base = 8600

	t.Run("already assigned is returned unchanged (idempotent)", func(t *testing.T) {
		app := Application{ID: "app_1", ProxyListenPort: 8601}
		serverApps := []Application{app, {ID: "app_2", ProxyListenPort: 8600}}
		if got := AssignProxyListenPort(serverApps, app, base); got != 8601 {
			t.Fatalf("AssignProxyListenPort = %d, want 8601 (unchanged)", got)
		}
	})

	t.Run("no apps taken picks base", func(t *testing.T) {
		app := Application{ID: "app_1"}
		if got := AssignProxyListenPort(nil, app, base); got != base {
			t.Fatalf("AssignProxyListenPort = %d, want %d", got, base)
		}
	})

	t.Run("picks the lowest free port >= base, skipping other apps' taken ports", func(t *testing.T) {
		app := Application{ID: "app_new"}
		serverApps := []Application{
			{ID: "app_a", ProxyListenPort: 8600},
			{ID: "app_b", ProxyListenPort: 8601},
			app, // the app being assigned; its own (zero) port never counts as taken
		}
		if got := AssignProxyListenPort(serverApps, app, base); got != 8602 {
			t.Fatalf("AssignProxyListenPort = %d, want 8602", got)
		}
	})

	t.Run("fills a gap below the highest taken port", func(t *testing.T) {
		app := Application{ID: "app_new"}
		serverApps := []Application{
			{ID: "app_a", ProxyListenPort: 8600},
			{ID: "app_b", ProxyListenPort: 8602},
			app,
		}
		if got := AssignProxyListenPort(serverApps, app, base); got != 8601 {
			t.Fatalf("AssignProxyListenPort = %d, want 8601 (fill the gap)", got)
		}
	})

	t.Run("stable across repeated calls once assigned", func(t *testing.T) {
		app := Application{ID: "app_new"}
		serverApps := []Application{{ID: "app_a", ProxyListenPort: 8600}, app}
		first := AssignProxyListenPort(serverApps, app, base)
		app.ProxyListenPort = first
		serverApps[1] = app
		second := AssignProxyListenPort(serverApps, app, base)
		if second != first {
			t.Fatalf("re-run reassigned: first=%d second=%d", first, second)
		}
	})
}

// TestTelemetrySampleStoreInterface is a compile-time assertion that the Store
// interface carries the three telemetry-sample methods. The closure is never
// invoked (a nil Store would panic on method-value evaluation); type-checking
// its body is enough to fail the build until the interface declares them.
func TestTelemetrySampleStoreInterface(t *testing.T) {
	_ = func(s Store) {
		_ = s.InsertTelemetrySample
		_ = s.TelemetrySamples
		_ = s.PruneTelemetrySamples
	}
}

// TestMergeVendorAccountUsage pins the "merge, never blank" rule every usage
// writer (the passive scrape and the active fetch) relies on: the incoming
// snapshot wins for each field it KNOWS (percent >= 0, non-nil reset, non-empty
// credit string) and the stored value survives for each field it does not
// (-1 / nil / ""), so a partial reading can never wipe a good one. AccountID and
// UpdatedAt always come from the incoming snapshot.
func TestMergeVendorAccountUsage(t *testing.T) {
	t0 := time.Date(2026, 10, 8, 10, 0, 0, 0, time.UTC)
	t1 := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	fiveOld := time.Date(2026, 10, 8, 14, 0, 0, 0, time.UTC)
	fiveNew := time.Date(2026, 10, 8, 15, 0, 0, 0, time.UTC)
	weekOld := time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC)
	weekNew := time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)

	// existing is a fully known stored snapshot; unknown is a snapshot that
	// knows nothing (every field at its unknown sentinel) apart from the
	// identity and timestamp every snapshot carries.
	existing := VendorAccountUsage{
		AccountID: "old", FiveHourPct: 40, FiveHourResetAt: &fiveOld,
		WeeklyPct: 25, WeeklyResetAt: &weekOld, CreditBalance: "10.00", UpdatedAt: t0,
	}
	unknown := VendorAccountUsage{AccountID: "acc", FiveHourPct: -1, WeeklyPct: -1, SpendUsedPct: -1, UpdatedAt: t1}

	cases := []struct {
		name     string
		incoming VendorAccountUsage
		want     VendorAccountUsage
	}{
		{
			name:     "nothing known keeps every stored field",
			incoming: unknown,
			want: VendorAccountUsage{
				AccountID: "acc", FiveHourPct: 40, FiveHourResetAt: &fiveOld,
				WeeklyPct: 25, WeeklyResetAt: &weekOld, CreditBalance: "10.00", UpdatedAt: t1,
			},
		},
		{
			name:     "five-hour percent known overrides, the rest is kept",
			incoming: VendorAccountUsage{AccountID: "acc", FiveHourPct: 55, WeeklyPct: -1, UpdatedAt: t1},
			want: VendorAccountUsage{
				AccountID: "acc", FiveHourPct: 55, FiveHourResetAt: &fiveOld,
				WeeklyPct: 25, WeeklyResetAt: &weekOld, CreditBalance: "10.00", UpdatedAt: t1,
			},
		},
		{
			name:     "a real 0 percent is known and overrides (not mistaken for unknown)",
			incoming: VendorAccountUsage{AccountID: "acc", FiveHourPct: 0, WeeklyPct: 0, UpdatedAt: t1},
			want: VendorAccountUsage{
				AccountID: "acc", FiveHourPct: 0, FiveHourResetAt: &fiveOld,
				WeeklyPct: 0, WeeklyResetAt: &weekOld, CreditBalance: "10.00", UpdatedAt: t1,
			},
		},
		{
			name:     "five-hour reset known overrides, the rest is kept",
			incoming: VendorAccountUsage{AccountID: "acc", FiveHourPct: -1, FiveHourResetAt: &fiveNew, WeeklyPct: -1, UpdatedAt: t1},
			want: VendorAccountUsage{
				AccountID: "acc", FiveHourPct: 40, FiveHourResetAt: &fiveNew,
				WeeklyPct: 25, WeeklyResetAt: &weekOld, CreditBalance: "10.00", UpdatedAt: t1,
			},
		},
		{
			name:     "weekly percent known overrides, the rest is kept",
			incoming: VendorAccountUsage{AccountID: "acc", FiveHourPct: -1, WeeklyPct: 60, UpdatedAt: t1},
			want: VendorAccountUsage{
				AccountID: "acc", FiveHourPct: 40, FiveHourResetAt: &fiveOld,
				WeeklyPct: 60, WeeklyResetAt: &weekOld, CreditBalance: "10.00", UpdatedAt: t1,
			},
		},
		{
			name:     "weekly reset known overrides, the rest is kept",
			incoming: VendorAccountUsage{AccountID: "acc", FiveHourPct: -1, WeeklyPct: -1, WeeklyResetAt: &weekNew, UpdatedAt: t1},
			want: VendorAccountUsage{
				AccountID: "acc", FiveHourPct: 40, FiveHourResetAt: &fiveOld,
				WeeklyPct: 25, WeeklyResetAt: &weekNew, CreditBalance: "10.00", UpdatedAt: t1,
			},
		},
		{
			name:     "credit balance known overrides, the rest is kept",
			incoming: VendorAccountUsage{AccountID: "acc", FiveHourPct: -1, WeeklyPct: -1, CreditBalance: "3.25", UpdatedAt: t1},
			want: VendorAccountUsage{
				AccountID: "acc", FiveHourPct: 40, FiveHourResetAt: &fiveOld,
				WeeklyPct: 25, WeeklyResetAt: &weekOld, CreditBalance: "3.25", UpdatedAt: t1,
			},
		},
		{
			name: "everything known overrides every stored field",
			incoming: VendorAccountUsage{
				AccountID: "acc", FiveHourPct: 5, FiveHourResetAt: &fiveNew,
				WeeklyPct: 6, WeeklyResetAt: &weekNew, CreditBalance: "99", UpdatedAt: t1,
			},
			want: VendorAccountUsage{
				AccountID: "acc", FiveHourPct: 5, FiveHourResetAt: &fiveNew,
				WeeklyPct: 6, WeeklyResetAt: &weekNew, CreditBalance: "99", UpdatedAt: t1,
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := MergeVendorAccountUsage(existing, tc.incoming)
			if got.AccountID != tc.want.AccountID || got.FiveHourPct != tc.want.FiveHourPct ||
				got.WeeklyPct != tc.want.WeeklyPct || got.CreditBalance != tc.want.CreditBalance ||
				!got.UpdatedAt.Equal(tc.want.UpdatedAt) {
				t.Fatalf("scalar mismatch:\n got  %+v\n want %+v", got, tc.want)
			}
			if !mergeTimePtrEqual(got.FiveHourResetAt, tc.want.FiveHourResetAt) {
				t.Fatalf("FiveHourResetAt = %v, want %v", got.FiveHourResetAt, tc.want.FiveHourResetAt)
			}
			if !mergeTimePtrEqual(got.WeeklyResetAt, tc.want.WeeklyResetAt) {
				t.Fatalf("WeeklyResetAt = %v, want %v", got.WeeklyResetAt, tc.want.WeeklyResetAt)
			}
		})
	}

	t.Run("an all-unknown existing stays unknown under an all-unknown incoming", func(t *testing.T) {
		// No stored reading and no new one: the result must be the unknown
		// sentinels, never a fabricated 0 / zero time / credit.
		got := MergeVendorAccountUsage(
			VendorAccountUsage{AccountID: "acc", FiveHourPct: -1, WeeklyPct: -1, SpendUsedPct: -1},
			unknown,
		)
		if got.FiveHourPct != -1 || got.WeeklyPct != -1 || got.FiveHourResetAt != nil ||
			got.WeeklyResetAt != nil || got.CreditBalance != "" || got.SpendUsedPct != -1 ||
			got.SpendResetAt != nil || got.SpendUnit != "" || got.SpendLimit != "" ||
			got.SpendUsed != "" || got.SpendRemaining != "" || got.CreditStatus != "" {
			t.Fatalf("merged = %+v, want every field still unknown", got)
		}
	})
}

// TestMergeVendorAccountUsageSpendFields pins the "merge, never blank" rule for the
// Business spend-control fields (#195) the same way TestMergeVendorAccountUsage
// does for the rate-limit windows: an incoming value that is KNOWN (non-empty
// string, percent >= 0, non-nil reset) overwrites the stored one, and an UNKNOWN
// one ("" / -1 / nil) keeps it, field by field -- so a passive header scrape, which
// never carries spend data, cannot blank what the active fetch stored. A real 0
// percent is known.
func TestMergeVendorAccountUsageSpendFields(t *testing.T) {
	t0 := time.Date(2026, 10, 8, 10, 0, 0, 0, time.UTC)
	t1 := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	resetOld := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	resetNew := time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)

	existing := VendorAccountUsage{
		AccountID: "old", FiveHourPct: -1, WeeklyPct: -1, UpdatedAt: t0,
		SpendUnit: "credit", SpendLimit: "6000", SpendUsed: "1500", SpendRemaining: "4500",
		SpendUsedPct: 25, SpendResetAt: &resetOld, CreditStatus: "has_credits",
	}
	// unknownSpend knows nothing about spend control (and nothing about the
	// windows either): every spend field at its unknown sentinel.
	unknownSpend := VendorAccountUsage{AccountID: "acc", FiveHourPct: -1, WeeklyPct: -1, SpendUsedPct: -1, UpdatedAt: t1}

	// with returns unknownSpend after mutate has set the fields the case knows.
	with := func(mutate func(*VendorAccountUsage)) VendorAccountUsage {
		in := unknownSpend
		mutate(&in)
		return in
	}
	// keep is the merge result when nothing about spend is known: every stored
	// spend field survives, identity and timestamp come from the incoming.
	keep := func(mutate func(*VendorAccountUsage)) VendorAccountUsage {
		want := VendorAccountUsage{
			AccountID: "acc", FiveHourPct: -1, WeeklyPct: -1, UpdatedAt: t1,
			SpendUnit: "credit", SpendLimit: "6000", SpendUsed: "1500", SpendRemaining: "4500",
			SpendUsedPct: 25, SpendResetAt: &resetOld, CreditStatus: "has_credits",
		}
		if mutate != nil {
			mutate(&want)
		}
		return want
	}

	cases := []struct {
		name     string
		incoming VendorAccountUsage
		want     VendorAccountUsage
	}{
		{
			name:     "nothing known keeps every stored spend field",
			incoming: unknownSpend,
			want:     keep(nil),
		},
		{
			name:     "spend unit known overrides, the rest is kept",
			incoming: with(func(u *VendorAccountUsage) { u.SpendUnit = "usd" }),
			want:     keep(func(u *VendorAccountUsage) { u.SpendUnit = "usd" }),
		},
		{
			name:     "spend limit known overrides, the rest is kept",
			incoming: with(func(u *VendorAccountUsage) { u.SpendLimit = "9000" }),
			want:     keep(func(u *VendorAccountUsage) { u.SpendLimit = "9000" }),
		},
		{
			name:     "spend used known overrides, the rest is kept",
			incoming: with(func(u *VendorAccountUsage) { u.SpendUsed = "2000" }),
			want:     keep(func(u *VendorAccountUsage) { u.SpendUsed = "2000" }),
		},
		{
			name:     "spend remaining known overrides, the rest is kept",
			incoming: with(func(u *VendorAccountUsage) { u.SpendRemaining = "4000" }),
			want:     keep(func(u *VendorAccountUsage) { u.SpendRemaining = "4000" }),
		},
		{
			name:     "spend used percent known overrides, the rest is kept",
			incoming: with(func(u *VendorAccountUsage) { u.SpendUsedPct = 33.5 }),
			want:     keep(func(u *VendorAccountUsage) { u.SpendUsedPct = 33.5 }),
		},
		{
			name:     "a real 0 spend percent is known and overrides (not mistaken for unknown)",
			incoming: with(func(u *VendorAccountUsage) { u.SpendUsedPct = 0 }),
			want:     keep(func(u *VendorAccountUsage) { u.SpendUsedPct = 0 }),
		},
		{
			name:     "spend reset known overrides, the rest is kept",
			incoming: with(func(u *VendorAccountUsage) { u.SpendResetAt = &resetNew }),
			want:     keep(func(u *VendorAccountUsage) { u.SpendResetAt = &resetNew }),
		},
		{
			name:     "credit status known overrides, the rest is kept",
			incoming: with(func(u *VendorAccountUsage) { u.CreditStatus = "none" }),
			want:     keep(func(u *VendorAccountUsage) { u.CreditStatus = "none" }),
		},
		{
			name: "everything known overrides every stored spend field",
			incoming: with(func(u *VendorAccountUsage) {
				u.SpendUnit, u.SpendLimit, u.SpendUsed, u.SpendRemaining = "usd", "100", "10", "90"
				u.SpendUsedPct, u.SpendResetAt, u.CreditStatus = 10, &resetNew, "unlimited"
			}),
			want: keep(func(u *VendorAccountUsage) {
				u.SpendUnit, u.SpendLimit, u.SpendUsed, u.SpendRemaining = "usd", "100", "10", "90"
				u.SpendUsedPct, u.SpendResetAt, u.CreditStatus = 10, &resetNew, "unlimited"
			}),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := MergeVendorAccountUsage(existing, tc.incoming)
			if got.AccountID != tc.want.AccountID || !got.UpdatedAt.Equal(tc.want.UpdatedAt) ||
				got.SpendUnit != tc.want.SpendUnit || got.SpendLimit != tc.want.SpendLimit ||
				got.SpendUsed != tc.want.SpendUsed || got.SpendRemaining != tc.want.SpendRemaining ||
				got.SpendUsedPct != tc.want.SpendUsedPct || got.CreditStatus != tc.want.CreditStatus {
				t.Fatalf("spend scalar mismatch:\n got  %+v\n want %+v", got, tc.want)
			}
			if !mergeTimePtrEqual(got.SpendResetAt, tc.want.SpendResetAt) {
				t.Fatalf("SpendResetAt = %v, want %v", got.SpendResetAt, tc.want.SpendResetAt)
			}
		})
	}
}

func mergeTimePtrEqual(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Equal(*b)
}
