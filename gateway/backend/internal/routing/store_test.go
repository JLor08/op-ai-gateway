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
