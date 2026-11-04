// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/provider"
	"op-ai-gateway/internal/routing"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// The manual start and stop endpoints' tests run the real handler, portal
// authorization and registries over a seeded mapping (seedRuntimeAction) and a
// scripted agent (actionAgent).

const (
	actionServer  = "srv_ract"
	actionApp     = "app_ract"
	actionMapping = "map_ract"
	actionSpec    = "rspec_ract"
	actionModel   = "ract-model"
)

// actionFeatures is what a current agent declares.
var actionFeatures = []string{"runtime_manager", runtimeEnsureFeature, runtimeInstancesFeature, runtimeStopFeature}

// seedRuntimeAction seeds server srv_ract owned by usr_dev (the test
// principal), its server_agent application, mapping map_ract (upstream
// ract-model) and that mapping's spec, replicable with max_instances 4 unless
// edit changes it; the agent declares actionFeatures.
func seedRuntimeAction(t *testing.T, srv *Server, edit ...func(*routing.RuntimeSpec)) {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	if err := srv.Routes.CreateAIServer(ctx, routing.AIServer{ID: actionServer, Name: actionServer, Domain: actionServer + ".example.test", Provider: routing.ProviderMock, Endpoint: "mock://" + actionServer, Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("create server: %v", err)
	}
	if err := srv.Routes.SetServerOwners(ctx, actionServer, []string{"usr_dev"}); err != nil {
		t.Fatalf("set owners: %v", err)
	}
	if err := srv.Routes.CreateApplication(ctx, routing.Application{ID: actionApp, ServerID: actionServer, Type: routing.ProviderServerAgent, Port: 8081, Scheme: "http", APIFlavors: []string{routing.APIFlavorOpenAI}, TimeoutMS: 30000, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("create application: %v", err)
	}
	if err := srv.Routes.CreateMapping(ctx, routing.ModelMapping{ID: actionMapping, ApplicationID: actionApp, GatewayModelName: actionModel, AppModelName: actionModel, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("create mapping: %v", err)
	}
	spec := routing.RuntimeSpec{ID: actionSpec, MappingID: actionMapping, Enabled: true, Binary: "/usr/bin/llama-server", Args: "[]", Env: "{}", Replicable: true, MaxInstances: 4, StartupTimeoutSeconds: 30, CreatedAt: now, UpdatedAt: now}
	for _, e := range edit {
		e(&spec)
	}
	if err := srv.Routes.UpsertRuntimeSpec(ctx, spec); err != nil {
		t.Fatalf("upsert runtime spec: %v", err)
	}
	srv.AgentFeatures.Set(actionServer, actionFeatures)
}

// actionRows publishes the agent's status rows of spec rspec_ract, one per
// "k:state" entry, as a telemetry sample does.
func actionRows(srv *Server, entries ...string) {
	rows := make([]RuntimeStatusDTO, 0, len(entries))
	for _, e := range entries {
		k, state, _ := strings.Cut(e, ":")
		n := int(k[0] - '0')
		rows = append(rows, RuntimeStatusDTO{SpecID: actionSpec, Model: routing.InstanceAddress(actionModel, n), Instance: n, State: state})
	}
	srv.RuntimeStatus.publish(actionServer, rows)
}

// actionAgent is the scripted agent router behind the mapping: it records
// every ensure and stop, answers an ensure with ensureErr as it is once gate
// (when set) closes, or ErrUnavailable when the call's context ends first,
// and a stop with stopState or stopErr.
type actionAgent struct {
	provider.Mock

	mu        sync.Mutex
	ensures   []routing.Target
	stops     []routing.Target
	ensureErr error
	gate      chan struct{}
	arrived   chan struct{}
	stopState string
	stopErr   error
	deadline  time.Duration // the last ensure's remaining budget on arrival
}

func newActionAgent() *actionAgent {
	return &actionAgent{Mock: provider.NewMock(), arrived: make(chan struct{}, 16), stopState: provider.RuntimeStopStopping}
}

func (a *actionAgent) EnsureRuntimeModel(ctx context.Context, target routing.Target) error {
	deadline, _ := ctx.Deadline()
	a.mu.Lock()
	a.ensures = append(a.ensures, target)
	a.deadline = time.Until(deadline)
	gate := a.gate
	a.mu.Unlock()
	a.arrived <- struct{}{}
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return provider.ErrUnavailable
		}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.ensureErr
}

func (a *actionAgent) StopRuntimeModel(_ context.Context, target routing.Target) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.stops = append(a.stops, target)
	return a.stopState, a.stopErr
}

func (a *actionAgent) ensured() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]string, 0, len(a.ensures))
	for _, t := range a.ensures {
		out = append(out, t.ProviderModel)
	}
	return out
}

func (a *actionAgent) stopped() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]string, 0, len(a.stops))
	for _, t := range a.stops {
		out = append(out, t.ProviderModel)
	}
	return out
}

// newActionServer is a test server with the seeded mapping, agent a as its
// provider, the benchmark gate wired as cmd/gateway wires it, and the early
// answer window shortened to window.
func newActionServer(t *testing.T, a *actionAgent, window time.Duration, edit ...func(*routing.RuntimeSpec)) *Server {
	t.Helper()
	srv := NewTestServer()
	seedRuntimeAction(t, srv, edit...)
	srv.Provider = a
	svc, ok := srv.Portal.(*portal.Service)
	if !ok {
		t.Fatalf("test server's portal is %T, want *portal.Service", srv.Portal)
	}
	svc.SetBenchmarkReservationHook(srv.Benchmarks.ServerBusy)
	srv.runtimeStartWindow = window
	srv.runtimeStartVisibleWait = 50 * time.Millisecond
	t.Cleanup(srv.waitRuntimeStarts)
	return srv
}

func postAction(srv *Server, action, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, newJSONRequest(http.MethodPost, "/api/portal/mappings/"+actionMapping+"/runtime/"+action, body))
	return rec
}

// actionAnswer is a start or stop success body.
type actionAnswer struct {
	Instance int    `json:"instance"`
	State    string `json:"state"`
}

func decodeAnswer(t *testing.T, rec *httptest.ResponseRecorder) actionAnswer {
	t.Helper()
	var a actionAnswer
	if err := json.Unmarshal(rec.Body.Bytes(), &a); err != nil {
		t.Fatalf("decode %s: %v", rec.Body.String(), err)
	}
	return a
}

// featureOf is the feature a runtime.agent_feature_missing refusal names.
func featureOf(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Error struct {
			Feature string `json:"feature"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %s: %v", rec.Body.String(), err)
	}
	return body.Error.Feature
}

func TestRuntimeStartRefusals(t *testing.T) {
	cases := []struct {
		name        string
		spec        func(*routing.RuntimeSpec)
		setup       func(t *testing.T, srv *Server)
		wantStatus  int
		wantCode    string
		wantFeature string
	}{
		{
			name:       "force stopped",
			spec:       func(s *routing.RuntimeSpec) { s.AdminState = vramAdminStateForceStopped },
			wantStatus: http.StatusConflict, wantCode: codeRuntimeAdminStopped,
		},
		{
			name: "max instances reached",
			setup: func(_ *testing.T, srv *Server) {
				actionRows(srv, "0:running", "1:running", "2:starting", "3:running")
			},
			wantStatus: http.StatusConflict, wantCode: codeRuntimeMaxInstancesReached,
		},
		{
			name: "max instances 1 with instance 0 running",
			spec: func(s *routing.RuntimeSpec) { s.MaxInstances = 1 },
			setup: func(_ *testing.T, srv *Server) {
				actionRows(srv, "0:running")
			},
			wantStatus: http.StatusConflict, wantCode: codeRuntimeMaxInstancesReached,
		},
		{
			name:       "agent without runtime_ensure",
			setup:      func(_ *testing.T, srv *Server) { srv.AgentFeatures.Set(actionServer, []string{"runtime_manager"}) },
			wantStatus: http.StatusConflict, wantCode: codeRuntimeAgentFeatureMissing, wantFeature: runtimeEnsureFeature,
		},
		{
			name: "instance 1 on an agent without runtime_instances",
			setup: func(_ *testing.T, srv *Server) {
				srv.AgentFeatures.Set(actionServer, []string{"runtime_manager", runtimeEnsureFeature})
				actionRows(srv, "0:running")
			},
			wantStatus: http.StatusConflict, wantCode: codeRuntimeAgentFeatureMissing, wantFeature: runtimeInstancesFeature,
		},
		{
			name:       "agent not heard from",
			setup:      func(_ *testing.T, srv *Server) { srv.AgentFeatures.Forget(actionServer) },
			wantStatus: http.StatusServiceUnavailable, wantCode: codeRuntimeAgentUnavailable,
		},
		{
			name: "benchmark holds the server",
			setup: func(t *testing.T, srv *Server) {
				if _, ok := srv.Benchmarks.TryStart(actionServer, "load", "load", 1, time.Now(), func() {}); !ok {
					t.Fatal("reserve the server")
				}
			},
			wantStatus: http.StatusConflict, wantCode: "runtime_spec.server_benchmarking",
		},
		{
			name: "no spec",
			setup: func(t *testing.T, srv *Server) {
				if err := srv.Routes.DeleteRuntimeSpec(context.Background(), actionSpec); err != nil {
					t.Fatalf("delete spec: %v", err)
				}
			},
			wantStatus: http.StatusNotFound, wantCode: "runtime_spec.not_found",
		},
		{
			name: "caller without a claim to the mapping",
			setup: func(t *testing.T, srv *Server) {
				if err := srv.Routes.SetServerOwners(context.Background(), actionServer, []string{"usr_other"}); err != nil {
					t.Fatalf("set owners: %v", err)
				}
			},
			wantStatus: http.StatusNotFound, wantCode: "mapping.not_found",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			agent := newActionAgent()
			var edits []func(*routing.RuntimeSpec)
			if tc.spec != nil {
				edits = append(edits, tc.spec)
			}
			srv := newActionServer(t, agent, time.Second, edits...)
			if tc.setup != nil {
				tc.setup(t, srv)
			}
			rec := postAction(srv, "start", "")
			if rec.Code != tc.wantStatus || errorBodyOf(t, rec) != tc.wantCode {
				t.Fatalf("start = %d %s, want %d %s", rec.Code, rec.Body.String(), tc.wantStatus, tc.wantCode)
			}
			if tc.wantFeature != "" {
				if got := featureOf(t, rec); got != tc.wantFeature {
					t.Fatalf("feature = %q, want %q", got, tc.wantFeature)
				}
			}
			if got := agent.ensured(); len(got) != 0 {
				t.Fatalf("a refused start asked the agent for %v", got)
			}
			if held := srv.instanceClaims.heldBy(scaleKey{serverID: actionServer, mappingID: actionMapping}); len(held) != 0 {
				t.Fatalf("a refused start left claims %v", held)
			}
		})
	}
}

func TestRuntimeStartPicksTheInstance(t *testing.T) {
	cases := []struct {
		name  string
		spec  func(*routing.RuntimeSpec)
		rows  []string
		setup func(t *testing.T, srv *Server)
		want  int
	}{
		{name: "nothing runs", want: 0},
		{name: "instance 0 stopped", rows: []string{"0:stopped"}, want: 0},
		{name: "instance 0 running", rows: []string{"0:running"}, want: 1},
		{name: "instance 0 starting", rows: []string{"0:starting"}, want: 1},
		{name: "a failed index is free", rows: []string{"0:running", "1:running", "2:crashed"}, want: 2},
		{name: "a draining index is not", rows: []string{"0:running", "1:draining"}, want: 2},
		{name: "a gap", rows: []string{"0:running", "2:running"}, want: 1},
		{
			name: "an index another mapping's name takes",
			rows: []string{"0:running"},
			setup: func(t *testing.T, srv *Server) {
				now := time.Now().UTC()
				if err := srv.Routes.CreateMapping(context.Background(), routing.ModelMapping{ID: "map_lit", ApplicationID: actionApp, GatewayModelName: "lit", AppModelName: actionModel + "#1", Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
					t.Fatalf("create mapping: %v", err)
				}
			},
			want: 2,
		},
		{
			name: "an index the scaler is ensuring",
			rows: []string{"0:running"},
			setup: func(t *testing.T, srv *Server) {
				if !srv.instanceClaims.tryClaim(scaleKey{serverID: actionServer, mappingID: actionMapping}, 1) {
					t.Fatal("claim index 1")
				}
			},
			want: 2,
		},
		{name: "not replicable", spec: func(s *routing.RuntimeSpec) { s.Replicable = false }, rows: []string{"0:running"}, want: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			agent := newActionAgent()
			var edits []func(*routing.RuntimeSpec)
			if tc.spec != nil {
				edits = append(edits, tc.spec)
			}
			srv := newActionServer(t, agent, time.Second, edits...)
			actionRows(srv, tc.rows...)
			if tc.setup != nil {
				tc.setup(t, srv)
			}
			rec := postAction(srv, "start", "")
			if rec.Code != http.StatusOK {
				t.Fatalf("start = %d %s, want 200", rec.Code, rec.Body.String())
			}
			if got := decodeAnswer(t, rec); got.Instance != tc.want || got.State != "running" {
				t.Fatalf("answer = %+v, want instance %d running", got, tc.want)
			}
			want := routing.InstanceAddress(actionModel, tc.want)
			if got := agent.ensured(); !slices.Equal(got, []string{want}) {
				t.Fatalf("ensures = %v, want [%s]", got, want)
			}
		})
	}
}

// The ensure is bounded by the spec's startup timeout plus the scaler's margin,
// and carries the mapping's upstream target.
func TestRuntimeStartEnsureBoundAndTarget(t *testing.T) {
	agent := newActionAgent()
	srv := newActionServer(t, agent, time.Second)
	if rec := postAction(srv, "start", ""); rec.Code != http.StatusOK {
		t.Fatalf("start = %d %s", rec.Code, rec.Body.String())
	}
	agent.mu.Lock()
	defer agent.mu.Unlock()
	if want := 30*time.Second + instanceEnsureMargin; agent.deadline > want || agent.deadline < want-5*time.Second {
		t.Fatalf("ensure budget = %v, want about %v", agent.deadline, want)
	}
	tgt := agent.ensures[0]
	if tgt.ServerID != actionServer || tgt.RouteID != actionMapping || tgt.Provider != routing.ProviderServerAgent || tgt.Endpoint == "" {
		t.Fatalf("target = %+v, want the mapping's agent target", tgt)
	}
}

// An agent that refuses within the window is answered with its mapped code.
func TestRuntimeStartEarlyRefusalIsMapped(t *testing.T) {
	cases := []struct {
		code       string
		status     int
		wantStatus int
	}{
		{runtimeInstanceNoCapacityCode, http.StatusConflict, http.StatusConflict},
		{runtimeInstanceDemandUnknownCode, http.StatusConflict, http.StatusConflict},
		{"runtime.start_failed", http.StatusBadGateway, http.StatusBadGateway},
		{"runtime.admission_blocked", http.StatusServiceUnavailable, http.StatusServiceUnavailable},
		{"runtime.model_not_managed", http.StatusNotFound, http.StatusNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.code, func(t *testing.T) {
			agent := newActionAgent()
			agent.ensureErr = provider.RouterStatusError(tc.status, []byte(`{"error":{"code":"`+tc.code+`","message":"`+tc.code+`"}}`))
			srv := newActionServer(t, agent, time.Second)
			actionRows(srv, "0:running")
			rec := postAction(srv, "start", "")
			if rec.Code != tc.wantStatus || errorBodyOf(t, rec) != tc.code {
				t.Fatalf("start = %d %s, want %d %s", rec.Code, rec.Body.String(), tc.wantStatus, tc.code)
			}
			srv.waitRuntimeStarts()
			if held := srv.instanceClaims.heldBy(scaleKey{serverID: actionServer, mappingID: actionMapping}); len(held) != 0 {
				t.Fatalf("a refused ensure left claims %v", held)
			}
		})
	}
}

// A start the agent has not answered within the window is answered 202 and
// completes in the background: its index stays claimed until the agent reports
// the instance, so a second start picks the next one; a later failure is
// logged.
func TestRuntimeStartAnswers202AndCompletesInTheBackground(t *testing.T) {
	logs := withCapturedSlogAtTheDefaultLevel(t)
	agent := newActionAgent()
	agent.gate = make(chan struct{})
	srv := newActionServer(t, agent, 20*time.Millisecond)
	actionRows(srv, "0:running")
	key := scaleKey{serverID: actionServer, mappingID: actionMapping}

	rec := postAction(srv, "start", "")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("start = %d %s, want 202", rec.Code, rec.Body.String())
	}
	if got := decodeAnswer(t, rec); got.Instance != 1 || got.State != "starting" {
		t.Fatalf("answer = %+v, want instance 1 starting", got)
	}
	rec = postAction(srv, "start", "")
	if got := decodeAnswer(t, rec); rec.Code != http.StatusAccepted || got.Instance != 2 {
		t.Fatalf("second start = %d %s, want 202 for instance 2 while 1 is claimed", rec.Code, rec.Body.String())
	}

	agent.mu.Lock()
	agent.ensureErr = provider.RouterStatusError(http.StatusBadGateway, []byte(`{"error":{"code":"runtime.start_failed","message":"runtime.start_failed"}}`))
	agent.mu.Unlock()
	close(agent.gate)
	srv.waitRuntimeStarts()
	if held := srv.instanceClaims.heldBy(key); len(held) != 0 {
		t.Fatalf("claims after the failed starts = %v, want none", held)
	}
	if !findLogRecord(logs.Snapshot(), "WARN", "runtime start: starting an instance failed") {
		t.Fatalf("a background failure was not logged at warn: %+v", logs.Snapshot())
	}
}

// A background start that succeeds holds its index until the agent's status
// reports the instance, then releases it.
func TestRuntimeStartHoldsTheIndexUntilReported(t *testing.T) {
	agent := newActionAgent()
	agent.gate = make(chan struct{})
	srv := newActionServer(t, agent, 20*time.Millisecond)
	srv.runtimeStartVisibleWait = 10 * time.Second
	actionRows(srv, "0:running")
	key := scaleKey{serverID: actionServer, mappingID: actionMapping}
	if rec := postAction(srv, "start", ""); rec.Code != http.StatusAccepted {
		t.Fatalf("start = %d %s, want 202", rec.Code, rec.Body.String())
	}
	close(agent.gate)
	deadline := time.Now().Add(2 * time.Second)
	for !srv.instanceClaims.heldBy(key)[1] {
		if time.Now().After(deadline) {
			t.Fatal("index 1 is not held while the agent has not reported it")
		}
		time.Sleep(5 * time.Millisecond)
	}
	actionRows(srv, "0:running", "1:running")
	srv.waitRuntimeStarts()
	if held := srv.instanceClaims.heldBy(key); len(held) != 0 {
		t.Fatalf("claims after the instance was reported = %v, want none", held)
	}
}

// The server's shutdown ends a background start: no goroutine outlives it.
func TestRuntimeStartEndsWithShutdown(t *testing.T) {
	agent := newActionAgent()
	agent.gate = make(chan struct{})
	srv := newActionServer(t, agent, 20*time.Millisecond)
	base, cancel := context.WithCancel(context.Background())
	srv.SetBaseContext(base)
	if rec := postAction(srv, "start", ""); rec.Code != http.StatusAccepted {
		t.Fatalf("start = %d %s, want 202", rec.Code, rec.Body.String())
	}
	cancel()
	done := make(chan struct{})
	go func() {
		srv.waitRuntimeStarts()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a background start outlived the server's shutdown")
	}
}

func TestRuntimeStopPassesThrough(t *testing.T) {
	agent := newActionAgent()
	agent.stopState = provider.RuntimeStopStopped
	srv := newActionServer(t, agent, time.Second)
	rec := postAction(srv, "stop", `{"instance":2}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("stop = %d %s, want 200", rec.Code, rec.Body.String())
	}
	if got := decodeAnswer(t, rec); got.Instance != 2 || got.State != provider.RuntimeStopStopped {
		t.Fatalf("answer = %+v, want instance 2 stopped", got)
	}
	if got := agent.stopped(); !slices.Equal(got, []string{actionModel + "#2"}) {
		t.Fatalf("stops = %v, want [%s#2]", got, actionModel)
	}
	rec = postAction(srv, "stop", `{"instance":0}`)
	if got := decodeAnswer(t, rec); rec.Code != http.StatusOK || got.Instance != 0 {
		t.Fatalf("stop of instance 0 = %d %s", rec.Code, rec.Body.String())
	}
	if got := agent.stopped(); got[len(got)-1] != actionModel {
		t.Fatalf("instance 0's stop addressed %q, want %q", got[len(got)-1], actionModel)
	}
}

func TestRuntimeStopRefusals(t *testing.T) {
	refusal := func(status int, code string) error {
		return provider.RouterStatusError(status, []byte(`{"error":{"code":"`+code+`","message":"`+code+`"}}`))
	}
	cases := []struct {
		name        string
		body        string
		agentErr    error
		setup       func(t *testing.T, srv *Server)
		wantStatus  int
		wantCode    string
		wantFeature string
		wantAsked   bool
	}{
		{name: "protected", body: `{"instance":0}`, agentErr: refusal(http.StatusConflict, "runtime.instance_protected"), wantStatus: http.StatusConflict, wantCode: "runtime.instance_protected", wantAsked: true},
		{name: "not highest", body: `{"instance":1}`, agentErr: refusal(http.StatusConflict, "runtime.instance_not_highest"), wantStatus: http.StatusConflict, wantCode: "runtime.instance_not_highest", wantAsked: true},
		{name: "not managed", body: `{"instance":9}`, agentErr: refusal(http.StatusNotFound, "runtime.model_not_managed"), wantStatus: http.StatusNotFound, wantCode: "runtime.model_not_managed", wantAsked: true},
		{name: "agent unreachable", body: `{"instance":1}`, agentErr: provider.ErrUnavailable, wantStatus: http.StatusServiceUnavailable, wantCode: codeRuntimeAgentUnavailable, wantAsked: true},
		{
			name: "agent without runtime_stop", body: `{"instance":1}`,
			setup: func(_ *testing.T, srv *Server) {
				srv.AgentFeatures.Set(actionServer, []string{"runtime_manager", runtimeEnsureFeature})
			},
			wantStatus: http.StatusConflict, wantCode: codeRuntimeAgentFeatureMissing, wantFeature: runtimeStopFeature,
		},
		{
			name: "agent not heard from", body: `{"instance":1}`,
			setup:      func(_ *testing.T, srv *Server) { srv.AgentFeatures.Forget(actionServer) },
			wantStatus: http.StatusServiceUnavailable, wantCode: codeRuntimeAgentUnavailable,
		},
		{
			name: "benchmark holds the server", body: `{"instance":1}`,
			setup: func(t *testing.T, srv *Server) {
				if _, ok := srv.Benchmarks.TryStart(actionServer, "load", "load", 1, time.Now(), func() {}); !ok {
					t.Fatal("reserve the server")
				}
			},
			wantStatus: http.StatusConflict, wantCode: "runtime_spec.server_benchmarking",
		},
		{name: "no instance", body: `{}`, wantStatus: http.StatusBadRequest, wantCode: codeRuntimeInstanceInvalid},
		{name: "negative instance", body: `{"instance":-1}`, wantStatus: http.StatusBadRequest, wantCode: codeRuntimeInstanceInvalid},
		{name: "instance beyond 63", body: `{"instance":64}`, wantStatus: http.StatusBadRequest, wantCode: codeRuntimeInstanceInvalid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			agent := newActionAgent()
			agent.stopErr = tc.agentErr
			srv := newActionServer(t, agent, time.Second)
			if tc.setup != nil {
				tc.setup(t, srv)
			}
			rec := postAction(srv, "stop", tc.body)
			if rec.Code != tc.wantStatus || errorBodyOf(t, rec) != tc.wantCode {
				t.Fatalf("stop = %d %s, want %d %s", rec.Code, rec.Body.String(), tc.wantStatus, tc.wantCode)
			}
			if tc.wantFeature != "" {
				if got := featureOf(t, rec); got != tc.wantFeature {
					t.Fatalf("feature = %q, want %q", got, tc.wantFeature)
				}
			}
			if asked := len(agent.stopped()) > 0; asked != tc.wantAsked {
				t.Fatalf("agent asked = %v, want %v", asked, tc.wantAsked)
			}
		})
	}
}

func TestRuntimeActionRouting(t *testing.T) {
	srv := newActionServer(t, newActionAgent(), time.Second)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, newJSONRequest(http.MethodGet, "/api/portal/mappings/"+actionMapping+"/runtime/start", ""))
	if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != http.MethodPost {
		t.Fatalf("GET start = %d (Allow %q), want 405 with Allow: POST", rec.Code, rec.Header().Get("Allow"))
	}
	rec = postAction(srv, "restart", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("POST runtime/restart = %d, want 404", rec.Code)
	}
}

// The early answer maps the agent's errors; anything the provider reports
// without a router code is classified by its kind.
func TestRuntimeAgentErrorMapping(t *testing.T) {
	cases := []struct {
		err        error
		wantStatus int
		wantCode   string
	}{
		{provider.ErrTimeout, http.StatusGatewayTimeout, codeRuntimeAgentTimeout},
		{provider.ErrUnavailable, http.StatusServiceUnavailable, codeRuntimeAgentUnavailable},
		{errors.New("boom"), http.StatusBadGateway, codeRuntimeAgentError},
		{provider.RouterStatusError(http.StatusTeapot, []byte(`{"error":{"code":"runtime.something_new","message":"x"}}`)), http.StatusBadGateway, codeRuntimeAgentError},
	}
	for _, tc := range cases {
		rec := httptest.NewRecorder()
		writeRuntimeAgentError(rec, tc.err)
		if rec.Code != tc.wantStatus || errorBodyOf(t, rec) != tc.wantCode {
			t.Fatalf("writeRuntimeAgentError(%v) = %d %s, want %d %s", tc.err, rec.Code, rec.Body.String(), tc.wantStatus, tc.wantCode)
		}
	}
}

// runtime_stop is agent-declared only: the gateway reads it, never offers it.
func TestRuntimeStopFeatureIsAgentDeclaredOnly(t *testing.T) {
	if runtimeStopFeature != "runtime_stop" {
		t.Fatalf("runtimeStopFeature = %q, want runtime_stop (the agent's spelling)", runtimeStopFeature)
	}
	if slices.Contains(gatewayAgentFeatures, runtimeStopFeature) {
		t.Fatal("runtime_stop is on the gateway's own feature list, want it on the agent's only")
	}
}
