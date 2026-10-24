// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
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

// relayVoiceChat runs the browser-facing half of a voice session after the upgrade: it
// owns the session's lifecycle, ONE context cancelled on any exit; a single writer
// goroutine that owns every WS write (data frames, the keepalive ping, the connection
// close); a read loop; and a teardown that can leave no goroutine blocked or leaked.
//
// This is the transport scaffold. It announces the session with an initial state frame,
// reads the browser's control frames and ends on bye or a drop.
//
// TODO(voice-chat state machine, Task 6): the ASR / LLM / TTS state machine slots in
// here. The seams are marked below: voiceReadLoop is where mic PCM becomes the per-turn
// ASR loopback's input (and is dropped while the session is speaking or draining) and
// where push_start, push_end and cancel drive the turn; the writer is how every frame
// the machine produces (state, asr_*, answer_*, audio_*, error) reaches the browser.
func (s *Server) relayVoiceChat(r *http.Request, conn *websocket.Conn, claims voiceTicketClaims) {
	id := nextRequestID()
	slog.Debug("voice chat: session started", "id", id, "chat_id", claims.ChatID)

	// The one context: cancelled on any exit it stops the writer, which closes the
	// connection, which unblocks the read loop. Derived from the request, but the read
	// loop deliberately does NOT read on it (see voiceReadLoop).
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	// The sole writer. Every WS write, frames, the ping and the connection close, goes
	// through it, so writes are always serialized.
	writer := newVoiceWriter(conn, cancel)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); writer.run(ctx) }()

	// Teardown, DEFERRED so it is airtight even if the relay panics: cancel the one
	// context (the writer flushes what is queued and closes the connection) and join the
	// writer. Nothing else is left running in this scaffold.
	defer func() {
		cancel()
		wg.Wait()
		slog.Debug("voice chat: session ended", "id", id, "chat_id", claims.ChatID)
	}()

	// TODO(Task 6): the machine decides the initial state (idle vs listening) from the
	// session; the scaffold opens straight into listening, the hands-free default.
	writer.send(ctx, voiceDownMsg{Type: voiceDownState, State: voiceStateListening})

	voiceReadLoop(conn, writer)
}

// voiceReadLoop reads the browser's frames until the session ends and returns on the
// first read error (a close, a drop, a frame over the read limit) or on bye.
//
//   - A BINARY frame is mic PCM. The scaffold discards it.
//   - A TEXT frame is a control (parseVoiceUpControl). bye ends the session with a clean
//     close handshake; every other text frame, including the controls the scaffold does
//     not act on yet and anything that does not parse, is ignored.
//
// The read uses context.Background(), NOT the handler context: cancelling that context
// makes coder/websocket tear the connection down abruptly from a second site, which would
// race the single writer. The writer's close (on its ctx.Done teardown) is what unblocks
// this read instead.
//
// On bye the loop has stopped reading before the writer closes, so the close HANDSHAKE
// (writer.closeNormally) can take the read mutex it needs. On any other exit the peer is
// gone or misbehaving and the writer closes abruptly.
func voiceReadLoop(conn *websocket.Conn, writer *voiceWriter) {
	readCtx := context.Background()
	for {
		typ, data, err := conn.Read(readCtx)
		if err != nil {
			return
		}
		switch typ {
		case websocket.MessageBinary:
			// TODO(Task 6): mic PCM -> the per-turn ASR loopback; discarded while the
			// session is speaking or draining (the half-duplex mic gate).
		case websocket.MessageText:
			ctl, ok := parseVoiceUpControl(data)
			if !ok {
				continue
			}
			switch ctl.Type {
			case voiceUpBye:
				writer.closeNormally()
				return
			case voiceUpPushStart, voiceUpPushEnd, voiceUpCancel:
				// TODO(Task 6): push_start / push_end bound a push-to-talk chunk and
				// cancel abandons the turn in flight.
			}
		}
	}
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
