// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

// This file adds a WebSocket telemetry sender alongside the POST client. Both
// implement the agent's poster contract (Post(ctx, *sample.Sample) error); main.go
// picks one from cfg.Transport.
package client

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/rand"
	"net/http"
	"net/url"
	"op-ai-server-agent/internal/gwapi"
	"op-ai-server-agent/internal/sample"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

// streamPath is the gateway WebSocket route (mirrors telemetryPath for the POST path).
const streamPath = "/api/agent/v1/stream"

// wsMaxFrameBytes caps a single frame (symmetry with the gateway's read limit).
const wsMaxFrameBytes int64 = 1 << 20

// streamFrame mirrors the gateway's typed WebSocket envelope
// ({"type":"telemetry","data":{…}}); Data is the marshaled sample.
type streamFrame struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data,omitempty"`
}

// WSSender streams telemetry over one persistent WebSocket. Post writes if connected,
// else dials once when the backoff window (D7) has elapsed — it never sleeps the
// collect loop for a full backoff. A background read loop auto-pongs the gateway's
// pings and detects a server-initiated close. All timings/rng are fields so tests can
// make reconnect deterministic + fast.
type WSSender struct {
	url        string
	token      string
	httpClient *http.Client

	backoffBase     time.Duration // base reconnect delay for a dial/IO error
	backoffCap      time.Duration // max reconnect delay
	cleanMin        time.Duration // min reconnect delay after a clean close
	cleanMax        time.Duration // max reconnect delay after a clean close
	stableThreshold time.Duration // a connection up longer than this resets the backoff
	dialTimeout     time.Duration
	writeTimeout    time.Duration
	pingInterval    time.Duration // liveness-probe cadence while connected
	pingTimeout     time.Duration // max wait for a pong before treating the peer as dead

	rng   *rand.Rand
	clock func() time.Time

	// certUpdates is buffered(1): a server->agent cert_update doorbell (see
	// readLoop) and every successful (re)connect (see maybeDial) each send a
	// non-blocking signal here. Buffering at 1 means a burst of either source
	// coalesces into a single pending wake rather than blocking the sender --
	// there is deliberately no queue depth beyond "one pending sync request".
	certUpdates  chan struct{}
	trustUpdates chan struct{}

	// runtimeUpdates is the one genuinely different doorbell shape in this
	// file: certUpdates/trustUpdates are content-free (chan struct{}) --
	// the payload is idempotent and cheap to re-fetch over HTTP, so a
	// coalesced "check again" is all a consumer ever needs. A pushed
	// runtime_config frame instead CARRIES the whole document, so this is a
	// latest-wins buffered(1) chan json.RawMessage: wakeRuntimeConfig always
	// drains any stale pending value before sending the newest one, so a
	// burst of pushes (or a burst with no reader draining at all) coalesces
	// into exactly the LAST document, never a stale earlier one. A nil
	// payload means "resync over HTTP" and is what maybeDial sends on every
	// successful (re)connect, mirroring how it already wakes certUpdates/
	// trustUpdates on connect.
	runtimeUpdates chan json.RawMessage

	mu          sync.Mutex
	conn        *websocket.Conn
	connDone    chan struct{} // closed by dropConn when this conn is torn down; stops pingLoop
	connectedAt time.Time
	nextDialAt  time.Time
	failures    int
	closed      bool
	sysReport   []byte // cached marshaled SystemReport, resent on each (re)connect
	// runtimeReport is the cached, already-redacted, already-marshaled
	// file-mode runtime report (internal/runtime.BuildReport), resent on
	// each (re)connect exactly like sysReport.
	runtimeReport []byte
}

// NewWSSender builds a WSSender for gatewayURL (http->ws, https->wss). A nil
// client uses the secure no-timeout legacy default for tests; production
// injects a no-timeout client backed by the shared dynamic trust store.
func NewWSSender(gatewayURL, token string, httpClient *http.Client) (*WSSender, error) {
	u, err := wsURLFromGateway(gatewayURL)
	if err != nil {
		return nil, err
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 0}
	}
	return &WSSender{
		url:             u,
		token:           token,
		httpClient:      httpClient,
		backoffBase:     500 * time.Millisecond,
		backoffCap:      30 * time.Second,
		cleanMin:        500 * time.Millisecond,
		cleanMax:        2 * time.Second,
		stableThreshold: 10 * time.Second,
		dialTimeout:     10 * time.Second,
		writeTimeout:    10 * time.Second,
		pingInterval:    30 * time.Second,
		pingTimeout:     10 * time.Second,
		rng:             rand.New(rand.NewSource(time.Now().UnixNano())),
		clock:           time.Now,
		certUpdates:     make(chan struct{}, 1),
		trustUpdates:    make(chan struct{}, 1),
		runtimeUpdates:  make(chan json.RawMessage, 1),
	}, nil
}

// CertUpdates returns a channel that receives a signal whenever the gateway
// pushes a cert_update doorbell (Phase 2 certificate distribution) OR this
// connection is (re)established -- both are "go check for a new certificate
// now" moments (see agent.Agent, which treats either as a wake to trigger a
// certinstall sync). Buffered(1) + non-blocking sends on the producing side
// (readLoop, maybeDial): a burst of either coalesces into one pending wake, so
// reading this channel is never required to keep the connection alive.
func (s *WSSender) CertUpdates() <-chan struct{} {
	return s.certUpdates
}

// TrustUpdates receives CA-refresh doorbells. It is deliberately separate
// from CertUpdates so neither consumer can steal the other's wake.
func (s *WSSender) TrustUpdates() <-chan struct{} { return s.trustUpdates }

// RuntimeUpdates returns a channel that receives the latest gateway-pushed
// runtime_config document (see the runtimeUpdates field doc) OR a nil
// payload every time this connection is (re)established -- a nil means
// "resync over HTTP" (the Task 18 consumer feeds either shape into the same
// runtime.GatewaySource.ApplyPushed / .Load reconciliation). Buffered(1)
// with a drain-then-send producer (wakeRuntimeConfig): a burst of pushes,
// with or without a reader ever draining in between, coalesces into exactly
// the newest document -- reading this channel is never required to keep the
// connection alive, and the producer (readLoop) never blocks on it.
func (s *WSSender) RuntimeUpdates() <-chan json.RawMessage { return s.runtimeUpdates }

// wakeCertUpdates is the shared non-blocking send both wake sources (readLoop's
// cert_update frame, maybeDial's connect success) use. A full channel (an
// already-pending, not-yet-consumed wake) means the send is simply dropped --
// there is nothing more to say than "check again", and a second wake before the
// first was even read adds no information.
func (s *WSSender) wakeCertUpdates() {
	select {
	case s.certUpdates <- struct{}{}:
	default:
	}
}

func (s *WSSender) wakeTrustUpdates() {
	select {
	case s.trustUpdates <- struct{}{}:
	default:
	}
}

// wakeRuntimeConfig is the latest-wins doorbell producer for
// RuntimeUpdates(): it unconditionally drains any stale pending document
// before sending the new one, so a consumer that has not yet read the
// previous value never observes it once a newer one has arrived. Called
// from readLoop (a genuine "runtime_config" frame, data = f.Data) and from
// maybeDial's connect hook (data = nil, meaning "resync over HTTP") -- both
// non-blocking, by construction: neither select here can ever block, since
// the channel is buffered(1) and the drain immediately preceding the send
// guarantees room for exactly one value. This is the load-bearing property
// for readLoop's caller: readLoop must NEVER block on this send, because it
// is also the goroutine that detects a dead peer (see readLoop's own doc).
func (s *WSSender) wakeRuntimeConfig(data json.RawMessage) {
	select {
	case <-s.runtimeUpdates: // drop the stale pending document, if any
	default:
	}
	select {
	case s.runtimeUpdates <- data:
	default:
	}
}

// wsURLFromGateway maps a gateway base URL to the ws(s) stream URL: http->ws,
// https->wss. A URL already in ws/wss form (as used by tests that construct the
// target directly from an httptest server) passes through unchanged, with the
// stream path normalized the same way. Any existing base path is PRESERVED and the
// stream path is appended to it (mirroring how the POST client composes
// gatewayURL+telemetryPath by string concatenation), so a gateway reachable at
// "https://gw/base" streams at "wss://gw/base/api/agent/v1/stream", not at the
// bare-root "wss://gw/api/agent/v1/stream".
func wsURLFromGateway(gatewayURL string) (string, error) {
	u, err := url.Parse(gwapi.TrimBase(gatewayURL))
	if err != nil {
		return "", err
	}
	switch u.Scheme {
	case "http", "ws":
		u.Scheme = "ws"
	case "https", "wss":
		u.Scheme = "wss"
	default:
		return "", fmt.Errorf("gateway URL must be http or https: %q", gatewayURL)
	}
	u.Path = strings.TrimRight(u.Path, "/") + streamPath
	u.RawQuery = ""
	u.Fragment = ""
	return u.String(), nil
}

// Post writes one telemetry frame, (re)dialing as needed. It never blocks longer than
// one dial/write timeout and never returns without either delivering or reporting a
// bounded failure (the caller loop logs + drops it — latest-wins telemetry).
func (s *WSSender) Post(ctx context.Context, sm *sample.Sample) error {
	sm.Normalize()
	data, err := json.Marshal(sm)
	if err != nil {
		return fmt.Errorf("marshal sample: %w", err)
	}

	conn := s.currentConn()
	if conn == nil {
		if err := s.maybeDial(ctx); err != nil {
			return err
		}
		conn = s.currentConn()
		if conn == nil {
			return errBackingOff
		}
	}

	wctx, cancel := context.WithTimeout(ctx, s.writeTimeout)
	defer cancel()
	if err := wsjson.Write(wctx, conn, streamFrame{Type: "telemetry", Data: data}); err != nil {
		s.dropConn(conn, isCleanClose(err))
		return fmt.Errorf("write telemetry frame: %w", err)
	}
	return nil
}

// PostSystemReport caches the marshaled report (for resend on every future
// reconnect) and writes it once on the current connection, dialing if needed. The
// dial path (maybeDial) itself resends the just-cached report on connect, so this
// avoids a double-send there.
func (s *WSSender) PostSystemReport(ctx context.Context, r *sample.SystemReport) error {
	r.Normalize()
	data, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("marshal system report: %w", err)
	}
	s.mu.Lock()
	s.sysReport = append([]byte(nil), data...)
	s.mu.Unlock()

	conn := s.currentConn()
	if conn == nil {
		if err := s.maybeDial(ctx); err != nil {
			return err
		}
		if s.currentConn() == nil {
			return errBackingOff
		}
		return nil // maybeDial already sent the cached report on connect
	}
	wctx, cancel := context.WithTimeout(ctx, s.writeTimeout)
	defer cancel()
	if err := wsjson.Write(wctx, conn, streamFrame{Type: "system_report", Data: data}); err != nil {
		s.dropConn(conn, isCleanClose(err))
		return fmt.Errorf("write system report frame: %w", err)
	}
	return nil
}

// resendSystemReport writes the cached hardware report on a freshly-dialed
// connection (best-effort; a write error is left for the ping/read loop to reap).
func (s *WSSender) resendSystemReport(conn *websocket.Conn) {
	s.mu.Lock()
	data := append([]byte(nil), s.sysReport...)
	s.mu.Unlock()
	if len(data) == 0 {
		return
	}
	wctx, cancel := context.WithTimeout(context.Background(), s.writeTimeout)
	defer cancel()
	if err := wsjson.Write(wctx, conn, streamFrame{Type: "system_report", Data: data}); err != nil {
		slog.Debug("ws system report resend failed", "err", err)
	}
}

// PostRuntimeReport caches an already-built, already-redacted file-mode
// runtime report (internal/runtime.BuildReport) for resend on every future
// reconnect, and writes it once on the current connection, dialing if
// needed -- the exact same shape as PostSystemReport/sysReport, just for the
// "runtime_report" frame type. raw is written byte-for-byte: redaction
// already happened at the point the report was built, so this transport
// must never re-marshal it.
func (s *WSSender) PostRuntimeReport(ctx context.Context, raw json.RawMessage) error {
	s.mu.Lock()
	s.runtimeReport = append([]byte(nil), raw...)
	s.mu.Unlock()

	conn := s.currentConn()
	if conn == nil {
		if err := s.maybeDial(ctx); err != nil {
			return err
		}
		if s.currentConn() == nil {
			return errBackingOff
		}
		return nil // maybeDial already sent the cached report on connect
	}
	wctx, cancel := context.WithTimeout(ctx, s.writeTimeout)
	defer cancel()
	if err := wsjson.Write(wctx, conn, streamFrame{Type: "runtime_report", Data: raw}); err != nil {
		s.dropConn(conn, isCleanClose(err))
		return fmt.Errorf("write runtime report frame: %w", err)
	}
	return nil
}

// resendRuntimeReport writes the cached runtime report on a freshly-dialed
// connection (best-effort; a write error is left for the ping/read loop to
// reap), mirroring resendSystemReport exactly.
func (s *WSSender) resendRuntimeReport(conn *websocket.Conn) {
	s.mu.Lock()
	data := append([]byte(nil), s.runtimeReport...)
	s.mu.Unlock()
	if len(data) == 0 {
		return
	}
	wctx, cancel := context.WithTimeout(context.Background(), s.writeTimeout)
	defer cancel()
	if err := wsjson.Write(wctx, conn, streamFrame{Type: "runtime_report", Data: data}); err != nil {
		slog.Debug("ws runtime report resend failed", "err", err)
	}
}

// Close cleanly shuts the current connection (agent shutdown). Idempotent. Closing
// connDone here (not just clearing s.conn) stops pingLoop immediately -- otherwise it
// would sit idle on its ticker, pinging an already-closed conn, until its next tick
// fires a no-op dropConn call (a goroutine that lingers rather than exits promptly).
func (s *WSSender) Close() {
	s.mu.Lock()
	s.closed = true
	conn := s.conn
	done := s.connDone
	s.conn = nil
	s.connDone = nil
	s.mu.Unlock()
	if done != nil {
		close(done)
	}
	if conn != nil {
		_ = conn.Close(websocket.StatusNormalClosure, "agent shutdown")
	}
}

var errBackingOff = fmt.Errorf("ws telemetry: connection unavailable (backing off)")

func (s *WSSender) currentConn() *websocket.Conn {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.conn
}

// maybeDial attempts a single dial when the backoff window has elapsed. It returns
// errBackingOff when still inside the window (no dial this tick) or the dial error.
func (s *WSSender) maybeDial(ctx context.Context) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return errBackingOff
	}
	if s.conn != nil {
		s.mu.Unlock()
		return nil
	}
	if s.clock().Before(s.nextDialAt) {
		s.mu.Unlock()
		return errBackingOff
	}
	s.mu.Unlock()

	dctx, cancel := context.WithTimeout(ctx, s.dialTimeout)
	defer cancel()
	conn, _, err := websocket.Dial(dctx, s.url, &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {gwapi.BearerValue(s.token)}},
		HTTPClient: s.httpClient,
	})
	if err != nil {
		s.recordDialFailure()
		return fmt.Errorf("dial gateway ws: %w", err)
	}
	conn.SetReadLimit(wsMaxFrameBytes)

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		_ = conn.Close(websocket.StatusNormalClosure, "agent shutdown")
		return errBackingOff
	}
	done := make(chan struct{})
	s.conn = conn
	s.connDone = done
	s.connectedAt = s.clock()
	s.mu.Unlock()

	go s.readLoop(conn)
	go s.pingLoop(conn, done)
	slog.Debug("ws telemetry connected", "url", s.url)
	s.resendSystemReport(conn)
	s.resendRuntimeReport(conn)
	// A fresh (re)connect is itself a "check for a new certificate" moment
	// (Task 5b, Phase 2 distribution): a certificate issued while this agent
	// was disconnected must not wait out the full poll interval (up to 6h on
	// this transport) to be picked up -- the very next successful connect
	// wakes a sync exactly like a cert_update doorbell would.
	s.wakeCertUpdates()
	s.wakeTrustUpdates()
	// Likewise for the runtime-config document: a reconnect may have missed
	// a push while disconnected, so it wakes RuntimeUpdates() with a nil
	// payload ("resync over HTTP") exactly like the cert/trust wakes above.
	s.wakeRuntimeConfig(nil)
	return nil
}

// readLoop drains incoming frames so coder/websocket auto-pongs the gateway's
// pings and a server-initiated close is detected promptly, and decodes a
// cert_update/ca_update/runtime_config push to wake the matching doorbell
// (CertUpdates/TrustUpdates/RuntimeUpdates). It deliberately does NOT use
// wsjson.Read: that helper writes a close frame with a protocol-error status
// on ANY JSON decode failure, which would turn a malformed -- or simply
// forward-incompatible, future-version -- frame into a full connection
// teardown. Instead this reads the raw message and tolerates anything that
// does not parse as the {type,data} envelope, or whose type it does not
// recognize: only the three types above are acted on, every other type
// (including one this build has never heard of) is silently discarded. This
// function NEVER writes to conn -- data frames and pings are written
// exclusively by the future writer path (pingLoop's control frames today); a
// third writing goroutine here is exactly what the Task 5b house rule (Task 3's
// "genau ein Writer" constraint, mirrored on the agent side) forbids. A
// background context is used deliberately: a cancelled read context would
// abruptly tear down the connection instead of surfacing the close status.
// wakeRuntimeConfig's own doc explains why its send here can never block --
// that is THE load-bearing property for this function: readLoop is also
// what detects a dead peer, so it must never stall on any of these wakes.
func (s *WSSender) readLoop(conn *websocket.Conn) {
	for {
		_, b, err := conn.Read(context.Background())
		if err != nil {
			s.dropConn(conn, isCleanClose(err))
			return
		}
		var f streamFrame
		if json.Unmarshal(b, &f) != nil {
			continue // malformed frame: tolerated, never a reason to close.
		}
		switch f.Type {
		case "cert_update":
			s.wakeCertUpdates()
		case "ca_update":
			s.wakeTrustUpdates()
		case "runtime_config":
			s.wakeRuntimeConfig(f.Data)
		}
		// Any other (including unrecognized future) type is discarded --
		// forward compatibility.
	}
}

// pingLoop actively probes conn's liveness while it is the current connection. A
// per-read deadline on conn.Read is deliberately NOT used: coder/websocket handles
// ping/pong control frames internally within Read and never surfaces a ping to the
// caller, so a read deadline would spuriously trip on a healthy but data-idle
// connection (there are no server->agent data frames today). Instead this mirrors the
// gateway's own agentStreamKeepalive: every pingInterval it sends a ping and BLOCKS
// (coder/websocket's Ping) until the matching pong or pingTimeout. A missed pong means
// the peer is dead or half-open -- most relevant here, a silent NetBird/WireGuard
// tunnel drop, which is UDP and produces no FIN/RST, so the write path (which succeeds
// into the kernel send buffer regardless) would otherwise never notice. On a ping
// error, dropConn(conn, false) reaps the connection via the SAME non-clean path a read
// error uses, so the escalating-backoff/reset-on-stable semantics apply identically.
// done is closed by dropConn (whichever path reaches it first: this loop, readLoop, or
// a Post write failure) so this goroutine exits immediately rather than waiting out a
// stale ticker on an already-dead connection -- no goroutine leak.
func (s *WSSender) pingLoop(conn *websocket.Conn, done <-chan struct{}) {
	t := time.NewTicker(s.pingInterval)
	defer t.Stop()
	for {
		select {
		case <-done:
			return
		case <-t.C:
			pctx, cancel := context.WithTimeout(context.Background(), s.pingTimeout)
			err := conn.Ping(pctx)
			cancel()
			if err != nil {
				s.dropConn(conn, false)
				return
			}
		}
	}
}

// dropConn tears down conn and schedules the next dial. Idempotent per connection
// (a no-op if conn was already replaced). A clean close (or a drop after a stable
// connection) resets the backoff to base; other errors grow it exponentially + jitter.
func (s *WSSender) dropConn(conn *websocket.Conn, clean bool) {
	s.mu.Lock()
	if s.conn != conn {
		s.mu.Unlock()
		return
	}
	s.conn = nil
	if s.connDone != nil {
		close(s.connDone)
		s.connDone = nil
	}
	now := s.clock()
	stable := !s.connectedAt.IsZero() && now.Sub(s.connectedAt) >= s.stableThreshold
	var delay time.Duration
	switch {
	case clean:
		s.failures = 0
		delay = s.cleanReconnectDelay()
	case stable:
		s.failures = 1
		delay = s.backoffDelay(s.failures)
	default:
		s.failures++
		delay = s.backoffDelay(s.failures)
	}
	s.connectedAt = time.Time{}
	s.nextDialAt = now.Add(delay)
	s.mu.Unlock()
	_ = conn.CloseNow()
}

// recordDialFailure grows the backoff after a failed dial (never connected -> not stable).
func (s *WSSender) recordDialFailure() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failures++
	s.nextDialAt = s.clock().Add(s.backoffDelay(s.failures))
}

// backoffDelay = base * 2^(failures-1) capped at backoffCap, plus [0, delay/2) jitter.
func (s *WSSender) backoffDelay(failures int) time.Duration {
	d := s.backoffBase
	for i := 1; i < failures && d < s.backoffCap; i++ {
		d *= 2
	}
	if d > s.backoffCap {
		d = s.backoffCap
	}
	return d + time.Duration(s.rng.Int63n(int64(d/2)+1))
}

// cleanReconnectDelay is a short jittered delay in [cleanMin, cleanMax].
func (s *WSSender) cleanReconnectDelay() time.Duration {
	span := int64(s.cleanMax - s.cleanMin)
	if span <= 0 {
		return s.cleanMin
	}
	return s.cleanMin + time.Duration(s.rng.Int63n(span+1))
}

// isCleanClose reports whether err is a graceful WebSocket close (1000/1001).
func isCleanClose(err error) bool {
	switch websocket.CloseStatus(err) {
	case websocket.StatusNormalClosure, websocket.StatusGoingAway:
		return true
	default:
		return false
	}
}
