// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/routing"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// --- the shared event log ---------------------------------------------------

// stopAllEvent is one thing the stop-all fixture saw, in the order it saw it:
// a batched admin-state write ("stop [ids]", "clear [ids]"; "stop! [ids]",
// "clear! [ids]" when it met a done context), a batched pinned write
// ("unpin [ids]", "repin [ids]"; "unpin! [ids]", "repin! [ids]" when it met a
// done context), a lease write ("lease clear=[ids] repin=[ids]", "lease! …"
// when it failed), a portal notification ("notify"), a document the fake
// agent applied ("doc [ids]", the spec ids it force-stops, followed by
// "pins [ids]", the spec ids it pins), or a chat the fake router answered
// ("chat <model> 200", "chat <model> 503").
type stopAllEvent struct {
	what string
	at   time.Time
}

type stopAllEvents struct {
	mu  sync.Mutex
	log []stopAllEvent
}

func (e *stopAllEvents) add(format string, args ...any) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.log = append(e.log, stopAllEvent{what: fmt.Sprintf(format, args...), at: time.Now()})
}

func (e *stopAllEvents) list() []stopAllEvent {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]stopAllEvent(nil), e.log...)
}

// with returns, in order, the events whose text starts with one of prefixes.
func (e *stopAllEvents) with(prefixes ...string) []string {
	var out []string
	for _, ev := range e.list() {
		for _, p := range prefixes {
			if strings.HasPrefix(ev.what, p) {
				out = append(out, ev.what)
				break
			}
		}
	}
	return out
}

// index is the position of the n-th (0-based) event whose text is exactly
// what, or -1.
func (e *stopAllEvents) index(what string, n int) int {
	for i, ev := range e.list() {
		if ev.what == what {
			if n == 0 {
				return i
			}
			n--
		}
	}
	return -1
}

// lastIndex is the position of the last event whose text is exactly what, or
// -1.
func (e *stopAllEvents) lastIndex(what string) int {
	last := -1
	for i, ev := range e.list() {
		if ev.what == what {
			last = i
		}
	}
	return last
}

// count is how many events have exactly this text.
func (e *stopAllEvents) count(what string) int {
	n := 0
	for _, ev := range e.list() {
		if ev.what == what {
			n++
		}
	}
	return n
}

// --- the fake agent -----------------------------------------------------------

// fakeAgentRuntimeSpec is one launch spec of fakeAgentRuntime.
type fakeAgentRuntimeSpec struct {
	id, model string
	// state and pid are what the agent reports at the start: "running" with
	// a pid, "stopped" with 0, or any other state.
	state string
	pid   int
	// load is how long a cold start of the child takes; exit is how long the
	// child takes to exit once a force_stopped document drains it.
	load, exit time.Duration
	// stuck is a child that never exits: a stop leaves it draining with its
	// pid.
	stuck bool
	// exitTo is the state a drained child ends in: "" is stopped, "backoff"
	// a child that crashed while it drained.
	exitTo string
	// backoff is how long a request for the spec waits while its row reads
	// backoff: the backoff timer. A force_stopped document cancels it.
	backoff time.Duration
	// adminState and pinned are what the agent applied before the test.
	adminState string
	pinned     bool
	// direct is a direct router client's traffic in flight on the spec,
	// reported in every row until the test ends it (setDirect).
	direct int
	// unreported is a spec the agent reports no row for.
	unreported bool
}

type fakeAgentRuntimeState struct {
	cfg         fakeAgentRuntimeSpec
	state       string
	pid         int
	inFlight    int // the benchmark's own requests
	direct      int
	adminState  string
	pinned      bool
	backoffEnds time.Time
}

func (st *fakeAgentRuntimeState) hasProcess() bool {
	return st.pid > 0 || vramStatesWithProcess[st.state] || !vramStateNoProcess(st.state)
}

// fakeAgentRuntime is a fake agent for the stop-all tests: the agent router's
// HTTP surface the benchmark reads (GET /running, streamed chat completions),
// a Manager model per spec (state, pid, in-flight requests, applied admin_state
// and pinned), and the agent's two links to the gateway:
//   - it applies every runtime_config frame the gateway enqueues on its one
//     registered stream connection, in order, and records each as a "doc"
//     and a "pins" event;
//   - it publishes a runtime-status frame into srv.RuntimeStatus on every
//     state change, and one per tick while somebody subscribes. Like the real
//     agent, it publishes nothing when a request ends.
//
// A chat for a model whose spec carries force_stopped gets 503
// runtime.admission_blocked. A chat for a spec without a process waits for a
// backoff to end, evicts every other running child first under a closed
// co-residency matrix, and then loads for the spec's load time.
type fakeAgentRuntime struct {
	t            *testing.T
	srv          *Server
	serverID     string
	events       *stopAllEvents
	ts           *httptest.Server
	closedMatrix bool
	// clearDelay is how long the fake waits before it applies a document
	// that lifts a force_stopped it holds, so a cold pass meets the 503 first.
	clearDelay time.Duration
	// dropClears: the fake never applies a document that lifts a
	// force_stopped (an agent the clear does not reach).
	dropClears bool
	// stopDelay is how long the fake waits before it applies any other
	// document, such as a stop (a slow delivery).
	stopDelay time.Duration
	// streamTime is how long a served chat streams between its first and its
	// last chunk.
	streamTime time.Duration

	mu      sync.Mutex
	specs   []*fakeAgentRuntimeState
	nextPID int
	frozen  bool
	served  map[string]int
	changed chan struct{}
	timers  []*time.Timer
	onApply func(doc portal.AgentRuntimeConfigDTO)
	onEnd   func(model string, served int)

	done chan struct{}
	wg   sync.WaitGroup
}

const fakeAgentRuntimeTick = 20 * time.Millisecond

func newFakeAgentRuntime(t *testing.T, srv *Server, serverID string, events *stopAllEvents, specs []fakeAgentRuntimeSpec) *fakeAgentRuntime {
	t.Helper()
	f := &fakeAgentRuntime{
		t: t, srv: srv, serverID: serverID, events: events,
		nextPID: 1000, served: map[string]int{}, changed: make(chan struct{}), done: make(chan struct{}),
	}
	for _, cfg := range specs {
		f.specs = append(f.specs, &fakeAgentRuntimeState{
			cfg: cfg, state: cfg.state, pid: cfg.pid, direct: cfg.direct, adminState: cfg.adminState, pinned: cfg.pinned,
			backoffEnds: time.Now().Add(cfg.backoff),
		})
	}
	f.ts = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.close)
	return f
}

// connect registers the fake's stream connection on srv.AgentStreams and
// starts the reader that applies every runtime_config frame, and the ticker.
func (f *fakeAgentRuntime) connect() {
	conn := &agentStreamConn{out: make(chan []byte, agentStreamQueueCapacity)}
	f.srv.AgentStreams.add(f.serverID, conn)
	f.wg.Add(2)
	go f.read(conn)
	go f.tickLoop()
	f.mu.Lock()
	f.publishLocked()
	f.mu.Unlock()
}

func (f *fakeAgentRuntime) close() {
	f.ts.Close()
	close(f.done)
	f.wg.Wait()
	f.mu.Lock()
	for _, tm := range f.timers {
		tm.Stop()
	}
	f.mu.Unlock()
}

func (f *fakeAgentRuntime) read(conn *agentStreamConn) {
	defer f.wg.Done()
	for {
		select {
		case <-f.done:
			return
		case raw := <-conn.out:
			var frame streamFrame
			if err := json.Unmarshal(raw, &frame); err != nil || frame.Type != "runtime_config" {
				continue
			}
			var doc portal.AgentRuntimeConfigDTO
			if err := json.Unmarshal(frame.Data, &doc); err != nil {
				continue
			}
			delay := f.stopDelay
			if f.liftsAStop(doc) {
				if f.dropClears {
					continue
				}
				delay = f.clearDelay
			}
			select {
			case <-f.done:
				return
			case <-time.After(delay):
			}
			f.apply(doc)
		}
	}
}

func (f *fakeAgentRuntime) tickLoop() {
	defer f.wg.Done()
	tick := time.NewTicker(fakeAgentRuntimeTick)
	defer tick.Stop()
	for {
		select {
		case <-f.done:
			return
		case <-tick.C:
			if runtimeSubscriberCount(f.srv.RuntimeStatus, f.serverID) == 0 {
				continue
			}
			f.mu.Lock()
			f.publishLocked()
			f.mu.Unlock()
		}
	}
}

// liftsAStop reports whether doc clears a force_stopped the fake holds.
func (f *fakeAgentRuntime) liftsAStop(doc portal.AgentRuntimeConfigDTO) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, d := range doc.Specs {
		if st := f.specLocked(d.ID); st != nil && st.adminState == vramAdminStateForceStopped && d.AdminState != vramAdminStateForceStopped {
			return true
		}
	}
	return false
}

// apply is the Manager's Apply of one document: a force_stopped spec's child
// drains, and a pinned spec without a process and without an override starts
// at once, as a re-pin's Apply starts a stopped pinned child. It records the
// document twice in the event log: as "doc [ids]", the spec ids it
// force-stops, and as "pins [ids]", the spec ids it pins.
func (f *fakeAgentRuntime) apply(doc portal.AgentRuntimeConfigDTO) {
	f.mu.Lock()
	var stopped, pinned []string
	for _, d := range doc.Specs {
		if d.AdminState == vramAdminStateForceStopped {
			stopped = append(stopped, d.ID)
		}
		if d.Pinned {
			pinned = append(pinned, d.ID)
		}
		st := f.specLocked(d.ID)
		if st == nil {
			continue
		}
		st.pinned = d.Pinned
		if d.AdminState != st.adminState {
			st.adminState = d.AdminState
			if d.AdminState == vramAdminStateForceStopped {
				f.stopLocked(st)
			}
		}
		if st.pinned && st.adminState == "" && st.state == agentSpecStateStopped && st.pid == 0 {
			f.startPinnedLocked(st)
		}
	}
	slices.Sort(stopped)
	slices.Sort(pinned)
	f.events.add("doc %v", stopped)
	f.events.add("pins %v", pinned)
	f.publishLocked()
	f.signalLocked()
	hook := f.onApply
	f.mu.Unlock()
	if hook != nil {
		hook(doc)
	}
}

// startPinnedLocked starts st's child without a request, as the Manager starts
// a pinned spec: starting with a pid at once, running after its load time.
func (f *fakeAgentRuntime) startPinnedLocked(st *fakeAgentRuntimeState) {
	st.state, st.pid = "starting", f.nextPID
	f.nextPID++
	f.timers = append(f.timers, time.AfterFunc(st.cfg.load, func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		if st.state != "starting" {
			return
		}
		st.state = "running"
		f.publishLocked()
		f.signalLocked()
	}))
}

// stopLocked drains st's child, as a force_stopped document does: a child
// with a process exits after its exit time, and a spec without one (backoff,
// start_failed with pid 0, …) turns stopped at once.
func (f *fakeAgentRuntime) stopLocked(st *fakeAgentRuntimeState) {
	if !st.hasProcess() {
		st.state = agentSpecStateStopped
		return
	}
	st.state = "draining"
	if st.cfg.stuck {
		return
	}
	f.timers = append(f.timers, time.AfterFunc(st.cfg.exit, func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		if st.state != "draining" {
			return
		}
		st.state, st.pid, st.inFlight = agentSpecStateStopped, 0, 0
		if st.cfg.exitTo != "" {
			st.state = st.cfg.exitTo
			st.backoffEnds = time.Now().Add(st.cfg.backoff)
		}
		f.publishLocked()
		f.signalLocked()
	}))
}

func (f *fakeAgentRuntime) specLocked(id string) *fakeAgentRuntimeState {
	for _, st := range f.specs {
		if st.cfg.id == id {
			return st
		}
	}
	return nil
}

func (f *fakeAgentRuntime) specByModel(model string) *fakeAgentRuntimeState {
	for _, st := range f.specs {
		if st.cfg.model == model {
			return st
		}
	}
	return nil
}

// signalLocked wakes every chat that waits for a state change.
func (f *fakeAgentRuntime) signalLocked() {
	close(f.changed)
	f.changed = make(chan struct{})
}

func (f *fakeAgentRuntime) publishLocked() {
	if f.frozen {
		return
	}
	rows := make([]RuntimeStatusDTO, 0, len(f.specs))
	for _, st := range f.specs {
		if st.cfg.unreported {
			continue
		}
		rows = append(rows, RuntimeStatusDTO{SpecID: st.cfg.id, Model: st.cfg.model, State: st.state, PID: st.pid, InFlight: st.inFlight + st.direct})
	}
	f.srv.RuntimeStatus.publish(f.serverID, rows)
}

// setState puts a spec into state with pid, as a child that a direct client
// started or that crashed, and publishes the change.
func (f *fakeAgentRuntime) setState(id, state string, pid int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	st := f.specLocked(id)
	st.state, st.pid = state, pid
	f.publishLocked()
	f.signalLocked()
}

// setDirect sets the direct client's traffic on a spec; no frame is
// published, as none is when a request ends.
func (f *fakeAgentRuntime) setDirect(id string, n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.specLocked(id).direct = n
}

// freeze stops every frame, transitions and ticks alike, while on holds.
func (f *fakeAgentRuntime) freeze(on bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.frozen = on
}

// stateOf is the state and pid the fake holds for a spec.
func (f *fakeAgentRuntime) stateOf(id string) (string, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	st := f.specLocked(id)
	return st.state, st.pid
}

func (f *fakeAgentRuntime) serve(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/running":
		f.serveRunning(w)
	case r.Method == http.MethodPost && r.URL.Path == "/v1/chat/completions":
		f.serveChat(w, r)
	default:
		f.events.add("other %s %s", r.Method, r.URL.Path)
		fakeAgentError(w, http.StatusNotFound, "runtime.model_not_managed", "request body does not name a managed model")
	}
}

func (f *fakeAgentRuntime) serveRunning(w http.ResponseWriter) {
	f.mu.Lock()
	type entry struct {
		Model string `json:"model"`
		State string `json:"state"`
	}
	entries := []entry{}
	for _, st := range f.specs {
		if st.state == "running" {
			entries = append(entries, entry{Model: st.cfg.model, State: "ready"})
		}
	}
	f.mu.Unlock()
	fakeAgentJSON(w, http.StatusOK, map[string]any{"running": entries})
}

func (f *fakeAgentRuntime) serveChat(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var peek struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(body, &peek)
	f.mu.Lock()
	st := f.specByModel(peek.Model)
	f.mu.Unlock()
	if st == nil {
		fakeAgentError(w, http.StatusNotFound, "runtime.model_not_managed", "request body does not name a managed model")
		return
	}
	if !f.admit(r.Context(), st) {
		f.events.add("chat %s 503", peek.Model)
		fakeAgentError(w, http.StatusServiceUnavailable, "runtime.admission_blocked", "admission blocked: the spec is force-stopped")
		return
	}
	f.events.add("chat %s 200", peek.Model)
	flusher, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	for i, frame := range []string{
		`{"choices":[{"delta":{"content":"ok"}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":12,"completion_tokens":20,"total_tokens":32},"timings":{"prompt_n":12,"prompt_per_second":900,"predicted_n":20,"predicted_per_second":42}}`,
		`[DONE]`,
	} {
		if i == 1 && f.streamTime > 0 {
			select {
			case <-r.Context().Done():
			case <-time.After(f.streamTime):
			}
		}
		_, _ = fmt.Fprintf(w, "data: %s\n\n", frame)
		if flusher != nil {
			flusher.Flush()
		}
	}
	f.mu.Lock()
	st.inFlight--
	f.served[peek.Model]++
	n, hook := f.served[peek.Model], f.onEnd
	f.mu.Unlock()
	if hook != nil {
		hook(peek.Model, n)
	}
}

// admit holds a chat until st's child runs and counts it in flight, or
// reports false for a force-stopped spec.
func (f *fakeAgentRuntime) admit(ctx context.Context, st *fakeAgentRuntimeState) bool {
	for {
		f.mu.Lock()
		if st.adminState == vramAdminStateForceStopped {
			f.mu.Unlock()
			return false
		}
		if st.state == "running" {
			st.inFlight++
			f.mu.Unlock()
			return true
		}
		wait, startNow := f.blockerLocked(st)
		if startNow {
			st.state, st.pid = "starting", f.nextPID
			f.nextPID++
			st.inFlight++
			f.publishLocked()
			f.signalLocked()
		}
		changed := f.changed
		f.mu.Unlock()
		if startNow {
			return f.load(ctx, st)
		}
		select {
		case <-ctx.Done():
			return false
		case <-changed:
		case <-time.After(wait):
		}
	}
}

// blockerLocked says what st's cold start waits for: a backoff timer that has
// not fired, a child of its own that is starting or draining, or, under a
// closed matrix, every other child with a process, which it evicts. When
// nothing blocks it, startNow is true.
func (f *fakeAgentRuntime) blockerLocked(st *fakeAgentRuntimeState) (wait time.Duration, startNow bool) {
	if st.state == "backoff" {
		if left := time.Until(st.backoffEnds); left > 0 {
			return left, false
		}
		st.state = agentSpecStateStopped
	}
	if st.hasProcess() {
		return time.Second, false
	}
	if f.closedMatrix {
		blocked := false
		for _, other := range f.specs {
			if other != st && other.hasProcess() {
				blocked = true
				if other.state == "running" {
					f.stopLocked(other)
					f.publishLocked()
				}
			}
		}
		if blocked {
			return time.Second, false
		}
	}
	return 0, true
}

func (f *fakeAgentRuntime) load(ctx context.Context, st *fakeAgentRuntimeState) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(st.cfg.load):
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if st.state != "starting" {
		st.inFlight--
		return false
	}
	st.state = "running"
	f.publishLocked()
	f.signalLocked()
	return true
}

// --- the portal and store doubles --------------------------------------------

// stopAllPortal is the real portal.Service behind the fixture, with the
// benchmark's batched admin-state and pinned writers and the lease methods
// wrapped: each call is recorded in the event log before it reaches the
// service, and a test can make a spec of a batch fail or conflict, or make a
// lease write or read fail. Like a real store, each wrapped call fails with
// ctx.Err() on a done context without reaching the service: a batch then
// reports every spec as Failed. The pinned writer's wrapper,
// SetBenchmarkRuntimeSpecsPinned, sits in benchmark_unpin_test.go.
type stopAllPortal struct {
	portal.API
	events   *stopAllEvents
	notifies func() int

	mu sync.Mutex
	// failClear makes a clear batch report the spec as Failed with this
	// error, and leaves its force_stopped in place.
	failClear map[string]error
	// failStopOnce makes the next stop batch that names the spec report it as
	// Failed with this error, without writing it.
	failStopOnce map[string]error
	// conflictStop makes a stop batch report the spec as a Conflict and write
	// nothing for it; conflictClear does the same for a clear batch.
	conflictStop  map[string]bool
	conflictClear map[string]bool
	// failLease, when it returns an error for the n-th lease write (1-based),
	// makes that write fail without storing anything.
	failLease func(n int) error
	// failLeaseRead makes BenchmarkOverrideLeases fail.
	failLeaseRead error
	// onBatch runs before every batched admin-state write reaches the service.
	onBatch     func(kind string, ids []string)
	leaseWrites int
	// batchNotifies is the number of notifications each batch sent, in call
	// order.
	batchNotifies []int
	// failUnpin makes an unpin batch report the spec as Failed with this
	// error, and write nothing for it.
	failUnpin map[string]error
	// failRepin makes the spec's next re-pin batches report it as Failed and
	// write nothing for it: how many still fail, -1 for every one.
	failRepin map[string]int
	// onPinBatch runs before every batched pinned write reaches the service.
	onPinBatch func(kind string, ids []string)
	// pinNotifies is the number of notifications each pinned batch sent, in
	// call order.
	pinNotifies []int
}

func (p *stopAllPortal) SetBenchmarkRuntimeSpecsAdminState(ctx context.Context, specIDs []string, expectedAdminState, adminState string) (portal.BenchmarkSpecsOutcome, error) {
	kind := "stop"
	if expectedAdminState == vramAdminStateForceStopped {
		kind = "clear"
	}
	ids := append([]string(nil), specIDs...)
	slices.Sort(ids)
	if err := ctx.Err(); err != nil {
		p.events.add("%s! %v", kind, ids)
		out := portal.BenchmarkSpecsOutcome{Failed: specIDs, Errs: map[string]error{}}
		for _, id := range specIDs {
			out.Errs[id] = err
		}
		return out, err
	}
	p.events.add("%s %v", kind, ids)
	p.mu.Lock()
	hook := p.onBatch
	p.mu.Unlock()
	if hook != nil {
		hook(kind, ids)
	}
	p.mu.Lock()
	var pass []string
	injected := map[string]error{}
	for _, id := range specIDs {
		switch {
		case kind == "clear" && p.failClear[id] != nil:
			injected[id] = p.failClear[id]
		case kind == "stop" && p.failStopOnce[id] != nil:
			injected[id] = p.failStopOnce[id]
			delete(p.failStopOnce, id)
		case (kind == "stop" && p.conflictStop[id]) || (kind == "clear" && p.conflictClear[id]):
			injected[id] = portal.ErrRuntimeSpecAdminStateConflict
		default:
			pass = append(pass, id)
		}
	}
	p.mu.Unlock()
	before := p.notifies()
	out := portal.BenchmarkSpecsOutcome{}
	var err error
	if len(pass) > 0 {
		out, err = p.API.SetBenchmarkRuntimeSpecsAdminState(ctx, pass, expectedAdminState, adminState)
	}
	if out.Errs == nil {
		out.Errs = map[string]error{}
	}
	for _, id := range specIDs {
		ierr, ok := injected[id]
		if !ok {
			continue
		}
		out.Errs[id] = ierr
		if errors.Is(ierr, portal.ErrRuntimeSpecAdminStateConflict) {
			out.Conflict = append(out.Conflict, id)
			continue
		}
		out.Failed = append(out.Failed, id)
		if err == nil {
			err = ierr
		}
	}
	p.mu.Lock()
	p.batchNotifies = append(p.batchNotifies, p.notifies()-before)
	p.mu.Unlock()
	return out, err
}

func (p *stopAllPortal) SetBenchmarkOverrideLease(ctx context.Context, serverID string, lease portal.BenchmarkOverrideLease) error {
	if err := ctx.Err(); err != nil {
		p.events.add("lease! clear=%v repin=%v", lease.ClearForceStopped, lease.Repin)
		return err
	}
	p.mu.Lock()
	p.leaseWrites++
	n, fail := p.leaseWrites, p.failLease
	p.mu.Unlock()
	if fail != nil {
		if err := fail(n); err != nil {
			p.events.add("lease! clear=%v repin=%v", lease.ClearForceStopped, lease.Repin)
			return err
		}
	}
	p.events.add("lease clear=%v repin=%v", lease.ClearForceStopped, lease.Repin)
	return p.API.SetBenchmarkOverrideLease(ctx, serverID, lease)
}

func (p *stopAllPortal) BenchmarkOverrideLeases(ctx context.Context) (map[string]portal.BenchmarkOverrideLease, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	p.mu.Lock()
	fail := p.failLeaseRead
	p.mu.Unlock()
	if fail != nil {
		return nil, fail
	}
	return p.API.BenchmarkOverrideLeases(ctx)
}

func (p *stopAllPortal) notifiesPerBatch() []int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]int(nil), p.batchNotifies...)
}

// stopAllRoutes is the gateway's view of the store. onSpecsRead can change
// what the n-th (1-based) RuntimeSpecsByApplication returns: the run reads it
// once at its start (beginBenchmarkUnpin), once more after the reconcile of a
// leftover lease, and once per stop-all (benchmarkStopRefusal). The portal
// reads the store directly, so the documents it derives stay the store's.
type stopAllRoutes struct {
	routing.Store
	mu          sync.Mutex
	reads       int
	onSpecsRead func(n int, specs []routing.RuntimeSpec) ([]routing.RuntimeSpec, error)
}

func (r *stopAllRoutes) RuntimeSpecsByApplication(ctx context.Context, appID string) ([]routing.RuntimeSpec, error) {
	specs, err := r.Store.RuntimeSpecsByApplication(ctx, appID)
	r.mu.Lock()
	r.reads++
	n, hook := r.reads, r.onSpecsRead
	r.mu.Unlock()
	if err != nil || hook == nil {
		return specs, err
	}
	return hook(n, specs)
}

// stopAllPortalRoutes is the portal's view of the store: failGPUsOnce makes
// the next SetRuntimeSpecGPUs for a spec fail, after the spec row itself was
// stored.
type stopAllPortalRoutes struct {
	routing.Store
	mu           sync.Mutex
	failGPUsOnce map[string]bool
}

func (r *stopAllPortalRoutes) SetRuntimeSpecGPUs(ctx context.Context, specID string, gpus []routing.RuntimeSpecGPU) error {
	r.mu.Lock()
	fail := r.failGPUsOnce[specID]
	delete(r.failGPUsOnce, specID)
	r.mu.Unlock()
	if fail {
		return errors.New("store: GPU rows not written")
	}
	return r.Store.SetRuntimeSpecGPUs(ctx, specID, gpus)
}

// --- the fixture ------------------------------------------------------------

// stopAllSpec is one mapping of the fixture's server_agent application and
// its launch spec: mapping <mappingID>, spec rs_<mappingID>, upstream model
// <model>. Its stored metrics start at 50 / 900 / 1234 (gen / prompt / load).
type stopAllSpec struct {
	fakeAgentRuntimeSpec
	mappingID string
	disabled  bool
}

type stopAllOpts struct {
	specs        []stopAllSpec
	closedMatrix bool
	clearDelay   time.Duration
	dropClears   bool
	stopDelay    time.Duration
	streamTime   time.Duration
	// lease is a leftover override lease for srv1 at the start.
	lease *portal.BenchmarkOverrideLease
}

type stopAllFixture struct {
	srv          *Server
	mem          *routing.MemoryStore
	agent        *fakeAgentRuntime
	portal       *stopAllPortal
	svc          *portal.Service
	routes       *stopAllRoutes
	portalRoutes *stopAllPortalRoutes
	events       *stopAllEvents
	server       routing.AIServer
	app          routing.Application

	cancelMu  sync.Mutex
	cancelCtx context.CancelFunc // the context of the run f.run is running
}

// shrinkStopAllTimings drives the stop-all's bounds down to milliseconds. They
// are set before the run starts and restored only after the test, when the run
// has finished.
func shrinkStopAllTimings(t *testing.T) {
	t.Helper()
	oldWait, oldSelect, oldGap, oldRestore := benchmarkStopWaitBound, benchmarkTelemetryContextWait, coldLoadPollGap, vramRestoreTimeout
	benchmarkStopWaitBound = 2 * time.Second
	benchmarkTelemetryContextWait = 400 * time.Millisecond
	coldLoadPollGap = 20 * time.Millisecond
	vramRestoreTimeout = 2 * time.Second
	t.Cleanup(func() {
		benchmarkStopWaitBound, benchmarkTelemetryContextWait, coldLoadPollGap, vramRestoreTimeout = oldWait, oldSelect, oldGap, oldRestore
	})
}

func newStopAllFixture(t *testing.T, opts stopAllOpts) *stopAllFixture {
	t.Helper()
	shrinkStopAllTimings(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	must := func(what string, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}
	events := &stopAllEvents{}
	mem := routing.NewMemoryStore()
	gwRoutes := &stopAllRoutes{Store: mem}
	portalRoutes := &stopAllPortalRoutes{Store: mem, failGPUsOnce: map[string]bool{}}
	srv := &Server{
		Provider:      newFakeAgentProvider(),
		Routes:        gwRoutes,
		Benchmarks:    NewBenchmarkRegistry(),
		RuntimeStatus: NewRuntimeStatusRegistry(),
		AgentFeatures: NewAgentFeaturesRegistry(),
		AgentStreams:  NewAgentStreamRegistry(),
	}
	var agentSpecs []fakeAgentRuntimeSpec
	for _, s := range opts.specs {
		agentSpecs = append(agentSpecs, s.fakeAgentRuntimeSpec)
	}
	agent := newFakeAgentRuntime(t, srv, "srv1", events, agentSpecs)
	agent.closedMatrix, agent.clearDelay, agent.dropClears = opts.closedMatrix, opts.clearDelay, opts.dropClears
	agent.stopDelay, agent.streamTime = opts.stopDelay, opts.streamTime

	server := routing.AIServer{ID: "srv1", Name: "Host", Provider: routing.ProviderMock, Endpoint: "mock://srv1", Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now}
	app := routing.Application{ID: "app1", ServerID: "srv1", Type: routing.ProviderServerAgent, APIFlavors: []string{routing.APIFlavorOpenAI}, TimeoutMS: 30000, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}
	fakeAgentPoint(t, agent.ts.URL, &server, &app)
	must("CreateAIServer", mem.CreateAIServer(ctx, server))
	must("CreateApplication", mem.CreateApplication(ctx, app))
	for _, s := range opts.specs {
		must("CreateMapping("+s.mappingID+")", mem.CreateMapping(ctx, routing.ModelMapping{
			ID: s.mappingID, ApplicationID: app.ID, GatewayModelName: "gw-" + s.model, AppModelName: s.model, Status: routing.ServerStatusActive,
			GenTokensPerSecond: 50, PromptTokensPerSecond: 900, LoadTimeMS: 1234, CreatedAt: now, UpdatedAt: now,
		}))
		must("UpsertRuntimeSpec("+s.id+")", mem.UpsertRuntimeSpec(ctx, routing.RuntimeSpec{
			ID: s.id, MappingID: s.mappingID, Enabled: !s.disabled, Type: string(routing.RuntimeSpecTypeLlamaCpp),
			Binary: "/usr/local/bin/llama-server", Args: "[]", Env: "{}", HealthPath: "/health",
			HealthTimeoutSeconds: 5, StartupTimeoutSeconds: 180, Pinned: s.pinned, AdminState: s.adminState,
			APIFlavors: []string{routing.APIFlavorOpenAI}, CreatedAt: now, UpdatedAt: now,
		}))
	}

	svc := portal.NewService(portal.ServiceDeps{Routes: portalRoutes, SystemSettings: portal.NewMemorySystemSettings(), Clock: func() time.Time { return now }})
	if opts.lease != nil {
		must("SetBenchmarkOverrideLease", svc.SetBenchmarkOverrideLease(ctx, "srv1", *opts.lease))
	}
	var notifyMu sync.Mutex
	notified := 0
	svc.SetRuntimeConfigChangedHook(func(serverID string) {
		notifyMu.Lock()
		notified++
		notifyMu.Unlock()
		events.add("notify")
		srv.PushRuntimeConfig(serverID)
	})
	double := &stopAllPortal{API: svc, events: events, notifies: func() int {
		notifyMu.Lock()
		defer notifyMu.Unlock()
		return notified
	}, failClear: map[string]error{}, failStopOnce: map[string]error{}, conflictStop: map[string]bool{}, conflictClear: map[string]bool{}, failUnpin: map[string]error{}, failRepin: map[string]int{}}
	srv.Portal = double
	srv.AgentFeatures.Set("srv1", []string{"runtime_manager"})
	agent.connect()
	return &stopAllFixture{
		srv: srv, mem: mem, agent: agent, portal: double, svc: svc, routes: gwRoutes, portalRoutes: portalRoutes,
		events: events, server: server, app: app,
	}
}

// fakeAgentPoint aims server and app at an httptest URL: its host, port and
// scheme.
func fakeAgentPoint(t *testing.T, rawURL string, server *routing.AIServer, app *routing.Application) {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parse %q: %v", rawURL, err)
	}
	host, portText, err := net.SplitHostPort(u.Host)
	if err != nil {
		t.Fatalf("split %q: %v", u.Host, err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatalf("port %q: %v", portText, err)
	}
	server.Domain, app.Port, app.Scheme = host, port, u.Scheme
}

// targets builds the run's targets for mappingIDs, in order, from the store,
// the way startBenchmark's partition does, and marks them for mode.
func (f *stopAllFixture) targets(t *testing.T, mode string, mappingIDs ...string) []benchmarkTarget {
	t.Helper()
	ctx := context.Background()
	var out []benchmarkTarget
	for _, id := range mappingIDs {
		mapping, err := f.mem.MappingByID(ctx, id)
		if err != nil {
			t.Fatalf("MappingByID(%s): %v", id, err)
		}
		spec, hasSpec, err := f.mem.RuntimeSpecByMapping(ctx, id)
		if err != nil {
			t.Fatalf("RuntimeSpecByMapping(%s): %v", id, err)
		}
		out = append(out, f.srv.benchmarkTargetFor(ctx, f.server, f.app, mapping, spec, hasSpec))
	}
	benchmarkMayPreStop(out, mode)
	return out
}

// run runs one benchmark over targets the way startBenchmark does, under a
// context with a 30 s deadline that cancelRun cancels, and returns the run's
// final status.
func (f *stopAllFixture) run(t *testing.T, mode string, targets []benchmarkTarget) BenchmarkStatus {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	f.cancelMu.Lock()
	f.cancelCtx = cancel
	f.cancelMu.Unlock()
	run, ok := f.srv.Benchmarks.TryStart("srv1", "owner", mode, len(targets), time.Now().UTC(), cancel)
	if !ok {
		t.Fatal("TryStart did not start")
	}
	f.srv.runBenchmark(ctx, run, "srv1", targets, mode)
	return f.srv.Benchmarks.Status("srv1")
}

// cancelRun cancels the context of the run f.run is running.
func (f *stopAllFixture) cancelRun() {
	f.cancelMu.Lock()
	defer f.cancelMu.Unlock()
	if f.cancelCtx != nil {
		f.cancelCtx()
	}
}

// speedRow is the mapping's one speed-history row.
func (f *stopAllFixture) speedRow(t *testing.T, mappingID string) routing.BenchmarkRun {
	t.Helper()
	rows, err := f.mem.BenchmarkRunsByMapping(context.Background(), mappingID, 10)
	if err != nil {
		t.Fatalf("BenchmarkRunsByMapping(%s): %v", mappingID, err)
	}
	var speed []routing.BenchmarkRun
	for _, r := range rows {
		if r.Kind == "" || r.Kind == "speed" {
			speed = append(speed, r)
		}
	}
	if len(speed) != 1 {
		t.Fatalf("speed history rows for %s = %d, want 1", mappingID, len(speed))
	}
	return speed[0]
}

// lease is srv1's override lease as the store holds it.
func (f *stopAllFixture) lease(t *testing.T) portal.BenchmarkOverrideLease {
	t.Helper()
	leases, err := f.svc.BenchmarkOverrideLeases(context.Background())
	if err != nil {
		t.Fatalf("BenchmarkOverrideLeases: %v", err)
	}
	return leases["srv1"]
}

// adminState is the spec's stored admin_state.
func (f *stopAllFixture) adminState(t *testing.T, specID string) string {
	t.Helper()
	spec, ok, err := f.mem.RuntimeSpecByID(context.Background(), specID)
	if err != nil || !ok {
		t.Fatalf("RuntimeSpecByID(%s) = %v, %v", specID, ok, err)
	}
	return spec.AdminState
}

// batches is every batched admin-state write, in call order.
func (f *stopAllFixture) batches() []string {
	return f.events.with("stop ", "clear ")
}

// docs is every document the fake agent applied, as the spec ids it
// force-stops, in order.
func (f *stopAllFixture) docs() []string {
	return f.events.with("doc ")
}

// within reports whether got is within pct percent of want.
func within(got int, want time.Duration, pct int) bool {
	w := int(want.Milliseconds())
	return got*100 >= w*(100-pct) && got*100 <= w*(100+pct)
}
