// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package store

import (
	"context"
	"errors"
	"op-ai-gateway/internal/routing"
	"testing"
	"time"
)

// seedNarrowWriterServer creates a NetBird-linked cluster member with a mesh
// IP, the consent columns set and a first last-seen time.
func seedNarrowWriterServer(ctx context.Context, t *testing.T, s routing.Store, now time.Time) routing.AIServer {
	t.Helper()
	seen := now.Add(-time.Minute)
	srv := routing.AIServer{
		ID: "srv_nw", Name: "srv-nw", Domain: "nw.example.test", Provider: routing.ProviderVLLM, Endpoint: "http://nw:8000",
		Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, LastSeenAt: &seen,
		NetbirdEnabled: true, NetbirdPeerID: "peer-1", NetbirdConnected: true, NetbirdIP: "100.64.0.1",
		RPCMemberEnabled: true, RPCAllowedHeadGroups: `["ugrp_a"]`, LANAddress: "192.168.1.5",
		CreatedAt: now, UpdatedAt: now,
	}
	if err := s.CreateAIServer(ctx, srv); err != nil {
		t.Fatalf("create server: %v", err)
	}
	return srv
}

// TestConformanceUpdateServerLastSeenNarrowWriter pins the telemetry
// heartbeat's targeted write: it sets last_seen_at and updated_at and nothing
// else, so a heartbeat that read the row before a portal save cannot write the
// old consent or mesh address back. An unknown id is ErrNotFound.
func TestConformanceUpdateServerLastSeenNarrowWriter(t *testing.T) {
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	forEachRoutingStore(t, func(t *testing.T, s routing.Store) {
		ctx := context.Background()
		srv := seedNarrowWriterServer(ctx, t, s, now)
		// The portal revokes the membership after the heartbeat read the row.
		revoked := srv
		revoked.RPCMemberEnabled, revoked.RPCAllowedHeadGroups = false, "[]"
		if err := s.UpdateAIServer(ctx, revoked); err != nil {
			t.Fatalf("revoke: %v", err)
		}
		seen, updated := now.Add(time.Second), now.Add(2*time.Second)
		if err := s.UpdateServerLastSeen(ctx, srv.ID, seen, updated); err != nil {
			t.Fatalf("UpdateServerLastSeen: %v", err)
		}
		got, err := s.AIServerByID(ctx, srv.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.LastSeenAt == nil || !got.LastSeenAt.Equal(seen) || !got.UpdatedAt.Equal(updated) {
			t.Fatalf("last_seen_at/updated_at = %v/%v, want %v/%v", got.LastSeenAt, got.UpdatedAt, seen, updated)
		}
		if got.RPCMemberEnabled || got.RPCAllowedHeadGroups != "[]" {
			t.Fatalf("heartbeat touched the consent: enabled=%v groups=%q", got.RPCMemberEnabled, got.RPCAllowedHeadGroups)
		}
		if got.NetbirdIP != "100.64.0.1" || got.LANAddress != "192.168.1.5" || got.Name != "srv-nw" || !got.NetbirdConnected {
			t.Fatalf("heartbeat touched another column: %+v", got)
		}
		if err := s.UpdateServerLastSeen(ctx, "srv_nope", seen, updated); !errors.Is(err, ErrNotFound) {
			t.Fatalf("unknown server = %v, want ErrNotFound", err)
		}
	})
}

// TestConformanceUpdateAIServerLeavesNetbirdIP pins that the mesh address is
// written only by the peer sync's targeted writes: a full-row update carrying
// an older address leaves the synced one, and ClearServerNetbirdIP empties it
// without touching the rest of the NetBird state. An unknown id is ErrNotFound.
func TestConformanceUpdateAIServerLeavesNetbirdIP(t *testing.T) {
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	forEachRoutingStore(t, func(t *testing.T, s routing.Store) {
		ctx := context.Background()
		stale := seedNarrowWriterServer(ctx, t, s, now)
		if err := s.UpdateServerNetbirdState(ctx, stale.ID, stale.Domain, "peer-1", "100.64.0.7", true); err != nil {
			t.Fatalf("sync: %v", err)
		}
		stale.Name = "srv-renamed"
		if err := s.UpdateAIServer(ctx, stale); err != nil {
			t.Fatalf("full update: %v", err)
		}
		got, err := s.AIServerByID(ctx, stale.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Name != "srv-renamed" || got.NetbirdIP != "100.64.0.7" {
			t.Fatalf("name/netbird_ip = %q/%q, want srv-renamed/100.64.0.7", got.Name, got.NetbirdIP)
		}
		if err := s.ClearServerNetbirdIP(ctx, stale.ID); err != nil {
			t.Fatalf("ClearServerNetbirdIP: %v", err)
		}
		got, err = s.AIServerByID(ctx, stale.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.NetbirdIP != "" || !got.NetbirdEnabled || got.NetbirdPeerID != "peer-1" || !got.NetbirdConnected ||
			got.Domain != stale.Domain || got.Name != "srv-renamed" {
			t.Fatalf("clear changed more than netbird_ip: %+v", got)
		}
		if err := s.ClearServerNetbirdIP(ctx, "srv_nope"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("unknown server = %v, want ErrNotFound", err)
		}
	})
}
