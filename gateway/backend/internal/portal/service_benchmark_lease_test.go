// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"
)

// TestBenchmarkOverrideLease pins the override lease's storage: one
// system_settings row per server, keyed benchmark_override_lease:<server_id>,
// holding the lease as JSON, and the empty string for a released row
// (system_settings has no delete). The reader returns only the leases that owe
// something, and never guesses at a value that does not parse.
func TestBenchmarkOverrideLease(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	newService := func(settings SystemSettingsStore) *Service {
		return NewService(ServiceDeps{SystemSettings: settings, Clock: func() time.Time { return now }})
	}
	raw := func(t *testing.T, settings SystemSettingsStore, key string) (string, bool) {
		t.Helper()
		values, err := settings.SystemSettings(ctx)
		if err != nil {
			t.Fatalf("SystemSettings: %v", err)
		}
		value, ok := values[key]
		return value, ok
	}

	t.Run("a lease is one JSON row under its server's key and reads back by server id", func(t *testing.T) {
		settings := NewMemorySystemSettings()
		svc := newService(settings)
		lease := BenchmarkOverrideLease{Repin: []string{"rs_a"}, ClearForceStopped: []string{"rs_b", "rs_c"}}
		if err := svc.SetBenchmarkOverrideLease(ctx, "srv1", lease); err != nil {
			t.Fatalf("SetBenchmarkOverrideLease: %v", err)
		}
		if got, _ := raw(t, settings, "benchmark_override_lease:srv1"); got != `{"repin":["rs_a"],"clear_force_stopped":["rs_b","rs_c"]}` {
			t.Fatalf("stored row = %q", got)
		}
		only := BenchmarkOverrideLease{Repin: []string{"rs_d"}}
		if err := svc.SetBenchmarkOverrideLease(ctx, "srv2", only); err != nil {
			t.Fatalf("SetBenchmarkOverrideLease(srv2): %v", err)
		}
		if got, _ := raw(t, settings, "benchmark_override_lease:srv2"); got != `{"repin":["rs_d"]}` {
			t.Fatalf("stored row (srv2) = %q, want only the list that owes something", got)
		}
		leases, err := svc.BenchmarkOverrideLeases(ctx)
		if err != nil {
			t.Fatalf("BenchmarkOverrideLeases: %v", err)
		}
		want := map[string]BenchmarkOverrideLease{"srv1": lease, "srv2": only}
		if !reflect.DeepEqual(leases, want) {
			t.Fatalf("leases = %#v, want %#v", leases, want)
		}
	})

	t.Run("an empty lease releases the row", func(t *testing.T) {
		settings := NewMemorySystemSettings()
		svc := newService(settings)
		if err := svc.SetBenchmarkOverrideLease(ctx, "srv1", BenchmarkOverrideLease{ClearForceStopped: []string{"rs_b"}}); err != nil {
			t.Fatalf("SetBenchmarkOverrideLease: %v", err)
		}
		if err := svc.SetBenchmarkOverrideLease(ctx, "srv1", BenchmarkOverrideLease{}); err != nil {
			t.Fatalf("SetBenchmarkOverrideLease(empty): %v", err)
		}
		got, ok := raw(t, settings, "benchmark_override_lease:srv1")
		if !ok || got != "" {
			t.Fatalf("released row = (%q, present %v), want ('', present)", got, ok)
		}
		leases, err := svc.BenchmarkOverrideLeases(ctx)
		if err != nil {
			t.Fatalf("BenchmarkOverrideLeases: %v", err)
		}
		if len(leases) != 0 {
			t.Fatalf("leases = %#v, want none after the release", leases)
		}
	})

	t.Run("only lease rows that owe something are read, and nothing else is logged", func(t *testing.T) {
		settings := NewMemorySystemSettings()
		for key, value := range map[string]string{
			"cert_enabled":                  "true",
			"benchmark_override_lease:srv2": "",
			"benchmark_override_lease:srv3": "{}",
			"benchmark_override_lease:srv4": `{"clear_force_stopped":["rs_x"]}`,
		} {
			if err := settings.SetSystemSetting(ctx, key, value, now); err != nil {
				t.Fatalf("seed %s: %v", key, err)
			}
		}
		svc := newService(settings)
		var leases map[string]BenchmarkOverrideLease
		var err error
		logs := captureSlog(t, func() { leases, err = svc.BenchmarkOverrideLeases(ctx) })
		if err != nil {
			t.Fatalf("BenchmarkOverrideLeases: %v", err)
		}
		want := map[string]BenchmarkOverrideLease{"srv4": {ClearForceStopped: []string{"rs_x"}}}
		if !reflect.DeepEqual(leases, want) {
			t.Fatalf("leases = %#v, want %#v", leases, want)
		}
		if logs != "" {
			t.Fatalf("logs = %q, want nothing: a released row, a lease that owes nothing and another setting are not malformed leases", logs)
		}
	})

	t.Run("a row that does not parse is skipped with a Warn and never guessed at", func(t *testing.T) {
		settings := NewMemorySystemSettings()
		for key, value := range map[string]string{
			"benchmark_override_lease:srv5": "not json",
			"benchmark_override_lease:srv6": `{"repin":["rs_a"],"clear_force_stopped":"rs_b"}`,
		} {
			if err := settings.SetSystemSetting(ctx, key, value, now); err != nil {
				t.Fatalf("seed %s: %v", key, err)
			}
		}
		svc := newService(settings)
		var leases map[string]BenchmarkOverrideLease
		var err error
		logs := captureSlog(t, func() { leases, err = svc.BenchmarkOverrideLeases(ctx) })
		if err != nil {
			t.Fatalf("BenchmarkOverrideLeases: %v", err)
		}
		if len(leases) != 0 {
			t.Fatalf("leases = %#v, want none: a value that does not parse is never guessed at, not even its readable half", leases)
		}
		for _, serverID := range []string{"srv5", "srv6"} {
			if !strings.Contains(logs, `level=WARN msg="benchmark: an override lease does not parse and is skipped" server_id=`+serverID+" ") {
				t.Fatalf("logs = %q, want the Warn for %s", logs, serverID)
			}
		}
	})

	t.Run("a store error is returned with what failed", func(t *testing.T) {
		svc := newService(erroringSettings{})
		if _, err := svc.BenchmarkOverrideLeases(ctx); err == nil || err.Error() != "read the benchmark override leases: boom" {
			t.Fatalf("BenchmarkOverrideLeases err = %v", err)
		}
		err := svc.SetBenchmarkOverrideLease(ctx, "srv1", BenchmarkOverrideLease{Repin: []string{"rs_a"}})
		if err == nil || err.Error() != "write the benchmark override lease of server srv1: boom" {
			t.Fatalf("SetBenchmarkOverrideLease err = %v", err)
		}
	})

	t.Run("a Service without a settings store holds no lease", func(t *testing.T) {
		svc := newService(nil)
		if err := svc.SetBenchmarkOverrideLease(ctx, "srv1", BenchmarkOverrideLease{Repin: []string{"rs_a"}}); err != nil {
			t.Fatalf("SetBenchmarkOverrideLease = %v, want nil", err)
		}
		leases, err := svc.BenchmarkOverrideLeases(ctx)
		if err != nil || len(leases) != 0 {
			t.Fatalf("BenchmarkOverrideLeases = (%#v, %v), want none and nil", leases, err)
		}
	})
}
