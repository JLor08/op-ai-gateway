// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"cmp"
	"context"
	"errors"
	"log/slog"
	"op-ai-gateway/internal/provider"
	"op-ai-gateway/internal/routing"
	"slices"
	"strings"
	"sync"
	"time"
)

// The instance scaler starts instance k >= 1 of a replicable spec when a gateway
// model needs one more: the resolver hands it a demand signal (routing.DemandSink)
// when a request is about to park because every instance it could use is at
// capacity. It asks an agent for the instance through the router's ensure route,
// and the agent has the final say -- it holds the real measurements and starts a
// burst instance only into free room. A host the agent refused is left alone for
// a while, and the next host is asked instead. Instances are never stopped here:
// the agent unloads an idle one by itself.

const (
	// defaultInstanceScaleCooldown is how long a host whose agent refused another
	// instance for lack of room, or failed to start one, is left alone when the
	// configuration (OP_AI_GATEWAY_INSTANCE_SCALE_COOLDOWN_SECONDS) sets nothing.
	defaultInstanceScaleCooldown = 60 * time.Second

	// instanceDemandUnknownDelay is how long a host is left alone after its agent
	// answered runtime.instance_demand_unknown: a running sibling's measurement has
	// not landed yet, which normally takes seconds, so the host is worth asking
	// again at the first demand signal after this short delay, not after a cooldown.
	instanceDemandUnknownDelay = 5 * time.Second

	// instanceEnsureMargin is added to a spec's startup timeout to bound an ensure.
	// The agent holds the call for the whole start and gives up on a start after the
	// startup timeout, so the margin leaves room for its admission and its answer.
	instanceEnsureMargin = 10 * time.Second

	// agentDefaultStartupTimeout is the startup timeout the agent applies to a spec
	// that sets none (its health poll's default).
	agentDefaultStartupTimeout = 30 * time.Second

	// instanceVisibleWait bounds how long a scale-up whose instance started waits for
	// routing to see that instance running before it wakes the queue and ends.
	// Routing sees an instance only once the agent's next telemetry sample reports
	// it, and a parked request signals again every time it wakes and still finds no
	// free instance: a scale-up that woke the queue the moment the agent answered
	// would be followed by another that ensures the very same instance, round after
	// round until the sample lands. The bound keeps an instance that never shows up
	// (it died at once, the agent reconnected) from holding the model's scale-ups
	// for longer than a few samples at the agent's default 1 s cadence.
	instanceVisibleWait = 5 * time.Second

	// instanceUnseenDelay is how long a host is left alone after an instance its
	// agent started did not become routable within instanceVisibleWait. The parked
	// requests are not woken for it, and they signal again on their next turn: the
	// delay keeps that signal from ensuring the same instance again at once, so an
	// instance that never shows up costs one ensure per delay, not one per turn.
	instanceUnseenDelay = 5 * time.Second

	// instanceScaleReadTimeout bounds each store read a scale-up makes (the instance
	// source bounds its own the same way): a scale-up holds its model's single
	// flight, so a stalled store must not hold it for longer than a read; a read
	// that times out counts as a failed one.
	instanceScaleReadTimeout = instanceSpecReadTimeout

	// The agent router's refusals of a burst instance (409).
	runtimeInstanceNoCapacityCode    = "runtime.instance_no_capacity"
	runtimeInstanceDemandUnknownCode = "runtime.instance_demand_unknown"
)

// instanceScalerStore is what the scaler reads from the store: a model's
// candidates, the mappings of a server's agent application, and a mapping's
// runtime spec. Satisfied by routing.Store.
type instanceScalerStore interface {
	ActiveMappingsForModel(ctx context.Context, gatewayModel string, apiFlavor string) ([]routing.MappingCandidate, error)
	MappingsByApplication(ctx context.Context, applicationID string) ([]routing.ModelMapping, error)
	RuntimeSpecByMapping(ctx context.Context, mappingID string) (routing.RuntimeSpec, bool, error)
}

// instanceRouting is the resolver's own verdicts, which the scaler reads rather
// than keeping copies of routing's rules: the routing score of a gateway model's
// mappings, whether a candidate passes the gates every request's candidates pass
// first, and the target a request routed to an instance gets. Satisfied by
// *routing.Resolver.
type instanceRouting interface {
	ScoreModelServers(ctx context.Context, model string, now time.Time) ([]routing.CandidateScore, error)
	CandidateServesNow(c routing.MappingCandidate) bool
	InstanceTarget(ctx context.Context, c routing.MappingCandidate, k int, apiFlavor string) (routing.Target, error)
}

// instanceScalerDeps is what a scaler works with. Every field is required.
type instanceScalerDeps struct {
	store   instanceScalerStore
	routing instanceRouting
	// instances is the resolver's own instance source, so the scaler counts the
	// instances routing sees, on the agents routing expands.
	instances routing.InstanceSource
	// statuses is the runtime-status registry that source reads. The scaler also
	// reads it for a starting instance 0, which the instance view leaves out, and
	// subscribes to it to see a started instance appear.
	statuses *runtimeStatusRegistry
	// ensurer returns the provider's ensure capability, nil when it has none. It is
	// asked at every signal, as every other caller of the ensure route asks the
	// provider at the call.
	ensurer func() provider.RuntimeEnsurer
	// authCtx attaches a target's upstream credential to an ensure's context.
	authCtx func(ctx context.Context, target routing.Target) context.Context
	// wake wakes one request parked in the admission queue on serverID.
	wake func(serverID string)
	// base is the server's lifetime: its end ends every scale-up.
	base func() context.Context
	now  func() time.Time
}

// scaleKey names one spec on one server by its mapping: a spec belongs to exactly
// one mapping, and a mapping to one application on one server.
type scaleKey struct{ serverID, mappingID string }

// signalKey is what tells two demand signals of one model apart: the server the
// request is pinned to ("" for none), the mappings it could use (sorted, joined)
// and its flavor -- everything that decides which instance would serve it. Two
// signals with one key ask for the same instance; signals that differ in any of
// them may each need one of their own.
type signalKey struct{ serverID, mappingIDs, apiFlavor string }

func signalKeyOf(d routing.DemandSignal) signalKey {
	return signalKey{serverID: d.ServerID, mappingIDs: strings.Join(slices.Sorted(slices.Values(d.MappingIDs)), "\x00"), apiFlavor: d.APIFlavor}
}

// signalServedBy reports whether an instance started for host (a zero host: none was)
// by a pass for a signal of flavor serves d: on d's pinned server, or anywhere for an
// unpinned d, and of one of the mappings d could use. For a signal that names none,
// any of the model's mappings of d's flavor serves it -- so only an instance a pass of
// that same flavor started, since that pass chose among the mappings of its own. An
// instance of another mapping would not serve the request that signalled d.
func signalServedBy(d routing.DemandSignal, host scaleKey, flavor string) bool {
	if host.serverID == "" || (d.ServerID != "" && d.ServerID != host.serverID) {
		return false
	}
	if len(d.MappingIDs) == 0 {
		return d.APIFlavor == flavor
	}
	return slices.Contains(d.MappingIDs, host.mappingID)
}

// scaleCandidate is one host a scale-up may start an instance on: instance index of
// mapping on key's server, asked for through target within timeout, ranked by
// score. parallelism is how many requests the instance serves at once.
type scaleCandidate struct {
	key         scaleKey
	mapping     routing.ModelMapping
	index       int
	parallelism int
	score       float64
	timeout     time.Duration
	target      routing.Target
}

// instanceScaler is the gateway's one scaler, the resolver's demand sink. See the
// top of this file.
type instanceScaler struct {
	deps        instanceScalerDeps
	cooldown    time.Duration
	visibleWait time.Duration // instanceVisibleWait; a test changes it
	readTimeout time.Duration // instanceScaleReadTimeout; a test changes it

	mu sync.Mutex
	// scaling holds the gateway models with a scale-up running, each with the
	// key of the signal that pass serves (signalKeyOf).
	scaling map[string]signalKey
	// followUps holds, per gateway model, the signals that arrived while a
	// scale-up of the model for another key ran -- one per key, in arrival order:
	// each may get a pass of its own once the running one ends.
	followUps map[string][]routing.DemandSignal
	// notBefore holds each host left alone after a refusal or a failure, until when.
	notBefore map[scaleKey]time.Time
	// ensuring holds the index an ensure is under way for, per host, which no pick
	// gives out a second time. A host's mapping serves one model and a model's
	// scale-ups run one at a time, so no other pick of that host runs meanwhile
	// today; the set keeps the index rule from depending on that.
	ensuring map[scaleKey]int

	wg sync.WaitGroup // one per running scale-up
}

// newInstanceScaler returns a scaler that leaves a refused host alone for cooldown,
// or for defaultInstanceScaleCooldown when cooldown is not positive.
func newInstanceScaler(deps instanceScalerDeps, cooldown time.Duration) *instanceScaler {
	if cooldown <= 0 {
		cooldown = defaultInstanceScaleCooldown
	}
	return &instanceScaler{
		deps:        deps,
		cooldown:    cooldown,
		visibleWait: instanceVisibleWait,
		readTimeout: instanceScaleReadTimeout,
		scaling:     map[string]signalKey{},
		followUps:   map[string][]routing.DemandSignal{},
		notBefore:   map[scaleKey]time.Time{},
		ensuring:    map[scaleKey]int{},
	}
}

// Signal starts a scale-up for d.Model in the background unless one is running
// already, and returns at once: the request that signals is about to park, or
// about to overflow onto a busy instance. What a signal leaves out is filled in,
// and what arrives while the model's scale-up runs is coalesced:
//   - d.MappingIDs empty: every mapping of d.Model the store lists for
//     d.APIFlavor may get the instance (the scale-up still skips those that do
//     not run instances).
//   - A signal arriving while a scale-up of d.Model for the same key runs (the
//     same pin, the same mappings and flavor: signalKeyOf) is folded into it:
//     that scale-up starts the instance the signal asks for. A parked request
//     signals again on its next turn, so the next scale-up starts from what this
//     one left; a one-shot signal -- the voice gate's, a pinned request's
//     overflow -- is not repeated, and is served only by the running scale-up.
//   - A signal arriving while a scale-up for another key runs may not be served
//     by it -- a scale-up pinned elsewhere, one free to place the instance off
//     the signal's pinned server, or one that may start an instance of a mapping
//     the signal's request cannot use -- so it is queued (one per key) and gets a
//     pass of its own once the running one ends (followUp), unless an instance
//     that serves it was started meanwhile (signalServedBy).
//
// Nothing starts, and the signal is dropped, when it names no model, when the
// provider cannot ensure, or once the server is shutting down.
func (s *instanceScaler) Signal(d routing.DemandSignal) {
	if s == nil || d.Model == "" {
		return
	}
	ensurer := s.deps.ensurer()
	ctx := s.deps.base()
	if ensurer == nil || ctx.Err() != nil {
		return
	}
	// The slice is the request's; the scale-up outlives the call.
	d.MappingIDs = slices.Clone(d.MappingIDs)
	key := signalKeyOf(d)
	s.mu.Lock()
	if runningKey, running := s.scaling[d.Model]; running {
		queued := s.followUps[d.Model]
		if key != runningKey && !slices.ContainsFunc(queued, func(q routing.DemandSignal) bool { return signalKeyOf(q) == key }) {
			s.followUps[d.Model] = append(queued, d)
		}
		s.mu.Unlock()
		return
	}
	s.scaling[d.Model] = key
	s.wg.Add(1)
	s.mu.Unlock()
	go s.run(ctx, ensurer, d)
}

// run is d.Model's scale-up: a pass for d, then one for each signal queued
// meanwhile that is still unserved (followUp).
func (s *instanceScaler) run(ctx context.Context, ensurer provider.RuntimeEnsurer, d routing.DemandSignal) {
	defer s.wg.Done()
	for {
		started := s.scaleUp(ctx, ensurer, d)
		next, more := s.followUp(ctx, d.Model, started, d.APIFlavor)
		if !more {
			return
		}
		d = next
	}
}

// followUp returns the next signal queued for model, and true, so the scale-up
// runs a pass for it. A signal the pass that just ended served is dropped: that pass,
// for a signal of flavor, started an instance for started (zero: none), and
// signalServedBy says which signals that instance serves. With none left, or once the
// server is shutting down, the model's scale-up ends.
func (s *instanceScaler) followUp(ctx context.Context, model string, started scaleKey, flavor string) (routing.DemandSignal, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	queued := s.followUps[model]
	for len(queued) > 0 && ctx.Err() == nil {
		next := queued[0]
		queued = queued[1:]
		if signalServedBy(next, started, flavor) {
			continue
		}
		s.followUps[model] = queued
		s.scaling[model] = signalKeyOf(next)
		return next, true
	}
	delete(s.followUps, model)
	delete(s.scaling, model)
	return routing.DemandSignal{}, false
}

// scaleUp starts one instance of d.Model on the best host that takes it, and
// returns that host (zero for none). Hosts are asked one at a time, best
// routing score first, and the choice is made afresh before each ask: an ensure
// can last as long as a start, and a host that was free of a benchmark or of a
// starting sibling when the scale-up began may not be any more. Each host is
// asked at most once -- a demand-unknown delay that runs out while another host
// is asked brings no host back -- so the scale-up ends once a host started an
// instance or none is left, and parked requests then wait for the instances
// there are. There is no retry beyond the next demand signal.
func (s *instanceScaler) scaleUp(ctx context.Context, ensurer provider.RuntimeEnsurer, d routing.DemandSignal) scaleKey {
	asked := map[scaleKey]bool{}
	for ctx.Err() == nil {
		cands := s.pickHost(ctx, d)
		i := slices.IndexFunc(cands, func(c scaleCandidate) bool { return !asked[c.key] })
		if i < 0 {
			return scaleKey{}
		}
		asked[cands[i].key] = true
		if s.start(ctx, ensurer, d.Model, cands[i]) {
			return cands[i].key
		}
	}
	return scaleKey{}
}

// start asks c's agent for instance c.index and reports whether it runs. Once it
// runs and routing sees it (awaitRoutable), as many requests parked on c's server
// are woken as the instance serves at once: each wake is one freed slot, as a
// finished request's release is. An instance routing does not see within the
// wait wakes no one -- a woken request would not find it -- and leaves the host
// alone for instanceUnseenDelay. A refusal leaves the host alone -- for the
// cooldown when the agent has no room, for instanceDemandUnknownDelay while a
// sibling's demand is unmeasured -- and so does any other failure, which is also
// logged. An ensure the server's shutdown cut short says nothing about the host.
func (s *instanceScaler) start(ctx context.Context, ensurer provider.RuntimeEnsurer, model string, c scaleCandidate) bool {
	s.mu.Lock()
	s.ensuring[c.key] = c.index
	s.mu.Unlock()
	ensureCtx, cancel := context.WithTimeout(ctx, c.timeout)
	err := ensurer.EnsureRuntimeModel(s.deps.authCtx(ensureCtx, c.target), c.target)
	cancel()
	s.mu.Lock()
	delete(s.ensuring, c.key)
	s.mu.Unlock()

	attrs := []any{"model", model, "server_id", c.key.serverID, "instance", c.target.ProviderModel}
	if err == nil {
		slog.Info("instance scaler: started an instance", attrs...)
		if !s.awaitRoutable(ctx, c) {
			if ctx.Err() == nil {
				slog.Warn("instance scaler: a started instance did not show up in the agent's status; waking no request and leaving the host alone briefly", append(attrs, "delay", instanceUnseenDelay)...)
				s.holdOff(c.key, instanceUnseenDelay)
			}
			return true
		}
		for range c.parallelism {
			s.deps.wake(c.key.serverID)
		}
		return true
	}
	if ctx.Err() != nil {
		return false
	}
	var re *provider.RouterError
	code := ""
	if errors.As(err, &re) {
		code = re.Code
	}
	switch code {
	case runtimeInstanceNoCapacityCode:
		slog.Debug("instance scaler: the agent has no room for another instance; leaving the host alone for the cooldown", append(attrs, "cooldown", s.cooldown)...)
		s.holdOff(c.key, s.cooldown)
	case runtimeInstanceDemandUnknownCode:
		slog.Debug("instance scaler: a sibling's demand is not measured yet; asking the host again after a short delay", append(attrs, "delay", instanceDemandUnknownDelay)...)
		s.holdOff(c.key, instanceDemandUnknownDelay)
	default:
		slog.Warn("instance scaler: starting an instance failed; leaving the host alone for the cooldown", append(attrs, "cooldown", s.cooldown, "err", err)...)
		s.holdOff(c.key, s.cooldown)
	}
	return false
}

// awaitRoutable reports whether routing sees c's new instance running -- the
// instance source reports it -- within s.visibleWait; it returns false at the
// bound or when ctx ends. It subscribes to the server's status before it looks,
// so a sample published in between is not missed.
func (s *instanceScaler) awaitRoutable(ctx context.Context, c scaleCandidate) bool {
	_, updates, unsubscribe := s.deps.statuses.subscribe(c.key.serverID)
	defer unsubscribe()
	timer := time.NewTimer(s.visibleWait)
	defer timer.Stop()
	for !s.routable(ctx, c) {
		select {
		case _, open := <-updates:
			if !open {
				updates = nil // a nil registry's closed channel: only the bound is left
			}
		case <-timer.C:
			return false
		case <-ctx.Done():
			return false
		}
	}
	return true
}

// routable reports whether the instance source lists c's instance as running.
func (s *instanceScaler) routable(ctx context.Context, c scaleCandidate) bool {
	readCtx, cancel := s.bounded(ctx)
	defer cancel()
	view, ok := s.deps.instances.InstanceView(readCtx, c.key.serverID, c.mapping)
	return ok && slices.Contains(view.Running, c.index)
}

// bounded is ctx for one store read of a scale-up: it ends after s.readTimeout.
func (s *instanceScaler) bounded(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, s.readTimeout)
}

// pickHost returns the hosts d's scale-up may start an instance on, best routing
// score first, ties in the request's candidate order. Only the mappings the parked
// request could use are considered (d.MappingIDs) -- an instance of any other would
// not serve it -- or, for a signal that names none, every mapping of the model in
// the store's order; and only on d.ServerID when the request is pinned there.
func (s *instanceScaler) pickHost(ctx context.Context, d routing.DemandSignal) []scaleCandidate {
	readCtx, cancel := s.bounded(ctx)
	pool, err := s.deps.store.ActiveMappingsForModel(readCtx, d.Model, d.APIFlavor)
	cancel()
	if err != nil {
		slog.Debug("instance scaler: reading the model's mappings failed", "model", d.Model, "err", err)
		return nil
	}
	byID := make(map[string]routing.MappingCandidate, len(pool))
	for _, c := range pool {
		byID[c.Mapping.ID] = c
	}
	ids := d.MappingIDs
	if len(ids) == 0 {
		ids = make([]string, 0, len(pool))
		for _, c := range pool {
			ids = append(ids, c.Mapping.ID)
		}
	}
	var out []scaleCandidate
	for _, id := range ids {
		c, ok := byID[id]
		if !ok || (d.ServerID != "" && c.Server.ID != d.ServerID) {
			continue
		}
		if sc, ok := s.candidate(ctx, c, d.APIFlavor); ok {
			out = append(out, sc)
		}
	}
	if len(out) > 1 {
		s.score(ctx, d.Model, out)
		slices.SortStableFunc(out, func(a, b scaleCandidate) int { return cmp.Compare(b.score, a.score) })
	}
	return out
}

// score sets each candidate's routing score. Scores the resolver could not compute
// leave the request's order; a candidate it found not viable scores 0 and is asked
// last.
func (s *instanceScaler) score(ctx context.Context, model string, cands []scaleCandidate) {
	readCtx, cancel := s.bounded(ctx)
	defer cancel()
	scores, err := s.deps.routing.ScoreModelServers(readCtx, model, s.deps.now())
	if err != nil {
		slog.Debug("instance scaler: scoring the model's hosts failed; asking them in the request's order", "model", model, "err", err)
		return
	}
	byID := make(map[string]float64, len(scores))
	for _, sc := range scores {
		byID[sc.MappingID] = sc.Score
	}
	for i := range cands {
		cands[i].score = byID[cands[i].mapping.ID]
	}
}

// candidate reports whether c's host can take another instance now and, if so,
// which one and how to ask for it (the target routing builds for that instance,
// for a request of apiFlavor). A host is passed over when
//   - its application is not an agent's;
//   - routing would not send it a request: c fails the gates every request's
//     candidates pass first (routing's CandidateServesNow -- a selectable
//     server, a reachable application, no benchmark);
//   - the instance source does not count the mapping as running instances there:
//     its agent is not known to declare runtime_instances (an older agent beside a
//     newer one is never sent an instance address), or there is no spec, or one
//     that is not replicable;
//   - the spec is at a ceiling, counting instance 0 whether it runs or not: its
//     max_instances, or the real-time concurrency the audio benchmark measured as
//     a performance limit (a limit of 0 included; a stop for resources or at a cap
//     is a lower bound, not a ceiling);
//   - it is left alone after a refusal or a failure (holdOff);
//   - any instance of the spec is starting: a new sibling would start beside one
//     whose demand is not measured yet;
//   - no index is left below max_instances (instanceIndex): the indexes the
//     agent still reports -- a draining or stopped instance's too -- are not
//     free (one resting at a failure is: specInstances), so a spec at its
//     ceiling by index is passed over quietly, with no hold.
//
// Every store read is bounded (bounded), and a read that fails passes the host
// over.
func (s *instanceScaler) candidate(ctx context.Context, c routing.MappingCandidate, apiFlavor string) (scaleCandidate, bool) {
	serverID := c.Server.ID
	if c.Application.Type != routing.ProviderServerAgent || !s.deps.routing.CandidateServesNow(c) {
		return scaleCandidate{}, false
	}
	viewCtx, cancel := s.bounded(ctx)
	view, ok := s.deps.instances.InstanceView(viewCtx, serverID, c.Mapping)
	cancel()
	if !ok {
		return scaleCandidate{}, false
	}
	if atInstanceCeiling(view, c.Mapping) {
		return scaleCandidate{}, false
	}
	key := scaleKey{serverID: serverID, mappingID: c.Mapping.ID}
	if s.heldOff(key) {
		return scaleCandidate{}, false
	}
	spec, found := s.candidateSpec(ctx, c.Mapping.ID)
	if !found {
		return scaleCandidate{}, false
	}
	starting, reported := s.specInstances(serverID, spec.ID)
	if starting {
		return scaleCandidate{}, false
	}
	k := s.instanceIndex(ctx, c, key, append(reported, view.Present...))
	if k == 0 || (view.MaxInstances > 0 && k >= view.MaxInstances) {
		return scaleCandidate{}, false
	}
	targetCtx, cancel := s.bounded(ctx)
	target, err := s.deps.routing.InstanceTarget(targetCtx, c, k, apiFlavor)
	cancel()
	if err != nil {
		slog.Debug("instance scaler: building the instance's target failed", "mapping_id", c.Mapping.ID, "err", err)
		return scaleCandidate{}, false
	}
	startup := time.Duration(spec.StartupTimeoutSeconds) * time.Second
	if startup <= 0 {
		startup = agentDefaultStartupTimeout
	}
	return scaleCandidate{
		key:         key,
		mapping:     c.Mapping,
		index:       k,
		parallelism: max(view.Parallelism, 1),
		timeout:     startup + instanceEnsureMargin,
		target:      target,
	}, true
}

// atInstanceCeiling reports whether the spec view describes is at a ceiling, counting
// instance 0 whether it runs or not: its max_instances, or the real-time concurrency the
// audio benchmark measured for m as a performance limit (a limit of 0 included).
func atInstanceCeiling(view routing.InstanceView, m routing.ModelMapping) bool {
	total := 1 + len(view.Present)
	if view.MaxInstances > 0 && total >= view.MaxInstances {
		return true
	}
	return m.AudioConcurrencyLimit == routing.AudioLimitPerformance && total >= m.AudioRealtimeConcurrency
}

// candidateSpec reads the runtime spec of mappingID for candidate, bounded (bounded). A
// read that fails is logged and, like a mapping without a spec, answers found false.
func (s *instanceScaler) candidateSpec(ctx context.Context, mappingID string) (routing.RuntimeSpec, bool) {
	specCtx, cancel := s.bounded(ctx)
	spec, found, err := s.deps.store.RuntimeSpecByMapping(specCtx, mappingID)
	cancel()
	if err != nil {
		slog.Debug("instance scaler: reading the runtime spec failed", "mapping_id", mappingID, "err", err)
		return routing.RuntimeSpec{}, false
	}
	return spec, found
}

// specInstances reads serverID's status rows of spec specID: whether any instance
// of it is starting, instance 0 included, and every index k >= 1 the agent
// reports for it in a state that keeps the index taken. An index the agent
// still reports draining, stopped or waiting is not one an ensure can start
// afresh. An index resting at a failure (instanceReusableStates) is: the agent
// keeps that row only for the operator to read, until it is named again or
// the next Apply, and an ensure of it is exactly what names it again -- it
// retires a crashed or start_failed row and starts a fresh instance there, and
// runs admission on a not_permitted one again (behind the agent's refusal rate
// limit). Routing never names such a row, so were its index counted as taken
// nothing would ever free it, and every failure would use up one index for
// good -- the last one below max_instances included.
func (s *instanceScaler) specInstances(serverID, specID string) (starting bool, reported []int) {
	rows := s.deps.statuses.statusRows(serverID)
	for i := range rows {
		row := &rows[i] // read-only: the published snapshot itself
		if row.SpecID != specID {
			continue
		}
		if row.State == agentSpecStateStarting {
			starting = true
		}
		if row.Instance >= 1 && !instanceReusableStates[row.State] {
			reported = append(reported, row.Instance)
		}
	}
	return starting, reported
}

// instanceReusableStates are the states a burst instance rests at after a
// failure, which the agent keeps until the index is named again: an ensure of
// such an index is the retry, so the scaler treats it as free.
var instanceReusableStates = map[string]bool{"crashed": true, "start_failed": true, "not_permitted": true}

// instanceIndex is the index c's next instance gets (nextInstanceIndex), 0 for none.
// Besides the indexes taken (taken: present, or reported in a state that keeps
// the index -- specInstances) and the one an ensure is under way for, it skips
// every index whose address is another mapping's upstream model on the same
// agent: the agent resolves a literal upstream name before it reads an instance
// address, so an ensure of that address would start the other spec, and the
// agent never runs that instance of this one.
// The agent's specs are the mappings of the server's server_agent application.
func (s *instanceScaler) instanceIndex(ctx context.Context, c routing.MappingCandidate, key scaleKey, present []int) int {
	readCtx, cancel := s.bounded(ctx)
	mappings, err := s.deps.store.MappingsByApplication(readCtx, c.Application.ID)
	cancel()
	if err != nil {
		slog.Debug("instance scaler: reading the agent's mappings failed", "application_id", c.Application.ID, "err", err)
		return 0
	}
	taken := map[int]bool{}
	s.mu.Lock()
	if k, ok := s.ensuring[key]; ok {
		taken[k] = true
	}
	s.mu.Unlock()
	others := make(map[string]bool, len(mappings))
	for _, m := range mappings {
		if m.ID != c.Mapping.ID {
			others[m.AppModelName] = true
		}
	}
	for k := 1; k <= routing.MaxInstanceIndex && len(others) > 0; k++ {
		if others[routing.InstanceAddress(c.Mapping.AppModelName, k)] {
			taken[k] = true
		}
	}
	return nextInstanceIndex(present, taken)
}

// nextInstanceIndex returns the lowest instance index k >= 1 that is neither in
// present nor in taken, and 0 when every index an address can carry
// (up to routing.MaxInstanceIndex) is.
func nextInstanceIndex(present []int, taken map[int]bool) int {
	for k := 1; k <= routing.MaxInstanceIndex; k++ {
		if !taken[k] && !slices.Contains(present, k) {
			return k
		}
	}
	return 0
}

// holdOff leaves key's host alone for d from now, and drops the holds that have
// run out, so the map holds no more than the hosts currently left alone.
func (s *instanceScaler) holdOff(key scaleKey, d time.Duration) {
	now := s.deps.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, until := range s.notBefore {
		if !now.Before(until) {
			delete(s.notBefore, k)
		}
	}
	s.notBefore[key] = now.Add(d)
}

// heldOff reports whether key's host is still left alone.
func (s *instanceScaler) heldOff(key scaleKey) bool {
	now := s.deps.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	until, ok := s.notBefore[key]
	return ok && now.Before(until)
}

// wait returns once every scale-up started so far has ended.
func (s *instanceScaler) wait() { s.wg.Wait() }
