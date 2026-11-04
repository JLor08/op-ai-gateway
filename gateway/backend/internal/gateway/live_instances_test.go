// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/provider"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/store"
	"op-ai-gateway/internal/usage"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// The instances of a replicable spec are each their own live slot, and a request
// that loses its instance -- a live slot taken in the race between selection and
// acquire, an instance the agent retired a moment after the telemetry that
// reported it running -- is re-selected onto a sibling instead of failing. These
// tests drive the real Server, resolver and provider client against a fake agent
// router that serves every instance of two replicable specs on one server.

const (
	liveInstServer  = "srv-live-inst"
	liveInstASRSpec = "spec-asr-inst"
	liveInstASRMap  = "route-asr-inst"
	liveInstASRUp   = "asr-up"
	liveInstChatMap = "route-chat-inst"
	liveInstChatUp  = "chat-up"
)

// routerRefusal is the error a fake agent router answers for an address.
type routerRefusal struct {
	status int
	code   string
}

// instanceAgentRouter is a test agent router. POST /live/{model} relays like an
// audio.cpp /live child: one SSE delta per PCM read, transcript.text.done on a
// clean end. POST /v1/chat/completions, /v1/responses and
// /v1/audio/transcriptions answer for the model their body names. An address in
// refuse answers its refusal with the router's error envelope instead; a live
// refusal first reads refuseAfter bytes of the session's PCM, as a request the
// transport had begun to send before the answer.
type instanceAgentRouter struct {
	server *httptest.Server

	mu          sync.Mutex
	refuse      map[string]routerRefusal
	refuseAfter int
	served      []string       // every address a request arrived for, in order
	liveBytes   map[string]int // PCM bytes each live address received
}

func newInstanceAgentRouter(t *testing.T) *instanceAgentRouter {
	t.Helper()
	a := &instanceAgentRouter{refuse: map[string]routerRefusal{}, liveBytes: map[string]int{}}
	a.server = httptest.NewServer(http.HandlerFunc(a.serve))
	t.Cleanup(a.server.Close)
	return a
}

func (a *instanceAgentRouter) setRefusal(model string, status int, code string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.refuse[model] = routerRefusal{status: status, code: code}
}

func (a *instanceAgentRouter) servedModels() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.served)
}

func (a *instanceAgentRouter) bytesFor(model string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.liveBytes[model]
}

// arrive records model and answers its refusal, if it has one.
func (a *instanceAgentRouter) arrive(w http.ResponseWriter, model string) bool {
	a.mu.Lock()
	a.served = append(a.served, model)
	ref, refused := a.refuse[model]
	a.mu.Unlock()
	if !refused {
		return false
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(ref.status)
	fmt.Fprintf(w, `{"error":{"code":%q,"message":"%s"}}`, ref.code, ref.code)
	_ = http.NewResponseController(w).Flush()
	return true
}

func (a *instanceAgentRouter) serve(w http.ResponseWriter, r *http.Request) {
	switch {
	case strings.HasPrefix(r.URL.Path, "/live/"):
		a.serveLive(w, r, strings.TrimPrefix(r.URL.Path, "/live/"))
	case r.URL.Path == "/v1/chat/completions":
		var body struct {
			Model  string `json:"model"`
			Stream bool   `json:"stream"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if a.arrive(w, body.Model) {
			return
		}
		if body.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"served by %s\"}}]}\n\n", body.Model)
			fmt.Fprint(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":2,\"total_tokens\":3}}\n\n")
			fmt.Fprint(w, "data: [DONE]\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"choices":[{"index":0,"message":{"role":"assistant","content":"served by %s"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`, body.Model)
	case r.URL.Path == "/v1/responses":
		var body struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if a.arrive(w, body.Model) {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id":"resp_1","object":"response","model":%q,"output":[],"usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}`, body.Model)
	case r.URL.Path == "/v1/audio/transcriptions":
		model := ""
		if mr, err := r.MultipartReader(); err == nil {
			for {
				part, err := mr.NextPart()
				if err != nil {
					break
				}
				if part.FormName() == "model" {
					raw, _ := io.ReadAll(part)
					model = string(raw)
				}
				_ = part.Close()
			}
		}
		if a.arrive(w, model) {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, transcriptionOKBody)
	default:
		http.NotFound(w, r)
	}
}

func (a *instanceAgentRouter) serveLive(w http.ResponseWriter, r *http.Request, model string) {
	rc := http.NewResponseController(w)
	_ = rc.EnableFullDuplex()
	a.mu.Lock()
	_, refused := a.refuse[model]
	consume := a.refuseAfter
	a.mu.Unlock()
	if refused && consume > 0 {
		_, _ = io.ReadFull(r.Body, make([]byte, consume))
	}
	if a.arrive(w, model) {
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	_ = rc.Flush()
	buf := make([]byte, 4096)
	for {
		n, err := r.Body.Read(buf)
		if n > 0 {
			a.mu.Lock()
			a.liveBytes[model] += n
			a.mu.Unlock()
			fmt.Fprintf(w, "data: {\"type\":\"transcript.text.delta\",\"delta\":\"+%d\"}\n\n", n)
			_ = rc.Flush()
		}
		if err != nil {
			if err == io.EOF {
				fmt.Fprint(w, "data: {\"type\":\"transcript.text.done\",\"text\":\"done\"}\n\ndata: [DONE]\n\n")
				_ = rc.Flush()
			}
			return
		}
	}
}

// liveInstanceRow is a running row of spec's instance k, addressed as the agent
// addresses it, carrying active requests in flight on it.
func liveInstanceRow(spec, upstream string, k, active int) RuntimeStatusDTO {
	return RuntimeStatusDTO{SpecID: spec, Model: routing.InstanceAddress(upstream, k), Instance: k, State: "running", ActiveRequests: active}
}

// newInstanceRouterServer builds a Server whose one agent server runs two specs
// behind the fake agent router: asr-inst (route-asr-inst, upstream asr-up, a live-
// and buffered-transcription model) and chat-inst (route-chat-inst, upstream
// chat-up, chat with /v1/responses passed through). replicable sets both specs'
// replicable flag. The agent declares runtime_instances, fullduplex_proxy and
// multipart_proxy; the status rows are the caller's to publish.
func newInstanceRouterServer(t *testing.T, agent *instanceAgentRouter, replicable bool) *Server {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	tokens := auth.NewTokenStore()
	directory := portal.NewMemoryDirectory(tokens)
	directory.AddUser(store.User{ID: "usr_dev", Email: "dev@example.test", DisplayName: "Dev User", Role: "admin", Status: store.UserStatusActive, PreferredLanguage: "de", CreatedAt: now, UpdatedAt: now})
	if err := directory.CreatePlainToken(ctx, store.TokenRecord{ID: "tok_dev", UserID: "usr_dev", Name: "Dev Token", Status: store.TokenStatusActive, Scopes: `["gateway:use","admin"]`, CreatedAt: now, UpdatedAt: now}, "dev-secret"); err != nil {
		t.Fatalf("CreatePlainToken: %v", err)
	}
	up, err := url.Parse(agent.server.URL)
	if err != nil {
		t.Fatalf("parse agent url: %v", err)
	}
	port, err := strconv.Atoi(up.Port())
	if err != nil {
		t.Fatalf("agent port: %v", err)
	}
	routes := routing.NewMemoryStore()
	must := func(what string, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}
	must("CreateAIServer", routes.CreateAIServer(ctx, routing.AIServer{ID: liveInstServer, Name: "Instances", Domain: up.Hostname(), Provider: routing.ProviderVLLM, Endpoint: agent.server.URL, Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now}))
	must("CreateApplication", routes.CreateApplication(ctx, routing.Application{ID: "app-live-inst", ServerID: liveInstServer, Type: routing.ProviderServerAgent, Port: port, Scheme: "http", APIFlavors: []string{routing.APIFlavorOpenAI, routing.APIFlavorOpenAIAudio}, Priority: 10, Weight: 50, TimeoutMS: 30000, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}))
	must("CreateMapping asr", routes.CreateMapping(ctx, routing.ModelMapping{ID: liveInstASRMap, ApplicationID: "app-live-inst", GatewayModelName: "asr-inst", AppModelName: liveInstASRUp, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}))
	must("CreateMapping chat", routes.CreateMapping(ctx, routing.ModelMapping{ID: liveInstChatMap, ApplicationID: "app-live-inst", GatewayModelName: "chat-inst", AppModelName: liveInstChatUp, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}))
	must("UpsertMappingCapabilities", routes.UpsertMappingCapabilities(ctx, liveInstASRMap, []routing.CapabilityRow{
		{Capability: routing.CapabilityTranscription, Verdict: routing.CapabilityYes, Source: routing.CapabilitySourceManual, CheckedAt: now},
		{Capability: routing.CapabilityTranscriptionStreaming, Verdict: routing.CapabilityYes, Source: routing.CapabilitySourceManual, CheckedAt: now},
	}))
	must("UpsertRuntimeSpec asr", routes.UpsertRuntimeSpec(ctx, routing.RuntimeSpec{ID: liveInstASRSpec, MappingID: liveInstASRMap, Enabled: true, Binary: "/usr/bin/audio-server", Args: "[]", Env: "{}", APIFlavors: []string{routing.APIFlavorOpenAIAudio}, Replicable: replicable, MaxInstances: 4, CreatedAt: now, UpdatedAt: now}))
	must("UpsertRuntimeSpec chat", routes.UpsertRuntimeSpec(ctx, routing.RuntimeSpec{ID: "spec-chat-inst", MappingID: liveInstChatMap, Enabled: true, Binary: "/usr/bin/llama-server", Args: "[]", Env: "{}", APIFlavors: []string{routing.APIFlavorOpenAI}, ResponsesMode: routing.EndpointModePassthrough, Replicable: replicable, MaxInstances: 4, CreatedAt: now, UpdatedAt: now}))
	must("UpsertTelemetry", routes.UpsertTelemetry(ctx, routing.ServerTelemetry{ServerID: liveInstServer, ReportedAt: now, LatencyMS: 100, ProviderHealth: `{}`, Capabilities: `{}`, RawSummary: `{}`, UpdatedAt: now}))
	recorder := usage.NewRecorder()
	srv := New(ServerDeps{
		Tokens:   tokens,
		Usage:    recorder,
		Provider: provider.NewOpenAICompatibleClient(http.DefaultClient),
		Routes:   routes,
		Portal:   portal.NewService(portal.ServiceDeps{Users: directory, Tokens: directory, Usage: recorder, Routes: routes, Clock: time.Now, ModelLister: provider.NewMock()}),
	})
	srv.AgentFeatures.Set(liveInstServer, []string{"runtime_manager", "runtime_model_probe", runtimeInstancesFeature, fullduplexProxyFeature, multipartProxyFeature})
	return srv
}

// liveSessionResult is what a live session started off the test goroutine got.
type liveSessionResult struct {
	pw   *io.PipeWriter
	resp *http.Response
	err  error
}

// startLiveSession opens a bearer live session like startBearerLive, but reports
// instead of failing, so it may run on a goroutine of its own. A refusal written
// before the relay reads the request body leaves the server draining a body the
// client never ends, and the client waiting on it; after liveAnswerBound with no
// answer the session's body is ended, so such a refusal fails the test instead of
// hanging it.
func startLiveSession(ctx context.Context, ts *httptest.Server, model string) liveSessionResult {
	pr, pw := io.Pipe()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ts.URL+"/v1/audio/transcriptions/live?"+bearerLiveQuery(model), pr)
	if err != nil {
		return liveSessionResult{err: err}
	}
	req.Header.Set("Authorization", "Bearer dev-secret")
	guard := time.AfterFunc(liveAnswerBound, func() { _ = pw.CloseWithError(errors.New("no answer in time")) })
	resp, err := ts.Client().Do(req)
	guard.Stop()
	return liveSessionResult{pw: pw, resp: resp, err: err}
}

// liveAnswerBound is how long startLiveSession waits for an answer.
const liveAnswerBound = 5 * time.Second

// finishLiveSession ends a session cleanly and reads its SSE to the done event.
func finishLiveSession(t *testing.T, s liveSessionResult) {
	t.Helper()
	_ = s.pw.Close()
	readLiveUntilDone(t, bufio.NewReader(s.resp.Body))
	_ = s.resp.Body.Close()
}

// readLiveUntilDone reads a live session's SSE up to its transcript.text.done.
func readLiveUntilDone(t *testing.T, br *bufio.Reader) {
	t.Helper()
	done := false
	for !done {
		done = strings.Contains(readBearerSSE(t, br, 5*time.Second), "transcript.text.done")
	}
}

// establishLiveSession starts a session, requires 200 and exchanges one frame.
func establishLiveSession(t *testing.T, ctx context.Context, ts *httptest.Server) liveSessionResult {
	t.Helper()
	s := startLiveSession(ctx, ts, "asr-inst")
	if s.err != nil {
		t.Fatalf("live session: %v", s.err)
	}
	t.Cleanup(func() {
		_ = s.pw.Close()
		_ = s.resp.Body.Close()
	})
	if s.resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(s.resp.Body)
		t.Fatalf("live session status = %d, want 200; body = %s", s.resp.StatusCode, body)
	}
	return s
}

func publishASRInstances(srv *Server, ks ...int) {
	rows := make([]RuntimeStatusDTO, 0, len(ks))
	for _, k := range ks {
		rows = append(rows, liveInstanceRow(liveInstASRSpec, liveInstASRUp, k, 0))
	}
	srv.RuntimeStatus.publish(liveInstServer, rows)
}

// Two concurrent live sessions on a model with two running instances land on
// the two instances, each holding its own slot -- neither is refused busy.
func TestTwoLiveSessionsUseTwoInstances(t *testing.T) {
	agent := newInstanceAgentRouter(t)
	srv := newInstanceRouterServer(t, agent, true)
	publishASRInstances(srv, 0, 1)
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	first := establishLiveSession(t, ctx, ts)
	second := establishLiveSession(t, ctx, ts)
	served := agent.servedModels()
	slices.Sort(served)
	if !slices.Equal(served, []string{"asr-up", "asr-up#1"}) {
		t.Fatalf("live addresses served = %v, want one session on each instance [asr-up asr-up#1]", served)
	}
	if !srv.liveSessions.LiveHeld("route-asr-inst#0") || !srv.liveSessions.LiveHeld("route-asr-inst#1") {
		t.Fatal("both instance slots must be held while both sessions run")
	}
	finishLiveSession(t, first)
	finishLiveSession(t, second)
}

// A live session whose selected instance's slot is taken between selection and
// acquire is re-selected onto the other instance instead of being refused busy.
func TestLiveCollisionReselects(t *testing.T) {
	agent := newInstanceAgentRouter(t)
	srv := newInstanceRouterServer(t, agent, true)
	publishASRInstances(srv, 0, 1)
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	var raced string
	var racedRelease func()
	liveSlotAcquireHook = func(instanceKey string) {
		if raced != "" {
			return
		}
		raced = instanceKey
		racedRelease, _ = srv.liveSessions.acquire(instanceKey)
	}
	t.Cleanup(func() {
		liveSlotAcquireHook = nil
		if racedRelease != nil {
			racedRelease()
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	s := establishLiveSession(t, ctx, ts)
	if raced == "" || racedRelease == nil {
		t.Fatalf("the hook took no slot (raced %q)", raced)
	}
	other := map[string]string{"route-asr-inst#0": "asr-up#1", "route-asr-inst#1": "asr-up"}[raced]
	if served := agent.servedModels(); !slices.Equal(served, []string{other}) {
		t.Fatalf("live addresses served = %v, want only %q: the session lost %s and was re-selected", served, other, raced)
	}
	finishLiveSession(t, s)
}

// Every live slot of a replicable model held: the session waits in the admission
// queue -- signalling demand for a live slot -- and is admitted once a slot frees.
func TestLiveAllHeldReplicableParks(t *testing.T) {
	agent := newInstanceAgentRouter(t)
	srv := newInstanceRouterServer(t, agent, true)
	publishASRInstances(srv, 0, 1)
	sink := &recordingDemandSink{}
	srv.Resolver.SetDemandSink(sink)
	release0, _ := srv.liveSessions.acquire("route-asr-inst#0")
	release1, _ := srv.liveSessions.acquire("route-asr-inst#1")
	defer release0()
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	done := make(chan liveSessionResult, 1)
	go func() { done <- startLiveSession(ctx, ts, "asr-inst") }()
	select {
	case s := <-done:
		status := 0
		if s.resp != nil {
			status = s.resp.StatusCode
		}
		t.Fatalf("session answered (status %d, err %v) while every slot was held, want it to wait", status, s.err)
	case <-time.After(400 * time.Millisecond):
	}
	signals := sink.signals()
	if len(signals) == 0 || !signals[0].Live || !slices.Equal(signals[0].MappingIDs, []string{liveInstASRMap}) || signals[0].Model != "asr-inst" {
		t.Fatalf("demand signals = %+v, want a live signal for asr-inst naming %s", signals, liveInstASRMap)
	}

	release1()
	var s liveSessionResult
	select {
	case s = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the parked session was not admitted after a slot freed")
	}
	if s.err != nil || s.resp.StatusCode != http.StatusOK {
		t.Fatalf("admitted session: err %v status %v, want 200", s.err, s.resp)
	}
	if served := agent.servedModels(); !slices.Equal(served, []string{"asr-up#1"}) {
		t.Fatalf("live addresses served = %v, want the freed instance asr-up#1", served)
	}
	finishLiveSession(t, s)
}

// A mapping without instances keeps its one live slot: a second session is
// refused at once with transcription.live_busy, as before.
func TestLiveNonReplicableStillBusy(t *testing.T) {
	agent := newInstanceAgentRouter(t)
	srv := newInstanceRouterServer(t, agent, false)
	publishASRInstances(srv, 0)
	release, ok := srv.liveSessions.acquire("route-asr-inst#0")
	if !ok {
		t.Fatal("could not take the mapping's live slot")
	}
	defer release()

	started := time.Now()
	rec := postBearerLiveRecorder(t, srv, bearerLiveQuery("asr-inst"), devBearerHeader)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body = %s", rec.Code, rec.Body.String())
	}
	requireErrorCode(t, rec.Body.String(), liveBearerBusyCode)
	if waited := time.Since(started); waited > time.Second {
		t.Fatalf("the refusal took %v, want it at once (no queue for a mapping without instances)", waited)
	}
	if served := agent.servedModels(); len(served) != 0 {
		t.Fatalf("upstream reached for %v, want no relay", served)
	}
}

// recordingDemandSink is a concurrency-safe routing.DemandSink.
type recordingDemandSink struct {
	mu  sync.Mutex
	got []routing.DemandSignal
}

func (s *recordingDemandSink) Signal(d routing.DemandSignal) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.got = append(s.got, d)
}

func (s *recordingDemandSink) signals() []routing.DemandSignal {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.got)
}

// publishChatInstances reports chat-inst's instance 0 loaded down and the
// instances ks idle, so the resolver prefers an idle instance k >= 1.
func publishChatInstances(srv *Server, ks ...int) {
	rows := []RuntimeStatusDTO{liveInstanceRow("spec-chat-inst", liveInstChatUp, 0, 10)}
	for _, k := range ks {
		rows = append(rows, liveInstanceRow("spec-chat-inst", liveInstChatUp, k, 0))
	}
	srv.RuntimeStatus.publish(liveInstServer, rows)
}

func postInstanceJSON(t *testing.T, srv *Server, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer dev-secret")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

// A request routed to instance m#2, which the agent retired a moment after the
// telemetry that reported it running, is re-selected once, excluding that
// instance, and served by instance 0: the client never sees the agent's refusal,
// and the one usage row is the served attempt's. Every agent refusal that means
// "this instance is gone" qualifies, on every relay.
func TestVanishedInstanceReselectedOnce(t *testing.T) {
	const chatBody = `{"model":"chat-inst","messages":[{"role":"user","content":"hi"}]}`
	cases := []struct {
		name, path, body string
		status           int
		code             string
		wantBody         string
	}{
		{"chat, model not managed", "/v1/chat/completions", chatBody, http.StatusNotFound, "runtime.model_not_managed", "served by chat-up"},
		{"chat, no capacity", "/v1/chat/completions", chatBody, http.StatusConflict, "runtime.instance_no_capacity", "served by chat-up"},
		{"chat, demand unknown", "/v1/chat/completions", chatBody, http.StatusConflict, "runtime.instance_demand_unknown", "served by chat-up"},
		{"streamed chat", "/v1/chat/completions", `{"model":"chat-inst","stream":true,"messages":[{"role":"user","content":"hi"}]}`, http.StatusNotFound, "runtime.model_not_managed", "served by chat-up"},
		{"responses passthrough", "/v1/responses", `{"model":"chat-inst","input":"hi"}`, http.StatusConflict, "runtime.instance_no_capacity", `"model":"chat-up"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			agent := newInstanceAgentRouter(t)
			srv := newInstanceRouterServer(t, agent, true)
			publishChatInstances(srv, 2)
			agent.setRefusal("chat-up#2", tc.status, tc.code)

			rec := postInstanceJSON(t, srv, tc.path, tc.body)
			if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), tc.wantBody) {
				t.Fatalf("status %d body %s; want 200 served by instance 0 (%s)", rec.Code, rec.Body.String(), tc.wantBody)
			}
			if served := agent.servedModels(); !slices.Equal(served, []string{"chat-up#2", "chat-up"}) {
				t.Fatalf("addresses served = %v, want [chat-up#2 chat-up]: the vanished instance once, then instance 0", served)
			}
			rows := srv.Usage.All()
			if len(rows) != 1 || rows[0].Status != "success" || rows[0].RouteID != liveInstChatMap {
				t.Fatalf("usage rows = %+v, want one success row for %s", rows, liveInstChatMap)
			}
			if n := srv.Active.InstanceActivity("route-chat-inst#2"); n != 0 {
				t.Fatalf("in flight on the vanished instance = %d, want 0", n)
			}
		})
	}
}

// The re-selection happens once: a request whose second instance is gone too
// answers the agent's refusal, and a refusal from instance 0 -- whose address
// every mapping has always sent -- is never re-selected at all.
func TestVanishedInstanceReselectedOnlyOnce(t *testing.T) {
	t.Run("the second instance is gone too", func(t *testing.T) {
		agent := newInstanceAgentRouter(t)
		srv := newInstanceRouterServer(t, agent, true)
		publishChatInstances(srv, 2, 3)
		// Instance 0 at its parallelism of one, so both re-selections stay on k >= 1.
		srv.Active.Add(ActiveRequest{ID: "busy0", ServerID: liveInstServer, InstanceKey: "route-chat-inst#0"})
		defer srv.Active.Remove("busy0")
		agent.setRefusal("chat-up#2", http.StatusConflict, "runtime.instance_no_capacity")
		agent.setRefusal("chat-up#3", http.StatusConflict, "runtime.instance_no_capacity")

		rec := postInstanceJSON(t, srv, "/v1/chat/completions", `{"model":"chat-inst","messages":[{"role":"user","content":"hi"}]}`)
		if rec.Code == http.StatusOK {
			t.Fatalf("status 200, want the second refusal answered; body %s", rec.Body.String())
		}
		if served := agent.servedModels(); len(served) != 2 {
			t.Fatalf("addresses served = %v, want two attempts and no third", served)
		}
	})
	t.Run("instance 0 is never re-selected", func(t *testing.T) {
		agent := newInstanceAgentRouter(t)
		srv := newInstanceRouterServer(t, agent, true)
		srv.RuntimeStatus.publish(liveInstServer, []RuntimeStatusDTO{liveInstanceRow("spec-chat-inst", liveInstChatUp, 0, 0)})
		agent.setRefusal("chat-up", http.StatusNotFound, "runtime.model_not_managed")

		rec := postInstanceJSON(t, srv, "/v1/chat/completions", `{"model":"chat-inst","messages":[{"role":"user","content":"hi"}]}`)
		if rec.Code == http.StatusOK {
			t.Fatalf("status 200, want the refusal answered; body %s", rec.Body.String())
		}
		if served := agent.servedModels(); !slices.Equal(served, []string{"chat-up"}) {
			t.Fatalf("addresses served = %v, want instance 0 once", served)
		}
	})
}

// A buffered transcription sent to a vanished instance is re-selected too, and
// its multipart body names the instance it is finally sent to.
func TestVanishedInstanceReselectedForTranscription(t *testing.T) {
	agent := newInstanceAgentRouter(t)
	srv := newInstanceRouterServer(t, agent, true)
	srv.RuntimeStatus.publish(liveInstServer, []RuntimeStatusDTO{
		liveInstanceRow(liveInstASRSpec, liveInstASRUp, 0, 10),
		liveInstanceRow(liveInstASRSpec, liveInstASRUp, 1, 0),
	})
	agent.setRefusal("asr-up#1", http.StatusNotFound, "runtime.model_not_managed")

	body, ct := buildEditMultipart(t, validTranscriptionParts("asr-inst"))
	rec := postEditRaw(t, srv, http.MethodPost, "/v1/audio/transcriptions", body, ct, devBearerHeader)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "hello there") {
		t.Fatalf("status %d body %s; want the transcription from instance 0", rec.Code, rec.Body.String())
	}
	if served := agent.servedModels(); !slices.Equal(served, []string{"asr-up#1", "asr-up"}) {
		t.Fatalf("addresses served = %v, want [asr-up#1 asr-up]", served)
	}
	if rows := srv.Usage.All(); len(rows) != 1 || rows[0].Status != "success" {
		t.Fatalf("usage rows = %+v, want one success row", rows)
	}
}

// A live session whose instance answers that it is not running is re-selected
// once, and the instance it lands on receives the whole session's audio --
// including the PCM the first attempt had already taken before refusing.
func TestVanishedLiveInstanceReselectedWithItsAudio(t *testing.T) {
	agent := newInstanceAgentRouter(t)
	agent.refuseAfter = 320
	srv := newInstanceRouterServer(t, agent, true)
	srv.RuntimeStatus.publish(liveInstServer, []RuntimeStatusDTO{
		liveInstanceRow(liveInstASRSpec, liveInstASRUp, 0, 0),
		liveInstanceRow(liveInstASRSpec, liveInstASRUp, 1, 0),
		liveInstanceRow(liveInstASRSpec, liveInstASRUp, 2, 5),
	})
	// Instance 0's slot is held, so the session goes to the idle instance 1,
	// which the agent has already retired, and is re-selected onto instance 2.
	release0, _ := srv.liveSessions.acquire("route-asr-inst#0")
	defer release0()
	agent.setRefusal("asr-up#1", http.StatusNotFound, "runtime.model_not_running")
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	pr, pw := io.Pipe()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ts.URL+"/v1/audio/transcriptions/live?"+bearerLiveQuery("asr-inst"), pr)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer dev-secret")
	go func() { _, _ = pw.Write(make([]byte, 320)) }() // sent before any answer arrives
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body = %s", resp.StatusCode, body)
	}
	br := bufio.NewReader(resp.Body)
	if _, err := pw.Write(make([]byte, 640)); err != nil {
		t.Fatalf("write pcm: %v", err)
	}
	_ = pw.Close()
	readLiveUntilDone(t, br)
	if served := agent.servedModels(); !slices.Equal(served, []string{"asr-up#1", "asr-up#2"}) {
		t.Fatalf("live addresses served = %v, want [asr-up#1 asr-up#2]", served)
	}
	if got := agent.bytesFor("asr-up#2"); got != 960 {
		t.Fatalf("instance 2 received %d PCM bytes, want all 960 the client sent", got)
	}
	ev := requireOneBearerLiveRow(t, srv, usage.BillingUnitAudioSecond, 960.0/32000.0)
	if ev.Status != "success" || ev.RouteID != liveInstASRMap {
		t.Fatalf("row = %+v, want a success row for %s", ev, liveInstASRMap)
	}
}
