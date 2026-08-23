// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package main

import (
	"context"
	"crypto/tls"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"op-ai-gateway/internal/config"
	"op-ai-gateway/internal/gateway"
	"op-ai-gateway/internal/portal"
	"strings"
	"sync"
	"time"
)

type agentListenerLastGoodTLS struct {
	loaded      bool
	fingerprint string
	notAfter    time.Time
}

// bindRole selects how a listenerBind wraps its freshly-bound raw socket before
// serving. bindRolePlainCombined is the ONLY role wired in this task and reproduces
// today's single-socket behavior exactly: serve plain HTTP, and when TLS material is
// present sniff the first byte (0x16 -> TLS) to upgrade in place. The plain-only and
// TLS-only roles exist for the dedicated dual-listener orchestration in a later task
// and are not driven here.
type bindRole int

const (
	bindRolePlainCombined bindRole = iota
	bindRolePlainOnly
	bindRoleTLSOnly
)

// listenerBind owns one running agent-listener generation: its bound address, the
// http.Server serving it, the serving listener, that server's bind-scoped
// BaseContext cancel, its hot-swappable TLS certificate holder, whether TLS is
// enabled, the last-good TLS material for the no-downgrade fail-safe, and the role
// that decides its serve wrapper. Every *Locked method requires the shared manager
// mutex (mgr.mu) held by the caller; retireServeGeneration acquires it itself. The
// mgr back reference reaches the shared baseCtx/listen/mutex so multiple binds
// coordinate through one manager. A listenerBind must never be copied by value — its
// certHolder embeds an atomic pointer — so all methods take a pointer receiver.
type listenerBind struct {
	mgr        *agentListenerManager
	role       bindRole
	addr       string
	server     *http.Server
	ln         net.Listener
	bindCancel context.CancelFunc
	holder     certHolder
	tlsEnabled bool
	lastGood   agentListenerLastGoodTLS
}

// agentListenerManager owns the running NetBird agent listener(s) (a second
// http.Server on the gateway's NetBird IP) so the reconcile loop can rebind it
// live when the selected gateway peer's IP changes. It is only ever driven by the
// single reconcile path (startup + the reconcile-loop goroutine); its mutex guards
// only against an overlapping shutdown during a fast rebind. It holds a primary
// `plain` bind plus an optional dedicated `tls` bind (nil in combined mode, wired in
// a later task); baseCtx and listen are shared by every bind.
type agentListenerManager struct {
	mu      sync.Mutex
	baseCtx context.Context
	listen  func(network, address string) (net.Listener, error)
	plain   listenerBind
	tls     *listenerBind
}

func (b *listenerBind) listenTCP(address string) (net.Listener, error) {
	if b.mgr.listen != nil {
		return b.mgr.listen("tcp", address)
	}
	return net.Listen("tcp", address)
}

func (b *listenerBind) rememberTLSMaterial(material portal.GatewayMeshCertificateMaterial) {
	b.lastGood = agentListenerLastGoodTLS{
		loaded: true, fingerprint: material.Fingerprint, notAfter: material.NotAfter,
	}
}

func (b *listenerBind) lastGoodMaterial() portal.GatewayMeshCertificateMaterial {
	return portal.GatewayMeshCertificateMaterial{
		Fingerprint: b.lastGood.fingerprint,
		NotAfter:    b.lastGood.notAfter,
	}
}

// ensure (re)binds the COMBINED-mode primary bind to desiredAddr and reconciles
// its current gateway leaf. On an unchanged TLS-capable address, a valid leaf
// refresh is an atomic holder swap and does not rebind.
// desiredAddr "" -> stop the current listener. Otherwise net.Listen the new addr
// FIRST (a failure leaves the current one intact), then shut down the old server
// and serve the new one. Fail-safe: a bind error logs a Warn and keeps the
// current state (never crashes). Updates the Server's observable state.
//
// This is the single-bind entry point (combined topology); the mode-aware full
// driver that also brings up the dedicated TLS bind in separate mode is ensureAll.
//
// The net.Listen is SYNCHRONOUS on purpose: a successful bind is BOTH the proof
// that the address is a local interface (a wrong-peer IP fails with "cannot
// assign requested address") AND what makes srv.AgentListenerActive() accurate.
// The listener state is set via the mutex-guarded accessors
// (internal/gateway/server.go), so the public-listener gate + the status endpoint
// can read it concurrently with a rebind.
func (m *agentListenerManager) ensure(ctx context.Context, srv *gateway.Server, desiredAddr string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.wirePlainLocked()
	m.setPlainRoleLocked(bindRolePlainCombined)
	return m.plain.ensureLocked(ctx, srv, desiredAddr)
}

// wirePlainLocked wires the primary bind's write-once identity (its mgr back-ref)
// exactly once. Caller holds m.mu. mgr must be write-once because a retiring serve
// goroutine reads b.mgr lock-free (it needs the pointer to reach the shared mutex
// before it can lock): the first ensure sets it before any serve goroutine exists,
// so that write cannot race a lock-free read; re-assigning it on every ensure
// (even to the same value) WOULD race a concurrent retireServeGeneration from a
// prior generation. role starts combined and is only ever changed via
// setPlainRoleLocked (under m.mu).
func (m *agentListenerManager) wirePlainLocked() {
	if m.plain.mgr == nil {
		m.plain.mgr = m
		m.plain.role = bindRolePlainCombined
	}
}

// setPlainRoleLocked switches the primary bind's role, forcing a clean re-wrap
// when the role actually changes. Caller holds m.mu. The running listener was
// wrapped for the old role at bind time (a combined bind sniffs for TLS; a
// plain-only bind is raw), so a role flip must stop the current generation
// (clearing b.addr so the caller's ensureLocked no longer sees a same-address
// no-op and rebinds fresh with the new serve wrapper). role is read only under
// m.mu, so this reassignment is race-free. A same-role call is a no-op
// (steady-state ticks never rebind).
//
// It deliberately does NOT publishInactive: the observable state stays ARMED
// across the stop-first re-wrap so a transient bind failure right after the stop
// never disarms the netbird_only gate. The caller's ensureLocked rebind rearms on
// success (startLocked republishes) and only publishInactive's on genuine bind
// exhaustion — the same no-downgrade fail-safe as enableTLSLocked.
func (m *agentListenerManager) setPlainRoleLocked(role bindRole) {
	if m.plain.role == role {
		return
	}
	m.plain.stopLocked()
	m.plain.tlsEnabled = false
	m.plain.role = role
}

// ensureAll is the mode-aware full driver: it reconciles BOTH agent-listener binds
// for the current cert_mesh_tls_mode each reconcile tick, so a portal toggle
// rebinds within one interval. Caller must NOT hold m.mu (this method takes it).
//
//   - COMBINED: the primary bind carries today's sniff-when-material behavior
//     (bindRolePlainCombined) at resolveAgentAddr; if a dedicated TLS bind is still
//     up from a prior separate run, it is stopped and cleared (mode switched back).
//   - SEPARATE: the primary bind is raw plaintext (bindRolePlainOnly) at
//     resolveAgentAddr, and a dedicated TLS bind (bindRoleTLSOnly) is brought up at
//     resolveAgentTLSAddr — but ONLY while mesh-cert material exists (reusing the
//     bind's own last-good no-downgrade logic). When material is absent the TLS bind
//     stays down and a later tick brings it up once material appears.
//
// The two resolvers each return ok=false on a transient control-plane error; that
// bind is left untouched that tick (a blip must never drop a valid data-plane
// listener), exactly as the single-bind path already does.
func (m *agentListenerManager) ensureAll(ctx context.Context, srv *gateway.Server, cfg config.Config) error {
	if srv == nil || srv.Portal == nil {
		return nil
	}
	// A mode-read failure follows the same "don't touch on a blip" contract as the
	// address resolvers (resolveAgentAddr ok=false): CertMeshTLSSeparateActive
	// returns (false, err) on any settings-store error, so DISCARDING the error would
	// fall into the combined branch and tear down a live dedicated TLS bind every
	// tick during a transient DB outage. Keep the current topology this tick instead.
	separate, err := srv.Portal.CertMeshTLSSeparateActive(ctx)
	if err != nil {
		slog.Debug("netbird: could not read the mesh TLS mode; keeping the current agent-listener topology", "error", err)
		return nil
	}
	plainAddr, plainOK := resolveAgentAddr(ctx, srv, cfg)

	m.mu.Lock()
	defer m.mu.Unlock()
	m.wirePlainLocked()

	if !separate {
		// Combined: drive the primary bind with the sniff-when-material role and
		// tear down any dedicated TLS bind left from a prior separate run. Do
		// NOTHING until plainOK is confirmed: setPlainRoleLocked stopLocked()s the
		// primary socket on a genuine role change (and deliberately skips
		// publishInactive so the observable state stays ARMED), so running it on a
		// plainOK=false blip and then skipping ensureLocked would leave NO listener
		// on any port while the status still reports it active -- and tearing down
		// the TLS bind before the primary can re-wrap as a sniffer would drop TLS
		// serving too. Keep the current topology one more tick; a later ok tick
		// completes the switch (a control-plane blip must never drop a valid
		// data-plane listener).
		if !plainOK {
			return nil
		}
		if m.tls != nil {
			m.tls.stopLocked()
			m.tls.publishInactive(srv) // clears only the TLS slot
			m.tls = nil
		}
		m.setPlainRoleLocked(bindRolePlainCombined)
		return m.plain.ensureLocked(ctx, srv, plainAddr)
	}

	// Separate: raw plaintext primary + dedicated TLS bind. Flip the plain role
	// ONLY once plainOK gives an address to bind: setPlainRoleLocked stopLocked()s
	// the primary socket on the combined->plain-only change but stays armed (no
	// publishInactive), so calling it on a plainOK=false blip and then skipping the
	// rebind would leave NO listener on any port while the status still reports it
	// active. On a blip, leave the current wrapper serving one more tick.
	var plainErr error
	if plainOK {
		m.setPlainRoleLocked(bindRolePlainOnly)
		plainErr = m.plain.ensureLocked(ctx, srv, plainAddr)
	}

	// The dedicated TLS bind follows the same write-once discipline as the primary
	// bind: its mgr back-ref is set at construction (before any serve goroutine for
	// it exists) and never reassigned; every method takes a pointer receiver and
	// runs under m.mu; retireServeGeneration re-acquires m.mu. It is never copied by
	// value (certHolder embeds an atomic pointer).
	if m.tls == nil {
		m.tls = &listenerBind{mgr: m, role: bindRoleTLSOnly}
	}
	tlsAddr, tlsOK := resolveAgentTLSAddr(ctx, srv, cfg)
	if tlsOK {
		if tlsAddr != "" && m.meshMaterialAvailableLocked(ctx, srv) {
			_ = m.tls.ensureLocked(ctx, srv, tlsAddr)
		} else {
			// No addr wanted (no peer), or no mesh material yet: keep the TLS bind
			// down. A later tick brings it up once material appears / a peer is
			// selected. A TLS-only bind must NEVER serve plaintext, so it is left
			// down here rather than started without a leaf.
			_ = m.tls.ensureLocked(ctx, srv, "")
		}
	}
	return plainErr
}

// meshMaterialAvailableLocked reports whether the gateway mesh certificate is
// usable this tick for the dedicated TLS bind: a fresh read succeeds, OR the bind
// already holds a last-good leaf (the no-downgrade fail-safe — a transient read
// error must not tear a serving TLS listener down). Caller holds m.mu.
func (m *agentListenerManager) meshMaterialAvailableLocked(ctx context.Context, srv *gateway.Server) bool {
	if m.tls != nil && m.tls.lastGood.loaded {
		return true
	}
	if srv.Portal == nil {
		return false
	}
	_, err := srv.Portal.GatewayMeshCertificate(ctx)
	return err == nil
}

// ensureLocked runs the single-socket reconcile for one bind. Caller holds mgr.mu.
// It is the extracted body of the manager's public ensure and preserves every
// invariant: the listen-before-stop fail-safe on an address change, the one
// same-address stop-first TLS promotion, and the last-good no-downgrade path.
func (b *listenerBind) ensureLocked(ctx context.Context, srv *gateway.Server, desiredAddr string) error {
	if desiredAddr == "" {
		b.stopLocked()
		b.tlsEnabled = false
		b.publishInactive(srv)
		return nil
	}

	// A plain-only bind never negotiates TLS: it serves raw HTTP regardless of any
	// mesh material and publishes only the plaintext state slot (the dedicated TLS
	// bind owns the TLS slot in separate mode). Short-circuit the certificate logic
	// entirely so it can never store a leaf, flip tlsEnabled, or write the TLS slot.
	if b.role == bindRolePlainOnly {
		if desiredAddr == b.addr {
			return nil // already bound plain at this addr -> no-op
		}
		if b.addr != "" {
			// Genuine address change (the peer IP moved): acquire the NEW socket
			// BEFORE dropping the current one, and keep the current listener + its
			// armed state on failure (never drop a valid listener on a blip).
			raw, err := b.listenTCP(desiredAddr)
			if err != nil {
				slog.Warn("netbird: plain agent listener rebind failed; keeping current listener", "addr", desiredAddr, "error", err)
				return err // fail-safe: current listener + state unchanged
			}
			b.stopLocked()
			b.startLocked(srv, desiredAddr, raw, nil)
			return nil
		}
		// Fresh bind, or a same-address re-wrap after setPlainRoleLocked stopped the
		// old generation (a combined<->separate toggle). The socket is already down,
		// so there is no listen-before-stop fail-safe available: bind with a bounded
		// relistenWithRetry (the just-freed port fd may not be reusable for a few ms)
		// and keep any armed observable state across it. Publish inactive ONLY on
		// genuine exhaustion -- mirrors enableTLSLocked's no-downgrade retry so a
		// transient failure never disarms the netbird_only gate.
		raw, err := b.relistenWithRetry(desiredAddr)
		if err != nil {
			b.tlsEnabled = false
			b.publishInactive(srv)
			slog.Warn("netbird: could not bind the plain agent listener; leaving it down until the next reconcile tick", "addr", desiredAddr, "error", err)
			return err
		}
		b.startLocked(srv, desiredAddr, raw, nil)
		return nil
	}

	materialErr := portal.ErrCertificateNotFound
	material := portal.GatewayMeshCertificateMaterial{}
	if srv.Portal != nil {
		material, materialErr = srv.Portal.GatewayMeshCertificate(ctx)
	}
	if desiredAddr == b.addr {
		if materialErr != nil {
			if b.tlsEnabled {
				slog.Warn("netbird: gateway mesh certificate refresh failed; keeping last-good TLS listener", "error", materialErr)
			}
			return nil
		}
		if b.tlsEnabled {
			if err := b.holder.StorePEM(material.FullchainPEM, material.KeyPEM); err != nil {
				slog.Warn("netbird: gateway mesh certificate refresh is unusable; keeping last-good leaf", "error", err)
				return nil
			}
			b.rememberTLSMaterial(material)
			srv.SetAgentListenerTLSState(gateway.AgentListenerTLSState{
				Active: true, Address: desiredAddr, Fingerprint: material.Fingerprint, NotAfter: material.NotAfter,
			})
			return nil
		}
		return b.enableTLSLocked(srv, desiredAddr, material)
	}

	// Acquire the new raw socket before touching the current listener or its
	// observable state. On a genuine address change (b.addr != "") the old listener
	// is still up, so a single listen keeps the listen-before-stop fail-safe (keep
	// the current listener on failure). On a fresh bind or a same-address re-wrap
	// after setPlainRoleLocked already stopped the old generation (b.addr == "", e.g.
	// a separate->combined toggle) there is nothing to preserve, so ride out the
	// just-freed-port-fd window with a bounded relistenWithRetry rather than leaving
	// the port down for a whole reconcile tick.
	var (
		raw net.Listener
		err error
	)
	if b.addr == "" {
		raw, err = b.relistenWithRetry(desiredAddr)
	} else {
		raw, err = b.listenTCP(desiredAddr)
	}
	if err != nil {
		// A wrong-peer IP (not a local wt0 interface) fails here with "cannot
		// assign requested address"; the fail-safe keeps the current listener +
		// state intact so a transient/misconfigured resolve never cuts agents off.
		slog.Warn("netbird: agent listener rebind failed; keeping current listener", "addr", desiredAddr, "error", err)
		return err // fail-safe: current listener + state unchanged
	}
	hasMaterial := materialErr == nil
	switch {
	case hasMaterial:
		if err := b.holder.StorePEM(material.FullchainPEM, material.KeyPEM); err != nil {
			slog.Warn("netbird: gateway mesh certificate is unusable", "error", err)
			if b.lastGood.loaded {
				material = b.lastGoodMaterial()
			} else {
				hasMaterial = false
			}
		} else {
			b.rememberTLSMaterial(material)
		}
	case b.lastGood.loaded:
		// A certificate read failure must never downgrade TLS, but it must not
		// freeze a stale NetBird bind address either. The new raw socket is already
		// acquired, so move the TLS-capable listener using the last-good holder and
		// update only the runtime address.
		material = b.lastGoodMaterial()
		hasMaterial = true
		slog.Warn("netbird: gateway mesh certificate refresh failed; moving listener with last-good TLS leaf", "addr", desiredAddr, "error", materialErr)
	case !errors.Is(materialErr, portal.ErrCertificateNotFound):
		slog.Warn("netbird: gateway mesh certificate unavailable; binding plain agent listener", "error", materialErr)
	}
	b.stopLocked()
	if hasMaterial {
		b.startLocked(srv, desiredAddr, raw, &material)
	} else {
		b.startLocked(srv, desiredAddr, raw, nil)
	}
	return nil
}

// enableTLSLocked performs the one unavoidable same-address rebind when valid
// material first appears after a plain listener is already serving. Caller holds
// m.mu. Later leaf changes never come here; they only swap holder.current.
func (b *listenerBind) enableTLSLocked(srv *gateway.Server, addr string, material portal.GatewayMeshCertificateMaterial) error {
	if err := b.holder.StorePEM(material.FullchainPEM, material.KeyPEM); err != nil {
		slog.Warn("netbird: gateway mesh certificate is unusable; keeping plain agent listener", "error", err)
		return nil
	}
	b.rememberTLSMaterial(material)
	b.stopLocked()
	// Same-address promotion is unavoidably stop-first: without SO_REUSEPORT the old
	// and new socket cannot hold the address at once, so the listen-before-stop
	// fail-safe of the address-change path is not available here. Re-bind with a
	// bounded retry instead, so a transient failure right after stopLocked (the just
	// -freed port fd not yet reusable, a momentary interface hiccup) does not leave
	// the agent listener down until the next reconcile tick (>=30s) -- a gap would
	// flip AgentListenerActive() and, with it, transiently reopen the netbird_only
	// public-mux isolation. The material was already parsed above, so a successful
	// re-bind restores TLS directly; only a genuinely gone address exhausts the
	// retries and reports no listener (the next reconcile tick then rebinds via the
	// listen-first path).
	raw, err := b.relistenWithRetry(addr)
	if err != nil {
		b.tlsEnabled = false
		b.publishInactive(srv)
		slog.Warn("netbird: could not re-bind the agent listener after enabling TLS", "addr", addr, "error", err)
		return err
	}
	b.startLocked(srv, addr, raw, &material)
	return nil
}

// relistenWithRetry re-binds addr for the same-address TLS promotion, retrying a
// few times over a short window so a transient bind failure immediately after
// stopLocked does not leave the agent listener down. It is bounded so a genuinely
// unavailable address (the NetBird peer IP removed) still returns promptly and the
// caller can fall back cleanly. Caller holds m.mu; the total sleep is tiny relative
// to the reconcile cadence.
func (b *listenerBind) relistenWithRetry(addr string) (net.Listener, error) {
	const attempts = 5
	var err error
	for i := 0; i < attempts; i++ {
		var raw net.Listener
		if raw, err = b.listenTCP(addr); err == nil {
			return raw, nil
		}
		if i < attempts-1 {
			time.Sleep(20 * time.Millisecond)
		}
	}
	return nil, err
}

// startLocked publishes one successfully-bound listener and starts serving it.
// A nil material pointer is the byte-for-byte plain HTTP path; non-nil installs
// the protocol sniffer with the manager's hot-swappable certificate holder.
func (b *listenerBind) startLocked(srv *gateway.Server, addr string, raw net.Listener, material *portal.GatewayMeshCertificateMaterial) {
	serveListener := b.serveListenerFor(raw, material)
	baseCtx := b.mgr.baseCtx
	if baseCtx == nil {
		baseCtx = context.Background()
	}
	bindCtx, bindCancel := context.WithCancel(baseCtx)
	as := newHTTPServer(bindCtx, addr, srv.AgentHandler())
	b.server, b.ln, b.addr = as, serveListener, addr
	b.bindCancel = bindCancel
	b.tlsEnabled = material != nil
	b.publishActive(srv, addr, material)
	slog.Info("netbird: agent listener bound to the NetBird IP", "addr", addr, "role", b.role, "tls", material != nil)
	go func() {
		serveErr := as.Serve(serveListener)
		if serveErr != nil && serveErr != http.ErrServerClosed {
			slog.Error("netbird: agent listener stopped serving", "addr", addr, "error", serveErr)
		}
		b.retireServeGeneration(srv, as, serveListener)
	}()
}

// serveListenerFor wraps the freshly-bound raw socket per the bind's role. The
// combined role is byte-for-byte today's behavior: the plain HTTP path when there is
// no material, else the protocol sniffer (first byte 0x16 -> TLS) with the manager's
// hot-swappable certificate holder. The plain-only and TLS-only branches serve the
// dedicated dual-listener wiring of a later task and are not driven here; TLS-only
// uses tls.NewListener so no plaintext is ever accepted while handlers still see
// r.TLS != nil.
func (b *listenerBind) serveListenerFor(raw net.Listener, material *portal.GatewayMeshCertificateMaterial) net.Listener {
	switch b.role {
	case bindRoleTLSOnly:
		return tls.NewListener(raw, &tls.Config{
			GetCertificate: b.holder.GetCertificate,
			MinVersion:     tls.VersionTLS12,
		})
	case bindRolePlainOnly:
		return raw
	default: // bindRolePlainCombined
		if material != nil {
			return newSniffListener(raw, &tls.Config{
				GetCertificate: b.holder.GetCertificate,
				MinVersion:     tls.VersionTLS12,
			})
		}
		return raw
	}
}

// publishActive writes the Server's observable state for a just-bound generation
// of THIS bind, per its role: a combined bind owns BOTH the plaintext and the TLS
// slot (one socket serves both), a plain-only bind owns only the plaintext slot,
// and a TLS-only bind owns only the TLS slot. A nil material means no TLS leaf
// (the plain paths). Keeping each bind to its own slot(s) is what lets the plain
// and TLS binds move independently in separate mode without clobbering each other.
func (b *listenerBind) publishActive(srv *gateway.Server, addr string, material *portal.GatewayMeshCertificateMaterial) {
	switch b.role {
	case bindRoleTLSOnly:
		// A TLS-only bind is only ever started with material (guarded by ensureAll);
		// stay defensive and publish TLS state only when a leaf is actually present.
		if material != nil {
			srv.SetAgentListenerTLSState(gateway.AgentListenerTLSState{
				Active: true, Address: addr, Fingerprint: material.Fingerprint, NotAfter: material.NotAfter,
			})
		}
	case bindRolePlainOnly:
		srv.SetAgentPlainListener(true, addr)
	default: // bindRolePlainCombined -- one socket is both slots
		srv.SetAgentPlainListener(true, addr)
		if material != nil {
			srv.SetAgentListenerTLSState(gateway.AgentListenerTLSState{
				Active: true, Address: addr, Fingerprint: material.Fingerprint, NotAfter: material.NotAfter,
			})
		} else {
			srv.SetAgentListenerTLSState(gateway.AgentListenerTLSState{})
		}
	}
}

// publishInactive clears the Server's observable state for the slot(s) THIS bind
// owns (see publishActive). A plain-only bind clears only the plaintext slot; a
// TLS-only bind clears only the TLS slot; a combined bind clears both.
func (b *listenerBind) publishInactive(srv *gateway.Server) {
	switch b.role {
	case bindRoleTLSOnly:
		srv.SetAgentListenerTLSState(gateway.AgentListenerTLSState{})
	case bindRolePlainOnly:
		srv.SetAgentPlainListener(false, "")
	default: // bindRolePlainCombined
		srv.SetAgentPlainListener(false, "")
		srv.SetAgentListenerTLSState(gateway.AgentListenerTLSState{})
	}
}

// retireServeGeneration removes runtime state after Serve returns, but only when
// the completed server/listener pair is still the manager's current generation.
// A normal stop or rebind holds m.mu while replacing that pair, so its delayed
// Serve completion cannot clear the successor generation.
func (b *listenerBind) retireServeGeneration(srv *gateway.Server, as *http.Server, listener net.Listener) {
	b.mgr.mu.Lock()
	defer b.mgr.mu.Unlock()
	if b.server != as || b.ln != listener {
		return
	}
	if b.bindCancel != nil {
		b.bindCancel()
		b.bindCancel = nil
	}
	_ = listener.Close()
	b.server, b.ln, b.addr = nil, nil, ""
	b.tlsEnabled = false
	b.publishInactive(srv)
}

// stopLocked shuts down the current agent server (if any) and clears the tracked
// state. Caller must hold m.mu. Shutdown gracefully drains, then the serving
// listener (which owns the raw listener) is closed explicitly to GUARANTEE the
// port fd is released even if the serving
// goroutine had not yet registered the listener with Serve when Shutdown ran (in
// which case Shutdown closes nothing) — otherwise an immediate rebind to a freed
// peer IP could hit "address already in use". Closing an already-closed listener
// is a harmless no-op.
func (b *listenerBind) stopLocked() {
	if b.bindCancel != nil {
		// http.Server.Shutdown deliberately ignores hijacked WebSockets. Cancelling
		// this bind-specific BaseContext lets the agent-stream handler send 1001
		// GoingAway and return, without touching streams on the public listener.
		b.bindCancel()
		b.bindCancel = nil
	}
	if b.server != nil {
		shutCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		_ = b.server.Shutdown(shutCtx)
		cancel()
	}
	if b.ln != nil {
		_ = b.ln.Close()
	}
	b.server, b.ln, b.addr = nil, nil, ""
}

func resolveAgentAddr(ctx context.Context, srv *gateway.Server, cfg config.Config) (string, bool) {
	if a := strings.TrimSpace(cfg.AgentAddr); a != "" {
		return a, true
	}
	ip, err := srv.Portal.ResolveGatewayPeerIP(ctx)
	if err != nil {
		// Debug (not Warn): this runs every reconcile tick. A transient error must
		// not tear down a valid listener, so signal "don't touch" via ok=false.
		slog.Debug("netbird: could not resolve the gateway peer IP; keeping the current agent listener", "error", err)
		return "", false
	}
	if ip == "" {
		return "", true // no gateway peer selected -> no agent listener wanted
	}
	return net.JoinHostPort(ip, cfg.AgentPort), true
}

// resolveAgentTLSAddr computes the desired bind address for the dedicated
// encrypted agent listener in separate mode. It mirrors resolveAgentAddr exactly,
// one port family over: explicit OP_AI_GATEWAY_AGENT_TLS_ADDR wins, else the
// selected gateway peer IP + AGENT_TLS_PORT, else "". The (addr, ok) contract is
// identical -- ok=false means "could not determine this tick, don't touch the
// current TLS bind" (a control-plane blip must never drop a valid data-plane
// listener); ok=true with addr=="" means no listener wanted (no gateway peer). In
// separate mode the plaintext and TLS binds share the same host (peer IP or the
// explicit host) and differ only in port.
func resolveAgentTLSAddr(ctx context.Context, srv *gateway.Server, cfg config.Config) (string, bool) {
	if a := strings.TrimSpace(cfg.AgentTLSAddr); a != "" {
		return a, true
	}
	ip, err := srv.Portal.ResolveGatewayPeerIP(ctx)
	if err != nil {
		slog.Debug("netbird: could not resolve the gateway peer IP; keeping the current TLS agent listener", "error", err)
		return "", false
	}
	if ip == "" {
		return "", true // no gateway peer selected -> no TLS agent listener wanted
	}
	// Bind the SAME validated port the panel/policy advertise (effectiveAgentTLSPort),
	// never the raw cfg.AgentTLSPort: a malformed value must not make the bind and the
	// advertised port diverge -- both fall back to 8443 consistently.
	return net.JoinHostPort(ip, effectiveAgentTLSPort(cfg)), true
}

// startGatewayPeerReconcileLoop periodically auto-selects the gateway peer
// (portal.ReconcileGatewayPeer: sets netbird_gateway_peer_id when empty/stale +
// renames to netbird_gateway_peer_name) and rebinds the agent listener(s) to their
// current IP — so a freshly-enrolled sidecar becomes the live agent listener within
// one interval, no restart. It drives the mode-aware ensureAll each tick, so a
// portal cert_mesh_tls_mode toggle (combined<->separate) also rebinds within one
// interval. No-op when the module is off; explicit AGENT_ADDR/AGENT_TLS_ADDR
// resolve to fixed addrs so ensureAll no-ops (no rebind).
func startGatewayPeerReconcileLoop(srv *gateway.Server, cfg config.Config, mgr *agentListenerManager, interval time.Duration) context.CancelFunc {
	if interval < 30*time.Second {
		interval = 30 * time.Second
	}
	return startLoop(loopOpts{
		Interval: func() time.Duration { return interval }, // fixed cadence, no re-read
		Pass: func(ctx context.Context) {
			if _, _, err := srv.Portal.ReconcileGatewayPeer(ctx); err != nil {
				slog.Debug("netbird: gateway-peer reconcile failed", "error", err)
			}
			_ = mgr.ensureAll(ctx, srv, cfg)
		},
	})
}

// startAgentListener performs the initial (startup) agent-listener bind and returns
// the manager so the reconcile loop can rebind it live. It uses the mode-aware
// ensureAll so a separate-mode deployment brings up both the plaintext and the
// dedicated TLS bind at startup. Fail-safe: any problem leaves the current
// behavior intact (the public agent path stays open) — it never crashes the process.
func startAgentListener(baseCtx context.Context, srv *gateway.Server, cfg config.Config) *agentListenerManager {
	mgr := &agentListenerManager{baseCtx: baseCtx}
	_ = mgr.ensureAll(context.Background(), srv, cfg)
	return mgr
}
