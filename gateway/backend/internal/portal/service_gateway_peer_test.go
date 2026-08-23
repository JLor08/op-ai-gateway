// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// gwPeerFake is a minimal NetBird admin-API stand-in for ReconcileGatewayPeer:
// GET /api/groups (ResolveGroupID find), GET /api/groups/{id} (group membership),
// GET/PUT /api/peers/{id} (peer detail + rename capture). Named to avoid the
// package's pre-existing fakeNetbird (netbird_server_test.go).
type gwPeerFake struct {
	groupID    string
	groupPeers []string
	peers      map[string]map[string]any
	errPeers   map[string]bool // peer ids whose GetPeer transiently 500s (still listed in the group)
	renamed    map[string]string
}

func (f *gwPeerFake) server(t *testing.T) *httptest.Server {
	if f.renamed == nil {
		f.renamed = map[string]string{}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/groups", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{{"id": f.groupID, "name": "op-gw-portal"}})
	})
	mux.HandleFunc("/api/groups/", func(w http.ResponseWriter, r *http.Request) {
		peers := make([]map[string]any, 0, len(f.groupPeers))
		for _, id := range f.groupPeers {
			peers = append(peers, map[string]any{"id": id})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": f.groupID, "name": "op-gw-portal", "peers": peers})
	})
	mux.HandleFunc("/api/peers/", func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/api/peers/")
		if f.errPeers[id] {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		if r.Method == http.MethodPut {
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if n, ok := body["name"].(string); ok {
				f.renamed[id] = n
			}
		}
		_ = json.NewEncoder(w).Encode(f.peers[id])
	})
	return httptest.NewServer(mux)
}

// newGatewayPeerTestService builds a NetBird-enabled Service pointed at url via
// the package's real constructor. SettingsVolatile makes a "plain:" token open;
// netbird_enabled/url/token are seeded directly into the memory settings store
// (no UpdateSystemSettings validation needed — the httptest URL is a valid base).
func newGatewayPeerTestService(t *testing.T, url string) *Service {
	t.Helper()
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	svc := NewService(ServiceDeps{SystemSettings: NewMemorySystemSettings(), SettingsVolatile: true, Clock: func() time.Time { return now }})
	ctx := context.Background()
	for _, kv := range []struct{ k, v string }{
		{netbirdEnabledKey, "true"},
		{netbirdURLKey, url},
		{netbirdTokenKey, "plain:tok"},
	} {
		if err := svc.settings.SetSystemSetting(ctx, kv.k, kv.v, svc.clock()); err != nil {
			t.Fatalf("seed %s: %v", kv.k, err)
		}
	}
	return svc
}

func TestReconcileGatewayPeerSelectsWhenEmpty(t *testing.T) {
	f := &gwPeerFake{
		groupID: "g1", groupPeers: []string{"p1"},
		peers: map[string]map[string]any{"p1": {"id": "p1", "name": "op-gateway", "connected": true, "ip": "100.0.0.1", "last_seen": "2026-07-29T10:00:00Z"}},
	}
	ts := f.server(t)
	defer ts.Close()
	svc := newGatewayPeerTestService(t, ts.URL)
	ctx := context.Background()
	id, changed, err := svc.ReconcileGatewayPeer(ctx)
	if err != nil || !changed || id != "p1" {
		t.Fatalf("= (%q,%v,%v), want (p1,true,nil)", id, changed, err)
	}
	if got := svc.NetbirdGatewayPeerID(ctx); got != "p1" {
		t.Fatalf("persisted = %q, want p1", got)
	}
}

func TestReconcileGatewayPeerKeepsValidManual(t *testing.T) {
	f := &gwPeerFake{
		groupID: "g1", groupPeers: []string{"p1"},
		peers: map[string]map[string]any{"p1": {"id": "p1", "name": "op-gateway", "connected": true, "last_seen": "2026-07-29T10:00:00Z"}},
	}
	ts := f.server(t)
	defer ts.Close()
	svc := newGatewayPeerTestService(t, ts.URL)
	ctx := context.Background()
	_ = svc.settings.SetSystemSetting(ctx, netbirdGatewayPeerIDKey, "p1", svc.clock())
	if _, changed, _ := svc.ReconcileGatewayPeer(ctx); changed {
		t.Fatalf("valid manual pick must be kept (no write)")
	}
}

func TestReconcileGatewayPeerReplacesStale(t *testing.T) {
	f := &gwPeerFake{
		groupID: "g1", groupPeers: []string{"p2"},
		peers: map[string]map[string]any{"p2": {"id": "p2", "name": "op-gateway", "connected": true, "last_seen": "2026-07-29T11:00:00Z"}},
	}
	ts := f.server(t)
	defer ts.Close()
	svc := newGatewayPeerTestService(t, ts.URL)
	ctx := context.Background()
	_ = svc.settings.SetSystemSetting(ctx, netbirdGatewayPeerIDKey, "p1-gone", svc.clock())
	id, changed, _ := svc.ReconcileGatewayPeer(ctx)
	if !changed || id != "p2" || svc.NetbirdGatewayPeerID(ctx) != "p2" {
		t.Fatalf("stale not replaced: id=%q changed=%v persisted=%q", id, changed, svc.NetbirdGatewayPeerID(ctx))
	}
}

func TestReconcileGatewayPeerRenames(t *testing.T) {
	f := &gwPeerFake{
		groupID: "g1", groupPeers: []string{"p1"},
		peers: map[string]map[string]any{"p1": {"id": "p1", "name": "op-gateway", "connected": true, "last_seen": "2026-07-29T10:00:00Z"}},
	}
	ts := f.server(t)
	defer ts.Close()
	svc := newGatewayPeerTestService(t, ts.URL)
	ctx := context.Background()
	_ = svc.settings.SetSystemSetting(ctx, netbirdGatewayPeerNameKey, "renamed-gw", svc.clock())
	if _, _, err := svc.ReconcileGatewayPeer(ctx); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if f.renamed["p1"] != "renamed-gw" {
		t.Fatalf("not renamed: %v", f.renamed)
	}
}

func TestReconcileGatewayPeerKeepsStoredOnTransientGetPeerError(t *testing.T) {
	f := &gwPeerFake{
		groupID: "g1", groupPeers: []string{"p0", "p1"},
		peers: map[string]map[string]any{
			"p0": {"id": "p0", "name": "op-gateway", "connected": false, "last_seen": "2026-07-29T09:00:00Z"},
			"p1": {"id": "p1", "name": "op-gateway", "connected": true, "last_seen": "2026-07-29T10:00:00Z"},
		},
		errPeers: map[string]bool{"p1": true}, // p1 (the stored peer) transiently fails GetPeer
	}
	ts := f.server(t)
	defer ts.Close()
	svc := newGatewayPeerTestService(t, ts.URL)
	ctx := context.Background()
	_ = svc.settings.SetSystemSetting(ctx, netbirdGatewayPeerIDKey, "p1", svc.clock())
	_, changed, _ := svc.ReconcileGatewayPeer(ctx)
	if changed {
		t.Fatalf("changed=true: a transient GetPeer error on the still-member stored peer must NOT re-select")
	}
	if got := svc.NetbirdGatewayPeerID(ctx); got != "p1" {
		t.Fatalf("stored id = %q, want p1 (kept)", got)
	}
}

func TestReconcileGatewayPeerPicksConnectedLatest(t *testing.T) {
	f := &gwPeerFake{
		groupID: "g1", groupPeers: []string{"p-old", "p-live", "p-dead"},
		peers: map[string]map[string]any{
			"p-old":  {"id": "p-old", "name": "op-gateway", "connected": true, "last_seen": "2026-07-29T08:00:00Z"},
			"p-live": {"id": "p-live", "name": "op-gateway", "connected": true, "last_seen": "2026-07-29T12:00:00Z"},
			"p-dead": {"id": "p-dead", "name": "op-gateway", "connected": false, "last_seen": "2026-07-29T23:00:00Z"},
		},
	}
	ts := f.server(t)
	defer ts.Close()
	svc := newGatewayPeerTestService(t, ts.URL)
	id, changed, err := svc.ReconcileGatewayPeer(context.Background())
	if err != nil || !changed || id != "p-live" {
		t.Fatalf("winner = (%q,%v,%v), want p-live (connected + latest last_seen beats a disconnected-but-newer peer)", id, changed, err)
	}
}

// TestUpdateSystemSettingsRenamesGatewayPeerSynchronously: saving a new
// netbird_gateway_peer_name must apply the rename to NetBird BEFORE the PUT
// returns (via a synchronous best-effort ReconcileGatewayPeer), so the live
// status the client refetches reflects the new name immediately — not one
// reconcile-loop interval later. Regression guard for the "UI shows the old
// name after save" bug.
func TestUpdateSystemSettingsRenamesGatewayPeerSynchronously(t *testing.T) {
	f := &gwPeerFake{
		groupID: "g1", groupPeers: []string{"p1"},
		peers: map[string]map[string]any{"p1": {"id": "p1", "name": "op-gateway", "connected": true, "last_seen": "2026-07-29T10:00:00Z"}},
	}
	ts := f.server(t)
	defer ts.Close()
	svc := newGatewayPeerTestService(t, ts.URL)
	ctx := context.Background()
	name := "renamed-gw"
	if _, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{NetbirdGatewayPeerName: &name}); err != nil {
		t.Fatalf("UpdateSystemSettings: %v", err)
	}
	if f.renamed["p1"] != "renamed-gw" {
		t.Fatalf("gateway peer was not renamed synchronously by the settings save: %v", f.renamed)
	}
}

// TestUpdateSystemSettingsNoGatewayPeerFieldsNoRename: a settings save that does
// NOT touch the gateway-peer id/name must not trigger a gateway-peer reconcile
// (no rename), so unrelated saves stay cheap and side-effect-free.
func TestUpdateSystemSettingsNoGatewayPeerFieldsNoRename(t *testing.T) {
	f := &gwPeerFake{
		groupID: "g1", groupPeers: []string{"p1"},
		peers: map[string]map[string]any{"p1": {"id": "p1", "name": "op-gateway", "connected": true, "last_seen": "2026-07-29T10:00:00Z"}},
	}
	ts := f.server(t)
	defer ts.Close()
	svc := newGatewayPeerTestService(t, ts.URL)
	ctx := context.Background()
	// Pre-seed a desired name so a reconcile, IF wrongly triggered, would rename.
	_ = svc.settings.SetSystemSetting(ctx, netbirdGatewayPeerNameKey, "should-not-apply", svc.clock())
	lang := "en"
	if _, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{Language: &lang}); err != nil {
		t.Fatalf("UpdateSystemSettings: %v", err)
	}
	if len(f.renamed) != 0 {
		t.Fatalf("a non-gateway-peer save must not reconcile/rename, got %v", f.renamed)
	}
}

func TestReconcileGatewayPeerModuleOffNoop(t *testing.T) {
	svc := NewService(ServiceDeps{SystemSettings: NewMemorySystemSettings()}) // module off -> no-op
	id, changed, err := svc.ReconcileGatewayPeer(context.Background())
	if id != "" || changed || err != nil {
		t.Fatalf("module-off must be no-op, got (%q,%v,%v)", id, changed, err)
	}
}
