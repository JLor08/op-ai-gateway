// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/inference"
	"op-ai-gateway/internal/provider"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/usage"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
)

// liveWSSubprotocol is the fixed WebSocket subprotocol the browser offers first on
// a live-transcription upgrade; the single-use ticket (Task 6/7) is the SECOND
// offered value. The browser cannot set custom headers on a WebSocket upgrade, so
// `new WebSocket(url, ["op-live-transcription", <ticket>])` is how it carries both
// the protocol selector and the ticket. The handler reads the ticket off
// Sec-WebSocket-Protocol and redeems it BEFORE websocket.Accept, then selects this
// fixed name in the handshake so the browser's WebSocket.protocol is stable and the
// opaque ticket never becomes the negotiated subprotocol.
const liveWSSubprotocol = "op-live-transcription"

// maxLiveFrameBytes caps a single inbound PCM WebSocket frame, the twin of
// maxAgentFrameBytes on the agent stream. It is both a guard and a CONTRACT the
// portal's capture worklet has to frame its PCM against: a frame one byte over makes
// coder/websocket fail the read and close the connection 1009, ending the session.
// 1 MiB is ~5.4 s of 48 kHz f32le stereo or ~32 s of 16 kHz s16le mono, far more
// than a worklet's per-callback batch, so it never clips a well-behaved client while
// still bounding a hostile one.
const maxLiveFrameBytes int64 = 1 << 20

// maxLiveSSELineBytes bounds one line of the upstream SSE the gateway relays down.
// A transcript.text.done frame restates the whole transcript, so the bound matches
// the transcription body cap used elsewhere; a line past it fails the read, which
// ends the relay (and so bills a loud zero, never a silent one).
const maxLiveSSELineBytes = 1 << 20

// liveOutboundQueue bounds the single writer goroutine's outbound queue. Transcript
// frames are small and infrequent relative to the audio, so a short queue is ample;
// a sender that outruns it blocks (never drops a transcript frame), bounded by the
// handler context so it can never block past teardown.
const liveOutboundQueue = 32

// liveUpstreamContentType is the media type the gateway labels the raw PCM request
// body with. audio.cpp /live (and the agent's /live/{model} relay, which forwards
// the header verbatim) reads the body as raw interleaved PCM regardless of
// Content-Type and takes its parameters from the query, so this is purely honest
// labelling: octet-stream, not the provider seam's application/json default, which
// would mislabel raw audio as JSON.
const liveUpstreamContentType = "application/octet-stream"

// liveSSEContentType is recorded on the usage row as the response content type: the
// gateway relayed audio.cpp's own text/event-stream down to the browser.
const liveSSEContentType = "text/event-stream"

// The SSE event types audio.cpp's /live answers with, beyond the delta/done the
// buffered streamed transcription already names (transcription_stream.go): a
// revisable-hypothesis model emits a provisional whole-transcript `partial` to
// REPLACE (not append), and a mid-stream failure is an `error` event.
const (
	liveEventPartial = "transcript.text.partial"
	liveEventError   = "error"
)

var (
	// errLiveRelayDone unblocks the pipe on a clean teardown: the transport's
	// body-copy (reading the pipe reader) and any still-pending pipe write both
	// return it. It is never surfaced to the client.
	errLiveRelayDone = errors.New("live transcription: relay done")
	// errLiveClientGone aborts the upstream request body when the WebSocket closes
	// WITHOUT an end-of-input control: the pipe writer fails with it, so the
	// transport sends NO terminating chunk and audio.cpp treats the input as
	// truncated (no transcript.text.done) -- which is what makes the session a loud
	// zero rather than a billed one.
	errLiveClientGone = errors.New("live transcription: client gone without end-of-input")
	// liveNewline reconstructs, for the completion scanner, the newline bufio.Scanner
	// strips from each relayed SSE line.
	liveNewline = []byte{'\n'}
)

// liveDownMsg is the typed envelope the gateway relays down to the browser over the
// WebSocket. delta/partial/done carry Text (the increment, the provisional whole
// transcript, or the final transcript); error carries Message. One field per shape
// keeps the wire minimal; the browser switches on Type.
type liveDownMsg struct {
	Type    string `json:"type"`
	Text    string `json:"text,omitempty"`
	Message string `json:"message,omitempty"`
}

// liveUpControl is the only text (non-binary) message the browser sends up: an
// explicit end-of-input. It is distinct from a WebSocket close -- audio.cpp /live
// ends NORMALLY only on the terminating chunk this message produces, so a close
// WITHOUT it is a truncated session, not a clean finish.
type liveUpControl struct {
	Type string `json:"type"`
}

// handleLiveTranscription is GET /api/portal/transcription-live: the browser's
// full-duplex WebSocket transport for live transcription (PCM up, typed transcript
// text down). It is the merged Task 9 (upgrade/auth/guard/envelope) + Task 10
// (relay/billing/lifecycle).
//
// Security boundary (BEFORE any upgrade): the single-use, encrypted ticket the
// authenticated CSRF-protected POST minted (live_transcription_ticket.go) rides as a
// Sec-WebSocket-Protocol value and is redeemed here before websocket.Accept. A
// missing/expired/replayed ticket is refused HTTP 401 with NO upgrade. Accept is
// called with its DEFAULT origin policy (no InsecureSkipVerify, no OriginPatterns),
// which is coder/websocket's same-origin check -- the CSRF/origin defense for a
// browser upgrade that cannot carry X-OP-CSRF. The handler never takes a model,
// route or billing rate from the client: all of that is the server's own decision,
// sealed in the redeemed claims, which this handler TRUSTS (no re-resolve, no
// re-gate, no second admission -- the ticket endpoint already spent the one).
func (s *Server) handleLiveTranscription(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// 1. Redeem the ticket BEFORE Accept. A miss is a plain HTTP 401 (no upgrade),
	// which a cross-origin page cannot turn into a session because it cannot obtain a
	// ticket in the first place.
	ticket := extractLiveTicket(r.Header.Get("Sec-WebSocket-Protocol"))
	if ticket == "" || s.liveTickets == nil {
		http.Error(w, "missing live transcription ticket", http.StatusUnauthorized)
		return
	}
	claims, ok := s.liveTickets.Redeem(ticket)
	if !ok {
		http.Error(w, "invalid or expired live transcription ticket", http.StatusUnauthorized)
		return
	}

	// 2. Upgrade with the DEFAULT (same-origin) origin policy and the fixed
	// subprotocol selected. A cross-origin request is refused 403 here by
	// coder/websocket itself.
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{liveWSSubprotocol}})
	if err != nil {
		slog.Debug("live transcription: websocket accept failed", "model", claims.Target.Model, "err", err)
		return
	}
	conn.SetReadLimit(maxLiveFrameBytes)

	// 3. One live session per route (audio.cpp serves one /live per model). A second
	// concurrent session on the same route is refused with a structured busy close,
	// never a silent collision at the backend. A nil registry fails closed.
	release, acquired := s.liveSessions.acquire(claims.Target.RouteID)
	if !acquired {
		_ = conn.Close(websocket.StatusTryAgainLater, "live transcription busy: one session per model")
		return
	}
	defer release()

	s.relayLiveTranscription(r, conn, claims)
}

// extractLiveTicket returns the ticket from a Sec-WebSocket-Protocol header value:
// the one offered subprotocol that is NOT the fixed liveWSSubprotocol. Empty when
// none is offered (so the handler refuses before any upgrade).
func extractLiveTicket(header string) string {
	for _, part := range strings.Split(header, ",") {
		v := strings.TrimSpace(part)
		if v != "" && v != liveWSSubprotocol {
			return v
		}
	}
	return ""
}

// relayLiveTranscription runs the full-duplex relay and bills it once. It owns the
// lifecycle: ONE context cancelled on any exit; a single writer goroutine that owns
// every WS write (data frames, the keepalive ping, and the connection close); a read
// loop that pumps inbound PCM into the upstream request body through an io.Pipe
// while counting the payload bytes; and a release-once teardown that can leave no
// goroutine blocked on the pipe.
func (s *Server) relayLiveTranscription(r *http.Request, conn *websocket.Conn, claims liveTicketClaims) {
	start := time.Now()
	id := nextRequestID()

	// The one context: cancelled on any exit tears down the upstream request, the
	// read loop and the pipe feeder together. Derived from the request, but the read
	// loop deliberately does NOT read on it (see liveReadLoop).
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	// The sole writer. Every WS write -- transcript frames, the ping, and the
	// connection close -- goes through it, so writes are always serialized: a stray
	// second writer would make a write "succeed" while the frame is lost on the wire.
	writer := newLiveWriter(conn, cancel)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); writer.run(ctx) }()

	// The upstream request body is the read side of this pipe; the read loop writes
	// inbound PCM into the write side and counts the payload.
	pr, pw := io.Pipe()
	var pcmBytes int64
	var endRequested atomic.Bool
	wg.Add(1)
	go func() {
		defer wg.Done()
		liveReadLoop(conn, cancel, pw, &pcmBytes, &endRequested)
	}()

	// Open the full-duplex upstream request. ProxyNativeDuplex returns as soon as the
	// response headers arrive, with the body still open and the request body (pr)
	// still being read; it applies no target.Timeout.
	scan := s.newTranscriptionStreamScan(endpointTranscription, false)
	resp, upErr := s.proxyLiveDuplex(ctx, claims, pr)
	if upErr == nil {
		defer resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			writer.send(ctx, liveMarshal(liveDownMsg{Type: "error", Message: "live transcription upstream error"}))
			slog.Error("live transcription: upstream non-2xx", "id", id, "status", resp.StatusCode, "model", claims.Target.Model, "server", s.serverName(claims.Target.ServerID))
		} else {
			s.liveRelaySSE(ctx, resp.Body, scan, writer)
		}
	} else {
		writer.send(ctx, liveMarshal(liveDownMsg{Type: "error", Message: "live transcription upstream unavailable"}))
		slog.Error("live transcription: upstream dial failed", "id", id, "model", claims.Target.Model, "server", s.serverName(claims.Target.ServerID), "err", upErr)
	}

	// Teardown, leak-free and in order:
	//   1. Unblock the pipe from BOTH sides at once: the transport's body-copy
	//      (reading pr) and the read loop's possibly-blocked pw.Write both return.
	//      The transport uses io.NopCloser, so it never closes pr for us; net/http
	//      cancels r.Context() on a client drop only AFTER the body is drained, so
	//      teardown is driven from here, never by waiting on r.Context().
	_ = pr.CloseWithError(errLiveRelayDone)
	//   2. Cancel the one context: this is the single teardown signal. The writer
	//      flushes any queued transcript frames and then CloseNow()s the connection
	//      (an abrupt close after clean data delivery), which unblocks the read loop's
	//      conn.Read. A clean close HANDSHAKE is deliberately NOT used on this hot
	//      path: coder/websocket's Close() needs the read mutex to complete, but the
	//      read loop holds it blocked in conn.Read, so a handshake would stall ~5s
	//      every session -- the same reason handleAgentStream CloseNow()s its hot
	//      path. The browser already has every transcript frame (the done/error
	//      message is the semantic end); the close code is not load-bearing here. The
	//      structured close frame IS used where it matters and no reader competes: the
	//      busy reject (handleLiveTranscription).
	cancel()
	//   3. Join: pcmBytes and endRequested are stable once both goroutines return.
	wg.Wait()

	s.recordLiveUsage(start, id, r, claims, endRequested.Load(), scan, atomic.LoadInt64(&pcmBytes), upErr, resp)
}

// proxyLiveDuplex opens the full-duplex upstream request for claims, feeding its body
// from pr. The native provider must offer the optional duplex capability (the
// Multiplexer over the OpenAI-compatible client does); a provider without it is a
// clean ErrUnavailable, never a panic.
func (s *Server) proxyLiveDuplex(ctx context.Context, claims liveTicketClaims, pr *io.PipeReader) (*provider.ProxyResponse, error) {
	duplex, ok := s.Provider.(provider.NativeDuplexProxyClient)
	if !ok {
		return nil, fmt.Errorf("%w: live transcription duplex relay not supported by the native provider", provider.ErrUnavailable)
	}
	return duplex.ProxyNativeDuplex(ctx, claims.Target, liveUpstreamPath(claims), pr, liveUpstreamContentType)
}

// liveUpstreamPath builds the upstream request path from the claims, by provider.
//   - A server_agent target is reached through the agent's /live/{model} relay, which
//     extracts the model from the PATH and adds model= to the CHILD query itself, so
//     the gateway forwards only the audio params and NEVER adds model here.
//   - A direct audio_cpp target has no agent in front, so the model rides in the
//     query of audio.cpp's own /v1/audio/transcriptions/live.
//
// The (sample_rate, channels, sample_format, language) triple-plus-language comes
// from the claims -- the SAME validated values the session bills at (billing-rate ≡
// forwarded-rate). Language is already validated to [A-Za-z0-9_-]{0,64}, so it
// cannot smuggle a second query parameter; it is omitted when empty (audio.cpp then
// detects the language).
func liveUpstreamPath(claims liveTicketClaims) string {
	q := url.Values{}
	q.Set("sample_rate", strconv.Itoa(claims.SampleRate))
	q.Set("channels", strconv.Itoa(claims.Channels))
	q.Set("sample_format", claims.Format)
	if claims.Language != "" {
		q.Set("language", claims.Language)
	}
	if claims.Target.Provider == routing.ProviderServerAgent {
		return provider.ExpandModelPath("/live/{model}", claims.Target.ProviderModel) + "?" + q.Encode()
	}
	q.Set("model", claims.Target.ProviderModel)
	return "/v1/audio/transcriptions/live?" + q.Encode()
}

// liveReadLoop pumps inbound WebSocket frames into the upstream request body and
// counts the PCM payload. It is the SOLE writer of pw.
//
//   - A BINARY frame is PCM: its payload is written to pw (and so streamed upstream)
//     and its length added to pcmBytes -- WS framing is never counted, only the bytes
//     relayed. A write that fails (the pipe was torn down) ends the loop.
//   - A TEXT frame is a control message; the only one is {"type":"end"}, the explicit
//     end-of-input. It closes pw cleanly (io.EOF), which the transport turns into the
//     terminating chunk audio.cpp needs to emit transcript.text.done + [DONE]. Every
//     other text frame is ignored.
//
// The read uses context.Background(), NOT the handler context: cancelling that
// context triggers coder/websocket's ABRUPT teardown from a second site, which would
// race the single writer. The writer's CloseNow (on its ctx.Done teardown) is what
// unblocks this read instead. On exit:
//   - end-of-input was requested → the terminating chunk is already sent; do nothing
//     and let the upstream finish (cancelling here would abort it before the terminal
//     events and wrongly loud-zero a clean session);
//   - otherwise (a close/drop without end-of-input) → abort the upstream body (no
//     terminating chunk) and cancel the context, tearing the whole relay down.
func liveReadLoop(conn *websocket.Conn, cancel context.CancelFunc, pw *io.PipeWriter, pcmBytes *int64, endRequested *atomic.Bool) {
	readCtx := context.Background()
readLoop:
	for {
		typ, data, err := conn.Read(readCtx)
		if err != nil {
			break
		}
		switch typ {
		case websocket.MessageBinary:
			n, werr := pw.Write(data)
			if werr != nil {
				break readLoop
			}
			atomic.AddInt64(pcmBytes, int64(n))
		case websocket.MessageText:
			if isLiveEndControl(data) {
				endRequested.Store(true)
				_ = pw.Close() // clean EOF -> terminating chunk
			}
		}
	}
	if endRequested.Load() {
		return
	}
	_ = pw.CloseWithError(errLiveClientGone)
	cancel()
}

// isLiveEndControl reports whether a text frame is the end-of-input control.
func isLiveEndControl(data []byte) bool {
	var c liveUpControl
	if err := json.Unmarshal(data, &c); err != nil {
		return false
	}
	return c.Type == "end"
}

// liveRelaySSE reads the upstream SSE, feeds the completion scanner, and relays each
// event to the browser as a typed down-message through the single writer. It uses
// the SAME audio.cpp-SSE parser the buffered streamed transcription uses
// (transcriptionStreamScan for completion, decodeTranscriptionEvent for each event);
// it does not fork it. It returns when the body ends (EOF, error, or a line past the
// bound), which is the relay's end.
func (s *Server) liveRelaySSE(ctx context.Context, body io.Reader, scan *transcriptionStreamScan, writer *liveWriter) {
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 0, 64*1024), maxLiveSSELineBytes)
	for sc.Scan() {
		line := sc.Bytes()
		// Completion tracking: feed the line and the newline the Scanner stripped, so
		// scan.complete() latches exactly as it does on the buffered relay.
		scan.feed(line)
		scan.feed(liveNewline)
		msg, ok := liveDownMsgFromSSELine(line)
		if !ok {
			continue
		}
		writer.send(ctx, liveMarshal(msg))
		if msg.Type == "error" {
			// A mid-stream error is terminal: nothing more will be transcribed, and
			// the upstream may now sit draining our still-open request body (net/http
			// discards an unread body before completing the response). Stop relaying
			// so teardown aborts that body and the exchange can end instead of hanging.
			break
		}
	}
	scan.finish()
}

// liveDownMsgFromSSELine maps one SSE line to a typed down-message, or reports false
// for a line that carries nothing to relay (a comment, a blank, the [DONE] sentinel,
// an event of a type the browser does not consume). It reuses decodeTranscriptionEvent
// -- the shared audio.cpp event decoder -- rather than a second parser.
func liveDownMsgFromSSELine(line []byte) (liveDownMsg, bool) {
	payload, ok := bytes.CutPrefix(bytes.TrimSpace(line), []byte("data:"))
	if !ok {
		return liveDownMsg{}, false
	}
	payload = bytes.TrimSpace(payload)
	if len(payload) == 0 || string(payload) == sseDoneSentinel {
		return liveDownMsg{}, false
	}
	event, ok := decodeTranscriptionEvent(payload)
	if !ok {
		return liveDownMsg{}, false
	}
	switch event.Type {
	case transcriptionEventDelta:
		return liveDownMsg{Type: "delta", Text: event.Delta}, true
	case liveEventPartial:
		return liveDownMsg{Type: "partial", Text: liveEventText(event)}, true
	case transcriptionEventDone:
		return liveDownMsg{Type: "done", Text: liveEventText(event)}, true
	case liveEventError:
		return liveDownMsg{Type: "error", Message: liveErrorMessage(payload)}, true
	}
	return liveDownMsg{}, false
}

// liveEventText is the text a partial or done event states: its Text when set (the
// whole provisional or final transcript), else its Delta. An empty done text is a
// valid statement (the client keeps whatever it had), so Text wins whenever present.
func liveEventText(event transcriptionEvent) string {
	if event.Text != nil {
		return *event.Text
	}
	return event.Delta
}

// liveErrorMessage extracts a human-readable message from an SSE error event's
// payload. audio.cpp's exact error shape is treated as given; this reads the common
// forms -- {"error":{"message":...}}, {"error":"..."} or {"message":...} -- and
// falls back to a generic sentence, so the browser always gets a non-empty message.
func liveErrorMessage(payload []byte) string {
	var shape struct {
		Error   json.RawMessage `json:"error"`
		Message string          `json:"message"`
	}
	if err := json.Unmarshal(payload, &shape); err == nil {
		if shape.Message != "" {
			return shape.Message
		}
		if len(shape.Error) > 0 {
			var obj struct {
				Message string `json:"message"`
			}
			if json.Unmarshal(shape.Error, &obj) == nil && obj.Message != "" {
				return obj.Message
			}
			var str string
			if json.Unmarshal(shape.Error, &str) == nil && str != "" {
				return str
			}
		}
	}
	return "live transcription upstream error"
}

// liveMarshal marshals a down-message. The shapes here never fail to marshal; a
// defensive failure yields an empty frame, which the writer skips.
func liveMarshal(m liveDownMsg) []byte {
	b, err := json.Marshal(m)
	if err != nil {
		return nil
	}
	return b
}

// recordLiveUsage bills the session ONCE, honestly (ADR-041), mirroring
// setStreamedSpeechBillingQuantity:
//   - the failure default is (audio_second, 0);
//   - a CLEAN completion -- no upstream error, end-of-input was requested, the
//     scanner saw transcript.text.done THEN [DONE], and the validated byte-rate is
//     positive -- bills pcmBytes / byteRate, where byteRate is recomputed from the
//     SAME validated triple forwarded upstream (billing-rate ≡ forwarded-rate);
//   - every other end (a close without end-of-input, an upstream error, an
//     incomplete stream, a non-positive rate) leaves the quantity 0 and says so once
//     at Error -- a loud zero, never a silent one, and never a guessed count.
//
// The row attributes UserID + TokenID + RouteID + ServerID from the claims. The
// claims carry no ServiceID/ProjectID, so a live row is not service/project-scoped
// (acceptable for phase 1; a follow-up could thread them through the ticket).
func (s *Server) recordLiveUsage(start time.Time, id string, r *http.Request, claims liveTicketClaims, endRequested bool, scan *transcriptionStreamScan, pcmBytes int64, upErr error, resp *provider.ProxyResponse) {
	meta := usageMeta{
		ReqPath:     r.URL.Path,
		HTTPStatus:  liveHTTPStatus(upErr, resp),
		ContentType: liveSSEContentType,
		Endpoint:    endpointTranscription,
	}
	meta.BillingUnit, meta.BillingQuantity = usage.BillingUnitAudioSecond, 0

	byteRate, rateOK := liveByteRate(claims.SampleRate, claims.Channels, claims.Format)
	status, errorCode := "success", ""
	switch {
	case upErr == nil && endRequested && scan.complete() && rateOK && byteRate > 0:
		meta.BillingQuantity = float64(pcmBytes) / float64(byteRate)
	default:
		slog.Error("live transcription billed zero",
			"id", id, "path", meta.ReqPath, "model", claims.Target.Model, "server", s.serverName(claims.Target.ServerID),
			"end_requested", endRequested, "complete", scan.complete(), "rate_ok", rateOK, "byte_rate", byteRate, "pcm_bytes", pcmBytes, "upstream_error", upErr)
		if upErr != nil {
			status, errorCode = "error", completionErrorCode(upErr)
		}
	}

	token := auth.Token{UserID: claims.UserID, ID: claims.TokenID}
	req := inference.Request{Model: claims.Target.Model, APIFlavor: claims.Target.APIFlavor}
	if req.APIFlavor == "" {
		req.APIFlavor = apiFlavorSpeech
	}
	s.recordUsage(start, token, req, claims.Target, provider.Response{}, errorCode, status, meta, id, nil, nil)
}

// liveHTTPStatus is the status recorded on the usage row: the mapped status of an
// upstream dial failure, the upstream's own non-2xx, or 200 for a session that
// relayed (clean or truncated -- the truncation shows in the loud-zero quantity, not
// the status).
func liveHTTPStatus(upErr error, resp *provider.ProxyResponse) int {
	if upErr != nil {
		return completionHTTPStatus(upErr)
	}
	if resp != nil && (resp.StatusCode < 200 || resp.StatusCode >= 300) {
		return resp.StatusCode
	}
	return http.StatusOK
}

// liveWriter is a live session's sole WebSocket writer. Exactly one goroutine
// (run) drains its queue AND owns the keepalive ping AND performs the connection's
// close, so no two writes to the connection ever race -- the "genau EIN Writer pro
// WS-Verbindung" discipline agentStreamConn.runWriter documents.
type liveWriter struct {
	conn   *websocket.Conn
	cancel context.CancelFunc
	out    chan []byte

	pingInterval time.Duration
	pingTimeout  time.Duration
	writeTimeout time.Duration
}

func newLiveWriter(conn *websocket.Conn, cancel context.CancelFunc) *liveWriter {
	return &liveWriter{
		conn:         conn,
		cancel:       cancel,
		out:          make(chan []byte, liveOutboundQueue),
		pingInterval: agentStreamPingInterval,
		pingTimeout:  agentStreamPingTimeout,
		writeTimeout: agentStreamWriteTimeout,
	}
}

// run is the sole writer loop. It returns:
//   - on CONTEXT CANCEL (the one teardown signal, from a clean end, a client drop,
//     an upstream failure or a reap from within this loop): it flushes any queued
//     transcript frames best-effort, then CloseNow()s the connection -- an abrupt
//     close AFTER clean data delivery, which also unblocks the read loop's conn.Read.
//     A clean close handshake is not attempted here (see relayLiveTranscription's
//     teardown note: it would stall on the read mutex the read loop holds);
//   - on the first write/ping failure: it reaps the connection.
//
// Every exit cancels the handler context, so a dead peer tears the whole relay down.
func (w *liveWriter) run(ctx context.Context) {
	ticker := time.NewTicker(w.pingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			w.drain()
			_ = w.conn.CloseNow()
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

// write sends one data frame with a per-write timeout; false means the write failed
// (a dead/half-open peer) and the caller must reap.
func (w *liveWriter) write(data []byte) bool {
	if len(data) == 0 {
		return true
	}
	wctx, wcancel := context.WithTimeout(context.Background(), w.writeTimeout)
	err := w.conn.Write(wctx, websocket.MessageText, data)
	wcancel()
	if err != nil {
		slog.Debug("live transcription: down-write failed, reaping", "err", err)
		return false
	}
	return true
}

// drain writes every data frame already queued, non-blocking, stopping on the first
// write failure or an empty queue. It is how teardown delivers the final transcript
// (a done or error message enqueued just before the context was cancelled) before the
// abrupt close.
func (w *liveWriter) drain() {
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

// send enqueues a data frame, blocking only until the writer takes it or the handler
// context ends (so it never outlives teardown, and never drops a transcript frame
// while the session is live).
func (w *liveWriter) send(ctx context.Context, data []byte) {
	if len(data) == 0 {
		return
	}
	select {
	case w.out <- data:
	case <-ctx.Done():
	}
}
