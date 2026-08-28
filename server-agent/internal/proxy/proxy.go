// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

// Package proxy implements the agent-side TLS-terminating reverse proxy: a
// Manager owns one running proxy per desired route, each a TLS listener (serving
// the installed mesh leaf via a hot-swappable certificate holder) fronting an
// httputil.ReverseProxy to the route's plaintext upstream.
//
// Route reconciliation mirrors the gateway's proven drain discipline
// (cmd/gateway/main.go startLocked/retireServeGeneration/stopLocked): each serve
// goroutine, on exit, retires its generation only if it is still the current one,
// so a delayed Serve-exit can never clobber a successor that already replaced it.
package proxy

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"op-ai-server-agent/internal/certfiles"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

const shutdownGrace = 3 * time.Second

// Route is one desired TLS-terminating proxy: accept TLS on Listen (a TCP port),
// terminate it with the installed mesh leaf, and reverse-proxy to Upstream (a
// plaintext URL such as "http://127.0.0.1:9000").
type Route struct {
	Listen   int
	Upstream string
}

// RouteState names WHY a route is (or isn't) serving, distinguishing the four
// causes startProxyLocked can leave a route pending on -- previously
// surfaced only as (some) log lines, with tls_active=false looking identical
// for "waiting for the first leaf" and "port already bound". StateActive is
// the only state with TLSActive true; the rest are all pending, for a
// distinct reason.
type RouteState string

const (
	// StatePendingLeaf: no certificate leaf is installed yet, so the route has
	// never been attempted. Expected and routine before the first cert install.
	StatePendingLeaf RouteState = "pending_leaf"
	// StateInvalidUpstream: r.Upstream failed to parse as a scheme+host URL.
	// Never resolves without a config/gateway-side fix.
	StateInvalidUpstream RouteState = "invalid_upstream"
	// StatePendingBindHost: the leaf is loaded but carries no usable IP/DNS SAN
	// (and no bind-host override is configured), so no safe bind address could
	// be derived.
	StatePendingBindHost RouteState = "pending_bind_host"
	// StateBindFailed: net.Listen on the resolved bind host/port failed (e.g.
	// the port is already bound by something else).
	StateBindFailed RouteState = "bind_failed"
	// StateActive: the TLS listener is up and serving.
	StateActive RouteState = "active"
)

// RouteStatus reports the observed state of a desired route. TLSActive is true
// only when the leaf is loaded AND the listener is serving; State gives the
// distinguishing reason (see RouteState) whether or not TLSActive is true.
type RouteStatus struct {
	Listen    int
	TLSActive bool
	State     RouteState
}

// validRoute reports whether a route is well-formed enough to serve.
func validRoute(r Route) bool {
	return r.Listen > 0 && r.Listen <= 65535 && r.Upstream != ""
}

// ResolveRoutes merges gateway-provided and locally-configured routes by listen
// port. mode "override" -> a local route wins over a gateway route on the same
// listen; any other mode ("fallback", the default) -> a local route only fills a
// listen the gateway did not provide. Malformed routes (bad port or empty
// upstream) are dropped and duplicates are collapsed (last wins within a source),
// gateway-first order preserved.
func ResolveRoutes(gateway, local []Route, mode string) []Route {
	byListen := make(map[int]Route)
	order := make([]int, 0, len(gateway)+len(local))
	put := func(r Route) {
		if _, ok := byListen[r.Listen]; !ok {
			order = append(order, r.Listen)
		}
		byListen[r.Listen] = r
	}
	for _, r := range gateway {
		if validRoute(r) {
			put(r)
		}
	}
	override := mode == "override"
	for _, r := range local {
		if !validRoute(r) {
			continue
		}
		if _, exists := byListen[r.Listen]; exists && !override {
			continue // fallback: gateway wins on a shared listen
		}
		put(r)
	}
	out := make([]Route, 0, len(order))
	for _, listen := range order {
		out = append(out, byListen[listen])
	}
	return out
}

// certHolder holds the current TLS leaf behind an atomic pointer so the
// GetCertificate callback (invoked concurrently during handshakes) reads it
// lock-free while a renewal hot-swaps it, without restarting any socket.
type certHolder struct {
	current atomic.Pointer[tls.Certificate]
}

// LoadFromDir loads certDir/fullchain.pem + certDir/privkey.pem and, on success,
// atomically swaps the current leaf. On any error the previous leaf is retained
// (no downgrade on a failed renewal).
func (h *certHolder) LoadFromDir(certDir string) error {
	cert, err := tls.LoadX509KeyPair(
		filepath.Join(certDir, certfiles.Fullchain),
		filepath.Join(certDir, certfiles.Privkey),
	)
	if err != nil {
		return err
	}
	h.current.Store(&cert)
	return nil
}

// GetCertificate is the tls.Config callback; it reads the live leaf on every
// handshake so a hot-swap takes effect immediately.
func (h *certHolder) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	cert := h.current.Load()
	if cert == nil {
		return nil, errors.New("proxy TLS certificate unavailable")
	}
	return cert, nil
}

// loaded reports whether a leaf is currently installed.
func (h *certHolder) loaded() bool {
	return h.current.Load() != nil
}

// bindHost derives the listener bind address from the current leaf's own
// identity: its first IP SAN, else its first DNS SAN. This is precisely the mesh
// address the certificate is issued for, so the proxy binds exactly where mesh
// peers reach it and is NEVER exposed on unrelated (e.g. a public NIC)
// interfaces the way an all-interfaces bind would be. Returns "" when no leaf is
// loaded or the leaf carries no usable SAN; the caller then keeps the route
// pending rather than falling back to all-interfaces.
func (h *certHolder) bindHost() string {
	cert := h.current.Load()
	if cert == nil {
		return ""
	}
	leaf := cert.Leaf
	if leaf == nil {
		// tls.LoadX509KeyPair populates Leaf on modern Go, but parse defensively
		// so an older or hand-built tls.Certificate still yields a bind host.
		if len(cert.Certificate) == 0 {
			return ""
		}
		parsed, err := x509.ParseCertificate(cert.Certificate[0])
		if err != nil {
			return ""
		}
		leaf = parsed
	}
	if len(leaf.IPAddresses) > 0 {
		return leaf.IPAddresses[0].String()
	}
	if len(leaf.DNSNames) > 0 {
		return leaf.DNSNames[0]
	}
	return ""
}

// DeriveBindHost loads the TLS leaf at certDir (the certfiles.Fullchain/
// Privkey layout) into a throwaway certHolder and returns its bindHost()
// derivation: the leaf's first IP SAN, else its first DNS SAN -- the exact
// mesh-identity address this package's own routes always bind to (see
// bindHost's doc). Returns "" when certDir has no loadable leaf, or the leaf
// carries no usable SAN.
//
// Exported for main.go, which needs the SAME derivation for the
// agent-managed model runtime's router bind address (task-18-fix-round-1.md
// I2): the runtime router lives in internal/runtime, which must not import
// internal/proxy or internal/certfiles (archtest), so main.go -- which
// already imports both -- resolves the default bind host here and threads
// the resulting plain string into runtime.NewDriver instead.
func DeriveBindHost(certDir string) string {
	h := &certHolder{}
	if err := h.LoadFromDir(certDir); err != nil {
		return ""
	}
	return h.bindHost()
}

// runningProxy is one live generation of a route's listener+server. Its fields
// are set once at start and read-only thereafter (except stopping, an atomic
// flag); the *runningProxy pointer is the generation identity used by the drain
// guard.
type runningProxy struct {
	listen   int
	upstream string
	server   *http.Server
	ln       net.Listener
	// stopping is set right before stopProxyLocked closes the listener for a
	// deliberate stop, so the serve goroutine treats the resulting Accept error as
	// a clean shutdown rather than logging it as a listener failure.
	stopping atomic.Bool
}

// Manager owns the running proxies for a desired route set. A single mutex guards
// all mutable state (desired, running, closed); the certificate holder is
// atomic and read lock-free by handshakes.
type Manager struct {
	certDir string
	holder  certHolder

	// host is an explicit bind-host override for every listener. When "" (the
	// production default) the bind host is derived per-listener from the
	// installed leaf's SAN (see certHolder.bindHost) so the proxy binds exactly
	// the agent's own mesh address -- never all interfaces. A non-empty value
	// (tests pass "127.0.0.1") overrides that derivation.
	host string

	mu      sync.Mutex
	desired []Route
	running map[int]*runningProxy
	// states records the last-observed RouteState per listen port, written at
	// each of startProxyLocked's early-return causes and its success path.
	// Purely observational (Status()/telemetry) -- reconcileLocked's
	// keep-on-transient-error / stop-on-not-desired decisions never read it.
	states map[int]RouteState
	// localUpstream resolves a LOOPBACK upstream port to an in-process
	// handler, or nil to dial it as usual. nil (the default) disables the
	// in-process path entirely. See SetLocalUpstream in local_upstream.go.
	localUpstream func(port int) http.Handler
	closed        bool
	wg            sync.WaitGroup // tracks serve goroutines for deterministic Close
}

// New creates a Manager reading its leaf from certDir. host is the listener
// bind-host override: pass "" in production so each listener binds the agent's
// own mesh address derived from the installed leaf's SAN (never all interfaces);
// tests pass "127.0.0.1". It attempts a best-effort initial load so an Apply can
// bring routes up immediately when the leaf is already installed; a missing leaf
// simply leaves routes pending until ReloadCert.
func New(certDir, host string) *Manager {
	m := &Manager{
		certDir: certDir,
		host:    host,
		running: make(map[int]*runningProxy),
		states:  make(map[int]RouteState),
	}
	_ = m.holder.LoadFromDir(certDir)
	return m
}

// Apply sets the desired route set and reconciles running proxies to match:
// removed (or upstream-changed) routes are drained and stopped; new routes with a
// loaded leaf are started; routes without a loadable leaf stay pending.
func (m *Manager) Apply(routes []Route) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.desired = dedupeValidRoutes(routes)
	m.reconcileLocked()
}

// ReloadCert re-loads the leaf from disk (a renewal hot-swap; the old leaf is
// kept if the reload fails) and reconciles, which brings up any route that was
// pending for want of a leaf. Called by the installer after a new install.
func (m *Manager) ReloadCert() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.holder.LoadFromDir(m.certDir); err != nil {
		slog.Debug("proxy: cert reload failed; keeping current leaf", "certDir", m.certDir, "error", err)
	}
	m.reconcileLocked()
}

// Status snapshots the observed state of every desired route under the mutex.
// TLSActive is true iff a running proxy exists for that listen (which implies the
// leaf was loaded and the listener is serving); State names why, distinguishing
// the pending causes from each other and from StateActive.
func (m *Manager) Status() []RouteStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]RouteStatus, 0, len(m.desired))
	for _, r := range m.desired {
		_, running := m.running[r.Listen]
		out = append(out, RouteStatus{Listen: r.Listen, TLSActive: running, State: m.states[r.Listen]})
	}
	return out
}

// Close stops every running proxy and blocks until all serve goroutines exit, so
// there is no goroutine or socket leak after it returns. The Manager is inert
// afterwards (further Apply/ReloadCert reconcile to nothing).
func (m *Manager) Close() {
	m.mu.Lock()
	m.closed = true
	for _, rp := range m.running {
		m.stopProxyLocked(rp)
	}
	m.desired = nil
	m.mu.Unlock()
	m.wg.Wait()
}

// reconcileLocked drives running proxies toward m.desired. Caller holds m.mu.
func (m *Manager) reconcileLocked() {
	if m.closed {
		return
	}
	// Pick up a leaf that appeared on disk before this Apply (New's initial load
	// may have run against an empty dir). A forced reload is ReloadCert's job.
	if !m.holder.loaded() {
		_ = m.holder.LoadFromDir(m.certDir)
	}
	desired := make(map[int]Route, len(m.desired))
	for _, r := range m.desired {
		desired[r.Listen] = r
	}
	// Stop running proxies that are no longer desired or whose upstream changed.
	// stopProxyLocked frees each port fd synchronously (under m.mu) and drains the
	// connections off-lock, so stopping N routes neither serializes on the
	// graceful-shutdown grace nor stalls Apply/ReloadCert/Status; the start loop
	// below can therefore rebind a just-freed listen within this same pass.
	for listen, rp := range m.running {
		want, ok := desired[listen]
		if !ok || want.Upstream != rp.upstream {
			m.stopProxyLocked(rp)
		}
		if !ok {
			delete(m.states, listen) // no longer desired at all: drop its stale state too
		}
	}
	// Start desired routes that are not currently running. A route whose leaf is
	// not loadable (or whose bind fails) stays pending — no half-open port.
	for _, r := range m.desired {
		if _, ok := m.running[r.Listen]; ok {
			continue
		}
		m.startProxyLocked(r)
	}
}

// startProxyLocked binds and serves one route. Caller holds m.mu. It is a no-op
// (route stays pending) when the leaf is not loaded, the upstream URL is
// malformed, or the bind fails — none of which leave a half-open port.
func (m *Manager) startProxyLocked(r Route) {
	if !m.holder.loaded() {
		m.states[r.Listen] = StatePendingLeaf
		// No leaf yet is the routine, expected state before the first cert
		// install (and on every reconcile until then) -- Debug, not Error, to
		// match how unremarkable this is; the siblings below log at Error only
		// because their causes are actual misconfiguration/environment failures.
		slog.Debug("proxy: leaf not loaded; route pending", "listen", r.Listen)
		return // pending: proxy starts only with an installed leaf
	}
	target, err := url.Parse(r.Upstream)
	if err != nil || target.Scheme == "" || target.Host == "" {
		m.states[r.Listen] = StateInvalidUpstream
		slog.Error("proxy: invalid upstream URL; route pending", "listen", r.Listen, "upstream", r.Upstream, "error", err)
		return
	}

	// Resolve the bind host: an explicit override wins, else the agent's own
	// mesh address from the installed leaf's SAN. If neither is available we
	// leave the route PENDING rather than binding all interfaces -- a
	// mesh-terminating proxy must never listen on unrelated NICs.
	bindHost := m.host
	if bindHost == "" {
		bindHost = m.holder.bindHost()
	}
	if bindHost == "" {
		m.states[r.Listen] = StatePendingBindHost
		slog.Error("proxy: no bind host (leaf carries no IP/DNS SAN and no host configured); route pending", "listen", r.Listen)
		return
	}

	raw, err := net.Listen("tcp", net.JoinHostPort(bindHost, strconv.Itoa(r.Listen)))
	if err != nil {
		m.states[r.Listen] = StateBindFailed
		slog.Error("proxy: bind failed; route pending", "listen", r.Listen, "error", err)
		return
	}
	ln := tls.NewListener(raw, &tls.Config{
		GetCertificate: m.holder.GetCertificate,
		MinVersion:     tls.VersionTLS12,
	})

	rp := &runningProxy{
		listen:   r.Listen,
		upstream: r.Upstream,
		ln:       ln,
	}
	// The dialled reverse proxy is still what this route IS; the in-process
	// hand-off only intercepts the one upstream shape that can name a handler
	// living in this same process (see loopbackUpstreamPort's contract, which
	// is deliberately narrow -- a rejection just dials, as before). Wrapping
	// happens here, at listener start, but the RESOLUTION inside the wrapper
	// happens per request; see localFirst.
	var handler http.Handler = newReverseProxy(target)
	if m.localUpstream != nil {
		if port, ok := loopbackUpstreamPort(target); ok {
			handler = &localFirst{resolve: m.localUpstream, port: port, fallback: handler}
		}
	}
	rp.server = &http.Server{Handler: handler}
	m.running[r.Listen] = rp
	m.states[r.Listen] = StateActive

	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		serveErr := rp.server.Serve(ln)
		if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) && !rp.stopping.Load() {
			// A deliberate stop closes the listener under the lock (stopping set),
			// which surfaces here as an Accept error; that is a clean shutdown, not
			// a failure, so only an unexpected serve error is logged.
			slog.Error("proxy: listener stopped serving", "listen", rp.listen, "error", serveErr)
		}
		m.retireServeGeneration(rp)
	}()
	slog.Info("proxy: TLS listener serving", "listen", r.Listen, "upstream", r.Upstream)
}

// stopProxyLocked retires one running proxy. Caller holds m.mu. It (1) clears the
// map entry FIRST so the generation guard in retireServeGeneration turns this
// generation's delayed Serve-exit into a no-op; (2) closes the listener
// SYNCHRONOUSLY (marking stopping first so the serve goroutine sees a clean
// shutdown) so the port fd is released before this reconcile pass may rebind the
// same listen; and (3) drains in-flight connections OFF-LOCK in a tracked
// goroutine. Only the O(1) map-delete + listener-close run under m.mu -- the up-to
// shutdownGrace wait for active connections does not -- so stopping many routes,
// or one with a long in-flight request, never stalls Apply/ReloadCert/Status.
func (m *Manager) stopProxyLocked(rp *runningProxy) {
	if cur, ok := m.running[rp.listen]; ok && cur == rp {
		delete(m.running, rp.listen)
	}
	rp.stopping.Store(true)
	_ = rp.ln.Close() // free the port fd now, under the lock, for an immediate rebind
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		// Shutdown re-closes the (already closed) listener harmlessly and waits for
		// active connections to finish, up to shutdownGrace; on timeout it returns
		// and an over-grace connection is left to complete (unchanged semantics).
		shutCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
		defer cancel()
		_ = rp.server.Shutdown(shutCtx)
	}()
}

// retireServeGeneration cleans up after a serve goroutine returns, but ONLY when
// its (rp) is still the current generation for that listen. A stop or an
// upstream-change rebind replaces/removes the map entry under m.mu before this
// runs, so a delayed Serve-exit from a superseded generation cannot clobber its
// successor. This mirrors cmd/gateway/main.go's retireServeGeneration.
//
// Reaching the body below (cur == rp) means neither happened: rp's own Serve
// returned on its own (the "listener stopped serving" log site) while it was
// still the map's current generation. m.states still says StateActive for
// this listen; clear it rather than leave a stale "active" beside the
// TLSActive=false that m.running's deletion now produces -- purely
// observational bookkeeping, no change to the keep/stop decision itself,
// which the next reconcile still makes exactly as before.
func (m *Manager) retireServeGeneration(rp *runningProxy) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if cur, ok := m.running[rp.listen]; !ok || cur != rp {
		return // superseded: the successor owns this listen now
	}
	delete(m.states, rp.listen)
	_ = rp.ln.Close()
	delete(m.running, rp.listen)
}

// newReverseProxy builds a streaming reverse proxy to target: it rewrites each
// request onto target (scheme+host, joining paths) via NewSingleHostReverseProxy's
// director, flushes immediately after every write (FlushInterval:-1) so streamed
// responses are not buffered, and returns 502 when the upstream is unreachable.
func newReverseProxy(target *url.URL) *httputil.ReverseProxy {
	rp := httputil.NewSingleHostReverseProxy(target)
	rp.FlushInterval = -1
	rp.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		slog.Warn("proxy: upstream error", "upstream", target.String(), "path", r.URL.Path, "error", err)
		w.WriteHeader(http.StatusBadGateway)
	}
	return rp
}

// dedupeValidRoutes drops malformed routes and collapses duplicate listen ports
// (last wins), preserving first-seen order — the same normalization ResolveRoutes
// applies, so Apply is safe to call with a raw route slice.
func dedupeValidRoutes(routes []Route) []Route {
	idx := make(map[int]int, len(routes))
	out := make([]Route, 0, len(routes))
	for _, r := range routes {
		if !validRoute(r) {
			continue
		}
		if i, ok := idx[r.Listen]; ok {
			out[i] = r
			continue
		}
		idx[r.Listen] = len(out)
		out = append(out, r)
	}
	return out
}
