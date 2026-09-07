// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

// runtimeModelStateChecker adapts the volatile runtime-status registry onto the
// routing.RuntimeModelStateChecker seam (Task 13, per-model live load + prefer-starting)
// so the resolver can read live per-model lifecycle state and active/queue counts
// without internal/routing importing internal/gateway (the one-way dependency the
// existing LoadedModelChecker/ServerActivityChecker adapters already preserve).
//
// It holds the SAME *runtimeStatusRegistry instance the Server publishes agent
// telemetry into (Server.RuntimeStatus), so the resolver's view is always as fresh as
// the registry's latest ingest -- mirrors modelWarmer/portalProvisioningGate: a small
// adapter type over an existing registry, wired once in New.
type runtimeModelStateChecker struct {
	registry *runtimeStatusRegistry
}

func newRuntimeModelStateChecker(registry *runtimeStatusRegistry) *runtimeModelStateChecker {
	return &runtimeModelStateChecker{registry: registry}
}

// RuntimeModelState scans serverID's latest published runtime-status snapshot for a
// managed spec reporting appModelName (RuntimeStatusDTO.Model, set agent-side from
// st.spec.UpstreamModel -- the same value routing.ModelMapping.AppModelName carries for
// that mapping). ok is false when nothing matches: a legacy/non-runtime agent that never
// publishes runtime status, or a model with no managed spec on this server -- the
// resolver then falls back to per-server telemetry and treats state as unknown.
//
// When more than one spec reports the same upstream model name (a benchmark run, a
// duplicate mapping), the RUNNING entry wins over any other state, so a live serving
// instance is never shadowed by e.g. a stale/starting sibling; absent a running match,
// the first match is used. Nil-safe: a nil checker or nil registry reports ok=false,
// matching the registry's own nil-safety (statusSnapshot returns nil).
func (c *runtimeModelStateChecker) RuntimeModelState(serverID, appModelName string) (state string, active int, queue int, ok bool) {
	if c == nil || c.registry == nil {
		return "", 0, 0, false
	}
	var match *RuntimeStatusDTO
	for _, dto := range c.registry.statusSnapshot(serverID) {
		if dto.Model != appModelName {
			continue
		}
		row := dto
		if match == nil {
			match = &row
		}
		if dto.State == "running" {
			match = &row
			break
		}
	}
	if match == nil {
		return "", 0, 0, false
	}
	return match.State, match.ActiveRequests, match.QueueDepth, true
}
