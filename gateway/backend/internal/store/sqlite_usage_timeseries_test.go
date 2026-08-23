// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package store

import (
	"op-ai-gateway/internal/usage"
	"testing"
	"time"
)

// tsWindowEvent builds a usage event at from+offsetSecs with the given latency
// and token counts, scoped to userID.
func tsWindowEvent(id, userID string, from time.Time, offsetSecs int, latencyMS int64, in, out int) usage.Event {
	e := testUsageEvent(id, userID, "tok_1", "success")
	e.HTTPStatus = 200
	e.CreatedAt = from.Add(time.Duration(offsetSecs) * time.Second)
	e.LatencyMS = latencyMS
	e.InputTokens = in
	e.OutputTokens = out
	return e
}

func TestSQLiteUsageTimeSeries(t *testing.T) {
	st := openMigratedTestSQLite(t)
	defer st.Close()

	from := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	to := from.Add(10 * time.Second) // single 10s bucket
	st.Record(tsWindowEvent("req_1", "usr_1", from, 1, 0, 100, 40))
	st.Record(tsWindowEvent("req_2", "usr_1", from, 2, 0, 100, 40))
	st.Record(tsWindowEvent("req_other", "usr_2", from, 3, 0, 100, 40))   // filtered out in own scope
	st.Record(tsWindowEvent("req_before", "usr_1", from, -5, 0, 100, 40)) // before window

	ts, err := st.TimeSeries(usage.Query{UserID: "usr_1", From: from, To: to}, 10)
	if err != nil {
		t.Fatalf("TimeSeries returned err: %v", err)
	}
	if err := st.LastUsageError(); err != nil {
		t.Fatalf("LastUsageError = %v", err)
	}
	if len(ts.Points) != 1 {
		t.Fatalf("Points = %d, want 1", len(ts.Points))
	}
	if ts.Points[0].Connections != 2 {
		t.Fatalf("Connections = %d, want 2 (own scope, in window)", ts.Points[0].Connections)
	}
	if ts.Points[0].PromptTokensPerSecond != 20 {
		t.Fatalf("PromptTokensPerSecond = %v, want 20 (200/10s)", ts.Points[0].PromptTokensPerSecond)
	}
	if ts.Points[0].CompletionTokensPerSecond != 8 {
		t.Fatalf("CompletionTokensPerSecond = %v, want 8 (80/10s)", ts.Points[0].CompletionTokensPerSecond)
	}

	// ScopeAll counts usr_2's event too.
	all, err := st.TimeSeries(usage.Query{ScopeAll: true, From: from, To: to}, 10)
	if err != nil {
		t.Fatalf("TimeSeries returned err: %v", err)
	}
	if all.Points[0].Connections != 3 {
		t.Fatalf("scope=all Connections = %d, want 3", all.Points[0].Connections)
	}
}

// TestSQLiteUsageTimeSeriesServerFilter proves TimeSeries applies q.Server: with
// events for two servers ("alpha" and "beta") in one bucket, filtering
// Server:"alpha" sums only alpha's tokens and excludes beta's (mirrors the
// usageWhere server predicate = server_name substring with a host fallback).
func TestSQLiteUsageTimeSeriesServerFilter(t *testing.T) {
	st := openMigratedTestSQLite(t)
	defer st.Close()

	from := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	to := from.Add(10 * time.Second) // single 10s bucket

	alpha := tsWindowEvent("req_alpha", "usr_1", from, 1, 0, 100, 40)
	alpha.ServerName = "alpha"
	alpha.Host = ""
	st.Record(alpha)

	beta := tsWindowEvent("req_beta", "usr_1", from, 2, 0, 200, 80)
	beta.ServerName = "beta"
	beta.Host = ""
	st.Record(beta)

	ts, err := st.TimeSeries(usage.Query{From: from, To: to, ScopeAll: true, Server: "alpha"}, 10)
	if err != nil {
		t.Fatalf("TimeSeries returned err: %v", err)
	}
	if err := st.LastUsageError(); err != nil {
		t.Fatalf("LastUsageError = %v", err)
	}
	if len(ts.Points) != 1 {
		t.Fatalf("Points = %d, want 1", len(ts.Points))
	}
	if ts.Points[0].Connections != 1 {
		t.Fatalf("Connections = %d, want 1 (only alpha, beta excluded)", ts.Points[0].Connections)
	}
	if ts.Points[0].PromptTokensPerSecond != 10 {
		t.Fatalf("PromptTokensPerSecond = %v, want 10 (alpha 100/10s, beta excluded)", ts.Points[0].PromptTokensPerSecond)
	}
	if ts.Points[0].CompletionTokensPerSecond != 4 {
		t.Fatalf("CompletionTokensPerSecond = %v, want 4 (alpha 40/10s, beta excluded)", ts.Points[0].CompletionTokensPerSecond)
	}
}

// TestSQLiteUsageTimeSeriesProjectFilter proves TimeSeries honors the project
// clauses that mirror usageWhere (design spec §7/§8): a ProjectIDs IN-list sums
// only the matching project's rows, and a non-nil EMPTY ProjectIDs (a non-admin
// who is a member of zero projects) matches ZERO rows even under ScopeAll -- the
// defensive guard so a future timeseries-by-project can't leak other members'
// rows once the caller's own-user pin is dropped (ScopeAll=true).
func TestSQLiteUsageTimeSeriesProjectFilter(t *testing.T) {
	st := openMigratedTestSQLite(t)
	defer st.Close()

	from := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	to := from.Add(10 * time.Second) // single 10s bucket

	inP := tsWindowEvent("req_inproj", "usr_1", from, 1, 0, 100, 40)
	inP.ProjectID = "prj_alpha"
	st.Record(inP)

	outP := tsWindowEvent("req_outproj", "usr_2", from, 2, 0, 200, 80)
	outP.ProjectID = "prj_beta"
	st.Record(outP)

	// ScopeAll (user pin dropped) + ProjectIDs=[prj_alpha]: only alpha's row.
	ts, err := st.TimeSeries(usage.Query{From: from, To: to, ScopeAll: true, ProjectIDs: []string{"prj_alpha"}}, 10)
	if err != nil {
		t.Fatalf("TimeSeries returned err: %v", err)
	}
	if err := st.LastUsageError(); err != nil {
		t.Fatalf("LastUsageError = %v", err)
	}
	if len(ts.Points) != 1 || ts.Points[0].Connections != 1 {
		t.Fatalf("ProjectIDs filter: Points/Connections = %v, want 1 conn (only prj_alpha)", ts.Points)
	}
	if ts.Points[0].PromptTokensPerSecond != 10 {
		t.Fatalf("PromptTokensPerSecond = %v, want 10 (alpha 100/10s, beta excluded)", ts.Points[0].PromptTokensPerSecond)
	}

	// ProjectIDExact drill-down: only alpha's row.
	tsExact, err := st.TimeSeries(usage.Query{From: from, To: to, ScopeAll: true, HasProjectIDExact: true, ProjectIDExact: "prj_beta"}, 10)
	if err != nil {
		t.Fatalf("TimeSeries returned err: %v", err)
	}
	if len(tsExact.Points) != 1 || tsExact.Points[0].Connections != 1 || tsExact.Points[0].CompletionTokensPerSecond != 8 {
		t.Fatalf("ProjectIDExact filter: Points = %v, want 1 conn / 8 gen tok/s (only prj_beta)", tsExact.Points)
	}

	// Non-nil EMPTY ProjectIDs under ScopeAll => zero matching rows (never falls
	// back to all). ComputeTimeSeries still emits the window's bucket, so assert
	// on Connections (no events matched), not the point count.
	tsEmpty, err := st.TimeSeries(usage.Query{From: from, To: to, ScopeAll: true, ProjectIDs: []string{}}, 10)
	if err != nil {
		t.Fatalf("TimeSeries returned err: %v", err)
	}
	for _, p := range tsEmpty.Points {
		if p.Connections != 0 {
			t.Fatalf("empty ProjectIDs: Connections = %d, want 0 (member of zero projects sees nothing)", p.Connections)
		}
	}
}

// TestSQLiteUsageTimeSeriesCoarseBucket exercises a coarse window/bucket pair
// (1d window, 3600s/1h buckets) end to end through the SQLite path.
func TestSQLiteUsageTimeSeriesCoarseBucket(t *testing.T) {
	st := openMigratedTestSQLite(t)
	defer st.Close()

	from := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	to := from.Add(24 * time.Hour) // 1d window
	// Two events in hour-bucket 0, one in hour-bucket 5.
	st.Record(tsWindowEvent("req_1", "usr_1", from, 60, 0, 100, 40))
	st.Record(tsWindowEvent("req_2", "usr_1", from, 120, 0, 100, 40))
	st.Record(tsWindowEvent("req_3", "usr_1", from, 5*3600+10, 0, 100, 40))

	ts, err := st.TimeSeries(usage.Query{UserID: "usr_1", From: from, To: to}, 3600)
	if err != nil {
		t.Fatalf("TimeSeries returned err: %v", err)
	}
	if err := st.LastUsageError(); err != nil {
		t.Fatalf("LastUsageError = %v", err)
	}
	if ts.BucketSeconds != 3600 {
		t.Fatalf("BucketSeconds = %d, want 3600", ts.BucketSeconds)
	}
	if len(ts.Points) != 24 {
		t.Fatalf("Points = %d, want 24 (1d / 1h)", len(ts.Points))
	}
	if ts.Points[0].Connections != 2 {
		t.Fatalf("hour 0 Connections = %d, want 2", ts.Points[0].Connections)
	}
	if ts.Points[5].Connections != 1 {
		t.Fatalf("hour 5 Connections = %d, want 1", ts.Points[5].Connections)
	}
}

// TestSQLiteUsageTimeSeriesCoarsens proves the coarsening (not truncation) path is
// reached through the SQLite store: a 1y window with 1s buckets returns a bounded
// series that still covers the full window.
func TestSQLiteUsageTimeSeriesCoarsens(t *testing.T) {
	st := openMigratedTestSQLite(t)
	defer st.Close()

	from := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	to := from.Add(365 * 24 * time.Hour) // 1y window
	// One event near the very end of the window; it must survive coarsening and
	// land in the (covered) last bucket, proving the tail is not truncated.
	st.Record(tsWindowEvent("req_late", "usr_1", from, 365*24*3600-30, 0, 100, 40))

	ts, err := st.TimeSeries(usage.Query{UserID: "usr_1", From: from, To: to}, 1)
	if err != nil {
		t.Fatalf("TimeSeries returned err: %v", err)
	}
	if err := st.LastUsageError(); err != nil {
		t.Fatalf("LastUsageError = %v", err)
	}
	if len(ts.Points) == 0 || len(ts.Points) > 5000 {
		t.Fatalf("Points = %d, want in (0,5000] (coarsened)", len(ts.Points))
	}
	if ts.BucketSeconds <= 1 {
		t.Fatalf("BucketSeconds = %d, want > 1 (coarsened)", ts.BucketSeconds)
	}
	if !ts.Points[0].T.Equal(from) {
		t.Fatalf("Points[0].T = %v, want from=%v", ts.Points[0].T, from)
	}
	last := ts.Points[len(ts.Points)-1].T
	bucket := time.Duration(ts.BucketSeconds) * time.Second
	if !last.Before(to) || last.Add(bucket).Before(to) {
		t.Fatalf("last bucket [%v,%v) does not cover to=%v", last, last.Add(bucket), to)
	}
	// The late event must be counted (window fully covered, no truncation).
	total := 0
	for _, p := range ts.Points {
		total += p.Connections
	}
	if total != 1 {
		t.Fatalf("total connections = %d, want 1 (late event retained)", total)
	}
}

// TestSQLiteUsageTimeSeriesEnergySum proves the SQL TimeSeries SELECT carries
// energy_wh through to the bucketed EnergyWh sum (a plain per-bucket total).
func TestSQLiteUsageTimeSeriesEnergySum(t *testing.T) {
	st := openMigratedTestSQLite(t)
	defer st.Close()

	from := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	to := from.Add(10 * time.Second) // single 10s bucket

	e1 := tsWindowEvent("req_1", "usr_1", from, 1, 0, 100, 40)
	e1.EnergyWh = 0.4
	st.Record(e1)
	e2 := tsWindowEvent("req_2", "usr_1", from, 2, 0, 100, 40)
	e2.EnergyWh = 0.6
	st.Record(e2)
	outOfWindow := tsWindowEvent("req_before", "usr_1", from, -5, 0, 100, 40)
	outOfWindow.EnergyWh = 100
	st.Record(outOfWindow)

	ts, err := st.TimeSeries(usage.Query{UserID: "usr_1", From: from, To: to}, 10)
	if err != nil {
		t.Fatalf("TimeSeries returned err: %v", err)
	}
	if err := st.LastUsageError(); err != nil {
		t.Fatalf("LastUsageError = %v", err)
	}
	if len(ts.Points) != 1 {
		t.Fatalf("Points = %d, want 1", len(ts.Points))
	}
	if ts.Points[0].EnergyWh != 1.0 {
		t.Fatalf("EnergyWh = %v, want 1.0 (0.4+0.6, out-of-window excluded)", ts.Points[0].EnergyWh)
	}
}

// TestCrossStoreTimeSeriesParity proves the memory Recorder and SQLiteStore
// return identical Points for the same recorded events and query (mirrors the
// capture/stats cross-store parity style).
//
// ST-1: SQLiteStore.TimeSeries used to build its own inline WHERE covering only
// ~8 of usageWhere's ~25 filter dimensions; it now delegates to usageWhere
// itself (see TimeSeries in sqlite_usage.go), so the filtered cases below
// (server/serverExact/token/service/project) exercise dimensions that were
// already present in the old inline subset EXCEPT server_exact_empty_pin,
// whose old inline check was `if q.ServerExact != ""` (no HasServerExact) —
// with ServerExact=="" that condition never fired, so the old code silently
// dropped the pin and matched every row instead of just the empty-server-name
// bucket. That case's wantTotalConn assertion (and the memory/sqlite parity
// check itself) fails if TimeSeries is reverted to the old subset.
func TestCrossStoreTimeSeriesParity(t *testing.T) {
	from := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	to := from.Add(5 * time.Second) // 5 buckets of 1s
	events := []usage.Event{
		tsWindowEvent("req_a", "usr_1", from, 0, 0, 10, 5),      // conn bucket 0
		tsWindowEvent("req_b", "usr_1", from, 3, 3000, 20, 7),   // conn bucket 3, concurrency buckets 0..2
		tsWindowEvent("req_c", "usr_1", from, 4, 1500, 30, 9),   // conn bucket 4, concurrency buckets 2..3
		tsWindowEvent("req_other", "usr_2", from, 1, 0, 99, 99), // other user (excluded in own scope)
	}
	events[0].EnergyWh = 0.1
	events[1].EnergyWh = 0.2
	events[2].EnergyWh = 0.3

	// Distinct server/token/service/project identity per event, purely so the
	// filtered parity cases below have something real to select on.
	events[0].ServerName = "alpha"      // req_a: exact "alpha"
	events[1].ServerName = "alpha-prod" // req_b: substring-only match on "alpha"
	events[1].TokenID = "tok_2"
	events[1].ServiceID = "svc_1"
	events[1].ProjectID = "prj_y"
	events[2].ServerName = "beta" // req_c: excluded from every "alpha" server filter
	events[2].ProjectID = "prj_x"
	// req_other (events[3]) keeps ServerName == "" (the empty-server-name pin
	// target) and the tsWindowEvent defaults TokenID "tok_1" / ServiceID "" /
	// ProjectID "".

	mem := usage.NewRecorder()
	st := openMigratedTestSQLite(t)
	defer st.Close()
	for _, e := range events {
		mem.Record(e)
		st.Record(e)
	}

	for _, tc := range []struct {
		name string
		q    usage.Query
		// wantTotalConn, when >= 0, asserts the sqlite store's total Connections
		// across all buckets equals this count -- a genuine-coverage check so a
		// filtered case can't pass vacuously via both stores agreeing on zero.
		wantTotalConn int
	}{
		{"own", usage.Query{UserID: "usr_1", From: from, To: to}, -1},
		{"all", usage.Query{ScopeAll: true, From: from, To: to}, -1},
		// Server: substring over server_name (host fallback) -- matches both
		// "alpha" and "alpha-prod".
		{"server_substring", usage.Query{ScopeAll: true, From: from, To: to, Server: "alpha"}, 2},
		// ServerExact: exact case-insensitive match -- "alpha-prod" must NOT
		// satisfy an exact pin of "alpha".
		{"server_exact", usage.Query{ScopeAll: true, From: from, To: to, ServerExact: "alpha"}, 1},
		// ServerExact empty-server-name pin (HasServerExact + ServerExact==""):
		// matches only req_other, whose ServerName is "".
		{"server_exact_empty_pin", usage.Query{ScopeAll: true, From: from, To: to, HasServerExact: true, ServerExact: ""}, 1},
		// token_id: matches only req_b (tok_2).
		{"token_id", usage.Query{ScopeAll: true, From: from, To: to, HasTokenFilter: true, TokenID: "tok_2"}, 1},
		// service_id: matches only req_b (svc_1).
		{"service_id", usage.Query{ScopeAll: true, From: from, To: to, HasServiceFilter: true, ServiceID: "svc_1"}, 1},
		// ProjectIDs IN-list: matches req_b (prj_y) and req_c (prj_x).
		{"project_ids", usage.Query{ScopeAll: true, From: from, To: to, ProjectIDs: []string{"prj_x", "prj_y"}}, 2},
	} {
		memTS, err := mem.TimeSeries(tc.q, 1)
		if err != nil {
			t.Fatalf("%s: memory TimeSeries returned err: %v", tc.name, err)
		}
		sqlTS, err := st.TimeSeries(tc.q, 1)
		if err != nil {
			t.Fatalf("%s: sqlite TimeSeries returned err: %v", tc.name, err)
		}
		if err := st.LastUsageError(); err != nil {
			t.Fatalf("%s: LastUsageError = %v", tc.name, err)
		}
		if len(memTS.Points) != len(sqlTS.Points) {
			t.Fatalf("%s: point count memory=%d sqlite=%d", tc.name, len(memTS.Points), len(sqlTS.Points))
		}
		for i := range memTS.Points {
			m, s := memTS.Points[i], sqlTS.Points[i]
			if !m.T.Equal(s.T) || m.Connections != s.Connections || m.Concurrency != s.Concurrency ||
				m.PromptTokensPerSecond != s.PromptTokensPerSecond || m.CompletionTokensPerSecond != s.CompletionTokensPerSecond ||
				m.EnergyWh != s.EnergyWh {
				t.Fatalf("%s: bucket %d mismatch memory=%#v sqlite=%#v", tc.name, i, m, s)
			}
		}
		if tc.wantTotalConn >= 0 {
			total := 0
			for _, p := range sqlTS.Points {
				total += p.Connections
			}
			if total != tc.wantTotalConn {
				t.Fatalf("%s: sqlite total connections = %d, want %d", tc.name, total, tc.wantTotalConn)
			}
		}
	}
}
