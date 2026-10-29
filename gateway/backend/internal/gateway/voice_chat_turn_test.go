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
	"sync"
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
	chatStore     portal.ChatStore // defaults to a plain in-memory store
	speechPCM     [][]byte         // the PCM chunks the TTS stream carries; defaults to voiceTestPCM
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
// still playing. Here chunk A is relayed, a gap playback_done arrives, THEN chunk B is relayed
// (which clears the remembered signal) and the turn commits; the mic must stay gated until the
// genuine end-of-audio signal, never re-opening on the stale gap signal (echo exposure).
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
	// drain the moment the turn commits.
	sendVoiceControl(t, ctx, conn, "playback_done")
	time.Sleep(500 * time.Millisecond)

	// Release chunk B: relaying it CLEARS the remembered gap signal, then the turn commits. B is
	// published before the run's terminal, so it is relayed -- and the signal cleared -- before the
	// post-commit drain begins.
	close(gate)
	readVoiceUntilState(t, ctx, conn, voiceStateDraining)

	// Past the mic-resume tail: with the fix the mic is still gated (the gap signal was cleared, so
	// the drain waits for the genuine end-of-audio signal); without it the stale gap signal has
	// already re-opened the mic. A burst now must be discarded.
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
