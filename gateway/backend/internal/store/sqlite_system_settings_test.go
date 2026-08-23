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
