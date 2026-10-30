// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/inference"
	"op-ai-gateway/internal/provider"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/usage"
	"strings"
	"sync"
	"testing"
	"time"
)

// Voice Chat's session-open TTS pre-warm (#342) fires one short synthesis for the session's voice
// so the first spoken reply is warm. The synthesis must NEVER be billed or stored, which is why it
// goes straight to the upstream through the provider's native relay (the speech benchmark's
// dispatch) and not through the gateway's own /v1/audio/speech, whose relay records a usage row.
// These tests pin that dispatch: what is sent, where, that nothing is recorded, and that a failure
// comes back to the caller (which logs it) instead of being swallowed.

const (
	primingGatewayModel  = "tts-gateway-name"
	primingUpstreamModel = "tts-upstream-name"
	primingVoice         = "alba"
)

// primingCall is one ProxyNative request the fake provider received.
type primingCall struct {
	target      routing.Target
	path        string
	body        []byte
	contentType string
	ctx         context.Context
}

// primingProvider is a provider.Client with the native relay: it records every ProxyNative call and
// answers through answer (a 200 audio/wav by default). Complete is a failure the tests never expect,
// because the priming path must not take the translated chat path.
type primingProvider struct {
	mu          sync.Mutex
	calls       []primingCall
	completes   int
	answer      func(ctx context.Context) (*provider.ProxyResponse, error)
	bodyDrained *bool
	bodyClosed  *bool
}

func (p *primingProvider) Complete(context.Context, routing.Target, inference.Request) (provider.Response, error) {
	p.mu.Lock()
	p.completes++
	p.mu.Unlock()
	return provider.Response{}, errors.New("priming must not take the chat path")
}

func (p *primingProvider) ProxyNative(ctx context.Context, target routing.Target, path string, body []byte, contentType string) (*provider.ProxyResponse, error) {
	p.mu.Lock()
	p.calls = append(p.calls, primingCall{target: target, path: path, body: append([]byte(nil), body...), contentType: contentType, ctx: ctx})
	p.mu.Unlock()
	if p.answer != nil {
		return p.answer(ctx)
	}
	return &provider.ProxyResponse{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"audio/wav"}},
		Body:       &trackedBody{r: strings.NewReader(strings.Repeat("RIFF", 1024)), drained: p.bodyDrained, closed: p.bodyClosed},
	}, nil
}

func (p *primingProvider) received() []primingCall {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]primingCall(nil), p.calls...)
}

// trackedBody reports whether its reader was read to the end and whether it was closed.
type trackedBody struct {
	r       io.Reader
	drained *bool
	closed  *bool
}

func (b *trackedBody) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	if errors.Is(err, io.EOF) && b.drained != nil {
		*b.drained = true
	}
	return n, err
}

func (b *trackedBody) Close() error {
	if b.closed != nil {
		*b.closed = true
	}
	return nil
}

// nativeOnlyProvider is a provider.Client WITHOUT the native relay.
type nativeOnlyProvider struct{}

func (nativeOnlyProvider) Complete(context.Context, routing.Target, inference.Request) (provider.Response, error) {
	return provider.Response{}, nil
}

func primingTarget() routing.Target {
	return routing.Target{
		RouteID:       "route-tts",
		ServerID:      "srv-audio",
		Provider:      routing.ProviderAudioCpp,
		Endpoint:      "http://audio.example.test:8200",
		Model:         primingGatewayModel,
		ProviderModel: primingUpstreamModel,
		APIFlavor:     routing.APIFlavorOpenAIAudio,
	}
}

// The priming synth goes straight to the speech target's upstream: one POST of /v1/audio/speech
// through the native relay, a plain non-stream JSON body naming the application's own model id, the
// fixed priming text and the one chosen voice -- and nothing else. It writes no usage row, and it
// reads the answer to its end and closes it.
func TestPrimeSpeechVoiceDispatchesDirectlyAndRecordsNothing(t *testing.T) {
	var drained, closed bool
	prov := &primingProvider{bodyDrained: &drained, bodyClosed: &closed}
	recorder := usage.NewRecorder()
	s := &Server{Provider: prov, Usage: recorder}
	tts := primingTarget()

	if err := s.primeSpeechVoice(context.Background(), tts, primingVoice); err != nil {
		t.Fatalf("primeSpeechVoice returned %v, want nil", err)
	}

	calls := prov.received()
	if len(calls) != 1 {
		t.Fatalf("the upstream saw %d requests, want exactly one", len(calls))
	}
	call := calls[0]
	if call.path != "/v1/audio/speech" {
		t.Errorf("upstream path = %q, want /v1/audio/speech", call.path)
	}
	if call.contentType != "application/json" {
		t.Errorf("content type = %q, want application/json", call.contentType)
	}
	if call.target.Endpoint != tts.Endpoint || call.target.RouteID != tts.RouteID {
		t.Errorf("dispatched to target %+v, want the resolved speech target %+v", call.target, tts)
	}
	var body map[string]any
	if err := json.Unmarshal(call.body, &body); err != nil {
		t.Fatalf("the priming body is not JSON: %v (%s)", err, call.body)
	}
	want := map[string]any{"model": primingUpstreamModel, "input": voicePrimingText, "voice": primingVoice}
	if len(body) != len(want) {
		t.Errorf("priming body = %v, want exactly %v (no stream, no format, no speed)", body, want)
	}
	for k, v := range want {
		if body[k] != v {
			t.Errorf("priming body[%q] = %v, want %v", k, body[k], v)
		}
	}
	if !drained {
		t.Error("the answer body was not read to its end")
	}
	if !closed {
		t.Error("the answer body was not closed")
	}
	if prov.completes != 0 {
		t.Errorf("the translated chat path was taken %d time(s), want none", prov.completes)
	}
	if rows := recorder.All(); len(rows) != 0 {
		t.Errorf("priming recorded %d usage row(s), want none: %+v", len(rows), rows)
	}
}

// A target without a per-model override (ProviderModel empty) is sent under the gateway's name for
// the model, exactly as the speech relay does (rewriteModelField leaves the client's name when
// there is no override).
func TestPrimeSpeechVoiceModelFallsBackToTheGatewayName(t *testing.T) {
	prov := &primingProvider{}
	s := &Server{Provider: prov}
	tts := primingTarget()
	tts.ProviderModel = ""

	if err := s.primeSpeechVoice(context.Background(), tts, primingVoice); err != nil {
		t.Fatalf("primeSpeechVoice returned %v, want nil", err)
	}
	calls := prov.received()
	if len(calls) != 1 {
		t.Fatalf("the upstream saw %d requests, want one", len(calls))
	}
	var body struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(calls[0].body, &body)
	if body.Model != primingGatewayModel {
		t.Errorf("model = %q, want the gateway name %q", body.Model, primingGatewayModel)
	}
}

// The upstream credential reaches the call: an authenticated application would answer the priming
// synth 401 otherwise, and the pre-warm would silently never warm anything.
func TestPrimeSpeechVoiceAttachesTheUpstreamCredential(t *testing.T) {
	prov := &primingProvider{}
	s := &Server{Provider: prov}
	tts := primingTarget()
	tts.APIToken = "plain:upstream-secret"

	if err := s.primeSpeechVoice(context.Background(), tts, primingVoice); err != nil {
		t.Fatalf("primeSpeechVoice returned %v, want nil", err)
	}
	calls := prov.received()
	if len(calls) != 1 {
		t.Fatalf("the upstream saw %d requests, want one", len(calls))
	}
	auth, ok := provider.UpstreamAuthFrom(calls[0].ctx)
	if !ok || auth.Token != "upstream-secret" {
		t.Errorf("the call's context carries credential %+v (ok=%v), want the application's token", auth, ok)
	}
}

// A failed dispatch is the caller's to see (it logs it): the transport error comes back wrapped, and
// a non-2xx answer comes back as an error that names the status and the upstream's own message.
func TestPrimeSpeechVoiceReturnsTheUpstreamFailure(t *testing.T) {
	t.Run("transport error", func(t *testing.T) {
		boom := errors.New("dial tcp: connection refused")
		prov := &primingProvider{answer: func(context.Context) (*provider.ProxyResponse, error) { return nil, boom }}
		s := &Server{Provider: prov, Usage: usage.NewRecorder()}

		err := s.primeSpeechVoice(context.Background(), primingTarget(), primingVoice)
		if !errors.Is(err, boom) {
			t.Fatalf("primeSpeechVoice = %v, want it to wrap the transport error", err)
		}
	})
	t.Run("non-2xx answer", func(t *testing.T) {
		var closed bool
		prov := &primingProvider{answer: func(context.Context) (*provider.ProxyResponse, error) {
			return &provider.ProxyResponse{
				StatusCode: http.StatusInternalServerError,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       &trackedBody{r: strings.NewReader(`{"error":{"message":"voice not found"}}`), closed: &closed},
			}, nil
		}}
		recorder := usage.NewRecorder()
		s := &Server{Provider: prov, Usage: recorder}

		err := s.primeSpeechVoice(context.Background(), primingTarget(), primingVoice)
		if err == nil {
			t.Fatal("primeSpeechVoice = nil, want the upstream's HTTP 500 as an error")
		}
		for _, want := range []string{"500", "voice not found"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not mention %q", err, want)
			}
		}
		if !closed {
			t.Error("the error answer's body was not closed")
		}
		if rows := recorder.All(); len(rows) != 0 {
			t.Errorf("a failed priming recorded %d usage row(s), want none", len(rows))
		}
	})
}

// A provider without the native relay cannot be primed directly; that is an error, never a fallback
// to the billing loopback and never a panic.
func TestPrimeSpeechVoiceNeedsTheNativeRelay(t *testing.T) {
	s := &Server{Provider: nativeOnlyProvider{}}
	if err := s.primeSpeechVoice(context.Background(), primingTarget(), primingVoice); err == nil {
		t.Fatal("primeSpeechVoice = nil for a provider without ProxyNative, want an error")
	}
}

// The helper runs on the caller's context (the caller bounds it with voicePrimingTimeout, and it
// must never outlive that bound): cancelling it ends a priming synth the upstream is still holding.
func TestPrimeSpeechVoiceHonorsTheCallersContext(t *testing.T) {
	prov := &primingProvider{answer: func(ctx context.Context) (*provider.ProxyResponse, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	s := &Server{Provider: prov}
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() { done <- s.primeSpeechVoice(ctx, primingTarget(), primingVoice) }()
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("primeSpeechVoice = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("primeSpeechVoice did not return after its context was cancelled")
	}
}

// The priming text is a few words (a short clip is all the warm-up needs) and the bound is the
// documented 30 s.
func TestVoicePrimingConstants(t *testing.T) {
	if strings.TrimSpace(voicePrimingText) == "" || len(strings.Fields(voicePrimingText)) > 8 {
		t.Errorf("voicePrimingText = %q, want a short non-empty phrase", voicePrimingText)
	}
	if voicePrimingTimeout != 30*time.Second {
		t.Errorf("voicePrimingTimeout = %v, want 30s", voicePrimingTimeout)
	}
}

// The knob reaches the Server through ServerDeps, and a bare Server leaves the pre-warm OFF (the
// config default is on; cmd/gateway carries it explicitly, and a test that does not ask for the
// pre-warm never gets a stray synthesis).
func TestVoicePrewarmEnabledAccessor(t *testing.T) {
	if (&Server{}).voicePrewarmEnabled() {
		t.Error("a bare Server reports the pre-warm enabled, want off")
	}
	if !(&Server{voicePrewarm: true}).voicePrewarmEnabled() {
		t.Error("Server{voicePrewarm: true} reports the pre-warm disabled")
	}
	var nilServer *Server
	if nilServer.voicePrewarmEnabled() {
		t.Error("a nil Server reports the pre-warm enabled")
	}
	if !New(ServerDeps{VoicePrewarmEnabled: true}).voicePrewarmEnabled() {
		t.Error("ServerDeps{VoicePrewarmEnabled: true} did not reach the Server")
	}
	if New(ServerDeps{}).voicePrewarmEnabled() {
		t.Error("a bare ServerDeps enables the pre-warm, want off")
	}
}

// The ticket endpoint fires the priming (#342). The synthesis is a detached goroutine, so these
// tests are built around a spy that makes it deterministic without a sleep: prewarmSpy's entered
// and finished channels say "the priming reached the upstream" and "it came back", and a held
// synthesis stays open until the test lets it go. A test that expects NO priming has nothing to
// wait for, so it watches a short quiet window instead: a priming that wrongly fires starts its
// goroutine before the handler even returns, so the window is only ever a false pass for a
// broken endpoint, never a false failure for a correct one.

// prewarmQuietWindow is how long a test that expects no priming watches for one.
const prewarmQuietWindow = 300 * time.Millisecond

// prewarmAwait bounds every wait for something that must happen.
const prewarmAwait = 5 * time.Second

// prewarmBehavior is what the spy's upstream does with the priming request.
type prewarmBehavior struct {
	hold   bool  // keep the synthesis open until the test opens the spy
	err    error // answer the dispatch with this transport error
	status int   // answer with this HTTP status (0: 200 with a clip)
}

// prewarmSpy is the native relay behind the ticket endpoint's priming: primingProvider records the
// dispatch (target, path, body, the context it ran on), and the channels give the test its
// synchronization points.
type prewarmSpy struct {
	native   *primingProvider
	entered  chan struct{} // one token per ProxyNative call that has started
	finished chan struct{} // one token per ProxyNative call that has returned
	release  chan struct{} // closed by open(): lets held calls answer
	once     sync.Once
}

func newPrewarmSpy(t *testing.T, b prewarmBehavior) *prewarmSpy {
	t.Helper()
	spy := &prewarmSpy{entered: make(chan struct{}, 8), finished: make(chan struct{}, 8), release: make(chan struct{})}
	// A test that fails while a synthesis is held must not leave its goroutine blocked.
	t.Cleanup(spy.open)
	spy.native = &primingProvider{answer: func(ctx context.Context) (*provider.ProxyResponse, error) {
		spy.entered <- struct{}{}
		defer func() { spy.finished <- struct{}{} }()
		if b.hold {
			select {
			case <-spy.release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		if b.err != nil {
			return nil, b.err
		}
		if b.status != 0 && b.status != http.StatusOK {
			return &provider.ProxyResponse{
				StatusCode: b.status,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"voice not found"}}`)),
			}, nil
		}
		return &provider.ProxyResponse{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"audio/wav"}},
			Body:       io.NopCloser(strings.NewReader("RIFF")),
		}, nil
	}}
	return spy
}

// open lets every held synthesis answer. It is safe to call more than once.
func (p *prewarmSpy) open() { p.once.Do(func() { close(p.release) }) }

func (p *prewarmSpy) awaitEntered(t *testing.T) {
	t.Helper()
	select {
	case <-p.entered:
	case <-time.After(prewarmAwait):
		t.Fatal("the priming synthesis never reached the upstream")
	}
}

func (p *prewarmSpy) awaitFinished(t *testing.T) {
	t.Helper()
	select {
	case <-p.finished:
	case <-time.After(prewarmAwait):
		t.Fatal("the priming synthesis never finished")
	}
}

// requireNoPriming fails when a priming synthesis reaches the upstream within the quiet window.
func (p *prewarmSpy) requireNoPriming(t *testing.T) {
	t.Helper()
	select {
	case <-p.entered:
		t.Fatal("a priming synthesis was fired, want none")
	case <-time.After(prewarmQuietWindow):
	}
	if calls := p.native.received(); len(calls) != 0 {
		t.Fatalf("the upstream saw %d priming request(s), want none", len(calls))
	}
}

// prewarmGateProvider is the ticket world's agent fake (the gate's ensure and loaded-set reads)
// with the spy's native relay on top, so one provider answers the gate and the priming.
type prewarmGateProvider struct {
	*voiceGateFake
	spy *prewarmSpy
}

func (p prewarmGateProvider) ProxyNative(ctx context.Context, target routing.Target, path string, body []byte, contentType string) (*provider.ProxyResponse, error) {
	return p.spy.native.ProxyNative(ctx, target, path, body, contentType)
}

// newPrewarmTicketWorld is the ticket endpoint's world over prewarmGateProvider, with the knob set
// as asked (a bare Server leaves it off) and a usage recorder the priming must never write to.
func newPrewarmTicketWorld(t *testing.T, spy *prewarmSpy, enabled bool) (*voiceTicketWorld, *usage.Recorder) {
	t.Helper()
	w := newVoiceTicketWorld(t, prewarmGateProvider{voiceGateFake: &voiceGateFake{}, spy: spy})
	recorder := usage.NewRecorder()
	w.srv.Usage = recorder
	w.srv.voicePrewarm = enabled
	return w, recorder
}

func requireNothingRecorded(t *testing.T, recorder *usage.Recorder) {
	t.Helper()
	if rows := recorder.All(); len(rows) != 0 {
		t.Fatalf("the priming recorded %d usage row(s), want none: %+v", len(rows), rows)
	}
}

// After a passing gate the endpoint primes the voice the ticket carries, on the TTS target the gate
// resolved: one direct synthesis, the application's own model id, the ticket's voice -- and the
// ticket is the ordinary 200. The requested voice wins over a stored one, as it does in the ticket.
func TestVoiceTicketEndpointPrimesTheChosenVoice(t *testing.T) {
	spy := newPrewarmSpy(t, prewarmBehavior{})
	w, recorder := newPrewarmTicketWorld(t, spy, true)
	chatID := w.newChat(t, "usr_voice", `{"audio":{"voice":"stored"}}`)

	claims := w.redeem(t, w.postBearer(voiceTicketBodyVoice(chatID, "voice-asr", "voice-llm", "voice-tts", "marius")))
	spy.awaitFinished(t)

	calls := spy.native.received()
	if len(calls) != 1 {
		t.Fatalf("the upstream saw %d priming request(s), want exactly one", len(calls))
	}
	call := calls[0]
	if call.path != "/v1/audio/speech" {
		t.Errorf("priming path = %q, want /v1/audio/speech", call.path)
	}
	if call.target.RouteID != claims.TTS.RouteID || call.target.ServerID != claims.TTS.ServerID || call.target.Endpoint != claims.TTS.Endpoint ||
		call.target.Model != "voice-tts" || call.target.ProviderModel != "tts_voice" {
		t.Errorf("priming went to target %+v, want the gate's resolved speech target %+v", call.target, claims.TTS)
	}
	var body map[string]any
	if err := json.Unmarshal(call.body, &body); err != nil {
		t.Fatalf("the priming body is not JSON: %v (%s)", err, call.body)
	}
	if body["model"] != "tts_voice" || body["input"] != voicePrimingText || body["voice"] != "marius" || body["voice"] != claims.Audio.Voice {
		t.Errorf("priming body = %v, want the upstream model, the priming text and the ticket's voice %q", body, claims.Audio.Voice)
	}
	if got := spy.native.completes; got != 0 {
		t.Errorf("the translated chat path was taken %d time(s), want none", got)
	}
	requireNothingRecorded(t, recorder)
}

// The priming speaks the voice the TICKET carries, not the one the request named: a preset-clone
// model with no voice named is minted on its first preset (withCloneDefaultVoice), and that preset
// is what gets warmed -- the first spoken reply would otherwise still pay for the encoding.
func TestVoiceTicketEndpointPrimesTheClonesDefaultPreset(t *testing.T) {
	spy := newPrewarmSpy(t, prewarmBehavior{})
	w, _ := newPrewarmTicketWorld(t, spy, true)
	row := routing.ModelMappingCacheRow{MappingID: "route-voice-tts-clone", CacheType: routing.ModelMappingCacheVoices, Payload: `["zeta","alba"]`, UpdatedAt: time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)}
	if err := w.routes.UpsertModelMappingCache(context.Background(), row); err != nil {
		t.Fatalf("UpsertModelMappingCache: %v", err)
	}
	chatID := w.newChat(t, "usr_voice", `{}`)

	claims := w.redeem(t, w.postBearer(voiceTicketBody(chatID, "voice-asr", "voice-llm", "voice-tts-clone")))
	spy.awaitFinished(t)

	if claims.Audio.Voice != "alba" {
		t.Fatalf("ticket voice = %q, want the clone's first preset alba", claims.Audio.Voice)
	}
	calls := spy.native.received()
	if len(calls) != 1 {
		t.Fatalf("the upstream saw %d priming request(s), want one", len(calls))
	}
	var body struct {
		Model string `json:"model"`
		Voice string `json:"voice"`
	}
	if err := json.Unmarshal(calls[0].body, &body); err != nil {
		t.Fatalf("the priming body is not JSON: %v", err)
	}
	if body.Voice != "alba" || body.Model != "tts_clone" {
		t.Errorf("priming body = %+v, want the clone's first preset on the clone's model", body)
	}
}

// The ticket never waits for the priming: a synthesis the upstream holds open leaves the ticket's
// latency and success untouched, and the priming still runs on, to completion, afterwards.
func TestVoiceTicketEndpointDoesNotWaitForThePriming(t *testing.T) {
	spy := newPrewarmSpy(t, prewarmBehavior{hold: true})
	w, _ := newPrewarmTicketWorld(t, spy, true)
	chatID := w.newChat(t, "usr_voice", `{}`)
	body := voiceTicketBodyVoice(chatID, "voice-asr", "voice-llm", "voice-tts", "marius")

	answered := make(chan *httptest.ResponseRecorder, 1)
	go func() { answered <- w.postBearer(body) }()
	var rec *httptest.ResponseRecorder
	select {
	case rec = <-answered:
	case <-time.After(prewarmAwait):
		t.Fatal("the ticket response waited on the priming synthesis")
	}

	w.redeem(t, rec)
	spy.awaitEntered(t)
	select {
	case <-spy.finished:
		t.Fatal("the priming finished while the upstream still held it")
	default:
	}
	spy.open()
	spy.awaitFinished(t)
}

// A priming that fails -- the upstream's transport breaks, it answers a non-2xx, the provider has
// no native relay at all -- is logged and nothing else: the ticket is the same 200 with a ticket
// that redeems, and the failure is never an error row.
func TestVoiceTicketEndpointPrimingFailureNeverFailsTheTicket(t *testing.T) {
	for name, tc := range map[string]struct {
		behavior prewarmBehavior
		noRelay  bool
	}{
		"transport error": {behavior: prewarmBehavior{err: errors.New("dial tcp: connection refused")}},
		"upstream 500":    {behavior: prewarmBehavior{status: http.StatusInternalServerError}},
		"no native relay": {noRelay: true},
	} {
		t.Run(name, func(t *testing.T) {
			logs, restore := withCapturedSlog(t)
			t.Cleanup(restore)
			spy := newPrewarmSpy(t, tc.behavior)
			w, recorder := newPrewarmTicketWorld(t, spy, true)
			if tc.noRelay {
				w.srv.Provider = &voiceGateFake{}
			}
			chatID := w.newChat(t, "usr_voice", `{}`)

			rec := w.postBearer(voiceTicketBodyVoice(chatID, "voice-asr", "voice-llm", "voice-tts", "marius"))

			claims := w.redeem(t, rec)
			if claims.Audio.Voice != "marius" {
				t.Fatalf("ticket voice = %q, want marius: a failed priming must not touch the ticket", claims.Audio.Voice)
			}
			var parsed map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &parsed); err != nil || len(parsed) != 1 {
				t.Fatalf("response = %s (%v), want the ordinary single-key ticket body", rec.Body.String(), err)
			}
			if !tc.noRelay {
				spy.awaitFinished(t)
			}
			waitFor(t, func() bool { return findLogRecord(logs.Snapshot(), "WARN", "voice prewarm") })
			requireNothingRecorded(t, recorder)
		})
	}
}

// The knob decides: with the pre-warm off (a bare Server, or OP_AI_GATEWAY_VOICE_PREWARM=false) the
// ticket is minted exactly as before and no synthesis is fired.
func TestVoiceTicketEndpointDoesNotPrimeWithTheKnobOff(t *testing.T) {
	spy := newPrewarmSpy(t, prewarmBehavior{})
	w, _ := newPrewarmTicketWorld(t, spy, false)
	chatID := w.newChat(t, "usr_voice", `{}`)

	w.redeem(t, w.postBearer(voiceTicketBodyVoice(chatID, "voice-asr", "voice-llm", "voice-tts", "marius")))

	spy.requireNoPriming(t)
}

// A ticket with no voice to warm (a plain model left on its own default, nothing stored) primes
// nothing: the priming names the one chosen voice, and there is none.
func TestVoiceTicketEndpointDoesNotPrimeWithoutAVoice(t *testing.T) {
	spy := newPrewarmSpy(t, prewarmBehavior{})
	w, _ := newPrewarmTicketWorld(t, spy, true)
	chatID := w.newChat(t, "usr_voice", `{}`)

	claims := w.redeem(t, w.postBearer(voiceTicketBody(chatID, "voice-asr", "voice-llm", "voice-tts")))

	if claims.Audio.Voice != "" {
		t.Fatalf("ticket voice = %q, want none for this fixture", claims.Audio.Voice)
	}
	spy.requireNoPriming(t)
}

// Only a ticket that is actually handed out primes anything: a request the gate refuses (the speech
// model cannot stream), and a mint that fails (a ticket too large for a proxy's header line), warm
// nothing -- there is no session to open for them.
func TestVoiceTicketEndpointPrimesOnlyAnIssuedTicket(t *testing.T) {
	t.Run("a refused gate", func(t *testing.T) {
		spy := newPrewarmSpy(t, prewarmBehavior{})
		w, _ := newPrewarmTicketWorld(t, spy, true)
		chatID := w.newChat(t, "usr_voice", `{}`)

		rec := w.postBearer(voiceTicketBodyVoice(chatID, "voice-asr", "voice-llm", "voice-tts-buffered", "marius"))

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
		}
		requireNoTicket(t, rec)
		spy.requireNoPriming(t)
	})
	t.Run("a refused request", func(t *testing.T) {
		spy := newPrewarmSpy(t, prewarmBehavior{})
		w, _ := newPrewarmTicketWorld(t, spy, true)

		rec := w.postBearer(voiceTicketBodyVoice("no-such-chat", "voice-asr", "voice-llm", "voice-tts", "marius"))

		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404; body = %s", rec.Code, rec.Body.String())
		}
		spy.requireNoPriming(t)
	})
	t.Run("a ticket too large to hand out", func(t *testing.T) {
		spy := newPrewarmSpy(t, prewarmBehavior{})
		w, _ := newPrewarmTicketWorld(t, spy, true)
		now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
		if err := w.routes.UpdateMapping(context.Background(), routing.ModelMapping{ID: "route-voice-asr", ApplicationID: "app-voice-agent", GatewayModelName: "voice-asr", AppModelName: strings.Repeat("m", 3*voiceTicketMaxBytes), Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("UpdateMapping: %v", err)
		}
		chatID := w.newChat(t, "usr_voice", `{}`)

		rec := w.postBearer(voiceTicketBodyVoice(chatID, "voice-asr", "voice-llm", "voice-tts", "marius"))

		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500; body = %s", rec.Code, rec.Body.String())
		}
		requireNoTicket(t, rec)
		spy.requireNoPriming(t)
	})
}

// prewarmCtxKey marks a value on the request's context, to prove none of it reaches the priming.
type prewarmCtxKey struct{}

// The priming runs on its own clean, bounded context, never the request's. The request's ends the
// moment the ticket is answered, which would cut the synthesis short; and it carries the
// request-scoped state of the call (here a payload-capture sink and a value), none of which may
// follow the priming into the upstream -- a captured priming would be a stored one.
func TestVoiceTicketEndpointPrimesOnACleanBackgroundContext(t *testing.T) {
	spy := newPrewarmSpy(t, prewarmBehavior{hold: true})
	w, recorder := newPrewarmTicketWorld(t, spy, true)
	chatID := w.newChat(t, "usr_voice", `{}`)

	sink := provider.NewCaptureSink(1 << 10)
	reqCtx, cancelRequest := context.WithCancel(context.WithValue(context.Background(), prewarmCtxKey{}, "request-scoped"))
	defer cancelRequest()
	reqCtx = provider.WithCaptureSink(reqCtx, sink)
	req := httptest.NewRequest(http.MethodPost, voiceTicketPath, strings.NewReader(voiceTicketBodyVoice(chatID, "voice-asr", "voice-llm", "voice-tts", "marius"))).WithContext(reqCtx)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+voiceTicketBearer)
	rec := httptest.NewRecorder()

	w.srv.ServeHTTP(rec, req)
	w.redeem(t, rec)
	spy.awaitEntered(t)
	// What the server does once the response is out: the request's context ends.
	cancelRequest()

	calls := spy.native.received()
	if len(calls) != 1 {
		t.Fatalf("the upstream saw %d priming request(s), want one", len(calls))
	}
	ctx := calls[0].ctx
	if err := ctx.Err(); err != nil {
		t.Errorf("the priming's context ended with the request (%v), want it independent of it", err)
	}
	if ctx.Value(prewarmCtxKey{}) != nil {
		t.Error("a value of the request's context reached the priming, want a clean context")
	}
	if provider.CaptureSinkFrom(ctx) != nil {
		t.Error("the request's capture sink reached the priming, want none: a captured priming would be stored")
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatal("the priming's context has no deadline, want it bounded by voicePrimingTimeout")
	}
	if left := time.Until(deadline); left <= 0 || left > voicePrimingTimeout {
		t.Errorf("the priming's deadline is %v away, want within (0, %v]", left, voicePrimingTimeout)
	}

	spy.open()
	spy.awaitFinished(t)
	if len(sink.RequestBody()) != 0 || len(sink.ResponseBody()) != 0 {
		t.Error("the request's capture sink recorded the priming exchange")
	}
	requireNothingRecorded(t, recorder)
}
