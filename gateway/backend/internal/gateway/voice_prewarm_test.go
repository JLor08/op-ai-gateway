// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
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
