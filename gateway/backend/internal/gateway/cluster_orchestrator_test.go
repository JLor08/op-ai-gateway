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
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/provider"
	"op-ai-gateway/internal/routing"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// The cluster orchestration's tests drive the real orchestrator over a
// scripted portal (orchPortal: the plan, the derived documents, the kept-up
// heads) and a scripted agent fleet (orchAgent: every ensure, stop, release),
// with the real runtime-status registry carrying the telemetry.

const (
	orchHead       = "rspec_head"
	orchHeadServer = "srv_head"
	orchHeadModel  = "big-model"
)

func orchMemberServer(i int) string { return fmt.Sprintf("srv_m%d", i) }
func orchMemberSpec(i int) string   { return fmt.Sprintf("rpca_%s_%s", orchHead, orchMemberServer(i)) }

// orchTimings are the orchestrator's bounds scaled down: a startup second is
// 10 ms, so the head's 10 s startup is 100 ms and a member's 5 s is 50 ms.
func orchTimings() clusterTimings {
	return clusterTimings{
		unit: 10 * time.Millisecond, configWait: 300 * time.Millisecond, poll: 5 * time.Millisecond,
		cooldown: 60 * time.Second, codeWait: 200 * time.Millisecond, stopWait: 300 * time.Millisecond,
		memberMargin: 20 * time.Millisecond, headMargin: 50 * time.Millisecond, reserveMargin: 60 * time.Millisecond,
		bootGrace: 60 * time.Second, minAgentInterval: time.Second, staleFloor: 15 * time.Second, blockedRetry: 10 * time.Second, releaseRetry: 30 * time.Second,
		sessionWait: 50 * time.Millisecond, planRetry: 5 * time.Second, policySettle: 200 * time.Millisecond,
	}
}

// orchHeadBound is the head's bound under orchTimings: 10 units + the margin.
const orchHeadBound = 150 * time.Millisecond

// orchPortal is the scripted portal: one head's plan, a derived document per
// server whose ETag changes with every edit, and the kept-up heads.
type orchPortal struct {
	mu         sync.Mutex
	plan       portal.ClusterPlan
	headArgs   []string
	version    int
	reconciles int
	kept       []string
	// planReads counts ClusterPlan calls; afterPlan, when set, runs once
	// after a read was answered (a write that lands right after it).
	planReads int
	afterPlan func(p *orchPortal)
	// reconcileGate, when set, holds the policy pass until it closes (a pass
	// stuck behind another on the policy lock; it ignores ctx). writes is
	// what the pass reports as still settling: member policy writes, by it or
	// an earlier pass, with their times.
	reconcileGate chan struct{}
	writes        []portal.RPCPolicyWrite
	// withheld, when set, is the derivation's reason: the plan is read as
	// withheld (no members, no network or transport), as the portal answers.
	withheld string
	// keptCtxErr is the context error the last kept-up listing saw;
	// keptReads counts the listings.
	keptCtxErr error
	keptReads  int
	// planErr, when set, fails every plan read (a store outage); gone makes
	// the head read as not found (deleted, or no cluster head any more).
	planErr error
	gone    bool
}

func (p *orchPortal) ClusterPlan(_ context.Context, headSpecID string) (portal.ClusterPlan, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if headSpecID != p.plan.Head.Spec.ID || p.gone {
		return portal.ClusterPlan{}, portal.ErrRuntimeSpecNotFound
	}
	p.planReads++
	if p.planErr != nil {
		return portal.ClusterPlan{}, p.planErr
	}
	plan := p.plan
	plan.Members = slices.Clone(p.plan.Members)
	if p.withheld != "" {
		plan.WithheldReason, plan.Network, plan.Transport, plan.Generation = p.withheld, "", "", ""
		plan.Members = []portal.ClusterPlanMember{}
	}
	if hook := p.afterPlan; hook != nil {
		p.afterPlan = nil
		hook(p)
	}
	return plan, nil
}

func (p *orchPortal) etag(serverID string) string {
	return fmt.Sprintf("etag-%s-%d", serverID, p.version)
}

func (p *orchPortal) AgentRuntimeConfig(_ context.Context, serverID string) (portal.AgentRuntimeConfigDTO, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	doc := portal.AgentRuntimeConfigDTO{ETag: p.etag(serverID), Specs: []portal.AgentRuntimeSpecDTO{}}
	if serverID == p.plan.Head.Server.ID {
		doc.Specs = append(doc.Specs, portal.AgentRuntimeSpecDTO{
			ID: p.plan.Head.Spec.ID, Args: slices.Clone(p.headArgs),
			Cluster: &portal.AgentClusterHeadDTO{Transport: p.plan.Transport, Network: p.plan.Network, Generation: p.plan.Generation},
		})
	}
	for _, m := range p.plan.Members {
		if m.Server.ID == serverID {
			doc.Specs = append(doc.Specs, portal.AgentRuntimeSpecDTO{ID: m.SpecID, Kind: portal.AgentSpecKindRPCServer})
		}
	}
	return doc, nil
}

func (p *orchPortal) ReconcileRPCPolicies(context.Context) []portal.RPCPolicyWrite {
	p.mu.Lock()
	p.reconciles++
	gate, writes := p.reconcileGate, slices.Clone(p.writes)
	p.mu.Unlock()
	if gate != nil {
		<-gate
	}
	return writes
}

func (p *orchPortal) KeptUpClusterHeads(ctx context.Context) ([]string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.keptCtxErr = ctx.Err()
	p.keptReads++
	return slices.Clone(p.kept), nil
}

// changeHead edits the head's launch: a new document on every server.
func (p *orchPortal) changeHead() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.headArgs = append(p.headArgs, "--changed")
	p.version++
}

func (p *orchPortal) servers() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	ids := []string{p.plan.Head.Server.ID}
	for _, m := range p.plan.Members {
		ids = append(ids, m.Server.ID)
	}
	return ids
}

// orchAgent is the scripted agent fleet. calls records, in order:
// "member:ensure:<spec>", "head:ensure:<model>:<mark>",
// "head:stop:<model>[:if_evictable]", "member:release:<spec>",
// "member:stop:<spec>[:force]".
type orchAgent struct {
	mu           sync.Mutex
	calls        []string
	reserves     map[string]time.Duration
	memberErr    map[string]error
	memberGate   map[string]chan struct{}
	headErr      error
	headStopErr  error
	releaseGate  chan struct{}
	stopState    string
	arrived      chan string
	onHeadEnsure func()
	onHeadStop   func()
}

func newOrchAgent() *orchAgent {
	return &orchAgent{
		reserves: map[string]time.Duration{}, memberErr: map[string]error{}, memberGate: map[string]chan struct{}{},
		stopState: provider.RuntimeStopStopped, arrived: make(chan string, 64),
	}
}

func (a *orchAgent) record(call string) {
	a.mu.Lock()
	a.calls = append(a.calls, call)
	a.mu.Unlock()
	a.arrived <- call
}

func (a *orchAgent) recorded() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.calls)
}

func (a *orchAgent) count(prefix string) int {
	n := 0
	for _, c := range a.recorded() {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

func (a *orchAgent) EnsureRuntimeModel(ctx context.Context, target routing.Target) error {
	mark := "plain"
	if provider.IsClusterStart(ctx) {
		mark = "cluster"
	}
	a.record("head:ensure:" + target.ProviderModel + ":" + mark)
	a.mu.Lock()
	hook, err := a.onHeadEnsure, a.headErr
	a.mu.Unlock()
	if hook != nil {
		hook()
	}
	return err
}

func (a *orchAgent) StopRuntimeModel(ctx context.Context, target routing.Target) (string, error) {
	call := "head:stop:" + target.ProviderModel
	if provider.IsIfEvictable(ctx) {
		call += ":if_evictable"
	}
	a.record(call)
	a.mu.Lock()
	hook, state, err := a.onHeadStop, a.stopState, a.headStopErr
	a.mu.Unlock()
	if err != nil {
		return "", err
	}
	if hook != nil {
		hook()
	}
	return state, nil
}

func (a *orchAgent) EnsureRPCMember(ctx context.Context, _ routing.Target, specID string, reserve time.Duration) error {
	a.mu.Lock()
	a.reserves[specID] = reserve
	gate, err := a.memberGate[specID], a.memberErr[specID]
	a.mu.Unlock()
	a.record("member:ensure:" + specID)
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return fmt.Errorf("%w: ensure route: %w", provider.ErrTimeout, context.Cause(ctx))
		}
	}
	return err
}

func (a *orchAgent) ReleaseRPCMember(ctx context.Context, _ routing.Target, specID string) error {
	a.record("member:release:" + specID)
	a.mu.Lock()
	gate := a.releaseGate
	a.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

func (a *orchAgent) StopRPCMember(_ context.Context, _ routing.Target, specID string, force bool) (string, error) {
	call := "member:stop:" + specID
	if force {
		call += ":force"
	}
	a.record(call)
	return provider.RuntimeStopStopping, nil
}

// orchFixture is one orchestrator over orchPortal and orchAgent with a head
// on srv_head and n members on srv_m0.., a clock the test moves.
type orchFixture struct {
	t        *testing.T
	o        *clusterOrchestrator
	portal   *orchPortal
	agent    *orchAgent
	statuses *runtimeStatusRegistry

	clockMu sync.Mutex
	now     time.Time
}

func newOrchFixture(t *testing.T, members int, transport, network string) *orchFixture {
	t.Helper()
	plan := portal.ClusterPlan{
		Head: portal.RuntimeActionScope{
			Server:      routing.AIServer{ID: orchHeadServer, Domain: "head.example.test"},
			Application: routing.Application{ID: "app_head", ServerID: orchHeadServer, Type: routing.ProviderServerAgent, Port: 8081},
			Mapping:     routing.ModelMapping{ID: "map_head", GatewayModelName: orchHeadModel, AppModelName: orchHeadModel},
			Spec:        routing.RuntimeSpec{ID: orchHead, MappingID: "map_head", Enabled: true, ClusterEnabled: true, StartupTimeoutSeconds: 10},
		},
		Network: network, Transport: transport, Generation: "gen1",
	}
	for i := range members {
		plan.Members = append(plan.Members, portal.ClusterPlanMember{
			Position: i, Server: routing.AIServer{ID: orchMemberServer(i), Domain: orchMemberServer(i) + ".example.test"},
			Application: routing.Application{ID: "app_" + orchMemberServer(i), ServerID: orchMemberServer(i), Type: routing.ProviderServerAgent, Port: 8081},
			SpecID:      orchMemberSpec(i), StartupTimeoutSeconds: 5,
		})
	}
	f := &orchFixture{
		t: t, portal: &orchPortal{plan: plan, headArgs: []string{"-m", "x"}}, agent: newOrchAgent(),
		statuses: newRuntimeStatusRegistry(), now: time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC),
	}
	f.o = newClusterOrchestrator(clusterOrchestratorDeps{
		portal:   f.portal,
		agents:   func() clusterAgentClient { return f.agent },
		statuses: f.statuses,
		authCtx:  func(ctx context.Context, _ routing.Target) context.Context { return ctx },
		base:     context.Background,
		now:      f.clock,
	}, orchTimings())
	t.Cleanup(f.o.wait)
	return f
}

func (f *orchFixture) clock() time.Time {
	f.clockMu.Lock()
	defer f.clockMu.Unlock()
	return f.now
}

func (f *orchFixture) advance(d time.Duration) {
	f.clockMu.Lock()
	defer f.clockMu.Unlock()
	f.now = f.now.Add(d)
}

// ackAll has every server's agent acknowledge its current document.
func (f *orchFixture) ackAll() {
	for _, id := range f.portal.servers() {
		f.ack(id)
	}
}

func (f *orchFixture) ack(serverID string) {
	f.portal.mu.Lock()
	etag := f.portal.etag(serverID)
	f.portal.mu.Unlock()
	f.statuses.SetAppliedConfigETag(serverID, etag)
}

func orchMemberRow(spec, heldBy string) RuntimeStatusDTO {
	return RuntimeStatusDTO{SpecID: spec, State: agentSpecStateRunning, Kind: "rpc_server", RPC: &RuntimeRPCStatusDTO{HeldBy: heldBy, DeviceCount: 1}}
}

// hold publishes, for each given member index, a frame that shows it held
// by the head.
func (f *orchFixture) hold(members ...int) {
	f.portal.mu.Lock()
	head := f.portal.plan.Head.Spec.ID
	f.portal.mu.Unlock()
	for _, i := range members {
		f.statuses.publish(orchMemberServer(i), []RuntimeStatusDTO{orchMemberRow(orchMemberSpec(i), head)})
	}
}

// holdOnHeadEnsure makes the head's ensure publish a hold of every given
// member, as a head that attached them does.
func (f *orchFixture) holdOnHeadEnsure(members ...int) {
	f.agent.mu.Lock()
	defer f.agent.mu.Unlock()
	f.agent.onHeadEnsure = func() { f.hold(members...) }
}

func clusterErrOf(t *testing.T, err error) *clusterError {
	t.Helper()
	var ce *clusterError
	if !errors.As(err, &ce) {
		t.Fatalf("err = %v (%T), want a *clusterError", err, err)
	}
	return ce
}

// State is head's gate state. A head the orchestration never saw is cold.
func (o *clusterOrchestrator) State(head string) clusterGateState {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.stateLocked(o.heads[head])
}

// joiners reports how many Ensure calls joined head's running start.
func (o *clusterOrchestrator) joiners(head string) int {
	o.mu.Lock()
	defer o.mu.Unlock()
	if st := o.heads[head]; st != nil && st.start != nil {
		return st.start.joined
	}
	return 0
}

func orchRefusal(status int, code string) error {
	return provider.RouterStatusError(status, []byte(`{"error":{"code":"`+code+`","message":"`+code+`"}}`))
}

// TestEnsureClusterMembersBeforeHead pins the order of a start: every member
// is ensured (with its reservation) before the head, the head is ensured once
// with the cluster-start mark, and the cluster is formed once every member
// reports the head's hold.
func TestEnsureClusterMembersBeforeHead(t *testing.T) {
	f := newOrchFixture(t, 2, "relay", "lan")
	f.ackAll()
	f.holdOnHeadEnsure(0, 1)

	if err := f.o.Ensure(context.Background(), orchHead); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	calls := f.agent.recorded()
	if len(calls) != 3 {
		t.Fatalf("calls = %q, want two member ensures then the head's", calls)
	}
	members := slices.Sorted(slices.Values(calls[:2]))
	if !slices.Equal(members, []string{"member:ensure:" + orchMemberSpec(0), "member:ensure:" + orchMemberSpec(1)}) {
		t.Fatalf("first calls = %q, want both member ensures", calls[:2])
	}
	if calls[2] != "head:ensure:"+orchHeadModel+":cluster" {
		t.Fatalf("last call = %q, want the head's cluster-start ensure", calls[2])
	}
	wantReserve := orchHeadBound + 60*time.Millisecond
	for i := range 2 {
		if got := f.agent.reserves[orchMemberSpec(i)]; got != wantReserve {
			t.Fatalf("member %d reservation = %v, want the head's bound plus the margin (%v)", i, got, wantReserve)
		}
	}
	if st := f.o.State(orchHead); st != clusterFormed {
		t.Fatalf("state = %v, want formed", st)
	}
	status := f.o.ClusterStatus(orchHead)
	if status.State != portal.ClusterStateFormed || status.Phase != portal.ClusterPhaseFormed || len(status.Members) != 2 ||
		status.Members[0].State != clusterMemberHeld || status.Members[1].State != clusterMemberHeld {
		t.Fatalf("status = %+v, want formed with both members held", status)
	}
}

// TestEnsureClusterRestartsHeldDirectMember pins the direct-mode rule: a
// member that still reports a hold while its head is not running holds a dead
// connection, so it is force-stopped before it is ensured again.
func TestEnsureClusterRestartsHeldDirectMember(t *testing.T) {
	f := newOrchFixture(t, 2, "direct", "lan")
	f.ackAll()
	f.statuses.publish(orchMemberServer(1), []RuntimeStatusDTO{orchMemberRow(orchMemberSpec(1), orchHead)})
	f.holdOnHeadEnsure(0, 1)

	if err := f.o.Ensure(context.Background(), orchHead); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	calls := f.agent.recorded()
	stop := slices.Index(calls, "member:stop:"+orchMemberSpec(1)+":force")
	ensure := slices.Index(calls, "member:ensure:"+orchMemberSpec(1))
	if stop < 0 || ensure < stop {
		t.Fatalf("calls = %q, want member 1 force-stopped before its ensure", calls)
	}
	if f.agent.count("member:stop:"+orchMemberSpec(0)) != 0 {
		t.Fatalf("calls = %q, want member 0 (no hold) left alone", calls)
	}
}

// TestEnsureClusterOneStartPerHead pins the single flight: an Ensure while a
// start of the same head runs joins it -- one sequence of agent calls, one
// outcome for both.
func TestEnsureClusterOneStartPerHead(t *testing.T) {
	f := newOrchFixture(t, 2, "relay", "lan")
	f.ackAll()
	f.holdOnHeadEnsure(0, 1)
	gate := make(chan struct{})
	f.agent.memberGate[orchMemberSpec(0)] = gate
	f.portal.plan.Members[0].StartupTimeoutSeconds = 500 // the gate, not the bound, decides

	errs := make(chan error, 2)
	go func() { errs <- f.o.Ensure(context.Background(), orchHead) }()
	for call := range f.agent.arrived {
		if call == "member:ensure:"+orchMemberSpec(0) {
			break
		}
	}
	go func() { errs <- f.o.Ensure(context.Background(), orchHead) }()
	deadline := time.Now().Add(time.Second)
	for f.o.joiners(orchHead) < 1 {
		if time.Now().After(deadline) {
			t.Fatal("the second Ensure never joined the running start")
		}
		time.Sleep(time.Millisecond)
	}
	close(gate)
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatalf("Ensure: %v", err)
		}
	}
	if n := f.agent.count("member:ensure:"); n != 2 {
		t.Fatalf("member ensures = %d (%q), want one per member", n, f.agent.recorded())
	}
	if n := f.agent.count("head:ensure:"); n != 1 {
		t.Fatalf("head ensures = %d, want one", n)
	}
}

// TestEnsureClusterMemberRefusedCoolsDown pins a member's refusal: the head
// is never asked, the start fails with runtime.cluster_start_failed naming
// the member's position and the agent's code, and the head cools down -- an
// automatic Ensure inside the cooldown asks nobody -- until the cooldown ends.
func TestEnsureClusterMemberRefusedCoolsDown(t *testing.T) {
	f := newOrchFixture(t, 2, "relay", "lan")
	f.ackAll()
	f.agent.memberErr[orchMemberSpec(1)] = orchRefusal(http.StatusServiceUnavailable, "runtime.admission_blocked")

	ce := clusterErrOf(t, f.o.Ensure(context.Background(), orchHead))
	if ce.Code != codeClusterStartFailed || ce.Cause != "runtime.admission_blocked" || ce.Position != 1 || ce.Head {
		t.Fatalf("error = %+v, want cluster_start_failed for member 1 with admission_blocked", ce)
	}
	if n := f.agent.count("head:"); n != 0 {
		t.Fatalf("head calls = %d, want none after a member failed", n)
	}
	if st := f.o.State(orchHead); st != clusterCoolingDown {
		t.Fatalf("state = %v, want cooling down", st)
	}
	status := f.o.ClusterStatus(orchHead)
	if status.State != portal.ClusterStateCoolingDown || status.LastFailure == nil || status.LastFailure.Position == nil ||
		*status.LastFailure.Position != 1 || status.LastFailure.Code != "runtime.admission_blocked" ||
		status.CooldownUntil == nil || !status.CooldownUntil.Equal(f.clock().Add(60*time.Second)) ||
		status.Members[1].State != clusterMemberFailed || status.Members[1].Code != "runtime.admission_blocked" {
		t.Fatalf("status = %+v, want the cooldown naming member 1", status)
	}

	f.o.wait() // the failed start's member releases
	before := len(f.agent.recorded())
	ce = clusterErrOf(t, f.o.Ensure(context.Background(), orchHead))
	if ce.Code != codeClusterStartFailed || len(f.agent.recorded()) != before {
		t.Fatalf("Ensure in the cooldown = %+v with calls %q, want the cooldown's error and no call", ce, f.agent.recorded()[before:])
	}

	f.advance(61 * time.Second)
	delete(f.agent.memberErr, orchMemberSpec(1))
	f.holdOnHeadEnsure(0, 1)
	if err := f.o.Ensure(context.Background(), orchHead); err != nil {
		t.Fatalf("Ensure after the cooldown: %v", err)
	}
	if f.o.State(orchHead) != clusterFormed {
		t.Fatalf("state = %v, want formed after the retry", f.o.State(orchHead))
	}
}

// TestEnsureClusterMemberTimeout pins a member's bound: its startup timeout
// plus the margin. A member that does not answer within it fails the start
// as runtime.start_timeout naming its position, and the head is never asked.
func TestEnsureClusterMemberTimeout(t *testing.T) {
	f := newOrchFixture(t, 2, "relay", "lan")
	f.ackAll()
	f.agent.memberGate[orchMemberSpec(0)] = make(chan struct{}) // never answers

	began := time.Now()
	ce := clusterErrOf(t, f.o.Ensure(context.Background(), orchHead))
	elapsed := time.Since(began)
	if ce.Code != codeClusterStartFailed || ce.Cause != codeRuntimeStartTimeout || ce.Position != 0 {
		t.Fatalf("error = %+v, want cluster_start_failed for member 0 with start_timeout", ce)
	}
	if elapsed < 70*time.Millisecond || elapsed > 2*time.Second {
		t.Fatalf("the start failed after %v, want the member's bound (70 ms)", elapsed)
	}
	if n := f.agent.count("head:"); n != 0 {
		t.Fatalf("head calls = %d, want none", n)
	}
}

// TestEnsureClusterFormationTimeoutStopsHead pins formation: a member that
// never reports the head's hold within the head's bound fails the start with
// runtime.cluster_member_unused naming it, and the head is stopped.
func TestEnsureClusterFormationTimeoutStopsHead(t *testing.T) {
	f := newOrchFixture(t, 2, "relay", "lan")
	f.ackAll()
	f.holdOnHeadEnsure(0) // member 1 is never attached

	ce := clusterErrOf(t, f.o.Ensure(context.Background(), orchHead))
	if ce.Code != codeClusterMemberUnused || ce.Position != 1 {
		t.Fatalf("error = %+v, want cluster_member_unused naming member 1", ce)
	}
	f.o.wait()
	calls := f.agent.recorded()
	stop := slices.Index(calls, "head:stop:"+orchHeadModel)
	if stop < 0 || slices.ContainsFunc(calls[stop+1:], func(c string) bool { return !strings.HasPrefix(c, "member:release:") }) {
		t.Fatalf("calls = %q, want the head stopped, followed by the member releases only", calls)
	}
	if st := f.o.State(orchHead); st != clusterCoolingDown {
		t.Fatalf("state = %v, want cooling down", st)
	}
}

// TestEnsureClusterTerminalCodeStopsRetries pins the terminal causes: a head
// whose start fails with a code the agent reports only on telemetry (here
// runtime.rpc_unsupported_build) is not retried automatically -- past the
// cooldown too -- until the head's configuration changes.
func TestEnsureClusterTerminalCodeStopsRetries(t *testing.T) {
	f := newOrchFixture(t, 1, "relay", "lan")
	f.ackAll()
	f.agent.headErr = orchRefusal(http.StatusBadGateway, "runtime.start_failed")
	f.agent.onHeadEnsure = func() {
		f.statuses.publish(orchHeadServer, []RuntimeStatusDTO{{
			SpecID: orchHead, Model: orchHeadModel, State: "start_failed",
			LastError: &RuntimeErrorDTO{Message: "RPC not supported", At: time.Now(), Failures: 1, Code: codeRPCUnsupportedBuild},
		}})
	}

	ce := clusterErrOf(t, f.o.Ensure(context.Background(), orchHead))
	if ce.Code != codeClusterStartFailed || ce.Cause != codeRPCUnsupportedBuild || !ce.Head {
		t.Fatalf("error = %+v, want cluster_start_failed for the head with rpc_unsupported_build", ce)
	}
	if st := f.o.State(orchHead); st != clusterInvalid {
		t.Fatalf("state = %v, want invalid", st)
	}

	f.o.wait() // the failed start's member releases
	f.advance(10 * time.Minute)
	before := len(f.agent.recorded())
	ce = clusterErrOf(t, f.o.Ensure(context.Background(), orchHead))
	if ce.Cause != codeRPCUnsupportedBuild || len(f.agent.recorded()) != before {
		t.Fatalf("Ensure past the cooldown = %+v with calls %q, want the terminal error and no call", ce, f.agent.recorded()[before:])
	}

	f.portal.changeHead()
	f.ackAll()
	f.o.Reconcile(context.Background())
	if st := f.o.State(orchHead); st != clusterCold {
		t.Fatalf("state after a configuration change = %v, want cold", st)
	}
	f.agent.mu.Lock()
	f.agent.headErr, f.agent.onHeadEnsure = nil, func() { f.hold(0) }
	f.agent.mu.Unlock()
	if err := f.o.Ensure(context.Background(), orchHead); err != nil {
		t.Fatalf("Ensure after the change: %v", err)
	}
}

// TestEnsureClusterWaitsForConfigAck pins step one: no agent is asked
// anything before the head's and every member's agent acknowledged a document
// that carries its part of the cluster; a mesh cluster's policies are
// reconciled first; and an acknowledgement that never comes fails the start
// within the bound.
func TestEnsureClusterWaitsForConfigAck(t *testing.T) {
	f := newOrchFixture(t, 2, "relay", "mesh")
	f.holdOnHeadEnsure(0, 1)
	done := make(chan error, 1)
	go func() { done <- f.o.Ensure(context.Background(), orchHead) }()

	time.Sleep(30 * time.Millisecond)
	f.ack(orchHeadServer)
	f.ack(orchMemberServer(0))
	time.Sleep(30 * time.Millisecond)
	if calls := f.agent.recorded(); len(calls) != 0 {
		t.Fatalf("calls before every acknowledgement = %q, want none", calls)
	}
	f.portal.mu.Lock()
	reconciles := f.portal.reconciles
	f.portal.mu.Unlock()
	if reconciles != 1 {
		t.Fatalf("policy reconciles = %d, want one before the start", reconciles)
	}
	f.ack(orchMemberServer(1))
	if err := <-done; err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	g := newOrchFixture(t, 1, "relay", "lan")
	g.statuses.SetAppliedConfigETag(orchHeadServer, "an-older-document")
	ce := clusterErrOf(t, g.o.Ensure(context.Background(), orchHead))
	if ce.Code != codeClusterStartFailed || ce.Cause != codeClusterConfigUnacknowledged || !ce.Head {
		t.Fatalf("error = %+v, want cluster_start_failed with config_unacknowledged on the head", ce)
	}
	if calls := g.agent.recorded(); len(calls) != 0 {
		t.Fatalf("calls = %q, want none", calls)
	}
}

// TestEnsureClusterIgnoresStaleTelemetryDuringFormation pins that formation
// counts only telemetry that arrived after the head's start began: a hold a
// frame reported before (a previous run's) is not evidence, and a fresh frame
// without the hold does not fail the start before its bound.
func TestEnsureClusterIgnoresStaleTelemetryDuringFormation(t *testing.T) {
	f := newOrchFixture(t, 1, "relay", "lan")
	f.ackAll()
	f.hold(0) // published before the start: stale

	ce := clusterErrOf(t, f.o.Ensure(context.Background(), orchHead))
	if ce.Code != codeClusterMemberUnused || ce.Position != 0 {
		t.Fatalf("error = %+v, want cluster_member_unused: a stale hold is no evidence", ce)
	}

	f.advance(61 * time.Second)
	f.agent.mu.Lock()
	f.agent.onHeadEnsure = func() {
		f.statuses.publish(orchMemberServer(0), []RuntimeStatusDTO{orchMemberRow(orchMemberSpec(0), "")})
		go func() {
			time.Sleep(40 * time.Millisecond)
			f.hold(0)
		}()
	}
	f.agent.mu.Unlock()
	if err := f.o.Ensure(context.Background(), orchHead); err != nil {
		t.Fatalf("Ensure with a late hold: %v", err)
	}
	if f.o.State(orchHead) != clusterFormed {
		t.Fatalf("state = %v, want formed", f.o.State(orchHead))
	}
}

// TestStopClusterHeadFirst pins the explicit stop: the head is drained first,
// the stop waits for the head's stopped state on the status stream, and only
// then are the members released and stopped.
func TestStopClusterHeadFirst(t *testing.T) {
	f := newOrchFixture(t, 2, "relay", "lan")
	f.ackAll()
	f.holdOnHeadEnsure(0, 1)
	if err := f.o.Ensure(context.Background(), orchHead); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	f.statuses.publish(orchHeadServer, []RuntimeStatusDTO{{SpecID: orchHead, Model: orchHeadModel, State: agentSpecStateRunning}})
	f.agent.mu.Lock()
	f.agent.stopState = provider.RuntimeStopStopping
	f.agent.onHeadStop = func() {
		go func() {
			time.Sleep(30 * time.Millisecond)
			f.agent.record("head-reported-stopped")
			f.statuses.publish(orchHeadServer, []RuntimeStatusDTO{{SpecID: orchHead, Model: orchHeadModel, State: "stopped"}})
		}()
	}
	f.agent.mu.Unlock()
	before := len(f.agent.recorded())

	if err := f.o.Stop(context.Background(), orchHead, true); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	calls := f.agent.recorded()[before:]
	want := []string{
		"head:stop:" + orchHeadModel, "head-reported-stopped",
		"member:release:" + orchMemberSpec(0), "member:stop:" + orchMemberSpec(0),
		"member:release:" + orchMemberSpec(1), "member:stop:" + orchMemberSpec(1),
	}
	if !slices.Equal(calls, want) {
		t.Fatalf("calls = %q, want %q", calls, want)
	}
	if st := f.o.State(orchHead); st != clusterCold {
		t.Fatalf("state = %v, want cold", st)
	}
}

// TestBootReconcilerStartsPinnedHead pins the reconciler: a kept-up head is
// started members first, but not while the gateway is in its boot grace and
// a server of the cluster has not reported yet -- the gateway cannot tell
// then whether the cluster still runs.
func TestBootReconcilerStartsPinnedHead(t *testing.T) {
	f := newOrchFixture(t, 1, "relay", "lan")
	f.ackAll()
	f.holdOnHeadEnsure(0)
	f.portal.kept = []string{orchHead}

	f.o.Reconcile(context.Background())
	f.o.wait()
	if calls := f.agent.recorded(); len(calls) != 0 {
		t.Fatalf("calls in the boot grace = %q, want none", calls)
	}

	f.statuses.publish(orchHeadServer, []RuntimeStatusDTO{})
	f.statuses.publish(orchMemberServer(0), []RuntimeStatusDTO{})
	f.o.Reconcile(context.Background())
	f.o.wait()
	if f.o.State(orchHead) != clusterFormed {
		t.Fatalf("state = %v (calls %q), want formed", f.o.State(orchHead), f.agent.recorded())
	}
	if calls := f.agent.recorded(); len(calls) != 2 || calls[1] != "head:ensure:"+orchHeadModel+":cluster" {
		t.Fatalf("calls = %q, want the member then the head", calls)
	}

	f.o.Reconcile(context.Background())
	f.o.wait()
	if n := len(f.agent.recorded()); n != 2 {
		t.Fatalf("a formed head was started again: %q", f.agent.recorded())
	}
}

// TestClusterHeadExitMarksColdAndTriggers pins the head-exit trigger: a head
// whose launch facts disappear from its server's telemetry is no longer
// formed, and the reconciler is woken for it.
func TestClusterHeadExitMarksColdAndTriggers(t *testing.T) {
	f := newOrchFixture(t, 1, "relay", "lan")
	f.ackAll()
	f.holdOnHeadEnsure(0)
	if err := f.o.Ensure(context.Background(), orchHead); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	running := []RuntimeStatusDTO{{SpecID: orchHead, State: agentSpecStateRunning, Cluster: &RuntimeClusterStatusDTO{Launched: []string{"a:1"}}}}
	exited := []RuntimeStatusDTO{{SpecID: orchHead, State: agentSpecStateStopped}}
	f.o.observeStatus(orchHeadServer, running, exited)
	f.o.wait()
	if st := f.o.State(orchHead); st != clusterCold {
		t.Fatalf("state = %v, want cold after the head exited", st)
	}
	select {
	case <-f.o.TriggerChannel():
	default:
		t.Fatal("the head's exit did not wake the reconciler")
	}
}

// TestManualStartClusterAnswers202 pins the manual start of a cluster head:
// it runs the members-first start in the background and answers 202 starting
// for instance 0 once the early-answer window ran out, without the plain
// ensure a model's start sends.
func TestManualStartClusterAnswers202(t *testing.T) {
	agent := newActionAgent()
	srv := newActionServer(t, agent, 20*time.Millisecond, func(spec *routing.RuntimeSpec) {
		spec.ClusterEnabled, spec.Replicable, spec.MaxInstances = true, false, 0
	})
	f := newOrchFixture(t, 1, "relay", "lan")
	f.portal.plan.Head.Spec.ID = actionSpec
	f.ackAll()
	f.holdOnHeadEnsure(0)
	gate := make(chan struct{})
	f.agent.memberGate[orchMemberSpec(0)] = gate
	f.portal.plan.Members[0].StartupTimeoutSeconds = 500
	srv.clusters = f.o

	rec := postAction(srv, "start", "")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("start = %d %s, want 202", rec.Code, rec.Body.String())
	}
	if got := decodeAnswer(t, rec); got.Instance != 0 || got.State != "starting" {
		t.Fatalf("answer = %+v, want instance 0 starting", got)
	}
	close(gate)
	srv.waitRuntimeStarts()
	f.o.wait()
	if len(agent.ensured()) != 0 {
		t.Fatalf("plain ensures = %q, want none for a cluster head", agent.ensured())
	}
	if f.o.State(actionSpec) != clusterFormed {
		t.Fatalf("state = %v (calls %q), want formed", f.o.State(actionSpec), f.agent.recorded())
	}
}

// TestManualStartClusterRefusalIsMapped pins a start that fails within the
// window: the cluster's code with the failing position and the cause beside
// it.
func TestManualStartClusterRefusalIsMapped(t *testing.T) {
	agent := newActionAgent()
	srv := newActionServer(t, agent, time.Second, func(spec *routing.RuntimeSpec) {
		spec.ClusterEnabled, spec.Replicable, spec.MaxInstances = true, false, 0
	})
	f := newOrchFixture(t, 1, "relay", "lan")
	f.portal.plan.Head.Spec.ID = actionSpec
	f.ackAll()
	f.agent.memberErr[orchMemberSpec(0)] = orchRefusal(http.StatusServiceUnavailable, "runtime.admission_blocked")
	srv.clusters = f.o

	rec := postAction(srv, "start", "")
	var body struct {
		Error struct {
			Code     string `json:"code"`
			Cause    string `json:"cause"`
			Position *int   `json:"position"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %s: %v", rec.Body.String(), err)
	}
	if rec.Code != http.StatusBadGateway || body.Error.Code != codeClusterStartFailed || body.Error.Cause != "runtime.admission_blocked" ||
		body.Error.Position == nil || *body.Error.Position != 0 {
		t.Fatalf("start = %d %s, want 502 cluster_start_failed for member 0 with admission_blocked", rec.Code, rec.Body.String())
	}
}

// TestReconcileRefreshesWithheldHead pins that a head the runtime documents
// withheld is invalid only while they do: the next reconciler pass reads the
// plan again, so a head withheld at boot (agent features not known yet) is
// startable once they are, kept up or not.
func TestReconcileRefreshesWithheldHead(t *testing.T) {
	f := newOrchFixture(t, 1, "relay", "lan")
	f.portal.plan.WithheldReason = "runtime_config.agent_features_pending"
	ce := clusterErrOf(t, f.o.Ensure(context.Background(), orchHead))
	if ce.Code != codeClusterUnavailable || ce.Cause != "runtime_config.agent_features_pending" {
		t.Fatalf("error = %+v, want cluster_unavailable with the withheld reason", ce)
	}
	if st := f.o.State(orchHead); st != clusterInvalid {
		t.Fatalf("state = %v, want invalid while withheld", st)
	}
	f.portal.mu.Lock()
	f.portal.plan.WithheldReason = ""
	f.portal.mu.Unlock()
	f.o.Reconcile(context.Background())
	if st := f.o.State(orchHead); st != clusterCold {
		t.Fatalf("state after the documents carry the head = %v, want cold", st)
	}
}

// TestManualStopClusterAnswers pins the manual stop of a cluster head: it runs
// in the background under the server's lifetime and answers 200 stopped when
// the drain finished within the early-answer window, else 202 stopping while
// it completes.
func TestManualStopClusterAnswers(t *testing.T) {
	for _, tc := range []struct {
		name      string
		slow      bool
		wantCode  int
		wantState string
	}{
		{name: "inside the window", wantCode: http.StatusOK, wantState: provider.RuntimeStopStopped},
		{name: "past the window", slow: true, wantCode: http.StatusAccepted, wantState: provider.RuntimeStopStopping},
	} {
		t.Run(tc.name, func(t *testing.T) {
			agent := newActionAgent()
			srv := newActionServer(t, agent, 50*time.Millisecond, func(spec *routing.RuntimeSpec) {
				spec.ClusterEnabled, spec.Replicable, spec.MaxInstances = true, false, 0
			})
			f := newOrchFixture(t, 1, "relay", "lan")
			f.portal.plan.Head.Spec.ID = actionSpec
			srv.clusters = f.o
			f.statuses.publish(orchHeadServer, []RuntimeStatusDTO{{SpecID: actionSpec, State: agentSpecStateRunning}})
			release := make(chan struct{})
			if tc.slow {
				f.agent.stopState = provider.RuntimeStopStopping
				f.agent.onHeadStop = func() {
					go func() {
						<-release
						f.statuses.publish(orchHeadServer, []RuntimeStatusDTO{{SpecID: actionSpec, State: agentSpecStateStopped}})
					}()
				}
			}

			rec := postAction(srv, "stop", `{"instance":0,"stop_members":true}`)
			if rec.Code != tc.wantCode {
				t.Fatalf("stop = %d %s, want %d", rec.Code, rec.Body.String(), tc.wantCode)
			}
			if got := decodeAnswer(t, rec); got.Instance != 0 || got.State != tc.wantState {
				t.Fatalf("answer = %+v, want instance 0 %s", got, tc.wantState)
			}
			close(release)
			srv.waitRuntimeStarts()
			calls := f.agent.recorded()
			if len(calls) != 3 || calls[0] != "head:stop:"+orchHeadModel || calls[2] != "member:stop:"+orchMemberSpec(0) {
				t.Fatalf("calls = %q, want the head stopped, then the member released and stopped", calls)
			}
			if len(agent.stopped()) != 0 {
				t.Fatalf("plain stops = %q, want none for a cluster head", agent.stopped())
			}
		})
	}
}

// TestEnsureClusterPolicyPassBounded pins step one's single bound: a mesh
// cluster's policy pass that is stuck (behind another on the policy lock)
// is waited for no longer than the configuration bound, and the start goes on
// with the acknowledgement wait.
func TestEnsureClusterPolicyPassBounded(t *testing.T) {
	f := newOrchFixture(t, 1, "relay", "mesh")
	f.ackAll()
	f.holdOnHeadEnsure(0)
	gate := make(chan struct{})
	f.portal.reconcileGate = gate
	t.Cleanup(func() { close(gate) })

	began := time.Now()
	if err := f.o.Ensure(context.Background(), orchHead); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if elapsed := time.Since(began); elapsed > 2*time.Second {
		t.Fatalf("the start took %v behind a stuck policy pass, want about the configuration bound", elapsed)
	}
}

// TestEnsureClusterRereadsStalePlan pins that a plan the runtime documents
// moved past during step one (a write that changed the generation right after
// the plan was read) is read again, not waited out until the bound.
func TestEnsureClusterRereadsStalePlan(t *testing.T) {
	f := newOrchFixture(t, 1, "relay", "lan")
	f.holdOnHeadEnsure(0)
	f.portal.afterPlan = func(p *orchPortal) {
		p.plan.Generation = "gen2"
		p.version++
	}
	// The agents acknowledge the documents as they are after the write.
	f.portal.mu.Lock()
	f.portal.version++
	f.portal.mu.Unlock()
	f.ackAll()
	f.portal.mu.Lock()
	f.portal.version--
	f.portal.mu.Unlock()

	began := time.Now()
	if err := f.o.Ensure(context.Background(), orchHead); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if elapsed := time.Since(began); elapsed >= 300*time.Millisecond {
		t.Fatalf("the start took %v, want it to re-read the plan instead of waiting out the bound", elapsed)
	}
	f.portal.mu.Lock()
	reads := f.portal.planReads
	f.portal.mu.Unlock()
	if reads < 2 {
		t.Fatalf("plan reads = %d, want the stale plan read again", reads)
	}
}

// TestEnsureClusterJoinerLeavingKeepsTheStart pins that the start belongs to
// no caller: a joiner whose context ends gets its context's error, and the
// start goes on for the others.
func TestEnsureClusterJoinerLeavingKeepsTheStart(t *testing.T) {
	f := newOrchFixture(t, 1, "relay", "lan")
	f.ackAll()
	f.holdOnHeadEnsure(0)
	gate := make(chan struct{})
	f.agent.memberGate[orchMemberSpec(0)] = gate
	f.portal.plan.Members[0].StartupTimeoutSeconds = 500

	leaving, leave := context.WithCancel(context.Background())
	first := make(chan error, 1)
	go func() { first <- f.o.Ensure(leaving, orchHead) }()
	<-f.agent.arrived
	second := make(chan error, 1)
	go func() { second <- f.o.Ensure(context.Background(), orchHead) }()
	for f.o.joiners(orchHead) < 1 {
		time.Sleep(time.Millisecond)
	}
	leave()
	if err := <-first; !errors.Is(err, context.Canceled) {
		t.Fatalf("leaving caller = %v, want its context's error", err)
	}
	close(gate)
	if err := <-second; err != nil {
		t.Fatalf("remaining caller = %v, want the start formed", err)
	}
	if f.o.State(orchHead) != clusterFormed {
		t.Fatalf("state = %v, want formed", f.o.State(orchHead))
	}
}

// TestStopClusterCancelsRunningStart pins a stop during a start: the start is
// cancelled (its callers get a 503-class cancellation, not a failure that
// cools the head down), the head is drained, and a start asked for while the
// stop runs is refused.
func TestStopClusterCancelsRunningStart(t *testing.T) {
	f := newOrchFixture(t, 1, "relay", "lan")
	f.ackAll()
	f.agent.memberGate[orchMemberSpec(0)] = make(chan struct{}) // never answers
	f.portal.plan.Members[0].StartupTimeoutSeconds = 500
	f.statuses.publish(orchHeadServer, []RuntimeStatusDTO{{SpecID: orchHead, State: agentSpecStateRunning}})
	release := make(chan struct{})
	f.agent.stopState = provider.RuntimeStopStopping
	f.agent.onHeadStop = func() {
		go func() {
			<-release
			f.statuses.publish(orchHeadServer, []RuntimeStatusDTO{{SpecID: orchHead, State: agentSpecStateStopped}})
		}()
	}

	started := make(chan error, 1)
	go func() { started <- f.o.Ensure(context.Background(), orchHead) }()
	<-f.agent.arrived
	stopped := make(chan error, 1)
	go func() { stopped <- f.o.Stop(context.Background(), orchHead, false) }()

	ce := clusterErrOf(t, <-started)
	if ce.Code != codeClusterUnavailable || ce.Cause != codeClusterStartCancelled {
		t.Fatalf("start = %+v, want cluster_unavailable with start_cancelled", ce)
	}
	for call := range f.agent.arrived {
		if call == "head:stop:"+orchHeadModel {
			break
		}
	}
	ce = clusterErrOf(t, f.o.Ensure(context.Background(), orchHead))
	if ce.Cause != codeClusterStopping {
		t.Fatalf("start during the stop = %+v, want cluster_stopping", ce)
	}
	close(release)
	if err := <-stopped; err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if st := f.o.State(orchHead); st != clusterCold {
		t.Fatalf("state = %v, want cold (a cancelled start cools nothing down)", st)
	}
	rec := httptest.NewRecorder()
	writeRuntimeAgentError(rec, &clusterError{Code: codeClusterUnavailable, Cause: codeClusterStartCancelled, Position: -1})
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("cancelled start answers %d, want 503", rec.Code)
	}
}

// TestDrainingHeadStillOccupiesItsMembers pins that a draining head still has
// its process: its frame wakes no reconciler and keeps the cluster formed,
// and a direct member it still holds is not force-stopped meanwhile.
func TestDrainingHeadStillOccupiesItsMembers(t *testing.T) {
	f := newOrchFixture(t, 1, "direct", "lan")
	f.ackAll()
	f.holdOnHeadEnsure(0)
	if err := f.o.Ensure(context.Background(), orchHead); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	running := []RuntimeStatusDTO{{SpecID: orchHead, State: agentSpecStateRunning, Cluster: &RuntimeClusterStatusDTO{Launched: []string{"a:1"}}}}
	draining := []RuntimeStatusDTO{{SpecID: orchHead, State: agentSpecStateDraining, Cluster: &RuntimeClusterStatusDTO{Launched: []string{"a:1"}}}}
	f.o.observeStatus(orchHeadServer, running, draining)
	f.o.wait()
	select {
	case <-f.o.TriggerChannel():
		t.Fatal("a draining head woke the reconciler")
	default:
	}
	if st := f.o.State(orchHead); st != clusterFormed {
		t.Fatalf("state = %v, want still formed while the head drains", st)
	}

	g := newOrchFixture(t, 1, "direct", "lan")
	g.ackAll()
	g.holdOnHeadEnsure(0)
	g.statuses.publish(orchHeadServer, []RuntimeStatusDTO{{SpecID: orchHead, State: agentSpecStateDraining}})
	g.statuses.publish(orchMemberServer(0), []RuntimeStatusDTO{orchMemberRow(orchMemberSpec(0), orchHead)})
	if err := g.o.Ensure(context.Background(), orchHead); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if n := g.agent.count("member:stop:"); n != 0 {
		t.Fatalf("calls = %q, want no force-stop of a member a draining head holds", g.agent.recorded())
	}
}

// TestSignalBacksOffAfterAPlanReadFailure pins the start a store outage
// fails: the plan read failed, so nothing cools down and no failure is
// recorded, but the requests it wakes signal no new start until a short
// backoff ended -- they would otherwise spin start after failing start.
func TestSignalBacksOffAfterAPlanReadFailure(t *testing.T) {
	f := newOrchFixture(t, 1, "relay", "lan")
	f.portal.mu.Lock()
	f.portal.planErr = errors.New("database is locked")
	f.portal.mu.Unlock()
	reads := func() int {
		f.portal.mu.Lock()
		defer f.portal.mu.Unlock()
		return f.portal.planReads
	}
	signal := func() {
		f.o.Signal(routing.DemandSignal{Model: orchHeadModel, ClusterHead: orchHead})
		f.o.wait()
	}

	signal()
	signal()
	if n := reads(); n != 1 {
		t.Fatalf("plan reads = %d, want one start until the backoff ended", n)
	}
	if st := f.o.State(orchHead); st != clusterCold {
		t.Fatalf("state = %v, want cold: a failed read is no failed start", st)
	}
	if status := f.o.ClusterStatus(orchHead); status.LastFailure != nil || status.CooldownUntil != nil {
		t.Fatalf("status = %+v, want no failure and no cooldown", status)
	}

	f.advance(orchTimings().planRetry)
	f.portal.mu.Lock()
	f.portal.planErr = nil
	f.portal.mu.Unlock()
	f.ackAll()
	f.holdOnHeadEnsure(0)
	signal()
	if st := f.o.State(orchHead); st != clusterFormed {
		t.Fatalf("state = %v, want formed by the start after the backoff", st)
	}
}

// TestReconcileForgetsADeletedHead pins the end of a head's state: a head the
// reconciler would start again (the watchdog stopped it in use) whose spec is
// gone is forgotten, not read again on every pass.
func TestReconcileForgetsADeletedHead(t *testing.T) {
	f := newOrchFixture(t, 1, "relay", "lan")
	f.o.mu.Lock()
	f.o.headState(orchHead).resume = true
	f.o.mu.Unlock()
	f.portal.mu.Lock()
	f.portal.gone = true
	f.portal.mu.Unlock()

	f.o.Reconcile(context.Background())
	f.o.mu.Lock()
	_, known := f.o.heads[orchHead]
	f.o.mu.Unlock()
	if known {
		t.Fatal("the reconciler still follows a head whose spec is gone")
	}
}

// TestReconcileListsKeptUpHeadsAfterAChange pins the reconciler's cost on a
// fleet: the kept-up heads are listed again only after a runtime document
// changed (DocumentsChanged, which also wakes the reconciler) or after
// clusterKeptUpMaxAge, not on every pass.
func TestReconcileListsKeptUpHeadsAfterAChange(t *testing.T) {
	f := newOrchFixture(t, 1, "relay", "lan")
	ctx := context.Background()
	reads := func() int {
		f.portal.mu.Lock()
		defer f.portal.mu.Unlock()
		return f.portal.keptReads
	}

	f.o.Reconcile(ctx)
	f.o.Reconcile(ctx)
	if n := reads(); n != 1 {
		t.Fatalf("kept-up listings = %d after two passes, want one", n)
	}
	f.o.DocumentsChanged()
	select {
	case <-f.o.TriggerChannel():
	default:
		t.Fatal("a changed document did not wake the reconciler")
	}
	f.o.Reconcile(ctx)
	if n := reads(); n != 2 {
		t.Fatalf("kept-up listings = %d after a change, want two", n)
	}
	f.advance(clusterKeptUpMaxAge)
	f.o.Reconcile(ctx)
	if n := reads(); n != 3 {
		t.Fatalf("kept-up listings = %d once the list aged out, want three", n)
	}
}

// TestEnsureClusterWaitsForPolicySettle pins the settle after a policy write:
// when a member's NetBird policy was written just now, the members start at
// once but the head only policySettle after the write (NetBird hands the
// grant to the peers asynchronously), and the members' reservations cover
// that wait.
func TestEnsureClusterWaitsForPolicySettle(t *testing.T) {
	f := newOrchFixture(t, 1, "direct", "mesh")
	f.ackAll()
	f.portal.writes = []portal.RPCPolicyWrite{{ServerID: orchMemberServer(0), WrittenAt: f.clock()}}
	var headAt time.Time
	f.agent.onHeadEnsure = func() {
		headAt = time.Now()
		f.hold(0)
	}
	settle := f.o.t.policySettle

	began := time.Now()
	if err := f.o.Ensure(context.Background(), orchHead); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if waited := headAt.Sub(began); waited < settle {
		t.Fatalf("the head started %v into the start, want at least the %v policy settle", waited, settle)
	}
	want := []string{"member:ensure:" + orchMemberSpec(0), "head:ensure:" + orchHeadModel + ":cluster"}
	if calls := f.agent.recorded(); !slices.Equal(calls, want) {
		t.Fatalf("calls = %q, want %q (members start during the settle)", calls, want)
	}
	f.agent.mu.Lock()
	reserve := f.agent.reserves[orchMemberSpec(0)]
	f.agent.mu.Unlock()
	if wantReserve := orchHeadBound + f.o.t.reserveMargin + settle; reserve != wantReserve {
		t.Fatalf("member reservation = %v, want %v (the head's bound, the margin and the settle)", reserve, wantReserve)
	}
}

// TestEnsureClusterWaitsOnlyTheRestOfAnEarlierWrite pins the owner's case: the
// SAVE's policy pass wrote the member's policy, so the start's own pass finds
// it matching -- but the write is still settling, so the head waits for the
// rest of the window, measured from that earlier write.
func TestEnsureClusterWaitsOnlyTheRestOfAnEarlierWrite(t *testing.T) {
	f := newOrchFixture(t, 1, "direct", "mesh")
	f.ackAll()
	f.o.t.policySettle = time.Second
	const earlier = 700 * time.Millisecond
	f.portal.writes = []portal.RPCPolicyWrite{{ServerID: orchMemberServer(0), WrittenAt: f.clock().Add(-earlier)}}
	var headAt time.Time
	f.agent.onHeadEnsure = func() {
		headAt = time.Now()
		f.hold(0)
	}

	began := time.Now()
	if err := f.o.Ensure(context.Background(), orchHead); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	rest := f.o.t.policySettle - earlier
	if waited := headAt.Sub(began); waited < rest || waited >= f.o.t.policySettle-100*time.Millisecond {
		t.Fatalf("the head started %v into the start, want the %v left of the window, not the whole %v", waited, rest, f.o.t.policySettle)
	}
}

// TestEnsureClusterSettleIsCappedAfterAClockStep pins that a write time in the
// future (the wall clock stepped back since) never makes the head wait longer
// than policySettle.
func TestEnsureClusterSettleIsCappedAfterAClockStep(t *testing.T) {
	f := newOrchFixture(t, 1, "direct", "mesh")
	f.ackAll()
	f.holdOnHeadEnsure(0)
	f.portal.writes = []portal.RPCPolicyWrite{{ServerID: orchMemberServer(0), WrittenAt: f.clock().Add(time.Hour)}}

	began := time.Now()
	if err := f.o.Ensure(context.Background(), orchHead); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if elapsed := time.Since(began); elapsed < f.o.t.policySettle || elapsed > 5*time.Second {
		t.Fatalf("the start took %v, want about the %v settle, not the hour to the write's time", elapsed, f.o.t.policySettle)
	}
}

// TestEnsureClusterNoSettleWithoutRecentMemberPolicyWrite pins that no write,
// a write for a server outside this cluster, and a member write older than
// the window do not delay the head.
func TestEnsureClusterNoSettleWithoutRecentMemberPolicyWrite(t *testing.T) {
	const settle = 5 * time.Second
	for name, writes := range map[string]func(now time.Time) []portal.RPCPolicyWrite{
		"none": func(time.Time) []portal.RPCPolicyWrite { return nil },
		"other cluster": func(now time.Time) []portal.RPCPolicyWrite {
			return []portal.RPCPolicyWrite{{ServerID: "srv_elsewhere", WrittenAt: now}}
		},
		"window passed": func(now time.Time) []portal.RPCPolicyWrite {
			return []portal.RPCPolicyWrite{{ServerID: orchMemberServer(0), WrittenAt: now.Add(-settle - time.Second)}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newOrchFixture(t, 1, "direct", "mesh")
			f.ackAll()
			f.holdOnHeadEnsure(0)
			f.portal.writes = writes(f.clock())
			f.o.t.policySettle = settle

			began := time.Now()
			if err := f.o.Ensure(context.Background(), orchHead); err != nil {
				t.Fatalf("Ensure: %v", err)
			}
			if elapsed := time.Since(began); elapsed >= settle {
				t.Fatalf("the start took %v, want no policy settle", elapsed)
			}
		})
	}
}

// TestEnsureClusterStopEndsPolicySettle pins that a stop during the settle
// ends the start at once, as a cancellation, without starting the head.
func TestEnsureClusterStopEndsPolicySettle(t *testing.T) {
	f := newOrchFixture(t, 1, "direct", "mesh")
	f.ackAll()
	f.portal.writes = []portal.RPCPolicyWrite{{ServerID: orchMemberServer(0), WrittenAt: f.clock()}}
	f.o.t.policySettle = time.Minute

	started := make(chan error, 1)
	go func() { started <- f.o.Ensure(context.Background(), orchHead) }()
	// The members are ready: the start now waits out the settle.
	waitFor(t, func() bool { return f.o.ClusterStatus(orchHead).Phase == portal.ClusterPhaseMembersReady })
	began := time.Now()
	stopped := make(chan error, 1)
	go func() { stopped <- f.o.Stop(context.Background(), orchHead, false) }()

	select {
	case err := <-started:
		ce := clusterErrOf(t, err)
		if ce.Code != codeClusterUnavailable || ce.Cause != codeClusterStartCancelled {
			t.Fatalf("start = %+v, want cluster_unavailable with start_cancelled", ce)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the start kept waiting out the policy settle after a stop")
	}
	if elapsed := time.Since(began); elapsed > 2*time.Second {
		t.Fatalf("the start ended %v after the stop, want promptly", elapsed)
	}
	if err := <-stopped; err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if n := f.agent.count("head:ensure:"); n != 0 {
		t.Fatalf("head ensures = %d, want none", n)
	}
}

// TestClusterParkBoundCountsTheSettleForMeshOnly pins that the park bound
// covers the policy settle for a mesh cluster only, and in the fallback used
// before any plan is known.
func TestClusterParkBoundCountsTheSettleForMeshOnly(t *testing.T) {
	f := newOrchFixture(t, 1, "relay", "lan")
	lan := f.o.parkBound(time.Second, time.Second, false)
	if mesh := f.o.parkBound(time.Second, time.Second, true); mesh != lan+f.o.t.policySettle {
		t.Fatalf("mesh bound = %v, want the lan bound %v plus the %v settle", mesh, lan, f.o.t.policySettle)
	}
}
