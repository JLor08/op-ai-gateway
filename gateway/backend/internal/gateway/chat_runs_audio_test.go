// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
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

// speechRunPrep is the prepared run every speech test below starts from: the
// pinned speech kind, the speech-capable model, and a history whose last user
// message carries the text to speak.
func speechRunPrep() PrepareRunResult {
	return PrepareRunResult{
		History:  []portal.ChatAPIMessage{{Role: "user", Content: json.RawMessage(`"Hello there"`)}},
		Settings: portal.ChatRunSettings{Model: speechGatewayModel, Kind: chatRunKindSpeech},
	}
}

// decodeSpeechBody decodes a buildSpeechBody / loopback body into a generic map,
// failing the test when it is not a JSON object.
func decodeSpeechBody(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("speech body %s is not a JSON object: %v", raw, err)
	}
	return body
}

// --- buildSpeechBody -------------------------------------------------------

// With no audio settings the body is exactly model and input: a speech thread
// that set no knob must send nothing extra, so audio.cpp stays on its own
// defaults (voice, speed, ...). response_format is deliberately absent too -- the
// endpoint answers WAV by default and refuses everything else.
func TestBuildSpeechBodyWithoutSettingsIsModelAndInput(t *testing.T) {
	raw, err := buildSpeechBody(speechRunPrep())
	if err != nil {
		t.Fatal(err)
	}
	body := decodeSpeechBody(t, raw)
	if len(body) != 2 || body["model"] != speechGatewayModel || body["input"] != "Hello there" {
		t.Fatalf("want exactly model/input, got: %s", raw)
	}
}

// An audio block that is present but empty is the same as no block: nothing set,
// nothing sent.
func TestBuildSpeechBodyWithEmptySettingsIsModelAndInput(t *testing.T) {
	prep := speechRunPrep()
	prep.Settings.Audio = &portal.ChatAudioSettings{}
	raw, err := buildSpeechBody(prep)
	if err != nil {
		t.Fatal(err)
	}
	if body := decodeSpeechBody(t, raw); len(body) != 2 {
		t.Fatalf("an empty audio block must add nothing, got: %s", raw)
	}
}

// The set first-class knobs are forwarded; an unset knob is omitted (audio.cpp
// keeps its default) rather than sent as an inert zero.
func TestBuildSpeechBodyForwardsOnlyTheSetSettings(t *testing.T) {
	prep := speechRunPrep()
	speed := 1.25
	prep.Settings.Audio = &portal.ChatAudioSettings{Voice: "alloy", Speed: &speed}
	raw, err := buildSpeechBody(prep)
	if err != nil {
		t.Fatal(err)
	}
	body := decodeSpeechBody(t, raw)
	if body["model"] != speechGatewayModel || body["input"] != "Hello there" {
		t.Fatalf("base fields changed: %s", raw)
	}
	if body["voice"] != "alloy" {
		t.Fatalf("voice = %v, want alloy: %s", body["voice"], raw)
	}
	if got, _ := body["speed"].(float64); got != 1.25 {
		t.Fatalf("speed = %v, want 1.25: %s", body["speed"], raw)
	}
	if len(body) != 4 {
		t.Fatalf("want exactly model/input/voice/speed, got: %s", raw)
	}

	// Only a voice: speed is omitted.
	prep.Settings.Audio = &portal.ChatAudioSettings{Voice: "alloy"}
	raw, err = buildSpeechBody(prep)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := decodeSpeechBody(t, raw)["speed"]; ok {
		t.Fatalf("an unset speed must be omitted, not sent: %s", raw)
	}

	// Only a speed: the empty voice is omitted.
	prep.Settings.Audio = &portal.ChatAudioSettings{Speed: &speed}
	raw, err = buildSpeechBody(prep)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := decodeSpeechBody(t, raw)["voice"]; ok {
		t.Fatalf("an empty voice must be omitted, not sent: %s", raw)
	}

	// An explicit zero speed is a SET value (the pointer exists for exactly this),
	// so it is forwarded and left to the model's own range check.
	zero := 0.0
	prep.Settings.Audio = &portal.ChatAudioSettings{Speed: &zero}
	raw, err = buildSpeechBody(prep)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := decodeSpeechBody(t, raw)["speed"]; !ok || got != 0.0 {
		t.Fatalf("an explicit speed of 0 must be forwarded as 0: %s", raw)
	}
}

// Every passthrough key is merged into the body with the exact JSON type the user
// wrote -- string, number, bool, object -- because the values are raw JSON.
func TestBuildSpeechBodyMergesThePassthrough(t *testing.T) {
	prep := speechRunPrep()
	prep.Settings.Audio = &portal.ChatAudioSettings{
		Voice: "alloy",
		Passthrough: map[string]json.RawMessage{
			"instructions": json.RawMessage(`"Speak slowly and warmly."`),
			"seed":         json.RawMessage(`42`),
			"normalize":    json.RawMessage(`true`),
			"extra":        json.RawMessage(`{"temperature":0.7}`),
		},
	}
	raw, err := buildSpeechBody(prep)
	if err != nil {
		t.Fatal(err)
	}
	body := decodeSpeechBody(t, raw)
	if body["model"] != speechGatewayModel || body["input"] != "Hello there" || body["voice"] != "alloy" {
		t.Fatalf("first-class fields changed by the merge: %s", raw)
	}
	if body["instructions"] != "Speak slowly and warmly." {
		t.Fatalf("instructions = %v: %s", body["instructions"], raw)
	}
	if n, ok := body["seed"].(float64); !ok || n != 42 {
		t.Fatalf("seed must stay a JSON number, got %#v: %s", body["seed"], raw)
	}
	if b, ok := body["normalize"].(bool); !ok || !b {
		t.Fatalf("normalize must stay a JSON bool, got %#v: %s", body["normalize"], raw)
	}
	extra, ok := body["extra"].(map[string]any)
	if !ok || extra["temperature"] != 0.7 {
		t.Fatalf("extra must stay a JSON object, got %#v: %s", body["extra"], raw)
	}
}

// A passthrough key the run itself owns is REJECTED, never forwarded and never
// silently dropped: model and input are the run's own, and stream/response_format
// are the two the endpoint's contract (a single complete WAV) depends on. The match
// ignores case because the readers of this body decode with encoding/json, which
// matches a tagged field case-insensitively.
func TestBuildSpeechBodyRejectsAReservedPassthroughKey(t *testing.T) {
	for _, key := range []string{
		"model", "input", "stream", "response_format",
		"Model", "INPUT", "Stream", "Response_Format",
	} {
		t.Run(key, func(t *testing.T) {
			prep := speechRunPrep()
			prep.Settings.Audio = &portal.ChatAudioSettings{
				Passthrough: map[string]json.RawMessage{
					"instructions": json.RawMessage(`"fine"`),
					key:            json.RawMessage(`"evil"`),
				},
			}
			raw, err := buildSpeechBody(prep)
			if !errors.Is(err, errSpeechPassthroughReserved) {
				t.Fatalf("buildSpeechBody = (%s, %v), want errSpeechPassthroughReserved", raw, err)
			}
			if raw != nil {
				t.Fatalf("a rejected body must not be returned at all, got %s", raw)
			}
		})
	}
}

// The first-class settings are the visible controls, so when a thread sets BOTH a
// first-class knob and a passthrough key of the same name, the control wins and the
// passthrough key is not sent twice (not even under another case). With the control
// unset, the passthrough key is the only statement of it and goes through.
func TestBuildSpeechBodyFirstClassSettingWinsOverAPassthroughTwin(t *testing.T) {
	speed := 1.5
	prep := speechRunPrep()
	prep.Settings.Audio = &portal.ChatAudioSettings{
		Voice: "alloy",
		Speed: &speed,
		Passthrough: map[string]json.RawMessage{
			"voice": json.RawMessage(`"echo"`),
			"Speed": json.RawMessage(`9`),
		},
	}
	raw, err := buildSpeechBody(prep)
	if err != nil {
		t.Fatal(err)
	}
	body := decodeSpeechBody(t, raw)
	if body["voice"] != "alloy" {
		t.Fatalf("voice = %v, the first-class setting must win: %s", body["voice"], raw)
	}
	if got, _ := body["speed"].(float64); got != 1.5 {
		t.Fatalf("speed = %v, the first-class setting must win: %s", body["speed"], raw)
	}
	if _, ok := body["Speed"]; ok || len(body) != 4 {
		t.Fatalf("the shadowed passthrough twin must not be sent: %s", raw)
	}

	prep.Settings.Audio = &portal.ChatAudioSettings{
		Passthrough: map[string]json.RawMessage{"voice": json.RawMessage(`"echo"`)},
	}
	raw, err = buildSpeechBody(prep)
	if err != nil {
		t.Fatal(err)
	}
	if got := decodeSpeechBody(t, raw)["voice"]; got != "echo" {
		t.Fatalf("with no first-class voice the passthrough voice must go through, got %v: %s", got, raw)
	}
}

// A passthrough value that is not valid JSON cannot be marshalled into the body;
// buildSpeechBody reports it instead of sending a broken request.
func TestBuildSpeechBodyReportsAnInvalidPassthroughValue(t *testing.T) {
	prep := speechRunPrep()
	prep.Settings.Audio = &portal.ChatAudioSettings{
		Passthrough: map[string]json.RawMessage{"instructions": json.RawMessage(`{not json`)},
	}
	if raw, err := buildSpeechBody(prep); err == nil {
		t.Fatalf("buildSpeechBody = %s, want an error for an invalid raw value", raw)
	}
}

// The input is the LAST user message's text, whichever shape the content has --
// the same extractor the image prompt uses.
func TestBuildSpeechBodyUsesTheLastUserMessagesText(t *testing.T) {
	prep := speechRunPrep()
	prep.History = []portal.ChatAPIMessage{
		{Role: "user", Content: json.RawMessage(`"an older line"`)},
		{Role: "assistant", Content: json.RawMessage(`"ok"`)},
		{Role: "user", Content: json.RawMessage(`[{"type":"text","text":"read this aloud"}]`)},
	}
	raw, err := buildSpeechBody(prep)
	if err != nil {
		t.Fatal(err)
	}
	if got := decodeSpeechBody(t, raw)["input"]; got != "read this aloud" {
		t.Fatalf("input = %v, want the last user message's text: %s", got, raw)
	}
}

// A history with no user text yields an empty input, which is NOT rejected here:
// validateSpeechRequest owns "input is required" (speech.input_required), and a
// second copy of the rule in the executor would be free to drift from it.
func TestBuildSpeechBodyLeavesAnEmptyInputToTheEndpoint(t *testing.T) {
	prep := speechRunPrep()
	prep.History = nil
	raw, err := buildSpeechBody(prep)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := decodeSpeechBody(t, raw)["input"]; !ok || got != "" {
		t.Fatalf("input = %#v, want an empty string: %s", got, raw)
	}
}

// --- the loopback hop ------------------------------------------------------

// speechRunLoopbackCall is one request the run executor made to the gateway's own
// /v1/audio/speech, as a recording stand-in for the gateway received it.
type speechRunLoopbackCall struct {
	method      string
	path        string
	contentType string
	header      http.Header
	body        []byte
}

// newSpeechRunLoopback starts a recording stand-in for the gateway's own
// /v1/audio/speech: it keeps EVERY request it receives -- method, path, headers
// and body, not just "a request arrived" (#54) -- and answers status with body
// under contentType. The test points the server's selfBaseURL at it, so what is
// asserted is exactly what the executor put on the wire.
func newSpeechRunLoopback(t *testing.T, status int, contentType string, body []byte) (*httptest.Server, <-chan speechRunLoopbackCall) {
	t.Helper()
	calls := make(chan speechRunLoopbackCall, 8)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		calls <- speechRunLoopbackCall{method: r.Method, path: r.URL.Path, contentType: r.Header.Get("Content-Type"), header: r.Header.Clone(), body: raw}
		if contentType != "" {
			w.Header().Set("Content-Type", contentType)
		} else {
			// A nil value (not an absent key) is how net/http is told NOT to sniff a
			// type from the body, so "no Content-Type" really reaches the run.
			w.Header()["Content-Type"] = nil
		}
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}))
	t.Cleanup(ts.Close)
	return ts, calls
}

// takeSpeechRunCall returns the one recorded loopback call, failing when there is
// none or more than one.
func takeSpeechRunCall(t *testing.T, calls <-chan speechRunLoopbackCall) speechRunLoopbackCall {
	t.Helper()
	var got speechRunLoopbackCall
	select {
	case got = <-calls:
	default:
		t.Fatal("the loopback saw no request")
	}
	select {
	case extra := <-calls:
		t.Fatalf("the loopback saw a second request (%s %s); a speech run makes exactly one", extra.method, extra.path)
	default:
	}
	return got
}

// requireNoSpeechRunCall fails when the loopback saw any request.
func requireNoSpeechRunCall(t *testing.T, calls <-chan speechRunLoopbackCall) {
	t.Helper()
	select {
	case c := <-calls:
		t.Fatalf("the loopback received %s %s (%s), want no request at all", c.method, c.path, c.body)
	default:
	}
}

// newSpeechRunTestServer is the run executor's full wiring (loopback pair, chat
// store, cipher, ChatRuns, selfBaseURL pointing at the real gateway) over an
// audio.cpp application at upstreamURL with a (speech, yes) model mapping. It is
// newImageRunTestServer's speech twin: neither that helper nor
// newSpeechCapableTestServer can drive a speech run on its own, because the first
// has no audio application and the second no chat store or run registry.
//
// The tests that only look at the executor's own request point srv.selfBaseURL at
// a recording stand-in (newSpeechRunLoopback) afterwards; upstreamURL is then
// never reached.
func newSpeechRunTestServer(t *testing.T, upstreamURL string) (*Server, auth.Token, string, portal.ChatStore) {
	t.Helper()
	srv, _, owner, chatID, chats := newSpeechRunTestServerWithDirectory(t, upstreamURL)
	return srv, owner, chatID, chats
}

// newSpeechRunTestServerWithDirectory is newSpeechRunTestServer with the user directory as
// well, the only handle on the store AuthorizeRunAsToken reads, for a test that needs a
// run-as token owned by the chat's user.
func newSpeechRunTestServerWithDirectory(t *testing.T, upstreamURL string) (*Server, *portal.MemoryDirectory, auth.Token, string, portal.ChatStore) {
	t.Helper()
	chats := store.NewMemoryChatStore(0)
	cipher, err := capture.New(testCaptureKey)
	if err != nil {
		t.Fatalf("capture.New: %v", err)
	}
	tokens := auth.NewTokenStore()
	tokens.AddPlainToken(auth.Token{
		ID: "tok_dev", UserID: "usr_dev", Name: "Dev Token",
		Active: true, Scopes: []string{"gateway:use"},
	}, "dev-secret")
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	directory := portal.NewMemoryDirectory(auth.NewTokenStore())
	directory.AddUser(store.User{
		ID: "usr_dev", Email: "dev@example.test", DisplayName: "Dev User",
		Role: "admin", Status: store.UserStatusActive, PreferredLanguage: "de",
		CreatedAt: now, UpdatedAt: now,
	})
	recorder := usage.NewRecorder()
	routeStore := routing.NewMemoryStore()

	ctx := context.Background()
	up, err := url.Parse(upstreamURL)
	if err != nil {
		t.Fatalf("url.Parse(%q): %v", upstreamURL, err)
	}
	port, err := strconv.Atoi(up.Port())
	if err != nil {
		t.Fatalf("upstream port %q: %v", up.Port(), err)
	}
	if err := routeStore.CreateAIServer(ctx, routing.AIServer{ID: "srv-audio", Name: "Audio Upstream", Domain: up.Hostname(), Provider: routing.ProviderAudioCpp, Endpoint: upstreamURL, Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateAIServer: %v", err)
	}
	if err := routeStore.CreateApplication(ctx, routing.Application{ID: "app-audio", ServerID: "srv-audio", Type: routing.ProviderAudioCpp, Port: port, Scheme: up.Scheme, APIFlavors: []string{routing.APIFlavorOpenAIAudio}, Priority: 10, Weight: 50, TimeoutMS: 30000, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
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
		Provider:           provider.NewOpenAICompatibleClient(http.DefaultClient),
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

	owner := auth.Token{UserID: "usr_dev", Scopes: []string{"gateway:use"}}
	created, err := svc.CreateChat(ctx, owner, portal.CreateChatRequest{
		Content: json.RawMessage(`{"settings":{},"messages":[{"id":"u","role":"user","content":"Hello there"}]}`),
	})
	if err != nil {
		t.Fatalf("CreateChat: %v", err)
	}
	return srv, directory, owner, created.ID, chats
}

// speechRunOutcome starts the run, waits for it to end and returns it with its
// terminal snapshot.
func speechRunOutcome(t *testing.T, srv *Server, owner auth.Token, chatID string, prep PrepareRunResult) (*ChatRun, runEvent) {
	t.Helper()
	run, err := srv.startChatRun(owner, chatID, prep)
	if err != nil {
		t.Fatalf("startChatRun: %v", err)
	}
	waitFor(t, func() bool { return run.statusValue() != "running" })
	snap, _, unsub := run.subscribe()
	unsub()
	return run, snap
}

// audioPartURLs returns the url of every audio_url part of a content-parts array,
// failing on a part of the audio shape that is malformed.
func audioPartURLs(t *testing.T, parts json.RawMessage) []string {
	t.Helper()
	var decoded []struct {
		Type     string `json:"type"`
		AudioURL *struct {
			URL string `json:"url"`
		} `json:"audio_url"`
	}
	if err := json.Unmarshal(parts, &decoded); err != nil {
		t.Fatalf("content parts %s are not an array of parts: %v", parts, err)
	}
	var urls []string
	for _, p := range decoded {
		if p.Type != "audio_url" {
			continue
		}
		if p.AudioURL == nil {
			t.Fatalf("an audio_url part without an audio_url object: %s", parts)
		}
		urls = append(urls, p.AudioURL.URL)
	}
	return urls
}

// A speech run makes exactly ONE POST to the gateway's own /v1/audio/speech as
// JSON, carries the shared loopback header set, and sends the pinned body -- the
// model, the last user message's text, the set first-class settings and the
// passthrough -- and nothing else (no chat parameters, no response_format).
func TestSpeechRunPostsOneJSONRequestToAudioSpeech(t *testing.T) {
	wav := speechTestWAV()
	loopback, calls := newSpeechRunLoopback(t, http.StatusOK, "audio/wav", wav)
	srv, owner, chatID, _ := newSpeechRunTestServer(t, loopback.URL)
	srv.selfBaseURL = loopback.URL

	speed := 1.25
	prep := speechRunPrep()
	prep.Settings.Temperature = 0.7
	prep.Settings.MaxTokens = 1234
	prep.History = []portal.ChatAPIMessage{
		{Role: "user", Content: json.RawMessage(`"an older line"`)},
		{Role: "assistant", Content: json.RawMessage(`"ok"`)},
		{Role: "user", Content: json.RawMessage(`[{"type":"text","text":"Read this aloud."}]`)},
	}
	prep.Settings.Audio = &portal.ChatAudioSettings{
		Voice: "alloy",
		Speed: &speed,
		Passthrough: map[string]json.RawMessage{
			"instructions": json.RawMessage(`"Speak warmly."`),
			"seed":         json.RawMessage(`7`),
		},
	}
	if _, snap := speechRunOutcome(t, srv, owner, chatID, prep); snap.Status != "completed" {
		t.Fatalf("status = %q (err %q), want completed", snap.Status, snap.Err)
	}

	got := takeSpeechRunCall(t, calls)
	if got.method != http.MethodPost {
		t.Fatalf("method = %q, want POST", got.method)
	}
	if got.path != "/v1/audio/speech" {
		t.Fatalf("path = %q, want /v1/audio/speech", got.path)
	}
	if got.contentType != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", got.contentType)
	}
	// The shared header set (setRunLoopbackHeaders): the trusted-loopback pair that
	// authenticates the call as the chat's owner, and the chat id as the session.
	if got.header.Get(internalAuthHeaderName) != "test-internal-secret" || got.header.Get(internalUserHeaderName) != "usr_dev" {
		t.Fatalf("the trusted-loopback pair is missing: %v", got.header)
	}
	if got.header.Get(sessionHeaderName) != chatID {
		t.Fatalf("%s = %q, want the chat id %q", sessionHeaderName, got.header.Get(sessionHeaderName), chatID)
	}
	body := decodeSpeechBody(t, got.body)
	want := map[string]any{
		"model":        speechGatewayModel,
		"input":        "Read this aloud.",
		"voice":        "alloy",
		"speed":        1.25,
		"instructions": "Speak warmly.",
		"seed":         7.0,
	}
	if len(body) != len(want) {
		t.Fatalf("body = %s, want exactly %v", got.body, want)
	}
	for k, v := range want {
		if body[k] != v {
			t.Fatalf("body[%q] = %v, want %v: %s", k, body[k], v, got.body)
		}
	}
	for _, key := range []string{"messages", "stream", "stream_options", "temperature", "max_tokens", "response_format"} {
		if _, ok := body[key]; ok {
			t.Fatalf("speech body must not carry %q: %s", key, got.body)
		}
	}
}

// The run-as token and the thread's server override ride the shared header set, so
// a speech turn is billed, captured and routed exactly as the chat and image hops
// are.
func TestSpeechRunLoopbackCarriesRunAsAndServerOverride(t *testing.T) {
	loopback, calls := newSpeechRunLoopback(t, http.StatusOK, "audio/wav", speechTestWAV())
	srv, owner, chatID, _ := newSpeechRunTestServer(t, loopback.URL)
	srv.selfBaseURL = loopback.URL

	prep := speechRunPrep()
	prep.Settings.RunAsTokenID = "tok_dev"
	prep.Settings.ServerOverride = "srv-audio"
	prep.Settings.ServerOverrideForceUnreachable = true
	if _, snap := speechRunOutcome(t, srv, owner, chatID, prep); snap.Status != "completed" {
		t.Fatalf("status = %q (err %q), want completed", snap.Status, snap.Err)
	}
	got := takeSpeechRunCall(t, calls)
	if got.header.Get(runAsHeaderName) != "tok_dev" {
		t.Fatalf("%s = %q, want tok_dev", runAsHeaderName, got.header.Get(runAsHeaderName))
	}
	if got.header.Get(serverOverrideHeaderName) != "srv-audio" || got.header.Get(serverOverrideForceHeaderName) != "1" {
		t.Fatalf("server override headers = %q / %q, want srv-audio / 1", got.header.Get(serverOverrideHeaderName), got.header.Get(serverOverrideForceHeaderName))
	}
}

// The audio/wav the endpoint answers becomes the assistant turn's content: ONE
// audio_url part whose url is a standard-base64 data: URL of exactly those bytes.
// This is the part shape the blob extraction and the portal renderer consume.
func TestSpeechRunTurnCarriesTheWAVAsAnAudioURLPart(t *testing.T) {
	wav := speechTestWAV()
	loopback, _ := newSpeechRunLoopback(t, http.StatusOK, "audio/wav", wav)
	srv, owner, chatID, _ := newSpeechRunTestServer(t, loopback.URL)
	srv.selfBaseURL = loopback.URL

	_, snap := speechRunOutcome(t, srv, owner, chatID, speechRunPrep())
	if snap.Status != "completed" || snap.Err != "" {
		t.Fatalf("status = %q (err %q), want completed", snap.Status, snap.Err)
	}
	if snap.Content != "" {
		t.Fatalf("content = %q, want empty -- a speech run streams no text and must not fabricate any", snap.Content)
	}
	want := `[{"type":"audio_url","audio_url":{"url":"data:audio/wav;base64,` + base64.StdEncoding.EncodeToString(wav) + `"}}]`
	if string(snap.ContentParts) != want {
		t.Fatalf("content parts = %s\nwant            %s", snap.ContentParts, want)
	}

	// The turn is committed complete, not left pending, and its content is still
	// the audio part: the commit moved the WAV into the chat blob store and
	// replaced the part's url, never the part.
	stored, err := srv.Portal.GetChat(context.Background(), owner, chatID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(stored.Content), `"type":"audio_url"`) || !strings.Contains(string(stored.Content), `"status":"complete"`) {
		t.Fatalf("the stored turn must be a complete audio turn: %s", stored.Content)
	}
	if strings.Contains(string(stored.Content), "data:") {
		t.Fatalf("the stored turn still carries the WAV inline: %.300s", stored.Content)
	}
	id := storedAudioBlobID(t, stored.Content)
	got, mediaType, err := srv.Portal.ChatBlobBytes(context.Background(), owner, id)
	if err != nil {
		t.Fatalf("ChatBlobBytes(%s): %v", id, err)
	}
	if mediaType != "audio/wav" || !bytes.Equal(got, wav) {
		t.Fatalf("the blob is %d bytes as %s, want the %d WAV bytes as audio/wav", len(got), mediaType, len(wav))
	}
}

// storedAudioBlobID is the blob id the one audio_url part of a stored chat
// document points at; the url must be the gateway's chat-blob path.
func storedAudioBlobID(t *testing.T, content json.RawMessage) string {
	t.Helper()
	var doc struct {
		Messages []struct {
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(content, &doc); err != nil {
		t.Fatalf("stored document is not JSON: %v", err)
	}
	var urls []string
	for _, m := range doc.Messages {
		var parts []struct {
			Type     string `json:"type"`
			AudioURL struct {
				URL string `json:"url"`
			} `json:"audio_url"`
		}
		if json.Unmarshal(m.Content, &parts) != nil {
			continue
		}
		for _, p := range parts {
			if p.Type == "audio_url" {
				urls = append(urls, p.AudioURL.URL)
			}
		}
	}
	if len(urls) != 1 {
		t.Fatalf("stored audio urls = %v, want exactly one", urls)
	}
	id, ok := strings.CutPrefix(urls[0], chatBlobRoutePrefix)
	if !ok {
		t.Fatalf("stored audio url = %.80q, want a %s path", urls[0], chatBlobRoutePrefix)
	}
	return id
}

// Aliases of audio/wav that a server may legitimately send are accepted and all
// land as audio/wav: the data: URL's media type is the contract the store and the
// renderer key on, so it is one value.
func TestSpeechRunAcceptsTheWAVMediaTypeAliases(t *testing.T) {
	for _, ct := range []string{"audio/wav", "audio/x-wav", "audio/wave", "AUDIO/WAV; charset=binary"} {
		t.Run(ct, func(t *testing.T) {
			loopback, _ := newSpeechRunLoopback(t, http.StatusOK, ct, speechTestWAV())
			srv, owner, chatID, _ := newSpeechRunTestServer(t, loopback.URL)
			srv.selfBaseURL = loopback.URL
			_, snap := speechRunOutcome(t, srv, owner, chatID, speechRunPrep())
			if snap.Status != "completed" {
				t.Fatalf("status = %q (err %q), want completed", snap.Status, snap.Err)
			}
			urls := audioPartURLs(t, snap.ContentParts)
			if len(urls) != 1 || !strings.HasPrefix(urls[0], "data:audio/wav;base64,") {
				t.Fatalf("want one data:audio/wav part, got %v", urls)
			}
		})
	}
}

// A passthrough key the run owns fails the run with a code of its own, and
// NOTHING is sent: forwarding the rest would answer a request the user did not
// make.
func TestSpeechRunRejectsAReservedPassthroughKeyWithoutSendingAnything(t *testing.T) {
	loopback, calls := newSpeechRunLoopback(t, http.StatusOK, "audio/wav", speechTestWAV())
	srv, owner, chatID, _ := newSpeechRunTestServer(t, loopback.URL)
	srv.selfBaseURL = loopback.URL

	prep := speechRunPrep()
	prep.Settings.Audio = &portal.ChatAudioSettings{
		Passthrough: map[string]json.RawMessage{"stream": json.RawMessage(`true`)},
	}
	run, snap := speechRunOutcome(t, srv, owner, chatID, prep)
	if snap.Status != "error" {
		t.Fatalf("status = %q, want error", snap.Status)
	}
	if got := runError(t, run); got != speechRunReservedFieldMessage {
		t.Fatalf("error = %q, want %q", got, speechRunReservedFieldMessage)
	}
	requireNoSpeechRunCall(t, calls)
	if len(snap.ContentParts) != 0 {
		t.Fatalf("a failed run commits no audio, got %s", snap.ContentParts)
	}
}

// An upstream failure ends the run as a FINISHED error turn that names the
// failure, never a blank or pending turn: the gateway's own error code is kept (it
// is what the portal can label), and a body that is not that envelope falls back to
// the status line.
func TestSpeechRunUpstreamErrorFinishesAnErrorTurn(t *testing.T) {
	cases := []struct {
		name        string
		status      int
		contentType string
		body        string
		wantErr     string
	}{
		{
			name: "gateway envelope", status: http.StatusInternalServerError, contentType: "application/json",
			body:    `{"error":{"message":"upstream blew up","type":"upstream_error","code":"speech.upstream_error"}}`,
			wantErr: "speech.upstream_error",
		},
		{
			name: "a request error code", status: http.StatusBadRequest, contentType: "application/json",
			body:    `{"error":{"message":"input is required","type":"invalid_request_error","code":"speech.input_required"}}`,
			wantErr: "speech.input_required",
		},
		{
			name: "not an envelope", status: http.StatusInternalServerError, contentType: "text/plain",
			body:    "boom",
			wantErr: "upstream status 500 Internal Server Error",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			loopback, calls := newSpeechRunLoopback(t, tc.status, tc.contentType, []byte(tc.body))
			srv, owner, chatID, _ := newSpeechRunTestServer(t, loopback.URL)
			srv.selfBaseURL = loopback.URL

			_, snap := speechRunOutcome(t, srv, owner, chatID, speechRunPrep())
			takeSpeechRunCall(t, calls)
			if snap.Status != "error" {
				t.Fatalf("status = %q, want error", snap.Status)
			}
			if snap.Err != tc.wantErr {
				t.Fatalf("error = %q, want %q", snap.Err, tc.wantErr)
			}
			if len(snap.ContentParts) != 0 {
				t.Fatalf("a failed run commits no audio, got %s", snap.ContentParts)
			}
			// The turn is FINISHED in the store as an error turn, not left pending.
			stored, err := srv.Portal.GetChat(context.Background(), owner, chatID)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(stored.Content), `"status":"error"`) || strings.Contains(string(stored.Content), `"status":"pending"`) {
				t.Fatalf("the stored turn must be a finished error turn: %s", stored.Content)
			}
		})
	}
}

// A 2xx the run cannot turn into audio is an ERROR, never an empty success or a
// part labelled audio/wav over bytes nobody checked: a blank bubble would be
// indistinguishable from a bug.
func TestSpeechRunTreatsAnUnusable2xxAsAnError(t *testing.T) {
	cases := []struct {
		name        string
		contentType string
		body        []byte
		wantErr     string
	}{
		{name: "empty body", contentType: "audio/wav", body: nil, wantErr: speechRunNoAudioMessage},
		{name: "a JSON body", contentType: "application/json", body: []byte(`{"ok":true}`), wantErr: speechRunResponseUnreadableMessage},
		{name: "an HTML error page", contentType: "text/html", body: []byte(`<html>bad gateway</html>`), wantErr: speechRunResponseUnreadableMessage},
		{name: "no content type", contentType: "", body: speechTestWAV(), wantErr: speechRunResponseUnreadableMessage},
		{name: "another audio format", contentType: "audio/mpeg", body: []byte("ID3"), wantErr: speechRunResponseUnreadableMessage},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			loopback, _ := newSpeechRunLoopback(t, http.StatusOK, tc.contentType, tc.body)
			srv, owner, chatID, _ := newSpeechRunTestServer(t, loopback.URL)
			srv.selfBaseURL = loopback.URL
			run, snap := speechRunOutcome(t, srv, owner, chatID, speechRunPrep())
			if snap.Status != "error" {
				t.Fatalf("status = %q, want error", snap.Status)
			}
			if got := runError(t, run); got != tc.wantErr {
				t.Fatalf("error = %q, want %q", got, tc.wantErr)
			}
			if len(snap.ContentParts) != 0 {
				t.Fatalf("an unusable response commits no audio, got %s", snap.ContentParts)
			}
		})
	}
}

// A gateway that cannot reach ITSELF must not show a user Go's transport error
// (which embeds the internal URL): the run ends with the dispatch code, and the
// full error stays in the operator's log.
func TestSpeechRunDispatchFailureIsACodeNotARawGoError(t *testing.T) {
	loopback, _ := newSpeechRunLoopback(t, http.StatusOK, "audio/wav", speechTestWAV())
	srv, owner, chatID, _ := newSpeechRunTestServer(t, loopback.URL)
	dead := httptest.NewServer(http.NotFoundHandler())
	srv.selfBaseURL = dead.URL
	dead.Close()

	run, snap := speechRunOutcome(t, srv, owner, chatID, speechRunPrep())
	if snap.Status != "error" {
		t.Fatalf("status = %q, want error", snap.Status)
	}
	got := runError(t, run)
	if got != speechRunDispatchFailedMessage {
		t.Fatalf("error = %q, want the mapped code %q", got, speechRunDispatchFailedMessage)
	}
	if strings.Contains(got, "127.0.0.1") || strings.Contains(got, "dial") {
		t.Fatalf("error %q leaks the internal URL / transport detail", got)
	}
}

// stalledSpeechLoopback is a /v1/audio/speech stand-in that never answers until
// released (or its request ends), for the lifecycle tests that need a run still in
// flight. release is idempotent and must be called before the test returns: closing
// an httptest server waits for its in-flight handlers.
func stalledSpeechLoopback(t *testing.T) (*httptest.Server, func()) {
	t.Helper()
	released := make(chan struct{})
	var once sync.Once
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-released:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(ts.Close)
	return ts, func() { once.Do(func() { close(released) }) }
}

// A speech run is BOUNDED like an image run, and the deadline is not a cancel: the
// user pressed nothing, so the run ends with the timeout code, not the empty
// message a Stop carries.
func TestSpeechRunDeadlineIsBoundedAndIsNotACancel(t *testing.T) {
	if d, bounded := runDeadlineFor(chatRunKindSpeech); !bounded || d != speechRunDeadline {
		t.Fatalf("runDeadlineFor(speech) = (%v, %v), want (%v, true)", d, bounded, speechRunDeadline)
	}
	restore := speechRunDeadline
	speechRunDeadline = 50 * time.Millisecond
	t.Cleanup(func() { speechRunDeadline = restore })

	loopback, release := stalledSpeechLoopback(t)
	defer release()
	srv, owner, chatID, _ := newSpeechRunTestServer(t, loopback.URL)
	srv.selfBaseURL = loopback.URL

	run, snap := speechRunOutcome(t, srv, owner, chatID, speechRunPrep())
	if snap.Status != "error" {
		t.Fatalf("status = %q, want error -- a deadline is not a cancel", snap.Status)
	}
	if got := runError(t, run); got != runTimedOutMessage {
		t.Fatalf("error = %q, want the timeout code %q", got, runTimedOutMessage)
	}
}

// A user Stop on a speech run stays a CANCEL: status canceled with an EMPTY
// message, which is what the UI reads as "the user pressed Stop".
func TestSpeechRunUserCancelStaysACancel(t *testing.T) {
	loopback, release := stalledSpeechLoopback(t)
	defer release()
	srv, owner, chatID, _ := newSpeechRunTestServer(t, loopback.URL)
	srv.selfBaseURL = loopback.URL

	run, err := srv.startChatRun(owner, chatID, speechRunPrep())
	if err != nil {
		t.Fatalf("startChatRun: %v", err)
	}
	if !srv.ChatRuns.cancelChat(owner.UserID, chatID) {
		t.Fatal("cancelChat found no active run to cancel")
	}
	waitFor(t, func() bool { return run.statusValue() != "running" })
	if got := run.statusValue(); got != "canceled" {
		t.Fatalf("status = %q, want canceled", got)
	}
	if got := runError(t, run); got != "" {
		t.Fatalf("error = %q, want empty -- a Stop carries no message", got)
	}
}

// --- end to end ------------------------------------------------------------

// The whole seam, with the real gateway on the loopback hop: the executor's call
// authenticates as the chat's owner, clears the capability gate of the
// (speech, yes) mapping, is relayed to the audio.cpp stub with the model mapped to
// its own id and the settings/passthrough as sent, and the stub's WAV comes back
// byte for byte as the committed audio part.
func TestSpeechRunEndToEndThroughTheGateway(t *testing.T) {
	wav := speechTestWAV()
	upstream, upstreamCalls := newSpeechUpstream(t, wav)
	srv, owner, chatID, _ := newSpeechRunTestServer(t, upstream.URL)

	speed := 0.9
	prep := speechRunPrep()
	prep.Settings.Audio = &portal.ChatAudioSettings{
		Voice:       "alloy",
		Speed:       &speed,
		Passthrough: map[string]json.RawMessage{"instructions": json.RawMessage(`"Speak softly."`)},
	}
	_, snap := speechRunOutcome(t, srv, owner, chatID, prep)
	if snap.Status != "completed" {
		t.Fatalf("status = %q (err %q), want completed", snap.Status, snap.Err)
	}

	var call speechUpstreamCall
	select {
	case call = <-upstreamCalls:
	default:
		t.Fatal("audio.cpp never received the request")
	}
	if call.method != http.MethodPost || call.path != "/v1/audio/speech" {
		t.Fatalf("upstream got %s %s, want POST /v1/audio/speech", call.method, call.path)
	}
	body := decodeSpeechBody(t, call.body)
	if body["model"] != speechUpstreamModel {
		t.Fatalf("upstream model = %v, want the mapped %q", body["model"], speechUpstreamModel)
	}
	if body["input"] != "Hello there" || body["voice"] != "alloy" || body["instructions"] != "Speak softly." {
		t.Fatalf("upstream body = %s", call.body)
	}
	if got, _ := body["speed"].(float64); got != 0.9 {
		t.Fatalf("upstream speed = %v, want 0.9: %s", body["speed"], call.body)
	}

	urls := audioPartURLs(t, snap.ContentParts)
	if len(urls) != 1 {
		t.Fatalf("want one audio part, got %s", snap.ContentParts)
	}
	payload, ok := strings.CutPrefix(urls[0], "data:audio/wav;base64,")
	if !ok {
		t.Fatalf("audio url %.60q is not a data:audio/wav;base64 url", urls[0])
	}
	got, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		t.Fatalf("the audio payload is not standard padded base64: %v", err)
	}
	if string(got) != string(wav) {
		t.Fatalf("the committed audio differs from the WAV audio.cpp produced: % x vs % x", got, wav)
	}
}

// A model that is not speech-capable is refused by the endpoint's own capability
// gate, and that refusal reaches the user as the gateway's code -- not a blank
// turn.
func TestSpeechRunSurfacesTheEndpointsOwnRefusal(t *testing.T) {
	upstream, _ := newSpeechUpstream(t, speechTestWAV())
	srv, owner, chatID, _ := newSpeechRunTestServer(t, upstream.URL)

	prep := speechRunPrep()
	prep.Settings.Model = "no-such-voice-model"
	run, snap := speechRunOutcome(t, srv, owner, chatID, prep)
	if snap.Status != "error" {
		t.Fatalf("status = %q, want error", snap.Status)
	}
	if got := runError(t, run); got == "" || strings.Contains(got, "127.0.0.1") {
		t.Fatalf("error = %q, want the gateway's own error code", got)
	}
}

// --- the buffered turn: split and total cap (#279) ---------------------------
//
// The one-shot WAV a buffered speech run reads is cut into ordered parts exactly as a streamed
// clip is (the stream's constants and caps are reused: a part is streamTestPartSize bytes of
// PCM), and a clip past the total audio cap is the honest "audio too large" error.

// bufferedTestTurn runs bufferedSpeechTurn over a 200 answer carrying body as contentType, on
// a server with the given caps.
func bufferedTestTurn(srv *Server, contentType string, body []byte) (parts json.RawMessage, status, msg string) {
	run := newChatRun("run_buffered", "chat_buffered", "usr_buffered", func() {})
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {contentType}},
		Body:       io.NopCloser(bytes.NewReader(body)),
	}
	return srv.bufferedSpeechTurn(context.Background(), run, resp)
}

// committedWAVs decodes the content array of a completed turn into the WAV of every part.
func committedWAVs(t *testing.T, raw json.RawMessage) [][]byte {
	t.Helper()
	var parts []speechAudioPart
	if err := json.Unmarshal(raw, &parts); err != nil {
		t.Fatalf("the committed content is not a parts array: %v: %.120s", err, raw)
	}
	return partWAVs(t, parts)
}

// A clip of 2.1 parts is committed as three ordered audio_url parts -- two full ones and the
// remainder -- whose audio put end to end is the clip, sample for sample.
func TestBufferedSplitsLongWAV(t *testing.T) {
	pcm := speechPCM(streamTestPartSize*21/10, 3)

	raw, status, msg := bufferedTestTurn(streamTestServer(streamTestAudioCap, streamTestBlobCap), "audio/wav", wavFromPCM16(pcm, partsTestRate))

	if status != "completed" || msg != "" {
		t.Fatalf("outcome = %q %q, want completed with no message", status, msg)
	}
	sizes, joined := dataSizes(t, committedWAVs(t, raw))
	if want := []int{1000, 1000, 100}; !slices.Equal(sizes, want) {
		t.Fatalf("part data sizes = %v, want %v", sizes, want)
	}
	if !bytes.Equal(joined, pcm) {
		t.Fatal("the parts' data chunks put end to end are not the PCM the WAV carried")
	}
}

// A clip past the total cap is the audio-too-large error and commits nothing. The cap is on
// the decoded PCM, as the streamed path measures it, so a clip of exactly the cap -- whose WAV
// is a header longer than the cap -- is still committed, and two more bytes are not.
func TestBufferedOverCap(t *testing.T) {
	srv := streamTestServer(streamTestAudioCap, streamTestBlobCap)

	t.Run("over", func(t *testing.T) {
		raw, status, msg := bufferedTestTurn(srv, "audio/wav", wavFromPCM16(speechPCM(streamTestAudioCap+2, 5), partsTestRate))
		if status != "error" || msg != speechRunAudioTooLargeMessage {
			t.Fatalf("outcome = %q %q, want error %q", status, msg, speechRunAudioTooLargeMessage)
		}
		if len(raw) != 0 {
			t.Fatalf("an over-cap clip committed parts: %.120s", raw)
		}
	})

	t.Run("exactly the cap", func(t *testing.T) {
		pcm := speechPCM(streamTestAudioCap, 5)
		raw, status, msg := bufferedTestTurn(srv, "audio/wav", wavFromPCM16(pcm, partsTestRate))
		if status != "completed" || msg != "" {
			t.Fatalf("outcome = %q %q, want completed: the cap counts PCM, not the WAV header", status, msg)
		}
		sizes, joined := dataSizes(t, committedWAVs(t, raw))
		if want := []int{1000, 1000, 1000, 1000, 1000}; !slices.Equal(sizes, want) {
			t.Fatalf("part data sizes = %v, want %v", sizes, want)
		}
		if !bytes.Equal(joined, pcm) {
			t.Fatal("the parts' data chunks put end to end are not the PCM the WAV carried")
		}
	})
}

// A clip shorter than one part is one part, exactly the WAV the single-part turn has always
// committed: chunking changes nothing for a short answer.
func TestBufferedShortWAVOnePart(t *testing.T) {
	wav := wavFromPCM16(speechPCM(600, 11), partsTestRate)

	raw, status, msg := bufferedTestTurn(streamTestServer(streamTestAudioCap, streamTestBlobCap), "audio/wav", wav)

	if status != "completed" || msg != "" {
		t.Fatalf("outcome = %q %q, want completed", status, msg)
	}
	wavs := committedWAVs(t, raw)
	if len(wavs) != 1 || !bytes.Equal(wavs[0], wav) {
		t.Fatalf("committed %d parts, want the one WAV audio.cpp answered, byte for byte", len(wavs))
	}
}

// A WAV that is not mono 16-bit PCM is not cut -- the cut is made on 16-bit mono samples and
// would split any other format mid-frame -- and is committed as the one whole-WAV part it
// always was, however long it is. So is a body that is not a WAV at all under an audio
// content type: what the endpoint labelled audio is still committed as the endpoint sent it.
func TestBufferedKeepsAnyOtherFormatWhole(t *testing.T) {
	const rate = partsTestRate
	longData := speechPCM(streamTestPartSize*3, 9) // over two parts of 16-bit mono
	cases := map[string][]byte{
		"stereo 16-bit": wavFile(t, wavFmtChunk(t, rate, 2, 16), wavChunk(t, "data", uint32(len(longData)), longData)),
		"mono 8-bit":    wavFile(t, wavFmtChunk(t, rate, 1, 8), wavChunk(t, "data", uint32(len(longData)), longData)),
		"mono float32":  wavFile(t, wavFmtChunkTagged(t, 3, 1, rate, 32), wavChunk(t, "data", uint32(len(longData)), longData)),
		"not a WAV":     []byte("ID3 this is not a RIFF container"),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			raw, status, msg := bufferedTestTurn(streamTestServer(streamTestAudioCap, streamTestBlobCap), "audio/wav", body)
			if status != "completed" || msg != "" {
				t.Fatalf("outcome = %q %q, want completed", status, msg)
			}
			wavs := committedWAVs(t, raw)
			if len(wavs) != 1 || !bytes.Equal(wavs[0], body) {
				t.Fatalf("committed %d parts, want the one body as it came, byte for byte", len(wavs))
			}
		})
	}
}

// A clip with no whole sample is the no-audio error, as for a stream, and never a header over
// nothing -- the blank bubble a part without audio would be.
func TestBufferedClipWithoutASampleIsNoAudio(t *testing.T) {
	for name, wav := range map[string][]byte{
		"a header and no data": wavFromPCM16(nil, partsTestRate),
		"half a sample":        wavFile(t, wavFmtChunk(t, partsTestRate, 1, 16), wavChunk(t, "data", 1, []byte{0x7f})),
	} {
		t.Run(name, func(t *testing.T) {
			raw, status, msg := bufferedTestTurn(streamTestServer(streamTestAudioCap, streamTestBlobCap), "audio/wav", wav)
			if status != "error" || msg != speechRunNoAudioMessage {
				t.Fatalf("outcome = %q %q, want error %q", status, msg, speechRunNoAudioMessage)
			}
			if len(raw) != 0 {
				t.Fatalf("a clip without audio committed parts: %.120s", raw)
			}
		})
	}
}

// A header that promises more audio than the body holds -- a truncated answer, or a size left
// at the format's maximum by a writer that did not know the length -- is cut at the audio that
// is there: the declared size is the header's word, and neither sizes an allocation nor
// invents samples.
func TestBufferedTrustsTheBodyOverTheDeclaredSize(t *testing.T) {
	pcm := speechPCM(1500, 7)
	for name, declared := range map[string]uint32{"truncated": 100_000, "maximum": 0xFFFFFFFF} {
		t.Run(name, func(t *testing.T) {
			wav := wavFile(t, wavFmtChunk(t, partsTestRate, 1, 16), wavChunk(t, "data", declared, pcm))

			raw, status, msg := bufferedTestTurn(streamTestServer(streamTestAudioCap, streamTestBlobCap), "audio/wav", wav)

			if status != "completed" || msg != "" {
				t.Fatalf("outcome = %q %q, want completed", status, msg)
			}
			sizes, joined := dataSizes(t, committedWAVs(t, raw))
			if want := []int{1000, 500}; !slices.Equal(sizes, want) {
				t.Fatalf("part data sizes = %v, want %v", sizes, want)
			}
			if !bytes.Equal(joined, pcm) {
				t.Fatal("the parts' data chunks put end to end are not the audio the body held")
			}
		})
	}
}

// A Server not built by New has no caps of its own (0) and means the defaults, so a modest
// clip is one part and not "over a cap of nothing".
func TestBufferedBareServerUsesTheDefaultCaps(t *testing.T) {
	wav := wavFromPCM16(speechPCM(600, 11), partsTestRate)

	raw, status, msg := bufferedTestTurn(&Server{}, "audio/wav", wav)

	if status != "completed" || msg != "" {
		t.Fatalf("outcome = %q %q, want completed", status, msg)
	}
	if wavs := committedWAVs(t, raw); len(wavs) != 1 || !bytes.Equal(wavs[0], wav) {
		t.Fatalf("committed %d parts, want the one WAV, byte for byte", len(wavs))
	}
}
