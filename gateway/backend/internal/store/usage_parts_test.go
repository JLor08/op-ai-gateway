// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package store

import (
	"context"
	"op-ai-gateway/internal/usage"
	"testing"
	"time"
)

func seedUsageEventForParts(t *testing.T, s *SQLStore, id, host string, latencyMS int64, at time.Time) {
	t.Helper()
	s.Record(usage.Event{ID: id, UserID: "u1", Host: host, LatencyMS: latencyMS, CreatedAt: at})
	if err := s.LastUsageError(); err != nil {
		t.Fatalf("record usage %s: %v", id, err)
	}
}

// TestUsageEventsForServerWindowIncludesMemberRows pins that a cluster event
// (host = head server) is also returned for a member server that it lists in
// usage_event_hosts, exactly once, while an unrelated server still sees nothing.
func TestUsageEventsForServerWindowIncludesMemberRows(t *testing.T) {
	forEachDialect(t, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		at := time.Date(2026, 11, 5, 10, 0, 10, 0, time.UTC)
		seedUsageEventForParts(t, s, "ev_cluster", "srv_head", 10000, at)
		seedUsageEventForParts(t, s, "ev_member_local", "srv_member", 10000, at)
		hosts := []usage.EventHost{
			{EventID: "ev_cluster", ServerID: "srv_head", Role: "head", CreatedAt: at},
			{EventID: "ev_cluster", ServerID: "srv_member", Role: "member", CreatedAt: at},
			{EventID: "ev_member_local", ServerID: "srv_member", Role: "head", CreatedAt: at},
		}
		if err := s.RecordEventHosts(ctx, hosts); err != nil {
			t.Fatalf("record event hosts: %v", err)
		}
		from := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
		to := time.Date(2026, 11, 5, 10, 0, 20, 0, time.UTC)

		got, err := s.UsageEventsForServerWindow(ctx, "srv_member", from, to)
		if err != nil {
			t.Fatalf("window: %v", err)
		}
		ids := usageEventIDs(got)
		if len(ids) != 2 || !usageEventIDsContain(ids, "ev_cluster") || !usageEventIDsContain(ids, "ev_member_local") {
			t.Fatalf("member ids = %v, want ev_cluster and ev_member_local once each", ids)
		}
		got, err = s.UsageEventsForServerWindow(ctx, "srv_head", from, to)
		if err != nil {
			t.Fatalf("window head: %v", err)
		}
		if ids := usageEventIDs(got); len(ids) != 1 || ids[0] != "ev_cluster" {
			t.Fatalf("head ids = %v, want [ev_cluster] once", ids)
		}
		got, err = s.UsageEventsForServerWindow(ctx, "srv_other", from, to)
		if err != nil || len(got) != 0 {
			t.Fatalf("unrelated server got %v err=%v, want none", usageEventIDs(got), err)
		}
		// Outside the overlap window the member row does not match.
		got, err = s.UsageEventsForServerWindow(ctx, "srv_member", from.Add(2*time.Hour), to.Add(2*time.Hour))
		if err != nil || len(got) != 0 {
			t.Fatalf("late window got %v err=%v, want none", usageEventIDs(got), err)
		}
	})
}

func TestUsageEventHostsAndEnergyPartsRoundTrip(t *testing.T) {
	forEachDialect(t, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		at := time.Date(2026, 11, 5, 10, 0, 10, 0, time.UTC)
		seedUsageEventForParts(t, s, "ev1", "srv_head", 1000, at)

		if got, err := s.EventHosts(ctx, "ev1"); err != nil || len(got) != 0 {
			t.Fatalf("empty hosts = %v err=%v", got, err)
		}
		if err := s.RecordEventHosts(ctx, []usage.EventHost{
			{EventID: "ev1", ServerID: "srv_b", Role: "member", CreatedAt: at},
			{EventID: "ev1", ServerID: "srv_head", Role: "head", CreatedAt: at},
		}); err != nil {
			t.Fatalf("record hosts: %v", err)
		}
		// Recording again is idempotent.
		if err := s.RecordEventHosts(ctx, []usage.EventHost{{EventID: "ev1", ServerID: "srv_b", Role: "member", CreatedAt: at}}); err != nil {
			t.Fatalf("re-record hosts: %v", err)
		}
		hosts, err := s.EventHosts(ctx, "ev1")
		if err != nil {
			t.Fatalf("event hosts: %v", err)
		}
		if len(hosts) != 2 || hosts[0].ServerID != "srv_head" || hosts[0].Role != "head" || hosts[1].ServerID != "srv_b" || !hosts[1].CreatedAt.Equal(at) {
			t.Fatalf("hosts = %+v, want head first then member with created_at", hosts)
		}

		parts := []usage.EnergyPart{
			{EventID: "ev1", ServerID: "srv_head", Role: "head", Wh: 0.123456789, MarginalWh: 0.0123456789, Source: "measured"},
			{EventID: "ev1", ServerID: "srv_b", Role: "member", Wh: 0.5, MarginalWh: 0.25, Source: "modeled"},
		}
		if err := s.SetEnergyParts(ctx, "ev1", parts); err != nil {
			t.Fatalf("set parts: %v", err)
		}
		got, err := s.EnergyParts(ctx, "ev1")
		if err != nil {
			t.Fatalf("energy parts: %v", err)
		}
		if len(got) != 2 || got[0] != parts[0] || got[1] != parts[1] {
			t.Fatalf("parts = %+v, want %+v", got, parts)
		}
		// Setting replaces the previous set.
		if err := s.SetEnergyParts(ctx, "ev1", parts[:1]); err != nil {
			t.Fatalf("replace parts: %v", err)
		}
		if got, _ := s.EnergyParts(ctx, "ev1"); len(got) != 1 {
			t.Fatalf("after replace parts = %+v, want 1", got)
		}

		// Deleting the usage event cascades both child tables.
		mustExec(ctx, t, s, `delete from usage_events where id = ?`, "ev1")
		if got, _ := s.EventHosts(ctx, "ev1"); len(got) != 0 {
			t.Fatalf("hosts survived cascade: %+v", got)
		}
		if got, _ := s.EnergyParts(ctx, "ev1"); len(got) != 0 {
			t.Fatalf("parts survived cascade: %+v", got)
		}
	})
}

// usagePartsContract pins the write semantics every usage.Store must share:
// an unknown event is an error, a part list naming one server twice is an
// error, a repeated host is kept once, and a failed call writes nothing.
func usagePartsContract(t *testing.T, st usage.Store, record func(id string, at time.Time)) {
	t.Helper()
	ctx := context.Background()
	at := time.Date(2026, 11, 5, 10, 0, 10, 0, time.UTC)
	record("ev1", at)

	if err := st.RecordEventHosts(ctx, []usage.EventHost{
		{EventID: "ev1", ServerID: "srv_a", Role: "head", CreatedAt: at},
		{EventID: "ev_unknown", ServerID: "srv_a", Role: "head", CreatedAt: at},
	}); err == nil {
		t.Fatal("RecordEventHosts accepted an unknown event")
	}
	if got, _ := st.EventHosts(ctx, "ev1"); len(got) != 0 {
		t.Fatalf("failed RecordEventHosts partially wrote %+v", got)
	}
	if err := st.RecordEventHosts(ctx, []usage.EventHost{
		{EventID: "ev1", ServerID: "srv_a", Role: "head", CreatedAt: at},
		{EventID: "ev1", ServerID: "srv_a", Role: "member", CreatedAt: at},
	}); err != nil {
		t.Fatalf("duplicate host in one batch: %v", err)
	}
	if got, _ := st.EventHosts(ctx, "ev1"); len(got) != 1 || got[0].Role != "head" {
		t.Fatalf("hosts = %+v, want the first row kept once", got)
	}

	good := []usage.EnergyPart{{EventID: "ev1", ServerID: "srv_a", Role: "head", Wh: 1, Source: "modeled"}}
	if err := st.SetEnergyParts(ctx, "ev1", good); err != nil {
		t.Fatalf("set parts: %v", err)
	}
	if err := st.SetEnergyParts(ctx, "ev_unknown", good); err == nil {
		t.Fatal("SetEnergyParts accepted an unknown event")
	}
	dup := []usage.EnergyPart{
		{ServerID: "srv_b", Role: "member", Wh: 2},
		{ServerID: "srv_b", Role: "member", Wh: 3},
	}
	if err := st.SetEnergyParts(ctx, "ev1", dup); err == nil {
		t.Fatal("SetEnergyParts accepted two parts for one server")
	}
	if got, _ := st.EnergyParts(ctx, "ev1"); len(got) != 1 || got[0] != good[0] {
		t.Fatalf("failed SetEnergyParts changed the parts: %+v", got)
	}
}

func TestUsagePartsWriteContractMemory(t *testing.T) {
	rec := usage.NewRecorder()
	usagePartsContract(t, rec, func(id string, at time.Time) {
		rec.Record(usage.Event{ID: id, UserID: "u1", Host: "srv_a", CreatedAt: at})
	})
}

func TestUsagePartsWriteContractSQL(t *testing.T) {
	forEachDialect(t, func(t *testing.T, s *SQLStore) {
		usagePartsContract(t, s, func(id string, at time.Time) {
			seedUsageEventForParts(t, s, id, "srv_a", 100, at)
		})
	})
}
