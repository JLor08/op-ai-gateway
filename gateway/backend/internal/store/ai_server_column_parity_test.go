// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package store

import (
	"context"
	"op-ai-gateway/internal/routing"
	"reflect"
	"strconv"
	"testing"
	"time"
)

// aiServerParityRows is how many ai_servers rows the column-parity fixture
// seeds, and it is a computed minimum, not a round number: the select list
// carries SIX bool columns, and two same-typed columns are only
// distinguishable by value if they differ in at least one seeded row, so each
// bool column needs its own DISTINCT pattern across the rows. With r rows
// there are 2^r patterns, so r must satisfy 2^r >= 6 — three rows.
//
// One row cannot work at all (every bool would have to hold the same value as
// its neighbours), and two rows cover only 4 of the 6 columns' worth of
// patterns, leaving pairs like netbird_enabled/netbird_peer_managed
// interchangeable. That was the actual defect: the original single-row fixture
// set all six bools to true, so swapping netbird_allow_ping and
// netbird_ping_exclude in one reader's select list was invisible.
const aiServerParityRows = 3

// aiServerParityBools is the bit-pattern table, one row per bool column in
// SELECT-LIST order (netbird_enabled, netbird_connected, netbird_peer_managed,
// netbird_allow_ping, netbird_ping_exclude, managed_runtime_only) and one
// column per seeded server.
//
// Two properties are load-bearing and both are asserted by
// TestAIServerParityFixtureDistinguishesEverySameTypedPair below, so this
// table cannot silently degrade:
//
//   - every PAIR of rows differs in at least one column, so a swap of any two
//     of the six bool columns changes an observable value in at least one
//     seeded server;
//   - every row is true in at least one column, so a column dropped from a
//     reader's select list (coming back as the false zero value) still shows
//     up as a mismatch somewhere.
var aiServerParityBools = [6][aiServerParityRows]bool{
	{true, true, false},  // netbird_enabled
	{true, false, true},  // netbird_connected
	{true, false, false}, // netbird_peer_managed
	{false, true, true},  // netbird_allow_ping
	{false, true, false}, // netbird_ping_exclude
	{false, false, true}, // managed_runtime_only
}

// aiServerParityOverrides is the same table for the three 3-state override
// columns in select-list order (netbird_policy_override,
// certificate_override, https_switch_override). They are strings, but with
// only two non-empty legal values ("include"/"exclude") they are exactly as
// interchangeable as the bools, and "" is excluded on purpose: it is the
// zero value, so a row carrying it could not catch an omitted column. Three
// distinct patterns over three rows, so every pair differs somewhere.
var aiServerParityOverrides = [3][aiServerParityRows]string{
	{"include", "include", "exclude"}, // netbird_policy_override
	{"include", "exclude", "include"}, // certificate_override
	{"exclude", "include", "include"}, // https_switch_override
}

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
// carry the value seeded into each field.
//
// WHAT THE FIXTURE ACTUALLY GUARANTEES, stated precisely because the earlier
// version of this comment claimed more than it delivered ("every field
// carries a DISTINCT, non-zero, non-default value, so a swapped pair of
// same-typed columns in any one reader's select list shows up as a value
// mismatch"). That was false for 8 of the 32 fields: all six bools were
// seeded true and two of the three override strings were seeded "include", so
// swapping netbird_allow_ping with netbird_ping_exclude in
// ServersByAdminGroups' select list left this test GREEN. A bool has two
// possible values and a 3-state override two usable ones, so no single row
// can make them pairwise distinct — the fixture needs several rows with
// different patterns, not richer values.
//
// So: the fields that CAN hold a distinct value per row (strings, ints,
// floats, times) do, and each is varied per row as well, so a reader that
// returns the wrong ROW is caught too. The six bools and three override
// strings carry the bit-pattern tables above, sized so that every pair of
// same-typed columns differs in at least one of the three seeded servers.
// Together that makes a swapped pair in any one reader's list observable
// wherever the two columns are same-typed — which is the only case that does
// not already fail loudly at Scan.
func TestConformanceAIServerReadersAgreeOnEveryColumn(t *testing.T) {
	forEachDialect(t, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		now := time.Now().UTC().Truncate(time.Second)

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

		// All three servers share the one owner and the one admin group, so
		// every list reader returns all three rows and must return the right
		// VALUES FOR THE RIGHT ROW — a reader that scanned a fixed row, or
		// mismatched rows to ids, could not pass.
		want := make([]routing.AIServer, 0, aiServerParityRows)
		for i := 0; i < aiServerParityRows; i++ {
			idx := strconv.Itoa(i)
			lastSeen := now.Add(time.Duration(-3-i) * time.Minute)
			want = append(want, routing.AIServer{
				ID: "srv_cols_" + idx, Name: "Column Parity " + idx, Domain: "cols" + idx + ".example.test",
				ServerPathSuffix:  "/upstream-" + idx,
				NetbirdEnabled:    aiServerParityBools[0][i],
				NetbirdSetupKeyID: "nb-setup-key-" + idx, NetbirdGroupID: "nb-group-" + idx,
				NetbirdPeerID: "nb-peer-" + idx, NetbirdConnected: aiServerParityBools[1][i],
				NetbirdGroupIDs:    `[{"id":"nbg` + idx + `","name":"Policy ` + idx + `"}]`,
				NetbirdPeerManaged: aiServerParityBools[2][i], NetbirdPolicyOverride: aiServerParityOverrides[0][i],
				NetbirdAllowPing: aiServerParityBools[3][i], NetbirdPingExclude: aiServerParityBools[4][i],
				AgentPresenceTimeoutSeconds: 4242 + i,
				EstimatedWatts:              321.5 + float64(i), IdleWatts: 65.25 + float64(i),
				PricePerKwh: 0.3175 + float64(i)/10000, Pue: 1.375 + float64(i)/100,
				PriceUnit: "eur", SystemGroupID: sysGroup.ID,
				CertificateOverride: aiServerParityOverrides[1][i], HTTPSSwitchOverride: aiServerParityOverrides[2][i],
				RuntimeMaxProcesses: 7 + i, ManagedRuntimeOnly: aiServerParityBools[5][i],
				Provider: routing.ProviderVLLM, Endpoint: "http://cols" + idx + ".example.test:800" + idx,
				Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy,
				LastSeenAt: &lastSeen, CreatedAt: now, UpdatedAt: now,
			})
		}

		for _, server := range want {
			if err := s.CreateAIServer(ctx, server); err != nil {
				t.Fatalf("create server %s: %v", server.ID, err)
			}
			// SystemGroupID has its own writer (it is not part of the insert).
			if err := s.UpdateServerSystemGroup(ctx, server.ID, sysGroup.ID); err != nil {
				t.Fatalf("update system group %s: %v", server.ID, err)
			}
			if err := s.SetServerOwners(ctx, server.ID, []string{"usr_cols"}); err != nil {
				t.Fatalf("set owners %s: %v", server.ID, err)
			}
			if err := s.SetServerAdminGroup(ctx, server.ID, adminGroup.ID); err != nil {
				t.Fatalf("set admin group %s: %v", server.ID, err)
			}
		}

		list, err := s.AIServers(ctx)
		if err != nil {
			t.Fatalf("AIServers: %v", err)
		}
		byOwner, err := s.ServersByOwner(ctx, "usr_cols")
		if err != nil {
			t.Fatalf("ServersByOwner: %v", err)
		}
		byAdminGroups, err := s.ServersByAdminGroups(ctx, []string{adminGroup.ID})
		if err != nil {
			t.Fatalf("ServersByAdminGroups: %v", err)
		}
		readers := []struct {
			name string
			got  []routing.AIServer
		}{
			{"AIServers", list},
			{"ServersByOwner", byOwner},
			{"ServersByAdminGroups", byAdminGroups},
		}
		for _, reader := range readers {
			if len(reader.got) != aiServerParityRows {
				t.Fatalf("%s returned %d servers, want %d", reader.name, len(reader.got), aiServerParityRows)
			}
		}

		for _, expected := range want {
			byID, err := s.AIServerByID(ctx, expected.ID)
			if err != nil {
				t.Fatalf("AIServerByID(%s): %v", expected.ID, err)
			}
			// The baseline the other three readers are compared against is
			// itself checked field-by-field against the seeded value, so a
			// column missing from ALL FOUR lists (a genuinely forgotten
			// additive column) cannot hide behind four readers agreeing on the
			// same zero.
			if !reflect.DeepEqual(normalizeServerForCompare(byID), normalizeServerForCompare(expected)) {
				t.Fatalf("AIServerByID(%s) lost or reordered a column:\n got  %+v\n want %+v", expected.ID, byID, expected)
			}
			for _, reader := range readers {
				got, ok := findServerByID(reader.got, expected.ID)
				if !ok {
					t.Fatalf("%s did not return %s at all", reader.name, expected.ID)
				}
				if !reflect.DeepEqual(normalizeServerForCompare(got), normalizeServerForCompare(byID)) {
					t.Fatalf("%s disagrees with AIServerByID on at least one column of %s:\n got  %+v\n want %+v",
						reader.name, expected.ID, got, byID)
				}
			}
		}
	})
}

// TestAIServerParityFixtureDistinguishesEverySameTypedPair pins the two
// properties the bit-pattern tables above must have for the fixture to be
// able to catch a swapped pair at all. Without it, someone "simplifying" the
// tables back to all-true would silently restore the exact hole this fixture
// was rebuilt to close, and every test would stay green — which is how the
// hole got there the first time.
func TestAIServerParityFixtureDistinguishesEverySameTypedPair(t *testing.T) {
	boolNames := []string{
		"netbird_enabled", "netbird_connected", "netbird_peer_managed",
		"netbird_allow_ping", "netbird_ping_exclude", "managed_runtime_only",
	}
	for i := range aiServerParityBools {
		trueSomewhere := false
		for _, v := range aiServerParityBools[i] {
			if v {
				trueSomewhere = true
			}
		}
		if !trueSomewhere {
			t.Errorf("%s is false in every seeded row: an omitted column would read back as the same false", boolNames[i])
		}
		for j := i + 1; j < len(aiServerParityBools); j++ {
			if aiServerParityBools[i] == aiServerParityBools[j] {
				t.Errorf("%s and %s carry the same pattern %v: swapping them in a select list is invisible",
					boolNames[i], boolNames[j], aiServerParityBools[i])
			}
		}
	}

	overrideNames := []string{"netbird_policy_override", "certificate_override", "https_switch_override"}
	for i := range aiServerParityOverrides {
		for _, v := range aiServerParityOverrides[i] {
			if v == "" {
				t.Errorf("%s is empty in a seeded row: '' is the zero value, so an omitted column would be invisible there", overrideNames[i])
			}
		}
		for j := i + 1; j < len(aiServerParityOverrides); j++ {
			if aiServerParityOverrides[i] == aiServerParityOverrides[j] {
				t.Errorf("%s and %s carry the same pattern %v: swapping them in a select list is invisible",
					overrideNames[i], overrideNames[j], aiServerParityOverrides[i])
			}
		}
	}
}

// findServerByID returns the server with the given id from a list reader's
// result. Matching by id rather than by position is deliberate: it makes the
// comparison independent of each reader's ORDER BY, so a reordering change
// fails the ordering tests that exist for it rather than this one.
func findServerByID(servers []routing.AIServer, id string) (routing.AIServer, bool) {
	for _, server := range servers {
		if server.ID == id {
			return server, true
		}
	}
	return routing.AIServer{}, false
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
