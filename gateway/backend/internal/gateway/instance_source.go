// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"log/slog"
	"op-ai-gateway/internal/routing"
	"slices"
	"sync"
	"time"
)

// instanceSpecCacheTTL is how long the instance source keeps a mapping's spec
// before reading it again. A spec changes when an operator saves it, so a
// spec switched off replicable (or its parallelism changed) reaches routing
// within this long, while the resolver -- which asks for every agent
// candidate of every request -- reads each mapping's spec at most once per
// period instead of once per request.
const instanceSpecCacheTTL = 5 * time.Second

// instanceSpecReadTimeout bounds the one store read a cache miss makes. The
// InstanceSource seam carries no request context, so this is what keeps a
// stalled store from holding a request in routing indefinitely; a read that
// times out is answered as "not instance-capable", the behaviour of a mapping
// without instances.
const instanceSpecReadTimeout = 2 * time.Second

// instanceSpecReader is the one store read the instance source makes.
type instanceSpecReader interface {
	RuntimeSpecByMapping(ctx context.Context, mappingID string) (routing.RuntimeSpec, bool, error)
}

// instanceSpecEntry is a cached spec read: the fields instance expansion
// needs, whether the mapping has a spec at all, and when it was read.
type instanceSpecEntry struct {
	found        bool
	specID       string
	replicable   bool
	maxInstances int
	parallelism  int
	readAt       time.Time
}

// instanceSource adapts the runtime-status registry, the agent-features
// registry and the mapping's runtime spec onto routing.InstanceSource, so the
// resolver can expand a mapping into the instances its agent runs without
// internal/routing importing this package. It holds the SAME registries the
// Server publishes telemetry into, so a parked request sees an instance as
// soon as the telemetry that reports it running is ingested.
type instanceSource struct {
	store    instanceSpecReader
	features *agentFeaturesRegistry
	registry *runtimeStatusRegistry
	now      func() time.Time

	// mu guards specs only; it is never held across the store read.
	mu    sync.Mutex
	specs map[string]instanceSpecEntry // by mapping id
}

func newInstanceSource(store instanceSpecReader, features *agentFeaturesRegistry, registry *runtimeStatusRegistry) *instanceSource {
	return &instanceSource{
		store:    store,
		features: features,
		registry: registry,
		now:      time.Now,
		specs:    map[string]instanceSpecEntry{},
	}
}

// InstanceView reports the instances of mapping m on serverID. ok requires
// both an agent KNOWN to declare runtime_instances (Has is false for an agent
// whose features are not known yet as much as for one that lacks it: an older
// agent beside a newer one is never expanded) and a replicable spec behind the
// mapping; anything else is the mapping's instance 0 alone, today's
// behaviour. The feature is checked first, so a mixed fleet's older agents
// cost no spec read.
//
// Running and Present come from the server's latest status snapshot, which
// every telemetry sample replaces whole: an instance the agent stopped
// reporting -- it was retired, or the agent restarted -- is gone from both on
// the next sample, with nothing to expire. The snapshot is read without a
// copy (statusRows), so the registry's lock is held for the map read alone on
// this per-request path. A row counts only when it is this spec's (SpecID) and
// names the address this mapping sends for its index, so routing never sends
// an address the agent is not serving.
func (s *instanceSource) InstanceView(serverID string, m routing.ModelMapping) (routing.InstanceView, bool) {
	if s == nil || !s.features.Has(serverID, runtimeInstancesFeature) {
		return routing.InstanceView{}, false
	}
	spec := s.spec(m.ID)
	if !spec.found || !spec.replicable {
		return routing.InstanceView{}, false
	}
	view := routing.InstanceView{
		Replicable:   true,
		Parallelism:  max(spec.parallelism, 1),
		MaxInstances: spec.maxInstances,
	}
	rows := s.registry.statusRows(serverID)
	for i := range rows {
		row := &rows[i] // read-only: the published snapshot itself
		k := row.Instance
		if row.SpecID != spec.specID || k < 1 || k > routing.MaxInstanceIndex || row.Model != routing.InstanceAddress(m.AppModelName, k) {
			continue
		}
		switch row.State {
		case agentSpecStateRunning:
			view.Running = append(view.Running, k)
			view.Present = append(view.Present, k)
		case agentSpecStateStarting:
			view.Present = append(view.Present, k)
		}
	}
	slices.Sort(view.Running)
	view.Running = slices.Compact(view.Running)
	slices.Sort(view.Present)
	view.Present = slices.Compact(view.Present)
	return view, true
}

// spec returns mapping mappingID's spec, from the cache while the last read is
// younger than instanceSpecCacheTTL. A failed read is not cached -- the next
// request asks again -- and reads as no spec. A miss drops every expired
// entry, which bounds the cache by the mappings read in the last period.
func (s *instanceSource) spec(mappingID string) instanceSpecEntry {
	now := s.now()
	s.mu.Lock()
	e, hit := s.specs[mappingID]
	s.mu.Unlock()
	if hit && now.Sub(e.readAt) < instanceSpecCacheTTL {
		return e
	}
	if s.store == nil {
		return instanceSpecEntry{}
	}
	ctx, cancel := context.WithTimeout(context.Background(), instanceSpecReadTimeout)
	defer cancel()
	spec, found, err := s.store.RuntimeSpecByMapping(ctx, mappingID)
	if err != nil {
		slog.Warn("instance expansion: runtime spec read failed; routing the mapping as one instance", "mapping", mappingID, "err", err)
		return instanceSpecEntry{}
	}
	e = instanceSpecEntry{
		found:        found,
		specID:       spec.ID,
		replicable:   spec.Replicable,
		maxInstances: spec.MaxInstances,
		parallelism:  spec.InstanceParallelism,
		readAt:       now,
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, old := range s.specs {
		if now.Sub(old.readAt) >= instanceSpecCacheTTL {
			delete(s.specs, id)
		}
	}
	s.specs[mappingID] = e
	return e
}
