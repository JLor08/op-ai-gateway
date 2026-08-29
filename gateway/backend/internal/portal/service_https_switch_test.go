// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"op-ai-gateway/internal/routing"
	"strconv"
	"testing"
	"time"
)

// stubProxyStatus is a ProxyRouteStatusReader test double: it returns a
// preloaded per-server route-status snapshot, letting a test drive the switch
// reconcile through forward / revert / missing-report transitions by swapping
// the snapshot between passes.
type stubProxyStatus struct {
	byServer map[string][]ProxyRouteStatus
}

func (s *stubProxyStatus) ProxyRouteStatuses(serverID string) []ProxyRouteStatus {
	return s.byServer[serverID]
}

func schemeOf(t *testing.T, svc *Service, ctx context.Context, appID string) string {
	t.Helper()
	app, err := svc.routes.ApplicationByID(ctx, appID)
	if err != nil {
		t.Fatalf("read app %s: %v", appID, err)
	}
	return app.Scheme
}

// TestIsProxySwitchCandidate pins the eligibility rule: a NOT-EXCLUDED, ENABLED
// app that is currently http, OR already proxy-switched (https with a non-zero
// ProxyListenPort), is a candidate. A disabled app is never a candidate
// regardless of scheme/port, and an https app with NO proxy port is refused by
// the physical guard (the proxy can only front a plaintext upstream).
func TestIsProxySwitchCandidate(t *testing.T) {
	cases := []struct {
		name string
		app  routing.Application
		want bool
	}{
		{"enabled http", routing.Application{Status: routing.ServerStatusActive, Scheme: "http"}, true},
		{"enabled http, empty status is active", routing.Application{Scheme: "http"}, true},
		{"enabled empty scheme (defaults http)", routing.Application{Status: routing.ServerStatusActive}, true},
		{"enabled proxy-switched https", routing.Application{Status: routing.ServerStatusActive, Scheme: "https", ProxyListenPort: 8600}, true},
		{"disabled http", routing.Application{Status: routing.ServerStatusDisabled, Scheme: "http"}, false},
		{"disabled proxy-switched https", routing.Application{Status: routing.ServerStatusDisabled, Scheme: "https", ProxyListenPort: 8600}, false},
		// The DEFENCE-IN-DEPTH arm, not a second representation of "excluded":
		// migration 70 backfilled every stored row of this shape into the flag,
		// so a row that still reads false here reached the store without passing
		// through the portal's normalization (a pre-70 binary's insert, whose
		// omitted column takes the DEFAULT 0, or a direct store write). It must
		// still be refused -- the agent's proxy publishes an
		// http://127.0.0.1:<Port> upstream, so making this a candidate would
		// point it at an https upstream as if it were plaintext.
		{"legacy own-tls https with the flag false (no proxy port)", routing.Application{Status: routing.ServerStatusActive, Scheme: "https", ProxyListenPort: 0}, false},
	}
	// The exclusion is tested FIRST and independently of scheme and status, so
	// it holds across EVERY combination -- including the one that makes the
	// feature exist at all: ENABLED, scheme http, which is a candidate
	// unconditionally without the flag and which no value of
	// (Scheme, ProxyListenPort) could ever exclude.
	for _, status := range []string{routing.ServerStatusActive, routing.ServerStatusDisabled, ""} {
		for _, scheme := range []string{"http", "https", "", "gopher"} {
			for _, port := range []int{0, 8600} {
				cases = append(cases, struct {
					name string
					app  routing.Application
					want bool
				}{
					name: "excluded status=" + status + " scheme=" + scheme + " port=" + strconv.Itoa(port),
					app: routing.Application{
						Status: status, Scheme: scheme, ProxyListenPort: port, ProxyExcluded: true,
					},
					want: false,
				})
			}
		}
	}
	for _, c := range cases {
		if got := isProxySwitchCandidate(c.app); got != c.want {
			t.Errorf("%s: isProxySwitchCandidate = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestHTTPSSwitchNeverTouchesAnExcludedApplication is the ADR-017 regression
// guard, and it must never be deleted: it pins that this feature reinstated no
// automatic move to plaintext, and that both recovery arms leave an excluded
// application entirely alone.
//
// The two arms fail in OPPOSITE directions, so both are asserted:
//
//   - OUT OF SCOPE: revertScopeExit is unconditional and consults no snapshot.
//     An excluded https application must not be flipped to http by it, on this
//     pass or any later one -- the operator, not the gateway, owns that scheme
//     now. An ORDINARY proxy-switched application on the SAME server still is,
//     so the guard cannot pass by the revert simply having stopped working.
//   - IN SCOPE: an excluded http application must stay http across a pass in
//     which the agent reports tls_active=true for every port, and must never be
//     named by HTTPSSwitchUnreachableApps.
func TestHTTPSSwitchNeverTouchesAnExcludedApplication(t *testing.T) {
	svc, ctx := certEnv(t)
	setHTTPSSwitchMode(t, svc, ctx, "auto")

	// OUT OF SCOPE via the per-server exclude override.
	mustCreateSwitchTestServer(t, svc, ctx, "srv-out", "exclude")
	excluded := mustCreateSwitchTestApp(t, svc, ctx, "app-excluded", "srv-out", 8080, 0)
	excluded.Scheme = "https"
	excluded.ProxyExcluded = true
	if err := svc.routes.UpdateApplication(ctx, excluded); err != nil {
		t.Fatalf("mark excluded: %v", err)
	}
	// The control: an ordinary proxy-switched app on the same out-of-scope
	// server, which the scope-exit revert MUST still move.
	ordinary := mustCreateSwitchTestApp(t, svc, ctx, "app-ordinary", "srv-out", 8081, 8601)
	ordinary.Scheme = "https"
	if err := svc.routes.UpdateApplication(ctx, ordinary); err != nil {
		t.Fatalf("mark ordinary proxied: %v", err)
	}

	// IN SCOPE, with an agent reporting TLS up on every port it could possibly
	// have: an excluded http app must not be forwarded.
	mustCreateSwitchTestServer(t, svc, ctx, "srv-in", "")
	inScope := mustCreateSwitchTestApp(t, svc, ctx, "app-in-excluded", "srv-in", 8090, 0)
	inScope.ProxyExcluded = true
	if err := svc.routes.UpdateApplication(ctx, inScope); err != nil {
		t.Fatalf("mark in-scope app excluded: %v", err)
	}
	// The row that makes the in-scope arm a REAL guard rather than a vacuous
	// one. An excluded application with ProxyListenPort 0 is skipped by the
	// reconcile's own "no port assigned yet" clause, so it would stay http even
	// if this feature did not exist -- the port is what AgentProxyRoutes
	// withholds, and that is covered by its own test. This row carries a
	// NON-ZERO port (reachable only by a direct store write, since the portal's
	// invariant forbids it) with the agent reporting tls_active=true for it, so
	// the ONLY thing standing between it and a forward flip to https is
	// isProxySwitchCandidate's ProxyExcluded clause.
	held := mustCreateSwitchTestApp(t, svc, ctx, "app-in-excluded-holding-port", "srv-in", 8091, 8602)
	held.ProxyExcluded = true
	if err := svc.routes.UpdateApplication(ctx, held); err != nil {
		t.Fatalf("mark port-holding app excluded: %v", err)
	}
	svc.proxyStatus = &stubProxyStatus{byServer: map[string][]ProxyRouteStatus{
		"srv-in": {
			{Listen: 8600, TLSActive: true},
			{Listen: 8601, TLSActive: true},
			{Listen: 8602, TLSActive: true},
		},
	}}

	// Several passes: a guard that only holds on the first one is not a guard.
	for pass := 0; pass < 3; pass++ {
		svc.ReconcileHTTPSSwitch(ctx)
	}

	if got := schemeOf(t, svc, ctx, "app-excluded"); got != "https" {
		t.Fatalf("out-of-scope excluded app: scheme = %q, want https (the gateway must never write a scheme on this path)", got)
	}
	if got := schemeOf(t, svc, ctx, "app-ordinary"); got != "http" {
		t.Fatalf("out-of-scope ORDINARY proxied app: scheme = %q, want http -- the scope-exit revert must still work", got)
	}
	if got := schemeOf(t, svc, ctx, "app-in-excluded"); got != "http" {
		t.Fatalf("in-scope excluded http app: scheme = %q, want http (never forwarded)", got)
	}
	if got := schemeOf(t, svc, ctx, "app-in-excluded-holding-port"); got != "http" {
		t.Fatalf("in-scope excluded app HOLDING a port whose listener reports tls_active=true: scheme = %q, want http -- the forward arm must be stopped by the exclusion alone", got)
	}
	after, err := svc.routes.ApplicationByID(ctx, "app-in-excluded")
	if err != nil {
		t.Fatalf("read app-in-excluded: %v", err)
	}
	if after.ProxyListenPort != 0 {
		t.Fatalf("in-scope excluded app got ProxyListenPort = %d, want 0", after.ProxyListenPort)
	}

	// And it is structurally invisible to the unreachable-apps view: the filter
	// starts with isProxySwitchCandidate. That is correct -- the gateway is not
	// managing this application -- but it is asserted so the silence is a
	// pinned property rather than an accident.
	for _, row := range svc.HTTPSSwitchUnreachableApps(ctx) {
		if row.AppID == "app-excluded" || row.AppID == "app-in-excluded" || row.AppID == "app-in-excluded-holding-port" {
			t.Fatalf("HTTPSSwitchUnreachableApps named an excluded application: %+v", row)
		}
	}
}

// TestHTTPSSwitchForwardAndDeclinedRevert drives the reconcile through every
// status-driven transition: forward on an explicit tls_active=true, NO revert
// on a missing report, and -- since the no-automatic-downgrade policy -- no
// revert on an explicit tls_active=false either. The application stays https
// and becomes unreachable until TLS is restored or an operator moves it
// deliberately; see TestHTTPSSwitchDeclinedRevertIsObservable for the signals
// that make that state visible.
func TestHTTPSSwitchForwardAndDeclinedRevert(t *testing.T) {
	svc, ctx := certEnv(t)
	setHTTPSSwitchMode(t, svc, ctx, "auto")
	mustCreateSwitchTestServer(t, svc, ctx, "srv-a", "")
	mustCreateSwitchTestApp(t, svc, ctx, "app-1", "srv-a", 8080, 8600) // http, port pre-assigned

	stub := &stubProxyStatus{byServer: map[string][]ProxyRouteStatus{}}
	svc.proxyStatus = stub

	// No report yet: an http candidate must not be touched.
	svc.ReconcileHTTPSSwitch(ctx)
	if got := schemeOf(t, svc, ctx, "app-1"); got != "http" {
		t.Fatalf("no report: scheme = %q, want http (unchanged)", got)
	}

	// FORWARD: the agent reports its proxy listener terminating TLS.
	stub.byServer["srv-a"] = []ProxyRouteStatus{{Listen: 8600, TLSActive: true}}
	svc.ReconcileHTTPSSwitch(ctx)
	if got := schemeOf(t, svc, ctx, "app-1"); got != "https" {
		t.Fatalf("forward: scheme = %q, want https", got)
	}

	// MISSING report (agent absent / empty snapshot): NEVER revert.
	stub.byServer["srv-a"] = nil
	svc.ReconcileHTTPSSwitch(ctx)
	if got := schemeOf(t, svc, ctx, "app-1"); got != "https" {
		t.Fatalf("missing report: scheme = %q, want https (no revert on absence)", got)
	}

	// A report that omits this listen is also "missing" for this route: no revert.
	stub.byServer["srv-a"] = []ProxyRouteStatus{{Listen: 9999, TLSActive: false}}
	svc.ReconcileHTTPSSwitch(ctx)
	if got := schemeOf(t, svc, ctx, "app-1"); got != "https" {
		t.Fatalf("other-listen report: scheme = %q, want https (no revert)", got)
	}

	// DECLINED REVERT: an EXPLICIT per-route tls_active=false used to flip the
	// application back to plaintext http. It must not any more -- answering a
	// broken certificate by turning off encryption is the security problem, not
	// the mitigation. The app stays https.
	stub.byServer["srv-a"] = []ProxyRouteStatus{{Listen: 8600, TLSActive: false, State: "pending_leaf"}}
	svc.ReconcileHTTPSSwitch(ctx)
	if got := schemeOf(t, svc, ctx, "app-1"); got != "https" {
		t.Fatalf("tls_active=false: scheme = %q, want https (never an automatic downgrade)", got)
	}
	// And it stays declined on every later pass -- no eventual give-up.
	svc.ReconcileHTTPSSwitch(ctx)
	if got := schemeOf(t, svc, ctx, "app-1"); got != "https" {
		t.Fatalf("tls_active=false (2nd pass): scheme = %q, want https", got)
	}

	// RECOVERY: once TLS is back the application simply works again -- it was
	// never moved, so there is nothing to switch forward.
	stub.byServer["srv-a"] = []ProxyRouteStatus{{Listen: 8600, TLSActive: true}}
	svc.ReconcileHTTPSSwitch(ctx)
	if got := schemeOf(t, svc, ctx, "app-1"); got != "https" {
		t.Fatalf("recovery: scheme = %q, want https", got)
	}
	if len(svc.HTTPSSwitchUnreachableApps(ctx)) != 0 {
		t.Fatalf("recovery: still reported unreachable: %+v", svc.HTTPSSwitchUnreachableApps(ctx))
	}
}

// TestHTTPSSwitchDeclinedRevertIsObservable is the other half of the policy:
// removing the automatic downgrade must not trade a SILENT DOWNGRADE for a
// SILENT OUTAGE. The declined revert has to be visible without an operator
// knowing to go looking, so this asserts the portal-facing signal (which the
// certificates view renders as an alert) and not merely the absence of a write.
func TestHTTPSSwitchDeclinedRevertIsObservable(t *testing.T) {
	svc, ctx := certEnv(t)
	setHTTPSSwitchMode(t, svc, ctx, "auto")
	mustCreateSwitchTestServer(t, svc, ctx, "srv-a", "")
	mustCreateProxySwitchedApp(t, svc, ctx, "app-1", "srv-a", 8080, 8600)
	svc.proxyStatus = &stubProxyStatus{byServer: map[string][]ProxyRouteStatus{
		"srv-a": {{Listen: 8600, TLSActive: false, State: "bind_failed"}},
	}}

	counter := &countingUpdateApplicationStore{Store: svc.routes}
	svc.routes = counter
	svc.ReconcileHTTPSSwitch(ctx)
	if counter.updates != 0 {
		t.Fatalf("declined revert did %d UpdateApplication writes, want 0", counter.updates)
	}

	got := svc.HTTPSSwitchUnreachableApps(ctx)
	if len(got) != 1 {
		t.Fatalf("HTTPSSwitchUnreachableApps = %+v, want exactly one entry", got)
	}
	if got[0].AppID != "app-1" || got[0].ServerID != "srv-a" || got[0].ProxyListenPort != 8600 {
		t.Fatalf("unreachable entry = %+v, want app-1 on srv-a:8600", got[0])
	}
	// The REASON is the agent's own reported RouteState, relayed rather than
	// re-invented gateway-side: an operator reading "bind_failed" knows to look
	// for whatever else holds that port.
	if got[0].RouteState != "bind_failed" {
		t.Fatalf("unreachable entry RouteState = %q, want %q (the agent's reported state)", got[0].RouteState, "bind_failed")
	}
	if got[0].ServerName == "" {
		t.Fatal("unreachable entry carries no server name; the alert has to name the server")
	}
}

// TestHTTPSSwitchUnreachableAppsExcludesEverythingElse pins the negative space
// around that signal. It must fire ONLY for the state it names -- a
// proxy-switched app whose agent explicitly reports its proxy listener not
// terminating TLS -- or the alert becomes noise an operator learns to ignore.
func TestHTTPSSwitchUnreachableAppsExcludesEverythingElse(t *testing.T) {
	svc, ctx := certEnv(t)
	setHTTPSSwitchMode(t, svc, ctx, "auto")
	mustCreateSwitchTestServer(t, svc, ctx, "srv-a", "")
	// healthy: TLS is up
	mustCreateProxySwitchedApp(t, svc, ctx, "app-ok", "srv-a", 8080, 8600)
	// own-TLS https (no proxy port): not a proxy switch at all
	mustCreateProxySwitchedApp(t, svc, ctx, "app-own", "srv-a", 8081, 0)
	// plain http: nothing to be unreachable about
	mustCreateSwitchTestApp(t, svc, ctx, "app-http", "srv-a", 8082, 8602)
	// proxy-switched but the agent reports NOTHING for its port (silent agent):
	// deliberately not a revert, and deliberately not an alert either.
	mustCreateProxySwitchedApp(t, svc, ctx, "app-silent", "srv-a", 8083, 8603)

	svc.proxyStatus = &stubProxyStatus{byServer: map[string][]ProxyRouteStatus{
		"srv-a": {
			{Listen: 8600, TLSActive: true},
			{Listen: 8602, TLSActive: false, State: "pending_leaf"},
		},
	}}
	svc.ReconcileHTTPSSwitch(ctx)
	if got := svc.HTTPSSwitchUnreachableApps(ctx); len(got) != 0 {
		t.Fatalf("HTTPSSwitchUnreachableApps = %+v, want none", got)
	}

	// A disabled app is not a candidate either: the operator turned it off.
	app, err := svc.routes.ApplicationByID(ctx, "app-ok")
	if err != nil {
		t.Fatalf("read app-ok: %v", err)
	}
	app.Status = routing.ServerStatusDisabled
	if err := svc.routes.UpdateApplication(ctx, app); err != nil {
		t.Fatalf("disable app-ok: %v", err)
	}
	svc.proxyStatus = &stubProxyStatus{byServer: map[string][]ProxyRouteStatus{
		"srv-a": {{Listen: 8600, TLSActive: false, State: "pending_leaf"}},
	}}
	if got := svc.HTTPSSwitchUnreachableApps(ctx); len(got) != 0 {
		t.Fatalf("disabled app reported unreachable: %+v", got)
	}
}

// TestHTTPSSwitchUnreachableAppsIgnoresOutOfScopeServer: an out-of-scope
// server's proxy-switched apps are reverted by the scope-exit arm (see
// TestHTTPSSwitchScopeExitStillRevertsAfterTheNoDowngradePolicy), so they are
// never left in the unreachable state and must never be alerted about. The
// alert names a condition the operator has to ACT on; a state the gateway
// resolves by itself on the same pass is not one.
func TestHTTPSSwitchUnreachableAppsIgnoresOutOfScopeServer(t *testing.T) {
	svc, ctx := certEnv(t)
	setHTTPSSwitchMode(t, svc, ctx, "auto")
	mustCreateSwitchTestServer(t, svc, ctx, "srv-x", "exclude")
	mustCreateProxySwitchedApp(t, svc, ctx, "app-x", "srv-x", 8080, 8600)
	svc.proxyStatus = &stubProxyStatus{byServer: map[string][]ProxyRouteStatus{
		"srv-x": {{Listen: 8600, TLSActive: false, State: "pending_leaf"}},
	}}

	if got := svc.HTTPSSwitchUnreachableApps(ctx); len(got) != 0 {
		t.Fatalf("out-of-scope server reported unreachable: %+v", got)
	}
}

// TestHTTPSSwitchScopeExitStillRevertsAfterTheNoDowngradePolicy is the pin on
// the ONE automatic move to plaintext that survives the policy change, so that
// it can never be scoped out by accident later.
//
// It is a different case from a tls_active=false report and the difference is
// the operator. There, nothing was asked for: a leaf expired, a port got taken,
// a file went missing, and answering that by turning off encryption is exactly
// the security problem. Here the operator performed an explicit portal action
// whose documented and only effect is "this server no longer runs the
// gateway-guided TLS proxy" -- the gateway ITSELF then withdrew the routes
// (AgentProxyRoutes returns [] for an out-of-scope server) and the agent tore
// its listeners down. Leaving the app on https would point it at a port that is
// genuinely gone, with NO path back: the status-driven pass does not run for an
// out-of-scope server, and there is no proxy_listen_port field in the portal
// UI, so the rescue would be API-only. That is a permanent outage, not a
// recoverable one.
func TestHTTPSSwitchScopeExitStillRevertsAfterTheNoDowngradePolicy(t *testing.T) {
	svc, ctx := certEnv(t)
	setHTTPSSwitchMode(t, svc, ctx, "auto")
	mustCreateSwitchTestServer(t, svc, ctx, "srv-a", "")
	mustCreateProxySwitchedApp(t, svc, ctx, "app-1", "srv-a", 8080, 8600)
	// Even with the agent explicitly reporting TLS DOWN -- the exact report that
	// no longer reverts an in-scope app -- the scope exit still reverts.
	svc.proxyStatus = &stubProxyStatus{byServer: map[string][]ProxyRouteStatus{
		"srv-a": {{Listen: 8600, TLSActive: false, State: "pending_leaf"}},
	}}

	setHTTPSSwitchMode(t, svc, ctx, "manual")
	svc.ReconcileHTTPSSwitch(ctx)
	if got := schemeOf(t, svc, ctx, "app-1"); got != "http" {
		t.Fatalf("scope exit: scheme = %q, want http (the one surviving automatic revert)", got)
	}
}

// TestHTTPSSwitchManualModeNeverSwitches confirms the byte-neutral default:
// manual mode leaves every scheme untouched even with a tls_active=true report.
func TestHTTPSSwitchManualModeNeverSwitches(t *testing.T) {
	svc, ctx := certEnv(t)
	// manual is the default; do not set any mode.
	mustCreateSwitchTestServer(t, svc, ctx, "srv-m", "")
	mustCreateSwitchTestApp(t, svc, ctx, "app-m", "srv-m", 8080, 8600)
	svc.proxyStatus = &stubProxyStatus{byServer: map[string][]ProxyRouteStatus{
		"srv-m": {{Listen: 8600, TLSActive: true}},
	}}

	svc.ReconcileHTTPSSwitch(ctx)
	if got := schemeOf(t, svc, ctx, "app-m"); got != "http" {
		t.Fatalf("manual mode: scheme = %q, want http (never switches)", got)
	}
}

// TestHTTPSSwitchOutOfScopeServerNeverSwitches confirms an auto-mode server that
// is explicitly excluded (opt-out) is never switched, even with a
// tls_active=true report.
func TestHTTPSSwitchOutOfScopeServerNeverSwitches(t *testing.T) {
	svc, ctx := certEnv(t)
	setHTTPSSwitchMode(t, svc, ctx, "auto")
	mustCreateSwitchTestServer(t, svc, ctx, "srv-x", "exclude")
	mustCreateSwitchTestApp(t, svc, ctx, "app-x", "srv-x", 8080, 8600)
	svc.proxyStatus = &stubProxyStatus{byServer: map[string][]ProxyRouteStatus{
		"srv-x": {{Listen: 8600, TLSActive: true}},
	}}

	svc.ReconcileHTTPSSwitch(ctx)
	if got := schemeOf(t, svc, ctx, "app-x"); got != "http" {
		t.Fatalf("excluded server: scheme = %q, want http (out of scope)", got)
	}
}

// countingUpdateApplicationStore counts UpdateApplication (scheme-write) calls
// so a test can assert the reconcile made ZERO scheme writes.
type countingUpdateApplicationStore struct {
	routing.Store
	updates int
}

func (s *countingUpdateApplicationStore) UpdateApplication(ctx context.Context, app routing.Application) error {
	s.updates++
	return s.Store.UpdateApplication(ctx, app)
}

// mustCreateProxySwitchedApp seeds an ALREADY-switched app -- Scheme "https"
// with a non-zero ProxyListenPort -- directly in the store, mirroring
// mustCreateSwitchTestApp but for the steady state the scope-exit revert acts
// on. proxyListenPort 0 seeds an own-TLS https app (never a switch target).
func mustCreateProxySwitchedApp(t *testing.T, svc *Service, ctx context.Context, id, serverID string, port, proxyListenPort int) {
	t.Helper()
	now := time.Now().UTC()
	if err := svc.routes.CreateApplication(ctx, routing.Application{
		ID: id, ServerID: serverID, Type: "openai_compatible", Port: port, Scheme: "https",
		Status: routing.ServerStatusActive, Priority: 1, Weight: 1, ProxyListenPort: proxyListenPort,
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create proxy-switched app %s: %v", id, err)
	}
}

// TestHTTPSSwitchScopeExitRevertsAutoToManual is the core scope-exit guard: an
// app auto-switched to proxied-https must be reverted to http when the fleet
// flips auto->manual, UNCONDITIONALLY -- even when the (now stale) proxy status
// snapshot still reports tls_active=true. Otherwise the app is stranded routing
// to a dead proxy port after the agent tears the listener down. Idempotent: a
// second tick must not flap it.
func TestHTTPSSwitchScopeExitRevertsAutoToManual(t *testing.T) {
	svc, ctx := certEnv(t)
	setHTTPSSwitchMode(t, svc, ctx, "auto")
	mustCreateSwitchTestServer(t, svc, ctx, "srv-a", "")
	mustCreateProxySwitchedApp(t, svc, ctx, "app-1", "srv-a", 8080, 8600)
	// A snapshot that STILL claims TLS is up must not stop the scope-exit revert:
	// once out of scope the gateway has already withdrawn the route.
	svc.proxyStatus = &stubProxyStatus{byServer: map[string][]ProxyRouteStatus{
		"srv-a": {{Listen: 8600, TLSActive: true}},
	}}

	setHTTPSSwitchMode(t, svc, ctx, "manual")
	svc.ReconcileHTTPSSwitch(ctx)
	if got := schemeOf(t, svc, ctx, "app-1"); got != "http" {
		t.Fatalf("auto->manual: scheme = %q, want http (unconditional scope-exit revert)", got)
	}
	// Idempotent: already http -> the predicate skips it, no flap.
	svc.ReconcileHTTPSSwitch(ctx)
	if got := schemeOf(t, svc, ctx, "app-1"); got != "http" {
		t.Fatalf("auto->manual (2nd tick): scheme = %q, want http (no flap)", got)
	}
}

// TestHTTPSSwitchScopeExitRevertsSelectedWithoutInclude: narrowing auto->selected
// takes a server that lacks HTTPSSwitchOverride="include" out of scope, so its
// proxy-switched app reverts to http.
func TestHTTPSSwitchScopeExitRevertsSelectedWithoutInclude(t *testing.T) {
	svc, ctx := certEnv(t)
	setHTTPSSwitchMode(t, svc, ctx, "auto")
	mustCreateSwitchTestServer(t, svc, ctx, "srv-s", "") // no include override
	mustCreateProxySwitchedApp(t, svc, ctx, "app-s", "srv-s", 8080, 8600)

	setHTTPSSwitchMode(t, svc, ctx, "selected")
	svc.ReconcileHTTPSSwitch(ctx)
	if got := schemeOf(t, svc, ctx, "app-s"); got != "http" {
		t.Fatalf("auto->selected (no include): scheme = %q, want http (scope-exit revert)", got)
	}
}

// TestHTTPSSwitchScopeExitRevertsExcludedServer: a per-server opt-out
// (HTTPSSwitchOverride="exclude") under auto takes just that server out of
// scope, reverting its proxy-switched app to http.
func TestHTTPSSwitchScopeExitRevertsExcludedServer(t *testing.T) {
	svc, ctx := certEnv(t)
	setHTTPSSwitchMode(t, svc, ctx, "auto")
	mustCreateSwitchTestServer(t, svc, ctx, "srv-x", "")
	mustCreateProxySwitchedApp(t, svc, ctx, "app-x", "srv-x", 8080, 8600)

	if err := svc.routes.UpdateServerHTTPSSwitchOverride(ctx, "srv-x", "exclude"); err != nil {
		t.Fatalf("exclude override: %v", err)
	}
	svc.ReconcileHTTPSSwitch(ctx)
	if got := schemeOf(t, svc, ctx, "app-x"); got != "http" {
		t.Fatalf("excluded server: scheme = %q, want http (scope-exit revert)", got)
	}
}

// TestHTTPSSwitchScopeExitLeavesOwnTLSAppAlone confirms the predicate: an
// own-TLS https app (ProxyListenPort==0) is NOT a proxy switch and must survive a
// scope exit untouched.
func TestHTTPSSwitchScopeExitLeavesOwnTLSAppAlone(t *testing.T) {
	svc, ctx := certEnv(t)
	setHTTPSSwitchMode(t, svc, ctx, "auto")
	mustCreateSwitchTestServer(t, svc, ctx, "srv-o", "")
	mustCreateProxySwitchedApp(t, svc, ctx, "app-o", "srv-o", 8080, 0) // https, no proxy port

	setHTTPSSwitchMode(t, svc, ctx, "manual")
	svc.ReconcileHTTPSSwitch(ctx)
	if got := schemeOf(t, svc, ctx, "app-o"); got != "https" {
		t.Fatalf("own-TLS https app: scheme = %q, want https (never reverted)", got)
	}
}

// TestHTTPSSwitchManualNoProxySwitchedAppsWritesNothing pins the byte-neutral
// cost: manual mode with only plain-http apps enumerates the fleet (so it CAN
// revert a leftover switch) but makes ZERO scheme writes when there is nothing
// to revert.
func TestHTTPSSwitchManualNoProxySwitchedAppsWritesNothing(t *testing.T) {
	svc, ctx := certEnv(t)
	mustCreateSwitchTestServer(t, svc, ctx, "srv-n", "")
	mustCreateSwitchTestApp(t, svc, ctx, "app-n", "srv-n", 8080, 0) // plain http

	counter := &countingUpdateApplicationStore{Store: svc.routes}
	svc.routes = counter
	svc.ReconcileHTTPSSwitch(ctx)
	if counter.updates != 0 {
		t.Fatalf("manual mode with no proxy-switched apps did %d UpdateApplication writes, want 0", counter.updates)
	}
}

// TestMigration70BackfillIsBehaviourPreserving proves — mechanically, over
// every pre-70 stored shape rather than by example — that migration 70 changed
// no application's participation.
//
// It reimplements the PRE-70 predicate verbatim (that is the point: the real
// one has moved on, so the old answer has to come from somewhere) and the
// backfill's own SQL predicate, then asserts for every (scheme x
// proxy_listen_port x status) row that
//
//	old(row) == new(backfill(row))
//
// The interesting rows are the ones the backfill actually flips: `https` with
// no proxy port. They failed the old predicate's https arm and fail the new
// flag clause — the same skip, reached from a different clause, which is what
// makes this a rename of an existing state rather than a behaviour change.
func TestMigration70BackfillIsBehaviourPreserving(t *testing.T) {
	// The candidate predicate exactly as it stood before this change.
	preFlagCandidate := func(app routing.Application) bool {
		if app.Status == routing.ServerStatusDisabled {
			return false
		}
		switch effectiveScheme(app) {
		case "http":
			return true
		case "https":
			return app.ProxyListenPort != 0
		default:
			return false
		}
	}
	// migration70Up's UPDATE predicate:
	//   set proxy_excluded = 1 where scheme = 'https' and proxy_listen_port = 0
	// Note it tests the RAW scheme column, not the http-defaulted one.
	backfill := func(app routing.Application) routing.Application {
		if app.Scheme == "https" && app.ProxyListenPort == 0 {
			app.ProxyExcluded = true
		}
		return app
	}

	flipped := 0
	for _, scheme := range []string{"http", "https", ""} {
		for _, port := range []int{0, 8600} {
			for _, status := range []string{routing.ServerStatusActive, routing.ServerStatusDisabled, ""} {
				before := routing.Application{Scheme: scheme, ProxyListenPort: port, Status: status}
				after := backfill(before)
				if after.ProxyExcluded {
					flipped++
				}
				if got, want := isProxySwitchCandidate(after), preFlagCandidate(before); got != want {
					t.Errorf("scheme=%q port=%d status=%q: candidate after the backfill = %v, before = %v",
						scheme, port, status, got, want)
				}
			}
		}
	}
	// Guard against a vacuous pass: if the backfill flipped nothing, the loop
	// above would be comparing the new predicate against the old one on rows
	// where the flag is uniformly false, which proves nothing about the flip.
	if flipped != 3 {
		t.Fatalf("the backfill flipped %d of the seeded rows, want 3 (https + port 0, one per status)", flipped)
	}
}
