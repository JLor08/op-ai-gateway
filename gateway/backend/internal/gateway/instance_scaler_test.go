// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"fmt"
	"net/http"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/inference"
	"op-ai-gateway/internal/provider"
	"op-ai-gateway/internal/routing"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"
)

// The scaler's tests run it against the real runtime-status and
// agent-feature registries, the real instance source and a real resolver's
// gates and targets -- the views routing itself reads -- over a scripted
// store, scores and agent (scalerFixture). The
// fixture's clock moves only when a test advances it, and a test waits for
// every scale-up it starts (instanceScaler.wait), so no outcome depends on
// how the scaler's goroutines happen to interleave with the test.

const (
	scaleModel    = "gm" // the gateway model every fixture host serves
	scaleUpstream = "m"  // its upstream model: instance 0's address
)

// scalerStore is the scaler's store, and the resolver's: the candidates of
// each gateway model, further mappings by application, and each mapping's
// runtime spec. The embedded Store is nil: a read the scaler does not make
// panics. A read named in stalled blocks until its context ends.
type scalerStore struct {
	routing.Store

	mu      sync.Mutex
	cands   []routing.MappingCandidate
	specs   map[string]routing.RuntimeSpec    // by mapping id
	more    map[string][]routing.ModelMapping // by application id
	stalled map[string]bool                   // by read: "mappings", "byApp", "spec", "score"
}

// stall blocks a read named read that is stalled until ctx ends, and reports
// ctx's error; nil when the read is not stalled.
func (s *scalerStore) stall(ctx context.Context, read string) error {
	s.mu.Lock()
	stalled := s.stalled[read]
	s.mu.Unlock()
	if !stalled {
		return nil
	}
	<-ctx.Done()
	return ctx.Err()
}

func (s *scalerStore) setStalled(read string, stalled bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stalled[read] = stalled
}

func (s *scalerStore) ActiveMappingsForModel(ctx context.Context, model, _ string) ([]routing.MappingCandidate, error) {
	if err := s.stall(ctx, "mappings"); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []routing.MappingCandidate
	for _, c := range s.cands {
		if c.Mapping.GatewayModelName == model {
			out = append(out, c)
		}
	}
	return out, nil
}

func (s *scalerStore) MappingsByApplication(ctx context.Context, appID string) ([]routing.ModelMapping, error) {
	if err := s.stall(ctx, "byApp"); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []routing.ModelMapping
	for _, c := range s.cands {
		if c.Application.ID == appID {
			out = append(out, c.Mapping)
		}
	}
	return append(out, s.more[appID]...), nil
}

func (s *scalerStore) RuntimeSpecByMapping(ctx context.Context, mappingID string) (routing.RuntimeSpec, bool, error) {
	if err := s.stall(ctx, "spec"); err != nil {
		return routing.RuntimeSpec{}, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	spec, ok := s.specs[mappingID]
	return spec, ok, nil
}

// scalerScores is each mapping's fixed routing score.
type scalerScores map[string]float64

// scalerRouting is a real resolver over the fixture's store and checkers --
// its gates and its targets are routing's own -- whose scores are scripted.
type scalerRouting struct {
	*routing.Resolver
	store  *scalerStore
	scores scalerScores
}

func (r scalerRouting) ScoreModelServers(ctx context.Context, _ string, _ time.Time) ([]routing.CandidateScore, error) {
	if err := r.store.stall(ctx, "score"); err != nil {
		return nil, err
	}
	out := make([]routing.CandidateScore, 0, len(r.scores))
	for id, score := range r.scores {
		out = append(out, routing.CandidateScore{MappingID: id, Score: score, Available: true})
	}
	return out, nil
}

// scalerBusy is the set of benchmark-busy servers.
type scalerBusy map[string]bool

func (b scalerBusy) ServerBusy(serverID string) bool { return b[serverID] }

// scalerUnreachable is the set of unreachable applications.
type scalerUnreachable map[string]bool

func (u scalerUnreachable) Reachable(appID string) bool { return !u[appID] }

// scaleAuthKey marks a context the scaler's auth wrapper has seen.
type scaleAuthKey struct{}

// scaleEnsureCall is one ensure the agent was asked for: the target, and how
// long the call had left until its deadline when it arrived.
type scaleEnsureCall struct {
	target routing.Target
	budget time.Duration
	authed bool
}

// scaleEnsurer is the scripted agent behind every host: it records each
// ensure, answers it with the next refusal queued for its server (running
// when none is left), and on success reports the instance running the way
// the agent's next telemetry sample would (started). gate, when set, holds
// every call until it is closed or the call's context ends.
type scaleEnsurer struct {
	mu      sync.Mutex
	calls   []scaleEnsureCall
	refuse  map[string][]error
	gate    chan struct{}
	started func(routing.Target)
	arrived chan struct{}
	// during, when set, runs inside every call before it answers: whatever
	// changes while an ensure is under way.
	during func(routing.Target)
}

func (e *scaleEnsurer) EnsureRuntimeModel(ctx context.Context, target routing.Target) error {
	deadline, _ := ctx.Deadline()
	_, authed := ctx.Value(scaleAuthKey{}).(bool)
	e.mu.Lock()
	e.calls = append(e.calls, scaleEnsureCall{target: target, budget: time.Until(deadline), authed: authed})
	var err error
	if q := e.refuse[target.ServerID]; len(q) > 0 {
		err, e.refuse[target.ServerID] = q[0], q[1:]
	}
	gate, started, during := e.gate, e.started, e.during
	e.mu.Unlock()
	e.arrived <- struct{}{}
	if during != nil {
		during(target)
	}
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return fmt.Errorf("%w: %v", provider.ErrUnavailable, ctx.Err())
		}
	}
	if err == nil && started != nil {
		started(target)
	}
	return err
}

// refuseNext queues errs as the answers to serverID's next ensures.
func (e *scaleEnsurer) refuseNext(serverID string, errs ...error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.refuse[serverID] = append(e.refuse[serverID], errs...)
}

// hold makes every later ensure wait for the returned gate to close.
func (e *scaleEnsurer) hold() chan struct{} {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.gate = make(chan struct{})
	return e.gate
}

// scaleRefusal is the agent router's 409 refusal with code.
func scaleRefusal(code string) error {
	return provider.RouterStatusError(http.StatusConflict, []byte(`{"error":{"code":"`+code+`","message":"`+code+`"}}`))
}

type scalerFixture struct {
	t           *testing.T
	store       *scalerStore
	scores      scalerScores
	busy        scalerBusy
	unreachable scalerUnreachable
	features    *agentFeaturesRegistry
	statuses    *runtimeStatusRegistry
	ensurer     *scaleEnsurer
	instances   *instanceSource
	resolver    *routing.Resolver
	scaler      *instanceScaler
	cancelBase  context.CancelFunc

	mu    sync.Mutex
	now   time.Time
	rows  map[string][]RuntimeStatusDTO
	wakes []string
	woken chan string
}

func newScalerFixture(t *testing.T) *scalerFixture {
	return newScalerFixtureWithCooldown(t, 0)
}

// newScalerFixtureWithCooldown builds a scaler with no host yet; cooldown 0
// leaves the scaler's default.
func newScalerFixtureWithCooldown(t *testing.T, cooldown time.Duration) *scalerFixture {
	t.Helper()
	f := &scalerFixture{
		t:           t,
		store:       &scalerStore{specs: map[string]routing.RuntimeSpec{}, more: map[string][]routing.ModelMapping{}, stalled: map[string]bool{}},
		scores:      scalerScores{},
		busy:        scalerBusy{},
		unreachable: scalerUnreachable{},
		features:    newAgentFeaturesRegistry(),
		statuses:    newRuntimeStatusRegistry(),
		ensurer:     &scaleEnsurer{refuse: map[string][]error{}, arrived: make(chan struct{}, 64)},
		now:         time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC),
		rows:        map[string][]RuntimeStatusDTO{},
		woken:       make(chan string, 64),
	}
	f.ensurer.started = f.reportStarted
	src := newInstanceSource(f.store, f.features, f.statuses)
	src.now = f.clock
	f.instances = src
	base, cancel := context.WithCancel(context.Background())
	f.cancelBase = cancel
	f.resolver = routing.NewResolver(f.store, f.clock, f.unreachable)
	f.resolver.SetServerBusyChecker(f.busy)
	f.scaler = newInstanceScaler(instanceScalerDeps{
		store:     f.store,
		routing:   scalerRouting{Resolver: f.resolver, store: f.store, scores: f.scores},
		instances: src,
		statuses:  f.statuses,
		ensurer:   func() provider.RuntimeEnsurer { return f.ensurer },
		authCtx: func(ctx context.Context, _ routing.Target) context.Context {
			return context.WithValue(ctx, scaleAuthKey{}, true)
		},
		wake: f.wake,
		base: func() context.Context { return base },
		now:  f.clock,
	}, cooldown)
	t.Cleanup(func() {
		cancel()
		f.scaler.wait()
	})
	return f
}

// addHost adds server serverID offering gateway model gm (upstream m) through
// mapping map_<serverID> of server_agent application app_<serverID>, whose
// spec spec_<serverID> is replicable, on an agent that declares
// runtime_instances, scored score. edit adjusts the candidate and the spec.
func (f *scalerFixture) addHost(serverID string, score float64, edit ...func(*routing.MappingCandidate, *routing.RuntimeSpec)) {
	c := routing.MappingCandidate{
		Server:      routing.AIServer{ID: serverID, Domain: serverID + ".test", Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy},
		Application: routing.Application{ID: "app_" + serverID, ServerID: serverID, Type: routing.ProviderServerAgent, Port: 8081, Scheme: "http", APIToken: "plain:tok_" + serverID, TimeoutMS: 30000, Status: routing.ServerStatusActive},
		Mapping:     routing.ModelMapping{ID: "map_" + serverID, ApplicationID: "app_" + serverID, GatewayModelName: scaleModel, AppModelName: scaleUpstream, Status: routing.ServerStatusActive},
	}
	spec := routing.RuntimeSpec{ID: "spec_" + serverID, MappingID: c.Mapping.ID, Replicable: true, StartupTimeoutSeconds: 120}
	for _, e := range edit {
		e(&c, &spec)
	}
	f.store.mu.Lock()
	f.store.cands = append(f.store.cands, c)
	f.store.specs[c.Mapping.ID] = spec
	f.store.mu.Unlock()
	f.scores[c.Mapping.ID] = score
	f.features.Set(serverID, []string{"runtime_manager", runtimeInstancesFeature})
}

// report replaces serverID's runtime-status rows, as a telemetry sample does.
func (f *scalerFixture) report(serverID string, rows ...RuntimeStatusDTO) {
	f.mu.Lock()
	f.rows[serverID] = rows
	f.mu.Unlock()
	f.statuses.publish(serverID, rows)
}

// reportStarted adds a running row for the instance target addresses to its
// server's rows: the agent's next sample after it started the instance.
func (f *scalerFixture) reportStarted(target routing.Target) {
	f.store.mu.Lock()
	specID := f.store.specs[target.RouteID].ID
	f.store.mu.Unlock()
	f.mu.Lock()
	rows := append(slices.Clone(f.rows[target.ServerID]), specRow(specID, scaleUpstream, target.Instance(), agentSpecStateRunning))
	f.rows[target.ServerID] = rows
	f.mu.Unlock()
	f.statuses.publish(target.ServerID, rows)
}

// instancesOf is spec_<serverID>'s rows for its instances ks in state.
func instancesOf(serverID, state string, ks ...int) []RuntimeStatusDTO {
	rows := make([]RuntimeStatusDTO, 0, len(ks))
	for _, k := range ks {
		rows = append(rows, specRow("spec_"+serverID, scaleUpstream, k, state))
	}
	return rows
}

func (f *scalerFixture) clock() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

func (f *scalerFixture) advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(d)
}

func (f *scalerFixture) wake(serverID string) {
	f.mu.Lock()
	f.wakes = append(f.wakes, serverID)
	f.mu.Unlock()
	f.woken <- serverID
}

func (f *scalerFixture) woke() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.wakes)
}

// demand is a demand signal for gm a request able to use mappingIDs sends.
func (f *scalerFixture) demand(mappingIDs ...string) routing.DemandSignal {
	return routing.DemandSignal{Model: scaleModel, APIFlavor: routing.APIFlavorOpenAI, MappingIDs: mappingIDs}
}

// signal sends d and waits for the scale-up it may start to end.
func (f *scalerFixture) signal(d routing.DemandSignal) {
	f.scaler.Signal(d)
	f.scaler.wait()
}

// ensured lists the ensures asked for so far, as "<server> <address>".
func (f *scalerFixture) ensured() []string {
	f.ensurer.mu.Lock()
	defer f.ensurer.mu.Unlock()
	out := make([]string, 0, len(f.ensurer.calls))
	for _, c := range f.ensurer.calls {
		out = append(out, c.target.ServerID+" "+c.target.ProviderModel)
	}
	return out
}

// awaitEnsure waits for the next ensure to reach the agent.
func (f *scalerFixture) awaitEnsure() {
	f.t.Helper()
	select {
	case <-f.ensurer.arrived:
	case <-time.After(5 * time.Second):
		f.t.Fatal("no ensure reached the agent")
	}
}

func (f *scalerFixture) calls() []scaleEnsureCall {
	f.ensurer.mu.Lock()
	defer f.ensurer.mu.Unlock()
	return slices.Clone(f.ensurer.calls)
}

func (f *scalerFixture) wantEnsured(want ...string) {
	f.t.Helper()
	if got := f.ensured(); !slices.Equal(got, want) {
		f.t.Fatalf("ensures = %q, want %q", got, want)
	}
}

func TestScalerSingleFlightPerModel(t *testing.T) {
	f := newScalerFixture(t)
	f.addHost("srv_a", 50)
	f.addHost("srv_b", 50, func(c *routing.MappingCandidate, _ *routing.RuntimeSpec) { c.Mapping.GatewayModelName = "gm2" })
	other := routing.DemandSignal{Model: "gm2", APIFlavor: routing.APIFlavorOpenAI, MappingIDs: []string{"map_srv_b"}}
	gate := f.ensurer.hold()

	var signals sync.WaitGroup
	for range 10 {
		signals.Add(2)
		go func() {
			defer signals.Done()
			f.scaler.Signal(f.demand("map_srv_a"))
		}()
		go func() {
			defer signals.Done()
			f.scaler.Signal(other)
		}()
	}
	signals.Wait()
	close(gate)
	f.scaler.wait()
	got := f.ensured()
	slices.Sort(got)
	if !slices.Equal(got, []string{"srv_a m#1", "srv_b m#1"}) {
		t.Fatalf("ensures = %q, want one per model: ten concurrent signals for a model are one scale-up", got)
	}

	// Once a model's scale-up has ended, its next signal scales again.
	f.signal(f.demand("map_srv_a"))
	if got := f.ensured(); len(got) != 3 || got[2] != "srv_a m#2" {
		t.Fatalf("ensures = %q, want a third, srv_a m#2, once the first scale-up ended", got)
	}
}

func TestScalerPicksLowestFreeIndex(t *testing.T) {
	f := newScalerFixture(t)
	f.addHost("srv_a", 50)
	f.report("srv_a", append(instancesOf("srv_a", agentSpecStateRunning, 0, 1, 3), specRow("spec_srv_a", scaleUpstream, 4, "crashed"))...)
	f.signal(f.demand("map_srv_a"))
	f.wantEnsured("srv_a m#2")
}

// An address of the spec that is another mapping's literal upstream model on
// the same agent reaches that mapping's spec, never an instance of this one,
// so its index is skipped.
func TestScalerSkipsIndexNamedByAnotherMapping(t *testing.T) {
	f := newScalerFixture(t)
	f.addHost("srv_a", 50)
	f.store.more["app_srv_a"] = []routing.ModelMapping{
		{ID: "map_lit1", ApplicationID: "app_srv_a", GatewayModelName: "lit1", AppModelName: "m#1"},
		{ID: "map_lit2", ApplicationID: "app_srv_a", GatewayModelName: "lit2", AppModelName: "m#02"}, // not an address of m
		{ID: "map_lit3", ApplicationID: "app_srv_a", GatewayModelName: "lit3", AppModelName: "x#3"},  // another base
	}
	f.report("srv_a", instancesOf("srv_a", agentSpecStateRunning, 2)...)
	f.signal(f.demand("map_srv_a"))
	f.wantEnsured("srv_a m#3")
}

func TestNextInstanceIndex(t *testing.T) {
	all := func(n int) []int {
		ks := make([]int, 0, n)
		for k := 1; k <= n; k++ {
			ks = append(ks, k)
		}
		return ks
	}
	cases := []struct {
		name     string
		present  []int
		inFlight map[int]bool
		want     int
	}{
		{"none present", nil, nil, 1},
		{"a gap", []int{1, 3}, nil, 2},
		{"unsorted", []int{2, 1}, nil, 3},
		{"one being started", []int{1, 2}, map[int]bool{3: true}, 4},
		{"indexes no address carries are not present", []int{0, 64, -1}, nil, 1},
		{"every index present", all(routing.MaxInstanceIndex), nil, 0},
		{"the last one being started", all(routing.MaxInstanceIndex - 1), map[int]bool{routing.MaxInstanceIndex: true}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := nextInstanceIndex(tc.present, tc.inFlight); got != tc.want {
				t.Fatalf("nextInstanceIndex(%v, %v) = %d, want %d", tc.present, tc.inFlight, got, tc.want)
			}
		})
	}
}

func TestScalerRespectsMax(t *testing.T) {
	cases := []struct {
		name    string
		max     int
		present []int
		want    []string
	}{
		{"at max, instance 0 counted", 3, []int{1, 2}, nil},
		{"below max", 3, []int{1}, []string{"srv_a m#2"}},
		{"no max of its own", 0, []int{1, 2}, []string{"srv_a m#3"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newScalerFixture(t)
			f.addHost("srv_a", 50, func(_ *routing.MappingCandidate, s *routing.RuntimeSpec) { s.MaxInstances = tc.max })
			f.report("srv_a", instancesOf("srv_a", agentSpecStateRunning, tc.present...)...)
			f.signal(f.demand("map_srv_a"))
			f.wantEnsured(tc.want...)
		})
	}
}

// Only a ramp that stopped on performance measured a ceiling; a stop for
// resources or at a cap is a lower bound the scaler does not stop at.
func TestScalerRespectsPerformanceCeilingOnly(t *testing.T) {
	cases := []struct {
		name        string
		limit       string
		concurrency int
		present     []int
		want        []string
	}{
		{"performance ceiling reached", routing.AudioLimitPerformance, 2, []int{1}, nil},
		{"below the performance ceiling", routing.AudioLimitPerformance, 3, []int{1}, []string{"srv_a m#2"}},
		{"a measured ceiling of 0", routing.AudioLimitPerformance, 0, nil, nil},
		{"a resources stop", routing.AudioLimitResources, 2, []int{1}, []string{"srv_a m#2"}},
		{"a cap stop", routing.AudioLimitCap, 2, []int{1}, []string{"srv_a m#2"}},
		{"no ramp yet", "", 0, nil, []string{"srv_a m#1"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newScalerFixture(t)
			f.addHost("srv_a", 50, func(c *routing.MappingCandidate, _ *routing.RuntimeSpec) {
				c.Mapping.AudioConcurrencyLimit = tc.limit
				c.Mapping.AudioRealtimeConcurrency = tc.concurrency
			})
			f.report("srv_a", instancesOf("srv_a", agentSpecStateRunning, tc.present...)...)
			f.signal(f.demand("map_srv_a"))
			f.wantEnsured(tc.want...)
		})
	}
}

func TestScalerSkipsStartingSibling(t *testing.T) {
	cases := []struct {
		name string
		rows []RuntimeStatusDTO
		want []string
	}{
		{"a burst instance starting", append(instancesOf("srv_a", agentSpecStateRunning, 0), instancesOf("srv_a", agentSpecStateStarting, 1)...), nil},
		{"instance 0 starting", instancesOf("srv_a", agentSpecStateStarting, 0), nil},
		{"another spec starting", []RuntimeStatusDTO{specRow("spec_other", "o", 0, agentSpecStateStarting)}, []string{"srv_a m#1"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newScalerFixture(t)
			f.addHost("srv_a", 50)
			f.report("srv_a", tc.rows...)
			f.signal(f.demand("map_srv_a"))
			f.wantEnsured(tc.want...)
		})
	}
}

// Hosts are tried by routing score, best first, whatever order the request
// listed them in.
func TestScalerFallsBackToNextHostOnNoCapacity(t *testing.T) {
	f := newScalerFixture(t)
	f.addHost("srv_a", 90)
	f.addHost("srv_b", 50)
	d := f.demand("map_srv_b", "map_srv_a")
	f.ensurer.refuseNext("srv_a", scaleRefusal("runtime.instance_no_capacity"))

	f.signal(d)
	f.wantEnsured("srv_a m#1", "srv_b m#1")
	if got := f.woke(); !slices.Equal(got, []string{"srv_b"}) {
		t.Fatalf("wakes = %q, want srv_b's queue alone", got)
	}

	f.advance(defaultInstanceScaleCooldown - time.Millisecond)
	f.signal(d)
	f.wantEnsured("srv_a m#1", "srv_b m#1", "srv_b m#2")

	f.advance(time.Millisecond)
	f.signal(d)
	f.wantEnsured("srv_a m#1", "srv_b m#1", "srv_b m#2", "srv_a m#1")
}

func TestScalerUsesConfiguredCooldown(t *testing.T) {
	f := newScalerFixtureWithCooldown(t, 15*time.Second)
	f.addHost("srv_a", 50)
	f.ensurer.refuseNext("srv_a", scaleRefusal("runtime.instance_no_capacity"))
	f.signal(f.demand("map_srv_a"))
	f.advance(15*time.Second - time.Millisecond)
	f.signal(f.demand("map_srv_a"))
	f.wantEnsured("srv_a m#1")
	f.advance(time.Millisecond)
	f.signal(f.demand("map_srv_a"))
	f.wantEnsured("srv_a m#1", "srv_a m#1")
}

// A sibling whose demand is not measured yet holds the host back for 5 s
// only, and the same scale-up moves on to the next host.
func TestScalerDemandUnknownShortDelay(t *testing.T) {
	f := newScalerFixture(t)
	f.addHost("srv_a", 90)
	f.addHost("srv_b", 50)
	d := f.demand("map_srv_a", "map_srv_b")
	f.ensurer.refuseNext("srv_a", scaleRefusal("runtime.instance_demand_unknown"))
	f.ensurer.refuseNext("srv_b", scaleRefusal("runtime.instance_no_capacity"))

	f.signal(d)
	f.wantEnsured("srv_a m#1", "srv_b m#1")

	f.advance(5*time.Second - time.Millisecond)
	f.signal(d)
	f.wantEnsured("srv_a m#1", "srv_b m#1")

	f.advance(time.Millisecond)
	f.signal(d)
	f.wantEnsured("srv_a m#1", "srv_b m#1", "srv_a m#1")
}

// A scale-up asks each host at most once: a host whose demand-unknown delay
// runs out while another host is asked is left for the next demand signal.
func TestScalerAsksEachHostOncePerScaleUp(t *testing.T) {
	f := newScalerFixture(t)
	f.addHost("srv_a", 90)
	f.addHost("srv_b", 50)
	d := f.demand("map_srv_a", "map_srv_b")
	f.ensurer.refuseNext("srv_a", scaleRefusal("runtime.instance_demand_unknown"))
	f.ensurer.refuseNext("srv_b", scaleRefusal("runtime.instance_no_capacity"))
	f.ensurer.during = func(target routing.Target) {
		if target.ServerID == "srv_b" {
			f.advance(instanceDemandUnknownDelay)
		}
	}
	f.signal(d)
	f.wantEnsured("srv_a m#1", "srv_b m#1")
}

// The host to ask next is chosen when it is asked: a host a benchmark took
// while the one before it was being asked is not asked.
func TestScalerRechecksHostsBeforeEachAsk(t *testing.T) {
	f := newScalerFixture(t)
	f.addHost("srv_a", 90)
	f.addHost("srv_b", 50)
	f.ensurer.refuseNext("srv_a", scaleRefusal("runtime.instance_no_capacity"))
	f.ensurer.during = func(target routing.Target) {
		if target.ServerID == "srv_a" {
			f.busy["srv_b"] = true
		}
	}
	f.signal(f.demand("map_srv_a", "map_srv_b"))
	f.wantEnsured("srv_a m#1")
}

// Any other failure cools the host down like a refusal for room, is logged
// as a warning, and the scale-up moves on to the next host.
func TestScalerOtherErrorCoolsDownAndWarns(t *testing.T) {
	buf := withCapturedSlogAtTheDefaultLevel(t)
	f := newScalerFixture(t)
	f.addHost("srv_a", 90)
	f.addHost("srv_b", 50)
	d := f.demand("map_srv_a", "map_srv_b")
	f.ensurer.refuseNext("srv_a", fmt.Errorf("%w: connection refused", provider.ErrUnavailable))

	f.signal(d)
	f.wantEnsured("srv_a m#1", "srv_b m#1")
	if !findLogRecord(buf.Snapshot(), "WARN", "instance scaler") {
		t.Fatal("no WARN record from the instance scaler for a failed ensure")
	}

	f.advance(defaultInstanceScaleCooldown - time.Millisecond)
	f.signal(d)
	f.wantEnsured("srv_a m#1", "srv_b m#1", "srv_b m#2")
}

func TestScalerRestrictsToPinnedServer(t *testing.T) {
	f := newScalerFixture(t)
	f.addHost("srv_a", 90)
	f.addHost("srv_b", 50)
	d := f.demand("map_srv_a", "map_srv_b")
	d.ServerID = "srv_b"
	f.signal(d)
	f.wantEnsured("srv_b m#1")

	// A pin to a server that cannot take an instance is not moved elsewhere.
	f.busy["srv_b"] = true
	f.signal(d)
	f.wantEnsured("srv_b m#1")
}

// A mixed fleet: whatever keeps host A out of the scale-up, host B -- scored
// lower -- is the one asked, and A never is.
func TestScalerSkipsBenchmarkBusyAndOldAgents(t *testing.T) {
	cases := []struct {
		name  string
		spoil func(f *scalerFixture)
	}{
		{"benchmark busy", func(f *scalerFixture) { f.busy["srv_a"] = true }},
		{"agent without runtime_instances", func(f *scalerFixture) { f.features.Set("srv_a", []string{"runtime_manager"}) }},
		{"agent features not known yet", func(f *scalerFixture) { f.features.Forget("srv_a") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newScalerFixture(t)
			f.addHost("srv_a", 90)
			f.addHost("srv_b", 50)
			tc.spoil(f)
			f.signal(f.demand("map_srv_a", "map_srv_b"))
			f.wantEnsured("srv_b m#1")
		})
	}
}

func TestScalerSkipsUnusableCandidates(t *testing.T) {
	cases := []struct {
		name   string
		edit   func(*routing.MappingCandidate, *routing.RuntimeSpec)
		spoil  func(f *scalerFixture)
		demand []string
	}{
		{name: "application unreachable", spoil: func(f *scalerFixture) { f.unreachable["app_srv_a"] = true }},
		{name: "server unhealthy", edit: func(c *routing.MappingCandidate, _ *routing.RuntimeSpec) {
			c.Server.HealthStatus = routing.HealthUnhealthy
		}},
		{name: "server not active", edit: func(c *routing.MappingCandidate, _ *routing.RuntimeSpec) {
			c.Server.Status = routing.ServerStatusMaintenance
		}},
		{name: "spec not replicable", edit: func(_ *routing.MappingCandidate, s *routing.RuntimeSpec) { s.Replicable = false }},
		{name: "not an application with an agent", edit: func(c *routing.MappingCandidate, _ *routing.RuntimeSpec) { c.Application.Type = routing.ProviderVLLM }},
		{name: "a mapping the request cannot use", demand: []string{"map_srv_b"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newScalerFixture(t)
			if tc.edit != nil {
				f.addHost("srv_a", 90, tc.edit)
			} else {
				f.addHost("srv_a", 90)
			}
			f.addHost("srv_b", 50)
			if tc.spoil != nil {
				tc.spoil(f)
			}
			demand := tc.demand
			if demand == nil {
				demand = []string{"map_srv_a", "map_srv_b"}
			}
			f.signal(f.demand(demand...))
			f.wantEnsured("srv_b m#1")
		})
	}
}

// The ensure carries the target's credential and runs for the spec's startup
// timeout plus 10 s -- the agent's own 30 s when the spec sets none.
func TestScalerEnsureTimeoutAndCredential(t *testing.T) {
	cases := []struct {
		name    string
		startup int
		want    time.Duration
	}{
		{"spec's startup timeout", 120, 130 * time.Second},
		{"agent's default startup timeout", 0, 40 * time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newScalerFixture(t)
			f.addHost("srv_a", 50, func(_ *routing.MappingCandidate, s *routing.RuntimeSpec) { s.StartupTimeoutSeconds = tc.startup })
			f.signal(f.demand("map_srv_a"))
			calls := f.calls()
			if len(calls) != 1 {
				t.Fatalf("ensures = %d, want 1", len(calls))
			}
			c := calls[0]
			if c.budget > tc.want || c.budget < tc.want-time.Second {
				t.Fatalf("ensure deadline %v away, want %v", c.budget, tc.want)
			}
			if !c.authed {
				t.Fatal("the ensure's context did not pass through the upstream auth wrapper")
			}
			tgt := c.target
			if tgt.Provider != routing.ProviderServerAgent || tgt.Endpoint != "http://srv_a.test:8081" || tgt.ProviderModel != "m#1" ||
				tgt.InstanceKey != "map_srv_a#1" || tgt.RouteID != "map_srv_a" || tgt.Model != scaleModel || tgt.APIToken != "plain:tok_srv_a" {
				t.Fatalf("target = %+v, want instance 1 of map_srv_a on srv_a's agent with its application's token", tgt)
			}
		})
	}
}

// A started instance wakes as many parked requests on its server as it
// serves at once -- once routing can see it, so a woken request finds it.
func TestScalerWakesQueueOnSuccess(t *testing.T) {
	parallel := func(_ *routing.MappingCandidate, s *routing.RuntimeSpec) { s.InstanceParallelism = 2 }

	t.Run("reported running", func(t *testing.T) {
		f := newScalerFixture(t)
		f.addHost("srv_a", 50, parallel)
		f.signal(f.demand("map_srv_a"))
		if got := f.woke(); !slices.Equal(got, []string{"srv_a", "srv_a"}) {
			t.Fatalf("wakes = %q, want srv_a twice", got)
		}
	})

	t.Run("not before the status registry reports it", func(t *testing.T) {
		f := newScalerFixture(t)
		f.addHost("srv_a", 50, parallel)
		f.ensurer.started = nil
		f.scaler.visibleWait = time.Hour
		f.scaler.Signal(f.demand("map_srv_a"))
		f.awaitEnsure()
		select {
		case id := <-f.woken:
			t.Fatalf("woke %s before the instance was reported running", id)
		case <-time.After(50 * time.Millisecond):
		}
		f.report("srv_a", instancesOf("srv_a", agentSpecStateRunning, 1)...)
		f.scaler.wait()
		if got := f.woke(); !slices.Equal(got, []string{"srv_a", "srv_a"}) {
			t.Fatalf("wakes = %q, want srv_a twice once reported", got)
		}
	})

	// An instance routing never sees wakes no one -- a woken request would not
	// find it -- and the host is left alone for 5 s, so the requests' next
	// signals do not ensure it again turn after turn.
	t.Run("not reported within the wait", func(t *testing.T) {
		buf := withCapturedSlogAtTheDefaultLevel(t)
		f := newScalerFixture(t)
		f.addHost("srv_a", 50, parallel)
		f.ensurer.started = nil
		f.scaler.visibleWait = time.Millisecond
		f.signal(f.demand("map_srv_a"))
		if got := f.woke(); len(got) != 0 {
			t.Fatalf("wakes = %q, want none for an instance routing does not see", got)
		}
		if !findLogRecord(buf.Snapshot(), "WARN", "did not show up") {
			t.Fatal("no WARN record for a started instance that never showed up")
		}
		f.advance(instanceUnseenDelay - time.Millisecond)
		f.signal(f.demand("map_srv_a"))
		f.wantEnsured("srv_a m#1")
		f.advance(time.Millisecond)
		f.signal(f.demand("map_srv_a"))
		f.wantEnsured("srv_a m#1", "srv_a m#1")
	})

	t.Run("refused", func(t *testing.T) {
		f := newScalerFixture(t)
		f.addHost("srv_a", 50, parallel)
		f.ensurer.refuseNext("srv_a", scaleRefusal("runtime.instance_no_capacity"))
		f.signal(f.demand("map_srv_a"))
		if got := f.woke(); len(got) != 0 {
			t.Fatalf("wakes = %q, want none for a refused instance", got)
		}
	})
}

// The server's shutdown ends an ensure in flight, leaves the host without a
// cooldown, and no later signal starts another.
func TestScalerShutdownCancelsInFlightEnsure(t *testing.T) {
	f := newScalerFixture(t)
	f.addHost("srv_a", 50)
	f.ensurer.hold()
	f.scaler.Signal(f.demand("map_srv_a"))
	f.awaitEnsure()
	f.cancelBase()
	f.scaler.wait()
	if got := f.woke(); len(got) != 0 {
		t.Fatalf("wakes = %q, want none after a shutdown", got)
	}
	f.scaler.mu.Lock()
	held := len(f.scaler.notBefore)
	f.scaler.mu.Unlock()
	if held != 0 {
		t.Fatalf("hosts held off = %d, want none: a shutdown says nothing about the host", held)
	}
	f.signal(f.demand("map_srv_a"))
	f.wantEnsured("srv_a m#1")
}

// scaleWiringProvider is the test server's mock provider that also starts
// agent instances: each ensure is handed to the test.
type scaleWiringProvider struct {
	provider.Mock
	ensures chan routing.Target
}

func (p scaleWiringProvider) EnsureRuntimeModel(_ context.Context, target routing.Target) error {
	p.ensures <- target
	return nil
}

// New wires the scaler as the resolver's demand sink: a request parked
// behind a busy instance 0 makes it start instance 1, and once telemetry
// reports that instance running the scaler's wake hands it to the request.
// The queue's periodic recheck is off, so only that wake can.
func TestInstanceScalerIsWiredAsTheDemandSink(t *testing.T) {
	srv := NewTestServer()
	seedReplicableRuntimeSpec(t, srv)
	srv.Active.admission.recheck = 0
	ensures := make(chan routing.Target, 4)
	srv.Provider = scaleWiringProvider{Mock: provider.NewMock(), ensures: ensures}
	const features = `"runtime_manager","runtime_model_probe","runtime_instances"`
	ingestInstanceRows(t, srv, features, instRow(0, "running", 1, 0))
	srv.Active.Add(ActiveRequest{ID: "held0", ServerID: "mock-host-qwen", InstanceKey: "map_rspec_inst#0"})
	defer srv.Active.Remove("held0")
	t.Cleanup(srv.instanceScaler.wait)

	type result struct {
		target routing.Target
		err    error
	}
	done := make(chan result, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go func() {
		target, err := srv.Resolver.Resolve(ctx, auth.Token{}, inference.Request{Model: "inst-model", APIFlavor: "openai_chat"})
		done <- result{target, err}
	}()

	var target routing.Target
	select {
	case target = <-ensures:
	case res := <-done:
		t.Fatalf("Resolve returned %q, %v before any instance was started", res.target.InstanceKey, res.err)
	case <-time.After(5 * time.Second):
		t.Fatal("the parked request started no instance")
	}
	if target.ServerID != "mock-host-qwen" || target.ProviderModel != "inst-model#1" {
		t.Fatalf("ensured %s on %s, want inst-model#1 on mock-host-qwen", target.ProviderModel, target.ServerID)
	}
	ingestInstanceRows(t, srv, features, instRow(0, "running", 1, 0), instRow(1, "running", 0, 0))
	select {
	case res := <-done:
		if res.err != nil || res.target.InstanceKey != "map_rspec_inst#1" {
			t.Fatalf("woken Resolve = %q, %v; want the new instance map_rspec_inst#1", res.target.InstanceKey, res.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the started instance did not wake the parked request")
	}
}

// The agent still reports a draining or stopped instance by its index; an
// ensure of that address is not a fresh start, so the index is not free.
func TestScalerSkipsIndexesTheAgentStillReports(t *testing.T) {
	f := newScalerFixture(t)
	f.addHost("srv_a", 50)
	f.report("srv_a",
		specRow("spec_srv_a", scaleUpstream, 0, agentSpecStateRunning),
		specRow("spec_srv_a", scaleUpstream, 1, agentSpecStateRunning),
		specRow("spec_srv_a", scaleUpstream, 2, "draining"),
		specRow("spec_srv_a", scaleUpstream, 3, "stopped"),
		specRow("spec_other", "o", 4, "draining"), // another spec's index blocks nothing
	)
	f.signal(f.demand("map_srv_a"))
	f.wantEnsured("srv_a m#4")
}

// A burst instance resting at a failure keeps its row on the agent until its
// index is named again, and an ensure of it is the retry (the agent retires a
// crashed or start_failed row and starts afresh, and re-runs admission on a
// not_permitted one). Routing never names such a row, so the scaler must: were
// the index counted as taken, a failure at the last index below max_instances
// would stop the spec scaling on that host until the next config push.
func TestScalerReusesAnIndexRestingAtAFailure(t *testing.T) {
	for _, state := range []string{"crashed", "start_failed", "not_permitted"} {
		t.Run(state, func(t *testing.T) {
			f := newScalerFixture(t)
			f.addHost("srv_a", 50, func(_ *routing.MappingCandidate, s *routing.RuntimeSpec) { s.MaxInstances = 2 })
			f.report("srv_a",
				specRow("spec_srv_a", scaleUpstream, 0, agentSpecStateRunning),
				specRow("spec_srv_a", scaleUpstream, 1, state),
			)
			f.signal(f.demand("map_srv_a"))
			f.wantEnsured("srv_a m#1")
		})
	}
}

// An index at or above max_instances is one the agent refuses: a spec whose
// lowest free index is there is at its ceiling, passed over quietly -- no
// ensure, no hold -- and scaled again as soon as an index below frees.
func TestScalerRespectsMaxByIndex(t *testing.T) {
	f := newScalerFixture(t)
	f.addHost("srv_a", 90, func(_ *routing.MappingCandidate, s *routing.RuntimeSpec) { s.MaxInstances = 3 })
	f.addHost("srv_b", 50)
	f.report("srv_a", append(instancesOf("srv_a", agentSpecStateRunning, 0), instancesOf("srv_a", "draining", 1, 2)...)...)
	f.signal(f.demand("map_srv_a", "map_srv_b"))
	f.wantEnsured("srv_b m#1")
	f.scaler.mu.Lock()
	_, held := f.scaler.notBefore[scaleKey{serverID: "srv_a", mappingID: "map_srv_a"}]
	f.scaler.mu.Unlock()
	if held {
		t.Fatal("srv_a is held off: a host at its ceiling by index is passed over, not cooled down")
	}

	f.report("srv_a", append(instancesOf("srv_a", agentSpecStateRunning, 0), instancesOf("srv_a", "draining", 1)...)...)
	f.signal(f.demand("map_srv_a"))
	f.wantEnsured("srv_b m#1", "srv_a m#2")
}

// A signal that names no mapping -- the scaler cannot tell which the request
// could use -- may get an instance of any mapping of the model.
func TestScalerSignalWithoutMappingIDsUsesTheModelsMappings(t *testing.T) {
	f := newScalerFixture(t)
	f.addHost("srv_b", 50)
	f.addHost("srv_a", 90)
	f.signal(f.demand())
	f.wantEnsured("srv_a m#1")

	pinned := f.demand()
	pinned.ServerID = "srv_b"
	f.signal(pinned)
	f.wantEnsured("srv_a m#1", "srv_b m#1")
}

// A signal arriving while a scale-up with another pin runs gets a pass of its
// own once that one ends -- unless that one started an instance that serves
// it: on the signal's pinned server, or anywhere for an unpinned signal.
// Signals with the running scale-up's own pin are folded into it.
func TestScalerFollowsUpSignalsWithAnotherPin(t *testing.T) {
	pinnedTo := func(f *scalerFixture, serverID string) routing.DemandSignal {
		d := f.demand("map_srv_a", "map_srv_b", "map_srv_c")
		d.ServerID = serverID
		return d
	}
	cases := []struct {
		name    string
		first   string   // the running scale-up's pin
		arrive  []string // the pins of the signals that arrive while it runs
		refuse  string   // the server whose agent refuses its first ensure
		ensured []string
	}{
		{"pinned during an unpinned scale-up", "", []string{"srv_b"}, "", []string{"srv_a m#1", "srv_b m#1"}},
		{"pinned to where the scale-up started one", "", []string{"srv_a"}, "", []string{"srv_a m#1"}},
		{"two servers, each once", "", []string{"srv_c", "srv_b", "srv_c", ""}, "", []string{"srv_a m#1", "srv_c m#1", "srv_b m#1"}},
		{"pinned during a scale-up pinned elsewhere", "srv_b", []string{"srv_c"}, "", []string{"srv_b m#1", "srv_c m#1"}},
		{"pinned during a scale-up pinned there", "srv_b", []string{"srv_b"}, "", []string{"srv_b m#1"}},
		{"unpinned during a pinned scale-up that started one", "srv_b", []string{""}, "", []string{"srv_b m#1"}},
		{"unpinned during a pinned scale-up that started none", "srv_b", []string{""}, "srv_b", []string{"srv_b m#1", "srv_a m#1"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newScalerFixture(t)
			f.addHost("srv_a", 90)
			f.addHost("srv_b", 50)
			f.addHost("srv_c", 40)
			if tc.refuse != "" {
				f.ensurer.refuseNext(tc.refuse, scaleRefusal(runtimeInstanceNoCapacityCode))
			}
			gate := f.ensurer.hold()
			f.scaler.Signal(pinnedTo(f, tc.first))
			f.awaitEnsure()
			for _, pin := range tc.arrive {
				f.scaler.Signal(pinnedTo(f, pin))
			}
			close(gate)
			f.scaler.wait()
			f.wantEnsured(tc.ensured...)
		})
	}
}

// The follow-up queue tells signals apart by everything that decides which
// instance serves them -- the pinned server, the mappings the request could use
// and its flavor -- not by the pin alone: a signal for other mappings is neither
// folded into a running scale-up with its pin nor dropped as a duplicate of a
// queued one, and the pass that ended serves a queued signal only with an
// instance of one of its mappings, on its server when it is pinned.
func TestScalerFollowUpKeysByMappings(t *testing.T) {
	signal := func(f *scalerFixture, serverID string, mappingIDs ...string) routing.DemandSignal {
		d := f.demand(mappingIDs...)
		d.ServerID = serverID
		return d
	}
	type arrival struct {
		pin      string
		mappings []string
	}
	cases := []struct {
		name          string
		firstPin      string
		firstMappings []string
		arrive        []arrival
		ensured       []string
	}{
		{"same pin, other mappings: a pass of its own", "", []string{"map_srv_a"}, []arrival{{"", []string{"map_srv_b"}}}, []string{"srv_a m#1", "srv_b m#1"}},
		{"same pin, same mappings: folded", "", []string{"map_srv_a"}, []arrival{{"", []string{"map_srv_a"}}}, []string{"srv_a m#1"}},
		{"queued twice for other mappings: one pass each", "", []string{"map_srv_a"}, []arrival{{"", []string{"map_srv_b"}}, {"", []string{"map_srv_c"}}, {"", []string{"map_srv_b"}}}, []string{"srv_a m#1", "srv_b m#1", "srv_c m#1"}},
		{"unpinned, not served by an instance of another mapping", "srv_b", []string{"map_srv_a", "map_srv_b"}, []arrival{{"", []string{"map_srv_a"}}}, []string{"srv_b m#1", "srv_a m#1"}},
		{"unpinned, served by an instance of one of its mappings", "srv_b", []string{"map_srv_a", "map_srv_b"}, []arrival{{"", []string{"map_srv_a", "map_srv_b"}}}, []string{"srv_b m#1"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newScalerFixture(t)
			f.addHost("srv_a", 90)
			f.addHost("srv_b", 50)
			f.addHost("srv_c", 40)
			gate := f.ensurer.hold()
			f.scaler.Signal(signal(f, tc.firstPin, tc.firstMappings...))
			f.awaitEnsure()
			for _, a := range tc.arrive {
				f.scaler.Signal(signal(f, a.pin, a.mappings...))
			}
			close(gate)
			f.scaler.wait()
			f.wantEnsured(tc.ensured...)
		})
	}
}

// A queued signal that names no mappings is served by the instance the running pass
// started only when it asks for the same flavor: the pass chose among the model's
// mappings of its own flavor, and a mapping of the model need not serve another.
func TestScalerFollowUpRespectsFlavorWithoutMappings(t *testing.T) {
	for _, tc := range []struct {
		name    string
		flavor  string
		ensured []string
	}{
		{"same flavor: served by the started instance", routing.APIFlavorOpenAI, []string{"srv_a m#1"}},
		{"another flavor: a pass of its own", routing.APIFlavorOpenAIAudio, []string{"srv_a m#1", "srv_a m#2"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newScalerFixture(t)
			f.addHost("srv_a", 90)
			gate := f.ensurer.hold()
			f.scaler.Signal(f.demand())
			f.awaitEnsure()
			queued := f.demand()
			queued.ServerID, queued.APIFlavor = "srv_a", tc.flavor // another key than the running pass's
			f.scaler.Signal(queued)
			close(gate)
			f.scaler.wait()
			f.wantEnsured(tc.ensured...)
		})
	}
}

// Every store read a scale-up makes is bounded: a stalled store ends the
// scale-up -- or, for the scores, leaves the request's order -- instead of
// holding the model's single flight, and the next signal scales again once
// the store answers.
func TestScalerBoundsStoreReads(t *testing.T) {
	cases := []struct {
		read string
		want []string // ensured while the read stalls
	}{
		{"mappings", nil},
		{"spec", nil},
		{"byApp", nil},
		{"score", []string{"srv_b m#1"}}, // the request listed srv_b first
	}
	for _, tc := range cases {
		t.Run(tc.read, func(t *testing.T) {
			f := newScalerFixture(t)
			f.addHost("srv_a", 90)
			f.addHost("srv_b", 50)
			f.scaler.readTimeout = 20 * time.Millisecond
			// The instance source's spec cache is warm, so the reads that stall
			// are the scale-up's own.
			for _, c := range f.store.cands {
				if _, ok := f.instances.InstanceView(context.Background(), c.Server.ID, c.Mapping); !ok {
					t.Fatalf("no instance view for %s", c.Mapping.ID)
				}
			}
			f.store.setStalled(tc.read, true)
			d := f.demand("map_srv_b", "map_srv_a")
			f.scaler.Signal(d)
			ended := make(chan struct{})
			go func() {
				f.scaler.wait()
				close(ended)
			}()
			select {
			case <-ended:
			case <-time.After(5 * time.Second):
				t.Fatalf("a stalled %s read held the scale-up", tc.read)
			}
			f.wantEnsured(tc.want...)

			f.store.setStalled(tc.read, false)
			f.advance(time.Minute) // past the instance source's memory of a failed spec read
			f.signal(d)
			if got := f.ensured(); len(got) != len(tc.want)+1 {
				t.Fatalf("ensures = %q, want one more once the store answers", got)
			}
		})
	}
}

// The ensure carries the very target routing builds for the instance: the
// scaler does not assemble one of its own.
func TestScalerEnsuresRoutingsInstanceTarget(t *testing.T) {
	f := newScalerFixture(t)
	f.addHost("srv_a", 50)
	f.report("srv_a", instancesOf("srv_a", agentSpecStateRunning, 1)...)
	f.signal(f.demand("map_srv_a"))
	calls := f.calls()
	if len(calls) != 1 {
		t.Fatalf("ensures = %d, want 1", len(calls))
	}
	pool, err := f.store.ActiveMappingsForModel(context.Background(), scaleModel, routing.APIFlavorOpenAI)
	if err != nil || len(pool) != 1 {
		t.Fatalf("candidates = %d, %v; want one", len(pool), err)
	}
	want, err := f.resolver.InstanceTarget(context.Background(), pool[0], 2, routing.APIFlavorOpenAI)
	if err != nil {
		t.Fatalf("InstanceTarget: %v", err)
	}
	if !reflect.DeepEqual(calls[0].target, want) {
		t.Fatalf("ensured target\n %+v\nwant routing's\n %+v", calls[0].target, want)
	}
}

// A host routing would not send a request to is not scaled onto, whatever
// gate keeps it out: the scaler asks routing's own gates.
func TestScalerAsksRoutingsGates(t *testing.T) {
	f := newScalerFixture(t)
	f.addHost("srv_a", 90, func(c *routing.MappingCandidate, _ *routing.RuntimeSpec) {
		c.Server.HealthStatus = routing.HealthDegraded
	})
	f.addHost("srv_b", 50, func(c *routing.MappingCandidate, _ *routing.RuntimeSpec) {
		c.Server.Status = routing.ServerStatusMaintenance
	})
	f.signal(f.demand("map_srv_a", "map_srv_b"))
	f.wantEnsured("srv_a m#1") // degraded is selectable; maintenance is not
}
