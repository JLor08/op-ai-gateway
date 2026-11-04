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

// instanceSpecCacheTTL is how long a mapping's spec answers from the cache,
// counted from when the read that fetched it returned. The resolver -- which
// asks for every agent candidate of every request -- then reads each mapping's
// spec at most once per period instead of once per request. A spec changes when
// an operator saves it, and while the store answers, a save (a spec switched off
// replicable, its parallelism changed) reaches routing within this period plus
// the duration of the read that missed it, which is at most
// instanceSpecReadTimeout: within 7 s. While the store fails, the last spec read
// keeps answering (instanceSource.spec), so a save that the store itself cannot
// serve is not seen until a read succeeds.
const instanceSpecCacheTTL = 5 * time.Second

// instanceSpecReadTimeout bounds the one store read a cache miss makes. A request
// may carry no deadline at all (a stream lifts its own), and this keeps a stalled
// store from holding it in routing; a read that times out counts as a failed
// read.
const instanceSpecReadTimeout = 2 * time.Second

// instanceSpecFailureTTL is how long a failed spec read is remembered, counted
// from when it returned: a degraded store is then asked -- and its failure logged
// -- once per this period per mapping, instead of on every admission turn of every
// request. It is never shorter than instanceSpecReadTimeout: every request missing
// the cache waits on the read in flight (single-flight), so a store that stalls
// every read holds requests for at most half of each read-and-remember cycle,
// where a shorter period would leave them waiting on reads most of the time.
const instanceSpecFailureTTL = instanceSpecReadTimeout

// instanceSpecRetention is how long an entry is kept after it stopped answering.
// An expired entry still serves as the last good spec when a re-read fails; one
// nobody has asked about for this long is dropped, which bounds the cache by the
// mappings routed to recently.
const instanceSpecRetention = time.Minute

// instanceSpecReader is the one store read the instance source makes.
type instanceSpecReader interface {
	RuntimeSpecByMapping(ctx context.Context, mappingID string) (routing.RuntimeSpec, bool, error)
}

// instanceSpecEntry is a cached spec read: the fields instance expansion needs,
// whether the mapping has a spec at all, when the read that produced it began,
// and until when it answers. A failed read with no good read before it is cached
// too, as an entry with failed set and no spec.
type instanceSpecEntry struct {
	found        bool
	failed       bool
	specID       string
	replicable   bool
	maxInstances int
	parallelism  int
	startedAt    time.Time
	until        time.Time
}

// fresh reports whether the entry still answers at now.
func (e instanceSpecEntry) fresh(now time.Time) bool {
	return now.Before(e.until)
}

// specFlight is the one spec read in flight for a mapping: every request that
// misses the cache meanwhile waits for its entry instead of reading too.
type specFlight struct {
	done  chan struct{}
	entry instanceSpecEntry
}

// instanceSource adapts the runtime-status registry, the agent-features
// registry and the mapping's runtime spec onto routing.InstanceSource, so the
// resolver can expand a mapping into the instances its agent runs without
// internal/routing importing this package. It holds the SAME registries the
// Server publishes telemetry into, so a parked request sees an instance as
// soon as the telemetry that reports it running is ingested.
type instanceSource struct {
	store       instanceSpecReader
	features    *agentFeaturesRegistry
	registry    *runtimeStatusRegistry
	now         func() time.Time
	readTimeout time.Duration // instanceSpecReadTimeout; a test shortens it

	// mu guards specs and flights; it is never held across the store read.
	mu      sync.Mutex
	specs   map[string]instanceSpecEntry // by mapping id
	flights map[string]*specFlight       // by mapping id
}

func newInstanceSource(store instanceSpecReader, features *agentFeaturesRegistry, registry *runtimeStatusRegistry) *instanceSource {
	return &instanceSource{
		store:       store,
		features:    features,
		registry:    registry,
		now:         time.Now,
		readTimeout: instanceSpecReadTimeout,
		specs:       map[string]instanceSpecEntry{},
		flights:     map[string]*specFlight{},
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
func (s *instanceSource) InstanceView(ctx context.Context, serverID string, m routing.ModelMapping) (routing.InstanceView, bool) {
	if s == nil || !s.features.Has(serverID, runtimeInstancesFeature) {
		return routing.InstanceView{}, false
	}
	spec := s.spec(ctx, m.ID)
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

// spec returns mapping mappingID's spec, from the cache while its entry is
// fresh. A miss reads the store, once per mapping at a time: the first request
// to miss reads (read), and every request that misses while that read is in
// flight waits for its entry -- or, if its own request ends first, answers with
// the last entry the cache holds. A request already gone when it misses reads
// nothing and answers the same way, so a cancelled client neither starts a read
// nor has its cancellation remembered as the store's failure.
func (s *instanceSource) spec(ctx context.Context, mappingID string) instanceSpecEntry {
	s.mu.Lock()
	e, hit := s.specs[mappingID]
	if hit && e.fresh(s.now()) {
		s.mu.Unlock()
		return e
	}
	if s.store == nil || ctx.Err() != nil {
		s.mu.Unlock()
		return s.answerStale(e, hit)
	}
	f, waiting := s.flights[mappingID]
	if !waiting {
		f = &specFlight{done: make(chan struct{})}
		s.flights[mappingID] = f
	}
	s.mu.Unlock()
	if waiting {
		select {
		case <-f.done:
			return f.entry
		case <-ctx.Done():
			return s.answerStale(e, hit)
		}
	}
	// Landed however the read ends, so a waiter is never left on a flight that
	// will not finish.
	defer func() {
		s.mu.Lock()
		delete(s.flights, mappingID)
		s.mu.Unlock()
		close(f.done)
	}()
	f.entry = s.read(ctx, mappingID)
	return f.entry
}

// answerStale is what a request that cannot wait for a read answers: the entry
// the cache held when it missed, expired or not, and no spec without one.
func (s *instanceSource) answerStale(e instanceSpecEntry, hit bool) instanceSpecEntry {
	if !hit {
		return instanceSpecEntry{}
	}
	return e
}

// read fetches mappingID's spec from the store and caches it. The read carries
// the request's values (it is traced as part of the request) but not its end:
// other requests wait on it, so it must not fail because the one that started it
// went away. readTimeout bounds it instead.
//
// A read that succeeds answers for instanceSpecCacheTTL. One that fails -- a store
// error, or a read that ran into readTimeout -- keeps the last good spec answering
// (stale-if-error) and is only cached as "no spec" when there is none; either way
// the store is asked again after instanceSpecFailureTTL. Both periods count from
// when the read returned, so a read that took longer than its own period is still
// remembered for all of it. A read never replaces an entry from a read that began
// after it. Storing an entry drops every entry expired for longer than
// instanceSpecRetention.
func (s *instanceSource) read(ctx context.Context, mappingID string) instanceSpecEntry {
	startedAt := s.now()
	readCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.readTimeout)
	defer cancel()
	spec, found, err := s.store.RuntimeSpecByMapping(readCtx, mappingID)
	readAt := s.now()
	e, stale := s.remember(mappingID, startedAt, readAt, spec, found, err)
	switch {
	case err == nil:
	case stale:
		slog.Warn("instance expansion: runtime spec read failed; routing the mapping by its last spec read", "mapping", mappingID, "err", err)
	default:
		slog.Warn("instance expansion: runtime spec read failed; routing the mapping as one instance", "mapping", mappingID, "err", err)
	}
	return e
}

// remember caches the outcome of a read of mappingID that began at startedAt and
// returned at readAt (see read), and returns the entry that now answers for it.
// stale reports that a failed read left the last good spec answering.
func (s *instanceSource) remember(mappingID string, startedAt, readAt time.Time, spec routing.RuntimeSpec, found bool, err error) (e instanceSpecEntry, stale bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	prev, hasPrev := s.specs[mappingID]
	if hasPrev && prev.startedAt.After(startedAt) {
		return prev, false
	}
	if err != nil {
		stale = hasPrev && !prev.failed
		if stale {
			e = prev
		} else {
			e = instanceSpecEntry{failed: true}
		}
		e.until = readAt.Add(instanceSpecFailureTTL)
	} else {
		e = instanceSpecEntry{
			found:        found,
			specID:       spec.ID,
			replicable:   spec.Replicable,
			maxInstances: spec.MaxInstances,
			parallelism:  spec.InstanceParallelism,
			until:        readAt.Add(instanceSpecCacheTTL),
		}
	}
	e.startedAt = startedAt
	for id, old := range s.specs {
		if readAt.Sub(old.until) > instanceSpecRetention {
			delete(s.specs, id)
		}
	}
	s.specs[mappingID] = e
	return e, stale
}
