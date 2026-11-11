// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/provider"
	"op-ai-gateway/internal/routing"
	"slices"
	"strconv"
	"sync"
	"time"
)

// The cluster orchestration starts a llama.cpp RPC cluster members first: an
// agent never starts a cold cluster head by itself -- only an ensure that
// carries the cluster-start mark does, and only this orchestration sends one.
// A start (ensureCluster) runs four steps, one start per head at a time:
//
//  1. Configuration applied: the head's and every member's agent acknowledge
//     a runtime document that carries its part of the cluster (for a mesh
//     cluster after the RPC NetBird policies were reconciled), bounded.
//  2. Members, in parallel: each is ensured through the agent's member route
//     within its startup timeout plus a margin, arming a reservation that
//     outlasts the head's start. In direct transport a member that still
//     reports a hold while its head is not running holds a dead connection,
//     so it is force-stopped first.
//  3. Head: one ensure with the cluster-start mark, bounded by the head's
//     startup timeout plus a margin.
//  4. Formed: every member reports the head's hold, on telemetry that arrived
//     after the head's start began, within the same bound; otherwise the head
//     is stopped and the start fails with runtime.cluster_member_unused.
//
// A failed start leaves the started members with nothing but their
// reservation, so they idle out on their own. The head then cools down; a
// cause that only a configuration change can fix (an unsupported build, a
// version or device mismatch) blocks automatic starts until the head's
// document changes. The orchestration's state lives in memory: a restarted
// gateway learns it again from the agents' telemetry.

// The cluster orchestration's own error codes. A start's error names its cause
// (the agent's code or one of these) and the failing member's position or the
// head beside the code (clusterError).
const (
	// codeClusterStartFailed: a start failed; the head cools down.
	codeClusterStartFailed = "runtime.cluster_start_failed"
	// codeClusterMemberUnused: the head started but a member never reported
	// its hold within the bound (a split of 0, a model that fits elsewhere).
	codeClusterMemberUnused = "runtime.cluster_member_unused"
	// codeClusterUnavailable: the cluster cannot be started at all -- the
	// runtime documents withhold it, or its head spec is gone.
	codeClusterUnavailable = "runtime.cluster_unavailable"
	// codeClusterConfigUnacknowledged: an agent of the cluster did not
	// acknowledge a document carrying its part within the bound.
	codeClusterConfigUnacknowledged = "runtime.cluster_config_unacknowledged"
	// codeClusterStartCancelled: a stop of the head or the gateway's shutdown
	// ended the start before it completed. Nothing failed, so nothing cools
	// down.
	codeClusterStartCancelled = "runtime.cluster_start_cancelled"
	// codeClusterStopping: a stop of the head is under way; a start waits
	// for it to finish.
	codeClusterStopping = "runtime.cluster_stopping"
)

// maxPlanRereads bounds how often one start re-reads a plan the runtime
// documents moved past during step one.
const maxPlanRereads = 3

// The agent's codes the orchestration reads: the router's start refusals and
// the lifecycle causes that travel on telemetry (last_error.code), never on
// the router's answer.
const (
	codeRuntimeStartTimeout      = "runtime.start_timeout"
	codeRuntimeStartFailed       = "runtime.start_failed"
	codeRuntimeNotPermitted      = "runtime.not_permitted"
	codeRPCUnsupportedBuild      = "runtime.rpc_unsupported_build"
	codeClusterVersionMismatch   = "runtime.cluster_version_mismatch"
	codeDeviceBackendMismatch    = "runtime.device_backend_mismatch"
	codeRPCDeviceCountMismatch   = "runtime.rpc_device_count_mismatch"
	codeClusterMemberUnreachable = "runtime.cluster_member_unreachable"
	codeBindMismatch             = "runtime.bind_mismatch"
	codeClusterNotReady          = "runtime.cluster_not_ready"
	codeClusterMemberInUse       = "runtime.cluster_member_in_use"
)

// terminalClusterCauses are the causes a retry cannot fix: the builds do not
// speak RPC or each other's protocol version, or the devices the member or
// head offers are not the ones its configuration names. Automatic starts stop
// until the head's document changes. Every other cause, an unreachable member
// or a bind mismatch included, only cools down: the member may come back.
var terminalClusterCauses = map[string]bool{
	codeRPCUnsupportedBuild:    true,
	codeClusterVersionMismatch: true,
	codeDeviceBackendMismatch:  true,
	codeRPCDeviceCountMismatch: true,
}

// telemetryDetailedCauses are the router answers whose specific cause the
// agent reports on telemetry instead: a start that failed or timed out, a
// start the agent did not permit, a head whose members it could not reach.
var telemetryDetailedCauses = map[string]bool{
	codeRuntimeStartFailed:       true,
	codeRuntimeStartTimeout:      true,
	codeRuntimeNotPermitted:      true,
	codeClusterMemberUnreachable: true,
}

// The values of a runtime document's cluster network and transport the
// orchestration branches on.
const (
	clusterNetworkMesh     = "mesh"
	clusterTransportDirect = "direct"
)

// The states of one member in a start (portal.ClusterMemberStatusDTO.State).
const (
	clusterMemberPending  = "pending"
	clusterMemberStarting = "starting"
	clusterMemberReady    = "ready"
	clusterMemberHeld     = "held"
	clusterMemberFailed   = "failed"
)

// clusterGateState is a head's state as routing reads it: formed serves,
// forming and cold may park a request, cooling down and invalid exclude the
// head.
type clusterGateState int

const (
	clusterCold clusterGateState = iota
	clusterForming
	clusterFormed
	clusterCoolingDown
	clusterInvalid
)

// String is the state's portal name (portal.ClusterStatusDTO.State).
func (s clusterGateState) String() string {
	switch s {
	case clusterForming:
		return portal.ClusterStateForming
	case clusterFormed:
		return portal.ClusterStateFormed
	case clusterCoolingDown:
		return portal.ClusterStateCoolingDown
	case clusterInvalid:
		return portal.ClusterStateInvalid
	default:
		return portal.ClusterStateCold
	}
}

// clusterError is a failed or refused cluster start: Code is the
// orchestration's code (codeClusterStartFailed, codeClusterMemberUnused,
// codeClusterUnavailable), Cause the code of what failed, and Position the
// member that failed (-1 when none did) or Head for the head.
type clusterError struct {
	Code     string
	Cause    string
	Position int
	Head     bool
	Message  string
}

func (e *clusterError) Error() string {
	where := "cluster"
	switch {
	case e.Head:
		where = "head"
	case e.Position >= 0:
		where = "member " + strconv.Itoa(e.Position)
	}
	msg := e.Code + ": " + where
	if e.Cause != "" && e.Cause != e.Code {
		msg += ": " + e.Cause
	}
	if e.Message != "" {
		msg += ": " + e.Message
	}
	return msg
}

// clusterPortal is what the orchestration reads from the portal: a head's
// plan, the derived runtime documents its agents must acknowledge, the
// synchronous RPC policy pass and the heads the gateway keeps up. Satisfied by
// portal.API.
type clusterPortal interface {
	ClusterPlan(ctx context.Context, headSpecID string) (portal.ClusterPlan, error)
	AgentRuntimeConfig(ctx context.Context, serverID string) (portal.AgentRuntimeConfigDTO, error)
	ReconcileRPCPolicies(ctx context.Context)
	KeptUpClusterHeads(ctx context.Context) ([]string, error)
}

// clusterAgentClient is the agent calls a start and a stop make: the head's
// ensure and stop, and the member routes. Satisfied by the provider
// Multiplexer.
type clusterAgentClient interface {
	provider.RuntimeEnsurer
	provider.RuntimeStopper
	provider.RPCMemberController
}

// clusterOrchestratorDeps is what an orchestrator works with. Every field but
// wake is required.
type clusterOrchestratorDeps struct {
	portal clusterPortal
	// agents returns the provider's agent calls, nil when it has none; asked
	// at every start, as every other caller of the agent routes asks.
	agents   func() clusterAgentClient
	statuses *runtimeStatusRegistry
	// authCtx attaches a target's upstream credential to a call's context.
	authCtx func(ctx context.Context, target routing.Target) context.Context
	// base is the server's lifetime: its end ends every start.
	base func() context.Context
	now  func() time.Time
	// wake, when set, wakes the requests parked in the admission queue for a
	// server: called with the head's server once a start ended, formed or
	// failed, so a request parked for the cluster is served -- or answered
	// the cluster's failure -- at once.
	wake func(serverID string)
}

// clusterTimings are the orchestration's bounds.
type clusterTimings struct {
	// unit is the length of one startup-timeout second; a test shortens it.
	unit time.Duration
	// configWait bounds step one; poll is how often it and the telemetry
	// reads look again.
	configWait time.Duration
	poll       time.Duration
	// cooldown is how long a head rests after a failed start.
	cooldown time.Duration
	// codeWait bounds the wait for a failure's specific code on telemetry.
	codeWait time.Duration
	// stopWait bounds a stop's wait for the head's stopped state.
	stopWait time.Duration
	// memberMargin, headMargin and reserveMargin are added to a member's
	// startup timeout, to the head's, and to the head's bound for a member's
	// reservation.
	memberMargin  time.Duration
	headMargin    time.Duration
	reserveMargin time.Duration
	// bootGrace is how long after the gateway started the reconciler leaves a
	// cluster alone whose servers have not reported yet.
	bootGrace time.Duration
}

func defaultClusterTimings() clusterTimings {
	return clusterTimings{
		unit: time.Second, configWait: 30 * time.Second, poll: 250 * time.Millisecond,
		cooldown: 60 * time.Second, codeWait: 2 * time.Second, stopWait: 60 * time.Second,
		memberMargin: 10 * time.Second, headMargin: 30 * time.Second, reserveMargin: 60 * time.Second,
		bootGrace: 60 * time.Second,
	}
}

// clusterReadTimeout bounds the portal reads a reconciler pass makes.
const clusterReadTimeout = 15 * time.Second

// clusterOrchestrator is the gateway's one cluster orchestration. See the top
// of this file.
type clusterOrchestrator struct {
	deps    clusterOrchestratorDeps
	t       clusterTimings
	bootAt  time.Time
	trigger chan struct{}

	mu    sync.Mutex
	heads map[string]*clusterHeadState
	// views is the cluster gate's view of each head (gateView), viewReads the
	// heads a view read is in flight for.
	views     map[string]clusterGateView
	viewReads map[string]bool

	wg sync.WaitGroup // one per running start and per reconciler-spawned ensure
}

// clusterHeadState is what the orchestration knows of one head.
type clusterHeadState struct {
	formed  bool
	phase   string
	members []portal.ClusterMemberStatusDTO
	// failure is the last failed start; failErr is the error an automatic
	// Ensure answers while the head cools down or is blocked.
	failure       *portal.ClusterFailureDTO
	failErr       *clusterError
	cooldownUntil time.Time
	// terminal blocks automatic starts until the head document's fingerprint
	// differs from terminalPrint.
	terminal      bool
	terminalPrint string
	// withheld is the derivation's reason the last plan was not startable.
	withheld string
	// parked: an operator stopped the head; the reconciler leaves it down
	// until a manual start.
	parked bool
	// stopping counts the stops of the head under way; every start is
	// refused meanwhile, so none can race the drain.
	stopping int
	start    *clusterStart
	// server is the head's server, from the last plan read: the server whose
	// parked requests a start's end wakes.
	server string
}

// clusterStart is one running start that every Ensure of its head joins.
type clusterStart struct {
	done   chan struct{}
	err    error
	cancel context.CancelFunc
	joined int
}

func newClusterOrchestrator(deps clusterOrchestratorDeps, t clusterTimings) *clusterOrchestrator {
	return &clusterOrchestrator{
		deps: deps, t: t, bootAt: deps.now(), trigger: make(chan struct{}, 1),
		heads: map[string]*clusterHeadState{}, views: map[string]clusterGateView{}, viewReads: map[string]bool{},
	}
}

// headState returns head's state, created cold. The caller holds o.mu.
func (o *clusterOrchestrator) headState(head string) *clusterHeadState {
	st := o.heads[head]
	if st == nil {
		st = &clusterHeadState{}
		o.heads[head] = st
	}
	return st
}

// State is head's gate state. A head the orchestration never saw is cold.
func (o *clusterOrchestrator) State(head string) clusterGateState {
	if o == nil {
		return clusterCold
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.stateLocked(o.heads[head])
}

func (o *clusterOrchestrator) stateLocked(st *clusterHeadState) clusterGateState {
	switch {
	case st == nil:
		return clusterCold
	case st.start != nil:
		return clusterForming
	case st.withheld != "" || st.terminal:
		return clusterInvalid
	case o.deps.now().Before(st.cooldownUntil):
		return clusterCoolingDown
	case st.formed:
		return clusterFormed
	default:
		return clusterCold
	}
}

// ClusterStatus is head's orchestration state for the portal's read view
// (portal.ClusterStatusReader).
func (o *clusterOrchestrator) ClusterStatus(head string) portal.ClusterStatusDTO {
	out := portal.ClusterStatusDTO{State: portal.ClusterStateCold, Members: []portal.ClusterMemberStatusDTO{}}
	if o == nil {
		return out
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	st := o.heads[head]
	out.State = o.stateLocked(st).String()
	if st == nil {
		return out
	}
	out.Phase = st.phase
	out.Members = append(out.Members, st.members...)
	if st.failure != nil {
		f := *st.failure
		if f.Position != nil {
			p := *f.Position
			f.Position = &p
		}
		out.LastFailure = &f
	}
	if !st.terminal && o.deps.now().Before(st.cooldownUntil) {
		until := st.cooldownUntil
		out.CooldownUntil = &until
	}
	return out
}

// Ensure starts head's cluster members first and returns once it is formed,
// or with the start's *clusterError. A start of head already running is
// joined, not repeated. While head cools down after a failed start, or is
// blocked by a terminal cause, Ensure answers that failure at once and asks
// no agent. The start itself runs in the background under the server's
// lifetime: ctx ending returns ctx's error to this caller only.
func (o *clusterOrchestrator) Ensure(ctx context.Context, head string) error {
	return o.ensure(ctx, head, false)
}

// StartManual is the operator's start of head: Ensure without the cooldown
// and the terminal block, which an operator's explicit start overrides, and it
// ends a park a manual stop set.
func (o *clusterOrchestrator) StartManual(ctx context.Context, head string) error {
	return o.ensure(ctx, head, true)
}

func (o *clusterOrchestrator) ensure(ctx context.Context, head string, manual bool) error {
	if o == nil {
		return &clusterError{Code: codeClusterUnavailable, Position: -1, Message: "this gateway runs no cluster orchestration"}
	}
	o.mu.Lock()
	st := o.headState(head)
	if st.stopping > 0 {
		o.mu.Unlock()
		return &clusterError{Code: codeClusterUnavailable, Cause: codeClusterStopping, Position: -1, Message: "the cluster is being stopped; start it again once the stop finished"}
	}
	if manual {
		st.parked = false
	}
	if s := st.start; s != nil {
		s.joined++
		o.mu.Unlock()
		return joinClusterStart(ctx, s)
	}
	if !manual && st.failErr != nil && (st.terminal || o.deps.now().Before(st.cooldownUntil)) {
		err := st.failErr
		o.mu.Unlock()
		return err
	}
	base := o.deps.base()
	if base.Err() != nil {
		o.mu.Unlock()
		return &clusterError{Code: codeClusterUnavailable, Cause: codeClusterStartCancelled, Position: -1, Message: "the gateway is shutting down"}
	}
	s := o.startLocked(base, head, st)
	o.mu.Unlock()
	return joinClusterStart(ctx, s)
}

// startLocked begins a start of head under base and records it on st; the
// caller holds o.mu and has checked that none runs. Once the start ended, the
// requests parked on the head's server are woken (deps.wake).
func (o *clusterOrchestrator) startLocked(base context.Context, head string, st *clusterHeadState) *clusterStart {
	runCtx, cancel := context.WithCancel(base)
	s := &clusterStart{done: make(chan struct{}), cancel: cancel}
	st.start = s
	o.wg.Add(1)
	go func() {
		defer o.wg.Done()
		defer cancel()
		err := o.run(runCtx, head)
		o.mu.Lock()
		hs := o.headState(head)
		hs.start = nil
		s.err = err
		close(s.done)
		server := hs.server
		o.mu.Unlock()
		if o.deps.wake != nil && server != "" {
			o.deps.wake(server)
		}
	}()
	return s
}

// Signal is the cluster orchestration's demand sink (routing.DemandSink, through
// the gateway's fan-out): a request parked for d.ClusterHead, a cold or forming
// head, asks for its cluster. It starts one in the background under the
// server's lifetime unless one runs already, a stop of the head is under way,
// or the head cools down or is blocked -- the automatic start's rules
// (Ensure). It never blocks: every parked request signals on every park turn,
// and the signals of one head coalesce into its one start.
func (o *clusterOrchestrator) Signal(d routing.DemandSignal) {
	if o == nil || d.ClusterHead == "" {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	st := o.headState(d.ClusterHead)
	if st.start != nil || st.stopping > 0 || st.formed {
		return
	}
	if st.failErr != nil && (st.terminal || o.deps.now().Before(st.cooldownUntil)) {
		return
	}
	base := o.deps.base()
	if base.Err() != nil {
		return
	}
	o.startLocked(base, d.ClusterHead, st)
}

func joinClusterStart(ctx context.Context, s *clusterStart) error {
	select {
	case <-s.done:
		return s.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// joiners reports how many Ensure calls joined head's running start; for
// tests.
func (o *clusterOrchestrator) joiners(head string) int {
	o.mu.Lock()
	defer o.mu.Unlock()
	if st := o.heads[head]; st != nil && st.start != nil {
		return st.start.joined
	}
	return 0
}

// wait returns once every start and reconciler-spawned ensure has ended. The
// shutdown cancels the server's lifetime first, which ends each of them, and
// then joins them here before the store closes; tests join them as well.
func (o *clusterOrchestrator) wait() {
	if o != nil {
		o.wg.Wait()
	}
}

// run is one start of head (the four steps at the top of this file).
func (o *clusterOrchestrator) run(ctx context.Context, head string) error {
	plan, err := o.plan(ctx, head)
	if err != nil {
		return err
	}
	agents := o.deps.agents()
	if agents == nil {
		return &clusterError{Code: codeClusterUnavailable, Position: -1, Message: "this gateway's provider cannot start a cluster through the agents"}
	}
	if o.formedNow(plan) {
		o.markFormed(head, plan)
		return nil
	}
	o.beginStart(head, plan)

	plan, ce := o.applyConfig(ctx, head, plan)
	fp := o.fingerprint(ctx, plan)
	if ce != nil {
		return o.fail(ctx, head, ce, fp)
	}

	headBound := o.startupBound(plan.Head.Spec.StartupTimeoutSeconds) + o.t.headMargin
	o.setPhase(head, portal.ClusterPhaseMembersStarting)
	if ce := o.startMembers(ctx, head, plan, agents, headBound+o.t.reserveMargin); ce != nil {
		return o.fail(ctx, head, ce, fp)
	}
	o.setPhase(head, portal.ClusterPhaseMembersReady)

	o.setPhase(head, portal.ClusterPhaseHeadStarting)
	watch := o.watchHolds(ctx, plan)
	defer watch.stop()
	hctx, cancel := context.WithTimeout(ctx, headBound)
	defer cancel()
	target := runtimeInstanceTarget(plan.Head, 0)
	before := o.lastError(plan.Head.Server.ID, head)
	if err := agents.EnsureRuntimeModel(provider.WithClusterStart(o.deps.authCtx(hctx, target)), target); err != nil {
		cause := o.cause(ctx, err, plan.Head.Server.ID, head, before)
		return o.fail(ctx, head, &clusterError{Code: codeClusterStartFailed, Cause: cause, Position: -1, Head: true}, fp)
	}
	if pos, formed := watch.await(hctx, head, plan); !formed {
		o.stopHead(ctx, plan, agents)
		return o.fail(ctx, head, &clusterError{
			Code: codeClusterMemberUnused, Cause: codeClusterMemberUnused, Position: pos,
			Message: "the member never reported the head's hold; check the split (a member given no layers) and whether the model fits without it",
		}, fp)
	}
	o.markFormed(head, plan)
	return nil
}

// plan loads head's plan; a withheld cluster marks the head invalid.
func (o *clusterOrchestrator) plan(ctx context.Context, head string) (portal.ClusterPlan, error) {
	plan, err := o.deps.portal.ClusterPlan(ctx, head)
	if errors.Is(err, portal.ErrRuntimeSpecNotFound) {
		return plan, &clusterError{Code: codeClusterUnavailable, Cause: portal.ErrRuntimeSpecNotFound.Error(), Position: -1}
	}
	if err != nil {
		// The store's error stays in the log: it is no answer for a client.
		slog.Warn("cluster start: reading the head's plan failed", "head_spec_id", head, "err", err)
		return plan, &clusterError{Code: codeClusterUnavailable, Cause: codeRuntimeActionFailed, Position: -1}
	}
	o.mu.Lock()
	st := o.headState(head)
	st.withheld, st.server = plan.WithheldReason, plan.Head.Server.ID
	o.views[head] = o.viewOf(plan)
	o.mu.Unlock()
	if plan.WithheldReason != "" {
		return plan, &clusterError{Code: codeClusterUnavailable, Cause: plan.WithheldReason, Position: -1}
	}
	return plan, nil
}

// startupBound is a startup timeout of secs seconds in the orchestration's
// unit; 0 is the agent's own default.
func (o *clusterOrchestrator) startupBound(secs int) time.Duration {
	if secs <= 0 {
		secs = int(agentDefaultStartupTimeout / time.Second)
	}
	return time.Duration(secs) * o.t.unit
}

// beginStart resets head's start view: every member pending.
func (o *clusterOrchestrator) beginStart(head string, plan portal.ClusterPlan) {
	o.mu.Lock()
	defer o.mu.Unlock()
	st := o.headState(head)
	st.formed = false
	st.phase = ""
	st.members = make([]portal.ClusterMemberStatusDTO, len(plan.Members))
	for i, m := range plan.Members {
		st.members[i] = portal.ClusterMemberStatusDTO{Position: m.Position, State: clusterMemberPending}
	}
}

func (o *clusterOrchestrator) setPhase(head, phase string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.headState(head).phase = phase
}

func (o *clusterOrchestrator) setMember(head string, position int, state, code string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	st := o.headState(head)
	for i := range st.members {
		m := &st.members[i]
		if m.Position == position {
			m.State, m.Code = state, code
		}
	}
}

// markFormed records head formed: every member held, no cooldown, no block.
func (o *clusterOrchestrator) markFormed(head string, plan portal.ClusterPlan) {
	o.mu.Lock()
	defer o.mu.Unlock()
	st := o.headState(head)
	st.formed, st.phase = true, portal.ClusterPhaseFormed
	st.members = make([]portal.ClusterMemberStatusDTO, len(plan.Members))
	for i, m := range plan.Members {
		st.members[i] = portal.ClusterMemberStatusDTO{Position: m.Position, State: clusterMemberHeld}
	}
	st.cooldownUntil, st.failErr, st.terminal, st.terminalPrint = time.Time{}, nil, false, ""
}

// fail records a failed start and returns ce: a terminal cause blocks
// automatic starts until the head's document fingerprint differs from print,
// any other cools the head down. A start the stop or the shutdown cancelled
// records nothing: it did not fail, it was ended.
func (o *clusterOrchestrator) fail(ctx context.Context, head string, ce *clusterError, fp string) error {
	if ctx.Err() != nil {
		o.mu.Lock()
		st := o.headState(head)
		st.formed, st.phase = false, ""
		o.mu.Unlock()
		return &clusterError{Code: codeClusterUnavailable, Cause: codeClusterStartCancelled, Position: -1, Message: "a stop of the head or the gateway's shutdown ended the start"}
	}
	now := o.deps.now()
	failure := &portal.ClusterFailureDTO{Code: ce.Cause, Head: ce.Head, At: now}
	if ce.Position >= 0 {
		p := ce.Position
		failure.Position = &p
	}
	o.mu.Lock()
	st := o.headState(head)
	st.formed, st.failure, st.failErr = false, failure, ce
	if terminalClusterCauses[ce.Cause] {
		st.terminal, st.terminalPrint = true, fp
	} else {
		st.cooldownUntil = now.Add(o.t.cooldown)
	}
	o.mu.Unlock()
	slog.Warn("cluster start failed", "head_spec_id", head, "code", ce.Code, "cause", ce.Cause,
		"position", ce.Position, "head", ce.Head, "terminal", terminalClusterCauses[ce.Cause])
	return ce
}

// fingerprint is a digest of head's spec in its server's current document --
// what a terminal cause waits to see change -- or "" when the document cannot
// be derived or does not carry the head.
func (o *clusterOrchestrator) fingerprint(ctx context.Context, plan portal.ClusterPlan) string {
	doc, err := o.deps.portal.AgentRuntimeConfig(ctx, plan.Head.Server.ID)
	if err != nil {
		return ""
	}
	i := slices.IndexFunc(doc.Specs, func(s portal.AgentRuntimeSpecDTO) bool { return s.ID == plan.Head.Spec.ID })
	if i < 0 {
		return ""
	}
	raw, err := json.Marshal(doc.Specs[i])
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:8])
}

// row returns the status row of spec's instance 0 on serverID, ok false when
// the server reports none.
func (o *clusterOrchestrator) row(serverID, spec string) (RuntimeStatusDTO, bool) {
	rows := o.deps.statuses.statusRows(serverID)
	for i := range rows {
		if rows[i].SpecID == spec && rows[i].Instance == 0 {
			return rows[i], true
		}
	}
	return RuntimeStatusDTO{}, false
}

// headOccupied reports whether the head's agent reports a process for it:
// starting, running or draining. While it has one the head may still hold its
// members, so a member's hold is not yet a dead connection.
func (o *clusterOrchestrator) headOccupied(plan portal.ClusterPlan) bool {
	r, ok := o.row(plan.Head.Server.ID, plan.Head.Spec.ID)
	return ok && stateOccupied(r.State)
}

// stateOccupied reports whether an agent state has a process behind it.
func stateOccupied(state string) bool {
	return state == agentSpecStateRunning || state == agentSpecStateStarting || state == agentSpecStateDraining
}

// agentSpecStateDraining is the state of an instance on its way out.
const agentSpecStateDraining = "draining"

// applyConfig is step one under a single bound of configWait. A mesh
// cluster's RPC policies are reconciled first, inside that bound: the policy
// pass serializes on the portal's policy lock and does not watch ctx while it
// waits for it, so it runs aside and the start stops waiting for it when the
// bound ends -- the acknowledgement wait then has what is left, and the next
// policy pass (every policy write and the NetBird sync run one) completes the
// rules. When the runtime documents moved past plan meanwhile (the head's
// document no longer carries plan's generation), the plan is read again --
// at most maxPlanRereads times -- rather than waiting out the bound for an
// acknowledgement that can never come.
func (o *clusterOrchestrator) applyConfig(ctx context.Context, head string, plan portal.ClusterPlan) (portal.ClusterPlan, *clusterError) {
	cctx, cancel := context.WithTimeout(ctx, o.t.configWait)
	defer cancel()
	if plan.Network == clusterNetworkMesh {
		o.reconcilePolicies(cctx, head)
	}
	for reread := 0; ; reread++ {
		ce, stale := o.awaitConfig(ctx, cctx, plan)
		if !stale {
			return plan, ce
		}
		if reread >= maxPlanRereads {
			return plan, &clusterError{
				Code: codeClusterStartFailed, Cause: codeClusterConfigUnacknowledged, Position: -1, Head: true,
				Message: "the cluster's configuration kept changing while it started",
			}
		}
		next, err := o.plan(ctx, head)
		if err != nil {
			var ce *clusterError
			errors.As(err, &ce)
			return plan, ce
		}
		plan = next
		o.beginStart(head, plan)
	}
}

// reconcilePolicies runs the portal's RPC policy pass and waits for it until
// ctx ends (see applyConfig).
func (o *clusterOrchestrator) reconcilePolicies(ctx context.Context, head string) {
	done := make(chan struct{})
	o.wg.Add(1)
	go func() {
		defer o.wg.Done()
		defer close(done)
		o.deps.portal.ReconcileRPCPolicies(ctx)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		slog.Info("cluster start: the RPC policy pass did not finish within the configuration bound; going on", "head_spec_id", head)
	}
}

// formedNow reports whether the agents' telemetry already shows the cluster
// formed: the head running with its launch facts and every member held by
// it. A start of such a cluster has nothing to do -- the case of a gateway
// restarted under a running cluster.
func (o *clusterOrchestrator) formedNow(plan portal.ClusterPlan) bool {
	r, ok := o.row(plan.Head.Server.ID, plan.Head.Spec.ID)
	if !ok || r.State != agentSpecStateRunning || r.Cluster == nil {
		return false
	}
	for _, m := range plan.Members {
		mr, ok := o.row(m.Server.ID, m.SpecID)
		if !ok || mr.RPC == nil || mr.RPC.HeldBy != plan.Head.Spec.ID {
			return false
		}
	}
	return true
}

// awaitConfig waits, until cctx ends, for every agent of the cluster to
// acknowledge a document that carries its part -- the head's spec with this
// cluster's generation on the head's server, the member's spec on each
// member's server -- or fails naming the first agent that did not. Like the
// VRAM isolation gate it accepts only an ETag the gateway derived itself and
// found to carry that part, so no agent-supplied string can pass it; a
// reported ETag is derived at most once. stale reports that the head's
// current document does not carry plan's cluster: the plan is out of date.
// ctx is the start's: its end is a cancellation, cctx's alone the bound.
func (o *clusterOrchestrator) awaitConfig(ctx, cctx context.Context, plan portal.ClusterPlan) (ce *clusterError, stale bool) {
	type gate struct {
		serverID  string
		position  int // -1: the head's server
		accepted  map[string]bool
		evaluated string
	}
	headServer := plan.Head.Server.ID
	if doc, err := o.deps.portal.AgentRuntimeConfig(cctx, headServer); err == nil && doc.ETag != "" && !documentCarriesCluster(doc, plan, headServer) {
		return nil, true
	}
	gates := []*gate{{serverID: headServer, position: -1, accepted: map[string]bool{}}}
	for _, m := range plan.Members {
		if m.Server.ID != headServer && !slices.ContainsFunc(gates, func(g *gate) bool { return g.serverID == m.Server.ID }) {
			gates = append(gates, &gate{serverID: m.Server.ID, position: m.Position, accepted: map[string]bool{}})
		}
	}
	tick := time.NewTicker(o.t.poll)
	defer tick.Stop()
	for {
		var pending *gate
		for _, g := range gates {
			reported := o.deps.statuses.AppliedConfigETag(g.serverID)
			if reported != "" && !g.accepted[reported] && reported != g.evaluated {
				if doc, err := o.deps.portal.AgentRuntimeConfig(cctx, g.serverID); err == nil && doc.ETag != "" {
					carries := documentCarriesCluster(doc, plan, g.serverID)
					if !carries && g.serverID == headServer {
						return nil, true
					}
					if carries {
						g.accepted[doc.ETag] = true
					}
					g.evaluated = reported
				}
			}
			if pending == nil && (reported == "" || !g.accepted[reported]) {
				pending = g
			}
		}
		if pending == nil {
			return nil, false
		}
		select {
		case <-cctx.Done():
			if ctx.Err() != nil {
				return &clusterError{Code: codeClusterUnavailable, Cause: codeClusterStartCancelled, Position: -1}, false
			}
			return &clusterError{
				Code: codeClusterStartFailed, Cause: codeClusterConfigUnacknowledged,
				Position: pending.position, Head: pending.position < 0,
				Message: "the agent did not acknowledge a runtime document carrying its part of the cluster",
			}, false
		case <-tick.C:
		}
	}
}

// documentCarriesCluster reports whether doc, serverID's derived document,
// carries serverID's part of plan's cluster.
func documentCarriesCluster(doc portal.AgentRuntimeConfigDTO, plan portal.ClusterPlan, serverID string) bool {
	has := func(pred func(portal.AgentRuntimeSpecDTO) bool) bool { return slices.ContainsFunc(doc.Specs, pred) }
	if serverID == plan.Head.Server.ID && !has(func(s portal.AgentRuntimeSpecDTO) bool {
		return s.ID == plan.Head.Spec.ID && s.Cluster != nil && s.Cluster.Generation == plan.Generation
	}) {
		return false
	}
	for _, m := range plan.Members {
		if m.Server.ID == serverID && !has(func(s portal.AgentRuntimeSpecDTO) bool { return s.ID == m.SpecID }) {
			return false
		}
	}
	return true
}

// startMembers is step two: every member ensured in parallel. The first
// failure cancels the others' calls and is returned.
func (o *clusterOrchestrator) startMembers(ctx context.Context, head string, plan portal.ClusterPlan, agents clusterAgentClient, reserve time.Duration) *clusterError {
	mctx, cancel := context.WithCancel(ctx)
	defer cancel()
	headDown := !o.headOccupied(plan)
	results := make(chan *clusterError, len(plan.Members))
	for _, m := range plan.Members {
		go func() { results <- o.startMember(ctx, mctx, head, plan, m, agents, reserve, headDown) }()
	}
	var first *clusterError
	for range plan.Members {
		if ce := <-results; ce != nil && first == nil {
			first = ce
			cancel()
		}
	}
	return first
}

// errMemberCancelled marks a member call a sibling's failure cancelled.
var errMemberCancelled = &clusterError{Code: codeClusterStartFailed, Position: -1, Message: "cancelled"}

// startMember ensures one member within its bound. runCtx is the start's
// context, mctx the members' (a sibling's failure cancels it).
func (o *clusterOrchestrator) startMember(runCtx, mctx context.Context, head string, plan portal.ClusterPlan, m portal.ClusterPlanMember, agents clusterAgentClient, reserve time.Duration, headDown bool) *clusterError {
	o.setMember(head, m.Position, clusterMemberStarting, "")
	ctx, cancel := context.WithTimeout(mctx, o.startupBound(m.StartupTimeoutSeconds)+o.t.memberMargin)
	defer cancel()
	target := clusterMemberTarget(m)
	failed := func(err error, before runtimeErrorMark) *clusterError {
		if mctx.Err() != nil {
			o.setMember(head, m.Position, clusterMemberPending, "")
			return errMemberCancelled
		}
		cause := o.cause(runCtx, err, m.Server.ID, m.SpecID, before)
		o.setMember(head, m.Position, clusterMemberFailed, cause)
		return &clusterError{Code: codeClusterStartFailed, Cause: cause, Position: m.Position}
	}
	if plan.Transport == clusterTransportDirect && headDown {
		if r, ok := o.row(m.Server.ID, m.SpecID); ok && r.RPC != nil && r.RPC.HeldBy != "" {
			// A direct member that lists a connection while its head is not
			// running holds a dead one, which would refuse the head's next.
			before := o.lastError(m.Server.ID, m.SpecID)
			if _, err := agents.StopRPCMember(o.deps.authCtx(ctx, target), target, m.SpecID, true); err != nil {
				return failed(err, before)
			}
		}
	}
	before := o.lastError(m.Server.ID, m.SpecID)
	if err := agents.EnsureRPCMember(o.deps.authCtx(ctx, target), target, m.SpecID, reserve); err != nil {
		return failed(err, before)
	}
	o.setMember(head, m.Position, clusterMemberReady, "")
	return nil
}

// clusterMemberTarget is the agent target of m's agent router: its server's
// server_agent application with that application's credential.
func clusterMemberTarget(m portal.ClusterPlanMember) routing.Target {
	return routing.Target{
		ServerID:       m.Server.ID,
		Provider:       m.Application.Type,
		Endpoint:       routing.ApplicationEndpoint(m.Server, m.Application),
		Timeout:        time.Duration(m.Application.TimeoutMS) * time.Millisecond,
		APIToken:       m.Application.APIToken,
		APITokenHeader: m.Application.APITokenHeader,
	}
}

// runtimeErrorMark is a spec's last error as a call found it, so a later one
// can be told apart from it.
type runtimeErrorMark struct {
	at       time.Time
	failures int
	message  string
}

func (o *clusterOrchestrator) lastError(serverID, spec string) runtimeErrorMark {
	r, ok := o.row(serverID, spec)
	if !ok || r.LastError == nil {
		return runtimeErrorMark{}
	}
	return runtimeErrorMark{at: r.LastError.At, failures: r.LastError.Failures, message: r.LastError.Message}
}

// cause is the code of a failed agent call: the router's code, or for a
// start that failed, timed out or was not permitted the specific code the
// agent reports on telemetry once a last error newer than before arrives
// (bounded by codeWait). An expired bound is runtime.start_timeout: the
// ensure holds the whole start.
func (o *clusterOrchestrator) cause(ctx context.Context, err error, serverID, spec string, before runtimeErrorMark) string {
	code := codeRuntimeAgentError
	var re *provider.RouterError
	switch {
	case errors.As(err, &re):
		code = re.Code
	case errors.Is(err, provider.ErrTimeout):
		code = codeRuntimeStartTimeout
	case errors.Is(err, provider.ErrUnavailable):
		code = codeRuntimeAgentUnavailable
	}
	if !telemetryDetailedCauses[code] {
		return code
	}
	if detail := o.awaitErrorCode(ctx, serverID, spec, before); detail != "" {
		return detail
	}
	return code
}

// awaitErrorCode waits up to codeWait for spec's row on serverID to carry a
// last error newer than before with a code, and returns that code ("" when
// none arrives).
func (o *clusterOrchestrator) awaitErrorCode(ctx context.Context, serverID, spec string, before runtimeErrorMark) string {
	rows, updates, unsubscribe := o.deps.statuses.subscribe(serverID)
	defer unsubscribe()
	timer := time.NewTimer(o.t.codeWait)
	defer timer.Stop()
	for {
		for i := range rows {
			r := &rows[i]
			if r.SpecID != spec || r.Instance != 0 || r.LastError == nil || r.LastError.Code == "" {
				continue
			}
			e := r.LastError
			if !e.At.Equal(before.at) || e.Failures != before.failures || e.Message != before.message {
				return e.Code
			}
		}
		select {
		case next, open := <-updates:
			if !open {
				return ""
			}
			rows = next
		case <-timer.C:
			return ""
		case <-ctx.Done():
			return ""
		}
	}
}

// holdWatch follows the member servers' telemetry from before the head's
// start: only frames that arrive after it began count as formation evidence,
// so a hold a frame reported earlier -- a previous run's -- is never taken
// for this one.
type holdWatch struct {
	frames chan serverFrame
	stop   func()
}

type serverFrame struct {
	serverID string
	rows     []RuntimeStatusDTO
}

func (o *clusterOrchestrator) watchHolds(ctx context.Context, plan portal.ClusterPlan) *holdWatch {
	wctx, cancel := context.WithCancel(ctx)
	frames := make(chan serverFrame, runtimeStatusSubBuffer)
	var wg sync.WaitGroup
	var servers []string
	for _, m := range plan.Members {
		if !slices.Contains(servers, m.Server.ID) {
			servers = append(servers, m.Server.ID)
		}
	}
	for _, id := range servers {
		_, updates, unsubscribe := o.deps.statuses.subscribe(id)
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer unsubscribe()
			for {
				select {
				case rows, open := <-updates:
					if !open {
						return
					}
					select {
					case frames <- serverFrame{serverID: id, rows: rows}:
					case <-wctx.Done():
						return
					}
				case <-wctx.Done():
					return
				}
			}
		}()
	}
	return &holdWatch{frames: frames, stop: func() { cancel(); wg.Wait() }}
}

// await is step four: it returns once every member's latest fresh frame shows
// head's hold (formed true), or when ctx ends, with the position of the first
// member not held.
func (w *holdWatch) await(ctx context.Context, head string, plan portal.ClusterPlan) (int, bool) {
	held := make([]bool, len(plan.Members))
	firstUnheld := func() int {
		for i, h := range held {
			if !h {
				return plan.Members[i].Position
			}
		}
		return -1
	}
	if len(plan.Members) == 0 {
		return -1, true
	}
	for {
		select {
		case f := <-w.frames:
			for i, m := range plan.Members {
				if m.Server.ID != f.serverID {
					continue
				}
				held[i] = false
				for _, r := range f.rows {
					if r.SpecID == m.SpecID && r.RPC != nil && r.RPC.HeldBy == head {
						held[i] = true
					}
				}
			}
			if firstUnheld() < 0 {
				return -1, true
			}
		case <-ctx.Done():
			return firstUnheld(), false
		}
	}
}

// stopHead drains the head after a failed formation, bounded; a failure is
// logged, since the start fails either way and the head then idles out.
func (o *clusterOrchestrator) stopHead(ctx context.Context, plan portal.ClusterPlan, agents clusterAgentClient) {
	sctx, cancel := context.WithTimeout(ctx, runtimeStopTimeout)
	defer cancel()
	target := runtimeInstanceTarget(plan.Head, 0)
	if _, err := agents.StopRuntimeModel(o.deps.authCtx(sctx, target), target); err != nil {
		slog.Warn("cluster start: stopping the head of an unformed cluster failed", "head_spec_id", plan.Head.Spec.ID, "err", err)
	}
}

// Stop ends head's cluster head first: a running start of it is cancelled,
// the head is drained and the stop waits, bounded, for the head's stopped
// state on its server's status stream; then, with stopMembers, each member's
// reservation and hold are released and the member stopped. Without
// stopMembers the members idle out on their short timeout once the head's
// holds end.
func (o *clusterOrchestrator) Stop(ctx context.Context, head string, stopMembers bool) error {
	if o == nil {
		return &clusterError{Code: codeClusterUnavailable, Position: -1, Message: "this gateway runs no cluster orchestration"}
	}
	o.mu.Lock()
	st := o.headState(head)
	st.stopping++
	s := st.start
	o.mu.Unlock()
	defer func() {
		o.mu.Lock()
		o.headState(head).stopping--
		o.mu.Unlock()
	}()
	if s != nil {
		s.cancel()
		if err := joinClusterStart(ctx, s); ctx.Err() != nil {
			return err
		}
	}
	plan, err := o.deps.portal.ClusterPlan(ctx, head)
	if err != nil {
		return err
	}
	agents := o.deps.agents()
	if agents == nil {
		return fmt.Errorf("%w: this gateway's provider cannot stop a cluster through the agents", provider.ErrUnavailable)
	}
	if err := o.drainHead(ctx, plan, agents); err != nil {
		return err
	}
	o.mu.Lock()
	st.formed, st.phase = false, ""
	for i := range st.members {
		st.members[i].State, st.members[i].Code = clusterMemberPending, ""
	}
	o.mu.Unlock()
	if !stopMembers {
		return nil
	}
	var first error
	for _, m := range plan.Members {
		if err := o.stopMember(ctx, m, agents); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// StopManual is the operator's stop of head: Stop, and the head stays down
// -- the reconciler does not start a pinned or force_running head an
// operator stopped -- until a manual start or a gateway restart.
func (o *clusterOrchestrator) StopManual(ctx context.Context, head string, stopMembers bool) error {
	if o != nil {
		o.mu.Lock()
		o.headState(head).parked = true
		o.mu.Unlock()
	}
	return o.Stop(ctx, head, stopMembers)
}

// drainHead asks the head's agent to stop the head and waits, bounded by
// stopWait, until its status shows no process. It subscribes before it asks,
// so the frame that reports the stop is not missed.
func (o *clusterOrchestrator) drainHead(ctx context.Context, plan portal.ClusterPlan, agents clusterAgentClient) error {
	rows, updates, unsubscribe := o.deps.statuses.subscribe(plan.Head.Server.ID)
	defer unsubscribe()
	sctx, cancel := context.WithTimeout(ctx, runtimeStopTimeout)
	target := runtimeInstanceTarget(plan.Head, 0)
	state, err := agents.StopRuntimeModel(o.deps.authCtx(sctx, target), target)
	cancel()
	if err != nil || state == provider.RuntimeStopStopped {
		return err
	}
	timer := time.NewTimer(o.t.stopWait)
	defer timer.Stop()
	for !headGone(rows, plan.Head.Spec.ID) {
		select {
		case next, open := <-updates:
			if !open {
				updates = nil
				continue
			}
			rows = next
		case <-timer.C:
			return fmt.Errorf("%w: the cluster head did not stop within %s", provider.ErrTimeout, o.t.stopWait)
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

// headGone reports whether rows show head's instance 0 without a process.
func headGone(rows []RuntimeStatusDTO, head string) bool {
	for i := range rows {
		r := &rows[i]
		if r.SpecID == head && r.Instance == 0 {
			return !stateOccupied(r.State)
		}
	}
	return true
}

// stopMember releases m's reservation and hold and stops it, each call
// bounded.
func (o *clusterOrchestrator) stopMember(ctx context.Context, m portal.ClusterPlanMember, agents clusterAgentClient) error {
	target := clusterMemberTarget(m)
	rctx, cancel := context.WithTimeout(ctx, runtimeStopTimeout)
	err := agents.ReleaseRPCMember(o.deps.authCtx(rctx, target), target, m.SpecID)
	cancel()
	if err != nil {
		return err
	}
	sctx, cancel := context.WithTimeout(ctx, runtimeStopTimeout)
	defer cancel()
	_, err = agents.StopRPCMember(o.deps.authCtx(sctx, target), target, m.SpecID, false)
	return err
}

// Trigger wakes the reconciler: a runtime document was pushed, or a head
// exited. Coalesced: one pending wake is enough.
func (o *clusterOrchestrator) Trigger() {
	if o == nil {
		return
	}
	select {
	case o.trigger <- struct{}{}:
	default:
	}
}

// TriggerChannel is what the reconciler loop waits on beside its tick.
func (o *clusterOrchestrator) TriggerChannel() <-chan struct{} {
	if o == nil {
		return nil
	}
	return o.trigger
}

// observeStatus reads one telemetry frame of serverID against the previous
// one: a cluster head that had a process with its launch facts and has none
// now (it exited) is no longer formed, and the reconciler is woken, since a
// kept-up head must be started again members first. A head that is only
// draining has not exited yet.
func (o *clusterOrchestrator) observeStatus(serverID string, prev, next []RuntimeStatusDTO) {
	if o == nil {
		return
	}
	var exited []string
	for i := range prev {
		p := &prev[i]
		if p.Cluster == nil || p.Instance != 0 || !stateOccupied(p.State) {
			continue
		}
		// A draining head still has its process and holds its members; it
		// has exited once no process is left.
		j := slices.IndexFunc(next, func(r RuntimeStatusDTO) bool { return r.SpecID == p.SpecID && r.Instance == 0 })
		if j < 0 || !stateOccupied(next[j].State) {
			exited = append(exited, p.SpecID)
		}
	}
	if len(exited) == 0 {
		return
	}
	o.mu.Lock()
	for _, head := range exited {
		if st := o.heads[head]; st != nil && st.formed {
			st.formed, st.phase = false, ""
			for i := range st.members {
				st.members[i].State, st.members[i].Code = clusterMemberPending, ""
			}
		}
	}
	o.mu.Unlock()
	slog.Info("cluster head exited", "server_id", serverID, "head_spec_ids", exited)
	o.Trigger()
}

// Reconcile is one reconciler pass. It lifts a terminal block whose head
// document changed, and starts every kept-up head (pinned or force_running)
// that is cold -- except one an operator stopped, and, during the boot grace,
// one whose servers have not all reported, since the gateway cannot yet tell
// whether that cluster still runs. Each start runs in the background, bounded
// by its own bounds and the server's lifetime; the pass itself only reads.
func (o *clusterOrchestrator) Reconcile(ctx context.Context) {
	if o == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, clusterReadTimeout)
	defer cancel()
	o.liftChangedBlocks(ctx)
	heads, err := o.deps.portal.KeptUpClusterHeads(ctx)
	if err != nil {
		slog.Warn("cluster reconcile: listing the kept-up heads failed", "err", err)
		return
	}
	base := o.deps.base()
	for _, head := range heads {
		o.mu.Lock()
		st := o.heads[head]
		state, parked, terminal := o.stateLocked(st), st != nil && (st.parked || st.stopping > 0), st != nil && st.terminal
		o.mu.Unlock()
		if parked || (state != clusterCold && (state != clusterInvalid || terminal)) {
			continue
		}
		plan, err := o.plan(ctx, head)
		if err != nil {
			continue
		}
		if o.inBootGrace() && !o.clusterReported(plan) {
			continue
		}
		o.wg.Add(1)
		go func() {
			defer o.wg.Done()
			if err := o.Ensure(base, head); err != nil {
				slog.Debug("cluster reconcile: start of a kept-up head did not form", "head_spec_id", head, "err", err)
			}
		}()
	}
}

// liftChangedBlocks re-reads every head that is invalid: one the runtime
// documents withheld is valid again once they carry it, a terminal block is
// cleared once the head's document fingerprint changed, and a head whose spec
// is gone is forgotten. Without it a head that is not kept up would stay
// invalid for good, since routing never asks an invalid head to start.
func (o *clusterOrchestrator) liftChangedBlocks(ctx context.Context) {
	o.mu.Lock()
	var blocked []string
	for head, st := range o.heads {
		if st.start == nil && (st.terminal || st.withheld != "") {
			blocked = append(blocked, head)
		}
	}
	o.mu.Unlock()
	for _, head := range blocked {
		plan, err := o.deps.portal.ClusterPlan(ctx, head)
		if errors.Is(err, portal.ErrRuntimeSpecNotFound) {
			o.mu.Lock()
			delete(o.heads, head)
			o.mu.Unlock()
			continue
		}
		if err != nil {
			continue
		}
		o.mu.Lock()
		o.headState(head).withheld = plan.WithheldReason
		terminal := o.heads[head].terminal
		o.mu.Unlock()
		if !terminal || plan.WithheldReason != "" {
			continue
		}
		fp := o.fingerprint(ctx, plan)
		o.mu.Lock()
		if st := o.heads[head]; st != nil && st.terminal && fp != "" && fp != st.terminalPrint {
			st.terminal, st.terminalPrint, st.failErr, st.cooldownUntil = false, "", nil, time.Time{}
		}
		o.mu.Unlock()
	}
}

func (o *clusterOrchestrator) inBootGrace() bool {
	return o.deps.now().Before(o.bootAt.Add(o.t.bootGrace))
}

// clusterReported reports whether every server of plan has reported its
// runtime status since the gateway started.
func (o *clusterOrchestrator) clusterReported(plan portal.ClusterPlan) bool {
	if o.deps.statuses.statusRows(plan.Head.Server.ID) == nil {
		return false
	}
	for _, m := range plan.Members {
		if o.deps.statuses.statusRows(m.Server.ID) == nil {
			return false
		}
	}
	return true
}
