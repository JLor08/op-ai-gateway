// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

// This file is the agent-managed model runtime's top-level driver -- the
// single object main.go hands to internal/agent (as that package's
// unexported runtimeDriver seam) once feature negotiation has decided
// runtime_manager is active for this agent<->gateway pair. It mirrors
// internal/proxy.Driver (cert_mode=proxy) / internal/agent's certProxyDriver
// shape exactly: one Sync method the agent's run loop calls on its own
// cadence/wake schedule, a Status accessor for the outgoing telemetry
// sample, and a Close for shutdown.
//
// Sync's three steps (design doc
// docs/superpowers/specs/2026-08-25-agent-runtime-manager-design.md §9/§10.2):
//
//  1. Re-check feature negotiation. runtime_manager can be revoked by the
//     gateway at any time (a downgrade, a reconfiguration) after this
//     Driver was already constructed at startup with it active --
//     FeaturesClient's own ETag-conditional GET makes re-checking on every
//     Sync cheap (almost always a cached 304), and the moment the gateway
//     stops declaring it, Sync drains every managed process and stops
//     reconciling until the feature reappears. Deliberately NOT gated by
//     config source (gateway vs file): feature negotiation governs whether
//     this Driver does anything at all, independent of where the
//     desired-state document comes from.
//  2. Load the desired config: source.Load(ctx), UNLESS pushed carries a
//     real WS-delivered document and the source is a *GatewaySource, in
//     which case ApplyPushed consumes it directly (zero extra round trip).
//     A *FileSource ignores a pushed payload outright (spec §10.2: file
//     mode never consumes the gateway's document) -- pushed is simply not
//     looked at on that path, exactly as if it were nil.
//  3. changed -> manager.Apply(cfg) and (re)bind the router listener to
//     cfg.RouterListen; in file mode, also send the redacted upward report
//     (BuildReport) over the reporter, so the portal's report view reflects
//     what this agent is actually running from.
package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
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
// as its unexported runtimeDriver (nil = feature absent, the no-op
// invariant that package documents throughout). See the package doc above
// for Sync's three-step contract.
type Driver struct {
	mgr      driverManager
	src      Source
	features *FeaturesClient
	reporter RuntimeReporter

	// syncMu serializes Sync's body. internal/agent's own triggerRuntimeSync
	// already single-flights calls via CompareAndSwap (this package's
	// trigger pattern is the AGENT's, not this Driver's own -- see the
	// package doc), but Sync is exported and directly callable by any
	// future caller (as driver_test.go does), so this guards against two
	// Syncs racing each other's blocked-state bookkeeping and router
	// (re)bind regardless of caller discipline. Held for the duration of
	// one Sync call only, never across a retried network round trip.
	syncMu sync.Mutex

	// blocked tracks whether the LAST Sync found runtime_manager inactive,
	// so the "blocked" note logs once per transition (into, and back out
	// of, the blocked state) rather than once per tick -- the same
	// loggedMissing/warnMissingOnce idiom config_client.go and
	// features_client.go already use in this package.
	blocked bool

	routerMu     sync.Mutex
	routerListen int
	routerSrv    *http.Server
}

// NewDriver builds a Driver over m (never nil in production: main.go always
// constructs a real *Manager before reaching this call). A literal nil is
// still handled explicitly -- never assigned through the driverManager
// interface unchecked -- to avoid the classic typed-nil-interface trap this
// module documents throughout (router.go's NewRouter is the precedent this
// mirrors exactly).
func NewDriver(m *Manager, src Source, features *FeaturesClient, reporter RuntimeReporter) *Driver {
	if m == nil {
		return newDriver(nil, src, features, reporter)
	}
	return newDriver(m, src, features, reporter)
}

// newDriver is NewDriver's unexported worker, taking the driverManager
// interface directly so driver_test.go can construct a Driver over a fake
// manager without going through the *Manager-typed exported constructor
// (the NewRouter/newRouter split this package already uses).
func newDriver(m driverManager, src Source, features *FeaturesClient, reporter RuntimeReporter) *Driver {
	return &Driver{mgr: m, src: src, features: features, reporter: reporter}
}

// Sync fetches the gateway's currently declared feature set, confirms
// runtime_manager is still mutually active, loads the desired config (from
// pushed when applicable, else source.Load), and -- on change -- applies it
// to the manager, (re)binds the router listener, and (file mode) sends the
// redacted upward report. See the package doc for the exact three-step
// contract. Safe to call concurrently with itself (syncMu), though the
// agent's own single-flight trigger already ensures it never is in
// practice.
func (d *Driver) Sync(ctx context.Context, pushed json.RawMessage) {
	d.syncMu.Lock()
	defer d.syncMu.Unlock()

	if !d.featureActive(ctx) {
		d.stopAll()
		return
	}
	d.clearBlocked()

	cfg, changed, err := d.load(ctx, pushed)
	if err != nil {
		// ApplyPushed's own contract: a malformed pushed document returns
		// the last known-good config unchanged alongside this error, so
		// there is nothing unsafe about falling through below regardless --
		// changed is simply false in that case. Logging here is the only
		// place this specific failure would otherwise be visible.
		slog.Warn("runtime: pushed runtime-config invalid; keeping current config", "error", err)
	}
	if !changed {
		return
	}

	d.mgr.Apply(cfg)
	if err := d.StartRouter(cfg.RouterListen); err != nil {
		slog.Warn("runtime: router (re)bind failed", "listen", cfg.RouterListen, "error", err)
	}
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
		// Cannot actually happen per Fetch's own documented contract (every
		// failure mode already resolves to (last-known-good, nil)), but
		// logged rather than ignored outright in case that contract is ever
		// loosened.
		slog.Debug("runtime: gateway features fetch failed", "error", err)
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
// method needed on Manager) and tears down the router listener, then logs
// the transition into the blocked state exactly once (see the blocked
// field's doc).
func (d *Driver) stopAll() {
	d.mgr.Apply(emptyConfig())
	if err := d.StartRouter(0); err != nil {
		slog.Debug("runtime: router stop failed", "error", err)
	}
	if !d.blocked {
		d.blocked = true
		slog.Warn("runtime: runtime_manager is not active on both agent and gateway; the agent-managed runtime is stopped")
	}
}

// clearBlocked logs the reverse transition (see stopAll) exactly once.
func (d *Driver) clearBlocked() {
	if d.blocked {
		d.blocked = false
		slog.Info("runtime: runtime_manager active again; agent-managed runtime resuming")
	}
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
// WSSender.resendRuntimeReport) to retry. Only ever called when d.src is a
// *FileSource (Sync's own guard).
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

// StartRouter (re)binds the router-port listener to listen, tearing down
// any previous listener first. listen<=0 means "no server_agent
// application configured for this server" (task-7-report.md: an empty
// runtime-config document reports router_listen 0) -- the router is simply
// not served in that case, and any previously bound listener is torn down.
// Idempotent: calling it again with the SAME already-bound port is a no-op,
// so Sync can call this unconditionally on every changed config without
// dropping in-flight connections over an unrelated spec edit.
//
// Binds on all interfaces (not 127.0.0.1): unlike the managed model
// processes themselves (loopback-only, reached exclusively through this
// router), the router port is what the GATEWAY connects to -- over the
// NetBird mesh or a plain LAN, exactly like an ollama/vllm application's own
// listen address -- so it must be reachable from outside this host.
func (d *Driver) StartRouter(listen int) error {
	d.routerMu.Lock()
	defer d.routerMu.Unlock()

	if listen == d.routerListen && (listen <= 0 || d.routerSrv != nil) {
		return nil // already in the desired state
	}
	d.stopRouterLocked()
	d.routerListen = listen
	if listen <= 0 {
		return nil
	}

	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", listen))
	if err != nil {
		return fmt.Errorf("runtime: bind router listener on port %d: %w", listen, err)
	}
	srv := &http.Server{Handler: newRouter(d.mgr)}
	d.routerSrv = srv
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Warn("runtime: router listener stopped unexpectedly", "listen", listen, "error", err)
		}
	}()
	slog.Info("runtime: router listening", "listen", listen)
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

// Close stops the router listener, if one is bound. It deliberately does
// NOT close the underlying Manager -- main.go owns that lifecycle directly
// (its own `defer mgr.Close()`, right where the Manager is constructed),
// since the Manager exists independently of whether a Driver happens to
// wrap it at any given moment.
func (d *Driver) Close() {
	_ = d.StartRouter(0)
}
