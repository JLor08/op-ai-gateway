// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package main

import (
	"context"
	"time"
)

// certReconcileMinInterval is the floor used when a caller passes a non-positive
// interval (unreachable in production -- config floors it at 60s, see
// config.Config.CertReconcileIntervalSeconds -- but a test fake could pass one,
// and time.NewTicker panics on a duration <= 0).
const certReconcileMinInterval = time.Minute

// certPassTimeout is the PRIMARY bound on a single reconcile pass: every call
// to ReconcileCertificates is wrapped in its own context.WithTimeout derived
// from this (see certPassDeadline). ReconcileCertificates may place up to
// certOrdersPerPass (5, see internal/portal/service_certificates.go) serial
// ACME orders in one pass, each involving several round trips to the
// configured directory -- it needs real room. But without ANY deadline here,
// a stalled ACME server, or an authorization that never leaves "pending",
// makes the underlying x/crypto/acme poll loop retry forever: the pass never
// returns, no certificate is ever issued or renewed again for the lifetime of
// the process, and -- because ReconcileCertificates holds portal.Service's
// certMu for its whole duration -- the CA-rotate HTTP handler
// (POST /api/system/certificates/ca/rotate, which also takes certMu) blocks
// forever too, with nothing in the logs pointing at the cause. This mirrors
// the benchmark-runner idle-watchdog fix for the identical failure shape (a
// background-context call to http.DefaultClient has no timeout of its own).
// 10 minutes is generous for 5 serial orders yet far below any sane cadence.
const certPassTimeout = 10 * time.Minute

// certReconciler is the one operation this loop drives. portal.Service
// implements it (ReconcileCertificates is a no-op while the ACME/CA module is
// disabled, so ticking costs nothing then).
type certReconciler interface {
	ReconcileCertificates(ctx context.Context)
}

// httpsSwitchReconciler is the OPTIONAL companion pass this loop also drives when
// its certReconciler implements it (portal.Service does): the P4 https-auto-switch
// reconcile. It rides the SAME cadence as the certificate pass -- both are
// tick-driven cert/TLS reconciles that should also react to a cert-settings save
// (cert_https_switch_mode is a cert setting) -- but is invoked separately with
// its own short deadline so a slow ACME pass never starves it, and it never runs
// under the cert pass's certMu. A test fake that only implements certReconciler
// (see fakeCertReconciler) is simply skipped by the type assertion.
type httpsSwitchReconciler interface {
	ReconcileHTTPSSwitch(ctx context.Context)
}

// httpsSwitchPassTimeout bounds one https-auto-switch pass. It is a cheap
// store-only reconcile (list servers + apps, read the in-memory proxy-status
// registry, flip a scheme or two), so this is generous; it exists only so a
// pathological store stall cannot wedge the loop goroutine.
const httpsSwitchPassTimeout = time.Minute

// certPassMinDeadline is the FLOOR on the per-pass deadline, independent of
// how short the configured interval is (down to its own config-level floor of
// 60s -- see config.Config.CertReconcileIntervalSeconds). Without this floor,
// a pass capped at the raw interval could be cancelled AFTER an ACME order
// already succeeded (the CA issued a certificate) but BEFORE issueAndStore
// persists it: the next pass then sees the domain still "wanted" and places a
// duplicate order for the SAME name, burning Let's Encrypt's
// 5-duplicate-certificates-per-week rate limit for no reason. Two minutes
// comfortably covers a single order's round trips (well under certPassTimeout's
// 5-order budget) while staying far below any sane reconcile cadence.
//
// A package var (not a const) purely so a test that needs a SHORT loop
// interval to stay fast (e.g. a stalled-ACME-pass regression driving the real
// production loop end-to-end) can lower it and restore it via t.Cleanup --
// mirrors internal/gateway/benchmark_runner.go's coldLoadMaxWait/
// coldLoadPollGap pattern. Production code never mutates it.
var certPassMinDeadline = 2 * time.Minute

// certPassDeadline is the actual per-pass bound handed to context.WithTimeout:
// certPassTimeout, capped at interval so a pass can never run past (or into)
// the loop's own next tick -- two overlapping passes would contend on
// portal.Service's certMu for no benefit, since the second pass would just
// re-derive the same desired set -- but never allowed below certPassMinDeadline
// (see its doc for why a too-short deadline is unsafe, not just slow).
// interval is expected to already be > 0 (runCertReconcileLoop floors it via
// certReconcileMinInterval before this is ever called), but the <= 0 branch is
// kept as a second, independent guard: context.WithTimeout with a zero or
// negative duration expires the context IMMEDIATELY, which would make every
// pass fail instantly -- so any non-positive interval here falls back to
// certPassTimeout rather than being used verbatim. The floor is applied AFTER
// that fallback, so it also protects the "never non-positive" property.
func certPassDeadline(interval time.Duration) time.Duration {
	deadline := certPassTimeout
	if interval > 0 && interval < certPassTimeout {
		deadline = interval
	}
	if deadline < certPassMinDeadline {
		deadline = certPassMinDeadline
	}
	return deadline
}

// runCertPass runs exactly one bounded reconcile pass: r.ReconcileCertificates
// is given a fresh context.WithTimeout(ctx, certPassDeadline(interval)),
// cancelled as soon as the pass returns (whether it finished normally or hit
// the deadline). This is the ONLY change from a bare r.ReconcileCertificates(ctx)
// call -- no retry logic, no change to what a pass decides to do.
func runCertPass(ctx context.Context, r certReconciler, interval time.Duration) {
	passCtx, cancel := context.WithTimeout(ctx, certPassDeadline(interval))
	r.ReconcileCertificates(passCtx)
	cancel()
	// The https-auto-switch pass rides the same cadence but runs AFTER (and
	// outside) the certificate pass with its own fresh, short deadline: a stalled
	// ACME pass that burned the whole certPassDeadline must never leave the switch
	// reconcile running on an already-cancelled context. It is a no-op unless the
	// reconciler also implements httpsSwitchReconciler and the mode is auto/selected.
	if sw, ok := r.(httpsSwitchReconciler); ok {
		switchCtx, cancelSwitch := context.WithTimeout(ctx, httpsSwitchPassTimeout)
		sw.ReconcileHTTPSSwitch(switchCtx)
		cancelSwitch()
	}
}

// runCertReconcileLoop runs one certificate reconcile pass immediately, then
// again on every tick of interval, until ctx is cancelled. Mirrors
// runNetbirdReconcileLoop's immediate-then-tick shape (see netbird_sync.go).
//
// A send on trigger runs an EXTRA pass immediately, independent of the ticker —
// used after a certificate SETTINGS change (portal.ServiceDeps.
// OnCertSettingsChanged), because the operator-facing cert_last_error note is
// written and cleared ONLY by a pass, so a corrective save would otherwise leave
// a stale note standing for up to one whole interval (default 900s). trigger may
// be nil (never fires). This mirrors runNetbirdSyncLoop's dns_domain trigger
// exactly.
//
// The trigger deliberately feeds THIS loop rather than spawning its own pass, and
// that is what keeps the concurrency honest: this goroutine is the only thing
// that ever calls runCertPass, so two passes can never overlap and no queue of
// goroutines can pile up on portal.Service's certMu (which a pass holds for its
// whole duration, up to certPassTimeout). The channel is buffered(1) with a
// non-blocking send at the hook site, so a burst of saves coalesces into at most
// one pending extra pass.
func runCertReconcileLoop(ctx context.Context, r certReconciler, interval time.Duration, trigger <-chan struct{}) {
	if interval <= 0 {
		interval = certReconcileMinInterval
	}
	runLoop(ctx, loopOpts{
		Immediate: true,
		Interval:  func() time.Duration { return interval }, // fixed: this loop never re-reads/Resets
		Trigger:   trigger,
		Pass:      func(ctx context.Context) { runCertPass(ctx, r, interval) },
	})
}

// startCertReconcileLoop launches runCertReconcileLoop in a goroutine and
// returns its cancel func. A package var (not a plain function) so a test can
// substitute a fake driving loop and observe start/stop without a real
// portal.Service -- mirrors startNetbirdReconcileLoop. trigger, when non-nil,
// lets a caller force an immediate extra pass (e.g. after a certificate settings
// change); pass nil when no such trigger is needed.
var startCertReconcileLoop = func(r certReconciler, interval time.Duration, trigger <-chan struct{}) context.CancelFunc {
	ctx, cancel := context.WithCancel(context.Background())
	go runCertReconcileLoop(ctx, r, interval, trigger)
	return cancel
}

// certReconcileTriggerFunc builds the portal.ServiceDeps.OnCertSettingsChanged
// hook for a loop fed by trigger: a NON-BLOCKING send, so the settings PUT that
// calls it inline is never delayed — not even while a pass is in flight holding
// certMu and placing ACME orders. A full buffer means an extra pass is already
// pending, which covers this change too, so dropping is correct (and there is
// always the ticker as the ultimate backstop).
func certReconcileTriggerFunc(trigger chan<- struct{}) func() {
	return func() {
		select {
		case trigger <- struct{}{}:
		default: // coalesce: a pending trigger already covers this change
		}
	}
}
