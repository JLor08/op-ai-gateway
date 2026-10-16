// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"op-ai-gateway/internal/apierror"
	"op-ai-gateway/internal/auth"
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
)

const (
	// speechGatewayModel is the name a client sends; speechUpstreamModel the
	// model id audio.cpp serves. They differ so the relay's model rewrite is
	// observable at the upstream.
	speechGatewayModel  = "tts-voice"
	speechUpstreamModel = "pocket_tts_english_bf16"
)

// TestApiFlavorSpeechIsTheRoutingAudioFlavor pins the local alias to the coarse
// routing flavor it mirrors, so the two cannot drift apart: candidacy,
// targetServesFlavor and the usage label all key on this exact string.
func TestApiFlavorSpeechIsTheRoutingAudioFlavor(t *testing.T) {
	// Both sides are untyped constants; routing them through variables keeps
	// go vet from flagging the (intentional) constant comparison.
	alias, routingFlavor := apiFlavorSpeech, routing.APIFlavorOpenAIAudio
	if alias != "openai_audio" || alias != routingFlavor {
		t.Fatalf("apiFlavorSpeech = %q, want %q (routing.APIFlavorOpenAIAudio = %q)", alias, "openai_audio", routingFlavor)
	}
	if got := routing.NormalizeAPIFlavor(alias); got != alias {
		t.Fatalf("NormalizeAPIFlavor(%q) = %q, want it to fold to itself", alias, got)
	}
}

// TestValidateSpeechRequest pins the request shape the handler owns, including
// the wav-only and no-streaming rules. want is the expected error code, "" for a
// request that must pass.
func TestValidateSpeechRequest(t *testing.T) {
	cases := []struct {
		name  string
		body  string
		model string
		want  string
	}{
		{"minimal request", `{"model":"m","input":"hi"}`, "m", ""},
		{"every optional field", `{"model":"m","input":"hi","voice":"alba","instructions":"calm","speed":1.2,"response_format":"wav"}`, "m", ""},
		{"response_format absent", `{"model":"m","input":"hi","voice":"alba"}`, "m", ""},
		{"response_format null is absent", `{"model":"m","input":"hi","response_format":null}`, "m", ""},
		{"response_format empty string is absent", `{"model":"m","input":"hi","response_format":""}`, "m", ""},
		{"stream false", `{"model":"m","input":"hi","stream":false}`, "m", ""},
		{"stream null is absent", `{"model":"m","input":"hi","stream":null}`, "m", ""},
		{"stream_format null is absent", `{"model":"m","input":"hi","stream_format":null}`, "m", ""},

		{"model missing", `{"input":"hi"}`, "", speechModelRequired},
		{"model blank", `{"model":"  ","input":"hi"}`, "  ", speechModelRequired},
		{"input missing", `{"model":"m"}`, "m", speechInputRequired},
		{"input blank", `{"model":"m","input":"   "}`, "m", speechInputRequired},
		{"input empty", `{"model":"m","input":""}`, "m", speechInputRequired},
		{"input not a string", `{"model":"m","input":123}`, "m", speechInputRequired},
		{"input is an array", `{"model":"m","input":["hi"]}`, "m", speechInputRequired},
		{"model is checked before input", `{"response_format":"mp3"}`, "", speechModelRequired},
		{"input is checked before response_format", `{"model":"m","response_format":"mp3"}`, "m", speechInputRequired},

		{"mp3", `{"model":"m","input":"hi","response_format":"mp3"}`, "m", speechResponseFormatUnsupported},
		{"opus", `{"model":"m","input":"hi","response_format":"opus"}`, "m", speechResponseFormatUnsupported},
		{"flac", `{"model":"m","input":"hi","response_format":"flac"}`, "m", speechResponseFormatUnsupported},
		{"aac", `{"model":"m","input":"hi","response_format":"aac"}`, "m", speechResponseFormatUnsupported},
		{"pcm", `{"model":"m","input":"hi","response_format":"pcm"}`, "m", speechResponseFormatUnsupported},
		{"json", `{"model":"m","input":"hi","response_format":"json"}`, "m", speechResponseFormatUnsupported},
		{"b64_json", `{"model":"m","input":"hi","response_format":"b64_json"}`, "m", speechResponseFormatUnsupported},
		{"response_format is an array", `{"model":"m","input":"hi","response_format":["wav"]}`, "m", speechResponseFormatUnsupported},
		{"response_format is a number", `{"model":"m","input":"hi","response_format":123}`, "m", speechResponseFormatUnsupported},

		{"stream true", `{"model":"m","input":"hi","stream":true}`, "m", speechStreamUnsupported},
		{"stream is a string", `{"model":"m","input":"hi","stream":"true"}`, "m", speechStreamUnsupported},
		{"stream_format sse", `{"model":"m","input":"hi","stream_format":"sse"}`, "m", speechStreamUnsupported},
		{"stream_format audio", `{"model":"m","input":"hi","stream_format":"audio"}`, "m", speechStreamUnsupported},
		{"stream_format beside stream false", `{"model":"m","input":"hi","stream":false,"stream_format":"audio"}`, "m", speechStreamUnsupported},
		{"response_format is checked before stream", `{"model":"m","input":"hi","response_format":"mp3","stream":true}`, "m", speechResponseFormatUnsupported},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateSpeechRequest([]byte(tc.body), tc.model)
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("validateSpeechRequest = %q (%s), want it accepted", err.Code, err.Message)
			case tc.want != "" && err == nil:
				t.Fatalf("validateSpeechRequest accepted the request, want code %q", tc.want)
			case err != nil && err.Code != tc.want:
				t.Fatalf("code = %q, want %q (message: %s)", err.Code, tc.want, err.Message)
			}
		})
	}
}

// The codes are API surface (clients branch on them), so they are pinned by
// value, not only by the constants' own names.
func TestSpeechErrorCodesAreStable(t *testing.T) {
	for got, want := range map[string]string{
		speechModelRequired:             "speech.model_required",
		speechInputRequired:             "speech.input_required",
		speechResponseFormatUnsupported: "speech.response_format_unsupported",
		speechStreamUnsupported:         "speech.stream_unsupported",
	} {
		if got != want {
			t.Errorf("code = %q, want %q", got, want)
		}
	}
}

// postSpeech sends a speech request through the real mux with the dev bearer.
func postSpeech(t *testing.T, srv *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	return postSpeechWithHeaders(t, srv, "/v1/audio/speech", body, map[string]string{"Authorization": "Bearer dev-secret"})
}

// postSpeechWithHeaders is postSpeech's sibling for the auth tests: the caller
// owns the path and the headers. postImagesWithHeaders is generic despite its
// name (path, body and headers are all parameters), so it is the one copy.
func postSpeechWithHeaders(t *testing.T, srv *Server, path, body string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	return postImagesWithHeaders(t, srv, path, body, hdr)
}

// speechTestWAV is a small RIFF/WAVE body ending in bytes that are neither valid
// UTF-8 nor valid JSON, so a relay that decoded, re-encoded or text-handled the
// body in any way would corrupt it.
func speechTestWAV() []byte {
	wav := []byte("RIFF\x2c\x00\x00\x00WAVEfmt \x10\x00\x00\x00\x01\x00\x01\x00\x80\x3e\x00\x00\x00\x7d\x00\x00\x02\x00\x10\x00data\x08\x00\x00\x00")
	return append(wav, 0x00, 0xff, 0xfe, 0x80, 0x7f, 0x00, 0x01, 0xc3)
}

// speechUpstreamCall is one request an audio.cpp stub received.
type speechUpstreamCall struct {
	method      string
	path        string
	contentType string
	body        []byte
}

// newSpeechUpstream starts an audio.cpp-shaped upstream that answers every
// request with wav as audio/wav, with the metering header the real server sets.
// calls receives each request it saw, over a channel so the test reads them
// race-free after the response completes.
func newSpeechUpstream(t *testing.T, wav []byte) (*httptest.Server, <-chan speechUpstreamCall) {
	t.Helper()
	calls := make(chan speechUpstreamCall, 16)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		calls <- speechUpstreamCall{method: r.Method, path: r.URL.Path, contentType: r.Header.Get("Content-Type"), body: body}
		w.Header().Set("Content-Type", "audio/wav")
		w.Header().Set("X-AudioCPP-Audio-Duration-Ms", "1500")
		_, _ = w.Write(wav)
	}))
	t.Cleanup(up.Close)
	return up, calls
}

// requireNoSpeechUpstreamCall fails when the stub saw a request.
func requireNoSpeechUpstreamCall(t *testing.T, calls <-chan speechUpstreamCall) {
	t.Helper()
	select {
	case c := <-calls:
		t.Fatalf("the upstream received %s %s, want no call", c.method, c.path)
	default:
	}
}

// newSpeechCapableTestServer builds a server over the shared qwen-coder chat
// seed plus one audio.cpp application pointing at upstreamURL, with three
// mappings that are the three verdict states the gate tells apart:
//
//   - tts-voice: a (speech, yes) verdict -- the only one that serves;
//   - asr-model: a (speech, no) verdict, the bidirectional probe's answer for a
//     loaded model whose task is not speech;
//   - unprobed-voice: no row at all, the state of a model audio.cpp has not
//     loaded yet.
//
// The Provider is a REAL OpenAI-compatible client making a genuine HTTP round
// trip (the mock never reaches a stub upstream; see newImageCapableTestServer).
// The server also wires the internal loopback pair and its user lookup, and the
// returned directory lets a test seed a run-as token; the dev bearer works too.
func newSpeechCapableTestServer(t *testing.T, upstreamURL string) (*Server, *portal.MemoryDirectory) {
	t.Helper()
	tokens := auth.NewTokenStore()
	directory := portal.NewMemoryDirectory(tokens)
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	directory.AddUser(store.User{ID: "usr_dev", Email: "dev@example.test", DisplayName: "Dev User", Role: "admin", Status: store.UserStatusActive, PreferredLanguage: "de", CreatedAt: now, UpdatedAt: now})
	if err := directory.CreatePlainToken(context.Background(), store.TokenRecord{ID: "tok_dev", UserID: "usr_dev", Name: "Dev Token", Status: store.TokenStatusActive, Scopes: `["gateway:use","admin"]`, CreatedAt: now, UpdatedAt: now}, "dev-secret"); err != nil {
		t.Fatalf("CreatePlainToken: %v", err)
	}
	recorder := usage.NewRecorder()
	routeStore := routing.NewMemoryStore()
	seedGatewayTestRoutes(routeStore, now)

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
	for _, m := range []struct{ id, gateway, upstream, verdict string }{
		{"route-tts", speechGatewayModel, speechUpstreamModel, routing.CapabilityYes},
		{"route-asr", "asr-model", "whisper_x", routing.CapabilityNo},
		{"route-unprobed", "unprobed-voice", "not_loaded_yet", ""},
	} {
		if err := routeStore.CreateMapping(ctx, routing.ModelMapping{ID: m.id, ApplicationID: "app-audio", GatewayModelName: m.gateway, AppModelName: m.upstream, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("CreateMapping %s: %v", m.id, err)
		}
		if m.verdict == "" {
			continue
		}
		if err := routeStore.UpsertMappingCapabilities(ctx, m.id, []routing.CapabilityRow{{Capability: routing.CapabilitySpeech, Verdict: m.verdict, Source: routing.CapabilitySourceAudioCppModels, CheckedAt: now}}); err != nil {
			t.Fatalf("UpsertMappingCapabilities %s: %v", m.id, err)
		}
	}
	if err := routeStore.UpsertTelemetry(ctx, routing.ServerTelemetry{ServerID: "srv-audio", ReportedAt: now, LatencyMS: 100, ProviderHealth: `{}`, Capabilities: `{}`, RawSummary: `{}`, UpdatedAt: now}); err != nil {
		t.Fatalf("UpsertTelemetry: %v", err)
	}

	return New(ServerDeps{
		Tokens:             tokens,
		Usage:              recorder,
		Provider:           provider.NewOpenAICompatibleClient(http.DefaultClient),
		Routes:             routeStore,
		Portal:             portal.NewService(portal.ServiceDeps{Users: directory, Tokens: directory, Usage: recorder, Routes: routeStore, Clock: func() time.Time { return now }, ModelLister: provider.NewMock()}),
		InternalAuthSecret: "s3cret",
		Users:              directory,
	}), directory
}

// A speech-capable model is served end to end: the WAV the audio.cpp stub
// produces reaches the client byte for byte under its own audio/wav
// Content-Type, the upstream is called on /v1/audio/speech with the model
// mapped to its own name and every other field untouched, and the usage row is
// labelled as a speech request (flavor and both paths).
func TestSpeechRelaysAWAVFromASpeechCapableModel(t *testing.T) {
	wav := speechTestWAV()
	up, calls := newSpeechUpstream(t, wav)
	srv, _ := newSpeechCapableTestServer(t, up.URL)

	rec := postSpeech(t, srv, `{"model":"tts-voice","input":"Hello there.","voice":"alba","speed":1.25}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %q", rec.Code, rec.Body.Bytes())
	}
	if got := rec.Header().Get("Content-Type"); got != "audio/wav" {
		t.Fatalf("Content-Type = %q, want audio/wav relayed from the upstream", got)
	}
	if !bytes.Equal(rec.Body.Bytes(), wav) {
		t.Fatalf("body = %x, want the upstream WAV %x byte for byte", rec.Body.Bytes(), wav)
	}

	var call speechUpstreamCall
	select {
	case call = <-calls:
	default:
		t.Fatal("the upstream was never called")
	}
	if call.method != http.MethodPost || call.path != "/v1/audio/speech" {
		t.Fatalf("upstream call = %s %s, want POST /v1/audio/speech", call.method, call.path)
	}
	if call.contentType != "application/json" {
		t.Fatalf("upstream Content-Type = %q, want application/json", call.contentType)
	}
	var sent map[string]any
	if err := json.Unmarshal(call.body, &sent); err != nil {
		t.Fatalf("upstream body is not JSON: %v (%s)", err, call.body)
	}
	if sent["model"] != speechUpstreamModel {
		t.Fatalf("upstream model = %v, want the mapped name %q", sent["model"], speechUpstreamModel)
	}
	if sent["input"] != "Hello there." || sent["voice"] != "alba" || sent["speed"] != 1.25 {
		t.Fatalf("upstream body = %v, want input, voice and speed relayed untouched", sent)
	}

	got := lastUsageEvent(t, srv)
	if got.APIFlavor != apiFlavorSpeech {
		t.Fatalf("APIFlavor = %q, want exactly %q (upstreamPath matches by string equality)", got.APIFlavor, apiFlavorSpeech)
	}
	if got.ReqPath != "/v1/audio/speech" || got.ProviderPath != "/v1/audio/speech" {
		t.Fatalf("ReqPath/ProviderPath = %q/%q, want /v1/audio/speech for both", got.ReqPath, got.ProviderPath)
	}
	if got.Model != speechGatewayModel || got.ProviderModel != speechUpstreamModel {
		t.Fatalf("Model/ProviderModel = %q/%q, want %q/%q", got.Model, got.ProviderModel, speechGatewayModel, speechUpstreamModel)
	}
	if got.HTTPStatus != http.StatusOK || got.Status != "success" || got.ContentType != "audio/wav" {
		t.Fatalf("usage row = status %d/%q, content type %q, want 200/success/audio/wav", got.HTTPStatus, got.Status, got.ContentType)
	}
}

// response_format "wav" (and an explicit stream:false) are the two values this
// endpoint already does, so they are accepted rather than refused.
func TestSpeechAcceptsTheValuesItAlreadyDoes(t *testing.T) {
	up, calls := newSpeechUpstream(t, speechTestWAV())
	srv, _ := newSpeechCapableTestServer(t, up.URL)

	for _, body := range []string{
		`{"model":"tts-voice","input":"hi","response_format":"wav"}`,
		`{"model":"tts-voice","input":"hi","stream":false}`,
	} {
		rec := postSpeech(t, srv, body)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status = %d, want 200; body = %q", body, rec.Code, rec.Body.Bytes())
		}
		select {
		case <-calls:
		default:
			t.Fatalf("%s: the upstream was never called", body)
		}
	}
}

// A request this endpoint cannot honor is a gateway 400 BEFORE anything is
// resolved or relayed: audio.cpp answers a client error with a 500, which the
// gateway must not relay, and a refused request spends no admission or usage.
func TestSpeechRejectsAnInvalidRequestBeforeRouting(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		wantCode string
	}{
		{"missing model", `{"input":"hi"}`, "speech.model_required"},
		{"missing input", `{"model":"tts-voice"}`, "speech.input_required"},
		{"mp3", `{"model":"tts-voice","input":"hi","response_format":"mp3"}`, "speech.response_format_unsupported"},
		{"b64_json", `{"model":"tts-voice","input":"hi","response_format":"b64_json"}`, "speech.response_format_unsupported"},
		{"stream", `{"model":"tts-voice","input":"hi","stream":true}`, "speech.stream_unsupported"},
		{"stream_format", `{"model":"tts-voice","input":"hi","stream_format":"sse"}`, "speech.stream_unsupported"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			up, calls := newSpeechUpstream(t, speechTestWAV())
			srv, _ := newSpeechCapableTestServer(t, up.URL)

			rec := postSpeech(t, srv, tc.body)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
			}
			requireErrorCode(t, rec.Body.String(), tc.wantCode)
			requireNoSpeechUpstreamCall(t, calls)
			if events := srv.Usage.All(); len(events) != 0 {
				t.Fatalf("a rejected request recorded usage %+v, want none", events)
			}
		})
	}
}

// The capability gate, end to end through the handler: only a (speech, yes)
// mapping is served. A model whose probed task is not speech (an explicit no)
// and one that was never probed (no row: the gate refuses on unknown, it does
// not fail open) are refused as not capable; a chat model on a text
// application and an unknown model are no route at all. No refusal reaches the
// upstream, and each records a usage row that claims no upstream call.
func TestSpeechRefusesAModelWithoutASpeechVerdict(t *testing.T) {
	cases := []struct {
		name     string
		model    string
		wantCode string
	}{
		{"an audio model whose task is not speech", "asr-model", "routing.model_not_capable"},
		{"an audio model that was never probed", "unprobed-voice", "routing.model_not_capable"},
		{"a chat model on a text application", "qwen-coder", "routing.no_model_route"},
		{"an unknown model", "no-such-voice", "routing.no_model_route"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			up, calls := newSpeechUpstream(t, speechTestWAV())
			srv, _ := newSpeechCapableTestServer(t, up.URL)

			rec := postSpeech(t, srv, `{"model":"`+tc.model+`","input":"hi"}`)

			if rec.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404; body = %s", rec.Code, rec.Body.String())
			}
			requireErrorCode(t, rec.Body.String(), tc.wantCode)
			requireNoSpeechUpstreamCall(t, calls)

			events := srv.Usage.All()
			if len(events) != 1 {
				t.Fatalf("usage events = %+v, want exactly one refusal row", events)
			}
			got := events[0]
			if got.ReqPath != "/v1/audio/speech" || got.APIFlavor != apiFlavorSpeech {
				t.Fatalf("ReqPath/APIFlavor = %q/%q, want /v1/audio/speech and %q", got.ReqPath, got.APIFlavor, apiFlavorSpeech)
			}
			if got.HTTPStatus != http.StatusNotFound || got.Status != "error" || got.ErrorCode != tc.wantCode {
				t.Fatalf("row = status %d/%q/%q, want 404/error/%s", got.HTTPStatus, got.Status, got.ErrorCode, tc.wantCode)
			}
			// No upstream call was made, so the row must not claim one: a populated
			// Host would make the energy reconciler price it against a server that
			// never saw the request.
			if got.Host != "" || got.RouteID != "" || got.ProviderPath != "" {
				t.Fatalf("row = %+v, want Host/RouteID/ProviderPath empty", got)
			}
		})
	}
}

// The method is checked before anything else.
func TestSpeechRejectsAGet(t *testing.T) {
	srv, _ := newSpeechCapableTestServer(t, "http://127.0.0.1:1")
	req := httptest.NewRequest(http.MethodGet, "/v1/audio/speech", nil)
	req.Header.Set("Authorization", "Bearer dev-secret")
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != http.MethodPost {
		t.Fatalf("status = %d, Allow = %q, want 405 with Allow: POST", rec.Code, rec.Header().Get("Allow"))
	}
}

// A request with no credentials is refused before any body handling.
func TestSpeechRequiresAuthentication(t *testing.T) {
	up, calls := newSpeechUpstream(t, speechTestWAV())
	srv, _ := newSpeechCapableTestServer(t, up.URL)

	rec := postSpeechWithHeaders(t, srv, "/v1/audio/speech", `{"model":"tts-voice","input":"hi"}`, nil)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body = %s", rec.Code, rec.Body.String())
	}
	requireNoSpeechUpstreamCall(t, calls)
}

// A bearer token and the internal trusted-loopback pair are the two ways in, on
// the endpoint's path and on its /openai alias alike.
func TestSpeechAcceptsABearerAndTheInternalLoopback(t *testing.T) {
	up, _ := newSpeechUpstream(t, speechTestWAV())
	srv, _ := newSpeechCapableTestServer(t, up.URL)
	body := `{"model":"tts-voice","input":"hi"}`

	for _, path := range []string{"/v1/audio/speech", "/openai/v1/audio/speech"} {
		for name, hdr := range map[string]map[string]string{
			"bearer":   {"Authorization": "Bearer dev-secret"},
			"loopback": {internalAuthHeaderName: "s3cret", internalUserHeaderName: "usr_dev"},
		} {
			rec := postSpeechWithHeaders(t, srv, path, body, hdr)
			if rec.Code != http.StatusOK {
				t.Fatalf("%s via %s: status = %d, want 200; body = %q", path, name, rec.Code, rec.Body.Bytes())
			}
		}
	}
}

// THE BOUNDARY: a browser session cookie must NOT reach this endpoint (a logged-in
// browser never calls an inference endpoint directly; AGENTS.md). newChatTestServer
// wires a real Account and loginCookie mints a real session. A 401 proves auth
// refused: had the cookie been accepted the request would have failed later and
// differently (the capability gate, a 404), never with a 401.
func TestSpeechRefusesASessionCookie(t *testing.T) {
	srv, dir := newChatTestServer(t)
	seedLoginUser(t, dir, "usr_speech", "speech@example.test", "password-1", "user")
	cookie := loginCookie(t, srv, "speech@example.test", "password-1")

	rec := postSpeechWithHeaders(t, srv, "/v1/audio/speech",
		`{"model":"tts-voice","input":"hi"}`,
		map[string]string{"Cookie": cookie.Name + "=" + cookie.Value, csrfHeaderName: "1"})

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 from a real session cookie: %s", rec.Code, rec.Body.String())
	}
}

// The run-as block is copied from the images handler, so it is pinned here too:
// a loopback request naming a token the loopback user owns is attributed to that
// token, not to the bare loopback principal.
func TestSpeechRunAsTokenAttributesUsage(t *testing.T) {
	up, _ := newSpeechUpstream(t, speechTestWAV())
	srv, dir := newSpeechCapableTestServer(t, up.URL)
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	if err := dir.CreatePlainToken(context.Background(), store.TokenRecord{ID: "tok_ra_speech", UserID: "usr_dev", Name: "RA Speech", Status: store.TokenStatusActive, Scopes: `["gateway:use"]`, CreatedAt: now, UpdatedAt: now}, "ra-secret"); err != nil {
		t.Fatalf("seed run-as token: %v", err)
	}

	rec := postSpeechWithHeaders(t, srv, "/v1/audio/speech", `{"model":"tts-voice","input":"hi"}`,
		map[string]string{internalAuthHeaderName: "s3cret", internalUserHeaderName: "usr_dev", runAsHeaderName: "tok_ra_speech"})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %q", rec.Code, rec.Body.Bytes())
	}
	if got := lastUsageEvent(t, srv).TokenID; got != "tok_ra_speech" {
		t.Fatalf("TokenID = %q, want tok_ra_speech (the run-as token, not the bare loopback principal)", got)
	}
}

// Naming a run-as token the loopback user does not own is a 403, refused rather
// than downgraded to the bare principal, and nothing reaches the upstream or the
// usage log.
func TestSpeechRunAsTokenForbiddenForAnUnownedToken(t *testing.T) {
	up, calls := newSpeechUpstream(t, speechTestWAV())
	srv, dir := newSpeechCapableTestServer(t, up.URL)
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	dir.AddUser(store.User{ID: "usr_other", Email: "other@example.test", DisplayName: "Other User", Role: "user", Status: store.UserStatusActive, PreferredLanguage: "de", CreatedAt: now, UpdatedAt: now})
	if err := dir.CreatePlainToken(context.Background(), store.TokenRecord{ID: "tok_other_speech", UserID: "usr_other", Name: "Other", Status: store.TokenStatusActive, Scopes: `["gateway:use"]`, CreatedAt: now, UpdatedAt: now}, "other-secret"); err != nil {
		t.Fatalf("seed token: %v", err)
	}

	rec := postSpeechWithHeaders(t, srv, "/v1/audio/speech", `{"model":"tts-voice","input":"hi"}`,
		map[string]string{internalAuthHeaderName: "s3cret", internalUserHeaderName: "usr_dev", runAsHeaderName: "tok_other_speech"})

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body = %s", rec.Code, rec.Body.String())
	}
	requireErrorCode(t, rec.Body.String(), "portal.token_forbidden")
	requireNoSpeechUpstreamCall(t, calls)
	if events := srv.Usage.All(); len(events) != 0 {
		t.Fatalf("a forbidden run-as recorded usage %+v, want none", events)
	}
}

// A bare &Server{} (no Portal) must refuse a run-as request rather than panic or
// proceed as the bare loopback principal.
func TestSpeechRunAsWithoutPortalIsRefused(t *testing.T) {
	s := &Server{internalAuthSecret: "s3cret", users: fakeUserLookup{
		"usr_1": {ID: "usr_1", Role: "user"},
	}}
	r := httptest.NewRequest(http.MethodPost, "/v1/audio/speech", strings.NewReader(`{"model":"tts-voice","input":"hi"}`))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set(internalAuthHeaderName, "s3cret")
	r.Header.Set(internalUserHeaderName, "usr_1")
	r.Header.Set(runAsHeaderName, "tok_whatever")
	w := httptest.NewRecorder()

	s.handleOpenAISpeech(w, r)

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d body = %s, want 403", w.Code, w.Body.String())
	}
	requireErrorCode(t, w.Body.String(), "portal.token_forbidden")
}

// newServerAgentSpeechSpecTestServer seeds a server_agent application candidacy
// ADMITS for speech (app-level flavors include openai_audio, and the mapping
// carries (speech, yes)) whose mapping's runtime spec carries specFlavors. The
// spec is the per-model authority for a server_agent mapping, known only after
// the resolve, so the relay's own effective-flavor check decides whether the
// request is served.
func newServerAgentSpeechSpecTestServer(t *testing.T, prov provider.Client, specFlavors []string) *Server {
	t.Helper()
	tokens := auth.NewTokenStore()
	directory := portal.NewMemoryDirectory(tokens)
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	directory.AddUser(store.User{ID: "usr_dev", Email: "dev@example.test", DisplayName: "Dev User", Role: "admin", Status: store.UserStatusActive, PreferredLanguage: "de", CreatedAt: now, UpdatedAt: now})
	if err := directory.CreatePlainToken(context.Background(), store.TokenRecord{ID: "tok_dev", UserID: "usr_dev", Name: "Dev Token", Status: store.TokenStatusActive, Scopes: `["gateway:use","admin"]`, CreatedAt: now, UpdatedAt: now}, "dev-secret"); err != nil {
		t.Fatalf("CreatePlainToken: %v", err)
	}
	recorder := usage.NewRecorder()
	routeStore := routing.NewMemoryStore()
	ctx := context.Background()
	if err := routeStore.CreateAIServer(ctx, routing.AIServer{ID: "srv-agent-audio", Name: "Agent Audio", Domain: "agent-audio.example.test", Provider: routing.ProviderVLLM, Endpoint: "http://agent-audio.example.test:8081", Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateAIServer: %v", err)
	}
	if err := routeStore.CreateApplication(ctx, routing.Application{ID: "app-agent-audio", ServerID: "srv-agent-audio", Type: routing.ProviderServerAgent, Port: 8081, Scheme: "http", APIFlavors: []string{routing.APIFlavorOpenAI, routing.APIFlavorOpenAIAudio}, Priority: 10, Weight: 50, TimeoutMS: 600000, Status: routing.ServerStatusActive, ResponsesMode: routing.EndpointModePassthrough, MessagesMode: routing.EndpointModePassthrough, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateApplication: %v", err)
	}
	if err := routeStore.CreateMapping(ctx, routing.ModelMapping{ID: "route-agent-audio", ApplicationID: "app-agent-audio", GatewayModelName: "agent-voice", AppModelName: "agent-voice", Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateMapping: %v", err)
	}
	if err := routeStore.UpsertMappingCapabilities(ctx, "route-agent-audio", []routing.CapabilityRow{{Capability: routing.CapabilitySpeech, Verdict: routing.CapabilityYes, Source: routing.CapabilitySourceManual, CheckedAt: now}}); err != nil {
		t.Fatalf("UpsertMappingCapabilities: %v", err)
	}
	if err := routeStore.UpsertRuntimeSpec(ctx, routing.RuntimeSpec{ID: "spec-agent-audio", MappingID: "route-agent-audio", APIFlavors: specFlavors, ResponsesMode: routing.EndpointModeDisabled, MessagesMode: routing.EndpointModeDisabled, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("UpsertRuntimeSpec: %v", err)
	}
	if err := routeStore.UpsertTelemetry(ctx, routing.ServerTelemetry{ServerID: "srv-agent-audio", ReportedAt: now, LatencyMS: 100, ProviderHealth: `{}`, Capabilities: `{}`, RawSummary: `{}`, UpdatedAt: now}); err != nil {
		t.Fatalf("UpsertTelemetry: %v", err)
	}
	return New(ServerDeps{
		Tokens:   tokens,
		Usage:    recorder,
		Provider: prov,
		Routes:   routeStore,
		Portal:   portal.NewService(portal.ServiceDeps{Users: directory, Tokens: directory, Usage: recorder, Routes: routeStore, Clock: func() time.Time { return now }, ModelLister: provider.NewMock()}),
	})
}

// An agent-managed child is held to its own runtime spec after the resolve:
// candidacy gates a server_agent application on its app-level flavors only, so a
// spec narrowed to text must not be sent a speech request. The refusal is
// candidacy's own no-route answer, nothing is relayed, and the row claims no
// upstream call.
func TestSpeechRefusesAServerAgentChildWhoseSpecExcludesSpeech(t *testing.T) {
	prov := &recordingProxyProvider{respBody: "RIFF"}
	srv := newServerAgentSpeechSpecTestServer(t, prov, []string{routing.APIFlavorOpenAI})

	rec := postSpeech(t, srv, `{"model":"agent-voice","input":"hi"}`)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body = %s", rec.Code, rec.Body.String())
	}
	requireErrorCode(t, rec.Body.String(), "routing.no_model_route")
	if prov.proxyCalls != 0 {
		t.Fatalf("ProxyNative calls = %d, want 0 -- a text-only spec must never be sent a speech request", prov.proxyCalls)
	}
	events := srv.Usage.All()
	if len(events) != 1 || events[0].ErrorCode != "routing.no_model_route" {
		t.Fatalf("usage events = %+v, want one routing.no_model_route error", events)
	}
	if got := events[0]; got.Host != "" || got.RouteID != "" || got.ProviderPath != "" || got.ReqPath != "/v1/audio/speech" || got.HTTPStatus != http.StatusNotFound {
		t.Fatalf("usage row = %+v, want an upstream-less 404 row on /v1/audio/speech", got)
	}
}

// The positive control for the test above: the same fixture with a spec that
// lists openai_audio is served, on the speech path. Without it the refusal above
// could come from anything in the fixture rather than from the flavor check.
func TestSpeechServesAServerAgentChildWhoseSpecIncludesSpeech(t *testing.T) {
	prov := &recordingProxyProvider{respBody: "RIFF"}
	srv := newServerAgentSpeechSpecTestServer(t, prov, []string{routing.APIFlavorOpenAIAudio})

	rec := postSpeech(t, srv, `{"model":"agent-voice","input":"hi"}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %q", rec.Code, rec.Body.Bytes())
	}
	if prov.proxyCalls != 1 || prov.gotPath != "/v1/audio/speech" {
		t.Fatalf("ProxyNative calls = %d on %q, want 1 on /v1/audio/speech", prov.proxyCalls, prov.gotPath)
	}
}

// postSpeechOverTheWire sends a speech request to a REAL listener fronting srv
// and returns the client-side response. A ResponseRecorder cannot tell a sized
// body from a chunked one -- it records the headers the handler set and none of
// the framing net/http picks on the wire -- so the Content-Length assertions
// below need a genuine round trip. resp.ContentLength is -1 and
// resp.TransferEncoding is ["chunked"] for a chunked body, and the declared
// length for a sized one.
func postSpeechOverTheWire(t *testing.T, srv *Server, body string) (*http.Response, error) {
	t.Helper()
	front := httptest.NewServer(srv)
	t.Cleanup(front.Close)
	req, err := http.NewRequest(http.MethodPost, front.URL+"/v1/audio/speech", strings.NewReader(body))
	if err != nil {
		return nil, err // returned, not t.Fatalf: the streaming test calls this from a goroutine
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer dev-secret")
	return http.DefaultClient.Do(req)
}

// A speech upstream that declares its length hands the client a SIZED audio/wav
// body: the relay forwards Content-Length, the body is the upstream's bytes
// verbatim, and the wire framing is Content-Length rather than chunked.
//
// The upstream writes the first half, flushes, and then BLOCKS until the client
// has read that half. That is the streaming assertion: a relay that buffered the
// whole body before answering would never send response headers while the
// upstream is parked (and the test would time out), whereas a relay that
// forwards the declared length but still streams delivers the first half while
// the upstream is parked.
func TestSpeechRelayForwardsTheUpstreamContentLengthAndStillStreams(t *testing.T) {
	wav := speechTestWAV()
	half := len(wav) / 2
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseUpstream := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseUpstream)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "audio/wav")
		w.Header().Set("Content-Length", strconv.Itoa(len(wav)))
		_, _ = w.Write(wav[:half])
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		_, _ = w.Write(wav[half:])
	}))
	t.Cleanup(up.Close)
	srv, _ := newSpeechCapableTestServer(t, up.URL)

	type result struct {
		resp *http.Response
		err  error
	}
	done := make(chan result, 1)
	go func() {
		resp, err := postSpeechOverTheWire(t, srv, `{"model":"tts-voice","input":"Hello there."}`)
		done <- result{resp, err}
	}()
	var res result
	select {
	case res = <-done:
	case <-time.After(10 * time.Second):
		releaseUpstream()
		t.Fatal("no response headers while the upstream was parked mid-body: the relay buffers the body instead of streaming it")
	}
	if res.err != nil {
		t.Fatalf("POST /v1/audio/speech: %v", res.err)
	}
	defer res.resp.Body.Close()

	if res.resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.resp.StatusCode)
	}
	if got := res.resp.Header.Get("Content-Type"); got != "audio/wav" {
		t.Fatalf("Content-Type = %q, want audio/wav", got)
	}
	if res.resp.ContentLength != int64(len(wav)) {
		t.Fatalf("ContentLength = %d, want %d: the upstream's declared length must reach the client", res.resp.ContentLength, len(wav))
	}
	if len(res.resp.TransferEncoding) != 0 {
		t.Fatalf("TransferEncoding = %v, want none -- a sized body is not chunked", res.resp.TransferEncoding)
	}

	first := make([]byte, half)
	if _, err := io.ReadFull(res.resp.Body, first); err != nil {
		t.Fatalf("reading the first half while the upstream is parked: %v", err)
	}
	if !bytes.Equal(first, wav[:half]) {
		t.Fatalf("first half = %x, want %x", first, wav[:half])
	}
	releaseUpstream()
	rest, err := io.ReadAll(res.resp.Body)
	if err != nil {
		t.Fatalf("reading the rest: %v", err)
	}
	if got := append(first, rest...); !bytes.Equal(got, wav) {
		t.Fatalf("body = %x, want the upstream WAV %x byte for byte", got, wav)
	}
}

// A genuinely streamed speech upstream -- no Content-Length, chunked on its own
// wire -- stays chunked toward the client. There is no length to forward, and
// inventing one would be wrong.
func TestSpeechRelayLeavesAnUnsizedUpstreamChunked(t *testing.T) {
	wav := speechTestWAV()
	half := len(wav) / 2
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "audio/wav")
		_, _ = w.Write(wav[:half])
		w.(http.Flusher).Flush() // flushing before the handler returns forces chunked framing upstream
		_, _ = w.Write(wav[half:])
	}))
	t.Cleanup(up.Close)
	srv, _ := newSpeechCapableTestServer(t, up.URL)

	resp, err := postSpeechOverTheWire(t, srv, `{"model":"tts-voice","input":"Hello there."}`)
	if err != nil {
		t.Fatalf("POST /v1/audio/speech: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if resp.ContentLength != -1 || resp.Header.Get("Content-Length") != "" {
		t.Fatalf("ContentLength = %d (header %q), want an unsized, chunked body", resp.ContentLength, resp.Header.Get("Content-Length"))
	}
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading the body: %v", err)
	}
	if !bytes.Equal(got, wav) {
		t.Fatalf("body = %x, want the upstream WAV %x byte for byte", got, wav)
	}
}

// The speech error relay is as narrow as the images one: only a speech request
// whose upstream answered a non-2xx status is diverted from the streaming
// copier. A 2xx audio/wav must keep streaming, and every other flavor's errors
// stay on the copier byte for byte.
func TestNeedsSpeechErrorRelay(t *testing.T) {
	cases := []struct {
		name   string
		flavor string
		status int
		want   bool
	}{
		{"speech 200", apiFlavorSpeech, 200, false},
		{"speech 204", apiFlavorSpeech, 204, false},
		{"speech 299", apiFlavorSpeech, 299, false},
		{"speech 199 is outside the 2xx window", apiFlavorSpeech, 199, true},
		{"speech 300 is outside the 2xx window", apiFlavorSpeech, 300, true},
		{"speech 400", apiFlavorSpeech, 400, true},
		{"speech 404", apiFlavorSpeech, 404, true},
		{"speech 500, audio.cpp's answer to a client error too", apiFlavorSpeech, 500, true},
		{"speech 502", apiFlavorSpeech, 502, true},
		{"images 500 has its own relay", apiFlavorImages, 500, false},
		{"openai chat 500", "openai", 500, false},
		{"openai_responses 500", "openai_responses", 500, false},
		{"anthropic_messages 500", "anthropic_messages", 500, false},
		{"empty flavor 500", "", 500, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := needsSpeechErrorRelay(tc.flavor, tc.status); got != tc.want {
				t.Fatalf("needsSpeechErrorRelay(%q, %d) = %v, want %v", tc.flavor, tc.status, got, tc.want)
			}
		})
	}
}

// The gateway's own classification of a normalised speech error is API surface
// (clients branch on the code), so it is pinned by value.
func TestSpeechUpstreamErrorClassificationIsStable(t *testing.T) {
	if speechUpstreamErrorType != "upstream_error" {
		t.Errorf("speechUpstreamErrorType = %q, want %q", speechUpstreamErrorType, "upstream_error")
	}
	if speechUpstreamErrorCode != "speech.upstream_error" {
		t.Errorf("speechUpstreamErrorCode = %q, want %q", speechUpstreamErrorCode, "speech.upstream_error")
	}
}

// audio.cpp already answers with an OpenAI-shaped object, so the normaliser has
// nothing to translate: the upstream's own bytes, its own type and no invented
// code reach the client. Re-normalising would restate a gateway-authored value
// as the upstream's.
func TestNormalizeSpeechUpstreamErrorPassesAnOpenAIObjectThrough(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"audio.cpp's own shape", `{"error":{"message":"unknown model id: x","type":"server_error"}}`},
		{"a full OpenAI object", `{"error":{"message":"bad request","type":"invalid_request_error","code":"bad_prompt","param":"input"}}`},
		{"an object with only a message", `{"error":{"message":"boom"}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := normalizeSpeechUpstreamError([]byte(tc.body), http.StatusInternalServerError)
			if !ok {
				t.Fatal("an OpenAI-shaped body must be accepted")
			}
			if string(got) != tc.body {
				t.Fatalf("body = %s, want the upstream's bytes untouched: %s", got, tc.body)
			}
		})
	}
}

// A plain-string body ({"error":"..."}) is not an OpenAI object, so it is
// wrapped: the upstream's string is the message verbatim, and type and code are
// the GATEWAY's, fixed regardless of the upstream status.
func TestNormalizeSpeechUpstreamErrorWrapsAPlainString(t *testing.T) {
	for _, status := range []int{400, 500, 502} {
		got, ok := normalizeSpeechUpstreamError([]byte(`{"error":"out of memory"}`), status)
		if !ok {
			t.Fatalf("status %d: a plain-string error body must normalise", status)
		}
		var body apierror.Body
		if err := json.Unmarshal(got, &body); err != nil {
			t.Fatalf("status %d: result is not an OpenAI error object: %v (%s)", status, err, got)
		}
		if body.Error.Message != "out of memory" {
			t.Errorf("status %d: message = %q, want the upstream string verbatim", status, body.Error.Message)
		}
		if body.Error.Type != speechUpstreamErrorType || body.Error.Code != speechUpstreamErrorCode {
			t.Errorf("status %d: type/code = %q/%q, want the gateway's %q/%q", status, body.Error.Type, body.Error.Code, speechUpstreamErrorType, speechUpstreamErrorCode)
		}
	}
}

// A body that is neither shape is not guessed at: ok is false and the caller
// relays the upstream bytes unchanged.
func TestNormalizeSpeechUpstreamErrorLeavesAnUnrecognisedBodyAlone(t *testing.T) {
	for _, body := range []string{
		``,
		`<html>502 Bad Gateway</html>`,
		`not json at all`,
		`{}`,
		`null`,
		`[]`,
		`{"error":null}`,
		`{"error":""}`,
		`{"error":{}}`,
		`{"error":{"message":""}}`,
		`{"error":{"type":"server_error"}}`,
		`{"error":42}`,
		`{"message":"no error key"}`,
		"RIFF\x2c\x00\x00\x00WAVE\xff\xfe",
	} {
		if got, ok := normalizeSpeechUpstreamError([]byte(body), 500); ok {
			t.Errorf("body %q: ok = true (result %s), want false so the upstream bytes are relayed unchanged", body, got)
		}
	}
}

// newSpeechErrorUpstream starts an audio.cpp stand-in that answers every request
// with the given status, content type and body.
func newSpeechErrorUpstream(t *testing.T, status int, contentType, body string) *httptest.Server {
	t.Helper()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if contentType != "" {
			w.Header().Set("Content-Type", contentType)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(up.Close)
	return up
}

// The reachability proof for the unit tests above: a REAL non-2xx response from
// an audio.cpp-shaped upstream goes through the actual mux and the client sees
// the OpenAI object -- audio.cpp's own, untouched (its type is not overwritten
// with ours) -- under the upstream's status. The row is an error row with the
// audio_second unit and no billed audio.
func TestSpeechRelayPassesAnAudioCppErrorThroughOverHTTP(t *testing.T) {
	const upstreamBody = `{"error":{"message":"unknown model id: x","type":"server_error"}}`
	up := newSpeechErrorUpstream(t, http.StatusInternalServerError, "application/json", upstreamBody)
	srv, _ := newSpeechCapableTestServer(t, up.URL)

	rec := postSpeech(t, srv, `{"model":"tts-voice","input":"Hello there."}`)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (the upstream's own status); body = %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", got)
	}
	var body apierror.Body
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("client body is not an OpenAI error object: %v (%s)", err, rec.Body.String())
	}
	if body.Error.Message != "unknown model id: x" || body.Error.Type != "server_error" || body.Error.Code != "" {
		t.Fatalf("error = %+v, want audio.cpp's own message and type with no gateway code added", body.Error)
	}

	got := lastUsageEvent(t, srv)
	if got.Status != "error" || got.HTTPStatus != http.StatusInternalServerError || got.ErrorCode != "upstream.500" {
		t.Fatalf("row = %q/%d/%q, want error/500/upstream.500", got.Status, got.HTTPStatus, got.ErrorCode)
	}
	if got.ContentType != "application/json" || got.APIFlavor != apiFlavorSpeech || got.ReqPath != "/v1/audio/speech" {
		t.Fatalf("row content type/flavor/path = %q/%q/%q, want application/json/%s//v1/audio/speech", got.ContentType, got.APIFlavor, got.ReqPath, apiFlavorSpeech)
	}
	if got.BillingQuantity != 0 {
		t.Errorf("BillingQuantity = %v, want 0 for a failed relay", got.BillingQuantity)
	}
	requireSpeechNonTokenRow(t, got)
}

// An upstream error that is NOT an OpenAI object but a plain string reaches the
// client wrapped, with the gateway's type and code, under the upstream's status.
func TestSpeechRelayWrapsAPlainStringErrorOverHTTP(t *testing.T) {
	up := newSpeechErrorUpstream(t, http.StatusBadRequest, "application/json", `{"error":"voice not found"}`)
	srv, _ := newSpeechCapableTestServer(t, up.URL)

	rec := postSpeech(t, srv, `{"model":"tts-voice","input":"Hello there.","voice":"nobody"}`)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (the upstream's own status); body = %s", rec.Code, rec.Body.String())
	}
	var body apierror.Body
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("client body is not an OpenAI error object: %v (%s)", err, rec.Body.String())
	}
	if body.Error.Message != "voice not found" || body.Error.Type != speechUpstreamErrorType || body.Error.Code != speechUpstreamErrorCode {
		t.Fatalf("error = %+v, want the upstream string with the gateway's type/code", body.Error)
	}
	if got := lastUsageEvent(t, srv); got.ErrorCode != "upstream.400" || got.HTTPStatus != http.StatusBadRequest {
		t.Fatalf("row = %q/%d, want upstream.400/400", got.ErrorCode, got.HTTPStatus)
	}
}

// A body the normaliser does not recognise is relayed exactly as the upstream
// sent it -- bytes, status and Content-Type -- never replaced by a guess.
func TestSpeechRelayRelaysAnUnrecognisedErrorBodyUnchanged(t *testing.T) {
	const upstreamBody = "<html>502 Bad Gateway</html>"
	up := newSpeechErrorUpstream(t, http.StatusBadGateway, "text/html; charset=utf-8", upstreamBody)
	srv, _ := newSpeechCapableTestServer(t, up.URL)

	rec := postSpeech(t, srv, `{"model":"tts-voice","input":"Hello there."}`)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "text/html; charset=utf-8" {
		t.Fatalf("Content-Type = %q, want the upstream's own", got)
	}
	if rec.Body.String() != upstreamBody {
		t.Fatalf("body = %q, want the upstream's bytes %q", rec.Body.String(), upstreamBody)
	}
	if got := lastUsageEvent(t, srv); got.ErrorCode != "upstream.502" || got.ContentType != "text/html; charset=utf-8" {
		t.Fatalf("row = %q/%q, want upstream.502 and the upstream's content type", got.ErrorCode, got.ContentType)
	}
}

// A 2xx audio/wav is not an error: it is relayed byte for byte (not through the
// error relay's bounded buffer) and recorded as a success.
func TestSpeechRelayDoesNotTreatASuccessfulWAVAsAnError(t *testing.T) {
	wav := speechTestWAV()
	up, _ := newSpeechUpstream(t, wav)
	srv, _ := newSpeechCapableTestServer(t, up.URL)

	rec := postSpeech(t, srv, `{"model":"tts-voice","input":"Hello there."}`)

	if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), wav) {
		t.Fatalf("status = %d, body = %x, want 200 and the WAV byte for byte", rec.Code, rec.Body.Bytes())
	}
	if got := lastUsageEvent(t, srv); got.Status != "success" || got.ErrorCode != "" {
		t.Fatalf("row = %q/%q, want success with no error code", got.Status, got.ErrorCode)
	}
}

// The metering headers a skipped speech body's capture keeps are the audio.cpp
// ones (the X-AudioCPP- family: duration, wall time, real-time factor), and only
// those: the upstream's other headers are not metering.
func TestSpeechMeteringHeaders(t *testing.T) {
	if !strings.HasPrefix(strings.ToLower(speechDurationHeader), speechMeteringHeaderPrefix) {
		t.Fatalf("the billing duration header %q is not in the metering family %q", speechDurationHeader, speechMeteringHeaderPrefix)
	}
	upstream := http.Header{}
	upstream.Set("Content-Type", "audio/wav")
	upstream.Set("Content-Length", "52")
	upstream.Set("X-AudioCPP-Audio-Duration-Ms", "1500.000000")
	upstream.Set("X-AudioCPP-Wall-Ms", "120.5")
	upstream.Set("x-audiocpp-rtf", "0.08")
	upstream.Set("X-Unrelated", "keep-out")

	got := speechMeteringHeaders(upstream)

	want := http.Header{}
	want.Set("X-AudioCPP-Audio-Duration-Ms", "1500.000000")
	want.Set("X-AudioCPP-Wall-Ms", "120.5")
	want.Set("X-AudioCPP-RTF", "0.08")
	if len(got) != len(want) {
		t.Fatalf("metering headers = %v, want exactly %v", got, want)
	}
	for name := range want {
		if got.Get(name) != want.Get(name) {
			t.Errorf("%s = %q, want %q", name, got.Get(name), want.Get(name))
		}
	}
	if speechMeteringHeaders(http.Header{"Content-Type": {"audio/wav"}}) != nil {
		t.Error("an upstream with no metering headers must yield nil, not an empty header")
	}
	if speechMeteringHeaders(nil) != nil {
		t.Error("a nil header must yield nil")
	}
}
