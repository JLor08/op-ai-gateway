// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/logbuffer"
	"op-ai-gateway/internal/portal"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestTrackerOnlyCountsRealExternalTraffic(t *testing.T) {
	tr := &edgeSchemeTracker{}
	base := time.Unix(1_700_000_000, 0).UTC()
	// These must NOT count as observations — each would let the arming
	// precondition pass without proving anything about the fronting proxy. The
	// fourth case is the gateway's OWN synthetic TLS self-probe: it satisfies
	// every other predicate here (not /healthz, not the agent path, and in a
	// deployment where the fronting proxy shares the host it is not even
	// loopback), so without this exclusion the precondition would pass on the
	// strength of the gateway convincing itself.
	for _, c := range []struct {
		path, remote string
		selfProbe    bool
	}{
		{"/healthz", "203.0.113.9:1", false},
		{"/api/agent/v1/telemetry", "203.0.113.9:1", false},
		{"/api/portal/me", "127.0.0.1:5555", false},
		{"/api/portal/me", "203.0.113.9:1", true},
	} {
		if countsAsObservation(c.path, c.remote, c.selfProbe) {
			t.Errorf("countsAsObservation(%q, %q, selfProbe=%v) = true, want false", c.path, c.remote, c.selfProbe)
		}
	}
	if !countsAsObservation("/api/portal/me", "203.0.113.9:1", false) {
		t.Fatal("an ordinary external portal request must count")
	}
	tr.Note(true, base)
	enc, lastEnc, _ := tr.Seen(base.Add(time.Minute), 5*time.Minute)
	if !enc || !lastEnc.Equal(base) {
		t.Fatalf("Seen = (%v, %v), want an encrypted observation at %v", enc, lastEnc, base)
	}
	if enc, _, _ := tr.Seen(base.Add(6*time.Minute), 5*time.Minute); enc {
		t.Fatal("an observation older than the window must not count")
	}
}

// TestNewCopiesCertEdgeRequireHTTPSDisableFromDeps pins that the plan-B kill
// switch reaches *Server as a plain in-process bool, NOT via s.Portal / any
// store lookup -- that is the entire point (see ServerDeps.
// CertEdgeRequireHTTPSDisable's doc comment): the plaintext-refusal gate (a
// later task) must be able to check it even when the settings store, or the
// portal itself, is unreachable. Dropping the copy in New() would silently
// leave the kill switch permanently disengaged regardless of the configured
// env var, with the rest of the Go suite still green.
func TestNewCopiesCertEdgeRequireHTTPSDisableFromDeps(t *testing.T) {
	if New(ServerDeps{}).certEdgeRequireHTTPSDisable {
		t.Fatal("New(ServerDeps{}) = true, want false (default off)")
	}
	if !New(ServerDeps{CertEdgeRequireHTTPSDisable: true}).certEdgeRequireHTTPSDisable {
		t.Fatal("New(ServerDeps{CertEdgeRequireHTTPSDisable: true}).certEdgeRequireHTTPSDisable = false, want true")
	}
}

// ---------------------------------------------------------------------------
// The plaintext-refusal gate.
//
// This gate sits in front of the portal, the whole API and /api/auth/login, so a
// mistake here does not produce a bug -- it locks the operator out of their own
// gateway, or cuts every agent off, or kills every background chat run. The
// tests below are therefore organised around the ways it must REFUSE TO REFUSE:
// the four unconditionally-open paths, the missing-observation fail-safe, and
// the emergency kill switch. Only after those does one test pin that it actually
// refuses when it should.
// ---------------------------------------------------------------------------

// testGateSecret stands in for s.internalAuthSecret (the per-process secret the
// background chat-run executor presents when it calls the gateway's own
// /v1/chat/completions over loopback).
const testGateSecret = "internal-loopback-secret-for-tests"

// fakePortalEdgeRequireHTTPS reports the plan-B plaintext switch, and nothing
// else -- same nil-embedded-interface trick as fakePortalRotateInProgress
// (certificates_test.go): only the overridden method is ever called.
type fakePortalEdgeRequireHTTPS struct {
	portal.API
	on bool
}

func (f fakePortalEdgeRequireHTTPS) CertEdgeRequireHTTPSChecked(context.Context) bool { return f.on }

// NetbirdOnly is off here: agentSourceRefused (agent_netbird_gate.go) reads it
// on every agent-listener request now, so a fake driven through AgentHandler
// (TestEdgeGateOnlyGuardsThePublicMux) must answer it -- this fake's tests are
// about the plaintext-edge gate, not the netbird_only source gate, so "off"
// keeps the source gate a no-op.
func (f fakePortalEdgeRequireHTTPS) NetbirdOnly(context.Context) bool { return false }

// newGateServer builds the smallest *Server the gate reads, with BOTH muxes
// wired to a catch-all handler that writes "served" -- so a test can distinguish
// "the gate let this through" (200 served) from "the gate refused" (403/307,
// never reaching the handler) AND can drive the two REAL listener entry points
// (ServeHTTP for the public listener, AgentHandler for the NetBird one) instead
// of calling serveWith with a hand-picked mux flag.
//
// switchOn/killSwitch are the two operator-controlled inputs; the caller adds
// the observation (Note) itself, because "armed but nothing observed" is one of
// the states under test.
func newGateServer(t *testing.T, switchOn, killSwitch bool) *Server {
	t.Helper()
	served := func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("served"))
	}
	s := &Server{
		Portal:                      fakePortalEdgeRequireHTTPS{on: switchOn},
		edgeScheme:                  &edgeSchemeTracker{},
		internalAuthSecret:          testGateSecret,
		certEdgeRequireHTTPSDisable: killSwitch,
		mux:                         http.NewServeMux(),
		agentMux:                    http.NewServeMux(),
	}
	s.mux.HandleFunc("/", served)
	s.agentMux.HandleFunc("/", served)
	return s
}

// newArmedGateServer is newGateServer in the ONE state where the gate refuses:
// switch on, kill switch off, and a FRESH encrypted observation. Every test that
// then observes "served" is observing a deliberate exclusion, not an accident.
func newArmedGateServer(t *testing.T) *Server {
	t.Helper()
	s := newGateServer(t, true, false)
	s.edgeScheme.Note(true, time.Now())
	return s
}

// plainGateRequest is a request whose last hop was UNENCRYPTED: no
// X-OP-Edge-Scheme header at all, which is exactly what a client reaching the
// nginx :80 server block produces (the :443 block sets the header from $scheme).
func plainGateRequest(method, path, remote string) *http.Request {
	r := httptest.NewRequest(method, path, nil)
	r.RemoteAddr = remote
	return r
}

// TestEdgeGateNeverRefusesTheFourOpenPaths is the most important test in this
// file. Each case would break something already merged if the gate closed over
// it:
//
//  1. /.well-known/acme-challenge/ -- the renewal of the very certificate that
//     makes enforcement possible goes through here over plain HTTP-01.
//  2. /healthz -- the k8s readiness AND liveness probes hit it from the node
//     (non-loopback), and the frontend connection gate polls it every 4s. (There
//     is no /healthz healthcheck in either compose file -- the only healthcheck
//     there is db's pg_isready.)
//  3. /api/agent/v1/ on the public mux -- every agent route is registered there
//     too and the bundled nginx configs proxy them on port 80; in the
//     no-netbird topology that is the agents' ONLY path.
//  4. Loopback / the internal trusted path -- background chat runs POST to the
//     gateway's own http://127.0.0.1:<port>/v1/chat/completions, bypassing nginx
//     entirely, so they carry no hop header at all.
func TestEdgeGateNeverRefusesTheFourOpenPaths(t *testing.T) {
	s := newArmedGateServer(t)

	// Anti-vacuity: the fixture really is armed, so a "served" below means an
	// exclusion fired -- not that the gate was off all along.
	if _, refuse := s.edgeGateVerdict(plainGateRequest(http.MethodPost, "/v1/chat/completions", "203.0.113.9:1")); !refuse {
		t.Fatal("fixture is not armed: an ordinary external plaintext API POST must be refused")
	}

	for _, c := range []struct{ name, method, path, remote, internalAuth string }{
		{"acme challenge", http.MethodGet, "/.well-known/acme-challenge/tok", "203.0.113.9:1", ""},
		{"healthz", http.MethodGet, "/healthz", "203.0.113.9:1", ""},
		{"agent telemetry", http.MethodPost, "/api/agent/v1/telemetry", "203.0.113.9:1", ""},
		{"agent stream", http.MethodGet, "/api/agent/v1/stream", "203.0.113.9:1", ""},
		{"loopback chat run", http.MethodPost, "/v1/chat/completions", "127.0.0.1:5555", ""},
		{"loopback ipv6 chat run", http.MethodPost, "/v1/chat/completions", "[::1]:5555", ""},
		{"internal auth header", http.MethodPost, "/v1/chat/completions", "203.0.113.9:1", testGateSecret},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := plainGateRequest(c.method, c.path, c.remote)
			if c.internalAuth != "" {
				r.Header.Set(internalAuthHeaderName, c.internalAuth)
			}
			// Neither verdict: a 307 on the ACME challenge or on /healthz breaks
			// them just as thoroughly as a 403 would.
			if redirect, refuse := s.edgeGateVerdict(r); redirect || refuse {
				t.Fatalf("edgeGateVerdict = (redirect %v, refuse %v), want (false, false): this path must stay open", redirect, refuse)
			}
			// And end-to-end through the real public listener entry point: the
			// handler must actually run.
			rec := httptest.NewRecorder()
			r2 := plainGateRequest(c.method, c.path, c.remote)
			if c.internalAuth != "" {
				r2.Header.Set(internalAuthHeaderName, c.internalAuth)
			}
			s.ServeHTTP(rec, r2)
			if rec.Code != http.StatusOK || rec.Body.String() != "served" {
				t.Fatalf("ServeHTTP = %d %q, want 200 \"served\"", rec.Code, rec.Body.String())
			}
		})
	}
}

// TestEdgeGateInternalHeaderExemptionRequiresTheRealSecret: the loopback
// exemption keys on a constant-time comparison against s.internalAuthSecret, so
// a client that merely GUESSES the header name cannot buy itself an exemption.
// (nginx also blanks X-OP-Internal-* at the public edge; this is the in-process
// half of that defence.)
func TestEdgeGateInternalHeaderExemptionRequiresTheRealSecret(t *testing.T) {
	s := newArmedGateServer(t)
	for _, presented := range []string{"wrong", testGateSecret + "x", testGateSecret[:len(testGateSecret)-1], ""} {
		r := plainGateRequest(http.MethodPost, "/v1/chat/completions", "203.0.113.9:1")
		r.Header.Set(internalAuthHeaderName, presented)
		if _, refuse := s.edgeGateVerdict(r); !refuse {
			t.Fatalf("internal-auth header %q bought an exemption; only the real secret may", presented)
		}
	}

	// And with NO secret configured the header can never exempt anything, even
	// if a client sends an empty value matching the empty secret.
	s.internalAuthSecret = ""
	r := plainGateRequest(http.MethodPost, "/v1/chat/completions", "203.0.113.9:1")
	r.Header.Set(internalAuthHeaderName, "")
	if _, refuse := s.edgeGateVerdict(r); !refuse {
		t.Fatal("an unconfigured internal secret must not make the header exempt anything")
	}
}

// TestEdgeGateRefusesAPlainAPIRequestAndRedirectsAPortalGET pins the two
// client-visible outcomes: a browser navigation gets a redirect it can follow, a
// programmatic call gets a hard refusal with a machine-readable code (a redirect
// on a POST would silently drop the body).
func TestEdgeGateRefusesAPlainAPIRequestAndRedirectsAPortalGET(t *testing.T) {
	s := newArmedGateServer(t)

	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, plainGateRequest(http.MethodPost, "/v1/chat/completions", "203.0.113.9:1"))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("plaintext API POST = %d, want 403 (body %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "certificate.https_required") {
		t.Fatalf("body = %s, want the certificate.https_required code", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "served") {
		t.Fatal("the gate must short-circuit BEFORE the mux -- the handler ran")
	}

	for _, method := range []string{http.MethodGet, http.MethodHead} {
		rec = httptest.NewRecorder()
		r := plainGateRequest(method, "/api/portal/me?x=1", "203.0.113.9:1")
		s.ServeHTTP(rec, r)
		// 307, NOT 301: a permanent redirect is heuristically cacheable forever, so
		// after the operator disarms the gate their browser would keep redirecting to
		// an https URL they cannot reach and they would think the disarm failed.
		if rec.Code != http.StatusTemporaryRedirect {
			t.Fatalf("plaintext portal %s = %d, want 307 (body %s)", method, rec.Code, rec.Body.String())
		}
		if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
			t.Fatalf("Cache-Control = %q, want no-store: the redirect must never be cached", cc)
		}
		if got, want := rec.Header().Get("Location"), "https://example.com/api/portal/me?x=1"; got != want {
			t.Fatalf("Location = %q, want %q (host, path AND query preserved)", got, want)
		}
	}

	// An encrypted hop is served normally -- the gate is not a blanket block.
	rec = httptest.NewRecorder()
	r := plainGateRequest(http.MethodPost, "/v1/chat/completions", "203.0.113.9:1")
	r.Header.Set(edgeSchemeHeaderName, "https")
	s.ServeHTTP(rec, r)
	if rec.Code != http.StatusOK || rec.Body.String() != "served" {
		t.Fatalf("encrypted hop = %d %q, want 200 \"served\"", rec.Code, rec.Body.String())
	}
}

// TestEdgeGateOnlyGuardsThePublicMux: the NetBird agent listener shares
// serveWith with the public listener, and gating it would be the agent lockout
// that is explicitly deferred to a later phase. Driven through the REAL
// AgentHandler entry point so the flag actually passed at that call site is what
// gets tested -- with a path that IS refused on the public listener, so the test
// cannot pass on the strength of the agent-path exclusion.
func TestEdgeGateOnlyGuardsThePublicMux(t *testing.T) {
	s := newArmedGateServer(t)

	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, plainGateRequest(http.MethodPost, "/v1/chat/completions", "203.0.113.9:1"))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("public listener = %d, want 403 (precondition for this test)", rec.Code)
	}

	rec = httptest.NewRecorder()
	s.AgentHandler().ServeHTTP(rec, plainGateRequest(http.MethodPost, "/v1/chat/completions", "203.0.113.9:1"))
	if rec.Code != http.StatusOK || rec.Body.String() != "served" {
		t.Fatalf("agent listener = %d %q, want 200 \"served\": the gate must not run on the agent mux", rec.Code, rec.Body.String())
	}
}

// TestEdgeGateTrustsOnlyItsOwnHopHeader: X-OP-Edge-Scheme is trustworthy ONLY
// because nginx SETS it from $scheme in every one of its nine header-setting
// blocks, so a client-supplied value is always overwritten (plan A proved that in
// a real container; the nginx config test pins it). X-Forwarded-Proto has no such
// guarantee -- a client can send it and nginx passes it through -- so the gate must
// never read it. A handler test cannot distinguish a forged X-OP-Edge-Scheme from
// a genuine one; what it CAN pin is that no other header buys a bypass.
func TestEdgeGateTrustsOnlyItsOwnHopHeader(t *testing.T) {
	s := newArmedGateServer(t)
	for _, h := range []struct{ name, value string }{
		{"X-Forwarded-Proto", "https"},
		{"X-Forwarded-Protocol", "https"},
		{"X-Forwarded-Ssl", "on"},
		{"X-Url-Scheme", "https"},
		{"Front-End-Https", "on"},
	} {
		r := plainGateRequest(http.MethodPost, "/v1/chat/completions", "203.0.113.9:1")
		r.Header.Set(h.name, h.value)
		if _, refuse := s.edgeGateVerdict(r); !refuse {
			t.Fatalf("%s: %s bought a bypass; only %s (which nginx overwrites) may be trusted", h.name, h.value, edgeSchemeHeaderName)
		}
	}
	// The one header that IS trusted, and case-insensitively (nginx's $scheme is
	// lowercase, but be liberal about what we accept as proof of encryption).
	for _, v := range []string{"https", "HTTPS", "Https"} {
		r := plainGateRequest(http.MethodPost, "/v1/chat/completions", "203.0.113.9:1")
		r.Header.Set(edgeSchemeHeaderName, v)
		if _, refuse := s.edgeGateVerdict(r); refuse {
			t.Fatalf("%s: %q must count as an encrypted hop", edgeSchemeHeaderName, v)
		}
	}
	// Anything else on that header is NOT proof of encryption.
	for _, v := range []string{"http", "", "wss", "https " /* trailing space */} {
		r := plainGateRequest(http.MethodPost, "/v1/chat/completions", "203.0.113.9:1")
		r.Header.Set(edgeSchemeHeaderName, v)
		if _, refuse := s.edgeGateVerdict(r); !refuse {
			t.Fatalf("%s: %q must NOT count as an encrypted hop", edgeSchemeHeaderName, v)
		}
	}
}

// TestEdgeGateFallsOpenWithoutAnObservation is the runtime fail-safe: the gate
// EXTINGUISHES ITSELF when the encrypted path breaks. If the fronting proxy's
// TLS listener dies, no encrypted request arrives, the window lapses, and the
// gateway starts answering plaintext again instead of refusing everything --
// which is the difference between a degraded gateway and a locked-out operator.
func TestEdgeGateFallsOpenWithoutAnObservation(t *testing.T) {
	// (a) armed switch, nothing ever observed.
	s := newGateServer(t, true, false)
	if _, refuse := s.edgeGateVerdict(plainGateRequest(http.MethodPost, "/v1/chat/completions", "203.0.113.9:1")); refuse {
		t.Fatal("armed with NO encrypted observation must serve, not refuse")
	}

	// (b) an observation that has fallen out of the window (the self-extinguish).
	s = newGateServer(t, true, false)
	s.edgeScheme.Note(true, time.Now().Add(-edgeSchemeObservationWindow-time.Minute))
	if _, refuse := s.edgeGateVerdict(plainGateRequest(http.MethodPost, "/v1/chat/completions", "203.0.113.9:1")); refuse {
		t.Fatal("an encrypted observation older than the window must not keep the gate armed")
	}

	// (c) plaintext observations alone never arm it.
	s = newGateServer(t, true, false)
	s.edgeScheme.Note(false, time.Now())
	if _, refuse := s.edgeGateVerdict(plainGateRequest(http.MethodPost, "/v1/chat/completions", "203.0.113.9:1")); refuse {
		t.Fatal("a plaintext observation must not arm the gate")
	}
}

// TestEdgeGateFallsOpenWithTheKillSwitch: the env-var override must work even
// when the portal (and the settings store behind it) is unreachable, which is why
// it is a plain in-process bool and is checked FIRST.
func TestEdgeGateFallsOpenWithTheKillSwitch(t *testing.T) {
	s := newGateServer(t, true, true)
	s.edgeScheme.Note(true, time.Now())
	if _, refuse := s.edgeGateVerdict(plainGateRequest(http.MethodPost, "/v1/chat/completions", "203.0.113.9:1")); refuse {
		t.Fatal("the kill switch must disengage the gate even when armed and observed")
	}
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, plainGateRequest(http.MethodPost, "/v1/chat/completions", "203.0.113.9:1"))
	if rec.Code != http.StatusOK || rec.Body.String() != "served" {
		t.Fatalf("kill switch end-to-end = %d %q, want 200 \"served\"", rec.Code, rec.Body.String())
	}
}

// TestEdgeGateIsANoOpWithTheSwitchOff is the default-deployment invariant: with
// cert_edge_require_https unset (false) nothing is refused, whatever else is true.
func TestEdgeGateIsANoOpWithTheSwitchOff(t *testing.T) {
	s := newGateServer(t, false, false)
	s.edgeScheme.Note(true, time.Now())
	for _, c := range []struct{ method, path string }{
		{http.MethodPost, "/v1/chat/completions"},
		{http.MethodGet, "/api/portal/me"},
		{http.MethodPost, "/api/auth/login"},
	} {
		if redirect, refuse := s.edgeGateVerdict(plainGateRequest(c.method, c.path, "203.0.113.9:1")); redirect || refuse {
			t.Fatalf("%s %s with the switch off = (redirect %v, refuse %v), want (false, false)", c.method, c.path, redirect, refuse)
		}
	}
}

// TestEdgeGateIsNilSafe: a bare &Server{} (no portal, no tracker) must never
// refuse. Several tests in this package build a *Server directly, and a nil
// dereference on the shared request path would be a crash on every request.
func TestEdgeGateIsNilSafe(t *testing.T) {
	s := &Server{}
	if redirect, refuse := s.edgeGateVerdict(plainGateRequest(http.MethodPost, "/v1/chat/completions", "203.0.113.9:1")); redirect || refuse {
		t.Fatalf("bare &Server{} = (redirect %v, refuse %v), want (false, false)", redirect, refuse)
	}
	// And the note-taking side must not panic either.
	s.noteEdgeScheme(plainGateRequest(http.MethodGet, "/api/portal/me", "203.0.113.9:1"), time.Now())
}

// TestEdgeGateRunsBeforeAuthentication: an UNAUTHENTICATED plaintext request is
// refused too. If the gate sat after auth, an attacker probing /api/auth/login
// over plaintext would still get to submit credentials in the clear.
func TestEdgeGateRunsBeforeAuthentication(t *testing.T) {
	s := newArmedGateServer(t)
	rec := httptest.NewRecorder()
	// No Authorization header, no cookie.
	s.ServeHTTP(rec, plainGateRequest(http.MethodPost, "/api/auth/login", "203.0.113.9:1"))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("unauthenticated plaintext login = %d, want 403 (body %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "certificate.https_required") {
		t.Fatalf("body = %s, want certificate.https_required (not an auth error)", rec.Body.String())
	}
}

// TestServeWithNotesOnlyRealExternalObservations wires the tracker to the actual
// request path: an external request's hop is recorded, and each of the four
// non-observable classes is not. Without this the arming precondition would be
// satisfiable by traffic that never went through the fronting proxy.
func TestServeWithNotesOnlyRealExternalObservations(t *testing.T) {
	t.Run("encrypted external request is recorded", func(t *testing.T) {
		s := newGateServer(t, false, false)
		r := plainGateRequest(http.MethodGet, "/api/portal/me", "203.0.113.9:1")
		r.Header.Set(edgeSchemeHeaderName, "https")
		s.ServeHTTP(httptest.NewRecorder(), r)
		enc, lastEnc, _ := s.edgeScheme.Seen(time.Now(), edgeSchemeObservationWindow)
		if !enc || lastEnc.IsZero() {
			t.Fatal("an encrypted external request must be recorded as an encrypted observation")
		}
	})

	t.Run("plaintext external request is recorded as plaintext only", func(t *testing.T) {
		s := newGateServer(t, false, false)
		s.ServeHTTP(httptest.NewRecorder(), plainGateRequest(http.MethodGet, "/api/portal/me", "203.0.113.9:1"))
		enc, _, lastPlain := s.edgeScheme.Seen(time.Now(), edgeSchemeObservationWindow)
		if enc {
			t.Fatal("a plaintext request must never register as encrypted")
		}
		if lastPlain.IsZero() {
			t.Fatal("a plaintext external request must be recorded as a plaintext observation")
		}
	})

	// Every one of these carries an ENCRYPTED hop, so the only reason it must not
	// arm the precondition is its own exclusion.
	for _, c := range []struct {
		name, path, remote string
		selfProbe          bool
	}{
		{"healthz", "/healthz", "203.0.113.9:1", false},
		{"agent route", "/api/agent/v1/telemetry", "203.0.113.9:1", false},
		{"loopback", "/api/portal/me", "127.0.0.1:5555", false},
		{"self probe", "/api/portal/me", "203.0.113.9:1", true},
	} {
		t.Run("not observable: "+c.name, func(t *testing.T) {
			s := newGateServer(t, false, false)
			r := plainGateRequest(http.MethodGet, c.path, c.remote)
			r.Header.Set(edgeSchemeHeaderName, "https")
			if c.selfProbe {
				r.Header.Set(edgeSchemeSelfProbeHeaderName, "1")
			}
			s.ServeHTTP(httptest.NewRecorder(), r)
			if enc, _, _ := s.edgeScheme.Seen(time.Now(), edgeSchemeObservationWindow); enc {
				t.Fatalf("%s must not count as an encrypted observation", c.name)
			}
		})
	}
}

// armRequest builds an arming PUT whose hop is encrypted or not, so a test can
// isolate ONE of the two arming conditions at a time.
func armRequest(encrypted bool) *http.Request {
	r := httptest.NewRequest(http.MethodPut, "/api/system/settings", nil)
	if encrypted {
		r.Header.Set(edgeSchemeHeaderName, "https")
	}
	return r
}

// TestArmEdgeRequireHTTPSRequiresAnObservation is arming condition 1: an
// operator can only switch the gate ON against evidence that encrypted traffic
// actually reaches this gateway. Every case here sends an ENCRYPTED hop so the
// failures below can only come from the observation clause. Turning it OFF is
// never blocked.
func TestArmEdgeRequireHTTPSRequiresAnObservation(t *testing.T) {
	s := newGateServer(t, false, false)
	if err := s.ArmEdgeRequireHTTPS(armRequest(true)); !errors.Is(err, errEdgeHTTPSNotObserved) {
		t.Fatalf("arming with no observation = %v, want errEdgeHTTPSNotObserved", err)
	}
	s.edgeScheme.Note(false, time.Now())
	if err := s.ArmEdgeRequireHTTPS(armRequest(true)); !errors.Is(err, errEdgeHTTPSNotObserved) {
		t.Fatalf("arming on a PLAINTEXT observation = %v, want errEdgeHTTPSNotObserved", err)
	}
	s.edgeScheme.Note(true, time.Now().Add(-edgeSchemeObservationWindow-time.Minute))
	if err := s.ArmEdgeRequireHTTPS(armRequest(true)); !errors.Is(err, errEdgeHTTPSNotObserved) {
		t.Fatalf("arming on a STALE encrypted observation = %v, want errEdgeHTTPSNotObserved", err)
	}
	s.edgeScheme.Note(true, time.Now())
	if err := s.ArmEdgeRequireHTTPS(armRequest(true)); err != nil {
		t.Fatalf("arming on a fresh encrypted observation = %v, want nil", err)
	}
	// A bare &Server{} has no tracker: fail safe, refuse to arm rather than arm
	// a lockout on no evidence at all.
	if err := (&Server{}).ArmEdgeRequireHTTPS(armRequest(true)); !errors.Is(err, errEdgeHTTPSNotObserved) {
		t.Fatalf("arming with no tracker = %v, want errEdgeHTTPSNotObserved", err)
	}
}

// TestArmEdgeRequireHTTPSRequiresAnEncryptedOwnHop is arming condition 2 -- the
// one that makes runbook scenario 8.2a impossible for whoever arms the gate.
// Condition 1 is satisfiable by SOMEBODY ELSE'S traffic: a proxy that terminates
// TLS for other clients but reaches this gateway in the clear on the operator's
// own route keeps the observation fresh, so without this check that operator
// could arm the gate and be 403'd by their very own next request. The request
// that arms the gate must be one the armed gate would have let through.
func TestArmEdgeRequireHTTPSRequiresAnEncryptedOwnHop(t *testing.T) {
	s := newGateServer(t, false, false)
	// Somebody else's encrypted traffic: condition 1 is satisfied.
	s.edgeScheme.Note(true, time.Now())
	if err := s.ArmEdgeRequireHTTPS(armRequest(true)); err != nil {
		t.Fatalf("precondition: arming over an encrypted hop = %v, want nil", err)
	}

	// Same observation, but THIS request arrived in the clear.
	err := s.ArmEdgeRequireHTTPS(armRequest(false))
	if !errors.Is(err, errEdgeArmHopPlaintext) {
		t.Fatalf("arming over a PLAINTEXT hop = %v, want errEdgeArmHopPlaintext", err)
	}
	// It must NOT be reported as "never observed": the panel next to the switch
	// shows a fresh last-encrypted timestamp, so that message would contradict it.
	if errors.Is(err, errEdgeHTTPSNotObserved) {
		t.Fatal("a plaintext arming hop must not be reported as errEdgeHTTPSNotObserved -- the two conditions are distinct")
	}

	// A hop header that is not exactly "https" is not proof either (mirrors
	// hopEncrypted, which the armed gate uses on the very same header).
	r := armRequest(false)
	r.Header.Set(edgeSchemeHeaderName, "http")
	if err := s.ArmEdgeRequireHTTPS(r); !errors.Is(err, errEdgeArmHopPlaintext) {
		t.Fatalf("arming with X-OP-Edge-Scheme: http = %v, want errEdgeArmHopPlaintext", err)
	}
}

// TestSystemSettingsPUTGatesArmingOnAnObservation drives the arming precondition
// through the REAL settings endpoint with a real portal service, because that
// wiring -- not ArmEdgeRequireHTTPS itself -- is what stops an operator arming a
// lockout. It also pins that the refusal stores NOTHING (a later GET still reports
// the switch off) and that turning the gate OFF is never blocked.
func TestSystemSettingsPUTGatesArmingOnAnObservation(t *testing.T) {
	srv, _ := newTestServerWithACME(t, nil)

	// encrypted mirrors what the operator's own browser session looks like once the
	// fronting proxy terminates TLS: nginx's :443 block sets the hop header. Once
	// the gate is armed the portal is only reachable that way, which is the point --
	// so the settings calls after arming must model it, exactly as a real operator
	// would experience.
	settings := func(method, body string, encrypted bool) *httptest.ResponseRecorder {
		var r *http.Request
		if body == "" {
			r = certSystemRequest(t, method, "/api/system/settings", nil)
		} else {
			r = certSystemRequest(t, method, "/api/system/settings", strings.NewReader(body))
		}
		if encrypted {
			r.Header.Set(edgeSchemeHeaderName, "https")
		}
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, r)
		return rec
	}
	switchStored := func(encrypted bool) bool {
		rec := settings(http.MethodGet, "", encrypted)
		if rec.Code != http.StatusOK {
			t.Fatalf("settings GET = %d, body %s", rec.Code, rec.Body.String())
		}
		return strings.Contains(rec.Body.String(), `"cert_edge_require_https":true`)
	}

	// No encrypted request has ever arrived -> arming is refused, and nothing is
	// written.
	rec := settings(http.MethodPut, `{"cert_edge_require_https":true}`, false)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("arming without an observation = %d, want 400 (body %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "certificate.edge_https_not_observed") {
		t.Fatalf("body = %s, want the certificate.edge_https_not_observed code", rec.Body.String())
	}
	if switchStored(false) {
		t.Fatal("a refused arming attempt must not store the switch")
	}

	// An observation now exists, but the operator's OWN route is plaintext: refused
	// with a DISTINCT code, because "no encrypted request observed" would flatly
	// contradict the fresh timestamp the panel is showing them. This is the
	// endpoint-level proof that runbook scenario 8.2a cannot be armed into
	// existence.
	srv.edgeScheme.Note(true, time.Now())
	rec = settings(http.MethodPut, `{"cert_edge_require_https":true}`, false)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("arming over a plaintext hop = %d, want 400 (body %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "certificate.edge_arm_requires_https") {
		t.Fatalf("body = %s, want the certificate.edge_arm_requires_https code", rec.Body.String())
	}
	if switchStored(false) {
		t.Fatal("an arming attempt refused for a plaintext hop must not store the switch")
	}

	// Turning it OFF is allowed with no encrypted hop and (as the very first PUT
	// above showed) no observation whatsoever -- disarming must never be blocked,
	// because the loopback-curl recovery path has neither.
	if rec := settings(http.MethodPut, `{"cert_edge_require_https":false}`, false); rec.Code != http.StatusOK {
		t.Fatalf("disarming = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}

	// With an encrypted observation inside the window, arming succeeds and sticks.
	srv.edgeScheme.Note(true, time.Now())
	if rec := settings(http.MethodPut, `{"cert_edge_require_https":true}`, true); rec.Code != http.StatusOK {
		t.Fatalf("arming with an observation = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	// The operator's own https session keeps working -- they are not locked out of
	// the switch they just armed.
	if !switchStored(true) {
		t.Fatal("a successful arming must store the switch")
	}

	// And the gate is live on the very next request without waiting out the switch
	// cache TTL: the PUT invalidates it. (This request is plaintext + external, so
	// it is exactly what the armed gate refuses.)
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, plainGateRequest(http.MethodPost, "/v1/chat/completions", "203.0.113.9:1"))
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "certificate.https_required") {
		t.Fatalf("after arming, a plaintext API POST = %d %s, want 403 certificate.https_required", rec.Code, rec.Body.String())
	}

	// Disarming likewise takes effect immediately -- the direction that matters
	// when an operator is trying to get back in.
	if rec := settings(http.MethodPut, `{"cert_edge_require_https":false}`, true); rec.Code != http.StatusOK {
		t.Fatalf("disarming after arming = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, plainGateRequest(http.MethodPost, "/v1/chat/completions", "203.0.113.9:1"))
	if rec.Code == http.StatusForbidden {
		t.Fatal("after disarming, the gate must stop refusing on the next request, not after the cache TTL")
	}
}

// TestEdgeRequireHTTPSSwitchIsCached pins the hot-path guard: the gate must not
// issue a system_settings store read per request. Without the cache this is a
// database round-trip on EVERY public request, inference included.
func TestEdgeRequireHTTPSSwitchIsCached(t *testing.T) {
	counter := &fakePortalEdgeSwitchCounter{}
	s := newGateServer(t, false, false)
	s.Portal = counter
	// An encrypted observation must exist, otherwise edgeGateVerdict short-circuits
	// on the fail-safe and never reaches the switch at all (see its doc comment on
	// evaluation order).
	s.edgeScheme.Note(true, time.Now())
	for i := 0; i < 50; i++ {
		s.ServeHTTP(httptest.NewRecorder(), plainGateRequest(http.MethodPost, "/v1/chat/completions", "203.0.113.9:1"))
	}
	if got := counter.count(); got > 2 {
		t.Fatalf("the switch was read %d times for 50 requests; it must be TTL-cached", got)
	}
	if counter.count() == 0 {
		t.Fatal("the switch was never read at all -- the gate is not consulting it")
	}
	// Invalidation forces exactly one more read.
	before := counter.count()
	s.invalidateEdgeRequireHTTPSCache()
	s.ServeHTTP(httptest.NewRecorder(), plainGateRequest(http.MethodPost, "/v1/chat/completions", "203.0.113.9:1"))
	if counter.count() != before+1 {
		t.Fatalf("after invalidation the switch was read %d times, want %d", counter.count(), before+1)
	}
}

type fakePortalEdgeSwitchCounter struct {
	portal.API
	mu sync.Mutex
	n  int
}

func (f *fakePortalEdgeSwitchCounter) CertEdgeRequireHTTPSChecked(context.Context) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.n++
	return false
}

func (f *fakePortalEdgeSwitchCounter) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.n
}

// fakePortalEdgeSwitchBlocking lets a test hold a switch read INSIDE the store
// call, which is the only way to interleave an invalidation with a read that has
// already started.
type fakePortalEdgeSwitchBlocking struct {
	portal.API
	entered chan struct{} // signalled once, when the first read is inside
	release chan struct{} // the first read returns when this is closed
	mu      sync.Mutex
	val     bool
	first   bool
}

func (f *fakePortalEdgeSwitchBlocking) CertEdgeRequireHTTPSChecked(context.Context) bool {
	f.mu.Lock()
	blocking := !f.first
	f.first = true
	v := f.val
	f.mu.Unlock()
	if blocking {
		close(f.entered)
		<-f.release
	}
	return v
}

func (f *fakePortalEdgeSwitchBlocking) set(v bool) {
	f.mu.Lock()
	f.val = v
	f.mu.Unlock()
}

// TestEdgeRequireHTTPSCacheDiscardsAReadRacedByAnInvalidation is the fix for the
// disarm race. The switch read happens OUTSIDE the cache mutex (holding it across
// a database round-trip would serialise every concurrent request), so a read that
// started BEFORE a disarming PUT can finish AFTER it and would otherwise write its
// now-stale `true` back with a fresh full TTL -- continuing to refuse requests for
// up to edgeSchemeSwitchTTL after a SUCCESSFUL disarm, which is exactly the window
// the operator is watching to see whether the disarm worked. The generation counter
// makes such a read discard its result instead.
func TestEdgeRequireHTTPSCacheDiscardsAReadRacedByAnInvalidation(t *testing.T) {
	fake := &fakePortalEdgeSwitchBlocking{
		entered: make(chan struct{}),
		release: make(chan struct{}),
		val:     true, // the pre-disarm value the in-flight read will return
	}
	s := newGateServer(t, false, false)
	s.Portal = fake

	done := make(chan bool, 1)
	go func() { done <- s.edgeRequireHTTPSOn(context.Background()) }()

	// The read is now inside the store call, having already captured the generation.
	<-fake.entered
	// The disarming PUT lands: it writes cert_edge_require_https=false and
	// invalidates the cache.
	fake.set(false)
	s.invalidateEdgeRequireHTTPSCache()
	close(fake.release)

	if got := <-done; !got {
		t.Fatalf("the in-flight read returned %v, want true -- it read the pre-disarm value", got)
	}

	// THE assertion: the next request must see the disarm, not the value the raced
	// read observed. Without the generation guard this reads the cached `true`.
	if s.edgeRequireHTTPSOn(context.Background()) {
		t.Fatal("the gate still reports armed after a successful disarm -- the raced read poisoned the cache")
	}
}

// TestEdgeGateRedirectTargetEdgeCases pins the redirect URL construction: an
// explicit :80 in the Host is dropped (the https form of the default http port is
// the default https port), any other port is left alone (the gateway cannot know
// the proxy's http->https port mapping), and a request with no Host at all falls
// back to the 403 rather than emitting a malformed "https:///path".
func TestEdgeGateRedirectTargetEdgeCases(t *testing.T) {
	s := newArmedGateServer(t)
	for _, c := range []struct{ host, wantLocation string }{
		{"example.com", "https://example.com/api/portal/me"},
		{"example.com:80", "https://example.com/api/portal/me"},
		{"example.com:8443", "https://example.com:8443/api/portal/me"},
	} {
		r := plainGateRequest(http.MethodGet, "/api/portal/me", "203.0.113.9:1")
		r.Host = c.host
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, r)
		if rec.Code != http.StatusTemporaryRedirect {
			t.Fatalf("host %q = %d, want 307", c.host, rec.Code)
		}
		if got := rec.Header().Get("Location"); got != c.wantLocation {
			t.Fatalf("host %q -> Location %q, want %q", c.host, got, c.wantLocation)
		}
	}
	r := plainGateRequest(http.MethodGet, "/api/portal/me", "203.0.113.9:1")
	r.Host = ""
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, r)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("hostless GET = %d, want 403 (no meaningful redirect target)", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "" {
		t.Fatalf("hostless GET set Location = %q, want none", loc)
	}
}

// TestEdgeGateIsConcurrencySafe drives the gate from many goroutines at once,
// which is how it actually runs: it sits on the shared request path, so its switch
// cache and the observation tracker are touched by every in-flight request
// simultaneously. Run under -race this is the guard for both mutexes.
func TestEdgeGateIsConcurrencySafe(t *testing.T) {
	s := newArmedGateServer(t)
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// A mix that exercises every branch: refusals, redirects, exclusions,
			// observation writes, and cache invalidation racing the reads.
			switch i % 5 {
			case 0:
				s.ServeHTTP(httptest.NewRecorder(), plainGateRequest(http.MethodPost, "/v1/chat/completions", "203.0.113.9:1"))
			case 1:
				s.ServeHTTP(httptest.NewRecorder(), plainGateRequest(http.MethodGet, "/api/portal/me", "203.0.113.9:1"))
			case 2:
				r := plainGateRequest(http.MethodGet, "/api/portal/me", "203.0.113.9:1")
				r.Header.Set(edgeSchemeHeaderName, "https")
				s.ServeHTTP(httptest.NewRecorder(), r)
			case 3:
				s.ServeHTTP(httptest.NewRecorder(), plainGateRequest(http.MethodPost, "/api/agent/v1/telemetry", "203.0.113.9:1"))
			case 4:
				s.invalidateEdgeRequireHTTPSCache()
			}
		}(i)
	}
	wg.Wait()
}

// TestEdgeGateRefusalWarnIsThrottled: the refusal Warn exists so a locked-out
// operator can SEE why they are being refused (that is the whole reason the verdict
// is applied at the dispatch point rather than returned early). One line per refused
// REQUEST defeats it: a retrying inference client emits thousands and evicts the
// 2000-entry log ring the operator is supposed to read. So the first refusal of any
// path is immediate and the rest are collapsed per path per interval.
func TestEdgeGateRefusalWarnIsThrottled(t *testing.T) {
	logs := logbuffer.NewBuffer(200, logbuffer.LevelTrace)
	setDefaultSlogForTest(t, logs)
	s := newArmedGateServer(t)

	const msg = "plaintext request refused by the edge https gate"
	count := func() int {
		n := 0
		for _, r := range logs.Snapshot() {
			if r.Msg == msg {
				n++
			}
		}
		return n
	}

	// A retrying client hammering ONE path must produce exactly one line...
	for i := 0; i < 40; i++ {
		s.ServeHTTP(httptest.NewRecorder(), plainGateRequest(http.MethodPost, "/v1/chat/completions", "203.0.113.9:1"))
	}
	if got := count(); got != 1 {
		t.Fatalf("40 refusals of one path logged %d lines, want exactly 1 (the ring must survive a retry storm)", got)
	}
	// ...and the retry storm must not be able to grow the throttle map either.
	s.edgeWarn.mu.Lock()
	entries := len(s.edgeWarn.at)
	s.edgeWarn.mu.Unlock()
	if entries != 1 {
		t.Fatalf("throttle map holds %d entries after 40 refusals of one path, want 1", entries)
	}

	// A DIFFERENT path is a different fact -- its first refusal is immediate.
	s.ServeHTTP(httptest.NewRecorder(), plainGateRequest(http.MethodGet, "/api/portal/me", "203.0.113.9:1"))
	if got := count(); got != 2 {
		t.Fatalf("a first refusal on a second path logged %d lines total, want 2 (per-path, not global)", got)
	}

	// Once the interval elapses the same path logs again -- the operator must be able
	// to tell an ONGOING lockout from a single past refusal.
	s.edgeWarn.mu.Lock()
	for p := range s.edgeWarn.at {
		s.edgeWarn.at[p] = time.Now().Add(-edgeGateWarnInterval - time.Second)
	}
	s.edgeWarn.mu.Unlock()
	s.ServeHTTP(httptest.NewRecorder(), plainGateRequest(http.MethodPost, "/v1/chat/completions", "203.0.113.9:1"))
	if got := count(); got != 3 {
		t.Fatalf("after the interval elapsed the path logged %d lines total, want 3", got)
	}
	// The prune ran on that admitted line: the stale sibling path is gone.
	s.edgeWarn.mu.Lock()
	entries = len(s.edgeWarn.at)
	s.edgeWarn.mu.Unlock()
	if entries != 1 {
		t.Fatalf("throttle map holds %d entries, want 1 (stale paths must be pruned)", entries)
	}

	// And the throttle must NEVER change the client-visible answer.
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, plainGateRequest(http.MethodPost, "/v1/chat/completions", "203.0.113.9:1"))
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "certificate.https_required") {
		t.Fatalf("a throttled refusal = %d %s, want 403 certificate.https_required", rec.Code, rec.Body.String())
	}
}
