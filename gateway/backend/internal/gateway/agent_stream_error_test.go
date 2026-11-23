// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/inference"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/provider"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/store"
	"op-ai-gateway/internal/usage"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	agentKeepalives        = ": keepalive\n\n: keepalive\n\n"
	agentStartFailedFrame  = `data: {"error":{"code":"runtime.start_failed","message":"engine exited during startup"}}` + "\n\n"
	agentAdmissionFrame    = `data: {"error":{"code":"runtime.admission_blocked","message":"no free VRAM"}}` + "\n\n"
	modelServerErrorFrame  = `data: {"error":{"code":"context_length_exceeded","message":"too long"}}` + "\n\n"
	agentResponsesCreated  = "event: response.created\n" + `data: {"type":"response.created","sequence_number":0,"response":{"id":"resp_up1","object":"response","status":"in_progress"}}` + "\n\n"
	agentResponsesProgress = "event: response.in_progress\n" + `data: {"type":"response.in_progress","sequence_number":1,"response":{"id":"resp_up1","object":"response","status":"in_progress"}}` + "\n\n"
)

// sseEvents splits an SSE body into its events' (event name, data JSON) pairs,
// skipping comments.
func sseEvents(t *testing.T, body string) []struct {
	name string
	data map[string]any
} {
	t.Helper()
	var out []struct {
		name string
		data map[string]any
	}
	for _, block := range strings.Split(body, "\n\n") {
		var name, data string
		for _, line := range strings.Split(block, "\n") {
			if v, ok := strings.CutPrefix(line, "event:"); ok {
				name = strings.TrimSpace(v)
			}
			if v, ok := strings.CutPrefix(line, "data:"); ok {
				data = strings.TrimSpace(v)
			}
		}
		if data == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(data), &m); err != nil {
			t.Fatalf("event data %q: %v", data, err)
		}
		out = append(out, struct {
			name string
			data map[string]any
		}{name, m})
	}
	return out
}

func agentTarget() routing.Target { return routing.Target{Provider: routing.ProviderServerAgent} }

// runFilter pushes body through the filter in chunks of size n and flushes it.
func runFilter(f *agentStreamErrorFilter, body string, n int) string {
	var out []byte
	for i := 0; i < len(body); i += n {
		out = append(out, f.push([]byte(body[i:min(i+n, len(body))]))...)
	}
	return string(append(out, f.flush()...))
}

func TestAgentStreamErrorFilterOnlyForStreamedAgentAnthropicAndResponses(t *testing.T) {
	cases := []struct {
		name   string
		target routing.Target
		flavor string
		stream bool
		want   bool
	}{
		{"agent messages stream", agentTarget(), inference.APIFlavorAnthropicMessages, true, true},
		{"agent responses stream", agentTarget(), inference.APIFlavorOpenAIResponses, true, true},
		{"agent chat completions", agentTarget(), "openai_chat_completions", true, false},
		{"agent buffered", agentTarget(), inference.APIFlavorAnthropicMessages, false, false},
		{"strata application", routing.Target{Provider: routing.ProviderStrata}, inference.APIFlavorAnthropicMessages, true, false},
		{"llama.cpp application", routing.Target{Provider: routing.ProviderLlamaCPP}, inference.APIFlavorOpenAIResponses, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := newAgentStreamErrorFilter(tc.target, tc.flavor, tc.stream, "req_1") != nil; got != tc.want {
				t.Fatalf("filter built = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestAgentStreamErrorFilterAnthropicTranslation(t *testing.T) {
	cases := []struct {
		frame, wantType, wantMessage, wantCode string
	}{
		{agentStartFailedFrame, "api_error", "runtime.start_failed: engine exited during startup", "runtime.start_failed"},
		{agentAdmissionFrame, "overloaded_error", "runtime.admission_blocked: no free VRAM", "runtime.admission_blocked"},
		{`data: {"error":{"code":"runtime.cluster_member_unreachable","message":"member down"}}` + "\n\n", "overloaded_error", "runtime.cluster_member_unreachable: member down", "runtime.cluster_member_unreachable"},
	}
	for _, tc := range cases {
		for _, n := range []int{1, 7, 1 << 20} {
			t.Run(tc.wantCode+"/chunk"+strconv.Itoa(n), func(t *testing.T) {
				f := newAgentStreamErrorFilter(agentTarget(), inference.APIFlavorAnthropicMessages, true, "req_1")
				got := runFilter(f, agentKeepalives+tc.frame, n)
				if !strings.HasPrefix(got, agentKeepalives) {
					t.Fatalf("keepalives not relayed first: %q", got)
				}
				evs := sseEvents(t, got)
				if len(evs) != 1 || evs[0].name != "error" || evs[0].data["type"] != "error" {
					t.Fatalf("events = %+v, want one Anthropic error event", evs)
				}
				e, _ := evs[0].data["error"].(map[string]any)
				if e["type"] != tc.wantType || e["message"] != tc.wantMessage {
					t.Fatalf("error = %v, want type %s message %q", e, tc.wantType, tc.wantMessage)
				}
				if f.failure == nil || f.failure.code != tc.wantCode {
					t.Fatalf("failure = %+v, want code %s", f.failure, tc.wantCode)
				}
			})
		}
	}
}

// Without a response.created the response.failed still follows the error event,
// under the gateway's own response id: Codex reads only response.failed.
func TestAgentStreamErrorFilterResponsesWithoutCreated(t *testing.T) {
	f := newAgentStreamErrorFilter(agentTarget(), inference.APIFlavorOpenAIResponses, true, "req_1")
	got := runFilter(f, agentKeepalives+agentStartFailedFrame, 5)
	evs := sseEvents(t, got)
	if len(evs) != 2 || evs[0].name != "error" || evs[1].name != "response.failed" {
		t.Fatalf("events = %+v, want error then response.failed", evs)
	}
	d := evs[0].data
	if d["type"] != "error" || d["code"] != "runtime.start_failed" || d["message"] != "engine exited during startup" || d["sequence_number"] != float64(0) {
		t.Fatalf("error event = %v", d)
	}
	if p, has := d["param"]; !has || p != nil {
		t.Fatalf("error event param = %v (present %v), want null", p, has)
	}
	resp, _ := evs[1].data["response"].(map[string]any)
	e, _ := resp["error"].(map[string]any)
	if resp["id"] != "resp_req_1" || resp["status"] != "failed" || e["code"] != "runtime.start_failed" || e["message"] != "engine exited during startup" || evs[1].data["sequence_number"] != float64(1) {
		t.Fatalf("response.failed = %v", evs[1].data)
	}
}

func TestAgentStreamErrorFilterResponsesAfterCreatedAlsoFailsTheResponse(t *testing.T) {
	f := newAgentStreamErrorFilter(agentTarget(), inference.APIFlavorOpenAIResponses, true, "req_1")
	prefix := agentResponsesCreated + agentResponsesProgress + agentKeepalives
	got := runFilter(f, prefix+agentStartFailedFrame, 3)
	if !strings.HasPrefix(got, prefix) {
		t.Fatalf("frames before the failure changed: %q", got)
	}
	evs := sseEvents(t, got)
	if len(evs) != 4 || evs[2].name != "error" || evs[3].name != "response.failed" {
		t.Fatalf("events = %+v, want created, in_progress, error, response.failed", evs)
	}
	if evs[2].data["sequence_number"] != float64(2) || evs[3].data["sequence_number"] != float64(3) {
		t.Fatalf("sequence numbers = %v, %v, want 2, 3", evs[2].data["sequence_number"], evs[3].data["sequence_number"])
	}
	resp, _ := evs[3].data["response"].(map[string]any)
	e, _ := resp["error"].(map[string]any)
	if resp["id"] != "resp_up1" || resp["status"] != "failed" || resp["object"] != "response" || e["code"] != "runtime.start_failed" || e["message"] != "engine exited during startup" {
		t.Fatalf("response.failed = %v", evs[3].data)
	}
}

// Everything that is not the agent's exact frame passes byte for byte: a model
// server's own error frame without the runtime. prefix, the agent's shape with an
// event: line or extra fields, ordinary events and comments.
func TestAgentStreamErrorFilterLeavesOtherFramesUnchanged(t *testing.T) {
	bodies := []string{
		agentKeepalives + modelServerErrorFrame,
		"event: error\n" + agentStartFailedFrame,
		`data: {"error":{"code":"runtime.start_failed","message":"x","type":"server_error"}}` + "\n\n",
		`data: {"error":{"code":"runtime.start_failed","message":"x"},"id":"1"}` + "\n\n",
		`data: {"error":{"code":"runtime.start_failed","message":7}}` + "\n\n",
		agentResponsesCreated + agentResponsesProgress + "data: [DONE]\n\n",
		"event: message_start\r\ndata: {\"type\":\"message_start\"}\r\n\r\n",
		`data: {"error":` + strings.Repeat("x", agentErrorFrameMaxBytes+10) + "\n\n",
		`data: {"error":{"code":"runtime.start_failed","message":"cut"`, // never terminated
	}
	for i, body := range bodies {
		for _, flavor := range []string{inference.APIFlavorAnthropicMessages, inference.APIFlavorOpenAIResponses} {
			f := newAgentStreamErrorFilter(agentTarget(), flavor, true, "req_1")
			if got := runFilter(f, body, 4); got != body {
				t.Fatalf("body %d (%s) changed:\n got %q\nwant %q", i, flavor, got, body)
			}
			if f.failure != nil {
				t.Fatalf("body %d (%s) recorded failure %+v", i, flavor, f.failure)
			}
		}
	}
}

// Comments and ordinary events reach the client as soon as they arrive; only an
// event that opens like the agent's frame is held until it ends.
func TestAgentStreamErrorFilterForwardsNonCandidatesImmediately(t *testing.T) {
	f := newAgentStreamErrorFilter(agentTarget(), inference.APIFlavorAnthropicMessages, true, "req_1")
	if got := string(f.push([]byte(": keep"))); got != ": keep" {
		t.Fatalf("comment fragment = %q, want it forwarded at once", got)
	}
	if got := string(f.push([]byte("alive\n\ndata: {\"err"))); got != "alive\n\n" {
		t.Fatalf("got %q, want the comment's rest and the candidate held", got)
	}
	if got := string(f.push([]byte("or\""))); got != "" {
		t.Fatalf("got %q, want still held", got)
	}
	if got := string(f.push([]byte("x"))); got != "data: {\"error\"x" {
		t.Fatalf("got %q, want the held bytes released once the prefix diverges", got)
	}
}

// newAgentStreamTestServer seeds one server_agent application at upstreamURL whose
// one mapping, "agent-model", carries a strata runtime spec serving the Anthropic
// and OpenAI flavors in passthrough mode, served by the real OpenAI-compatible
// client: a streamed request reaches upstreamURL as it would reach the agent's
// runtime router.
func newAgentStreamTestServer(t *testing.T, upstreamURL string) *Server {
	t.Helper()
	tokens := auth.NewTokenStore()
	directory := portal.NewMemoryDirectory(tokens)
	now := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	directory.AddUser(store.User{ID: "usr_dev", Email: "dev@example.test", DisplayName: "Dev User", Role: "admin", Status: store.UserStatusActive, PreferredLanguage: "de", CreatedAt: now, UpdatedAt: now})
	if err := directory.CreatePlainToken(context.Background(), store.TokenRecord{ID: "tok_dev", UserID: "usr_dev", Name: "Dev Token", Status: store.TokenStatusActive, Scopes: `["gateway:use"]`, CreatedAt: now, UpdatedAt: now}, "dev-secret"); err != nil {
		t.Fatalf("CreatePlainToken: %v", err)
	}
	up, err := url.Parse(upstreamURL)
	if err != nil {
		t.Fatalf("url.Parse(%q): %v", upstreamURL, err)
	}
	port, err := strconv.Atoi(up.Port())
	if err != nil {
		t.Fatalf("upstream port %q: %v", up.Port(), err)
	}
	recorder := usage.NewRecorder()
	routeStore := routing.NewMemoryStore()
	ctx := context.Background()
	if err := routeStore.CreateAIServer(ctx, routing.AIServer{ID: "srv-agent", Name: "Agent", Domain: up.Hostname(), Provider: routing.ProviderVLLM, Endpoint: upstreamURL, Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateAIServer: %v", err)
	}
	if err := routeStore.CreateApplication(ctx, routing.Application{ID: "app-agent", ServerID: "srv-agent", Type: routing.ProviderServerAgent, Port: port, Scheme: up.Scheme, APIFlavors: []string{routing.APIFlavorOpenAI, routing.APIFlavorAnthropic}, Priority: 10, Weight: 50, TimeoutMS: 30000, Status: routing.ServerStatusActive, ResponsesMode: routing.EndpointModePassthrough, MessagesMode: routing.EndpointModePassthrough, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateApplication: %v", err)
	}
	if err := routeStore.CreateMapping(ctx, routing.ModelMapping{ID: "route-agent", ApplicationID: "app-agent", GatewayModelName: "agent-model", AppModelName: "agent-model", Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateMapping: %v", err)
	}
	if err := routeStore.UpsertRuntimeSpec(ctx, routing.RuntimeSpec{ID: "spec-agent", MappingID: "route-agent", Type: routing.ProviderStrata, APIFlavors: []string{routing.APIFlavorOpenAI, routing.APIFlavorAnthropic}, ResponsesMode: routing.EndpointModePassthrough, MessagesMode: routing.EndpointModePassthrough, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("UpsertRuntimeSpec: %v", err)
	}
	if err := routeStore.UpsertTelemetry(ctx, routing.ServerTelemetry{ServerID: "srv-agent", ReportedAt: now, LatencyMS: 100, ProviderHealth: `{}`, Capabilities: `{}`, RawSummary: `{}`, UpdatedAt: now}); err != nil {
		t.Fatalf("UpsertTelemetry: %v", err)
	}
	return New(ServerDeps{
		Tokens:   tokens,
		Usage:    recorder,
		Provider: provider.NewOpenAICompatibleClient(http.DefaultClient),
		Routes:   routeStore,
		Portal:   portal.NewService(portal.ServiceDeps{Users: directory, Tokens: directory, Usage: recorder, Routes: routeStore, Clock: func() time.Time { return now }, ModelLister: provider.NewMock()}),
	})
}

// sseUpstream answers every request with 200 text/event-stream and body.
func sseUpstream(t *testing.T, body string) *httptest.Server {
	t.Helper()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(up.Close)
	return up
}

func postAgentStream(t *testing.T, srv *Server, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer dev-secret")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

const (
	agentMessagesBody  = `{"model":"agent-model","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"hi"}]}`
	agentResponsesBody = `{"model":"agent-model","stream":true,"input":"hi"}`
)

// The live failure: an agent that committed 200 and heartbeats, then failed the
// start, ends the stream with its terminal frame. The client gets its flavor's
// error event after the keepalives, and the row is a failure carrying the agent's
// code, with the wire status 200 and no tokens.
func TestAgentStreamStartFailureBecomesTheClientsErrorEventAndAFailedRow(t *testing.T) {
	cases := []struct {
		name, path, body, wantEvent string
	}{
		{"messages", "/v1/messages", agentMessagesBody, "error"},
		{"responses", "/v1/responses", agentResponsesBody, "response.failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newAgentStreamTestServer(t, sseUpstream(t, agentKeepalives+agentStartFailedFrame).URL)
			rec := postAgentStream(t, srv, tc.path, tc.body)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}
			got := rec.Body.String()
			if !strings.HasPrefix(got, agentKeepalives) {
				t.Fatalf("keepalives not relayed: %q", got)
			}
			if strings.Contains(got, agentStartFailedFrame) {
				t.Fatalf("the agent's raw frame reached the client: %q", got)
			}
			evs := sseEvents(t, got)
			if len(evs) == 0 || evs[0].name != "error" || evs[len(evs)-1].name != tc.wantEvent {
				t.Fatalf("events = %+v, want an error event ending with %s", evs, tc.wantEvent)
			}
			ev := lastUsageEvent(t, srv)
			if ev.Status != "error" || ev.ErrorCode != "runtime.start_failed" || ev.HTTPStatus != http.StatusOK || ev.TotalTokens != 0 {
				t.Fatalf("row status/code/http/total = %s/%s/%d/%d, want error/runtime.start_failed/200/0", ev.Status, ev.ErrorCode, ev.HTTPStatus, ev.TotalTokens)
			}
		})
	}
}

// A model server's own error frame (no runtime. code) is relayed verbatim and the
// row keeps today's classification.
func TestAgentStreamModelServerErrorFrameIsRelayedUnchanged(t *testing.T) {
	srv := newAgentStreamTestServer(t, sseUpstream(t, agentKeepalives+modelServerErrorFrame).URL)
	rec := postAgentStream(t, srv, "/v1/messages", agentMessagesBody)
	if got := rec.Body.String(); got != agentKeepalives+modelServerErrorFrame {
		t.Fatalf("body = %q, want it relayed verbatim", got)
	}
	if ev := lastUsageEvent(t, srv); ev.Status != "success" || ev.ErrorCode != "" {
		t.Fatalf("row status/code = %s/%s, want success and none", ev.Status, ev.ErrorCode)
	}
}

// The same frame from a target that is not agent-managed passes untouched.
func TestNonAgentTargetRelaysAgentShapedFrameUnchanged(t *testing.T) {
	srv := newStrataTestServer(t, sseUpstream(t, agentKeepalives+agentStartFailedFrame).URL)
	body := strings.Replace(agentMessagesBody, "agent-model", "strata-model", 1)
	rec := postAgentStream(t, srv, "/v1/messages", body)
	if got := rec.Body.String(); got != agentKeepalives+agentStartFailedFrame {
		t.Fatalf("body = %q, want it relayed verbatim", got)
	}
	if ev := lastUsageEvent(t, srv); ev.Status != "success" {
		t.Fatalf("row status = %s, want success (unchanged)", ev.Status)
	}
}
