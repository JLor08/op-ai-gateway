// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"log/slog"
	"net"
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
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// The /v1/realtime tests drive the gateway through a real httptest server, the way a
// client reaches it, against a fake agent router that speaks the agent's GET
// /realtime/{model} route. Nothing sleeps: every wait is on a channel, a read or the
// handler's return, and rtWait only bounds a wait that never resolves.
const rtWait = 5 * time.Second

// The fixture's names: the gateway model a client asks for, the model the agent's
// router keys the child on, and the dev bearer the fixture seeds.
const (
	rtModel       = "omni-rt"
	rtUpstream    = "qwen3-omni"
	rtSecret      = "dev-secret"
	rtServerID    = "srv-rt"
	rtMappingID   = "route-rt"
	rtFeatureName = "realtime_proxy" // a literal: what an agent puts on the wire
)

// rtAgentSeen is what the fake agent observed about one handshake.
type rtAgentSeen struct {
	path        string
	escapedPath string
	query       string
	header      http.Header
}

// rtAgentEnd is how a session ended at the fake agent: the close status and reason it
// read, or self when the agent ended the session itself.
type rtAgentEnd struct {
	code   websocket.StatusCode
	reason string
	self   bool
}

// rtAgent is a fake agent router. GET /realtime/{model} accepts a WebSocket with the
// real route's default origin check (an Origin naming another host is refused), echoes
// every message with its type (but one that starts with "noecho:"), and obeys text
// commands: "agent:send <text>" sends text, "agent:flood" sends text messages back to
// back until a write fails,
// "agent:close <code>" closes with that code and "agent says bye", "agent:emptyclose"
// closes without a status (1005), "agent:drop" drops the connection without a close
// frame, and "agent:big" sends one binary message a byte over the 4 MiB limit. With
// refuse set it answers every handshake with that status and the router's
// runtime.upstream_gone envelope instead, as the route does when the child dial fails.
type rtAgent struct {
	srv      *httptest.Server
	dials    atomic.Int64
	seen     chan rtAgentSeen
	ended    chan rtAgentEnd
	oversize chan int
	refuse   int
	// hold, when set, keeps every handshake waiting until it is closed or the
	// gateway gives up on the dial -- an agent still starting the model.
	hold chan struct{}
}

func newRTAgent(t *testing.T) *rtAgent {
	t.Helper()
	a := &rtAgent{seen: make(chan rtAgentSeen, 8), ended: make(chan rtAgentEnd, 8), oversize: make(chan int, 8)}
	a.srv = httptest.NewServer(http.HandlerFunc(a.serve))
	t.Cleanup(a.srv.Close)
	return a
}

func (a *rtAgent) serve(w http.ResponseWriter, r *http.Request) {
	if !strings.HasPrefix(r.URL.Path, "/realtime/") {
		http.NotFound(w, r)
		return
	}
	a.dials.Add(1)
	a.seen <- rtAgentSeen{path: r.URL.Path, escapedPath: r.URL.EscapedPath(), query: r.URL.RawQuery, header: r.Header.Clone()}
	if a.hold != nil {
		select {
		case <-a.hold:
		case <-r.Context().Done():
			a.ended <- rtAgentEnd{self: true}
			return
		}
	}
	if a.refuse != 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(a.refuse)
		_, _ = io.WriteString(w, `{"error":{"code":"runtime.upstream_gone","message":"dial the child's realtime endpoint: connection refused"}}`)
		return
	}
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	conn.SetReadLimit(-1)
	ctx := context.Background()
	for {
		typ, data, err := conn.Read(ctx)
		if err != nil {
			var ce websocket.CloseError
			errors.As(err, &ce)
			a.ended <- rtAgentEnd{code: websocket.CloseStatus(err), reason: ce.Reason}
			return
		}
		if typ == websocket.MessageText && bytes.HasPrefix(data, []byte("agent:")) {
			if a.command(ctx, conn, string(data[len("agent:"):])) {
				return
			}
			continue
		}
		if bytes.HasPrefix(data, []byte("noecho:")) {
			continue
		}
		if len(data) > rtMessageLimit {
			a.oversize <- len(data)
			continue
		}
		_ = conn.Write(ctx, typ, data)
	}
}

// command runs one agent command and reports whether it ended the session.
func (a *rtAgent) command(ctx context.Context, conn *websocket.Conn, cmd string) bool {
	switch {
	case strings.HasPrefix(cmd, "send "):
		_ = conn.Write(ctx, websocket.MessageText, []byte(strings.TrimPrefix(cmd, "send ")))
	case strings.HasPrefix(cmd, "close "):
		code, _ := strconv.Atoi(strings.TrimPrefix(cmd, "close "))
		_ = conn.Close(websocket.StatusCode(code), "agent says bye")
		a.ended <- rtAgentEnd{code: websocket.StatusCode(code), self: true}
		return true
	case cmd == "emptyclose":
		_ = conn.Close(websocket.StatusNoStatusRcvd, "")
		a.ended <- rtAgentEnd{code: websocket.StatusNoStatusRcvd, self: true}
		return true
	case cmd == "drop":
		_ = conn.CloseNow()
		a.ended <- rtAgentEnd{self: true}
		return true
	case cmd == "big":
		_ = conn.Write(ctx, websocket.MessageBinary, make([]byte, rtMessageLimit+1))
	case cmd == "flood":
		go func() {
			for {
				if err := conn.Write(ctx, websocket.MessageText, []byte(`{"type":"response.audio.delta","delta":"AAAA"}`)); err != nil {
					return
				}
			}
		}()
	}
	return false
}

// rtMessageLimit is the gateway's per-message limit, restated: the test must fail if
// the constant moves.
const rtMessageLimit = 4 << 20

// rtRig is a gateway serving /v1/realtime in front of one or more fake agents.
// handled receives one value each time a /v1/realtime handler has returned, which is
// when its usage row is recorded and its active request removed.
type rtRig struct {
	srv     *Server
	ts      *httptest.Server
	agent   *rtAgent
	handled chan struct{}
}

// rtCandidate is one agent-launched mapping of the fixture's model.
type rtCandidate struct {
	serverID, appID, mappingID string
	agent                      *rtAgent
	priority                   int
	features                   []string
}

// newRTRig builds the gateway with one served candidate: an agent that declared
// realtime_proxy, its mapping of rtModel with a manual realtime:yes verdict and a
// runtime spec serving openai_audio.
func newRTRig(t *testing.T) *rtRig {
	t.Helper()
	agent := newRTAgent(t)
	return newRTRigWith(t, rtCandidate{serverID: rtServerID, appID: "app-rt", mappingID: rtMappingID, agent: agent, priority: 10, features: []string{"runtime_manager", rtFeatureName}})
}

func newRTRigWith(t *testing.T, candidates ...rtCandidate) *rtRig {
	t.Helper()
	tokens := auth.NewTokenStore()
	directory := portal.NewMemoryDirectory(tokens)
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	directory.AddUser(store.User{ID: "usr_dev", Email: "dev@example.test", DisplayName: "Dev User", Role: "admin", Status: store.UserStatusActive, PreferredLanguage: "de", CreatedAt: now, UpdatedAt: now})
	if err := directory.CreatePlainToken(context.Background(), store.TokenRecord{ID: "tok_dev", UserID: "usr_dev", Name: "Dev Token", Status: store.TokenStatusActive, Scopes: `["gateway:use"]`, CreatedAt: now, UpdatedAt: now}, rtSecret); err != nil {
		t.Fatalf("CreatePlainToken: %v", err)
	}
	if err := directory.CreatePlainToken(context.Background(), store.TokenRecord{ID: "tok_admin_only", UserID: "usr_dev", Name: "Admin Only", Status: store.TokenStatusActive, Scopes: `["admin"]`, CreatedAt: now, UpdatedAt: now}, "admin-only-secret"); err != nil {
		t.Fatalf("CreatePlainToken: %v", err)
	}
	recorder := usage.NewRecorder()
	routes := routing.NewMemoryStore()
	ctx := context.Background()
	for _, c := range candidates {
		u, err := url.Parse(c.agent.srv.URL)
		if err != nil {
			t.Fatalf("agent URL: %v", err)
		}
		host, portText, _ := net.SplitHostPort(u.Host)
		port, _ := strconv.Atoi(portText)
		if err := routes.CreateAIServer(ctx, routing.AIServer{ID: c.serverID, Name: "Agent " + c.serverID, Domain: host, Provider: routing.ProviderVLLM, Endpoint: c.agent.srv.URL, Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("CreateAIServer: %v", err)
		}
		if err := routes.CreateApplication(ctx, routing.Application{ID: c.appID, ServerID: c.serverID, Type: routing.ProviderServerAgent, Port: port, Scheme: "http", APIFlavors: []string{routing.APIFlavorOpenAIAudio}, Priority: c.priority, Weight: 50, TimeoutMS: 600000, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("CreateApplication: %v", err)
		}
		if err := routes.CreateMapping(ctx, routing.ModelMapping{ID: c.mappingID, ApplicationID: c.appID, GatewayModelName: rtModel, AppModelName: rtUpstream, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("CreateMapping: %v", err)
		}
		if err := routes.UpsertMappingCapabilities(ctx, c.mappingID, []routing.CapabilityRow{{Capability: routing.CapabilityRealtime, Verdict: routing.CapabilityYes, Source: routing.CapabilitySourceManual, CheckedAt: now}}); err != nil {
			t.Fatalf("UpsertMappingCapabilities: %v", err)
		}
		if err := routes.UpsertRuntimeSpec(ctx, routing.RuntimeSpec{ID: "spec-" + c.mappingID, MappingID: c.mappingID, Type: string(routing.RuntimeSpecTypeSGLangOmni), APIFlavors: []string{routing.APIFlavorOpenAIAudio}, ResponsesMode: routing.EndpointModeDisabled, MessagesMode: routing.EndpointModeDisabled, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("UpsertRuntimeSpec: %v", err)
		}
		if err := routes.UpsertTelemetry(ctx, routing.ServerTelemetry{ServerID: c.serverID, ReportedAt: time.Now().UTC(), LatencyMS: 10, ProviderHealth: `{}`, Capabilities: `{}`, RawSummary: `{}`, UpdatedAt: now}); err != nil {
			t.Fatalf("UpsertTelemetry: %v", err)
		}
	}
	agentClient := provider.NewOpenAICompatibleClient(nil)
	srv := New(ServerDeps{
		Tokens:   tokens,
		Usage:    recorder,
		Provider: provider.NewMultiplexer(map[string]provider.Client{routing.ProviderServerAgent: agentClient}, agentClient),
		Routes:   routes,
		Portal:   portal.NewService(portal.ServiceDeps{Users: directory, Tokens: directory, Usage: recorder, Routes: routes, Clock: func() time.Time { return now }, ModelLister: provider.NewMock()}),
	})
	for _, c := range candidates {
		srv.AgentFeatures.Set(c.serverID, c.features)
	}
	rig := &rtRig{srv: srv, agent: candidates[0].agent, handled: make(chan struct{}, 16)}
	rig.ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		srv.ServeHTTP(w, r)
		if strings.HasSuffix(r.URL.Path, "/realtime") {
			rig.handled <- struct{}{}
		}
	}))
	t.Cleanup(rig.ts.Close)
	return rig
}

// dial opens a /v1/realtime session with the dev bearer.
func (rig *rtRig) dial(t *testing.T, query string) *websocket.Conn {
	t.Helper()
	conn, resp, err := rig.dialWith(t, "/v1/realtime", query, &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + rtSecret}}})
	if err != nil {
		t.Fatalf("dial /v1/realtime: %v (status %d)", err, statusOf(resp))
	}
	return conn
}

func (rig *rtRig) dialWith(t *testing.T, path, query string, opts *websocket.DialOptions) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), rtWait)
	defer cancel()
	conn, resp, err := websocket.Dial(ctx, rig.ts.URL+path+"?"+query, opts)
	if conn != nil {
		conn.SetReadLimit(-1)
		t.Cleanup(func() { _ = conn.CloseNow() })
	}
	return conn, resp, err
}

func statusOf(resp *http.Response) int {
	if resp == nil {
		return 0
	}
	return resp.StatusCode
}

// waitHandled waits until a /v1/realtime handler has returned.
func (rig *rtRig) waitHandled(t *testing.T) {
	t.Helper()
	select {
	case <-rig.handled:
	case <-time.After(rtWait):
		t.Fatal("the /v1/realtime handler did not return")
	}
}

// oneRow waits for the handler to return and requires exactly one usage row.
func (rig *rtRig) oneRow(t *testing.T) usage.Event {
	t.Helper()
	rig.waitHandled(t)
	rows := rig.srv.Usage.All()
	if len(rows) != 1 {
		t.Fatalf("usage rows = %d (%+v), want exactly one", len(rows), rows)
	}
	if n := len(rig.srv.Active.Snapshot()); n != 0 {
		t.Fatalf("active requests after the session = %d, want 0", n)
	}
	return rows[0]
}

func recvSeen(t *testing.T, a *rtAgent) rtAgentSeen {
	t.Helper()
	select {
	case s := <-a.seen:
		return s
	case <-time.After(rtWait):
		t.Fatal("the agent saw no handshake")
		return rtAgentSeen{}
	}
}

func recvEnd(t *testing.T, a *rtAgent) rtAgentEnd {
	t.Helper()
	select {
	case e := <-a.ended:
		return e
	case <-time.After(rtWait):
		t.Fatal("the agent's session did not end")
		return rtAgentEnd{}
	}
}

func rtWrite(t *testing.T, c *websocket.Conn, typ websocket.MessageType, data []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), rtWait)
	defer cancel()
	if err := c.Write(ctx, typ, data); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func rtRead(t *testing.T, c *websocket.Conn) (websocket.MessageType, []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), rtWait)
	defer cancel()
	typ, data, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return typ, data
}

// rtReadClose reads until the connection ends and returns the close status and reason
// it ended with (-1 for an end without a close frame).
func rtReadClose(t *testing.T, c *websocket.Conn) (websocket.StatusCode, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), rtWait)
	defer cancel()
	for {
		_, _, err := c.Read(ctx)
		if err == nil {
			continue
		}
		if errors.Is(err, context.DeadlineExceeded) {
			t.Fatal("the connection did not end")
		}
		var ce websocket.CloseError
		errors.As(err, &ce)
		return websocket.CloseStatus(err), ce.Reason
	}
}

// roundTrip proves the session is relayed end to end: an echo comes back.
func roundTrip(t *testing.T, c *websocket.Conn) {
	t.Helper()
	rtWrite(t, c, websocket.MessageText, []byte(`{"type":"session.update"}`))
	if _, data := rtRead(t, c); string(data) != `{"type":"session.update"}` {
		t.Fatalf("echo = %q", data)
	}
}

func appendEvent(pcmBytes int) []byte {
	return []byte(`{"type":"input_audio_buffer.append","audio":"` + base64.StdEncoding.EncodeToString(make([]byte, pcmBytes)) + `"}`)
}

// syncWriter is a goroutine-safe sink for a captured log.
type syncWriter struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (w *syncWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

func (w *syncWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

// captureRTLog sends every log record, Debug included, to the returned writer until
// the test ends.
func captureRTLog(t *testing.T) *syncWriter {
	t.Helper()
	w := &syncWriter{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return w
}

// A bearer key opens a session; a wrong key, no key and a key without the
// gateway:use or llm:invoke scope are refused before any upgrade, and no agent is
// dialed for them.
func TestRealtimeBearerAuth(t *testing.T) {
	rig := newRTRig(t)
	for _, tc := range []struct {
		name   string
		header http.Header
		status int
	}{
		{"wrong key", http.Header{"Authorization": {"Bearer nope"}}, http.StatusUnauthorized},
		{"no key", nil, http.StatusUnauthorized},
		{"wrong key in the subprotocol", nil, http.StatusUnauthorized},
		{"no inference scope", http.Header{"Authorization": {"Bearer admin-only-secret"}}, http.StatusForbidden},
		{"a wrong header wins over a right subprotocol key", http.Header{"Authorization": {"Bearer nope"}}, http.StatusUnauthorized},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := &websocket.DialOptions{HTTPHeader: tc.header}
			switch tc.name {
			case "wrong key in the subprotocol":
				opts.Subprotocols = []string{"realtime", "openai-insecure-api-key.nope"}
			case "a wrong header wins over a right subprotocol key":
				opts.Subprotocols = []string{"realtime", "openai-insecure-api-key." + rtSecret}
			}
			conn, resp, err := rig.dialWith(t, "/v1/realtime", "model="+rtModel, opts)
			if err == nil || conn != nil {
				t.Fatal("the session was opened")
			}
			if statusOf(resp) != tc.status {
				t.Fatalf("status = %d, want %d before any upgrade", statusOf(resp), tc.status)
			}
			rig.waitHandled(t)
		})
	}
	if n := rig.agent.dials.Load(); n != 0 {
		t.Fatalf("agent dials = %d, want 0", n)
	}
	if rows := rig.srv.Usage.All(); len(rows) != 0 {
		t.Fatalf("usage rows = %d, want none for a refused handshake", len(rows))
	}

	conn := rig.dial(t, "model="+rtModel)
	roundTrip(t, conn)
	_ = conn.Close(websocket.StatusNormalClosure, "")
	if ev := rig.oneRow(t); ev.UserID != "usr_dev" || ev.TokenID != "tok_dev" {
		t.Fatalf("row attribution = %q/%q, want usr_dev/tok_dev", ev.UserID, ev.TokenID)
	}
}

// The OpenAI browser convention: a client that offers "realtime" plus
// "openai-insecure-api-key.<key>" is authenticated with that key, and the handshake
// selects "realtime" alone. The key is never echoed in the response, never logged,
// and never reaches the agent.
func TestRealtimeSubprotocolAuthSelectsRealtimeOnly(t *testing.T) {
	logs := captureRTLog(t)
	rig := newRTRig(t)
	conn, resp, err := rig.dialWith(t, "/v1/realtime", "model="+rtModel, &websocket.DialOptions{
		Subprotocols: []string{"realtime", "openai-insecure-api-key." + rtSecret, "openai-beta.realtime-v1"},
		HTTPHeader:   http.Header{"Origin": {"https://app.example.test"}},
	})
	if err != nil {
		t.Fatalf("dial: %v (status %d)", err, statusOf(resp))
	}
	if got := conn.Subprotocol(); got != "realtime" {
		t.Fatalf("selected subprotocol = %q, want realtime", got)
	}
	if got := resp.Header.Values("Sec-WebSocket-Protocol"); len(got) != 1 || got[0] != "realtime" {
		t.Fatalf("Sec-WebSocket-Protocol = %q, want exactly realtime", got)
	}
	for name, values := range resp.Header {
		for _, v := range values {
			if strings.Contains(v, rtSecret) {
				t.Fatalf("response header %s echoes the key", name)
			}
		}
	}
	seen := recvSeen(t, rig.agent)
	for name, values := range seen.header {
		for _, v := range values {
			if strings.Contains(v, rtSecret) {
				t.Fatalf("the agent was sent the key in %s", name)
			}
		}
	}
	roundTrip(t, conn)
	_ = conn.Close(websocket.StatusNormalClosure, "")
	if ev := rig.oneRow(t); ev.TokenID != "tok_dev" {
		t.Fatalf("row token = %q, want tok_dev", ev.TokenID)
	}
	if strings.Contains(logs.String(), rtSecret) {
		t.Fatal("the key was logged")
	}
}

// A handshake without a model is 400 realtime.model_required before any upgrade.
func TestRealtimeMissingModelIs400(t *testing.T) {
	rig := newRTRig(t)
	for _, query := range []string{"", "model=", "model=%20&intent=transcription"} {
		_, resp, err := rig.dialWith(t, "/v1/realtime", query, &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + rtSecret}}})
		if err == nil || statusOf(resp) != http.StatusBadRequest {
			t.Fatalf("query %q: status = %d, want 400", query, statusOf(resp))
		}
		body, _ := io.ReadAll(resp.Body)
		requireErrorCode(t, string(body), "realtime.model_required")
		rig.waitHandled(t)
	}
	if n := rig.agent.dials.Load(); n != 0 {
		t.Fatalf("agent dials = %d, want 0", n)
	}
}

// A request that is not a WebSocket handshake is refused before anything is spent on
// it -- no admission, no agent dial, no usage row -- and a method other than GET is 405.
func TestRealtimeRefusesWhatIsNotAHandshake(t *testing.T) {
	rig := newRTRig(t)
	for _, tc := range []struct {
		name   string
		method string
		header http.Header
		status int
	}{
		{"plain GET", http.MethodGet, nil, http.StatusUpgradeRequired},
		{"wrong version", http.MethodGet, http.Header{"Connection": {"Upgrade"}, "Upgrade": {"websocket"}, "Sec-Websocket-Version": {"8"}, "Sec-Websocket-Key": {"dGhlIHNhbXBsZSBub25jZQ=="}}, http.StatusBadRequest},
		{"bad key", http.MethodGet, http.Header{"Connection": {"Upgrade"}, "Upgrade": {"websocket"}, "Sec-Websocket-Version": {"13"}, "Sec-Websocket-Key": {"short"}}, http.StatusBadRequest},
		{"POST", http.MethodPost, nil, http.StatusMethodNotAllowed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, _ := http.NewRequest(tc.method, rig.ts.URL+"/v1/realtime?model="+rtModel, nil)
			for k, v := range tc.header {
				req.Header[k] = v
			}
			req.Header.Set("Authorization", "Bearer "+rtSecret)
			resp, err := rig.ts.Client().Do(req)
			if err != nil {
				t.Fatalf("do: %v", err)
			}
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode != tc.status {
				t.Fatalf("status = %d, want %d (%s)", resp.StatusCode, tc.status, body)
			}
			if tc.status != http.StatusMethodNotAllowed {
				requireErrorCode(t, string(body), "realtime.upgrade_required")
			}
			rig.waitHandled(t)
		})
	}
	if n := rig.agent.dials.Load(); n != 0 {
		t.Fatalf("agent dials = %d, want 0", n)
	}
	if rows := rig.srv.Usage.All(); len(rows) != 0 {
		t.Fatalf("usage rows = %d, want none", len(rows))
	}
}

// Frames cross in both directions with their type and their bytes unchanged, on both
// routes (/v1/realtime and its /openai alias).
func TestRealtimeRelaysFramesBothWays(t *testing.T) {
	for _, path := range []string{"/v1/realtime", "/openai/v1/realtime"} {
		t.Run(path, func(t *testing.T) {
			rig := newRTRig(t)
			conn, resp, err := rig.dialWith(t, path, "model="+rtModel, &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + rtSecret}}})
			if err != nil {
				t.Fatalf("dial: %v (status %d)", err, statusOf(resp))
			}
			text := []byte(`{"type":"session.update","session":{"instructions":"café \\u00e9"}}`)
			rtWrite(t, conn, websocket.MessageText, text)
			if typ, data := rtRead(t, conn); typ != websocket.MessageText || !bytes.Equal(data, text) {
				t.Fatalf("text echo = %v %q, want the same text", typ, data)
			}
			bin := []byte{0, 1, 2, 0xff, 0xfe}
			rtWrite(t, conn, websocket.MessageBinary, bin)
			if typ, data := rtRead(t, conn); typ != websocket.MessageBinary || !bytes.Equal(data, bin) {
				t.Fatalf("binary echo = %v %v, want the same binary", typ, data)
			}
			event := `{"type":"response.created","response":{"id":"resp_1"}}`
			rtWrite(t, conn, websocket.MessageText, []byte("agent:send "+event))
			if typ, data := rtRead(t, conn); typ != websocket.MessageText || string(data) != event {
				t.Fatalf("agent-sent event = %v %q", typ, data)
			}
			_ = conn.Close(websocket.StatusNormalClosure, "")
			if ev := rig.oneRow(t); ev.ReqPath != path {
				t.Fatalf("row req path = %q, want %q", ev.ReqPath, path)
			}
		})
	}
}

// The agent is dialed at /realtime/<upstream model, path-escaped> with the client's
// query minus model, and with nothing of the client's handshake: no Origin, no client
// subprotocol offer, no client Authorization.
func TestRealtimeForwardsTheQueryMinusModel(t *testing.T) {
	rig := newRTRig(t)
	conn, resp, err := rig.dialWith(t, "/v1/realtime", "intent=transcription&model="+rtModel+"&x=a+b", &websocket.DialOptions{
		HTTPHeader:   http.Header{"Authorization": {"Bearer " + rtSecret}, "Origin": {"https://app.example.test"}},
		Subprotocols: []string{"realtime"},
	})
	if err != nil {
		t.Fatalf("dial: %v (status %d)", err, statusOf(resp))
	}
	seen := recvSeen(t, rig.agent)
	if seen.path != "/realtime/"+rtUpstream {
		t.Fatalf("agent path = %q, want /realtime/%s", seen.path, rtUpstream)
	}
	q, _ := url.ParseQuery(seen.query)
	if q.Has("model") || q.Get("intent") != "transcription" || q.Get("x") != "a b" || len(q) != 2 {
		t.Fatalf("agent query = %q, want intent and x without model", seen.query)
	}
	for _, h := range []string{"Origin", "Authorization", "Sec-Websocket-Protocol"} {
		if v := seen.header.Get(h); v != "" {
			t.Fatalf("agent got %s = %q, want none", h, v)
		}
	}
	roundTrip(t, conn)
	_ = conn.Close(websocket.StatusNormalClosure, "")
	rig.oneRow(t)
}

// An instance address (m#k) and a name that needs escaping reach the agent's path
// escaped, so the agent's decoded path is the address itself.
func TestRealtimeUpstreamPathEscapesTheModel(t *testing.T) {
	if got := realtimeUpstreamPath(routing.Target{ProviderModel: "org/qwen omni#2"}); got != "/realtime/org/qwen%20omni%232" {
		t.Fatalf("realtimeUpstreamPath = %q", got)
	}
	if got := realtimeUpstreamPath(routing.Target{Model: "omni"}); got != "/realtime/omni" {
		t.Fatalf("realtimeUpstreamPath without a provider model = %q", got)
	}
}

// The agent's refusal of the handshake (its child dial failed) is the gateway's own
// 502 before any upgrade; the session is recorded once as an error row and leaves no
// active request.
func TestRealtimeDialFailureIs502BeforeUpgrade(t *testing.T) {
	rig := newRTRig(t)
	rig.agent.refuse = http.StatusBadGateway
	_, resp, err := rig.dialWith(t, "/v1/realtime", "model="+rtModel, &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + rtSecret}}})
	if err == nil || statusOf(resp) != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", statusOf(resp))
	}
	body, _ := io.ReadAll(resp.Body)
	requireErrorCode(t, string(body), "provider.unavailable")
	ev := rig.oneRow(t)
	if ev.Status != "error" || ev.HTTPStatus != http.StatusBadGateway || ev.ErrorCode != "provider.unavailable" {
		t.Fatalf("row = status %q http %d code %q, want an error row 502 provider.unavailable", ev.Status, ev.HTTPStatus, ev.ErrorCode)
	}
	if ev.RouteID != rtMappingID || ev.BillingUnit != usage.BillingUnitAudioSecond || ev.BillingQuantity != 0 {
		t.Fatalf("row = route %q unit %q qty %v, want the route with a zero audio measure", ev.RouteID, ev.BillingUnit, ev.BillingQuantity)
	}
}

// A client that leaves while the agent is still answering the dial (starting the
// model) abandons the dial: the agent's handshake is given up, and the session is
// recorded once, as an error row naming the client's departure.
func TestRealtimeClientLeavingDuringTheDialAbandonsIt(t *testing.T) {
	rig := newRTRig(t)
	rig.agent.hold = make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	dialed := make(chan error, 1)
	go func() {
		_, _, err := websocket.Dial(ctx, rig.ts.URL+"/v1/realtime?model="+rtModel, &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + rtSecret}}})
		dialed <- err
	}()
	recvSeen(t, rig.agent)
	if n := len(rig.srv.Active.Snapshot()); n != 1 {
		t.Fatalf("active requests while dialing = %d, want 1", n)
	}
	cancel()
	if err := <-dialed; err == nil {
		t.Fatal("the client's dial succeeded")
	}
	if end := recvEnd(t, rig.agent); !end.self {
		t.Fatalf("agent end = %+v, want the held handshake given up", end)
	}
	ev := rig.oneRow(t)
	if ev.Status != "error" || ev.ErrorCode != errCodeClientDisconnected {
		t.Fatalf("row = status %q code %q, want an error row %s", ev.Status, ev.ErrorCode, errCodeClientDisconnected)
	}
}

// An agent that has not declared realtime_proxy is never dialed: the request is
// refused with the routing answer for a model with no candidate.
func TestRealtimeNeverDialsAnAgentWithoutTheFeature(t *testing.T) {
	for _, features := range [][]string{{"runtime_manager", "fullduplex_proxy"}, nil} {
		agent := newRTAgent(t)
		rig := newRTRigWith(t, rtCandidate{serverID: rtServerID, appID: "app-rt", mappingID: rtMappingID, agent: agent, priority: 10, features: features})
		_, resp, err := rig.dialWith(t, "/v1/realtime", "model="+rtModel, &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + rtSecret}}})
		if err == nil || statusOf(resp) != http.StatusNotFound {
			t.Fatalf("features %v: status = %d, want 404", features, statusOf(resp))
		}
		body, _ := io.ReadAll(resp.Body)
		requireErrorCode(t, string(body), "routing.no_model_route")
		rig.waitHandled(t)
		if n := agent.dials.Load(); n != 0 {
			t.Fatalf("features %v: agent dials = %d, want 0", features, n)
		}
	}
}

// A model mapped on an agent without the feature AND on one with it reaches the one
// with it, even when routing prefers the other: the unserved instance is excluded and
// the request resolved again. The unserved agent is never dialed.
func TestRealtimeFallsBackToAServedAgent(t *testing.T) {
	unserved, served := newRTAgent(t), newRTAgent(t)
	rig := newRTRigWith(t,
		rtCandidate{serverID: "srv-old", appID: "app-old", mappingID: "route-old", agent: unserved, priority: 100, features: []string{"runtime_manager"}},
		rtCandidate{serverID: "srv-new", appID: "app-new", mappingID: "route-new", agent: served, priority: 1, features: []string{rtFeatureName}},
	)
	token, ok := rig.srv.Tokens.LookupBearer("Bearer " + rtSecret)
	if !ok {
		t.Fatal("the dev token does not resolve")
	}
	if target, err := rig.srv.Resolver.Resolve(context.Background(), token, realtimeProbeRequest()); err != nil || target.ServerID != "srv-old" {
		t.Fatalf("plain resolve = %q, %v; the fixture must prefer the unserved agent", target.ServerID, err)
	}
	conn := rig.dial(t, "model="+rtModel)
	roundTrip(t, conn)
	_ = conn.Close(websocket.StatusNormalClosure, "")
	if ev := rig.oneRow(t); ev.RouteID != "route-new" || ev.Host != "srv-new" {
		t.Fatalf("row = route %q host %q, want route-new on srv-new", ev.RouteID, ev.Host)
	}
	if n := unserved.dials.Load(); n != 0 {
		t.Fatalf("unserved agent dials = %d, want 0", n)
	}
	if n := served.dials.Load(); n != 1 {
		t.Fatalf("served agent dials = %d, want 1", n)
	}
}

// realtimeProbeRequest is the request the handler resolves, without the gate.
func realtimeProbeRequest() inference.Request {
	return inference.Request{Model: rtModel, RequestedModel: rtModel, APIFlavor: routing.APIFlavorOpenAIAudio, RequiredCapabilities: []string{routing.CapabilityRealtime}}
}

// The target loop is bounded: a resolver that keeps answering unserved targets is
// asked at most realtimeMaxTargetAttempts times, a target without an instance key
// ends the loop at once, and a target already excluded (a pin that ignores the
// exclusion) ends it too. Each ends in the no-candidate refusal.
func TestRealtimeTargetLoopIsBounded(t *testing.T) {
	srv := &Server{AgentFeatures: NewAgentFeaturesRegistry()}
	served := routing.Target{Provider: routing.ProviderServerAgent, ServerID: "srv-ok", InstanceKey: "map-ok#0", APIFlavors: []string{routing.APIFlavorOpenAIAudio}}
	srv.AgentFeatures.Set("srv-ok", []string{rtFeatureName})
	unserved := func(k int) routing.Target {
		return routing.Target{Provider: routing.ProviderServerAgent, ServerID: "srv-old", InstanceKey: "map-old#" + strconv.Itoa(k), APIFlavors: []string{routing.APIFlavorOpenAIAudio}}
	}
	for _, tc := range []struct {
		name      string
		answers   func(call int) routing.Target
		wantCalls int
		wantOK    bool
	}{
		{"served on the third try", func(call int) routing.Target {
			if call == 3 {
				return served
			}
			return unserved(call)
		}, 3, true},
		{"never served", unserved, realtimeMaxTargetAttempts, false},
		{"no instance key", func(int) routing.Target { return routing.Target{Provider: routing.ProviderAudioCpp} }, 1, false},
		{"a pin that ignores the exclusion", func(int) routing.Target { return unserved(0) }, 2, false},
		{"a target whose spec excludes audio", func(call int) routing.Target {
			if call == 1 {
				t := served
				t.APIFlavors = []string{routing.APIFlavorOpenAI}
				t.InstanceKey = "map-text#0"
				return t
			}
			return served
		}, 2, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			var excluded [][]string
			resolve := func(_ context.Context, req inference.Request) (routing.Target, error) {
				calls++
				excluded = append(excluded, append([]string(nil), req.ExcludeInstanceKeys...))
				return tc.answers(calls), nil
			}
			target, err := srv.selectRealtimeTarget(context.Background(), realtimeProbeRequest(), resolve)
			if calls != tc.wantCalls {
				t.Fatalf("resolver calls = %d, want %d (exclusions %v)", calls, tc.wantCalls, excluded)
			}
			if tc.wantOK {
				if err != nil || target.InstanceKey != served.InstanceKey {
					t.Fatalf("target = %q, %v; want the served one", target.InstanceKey, err)
				}
				return
			}
			if !errors.Is(err, routing.ErrNoModelRoute) {
				t.Fatalf("err = %v, want routing.ErrNoModelRoute", err)
			}
		})
	}
	t.Run("a refusal after an exclusion is the no-candidate refusal; one before it is kept", func(t *testing.T) {
		calls := 0
		resolve := func(context.Context, inference.Request) (routing.Target, error) {
			calls++
			if calls == 1 {
				return unserved(1), nil
			}
			return routing.Target{}, routing.ErrNoHealthyHost
		}
		if _, err := srv.selectRealtimeTarget(context.Background(), realtimeProbeRequest(), resolve); !errors.Is(err, routing.ErrNoModelRoute) {
			t.Fatalf("err = %v, want routing.ErrNoModelRoute", err)
		}
		first := func(context.Context, inference.Request) (routing.Target, error) {
			return routing.Target{}, routing.ErrModelNotCapable
		}
		if _, err := srv.selectRealtimeTarget(context.Background(), realtimeProbeRequest(), first); !errors.Is(err, routing.ErrModelNotCapable) {
			t.Fatalf("err = %v, want the resolver's own refusal", err)
		}
		queued := func(context.Context, inference.Request) (routing.Target, error) {
			calls++
			if calls%2 == 1 {
				return unserved(9), nil
			}
			return routing.Target{}, routing.ErrAdmissionQueueTimeout
		}
		calls = 0
		if _, err := srv.selectRealtimeTarget(context.Background(), realtimeProbeRequest(), queued); !errors.Is(err, routing.ErrAdmissionQueueTimeout) {
			t.Fatalf("err = %v, want the queue timeout kept", err)
		}
	})
}

// Token usage: the usage of every response.done whose response.usage is an object is
// summed -- prompt_tokens/completion_tokens (sglang-omni's conversation flavor) and
// input_tokens/output_tokens alike -- and a null usage adds nothing. The row is
// token-metered (no billing quantity, per the billing XOR), status success with the
// handshake's 101.
func TestRealtimeUsageSumsTokensFromResponseDone(t *testing.T) {
	rig := newRTRig(t)
	conn := rig.dial(t, "model="+rtModel)
	for _, ev := range []string{
		`{"type":"response.done","response":{"id":"r1","status":"completed","usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15,"engine_time_s":0.4}}}`,
		`{"type":"response.done","response":{"id":"r2","status":"cancelled","usage":null}}`,
		`{"type":"response.done","response":{"id":"r3","status":"completed","usage":{"input_tokens":7,"output_tokens":3,"total_tokens":10}}}`,
		`{"type":"response.text.done","response":{"usage":{"prompt_tokens":1000}}}`,
	} {
		rtWrite(t, conn, websocket.MessageText, []byte("agent:send "+ev))
		if _, data := rtRead(t, conn); string(data) != ev {
			t.Fatalf("relayed %q, want %q", data, ev)
		}
	}
	rtWrite(t, conn, websocket.MessageText, appendEvent(32000)) // tokens win over audio
	rtRead(t, conn)
	_ = conn.Close(websocket.StatusNormalClosure, "")
	ev := rig.oneRow(t)
	if ev.InputTokens != 17 || ev.OutputTokens != 8 || ev.TotalTokens != 25 {
		t.Fatalf("tokens = in %d out %d total %d, want 17/8/25", ev.InputTokens, ev.OutputTokens, ev.TotalTokens)
	}
	if ev.BillingUnit != usage.BillingUnitTokens || ev.BillingQuantity != 0 {
		t.Fatalf("billing = %v %q, want token-metered", ev.BillingQuantity, ev.BillingUnit)
	}
	if err := usage.ValidateBillingXOR(ev); err != nil {
		t.Fatalf("billing XOR: %v", err)
	}
	if ev.Status != "success" || ev.HTTPStatus != http.StatusSwitchingProtocols || ev.ErrorCode != "" {
		t.Fatalf("row = status %q http %d code %q, want success 101", ev.Status, ev.HTTPStatus, ev.ErrorCode)
	}
	if ev.Model != rtModel || ev.ProviderModel != rtUpstream || ev.APIFlavor != routing.APIFlavorOpenAIAudio || ev.ProviderPath != "/v1/realtime" || !ev.Stream {
		t.Fatalf("row = model %q provider model %q flavor %q provider path %q stream %v", ev.Model, ev.ProviderModel, ev.APIFlavor, ev.ProviderPath, ev.Stream)
	}
}

// Without any usage, the session bills audio seconds: the decoded bytes of the
// client's input_audio_buffer.append audio over 32000 (PCM16 mono at 16 kHz).
func TestRealtimeUsageFallsBackToAppendedAudio(t *testing.T) {
	rig := newRTRig(t)
	conn := rig.dial(t, "model="+rtModel)
	for _, n := range []int{32000, 16000} {
		rtWrite(t, conn, websocket.MessageText, appendEvent(n))
		rtRead(t, conn)
	}
	rtWrite(t, conn, websocket.MessageText, []byte(`{"type":"response.done","response":{"usage":null}}`))
	rtRead(t, conn)
	_ = conn.Close(websocket.StatusNormalClosure, "")
	ev := rig.oneRow(t)
	if ev.BillingUnit != usage.BillingUnitAudioSecond || ev.BillingQuantity != 1.5 {
		t.Fatalf("billing = %v %q, want 1.5 audio_second", ev.BillingQuantity, ev.BillingUnit)
	}
	if err := usage.ValidateBillingXOR(ev); err != nil {
		t.Fatalf("billing XOR: %v", err)
	}
}

// A frame stream with nothing countable in it -- text that is not JSON, binary frames,
// an append whose audio holds no base64 character, an append that is no object, a
// response.done with no usage -- is relayed unchanged and bills a zero that is logged.
func TestRealtimeMalformedFramesBillALoggedZero(t *testing.T) {
	logs := captureRTLog(t)
	rig := newRTRig(t)
	conn := rig.dial(t, "model="+rtModel)
	for _, frame := range []struct {
		typ  websocket.MessageType
		data string
	}{
		{websocket.MessageText, `not json at all`},
		{websocket.MessageBinary, `{"type":"input_audio_buffer.append","audio":"AAAA"}`},
		{websocket.MessageText, `{"type":"input_audio_buffer.append","audio":"!!! ==="}`},
		{websocket.MessageText, `["input_audio_buffer.append"]`},
		{websocket.MessageText, `{"type":"input_audio_buffer.append","audio":42}`},
		{websocket.MessageText, `{"type":"response.done"}`},
	} {
		rtWrite(t, conn, frame.typ, []byte(frame.data))
		if typ, data := rtRead(t, conn); typ != frame.typ || string(data) != frame.data {
			t.Fatalf("relayed %v %q, want %v %q", typ, data, frame.typ, frame.data)
		}
	}
	_ = conn.Close(websocket.StatusNormalClosure, "")
	ev := rig.oneRow(t)
	if ev.BillingUnit != usage.BillingUnitAudioSecond || ev.BillingQuantity != 0 || ev.InputTokens != 0 {
		t.Fatalf("billing = %v %q tokens %d, want a zero audio_second", ev.BillingQuantity, ev.BillingUnit, ev.InputTokens)
	}
	if !strings.Contains(logs.String(), "realtime session billed zero") {
		t.Fatal("the zero was not logged")
	}
}

// The session shows in Active Requests from before the dial until it ends, attributed
// to the token, the target and the realtime path.
func TestRealtimeActiveRequestShowsTheOpenSession(t *testing.T) {
	rig := newRTRig(t)
	conn := rig.dial(t, "model="+rtModel)
	roundTrip(t, conn)
	rows := rig.srv.Active.Snapshot()
	if len(rows) != 1 {
		t.Fatalf("active requests = %d, want 1", len(rows))
	}
	row := rows[0]
	if row.ReqPath != "/v1/realtime" || row.Model != rtModel || row.TokenID != "tok_dev" || row.ServerID != rtServerID || row.InstanceKey != routing.InstanceKey(rtMappingID, 0) || !row.Stream {
		t.Fatalf("active row = %+v", row)
	}
	if row.ProviderPath != "/realtime/"+rtUpstream || row.APIFlavor != routing.APIFlavorOpenAIAudio {
		t.Fatalf("active row provider path %q flavor %q", row.ProviderPath, row.APIFlavor)
	}
	_ = conn.Close(websocket.StatusNormalClosure, "")
	rig.oneRow(t)
}

// A close is relayed with its status and reason in both directions, and a close
// without a status (1005) is relayed as one.
func TestRealtimeRelaysTheCloseStatus(t *testing.T) {
	t.Run("client closes", func(t *testing.T) {
		rig := newRTRig(t)
		conn := rig.dial(t, "model="+rtModel)
		roundTrip(t, conn)
		_ = conn.Close(4002, "client says bye")
		if end := recvEnd(t, rig.agent); end.code != 4002 || end.reason != "client says bye" {
			t.Fatalf("agent end = %+v, want 4002 client says bye", end)
		}
		rig.oneRow(t)
	})
	t.Run("agent closes", func(t *testing.T) {
		rig := newRTRig(t)
		conn := rig.dial(t, "model="+rtModel)
		rtWrite(t, conn, websocket.MessageText, []byte("agent:close 4001"))
		if code, reason := rtReadClose(t, conn); code != 4001 || reason != "agent says bye" {
			t.Fatalf("client close = %d %q, want 4001 agent says bye", code, reason)
		}
		rig.oneRow(t)
	})
	t.Run("client closes without a status", func(t *testing.T) {
		rig := newRTRig(t)
		conn := rig.dial(t, "model="+rtModel)
		roundTrip(t, conn)
		_ = conn.Close(websocket.StatusNoStatusRcvd, "")
		if end := recvEnd(t, rig.agent); end.code != websocket.StatusNoStatusRcvd {
			t.Fatalf("agent end = %+v, want 1005", end)
		}
		if ev := rig.oneRow(t); ev.Status != "success" {
			t.Fatalf("row status = %q, want success for a close without a status", ev.Status)
		}
	})
	t.Run("agent closes without a status", func(t *testing.T) {
		rig := newRTRig(t)
		conn := rig.dial(t, "model="+rtModel)
		rtWrite(t, conn, websocket.MessageText, []byte("agent:emptyclose"))
		if code, _ := rtReadClose(t, conn); code != websocket.StatusNoStatusRcvd {
			t.Fatalf("client close = %d, want 1005", code)
		}
		rig.oneRow(t)
	})
}

// A close is relayed with the closer's own status even while messages flow towards
// the closer. The pump writing to the closer fails the moment the closer's socket
// closes, racing the closer's own pump, which reads the real status; without the
// settle the far side got 1011 "<closer> connection lost". Each direction runs many
// sessions, so a race left open fails all but certainly.
func TestRealtimeCloseUnderTrafficRelaysTheClosersStatus(t *testing.T) {
	const runs = 25
	t.Run("the client closes while the agent floods it", func(t *testing.T) {
		for i := range runs {
			rig := newRTRig(t)
			conn := rig.dial(t, "model="+rtModel)
			rtWrite(t, conn, websocket.MessageText, []byte("agent:flood"))
			rtRead(t, conn)
			rtRead(t, conn)
			_ = conn.Close(4002, "client says bye")
			if end := recvEnd(t, rig.agent); end.code != 4002 || end.reason != "client says bye" {
				t.Fatalf("run %d: agent end = %+v, want 4002 client says bye", i, end)
			}
			rig.oneRow(t)
		}
	})
	t.Run("the agent closes while the client floods it", func(t *testing.T) {
		for i := range runs {
			rig := newRTRig(t)
			conn := rig.dial(t, "model="+rtModel)
			roundTrip(t, conn)
			flooding := make(chan struct{})
			go func() {
				defer close(flooding)
				ctx := context.Background()
				for {
					if err := conn.Write(ctx, websocket.MessageText, []byte(`noecho:{"type":"input_audio_buffer.append","audio":"AAAA"}`)); err != nil {
						return
					}
				}
			}()
			rtWrite(t, conn, websocket.MessageText, []byte("agent:close 4001"))
			if code, reason := rtReadClose(t, conn); code != 4001 || reason != "agent says bye" {
				t.Fatalf("run %d: client close = %d %q, want 4001 agent says bye", i, code, reason)
			}
			<-flooding
			rig.oneRow(t)
		}
	})
}

// Every end of a session -- either side closing or vanishing, an oversized message
// from either side, the gateway's idle bound, the gateway shutting down -- closes the
// other side as described, records exactly one usage row and removes the active
// request.
func TestRealtimeRecordsOneRowOnEveryEnd(t *testing.T) {
	for _, tc := range []struct {
		name       string
		setup      func(*rtRig)
		act        func(t *testing.T, rig *rtRig, conn *websocket.Conn, cancel context.CancelFunc)
		clientCode websocket.StatusCode // what the client reads, 0 when it closed itself
		agentCode  websocket.StatusCode // what the agent reads, 0 when it ended the session itself
		status     string
		errorCode  string
	}{
		{name: "client closes normally", act: func(_ *testing.T, _ *rtRig, c *websocket.Conn, _ context.CancelFunc) {
			_ = c.Close(websocket.StatusNormalClosure, "done")
		}, agentCode: websocket.StatusNormalClosure, status: "success"},
		{name: "client goes away", act: func(_ *testing.T, _ *rtRig, c *websocket.Conn, _ context.CancelFunc) {
			_ = c.Close(websocket.StatusGoingAway, "")
		}, agentCode: websocket.StatusGoingAway, status: "success"},
		{name: "client vanishes", act: func(_ *testing.T, _ *rtRig, c *websocket.Conn, _ context.CancelFunc) {
			_ = c.CloseNow()
		}, agentCode: websocket.StatusInternalError, status: "error", errorCode: "realtime.close_1011"},
		{name: "agent closes", act: func(t *testing.T, _ *rtRig, c *websocket.Conn, _ context.CancelFunc) {
			rtWrite(t, c, websocket.MessageText, []byte("agent:close 1008"))
		}, clientCode: websocket.StatusPolicyViolation, status: "error", errorCode: "realtime.close_1008"},
		{name: "agent vanishes", act: func(t *testing.T, _ *rtRig, c *websocket.Conn, _ context.CancelFunc) {
			rtWrite(t, c, websocket.MessageText, []byte("agent:drop"))
		}, clientCode: websocket.StatusInternalError, status: "error", errorCode: "realtime.close_1011"},
		{name: "oversized message from the client", act: func(t *testing.T, _ *rtRig, c *websocket.Conn, _ context.CancelFunc) {
			rtWrite(t, c, websocket.MessageBinary, make([]byte, rtMessageLimit+1))
		}, clientCode: websocket.StatusMessageTooBig, agentCode: websocket.StatusMessageTooBig, status: "error", errorCode: "realtime.close_1009"},
		{name: "oversized message from the agent", act: func(t *testing.T, _ *rtRig, c *websocket.Conn, _ context.CancelFunc) {
			rtWrite(t, c, websocket.MessageText, []byte("agent:big"))
		}, clientCode: websocket.StatusMessageTooBig, agentCode: websocket.StatusMessageTooBig, status: "error", errorCode: "realtime.close_1009"},
		{name: "gateway idle bound", setup: func(rig *rtRig) { rig.srv.realtimeIdle = 50 * time.Millisecond }, act: func(*testing.T, *rtRig, *websocket.Conn, context.CancelFunc) {
			// Nothing: the session stays silent until the idle bound ends it.
		}, clientCode: websocket.StatusPolicyViolation, agentCode: websocket.StatusPolicyViolation, status: "error", errorCode: "realtime.close_1008"},
		{name: "gateway shuts down", act: func(_ *testing.T, _ *rtRig, _ *websocket.Conn, cancel context.CancelFunc) {
			cancel()
		}, clientCode: websocket.StatusGoingAway, agentCode: websocket.StatusGoingAway, status: "success"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newRTRig(t)
			lifetime, cancel := context.WithCancel(context.Background())
			defer cancel()
			rig.srv.SetBaseContext(lifetime)
			if tc.setup != nil {
				tc.setup(rig)
			}
			conn := rig.dial(t, "model="+rtModel)
			if tc.name != "gateway idle bound" {
				roundTrip(t, conn)
			}
			tc.act(t, rig, conn, cancel)
			if tc.clientCode != 0 {
				if code, _ := rtReadClose(t, conn); code != tc.clientCode {
					t.Fatalf("client close = %d, want %d", code, tc.clientCode)
				}
			}
			if tc.agentCode != 0 {
				if end := recvEnd(t, rig.agent); end.code != tc.agentCode {
					t.Fatalf("agent end = %+v, want %d", end, tc.agentCode)
				}
			}
			ev := rig.oneRow(t)
			if ev.Status != tc.status || ev.ErrorCode != tc.errorCode || ev.HTTPStatus != http.StatusSwitchingProtocols {
				t.Fatalf("row = status %q code %q http %d, want %q %q 101", ev.Status, ev.ErrorCode, ev.HTTPStatus, tc.status, tc.errorCode)
			}
			select {
			case n := <-rig.agent.oversize:
				t.Fatalf("the agent received a %d-byte message; the gateway must stop it at %d", n, rtMessageLimit)
			default:
			}
		})
	}
}

// A message of exactly 4 MiB crosses in both directions.
func TestRealtimeFourMiBCrosses(t *testing.T) {
	rig := newRTRig(t)
	conn := rig.dial(t, "model="+rtModel)
	exact := bytes.Repeat([]byte{'a'}, rtMessageLimit)
	rtWrite(t, conn, websocket.MessageBinary, exact)
	if _, data := rtRead(t, conn); len(data) != rtMessageLimit {
		t.Fatalf("echo of 4 MiB = %d bytes", len(data))
	}
	_ = conn.Close(websocket.StatusNormalClosure, "")
	rig.oneRow(t)
	if realtimeMessageLimit != rtMessageLimit {
		t.Fatalf("realtimeMessageLimit = %d, want 4 MiB", realtimeMessageLimit)
	}
}

// sendableRealtimeCode passes on every code a close frame may carry, keeps 1005 (sent
// as a close frame without a status), and turns every other code into 1011.
func TestSendableRealtimeCode(t *testing.T) {
	for _, code := range []websocket.StatusCode{1000, 1001, 1003, 1007, 1009, 1011, 1014, 3000, 4999, websocket.StatusNoStatusRcvd} {
		if got := sendableRealtimeCode(code); got != code {
			t.Errorf("sendableRealtimeCode(%d) = %d, want it kept", code, got)
		}
	}
	for _, code := range []websocket.StatusCode{0, 999, 1004, 1006, 1015, 1016, 2999, 5000} {
		if got := sendableRealtimeCode(code); got != websocket.StatusInternalError {
			t.Errorf("sendableRealtimeCode(%d) = %d, want 1011", code, got)
		}
	}
}

// The audio meter reads a client's append the way the upstream does: sglang-omni
// parses the frame with Python's json (keys exact and case-sensitive, the last
// duplicate wins, escapes decoded) and its base64 decode skips characters outside
// the alphabet. A frame the upstream takes as an append of N bytes must count N
// bytes here, so no spelling of the frame lets a client bill its audio as zero.
func TestRealtimeMeterCountsAppendsAsTheUpstreamReadsThem(t *testing.T) {
	audio := base64.StdEncoding.EncodeToString(make([]byte, 32000))
	escapedSlashes := strings.ReplaceAll(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0xff}, 30)), "/", `\/`)
	for _, tc := range []struct {
		name  string
		typ   websocket.MessageType
		frame string
		want  int64
	}{
		{"plain", websocket.MessageText, `{"type":"input_audio_buffer.append","audio":"` + audio + `"}`, 32000},
		{"escaped type", websocket.MessageText, `{"type":"input_audio_buffer\u002eappend","audio":"` + audio + `"}`, 32000},
		{"escaped type, fully", websocket.MessageText, `{"\u0074ype":"\u0069nput_audio_buffer.append","audio":"` + audio + `"}`, 32000},
		{"a case-folded duplicate key", websocket.MessageText, `{"type":"input_audio_buffer.append","audio":"` + audio + `","AUDIO":""}`, 32000},
		{"a case-folded type key is not the type", websocket.MessageText, `{"Type":"input_audio_buffer.append","audio":"` + audio + `"}`, 0},
		{"the last duplicate audio wins", websocket.MessageText, `{"type":"input_audio_buffer.append","audio":"","audio":"` + audio + `"}`, 32000},
		{"the last duplicate audio wins, empty", websocket.MessageText, `{"type":"input_audio_buffer.append","audio":"` + audio + `","audio":""}`, 0},
		{"the last duplicate type wins", websocket.MessageText, `{"type":"session.update","type":"input_audio_buffer.append","audio":"` + audio + `"}`, 32000},
		{"a stray character in the base64", websocket.MessageText, `{"type":"input_audio_buffer.append","audio":"` + audio[:100] + ` ` + audio[100:] + `"}`, 32000},
		{"unpadded base64", websocket.MessageText, `{"type":"input_audio_buffer.append","audio":"` + base64.RawStdEncoding.EncodeToString(make([]byte, 31999)) + `"}`, 31999},
		{"escaped slashes", websocket.MessageText, `{"type":"input_audio_buffer.append","audio":"` + escapedSlashes + `"}`, 30},
		{"another type", websocket.MessageText, `{"type":"Input_Audio_Buffer.Append","audio":"` + audio + `"}`, 0},
		{"a binary frame", websocket.MessageBinary, `{"type":"input_audio_buffer.append","audio":"` + audio + `"}`, 0},
		{"audio that is no string", websocket.MessageText, `{"type":"input_audio_buffer.append","audio":42}`, 0},
		{"no object", websocket.MessageText, `["input_audio_buffer.append"]`, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var m realtimeMeter
			m.observeClient(tc.typ, []byte(tc.frame))
			if m.appendBytes != tc.want {
				t.Fatalf("appendBytes = %d, want %d", m.appendBytes, tc.want)
			}
		})
	}
}

// The subprotocol key's edges: of several key offers the first is used, an empty one
// is skipped (so an empty key alone authenticates nothing), and an Authorization
// header is used over any subprotocol key, right or wrong.
func TestRealtimeSubprotocolKeyEdges(t *testing.T) {
	rig := newRTRig(t)
	for _, tc := range []struct {
		name   string
		header http.Header
		offers []string
		status int // 0: the session opens
	}{
		{"the first of two keys is used", nil, []string{"realtime", "openai-insecure-api-key." + rtSecret, "openai-insecure-api-key.nope"}, 0},
		{"the first of two keys is used, wrong", nil, []string{"realtime", "openai-insecure-api-key.nope", "openai-insecure-api-key." + rtSecret}, http.StatusUnauthorized},
		{"an empty key alone", nil, []string{"realtime", "openai-insecure-api-key."}, http.StatusUnauthorized},
		{"an empty key is skipped", nil, []string{"realtime", "openai-insecure-api-key.", "openai-insecure-api-key." + rtSecret}, 0},
		{"a right header over a wrong key", http.Header{"Authorization": {"Bearer " + rtSecret}}, []string{"realtime", "openai-insecure-api-key.nope"}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn, resp, err := rig.dialWith(t, "/v1/realtime", "model="+rtModel, &websocket.DialOptions{HTTPHeader: tc.header, Subprotocols: tc.offers})
			if tc.status != 0 {
				if err == nil || statusOf(resp) != tc.status {
					t.Fatalf("status = %d, want %d", statusOf(resp), tc.status)
				}
				rig.waitHandled(t)
				return
			}
			if err != nil {
				t.Fatalf("dial: %v (status %d)", err, statusOf(resp))
			}
			if conn.Subprotocol() != realtimeSubprotocol {
				t.Fatalf("subprotocol = %q, want realtime", conn.Subprotocol())
			}
			roundTrip(t, conn)
			_ = conn.Close(websocket.StatusNormalClosure, "")
			rig.waitHandled(t)
		})
	}
}

// WaitRealtimeSessions is the shutdown's wait for the sessions the gateway's lifetime
// ended: the open session is closed with 1001 on both sides and its usage row is
// written before the wait returns, a handshake after it is refused, and the wait is
// bounded.
func TestRealtimeWaitSessionsOnShutdown(t *testing.T) {
	t.Run("no session", func(t *testing.T) {
		if !newRTRig(t).srv.WaitRealtimeSessions(rtWait) {
			t.Fatal("the wait for no session timed out")
		}
	})
	t.Run("an open session ends and is recorded", func(t *testing.T) {
		rig := newRTRig(t)
		lifetime, cancel := context.WithCancel(context.Background())
		defer cancel()
		rig.srv.SetBaseContext(lifetime)
		conn := rig.dial(t, "model="+rtModel)
		roundTrip(t, conn)
		// A client keeps reading, which is what answers the gateway's close.
		clientClose := make(chan websocket.StatusCode, 1)
		go func() {
			for {
				if _, _, err := conn.Read(context.Background()); err != nil {
					clientClose <- websocket.CloseStatus(err)
					return
				}
			}
		}()
		cancel()
		if !rig.srv.WaitRealtimeSessions(rtWait) {
			t.Fatal("the wait timed out")
		}
		if rows := rig.srv.Usage.All(); len(rows) != 1 {
			t.Fatalf("usage rows after the wait = %d, want 1", len(rows))
		}
		if code := <-clientClose; code != websocket.StatusGoingAway {
			t.Fatalf("client close = %d, want 1001", code)
		}
		if end := recvEnd(t, rig.agent); end.code != websocket.StatusGoingAway {
			t.Fatalf("agent end = %+v, want 1001", end)
		}
		rig.waitHandled(t)

		_, resp, err := rig.dialWith(t, "/v1/realtime", "model="+rtModel, &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + rtSecret}}})
		if err == nil || statusOf(resp) != http.StatusServiceUnavailable {
			t.Fatalf("a handshake after the wait: status = %d, want 503", statusOf(resp))
		}
		body, _ := io.ReadAll(resp.Body)
		requireErrorCode(t, string(body), realtimeShuttingDown)
		rig.waitHandled(t)
		if n := rig.agent.dials.Load(); n != 1 {
			t.Fatalf("agent dials = %d, want only the first session's", n)
		}
	})
	t.Run("the wait is bounded", func(t *testing.T) {
		srv := &Server{}
		if !srv.realtimeSessions.begin() {
			t.Fatal("begin refused before any wait")
		}
		defer srv.realtimeSessions.finish()
		if srv.WaitRealtimeSessions(10 * time.Millisecond) {
			t.Fatal("the wait for a session that never ends returned true")
		}
		if srv.realtimeSessions.begin() {
			t.Fatal("begin admitted a session after the wait")
		}
	})
}
