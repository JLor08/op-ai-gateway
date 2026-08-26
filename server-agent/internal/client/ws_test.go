// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"op-ai-server-agent/internal/sample"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

func TestWSURLFromGateway(t *testing.T) {
	cases := map[string]string{
		"https://gw.example":       "wss://gw.example/api/agent/v1/stream",
		"http://gw.example:8080":   "ws://gw.example:8080/api/agent/v1/stream",
		"https://gw.example/base/": "wss://gw.example/base/api/agent/v1/stream",
		"https://gw":               "wss://gw/api/agent/v1/stream",
	}
	for in, want := range cases {
		got, err := wsURLFromGateway(in)
		if err != nil || got != want {
			t.Fatalf("wsURLFromGateway(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := wsURLFromGateway("ftp://x"); err == nil {
		t.Fatal("expected error for non-http scheme")
	}
}

func TestNewWSSenderUsesInjectedHTTPClient(t *testing.T) {
	injected := &http.Client{Timeout: 17 * time.Second}
	sender, err := NewWSSender("https://gw.example", "tok", injected)
	if err != nil {
		t.Fatalf("NewWSSender: %v", err)
	}
	if sender.httpClient != injected {
		t.Fatal("NewWSSender did not retain the injected HTTP client")
	}
}

func TestNewWSSenderNilHTTPClientHasNoOverallTimeout(t *testing.T) {
	sender, err := NewWSSender("https://gw.example", "tok", nil)
	if err != nil {
		t.Fatalf("NewWSSender: %v", err)
	}
	if got := sender.httpClient.Timeout; got != 0 {
		t.Fatalf("default WebSocket HTTP timeout = %v, want 0", got)
	}
}

func TestBackoffDelayGrowsAndCaps(t *testing.T) {
	base := 500 * time.Millisecond
	backoffCap := 30 * time.Second
	s := &WSSender{backoffBase: base, backoffCap: backoffCap, rng: rand.New(rand.NewSource(1))}

	// f=1: base + jitter[0,base/2].
	if d := s.backoffDelay(1); d < base || d > base+base/2 {
		t.Fatalf("delay(1) = %v, want within [%v,%v]", d, base, base+base/2)
	}
	// f=3: two doublings -> 4*base + jitter[0,2*base].
	if d := s.backoffDelay(3); d < 4*base || d > 6*base {
		t.Fatalf("delay(3) = %v, want within [%v,%v]", d, 4*base, 6*base)
	}

	// f=20: unclamped this would be base*2^19 (~72h). The
	// `if d > s.backoffCap { d = s.backoffCap }` clamp must bring it down to
	// exactly [backoffCap, backoffCap+backoffCap/2]; removing the clamp would fail
	// this assertion by many orders of magnitude.
	if d := s.backoffDelay(20); d < backoffCap || d > backoffCap+backoffCap/2 {
		t.Fatalf("delay(20) = %v, want within [%v,%v] (the cap must clamp the exponential growth)", d, backoffCap, backoffCap+backoffCap/2)
	}

	// Jitter must vary with the RNG: two different seeds must (overwhelmingly)
	// produce different delays, and neither may land exactly on backoffBase. If
	// the `+ time.Duration(rng.Int63n(...))` jitter term were removed,
	// backoffDelay(1) would always equal backoffBase regardless of RNG state, and
	// d1 would equal d2.
	s1 := &WSSender{backoffBase: base, backoffCap: backoffCap, rng: rand.New(rand.NewSource(11))}
	s2 := &WSSender{backoffBase: base, backoffCap: backoffCap, rng: rand.New(rand.NewSource(22))}
	d1 := s1.backoffDelay(1)
	d2 := s2.backoffDelay(1)
	if d1 == base || d2 == base {
		t.Fatalf("backoffDelay(1) == backoffBase exactly (d1=%v d2=%v base=%v); jitter term appears missing", d1, d2, base)
	}
	if d1 == d2 {
		t.Fatalf("backoffDelay(1) produced the same value %v for two different RNG seeds; jitter term appears missing", d1)
	}
}

func TestIsCleanClose(t *testing.T) {
	if isCleanClose(errors.New("boom")) {
		t.Fatal("plain error must not be clean")
	}
}

// fakeClock is an injectable, mutex-guarded clock for deterministic backoff/reconnect
// tests: the caller advances it explicitly instead of waiting out real delays.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock(t time.Time) *fakeClock { return &fakeClock{t: t} }

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) setTo(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = t
}

// fakeGateway accepts a WebSocket, records ingested telemetry frames, and can be told
// to close the first connection with a given status to exercise reconnect.
type fakeGateway struct {
	mu       sync.Mutex
	frames   []string
	closeNow websocket.StatusCode // 0 = don't force close
}

func (f *fakeGateway) got() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.frames...)
}

func (f *fakeGateway) handler(w http.ResponseWriter, r *http.Request) {
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return
	}
	defer c.CloseNow()
	for {
		var raw map[string]any
		if err := wsjson.Read(r.Context(), c, &raw); err != nil {
			return
		}
		f.mu.Lock()
		f.frames = append(f.frames, fmt.Sprint(raw["type"]))
		cn := f.closeNow
		f.closeNow = 0
		f.mu.Unlock()
		if cn != 0 {
			_ = c.Close(cn, "bye")
			return
		}
	}
}

// flappingGateway accepts a WebSocket, reads exactly one frame, then aborts the
// underlying connection with CloseNow (no WebSocket close frame) — an ABNORMAL,
// non-clean disconnect from the client's point of view, simulating a flaky link
// rather than a graceful server-initiated close.
type flappingGateway struct{}

func (flappingGateway) handler(w http.ResponseWriter, r *http.Request) {
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return
	}
	var raw map[string]any
	_ = wsjson.Read(r.Context(), c, &raw)
	c.CloseNow()
}

// dialCounter is an http.Handler that counts how many times it was hit (i.e. how many
// dial attempts reached the network), then refuses the WebSocket upgrade so the dial
// fails fast without holding a connection open. Used to prove a dial was (or was not)
// attempted without depending on connection-refused timing.
type dialCounter struct {
	mu    sync.Mutex
	count int
}

func (d *dialCounter) handler(w http.ResponseWriter, r *http.Request) {
	d.mu.Lock()
	d.count++
	d.mu.Unlock()
	http.Error(w, "no upgrade", http.StatusBadRequest)
}

func (d *dialCounter) get() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.count
}

func wsTestSample() *sample.Sample {
	return &sample.Sample{Host: &sample.Host{CPUUtilPct: 5}}
}

func newTestWSSender(t *testing.T, url string) *WSSender {
	t.Helper()
	return newTestWSSenderWithClient(t, url, nil)
}

func newTestWSSenderWithClient(t *testing.T, url string, httpClient *http.Client) *WSSender {
	t.Helper()
	s, err := NewWSSender(url, "tok", httpClient)
	if err != nil {
		t.Fatalf("NewWSSender: %v", err)
	}
	// Fast, deterministic reconnect for tests.
	s.backoffBase = time.Millisecond
	s.backoffCap = 5 * time.Millisecond
	s.cleanMin = time.Millisecond
	s.cleanMax = time.Millisecond
	s.dialTimeout = 2 * time.Second
	s.writeTimeout = 2 * time.Second
	s.rng = rand.New(rand.NewSource(1))
	return s
}

func TestWSSenderWritesFrame(t *testing.T) {
	fg := &fakeGateway{}
	ts := httptest.NewServer(http.HandlerFunc(fg.handler))
	defer ts.Close()
	s := newTestWSSender(t, wsHTTPToWS(ts.URL))
	defer s.Close()
	if err := s.Post(context.Background(), wsTestSample()); err != nil {
		t.Fatalf("post: %v", err)
	}
	waitFor(t, func() bool { return len(fg.got()) == 1 && fg.got()[0] == "telemetry" })
}

func TestWSSenderReconnectsAfterCleanClose(t *testing.T) {
	fg := &fakeGateway{closeNow: websocket.StatusGoingAway}
	ts := httptest.NewServer(http.HandlerFunc(fg.handler))
	defer ts.Close()
	s := newTestWSSender(t, wsHTTPToWS(ts.URL))
	defer s.Close()

	// First Post connects + writes; the fake gateway then closes GoingAway.
	_ = s.Post(context.Background(), wsTestSample())
	waitFor(t, func() bool { return len(fg.got()) >= 1 })
	// Let the read loop observe the close + schedule the (tiny) clean reconnect.
	time.Sleep(20 * time.Millisecond)
	// Subsequent Posts must redial and deliver again.
	for i := 0; i < 5; i++ {
		_ = s.Post(context.Background(), wsTestSample())
		time.Sleep(10 * time.Millisecond)
	}
	waitFor(t, func() bool { return len(fg.got()) >= 2 })
}

func TestWSSenderUsesInjectedHTTPClientForTLSAndReconnect(t *testing.T) {
	fg := &fakeGateway{closeNow: websocket.StatusGoingAway}
	srv := httptest.NewTLSServer(http.HandlerFunc(fg.handler))
	defer srv.Close()
	s := newTestWSSenderWithClient(t, srv.URL, srv.Client())
	defer s.Close()

	if err := s.Post(context.Background(), wsTestSample()); err != nil {
		t.Fatalf("first TLS post: %v", err)
	}
	waitFor(t, func() bool { return len(fg.got()) >= 1 })
	time.Sleep(20 * time.Millisecond)
	for i := 0; i < 5; i++ {
		_ = s.Post(context.Background(), wsTestSample())
		time.Sleep(10 * time.Millisecond)
	}
	waitFor(t, func() bool { return len(fg.got()) >= 2 })
}

// TestBackoffEscalatesOnRepeatedUnstableDrops is the regression test for the
// reset-on-stable backoff bug (D7 deviation): maybeDial's success path used to zero
// s.failures on EVERY successful handshake, so dropConn's non-clean/non-stable branch
// always incremented from 0 -> 1, pinning the reconnect delay at ~backoffBase forever
// instead of growing toward backoffCap across repeated connect/abnormal-drop flaps.
//
// It simulates N cycles of "successful connect -> abnormal (non-clean, non-stable)
// drop" using a flappingGateway (aborts the TCP connection after one frame, so the
// client observes a plain I/O error, not a WebSocket close code) and an injected fake
// clock held fixed across each connect+drop pair (so connectedAt == drop-time, and the
// "stable" branch in dropConn can never trigger). It asserts the scheduled reconnect
// delay lands in the range implied by an ESCALATING failure count (f=1,2,3,4), which is
// disjoint from the range implied by a pinned f=1 on every cycle -- so this test fails
// on the pre-fix code and passes once maybeDial no longer resets failures to 0.
func TestBackoffEscalatesOnRepeatedUnstableDrops(t *testing.T) {
	fg := flappingGateway{}
	ts := httptest.NewServer(http.HandlerFunc(fg.handler))
	defer ts.Close()

	s := newTestWSSender(t, wsHTTPToWS(ts.URL))
	defer s.Close()

	base := 50 * time.Millisecond
	s.backoffBase = base
	s.backoffCap = 10 * time.Second
	// Large relative to the (frozen-clock) cycle duration below, so no drop in this
	// test is ever misclassified as "stable".
	s.stableThreshold = 10 * time.Second

	clk := newFakeClock(time.Unix(1_000_000, 0))
	s.clock = clk.now

	const cycles = 4
	delays := make([]time.Duration, 0, cycles)
	for i := 0; i < cycles; i++ {
		if err := s.Post(context.Background(), wsTestSample()); err != nil {
			t.Fatalf("cycle %d: post failed: %v", i, err)
		}
		// The write succeeds before the gateway aborts the connection; wait for the
		// background read loop to observe the abrupt close and drop the connection.
		waitFor(t, func() bool { return s.currentConn() == nil })

		s.mu.Lock()
		delays = append(delays, s.nextDialAt.Sub(clk.now()))
		nextDialAt := s.nextDialAt
		s.mu.Unlock()

		// Jump the fake clock just past the scheduled dial so the next cycle's Post
		// redials immediately, without waiting out the real backoff, and without the
		// elapsed (frozen) connect-to-drop duration ever crossing stableThreshold.
		clk.setTo(nextDialAt.Add(time.Millisecond))
	}

	// Escalating ranges from backoffDelay's base*2^(f-1) + jitter[0,d/2] formula:
	//   f=1: [base,   1.5*base]
	//   f=2: [2*base, 3*base]
	//   f=3: [4*base, 6*base]
	//   f=4: [8*base, 12*base]
	// These ranges are pairwise disjoint, so any cycle landing below its expected
	// floor proves the failure count did NOT escalate (the bug's symptom: every cycle
	// stays pinned in the f=1 range because maybeDial keeps zeroing it on connect).
	if delays[0] < base || delays[0] > base+base/2 {
		t.Fatalf("delay[0] = %v, want within the f=1 range [%v,%v]", delays[0], base, base+base/2)
	}
	if delays[1] < 2*base {
		t.Fatalf("delay[1] = %v, want >= %v (f=2 floor) -- bug symptom: pinned at ~base instead of escalating", delays[1], 2*base)
	}
	if delays[2] < 4*base {
		t.Fatalf("delay[2] = %v, want >= %v (f=3 floor) -- escalation must continue", delays[2], 4*base)
	}
	if delays[3] < 8*base {
		t.Fatalf("delay[3] = %v, want >= %v (f=4 floor) -- escalation must continue", delays[3], 8*base)
	}
}

// TestBackoffResetsAfterStableDrop pins dropConn's existing "stable" branch: a drop of
// a connection that has been up longer than stableThreshold resets failures to 1
// (base delay) regardless of how many unstable drops preceded it.
func TestBackoffResetsAfterStableDrop(t *testing.T) {
	fg := &fakeGateway{}
	ts := httptest.NewServer(http.HandlerFunc(fg.handler))
	defer ts.Close()

	s := newTestWSSender(t, wsHTTPToWS(ts.URL))
	defer s.Close()

	base := 50 * time.Millisecond
	s.backoffBase = base
	s.backoffCap = 10 * time.Second
	s.stableThreshold = 10 * time.Second

	clk := newFakeClock(time.Unix(2_000_000, 0))
	s.clock = clk.now

	if err := s.Post(context.Background(), wsTestSample()); err != nil {
		t.Fatalf("post: %v", err)
	}
	conn := s.currentConn()
	if conn == nil {
		t.Fatal("expected an active connection after Post")
	}

	// Simulate a long run of prior failures and a connection that has been up longer
	// than stableThreshold.
	s.mu.Lock()
	s.failures = 10
	s.connectedAt = clk.now().Add(-s.stableThreshold - time.Second)
	s.mu.Unlock()

	s.dropConn(conn, false) // abnormal (non-clean) drop of a STABLE connection

	s.mu.Lock()
	delay := s.nextDialAt.Sub(clk.now())
	failures := s.failures
	s.mu.Unlock()

	if failures != 1 {
		t.Fatalf("failures after a stable drop = %d, want 1 (reset)", failures)
	}
	if delay < base || delay > base+base/2 {
		t.Fatalf("delay after a stable drop = %v, want within the f=1 range [%v,%v] (must reset, not continue escalating from failures=10)", delay, base, base+base/2)
	}
}

// TestBackoffUsesCleanDelayRegardlessOfPriorFailures pins the existing clean-close
// branch: a graceful server close (GoingAway) always uses the short clean-reconnect
// delay and resets failures to 0, even after several prior unstable-drop failures.
func TestBackoffUsesCleanDelayRegardlessOfPriorFailures(t *testing.T) {
	fg := &fakeGateway{closeNow: websocket.StatusGoingAway}
	ts := httptest.NewServer(http.HandlerFunc(fg.handler))
	defer ts.Close()

	s := newTestWSSender(t, wsHTTPToWS(ts.URL))
	defer s.Close()

	base := 50 * time.Millisecond
	s.backoffBase = base
	s.backoffCap = 10 * time.Second
	s.cleanMin = time.Millisecond
	s.cleanMax = 2 * time.Millisecond
	s.stableThreshold = 10 * time.Second

	clk := newFakeClock(time.Unix(3_000_000, 0))
	s.clock = clk.now

	// Pre-seed a high failure count as if several unstable drops had already
	// happened.
	s.mu.Lock()
	s.failures = 7
	s.mu.Unlock()

	if err := s.Post(context.Background(), wsTestSample()); err != nil {
		t.Fatalf("post: %v", err)
	}
	waitFor(t, func() bool { return s.currentConn() == nil })

	s.mu.Lock()
	delay := s.nextDialAt.Sub(clk.now())
	failures := s.failures
	cleanMin, cleanMax := s.cleanMin, s.cleanMax
	s.mu.Unlock()

	if failures != 0 {
		t.Fatalf("failures after a clean close = %d, want 0 (reset)", failures)
	}
	if delay < cleanMin || delay > cleanMax {
		t.Fatalf("delay after a clean close = %v, want within [%v,%v] (clean delay, not exponential backoff from failures=7)", delay, cleanMin, cleanMax)
	}
}

// TestPostSkipsDialInsideBackoffWindow proves, via an injected clock and an observable
// dial-attempt counter (rather than timing the wall-clock duration of a fast-failing
// dial), that Post attempts at most one dial per elapsed backoff window: no attempt
// while the fake clock is still inside [now, nextDialAt), and exactly one more once the
// clock is advanced past it.
func TestPostSkipsDialInsideBackoffWindow(t *testing.T) {
	dc := &dialCounter{}
	ts := httptest.NewServer(http.HandlerFunc(dc.handler))
	defer ts.Close()

	s := newTestWSSender(t, wsHTTPToWS(ts.URL))
	defer s.Close()

	clk := newFakeClock(time.Unix(4_000_000, 0))
	s.clock = clk.now
	s.backoffBase = 50 * time.Millisecond
	s.backoffCap = 10 * time.Second

	// First Post: the handler refuses the upgrade, so the dial fails and a backoff
	// window is scheduled.
	if err := s.Post(context.Background(), wsTestSample()); err == nil {
		t.Fatal("expected a dial failure against the upgrade-refusing handler")
	}
	if got := dc.get(); got != 1 {
		t.Fatalf("dial attempts after the first Post = %d, want 1", got)
	}

	s.mu.Lock()
	nextDialAt := s.nextDialAt
	s.mu.Unlock()
	if !clk.now().Before(nextDialAt) {
		t.Fatalf("expected the fake clock (%v) to be inside the backoff window (< %v)", clk.now(), nextDialAt)
	}

	// While still inside the window, repeated Posts must NOT attempt another dial.
	for i := 0; i < 5; i++ {
		if err := s.Post(context.Background(), wsTestSample()); err == nil {
			t.Fatal("expected Post to keep failing while backing off")
		}
	}
	if got := dc.get(); got != 1 {
		t.Fatalf("dial attempts while inside the backoff window = %d, want still 1 (no dial should be attempted)", got)
	}

	// Advance the clock past nextDialAt: the next Post must attempt exactly one more
	// dial.
	clk.setTo(nextDialAt.Add(time.Millisecond))
	if err := s.Post(context.Background(), wsTestSample()); err == nil {
		t.Fatal("expected the dial to fail again against the upgrade-refusing handler")
	}
	if got := dc.get(); got != 2 {
		t.Fatalf("dial attempts after the window elapsed = %d, want 2 (exactly one new attempt)", got)
	}
}

// silentGateway accepts the WebSocket upgrade (so the TCP/HTTP handshake completes and
// the client believes it is connected) but never calls Read again -- so it can never
// process, and therefore never answer, an incoming ping. This simulates a silent/
// half-open gateway: host hard-down, a network partition, or (most relevant for this
// agent's actual mesh transport) a NetBird/WireGuard tunnel drop, which is UDP and
// produces no FIN/RST. A write into such a connection still succeeds into the kernel
// send buffer, so only an ACTIVE liveness probe (not the write path) can detect it.
type silentGateway struct {
	mu    sync.Mutex
	conns int
}

func (g *silentGateway) handler(w http.ResponseWriter, r *http.Request) {
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return
	}
	defer c.CloseNow()
	g.mu.Lock()
	g.conns++
	g.mu.Unlock()
	// Block until the test tears the server down: deliberately no Read loop, so a
	// ping frame the client sends is never seen and never pong'd.
	<-r.Context().Done()
}

func (g *silentGateway) count() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.conns
}

// TestWSSenderDetectsSilentGatewayAndReconnects is the regression test for the missing
// liveness probe: a WSSender talking to a gateway that accepted the connection but never
// answers a ping must detect this (not hang forever) and reconnect. It fails on the
// pre-fix code (readLoop only reacts to a read error, which a silent peer never
// produces) and passes once a ping-probe goroutine reaps the connection on a missed
// pong.
func TestWSSenderDetectsSilentGatewayAndReconnects(t *testing.T) {
	sg := &silentGateway{}
	ts := httptest.NewServer(http.HandlerFunc(sg.handler))
	defer ts.Close()

	s := newTestWSSender(t, wsHTTPToWS(ts.URL))
	defer s.Close()
	s.pingInterval = 20 * time.Millisecond
	s.pingTimeout = 20 * time.Millisecond

	// The silent gateway still completes the WS upgrade, so the first Post succeeds --
	// the write lands in the kernel send buffer regardless of whether anyone reads it.
	// This is exactly the deceptive "looks connected" state a half-open/silent gateway
	// produces; the write path alone can never catch it.
	if err := s.Post(context.Background(), wsTestSample()); err != nil {
		t.Fatalf("post: %v", err)
	}
	waitFor(t, func() bool { return sg.count() >= 1 })
	if s.currentConn() == nil {
		t.Fatal("expected an active connection after the first post")
	}

	// The ping probe must notice the missing pong and drop the connection within
	// ~pingInterval+pingTimeout (here ~40ms; waitFor's 3s deadline gives ample margin).
	waitFor(t, func() bool { return s.currentConn() == nil })

	// A subsequent Post must redial -- proving reconnection, not merely detection.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		_ = s.Post(context.Background(), wsTestSample())
		if sg.count() >= 2 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("expected a reconnect dial after the ping-probe drop; only %d connection(s) accepted", sg.count())
}

func wsTestReport() *sample.SystemReport {
	return &sample.SystemReport{AgentVersion: "1.2.3", CPU: sample.CPUInfo{Model: "X"}}
}

func TestWSSenderWritesSystemReportFrame(t *testing.T) {
	fg := &fakeGateway{}
	ts := httptest.NewServer(http.HandlerFunc(fg.handler))
	defer ts.Close()
	s := newTestWSSender(t, wsHTTPToWS(ts.URL))
	defer s.Close()

	if err := s.PostSystemReport(context.Background(), wsTestReport()); err != nil {
		t.Fatalf("PostSystemReport: %v", err)
	}
	waitFor(t, func() bool {
		got := fg.got()
		return len(got) >= 1 && got[0] == "system_report"
	})
}

func TestWSSenderResendsSystemReportOnReconnect(t *testing.T) {
	// The gateway closes after the first frame; the cached report must be re-sent
	// on the redial that a subsequent telemetry Post triggers.
	fg := &fakeGateway{closeNow: websocket.StatusGoingAway}
	ts := httptest.NewServer(http.HandlerFunc(fg.handler))
	defer ts.Close()
	s := newTestWSSender(t, wsHTTPToWS(ts.URL))
	defer s.Close()

	// First: cache + send the system_report (frame 1), gateway then closes.
	_ = s.PostSystemReport(context.Background(), wsTestReport())
	waitFor(t, func() bool { return len(fg.got()) >= 1 })
	time.Sleep(20 * time.Millisecond) // let the read loop observe the close

	// Subsequent telemetry Posts redial; maybeDial resends the cached report first.
	for i := 0; i < 5; i++ {
		_ = s.Post(context.Background(), wsTestSample())
		time.Sleep(10 * time.Millisecond)
	}
	waitFor(t, func() bool {
		count := 0
		for _, f := range fg.got() {
			if f == "system_report" {
				count++
			}
		}
		return count >= 2
	})
}

// TestWSSenderWritesRuntimeReportFrame proves PostRuntimeReport writes a
// {"type":"runtime_report","data":<raw>} frame verbatim.
func TestWSSenderWritesRuntimeReportFrame(t *testing.T) {
	fg := &fakeGateway{}
	ts := httptest.NewServer(http.HandlerFunc(fg.handler))
	defer ts.Close()
	s := newTestWSSender(t, wsHTTPToWS(ts.URL))
	defer s.Close()

	raw := json.RawMessage(`{"source":"file","collected_at":"2026-08-26T09:00:00Z","config":{}}`)
	if err := s.PostRuntimeReport(context.Background(), raw); err != nil {
		t.Fatalf("PostRuntimeReport: %v", err)
	}
	waitFor(t, func() bool {
		got := fg.got()
		return len(got) >= 1 && got[0] == "runtime_report"
	})
}

// TestWSSenderResendsRuntimeReportOnReconnect proves the cached runtime
// report is re-sent on every future reconnect, exactly like the cached
// system report.
func TestWSSenderResendsRuntimeReportOnReconnect(t *testing.T) {
	fg := &fakeGateway{closeNow: websocket.StatusGoingAway}
	ts := httptest.NewServer(http.HandlerFunc(fg.handler))
	defer ts.Close()
	s := newTestWSSender(t, wsHTTPToWS(ts.URL))
	defer s.Close()

	raw := json.RawMessage(`{"source":"file","collected_at":"2026-08-26T09:00:00Z","config":{}}`)
	_ = s.PostRuntimeReport(context.Background(), raw)
	waitFor(t, func() bool { return len(fg.got()) >= 1 })
	time.Sleep(20 * time.Millisecond) // let the read loop observe the close

	for i := 0; i < 5; i++ {
		_ = s.Post(context.Background(), wsTestSample())
		time.Sleep(10 * time.Millisecond)
	}
	waitFor(t, func() bool {
		count := 0
		for _, f := range fg.got() {
			if f == "runtime_report" {
				count++
			}
		}
		return count >= 2
	})
}

func wsHTTPToWS(u string) string {
	return "ws" + u[len("http"):]
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not met before deadline")
}

// serverPusher is a minimal test gateway that accepts exactly one connection,
// signals readiness via up (closed once the connection is captured), and lets
// the test push arbitrary raw frames to the client on demand via push. It
// drains (never itself parses meaningfully) whatever the client subsequently
// writes.
type serverPusher struct {
	mu   sync.Mutex
	conn *websocket.Conn
}

func newServerPusher() (*serverPusher, chan struct{}) {
	return &serverPusher{}, make(chan struct{})
}

func (g *serverPusher) handler(up chan struct{}) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		defer c.CloseNow()
		g.mu.Lock()
		g.conn = c
		g.mu.Unlock()
		close(up)
		for {
			var raw map[string]any
			if err := wsjson.Read(r.Context(), c, &raw); err != nil {
				return
			}
		}
	}
}

func (g *serverPusher) push(t *testing.T, raw []byte) {
	t.Helper()
	g.mu.Lock()
	c := g.conn
	g.mu.Unlock()
	if c == nil {
		t.Fatal("serverPusher.push called before a connection was accepted")
	}
	if err := c.Write(context.Background(), websocket.MessageText, raw); err != nil {
		t.Fatalf("server push: %v", err)
	}
}

// TestWSSenderCertUpdateFrameWakesCertUpdates proves the gateway-pushed
// cert_update doorbell wakes CertUpdates() (readLoop decodes the {type,data}
// envelope and signals on that specific type).
func TestWSSenderCertUpdateFrameWakesCertUpdates(t *testing.T) {
	g, up := newServerPusher()
	ts := httptest.NewServer(g.handler(up))
	defer ts.Close()
	s := newTestWSSender(t, wsHTTPToWS(ts.URL))
	defer s.Close()

	if err := s.Post(context.Background(), wsTestSample()); err != nil {
		t.Fatalf("post: %v", err)
	}
	<-up
	// Drain the connect-triggered wake (every successful (re)connect also
	// wakes CertUpdates -- see maybeDial) so it cannot be mistaken for the
	// signal the pushed frame below is supposed to cause.
	select {
	case <-s.CertUpdates():
	case <-time.After(time.Second):
		t.Fatal("expected the initial connect wake")
	}

	g.push(t, []byte(`{"type":"cert_update","data":{"fingerprint":"abc123"}}`))
	select {
	case <-s.CertUpdates():
	case <-time.After(time.Second):
		t.Fatal("expected the cert_update frame to wake CertUpdates()")
	}
}

func TestWSSenderCAUpdateAndReconnectWakeTrustWithoutStealingCertWake(t *testing.T) {
	g, up := newServerPusher()
	ts := httptest.NewServer(g.handler(up))
	defer ts.Close()
	s := newTestWSSender(t, wsHTTPToWS(ts.URL))
	defer s.Close()
	if err := s.Post(context.Background(), wsTestSample()); err != nil {
		t.Fatal(err)
	}
	<-up
	select {
	case <-s.CertUpdates():
	case <-time.After(time.Second):
		t.Fatal("connect did not wake cert")
	}
	select {
	case <-s.TrustUpdates():
	case <-time.After(time.Second):
		t.Fatal("connect did not wake trust")
	}

	g.push(t, []byte(`{"type":"cert_update","data":{"fingerprint":"leaf"}}`))
	select {
	case <-s.CertUpdates():
	case <-time.After(time.Second):
		t.Fatal("cert_update did not wake cert")
	}
	select {
	case <-s.TrustUpdates():
		t.Fatal("cert_update stole trust wake")
	case <-time.After(30 * time.Millisecond):
	}

	g.push(t, []byte(`{"type":"ca_update","data":{"fingerprint":"root"}}`))
	select {
	case <-s.TrustUpdates():
	case <-time.After(time.Second):
		t.Fatal("ca_update did not wake trust")
	}
	select {
	case <-s.CertUpdates():
		t.Fatal("ca_update stole cert wake")
	case <-time.After(30 * time.Millisecond):
	}
}

// TestWSSenderMalformedAndUnknownFramesDoNotDropConnectionOrWake is the
// direct proof of the Task 5b house rule: readLoop must NEVER use
// wsjson.Read, because it writes a protocol-error close frame on ANY JSON
// decode failure -- turning a malformed (or simply forward-incompatible)
// frame into a full connection teardown. It also proves an unknown-but-
// well-formed frame type is silently discarded (forward compatibility), and
// that only "cert_update" specifically wakes CertUpdates().
func TestWSSenderMalformedAndUnknownFramesDoNotDropConnectionOrWake(t *testing.T) {
	g, up := newServerPusher()
	ts := httptest.NewServer(g.handler(up))
	defer ts.Close()
	s := newTestWSSender(t, wsHTTPToWS(ts.URL))
	defer s.Close()

	if err := s.Post(context.Background(), wsTestSample()); err != nil {
		t.Fatalf("post: %v", err)
	}
	<-up
	select {
	case <-s.CertUpdates():
	case <-time.After(time.Second):
		t.Fatal("expected the initial connect wake")
	}
	conn := s.currentConn()
	if conn == nil {
		t.Fatal("expected an active connection")
	}

	// A malformed (non-JSON) frame: if readLoop used wsjson.Read here, the
	// decode failure would close the connection with a protocol-error status
	// -- the very next check would then see a nil/replaced connection.
	g.push(t, []byte("not json at all"))
	time.Sleep(30 * time.Millisecond)
	if s.currentConn() != conn {
		t.Fatal("connection was dropped after a malformed frame; readLoop must tolerate a decode failure, not close the connection")
	}
	select {
	case <-s.CertUpdates():
		t.Fatal("a malformed frame must not wake CertUpdates")
	default:
	}

	// A well-formed but unrecognized frame type must also be a no-op.
	g.push(t, []byte(`{"type":"some-future-feature","data":{"anything":true}}`))
	time.Sleep(30 * time.Millisecond)
	if s.currentConn() != conn {
		t.Fatal("connection was dropped after an unrecognized frame type")
	}
	select {
	case <-s.CertUpdates():
		t.Fatal("an unrecognized frame type must not wake CertUpdates")
	default:
	}

	// Finally: a genuine cert_update DOES wake CertUpdates(), and the
	// connection is still the exact same one throughout -- never dropped or
	// redialed by anything sent so far.
	g.push(t, []byte(`{"type":"cert_update","data":{"fingerprint":"abc123"}}`))
	select {
	case <-s.CertUpdates():
	case <-time.After(time.Second):
		t.Fatal("expected cert_update to wake CertUpdates()")
	}
	if s.currentConn() != conn {
		t.Fatal("connection changed at some point; expected it to remain the same throughout")
	}
}

// TestWSSenderCertUpdatesChannelCoalescesBursts proves the buffered(1) +
// non-blocking-send design: pushing two cert_update frames back-to-back
// without draining CertUpdates() in between must not block readLoop (which
// would otherwise wedge the connection) and must coalesce into a single
// pending signal.
func TestWSSenderCertUpdatesChannelCoalescesBursts(t *testing.T) {
	g, up := newServerPusher()
	ts := httptest.NewServer(g.handler(up))
	defer ts.Close()
	s := newTestWSSender(t, wsHTTPToWS(ts.URL))
	defer s.Close()

	if err := s.Post(context.Background(), wsTestSample()); err != nil {
		t.Fatalf("post: %v", err)
	}
	<-up
	select {
	case <-s.CertUpdates():
	case <-time.After(time.Second):
		t.Fatal("expected the initial connect wake")
	}

	// Push BOTH frames, then wait long enough for readLoop to process both
	// WITHOUT this test draining CertUpdates() in between. That "no drain
	// in between" is load-bearing for what this test actually proves: only
	// while the channel stays non-empty for the whole burst does the second
	// frame's wake attempt land on an already-full channel and get dropped.
	// Draining between the two pushes (e.g. a blocking read racing readLoop
	// mid-burst) would legitimately re-arm the channel for the second push
	// and is a different, unrelated behavior this test is not about.
	g.push(t, []byte(`{"type":"cert_update","data":{"fingerprint":"a"}}`))
	g.push(t, []byte(`{"type":"cert_update","data":{"fingerprint":"b"}}`))
	time.Sleep(300 * time.Millisecond)

	// readLoop must not have wedged on the burst: the connection still
	// accepts a further Post (proves the goroutine kept servicing conn.Read
	// throughout).
	if err := s.Post(context.Background(), wsTestSample()); err != nil {
		t.Fatalf("post after burst: %v", err)
	}

	select {
	case <-s.CertUpdates():
	default:
		t.Fatal("expected a coalesced wake to already be pending after the burst")
	}
	select {
	case <-s.CertUpdates():
		t.Fatal("a two-frame burst must coalesce into exactly one pending wake, not two")
	default:
	}
}

// drainInitialRuntimeConfigWake reads and discards the nil payload every
// successful (re)connect pushes onto RuntimeUpdates() (maybeDial's
// s.wakeRuntimeConfig(nil) call), so a test's own pushed frame cannot be
// mistaken for it -- the direct analogue of the cert/trust "drain the
// connect-triggered wake first" gotcha documented on
// TestWSSenderCertUpdateFrameWakesCertUpdates.
func drainInitialRuntimeConfigWake(t *testing.T, s *WSSender) {
	t.Helper()
	select {
	case v := <-s.RuntimeUpdates():
		if v != nil {
			t.Fatalf("expected the initial connect wake to carry a nil payload, got %s", v)
		}
	case <-time.After(time.Second):
		t.Fatal("expected the initial connect wake on RuntimeUpdates()")
	}
}

// TestRuntimeConfigFramePayloadDelivered proves the gateway->agent
// runtime_config push: a well-formed {"type":"runtime_config","data":{...}}
// frame's Data payload is delivered on RuntimeUpdates() exactly as sent.
func TestRuntimeConfigFramePayloadDelivered(t *testing.T) {
	g, up := newServerPusher()
	ts := httptest.NewServer(g.handler(up))
	defer ts.Close()
	s := newTestWSSender(t, wsHTTPToWS(ts.URL))
	defer s.Close()

	if err := s.Post(context.Background(), wsTestSample()); err != nil {
		t.Fatalf("post: %v", err)
	}
	<-up
	drainInitialRuntimeConfigWake(t, s)

	g.push(t, []byte(`{"type":"runtime_config","data":{"router_listen":9000,"specs":[]}}`))
	select {
	case got := <-s.RuntimeUpdates():
		if string(got) != `{"router_listen":9000,"specs":[]}` {
			t.Fatalf("RuntimeUpdates() payload = %s, want the frame's exact data field", got)
		}
	case <-time.After(time.Second):
		t.Fatal("expected the runtime_config frame to deliver its payload on RuntimeUpdates()")
	}
}

// TestRuntimeUpdatesLatestWins is the test that distinguishes the required
// "drain then send" doorbell from a plain buffered(1) non-blocking-send
// channel: pushing two runtime_config frames back-to-back, without draining
// RuntimeUpdates() in between, must leave the SECOND document pending, not
// the first. A plain buffered(1) channel with only a non-blocking send would
// drop the second send (the channel is already full from the first) and a
// reader would observe the FIRST, stale document -- exactly what "latest
// wins" forbids, since each frame is the full config and an older one must
// never survive over a newer one.
func TestRuntimeUpdatesLatestWins(t *testing.T) {
	g, up := newServerPusher()
	ts := httptest.NewServer(g.handler(up))
	defer ts.Close()
	s := newTestWSSender(t, wsHTTPToWS(ts.URL))
	defer s.Close()

	if err := s.Post(context.Background(), wsTestSample()); err != nil {
		t.Fatalf("post: %v", err)
	}
	<-up
	drainInitialRuntimeConfigWake(t, s)

	// Push BOTH frames before this test reads RuntimeUpdates() at all --
	// that "no drain in between" is load-bearing, exactly as documented on
	// TestWSSenderCertUpdatesChannelCoalescesBursts.
	g.push(t, []byte(`{"type":"runtime_config","data":{"v":1}}`))
	g.push(t, []byte(`{"type":"runtime_config","data":{"v":2}}`))
	time.Sleep(300 * time.Millisecond)

	var got json.RawMessage
	select {
	case got = <-s.RuntimeUpdates():
	case <-time.After(time.Second):
		t.Fatal("expected a coalesced pending document after the burst")
	}
	if string(got) != `{"v":2}` {
		t.Fatalf("latest-wins violated: RuntimeUpdates() = %s, want the SECOND (newest) document {\"v\":2}", got)
	}
	select {
	case extra := <-s.RuntimeUpdates():
		t.Fatalf("a two-frame burst must coalesce into exactly one pending document, got a second: %s", extra)
	default:
	}
}

// TestRuntimeUpdatesReadLoopNeverBlocksWithoutAReader proves the load-bearing
// constraint from the task brief: readLoop must never block on the
// RuntimeUpdates() send, even under a sustained burst with NO reader ever
// draining the channel -- because readLoop is also what detects a dead
// peer, and a wedged readLoop would silently stall connection health
// detection. It never drains RuntimeUpdates() at all (not even the initial
// connect wake), pushes a burst of frames, then proves the connection is
// still alive two independent ways: a further application-level Post
// succeeds, and a raw WebSocket ping/pong round-trip on the SAME connection
// still completes.
func TestRuntimeUpdatesReadLoopNeverBlocksWithoutAReader(t *testing.T) {
	g, up := newServerPusher()
	ts := httptest.NewServer(g.handler(up))
	defer ts.Close()
	s := newTestWSSender(t, wsHTTPToWS(ts.URL))
	defer s.Close()

	if err := s.Post(context.Background(), wsTestSample()); err != nil {
		t.Fatalf("post: %v", err)
	}
	<-up
	conn := s.currentConn()
	if conn == nil {
		t.Fatal("expected an active connection")
	}

	// Deliberately never drain RuntimeUpdates() -- not the connect wake, not
	// any of the pushes below.
	for i := 0; i < 20; i++ {
		g.push(t, []byte(`{"type":"runtime_config","data":{"n":`+strconv.Itoa(i)+`}}`))
	}
	time.Sleep(300 * time.Millisecond)

	if s.currentConn() != conn {
		t.Fatal("connection was dropped by an unread runtime_config burst; readLoop must never block/wedge on the payload channel send")
	}
	if err := s.Post(context.Background(), wsTestSample()); err != nil {
		t.Fatalf("post after unread runtime_config burst: %v", err)
	}
	pctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := conn.Ping(pctx); err != nil {
		t.Fatalf("ping/pong liveness round-trip failed after unread runtime_config burst: %v", err)
	}
}

// TestReconnectWakesRuntimeNil proves a reconnect wakes RuntimeUpdates()
// with a nil payload (meaning "resync over HTTP"), and that this nil is
// distinguishable from a real pushed document (a non-nil json.RawMessage).
func TestReconnectWakesRuntimeNil(t *testing.T) {
	fg := &fakeGateway{closeNow: websocket.StatusGoingAway}
	ts := httptest.NewServer(http.HandlerFunc(fg.handler))
	defer ts.Close()
	s := newTestWSSender(t, wsHTTPToWS(ts.URL))
	defer s.Close()

	if err := s.Post(context.Background(), wsTestSample()); err != nil {
		t.Fatalf("post: %v", err)
	}
	waitFor(t, func() bool { return len(fg.got()) >= 1 })
	drainInitialRuntimeConfigWake(t, s)
	time.Sleep(20 * time.Millisecond) // let the read loop observe the close + schedule the reconnect

	for i := 0; i < 5; i++ {
		_ = s.Post(context.Background(), wsTestSample())
		time.Sleep(10 * time.Millisecond)
	}

	select {
	case v := <-s.RuntimeUpdates():
		if v != nil {
			t.Fatalf("expected a nil payload on the reconnect wake, got a real document: %s", v)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("expected the reconnect to wake RuntimeUpdates() with a nil payload")
	}
}
