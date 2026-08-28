// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

// maxAgentFrameBytes caps a single inbound WebSocket frame, mirroring the POST
// path's maxJSONBodyBytes: a telemetry snapshot is a few KiB, so 1 MiB is generous.
const maxAgentFrameBytes int64 = 1 << 20

const (
	// agentStreamPingInterval is how often the gateway pings an idle agent stream so
	// a silent/half-open connection is detected and reaped.
	agentStreamPingInterval = 30 * time.Second
	// agentStreamPingTimeout bounds the wait for a pong; a miss reaps the connection.
	agentStreamPingTimeout = 10 * time.Second
	// agentStreamWriteTimeout bounds a single data-frame write to an agent
	// connection (agentStreamConn.runWriter). Deliberately SHORTER than
	// agentStreamPingTimeout: coder/websocket reserves a fixed 5s budget of its
	// own for writing the close frame on shutdown, and a longer in-flight data
	// write could swallow the shutdown watcher's GoingAway close entirely.
	agentStreamWriteTimeout = 5 * time.Second
)

// streamFrame is the typed JSON envelope carried over the agent WebSocket. Type
// discriminates the payload; Data holds the raw payload (a telemetry snapshot for
// the only shipped type). Unknown types are ignored (forward-compat), so new frame
// types (and a future gateway->agent direction) can be added without a wire break.
type streamFrame struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data,omitempty"`
}

// handleAgentStream upgrades an agent connection to a WebSocket and ingests telemetry
// frames over it, feeding the SAME pipeline as the POST path (ingestTelemetrySample).
// Auth (bearer -> agent token -> serverID) happens BEFORE the upgrade, so a
// bad/anonymous request gets a normal HTTP 401/500 (no upgrade). A shutdown watcher
// translates either process shutdown (s.baseCtx) or this listener bind's request
// context cancellation into a clean 1001 GoingAway.
func (s *Server) handleAgentStream(w http.ResponseWriter, r *http.Request) {
	// 1. Auth before upgrade (shared by every agent endpoint).
	principal, ok := s.authenticateAgent(w, r)
	if !ok {
		return
	}
	serverID := principal.ServerID

	// 2. Upgrade. The agent is a programmatic (non-browser) client, so origin checks
	// are irrelevant; the bearer already authenticated it. Accept writes its own
	// error response, so on failure just return.
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		slog.Debug("agent stream: websocket accept failed", "server_id", serverID, "err", err)
		return
	}
	conn.SetReadLimit(maxAgentFrameBytes)
	defer func() { _ = conn.CloseNow() }()

	// done is closed when the read loop exits; it stops the background goroutines.
	done := make(chan struct{})
	defer close(done)

	// 3. Shutdown watcher: translate a server shutdown (s.baseCtx cancel) into a clean
	// 1001 GoingAway. We use conn.Close (a proper close handshake) rather than binding
	// the read to s.baseCtx, because a cancelled read context triggers coder/websocket's
	// abrupt teardown (c.close()) BEFORE a close frame can be written. This is the ONLY
	// place that writes a close frame, and it writes exactly one, at the very end of
	// the connection's life -- every other write to this connection (data frames AND
	// keepalive pings) goes exclusively through the writer goroutine started below
	// (agentStreamConn.runWriter), never through this watcher.
	go func() {
		select {
		case <-s.baseCtx.Done():
			_ = conn.Close(websocket.StatusGoingAway, "gateway restart")
		case <-r.Context().Done():
			_ = conn.Close(websocket.StatusGoingAway, "gateway listener restart")
		case <-done:
		}
	}()

	// 4. Register this connection so a certificate reconcile pass can push it a
	// cert_update doorbell (AgentStreamRegistry.NotifyCertUpdate), and start its
	// SOLE writer goroutine: every data frame AND the keepalive ping both go
	// through runWriter, so writes to this connection are always serialized (see
	// agent_stream_registry.go). Deregistered via defer once the read loop below
	// returns and done is closed.
	sc := newAgentStreamConn(conn)
	s.AgentStreams.add(serverID, sc)
	defer s.AgentStreams.remove(serverID, sc)
	go sc.runWriter(done)

	// Restate which specs' output this agent should be streaming, on EVERY
	// new connection and unconditionally -- including the empty set, which is
	// the common case and is not a no-op here. Two things depend on it:
	//
	//   - an operator whose log view was open across an agent restart or a
	//     reconnect gets the stream back (with a fresh scrollback) without
	//     having to close and reopen it;
	//   - an agent that was streaming before the drop, but whose viewers have
	//     since gone, is told to stop, so a lost unsubscribe can never leave
	//     it streaming to nobody for the rest of its uptime.
	//
	// This is also why the agent needs no connect-time reset of its own: what
	// the gateway says on a connection is authoritative for that connection.
	s.AgentStreams.NotifyRuntimeLogWatch(serverID, s.RuntimeLogs.watched(serverID))

	slog.Debug("agent stream opened", "server_id", serverID)
	// 5. Read loop. A background context (not s.baseCtx) so a shutdown cancel does not
	// abruptly tear the connection down before the watcher's GoingAway frame is sent.
	for {
		var f streamFrame
		if err := wsjson.Read(context.Background(), conn, &f); err != nil {
			// The connection closed: client close, keepalive/shutdown close, or a
			// decode/transport error. Nothing more to do; defer CloseNow cleans up.
			slog.Debug("agent stream closed", "server_id", serverID, "err", err)
			return
		}
		switch f.Type {
		case "telemetry":
			var req agentTelemetryRequest
			if err := json.Unmarshal(f.Data, &req); err != nil {
				// A malformed payload skips the frame but keeps the stream (latest-wins).
				slog.Debug("agent stream: invalid telemetry frame", "server_id", serverID, "err", err)
				continue
			}
			// context.Background(): a shutdown must not turn an in-flight ingest into a
			// store error (which would close with 1011 instead of the watcher's 1001).
			if err := s.ingestTelemetrySample(context.Background(), serverID, req, f.Data); err != nil {
				switch {
				case errors.Is(err, errAgentTelemetryInvalid):
					continue // skip the bad frame, keep streaming
				case errors.Is(err, ErrAgentServerMismatch), errors.Is(err, errAgentUnknownServer):
					_ = conn.Close(websocket.StatusPolicyViolation, "server mismatch")
					return
				default: // store error
					_ = conn.Close(websocket.StatusInternalError, "ingest failed")
					return
				}
			}
		case "system_report":
			// context.Background(): a shutdown must not turn an in-flight ingest into a
			// store error (which would close with 1011 instead of the watcher's 1001).
			if err := s.ingestSystemReport(context.Background(), serverID, f.Data); err != nil {
				switch {
				case errors.Is(err, errAgentSystemReportInvalid):
					slog.Debug("agent stream: invalid system_report frame", "server_id", serverID, "err", err)
					continue // skip the bad frame, keep streaming
				case errors.Is(err, errAgentUnknownServer):
					_ = conn.Close(websocket.StatusPolicyViolation, "unknown server")
					return
				default: // store error
					_ = conn.Close(websocket.StatusInternalError, "system report ingest failed")
					return
				}
			}
		case "runtime_report":
			// context.Background(): a shutdown must not turn an in-flight ingest into a
			// store error (which would close with 1011 instead of the watcher's 1001).
			if err := s.ingestRuntimeReport(context.Background(), serverID, f.Data); err != nil {
				switch {
				case errors.Is(err, errAgentRuntimeReportInvalid):
					slog.Debug("agent stream: invalid runtime_report frame", "server_id", serverID, "err", err)
					continue // skip the bad frame, keep streaming
				case errors.Is(err, errAgentUnknownServer):
					_ = conn.Close(websocket.StatusPolicyViolation, "unknown server")
					return
				default: // store error
					_ = conn.Close(websocket.StatusInternalError, "runtime report ingest failed")
					return
				}
			}
		case "runtime_log":
			// Relay only: parsed, clamped, fanned out to whoever is watching,
			// and forgotten. NOTHING on this path touches the store, and no
			// branch of it logs the payload or any fragment of it -- managed
			// process output can contain prompt text (see runtime_logs.go).
			// A malformed frame is skipped silently for the same reason a
			// malformed telemetry frame is: latest-wins, keep streaming.
			s.ingestRuntimeLog(serverID, f.Data)
		default:
			// Unknown frame type: ignore for forward-compat.
			slog.Debug("agent stream: unknown frame type", "server_id", serverID, "type", f.Type)
		}
	}
}

// writeStreamFrame marshals v into a typed streamFrame and writes it directly to
// the agent over the WebSocket. It is NOT the path the gateway actually uses to
// push a frame to an agent -- that is AgentStreamRegistry.NotifyCertUpdate, which
// only marshals (via marshalStreamFrame) and enqueues onto the connection's
// agentStreamConn.out for its SOLE writer goroutine (agentStreamConn.runWriter) to
// send. A direct write here, from an arbitrary caller's goroutine, would violate
// the "exactly one writer per connection" invariant (see agent_stream_registry.go)
// by racing with that writer's data frames and keepalive pings. Kept, with its
// existing test, as a small reusable synchronous-write helper and a hook for a
// possible future frame type that genuinely needs one; unused by production code
// in this feature.
func writeStreamFrame(ctx context.Context, conn *websocket.Conn, frameType string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return wsjson.Write(ctx, conn, streamFrame{Type: frameType, Data: data})
}
