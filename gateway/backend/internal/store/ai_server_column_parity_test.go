// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package store

import (
	"context"
	"op-ai-gateway/internal/routing"
	"reflect"
	"testing"
	"time"
)

// TestConformanceAIServerReadersAgreeOnEveryColumn closes a PRE-EXISTING
// systemic gap in the conformance suite: the ai_servers column list is
// hand-maintained in FOUR separate queries (AIServerByID, AIServers,
// ServersByOwner, ServersByAdminGroups) that all feed the single
// scanAIServer, and only the first two had round-trip coverage for the
// additive columns. ServersByAdminGroups had none at all.
//
// An OMITTED column already fails loudly — scanAIServer's fixed destination
// count makes database/sql report "expected N destination arguments in Scan"
// — which is why the gap was deferred rather than treated as a live bug. A
// REORDERED column list does NOT fail loudly: two same-typed columns swapped
// in one of the four lists silently returns the wrong values from that one
// reader. So rather than repeat a full field-by-field round-trip per reader,
// this asserts something stronger and shorter: every reader must return the
// EXACT same routing.AIServer value for the same row, and that value must
// carry the distinct non-zero value seeded into each field. One test covers
// omission (loudly), reordering (by value), and any future additive column
// that reaches the struct — a reader that forgets it comes back with a zero
// where its siblings have data.
func TestConformanceAIServerReadersAgreeOnEveryColumn(t *testing.T) {
	forEachDialect(t, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		now := time.Now().UTC().Truncate(time.Second)
		lastSeen := now.Add(-3 * time.Minute)

		if err := s.CreateUser(ctx, User{
			ID: "usr_cols", Email: "cols@example.test", DisplayName: "Cols Owner", Role: "admin",
			Status: UserStatusActive, PreferredLanguage: "de", CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("create owner: %v", err)
		}
		sysGroup := UserGroup{ID: "ugrp_cols_sys", Tier: GroupTierSystem, Name: "Cols System", CreatedAt: now, UpdatedAt: now}
		if err := s.CreateUserGroup(ctx, sysGroup); err != nil {
			t.Fatalf("create system group: %v", err)
		}
		adminGroup := UserGroup{
			ID: "ugrp_cols_admin", Tier: GroupTierAdmin, Name: "Cols Admin",
			ParentGroupID: sysGroup.ID, OwnerUserID: "usr_cols", CreatedAt: now, UpdatedAt: now,
		}
		if err := s.CreateUserGroup(ctx, adminGroup); err != nil {
			t.Fatalf("create admin group: %v", err)
		}

		// Every field carries a DISTINCT, non-zero, non-default value, so a
		// swapped pair of same-typed columns in any one reader's select list
		// shows up as a value mismatch rather than passing unnoticed. The
		// float fields in particular are all different (ADR-005: postgres
		// double precision, so the fractional parts must survive).
		want := routing.AIServer{
			ID: "srv_cols", Name: "Column Parity", Domain: "cols.example.test",
			ServerPathSuffix:  "/upstream",
			NetbirdEnabled:    true,
			NetbirdSetupKeyID: "nb-setup-key-1", NetbirdGroupID: "nb-group-1",
			NetbirdPeerID: "nb-peer-1", NetbirdConnected: true,
			NetbirdGroupIDs:    `[{"id":"nbg1","name":"Policy A"}]`,
			NetbirdPeerManaged: true, NetbirdPolicyOverride: "include",
			NetbirdAllowPing: true, NetbirdPingExclude: true,
			AgentPresenceTimeoutSeconds: 4242,
			EstimatedWatts:              321.5, IdleWatts: 65.25, PricePerKwh: 0.3175, Pue: 1.375,
			PriceUnit: "eur", SystemGroupID: sysGroup.ID,
			CertificateOverride: "exclude", HTTPSSwitchOverride: "include",
			RuntimeMaxProcesses: 7, ManagedRuntimeOnly: true,
			Provider: routing.ProviderVLLM, Endpoint: "http://cols.example.test:8000",
			Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy,
			LastSeenAt: &lastSeen, CreatedAt: now, UpdatedAt: now,
		}
		if err := s.CreateAIServer(ctx, want); err != nil {
			t.Fatalf("create server: %v", err)
		}
		// SystemGroupID has its own writer (it is not part of the insert).
		if err := s.UpdateServerSystemGroup(ctx, want.ID, sysGroup.ID); err != nil {
			t.Fatalf("update system group: %v", err)
		}
		if err := s.SetServerOwners(ctx, want.ID, []string{"usr_cols"}); err != nil {
			t.Fatalf("set owners: %v", err)
		}
		if err := s.SetServerAdminGroup(ctx, want.ID, adminGroup.ID); err != nil {
			t.Fatalf("set admin group: %v", err)
		}

		byID, err := s.AIServerByID(ctx, want.ID)
		if err != nil {
			t.Fatalf("AIServerByID: %v", err)
		}
		// The baseline the other three readers are compared against is itself
		// checked field-by-field against the seeded value, so a column
		// missing from ALL FOUR lists (a genuinely forgotten additive column)
		// cannot hide behind four readers agreeing on the same zero.
		if !reflect.DeepEqual(normalizeServerForCompare(byID), normalizeServerForCompare(want)) {
			t.Fatalf("AIServerByID lost or reordered a column:\n got  %+v\n want %+v", byID, want)
		}

		list, err := s.AIServers(ctx)
		if err != nil || len(list) != 1 {
			t.Fatalf("AIServers: err=%v n=%d, want 1", err, len(list))
		}
		byOwner, err := s.ServersByOwner(ctx, "usr_cols")
		if err != nil || len(byOwner) != 1 {
			t.Fatalf("ServersByOwner: err=%v n=%d, want 1", err, len(byOwner))
		}
		byAdminGroups, err := s.ServersByAdminGroups(ctx, []string{adminGroup.ID})
		if err != nil || len(byAdminGroups) != 1 {
			t.Fatalf("ServersByAdminGroups: err=%v n=%d, want 1", err, len(byAdminGroups))
		}

		for _, reader := range []struct {
			name string
			got  routing.AIServer
		}{
			{"AIServers", list[0]},
			{"ServersByOwner", byOwner[0]},
			{"ServersByAdminGroups", byAdminGroups[0]},
		} {
			if !reflect.DeepEqual(normalizeServerForCompare(reader.got), normalizeServerForCompare(byID)) {
				t.Fatalf("%s disagrees with AIServerByID on at least one column:\n got  %+v\n want %+v",
					reader.name, reader.got, byID)
			}
		}
	})
}

// normalizeServerForCompare makes two routing.AIServer values comparable with
// reflect.DeepEqual across the dialects: LastSeenAt is a *time.Time (pointer
// identity would never match) and postgres returns timestamptz in a different
// *time.Location than the sqlite driver, so every time is compared as a UTC
// wall-clock value. Nothing else is touched — in particular no field is
// zeroed, which would be the way to accidentally exclude a column from the
// comparison this test exists to make.
func normalizeServerForCompare(in routing.AIServer) routing.AIServer {
	out := in
	out.CreatedAt = in.CreatedAt.UTC()
	out.UpdatedAt = in.UpdatedAt.UTC()
	if in.LastSeenAt != nil {
		utc := in.LastSeenAt.UTC()
		out.LastSeenAt = &utc
	}
	return out
}
