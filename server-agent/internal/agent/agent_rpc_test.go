// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package agent

import (
	"context"
	"encoding/json"
	"net"
	"net/netip"
	"op-ai-server-agent/internal/collector"
	"op-ai-server-agent/internal/config"
	"op-ai-server-agent/internal/sample"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	runtimectl "op-ai-server-agent/internal/runtime"
)

// rpcTelemetryAgent builds an agent over a fake runtime driver and member
// sink, with the config files dir pointing at a temp dir.
func rpcTelemetryAgent(t *testing.T, rows ...runtimectl.Status) (*Agent, *capturePoster, string) {
	t.Helper()
	poster := &capturePoster{}
	drv := newFakeRuntimeDriver()
	drv.setActive(true)
	drv.setStatuses(rows)
	dir := t.TempDir()
	a := NewFromDeps(config.Config{Interval: time.Hour, ConfigFilesDir: dir},
		Deps{Poster: poster, RuntimeDriver: drv, RPCMembers: &fakeMembers{}})
	t.Cleanup(a.relays.close)
	return a, poster, dir
}

func runtimeRow(t *testing.T, poster *capturePoster, specID string) sample.RuntimeSample {
	t.Helper()
	s := poster.last()
	if s == nil {
		t.Fatal("no sample posted")
	}
	for _, r := range s.Runtimes {
		if r.SpecID == specID {
			return r
		}
	}
	t.Fatalf("no runtime row for %s in %+v", specID, s.Runtimes)
	return sample.RuntimeSample{}
}

func TestSampleCarriesRPCStatus(t *testing.T) {
	held := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	until := held.Add(time.Minute)
	row := runtimectl.Status{
		SpecID: "rpcm_a", State: runtimectl.StateRunning, Kind: runtimectl.SpecKindRPCServer,
		RPCProtocol: "8.0.1",
		RPCMember:   &runtimectl.RPCMember{HeadSpecID: "rspec_h", Transport: "relay", BindIP: "127.0.0.1", Port: 1, CacheEnabled: true},
		RPC: &runtimectl.RPCStatus{
			HeldBy: "rspec_h", HeldSince: held, ReservedUntil: until, DeviceCount: 2, BindMismatch: false,
		},
	}
	a, poster, dir := rpcTelemetryAgent(t, row)

	cache := filepath.Join(dir, "rpc", "rspec_h")
	if err := os.MkdirAll(cache, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cache, "blob"), make([]byte, 3<<20), 0o600); err != nil {
		t.Fatal(err)
	}

	// Relay side: a refused peer, a stall and the relay's bind-mismatch flag;
	// observer side: another peer plus a duplicate of the relay's.
	a.relays.Refused("rpcm_a", net.ParseIP("100.64.0.9"))
	a.relays.Stalled("rpcm_a")
	a.relays.telMu.Lock()
	a.relays.telFor("rpcm_a").BindMismatch = true
	a.relays.telMu.Unlock()
	seen := time.Now()
	obsMember := &observedMember{}
	obsMember.breaches.note(seen, "100.64.0.9")
	obsMember.breaches.note(seen.Add(time.Second), "100.64.0.10")
	a.rpcObs.mu.Lock()
	a.rpcObs.members["rpcm_a"] = obsMember
	a.rpcObs.mu.Unlock()

	var got sample.RuntimeSample
	waitFor(t, "cache size reported", func() bool {
		a.collectOnce(context.Background())
		got = runtimeRow(t, poster, "rpcm_a")
		return got.RPC != nil && got.RPC.CacheMB == 3
	})
	if got.Kind != "rpc_server" {
		t.Fatalf("kind = %q", got.Kind)
	}
	r := got.RPC
	if r.HeldBy != "rspec_h" || !r.HeldSince.Equal(held) || !r.ReservedUntil.Equal(until) {
		t.Fatalf("hold = %+v", r)
	}
	if r.Protocol != "8.0.1" || r.DeviceCount != 2 || !r.BindMismatch || !r.Stalled {
		t.Fatalf("rpc = %+v", r)
	}
	peers := map[string]int{}
	for _, b := range r.Breaches {
		peers[b.Peer]++
	}
	if len(r.Breaches) != 2 || peers["100.64.0.9"] != 1 || peers["100.64.0.10"] != 1 {
		t.Fatalf("breaches must be the union deduped by peer, got %+v", r.Breaches)
	}
	raw, _ := json.Marshal(got)
	for _, key := range []string{
		`"kind":"rpc_server"`, `"held_by":"rspec_h"`, `"protocol":"8.0.1"`, `"device_count":2`,
		`"bind_mismatch":true`, `"cache_mb":3`, `"stalled":true`, `"reserved_until"`, `"breaches":[`,
	} {
		if !strings.Contains(string(raw), key) {
			t.Fatalf("wire form misses %s: %s", key, raw)
		}
	}
}

func TestSampleRPCBindMismatchFromRuntimeFlag(t *testing.T) {
	row := runtimectl.Status{
		SpecID: "rpcm_a", State: runtimectl.StateStopped, Kind: runtimectl.SpecKindRPCServer,
		RPCMember: &runtimectl.RPCMember{HeadSpecID: "rspec_h", Transport: "direct"},
		RPC:       &runtimectl.RPCStatus{BindMismatch: true},
	}
	a, poster, _ := rpcTelemetryAgent(t, row)
	a.collectOnce(context.Background())
	if r := runtimeRow(t, poster, "rpcm_a"); r.RPC == nil || !r.RPC.BindMismatch {
		t.Fatalf("the runtime's bind_mismatch flag was dropped: %+v", r.RPC)
	}
}

func TestSampleModelRowHasNoRPCKeys(t *testing.T) {
	row := runtimectl.Status{SpecID: "rspec_m", Model: "m", State: runtimectl.StateStopped}
	a, poster, _ := rpcTelemetryAgent(t, row)
	a.collectOnce(context.Background())
	raw, _ := json.Marshal(runtimeRow(t, poster, "rspec_m"))
	for _, key := range []string{`"kind"`, `"rpc"`, `"cluster"`, `"admission_blocked_by"`} {
		if strings.Contains(string(raw), key) {
			t.Fatalf("a model row carries %s: %s", key, raw)
		}
	}
}

func TestSampleCarriesClusterLaunch(t *testing.T) {
	row := runtimectl.Status{
		SpecID: "rspec_h", Model: "m", State: runtimectl.StateRunning,
		Cluster: &runtimectl.ClusterStatus{Launched: []string{"100.64.0.5:47001"}},
	}
	a, poster, _ := rpcTelemetryAgent(t, row)
	a.collectOnce(context.Background())
	r := runtimeRow(t, poster, "rspec_h")
	if r.Cluster == nil || len(r.Cluster.Launched) != 1 || r.Cluster.Launched[0] != "100.64.0.5:47001" {
		t.Fatalf("cluster = %+v", r.Cluster)
	}
	// The block carries the launch facts only: the agent records no cluster
	// version (the gateway knows every binary's from the inventory).
	raw, _ := json.Marshal(r.Cluster)
	if string(raw) != `{"launched":["100.64.0.5:47001"]}` {
		t.Fatalf("cluster wire form = %s", raw)
	}
}

func TestSampleCarriesAdmissionBlockedBy(t *testing.T) {
	row := runtimectl.Status{SpecID: "rspec_m", Model: "m", State: runtimectl.StateStopped, AdmissionBlockedBy: "rpcm_x"}
	a, poster, _ := rpcTelemetryAgent(t, row)
	a.collectOnce(context.Background())
	r := runtimeRow(t, poster, "rspec_m")
	if r.AdmissionBlockedBy != "rpcm_x" {
		t.Fatalf("admission_blocked_by = %q", r.AdmissionBlockedBy)
	}
	raw, _ := json.Marshal(r)
	if !strings.Contains(string(raw), `"admission_blocked_by":"rpcm_x"`) {
		t.Fatalf("wire form: %s", raw)
	}
}

func TestSampleCarriesLastErrorCode(t *testing.T) {
	row := runtimectl.Status{
		SpecID: "rspec_m", Model: "m", State: runtimectl.StateCrashed,
		LastError: &runtimectl.LastError{Message: "boom", Code: "runtime.device_backend_mismatch"},
	}
	a, poster, _ := rpcTelemetryAgent(t, row)
	a.collectOnce(context.Background())
	r := runtimeRow(t, poster, "rspec_m")
	if r.LastError == nil || r.LastError.Code != "runtime.device_backend_mismatch" {
		t.Fatalf("last_error = %+v", r.LastError)
	}
	raw, _ := json.Marshal(r)
	if !strings.Contains(string(raw), `"code":"runtime.device_backend_mismatch"`) {
		t.Fatalf("wire form: %s", raw)
	}
}

func TestCacheSizerIsCachedAndOffTheLoop(t *testing.T) {
	var walks atomic.Int32
	release := make(chan struct{})
	now := time.Unix(1000, 0)
	c := newCacheSizer(func(string) int64 {
		walks.Add(1)
		<-release
		return 5 << 20
	})
	c.now = func() time.Time { return now }

	if got := c.mb("d"); got != 0 {
		t.Fatalf("first read = %d, want 0 while the walk is in flight", got)
	}
	c.mb("d") // a second read must not start a second walk
	close(release)
	waitFor(t, "walk result", func() bool { return c.mb("d") == 5 })
	n := walks.Load()
	c.mb("d")
	now = now.Add(30 * time.Second)
	c.mb("d")
	time.Sleep(20 * time.Millisecond)
	if walks.Load() != n {
		t.Fatalf("walked again within the ttl: %d -> %d", n, walks.Load())
	}
	now = now.Add(31 * time.Second)
	c.mb("d")
	waitFor(t, "refresh after ttl", func() bool { return walks.Load() == n+1 })
}

func TestObserverPollAfterCancelTouchesNothing(t *testing.T) {
	f := newObsFixture(t, directMember())
	f.set(connFrom(obsHead, 5000))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f.obs.pollAll(ctx)
	if got := f.holds.last(); got != "<none>" {
		t.Fatalf("a poll after shutdown reported %q", got)
	}
}

func TestHeadResolverRefreshAfterCancelCallsNothing(t *testing.T) {
	lf := &lookupFake{ips: []netip.Addr{netip.MustParseAddr("192.168.1.10")}}
	var changed int
	r := newHeadResolver(lf.lookup, func(string) { changed++ })
	ctx, cancel := context.WithCancel(context.Background())
	r.refresh(context.Background(), map[string]string{"rpcm_a": "head.lan"})
	changed = 0
	cancel()
	r.refresh(ctx, map[string]string{})
	if changed != 0 {
		t.Fatalf("onChange ran %d times after cancellation", changed)
	}
}

func TestHeadResolverReportsLostDomain(t *testing.T) {
	lf := &lookupFake{ips: []netip.Addr{netip.MustParseAddr("192.168.1.10")}}
	var changed []string
	r := newHeadResolver(lf.lookup, func(id string) { changed = append(changed, id) })
	ctx := context.Background()
	r.refresh(ctx, map[string]string{"rpcm_a": "head.lan", "rpcm_b": "other.lan"})
	changed = nil
	// rpcm_a's domain disappears (its entry had IPs), rpcm_b never resolved
	// anything new and stays.
	r.refresh(ctx, map[string]string{"rpcm_b": "other.lan"})
	if len(changed) != 1 || changed[0] != "rpcm_a" {
		t.Fatalf("changed = %v, want [rpcm_a]: sessions from the old IPs must close", changed)
	}
	// An entry that never held addresses has nothing to close.
	lf.set(nil)
	changed = nil
	r.refresh(ctx, map[string]string{"rpcm_b": "other.lan", "rpcm_c": "none.lan"})
	changed = nil
	r.refresh(ctx, map[string]string{"rpcm_b": "other.lan"})
	if len(changed) != 0 {
		t.Fatalf("changed = %v for a domain that never resolved", changed)
	}
}

func TestRelaySyncAfterCloseIsNoOp(t *testing.T) {
	port := relayPort(t)
	table := &statusTable{}
	table.set(relayMemberRow("rpcm_a", runtimectl.StateRunning, echoChild(t), port, "127.0.0.1"))
	l := newTestRelayLink(t, table, &fakeMembers{})
	l.close()
	l.sync()
	time.Sleep(50 * time.Millisecond)
	if relayAccepts(port) {
		t.Fatal("sync after close brought a relay up")
	}
}

// TestRunWaitsForObserverGoroutines: Run returns only once an in-flight
// connection listing has finished, so nothing touches the relays or the
// runtime after it returned.
func TestRunWaitsForObserverGoroutines(t *testing.T) {
	drv := newFakeRuntimeDriver()
	drv.setStatuses([]runtimectl.Status{directMember()})
	a := NewFromDeps(config.Config{Interval: time.Hour}, Deps{Poster: &capturePoster{}, RuntimeDriver: drv, RPCMembers: &fakeMembers{}})
	started := make(chan struct{})
	var finished atomic.Bool
	a.rpcObs.conns = func(int32) ([]collector.Conn, error) {
		select {
		case started <- struct{}{}:
		default:
		}
		time.Sleep(300 * time.Millisecond)
		finished.Store(true)
		return nil, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = a.Run(ctx)
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the observer never polled")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return")
	}
	if !finished.Load() {
		t.Fatal("Run returned while a connection listing was still in flight")
	}
}

// TestLoadedModelsOmitMembers: a running rpc_server member serves no model, so
// it never appears in loaded_models (its row's model is empty).
func TestLoadedModelsOmitMembers(t *testing.T) {
	member := runtimectl.Status{
		SpecID: "rpcm_a", State: runtimectl.StateRunning, Kind: runtimectl.SpecKindRPCServer, PID: 1,
		RPCMember: &runtimectl.RPCMember{HeadSpecID: "rspec_h", Transport: "direct", BindIP: "127.0.0.1", Port: 1},
		RPC:       &runtimectl.RPCStatus{},
	}
	model := runtimectl.Status{SpecID: "rspec_m", Model: "m", State: runtimectl.StateRunning}
	a, poster, _ := rpcTelemetryAgent(t, member, model)
	a.collectOnce(context.Background())
	if got := poster.last().LoadedModels; len(got) != 1 || got[0] != "m" {
		t.Fatalf("loaded_models = %q, want only the model", got)
	}
}
