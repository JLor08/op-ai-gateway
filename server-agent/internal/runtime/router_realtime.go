// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package runtime

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// realtimePathPrefix is the path prefix of GET /realtime/{model}; everything
// after it is the model, decoded like the /live path.
const realtimePathPrefix = "/realtime/"

// realtimeChildPath is the child endpoint GET /realtime/{model} relays to: the
// OpenAI Realtime-style WebSocket that sglang-omni mounts with
// --enable-realtime.
const realtimeChildPath = "/v1/realtime"

// realtimeMessageLimit bounds one message read from either side of a realtime
// session (4 MiB). A message one byte over it ends the session with 1009 on
// both sides; nothing larger is ever buffered.
const realtimeMessageLimit = 4 << 20

// realtimeIdleTimeout is how long a realtime session may go without a message
// in either direction before the router ends it. Pings do not count. It is far
// above sglang-omni's own idle timeout (300 s for a native session), so the
// child normally ends a silent session first; this bound exists so that a
// session nothing ends -- a peer gone without a FIN, say -- cannot hold its
// spec admitted for good.
const realtimeIdleTimeout = 15 * time.Minute

// realtimeCloseBound is how long Close waits for the realtime sessions it ends.
// coder/websocket bounds each close handshake itself (5 s to write the close
// frame, 5 s to wait for the answer), so a session Close stops waiting for
// still ends, and releases its admission, shortly after (closeRealtime names
// the one case it does not bound).
const realtimeCloseBound = 2 * time.Second

// codeRealtimeUpgradeRequired answers a GET /realtime/{model} that is not a
// WebSocket handshake.
const codeRealtimeUpgradeRequired = "runtime.realtime_upgrade_required"

// The close reasons the router itself sends. Reasons a peer sent are relayed
// as they are.
const (
	// realtimeReasonIdle is sglang-omni's own reason for its idle close, so a
	// client sees one shape whichever of the two timed the session out.
	realtimeReasonIdle    = "idle_timeout"
	realtimeReasonClosing = "router closing"
	realtimeReasonTooBig  = "message too big"
)

// realtimeSide names one end of a realtime session.
type realtimeSide int

const (
	realtimeSideNone   realtimeSide = iota // the router itself ended the session
	realtimeSideClient                     // the inbound socket (the gateway)
	realtimeSideChild                      // the managed child's socket
)

// lostReason is the close reason the other side gets when this one is lost
// without a close frame.
func (s realtimeSide) lostReason() string {
	if s == realtimeSideChild {
		return "child connection lost"
	}
	return "client connection lost"
}

// other is the opposite side.
func (s realtimeSide) other() realtimeSide {
	if s == realtimeSideChild {
		return realtimeSideClient
	}
	return realtimeSideChild
}

// realtimeEnd is why a realtime session ended: the status and reason both sides
// are closed with, and the side whose socket caused it (realtimeSideNone when
// the router ended the session itself). The cause's socket is already closed,
// or has had its close frame sent by coder/websocket, so it is closed without a
// handshake.
type realtimeEnd struct {
	code   websocket.StatusCode
	reason string
	cause  realtimeSide
}

// realtimeSessions tracks the open realtime sessions of one router, so Close can
// end them: a session runs on a hijacked connection, which http.Server.Close
// neither sees nor closes. stop is canceled by Close and ends every session;
// wg counts the sessions whose handlers have not returned. closed refuses new
// sessions once Close has run, under mu, so no wg.Add can follow Close's Wait.
type realtimeSessions struct {
	mu     sync.Mutex
	closed bool
	wg     sync.WaitGroup
	stop   context.Context
	cancel context.CancelFunc
}

func newRealtimeSessions() *realtimeSessions {
	stop, cancel := context.WithCancel(context.Background())
	return &realtimeSessions{stop: stop, cancel: cancel}
}

// begin counts a new session in, or refuses it once the router is closed.
func (rs *realtimeSessions) begin() bool {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if rs.closed {
		return false
	}
	rs.wg.Add(1)
	return true
}

// finish counts a session out; its handler is about to return.
func (rs *realtimeSessions) finish() {
	rs.wg.Done()
}

// close refuses new sessions, ends the open ones, and returns a channel that is
// closed once every one of them has finished.
func (rs *realtimeSessions) close() <-chan struct{} {
	rs.mu.Lock()
	rs.closed = true
	rs.cancel()
	rs.mu.Unlock()
	done := make(chan struct{})
	go func() {
		rs.wg.Wait()
		close(done)
	}()
	return done
}

// Close ends every realtime session with 1001 and refuses new ones. It waits
// for them at most realtimeCloseWait (realtimeCloseBound): coder/websocket
// bounds each close handshake itself, so a session whose peer does not answer
// still ends, and releases its admission, within seconds. The Driver calls
// it whenever it retires a router (Driver.Close, an unbind, a rebind to another
// port). It is idempotent, and a nil router is a no-op.
func (rt *router) Close() {
	if rt == nil {
		return
	}
	done := rt.realtime.close()
	bound := time.NewTimer(rt.realtimeCloseWait)
	defer bound.Stop()
	select {
	case <-done:
	case <-bound.C:
		slog.Warn("runtime: realtime sessions are still closing after the router's close bound; they end on their own close handshake bound",
			"bound", rt.realtimeCloseWait)
	}
}

// serveRealtime handles GET /realtime/{model}: a WebSocket relay to a managed
// child's /v1/realtime (sglang-omni with --enable-realtime), held for the whole
// session. It is the router's one route that upgrades a connection, so it is
// the one route that must solve what agent-runtime-manager §4.5 refuses an
// upgrade over everywhere else: a hijacked connection has no write deadline,
// is invisible to http.Server.Close, and would keep the admission's release()
// from ever running.
//
// Deliberate properties:
//   - A request that is not a WebSocket handshake (Connection: upgrade plus
//     Upgrade: websocket) is 400 runtime.realtime_upgrade_required before
//     anything is admitted. The model is the decoded PATH after "/realtime/",
//     exactly as /live reads it; an empty model or a nil manager is 404
//     runtime.model_not_managed, and so is an unknown model (EnsureServing).
//   - The model is admitted with EnsureServing and its address read again once
//     admitted (ensureServing, addressMoved), exactly like /live: an instance
//     address "m#k" reaches instance k's child, which is handed the base name m.
//     release() is held until the session has ended on both sides, and runs
//     exactly once on every path.
//   - The child is dialed BEFORE the inbound socket is accepted, at
//     {endpoint}/v1/realtime with the inbound query and model= set to the
//     child's model (realtimeChildQuery). A dial that fails -- the child is
//     unreachable, or answers the handshake with anything but 101, as
//     sglang-omni answers 404 without --enable-realtime -- is a plain HTTP 502
//     runtime.upstream_gone, and nothing is upgraded.
//   - coder/websocket hijacks the server's own writer. The route never sees a
//     deadlineWriter, whose Hijack refuses by design and keeps refusing for
//     every other route. The session bounds itself instead: an idle timeout
//     (realtimeIdle), a message limit (realtimeMessageLimit) on both sides, and
//     the router's Close, which ends every session (realtimeSessions).
//   - Until the inbound socket is accepted, the client leaving or the router
//     closing abandons the admission and the dial (the request context and the
//     router's stop). From then on the session ends only as relayRealtime
//     describes.
func (rt *router) serveRealtime(w http.ResponseWriter, r *http.Request) {
	if !isWebSocketUpgrade(r) {
		writeError(w, http.StatusBadRequest, codeRealtimeUpgradeRequired, "GET /realtime/{model} must be a WebSocket handshake")
		return
	}
	model := strings.TrimPrefix(r.URL.Path, realtimePathPrefix)
	if rt.m == nil || model == "" {
		writeSentinelError(w, ErrModelNotManaged)
		return
	}
	if !rt.realtime.begin() {
		writeError(w, http.StatusBadGateway, codeUpstreamGone, "the runtime router is closing")
		return
	}
	defer rt.realtime.finish()

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	stopWatch := context.AfterFunc(rt.realtime.stop, cancel)
	defer stopWatch()

	addr := rt.readAddress(model)
	childModel := model
	if base, ok := addr.childModel(); ok {
		childModel = base
	}
	endpoint, release, err := rt.ensureServing(ctx, addr)
	if err != nil {
		writeSentinelError(w, err)
		return
	}
	defer release()

	child, err := rt.dialRealtimeChild(ctx, endpoint, r, childModel)
	if err != nil {
		writeError(w, http.StatusBadGateway, codeUpstreamGone, err.Error())
		return
	}
	client, err := websocket.Accept(w, r, nil)
	if err != nil {
		// Accept has answered the client itself. Nothing was relayed, and
		// nothing reads the child's socket, so it is dropped rather than closed
		// with a handshake (closeRealtime explains why that matters).
		_ = child.CloseNow()
		return
	}
	// The session is up: from here the router's stop ends it through a close
	// handshake (relayRealtime), not by canceling the dial's context.
	stopWatch()
	client.SetReadLimit(realtimeMessageLimit)
	end := rt.relayRealtime(client, child)
	slog.Debug("runtime: realtime session ended", "model", model, "code", int(end.code))
}

// dialRealtimeChild dials the child's realtime endpoint over the router's
// transport. The client has no timeout (the session outlives any), follows no
// redirect (the router never does), and hands the child the inbound headers
// minus the handshake's own (realtimeChildHeader).
func (rt *router) dialRealtimeChild(ctx context.Context, endpoint string, r *http.Request, childModel string) (*websocket.Conn, error) {
	target := endpoint + realtimeChildPath + "?" + realtimeChildQuery(r.URL.RawQuery, childModel)
	conn, resp, err := websocket.Dial(ctx, target, &websocket.DialOptions{
		HTTPClient: &http.Client{
			Transport: rt.transport,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		HTTPHeader: realtimeChildHeader(r.Header),
	})
	if err != nil {
		if resp != nil {
			return nil, fmt.Errorf("runtime: the child answered the realtime handshake with %d: %w", resp.StatusCode, err)
		}
		return nil, fmt.Errorf("runtime: dial the child's realtime endpoint: %w", err)
	}
	conn.SetReadLimit(realtimeMessageLimit)
	return conn, nil
}

// realtimeChildQuery builds the child's query for serveRealtime by /live's rule
// (liveChildQuery): the inbound params as sent, with model set to the child's
// model -- the path model, or its base name for an instance address -- so a
// stray inbound model= is replaced, never appended. Unlike /live it is encoded
// normally: sglang-omni decodes its query, so there is no verbatim quirk to
// serve.
func realtimeChildQuery(inbound, model string) string {
	values, _ := url.ParseQuery(inbound) // partial parse on error is fine; model is set authoritatively below
	values.Set("model", model)
	return values.Encode()
}

// realtimeChildHeader is the header the router dials the child with: the
// inbound header minus the hop-by-hop fields, the inbound handshake's own
// Sec-WebSocket-* fields (coder/websocket writes its own; a forwarded offer of
// a subprotocol or an extension the router does not speak would make the
// child's answer fail the dial), and Origin (the router, not a browser, is the
// child's client, and an Origin naming the router's host would fail a child
// that checks it).
func realtimeChildHeader(in http.Header) http.Header {
	h := in.Clone()
	for _, k := range hopByHopHeaders {
		h.Del(k)
	}
	h.Del("Origin")
	for k := range h {
		if strings.HasPrefix(k, "Sec-Websocket-") {
			delete(h, k)
		}
	}
	return h
}

// isWebSocketUpgrade reports whether r asks for a WebSocket upgrade. The
// remaining handshake checks (Sec-WebSocket-Version, Sec-WebSocket-Key) are
// websocket.Accept's, which answers a malformed handshake itself.
func isWebSocketUpgrade(r *http.Request) bool {
	return headerHasToken(r.Header, "Connection", "upgrade") && headerHasToken(r.Header, "Upgrade", "websocket")
}

// headerHasToken reports whether one of key's comma-separated values is token,
// in any letter case.
func headerHasToken(h http.Header, key, token string) bool {
	for _, v := range h.Values(key) {
		for t := range strings.SplitSeq(v, ",") {
			if strings.EqualFold(strings.TrimSpace(t), token) {
				return true
			}
		}
	}
	return false
}

// relayRealtime pumps messages between client and child until the session ends,
// closes both sides, and returns why it ended. One goroutine per direction reads
// a message from one side and writes it to the other with its type (text or
// binary); this goroutine watches them, the idle timer and the router's stop.
//
// The session ends at the first of:
//   - a side's close frame: the other side is closed with the same status and
//     reason. A status that may not be sent on becomes 1011
//     (sendableCloseCode); a close frame without a status is relayed as one.
//   - a side lost without a close frame (a read or write error): the other side
//     is closed with 1011.
//   - a message over realtimeMessageLimit: coder/websocket has already sent the
//     offending side 1009, and the other side is closed with 1009 too.
//   - realtimeIdle without a message in either direction (pings do not count):
//     both sides are closed with 1008 "idle_timeout".
//   - the router's Close: both sides are closed with 1001 "router closing".
//
// It returns only once both sockets are closed and both pumps have exited, so
// the caller's release() runs after the session is over on both sides
// (closeRealtime says how long a close can take).
func (rt *router) relayRealtime(client, child *websocket.Conn) realtimeEnd {
	activity := make(chan struct{}, 1)
	ends := make(chan realtimeEnd, 2) // each pump reports exactly once, so a report never blocks
	var pumps sync.WaitGroup
	pumps.Add(2)
	go func() {
		defer pumps.Done()
		pumpRealtime(client, child, realtimeSideClient, activity, ends)
	}()
	go func() {
		defer pumps.Done()
		pumpRealtime(child, client, realtimeSideChild, activity, ends)
	}()

	idle := time.NewTimer(rt.realtimeIdle)
	defer idle.Stop()
	var end realtimeEnd
wait:
	for {
		select {
		case <-activity:
			idle.Reset(rt.realtimeIdle)
		case end = <-ends:
			break wait
		case <-idle.C:
			end = realtimeEnd{code: websocket.StatusPolicyViolation, reason: realtimeReasonIdle, cause: realtimeSideNone}
			break wait
		case <-rt.realtime.stop.Done():
			end = realtimeEnd{code: websocket.StatusGoingAway, reason: realtimeReasonClosing, cause: realtimeSideNone}
			break wait
		}
	}

	closeRealtime(client, child, end)
	pumps.Wait() // both sockets are closed now, so both pumps' reads fail at once
	return end
}

// pumpRealtime relays messages from src to dst until either fails, reports why
// on ends exactly once, and returns once src can no longer be read. Each message
// read signals activity without blocking.
//
// A failed write ends the session but not the reading: the pump goes on reading
// src, discarding, until src is closed, so src keeps a reader for its close
// handshake (closeRealtime). The context is never canceled: a canceled read or
// write makes coder/websocket drop the connection without a close frame, and
// the session closes its sockets with one instead.
func pumpRealtime(src, dst *websocket.Conn, from realtimeSide, activity chan<- struct{}, ends chan<- realtimeEnd) {
	ctx := context.Background()
	for {
		typ, data, err := src.Read(ctx)
		if err != nil {
			ends <- realtimeEndOfRead(from, err)
			return
		}
		select {
		case activity <- struct{}{}:
		default:
		}
		if err := dst.Write(ctx, typ, data); err != nil {
			to := from.other()
			ends <- realtimeEnd{code: websocket.StatusInternalError, reason: to.lostReason(), cause: to}
			for {
				if _, _, err := src.Read(ctx); err != nil {
					return
				}
			}
		}
	}
}

// realtimeEndOfRead maps the error that ended a read from side to the session's
// end: a close frame's own status and reason, 1009 for a message over the limit,
// and 1011 for a side lost without a close frame.
func realtimeEndOfRead(side realtimeSide, err error) realtimeEnd {
	var ce websocket.CloseError
	switch {
	case errors.As(err, &ce):
		return realtimeEnd{code: sendableCloseCode(ce.Code), reason: ce.Reason, cause: side}
	case errors.Is(err, websocket.ErrMessageTooBig):
		return realtimeEnd{code: websocket.StatusMessageTooBig, reason: realtimeReasonTooBig, cause: side}
	default:
		return realtimeEnd{code: websocket.StatusInternalError, reason: side.lostReason(), cause: side}
	}
}

// sendableCloseCode is code when a close frame may carry it on to the other
// side (RFC 6455 §7.4: 1000-1003, 1007-1014, 3000-4999), and 1011 otherwise.
// 1005, "no status received", is kept: coder/websocket sends it as a close
// frame without a status, which is exactly what the peer sent.
func sendableCloseCode(code websocket.StatusCode) websocket.StatusCode {
	switch {
	case code == websocket.StatusNoStatusRcvd,
		code >= websocket.StatusNormalClosure && code <= websocket.StatusUnsupportedData,
		code >= websocket.StatusInvalidFramePayloadData && code <= websocket.StatusBadGateway,
		code >= 3000 && code <= 4999:
		return code
	default:
		return websocket.StatusInternalError
	}
}

// closeRealtime closes both sides of a session for end, at the same time, and
// returns once both are closed: the side that caused the end without a
// handshake (it has its close frame already, or is gone), any other with end's
// status and reason.
//
// coder/websocket's Close writes the close frame within 5 s and waits at most
// 5 s for the reader, then closes the connection whatever the answer. What it
// does not bound is reading, itself, the rest of a frame left unfinished: a
// peer that stalls mid-frame keeps it there until the peer's connection fails.
// So the side whose read ended -- the one socket nothing reads any more, and
// the only one that can have the unsent rest of an oversized message pending
// -- is the cause and is dropped, and every other socket keeps its pump reading
// it until it is closed (pumpRealtime). What remains unbounded is a peer that
// stalls mid-frame exactly while a close handshake holds the reader, which
// lasts until that peer's connection fails (its process ends, or TCP gives up).
func closeRealtime(client, child *websocket.Conn, end realtimeEnd) {
	var wg sync.WaitGroup
	closeSide := func(conn *websocket.Conn, side realtimeSide) {
		defer wg.Done()
		if side == end.cause {
			_ = conn.CloseNow()
			return
		}
		_ = conn.Close(end.code, end.reason)
	}
	wg.Add(2)
	go closeSide(client, realtimeSideClient)
	go closeSide(child, realtimeSideChild)
	wg.Wait()
}
