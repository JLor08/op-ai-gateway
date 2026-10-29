// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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

	// speechChunkFrames / speechTrailer / speechGate drive a GATED speech body: when speechGate is
	// non-nil the TTS leg streams the FIRST frame immediately and holds the rest until the gate
	// closes, so a test can relay a second audio chunk at a controlled time (e.g. after a
	// mid-stream playback_done). With no gate the body is the whole speechFrames string at once.
	speechChunkFrames []string
	speechTrailer     string
	speechGate        <-chan struct{}

	llmErr        error  // when set, the LLM leg fails
	speechStatus  int    // when non-zero, the TTS leg answers this status with speechErrBody
	speechErrBody string // the error envelope the TTS leg returns under speechStatus
}

// gatedSpeechBody streams a speech event body with the first frame sent immediately and the rest
// held until gate closes. Writes go through an io.Pipe (synchronous), so each frame is produced
// only once the reader has consumed the previous one -- a deterministic inter-chunk gap for a test.
func gatedSpeechBody(frames []string, trailer string, gate <-chan struct{}) io.ReadCloser {
	pr, pw := io.Pipe()
	go func() {
		if len(frames) > 0 {
			_, _ = io.WriteString(pw, frames[0])
		}
		<-gate
		for _, f := range frames[1:] {
			_, _ = io.WriteString(pw, f)
		}
		_, _ = io.WriteString(pw, trailer)
		_ = pw.Close()
	}()
	return pr
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
		body := io.NopCloser(strings.NewReader(p.speechFrames))
		if p.speechGate != nil {
			body = gatedSpeechBody(p.speechChunkFrames, p.speechTrailer, p.speechGate)
		}
		return &provider.ProxyResponse{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       body,
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

// voiceTurnOpts tune one harness: the LLM pacing/failure, a TTS failure, the timing, an
// optional chat store seam (to drive a resume during the commit write), and the TTS PCM the
// canned speech stream carries (so a drain test can size the relayed audio's real-time
// duration, which bounds the playout fallback).
type voiceTurnOpts struct {
	llmDelay      time.Duration
	llmErr        error
	speechStatus  int
	speechErrBody string
	timing        voiceTiming
	// streamIdle is the gateway edge's SSE idle watchdog (ServerDeps.StreamIdleTimeout, 120s in
	// production): it cuts a loopback whose upstream sends no SSE for this long. Zero (the default
	// here) leaves it off, as every pre-#335 voice test ran.
	streamIdle time.Duration
	chatStore  portal.ChatStore // defaults to a plain in-memory store
	speechPCM  [][]byte         // the PCM chunks the TTS stream carries; defaults to voiceTestPCM
	// speechGate, when non-nil, holds every speech frame AFTER the first until the channel closes,
	// so a test can relay a second audio chunk at a controlled time. Needs speechPCM with >= 2 chunks.
	speechGate <-chan struct{}
}

func newVoiceTurnHarness(t *testing.T, child *fakeVoiceASRChild, opts voiceTurnOpts) *voiceTurnHarness {
	t.Helper()
	first, second := voiceTestPCM()
	chunks := [][]byte{first, second}
	if len(opts.speechPCM) > 0 {
		chunks = opts.speechPCM
	}
	frameList := make([]string, 0, len(chunks))
	for _, c := range chunks {
		frameList = append(frameList, speechFrameDelta(c))
	}
	trailer := speechSSEDone + speechSSEEnd
	frames := strings.Join(frameList, "") + trailer
	prov := voiceLoopProvider{
		Mock:              provider.Mock{Delay: opts.llmDelay},
		duplex:            provider.NewOpenAICompatibleClient(http.DefaultClient),
		speechFrames:      frames,
		speechChunkFrames: frameList,
		speechTrailer:     trailer,
		speechGate:        opts.speechGate,
		speechBodies:      make(chan []byte, 8),
		llmErr:            opts.llmErr,
		speechStatus:      opts.speechStatus,
		speechErrBody:     opts.speechErrBody,
	}

	var chats portal.ChatStore = store.NewMemoryChatStore(0)
	if opts.chatStore != nil {
		chats = opts.chatStore
	}
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
	srv.streamIdleTimeout = opts.streamIdle

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

// finishSpokenTurn mimics the browser once an answer's audio is all relayed: it reads through
// audio_done, reports the player drained ({"type":"playback_done"} -- the #304 signal), and
// waits for the session to return to listening after the half-duplex drain ends. Without the
// signal the drain would hold until its absolute ceiling (VoicePlaybackFallbackMs), so a turn
// test that expects to reach listening sends it, exactly as the real client does.
func finishSpokenTurn(t *testing.T, ctx context.Context, conn *websocket.Conn) {
	t.Helper()
	readVoiceUntil(t, ctx, conn, voiceDownAudioDone)
	sendVoiceControl(t, ctx, conn, "playback_done")
	readVoiceUntilState(t, ctx, conn, voiceStateListening)
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
	// The player drained -> the drain ends and the session returns to listening.
	sendVoiceControl(t, ctx, conn, "playback_done")
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
	finishSpokenTurn(t, ctx, conn)

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
// runs the full round trip, commits, and the committed assistant message carries the
// synthesized audio as a stored blob (not inline). Audio is only ever delivered after the
// mic has shut (the ordered-frame assertion lives in the happy-path test); here the focus is
// that no resume -> exactly one committed turn with stored audio.
func TestVoiceCommitsAfterGraceNoResume(t *testing.T) {
	child := newFakeVoiceASRChild(t)
	child.finals = []string{"commit me"}
	timing := turnGraceTiming()
	timing.continuationGrace = 250 * time.Millisecond // a real grace the window logic runs through
	h := newVoiceTurnHarness(t, child, voiceTurnOpts{timing: timing})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	conn := h.dial(t, ctx)
	readVoiceUntilState(t, ctx, conn, voiceStateListening)

	if final := drainAChunk(t, ctx, conn); final != "commit me" {
		t.Fatalf("asr_final = %q, want %q", final, "commit me")
	}
	// No resume: the turn walks answer -> speaking -> audio -> commit.
	finishSpokenTurn(t, ctx, conn)

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

// TestVoiceFastCommitThenResumeDoesNotDoubleCommit is the fast-commit edge: when the whole
// run (LLM + TTS + commit) finishes WITHIN the continuation grace, the committed turn is the
// window's point of no return -- the window is forced closed at commit (the session returns
// to listening FAST, not after the full grace), so a resume afterwards is a NEW turn, never a
// cancel-and-extend of the already-committed one. Regression for the double-commit where a
// resume in the post-commit/pre-grace sub-window re-ran on the extended text.
func TestVoiceFastCommitThenResumeDoesNotDoubleCommit(t *testing.T) {
	child := newFakeVoiceASRChild(t)
	child.finals = []string{"one", "two"}
	timing := turnGraceTiming()
	timing.continuationGrace = time.Second // long grace; the fast run commits well within it
	h := newVoiceTurnHarness(t, child, voiceTurnOpts{timing: timing})
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()

	conn := h.dial(t, ctx)
	readVoiceUntilState(t, ctx, conn, voiceStateListening)

	// Turn 1 commits within the 1s grace. Because the run has persisted, the window is forced
	// closed AT COMMIT: the session must return to listening well before the grace would fire.
	start := time.Now()
	if f := drainAChunk(t, ctx, conn); f != "one" {
		t.Fatalf("turn 1 final = %q, want %q", f, "one")
	}
	finishSpokenTurn(t, ctx, conn)
	if waited := time.Since(start); waited >= 900*time.Millisecond {
		t.Fatalf("turn 1 returned to listening after %v, want it closed at commit (<<1s grace): the window did not close on commit", waited)
	}

	// The user resumes AFTER commit: it must be a NEW turn, not an extension of utterance 1.
	if f := drainAChunk(t, ctx, conn); f != "two" {
		t.Fatalf("turn 2 final = %q, want %q", f, "two")
	}
	finishSpokenTurn(t, ctx, conn)

	stored, err := h.srv.Portal.GetChat(context.Background(), auth.Token{UserID: "usr_dev"}, h.chatID)
	if err != nil {
		t.Fatal(err)
	}
	msgs := storedMessages(t, stored.Content)
	if len(msgs) != 4 {
		t.Fatalf("want two DISTINCT committed turns (4 messages), got %d: %s", len(msgs), stored.Content)
	}
	for _, m := range msgs {
		if m.Role == "user" && strings.Contains(string(m.Content), "onetwo") {
			t.Fatalf("a user turn carries the EXTENDED text \"onetwo\": utterance 1 was double-committed: %s", stored.Content)
		}
	}
	if !strings.Contains(string(msgs[0].Content), "one") || strings.Contains(string(msgs[0].Content), "two") {
		t.Fatalf("turn 1 user = %s, want exactly \"one\"", msgs[0].Content)
	}
	if !strings.Contains(string(msgs[2].Content), "two") {
		t.Fatalf("turn 2 user = %s, want the resume \"two\" as a distinct new turn", msgs[2].Content)
	}
}

// degradeAssert runs one turn against a harness whose leg fails and asserts the single
// error frame carries wantCode (when non-empty) and that the session degrades to idle with
// no retry (no answer_delta follows the error).
// degradeAssert drives a turn whose leg fails, and asserts the degrade-to-idle flow: one
// typed error frame carrying wantCode, a fall back to idle, and no silent retry.
//
// wantTextCommitted splits the two phase-2 persistence contracts (spec 5.5):
//   - false: the LLM failed BEFORE any <speak> summary, so no speech fired and the turn
//     persists NOTHING -- the chat document is left empty.
//   - true: the SPEECH leg failed (after a clean LLM answer), so the text turn STILL commits
//     with no audio block and no spoken_sections, and only the degrade is surfaced.
func degradeAssert(t *testing.T, h *voiceTurnHarness, wantCode string, wantTextCommitted bool) {
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

	stored, err := h.srv.Portal.GetChat(context.Background(), auth.Token{UserID: "usr_dev"}, h.chatID)
	if err != nil {
		t.Fatal(err)
	}
	msgs := storedMessages(t, stored.Content)
	if !wantTextCommitted {
		// The LLM failed before any summary: nothing fired, nothing persists.
		if n := len(msgs); n != 0 {
			t.Fatalf("a degraded turn committed %d messages, want none: %s", n, stored.Content)
		}
		return
	}
	// The speech leg failed: the text turn still commits (spec 5.5), with NO audio and NO
	// spoken_sections -- nothing was spoken -- and the degrade is surfaced separately above.
	if n := len(msgs); n != 2 {
		t.Fatalf("a speech-failed turn committed %d messages, want [user, assistant]: %s", n, stored.Content)
	}
	if strings.Contains(string(stored.Content), `"type":"audio_url"`) || strings.Contains(string(stored.Content), "data:") {
		t.Fatalf("a speech-failed turn must commit no audio: %s", stored.Content)
	}
	if _, ok := storedSpokenSummary(t, stored.Content); ok {
		t.Fatalf("a speech-failed turn must persist no spoken_sections: %s", stored.Content)
	}
}

// slowCommitChatStore is a chat store that blocks the FIRST SaveChatWithBlobs (a voice turn's
// deferred commit) so a test can act -- here, resume -- while the run is inside its commit
// write: the commit is claimed, the context is WithoutCancel, but status is still "running"
// and the window still open. It signals commitStarted, then waits for release.
type slowCommitChatStore struct {
	*store.MemoryChatStore
	once          sync.Once
	commitStarted chan struct{}
	release       chan struct{}
}

func newSlowCommitChatStore() *slowCommitChatStore {
	return &slowCommitChatStore{
		MemoryChatStore: store.NewMemoryChatStore(0),
		commitStarted:   make(chan struct{}),
		release:         make(chan struct{}),
	}
}

func (s *slowCommitChatStore) SaveChatWithBlobs(ctx context.Context, chat store.Chat, blobs []store.ChatBlob, del []string) error {
	s.once.Do(func() {
		close(s.commitStarted)
		<-s.release
	})
	return s.MemoryChatStore.SaveChatWithBlobs(ctx, chat, blobs, del)
}

// TestVoiceResumeDuringCommitWriteDoesNotExtend is the residual double-commit regression: a
// resume that lands WHILE the run is inside its commit write (status still "running", window
// still open, but past the point of no return) must NOT cancel-and-extend -- no answer_reset,
// and exactly ONE committed turn carrying the first utterance alone. The commit claim (the
// run's commit/cancel word), not the run status, is what closes this.
func TestVoiceResumeDuringCommitWriteDoesNotExtend(t *testing.T) {
	child := newFakeVoiceASRChild(t)
	child.finals = []string{"one", "two"}
	slow := newSlowCommitChatStore()
	timing := turnGraceTiming()
	timing.continuationGrace = 5 * time.Second // keep the window open across the blocked write
	h := newVoiceTurnHarness(t, child, voiceTurnOpts{timing: timing, chatStore: slow})
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()

	conn := h.dial(t, ctx)
	readVoiceUntilState(t, ctx, conn, voiceStateListening)

	// Turn 1: the run reaches commit and BLOCKS in SaveChatWithBlobs. The commit is claimed.
	if f := drainAChunk(t, ctx, conn); f != "one" {
		t.Fatalf("turn 1 final = %q, want %q", f, "one")
	}
	select {
	case <-slow.commitStarted:
	case <-time.After(10 * time.Second):
		t.Fatal("the commit write never started")
	}

	// Resume DURING the commit write: must NOT be treated as cancel-and-extend (no reset). The
	// resumed chunk's own partial confirms it was captured but left as an in-progress chunk.
	sendVoicePCM(t, ctx, conn, 320)
	for {
		m, _ := readVoiceFrame(t, ctx, conn)
		if m.Type == voiceDownAnswerReset {
			t.Fatal("answer_reset during the commit write: a committing run was cancel-and-extended (double-commit)")
		}
		if m.Type == voiceDownASRPartial {
			break
		}
	}

	// Release the commit; the run finishes and the force-close discards the resumed chunk.
	close(slow.release)
	finishSpokenTurn(t, ctx, conn)

	stored, err := h.srv.Portal.GetChat(context.Background(), auth.Token{UserID: "usr_dev"}, h.chatID)
	if err != nil {
		t.Fatal(err)
	}
	msgs := storedMessages(t, stored.Content)
	if len(msgs) != 2 {
		t.Fatalf("want exactly ONE committed turn [user, assistant], got %d: %s", len(msgs), stored.Content)
	}
	if strings.Contains(string(msgs[0].Content), "two") {
		t.Fatalf("turn 1 user = %s, want exactly \"one\" (not extended by the resume): %s", msgs[0].Content, stored.Content)
	}
}

// TestVoiceRunResumableGuard pins the guard that closes the residual at the unit level: a run
// that is committing (past its point of no return) is NOT resumable even though its status is
// still "running" for the whole WithoutCancel commit write.
func TestVoiceRunResumableGuard(t *testing.T) {
	run := newChatRun("run_x", "chat_x", "usr_x", func() {})
	m := &voiceMachine{curRun: run}
	if !m.runResumable() {
		t.Fatal("a running run in an open window must be resumable")
	}
	if !run.claimCommit() {
		t.Fatal("claimCommit on an undecided run must win")
	}
	if run.statusValue() != "running" {
		t.Fatalf("precondition: status = %q, want still \"running\" while committing", run.statusValue())
	}
	if m.runResumable() {
		t.Fatal("a committing run must NOT be resumable: a resume would double-commit")
	}

	// A completed run, a closed window, and no run are all non-resumable too.
	done := newChatRun("run_done", "chat_x", "usr_x", func() {})
	done.finish("completed", "", nil)
	if (&voiceMachine{curRun: done}).runResumable() {
		t.Fatal("a completed run must not be resumable")
	}
	if (&voiceMachine{curRun: newChatRun("r", "c", "u", func() {}), windowClosed: true}).runResumable() {
		t.Fatal("a closed window must not be resumable")
	}
	if (&voiceMachine{}).runResumable() {
		t.Fatal("no run must not be resumable")
	}
}

// TestVoiceRunCommitStateTransitions pins the commit/cancel word's state machine: the word
// leaves "running" exactly once, so the run's commit claim and a cancel claim are mutually
// exclusive in BOTH orders, and a second claim of the same kind never wins (no double-commit).
func TestVoiceRunCommitStateTransitions(t *testing.T) {
	t.Run("commit first: the cancel loses", func(t *testing.T) {
		run := newChatRun("run_a", "chat_a", "usr_a", func() {})
		if !run.commitUndecided() || run.isCommitting() {
			t.Fatal("a fresh run must be undecided and not committing")
		}
		if !run.claimCommit() {
			t.Fatal("running->committing must succeed on an undecided run")
		}
		if run.claimCancel() {
			t.Fatal("a cancel must NOT win once the run has claimed its commit")
		}
		if run.claimCommit() {
			t.Fatal("a second commit claim must not win (double-commit)")
		}
		if !run.isCommitting() || run.commitUndecided() {
			t.Fatal("the run must stay committing after the lost cancel")
		}
	})
	t.Run("cancel first: the commit loses", func(t *testing.T) {
		run := newChatRun("run_b", "chat_b", "usr_b", func() {})
		if !run.claimCancel() {
			t.Fatal("running->canceled must succeed on an undecided run")
		}
		if run.claimCommit() {
			t.Fatal("the commit must NOT be claimable once a cancel claimed the run: it would persist a cancelled run")
		}
		if run.claimCancel() {
			t.Fatal("a second cancel claim must not win")
		}
		if run.isCommitting() || run.commitUndecided() {
			t.Fatal("a canceled run is neither committing nor undecided")
		}
	})
	t.Run("cancelContext claims the cancel before cancelling", func(t *testing.T) {
		var run *ChatRun
		var sawClaimed bool
		run = newChatRun("run_c", "chat_c", "usr_c", func() {
			// By the time the context is cancelled the word must already be decided, so a run
			// that observes its cancelled context can never win the commit claim afterwards.
			sawClaimed = !run.commitUndecided()
		})
		run.cancelContext()
		if !sawClaimed {
			t.Fatal("cancelContext cancelled the context before claiming the cancel")
		}
		if run.claimCommit() {
			t.Fatal("a run cancelled through cancelContext must not be able to claim its commit")
		}
	})
	t.Run("cancelContext leaves a committing run committing", func(t *testing.T) {
		cancelled := false
		run := newChatRun("run_d", "chat_d", "usr_d", func() { cancelled = true })
		if !run.claimCommit() {
			t.Fatal("claimCommit on an undecided run must win")
		}
		run.cancelContext()
		if !run.isCommitting() {
			t.Fatal("a cancel after the commit claim must not flip the word away from committing")
		}
		if !cancelled {
			t.Fatal("the context cancel still fires (the commit write is under WithoutCancel)")
		}
	})
}

// TestVoiceTryResumeArbitratesWithCommit is the orchestrator-side arbitration: a resume is
// accepted only by winning the cancel claim, so it can never be accepted for a run that has
// claimed its commit -- and, once accepted, the run can never claim its commit.
func TestVoiceTryResumeArbitratesWithCommit(t *testing.T) {
	t.Run("resume wins: the run then cannot commit", func(t *testing.T) {
		run := newChatRun("run_e", "chat_e", "usr_e", func() {})
		m := &voiceMachine{curRun: run}
		if !m.tryResume() {
			t.Fatal("a resume of an undecided run in an open window must be accepted")
		}
		if run.claimCommit() {
			t.Fatal("after an accepted resume the run must NOT be able to claim its commit (it would double-commit)")
		}
	})
	t.Run("commit wins: the resume is rejected", func(t *testing.T) {
		run := newChatRun("run_f", "chat_f", "usr_f", func() {})
		m := &voiceMachine{curRun: run}
		if !run.claimCommit() {
			t.Fatal("claimCommit on an undecided run must win")
		}
		if m.tryResume() {
			t.Fatal("a resume of a committing run must be rejected: it is a new turn, never a cancel-and-extend")
		}
	})
	t.Run("a pre-check pass cannot be turned into a win by the claim alone", func(t *testing.T) {
		// runResumable is only a pre-check: when the run commits between it and the claim, the
		// CAS -- not the stale read -- rejects the resume.
		run := newChatRun("run_g", "chat_g", "usr_g", func() {})
		m := &voiceMachine{curRun: run}
		if !m.runResumable() {
			t.Fatal("precondition: the run is resumable by the pre-check")
		}
		if !run.claimCommit() { // the run commits in the gap after the pre-check
			t.Fatal("claimCommit on an undecided run must win")
		}
		if run.claimCancel() {
			t.Fatal("the claim must fail for a run that committed after the pre-check")
		}
	})
}

// TestVoiceCommitRacingCancelHasExactlyOneWinner hammers the arbitration under -race: the
// run's commit claim and the orchestrator's resume claim run concurrently, and in every
// iteration EXACTLY ONE wins and the word agrees with the winner -- never both (a double
// commit / a persisted cancelled run) and never neither (a run that neither commits nor yields).
// The scheduler decides who wins each time; the assertion holds for either order.
func TestVoiceCommitRacingCancelHasExactlyOneWinner(t *testing.T) {
	commits, resumes := 0, 0
	for i := 0; i < 5000; i++ {
		run := newChatRun("run_race", "chat_race", "usr_race", func() {})
		m := &voiceMachine{curRun: run}
		var commitWon, resumeWon bool
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); <-start; commitWon = run.claimCommit() }()
		go func() { defer wg.Done(); <-start; resumeWon = m.tryResume() }()
		close(start)
		wg.Wait()
		if commitWon == resumeWon {
			t.Fatalf("iteration %d: commitWon=%v resumeWon=%v, want exactly one winner", i, commitWon, resumeWon)
		}
		if commitWon != run.isCommitting() {
			t.Fatalf("iteration %d: commitWon=%v but isCommitting=%v: the word disagrees with the winner", i, commitWon, run.isCommitting())
		}
		if commitWon {
			commits++
		} else {
			resumes++
		}
	}
	t.Logf("5000 races: run committed %d, resume cancelled %d", commits, resumes)
}

// TestVoiceLLMErrorDegradesToIdle: an LLM failure stops the turn with one error frame and a
// fall back to idle, with no retry.
func TestVoiceLLMErrorDegradesToIdle(t *testing.T) {
	child := newFakeVoiceASRChild(t)
	child.finals = []string{"ask the model"}
	// provider.ErrUnavailable is mapped to the stable completion code provider.unavailable,
	// which the degradation error frame must carry (spec 6.6).
	h := newVoiceTurnHarness(t, child, voiceTurnOpts{llmErr: provider.ErrUnavailable, timing: turnGraceTiming()})
	// The LLM failed before any <speak> summary: nothing fired, nothing persists.
	degradeAssert(t, h, "provider.unavailable", false)
}

// TestVoiceTTSErrorDegradesToIdle: a TTS failure (after a clean LLM answer) stops the turn
// with one error frame carrying the speech leg's own stable code (speech.upstream_error, the
// code the gateway wraps a plain-string upstream error with), and degrades to idle. Under
// phase 2 the text turn STILL commits (no audio); only the degrade is surfaced (spec 5.5).
func TestVoiceTTSErrorDegradesToIdle(t *testing.T) {
	child := newFakeVoiceASRChild(t)
	child.finals = []string{"speak this"}
	h := newVoiceTurnHarness(t, child, voiceTurnOpts{
		speechStatus:  http.StatusInternalServerError,
		speechErrBody: `{"error":"the speech upstream failed"}`,
		timing:        turnGraceTiming(),
	})
	degradeAssert(t, h, "speech.upstream_error", true)
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
	// A speech-leg admission block is a speech-leg failure: the text turn commits, degrade surfaced.
	degradeAssert(t, h, voiceAdmissionBlockedCode, true)
}

// drainBurstDiscarded sends a mic PCM burst and asserts, after a short settle, that NONE of it
// reached the ASR child -- the half-duplex gate is still holding (the session is still
// draining). before is the child snapshot taken when draining began.
func drainBurstDiscarded(t *testing.T, ctx context.Context, conn *websocket.Conn, child *fakeVoiceASRChild, beforeConns int, beforeBytes int64, why string) {
	t.Helper()
	for range 5 {
		sendVoicePCM(t, ctx, conn, 3200)
	}
	time.Sleep(200 * time.Millisecond)
	if conns, bytes := child.snapshot(); conns != beforeConns || bytes != beforeBytes {
		t.Fatalf("%s (conns %d->%d, bytes %d->%d)", why, beforeConns, conns, beforeBytes, bytes)
	}
}

// TestVoiceDrainWaitsForPlaybackDone is the playout-aware half-duplex drain (spec 6.2) and its
// anti-goal (spec 2): the orchestrator relays TTS faster than real time, so the server holds the
// mic gated until TRUE playout-done, which it computes as max(FLOOR, playback_done) -- the FLOOR
// (the relayed audio's own real-time duration) is the server-authoritative minimum. An EARLY
// {"type":"playback_done"} (a buggy or malicious client that claims playout finished at once)
// must NOT shorten the drain below the floor: the mic stays gated, and PCM sent before the floor
// is still DISCARDED, so the model can never transcribe its own TTS output.
func TestVoiceDrainWaitsForPlaybackDone(t *testing.T) {
	child := newFakeVoiceASRChild(t)
	child.finals = []string{"hold the mic"}
	timing := turnGraceTiming()
	timing.micResumeTail = 30 * time.Millisecond
	// ~2s of audio at the fixture byte rate (88200 B/s) => a ~2s floor to hold the mic past.
	h := newVoiceTurnHarness(t, child, voiceTurnOpts{
		timing:    timing,
		speechPCM: [][]byte{speechPCM(88200*2, 1)},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()

	conn := h.dial(t, ctx)
	conn.SetReadLimit(1 << 21) // the relayed audio_delta is large; read it whole
	readVoiceUntilState(t, ctx, conn, voiceStateListening)

	if f := drainAChunk(t, ctx, conn); f != "hold the mic" {
		t.Fatalf("asr_final = %q, want %q", f, "hold the mic")
	}

	// Relay-done (audio_done) is NOT playout-done: the mic must stay gated in draining.
	readVoiceUntil(t, ctx, conn, voiceDownAudioDone)
	readVoiceUntilState(t, ctx, conn, voiceStateDraining)
	tDraining := time.Now()
	beforeConns, beforeBytes := child.snapshot()
	if beforeBytes != 320 {
		t.Fatalf("the one real chunk delivered %d bytes, want 320 (harness sanity)", beforeBytes)
	}

	// A buggy/early playback_done at the very start of the drain must NOT end it below the floor.
	sendVoiceControl(t, ctx, conn, "playback_done")

	// Well after the early signal but before the ~2s floor, the gate still holds: mic PCM is
	// still discarded despite the client's (false) claim that playout is done.
	time.Sleep(600 * time.Millisecond)
	drainBurstDiscarded(t, ctx, conn, child, beforeConns, beforeBytes,
		"an early playback_done re-opened the mic before the floor: the server bound is not a floor")

	// The drain ends only at ~floor+tail, not at the early signal.
	readVoiceUntilState(t, ctx, conn, voiceStateListening)
	if waited := time.Since(tDraining); waited < time.Second {
		t.Fatalf("returned to listening %v after draining began, want it held to the ~2s floor despite the early playback_done", waited)
	}
}

// TestVoiceDrainCounterBearingEarlyPlaybackDoneIsFloorClamped: the playout counter (#322) can only
// make a signal HONORED, never shorten the drain. A playback_done that is VALID -- its played_through
// accounts for all the audio relayed (or claims more) -- but EARLY (sent at the very start of the
// drain, ~2s before the floor) is remembered and still clamped up to the floor exactly like a
// counter-less one: the mic stays gated, PCM sent before the floor is DISCARDED, and the session
// returns to listening only at ~floor+tail (not at once, and not at the 30s ceiling either).
func TestVoiceDrainCounterBearingEarlyPlaybackDoneIsFloorClamped(t *testing.T) {
	for name, extra := range map[string]int{
		"counter equals the relayed audio":     0,
		"counter claims more than was relayed": 98, // a forged-high counter is valid too, and no less clamped
	} {
		t.Run(name, func(t *testing.T) {
			child := newFakeVoiceASRChild(t)
			child.finals = []string{"early but valid"}
			timing := turnGraceTiming()
			timing.micResumeTail = 30 * time.Millisecond
			// ~2s of audio at the fixture byte rate (88200 B/s) => a ~2s floor to hold the mic past.
			h := newVoiceTurnHarness(t, child, voiceTurnOpts{
				timing:    timing,
				speechPCM: [][]byte{speechPCM(88200*2, 1)},
			})
			ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
			defer cancel()

			conn := h.dial(t, ctx)
			conn.SetReadLimit(1 << 21) // the relayed audio_delta is large; read it whole
			readVoiceUntilState(t, ctx, conn, voiceStateListening)

			if f := drainAChunk(t, ctx, conn); f != "early but valid" {
				t.Fatalf("asr_final = %q, want %q", f, "early but valid")
			}
			// Count the audio chunks the server relays: the counter the client would send.
			relayed := 0
			for {
				m, _ := readVoiceFrame(t, ctx, conn)
				if m.Type == voiceDownAudioDelta {
					relayed++
				}
				if m.Type == voiceDownAudioDone {
					break
				}
			}
			if relayed != 1 {
				t.Fatalf("relayed %d audio chunks, want 1 (harness sanity: one speech frame in)", relayed)
			}
			readVoiceUntilState(t, ctx, conn, voiceStateDraining)
			tDraining := time.Now()
			beforeConns, beforeBytes := child.snapshot()

			// VALID (accounts for all relayed audio) but EARLY: it must not end the drain below the floor.
			sendVoicePlaybackDone(t, ctx, conn, relayed+extra)
			time.Sleep(600 * time.Millisecond)
			drainBurstDiscarded(t, ctx, conn, child, beforeConns, beforeBytes,
				"a counter-bearing early playback_done re-opened the mic before the floor: the counter shortened the drain")

			// The drain ends at ~floor+tail: the remembered valid signal is honored the moment the
			// floor passes -- so well under the 30s ceiling, and not before the floor.
			lctx, lcancel := context.WithTimeout(ctx, 6*time.Second)
			defer lcancel()
			readVoiceUntilState(t, lctx, conn, voiceStateListening)
			if waited := time.Since(tDraining); waited < time.Second {
				t.Fatalf("returned to listening %v after draining began, want it held to the ~2s floor despite the early counter-bearing signal", waited)
			}
		})
	}
}

// TestVoiceDrainExtendsForLatePlaybackDone: a playback_done that arrives AFTER the floor (a slow
// client still playing the answer out) EXTENDS the drain to the signal. Past the short floor,
// with no signal yet, the mic is still gated -- it has NOT re-opened at the floor -- and only
// the late signal ends the drain.
func TestVoiceDrainExtendsForLatePlaybackDone(t *testing.T) {
	child := newFakeVoiceASRChild(t)
	child.finals = []string{"slow client"}
	timing := turnGraceTiming()
	timing.micResumeTail = 30 * time.Millisecond
	// ~0.25s floor, generous (default) ceiling: the LATE signal, not the ceiling, ends the drain.
	h := newVoiceTurnHarness(t, child, voiceTurnOpts{
		timing:    timing,
		speechPCM: [][]byte{speechPCM(88200/4, 1)},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()

	conn := h.dial(t, ctx)
	conn.SetReadLimit(1 << 21)
	readVoiceUntilState(t, ctx, conn, voiceStateListening)

	if f := drainAChunk(t, ctx, conn); f != "slow client" {
		t.Fatalf("asr_final = %q, want %q", f, "slow client")
	}
	readVoiceUntil(t, ctx, conn, voiceDownAudioDone)
	readVoiceUntilState(t, ctx, conn, voiceStateDraining)
	beforeConns, beforeBytes := child.snapshot()

	// Well past the ~0.25s floor, with no signal yet: the drain must NOT have ended at the floor,
	// so a slow client's late signal can still extend it. The mic is still gated.
	time.Sleep(700 * time.Millisecond)
	drainBurstDiscarded(t, ctx, conn, child, beforeConns, beforeBytes,
		"the drain ended at the floor with no signal: a late playback_done could not extend it")

	// The slow client finally reports playout done -> the drain ends now (+ tail).
	sendVoiceControl(t, ctx, conn, "playback_done")
	lctx, lcancel := context.WithTimeout(ctx, 2*time.Second)
	defer lcancel()
	readVoiceUntilState(t, lctx, conn, voiceStateListening)
}

// TestVoiceDrainFallbackEndsWithoutSignal: with no playback_done at all, the drain must still
// end -- not instantly (the mic stays gated past the floor, waiting out the ceiling, so PCM sent
// mid-drain is discarded) and not never (the absolute ceiling re-opens the session). A short
// ceiling is set for the test; in production it is the generous VoicePlaybackFallbackMs.
func TestVoiceDrainFallbackEndsWithoutSignal(t *testing.T) {
	child := newFakeVoiceASRChild(t)
	child.finals = []string{"no signal"}
	timing := turnGraceTiming()
	timing.micResumeTail = 30 * time.Millisecond
	timing.playbackCap = 900 * time.Millisecond // a short absolute ceiling for the test
	h := newVoiceTurnHarness(t, child, voiceTurnOpts{
		timing:    timing,
		speechPCM: [][]byte{speechPCM(88200/4, 1)}, // ~0.25s floor, well under the 900ms ceiling
	})
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()

	conn := h.dial(t, ctx)
	conn.SetReadLimit(1 << 21)
	readVoiceUntilState(t, ctx, conn, voiceStateListening)

	if f := drainAChunk(t, ctx, conn); f != "no signal" {
		t.Fatalf("asr_final = %q, want %q", f, "no signal")
	}
	readVoiceUntil(t, ctx, conn, voiceDownAudioDone)
	readVoiceUntilState(t, ctx, conn, voiceStateDraining)
	beforeConns, beforeBytes := child.snapshot()

	// NOT instantly: past the ~0.25s floor, with no signal, the mic is still gated (waiting out
	// the ceiling), so a burst here is discarded.
	time.Sleep(450 * time.Millisecond)
	drainBurstDiscarded(t, ctx, conn, child, beforeConns, beforeBytes,
		"mid-drain PCM reached the ASR: the drain ended before the ceiling")

	// NOT never: with no playback_done, the absolute ceiling (~900ms) re-opens the session.
	lctx, lcancel := context.WithTimeout(ctx, 3*time.Second)
	defer lcancel()
	readVoiceUntilState(t, lctx, conn, voiceStateListening)
}

// concurrentSpeakFinal is an ASR'd user text that carries a <speak> region. The mock LLM echoes
// its prompt, so the voice run's answer stream closes a <speak> summary MID-stream and the speech
// leg fires CONCURRENTLY (the phase-2 overlap, spec 5.5) while the detail text keeps streaming.
// The summary text is in the user turn only because the mock has no other way to put <speak> in
// its output; the orchestrator strips the tags from the ANSWER, which is all these tests need.
const concurrentSpeakFinal = "<speak>hi</speak> and here is the longer written detail"

// TestVoiceConcurrentEarlyPlaybackDoneHonoredAtFloor is the phase-2 mic re-open regression: when
// the LLM detail OUTLASTS the summary audio's real-time playout, the client sends its single
// playback_done when the SUMMARY queue drains -- which happens while the server is still SPEAKING
// (the detail is not yet committed, so the drain wait has not begun). That early signal must be
// REMEMBERED, so the post-commit drain ends at the FLOOR (the summary audio's duration, already
// elapsed) and the mic re-opens promptly -- NOT after the 30s ceiling (dead air with the mic shut).
// It stays loop-safe: the mic only ever re-opens LATER than the floor, never earlier.
func TestVoiceConcurrentEarlyPlaybackDoneHonoredAtFloor(t *testing.T) {
	child := newFakeVoiceASRChild(t)
	child.finals = []string{concurrentSpeakFinal}
	timing := turnGraceTiming()
	timing.micResumeTail = 30 * time.Millisecond
	// A SHORT summary audio (~0.25s floor at the 88200 B/s fixture rate) that the client plays
	// out and reports drained long before the (held-open) detail commits.
	h := newVoiceTurnHarness(t, child, voiceTurnOpts{
		timing:    timing,
		speechPCM: [][]byte{speechPCM(88200/4, 1)},
	})
	// Hold the run at its commit point: the summary audio is relayed and the session enters
	// speaking, but the turn does not commit (runFinished stays false) until the test releases it
	// -- standing in for an LLM detail leg that outlasts the summary's playout.
	release := make(chan struct{})
	h.srv.voiceBeforeCommitClaim = func(_ *ChatRun) { <-release }

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	conn := h.dial(t, ctx)
	conn.SetReadLimit(1 << 21) // the relayed audio_delta is large; read it whole
	readVoiceUntilState(t, ctx, conn, voiceStateListening)

	if f := drainAChunk(t, ctx, conn); !strings.Contains(f, "hi") {
		t.Fatalf("asr_final = %q, want it to carry the spoken summary text", f)
	}
	// The window closes on the summary audio -> speaking, and the summary is relayed. (Reading
	// the audio_delta confirms speaking, since audio is relayed only after the mic shuts.)
	readVoiceUntil(t, ctx, conn, voiceDownAudioDelta)

	// The client plays the SUMMARY out and reports it drained -- while the server is still
	// speaking (awaitingPlayback is false). The pre-fix server DROPPED this signal.
	sendVoiceControl(t, ctx, conn, "playback_done")

	// Let the ~0.25s summary floor elapse in real time while still speaking, so by the time the
	// detail commits the drain's only remaining bound is the floor, which has already passed.
	time.Sleep(600 * time.Millisecond)

	// The detail finishes and the turn commits -> the post-commit drain begins with the floor
	// already passed and the early playback_done remembered, so it ends at the FLOOR (promptly,
	// plus the mic-resume tail), NOT after the 30s ceiling.
	released := time.Now()
	close(release)

	lctx, lcancel := context.WithTimeout(ctx, 3*time.Second)
	defer lcancel()
	readVoiceUntilState(t, lctx, conn, voiceStateListening)
	if waited := time.Since(released); waited > 2*time.Second {
		t.Fatalf("returned to listening %v after commit, want the floor (early playback_done honored), not the ~30s ceiling", waited)
	}
}

// TestVoiceConcurrentEarlyPlaybackDoneClampedToFloor is the anti-cheat for the remembered-signal
// path: an early playback_done seen while SPEAKING is remembered, but it must STILL NOT re-open
// the mic before the floor. Here the detail commits BEFORE the (longer) summary floor elapses, so
// the drain must hold the mic gated past the commit until the floor, then end at the floor (the
// remembered signal honored once the floor passes) -- never instantly at commit, never the ceiling.
func TestVoiceConcurrentEarlyPlaybackDoneClampedToFloor(t *testing.T) {
	child := newFakeVoiceASRChild(t)
	child.finals = []string{concurrentSpeakFinal}
	timing := turnGraceTiming()
	timing.micResumeTail = 30 * time.Millisecond
	// A LONGER summary audio (~1s floor) so the floor is still pending when the detail commits.
	h := newVoiceTurnHarness(t, child, voiceTurnOpts{
		timing:    timing,
		speechPCM: [][]byte{speechPCM(88200, 1)},
	})
	release := make(chan struct{})
	h.srv.voiceBeforeCommitClaim = func(_ *ChatRun) { <-release }

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	conn := h.dial(t, ctx)
	conn.SetReadLimit(1 << 21)
	readVoiceUntilState(t, ctx, conn, voiceStateListening)

	if f := drainAChunk(t, ctx, conn); !strings.Contains(f, "hi") {
		t.Fatalf("asr_final = %q, want it to carry the spoken summary text", f)
	}
	readVoiceUntil(t, ctx, conn, voiceDownAudioDelta) // speaking: the summary was relayed

	// Early playback_done while speaking -- remembered by the fix. Settle so the orchestrator
	// folds it in BEFORE the commit (the remembered-during-speaking path), while well under the
	// ~1s floor.
	sendVoiceControl(t, ctx, conn, "playback_done")
	time.Sleep(150 * time.Millisecond)

	// The detail commits while the floor is NOT yet passed -> the post-commit drain arms the
	// floor, and the remembered signal must NOT short-circuit it.
	close(release)
	readVoiceUntil(t, ctx, conn, voiceDownAudioDone)
	readVoiceUntilState(t, ctx, conn, voiceStateDraining)
	beforeConns, beforeBytes := child.snapshot()
	drainBurstDiscarded(t, ctx, conn, child, beforeConns, beforeBytes,
		"a remembered early playback_done re-opened the mic before the floor: the floor clamp was lost")

	// It still ends at the floor (the remembered signal is honored once the floor passes), not at
	// the 30s ceiling.
	lctx, lcancel := context.WithTimeout(ctx, 3*time.Second)
	defer lcancel()
	readVoiceUntilState(t, lctx, conn, voiceStateListening)
}

// TestVoiceConcurrentMidStreamGapSignalDoesNotEndDrainEarly guards the half-duplex loop against
// the remembered-signal fix itself. The client's player fires onEmpty -- and sends playback_done
// -- on EVERY queue drain, including a gap between TTS chunks when TTS lags real time, not only at
// the true end. A gap signal remembered mid-stream must NOT end the drain while later audio is
// still playing. Here chunk A is relayed, a gap playback_done arrives, THEN more audio is relayed
// (which clears the remembered signal) and the turn commits; the mic must stay gated until the
// genuine end-of-audio signal, never re-opening on the stale gap signal (echo exposure).
//
// The "more audio" that clears the signal is no longer chunk B alone. Chunk B is held for half a
// second, and the relay does not leave the player idle over a gap that long: it plays keepalive
// silence (#339), one frame every ~150 ms, and every frame it relays -- silence included, since it is
// audio the client still has to play -- clears the remembered gap signal exactly as a real chunk does
// (TestVoiceRelayAudioTreatsSilenceLikeAnyChunk pins that on the machine). So the signal is cleared
// mid-gap by the next silence frame after the gap signal (the machine holds chunk A back until the
// continuation grace closes, so a silence frame can already have been relayed before the signal is
// sent), well before B is released; B then adds the last audio and the turn commits. That is the
// correct behavior and what the assertions below hold; only the mechanism that clears the signal
// moved from B to the silence. The silence also counts toward the drain floor (relayedPlaybackDur),
// so over this 500 ms the floor alone would keep the mic gated: this test no longer isolates the
// clear-on-relay guard, which is why that guard is pinned directly on the machine.
func TestVoiceConcurrentMidStreamGapSignalDoesNotEndDrainEarly(t *testing.T) {
	child := newFakeVoiceASRChild(t)
	child.finals = []string{concurrentSpeakFinal}
	timing := turnGraceTiming()
	timing.micResumeTail = 30 * time.Millisecond
	// Two summary chunks (~0.1s floor each, ~0.2s total). The gate holds chunk B until the test
	// releases it -- after a gap playback_done -- so B is relayed AFTER the gap signal is seen.
	gate := make(chan struct{})
	h := newVoiceTurnHarness(t, child, voiceTurnOpts{
		timing:     timing,
		speechPCM:  [][]byte{speechPCM(88200/10, 1), speechPCM(88200/10, 2)},
		speechGate: gate,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	conn := h.dial(t, ctx)
	conn.SetReadLimit(1 << 21)
	readVoiceUntilState(t, ctx, conn, voiceStateListening)

	if f := drainAChunk(t, ctx, conn); !strings.Contains(f, "hi") {
		t.Fatalf("asr_final = %q, want it to carry the spoken summary text", f)
	}
	// Chunk A is relayed at window close -> speaking.
	readVoiceUntil(t, ctx, conn, voiceDownAudioDelta)

	// A GAP playback_done (the player's queue drained between chunks) arrives while speaking --
	// remembered by the fix. Let the ~0.2s floor elapse so a stale signal would be able to end the
	// drain the moment the turn commits. B is still held, so the relay plays silence over these
	// 500 ms (#339), and the next silence frame it relays after this signal clears the remembered signal.
	sendVoiceControl(t, ctx, conn, "playback_done")
	time.Sleep(500 * time.Millisecond)

	// Release chunk B: it is the last audio, and the turn then commits. B is published before the
	// run's terminal, so it is relayed before the post-commit drain begins.
	close(gate)
	readVoiceUntilState(t, ctx, conn, voiceStateDraining)

	// Past the mic-resume tail the mic must still be gated: the drain waits for the genuine
	// end-of-audio signal, not the stale gap signal. A burst now must be discarded. (This test does
	// not isolate the clear-on-relay guard -- the silence's floor extension keeps the mic gated here
	// either way; that guard is pinned by TestVoiceRelayAudioTreatsSilenceLikeAnyChunk.)
	beforeConns, beforeBytes := child.snapshot()
	time.Sleep(150 * time.Millisecond)
	drainBurstDiscarded(t, ctx, conn, child, beforeConns, beforeBytes,
		"a mid-stream gap playback_done re-opened the mic before the genuine end-of-audio signal (echo exposure)")

	// The genuine end-of-audio signal (nothing relayed after it) ends the drain.
	sendVoiceControl(t, ctx, conn, "playback_done")
	lctx, lcancel := context.WithTimeout(ctx, 3*time.Second)
	defer lcancel()
	readVoiceUntilState(t, lctx, conn, voiceStateListening)
}

// TestVoiceDrainStalePlaybackCounterDoesNotEndDrain closes the reorder residual the clear-on-relay
// heuristic alone leaves (#322). The client sends playback_done on EVERY queue drain, each carrying
// a monotonic count of the audio it has RECEIVED (played_through). A stale gap signal (sent after an
// earlier chunk drained, before a later one was received) can, under the server/client race, reach
// the server AFTER all audio was relayed and the floor passed -- the one ordering clear-on-relay
// cannot catch, because no further relay happens during the drain to clear it. The counter catches
// it: a signal whose count is BELOW the audio already relayed is not the end of audio and must not
// end the drain, while the genuine final signal (count == relayed) ends it at the floor.
func TestVoiceDrainStalePlaybackCounterDoesNotEndDrain(t *testing.T) {
	child := newFakeVoiceASRChild(t)
	child.finals = []string{"stale counter"}
	timing := turnGraceTiming()
	timing.micResumeTail = 30 * time.Millisecond
	// Two chunks (~0.1s each, ~0.2s total floor) relayed before the drain begins, so the server
	// has relayed 2 and a stale played_through=1 accounts for only the first. The default (30s)
	// ceiling leaves the genuine signal, not the ceiling, to end the drain.
	h := newVoiceTurnHarness(t, child, voiceTurnOpts{
		timing:    timing,
		speechPCM: [][]byte{speechPCM(88200/10, 1), speechPCM(88200/10, 2)},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()

	conn := h.dial(t, ctx)
	conn.SetReadLimit(1 << 21) // the relayed audio_delta is large; read it whole
	readVoiceUntilState(t, ctx, conn, voiceStateListening)

	if f := drainAChunk(t, ctx, conn); f != "stale counter" {
		t.Fatalf("asr_final = %q, want %q", f, "stale counter")
	}
	readVoiceUntil(t, ctx, conn, voiceDownAudioDone)
	readVoiceUntilState(t, ctx, conn, voiceStateDraining)

	// Let the ~0.2s floor elapse: from here the only thing that may end the drain is a genuine
	// end-of-audio signal (else it waits out the 30s ceiling). Both chunks are relayed.
	time.Sleep(500 * time.Millisecond)
	beforeConns, beforeBytes := child.snapshot()

	// A STALE signal: it accounts for only 1 of the 2 relayed chunks (a gap signal that lost the
	// race and arrives during the drain). It must NOT end the drain -- the mic stays gated.
	sendVoicePlaybackDone(t, ctx, conn, 1)
	time.Sleep(150 * time.Millisecond)
	drainBurstDiscarded(t, ctx, conn, child, beforeConns, beforeBytes,
		"a playback_done whose counter is below the relayed audio ended the drain early (reorder residual)")

	// The genuine final signal accounts for all relayed audio: it ends the drain at the floor
	// (promptly, plus the mic-resume tail), NOT the 30s ceiling.
	released := time.Now()
	sendVoicePlaybackDone(t, ctx, conn, 2)
	lctx, lcancel := context.WithTimeout(ctx, 3*time.Second)
	defer lcancel()
	readVoiceUntilState(t, lctx, conn, voiceStateListening)
	if waited := time.Since(released); waited > 2*time.Second {
		t.Fatalf("returned to listening %v after the matching signal, want the floor, not the ~30s ceiling", waited)
	}
}

// TestVoiceDrainMissingPlaybackCounterKeepsOldBehavior pins the additive back-compat: an older
// client sends playback_done with NO played_through. The server must treat it exactly as before
// (the remembered-signal path), so a non-reordering client is never regressed. Here a single
// counter-less signal after the floor ends the drain at once.
func TestVoiceDrainMissingPlaybackCounterKeepsOldBehavior(t *testing.T) {
	child := newFakeVoiceASRChild(t)
	child.finals = []string{"old client"}
	timing := turnGraceTiming()
	timing.micResumeTail = 30 * time.Millisecond
	h := newVoiceTurnHarness(t, child, voiceTurnOpts{
		timing:    timing,
		speechPCM: [][]byte{speechPCM(88200/10, 1)}, // ~0.1s floor
	})
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()

	conn := h.dial(t, ctx)
	conn.SetReadLimit(1 << 21)
	readVoiceUntilState(t, ctx, conn, voiceStateListening)

	if f := drainAChunk(t, ctx, conn); f != "old client" {
		t.Fatalf("asr_final = %q, want %q", f, "old client")
	}
	readVoiceUntil(t, ctx, conn, voiceDownAudioDone)
	readVoiceUntilState(t, ctx, conn, voiceStateDraining)

	// Past the ~0.1s floor, a counter-less playback_done (an old client) ends the drain, as before.
	time.Sleep(300 * time.Millisecond)
	sendVoiceControl(t, ctx, conn, "playback_done")
	lctx, lcancel := context.WithTimeout(ctx, 3*time.Second)
	defer lcancel()
	readVoiceUntilState(t, lctx, conn, voiceStateListening)
}

// TestVoiceConcurrentLoopbackFeed is the #331 regression: the per-turn ASR loopback must be fed
// PCM CONCURRENTLY with its dial. The dial (openLiveLoopback -> Do) only returns once the ASR
// child accepts the request, and on a cold model that accept is slow; audio.cpp starts its idle
// timer at the accept, so a body that stays empty through the dial idle-times-out ("live
// request body stalled"). The state-machine run loop used to block in that dial and write the
// first frame only after it returned, so the child saw no PCM while its accept was pending.
//
// The child here HOLDS its accept (acceptGate) while still reading and recording its request
// body -- a real child waiting on its model. The test sends frames, and asserts the child
// received them BEFORE the accept (the gate is only released afterwards, by the test, so the
// assertion is ordering, not a race against a sleep); then it releases the accept and proves
// the held turn still completes with the full PCM stream delivered intact and in order.
func TestVoiceConcurrentLoopbackFeed(t *testing.T) {
	child := newFakeVoiceASRChild(t)
	child.finals = []string{"slow but whole"}
	child.acceptGate = make(chan struct{})
	h := newVoiceTurnHarness(t, child, voiceTurnOpts{timing: turnGraceTiming()})
	// Release the held accept on any exit (registered after the harness so it runs BEFORE the
	// servers' Close, which would otherwise wait on the held request).
	var release sync.Once
	releaseAccept := func() { release.Do(func() { close(child.acceptGate) }) }
	t.Cleanup(releaseAccept)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn := h.dial(t, ctx)
	readVoiceUntilState(t, ctx, conn, voiceStateListening)

	// Distinct, recognisable frames: frame i is 320 bytes of value i+1, so ordering and loss
	// show up in the child's recorded stream.
	const frames, frameLen = 6, 320
	var want []byte
	for i := range frames {
		frame := make([]byte, frameLen)
		for j := range frame {
			frame[j] = byte(i + 1)
		}
		want = append(want, frame...)
		if err := conn.Write(ctx, websocket.MessageBinary, frame); err != nil {
			t.Fatalf("send pcm frame %d: %v", i, err)
		}
	}

	// The accept is held, so any PCM the child has is PCM that crossed the loopback DURING the
	// dial. On the serial code this never happens: the run loop is parked in Do.
	deadline := time.Now().Add(5 * time.Second)
	for child.bytesBeforeAccept() < len(want) {
		if time.Now().After(deadline) {
			t.Fatalf("the ASR child received %d of %d PCM bytes while its accept was held: the loopback is not fed concurrently with the dial (the run loop is blocked in the dial)", child.bytesBeforeAccept(), len(want))
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Let the child accept: the turn must still complete, with its partials and the clean final.
	releaseAccept()
	readVoiceUntil(t, ctx, conn, voiceDownASRPartial)
	sendVoiceControl(t, ctx, conn, "push_end")
	if final := readVoiceUntil(t, ctx, conn, voiceDownASRFinal); final.Text != "slow but whole" {
		t.Fatalf("asr_final = %q, want %q", final.Text, "slow but whole")
	}

	// The child's recorded body is exactly what the browser sent: nothing dropped or
	// reordered, all of it before the accept.
	var got []byte
	for _, rd := range child.bodyReads() {
		if !rd.beforeAccept {
			t.Fatalf("a body read of %d bytes arrived AFTER the accept; all %d PCM bytes should have been fed during the dial", len(rd.data), len(want))
		}
		got = append(got, rd.data...)
	}
	if string(got) != string(want) {
		t.Fatalf("the child received %d PCM bytes in the wrong shape; want the %d bytes the browser sent, in order", len(got), len(want))
	}
}

// TestVoiceLoopbackRefusalSurfacesCodeAndMessage pins the refusal path now that the dial
// outcome reaches the machine as an event (#331) instead of a synchronous return: when /v1
// REFUSES the per-turn loopback (here a 409 -- the ASR route lost its streaming verdict after
// the ticket was minted, so the loopback's own re-gate refuses), the browser gets /v1's own
// stable code and message as the error frame, the session goes idle, and it stays usable.
func TestVoiceLoopbackRefusalSurfacesCodeAndMessage(t *testing.T) {
	child := newFakeVoiceASRChild(t)
	h := newVoiceTurnHarness(t, child, voiceTurnOpts{timing: turnGraceTiming()})
	now := time.Now()
	if err := h.srv.Routes.UpsertMappingCapabilities(context.Background(), "route-asr", []routing.CapabilityRow{
		{Capability: routing.CapabilityTranscription, Verdict: routing.CapabilityYes, Source: routing.CapabilitySourceAudioCppModels, CheckedAt: now},
		{Capability: routing.CapabilityTranscriptionStreaming, Verdict: routing.CapabilityNo, Source: routing.CapabilitySourceAudioCppModels, CheckedAt: now},
	}); err != nil {
		t.Fatalf("UpsertMappingCapabilities: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	conn := h.dial(t, ctx)
	readVoiceUntilState(t, ctx, conn, voiceStateListening)
	sendVoicePCM(t, ctx, conn, 320)

	errFrame := readVoiceUntil(t, ctx, conn, voiceDownError)
	if errFrame.Code != liveTranscriptionNotStreaming || errFrame.Message == "" {
		t.Fatalf("error frame = code %q message %q, want /v1's code %q and its message", errFrame.Code, errFrame.Message, liveTranscriptionNotStreaming)
	}
	readVoiceUntilState(t, ctx, conn, voiceStateIdle)

	// The refusal tore the turn down cleanly: the session is alive and re-engages on push_start.
	sendVoiceControl(t, ctx, conn, "push_start")
	readVoiceUntilState(t, ctx, conn, voiceStateListening)
}

// TestVoiceLoopbackDialErrorFailsTurn pins the dial-error path: when the loopback dial itself
// fails (the gateway's own address refuses the connection), the machine degrades with the
// stable transcription.unavailable code and goes idle -- no retry, no hang.
func TestVoiceLoopbackDialErrorFailsTurn(t *testing.T) {
	child := newFakeVoiceASRChild(t)
	h := newVoiceTurnHarness(t, child, voiceTurnOpts{timing: turnGraceTiming()})
	h.srv.selfBaseURL = "http://127.0.0.1:1" // nothing listens: the dial is refused at connect
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	conn := h.dial(t, ctx)
	readVoiceUntilState(t, ctx, conn, voiceStateListening)
	sendVoicePCM(t, ctx, conn, 320)

	if errFrame := readVoiceUntil(t, ctx, conn, voiceDownError); errFrame.Code != "transcription.unavailable" {
		t.Fatalf("error frame code = %q, want transcription.unavailable", errFrame.Code)
	}
	readVoiceUntilState(t, ctx, conn, voiceStateIdle)
}

// --- Teardown, flood and re-dial of the per-turn loopback (#331) -------------------------
//
// The pump/dial split (openTurnLoopback) leaves three goroutines per turn that the state
// machine must start, feed and JOIN without ever blocking on them. TestVoiceConcurrentLoopbackFeed
// pins the happy ordering; the tests below pin the edges: the session ending (or the turn being
// cancelled) while the dial is still in flight, a flood of PCM against a pump that cannot drain,
// and a fresh loopback after an abandoned one.

// voiceLoopbackGoroutineNeedles name the functions one turn's loopback goroutines run: the PCM
// pump, the dial (parked in openLiveLoopback until the child accepts) and the SSE relay. They
// are the goroutines relayWG joins, so none may be left once the machine has returned.
var voiceLoopbackGoroutineNeedles = []string{
	"gateway.voiceLoopbackPump(",
	"gateway.(*Server).dialTurnLoopback(",
	"gateway.(*Server).openLiveLoopback(",
	"gateway.voiceASRRelay(",
}

// goroutineStacks returns the stack of every live goroutine, keyed by goroutine id.
func goroutineStacks() map[int]string {
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			buf = buf[:n]
			break
		}
		buf = make([]byte, 2*len(buf))
	}
	stacks := map[int]string{}
	for _, g := range strings.Split(string(buf), "\n\n") {
		var id int
		if _, err := fmt.Sscanf(g, "goroutine %d [", &id); err == nil {
			stacks[id] = g
		}
	}
	return stacks
}

// newRepoGoroutines returns the stacks of the goroutines that are NOT in base (a goroutineStacks
// snapshot taken before the code under test ran) and are running this module's code -- the
// goroutines the test itself caused. With needles it keeps only those running one of them. Counting
// by id and by module frame, not runtime.NumGoroutine, keeps the check exact: the http transport's
// keep-alive goroutines and the runtime's own never count, and a leak names its own stack.
func newRepoGoroutines(base map[int]string, needles ...string) []string {
	var found []string
	for id, stack := range goroutineStacks() {
		if _, old := base[id]; old || !strings.Contains(stack, "op-ai-gateway/internal/") {
			continue
		}
		if len(needles) > 0 && !containsAnySubstring(stack, needles) {
			continue
		}
		found = append(found, stack)
	}
	return found
}

func containsAnySubstring(s string, subs []string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// voicePoll polls cond every 10ms until it holds or d elapses, reporting whether it held. It is the
// condition poll the loopback tests use instead of a fixed sleep: it returns the moment the
// condition is true, and d only bounds a failing run.
func voicePoll(d time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
	return true
}

// voiceChildStream is the PCM the fake ASR child received on one connection (1-based), in order.
func voiceChildStream(child *fakeVoiceASRChild, conn int) []byte {
	var stream []byte
	for _, rd := range child.bodyReads() {
		if rd.conn == conn {
			stream = append(stream, rd.data...)
		}
	}
	return stream
}

// voiceIndexedFrame is a PCM frame that identifies itself: a 2-byte big-endian index, then a
// payload derived from the index and the offset. A dropped, duplicated, reordered or split frame
// therefore shows in a recorded stream (voiceParseIndexedStream).
func voiceIndexedFrame(index, size int) []byte {
	f := make([]byte, size)
	f[0], f[1] = byte(index>>8), byte(index)
	for j := 2; j < size; j++ {
		f[j] = byte(index) ^ byte(j)
	}
	return f
}

// voiceParseIndexedStream splits a recorded stream back into its frames and returns their indexes
// in arrival order. It fails the test unless the stream is a whole number of well-formed frames:
// the child must never see half a frame, whatever was dropped upstream.
func voiceParseIndexedStream(t *testing.T, stream []byte, size int) []int {
	t.Helper()
	if len(stream)%size != 0 {
		t.Fatalf("the child received %d PCM bytes, not a whole number of %d-byte frames: a frame was split or truncated", len(stream), size)
	}
	var order []int
	for off := 0; off < len(stream); off += size {
		got := stream[off : off+size]
		index := int(got[0])<<8 | int(got[1])
		if !bytes.Equal(got, voiceIndexedFrame(index, size)) {
			t.Fatalf("the %d-byte frame at offset %d is not a well-formed frame %d: the stream is corrupted", size, off, index)
		}
		order = append(order, index)
	}
	return order
}

// readVoiceUntilFailingOnError is readVoiceUntil, except an error frame on the way fails the
// test: where one is waited on, an error frame can only mean an abandoned loopback leaked its
// failure into the live session.
func readVoiceUntilFailingOnError(t *testing.T, ctx context.Context, conn *websocket.Conn, typ string) voiceDownMsg {
	t.Helper()
	for {
		m, _ := readVoiceFrame(t, ctx, conn)
		if m.Type == voiceDownError {
			t.Fatalf("error frame while waiting for %q: code=%q message=%q", typ, m.Code, m.Message)
		}
		if m.Type == typ {
			return m
		}
	}
}

// TestVoiceTeardownMidDialJoinsEverything: the session ends while the turn's loopback DIAL is
// still in flight -- the child holds its accept, so openLiveLoopback is parked in Do, the pump has
// already fed PCM into the body, and no response has come back. Teardown must cancel that dial,
// unblock the pipe, and JOIN the dial and pump (relayWG) before the machine returns; the handler
// then returns promptly (well inside the child's held-accept cap, so teardown cannot be waiting
// on the accept) and nothing the session started is left running.
//
// The join is asserted twice. Right after the handler returns the turn's own goroutines (pump,
// dial, relay) must already be gone -- no polling: the machine joins them before it returns, so
// any one still running is a missed join, not slowness. The rest of the chain (the /v1 handler
// the dial reached, the child's request handler and its body reader) lives on the far side of
// connections the teardown only CLOSES, so those get a bounded condition poll. And the aborted
// dial's failure must not surface as an error frame; the bye case checks that on the wire as far
// as it can be observed (the run loop almost always sees ctx.Done before the stale event), and
// TestVoiceLoopbackEventsAreFoldedByTheTurnTheyBelongTo is what pins the machine's handling of
// it.
//
// Both ways a session ends are run: a bye (clean close handshake) and a vanished client (the
// read loop's error cancels the handler context from the other side).
func TestVoiceTeardownMidDialJoinsEverything(t *testing.T) {
	cases := []struct {
		name string
		end  func(t *testing.T, ctx context.Context, conn *websocket.Conn)
	}{
		{"bye", func(t *testing.T, ctx context.Context, conn *websocket.Conn) {
			sendVoiceControl(t, ctx, conn, "bye")
			// The session closes cleanly, and no error frame comes down on the way out.
			for {
				typ, data, err := conn.Read(ctx)
				if err != nil {
					if websocket.CloseStatus(err) != websocket.StatusNormalClosure {
						t.Fatalf("after bye the close status = %v (err %v), want StatusNormalClosure", websocket.CloseStatus(err), err)
					}
					return
				}
				var m voiceDownMsg
				if typ == websocket.MessageText && json.Unmarshal(data, &m) == nil && m.Type == voiceDownError {
					t.Fatalf("the torn-down dial surfaced an error frame: code=%q message=%q", m.Code, m.Message)
				}
			}
		}},
		{"client drop", func(_ *testing.T, _ context.Context, conn *websocket.Conn) {
			_ = conn.CloseNow()
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			child := newFakeVoiceASRChild(t)
			child.acceptGate = make(chan struct{})
			h := newVoiceTurnHarness(t, child, voiceTurnOpts{timing: turnGraceTiming()})
			// Release the held accept on any exit, before the servers' Close waits on it.
			var release sync.Once
			t.Cleanup(func() { release.Do(func() { close(child.acceptGate) }) })

			base := goroutineStacks() // before the session starts anything
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			conn := h.dial(t, ctx)
			readVoiceUntilState(t, ctx, conn, voiceStateListening)

			var want []byte
			for i := range 3 {
				frame := voiceIndexedFrame(i+1, 320)
				want = append(want, frame...)
				if err := conn.Write(ctx, websocket.MessageBinary, frame); err != nil {
					t.Fatalf("send pcm frame %d: %v", i, err)
				}
			}
			// Mid-dial for certain: the PCM crossed to the child while its accept is held, and the
			// dial and the pump are both running. Without this the test could pass on a turn that
			// had already settled.
			if !voicePoll(5*time.Second, func() bool {
				return child.bytesBeforeAccept() >= len(want) &&
					len(newRepoGoroutines(base, "gateway.(*Server).dialTurnLoopback(")) == 1 &&
					len(newRepoGoroutines(base, "gateway.voiceLoopbackPump(")) == 1
			}) {
				t.Fatalf("the dial never reached the held-accept state (child saw %d of %d PCM bytes before accept)", child.bytesBeforeAccept(), len(want))
			}

			tc.end(t, ctx, conn)

			select {
			case <-h.returned:
			case <-time.After(5 * time.Second):
				t.Fatal("the handler did not return with the dial in flight: teardown is waiting on the dial, the pump or the held accept")
			}
			if left := newRepoGoroutines(base, voiceLoopbackGoroutineNeedles...); len(left) > 0 {
				t.Fatalf("the handler returned with %d of the turn's dial/pump/relay goroutines still running: teardown did not join them\n%s", len(left), strings.Join(left, "\n\n"))
			}

			var leaked []string
			if !voicePoll(5*time.Second, func() bool {
				leaked = newRepoGoroutines(base)
				return len(leaked) == 0
			}) {
				t.Fatalf("%d goroutine(s) the session started are still running after teardown:\n%s", len(leaked), strings.Join(leaked, "\n\n"))
			}
		})
	}
}

// closeTrackingBody is a dial outcome's response body that records whether it was closed.
type closeTrackingBody struct{ closed atomic.Bool }

func (b *closeTrackingBody) Read([]byte) (int, error) { return 0, io.EOF }
func (b *closeTrackingBody) Close() error             { b.closed.Store(true); return nil }

// TestVoiceTeardownTurnJoinsParkedPumpAndLateDial pins the join DETERMINISTICALLY, which the
// end-to-end teardown test cannot: there a missed join is only a race the goroutines usually win
// (a cancelled dial and a closed pipe unwind in microseconds), so it passes either way. Here the
// turn is assembled by hand, around the REAL pump, with goroutines whose timing the test owns:
//   - the pump is parked on its first pw.Write (nothing reads the pipe, the state a dial that
//     never accepts leaves it in), and teardown must unblock it -- by closing the pipe with
//     errVoiceTurnDone BEFORE it closes the queue -- without the pump mistaking that for a
//     failure to report;
//   - a stand-in for the dial notices the cancel but unwinds slowly, then reports a dial that
//     completed anyway. teardownTurn must not return before it has finished (relayWG), and must
//     close the body of the outcome the run loop never consumed instead of leaking its connection.
func TestVoiceTeardownTurnJoinsParkedPumpAndLateDial(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	turnCtx, turnCancel := context.WithCancel(ctx)
	_, pw := io.Pipe() // nothing reads the other end: a write parks until the pipe is closed
	queue := make(chan []byte, voiceLoopbackPCMQueue)
	pumpEvents := make(chan voiceLoopbackEvent, 2) // the pump's own channel: it must stay empty
	dialEvents := make(chan voiceLoopbackEvent, 2) // the machine's loopCh: the dial's late outcome lands here
	m := &voiceMachine{ctx: ctx, pw: pw, turnCtx: turnCtx, turnCancel: turnCancel, pcmOut: queue, loopCh: dialEvents}

	base := goroutineStacks()
	queue <- []byte("pcm")
	m.relayWG.Add(1)
	go func() { defer m.relayWG.Done(); voiceLoopbackPump(turnCtx, queue, pw, pumpEvents) }()
	pumpParked := func() bool {
		for _, stack := range newRepoGoroutines(base, "gateway.voiceLoopbackPump(") {
			if strings.Contains(stack, "io.(*PipeWriter).Write") {
				return true
			}
		}
		return false
	}
	if !voicePoll(5*time.Second, pumpParked) {
		t.Fatal("the pump never parked on its pipe write")
	}

	lateBody := &closeTrackingBody{}
	finishDial := make(chan struct{})
	m.relayWG.Add(1)
	go func() {
		defer m.relayWG.Done()
		// An in-flight dial notices the cancel, takes its time unwinding, and then reports a dial
		// that completed anyway.
		<-turnCtx.Done()
		<-finishDial
		dialEvents <- voiceLoopbackEvent{kind: loopbackDialed, body: lateBody}
	}()

	torn := make(chan struct{})
	go func() { defer close(torn); m.teardownTurn() }()

	// The pump is released by the teardown itself: it was parked and nothing else could free it.
	if !voicePoll(5*time.Second, func() bool { return len(newRepoGoroutines(base, "gateway.voiceLoopbackPump(")) == 0 }) {
		t.Fatal("teardownTurn did not unblock the pump parked on the pipe write")
	}
	// ... but the dial is still unwinding, so teardown must still be waiting for it.
	select {
	case <-torn:
		t.Fatal("teardownTurn returned while the dial goroutine was still running: it does not join it")
	case <-time.After(100 * time.Millisecond):
	}
	close(finishDial)
	select {
	case <-torn:
	case <-time.After(5 * time.Second):
		t.Fatal("teardownTurn did not return once the dial finished")
	}

	if !lateBody.closed.Load() {
		t.Fatal("the late dial outcome's response body was not closed: its connection leaks")
	}
	if len(pumpEvents) != 0 {
		t.Fatalf("the pump reported %d event(s) for a write that teardown itself failed: it must stay silent once the turn is ending", len(pumpEvents))
	}
	if m.turnCancel != nil || m.pw != nil || m.loopCh != nil {
		t.Fatal("teardownTurn left the turn's loopback fields set")
	}
}

// TestVoiceLoopbackEventsAreFoldedByTheTurnTheyBelongTo pins which dial/pump reports the run loop
// acts on (onLoopbackEvent), by the state each leaves the machine in. They are the reports of
// goroutines the machine does not control, so an event can arrive late or for a turn that is already
// ending, and acting on it would surface an error the user never caused:
//   - a dial failure that lands while the session itself is ending is just the teardown
//     cancelling the dial: no failure, no error frame (a live session fails the turn);
//   - a pump failure while the dial is still pending is ignored -- a failed dial closes the pipe,
//     which fails the pump's write first, and the dial's own outcome is the authoritative one --
//     and so is one while finalizing, which the relay's end or the finalize timeout already settles;
//   - a pump failure once the dial succeeded, mid-chunk, is a real stream error.
//
// The end-to-end teardown tests cannot pin the first case: the run loop almost always sees
// ctx.Done before the event, so the guard is otherwise unobservable.
func TestVoiceLoopbackEventsAreFoldedByTheTurnTheyBelongTo(t *testing.T) {
	cases := []struct {
		name      string
		ev        voiceLoopbackEventKind
		ending    bool // the session context is already cancelled
		dialDone  bool // the dial succeeded (asrCh is set)
		finalize  bool
		wantState string
		wantCode  string // the error frame the turn fails with; empty when none
	}{
		{name: "dial failure in a live session fails the turn", ev: loopbackDialFailed, wantState: voiceStateIdle, wantCode: "transcription.unavailable"},
		{name: "dial failure while the session ends is inert", ev: loopbackDialFailed, ending: true, wantState: voiceStateListening},
		{name: "pump failure while the dial is pending is inert", ev: loopbackPumpFailed, wantState: voiceStateListening},
		{name: "pump failure while finalizing is inert", ev: loopbackPumpFailed, dialDone: true, finalize: true, wantState: voiceStateListening},
		{name: "pump failure after the dial is a stream error", ev: loopbackPumpFailed, dialDone: true, wantState: voiceStateIdle, wantCode: "transcription.stream_error"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.ending {
				cancel()
			}
			m := &voiceMachine{ctx: ctx, writer: newVoiceWriter(&fakeVoiceSocket{}, func() {}), state: voiceStateListening, finalizing: tc.finalize}
			if tc.dialDone {
				m.asrCh = make(chan asrEvent)
			}
			m.onLoopbackEvent(voiceLoopbackEvent{kind: tc.ev})

			if m.state != tc.wantState {
				t.Fatalf("state after the event = %q, want %q", m.state, tc.wantState)
			}
			var code string
			for len(m.writer.out) > 0 {
				var f voiceDownMsg
				if err := json.Unmarshal(<-m.writer.out, &f); err != nil {
					t.Fatalf("down-frame is not JSON: %v", err)
				}
				if f.Type == voiceDownError {
					code = f.Code
				}
			}
			if tc.ending {
				return // a cancelled session context makes the writer's own send racy; the state is the verdict
			}
			if code != tc.wantCode {
				t.Fatalf("error frame code = %q, want %q", code, tc.wantCode)
			}
		})
	}
}

// TestVoiceLoopbackDropOldestKeepsRunLoopLive pins the drop-oldest policy of the run loop ->
// pump queue (feedLoopback) on the REAL run loop, with a loopback whose pump is parked: the
// machine is given a loopback that exists but whose queue nothing drains, exactly the state a
// dial that never accepts leaves the pump in (a blocked pw.Write). It is driven over the machine's
// own inbound channels with MORE frames than the queue holds, and must
//   - never block: every frame is taken (the inbound channel holds only voiceInboundQueue of
//     them, so a run loop stuck on the full queue would stall the sends -- and with them the read
//     loop and the socket);
//   - keep serving other events afterwards: a cancel control still tears the loopback down (it
//     closes the queue, which is how the test sees it was handled);
//   - retain exactly the NEWEST voiceLoopbackPCMQueue frames, in order, each whole -- the oldest
//     dropped, nothing split, reordered or duplicated.
//
// It is a machine-level test because it cannot be driven end to end deterministically: the fake
// child's accept gate still lets the transport drain the pipe (that is the point of
// TestVoiceConcurrentLoopbackFeed), so the real pump never parks long enough to fill 128 frames.
// The end-to-end counterpart is TestVoiceLoopbackFloodDeliversWholeFramesInOrder.
func TestVoiceLoopbackDropOldestKeepsRunLoopLive(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	queue := make(chan []byte, voiceLoopbackPCMQueue)
	pcmCh := make(chan []byte, voiceInboundQueue)
	ctrlCh := make(chan voiceUpControl, voiceInboundQueue)
	// torn closes when the run loop handles the cancel control: teardownTurn calls turnCancel FIRST,
	// on the run loop, after every earlier frame has been fed and before it closes the queue. It is
	// the barrier that orders the last feedLoopback before the test drains the queue.
	torn := make(chan struct{})
	m := &voiceMachine{
		ctx: ctx, cancel: cancel, timing: fastVoiceTiming(),
		writer: newVoiceWriter(&fakeVoiceSocket{}, cancel), // never run: the state frame just sits in its queue
		pcmCh:  pcmCh, ctrlCh: ctrlCh,
		turnCancel: func() { close(torn) }, // a loopback exists (opening or open): onPCM feeds it, it does not dial
		pcmOut:     queue,                  // ... and its pump is parked: nothing ever drains the queue
	}
	runDone := make(chan struct{})
	go func() { defer close(runDone); m.run() }()

	const frameLen = 320
	total := voiceLoopbackPCMQueue + 40
	frames := make([][]byte, total)
	for i := range frames {
		frames[i] = voiceIndexedFrame(i, frameLen)
	}
	sendCtx, sendCancel := context.WithTimeout(ctx, 5*time.Second)
	defer sendCancel()
	for i, f := range frames {
		select {
		case pcmCh <- f:
		case <-sendCtx.Done():
			t.Fatalf("the run loop stopped taking PCM after %d of %d frames while the pump was parked: it blocked on the full loopback queue", i, total)
		}
	}
	// Every frame has been taken; the last one's handler finishes before the loop selects again, so
	// the cancel below is handled after all of them.
	if !voicePoll(5*time.Second, func() bool { return len(pcmCh) == 0 }) {
		t.Fatal("the run loop never took the last queued frames")
	}
	ctrlCh <- voiceUpControl{Type: voiceUpCancel}

	// An empty pcmCh only proves the run loop RECEIVED the last frame, not that feedLoopback for it
	// has returned, so the queue must not be read until the cancel is being handled: torn closing is
	// the proof every feed is over and the run loop was still alive to handle the control.
	select {
	case <-torn:
	case <-time.After(5 * time.Second):
		t.Fatal("the cancel control never reached the run loop: it is wedged")
	}

	// The cancel then closes the queue (endLoopbackFeed): what is left in it is what the pump would
	// have received.
	var got [][]byte
	deadline := time.After(5 * time.Second)
	for open := true; open; {
		select {
		case f, ok := <-queue:
			if ok {
				got = append(got, f)
			} else {
				open = false
			}
		case <-deadline:
			t.Fatal("the cancel control never tore the loopback down: the run loop is wedged")
		}
	}
	if len(got) != voiceLoopbackPCMQueue {
		t.Fatalf("the queue held %d frames, want its full capacity %d", len(got), voiceLoopbackPCMQueue)
	}
	for k, f := range got {
		want := frames[total-voiceLoopbackPCMQueue+k]
		if !bytes.Equal(f, want) {
			t.Fatalf("queued frame %d is not frame %d: the oldest %d frames should be dropped and the newest %d kept whole and in order",
				k, total-voiceLoopbackPCMQueue+k, total-voiceLoopbackPCMQueue, voiceLoopbackPCMQueue)
		}
	}

	cancel()
	select {
	case <-runDone:
	case <-time.After(5 * time.Second):
		t.Fatal("the run loop did not exit when its context ended")
	}
}

// TestVoiceLoopbackFloodDeliversWholeFramesInOrder is the end-to-end counterpart of the
// drop-oldest pin, an integrity-under-flood test rather than a drop test: far more frames than the
// queue holds are flooded in while the child holds its accept, through the real pump, pipe and
// transport. The transport keeps draining, so in practice nothing is dropped; how many (if any)
// the run loop drops depends on how fast it does, so the test asserts what must hold either way:
// the run loop keeps taking the flood, the child receives a whole number of well-formed frames in
// strictly increasing order, the NEWEST frame is among them (a drop only ever takes the oldest),
// and the turn still finishes with its clean final.
func TestVoiceLoopbackFloodDeliversWholeFramesInOrder(t *testing.T) {
	child := newFakeVoiceASRChild(t)
	child.finals = []string{"flooded"}
	child.acceptGate = make(chan struct{})
	h := newVoiceTurnHarness(t, child, voiceTurnOpts{timing: turnGraceTiming()})
	var release sync.Once
	releaseAccept := func() { release.Do(func() { close(child.acceptGate) }) }
	t.Cleanup(releaseAccept)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn := h.dial(t, ctx)
	readVoiceUntilState(t, ctx, conn, voiceStateListening)

	const frameLen = 320
	total := 2*voiceLoopbackPCMQueue + 50
	for i := range total {
		if err := conn.Write(ctx, websocket.MessageBinary, voiceIndexedFrame(i, frameLen)); err != nil {
			t.Fatalf("send pcm frame %d: %v", i, err)
		}
	}

	// The accept is still held: the newest frame must reach the child anyway (it is always queued).
	newest := voiceIndexedFrame(total-1, frameLen)
	if !voicePoll(5*time.Second, func() bool { return bytes.HasSuffix(voiceChildStream(child, 1), newest) }) {
		t.Fatalf("the newest of %d frames never reached the child during the held accept (it saw %d bytes): the run loop blocked or lost it", total, len(voiceChildStream(child, 1)))
	}

	releaseAccept()
	readVoiceUntilFailingOnError(t, ctx, conn, voiceDownASRPartial)
	sendVoiceControl(t, ctx, conn, "push_end")
	if final := readVoiceUntilFailingOnError(t, ctx, conn, voiceDownASRFinal); final.Text != "flooded" {
		t.Fatalf("asr_final = %q, want %q", final.Text, "flooded")
	}

	order := voiceParseIndexedStream(t, voiceChildStream(child, 1), frameLen)
	for k := 1; k < len(order); k++ {
		if order[k] <= order[k-1] {
			t.Fatalf("frame %d arrived after frame %d: the stream is not in order", order[k], order[k-1])
		}
	}
	if len(order) == 0 || order[len(order)-1] != total-1 {
		t.Fatalf("the child's stream ends at frame %v, want the newest frame %d", order[len(order)-1:], total-1)
	}
	t.Logf("the child received %d of %d frames (%d dropped by the queue)", len(order), total, total-len(order))
}

// TestVoiceCancelMidDialThenRedial: a cancel control while the loopback dial is in flight tears
// that turn down (aborts the dial, joins its pump and dial), and the next voiced frame dials a
// FRESH loopback that works -- accepted at once, fed only the new frame, and finishing the turn
// with its own final. The abandoned loopback must leak nothing into the new one: no stale PCM in
// the new body, no error frame from the dial that was cancelled, no stale dial outcome folded in.
func TestVoiceCancelMidDialThenRedial(t *testing.T) {
	child := newFakeVoiceASRChild(t)
	child.finals = []string{"discarded", "second ok"} // connection 1 is abandoned, connection 2 completes
	child.acceptGate = make(chan struct{})
	h := newVoiceTurnHarness(t, child, voiceTurnOpts{timing: turnGraceTiming()})
	var release sync.Once
	releaseAccept := func() { release.Do(func() { close(child.acceptGate) }) }
	t.Cleanup(releaseAccept)

	base := goroutineStacks()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn := h.dial(t, ctx)
	readVoiceUntilState(t, ctx, conn, voiceStateListening)

	// Dial #1 starts on the first frame and is held: the PCM reaches the child before its accept.
	first := voiceIndexedFrame(1, 320)
	if err := conn.Write(ctx, websocket.MessageBinary, first); err != nil {
		t.Fatalf("send the first frame: %v", err)
	}
	if !voicePoll(5*time.Second, func() bool {
		return bytes.Equal(voiceChildStream(child, 1), first) && len(newRepoGoroutines(base, "gateway.(*Server).dialTurnLoopback(")) == 1
	}) {
		t.Fatalf("dial #1 never reached the held-accept state (child saw %d bytes)", len(voiceChildStream(child, 1)))
	}

	// Cancel mid-dial. The turn's dial, pump and relay are joined by teardown, so their goroutines
	// disappearing is the proof the cancel was handled -- and that the next frame is handled after it.
	sendVoiceControl(t, ctx, conn, "cancel")
	if !voicePoll(5*time.Second, func() bool { return len(newRepoGoroutines(base, voiceLoopbackGoroutineNeedles...)) == 0 }) {
		t.Fatalf("the cancel left the in-flight dial's goroutines running:\n%s", strings.Join(newRepoGoroutines(base, voiceLoopbackGoroutineNeedles...), "\n\n"))
	}

	// The aborted request must have fully unwound before the redial: the /v1 live handler holds the
	// user's live-session slot until it returns, and a redial that beats the release is refused
	// (transcription.live_busy). The handlers end when the cancelled connection reaches them.
	handlers := []string{"gateway.(*Server).handleBearerLiveTranscription(", "gateway.(*fakeVoiceASRChild).serve("}
	if !voicePoll(5*time.Second, func() bool { return len(newRepoGoroutines(base, handlers...)) == 0 }) {
		t.Fatalf("the cancelled dial's /v1 and child handlers never unwound:\n%s", strings.Join(newRepoGoroutines(base, handlers...), "\n\n"))
	}

	// A fresh loopback is accepted at once from here on. The next voiced frame dials it.
	releaseAccept()
	second := voiceIndexedFrame(2, 320)
	if err := conn.Write(ctx, websocket.MessageBinary, second); err != nil {
		t.Fatalf("send the second frame: %v", err)
	}
	readVoiceUntilFailingOnError(t, ctx, conn, voiceDownASRPartial)
	sendVoiceControl(t, ctx, conn, "push_end")
	if final := readVoiceUntilFailingOnError(t, ctx, conn, voiceDownASRFinal); final.Text != "second ok" {
		t.Fatalf("asr_final = %q, want the fresh loopback's %q", final.Text, "second ok")
	}

	if conns, _ := child.snapshot(); conns != 2 {
		t.Fatalf("the child saw %d loopbacks, want exactly the abandoned one and its replacement", conns)
	}
	if got := voiceChildStream(child, 1); !bytes.Equal(got, first) {
		t.Fatalf("the abandoned loopback carried %d bytes, want only the first frame (%d bytes)", len(got), len(first))
	}
	if got := voiceChildStream(child, 2); !bytes.Equal(got, second) {
		t.Fatalf("the fresh loopback carried %d bytes, want only the second frame (%d bytes): the abandoned turn's PCM bled into it", len(got), len(second))
	}
}

// --- Mute-aware ASR loopback (#335) -------------------------------------------------------
//
// A muted mic sends the gateway no PCM, so an open loopback starves and audio.cpp aborts its body
// ("live request body stalled: no data within the idle timeout"). The portal now tells the gateway
// about the mute (mic_mute / mic_unmute); while muted the machine feeds the loopback zeroed
// silence itself, discards what the ASR says (a model can hallucinate text on silence), and --
// after the operator's mute teardown -- tears the loopback down gracefully. The tests below
// drive that end to end against the idle-modelling fake child, and pin the run-loop timer
// discipline (one owner, every timer stopped on unmute, teardown and session end) on the machine.

// muteSilenceFrameBytes is the keepalive frame the machine feeds for the harness's PCM shape: 100 ms
// of 16 kHz mono s16le, the cadence of the browser's own batches.
const muteSilenceFrameBytes = 3200

// muteTestTiming is turnGraceTiming plus a fast keepalive tick and a mute teardown no test reaches
// unless it overrides it. Chunks end only on push_end (the silence timer is 10s).
func muteTestTiming() voiceTiming {
	t := turnGraceTiming()
	t.muteKeepaliveEvery = 25 * time.Millisecond
	t.muteTeardown = 30 * time.Second
	return t
}

// voiceFrameLog reads every down-frame of a session on its own goroutine and records them, so a
// test can assert what did NOT arrive: a blocking conn.Read cannot be polled, because cancelling its
// context closes the connection. It ends with the connection.
type voiceFrameLog struct {
	mu     sync.Mutex
	frames []voiceDownMsg
}

func startVoiceFrameLog(ctx context.Context, conn *websocket.Conn) *voiceFrameLog {
	l := &voiceFrameLog{}
	go func() {
		for {
			typ, data, err := conn.Read(ctx)
			if err != nil {
				return
			}
			var m voiceDownMsg
			if typ != websocket.MessageText || json.Unmarshal(data, &m) != nil {
				continue
			}
			l.mu.Lock()
			l.frames = append(l.frames, m)
			l.mu.Unlock()
		}
	}()
	return l
}

// count is how many frames of typ have arrived so far.
func (l *voiceFrameLog) count(typ string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, f := range l.frames {
		if f.Type == typ {
			n++
		}
	}
	return n
}

// requireNone fails the test if any frame of typ has arrived, naming what the session sent.
func (l *voiceFrameLog) requireNone(t *testing.T, typ, why string) {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, f := range l.frames {
		if f.Type == typ {
			t.Fatalf("%s: got a %q frame %+v", why, typ, f)
		}
	}
}

// requireVoiceSilence asserts stream is a whole number of zeroed keepalive frames and returns how
// many: the machine's silence is never split, never shorter than a frame and never non-zero.
func requireVoiceSilence(t *testing.T, stream []byte) int {
	t.Helper()
	if len(stream)%muteSilenceFrameBytes != 0 {
		t.Fatalf("the child received %d bytes after the real audio, not a whole number of %d-byte silence frames", len(stream), muteSilenceFrameBytes)
	}
	for i, b := range stream {
		if b != 0 {
			t.Fatalf("byte %d of the post-mute stream is %#x: keepalive silence must be zeroed PCM", i, b)
		}
	}
	return len(stream) / muteSilenceFrameBytes
}

// muteSession is a harness with one session that has an open, accepted, voiced loopback: one real
// frame sent, its asr_partial read. base is a goroutine snapshot from before the session, so the
// loopback's goroutines can be counted as the ones the test caused.
type muteSession struct {
	h     *voiceTurnHarness
	child *fakeVoiceASRChild
	conn  *websocket.Conn
	ctx   context.Context
	base  map[int]string
	real  []byte // the one real PCM frame the loopback carried before any mute
}

func newMuteSession(t *testing.T, child *fakeVoiceASRChild, timing voiceTiming) *muteSession {
	t.Helper()
	return newMuteSessionWith(t, child, voiceTurnOpts{timing: timing})
}

func newMuteSessionWith(t *testing.T, child *fakeVoiceASRChild, opts voiceTurnOpts) *muteSession {
	t.Helper()
	s := &muteSession{child: child, base: goroutineStacks(), real: voiceIndexedFrame(1, 320)}
	s.h = newVoiceTurnHarness(t, child, opts)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	s.ctx = ctx
	s.conn = s.h.dial(t, ctx)
	readVoiceUntilState(t, ctx, s.conn, voiceStateListening)
	if err := s.conn.Write(ctx, websocket.MessageBinary, s.real); err != nil {
		t.Fatalf("send the real frame: %v", err)
	}
	readVoiceUntil(t, ctx, s.conn, voiceDownASRPartial)
	if len(newRepoGoroutines(s.base, "gateway.voiceLoopbackPump(")) != 1 {
		t.Fatal("no loopback is open after the first voiced frame")
	}
	return s
}

// loopbackGone reports whether the loopback is fully unwound: its own goroutines (pump, dial,
// relay) AND the far side's handlers (the /v1 live handler that holds the user's live-session slot,
// and the child's request handler) have returned.
func (s *muteSession) loopbackGone() bool {
	needles := append([]string{"gateway.(*Server).handleBearerLiveTranscription(", "gateway.(*fakeVoiceASRChild).serve("}, voiceLoopbackGoroutineNeedles...)
	return len(newRepoGoroutines(s.base, needles...)) == 0
}

// loopbackOpen reports whether the loopback's pump and the child's handler are both still running.
func (s *muteSession) loopbackOpen() bool {
	return len(newRepoGoroutines(s.base, "gateway.voiceLoopbackPump(")) == 1 &&
		len(newRepoGoroutines(s.base, "gateway.(*fakeVoiceASRChild).serve(")) == 1
}

// TestVoiceMuteKeepaliveSilenceOutlivesTheChildIdleTimeout is the #335 bug itself: the mic is
// muted, the browser sends nothing, and the child -- which aborts a body that sees no data for its
// idle timeout -- would stall the loopback out. With mic_mute the machine feeds silence of its own,
// so the child keeps receiving data across several idle windows, the loopback stays open on its one
// connection, nothing audio-shaped comes from the browser, and no error frame reaches the client.
//
// It proves the BODY side only: the harness leaves the gateway edge's SSE idle watchdog off and the
// child answers every read, so that watchdog never fires here. The SSE side -- a child that says
// nothing on silence -- is TestVoiceMuteKeepaliveLoopbackCutByTheEdgeWatchdogStaysQuiet.
func TestVoiceMuteKeepaliveSilenceOutlivesTheChildIdleTimeout(t *testing.T) {
	child := newFakeVoiceASRChild(t)
	child.idleTimeout = 400 * time.Millisecond
	s := newMuteSession(t, child, muteTestTiming())
	log := startVoiceFrameLog(s.ctx, s.conn)

	sendVoiceControl(t, s.ctx, s.conn, "mic_mute")

	// Run across three of the child's idle windows. Without the keepalive the child aborts the body
	// at the first one and the stall comes back as an error frame.
	deadline := time.Now().Add(3 * child.idleTimeout)
	for time.Now().Before(deadline) {
		log.requireNone(t, voiceDownError, "the muted loopback stalled out")
		time.Sleep(20 * time.Millisecond)
	}
	log.requireNone(t, voiceDownError, "the muted loopback stalled out")

	if conns, _ := child.snapshot(); conns != 1 {
		t.Fatalf("the child saw %d loopbacks, want the one the muted session kept alive", conns)
	}
	if !s.loopbackOpen() {
		t.Fatal("the loopback is gone: the keepalive did not hold it open across the child's idle windows")
	}
	stream := voiceChildStream(child, 1)
	if !bytes.HasPrefix(stream, s.real) {
		t.Fatalf("the child's stream does not start with the one real frame the browser sent")
	}
	// Whole zeroed frames, a few per idle window: the browser sent only the real frame.
	if n := requireVoiceSilence(t, stream[len(s.real):]); n < 8 {
		t.Fatalf("the child received only %d silence frames across three idle windows at a 25ms tick, want at least 8", n)
	}
}

// TestVoiceMuteDiscardsASROutputWhileMuted pins the hallucination guard on the wire: the child
// answers every silence frame with a partial (a model inventing text on silence), and none of it may
// reach the client as an asr_partial/asr_final nor start a chunk (no state change).
func TestVoiceMuteDiscardsASROutputWhileMuted(t *testing.T) {
	child := newFakeVoiceASRChild(t)
	s := newMuteSession(t, child, muteTestTiming())
	log := startVoiceFrameLog(s.ctx, s.conn)

	sendVoiceControl(t, s.ctx, s.conn, "mic_mute")
	// Wait until the child has demonstrably been fed (and so answered) several silence frames.
	if !voicePoll(5*time.Second, func() bool { return len(voiceChildStream(child, 1)) >= len(s.real)+5*muteSilenceFrameBytes }) {
		t.Fatal("the muted loopback was not fed silence")
	}
	// Give the last partials time to cross the relay: a frame they produced would be in the log by now.
	time.Sleep(150 * time.Millisecond)

	log.requireNone(t, voiceDownASRPartial, "the ASR's output on silence reached the client while muted")
	log.requireNone(t, voiceDownASRFinal, "an ASR final reached the client while muted")
	log.requireNone(t, voiceDownError, "muting must not fail the turn")
	log.requireNone(t, voiceDownState, "muting must not move the session out of listening")
}

// TestVoiceMuteTeardownClosesLoopbackCleanly: after the operator's mute teardown a still-muted mic
// has its loopback torn down -- no more silence, the pump/dial/relay and both server-side handlers
// returned, the user's live-session slot freed -- with no error frame, no state change, and no
// redial of its own.
func TestVoiceMuteTeardownClosesLoopbackCleanly(t *testing.T) {
	child := newFakeVoiceASRChild(t)
	timing := muteTestTiming()
	timing.muteTeardown = 200 * time.Millisecond
	s := newMuteSession(t, child, timing)
	log := startVoiceFrameLog(s.ctx, s.conn)

	muted := time.Now()
	sendVoiceControl(t, s.ctx, s.conn, "mic_mute")
	if !voicePoll(10*time.Second, s.loopbackGone) {
		t.Fatalf("the loopback was not torn down after the mute teardown:\n%s", strings.Join(newRepoGoroutines(s.base, voiceLoopbackGoroutineNeedles...), "\n\n"))
	}
	if since := time.Since(muted); since < 150*time.Millisecond {
		t.Fatalf("the loopback was torn down %v after the mute, well before the 200ms teardown", since)
	}

	// Nothing keeps feeding a dead loopback, nothing redials one, and the client heard nothing.
	_, before := child.snapshot()
	time.Sleep(200 * time.Millisecond) // eight keepalive ticks
	if conns, after := child.snapshot(); conns != 1 || after != before {
		t.Fatalf("after the teardown the child saw %d loopbacks and %d new bytes, want 1 and none", conns, after-before)
	}
	log.requireNone(t, voiceDownError, "a mute teardown is graceful")
	log.requireNone(t, voiceDownState, "a mute teardown must leave the session where it was")
}

// TestVoiceMuteTeardownEndsWithACleanEndOfInput pins that a mute teardown is a normal end, not an
// abort. A sustained mute is the common case (every mute past the 10s default), so an aborted body
// would loud-zero each one (ADR-041): an ERROR "live transcription billed zero" log and a
// zero-quantity usage row. The teardown instead closes the feed like a finished chunk does: the child
// sees the terminating chunk (a clean end-of-input), /v1's relay completes and bills what the loopback
// carried, and nothing is logged at Error.
func TestVoiceMuteTeardownEndsWithACleanEndOfInput(t *testing.T) {
	logs, restore := withCapturedSlog(t)
	t.Cleanup(restore)
	child := newFakeVoiceASRChild(t)
	timing := muteTestTiming()
	timing.muteTeardown = 150 * time.Millisecond
	s := newMuteSession(t, child, timing)
	log := startVoiceFrameLog(s.ctx, s.conn)

	sendVoiceControl(t, s.ctx, s.conn, "mic_mute")
	if !voicePoll(10*time.Second, s.loopbackGone) {
		t.Fatal("the loopback was not torn down after the mute teardown")
	}

	if got := child.cleanEnds(); got != 1 {
		t.Fatalf("the child's request body ended cleanly %d times, want 1: the mute teardown aborted the body instead of ending its input", got)
	}
	// The /v1 row is recorded when its handler returns, which loopbackGone has waited for.
	row := onlyUsageEvent(t, s.h.srv)
	if row.BillingUnit != usage.BillingUnitAudioSecond || row.BillingQuantity <= 0 || row.Status != "success" {
		t.Fatalf("the loopback's usage row = %v %q status %q, want the audio it carried billed as a normal end (a zero quantity is the abort's loud zero)",
			row.BillingQuantity, row.BillingUnit, row.Status)
	}
	for _, rec := range errorLogs(logs) {
		if strings.Contains(rec.Msg, "billed zero") {
			t.Fatalf("the mute teardown was logged as a loud zero: %q", rec.Msg)
		}
	}
	log.requireNone(t, voiceDownError, "a mute teardown is graceful")
	log.requireNone(t, voiceDownState, "a mute teardown must leave the session where it was")
}

// TestVoiceMuteKeepaliveLoopbackCutByTheEdgeWatchdogStaysQuiet: the keepalive's silence satisfies
// audio.cpp's BODY idle timeout, but the loopback goes through the gateway's own /v1, whose edge
// watchdog (StreamIdleTimeout, 120s in production) is re-armed only by SSE from the child. A model
// that answers silence with nothing therefore has its muted keepalive loopback cut by the gateway
// itself. With the operator's keepalive nothing else ends it, so this is the path that used to turn
// into a turn-killing error frame for a mic the user had muted. It must instead be a quiet teardown:
// no error frame, no state change, and the first real frame after the unmute reopens a loopback.
func TestVoiceMuteKeepaliveLoopbackCutByTheEdgeWatchdogStaysQuiet(t *testing.T) {
	child := newFakeVoiceASRChild(t)
	timing := muteTestTiming()
	timing.muteKeepalive = true // never torn down by the machine: only the edge can end it
	s := newMuteSessionWith(t, child, voiceTurnOpts{timing: timing, streamIdle: 300 * time.Millisecond})
	log := startVoiceFrameLog(s.ctx, s.conn)

	// The first partial has been answered (it armed the edge watchdog to its idle window); from now
	// on the ASR says nothing, whatever it is fed.
	child.silent.Store(true)
	sendVoiceControl(t, s.ctx, s.conn, "mic_mute")
	if !voicePoll(10*time.Second, s.loopbackGone) {
		t.Fatal("the edge watchdog never cut the silent loopback (or the machine never tore it down)")
	}
	time.Sleep(100 * time.Millisecond) // a frame the cut produced would be on the wire by now
	log.requireNone(t, voiceDownError, "the edge watchdog's cut of a muted loopback reached the client")
	log.requireNone(t, voiceDownState, "the cut must leave the session where it was (listening), not idle")

	// The session is intact: unmute and speak, and a fresh loopback carries it.
	child.silent.Store(false)
	sendVoiceControl(t, s.ctx, s.conn, "mic_unmute")
	resumed := voiceIndexedFrame(2, 320)
	if err := s.conn.Write(s.ctx, websocket.MessageBinary, resumed); err != nil {
		t.Fatalf("send the resumed frame: %v", err)
	}
	if !voicePoll(10*time.Second, func() bool { return log.count(voiceDownASRPartial) >= 1 }) {
		t.Fatal("no asr_partial after the unmute: the real frame did not reopen a loopback")
	}
	if conns, _ := child.snapshot(); conns != 2 {
		t.Fatalf("the child saw %d loopbacks, want the cut one and its replacement", conns)
	}
	log.requireNone(t, voiceDownError, "the quiet teardown must not surface later either")
}

// TestVoiceMuteKeepaliveHoldsTheLoopbackOpenIndefinitely: with the operator's keepalive (the
// knob's explicit 0) the loopback is never torn down while muted, however long. muteTeardown is set
// SHORT on purpose: it must be ignored, and a gateway that read it would tear down at 50ms.
func TestVoiceMuteKeepaliveHoldsTheLoopbackOpenIndefinitely(t *testing.T) {
	child := newFakeVoiceASRChild(t)
	child.idleTimeout = 300 * time.Millisecond
	timing := muteTestTiming()
	timing.muteKeepalive = true
	timing.muteTeardown = 50 * time.Millisecond
	s := newMuteSession(t, child, timing)
	log := startVoiceFrameLog(s.ctx, s.conn)

	sendVoiceControl(t, s.ctx, s.conn, "mic_mute")
	time.Sleep(1200 * time.Millisecond) // 24x the short teardown, 4x the child's idle window
	log.requireNone(t, voiceDownError, "the keepalive loopback stalled or was torn down")
	if !s.loopbackOpen() {
		t.Fatal("the loopback was torn down although the operator asked for keepalive")
	}
	if conns, _ := child.snapshot(); conns != 1 {
		t.Fatalf("the child saw %d loopbacks, want the one kept alive", conns)
	}

	// Unmuting resumes real audio on that SAME loopback.
	sendVoiceControl(t, s.ctx, s.conn, "mic_unmute")
	resumed := voiceIndexedFrame(2, 320)
	if err := s.conn.Write(s.ctx, websocket.MessageBinary, resumed); err != nil {
		t.Fatalf("send the resumed frame: %v", err)
	}
	if !voicePoll(5*time.Second, func() bool { return bytes.HasSuffix(voiceChildStream(child, 1), resumed) }) {
		t.Fatal("the resumed real frame never reached the kept-alive loopback")
	}
	if conns, _ := child.snapshot(); conns != 1 {
		t.Fatalf("unmuting redialled: the child saw %d loopbacks, want 1", conns)
	}
}

// TestVoiceMuteQuickUnmuteKeepsLoopbackAndStopsTimers: an unmute inside the teardown window keeps
// the open loopback (no redial) and disarms BOTH timers -- the keepalive stops feeding silence and
// the teardown no longer fires, however long the mic then stays unmuted with the loopback idle.
func TestVoiceMuteQuickUnmuteKeepsLoopbackAndStopsTimers(t *testing.T) {
	child := newFakeVoiceASRChild(t)
	timing := muteTestTiming()
	timing.muteTeardown = 400 * time.Millisecond
	s := newMuteSession(t, child, timing)
	log := startVoiceFrameLog(s.ctx, s.conn)

	sendVoiceControl(t, s.ctx, s.conn, "mic_mute")
	if !voicePoll(5*time.Second, func() bool { return len(voiceChildStream(child, 1)) >= len(s.real)+2*muteSilenceFrameBytes }) {
		t.Fatal("the muted loopback was not fed silence")
	}
	sendVoiceControl(t, s.ctx, s.conn, "mic_unmute")

	// Unmuted: the silence stops. A tick already queued when the unmute landed may still go out, so
	// let the stream settle, then it must not move.
	var settled []byte
	if !voicePoll(5*time.Second, func() bool {
		a := voiceChildStream(child, 1)
		time.Sleep(100 * time.Millisecond) // four would-be ticks
		b := voiceChildStream(child, 1)
		settled = b
		return bytes.Equal(a, b)
	}) {
		t.Fatal("the child kept receiving silence after mic_unmute: the keepalive was not stopped")
	}
	// Well past the 400ms teardown with the mic unmuted and the loopback idle: still open.
	time.Sleep(700 * time.Millisecond)
	if !s.loopbackOpen() {
		t.Fatal("the loopback was torn down after mic_unmute: the teardown timer was not stopped")
	}
	if got := voiceChildStream(child, 1); !bytes.Equal(got, settled) {
		t.Fatalf("the child received %d more bytes while unmuted and idle", len(got)-len(settled))
	}

	// And it is live: a real frame goes into the same loopback and is transcribed.
	next := voiceIndexedFrame(2, 320)
	if err := s.conn.Write(s.ctx, websocket.MessageBinary, next); err != nil {
		t.Fatalf("send the next real frame: %v", err)
	}
	if !voicePoll(5*time.Second, func() bool { return bytes.HasSuffix(voiceChildStream(child, 1), next) }) {
		t.Fatal("the real frame after the unmute never reached the loopback")
	}
	if !voicePoll(5*time.Second, func() bool { return log.count(voiceDownASRPartial) >= 1 }) {
		t.Fatal("no asr_partial after the unmute: the ASR filter was not lifted")
	}
	if conns, _ := child.snapshot(); conns != 1 {
		t.Fatalf("the child saw %d loopbacks, want the one the quick unmute kept", conns)
	}
	log.requireNone(t, voiceDownError, "a quick mute/unmute is invisible")
}

// TestVoiceMuteUnmuteAfterTeardownReopensLoopback: when the mute outlasted the teardown the unmute
// does not redial by itself -- the NEXT real frame does, lazily (the existing onPCM behavior) -- and
// the fresh loopback carries only that frame and finishes the turn with its own final.
func TestVoiceMuteUnmuteAfterTeardownReopensLoopback(t *testing.T) {
	child := newFakeVoiceASRChild(t)
	child.finals = []string{"unused", "resumed ok"} // connection 1 is torn down by the mute, 2 completes
	timing := muteTestTiming()
	timing.muteTeardown = 150 * time.Millisecond
	s := newMuteSession(t, child, timing)

	sendVoiceControl(t, s.ctx, s.conn, "mic_mute")
	// Fully unwound, including the /v1 handler that holds the live-session slot: a redial that beats
	// its release is refused (transcription.live_busy), as in TestVoiceCancelMidDialThenRedial.
	if !voicePoll(10*time.Second, s.loopbackGone) {
		t.Fatal("the loopback was not torn down after the mute teardown")
	}
	sendVoiceControl(t, s.ctx, s.conn, "mic_unmute")
	if conns, _ := child.snapshot(); conns != 1 {
		t.Fatalf("unmuting redialled by itself: the child saw %d loopbacks, want it to wait for audio", conns)
	}

	resumed := voiceIndexedFrame(2, 320)
	if err := s.conn.Write(s.ctx, websocket.MessageBinary, resumed); err != nil {
		t.Fatalf("send the resumed frame: %v", err)
	}
	readVoiceUntilFailingOnError(t, s.ctx, s.conn, voiceDownASRPartial)
	sendVoiceControl(t, s.ctx, s.conn, "push_end")
	if final := readVoiceUntilFailingOnError(t, s.ctx, s.conn, voiceDownASRFinal); final.Text != "resumed ok" {
		t.Fatalf("asr_final = %q, want the fresh loopback's %q", final.Text, "resumed ok")
	}
	if conns, _ := child.snapshot(); conns != 2 {
		t.Fatalf("the child saw %d loopbacks, want the torn-down one and its replacement", conns)
	}
	if got := voiceChildStream(child, 2); !bytes.Equal(got, resumed) {
		t.Fatalf("the fresh loopback carried %d bytes, want only the resumed frame (%d bytes)", len(got), len(resumed))
	}
}

// TestVoiceMuteStaleFrameOpeningALoopbackIsStillManaged: the read loop forwards PCM and controls on
// separate channels, so a real frame sent just before the mute can be taken AFTER it. If that frame
// opens a loopback while muted, the loopback is not an orphan: it is kept alive, its ASR output is
// discarded, and it is torn down on the same mute teardown.
func TestVoiceMuteStaleFrameOpeningALoopbackIsStillManaged(t *testing.T) {
	child := newFakeVoiceASRChild(t)
	timing := muteTestTiming()
	timing.muteTeardown = 200 * time.Millisecond
	base := goroutineStacks()
	h := newVoiceTurnHarness(t, child, voiceTurnOpts{timing: timing})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn := h.dial(t, ctx)
	readVoiceUntilState(t, ctx, conn, voiceStateListening)
	log := startVoiceFrameLog(ctx, conn)

	sendVoiceControl(t, ctx, conn, "mic_mute") // no loopback yet: nothing to keep alive or tear down
	if err := conn.Write(ctx, websocket.MessageBinary, voiceIndexedFrame(1, 320)); err != nil {
		t.Fatalf("send the stale frame: %v", err)
	}
	if !voicePoll(5*time.Second, func() bool { return len(newRepoGoroutines(base, "gateway.voiceLoopbackPump(")) == 1 }) {
		t.Fatal("the frame did not open a loopback")
	}
	// Torn down by the mute teardown, armed when the loopback opened.
	handlers := []string{"gateway.(*Server).handleBearerLiveTranscription(", "gateway.(*fakeVoiceASRChild).serve("}
	if !voicePoll(10*time.Second, func() bool {
		return len(newRepoGoroutines(base, append(handlers, voiceLoopbackGoroutineNeedles...)...)) == 0
	}) {
		t.Fatal("a loopback opened while muted was never torn down: it is an orphan")
	}
	log.requireNone(t, voiceDownASRPartial, "the ASR output of a frame taken while muted reached the client")
	log.requireNone(t, voiceDownError, "the orphan guard must be graceful")
}

// --- Mute timers on the machine (#335) ----------------------------------------------------

// muteUnitMachine is a bare machine with a stubbed loopback (it exists: turnCancel is set, and its
// pump is a queue the test reads), so the mute handlers can be driven without a network. The test is
// the machine's only goroutine, as the run loop is in production. torn counts turnCancel calls.
func muteUnitMachine(timing voiceTiming) (m *voiceMachine, queue chan []byte, torn *int) {
	ctx, cancel := context.WithCancel(context.Background())
	queue = make(chan []byte, voiceLoopbackPCMQueue)
	torn = new(int)
	m = &voiceMachine{
		ctx: ctx, cancel: cancel, timing: timing,
		claims: voiceTicketClaims{SampleRate: 16000, Channels: 1, SampleFormat: "s16le"},
		writer: newVoiceWriter(&fakeVoiceSocket{}, cancel), // never run: frames just sit in its queue
		state:  voiceStateListening, turnCancel: func() { *torn++ }, pcmOut: queue,
	}
	return m, queue, torn
}

// requireMuteTimers asserts whether the machine's two mute timers are armed.
func requireMuteTimers(t *testing.T, m *voiceMachine, keepalive, teardown bool, why string) {
	t.Helper()
	if got := m.muteKeepC != nil && m.muteKeepTicker != nil; got != keepalive {
		t.Fatalf("%s: keepalive ticker armed = %v, want %v", why, got, keepalive)
	}
	if got := m.muteTeardownC != nil && m.muteTeardownTimer != nil; got != teardown {
		t.Fatalf("%s: teardown timer armed = %v, want %v", why, got, teardown)
	}
}

// TestVoiceMutedASREventsAreDiscarded: while muted every transcript event is dropped before it can
// touch the turn -- no pendingText, no chunk, no silence/max timer, no asr_* down-frame -- in the
// continuation window (thinking) as much as in listening, where a phantom partial would otherwise
// cancel the speculative run. Unmuted the very same events are processed, so the guard is the mute
// and not a broken handler. (An upstream ERROR is a loopback end, not transcript text:
// TestVoiceMutedLoopbackEndsAreQuiet.)
func TestVoiceMutedASREventsAreDiscarded(t *testing.T) {
	events := []asrEvent{
		{kind: asrKindDelta, text: "Thank you."},
		{kind: asrKindPartial, text: "Thank you."},
		{kind: asrKindDone, text: "Thanks for watching."},
	}
	for _, state := range []string{voiceStateListening, voiceStateThinking} {
		for _, ev := range events {
			t.Run(fmt.Sprintf("%s/kind%d", state, ev.kind), func(t *testing.T) {
				m, _, _ := muteUnitMachine(fastVoiceTiming())
				m.state = state
				m.micMuted = true
				m.onASREvent(ev)
				if m.pendingText != "" || m.chunkText != "" || m.chunkActive || m.silenceTimer != nil || m.maxTimer != nil {
					t.Fatalf("a muted %v event reached the turn: pendingText=%q chunkText=%q chunkActive=%v", ev.kind, m.pendingText, m.chunkText, m.chunkActive)
				}
				if n := len(m.writer.out); n != 0 {
					t.Fatalf("a muted %v event produced %d down-frames, want none", ev.kind, n)
				}

				// The same event unmuted is processed. A delta/partial streams an asr_partial; a final
				// ends the chunk into a speculative run, which needs a whole server, so the end-to-end
				// tests carry that half and the delta/partial cases carry the contrast here.
				m, _, _ = muteUnitMachine(fastVoiceTiming())
				m.state = state
				defer m.stopAllTimers()
				if ev.kind == asrKindDone {
					return
				}
				m.onASREvent(ev)
				if m.chunkText != ev.text || len(m.writer.out) == 0 {
					t.Fatalf("the unmuted %v event was not processed: chunkText=%q frames=%d", ev.kind, m.chunkText, len(m.writer.out))
				}
			})
		}
	}
}

// TestVoiceMuteTimersFollowTheLoopback walks the timers through their whole life on the machine:
// armed by mic_mute only while a loopback exists, keepalive-only for the operator's keepalive, never
// re-armed by a repeated mic_mute (the teardown clock would slide), stopped by mic_unmute and by the
// turn's own teardown, and a keepalive tick feeding exactly the ticket-shaped zeroed frame.
func TestVoiceMuteTimersFollowTheLoopback(t *testing.T) {
	tick := func(t *testing.T, m *voiceMachine) {
		t.Helper()
		select {
		case <-m.muteKeepC:
		case <-time.After(5 * time.Second):
			t.Fatal("the keepalive ticker never fired")
		}
		m.onMuteKeepalive()
	}
	timing := fastVoiceTiming()
	timing.muteKeepaliveEvery = 10 * time.Millisecond
	timing.muteTeardown = time.Hour

	t.Run("mute arms both, tick feeds one silence frame", func(t *testing.T) {
		m, queue, _ := muteUnitMachine(timing)
		defer m.stopAllTimers()
		m.onControl(voiceUpControl{Type: voiceUpMicMute})
		if !m.micMuted {
			t.Fatal("mic_mute did not set micMuted")
		}
		requireMuteTimers(t, m, true, true, "after mic_mute")
		tick(t, m)
		select {
		case frame := <-queue:
			if len(frame) != muteSilenceFrameBytes {
				t.Fatalf("keepalive frame is %d bytes, want %d (100ms of 16kHz mono s16le)", len(frame), muteSilenceFrameBytes)
			}
			if !bytes.Equal(frame, make([]byte, muteSilenceFrameBytes)) {
				t.Fatal("the keepalive frame is not zeroed PCM")
			}
		default:
			t.Fatal("a keepalive tick fed nothing into the loopback queue")
		}
	})

	t.Run("operator keepalive arms no teardown", func(t *testing.T) {
		keep := timing
		keep.muteKeepalive = true
		keep.muteTeardown = time.Millisecond // must be ignored
		m, _, _ := muteUnitMachine(keep)
		defer m.stopAllTimers()
		m.onControl(voiceUpControl{Type: voiceUpMicMute})
		requireMuteTimers(t, m, true, false, "keepalive mode")
	})

	t.Run("no loopback arms nothing", func(t *testing.T) {
		m, _, _ := muteUnitMachine(timing)
		m.turnCancel, m.pcmOut = nil, nil
		m.onControl(voiceUpControl{Type: voiceUpMicMute})
		if !m.micMuted {
			t.Fatal("mic_mute did not set micMuted without a loopback")
		}
		requireMuteTimers(t, m, false, false, "mute with no loopback")
	})

	t.Run("a repeated mute does not slide the teardown clock", func(t *testing.T) {
		m, _, _ := muteUnitMachine(timing)
		defer m.stopAllTimers()
		m.onControl(voiceUpControl{Type: voiceUpMicMute})
		keepTicker, teardownTimer := m.muteKeepTicker, m.muteTeardownTimer
		m.onControl(voiceUpControl{Type: voiceUpMicMute})
		if m.muteKeepTicker != keepTicker || m.muteTeardownTimer != teardownTimer {
			t.Fatal("a second mic_mute re-armed the timers: the mute teardown would restart")
		}
	})

	t.Run("unmute stops both and lifts the mute", func(t *testing.T) {
		m, queue, torn := muteUnitMachine(timing)
		m.onControl(voiceUpControl{Type: voiceUpMicMute})
		m.onControl(voiceUpControl{Type: voiceUpMicUnmute})
		if m.micMuted {
			t.Fatal("mic_unmute left micMuted set")
		}
		requireMuteTimers(t, m, false, false, "after mic_unmute")
		if *torn != 0 || m.turnCancel == nil || len(queue) != 0 {
			t.Fatalf("mic_unmute touched the loopback: torn=%d turnCancel set=%v queued=%d", *torn, m.turnCancel != nil, len(queue))
		}
		m.onControl(voiceUpControl{Type: voiceUpMicUnmute}) // an unmute with no mute is a no-op
		requireMuteTimers(t, m, false, false, "a stray mic_unmute")
	})

	t.Run("the turn's own teardown stops them, mute persists", func(t *testing.T) {
		m, _, _ := muteUnitMachine(timing)
		m.onControl(voiceUpControl{Type: voiceUpMicMute})
		m.teardownTurn() // e.g. the continuation window closing under a muted mic
		requireMuteTimers(t, m, false, false, "after the turn's teardown")
		if !m.micMuted {
			t.Fatal("a turn teardown cleared the mute: the next loopback would not be filtered")
		}
	})

	t.Run("a stray tick without a loopback feeds nothing and disarms itself", func(t *testing.T) {
		m, queue, _ := muteUnitMachine(timing)
		m.onControl(voiceUpControl{Type: voiceUpMicMute})
		m.turnCancel, m.pcmOut = nil, nil
		m.onMuteKeepalive()
		if len(queue) != 0 {
			t.Fatal("a tick with no loopback fed a frame")
		}
		requireMuteTimers(t, m, false, false, "a tick with nothing to keep alive")
	})
}

// TestVoiceMuteTeardownTimerFires drives the machine's own teardown handler. It ends a still-muted
// loopback CLEANLY, not by abort: the feed is closed (the pump's clean end-of-input), the keepalive
// stops, the in-progress chunk is dropped (a half-built chunk whose loopback is going must not seed
// the next one), and the loopback itself is torn down only once the relay reports the child's clean
// end (onASRClosed) -- or the finalize bound expires -- all of it silent: no error frame, no state
// change, mute kept. An unmute in the meantime tears the half-closed loopback down at once. It skips
// a loopback whose chunk is already finalizing (finishChunk tears it down, with the clean final) and
// is inert once the mic is unmuted.
func TestVoiceMuteTeardownTimerFires(t *testing.T) {
	timing := fastVoiceTiming()
	timing.muteKeepaliveEvery = time.Hour
	timing.muteTeardown = 10 * time.Millisecond
	fire := func(t *testing.T, m *voiceMachine) {
		t.Helper()
		select {
		case <-m.muteTeardownC:
		case <-time.After(5 * time.Second):
			t.Fatal("the teardown timer never fired")
		}
		m.onMuteTeardown()
	}
	// closing builds a thinking-window machine with a half-built chunk, mutes it and fires the teardown.
	closing := func(t *testing.T) (*voiceMachine, chan []byte, *int) {
		t.Helper()
		m, queue, torn := muteUnitMachine(timing)
		m.state = voiceStateThinking // the continuation window: a resumed chunk's loopback is open
		m.chunkActive, m.chunkText = true, "half a chunk"
		m.onControl(voiceUpControl{Type: voiceUpMicMute})
		fire(t, m)
		t.Cleanup(m.stopAllTimers)
		return m, queue, torn
	}
	requireQuiet := func(t *testing.T, m *voiceMachine, why string) {
		t.Helper()
		if m.state != voiceStateThinking || len(m.writer.out) != 0 || !m.micMuted {
			t.Fatalf("%s was not silent: state=%q frames=%d micMuted=%v", why, m.state, len(m.writer.out), m.micMuted)
		}
	}

	t.Run("closes the feed cleanly and waits for the child", func(t *testing.T) {
		m, queue, torn := closing(t)
		if _, open := <-queue; open {
			t.Fatal("the loopback's feed was not closed: the pump would never end the body cleanly")
		}
		if *torn != 0 || m.turnCancel == nil || !m.muteClosing {
			t.Fatalf("the teardown aborted instead of closing: torn=%d turnCancel set=%v muteClosing=%v", *torn, m.turnCancel != nil, m.muteClosing)
		}
		if m.muteKeepC != nil || m.muteTeardownC != nil || m.finalizeC == nil {
			t.Fatalf("timers after the close: keepalive armed=%v teardown armed=%v finalize bound armed=%v, want only the finalize bound",
				m.muteKeepC != nil, m.muteTeardownC != nil, m.finalizeC != nil)
		}
		if m.chunkActive || m.chunkText != "" {
			t.Fatalf("the half-built chunk survived: chunkActive=%v chunkText=%q", m.chunkActive, m.chunkText)
		}
		requireQuiet(t, m, "closing the loopback")
	})

	t.Run("finishes quietly when the child's end arrives", func(t *testing.T) {
		m, _, torn := closing(t)
		// The child's closing transcript is filtered, the relay closing completes the teardown.
		m.onASREvent(asrEvent{kind: asrKindDone, text: "Thanks for watching."})
		if *torn != 0 || m.pendingText != "" {
			t.Fatalf("the filtered closing transcript acted: torn=%d pendingText=%q", *torn, m.pendingText)
		}
		m.onASRClosed()
		if *torn != 1 || m.turnCancel != nil || m.muteClosing || m.finalizeC != nil {
			t.Fatalf("after the relay closed torn=%d turnCancel set=%v muteClosing=%v finalize armed=%v, want the loopback gone",
				*torn, m.turnCancel != nil, m.muteClosing, m.finalizeC != nil)
		}
		requireQuiet(t, m, "the end of the mute teardown")
	})

	t.Run("a child that never answers is cut at the finalize bound", func(t *testing.T) {
		m, _, torn := closing(t)
		m.onFinalizeTimeout()
		if *torn != 1 || m.turnCancel != nil || m.muteClosing {
			t.Fatalf("after the bound torn=%d turnCancel set=%v muteClosing=%v, want the loopback gone", *torn, m.turnCancel != nil, m.muteClosing)
		}
		requireQuiet(t, m, "the finalize bound")
	})

	t.Run("an unmute while closing tears the half-closed loopback down", func(t *testing.T) {
		m, _, torn := closing(t)
		m.onControl(voiceUpControl{Type: voiceUpMicUnmute})
		if *torn != 1 || m.turnCancel != nil || m.muteClosing || m.micMuted {
			t.Fatalf("after the unmute torn=%d turnCancel set=%v muteClosing=%v micMuted=%v, want the loopback gone and the mic live",
				*torn, m.turnCancel != nil, m.muteClosing, m.micMuted)
		}
	})

	t.Run("leaves a finalizing chunk to finishChunk", func(t *testing.T) {
		m, queue, torn := muteUnitMachine(timing)
		m.onControl(voiceUpControl{Type: voiceUpMicMute})
		m.finalizing = true
		fire(t, m)
		if *torn != 0 || m.turnCancel == nil || m.muteClosing || len(queue) != 0 {
			t.Fatalf("the teardown touched a chunk that was awaiting its final: torn=%d muteClosing=%v", *torn, m.muteClosing)
		}
		select {
		case _, open := <-queue:
			if !open {
				t.Fatal("the teardown closed the feed of a finalizing chunk")
			}
		default:
		}
	})

	t.Run("is inert after the unmute", func(t *testing.T) {
		m, queue, torn := muteUnitMachine(timing)
		m.onControl(voiceUpControl{Type: voiceUpMicMute})
		m.micMuted = false // an unmute whose timer fire was already in flight
		m.onMuteTeardown()
		if *torn != 0 || m.muteClosing || len(queue) != 0 {
			t.Fatal("a stale teardown fire acted on an unmuted loopback")
		}
		m.stopAllTimers()
	})
}

// TestVoiceMutedLoopbackEndsAreQuiet: while the mic is muted every way the loopback can end -- the
// relay closing (the gateway edge's idle watchdog cutting a keepalive loopback whose ASR sent no
// SSE), the request body failing under the pump, a failed or refused dial, an ASR error event -- is a
// QUIET teardown: the loopback is torn down and the mute stays, but no error frame goes down and the
// session does not go idle, because the user muted the mic and can neither see nor act on a loopback.
// Unmuted the same ends fail the turn as before (the existing tests cover those), and an end that
// is only a pending dial's pump failure stays inert. A chunk that is finalizing still finishes the
// turn it belongs to.
func TestVoiceMutedLoopbackEndsAreQuiet(t *testing.T) {
	newMuted := func(t *testing.T) (*voiceMachine, *int) {
		m, _, torn := muteUnitMachine(fastVoiceTiming())
		m.micMuted = true
		m.asrCh = make(chan asrEvent) // the dial succeeded: the loopback is established
		m.chunkActive, m.chunkText = true, "half a chunk"
		t.Cleanup(m.stopAllTimers)
		return m, torn
	}
	cases := []struct {
		name string
		end  func(m *voiceMachine)
	}{
		{"relay closed (the edge idle watchdog's cut)", func(m *voiceMachine) { m.onASRClosed() }},
		{"pump failed", func(m *voiceMachine) { m.onLoopbackEvent(voiceLoopbackEvent{kind: loopbackPumpFailed}) }},
		{"dial failed", func(m *voiceMachine) { m.onLoopbackEvent(voiceLoopbackEvent{kind: loopbackDialFailed}) }},
		{"dial refused", func(m *voiceMachine) {
			m.onLoopbackEvent(voiceLoopbackEvent{kind: loopbackRefused, code: "transcription.live_busy", message: "busy"})
		}},
		{"asr error event", func(m *voiceMachine) {
			m.onASREvent(asrEvent{kind: asrKindError, code: "transcription.stream_error", message: "stalled"})
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, torn := newMuted(t)
			tc.end(m)
			if *torn != 1 || m.turnCancel != nil {
				t.Fatalf("the muted loopback was not torn down: torn=%d turnCancel set=%v", *torn, m.turnCancel != nil)
			}
			if m.state != voiceStateListening || len(m.writer.out) != 0 || !m.micMuted {
				t.Fatalf("the end was not quiet: state=%q frames=%d micMuted=%v", m.state, len(m.writer.out), m.micMuted)
			}
			if m.chunkActive || m.chunkText != "" {
				t.Fatalf("the dead loopback's half-built chunk survived: chunkActive=%v chunkText=%q", m.chunkActive, m.chunkText)
			}
		})
	}

	t.Run("a pump failure with the dial still pending stays inert", func(t *testing.T) {
		m, torn := newMuted(t)
		m.asrCh = nil // the dial's own outcome follows and is authoritative
		m.onLoopbackEvent(voiceLoopbackEvent{kind: loopbackPumpFailed})
		if *torn != 0 || m.turnCancel == nil {
			t.Fatalf("a pending dial's pump failure tore the loopback down: torn=%d", *torn)
		}
	})

	t.Run("unmuted the same relay end still fails the turn", func(t *testing.T) {
		m, torn := newMuted(t)
		m.micMuted = false
		m.onASRClosed()
		if *torn != 1 || m.state != voiceStateIdle || len(m.writer.out) == 0 {
			t.Fatalf("unmuted relay end: torn=%d state=%q frames=%d, want the turn failed (1, idle, an error frame)", *torn, m.state, len(m.writer.out))
		}
	})

	t.Run("an error while a chunk is finalizing still fails that turn", func(t *testing.T) {
		m, torn := newMuted(t)
		m.finalizing = true
		m.onASREvent(asrEvent{kind: asrKindError, code: "transcription.stream_error", message: "stalled"})
		if *torn != 1 || m.state != voiceStateIdle || len(m.writer.out) == 0 {
			t.Fatalf("a finalizing chunk's error: torn=%d state=%q frames=%d, want the turn failed with an error frame", *torn, m.state, len(m.writer.out))
		}
	})
}

// TestVoiceMuteRunLoopOwnsTheTimers runs the REAL run loop over a stubbed loopback: mic_mute makes
// it tick silence into the queue; mic_unmute stops that; a muted session that ends leaves no timer
// behind (they are run-loop state, stopped by the loop's own teardown), and a mute teardown fires
// into the loop's select, not from a timer goroutine. Run under -race, it is also the check that no
// machine field is touched off the run loop.
func TestVoiceMuteRunLoopOwnsTheTimers(t *testing.T) {
	timing := fastVoiceTiming()
	timing.muteKeepaliveEvery = 20 * time.Millisecond
	timing.muteTeardown = 300 * time.Millisecond
	m, queue, torn := muteUnitMachine(timing)
	pcmCh := make(chan []byte, voiceInboundQueue)
	ctrlCh := make(chan voiceUpControl, voiceInboundQueue)
	m.pcmCh, m.ctrlCh = pcmCh, ctrlCh
	runDone := make(chan struct{})
	go func() { defer close(runDone); m.run() }()

	ctrlCh <- voiceUpControl{Type: voiceUpMicMute}
	if !voicePoll(5*time.Second, func() bool { return len(queue) >= 3 }) {
		t.Fatalf("the run loop fed only %d keepalive frames after mic_mute", len(queue))
	}

	// Unmute, then a real frame as the barrier: the loop handles it after the unmute's effects.
	ctrlCh <- voiceUpControl{Type: voiceUpMicUnmute}
	if !voicePoll(5*time.Second, func() bool { return len(ctrlCh) == 0 }) {
		t.Fatal("the run loop never took mic_unmute")
	}
	marker := voiceIndexedFrame(7, 64)
	pcmCh <- marker
	for seen := false; !seen; {
		select {
		case f := <-queue:
			seen = bytes.Equal(f, marker)
		case <-time.After(5 * time.Second):
			t.Fatal("the real frame after the unmute never reached the loopback queue")
		}
	}
	// Past the teardown (300ms) and several ticks (20ms): an unmuted loop feeds and tears down nothing.
	time.Sleep(450 * time.Millisecond)
	if len(queue) != 0 || *torn != 0 {
		t.Fatalf("an unmuted run loop kept acting on the mute: %d frames queued, torn=%d", len(queue), *torn)
	}

	// Mute again and let the TEARDOWN fire into the loop's select.
	ctrlCh <- voiceUpControl{Type: voiceUpMicMute}
	if !voicePoll(5*time.Second, func() bool { return len(queue) > 0 }) { // ticking again
		t.Fatal("the second mute never ticked")
	}
	select {
	case <-drainUntilClosed(queue): // the mute teardown closes the feed (a clean end-of-input)
	case <-time.After(5 * time.Second):
		t.Fatal("the mute teardown never fired into the run loop")
	}
	// The stub has no relay to report the child's clean end, so the loopback is still waiting for it:
	// the teardown closed the feed but did not abort. The session ending then aborts it, once.
	if *torn != 0 {
		t.Fatalf("the mute teardown aborted the loopback (turnCancel called %d times) instead of closing it cleanly", *torn)
	}

	m.cancel()
	select {
	case <-runDone:
	case <-time.After(5 * time.Second):
		t.Fatal("the run loop did not exit when its context ended")
	}
	if *torn != 1 {
		t.Fatalf("turnCancel called %d times, want once (the session's exit teardown)", *torn)
	}
	requireMuteTimers(t, m, false, false, "after the session ended")
}

// drainUntilClosed discards frames until the queue is closed and returns a closed channel, so a
// test can wait for the loopback's feed to be closed (its teardown) without a sleep.
func drainUntilClosed(queue <-chan []byte) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			if _, ok := <-queue; !ok {
				return
			}
		}
	}()
	return done
}

// TestVoiceMuteSessionEndWhileMutedStopsTimers: the session ends (the handler context) while muted
// with a live loopback and both timers armed. The run loop's exit path tears the loopback down and
// stops the timers: nothing is left ticking.
func TestVoiceMuteSessionEndWhileMutedStopsTimers(t *testing.T) {
	timing := fastVoiceTiming()
	timing.muteKeepaliveEvery = 20 * time.Millisecond
	timing.muteTeardown = time.Hour
	m, queue, torn := muteUnitMachine(timing)
	pcmCh := make(chan []byte, voiceInboundQueue)
	ctrlCh := make(chan voiceUpControl, voiceInboundQueue)
	m.pcmCh, m.ctrlCh = pcmCh, ctrlCh
	runDone := make(chan struct{})
	go func() { defer close(runDone); m.run() }()

	ctrlCh <- voiceUpControl{Type: voiceUpMicMute}
	if !voicePoll(5*time.Second, func() bool { return len(queue) >= 2 }) {
		t.Fatal("the muted loop never ticked")
	}
	m.cancel()
	select {
	case <-runDone:
	case <-time.After(5 * time.Second):
		t.Fatal("the run loop did not exit when its context ended")
	}
	if *torn != 1 {
		t.Fatalf("turnCancel called %d times on exit, want once", *torn)
	}
	requireMuteTimers(t, m, false, false, "after the session ended while muted")
}

// TestVoiceSilenceFrameFollowsTheTicketShape: the keepalive frame is the ticket's fixed PCM shape
// -- 100 ms of silence, whole sample frames, zero bytes -- for both formats live transcription takes.
func TestVoiceSilenceFrameFollowsTheTicketShape(t *testing.T) {
	cases := []struct {
		rate, channels int
		format         string
		want           int
	}{
		{16000, 1, "s16le", 3200},
		{48000, 2, "s16le", 19200},
		{16000, 1, "f32le", 6400},
		{44100, 1, "s16le", 8820},
		{1000, 1, "s16le", 200},
	}
	for _, tc := range cases {
		got := voiceSilenceFrame(voiceTicketClaims{SampleRate: tc.rate, Channels: tc.channels, SampleFormat: tc.format})
		if len(got) != tc.want || !bytes.Equal(got, make([]byte, tc.want)) {
			t.Errorf("silence frame for %d Hz x%d %s: %d bytes (zeroed=%v), want %d zeroed bytes",
				tc.rate, tc.channels, tc.format, len(got), bytes.Equal(got, make([]byte, len(got))), tc.want)
		}
	}
	if got := voiceSilenceFrame(voiceTicketClaims{}); len(got) != 0 {
		t.Errorf("a claims-less silence frame is %d bytes, want none (nothing to feed)", len(got))
	}
}
