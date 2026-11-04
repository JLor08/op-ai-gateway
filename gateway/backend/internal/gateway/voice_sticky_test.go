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
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/routing"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// A voice chat keeps its turns on the instances it already uses: each leg's loopback
// request names the instance that leg ran on last, starting at the ticket gate's
// target (for the TTS the one the pre-warm primed), and /v1 routes it there while it
// is free -- a preference honoured only on the internal loopback pair. These tests
// drive whole voice sessions through the real Server, resolver and provider client
// against the fake agent router that serves every instance (live_instances_test.go).

// preferenceRecorder is a loopback stand-in in front of the gateway: it keeps the path
// and the preferred-instance header of every request, then hands it on.
type preferenceRecorder struct {
	next http.Handler
	mu   sync.Mutex
	seen map[string][]string // path -> X-OP-Preferred-Instance of each request, in order
}

func (p *preferenceRecorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/v1/") {
		p.mu.Lock()
		p.seen[r.URL.Path] = append(p.seen[r.URL.Path], r.Header.Get(preferredInstanceHeaderName))
		p.mu.Unlock()
	}
	p.next.ServeHTTP(w, r)
}

func (p *preferenceRecorder) preferred(path string) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.seen[path])
}

// voiceStickyWorld is the instance fixture wired for voice sessions: asr-inst and
// tts-inst run instances 0 and 1, chat-inst its instance 0, tts-inst streams its
// speech, and the gateway calls itself through a preferenceRecorder.
type voiceStickyWorld struct {
	srv    *Server
	agent  *instanceAgentRouter
	rec    *preferenceRecorder
	url    string
	chatID string
	token  auth.Token
}

func newVoiceStickyWorld(t *testing.T) *voiceStickyWorld {
	t.Helper()
	agent := newInstanceAgentRouter(t)
	srv := newInstanceRouterServerWith(t, agent, instanceRouterConfig{replicable: true, voice: true})
	now := time.Now().UTC()
	routes, ok := srv.Routes.(*routing.MemoryStore)
	if !ok {
		t.Fatalf("routes are %T, want the memory store", srv.Routes)
	}
	if err := routes.UpsertMappingCapabilities(context.Background(), liveInstTTSMap, []routing.CapabilityRow{
		{Capability: routing.CapabilitySpeech, Verdict: routing.CapabilityYes, Source: routing.CapabilitySourceManual, CheckedAt: now},
		{Capability: routing.CapabilitySpeechStreaming, Verdict: routing.CapabilityYes, Source: routing.CapabilitySourceManual, CheckedAt: now},
	}); err != nil {
		t.Fatalf("UpsertMappingCapabilities tts: %v", err)
	}
	srv.RuntimeStatus.publish(liveInstServer, []RuntimeStatusDTO{
		liveInstanceRow(liveInstASRSpec, liveInstASRUp, 0, 0),
		liveInstanceRow(liveInstASRSpec, liveInstASRUp, 1, 0),
		liveInstanceRow(liveInstTTSSpec, liveInstTTSUp, 0, 0),
		liveInstanceRow(liveInstTTSSpec, liveInstTTSUp, 1, 0),
	})
	srv.SetBaseContext(context.Background())
	srv.voiceTiming = turnGraceTiming()
	rec := &preferenceRecorder{next: srv, seen: map[string][]string{}}
	ts := httptest.NewServer(rec)
	t.Cleanup(ts.Close)
	srv.selfBaseURL = ts.URL

	owner := auth.Token{UserID: "usr_dev", Scopes: []string{"gateway:use"}}
	created, err := srv.Portal.CreateChat(context.Background(), owner, portal.CreateChatRequest{Content: json.RawMessage(`{"settings":{},"messages":[]}`)})
	if err != nil {
		t.Fatalf("CreateChat: %v", err)
	}
	return &voiceStickyWorld{srv: srv, agent: agent, rec: rec, url: ts.URL, chatID: created.ID, token: auth.Token{ID: "tok_dev", UserID: "usr_dev", Active: true}}
}

// gate runs the ticket's voice-chat gate for the three models, as the ticket endpoint
// does, and returns its claims. The gate's ensures and residency read go to a fake
// agent; every request after it goes to the router again.
func (w *voiceStickyWorld) gate(t *testing.T) voiceTicketClaims {
	t.Helper()
	served := w.srv.Provider
	w.srv.Provider = &voiceGateFake{ensureErr: map[string]error{}}
	defer func() { w.srv.Provider = served }()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	asr, llm, tts, code, err := w.srv.voiceCoLoadGate(ctx, &w.token, "asr-inst", "chat-inst", "tts-inst")
	if err != nil || code != "" {
		t.Fatalf("gate = %q, %v; want it passed", code, err)
	}
	return voiceTicketClaims{
		ASR: asr, LLM: llm, TTS: tts,
		ChatID: w.chatID, UserID: "usr_dev", TokenID: "tok_dev",
		SampleRate: voiceSampleRate, Channels: voiceChannels, SampleFormat: voiceSampleFormat,
	}
}

// busy puts one request in flight on the gateway instance key until the returned
// function takes it off again.
func (w *voiceStickyWorld) busy(instanceKey string) (free func()) {
	id := "sticky-busy-" + instanceKey
	w.srv.Active.Add(ActiveRequest{ID: id, ServerID: liveInstServer, InstanceKey: instanceKey})
	return func() { w.srv.Active.Remove(id) }
}

func (w *voiceStickyWorld) dial(t *testing.T, ctx context.Context, claims voiceTicketClaims) *websocket.Conn {
	t.Helper()
	ticket := mustMintVoiceTicket(t, w.srv.voiceTickets, claims)
	conn, _, err := websocket.Dial(ctx, wsURL(w.url, "/api/portal/voice-chat"), &websocket.DialOptions{
		Subprotocols: []string{"op-voice-chat", ticket},
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.CloseNow() })
	readVoiceUntilState(t, ctx, conn, voiceStateListening)
	return conn
}

// turn speaks one turn and lets it finish: PCM, the transcript, the chunk end, the
// answer and its speech, the player drained, back to listening. An error frame fails
// the test at once.
func (w *voiceStickyWorld) turn(t *testing.T, ctx context.Context, conn *websocket.Conn) {
	t.Helper()
	until := func(typ string) {
		t.Helper()
		for {
			m, _ := readVoiceFrame(t, ctx, conn)
			if m.Type == voiceDownError {
				t.Fatalf("error frame: code %q message %q", m.Code, m.Message)
			}
			if m.Type == typ {
				return
			}
		}
	}
	sendVoicePCM(t, ctx, conn, 320)
	until(voiceDownASRPartial)
	sendVoiceControl(t, ctx, conn, "push_end")
	until(voiceDownAudioDone)
	sendVoiceControl(t, ctx, conn, "playback_done")
	readVoiceUntilState(t, ctx, conn, voiceStateListening)
}

// served is every address of upstream (its instance 0 and its instances "up#k") the
// router was asked for, in order.
func (w *voiceStickyWorld) served(upstream string) []string {
	var out []string
	for _, m := range w.agent.servedModels() {
		if m == upstream || strings.HasPrefix(m, upstream+"#") {
			out = append(out, m)
		}
	}
	return out
}

// speak sends one /v1/audio/speech request for tts-inst with the given headers and
// returns its response; the body is drained.
func (w *voiceStickyWorld) speak(t *testing.T, hdr map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, w.url+"/v1/audio/speech", bytes.NewBufferString(`{"model":"tts-inst","input":"hi"}`))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("speech request: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("speech request = %d, want 200", resp.StatusCode)
	}
	return resp
}

func repeatKey(s string, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = s
	}
	return out
}

// Three turns of one session stay on the ASR and TTS instances its first turn ran on,
// though the plain selection would serve instance 0 again from the second turn on: the
// first turn found instance 0 of both busy and moved to instance 1, and every later
// turn names instance 1 as its leg's preference and is served there.
func TestVoiceTurnsStickToInstance(t *testing.T) {
	w := newVoiceStickyWorld(t)
	claims := w.gate(t)
	if claims.ASR.InstanceKey != liveInstASRMap+"#0" || claims.TTS.InstanceKey != liveInstTTSMap+"#0" {
		t.Fatalf("gate targets = %q, %q; want instance 0 of both, the plain selection", claims.ASR.InstanceKey, claims.TTS.InstanceKey)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn := w.dial(t, ctx, claims)

	freeASR, freeTTS := w.busy(liveInstASRMap+"#0"), w.busy(liveInstTTSMap+"#0")
	w.turn(t, ctx, conn)
	freeASR()
	freeTTS()
	w.turn(t, ctx, conn)
	w.turn(t, ctx, conn)

	if got := w.served(liveInstASRUp); !slices.Equal(got, repeatKey(liveInstASRUp+"#1", 3)) {
		t.Fatalf("ASR addresses served = %v, want instance 1 for all three turns", got)
	}
	if got := w.served(liveInstTTSUp); !slices.Equal(got, repeatKey(liveInstTTSUp+"#1", 3)) {
		t.Fatalf("TTS addresses served = %v, want instance 1 for all three turns", got)
	}
	// What each leg asked for: the gate's instance first, then the one it was served on.
	for _, leg := range []struct{ path, mapping string }{
		{"/v1/audio/transcriptions/live", liveInstASRMap},
		{"/v1/audio/speech", liveInstTTSMap},
	} {
		want := []string{leg.mapping + "#0", leg.mapping + "#1", leg.mapping + "#1"}
		if got := w.rec.preferred(leg.path); !slices.Equal(got, want) {
			t.Fatalf("%s preferred = %v, want %v", leg.path, got, want)
		}
	}
	if got := w.rec.preferred("/v1/chat/completions"); !slices.Equal(got, repeatKey(liveInstChatMap+"#0", 3)) {
		t.Fatalf("LLM leg preferred = %v, want the gate's chat instance on every turn", got)
	}

	// The control: a request without a preference is served on instance 0 again.
	w.speak(t, map[string]string{"Authorization": "Bearer dev-secret"})
	if got := w.served(liveInstTTSUp); got[len(got)-1] != liveInstTTSUp {
		t.Fatalf("a plain speech request was served on %q, want instance 0", got[len(got)-1])
	}
}

// The first turn's speech goes to the instance the gate selected and the pre-warm
// primed -- instance 1 here, since instance 0 was busy at the gate -- although instance
// 0 is free again by then and the plain selection would take it.
func TestVoiceFirstTurnUsesPrewarmedTTS(t *testing.T) {
	w := newVoiceStickyWorld(t)
	freeTTS := w.busy(liveInstTTSMap + "#0")
	claims := w.gate(t)
	freeTTS()
	if claims.TTS.InstanceKey != liveInstTTSMap+"#1" {
		t.Fatalf("gate TTS target = %q, want %s#1, the free instance", claims.TTS.InstanceKey, liveInstTTSMap)
	}
	// The ticket endpoint primes the gate's TTS target (startVoicePrewarm runs this).
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := w.srv.primeSpeechVoice(ctx, claims.TTS, "alloy"); err != nil {
		t.Fatalf("primeSpeechVoice: %v", err)
	}

	conn := w.dial(t, ctx, claims)
	w.turn(t, ctx, conn)

	if got := w.served(liveInstTTSUp); !slices.Equal(got, []string{liveInstTTSUp + "#1", liveInstTTSUp + "#1"}) {
		t.Fatalf("TTS addresses served = %v, want the pre-warm and the first turn both on instance 1", got)
	}
	if got := w.rec.preferred("/v1/audio/speech"); !slices.Equal(got, []string{liveInstTTSMap + "#1"}) {
		t.Fatalf("first turn's speech preferred %v, want the pre-warmed %s#1", got, liveInstTTSMap)
	}
}

// The preferred-instance header is honoured -- and the served instance reported --
// only on the full internal loopback pair: not from a bearer client, and not beside
// the secret alone when its user does not resolve (that request is the bearer's).
func TestPreferredHeaderIgnoredOutsideLoopback(t *testing.T) {
	w := newVoiceStickyWorld(t)
	prefer := map[string]string{preferredInstanceHeaderName: liveInstTTSMap + "#1"}
	with := func(maps ...map[string]string) map[string]string {
		out := map[string]string{}
		for _, m := range maps {
			for k, v := range m {
				out[k] = v
			}
		}
		return out
	}
	loopback := map[string]string{internalAuthHeaderName: "test-internal-secret", internalUserHeaderName: "usr_dev", csrfHeaderName: "1"}
	bearer := map[string]string{"Authorization": "Bearer dev-secret"}
	secretOnly := map[string]string{internalAuthHeaderName: "test-internal-secret", internalUserHeaderName: "usr_nobody", "Authorization": "Bearer dev-secret"}

	for _, tc := range []struct {
		name       string
		hdr        map[string]string
		wantServed string // the router address
		wantReport string // X-OP-Served-Instance
	}{
		{"loopback pair with the header", with(loopback, prefer), liveInstTTSUp + "#1", liveInstTTSMap + "#1"},
		{"loopback pair without it", loopback, liveInstTTSUp, ""},
		{"bearer with the header", with(bearer, prefer), liveInstTTSUp, ""},
		{"secret without a resolvable user", with(secretOnly, prefer), liveInstTTSUp, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := w.speak(t, tc.hdr)
			got := w.served(liveInstTTSUp)
			if got[len(got)-1] != tc.wantServed {
				t.Fatalf("served on %q, want %q", got[len(got)-1], tc.wantServed)
			}
			if report := resp.Header.Get(servedInstanceHeaderName); report != tc.wantReport {
				t.Fatalf("%s = %q, want %q", servedInstanceHeaderName, report, tc.wantReport)
			}
		})
	}

	t.Run("a streamed chat completion reports its instance too", func(t *testing.T) {
		w.srv.RuntimeStatus.publish(liveInstServer, []RuntimeStatusDTO{liveInstanceRow("spec-chat-inst", liveInstChatUp, 1, 0)})
		req, err := http.NewRequest(http.MethodPost, w.url+"/v1/chat/completions", bytes.NewBufferString(`{"model":"chat-inst","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
		if err != nil {
			t.Fatalf("NewRequest: %v", err)
		}
		req.Header.Set("Content-Type", "application/json")
		for k, v := range with(loopback, map[string]string{preferredInstanceHeaderName: liveInstChatMap + "#1"}) {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("chat request: %v", err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK || resp.Header.Get(servedInstanceHeaderName) != liveInstChatMap+"#1" {
			t.Fatalf("chat = %d with %s %q, want 200 reporting %s#1", resp.StatusCode, servedInstanceHeaderName, resp.Header.Get(servedInstanceHeaderName), liveInstChatMap)
		}
		if got := w.served(liveInstChatUp); got[len(got)-1] != liveInstChatUp+"#1" {
			t.Fatalf("chat served on %q, want %s#1", got[len(got)-1], liveInstChatUp)
		}
	})
}
