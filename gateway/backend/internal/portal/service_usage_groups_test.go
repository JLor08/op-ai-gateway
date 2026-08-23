// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"errors"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/store"
	"op-ai-gateway/internal/usage"
	"testing"
	"time"
)

// TestUsageGroupsRejectsUnknownDimension proves the whitelist gate fires BEFORE
// any store call, returning ErrUsageGroupByInvalid (→ HTTP 400).
func TestUsageGroupsRejectsUnknownDimension(t *testing.T) {
	svc := NewService(ServiceDeps{Usage: usage.NewRecorder()})
	_, err := svc.UsageGroups(auth.Token{UserID: "usr_1", Scopes: []string{"gateway:use"}}, usage.Query{}, "bogus")
	if !errors.Is(err, ErrUsageGroupByInvalid) {
		t.Fatalf("err = %v, want ErrUsageGroupByInvalid", err)
	}
}

// TestUsageGroupsFoldsCostPerServerPrice proves a single session spanning TWO
// servers at DIFFERENT prices folds into ONE group whose CostEUR is the exact
// per-server-price-weighted sum — not a blended rate. srv_a: 1000Wh @ 0.30 =
// 0.30; srv_b: 100Wh @ 0.50 = 0.05; total 0.35. Also checks the summed
// token/energy/count fold and TotalTokens.
func TestUsageGroupsFoldsCostPerServerPrice(t *testing.T) {
	now := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)
	earlier := now.Add(-time.Hour)
	ctx := context.Background()

	routes := routing.NewMemoryStore()
	if err := routes.CreateAIServer(ctx, routing.AIServer{ID: "srv_a", Name: "A", Domain: "a.example.test", PricePerKwh: 0.30}); err != nil {
		t.Fatalf("CreateAIServer a: %v", err)
	}
	if err := routes.CreateAIServer(ctx, routing.AIServer{ID: "srv_b", Name: "B", Domain: "b.example.test", PricePerKwh: 0.50}); err != nil {
		t.Fatalf("CreateAIServer b: %v", err)
	}

	rec := usage.NewRecorder()
	rec.Record(usage.Event{ID: "e1", UserID: "usr_1", SessionID: "sess_x", Host: "srv_a", InputTokens: 10, OutputTokens: 5, EnergyWh: 1000, CreatedAt: earlier})
	rec.Record(usage.Event{ID: "e2", UserID: "usr_1", SessionID: "sess_x", Host: "srv_b", InputTokens: 3, OutputTokens: 2, EnergyWh: 100, CreatedAt: now})

	svc := NewService(ServiceDeps{Usage: rec, Routes: routes, Clock: func() time.Time { return now }})

	groups, err := svc.UsageGroups(auth.Token{UserID: "usr_1", Scopes: []string{"gateway:use"}}, usage.Query{}, "session")
	if err != nil {
		t.Fatalf("UsageGroups: %v", err)
	}
	if len(groups) != 1 {
		t.Fatalf("groups = %d, want 1", len(groups))
	}
	g := groups[0]
	if g.Key != "sess_x" {
		t.Fatalf("Key = %q, want sess_x", g.Key)
	}
	if g.Count != 2 {
		t.Fatalf("Count = %d, want 2", g.Count)
	}
	// 1000Wh/1000*0.30 + 100Wh/1000*0.50 = 0.30 + 0.05 = 0.35 (per-server weighted).
	if !approxEq(g.CostEUR, 0.35) {
		t.Fatalf("CostEUR = %v, want 0.35 (per-server weighted, not blended)", g.CostEUR)
	}
	if !approxEq(g.EnergyWh, 1100) {
		t.Fatalf("EnergyWh = %v, want 1100 (summed)", g.EnergyWh)
	}
	if g.InputTokens != 13 || g.OutputTokens != 7 {
		t.Fatalf("tokens = %d/%d, want 13/7", g.InputTokens, g.OutputTokens)
	}
	if g.TotalTokens != 20 {
		t.Fatalf("TotalTokens = %d, want 20 (input+output+cached+cache_write)", g.TotalTokens)
	}
	// FirstAt/LastAt span both buckets (earliest first, latest last).
	if g.FirstAt != earlier.UTC().Format(time.RFC3339) {
		t.Fatalf("FirstAt = %q, want %q", g.FirstAt, earlier.UTC().Format(time.RFC3339))
	}
	if g.LastAt != now.UTC().Format(time.RFC3339) {
		t.Fatalf("LastAt = %q, want %q", g.LastAt, now.UTC().Format(time.RFC3339))
	}
}

// TestUsageGroupsScopedToOwnUser proves a non-admin principal only ever sees
// their own rows — applyUsageScope pins UserID and forces ScopeAll=false even
// when the query asks for all — so another user's group never leaks in.
func TestUsageGroupsScopedToOwnUser(t *testing.T) {
	now := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)
	rec := usage.NewRecorder()
	rec.Record(usage.Event{ID: "mine", UserID: "usr_1", ServerName: "srv-mine", Host: "srv_1", CreatedAt: now})
	rec.Record(usage.Event{ID: "theirs", UserID: "usr_2", ServerName: "srv-theirs", Host: "srv_2", CreatedAt: now})

	svc := NewService(ServiceDeps{Usage: rec, Clock: func() time.Time { return now }})

	// Non-admin asks for scope=all; applyUsageScope must ignore it.
	groups, err := svc.UsageGroups(
		auth.Token{UserID: "usr_1", Scopes: []string{"gateway:use"}},
		usage.Query{ScopeAll: true},
		"server",
	)
	if err != nil {
		t.Fatalf("UsageGroups: %v", err)
	}
	if len(groups) != 1 {
		t.Fatalf("groups = %d, want 1 (own rows only)", len(groups))
	}
	if groups[0].Key != "srv-mine" {
		t.Fatalf("Key = %q, want srv-mine (usr_2's rows must not leak)", groups[0].Key)
	}
}

type stubUsers struct{ byID map[string]store.User }

func (s stubUsers) UserByID(_ context.Context, id string) (store.User, error) {
	u, ok := s.byID[id]
	if !ok {
		return store.User{}, errors.New("not found")
	}
	return u, nil
}

func (s stubUsers) ListUsers(_ context.Context) ([]store.User, error) {
	out := make([]store.User, 0, len(s.byID))
	for _, u := range s.byID {
		out = append(out, u)
	}
	return out, nil
}

// TestUsageGroupsUserLabelResolution proves group-by-user resolves the display
// name (email fallback) via the user reader, while an unresolvable id and every
// non-user dimension echo the raw key.
func TestUsageGroupsUserLabelResolution(t *testing.T) {
	now := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)
	rec := usage.NewRecorder()
	rec.Record(usage.Event{ID: "a", UserID: "usr_named", CreatedAt: now})
	rec.Record(usage.Event{ID: "b", UserID: "usr_emailonly", CreatedAt: now})
	rec.Record(usage.Event{ID: "c", UserID: "usr_ghost", CreatedAt: now})

	users := stubUsers{byID: map[string]store.User{
		"usr_named":     {ID: "usr_named", DisplayName: "Named User", Email: "named@example.test"},
		"usr_emailonly": {ID: "usr_emailonly", Email: "email@example.test"},
	}}
	svc := NewService(ServiceDeps{Usage: rec, Users: users, Clock: func() time.Time { return now }})

	// Admin scope=all so all three users' rows are visible.
	groups, err := svc.UsageGroups(
		auth.Token{UserID: "usr_admin", Scopes: []string{"gateway:use", "admin"}},
		usage.Query{ScopeAll: true},
		"user",
	)
	if err != nil {
		t.Fatalf("UsageGroups: %v", err)
	}
	labels := map[string]string{}
	for _, g := range groups {
		labels[g.Key] = g.KeyLabel
	}
	if labels["usr_named"] != "Named User" {
		t.Fatalf("KeyLabel(usr_named) = %q, want display name", labels["usr_named"])
	}
	if labels["usr_emailonly"] != "email@example.test" {
		t.Fatalf("KeyLabel(usr_emailonly) = %q, want email fallback", labels["usr_emailonly"])
	}
	if labels["usr_ghost"] != "usr_ghost" {
		t.Fatalf("KeyLabel(usr_ghost) = %q, want raw id fallback", labels["usr_ghost"])
	}
}

// TestUsageGroupsServiceDimensionBucketsByServiceIDAndLabelsByName proves the
// "service" group-by dimension (service accounts, Phase 1) folds
// usage_events.service_id and resolves the display label via the CURRENT
// routing.Service.Name (a live lookup, mirroring group-by-user's UserByID
// resolution) rather than the request-time denormalized service_name — so a
// service rename is reflected immediately, exactly like a user display-name
// change. An unresolvable service id and a plain user-token row (service_id=="")
// both fall back to echoing the raw key, consistent with every other dimension.
func TestUsageGroupsServiceDimensionBucketsByServiceIDAndLabelsByName(t *testing.T) {
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()

	routes := routing.NewMemoryStore()
	if err := routes.CreateService(ctx, routing.Service{
		ID: "svc_1", Name: "Nightly Batch", Status: routing.ServerStatusActive,
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create service: %v", err)
	}

	rec := usage.NewRecorder()
	// Two rows attributed to svc_1; the denormalized ServiceName is intentionally
	// STALE ("(old name)") so the label test proves live resolution, not an echo.
	rec.Record(usage.Event{ID: "e1", ServiceID: "svc_1", ServiceName: "Nightly Batch (old name)", Host: "srv_1", InputTokens: 10, OutputTokens: 5, EnergyWh: 100, CreatedAt: now})
	rec.Record(usage.Event{ID: "e2", ServiceID: "svc_1", ServiceName: "Nightly Batch (old name)", Host: "srv_1", InputTokens: 3, OutputTokens: 2, EnergyWh: 50, CreatedAt: now.Add(time.Minute)})
	// A service id with no matching row in the store falls back to the raw id.
	rec.Record(usage.Event{ID: "e3", ServiceID: "svc_ghost", Host: "srv_1", InputTokens: 1, CreatedAt: now})
	// A plain user-token row (no service attribution) must fold into its own
	// empty-key group, never mixed into a service's bucket.
	rec.Record(usage.Event{ID: "e4", UserID: "usr_1", Host: "srv_1", InputTokens: 999, CreatedAt: now})

	svc := NewService(ServiceDeps{Usage: rec, Routes: routes, Clock: func() time.Time { return now }})

	groups, err := svc.UsageGroups(
		auth.Token{UserID: "usr_admin", Scopes: []string{"gateway:use", "admin"}},
		usage.Query{ScopeAll: true},
		"service",
	)
	if err != nil {
		t.Fatalf("UsageGroups(service): %v", err)
	}
	byKey := map[string]UsageGroupDTO{}
	for _, g := range groups {
		byKey[g.Key] = g
	}

	svc1 := byKey["svc_1"]
	if svc1.Count != 2 || svc1.InputTokens != 13 || svc1.OutputTokens != 7 {
		t.Fatalf("svc_1 bucket = %+v, want count=2 input=13 output=7", svc1)
	}
	if svc1.KeyLabel != "Nightly Batch" {
		t.Fatalf("svc_1 KeyLabel = %q, want live-resolved %q (not the stale denormalized name)", svc1.KeyLabel, "Nightly Batch")
	}

	ghost := byKey["svc_ghost"]
	if ghost.Count != 1 || ghost.KeyLabel != "svc_ghost" {
		t.Fatalf("svc_ghost bucket = %+v, want count=1 label=svc_ghost (raw-id fallback)", ghost)
	}

	empty, ok := byKey[""]
	if !ok || empty.InputTokens != 999 {
		t.Fatalf("expected an empty-key group for the non-service (user-token) row e4 with 999 input tokens, got ok=%v %+v", ok, empty)
	}
}

// TestUsageGroupsServiceDimensionScopedAwayFromNonAdmin proves the §10 v1
// attribution decision end-to-end: service usage carries user_id="" (owned by
// nobody), so applyUsageScope's own-scope pin (user_id==principal) means a
// NON-ADMIN grouping by "service" never sees service-attributed rows — even
// though the whitelist accepts the dimension for them. This is not a new
// mechanism; it falls out of the existing scope pin unchanged.
func TestUsageGroupsServiceDimensionScopedAwayFromNonAdmin(t *testing.T) {
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	rec := usage.NewRecorder()
	rec.Record(usage.Event{ID: "svc_row", ServiceID: "svc_1", ServiceName: "Nightly Batch", CreatedAt: now})
	rec.Record(usage.Event{ID: "mine", UserID: "usr_1", ServiceID: "", CreatedAt: now})

	svc := NewService(ServiceDeps{Usage: rec, Clock: func() time.Time { return now }})

	groups, err := svc.UsageGroups(
		auth.Token{UserID: "usr_1", Scopes: []string{"gateway:use"}},
		usage.Query{ScopeAll: true}, // a non-admin's scope=all request is ignored by applyUsageScope
		"service",
	)
	if err != nil {
		t.Fatalf("UsageGroups(service): %v", err)
	}
	if len(groups) != 1 || groups[0].Key != "" {
		t.Fatalf("non-admin service groups = %+v, want only the empty-key group (own row, service usage excluded)", groups)
	}
}
