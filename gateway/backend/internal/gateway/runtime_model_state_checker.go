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
	// features is the shared agent-features registry (the SAME instance the
	// Server publishes agent capability declarations into). It gates the
	// active/queue METRICS on the reporting agent having declared
	// "runtime_model_probe": the registry is populated for EVERY agent-managed
	// spec regardless of that flag, so a pre-0.6.0 runtime_manager agent (which
	// never sends the new active/queue fields -> they default to a fabricated 0)
	// must not have that 0 scored as real load. It is nil-safe (Has returns
	// false on a nil registry), so a bare checker keeps working.
	features *agentFeaturesRegistry
}

func newRuntimeModelStateChecker(registry *runtimeStatusRegistry, features *agentFeaturesRegistry) *runtimeModelStateChecker {
	return &runtimeModelStateChecker{registry: registry, features: features}
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
//
// metricsOK gates the active/queue metrics separately from ok/state: it is true only when
// a DTO matched AND serverID's agent has declared the "runtime_model_probe" feature (the
// capability that actually populates active/queue). state/ok stay valid for any
// runtime_manager agent so prefer-starting is unaffected; only the metrics overlay is
// held back for a non-probing agent whose active/queue are a fabricated 0.
func (c *runtimeModelStateChecker) RuntimeModelState(serverID, appModelName string) (state string, active int, queue int, ok bool, metricsOK bool) {
	if c == nil || c.registry == nil {
		return "", 0, 0, false, false
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
		return "", 0, 0, false, false
	}
	return match.State, match.ActiveRequests, match.QueueDepth, true, c.features.Has(serverID, runtimeModelProbeFeature)
}
