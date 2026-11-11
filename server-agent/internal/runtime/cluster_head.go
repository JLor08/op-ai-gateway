// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package runtime

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"
)

// The head guard of a llama.cpp RPC cluster. A cluster head's llama-server
// connects to its remote rpc-server members while it loads, and a member serves
// one client at a time, so the head may only start once the gateway has brought
// those members up for it (members first) -- something only the gateway can
// orchestrate. A COLD head (no process) therefore starts through exactly one
// path: the gateway's cluster start, POST /ensure/{model} carrying
// ClusterStartHeader, which reaches EnsureClusterHead. Everything else that
// would start a process refuses a cold head or leaves it down:
//
//   - a request the router forwards, a live request, and an ensure without the
//     marker are refused with ErrClusterNotReady (handleEnsure), which the
//     gateway answers by orchestrating the cluster and retrying;
//   - Apply never starts one, even pinned or force_running, and the admission
//     wakes skip it (specState.wantsStart);
//   - a head that crashes or fails to start never enters the crash-loop wait
//     (enterBackoff): it rests at the failure, and the gateway's next cluster
//     start is the retry -- the same rule burst capacity follows, and for the
//     same reason: something outside the agent decides when it is worth a load;
//   - a clean exit never re-admits it for a request that was queued without the
//     marker (admitAndStart fails such a request with ErrClusterNotReady).
//
// A RUNNING head serves every request as any model does, and a starting one
// (always started by a cluster start) lets requests queue for its start. For a
// head, pinned and force_running mean "the gateway keeps it up": the idle scan
// skips it, eviction treats a force_running head as pinned
// (specState.pinnedBySpec), and POST /stop/{model} drains it instead of
// refusing it as protected.

// ClusterStartHeader marks POST /ensure/{model} as the gateway's cluster start,
// the one request that may start a cold cluster head. Its value is "1".
const ClusterStartHeader = "X-OP-Cluster-Start"

// The cluster refusals. Stable identity (errors.Is) and stable wire code
// (Error() text), like the sentinels in manager.go; sentinelCode maps them.
var (
	// ErrClusterNotReady refuses a request that would have to start a cold
	// cluster head without the cluster-start marker. 503: the gateway
	// orchestrates the cluster (members first) and retries.
	ErrClusterNotReady = errors.New("runtime.cluster_not_ready")
	// ErrClusterMemberUnreachable is a cluster start whose pre-exec probe could
	// not reach every member endpoint within the head's startup bound; the head
	// was not exec'd. 503. A head that exits with llama.cpp's own "failed to
	// connect" message carries the same code on its last error.
	ErrClusterMemberUnreachable = errors.New(codeClusterMemberUnreach)
	// ErrInstanceBusy refuses a conditional stop (StopIfEvictable) of an
	// instance the agent's own eviction rule would not pick right now. 409.
	ErrInstanceBusy = errors.New("runtime.instance_busy")
)

// probeRetryInterval is how long probeEndpoints waits between two attempts on
// an endpoint that refused or timed out. A variable so tests can shrink it.
var probeRetryInterval = 500 * time.Millisecond

// defaultStartupTimeout is the startup bound of a spec that sets none, the
// bound pollHealth applies.
const defaultStartupTimeout = 30 * time.Second

// startupBound is spec's startup timeout as the manager enforces it.
func startupBound(spec Spec) time.Duration {
	if spec.StartupTimeoutSeconds <= 0 {
		return defaultStartupTimeout
	}
	return time.Duration(spec.StartupTimeoutSeconds) * time.Second
}

// cmdClusterHeadInfo asks the owner what a cluster start for model has to
// probe (Manager.EnsureClusterHead).
type cmdClusterHeadInfo struct {
	model string
	reply chan clusterHeadInfo
}

// isCommand marks cmdClusterHeadInfo as an owner command; the marker has no
// body.
func (cmdClusterHeadInfo) isCommand() {
	// Intentionally empty: it only places cmdClusterHeadInfo in the command set.
}

// clusterHeadInfo is cmdClusterHeadInfo's answer. probe is set for a cluster
// head that has no process yet, or one on its way out: its next process is
// exec'd for this start, so its endpoints are probed first. A head that is
// running or starting needs no probe, and neither does a model that is no head
// (or nothing managed at all), nor a head the ensure refuses anyway (a terminal
// verdict, force_stopped): the ensure then answers on its own.
type clusterHeadInfo struct {
	probe     bool
	endpoints []ClusterEndpoint
	bound     time.Duration
}

// clusterHeadInfo is cmdClusterHeadInfo on the owner.
func (o *owner) clusterHeadInfo(model string) clusterHeadInfo {
	specID, _, k, ok := o.resolveAddress(model)
	if !ok {
		return clusterHeadInfo{}
	}
	st := o.specs[InstanceKey(specID, k)]
	if st == nil {
		st = o.specs[specID]
	}
	if st == nil || !st.spec.IsClusterHead() {
		return clusterHeadInfo{}
	}
	if st.proc != nil && st.state != StateDraining {
		return clusterHeadInfo{}
	}
	// A head the ensure refuses whatever the members do is not probed: the
	// ensure answers it at once instead of after the probe's bound.
	if terminalHeld(st) || st.spec.AdminState == "force_stopped" {
		return clusterHeadInfo{}
	}
	eps := make([]ClusterEndpoint, len(st.spec.Cluster.Endpoints))
	copy(eps, st.spec.Cluster.Endpoints)
	return clusterHeadInfo{probe: true, endpoints: eps, bound: startupBound(st.spec)}
}

// EnsureClusterHead is the gateway's cluster start (POST /ensure/{model} with
// ClusterStartHeader): EnsureRunning, and the one ensure allowed to start a cold
// cluster head. The gateway calls it once it has brought the head's members up.
//
// Before the head is exec'd, every member endpoint is probed (probeEndpoints):
// a TCP connect that sends nothing, retried until the head's startup bound, so
// a member whose port is not reachable yet -- a firewall rule or mesh ACL still
// propagating -- fails the start here with ErrClusterMemberUnreachable instead
// of a head that loads and then dies on its first connection. The probe runs on
// the caller's goroutine, never on the owner's: the owner must never block
// (admitAndStart), and the bound is long. A head that is running or starting is
// not probed -- its members are in use or about to be -- and is admitted at
// once.
//
// For a model that is no cluster head the marker means nothing, and this is
// EnsureRunning.
func (m *Manager) EnsureClusterHead(ctx context.Context, model string) (endpoint string, release func(), err error) {
	info, err := m.askClusterHeadInfo(ctx, model)
	if err != nil {
		return "", nil, err
	}
	if info.probe {
		if err := probeEndpoints(ctx, info.endpoints, info.bound); err != nil {
			return "", nil, err
		}
	}
	return m.startClusterHead(ctx, model)
}

// startClusterHead is EnsureClusterHead once its probe has passed: the ensure
// that carries the cluster-start mark to the owner.
func (m *Manager) startClusterHead(ctx context.Context, model string) (endpoint string, release func(), err error) {
	return m.ensure(ctx, cmdEnsure{model: model, clusterStart: true})
}

// askClusterHeadInfo posts cmdClusterHeadInfo and waits for its answer.
func (m *Manager) askClusterHeadInfo(ctx context.Context, model string) (clusterHeadInfo, error) {
	reply := make(chan clusterHeadInfo, 1)
	select {
	case m.cmds <- cmdClusterHeadInfo{model: model, reply: reply}:
	case <-ctx.Done():
		return clusterHeadInfo{}, ctx.Err()
	case <-m.done:
		return clusterHeadInfo{}, ErrManagerClosed
	}
	select {
	case info := <-reply:
		return info, nil
	case <-m.done:
		select {
		case info := <-reply:
			return info, nil
		default:
			return clusterHeadInfo{}, ErrManagerClosed
		}
	}
}

// probeEndpoints connects to every endpoint in turn and closes each connection
// at once, never sending a byte: an rpc-server serves one client at a time and
// reads a command from the first byte on, so a probe that spoke would cost it
// its session, while a bare connect is accepted and then seen to end. An
// endpoint that refuses or does not answer is tried again every
// probeRetryInterval until bound has passed since the probe began, which is
// when it gives up with ErrClusterMemberUnreachable naming the first endpoint
// it could not reach. ctx ending stops it with ctx's error.
func probeEndpoints(ctx context.Context, eps []ClusterEndpoint, bound time.Duration) error {
	deadline := time.Now().Add(bound)
	var dialer net.Dialer
	for _, ep := range eps {
		for {
			attempt, cancel := context.WithDeadline(ctx, deadline)
			conn, err := dialer.DialContext(attempt, "tcp", ep.Address)
			cancel()
			if err == nil {
				_ = conn.Close()
				break
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			wait := min(probeRetryInterval, time.Until(deadline))
			if wait <= 0 {
				return fmt.Errorf("%w: member %s did not accept a connection within %s: %v", ErrClusterMemberUnreachable, ep.Address, bound, err)
			}
			timer := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
		}
	}
	return nil
}

// clusterStartQueued reports whether a cluster start is among st's queued
// requests.
func (st *specState) clusterStartQueued() bool {
	for _, w := range st.pending {
		if w.clusterStart {
			return true
		}
	}
	return false
}

// wantsStart reports whether anything wants st started: a queued request, or
// its being protected (pinned, force_running, or inside the min_instances
// floor). A cluster head is the exception: only a queued cluster start wants
// it started, because only the gateway knows when its members are up.
func (st *specState) wantsStart() bool {
	if st.spec.IsClusterHead() {
		return st.clusterStartQueued()
	}
	return len(st.pending) > 0 || st.protected()
}

// headRefusesUnmarked reports whether a request without the cluster-start mark
// is refused by st with ErrClusterNotReady: st is a cluster head with no
// process to serve it, or one that is draining. A starting head was started by
// a cluster start, so a request waits for that start like any other.
func headRefusesUnmarked(st *specState) bool {
	return st.spec.IsClusterHead() && (st.proc == nil || st.state == StateDraining)
}

// headKeptUp reports whether spec is a cluster head the gateway keeps up: one
// that is force_running. The agent does not start it on its own for that, but
// it counts as pinned wherever a victim is chosen or a conditional stop is
// judged (specState.pinnedBySpec), exactly as a pinned head does.
func headKeptUp(spec Spec) bool {
	return spec.IsClusterHead() && spec.AdminState == "force_running"
}
