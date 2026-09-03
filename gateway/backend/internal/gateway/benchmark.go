// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"sync"
	"time"
)

// BenchmarkStatus is a snapshot of a per-server benchmark run. Its zero value
// (Running:false) means "no run known for this server".
type BenchmarkStatus struct {
	Running   bool      `json:"running"`
	ServerID  string    `json:"server_id"`
	Scope     string    `json:"scope"`
	Mode      string    `json:"mode,omitempty"`
	StartedAt time.Time `json:"started_at,omitempty"`
	Total     int       `json:"total"`
	Done      int       `json:"done"`
	// CurrentConcurrency is the concurrency level a capacity ramp is currently
	// probing (live-only; 0 when not ramping). Cleared when a mapping's result lands.
	CurrentConcurrency int               `json:"current_concurrency,omitempty"`
	Error              string            `json:"error,omitempty"`
	Results            []BenchmarkResult `json:"results,omitempty"`
}

// BenchmarkResult is one model mapping's measured metrics from a benchmark run.
type BenchmarkResult struct {
	MappingID             string  `json:"mapping_id"`
	GatewayModelName      string  `json:"gateway_model_name"`
	GenTokensPerSecond    float64 `json:"gen_tokens_per_second"`
	PromptTokensPerSecond float64 `json:"prompt_tokens_per_second"`
	LoadTimeMS            int     `json:"load_time_ms"`
	ContextSize           int     `json:"context_size,omitempty"`
	// Loaded is true when a load action confirmed the model resident (report-only; a load run).
	Loaded bool `json:"loaded,omitempty"`
	// Capacity metrics (populated on a capacity/both run; omitted on a speed run).
	MaxConcurrency               int     `json:"max_concurrency,omitempty"`
	RecommendedConcurrency       int     `json:"recommended_concurrency,omitempty"`
	GenTokensPerSecondAtCapacity float64 `json:"gen_tokens_per_second_at_capacity,omitempty"`
	// VisionCapable is the vision benchmark's verdict: nil = not run or
	// inconclusive (nothing persisted); non-nil true/false = a definitive result.
	VisionCapable *bool `json:"vision_capable,omitempty"`
	// VRAM is the VRAM benchmark's result: nil = the run never reached the
	// measurement phase (refused before it started, isolation refused, or a
	// hard error -- see Error). Non-nil with Inconclusive set = it ran and
	// reached no number, and WHY is the operator's next action. The nested
	// shape mirrors routing.CapacityReport; what is deliberately NOT copied is
	// VisionCapable's nil-means-both contract above, because "no result" and
	// "no result because the model was already being served by something we
	// could not stop" send an operator to two different places. See
	// benchmark_vram.go for the report itself.
	//
	// Held by POINTER, and a report attached to a result is IMMUTABLE from
	// that moment: benchmarkRun.snapshot copies the Results slice but not what
	// its pointers reference, so a mutation after addResult would be visible
	// to every already-published SSE frame (VisionCapable has the same
	// property, and the same rule).
	VRAM  *VRAMReport `json:"vram,omitempty"`
	Error string      `json:"error,omitempty"`
}

type benchmarkRun struct {
	mu     sync.Mutex
	status BenchmarkStatus
	cancel func()
}

// benchmarkSubBuffer is the per-subscriber channel buffer for live-progress SSE.
// A slow reader simply drops frames once the buffer fills; it recovers on its next
// snapshot (each frame is a full status, not an incremental delta).
const benchmarkSubBuffer = 64

// BenchmarkRegistry tracks at most one in-flight benchmark run per server. It is
// volatile (in-memory) and every method is nil-safe so a nil registry is a valid
// "feature off" value.
//
// Lock ordering: r.mu is always acquired before a run's mu (TryStart holds r.mu
// while calling existing.snapshot(); ServerBusy/Status do the same). Never take
// r.mu while holding a run's mu.
type BenchmarkRegistry struct {
	mu   sync.Mutex
	runs map[string]*benchmarkRun                     // keyed by serverID
	subs map[string]map[chan BenchmarkStatus]struct{} // live-progress subscribers, keyed by serverID
}

// NewBenchmarkRegistry returns an empty registry.
func NewBenchmarkRegistry() *BenchmarkRegistry {
	return &BenchmarkRegistry{
		runs: map[string]*benchmarkRun{},
		subs: map[string]map[chan BenchmarkStatus]struct{}{},
	}
}

// TryStart atomically registers a running benchmark for serverID; returns
// (nil,false) if a run is already in flight for that server. mode is the
// benchmark mode ("speed"|"capacity"|"both"|"vision"), recorded on the run's status.
func (r *BenchmarkRegistry) TryStart(serverID, scope, mode string, total int, now time.Time, cancel func()) (*benchmarkRun, bool) {
	if r == nil {
		return nil, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if existing, ok := r.runs[serverID]; ok && existing.running() {
		return nil, false
	}
	run := &benchmarkRun{cancel: cancel, status: BenchmarkStatus{Running: true, ServerID: serverID, Scope: scope, Mode: mode, StartedAt: now, Total: total}}
	r.runs[serverID] = run
	return run, true
}

// ServerBusy reports whether serverID has a running benchmark (routing exclusion).
func (r *BenchmarkRegistry) ServerBusy(serverID string) bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	run, ok := r.runs[serverID]
	return ok && run.running()
}

// Release removes a server's run entry entirely — for undoing a reservation whose
// pre-run idle-gate failed, so no zombie status lingers and ServerBusy is false.
// Unlike finish (which keeps a terminal, errored status for a run that executed),
// Release forgets the run completely. A nil registry is a no-op.
func (r *BenchmarkRegistry) Release(serverID string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	delete(r.runs, serverID)
	r.mu.Unlock()
}

// Status returns a copy of the server's latest run status (zero value if none).
func (r *BenchmarkRegistry) Status(serverID string) BenchmarkStatus {
	if r == nil {
		return BenchmarkStatus{}
	}
	r.mu.Lock()
	run, ok := r.runs[serverID]
	r.mu.Unlock()
	if !ok {
		return BenchmarkStatus{}
	}
	return run.snapshot()
}

// ActiveRuns returns a snapshot of every currently-running benchmark (one per server).
// Finished/absent runs are excluded. Mirrors Status's locking: collect the run pointers
// under r.mu, release r.mu, then snapshot each (which takes run.mu) — never both locks at once.
func (r *BenchmarkRegistry) ActiveRuns() []BenchmarkStatus {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	runs := make([]*benchmarkRun, 0, len(r.runs))
	for _, run := range r.runs {
		runs = append(runs, run)
	}
	r.mu.Unlock()
	out := make([]BenchmarkStatus, 0, len(runs))
	for _, run := range runs {
		if s := run.snapshot(); s.Running {
			out = append(out, s)
		}
	}
	return out
}

// running is a lightweight read of just the Running flag — used on the routing
// hot path (ServerBusy) to avoid snapshot's deep copy of Results. Takes only the
// run's own mutex, preserving the r.mu→run.mu lock ordering.
func (run *benchmarkRun) running() bool {
	run.mu.Lock()
	defer run.mu.Unlock()
	return run.status.Running
}

func (run *benchmarkRun) snapshot() BenchmarkStatus {
	run.mu.Lock()
	defer run.mu.Unlock()
	cp := run.status
	cp.Results = append([]BenchmarkResult(nil), run.status.Results...)
	return cp
}

func (run *benchmarkRun) setCurrentConcurrency(n int) {
	run.mu.Lock()
	run.status.CurrentConcurrency = n
	run.mu.Unlock()
}

func (run *benchmarkRun) addResult(res BenchmarkResult) {
	run.mu.Lock()
	run.status.Results = append(run.status.Results, res)
	run.status.Done = len(run.status.Results)
	run.status.CurrentConcurrency = 0
	run.mu.Unlock()
}

func (run *benchmarkRun) finish(errMsg string) {
	run.mu.Lock()
	run.status.Running = false
	run.status.Error = errMsg
	run.mu.Unlock()
}

// Subscribe registers a live-progress subscriber for serverID and returns the
// current status snapshot + a channel of subsequent status frames + an idempotent
// unsubscribe. Nil-safe (a nil registry yields a closed channel).
func (r *BenchmarkRegistry) Subscribe(serverID string) (BenchmarkStatus, <-chan BenchmarkStatus, func()) {
	if r == nil {
		ch := make(chan BenchmarkStatus)
		close(ch)
		return BenchmarkStatus{}, ch, func() { /* no-op: nil registry has no subscriber map to remove from */ }
	}
	ch := make(chan BenchmarkStatus, benchmarkSubBuffer)
	r.mu.Lock()
	run := r.runs[serverID] // grab the run pointer UNDER r.mu ...
	if r.subs == nil {
		r.subs = map[string]map[chan BenchmarkStatus]struct{}{}
	}
	if r.subs[serverID] == nil {
		r.subs[serverID] = map[chan BenchmarkStatus]struct{}{}
	}
	r.subs[serverID][ch] = struct{}{}
	r.mu.Unlock() // ... release r.mu BEFORE taking run.mu (preserve r.mu->run.mu order)
	snap := BenchmarkStatus{}
	if run != nil {
		snap = run.snapshot()
	}
	unsub := func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		if set, ok := r.subs[serverID]; ok {
			delete(set, ch)
			if len(set) == 0 {
				delete(r.subs, serverID)
			}
		}
	}
	return snap, ch, unsub
}

// publish fans a status frame out to serverID's subscribers (non-blocking; a slow
// reader drops frames and recovers on its snapshot). Called by the runner. The
// subscriber channels are snapshotted under r.mu, then delivered OUTSIDE it so a
// slow reader never blocks the publisher.
func (r *BenchmarkRegistry) publish(serverID string, status BenchmarkStatus) {
	if r == nil || serverID == "" {
		return
	}
	r.mu.Lock()
	targets := make([]chan BenchmarkStatus, 0, len(r.subs[serverID]))
	for ch := range r.subs[serverID] {
		targets = append(targets, ch)
	}
	r.mu.Unlock()
	for _, ch := range targets {
		select {
		case ch <- status:
		default:
		}
	}
}
