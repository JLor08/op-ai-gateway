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
	"op-ai-gateway/internal/capture"
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

	"github.com/coder/websocket"
)

// These tests drive the WHOLE voice turn end to end through the orchestration WebSocket:
// the browser sends PCM, the per-turn ASR loopback reaches a fake full-duplex child, a
// chunk end starts a SPECULATIVE voice run over the real /v1/chat/completions (mock LLM)
// and /v1/audio/speech (canned TTS) loopbacks, and the machine folds the run's answer/audio
// deltas back down while running the continuation window (spec 6.1) and degrading cleanly on
// a leg failure (spec 6.6). The harness wires all three legs so the run's own metering and
// persistence happen for real; only the upstreams are faked.

// voiceLoopProvider serves all three legs of a voice turn from one provider:
//   - the LLM leg via the embedded mock's CompleteStream (streamed, billed), or a forced
//     error when llmErr is set;
//   - the TTS leg via an overridden ProxyNative that answers /v1/audio/speech with the
//     canned SSE stream (or, when speechStatus is set, a non-2xx error envelope so a TTS /
//     admission failure can be injected);
//   - the ASR leg via ProxyNativeDuplex, delegated to a real OpenAI-compatible client that
//     reaches the fake full-duplex child at the resolved target endpoint.
type voiceLoopProvider struct {
	provider.Mock
	duplex       *provider.OpenAICompatibleClient
	speechFrames string
	speechBodies chan []byte

	llmErr        error  // when set, the LLM leg fails
	speechStatus  int    // when non-zero, the TTS leg answers this status with speechErrBody
	speechErrBody string // the error envelope the TTS leg returns under speechStatus
}

func (p voiceLoopProvider) CompleteStream(ctx context.Context, target routing.Target, req inference.Request, emit provider.StreamEmit) error {
	if p.llmErr != nil {
		return p.llmErr
	}
	return p.Mock.CompleteStream(ctx, target, req, emit)
}

func (p voiceLoopProvider) ProxyNative(ctx context.Context, target routing.Target, path string, body []byte, contentType string) (*provider.ProxyResponse, error) {
	if strings.Contains(path, "/audio/speech") {
		select {
		case p.speechBodies <- append([]byte(nil), body...):
		default:
		}
		if p.speechStatus != 0 {
			return &provider.ProxyResponse{
				StatusCode: p.speechStatus,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(p.speechErrBody)),
			}, nil
		}
		return &provider.ProxyResponse{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader(p.speechFrames)),
		}, nil
	}
	return p.Mock.ProxyNative(ctx, target, path, body, contentType)
}

func (p voiceLoopProvider) ProxyNativeDuplex(ctx context.Context, target routing.Target, path string, body io.Reader, contentType string) (*provider.ProxyResponse, error) {
	return p.duplex.ProxyNativeDuplex(ctx, target, path, body, contentType)
}

// voiceTurnHarness is the full three-leg loopback for a voice session behind one httptest
// server: a fake ASR child, the mock LLM and the canned TTS, with selfBaseURL pointed at the
// gateway itself so every leg is a real in-process HTTP call.
type voiceTurnHarness struct {
	srv          *Server
	url          string
	child        *fakeVoiceASRChild
	chatID       string
	speechBodies <-chan []byte
	returned     chan struct{}
}

// voiceTurnOpts tune one harness: the LLM pacing/failure, a TTS failure, and the timing.
type voiceTurnOpts struct {
	llmDelay      time.Duration
	llmErr        error
	speechStatus  int
	speechErrBody string
	timing        voiceTiming
}

func newVoiceTurnHarness(t *testing.T, child *fakeVoiceASRChild, opts voiceTurnOpts) *voiceTurnHarness {
	t.Helper()
	first, second := voiceTestPCM()
	prov := voiceLoopProvider{
		Mock:          provider.Mock{Delay: opts.llmDelay},
		duplex:        provider.NewOpenAICompatibleClient(http.DefaultClient),
		speechFrames:  speechFrameDelta(first) + speechFrameDelta(second) + speechSSEDone + speechSSEEnd,
		speechBodies:  make(chan []byte, 8),
		llmErr:        opts.llmErr,
		speechStatus:  opts.speechStatus,
		speechErrBody: opts.speechErrBody,
	}

	chats := store.NewMemoryChatStore(0)
	cipher, err := capture.New(testCaptureKey)
	if err != nil {
		t.Fatalf("capture.New: %v", err)
	}
	tokens := auth.NewTokenStore()
	tokens.AddPlainToken(auth.Token{ID: "tok_dev", UserID: "usr_dev", Name: "Dev Token", Active: true, Scopes: []string{"gateway:use"}}, "dev-secret")
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	directory := portal.NewMemoryDirectory(auth.NewTokenStore())
	directory.AddUser(store.User{ID: "usr_dev", Email: "dev@example.test", DisplayName: "Dev User", Role: "admin", Status: store.UserStatusActive, PreferredLanguage: "de", CreatedAt: now, UpdatedAt: now})
	recorder := usage.NewRecorder()
	routeStore := routing.NewMemoryStore()
	seedGatewayTestRoutes(routeStore, now) // qwen-coder (mock LLM) for the LLM leg

	ctx := context.Background()
	up, err := url.Parse(child.server.URL)
	if err != nil {
		t.Fatalf("parse child url: %v", err)
	}
	port, err := strconv.Atoi(up.Port())
	if err != nil {
		t.Fatalf("child port %q: %v", up.Port(), err)
	}
	// One audio.cpp server/app, endpoint = the fake child, carrying BOTH the TTS mapping
	// (served by the provider's ProxyNative) and the ASR mapping (reached by the duplex
	// relay at this endpoint).
	if err := routeStore.CreateAIServer(ctx, routing.AIServer{ID: "srv-audio", Name: "Audio Upstream", Domain: up.Hostname(), Provider: routing.ProviderAudioCpp, Endpoint: child.server.URL, Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateAIServer: %v", err)
	}
	if err := routeStore.CreateApplication(ctx, routing.Application{ID: "app-audio", ServerID: "srv-audio", Type: routing.ProviderAudioCpp, Port: port, Scheme: up.Scheme, APIFlavors: []string{routing.APIFlavorOpenAIAudio}, Priority: 10, Weight: 50, TimeoutMS: 30000, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateApplication: %v", err)
	}
	if err := routeStore.CreateMapping(ctx, routing.ModelMapping{ID: "route-tts", ApplicationID: "app-audio", GatewayModelName: speechGatewayModel, AppModelName: speechUpstreamModel, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateMapping tts: %v", err)
	}
	if err := routeStore.UpsertMappingCapabilities(ctx, "route-tts", []routing.CapabilityRow{{Capability: routing.CapabilitySpeech, Verdict: routing.CapabilityYes, Source: routing.CapabilitySourceAudioCppModels, CheckedAt: now}}); err != nil {
		t.Fatalf("UpsertMappingCapabilities tts: %v", err)
	}
	if err := routeStore.CreateMapping(ctx, routing.ModelMapping{ID: "route-asr", ApplicationID: "app-audio", GatewayModelName: "asr-model", AppModelName: "whisper_x", Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateMapping asr: %v", err)
	}
	if err := routeStore.UpsertMappingCapabilities(ctx, "route-asr", []routing.CapabilityRow{
		{Capability: routing.CapabilityTranscription, Verdict: routing.CapabilityYes, Source: routing.CapabilitySourceAudioCppModels, CheckedAt: now},
		{Capability: routing.CapabilityTranscriptionStreaming, Verdict: routing.CapabilityYes, Source: routing.CapabilitySourceAudioCppModels, CheckedAt: now},
	}); err != nil {
		t.Fatalf("UpsertMappingCapabilities asr: %v", err)
	}
	if err := routeStore.UpsertTelemetry(ctx, routing.ServerTelemetry{ServerID: "srv-audio", ReportedAt: now, LatencyMS: 100, ProviderHealth: `{}`, Capabilities: `{}`, RawSummary: `{}`, UpdatedAt: now}); err != nil {
		t.Fatalf("UpsertTelemetry: %v", err)
	}

	svc := portal.NewService(portal.ServiceDeps{
		Users: directory, Tokens: directory, Usage: recorder, Routes: routeStore,
		Clock: func() time.Time { return now }, ModelLister: provider.NewMock(),
		Chats: chats, Cipher: cipher,
	})
	srv := New(ServerDeps{
		Tokens:             tokens,
		Usage:              recorder,
		Provider:           prov,
		Routes:             routeStore,
		Portal:             svc,
		Captures:           &fakeCaptureStore{},
		Cipher:             cipher,
		InternalAuthSecret: "test-internal-secret",
		Users:              directory,
		ChatRuns:           NewChatRunRegistry(5),
	})
	srv.SetBaseContext(context.Background())
	srv.voiceTiming = opts.timing

	h := &voiceTurnHarness{srv: srv, child: child, speechBodies: prov.speechBodies, returned: make(chan struct{}, 16)}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		isWS := r.URL.Path == "/api/portal/voice-chat"
		srv.ServeHTTP(w, r)
		if isWS {
			select {
			case h.returned <- struct{}{}:
			default:
			}
		}
	}))
	t.Cleanup(ts.Close)
	srv.selfBaseURL = ts.URL
	h.url = ts.URL

	// The streaming gate the TTS leg checks: (speech_streaming, yes) + a cached rate.
	seedStreamableSpeech(t, srv, "route-tts", speechRunTestByteRate)

	owner := auth.Token{UserID: "usr_dev", Scopes: []string{"gateway:use"}}
	created, err := svc.CreateChat(ctx, owner, portal.CreateChatRequest{Content: json.RawMessage(`{"settings":{},"messages":[]}`)})
	if err != nil {
		t.Fatalf("CreateChat: %v", err)
	}
	h.chatID = created.ID
	return h
}

func (h *voiceTurnHarness) dial(t *testing.T, ctx context.Context) *websocket.Conn {
	t.Helper()
	c := voiceTicketClaims{
		ASR:          routing.Target{Model: "asr-model"},
		LLM:          routing.Target{Model: "qwen-coder"},
		TTS:          routing.Target{Model: speechGatewayModel},
		ChatID:       h.chatID,
		UserID:       "usr_dev",
		SampleRate:   16000,
		Channels:     1,
		SampleFormat: "s16le",
	}
	ticket := mustMintVoiceTicket(t, h.srv.voiceTickets, c)
	conn, _, err := websocket.Dial(ctx, wsURL(h.url, "/api/portal/voice-chat"), &websocket.DialOptions{
		Subprotocols: []string{"op-voice-chat", ticket},
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.CloseNow() })
	return conn
}

// turnGraceTiming is a short timing that still exercises the continuation grace (so the
// hold-audio-until-grace path runs) without slowing the suite.
func turnGraceTiming() voiceTiming {
	return voiceTiming{
		silenceTimeout:    10 * time.Second, // chunks end on push_end, deterministically
		minUtterance:      10 * time.Millisecond,
		maxUtterance:      10 * time.Second,
		micResumeTail:     20 * time.Millisecond,
		continuationGrace: 120 * time.Millisecond,
	}
}

// drainAChunk sends one PCM frame, waits for the ASR partial, then ends the chunk with
// push_end and waits for its asr_final, returning the final text.
func drainAChunk(t *testing.T, ctx context.Context, conn *websocket.Conn) string {
	t.Helper()
	sendVoicePCM(t, ctx, conn, 320)
	readVoiceUntil(t, ctx, conn, voiceDownASRPartial)
	sendVoiceControl(t, ctx, conn, "push_end")
	return readVoiceUntil(t, ctx, conn, voiceDownASRFinal).Text
}

// TestVoiceHappyPathRoundTrips: one spoken turn flows PCM -> asr_final -> answer_delta* ->
// answer_done -> audio_delta* -> audio_done -> back to listening, and the chat document ends
// with BOTH the user turn and the assistant turn committed.
func TestVoiceHappyPathRoundTrips(t *testing.T) {
	child := newFakeVoiceASRChild(t)
	child.finals = []string{"hello there"}
	h := newVoiceTurnHarness(t, child, voiceTurnOpts{timing: turnGraceTiming()})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	conn := h.dial(t, ctx)
	readVoiceUntilState(t, ctx, conn, voiceStateListening)

	if final := drainAChunk(t, ctx, conn); final != "hello there" {
		t.Fatalf("asr_final = %q, want %q", final, "hello there")
	}

	// The ordered frames of one answer: at least one answer_delta, then answer_done, then at
	// least one audio_delta, then audio_done. Read in order; a frame out of sequence fails.
	var sawDelta, sawAnswerDone, sawAudio bool
	lastAudioSeq := -1
	for {
		m, _ := readVoiceFrame(t, ctx, conn)
		switch m.Type {
		case voiceDownAnswerDelta:
			if sawAnswerDone {
				t.Fatal("answer_delta arrived after answer_done")
			}
			sawDelta = true
		case voiceDownAnswerDone:
			if !sawDelta {
				t.Fatal("answer_done arrived before any answer_delta")
			}
			sawAnswerDone = true
		case voiceDownAudioDelta:
			if !sawAnswerDone {
				t.Fatal("audio_delta arrived before answer_done: TTS must not play before the answer text is done")
			}
			if m.Seq != lastAudioSeq+1 {
				t.Fatalf("audio_delta seq = %d, want %d (contiguous from 0)", m.Seq, lastAudioSeq+1)
			}
			lastAudioSeq = m.Seq
			sawAudio = true
		case voiceDownAudioDone:
			if !sawAudio {
				t.Fatal("audio_done arrived before any audio_delta")
			}
			goto answered
		case voiceDownError:
			t.Fatalf("unexpected error frame: code=%q message=%q", m.Code, m.Message)
		}
	}
answered:
	readVoiceUntilState(t, ctx, conn, voiceStateListening)

	// The committed document: exactly [user, assistant], both complete.
	stored, err := h.srv.Portal.GetChat(context.Background(), auth.Token{UserID: "usr_dev"}, h.chatID)
	if err != nil {
		t.Fatal(err)
	}
	msgs := storedMessages(t, stored.Content)
	if len(msgs) != 2 || msgs[0].Role != "user" || msgs[1].Role != "assistant" {
		t.Fatalf("stored messages = %+v, want [user, assistant]: %s", msgs, stored.Content)
	}
	if !strings.Contains(string(msgs[0].Content), "hello there") {
		t.Fatalf("user turn = %s, want the pending text", msgs[0].Content)
	}
	if !strings.Contains(string(msgs[1].Content), "Mock response for qwen-coder: hello there") {
		t.Fatalf("assistant turn = %s, want the LLM answer", msgs[1].Content)
	}
}

// TestVoiceResumeCancelsAndExtends: resumed speech inside the continuation window cancels the
// speculative run (persisting NOTHING), emits answer_reset, extends pendingText, and the NEXT
// chunk end re-runs on the COMBINED text -- which is the turn that finally commits. The chat
// document ends with exactly ONE committed turn carrying the combined user text.
func TestVoiceResumeCancelsAndExtends(t *testing.T) {
	child := newFakeVoiceASRChild(t)
	child.finals = []string{"one ", "two"}
	// A long LLM delay keeps the first (speculative) run in flight so the resume lands while
	// it is still running and cancels it before it could commit.
	h := newVoiceTurnHarness(t, child, voiceTurnOpts{llmDelay: 3 * time.Second, timing: turnGraceTiming()})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	conn := h.dial(t, ctx)
	readVoiceUntilState(t, ctx, conn, voiceStateListening)

	// Chunk 1 -> speculative run #1 starts (thinking, mic still open).
	if final := drainAChunk(t, ctx, conn); final != "one " {
		t.Fatalf("chunk 1 final = %q, want %q", final, "one ")
	}
	readVoiceUntilState(t, ctx, conn, voiceStateThinking)

	// Resume: a fresh voiced chunk inside the window must cancel run #1 and reset the answer.
	sendVoicePCM(t, ctx, conn, 320)
	readVoiceUntil(t, ctx, conn, voiceDownAnswerReset)

	// The resumed chunk ends -> pendingText extends to "one two" -> run #2 on the combined
	// text, which runs to completion and commits.
	sendVoiceControl(t, ctx, conn, "push_end")
	if final := readVoiceUntil(t, ctx, conn, voiceDownASRFinal).Text; final != "two" {
		t.Fatalf("chunk 2 final = %q, want %q", final, "two")
	}
	readVoiceUntil(t, ctx, conn, voiceDownAudioDone)
	readVoiceUntilState(t, ctx, conn, voiceStateListening)

	// Exactly ONE committed turn, carrying the COMBINED user text -- run #1 persisted nothing.
	stored, err := h.srv.Portal.GetChat(context.Background(), auth.Token{UserID: "usr_dev"}, h.chatID)
	if err != nil {
		t.Fatal(err)
	}
	msgs := storedMessages(t, stored.Content)
	if len(msgs) != 2 {
		t.Fatalf("stored %d messages, want exactly one committed turn [user, assistant]: %s", len(msgs), stored.Content)
	}
	if !strings.Contains(string(msgs[0].Content), "one two") {
		t.Fatalf("user turn = %s, want the combined text %q", msgs[0].Content, "one two")
	}
	if !strings.Contains(string(msgs[1].Content), "Mock response for qwen-coder: one two") {
		t.Fatalf("assistant turn = %s, want the answer to the combined text", msgs[1].Content)
	}
}

// TestVoiceCommitsAfterGraceNoResume: with a real continuation grace and no resume, the turn
// still commits once the answer has streamed and the grace has elapsed, and the committed
// turn's assistant message carries the synthesized audio as a stored blob (not inline).
func TestVoiceCommitsAfterGraceNoResume(t *testing.T) {
	child := newFakeVoiceASRChild(t)
	child.finals = []string{"commit me"}
	timing := turnGraceTiming()
	timing.continuationGrace = 250 * time.Millisecond // a grace worth waiting out
	h := newVoiceTurnHarness(t, child, voiceTurnOpts{timing: timing})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	conn := h.dial(t, ctx)
	readVoiceUntilState(t, ctx, conn, voiceStateListening)

	start := time.Now()
	if final := drainAChunk(t, ctx, conn); final != "commit me" {
		t.Fatalf("asr_final = %q, want %q", final, "commit me")
	}
	// No resume: the window closes on (answer streamed AND grace). The first audio_delta can
	// only be delivered once the mic has shut (speaking), which is after the grace.
	readVoiceUntil(t, ctx, conn, voiceDownAudioDelta)
	if waited := time.Since(start); waited < 200*time.Millisecond {
		t.Fatalf("audio played %v after the chunk, want it held at least the ~250ms grace", waited)
	}
	readVoiceUntil(t, ctx, conn, voiceDownAudioDone)
	readVoiceUntilState(t, ctx, conn, voiceStateListening)

	stored, err := h.srv.Portal.GetChat(context.Background(), auth.Token{UserID: "usr_dev"}, h.chatID)
	if err != nil {
		t.Fatal(err)
	}
	msgs := storedMessages(t, stored.Content)
	if len(msgs) != 2 {
		t.Fatalf("stored %d messages, want [user, assistant]: %s", len(msgs), stored.Content)
	}
	if !strings.Contains(string(stored.Content), `"type":"audio_url"`) {
		t.Fatalf("the assistant turn carries no stored audio: %s", stored.Content)
	}
	if strings.Contains(string(stored.Content), "data:") {
		t.Fatalf("the audio was committed inline, not as a blob: %.300s", stored.Content)
	}
}

// degradeAssert runs one turn against a harness whose leg fails and asserts the single
// error frame carries wantCode (when non-empty) and that the session degrades to idle with
// no retry (no answer_delta follows the error).
func degradeAssert(t *testing.T, h *voiceTurnHarness, wantCode string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	conn := h.dial(t, ctx)
	readVoiceUntilState(t, ctx, conn, voiceStateListening)

	if final := drainAChunk(t, ctx, conn); final == "" {
		t.Fatal("asr_final was empty")
	}

	// The leg fails -> exactly one error frame -> idle. Read until the error frame.
	var errFrame voiceDownMsg
	for {
		m, _ := readVoiceFrame(t, ctx, conn)
		if m.Type == voiceDownError {
			errFrame = m
			break
		}
	}
	if errFrame.Code == "" {
		t.Fatal("the error frame carries no code")
	}
	if wantCode != "" && errFrame.Code != wantCode {
		t.Fatalf("error code = %q, want %q", errFrame.Code, wantCode)
	}
	readVoiceUntilState(t, ctx, conn, voiceStateIdle)

	// No retry: no further answer_delta / audio within a short window after the error.
	rctx, rcancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer rcancel()
	for {
		typ, data, err := conn.Read(rctx)
		if err != nil {
			break // the window elapsed with no retry
		}
		if typ != websocket.MessageText {
			continue
		}
		var m voiceDownMsg
		if json.Unmarshal(data, &m) == nil && (m.Type == voiceDownAnswerDelta || m.Type == voiceDownAudioDelta) {
			t.Fatalf("a %s frame arrived after the error: the turn was silently retried", m.Type)
		}
	}

	// Nothing committed: the failed turn leaves the document empty.
	stored, err := h.srv.Portal.GetChat(context.Background(), auth.Token{UserID: "usr_dev"}, h.chatID)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(storedMessages(t, stored.Content)); n != 0 {
		t.Fatalf("a degraded turn committed %d messages, want none: %s", n, stored.Content)
	}
}

// TestVoiceLLMErrorDegradesToIdle: an LLM failure stops the turn with one error frame and a
// fall back to idle, with no retry.
func TestVoiceLLMErrorDegradesToIdle(t *testing.T) {
	child := newFakeVoiceASRChild(t)
	child.finals = []string{"ask the model"}
	// provider.ErrUnavailable is mapped to the stable completion code provider.unavailable,
	// which the degradation error frame must carry (spec 6.6).
	h := newVoiceTurnHarness(t, child, voiceTurnOpts{llmErr: provider.ErrUnavailable, timing: turnGraceTiming()})
	degradeAssert(t, h, "provider.unavailable")
}

// TestVoiceTTSErrorDegradesToIdle: a TTS failure (after a clean LLM answer) stops the turn
// with one error frame carrying the TTS leg's stable code, and degrades to idle.
func TestVoiceTTSErrorDegradesToIdle(t *testing.T) {
	child := newFakeVoiceASRChild(t)
	child.finals = []string{"speak this"}
	h := newVoiceTurnHarness(t, child, voiceTurnOpts{
		speechStatus:  http.StatusInternalServerError,
		speechErrBody: `{"error":{"message":"tts upstream failed","code":"speech.generation_failed","type":"server_error"}}`,
		timing:        turnGraceTiming(),
	})
	degradeAssert(t, h, "speech.generation_failed")
}

// TestVoiceMidTurnAdmissionBlockedDegrades: a mid-turn runtime.admission_blocked on a leg
// (here the TTS leg) stops the turn with that exact stable code and degrades to idle.
func TestVoiceMidTurnAdmissionBlockedDegrades(t *testing.T) {
	child := newFakeVoiceASRChild(t)
	child.finals = []string{"no room at the inn"}
	h := newVoiceTurnHarness(t, child, voiceTurnOpts{
		speechStatus:  http.StatusServiceUnavailable,
		speechErrBody: `{"error":{"message":"no capacity","code":"runtime.admission_blocked","type":"server_error"}}`,
		timing:        turnGraceTiming(),
	})
	degradeAssert(t, h, voiceAdmissionBlockedCode)
}
