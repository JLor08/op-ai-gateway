// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"cmp"
	"context"
	"errors"
	"log/slog"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/provider"
	"slices"
	"strings"
	"sync"
	"time"
)

// The cluster watchdog follows a running llama.cpp RPC cluster on the agents'
// telemetry. Losing a member aborts its head, and a member is removed only
// through its head (head first), so every rule here acts on the head:
//
//   - Positive evidence that a formed cluster lost a member -- the member,
//     held by the head in the frame before, exited, changed its PID, no
//     longer reports the head's hold (the session closed, or another head
//     holds it), or its relay reports a stall -- is a suspicion first: a head
//     that stops by itself (idle, eviction, crash) ends its members' sessions
//     too, and the member's frame may arrive before the head's. Only a frame
//     of the head's server that arrived after the member's, and still shows
//     the head running with the same process, confirms it; any other head
//     frame drops it. A confirmed loss stops the head and releases the
//     members after it. The member is recorded as the cause of a cooldown
//     (runtime.cluster_member_lost), and a head that served requests then is
//     started again once the cooldown ended; a kept-up head is started again
//     by the reconciler anyway.
//   - Stale telemetry: a member server that has not reported for three of its
//     telemetry intervals, and at least staleFloor, counts as lost too, but
//     only on a fresh frame of the head's server that shows the head running,
//     never inside the gateway's boot grace, and never before the cluster
//     formed.
//   - A head that exited (its launch facts gone from its server's telemetry)
//     is no longer formed; its members are released and the reconciler is
//     woken. A member that still reports the hold of a direct head that is
//     not running holds a dead connection, so it is released again.
//   - A local start on a member's server that waits only for a held member
//     (admission_blocked_by) makes the gateway ask the head's agent for a
//     conditional stop: the head goes only when that agent's own eviction
//     rule lets it go, and the members are released after it; otherwise the
//     start waits. A head that is forming is left alone.
//   - A peer a member does not allow, or a hold by another head, is a breach:
//     logged and listed in the head's cluster status by member position. A
//     hold by another head is never this head's, so on a formed cluster it is
//     the lost session it means.
//
// The ingest only enqueues a frame (observeStatus); frames of one server
// coalesce while the watchdog is busy, and one worker goroutine at a time
// handles them. Every agent call runs in its own action goroutine. A stop of
// a head takes the head's stop marker, so it never runs beside a start or
// another stop of that head; a release of members only begins while neither
// runs, and a start that begins meanwhile waits for it before it ensures the
// members.

// codeClusterMemberLost: the watchdog stopped a formed cluster's head because
// a member was lost; the failure names the member's position.
const codeClusterMemberLost = "runtime.cluster_member_lost"

// codeRuntimeInstanceBusy is the agent's refusal of a conditional stop: its
// eviction rule does not let the head go right now.
const codeRuntimeInstanceBusy = "runtime.instance_busy"

// clusterStaleIntervals is how many telemetry intervals a member server may
// stay silent before the watchdog counts it as lost.
const clusterStaleIntervals = 3

// watchGapSamples is how many recent arrival gaps per server the watchdog
// keeps to estimate the server's telemetry interval (the largest of them).
const watchGapSamples = 8

// The kinds of evidence that a member was lost, as the watchdog logs them.
const (
	evidenceExited        = "member_exited"
	evidencePIDChanged    = "member_pid_changed"
	evidenceSessionClosed = "session_closed"
	evidenceStalled       = "relay_stalled"
	evidenceStale         = "telemetry_stale"
)

// clusterWatchdog is the orchestration's telemetry follower. See the top of
// this file.
type clusterWatchdog struct {
	o *clusterOrchestrator

	mu       sync.Mutex
	pending  map[string]*watchFrame
	queue    []string
	running  bool
	arrivals map[string]*watchArrivals
	seq      uint64

	// Owned by the one worker goroutine that runs at a time: when a member
	// release or a blocked admission was last acted on, by member key.
	released map[string]time.Time
	asked    map[string]time.Time
	suspects map[string]watchSuspicion // by head
}

// watchFrame is a server's frame pair not yet handled: the frame before the
// first one that arrived, and the latest one with its arrival sequence.
type watchFrame struct {
	prev, next []RuntimeStatusDTO
	seq        uint64
}

// watchSuspicion is a member loss a member frame showed, waiting for a head
// frame that arrived after it (seq): position and evidence of the member,
// and the head's process as the registry showed it then.
type watchSuspicion struct {
	position int
	evidence string
	seq      uint64
	headPID  int
}

// watchArrivals is when a server's frames arrived: the last one and the
// recent gaps between them.
type watchArrivals struct {
	last time.Time
	gaps [watchGapSamples]time.Duration
	n    int
}

func newClusterWatchdog(o *clusterOrchestrator) *clusterWatchdog {
	return &clusterWatchdog{
		o: o, pending: map[string]*watchFrame{}, arrivals: map[string]*watchArrivals{},
		released: map[string]time.Time{}, asked: map[string]time.Time{}, suspects: map[string]watchSuspicion{},
	}
}

// enqueue records the frame's arrival and queues it; while a frame of the
// server waits, a newer one replaces its latest frame. It starts the worker
// when none runs.
func (w *clusterWatchdog) enqueue(serverID string, prev, next []RuntimeStatusDTO) {
	now := w.o.deps.now()
	w.mu.Lock()
	a := w.arrivals[serverID]
	if a == nil {
		a = &watchArrivals{}
		w.arrivals[serverID] = a
	} else {
		a.gaps[a.n%watchGapSamples] = now.Sub(a.last)
		a.n++
	}
	a.last = now
	w.seq++
	if f := w.pending[serverID]; f != nil {
		f.next, f.seq = next, w.seq
	} else {
		w.pending[serverID] = &watchFrame{prev: prev, next: next, seq: w.seq}
		w.queue = append(w.queue, serverID)
	}
	start := !w.running
	w.running = true
	w.mu.Unlock()
	if !start {
		return
	}
	if w.o.deps.base().Err() != nil {
		w.mu.Lock()
		w.running = false
		w.mu.Unlock()
		return
	}
	w.o.wg.Add(1)
	go w.drain()
}

// drain is the worker: it handles queued frames until none is left.
func (w *clusterWatchdog) drain() {
	defer w.o.wg.Done()
	for {
		w.mu.Lock()
		if len(w.queue) == 0 {
			w.running = false
			w.mu.Unlock()
			return
		}
		id := w.queue[0]
		w.queue = w.queue[1:]
		f := w.pending[id]
		delete(w.pending, id)
		w.mu.Unlock()
		w.handle(id, f.prev, f.next, f.seq)
	}
}

// lastArrival is when serverID's last frame arrived; zero when none did since
// the gateway started.
func (w *clusterWatchdog) lastArrival(serverID string) time.Time {
	w.mu.Lock()
	defer w.mu.Unlock()
	if a := w.arrivals[serverID]; a != nil {
		return a.last
	}
	return time.Time{}
}

// silentSince reports whether serverID's telemetry has stayed away past the
// stale threshold -- three of its intervals, and at least staleFloor -- with
// the arrival of its last frame; a server with no frame since the gateway
// started is not judged.
func (w *clusterWatchdog) silentSince(serverID string) (time.Time, bool) {
	last := w.lastArrival(serverID)
	if last.IsZero() {
		return time.Time{}, false
	}
	limit := max(clusterStaleIntervals*w.interval(serverID), w.o.t.staleFloor)
	return last, w.o.deps.now().Sub(last) > limit
}

// interval is serverID's telemetry interval as the watchdog estimates it: the
// largest recent gap between its frames, at least minAgentInterval. A frame
// that arrived early (an agent reports a transition at once) shortens no gap
// that counts, and a late one makes the estimate more patient.
func (w *clusterWatchdog) interval(serverID string) time.Duration {
	w.mu.Lock()
	defer w.mu.Unlock()
	d := w.o.t.minAgentInterval
	if a := w.arrivals[serverID]; a != nil {
		for _, g := range a.gaps[:min(a.n, watchGapSamples)] {
			d = max(d, g)
		}
	}
	return d
}

// watchView is what the worker reads of one head the orchestration knows a
// plan for.
type watchView struct {
	head     string
	plan     portal.ClusterPlan
	formed   bool
	busy     bool // a start or a stop of the head runs
	formedAt time.Time
}

func (o *clusterOrchestrator) watchViews() []watchView {
	o.mu.Lock()
	defer o.mu.Unlock()
	views := make([]watchView, 0, len(o.heads))
	for head, st := range o.heads {
		if st.lastPlan == nil {
			continue
		}
		views = append(views, watchView{
			head: head, plan: *st.lastPlan, formed: st.formed,
			busy: st.start != nil || st.stopping > 0, formedAt: st.formedAt,
		})
	}
	slices.SortFunc(views, func(a, b watchView) int { return cmp.Compare(a.head, b.head) })
	return views
}

// handle is the worker's turn for one frame pair of serverID; seq is the
// arrival sequence of next.
func (w *clusterWatchdog) handle(serverID string, prev, next []RuntimeStatusDTO, seq uint64) {
	o := w.o
	for _, head := range exitedHeads(prev, next) {
		w.headExited(serverID, head)
	}
	for _, v := range o.watchViews() {
		if v.plan.Head.Server.ID == serverID {
			w.confirmSuspicion(v, next, seq)
			w.checkStale(v, next)
		}
		w.checkMembers(v, serverID, prev, next, seq)
	}
	w.checkBlocked(serverID, next)
}

// confirmSuspicion decides v's pending suspicion on a frame of its head's
// server: a frame that arrived after the member's and shows the head running
// with the process the suspicion saw confirms the loss; any other head frame
// that arrived after it drops the suspicion.
func (w *clusterWatchdog) confirmSuspicion(v watchView, next []RuntimeStatusDTO, seq uint64) {
	sus, ok := w.suspects[v.head]
	if !ok || seq <= sus.seq {
		return
	}
	delete(w.suspects, v.head)
	r, ok := findInstanceRow(next, v.head)
	if !ok || r.State != agentSpecStateRunning || r.PID != sus.headPID {
		slog.Debug("cluster watchdog: the head stopped by itself; a member's lost session is no loss",
			"head_spec_id", v.head, "position", sus.position, "evidence", sus.evidence)
		return
	}
	w.o.memberLost(v.head, sus.position, sus.evidence, headInUse(r))
}

// exitedHeads lists the cluster heads that had a process with their launch
// facts in prev and have none in next. A draining head still has its process.
func exitedHeads(prev, next []RuntimeStatusDTO) []string {
	var exited []string
	for i := range prev {
		p := &prev[i]
		if p.Cluster == nil || p.Instance != 0 || !stateOccupied(p.State) {
			continue
		}
		if r, ok := findInstanceRow(next, p.SpecID); !ok || !stateOccupied(r.State) {
			exited = append(exited, p.SpecID)
		}
	}
	return exited
}

// findInstanceRow returns spec's instance-0 row in rows.
func findInstanceRow(rows []RuntimeStatusDTO, spec string) (RuntimeStatusDTO, bool) {
	i := slices.IndexFunc(rows, func(r RuntimeStatusDTO) bool { return r.SpecID == spec && r.Instance == 0 })
	if i < 0 {
		return RuntimeStatusDTO{}, false
	}
	return rows[i], true
}

// headExited follows a head whose process is gone: no longer formed, its
// members released unless a start or a stop of it runs (which handles them),
// then the reconciler woken, since a kept-up head starts again members first.
// The head's run ends here unless a start runs: that start began a new run,
// and an exit seen meanwhile is the old process's or a failed start's, whose
// run ends as unformed when the next one begins.
func (w *clusterWatchdog) headExited(serverID, head string) {
	o := w.o
	o.mu.Lock()
	st := o.heads[head]
	var run time.Time
	var formed bool
	if st != nil && st.start == nil {
		run, formed = endRunLocked(st)
	}
	var members []portal.ClusterPlanMember
	if st != nil {
		if st.formed {
			st.formed, st.phase = false, ""
			for i := range st.members {
				st.members[i].State, st.members[i].Code = clusterMemberPending, ""
			}
		}
		if st.lastPlan != nil && st.start == nil && st.stopping == 0 {
			members = slices.Clone(st.lastPlan.Members)
		}
	}
	o.mu.Unlock()
	slog.Info("cluster head exited", "server_id", serverID, "head_spec_id", head)
	o.runEnded(head, run, formed)
	if len(members) == 0 {
		o.Trigger()
		return
	}
	now := o.deps.now()
	for _, m := range members {
		w.released[memberKey(m.Server.ID, m.SpecID)] = now
	}
	o.goAction(func(ctx context.Context) {
		defer o.Trigger()
		o.releaseIdle(ctx, head, members)
	})
}

func memberKey(serverID, spec string) string { return serverID + "\x00" + spec }

// checkStale is the stale-telemetry rule on a frame of v's head server.
func (w *clusterWatchdog) checkStale(v watchView, next []RuntimeStatusDTO) {
	o := w.o
	if !v.formed || v.busy || o.inBootGrace() {
		return
	}
	headRow, ok := findInstanceRow(next, v.head)
	if !ok || headRow.State != agentSpecStateRunning {
		return
	}
	now := o.deps.now()
	for _, m := range v.plan.Members {
		since := w.lastArrival(m.Server.ID)
		if since.Before(v.formedAt) {
			since = v.formedAt
		}
		limit := max(clusterStaleIntervals*w.interval(m.Server.ID), o.t.staleFloor)
		if now.Sub(since) > limit {
			o.memberLost(v.head, m.Position, evidenceStale, headInUse(headRow))
			return
		}
	}
}

// headInUse reports whether a head's row shows requests: in flight, active or
// queued.
func headInUse(r RuntimeStatusDTO) bool {
	return r.InFlight > 0 || r.ActiveRequests > 0 || r.QueueDepth > 0
}

// headRow returns plan's head row from the registry: ok false when its server
// has not reported since the gateway started; present false when the server's
// telemetry has no row for it.
func (o *clusterOrchestrator) headRow(plan portal.ClusterPlan) (row RuntimeStatusDTO, present, ok bool) {
	rows := o.deps.statuses.statusRows(plan.Head.Server.ID)
	if rows == nil {
		return RuntimeStatusDTO{}, false, false
	}
	row, present = findInstanceRow(rows, plan.Head.Spec.ID)
	return row, present, true
}

// checkMembers applies the member rules to v's members on serverID: the
// breaches, the positive evidence of a lost member (a suspicion until a later
// head frame confirms it), and the release of a dead direct hold.
func (w *clusterWatchdog) checkMembers(v watchView, serverID string, prev, next []RuntimeStatusDTO, seq uint64) {
	o := w.o
	var here []portal.ClusterPlanMember
	for _, m := range v.plan.Members {
		if m.Server.ID == serverID {
			here = append(here, m)
		}
	}
	if len(here) == 0 {
		return
	}
	o.noteBreaches(v.head, serverID, v.plan.Members, here, next)
	if v.busy {
		delete(w.suspects, v.head)
		return
	}
	hr, present, reported := o.headRow(v.plan)
	if v.formed && present && hr.State == agentSpecStateRunning {
		if _, pending := w.suspects[v.head]; pending {
			return
		}
		for _, m := range here {
			if ev := memberEvidence(prev, next, m.SpecID, v.head); ev != "" {
				w.suspects[v.head] = watchSuspicion{position: m.Position, evidence: ev, seq: seq, headPID: hr.PID}
				return
			}
		}
		return
	}
	if v.plan.Transport != clusterTransportDirect || !reported || (present && stateOccupied(hr.State)) {
		return
	}
	now := o.deps.now()
	for _, m := range here {
		r, ok := findInstanceRow(next, m.SpecID)
		key := memberKey(serverID, m.SpecID)
		if !ok || r.RPC == nil || r.RPC.HeldBy != v.head || now.Before(w.released[key].Add(o.t.releaseRetry)) {
			continue
		}
		w.released[key] = now
		slog.Info("cluster watchdog: releasing a member that holds a dead connection to its stopped head",
			"head_spec_id", v.head, "position", m.Position)
		o.goAction(func(ctx context.Context) { o.releaseIdle(ctx, v.head, []portal.ClusterPlanMember{m}) })
	}
}

// memberEvidence is the positive evidence, if any, that the member spec lost
// head between the frames prev and next. Only a member that prev shows held
// by head gives a baseline -- a frame from before the cluster formed shows no
// hold, so a member restarted for the start is no loss -- except for a stall,
// which next shows by itself.
func memberEvidence(prev, next []RuntimeStatusDTO, spec, head string) string {
	n, nok := findInstanceRow(next, spec)
	p, pok := findInstanceRow(prev, spec)
	switch {
	case nok && stateOccupied(n.State) && n.RPC != nil && n.RPC.Stalled:
		return evidenceStalled
	case !pok || !stateOccupied(p.State) || p.RPC == nil || p.RPC.HeldBy != head:
		return ""
	case !nok || !stateOccupied(n.State):
		return evidenceExited
	case p.PID != 0 && n.PID != 0 && p.PID != n.PID:
		return evidencePIDChanged
	case n.RPC == nil || n.RPC.HeldBy != head:
		return evidenceSessionClosed
	}
	return ""
}

// noteBreaches replaces head's breaches of the members here (all on
// serverID) by what next reports, drops those of positions no longer among
// members (the head's plan), and logs each one not listed before.
func (o *clusterOrchestrator) noteBreaches(head, serverID string, members, here []portal.ClusterPlanMember,
	next []RuntimeStatusDTO,
) {
	var found []portal.ClusterBreachDTO
	positions := make([]int, 0, len(here))
	for _, m := range here {
		positions = append(positions, m.Position)
		r, ok := findInstanceRow(next, m.SpecID)
		if !ok || r.RPC == nil {
			continue
		}
		for _, b := range r.RPC.Breaches {
			found = append(found, portal.ClusterBreachDTO{Position: m.Position, Kind: portal.ClusterBreachPeer, Peer: b.Peer, At: b.At})
		}
		if r.RPC.HeldBy != "" && r.RPC.HeldBy != head {
			found = append(found, portal.ClusterBreachDTO{Position: m.Position, Kind: portal.ClusterBreachForeignHold, At: r.RPC.HeldSince})
		}
	}
	o.mu.Lock()
	st := o.heads[head]
	if st == nil {
		o.mu.Unlock()
		return
	}
	old := st.breaches
	breaches := slices.DeleteFunc(slices.Clone(old), func(b portal.ClusterBreachDTO) bool {
		inPlan := slices.ContainsFunc(members, func(m portal.ClusterPlanMember) bool { return m.Position == b.Position })
		return !inPlan || slices.Contains(positions, b.Position)
	})
	breaches = append(breaches, found...)
	st.breaches = breaches
	slices.SortFunc(st.breaches, func(a, b portal.ClusterBreachDTO) int {
		return cmp.Or(cmp.Compare(a.Position, b.Position), cmp.Compare(a.Kind, b.Kind), cmp.Compare(a.Peer, b.Peer))
	})
	if len(st.breaches) == 0 {
		st.breaches = nil
	}
	o.mu.Unlock()
	for _, b := range found {
		if !slices.ContainsFunc(old, func(x portal.ClusterBreachDTO) bool {
			return x.Position == b.Position && x.Kind == b.Kind && x.Peer == b.Peer
		}) {
			slog.Warn("cluster member breach", "head_spec_id", head, "server_id", serverID, "position", b.Position, "kind", b.Kind, "peer", b.Peer)
		}
	}
}

// memberLost stops head after the member at position was lost (evidence
// names how the watchdog knows): unless a start or a stop of the head runs,
// or it is not formed, the failure is recorded at once -- so a request the
// head fails meanwhile names the member -- and the head cools down; the head
// is then stopped and its members released. inUse makes the reconciler start
// the head again once the cooldown ended.
func (o *clusterOrchestrator) memberLost(head string, position int, evidence string, inUse bool) {
	o.mu.Lock()
	st := o.heads[head]
	if st == nil || !st.formed || st.start != nil || st.stopping > 0 {
		o.mu.Unlock()
		return
	}
	now := o.deps.now()
	p := position
	st.formed, st.phase = false, ""
	st.failure = &portal.ClusterFailureDTO{Code: codeClusterMemberLost, Position: &p, At: now}
	st.failErr = &clusterError{
		Code: codeClusterStartFailed, Cause: codeClusterMemberLost, Position: position,
		Message: "the cluster lost this member (" + evidence + ") and was stopped; it cools down before it starts again",
	}
	st.cooldownUntil = now.Add(o.t.cooldown)
	st.resume = st.resume || inUse
	for i := range st.members {
		if st.members[i].Position == position {
			st.members[i].State, st.members[i].Code = clusterMemberFailed, codeClusterMemberLost
		} else {
			st.members[i].State, st.members[i].Code = clusterMemberPending, ""
		}
	}
	// The stop marker keeps every start out until the head is down; Stop
	// takes its own.
	st.stopping++
	o.mu.Unlock()
	slog.Warn("cluster watchdog: a member was lost; stopping the head", "head_spec_id", head,
		"position", position, "evidence", evidence, "restart_after_cooldown", inUse)
	if !o.goAction(func(ctx context.Context) {
		defer o.unmarkStopping(head)
		// One retry: the head aborts once a member is gone, so a stop that
		// failed (an agent restarting, a store read timing out) is worth one
		// more try; after it the head idles out, and its exit releases the
		// members.
		for attempt := range 2 {
			err := o.Stop(ctx, head, false)
			if err == nil || ctx.Err() != nil {
				break
			}
			slog.Warn("cluster watchdog: stopping the head failed", "head_spec_id", head, "attempt", attempt+1, "err", err)
		}
		if inUse {
			time.AfterFunc(o.t.cooldown, o.Trigger)
		}
	}) {
		o.unmarkStopping(head)
	}
}

func (o *clusterOrchestrator) unmarkStopping(head string) {
	o.mu.Lock()
	o.headState(head).stopping--
	o.mu.Unlock()
}

// goAction runs fn on its own goroutine under the server's lifetime and
// reports whether it did: not once the lifetime ended.
func (o *clusterOrchestrator) goAction(fn func(ctx context.Context)) bool {
	base := o.deps.base()
	if base.Err() != nil {
		return false
	}
	o.wg.Add(1)
	go func() {
		defer o.wg.Done()
		fn(base)
	}()
	return true
}

// claimIdle takes head's stop marker for a removal the watchdog begins, when
// neither a start nor a stop of the head runs; ok false otherwise. The
// returned func gives the marker back.
func (o *clusterOrchestrator) claimIdle(head string) (func(), bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	st := o.headState(head)
	if st.start != nil || st.stopping > 0 {
		return nil, false
	}
	st.stopping++
	return func() { o.unmarkStopping(head) }, true
}

// claimRelease counts a member release of head as under way when neither a
// start nor a stop of the head runs; ok false otherwise. Unlike claimIdle it
// refuses no start: a start waits for the release (awaitReleases) before it
// ensures the members. The returned func ends the release.
func (o *clusterOrchestrator) claimRelease(head string) (func(), bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	st := o.headState(head)
	if st.start != nil || st.stopping > 0 {
		return nil, false
	}
	st.releasing++
	return func() {
		o.mu.Lock()
		defer o.mu.Unlock()
		st := o.headState(head)
		if st.releasing--; st.releasing == 0 && st.releasesDone != nil {
			close(st.releasesDone)
			st.releasesDone = nil
		}
	}, true
}

// awaitReleases returns once no member release of head claimed by
// claimRelease is under way, or with ctx's error. Each release call is
// bounded, so the wait is too.
func (o *clusterOrchestrator) awaitReleases(ctx context.Context, head string) error {
	o.mu.Lock()
	st := o.headState(head)
	if st.releasing == 0 {
		o.mu.Unlock()
		return nil
	}
	if st.releasesDone == nil {
		st.releasesDone = make(chan struct{})
	}
	done := st.releasesDone
	o.mu.Unlock()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// releaseIdle releases members of head while no start or stop of it runs (a
// stop releases them itself; a start that begins meanwhile waits for the
// release before it re-arms their reservations).
func (o *clusterOrchestrator) releaseIdle(ctx context.Context, head string, members []portal.ClusterPlanMember) {
	done, ok := o.claimRelease(head)
	if !ok {
		return
	}
	defer done()
	agents := o.deps.agents()
	if agents == nil {
		return
	}
	o.releaseMembers(ctx, head, members, agents)
}

// releaseMembers ends each member's reservation and hold, in parallel, each
// call bounded; a failure is logged, since the member then idles out at the
// end of its reservation anyway.
func (o *clusterOrchestrator) releaseMembers(ctx context.Context, head string, members []portal.ClusterPlanMember, agents clusterAgentClient) {
	var wg sync.WaitGroup
	for _, m := range members {
		wg.Go(func() {
			target := clusterMemberTarget(m)
			rctx, cancel := context.WithTimeout(ctx, runtimeStopTimeout)
			defer cancel()
			if err := agents.ReleaseRPCMember(o.deps.authCtx(rctx, target), target, m.SpecID); err != nil {
				slog.Warn("cluster: releasing a member failed", "head_spec_id", head, "position", m.Position, "err", err)
			}
		})
	}
	wg.Wait()
}

// releaseAfterFailedStart releases the members a failed start of head
// reserved, once the head shows no process (a formation that never came
// stopped it), bounded by stopWait; a head that keeps running keeps them.
func (o *clusterOrchestrator) releaseAfterFailedStart(ctx context.Context, head string, plan portal.ClusterPlan) {
	rows, updates, unsubscribe := o.deps.statuses.subscribe(plan.Head.Server.ID)
	gone := o.awaitHeadGone(ctx, plan, rows, updates)
	unsubscribe()
	if !gone {
		slog.Info("cluster start failed: the head kept running, so its members keep their reservations", "head_spec_id", head)
		return
	}
	o.releaseIdle(ctx, head, plan.Members)
}

// checkBlocked handles the local starts on serverID that wait only for a held
// member (admission_blocked_by): each blocking member is mapped to its head
// and the head's removal is asked for, at most once per blockedRetry per
// member.
func (w *clusterWatchdog) checkBlocked(serverID string, next []RuntimeStatusDTO) {
	o := w.o
	now := o.deps.now()
	var seen []string
	for _, r := range next {
		member := r.AdmissionBlockedBy
		if member == "" || slices.Contains(seen, member) {
			continue
		}
		seen = append(seen, member)
		key := memberKey(serverID, member)
		if last, ok := w.asked[key]; ok && now.Before(last.Add(o.t.blockedRetry)) {
			continue
		}
		w.asked[key] = now
		candidates := o.memberHeadCandidates(serverID, member, next)
		o.goAction(func(ctx context.Context) { o.removeForBlockedStart(ctx, serverID, member, candidates) })
	}
}

// memberHeadCandidates lists the heads the member spec on serverID may serve:
// the head it reports holding it, the heads whose last plan lists it, and the
// head an automatic member's id names (rpca_<head>_<server>). They are only
// candidates: removeForBlockedStart keeps one only when its plan lists the
// member on serverID.
func (o *clusterOrchestrator) memberHeadCandidates(serverID, member string, next []RuntimeStatusDTO) []string {
	var heads []string
	add := func(h string) {
		if h != "" && !slices.Contains(heads, h) {
			heads = append(heads, h)
		}
	}
	if r, ok := findInstanceRow(next, member); ok && r.RPC != nil {
		add(r.RPC.HeldBy)
	}
	for _, v := range o.watchViews() {
		if slices.ContainsFunc(v.plan.Members, func(m portal.ClusterPlanMember) bool { return m.Server.ID == serverID && m.SpecID == member }) {
			add(v.head)
		}
	}
	if rest, ok := strings.CutPrefix(member, "rpca_"); ok {
		if head, ok := strings.CutSuffix(rest, "_"+serverID); ok {
			add(head)
		}
	}
	return heads
}

// removeForBlockedStart removes the cluster whose member on serverID a local
// start waits for, head first, when the head's agent lets the head go: a
// conditional stop, and once the head is gone its members released. A head
// that is forming, stopping or busy stays, and the start waits.
func (o *clusterOrchestrator) removeForBlockedStart(ctx context.Context, serverID, member string, candidates []string) {
	for _, head := range candidates {
		rctx, cancel := context.WithTimeout(ctx, clusterReadTimeout)
		plan, err := o.deps.portal.ClusterPlan(rctx, head)
		cancel()
		if err != nil {
			continue
		}
		o.mu.Lock()
		o.rememberPlanLocked(o.headState(head), plan)
		o.mu.Unlock()
		members := o.membersOf(head, plan)
		if !slices.ContainsFunc(members, func(m portal.ClusterPlanMember) bool { return m.Server.ID == serverID && m.SpecID == member }) {
			continue
		}
		o.conditionalStop(ctx, head, plan, members)
		return
	}
	slog.Debug("cluster watchdog: no head serves the member a local start waits for", "server_id", serverID, "member_spec_id", member)
}

func (o *clusterOrchestrator) conditionalStop(ctx context.Context, head string, plan portal.ClusterPlan, members []portal.ClusterPlanMember) {
	done, ok := o.claimIdle(head)
	if !ok {
		slog.Debug("cluster watchdog: a start or stop of the head runs; the local start waits", "head_spec_id", head)
		return
	}
	defer done()
	agents := o.deps.agents()
	if agents == nil {
		return
	}
	err := o.drainHead(ctx, plan, agents, true)
	var re *provider.RouterError
	switch {
	case errors.As(err, &re) && re.Code == codeRuntimeInstanceBusy:
		slog.Info("cluster watchdog: a local start waits for a cluster member; the head is in use, so the start waits", "head_spec_id", head)
		return
	case err != nil:
		slog.Warn("cluster watchdog: the conditional stop of the head failed; the local start waits", "head_spec_id", head, "err", err)
		return
	}
	slog.Info("cluster watchdog: removed a cluster, head first, for a local start on a member's server", "head_spec_id", head)
	o.markStopped(head)
	o.releaseMembers(ctx, head, members, agents)
}

var _ portal.ClusterMeshDriftHandler = (*clusterOrchestrator)(nil)

// StopDirectMeshClusters stops every direct mesh cluster the derivation now
// withholds (portal.ClusterWithheldDirectUnavailable: NetBird's Default policy
// was seen enabled again, so every mesh peer could reach the members' RPC
// ports), head first, then releases and stops the members; it returns once
// every stop ended. Only heads that run, are formed or are forming are
// stopped. It is the portal's ClusterMeshDriftHandler, called before the
// documents that withhold those clusters are pushed.
func (o *clusterOrchestrator) StopDirectMeshClusters(ctx context.Context) {
	if o == nil {
		return
	}
	var wg sync.WaitGroup
	for _, head := range o.liveHeads() {
		wg.Go(func() {
			rctx, cancel := context.WithTimeout(ctx, clusterReadTimeout)
			plan, err := o.deps.portal.ClusterPlan(rctx, head)
			cancel()
			if err != nil || plan.WithheldReason != portal.ClusterWithheldDirectUnavailable {
				return
			}
			slog.Warn("cluster: NetBird's Default policy is enabled again; stopping a direct mesh cluster head first", "head_spec_id", head)
			if err := o.Stop(ctx, head, true); err != nil {
				slog.Warn("cluster: stopping a direct mesh cluster after the policy drift failed", "head_spec_id", head, "err", err)
			}
		})
	}
	wg.Wait()
}

// liveHeads lists the heads that are formed or forming in the orchestration,
// or whose process the agents' telemetry shows with launch facts.
func (o *clusterOrchestrator) liveHeads() []string {
	var heads []string
	o.mu.Lock()
	for head, st := range o.heads {
		if st.formed || st.start != nil {
			heads = append(heads, head)
		}
	}
	o.mu.Unlock()
	for _, id := range o.deps.statuses.serverIDs() {
		for _, r := range o.deps.statuses.statusRows(id) {
			if r.Cluster != nil && r.Instance == 0 && stateOccupied(r.State) && !slices.Contains(heads, r.SpecID) {
				heads = append(heads, r.SpecID)
			}
		}
	}
	slices.Sort(heads)
	return heads
}
