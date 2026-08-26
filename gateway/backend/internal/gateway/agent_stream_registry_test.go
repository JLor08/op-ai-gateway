// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

// fakeAgentSocket is a test double for agentSocket that lets runWriter's write
// and ping failure paths be exercised deterministically, without a real
// network connection. All accessors take fake.mu so reading them from the test
// goroutine after runWriter has (verifiably, via a channel close) returned is
// race-free.
type fakeAgentSocket struct {
	mu       sync.Mutex
	writeErr error
	pingErr  error
	writes   [][]byte
	pings    int
	closes   int
}

func (f *fakeAgentSocket) Write(_ context.Context, _ websocket.MessageType, p []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.writeErr != nil {
		return f.writeErr
	}
	f.writes = append(f.writes, append([]byte(nil), p...))
	return nil
}

func (f *fakeAgentSocket) Ping(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pings++
	return f.pingErr
}

func (f *fakeAgentSocket) CloseNow() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closes++
	return nil
}

func (f *fakeAgentSocket) writeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.writes)
}

func (f *fakeAgentSocket) writesSnapshot() [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([][]byte, len(f.writes))
	copy(out, f.writes)
	return out
}

func (f *fakeAgentSocket) pingCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pings
}

func (f *fakeAgentSocket) closeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closes
}

// TestCertUpdateFramePayloadIsExactlyFingerprint is the executable form of the
// plan's hardest constraint: the gateway must never be able to deliver
// anything beyond a bare doorbell to an agent. A mutation adding ANY second
// field to certUpdateFrame (a path, a command, anything) must fail this test.
func TestCertUpdateFramePayloadIsExactlyFingerprint(t *testing.T) {
	b, err := marshalStreamFrame("cert_update", certUpdateFrame{Fingerprint: "abc123"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var f streamFrame
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	if f.Type != "cert_update" {
		t.Fatalf("type = %q, want cert_update", f.Type)
	}
	var data map[string]json.RawMessage
	if err := json.Unmarshal(f.Data, &data); err != nil {
		t.Fatalf("unmarshal data: %v", err)
	}
	if len(data) != 1 {
		t.Fatalf("data has %d keys, want exactly 1 -- the gateway must never be able to deliver "+
			"anything beyond the fingerprint to an agent: %v", len(data), data)
	}
	raw, ok := data["fingerprint"]
	if !ok {
		t.Fatalf("data has no %q key: %v", "fingerprint", data)
	}
	var got string
	if err := json.Unmarshal(raw, &got); err != nil || got != "abc123" {
		t.Fatalf("fingerprint = %s (err %v), want %q", raw, err, "abc123")
	}
}

func TestCAUpdateFramePayloadIsExactlyFingerprint(t *testing.T) {
	b, err := marshalStreamFrame("ca_update", caUpdateFrame{Fingerprint: "ca123"})
	if err != nil {
		t.Fatal(err)
	}
	var f streamFrame
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatal(err)
	}
	if f.Type != "ca_update" {
		t.Fatalf("type=%q", f.Type)
	}
	var data map[string]json.RawMessage
	if err := json.Unmarshal(f.Data, &data); err != nil {
		t.Fatal(err)
	}
	if len(data) != 1 {
		t.Fatalf("keys=%v, want exactly fingerprint", data)
	}
	if _, ok := data["fingerprint"]; !ok {
		t.Fatalf("data=%v", data)
	}
}

func TestNotifyCAUpdateBroadcastsNonBlockingToEveryServer(t *testing.T) {
	r := NewAgentStreamRegistry()
	full := &agentStreamConn{out: make(chan []byte, 1)}
	full.out <- []byte("occupied")
	a := &agentStreamConn{out: make(chan []byte, 1)}
	b := &agentStreamConn{out: make(chan []byte, 1)}
	r.add("server-a", full)
	r.add("server-a", a)
	r.add("server-b", b)
	done := make(chan struct{})
	go func() { r.NotifyCAUpdate("ca-fp"); close(done) }()
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("NotifyCAUpdate blocked")
	}
	for name, c := range map[string]*agentStreamConn{"a": a, "b": b} {
		select {
		case raw := <-c.out:
			var f streamFrame
			if err := json.Unmarshal(raw, &f); err != nil || f.Type != "ca_update" {
				t.Fatalf("%s frame=%s err=%v", name, raw, err)
			}
		default:
			t.Fatalf("%s did not receive ca_update", name)
		}
	}
}

// TestAgentStreamConnEnqueueDropsOnFullQueueWithoutBlocking measures drop-on-full
// directly at enqueue, on a connection with NO writer goroutine draining it (a
// live end-to-end test with 8 small frames would be vacuously green -- they
// would never actually fill a real socket buffer).
func TestAgentStreamConnEnqueueDropsOnFullQueueWithoutBlocking(t *testing.T) {
	c := &agentStreamConn{out: make(chan []byte, agentStreamQueueCapacity)}
	for i := 0; i < agentStreamQueueCapacity; i++ {
		if !c.enqueue([]byte("x")) {
			t.Fatalf("enqueue %d reported the queue full early (capacity %d)", i, agentStreamQueueCapacity)
		}
	}
	if c.enqueue([]byte("overflow")) {
		t.Fatalf("the %d-th enqueue on a capacity-%d queue with no reader must report false",
			agentStreamQueueCapacity+1, agentStreamQueueCapacity)
	}
}

// TestNotifyCertUpdateNeverBlocksOnAFullQueue proves NotifyCertUpdate returns
// promptly even when the sole registered connection's queue is already full --
// the property that lets it be called synchronously from inside
// portal.Service.issueAndStore while certMu is held for the whole reconcile
// pass.
func TestNotifyCertUpdateNeverBlocksOnAFullQueue(t *testing.T) {
	r := NewAgentStreamRegistry()
	c := &agentStreamConn{out: make(chan []byte, agentStreamQueueCapacity)}
	r.add("srv-full", c)
	for i := 0; i < agentStreamQueueCapacity; i++ {
		c.enqueue([]byte("x"))
	}
	done := make(chan struct{})
	go func() {
		r.NotifyCertUpdate("srv-full", "fp")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("NotifyCertUpdate blocked on a full queue -- it is called from inside " +
			"issueAndStore while portal.Service holds certMu for the whole reconcile pass")
	}
}

// TestAgentStreamRegistryBothConnectionsGetDoorbellThenRemoveCleansUp covers
// add/remove/NotifyCertUpdate together: two connections for one server id both
// get the doorbell, removing one stops it from getting further ones while the
// other keeps receiving them, and removing the LAST connection deletes the
// outer map entry (not just the inner set) -- otherwise the outer map grows
// monotonically for every server id ever seen, live or not.
func TestAgentStreamRegistryBothConnectionsGetDoorbellThenRemoveCleansUp(t *testing.T) {
	r := NewAgentStreamRegistry()
	a := &agentStreamConn{out: make(chan []byte, agentStreamQueueCapacity)}
	b := &agentStreamConn{out: make(chan []byte, agentStreamQueueCapacity)}
	r.add("srv-multi", a)
	r.add("srv-multi", b)

	r.NotifyCertUpdate("srv-multi", "fp-1")
	select {
	case <-a.out:
	default:
		t.Fatal("connection a did not receive the doorbell")
	}
	select {
	case <-b.out:
	default:
		t.Fatal("connection b did not receive the doorbell")
	}

	r.remove("srv-multi", a)
	r.NotifyCertUpdate("srv-multi", "fp-2")
	select {
	case <-a.out:
		t.Fatal("a REMOVED connection must not receive a later doorbell")
	default:
	}
	select {
	case <-b.out:
	default:
		t.Fatal("the remaining connection must still receive the doorbell")
	}

	r.remove("srv-multi", b)
	r.mu.RLock()
	_, stillPresent := r.conns["srv-multi"]
	r.mu.RUnlock()
	if stillPresent {
		t.Fatal("removing the LAST connection for a server must delete the outer map entry too, " +
			"or it grows monotonically for every server id ever seen")
	}
}

// TestAgentStreamRegistryNotifyCertUpdateUnknownServerAndNilRegistryAreNoOps
// covers "agent offline / unknown server -> no-op, no panic" plus the nil
// receiver, which NotifyCertUpdate must also tolerate (mirrors every other
// registry in this package, e.g. AgentPresenceRegistry).
func TestAgentStreamRegistryNotifyCertUpdateUnknownServerAndNilRegistryAreNoOps(t *testing.T) {
	r := NewAgentStreamRegistry()
	r.NotifyCertUpdate("never-registered", "fp") // must not panic

	var nilRegistry *AgentStreamRegistry
	nilRegistry.NotifyCertUpdate("anything", "fp") // must not panic
}

// TestAgentStreamConnRunWriterReapsOnWriteFailure: a data-write failure must
// reap the connection (CloseNow) and return -- never a bare `return`, which
// would strand the goroutine on <-done forever and leak the registry entry
// (nothing else would ever call remove for a connection whose read loop never
// independently errors).
func TestAgentStreamConnRunWriterReapsOnWriteFailure(t *testing.T) {
	fake := &fakeAgentSocket{writeErr: errors.New("boom")}
	c := &agentStreamConn{
		conn:         fake,
		out:          make(chan []byte, agentStreamQueueCapacity),
		pingInterval: time.Hour, // never fires; isolates the write-failure path
		pingTimeout:  time.Second,
		writeTimeout: time.Second,
	}
	done := make(chan struct{})
	returned := make(chan struct{})
	go func() {
		c.runWriter(done)
		close(returned)
	}()

	if !c.enqueue([]byte(`{"type":"cert_update"}`)) {
		t.Fatal("enqueue on a fresh connection must succeed")
	}
	select {
	case <-returned:
	case <-time.After(2 * time.Second):
		t.Fatal("runWriter did not return after a write failure -- the handler would hang forever, " +
			"leaking the writer goroutine and the registry entry")
	}
	if got := fake.closeCount(); got != 1 {
		t.Fatalf("CloseNow called %d times, want exactly 1 (the reap)", got)
	}
	select {
	case <-done:
		t.Fatal("the test never closed done -- runWriter must have returned via the write-failure " +
			"branch, and this assertion is only meaningful if that is true")
	default:
	}
}

// TestAgentStreamConnRunWriterReapsOnPingFailure: the SAME reap contract as a
// write failure applies to a failed keepalive ping (a dead/half-open peer).
func TestAgentStreamConnRunWriterReapsOnPingFailure(t *testing.T) {
	fake := &fakeAgentSocket{pingErr: errors.New("no pong")}
	c := &agentStreamConn{
		conn:         fake,
		out:          make(chan []byte, agentStreamQueueCapacity),
		pingInterval: 2 * time.Millisecond,
		pingTimeout:  200 * time.Millisecond,
		writeTimeout: 200 * time.Millisecond,
	}
	done := make(chan struct{})
	returned := make(chan struct{})
	go func() {
		c.runWriter(done)
		close(returned)
	}()

	select {
	case <-returned:
	case <-time.After(2 * time.Second):
		t.Fatal("runWriter did not reap a connection whose peer stopped ponging")
	}
	if got := fake.closeCount(); got != 1 {
		t.Fatalf("CloseNow called %d times, want exactly 1", got)
	}
}

// TestAgentStreamConnRunWriterSurvivesSilentConnectionAcrossMultiplePings: a
// connection with NO data traffic at all must survive many ping cycles
// unharmed as long as each ping succeeds, and must be reaped ONLY via the done
// channel (a clean shutdown), never via CloseNow.
func TestAgentStreamConnRunWriterSurvivesSilentConnectionAcrossMultiplePings(t *testing.T) {
	fake := &fakeAgentSocket{}
	c := &agentStreamConn{
		conn:         fake,
		out:          make(chan []byte, agentStreamQueueCapacity),
		pingInterval: 2 * time.Millisecond,
		pingTimeout:  200 * time.Millisecond,
		writeTimeout: 200 * time.Millisecond,
	}
	done := make(chan struct{})
	returned := make(chan struct{})
	go func() {
		c.runWriter(done)
		close(returned)
	}()

	time.Sleep(40 * time.Millisecond) // several ping cycles at a 2ms interval
	close(done)
	select {
	case <-returned:
	case <-time.After(2 * time.Second):
		t.Fatal("runWriter did not return after done was closed")
	}
	if got := fake.pingCount(); got < 3 {
		t.Fatalf("ping count = %d, want several -- a data-silent connection must survive multiple "+
			"ping cycles unharmed", got)
	}
	if got := fake.closeCount(); got != 0 {
		t.Fatalf("CloseNow called %d times, want 0 -- a healthy silent connection must not be reaped", got)
	}
}

// TestAgentStreamConnRunWriterPreservesOrderFromOneProducer: the ordering
// guarantee this design promises is narrow and stated honestly in the plan --
// frames from a SINGLE producer arrive in queue order (not "every frame
// arrives", which the drop-on-full semantics already rule out).
func TestAgentStreamConnRunWriterPreservesOrderFromOneProducer(t *testing.T) {
	fake := &fakeAgentSocket{}
	c := &agentStreamConn{
		conn:         fake,
		out:          make(chan []byte, agentStreamQueueCapacity),
		pingInterval: time.Hour,
		pingTimeout:  time.Second,
		writeTimeout: time.Second,
	}
	done := make(chan struct{})
	defer close(done)
	go c.runWriter(done)

	for i := 0; i < agentStreamQueueCapacity; i++ {
		if !c.enqueue([]byte(fmt.Sprintf("%d", i))) {
			t.Fatalf("enqueue %d unexpectedly dropped", i)
		}
	}
	deadline := time.Now().Add(2 * time.Second)
	for fake.writeCount() < agentStreamQueueCapacity && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	got := fake.writesSnapshot()
	if len(got) != agentStreamQueueCapacity {
		t.Fatalf("wrote %d of %d frames within the deadline", len(got), agentStreamQueueCapacity)
	}
	for i, w := range got {
		if string(w) != fmt.Sprintf("%d", i) {
			t.Fatalf("frame %d = %q, want %q -- frames from one producer must arrive in queue order", i, w, i)
		}
	}
}

// TestAgentStreamRegistryConcurrentPingEnqueueAndReadIsRaceFree is the plan's
// required `-race` proof: a REAL WebSocket connection, with (a) a
// millisecond-scale ping ticker running throughout (via ms-scale fields, NOT
// the 30s/10s/5s production constants -- see the type doc's honesty note on
// why that requires a direct struct literal here rather than
// newAgentStreamConn), (b) 50 NotifyCertUpdate calls arriving concurrently
// from 5 goroutines, and (c) the "agent" (the dialed client) concurrently
// writing telemetry-shaped frames the server reads on the SAME underlying
// *websocket.Conn the writer goroutine is pinging/writing on --
// coder/websocket documents this single-reader/single-writer-concurrently
// shape as safe; this test is for OUR code (the registry's mutex and the
// enqueue channel), run under `go test -race`.
func TestAgentStreamRegistryConcurrentPingEnqueueAndReadIsRaceFree(t *testing.T) {
	const serverID = "race-host"
	registry := NewAgentStreamRegistry()
	connRegistered := make(chan struct{})
	readLoopDone := make(chan struct{})
	writerDone := make(chan struct{})

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		defer conn.CloseNow()
		sc := &agentStreamConn{
			conn:         conn,
			out:          make(chan []byte, agentStreamQueueCapacity),
			pingInterval: 2 * time.Millisecond,
			pingTimeout:  200 * time.Millisecond,
			writeTimeout: 200 * time.Millisecond,
		}
		registry.add(serverID, sc)
		defer registry.remove(serverID, sc)
		close(connRegistered)
		go func() {
			sc.runWriter(readLoopDone)
			close(writerDone)
		}()
		// (c) a minimal read loop -- the agent's telemetry frames -- running
		// concurrently with the writer goroutine above on the SAME conn.
		for {
			if _, _, err := conn.Read(context.Background()); err != nil {
				close(readLoopDone)
				return
			}
		}
	})
	ts := httptest.NewServer(handler)
	defer ts.Close()

	ctx := context.Background()
	client, _, err := websocket.Dial(ctx, wsURL(ts.URL, "/"), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.CloseNow()

	select {
	case <-connRegistered:
	case <-time.After(2 * time.Second):
		t.Fatal("the server-side connection was never registered")
	}

	var wg sync.WaitGroup
	// (b) 50 NotifyCertUpdate calls from 5 goroutines.
	for range 5 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 10 {
				registry.NotifyCertUpdate(serverID, "fp")
			}
		}()
	}
	// (c) the agent side writes telemetry-shaped frames concurrently.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for range 20 {
			_ = wsjson.Write(ctx, client, streamFrame{Type: "telemetry", Data: json.RawMessage(`{"host":{"cpu_util_pct":1}}`)})
		}
	}()
	wg.Wait()

	// Let the (a) ping ticker (2ms) fire a few more times, then close cleanly.
	time.Sleep(20 * time.Millisecond)
	_ = client.Close(websocket.StatusNormalClosure, "")

	select {
	case <-writerDone:
	case <-time.After(2 * time.Second):
		t.Fatal("the writer goroutine never returned after the client closed")
	}
}

// TestNotifyRuntimeConfigDeliversFullPayload pins the wire contract: the
// frame is {"type":"runtime_config","data":<the exact document>} -- the
// WHOLE document, verbatim, never a reduced command/id like the cert_update
// doorbell (contrast TestCertUpdateFramePayloadIsExactlyFingerprint above,
// which pins the OPPOSITE property for that frame -- this one is deliberately
// different, per the design's rejection of a command frame).
func TestNotifyRuntimeConfigDeliversFullPayload(t *testing.T) {
	r := NewAgentStreamRegistry()
	c := &agentStreamConn{out: make(chan []byte, agentStreamQueueCapacity)}
	r.add("srv-runtime", c)

	payload := json.RawMessage(`{"router_listen":8081,"max_processes":3,"gpu_budgets":[{"index":0,"budget_mb":46000}],"specs":[{"id":"rspec_a","model":"qwen-coder"}],"coresident":[["rspec_a","rspec_b"]],"etag":"cafef00d"}`)
	r.NotifyRuntimeConfig("srv-runtime", payload)

	select {
	case raw := <-c.out:
		var f streamFrame
		if err := json.Unmarshal(raw, &f); err != nil {
			t.Fatalf("unmarshal envelope: %v", err)
		}
		if f.Type != "runtime_config" {
			t.Fatalf("type = %q, want runtime_config", f.Type)
		}
		var got, want map[string]json.RawMessage
		if err := json.Unmarshal(f.Data, &got); err != nil {
			t.Fatalf("unmarshal data: %v", err)
		}
		if err := json.Unmarshal(payload, &want); err != nil {
			t.Fatal(err)
		}
		if len(got) != len(want) {
			t.Fatalf("data has %d top-level keys, want %d -- the frame must carry the FULL document, "+
				"never a reduced one: got=%s", len(got), len(want), f.Data)
		}
		for k, wv := range want {
			gv, ok := got[k]
			if !ok || string(gv) != string(wv) {
				t.Fatalf("data[%q] = %s, want %s", k, gv, wv)
			}
		}
	default:
		t.Fatal("connection did not receive the runtime_config frame")
	}
}

// TestNotifyRuntimeConfigDropsOnFullQueueWithoutBlocking mirrors
// TestNotifyCertUpdateNeverBlocksOnAFullQueue: the push must never stall its
// caller (PushRuntimeConfig's goroutine) regardless of how full or stuck a
// given connection's queue is.
func TestNotifyRuntimeConfigDropsOnFullQueueWithoutBlocking(t *testing.T) {
	r := NewAgentStreamRegistry()
	c := &agentStreamConn{out: make(chan []byte, agentStreamQueueCapacity)}
	r.add("srv-full-rc", c)
	for i := 0; i < agentStreamQueueCapacity; i++ {
		c.enqueue([]byte("x"))
	}
	done := make(chan struct{})
	go func() {
		r.NotifyRuntimeConfig("srv-full-rc", json.RawMessage(`{"etag":"x"}`))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("NotifyRuntimeConfig blocked on a full queue -- it must be safe to call from a hook " +
			"that itself must never be slowed down by a stuck agent peer")
	}
}

// TestAgentStreamRegistryNotifyRuntimeConfigUnknownServerAndNilRegistryAreNoOps
// mirrors the cert_update nil-registry/unknown-server contract.
func TestAgentStreamRegistryNotifyRuntimeConfigUnknownServerAndNilRegistryAreNoOps(t *testing.T) {
	r := NewAgentStreamRegistry()
	r.NotifyRuntimeConfig("never-registered", json.RawMessage(`{"etag":"x"}`)) // must not panic

	var nilRegistry *AgentStreamRegistry
	nilRegistry.NotifyRuntimeConfig("anything", json.RawMessage(`{"etag":"x"}`)) // must not panic
}

// TestNotifyRuntimeConfigEmptyPayloadIsNoOp: an empty/nil payload must never
// reach the wire as a bodyless or malformed frame -- there is nothing
// meaningful to push.
func TestNotifyRuntimeConfigEmptyPayloadIsNoOp(t *testing.T) {
	r := NewAgentStreamRegistry()
	c := &agentStreamConn{out: make(chan []byte, agentStreamQueueCapacity)}
	r.add("srv-empty-rc", c)
	r.NotifyRuntimeConfig("srv-empty-rc", nil)
	select {
	case raw := <-c.out:
		t.Fatalf("an empty payload must not be enqueued, got %s", raw)
	default:
	}
}

// TestNotifyRuntimeConfigBroadcastsToEveryConnectionForTheServer mirrors
// TestAgentStreamRegistryBothConnectionsGetDoorbellThenRemoveCleansUp: a
// reconnect overlap can leave more than one open connection for the same
// server id, and ALL of them must get the push, not just one.
func TestNotifyRuntimeConfigBroadcastsToEveryConnectionForTheServer(t *testing.T) {
	r := NewAgentStreamRegistry()
	a := &agentStreamConn{out: make(chan []byte, agentStreamQueueCapacity)}
	b := &agentStreamConn{out: make(chan []byte, agentStreamQueueCapacity)}
	other := &agentStreamConn{out: make(chan []byte, agentStreamQueueCapacity)}
	r.add("srv-multi-rc", a)
	r.add("srv-multi-rc", b)
	r.add("srv-other-rc", other)

	r.NotifyRuntimeConfig("srv-multi-rc", json.RawMessage(`{"etag":"x"}`))
	for name, c := range map[string]*agentStreamConn{"a": a, "b": b} {
		select {
		case <-c.out:
		default:
			t.Fatalf("connection %s did not receive the runtime_config push", name)
		}
	}
	select {
	case raw := <-other.out:
		t.Fatalf("a DIFFERENT server's connection must not receive the push, got %s", raw)
	default:
	}
}
