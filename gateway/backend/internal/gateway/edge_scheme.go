// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"crypto/subtle"
	"errors"
	"net"
	"net/http"
	"op-ai-gateway/internal/apierror"
	"strings"
	"sync"
	"time"
)

// edgeSchemeHeaderName is the hop header nginx's :443 AND :80 server blocks set
// from $scheme in every one of their header-setting blocks, overwriting any
// client-supplied value (see the gateway-edge-tls plan A nginx configs). Nothing
// reads it yet in this task -- it is defined here, in the same file as the
// tracker it will feed, so the string lives in exactly one place for the gate
// (a later task) to consume.
const edgeSchemeHeaderName = "X-OP-Edge-Scheme"

// edgeSchemeSelfProbeHeaderName marks a request as the gateway's OWN synthetic
// TLS self-probe (a later task: the gateway calls its own :443 listener over TLS
// to confirm the proxy in front of it terminates encryption at all). That probe
// necessarily satisfies every other "real external traffic" predicate below --
// it isn't /healthz, isn't the agent path, and isn't a loopback RemoteAddr in a
// deployment where the fronting proxy also runs on 127.0.0.1 -- so without an
// explicit exclusion it would let the arming precondition pass on the strength
// of the gateway convincing itself, proving nothing about the real proxy. The
// header is reserved now (unread) so that later task can set it without
// touching this file's exclusion list again.
//
// NOTHING PRODUCES THIS HEADER TODAY, deliberately: the self-probe that shipped
// performs a TLS HANDSHAKE ONLY and sends no HTTP request at all, so it can never
// be observed and needs no exclusion. The marker (and nginx's blanking of it)
// still earns its keep in both directions: it keeps the exclusion ready for a
// future prober that DOES send a request, and the blanking stops a client
// suppressing its own observations by guessing the name.
const edgeSchemeSelfProbeHeaderName = "X-OP-Edge-Self-Probe"

// edgeSchemeObservationWindow is how long a single observation stays "recent".
// It governs BOTH directions of the same fact and that symmetry is deliberate:
// the arming precondition needs an encrypted request inside this window before
// the operator may switch the gate on, and the armed gate keeps refusing only
// while one is still inside it. So if the fronting proxy's TLS listener dies,
// the window lapses and the gateway goes back to answering plaintext instead of
// refusing everything -- the difference between a degraded gateway and a
// locked-out operator. Five minutes is long enough that a quiet gateway does not
// flap and short enough that a broken TLS path un-arms itself well before an
// operator finishes diagnosing it.
const edgeSchemeObservationWindow = 5 * time.Minute

// edgeSchemeSwitchTTL bounds how long the gate reuses a cached read of
// cert_edge_require_https. The switch lives in system_settings, whose store read
// is an uncached full-table SELECT; the gate runs on EVERY public request
// (inference included), so reading it per request would add a database
// round-trip to the hot path. handleSystemSettings invalidates the cache
// explicitly after a successful PUT that carried the switch, so an operator
// toggling it in the portal sees the effect immediately; the TTL only bounds how
// stale an OUT-OF-BAND change (a direct database edit) can be.
const edgeSchemeSwitchTTL = 5 * time.Second

// edgeGateWarnInterval throttles the per-refusal log line to at most one per PATH
// per interval. Without it a single retrying inference client emits one Warn per
// attempt and evicts the 2000-entry log ring that the locked-out operator is
// supposed to read -- destroying the very record the "apply the verdict at the
// dispatch point so a refusal is logged" design exists to preserve. 60s is chosen
// against that ring: even a dozen distinct refused paths then cost ~12 lines a
// minute, which cannot outrun 2000 entries before an operator looks, while still
// showing that refusals are ONGOING rather than a single past event.
const edgeGateWarnInterval = time.Minute

// errEdgeHTTPSNotObserved is the arming precondition's refusal: no encrypted
// request has been seen inside edgeSchemeObservationWindow, so switching the gate
// on would arm a lockout on no evidence at all. Surfaced by the settings PUT as
// 400 certificate.edge_https_not_observed.
var errEdgeHTTPSNotObserved = errors.New("no encrypted request observed within the window")

// errEdgeArmHopPlaintext is the arming precondition's SECOND refusal, and a
// DIFFERENT condition from errEdgeHTTPSNotObserved: encrypted traffic HAS been
// observed recently, but the arming request itself arrived over a plaintext hop.
// Conflating the two would be actively misleading -- the operator would read "no
// encrypted request observed" while the panel right next to the switch shows a
// fresh last-encrypted timestamp. Surfaced by the settings PUT as 400
// certificate.edge_arm_requires_https.
var errEdgeArmHopPlaintext = errors.New("the arming request itself arrived unencrypted")

// edgeSchemeTracker records only the two timestamps needed to answer "have we
// actually seen encrypted traffic recently, and have we also seen plaintext
// traffic recently" -- no counters, no per-path breakdown, nothing that grows
// with traffic. It is written on every qualifying request and read by an HTTP
// status handler, so the critical section here must stay tiny: never hold the
// lock across I/O.
type edgeSchemeTracker struct {
	mu            sync.Mutex
	lastEncrypted time.Time
	lastPlain     time.Time
}

// Note records one observation at the given scheme (true = encrypted) and time.
// Callers pass the qualifying request's arrival time, not time.Now(), so this
// stays trivially testable and never touches the clock itself. A nil receiver
// is a no-op, mirroring the other in-process registries on *Server (e.g.
// BenchmarkRegistry.Release) so a bare &Server{} test fixture stays safe.
func (t *edgeSchemeTracker) Note(encrypted bool, now time.Time) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if encrypted {
		t.lastEncrypted = now
	} else {
		t.lastPlain = now
	}
}

// Seen reports whether an encrypted observation fell within window of now
// (encrypted), plus the raw last-encrypted/last-plain timestamps (zero time.Time
// when never observed) so a caller can also report plaintext recency without a
// second lock round-trip. A nil receiver returns the zero value (never
// encrypted, both timestamps zero) -- the fail-safe direction for "have we
// proven anything about the fronting proxy".
func (t *edgeSchemeTracker) Seen(now time.Time, window time.Duration) (encrypted bool, lastEncrypted, lastPlain time.Time) {
	if t == nil {
		return false, time.Time{}, time.Time{}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	lastEncrypted = t.lastEncrypted
	lastPlain = t.lastPlain
	encrypted = !lastEncrypted.IsZero() && now.Sub(lastEncrypted) <= window
	return encrypted, lastEncrypted, lastPlain
}

// countsAsObservation reports whether a request at path from remote is eligible
// to prove (or disprove) that the fronting reverse proxy speaks TLS to this
// gateway. The arming precondition this feeds may only be satisfied by traffic
// that genuinely round-tripped through that proxy, so every exclusion below is
// deliberate:
//
//   - /healthz is polled every few seconds by the frontend connection gate and by
//     container/k8s liveness probes -- usually loopback or an orchestrator
//     sidecar, never evidence about the public-facing proxy.
//   - /api/agent/v1/ is also served on the public mux (the no-netbird topology's
//     only path for agents) but carries no X-OP-Edge-Scheme expectation of its
//     own and would otherwise dilute the signal with agent traffic that may not
//     even route through the same proxy.
//   - A loopback remote is the gateway calling itself (background chat runs POST
//     to their own http://127.0.0.1:<port>/v1/chat/completions), which bypasses
//     nginx entirely -- it can never carry a proxy-set header honestly.
//   - selfProbe is the gateway's OWN synthetic TLS self-probe
//     (edgeSchemeSelfProbeHeaderName above), which satisfies every predicate
//     above -- it is not /healthz, not the agent path, and not loopback in a
//     deployment where the fronting proxy shares the host -- so without this the
//     precondition would pass on the strength of the gateway convincing itself,
//     proving nothing about the real proxy.
//
// Fail-safe direction: an unparseable remote address counts as NOT observable
// (under-counting only delays arming; over-counting would let an operator arm a
// lockout on evidence that never actually came from the fronting proxy). NOTE
// that the plaintext GATE resolves the same ambiguity the OTHER way (an
// unparseable remote is treated as local and therefore exempt) -- each fails in
// the safe direction for its own purpose, and the asymmetry is intentional: here
// the risk is arming on bad evidence, there the risk is refusing service.
//
// selfProbe is a parameter rather than a header read inside this function so the
// whole precondition stays in ONE auditable predicate: a future second call site
// cannot compile without deciding what to pass, whereas a caller-side header
// check could silently be forgotten. (Task 1 reserved the header and left the
// check to the caller; centralising it here is a deliberate deviation.)
func countsAsObservation(path string, remote string, selfProbe bool) bool {
	if path == "/healthz" {
		return false
	}
	if strings.HasPrefix(path, "/api/agent/v1/") {
		return false
	}
	if selfProbe {
		return false
	}
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		// http.Request.RemoteAddr is always "host:port" in practice; anything
		// that doesn't split is malformed input we can't trust. Fail safe: not
		// observable, rather than guessing at a bare-host fallback.
		return false
	}
	ip := net.ParseIP(host)
	if ip == nil {
		// Could not parse an address at all: fail safe, not observable.
		return false
	}
	if ip.IsLoopback() {
		return false
	}
	return true
}

// hopEncrypted reports whether the LAST hop into this gateway was encrypted,
// based solely on X-OP-Edge-Scheme. Absent or anything but "https" (compared
// case-insensitively, and exactly -- a trailing space is not proof) counts as
// plaintext: "no header, no proof".
//
// This header is trustworthy for exactly one reason: nginx SETS it from $scheme
// in every one of its nine header-setting blocks, so a client-supplied value is
// always overwritten (plan A proved that in a real container; the nginx config
// test pins it). X-Forwarded-Proto has NO such guarantee -- a client can send it
// and nginx passes it through -- so this gate must never read it, and does not.
func hopEncrypted(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get(edgeSchemeHeaderName), "https")
}

// isEdgeSelfProbe reports whether this request is the gateway's own synthetic TLS
// self-probe. Used ONLY to suppress the observation (see countsAsObservation) --
// never as a gate exemption, so a client that guesses the header name can at
// worst delay arming (a denial of arming, not a bypass).
func isEdgeSelfProbe(r *http.Request) bool {
	return r.Header.Get(edgeSchemeSelfProbeHeaderName) != ""
}

// edgeGateOpenPath reports whether path is one of the three path families that
// stay reachable over plaintext no matter what. Each closure would break
// something already merged; see edgeGateVerdict.
func edgeGateOpenPath(path string) bool {
	return path == "/healthz" ||
		strings.HasPrefix(path, "/.well-known/acme-challenge/") ||
		strings.HasPrefix(path, "/api/agent/v1/")
}

// edgeGateInternalCaller reports whether this request came from the gateway
// itself rather than through the fronting proxy -- the fourth open path.
//
// Two independent signals, either sufficient:
//
//   - The internal trusted-loopback header, compared in constant time against the
//     per-process secret exactly as authenticateWeb (auth.go) does, but WITHOUT
//     writing a response: this is a gate exemption, not an authentication
//     decision, and it must not disturb the request. An empty configured secret
//     disables the branch entirely (fail-closed, same as authenticateWeb), so a
//     client sending an empty header value can never match.
//   - A loopback RemoteAddr. Background chat runs POST to the gateway's own
//     http://127.0.0.1:<port>/v1/chat/completions, bypassing nginx entirely, so
//     they carry NO hop header; without this exemption an armed gate would 403
//     every chat run. The header alone is not enough to rely on here, because the
//     loopback caller is the gateway's own HTTP client and a future in-process
//     caller may not set it.
//
// Fail-open on an unparseable RemoteAddr: net/http always produces "host:port"
// for a real network connection, so the realistic producers of anything else are
// in-process/synthetic requests and unix-socket listeners (RemoteAddr "@") --
// exactly the class this exemption exists for. Refusing them would gain nothing
// and risks refusing service, which is this gate's one unforgivable failure.
func (s *Server) edgeGateInternalCaller(r *http.Request) bool {
	if s.internalAuthSecret != "" {
		if presented := r.Header.Get(internalAuthHeaderName); presented != "" &&
			subtle.ConstantTimeCompare([]byte(presented), []byte(s.internalAuthSecret)) == 1 {
			return true
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return true
	}
	return ip.IsLoopback()
}

// edgeRequireHTTPSOn reports the cert_edge_require_https switch through a short
// TTL cache (edgeSchemeSwitchTTL, via settingCache -- ttlcache.go). Nil-safe ->
// false, and the underlying CertEdgeRequireHTTPSChecked already reports false on
// any store error, so every failure mode leaves the gate DISENGAGED.
//
// settingCache.Get reads outside its lock and discards a read raced by an
// Invalidate() (see invalidateEdgeRequireHTTPSCache): without that guard, a read
// that STARTED before a disarming PUT could finish AFTER it and write its
// pre-disarm `true` back with a fresh full TTL -- i.e. up to edgeSchemeSwitchTTL
// of continued lockout after a SUCCESSFUL disarm, which is exactly the window an
// operator is watching to confirm the disarm worked.
func (s *Server) edgeRequireHTTPSOn(ctx context.Context) bool {
	if s.Portal == nil {
		return false
	}
	return s.edgeSwitch.Get(ctx, edgeSchemeSwitchTTL, s.Portal.CertEdgeRequireHTTPSChecked)
}

// invalidateEdgeRequireHTTPSCache drops the cached switch so the next request
// re-reads it. Called by handleSystemSettings after a successful PUT that carried
// cert_edge_require_https, so an operator toggling the switch in the portal does
// not have to wait out edgeSchemeSwitchTTL. Also cancels any store read already
// in flight (see edgeRequireHTTPSOn / settingCache.Get), so an in-flight
// pre-disarm read cannot re-arm the cache behind this invalidation.
func (s *Server) invalidateEdgeRequireHTTPSCache() {
	s.edgeSwitch.Invalidate()
}

// edgeGateVerdict decides what to do with one request on the PUBLIC listener.
//
// It refuses only while ALL FOUR of these hold -- stated as one positive
// conjunction because every clause is a reason NOT to lock the operator out:
//
//	(a) the operator armed the switch (cert_edge_require_https),
//	(b) an encrypted request was observed inside edgeSchemeObservationWindow (so
//	    the gate EXTINGUISHES ITSELF when the encrypted path breaks),
//	(c) the path is none of the four unconditionally-open ones, and
//	(d) the emergency kill switch is NOT set.
//
// Returns (redirect, refuse). GET/HEAD get a 307 to the https form of the same
// URL -- a browser navigating the portal recovers on its own -- while every other
// method gets a hard 403 certificate.https_required, because redirecting a POST
// would silently drop its body.
//
// The conjunction is the same whichever order the clauses are evaluated in, so
// the order below is chosen for cost and blast radius: the two allocation-free
// in-process checks first, then the in-memory observation, and the ONLY call that
// leaves this package -- the settings read -- last. Two consequences worth stating,
// because they are load-bearing rather than incidental:
//
//   - Checking the hop before anything expensive is a pure optimisation: an
//     encrypted hop can never be refused under any combination of the other
//     clauses, and in a working armed deployment that is ALL real traffic.
//   - Putting the observation check before the switch read means a gateway that
//     has never seen encrypted traffic never touches the settings store from this
//     path at all. That also keeps the gate off the many partial portal fakes in
//     this package's tests (they embed a nil portal.API and would panic on any
//     method they do not override), so adding the gate to the shared request path
//     cannot break an unrelated test that never populates the tracker.
func (s *Server) edgeGateVerdict(r *http.Request) (redirect bool, refuse bool) {
	// (d) The kill switch: first, cheapest, and the one check that must keep
	// working when the portal and the settings store are unreachable.
	if s.certEdgeRequireHTTPSDisable {
		return false, false
	}
	// (c) The four open paths.
	if edgeGateOpenPath(r.URL.Path) || s.edgeGateInternalCaller(r) {
		return false, false
	}
	// Already encrypted: nothing to enforce.
	if hopEncrypted(r) {
		return false, false
	}
	// (b) The fail-safe: refuse plaintext only while encryption is demonstrably
	// working. A nil tracker reports "never observed" and therefore never refuses.
	if encrypted, _, _ := s.edgeScheme.Seen(time.Now(), edgeSchemeObservationWindow); !encrypted {
		return false, false
	}
	// (a) The operator's switch.
	if !s.edgeRequireHTTPSOn(r.Context()) {
		return false, false
	}
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		return true, false
	}
	return false, true
}

// writeEdgeGateRefusal emits the gate's verdict. The redirect target keeps the
// host, path AND query of the original request; an explicit :80 in the Host
// header is dropped because the https form of ":80" is the default 443, while any
// other port is left alone (the gateway cannot know the proxy's http->https port
// mapping -- see the known limits in the runbook).
//
// The status is **307** (temporary, method-preserving), NOT 301. A 301 carries no
// Cache-Control of its own and is heuristically cacheable indefinitely, so after the
// operator disarms the gate -- via the kill switch or a loopback curl, i.e. exactly
// the two emergency recovery paths -- their own browser would keep redirecting every
// portal GET to the https URL they could not reach, and they would conclude the
// disarm failed. The actual fix would be "clear your browser cache", which no
// runbook step names. A PERMANENT redirect is also semantically wrong for a
// condition driven by a runtime toggle that can flip on the very next request and
// that deliberately extinguishes itself. Cache-Control: no-store is sent as well, so
// the answer cannot be reused even by a cache that ignores 307's default.
//
// A request with no Host at all (a bare HTTP/1.0 client) cannot be redirected to a
// meaningful absolute URL, so it falls through to the 403 rather than emitting a
// malformed "https:///path".
func writeEdgeGateRefusal(w http.ResponseWriter, r *http.Request, redirect bool) {
	if redirect && r.Host != "" {
		host := r.Host
		if h, port, err := net.SplitHostPort(host); err == nil && port == "80" {
			host = h
		}
		w.Header().Set("Cache-Control", "no-store")
		http.Redirect(w, r, "https://"+host+r.URL.RequestURI(), http.StatusTemporaryRedirect)
		return
	}
	writeJSON(w, http.StatusForbidden, apierror.Response(
		"certificate.https_required",
		"this gateway refuses unencrypted requests; use https",
		""))
}

// shouldLogEdgeGateRefusal reports whether this refusal should emit a log line:
// the FIRST refusal for any given path is always immediate, and after that at most
// one per path per edgeGateWarnInterval (via warnThrottle -- ttlcache.go). Keyed by
// path (not by remote) so a distributed retry storm against one endpoint still
// collapses to one line. A path is only ever inserted when it is actually refused
// (post-exclusion, post-arming), so an attacker cannot grow the throttle's map by
// walking arbitrary URLs on an unarmed gateway.
func (s *Server) shouldLogEdgeGateRefusal(path string, now time.Time) bool {
	return s.edgeWarn.ShouldLog(path, now, edgeGateWarnInterval)
}

// noteEdgeScheme records one hop observation for the arming precondition. Called
// for every PUBLIC request; countsAsObservation decides whether it may count.
// Nil-safe via edgeSchemeTracker.Note.
func (s *Server) noteEdgeScheme(r *http.Request, now time.Time) {
	if !countsAsObservation(r.URL.Path, r.RemoteAddr, isEdgeSelfProbe(r)) {
		return
	}
	s.edgeScheme.Note(hopEncrypted(r), now)
}

// ArmEdgeRequireHTTPS is the precondition for switching the plaintext gate ON.
// TWO independent conditions must hold, each refusing with its own error:
//
//	(1) an encrypted request was observed inside edgeSchemeObservationWindow
//	    (errEdgeHTTPSNotObserved) -- otherwise an operator could arm a total
//	    lockout of their own gateway on nothing but optimism, e.g. before the
//	    fronting proxy's TLS listener has ever worked; and
//	(2) the ARMING REQUEST ITSELF arrived over an encrypted hop
//	    (errEdgeArmHopPlaintext).
//
// (2) exists because (1) alone is satisfiable by SOMEBODY ELSE'S traffic. A proxy
// that terminates TLS for most clients but reaches this gateway in the clear on
// the operator's own route lets that operator arm the gate against a stranger's
// evidence and be refused by their very next request -- the first lockout scenario
// the runbook documents (§8.2a). Requiring the armer's own hop to be encrypted
// makes that outcome impossible BY CONSTRUCTION for whoever armed it: the request
// that armed the gate is, by definition, a request the armed gate would have let
// through.
//
// Order matters for the error the operator reads. The observation is checked
// FIRST, so a gateway where TLS has never worked at all still reports the more
// fundamental "never observed" rather than the narrower hop complaint.
//
// Turning the switch OFF is never gated (the settings PUT only calls this when
// the request sets it to true), and neither condition applies there -- so a
// plaintext loopback curl, one of the two documented recovery paths, can always
// disarm.
//
// r supplies the hop header; r.Context() is what a future evidence source (e.g. a
// persisted last-encrypted timestamp) would read, so no call-site change is
// needed for one.
func (s *Server) ArmEdgeRequireHTTPS(r *http.Request) error {
	if encrypted, _, _ := s.edgeScheme.Seen(time.Now(), edgeSchemeObservationWindow); !encrypted {
		return errEdgeHTTPSNotObserved
	}
	if !hopEncrypted(r) {
		return errEdgeArmHopPlaintext
	}
	return nil
}
