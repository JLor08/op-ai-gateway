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

// viewOf is plan's gate view: the park bound is the head's startup timeout
// plus its slowest member's -- the time a cold cluster takes to form, which a
// request parked for it waits at most.
func (o *clusterOrchestrator) viewOf(plan portal.ClusterPlan) clusterGateView {
	v := clusterGateView{withheld: plan.WithheldReason, until: o.deps.now().Add(clusterGateViewTTL)}
	slowest := time.Duration(0)
	for _, m := range plan.Members {
		v.members = append(v.members, m.Server)
		slowest = max(slowest, o.startupBound(m.StartupTimeoutSeconds))
	}
	v.bound = o.startupBound(plan.Head.Spec.StartupTimeoutSeconds) + slowest
	return v
}

// gateView is head's gate view, from the cache while it answers. A stale or
// missing view is read again (one read per head at a time; a request that
// finds a read in flight answers with the view it has), bounded by
// clusterGateViewReadTimeout. A read that fails keeps the last view, or --
// without one -- assumes the agent's default startup timeout for the head and
// one member, and judges no member server.
func (o *clusterOrchestrator) gateView(ctx context.Context, head string) clusterGateView {
	o.mu.Lock()
	v, ok := o.views[head]
	if ok && (o.deps.now().Before(v.until) || o.viewReads[head]) {
		o.mu.Unlock()
		return v
	}
	o.viewReads[head] = true
	o.mu.Unlock()

	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), clusterGateViewReadTimeout)
	plan, err := o.deps.portal.ClusterPlan(rctx, head)
	cancel()

	o.mu.Lock()
	defer o.mu.Unlock()
	delete(o.viewReads, head)
	switch {
	case errors.Is(err, portal.ErrRuntimeSpecNotFound):
		v = clusterGateView{withheld: portal.ErrRuntimeSpecNotFound.Error(), until: o.deps.now().Add(clusterGateViewTTL)}
	case err != nil:
		slog.Warn("cluster gate: reading the head's plan failed; judging it by the last view", "head_spec_id", head, "err", err)
		if !ok {
			v = clusterGateView{bound: 2 * o.startupBound(0)}
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
	clusterHead(ctx context.Context, mappingID string) (string, bool)
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
//   - cold or forming: parkable, with the head's startup timeout plus its
//     slowest member's as the bound;
//   - cooling down after a failed start: excluded with
//     runtime.cluster_start_failed;
//   - withheld by the runtime documents, blocked by a cause only a
//     configuration change fixes, a member server not selectable or held by a
//     benchmark, or no orchestration at all: excluded with
//     runtime.cluster_unavailable.
func (g *clusterRoutingGate) ClusterGateState(ctx context.Context, mappingID string) routing.ClusterGateVerdict {
	head, ok := g.heads.clusterHead(ctx, mappingID)
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
	switch o.State(head) {
	case clusterFormed:
		v.State, v.Code = routing.ClusterGateFormed, ""
	case clusterCold, clusterForming:
		v.State, v.ParkBound = routing.ClusterGateParkable, view.bound
	case clusterCoolingDown:
		v.Code = codeClusterStartFailed
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
// the target's own timeout, and true says the request is to be sent to the same
// head once more. The caller sends it once; a second refusal is the request's
// answer, so the retry never loops. False for any other error, a head whose
// start failed, and a gateway without an orchestration.
func (s *Server) readyClusterHead(ctx context.Context, target routing.Target, err error) bool {
	if !isClusterNotReady(err) || s.clusters == nil || s.Routes == nil {
		return false
	}
	spec, ok, serr := s.Routes.RuntimeSpecByMapping(ctx, target.RouteID)
	if serr != nil || !ok || !spec.ClusterEnabled {
		return false
	}
	if target.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, target.Timeout)
		defer cancel()
	}
	if eerr := s.clusters.Ensure(ctx, spec.ID); eerr != nil {
		slog.Info("inference: the cluster head was not ready and its cluster did not form; answering the refusal", "head_spec_id", spec.ID, "err", eerr)
		return false
	}
	slog.Info("inference: the cluster head was not ready; its cluster formed, retrying once", "head_spec_id", spec.ID, "server", target.ServerID)
	return true
}
