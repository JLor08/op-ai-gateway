// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package routing

import "context"

// The resolver's own verdicts, for a component that acts on routing's behalf
// outside a request -- the gateway's instance scaler, which starts an instance
// where routing would send the next request. It reads them here rather than
// keeping copies of the rules that could drift from the resolver's.

// CandidateServesNow reports whether c passes the gates every candidate of a
// request passes first (selectCandidate's first pass): its server selectable,
// its application reachable by the resolver's reachability checker, and its
// server not held by a benchmark (the resolver's busy checker). A checker left
// unset passes every candidate, as it does for a request.
func (r *Resolver) CandidateServesNow(c MappingCandidate) bool {
	if !serverSelectable(c.Server) {
		return false
	}
	if r.checker != nil && !r.checker.Reachable(c.Application.ID) {
		return false
	}
	return r.busy == nil || !r.busy.ServerBusy(c.Server.ID)
}

// InstanceTarget returns the Target a request of apiFlavor routed to instance
// k of c's mapping gets (targetFrom): the instance's address and key, the
// mapping's endpoint and the spec's credential, read from the store under ctx.
func (r *Resolver) InstanceTarget(ctx context.Context, c MappingCandidate, k int, apiFlavor string) (Target, error) {
	c.Instance, c.InstanceCapable = k, true
	return r.targetFrom(ctx, c, apiFlavor)
}
