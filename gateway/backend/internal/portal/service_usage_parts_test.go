// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"encoding/json"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/store"
	"op-ai-gateway/internal/usage"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestClusterCostUsesEachHostPrice pins that a cluster request's cost is the
// sum of its energy parts, each at its own host's price: the head part at the
// head server's price_per_kwh, each member part at that member server's own
// (else the system default). The row, the stats total and every group agree.
func TestClusterCostUsesEachHostPrice(t *testing.T) {
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()

	routes := routing.NewMemoryStore()
	for _, s := range []routing.AIServer{
		{ID: "srv_head", Name: "Head", Domain: "head.example.test", PricePerKwh: 0.30},
		{ID: "srv_mem", Name: "Member", Domain: "mem.example.test", PricePerKwh: 0.10},
		{ID: "srv_mem2", Name: "Member 2", Domain: "mem2.example.test"}, // system default
	} {
		if err := routes.CreateAIServer(ctx, s); err != nil {
			t.Fatalf("CreateAIServer %s: %v", s.ID, err)
		}
	}
	settings := NewMemorySystemSettings()
	if err := settings.SetSystemSetting(ctx, "energy_default_price_per_kwh", "0.50", now); err != nil {
		t.Fatal(err)
	}

	rec := usage.NewRecorder()
	rec.Record(usage.Event{ID: "ev_cluster", UserID: "usr_1", Model: "big", Host: "srv_head", ServerName: "Head", EnergyWh: 3500, EnergySource: "measured", CreatedAt: now})
	rec.Record(usage.Event{ID: "ev_plain", UserID: "usr_1", Model: "big", Host: "srv_head", ServerName: "Head", EnergyWh: 1000, EnergySource: "measured", CreatedAt: now})
	if err := rec.SetEnergyParts(ctx, "ev_cluster", []usage.EnergyPart{
		{ServerID: "srv_head", Role: usage.EventHostRoleHead, Wh: 1000, Source: "measured"},
		{ServerID: "srv_mem", Role: usage.EventHostRoleMember, Wh: 2000, Source: "measured"},
		{ServerID: "srv_mem2", Role: usage.EventHostRoleMember, Wh: 500, Source: "measured"},
	}); err != nil {
		t.Fatal(err)
	}
	svc := NewService(ServiceDeps{Usage: rec, Routes: routes, SystemSettings: settings, Clock: func() time.Time { return now }})
	user := auth.Token{UserID: "usr_1", Scopes: []string{"gateway:use"}}

	// head 1 kWh × 0.30 + member 2 kWh × 0.10 + member 0.5 kWh × 0.50 (default).
	const clusterCost = 0.30 + 0.20 + 0.25
	const plainCost = 0.30

	page, err := svc.Usage(user, usage.Query{Page: 1, Limit: 25})
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range page.Data {
		want := plainCost
		if row.ID == "ev_cluster" {
			want = clusterCost
		}
		if !approxEq(row.CostEUR, want) {
			t.Fatalf("%s cost = %v, want %v", row.ID, row.CostEUR, want)
		}
	}

	stats, err := svc.UsageStats(user, usage.Query{})
	if err != nil {
		t.Fatal(err)
	}
	if !approxEq(stats.Totals.TotalCostEUR, clusterCost+plainCost) || !approxEq(stats.Totals.TotalEnergyWh, 4500) {
		t.Fatalf("totals = %+v, want cost %v over 4500 Wh", stats.Totals, clusterCost+plainCost)
	}

	// Grouped by server, every host's share shows under that server, at its
	// price; a member row counts no requests.
	byServer, err := svc.UsageGroups(user, usage.Query{}, "server")
	if err != nil {
		t.Fatal(err)
	}
	wantServers := map[string]UsageGroupDTO{
		"Head":     {Count: 2, EnergyWh: 2000, CostEUR: 0.60},
		"Member":   {Count: 0, EnergyWh: 2000, CostEUR: 0.20},
		"Member 2": {Count: 0, EnergyWh: 500, CostEUR: 0.25},
	}
	if len(byServer) != len(wantServers) {
		t.Fatalf("groups by server = %+v, want %d rows", byServer, len(wantServers))
	}
	for _, g := range byServer {
		want, ok := wantServers[g.Key]
		if !ok || g.Count != want.Count || !approxEq(g.EnergyWh, want.EnergyWh) || !approxEq(g.CostEUR, want.CostEUR) {
			t.Fatalf("group %q = %+v, want %+v", g.Key, g, want)
		}
	}

	for _, dim := range []string{"model", "user"} {
		groups, err := svc.UsageGroups(user, usage.Query{}, dim)
		if err != nil {
			t.Fatalf("groups by %s: %v", dim, err)
		}
		if len(groups) != 1 {
			t.Fatalf("groups by %s = %+v, want one", dim, groups)
		}
		g := groups[0]
		if g.Count != 2 || !approxEq(g.EnergyWh, 4500) || !approxEq(g.CostEUR, clusterCost+plainCost) {
			t.Fatalf("group by %s = %+v, want 2 requests, 4500 Wh, cost %v", dim, g, clusterCost+plainCost)
		}
	}
}

// TestMemberServerFilterShowsAggregatedShare pins the member server's view: a
// usage aggregate filtered by a server includes the energy that server drew as
// a member of other servers' cluster requests, at its own price -- energy and
// cost only, no request, count or token of those requests -- while the
// request list stays the requests served by that server.
func TestMemberServerFilterShowsAggregatedShare(t *testing.T) {
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	routes := routing.NewMemoryStore()
	for _, s := range []routing.AIServer{
		{ID: "srv_head", Name: "Head", Domain: "head.example.test", PricePerKwh: 0.30},
		{ID: "srv_mem", Name: "Member", Domain: "mem.example.test", PricePerKwh: 0.10},
	} {
		if err := routes.CreateAIServer(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	rec := usage.NewRecorder()
	// R: another user's cluster request, served by Head with a part on Member.
	rec.Record(usage.Event{ID: "ev_r", UserID: "usr_other", Model: "big", Host: "srv_head", ServerName: "Head", InputTokens: 40, OutputTokens: 60, TotalTokens: 100, EnergyWh: 3000, EnergySource: "measured", CreatedAt: now})
	if err := rec.SetEnergyParts(ctx, "ev_r", []usage.EnergyPart{
		{ServerID: "srv_head", Role: usage.EventHostRoleHead, Wh: 1000, Source: "measured"},
		{ServerID: "srv_mem", Role: usage.EventHostRoleMember, Wh: 2000, Source: "measured"},
	}); err != nil {
		t.Fatal(err)
	}
	svc := NewService(ServiceDeps{Usage: rec, Routes: routes, Clock: func() time.Time { return now }})
	admin := auth.Token{UserID: "usr_admin", Scopes: []string{"gateway:use", "admin"}}

	for _, q := range []usage.Query{
		{ScopeAll: true, ServerExact: "Member", HasServerExact: true},
		{ScopeAll: true, Server: "mem"},
	} {
		stats, err := svc.UsageStats(admin, q)
		if err != nil {
			t.Fatal(err)
		}
		tot := stats.Totals
		if tot.TotalRequests != 0 || tot.InputTokens != 0 || tot.OutputTokens != 0 || !approxEq(tot.TotalEnergyWh, 2000) || !approxEq(tot.TotalCostEUR, 0.20) {
			t.Fatalf("stats filtered by %+v = %+v, want no request or token, 2000 Wh at 0.20", q, tot)
		}
		groups, err := svc.UsageGroups(admin, q, "server")
		if err != nil {
			t.Fatal(err)
		}
		if len(groups) != 1 || groups[0].Key != "Member" || groups[0].Count != 0 || groups[0].TotalTokens != 0 ||
			!approxEq(groups[0].EnergyWh, 2000) || !approxEq(groups[0].CostEUR, 0.20) {
			t.Fatalf("groups filtered by %+v = %+v, want Member's share only", q, groups)
		}
		page, err := svc.Usage(admin, usage.Query{ScopeAll: true, ServerExact: q.ServerExact, HasServerExact: q.HasServerExact, Server: q.Server, Page: 1, Limit: 25})
		if err != nil {
			t.Fatal(err)
		}
		if page.Total != 0 {
			t.Fatalf("rows filtered by %+v = %+v, want none: R was not served by Member", q, page.Data)
		}
	}

	// Filtered by the head, the request counts in full, as before.
	stats, err := svc.UsageStats(admin, usage.Query{ScopeAll: true, ServerExact: "Head", HasServerExact: true})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Totals.TotalRequests != 1 || !approxEq(stats.Totals.TotalEnergyWh, 3000) || !approxEq(stats.Totals.TotalCostEUR, 0.50) {
		t.Fatalf("stats filtered by Head = %+v, want R in full: 3000 Wh, 0.30 + 0.20", stats.Totals)
	}
}

// TestServerFilterNeverCountsAMemberPartTwice pins the regression: a request
// the server filter selects by its own (snapshot) server name counts in full,
// its member parts included, even when its server was renamed or deleted
// since -- the member's share is not added a second time because the member
// also matches the filter by its current name.
func TestServerFilterNeverCountsAMemberPartTwice(t *testing.T) {
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name    string
		servers []routing.AIServer
	}{
		{"head renamed", []routing.AIServer{
			{ID: "srv_h", Name: "x-renamed", Domain: "h.example.test", PricePerKwh: 0.30},
			{ID: "srv_m", Name: "gpu-b", Domain: "m.example.test", PricePerKwh: 0.10},
		}},
		{"head deleted", []routing.AIServer{
			{ID: "srv_m", Name: "gpu-b", Domain: "m.example.test", PricePerKwh: 0.10},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			routes := routing.NewMemoryStore()
			for _, s := range tc.servers {
				if err := routes.CreateAIServer(ctx, s); err != nil {
					t.Fatal(err)
				}
			}
			settings := NewMemorySystemSettings()
			if err := settings.SetSystemSetting(ctx, "energy_default_price_per_kwh", "0.30", now); err != nil {
				t.Fatal(err)
			}
			rec := usage.NewRecorder()
			rec.Record(usage.Event{ID: "ev_r", UserID: "usr_1", Host: "srv_h", ServerName: "gpu-a", TotalTokens: 10, EnergyWh: 3000, EnergySource: "measured", CreatedAt: now})
			if err := rec.SetEnergyParts(ctx, "ev_r", []usage.EnergyPart{
				{ServerID: "srv_h", Role: usage.EventHostRoleHead, Wh: 1000, Source: "measured"},
				{ServerID: "srv_m", Role: usage.EventHostRoleMember, Wh: 2000, Source: "measured"},
			}); err != nil {
				t.Fatal(err)
			}
			svc := NewService(ServiceDeps{Usage: rec, Routes: routes, SystemSettings: settings, Clock: func() time.Time { return now }})
			user := auth.Token{UserID: "usr_1", Scopes: []string{"gateway:use"}}
			q := usage.Query{Server: "gpu"}

			stats, err := svc.UsageStats(user, q)
			if err != nil {
				t.Fatal(err)
			}
			// head 1 kWh × 0.30 + member 2 kWh × 0.10, once.
			if stats.Totals.TotalRequests != 1 || !approxEq(stats.Totals.TotalEnergyWh, 3000) || !approxEq(stats.Totals.TotalCostEUR, 0.50) {
				t.Fatalf("stats = %+v, want R once: 3000 Wh, 0.50", stats.Totals)
			}
			groups, err := svc.UsageGroups(user, q, "server")
			if err != nil {
				t.Fatal(err)
			}
			want := map[string]float64{"gpu-a": 1000, "gpu-b": 2000}
			if len(groups) != len(want) {
				t.Fatalf("groups = %+v, want %v", groups, want)
			}
			for _, g := range groups {
				if wh, ok := want[g.Key]; !ok || !approxEq(g.EnergyWh, wh) {
					t.Fatalf("group %q = %+v, want %v Wh", g.Key, g, want[g.Key])
				}
			}
		})
	}
}

// countingPartsStore counts the energy-part reads a usage list makes.
type countingPartsStore struct {
	usage.Store
	mu          sync.Mutex
	batched     int
	singleReads int
}

func (c *countingPartsStore) EnergyPartsByEvents(ctx context.Context, ids []string) (map[string][]usage.EnergyPart, error) {
	c.mu.Lock()
	c.batched++
	c.mu.Unlock()
	return c.Store.EnergyPartsByEvents(ctx, ids)
}

func (c *countingPartsStore) EnergyParts(ctx context.Context, id string) ([]usage.EnergyPart, error) {
	c.mu.Lock()
	c.singleReads++
	c.mu.Unlock()
	return c.Store.EnergyParts(ctx, id)
}

// TestUsageRowsCarryVisibleEnergyParts pins the per-host energy a cluster
// request's Activity row carries: one part per host, read for the whole page
// in one call. A member server the caller may not see (the server list's
// rule) is an anonymous numbered member -- no name, no id -- with its energy,
// source and cost like every other part (the row's own cost contains its
// price); the head is named as the row itself names it. A single-host row
// carries no parts at all.
func TestUsageRowsCarryVisibleEnergyParts(t *testing.T) {
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	routes := routing.NewMemoryStore()
	for _, s := range []routing.AIServer{
		{ID: "srv_head", Name: "Head", Domain: "head.example.test", PricePerKwh: 0.30},
		{ID: "srv_hidden", Name: "Hidden box", Domain: "hidden.example.test", PricePerKwh: 0.20},
		{ID: "srv_mine", Name: "Mine", Domain: "mine.example.test", PricePerKwh: 0.10},
	} {
		if err := routes.CreateAIServer(ctx, s); err != nil {
			t.Fatalf("CreateAIServer %s: %v", s.ID, err)
		}
	}
	if err := routes.SetServerOwners(ctx, "srv_mine", []string{"usr_1"}); err != nil {
		t.Fatal(err)
	}
	rec := usage.NewRecorder()
	rec.Record(usage.Event{ID: "ev_cluster", UserID: "usr_1", Model: "big", Host: "srv_head", ServerName: "Head", EnergyWh: 3500, EnergySource: "estimated", CreatedAt: now})
	rec.Record(usage.Event{ID: "ev_plain", UserID: "usr_1", Model: "big", Host: "srv_head", ServerName: "Head", EnergyWh: 1000, EnergySource: "measured", CreatedAt: now.Add(-time.Minute)})
	if err := rec.SetEnergyParts(ctx, "ev_cluster", []usage.EnergyPart{
		{ServerID: "srv_head", Role: usage.EventHostRoleHead, Wh: 1000, MarginalWh: 600, Source: "measured"},
		{ServerID: "srv_hidden", Role: usage.EventHostRoleMember, Wh: 500, MarginalWh: 300, Source: "estimated"},
		{ServerID: "srv_mine", Role: usage.EventHostRoleMember, Wh: 2000, MarginalWh: 2000, Source: "measured"},
	}); err != nil {
		t.Fatal(err)
	}
	store := &countingPartsStore{Store: rec}
	svc := NewService(ServiceDeps{Usage: store, Routes: routes, SystemSettings: NewMemorySystemSettings(), Clock: func() time.Time { return now }})

	cost := func(v float64) *float64 { return &v }
	user := auth.Token{UserID: "usr_1", Scopes: []string{"gateway:use"}}
	page, err := svc.Usage(user, usage.Query{Page: 1, Limit: 25})
	if err != nil {
		t.Fatal(err)
	}
	if store.batched != 1 || store.singleReads != 0 {
		t.Fatalf("part reads = %d batched, %d single; want one batched read for the page", store.batched, store.singleReads)
	}
	rows := map[string]usage.Row{}
	for _, r := range page.Data {
		rows[r.ID] = r
	}
	want := []usage.RowEnergyPart{
		{Role: usage.EventHostRoleHead, ServerName: "Head", EnergyWh: 1000, MarginalWh: 600, EnergySource: "measured", Cost: cost(0.30)},
		{Role: usage.EventHostRoleMember, Member: 1, EnergyWh: 500, MarginalWh: 300, EnergySource: "estimated", Cost: cost(0.10)},
		{Role: usage.EventHostRoleMember, Member: 2, ServerName: "Mine", EnergyWh: 2000, MarginalWh: 2000, EnergySource: "measured", Cost: cost(0.20)},
	}
	if got := rows["ev_cluster"].EnergyParts; !reflect.DeepEqual(got, want) {
		t.Fatalf("parts = %+v, want %+v", got, want)
	}
	raw, err := json.Marshal(rows["ev_cluster"])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"marginal_wh":300`) {
		t.Fatalf("row JSON = %s, want each part's marginal_wh", raw)
	}
	for _, leak := range []string{"srv_hidden", "Hidden box", "srv_mine"} {
		if strings.Contains(string(raw), leak) {
			t.Fatalf("row JSON leaks %q: %s", leak, raw)
		}
	}
	plain, err := json.Marshal(rows["ev_plain"])
	if err != nil {
		t.Fatal(err)
	}
	if rows["ev_plain"].EnergyParts != nil || strings.Contains(string(plain), "energy_parts") {
		t.Fatalf("single-host row = %s, want no energy_parts", plain)
	}

	// System scope sees every server.
	sys := auth.Token{UserID: "usr_sys", Scopes: []string{"gateway:use", "admin", "system"}}
	page, err = svc.Usage(sys, usage.Query{Page: 1, Limit: 25, ScopeAll: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range page.Data {
		if r.ID == "ev_cluster" && (r.EnergyParts[1].ServerName != "Hidden box" || r.EnergyParts[1].Cost == nil) {
			t.Fatalf("system view part = %+v, want the hidden server named with its cost", r.EnergyParts[1])
		}
	}
}

// countingServerRoutes counts, per server, the reads authorizeServer and the
// price lookup make.
type countingServerRoutes struct {
	*routing.MemoryStore
	mu     sync.Mutex
	byID   map[string]int
	owners map[string]int
	groups map[string]int
}

func newCountingServerRoutes() *countingServerRoutes {
	return &countingServerRoutes{MemoryStore: routing.NewMemoryStore(), byID: map[string]int{}, owners: map[string]int{}, groups: map[string]int{}}
}

func (c *countingServerRoutes) count(m map[string]int, id string) {
	c.mu.Lock()
	m[id]++
	c.mu.Unlock()
}

func (c *countingServerRoutes) reset() {
	c.mu.Lock()
	c.byID, c.owners, c.groups = map[string]int{}, map[string]int{}, map[string]int{}
	c.mu.Unlock()
}

func (c *countingServerRoutes) AIServerByID(ctx context.Context, id string) (routing.AIServer, error) {
	c.count(c.byID, id)
	return c.MemoryStore.AIServerByID(ctx, id)
}

func (c *countingServerRoutes) ServerOwners(ctx context.Context, id string) ([]string, error) {
	c.count(c.owners, id)
	return c.MemoryStore.ServerOwners(ctx, id)
}

func (c *countingServerRoutes) ServerAdminGroups(ctx context.Context, id string) ([]string, error) {
	c.count(c.groups, id)
	return c.MemoryStore.ServerAdminGroups(ctx, id)
}

// TestUsageEnergyPartsResolveEachServerOnce pins the cost of the parts on a
// page whose cluster rows share their hosts: each member server's visibility
// is resolved once for the page however many rows list it -- one owner read
// and one admin-group read per member server, one server read for its price
// and one for the visibility check -- and the rule is the server list's. A
// plain admin who manages no admin group of a server sees it as an anonymous
// member (the ErrServerNotFound of authorizeServer); a co-manager with
// can_manage_servers on the server's admin group sees its name. Every part
// carries its cost either way.
func TestUsageEnergyPartsResolveEachServerOnce(t *testing.T) {
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	routes := newCountingServerRoutes()
	dir := NewMemoryDirectory(auth.NewTokenStore())
	rec := usage.NewRecorder()
	svc := NewService(ServiceDeps{Users: dir, Groups: dir, Routes: routes, Usage: rec, SystemSettings: NewMemorySystemSettings(), Clock: func() time.Time { return now }})
	e := &groupTestEnv{t: t, ctx: context.Background(), dir: dir, svc: svc, now: now, routes: routes.MemoryStore}
	e.createUser("usr_s", "system_admin")
	e.createUser("usr_admin", "admin")
	e.createUser("usr_mgr", "admin")
	sysAdmin := token("usr_s", "system", "admin")
	sg := e.mustCreateGroup(sysAdmin, CreateGroupInput{Tier: store.GroupTierSystem, Name: "SG"})
	e.mustAddMembers(sysAdmin, sg.ID, "usr_admin", "usr_mgr")
	ag := e.mustCreateGroup(sysAdmin, CreateGroupInput{Tier: store.GroupTierAdmin, Name: "AG", ParentGroupID: sg.ID})
	e.mustAddMembers(sysAdmin, ag.ID, "usr_mgr")
	if err := svc.PromoteManager(e.ctx, sysAdmin, ag.ID, "usr_mgr", ManagerPermissions{CanManageServers: true}); err != nil {
		t.Fatal(err)
	}
	for _, srv := range []routing.AIServer{
		{ID: "srv_head", Name: "Head", Domain: "head.example.test", PricePerKwh: 0.30},
		{ID: "srv_hidden", Name: "Hidden box", Domain: "hidden.example.test", PricePerKwh: 0.20},
		{ID: "srv_shared", Name: "Shared", Domain: "shared.example.test", PricePerKwh: 0.10},
	} {
		if err := routes.CreateAIServer(e.ctx, srv); err != nil {
			t.Fatal(err)
		}
	}
	if err := routes.SetServerAdminGroup(e.ctx, "srv_shared", ag.ID); err != nil {
		t.Fatal(err)
	}
	for i, id := range []string{"ev_a", "ev_b", "ev_c"} {
		rec.Record(usage.Event{ID: id, UserID: "usr_req", Model: "big", Host: "srv_head", ServerName: "Head", EnergyWh: 3, EnergySource: "measured", CreatedAt: now.Add(-time.Duration(i) * time.Minute)})
		if err := rec.SetEnergyParts(e.ctx, id, []usage.EnergyPart{
			{ServerID: "srv_head", Role: usage.EventHostRoleHead, Wh: 1000, Source: "measured"},
			{ServerID: "srv_hidden", Role: usage.EventHostRoleMember, Wh: 1000, Source: "measured"},
			{ServerID: "srv_shared", Role: usage.EventHostRoleMember, Wh: 1000, Source: "measured"},
		}); err != nil {
			t.Fatal(err)
		}
	}

	for _, tc := range []struct {
		name       string
		viewer     auth.Token
		sharedName string
	}{
		{"plain admin", token("usr_admin", "admin"), ""},
		{"group manager", token("usr_mgr", "admin"), "Shared"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			routes.reset()
			page, err := svc.Usage(tc.viewer, usage.Query{Page: 1, Limit: 25, ScopeAll: true})
			if err != nil {
				t.Fatal(err)
			}
			if len(page.Data) != 3 {
				t.Fatalf("rows = %d, want the three cluster rows", len(page.Data))
			}
			for _, row := range page.Data {
				parts := row.EnergyParts
				if len(parts) != 3 || parts[1].ServerName != "" || parts[1].Member != 1 || parts[2].ServerName != tc.sharedName || parts[2].Member != 2 {
					t.Fatalf("%s parts = %+v, want the hidden member anonymous and the shared one named %q", row.ID, parts, tc.sharedName)
				}
				for _, p := range parts {
					if p.Cost == nil {
						t.Fatalf("%s part %+v has no cost", row.ID, p)
					}
				}
			}
			for _, srv := range []string{"srv_hidden", "srv_shared"} {
				if routes.owners[srv] > 1 || routes.groups[srv] > 1 || routes.byID[srv] > 2 {
					t.Fatalf("%s reads: %d owner, %d admin-group, %d server; want each resolved once for the page", srv, routes.owners[srv], routes.groups[srv], routes.byID[srv])
				}
			}
		})
	}
}
