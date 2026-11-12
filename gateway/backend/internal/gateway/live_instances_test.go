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
	"op-ai-gateway/internal/capture"
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
	liveInstServer       = "srv-live-inst"
	liveInstASRSpec      = "spec-asr-inst"
	liveInstASRMap       = "route-asr-inst"
	liveInstASRUp        = "asr-up"
	liveInstASRPlainSpec = "spec-asr-plain"
	liveInstASRPlainMap  = "route-asr-plain"
	liveInstASRPlainUp   = "asr-plain"
	liveInstChatMap      = "route-chat-inst"
	liveInstChatUp       = "chat-up"
	liveInstTTSSpec      = "spec-tts-inst"
	liveInstTTSMap       = "route-tts-inst"
	liveInstTTSUp        = "tts-up"
	liveInstImgSpec      = "spec-img-inst"
	liveInstImgMap       = "route-img-inst"
	liveInstImgUp        = "img-up"
	liveInstSysSpec      = "spec-sys-inst"
	liveInstSysMap       = "route-sys-inst"
	liveInstSysUp        = "sys-up"
)

// routerRefusal is the error a fake agent router answers for an address.
type routerRefusal struct {
	status int
	code   string
}

// instanceAgentRouter is a test agent router. POST /live/{model} relays like an
// audio.cpp /live child: one SSE delta per PCM read, transcript.text.done on a
// clean end. POST /v1/chat/completions, /v1/responses, /v1/audio/speech,
// /v1/images/generations, and the multipart /v1/audio/transcriptions and
// /v1/images/edits answer for the model their body names. An address in refuse
// answers its refusal with the router's error envelope instead; a live refusal
// first reads refuseAfter bytes of the session's PCM, as a request the transport
// had begun to send before the answer. A chat completion for an address in stall
// answers only after that long, as a child still producing its first token.
// onArrive, when set, is called with every address as its request arrives, before
// it is answered, so a test can look at the gateway while the request is in flight.
// A streamed chat completion's content is chatAnswer when set ("served by <address>"
// otherwise).
type instanceAgentRouter struct {
	server *httptest.Server

	mu          sync.Mutex
	refuse      map[string]routerRefusal
	refuseAfter int
	stall       map[string]time.Duration
	served      []string       // every address a request arrived for, in order
	liveBytes   map[string]int // PCM bytes each live address received
	onArrive    func(model string)
	chatAnswer  string
}

// setChatAnswer sets the content every streamed chat completion answers with.
func (a *instanceAgentRouter) setChatAnswer(answer string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.chatAnswer = answer
}

// setOnArrive installs the arrival hook (see instanceAgentRouter).
func (a *instanceAgentRouter) setOnArrive(hook func(model string)) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.onArrive = hook
}

func (a *instanceAgentRouter) setStall(model string, d time.Duration) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.stall == nil {
		a.stall = map[string]time.Duration{}
	}
	a.stall[model] = d
}

// stallFor waits out model's stall, or until the request ends.
func (a *instanceAgentRouter) stallFor(r *http.Request, model string) {
	a.mu.Lock()
	d := a.stall[model]
	a.mu.Unlock()
	if d <= 0 {
		return
	}
	select {
	case <-time.After(d):
	case <-r.Context().Done():
	}
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

// arrive records model, runs the arrival hook, and answers model's refusal, if it
// has one.
func (a *instanceAgentRouter) arrive(w http.ResponseWriter, model string) bool {
	a.mu.Lock()
	a.served = append(a.served, model)
	ref, refused := a.refuse[model]
	hook := a.onArrive
	a.mu.Unlock()
	if hook != nil {
		hook(model)
	}
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
		a.stallFor(r, body.Model)
		if body.Stream {
			a.mu.Lock()
			content := a.chatAnswer
			a.mu.Unlock()
			if content == "" {
				content = "served by " + body.Model
			}
			quoted, _ := json.Marshal(content)
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":%s}}]}\n\n", quoted)
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
		if a.arrive(w, multipartModel(r)) {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, transcriptionOKBody)
	case r.URL.Path == "/v1/audio/speech":
		var body struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if a.arrive(w, body.Model) {
			return
		}
		w.Header().Set("Content-Type", "audio/wav")
		_, _ = w.Write(speechTestWAV())
	case r.URL.Path == "/v1/systemone":
		var body struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if a.arrive(w, body.Model) {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"model":"sys","answers":{"q":"yes"},"usage":{"input_tokens":1,"output_tokens":0}}`)
	case r.URL.Path == "/v1/images/generations":
		var body struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if a.arrive(w, body.Model) {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, imagesOKBody)
	case r.URL.Path == "/v1/images/edits":
		if a.arrive(w, multipartModel(r)) {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, imagesOKBody)
	default:
		http.NotFound(w, r)
	}
}

// multipartModel is the model part of a multipart request body, "" without one.
func multipartModel(r *http.Request) string {
	model := ""
	mr, err := r.MultipartReader()
	if err != nil {
		return model
	}
	for {
		part, err := mr.NextPart()
		if err != nil {
			return model
		}
		if part.FormName() == "model" {
			raw, _ := io.ReadAll(part)
			model = string(raw)
		}
		_ = part.Close()
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
	return newInstanceRouterServerWith(t, agent, instanceRouterConfig{replicable: replicable})
}

// instanceRouterConfig shapes newInstanceRouterServerWith's fixture.
type instanceRouterConfig struct {
	// replicable is both specs' replicable flag.
	replicable bool
	// asrParallelism is asr-inst's instance_parallelism (0 = one request at a time).
	asrParallelism int
	// queueTimeoutSecs is the application's admission_queue_timeout_seconds (0 = a
	// request parks until its context ends).
	queueTimeoutSecs int
	// plainASR adds route-asr-plain: a second mapping of asr-inst, upstream
	// asr-plain, whose spec is not replicable -- a mixed fleet.
	plainASR bool
	// voice wires what a voice chat session needs beyond routing: a chat store
	// and its cipher, the chat-run registry, and the internal loopback pair (the
	// secret "test-internal-secret" for the user usr_dev) its turns call /v1 with.
	voice bool
}

// newInstanceRouterServerWith is newInstanceRouterServer shaped by cfg.
func newInstanceRouterServerWith(t *testing.T, agent *instanceAgentRouter, cfg instanceRouterConfig) *Server {
	t.Helper()
	replicable := cfg.replicable
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
	must("CreateApplication", routes.CreateApplication(ctx, routing.Application{ID: "app-live-inst", ServerID: liveInstServer, Type: routing.ProviderServerAgent, Port: port, Scheme: "http", APIFlavors: []string{routing.APIFlavorOpenAI, routing.APIFlavorOpenAIAudio, routing.APIFlavorOpenAIImages, routing.APIFlavorDecisions}, Priority: 10, Weight: 50, TimeoutMS: 30000, AdmissionQueueTimeoutSeconds: cfg.queueTimeoutSecs, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}))
	// tts-inst (speech), img-inst (image generation and edit) and sys-inst (System One), replicable alike.
	for _, m := range []struct {
		mapping, spec, model, up, flavor, capability string
	}{
		{liveInstTTSMap, liveInstTTSSpec, "tts-inst", liveInstTTSUp, routing.APIFlavorOpenAIAudio, routing.CapabilitySpeech},
		{liveInstImgMap, liveInstImgSpec, "img-inst", liveInstImgUp, routing.APIFlavorOpenAIImages, routing.CapabilityImage},
		{liveInstSysMap, liveInstSysSpec, "sys-inst", liveInstSysUp, routing.APIFlavorDecisions, routing.CapabilityDecision},
	} {
		must("CreateMapping "+m.model, routes.CreateMapping(ctx, routing.ModelMapping{ID: m.mapping, ApplicationID: "app-live-inst", GatewayModelName: m.model, AppModelName: m.up, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}))
		must("UpsertMappingCapabilities "+m.model, routes.UpsertMappingCapabilities(ctx, m.mapping, []routing.CapabilityRow{
			{Capability: m.capability, Verdict: routing.CapabilityYes, Source: routing.CapabilitySourceManual, CheckedAt: now},
		}))
		must("UpsertRuntimeSpec "+m.model, routes.UpsertRuntimeSpec(ctx, routing.RuntimeSpec{ID: m.spec, MappingID: m.mapping, Enabled: true, Binary: "/usr/bin/" + m.up, Args: "[]", Env: "{}", APIFlavors: []string{m.flavor}, Replicable: cfg.replicable, MaxInstances: 4, CreatedAt: now, UpdatedAt: now}))
	}
	must("CreateMapping asr", routes.CreateMapping(ctx, routing.ModelMapping{ID: liveInstASRMap, ApplicationID: "app-live-inst", GatewayModelName: "asr-inst", AppModelName: liveInstASRUp, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}))
	must("CreateMapping chat", routes.CreateMapping(ctx, routing.ModelMapping{ID: liveInstChatMap, ApplicationID: "app-live-inst", GatewayModelName: "chat-inst", AppModelName: liveInstChatUp, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}))
	must("UpsertMappingCapabilities", routes.UpsertMappingCapabilities(ctx, liveInstASRMap, []routing.CapabilityRow{
		{Capability: routing.CapabilityTranscription, Verdict: routing.CapabilityYes, Source: routing.CapabilitySourceManual, CheckedAt: now},
		{Capability: routing.CapabilityTranscriptionStreaming, Verdict: routing.CapabilityYes, Source: routing.CapabilitySourceManual, CheckedAt: now},
	}))
	must("UpsertRuntimeSpec asr", routes.UpsertRuntimeSpec(ctx, routing.RuntimeSpec{ID: liveInstASRSpec, MappingID: liveInstASRMap, Enabled: true, Binary: "/usr/bin/audio-server", Args: "[]", Env: "{}", APIFlavors: []string{routing.APIFlavorOpenAIAudio}, Replicable: replicable, MaxInstances: 4, InstanceParallelism: cfg.asrParallelism, CreatedAt: now, UpdatedAt: now}))
	if cfg.plainASR {
		must("CreateMapping asr plain", routes.CreateMapping(ctx, routing.ModelMapping{ID: liveInstASRPlainMap, ApplicationID: "app-live-inst", GatewayModelName: "asr-inst", AppModelName: liveInstASRPlainUp, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}))
		must("UpsertMappingCapabilities plain", routes.UpsertMappingCapabilities(ctx, liveInstASRPlainMap, []routing.CapabilityRow{
			{Capability: routing.CapabilityTranscription, Verdict: routing.CapabilityYes, Source: routing.CapabilitySourceManual, CheckedAt: now},
			{Capability: routing.CapabilityTranscriptionStreaming, Verdict: routing.CapabilityYes, Source: routing.CapabilitySourceManual, CheckedAt: now},
		}))
		must("UpsertRuntimeSpec asr plain", routes.UpsertRuntimeSpec(ctx, routing.RuntimeSpec{ID: liveInstASRPlainSpec, MappingID: liveInstASRPlainMap, Enabled: true, Binary: "/usr/bin/audio-server", Args: "[]", Env: "{}", APIFlavors: []string{routing.APIFlavorOpenAIAudio}, CreatedAt: now, UpdatedAt: now}))
	}
	must("UpsertRuntimeSpec chat", routes.UpsertRuntimeSpec(ctx, routing.RuntimeSpec{ID: "spec-chat-inst", MappingID: liveInstChatMap, Enabled: true, Binary: "/usr/bin/llama-server", Args: "[]", Env: "{}", APIFlavors: []string{routing.APIFlavorOpenAI}, ResponsesMode: routing.EndpointModePassthrough, Replicable: replicable, MaxInstances: 4, CreatedAt: now, UpdatedAt: now}))
	must("UpsertTelemetry", routes.UpsertTelemetry(ctx, routing.ServerTelemetry{ServerID: liveInstServer, ReportedAt: now, LatencyMS: 100, ProviderHealth: `{}`, Capabilities: `{}`, RawSummary: `{}`, UpdatedAt: now}))
	recorder := usage.NewRecorder()
	deps := ServerDeps{
		Tokens:   tokens,
		Usage:    recorder,
		Provider: provider.NewOpenAICompatibleClient(http.DefaultClient),
		Routes:   routes,
		Portal:   portal.NewService(portal.ServiceDeps{Users: directory, Tokens: directory, Usage: recorder, Routes: routes, Clock: time.Now, ModelLister: provider.NewMock()}),
	}
	if cfg.voice {
		cipher, err := capture.New(testCaptureKey)
		must("capture.New", err)
		deps.Portal = portal.NewService(portal.ServiceDeps{Users: directory, Tokens: directory, Usage: recorder, Routes: routes, Clock: time.Now, ModelLister: provider.NewMock(), Chats: store.NewMemoryChatStore(0), Cipher: cipher})
		deps.Cipher, deps.Captures = cipher, &fakeCaptureStore{}
		deps.InternalAuthSecret, deps.Users, deps.ChatRuns = "test-internal-secret", directory, NewChatRunRegistry(5)
	}
	srv := New(deps)
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

// A live session that loses the race for the slot it selected treats that slot like
// any other held one. The instance still exists and its slot will free, so it stays a
// candidate -- the registry now reports it held, and every selection passes over it
// until it frees. A session that finds every slot held that way, however it got there,
// parks and signals live demand rather than being refused busy, and it takes the slot
// it lost once that slot frees.
func TestLiveLostRaceParksLikeAHeldSlot(t *testing.T) {
	addressOf := map[string]string{"route-asr-inst#0": "asr-up", "route-asr-inst#1": "asr-up#1"}
	cases := []struct {
		name    string
		running []int    // instances reported running
		held    []string // slots other sessions hold before the session starts
		races   int      // selections whose slot the hook takes before the acquire
	}{
		{"the only running instance", []int{0}, nil, 1},
		{"the other slot is held", []int{0, 1}, []string{"route-asr-inst#1"}, 1},
		{"every slot lost in a race", []int{0, 1}, nil, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			agent := newInstanceAgentRouter(t)
			srv := newInstanceRouterServer(t, agent, true)
			publishASRInstances(srv, tc.running...)
			sink := &recordingDemandSink{}
			srv.Resolver.SetDemandSink(sink)
			srv.Active.admission.recheck = 0 // only a release wakes the parked session
			// Closed last (cleanups run in reverse): every slot is released first and a
			// session still parked when the test fails is woken onto one, instead of
			// holding Close.
			ts := httptest.NewServer(srv)
			t.Cleanup(ts.Close)
			t.Cleanup(func() { wakeAdmission(srv) })
			for _, key := range tc.held {
				release, ok := srv.liveSessions.acquire(key)
				if !ok {
					t.Fatalf("could not take %s", key)
				}
				t.Cleanup(release)
			}
			var mu sync.Mutex
			var raced []string
			var racedReleases []func()
			liveSlotAcquireHook = func(instanceKey string) {
				mu.Lock()
				defer mu.Unlock()
				if len(raced) == tc.races {
					return
				}
				if release, ok := srv.liveSessions.acquire(instanceKey); ok {
					raced = append(raced, instanceKey)
					racedReleases = append(racedReleases, release)
				}
			}
			t.Cleanup(func() {
				liveSlotAcquireHook = nil
				mu.Lock()
				defer mu.Unlock()
				for _, release := range racedReleases {
					release()
				}
			})
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()

			done := make(chan liveSessionResult, 1)
			go func() { done <- startLiveSession(ctx, ts, "asr-inst") }()
			waitForParked(t, srv.Active.admission, 1)
			select {
			case s := <-done:
				status := 0
				if s.resp != nil {
					status = s.resp.StatusCode
				}
				t.Fatalf("session answered (status %d, err %v) after losing its slot while no slot was free, want it to wait", status, s.err)
			default:
			}
			mu.Lock()
			lost := slices.Clone(raced)
			mu.Unlock()
			if len(lost) != tc.races {
				t.Fatalf("the hook took %v, want %d slots", lost, tc.races)
			}
			signals := sink.signals()
			if len(signals) == 0 || !signals[0].Live || signals[0].Model != "asr-inst" || !slices.Equal(signals[0].MappingIDs, []string{liveInstASRMap}) {
				t.Fatalf("demand signals = %+v after the session lost %v, want a live signal for asr-inst naming %s", signals, lost, liveInstASRMap)
			}

			mu.Lock()
			racedReleases[0]()
			mu.Unlock()
			wakeAdmission(srv) // the lost slot's session ends: its row's removal wakes the queue
			var s liveSessionResult
			select {
			case s = <-done:
			case <-time.After(3 * time.Second):
				t.Fatalf("the parked session was not admitted after %s, the slot it lost, freed", lost[0])
			}
			if s.err != nil || s.resp.StatusCode != http.StatusOK {
				t.Fatalf("admitted session: err %v status %v, want 200", s.err, s.resp)
			}
			if served := agent.servedModels(); !slices.Equal(served, []string{addressOf[lost[0]]}) {
				t.Fatalf("live addresses served = %v, want only %s: the instance whose slot the session lost and then took", served, addressOf[lost[0]])
			}
			finishLiveSession(t, s)
		})
	}
}

// Without an admission queue a live session has nowhere to wait: with every slot
// held it is answered busy, and it tries each held slot at most once more instead
// of selecting it over and over.
func TestLiveAllHeldWithoutAQueueAnswersBusy(t *testing.T) {
	agent := newInstanceAgentRouter(t)
	srv := newInstanceRouterServer(t, agent, true)
	publishASRInstances(srv, 0, 1)
	srv.Resolver.SetAdmissionController(nil)
	release0, _ := srv.liveSessions.acquire("route-asr-inst#0")
	release1, _ := srv.liveSessions.acquire("route-asr-inst#1")
	defer release0()
	defer release1()
	selections := 0
	liveSlotAcquireHook = func(string) { selections++ }
	t.Cleanup(func() { liveSlotAcquireHook = nil })

	rec := postBearerLiveRecorder(t, srv, bearerLiveQuery("asr-inst"), devBearerHeader)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body = %s", rec.Code, rec.Body.String())
	}
	requireErrorCode(t, rec.Body.String(), liveBearerBusyCode)
	if selections > 3 {
		t.Fatalf("%d selections, want at most one per held slot and one more", selections)
	}
	if served := agent.servedModels(); len(served) != 0 {
		t.Fatalf("upstream reached for %v, want no relay", served)
	}
}

// Every live slot of a replicable model held: the session waits in the admission
// queue -- signalling demand for a live slot -- and is admitted onto the instance
// whose session ends first. The queue's periodic recheck is off, so it is that
// session's end -- its slot let go, then its active row removed -- that wakes it.
func TestLiveAllHeldReplicableParks(t *testing.T) {
	agent := newInstanceAgentRouter(t)
	srv := newInstanceRouterServer(t, agent, true)
	publishASRInstances(srv, 0, 1)
	srv.Active.admission.recheck = 0
	sink := &recordingDemandSink{}
	srv.Resolver.SetDemandSink(sink)
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	first := establishLiveSession(t, ctx, ts)
	second := establishLiveSession(t, ctx, ts)
	held := agent.servedModels() // [first's instance, second's instance]
	done := make(chan liveSessionResult, 1)
	go func() { done <- startLiveSession(ctx, ts, "asr-inst") }()
	waitForParked(t, srv.Active.admission, 1)
	select {
	case s := <-done:
		t.Fatalf("session answered (status %v, err %v) while every slot was held, want it parked", s.resp, s.err)
	default:
	}
	signals := sink.signals()
	if len(signals) != 1 || !signals[0].Live || !slices.Equal(signals[0].MappingIDs, []string{liveInstASRMap}) || signals[0].Model != "asr-inst" {
		t.Fatalf("demand signals = %+v, want one live signal for asr-inst naming %s", signals, liveInstASRMap)
	}

	finishLiveSession(t, second)
	var s liveSessionResult
	select {
	case s = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the parked session was not woken when a session ended")
	}
	if s.err != nil || s.resp.StatusCode != http.StatusOK {
		t.Fatalf("admitted session: err %v status %v, want 200", s.err, s.resp)
	}
	if served := agent.servedModels(); len(served) != 3 || served[2] != held[1] {
		t.Fatalf("live addresses served = %v, want the third session on %s, the instance whose session ended", served, held[1])
	}
	finishLiveSession(t, s)
	finishLiveSession(t, first)
}

// A live session that keeps losing the race for the slot it selected -- each time to a
// session that took it a moment before, while the slot it lost the time before frees
// -- is answered busy after maxLiveSlotRaces selections instead of selecting forever.
func TestLiveSlotRacesAreBounded(t *testing.T) {
	agent := newInstanceAgentRouter(t)
	srv := newInstanceRouterServer(t, agent, true)
	publishASRInstances(srv, 0, 1)
	var mu sync.Mutex
	races := 0
	var winner func()
	liveSlotAcquireHook = func(instanceKey string) {
		mu.Lock()
		defer mu.Unlock()
		races++
		if winner != nil {
			winner() // the last race's winner ends, so a slot is free at every selection
			winner = nil
		}
		if races <= maxLiveSlotRaces+1 { // a loop without a bound gets its slot after that
			winner, _ = srv.liveSessions.acquire(instanceKey)
		}
	}
	t.Cleanup(func() {
		liveSlotAcquireHook = nil
		mu.Lock()
		defer mu.Unlock()
		if winner != nil {
			winner()
		}
	})

	rec := postBearerLiveRecorder(t, srv, bearerLiveQuery("asr-inst"), devBearerHeader)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body = %s", rec.Code, rec.Body.String())
	}
	requireErrorCode(t, rec.Body.String(), liveBearerBusyCode)
	mu.Lock()
	defer mu.Unlock()
	if races != maxLiveSlotRaces {
		t.Fatalf("%d selections, want maxLiveSlotRaces = %d", races, maxLiveSlotRaces)
	}
	if served := agent.servedModels(); len(served) != 0 {
		t.Fatalf("upstream reached for %v, want no relay", served)
	}
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
// the one usage row is the served attempt's, and while instance 0 serves it the
// gone instance no longer counts it. Every agent refusal that means "this
// instance is gone" qualifies.
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
			inFlightOnGone := -1
			agent.setOnArrive(func(model string) {
				if model == liveInstChatUp {
					inFlightOnGone = srv.Active.InstanceActivity("route-chat-inst#2")
				}
			})

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
			if inFlightOnGone != 0 {
				t.Fatalf("in flight on the vanished instance while instance 0 served = %d, want 0", inFlightOnGone)
			}
		})
	}
}

// A streamed request re-selected off a gone instance may wait in the admission
// queue for the next one. That wait is not the stream going idle: the idle
// watchdog is held while the request is parked and starts afresh for the
// instance it is sent to, so the stream is served there even when the wait
// outlasts the idle timeout.
func TestVanishedStreamReselectWaitsOutsideTheIdleBudget(t *testing.T) {
	agent := newInstanceAgentRouter(t)
	srv := newInstanceRouterServer(t, agent, true)
	const idle = 300 * time.Millisecond
	srv.streamIdleTimeout = idle
	publishChatInstances(srv, 2)
	// Instance 0 at its parallelism of one: the stream goes to instance 2, and its
	// re-selection waits for instance 0.
	srv.Active.Add(ActiveRequest{ID: "busy0", ServerID: liveInstServer, InstanceKey: "route-chat-inst#0"})
	defer srv.Active.Remove("busy0")
	agent.setRefusal("chat-up#2", http.StatusNotFound, "runtime.model_not_managed")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel() // lets go of a request still parked when the test fails
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"chat-inst","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer dev-secret")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.ServeHTTP(rec, req)
	}()

	refused := time.Now().Add(5 * time.Second)
	for !slices.Contains(agent.servedModels(), "chat-up#2") {
		if time.Now().After(refused) {
			t.Fatal("the stream never reached instance 2")
		}
		time.Sleep(10 * time.Millisecond)
	}
	select {
	case <-done:
		t.Fatalf("the stream ended while instance 0 was busy, want its re-selection parked; body %s", rec.Body.String())
	case <-time.After(3 * idle):
	}
	srv.Active.Remove("busy0")
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the parked re-selection was not admitted after instance 0 freed")
	}
	body := rec.Body.String()
	if !strings.Contains(body, "served by chat-up") || strings.Contains(body, errCodeStreamIdleTimeout) {
		t.Fatalf("stream body = %s; want it served by instance 0 with no idle timeout", body)
	}
	if served := agent.servedModels(); !slices.Equal(served, []string{"chat-up#2", "chat-up"}) {
		t.Fatalf("addresses served = %v, want [chat-up#2 chat-up]", served)
	}
	rows := srv.Usage.All()
	if len(rows) != 1 || rows[0].Status != "success" {
		t.Fatalf("usage rows = %+v, want one success row", rows)
	}
}

// The idle watchdog of a re-selected stream starts afresh for the instance it is
// sent to: a wait for that instance shorter than the idle timeout does not eat
// into the instance's first-byte budget, and an instance that then sends nothing
// is still cut at the idle timeout.
func TestVanishedStreamReselectArmsAFreshIdleBudget(t *testing.T) {
	const streamBody = `{"model":"chat-inst","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	start := func(t *testing.T, srv *Server) (*httptest.ResponseRecorder, chan struct{}) {
		t.Helper()
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel) // lets go of a request still running when the test fails
		req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1/chat/completions", strings.NewReader(streamBody))
		req.Header.Set("Authorization", "Bearer dev-secret")
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		done := make(chan struct{})
		go func() {
			defer close(done)
			srv.ServeHTTP(rec, req)
		}()
		return rec, done
	}
	t.Run("a short wait leaves the next instance the whole budget", func(t *testing.T) {
		agent := newInstanceAgentRouter(t)
		srv := newInstanceRouterServer(t, agent, true)
		const idle = time.Second
		srv.streamIdleTimeout = idle
		publishChatInstances(srv, 2)
		srv.Active.Add(ActiveRequest{ID: "busy0", ServerID: liveInstServer, InstanceKey: "route-chat-inst#0"})
		defer srv.Active.Remove("busy0")
		agent.setRefusal("chat-up#2", http.StatusNotFound, "runtime.model_not_managed")
		// Waiting 600 ms for instance 0 and then 700 ms for its first token: 1.3 s in
		// all, past the idle timeout, but each part well inside it.
		agent.setStall("chat-up", 700*time.Millisecond)

		rec, done := start(t, srv)
		refused := time.Now().Add(5 * time.Second)
		for !slices.Contains(agent.servedModels(), "chat-up#2") {
			if time.Now().After(refused) {
				t.Fatal("the stream never reached instance 2")
			}
			time.Sleep(10 * time.Millisecond)
		}
		time.Sleep(600 * time.Millisecond)
		srv.Active.Remove("busy0")
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("the stream did not end")
		}
		if body := rec.Body.String(); !strings.Contains(body, "served by chat-up") || strings.Contains(body, errCodeStreamIdleTimeout) {
			t.Fatalf("stream body = %s; want it served by instance 0 with no idle timeout", body)
		}
	})
	t.Run("the next instance is still watched", func(t *testing.T) {
		agent := newInstanceAgentRouter(t)
		srv := newInstanceRouterServer(t, agent, true)
		const idle = 300 * time.Millisecond
		srv.streamIdleTimeout = idle
		publishChatInstances(srv, 2)
		agent.setRefusal("chat-up#2", http.StatusNotFound, "runtime.model_not_managed")
		agent.setStall("chat-up", time.Minute)

		rec, done := start(t, srv)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("a re-selected stream whose instance sends nothing was not cut at the idle timeout")
		}
		if body := rec.Body.String(); !strings.Contains(body, errCodeStreamIdleTimeout) {
			t.Fatalf("stream body = %s; want the idle timeout", body)
		}
		if served := agent.servedModels(); !slices.Equal(served, []string{"chat-up#2", "chat-up"}) {
			t.Fatalf("addresses served = %v, want [chat-up#2 chat-up]", served)
		}
	})
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
		requireGoneAnswered(t, srv, rec)
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
		requireGoneAnswered(t, srv, rec)
		if served := agent.servedModels(); !slices.Equal(served, []string{"chat-up"}) {
			t.Fatalf("addresses served = %v, want instance 0 once", served)
		}
	})
}

// requireGoneAnswered requires the answer a chat completion gets for an agent
// refusal it is not re-selected off -- the gateway's 502 provider.unavailable, not
// the agent's code -- and the one error row that records it.
func requireGoneAnswered(t *testing.T, srv *Server, rec *httptest.ResponseRecorder) {
	t.Helper()
	if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "provider.unavailable") || strings.Contains(rec.Body.String(), "runtime.") {
		t.Fatalf("status %d body %s; want 502 provider.unavailable without the agent's code", rec.Code, rec.Body.String())
	}
	if rows := srv.Usage.All(); len(rows) != 1 || rows[0].Status != "error" || rows[0].ErrorCode != "provider.unavailable" || rows[0].RouteID != liveInstChatMap {
		t.Fatalf("usage rows = %+v, want one error row provider.unavailable for %s", rows, liveInstChatMap)
	}
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

// wakeAdmission wakes a request parked for the instance server, the way a
// request finishing there does: its active row's removal releases the queue.
func wakeAdmission(srv *Server) {
	srv.Active.Add(ActiveRequest{ID: "waker", ServerID: liveInstServer})
	srv.Active.Remove("waker")
}

// postTranscription sends a buffered transcription of model.
func postTranscription(t *testing.T, srv *Server, model string) *httptest.ResponseRecorder {
	t.Helper()
	body, ct := buildEditMultipart(t, validTranscriptionParts(model))
	return postEditRaw(t, srv, http.MethodPost, "/v1/audio/transcriptions", body, ct, devBearerHeader)
}

// A buffered transcription is never routed onto an instance a live session holds,
// even where the instance's parallelism leaves room beside the session: audio.cpp
// serves one transcription at a time, so it would only be refused busy there. It
// goes to a sibling whose slot is free -- whose own check, keyed by that instance,
// lets it through -- or waits for a slot when every one is held.
func TestBufferedTranscriptionAvoidsALiveHeldInstance(t *testing.T) {
	cfg := instanceRouterConfig{replicable: true, asrParallelism: 2}
	t.Run("a free sibling serves it", func(t *testing.T) {
		agent := newInstanceAgentRouter(t)
		srv := newInstanceRouterServerWith(t, agent, cfg)
		// Instance 0 reports the lighter load, so it is the one chosen when its slot is
		// not considered.
		srv.RuntimeStatus.publish(liveInstServer, []RuntimeStatusDTO{
			liveInstanceRow(liveInstASRSpec, liveInstASRUp, 0, 0),
			liveInstanceRow(liveInstASRSpec, liveInstASRUp, 1, 5),
		})
		release, _ := srv.liveSessions.acquire("route-asr-inst#0")
		defer release()

		rec := postTranscription(t, srv, "asr-inst")
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "hello there") {
			t.Fatalf("status %d body %s; want the transcription from instance 1", rec.Code, rec.Body.String())
		}
		if served := agent.servedModels(); !slices.Equal(served, []string{"asr-up#1"}) {
			t.Fatalf("addresses served = %v, want [asr-up#1], the instance whose live slot is free", served)
		}
	})
	t.Run("it waits for a slot instead of being refused", func(t *testing.T) {
		agent := newInstanceAgentRouter(t)
		srv := newInstanceRouterServerWith(t, agent, cfg)
		srv.Active.admission.recheck = 0 // only a release wakes a parked request
		publishASRInstances(srv, 0)
		release, _ := srv.liveSessions.acquire("route-asr-inst#0")
		defer release()

		done := make(chan *httptest.ResponseRecorder, 1)
		go func() { done <- postTranscription(t, srv, "asr-inst") }()
		waitForParked(t, srv.Active.admission, 1)
		select {
		case rec := <-done:
			t.Fatalf("answered %d %s while the only instance's live slot was held, want it parked", rec.Code, rec.Body.String())
		default:
		}
		release()
		wakeAdmission(srv)
		var rec *httptest.ResponseRecorder
		select {
		case rec = <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("the parked transcription was not admitted after the live slot freed")
		}
		if rec.Code != http.StatusOK || !slices.Equal(agent.servedModels(), []string{"asr-up"}) {
			t.Fatalf("status %d body %s served %v; want instance 0's transcription", rec.Code, rec.Body.String(), agent.servedModels())
		}
	})
}

// publishMixedASR reports asr-inst's instances ks running under load and the
// plain mapping's one process idle, so a selection that does not judge the plain
// mapping's live slot prefers it.
func publishMixedASR(srv *Server, ks ...int) {
	rows := []RuntimeStatusDTO{liveInstanceRow(liveInstASRPlainSpec, liveInstASRPlainUp, 0, 0)}
	for _, k := range ks {
		rows = append(rows, liveInstanceRow(liveInstASRSpec, liveInstASRUp, k, 10))
	}
	srv.RuntimeStatus.publish(liveInstServer, rows)
}

// A model served by a mapping without instances beside a replicable one (a mixed
// fleet) is a model with instances: a live session is not refused busy because the
// plain mapping's one slot is taken while an instance has a free slot. Whether the
// slot was held when the session was selected, or taken in the race between the
// selection and the acquire, the session is selected again onto the instance; with
// every slot held it waits, and takes whichever slot frees first.
func TestLiveMixedFleetSessionIsNotRefusedForAPlainSlot(t *testing.T) {
	cfg := instanceRouterConfig{replicable: true, plainASR: true}
	t.Run("the plain slot is taken in the race", func(t *testing.T) {
		agent := newInstanceAgentRouter(t)
		srv := newInstanceRouterServerWith(t, agent, cfg)
		publishMixedASR(srv, 0)
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
		if raced != "route-asr-plain#0" {
			t.Fatalf("the first selection took %q, want the idle plain mapping route-asr-plain#0", raced)
		}
		if served := agent.servedModels(); !slices.Equal(served, []string{"asr-up"}) {
			t.Fatalf("live addresses served = %v, want [asr-up]: the session lost the plain slot and was selected onto the instance", served)
		}
		finishLiveSession(t, s)
	})
	t.Run("the plain slot is held", func(t *testing.T) {
		agent := newInstanceAgentRouter(t)
		srv := newInstanceRouterServerWith(t, agent, cfg)
		publishMixedASR(srv, 0)
		release, _ := srv.liveSessions.acquire("route-asr-plain#0")
		defer release()
		ts := httptest.NewServer(srv)
		t.Cleanup(ts.Close)
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()

		s := establishLiveSession(t, ctx, ts)
		if served := agent.servedModels(); !slices.Equal(served, []string{"asr-up"}) {
			t.Fatalf("live addresses served = %v, want [asr-up], the instance whose slot is free", served)
		}
		finishLiveSession(t, s)
	})
	t.Run("every slot is held", func(t *testing.T) {
		agent := newInstanceAgentRouter(t)
		srv := newInstanceRouterServerWith(t, agent, cfg)
		srv.Active.admission.recheck = 0 // only a release wakes the parked session
		sink := &recordingDemandSink{}
		srv.Resolver.SetDemandSink(sink)
		publishMixedASR(srv, 0)
		releasePlain, _ := srv.liveSessions.acquire("route-asr-plain#0")
		releaseInst, _ := srv.liveSessions.acquire("route-asr-inst#0")
		defer releaseInst()
		ts := httptest.NewServer(srv)
		t.Cleanup(ts.Close)
		t.Cleanup(releasePlain) // before ts.Close: a session still parked is let go
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()

		done := make(chan liveSessionResult, 1)
		go func() { done <- startLiveSession(ctx, ts, "asr-inst") }()
		waitForParked(t, srv.Active.admission, 1)
		if signals := sink.signals(); len(signals) != 1 || !signals[0].Live || !slices.Equal(signals[0].MappingIDs, []string{liveInstASRMap}) {
			t.Fatalf("demand signals = %+v, want one live signal naming %s, the mapping that can start another instance", signals, liveInstASRMap)
		}
		releasePlain()
		wakeAdmission(srv)
		var s liveSessionResult
		select {
		case s = <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("the parked session was not admitted after the plain slot freed")
		}
		if s.err != nil || s.resp.StatusCode != http.StatusOK {
			t.Fatalf("admitted session: err %v status %v, want 200", s.err, s.resp)
		}
		if served := agent.servedModels(); !slices.Equal(served, []string{"asr-plain"}) {
			t.Fatalf("live addresses served = %v, want [asr-plain], the slot that freed", served)
		}
		finishLiveSession(t, s)
	})
}

// deadlineRecordingAdmission wraps the server's admission queue and records the
// absolute deadline each wait was given (the time of the call plus its timeout).
type deadlineRecordingAdmission struct {
	inner     routing.AdmissionController
	mu        sync.Mutex
	deadlines []time.Time
}

func (a *deadlineRecordingAdmission) WaitForSlot(ctx context.Context, serverIDs []string, timeout time.Duration) error {
	a.mu.Lock()
	a.deadlines = append(a.deadlines, time.Now().Add(timeout))
	a.mu.Unlock()
	return a.inner.WaitForSlot(ctx, serverIDs, timeout)
}

func (a *deadlineRecordingAdmission) recorded() []time.Time {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.deadlines)
}

// A live session that parks, is woken, and loses the freed slot to another session
// parks again under the deadline it parked with first: the races of one session
// share one admission-queue timeout instead of each starting a new one.
func TestLiveSlotRacesKeepOneAdmissionDeadline(t *testing.T) {
	agent := newInstanceAgentRouter(t)
	srv := newInstanceRouterServerWith(t, agent, instanceRouterConfig{replicable: true, queueTimeoutSecs: 30})
	publishASRInstances(srv, 0, 1)
	srv.Active.admission.recheck = 0 // only a release wakes the parked session
	adm := &deadlineRecordingAdmission{inner: srv.Active.admission}
	srv.Resolver.SetAdmissionController(adm)
	release0, _ := srv.liveSessions.acquire("route-asr-inst#0")
	release1, _ := srv.liveSessions.acquire("route-asr-inst#1")
	var mu sync.Mutex
	race := false
	var racedRelease func()
	liveSlotAcquireHook = func(instanceKey string) {
		mu.Lock()
		defer mu.Unlock()
		if race && racedRelease == nil {
			racedRelease, _ = srv.liveSessions.acquire(instanceKey)
		}
	}
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	// Before the server closes: every slot is let go and a session still parked is
	// woken onto one, since a parked bearer session does not notice its client leave.
	t.Cleanup(func() {
		liveSlotAcquireHook = nil
		release0()
		release1()
		mu.Lock()
		if racedRelease != nil {
			racedRelease()
		}
		mu.Unlock()
		wakeAdmission(srv)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	done := make(chan liveSessionResult, 1)
	go func() { done <- startLiveSession(ctx, ts, "asr-inst") }()
	waitForParked(t, srv.Active.admission, 1)
	// Time passes before the slot frees, so a deadline counted afresh from the second
	// park would lie measurably later than the first.
	time.Sleep(200 * time.Millisecond)
	mu.Lock()
	race = true
	mu.Unlock()
	release0()
	wakeAdmission(srv)
	waitForParked(t, srv.Active.admission, 1)

	mu.Lock()
	raced := racedRelease != nil
	mu.Unlock()
	deadlines := adm.recorded()
	if !raced || len(deadlines) != 2 {
		t.Fatalf("raced %v, %d waits; want the session woken, its slot taken in the race, and parked again", raced, len(deadlines))
	}
	if drift := deadlines[1].Sub(deadlines[0]); drift > 50*time.Millisecond || drift < -50*time.Millisecond {
		t.Fatalf("the second park's deadline lies %v from the first's, want the same deadline", drift)
	}
	release1()
	wakeAdmission(srv)
	s := <-done
	if s.err != nil || s.resp.StatusCode != http.StatusOK {
		t.Fatalf("the session admitted once instance 1 freed: err %v status %v, want 200", s.err, s.resp)
	}
	finishLiveSession(t, s)
}

// A request the admission queue has no room for is refused as full and starts
// no instance: only the request that parked signalled demand.
func TestFullAdmissionQueueSignalsNoDemand(t *testing.T) {
	agent := newInstanceAgentRouter(t)
	srv := newInstanceRouterServer(t, agent, true)
	srv.Active.admission.recheck = 0
	srv.Active.admission.maxDepth = 1
	sink := &recordingDemandSink{}
	srv.Resolver.SetDemandSink(sink)
	publishChatInstances(srv)
	srv.Active.Add(ActiveRequest{ID: "busy0", ServerID: liveInstServer, InstanceKey: "route-chat-inst#0"})
	defer srv.Active.Remove("busy0")
	const chatBody = `{"model":"chat-inst","messages":[{"role":"user","content":"hi"}]}`

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	parked := make(chan struct{})
	go func() {
		defer close(parked)
		req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1/chat/completions", strings.NewReader(chatBody))
		req.Header.Set("Authorization", "Bearer dev-secret")
		req.Header.Set("Content-Type", "application/json")
		srv.ServeHTTP(httptest.NewRecorder(), req)
	}()
	waitForParked(t, srv.Active.admission, 1)

	rec := postInstanceJSON(t, srv, "/v1/chat/completions", chatBody)
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), routing.ErrAdmissionQueueFull.Error()) {
		t.Fatalf("status %d body %s; want 503 %s", rec.Code, rec.Body.String(), routing.ErrAdmissionQueueFull)
	}
	if signals := sink.signals(); len(signals) != 1 {
		t.Fatalf("demand signals = %+v, want only the parked request's one", signals)
	}
	cancel()
	<-parked
}
