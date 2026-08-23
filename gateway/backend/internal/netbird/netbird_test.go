// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package netbird

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func testCfg(url string) Config { return Config{URL: url, Token: "tok-123"} }

func TestCreateSetupKey(t *testing.T) {
	var (
		gotMethod, gotPath, gotAuth, gotContentType string
		gotBody                                     map[string]any
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotContentType = r.Header.Get("Content-Type")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"sk-1","key":"FULL-PLAINTEXT-KEY","name":"srv-a","type":"one-off"}`))
	}))
	defer srv.Close()

	sk, err := CreateSetupKey(context.Background(), testCfg(srv.URL), time.Second,
		SetupKeyParams{Name: "srv-a", AutoGroups: []string{"g1"}})
	if err != nil {
		t.Fatalf("CreateSetupKey: %v", err)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if gotPath != "/api/setup-keys" {
		t.Errorf("path = %q, want /api/setup-keys", gotPath)
	}
	if gotAuth != "Token tok-123" {
		t.Errorf("auth header = %q, want %q", gotAuth, "Token tok-123")
	}
	if gotContentType != "application/json" {
		t.Errorf("content-type = %q, want application/json", gotContentType)
	}
	if gotBody["type"] != "one-off" {
		t.Errorf("body type = %v, want one-off", gotBody["type"])
	}
	if gotBody["usage_limit"] != float64(1) {
		t.Errorf("body usage_limit = %v, want 1", gotBody["usage_limit"])
	}
	if gotBody["name"] != "srv-a" {
		t.Errorf("body name = %v, want srv-a", gotBody["name"])
	}
	ag, ok := gotBody["auto_groups"].([]any)
	if !ok || len(ag) != 1 || ag[0] != "g1" {
		t.Errorf("body auto_groups = %v, want [g1]", gotBody["auto_groups"])
	}
	if sk.ID != "sk-1" || sk.Key != "FULL-PLAINTEXT-KEY" {
		t.Errorf("SetupKey = %+v, want {sk-1 FULL-PLAINTEXT-KEY}", sk)
	}
}

func TestCreateSetupKeyNilAutoGroupsSerializesEmptyArray(t *testing.T) {
	var rawBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rawBody, _ = io.ReadAll(r.Body)
		_, _ = w.Write([]byte(`{"id":"sk-2","key":"k"}`))
	}))
	defer srv.Close()

	if _, err := CreateSetupKey(context.Background(), testCfg(srv.URL), time.Second,
		SetupKeyParams{Name: "srv-b", AutoGroups: nil}); err != nil {
		t.Fatalf("CreateSetupKey: %v", err)
	}
	// Must serialize as an empty array, never null.
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(rawBody, &decoded); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if got := string(decoded["auto_groups"]); got != "[]" {
		t.Errorf("auto_groups raw = %s, want []", got)
	}
}

func TestResolveGroupID(t *testing.T) {
	t.Run("existing returns id without POST", func(t *testing.T) {
		posted := false
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPost {
				posted = true
			}
			if r.Method == http.MethodGet && r.URL.Path == "/api/groups" {
				_, _ = w.Write([]byte(`[{"id":"g-other","name":"other"},{"id":"g-team","name":"team"}]`))
				return
			}
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer srv.Close()

		id, err := ResolveGroupID(context.Background(), testCfg(srv.URL), time.Second, "team")
		if err != nil {
			t.Fatalf("ResolveGroupID: %v", err)
		}
		if id != "g-team" {
			t.Errorf("id = %q, want g-team", id)
		}
		if posted {
			t.Error("unexpected POST for an existing group")
		}
	})

	t.Run("missing creates and returns new id", func(t *testing.T) {
		posted := false
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.Method == http.MethodGet && r.URL.Path == "/api/groups":
				_, _ = w.Write([]byte(`[{"id":"g-other","name":"other"}]`))
			case r.Method == http.MethodPost && r.URL.Path == "/api/groups":
				posted = true
				var body map[string]any
				_ = json.NewDecoder(r.Body).Decode(&body)
				if body["name"] != "team" {
					t.Errorf("create body name = %v, want team", body["name"])
				}
				_, _ = w.Write([]byte(`{"id":"g-new","name":"team"}`))
			default:
				w.WriteHeader(http.StatusInternalServerError)
			}
		}))
		defer srv.Close()

		id, err := ResolveGroupID(context.Background(), testCfg(srv.URL), time.Second, "team")
		if err != nil {
			t.Fatalf("ResolveGroupID: %v", err)
		}
		if id != "g-new" {
			t.Errorf("id = %q, want g-new", id)
		}
		if !posted {
			t.Error("expected POST to create the missing group")
		}
	})

	t.Run("empty name returns empty with no call", func(t *testing.T) {
		called := false
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			called = true
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer srv.Close()

		id, err := ResolveGroupID(context.Background(), testCfg(srv.URL), time.Second, "")
		if err != nil {
			t.Fatalf("ResolveGroupID: %v", err)
		}
		if id != "" {
			t.Errorf("id = %q, want empty", id)
		}
		if called {
			t.Error("unexpected API call for an empty group name")
		}
	})
}

func TestGetGroupParsesPeers(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = w.Write([]byte(`{"id":"g1","name":"team","peers":[{"id":"p1","name":"peerA"},{"id":"p2","name":"peerB"}]}`))
	}))
	defer srv.Close()

	g, err := GetGroup(context.Background(), testCfg(srv.URL), time.Second, "g1")
	if err != nil {
		t.Fatalf("GetGroup: %v", err)
	}
	if gotPath != "/api/groups/g1" {
		t.Errorf("path = %q, want /api/groups/g1", gotPath)
	}
	if len(g.Peers) != 2 {
		t.Fatalf("peers len = %d, want 2", len(g.Peers))
	}
	if g.Peers[0].ID != "p1" || g.Peers[0].Name != "peerA" {
		t.Errorf("peers[0] = %+v, want {p1 peerA}", g.Peers[0])
	}
	if g.Peers[1].ID != "p2" || g.Peers[1].Name != "peerB" {
		t.Errorf("peers[1] = %+v, want {p2 peerB}", g.Peers[1])
	}
}

func TestListPeersParsesFields(t *testing.T) {
	var gotMethod, gotPath, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`[
			{"id":"peer-1","name":"gpu-box","dns_label":"gpu-box.netbird.cloud","connected":true},
			{"id":"peer-2","name":"cpu-box","dns_label":"cpu-box.netbird.cloud","connected":false}
		]`))
	}))
	defer srv.Close()

	peers, err := ListPeers(context.Background(), testCfg(srv.URL), time.Second)
	if err != nil {
		t.Fatalf("ListPeers: %v", err)
	}
	if gotMethod != http.MethodGet {
		t.Errorf("method = %q, want GET", gotMethod)
	}
	if gotPath != "/api/peers" {
		t.Errorf("path = %q, want /api/peers", gotPath)
	}
	if gotAuth != "Token tok-123" {
		t.Errorf("auth header = %q, want %q", gotAuth, "Token tok-123")
	}
	if len(peers) != 2 {
		t.Fatalf("peers len = %d, want 2", len(peers))
	}
	if peers[0].ID != "peer-1" || peers[0].Name != "gpu-box" {
		t.Errorf("peers[0] = %+v, want {peer-1 gpu-box}", peers[0])
	}
	if peers[0].DNSLabel != "gpu-box.netbird.cloud" || !peers[0].Connected {
		t.Errorf("peers[0] dns/connected = %q/%v, want gpu-box.netbird.cloud/true", peers[0].DNSLabel, peers[0].Connected)
	}
	if peers[1].DNSLabel != "cpu-box.netbird.cloud" || peers[1].Connected {
		t.Errorf("peers[1] dns/connected = %q/%v, want cpu-box.netbird.cloud/false", peers[1].DNSLabel, peers[1].Connected)
	}
}

func TestGetPeerParsesFields(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = w.Write([]byte(`{
			"id":"peer-1",
			"name":"gpu-box",
			"dns_label":"gpu-box.netbird.cloud",
			"connected":true,
			"ssh_enabled":true,
			"login_expiration_enabled":false,
			"inactivity_expiration_enabled":true,
			"last_seen":"2024-01-02T03:04:05Z"
		}`))
	}))
	defer srv.Close()

	p, err := GetPeer(context.Background(), testCfg(srv.URL), time.Second, "peer-1")
	if err != nil {
		t.Fatalf("GetPeer: %v", err)
	}
	if gotPath != "/api/peers/peer-1" {
		t.Errorf("path = %q, want /api/peers/peer-1", gotPath)
	}
	if p.DNSLabel != "gpu-box.netbird.cloud" {
		t.Errorf("dns_label = %q", p.DNSLabel)
	}
	if p.LastSeen != "2024-01-02T03:04:05Z" {
		t.Errorf("last_seen = %q, want 2024-01-02T03:04:05Z", p.LastSeen)
	}
	if !p.Connected {
		t.Error("connected = false, want true")
	}
	if !p.SSHEnabled {
		t.Error("ssh_enabled = false, want true")
	}
	if p.LoginExpirationEnabled {
		t.Error("login_expiration_enabled = true, want false")
	}
	if !p.InactivityExpirationEnabled {
		t.Error("inactivity_expiration_enabled = false, want true")
	}
}

func TestGetPeerParsesIP(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{
			"id":"peer-1",
			"name":"gpu-box",
			"dns_label":"gpu-box.netbird.cloud",
			"ip":"100.92.0.7",
			"connected":true
		}`))
	}))
	defer srv.Close()

	p, err := GetPeer(context.Background(), testCfg(srv.URL), time.Second, "peer-1")
	if err != nil {
		t.Fatalf("GetPeer: %v", err)
	}
	if p.IP != "100.92.0.7" {
		t.Errorf("ip = %q, want 100.92.0.7", p.IP)
	}
}

func TestUpdatePeerNamePreservesFlags(t *testing.T) {
	var (
		gotMethod, gotPath string
		gotBody            map[string]any
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_, _ = w.Write([]byte(`{"id":"peer-1","name":"new-name","dns_label":"nn.netbird.cloud","connected":true,"ssh_enabled":true,"login_expiration_enabled":false,"inactivity_expiration_enabled":true}`))
	}))
	defer srv.Close()

	existing := Peer{
		ID:                          "peer-1",
		Name:                        "old-name",
		SSHEnabled:                  true,
		LoginExpirationEnabled:      false,
		InactivityExpirationEnabled: true,
	}
	updated, err := UpdatePeerName(context.Background(), testCfg(srv.URL), time.Second, existing, "new-name")
	if err != nil {
		t.Fatalf("UpdatePeerName: %v", err)
	}
	if gotMethod != http.MethodPut {
		t.Errorf("method = %q, want PUT", gotMethod)
	}
	if gotPath != "/api/peers/peer-1" {
		t.Errorf("path = %q, want /api/peers/peer-1", gotPath)
	}
	if gotBody["name"] != "new-name" {
		t.Errorf("body name = %v, want new-name", gotBody["name"])
	}
	if gotBody["ssh_enabled"] != true {
		t.Errorf("body ssh_enabled = %v, want true (preserved)", gotBody["ssh_enabled"])
	}
	if gotBody["login_expiration_enabled"] != false {
		t.Errorf("body login_expiration_enabled = %v, want false (preserved)", gotBody["login_expiration_enabled"])
	}
	if gotBody["inactivity_expiration_enabled"] != true {
		t.Errorf("body inactivity_expiration_enabled = %v, want true (preserved)", gotBody["inactivity_expiration_enabled"])
	}
	if updated.Name != "new-name" {
		t.Errorf("updated name = %q, want new-name", updated.Name)
	}
}

func TestGetPeerParsesGroups(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{
			"id":"peer-1",
			"name":"gpu-box",
			"groups":[{"id":"g1","name":"gateways"},{"id":"g2","name":"prod"}]
		}`))
	}))
	defer srv.Close()

	p, err := GetPeer(context.Background(), testCfg(srv.URL), time.Second, "peer-1")
	if err != nil {
		t.Fatalf("GetPeer: %v", err)
	}
	if len(p.Groups) != 2 {
		t.Fatalf("groups len = %d, want 2", len(p.Groups))
	}
	if p.Groups[0].ID != "g1" || p.Groups[0].Name != "gateways" {
		t.Errorf("groups[0] = %+v, want {g1 gateways}", p.Groups[0])
	}
	if p.Groups[1].ID != "g2" || p.Groups[1].Name != "prod" {
		t.Errorf("groups[1] = %+v, want {g2 prod}", p.Groups[1])
	}
}

func TestUpdateGroupPeers(t *testing.T) {
	var (
		gotMethod, gotPath string
		gotBody            map[string]any
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_, _ = w.Write([]byte(`{"id":"g1","name":"team","peers":[{"id":"p1","name":"a"},{"id":"p2","name":"b"}]}`))
	}))
	defer srv.Close()

	updated, err := UpdateGroupPeers(context.Background(), testCfg(srv.URL), time.Second,
		Group{ID: "g1", Name: "team"}, []string{"p1", "p2"})
	if err != nil {
		t.Fatalf("UpdateGroupPeers: %v", err)
	}
	if gotMethod != http.MethodPut {
		t.Errorf("method = %q, want PUT", gotMethod)
	}
	if gotPath != "/api/groups/g1" {
		t.Errorf("path = %q, want /api/groups/g1", gotPath)
	}
	if gotBody["name"] != "team" {
		t.Errorf("body name = %v, want team", gotBody["name"])
	}
	peers, ok := gotBody["peers"].([]any)
	if !ok || len(peers) != 2 || peers[0] != "p1" || peers[1] != "p2" {
		t.Errorf("body peers = %v, want [p1 p2]", gotBody["peers"])
	}
	if len(updated.Peers) != 2 {
		t.Errorf("updated peers len = %d, want 2", len(updated.Peers))
	}
}

func TestUpdateGroupPeersNilSerializesEmptyArray(t *testing.T) {
	var rawBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rawBody, _ = io.ReadAll(r.Body)
		_, _ = w.Write([]byte(`{"id":"g1","name":"team"}`))
	}))
	defer srv.Close()

	if _, err := UpdateGroupPeers(context.Background(), testCfg(srv.URL), time.Second,
		Group{ID: "g1", Name: "team"}, nil); err != nil {
		t.Fatalf("UpdateGroupPeers: %v", err)
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(rawBody, &decoded); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if got := string(decoded["peers"]); got != "[]" {
		t.Errorf("peers raw = %s, want []", got)
	}
}

// setPeerGroupsFake serves GET/PUT /api/groups/{id} over an in-memory group map,
// recording the peers list of every PUT so a test can assert the exact push.
type setPeerGroupsFake struct {
	mu       sync.Mutex
	groups   map[string][]string // group id -> peer ids
	puts     map[string][]string // group id -> last PUT peers list
	putNames map[string]string   // group id -> last PUT name (must equal the real name)
}

func newSetPeerGroupsFake(groups map[string][]string) *setPeerGroupsFake {
	return &setPeerGroupsFake{groups: groups, puts: map[string][]string{}, putNames: map[string]string{}}
}

func (f *setPeerGroupsFake) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/api/groups/")
		f.mu.Lock()
		defer f.mu.Unlock()
		peers, ok := f.groups[id]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.Method == http.MethodGet {
			out := map[string]any{"id": id, "name": "grp-" + id, "peers": peerObjs(peers)}
			_ = json.NewEncoder(w).Encode(out)
			return
		}
		// PUT: record + apply the new peers list.
		var body struct {
			Name  string   `json:"name"`
			Peers []string `json:"peers"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.puts[id] = body.Peers
		f.putNames[id] = body.Name
		f.groups[id] = body.Peers
		_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "name": body.Name, "peers": peerObjs(body.Peers)})
	}
}

func peerObjs(ids []string) []map[string]any {
	out := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		out = append(out, map[string]any{"id": id, "name": "n-" + id})
	}
	return out
}

func TestSetPeerGroupsAddsAndRemoves(t *testing.T) {
	fake := newSetPeerGroupsFake(map[string][]string{
		"ga": {"p9"},       // add p1 -> [p9,p1]
		"gb": {"p1", "p9"}, // add p1 -> already present -> no PUT
		"gc": {"p1", "p9"}, // remove p1 -> [p9]
		"gd": {"p9"},       // remove p1 -> not present -> no PUT
	})
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	err := SetPeerGroups(context.Background(), testCfg(srv.URL), time.Second, "p1",
		[]string{"ga", "gb"}, []string{"gc", "gd"})
	if err != nil {
		t.Fatalf("SetPeerGroups: %v", err)
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if _, put := fake.puts["gb"]; put {
		t.Error("gb was PUT, want no-op (peer already a member)")
	}
	if _, put := fake.puts["gd"]; put {
		t.Error("gd was PUT, want no-op (peer not a member)")
	}
	ga, ok := fake.puts["ga"]
	if !ok || len(ga) != 2 || ga[0] != "p9" || ga[1] != "p1" {
		t.Errorf("ga PUT peers = %v, want [p9 p1]", ga)
	}
	gc, ok := fake.puts["gc"]
	if !ok || len(gc) != 1 || gc[0] != "p9" {
		t.Errorf("gc PUT peers = %v, want [p9]", gc)
	}
	// The PUT must carry the group's REAL name (the fake serves "grp-<id>" on GET).
	// A regression that PUTs a blank name would rename the NetBird group.
	if got := fake.putNames["ga"]; got != "grp-ga" {
		t.Errorf("ga PUT name = %q, want grp-ga (blank name would rename the group)", got)
	}
	if got := fake.putNames["gc"]; got != "grp-gc" {
		t.Errorf("gc PUT name = %q, want grp-gc (blank name would rename the group)", got)
	}
}

func TestSetPeerGroupsPerGroupBestEffort(t *testing.T) {
	// gbad 500s on GET; gok must still be updated. The joined error is non-nil.
	var mu sync.Mutex
	gokPeers := []string{"p9"}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/api/groups/")
		if id == "gbad" {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"message":"boom"}`))
			return
		}
		mu.Lock()
		defer mu.Unlock()
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`{"id":"gok","name":"ok","peers":[{"id":"p9","name":"n"}]}`))
			return
		}
		var body struct {
			Peers []string `json:"peers"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		gokPeers = body.Peers
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "gok"})
	}))
	defer srv.Close()

	err := SetPeerGroups(context.Background(), testCfg(srv.URL), time.Second, "p1",
		[]string{"gbad", "gok"}, nil)
	if err == nil {
		t.Fatal("expected a joined error from the failing group")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(gokPeers) != 2 || gokPeers[1] != "p1" {
		t.Errorf("gok peers = %v, want [p9 p1] (the good group is still updated)", gokPeers)
	}
}

func TestDeleteGroup(t *testing.T) {
	t.Run("2xx returns nil", func(t *testing.T) {
		var gotMethod, gotPath string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotMethod = r.Method
			gotPath = r.URL.Path
			w.WriteHeader(http.StatusOK)
		}))
		defer srv.Close()
		if err := DeleteGroup(context.Background(), testCfg(srv.URL), time.Second, "g1"); err != nil {
			t.Fatalf("DeleteGroup(200) = %v, want nil", err)
		}
		if gotMethod != http.MethodDelete {
			t.Errorf("method = %q, want DELETE", gotMethod)
		}
		if gotPath != "/api/groups/g1" {
			t.Errorf("path = %q, want /api/groups/g1", gotPath)
		}
	})

	t.Run("404 returns nil (idempotent)", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"not found"}`))
		}))
		defer srv.Close()
		if err := DeleteGroup(context.Background(), testCfg(srv.URL), time.Second, "gone"); err != nil {
			t.Fatalf("DeleteGroup(404) = %v, want nil", err)
		}
	})

	t.Run("500 returns error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"message":"referenced by policy"}`))
		}))
		defer srv.Close()
		err := DeleteGroup(context.Background(), testCfg(srv.URL), time.Second, "g1")
		if err == nil {
			t.Fatal("DeleteGroup(500) = nil, want error")
		}
		if !strings.Contains(err.Error(), "500") || strings.Contains(err.Error(), "tok-123") {
			t.Errorf("error %q should carry the status and never the token", err.Error())
		}
	})
}

// runDeleteContract exercises the shared idempotent-delete contract (mirrors
// TestDeleteGroup) against a DeletePeer/DeleteSetupKey-shaped func: DELETE method,
// the exact url-escaped path, the "Token <token>" auth header, 2xx/204 -> nil,
// 404 -> nil (idempotent), 401/403 -> ErrAuth, and a 500 whose error carries the
// status but never the token.
func runDeleteContract(t *testing.T, del func(context.Context, Config, time.Duration, string) error, pathBase string) {
	t.Helper()

	t.Run("200 returns nil, sends DELETE + Token auth to the exact path", func(t *testing.T) {
		var gotMethod, gotPath, gotAuth string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotMethod = r.Method
			gotPath = r.URL.Path
			gotAuth = r.Header.Get("Authorization")
			w.WriteHeader(http.StatusOK)
		}))
		defer srv.Close()
		if err := del(context.Background(), testCfg(srv.URL), time.Second, "p1"); err != nil {
			t.Fatalf("del(200) = %v, want nil", err)
		}
		if gotMethod != http.MethodDelete {
			t.Errorf("method = %q, want DELETE", gotMethod)
		}
		if gotPath != pathBase+"p1" {
			t.Errorf("path = %q, want %q", gotPath, pathBase+"p1")
		}
		if gotAuth != "Token tok-123" {
			t.Errorf("auth header = %q, want %q", gotAuth, "Token tok-123")
		}
	})

	t.Run("204 returns nil", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		}))
		defer srv.Close()
		if err := del(context.Background(), testCfg(srv.URL), time.Second, "p1"); err != nil {
			t.Fatalf("del(204) = %v, want nil", err)
		}
	})

	t.Run("404 returns nil (idempotent)", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"not found"}`))
		}))
		defer srv.Close()
		if err := del(context.Background(), testCfg(srv.URL), time.Second, "gone"); err != nil {
			t.Fatalf("del(404) = %v, want nil (idempotent)", err)
		}
	})

	t.Run("401 returns ErrAuth", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"message":"unauthorized"}`))
		}))
		defer srv.Close()
		if err := del(context.Background(), testCfg(srv.URL), time.Second, "p1"); !errors.Is(err, ErrAuth) {
			t.Fatalf("del(401) = %v, want ErrAuth", err)
		}
	})

	t.Run("403 returns ErrAuth", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusForbidden)
		}))
		defer srv.Close()
		if err := del(context.Background(), testCfg(srv.URL), time.Second, "p1"); !errors.Is(err, ErrAuth) {
			t.Fatalf("del(403) = %v, want ErrAuth", err)
		}
	})

	t.Run("500 returns error carrying status, never the token", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"message":"boom"}`))
		}))
		defer srv.Close()
		err := del(context.Background(), testCfg(srv.URL), time.Second, "p1")
		if err == nil {
			t.Fatal("del(500) = nil, want error")
		}
		if errors.Is(err, ErrAuth) {
			t.Fatalf("500 must not be ErrAuth: %v", err)
		}
		if !strings.Contains(err.Error(), "500") || strings.Contains(err.Error(), "tok-123") {
			t.Errorf("error %q should carry the status and never the token", err.Error())
		}
	})

	t.Run("id is url-escaped in the request path", func(t *testing.T) {
		var gotEscaped string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotEscaped = r.URL.EscapedPath()
			w.WriteHeader(http.StatusOK)
		}))
		defer srv.Close()
		// "a/b c" -> url.PathEscape -> "a%2Fb%20c"; an unescaped path would keep the
		// literal slash (and the fragment/query-unsafe chars), so this discriminates
		// the drop-PathEscape mutation.
		if err := del(context.Background(), testCfg(srv.URL), time.Second, "a/b c"); err != nil {
			t.Fatalf("del(escape) = %v, want nil", err)
		}
		want := pathBase + "a%2Fb%20c"
		if gotEscaped != want {
			t.Errorf("escaped path = %q, want %q (id must be url.PathEscape'd)", gotEscaped, want)
		}
	})
}

func TestDeletePeer(t *testing.T) {
	runDeleteContract(t, DeletePeer, "/api/peers/")
}

func TestDeleteSetupKey(t *testing.T) {
	runDeleteContract(t, DeleteSetupKey, "/api/setup-keys/")
}

func TestPing(t *testing.T) {
	t.Run("200 returns nil", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`[]`))
		}))
		defer srv.Close()
		if err := Ping(context.Background(), testCfg(srv.URL), time.Second); err != nil {
			t.Fatalf("Ping: %v", err)
		}
	})

	t.Run("401 returns ErrAuth", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"message":"unauthorized"}`))
		}))
		defer srv.Close()
		if err := Ping(context.Background(), testCfg(srv.URL), time.Second); !errors.Is(err, ErrAuth) {
			t.Fatalf("Ping err = %v, want ErrAuth", err)
		}
	})

	t.Run("403 returns ErrAuth", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusForbidden)
		}))
		defer srv.Close()
		if err := Ping(context.Background(), testCfg(srv.URL), time.Second); !errors.Is(err, ErrAuth) {
			t.Fatalf("Ping err = %v, want ErrAuth", err)
		}
	})
}

func TestNon2xxCarriesStatusAndMessage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"message":"expires_in out of range"}`))
	}))
	defer srv.Close()

	_, err := CreateSetupKey(context.Background(), testCfg(srv.URL), time.Second, SetupKeyParams{Name: "x"})
	if err == nil {
		t.Fatal("expected an error for a 400 response")
	}
	if errors.Is(err, ErrAuth) {
		t.Fatalf("400 must not be ErrAuth: %v", err)
	}
	msg := err.Error()
	if !strings.Contains(msg, "400") || !strings.Contains(msg, "expires_in out of range") {
		t.Errorf("error %q should carry the status and body message", msg)
	}
	if strings.Contains(msg, "tok-123") {
		t.Errorf("error %q must NOT contain the token", msg)
	}
}

func TestTimeoutDoesNotHang(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Block until the client cancels (timeout) OR a bounded fallback fires, so
		// srv.Close() never deadlocks.
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	}))
	defer srv.Close()

	start := time.Now()
	err := Ping(context.Background(), testCfg(srv.URL), 50*time.Millisecond)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected a timeout error")
	}
	if errors.Is(err, ErrAuth) {
		t.Fatalf("timeout must not be ErrAuth: %v", err)
	}
	if elapsed > time.Second {
		t.Fatalf("call took %v, expected it to respect the 50ms timeout", elapsed)
	}
}

func TestDefaultTimeoutWhenNonPositive(t *testing.T) {
	// A non-positive timeout must not use http.DefaultClient (which has no timeout) —
	// it falls back to a bounded default.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()
	if err := Ping(context.Background(), testCfg(srv.URL), 0); err != nil {
		t.Fatalf("Ping with zero timeout: %v", err)
	}
	if c := newClient(0); c.Timeout != defaultTimeout {
		t.Errorf("newClient(0).Timeout = %v, want %v", c.Timeout, defaultTimeout)
	}
	if c := newClient(-1); c.Timeout != defaultTimeout {
		t.Errorf("newClient(-1).Timeout = %v, want %v", c.Timeout, defaultTimeout)
	}
}

// TestCanonicalGroupsJSON verifies the shared canonical form used by BOTH the
// sync mirror and the editor push: sorted by id (then name), stable regardless
// of input order, "" for empty/nil, and a non-mutating copy.
func TestCanonicalGroupsJSON(t *testing.T) {
	t.Run("empty and nil -> empty string", func(t *testing.T) {
		for _, in := range [][]GroupRef{nil, {}} {
			got, err := CanonicalGroupsJSON(in)
			if err != nil || got != "" {
				t.Fatalf("CanonicalGroupsJSON(%v) = %q, %v; want \"\", nil", in, got, err)
			}
		}
	})

	t.Run("sorted by id, input order irrelevant", func(t *testing.T) {
		a := []GroupRef{{ID: "gB", Name: "B"}, {ID: "gA", Name: "A"}}
		b := []GroupRef{{ID: "gA", Name: "A"}, {ID: "gB", Name: "B"}}
		want := `[{"id":"gA","name":"A"},{"id":"gB","name":"B"}]`
		for _, in := range [][]GroupRef{a, b} {
			got, err := CanonicalGroupsJSON(in)
			if err != nil {
				t.Fatalf("CanonicalGroupsJSON: %v", err)
			}
			if got != want {
				t.Fatalf("CanonicalGroupsJSON(%v) = %q, want %q", in, got, want)
			}
		}
	})

	t.Run("does not mutate input", func(t *testing.T) {
		in := []GroupRef{{ID: "gB", Name: "B"}, {ID: "gA", Name: "A"}}
		if _, err := CanonicalGroupsJSON(in); err != nil {
			t.Fatalf("CanonicalGroupsJSON: %v", err)
		}
		if in[0].ID != "gB" || in[1].ID != "gA" {
			t.Fatalf("input was mutated: %+v", in)
		}
	})
}

func TestListPolicies(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Token tok-123" {
			t.Fatalf("auth header = %q", got)
		}
		if r.Method != http.MethodGet || r.URL.Path != "/api/policies" {
			t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
		}
		_, _ = w.Write([]byte(`[{"id":"p1","name":"op-gw-access-s1","enabled":true,"rules":[{"id":"r1","name":"op-gw-access-s1","enabled":true,"action":"accept","bidirectional":false,"protocol":"tcp","ports":["8080"],"sources":[{"id":"g-gw","name":"op-gw-portal"}],"destinations":[{"id":"g-s1","name":"op-gw-s1"}]}]}]`))
	}))
	defer srv.Close()
	got, err := ListPolicies(context.Background(), Config{URL: srv.URL, Token: "tok-123"}, time.Second)
	if err != nil {
		t.Fatalf("ListPolicies: %v", err)
	}
	if len(got) != 1 || got[0].Name != "op-gw-access-s1" || len(got[0].Rules) != 1 {
		t.Fatalf("unexpected policies: %+v", got)
	}
	if got[0].Rules[0].Sources[0].ID != "g-gw" || got[0].Rules[0].Destinations[0].ID != "g-s1" {
		t.Fatalf("sources/destinations decode wrong: %+v", got[0].Rules[0])
	}
	if got[0].Rules[0].Ports[0] != "8080" {
		t.Fatalf("ports decode wrong: %+v", got[0].Rules[0].Ports)
	}
}

func TestCreatePolicyRequestBodyShape(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/policies" {
			t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		_, _ = w.Write([]byte(`{"id":"p1","name":"op-gw-access-s1","enabled":true,"rules":[{"id":"r1"}]}`))
	}))
	defer srv.Close()
	req := PolicyRequest{
		Name:    "op-gw-access-s1",
		Enabled: true,
		Rules: []PolicyRuleRequest{{
			Name: "op-gw-access-s1", Enabled: true, Action: "accept", Bidirectional: false,
			Protocol: "tcp", Ports: []string{"8080"}, Sources: []string{"g-gw"}, Destinations: []string{"g-s1"},
		}},
	}
	got, err := CreatePolicy(context.Background(), Config{URL: srv.URL, Token: "tok-123"}, time.Second, req)
	if err != nil {
		t.Fatalf("CreatePolicy: %v", err)
	}
	if got.ID != "p1" {
		t.Fatalf("decode id = %q", got.ID)
	}
	rules, _ := body["rules"].([]any)
	rule0, _ := rules[0].(map[string]any)
	if src, _ := rule0["sources"].([]any); len(src) != 1 || src[0] != "g-gw" {
		t.Fatalf("sources not string-ID array: %v", rule0["sources"])
	}
	if dst, _ := rule0["destinations"].([]any); len(dst) != 1 || dst[0] != "g-s1" {
		t.Fatalf("destinations not string-ID array: %v", rule0["destinations"])
	}
}

func TestDeletePolicyContract(t *testing.T) {
	runDeleteContract(t, DeletePolicy, "/api/policies/")
}

func TestUpdatePolicyRequestBodyShape(t *testing.T) {
	var (
		gotMethod, gotPath string
		body               map[string]any
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&body)
		_, _ = w.Write([]byte(`{"id":"p1","name":"op-gw-access-s1","enabled":true,"rules":[{"id":"r1"}]}`))
	}))
	defer srv.Close()
	req := PolicyRequest{
		Name:    "op-gw-access-s1",
		Enabled: true,
		Rules: []PolicyRuleRequest{{
			Name: "op-gw-access-s1", Enabled: true, Action: "accept", Bidirectional: false,
			Protocol: "tcp", Ports: []string{"8080"}, Sources: []string{"g-gw"}, Destinations: []string{"g-s1"},
		}},
	}
	got, err := UpdatePolicy(context.Background(), Config{URL: srv.URL, Token: "tok-123"}, time.Second, "p1", req)
	if err != nil {
		t.Fatalf("UpdatePolicy: %v", err)
	}
	if got.ID != "p1" {
		t.Fatalf("decode id = %q", got.ID)
	}
	if gotMethod != http.MethodPut {
		t.Errorf("method = %q, want PUT", gotMethod)
	}
	if gotPath != "/api/policies/p1" {
		t.Errorf("path = %q, want /api/policies/p1 (id must be url.PathEscape'd)", gotPath)
	}
	rules, _ := body["rules"].([]any)
	rule0, _ := rules[0].(map[string]any)
	if src, _ := rule0["sources"].([]any); len(src) != 1 || src[0] != "g-gw" {
		t.Fatalf("sources not string-ID array: %v", rule0["sources"])
	}
	if dst, _ := rule0["destinations"].([]any); len(dst) != 1 || dst[0] != "g-s1" {
		t.Fatalf("destinations not string-ID array: %v", rule0["destinations"])
	}
}

func TestGetPolicy(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = w.Write([]byte(`{"id":"p1","name":"op-gw-access-s1","enabled":true,"rules":[{"id":"r1","name":"op-gw-access-s1","enabled":true,"action":"accept","bidirectional":false,"protocol":"tcp","ports":["8080"],"sources":[{"id":"g-gw","name":"op-gw-portal"}],"destinations":[{"id":"g-s1","name":"op-gw-s1"}]}]}`))
	}))
	defer srv.Close()

	p, err := GetPolicy(context.Background(), testCfg(srv.URL), time.Second, "p1")
	if err != nil {
		t.Fatalf("GetPolicy: %v", err)
	}
	if gotPath != "/api/policies/p1" {
		t.Errorf("path = %q, want /api/policies/p1", gotPath)
	}
	if len(p.Rules) != 1 {
		t.Fatalf("rules len = %d, want 1", len(p.Rules))
	}
	rule := p.Rules[0]
	if len(rule.Sources) != 1 || rule.Sources[0].ID != "g-gw" || rule.Sources[0].Name != "op-gw-portal" {
		t.Errorf("sources = %+v, want [{g-gw op-gw-portal}]", rule.Sources)
	}
	if len(rule.Destinations) != 1 || rule.Destinations[0].ID != "g-s1" || rule.Destinations[0].Name != "op-gw-s1" {
		t.Errorf("destinations = %+v, want [{g-s1 op-gw-s1}]", rule.Destinations)
	}
	if len(rule.Ports) != 1 || rule.Ports[0] != "8080" {
		t.Errorf("ports = %+v, want [8080]", rule.Ports)
	}
}

func TestCreatePolicyNilSlicesSerializeEmptyArrays(t *testing.T) {
	var rawBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rawBody, _ = io.ReadAll(r.Body)
		_, _ = w.Write([]byte(`{"id":"p1","name":"op-gw-access-s1","enabled":true,"rules":[{"id":"r1"}]}`))
	}))
	defer srv.Close()

	req := PolicyRequest{
		Name:    "op-gw-access-s1",
		Enabled: true,
		Rules: []PolicyRuleRequest{{
			Name: "op-gw-access-s1", Enabled: true, Action: "accept", Bidirectional: false,
			Protocol: "tcp", Ports: nil, Sources: nil, Destinations: nil,
		}},
	}
	if _, err := CreatePolicy(context.Background(), testCfg(srv.URL), time.Second, req); err != nil {
		t.Fatalf("CreatePolicy: %v", err)
	}

	var decoded struct {
		Rules []map[string]json.RawMessage `json:"rules"`
	}
	if err := json.Unmarshal(rawBody, &decoded); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if len(decoded.Rules) != 1 {
		t.Fatalf("rules len = %d, want 1", len(decoded.Rules))
	}
	rule := decoded.Rules[0]
	if got := string(rule["ports"]); got != "[]" {
		t.Errorf("ports raw = %s, want []", got)
	}
	if got := string(rule["sources"]); got != "[]" {
		t.Errorf("sources raw = %s, want []", got)
	}
	if got := string(rule["destinations"]); got != "[]" {
		t.Errorf("destinations raw = %s, want []", got)
	}
}

// --- User + personal-access-token endpoints ---

func TestListUsersParsesCurrentAndServiceUser(t *testing.T) {
	var gotPath, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.String()
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`[{"id":"u1","is_current":false},{"id":"u2","is_current":true,"is_service_user":true}]`))
	}))
	defer srv.Close()

	users, err := ListUsers(context.Background(), testCfg(srv.URL), time.Second, nil)
	if err != nil {
		t.Fatalf("ListUsers: %v", err)
	}
	if gotPath != "/api/users" {
		t.Errorf("path = %q, want /api/users (no query when serviceUser is nil)", gotPath)
	}
	if gotAuth != "Token tok-123" {
		t.Errorf("auth header = %q, want %q", gotAuth, "Token tok-123")
	}
	if len(users) != 2 {
		t.Fatalf("len(users) = %d, want 2", len(users))
	}
	if users[0].ID != "u1" || users[0].IsCurrent || users[0].IsServiceUser {
		t.Errorf("users[0] = %+v, want {u1 false false}", users[0])
	}
	if users[1].ID != "u2" || !users[1].IsCurrent || !users[1].IsServiceUser {
		t.Errorf("users[1] = %+v, want {u2 true true}", users[1])
	}
}

func TestListUsersServiceUserFilterAppendsQuery(t *testing.T) {
	t.Run("true", func(t *testing.T) {
		var gotQuery string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotQuery = r.URL.RawQuery
			_, _ = w.Write([]byte(`[]`))
		}))
		defer srv.Close()
		svc := true
		if _, err := ListUsers(context.Background(), testCfg(srv.URL), time.Second, &svc); err != nil {
			t.Fatalf("ListUsers: %v", err)
		}
		if gotQuery != "service_user=true" {
			t.Errorf("query = %q, want service_user=true", gotQuery)
		}
	})
	t.Run("false", func(t *testing.T) {
		var gotQuery string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotQuery = r.URL.RawQuery
			_, _ = w.Write([]byte(`[]`))
		}))
		defer srv.Close()
		svc := false
		if _, err := ListUsers(context.Background(), testCfg(srv.URL), time.Second, &svc); err != nil {
			t.Fatalf("ListUsers: %v", err)
		}
		if gotQuery != "service_user=false" {
			t.Errorf("query = %q, want service_user=false", gotQuery)
		}
	})
}

func TestResolveCurrentUserIDPlainListHit(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.String())
		_, _ = w.Write([]byte(`[{"id":"u1","is_current":true}]`))
	}))
	defer srv.Close()
	id, err := ResolveCurrentUserID(context.Background(), testCfg(srv.URL), time.Second)
	if err != nil || id != "u1" {
		t.Fatalf("got %q, %v; want u1", id, err)
	}
	if len(paths) != 1 {
		t.Fatalf("expected 1 request (no service-user retry needed), got %v", paths)
	}
}

func TestResolveCurrentUserIDRetriesServiceUsers(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.String())
		if r.Header.Get("Authorization") != "Token tok-123" {
			t.Fatalf("missing token header")
		}
		if r.URL.Query().Get("service_user") == "true" {
			_, _ = w.Write([]byte(`[{"id":"su1","is_current":true,"is_service_user":true}]`))
			return
		}
		_, _ = w.Write([]byte(`[{"id":"u1","is_current":false}]`)) // no is_current on the plain list
	}))
	defer srv.Close()
	id, err := ResolveCurrentUserID(context.Background(), testCfg(srv.URL), time.Second)
	if err != nil || id != "su1" {
		t.Fatalf("got %q, %v; want su1", id, err)
	}
	if len(paths) != 2 { // plain, then ?service_user=true
		t.Fatalf("expected 2 requests, got %v", paths)
	}
}

func TestResolveCurrentUserIDUnknownWhenNeitherListHasCurrent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[{"id":"u1","is_current":false}]`))
	}))
	defer srv.Close()
	_, err := ResolveCurrentUserID(context.Background(), testCfg(srv.URL), time.Second)
	if !errors.Is(err, ErrCurrentUserUnknown) {
		t.Fatalf("err = %v, want ErrCurrentUserUnknown", err)
	}
}

func TestResolveCurrentUserIDPropagatesListError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	_, err := ResolveCurrentUserID(context.Background(), testCfg(srv.URL), time.Second)
	if !errors.Is(err, ErrAuth) {
		t.Fatalf("err = %v, want ErrAuth", err)
	}
}

func TestListTokensParsesExpirationAndLastUsed(t *testing.T) {
	var gotPath, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`[{"id":"t1","name":"op-gateway","expiration_date":"2027-01-01T00:00:00Z","last_used":"2026-08-01T00:00:00Z"}]`))
	}))
	defer srv.Close()
	toks, err := ListTokens(context.Background(), testCfg(srv.URL), time.Second, "u1")
	if err != nil {
		t.Fatalf("ListTokens: %v", err)
	}
	if gotPath != "/api/users/u1/tokens" {
		t.Errorf("path = %q, want /api/users/u1/tokens", gotPath)
	}
	if gotAuth != "Token tok-123" {
		t.Errorf("auth header = %q, want %q", gotAuth, "Token tok-123")
	}
	if len(toks) != 1 {
		t.Fatalf("len(toks) = %d, want 1", len(toks))
	}
	want := Token{ID: "t1", Name: "op-gateway", ExpirationDate: "2027-01-01T00:00:00Z", LastUsed: "2026-08-01T00:00:00Z"}
	if toks[0] != want {
		t.Errorf("toks[0] = %+v, want %+v", toks[0], want)
	}
}

func TestListTokensUserIDIsEscaped(t *testing.T) {
	var gotEscaped string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotEscaped = r.URL.EscapedPath()
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()
	if _, err := ListTokens(context.Background(), testCfg(srv.URL), time.Second, "a/b c"); err != nil {
		t.Fatalf("ListTokens: %v", err)
	}
	want := "/api/users/a%2Fb%20c/tokens"
	if gotEscaped != want {
		t.Errorf("escaped path = %q, want %q (userID must be url.PathEscape'd)", gotEscaped, want)
	}
}

func TestGetTokenParsesFields(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = w.Write([]byte(`{"id":"t1","name":"op-gateway","expiration_date":"2027-01-01T00:00:00Z","last_used":"2026-08-01T00:00:00Z"}`))
	}))
	defer srv.Close()
	tok, err := GetToken(context.Background(), testCfg(srv.URL), time.Second, "u1", "t1")
	if err != nil {
		t.Fatalf("GetToken: %v", err)
	}
	if gotPath != "/api/users/u1/tokens/t1" {
		t.Errorf("path = %q, want /api/users/u1/tokens/t1", gotPath)
	}
	want := Token{ID: "t1", Name: "op-gateway", ExpirationDate: "2027-01-01T00:00:00Z", LastUsed: "2026-08-01T00:00:00Z"}
	if tok != want {
		t.Errorf("tok = %+v, want %+v", tok, want)
	}
}

func TestGetTokenNotFoundIsAnError(t *testing.T) {
	// Unlike the deletes, GetToken has no idempotent-404 contract: a missing token
	// is a real error to the caller (e.g. status lookup).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"not found"}`))
	}))
	defer srv.Close()
	_, err := GetToken(context.Background(), testCfg(srv.URL), time.Second, "u1", "gone")
	if err == nil {
		t.Fatal("GetToken(404) = nil, want error")
	}
	if errors.Is(err, ErrAuth) {
		t.Fatalf("404 must not be ErrAuth: %v", err)
	}
}

func TestCreateTokenReadsPlainAndMetadata(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		if r.Method != http.MethodPost || r.URL.Path != "/api/users/u1/tokens" {
			t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["name"] != "op-gateway" || body["expires_in"].(float64) != 365 {
			t.Fatalf("bad body %v", body)
		}
		_, _ = w.Write([]byte(`{"plain_token":"nbp_new","personal_access_token":{"id":"t2","name":"op-gateway","expiration_date":"2027-08-03T00:00:00Z"}}`))
	}))
	defer srv.Close()
	plain, tok, err := CreateToken(context.Background(), testCfg(srv.URL), time.Second, "u1", "op-gateway", 365)
	if err != nil || plain != "nbp_new" || tok.ID != "t2" || tok.ExpirationDate != "2027-08-03T00:00:00Z" {
		t.Fatalf("got %q %+v %v", plain, tok, err)
	}
	if gotAuth != "Token tok-123" {
		t.Errorf("auth header = %q, want %q", gotAuth, "Token tok-123")
	}
}

func TestCreateTokenErrorNeverContainsToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"message":"expires_in out of range"}`))
	}))
	defer srv.Close()
	_, _, err := CreateToken(context.Background(), testCfg(srv.URL), time.Second, "u1", "op-gateway", 9999)
	if err == nil {
		t.Fatal("expected an error for a 400 response")
	}
	if strings.Contains(err.Error(), "tok-123") {
		t.Errorf("error %q must NOT contain the token", err.Error())
	}
}

func TestDeleteToken(t *testing.T) {
	del := func(ctx context.Context, cfg Config, timeout time.Duration, tokenID string) error {
		return DeleteToken(ctx, cfg, timeout, "u1", tokenID)
	}
	runDeleteContract(t, del, "/api/users/u1/tokens/")
}
