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

// TestUsageMemberPartBuckets pins the member-part buckets: UsageGroups emits
// another host's part as an energy-only bucket flagged MemberParts, never
// merged into that host's own request bucket (so the portal can re-key it),
// and MemberPartEnergy sums the parts drawn on the given hosts for requests
// served by any other host -- energy only, under the request's group value.
func TestUsageMemberPartBuckets(t *testing.T) {
	forEachDialect(t, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		now := time.Now().UTC().Truncate(time.Second)
		for _, e := range []usage.Event{
			{ID: "ev_cluster", UserID: "u1", Model: "big", Host: "srv_head", ServerName: "Head", InputTokens: 7, EnergyWh: 3000, Status: "ok", HTTPStatus: 200, CreatedAt: now},
			{ID: "ev_local", UserID: "u1", Model: "big", Host: "srv_mem", ServerName: "Member", InputTokens: 3, EnergyWh: 500, Status: "ok", HTTPStatus: 200, CreatedAt: now},
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
		assertMemberPartBuckets(t, s, q)
	})
}

// assertMemberPartBuckets runs the member-part bucket expectations on st,
// seeded as TestUsageMemberPartBuckets seeds it.
func assertMemberPartBuckets(t *testing.T, st usage.Store, q usage.Query) {
	t.Helper()
	ctx := context.Background()
	buckets, err := st.UsageGroups(ctx, q, "model")
	if err != nil {
		t.Fatal(err)
	}
	var own, member *usage.GroupBucket
	for i := range buckets {
		b := &buckets[i]
		if b.Host != "srv_mem" {
			continue
		}
		if b.MemberParts {
			member = b
		} else {
			own = b
		}
	}
	if own == nil || own.Count != 1 || !closeTo(own.EnergyWh, 500) || own.InputTokens != 3 {
		t.Fatalf("member host's own bucket = %+v, want 1 request, 500 Wh", own)
	}
	if member == nil || member.Count != 0 || member.InputTokens != 0 || member.Key != "big" || !closeTo(member.EnergyWh, 2000) {
		t.Fatalf("member part bucket = %+v, want energy only (2000 Wh) under the request's group", member)
	}

	got, err := st.MemberPartEnergy(ctx, q, []string{"srv_mem"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Host != "srv_mem" || got[0].Key != "" || !got[0].MemberParts || got[0].Count != 0 || !closeTo(got[0].EnergyWh, 2000) {
		t.Fatalf("member part energy = %+v, want srv_mem 2000 Wh, energy only", got)
	}
	got, err = st.MemberPartEnergy(ctx, q, []string{"srv_mem"}, "server")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Key != "Head" || !closeTo(got[0].EnergyWh, 2000) {
		t.Fatalf("member part energy by server = %+v, want under the request's group value", got)
	}
	// A request the server filter selects already counts in full.
	selectsHead := q
	selectsHead.ServerExact, selectsHead.HasServerExact = "Head", true
	got, err = st.MemberPartEnergy(ctx, selectsHead, []string{"srv_mem"}, "")
	if err != nil || len(got) != 0 {
		t.Fatalf("member part energy with the head's request selected = %+v err=%v, want none", got, err)
	}
	if got, err := st.MemberPartEnergy(ctx, q, nil, ""); err != nil || len(got) != 0 {
		t.Fatalf("member part energy of no hosts = %+v err=%v", got, err)
	}
}

// TestMemberPartEnergyExcludesByTheRequestFilter pins that MemberPartEnergy
// leaves out every request the server filter itself selects -- matched, like
// the base query, by the request's own server_name snapshot or host -- even
// when the serving server was renamed or deleted since, so a filtered
// aggregate never counts such a request's member part twice.
func TestMemberPartEnergyExcludesByTheRequestFilter(t *testing.T) {
	forEachDialect(t, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		now := time.Now().UTC().Truncate(time.Second)
		for _, e := range []usage.Event{
			// Served by srv_h while it was named gpu-a (renamed since).
			{ID: "ev_renamed", UserID: "u1", Host: "srv_h", ServerName: "gpu-a", EnergyWh: 3000, Status: "ok", HTTPStatus: 200, CreatedAt: now},
			// Served by a server deleted since.
			{ID: "ev_deleted", UserID: "u1", Host: "srv_gone", ServerName: "gpu-c", EnergyWh: 3000, Status: "ok", HTTPStatus: 200, CreatedAt: now},
		} {
			s.Record(e)
			if err := s.LastUsageError(); err != nil {
				t.Fatalf("record %s: %v", e.ID, err)
			}
		}
		for _, id := range []string{"ev_renamed", "ev_deleted"} {
			host := map[string]string{"ev_renamed": "srv_h", "ev_deleted": "srv_gone"}[id]
			if err := s.SetEnergyParts(ctx, id, []usage.EnergyPart{
				{ServerID: host, Role: usage.EventHostRoleHead, Wh: 1000, Source: "measured"},
				{ServerID: "srv_m", Role: usage.EventHostRoleMember, Wh: 2000, Source: "measured"},
			}); err != nil {
				t.Fatal(err)
			}
		}
		assertMemberPartEnergyExcludesByTheRequestFilter(t, s)
	})
}

// assertMemberPartEnergyExcludesByTheRequestFilter runs the expectations of
// TestMemberPartEnergyExcludesByTheRequestFilter on st.
func assertMemberPartEnergyExcludesByTheRequestFilter(t *testing.T, st usage.Store) {
	t.Helper()
	ctx := context.Background()
	// "gpu" selects both requests by their snapshot names; srv_m (gpu-b
	// today) is the only server the current names resolve.
	got, err := st.MemberPartEnergy(ctx, usage.Query{UserID: "u1", Server: "gpu"}, []string{"srv_m"}, "")
	if err != nil || len(got) != 0 {
		t.Fatalf("member parts under a filter that selects their requests = %+v err=%v, want none", got, err)
	}
	// Filtered by the member alone, neither request is selected: both parts count.
	got, err = st.MemberPartEnergy(ctx, usage.Query{UserID: "u1", ServerExact: "gpu-b", HasServerExact: true}, []string{"srv_m"}, "")
	if err != nil || len(got) != 1 || got[0].Host != "srv_m" || !closeTo(got[0].EnergyWh, 4000) {
		t.Fatalf("member parts filtered by the member = %+v err=%v, want srv_m 4000 Wh", got, err)
	}
}

// seedMemberPartFilterEvents records three cluster requests that each drew a
// part on srv_m: ev_a (host srv_h, gpu-a, session sess-1, at t0), ev_b (host
// srv_h2, gpu-b, sess-1, an hour later) and ev_c (host srv_h2, gpu-b, sess-2,
// at t0), with distinct member parts (2000, 4000, 8000 Wh).
func seedMemberPartFilterEvents(t *testing.T, st usage.Store, record func(usage.Event), t0 time.Time) {
	t.Helper()
	ctx := context.Background()
	for _, e := range []struct {
		ev     usage.Event
		member float64
	}{
		{usage.Event{ID: "ev_a", UserID: "u1", Host: "srv_h", ServerName: "gpu-a", SessionID: "sess-1", EnergyWh: 3000, CreatedAt: t0}, 2000},
		{usage.Event{ID: "ev_b", UserID: "u1", Host: "srv_h2", ServerName: "gpu-b", SessionID: "sess-1", EnergyWh: 5000, CreatedAt: t0.Add(time.Hour)}, 4000},
		{usage.Event{ID: "ev_c", UserID: "u1", Host: "srv_h2", ServerName: "gpu-b", SessionID: "sess-2", EnergyWh: 9000, CreatedAt: t0}, 8000},
	} {
		e.ev.Status, e.ev.HTTPStatus = "ok", 200
		record(e.ev)
		if err := st.SetEnergyParts(ctx, e.ev.ID, []usage.EnergyPart{
			{ServerID: e.ev.Host, Role: usage.EventHostRoleHead, Wh: 1000, Source: "measured"},
			{ServerID: "srv_m", Role: usage.EventHostRoleMember, Wh: e.member, Source: "measured"},
		}); err != nil {
			t.Fatal(err)
		}
	}
}

// assertMemberPartEnergyFilterCombinations pins MemberPartEnergy under a
// server filter combined with the filters usageWhere binds after the server
// predicates (SessionIDExact, From, To): a server filter matches a request by
// its host id as well as by its server name, and every other filter keeps
// selecting by its own value.
func assertMemberPartEnergyFilterCombinations(t *testing.T, st usage.Store, t0 time.Time) {
	t.Helper()
	ctx := context.Background()
	memberWh := func(q usage.Query) float64 {
		t.Helper()
		got, err := st.MemberPartEnergy(ctx, q, []string{"srv_m"}, "")
		if err != nil {
			t.Fatalf("member part energy %+v: %v", q, err)
		}
		sum := 0.0
		for _, b := range got {
			if b.Host != "srv_m" || !b.MemberParts {
				t.Fatalf("bucket %+v, want srv_m member parts only", b)
			}
			sum += b.EnergyWh
		}
		return sum
	}
	window := usage.Query{UserID: "u1", From: t0.Add(-time.Minute), To: t0.Add(30 * time.Minute)}

	// The window and the session select ev_a alone; the host-id filter
	// selects ev_b and ev_c, so ev_a's part counts.
	q := window
	q.Server, q.SessionIDExact, q.HasSessionIDExact = "srv_h2", "sess-1", true
	if got := memberWh(q); !closeTo(got, 2000) {
		t.Fatalf("host-id filter srv_h2 with session and window = %v Wh, want 2000 (ev_a)", got)
	}
	// "srv_h" matches ev_a by its host id (its name gpu-a does not), so the
	// one request the window and session select is left out.
	q.Server = "srv_h"
	if got := memberWh(q); !closeTo(got, 0) {
		t.Fatalf("host-id filter srv_h with session and window = %v Wh, want 0", got)
	}
	// The exact name filter selects ev_c, the only request of sess-2 in the window.
	q = window
	q.ServerExact, q.HasServerExact = "gpu-b", true
	q.SessionIDExact, q.HasSessionIDExact = "sess-2", true
	if got := memberWh(q); !closeTo(got, 0) {
		t.Fatalf("exact gpu-b with sess-2 = %v Wh, want 0", got)
	}
	// Widened to both hours and sess-1: ev_b is selected by the name, ev_a counts.
	q.To = t0.Add(2 * time.Hour)
	q.SessionIDExact = "sess-1"
	if got := memberWh(q); !closeTo(got, 2000) {
		t.Fatalf("exact gpu-b with sess-1 over two hours = %v Wh, want 2000 (ev_a)", got)
	}
	// Without the session filter the window alone: ev_a and ev_c in the first
	// half hour, ev_c selected by the name.
	q = window
	q.ServerExact, q.HasServerExact = "gpu-b", true
	if got := memberWh(q); !closeTo(got, 2000) {
		t.Fatalf("exact gpu-b in the window = %v Wh, want 2000 (ev_a)", got)
	}
}

func TestMemberPartEnergyFilterCombinationsMemory(t *testing.T) {
	t0 := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	rec := usage.NewRecorder()
	seedMemberPartFilterEvents(t, rec, func(e usage.Event) { rec.Record(e) }, t0)
	assertMemberPartEnergyFilterCombinations(t, rec, t0)
}

func TestMemberPartEnergyFilterCombinationsSQL(t *testing.T) {
	t0 := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	forEachDialect(t, func(t *testing.T, s *SQLStore) {
		seedMemberPartFilterEvents(t, s, func(e usage.Event) {
			s.Record(e)
			if err := s.LastUsageError(); err != nil {
				t.Fatalf("record %s: %v", e.ID, err)
			}
		}, t0)
		assertMemberPartEnergyFilterCombinations(t, s, t0)
	})
}
