// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/usage"
	"runtime"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// pushTestQueueCapacity is the send queue of every connection these tests
// register. It exceeds the most frames any test can enqueue, so no frame is
// ever dropped for a full queue and the queue shows every frame the worker
// sent.
const pushTestQueueCapacity = 4096

// pushTestPortal is the push serializer's portal double. Its store is one
// version string per server. AgentRuntimeConfig derives {ETag: the version at
// the derive's start}, numbers every derive from 1 in start order, reports
// each start on starts (when starts is not nil), and holds derive n open until
// release(n) when hold(n) was called first. A held derive whose context
// expires reports n on expired and still waits for its release, so the test
// decides when the timed-out derive returns.
type pushTestPortal struct {
	portal.API // embedded nil interface; only AgentRuntimeConfig is called
	starts     chan int
	expired    chan int

	mu          sync.Mutex
	store       map[string]string
	versions    map[string]int
	derives     int
	inFlight    map[string]int
	maxInFlight map[string]int
	holds       map[int]chan struct{}
	failures    map[int]error
}

func newPushTestPortal() *pushTestPortal {
	return &pushTestPortal{
		starts:      make(chan int, 64),
		expired:     make(chan int, 64),
		store:       map[string]string{},
		versions:    map[string]int{},
		inFlight:    map[string]int{},
		maxInFlight: map[string]int{},
		holds:       map[int]chan struct{}{},
		failures:    map[int]error{},
	}
}

func (p *pushTestPortal) AgentRuntimeConfig(ctx context.Context, serverID string) (portal.AgentRuntimeConfigDTO, error) {
	p.mu.Lock()
	p.derives++
	n := p.derives
	etag := p.store[serverID]
	gate, failure := p.holds[n], p.failures[n]
	p.inFlight[serverID]++
	p.maxInFlight[serverID] = max(p.maxInFlight[serverID], p.inFlight[serverID])
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		p.inFlight[serverID]--
		p.mu.Unlock()
	}()
	if p.starts != nil {
		p.starts <- n
	}
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			p.expired <- n
			<-gate
		}
	}
	if err := ctx.Err(); err != nil {
		return portal.AgentRuntimeConfigDTO{}, err
	}
	if failure != nil {
		return portal.AgentRuntimeConfigDTO{}, failure
	}
	return portal.AgentRuntimeConfigDTO{ETag: etag}, nil
}

// write sets serverID's store to version.
func (p *pushTestPortal) write(serverID, version string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.store[serverID] = version
}

// bump increments serverID's numeric store version and returns it.
func (p *pushTestPortal) bump(serverID string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.versions[serverID]++
	p.store[serverID] = strconv.Itoa(p.versions[serverID])
	return p.versions[serverID]
}

// hold makes derive n wait for release(n) once it starts.
func (p *pushTestPortal) hold(n int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.holds[n] = make(chan struct{})
}

// fail makes derive n return err once it is released.
func (p *pushTestPortal) fail(n int, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.failures[n] = err
}

// release lets derive n return.
func (p *pushTestPortal) release(n int) {
	p.mu.Lock()
	gate := p.holds[n]
	delete(p.holds, n)
	p.mu.Unlock()
	if gate != nil {
		close(gate)
	}
}

// releaseAll lets every held derive return, so a failed test leaves no
// worker blocked on a gate.
func (p *pushTestPortal) releaseAll() {
	p.mu.Lock()
	gates := p.holds
	p.holds = map[int]chan struct{}{}
	p.mu.Unlock()
	for _, gate := range gates {
		close(gate)
	}
}

// started is the number of derives that have started, over all servers.
func (p *pushTestPortal) started() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.derives
}

// mostConcurrent is the most derives that ran at once for serverID.
func (p *pushTestPortal) mostConcurrent(serverID string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.maxInFlight[serverID]
}

// owesPass reports whether serverID's worker runs and owes one more pass.
func (p *runtimeConfigPusher) owesPass(serverID string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	w, ok := p.workers[serverID]
	return ok && w.dirty
}

// workerCount is the number of servers with a running worker.
func (p *runtimeConfigPusher) workerCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.workers)
}

// newPushTestServer returns a bare Server (no spacing, the default push
// bound) whose pushes derive through p. Every server in serverIDs has
// runtime_manager declared and one registered connection, returned in the
// same order.
func newPushTestServer(t *testing.T, p *pushTestPortal, serverIDs ...string) (*Server, []*agentStreamConn) {
	t.Helper()
	t.Cleanup(p.releaseAll)
	srv := &Server{
		Portal:        p,
		AgentFeatures: NewAgentFeaturesRegistry(),
		AgentStreams:  NewAgentStreamRegistry(),
		RuntimeStatus: NewRuntimeStatusRegistry(),
	}
	conns := make([]*agentStreamConn, 0, len(serverIDs))
	for _, id := range serverIDs {
		srv.AgentFeatures.Set(id, []string{"runtime_manager"})
		c := &agentStreamConn{out: make(chan []byte, pushTestQueueCapacity)}
		srv.AgentStreams.add(id, c)
		conns = append(conns, c)
	}
	return srv, conns
}

// notifyPromptly calls srv.PushRuntimeConfig and fails the test when the call
// does not return at once: the portal's write-path hook must never wait on a
// derive.
func notifyPromptly(t *testing.T, srv *Server, serverID string) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		srv.PushRuntimeConfig(serverID)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("PushRuntimeConfig(%q) did not return within 2s: the hook must never wait on a derive or a pass", serverID)
	}
}

// waitDeriveStart waits for the next derive start p reports and fails unless
// it is derive want.
func waitDeriveStart(t *testing.T, p *pushTestPortal, want int) {
	t.Helper()
	select {
	case n := <-p.starts:
		if n != want {
			t.Fatalf("derive %d started, want derive %d", n, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("derive %d never started", want)
	}
}

// waitDeriveExpired waits until held derive want has seen its context expire.
func waitDeriveExpired(t *testing.T, p *pushTestPortal, want int) {
	t.Helper()
	select {
	case n := <-p.expired:
		if n != want {
			t.Fatalf("derive %d expired, want derive %d", n, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("derive %d never saw its context expire: the push bound did not apply", want)
	}
}

// waitPushIdle waits until no push worker runs for any of serverIDs.
func waitPushIdle(t *testing.T, srv *Server, serverIDs ...string) {
	t.Helper()
	for _, id := range serverIDs {
		select {
		case <-srv.runtimePush.idle(id):
		case <-time.After(10 * time.Second):
			t.Fatalf("the push worker for %s never went idle", id)
		}
	}
}

// requireOwedPass fails unless exactly one derive has started and serverID's
// worker owes one more pass: what a notification during held derive 1 must
// leave behind.
func requireOwedPass(t *testing.T, srv *Server, p *pushTestPortal, serverID string) {
	t.Helper()
	if got := p.started(); got != 1 {
		t.Fatalf("derives started while derive 1 was held = %d, want 1: a second derive overlapped the first", got)
	}
	if !srv.runtimePush.owesPass(serverID) {
		t.Fatal("the notification during the held derive left no pass owed: its write can be lost, or its document can overtake the held one")
	}
}

// decodePushedDocument parses one queued runtime_config frame.
func decodePushedDocument(raw []byte) (portal.AgentRuntimeConfigDTO, error) {
	var f streamFrame
	if err := json.Unmarshal(raw, &f); err != nil {
		return portal.AgentRuntimeConfigDTO{}, fmt.Errorf("unmarshal frame %s: %w", raw, err)
	}
	if f.Type != "runtime_config" {
		return portal.AgentRuntimeConfigDTO{}, fmt.Errorf("frame type = %q, want runtime_config", f.Type)
	}
	var doc portal.AgentRuntimeConfigDTO
	if err := json.Unmarshal(f.Data, &doc); err != nil {
		return portal.AgentRuntimeConfigDTO{}, fmt.Errorf("unmarshal document %s: %w", f.Data, err)
	}
	return doc, nil
}

// drainETags drains c's queue without blocking and returns each document's
// ETag in queue order. It reports instead of failing, so a goroutine other
// than the test's can call it.
func drainETags(c *agentStreamConn) ([]string, error) {
	var etags []string
	for {
		select {
		case raw := <-c.out:
			doc, err := decodePushedDocument(raw)
			if err != nil {
				return etags, err
			}
			etags = append(etags, doc.ETag)
		default:
			return etags, nil
		}
	}
}

// queuedDocuments drains c's queue without blocking and returns the pushed
// documents in queue order.
func queuedDocuments(t *testing.T, c *agentStreamConn) []portal.AgentRuntimeConfigDTO {
	t.Helper()
	var docs []portal.AgentRuntimeConfigDTO
	for {
		select {
		case raw := <-c.out:
			doc, err := decodePushedDocument(raw)
			if err != nil {
				t.Fatal(err)
			}
			docs = append(docs, doc)
		default:
			return docs
		}
	}
}

// queuedETags is queuedDocuments reduced to each document's ETag.
func queuedETags(t *testing.T, c *agentStreamConn) []string {
	t.Helper()
	docs := queuedDocuments(t, c)
	etags := make([]string, 0, len(docs))
	for _, d := range docs {
		etags = append(etags, d.ETag)
	}
	return etags
}

// TestRuntimeConfigPushOrdersDocumentsByDerive: a stop whose derive is still
// running when a clear is written and notified reaches the connection before
// the clear. With a goroutine per notification the clear's document would be
// enqueued first and the stale stop last, and the agent, which adopts every
// document whose ETag differs from its own, would end on the stop while the
// store holds the clear.
func TestRuntimeConfigPushOrdersDocumentsByDerive(t *testing.T) {
	p := newPushTestPortal()
	srv, conns := newPushTestServer(t, p, "srv1")
	p.hold(1)
	p.write("srv1", "stop")
	notifyPromptly(t, srv, "srv1")
	waitDeriveStart(t, p, 1)

	p.write("srv1", "clear")
	notifyPromptly(t, srv, "srv1")
	requireOwedPass(t, srv, p, "srv1")

	p.release(1)
	waitPushIdle(t, srv, "srv1")
	if got, want := queuedETags(t, conns[0]), []string{"stop", "clear"}; !slices.Equal(got, want) {
		t.Fatalf("enqueued = %v, want %v: documents reach the connection in derive order, and the last reflects the latest write", got, want)
	}
}

// TestRuntimeConfigPushCoalescesWritesDuringADerive: nine writes that arrive
// while a derive runs cost one more pass, not nine, and that pass sees the
// last of them.
func TestRuntimeConfigPushCoalescesWritesDuringADerive(t *testing.T) {
	p := newPushTestPortal()
	srv, conns := newPushTestServer(t, p, "srv1")
	p.hold(1)
	p.write("srv1", "w01")
	notifyPromptly(t, srv, "srv1")
	waitDeriveStart(t, p, 1)
	for i := 2; i <= 10; i++ {
		p.write("srv1", fmt.Sprintf("w%02d", i))
		notifyPromptly(t, srv, "srv1")
	}
	requireOwedPass(t, srv, p, "srv1")

	p.release(1)
	waitPushIdle(t, srv, "srv1")
	if got := p.started(); got != 2 {
		t.Fatalf("derives = %d, want 2: the writes during the held derive coalesce into one owed pass", got)
	}
	if got, want := queuedETags(t, conns[0]), []string{"w01", "w10"}; !slices.Equal(got, want) {
		t.Fatalf("enqueued = %v, want %v", got, want)
	}
}

// TestRuntimeConfigPushRunsTheOwedPassAfterAFailedDerive: a derive that fails
// or outlives the push bound enqueues nothing and is not retried, but the pass
// a later write owes still runs and delivers the latest document.
func TestRuntimeConfigPushRunsTheOwedPassAfterAFailedDerive(t *testing.T) {
	t.Run("a store error", func(t *testing.T) {
		p := newPushTestPortal()
		srv, conns := newPushTestServer(t, p, "srv1")
		p.hold(1)
		p.fail(1, errors.New("store unavailable"))
		p.write("srv1", "a")
		notifyPromptly(t, srv, "srv1")
		waitDeriveStart(t, p, 1)
		p.write("srv1", "b")
		notifyPromptly(t, srv, "srv1")
		requireOwedPass(t, srv, p, "srv1")

		p.release(1)
		waitPushIdle(t, srv, "srv1")
		if got, want := queuedETags(t, conns[0]), []string{"b"}; !slices.Equal(got, want) {
			t.Fatalf("enqueued = %v, want %v: the failed derive sends nothing, and the owed pass sends the latest write", got, want)
		}
	})

	t.Run("a derive past the push bound", func(t *testing.T) {
		p := newPushTestPortal()
		srv, conns := newPushTestServer(t, p, "srv1")
		srv.pushRuntimeConfigTimeout = 20 * time.Millisecond
		p.hold(1)
		p.write("srv1", "a")
		notifyPromptly(t, srv, "srv1")
		waitDeriveStart(t, p, 1)
		p.write("srv1", "b")
		notifyPromptly(t, srv, "srv1")
		requireOwedPass(t, srv, p, "srv1")

		waitDeriveExpired(t, p, 1)
		p.release(1)
		waitPushIdle(t, srv, "srv1")
		if got, want := queuedETags(t, conns[0]), []string{"b"}; !slices.Equal(got, want) {
			t.Fatalf("enqueued = %v, want %v: the timed-out derive sends nothing, and the owed pass sends the latest write", got, want)
		}
	})
}

// TestRuntimeConfigPushServersAreIndependent: a held derive delays only its
// own server. Another server's push runs to completion meanwhile.
func TestRuntimeConfigPushServersAreIndependent(t *testing.T) {
	p := newPushTestPortal()
	srv, conns := newPushTestServer(t, p, "srv1", "srv2")
	p.hold(1)
	p.write("srv1", "a")
	notifyPromptly(t, srv, "srv1")
	waitDeriveStart(t, p, 1)

	p.write("srv2", "x")
	notifyPromptly(t, srv, "srv2")
	waitPushIdle(t, srv, "srv2")
	if got, want := queuedETags(t, conns[1]), []string{"x"}; !slices.Equal(got, want) {
		t.Fatalf("srv2 enqueued = %v while srv1's derive was held, want %v", got, want)
	}
	if got := queuedETags(t, conns[0]); len(got) != 0 {
		t.Fatalf("srv1 enqueued = %v while its derive was held, want nothing", got)
	}

	p.release(1)
	waitPushIdle(t, srv, "srv1")
	if got, want := queuedETags(t, conns[0]), []string{"a"}; !slices.Equal(got, want) {
		t.Fatalf("srv1 enqueued = %v, want %v", got, want)
	}
}

// TestRuntimeConfigPushKeepsNoStateForAnIdleServer: a worker leaves the map
// when its last pass leaves it clean, so an idle server and an unknown one
// keep no state and need no pruning, and an empty server id starts nothing.
func TestRuntimeConfigPushKeepsNoStateForAnIdleServer(t *testing.T) {
	p := newPushTestPortal()
	srv, _ := newPushTestServer(t, p, "srv1", "srv2")
	for _, id := range []string{"srv1", "srv2", "srv1", "srv-unknown"} {
		p.write(id, "v")
		notifyPromptly(t, srv, id)
		waitPushIdle(t, srv, id)
	}
	if got := srv.runtimePush.workerCount(); got != 0 {
		t.Fatalf("workers = %d once every server went idle, want 0", got)
	}
	if got := p.started(); got != 3 {
		t.Fatalf("derives = %d, want 3 (srv1, srv2, srv1; srv-unknown never declared runtime_manager)", got)
	}

	// An empty server id starts no worker at all, whatever its pass would do.
	var pusher runtimeConfigPusher
	var passes atomic.Int32
	pusher.notify("", 0, func(string) bool {
		passes.Add(1)
		return false
	})
	select {
	case <-pusher.idle(""):
	case <-time.After(2 * time.Second):
		t.Fatal("a worker for the empty server id never went idle")
	}
	if got := passes.Load(); got != 0 {
		t.Fatalf("passes for the empty server id = %d, want 0: an empty id starts no worker", got)
	}
}

// manualSpacing is a spacing timer the test fires by hand. Each wait is
// recorded and announced on waits; the channel it returns delivers once per
// fire.
type manualSpacing struct {
	mu        sync.Mutex
	durations []time.Duration
	waits     chan struct{}
	fired     chan time.Time
}

func newManualSpacing(t *testing.T) *manualSpacing {
	t.Helper()
	m := &manualSpacing{waits: make(chan struct{}, 16), fired: make(chan time.Time)}
	t.Cleanup(func() { close(m.fired) })
	return m
}

func (m *manualSpacing) after(d time.Duration) <-chan time.Time {
	m.mu.Lock()
	m.durations = append(m.durations, d)
	m.mu.Unlock()
	m.waits <- struct{}{}
	return m.fired
}

// waitSpacing waits until the worker has started spacing wait n.
func (m *manualSpacing) waitSpacing(t *testing.T, n int) {
	t.Helper()
	select {
	case <-m.waits:
	case <-time.After(2 * time.Second):
		t.Fatalf("the worker never started spacing wait %d", n)
	}
}

// fire ends the worker's current spacing wait.
func (m *manualSpacing) fire(t *testing.T) {
	t.Helper()
	select {
	case m.fired <- time.Time{}:
	case <-time.After(2 * time.Second):
		t.Fatal("no worker was waiting on the spacing timer")
	}
}

func (m *manualSpacing) recorded() []time.Duration {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.durations)
}

// TestRuntimeConfigPushSpacesConsecutivePasses: after a pass that enqueued a
// frame, the worker waits pushRuntimeConfigSpacing before its next pass. The
// first document after a quiet period goes out at once, notifications inside
// the wait start no derive, and one pass after it sends the latest of them. A
// spacing of 0, which a bare Server has, means no wait at all.
func TestRuntimeConfigPushSpacesConsecutivePasses(t *testing.T) {
	t.Run("250 ms after a pass that sent a frame", func(t *testing.T) {
		p := newPushTestPortal()
		srv, conns := newPushTestServer(t, p, "srv1")
		srv.pushRuntimeConfigSpacing = 250 * time.Millisecond
		timer := newManualSpacing(t)
		srv.runtimePush.after = timer.after

		p.write("srv1", "v1")
		notifyPromptly(t, srv, "srv1")
		timer.waitSpacing(t, 1)
		if got, want := queuedETags(t, conns[0]), []string{"v1"}; !slices.Equal(got, want) {
			t.Fatalf("enqueued before the first spacing wait = %v, want %v: the first document after a quiet period goes out at once", got, want)
		}
		for _, v := range []string{"v2", "v3", "v4"} {
			p.write("srv1", v)
			notifyPromptly(t, srv, "srv1")
		}
		if got := p.started(); got != 1 {
			t.Fatalf("derives during the spacing wait = %d, want 1: notifications inside it wait for its end", got)
		}

		timer.fire(t)
		timer.waitSpacing(t, 2)
		if got, want := queuedETags(t, conns[0]), []string{"v4"}; !slices.Equal(got, want) {
			t.Fatalf("enqueued after the first spacing wait = %v, want %v: the notifications inside it coalesce into one pass", got, want)
		}
		timer.fire(t)
		waitPushIdle(t, srv, "srv1")
		if got := p.started(); got != 2 {
			t.Fatalf("derives = %d, want 2", got)
		}
		if got, want := timer.recorded(), []time.Duration{250 * time.Millisecond, 250 * time.Millisecond}; !slices.Equal(got, want) {
			t.Fatalf("spacing waits = %v, want %v", got, want)
		}
	})

	t.Run("a spacing of 0", func(t *testing.T) {
		p := newPushTestPortal()
		srv, conns := newPushTestServer(t, p, "srv1")
		waits := countingSpacing(srv)
		srv.pushRuntimeConfigSpacing = 0 // the counting timer stays installed
		p.hold(1)
		p.write("srv1", "v1")
		notifyPromptly(t, srv, "srv1")
		waitDeriveStart(t, p, 1)
		p.write("srv1", "v2")
		notifyPromptly(t, srv, "srv1")
		requireOwedPass(t, srv, p, "srv1")

		p.release(1)
		waitPushIdle(t, srv, "srv1")
		if got, want := queuedETags(t, conns[0]), []string{"v1", "v2"}; !slices.Equal(got, want) {
			t.Fatalf("enqueued = %v, want %v", got, want)
		}
		if got := waits.Load(); got != 0 {
			t.Fatalf("spacing waits = %d with a spacing of 0, want 0: 0 means no spacing", got)
		}
	})
}

// TestRuntimeConfigPushBurstsKeepOrderAndLatest: concurrent writes and
// notifications for three servers. In a long burst the documents of each
// server never go back to an older version, the last one is the store's final
// version, and no two derives run at once. In many short rounds of two
// concurrent writes each, every round ends on its own latest write: a round's
// second notification often lands just as the worker finishes the first
// one's pass, which is where a lost wakeup would drop it.
func TestRuntimeConfigPushBurstsKeepOrderAndLatest(t *testing.T) {
	ids := []string{"srv1", "srv2", "srv3"}

	t.Run("a long burst", func(t *testing.T) {
		const writersPerServer, writesPerWriter = 6, 300
		p := newPushTestPortal()
		p.starts = nil
		srv, conns := newPushTestServer(t, p, ids...)

		var wg sync.WaitGroup
		for _, id := range ids {
			for g := range writersPerServer {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for i := range writesPerWriter {
						p.bump(id)
						srv.PushRuntimeConfig(id)
						if (i+g)%3 == 0 {
							runtime.Gosched()
						}
					}
				}()
			}
		}
		wg.Wait()
		waitPushIdle(t, srv, ids...)

		final := strconv.Itoa(writersPerServer * writesPerWriter)
		for i, id := range ids {
			etags := queuedETags(t, conns[i])
			if len(etags) == 0 {
				t.Fatalf("%s: no frame enqueued", id)
			}
			prev := 0
			for k, etag := range etags {
				v, err := strconv.Atoi(etag)
				if err != nil {
					t.Fatalf("%s frame %d: ETag %q is not a version", id, k, etag)
				}
				if v < prev {
					t.Fatalf("%s frame %d: version %d after %d: a document derived before another was enqueued after it", id, k, v, prev)
				}
				prev = v
			}
			if last := etags[len(etags)-1]; last != final {
				t.Fatalf("%s: last frame = %s, want the final store version %s (%d frames)", id, last, final, len(etags))
			}
			if got := p.mostConcurrent(id); got > 1 {
				t.Fatalf("%s: %d derives ran at once, want at most 1", id, got)
			}
		}
	})

	t.Run("many short rounds", func(t *testing.T) {
		const rounds = 2000
		p := newPushTestPortal()
		p.starts = nil
		srv, conns := newPushTestServer(t, p, ids...)
		failures := make(chan string, len(ids))
		var servers sync.WaitGroup
		for i, id := range ids {
			servers.Add(1)
			go func() {
				defer servers.Done()
				if msg := runShortPushRounds(srv, p, conns[i], id, rounds); msg != "" {
					failures <- msg
				}
			}()
		}
		servers.Wait()
		close(failures)
		for msg := range failures {
			t.Error(msg)
		}
	})
}

// runShortPushRounds runs rounds for serverID, each of two concurrent writes
// and notifications followed by the worker's exit, and returns a description
// of the first round that did not end on its latest write ("" when every
// round did). It reports instead of failing, because it runs on a goroutine
// of its own.
func runShortPushRounds(srv *Server, p *pushTestPortal, c *agentStreamConn, serverID string, rounds int) string {
	for r := 1; r <= rounds; r++ {
		var wg sync.WaitGroup
		for range 2 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				p.bump(serverID)
				srv.PushRuntimeConfig(serverID)
			}()
		}
		wg.Wait()
		select {
		case <-srv.runtimePush.idle(serverID):
		case <-time.After(10 * time.Second):
			return fmt.Sprintf("%s round %d: the push worker never went idle", serverID, r)
		}
		etags, err := drainETags(c)
		if err != nil {
			return fmt.Sprintf("%s round %d: %v", serverID, r, err)
		}
		want := strconv.Itoa(2 * r)
		if len(etags) == 0 || etags[len(etags)-1] != want {
			return fmt.Sprintf("%s round %d: frames %v, want the round to end on version %s: a notification was lost", serverID, r, etags, want)
		}
	}
	return ""
}

// countingSpacing installs a spacing timer on srv that fires at once and
// counts its waits.
func countingSpacing(srv *Server) *atomic.Int32 {
	var waits atomic.Int32
	srv.pushRuntimeConfigSpacing = 250 * time.Millisecond
	srv.runtimePush.after = func(time.Duration) <-chan time.Time {
		waits.Add(1)
		fired := make(chan time.Time, 1)
		fired <- time.Time{}
		return fired
	}
	return &waits
}

// TestRuntimeConfigPushDoesNotSpaceAPassThatSentNothing: a pass that
// enqueued no frame starts no spacing wait, so the next notification derives
// at once. A pass sends nothing without an open connection (the POST
// transport, a disconnected agent), after a failed derive, behind a refused
// gate, and without a portal.
func TestRuntimeConfigPushDoesNotSpaceAPassThatSentNothing(t *testing.T) {
	t.Run("no open connection", func(t *testing.T) {
		p := newPushTestPortal()
		srv, _ := newPushTestServer(t, p)
		srv.AgentFeatures.Set("srv1", []string{"runtime_manager"})
		waits := countingSpacing(srv)
		for i := 1; i <= 3; i++ {
			p.write("srv1", fmt.Sprintf("v%d", i))
			notifyPromptly(t, srv, "srv1")
			waitPushIdle(t, srv, "srv1")
		}
		if got := p.started(); got != 3 {
			t.Fatalf("derives = %d, want 3: every notify/idle cycle runs one pass", got)
		}
		if got := waits.Load(); got != 0 {
			t.Fatalf("spacing waits = %d, want 0: a pass that enqueued no frame is not spaced", got)
		}
	})

	t.Run("a failed derive", func(t *testing.T) {
		p := newPushTestPortal()
		srv, conns := newPushTestServer(t, p, "srv1")
		waits := countingSpacing(srv)
		p.fail(1, errors.New("store unavailable"))
		notifyPromptly(t, srv, "srv1")
		waitPushIdle(t, srv, "srv1")
		if got := queuedETags(t, conns[0]); len(got) != 0 {
			t.Fatalf("enqueued = %v after a failed derive, want nothing", got)
		}
		if got := waits.Load(); got != 0 {
			t.Fatalf("spacing waits = %d after a failed derive, want 0", got)
		}
	})

	t.Run("a refused gate", func(t *testing.T) {
		p := newPushTestPortal()
		srv, conns := newPushTestServer(t, p, "srv1")
		waits := countingSpacing(srv)
		srv.RuntimeStatus.SetFileMode("srv1", true)
		notifyPromptly(t, srv, "srv1")
		waitPushIdle(t, srv, "srv1")
		if got := p.started(); got != 0 {
			t.Fatalf("derives = %d for a file-mode agent, want 0: the gate is checked before any store read", got)
		}
		if got := queuedETags(t, conns[0]); len(got) != 0 {
			t.Fatalf("enqueued = %v for a file-mode agent, want nothing", got)
		}
		if got := waits.Load(); got != 0 {
			t.Fatalf("spacing waits = %d behind a refused gate, want 0", got)
		}
	})

	t.Run("a nil Portal", func(t *testing.T) {
		p := newPushTestPortal()
		srv, conns := newPushTestServer(t, p, "srv1")
		waits := countingSpacing(srv)
		srv.Portal = nil
		notifyPromptly(t, srv, "srv1")
		waitPushIdle(t, srv, "srv1")
		if got := queuedETags(t, conns[0]); len(got) != 0 {
			t.Fatalf("enqueued = %v without a portal, want nothing", got)
		}
		if got := waits.Load(); got != 0 {
			t.Fatalf("spacing waits = %d without a portal, want 0", got)
		}
	})
}

// TestRuntimeConfigPushEndsOnTheStoreDocumentAfterAPortalBurst drives the
// real portal.Service, wired to PushRuntimeConfig as cmd/gateway wires it,
// through a burst of four benchmark admin_state writes, each of which
// notifies. The force_stopped set of the frames on the connection never
// shrinks, and the last frame is the document the store holds now.
func TestRuntimeConfigPushEndsOnTheStoreDocumentAfterAPortalBurst(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	mem := routing.NewMemoryStore()
	must := func(what string, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}
	must("CreateAIServer", mem.CreateAIServer(ctx, routing.AIServer{ID: "srv1", Name: "Host", Domain: "host.example.test", Provider: routing.ProviderMock, Endpoint: "mock://srv1", Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now}))
	must("CreateApplication", mem.CreateApplication(ctx, routing.Application{ID: "app1", ServerID: "srv1", Type: routing.ProviderServerAgent, Port: 9000, Scheme: "http", TimeoutMS: 30000, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}))
	specIDs := []string{"rs_1", "rs_2", "rs_3", "rs_4"}
	for i, id := range specIDs {
		mappingID := fmt.Sprintf("map_%d", i+1)
		must("CreateMapping("+mappingID+")", mem.CreateMapping(ctx, routing.ModelMapping{ID: mappingID, ApplicationID: "app1", GatewayModelName: fmt.Sprintf("gw-%d", i+1), AppModelName: fmt.Sprintf("up-%d", i+1), Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}))
		must("UpsertRuntimeSpec("+id+")", mem.UpsertRuntimeSpec(ctx, routing.RuntimeSpec{
			ID: id, MappingID: mappingID, Enabled: true, Binary: "/usr/local/bin/llama-server",
			Args: "[]", Env: "{}", HealthPath: "/health", HealthTimeoutSeconds: 5, StartupTimeoutSeconds: 180,
			CreatedAt: now, UpdatedAt: now,
		}))
	}
	dir := portal.NewMemoryDirectory(nil)
	svc := portal.NewService(portal.ServiceDeps{
		Users: dir, Groups: dir, Usage: usage.NewRecorder(), Routes: mem,
		Clock: func() time.Time { return now },
	})
	srv := &Server{
		Portal:        svc,
		AgentFeatures: NewAgentFeaturesRegistry(),
		AgentStreams:  NewAgentStreamRegistry(),
		RuntimeStatus: NewRuntimeStatusRegistry(),
	}
	srv.AgentFeatures.Set("srv1", []string{"runtime_manager"})
	conn := &agentStreamConn{out: make(chan []byte, pushTestQueueCapacity)}
	srv.AgentStreams.add("srv1", conn)
	svc.SetRuntimeConfigChangedHook(srv.PushRuntimeConfig)

	for _, id := range specIDs {
		if _, err := svc.SetBenchmarkRuntimeSpecAdminState(ctx, id, "", "force_stopped"); err != nil {
			t.Fatalf("SetBenchmarkRuntimeSpecAdminState(%s): %v", id, err)
		}
	}
	waitPushIdle(t, srv, "srv1")

	docs := queuedDocuments(t, conn)
	if len(docs) == 0 {
		t.Fatal("the burst enqueued no frame")
	}
	var prev []string
	for k, doc := range docs {
		var stopped []string
		for _, spec := range doc.Specs {
			if spec.AdminState == "force_stopped" {
				stopped = append(stopped, spec.ID)
			}
		}
		for _, id := range prev {
			if !slices.Contains(stopped, id) {
				t.Fatalf("frame %d of %d: force_stopped %v drops %s from the previous frame's %v", k+1, len(docs), stopped, id, prev)
			}
		}
		prev = stopped
	}
	if len(prev) != len(specIDs) {
		t.Fatalf("last frame force_stopped = %v, want all of %v", prev, specIDs)
	}
	want, err := svc.AgentRuntimeConfig(ctx, "srv1")
	if err != nil {
		t.Fatalf("AgentRuntimeConfig: %v", err)
	}
	if last := docs[len(docs)-1].ETag; last != want.ETag {
		t.Fatalf("last frame ETag = %s, want the store's current document %s", last, want.ETag)
	}
}
