// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

// Package rpcrelay is the agent side of an RPC cluster member in relay mode.
// The member's rpc-server binds loopback only; the relay listens on the
// member's mesh (or LAN) address, the one the head dials, and copies bytes
// between the head and the rpc-server without parsing them.
//
// The relay is the member's network gate, so it fails closed throughout:
//
//   - The peer's source address is checked against the allowlist before a
//     single byte is read. A peer that is not listed (and every peer, while the
//     list is empty) is closed at once and reported, and none of its bytes ever
//     reaches the rpc-server.
//   - A connection that sends nothing never reaches the rpc-server either: the
//     relay dials the member only once the head's first byte has arrived, so a
//     reachability probe (connect, then close) costs the member nothing.
//   - The first byte takes the member's hold, and only while the member is
//     running; the relay never starts a member.
//   - One head per member: a newer connection that delivers its first byte
//     replaces the older forwarding one (the head reconnects during its own
//     startup), and the hold carries across the replacement.
//   - At most MaxSockets accepted connections per member are open at once, and
//     each has FirstByteTimeout to send its first byte.
//   - Every session is closed when its member's relay is removed (drain, stop),
//     when the allowlist changes, and on Close.
package rpcrelay

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// MaxSockets caps the connections a member's relay keeps open at once,
	// counting both the forwarding session and connections still waiting
	// for their first byte. A head needs one; the rest is room for its
	// reconnects and probes.
	MaxSockets = 4
	// FirstByteTimeout is how long an accepted connection may stay silent
	// before the relay closes it.
	FirstByteTimeout = 10 * time.Second
	// DefaultStallAfter is the stall threshold when a Config leaves it unset.
	DefaultStallAfter = 120 * time.Second
	// BindRetryInterval is how often a relay that could not bind tries again.
	BindRetryInterval = 5 * time.Second

	// TCP keepalive on the head's connection: probing starts after 30 s of
	// silence, so a head that vanished without closing its socket (a crash, a
	// lost mesh link) is noticed in about a minute and its hold ends.
	keepAliveIdle     = 30 * time.Second
	keepAliveInterval = 10 * time.Second
	keepAliveCount    = 3

	// dialTimeout bounds the connect to the member's loopback rpc-server.
	dialTimeout = 5 * time.Second
	// copyBufferSize is the per-direction copy buffer of a session.
	copyBufferSize = 64 << 10
	// noticeEvery rate-limits the relay's warnings about refused and
	// over-cap connections per member, so a scanner cannot flood the log.
	noticeEvery = 10 * time.Second
)

// ErrClosed is returned by Apply after Close.
var ErrClosed = errors.New("rpcrelay: closed")

// Config is one member's relay.
type Config struct {
	// SpecID is the member's rpc_server spec ID.
	SpecID string
	// Bind is the exact IPv4 address and port the relay listens on, the
	// address the head dials ("0.0.0.0" binds every interface).
	Bind string
	// Target is the member's rpc-server; it must be a loopback address.
	Target string
	// AllowedIPs returns the head addresses allowed to connect. It is called
	// for every accepted connection and on every Apply; nil, or an empty
	// result, refuses everyone.
	AllowedIPs func() []net.IP
	// HeadSpecID is the head the hold is reported for.
	HeadSpecID string
	// StallAfter is how long the head's bytes may stay unanswered before the
	// session reports a stall; 0 means DefaultStallAfter.
	StallAfter time.Duration
}

// Events receives what the relay observes. The calls for one member are
// serialized, in the order they happened; Refused may arrive concurrently with
// them. Implementations must not call back into the Manager.
type Events interface {
	// Held: a head connected to the member and sent its first byte while the
	// member was running. It is not repeated when a newer connection replaces
	// the forwarding one.
	Held(specID, headSpecID string, since time.Time)
	// Released: the forwarding session ended and no newer one replaced it.
	Released(specID string)
	// Refused: a peer outside the allowlist connected; nothing of it was read.
	Refused(specID string, peer net.IP)
	// Stalled: the head's last bytes have been unanswered for StallAfter. It
	// is reported once per unanswered stretch.
	Stalled(specID string)
}

// ListenerStatus is the state of one member's relay listener.
type ListenerStatus struct {
	// Listening: the relay is bound to its address and accepting.
	Listening bool
	// BindMismatch: the bind address is not an address of this host, so the
	// relay cannot listen on it; it retries every BindRetryInterval.
	BindMismatch bool
	// Err is why the last bind attempt failed, while the relay is not
	// listening; empty once it is.
	Err string
}

// Manager runs one relay listener per member. It is safe for concurrent use.
type Manager struct {
	ev      Events
	running func(specID string) bool

	// applyMu serializes Apply and Close; mu guards members and closed, and is
	// never held across a member's shutdown.
	applyMu sync.Mutex
	mu      sync.Mutex
	members map[string]*member
	closed  bool

	// Seams for tests; NewManager sets the production values.
	isLocal          func(net.IP) bool
	retryEvery       time.Duration
	firstByteTimeout time.Duration
	// admitHook, when set, runs between a connection's allowlist check and
	// its registration.
	admitHook func()
}

// NewManager returns a Manager reporting to ev. running reports whether a
// member is running; a hold is taken only while it says so. A nil running
// reads as never running, a nil ev as nobody listening.
func NewManager(ev Events, running func(specID string) bool) *Manager {
	if ev == nil {
		ev = nopEvents{}
	}
	if running == nil {
		running = func(string) bool { return false }
	}
	return &Manager{
		ev:               ev,
		running:          running,
		members:          map[string]*member{},
		isLocal:          isLocalIP,
		retryEvery:       BindRetryInterval,
		firstByteTimeout: FirstByteTimeout,
	}
}

// Apply makes the running relays match cfgs: it starts the new ones, removes
// the ones no longer listed (closing their sessions), and replaces one whose
// bind address, target, head or stall threshold changed. A relay whose
// allowlist changed keeps its listener but closes every session. An invalid
// config is left out, as if it were not listed, and reported in the returned
// error; the valid ones are applied regardless.
func (m *Manager) Apply(cfgs []Config) error {
	m.applyMu.Lock()
	defer m.applyMu.Unlock()
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return ErrClosed
	}
	current := make(map[string]*member, len(m.members))
	for id, mem := range m.members {
		current[id] = mem
	}
	m.mu.Unlock()

	valid, err := validConfigs(cfgs)
	next := make(map[string]*member, len(valid))
	var stale, fresh []*member
	for _, cfg := range valid {
		allowed := evalAllowlist(cfg.AllowedIPs)
		mem := current[cfg.SpecID]
		if mem != nil && mem.sameShape(cfg) {
			mem.updateAllowlist(cfg.AllowedIPs, allowed)
			next[cfg.SpecID] = mem
			continue
		}
		if mem != nil {
			stale = append(stale, mem)
		}
		mem = newMember(m, cfg, allowed)
		next[cfg.SpecID] = mem
		fresh = append(fresh, mem)
	}
	for id, mem := range current {
		if _, ok := next[id]; !ok {
			stale = append(stale, mem)
		}
	}
	// Old listeners go first, so a replacement can bind the same address.
	for _, mem := range stale {
		mem.stop()
	}
	for _, mem := range fresh {
		mem.start()
	}
	m.mu.Lock()
	m.members = next
	m.mu.Unlock()
	return err
}

// Status reports the listener of member specID, and whether it has a relay.
func (m *Manager) Status(specID string) (ListenerStatus, bool) {
	m.mu.Lock()
	mem := m.members[specID]
	m.mu.Unlock()
	if mem == nil {
		return ListenerStatus{}, false
	}
	mem.mu.Lock()
	defer mem.mu.Unlock()
	return ListenerStatus{Listening: mem.ln != nil, BindMismatch: mem.bindMismatch, Err: mem.bindErr}, true
}

// Close stops every relay and closes every session; later Applies fail with
// ErrClosed. It waits for the relays' goroutines and is idempotent.
func (m *Manager) Close() {
	m.applyMu.Lock()
	defer m.applyMu.Unlock()
	m.mu.Lock()
	m.closed = true
	members := m.members
	m.members = map[string]*member{}
	m.mu.Unlock()
	for _, mem := range members {
		mem.stop()
	}
}

// --- configuration -------------------------------------------------------------

// validConfigs returns the usable configs, normalized, and an error naming each
// one left out. Two configs for one member are both left out: which one was
// meant cannot be told.
func validConfigs(cfgs []Config) ([]Config, error) {
	seen := map[string]int{}
	for _, c := range cfgs {
		seen[c.SpecID]++
	}
	var out []Config
	var errs []error
	for _, c := range cfgs {
		if c.SpecID == "" {
			errs = append(errs, errors.New("rpcrelay: config without a spec id"))
			continue
		}
		if seen[c.SpecID] > 1 {
			errs = append(errs, fmt.Errorf("rpcrelay: %s: listed more than once", c.SpecID))
			continue
		}
		bind, _, err := parseIPv4Addr(c.Bind)
		if err != nil {
			errs = append(errs, fmt.Errorf("rpcrelay: %s: bind: %w", c.SpecID, err))
			continue
		}
		target, ip, err := parseIPv4Addr(c.Target)
		if err != nil {
			errs = append(errs, fmt.Errorf("rpcrelay: %s: target: %w", c.SpecID, err))
			continue
		}
		if !ip.IsLoopback() {
			errs = append(errs, fmt.Errorf("rpcrelay: %s: target %s is not a loopback address", c.SpecID, c.Target))
			continue
		}
		c.Bind, c.Target = bind, target
		if c.StallAfter <= 0 {
			c.StallAfter = DefaultStallAfter
		}
		out = append(out, c)
	}
	return out, errors.Join(errs...)
}

// parseIPv4Addr parses "a.b.c.d:port" with a literal IPv4 address and a port in
// 1..65535, returning it in canonical form.
func parseIPv4Addr(s string) (string, net.IP, error) {
	host, port, err := net.SplitHostPort(s)
	if err != nil {
		return "", nil, err
	}
	ip := net.ParseIP(host)
	if ip == nil || strings.Contains(host, ":") || ip.To4() == nil {
		return "", nil, fmt.Errorf("%q is not a literal IPv4 address", host)
	}
	p, err := strconv.Atoi(port)
	if err != nil || p < 1 || p > 65535 {
		return "", nil, fmt.Errorf("port %q out of range", port)
	}
	ip = ip.To4()
	return net.JoinHostPort(ip.String(), strconv.Itoa(p)), ip, nil
}

// evalAllowlist evaluates an allowlist into a sorted, deduplicated set of
// usable addresses: nil and unspecified entries are dropped, since they could
// only ever widen what "allowed" means.
func evalAllowlist(f func() []net.IP) []string {
	if f == nil {
		return nil
	}
	var out []string
	for _, ip := range f() {
		if ip == nil || ip.IsUnspecified() {
			continue
		}
		out = append(out, ip.String())
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// isLocalIP reports whether ip is an address of this host (the unspecified
// address always is: it binds every interface).
func isLocalIP(ip net.IP) bool {
	if ip.IsUnspecified() {
		return true
	}
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return false
	}
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && n.IP.Equal(ip) {
			return true
		}
	}
	return false
}

type nopEvents struct{}

func (nopEvents) Held(string, string, time.Time) {}
func (nopEvents) Released(string)                {}
func (nopEvents) Refused(string, net.IP)         {}
func (nopEvents) Stalled(string)                 {}

// --- one member's relay -------------------------------------------------------------

type member struct {
	m   *Manager
	cfg Config // AllowedIPs is replaced live, under mu; the rest is fixed
	// allowed is the allowlist as last evaluated by Apply, to tell a change.
	// Only Apply touches it, under the Manager's applyMu.
	allowed []string

	ctx    context.Context // cancelled when the relay is removed
	cancel context.CancelFunc
	wg     sync.WaitGroup // the accept loop, the bind retry and every session

	// evMu orders this member's Held, Released and Stalled events: each is
	// decided under mu and emitted before evMu is let go.
	evMu sync.Mutex

	mu           sync.Mutex
	ln           net.Listener
	bindMismatch bool
	bindErr      string // why the last bind attempt failed; "" once bound
	// allowGen counts allowlist changes. A connection is checked against the
	// list of one generation and registered only while that generation still
	// stands, so a change that lands in between cannot miss it: the change
	// bumps the generation before it closes the registered sessions.
	allowGen   uint64
	sockets    map[*session]struct{}
	active     *session // the session forwarding to the member
	held       bool     // a hold has been reported and not released
	lastNotice time.Time
	suppressed int
}

func newMember(m *Manager, cfg Config, allowed []string) *member {
	ctx, cancel := context.WithCancel(context.Background())
	return &member{m: m, cfg: cfg, allowed: allowed, ctx: ctx, cancel: cancel, sockets: map[*session]struct{}{}}
}

// sameShape reports whether cfg can be applied to the running relay in place:
// everything but the allowlist is unchanged.
func (mem *member) sameShape(cfg Config) bool {
	return mem.cfg.Bind == cfg.Bind && mem.cfg.Target == cfg.Target &&
		mem.cfg.HeadSpecID == cfg.HeadSpecID && mem.cfg.StallAfter == cfg.StallAfter
}

// updateAllowlist installs a new allowlist function; when the set it yields
// differs from the last one, every session is closed, so no connection admitted
// under the old list outlives it.
func (mem *member) updateAllowlist(f func() []net.IP, allowed []string) {
	changed := !slices.Equal(mem.allowed, allowed)
	mem.mu.Lock()
	mem.cfg.AllowedIPs = f
	if changed {
		mem.allowGen++
	}
	mem.mu.Unlock()
	if !changed {
		return
	}
	mem.allowed = allowed
	slog.Info("rpcrelay: allowlist changed; closing the member's sessions", "spec", mem.cfg.SpecID)
	mem.closeSessions()
}

// start binds the relay, or starts retrying when it cannot.
func (mem *member) start() {
	if mem.bind() {
		return
	}
	mem.wg.Go(mem.retryBind)
}

// retryBind tries to bind every retryEvery until it succeeds or the relay is
// removed.
func (mem *member) retryBind() {
	t := time.NewTicker(mem.m.retryEvery)
	defer t.Stop()
	for {
		select {
		case <-mem.ctx.Done():
			return
		case <-t.C:
			if mem.bind() {
				slog.Info("rpcrelay: relay listening", "spec", mem.cfg.SpecID, "bind", mem.cfg.Bind)
				return
			}
		}
	}
}

// bind attempts to listen on the relay's address once. It reports true once
// nothing is left to retry: bound, or the relay was removed meanwhile.
func (mem *member) bind() bool {
	host, _, _ := net.SplitHostPort(mem.cfg.Bind)
	if !mem.m.isLocal(net.ParseIP(host)) {
		if mem.bindFailed(true, "bind address "+host+" is not an address of this host") {
			slog.Warn("rpcrelay: bind address is not an address of this host; retrying",
				"spec", mem.cfg.SpecID, "bind", mem.cfg.Bind, "retry_every", mem.m.retryEvery)
		}
		return mem.ctx.Err() != nil
	}
	lc := net.ListenConfig{KeepAliveConfig: net.KeepAliveConfig{
		Enable: true, Idle: keepAliveIdle, Interval: keepAliveInterval, Count: keepAliveCount,
	}}
	ln, err := lc.Listen(mem.ctx, "tcp4", mem.cfg.Bind)
	if err != nil {
		if mem.bindFailed(false, err.Error()) {
			slog.Warn("rpcrelay: cannot listen; retrying", "spec", mem.cfg.SpecID, "bind", mem.cfg.Bind, "err", err, "retry_every", mem.m.retryEvery)
		}
		return mem.ctx.Err() != nil
	}
	mem.mu.Lock()
	if mem.ctx.Err() != nil {
		mem.mu.Unlock()
		ln.Close()
		return true
	}
	mem.ln, mem.bindMismatch, mem.bindErr = ln, false, ""
	mem.mu.Unlock()
	mem.wg.Go(func() { mem.acceptLoop(ln) })
	return true
}

// bindFailed records a failed bind attempt and reports whether its error is
// new, so a retry that fails the same way is not logged again.
func (mem *member) bindFailed(mismatch bool, msg string) bool {
	mem.mu.Lock()
	defer mem.mu.Unlock()
	mem.bindMismatch = mismatch
	repeated := mem.bindErr == msg
	mem.bindErr = msg
	return !repeated
}

func (mem *member) acceptLoop(ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			if mem.ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return
			}
			// A transient failure (descriptor exhaustion): back off briefly
			// rather than spin.
			select {
			case <-mem.ctx.Done():
				return
			case <-time.After(50 * time.Millisecond):
			}
			continue
		}
		mem.admit(c)
	}
}

// admit decides on an accepted connection before reading anything from it.
func (mem *member) admit(c net.Conn) {
	var peer net.IP
	if a, ok := c.RemoteAddr().(*net.TCPAddr); ok {
		peer = a.IP
	}
	allowed, gen := mem.allows(peer)
	if !allowed {
		c.Close()
		mem.m.ev.Refused(mem.cfg.SpecID, peer)
		mem.notice("rpcrelay: refused a connection from a source outside the allowlist", "peer", peer.String())
		return
	}
	if hook := mem.m.admitHook; hook != nil {
		hook()
	}
	s := &session{mem: mem, client: c, allowGen: gen}
	mem.mu.Lock()
	if mem.ctx.Err() != nil || mem.allowGen != gen {
		// Removed, or the allowlist this peer was checked against was
		// replaced meanwhile: the peer is not re-checked, it reconnects.
		mem.mu.Unlock()
		c.Close()
		return
	}
	if len(mem.sockets) >= MaxSockets {
		mem.mu.Unlock()
		c.Close()
		mem.notice("rpcrelay: refused a connection over the per-member socket cap", "peer", peer.String(), "cap", MaxSockets)
		return
	}
	mem.sockets[s] = struct{}{}
	mem.mu.Unlock()
	mem.wg.Go(s.serve)
}

// allows reports whether peer is on the allowlist, evaluated now, and the
// allowlist generation it was evaluated against.
func (mem *member) allows(peer net.IP) (bool, uint64) {
	mem.mu.Lock()
	f, gen := mem.cfg.AllowedIPs, mem.allowGen
	mem.mu.Unlock()
	if peer == nil || f == nil {
		return false, gen
	}
	for _, ip := range f() {
		if ip != nil && !ip.IsUnspecified() && ip.Equal(peer) {
			return true, gen
		}
	}
	return false, gen
}

// notice logs a refusal at Warn, at most once per noticeEvery per member,
// counting the ones it held back.
func (mem *member) notice(msg string, args ...any) {
	mem.mu.Lock()
	now := time.Now()
	if !mem.lastNotice.IsZero() && now.Sub(mem.lastNotice) < noticeEvery {
		mem.suppressed++
		mem.mu.Unlock()
		return
	}
	suppressed := mem.suppressed
	mem.lastNotice, mem.suppressed = now, 0
	mem.mu.Unlock()
	slog.Warn(msg, append([]any{"spec", mem.cfg.SpecID, "suppressed_since_last", suppressed}, args...)...)
}

// closeSessions closes every open connection of the relay.
func (mem *member) closeSessions() {
	mem.mu.Lock()
	sessions := make([]*session, 0, len(mem.sockets))
	for s := range mem.sockets {
		sessions = append(sessions, s)
	}
	mem.mu.Unlock()
	for _, s := range sessions {
		s.closeConns()
	}
}

// stop removes the relay: the listener and every session close, and stop
// returns once all of its goroutines have ended (and a hold it had reported is
// released).
func (mem *member) stop() {
	mem.cancel()
	mem.mu.Lock()
	ln := mem.ln
	mem.ln = nil
	mem.mu.Unlock()
	if ln != nil {
		ln.Close()
	}
	mem.closeSessions()
	mem.wg.Wait()
}

// --- one connection --------------------------------------------------------------

type session struct {
	mem    *member
	client net.Conn
	// allowGen is the allowlist generation the peer was admitted under.
	allowGen uint64

	mu         sync.Mutex
	target     net.Conn
	closed     bool
	pending    bool      // the head sent bytes the member has not answered yet
	lastClient time.Time // when the head last sent bytes
	stalledFor time.Time // the lastClient a stall was reported for
}

// serve runs one accepted connection to its end.
func (s *session) serve() {
	defer s.teardown()
	mem := s.mem
	buf := make([]byte, copyBufferSize)
	// Without the first-byte deadline a silent socket could hold a slot
	// forever, so a connection that cannot have one is not served.
	if err := s.client.SetReadDeadline(time.Now().Add(mem.m.firstByteTimeout)); err != nil {
		return
	}
	n, _ := s.client.Read(buf)
	if n == 0 {
		return // silent, closed or timed out: nothing ever reaches the member
	}
	if err := s.client.SetReadDeadline(time.Time{}); err != nil {
		return
	}
	if !mem.m.running(mem.cfg.SpecID) {
		slog.Debug("rpcrelay: head sent bytes while the member is not running; closing", "spec", mem.cfg.SpecID)
		return
	}
	d := net.Dialer{Timeout: dialTimeout}
	tc, err := d.DialContext(mem.ctx, "tcp4", mem.cfg.Target)
	if err != nil {
		slog.Warn("rpcrelay: cannot reach the member's rpc-server", "spec", mem.cfg.SpecID, "err", err)
		return
	}
	if !s.promote(tc) {
		tc.Close()
		return
	}
	s.noteClient()
	if _, err := tc.Write(buf[:n]); err != nil {
		return
	}

	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		pump(s.client, tc, make([]byte, copyBufferSize), s.noteTarget)
		s.closeConns()
	})
	wg.Go(func() { s.watchStall(done) })
	pump(tc, s.client, buf, s.noteClient)
	s.closeConns()
	close(done)
	wg.Wait()
}

// promote makes s the member's forwarding session: an older one is closed, and
// the hold is reported unless one already stands (the replacement keeps it).
func (s *session) promote(tc net.Conn) bool {
	mem := s.mem
	mem.evMu.Lock()
	defer mem.evMu.Unlock()
	mem.mu.Lock()
	if mem.ctx.Err() != nil || mem.allowGen != s.allowGen || !s.attach(tc) {
		mem.mu.Unlock()
		return false
	}
	old := mem.active
	mem.active = s
	wasHeld := mem.held
	mem.held = true
	mem.mu.Unlock()
	if old != nil {
		old.closeConns()
	}
	if !wasHeld {
		mem.m.ev.Held(mem.cfg.SpecID, mem.cfg.HeadSpecID, time.Now())
	}
	return true
}

// attach records the member connection, unless s was closed meanwhile.
func (s *session) attach(tc net.Conn) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	s.target = tc
	return true
}

// teardown closes s and, when it was the forwarding session, releases the hold.
func (s *session) teardown() {
	s.closeConns()
	mem := s.mem
	mem.evMu.Lock()
	defer mem.evMu.Unlock()
	mem.mu.Lock()
	delete(mem.sockets, s)
	release := false
	if mem.active == s {
		mem.active = nil
		release, mem.held = mem.held, false
	}
	mem.mu.Unlock()
	if release {
		mem.m.ev.Released(mem.cfg.SpecID)
	}
}

// closeConns closes both connections; it is idempotent and never blocks on the
// member's locks.
func (s *session) closeConns() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	tc := s.target
	s.mu.Unlock()
	s.client.Close()
	if tc != nil {
		tc.Close()
	}
}

func (s *session) noteClient() {
	s.mu.Lock()
	s.pending, s.lastClient = true, time.Now()
	s.mu.Unlock()
}

func (s *session) noteTarget() {
	s.mu.Lock()
	s.pending = false
	s.mu.Unlock()
}

// watchStall reports a stall when the head's last bytes have gone unanswered
// for StallAfter. The clock runs from the head's LAST bytes, not its first
// unanswered ones: commands the rpc-server never answers (tensor uploads)
// stream for minutes, and a head still sending is not waiting. A stalled
// member that stops reading stops the head's bytes too, through TCP
// backpressure, so that case is caught as well.
func (s *session) watchStall(done <-chan struct{}) {
	after := s.mem.cfg.StallAfter
	every := min(max(after/4, 5*time.Millisecond), 5*time.Second)
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-done:
			return
		case now := <-t.C:
			if s.stalled(now, after) {
				s.reportStall()
			}
		}
	}
}

// stalled reports a stall not yet reported for the head's current last bytes.
func (s *session) stalled(now time.Time, after time.Duration) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.pending || s.closed || now.Sub(s.lastClient) < after || s.stalledFor.Equal(s.lastClient) {
		return false
	}
	s.stalledFor = s.lastClient
	return true
}

// reportStall emits Stalled, if s is still the member's forwarding session.
func (s *session) reportStall() {
	mem := s.mem
	mem.evMu.Lock()
	defer mem.evMu.Unlock()
	mem.mu.Lock()
	current := mem.active == s
	mem.mu.Unlock()
	if current {
		slog.Warn("rpcrelay: the member has not answered the head", "spec", mem.cfg.SpecID, "after", mem.cfg.StallAfter)
		mem.m.ev.Stalled(mem.cfg.SpecID)
	}
}

// pump copies src to dst through buf until either fails, calling note for
// every chunk read from src before it is written on.
func pump(dst, src net.Conn, buf []byte, note func()) {
	for {
		n, err := src.Read(buf)
		if n > 0 {
			note()
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}
