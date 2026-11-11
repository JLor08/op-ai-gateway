// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package store

import (
	"context"
	"op-ai-gateway/internal/routing"
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

// TestUsageCostPricesPartsByPartHost pins that the store's per-host energy
// sums attribute a cluster request's energy to the host of each part (head and
// members), while a request without parts stays on its own host, and that
// counts are never multiplied by the parts: EnergyByServer, UsageGroups and
// the cost of UsageAggregateSince.
func TestUsageCostPricesPartsByPartHost(t *testing.T) {
	forEachDialect(t, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		now := time.Now().UTC().Truncate(time.Second)
		for _, srv := range []routing.AIServer{
			{ID: "srv_head", Name: "head", Domain: "head.example.test", PricePerKwh: 0.25},
			{ID: "srv_mem", Name: "mem", Domain: "mem.example.test", PricePerKwh: 0.5},
		} {
			srv.Provider, srv.Endpoint = routing.ProviderOllama, "http://"+srv.Domain+":11434"
			srv.Status, srv.HealthStatus, srv.CreatedAt, srv.UpdatedAt = routing.ServerStatusActive, routing.HealthHealthy, now, now
			if err := s.CreateAIServer(ctx, srv); err != nil {
				t.Fatalf("create %s: %v", srv.ID, err)
			}
		}
		for _, e := range []usage.Event{
			{ID: "ev_cluster", UserID: "u1", Model: "big", Host: "srv_head", EnergyWh: 3000, EnergySource: "measured", Status: "ok", HTTPStatus: 200, CreatedAt: now},
			{ID: "ev_plain", UserID: "u1", Model: "big", Host: "srv_head", EnergyWh: 1000, EnergySource: "measured", Status: "ok", HTTPStatus: 200, CreatedAt: now},
		} {
			s.Record(e)
			if err := s.LastUsageError(); err != nil {
				t.Fatalf("record %s: %v", e.ID, err)
			}
		}
		if err := s.SetEnergyParts(ctx, "ev_cluster", []usage.EnergyPart{
			{ServerID: "srv_head", Role: usage.EventHostRoleHead, Wh: 1000, Source: "measured"},
			{ServerID: "srv_mem", Role: usage.EventHostRoleMember, Wh: 2000, Source: "measured"},
		}); err != nil {
			t.Fatal(err)
		}
		q := usage.Query{UserID: "u1"}

		byServer, err := s.EnergyByServer(ctx, q)
		if err != nil {
			t.Fatal(err)
		}
		if len(byServer) != 2 || !closeTo(byServer["srv_head"], 2000) || !closeTo(byServer["srv_mem"], 2000) {
			t.Fatalf("energy by server = %v, want head 2000 and member 2000", byServer)
		}

		buckets, err := s.UsageGroups(ctx, q, "model")
		if err != nil {
			t.Fatal(err)
		}
		got := map[string]usage.GroupBucket{}
		for _, b := range buckets {
			if b.Key != "big" {
				t.Fatalf("bucket %+v under an unexpected key", b)
			}
			got[b.Host] = b
		}
		if len(got) != 2 || got["srv_head"].Count != 2 || !closeTo(got["srv_head"].EnergyWh, 2000) ||
			got["srv_mem"].Count != 0 || !closeTo(got["srv_mem"].EnergyWh, 2000) {
			t.Fatalf("groups = %+v, want head 2 requests/2000 Wh and member 0 requests/2000 Wh", got)
		}

		requests, _, cost, err := s.UsageAggregateSince(ctx, routing.PrincipalTypeUser, "u1", now.Add(-time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		// head 2 kWh × 0.25 + member 2 kWh × 0.5.
		if requests != 2 || !closeTo(cost, 1.5) {
			t.Fatalf("aggregate = %d requests, cost %v; want 2 and 1.5", requests, cost)
		}

		parts, err := s.EnergyPartsByEvents(ctx, []string{"ev_cluster", "ev_plain", "ev_missing"})
		if err != nil {
			t.Fatal(err)
		}
		if len(parts) != 1 || len(parts["ev_cluster"]) != 2 || parts["ev_cluster"][0].ServerID != "srv_head" || parts["ev_cluster"][1].ServerID != "srv_mem" {
			t.Fatalf("parts by events = %+v, want ev_cluster's two parts, head first", parts)
		}
		if none, err := s.EnergyPartsByEvents(ctx, nil); err != nil || len(none) != 0 {
			t.Fatalf("parts of no events = %v err=%v", none, err)
		}
	})
}

// closeTo tolerates postgres' float4 energy columns.
func closeTo(a, b float64) bool { return a-b < 1e-3 && b-a < 1e-3 }
