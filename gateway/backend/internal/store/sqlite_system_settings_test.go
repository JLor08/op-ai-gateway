// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package store

import (
	"context"
	"testing"
	"time"
)

func TestSQLiteSystemSettingsFreshStoreReturnsEmptyMap(t *testing.T) {
	ctx := context.Background()
	st := openMigratedTestSQLite(t)
	defer st.Close()

	got, err := st.SystemSettings(ctx)
	if err != nil {
		t.Fatalf("SystemSettings returned %v", err)
	}
	if got == nil {
		t.Fatalf("SystemSettings returned nil map, want non-nil empty map")
	}
	if len(got) != 0 {
		t.Fatalf("SystemSettings = %#v, want empty map", got)
	}
}

func TestSQLiteSystemSettingsSetAndRead(t *testing.T) {
	ctx := context.Background()
	st := openMigratedTestSQLite(t)
	defer st.Close()
	now := time.Date(2026, 7, 12, 10, 0, 0, 0, time.UTC)

	if err := st.SetSystemSetting(ctx, "theme", "matrix", now); err != nil {
		t.Fatalf("SetSystemSetting returned %v", err)
	}

	got, err := st.SystemSettings(ctx)
	if err != nil {
		t.Fatalf("SystemSettings returned %v", err)
	}
	want := map[string]string{"theme": "matrix"}
	if len(got) != len(want) || got["theme"] != want["theme"] {
		t.Fatalf("SystemSettings = %#v, want %#v", got, want)
	}
}

func TestConformanceSetSystemSettingsWritesAllKeys(t *testing.T) {
	forEachDialect(t, func(t *testing.T, st *SQLStore) {
		ctx := context.Background()
		now := time.Date(2026, 7, 12, 10, 0, 0, 0, time.UTC)

		if err := st.SetSystemSettings(ctx, map[string]string{
			"cert_enabled":     "true",
			"cert_issuer_mode": "self_signed",
			"cert_base_domain": "int.example.test",
		}, now); err != nil {
			t.Fatalf("SetSystemSettings returned %v", err)
		}

		got, err := st.SystemSettings(ctx)
		if err != nil {
			t.Fatalf("SystemSettings returned %v", err)
		}
		want := map[string]string{
			"cert_enabled":     "true",
			"cert_issuer_mode": "self_signed",
			"cert_base_domain": "int.example.test",
		}
		if len(got) != len(want) {
			t.Fatalf("SystemSettings = %#v, want %#v", got, want)
		}
		for k, v := range want {
			if got[k] != v {
				t.Fatalf("SystemSettings[%q] = %q, want %q", k, got[k], v)
			}
		}
	})
}

// TestConformanceSetSystemSettingsRespectsCancelledContext covers the "cannot
// even begin" failure path on both dialects: a pre-cancelled context fails at
// BeginTx, before any upsert runs, so nothing is written and a pre-existing
// baseline is untouched. The stronger property -- that a failure PART WAY through
// the batch rolls the already-applied writes back -- needs a fault injected after
// a successful write and is proved by TestSetSystemSettingsRollsBackPartialWrite.
func TestConformanceSetSystemSettingsRespectsCancelledContext(t *testing.T) {
	forEachDialect(t, func(t *testing.T, st *SQLStore) {
		ctx := context.Background()
		now := time.Date(2026, 7, 12, 10, 0, 0, 0, time.UTC)

		// A baseline value that must survive a later failed batch untouched.
		if err := st.SetSystemSetting(ctx, "cert_issuer_mode", "acme", now); err != nil {
			t.Fatalf("seed baseline: %v", err)
		}

		cancelledCtx, cancel := context.WithCancel(ctx)
		cancel()
		err := st.SetSystemSettings(cancelledCtx, map[string]string{
			"cert_enabled":     "true",
			"cert_issuer_mode": "self_signed",
			"cert_base_domain": "int.example.test",
		}, now)
		if err == nil {
			t.Fatalf("SetSystemSettings with a cancelled context returned nil, want an error")
		}

		got, err := st.SystemSettings(ctx)
		if err != nil {
			t.Fatalf("SystemSettings returned %v", err)
		}
		if _, ok := got["cert_enabled"]; ok {
			t.Fatalf("cert_enabled was persisted despite the batch failing: %#v", got)
		}
		if _, ok := got["cert_base_domain"]; ok {
			t.Fatalf("cert_base_domain was persisted despite the batch failing: %#v", got)
		}
		if got["cert_issuer_mode"] != "acme" {
			t.Fatalf("cert_issuer_mode = %q, want the untouched baseline %q", got["cert_issuer_mode"], "acme")
		}
	})
}

func TestConformanceSetSystemSettingsEmptyIsNoOp(t *testing.T) {
	forEachDialect(t, func(t *testing.T, st *SQLStore) {
		ctx := context.Background()
		if err := st.SetSystemSettings(ctx, nil, time.Date(2026, 7, 12, 10, 0, 0, 0, time.UTC)); err != nil {
			t.Fatalf("SetSystemSettings(nil) returned %v", err)
		}
		if err := st.SetSystemSettings(ctx, map[string]string{}, time.Date(2026, 7, 12, 10, 0, 0, 0, time.UTC)); err != nil {
			t.Fatalf("SetSystemSettings(empty) returned %v", err)
		}
	})
}

func TestSQLiteSystemSettingsUpsertOverwrites(t *testing.T) {
	ctx := context.Background()
	st := openMigratedTestSQLite(t)
	defer st.Close()
	now := time.Date(2026, 7, 12, 10, 0, 0, 0, time.UTC)
	later := now.Add(time.Hour)

	if err := st.SetSystemSetting(ctx, "theme", "matrix", now); err != nil {
		t.Fatalf("SetSystemSetting (first) returned %v", err)
	}
	if err := st.SetSystemSetting(ctx, "theme", "default", later); err != nil {
		t.Fatalf("SetSystemSetting (upsert) returned %v", err)
	}

	got, err := st.SystemSettings(ctx)
	if err != nil {
		t.Fatalf("SystemSettings returned %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("SystemSettings = %#v, want exactly one row", got)
	}
	if got["theme"] != "default" {
		t.Fatalf("SystemSettings[theme] = %q, want %q", got["theme"], "default")
	}

	var count int
	if err := st.db.QueryRowContext(ctx, `select count(*) from system_settings`).Scan(&count); err != nil {
		t.Fatalf("count system_settings: %v", err)
	}
	if count != 1 {
		t.Fatalf("system_settings row count = %d, want 1", count)
	}
}
