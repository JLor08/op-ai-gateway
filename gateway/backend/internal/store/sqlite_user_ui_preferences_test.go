// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package store

import (
	"context"
	"testing"
)

func TestSQLiteUIPreferencesFreshUserReturnsEmpty(t *testing.T) {
	ctx := context.Background()
	st := openMigratedTestSQLite(t)
	defer st.Close()
	mustCreateUser(t, st, "usr_1", "u1@example.test")

	got, err := st.UIPreferences(ctx, "usr_1")
	if err != nil {
		t.Fatalf("UIPreferences returned %v", err)
	}
	if got == nil {
		t.Fatalf("UIPreferences returned nil slice, want non-nil empty slice")
	}
	if len(got) != 0 {
		t.Fatalf("UIPreferences = %#v, want empty", got)
	}
}

func TestSQLiteUIPreferencesUpsertOverwrites(t *testing.T) {
	ctx := context.Background()
	st := openMigratedTestSQLite(t)
	defer st.Close()
	mustCreateUser(t, st, "usr_1", "u1@example.test")

	if err := st.SetUIPreference(ctx, "usr_1", "table.activity", `{"cols":["a"]}`); err != nil {
		t.Fatalf("SetUIPreference (insert) returned %v", err)
	}
	if err := st.SetUIPreference(ctx, "usr_1", "table.activity", `{"cols":["a","b"]}`); err != nil {
		t.Fatalf("SetUIPreference (update) returned %v", err)
	}

	got, err := st.UIPreferences(ctx, "usr_1")
	if err != nil {
		t.Fatalf("UIPreferences returned %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("UIPreferences = %#v, want exactly one row", got)
	}
	if got[0].Key != "table.activity" || got[0].ValueJSON != `{"cols":["a","b"]}` {
		t.Fatalf("upsert did not overwrite: %#v", got[0])
	}

	var count int
	if err := st.db.QueryRowContext(ctx, `select count(*) from user_ui_preferences`).Scan(&count); err != nil {
		t.Fatalf("count user_ui_preferences: %v", err)
	}
	if count != 1 {
		t.Fatalf("user_ui_preferences row count = %d, want 1", count)
	}
}

func TestSQLiteUIPreferencesPerUserScoping(t *testing.T) {
	ctx := context.Background()
	st := openMigratedTestSQLite(t)
	defer st.Close()
	mustCreateUser(t, st, "usr_a", "a@example.test")
	mustCreateUser(t, st, "usr_b", "b@example.test")

	if err := st.SetUIPreference(ctx, "usr_a", "theme", `"dark"`); err != nil {
		t.Fatalf("SetUIPreference(a) returned %v", err)
	}
	if err := st.SetUIPreference(ctx, "usr_b", "theme", `"light"`); err != nil {
		t.Fatalf("SetUIPreference(b) returned %v", err)
	}

	a, err := st.UIPreferences(ctx, "usr_a")
	if err != nil {
		t.Fatalf("UIPreferences(a) returned %v", err)
	}
	if len(a) != 1 || a[0].ValueJSON != `"dark"` {
		t.Fatalf("user A prefs = %#v, want single dark theme", a)
	}
	b, err := st.UIPreferences(ctx, "usr_b")
	if err != nil {
		t.Fatalf("UIPreferences(b) returned %v", err)
	}
	if len(b) != 1 || b[0].ValueJSON != `"light"` {
		t.Fatalf("user B prefs = %#v, want single light theme (A must not leak)", b)
	}
}

func TestSQLiteUIPreferencesListOrderedByKey(t *testing.T) {
	ctx := context.Background()
	st := openMigratedTestSQLite(t)
	defer st.Close()
	mustCreateUser(t, st, "usr_1", "u1@example.test")

	// Insert out of alphabetical order; the list must come back sorted by key.
	for _, key := range []string{"zeta", "alpha", "mike"} {
		if err := st.SetUIPreference(ctx, "usr_1", key, `1`); err != nil {
			t.Fatalf("SetUIPreference(%s) returned %v", key, err)
		}
	}
	got, err := st.UIPreferences(ctx, "usr_1")
	if err != nil {
		t.Fatalf("UIPreferences returned %v", err)
	}
	keys := make([]string, len(got))
	for i, pref := range got {
		keys[i] = pref.Key
	}
	want := []string{"alpha", "mike", "zeta"}
	if len(keys) != len(want) {
		t.Fatalf("keys = %v, want %v", keys, want)
	}
	for i := range want {
		if keys[i] != want[i] {
			t.Fatalf("keys = %v, want %v (ordered by key)", keys, want)
		}
	}
}
