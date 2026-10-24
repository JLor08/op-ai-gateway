// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/routing"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// These tests drive the Voice Chat state machine (relayVoiceChat / voiceMachine) end to
// end: a real browser WebSocket -> the in-process loopback -> /v1/audio/transcriptions/live
// -> a FAKE full-duplex ASR child that speaks the live SSE contract. They cover the part
// the transport tests (voice_chat_ws_test.go) deliberately leave out: per-turn ASR
// chunking, pendingText accumulation, the silence/min/max turn detection, and the
// half-duplex mic gate.

// fakeVoiceASRChild is a full-duplex /live child for the voice loopback: for each PCM chunk
// it reads it emits a transcript.text.partial (so the machine streams asr_partial and the
// silence timer resets), and on a clean end-of-input (io.EOF -- the terminating chunk the
// gateway sends when the machine closes the turn pipe) it emits transcript.text.done with
// this connection's configured final text, then [DONE]. A body cut WITHOUT end-of-input
// (a non-EOF read error) ends silently. It counts connections and the total PCM bytes it
// received, which is how a test proves the mic gate dropped speaking-era audio.
type fakeVoiceASRChild struct {
	server *httptest.Server

	mu        sync.Mutex
	conns     int
	bytes     int64
	finals    []string // done text per connection (1-based); fallback "final-N"
	emitError bool
}

func newFakeVoiceASRChild(t *testing.T) *fakeVoiceASRChild {
	t.Helper()
	c := &fakeVoiceASRChild{}
	c.server = httptest.NewServer(http.HandlerFunc(c.serve))
	t.Cleanup(c.server.Close)
	return c
}

func (c *fakeVoiceASRChild) serve(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	c.conns++
	idx := c.conns
	emitError := c.emitError
	c.mu.Unlock()

	rc := http.NewResponseController(w)
	_ = rc.EnableFullDuplex()
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	_ = rc.Flush()

	if emitError {
		fmt.Fprint(w, "data: {\"type\":\"error\",\"error\":{\"message\":\"backend blew up\"}}\n\n")
		_ = rc.Flush()
		return
	}

	buf := make([]byte, 4096)
	frame := 0
	var readErr error
	for {
		n, err := r.Body.Read(buf)
		if n > 0 {
			c.mu.Lock()
			c.bytes += int64(n)
			c.mu.Unlock()
			frame++
			fmt.Fprintf(w, "data: {\"type\":\"transcript.text.partial\",\"text\":\"p%d\"}\n\n", frame)
			_ = rc.Flush()
		}
		if err != nil {
			readErr = err
			break
		}
	}
	if readErr != io.EOF {
		return // truncated: no clean final
	}
	fmt.Fprintf(w, "data: {\"type\":\"transcript.text.done\",\"text\":%q}\n\n", c.finalFor(idx))
	fmt.Fprint(w, "data: [DONE]\n\n")
	_ = rc.Flush()
}

func (c *fakeVoiceASRChild) finalFor(idx int) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if idx-1 >= 0 && idx-1 < len(c.finals) {
		return c.finals[idx-1]
	}
	return fmt.Sprintf("final-%d", idx)
}

func (c *fakeVoiceASRChild) snapshot() (conns int, bytes int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.conns, c.bytes
}

// voiceMachineHarness is the full loopback chain for a voice session: the
// transcription-capable routing fixtures (asr-model -> route-asr -> whisper_x, streaming
// yes) pointed at the fake ASR child, with selfBaseURL wired to the gateway's own address
// so the per-turn loopback is a real in-process HTTP call. returned receives one value each
// time the voice WebSocket handler returns (so a test can observe a single clean teardown).
type voiceMachineHarness struct {
	srv      *Server
	url      string
	child    *fakeVoiceASRChild
	returned chan struct{}
}

func newVoiceMachineHarness(t *testing.T, child *fakeVoiceASRChild, timing voiceTiming) *voiceMachineHarness {
	t.Helper()
	srv, _ := newTranscriptionCapableTestServer(t, child.server.URL)
	srv.SetBaseContext(context.Background())
	srv.voiceTiming = timing
	h := &voiceMachineHarness{srv: srv, child: child, returned: make(chan struct{}, 16)}
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
	return h
}

// fastVoiceTiming is a short timing for the chunk tests; a test overrides the fields it
// cares about.
func fastVoiceTiming() voiceTiming {
	return voiceTiming{
		silenceTimeout:    150 * time.Millisecond,
		minUtterance:      40 * time.Millisecond,
		maxUtterance:      5 * time.Second,
		micResumeTail:     30 * time.Millisecond,
		continuationGrace: time.Second,
	}
}

func (h *voiceMachineHarness) dial(t *testing.T, ctx context.Context) *websocket.Conn {
	t.Helper()
	c := sampleVoiceTicketClaims()
	c.ASR = routing.Target{Model: "asr-model"}
	c.SampleRate, c.Channels, c.SampleFormat = 16000, 1, "s16le"
	c.UserID = "usr_dev"
	c.TokenID, c.ServiceID, c.ProjectID = "", "", ""
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

func sendVoicePCM(t *testing.T, ctx context.Context, conn *websocket.Conn, n int) {
	t.Helper()
	if err := conn.Write(ctx, websocket.MessageBinary, make([]byte, n)); err != nil {
		t.Fatalf("send pcm: %v", err)
	}
}

// readVoiceUntil reads down-frames until one of the wanted type, returning it. It fails if
// the socket ends first.
func readVoiceUntil(t *testing.T, ctx context.Context, conn *websocket.Conn, typ string) voiceDownMsg {
	t.Helper()
	for {
		m, _ := readVoiceFrame(t, ctx, conn)
		if m.Type == typ {
			return m
		}
	}
}

// readVoiceUntilState reads down-frames until a state frame naming state, returning it.
func readVoiceUntilState(t *testing.T, ctx context.Context, conn *websocket.Conn, state string) {
	t.Helper()
	for {
		m, _ := readVoiceFrame(t, ctx, conn)
		if m.Type == voiceDownState && m.State == state {
			return
		}
	}
}

// TestVoiceMicGateOnlyOpenWhenListening pins the half-duplex guarantee at the unit level:
// the mic feeds the ASR ONLY in listening, and never while finalizing a chunk. idle,
// thinking, speaking and draining all discard -- which is what makes it impossible for
// speaking/draining audio to reach the ASR, regardless of client behaviour.
func TestVoiceMicGateOnlyOpenWhenListening(t *testing.T) {
	m := &voiceMachine{}
	for _, st := range []string{voiceStateIdle, voiceStateThinking, voiceStateSpeaking, voiceStateDraining} {
		m.state, m.finalizing = st, false
		if m.micOpen() {
			t.Fatalf("micOpen in %q = true, want the mic gated (not listening)", st)
		}
	}
	m.state, m.finalizing = voiceStateListening, false
	if !m.micOpen() {
		t.Fatal("micOpen in listening = false, want the mic open")
	}
	m.finalizing = true
	if m.micOpen() {
		t.Fatal("micOpen while finalizing a chunk = true, want it gated (the pipe is closing)")
	}
}

// TestResolveVoiceTimingDefaults proves a zero VoiceTiming resolves to the Voice*Ms
// defaults and a set field overrides only itself.
func TestResolveVoiceTimingDefaults(t *testing.T) {
	got := resolveVoiceTiming(VoiceTiming{})
	if got != defaultVoiceTiming() {
		t.Fatalf("resolveVoiceTiming(zero) = %+v, want the Voice*Ms defaults %+v", got, defaultVoiceTiming())
	}
	if got.silenceTimeout != VoiceSilenceTimeoutMs*time.Millisecond {
		t.Fatalf("default silenceTimeout = %v, want %dms", got.silenceTimeout, VoiceSilenceTimeoutMs)
	}
	set := resolveVoiceTiming(VoiceTiming{SilenceTimeout: 42 * time.Millisecond})
	if set.silenceTimeout != 42*time.Millisecond || set.minUtterance != VoiceMinUtteranceMs*time.Millisecond {
		t.Fatalf("resolveVoiceTiming(one field) = %+v, want only silence overridden", set)
	}
}

// TestVoiceChunkEndsOnSilence: after a voiced utterance, no new partial for the silence
// timeout ends the chunk -- the gateway closes the per-turn pipe, the ASR returns the clean
// final, and the machine emits asr_final.
func TestVoiceChunkEndsOnSilence(t *testing.T) {
	child := newFakeVoiceASRChild(t)
	child.finals = []string{"hello world"}
	h := newVoiceMachineHarness(t, child, fastVoiceTiming())
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	conn := h.dial(t, ctx)
	readVoiceUntilState(t, ctx, conn, voiceStateListening)

	// Two voiced partials spaced beyond the min-utterance guard, then go quiet.
	sendVoicePCM(t, ctx, conn, 320)
	readVoiceUntil(t, ctx, conn, voiceDownASRPartial)
	time.Sleep(80 * time.Millisecond)
	sendVoicePCM(t, ctx, conn, 320)
	readVoiceUntil(t, ctx, conn, voiceDownASRPartial)

	// No more PCM -> the silence timer (150ms) fires -> the chunk ends with the clean final.
	// Bound the read well under the 5s max-utterance cap, so it is the SILENCE timer (not the
	// hard cap) that must have ended the chunk.
	sctx, scancel := context.WithTimeout(ctx, time.Second)
	defer scancel()
	final := readVoiceUntil(t, sctx, conn, voiceDownASRFinal)
	if final.Text != "hello world" {
		t.Fatalf("asr_final text = %q, want the clean final %q", final.Text, "hello world")
	}
}

// TestVoiceChunkEndsOnPushEnd: an explicit push_end ends the chunk immediately, without
// waiting for the silence timer.
func TestVoiceChunkEndsOnPushEnd(t *testing.T) {
	child := newFakeVoiceASRChild(t)
	child.finals = []string{"done now"}
	timing := fastVoiceTiming()
	timing.silenceTimeout = 10 * time.Second // prove it is push_end, not the timer, that ends it
	h := newVoiceMachineHarness(t, child, timing)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	conn := h.dial(t, ctx)
	readVoiceUntilState(t, ctx, conn, voiceStateListening)

	sendVoicePCM(t, ctx, conn, 320)
	readVoiceUntil(t, ctx, conn, voiceDownASRPartial)
	time.Sleep(60 * time.Millisecond) // past the min guard, though push_end bypasses it anyway
	sendVoiceControl(t, ctx, conn, "push_end")

	final := readVoiceUntil(t, ctx, conn, voiceDownASRFinal)
	if final.Text != "done now" {
		t.Fatalf("asr_final text = %q, want %q", final.Text, "done now")
	}
}

// TestVoicePendingTextAccumulatesAcrossChunks: two chunks in a session append to pendingText
// in order, so the buffer is chunk1 + chunk2 (the substrate the Task-7 continuation window
// sends to the LLM).
func TestVoicePendingTextAccumulatesAcrossChunks(t *testing.T) {
	child := newFakeVoiceASRChild(t)
	child.finals = []string{"one", "two"}
	timing := fastVoiceTiming()
	timing.silenceTimeout = 10 * time.Second // end each chunk with push_end, deterministically
	timing.micResumeTail = 20 * time.Millisecond
	h := newVoiceMachineHarness(t, child, timing)

	var mu sync.Mutex
	var lastPending string
	h.srv.voiceOnPending = func(p string) { mu.Lock(); lastPending = p; mu.Unlock() }

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	conn := h.dial(t, ctx)
	readVoiceUntilState(t, ctx, conn, voiceStateListening)

	// Chunk 1.
	sendVoicePCM(t, ctx, conn, 320)
	readVoiceUntil(t, ctx, conn, voiceDownASRPartial)
	sendVoiceControl(t, ctx, conn, "push_end")
	if f := readVoiceUntil(t, ctx, conn, voiceDownASRFinal); f.Text != "one" {
		t.Fatalf("chunk 1 final = %q, want %q", f.Text, "one")
	}

	// The placeholder answer walks thinking -> speaking -> draining -> listening.
	readVoiceUntilState(t, ctx, conn, voiceStateListening)

	// Chunk 2 (a fresh loopback).
	sendVoicePCM(t, ctx, conn, 320)
	readVoiceUntil(t, ctx, conn, voiceDownASRPartial)
	sendVoiceControl(t, ctx, conn, "push_end")
	if f := readVoiceUntil(t, ctx, conn, voiceDownASRFinal); f.Text != "two" {
		t.Fatalf("chunk 2 final = %q, want %q", f.Text, "two")
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		got := lastPending
		mu.Unlock()
		if got == "onetwo" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("pendingText = %q, want it accumulated to %q across the two chunks", got, "onetwo")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestVoiceSubMinUtteranceIgnored: a blip shorter than the min-utterance guard does not end
// a chunk when the silence timer fires -- no asr_final is emitted and the session stays
// listening.
func TestVoiceSubMinUtteranceIgnored(t *testing.T) {
	child := newFakeVoiceASRChild(t)
	child.finals = []string{"should-not-appear"}
	timing := fastVoiceTiming()
	timing.silenceTimeout = 120 * time.Millisecond
	timing.minUtterance = 300 * time.Millisecond // a single quick partial has a ~0 voiced span
	h := newVoiceMachineHarness(t, child, timing)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	conn := h.dial(t, ctx)
	readVoiceUntilState(t, ctx, conn, voiceStateListening)

	// One short blip: a single partial, then quiet.
	sendVoicePCM(t, ctx, conn, 320)
	readVoiceUntil(t, ctx, conn, voiceDownASRPartial)

	// The silence timer fires at 120ms, but the voiced span is below the 300ms guard, so the
	// chunk must NOT end: no asr_final within a window well past the silence timeout.
	rctx, rcancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer rcancel()
	for {
		typ, data, err := conn.Read(rctx)
		if err != nil {
			break // the read deadline elapsed with no chunk end: the blip was ignored
		}
		if typ != websocket.MessageText {
			continue
		}
		var m voiceDownMsg
		if json.Unmarshal(data, &m) == nil && m.Type == voiceDownASRFinal {
			t.Fatalf("a sub-min blip was finalized (asr_final %q); the min guard did not hold", m.Text)
		}
	}
}

// TestVoiceMicDroppedWhileSpeaking is the loop guarantee: binary PCM sent while the session
// is speaking/draining never reaches the ASR loopback. With a long mic-resume tail the
// machine holds in draining; PCM sent there opens no new loopback and adds no bytes to the
// child, so the child's connection count and byte total are exactly what the one real chunk
// produced.
func TestVoiceMicDroppedWhileSpeaking(t *testing.T) {
	child := newFakeVoiceASRChild(t)
	child.finals = []string{"turn one"}
	timing := fastVoiceTiming()
	timing.silenceTimeout = 10 * time.Second // end the chunk with push_end only
	timing.micResumeTail = 3 * time.Second   // hold in draining across the whole assertion
	h := newVoiceMachineHarness(t, child, timing)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	conn := h.dial(t, ctx)
	readVoiceUntilState(t, ctx, conn, voiceStateListening)

	// One real chunk: 320 bytes reach the ASR (connection #1).
	sendVoicePCM(t, ctx, conn, 320)
	readVoiceUntil(t, ctx, conn, voiceDownASRPartial)
	sendVoiceControl(t, ctx, conn, "push_end")
	readVoiceUntil(t, ctx, conn, voiceDownASRFinal)

	// Walk into draining and stay there (long tail).
	readVoiceUntilState(t, ctx, conn, voiceStateDraining)
	beforeConns, beforeBytes := child.snapshot()

	// Speaking/draining-era audio: a burst that must be DROPPED server-side.
	for range 5 {
		sendVoicePCM(t, ctx, conn, 3200)
	}
	time.Sleep(400 * time.Millisecond) // give any (buggy) forwarding time to happen

	conns, bytes := child.snapshot()
	if conns != beforeConns {
		t.Fatalf("ASR connections = %d, want %d: draining-era PCM opened a loopback", conns, beforeConns)
	}
	if bytes != beforeBytes {
		t.Fatalf("ASR received %d bytes, want %d: draining-era PCM reached the loopback (the mic gate leaked)", bytes, beforeBytes)
	}
	if beforeBytes != 320 {
		t.Fatalf("the one real chunk delivered %d bytes, want 320 (harness sanity)", beforeBytes)
	}
}

// TestVoiceByeTearsDownOnce: a bye with an open per-turn loopback closes the session cleanly
// and the handler returns exactly once -- the deferred teardown joins the writer, the
// machine and the turn's SSE relay, and cancels the loopback context, with nothing left
// running.
func TestVoiceByeTearsDownOnce(t *testing.T) {
	child := newFakeVoiceASRChild(t)
	child.finals = []string{"x"}
	timing := fastVoiceTiming()
	timing.silenceTimeout = 10 * time.Second
	h := newVoiceMachineHarness(t, child, timing)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	conn := h.dial(t, ctx)
	readVoiceUntilState(t, ctx, conn, voiceStateListening)

	// Open a turn's loopback, then bye.
	sendVoicePCM(t, ctx, conn, 320)
	readVoiceUntil(t, ctx, conn, voiceDownASRPartial)
	sendVoiceControl(t, ctx, conn, "bye")

	if _, _, err := conn.Read(ctx); websocket.CloseStatus(err) != websocket.StatusNormalClosure {
		t.Fatalf("after bye the close status = %v (err %v), want StatusNormalClosure", websocket.CloseStatus(err), err)
	}

	select {
	case <-h.returned:
	case <-time.After(5 * time.Second):
		t.Fatal("the voice handler did not return after bye: the session leaked")
	}
	select {
	case <-h.returned:
		t.Fatal("the voice handler returned twice: teardown ran more than once")
	case <-time.After(200 * time.Millisecond):
	}
}
