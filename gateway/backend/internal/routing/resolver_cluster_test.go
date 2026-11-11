// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package routing

import (
	"context"
	"errors"
	"op-ai-gateway/internal/auth"
	"slices"
	"sync"
	"testing"
	"time"
)

// The cluster gate's three states on the request path: a formed cluster head
// serves, a cold or forming one parks the request (at capacity, with a demand
// signal naming the head, under the cluster's own bound), and one cooling
// down or otherwise unusable is excluded -- with the cluster's own code when
// nothing else serves the model, never no_healthy_host.

const (
	clusterTestHead    = "rspec_head"
	clusterTestMapping = "map_head"
	clusterTestServer  = "srv_head"
	clusterTestModel   = "big-model"
)

// fakeClusterGate answers a verdict per mapping id and counts every question.
type fakeClusterGate struct {
	mu       sync.Mutex
	verdicts map[string]ClusterGateVerdict
	asked    []string
}

func (g *fakeClusterGate) ClusterGateState(_ context.Context, _, mappingID string) ClusterGateVerdict {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.asked = append(g.asked, mappingID)
	return g.verdicts[mappingID]
}

func (g *fakeClusterGate) set(mappingID string, v ClusterGateVerdict) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.verdicts[mappingID] = v
}

func (g *fakeClusterGate) questions() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.asked)
}

func parkable(bound time.Duration) ClusterGateVerdict {
	return ClusterGateVerdict{State: ClusterGateParkable, Head: clusterTestHead, ParkBound: bound, Code: "runtime.cluster_unavailable"}
}

// clusterHeadStore seeds one server_agent server serving clusterTestModel
// through the head's mapping, its application with the given admission queue
// timeout and request timeout.
func clusterHeadStore(t *testing.T, now time.Time, queueSecs, timeoutMS int) *MemoryStore {
	t.Helper()
	ctx := context.Background()
	store := NewMemoryStore()
	if err := store.CreateAIServer(ctx, AIServer{ID: clusterTestServer, Name: "head", Domain: "head.test", Status: ServerStatusActive, HealthStatus: HealthHealthy, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateAIServer: %v", err)
	}
	if err := store.CreateApplication(ctx, Application{ID: "app_head", ServerID: clusterTestServer, Type: ProviderServerAgent, Port: 8081, Scheme: "http", APIFlavors: []string{APIFlavorOpenAI}, Priority: 10, Weight: 50, TimeoutMS: timeoutMS, AdmissionQueueTimeoutSeconds: queueSecs, AffinityTTLSeconds: 1800, Status: ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateApplication: %v", err)
	}
	if err := store.CreateMapping(ctx, ModelMapping{ID: clusterTestMapping, ApplicationID: "app_head", GatewayModelName: clusterTestModel, AppModelName: "big-up", Status: ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateMapping: %v", err)
	}
	if err := store.UpsertTelemetry(ctx, ServerTelemetry{ServerID: clusterTestServer, ReportedAt: now, LatencyMS: 100, ProviderHealth: "{}", Capabilities: "{}", RawSummary: "{}", UpdatedAt: now}); err != nil {
		t.Fatalf("UpsertTelemetry: %v", err)
	}
	return store
}

func clusterResolver(store *MemoryStore, now time.Time, gate *fakeClusterGate) (*Resolver, *fakeDemandSink) {
	r := NewResolver(store, func() time.Time { return now }, nil)
	r.SetClusterGate(gate)
	sink := &fakeDemandSink{}
	r.SetDemandSink(sink)
	return r, sink
}

// A request for a cold cluster head parks: it is at capacity rather than
// unservable, it signals the head's demand right before it waits, and once
// the cluster formed (the next turn's verdict) it is served by the head.
func TestResolveParksColdClusterAndSignals(t *testing.T) {
	now := instanceCapacityNow
	gate := &fakeClusterGate{verdicts: map[string]ClusterGateVerdict{clusterTestMapping: parkable(2 * time.Minute)}}
	r, sink := clusterResolver(clusterHeadStore(t, now, 0, 600000), now, gate)
	var signalledBeforeWait []int
	adm := &fakeAdmission{onWait: func(int) error {
		signalledBeforeWait = append(signalledBeforeWait, len(sink.got))
		gate.set(clusterTestMapping, ClusterGateVerdict{State: ClusterGateFormed, Head: clusterTestHead})
		return nil
	}}
	r.SetAdmissionController(adm)

	target, err := r.Resolve(context.Background(), auth.Token{}, chatRequest(clusterTestModel))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if target.RouteID != clusterTestMapping {
		t.Fatalf("target = %q, want the head's mapping once formed", target.RouteID)
	}
	if adm.calls != 1 || !slices.Equal(adm.gotServers, []string{clusterTestServer}) {
		t.Fatalf("waits = %d on %v, want one wait on the head's server", adm.calls, adm.gotServers)
	}
	if !slices.Equal(signalledBeforeWait, []int{1}) {
		t.Fatalf("signals seen at the wait = %v, want [1]: the head's demand, sent before the wait", signalledBeforeWait)
	}
	want := DemandSignal{Model: clusterTestModel, APIFlavor: APIFlavorOpenAI, MappingIDs: []string{clusterTestMapping}, ClusterHead: clusterTestHead}
	if len(sink.got) != 1 || !demandSignalsEqual(sink.got, []DemandSignal{want}) || sink.got[0].ClusterHead != clusterTestHead {
		t.Fatalf("demand signals = %+v, want exactly %+v", sink.got, want)
	}
}

// A formed cluster head is an ordinary candidate: served at once, no wait,
// no signal.
func TestResolveServesFormedCluster(t *testing.T) {
	now := instanceCapacityNow
	gate := &fakeClusterGate{verdicts: map[string]ClusterGateVerdict{clusterTestMapping: {State: ClusterGateFormed, Head: clusterTestHead}}}
	r, sink := clusterResolver(clusterHeadStore(t, now, 0, 600000), now, gate)
	adm := timeoutAdmission()
	r.SetAdmissionController(adm)

	target, err := r.Resolve(context.Background(), auth.Token{}, chatRequest(clusterTestModel))
	if err != nil || target.RouteID != clusterTestMapping {
		t.Fatalf("Resolve = %q, %v; want the formed head", target.RouteID, err)
	}
	if adm.calls != 0 || len(sink.got) != 0 {
		t.Fatalf("waits %d, signals %+v; want neither", adm.calls, sink.got)
	}
}

// An excluded head (cooling down after a failed start, or otherwise unusable)
// is dropped: alone it fails the request with the cluster's own code -- never
// no_healthy_host -- without waiting or signalling; beside another candidate
// the request is served there.
func TestResolveCoolingDownFailsWithClusterCode(t *testing.T) {
	for _, code := range []string{"runtime.cluster_start_failed", "runtime.cluster_unavailable"} {
		t.Run(code+" alone", func(t *testing.T) {
			now := instanceCapacityNow
			gate := &fakeClusterGate{verdicts: map[string]ClusterGateVerdict{clusterTestMapping: {State: ClusterGateExcluded, Head: clusterTestHead, Code: code}}}
			r, sink := clusterResolver(clusterHeadStore(t, now, 0, 600000), now, gate)
			adm := timeoutAdmission()
			r.SetAdmissionController(adm)

			_, err := r.Resolve(context.Background(), auth.Token{}, chatRequest(clusterTestModel))
			var ce *ClusterUnavailableError
			if !errors.As(err, &ce) || ce.Code != code || ce.Head != clusterTestHead {
				t.Fatalf("Resolve err = %v, want a ClusterUnavailableError %s for the head", err, code)
			}
			if errors.Is(err, ErrNoHealthyHost) {
				t.Fatalf("Resolve err = %v matches ErrNoHealthyHost, want the cluster's code only", err)
			}
			if err.Error() != code {
				t.Fatalf("Error() = %q, want the stable code %q", err.Error(), code)
			}
			if adm.calls != 0 || len(sink.got) != 0 {
				t.Fatalf("waits %d, signals %+v; want neither", adm.calls, sink.got)
			}
		})
	}
	t.Run("beside another candidate", func(t *testing.T) {
		now := instanceCapacityNow
		store := clusterHeadStore(t, now, 0, 600000)
		addAgentMapping(t, store, now, "srv_other", "map_other", clusterTestModel, "big-up", 0, 0)
		gate := &fakeClusterGate{verdicts: map[string]ClusterGateVerdict{clusterTestMapping: {State: ClusterGateExcluded, Head: clusterTestHead, Code: "runtime.cluster_start_failed"}}}
		r, _ := clusterResolver(store, now, gate)
		r.SetAdmissionController(timeoutAdmission())

		target, err := r.Resolve(context.Background(), auth.Token{}, chatRequest(clusterTestModel))
		if err != nil || target.RouteID != "map_other" {
			t.Fatalf("Resolve = %q, %v; want the other candidate", target.RouteID, err)
		}
	})
}

// A request parked on a cold head waits for the cluster's own bound -- not
// the application's admission queue timeout -- clipped by the request's
// timeout (the application's timeout_ms).
func TestResolveParkBoundIsClusterBound(t *testing.T) {
	for _, tc := range []struct {
		name      string
		timeoutMS int
		want      time.Duration
	}{
		{"the cluster's bound", 600000, 2 * time.Minute},
		{"clipped by timeout_ms", 30000, 30 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := instanceCapacityNow
			gate := &fakeClusterGate{verdicts: map[string]ClusterGateVerdict{clusterTestMapping: parkable(2 * time.Minute)}}
			r, _ := clusterResolver(clusterHeadStore(t, now, 5, tc.timeoutMS), now, gate)
			adm := timeoutAdmission()
			r.SetAdmissionController(adm)

			if _, err := r.Resolve(context.Background(), auth.Token{}, chatRequest(clusterTestModel)); !errors.Is(err, ErrAdmissionQueueTimeout) {
				t.Fatalf("Resolve err = %v, want ErrAdmissionQueueTimeout", err)
			}
			if adm.gotTimeout != tc.want {
				t.Fatalf("wait bound = %v, want %v (not the 5 s admission queue timeout)", adm.gotTimeout, tc.want)
			}
		})
	}
}

// A request that must not park (a gate that only selects, like the voice
// chat's) is served by a cold head at once -- the gate then starts the
// cluster itself -- and signals the head's demand once.
func TestResolveNoParkServesColdClusterHead(t *testing.T) {
	now := instanceCapacityNow
	gate := &fakeClusterGate{verdicts: map[string]ClusterGateVerdict{clusterTestMapping: parkable(time.Minute)}}
	r, sink := clusterResolver(clusterHeadStore(t, now, 0, 600000), now, gate)
	adm := timeoutAdmission()
	r.SetAdmissionController(adm)
	req := chatRequest(clusterTestModel)
	req.NoPark = true

	target, err := r.Resolve(context.Background(), auth.Token{}, req)
	if err != nil || target.RouteID != clusterTestMapping {
		t.Fatalf("Resolve = %q, %v; want the cold head", target.RouteID, err)
	}
	if adm.calls != 0 || len(sink.got) != 1 || sink.got[0].ClusterHead != clusterTestHead {
		t.Fatalf("waits %d, signals %+v; want no wait and the head's one signal", adm.calls, sink.got)
	}
}

// Only an agent candidate can be a cluster head, so no other candidate costs
// the gate a question.
func TestResolveAsksTheClusterGateOnlyForAgentCandidates(t *testing.T) {
	now := instanceCapacityNow
	store := clusterHeadStore(t, now, 0, 600000)
	ctx := context.Background()
	if err := store.CreateAIServer(ctx, AIServer{ID: "srv_mock", Name: "mock", Domain: "mock.test", Status: ServerStatusActive, HealthStatus: HealthHealthy, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateAIServer: %v", err)
	}
	if err := store.CreateApplication(ctx, Application{ID: "app_mock", ServerID: "srv_mock", Type: ProviderMock, Port: 8000, Scheme: "http", APIFlavors: []string{APIFlavorOpenAI}, TimeoutMS: 30000, Status: ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateApplication: %v", err)
	}
	if err := store.CreateMapping(ctx, ModelMapping{ID: "map_mock", ApplicationID: "app_mock", GatewayModelName: "plain-model", AppModelName: "plain", Status: ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateMapping: %v", err)
	}
	gate := &fakeClusterGate{verdicts: map[string]ClusterGateVerdict{}}
	r, _ := clusterResolver(store, now, gate)

	if _, err := r.Resolve(ctx, auth.Token{}, chatRequest("plain-model")); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if q := gate.questions(); len(q) != 0 {
		t.Fatalf("gate asked about %v, want no question for a candidate without an agent", q)
	}
}

// A sticky pin to a cluster head that is not formed is passed over (and
// kept): the fresh selection parks the request for the cluster instead of
// sending it to a head that cannot answer.
func TestResolveAffinityPinToColdHeadParks(t *testing.T) {
	now := instanceCapacityNow
	store := clusterHeadStore(t, now, 0, 600000)
	gate := &fakeClusterGate{verdicts: map[string]ClusterGateVerdict{clusterTestMapping: parkable(time.Minute)}}
	r, sink := clusterResolver(store, now, gate)
	adm := timeoutAdmission()
	r.SetAdmissionController(adm)
	token := auth.Token{ID: "tok1", UserID: "u1", Active: true}
	key := AffinityKey{APITokenID: token.ID, Model: clusterTestModel, APIFlavor: NormalizeAPIFlavor("openai_chat")}
	if err := store.UpsertAffinity(context.Background(), RouteAffinity{
		ID: affinityID(key), APITokenID: token.ID, UserID: token.UserID, Model: clusterTestModel, APIFlavor: key.APIFlavor,
		ApplicationID: "app_head", ServerID: clusterTestServer, ExpiresAt: now.Add(time.Hour), LastUsedAt: now, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("UpsertAffinity: %v", err)
	}

	if _, err := r.Resolve(context.Background(), token, chatRequest(clusterTestModel)); !errors.Is(err, ErrAdmissionQueueTimeout) {
		t.Fatalf("Resolve err = %v, want the park's ErrAdmissionQueueTimeout", err)
	}
	if adm.calls != 1 || len(sink.got) != 1 || sink.got[0].ClusterHead != clusterTestHead {
		t.Fatalf("waits %d, signals %+v; want the fresh selection's park with the head's signal", adm.calls, sink.got)
	}
	if _, ok, _ := store.Affinity(context.Background(), key); !ok {
		t.Fatal("pin deleted, want it kept: the head is only not formed yet")
	}
}

// In a group, a member whose only candidate is an excluded cluster head is
// unavailable, and a group with nothing else to serve fails with the
// cluster's code.
func TestGroupExcludedClusterMemberFailsWithClusterCode(t *testing.T) {
	now := instanceCapacityNow
	store := clusterHeadStore(t, now, 0, 600000)
	gate := &fakeClusterGate{verdicts: map[string]ClusterGateVerdict{clusterTestMapping: {State: ClusterGateExcluded, Head: clusterTestHead, Code: "runtime.cluster_start_failed"}}}
	r, _ := clusterResolver(store, now, gate)
	r.SetAdmissionController(timeoutAdmission())
	r.SetGroupResolver(&fakeGroups{groups: map[string]fakeGroup{
		"big-group": {policy: GroupPolicy{FailoverMode: "sticky"}, members: []GroupMember{{ID: "gm_h", GroupID: "grp1", MemberGatewayName: clusterTestModel, Priority: 0}}},
	}})

	_, err := r.Resolve(context.Background(), auth.Token{}, chatRequest("big-group"))
	var ce *ClusterUnavailableError
	if !errors.As(err, &ce) || ce.Code != "runtime.cluster_start_failed" {
		t.Fatalf("Resolve err = %v, want the cluster's code", err)
	}
}

// A request that parked for a cold cluster keeps the cluster's bound once the
// cluster formed: woken by the formation, finding the formed head at its cap,
// it waits on for a slot until the later of that bound and the queue's own,
// rather than timing out at once on the short queue timeout it had outlived
// while the cluster formed.
func TestResolveKeepsTheClusterBoundOnceFormed(t *testing.T) {
	for _, model := range []string{clusterTestModel, "big-group"} {
		t.Run(model, func(t *testing.T) { keepsTheClusterBound(t, model) })
	}
}

func keepsTheClusterBound(t *testing.T, model string) {
	t.Helper()
	now := instanceCapacityNow
	store := clusterHeadStore(t, now, 5, 600000)
	m, err := store.MappingByID(context.Background(), clusterTestMapping)
	if err != nil {
		t.Fatalf("MappingByID: %v", err)
	}
	m.MaxConcurrency = 1
	if err := store.UpdateMapping(context.Background(), m); err != nil {
		t.Fatalf("UpdateMapping: %v", err)
	}
	gate := &fakeClusterGate{verdicts: map[string]ClusterGateVerdict{clusterTestMapping: parkable(2 * time.Minute)}}
	r, _ := clusterResolver(store, now, gate)
	r.SetServerActivityChecker(&fakeActivity{inFlight: map[string]int{clusterTestServer: 1}}, 0)
	r.SetGroupResolver(&fakeGroups{groups: map[string]fakeGroup{
		"big-group": {policy: GroupPolicy{FailoverMode: "sticky"}, members: []GroupMember{{ID: "gm_h", GroupID: "grp1", MemberGatewayName: clusterTestModel, Priority: 0}}},
	}})
	var timeouts []time.Duration
	adm := &fakeAdmission{}
	adm.onWait = func(call int) error {
		timeouts = append(timeouts, adm.gotTimeout)
		if call == 1 {
			gate.set(clusterTestMapping, ClusterGateVerdict{State: ClusterGateFormed, Head: clusterTestHead})
			return nil
		}
		return ErrAdmissionQueueTimeout
	}
	r.SetAdmissionController(adm)

	if _, err := r.Resolve(context.Background(), auth.Token{}, chatRequest(model)); !errors.Is(err, ErrAdmissionQueueTimeout) {
		t.Fatalf("Resolve err = %v, want the second wait's ErrAdmissionQueueTimeout", err)
	}
	if len(timeouts) != 2 || timeouts[0] != 2*time.Minute || timeouts[1] != 2*time.Minute {
		t.Fatalf("wait bounds = %v, want the cluster's 2m on both turns", timeouts)
	}
}

// A request that first parked for a slot on a formed head and finds the
// cluster cold on a later turn (the head exited meanwhile) waits for the
// cluster's bound from then on, in a group as on a plain model: the deadline
// is not the one its first turn fixed.
func TestResolveTakesTheClusterBoundOnALaterTurn(t *testing.T) {
	for _, model := range []string{clusterTestModel, "big-group"} {
		t.Run(model, func(t *testing.T) {
			now := instanceCapacityNow
			store := clusterHeadStore(t, now, 5, 600000)
			m, err := store.MappingByID(context.Background(), clusterTestMapping)
			if err != nil {
				t.Fatalf("MappingByID: %v", err)
			}
			m.MaxConcurrency = 1
			if err := store.UpdateMapping(context.Background(), m); err != nil {
				t.Fatalf("UpdateMapping: %v", err)
			}
			gate := &fakeClusterGate{verdicts: map[string]ClusterGateVerdict{clusterTestMapping: {State: ClusterGateFormed, Head: clusterTestHead}}}
			r, _ := clusterResolver(store, now, gate)
			r.SetServerActivityChecker(&fakeActivity{inFlight: map[string]int{clusterTestServer: 1}}, 0)
			r.SetGroupResolver(&fakeGroups{groups: map[string]fakeGroup{
				"big-group": {policy: GroupPolicy{FailoverMode: "sticky"}, members: []GroupMember{{ID: "gm_h", GroupID: "grp1", MemberGatewayName: clusterTestModel, Priority: 0}}},
			}})
			var timeouts []time.Duration
			adm := &fakeAdmission{}
			adm.onWait = func(call int) error {
				timeouts = append(timeouts, adm.gotTimeout)
				if call == 1 {
					gate.set(clusterTestMapping, parkable(2*time.Minute))
					return nil
				}
				return ErrAdmissionQueueTimeout
			}
			r.SetAdmissionController(adm)

			if _, err := r.Resolve(context.Background(), auth.Token{}, chatRequest(model)); !errors.Is(err, ErrAdmissionQueueTimeout) {
				t.Fatalf("Resolve err = %v, want the second wait's ErrAdmissionQueueTimeout", err)
			}
			if len(timeouts) != 2 || timeouts[0] != 5*time.Second || timeouts[1] != 2*time.Minute {
				t.Fatalf("wait bounds = %v, want the queue's 5s, then the cluster's 2m", timeouts)
			}
		})
	}
}
