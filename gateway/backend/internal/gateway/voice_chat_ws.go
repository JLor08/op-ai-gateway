// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"op-ai-gateway/internal/routing"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
)

// voiceWSSubprotocol is the fixed WebSocket subprotocol the browser offers first on a
// voice-chat upgrade; the single-use ticket (voice_chat_ticket.go) is the SECOND offered
// value. The browser cannot set custom headers on a WebSocket upgrade, so
// `new WebSocket(url, ["op-voice-chat", <ticket>])` is how it carries both the protocol
// selector and the ticket. The handler reads the ticket off Sec-WebSocket-Protocol and
// redeems it BEFORE websocket.Accept, then selects this fixed name in the handshake, so
// the browser's WebSocket.protocol is stable and the opaque ticket never becomes the
// negotiated subprotocol. It is the twin of liveWSSubprotocol.
const voiceWSSubprotocol = "op-voice-chat"

// voiceTicketRefusal is the ONE body of every refused voice-chat upgrade (HTTP 401, no
// upgrade): a missing, forged, expired or replayed ticket all answer with exactly this,
// so a caller holding a captured ticket cannot tell a replay from a forgery from a stale
// one. The specific cause is logged server-side (voiceTicketRefusalCause), never sent.
const voiceTicketRefusal = "voice chat ticket refused"

// voiceOutboundQueue bounds the single writer goroutine's outbound queue. A voice session
// sends small, infrequent frames (state, transcript and answer text) interleaved with the
// speech model's audio_delta chunks, which arrive in bursts, so the queue is twice the
// live transcription's. A sender that outruns it blocks (never drops a frame), bounded by
// the handler context so it can never block past teardown.
const voiceOutboundQueue = 64

// The text control frames the browser sends up (spec 5.2). Binary frames are the mic's
// s16le PCM.
const (
	voiceUpPushStart = "push_start"
	voiceUpPushEnd   = "push_end"
	voiceUpCancel    = "cancel"
	voiceUpBye       = "bye"
)

// The down-frame types the gateway sends to the browser (spec 5.2).
const (
	voiceDownState       = "state"
	voiceDownASRPartial  = "asr_partial"
	voiceDownASRFinal    = "asr_final"
	voiceDownAnswerDelta = "answer_delta"
	voiceDownAnswerDone  = "answer_done"
	voiceDownAnswerReset = "answer_reset"
	voiceDownAudioDelta  = "audio_delta"
	voiceDownAudioDone   = "audio_done"
	voiceDownError       = "error"
)

// The session states a down `state` frame names (spec 5.3). The UI keys its listening
// indicator and the mic gate on them.
const (
	voiceStateIdle      = "idle"
	voiceStateListening = "listening"
	voiceStateThinking  = "thinking"
	voiceStateSpeaking  = "speaking"
	voiceStateDraining  = "draining"
)

// The default Voice Chat turn-detection and half-duplex timings, in milliseconds
// (voice-chat design §6.1/§6.2). They are the defaults of the OP_AI_GATEWAY_VOICE_*
// knobs; config cannot import this package, so it repeats the literals.
//
//   - VoiceSilenceTimeoutMs: after a voiced ASR partial, this much quiet (no new
//     partial) ends the current chunk.
//   - VoiceMinUtteranceMs: a voiced span shorter than this is a blip the silence timer
//     ignores -- it does not end a chunk.
//   - VoiceMaxUtteranceMs: hard cap on one chunk's voiced span; it ends regardless.
//   - VoiceMicResumeTailMs: after draining, the mic stays gated this much longer before
//     it re-opens (the half-duplex tail).
//   - VoiceContinuationGraceMs: the speculative-run grace window before a turn commits.
//     Defined now; the continuation window that uses it is wired in Task 7.
const (
	VoiceSilenceTimeoutMs    = 900
	VoiceMinUtteranceMs      = 300
	VoiceMaxUtteranceMs      = 30000
	VoiceMicResumeTailMs     = 300
	VoiceContinuationGraceMs = 1500
)

// VoiceTiming is the operator-tunable turn-detection and half-duplex timing of a Voice
// Chat session (set from config via ServerDeps; the OP_AI_GATEWAY_VOICE_* knobs). A zero
// field defaults in New to its Voice*Ms default, so a bare ServerDeps gets the defaults.
type VoiceTiming struct {
	SilenceTimeout    time.Duration
	MinUtterance      time.Duration
	MaxUtterance      time.Duration
	MicResumeTail     time.Duration
	ContinuationGrace time.Duration
}

// voiceTiming is the resolved (defaults-filled) timing the state machine reads. It is the
// unexported twin of VoiceTiming so the machine's fields stay internal.
type voiceTiming struct {
	silenceTimeout    time.Duration
	minUtterance      time.Duration
	maxUtterance      time.Duration
	micResumeTail     time.Duration
	continuationGrace time.Duration
}

// defaultVoiceTiming is the built-in timing (the Voice*Ms defaults).
func defaultVoiceTiming() voiceTiming {
	return voiceTiming{
		silenceTimeout:    VoiceSilenceTimeoutMs * time.Millisecond,
		minUtterance:      VoiceMinUtteranceMs * time.Millisecond,
		maxUtterance:      VoiceMaxUtteranceMs * time.Millisecond,
		micResumeTail:     VoiceMicResumeTailMs * time.Millisecond,
		continuationGrace: VoiceContinuationGraceMs * time.Millisecond,
	}
}

// resolveVoiceTiming fills each non-positive field of d with its Voice*Ms default. New
// calls it once so the Server holds a fully-resolved timing.
func resolveVoiceTiming(d VoiceTiming) voiceTiming {
	out := defaultVoiceTiming()
	if d.SilenceTimeout > 0 {
		out.silenceTimeout = d.SilenceTimeout
	}
	if d.MinUtterance > 0 {
		out.minUtterance = d.MinUtterance
	}
	if d.MaxUtterance > 0 {
		out.maxUtterance = d.MaxUtterance
	}
	if d.MicResumeTail > 0 {
		out.micResumeTail = d.MicResumeTail
	}
	if d.ContinuationGrace > 0 {
		out.continuationGrace = d.ContinuationGrace
	}
	return out
}

// voiceUpControl is the only text (non-binary) message the browser sends up: a control
// frame whose Type is one of push_start, push_end, cancel or bye.
type voiceUpControl struct {
	Type string `json:"type"`
}

// parseVoiceUpControl decodes a text frame into a control message. It reports false for
// anything that is not a JSON object naming one of the four known controls: a malformed
// frame, an unknown or differently-cased type (including live transcription's "end"),
// a non-string type. Unknown extra fields are tolerated, so a newer client can add
// detail to a control without breaking an older gateway. The caller ignores a frame that
// does not parse: a control is advice to the state machine, never something a session
// can be wedged by.
func parseVoiceUpControl(data []byte) (voiceUpControl, bool) {
	var c voiceUpControl
	if err := json.Unmarshal(data, &c); err != nil {
		return voiceUpControl{}, false
	}
	switch c.Type {
	case voiceUpPushStart, voiceUpPushEnd, voiceUpCancel, voiceUpBye:
		return c, true
	}
	return voiceUpControl{}, false
}

// voiceDownMsg is the typed envelope the gateway sends down to the browser over the
// WebSocket; the browser switches on Type. One flat struct with a field per shape keeps
// the wire minimal: a field a frame does not own is zero and omitted.
//
//   - state:        State
//   - asr_partial:  Text (a provisional whole transcript, replace-not-append)
//   - asr_final:    Text (the finalized user turn) and MessageID
//   - answer_delta: Text (the increment)
//   - answer_done:  MessageID
//   - answer_reset: nothing (the speculative run was cancelled)
//   - audio_delta:  Seq (from 0), Rate (Hz) and Audio (base64 s16le PCM)
//   - audio_done:   nothing
//   - error:        Code (the stable code of the leg that failed) and Message
type voiceDownMsg struct {
	Type      string `json:"type"`
	State     string `json:"state,omitempty"`
	Text      string `json:"text,omitempty"`
	MessageID string `json:"message_id,omitempty"`
	Seq       int    `json:"seq,omitempty"`
	Rate      int    `json:"rate,omitempty"`
	Audio     string `json:"audio,omitempty"`
	Code      string `json:"code,omitempty"`
	Message   string `json:"message,omitempty"`
}

// MarshalJSON keeps `seq` on the wire for an audio_delta even when it is 0. The chunks
// are numbered from 0, and a plain `omitempty` int would drop the very first chunk's
// number, which the browser's player reads to order the stream. Every other frame omits
// seq, as it owns no such field.
func (m voiceDownMsg) MarshalJSON() ([]byte, error) {
	// plain has voiceDownMsg's fields but not its methods, so marshalling it does not
	// recurse. The outer Seq shadows the embedded one (the shallower field wins).
	type plain voiceDownMsg
	wire := struct {
		plain
		Seq *int `json:"seq,omitempty"`
	}{plain: plain(m)}
	if m.Type == voiceDownAudioDelta {
		wire.Seq = &m.Seq
	}
	return json.Marshal(wire)
}

// voiceMarshal marshals a down-message. The shapes here never fail to marshal; a
// defensive failure yields an empty frame, which the writer skips.
func voiceMarshal(m voiceDownMsg) []byte {
	b, err := json.Marshal(m)
	if err != nil {
		slog.Error("voice chat: marshal down-frame failed", "type", m.Type, "err", err)
		return nil
	}
	return b
}

// handleVoiceChat is GET /api/portal/voice-chat: the browser's full-duplex WebSocket
// transport for Voice Chat (mic PCM and text controls up, state, transcript, answer text
// and speech audio down). It is the twin of handleLiveTranscription and shares its
// security boundary.
//
// Security boundary (BEFORE any upgrade): the single-use, encrypted ticket the
// authenticated CSRF-protected POST minted (voice_chat_ticket_endpoint.go) rides as a
// Sec-WebSocket-Protocol value and is redeemed here before websocket.Accept. A
// missing, forged, expired or replayed ticket is refused HTTP 401 with NO upgrade, and
// every one of them gets the SAME refusal (voiceTicketRefusal); the cause is logged, not
// sent. Accept is called with its DEFAULT origin policy (no InsecureSkipVerify, no
// OriginPatterns), which is coder/websocket's same-origin check: the CSRF/origin defense
// for a browser upgrade that cannot carry X-OP-CSRF. The handler never takes a model, a
// chat or an audio rate from the client: all of that is sealed in the redeemed claims.
func (s *Server) handleVoiceChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// 1. Redeem the ticket BEFORE Accept. A miss is a plain HTTP 401 (no upgrade), which a
	// cross-origin page cannot turn into a session because it cannot obtain a ticket in
	// the first place. All the failures share one body; the log says which one it was.
	if s.voiceTickets == nil {
		slog.Error("voice chat: no ticket store configured, refusing the upgrade")
		http.Error(w, voiceTicketRefusal, http.StatusUnauthorized)
		return
	}
	ticket := extractVoiceTicket(r.Header.Get("Sec-WebSocket-Protocol"))
	if ticket == "" {
		slog.Debug("voice chat: upgrade without a ticket")
		http.Error(w, voiceTicketRefusal, http.StatusUnauthorized)
		return
	}
	claims, err := s.voiceTickets.Redeem(ticket)
	if err != nil {
		slog.Warn("voice chat: ticket refused", "cause", voiceTicketRefusalCause(err))
		http.Error(w, voiceTicketRefusal, http.StatusUnauthorized)
		return
	}

	// 2. Upgrade with the DEFAULT (same-origin) origin policy and the fixed subprotocol
	// selected. A cross-origin request is refused 403 here by coder/websocket itself.
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{voiceWSSubprotocol}})
	if err != nil {
		slog.Debug("voice chat: websocket accept failed", "chat_id", claims.ChatID, "err", err)
		return
	}
	conn.SetReadLimit(maxLiveFrameBytes)

	s.relayVoiceChat(r, conn, claims)
}

// extractVoiceTicket returns the ticket from a Sec-WebSocket-Protocol header value: the
// one offered subprotocol that is NOT the fixed voiceWSSubprotocol. Empty when none is
// offered (so the handler refuses before any upgrade).
func extractVoiceTicket(header string) string {
	for _, part := range strings.Split(header, ",") {
		v := strings.TrimSpace(part)
		if v != "" && v != voiceWSSubprotocol {
			return v
		}
	}
	return ""
}

// voiceTicketRefusalCause names, for the operator's log only, why Redeem refused a ticket.
// It is the one place the three sentinels are told apart; the wire never is.
func voiceTicketRefusalCause(err error) string {
	switch {
	case errors.Is(err, errVoiceTicketInvalid):
		return "invalid"
	case errors.Is(err, errVoiceTicketExpired):
		return "expired"
	case errors.Is(err, errVoiceTicketReplayed):
		return "replayed"
	}
	return "unknown"
}

// voiceInboundQueue bounds the read loop -> state-machine channels for PCM and controls.
// Mic PCM arrives in ~100 ms batches, so a short queue absorbs a brief machine stall
// (a pipe write, a loopback dial) without the read loop ever blocking the socket. A sender
// that outruns it blocks only until the machine takes the frame or the handler context
// ends.
const voiceInboundQueue = 32

// voiceASRQueue bounds a turn's ASR relay -> state-machine event channel. One live ASR
// relay emits delta/partial lines at speaking cadence; a small queue is ample and a full
// one makes the relay block on turnCtx, never past the turn.
const voiceASRQueue = 16

// errVoiceTurnDone unblocks a turn's loopback pipe on a clean turn end or teardown: the
// loopback transport's body read (reading the pipe reader) and any pending pipe write both
// return it. It is never surfaced to the client.
var errVoiceTurnDone = errors.New("voice chat: turn done")

// relayVoiceChat runs the browser-facing half of a voice session after the upgrade and
// drives the Voice Chat state machine over it. It owns the session's lifecycle: ONE
// context cancelled on any exit; a single writer goroutine that owns every WS write (data
// frames, the keepalive ping, the connection close); the state-machine goroutine (the
// single owner of all session state); a read loop (this goroutine) that forwards inbound
// PCM and controls to the machine over channels; and a teardown that joins every goroutine
// and cancels the per-turn ASR loopback, exactly once.
//
// The machine is the SINGLE OWNER of the state, the pendingText buffer, the per-turn ASR
// loopback and the turn timers (voiceMachine), so no two goroutines ever touch them: the
// half-duplex mic gate is a plain read of the machine's own state variable, race-free by
// construction. The read loop never mutates state; it only forwards events.
func (s *Server) relayVoiceChat(r *http.Request, conn *websocket.Conn, claims voiceTicketClaims) {
	id := nextRequestID()
	slog.Debug("voice chat: session started", "id", id, "chat_id", claims.ChatID)

	// The one context: cancelled on any exit it stops the writer (which closes the
	// connection, unblocking the read loop) and the state machine (which cancels the
	// per-turn loopback). Derived from the request, but the read loop deliberately does
	// NOT read on it (see voiceReadLoop).
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	// The sole writer. Every WS write, frames, the ping and the connection close, goes
	// through it, so writes are always serialized.
	writer := newVoiceWriter(conn, cancel)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); writer.run(ctx) }()

	// The read loop -> machine channels. Buffered so a brief machine stall never blocks
	// the socket read.
	pcmCh := make(chan []byte, voiceInboundQueue)
	ctrlCh := make(chan voiceUpControl, voiceInboundQueue)

	// The state machine: the single owner of all session state.
	m := s.newVoiceMachine(ctx, cancel, writer, claims, pcmCh, ctrlCh)
	wg.Add(1)
	go func() { defer wg.Done(); m.run() }()

	// Teardown, DEFERRED so it is airtight even if the relay panics: cancel the one
	// context and join every goroutine. Cancelling ctx makes the writer flush and close
	// the connection, and makes the machine cancel the per-turn ASR loopback and join its
	// SSE relay goroutine before it returns; wg.Wait then blocks until the writer and the
	// machine (and, through it, any turn relay) have all returned, so nothing is left
	// running or leaked.
	defer func() {
		cancel()
		wg.Wait()
		slog.Debug("voice chat: session ended", "id", id, "chat_id", claims.ChatID)
	}()

	voiceReadLoop(ctx, conn, cancel, writer, pcmCh, ctrlCh)
}

// voiceReadLoop reads the browser's frames until the session ends and returns on the
// first read error (a close, a drop, a frame over the read limit) or on bye. It never
// touches session state; it forwards events to the state machine.
//
//   - A BINARY frame is mic PCM: a COPY is forwarded to the machine over pcmCh (the
//     machine applies the half-duplex gate -- the read loop never decides discard-vs-pipe).
//     A copy is made because conn.Read may reuse its buffer on the next read.
//   - A TEXT frame is a control (parseVoiceUpControl). bye ends the session with a clean
//     close handshake; push_start/push_end/cancel are forwarded over ctrlCh; anything that
//     does not parse is ignored.
//
// The read uses context.Background(), NOT the handler context: cancelling that context
// makes coder/websocket tear the connection down abruptly from a second site, which would
// race the single writer. The writer's close (on its ctx.Done teardown) is what unblocks
// this read instead. A forward blocks only until the machine takes it or ctx ends.
//
// On bye the loop stops reading before the writer closes, so the close HANDSHAKE
// (writer.closeNormally) can take the read mutex it needs. On any other exit cancel() tears
// the whole relay down (the machine and the writer observe ctx.Done).
func voiceReadLoop(ctx context.Context, conn *websocket.Conn, cancel context.CancelFunc, writer *voiceWriter, pcmCh chan<- []byte, ctrlCh chan<- voiceUpControl) {
	readCtx := context.Background()
	for {
		typ, data, err := conn.Read(readCtx)
		if err != nil {
			cancel() // a drop/close/oversize frame tears the relay down
			return
		}
		switch typ {
		case websocket.MessageBinary:
			frame := append([]byte(nil), data...)
			select {
			case pcmCh <- frame:
			case <-ctx.Done():
				return
			}
		case websocket.MessageText:
			ctl, ok := parseVoiceUpControl(data)
			if !ok {
				continue
			}
			if ctl.Type == voiceUpBye {
				writer.closeNormally()
				return
			}
			select {
			case ctrlCh <- ctl:
			case <-ctx.Done():
				return
			}
		}
	}
}

// asrEventKind is the kind of one event the per-turn ASR relay forwards to the machine.
type asrEventKind int

const (
	asrKindDelta   asrEventKind = iota // an incremental transcript fragment
	asrKindPartial                     // a provisional WHOLE transcript (replace-not-append)
	asrKindDone                        // the clean final transcript (after end-of-input)
	asrKindError                       // a mid-stream upstream error
)

// asrEvent is one decoded line of the live ASR SSE, forwarded from a turn's relay goroutine
// to the machine over the turn's asrCh.
type asrEvent struct {
	kind    asrEventKind
	text    string
	code    string
	message string
}

// asrEventFromLive maps a decoded live-transcription down-message to an asrEvent, reusing
// the live SSE decoder (liveDownMsgFromSSELine) so the two paths never drift. It reports
// false for a line that carries nothing the machine consumes.
func asrEventFromLive(m liveDownMsg) (asrEvent, bool) {
	switch m.Type {
	case "delta":
		return asrEvent{kind: asrKindDelta, text: m.Text}, true
	case "partial":
		return asrEvent{kind: asrKindPartial, text: m.Text}, true
	case "done":
		return asrEvent{kind: asrKindDone, text: m.Text}, true
	case "error":
		return asrEvent{kind: asrKindError, message: m.Message, code: m.Code}, true
	}
	return asrEvent{}, false
}

// voiceASRRelay reads one turn's live-ASR SSE body and forwards each decoded event to the
// machine over asrCh, then closes asrCh so the machine sees the relay end. It reuses the
// exact live SSE parser; a line past the bound ends the relay (and the turn). A send blocks
// only until the machine takes it or turnCtx ends, so the relay can never outlive the turn.
func voiceASRRelay(turnCtx context.Context, body io.Reader, asrCh chan<- asrEvent) {
	defer close(asrCh)
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 0, 64*1024), maxLiveSSELineBytes)
	for sc.Scan() {
		msg, ok := liveDownMsgFromSSELine(sc.Bytes())
		if !ok {
			continue
		}
		ev, ok := asrEventFromLive(msg)
		if !ok {
			continue
		}
		select {
		case asrCh <- ev:
		case <-turnCtx.Done():
			return
		}
		if ev.kind == asrKindError {
			return // a mid-stream error is terminal; stop so teardown can abort the body
		}
	}
}

// voiceMachine is the single-owner Voice Chat state machine. Exactly one goroutine (run)
// ever reads or writes its fields, so the state, the pendingText buffer, the per-turn ASR
// loopback and the turn timers need no locks, and the half-duplex mic gate (onPCM) is a
// plain read of state -- race-free by construction. Every event reaches it over a channel:
// mic PCM and controls from the read loop, ASR SSE events from the per-turn relay, and the
// turn timers it arms itself.
//
// TODO(Task 7): the speculative voice run + continuation window attaches at the chunk
// boundary (see beginAnswerPlaceholder); this task delivers the states, the per-turn ASR
// loopback, the chunk detection + pendingText accumulation, the timers and the half-duplex
// mic gate, with the run/TTS/commit left as a placeholder.
type voiceMachine struct {
	s      *Server
	ctx    context.Context
	cancel context.CancelFunc
	writer *voiceWriter
	claims voiceTicketClaims
	timing voiceTiming

	pcmCh  <-chan []byte
	ctrlCh <-chan voiceUpControl

	state       string
	pendingText string

	// The current turn's ASR loopback. asrCh is nil whenever no loopback is open, which
	// disables its select case; relayWG joins the turn's SSE relay goroutine.
	pw         *io.PipeWriter
	turnCancel context.CancelFunc
	respBody   io.ReadCloser
	asrCh      chan asrEvent
	relayWG    sync.WaitGroup

	// The current chunk's accumulating transcript and the voiced-span bounds used by the
	// min/max utterance guards. chunkActive latches on the first voiced partial; finalizing
	// is set once the pipe is closed and we await the clean final.
	chunkText      string
	chunkActive    bool
	finalizing     bool
	firstPartialAt time.Time
	lastPartialAt  time.Time

	// The turn timers. Each is held as the *Timer (to Stop) plus its channel (nil when
	// disarmed, which disables its select case).
	silenceTimer  *time.Timer
	silenceC      <-chan time.Time
	maxTimer      *time.Timer
	maxC          <-chan time.Time
	tailTimer     *time.Timer
	tailC         <-chan time.Time
	finalizeTimer *time.Timer
	finalizeC     <-chan time.Time
}

// voiceFinalizeTimeout bounds the wait for the clean final transcript after the pipe is
// closed on a chunk end: audio.cpp emits transcript.text.done then [DONE] on end-of-input,
// but a stalled or truncated upstream must not wedge the turn, so after this the machine
// finalizes with whatever provisional text it has.
const voiceFinalizeTimeout = 5 * time.Second

// newVoiceMachine builds the state machine for a session. The timing is the Server's
// resolved voiceTiming (defaults filled in New; a test may override s.voiceTiming).
func (s *Server) newVoiceMachine(ctx context.Context, cancel context.CancelFunc, writer *voiceWriter, claims voiceTicketClaims, pcmCh <-chan []byte, ctrlCh <-chan voiceUpControl) *voiceMachine {
	timing := s.voiceTiming
	if timing == (voiceTiming{}) {
		timing = defaultVoiceTiming()
	}
	return &voiceMachine{
		s: s, ctx: ctx, cancel: cancel, writer: writer, claims: claims, timing: timing,
		pcmCh: pcmCh, ctrlCh: ctrlCh,
	}
}

// run is the single-owner event loop. It opens in listening (the hands-free default) and
// serves every event source over one select until the handler context ends, then tears the
// current turn down and joins its relay.
func (m *voiceMachine) run() {
	m.setState(voiceStateListening)
	for {
		select {
		case <-m.ctx.Done():
			m.teardownTurn()
			m.stopAllTimers()
			return
		case frame := <-m.pcmCh:
			m.onPCM(frame)
		case ctl := <-m.ctrlCh:
			m.onControl(ctl)
		case ev, ok := <-m.asrCh:
			if ok {
				m.onASREvent(ev)
			} else {
				m.onASRClosed()
			}
		case <-m.silenceC:
			m.onSilence()
		case <-m.maxC:
			m.onMaxUtterance()
		case <-m.tailC:
			m.onTailDone()
		case <-m.finalizeC:
			m.onFinalizeTimeout()
		}
	}
}

// setState records the state and, on a real change, announces it down. It is the sole
// writer of the state, so the mic gate (onPCM) reads a value only this goroutine ever sets.
func (m *voiceMachine) setState(state string) {
	if m.state == state {
		return
	}
	m.state = state
	m.writer.send(m.ctx, voiceDownMsg{Type: voiceDownState, State: state})
}

// micOpen reports whether inbound PCM is piped to the ASR this turn. ONLY listening feeds
// the ASR; idle, thinking, speaking and draining all discard -- and while finalizing a
// chunk (the pipe is closing) listening discards too. This is the whole half-duplex
// guarantee: speaking/draining can never route TTS-era audio into the ASR, enforced
// server-side regardless of client behaviour.
func (m *voiceMachine) micOpen() bool {
	return m.state == voiceStateListening && !m.finalizing
}

// onPCM applies the half-duplex mic gate and, when the mic is open, feeds the frame to the
// per-turn ASR loopback (opening it lazily on the turn's first frame). A discarded frame
// never reaches any loopback.
func (m *voiceMachine) onPCM(frame []byte) {
	if !m.micOpen() {
		return // half-duplex: discard during idle/thinking/speaking/draining (and finalizing)
	}
	if m.asrCh == nil {
		if m.s.selfBaseURL == "" {
			return // no loopback target configured (a bare/test server): discard silently
		}
		if !m.openTurnLoopback() {
			return // the dial failed; openTurnLoopback emitted the error and went idle
		}
	}
	if _, err := m.pw.Write(frame); err != nil {
		m.failTurn("transcription.stream_error", "the transcription stream ended")
	}
}

// openTurnLoopback opens a FRESH full-duplex loopback to the gateway's own
// /v1/audio/transcriptions/live for this turn (the openLiveLoopback pattern, reused as-is
// so the dial -- Expect: 100-continue, trusted-loopback headers, chunked io.Pipe body --
// never drifts from the live path). It feeds the sealed ASR model name + PCM shape, which
// /v1 re-resolves, re-gates, admits and bills per turn. On success it starts the turn's SSE
// relay goroutine and returns true; on a dial failure or a non-2xx refusal it surfaces the
// error and returns false.
func (m *voiceMachine) openTurnLoopback() bool {
	pr, pw := io.Pipe()
	turnCtx, turnCancel := context.WithCancel(m.ctx)
	liveClaims := liveTicketClaims{
		Target:     routing.Target{Model: m.claims.ASR.Model},
		SampleRate: m.claims.SampleRate,
		Channels:   m.claims.Channels,
		Format:     m.claims.SampleFormat,
		UserID:     m.claims.UserID,
	}
	resp, err := m.s.openLiveLoopback(turnCtx, liveClaims, pr)
	if err != nil {
		turnCancel()
		_ = pr.CloseWithError(err)
		_ = pw.CloseWithError(err)
		if m.ctx.Err() == nil {
			m.failTurn("transcription.unavailable", "live transcription unavailable")
		}
		return false
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		frame := liveRefusalFrame(resp.Body)
		_ = resp.Body.Close()
		turnCancel()
		_ = pr.CloseWithError(errVoiceTurnDone)
		_ = pw.CloseWithError(errVoiceTurnDone)
		m.emitError(frame.Code, frame.Message)
		m.setState(voiceStateIdle)
		return false
	}
	asrCh := make(chan asrEvent, voiceASRQueue)
	m.pw, m.turnCancel, m.respBody, m.asrCh = pw, turnCancel, resp.Body, asrCh
	m.relayWG.Add(1)
	go func() {
		defer m.relayWG.Done()
		voiceASRRelay(turnCtx, resp.Body, asrCh)
	}()
	return true
}

// onControl handles a push-to-talk / cancel control from the client. bye never reaches
// here (the read loop closes on it).
func (m *voiceMachine) onControl(ctl voiceUpControl) {
	switch ctl.Type {
	case voiceUpPushStart:
		// Begin listening from a rest/idle state. push_start never re-opens the mic during
		// thinking/speaking/draining -- the half-duplex gate is server-authoritative, so a
		// client control cannot force the mic open while the answer plays; and it does NOT
		// reset an in-progress listening chunk.
		if m.state == voiceStateIdle {
			m.setState(voiceStateListening)
		}
	case voiceUpPushEnd:
		// Explicit end-of-utterance: end the current chunk immediately (bypassing the
		// min-utterance guard), but only when a voiced chunk is actually in flight.
		if m.state == voiceStateListening && m.chunkActive && !m.finalizing {
			m.finalizeChunk()
		}
	case voiceUpCancel:
		// Abandon the in-flight turn and return to listening. Task 7 extends this to stop
		// the speculative run / TTS; here it tears the ASR loopback down and resets the
		// chunk. pendingText is left to Task 7's turn/commit semantics.
		m.teardownTurn()
		m.resetChunk()
		m.stopChunkTimers()
		m.stopTail()
		m.setState(voiceStateListening)
	}
}

// onASREvent folds one live-ASR event into the current chunk. delta/partial build the
// provisional WHOLE transcript streamed down as asr_partial (replace-not-append); done is
// the clean final that ends the chunk; an error fails the turn.
func (m *voiceMachine) onASREvent(ev asrEvent) {
	switch ev.kind {
	case asrKindDelta:
		m.chunkText += ev.text
		m.onVoiced()
	case asrKindPartial:
		m.chunkText = ev.text
		m.onVoiced()
	case asrKindDone:
		m.finishChunk(firstNonEmptyVoice(ev.text, m.chunkText))
	case asrKindError:
		m.failTurn(ev.code, firstNonEmptyVoice(ev.message, "live transcription error"))
	}
}

// onVoiced records a voiced partial: it latches the chunk (arming the max-utterance cap),
// advances the voiced-span bound, resets the silence timer and streams the provisional
// transcript down. An empty provisional is not treated as voiced.
func (m *voiceMachine) onVoiced() {
	if strings.TrimSpace(m.chunkText) == "" {
		return
	}
	now := time.Now()
	if !m.chunkActive {
		m.chunkActive = true
		m.firstPartialAt = now
		m.armMax()
	}
	m.lastPartialAt = now
	m.armSilence()
	m.writer.send(m.ctx, voiceDownMsg{Type: voiceDownASRPartial, Text: m.chunkText})
}

// onSilence fires VoiceSilenceTimeout after the last voiced partial: it ends the chunk,
// unless the voiced span is below the min-utterance guard, in which case the blip is
// ignored (no chunk end) and the next voiced partial re-arms the timer.
func (m *voiceMachine) onSilence() {
	if m.state != voiceStateListening || !m.chunkActive || m.finalizing {
		return
	}
	if m.lastPartialAt.Sub(m.firstPartialAt) < m.timing.minUtterance {
		m.stopSilence() // a sub-min blip: keep listening, do not end the chunk
		return
	}
	m.finalizeChunk()
}

// onMaxUtterance fires at the hard cap: it ends the chunk regardless of the min guard.
func (m *voiceMachine) onMaxUtterance() {
	if m.state != voiceStateListening || !m.chunkActive || m.finalizing {
		return
	}
	m.finalizeChunk()
}

// finalizeChunk ends the current chunk: it closes the loopback pipe cleanly (end-of-input
// -> the upstream's terminating chunk -> the clean transcript.text.done), stops the chunk
// timers and arms the finalize-timeout fallback. The machine stays in listening with
// finalizing set (so inbound PCM is gated) until the done arrives (finishChunk) or the
// fallback fires.
func (m *voiceMachine) finalizeChunk() {
	m.finalizing = true
	m.stopChunkTimers()
	if m.pw != nil {
		_ = m.pw.Close() // clean EOF -> terminating chunk -> transcript.text.done
	}
	m.armFinalize()
}

// finishChunk completes a chunk once its clean final arrives: it emits asr_final, APPENDS
// the final to pendingText (the accumulating turn text), tears the per-turn loopback down,
// and begins the (placeholder) answer so the session walks the speak/drain states and
// returns to listening.
func (m *voiceMachine) finishChunk(final string) {
	m.stopFinalize()
	m.writer.send(m.ctx, voiceDownMsg{Type: voiceDownASRFinal, Text: final})
	m.pendingText += final
	if m.s.voiceOnPending != nil {
		m.s.voiceOnPending(m.pendingText)
	}
	m.teardownTurn()
	m.resetChunk()
	m.beginAnswerPlaceholder()
}

// onASRClosed handles the turn relay ending (the SSE body closed). If we were finalizing,
// the clean done never came through -- finish with the provisional text so the turn is not
// wedged. Otherwise the ASR dropped mid-chunk, which is a recoverable leg error.
func (m *voiceMachine) onASRClosed() {
	if m.finalizing {
		m.finishChunk(m.chunkText)
		return
	}
	if m.state == voiceStateListening && m.asrCh != nil {
		m.failTurn("transcription.stream_error", "the transcription stream ended")
		return
	}
	m.asrCh = nil
}

// onFinalizeTimeout fires when the clean final did not arrive in time after end-of-input:
// finalize with the provisional text rather than wedge the turn.
func (m *voiceMachine) onFinalizeTimeout() {
	if !m.finalizing {
		return
	}
	m.finishChunk(m.chunkText)
}

// beginAnswerPlaceholder walks the turn's answer states. TODO(Task 7): this is the seam
// where the speculative voice run + continuation window attaches -- start the LLM on
// pendingText, keep the mic live during the grace (VoiceContinuationGrace), cancel-and-
// extend on resumed speech, then commit (persist the user+assistant turn, clear
// pendingText) and stream the answer's TTS as audio_delta. This task has no run or TTS, so
// it walks thinking -> speaking -> draining as placeholders (the half-duplex mic gate holds
// across all three) and arms the real mic-resume tail; pendingText is intentionally NOT
// cleared here (Task 7's commit clears it), so chunks accumulate across the turn.
func (m *voiceMachine) beginAnswerPlaceholder() {
	m.setState(voiceStateThinking)
	m.setState(voiceStateSpeaking)
	m.setState(voiceStateDraining)
	m.armTail()
}

// onTailDone fires when the mic-resume tail elapses: the half-duplex window is over, so the
// session returns to listening for the next turn (its loopback opens lazily on the next
// PCM frame).
func (m *voiceMachine) onTailDone() {
	m.stopTail()
	m.setState(voiceStateListening)
}

// failTurn surfaces a recoverable leg error (spec §6.6): emit the typed error, tear the
// turn down and return to idle with the mic no longer piping -- no silent retry that could
// spin into a loop. The client re-engages with push_start.
func (m *voiceMachine) failTurn(code, message string) {
	m.emitError(code, message)
	m.teardownTurn()
	m.resetChunk()
	m.stopChunkTimers()
	m.stopTail()
	m.setState(voiceStateIdle)
}

// emitError sends a typed error down-frame (best-effort; dropped if the session is already
// tearing down).
func (m *voiceMachine) emitError(code, message string) {
	m.writer.send(m.ctx, voiceDownMsg{Type: voiceDownError, Code: code, Message: message})
}

// teardownTurn ends the current ASR loopback exactly once and joins its relay goroutine:
// cancel the turn context (aborts the loopback request and the SSE read), unblock the pipe
// from both sides, close the response body, and wait for the relay to return. It is safe to
// call when no turn is open.
func (m *voiceMachine) teardownTurn() {
	if m.turnCancel != nil {
		m.turnCancel()
	}
	if m.pw != nil {
		_ = m.pw.CloseWithError(errVoiceTurnDone)
	}
	if m.respBody != nil {
		_ = m.respBody.Close()
	}
	m.relayWG.Wait()
	m.pw, m.turnCancel, m.respBody, m.asrCh = nil, nil, nil, nil
	m.finalizing = false
}

// resetChunk clears the per-chunk transcript and voiced-span bounds for the next chunk.
func (m *voiceMachine) resetChunk() {
	m.chunkText = ""
	m.chunkActive = false
	m.firstPartialAt = time.Time{}
	m.lastPartialAt = time.Time{}
}

// armSilence (re)starts the silence timer; armMax/armTail/armFinalize start theirs.
// Re-arming replaces the channel the select reads, so a stale fire from the old timer is
// never selected. All timer fields are touched only by the machine goroutine.
func (m *voiceMachine) armSilence() {
	if m.silenceTimer != nil {
		m.silenceTimer.Stop()
	}
	m.silenceTimer = time.NewTimer(m.timing.silenceTimeout)
	m.silenceC = m.silenceTimer.C
}

func (m *voiceMachine) armMax() {
	if m.maxTimer != nil {
		m.maxTimer.Stop()
	}
	m.maxTimer = time.NewTimer(m.timing.maxUtterance)
	m.maxC = m.maxTimer.C
}

func (m *voiceMachine) armTail() {
	if m.tailTimer != nil {
		m.tailTimer.Stop()
	}
	m.tailTimer = time.NewTimer(m.timing.micResumeTail)
	m.tailC = m.tailTimer.C
}

func (m *voiceMachine) armFinalize() {
	if m.finalizeTimer != nil {
		m.finalizeTimer.Stop()
	}
	m.finalizeTimer = time.NewTimer(voiceFinalizeTimeout)
	m.finalizeC = m.finalizeTimer.C
}

func (m *voiceMachine) stopSilence() {
	if m.silenceTimer != nil {
		m.silenceTimer.Stop()
		m.silenceTimer = nil
	}
	m.silenceC = nil
}

func (m *voiceMachine) stopMax() {
	if m.maxTimer != nil {
		m.maxTimer.Stop()
		m.maxTimer = nil
	}
	m.maxC = nil
}

func (m *voiceMachine) stopTail() {
	if m.tailTimer != nil {
		m.tailTimer.Stop()
		m.tailTimer = nil
	}
	m.tailC = nil
}

func (m *voiceMachine) stopFinalize() {
	if m.finalizeTimer != nil {
		m.finalizeTimer.Stop()
		m.finalizeTimer = nil
	}
	m.finalizeC = nil
}

// stopChunkTimers disarms the silence and max-utterance timers (a chunk ended).
func (m *voiceMachine) stopChunkTimers() {
	m.stopSilence()
	m.stopMax()
}

// stopAllTimers disarms every turn timer (teardown).
func (m *voiceMachine) stopAllTimers() {
	m.stopChunkTimers()
	m.stopTail()
	m.stopFinalize()
}

// firstNonEmptyVoice returns a when it has non-space content, else b. It picks the clean
// final over the provisional (and a message over a fallback) without letting an empty one
// win.
func firstNonEmptyVoice(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}

// voiceSocket is the narrow slice of *websocket.Conn the writer drives. It exists so the
// writer's branches (a failed write, a failed ping, a clean versus an abrupt close) are
// testable without a network; *websocket.Conn satisfies it.
type voiceSocket interface {
	Write(ctx context.Context, typ websocket.MessageType, p []byte) error
	Ping(ctx context.Context) error
	Close(code websocket.StatusCode, reason string) error
	CloseNow() error
}

var _ voiceSocket = (*websocket.Conn)(nil)

// voiceWriter is a voice session's sole WebSocket writer, a mirror of liveWriter: exactly
// one goroutine (run) drains its queue AND owns the keepalive ping AND performs the
// connection's close, so no two writes to the connection ever race, the "genau EIN Writer
// pro WS-Verbindung" discipline agentStreamConn.runWriter documents. It is a mirror, not
// a shared type, so the live-transcription path stays untouched; it differs by the
// voiceSocket seam and by closeNormally, which a live session never needs (its hot path
// ends by an abrupt close after the data is flushed).
type voiceWriter struct {
	conn   voiceSocket
	cancel context.CancelFunc
	out    chan []byte
	// normalClose makes run end the connection with a close handshake (1000) instead of
	// the abrupt close; set by closeNormally before it cancels the context.
	normalClose atomic.Bool

	pingInterval time.Duration
	pingTimeout  time.Duration
	writeTimeout time.Duration
}

func newVoiceWriter(conn voiceSocket, cancel context.CancelFunc) *voiceWriter {
	return &voiceWriter{
		conn:         conn,
		cancel:       cancel,
		out:          make(chan []byte, voiceOutboundQueue),
		pingInterval: agentStreamPingInterval,
		pingTimeout:  agentStreamPingTimeout,
		writeTimeout: agentStreamWriteTimeout,
	}
}

// run is the sole writer loop. It returns:
//   - on CONTEXT CANCEL (the one teardown signal, from a bye, a client drop or a reap
//     from within this loop): it flushes any queued frames best-effort, then closes the
//     connection, which also unblocks the read loop's conn.Read. The close is abrupt
//     (CloseNow) unless closeNormally asked for the handshake: the handshake needs the
//     read mutex, which a read loop still blocked in conn.Read holds, so it would stall
//     for seconds on every session that does not end with a bye;
//   - on the first write or ping failure: it reaps the connection.
//
// Every exit cancels the handler context, so a dead peer tears the whole relay down.
func (w *voiceWriter) run(ctx context.Context) {
	ticker := time.NewTicker(w.pingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			w.drain()
			w.closeConn()
			return
		case data := <-w.out:
			if !w.write(data) {
				w.cancel()
				_ = w.conn.CloseNow()
				return
			}
		case <-ticker.C:
			pctx, pcancel := context.WithTimeout(context.Background(), w.pingTimeout)
			err := w.conn.Ping(pctx)
			pcancel()
			if err != nil {
				w.cancel()
				_ = w.conn.CloseNow()
				return
			}
		}
	}
}

// closeConn ends the connection after the context was cancelled: a close handshake when
// closeNormally asked for one, an abrupt close otherwise.
func (w *voiceWriter) closeConn() {
	if w.normalClose.Load() {
		_ = w.conn.Close(websocket.StatusNormalClosure, "")
		return
	}
	_ = w.conn.CloseNow()
}

// closeNormally ends the session cleanly: the writer flushes what is queued and closes
// the connection with a 1000 handshake. It is for an exit where the read loop has already
// stopped reading (bye), so the handshake can take the read mutex. It cancels the handler
// context, which is what makes run act.
func (w *voiceWriter) closeNormally() {
	w.normalClose.Store(true)
	w.cancel()
}

// write sends one data frame with a per-write timeout; false means the write failed (a
// dead or half-open peer) and the caller must reap.
func (w *voiceWriter) write(data []byte) bool {
	if len(data) == 0 {
		return true
	}
	wctx, wcancel := context.WithTimeout(context.Background(), w.writeTimeout)
	err := w.conn.Write(wctx, websocket.MessageText, data)
	wcancel()
	if err != nil {
		slog.Debug("voice chat: down-write failed, reaping", "err", err)
		return false
	}
	return true
}

// drain writes every frame already queued, non-blocking, stopping on the first write
// failure or an empty queue. It is how teardown delivers the last frames (a final state
// or an error enqueued just before the context was cancelled) before the close.
func (w *voiceWriter) drain() {
	for {
		select {
		case data := <-w.out:
			if !w.write(data) {
				return
			}
		default:
			return
		}
	}
}

// send marshals msg and enqueues it, blocking only until the writer takes it or the
// handler context ends (so it never outlives teardown, and never drops a frame while the
// session is live).
func (w *voiceWriter) send(ctx context.Context, msg voiceDownMsg) {
	data := voiceMarshal(msg)
	if len(data) == 0 {
		return
	}
	select {
	case w.out <- data:
	case <-ctx.Done():
	}
}
