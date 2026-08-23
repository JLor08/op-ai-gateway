// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/portal"
	"strconv"
	"testing"
	"time"
)

// fakePortalSourceCheck reports netbird_only + the resolved gateway mesh peer
// IP and nothing else (same nil-embedded-interface trick as
// fakePortalMeshRequireTLS in agent_mesh_gate_test.go).
type fakePortalSourceCheck struct {
	portal.API
	netbirdOnly bool
	peerIP      string
	peerErr     error
	calls       *int
}

func (f fakePortalSourceCheck) NetbirdOnly(context.Context) bool { return f.netbirdOnly }

func (f fakePortalSourceCheck) ResolveGatewayPeerIP(context.Context) (string, error) {
	if f.calls != nil {
		*f.calls++
	}
	return f.peerIP, f.peerErr
}

// newSourceCheckServer builds the smallest *Server the agent source check
// reads, both muxes wired to a catch-all that writes "served" so a test can
// tell "let through" (200 served) from "refused" (403, never reaching the
// handler).
func newSourceCheckServer(portalAPI portal.API) *Server {
	served := func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("served"))
	}
	s := &Server{
		Portal:   portalAPI,
		mux:      http.NewServeMux(),
		agentMux: http.NewServeMux(),
	}
	s.mux.HandleFunc("/", served)
	s.agentMux.HandleFunc("/", served)
	return s
}

// agentRequestFrom builds an agent-telemetry request whose connection's LOCAL
// address (http.LocalAddrContextKey, as net/http sets it per connection) is
// localAddr -- the signal agentSourceRefused uses to tell a mesh-bound
// listener (local addr == mesh peer IP) from a host-published one.
func agentRequestFrom(t *testing.T, localAddr string) *http.Request {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/api/agent/v1/telemetry", nil)
	host, portStr, err := net.SplitHostPort(localAddr)
	if err != nil {
		t.Fatalf("split %q: %v", localAddr, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("parse port %q: %v", portStr, err)
	}
	ctx := context.WithValue(r.Context(), http.LocalAddrContextKey, &net.TCPAddr{IP: net.ParseIP(host), Port: port})
	return r.WithContext(ctx)
}

func sourceCheckBodyCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return body.Error.Code
}

// TestAgentSourceCheckRefusesNonMeshWhenNetbirdOnly is the brief's core case:
// with netbird_only on, a request whose LOCAL address equals the gateway's
// mesh peer IP is a mesh-bound listener (allowed, no-op); a request whose
// local address differs is a host-published bind (refused 403 netbird.only).
// With netbird_only off, neither is checked at all.
func TestAgentSourceCheckRefusesNonMeshWhenNetbirdOnly(t *testing.T) {
	const meshPeerIP = "100.64.0.2"

	t.Run("mesh-bound local addr is allowed", func(t *testing.T) {
		s := newSourceCheckServer(fakePortalSourceCheck{netbirdOnly: true, peerIP: meshPeerIP})
		rec := httptest.NewRecorder()
		s.AgentHandler().ServeHTTP(rec, agentRequestFrom(t, meshPeerIP+":8443"))
		if rec.Code != http.StatusOK || rec.Body.String() != "served" {
			t.Fatalf("mesh-bound request status=%d body=%q, want 200 served", rec.Code, rec.Body.String())
		}
	})

	t.Run("host-published local addr is refused", func(t *testing.T) {
		s := newSourceCheckServer(fakePortalSourceCheck{netbirdOnly: true, peerIP: meshPeerIP})
		rec := httptest.NewRecorder()
		s.AgentHandler().ServeHTTP(rec, agentRequestFrom(t, "10.0.0.9:8443"))
		if rec.Code != http.StatusForbidden {
			t.Fatalf("host-published request status=%d, want 403", rec.Code)
		}
		if code := sourceCheckBodyCode(t, rec); code != "netbird.only" {
			t.Fatalf("refusal code = %q, want netbird.only", code)
		}
	})

	t.Run("netbird_only off allows both", func(t *testing.T) {
		s := newSourceCheckServer(fakePortalSourceCheck{netbirdOnly: false, peerIP: meshPeerIP})

		meshRec := httptest.NewRecorder()
		s.AgentHandler().ServeHTTP(meshRec, agentRequestFrom(t, meshPeerIP+":8443"))
		if meshRec.Code != http.StatusOK {
			t.Fatalf("netbird_only off, mesh addr: status=%d, want 200", meshRec.Code)
		}

		hostRec := httptest.NewRecorder()
		s.AgentHandler().ServeHTTP(hostRec, agentRequestFrom(t, "10.0.0.9:8443"))
		if hostRec.Code != http.StatusOK {
			t.Fatalf("netbird_only off, host-published addr: status=%d, want 200", hostRec.Code)
		}
	})
}

// TestAgentSourceCheckFailsOpenWhenMeshIPUnresolvable is the deliberate
// fail-open case: netbird_only is on but the mesh peer IP cannot be resolved
// (a control-plane/NetBird blip), so the request is allowed rather than
// cutting agents off.
func TestAgentSourceCheckFailsOpenWhenMeshIPUnresolvable(t *testing.T) {
	s := newSourceCheckServer(fakePortalSourceCheck{netbirdOnly: true, peerErr: errors.New("netbird api unreachable")})
	rec := httptest.NewRecorder()
	s.AgentHandler().ServeHTTP(rec, agentRequestFrom(t, "10.0.0.9:8443"))
	if rec.Code != http.StatusOK || rec.Body.String() != "served" {
		t.Fatalf("unresolvable mesh IP should fail OPEN: status=%d body=%q, want 200 served", rec.Code, rec.Body.String())
	}
}

// TestAgentSourceCheckFailsOpenOnEmptyPeerIP covers ResolveGatewayPeerIP's
// documented ("", nil) result (no gateway peer selected): also fail-open.
func TestAgentSourceCheckFailsOpenOnEmptyPeerIP(t *testing.T) {
	s := newSourceCheckServer(fakePortalSourceCheck{netbirdOnly: true, peerIP: ""})
	rec := httptest.NewRecorder()
	s.AgentHandler().ServeHTTP(rec, agentRequestFrom(t, "10.0.0.9:8443"))
	if rec.Code != http.StatusOK || rec.Body.String() != "served" {
		t.Fatalf("empty mesh IP should fail OPEN: status=%d body=%q, want 200 served", rec.Code, rec.Body.String())
	}
}

// TestAgentSourceCheckFailsOpenWithoutLocalAddr: a request whose context
// carries no http.LocalAddrContextKey is allowed rather than refused --
// agentSourceRefused must never manufacture a refusal from an absent signal.
func TestAgentSourceCheckFailsOpenWithoutLocalAddr(t *testing.T) {
	s := newSourceCheckServer(fakePortalSourceCheck{netbirdOnly: true, peerIP: "100.64.0.2"})
	r := httptest.NewRequest(http.MethodPost, "/api/agent/v1/telemetry", nil)
	rec := httptest.NewRecorder()
	s.AgentHandler().ServeHTTP(rec, r)
	if rec.Code != http.StatusOK || rec.Body.String() != "served" {
		t.Fatalf("missing LocalAddr should fail OPEN: status=%d body=%q, want 200 served", rec.Code, rec.Body.String())
	}
}

// TestAgentSourceCheckIsNilPortalSafe: a bare &Server{} (no Portal, as in a
// test that never wires one) never refuses -- mirrors meshGateRefuses' and the
// public-mux netbird_only gates' nil-safety.
func TestAgentSourceCheckIsNilPortalSafe(t *testing.T) {
	s := &Server{mux: http.NewServeMux(), agentMux: http.NewServeMux()}
	s.agentMux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	rec := httptest.NewRecorder()
	s.AgentHandler().ServeHTTP(rec, agentRequestFrom(t, "10.0.0.9:8443"))
	if rec.Code != http.StatusOK {
		t.Fatalf("nil Portal should fail OPEN: status=%d, want 200", rec.Code)
	}
}

// fakePortalCtxAwareSource resolves the mesh peer IP unless the context passed
// to ResolveGatewayPeerIP is already cancelled/expired -- in which case it
// returns that ctx error, mimicking netbird.GetPeer failing on a cancelled
// request context. It is the fake the security regression needs: it lets the
// test prove agentSourceRefused resolves on a DETACHED context (client
// cancellation cannot reach the resolve) rather than on the request context.
type fakePortalCtxAwareSource struct {
	portal.API
	netbirdOnly bool
	peerIP      string
	sawCancel   *bool
}

func (f fakePortalCtxAwareSource) NetbirdOnly(context.Context) bool { return f.netbirdOnly }

func (f fakePortalCtxAwareSource) ResolveGatewayPeerIP(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		if f.sawCancel != nil {
			*f.sawCancel = true
		}
		return "", err // a cancelled/expired ctx surfaces as a resolve error
	}
	return f.peerIP, nil
}

// TestAgentSourceCheckIgnoresRequestCancellation is the CRITICAL security
// regression: a client on a host-published agent port must not be able to
// poison the mesh-IP cache by cancelling its own connection mid-resolve. With
// netbird_only on, a resolvable mesh IP, and a NON-mesh local addr, the request
// must still be refused (403) even when the REQUEST context is already
// cancelled -- because agentSourceRefused resolves on a detached context, so the
// cancellation never turns the resolve into a "" that flips the source gate OPEN
// (and gets cached for the TTL, refusing nobody for the whole window).
func TestAgentSourceCheckIgnoresRequestCancellation(t *testing.T) {
	const meshPeerIP = "100.64.0.2"
	sawCancel := false
	s := newSourceCheckServer(fakePortalCtxAwareSource{netbirdOnly: true, peerIP: meshPeerIP, sawCancel: &sawCancel})

	// A host-published (non-mesh) request whose connection the client already
	// cancelled -- the exact cache-poison move the fix defends against.
	r := agentRequestFrom(t, "10.0.0.9:8443")
	ctx, cancel := context.WithCancel(r.Context())
	cancel()
	r = r.WithContext(ctx)

	rec := httptest.NewRecorder()
	s.AgentHandler().ServeHTTP(rec, r)

	if sawCancel {
		t.Fatal("ResolveGatewayPeerIP saw a cancelled context -> the resolve was NOT detached from the request context")
	}
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cancelled request ctx flipped the source gate OPEN: status=%d, want 403 (still refused)", rec.Code)
	}
	if code := sourceCheckBodyCode(t, rec); code != "netbird.only" {
		t.Fatalf("refusal code = %q, want netbird.only", code)
	}
}

// TestAgentSourceCheckErrorMissSelfHeals verifies a transient resolve error is
// NOT cached for the full legitimate-empty TTL: the error-derived "" is pinned
// only for the short agentSourcePeerIPErrTTL, and once that window lapses the
// next request re-resolves rather than reusing the miss for the whole
// agentSourcePeerIPTTL. A genuinely-empty ("", nil) result still gets the full
// TTL (TestAgentSourceCheckCachesResolvedPeerIP covers the success/full-TTL side).
func TestAgentSourceCheckErrorMissSelfHeals(t *testing.T) {
	const meshPeerIP = "100.64.0.2"
	calls := 0
	fail := true
	s := newSourceCheckServer(fakePortalErrThenOK{netbirdOnly: true, peerIP: meshPeerIP, calls: &calls, fail: &fail})

	// First request: resolve errors -> fail open (200 served).
	rec1 := httptest.NewRecorder()
	s.AgentHandler().ServeHTTP(rec1, agentRequestFrom(t, "10.0.0.9:8443"))
	if rec1.Code != http.StatusOK {
		t.Fatalf("transient resolve error should fail OPEN: status=%d, want 200", rec1.Code)
	}

	// The error miss must be pinned only for the short error TTL, never the full
	// legitimate-empty TTL -- pre-fix it was cached for the full agentSourcePeerIPTTL.
	s.sourcePeerIP.mu.Lock()
	ttlLeft := time.Until(s.sourcePeerIP.exp)
	s.sourcePeerIP.mu.Unlock()
	if ttlLeft > agentSourcePeerIPErrTTL+50*time.Millisecond {
		t.Fatalf("error miss cached for ~%v; want <= the short error TTL %v (not the full %v)", ttlLeft, agentSourcePeerIPErrTTL, agentSourcePeerIPTTL)
	}

	// Once the short window lapses, the next request re-resolves (now OK) and
	// refuses. Expire the short miss deterministically instead of sleeping.
	fail = false
	s.sourcePeerIP.mu.Lock()
	s.sourcePeerIP.exp = time.Now().Add(-time.Millisecond)
	s.sourcePeerIP.mu.Unlock()

	rec2 := httptest.NewRecorder()
	s.AgentHandler().ServeHTTP(rec2, agentRequestFrom(t, "10.0.0.9:8443"))
	if rec2.Code != http.StatusForbidden {
		t.Fatalf("error-derived miss must self-heal (re-resolve) after the short TTL: status=%d, want 403", rec2.Code)
	}
	if calls < 2 {
		t.Fatalf("ResolveGatewayPeerIP called %d times; the error miss did not re-resolve", calls)
	}
}

type fakePortalErrThenOK struct {
	portal.API
	netbirdOnly bool
	peerIP      string
	calls       *int
	fail        *bool
}

func (f fakePortalErrThenOK) NetbirdOnly(context.Context) bool { return f.netbirdOnly }

func (f fakePortalErrThenOK) ResolveGatewayPeerIP(context.Context) (string, error) {
	if f.calls != nil {
		*f.calls++
	}
	if f.fail != nil && *f.fail {
		return "", errors.New("netbird api blip")
	}
	return f.peerIP, nil
}

// TestAgentSourceCheckCachesResolvedPeerIP verifies the TTL cache: repeated
// requests within agentSourcePeerIPTTL reuse the first ResolveGatewayPeerIP
// result rather than calling it again, so the hot agent-telemetry/stream path
// does not round-trip to NetBird's management API on every request.
func TestAgentSourceCheckCachesResolvedPeerIP(t *testing.T) {
	calls := 0
	s := newSourceCheckServer(fakePortalSourceCheck{netbirdOnly: true, peerIP: "100.64.0.2", calls: &calls})

	for i := 0; i < 3; i++ {
		s.AgentHandler().ServeHTTP(httptest.NewRecorder(), agentRequestFrom(t, "100.64.0.2:8443"))
	}
	if calls != 1 {
		t.Fatalf("ResolveGatewayPeerIP called %d times for 3 requests within the TTL, want 1 (cached)", calls)
	}
}
