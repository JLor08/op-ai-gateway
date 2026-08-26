// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import "sync"

// agentFeaturesRegistry records, per server, the feature-name set the
// connected ServerAgent last declared in its telemetry capabilities (design
// spec §9, feature negotiation): a feature is active iff BOTH sides declare
// it by name, so PushRuntimeConfig (agent_runtime.go) consults this before
// ever sending a runtime_config frame -- an agent binary that has not
// declared runtime_manager would not understand it. Written by
// ingestTelemetrySample (agent_ingest.go) after every store write succeeds.
//
// In-memory and nil-safe on every method, mirroring every other per-server
// registry in this package (e.g. AgentCertReportRegistry): a bare *Server
// built directly in a test, bypassing New, must keep working.
type agentFeaturesRegistry struct {
	mu       sync.RWMutex
	features map[string][]string
}

func newAgentFeaturesRegistry() *agentFeaturesRegistry {
	return &agentFeaturesRegistry{features: make(map[string][]string)}
}

// Set replaces serverID's declared feature set. A fresh telemetry sample is a
// FULL snapshot, never a delta (mirrors AgentProxyStatusRegistry.Report and
// LoadedModelRegistry.SetAgentReport elsewhere in this package) -- an agent
// that stops declaring a feature (a downgrade, or simply an older binary
// reconnecting) must see Has flip back to false on the very next sample, not
// keep a stale true forever. No-op on a nil registry or an empty server id.
func (r *agentFeaturesRegistry) Set(serverID string, features []string) {
	if r == nil || serverID == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.features[serverID] = append([]string(nil), features...)
}

// Has reports whether serverID's last-declared feature set contains feature.
// false is the fail-closed default for a nil registry, a server that has
// never reported, and a server whose reported set does not (or no longer)
// contain it -- PushRuntimeConfig relies on this default to never push at an
// agent it has no positive evidence understands the frame.
func (r *agentFeaturesRegistry) Has(serverID, feature string) bool {
	if r == nil {
		return false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, f := range r.features[serverID] {
		if f == feature {
			return true
		}
	}
	return false
}

// runtimeStatusRegistry holds per-server agent-managed-runtime STATUS the
// gateway gates its own behavior on. Task 8 introduces it holding ONLY the
// file-mode flag PushRuntimeConfig consults (an agent running in file mode
// manages its runtime from a local config file rather than this WS push /
// Task-7 poll loop, so pushing it a runtime_config frame would be pointless
// noise); a later task extends this SAME type with the snapshot+subscribe
// status stream the portal UI needs -- introducing the type here, rather than
// at that later task, avoids a forward dependency on code that does not
// exist yet.
//
// nil-safe on every method, mirroring agentFeaturesRegistry above.
type runtimeStatusRegistry struct {
	mu       sync.RWMutex
	fileMode map[string]bool
}

func newRuntimeStatusRegistry() *runtimeStatusRegistry {
	return &runtimeStatusRegistry{fileMode: make(map[string]bool)}
}

// SetFileMode records whether serverID's agent currently manages its runtime
// from a local file. No-op on a nil registry or an empty id.
func (r *runtimeStatusRegistry) SetFileMode(serverID string, on bool) {
	if r == nil || serverID == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if on {
		r.fileMode[serverID] = true
	} else {
		delete(r.fileMode, serverID)
	}
}

// IsFileMode reports serverID's last-recorded file-mode flag. false -- "not
// file mode," i.e. delivery is not withheld on this ground -- is the default
// for a nil registry and for a server that has never reported:
// PushRuntimeConfig must not silently withhold delivery from every agent
// just because nothing has set this yet.
func (r *runtimeStatusRegistry) IsFileMode(serverID string) bool {
	if r == nil {
		return false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.fileMode[serverID]
}
