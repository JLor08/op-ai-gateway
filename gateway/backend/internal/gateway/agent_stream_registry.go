// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// agentStreamQueueCapacity bounds a single agent connection's outbound frame
// queue (agentStreamConn.out). A cert_update doorbell is tiny (well under 100
// bytes) and infrequent (at most once per certificate lifetime, plus rare
// retries), so 8 is generous slack; a queue still full past that means the
// connection cannot drain, and enqueue drops rather than blocks the caller --
// see NotifyCertUpdate.
const agentStreamQueueCapacity = 8

// agentSocket is the narrow slice of *websocket.Conn that agentStreamConn's
// writer goroutine needs: a data write, a keepalive ping, and the abrupt-close
// reap. A real *websocket.Conn satisfies it (see newAgentStreamConn); tests
// substitute a fake so a write or ping failure can be injected deterministically
// without tearing down a real network connection.
type agentSocket interface {
	Write(ctx context.Context, typ websocket.MessageType, p []byte) error
	Ping(ctx context.Context) error
	CloseNow() error
}

// agentStreamConn is one open agent WebSocket connection's OUTBOUND side: a
// non-blocking queue of already-marshaled frames, drained by EXACTLY ONE writer
// goroutine (runWriter, started by handleAgentStream) that ALSO owns the
// connection's keepalive ping -- so every write to this connection, data or
// control, is serialized through one place. This is the "genau EIN Writer pro
// WS-Verbindung" global constraint from the Phase 2 distribution plan: the
// shutdown watcher in handleAgentStream still writes the single close frame at
// the very end of the connection's life, but no data frame or ping is ever
// written from anywhere else.
//
// pingInterval/pingTimeout/writeTimeout are FIELDS rather than the package
// constants (agentStreamPingInterval/agentStreamPingTimeout/
// agentStreamWriteTimeout) used directly, solely so a test can drive
// millisecond-scale values instead of waiting out the real 30s/10s/5s;
// newAgentStreamConn always seeds them from those constants, so production
// behaviour is byte-identical to before this change. Honesty check: the
// keepalive ping path itself had NO dedicated test coverage before this
// feature -- nothing here should be read as having previously pinned its
// timing.
type agentStreamConn struct {
	conn agentSocket
	out  chan []byte

	pingInterval time.Duration
	pingTimeout  time.Duration
	writeTimeout time.Duration
}

// newAgentStreamConn wraps a real, already-upgraded connection with the
// production timings.
func newAgentStreamConn(conn *websocket.Conn) *agentStreamConn {
	return &agentStreamConn{
		conn:         conn,
		out:          make(chan []byte, agentStreamQueueCapacity),
		pingInterval: agentStreamPingInterval,
		pingTimeout:  agentStreamPingTimeout,
		writeTimeout: agentStreamWriteTimeout,
	}
}

// enqueue queues b for the writer goroutine. Non-blocking (select+default): a
// full queue drops the frame and reports false rather than block the caller.
// This is load-bearing for NotifyCertUpdate, which is called SYNCHRONOUSLY from
// inside portal.Service.issueAndStore while it holds certMu for the whole
// certificate reconcile pass -- a blocking send here could stall certificate
// issuance for every other server for as long as this one connection's peer
// stays stuck.
func (c *agentStreamConn) enqueue(b []byte) bool {
	select {
	case c.out <- b:
		return true
	default:
		return false
	}
}

// runWriter is the connection's SOLE writer: every queued data frame AND every
// keepalive ping goes through this one goroutine and one ticker, so writes to
// the connection are always ordered and never concurrent with each other. It
// returns either when done is closed (the read loop in handleAgentStream
// exited) or the first time a write or ping fails (a dead/half-open peer),
// reaping the connection via CloseNow in the latter case -- which unblocks the
// read loop's blocked Read, so it also returns and lets handleAgentStream's
// deferred AgentStreamRegistry.remove run.
//
// A data-write failure is Debug-logged (the ping failure is not, matching the
// pre-existing keepalive goroutine this replaces -- see the type doc's honesty
// note: that path was never logged before either). Both failure branches MUST
// reap-and-return, never a bare `return`: dropping the reap would silently
// strand this goroutine parked on <-done forever (the liveness watchdog this
// goroutine implements would then do nothing) and leak the registry entry,
// since nothing else would ever call remove for a connection whose read loop
// never sees an error either.
func (c *agentStreamConn) runWriter(done <-chan struct{}) {
	ticker := time.NewTicker(c.pingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case b := <-c.out:
			ctx, cancel := context.WithTimeout(context.Background(), c.writeTimeout)
			err := c.conn.Write(ctx, websocket.MessageText, b)
			cancel()
			if err != nil {
				slog.Debug("agent stream: data write failed, reaping connection", "err", err)
				_ = c.conn.CloseNow()
				return
			}
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(context.Background(), c.pingTimeout)
			err := c.conn.Ping(ctx)
			cancel()
			if err != nil {
				_ = c.conn.CloseNow()
				return
			}
		}
	}
}

// AgentStreamRegistry tracks currently-open agent WebSocket connections, keyed
// by server id, so a certificate reconcile pass can push a best-effort
// cert_update doorbell to every open connection for the server whose
// certificate just changed (see NotifyCertUpdate) -- the Phase 2 distribution
// trigger. Multiple connections per server are expected during a reconnect
// overlap, so each server id maps to a SET of connections; all of them get the
// doorbell.
//
// Exported (the type, its constructor, and NotifyCertUpdate) because
// cmd/gateway -- which wires this into portal.ServiceDeps.OnCertificateIssued
// -- is a DIFFERENT package and must be able to name the type, construct it,
// and call the one push method. add/remove stay unexported: only
// agent_stream.go (same package) ever registers or deregisters a connection.
// Keeping the frame's wire type (certUpdateFrame / "cert_update") off the
// exported surface too is deliberate: NotifyCertUpdate's narrow signature
// (serverID, fingerprint string) is the only way anything outside this package
// can make the gateway push a frame to an agent, and it cannot be made to
// carry anything else.
type AgentStreamRegistry struct {
	mu    sync.RWMutex
	conns map[string]map[*agentStreamConn]struct{}
}

// NewAgentStreamRegistry constructs an empty registry.
func NewAgentStreamRegistry() *AgentStreamRegistry {
	return &AgentStreamRegistry{conns: make(map[string]map[*agentStreamConn]struct{})}
}

// add registers c under serverID. No-op on a nil registry, an empty id, or a
// nil connection.
func (r *AgentStreamRegistry) add(serverID string, c *agentStreamConn) {
	if r == nil || serverID == "" || c == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	set, ok := r.conns[serverID]
	if !ok {
		set = make(map[*agentStreamConn]struct{})
		r.conns[serverID] = set
	}
	set[c] = struct{}{}
}

// remove deregisters c. When c was the LAST connection registered for
// serverID, the now-empty inner set is deleted too, along with the outer map
// entry -- otherwise the outer map would grow by one entry for every distinct
// server id the gateway ever saw a connection from, even long after that
// server (or its last connection) is gone.
func (r *AgentStreamRegistry) remove(serverID string, c *agentStreamConn) {
	if r == nil || serverID == "" || c == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	set, ok := r.conns[serverID]
	if !ok {
		return
	}
	delete(set, c)
	if len(set) == 0 {
		delete(r.conns, serverID)
	}
}

// certUpdateFrame is the ENTIRE wire payload of a cert_update frame: a
// doorbell, never a command. The gateway must NEVER be able to deliver an
// executable instruction to an agent (see the Phase 2 plan's Global
// Constraints) -- a second field on this struct (a path, a shell fragment,
// anything beyond the fingerprint the agent already needs to decide whether to
// re-fetch) would violate that boundary.
// TestCertUpdateFramePayloadIsExactlyFingerprint pins the exact key set
// against exactly that mutation.
type certUpdateFrame struct {
	Fingerprint string `json:"fingerprint"`
}

type caUpdateFrame struct {
	Fingerprint string `json:"fingerprint"`
}

// marshalStreamFrame marshals v into the wire streamFrame{Type,Data} envelope
// (see agent_stream.go) as bytes, doing NO network I/O -- pure CPU. This split
// (marshal here, enqueue in NotifyCertUpdate, write in runWriter) is what lets
// NotifyCertUpdate be called synchronously from inside
// portal.Service.issueAndStore, which holds certMu for the whole certificate
// reconcile pass: a network write at that call site could stall certificate
// issuance for every other server on one slow or stuck agent peer.
func marshalStreamFrame(frameType string, v any) ([]byte, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return json.Marshal(streamFrame{Type: frameType, Data: data})
}

// NotifyCertUpdate is the gateway's ONE outbound push, and the only production
// caller of marshalStreamFrame and agentStreamConn.enqueue: it tells every
// currently-open agent connection for serverID that a fresh certificate
// (identified by fingerprint) is ready to fetch. It performs NO network write
// itself -- only a JSON marshal and a non-blocking channel send per connection,
// under r.mu's READ lock only -- so it can never block its caller regardless of
// how slow or stuck any agent peer is. See issueAndStore (internal/portal/
// service_certificates.go), which calls this while holding portal.Service's
// certMu for the whole reconcile pass.
//
// Best-effort throughout, by design: an unknown or offline server, a full
// queue, or a later write failure on the connection all fail SILENTLY here --
// there is deliberately no error return. The agent picks up the new
// certificate on its own next poll or (re)connect regardless of whether this
// doorbell ever arrives; a notification failure must never surface as a
// certificate reconcile error.
func (r *AgentStreamRegistry) NotifyCertUpdate(serverID, fingerprint string) {
	if r == nil || serverID == "" {
		return
	}
	b, err := marshalStreamFrame("cert_update", certUpdateFrame{Fingerprint: fingerprint})
	if err != nil {
		slog.Debug("agent stream: cert_update marshal failed", "server_id", serverID, "err", err)
		return
	}
	r.mu.RLock()
	set := r.conns[serverID]
	// Snapshot the live connections under the lock, then enqueue OUTSIDE it:
	// enqueue itself never blocks, but there is no reason to hold r.mu -- which
	// every add/remove also needs -- for one instant longer than reading the set.
	conns := make([]*agentStreamConn, 0, len(set))
	for c := range set {
		conns = append(conns, c)
	}
	r.mu.RUnlock()
	for _, c := range conns {
		if !c.enqueue(b) {
			slog.Debug("agent stream: cert_update queue full, frame dropped", "server_id", serverID)
		}
	}
}

func (r *AgentStreamRegistry) NotifyCAUpdate(fingerprint string) {
	if r == nil || fingerprint == "" {
		return
	}
	b, err := marshalStreamFrame("ca_update", caUpdateFrame{Fingerprint: fingerprint})
	if err != nil {
		slog.Debug("agent stream: ca_update marshal failed", "err", err)
		return
	}
	r.mu.RLock()
	conns := make([]*agentStreamConn, 0)
	for _, set := range r.conns {
		for c := range set {
			conns = append(conns, c)
		}
	}
	r.mu.RUnlock()
	for _, c := range conns {
		if !c.enqueue(b) {
			slog.Debug("agent stream: ca_update queue full, frame dropped")
		}
	}
}
