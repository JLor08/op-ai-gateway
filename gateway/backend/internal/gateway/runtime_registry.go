// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"sync"
	"time"
)

// runtimeStatusSubBuffer is the per-subscriber channel buffer for the
// runtime-status live stream. Small on purpose (unlike
// serverPerfSubBuffer's 256): each publish is a FULL replacement of a
// server's whole runtime list (agentFeaturesRegistry.Set's "always a
// snapshot, never a delta" discipline), so a slow subscriber only ever
// needs the LATEST one -- buffering many stale intermediate snapshots
// behind it would waste memory for no benefit once it catches up.
const runtimeStatusSubBuffer = 8

// RuntimeErrorDTO is one managed process's last failure, as published to
// live SSE subscribers (mirrors agentRuntimeError field-for-field). Volatile
// only -- see runtimeStatusRegistry's doc for why this, including
// StderrTail, is never persisted to the database.
type RuntimeErrorDTO struct {
	Message    string    `json:"message"`
	At         time.Time `json:"at"`
	ExitCode   int       `json:"exit_code"`
	Failures   int       `json:"failures"`
	StderrTail string    `json:"stderr_tail,omitempty"`
}

// RuntimeStatusDTO is one agent-managed model process's live state, as
// published to live SSE subscribers (mirrors agentRuntimeSample's json tags
// for every field it carries; it deliberately omits GPUs -- per-GPU measured
// VRAM feeds the store write-back, not the live status view).
type RuntimeStatusDTO struct {
	SpecID    string           `json:"spec_id"`
	Model     string           `json:"model"`
	State     string           `json:"state"`
	Since     time.Time        `json:"since"`
	PID       int              `json:"pid,omitempty"`
	Port      int              `json:"port,omitempty"`
	InFlight  int              `json:"in_flight"`
	Restarts  int              `json:"restarts"`
	LastError *RuntimeErrorDTO `json:"last_error,omitempty"`
}

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
// built directly in a test, bypassing New, must keep working. Bounded to the
// live server set by Retain below, like those same siblings.
type agentFeaturesRegistry struct {
	mu       sync.RWMutex
	features map[string][]string
}

func newAgentFeaturesRegistry() *agentFeaturesRegistry {
	return &agentFeaturesRegistry{features: make(map[string][]string)}
}

// NewAgentFeaturesRegistry builds an empty agent-features registry, exported
// (unlike the type itself, which stays lowercase) so cmd/gateway OWNS the one
// instance it prunes: it hands the same value to ServerDeps.AgentFeatures and
// to the app-health loop's per-cycle pruning bundle (cmd/gateway/
// app_health.go's agentRegistries) -- mirroring NewRuntimeStatusRegistry's
// exported-constructor-over-unexported-type pattern below. Without this,
// gateway.New's internal default-construction fallback builds an instance
// cmd/gateway never sees, so Retain -- however correct -- would never
// actually run against the registry production writes to.
func NewAgentFeaturesRegistry() *agentFeaturesRegistry {
	return newAgentFeaturesRegistry()
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

// Retain evicts the declared feature sets of servers no longer in live
// (mirrors runtimeStatusRegistry.Retain below and AgentCertReportRegistry.
// Retain, called at the end of every app-health cycle): a deleted server's
// last-declared features must not sit in memory for the rest of the
// process's lifetime. This is a memory bound, not a correctness fix -- a
// stale entry can never make Has return true for a live server, because
// server ids are never reused. A nil registry is a no-op.
func (r *agentFeaturesRegistry) Retain(live map[string]struct{}) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for id := range r.features {
		if _, ok := live[id]; !ok {
			delete(r.features, id)
		}
	}
}

// runtimeStatusRegistry holds per-server agent-managed-runtime STATUS the
// gateway gates its own behavior on AND streams live to the portal. Task 8
// introduced it holding ONLY the file-mode flag PushRuntimeConfig consults
// (an agent running in file mode manages its runtime from a local config
// file rather than this WS push / Task-7 poll loop, so pushing it a
// runtime_config frame would be pointless noise). Task 9 extends this SAME
// type with the snapshot+subscribe status stream the portal's live SSE view
// needs (handleRuntimeEvents, portal_runtime_endpoints.go), mirroring
// serverPerfRegistry's atomic-snapshot-plus-register discipline exactly --
// see subscribe below.
//
// Deliberately volatile-only, never the database: the bounded stderr tail on
// a RuntimeErrorDTO.LastError can carry prompt fragments from a chatty model
// server's crash output, and this project forbids persisting prompts or
// responses outside the opt-in payload-capture feature (see
// docs/architecture/cross-cutting/security-auth-rbac.md's payload-capture
// policy). Holding status in RAM only means a gateway restart simply forgets
// it -- the next telemetry sample (at most ~1s later) refills it, same as
// the active-requests list.
//
// nil-safe on every method, mirroring agentFeaturesRegistry above.
type runtimeStatusRegistry struct {
	mu       sync.RWMutex
	fileMode map[string]bool
	// statuses holds each server's MOST RECENT full runtime-status snapshot
	// (a fresh publish REPLACES it entirely, never merges -- the same "always
	// a full snapshot" discipline as agentFeaturesRegistry.Set), so a new
	// subscriber's initial `snapshot` frame reflects the live state, not an
	// empty placeholder, even before the next telemetry sample arrives.
	statuses map[string][]RuntimeStatusDTO
	// subs is the live SSE fan-out: one channel per active subscriber, keyed
	// by server id, mirroring serverPerfRegistry.subs.
	subs map[string]map[chan []RuntimeStatusDTO]struct{}
}

func newRuntimeStatusRegistry() *runtimeStatusRegistry {
	return &runtimeStatusRegistry{
		fileMode: make(map[string]bool),
		statuses: make(map[string][]RuntimeStatusDTO),
		subs:     make(map[string]map[chan []RuntimeStatusDTO]struct{}),
	}
}

// NewRuntimeStatusRegistry builds an empty runtime-status registry, exported
// (unlike the type itself, which stays lowercase) so cmd/gateway can
// construct one for ServerDeps.RuntimeStatus -- mirroring
// NewServerPerfRegistry's exported-constructor-over-unexported-type pattern
// -- and additionally wire its Retain method into the app-health loop's
// per-cycle pruning bundle (cmd/gateway/app_health.go's agentRegistries).
// Without this, gateway.New's internal default-construction fallback builds
// an instance cmd/gateway never sees, so Retain -- however correct -- would
// never actually run in production and this registry's per-server map
// entries would accumulate for every server that has ever been deleted.
func NewRuntimeStatusRegistry() *runtimeStatusRegistry {
	return newRuntimeStatusRegistry()
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

// publish replaces serverID's runtime-status snapshot and non-blockingly fans
// it out to that server's live subscribers (mirrors
// serverPerfRegistry.publish's outside-the-lock delivery discipline exactly:
// a slow reader's full channel buffer just drops this update and catches up
// on the next one, never blocking the ingest path that called publish). A
// nil registry or empty serverID is a no-op. statuses is always stored/
// delivered as a non-nil slice (an empty snapshot is a legitimate "nothing
// managed on this server" state, not the absence of one).
func (r *runtimeStatusRegistry) publish(serverID string, statuses []RuntimeStatusDTO) {
	if r == nil || serverID == "" {
		return
	}
	snap := append([]RuntimeStatusDTO(nil), statuses...)
	if snap == nil {
		snap = []RuntimeStatusDTO{}
	}
	r.mu.Lock()
	r.statuses[serverID] = snap
	targets := make([]chan []RuntimeStatusDTO, 0, len(r.subs[serverID]))
	for ch := range r.subs[serverID] {
		targets = append(targets, ch)
	}
	r.mu.Unlock()

	for _, ch := range targets {
		select {
		case ch <- snap:
		default:
		}
	}
}

// subscribe atomically returns serverID's current runtime-status snapshot
// plus a channel of subsequent full-snapshot publishes (so no update is lost
// between snapshot and registration -- see serverPerfRegistry.subscribe,
// which this mirrors exactly) and an idempotent unsubscribe. A nil registry
// returns a nil snapshot and an already-closed channel.
func (r *runtimeStatusRegistry) subscribe(serverID string) ([]RuntimeStatusDTO, <-chan []RuntimeStatusDTO, func()) {
	if r == nil {
		ch := make(chan []RuntimeStatusDTO)
		close(ch)
		return nil, ch, func() { /* no-op: nil registry has no subscriber map to remove from */ }
	}

	ch := make(chan []RuntimeStatusDTO, runtimeStatusSubBuffer)
	r.mu.Lock()
	snap := append([]RuntimeStatusDTO(nil), r.statuses[serverID]...)
	if r.subs[serverID] == nil {
		r.subs[serverID] = map[chan []RuntimeStatusDTO]struct{}{}
	}
	r.subs[serverID][ch] = struct{}{}
	r.mu.Unlock()

	unsub := func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		if set, ok := r.subs[serverID]; ok {
			delete(set, ch)
			if len(set) == 0 {
				delete(r.subs, serverID)
			}
		}
	}
	return snap, ch, unsub
}

// Retain prunes fileMode and statuses entries for servers no longer in live
// (mirrors AgentPresenceRegistry.Retain / AgentCertReportRegistry.Retain,
// called at the end of every app-health cycle): a deleted server's flag or
// last-known status must not linger in memory forever. A nil registry is a
// no-op. Existing subscriber channels for a pruned server are deliberately
// left alone -- there is nothing unsafe about an open SSE stream for a
// just-deleted server; it simply stops receiving updates and closes when its
// request context is done, same as it would for a server that just stopped
// reporting.
func (r *runtimeStatusRegistry) Retain(live map[string]struct{}) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for id := range r.fileMode {
		if _, ok := live[id]; !ok {
			delete(r.fileMode, id)
		}
	}
	for id := range r.statuses {
		if _, ok := live[id]; !ok {
			delete(r.statuses, id)
		}
	}
}
