// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package routing

import (
	"context"
	"slices"
	"time"
)

// A llama.cpp RPC cluster head is a mapping whose model runs only once the
// cluster formed: its members started first, then the head with the cluster
// marker. The agent refuses a request for a head that has not formed, so the
// resolver must not send one there -- but a cold head is not unservable
// either: it starts when asked. The cluster gate tells the two apart on every
// selection turn:
//
//   - formed: the head is an ordinary candidate;
//   - parkable (cold, or forming): the head is never dispatched; selection
//     treats it as at capacity, so the request parks, and the demand it
//     signals names the head (DemandSignal.ClusterHead), which starts the
//     cluster. The park is bounded by the cluster's own start bound, clipped
//     by the request's timeout (the application's timeout_ms), rather than by
//     the admission queue timeout, which is sized for a busy slot, not a
//     start;
//   - excluded (cooling down after a failed start, blocked by its
//     configuration, a member server unselectable or held by a benchmark):
//     the head is dropped. When nothing else serves the model the request
//     fails with the cluster's own code (ClusterUnavailableError), never
//     ErrNoHealthyHost, which would hide why.
//
// A request that must not park (inference.Request.NoPark, CheckOnly), or a
// resolver without an admission queue, is served by a parkable head when no
// other candidate is left: a gate that only selects starts the cluster itself
// before it answers, and a request sent to a head that is not ready yet is
// retried by the gateway once the cluster formed.

// ClusterGateState is a mapping's state at the cluster gate.
type ClusterGateState int

const (
	// ClusterGateNone: the mapping is no cluster head; the gate does not
	// apply.
	ClusterGateNone ClusterGateState = iota
	// ClusterGateFormed: the head's cluster formed; it serves.
	ClusterGateFormed
	// ClusterGateParkable: the cluster is cold or forming; a request parks.
	ClusterGateParkable
	// ClusterGateExcluded: the head cannot serve now; it is dropped.
	ClusterGateExcluded
)

// ClusterGateVerdict is the gate's answer for one mapping: its state, the
// head's spec id, the park bound (ClusterGateParkable: the head's startup
// timeout plus its slowest member's) and the code a request fails with when
// the head is all that is left (ClusterGateExcluded, and a parkable head a
// request cannot wait for).
type ClusterGateVerdict struct {
	State     ClusterGateState
	Head      string
	ParkBound time.Duration
	Code      string
}

// ClusterGate answers the gate state of a mapping. It is asked only for a
// server_agent candidate's instance 0 (a head is never replicable), once per
// candidate per selection turn, so an answer for a mapping that is no head
// must be cheap. Satisfied by the gateway's cluster orchestration.
type ClusterGate interface {
	ClusterGateState(ctx context.Context, mappingID string) ClusterGateVerdict
}

// SetClusterGate installs the cluster gate. Leaving it unset (nil) means no
// mapping is a cluster head.
func (r *Resolver) SetClusterGate(g ClusterGate) { r.clusterGate = g }

// ClusterUnavailableError is the request's answer when the only candidates
// left were cluster heads that cannot serve: Code is the cluster's stable code
// (runtime.cluster_start_failed while it cools down after a failed start,
// runtime.cluster_unavailable otherwise), Head the head's spec id.
type ClusterUnavailableError struct {
	Code string
	Head string
}

func (e *ClusterUnavailableError) Error() string { return e.Code }

// ServerSelectable reports whether routing may send a request to server at
// all: active and not unhealthy. The same rule judges every candidate's own
// server (CandidateServesNow), so the cluster gate judges a head's member
// servers by it as well.
func ServerSelectable(server AIServer) bool { return serverSelectable(server) }

// clusterSplit is one selection turn's candidates sorted by the cluster gate:
// serving holds every candidate that may be dispatched (no head, or a formed
// one); parked the parkable heads with their demands and the longest park
// bound; excluded the first excluded head's error.
type clusterSplit struct {
	parked   []MappingCandidate
	demands  []DemandSignal
	bound    time.Duration
	excluded *ClusterUnavailableError
	// fallback is the code a request that cannot wait for a parked head
	// fails with when serving it is not possible either.
	fallback *ClusterUnavailableError
}

// clusterVerdict asks the gate about c: only an agent candidate's instance 0
// can be a head.
func (r *Resolver) clusterVerdict(ctx context.Context, c MappingCandidate) ClusterGateVerdict {
	if r.clusterGate == nil || c.Application.Type != ProviderServerAgent || c.Instance != 0 {
		return ClusterGateVerdict{}
	}
	return r.clusterGate.ClusterGateState(ctx, c.Mapping.ID)
}

// gate sorts c by its verdict: it returns true when c may be dispatched,
// and otherwise records it as parked or excluded in s.
func (s *clusterSplit) gate(v ClusterGateVerdict, c MappingCandidate, req clusterDemandOf) bool {
	switch v.State {
	case ClusterGateParkable:
		s.parked = append(s.parked, c)
		if !slices.ContainsFunc(s.demands, func(d DemandSignal) bool { return d.ClusterHead == v.Head }) {
			s.demands = append(s.demands, DemandSignal{
				Model: c.Mapping.GatewayModelName, APIFlavor: req.apiFlavor, Live: req.live,
				MappingIDs: []string{c.Mapping.ID}, ClusterHead: v.Head,
			})
		}
		s.bound = max(s.bound, clusterParkBound(v.ParkBound, c.Application))
		if s.fallback == nil {
			s.fallback = &ClusterUnavailableError{Code: v.Code, Head: v.Head}
		}
		return false
	case ClusterGateExcluded:
		if s.excluded == nil {
			s.excluded = &ClusterUnavailableError{Code: v.Code, Head: v.Head}
		}
		return false
	default:
		return true
	}
}

// clusterDemandOf is what a cluster demand carries of the request.
type clusterDemandOf struct {
	apiFlavor string
	live      bool
}

// clusterParkBound is a parkable head's bound, clipped by the request's own
// timeout: its application's timeout_ms, when set.
func clusterParkBound(bound time.Duration, app Application) time.Duration {
	if app.TimeoutMS > 0 {
		bound = min(bound, time.Duration(app.TimeoutMS)*time.Millisecond)
	}
	return bound
}

// parkDeadline is when a park turn's wait ends. queueDeadline is the
// admission queue's (zero: unbounded). A request parked on cold cluster heads
// alone waits for the cluster's bound; one parked on other candidates at
// their cap as well waits for whichever bound is later -- either may serve it
// -- and an unbounded queue stays unbounded.
func parkDeadline(start, queueDeadline time.Time, clusterBound time.Duration, onlyClusters bool) time.Time {
	if clusterBound <= 0 {
		return queueDeadline
	}
	cd := start.Add(clusterBound)
	switch {
	case onlyClusters:
		return cd
	case queueDeadline.IsZero():
		return queueDeadline
	case cd.After(queueDeadline):
		return cd
	default:
		return queueDeadline
	}
}
