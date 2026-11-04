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
	"op-ai-gateway/internal/apierror"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/routing"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// A voice chat's speech and transcription legs must keep up with real time: at the
// ticket mint the gate refuses a leg whose every candidate was measured too slow,
// naming the leg, and every turn's loopback request carries the marker that makes
// /v1 apply the same gate -- a marker honoured only on the internal loopback pair.

// setVoiceAudioRate stores rate as mapping id's measured audio rate, as a speed run would.
func setVoiceAudioRate(t *testing.T, srv *Server, id string, rate float64) {
	t.Helper()
	if err := srv.Routes.UpdateMappingAudioMetrics(context.Background(), id, routing.AudioMetrics{RateX: rate}, "audio_speed", time.Now().UTC()); err != nil {
		t.Fatalf("UpdateMappingAudioMetrics %s: %v", id, err)
	}
}

func TestVoiceGateNoRealtimeModel(t *testing.T) {
	for _, leg := range []struct {
		name    voiceLeg
		mapping string
	}{
		{voiceLegASR, liveInstASRMap},
		{voiceLegTTS, liveInstTTSMap},
	} {
		t.Run(string(leg.name), func(t *testing.T) {
			w := newVoiceInstanceWorld(t)
			w.publish([]int{0}, []int{0})
			// The server's own threshold (OP_AI_GATEWAY_AUDIO_REALTIME_MIN_FACTOR, 1.2 by
			// default) is what the gate applies: nothing in the test sets it.
			setVoiceAudioRate(t, w.srv, leg.mapping, 0.9)

			asr, llm, tts, code, err := w.gate(t)
			requireVoiceGateRefused(t, asr, llm, tts, code, err, voiceChatNoRealtimeModel)
			if !errors.Is(err, routing.ErrNoRealtimeCandidate) || !strings.Contains(err.Error(), "voice chat "+string(leg.name)+" leg:") {
				t.Fatalf("err = %v, want ErrNoRealtimeCandidate naming the %s leg", err, leg.name)
			}
			if got := w.ensured(); len(got) != 0 {
				t.Fatalf("ensured = %v, want nothing started for a gate refused at step 1", got)
			}

			// The answer is a 409 the portal reads the leg from.
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, voiceGatePath, http.NoBody)
			w.srv.writeVoiceCoLoadRefusal(context.Background(), rec, req, "chat-1", code, err)
			if rec.Code != http.StatusConflict {
				t.Fatalf("status = %d, want 409", rec.Code)
			}
			var body apierror.Body
			if jerr := json.Unmarshal(rec.Body.Bytes(), &body); jerr != nil {
				t.Fatalf("body: %v (%s)", jerr, rec.Body.String())
			}
			if body.Error.Code != voiceChatNoRealtimeModel || !strings.HasPrefix(body.Error.Message, "voice chat "+string(leg.name)+" leg: ") {
				t.Fatalf("refusal = %+v, want %s with a message naming the %s leg", body.Error, voiceChatNoRealtimeModel, leg.name)
			}
		})
	}
	t.Run("unmeasured and fast legs pass", func(t *testing.T) {
		w := newVoiceInstanceWorld(t)
		w.publish([]int{0}, []int{0})
		setVoiceAudioRate(t, w.srv, liveInstTTSMap, 2.5)
		if _, _, _, code, err := w.gate(t); err != nil || code != "" {
			t.Fatalf("gate = %q, %v; want it passed", code, err)
		}
	})
}

// headerRecorder is a loopback stand-in: it keeps the path and the real-time header
// of every request, then hands it to next (nil: answers 200 with nothing).
type headerRecorder struct {
	next http.Handler
	mu   sync.Mutex
	seen []string // "path realtime-header"
}

func (h *headerRecorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	h.seen = append(h.seen, r.URL.Path+" "+r.Header.Get(realtimeRequiredHeaderName))
	h.mu.Unlock()
	if h.next == nil {
		_, _ = io.Copy(io.Discard, r.Body)
		return
	}
	h.next.ServeHTTP(w, r)
}

func (h *headerRecorder) requests() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.seen...)
}

func TestVoicePerTurnLegCarriesRealtime(t *testing.T) {
	t.Run("asr leg of a voice turn", func(t *testing.T) {
		// The ticket is minted directly (no gate), then the transcription mapping is
		// re-measured too slow: the turn's own loopback must carry the marker, and
		// /v1/audio/transcriptions/live must refuse it under the real-time gate.
		child := newFakeVoiceASRChild(t)
		h := newVoiceTurnHarness(t, child, voiceTurnOpts{timing: turnGraceTiming()})
		rec := &headerRecorder{next: h.srv}
		loop := httptest.NewServer(rec)
		t.Cleanup(loop.Close)
		h.srv.selfBaseURL = loop.URL
		setVoiceAudioRate(t, h.srv, "route-asr", 0.5)
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()

		conn := h.dial(t, ctx)
		readVoiceUntilState(t, ctx, conn, voiceStateListening)
		sendVoicePCM(t, ctx, conn, 320)

		if !voicePoll(5*time.Second, func() bool { return len(rec.requests()) > 0 }) {
			t.Fatal("the turn dialled no loopback")
		}
		if got := rec.requests(); got[0] != "/v1/audio/transcriptions/live 1" {
			t.Fatalf("turn loopback = %q, want a marked /v1/audio/transcriptions/live", got)
		}
		var errFrame voiceDownMsg
		for errFrame.Type != voiceDownError {
			if errFrame, _ = readVoiceFrame(t, ctx, conn); errFrame.Type == voiceDownASRPartial {
				t.Fatal("the turn was transcribed: /v1 did not hold its ASR loopback to real time")
			}
		}
		if errFrame.Code != voiceChatNoRealtimeModel || errFrame.Message == "" {
			t.Fatalf("error frame = code %q message %q, want /v1's %s for a model measured too slow", errFrame.Code, errFrame.Message, voiceChatNoRealtimeModel)
		}
		readVoiceUntilState(t, ctx, conn, voiceStateIdle)
		if n, _ := child.snapshot(); n != 0 {
			t.Fatalf("ASR child connections = %d, want 0: a gate refusal never reaches the child", n)
		}
	})

	t.Run("asr leg of a live session", func(t *testing.T) {
		// A live transcription session is no voice chat: its loopback carries no marker,
		// so a mapping measured too slow still serves it.
		child := newMockLiveChild(t)
		srv, ts := newLiveLoopbackTestServer(t, child)
		rec := &headerRecorder{next: srv}
		loop := httptest.NewServer(rec)
		t.Cleanup(loop.Close)
		srv.selfBaseURL = loop.URL
		setVoiceAudioRate(t, srv, "route-asr", 0.5)
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()

		conn, _, err := dialLive(t, ctx, ts.URL, mintLiveModelTicket(t, srv, "asr-model", 16000, 1, "s16le", ""), nil)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer conn.CloseNow()
		if err := conn.Write(ctx, websocket.MessageBinary, make([]byte, 1600)); err != nil {
			t.Fatalf("send pcm: %v", err)
		}
		m, err := readLiveMsg(t, ctx, conn)
		if err != nil || m.Type != "delta" {
			t.Fatalf("first message = %+v, %v; want a delta: a live session is not held to real time", m, err)
		}
		if got := rec.requests(); len(got) != 1 || got[0] != "/v1/audio/transcriptions/live " {
			t.Fatalf("live session loopback = %q, want one unmarked /v1/audio/transcriptions/live", got)
		}
		_ = conn.Close(websocket.StatusNormalClosure, "")
	})

	t.Run("tts leg of a voice run", func(t *testing.T) {
		srv, owner, chatID, _, _ := newVoiceRunTestServer(t, 0)
		rec := &headerRecorder{next: srv}
		ts := httptest.NewServer(rec)
		t.Cleanup(ts.Close)
		srv.selfBaseURL = ts.URL

		_, snap := voiceRunOutcome(t, srv, owner, chatID, "hello there")
		if snap.Status != "completed" {
			t.Fatalf("voice run status = %q (%s), want completed", snap.Status, snap.Err)
		}
		var speech, chat int
		for _, r := range rec.requests() {
			switch r {
			case "/v1/audio/speech 1":
				speech++
			case "/v1/chat/completions ":
				chat++
			default:
				t.Fatalf("loopback request %q: the speech leg must carry the marker, the chat leg must not", r)
			}
		}
		if speech == 0 || chat == 0 {
			t.Fatalf("loopback requests = %q, want the chat leg and a speech leg", rec.requests())
		}

		// A standalone speech run is no voice chat: its loopback carries no marker.
		before := len(rec.requests())
		history, err := srv.Portal.VoiceRunHistory(context.Background(), owner, chatID, "say this")
		if err != nil {
			t.Fatalf("VoiceRunHistory: %v", err)
		}
		run, runCtx, err := srv.reserveRun(owner, chatID)
		if err != nil {
			t.Fatalf("reserveRun: %v", err)
		}
		_, status, msg, _ := srv.generateSpeechTurn(runCtx, owner, run, PrepareRunResult{History: history, Settings: portal.ChatRunSettings{Model: speechGatewayModel, Kind: chatRunKindSpeech}}, func() {}, true, nil)
		if status != "completed" {
			t.Fatalf("speech turn = %q (%s), want completed", status, msg)
		}
		if got := rec.requests()[before:]; len(got) != 1 || got[0] != "/v1/audio/speech " {
			t.Fatalf("standalone speech loopback = %q, want one unmarked /v1/audio/speech", got)
		}
	})

	t.Run("honoured only on the internal loopback pair", func(t *testing.T) {
		srv, _, _, _, _ := newVoiceRunTestServer(t, 0)
		setVoiceAudioRate(t, srv, "route-tts", 0.5)
		post := func(hdr map[string]string) *httptest.ResponseRecorder {
			req := httptest.NewRequest(http.MethodPost, "/v1/audio/speech", bytes.NewBufferString(`{"model":"`+speechGatewayModel+`","input":"hi"}`))
			req.Header.Set("Content-Type", "application/json")
			for k, v := range hdr {
				req.Header.Set(k, v)
			}
			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, req)
			return rec
		}
		loopback := map[string]string{internalAuthHeaderName: "test-internal-secret", internalUserHeaderName: "usr_dev", csrfHeaderName: "1"}

		marked := map[string]string{realtimeRequiredHeaderName: "1"}
		for k, v := range loopback {
			marked[k] = v
		}
		if rec := post(marked); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), voiceChatNoRealtimeModel) {
			t.Fatalf("marked loopback = %d %s, want 409 %s for a model measured too slow", rec.Code, rec.Body.String(), voiceChatNoRealtimeModel)
		}
		if rec := post(loopback); rec.Code != http.StatusOK {
			t.Fatalf("unmarked loopback = %d %s, want 200: only a voice turn is gated", rec.Code, rec.Body.String())
		}
		bearer := map[string]string{"Authorization": "Bearer dev-secret", realtimeRequiredHeaderName: "1"}
		if rec := post(bearer); rec.Code != http.StatusOK {
			t.Fatalf("bearer with the marker = %d %s, want 200: a client cannot set the marker", rec.Code, rec.Body.String())
		}
	})
}
