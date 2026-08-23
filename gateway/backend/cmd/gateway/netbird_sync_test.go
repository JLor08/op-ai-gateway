// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/netbird"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/routing"
	"strings"
	"sync"
	"testing"
	"time"
)

// renameCall records one observed PUT /api/peers/{id} rename.
type renameCall struct {
	peerID string
	name   string
}

// fakeNetbirdServer is an httptest-backed NetBird admin API fake exercising the
// REAL internal/netbird client. It serves GET /api/groups/{id}, GET/PUT/DELETE
// /api/peers/{id}; ids in failIDs return 500. It records the total request count,
// the observed renames, the observed peer deletions, and the last Authorization
// header.
type fakeNetbirdServer struct {
	mu       sync.Mutex
	groups   map[string]netbird.Group
	peers    map[string]netbird.Peer
	failIDs  map[string]bool
	requests int
	renames  []renameCall
	deletes  []string
	lastAuth string
}

func newFakeNetbird() *fakeNetbirdServer {
	return &fakeNetbirdServer{
		groups:  map[string]netbird.Group{},
		peers:   map[string]netbird.Peer{},
		failIDs: map[string]bool{},
	}
}

func (f *fakeNetbirdServer) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.requests++
		f.lastAuth = r.Header.Get("Authorization")
		path := r.URL.Path
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(path, "/api/groups/"):
			id := strings.TrimPrefix(path, "/api/groups/")
			if f.failIDs[id] {
				http.Error(w, `{"message":"boom"}`, http.StatusInternalServerError)
				return
			}
			g, ok := f.groups[id]
			if !ok {
				http.Error(w, `{"message":"not found"}`, http.StatusNotFound)
				return
			}
			writeJSONResp(w, g)
		case r.Method == http.MethodGet && strings.HasPrefix(path, "/api/peers/"):
			id := strings.TrimPrefix(path, "/api/peers/")
			if f.failIDs[id] {
				http.Error(w, `{"message":"boom"}`, http.StatusInternalServerError)
				return
			}
			p, ok := f.peers[id]
			if !ok {
				http.Error(w, `{"message":"not found"}`, http.StatusNotFound)
				return
			}
			writeJSONResp(w, p)
		case r.Method == http.MethodPut && strings.HasPrefix(path, "/api/peers/"):
			id := strings.TrimPrefix(path, "/api/peers/")
			var body struct {
				Name string `json:"name"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.renames = append(f.renames, renameCall{peerID: id, name: body.Name})
			p := f.peers[id]
			p.Name = body.Name
			f.peers[id] = p
			writeJSONResp(w, p)
		case r.Method == http.MethodDelete && strings.HasPrefix(path, "/api/peers/"):
			id := strings.TrimPrefix(path, "/api/peers/")
			if f.failIDs[id] {
				http.Error(w, `{"message":"boom"}`, http.StatusInternalServerError)
				return
			}
			f.deletes = append(f.deletes, id)
			delete(f.peers, id)
			w.WriteHeader(http.StatusOK)
		default:
			http.Error(w, `{"message":"unexpected"}`, http.StatusNotFound)
		}
	})
}

func (f *fakeNetbirdServer) requestCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests
}

func (f *fakeNetbirdServer) renameCalls() []renameCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]renameCall(nil), f.renames...)
}

func (f *fakeNetbirdServer) deleteCalls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.deletes...)
}

func writeJSONResp(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// stateWrite records one UpdateServerNetbirdState call.
type stateWrite struct {
	id        string
	domain    string
	peerID    string
	connected bool
}

// fakeServerStore records server enumeration + state writes. serverListCalls
// counts AIServers invocations so a loop test can observe repeated passes.
type fakeServerStore struct {
	mu             sync.Mutex
	servers        []routing.AIServer
	writes         []stateWrite
	aiServersCalls int
}

func (f *fakeServerStore) AIServers(context.Context) ([]routing.AIServer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.aiServersCalls++
	return append([]routing.AIServer(nil), f.servers...), nil
}

func (f *fakeServerStore) UpdateServerNetbirdState(_ context.Context, id, domain, peerID string, connected bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writes = append(f.writes, stateWrite{id: id, domain: domain, peerID: peerID, connected: connected})
	return nil
}

func (f *fakeServerStore) stateWrites() []stateWrite {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]stateWrite(nil), f.writes...)
}

func (f *fakeServerStore) serverListCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.aiServersCalls
}

// fakeNetbirdSettings returns a fixed NetbirdConfig + ok.
type fakeNetbirdSettings struct {
	cfg portal.NetbirdConfig
	ok  bool
}

func (f fakeNetbirdSettings) NetbirdConfig(context.Context) (portal.NetbirdConfig, bool, error) {
	return f.cfg, f.ok, nil
}

// settingsFor builds an enabled settings source pointing at the fake NetBird URL.
func settingsFor(url string) fakeNetbirdSettings {
	return fakeNetbirdSettings{cfg: portal.NetbirdConfig{URL: url, Token: "test-token"}, ok: true}
}

func nowUTC() time.Time { return time.Now().UTC() }

func TestNetbirdSyncEnrolledRenamesAndWritesState(t *testing.T) {
	fake := newFakeNetbird()
	fake.groups["g1"] = netbird.Group{ID: "g1", Name: "op-gw-srv-1", Peers: []netbird.GroupPeer{{ID: "p1", Name: "old-peer-name"}}}
	fake.peers["p1"] = netbird.Peer{ID: "p1", Name: "old-peer-name", DNSLabel: "p1.netbird.cloud", Connected: true}
	ts := httptest.NewServer(fake.handler())
	defer ts.Close()

	store := &fakeServerStore{servers: []routing.AIServer{{
		ID: "srv-1", Name: "srv-1", Domain: "old.local",
		NetbirdEnabled: true, NetbirdGroupID: "g1", NetbirdPeerID: "", NetbirdConnected: false,
	}}}

	runNetbirdSyncOnce(context.Background(), store, settingsFor(ts.URL), netbirdCallTimeout, nowUTC, nil)

	renames := fake.renameCalls()
	if len(renames) != 1 || renames[0].peerID != "p1" || renames[0].name != "srv-1" {
		t.Fatalf("expected one rename of p1 to srv-1, got %+v", renames)
	}
	writes := store.stateWrites()
	if len(writes) != 1 {
		t.Fatalf("expected one state write, got %d: %+v", len(writes), writes)
	}
	want := stateWrite{id: "srv-1", domain: "p1.netbird.cloud", peerID: "p1", connected: true}
	if writes[0] != want {
		t.Fatalf("state write = %+v, want %+v", writes[0], want)
	}
	if fake.lastAuth != "Token test-token" {
		t.Fatalf("Authorization = %q, want %q", fake.lastAuth, "Token test-token")
	}
}

// TestNetbirdSyncPeerIDOnlyReconciles: a manually-linked server (peer id set, NO
// tracking group) is reconciled — the sync-gate no longer requires a group id, so
// the peer is resolved by its stored id, renamed, and its domain/connected written.
func TestNetbirdSyncPeerIDOnlyReconciles(t *testing.T) {
	fake := newFakeNetbird()
	fake.peers["p1"] = netbird.Peer{ID: "p1", Name: "old-peer-name", DNSLabel: "p1.netbird.cloud", Connected: true}
	ts := httptest.NewServer(fake.handler())
	defer ts.Close()

	// NetbirdGroupID is empty (manually linked via the system-admin editor); only
	// the peer id is set.
	store := &fakeServerStore{servers: []routing.AIServer{{
		ID: "srv-1", Name: "srv-1", Domain: "old.local",
		NetbirdEnabled: true, NetbirdGroupID: "", NetbirdPeerID: "p1", NetbirdConnected: false,
	}}}

	runNetbirdSyncOnce(context.Background(), store, settingsFor(ts.URL), netbirdCallTimeout, nowUTC, nil)

	renames := fake.renameCalls()
	if len(renames) != 1 || renames[0].peerID != "p1" || renames[0].name != "srv-1" {
		t.Fatalf("expected one rename of p1 to srv-1, got %+v", renames)
	}
	writes := store.stateWrites()
	if len(writes) != 1 {
		t.Fatalf("expected one state write, got %d: %+v", len(writes), writes)
	}
	want := stateWrite{id: "srv-1", domain: "p1.netbird.cloud", peerID: "p1", connected: true}
	if writes[0] != want {
		t.Fatalf("state write = %+v, want %+v", writes[0], want)
	}
}

// TestNetbirdSyncNoGroupNoPeerSkipped: a NetBird-enabled server with NEITHER a
// group nor a peer id is still skipped (nothing to reconcile), so no NetBird call
// is made for it.
func TestNetbirdSyncNoGroupNoPeerSkipped(t *testing.T) {
	fake := newFakeNetbird()
	ts := httptest.NewServer(fake.handler())
	defer ts.Close()

	store := &fakeServerStore{servers: []routing.AIServer{{
		ID: "srv-1", Name: "srv-1", Domain: "good.local",
		NetbirdEnabled: true, NetbirdGroupID: "", NetbirdPeerID: "", NetbirdConnected: false,
	}}}

	runNetbirdSyncOnce(context.Background(), store, settingsFor(ts.URL), netbirdCallTimeout, nowUTC, nil)

	if n := fake.requestCount(); n != 0 {
		t.Fatalf("expected zero HTTP calls for a group-less+peer-less server, got %d", n)
	}
	if writes := store.stateWrites(); len(writes) != 0 {
		t.Fatalf("expected no state write, got %+v", writes)
	}
}

func TestNetbirdSyncConnectedFlipOnly(t *testing.T) {
	fake := newFakeNetbird()
	// Already enrolled (stored peer id), name + domain steady, only connected flips.
	fake.peers["p1"] = netbird.Peer{ID: "p1", Name: "srv-1", DNSLabel: "p1.netbird.cloud", Connected: true}
	ts := httptest.NewServer(fake.handler())
	defer ts.Close()

	store := &fakeServerStore{servers: []routing.AIServer{{
		ID: "srv-1", Name: "srv-1", Domain: "p1.netbird.cloud",
		NetbirdEnabled: true, NetbirdGroupID: "g1", NetbirdPeerID: "p1", NetbirdConnected: false,
	}}}

	runNetbirdSyncOnce(context.Background(), store, settingsFor(ts.URL), netbirdCallTimeout, nowUTC, nil)

	if renames := fake.renameCalls(); len(renames) != 0 {
		t.Fatalf("expected no rename, got %+v", renames)
	}
	writes := store.stateWrites()
	if len(writes) != 1 {
		t.Fatalf("expected one state write, got %d: %+v", len(writes), writes)
	}
	want := stateWrite{id: "srv-1", domain: "p1.netbird.cloud", peerID: "p1", connected: true}
	if writes[0] != want {
		t.Fatalf("state write = %+v, want %+v", writes[0], want)
	}
}

func TestNetbirdSyncNotEnrolledNoWrite(t *testing.T) {
	fake := newFakeNetbird()
	fake.groups["g1"] = netbird.Group{ID: "g1", Name: "op-gw-srv-1", Peers: nil} // empty group
	ts := httptest.NewServer(fake.handler())
	defer ts.Close()

	store := &fakeServerStore{servers: []routing.AIServer{{
		ID: "srv-1", Name: "srv-1", Domain: "old.local",
		NetbirdEnabled: true, NetbirdGroupID: "g1", NetbirdPeerID: "",
	}}}

	runNetbirdSyncOnce(context.Background(), store, settingsFor(ts.URL), netbirdCallTimeout, nowUTC, nil)

	if renames := fake.renameCalls(); len(renames) != 0 {
		t.Fatalf("expected no rename, got %+v", renames)
	}
	if writes := store.stateWrites(); len(writes) != 0 {
		t.Fatalf("expected no state write, got %+v", writes)
	}
}

func TestNetbirdSyncSteadyStateNoWrite(t *testing.T) {
	fake := newFakeNetbird()
	fake.peers["p1"] = netbird.Peer{ID: "p1", Name: "srv-1", DNSLabel: "p1.netbird.cloud", Connected: true}
	ts := httptest.NewServer(fake.handler())
	defer ts.Close()

	store := &fakeServerStore{servers: []routing.AIServer{{
		ID: "srv-1", Name: "srv-1", Domain: "p1.netbird.cloud",
		NetbirdEnabled: true, NetbirdGroupID: "g1", NetbirdPeerID: "p1", NetbirdConnected: true,
	}}}

	runNetbirdSyncOnce(context.Background(), store, settingsFor(ts.URL), netbirdCallTimeout, nowUTC, nil)

	if renames := fake.renameCalls(); len(renames) != 0 {
		t.Fatalf("expected no rename, got %+v", renames)
	}
	if writes := store.stateWrites(); len(writes) != 0 {
		t.Fatalf("expected no state write in steady state, got %+v", writes)
	}
}

func TestNetbirdSyncModuleOffNoCalls(t *testing.T) {
	fake := newFakeNetbird()
	fake.peers["p1"] = netbird.Peer{ID: "p1", Name: "srv-1", DNSLabel: "p1.netbird.cloud", Connected: true}
	ts := httptest.NewServer(fake.handler())
	defer ts.Close()

	store := &fakeServerStore{servers: []routing.AIServer{{
		ID: "srv-1", Name: "srv-1", Domain: "old.local",
		NetbirdEnabled: true, NetbirdGroupID: "g1", NetbirdPeerID: "p1",
	}}}
	// ok=false -> module off.
	settings := fakeNetbirdSettings{cfg: portal.NetbirdConfig{URL: ts.URL, Token: "test-token"}, ok: false}

	runNetbirdSyncOnce(context.Background(), store, settings, netbirdCallTimeout, nowUTC, nil)

	if n := fake.requestCount(); n != 0 {
		t.Fatalf("expected zero HTTP calls when module off, got %d", n)
	}
	if writes := store.stateWrites(); len(writes) != 0 {
		t.Fatalf("expected no state write when module off, got %+v", writes)
	}
}

// TestNetbirdSyncStaleCachedPeerReResolvesViaGroup: a stored peer id that 404s
// (deleted + re-enrolled) must fall back to the tracking group and ADOPT the fresh
// peer — renaming it and writing its new id/dns/connected state.
func TestNetbirdSyncStaleCachedPeerReResolvesViaGroup(t *testing.T) {
	fake := newFakeNetbird()
	// Cached peer p1 is gone (404); the tracking group now holds a fresh peer p2.
	fake.groups["g1"] = netbird.Group{ID: "g1", Name: "op-gw-srv-1", Peers: []netbird.GroupPeer{{ID: "p2", Name: "auto-name"}}}
	fake.peers["p2"] = netbird.Peer{ID: "p2", Name: "auto-name", DNSLabel: "p2.netbird.cloud", Connected: true}
	ts := httptest.NewServer(fake.handler())
	defer ts.Close()

	store := &fakeServerStore{servers: []routing.AIServer{{
		ID: "srv-1", Name: "srv-1", Domain: "old.local",
		NetbirdEnabled: true, NetbirdGroupID: "g1", NetbirdPeerID: "p1", NetbirdConnected: false,
	}}}

	runNetbirdSyncOnce(context.Background(), store, settingsFor(ts.URL), netbirdCallTimeout, nowUTC, nil)

	// The fresh peer p2 is renamed to the gateway server name.
	renames := fake.renameCalls()
	if len(renames) != 1 || renames[0].peerID != "p2" || renames[0].name != "srv-1" {
		t.Fatalf("expected one rename of p2 to srv-1, got %+v", renames)
	}
	// And its new id/dns/connected are written (the re-enrolled peer is adopted).
	writes := store.stateWrites()
	if len(writes) != 1 {
		t.Fatalf("expected one state write, got %d: %+v", len(writes), writes)
	}
	want := stateWrite{id: "srv-1", domain: "p2.netbird.cloud", peerID: "p2", connected: true}
	if writes[0] != want {
		t.Fatalf("state write = %+v, want %+v", writes[0], want)
	}
}

// TestNetbirdSyncStaleCachedPeerAndEmptyGroupNoWrite: a stored peer id that 404s
// AND a tracking group that errors/is empty must NOT write — a transient error can
// never clear a good domain.
func TestNetbirdSyncStaleCachedPeerAndEmptyGroupNoWrite(t *testing.T) {
	// Empty group: the fallback finds no fresh peer -> skip without writing.
	empty := newFakeNetbird()
	empty.groups["g1"] = netbird.Group{ID: "g1", Name: "op-gw-srv-1", Peers: nil}
	// The cached peer p1 is not registered -> GetPeer 404s.
	tsEmpty := httptest.NewServer(empty.handler())
	defer tsEmpty.Close()

	// Group errors: GetGroup 500s -> skip without writing.
	failing := newFakeNetbird()
	failing.failIDs["g1"] = true
	tsFail := httptest.NewServer(failing.handler())
	defer tsFail.Close()

	for _, tc := range []struct {
		name string
		url  string
	}{
		{"empty-group", tsEmpty.URL},
		{"group-errors", tsFail.URL},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeServerStore{servers: []routing.AIServer{{
				ID: "srv-1", Name: "srv-1", Domain: "good.local",
				NetbirdEnabled: true, NetbirdGroupID: "g1", NetbirdPeerID: "p1", NetbirdConnected: true,
			}}}
			runNetbirdSyncOnce(context.Background(), store, settingsFor(tc.url), netbirdCallTimeout, nowUTC, nil)
			if writes := store.stateWrites(); len(writes) != 0 {
				t.Fatalf("expected no state write (domain preserved), got %+v", writes)
			}
		})
	}
}

func TestNetbirdSyncErrorSurvivesAndProcessesOthers(t *testing.T) {
	fake := newFakeNetbird()
	// srv-1 resolves via a failing peer id; srv-2 is healthy.
	fake.failIDs["pbad"] = true
	fake.peers["p2"] = netbird.Peer{ID: "p2", Name: "old-2", DNSLabel: "p2.netbird.cloud", Connected: true}
	ts := httptest.NewServer(fake.handler())
	defer ts.Close()

	store := &fakeServerStore{servers: []routing.AIServer{
		{ID: "srv-1", Name: "srv-1", Domain: "old-1.local", NetbirdEnabled: true, NetbirdGroupID: "g1", NetbirdPeerID: "pbad"},
		{ID: "srv-2", Name: "srv-2", Domain: "old-2.local", NetbirdEnabled: true, NetbirdGroupID: "g2", NetbirdPeerID: "p2", NetbirdConnected: false},
	}}

	runNetbirdSyncOnce(context.Background(), store, settingsFor(ts.URL), netbirdCallTimeout, nowUTC, nil)

	writes := store.stateWrites()
	if len(writes) != 1 {
		t.Fatalf("expected exactly one state write (srv-2), got %d: %+v", len(writes), writes)
	}
	want := stateWrite{id: "srv-2", domain: "p2.netbird.cloud", peerID: "p2", connected: true}
	if writes[0] != want {
		t.Fatalf("state write = %+v, want %+v", writes[0], want)
	}
	// srv-2 had name "srv-2" vs peer "old-2" -> a rename should have happened for p2.
	renames := fake.renameCalls()
	if len(renames) != 1 || renames[0].peerID != "p2" || renames[0].name != "srv-2" {
		t.Fatalf("expected one rename of p2 to srv-2, got %+v", renames)
	}
}

// --- one-peer dedup backstop (Task 3) -------------------------------------

// TestNetbirdSyncDedupTwoPeersNewestWins: the tracking group holds a stale OLD
// peer (older last_seen, the stored peer id) + a fresh NEW peer (newer
// last_seen). The backstop deletes OLD, keeps NEW, and the existing state write
// adopts NEW's id/dns/connected. (Mutation guard for the winner rule: flip the
// last_seen comparison so OLD wins -> NEW deleted, state keeps p-old -> this
// test fails.)
func TestNetbirdSyncDedupTwoPeersNewestWins(t *testing.T) {
	fake := newFakeNetbird()
	fake.groups["g1"] = netbird.Group{ID: "g1", Name: "op-gw-srv-1", Peers: []netbird.GroupPeer{
		{ID: "p-old", Name: "srv-1"},
		{ID: "p-new", Name: "srv-1"},
	}}
	fake.peers["p-old"] = netbird.Peer{ID: "p-old", Name: "srv-1", DNSLabel: "old.netbird.cloud", Connected: false, LastSeen: "2026-07-25T09:00:00Z"}
	fake.peers["p-new"] = netbird.Peer{ID: "p-new", Name: "srv-1", DNSLabel: "new.netbird.cloud", Connected: true, LastSeen: "2026-07-25T10:00:00Z"}
	ts := httptest.NewServer(fake.handler())
	defer ts.Close()

	// Resolver returns OLD (stored peer_id = p-old, still resolves).
	store := &fakeServerStore{servers: []routing.AIServer{{
		ID: "srv-1", Name: "srv-1", Domain: "old.netbird.cloud",
		NetbirdEnabled: true, NetbirdGroupID: "g1", NetbirdPeerID: "p-old", NetbirdConnected: false,
	}}}

	runNetbirdSyncOnce(context.Background(), store, settingsFor(ts.URL), netbirdCallTimeout, nowUTC, nil)

	// OLD is deleted, NEW survives (NEVER the winner).
	if dels := fake.deleteCalls(); len(dels) != 1 || dels[0] != "p-old" {
		t.Fatalf("expected only p-old deleted (newest p-new wins), got %+v", dels)
	}
	// State adopts NEW's id + dns + connected.
	writes := store.stateWrites()
	if len(writes) != 1 {
		t.Fatalf("expected one state write, got %d: %+v", len(writes), writes)
	}
	want := stateWrite{id: "srv-1", domain: "new.netbird.cloud", peerID: "p-new", connected: true}
	if writes[0] != want {
		t.Fatalf("state write = %+v, want %+v", writes[0], want)
	}
}

// TestNetbirdSyncDedupTieLargestIDWins: equal last_seen -> the lexicographically
// largest peer id wins (deterministic tie-break). (Mutation guard: flip the
// tie-break to smallest-id -> p-zzz deleted -> this test fails.)
func TestNetbirdSyncDedupTieLargestIDWins(t *testing.T) {
	fake := newFakeNetbird()
	const same = "2026-07-25T10:00:00Z"
	fake.groups["g1"] = netbird.Group{ID: "g1", Name: "op-gw-srv-1", Peers: []netbird.GroupPeer{
		{ID: "p-aaa", Name: "srv-1"},
		{ID: "p-zzz", Name: "srv-1"},
	}}
	fake.peers["p-aaa"] = netbird.Peer{ID: "p-aaa", Name: "srv-1", DNSLabel: "aaa.netbird.cloud", Connected: true, LastSeen: same}
	fake.peers["p-zzz"] = netbird.Peer{ID: "p-zzz", Name: "srv-1", DNSLabel: "zzz.netbird.cloud", Connected: true, LastSeen: same}
	ts := httptest.NewServer(fake.handler())
	defer ts.Close()

	store := &fakeServerStore{servers: []routing.AIServer{{
		ID: "srv-1", Name: "srv-1", Domain: "aaa.netbird.cloud",
		NetbirdEnabled: true, NetbirdGroupID: "g1", NetbirdPeerID: "p-aaa", NetbirdConnected: true,
	}}}

	runNetbirdSyncOnce(context.Background(), store, settingsFor(ts.URL), netbirdCallTimeout, nowUTC, nil)

	if dels := fake.deleteCalls(); len(dels) != 1 || dels[0] != "p-aaa" {
		t.Fatalf("expected only p-aaa deleted (tie -> largest id p-zzz wins), got %+v", dels)
	}
	writes := store.stateWrites()
	if len(writes) != 1 || writes[0].peerID != "p-zzz" {
		t.Fatalf("expected state to adopt p-zzz, got %+v", writes)
	}
}

// TestNetbirdSyncDedupGetPeerErrorNoDelete: a GetPeer error on ANY group member
// aborts the dedup for this tick (full info required), so NOTHING is deleted and
// the resolved peer is used unchanged. The server is not broken. The group has
// THREE members (two readable + one failing) so that skipping-instead-of-aborting
// would wrongly delete a readable non-winner — making this a real mutation guard:
// drop the "any error -> abort" and a delete fires -> this test fails.
func TestNetbirdSyncDedupGetPeerErrorNoDelete(t *testing.T) {
	fake := newFakeNetbird()
	fake.failIDs["p-bad"] = true // GetPeer(p-bad) 500s
	fake.groups["g1"] = netbird.Group{ID: "g1", Name: "op-gw-srv-1", Peers: []netbird.GroupPeer{
		{ID: "p1", Name: "srv-1"},
		{ID: "p2", Name: "srv-1"},
		{ID: "p-bad", Name: "srv-1"},
	}}
	fake.peers["p1"] = netbird.Peer{ID: "p1", Name: "srv-1", DNSLabel: "p1.netbird.cloud", Connected: true, LastSeen: "2026-07-25T09:00:00Z"}
	fake.peers["p2"] = netbird.Peer{ID: "p2", Name: "srv-1", DNSLabel: "p2.netbird.cloud", Connected: true, LastSeen: "2026-07-25T10:00:00Z"}
	ts := httptest.NewServer(fake.handler())
	defer ts.Close()

	store := &fakeServerStore{servers: []routing.AIServer{{
		ID: "srv-1", Name: "srv-1", Domain: "p1.netbird.cloud",
		NetbirdEnabled: true, NetbirdGroupID: "g1", NetbirdPeerID: "p1", NetbirdConnected: true,
	}}}

	runNetbirdSyncOnce(context.Background(), store, settingsFor(ts.URL), netbirdCallTimeout, nowUTC, nil)

	if dels := fake.deleteCalls(); len(dels) != 0 {
		t.Fatalf("expected NO delete on partial GetPeer info, got %+v", dels)
	}
	// Resolved p1 is steady -> no spurious state write.
	if writes := store.stateWrites(); len(writes) != 0 {
		t.Fatalf("expected no state write (resolved p1 steady), got %+v", writes)
	}
}

// TestNetbirdSyncDedupSinglePeerNoDelete: a tracking group with exactly one peer
// is left untouched — no deletion, behavior unchanged. (Mutation guard for the
// <=1 short-circuit combined with never-delete-the-winner.)
func TestNetbirdSyncDedupSinglePeerNoDelete(t *testing.T) {
	fake := newFakeNetbird()
	fake.groups["g1"] = netbird.Group{ID: "g1", Name: "op-gw-srv-1", Peers: []netbird.GroupPeer{{ID: "p1", Name: "srv-1"}}}
	fake.peers["p1"] = netbird.Peer{ID: "p1", Name: "srv-1", DNSLabel: "p1.netbird.cloud", Connected: true, LastSeen: "2026-07-25T10:00:00Z"}
	ts := httptest.NewServer(fake.handler())
	defer ts.Close()

	store := &fakeServerStore{servers: []routing.AIServer{{
		ID: "srv-1", Name: "srv-1", Domain: "p1.netbird.cloud",
		NetbirdEnabled: true, NetbirdGroupID: "g1", NetbirdPeerID: "p1", NetbirdConnected: true,
	}}}

	runNetbirdSyncOnce(context.Background(), store, settingsFor(ts.URL), netbirdCallTimeout, nowUTC, nil)

	if dels := fake.deleteCalls(); len(dels) != 0 {
		t.Fatalf("expected NO delete for a single-peer tracking group, got %+v", dels)
	}
	if writes := store.stateWrites(); len(writes) != 0 {
		t.Fatalf("expected no state write (steady), got %+v", writes)
	}
}

// TestNetbirdSyncDedupNoGroupIDNoDedup: a manually-linked server (peer id set, NO
// tracking group id) is never deduped — the backstop only ever operates on the
// server's own tracking group. Even though a group with two peers exists in
// NetBird, no deletion happens because the server has no group id.
func TestNetbirdSyncDedupNoGroupIDNoDedup(t *testing.T) {
	fake := newFakeNetbird()
	// A group with two peers exists, but the server is NOT linked to it.
	fake.groups["g-other"] = netbird.Group{ID: "g-other", Name: "op-gw-other", Peers: []netbird.GroupPeer{
		{ID: "p-a", Name: "other"},
		{ID: "p-b", Name: "other"},
	}}
	fake.peers["p1"] = netbird.Peer{ID: "p1", Name: "srv-1", DNSLabel: "p1.netbird.cloud", Connected: true, LastSeen: "2026-07-25T10:00:00Z"}
	fake.peers["p-a"] = netbird.Peer{ID: "p-a", Name: "other", DNSLabel: "a.netbird.cloud", LastSeen: "2026-07-25T09:00:00Z"}
	fake.peers["p-b"] = netbird.Peer{ID: "p-b", Name: "other", DNSLabel: "b.netbird.cloud", LastSeen: "2026-07-25T11:00:00Z"}
	ts := httptest.NewServer(fake.handler())
	defer ts.Close()

	store := &fakeServerStore{servers: []routing.AIServer{{
		ID: "srv-1", Name: "srv-1", Domain: "p1.netbird.cloud",
		NetbirdEnabled: true, NetbirdGroupID: "", NetbirdPeerID: "p1", NetbirdConnected: true,
	}}}

	runNetbirdSyncOnce(context.Background(), store, settingsFor(ts.URL), netbirdCallTimeout, nowUTC, nil)

	if dels := fake.deleteCalls(); len(dels) != 0 {
		t.Fatalf("expected NO delete when the server has no tracking group id, got %+v", dels)
	}
	// The resolved p1 is steady -> no state write.
	if writes := store.stateWrites(); len(writes) != 0 {
		t.Fatalf("expected no state write, got %+v", writes)
	}
}

// --- Loop A online event (Task 6) -----------------------------------------

// TestNetbirdSyncOnlineEventFires exercises the false->true online-event detection
// in runNetbirdSyncOnce: it fires exactly once (with the server id) on a genuine
// transition, and NOT when the server was already connected, on a write that isn't
// a transition, or when the peer can't be resolved (no write at all).
func TestNetbirdSyncOnlineEventFires(t *testing.T) {
	t.Run("false-to-true fires once", func(t *testing.T) {
		fake := newFakeNetbird()
		// Name + domain already match -> only the connected flip drives the write.
		fake.peers["p1"] = netbird.Peer{ID: "p1", Name: "srv-1", DNSLabel: "p1.netbird.cloud", Connected: true}
		ts := httptest.NewServer(fake.handler())
		defer ts.Close()

		store := &fakeServerStore{servers: []routing.AIServer{{
			ID: "srv-1", Name: "srv-1", Domain: "p1.netbird.cloud",
			NetbirdEnabled: true, NetbirdPeerID: "p1", NetbirdConnected: false,
		}}}

		var online []string
		runNetbirdSyncOnce(context.Background(), store, settingsFor(ts.URL), netbirdCallTimeout, nowUTC, func(id string) {
			online = append(online, id)
		})

		if len(online) != 1 || online[0] != "srv-1" {
			t.Fatalf("expected one online event for srv-1, got %+v", online)
		}
	})

	t.Run("already connected does not fire", func(t *testing.T) {
		fake := newFakeNetbird()
		fake.peers["p1"] = netbird.Peer{ID: "p1", Name: "srv-1", DNSLabel: "p1.netbird.cloud", Connected: true}
		ts := httptest.NewServer(fake.handler())
		defer ts.Close()

		// Steady state (already connected) -> no write, no event.
		store := &fakeServerStore{servers: []routing.AIServer{{
			ID: "srv-1", Name: "srv-1", Domain: "p1.netbird.cloud",
			NetbirdEnabled: true, NetbirdPeerID: "p1", NetbirdConnected: true,
		}}}

		var online []string
		runNetbirdSyncOnce(context.Background(), store, settingsFor(ts.URL), netbirdCallTimeout, nowUTC, func(id string) {
			online = append(online, id)
		})

		if len(online) != 0 {
			t.Fatalf("expected no online event when already connected, got %+v", online)
		}
	})

	t.Run("write without transition does not fire", func(t *testing.T) {
		// Already connected, but the DNS/domain changed -> a state write fires, yet
		// there is NO false->true transition, so the online event must NOT fire.
		// (Mutation guard for the !wasConnected condition: drop it and this fails.)
		fake := newFakeNetbird()
		fake.peers["p1"] = netbird.Peer{ID: "p1", Name: "srv-1", DNSLabel: "new.netbird.cloud", Connected: true}
		ts := httptest.NewServer(fake.handler())
		defer ts.Close()

		store := &fakeServerStore{servers: []routing.AIServer{{
			ID: "srv-1", Name: "srv-1", Domain: "old.netbird.cloud",
			NetbirdEnabled: true, NetbirdPeerID: "p1", NetbirdConnected: true,
		}}}

		var online []string
		runNetbirdSyncOnce(context.Background(), store, settingsFor(ts.URL), netbirdCallTimeout, nowUTC, func(id string) {
			online = append(online, id)
		})

		if writes := store.stateWrites(); len(writes) != 1 {
			t.Fatalf("expected one state write (domain changed), got %+v", writes)
		}
		if len(online) != 0 {
			t.Fatalf("expected no online event without a false->true transition, got %+v", online)
		}
	})

	t.Run("resolve error does not fire and does not write", func(t *testing.T) {
		fake := newFakeNetbird()
		fake.failIDs["pbad"] = true // GetPeer(pbad) 500s; no group -> unresolved
		ts := httptest.NewServer(fake.handler())
		defer ts.Close()

		store := &fakeServerStore{servers: []routing.AIServer{{
			ID: "srv-1", Name: "srv-1", Domain: "good.local",
			NetbirdEnabled: true, NetbirdPeerID: "pbad", NetbirdConnected: false,
		}}}

		var online []string
		runNetbirdSyncOnce(context.Background(), store, settingsFor(ts.URL), netbirdCallTimeout, nowUTC, func(id string) {
			online = append(online, id)
		})

		if len(online) != 0 {
			t.Fatalf("expected no online event on resolve error, got %+v", online)
		}
		if writes := store.stateWrites(); len(writes) != 0 {
			t.Fatalf("expected no state write on resolve error, got %+v", writes)
		}
	})
}

// --- Loop B group+policy reconcile (Task 6) -------------------------------

// fakeReconciler records ReconcileAllServerNetbird + MaybeRotateNetbirdToken
// invocations (Loop B's unit of work). It satisfies netbirdReconciler.
type fakeReconciler struct {
	mu          sync.Mutex
	calls       int
	rotateCalls int
}

func (f *fakeReconciler) ReconcileAllServerNetbird(context.Context) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
}

func (f *fakeReconciler) MaybeRotateNetbirdToken(context.Context) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rotateCalls++
}

func (f *fakeReconciler) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeReconciler) rotateCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.rotateCalls
}

// fakeIntervalReader returns fixed peer/reconcile cadences, satisfying
// intervalReader so a loop test can drive a short interval deterministically.
type fakeIntervalReader struct {
	peer      time.Duration
	reconcile time.Duration
}

func (f fakeIntervalReader) NetbirdPolicySettings(context.Context, int) portal.NetbirdPolicySettings {
	return portal.NetbirdPolicySettings{PeerSyncInterval: f.peer, ReconcileInterval: f.reconcile}
}

// TestNetbirdReconcileOnceCallsReconcileAll: runNetbirdReconcileOnce delegates to
// the reconciler's fleet pass.
func TestNetbirdReconcileOnceCallsReconcileAll(t *testing.T) {
	f := &fakeReconciler{}
	runNetbirdReconcileOnce(context.Background(), f)
	if f.count() != 1 {
		t.Fatalf("expected ReconcileAllServerNetbird called once, got %d", f.count())
	}
}

// TestNetbirdReconcileOnceCallsMaybeRotateToken: runNetbirdReconcileOnce also
// invokes the token auto-rotation check exactly once per pass, alongside
// ReconcileAllServerNetbird (not instead of it).
func TestNetbirdReconcileOnceCallsMaybeRotateToken(t *testing.T) {
	f := &fakeReconciler{}
	runNetbirdReconcileOnce(context.Background(), f)
	if f.rotateCount() != 1 {
		t.Fatalf("expected MaybeRotateNetbirdToken called once, got %d", f.rotateCount())
	}
	if f.count() != 1 {
		t.Fatalf("expected ReconcileAllServerNetbird still called once, got %d", f.count())
	}
}

// loopIntervalWaitWindow bounds how long the two interval-reading loop tests wait
// to observe a second pass. It is deliberately SHORTER than the reader's "wrong"
// field (500ms) and MUCH longer than its "right" field (15ms), so a field-swap
// mutation (a loop reading the other field) cannot reach >=2 passes in time and
// the test fails, while the correct loop reliably does.
const loopIntervalWaitWindow = 250 * time.Millisecond

// TestNetbirdReconcileLoopReadsReconcileInterval: Loop B runs once immediately and
// then ticks on the reconcile interval from the reader. The reader returns a SHORT
// reconcile (15ms) and a clearly-longer peer (500ms), so observing >=2 passes
// within loopIntervalWaitWindow is only possible if Loop B uses its OWN
// (ReconcileInterval) field — swapping it to PeerSyncInterval makes this fail.
func TestNetbirdReconcileLoopReadsReconcileInterval(t *testing.T) {
	f := &fakeReconciler{}
	intervals := fakeIntervalReader{peer: 500 * time.Millisecond, reconcile: 15 * time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go runNetbirdReconcileLoop(ctx, f, intervals, 0)

	deadline := time.Now().Add(loopIntervalWaitWindow)
	for time.Now().Before(deadline) {
		if f.count() >= 2 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if f.count() < 2 {
		t.Fatalf("expected >=2 reconcile passes at the short reconcile cadence (immediate + tick), got %d", f.count())
	}
}

// TestNetbirdSyncLoopReadsPeerInterval: Loop A runs once immediately and then ticks
// on the peer interval from the reader. The reader returns a SHORT peer (15ms) and
// a clearly-longer reconcile (500ms); serverListCalls counts each pass's AIServers
// enumeration. >=2 within loopIntervalWaitWindow is only possible if Loop A uses
// its OWN (PeerSyncInterval) field — swapping it to ReconcileInterval makes this
// fail (only the immediate pass fits the window).
func TestNetbirdSyncLoopReadsPeerInterval(t *testing.T) {
	fake := newFakeNetbird()
	ts := httptest.NewServer(fake.handler())
	defer ts.Close()

	store := &fakeServerStore{} // no servers -> each pass just enumerates
	intervals := fakeIntervalReader{peer: 15 * time.Millisecond, reconcile: 500 * time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go runNetbirdSyncLoop(ctx, netbirdSyncDeps{store: store, settings: settingsFor(ts.URL), intervals: intervals, timeout: netbirdCallTimeout, now: nowUTC}, nil)

	deadline := time.Now().Add(loopIntervalWaitWindow)
	for time.Now().Before(deadline) {
		if store.serverListCalls() >= 2 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if store.serverListCalls() < 2 {
		t.Fatalf("expected >=2 sync passes at the short peer cadence (immediate + tick), got %d", store.serverListCalls())
	}
}

// TestNetbirdSyncLoopTriggerCausesExtraPass: a send on the trigger channel makes
// Loop A run an EXTRA pass immediately, independent of the ticker cadence. The
// interval reader here returns a LONG peer interval (far longer than the test's
// wait window) so the ticker can never fire during the test — the only way a
// second pass can be observed is via the trigger. (Mutation guard: drop the
// `case <-trigger` arm and this test fails — no second pass ever appears.)
func TestNetbirdSyncLoopTriggerCausesExtraPass(t *testing.T) {
	fake := newFakeNetbird()
	ts := httptest.NewServer(fake.handler())
	defer ts.Close()

	store := &fakeServerStore{} // no servers -> each pass just enumerates
	// A LONG interval on both cadences: the ticker must never fire within the
	// test's wait window, so only the trigger can produce a second pass.
	intervals := fakeIntervalReader{peer: time.Hour, reconcile: time.Hour}
	trigger := make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go runNetbirdSyncLoop(ctx, netbirdSyncDeps{store: store, settings: settingsFor(ts.URL), intervals: intervals, timeout: netbirdCallTimeout, now: nowUTC}, trigger)

	// Wait for the initial (immediate) pass to settle before triggering, so the
	// extra pass we observe is unambiguously caused by the trigger send.
	deadline := time.Now().Add(loopIntervalWaitWindow)
	for time.Now().Before(deadline) && store.serverListCalls() < 1 {
		time.Sleep(2 * time.Millisecond)
	}
	if store.serverListCalls() < 1 {
		t.Fatalf("expected the initial immediate pass to have run, got %d calls", store.serverListCalls())
	}

	trigger <- struct{}{}

	deadline = time.Now().Add(loopIntervalWaitWindow)
	for time.Now().Before(deadline) {
		if store.serverListCalls() >= 2 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if store.serverListCalls() < 2 {
		t.Fatalf("expected an extra pass after the trigger send, got %d calls", store.serverListCalls())
	}
}

// TestSyncServerNetbirdOnceReturnsConnected exercises the extracted single-server
// helper directly: an enrolled, already-correctly-named peer whose domain differs
// is reconciled (one state write, connected=true), and a non-NetBird server
// resolves to (false,false) with no call/write at all.
func TestSyncServerNetbirdOnceReturnsConnected(t *testing.T) {
	fake := newFakeNetbird()
	// Peer name == server name so no rename; domain differs so a state write occurs.
	fake.groups["g1"] = netbird.Group{ID: "g1", Name: "op-gw-srv-1", Peers: []netbird.GroupPeer{{ID: "p1", Name: "srv-1"}}}
	fake.peers["p1"] = netbird.Peer{ID: "p1", Name: "srv-1", DNSLabel: "p1.netbird.cloud", Connected: true}
	ts := httptest.NewServer(fake.handler())
	defer ts.Close()

	store := &fakeServerStore{}
	server := routing.AIServer{
		ID: "srv-1", Name: "srv-1", Domain: "old.local",
		NetbirdEnabled: true, NetbirdGroupID: "g1", NetbirdPeerID: "p1", NetbirdConnected: false,
	}
	ncfg := netbird.Config{URL: ts.URL, Token: "test-token"}

	connected, resolved := syncServerNetbirdOnce(context.Background(), ncfg, netbirdCallTimeout, store, server, nil)
	if !resolved || !connected {
		t.Fatalf("want resolved+connected, got resolved=%v connected=%v", resolved, connected)
	}
	writes := store.stateWrites()
	if len(writes) != 1 || !writes[0].connected {
		t.Fatalf("expected one state write with connected=true, got %+v", writes)
	}

	// A non-NetBird server resolves to (false,false) with no call/write — and,
	// critically, makes NO NetBird HTTP call at all (else deleting the skip guard
	// could still coincidentally read back (false,false) from a "" -> 404 lookup).
	before := fake.requestCount()
	off := routing.AIServer{ID: "srv-2", NetbirdEnabled: false}
	if c, r := syncServerNetbirdOnce(context.Background(), ncfg, netbirdCallTimeout, store, off, nil); c || r {
		t.Fatalf("non-netbird server must be (false,false), got (%v,%v)", c, r)
	}
	if after := fake.requestCount(); after != before {
		t.Fatalf("non-netbird server must make no NetBird calls, got %d extra", after-before)
	}
}
