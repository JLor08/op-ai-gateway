// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

// Package agent runs the collect→merge→post ticker loop: it gathers a host
// sample plus every detected GPU collector (and an optional inference scrape)
// into one sample.Sample and pushes it to the gateway on the configured
// interval, surviving individual collector/push failures and shutting down
// cleanly when its context is cancelled.
package agent

import (
	"context"
	"encoding/json"
	"log/slog"
	"op-ai-server-agent/internal/certinstall"
	"op-ai-server-agent/internal/collector"
	"op-ai-server-agent/internal/config"
	"op-ai-server-agent/internal/proxy"
	"op-ai-server-agent/internal/sample"

	// runtimectl is internal/runtime, aliased because this file already
	// imports the standard library's "runtime" package (used for
	// runtime.GOOS/runtime.GOARCH below) -- internal/runtime's own package
	// doc calls out this exact collision and names "runtimectl" as the
	// alias every importer that needs both should use.
	runtimectl "op-ai-server-agent/internal/runtime"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// Version is the agent's self-reported version, sent as agent_version.
// Bump rule (design spec §9, feature negotiation): MINOR when Features
// gains a new entry (as here -- 0.1.0 -> 0.2.0 for "runtime_manager"),
// PATCH for anything else. TestFeatureRegistry enforces the MINOR case by
// failing if any Feature.Since outruns Version; it cannot catch a forgotten
// PATCH bump after a plain bugfix, which stays a process rule.
const Version = "0.2.0"

// collectTimeout bounds each individual collector invocation so a wedged
// external CLI (nvidia-smi/rocm-smi/ioreg) cannot block the single-goroutine
// loop indefinitely. It is comfortably above a healthy collect (tens of ms) and
// well below "forever"; a hung collector is cancelled and the loop advances.
const collectTimeout = 2 * time.Second

// poster is the minimal telemetry sink the loop needs; *client.Client satisfies
// it and tests inject a fake.
type poster interface {
	Post(context.Context, *sample.Sample) error
}

// reporter is the one-time static-hardware-inventory sink. Both *client.Client and
// *client.WSSender satisfy it (the same object the poster interface refers to), so
// New derives it from the poster via a type assertion.
type reporter interface {
	PostSystemReport(ctx context.Context, r *sample.SystemReport) error
}

// certSyncer is the Phase 2 certificate-distribution contract Agent needs;
// *certinstall.Installer satisfies it. It is an interface (not the concrete
// type) so a nil value is a well-typed, panic-free no-op -- every call site
// that reads a.certSync guards it with a plain "!= nil" check -- and so tests
// can inject a fake that counts Sync calls instead of a real installer talking
// HTTP to a gateway.
type certSyncer interface {
	Sync(ctx context.Context) (certinstall.Report, bool, error)
	Report() certinstall.Report
}

// certWaker is the optional interface a poster may additionally implement to
// signal "check for a new certificate now": a cert_update doorbell arriving
// over the WebSocket transport, or the connection being (re)established. Only
// *client.WSSender implements it today -- the POST transport has no server->
// agent channel at all, so certificate sync there relies solely on
// newCertTicker's periodic cadence. New derives it via a type assertion,
// mirroring how reporter is derived from the same poster value.
type certWaker interface {
	CertUpdates() <-chan struct{}
}

type trustSyncer interface {
	Refresh(context.Context) error
	DurableFingerprints() []string
}

type trustWaker interface {
	TrustUpdates() <-chan struct{}
}

// certProxyDriver is the optional cert_mode=proxy hook set: on each
// certificate-poll tick the agent refreshes its TLS-proxy routes from the
// gateway (SyncRoutes) and, after the installer reports a real leaf install,
// hot-swaps that leaf into the running proxy listeners (ReloadCert). Status
// (Certificates P4 Task 4) reports the observed state of those routes for the
// outgoing telemetry sample. *proxy.Driver satisfies it. It is an interface
// (not the concrete type) so a nil value is a well-typed no-op and tests can
// inject a counting fake; main.go constructs a real driver ONLY for
// cert_mode=proxy, so off/files leave it nil (and collectOnce's nil check
// below is what keeps ProxyRoutes absent from those agents' samples).
type certProxyDriver interface {
	SyncRoutes(ctx context.Context)
	ReloadCert()
	Status() []proxy.RouteStatus
}

// runtimeDriver is the optional agent-managed model runtime hook set,
// symmetric with certProxyDriver above: *runtimectl.Driver satisfies it.
// Sync fetches/reconciles the desired runtime-config document (see
// runtimectl.Driver.Sync's own doc for the exact three-step contract);
// Status reports the live per-spec state for the outgoing telemetry
// sample. It is an interface (not the concrete type) so a nil value is a
// well-typed no-op -- every call site below guards it with a plain
// "!= nil" check -- and so tests can inject a counting fake instead of a
// real process-supervision stack.
//
// Fix round 1, I3: main.go now constructs the driver UNCONDITIONALLY
// (never gated on a one-shot startup features probe, which used to freeze
// "inactive" for the whole process on any transient failure at boot).
// Active reports the driver's OWN current feature-negotiation outcome
// (its Sync's own step 1, re-checked on every call) -- collectOnce below
// gates the Runtimes/LoadedModels override on Active(), NOT merely on this
// interface being non-nil, precisely because "a driver exists" no longer
// implies "this agent's runtime feature is doing anything right now". A
// nil runtimeDriver (still possible: a test, or a hypothetical build that
// never wires one at all) is the only remaining case that keeps a legacy
// agent's telemetry sample byte-identical purely by construction (no
// "runtimes" key at all, see collectOnce below); an inactive-but-non-nil
// driver reaches the identical byte-neutral outcome via the Active() gate
// instead.
type runtimeDriver interface {
	Sync(ctx context.Context, pushed json.RawMessage)
	Status() []runtimectl.Status
	Active() bool
}

// runtimeReportResender is the optional interface a runtimeDriver may
// additionally implement to accept a periodic, unconditional resend of its
// file-mode report (fix round 1, I5): design spec §10.2 requires this as a
// POST-transport backstop (the WS transport already gets it for free on
// every reconnect via WSSender's own cached-frame resend). Rather than
// teaching this package or the driver which transport is in use, Run
// piggybacks the call on the EXISTING system-report ticker cadence --
// exactly the same transport-agnostic mechanism the system report itself
// already relies on. *runtimectl.Driver implements it; a fake that omits
// it (or a nil runtimeDriver) simply never gets this call.
type runtimeReportResender interface {
	ResendReport(ctx context.Context)
}

// runtimeWaker is the optional interface a poster may additionally
// implement to signal "a new runtime-config document is available now":
// a WS-pushed runtime_config frame, or this connection being (re)established
// (a nil payload in that case -- "resync over HTTP", see
// client.WSSender.RuntimeUpdates' own doc). Only *client.WSSender
// implements it today, mirroring certWaker/trustWaker exactly -- the POST
// transport has no server->agent channel at all, so runtime-config
// reconciliation there relies solely on newRuntimeTicker's periodic
// cadence. NewFromDeps derives it via a type assertion on d.Poster, exactly
// like certWaker/trustWaker.
type runtimeWaker interface {
	RuntimeUpdates() <-chan json.RawMessage
}

// runtimeTransitionsWaker is the optional interface a runtimeDriver may
// additionally implement to expose an immediate-resample doorbell: a spec
// state transition (design spec §7: "reverse direction latency" -- a
// portal click should feel like network latency, not the next 1s telemetry
// tick... let alone the 60s runtime-config poll). *runtimectl.Driver
// implements it (delegating to its Manager's own Transitions() channel).
// NewFromDeps derives it via a type assertion on d.RuntimeDriver, mirroring
// how certWaker/trustWaker are derived from d.Poster.
type runtimeTransitionsWaker interface {
	Transitions() <-chan struct{}
}

var trustRefreshInterval = 15 * time.Minute

// runtimePollInterval is the periodic backstop cadence for the runtime
// driver's Sync (analogous to certPollInterval, but a single fixed const
// rather than a transport-dependent choice): the WS wake/reconnect and the
// Transitions() doorbell already cover the common cases promptly, so this
// is purely a safety net against a missed wake -- no test needs to shrink
// it (every runtime-driver test in this package drives Sync directly via
// the wake/transitions channels or a direct triggerRuntimeSync call, never
// by waiting out this ticker), so unlike this file's other timing knobs it
// stays a plain const.
const runtimePollInterval = 60 * time.Second

// certPollIntervalWS/POST are the AUTOMATIC certificate-poll cadences (used
// when cfg.CertPollInterval == 0) selected by transport. WebSocket already
// gets a push (a cert_update doorbell) plus a wake on every (re)connect, so its
// periodic poll is only a distant backstop; POST has neither of those signals,
// so it must poll far more often to keep "installed" from lagging an issued
// certificate for long. An explicitly configured positive CertPollInterval
// (already floored at 1m by config.Load) always overrides both.
const (
	certPollIntervalWS   = 6 * time.Hour
	certPollIntervalPOST = 15 * time.Minute
)

// certPollInterval resolves cfg's certificate-poll cadence: an explicit,
// already-validated positive value wins outright; otherwise (0 = automatic)
// the concrete duration is chosen by cfg.Transport per the constants above.
func certPollInterval(cfg config.Config) time.Duration {
	if cfg.CertPollInterval > 0 {
		return cfg.CertPollInterval
	}
	if cfg.Transport == config.TransportWebSocket {
		return certPollIntervalWS
	}
	return certPollIntervalPOST
}

// certModeOff reports whether certificate distribution is disabled for this
// agent. Config.Load always normalizes an empty CertMode to config.CertModeOff
// before Agent ever sees it, but a config.Config built directly (every
// existing test in this package and the wider module) leaves CertMode at its
// Go zero value (""); treating "" the same as the explicit "off" constant
// keeps the ticker/fetch gate correct in both cases without requiring every
// test fixture to spell out CertMode.
func (a *Agent) certModeOff() bool {
	return a.cfg.CertMode == "" || a.cfg.CertMode == config.CertModeOff
}

// defaultSystemReportInterval is a safety fallback for a directly-constructed Agent
// whose cfg did not set SystemReportInterval (config.Load always sets a positive
// value). Keep in sync with config.defaultSystemReportInterval.
const defaultSystemReportInterval = 30 * time.Minute

// wattsLog renders a nullable watt pointer for a log attribute: the value, or
// "none" when unavailable this cycle (a *float64 would otherwise log as a pointer).
func wattsLog(v *float64) any {
	if v == nil {
		return "none"
	}
	return *v
}

// tempLog renders a nullable Celsius pointer for a log attribute. Celsius and
// watts render identically (the value, or "none" when unavailable), so this
// delegates to wattsLog rather than keeping a byte-identical copy; the two
// names stay separate so each call site documents its own unit.
func tempLog(v *float64) any {
	return wattsLog(v)
}

// hardwareCollectBudget bounds the whole one-shot hardware collection (its sub-
// sources are individually bounded inside collector.CollectHardware).
const hardwareCollectBudget = 15 * time.Second

// Agent composes the host + GPU collectors, an optional scraper, and a poster
// into a periodic telemetry loop.
type Agent struct {
	cfg     config.Config
	host    collector.HostCollector
	gpus    []collector.GPUCollector
	scraper collector.Scraper
	loaded  collector.LoadedModelLister
	power   collector.PowerCollector
	temp    collector.TempCollector
	poster  poster
	report  reporter
	hw      *sample.SystemReport

	// certSync is the Phase 2 certificate installer/syncer; nil when the
	// caller has none to offer (every pre-Phase-2 test construction in this
	// module), in which case certificate distribution is a complete no-op:
	// no ticker, no wake handling, no fields set on the outgoing Sample.
	certSync certSyncer
	// certWake is the optional wake channel derived from the poster (see
	// certWaker); nil when the poster does not support it (the POST
	// transport, or a test poster). A nil channel in a select case simply
	// never becomes ready -- Run's hot loop needs no conditional branch for
	// this case being disabled.
	certWake <-chan struct{}
	// certSyncing serializes certificate syncs: the periodic ticker and the
	// wake channel can fire arbitrarily close together, and this
	// CompareAndSwap guarantees at most one syncCert goroutine runs at a
	// time, coalescing any signal that arrives while one is already in
	// flight into a no-op (the in-flight sync will simply be re-triggered by
	// the next tick/wake once it finishes).
	certSyncing atomic.Bool
	// certReportMu guards certReport, the last certinstall.Report produced by
	// either seedCertReport (a synchronous, network-free disk read at
	// startup) or syncCert (after a background sync completes).
	// collectOnce reads it under the lock to populate the outgoing Sample's
	// four cert_* fields; this is the ONLY channel between the background
	// sync goroutine and the telemetry loop, so a sync running for a long
	// time (a slow or stuck gateway) never blocks a single sample from being
	// built and pushed.
	certReportMu sync.Mutex
	certReport   certinstall.Report

	trustSync    trustSyncer
	trustWake    <-chan struct{}
	trustSyncing atomic.Bool

	// proxy drives the agent-side TLS reverse proxy when cert_mode=proxy; nil
	// (off/files, and every pre-existing test) disables it entirely. It is
	// exercised only from syncCert, which already runs single-flight on the
	// certificate-poll cadence, so no extra synchronization is needed here (the
	// proxy.Manager it wraps is itself concurrency-safe regardless).
	proxy certProxyDriver

	// runtimeDriver is the optional agent-managed model runtime hook set;
	// nil (main.go's default, and every pre-Task-18 test construction)
	// disables the feature entirely -- no ticker, no wake handling, no
	// "runtimes" field on the outgoing Sample, no LoadedModels override.
	// See the runtimeDriver interface doc above for the exact contract.
	runtimeDriver runtimeDriver
	// runtimeWake is the optional wake channel derived from the poster (see
	// runtimeWaker); nil when the poster does not support it (the POST
	// transport, or a test poster). A nil channel in a select case simply
	// never becomes ready, mirroring certWake/trustWake exactly.
	runtimeWake <-chan json.RawMessage
	// runtimeTransitions is the optional immediate-resample doorbell derived
	// from runtimeDriver (see runtimeTransitionsWaker); nil when there is no
	// driver, or the driver does not implement it.
	runtimeTransitions <-chan struct{}
	// runtimeSyncing serializes runtime-config syncs: the periodic ticker,
	// the wake channel, and a direct call to Sync can all fire arbitrarily
	// close together, and this CompareAndSwap guarantees at most one Sync
	// runs at a time -- the exact same single-flight discipline
	// certSyncing/trustSyncing already use, and the pattern the task brief
	// calls "the AGENT's trigger pattern, not the Driver's own".
	runtimeSyncing atomic.Bool
	// runtimeReportResender is the optional periodic file-mode-report
	// resend hook derived from runtimeDriver (see the interface doc above);
	// nil when there is no driver, or the driver does not implement it.
	// Invoked from the same reportTicker cadence sendSystemReport already
	// uses (fix round 1, I5) -- never on its own ticker.
	runtimeReportResender runtimeReportResender
}

// SetCertProxyDriver installs the optional cert_mode=proxy driver. Call it once
// after New and before Run; nil (the default) leaves proxy behavior disabled.
// Kept for callers (agent_test.go) that build an Agent via New and attach the
// driver afterward; NewFromDeps sets the equivalent Deps.ProxyDriver field at
// construction time instead, so main.go no longer needs this setter.
func (a *Agent) SetCertProxyDriver(d certProxyDriver) {
	a.proxy = d
}

// Deps names every optional capability an Agent can be given, for use with
// NewFromDeps. A nil field disables that capability exactly as the
// corresponding argument to New (or, for ProxyDriver, a call to
// SetCertProxyDriver) always has: no ticker, no wake handling, no cert_*/
// ProxyRoutes fields on the outgoing Sample. Host, GPUs, Scraper, Loaded,
// Power, and Temp are the base collectors; Poster is the telemetry sink;
// CertSync/TrustSync are the Phase 2 certificate/trust syncers; ProxyDriver
// is the cert_mode=proxy hook set; RuntimeDriver is the agent-managed model
// runtime hook set (nil = feature absent, the no-op invariant -- see the
// runtimeDriver interface doc above). See New's and SetCertProxyDriver's
// docs above for the exact per-field disabled behavior each one preserves.
type Deps struct {
	Host          collector.HostCollector
	GPUs          []collector.GPUCollector
	Scraper       collector.Scraper
	Loaded        collector.LoadedModelLister
	Power         collector.PowerCollector
	Temp          collector.TempCollector
	Poster        poster
	CertSync      certSyncer
	TrustSync     trustSyncer
	ProxyDriver   certProxyDriver
	RuntimeDriver runtimeDriver
}

// New builds an Agent from the resolved config and its collectors/poster. The
// loaded-model lister, power collector, and temp collector are optional; a nil
// (or unavailable) one simply reports nothing. certSync is the Phase 2
// certificate installer/syncer (typically *certinstall.Installer); nil
// disables certificate distribution entirely (no ticker, no wake, no cert_*
// Sample fields) rather than panicking, so every pre-existing test
// construction that has nothing to say about certificates stays valid by
// passing nil for this argument.
//
// Deprecated: prefer NewFromDeps, which names every capability (including
// the optional trust syncer below and the cert_mode=proxy driver otherwise
// attached via SetCertProxyDriver) as a Deps field instead of positional
// args plus a trailing variadic. New is kept as a thin wrapper purely to
// avoid rewriting this package's and client/ws_e2e_test.go's many existing
// call sites; it delegates to NewFromDeps unchanged.
//
// trustSync is variadic (0 or 1 values) rather than a plain trustSyncer
// parameter so a caller with no trust syncer can omit the argument entirely
// instead of passing a value through it: a caller holding a possibly-nil
// concrete *trust.Refresher must never convert that pointer to the
// trustSyncer interface when it is nil, because doing so wraps a typed nil
// pointer in a non-nil interface value (the classic typed-nil-interface
// trap) -- every "!= nil" guard in this file (see triggerTrustSync,
// newTrustTicker) would then see a falsely non-nil trustSync and start a
// ticker that panics the first time Refresh is called on the nil receiver.
// Passing zero variadic args keeps a.trustSync a genuine nil interface;
// NewFromDeps's d.TrustSync field carries forward this exact same
// contract -- the caller must only ever assign a proven-non-nil value to it,
// never convert a possibly-nil concrete pointer into the field.
func New(cfg config.Config, host collector.HostCollector, gpus []collector.GPUCollector, scraper collector.Scraper, loaded collector.LoadedModelLister, power collector.PowerCollector, temp collector.TempCollector, c poster, certSync certSyncer, trustSync ...trustSyncer) *Agent {
	d := Deps{Host: host, GPUs: gpus, Scraper: scraper, Loaded: loaded, Power: power, Temp: temp, Poster: c, CertSync: certSync}
	if len(trustSync) > 0 {
		d.TrustSync = trustSync[0]
	}
	return NewFromDeps(cfg, d)
}

// NewFromDeps builds an Agent from cfg and d, per Deps's field-by-field
// disabled-when-nil contract (see the Deps doc above). It supersedes New's
// positional-args-plus-variadic shape and SetCertProxyDriver's
// post-construction setter: every capability, including the cert_mode=proxy
// driver, is named and available at construction time, so main.go no longer
// needs to branch on whether to include a trust syncer, or call a separate
// setter for the proxy driver.
func NewFromDeps(cfg config.Config, d Deps) *Agent {
	a := &Agent{
		cfg:           cfg,
		host:          d.Host,
		gpus:          d.GPUs,
		scraper:       d.Scraper,
		loaded:        d.Loaded,
		power:         d.Power,
		temp:          d.Temp,
		poster:        d.Poster,
		certSync:      d.CertSync,
		trustSync:     d.TrustSync,
		proxy:         d.ProxyDriver,
		runtimeDriver: d.RuntimeDriver,
	}
	if r, ok := d.Poster.(reporter); ok {
		a.report = r
	}
	if w, ok := d.Poster.(certWaker); ok {
		a.certWake = w.CertUpdates()
	}
	if w, ok := d.Poster.(trustWaker); ok {
		a.trustWake = w.TrustUpdates()
	}
	if w, ok := d.Poster.(runtimeWaker); ok {
		a.runtimeWake = w.RuntimeUpdates()
	}
	if w, ok := d.RuntimeDriver.(runtimeTransitionsWaker); ok {
		a.runtimeTransitions = w.Transitions()
	}
	if r, ok := d.RuntimeDriver.(runtimeReportResender); ok {
		a.runtimeReportResender = r
	}
	return a
}

// Run collects the static hardware inventory once, sends it, then collects+pushes
// telemetry immediately and on every cfg.Interval tick. A slow ticker re-sends the
// cached hardware inventory (POST self-heal; WS re-sends on reconnect regardless).
// Returns nil on graceful shutdown; a single failed collect/push/report never stops
// the loop.
func (a *Agent) Run(ctx context.Context) error {
	hw := collectHardware(ctx, a.gpus)
	a.hw = &hw
	a.sendSystemReport(ctx)

	// Phase 2 certificate distribution: seed the report synchronously from
	// whatever is already on disk (a pure, network-free read -- see
	// seedCertReport -- so the very first sample below is correct even before
	// any sync has run), then kick off one sync in the background right after
	// the system report, exactly like the ticker/wake cases do.
	a.seedCertReport()
	a.triggerCertSync(ctx)
	a.triggerTrustSync(ctx)
	// Agent-managed model runtime (Task 18): kick off one reconciliation in
	// the background right after cert/trust, exactly like those do -- the
	// very first sample below should reflect whatever the runtime driver
	// can determine promptly, rather than waiting up to runtimePollInterval
	// for the first tick. A nil pushed payload here means "resync over
	// HTTP", the same convention client.WSSender.RuntimeUpdates uses for a
	// fresh connect.
	a.triggerRuntimeSync(ctx, nil)

	a.collectOnce(ctx)
	ticker := time.NewTicker(a.cfg.Interval)
	defer ticker.Stop()

	reportInterval := a.cfg.SystemReportInterval
	if reportInterval <= 0 {
		reportInterval = defaultSystemReportInterval
	}
	reportTicker := time.NewTicker(reportInterval)
	defer reportTicker.Stop()

	certTicker, certTickerC := a.newCertTicker()
	if certTicker != nil {
		defer certTicker.Stop()
	}
	trustTicker, trustTickerC := a.newTrustTicker()
	if trustTicker != nil {
		defer trustTicker.Stop()
	}
	runtimeTicker, runtimeTickerC := a.newRuntimeTicker()
	if runtimeTicker != nil {
		defer runtimeTicker.Stop()
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			a.collectOnce(ctx)
		case <-reportTicker.C:
			a.sendSystemReport(ctx)
			a.resendRuntimeReport(ctx)
		case <-certTickerC:
			a.triggerCertSync(ctx)
		case <-a.certWake:
			a.triggerCertSync(ctx)
		case <-trustTickerC:
			a.triggerTrustSync(ctx)
		case <-a.trustWake:
			a.triggerTrustSync(ctx)
		case <-runtimeTickerC:
			a.triggerRuntimeSync(ctx, nil)
		case data := <-a.runtimeWake:
			a.triggerRuntimeSync(ctx, data)
		case <-a.runtimeTransitions:
			// Immediate coalesced sample on a spec state transition (design
			// spec §7): a portal click should feel like network latency, not
			// the next 1s telemetry tick.
			a.collectOnce(ctx)
		}
	}
}

func (a *Agent) newTrustTicker() (*time.Ticker, <-chan time.Time) {
	if a.trustSync == nil {
		return nil, nil
	}
	t := time.NewTicker(trustRefreshInterval)
	return t, t.C
}

func (a *Agent) triggerTrustSync(ctx context.Context) {
	if a.trustSync == nil || !a.trustSyncing.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer a.trustSyncing.Store(false)
		if err := a.trustSync.Refresh(ctx); err != nil {
			slog.Warn("gateway CA refresh failed", "err", err)
		} else {
			slog.Debug("gateway CA trust refreshed")
		}
	}()
}

// newRuntimeTicker builds the periodic runtime-config poll ticker, or --
// when there is no runtime driver at all -- returns a nil ticker and a nil
// channel. A nil channel in Run's select never becomes ready, so the hot
// loop needs no conditional branch to keep this case disabled; the caller
// is responsible for Stop()ing a non-nil ticker. Unlike newCertTicker there
// is no separate "mode off" check here: a.runtimeDriver's mere presence
// already means feature negotiation decided runtime_manager is active for
// this agent at startup (main.go only ever sets Deps.RuntimeDriver inside
// that check) -- the only thing that can change AFTER startup is whether
// the GATEWAY still declares it, which runtimeDriver.Sync itself re-checks
// on every call.
func (a *Agent) newRuntimeTicker() (*time.Ticker, <-chan time.Time) {
	if a.runtimeDriver == nil {
		return nil, nil
	}
	t := time.NewTicker(runtimePollInterval)
	return t, t.C
}

// triggerRuntimeSync starts one runtime-config sync in its own goroutine
// unless one is already running -- copies triggerCertSync's single-flight
// CompareAndSwap coalescing discipline verbatim (see that function's doc
// for the full rationale): a burst of ticker+wake+transition signals
// arriving close together must never run two syncs concurrently, and must
// never block Run's select loop on however long a sync takes.
//
// A pushed payload lost to single-flight coalescing (data arrives while a
// sync is already in flight, so this call is a no-op) is safe: the next
// tick calls Sync again with a nil payload, and the driver's own
// source.Load()/GatewaySource re-fetches or reads the source's own
// already-cached latest document regardless -- nothing pushed is ever
// silently dropped for good.
func (a *Agent) triggerRuntimeSync(ctx context.Context, data json.RawMessage) {
	if a.runtimeDriver == nil || !a.runtimeSyncing.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer a.runtimeSyncing.Store(false)
		a.runtimeDriver.Sync(ctx, data)
	}()
}

// newCertTicker builds the periodic certificate-poll ticker, or -- when
// certificate distribution is disabled (certModeOff) or there is no syncer to
// drive (certSync == nil) -- returns a nil ticker and a nil channel. A nil
// channel in Run's select never becomes ready, so the hot loop needs no
// conditional branch to keep this case disabled; the caller is responsible for
// Stop()ing a non-nil ticker.
func (a *Agent) newCertTicker() (*time.Ticker, <-chan time.Time) {
	if a.certModeOff() || a.certSync == nil {
		return nil, nil
	}
	t := time.NewTicker(certPollInterval(a.cfg))
	return t, t.C
}

// triggerCertSync starts one certificate sync in its own goroutine unless one
// is already running. The atomic.Bool CompareAndSwap is what lets Run's
// select loop call this from BOTH the periodic ticker case and the wake case
// without ever running two syncs concurrently, and without ever blocking the
// select loop itself on however long a sync takes (a slow or stuck gateway
// must never stall this agent's ~1s telemetry cadence).
//
// The certModeOff check here (not just at newCertTicker, which only disables
// the TICKER) is load-bearing: the wake channel (a.certWake) is driven by the
// WebSocket connection's own reconnect/cert_update signal, which fires
// regardless of this agent's LOCAL cert_mode -- the gateway has no way to know
// it. Without this guard, an agent configured with cert_mode=off but the
// websocket transport would still call Sync on every reconnect, violating the
// absolute "the agent never asks" contract for cert_mode=off (see the Task 5b
// plan's Global Constraints).
func (a *Agent) triggerCertSync(ctx context.Context) {
	if a.certSync == nil || a.certModeOff() {
		return
	}
	if !a.certSyncing.CompareAndSwap(false, true) {
		return // a sync is already in flight; this signal is coalesced into it
	}
	go func() {
		defer a.certSyncing.Store(false)
		a.syncCert(ctx)
	}()
}

// syncCert runs exactly one certificate sync and records the resulting report
// for collectOnce to read. Always call it via triggerCertSync (never
// directly from Run's select), which is what keeps it off the hot loop.
//
// Sync returns a non-nil error ONLY for a genuine on-disk write failure
// during install; every other outcome (a transport error, a non-2xx status, a
// decode failure) is already swallowed inside Sync and returns a nil error --
// so logging the error here is the whole response needed. The next tick or
// wake retries regardless, and a failed install leaves whatever was
// previously installed untouched (Sync's own continuity guarantee), so there
// is nothing to roll back here.
func (a *Agent) syncCert(ctx context.Context) {
	rep, changed, err := a.certSync.Sync(ctx)
	switch {
	case err != nil:
		slog.Warn("certificate sync failed", "err", err)
	case changed:
		slog.Debug("certificate installed", "fingerprint", rep.Fingerprint, "not_after", rep.NotAfter)
	default:
		slog.Debug("certificate sync: no change")
	}
	// cert_mode=proxy: reuse this same poll cadence to drive the TLS proxy. A
	// real leaf install (changed) hot-swaps it into the running listeners and
	// brings up any route that was pending for want of a leaf; then refresh the
	// desired route set from the gateway on every tick regardless of the cert
	// outcome (routes are independent of an install, and stay pending inside the
	// Manager until a leaf exists). nil for off/files. ReloadCert precedes
	// SyncRoutes so a freshly installed leaf is loaded before routes bind.
	if a.proxy != nil {
		if changed {
			a.proxy.ReloadCert()
		}
		a.proxy.SyncRoutes(ctx)
	}
	a.certReportMu.Lock()
	a.certReport = rep
	a.certReportMu.Unlock()
}

// seedCertReport populates certReport synchronously from certSync.Report() --
// a pure, network-free disk read (even with cert_mode == off, which returns
// Report{Mode: "off"} immediately, never touching disk) -- so the very first
// sample already carries whatever is truthfully on disk (including a prior
// agent run's installed certificate, or the bare cert_mode when nothing is
// installed) without waiting for the first background sync to complete.
func (a *Agent) seedCertReport() {
	if a.certSync == nil {
		return
	}
	a.certReportMu.Lock()
	a.certReport = a.certSync.Report()
	a.certReportMu.Unlock()
}

// collectHardware gathers the static inventory once, bounded by hardwareCollectBudget.
func collectHardware(ctx context.Context, gpus []collector.GPUCollector) sample.SystemReport {
	cctx, cancel := context.WithTimeout(ctx, hardwareCollectBudget)
	defer cancel()
	return collector.CollectHardware(cctx, gpus, Version)
}

// sendSystemReport pushes the cached inventory (best-effort; a failure is logged).
func (a *Agent) sendSystemReport(ctx context.Context) {
	if a.report == nil || a.hw == nil {
		return
	}
	if err := a.report.PostSystemReport(ctx, a.hw); err != nil {
		slog.Warn("system report send failed", "err", err)
	} else {
		slog.Debug("system report sent")
	}
}

// resendRuntimeReport re-sends the runtime driver's current file-mode
// report unconditionally, piggybacking on the SAME ticker cadence as
// sendSystemReport (fix round 1, I5: design spec §10.2's POST-transport
// periodic backstop) rather than teaching this loop -- or the driver --
// which transport is in use. A nil runtimeReportResender (no driver, or a
// driver/fake that does not implement it) makes this a no-op.
func (a *Agent) resendRuntimeReport(ctx context.Context) {
	if a.runtimeReportResender == nil {
		return
	}
	a.runtimeReportResender.ResendReport(ctx)
}

// collectOnce builds one sample from the host, GPU, and scrape collectors and
// pushes it. Each collector failure is logged and skipped so a partial sample
// still ships; a push failure is logged but not returned (the loop keeps going).
// Nothing logged here includes the token.
func (a *Agent) collectOnce(ctx context.Context) {
	slog.Debug("collect cycle start")
	s := sample.Sample{
		ReportedAt:   time.Now().UTC(),
		AgentVersion: Version,
		OS:           runtime.GOOS,
		Arch:         runtime.GOARCH,
		// Feature negotiation (design spec §9): declare what this agent
		// binary can do so the gateway can gate agent-only behavior (e.g.
		// runtime_manager) on name equality instead of guessing from
		// AgentVersion.
		Capabilities: capabilitiesJSON(),
	}
	if a.host != nil {
		cctx, cancel := context.WithTimeout(ctx, collectTimeout)
		h, err := a.host.Collect(cctx)
		cancel()
		if err != nil {
			slog.Warn("host collect failed", "err", err)
		} else {
			s.Host = h
		}
	}
	if a.power != nil && s.Host != nil {
		// Power scalars ride on the host section. Best-effort: a collector error
		// never fails the sample (bounded by the same collectTimeout as the others).
		cctx, cancel := context.WithTimeout(ctx, collectTimeout)
		cpu, system, err := a.power.Collect(cctx)
		cancel()
		if err != nil {
			slog.Debug("power collect failed", "err", err)
		} else {
			s.Host.CPUPowerW = cpu
			s.Host.SystemPowerW = system
			slog.Debug("power collected", "cpu_w", wattsLog(cpu), "system_w", wattsLog(system))
		}
	}
	if a.temp != nil && s.Host != nil {
		// CPU temperature rides on the host section, same best-effort contract as
		// power: a collector error/unavailability never fails the sample.
		cctx, cancel := context.WithTimeout(ctx, collectTimeout)
		temp, err := a.temp.Collect(cctx)
		cancel()
		if err != nil {
			slog.Debug("temp collect failed", "err", err)
		} else {
			s.Host.CPUTempC = temp
			slog.Debug("temp collected", "cpu_temp_c", tempLog(temp))
		}
	}
	for _, g := range a.gpus {
		// A wedged GPU CLI (e.g. nvidia-smi during a driver/GPU fault) must not
		// hang the single-goroutine loop forever: bound each collect so
		// exec.CommandContext kills the child and the loop advances to the next
		// tick, still shipping host + the other collectors' data.
		cctx, cancel := context.WithTimeout(ctx, collectTimeout)
		gpus, err := g.Collect(cctx)
		cancel()
		if err != nil {
			slog.Warn("gpu collect failed", "collector", g.Name(), "err", err)
			continue
		}
		s.GPUs = append(s.GPUs, gpus...)
	}
	if a.scraper != nil {
		cctx, cancel := context.WithTimeout(ctx, collectTimeout)
		active, queue, err := a.scraper.Scrape(cctx)
		cancel()
		if err != nil {
			slog.Warn("metrics scrape failed", "err", err)
		} else {
			s.ActiveRequests = active
			s.QueueDepth = queue
		}
	}
	if a.loaded != nil && a.loaded.Available() {
		cctx, cancel := context.WithTimeout(ctx, collectTimeout)
		models, err := a.loaded.Collect(cctx)
		cancel()
		if err != nil {
			slog.Warn("model-status scrape failed", "err", err)
		} else {
			s.LoadedModels = models
		}
	}
	// Agent-managed model runtime (Task 18, design spec §7/§9): when a
	// runtime driver is installed AND currently negotiated active, its
	// Status() is AUTHORITATIVE for both Runtimes and LoadedModels -- this
	// deliberately overrides whatever the a.loaded model-status scraper
	// above just set, per the design's own "the gateway treats a fresh
	// agent report as authoritative" contract applied consistently to the
	// runtime feature's own source of truth.
	//
	// Fix round 1, I3: the gate is Active(), NOT merely "a.runtimeDriver !=
	// nil". main.go now constructs the driver UNCONDITIONALLY (no more
	// one-shot startup probe that used to decide this forever), so a
	// non-nil driver no longer implies the feature is doing anything right
	// now -- gating the LoadedModels override on existence alone would wipe
	// a non-runtime (or not-yet-negotiated, or currently-drained) agent's
	// REAL loaded-model list with an empty one. Active() reflects the
	// driver's own last Sync outcome, re-checked on every call, so this
	// self-heals exactly when negotiation does. A nil driver (still
	// possible: a test, or a hypothetical build that never wires one)
	// short-circuits before even calling Active(), which is what keeps
	// Runtimes completely absent -- nil, thus omitted by the sample's
	// omitempty tag, exactly like ProxyRoutes above -- for every agent
	// that never wires the feature at all: the no-op invariant this whole
	// feature is built around.
	if a.runtimeDriver != nil && a.runtimeDriver.Active() {
		statuses := a.runtimeDriver.Status()
		var loaded []string
		if len(statuses) > 0 {
			runtimes := make([]sample.RuntimeSample, 0, len(statuses))
			for _, st := range statuses {
				rs := sample.RuntimeSample{
					SpecID:   st.SpecID,
					Model:    st.Model,
					State:    string(st.State),
					Since:    st.Since,
					PID:      st.PID,
					Port:     st.Port,
					InFlight: st.InFlight,
					Restarts: st.Restarts,
				}
				if len(st.MeasuredVRAM) > 0 {
					gpus := make([]sample.RuntimeGPUSample, 0, len(st.MeasuredVRAM))
					for idx, mb := range st.MeasuredVRAM {
						gpus = append(gpus, sample.RuntimeGPUSample{Index: idx, VRAMMeasuredMB: mb})
					}
					// map iteration order is unspecified; sort by index so
					// the wire order is deterministic (easier to diff/read,
					// and avoids flaky-looking test assertions downstream).
					sort.Slice(gpus, func(i, j int) bool { return gpus[i].Index < gpus[j].Index })
					rs.GPUs = gpus
				}
				if st.LastError != nil {
					rs.LastError = &sample.RuntimeErrorSample{
						Message:    st.LastError.Message,
						At:         st.LastError.At,
						ExitCode:   st.LastError.ExitCode,
						Failures:   st.LastError.Failures,
						StderrTail: st.LastError.StderrTail,
					}
				}
				runtimes = append(runtimes, rs)
				// Routing must never see a cold/loading model as usable
				// (prefer-loaded must not route to a model that cannot
				// answer yet) -- only StateRunning counts, matching the
				// design spec's own §7 "flat loaded_models list carries
				// only truly loaded models" rule and Manager.LoadedModels'
				// identical StateRunning-only filter.
				if st.State == runtimectl.StateRunning {
					loaded = append(loaded, st.Model)
				}
			}
			s.Runtimes = runtimes
		}
		// Authoritative from the manager even when EMPTY (no specs, or
		// none running): overrides whatever a.loaded produced above, since
		// the runtime feature being active means IT owns this server's
		// loaded-model truth now, not the generic model-status scraper.
		s.LoadedModels = loaded
	}
	// Phase 2 certificate distribution: report the LAST report syncCert (or
	// the startup seedCertReport) produced, read under certReportMu. This
	// never blocks on a sync in flight -- the mutex only ever guards a plain
	// struct copy, never the sync call itself -- so a slow or stuck gateway
	// can never delay a single sample from being built and pushed. With no
	// certSync wired at all, certReport stays its zero value (every field
	// empty), which is exactly "nothing to report".
	a.certReportMu.Lock()
	certReport := a.certReport
	a.certReportMu.Unlock()
	s.CertFingerprint = certReport.Fingerprint
	s.CertNotAfter = certReport.NotAfter
	s.CertMode = certReport.Mode
	var trustFingerprints []string
	if a.trustSync != nil {
		trustFingerprints = a.trustSync.DurableFingerprints()
	}
	// Durable gateway trust goes first: the gateway caps reports at eight roots,
	// so a long legacy cert bundle must not displace the currently recoverable
	// transport roots that gate safe leaf rotation.
	s.CertCAFingerprints = sample.MergeUniqueStrings(trustFingerprints, certReport.CAFingerprints)
	// Certificates P4 Task 4: report this agent's TLS-proxy route state. a.proxy
	// is non-nil ONLY for cert_mode=proxy (main.go's SetCertProxyDriver call is
	// itself gated on that mode), so this check alone keeps ProxyRoutes
	// completely absent -- nil, thus omitted by the sample's omitempty tag --
	// for every off/files agent, byte-neutral with the pre-Task-4 wire shape.
	if a.proxy != nil {
		statuses := a.proxy.Status()
		if len(statuses) > 0 {
			routes := make([]sample.ProxyRouteSample, 0, len(statuses))
			for _, rs := range statuses {
				routes = append(routes, sample.ProxyRouteSample{Listen: rs.Listen, TLSActive: rs.TLSActive, State: string(rs.State)})
			}
			s.ProxyRoutes = routes
		}
	}
	// Debug summary of what was collected (no secrets); handy when debugging why a
	// server shows no/partial data in the portal.
	if slog.Default().Enabled(ctx, slog.LevelDebug) {
		cores := 0
		if s.Host != nil {
			cores = len(s.Host.CPUCores)
		}
		slog.Debug("collected sample",
			"has_host", s.Host != nil,
			"cpu_cores", cores,
			"gpus", len(s.GPUs),
			"active_requests", s.ActiveRequests,
			"queue_depth", s.QueueDepth,
			"loaded_models", len(s.LoadedModels),
			"cert_mode", s.CertMode,
			"cert_fingerprint_set", s.CertFingerprint != "")
	}
	if err := a.poster.Post(ctx, &s); err != nil {
		slog.Warn("telemetry push failed", "err", err)
	} else {
		slog.Debug("telemetry pushed")
	}
}
