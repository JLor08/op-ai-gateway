// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

// This file is the agent-managed model runtime's top-level driver -- the
// single object main.go hands to internal/agent (as that package's
// unexported runtimeDriver seam), constructed UNCONDITIONALLY (fix round 1,
// I3): feature negotiation is decided, and continuously re-decided, by
// Sync's own step 1, never by a one-shot startup probe. It mirrors
// internal/proxy.Driver (cert_mode=proxy) / internal/agent's certProxyDriver
// shape: one Sync method the agent's run loop calls on its own cadence/wake
// schedule, a Status accessor and an Active accessor for the outgoing
// telemetry sample, and a Close for shutdown.
//
// Sync's three steps (design doc
// docs/superpowers/specs/2026-08-25-agent-runtime-manager-design.md §9/§10.2):
//
//  1. Re-check feature negotiation. runtime_manager can be revoked (or
//     granted) by the gateway at any time -- FeaturesClient's own
//     ETag-conditional GET makes re-checking on every Sync cheap (almost
//     always a cached 304). Inactive -> drain everything and stop
//     reconciling until it reappears. Deliberately NOT gated by config
//     source (gateway vs file): feature negotiation governs whether this
//     Driver does anything at all, independent of where the desired-state
//     document comes from.
//  2. Load the desired config: source.Load(ctx), UNLESS pushed carries a
//     real WS-delivered document and the source is a *GatewaySource, in
//     which case ApplyPushed consumes it directly (zero extra round trip).
//     A *FileSource ignores a pushed payload outright (spec §10.2: file
//     mode never consumes the gateway's document) -- pushed is simply not
//     looked at on that path, exactly as if it were nil.
//  3. Apply the config to the manager and (re)bind the router listener --
//     unconditionally on every active Sync (fix round 1, I1), not merely
//     when the SOURCE reports changed (fix round 1, C1: "different from
//     what I last fetched" does not survive a process restart --
//     GatewaySource seeds its ETag from disk -- or a drain, but "already
//     applied to the manager" is exactly the fact that does not survive
//     either of those, so it is tracked separately via Driver.applied).
//     In file mode, also send the redacted upward report (BuildReport)
//     over the reporter, so the portal's report view reflects what this
//     agent is actually running from.
package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// runtimeManagerFeature is the single feature name this Driver negotiates.
// internal/runtime cannot import internal/agent's Features registry --
// internal/agent already imports internal/runtime for the runtimeDriver
// seam, so the reverse edge would be an import cycle -- which is why the
// name is duplicated here rather than referenced from
// agent.FeatureNames(). The two must always name the identical string;
// internal/agent/features_test.go's registry test keeps the agent side
// honest, and this constant is internal/runtime's own half of the same
// contract.
const runtimeManagerFeature = "runtime_manager"

// RuntimeReporter is the upward file-mode report sink: both *client.Client
// and *client.WSSender satisfy it already via their existing
// PostRuntimeReport methods (Task 17). A nil RuntimeReporter simply means
// Sync's file-mode report step is skipped -- main.go's own comment on this
// says why it is kept as a checked nil rather than asserted away.
//
// The name reads as runtime.RuntimeReporter from outside this package
// (which is already named "runtime"), but it names the type exactly as the
// design spec/task brief for this feature do verbatim; a different name
// here would drift from every other document that already refers to it as
// RuntimeReporter.
//
//nolint:revive // intentional stutter -- see the comment above
type RuntimeReporter interface {
	PostRuntimeReport(ctx context.Context, raw json.RawMessage) error
}

// driverManager is the Driver's own minimal view of *Manager -- small and
// unexported, mirroring router.go's managerPort precedent, so driver_test.go
// can inject a hand-written fake and exercise Sync's control flow without a
// real process-supervision stack. It is a superset of managerPort (adds
// Apply/Transitions) so StartRouter below can hand the SAME value straight
// to router.go's unexported newRouter constructor. *Manager satisfies this
// today with no changes.
type driverManager interface {
	Apply(cfg Config)
	Status() []Status
	Transitions() <-chan struct{}
	EnsureRunning(ctx context.Context, upstreamModel string) (endpoint string, release func(), err error)
	LoadedModels() []string
}

// Driver is the top-level agent-managed-runtime object internal/agent holds
// as its unexported runtimeDriver. Constructed unconditionally by main.go
// now (fix round 1, I3) -- internal/agent gates the Runtimes/LoadedModels
// override on Active(), not on this pointer being non-nil, so a Driver that
// has never (yet) negotiated the feature is still a complete no-op on the
// wire. See the package doc above for Sync's contract.
type Driver struct {
	mgr      driverManager
	src      Source
	features *FeaturesClient
	reporter RuntimeReporter
	// bindHost is the operator-resolved router bind address (fix round 1,
	// I2), threaded in at construction time from main.go (which alone may
	// import both internal/config and internal/proxy). "" means "no
	// specific host resolved" -- StartRouter then binds all interfaces and
	// says so at Warn, rather than silently doing the unrestricted thing.
	bindHost string

	// syncMu serializes Sync's body. internal/agent's own triggerRuntimeSync
	// already single-flights calls via CompareAndSwap (this package's
	// trigger pattern is the AGENT's, not this Driver's own -- see the
	// package doc), but Sync is exported and directly callable by any
	// future caller (as driver_test.go does), so this guards against two
	// Syncs racing each other's applied/active bookkeeping and router
	// (re)bind regardless of caller discipline. Held for the duration of
	// one Sync call only, never across a retried network round trip.
	syncMu sync.Mutex
	// applied tracks whether the CURRENTLY HELD config has been applied to
	// the manager AT LEAST ONCE in this Driver's lifetime -- fix round 1,
	// C1. This is NOT the same fact as Source.Load's own `changed` return,
	// which only means "different from the last document THIS SOURCE
	// returned": GatewaySource seeds its ETag from its own disk cache, so a
	// reachable gateway answers 304 (changed=false) starting on the agent's
	// very SECOND start, and an unreachable one falls back to the same
	// cached, changed=false config -- either way a config that was never
	// actually handed to the manager in THIS process would otherwise never
	// be applied at all. Cleared by stopAll so the next active Sync
	// (recovering from a drain) also re-applies unconditionally.
	applied bool
	// active is the CURRENT feature-negotiation outcome (Sync step 1),
	// read from a different goroutine (Active, called from
	// internal/agent.collectOnce on whatever goroutine is building a
	// sample) than the one that writes it (Sync, always via
	// triggerRuntimeSync's single-flighted goroutine) -- an atomic.Bool,
	// not a plain bool guarded only by syncMu, so a read is never blocked
	// behind a live HTTP round trip. atomic.Bool.Swap's return value is
	// what makes "log a transition exactly once" race-free without a
	// separate flag.
	active atomic.Bool

	routerMu     sync.Mutex
	routerListen int
	routerSrv    *http.Server
	// closed is set by Close and checked by StartRouter, both under
	// routerMu (fix round 1, M3): Run returns on context cancellation
	// without waiting for an in-flight triggerRuntimeSync goroutine, so a
	// Sync call that is still running when Close executes could otherwise
	// rebind the router listener AFTER shutdown has already torn it down.
	closed bool
}

// NewDriver builds a Driver over m (never nil in production: main.go always
// constructs a real *Manager before reaching this call), binding the router
// to bindHost when it (re)binds (fix round 1, I2) -- "" lets StartRouter
// fall back to all interfaces, logging that choice rather than silently
// doing it. A literal nil m is still handled explicitly -- never assigned
// through the driverManager interface unchecked -- to avoid the classic
// typed-nil-interface trap this module documents throughout (router.go's
// NewRouter is the precedent this mirrors exactly).
func NewDriver(m *Manager, src Source, features *FeaturesClient, reporter RuntimeReporter, bindHost string) *Driver {
	if m == nil {
		return newDriver(nil, src, features, reporter, bindHost)
	}
	return newDriver(m, src, features, reporter, bindHost)
}

// newDriver is NewDriver's unexported worker, taking the driverManager
// interface directly so driver_test.go can construct a Driver over a fake
// manager without going through the *Manager-typed exported constructor
// (the NewRouter/newRouter split this package already uses).
func newDriver(m driverManager, src Source, features *FeaturesClient, reporter RuntimeReporter, bindHost string) *Driver {
	return &Driver{mgr: m, src: src, features: features, reporter: reporter, bindHost: bindHost}
}

// Sync fetches the gateway's currently declared feature set, confirms
// runtime_manager is still mutually active, loads the desired config (from
// pushed when applicable, else source.Load), and applies it to the manager
// and (re)binds the router listener -- both unconditionally while active,
// not merely on a changed load (fix round 1, C1+I1) -- and (file mode)
// sends the redacted upward report on an actual change. See the package doc
// for the full contract. Safe to call concurrently with itself (syncMu),
// though the agent's own single-flight trigger already ensures it never is
// in practice.
func (d *Driver) Sync(ctx context.Context, pushed json.RawMessage) {
	d.syncMu.Lock()
	defer d.syncMu.Unlock()

	// M2: d.mgr is a genuine nil interface only via the defensive
	// NewDriver(nil, ...) path (never production) -- a complete no-op
	// here, consistent with Status/Transitions' own nil-safety, rather than
	// a panic on the Apply call below.
	if d.mgr == nil {
		return
	}

	if !d.featureActive(ctx) {
		d.stopAll()
		return
	}
	if !d.active.Swap(true) {
		slog.Info("runtime: runtime_manager negotiated active; agent-managed runtime running")
	}

	cfg, changed, err := d.load(ctx, pushed)
	if err != nil {
		// ApplyPushed's own contract: a malformed pushed document returns
		// the last known-good config unchanged alongside this error, so
		// there is nothing unsafe about using cfg below regardless --
		// changed is simply false in that case. Logging here is the only
		// place this specific failure would otherwise be visible.
		slog.Warn("runtime: pushed runtime-config invalid; keeping current config", "error", err)
	}

	// I1: StartRouter is idempotent in the already-desired state and safe
	// to call on every active Sync, not just a changed one -- a bind
	// failure at startup (port busy, TIME_WAIT, another service) must not
	// be stuck behind a single Warn log for the rest of the process's life
	// just because the config never changes again. On a failed Load, cfg
	// is still the last known-good document, so this can never tear down a
	// good listener.
	if err := d.StartRouter(cfg.RouterListen); err != nil {
		slog.Warn("runtime: router (re)bind failed", "listen", cfg.RouterListen, "error", err)
	}

	// C1: `changed` means "different from the config Source.Load last
	// returned", NOT "already applied to the manager" -- those two facts
	// only coincide within a single process's uptime against a config that
	// never repeats. Apply whenever the config differs OR nothing has ever
	// actually been applied yet in this Driver's lifetime (a fresh
	// process, or recovering from a drain via stopAll clearing applied).
	if !changed && d.applied {
		return
	}

	d.mgr.Apply(cfg)
	d.applied = true

	if _, isFile := d.src.(*FileSource); isFile {
		d.sendReport(ctx, cfg)
	}
}

// featureActive fetches the gateway's currently declared feature set and
// reports whether runtime_manager is present in it. FeaturesClient.Fetch
// already implements the ETag-conditional-GET / last-known-good discipline
// (a transient gateway hiccup returns the LAST fetched set, never an empty
// one caused only by a network blip) -- Sync trusts that contract exactly
// as every other caller of Fetch in this module does. A nil features client
// (never constructed this way in production; defensive only) is treated as
// inactive rather than panicking.
func (d *Driver) featureActive(ctx context.Context) bool {
	if d.features == nil {
		return false
	}
	names, err := d.features.Fetch(ctx)
	if err != nil {
		// Fix round 1, I3: distinguish "could not even ask" from "asked,
		// and the gateway does not declare it" (a normal, silent outcome,
		// see the loop below) -- worth an operator's attention at Warn if
		// it persists (e.g. gateway down/restarting/mid-rollout).
		// Currently unreachable in practice: FeaturesClient.Fetch's own
		// documented contract never pairs a non-nil error with any outcome
		// (every failure mode already resolves to (last-known-good, nil)) --
		// kept here defensively in case that contract is ever loosened.
		slog.Warn("runtime: gateway features fetch failed; treating runtime_manager as inactive until the next check", "error", err)
	}
	for _, n := range names {
		if n == runtimeManagerFeature {
			return true
		}
	}
	return false
}

// stopAll drains every managed process (an empty Apply, reusing Manager's
// own drain-on-removal reconciliation -- no separate "stop everything"
// method needed on Manager), tears down the router listener, clears
// applied so the next active Sync re-applies unconditionally (C1), and logs
// the transition into the inactive state exactly once (active.Swap's
// return value, not a separate flag).
func (d *Driver) stopAll() {
	d.mgr.Apply(emptyConfig())
	d.applied = false
	if err := d.StartRouter(0); err != nil {
		slog.Debug("runtime: router stop failed", "error", err)
	}
	if d.active.Swap(false) {
		slog.Warn("runtime: runtime_manager is not active on both agent and gateway; the agent-managed runtime is stopped")
	}
}

// Active reports the CURRENT feature-negotiation outcome -- the last Sync's
// own step-1 result. internal/agent.collectOnce gates the Runtimes/
// LoadedModels override on this, not merely on a Driver existing (fix
// round 1, I3): the Driver is now constructed unconditionally by main.go,
// so "a driver exists" no longer implies "this agent's runtime feature is
// currently doing anything". Starts false (the atomic.Bool zero value)
// until the first Sync completes -- main.go no longer blocks startup
// waiting for that, so an early telemetry sample correctly shows no runtime
// data rather than guessing.
func (d *Driver) Active() bool {
	return d.active.Load()
}

// load resolves the desired config for one Sync call: a non-empty pushed
// payload on a *GatewaySource goes through ApplyPushed (zero extra round
// trip); a *FileSource ignores pushed outright (spec §10.2); every other
// case (no payload, or a Source this package does not specifically
// recognize) goes through the ordinary Load path.
func (d *Driver) load(ctx context.Context, pushed json.RawMessage) (Config, bool, error) {
	if len(pushed) > 0 {
		if gw, ok := d.src.(*GatewaySource); ok {
			return gw.ApplyPushed(pushed)
		}
		// *FileSource (or any other Source implementation): a pushed
		// payload is simply not looked at; fall through to Load exactly as
		// if pushed were nil.
	}
	return d.src.Load(ctx)
}

// sendReport builds the redacted file-mode report from cfg and posts it,
// best-effort: a failure is logged and left for the next changed Sync (or a
// fresh WS reconnect resending the cached bytes, see
// WSSender.resendRuntimeReport, or ResendReport's own periodic backstop
// below) to retry. Only ever called when d.src is a *FileSource (Sync's own
// guard; ResendReport re-checks it independently since it can be called
// without going through Sync at all).
func (d *Driver) sendReport(ctx context.Context, cfg Config) {
	if d.reporter == nil {
		return
	}
	var parseErr string
	if fs, ok := d.src.(*FileSource); ok {
		parseErr, _ = fs.LastParseError()
	}
	raw, err := BuildReport(cfg, "file", parseErr, time.Now().UTC())
	if err != nil {
		slog.Warn("runtime: build runtime report failed", "error", err)
		return
	}
	if err := d.reporter.PostRuntimeReport(ctx, raw); err != nil {
		slog.Warn("runtime: post runtime report failed", "error", err)
	}
}

// ResendReport re-sends the current file-mode report unconditionally,
// regardless of whether the local file has changed since the last Sync --
// fix round 1, I5: design spec §10.2 requires a periodic resend as a
// POST-transport backstop (the WS transport already gets this for free on
// every reconnect, via WSSender's own cached-frame resend). Rather than
// teaching this Driver (or internal/agent) which transport is in use, the
// caller (internal/agent) piggybacks this on its EXISTING system-report
// ticker cadence -- exactly the same transport-agnostic mechanism the
// system report itself already relies on (a WS agent resends it on
// reconnect AND on that same ticker; the ticker is what covers POST). A
// no-op when d.src is not a *FileSource (the report is a file-mode-only
// concept) or there is no reporter to send it to. Does not take syncMu:
// Source.Load and sendReport are each independently safe for concurrent
// use, and at most this races an in-flight Sync into sending the report
// bytes twice in quick succession, which is harmless (the report is
// idempotent, last-write-wins on the gateway side).
func (d *Driver) ResendReport(ctx context.Context) {
	fs, ok := d.src.(*FileSource)
	if !ok || d.reporter == nil {
		return
	}
	cfg, _, _ := fs.Load(ctx)
	d.sendReport(ctx, cfg)
}

// Status returns the manager's current per-spec status snapshot, or nil
// when there is no manager (the NewDriver(nil, ...) defensive case, never
// hit in production).
func (d *Driver) Status() []Status {
	if d.mgr == nil {
		return nil
	}
	return d.mgr.Status()
}

// Transitions exposes the manager's state-change doorbell for
// internal/agent's optional immediate-resample wake: mirrors
// certWaker/trustWaker there (a type assertion on Deps.RuntimeDriver finds
// this method). A nil manager yields a nil channel, which a select simply
// never receives from -- the same nil-channel discipline this whole feature
// follows throughout.
func (d *Driver) Transitions() <-chan struct{} {
	if d.mgr == nil {
		return nil
	}
	return d.mgr.Transitions()
}

// StartRouter (re)binds the router-port listener to listen on d.bindHost,
// tearing down any previous listener first. listen<=0 means "no
// server_agent application configured for this server" (task-7-report.md:
// an empty runtime-config document reports router_listen 0) -- the router
// is simply not served in that case, and any previously bound listener is
// torn down. Idempotent: calling it again with the SAME already-bound port
// is a no-op, so Sync can call this unconditionally on every active cycle
// (I1) without dropping in-flight connections when nothing changed.
//
// Bind host (fix round 1, I2): d.bindHost is operator-resolved by main.go
// (an explicit OP_AGENT_RUNTIME_ROUTER_BIND override, else a mesh-identity
// derivation, mirroring internal/proxy's own bindHost -- see
// proxy.DeriveBindHost). An empty d.bindHost falls back to all interfaces,
// which is announced at Warn (not merely a Go comment) precisely because it
// is the unauthenticated, most-exposed choice: this router authenticates
// nothing, so anyone who can reach it can enumerate/run/evict managed
// models.
func (d *Driver) StartRouter(listen int) error {
	d.routerMu.Lock()
	defer d.routerMu.Unlock()

	if d.closed {
		return nil // M3: Close already ran; nothing left to (re)bind
	}
	if listen == d.routerListen && (listen <= 0 || d.routerSrv != nil) {
		return nil // already in the desired state
	}
	d.stopRouterLocked()
	d.routerListen = listen
	if listen <= 0 {
		return nil
	}

	addr := net.JoinHostPort(d.bindHost, strconv.Itoa(listen))
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("runtime: bind router listener on %s: %w", addr, err)
	}
	if d.bindHost == "" {
		slog.Warn("runtime: router bind host not configured/derivable; listening on ALL interfaces (unauthenticated) -- set OP_AGENT_RUNTIME_ROUTER_BIND to restrict it", "listen", listen)
	}
	srv := &http.Server{Handler: newRouter(d.mgr)}
	d.routerSrv = srv
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Warn("runtime: router listener stopped unexpectedly", "listen", listen, "error", err)
		}
	}()
	slog.Info("runtime: router listening", "host", d.bindHost, "listen", listen)
	return nil
}

// stopRouterLocked closes the current router server, if any. Close (not
// Shutdown) is deliberate: the router's proxied requests include
// long-lived streaming connections with no natural end, so Shutdown would
// block waiting for them; a Driver-level "stop everything" (feature
// deactivated, or the gateway pushed a different port) needs an immediate
// stop, not a graceful per-connection drain -- that discipline belongs to
// the MANAGER's own drain-stop of the model processes, not to this
// listener. Caller must hold routerMu.
func (d *Driver) stopRouterLocked() {
	if d.routerSrv == nil {
		return
	}
	_ = d.routerSrv.Close()
	d.routerSrv = nil
}

// Close marks the Driver closed and stops the router listener, if one is
// bound (fix round 1, M3: closed is checked by StartRouter, under the same
// routerMu, so a Sync still in flight when Close runs -- Run returns on
// context cancellation without waiting for one -- cannot resurrect the
// listener afterward). It deliberately does NOT close the underlying
// Manager -- main.go owns that lifecycle directly (its own
// `defer mgr.Close()`, right where the Manager is constructed), since the
// Manager exists independently of whether a Driver happens to wrap it at
// any given moment.
func (d *Driver) Close() {
	d.routerMu.Lock()
	d.closed = true
	d.stopRouterLocked()
	d.routerMu.Unlock()
}
