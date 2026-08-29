// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/store"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeNetbird is a minimal NetBird admin-API stand-in for the create-hook +
// regenerate + test paths. It counts requests so a test can assert "no call was
// made", can be told to fail /api/setup-keys to exercise the non-blocking error
// path, and remembers created groups (GET reflects them) so a test can assert the
// tracking group is created idempotently (find-or-create, no duplicate).
type fakeNetbird struct {
	srv                *httptest.Server
	requests           int32
	failSetupKey       bool
	failGetPeer        bool // force GET /api/peers/{id} to 500 (best-effort reconcile path)
	failDeleteGroup    bool // force DELETE /api/groups/{id} to 500 (best-effort delete path)
	failDeletePeer     bool // force DELETE /api/peers/{id} to 500 (best-effort delete path)
	failDeleteSetupKey bool // force DELETE /api/setup-keys/{id} to 500 (best-effort delete path)
	keyValue           string

	failListPolicies bool // force GET /api/policies to 500 (best-effort reconcile path)
	failCreatePolicy bool // force POST /api/policies to 500 (best-effort reconcile path)

	mu               sync.Mutex
	groups           []map[string]any          // seeded with the module group "gateways"
	groupCreates     int32                     // count of POST /api/groups
	deletedGroups    []string                  // ids passed to DELETE /api/groups/{id}
	deletedPeers     []string                  // ids passed to DELETE /api/peers/{id}
	deletedSetupKeys []string                  // ids passed to DELETE /api/setup-keys/{id}
	peers            map[string]map[string]any // id -> peer json (for GetPeer / ListPeers)
	peerRenames      map[string]string         // peer id -> last name seen on a PUT rename
	lastAutoGroups   []string                  // auto_groups of the last POST /api/setup-keys

	// Policy CRUD state (T4 reconcile engine). policies is the fake's policy store
	// (read shape); the counters record List/Create/Update/Delete so a test can
	// assert the exact reconcile action.
	policies       []map[string]any // read-shape policies
	policyLists    int32            // count of GET /api/policies
	policyCreates  int32            // count of POST /api/policies
	policyUpdates  int32            // count of PUT /api/policies/{id}
	createdPolicy  []map[string]any // raw bodies of POST /api/policies
	updatedPolicy  []map[string]any // raw bodies of PUT /api/policies/{id}
	deletedPolicy  []string         // ids passed to DELETE /api/policies/{id}
	policyCreateID int              // running id suffix for created policies
}

func newFakeNetbird(t *testing.T) *fakeNetbird {
	t.Helper()
	f := &fakeNetbird{
		keyValue: "nbkey-secret-value",
		// The module group "gateways" already exists (ResolveGroupID finds it; also
		// serves the Ping in TestNetbird).
		groups:      []map[string]any{{"id": "g-mod", "name": "gateways"}},
		peers:       map[string]map[string]any{},
		peerRenames: map[string]string{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/groups", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&f.requests, 1)
		if r.Method == http.MethodGet {
			f.mu.Lock()
			snapshot := append([]map[string]any(nil), f.groups...)
			f.mu.Unlock()
			_ = json.NewEncoder(w).Encode(snapshot)
			return
		}
		var body struct {
			Name string `json:"name"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		// The first created group keeps the id "g-track" (existing tests assert it);
		// subsequent creates get a unique id. The created group is remembered so a
		// later GET (ResolveGroupID) finds it.
		n := atomic.AddInt32(&f.groupCreates, 1)
		id := "g-track"
		if n > 1 {
			id = fmt.Sprintf("g-track-%d", n)
		}
		grp := map[string]any{"id": id, "name": body.Name}
		f.mu.Lock()
		f.groups = append(f.groups, grp)
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(grp)
	})
	// GET /api/groups/{id} -> one group (with peers); PUT -> replace its peer list;
	// DELETE -> remove it (records the id; 500 when failDeleteGroup).
	mux.HandleFunc("/api/groups/", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&f.requests, 1)
		id := strings.TrimPrefix(r.URL.Path, "/api/groups/")
		switch r.Method {
		case http.MethodGet:
			f.mu.Lock()
			g := f.findGroupLocked(id)
			f.mu.Unlock()
			if g == nil {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_ = json.NewEncoder(w).Encode(g)
		case http.MethodPut:
			var body struct {
				Name  string   `json:"name"`
				Peers []string `json:"peers"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			peers := make([]map[string]any, 0, len(body.Peers))
			for _, pid := range body.Peers {
				peers = append(peers, map[string]any{"id": pid})
			}
			f.mu.Lock()
			if g := f.findGroupLocked(id); g != nil {
				g["peers"] = peers
			}
			f.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "name": body.Name, "peers": peers})
		case http.MethodDelete:
			if f.failDeleteGroup {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"message":"boom"}`))
				return
			}
			f.mu.Lock()
			f.deletedGroups = append(f.deletedGroups, id)
			next := make([]map[string]any, 0, len(f.groups))
			for _, g := range f.groups {
				if gid, _ := g["id"].(string); gid != id {
					next = append(next, g)
				}
			}
			f.groups = next
			f.mu.Unlock()
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})
	mux.HandleFunc("/api/setup-keys", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&f.requests, 1)
		var body struct {
			AutoGroups []string `json:"auto_groups"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.lastAutoGroups = body.AutoGroups
		f.mu.Unlock()
		if f.failSetupKey {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"message":"boom"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "sk-id", "key": f.keyValue})
	})
	// DELETE /api/setup-keys/{id} -> records the id (500 when failDeleteSetupKey).
	mux.HandleFunc("/api/setup-keys/", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&f.requests, 1)
		if r.Method != http.MethodDelete {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if f.failDeleteSetupKey {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"message":"boom"}`))
			return
		}
		id := strings.TrimPrefix(r.URL.Path, "/api/setup-keys/")
		f.mu.Lock()
		f.deletedSetupKeys = append(f.deletedSetupKeys, id)
		f.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	})
	// GET /api/peers -> list all peers (linkage-editor peer picker).
	mux.HandleFunc("/api/peers", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&f.requests, 1)
		f.mu.Lock()
		out := make([]map[string]any, 0, len(f.peers))
		for _, p := range f.peers {
			out = append(out, p)
		}
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(out)
	})
	// GET /api/peers/{id} -> one peer; PUT /api/peers/{id} -> rename (records it).
	mux.HandleFunc("/api/peers/", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&f.requests, 1)
		id := strings.TrimPrefix(r.URL.Path, "/api/peers/")
		if r.Method == http.MethodDelete {
			if f.failDeletePeer {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"message":"boom"}`))
				return
			}
			f.mu.Lock()
			f.deletedPeers = append(f.deletedPeers, id)
			delete(f.peers, id)
			f.mu.Unlock()
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.Method == http.MethodGet {
			if f.failGetPeer {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"message":"boom"}`))
				return
			}
			f.mu.Lock()
			p, ok := f.peers[id]
			f.mu.Unlock()
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_ = json.NewEncoder(w).Encode(p)
			return
		}
		// PUT rename: record the new name and reflect it in the returned peer.
		var body struct {
			Name string `json:"name"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.peerRenames[id] = body.Name
		if p, ok := f.peers[id]; ok {
			p["name"] = body.Name
			_ = json.NewEncoder(w).Encode(p)
			f.mu.Unlock()
			return
		}
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "name": body.Name})
	})
	// GET /api/policies -> list all policies; POST -> create (records the body).
	mux.HandleFunc("/api/policies", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&f.requests, 1)
		if r.Method == http.MethodGet {
			atomic.AddInt32(&f.policyLists, 1)
			if f.failListPolicies {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"message":"boom"}`))
				return
			}
			f.mu.Lock()
			snapshot := append([]map[string]any(nil), f.policies...)
			f.mu.Unlock()
			_ = json.NewEncoder(w).Encode(snapshot)
			return
		}
		// POST create.
		atomic.AddInt32(&f.policyCreates, 1)
		if f.failCreatePolicy {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"message":"boom"}`))
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.createdPolicy = append(f.createdPolicy, body)
		f.policyCreateID++
		id := fmt.Sprintf("pol-%d", f.policyCreateID)
		name, _ := body["name"].(string)
		created := map[string]any{"id": id, "name": name}
		f.policies = append(f.policies, created)
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(created)
	})
	// GET /api/policies/{id} -> one; PUT -> replace (records body); DELETE -> remove.
	mux.HandleFunc("/api/policies/", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&f.requests, 1)
		id := strings.TrimPrefix(r.URL.Path, "/api/policies/")
		switch r.Method {
		case http.MethodGet:
			f.mu.Lock()
			var found map[string]any
			for _, p := range f.policies {
				if pid, _ := p["id"].(string); pid == id {
					found = p
					break
				}
			}
			f.mu.Unlock()
			if found == nil {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_ = json.NewEncoder(w).Encode(found)
		case http.MethodPut:
			atomic.AddInt32(&f.policyUpdates, 1)
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.mu.Lock()
			f.updatedPolicy = append(f.updatedPolicy, body)
			f.mu.Unlock()
			body["id"] = id
			_ = json.NewEncoder(w).Encode(body)
		case http.MethodDelete:
			f.mu.Lock()
			f.deletedPolicy = append(f.deletedPolicy, id)
			next := make([]map[string]any, 0, len(f.policies))
			for _, p := range f.policies {
				if pid, _ := p["id"].(string); pid != id {
					next = append(next, p)
				}
			}
			f.policies = next
			f.mu.Unlock()
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

// seedPolicy adds a read-shape policy: one enabled accept/tcp rule with the given
// ports, source group, and destination group. The Description is set to the
// desired managed-policy description (matching desiredServerAccessPolicyRequest) so
// a caller seeding an otherwise-matching policy gets a true match; a test asserting
// description-drift overrides "description" on the returned policy afterward. Used
// to seed a "matching" or "stale" policy the reconcile engine should leave / update
// / delete.
func (f *fakeNetbird) seedPolicy(id, name string, enabled bool, ports []string, sourceID, destID string) {
	rulePorts := make([]any, 0, len(ports))
	for _, p := range ports {
		rulePorts = append(rulePorts, p)
	}
	desc := managedPolicyDescription(netbirdAccessPolicyPurpose)
	rule := map[string]any{
		"enabled":       true,
		"description":   desc,
		"action":        "accept",
		"bidirectional": false,
		"protocol":      "tcp",
		"ports":         rulePorts,
		"sources":       []any{map[string]any{"id": sourceID, "name": sourceID}},
		"destinations":  []any{map[string]any{"id": destID, "name": destID}},
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.policies = append(f.policies, map[string]any{"id": id, "name": name, "description": desc, "enabled": enabled, "rules": []any{rule}})
}

// seedDefaultPolicy adds NetBird's built-in "Default" policy with the given enabled state.
func (f *fakeNetbird) seedDefaultPolicy(id string, enabled bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.policies = append(f.policies, map[string]any{"id": id, "name": "Default", "enabled": enabled, "rules": []any{}})
}

func (f *fakeNetbird) policyListCount() int32   { return atomic.LoadInt32(&f.policyLists) }
func (f *fakeNetbird) policyCreateCount() int32 { return atomic.LoadInt32(&f.policyCreates) }
func (f *fakeNetbird) policyUpdateCount() int32 { return atomic.LoadInt32(&f.policyUpdates) }

// lastCreatedPolicy returns the raw body of the last POST /api/policies (nil if none).
func (f *fakeNetbird) lastCreatedPolicy() map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.createdPolicy) == 0 {
		return nil
	}
	return f.createdPolicy[len(f.createdPolicy)-1]
}

// policyCreateCountByName counts POST /api/policies bodies whose "name" equals the
// given name — so a fleet pass that creates several DISTINCT policies (e.g. a
// per-server op-gw-access-<id> AND the account-wide op-gw-agent-ingest) can be
// asserted per policy rather than on the total create count.
func (f *fakeNetbird) policyCreateCountByName(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, b := range f.createdPolicy {
		if bn, _ := b["name"].(string); bn == name {
			n++
		}
	}
	return n
}

// createdPolicyByName returns the raw body of the last POST /api/policies whose
// "name" equals name (nil if none).
func (f *fakeNetbird) createdPolicyByName(name string) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.createdPolicy) - 1; i >= 0; i-- {
		if bn, _ := f.createdPolicy[i]["name"].(string); bn == name {
			return f.createdPolicy[i]
		}
	}
	return nil
}

// updatedPolicyByName returns the raw body of the last PUT /api/policies/{id}
// whose "name" equals name (nil if none) — the PUT counterpart of
// createdPolicyByName above, for a pass that touches several distinct policies
// (a per-server op-gw-access-<id> AND the account-wide op-gw-agent-ingest) and
// must be asserted per policy rather than on whichever one happened to go last.
func (f *fakeNetbird) updatedPolicyByName(name string) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.updatedPolicy) - 1; i >= 0; i-- {
		if bn, _ := f.updatedPolicy[i]["name"].(string); bn == name {
			return f.updatedPolicy[i]
		}
	}
	return nil
}

// lastUpdatedPolicy returns the raw body of the last PUT /api/policies/{id} (nil if none).
func (f *fakeNetbird) lastUpdatedPolicy() map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.updatedPolicy) == 0 {
		return nil
	}
	return f.updatedPolicy[len(f.updatedPolicy)-1]
}

// wasPolicyDeleted reports whether DELETE /api/policies/{id} was called for id.
func (f *fakeNetbird) wasPolicyDeleted(id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, d := range f.deletedPolicy {
		if d == id {
			return true
		}
	}
	return false
}

func (f *fakeNetbird) deletedPolicyCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.deletedPolicy)
}

func (f *fakeNetbird) count() int32 { return atomic.LoadInt32(&f.requests) }

func (f *fakeNetbird) groupCreateCount() int32 { return atomic.LoadInt32(&f.groupCreates) }

// autoGroups returns the auto_groups sent on the last POST /api/setup-keys.
func (f *fakeNetbird) autoGroups() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.lastAutoGroups...)
}

// groupIDByName returns the id of the (seeded or created) group with the given name.
func (f *fakeNetbird) groupIDByName(name string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, g := range f.groups {
		if g["name"] == name {
			if id, ok := g["id"].(string); ok {
				return id
			}
		}
	}
	return ""
}

// setPeer seeds a peer the reconcile path can fetch/rename.
func (f *fakeNetbird) setPeer(id, name, dnsLabel string, connected bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.peers[id] = map[string]any{"id": id, "name": name, "dns_label": dnsLabel, "connected": connected}
}

// renameOf returns the last name a PUT /api/peers/{id} rename set for id.
func (f *fakeNetbird) renameOf(id string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n, ok := f.peerRenames[id]
	return n, ok
}

// setPeerWithGroups seeds a peer whose GetPeer reports the given group refs.
func (f *fakeNetbird) setPeerWithGroups(id, name, dnsLabel string, connected bool, groups []map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.peers[id] = map[string]any{"id": id, "name": name, "dns_label": dnsLabel, "connected": connected, "groups": groups}
}

// seedGroup adds a group (id,name) with the given peer ids as its members.
func (f *fakeNetbird) seedGroup(id, name string, peerIDs ...string) {
	peers := make([]map[string]any, 0, len(peerIDs))
	for _, pid := range peerIDs {
		peers = append(peers, map[string]any{"id": pid})
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.groups = append(f.groups, map[string]any{"id": id, "name": name, "peers": peers})
}

// findGroupLocked returns the group map with the given id (caller holds f.mu).
func (f *fakeNetbird) findGroupLocked(id string) map[string]any {
	for _, g := range f.groups {
		if gid, _ := g["id"].(string); gid == id {
			return g
		}
	}
	return nil
}

// groupMembers returns the current peer ids of the group (for membership asserts).
func (f *fakeNetbird) groupMembers(id string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	g := f.findGroupLocked(id)
	if g == nil {
		return nil
	}
	peers, _ := g["peers"].([]map[string]any)
	out := make([]string, 0, len(peers))
	for _, p := range peers {
		if pid, ok := p["id"].(string); ok {
			out = append(out, pid)
		}
	}
	return out
}

// groupHasPeer reports whether the group currently contains the peer id.
func (f *fakeNetbird) groupHasPeer(groupID, peerID string) bool {
	for _, pid := range f.groupMembers(groupID) {
		if pid == peerID {
			return true
		}
	}
	return false
}

// wasGroupDeleted reports whether DELETE /api/groups/{id} was called for id.
func (f *fakeNetbird) wasGroupDeleted(id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, d := range f.deletedGroups {
		if d == id {
			return true
		}
	}
	return false
}

// deletedPeerCount returns how many DELETE /api/peers/{id} calls were made.
func (f *fakeNetbird) deletedPeerCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.deletedPeers)
}

// wasPeerDeleted reports whether DELETE /api/peers/{id} was called for id.
func (f *fakeNetbird) wasPeerDeleted(id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, d := range f.deletedPeers {
		if d == id {
			return true
		}
	}
	return false
}

// wasSetupKeyDeleted reports whether DELETE /api/setup-keys/{id} was called for id.
func (f *fakeNetbird) wasSetupKeyDeleted(id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, d := range f.deletedSetupKeys {
		if d == id {
			return true
		}
	}
	return false
}

func newNetbirdServerTestService(t *testing.T, now time.Time) (*Service, *routing.MemoryStore) {
	t.Helper()
	dir := NewMemoryDirectory(auth.NewTokenStore())
	for _, u := range []string{"usr_admin", "usr_owner", "usr_other"} {
		if err := dir.CreateUser(context.Background(), store.User{ID: u, Email: u + "@example.test", DisplayName: u, Role: "user", Status: store.UserStatusActive, PreferredLanguage: "de", CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("CreateUser %s: %v", u, err)
		}
	}
	seedServerTestGroups(t, dir, now)
	routeStore := routing.NewMemoryStore()
	svc := NewService(ServiceDeps{Users: dir, Groups: dir, Routes: routeStore, SystemSettings: NewMemorySystemSettings(), Cipher: newTestCipher(t), Clock: func() time.Time { return now }})
	return svc, routeStore
}

func enableNetbird(t *testing.T, svc *Service, url string, enabled bool) {
	t.Helper()
	if _, err := svc.UpdateSystemSettings(context.Background(), systemToken(), UpdateSystemSettingsRequest{
		NetbirdEnabled: boolPtr(enabled),
		NetbirdURL:     strPtr(url),
		NetbirdGroups:  &[]string{"gateways"},
		NetbirdToken:   strPtr("nbtok"),
	}); err != nil {
		t.Fatalf("configure netbird: %v", err)
	}
}

// TestCreateServerNetbirdHookSuccess: with the module enabled + the flag set, the
// create hook generates a setup key (returned display-once) and records the key
// id + tracking-group id on the persisted server. Domain is optional.
func TestCreateServerNetbirdHookSuccess(t *testing.T) {
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	svc, routeStore := newNetbirdServerTestService(t, now)
	fake := newFakeNetbird(t)
	enableNetbird(t, svc, fake.srv.URL, true)

	dto, err := svc.CreateServer(context.Background(), adminToken(), CreateServerRequest{
		Name: "GPU 1", NetbirdEnabled: true, OwnerIDs: []string{"usr_owner"},
		AdminGroupIDs: []string{testAdminGroupID},
	})
	if err != nil {
		t.Fatalf("CreateServer (netbird, no domain): %v", err)
	}
	if !dto.NetbirdEnabled {
		t.Fatalf("dto.NetbirdEnabled = false, want true")
	}
	if dto.NetbirdSetupKey != "nbkey-secret-value" {
		t.Fatalf("dto.NetbirdSetupKey = %q, want the generated key", dto.NetbirdSetupKey)
	}
	if dto.NetbirdError != "" {
		t.Fatalf("dto.NetbirdError = %q, want empty", dto.NetbirdError)
	}
	if dto.NetbirdSetupKeyID != "sk-id" {
		t.Fatalf("dto.NetbirdSetupKeyID = %q, want sk-id", dto.NetbirdSetupKeyID)
	}
	stored, err := routeStore.AIServerByID(context.Background(), dto.ID)
	if err != nil {
		t.Fatalf("AIServerByID: %v", err)
	}
	if !stored.NetbirdEnabled || stored.NetbirdSetupKeyID != "sk-id" || stored.NetbirdGroupID != "g-track" {
		t.Fatalf("stored server = {enabled:%v key:%q grp:%q}, want {true sk-id g-track}", stored.NetbirdEnabled, stored.NetbirdSetupKeyID, stored.NetbirdGroupID)
	}
	// The setup-key VALUE is never persisted (no column, no write).
	if stored.NetbirdPeerID != "" || stored.NetbirdConnected {
		t.Fatalf("stored peer/connected should be zero, got %q/%v", stored.NetbirdPeerID, stored.NetbirdConnected)
	}
}

// TestCreateServerNetbirdModuleDisabledSkips: with the module disabled, a
// netbird_enabled request forces the flag false, makes NO netbird call, and the
// domain is required as usual.
func TestCreateServerNetbirdModuleDisabledSkips(t *testing.T) {
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	svc, routeStore := newNetbirdServerTestService(t, now)
	fake := newFakeNetbird(t)
	// Configured but DISABLED: if the hook wrongly ran it would hit the fake.
	enableNetbird(t, svc, fake.srv.URL, false)

	// Module off => netbird flag forced false => domain is required.
	if _, err := svc.CreateServer(context.Background(), adminToken(), CreateServerRequest{
		Name: "NoDomain", NetbirdEnabled: true, OwnerIDs: []string{"usr_owner"},
	}); !errors.Is(err, ErrServerDomainRequired) {
		t.Fatalf("module-off + empty domain = %v, want ErrServerDomainRequired", err)
	}

	dto, err := svc.CreateServer(context.Background(), adminToken(), CreateServerRequest{
		Name: "S", Domain: "s.local", NetbirdEnabled: true, OwnerIDs: []string{"usr_owner"},
		AdminGroupIDs: []string{testAdminGroupID},
	})
	if err != nil {
		t.Fatalf("CreateServer: %v", err)
	}
	if dto.NetbirdEnabled {
		t.Fatalf("dto.NetbirdEnabled = true, want forced false (module off)")
	}
	if dto.NetbirdSetupKey != "" || dto.NetbirdError != "" {
		t.Fatalf("dto netbird key/error should be empty, got %q/%q", dto.NetbirdSetupKey, dto.NetbirdError)
	}
	if fake.count() != 0 {
		t.Fatalf("netbird requests = %d, want 0 (module off => no call)", fake.count())
	}
	stored, _ := routeStore.AIServerByID(context.Background(), dto.ID)
	if stored.NetbirdEnabled {
		t.Fatalf("stored NetbirdEnabled = true, want false")
	}
}

// TestCreateServerNetbirdHookErrorStillCreates: a NetBird failure during the hook
// never fails the create — the server persists (flagged) and NetbirdError is set.
func TestCreateServerNetbirdHookErrorStillCreates(t *testing.T) {
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	svc, routeStore := newNetbirdServerTestService(t, now)
	fake := newFakeNetbird(t)
	fake.failSetupKey = true
	enableNetbird(t, svc, fake.srv.URL, true)

	dto, err := svc.CreateServer(context.Background(), adminToken(), CreateServerRequest{
		Name: "S", NetbirdEnabled: true, OwnerIDs: []string{"usr_owner"},
		AdminGroupIDs: []string{testAdminGroupID},
	})
	if err != nil {
		t.Fatalf("CreateServer must not fail on a netbird error: %v", err)
	}
	if dto.NetbirdError == "" {
		t.Fatalf("dto.NetbirdError = empty, want a failure message")
	}
	if dto.NetbirdSetupKey != "" {
		t.Fatalf("dto.NetbirdSetupKey = %q, want empty on failure", dto.NetbirdSetupKey)
	}
	stored, err := routeStore.AIServerByID(context.Background(), dto.ID)
	if err != nil {
		t.Fatalf("server must still exist: %v", err)
	}
	if !stored.NetbirdEnabled {
		t.Fatalf("stored NetbirdEnabled = false, want true (flag persisted)")
	}
}

// TestRegenerateNetbirdKeyAuthAndState: owner/admin can regenerate (no-leak 404
// for non-owner/unknown).
func TestRegenerateNetbirdKeyAuthAndState(t *testing.T) {
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	svc, _ := newNetbirdServerTestService(t, now)
	fake := newFakeNetbird(t)
	enableNetbird(t, svc, fake.srv.URL, true)

	nb, err := svc.CreateServer(context.Background(), adminToken(), CreateServerRequest{
		Name: "NB", NetbirdEnabled: true, OwnerIDs: []string{"usr_owner"},
		AdminGroupIDs: []string{testAdminGroupID},
	})
	if err != nil {
		t.Fatalf("CreateServer: %v", err)
	}

	// Owner may regenerate.
	key, command, err := svc.RegenerateNetbirdKey(context.Background(), ownerToken(), nb.ID)
	if err != nil || key != "nbkey-secret-value" {
		t.Fatalf("RegenerateNetbirdKey(owner) = %q, %v; want the key, nil", key, err)
	}
	// The response carries the ready-to-paste console command with the key.
	if want := "netbird up --management-url " + fake.srv.URL + " --setup-key nbkey-secret-value"; command != want {
		t.Fatalf("RegenerateNetbirdKey command = %q, want %q", command, want)
	}

	// Non-owner gets a no-leak not-found (not forbidden, not a different error).
	if _, _, err := svc.RegenerateNetbirdKey(context.Background(), otherToken(), nb.ID); !errors.Is(err, ErrServerNotFound) {
		t.Fatalf("RegenerateNetbirdKey(non-owner) = %v, want ErrServerNotFound", err)
	}
	// Unknown id -> not found.
	if _, _, err := svc.RegenerateNetbirdKey(context.Background(), systemAdminToken(), "srv_missing"); !errors.Is(err, ErrServerNotFound) {
		t.Fatalf("RegenerateNetbirdKey(unknown) = %v, want ErrServerNotFound", err)
	}
}

// TestRegenerateNetbirdKeyEnrollsNonNetbirdServer: calling the setup-key endpoint
// on a NON-NetBird server ENROLLS it — it returns a fresh key, flips
// netbird_enabled true, and records a tracking group (created for the server).
func TestRegenerateNetbirdKeyEnrollsNonNetbirdServer(t *testing.T) {
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	svc, routeStore := newNetbirdServerTestService(t, now)
	fake := newFakeNetbird(t)
	enableNetbird(t, svc, fake.srv.URL, true)

	// A plain (non-NetBird) server.
	plain, err := svc.CreateServer(context.Background(), adminToken(), CreateServerRequest{
		Name: "Plain", Domain: "p.local", OwnerIDs: []string{"usr_owner"},
		AdminGroupIDs: []string{testAdminGroupID},
	})
	if err != nil {
		t.Fatalf("CreateServer (plain): %v", err)
	}
	before, _ := routeStore.AIServerByID(context.Background(), plain.ID)
	if before.NetbirdEnabled || before.NetbirdGroupID != "" {
		t.Fatalf("precondition: server already netbird-enabled/grouped: %+v", before)
	}

	// Enroll: owner triggers the setup-key endpoint on the non-netbird server.
	key, _, err := svc.RegenerateNetbirdKey(context.Background(), ownerToken(), plain.ID)
	if err != nil || key != "nbkey-secret-value" {
		t.Fatalf("RegenerateNetbirdKey(enroll) = %q, %v; want the key, nil", key, err)
	}
	stored, err := routeStore.AIServerByID(context.Background(), plain.ID)
	if err != nil {
		t.Fatalf("AIServerByID: %v", err)
	}
	if !stored.NetbirdEnabled {
		t.Fatalf("after enroll NetbirdEnabled = false, want true (flag flipped)")
	}
	if stored.NetbirdGroupID == "" || stored.NetbirdSetupKeyID == "" {
		t.Fatalf("after enroll group/key not recorded: grp=%q key=%q", stored.NetbirdGroupID, stored.NetbirdSetupKeyID)
	}
	if fake.groupCreateCount() != 1 {
		t.Fatalf("group creates after enroll = %d, want 1 (tracking group created)", fake.groupCreateCount())
	}
}

// TestSetServerNetbird covers the system-admin linkage editor: it sets
// enabled+peer, resets connected, applies the disable-with-empty-domain guard,
// and returns a no-leak not-found for an unknown id.
func TestSetServerNetbird(t *testing.T) {
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	svc, routeStore := newNetbirdServerTestService(t, now)

	// A plain server with a domain; pretend it was previously connected.
	plain, err := svc.CreateServer(context.Background(), adminToken(), CreateServerRequest{
		Name: "S", Domain: "s.local", OwnerIDs: []string{"usr_owner"},
		AdminGroupIDs: []string{testAdminGroupID},
	})
	if err != nil {
		t.Fatalf("CreateServer: %v", err)
	}
	if err := routeStore.UpdateServerNetbirdState(context.Background(), plain.ID, "s.local", "peer-old", true); err != nil {
		t.Fatalf("seed connected state: %v", err)
	}

	// Link a manually-created peer: enabled=true + peer id, connected reset.
	dto, err := svc.SetServerNetbird(context.Background(), systemToken(), plain.ID, true, "  peer-manual  ", nil, false, "", false, false)
	if err != nil {
		t.Fatalf("SetServerNetbird(enable+peer): %v", err)
	}
	if !dto.NetbirdEnabled || dto.NetbirdPeerID != "peer-manual" || dto.NetbirdConnected {
		t.Fatalf("dto = {enabled:%v peer:%q conn:%v}, want {true peer-manual false}", dto.NetbirdEnabled, dto.NetbirdPeerID, dto.NetbirdConnected)
	}
	stored, _ := routeStore.AIServerByID(context.Background(), plain.ID)
	if !stored.NetbirdEnabled || stored.NetbirdPeerID != "peer-manual" || stored.NetbirdConnected {
		t.Fatalf("stored = {enabled:%v peer:%q conn:%v}, want {true peer-manual false}", stored.NetbirdEnabled, stored.NetbirdPeerID, stored.NetbirdConnected)
	}

	// Disable on a server that still has a domain -> OK (peer cleared).
	dto, err = svc.SetServerNetbird(context.Background(), systemToken(), plain.ID, false, "", nil, false, "", false, false)
	if err != nil {
		t.Fatalf("SetServerNetbird(disable w/ domain): %v", err)
	}
	if dto.NetbirdEnabled || dto.NetbirdPeerID != "" {
		t.Fatalf("dto after disable = {enabled:%v peer:%q}, want {false \"\"}", dto.NetbirdEnabled, dto.NetbirdPeerID)
	}

	// Domain guard: disabling NetBird on a domainless server is rejected.
	nb := routing.AIServer{ID: "srv_nodomain", Name: "NB", NetbirdEnabled: true, NetbirdPeerID: "p1", CreatedAt: now, UpdatedAt: now}
	if err := routeStore.CreateAIServer(context.Background(), nb); err != nil {
		t.Fatalf("CreateAIServer (domainless): %v", err)
	}
	if _, err := svc.SetServerNetbird(context.Background(), systemToken(), nb.ID, false, "", nil, false, "", false, false); !errors.Is(err, ErrServerDomainRequired) {
		t.Fatalf("SetServerNetbird(disable domainless) = %v, want ErrServerDomainRequired", err)
	}

	// Unknown id -> store not-found (no-leak 404 at the handler).
	if _, err := svc.SetServerNetbird(context.Background(), systemToken(), "srv_missing", true, "p", nil, false, "", false, false); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("SetServerNetbird(unknown) = %v, want store.ErrNotFound", err)
	}
}

// TestSetServerNetbirdForbidsNonSystem proves the PT-2 Part 2 internal authz
// guard: a principal without the "system" scope (even the server's own
// admin/owner) is rejected with ErrPrincipalForbidden and the server is left
// untouched -- the HTTP-level gate (requireWebScope("system") in
// handleSystemServerNetbird) is defense-in-depth on TOP of this, not instead
// of it. This is a stricter gate than authorizeServer's owner-or-admin-group
// check used elsewhere on the same server: SetServerNetbird is system-only.
func TestSetServerNetbirdForbidsNonSystem(t *testing.T) {
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	svc, routeStore := newNetbirdServerTestService(t, now)

	plain, err := svc.CreateServer(context.Background(), adminToken(), CreateServerRequest{
		Name: "S", Domain: "s.local", OwnerIDs: []string{"usr_owner"},
		AdminGroupIDs: []string{testAdminGroupID},
	})
	if err != nil {
		t.Fatalf("CreateServer: %v", err)
	}

	for _, tc := range []struct {
		name string
		tok  auth.Token
	}{
		{"owner", ownerToken()},
		{"plain admin (no system scope)", adminToken()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := svc.SetServerNetbird(context.Background(), tc.tok, plain.ID, true, "peer-x", nil, false, "", false, false); !errors.Is(err, ErrPrincipalForbidden) {
				t.Fatalf("SetServerNetbird(%s) err = %v, want ErrPrincipalForbidden", tc.name, err)
			}
		})
	}
	stored, err := routeStore.AIServerByID(context.Background(), plain.ID)
	if err != nil {
		t.Fatalf("AIServerByID: %v", err)
	}
	if stored.NetbirdEnabled || stored.NetbirdPeerID != "" {
		t.Fatalf("server mutated despite ErrPrincipalForbidden: %+v", stored)
	}
}

// TestSetServerNetbirdAllowsSystem proves the flip side of the same guard: a
// system-scoped principal succeeds exactly as before the PT-2 Part 2 guard
// was added.
func TestSetServerNetbirdAllowsSystem(t *testing.T) {
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	svc, routeStore := newNetbirdServerTestService(t, now)

	plain, err := svc.CreateServer(context.Background(), adminToken(), CreateServerRequest{
		Name: "S", Domain: "s.local", OwnerIDs: []string{"usr_owner"},
		AdminGroupIDs: []string{testAdminGroupID},
	})
	if err != nil {
		t.Fatalf("CreateServer: %v", err)
	}
	if _, err := svc.SetServerNetbird(context.Background(), systemToken(), plain.ID, true, "peer-x", nil, false, "", false, false); err != nil {
		t.Fatalf("SetServerNetbird(system): %v", err)
	}
	stored, err := routeStore.AIServerByID(context.Background(), plain.ID)
	if err != nil {
		t.Fatalf("AIServerByID: %v", err)
	}
	if !stored.NetbirdEnabled || stored.NetbirdPeerID != "peer-x" {
		t.Fatalf("stored = {enabled:%v peer:%q}, want {true peer-x}", stored.NetbirdEnabled, stored.NetbirdPeerID)
	}
}

// TestSetServerNetbirdPeerUniqueness: a peer id already linked to ANOTHER server
// is rejected (ErrNetbirdPeerInUse); re-linking the same peer to the SAME server
// is allowed (the current server id is excluded from the scan). The module is off
// so no reconcile runs — this exercises the uniqueness check in isolation.
func TestSetServerNetbirdPeerUniqueness(t *testing.T) {
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	svc, _ := newNetbirdServerTestService(t, now)

	a, err := svc.CreateServer(context.Background(), adminToken(), CreateServerRequest{
		Name: "A", Domain: "a.local", OwnerIDs: []string{"usr_owner"},
		AdminGroupIDs: []string{testAdminGroupID},
	})
	if err != nil {
		t.Fatalf("CreateServer A: %v", err)
	}
	b, err := svc.CreateServer(context.Background(), adminToken(), CreateServerRequest{
		Name: "B", Domain: "b.local", OwnerIDs: []string{"usr_owner"},
		AdminGroupIDs: []string{testAdminGroupID},
	})
	if err != nil {
		t.Fatalf("CreateServer B: %v", err)
	}

	// Link p1 to A.
	if _, err := svc.SetServerNetbird(context.Background(), systemToken(), a.ID, true, "p1", nil, false, "", false, false); err != nil {
		t.Fatalf("link p1 to A: %v", err)
	}
	// Linking the SAME peer to B is rejected.
	if _, err := svc.SetServerNetbird(context.Background(), systemToken(), b.ID, true, "p1", nil, false, "", false, false); !errors.Is(err, ErrNetbirdPeerInUse) {
		t.Fatalf("link p1 to B = %v, want ErrNetbirdPeerInUse", err)
	}
	// Re-linking p1 to A (same server) is allowed.
	if _, err := svc.SetServerNetbird(context.Background(), systemToken(), a.ID, true, "p1", nil, false, "", false, false); err != nil {
		t.Fatalf("re-link p1 to A (same server): %v", err)
	}
}

// TestSetServerNetbirdReconcile: linking a peer runs a synchronous best-effort
// reconcile — it sets the server domain to the peer's dns_label + connected NOW
// and renames the peer to the server name.
func TestSetServerNetbirdReconcile(t *testing.T) {
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	svc, routeStore := newNetbirdServerTestService(t, now)
	fake := newFakeNetbird(t)
	enableNetbird(t, svc, fake.srv.URL, true)
	fake.setPeer("peer-r", "old-peer-name", "host.netbird.io", true)

	srv, err := svc.CreateServer(context.Background(), adminToken(), CreateServerRequest{
		Name: "GPU", Domain: "old.local", OwnerIDs: []string{"usr_owner"},
		AdminGroupIDs: []string{testAdminGroupID},
	})
	if err != nil {
		t.Fatalf("CreateServer: %v", err)
	}

	dto, err := svc.SetServerNetbird(context.Background(), systemToken(), srv.ID, true, "peer-r", nil, false, "", false, false)
	if err != nil {
		t.Fatalf("SetServerNetbird(link + reconcile): %v", err)
	}
	if dto.Domain != "host.netbird.io" {
		t.Fatalf("dto.Domain = %q, want host.netbird.io (from dns_label)", dto.Domain)
	}
	if !dto.NetbirdConnected {
		t.Fatalf("dto.NetbirdConnected = false, want true (reconciled from the peer)")
	}
	if dto.NetbirdPeerID != "peer-r" {
		t.Fatalf("dto.NetbirdPeerID = %q, want peer-r", dto.NetbirdPeerID)
	}
	// The fake saw a rename PUT to the server name.
	if name, ok := fake.renameOf("peer-r"); !ok || name != "GPU" {
		t.Fatalf("peer rename = %q (ok=%v), want GPU", name, ok)
	}
	stored, _ := routeStore.AIServerByID(context.Background(), srv.ID)
	if stored.Domain != "host.netbird.io" || !stored.NetbirdConnected {
		t.Fatalf("stored = {domain:%q connected:%v}, want {host.netbird.io true}", stored.Domain, stored.NetbirdConnected)
	}
}

// TestSetServerNetbirdReconcileBestEffort: a NetBird error during the reconcile
// (GetPeer 500) leaves the link SAVED, the domain UNCHANGED, and returns no error
// — the sync loop reconciles later.
func TestSetServerNetbirdReconcileBestEffort(t *testing.T) {
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	svc, routeStore := newNetbirdServerTestService(t, now)
	fake := newFakeNetbird(t)
	fake.failGetPeer = true
	enableNetbird(t, svc, fake.srv.URL, true)

	srv, err := svc.CreateServer(context.Background(), adminToken(), CreateServerRequest{
		Name: "S", Domain: "keep.local", OwnerIDs: []string{"usr_owner"},
		AdminGroupIDs: []string{testAdminGroupID},
	})
	if err != nil {
		t.Fatalf("CreateServer: %v", err)
	}

	dto, err := svc.SetServerNetbird(context.Background(), systemToken(), srv.ID, true, "peer-x", nil, false, "", false, false)
	if err != nil {
		t.Fatalf("SetServerNetbird must not fail on a netbird error: %v", err)
	}
	if !dto.NetbirdEnabled || dto.NetbirdPeerID != "peer-x" {
		t.Fatalf("dto = {enabled:%v peer:%q}, want {true peer-x} (link saved)", dto.NetbirdEnabled, dto.NetbirdPeerID)
	}
	if dto.Domain != "keep.local" {
		t.Fatalf("dto.Domain = %q, want keep.local (unchanged — not clobbered)", dto.Domain)
	}
	stored, _ := routeStore.AIServerByID(context.Background(), srv.ID)
	if !stored.NetbirdEnabled || stored.NetbirdPeerID != "peer-x" || stored.Domain != "keep.local" {
		t.Fatalf("stored = {enabled:%v peer:%q domain:%q}, want {true peer-x keep.local}", stored.NetbirdEnabled, stored.NetbirdPeerID, stored.Domain)
	}
}

// TestNetbirdPeers: module off -> sentinel; module on -> the account peers.
func TestNetbirdPeers(t *testing.T) {
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	svc, _ := newNetbirdServerTestService(t, now)
	if _, err := svc.NetbirdPeers(context.Background()); !errors.Is(err, ErrNetbirdModuleDisabled) {
		t.Fatalf("NetbirdPeers(module off) = %v, want ErrNetbirdModuleDisabled", err)
	}
	fake := newFakeNetbird(t)
	enableNetbird(t, svc, fake.srv.URL, true)
	fake.setPeer("peer-1", "gpu-box", "gpu-box.netbird.io", true)

	peers, err := svc.NetbirdPeers(context.Background())
	if err != nil {
		t.Fatalf("NetbirdPeers = %v, want nil", err)
	}
	found := false
	for _, p := range peers {
		if p.ID == "peer-1" && p.Name == "gpu-box" && p.DNSLabel == "gpu-box.netbird.io" && p.Connected {
			found = true
		}
	}
	if !found {
		t.Fatalf("NetbirdPeers = %+v, want the seeded peer {peer-1 gpu-box}", peers)
	}
}

// TestResolveGatewayPeerIP: no selected peer → ("", nil); a selected peer with
// the module off → ErrNetbirdModuleDisabled; module on → the peer's IP.
func TestResolveGatewayPeerIP(t *testing.T) {
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	svc, _ := newNetbirdServerTestService(t, now)

	// No gateway peer selected → ("", nil), no NetBird call.
	if ip, err := svc.ResolveGatewayPeerIP(context.Background()); ip != "" || err != nil {
		t.Fatalf("ResolveGatewayPeerIP(no peer) = (%q, %v), want (\"\", nil)", ip, err)
	}

	// Select a peer but leave the module OFF → error (module off).
	if _, err := svc.UpdateSystemSettings(context.Background(), systemToken(), UpdateSystemSettingsRequest{
		NetbirdGatewayPeerID: strPtr("peer-1"),
	}); err != nil {
		t.Fatalf("select peer: %v", err)
	}
	if _, err := svc.ResolveGatewayPeerIP(context.Background()); !errors.Is(err, ErrNetbirdModuleDisabled) {
		t.Fatalf("ResolveGatewayPeerIP(module off) = %v, want ErrNetbirdModuleDisabled", err)
	}

	// Enable the module + seed the peer with a NetBird IP → returns that IP.
	fake := newFakeNetbird(t)
	enableNetbird(t, svc, fake.srv.URL, true)
	fake.mu.Lock()
	fake.peers["peer-1"] = map[string]any{"id": "peer-1", "name": "gw", "dns_label": "gw.netbird.io", "ip": "100.92.0.7", "connected": true}
	fake.mu.Unlock()

	ip, err := svc.ResolveGatewayPeerIP(context.Background())
	if err != nil {
		t.Fatalf("ResolveGatewayPeerIP = %v, want nil", err)
	}
	if ip != "100.92.0.7" {
		t.Fatalf("ResolveGatewayPeerIP ip = %q, want 100.92.0.7", ip)
	}
}

// TestRegenerateNetbirdKeyModuleDisabled: when the module is off, regenerate on a
// (previously) flagged server yields ErrNetbirdModuleDisabled.
func TestRegenerateNetbirdKeyModuleDisabled(t *testing.T) {
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	svc, _ := newNetbirdServerTestService(t, now)
	fake := newFakeNetbird(t)
	enableNetbird(t, svc, fake.srv.URL, true)
	nb, err := svc.CreateServer(context.Background(), adminToken(), CreateServerRequest{
		Name: "NB", NetbirdEnabled: true, OwnerIDs: []string{"usr_owner"},
		AdminGroupIDs: []string{testAdminGroupID},
	})
	if err != nil {
		t.Fatalf("CreateServer: %v", err)
	}
	// Disable the module.
	enableNetbird(t, svc, fake.srv.URL, false)
	if _, _, err := svc.RegenerateNetbirdKey(context.Background(), systemAdminToken(), nb.ID); !errors.Is(err, ErrNetbirdModuleDisabled) {
		t.Fatalf("RegenerateNetbirdKey(module off) = %v, want ErrNetbirdModuleDisabled", err)
	}
}

// TestUpdateServerDisableNetbirdRequiresDomain: disabling NetBird on an unsynced
// (empty-domain) server without supplying a domain must 400 (a domainless,
// unmanaged server the sync would ignore); the same PATCH WITH a domain succeeds.
func TestUpdateServerDisableNetbirdRequiresDomain(t *testing.T) {
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	svc, _ := newNetbirdServerTestService(t, now)
	fake := newFakeNetbird(t)
	enableNetbird(t, svc, fake.srv.URL, true)

	// A NetBird server created without a domain (auto-managed; empty until synced).
	nb, err := svc.CreateServer(context.Background(), adminToken(), CreateServerRequest{
		Name: "NB", NetbirdEnabled: true, OwnerIDs: []string{"usr_owner"},
		AdminGroupIDs: []string{testAdminGroupID},
	})
	if err != nil {
		t.Fatalf("CreateServer (netbird, no domain): %v", err)
	}
	if nb.Domain != "" {
		t.Fatalf("expected empty domain, got %q", nb.Domain)
	}

	// Disable NetBird without a domain -> ErrServerDomainRequired.
	if _, err := svc.UpdateServer(context.Background(), systemAdminToken(), nb.ID, UpdateServerRequest{
		NetbirdEnabled: boolPtr(false),
	}); !errors.Is(err, ErrServerDomainRequired) {
		t.Fatalf("disable netbird w/o domain = %v, want ErrServerDomainRequired", err)
	}

	// Disable NetBird WITH a domain -> OK, lands as a plain server.
	got, err := svc.UpdateServer(context.Background(), systemAdminToken(), nb.ID, UpdateServerRequest{
		NetbirdEnabled: boolPtr(false), Domain: strPtr("nb.local"),
	})
	if err != nil {
		t.Fatalf("disable netbird w/ domain: %v", err)
	}
	if got.NetbirdEnabled || got.Domain != "nb.local" {
		t.Fatalf("got {enabled:%v domain:%q}, want {false nb.local}", got.NetbirdEnabled, got.Domain)
	}
}

// TestGenerateNetbirdSetupKeyTrackingGroupIdempotent: generating a key twice for
// the same server (empty stored tracking id both times, e.g. a create-hook retry)
// must FIND the existing "op-gw-<id>" group the second time — creating no duplicate
// and resolving to the same tracking group id.
func TestGenerateNetbirdSetupKeyTrackingGroupIdempotent(t *testing.T) {
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	svc, routeStore := newNetbirdServerTestService(t, now)
	fake := newFakeNetbird(t)
	enableNetbird(t, svc, fake.srv.URL, true)

	cfg, ok, err := svc.NetbirdConfig(context.Background())
	if err != nil || !ok {
		t.Fatalf("NetbirdConfig = ok:%v err:%v, want true, nil", ok, err)
	}

	// An unsynced NetBird server with NO tracking group recorded yet.
	server := routing.AIServer{ID: "srv_fix4", Name: "NB", NetbirdEnabled: true, CreatedAt: now, UpdatedAt: now}
	if err := routeStore.CreateAIServer(context.Background(), server); err != nil {
		t.Fatalf("CreateAIServer: %v", err)
	}

	// First generation: resolve-by-name creates the tracking group once.
	if _, err := svc.generateNetbirdSetupKey(context.Background(), cfg, server, ""); err != nil {
		t.Fatalf("generateNetbirdSetupKey #1: %v", err)
	}
	stored1, _ := routeStore.AIServerByID(context.Background(), server.ID)
	if stored1.NetbirdGroupID == "" {
		t.Fatalf("tracking group id not recorded after #1")
	}
	if n := fake.groupCreateCount(); n != 1 {
		t.Fatalf("group creates after #1 = %d, want 1", n)
	}

	// Second generation with the SAME group-less server value + empty tracking id
	// (a regenerate / create-hook retry): find-or-create must reuse the existing
	// group, so no duplicate is created and the id is unchanged.
	if _, err := svc.generateNetbirdSetupKey(context.Background(), cfg, server, ""); err != nil {
		t.Fatalf("generateNetbirdSetupKey #2: %v", err)
	}
	if n := fake.groupCreateCount(); n != 1 {
		t.Fatalf("group creates after #2 = %d, want 1 (idempotent — no duplicate)", n)
	}
	stored2, _ := routeStore.AIServerByID(context.Background(), server.ID)
	if stored2.NetbirdGroupID != stored1.NetbirdGroupID {
		t.Fatalf("tracking group id changed across generations: %q -> %q", stored1.NetbirdGroupID, stored2.NetbirdGroupID)
	}
}

// TestCreateServerNetbirdHookMultiGroups: with MULTIPLE module groups configured,
// the create-hook setup key's auto_groups include EVERY resolved module group id
// PLUS the per-server tracking group id (no more, no less).
func TestCreateServerNetbirdHookMultiGroups(t *testing.T) {
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	svc, routeStore := newNetbirdServerTestService(t, now)
	fake := newFakeNetbird(t)
	// Two module groups: "gateways" (seeded → g-mod) + "prod" (created on demand).
	if _, err := svc.UpdateSystemSettings(context.Background(), systemToken(), UpdateSystemSettingsRequest{
		NetbirdEnabled: boolPtr(true),
		NetbirdURL:     strPtr(fake.srv.URL),
		NetbirdGroups:  &[]string{"gateways", "prod"},
		NetbirdToken:   strPtr("nbtok"),
	}); err != nil {
		t.Fatalf("configure netbird: %v", err)
	}

	dto, err := svc.CreateServer(context.Background(), adminToken(), CreateServerRequest{
		Name: "GPU multi", NetbirdEnabled: true, OwnerIDs: []string{"usr_owner"},
		AdminGroupIDs: []string{testAdminGroupID},
	})
	if err != nil {
		t.Fatalf("CreateServer: %v", err)
	}
	stored, err := routeStore.AIServerByID(context.Background(), dto.ID)
	if err != nil {
		t.Fatalf("AIServerByID: %v", err)
	}

	gatewaysID := fake.groupIDByName("gateways")
	prodID := fake.groupIDByName("prod")
	if gatewaysID == "" || prodID == "" {
		t.Fatalf("module group ids not resolved: gateways=%q prod=%q", gatewaysID, prodID)
	}
	if stored.NetbirdGroupID == "" {
		t.Fatalf("tracking group id not recorded")
	}
	got := fake.autoGroups()
	want := map[string]bool{gatewaysID: true, prodID: true, stored.NetbirdGroupID: true}
	if len(got) != len(want) {
		t.Fatalf("auto_groups = %v, want the 2 module groups + tracking (%v)", got, want)
	}
	for _, id := range got {
		if !want[id] {
			t.Fatalf("auto_groups %v contains unexpected id %q (want %v)", got, id, want)
		}
		delete(want, id)
	}
	if len(want) != 0 {
		t.Fatalf("auto_groups %v missing ids %v", got, want)
	}
}

// TestTestNetbird: not-configured -> sentinel; enabled + reachable -> nil.
func TestTestNetbird(t *testing.T) {
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	svc, _ := newNetbirdServerTestService(t, now)
	if err := svc.TestNetbird(context.Background(), systemToken(), nil); !errors.Is(err, ErrNetbirdNotConfigured) {
		t.Fatalf("TestNetbird(disabled) = %v, want ErrNetbirdNotConfigured", err)
	}
	fake := newFakeNetbird(t)
	enableNetbird(t, svc, fake.srv.URL, true)
	if err := svc.TestNetbird(context.Background(), systemToken(), nil); err != nil {
		t.Fatalf("TestNetbird(enabled, reachable) = %v, want nil", err)
	}
}

// TestTestNetbirdOverride: an override url/token is used INSTEAD of the stored
// values (each falling back to the stored value when nil) — proving the
// operator can test unsaved credentials before saving them.
func TestTestNetbirdOverride(t *testing.T) {
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	svc, _ := newNetbirdServerTestService(t, now)
	fake := newFakeNetbird(t)
	enableNetbird(t, svc, fake.srv.URL, true)

	// A bad override URL fails even though the stored URL is reachable.
	badURL := "http://127.0.0.1:1"
	if err := svc.TestNetbird(context.Background(), systemToken(), &NetbirdTestOverride{URL: &badURL}); err == nil {
		t.Fatalf("TestNetbird(bad override url) = nil, want an error")
	}
	// A nil override behaves like no override (stored values, reachable -> nil).
	if err := svc.TestNetbird(context.Background(), systemToken(), &NetbirdTestOverride{}); err != nil {
		t.Fatalf("TestNetbird(empty override) = %v, want nil (falls back to stored values)", err)
	}
}

// TestTestNetbirdOverrideRescuesFreshSetup reproduces the exact "fresh setup"
// state the codebase deliberately allows (validateNetbird permits
// netbird_enabled=true with an empty url/token — see docs/implementation-status.md
// "System Settings keeps ONLY the NetBird enable checkbox"): the module checkbox
// is ON but NO url/token has been saved yet. Before the fix, TestNetbird gated on
// NetbirdConfig's completeness check (which is false in this state) BEFORE
// applying the override, so a valid override could never rescue an unconfigured
// module — defeating the "test unsaved credentials before saving" purpose. The
// gate must be the raw module-enabled checkbox (NetbirdModuleChecked), and
// completeness must be (re-)checked only AFTER the override is applied.
func TestTestNetbirdOverrideRescuesFreshSetup(t *testing.T) {
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	svc, _ := newNetbirdServerTestService(t, now)
	fake := newFakeNetbird(t)

	// Enable ONLY the module checkbox -- no url, no token, no groups saved.
	if _, err := svc.UpdateSystemSettings(context.Background(), systemToken(), UpdateSystemSettingsRequest{
		NetbirdEnabled: boolPtr(true),
	}); err != nil {
		t.Fatalf("enable module checkbox only: %v", err)
	}

	// With no override, the module is on but incomplete -> ErrNetbirdNotConfigured.
	if err := svc.TestNetbird(context.Background(), systemToken(), nil); !errors.Is(err, ErrNetbirdNotConfigured) {
		t.Fatalf("TestNetbird(fresh setup, no override) = %v, want ErrNetbirdNotConfigured", err)
	}

	// An override supplying BOTH url and token (the unsaved credentials the
	// operator is about to save) must be pinged and succeed -- the whole point of
	// letting an operator test before saving.
	url := fake.srv.URL
	token := "nbtok"
	if err := svc.TestNetbird(context.Background(), systemToken(), &NetbirdTestOverride{URL: &url, Token: &token}); err != nil {
		t.Fatalf("TestNetbird(fresh setup, override url+token) = %v, want nil (the override must rescue an unconfigured module)", err)
	}
}

// TestServerDTONeverLeaksSetupKeyOnGet: a NetBird server's setup-key VALUE is
// present only on the create response (never persisted, so never on Get/List).
func TestServerDTONeverLeaksSetupKeyOnGet(t *testing.T) {
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	svc, _ := newNetbirdServerTestService(t, now)
	fake := newFakeNetbird(t)
	enableNetbird(t, svc, fake.srv.URL, true)
	created, err := svc.CreateServer(context.Background(), adminToken(), CreateServerRequest{
		Name: "NB", NetbirdEnabled: true, OwnerIDs: []string{"usr_owner"},
		AdminGroupIDs: []string{testAdminGroupID},
	})
	if err != nil {
		t.Fatalf("CreateServer: %v", err)
	}
	got, err := svc.GetServer(context.Background(), systemAdminToken(), created.ID)
	if err != nil {
		t.Fatalf("GetServer: %v", err)
	}
	if got.NetbirdSetupKey != "" {
		t.Fatalf("GetServer leaked NetbirdSetupKey = %q", got.NetbirdSetupKey)
	}
	// A marshaled Get DTO must not contain the setup-key VALUE anywhere (the
	// netbird_setup_key field is omitempty and the value is never persisted).
	raw, _ := json.Marshal(got)
	if strings.Contains(string(raw), "nbkey-secret-value") {
		t.Fatalf("GetServer DTO JSON leaked the setup-key value: %s", raw)
	}
	if got.NetbirdSetupKeyID != "sk-id" {
		t.Fatalf("GetServer NetbirdSetupKeyID = %q, want sk-id (the id is exposed, the value is not)", got.NetbirdSetupKeyID)
	}
	// The tracking-group id is a non-secret reference exposed on the DTO (Task 1).
	if got.NetbirdGroupID != "g-track" {
		t.Fatalf("GetServer NetbirdGroupID = %q, want g-track", got.NetbirdGroupID)
	}
	if !strings.Contains(string(raw), `"netbird_group_id"`) {
		t.Fatalf("Get DTO JSON missing netbird_group_id field: %s", raw)
	}
}

// TestNetbirdGroupsModuleDisabled: with the module off, listing groups yields the
// module-disabled sentinel and makes no NetBird call.
func TestNetbirdGroupsModuleDisabled(t *testing.T) {
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	svc, _ := newNetbirdServerTestService(t, now)
	if _, err := svc.NetbirdGroups(context.Background()); !errors.Is(err, ErrNetbirdModuleDisabled) {
		t.Fatalf("NetbirdGroups(module off) = %v, want ErrNetbirdModuleDisabled", err)
	}
}

// TestNetbirdGroupsReturnsGroups: with the module enabled + reachable, the account
// groups (the fake seeds the module group "gateways") are returned.
func TestNetbirdGroupsReturnsGroups(t *testing.T) {
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	svc, _ := newNetbirdServerTestService(t, now)
	fake := newFakeNetbird(t)
	enableNetbird(t, svc, fake.srv.URL, true)

	groups, err := svc.NetbirdGroups(context.Background())
	if err != nil {
		t.Fatalf("NetbirdGroups = %v, want nil", err)
	}
	found := false
	for _, g := range groups {
		if g.ID == "g-mod" && g.Name == "gateways" {
			found = true
		}
	}
	if !found {
		t.Fatalf("NetbirdGroups = %+v, want the seeded group {g-mod gateways}", groups)
	}
}

// TestNetbirdGroupsHidesTrackingGroups: the internal per-server tracking groups
// ("op-gw-<serverID>") must NEVER be pickable — NetbirdGroups filters them out so
// neither the system-settings nor the linkage-editor group multiselect can select
// one. Real (policy) groups pass through unchanged.
func TestNetbirdGroupsHidesTrackingGroups(t *testing.T) {
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	svc, _ := newNetbirdServerTestService(t, now)
	fake := newFakeNetbird(t)
	enableNetbird(t, svc, fake.srv.URL, true)

	fake.seedGroup("g-prod", "prod")
	fake.seedGroup("g-track1", "op-gw-srv1")
	fake.seedGroup("g-team", "team")

	groups, err := svc.NetbirdGroups(context.Background())
	if err != nil {
		t.Fatalf("NetbirdGroups = %v, want nil", err)
	}
	names := map[string]bool{}
	for _, g := range groups {
		if strings.HasPrefix(g.Name, "op-gw-") {
			t.Fatalf("NetbirdGroups returned internal tracking group %q; want it filtered out", g.Name)
		}
		names[g.Name] = true
	}
	if !names["prod"] || !names["team"] {
		t.Fatalf("NetbirdGroups = %+v, want prod + team present (real groups pass through)", groups)
	}
}

// decodeStoredRefs tolerantly decodes the opaque netbird_group_ids column value.
func decodeStoredRefs(t *testing.T, raw string) []NetbirdGroupRefDTO {
	t.Helper()
	if raw == "" {
		return nil
	}
	var out []NetbirdGroupRefDTO
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("decode netbird_group_ids %q: %v", raw, err)
	}
	return out
}

// TestSetServerNetbirdCreatesTrackingGroupAndPushesGroups: linking a peer on a
// server with NO tracking group creates "op-gw-<id>", adds the peer to it AND to
// the requested policy group A, records the group id, and mirrors [A].
func TestSetServerNetbirdCreatesTrackingGroupAndPushesGroups(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	svc, routeStore := newNetbirdServerTestService(t, now)
	fake := newFakeNetbird(t)
	enableNetbird(t, svc, fake.srv.URL, true)

	fake.seedGroup("g-A", "A")                                           // policy group to assign
	fake.setPeerWithGroups("peer-1", "old", "gpu.netbird.io", true, nil) // peer in no groups yet

	srv, err := svc.CreateServer(ctx, adminToken(), CreateServerRequest{
		Name: "GPU", Domain: "old.local", OwnerIDs: []string{"usr_owner"},
		AdminGroupIDs: []string{testAdminGroupID},
	})
	if err != nil {
		t.Fatalf("CreateServer: %v", err)
	}
	if before, _ := routeStore.AIServerByID(ctx, srv.ID); before.NetbirdGroupID != "" {
		t.Fatalf("precondition: tracking group already set: %q", before.NetbirdGroupID)
	}

	dto, err := svc.SetServerNetbird(ctx, systemToken(), srv.ID, true, "peer-1", []string{"g-A"}, false, "", false, false)
	if err != nil {
		t.Fatalf("SetServerNetbird(link + groups): %v", err)
	}

	stored, _ := routeStore.AIServerByID(ctx, srv.ID)
	if stored.NetbirdGroupID != "g-track" {
		t.Fatalf("tracking group = %q, want g-track (created op-gw-<id>)", stored.NetbirdGroupID)
	}
	if !fake.groupHasPeer("g-track", "peer-1") {
		t.Fatalf("peer not added to the tracking group")
	}
	if !fake.groupHasPeer("g-A", "peer-1") {
		t.Fatalf("peer not added to policy group A")
	}
	refs := decodeStoredRefs(t, stored.NetbirdGroupIDs)
	if len(refs) != 1 || refs[0].ID != "g-A" {
		t.Fatalf("mirror = %+v, want [{g-A ..}] (tracking excluded)", refs)
	}
	if len(dto.NetbirdGroupIDs) != 1 || dto.NetbirdGroupIDs[0].ID != "g-A" {
		t.Fatalf("dto.NetbirdGroupIDs = %+v, want [{g-A ..}]", dto.NetbirdGroupIDs)
	}
}

// TestSetServerNetbirdReusesExistingTrackingGroup: linking a peer on a server that
// ALREADY has a tracking group creates no duplicate — the stored id is unchanged
// and the peer is added to the existing tracking group + the policy group.
func TestSetServerNetbirdReusesExistingTrackingGroup(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	svc, routeStore := newNetbirdServerTestService(t, now)
	fake := newFakeNetbird(t)
	enableNetbird(t, svc, fake.srv.URL, true)

	// Create-hook makes the tracking group g-track (1 create).
	srv, err := svc.CreateServer(ctx, adminToken(), CreateServerRequest{
		Name: "NB", NetbirdEnabled: true, OwnerIDs: []string{"usr_owner"},
		AdminGroupIDs: []string{testAdminGroupID},
	})
	if err != nil {
		t.Fatalf("CreateServer: %v", err)
	}
	if n := fake.groupCreateCount(); n != 1 {
		t.Fatalf("after create groupCreates = %d, want 1", n)
	}
	if stored0, _ := routeStore.AIServerByID(ctx, srv.ID); stored0.NetbirdGroupID != "g-track" {
		t.Fatalf("tracking group after create = %q, want g-track", stored0.NetbirdGroupID)
	}
	fake.seedGroup("g-A", "A")
	fake.setPeerWithGroups("peer-2", "n", "nb.netbird.io", true, nil)

	if _, err := svc.SetServerNetbird(ctx, systemToken(), srv.ID, true, "peer-2", []string{"g-A"}, false, "", false, false); err != nil {
		t.Fatalf("SetServerNetbird(link, existing tracking group): %v", err)
	}
	if n := fake.groupCreateCount(); n != 1 {
		t.Fatalf("after link groupCreates = %d, want 1 (no duplicate tracking group)", n)
	}
	stored, _ := routeStore.AIServerByID(ctx, srv.ID)
	if stored.NetbirdGroupID != "g-track" {
		t.Fatalf("tracking group changed to %q, want g-track", stored.NetbirdGroupID)
	}
	if !fake.groupHasPeer("g-track", "peer-2") || !fake.groupHasPeer("g-A", "peer-2") {
		t.Fatalf("peer not added to tracking + A")
	}
}

// TestSetServerNetbirdNeverRemovesTrackingGroup: a push that changes the policy
// groups NEVER removes the peer from the tracking group (it is excluded from the
// remove set), removes only stale policy groups, and adds the new ones.
func TestSetServerNetbirdNeverRemovesTrackingGroup(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	svc, routeStore := newNetbirdServerTestService(t, now)
	fake := newFakeNetbird(t)
	enableNetbird(t, svc, fake.srv.URL, true)

	srv := routing.AIServer{ID: "srv_track", Name: "NB", Domain: "nb.local", NetbirdEnabled: true, NetbirdPeerID: "peer-3", NetbirdGroupID: "g-track", CreatedAt: now, UpdatedAt: now}
	if err := routeStore.CreateAIServer(ctx, srv); err != nil {
		t.Fatalf("CreateAIServer: %v", err)
	}
	// The tracking group + an OLD policy group both currently hold the peer.
	fake.seedGroup("g-track", "op-gw-srv_track", "peer-3")
	fake.seedGroup("g-old", "old", "peer-3")
	fake.seedGroup("g-new", "new")
	fake.setPeerWithGroups("peer-3", "NB", "nb.netbird.io", true, []map[string]any{
		{"id": "g-track", "name": "op-gw-srv_track"},
		{"id": "g-old", "name": "old"},
	})

	if _, err := svc.SetServerNetbird(ctx, systemToken(), srv.ID, true, "peer-3", []string{"g-new"}, false, "", false, false); err != nil {
		t.Fatalf("SetServerNetbird(re-link, new policy set): %v", err)
	}
	if !fake.groupHasPeer("g-track", "peer-3") {
		t.Fatalf("tracking group lost the peer — a push must NEVER remove the tracking group")
	}
	if fake.groupHasPeer("g-old", "peer-3") {
		t.Fatalf("g-old should have been removed from the peer")
	}
	if !fake.groupHasPeer("g-new", "peer-3") {
		t.Fatalf("g-new should have been added to the peer")
	}
	stored, _ := routeStore.AIServerByID(ctx, srv.ID)
	refs := decodeStoredRefs(t, stored.NetbirdGroupIDs)
	if len(refs) != 1 || refs[0].ID != "g-new" {
		t.Fatalf("mirror = %+v, want [{g-new ..}] (tracking excluded)", refs)
	}
}

// TestSetServerNetbirdDisableDeletesTrackingGroupAndClears: disabling a linked
// server deletes its tracking group and clears the stored group id + mirror +
// enabled/connected flags.
func TestSetServerNetbirdDisableDeletesTrackingGroupAndClears(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	svc, routeStore := newNetbirdServerTestService(t, now)
	fake := newFakeNetbird(t)
	enableNetbird(t, svc, fake.srv.URL, true)

	srv := routing.AIServer{ID: "srv_dis", Name: "NB", Domain: "nb.local", NetbirdEnabled: true, NetbirdPeerID: "peer-4", NetbirdSetupKeyID: "sk-1", NetbirdGroupID: "g-track", NetbirdGroupIDs: `[{"id":"g-A","name":"A"}]`, NetbirdConnected: true, CreatedAt: now, UpdatedAt: now}
	if err := routeStore.CreateAIServer(ctx, srv); err != nil {
		t.Fatalf("CreateAIServer: %v", err)
	}
	fake.seedGroup("g-track", "op-gw-srv_dis", "peer-4")

	dto, err := svc.SetServerNetbird(ctx, systemToken(), srv.ID, false, "", nil, false, "", false, false)
	if err != nil {
		t.Fatalf("SetServerNetbird(disable): %v", err)
	}
	if !fake.wasGroupDeleted("g-track") {
		t.Fatalf("tracking group was not deleted on disable")
	}
	stored, _ := routeStore.AIServerByID(ctx, srv.ID)
	if stored.NetbirdEnabled {
		t.Fatalf("stored still enabled after disable")
	}
	if stored.NetbirdGroupID != "" {
		t.Fatalf("stored group id not cleared: %q", stored.NetbirdGroupID)
	}
	if stored.NetbirdGroupIDs != "" {
		t.Fatalf("stored mirror not cleared: %q", stored.NetbirdGroupIDs)
	}
	if stored.NetbirdConnected {
		t.Fatalf("stored connected not reset")
	}
	if dto.NetbirdEnabled || len(dto.NetbirdGroupIDs) != 0 {
		t.Fatalf("dto not cleared: {enabled:%v groups:%+v}", dto.NetbirdEnabled, dto.NetbirdGroupIDs)
	}
}

// TestSetServerNetbirdDisableClearsMirrorWithoutTrackingGroup: disabling a server
// that has NO stored tracking-group id but a non-empty policy-group mirror still
// clears the LOCAL linkage — a stale mirror must not survive a disable. The
// tracking-group DeleteGroup stays gated on a stored group id, so no NetBird call
// is made here (module left off).
func TestSetServerNetbirdDisableClearsMirrorWithoutTrackingGroup(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	svc, routeStore := newNetbirdServerTestService(t, now)

	srv := routing.AIServer{ID: "srv_nogrp", Name: "NB", Domain: "nb.local", NetbirdEnabled: true, NetbirdPeerID: "peer-9", NetbirdGroupID: "", NetbirdGroupIDs: `[{"id":"g-A","name":"A"}]`, NetbirdConnected: true, CreatedAt: now, UpdatedAt: now}
	if err := routeStore.CreateAIServer(ctx, srv); err != nil {
		t.Fatalf("CreateAIServer: %v", err)
	}

	dto, err := svc.SetServerNetbird(ctx, systemToken(), srv.ID, false, "", nil, false, "", false, false)
	if err != nil {
		t.Fatalf("SetServerNetbird(disable): %v", err)
	}
	stored, _ := routeStore.AIServerByID(ctx, srv.ID)
	if stored.NetbirdGroupIDs != "" {
		t.Fatalf("stored mirror not cleared: %q (a stale mirror survived disable)", stored.NetbirdGroupIDs)
	}
	if stored.NetbirdEnabled || stored.NetbirdPeerID != "" || stored.NetbirdConnected {
		t.Fatalf("stored linkage not cleared: {enabled:%v peer:%q conn:%v}", stored.NetbirdEnabled, stored.NetbirdPeerID, stored.NetbirdConnected)
	}
	if dto.NetbirdEnabled || len(dto.NetbirdGroupIDs) != 0 {
		t.Fatalf("dto not cleared: {enabled:%v groups:%+v}", dto.NetbirdEnabled, dto.NetbirdGroupIDs)
	}
}

// TestDeleteServerDeletesTrackingGroup: deleting a server best-effort deletes its
// tracking group; a NetBird 500 during the delete does NOT fail the row delete.
func TestDeleteServerDeletesTrackingGroup(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	svc, routeStore := newNetbirdServerTestService(t, now)
	fake := newFakeNetbird(t)
	enableNetbird(t, svc, fake.srv.URL, true)

	// Success path.
	srv := routing.AIServer{ID: "srv_del1", Name: "NB1", Domain: "a.local", NetbirdEnabled: true, NetbirdGroupID: "g-track", CreatedAt: now, UpdatedAt: now}
	if err := routeStore.CreateAIServer(ctx, srv); err != nil {
		t.Fatalf("CreateAIServer: %v", err)
	}
	fake.seedGroup("g-track", "op-gw-srv_del1")
	if _, err := svc.DeleteServer(ctx, systemAdminToken(), srv.ID, false); err != nil {
		t.Fatalf("DeleteServer: %v", err)
	}
	if !fake.wasGroupDeleted("g-track") {
		t.Fatalf("tracking group not deleted on server delete")
	}
	if _, err := routeStore.AIServerByID(ctx, srv.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("server row still present after delete: %v", err)
	}

	// A NetBird 500 during the delete must NOT fail the delete.
	fake.failDeleteGroup = true
	srv2 := routing.AIServer{ID: "srv_del2", Name: "NB2", Domain: "b.local", NetbirdEnabled: true, NetbirdGroupID: "g-track2", CreatedAt: now, UpdatedAt: now}
	if err := routeStore.CreateAIServer(ctx, srv2); err != nil {
		t.Fatalf("CreateAIServer 2: %v", err)
	}
	fake.seedGroup("g-track2", "op-gw-srv_del2")
	if _, err := svc.DeleteServer(ctx, systemAdminToken(), srv2.ID, false); err != nil {
		t.Fatalf("DeleteServer must not fail on a netbird error: %v", err)
	}
	if _, err := routeStore.AIServerByID(ctx, srv2.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("server row must be deleted even when netbird errored: %v", err)
	}
}

// TestDeleteServerDeletePeerAndSetupKey: the opt-in delete_peer flag best-effort
// deletes the NetBird peer AND setup key. A NetBird failure surfaces via the
// returned flag but NEVER fails the row delete; deletePeer=false / module-off make
// no delete call; empty ids skip the corresponding call.
func TestDeleteServerDeletePeerAndSetupKey(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)

	// seed a server with the given peer + setup-key ids (no tracking group so the
	// peer/key deletes are isolated from the unconditional tracking-group delete).
	seed := func(t *testing.T, routeStore *routing.MemoryStore, id, peerID, keyID string) {
		t.Helper()
		srv := routing.AIServer{ID: id, Name: id, Domain: id + ".local", NetbirdEnabled: true, NetbirdPeerID: peerID, NetbirdSetupKeyID: keyID, CreatedAt: now, UpdatedAt: now}
		if err := routeStore.CreateAIServer(ctx, srv); err != nil {
			t.Fatalf("CreateAIServer: %v", err)
		}
	}

	t.Run("both deleted on success", func(t *testing.T) {
		svc, routeStore := newNetbirdServerTestService(t, now)
		fake := newFakeNetbird(t)
		enableNetbird(t, svc, fake.srv.URL, true)
		seed(t, routeStore, "srv_pk", "peer-1", "sk-1")

		failed, err := svc.DeleteServer(ctx, systemAdminToken(), "srv_pk", true)
		if err != nil {
			t.Fatalf("DeleteServer: %v", err)
		}
		if failed {
			t.Fatalf("failed = true, want false on success")
		}
		if !fake.wasPeerDeleted("peer-1") {
			t.Fatalf("peer not deleted")
		}
		if !fake.wasSetupKeyDeleted("sk-1") {
			t.Fatalf("setup key not deleted")
		}
		if _, err := routeStore.AIServerByID(ctx, "srv_pk"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("row still present after delete: %v", err)
		}
	})

	t.Run("peer delete failure returns true, row still deleted", func(t *testing.T) {
		svc, routeStore := newNetbirdServerTestService(t, now)
		fake := newFakeNetbird(t)
		fake.failDeletePeer = true
		enableNetbird(t, svc, fake.srv.URL, true)
		seed(t, routeStore, "srv_pk", "peer-1", "sk-1")

		failed, err := svc.DeleteServer(ctx, systemAdminToken(), "srv_pk", true)
		if err != nil {
			t.Fatalf("DeleteServer must not fail on a netbird error: %v", err)
		}
		if !failed {
			t.Fatalf("failed = false, want true when DeletePeer errored")
		}
		// The setup key is still attempted (independent of the peer failure).
		if !fake.wasSetupKeyDeleted("sk-1") {
			t.Fatalf("setup key not deleted despite peer failure")
		}
		if _, err := routeStore.AIServerByID(ctx, "srv_pk"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("row must be deleted even when netbird errored: %v", err)
		}
	})

	t.Run("deletePeer=false makes neither call", func(t *testing.T) {
		svc, routeStore := newNetbirdServerTestService(t, now)
		fake := newFakeNetbird(t)
		enableNetbird(t, svc, fake.srv.URL, true)
		seed(t, routeStore, "srv_pk", "peer-1", "sk-1")

		failed, err := svc.DeleteServer(ctx, systemAdminToken(), "srv_pk", false)
		if err != nil || failed {
			t.Fatalf("DeleteServer = failed:%v err:%v, want false,nil", failed, err)
		}
		if fake.wasPeerDeleted("peer-1") || fake.wasSetupKeyDeleted("sk-1") {
			t.Fatalf("deletePeer=false must not delete peer/key")
		}
	})

	t.Run("module disabled makes neither call", func(t *testing.T) {
		svc, routeStore := newNetbirdServerTestService(t, now)
		fake := newFakeNetbird(t)
		enableNetbird(t, svc, fake.srv.URL, false) // configured but DISABLED
		seed(t, routeStore, "srv_pk", "peer-1", "sk-1")

		failed, err := svc.DeleteServer(ctx, systemAdminToken(), "srv_pk", true)
		if err != nil || failed {
			t.Fatalf("DeleteServer = failed:%v err:%v, want false,nil", failed, err)
		}
		if fake.wasPeerDeleted("peer-1") || fake.wasSetupKeyDeleted("sk-1") {
			t.Fatalf("module off must make no netbird delete call")
		}
	})

	t.Run("empty peer id skips the peer delete", func(t *testing.T) {
		svc, routeStore := newNetbirdServerTestService(t, now)
		fake := newFakeNetbird(t)
		enableNetbird(t, svc, fake.srv.URL, true)
		seed(t, routeStore, "srv_keyonly", "", "sk-only")

		failed, err := svc.DeleteServer(ctx, systemAdminToken(), "srv_keyonly", true)
		if err != nil || failed {
			t.Fatalf("DeleteServer = failed:%v err:%v, want false,nil", failed, err)
		}
		if len(fake.deletedPeers) != 0 {
			t.Fatalf("empty peer id must be skipped, got deletes %v", fake.deletedPeers)
		}
		if !fake.wasSetupKeyDeleted("sk-only") {
			t.Fatalf("setup key should still be deleted when peer id is empty")
		}
	})
}

// TestNetbirdPeerManagedProvenance: a gateway-generated setup key (create hook /
// regenerate) marks the server peer MANAGED (true); a manual linkage-editor bind
// marks it UNMANAGED (false).
func TestNetbirdPeerManagedProvenance(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)

	t.Run("setup-key generation marks managed", func(t *testing.T) {
		svc, routeStore := newNetbirdServerTestService(t, now)
		fake := newFakeNetbird(t)
		enableNetbird(t, svc, fake.srv.URL, true)
		created, err := svc.CreateServer(ctx, adminToken(), CreateServerRequest{
			Name: "NB", NetbirdEnabled: true, OwnerIDs: []string{"usr_owner"},
			AdminGroupIDs: []string{testAdminGroupID},
		})
		if err != nil {
			t.Fatalf("CreateServer: %v", err)
		}
		stored, err := routeStore.AIServerByID(ctx, created.ID)
		if err != nil {
			t.Fatalf("AIServerByID: %v", err)
		}
		if !stored.NetbirdPeerManaged {
			t.Fatalf("NetbirdPeerManaged = false, want true after setup-key generation")
		}
	})

	t.Run("manual linkage marks unmanaged", func(t *testing.T) {
		svc, routeStore := newNetbirdServerTestService(t, now)
		fake := newFakeNetbird(t)
		enableNetbird(t, svc, fake.srv.URL, true)
		// Seed a server that is (wrongly) marked managed=true so the write to false is
		// mutation-proven (a missing write would leave it true).
		srv := routing.AIServer{ID: "srv_manual", Name: "Manual", Domain: "m.local", NetbirdEnabled: true, NetbirdPeerManaged: true, CreatedAt: now, UpdatedAt: now}
		if err := routeStore.CreateAIServer(ctx, srv); err != nil {
			t.Fatalf("CreateAIServer: %v", err)
		}
		fake.setPeer("peer-m", "old", "m.netbird.io", true)
		if _, err := svc.SetServerNetbird(ctx, systemToken(), "srv_manual", true, "peer-m", nil, false, "", false, false); err != nil {
			t.Fatalf("SetServerNetbird: %v", err)
		}
		stored, err := routeStore.AIServerByID(ctx, "srv_manual")
		if err != nil {
			t.Fatalf("AIServerByID: %v", err)
		}
		if stored.NetbirdPeerManaged {
			t.Fatalf("NetbirdPeerManaged = true, want false after a manual link")
		}
	})
}

// TestServerDTONetbirdPeerManagedAndSetupCommand: the Get/List DTO exposes
// netbird_peer_managed; the create response carries the display-once
// netbird_setup_command; and a plain Get DTO carries NEITHER the setup key NOR the
// command (both display-once, never persisted).
func TestServerDTONetbirdPeerManagedAndSetupCommand(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	svc, _ := newNetbirdServerTestService(t, now)
	fake := newFakeNetbird(t)
	enableNetbird(t, svc, fake.srv.URL, true)

	created, err := svc.CreateServer(ctx, adminToken(), CreateServerRequest{
		Name: "NB", NetbirdEnabled: true, OwnerIDs: []string{"usr_owner"},
		AdminGroupIDs: []string{testAdminGroupID},
	})
	if err != nil {
		t.Fatalf("CreateServer: %v", err)
	}
	// The create response carries the ready-to-paste console command with the key.
	wantCmd := "netbird up --management-url " + fake.srv.URL + " --setup-key nbkey-secret-value"
	if created.NetbirdSetupCommand != wantCmd {
		t.Fatalf("create NetbirdSetupCommand = %q, want %q", created.NetbirdSetupCommand, wantCmd)
	}
	// The create response is managed (portal-created).
	if !created.NetbirdPeerManaged {
		t.Fatalf("create NetbirdPeerManaged = false, want true")
	}

	got, err := svc.GetServer(ctx, systemAdminToken(), created.ID)
	if err != nil {
		t.Fatalf("GetServer: %v", err)
	}
	if got.NetbirdSetupCommand != "" {
		t.Fatalf("GetServer leaked NetbirdSetupCommand = %q", got.NetbirdSetupCommand)
	}
	if !got.NetbirdPeerManaged {
		t.Fatalf("GetServer NetbirdPeerManaged = false, want true")
	}
	raw, _ := json.Marshal(got)
	// The Get DTO exposes netbird_peer_managed but NEITHER display-once field.
	if !strings.Contains(string(raw), `"netbird_peer_managed"`) {
		t.Fatalf("Get DTO JSON missing netbird_peer_managed field: %s", raw)
	}
	// Exact-key checks (netbird_setup_key_id IS present — its prefix must not confuse
	// the leak assertion for the omitempty display-once netbird_setup_key field).
	if strings.Contains(string(raw), `"netbird_setup_command":`) {
		t.Fatalf("Get DTO JSON leaked netbird_setup_command: %s", raw)
	}
	if strings.Contains(string(raw), `"netbird_setup_key":`) {
		t.Fatalf("Get DTO JSON leaked netbird_setup_key: %s", raw)
	}
	if strings.Contains(string(raw), "nbkey-secret-value") {
		t.Fatalf("Get DTO JSON leaked the setup-key value: %s", raw)
	}
}

// TestSetServerNetbirdWritesPeerManaged (Task 2A): the linkage editor writes the
// PASSED peerManaged value — true stores managed=true, false stores managed=false.
// Both directions are seeded to the OPPOSITE value first so the write is
// mutation-proven (a hardcoded constant would fail one of the two cases).
func TestSetServerNetbirdWritesPeerManaged(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)

	t.Run("peerManaged=true stores true", func(t *testing.T) {
		svc, routeStore := newNetbirdServerTestService(t, now)
		// Seed managed=false so the true-write is mutation-proven.
		srv := routing.AIServer{ID: "srv_true", Name: "NB", Domain: "nb.local", NetbirdEnabled: true, NetbirdPeerManaged: false, CreatedAt: now, UpdatedAt: now}
		if err := routeStore.CreateAIServer(ctx, srv); err != nil {
			t.Fatalf("CreateAIServer: %v", err)
		}
		// module off => no reconcile; the managed write runs regardless.
		if _, err := svc.SetServerNetbird(ctx, systemToken(), srv.ID, true, "peer-t", nil, true, "", false, false); err != nil {
			t.Fatalf("SetServerNetbird(managed=true): %v", err)
		}
		stored, _ := routeStore.AIServerByID(ctx, srv.ID)
		if !stored.NetbirdPeerManaged {
			t.Fatalf("NetbirdPeerManaged = false, want true (the passed value must be written)")
		}
	})

	t.Run("peerManaged=false stores false", func(t *testing.T) {
		svc, routeStore := newNetbirdServerTestService(t, now)
		// Seed managed=true so the false-write is mutation-proven.
		srv := routing.AIServer{ID: "srv_false", Name: "NB", Domain: "nb.local", NetbirdEnabled: true, NetbirdPeerManaged: true, CreatedAt: now, UpdatedAt: now}
		if err := routeStore.CreateAIServer(ctx, srv); err != nil {
			t.Fatalf("CreateAIServer: %v", err)
		}
		if _, err := svc.SetServerNetbird(ctx, systemToken(), srv.ID, true, "peer-f", nil, false, "", false, false); err != nil {
			t.Fatalf("SetServerNetbird(managed=false): %v", err)
		}
		stored, _ := routeStore.AIServerByID(ctx, srv.ID)
		if stored.NetbirdPeerManaged {
			t.Fatalf("NetbirdPeerManaged = true, want false (the passed value must be written)")
		}
	})
}

// TestRegenerateNetbirdKeyGateNonManaged (Task 2B gate): a server with an existing
// NON-managed peer is blocked with ErrNetbirdPeerNotManaged — no key is created and
// no peer is deleted (the gate returns before any NetBird call).
func TestRegenerateNetbirdKeyGateNonManaged(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	svc, routeStore := newNetbirdServerTestService(t, now)
	fake := newFakeNetbird(t)
	enableNetbird(t, svc, fake.srv.URL, true)

	srv := routing.AIServer{ID: "srv_foreign", Name: "NB", Domain: "nb.local", NetbirdEnabled: true, NetbirdPeerManaged: false, NetbirdPeerID: "peer-foreign", NetbirdGroupID: "g-track", NetbirdSetupKeyID: "sk-x", CreatedAt: now, UpdatedAt: now}
	if err := routeStore.CreateAIServer(ctx, srv); err != nil {
		t.Fatalf("CreateAIServer: %v", err)
	}
	fake.seedGroup("g-track", "op-gw-srv_foreign", "peer-foreign")

	if _, _, err := svc.RegenerateNetbirdKey(ctx, systemAdminToken(), srv.ID); !errors.Is(err, ErrNetbirdPeerNotManaged) {
		t.Fatalf("RegenerateNetbirdKey(non-managed w/ peer) = %v, want ErrNetbirdPeerNotManaged", err)
	}
	// The gate returns before ANY NetBird call: no setup key, no DeletePeer.
	if fake.count() != 0 {
		t.Fatalf("netbird requests = %d, want 0 (gate must return before any call)", fake.count())
	}
	if fake.deletedPeerCount() != 0 {
		t.Fatalf("deleted peers = %d, want 0 (no foreign peer may be deleted)", fake.deletedPeerCount())
	}
	// Stored linkage is untouched (no new key recorded, peer preserved).
	stored, _ := routeStore.AIServerByID(ctx, srv.ID)
	if stored.NetbirdPeerID != "peer-foreign" || stored.NetbirdSetupKeyID != "sk-x" {
		t.Fatalf("stored = {peer:%q key:%q}, want {peer-foreign sk-x} (unchanged)", stored.NetbirdPeerID, stored.NetbirdSetupKeyID)
	}
}

// TestRegenerateNetbirdKeyManagedDeletesExistingPeers (Task 2B delete): on a
// MANAGED server with an existing peer + tracking group holding peers, regenerate
// deletes every group member, clears the stored peer id (domain preserved),
// generates a new key, and keeps managed=true.
func TestRegenerateNetbirdKeyManagedDeletesExistingPeers(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	svc, routeStore := newNetbirdServerTestService(t, now)
	fake := newFakeNetbird(t)
	enableNetbird(t, svc, fake.srv.URL, true)

	srv := routing.AIServer{ID: "srv_mng", Name: "NB", Domain: "nb.local", NetbirdEnabled: true, NetbirdPeerManaged: true, NetbirdPeerID: "peer-old", NetbirdGroupID: "g-track", NetbirdSetupKeyID: "sk-old", NetbirdConnected: true, CreatedAt: now, UpdatedAt: now}
	if err := routeStore.CreateAIServer(ctx, srv); err != nil {
		t.Fatalf("CreateAIServer: %v", err)
	}
	// The tracking group holds the stored peer PLUS an extra straggler peer.
	fake.seedGroup("g-track", "op-gw-srv_mng", "peer-old", "peer-extra")

	key, _, err := svc.RegenerateNetbirdKey(ctx, systemAdminToken(), srv.ID)
	if err != nil || key != "nbkey-secret-value" {
		t.Fatalf("RegenerateNetbirdKey(managed) = %q, %v; want the key, nil", key, err)
	}
	// Both group members were deleted (the proactive one-peer cleanup).
	if !fake.wasPeerDeleted("peer-old") {
		t.Fatalf("stored peer 'peer-old' was not deleted on regenerate")
	}
	if !fake.wasPeerDeleted("peer-extra") {
		t.Fatalf("straggler group member 'peer-extra' was not deleted on regenerate")
	}
	stored, _ := routeStore.AIServerByID(ctx, srv.ID)
	if stored.NetbirdPeerID != "" {
		t.Fatalf("stored peer id = %q, want cleared (UpdateServerNetbirdState)", stored.NetbirdPeerID)
	}
	if stored.NetbirdConnected {
		t.Fatalf("stored connected = true, want reset to false")
	}
	if stored.Domain != "nb.local" {
		t.Fatalf("stored domain = %q, want nb.local (never-clear-domain)", stored.Domain)
	}
	if stored.NetbirdSetupKeyID != "sk-id" {
		t.Fatalf("stored setup-key id = %q, want sk-id (new key recorded)", stored.NetbirdSetupKeyID)
	}
	if !stored.NetbirdPeerManaged {
		t.Fatalf("stored managed = false, want true (generateNetbirdSetupKey keeps it managed)")
	}
}

// TestRegenerateNetbirdKeyFreshServerNoDelete (Task 2B fresh): a fresh server
// (peer_id=="" && group_id=="") is always allowed — no delete runs, a key is
// created, and enrollment marks it managed=true.
func TestRegenerateNetbirdKeyFreshServerNoDelete(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	svc, routeStore := newNetbirdServerTestService(t, now)
	fake := newFakeNetbird(t)
	enableNetbird(t, svc, fake.srv.URL, true)

	srv := routing.AIServer{ID: "srv_fresh", Name: "NB", Domain: "nb.local", NetbirdEnabled: true, CreatedAt: now, UpdatedAt: now}
	if err := routeStore.CreateAIServer(ctx, srv); err != nil {
		t.Fatalf("CreateAIServer: %v", err)
	}

	key, _, err := svc.RegenerateNetbirdKey(ctx, systemAdminToken(), srv.ID)
	if err != nil || key != "nbkey-secret-value" {
		t.Fatalf("RegenerateNetbirdKey(fresh) = %q, %v; want the key, nil", key, err)
	}
	if fake.deletedPeerCount() != 0 {
		t.Fatalf("deleted peers = %d, want 0 (nothing to delete on a fresh server)", fake.deletedPeerCount())
	}
	stored, _ := routeStore.AIServerByID(ctx, srv.ID)
	if stored.NetbirdGroupID == "" || stored.NetbirdSetupKeyID == "" {
		t.Fatalf("after enroll group/key not recorded: grp=%q key=%q", stored.NetbirdGroupID, stored.NetbirdSetupKeyID)
	}
	if !stored.NetbirdPeerManaged {
		t.Fatalf("stored managed = false, want true (first enrollment marks it managed)")
	}
}

// TestRegenerateNetbirdKeyBestEffortOnDeletePeerFailure (Task 2B best-effort):
// the proactive one-peer cleanup is BEST-EFFORT — a NetBird DeletePeer error while
// clearing the existing peer(s) must NOT abort key generation. On a MANAGED server
// whose tracking group holds peers, with DELETE /api/peers/{id} forced to 500,
// regenerate STILL succeeds: it returns a non-empty key + command with no error and
// records the new setup-key id on the server.
func TestRegenerateNetbirdKeyBestEffortOnDeletePeerFailure(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	svc, routeStore := newNetbirdServerTestService(t, now)
	fake := newFakeNetbird(t)
	fake.failDeletePeer = true // the proactive peer delete 500s — must not abort key gen
	enableNetbird(t, svc, fake.srv.URL, true)

	srv := routing.AIServer{ID: "srv_mng", Name: "NB", Domain: "nb.local", NetbirdEnabled: true, NetbirdPeerManaged: true, NetbirdPeerID: "peer-old", NetbirdGroupID: "g-track", NetbirdSetupKeyID: "sk-old", NetbirdConnected: true, CreatedAt: now, UpdatedAt: now}
	if err := routeStore.CreateAIServer(ctx, srv); err != nil {
		t.Fatalf("CreateAIServer: %v", err)
	}
	// The tracking group holds the stored peer, so the proactive DeletePeer fires (and 500s).
	fake.seedGroup("g-track", "op-gw-srv_mng", "peer-old")

	key, command, err := svc.RegenerateNetbirdKey(ctx, systemAdminToken(), srv.ID)
	if err != nil {
		t.Fatalf("RegenerateNetbirdKey must not fail on a best-effort DeletePeer error: %v", err)
	}
	if key != "nbkey-secret-value" {
		t.Fatalf("key = %q, want the generated key (delete failure must not abort key gen)", key)
	}
	if command == "" {
		t.Fatalf("command = empty, want the ready-to-paste `netbird up` command")
	}
	// The new setup key was recorded despite the delete failure.
	stored, _ := routeStore.AIServerByID(ctx, srv.ID)
	if stored.NetbirdSetupKeyID != "sk-id" {
		t.Fatalf("stored setup-key id = %q, want sk-id (new key recorded despite delete failure)", stored.NetbirdSetupKeyID)
	}
}

// TestUpdateServerRenamesNetbirdPeerAndUpdatesDomain: renaming a NetBird-linked
// server must, on save, rename its NetBird peer to the new name AND pull the
// server's domain to the peer's (new) dns_label immediately — not one sync-loop
// interval later. Regression guard for the "name/domain lag after edit" bug.
func TestUpdateServerRenamesNetbirdPeerAndUpdatesDomain(t *testing.T) {
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	svc, routeStore := newNetbirdServerTestService(t, now)
	fake := newFakeNetbird(t)
	enableNetbird(t, svc, fake.srv.URL, true)
	// The linked peer as NetBird sees it (its dns_label is what the domain follows).
	fake.peers["peer-1"] = map[string]any{"id": "peer-1", "name": "old-name", "dns_label": "peer-dns.netbird.cloud", "connected": true}

	dto, err := svc.CreateServer(ctx, adminToken(), CreateServerRequest{Name: "old-name", NetbirdEnabled: true, OwnerIDs: []string{"usr_owner"}, AdminGroupIDs: []string{testAdminGroupID}})
	if err != nil {
		t.Fatalf("CreateServer: %v", err)
	}
	// Link the server to peer-1 directly in the store (as the sync loop/linkage would).
	if err := routeStore.UpdateServerNetbirdState(ctx, dto.ID, "old-domain", "peer-1", true); err != nil {
		t.Fatalf("link peer: %v", err)
	}

	name := "new-name"
	updated, err := svc.UpdateServer(ctx, systemAdminToken(), dto.ID, UpdateServerRequest{Name: &name})
	if err != nil {
		t.Fatalf("UpdateServer: %v", err)
	}

	// The peer was renamed in NetBird to the new server name.
	if fake.peerRenames["peer-1"] != "new-name" {
		t.Fatalf("peer not renamed synchronously on the server edit: %v", fake.peerRenames)
	}
	// The domain followed the peer's dns_label — in the returned DTO AND the store.
	if updated.Domain != "peer-dns.netbird.cloud" {
		t.Fatalf("dto domain = %q, want peer-dns.netbird.cloud (domain must follow the rename)", updated.Domain)
	}
	stored, err := routeStore.AIServerByID(ctx, dto.ID)
	if err != nil {
		t.Fatalf("AIServerByID: %v", err)
	}
	if stored.Name != "new-name" || stored.Domain != "peer-dns.netbird.cloud" {
		t.Fatalf("stored = {name:%q domain:%q}, want {new-name peer-dns.netbird.cloud}", stored.Name, stored.Domain)
	}
}

// TestUpdateServerNonNetbirdNoPeerCall: renaming a NON-NetBird server (no peer)
// must not make any NetBird peer call (the reconcile is gated on a linked peer).
func TestUpdateServerNonNetbirdNoPeerCall(t *testing.T) {
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	svc, _ := newNetbirdServerTestService(t, now)
	fake := newFakeNetbird(t)
	enableNetbird(t, svc, fake.srv.URL, true)
	dto, err := svc.CreateServer(ctx, adminToken(), CreateServerRequest{Name: "plain", Domain: "plain.example", OwnerIDs: []string{"usr_owner"}, AdminGroupIDs: []string{testAdminGroupID}})
	if err != nil {
		t.Fatalf("CreateServer: %v", err)
	}
	name := "plain-renamed"
	if _, err := svc.UpdateServer(ctx, systemAdminToken(), dto.ID, UpdateServerRequest{Name: &name}); err != nil {
		t.Fatalf("UpdateServer: %v", err)
	}
	if len(fake.peerRenames) != 0 {
		t.Fatalf("a non-NetBird server rename must not touch any peer, got %v", fake.peerRenames)
	}
}
