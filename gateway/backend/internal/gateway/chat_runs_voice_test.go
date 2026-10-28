// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/capture"
	"op-ai-gateway/internal/inference"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/provider"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/store"
	"op-ai-gateway/internal/usage"
	"strings"
	"sync"
	"testing"
	"time"
)

// The Voice Chat run kind (chat_runs_voice.go): a speculative text completion whose
// answer is streamed and then synthesized to speech, with ALL chat-document
// persistence deferred to a single finalize so a cancelled run leaves nothing to undo.
//
// The harness runs the LLM leg and the TTS leg through the REAL gateway over the
// loopback (selfBaseURL points at it), so the run's own metering happens exactly as it
// would in production: the chat leg is the mock provider's in-process streamed
// completion (billed by /v1/chat/completions), and the speech leg is a canned event
// stream returned by the provider's ProxyNative (billed by /v1/audio/speech). Nothing
// about billing is special-cased by the voice run -- it is inherited from the legs.

// voiceTestProvider serves BOTH legs of a voice run from one provider: the chat leg
// via the embedded mock's CompleteStream (in-process, streamed, billed), and the
// speech leg via an overridden ProxyNative that answers /v1/audio/speech with a canned
// response. A streamed request (stream:true) gets the SSE event stream the portal's
// composer plays; a buffered one gets a one-shot WAV. Every speech request body is
// captured so a test can assert WHAT text was synthesized (the LLM answer, not the
// user prompt).
type voiceTestProvider struct {
	provider.Mock
	speechFrames string
	speechWAV    []byte
	speechBodies chan []byte
	// speechArrived is closed (once, via speechOnce) the first time a speech request
	// reaches ProxyNative, so a scripted LLM leg can rendezvous with the speech leg firing
	// at </speak>. nil disables the rendezvous (the default, for the non-ordering tests).
	speechArrived chan struct{}
	speechOnce    *sync.Once
	// speechFail makes the speech leg's loopback answer a 500, to drive the speech-leg
	// failure path (the text turn still commits, no audio, degrade surfaced).
	speechFail bool
	// llmScript, when non-nil, REPLACES the mock's echoed answer with these scripted
	// deltas: a delta with waitSpeech blocks after it is emitted until the speech request
	// arrives (or ctx ends), and a delta with failAfter returns an error instead of
	// completing the stream (the LLM-failure path). nil leaves the plain mock answer.
	llmScript []scriptedLLMDelta
}

// scriptedLLMDelta is one step of voiceTestProvider.llmScript.
type scriptedLLMDelta struct {
	text       string
	waitSpeech bool
	failAfter  bool
}

func (p voiceTestProvider) ProxyNative(ctx context.Context, target routing.Target, path string, body []byte, contentType string) (*provider.ProxyResponse, error) {
	if strings.Contains(path, "/audio/speech") {
		select {
		case p.speechBodies <- append([]byte(nil), body...):
		default:
		}
		if p.speechArrived != nil && p.speechOnce != nil {
			p.speechOnce.Do(func() { close(p.speechArrived) })
		}
		if p.speechFail {
			return &provider.ProxyResponse{
				StatusCode: http.StatusInternalServerError,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"error":{"code":"gateway.mock_speech_failed","message":"boom"}}`)),
			}, nil
		}
		if speechBodyWantsStream(body) {
			return &provider.ProxyResponse{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(p.speechFrames)),
			}, nil
		}
		return &provider.ProxyResponse{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"audio/wav"}, speechDurationHeader: []string{"1500"}},
			Body:       io.NopCloser(bytes.NewReader(p.speechWAV)),
		}, nil
	}
	return p.Mock.ProxyNative(ctx, target, path, body, contentType)
}

// CompleteStream drives the LLM leg. With no llmScript it is the plain mock (the answer
// echoes the pending text); with one it emits the scripted deltas, optionally blocking after
// the </speak> delta until the speech leg fires (the rendezvous the ordering test needs) or
// returning an error to drive the LLM-failure-after-close path.
func (p voiceTestProvider) CompleteStream(ctx context.Context, target routing.Target, req inference.Request, emit provider.StreamEmit) error {
	if p.llmScript == nil {
		return p.Mock.CompleteStream(ctx, target, req, emit)
	}
	for _, d := range p.llmScript {
		if d.text != "" {
			if err := emit(inference.StreamEvent{Type: inference.StreamEventTextDelta, Text: d.text}); err != nil {
				return err
			}
		}
		if d.waitSpeech {
			select {
			case <-p.speechArrived:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		if d.failAfter {
			return errors.New("gateway.mock_llm_failed")
		}
	}
	u := inference.Usage{OutputTokens: 3, TotalTokens: 3}
	return emit(inference.StreamEvent{Type: inference.StreamEventCompleted, Usage: &u})
}

// voiceTestTargets are the resolved legs a voice run takes. Only the gateway model
// name of each is read by the run (the loopback body re-resolves the leg), so the
// tests hand just that.
var (
	voiceLLMTarget = routing.Target{Model: "qwen-coder"}
	voiceTTSTarget = routing.Target{Model: speechGatewayModel}
	voiceASRTarget = routing.Target{Model: "asr-model"}
)

// voiceTestPCM is the two PCM chunks the canned speech stream carries; the committed
// WAV is their concatenation under the streaming fixture's rate.
func voiceTestPCM() (first, second []byte) {
	return speechPCM(8000, 1), speechPCM(6000, 2)
}

// newVoiceRunTestServer wires the real gateway for a voice run: the shared chat seed
// (qwen-coder on the mock completion app) for the LLM leg, plus one audio.cpp app with
// a (speech, yes) + (speech_streaming, yes) mapping and a cached output byte rate for
// the TTS leg, all served by a voiceTestProvider. selfBaseURL points at the gateway so
// the run's loopback calls route and bill for real. The seeded chat is EMPTY, so a
// committed turn leaves exactly [user, assistant]. mockDelay paces the chat leg (a
// cancel test needs the LLM in flight when it cancels).
// defaultVoiceProvider is the baseline voiceTestProvider the voice-run tests share: the
// canned streaming speech frames and one-shot WAV, with no rendezvous, no speech failure and
// the plain echoed LLM answer. A test that needs the overlap rendezvous, a speech failure or
// a scripted LLM answer starts here, sets the fields it needs, and passes it to
// newVoiceRunTestServerProv.
func defaultVoiceProvider() voiceTestProvider {
	first, second := voiceTestPCM()
	return voiceTestProvider{
		Mock:         provider.Mock{},
		speechFrames: speechFrameDelta(first) + speechFrameDelta(second) + speechSSEDone + speechSSEEnd,
		speechWAV:    speechTestWAV(),
		speechBodies: make(chan []byte, 8),
	}
}

func newVoiceRunTestServer(t *testing.T, mockDelay time.Duration) (*Server, auth.Token, string, *usage.Recorder, <-chan []byte) {
	t.Helper()
	prov := defaultVoiceProvider()
	prov.Mock.Delay = mockDelay
	return newVoiceRunTestServerProv(t, prov)
}

// newVoiceRunTestServerProv wires the gateway exactly as newVoiceRunTestServer does, but over
// a caller-supplied provider so a test can inject the rendezvous / failure / scripted-answer
// seams above.
func newVoiceRunTestServerProv(t *testing.T, prov voiceTestProvider) (*Server, auth.Token, string, *usage.Recorder, <-chan []byte) {
	t.Helper()

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
	seedGatewayTestRoutes(routeStore, now) // the qwen-coder chat route for the LLM leg

	ctx := context.Background()
	if err := routeStore.CreateAIServer(ctx, routing.AIServer{ID: "srv-audio", Name: "Audio Upstream", Domain: "audio.example.test", Provider: routing.ProviderAudioCpp, Endpoint: "http://audio.example.test:8200", Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateAIServer: %v", err)
	}
	if err := routeStore.CreateApplication(ctx, routing.Application{ID: "app-audio", ServerID: "srv-audio", Type: routing.ProviderAudioCpp, Port: 8200, Scheme: "http", APIFlavors: []string{routing.APIFlavorOpenAIAudio}, Priority: 10, Weight: 50, TimeoutMS: 30000, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateApplication: %v", err)
	}
	if err := routeStore.CreateMapping(ctx, routing.ModelMapping{ID: "route-tts", ApplicationID: "app-audio", GatewayModelName: speechGatewayModel, AppModelName: speechUpstreamModel, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateMapping: %v", err)
	}
	if err := routeStore.UpsertMappingCapabilities(ctx, "route-tts", []routing.CapabilityRow{{Capability: routing.CapabilitySpeech, Verdict: routing.CapabilityYes, Source: routing.CapabilitySourceAudioCppModels, CheckedAt: now}}); err != nil {
		t.Fatalf("UpsertMappingCapabilities: %v", err)
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
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	srv.selfBaseURL = ts.URL

	// The streaming gate the TTS leg checks: (speech_streaming, yes) + a cached rate.
	seedStreamableSpeech(t, srv, "route-tts", speechRunTestByteRate)

	owner := auth.Token{UserID: "usr_dev", Scopes: []string{"gateway:use"}}
	created, err := svc.CreateChat(ctx, owner, portal.CreateChatRequest{Content: json.RawMessage(`{"settings":{},"messages":[]}`)})
	if err != nil {
		t.Fatalf("CreateChat: %v", err)
	}
	return srv, owner, created.ID, recorder, prov.speechBodies
}

// voiceRunOutcome starts a voice run and waits for it to end, returning the run and
// its terminal snapshot.
func voiceRunOutcome(t *testing.T, srv *Server, owner auth.Token, chatID, pendingText string) (*ChatRun, runEvent) {
	t.Helper()
	run, err := srv.startVoiceRun(context.Background(), voiceRunRequest{owner: owner, chatID: chatID, pendingText: pendingText, asr: voiceASRTarget, llm: voiceLLMTarget, tts: voiceTTSTarget})
	if err != nil {
		t.Fatalf("startVoiceRun: %v", err)
	}
	waitFor(t, func() bool { return run.statusValue() != "running" })
	snap, _, unsub := run.subscribe()
	unsub()
	return run, snap
}

// takeSpeechBody returns the one speech request body the provider saw, failing when
// there is none.
func takeSpeechBody(t *testing.T, bodies <-chan []byte) map[string]any {
	t.Helper()
	select {
	case b := <-bodies:
		var fields map[string]any
		if err := json.Unmarshal(b, &fields); err != nil {
			t.Fatalf("speech body is not a JSON object: %v (%s)", err, b)
		}
		return fields
	default:
		t.Fatal("the TTS leg sent no request to /v1/audio/speech")
		return nil
	}
}

// storedMessages decodes the role+content of a stored chat document's messages.
func storedMessages(t *testing.T, content json.RawMessage) []struct {
	Role    string
	Content json.RawMessage
} {
	t.Helper()
	var doc struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(content, &doc); err != nil {
		t.Fatalf("stored document is not JSON: %v", err)
	}
	out := make([]struct {
		Role    string
		Content json.RawMessage
	}, 0, len(doc.Messages))
	for _, m := range doc.Messages {
		out = append(out, struct {
			Role    string
			Content json.RawMessage
		}{m.Role, m.Content})
	}
	return out
}

// On an un-cancelled finish the chat doc gains a user turn carrying the pendingText
// and ONE assistant turn carrying the answer -- committed together, complete.
func TestVoiceRunCommitsUserAndAssistantOnFinish(t *testing.T) {
	srv, owner, chatID, _, speechBodies := newVoiceRunTestServer(t, 0)

	const pending = "What is the capital of France"
	run, snap := voiceRunOutcome(t, srv, owner, chatID, pending)
	if snap.Status != "completed" {
		t.Fatalf("status = %q (err %q), want completed", snap.Status, snap.Err)
	}
	wantAnswer := "Mock response for qwen-coder: " + pending

	stored, err := srv.Portal.GetChat(context.Background(), owner, chatID)
	if err != nil {
		t.Fatal(err)
	}
	msgs := storedMessages(t, stored.Content)
	if len(msgs) != 2 {
		t.Fatalf("stored %d messages, want exactly [user, assistant]: %s", len(msgs), stored.Content)
	}
	if msgs[0].Role != "user" || !strings.Contains(string(msgs[0].Content), pending) {
		t.Fatalf("message 0 = %q %s, want the user turn carrying the pending text", msgs[0].Role, msgs[0].Content)
	}
	if msgs[1].Role != "assistant" || !strings.Contains(string(msgs[1].Content), wantAnswer) {
		t.Fatalf("message 1 = %q %s, want the assistant turn carrying the answer", msgs[1].Role, msgs[1].Content)
	}
	if !strings.Contains(string(stored.Content), `"status":"complete"`) {
		t.Fatalf("the assistant turn must be committed complete: %s", stored.Content)
	}
	// The TTS leg synthesized the ANSWER, not the user prompt.
	if got := takeSpeechBody(t, speechBodies)["input"]; got != wantAnswer {
		t.Fatalf("TTS input = %q, want the LLM answer %q", got, wantAnswer)
	}
	_ = run
}

// A cancel mid-LLM (the orchestrator's resume within the continuation window) persists
// NOTHING: the chat doc is unchanged -- no user turn, no assistant turn -- so
// cancel-and-extend needs no undo.
func TestVoiceRunCancelledPersistsNothing(t *testing.T) {
	// A long chat-leg delay keeps the LLM in flight so the cancel lands mid-run.
	srv, owner, chatID, _, _ := newVoiceRunTestServer(t, 3*time.Second)

	before, err := srv.Portal.GetChat(context.Background(), owner, chatID)
	if err != nil {
		t.Fatal(err)
	}

	run, err := srv.startVoiceRun(context.Background(), voiceRunRequest{owner: owner, chatID: chatID, pendingText: "keep talking", asr: voiceASRTarget, llm: voiceLLMTarget, tts: voiceTTSTarget})
	if err != nil {
		t.Fatalf("startVoiceRun: %v", err)
	}
	if !srv.ChatRuns.cancelChat(owner.UserID, chatID) {
		t.Fatal("cancelChat found no active run to cancel")
	}
	waitFor(t, func() bool { return run.statusValue() != "running" })
	if got := run.statusValue(); got != "canceled" {
		t.Fatalf("status = %q, want canceled", got)
	}

	after, err := srv.Portal.GetChat(context.Background(), owner, chatID)
	if err != nil {
		t.Fatal(err)
	}
	if len(storedMessages(t, after.Content)) != 0 {
		t.Fatalf("a cancelled voice run persisted messages: %s", after.Content)
	}
	if string(before.Content) != string(after.Content) {
		t.Fatalf("a cancelled voice run changed the chat document:\nbefore %s\nafter  %s", before.Content, after.Content)
	}
}

// A cancel that lands in EXACTLY the window between the run finishing its TTS leg and its
// commit claim must persist NOTHING. This is the check-then-act gap the commit CAS closes:
// with a ctx.Err() read followed by a separate "committing" flag, a cancel here slipped past
// the read, the WithoutCancel commit then proceeded, and a resume that had already been
// accepted double-committed. voiceBeforeCommitClaim is the test seam that lands the cancel
// deterministically in that window.
func TestVoiceRunCancelBetweenFinalGateAndCommitClaimPersistsNothing(t *testing.T) {
	srv, owner, chatID, _, _ := newVoiceRunTestServer(t, 0)
	srv.voiceBeforeCommitClaim = func(run *ChatRun) { run.cancelContext() }

	before, err := srv.Portal.GetChat(context.Background(), owner, chatID)
	if err != nil {
		t.Fatal(err)
	}
	run, snap := voiceRunOutcome(t, srv, owner, chatID, "keep talking")
	if snap.Status != "canceled" {
		t.Fatalf("status = %q (err %q), want canceled: a cancel in the commit-claim window must win", snap.Status, snap.Err)
	}
	if run.isCommitting() {
		t.Fatal("a run cancelled in the window must never have claimed its commit")
	}
	after, err := srv.Portal.GetChat(context.Background(), owner, chatID)
	if err != nil {
		t.Fatal(err)
	}
	if string(before.Content) != string(after.Content) {
		t.Fatalf("a cancel in the commit-claim window still persisted:\nbefore %s\nafter  %s", before.Content, after.Content)
	}
}

// The committed assistant turn carries the synthesized audio as an audio_url blob:
// SaveChatWithBlobs moved the WAV into chat_blobs, and the stored turn references it by
// the gateway blob path, not inline.
func TestVoiceRunAttachesAudioBlobToAssistantTurn(t *testing.T) {
	srv, owner, chatID, _, _ := newVoiceRunTestServer(t, 0)

	_, snap := voiceRunOutcome(t, srv, owner, chatID, "say hello")
	if snap.Status != "completed" {
		t.Fatalf("status = %q (err %q), want completed", snap.Status, snap.Err)
	}

	stored, err := srv.Portal.GetChat(context.Background(), owner, chatID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(stored.Content), `"type":"audio_url"`) {
		t.Fatalf("the assistant turn carries no audio_url part: %s", stored.Content)
	}
	if strings.Contains(string(stored.Content), "data:") {
		t.Fatalf("the audio was committed inline, not as a blob: %.300s", stored.Content)
	}
	id := storedAudioBlobID(t, stored.Content)
	got, mediaType, err := srv.Portal.ChatBlobBytes(context.Background(), owner, id)
	if err != nil {
		t.Fatalf("ChatBlobBytes(%s): %v", id, err)
	}
	first, second := voiceTestPCM()
	wantWAV := wavFromPCM16(append(append([]byte(nil), first...), second...), speechRunTestRate)
	if mediaType != "audio/wav" || !bytes.Equal(got, wantWAV) {
		t.Fatalf("stored blob is %d bytes as %s, want the %d-byte assembled WAV as audio/wav", len(got), mediaType, len(wantWAV))
	}
}

// Metering is inherited, not deferred: on an un-cancelled finish both an LLM-token row
// (the /v1/chat/completions leg) and a TTS audio row (the /v1/audio/speech leg) are
// written.
func TestVoiceRunBillsLLMAndTTS(t *testing.T) {
	srv, owner, chatID, recorder, _ := newVoiceRunTestServer(t, 0)

	_, snap := voiceRunOutcome(t, srv, owner, chatID, "bill me")
	if snap.Status != "completed" {
		t.Fatalf("status = %q (err %q), want completed", snap.Status, snap.Err)
	}

	var sawLLMTokens, sawTTSAudio bool
	for _, ev := range recorder.All() {
		switch {
		case ev.BillingUnit == "" && ev.OutputTokens > 0:
			sawLLMTokens = true
		case ev.BillingUnit == usage.BillingUnitAudioSecond || ev.BillingUnit == usage.BillingUnitAudioBytes:
			sawTTSAudio = true
		}
	}
	if !sawLLMTokens {
		t.Fatalf("no LLM-token usage row was written: %+v", recorder.All())
	}
	if !sawTTSAudio {
		t.Fatalf("no TTS audio usage row was written: %+v", recorder.All())
	}
}

// Multi-turn: a prior assistant voice turn's synthesized audio must NOT ride into the
// next turn's LLM request. The committed turn is [{text},{audio_url:/api/portal/chat-blobs/…}],
// and that gateway-relative url no upstream can fetch; the voice LLM history keeps the
// answer TEXT and strips the audio. This is the turn-2 case the single-turn tests miss.
func TestVoiceRunHistoryStripsPriorAnswerAudio(t *testing.T) {
	srv, owner, chatID, _, _ := newVoiceRunTestServer(t, 0)

	// Turn 1: a committed voice turn -> [user1, assistant(text + audio_url blob)].
	_, snap := voiceRunOutcome(t, srv, owner, chatID, "first question")
	if snap.Status != "completed" {
		t.Fatalf("turn 1 status = %q (err %q), want completed", snap.Status, snap.Err)
	}
	wantPriorAnswer := "Mock response for qwen-coder: first question"

	// Sanity: the stored prior answer really does carry the audio_url blob ref that
	// would otherwise leak into turn 2's request -- so this test would fail without the
	// strip, not pass vacuously.
	stored, err := srv.Portal.GetChat(context.Background(), owner, chatID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(stored.Content), `"type":"audio_url"`) || !strings.Contains(string(stored.Content), chatBlobRoutePrefix) {
		t.Fatalf("precondition: the stored prior answer must carry an audio_url blob ref: %s", stored.Content)
	}

	// Turn 2: the exact /v1/chat/completions body the voice run would POST is
	// buildChatCompletionsBody over VoiceRunHistory's result.
	const pending2 = "second question"
	history, err := srv.Portal.VoiceRunHistory(context.Background(), owner, chatID, pending2)
	if err != nil {
		t.Fatalf("VoiceRunHistory: %v", err)
	}
	body, err := buildChatCompletionsBody(PrepareRunResult{History: history, Settings: portal.ChatRunSettings{Model: voiceLLMTarget.Model}})
	if err != nil {
		t.Fatalf("buildChatCompletionsBody: %v", err)
	}
	s := string(body)
	if !strings.Contains(s, wantPriorAnswer) {
		t.Fatalf("the LLM request dropped the prior answer's text: %s", s)
	}
	if strings.Contains(s, "audio_url") {
		t.Fatalf("the LLM request still carries the prior answer's audio_url part: %s", s)
	}
	if strings.Contains(s, chatBlobRoutePrefix) {
		t.Fatalf("the LLM request carries a gateway-relative chat-blob url no upstream can fetch: %s", s)
	}
	// The pending text is the latest user message.
	if n := len(history); n == 0 || history[n-1].Role != "user" || !strings.Contains(string(history[n-1].Content), pending2) {
		t.Fatalf("the latest message must be the pending user turn %q: %+v", pending2, history)
	}

	// And turn 2 runs end to end: the stripped history drives a clean completion and
	// commits, leaving four messages.
	_, snap2 := voiceRunOutcome(t, srv, owner, chatID, pending2)
	if snap2.Status != "completed" {
		t.Fatalf("turn 2 status = %q (err %q), want completed", snap2.Status, snap2.Err)
	}
	after, err := srv.Portal.GetChat(context.Background(), owner, chatID)
	if err != nil {
		t.Fatal(err)
	}
	if msgs := storedMessages(t, after.Content); len(msgs) != 4 {
		t.Fatalf("after two voice turns the chat has %d messages, want 4 [user, assistant, user, assistant]: %s", len(msgs), after.Content)
	}
}

// A MIXED thread: an earlier speech-run turn holds only audio (a TTS-only answer), so
// stripping it leaves nothing. The /v1/chat/completions body the voice run POSTs must carry NO
// assistant message with empty content -- a strict upstream (native Anthropic) rejects one on a
// non-final message -- must still alternate roles, and a normal voice turn (text + audio) must
// keep its text. The turn then runs end to end on that history.
func TestVoiceRunBodyHasNoEmptyAssistantContentInAMixedThread(t *testing.T) {
	srv, owner, chatID, _, _ := newVoiceRunTestServer(t, 0)

	// Seed the mixed thread by saving the whole document: [speech user, audio-only
	// assistant, voice user, text+audio assistant].
	audio := `{"type":"audio_removed"}`
	doc := `{"settings":{},"messages":[` +
		`{"id":"u1","role":"user","content":"say hello"},` +
		`{"id":"a1","role":"assistant","status":"complete","content":[` + audio + `]},` +
		`{"id":"u2","role":"user","content":"what is two plus two"},` +
		`{"id":"a2","role":"assistant","status":"complete","content":[{"type":"text","text":"four"},` + audio + `]}]}`
	content := json.RawMessage(doc)
	if _, err := srv.Portal.SaveChat(context.Background(), owner, chatID, portal.UpdateChatRequest{Content: &content}); err != nil {
		t.Fatalf("seed the mixed thread: %v", err)
	}

	const pending = "and three plus three"
	history, err := srv.Portal.VoiceRunHistory(context.Background(), owner, chatID, pending)
	if err != nil {
		t.Fatalf("VoiceRunHistory: %v", err)
	}
	body, err := buildChatCompletionsBody(PrepareRunResult{History: history, Settings: portal.ChatRunSettings{Model: voiceLLMTarget.Model}})
	if err != nil {
		t.Fatalf("buildChatCompletionsBody: %v", err)
	}
	var req struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("request body is not JSON: %v (%s)", err, body)
	}
	for i, m := range req.Messages {
		if m.Role == "assistant" && (len(m.Content) == 0 || string(m.Content) == `""` || string(m.Content) == "[]" || string(m.Content) == "null") {
			t.Fatalf("messages[%d] is an assistant message with empty content: %s", i, body)
		}
		if i > 0 && req.Messages[i-1].Role == m.Role {
			t.Fatalf("messages[%d] and messages[%d] are both %q: roles must alternate: %s", i-1, i, m.Role, body)
		}
	}
	if !strings.Contains(string(body), "four") {
		t.Fatalf("the voice turn's answer text was dropped: %s", body)
	}
	if strings.Contains(string(body), "audio") {
		t.Fatalf("an audio part reached the LLM request: %s", body)
	}
	if n := len(req.Messages); n == 0 || req.Messages[n-1].Role != "user" || !strings.Contains(string(req.Messages[n-1].Content), pending) {
		t.Fatalf("the last message must be the pending user turn: %s", body)
	}

	// The turn runs cleanly on that history.
	if _, snap := voiceRunOutcome(t, srv, owner, chatID, pending); snap.Status != "completed" {
		t.Fatalf("status = %q (err %q), want completed", snap.Status, snap.Err)
	}
}

// A subscriber sees the answer's text deltas FIRST and the synthesized audio deltas
// AFTER: the run streams the whole answer, then speaks it.
func TestVoiceRunPublishesTextThenAudio(t *testing.T) {
	srv, owner, chatID, _, _ := newVoiceRunTestServer(t, 0)

	// Subscribe BEFORE launch (reserve/subscribe/launch) so no delta is missed.
	history, err := srv.Portal.VoiceRunHistory(context.Background(), owner, chatID, "speak please")
	if err != nil {
		t.Fatalf("VoiceRunHistory: %v", err)
	}
	run, runCtx, err := srv.reserveRun(owner, chatID)
	if err != nil {
		t.Fatalf("reserveRun: %v", err)
	}
	_, ch, unsub := run.subscribe()
	defer unsub()
	srv.launchRun(runCtx, owner, run, PrepareRunResult{
		History:  history,
		Settings: portal.ChatRunSettings{Model: voiceLLMTarget.Model, Kind: chatRunKindVoice},
		Voice:    &voiceRunParams{pendingText: "speak please", tts: voiceTTSTarget},
	})

	var events []runEvent
	timeout := time.After(10 * time.Second)
	for {
		ev, open := <-ch
		if !open {
			break
		}
		events = append(events, ev)
		select {
		case <-timeout:
			t.Fatal("the run published no terminal event within 10s")
		default:
		}
	}

	lastText, firstAudio := -1, -1
	for i, ev := range events {
		switch {
		case ev.Event == "delta" && ev.Content != "":
			lastText = i
		case ev.Event == "audio" && ev.Audio != nil && firstAudio == -1:
			firstAudio = i
		}
	}
	if lastText == -1 {
		t.Fatalf("no text delta was published: %+v", events)
	}
	if firstAudio == -1 {
		t.Fatalf("no audio delta was published (the TTS leg did not stream): %+v", events)
	}
	if lastText >= firstAudio {
		t.Fatalf("a text delta (index %d) came at or after the first audio delta (index %d): text must stream before audio", lastText, firstAudio)
	}
}

// The last link of the voice dropdown (#318): the ticket's Audio is what the voice run's TTS leg
// synthesizes with. voiceSpeechPrep must carry the ticket's voice (and the rest of its speech
// settings) into the /v1/audio/speech request body, both the buffered body and the streaming one
// the voice run asks for; the other voice-run tests set no audio, so this link was unpinned.
func TestVoiceSpeechPrepCarriesTheTicketsVoiceIntoTheSpeechRequestBody(t *testing.T) {
	speed := 1.25
	audio := portal.ChatAudioSettings{Voice: "marius", Speed: &speed}
	prep := voiceSpeechPrep("the spoken answer", routing.Target{Model: "voice-tts"}, audio, "de")

	for name, build := range map[string]func(PrepareRunResult) ([]byte, error){
		"the buffered body":  buildSpeechBody,
		"the streaming body": buildSpeechStreamBody,
	} {
		t.Run(name, func(t *testing.T) {
			raw, err := build(prep)
			if err != nil {
				t.Fatalf("build: %v", err)
			}
			var body map[string]any
			if err := json.Unmarshal(raw, &body); err != nil {
				t.Fatalf("body is not JSON: %v", err)
			}
			if body["voice"] != "marius" || body["model"] != "voice-tts" || body["input"] != "the spoken answer" || body["speed"] != 1.25 || body["language"] != "de" {
				t.Fatalf("body = %v, want the ticket's voice, speed and language with the model and the answer as the input", body)
			}
		})
	}
}

// A ticket that names no voice sends none: the model's own default voice speaks.
func TestVoiceSpeechPrepSendsNoVoiceWhenTheTicketHasNone(t *testing.T) {
	prep := voiceSpeechPrep("answer", routing.Target{Model: "voice-tts"}, portal.ChatAudioSettings{}, "")

	fields, err := speechBodyFields(prep)
	if err != nil {
		t.Fatalf("speechBodyFields: %v", err)
	}
	if _, present := fields["voice"]; present {
		t.Fatalf("body = %v, want no voice member for a ticket with no voice", fields)
	}
}

// The per-answer language the LLM picks via <speak lang> reaches the speech leg only when the
// TTS model can take a `language` parameter: a model that cannot gets none, so the field is
// omitted rather than sent to be silently dropped (or rejected) upstream. The requested code is
// passed verbatim, never normalized.
func TestEffectiveSpeechLanguage(t *testing.T) {
	for _, tc := range []struct {
		name         string
		requested    string
		modelAccepts bool
		want         string
	}{
		{"accepted language passes through", "de", true, "de"},
		{"a model that cannot take it omits it", "de", false, ""},
		{"nothing requested stays empty", "", true, ""},
		{"nothing requested and unsupported stays empty", "", false, ""},
		{"verbatim, not normalized", "de-DE", true, "de-DE"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := effectiveSpeechLanguage(tc.requested, tc.modelAccepts); got != tc.want {
				t.Fatalf("effectiveSpeechLanguage(%q, %v) = %q, want %q", tc.requested, tc.modelAccepts, got, tc.want)
			}
		})
	}
}

// voiceSpeechPrep carries the per-answer language onto the speech leg's audio settings, where
// speechBodyFields turns it into the request body's top-level `language`.
func TestVoiceSpeechPrep_SetsLanguage(t *testing.T) {
	prep := voiceSpeechPrep("hi", routing.Target{Model: "voice-tts"}, portal.ChatAudioSettings{}, "de")

	if prep.Settings.Audio == nil || prep.Settings.Audio.Language != "de" {
		t.Fatalf("prep.Settings.Audio = %+v, want Language %q", prep.Settings.Audio, "de")
	}
	fields, err := speechBodyFields(prep)
	if err != nil {
		t.Fatalf("speechBodyFields: %v", err)
	}
	if fields["language"] != "de" {
		t.Fatalf("body = %v, want language %q", fields, "de")
	}
}

// An empty language leaves the audio settings' Language unset, so the request body carries no
// `language` member and the model speaks in its own default.
func TestVoiceSpeechPrep_EmptyLanguageUnset(t *testing.T) {
	prep := voiceSpeechPrep("hi", routing.Target{Model: "voice-tts"}, portal.ChatAudioSettings{}, "")

	if prep.Settings.Audio == nil || prep.Settings.Audio.Language != "" {
		t.Fatalf("prep.Settings.Audio = %+v, want an unset Language", prep.Settings.Audio)
	}
	fields, err := speechBodyFields(prep)
	if err != nil {
		t.Fatalf("speechBodyFields: %v", err)
	}
	if _, present := fields["language"]; present {
		t.Fatalf("body = %v, want no language member for an empty language", fields)
	}
}

// --- Phase 2: concurrent two-leg flow + live-delta tag stripping (spec 5.5) ---

// storedAssistantMsg returns the chat document's assistant message as a raw-field map, so a
// test can read both its content parts and its top-level spoken_summary key.
func storedAssistantMsg(t *testing.T, content json.RawMessage) map[string]json.RawMessage {
	t.Helper()
	var doc struct {
		Messages []map[string]json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(content, &doc); err != nil {
		t.Fatalf("stored document is not JSON: %v", err)
	}
	for _, m := range doc.Messages {
		if string(m["role"]) == `"assistant"` {
			return m
		}
	}
	t.Fatalf("stored document has no assistant message: %s", content)
	return nil
}

// voiceTextPart is the text of the first {"type":"text"} part of a stored message's content.
func voiceTextPart(t *testing.T, content json.RawMessage) string {
	t.Helper()
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(content, &parts); err != nil {
		t.Fatalf("content is not a parts array: %s", content)
	}
	for _, p := range parts {
		if p.Type == "text" {
			return p.Text
		}
	}
	t.Fatalf("content has no text part: %s", content)
	return ""
}

// storedSpokenSummary returns the assistant message's spoken_summary and whether the key is
// present at all (a no-tag / no-audio turn omits it).
func storedSpokenSummary(t *testing.T, content json.RawMessage) (string, bool) {
	t.Helper()
	raw, ok := storedAssistantMsg(t, content)["spoken_summary"]
	if !ok {
		return "", false
	}
	var summary string
	if err := json.Unmarshal(raw, &summary); err != nil {
		t.Fatalf("spoken_summary is not a string: %s", raw)
	}
	return summary, true
}

// seedVoiceSpeechLanguage writes the TTS mapping's scope-"" speech_language fact, as the
// health loop does, so the voice run's capability lookup finds the model accepts (or not) a
// `language` field.
func seedVoiceSpeechLanguage(t *testing.T, srv *Server, accepts bool, def string) {
	t.Helper()
	row := routing.ModelMappingCacheRow{
		MappingID: "route-tts",
		CacheType: routing.ModelMappingCacheSpeechLanguage,
		ScopeKey:  "",
		Payload:   routing.SpeechLanguagePayload(accepts, def),
		UpdatedAt: time.Now(),
	}
	if err := srv.Routes.UpsertModelMappingCache(context.Background(), row); err != nil {
		t.Fatalf("UpsertModelMappingCache: %v", err)
	}
}

// The speech leg fires at </speak>, while the LLM is still streaming the detail (spec 5.5).
// The scripted LLM leg BLOCKS after </speak> until the speech request arrives, so a run that
// synthesized only after the LLM finished (phase-1 ordering) would deadlock and never go
// terminal within waitFor's window -- the ordering is proven by the run completing at all,
// and by the TTS input being the captured summary.
func TestVoiceRun_FiresSpeechAtCloseTag(t *testing.T) {
	prov := defaultVoiceProvider()
	prov.speechArrived = make(chan struct{})
	prov.speechOnce = &sync.Once{}
	prov.llmScript = []scriptedLLMDelta{
		{text: `<speak lang="de">Kurz.`},
		{text: `</speak>`, waitSpeech: true},
		{text: ` Ausfuehrliche Details.`},
	}
	srv, owner, chatID, _, speechBodies := newVoiceRunTestServerProv(t, prov)

	_, snap := voiceRunOutcome(t, srv, owner, chatID, "unused")
	if snap.Status != "completed" {
		t.Fatalf("status = %q (err %q), want completed: the LLM leg blocks after </speak> until the speech leg fires, so a non-overlapped run never finishes", snap.Status, snap.Err)
	}
	if got := takeSpeechBody(t, speechBodies)["input"]; got != "Kurz." {
		t.Fatalf("TTS input = %q, want the captured summary %q (fired at </speak>)", got, "Kurz.")
	}
}

// The committed turn carries the FULL clean text (summary + detail, tags stripped) as its
// content, the summary region as spoken_summary, and the synthesized audio -- end to end,
// through the real gateway (the forwarding Task 4 could not test on its own).
func TestVoiceRun_CommitsFullTextAndSummary(t *testing.T) {
	srv, owner, chatID, _, speechBodies := newVoiceRunTestServer(t, 0)

	const pending = `<speak lang="de">Kurz zusammengefasst.</speak> Ausfuehrliche Details.`
	_, snap := voiceRunOutcome(t, srv, owner, chatID, pending)
	if snap.Status != "completed" {
		t.Fatalf("status = %q (err %q), want completed", snap.Status, snap.Err)
	}

	const wantSummary = "Kurz zusammengefasst."
	const wantContent = "Mock response for qwen-coder: Kurz zusammengefasst. Ausfuehrliche Details."

	stored, err := srv.Portal.GetChat(context.Background(), owner, chatID)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(stored.Content), "<speak") || strings.Contains(string(stored.Content), "</speak") {
		t.Fatalf("stored turn still carries <speak markup: %s", stored.Content)
	}
	msg := storedAssistantMsg(t, stored.Content)
	if got := voiceTextPart(t, msg["content"]); got != wantContent {
		t.Fatalf("text content = %q, want the full clean answer %q", got, wantContent)
	}
	if summary, ok := storedSpokenSummary(t, stored.Content); !ok || summary != wantSummary {
		t.Fatalf("spoken_summary = %q (present %v), want %q", summary, ok, wantSummary)
	}
	if !strings.Contains(string(stored.Content), `"type":"audio_url"`) {
		t.Fatalf("assistant turn has no audio part: %s", stored.Content)
	}
	// The TTS leg synthesized the SUMMARY, not the whole answer.
	if got := takeSpeechBody(t, speechBodies)["input"]; got != wantSummary {
		t.Fatalf("TTS input = %q, want the summary %q", got, wantSummary)
	}
}

// No <speak> tag: the whole answer is synthesized after the LLM finishes (the phase-1 ordered
// fallback) and the committed turn persists no spoken_summary key.
func TestVoiceRun_NoTagSpeaksWholeAnswer(t *testing.T) {
	srv, owner, chatID, _, speechBodies := newVoiceRunTestServer(t, 0)

	const pending = "plain question"
	_, snap := voiceRunOutcome(t, srv, owner, chatID, pending)
	if snap.Status != "completed" {
		t.Fatalf("status = %q (err %q), want completed", snap.Status, snap.Err)
	}
	wantAnswer := "Mock response for qwen-coder: " + pending
	if got := takeSpeechBody(t, speechBodies)["input"]; got != wantAnswer {
		t.Fatalf("TTS input = %q, want the whole answer %q", got, wantAnswer)
	}
	stored, err := srv.Portal.GetChat(context.Background(), owner, chatID)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := storedSpokenSummary(t, stored.Content); ok {
		t.Fatalf("a no-tag turn must persist no spoken_summary: %s", stored.Content)
	}
}

// Live-delta tag stripping: NO published delta carries <speak> markup -- the scanner strips
// the tags before run.publish fans the clean text out (and buffers it).
func TestVoiceRun_LiveDeltasAreTagStripped(t *testing.T) {
	srv, owner, chatID, _, _ := newVoiceRunTestServer(t, 0)

	const pending = `<speak lang="de">Kurz.</speak> Details.`
	history, err := srv.Portal.VoiceRunHistory(context.Background(), owner, chatID, pending)
	if err != nil {
		t.Fatalf("VoiceRunHistory: %v", err)
	}
	run, runCtx, err := srv.reserveRun(owner, chatID)
	if err != nil {
		t.Fatalf("reserveRun: %v", err)
	}
	_, ch, unsub := run.subscribe()
	defer unsub()
	srv.launchRun(runCtx, owner, run, PrepareRunResult{
		History:  history,
		Settings: portal.ChatRunSettings{Model: voiceLLMTarget.Model, Kind: chatRunKindVoice},
		Voice:    &voiceRunParams{pendingText: pending, tts: voiceTTSTarget},
	})

	var clean strings.Builder
	timeout := time.After(10 * time.Second)
	for {
		select {
		case ev, open := <-ch:
			if !open {
				goto collected
			}
			if ev.Event == "delta" {
				if strings.Contains(ev.Content, "<speak") || strings.Contains(ev.Content, "</speak") {
					t.Fatalf("a published delta carried speak markup: %q", ev.Content)
				}
				clean.WriteString(ev.Content)
			}
		case <-timeout:
			t.Fatal("the run published no terminal event within 10s")
		}
	}
collected:
	got := clean.String()
	if !strings.Contains(got, "Kurz.") || !strings.Contains(got, "Details.") || strings.Contains(got, "speak") {
		t.Fatalf("clean delta stream = %q, want the tag-stripped text", got)
	}
}

// The per-answer language reaches the speech leg only when the TTS model can take a
// `language` field: the default harness seeds no speech_language fact, so the model cannot,
// and the speech body omits language even though the LLM asked for <speak lang="de">.
func TestVoiceRun_LanguageOmittedWhenModelCannot(t *testing.T) {
	srv, owner, chatID, _, speechBodies := newVoiceRunTestServer(t, 0)

	const pending = `<speak lang="de">Kurz.</speak> Details.`
	_, snap := voiceRunOutcome(t, srv, owner, chatID, pending)
	if snap.Status != "completed" {
		t.Fatalf("status = %q (err %q), want completed", snap.Status, snap.Err)
	}
	body := takeSpeechBody(t, speechBodies)
	if lang, present := body["language"]; present {
		t.Fatalf("speech body carries language %v, want it omitted for a model without the capability", lang)
	}
	if body["input"] != "Kurz." {
		t.Fatalf("TTS input = %v, want the summary %q", body["input"], "Kurz.")
	}
}

// The positive control: when the TTS model's cached fact says it accepts a `language` field,
// the per-answer language the LLM requested via <speak lang> rides onto the speech body.
func TestVoiceRun_LanguagePassedWhenModelAccepts(t *testing.T) {
	srv, owner, chatID, _, speechBodies := newVoiceRunTestServer(t, 0)
	seedVoiceSpeechLanguage(t, srv, true, "de")

	const pending = `<speak lang="de">Kurz.</speak> Details.`
	_, snap := voiceRunOutcome(t, srv, owner, chatID, pending)
	if snap.Status != "completed" {
		t.Fatalf("status = %q (err %q), want completed", snap.Status, snap.Err)
	}
	if lang := takeSpeechBody(t, speechBodies)["language"]; lang != "de" {
		t.Fatalf("speech body language = %v, want %q when the model accepts it", lang, "de")
	}
}

// A cancel that lands in the commit-claim window wins even after the overlap: the speech leg
// fired at </speak> (the overlap really happened), but the CAS claim loses to the cancel and
// NOTHING is persisted.
func TestVoiceRun_CancelDuringOverlapPersistsNothing(t *testing.T) {
	srv, owner, chatID, _, speechBodies := newVoiceRunTestServer(t, 0)
	srv.voiceBeforeCommitClaim = func(run *ChatRun) { run.cancelContext() }

	before, err := srv.Portal.GetChat(context.Background(), owner, chatID)
	if err != nil {
		t.Fatal(err)
	}

	const pending = `<speak lang="de">Kurz.</speak> Details.`
	run, snap := voiceRunOutcome(t, srv, owner, chatID, pending)
	if snap.Status != "canceled" {
		t.Fatalf("status = %q (err %q), want canceled: a cancel in the commit-claim window must win even after the speech leg fired", snap.Status, snap.Err)
	}
	if run.isCommitting() {
		t.Fatal("a run cancelled in the window must never have claimed its commit")
	}
	// The overlap really happened: the speech leg fired at </speak> on the summary.
	if got := takeSpeechBody(t, speechBodies)["input"]; got != "Kurz." {
		t.Fatalf("TTS input = %q, want the summary %q (the speech leg must have fired during the overlap)", got, "Kurz.")
	}
	after, err := srv.Portal.GetChat(context.Background(), owner, chatID)
	if err != nil {
		t.Fatal(err)
	}
	if string(before.Content) != string(after.Content) {
		t.Fatalf("a cancel in the commit-claim window still persisted:\nbefore %s\nafter  %s", before.Content, after.Content)
	}
}

// The LLM fails AFTER </speak> (summary spoken, detail cut off): the turn is still committed
// with the summary text + its audio (detail truncated), and the degrade is surfaced on the
// run's terminal (spec 5.5).
func TestVoiceRun_LLMFailsAfterCloseCommitsSummary(t *testing.T) {
	prov := defaultVoiceProvider()
	prov.llmScript = []scriptedLLMDelta{
		{text: `<speak lang="de">Kurz.`},
		{text: `</speak> Teil`},
		{failAfter: true},
	}
	srv, owner, chatID, _, speechBodies := newVoiceRunTestServerProv(t, prov)

	_, snap := voiceRunOutcome(t, srv, owner, chatID, "unused")
	if snap.Status == "completed" || snap.Status == "canceled" {
		t.Fatalf("status = %q, want an error status surfacing the LLM degrade", snap.Status)
	}

	stored, err := srv.Portal.GetChat(context.Background(), owner, chatID)
	if err != nil {
		t.Fatal(err)
	}
	if msgs := storedMessages(t, stored.Content); len(msgs) != 2 {
		t.Fatalf("want [user, assistant], got %d messages: %s", len(msgs), stored.Content)
	}
	if summary, ok := storedSpokenSummary(t, stored.Content); !ok || summary != "Kurz." {
		t.Fatalf("spoken_summary = %q (present %v), want %q", summary, ok, "Kurz.")
	}
	if !strings.Contains(string(stored.Content), `"type":"audio_url"`) {
		t.Fatalf("committed turn has no audio part: %s", stored.Content)
	}
	// The detail is truncated: the content is only the clean text captured before the failure.
	if got := voiceTextPart(t, storedAssistantMsg(t, stored.Content)["content"]); got != "Kurz. Teil" {
		t.Fatalf("text content = %q, want the truncated clean text %q", got, "Kurz. Teil")
	}
	if got := takeSpeechBody(t, speechBodies)["input"]; got != "Kurz." {
		t.Fatalf("TTS input = %q, want the summary %q", got, "Kurz.")
	}
}

// The speech leg fails: the text turn still commits (no audio block, no spoken_summary) and
// the degrade is surfaced on the run's terminal (spec 5.5).
func TestVoiceRun_SpeechFailsCommitsTextOnly(t *testing.T) {
	prov := defaultVoiceProvider()
	prov.speechFail = true
	srv, owner, chatID, _, _ := newVoiceRunTestServerProv(t, prov)

	const pending = `<speak lang="de">Kurz.</speak> Details.`
	_, snap := voiceRunOutcome(t, srv, owner, chatID, pending)
	if snap.Status == "completed" || snap.Status == "canceled" {
		t.Fatalf("status = %q, want an error status surfacing the speech degrade", snap.Status)
	}

	stored, err := srv.Portal.GetChat(context.Background(), owner, chatID)
	if err != nil {
		t.Fatal(err)
	}
	if msgs := storedMessages(t, stored.Content); len(msgs) != 2 {
		t.Fatalf("want [user, assistant], got %d messages: %s", len(msgs), stored.Content)
	}
	const wantContent = "Mock response for qwen-coder: Kurz. Details."
	if got := voiceTextPart(t, storedAssistantMsg(t, stored.Content)["content"]); got != wantContent {
		t.Fatalf("text content = %q, want the full clean answer %q", got, wantContent)
	}
	if strings.Contains(string(stored.Content), `"type":"audio_url"`) || strings.Contains(string(stored.Content), "data:") {
		t.Fatalf("a failed speech leg must leave no audio: %s", stored.Content)
	}
	if _, ok := storedSpokenSummary(t, stored.Content); ok {
		t.Fatalf("a failed speech leg must persist no spoken_summary: %s", stored.Content)
	}
}
