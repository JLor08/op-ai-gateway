// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"errors"
	"log/slog"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/provider"
	"op-ai-gateway/internal/routing"
	"time"
)

// The request path of a llama.cpp RPC cluster head (routing's cluster_gate.go
// states the three gate states): the verdict routing reads for a mapping
// (clusterRoutingGate), the demand fan-out that starts a cold cluster
// (demandFanout, clusterOrchestrator.Signal), and the one retry of a request
// the agent refused because its head was not ready (readyClusterHead).

// clusterGateViewTTL is how long a head's gate view (gateView) answers from
// the cache; a start refreshes it on every plan read. Within it, a member
// server's status or a startup timeout an operator changed reaches the gate
// late by at most this long.
const clusterGateViewTTL = 5 * time.Second

// clusterGateViewReadTimeout bounds the one plan read a stale view makes on
// the request path.
const clusterGateViewReadTimeout = 2 * time.Second

// clusterGateView is what the cluster gate needs of a head's plan: its park
// bound, its member servers and the reason the runtime documents withhold it
// ("" when they carry it), and until when the view answers.
type clusterGateView struct {
	bound    time.Duration
	members  []routing.AIServer
	withheld string
	until    time.Time
}

// viewOf is plan's gate view: the park bound is the longest a start of the
// cold cluster may take -- the configuration wait, the slowest member's
// startup timeout and the head's, each with the start's margin -- which a
// request parked for it waits at most.
func (o *clusterOrchestrator) viewOf(plan portal.ClusterPlan) clusterGateView {
	v := clusterGateView{withheld: plan.WithheldReason, until: o.deps.now().Add(clusterGateViewTTL)}
	slowest := time.Duration(0)
	for _, m := range plan.Members {
		v.members = append(v.members, m.Server)
		slowest = max(slowest, o.startupBound(m.StartupTimeoutSeconds))
	}
	v.bound = o.parkBound(o.startupBound(plan.Head.Spec.StartupTimeoutSeconds), slowest)
	return v
}

// parkBound is the longest a start takes whose head starts within head and
// whose slowest member within slowest: step one's configuration wait, the
// members' bound, the policy settle a head may wait for after a member's
// NetBird policy was written (always counted: it is only an upper bound),
// and the head's bound, which also covers the formation (the bounds at the
// top of cluster_orchestrator.go).
func (o *clusterOrchestrator) parkBound(head, slowest time.Duration) time.Duration {
	return o.t.configWait + slowest + o.t.memberMargin + o.t.policySettle + head + o.t.headMargin
}

// gateView is head's gate view, from the cache while it answers. A stale or
// missing view is read again, one read per head at a time, bounded by
// clusterGateViewReadTimeout: a request that finds a read in flight answers
// with the stale view it has, or -- without one -- waits for that read (or its
// own end). A read that fails keeps the last view, or -- without one --
// assumes the agent's default startup timeout for the head and one member, and
// judges no member server; a request that ended before any view existed is
// judged the same way.
func (o *clusterOrchestrator) gateView(ctx context.Context, head string) clusterGateView {
	fallback := clusterGateView{bound: o.parkBound(o.startupBound(0), o.startupBound(0))}
	o.mu.Lock()
	v, ok := o.views[head]
	if ok && o.deps.now().Before(v.until) {
		o.mu.Unlock()
		return v
	}
	if done, reading := o.viewReads[head]; reading {
		hook := o.viewWaitHook
		o.mu.Unlock()
		if ok {
			return v
		}
		if hook != nil {
			hook(head)
		}
		select {
		case <-done:
		case <-ctx.Done():
		}
		o.mu.Lock()
		v, ok = o.views[head]
		o.mu.Unlock()
		if !ok {
			return fallback
		}
		return v
	}
	done := make(chan struct{})
	o.viewReads[head] = done
	o.mu.Unlock()

	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), clusterGateViewReadTimeout)
	plan, err := o.deps.portal.ClusterPlan(rctx, head)
	cancel()

	o.mu.Lock()
	defer o.mu.Unlock()
	delete(o.viewReads, head)
	defer close(done)
	switch {
	case errors.Is(err, portal.ErrRuntimeSpecNotFound):
		v = clusterGateView{withheld: portal.ErrRuntimeSpecNotFound.Error(), until: o.deps.now().Add(clusterGateViewTTL)}
	case err != nil:
		slog.Warn("cluster gate: reading the head's plan failed; judging it by the last view", "head_spec_id", head, "err", err)
		if !ok {
			v = fallback
		}
		v.until = o.deps.now().Add(clusterGateViewTTL)
	default:
		v = o.viewOf(plan)
	}
	o.views[head] = v
	return v
}

// clusterHeadSource answers whether a mapping's spec is a cluster head, and
// its spec id. Satisfied by the instance source's spec cache.
type clusterHeadSource interface {
	clusterHead(ctx context.Context, serverID, mappingID string) (string, bool)
}

// clusterRoutingGate is the gateway's routing.ClusterGate: a mapping whose
// spec is no head is not gated; a head is judged by its plan (withheld, a
// member server routing may not use) and its orchestration state.
type clusterRoutingGate struct {
	heads clusterHeadSource
	// clusters is read at every question, so the orchestration the server
	// holds when the request arrives answers it.
	clusters func() *clusterOrchestrator
	// busy reports a server a benchmark holds; nil holds none.
	busy routing.ServerBusyChecker
}

// ClusterGateState is the head's verdict (routing.ClusterGate):
//   - formed: the cluster serves;
//   - cold or forming: parkable, with the longest a start may take as the
//     bound (viewOf);
//   - cooling down after a failed start or a lost member: excluded with
//     runtime.cluster_start_failed;
//   - withheld by the runtime documents, blocked by a cause only a
//     configuration change fixes, a member server not selectable or held by a
//     benchmark, or no orchestration at all: excluded with
//     runtime.cluster_unavailable.
//
// A head excluded for a failure the orchestration recorded (the cooldown, the
// terminal block) carries that failure's cause and where it happened, so the
// refused request names it.
func (g *clusterRoutingGate) ClusterGateState(ctx context.Context, serverID, mappingID string) routing.ClusterGateVerdict {
	head, ok := g.heads.clusterHead(ctx, serverID, mappingID)
	if !ok {
		return routing.ClusterGateVerdict{}
	}
	v := routing.ClusterGateVerdict{State: routing.ClusterGateExcluded, Head: head, Code: codeClusterUnavailable}
	o := g.clusters()
	if o == nil {
		return v
	}
	view := o.gateView(ctx, head)
	if view.withheld != "" {
		return v
	}
	for _, m := range view.members {
		if !routing.ServerSelectable(m) || (g.busy != nil && g.busy.ServerBusy(m.ID)) {
			return v
		}
	}
	state, failure := o.gateState(head)
	switch state {
	case clusterFormed:
		v.State, v.Code = routing.ClusterGateFormed, ""
	case clusterCold, clusterForming:
		v.State, v.ParkBound = routing.ClusterGateParkable, view.bound
	case clusterCoolingDown:
		v.Code = codeClusterStartFailed
	}
	if failure != nil {
		v.Cause, v.Position, v.AtHead = failure.Cause, failure.position(), failure.Head
	}
	return v
}

// demandFanout is the resolver's one demand sink: a demand naming a cluster
// head goes to the cluster orchestration, every other to the instance scaler.
// Neither blocks.
type demandFanout struct {
	scaler   routing.DemandSink // nil: instance demands are dropped
	clusters func() *clusterOrchestrator
}

func (f demandFanout) Signal(d routing.DemandSignal) {
	if d.ClusterHead != "" {
		f.clusters().Signal(d)
		return
	}
	if f.scaler != nil {
		f.scaler.Signal(d)
	}
}

// isClusterNotReady reports whether err is the agent's refusal of a cluster
// head that has not formed: a request that reached it in the race between its
// selection and a stop of the head.
func isClusterNotReady(err error) bool {
	var re *provider.RouterError
	return errors.As(err, &re) && re.Code == codeClusterNotReady
}

// readyClusterHead decides the retry of a request target refused with err. When
// err is the agent's runtime.cluster_not_ready and target is a cluster head
// (instance 0, as every head is), the head's cluster is started again -- the
// orchestration's Ensure, joined if a start already runs -- bounded by ctx and
// the target's own timeout, and retry true says the request is to be sent to
// the same head once more. The caller sends it once; a second refusal is the
// request's answer, so the retry never loops.
//
// Without a retry, answer is the error the request is answered with: the
// cluster's own failure (a *routing.ClusterUnavailableError carrying the
// start's code and cause, answered 503 like the gate's exclusion) when the
// start failed, and err itself otherwise -- any other error, a mapping that is
// no head, a gateway without an orchestration, or a request that ended while
// the cluster formed.
func (s *Server) readyClusterHead(ctx context.Context, target routing.Target, err error) (retry bool, answer error) {
	if !isClusterNotReady(err) || s.clusters == nil || s.Routes == nil {
		return false, err
	}
	spec, ok, serr := s.Routes.RuntimeSpecByMapping(ctx, target.RouteID)
	if serr != nil || !ok || !spec.ClusterEnabled {
		return false, err
	}
	if target.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, target.Timeout)
		defer cancel()
	}
	if eerr := s.clusters.Ensure(ctx, spec.ID); eerr != nil {
		slog.Info("inference: the cluster head was not ready and its cluster did not form; answering the cluster's failure", "head_spec_id", spec.ID, "err", eerr)
		var ce *clusterError
		if errors.As(eerr, &ce) {
			return false, &routing.ClusterUnavailableError{Code: ce.Code, Cause: ce.Cause, Head: spec.ID, Position: ce.position(), AtHead: ce.Head}
		}
		return false, err
	}
	slog.Info("inference: the cluster head was not ready; its cluster formed, retrying once", "head_spec_id", spec.ID, "server", target.ServerID)
	return true, nil
}
