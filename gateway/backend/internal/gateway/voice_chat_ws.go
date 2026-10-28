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
	"op-ai-gateway/internal/auth"
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
	// voiceUpPlaybackDone: the client's audio player finished playing the answer out (its
	// StreamingPcmPlayer drained). It is what lets the server end `draining` at true
	// playout-done rather than relay-done (spec 6.2); the server still owns the gate and
	// applies its own fallback, so a missing or late signal cannot wedge the session.
	voiceUpPlaybackDone = "playback_done"
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
//     It is the continuation window (the mic stays open and the TTS audio is held until
//     it closes), which the state machine's beginAnswer arms on each chunk end.
const (
	VoiceSilenceTimeoutMs    = 900
	VoiceMinUtteranceMs      = 300
	VoiceMaxUtteranceMs      = 30000
	VoiceMicResumeTailMs     = 300
	VoiceContinuationGraceMs = 1500
)

// VoicePlaybackFallbackMs is the drain's ABSOLUTE upper bound (spec 6.2): the most the session
// will stay in draining before it re-opens the mic, even if no playback_done ever arrives, so a
// missing or pathologically-late signal can never wedge the session. It is deliberately
// generous because it is only the ceiling -- the normal drain ends at the relayed audio's own
// real-time duration (the floor) or at the client's playback_done, whichever is later. It is
// also the fallback when the relayed audio's duration cannot be computed (a chunk with no
// usable rate -- defensive, since the streamed TTS always carries one).
const VoicePlaybackFallbackMs = 30000

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
// unexported twin of VoiceTiming so the machine's fields stay internal. playbackCap is the
// drain's absolute upper bound (the VoicePlaybackFallbackMs const); it is deliberately NOT in
// the exported VoiceTiming -- it is not an operator knob, only an internal default and a test
// seam -- and a machine reads it through playbackCapOrDefault, so a zero value means the const.
type voiceTiming struct {
	silenceTimeout    time.Duration
	minUtterance      time.Duration
	maxUtterance      time.Duration
	micResumeTail     time.Duration
	continuationGrace time.Duration
	playbackCap       time.Duration
}

// defaultVoiceTiming is the built-in timing (the Voice*Ms defaults).
func defaultVoiceTiming() voiceTiming {
	return voiceTiming{
		silenceTimeout:    VoiceSilenceTimeoutMs * time.Millisecond,
		minUtterance:      VoiceMinUtteranceMs * time.Millisecond,
		maxUtterance:      VoiceMaxUtteranceMs * time.Millisecond,
		micResumeTail:     VoiceMicResumeTailMs * time.Millisecond,
		continuationGrace: VoiceContinuationGraceMs * time.Millisecond,
		playbackCap:       VoicePlaybackFallbackMs * time.Millisecond,
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
// frame whose Type is one of push_start, push_end, cancel, bye or playback_done.
//
// PlayedThrough rides a playback_done only: the client's monotonic count of the audio chunks it
// has RECEIVED for the turn (it sends one on every audio-queue drain). The server honors a
// playback_done as the end of audio only when this count accounts for all it has relayed, so a
// stale gap signal that lost the relay/signal race cannot end the drain while later audio is still
// playing (#322). It is additive and OPTIONAL: an older client omits it (a nil pointer), and the
// server then falls back to the remembered-signal path so a non-reordering client is never
// regressed.
type voiceUpControl struct {
	Type          string `json:"type"`
	PlayedThrough *int   `json:"played_through,omitempty"`
}

// parseVoiceUpControl decodes a text frame into a control message. It reports false for
// anything that is not a JSON object naming one of the known controls: a malformed frame, an
// unknown or differently-cased type (including live transcription's "end"), a non-string
// type. Unknown extra fields are tolerated, so a newer client can add detail to a control
// without breaking an older gateway. The caller ignores a frame that does not parse: a
// control is advice to the state machine, never something a session can be wedged by.
func parseVoiceUpControl(data []byte) (voiceUpControl, bool) {
	var c voiceUpControl
	if err := json.Unmarshal(data, &c); err != nil {
		return voiceUpControl{}, false
	}
	switch c.Type {
	case voiceUpPushStart, voiceUpPushEnd, voiceUpCancel, voiceUpBye, voiceUpPlaybackDone:
		return c, true
	}
	return voiceUpControl{}, false
}

// voiceDownMsg is the typed envelope the gateway sends down to the browser over the
// WebSocket; the browser switches on Type. One flat struct with a field per shape keeps
// the wire minimal: a field a frame does not own is zero and omitted.
//
//   - state:        State
//   - asr_partial:  Text (the CURRENT chunk's provisional text, replace-not-append; not the
//     whole turn -- across a pause-tolerant, multi-chunk turn the client shows the finalized
//     chunks so far plus this partial)
//   - asr_final:    Text (the finalized text of ONE chunk, which the client appends to the
//     turn's text; nothing is persisted at a chunk boundary)
//   - answer_delta: Text (the increment)
//   - answer_done:  nothing (provisional: sent when the answer text is complete, BEFORE the
//     commit, so a later answer_reset can still void it)
//   - answer_reset: nothing (the speculative run was cancelled because the user resumed)
//   - audio_delta:  Seq (from 0), Rate (Hz) and Audio (base64 s16le PCM)
//   - audio_done:   nothing (the audio is complete and the turn has been committed; the client
//     then reloads the chat thread to pick up the stored user and assistant turns)
//   - error:        Code (the stable code of the leg that failed) and Message
//
// No frame carries a message id. The user turn and the assistant turn are persisted together,
// once, when the turn truly finalizes (the voice run's deferred commit), so nothing exists to
// name earlier; MessageID is therefore never populated. It stays on the struct, omitempty, as
// reserved wire shape the portal's parser tolerates, and it never appears on the wire today.
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
	asrKindPartial                     // the current chunk's provisional transcript (replace-not-append)
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
// On a chunk end the machine starts a SPECULATIVE voice run (startVoiceRun) on the
// accumulated pendingText and subscribes to it, folding the run's text/audio deltas into
// the session as answer_*/audio_* down-frames. While the run is in flight and before any
// TTS audio plays -- the continuation window (spec 6.1) -- the mic STAYS OPEN (micOpen is
// true in thinking too), so resumed speech is captured: a resumed voiced chunk cancels the
// speculative run (nothing is persisted), emits answer_reset, extends pendingText and
// re-runs on the next chunk end. The window closes -- the mic shuts and the audio plays --
// only once the answer has streamed AND the continuation grace has elapsed with no resume;
// then the run commits (user + assistant together) and the session speaks/drains. Any leg
// failure degrades to idle with one error frame (spec 6.6), no retry.
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

	// The in-flight speculative voice run and its subscription. run/runUnsub are nil when
	// no run is open; runCh is nil whenever the run's events must not be selected (no run,
	// or after the run went terminal), which disables its select case. The machine is the
	// SOLE reader of runCh and the sole mutator of every field below, so the run's events
	// reach the state machine only as channel values folded in run()'s single select --
	// never a cross-goroutine write of machine state.
	curRun   *ChatRun
	runCh    <-chan runEvent
	runUnsub func()

	// The continuation-window bookkeeping for the current speculative run.
	//   - answerStreamed: the first audio event (the summary spoken at </speak>, in the
	//     concurrent flow while the detail text is still streaming), or a completed terminal --
	//     one of the two window-close conditions.
	//   - answerDoneSent: answer_done already emitted (emitted once, on that first audio --
	//     before any audio_delta, and typically before the detail text has finished; the client
	//     ignores answer_done's terminal flag).
	//   - graceElapsed: the continuation grace has fired -- the other window-close condition.
	//   - windowClosed: the window is closed (we have committed to this turn): the mic is
	//     shut and buffered audio plays. Set exactly when the machine leaves thinking for
	//     speaking; no resume is accepted past it.
	//   - runFinished: the run has reported its single terminal outcome.
	//   - audioBuf: audio chunks held while the window is still open (before speaking), so
	//     no TTS audio is ever delivered -- and so could never play while the mic is open --
	//     until the window closes. Flushed in order when the window closes.
	answerStreamed bool
	answerDoneSent bool
	graceElapsed   bool
	windowClosed   bool
	runFinished    bool
	// answerFinished guards finishAnswer against a double invocation within one turn (the
	// window can close and the run can finish along two paths). Reset per turn in beginAnswer,
	// never in detachRun, so it holds for the whole turn.
	answerFinished bool
	audioBuf       []runAudio

	// The continuation-grace timer (armed on each chunk end, disarmed on resume/commit).
	graceTimer *time.Timer
	graceC     <-chan time.Time

	// The playout-aware drain bookkeeping (spec 6.2). The orchestrator relays TTS faster than
	// real time, so audio_done (relay-done) is NOT "the user stopped hearing it." After
	// audio_done the session stays in draining -- the mic gated -- until playout-done, which the
	// server computes as max(FLOOR, min(CEILING, the client's playback_done)).
	// The FLOOR = firstAudioAt + relayedPlaybackDur is the relayed audio's own real-time
	// duration measured from when playout would have started (the first chunk relayed); it is
	// the SERVER-AUTHORITATIVE minimum, so the mic NEVER re-opens before it, however early a
	// (buggy or malicious) client claims playout finished. A playback_done before the floor is
	// clamped UP to the floor; one after the floor (a slow client still playing) EXTENDS the
	// drain to the signal; with no signal the drain waits out the CEILING (playbackCapOrDefault),
	// the no-signal upper bound that also caps how long a late signal can hold it. A floor that
	// is itself longer than the ceiling (a very long answer) exits at the floor.
	//   - firstAudioAt/relayedPlaybackDur accumulate as audio is relayed; playbackRateUnknown
	//     drops the computed floor (the ceiling is then the only bound) for a chunk with no
	//     usable rate (defensive).
	//   - relayedAudioChunks is the count of audio_delta frames relayed this turn; it is the
	//     server half of the #322 playout counter. A playback_done carries the client's own count
	//     of audio RECEIVED (voiceUpControl.PlayedThrough); the server honors it as the end of
	//     audio only when that count is >= relayedAudioChunks (it accounts for everything relayed).
	//     A stale gap signal (count below) is not the end of audio and does not end the drain. This
	//     closes the race the clear-on-relay heuristic alone cannot -- a stale signal that arrives
	//     AFTER the last chunk was relayed, during the drain, when no further relay happens to
	//     clear it.
	//   - awaitingPlayback is set for the whole wait; playbackSignaled records that a VALID
	//     playback_done (one whose counter accounts for all relayed audio, or a counter-less signal
	//     from an older client) has been seen with no chunk relayed since -- it is cleared per turn
	//     (beginAnswer) AND on every relayed chunk (relayAudio, belt-and-suspenders for the
	//     counter-less path), so only a signal after the last chunk, the genuine end of audio,
	//     survives to end the drain; playbackFloorPassed latches when the floor elapses (then the
	//     timer is re-armed to the ceiling to wait out a slow or absent signal).
	// All are reset per turn in beginAnswer. The mic STAYS discarded for the whole draining
	// period regardless (micOpen); only the EXIT moment moves from relay-done to playout-done.
	firstAudioAt        time.Time
	relayedPlaybackDur  time.Duration
	relayedAudioChunks  int
	playbackRateUnknown bool
	awaitingPlayback    bool
	playbackSignaled    bool
	playbackFloorPassed bool
	playbackTimer       *time.Timer
	playbackC           <-chan time.Time
}

// voiceFinalizeTimeout bounds the wait for the clean final transcript after the pipe is
// closed on a chunk end: audio.cpp emits transcript.text.done then [DONE] on end-of-input,
// but a stalled or truncated upstream must not wedge the turn, so after this the machine
// finalizes with whatever provisional text it has.
const voiceFinalizeTimeout = 5 * time.Second

// The gateway-side stable codes for a voice turn that failed before (or without) any core
// returning its own code. A leg that failed WITH a code (an LLM/TTS error, a mid-turn
// runtime.admission_blocked) degrades on THAT code; these are the fallbacks for the thin
// orchestration failures the cores never saw.
const (
	// voiceRunStartFailedCode: the speculative run could not be reserved or launched.
	voiceRunStartFailedCode = "gateway.voice_run_start_failed"
	// voiceRunClosedCode: the run's subscription ended with no terminal event (defensive).
	voiceRunClosedCode = "gateway.voice_run_closed"
	// voiceRunFailedCode: the run reported an error status but carried no code to surface.
	voiceRunFailedCode = "gateway.voice_run_failed"
)

// voiceTurnFailedMessage is the human half of every degradation error frame; the stable
// code is the machine-readable half the client switches on.
const voiceTurnFailedMessage = "the voice turn could not be completed"

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
			m.detachRun()
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
		case ev, ok := <-m.runCh:
			if ok {
				m.onRunEvent(ev)
			} else {
				m.onRunClosed()
			}
		case <-m.silenceC:
			m.onSilence()
		case <-m.maxC:
			m.onMaxUtterance()
		case <-m.graceC:
			m.onGrace()
		case <-m.playbackC:
			m.onPlaybackTimer()
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

// micOpen reports whether inbound PCM is piped to the ASR this turn. The mic feeds the ASR
// in listening AND in thinking while the continuation window is open (spec 6.1) -- during
// that window no TTS audio has played yet (audio is held, audioBuf), so capturing resumed
// speech cannot feed the model its own voice. It is CLOSED in idle, in speaking and in
// draining, once the window has closed (windowClosed), and while finalizing a chunk (the
// pipe is closing). The speaking/draining closure is the whole half-duplex guarantee:
// once any TTS audio can play, the mic can never route it into the ASR, enforced
// server-side regardless of client behaviour.
func (m *voiceMachine) micOpen() bool {
	if m.finalizing {
		return false
	}
	switch m.state {
	case voiceStateListening:
		return true
	case voiceStateThinking:
		// The continuation window: open until we commit to the answer (windowClosed), at
		// which point the state moves to speaking anyway.
		return !m.windowClosed
	default:
		return false // idle, speaking, draining
	}
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
		// Abandon the in-flight turn and return to listening (spec 5.3): cancel the
		// speculative run (nothing persisted), tear the ASR loopback down, drop the pending
		// text (the turn is abandoned, never silently re-run) and disarm every turn timer.
		m.detachRun()
		m.teardownTurn()
		m.resetChunk()
		m.stopChunkTimers()
		m.stopGrace()
		m.stopTail()
		m.resetPlaybackWait()
		m.pendingText = ""
		m.setState(voiceStateListening)
	case voiceUpPlaybackDone:
		// The client's audio player reported its queue drained. In the concurrent flow this can
		// arrive while still speaking (the summary drained before the detail committed) or in a
		// gap between chunks, so a VALID signal is REMEMBERED, not dropped -- only the genuine
		// end-of-audio signal survives to end the drain (spec 6.2). The playout counter it carries
		// (ctl.PlayedThrough) is what tells a genuine end from a stale gap signal; it still never
		// shortens the drain below the floor.
		m.onPlaybackDone(ctl.PlayedThrough)
	}
}

// onASREvent folds one live-ASR event into the current chunk. delta/partial build the
// current chunk's provisional transcript, streamed down as asr_partial (replace-not-append;
// it is this chunk only, the client adds the finalized chunks of earlier pauses); done is
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
		// The first voiced partial of a chunk that begins while the speculative run is still
		// cancel-and-extendable (tryResume: the cancel CAS beats the run's commit CAS) is
		// "resumed speech": cancel the run (nothing persisted), drop the answer preview, and
		// fall back to listening so this chunk extends pendingText and re-runs on the next chunk
		// end (spec 6.1). Once the run has passed its point of no return -- committing or
		// committed -- the resume is NOT accepted: this voiced input stays an in-progress
		// chunk, which the run's terminal force-close (closeWindow -> teardownTurn ->
		// resetChunk) then DISCARDS. On that narrow fast-answer edge the resumed utterance is
		// dropped, which keeps the turn single-committed and the state consistent (true
		// barge-in is deferred to phase 4).
		if m.tryResume() {
			m.onResume()
		}
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
// and begins the answer (beginAnswer): a speculative voice run on the accumulated text and
// the continuation window, which ends in the speak/drain states and a return to listening.
func (m *voiceMachine) finishChunk(final string) {
	m.stopFinalize()
	m.writer.send(m.ctx, voiceDownMsg{Type: voiceDownASRFinal, Text: final})
	m.pendingText += final
	if m.s.voiceOnPending != nil {
		m.s.voiceOnPending(m.pendingText)
	}
	m.teardownTurn()
	m.resetChunk()
	m.beginAnswer()
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

// beginAnswer starts the speculative voice run for the accumulated pendingText and opens
// the continuation window (spec 6.1). A chunk that produced no text is NOT a turn: it does
// not start a run and does not walk the answer states (this also swallows a blip-then-
// silence spurious turn), the session simply keeps listening. Otherwise the run is started
// and subscribed, the machine enters thinking (mic still open -- the window), the grace
// timer is armed, and any answer text the run already buffered before we subscribed is
// relayed so no delta is lost.
func (m *voiceMachine) beginAnswer() {
	if strings.TrimSpace(m.pendingText) == "" {
		m.pendingText = ""
		m.setState(voiceStateListening)
		return
	}
	owner := auth.Token{
		UserID:    m.claims.UserID,
		ID:        m.claims.TokenID,
		ServiceID: m.claims.ServiceID,
		ProjectID: m.claims.ProjectID,
	}
	run, err := m.s.startVoiceRun(m.ctx, voiceRunRequest{
		owner:       owner,
		chatID:      m.claims.ChatID,
		pendingText: m.pendingText,
		asr:         m.claims.ASR,
		llm:         m.claims.LLM,
		tts:         m.claims.TTS,
		audio:       m.claims.Audio,
	})
	if err != nil {
		// The run could not even be reserved/launched (no slot, a history read failure):
		// degrade exactly as a leg error would, no retry.
		m.degrade(voiceRunStartFailedCode)
		return
	}
	snap, ch, unsub := run.subscribe()
	m.curRun, m.runCh, m.runUnsub = run, ch, unsub
	m.answerStreamed, m.answerDoneSent, m.graceElapsed, m.windowClosed, m.runFinished = false, false, false, false, false
	m.answerFinished = false
	m.audioBuf = nil
	// The playout-drain accounting is per turn: no audio relayed yet, no drain wait pending.
	m.firstAudioAt, m.relayedPlaybackDur, m.playbackRateUnknown = time.Time{}, 0, false
	m.relayedAudioChunks = 0 // the #322 playout counter starts fresh each turn (audio numbers from 0)
	m.resetPlaybackWait()    // clears awaitingPlayback/playbackSignaled/playbackFloorPassed + the timer
	m.setState(voiceStateThinking)
	m.armGrace()
	// Relay whatever the run buffered between launch and subscribe (usually nothing); the
	// snapshot carries no audio, which only ever arrives after the text, long after here.
	if snap.Content != "" {
		m.writer.send(m.ctx, voiceDownMsg{Type: voiceDownAnswerDelta, Text: snap.Content})
	}
	// Defensive: a run that was already terminal at subscribe (it cannot be in practice --
	// the LLM leg is a loopback round trip -- but the snapshot is the source of truth).
	if snap.Status != "running" {
		m.handleRunTerminal(snap.Status, snap.Err)
	}
}

// onRunEvent folds one event from the speculative run's subscription into the session. A
// text delta becomes answer_delta; an audio chunk is held or relayed by the window logic;
// the terminal `done` drives the commit/degrade decision. The snapshot event never arrives
// here (beginAnswer consumed it).
func (m *voiceMachine) onRunEvent(ev runEvent) {
	switch ev.Event {
	case "delta":
		if ev.Content != "" {
			m.writer.send(m.ctx, voiceDownMsg{Type: voiceDownAnswerDelta, Text: ev.Content})
		}
	case "audio":
		m.onRunAudio(ev.Audio)
	case "done":
		m.handleRunTerminal(ev.Status, ev.Err)
	}
}

// onRunClosed handles the run's subscription channel closing (the run is terminal). The
// terminal `done` event always precedes the close, so by here handleRunTerminal has run;
// we only disable the select case. A close with no prior terminal is a defensive failure.
func (m *voiceMachine) onRunClosed() {
	if m.curRun != nil && !m.runFinished {
		m.degrade(voiceRunClosedCode)
		return
	}
	m.runCh = nil
}

// onRunAudio consumes one TTS audio chunk from the run. In the concurrent flow the first audio
// is the SUMMARY spoken at </speak> while the LLM detail is still streaming, so answer_done is
// emitted now (markAnswerStreamed) even though more answer text follows -- the client ignores
// answer_done's terminal flag. While the continuation window is still open the chunk is HELD
// (audioBuf) so no audio ever plays before the mic shuts; once the window has closed the chunk
// is relayed straight down.
func (m *voiceMachine) onRunAudio(a *runAudio) {
	if a == nil {
		return
	}
	m.markAnswerStreamed()
	if m.windowClosed {
		m.relayAudio(*a)
		return
	}
	m.audioBuf = append(m.audioBuf, *a)
	m.maybeCloseWindow()
}

// markAnswerStreamed records the first audio event (the summary spoken at </speak>, which in the
// concurrent flow arrives while the LLM detail is still streaming) or a completed run, and emits
// answer_done exactly once, before any audio_delta. answer_done thus fires while more answer text
// may still follow -- harmless, as the client ignores its terminal flag. It is one of the two
// conditions that close the continuation window.
func (m *voiceMachine) markAnswerStreamed() {
	if !m.answerDoneSent {
		m.answerDoneSent = true
		m.writer.send(m.ctx, voiceDownMsg{Type: voiceDownAnswerDone})
	}
	if m.answerStreamed {
		return
	}
	m.answerStreamed = true
	m.maybeCloseWindow()
}

// onGrace fires when the continuation grace elapses: it is the other window-close
// condition. Because the LLM runs during the grace, the grace is usually already hidden
// behind generation -- this only matters when the answer streamed faster than the grace.
func (m *voiceMachine) onGrace() {
	m.stopGrace()
	m.graceElapsed = true
	m.maybeCloseWindow()
}

// maybeCloseWindow closes the continuation window once the answer has streamed AND the
// grace has elapsed with no resume. Idempotent and cheap to call from either condition.
func (m *voiceMachine) maybeCloseWindow() {
	if m.windowClosed || !m.answerStreamed || !m.graceElapsed {
		return
	}
	m.closeWindow()
}

// closeWindow commits the session to the current turn: the mic shuts (half-duplex), the
// held audio plays, and -- if the run has already finished -- the answer is finished off.
// From here no resume is accepted; the speculative run is left to commit (user + assistant).
func (m *voiceMachine) closeWindow() {
	m.windowClosed = true
	m.stopGrace()
	// Tear down any ASR loopback opened in the window on pre-voiced input (a voiced one
	// would have resumed); we are committing to the answer now.
	m.teardownTurn()
	m.resetChunk()
	m.stopChunkTimers()
	m.setState(voiceStateSpeaking)
	m.flushAudioBuf()
	if m.runFinished {
		m.finishAnswer()
	}
}

// flushAudioBuf relays every held audio chunk in order, then clears the buffer.
func (m *voiceMachine) flushAudioBuf() {
	for _, a := range m.audioBuf {
		m.relayAudio(a)
	}
	m.audioBuf = nil
}

// relayAudio sends one TTS chunk as an audio_delta (seq from 0, one rate), the shape the
// browser's StreamingPcmPlayer reads, and records the real-time playout it adds so the drain
// fallback can bound the wait.
func (m *voiceMachine) relayAudio(a runAudio) {
	m.accountPlayback(a)
	m.relayedAudioChunks++ // the server half of the #322 playout counter (see onPlaybackDone)
	// A newly relayed chunk invalidates any end-of-playout signal remembered so far. The client's
	// onEmpty fires on EVERY queue drain -- including an inter-chunk gap when TTS lags real time,
	// not just the true end -- so a playback_done seen before this chunk was NOT the end of audio.
	// The playout counter (onPlaybackDone) is the primary guard against a stale gap signal; this
	// clear-on-relay is belt-and-suspenders that keeps the counter-less (older-client) path correct
	// for a non-reordering client by invalidating any signal remembered before this chunk.
	m.playbackSignaled = false
	m.writer.send(m.ctx, voiceDownMsg{Type: voiceDownAudioDelta, Seq: a.Seq, Rate: a.Rate, Audio: a.Data})
}

// accountPlayback adds the relayed chunk's real-time duration to the running total used by the
// playout fallback (beginPlaybackWait). The byte rate of s16le mono audio is rate x 2, so the
// chunk plays for pcmBytes / byteRate seconds; firstAudioAt marks when playout would have
// started (the first chunk relayed). A chunk with no usable rate falls the fallback back to
// the fixed cap rather than under-counting the wait.
func (m *voiceMachine) accountPlayback(a runAudio) {
	if m.firstAudioAt.IsZero() {
		m.firstAudioAt = time.Now()
	}
	byteRate := a.Rate * speechStreamBytesPerFrame
	if a.Rate <= 0 || byteRate <= 0 {
		m.playbackRateUnknown = true
		return
	}
	m.relayedPlaybackDur += time.Duration(voicePCMBytes(a.Data)) * time.Second / time.Duration(byteRate)
}

// handleRunTerminal records the run's single terminal outcome and acts on it: a completed
// run drives the commit/speak flow (finished once the window is closed), a cancelled run is
// our own resume (already handled, ignored here), and any other status degrades (spec 6.6).
func (m *voiceMachine) handleRunTerminal(status, errMsg string) {
	if m.runFinished {
		return
	}
	m.runFinished = true
	switch status {
	case "completed":
		// The run has COMMITTED (persistence happened): this is the window's point of no
		// return, so force it closed even if the grace has not yet elapsed. With Task 4's
		// non-pausable run (LLM -> TTS -> commit in sequence) we cannot both hold the mic open
		// for the full grace AND avoid a double commit when the whole run finishes faster than
		// the grace; closing at commit -- a shorter keep-talking window for a fast answer -- is
		// the accepted trade (spec 6.1). A resume past commit is then a NEW turn, never a
		// cancel-and-extend of the already-committed one.
		m.graceElapsed = true
		m.markAnswerStreamed() // make sure answer_done is out
		m.maybeCloseWindow()   // graceElapsed is set now, so a streamed answer closes the window
		if m.windowClosed {
			m.finishAnswer()
		}
	case "canceled":
		// Our own resume/cancel already emitted answer_reset and moved on; nothing to do.
	default:
		m.degrade(firstNonEmptyVoice(errMsg, voiceRunFailedCode))
	}
}

// finishAnswer ends the spoken turn after the run has committed (user + assistant): it
// closes the audio stream, clears the committed pending text, and enters draining. The drain
// holds the mic gated until playout-done (beginPlaybackWait) before the half-duplex tail
// returns the session to listening. Only ever reached on a completed run with the window
// closed.
func (m *voiceMachine) finishAnswer() {
	if m.answerFinished {
		return // the window close and the run terminal can both reach here; finish once
	}
	m.answerFinished = true
	m.writer.send(m.ctx, voiceDownMsg{Type: voiceDownAudioDone})
	m.detachRun()
	m.pendingText = ""
	if m.s.voiceOnPending != nil {
		m.s.voiceOnPending(m.pendingText)
	}
	m.setState(voiceStateDraining)
	m.beginPlaybackWait()
}

// beginPlaybackWait holds the session in draining (the mic gated) until playout-done, then
// applies the mic-resume tail (spec 6.2). The drain ends at max(floor, min(ceiling, signal)):
//   - FLOOR = the relayed audio's real-time duration, the server-authoritative minimum -- the
//     mic NEVER re-opens before it, so an early (buggy/malicious) playback_done is clamped up
//     to the floor and cannot make the model hear the TTS still playing;
//   - a playback_done after the floor extends the drain to the signal (a slow client still
//     playing);
//   - the ceiling (playbackCapOrDefault) is the no-signal upper bound: with no playback_done
//     the drain waits out the ceiling (not the floor), so a missing or late signal cannot
//     wedge the session. It never cuts below the floor.
//
// The gate stays server-authoritative: the client signal only says WHEN playout finished, and
// is never trusted to shorten the drain below the floor. When no audio was relayed there is
// nothing to play out, so the drain falls straight through to the tail.
func (m *voiceMachine) beginPlaybackWait() {
	m.awaitingPlayback = true
	// playbackSignaled is deliberately NOT reset here: a playback_done the client already sent
	// while the session was still speaking -- the summary audio drained before the detail
	// finished, the concurrent flow -- must be honored now, not clobbered back to false. The
	// per-turn reset lives in beginAnswer (resetPlaybackWait), so the flag is already fresh for a
	// turn that saw no early signal. The floor still clamps a remembered early signal:
	// onPlaybackFloorReached ends the drain only once the floor has passed, never before.
	m.playbackFloorPassed = false
	if m.firstAudioAt.IsZero() {
		m.endDrainWait() // no audio was relayed: nothing to play out
		return
	}
	if floor := m.playbackFloorRemaining(); floor > 0 {
		m.armPlaybackWait(floor)
		return
	}
	m.onPlaybackFloorReached() // the floor already elapsed by the time the answer finished
}

// onPlaybackFloorReached runs when the floor (the audio's real-time duration) has elapsed. If
// the client has already reported playout done, the drain ends now (the floor is the max of an
// early signal and the floor). Otherwise the drain waits out a slow or absent signal up to the
// ceiling, so a late playback_done can still extend it.
func (m *voiceMachine) onPlaybackFloorReached() {
	m.playbackFloorPassed = true
	if m.playbackSignaled {
		m.endDrainWait()
		return
	}
	if ceiling := m.playbackCeilingRemaining(); ceiling > 0 {
		m.armPlaybackWait(ceiling)
		return
	}
	m.endDrainWait()
}

// onPlaybackTimer fires for the one playout timer, which is armed first for the floor and then
// re-armed for the ceiling. Before the floor it is the floor reach; after it, it is the ceiling
// elapsing with no signal.
func (m *voiceMachine) onPlaybackTimer() {
	if !m.awaitingPlayback {
		m.stopPlaybackWait() // defensive: a stale fire after the wait already ended
		return
	}
	if !m.playbackFloorPassed {
		m.onPlaybackFloorReached()
		return
	}
	m.endDrainWait() // the ceiling elapsed with no playback_done
}

// onPlaybackDone records the client's playout-done signal. In the concurrent flow the summary
// audio drains -- and the client sends its single playback_done -- while the session is still
// SPEAKING: the LLM detail is still streaming, so the drain wait has not begun (awaitingPlayback
// is false). A VALID signal is therefore remembered REGARDLESS of awaitingPlayback, to be honored
// once the drain begins (beginPlaybackWait/onPlaybackFloorReached); the flag is cleared per turn
// in beginAnswer, so a stray frame can never leak across turns. The signal only ENDS the drain
// here when we are already awaiting playout AND the floor has passed (a slow client still
// playing). It can NEVER shorten the drain below the floor -- that stays the server-authoritative
// minimum, so a remembered early (or forged) signal is still clamped up to the floor.
//
// playedThrough is the client's monotonic count of the audio it has RECEIVED (#322). The client
// sends a playback_done on every queue drain -- including an inter-chunk gap and the summary's
// drain mid-stream -- so a signal is the genuine end of audio only when it accounts for all the
// server has relayed. A counter BELOW relayedAudioChunks is such a stale gap signal: it is ignored
// (not remembered), so it cannot end the drain while later audio is still playing out. A counter
// that accounts for everything relayed is valid. A nil counter is an older client: it falls back
// to the remembered-signal path (clear-on-relay keeps that correct for a non-reordering client),
// so the field stays additive and no old client regresses.
func (m *voiceMachine) onPlaybackDone(playedThrough *int) {
	if playedThrough != nil && *playedThrough < m.relayedAudioChunks {
		return // a stale gap signal: it does not account for all relayed audio, so it is not the end
	}
	m.playbackSignaled = true
	if m.awaitingPlayback && m.playbackFloorPassed {
		m.endDrainWait()
	}
}

// playbackFloorRemaining is how much longer the relayed answer is still playing at its own
// real-time rate, measured from when playout would have started (the first chunk relayed): the
// audio's duration (pcmBytes / byteRate) less the time already elapsed. It is zero when no
// audio was relayed, and the ceiling when a chunk carried no usable rate (no computable floor).
func (m *voiceMachine) playbackFloorRemaining() time.Duration {
	if m.firstAudioAt.IsZero() {
		return 0
	}
	if m.playbackRateUnknown {
		return m.playbackCeilingRemaining()
	}
	if remaining := time.Until(m.firstAudioAt.Add(m.relayedPlaybackDur)); remaining > 0 {
		return remaining
	}
	return 0
}

// playbackCeilingRemaining is how much of the absolute drain ceiling is left, measured from
// when playout would have started, so a missing or pathologically-late playback_done cannot
// wedge the session.
func (m *voiceMachine) playbackCeilingRemaining() time.Duration {
	if m.firstAudioAt.IsZero() {
		return 0
	}
	if remaining := time.Until(m.firstAudioAt.Add(m.playbackCapOrDefault())); remaining > 0 {
		return remaining
	}
	return 0
}

// playbackCapOrDefault is the drain's absolute ceiling: the resolved timing's value, or the
// VoicePlaybackFallbackMs const when unset (a bare/test timing).
func (m *voiceMachine) playbackCapOrDefault() time.Duration {
	if m.timing.playbackCap > 0 {
		return m.timing.playbackCap
	}
	return VoicePlaybackFallbackMs * time.Millisecond
}

// endDrainWait leaves the playout wait and applies the mic-resume tail (the half-duplex tail)
// before the session returns to listening.
func (m *voiceMachine) endDrainWait() {
	m.resetPlaybackWait()
	m.armTail()
}

// onResume handles resumed speech inside the continuation window (spec 6.1): cancel the
// speculative run (it persists nothing), tell the client to drop the answer preview, and
// fall back to listening. The resumed chunk's ASR loopback is LEFT OPEN -- it is capturing
// this new chunk, which will extend pendingText and re-run on the next chunk end.
func (m *voiceMachine) onResume() {
	m.detachRun()
	m.stopGrace()
	m.writer.send(m.ctx, voiceDownMsg{Type: voiceDownAnswerReset})
	m.setState(voiceStateListening)
}

// runResumable reports whether a resumed voiced chunk MAY cancel-and-extend the in-flight
// speculative run: there IS a run, the continuation window is still open, the run is still
// running, AND neither its commit nor a cancel has claimed it yet (commitUndecided: the one
// commit/cancel word is still voiceRunRunning). The word check closes the window the status
// check alone leaves: executeVoiceRun commits under context.WithoutCancel, so the run's
// status stays "running" for the whole store write while a cancel would no longer un-do it --
// a resume there would persist the first turn AND re-run on the extended text,
// double-committing the utterance. It is a pre-check (a READ): the decision to resume is
// tryResume's claimCancel CAS, which is what makes it race-free against the run's own
// claimCommit. All reads are safe from this goroutine: machine state plus the run's own
// atomic/locked accessors.
func (m *voiceMachine) runResumable() bool {
	return m.curRun != nil && !m.windowClosed &&
		m.curRun.statusValue() == "running" && m.curRun.commitUndecided()
}

// tryResume decides a resume: it reports true only when the run was resumable AND this call
// won the cancel -- claimCancel is a compare-and-swap on the same word the run's claimCommit
// uses, so exactly one of "the run commits" and "the resume cancels it" can happen. true
// means the run will now commit NOTHING (its claimCommit fails) and the caller proceeds to
// cancel-and-extend (onResume). false means the run is committing (or committed) -- the
// resume is NOT accepted and the voiced input stays an in-progress chunk, i.e. a new turn.
// The claim is the decision; the read before it (runResumable) only skips a pointless CAS.
func (m *voiceMachine) tryResume() bool {
	return m.runResumable() && m.curRun.claimCancel()
}

// degrade surfaces a recoverable leg failure (spec 6.6): one typed error frame carrying the
// core's stable code, the turn stopped (run cancelled, loopback torn down, pending dropped),
// and a fall back to idle with the mic re-enabled -- no silent retry. The client re-engages
// with push_start.
func (m *voiceMachine) degrade(code string) {
	m.emitError(code, voiceTurnFailedMessage)
	m.detachRun()
	m.teardownTurn()
	m.resetChunk()
	m.stopChunkTimers()
	m.stopGrace()
	m.stopTail()
	m.resetPlaybackWait()
	m.pendingText = ""
	m.setState(voiceStateIdle)
}

// detachRun ends the current speculative run and clears every window field. It unsubscribes
// (so no further event is delivered), cancels the run's context (a cancelled run persists
// nothing) and removes it from the registry synchronously so the NEXT chunk's run can
// reserve the chat's slot without racing the cancelled run's own async retirement. The
// cancel goes through ChatRun.cancelContext, which claims the run's commit/cancel word
// BEFORE cancelling the context -- so a run still short of its commit claim can never
// commit afterwards. A run that already claimed its commit stays committing (its write is
// under context.WithoutCancel); a RESUME never reaches here for such a run (tryResume).
// Safe to call when no run is open; idempotent on an already-finished run.
func (m *voiceMachine) detachRun() {
	if m.runUnsub != nil {
		m.runUnsub()
	}
	if m.curRun != nil {
		m.curRun.cancelContext()
		m.s.ChatRuns.remove(m.curRun)
	}
	m.curRun, m.runCh, m.runUnsub = nil, nil, nil
	m.answerStreamed, m.answerDoneSent, m.graceElapsed, m.windowClosed, m.runFinished = false, false, false, false, false
	m.audioBuf = nil
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
	m.resetPlaybackWait()
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

func (m *voiceMachine) armGrace() {
	if m.graceTimer != nil {
		m.graceTimer.Stop()
	}
	m.graceTimer = time.NewTimer(m.timing.continuationGrace)
	m.graceC = m.graceTimer.C
}

// armPlaybackWait starts the playout fallback timer for d (the relayed audio's remaining
// real-time duration, or the fixed cap). Re-arming replaces the channel the select reads, so a
// stale fire from an old timer is never selected.
func (m *voiceMachine) armPlaybackWait(d time.Duration) {
	if m.playbackTimer != nil {
		m.playbackTimer.Stop()
	}
	m.playbackTimer = time.NewTimer(d)
	m.playbackC = m.playbackTimer.C
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

func (m *voiceMachine) stopGrace() {
	if m.graceTimer != nil {
		m.graceTimer.Stop()
		m.graceTimer = nil
	}
	m.graceC = nil
}

// stopPlaybackWait disarms the playout timer (it does NOT clear the awaiting/floor/signal
// latches, so onPlaybackTimer can tell a real wait from a stale fire).
func (m *voiceMachine) stopPlaybackWait() {
	if m.playbackTimer != nil {
		m.playbackTimer.Stop()
		m.playbackTimer = nil
	}
	m.playbackC = nil
}

// resetPlaybackWait ends any playout drain wait: no longer awaiting, its latches cleared, and
// the timer disarmed.
func (m *voiceMachine) resetPlaybackWait() {
	m.awaitingPlayback = false
	m.playbackSignaled = false
	m.playbackFloorPassed = false
	m.stopPlaybackWait()
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
	m.stopGrace()
	m.stopPlaybackWait()
}

// voicePCMBytes is the exact decoded length of a standard (padded) base64 audio chunk, found
// arithmetically so no allocation or decode is needed just to time the chunk's playout. A
// chunk whose length is not a whole quantum is degenerate (it would not decode to playable
// audio either); the estimate it yields only feeds the drain fallback, never playback.
func voicePCMBytes(b64 string) int {
	n := len(b64)
	if n < 4 {
		return 0
	}
	pad := 0
	if b64[n-1] == '=' {
		pad++
		if b64[n-2] == '=' {
			pad++
		}
	}
	return n/4*3 - pad
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
