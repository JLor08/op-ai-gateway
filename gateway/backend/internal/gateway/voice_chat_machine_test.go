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
	"op-ai-gateway/internal/auth"
	"strings"
	"sync"
	"sync/atomic"
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
//
// It also RECORDS every request-body read in arrival order (reads) and can HOLD its accept
// (acceptGate): audio.cpp starts its idle timer when it accepts the request, and a cold ASR
// model makes that accept slow, so a gated child models the dial the per-turn loopback must
// survive. While the accept is held the child still reads (and records) the request body,
// exactly like a real child that is waiting on its model -- which is what lets a test assert
// the gateway fed PCM into the loopback DURING the dial instead of only after it.
type fakeVoiceASRChild struct {
	server *httptest.Server

	mu        sync.Mutex
	conns     int
	bytes     int64
	finals    []string // done text per connection (1-based); fallback "final-N"
	emitError bool
	reads     []fakeVoiceBodyRead

	// acceptGate, when non-nil (set BEFORE the first request), holds every connection's
	// response -- the 200 + SSE headers, i.e. the accept -- until the channel closes, the
	// request ends, or fakeVoiceAcceptHoldCap elapses (a safety net so a failing test can
	// never wedge the harness teardown).
	acceptGate chan struct{}
}

// fakeVoiceAcceptHoldCap bounds how long a gated child holds its accept regardless of the
// gate, so a test that fails before releasing the gate cannot hang httptest.Server.Close.
const fakeVoiceAcceptHoldCap = 15 * time.Second

// fakeVoiceBodyRead is one request-body read the child saw: which connection (1-based), the
// bytes, and whether it arrived BEFORE the child accepted that connection (its response
// headers were sent). reads is in arrival order across connections.
type fakeVoiceBodyRead struct {
	conn         int
	data         []byte
	beforeAccept bool
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
	gate := c.acceptGate
	c.mu.Unlock()
	if emitError {
		gate = nil // an erroring child never reads the body, so there is nothing to hold for
	}

	rc := http.NewResponseController(w)
	_ = rc.EnableFullDuplex()

	// accepted flips once the response headers go out; a body read before that is recorded
	// beforeAccept. Without a gate the child accepts at once, so no read is ever before it.
	var accepted atomic.Bool
	next := c.bodyReader(r.Body, idx, gate, &accepted)

	if gate != nil {
		select {
		case <-gate:
		case <-r.Context().Done():
		case <-time.After(fakeVoiceAcceptHoldCap):
		}
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	_ = rc.Flush()
	accepted.Store(true)

	if emitError {
		fmt.Fprint(w, "data: {\"type\":\"error\",\"error\":{\"message\":\"backend blew up\"}}\n\n")
		_ = rc.Flush()
		return
	}

	frame := 0
	var readErr error
	for {
		n, err := next()
		if n > 0 {
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

// bodyReader returns the func the handler pulls request-body reads through, each yielding the
// byte count and error of one read. It records every read (c.reads, c.bytes) the moment it
// happens. With no gate it reads the body inline. With a gate a dedicated goroutine reads the
// body from the moment the request arrives, so the child keeps consuming (and recording) PCM
// while its accept is held; the handler then drains the buffered results after the accept, in
// order. The goroutine only touches the request body (never the ResponseWriter) and ends on
// the read error the handler loops until, so it cannot outlive the handler.
func (c *fakeVoiceASRChild) bodyReader(body io.Reader, idx int, gate chan struct{}, accepted *atomic.Bool) func() (int, error) {
	buf := make([]byte, 4096)
	readOnce := func() (int, error) {
		n, err := body.Read(buf)
		if n > 0 {
			c.mu.Lock()
			c.bytes += int64(n)
			c.reads = append(c.reads, fakeVoiceBodyRead{conn: idx, data: append([]byte(nil), buf[:n]...), beforeAccept: !accepted.Load()})
			c.mu.Unlock()
		}
		return n, err
	}
	if gate == nil {
		return readOnce
	}
	type result struct {
		n   int
		err error
	}
	results := make(chan result, 1024)
	go func() {
		for {
			n, err := readOnce()
			results <- result{n, err}
			if err != nil {
				return
			}
		}
	}()
	return func() (int, error) {
		res := <-results
		return res.n, res.err
	}
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

// bodyReads returns a copy of every request-body read the child has seen, in arrival order.
func (c *fakeVoiceASRChild) bodyReads() []fakeVoiceBodyRead {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]fakeVoiceBodyRead(nil), c.reads...)
}

// bytesBeforeAccept is the PCM the child received on connections it had not yet accepted --
// non-zero only if the gateway fed the body while the dial was still in flight.
func (c *fakeVoiceASRChild) bytesBeforeAccept() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	total := 0
	for _, rd := range c.reads {
		if rd.beforeAccept {
			total += len(rd.data)
		}
	}
	return total
}

// newVoiceMachineHarness builds the full three-leg voice loopback (see newVoiceTurnHarness):
// the ASR chunking tests run against the SAME harness the turn tests do, so a chunk end
// starts a real speculative run rather than a placeholder -- these tests simply assert the
// ASR-side behaviour (partials, finals, the mic gate) and leave the answer to run in the
// background. The chunk-timing is the only knob they vary.
func newVoiceMachineHarness(t *testing.T, child *fakeVoiceASRChild, timing voiceTiming) *voiceTurnHarness {
	t.Helper()
	return newVoiceTurnHarness(t, child, voiceTurnOpts{timing: timing})
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

// TestVoiceMicGateOpenInListeningAndWindow pins the half-duplex guarantee at the unit level:
// the mic feeds the ASR in listening AND in thinking while the continuation window is open
// (spec 6.1) -- and never in idle, speaking or draining, never once the window has closed,
// and never while finalizing a chunk. The speaking/draining closure is what makes it
// impossible for TTS-era audio to reach the ASR, regardless of client behaviour.
func TestVoiceMicGateOpenInListeningAndWindow(t *testing.T) {
	m := &voiceMachine{}
	// The mic is CLOSED in idle, speaking and draining.
	for _, st := range []string{voiceStateIdle, voiceStateSpeaking, voiceStateDraining} {
		m.state, m.finalizing, m.windowClosed = st, false, false
		if m.micOpen() {
			t.Fatalf("micOpen in %q = true, want the mic gated", st)
		}
	}
	// Open in listening.
	m.state, m.finalizing, m.windowClosed = voiceStateListening, false, false
	if !m.micOpen() {
		t.Fatal("micOpen in listening = false, want the mic open")
	}
	// Open in thinking while the continuation window is open.
	m.state, m.finalizing, m.windowClosed = voiceStateThinking, false, false
	if !m.micOpen() {
		t.Fatal("micOpen in thinking (window open) = false, want the mic open for resumed speech")
	}
	// Closed in thinking once the window has closed (we are committing to the answer).
	m.windowClosed = true
	if m.micOpen() {
		t.Fatal("micOpen in thinking after the window closed = true, want it gated")
	}
	// Closed while finalizing a chunk, in any mic-open state.
	m.state, m.finalizing, m.windowClosed = voiceStateListening, true, false
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

// TestResolveVoiceTimingMuteTeardown pins the one knob whose zero is NOT "use the default":
// the mute teardown. Unset resolves to the 10s default, an explicit MuteKeepalive means
// "never tear down while muted" (the operator's 0), and a positive duration is taken as
// given. The keepalive flag must survive resolution -- folding it into the default is the
// bug this guards.
func TestResolveVoiceTimingMuteTeardown(t *testing.T) {
	const wantDefault = VoiceMuteTeardownMs * time.Millisecond
	if wantDefault != 10*time.Second {
		t.Fatalf("VoiceMuteTeardownMs default = %v, want 10s", wantDefault)
	}
	cases := []struct {
		name          string
		in            VoiceTiming
		wantTeardown  time.Duration
		wantKeepalive bool
	}{
		{"unset resolves to the default", VoiceTiming{}, wantDefault, false},
		{"negative duration is junk and uses the default", VoiceTiming{MuteTeardown: -time.Second}, wantDefault, false},
		{"positive duration is taken as given", VoiceTiming{MuteTeardown: 3 * time.Second}, 3 * time.Second, false},
		{"keepalive is preserved, not folded into the default", VoiceTiming{MuteKeepalive: true}, wantDefault, true},
		{"keepalive wins over a duration", VoiceTiming{MuteTeardown: 3 * time.Second, MuteKeepalive: true}, 3 * time.Second, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveVoiceTiming(tc.in)
			if got.muteTeardown != tc.wantTeardown || got.muteKeepalive != tc.wantKeepalive {
				t.Fatalf("resolveVoiceTiming(%+v) = {muteTeardown:%v muteKeepalive:%v}, want {%v %v}",
					tc.in, got.muteTeardown, got.muteKeepalive, tc.wantTeardown, tc.wantKeepalive)
			}
			// The accessor Task 2 reads must agree with the resolved fields.
			after, keepalive := got.muteTeardownAfter()
			if keepalive != tc.wantKeepalive || (!keepalive && after != tc.wantTeardown) {
				t.Fatalf("muteTeardownAfter() = (%v, %v), want (%v, %v)", after, keepalive, tc.wantTeardown, tc.wantKeepalive)
			}
		})
	}
}

// TestVoiceTimingMuteTeardownAfterUnresolved: a hand-built voiceTiming (a test fixture, or the
// bare &Server{}) that never went through resolveVoiceTiming has no teardown set; the
// accessor reads that as the default rather than as "tear down immediately".
func TestVoiceTimingMuteTeardownAfterUnresolved(t *testing.T) {
	after, keepalive := fastVoiceTiming().muteTeardownAfter()
	if keepalive || after != VoiceMuteTeardownMs*time.Millisecond {
		t.Fatalf("unresolved muteTeardownAfter() = (%v, %v), want (%dms, false)", after, keepalive, VoiceMuteTeardownMs)
	}
	set := fastVoiceTiming()
	set.muteTeardown = 40 * time.Millisecond
	if after, keepalive := set.muteTeardownAfter(); keepalive || after != 40*time.Millisecond {
		t.Fatalf("explicit muteTeardownAfter() = (%v, %v), want (40ms, false)", after, keepalive)
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

// TestVoiceTwoTurnsEachCommit: two separate spoken turns each run to completion and commit
// their own [user, assistant] pair, so the document ends with four messages. This pins the
// per-turn clearing of pendingText: a committed turn does not leak into the next (the
// within-a-turn accumulation is the resume case, TestVoiceResumeCancelsAndExtends).
func TestVoiceTwoTurnsEachCommit(t *testing.T) {
	child := newFakeVoiceASRChild(t)
	child.finals = []string{"first turn", "second turn"}
	h := newVoiceTurnHarness(t, child, voiceTurnOpts{timing: turnGraceTiming()})

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()

	conn := h.dial(t, ctx)
	readVoiceUntilState(t, ctx, conn, voiceStateListening)

	// Turn 1: chunk -> run -> commit -> back to listening.
	if f := drainAChunk(t, ctx, conn); f != "first turn" {
		t.Fatalf("turn 1 final = %q, want %q", f, "first turn")
	}
	finishSpokenTurn(t, ctx, conn)

	// Turn 2: a fresh chunk -> a fresh run on only the second turn's text.
	if f := drainAChunk(t, ctx, conn); f != "second turn" {
		t.Fatalf("turn 2 final = %q, want %q", f, "second turn")
	}
	finishSpokenTurn(t, ctx, conn)

	stored, err := h.srv.Portal.GetChat(context.Background(), auth.Token{UserID: "usr_dev"}, h.chatID)
	if err != nil {
		t.Fatal(err)
	}
	msgs := storedMessages(t, stored.Content)
	if len(msgs) != 4 {
		t.Fatalf("after two turns the chat has %d messages, want 4 [user, assistant, user, assistant]: %s", len(msgs), stored.Content)
	}
	if !strings.Contains(string(msgs[0].Content), "first turn") || !strings.Contains(string(msgs[2].Content), "second turn") {
		t.Fatalf("the two user turns did not each carry their own text: %s", stored.Content)
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
// is speaking/draining never reaches the ASR loopback. The turn runs end to end (the real
// run commits) and holds in draining under a long mic-resume tail; PCM sent there opens no
// new ASR loopback and adds no bytes to the child, so the child's connection count and byte
// total are exactly what the one real chunk produced before the answer began.
func TestVoiceMicDroppedWhileSpeaking(t *testing.T) {
	child := newFakeVoiceASRChild(t)
	child.finals = []string{"turn one"}
	timing := turnGraceTiming()
	timing.micResumeTail = 3 * time.Second // hold in draining across the whole assertion
	h := newVoiceTurnHarness(t, child, voiceTurnOpts{timing: timing})
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()

	conn := h.dial(t, ctx)
	readVoiceUntilState(t, ctx, conn, voiceStateListening)

	// One real chunk: its 320 bytes reach the ASR (connection #1). No PCM is sent during the
	// continuation window, so there is no resume and the turn commits.
	if f := drainAChunk(t, ctx, conn); f != "turn one" {
		t.Fatalf("asr_final = %q, want %q", f, "turn one")
	}

	// Walk into draining and stay there (long tail). The window closed, the answer spoke, the
	// run committed; now the mic is gated.
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
