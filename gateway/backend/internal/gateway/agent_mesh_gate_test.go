// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/store"
	"sync"
	"testing"
	"time"
)

// fakePortalMeshRequireTLS reports the P3 mesh plaintext switch and nothing else
// (same nil-embedded-interface trick as fakePortalEdgeRequireHTTPS).
type fakePortalMeshRequireTLS struct {
	portal.API
	on bool
}

func (f fakePortalMeshRequireTLS) CertMeshRequireTLSChecked(context.Context) bool { return f.on }

// NetbirdOnly is off here: agentSourceRefused (agent_netbird_gate.go) reads it
// on every agent-listener request now, so a fake driven through AgentHandler
// must answer it -- this fake's tests are about the TLS-required gate, not the
// source gate, so "off" keeps the source gate a no-op.
func (f fakePortalMeshRequireTLS) NetbirdOnly(context.Context) bool { return false }

// newMeshGateServer builds the smallest *Server the mesh gate reads, both muxes
// wired to a catch-all that writes "served" so a test can tell "let through"
// (200 served) from "refused" (403, never reaching the handler) and can drive
// the two REAL listener entry points (ServeHTTP public, AgentHandler mesh).
func newMeshGateServer(t *testing.T, switchOn, killSwitch bool) *Server {
	t.Helper()
	served := func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("served"))
	}
	s := &Server{
		Portal:                    fakePortalMeshRequireTLS{on: switchOn},
		AgentTransport:            NewAgentTransportRegistry(),
		certMeshRequireTLSDisable: killSwitch,
		mux:                       http.NewServeMux(),
		agentMux:                  http.NewServeMux(),
	}
	s.mux.HandleFunc("/", served)
	s.agentMux.HandleFunc("/", served)
	return s
}

func plainMeshRequest(method, path string) *http.Request {
	r := httptest.NewRequest(method, path, nil)
	r.RemoteAddr = "100.64.0.9:5555"
	return r
}

func tlsMeshRequest(method, path string) *http.Request {
	r := plainMeshRequest(method, path)
	r.TLS = &tls.ConnectionState{} // a non-nil TLS state is the encryption truth
	return r
}

func meshBodyCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return body.Error.Code
}

// TestMeshGateRejectsPlainAgentPathButLeavesHealthOpen: an armed gate 403s a
// plaintext /api/agent/v1/* request on the mesh listener but always serves
// /healthz (the orchestrator probe).
func TestMeshGateRejectsPlainAgentPathButLeavesHealthOpen(t *testing.T) {
	s := newMeshGateServer(t, true, false)

	rec := httptest.NewRecorder()
	s.AgentHandler().ServeHTTP(rec, plainMeshRequest(http.MethodPost, "/api/agent/v1/telemetry"))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("plain agent path status = %d, want 403", rec.Code)
	}
	if code := meshBodyCode(t, rec); code != "certificate.mesh_tls_required" {
		t.Fatalf("refusal code = %q, want certificate.mesh_tls_required", code)
	}

	health := httptest.NewRecorder()
	s.AgentHandler().ServeHTTP(health, plainMeshRequest(http.MethodGet, "/healthz"))
	if health.Code != http.StatusOK || health.Body.String() != "served" {
		t.Fatalf("/healthz status=%d body=%q, want 200 served even when armed", health.Code, health.Body.String())
	}
}

// TestMeshGateAllowsTLSAndNeverTouchesPublicMux: a TLS agent request is served,
// and the same plaintext agent path on the PUBLIC listener is never mesh-refused
// (that hop is governed by netbird_only + the edge gate, not this one).
func TestMeshGateAllowsTLSAndNeverTouchesPublicMux(t *testing.T) {
	s := newMeshGateServer(t, true, false)

	tlsRec := httptest.NewRecorder()
	s.AgentHandler().ServeHTTP(tlsRec, tlsMeshRequest(http.MethodPost, "/api/agent/v1/telemetry"))
	if tlsRec.Code != http.StatusOK || tlsRec.Body.String() != "served" {
		t.Fatalf("TLS agent request status=%d body=%q, want 200 served", tlsRec.Code, tlsRec.Body.String())
	}

	pubRec := httptest.NewRecorder()
	s.ServeHTTP(pubRec, plainMeshRequest(http.MethodPost, "/api/agent/v1/telemetry"))
	if pubRec.Code == http.StatusForbidden && meshBodyCode(t, pubRec) == "certificate.mesh_tls_required" {
		t.Fatal("mesh gate refused a request on the PUBLIC mux; it must guard only the agent listener")
	}
}

// TestMeshGateKillSwitchOverridesStoredSetting: the env kill switch disengages the
// gate even with the stored switch on.
func TestMeshGateKillSwitchOverridesStoredSetting(t *testing.T) {
	s := newMeshGateServer(t, true, true) // switch on, kill switch on
	rec := httptest.NewRecorder()
	s.AgentHandler().ServeHTTP(rec, plainMeshRequest(http.MethodPost, "/api/agent/v1/telemetry"))
	if rec.Code != http.StatusOK || rec.Body.String() != "served" {
		t.Fatalf("kill switch did not disengage the gate: status=%d body=%q", rec.Code, rec.Body.String())
	}
}

// TestMeshGateIsANoOpWithTheSwitchOff: with the stored switch off, plaintext is served.
func TestMeshGateIsANoOpWithTheSwitchOff(t *testing.T) {
	s := newMeshGateServer(t, false, false)
	rec := httptest.NewRecorder()
	s.AgentHandler().ServeHTTP(rec, plainMeshRequest(http.MethodPost, "/api/agent/v1/telemetry"))
	if rec.Code != http.StatusOK || rec.Body.String() != "served" {
		t.Fatalf("gate refused with the switch off: status=%d body=%q", rec.Code, rec.Body.String())
	}
}

// TestMeshGateStaysStrictWithoutRecentTLSObservation is the load-bearing
// difference from Plan B: enforcement is NOT coupled to a fresh-observation
// window. An armed gate refuses plaintext even when no TLS hop has been observed
// for hours -- the only way back is the operator (disarm or kill switch).
func TestMeshGateStaysStrictWithoutRecentTLSObservation(t *testing.T) {
	s := newMeshGateServer(t, true, false)
	// No TLS observation recorded at all (registry empty). Plan B's edge gate would
	// fall open here; the mesh gate must stay strict.
	rec := httptest.NewRecorder()
	s.AgentHandler().ServeHTTP(rec, plainMeshRequest(http.MethodPost, "/api/agent/v1/stream"))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("armed mesh gate fell open without a recent observation: status=%d, want 403", rec.Code)
	}
}

// TestArmMeshRequireTLSNeedsFreshTLSObservation: arming is refused unless some
// server was observed on TLS within the arming window, and a bare &Server{} (no
// registry) fails safe.
func TestArmMeshRequireTLSNeedsFreshTLSObservation(t *testing.T) {
	s := newMeshGateServer(t, false, false)
	if err := s.ArmMeshRequireTLS(); !errors.Is(err, errMeshTLSNotObserved) {
		t.Fatalf("arm with no observation = %v, want errMeshTLSNotObserved", err)
	}

	s.AgentTransport.Report("srv-1", true) // a fresh TLS observation
	if err := s.ArmMeshRequireTLS(); err != nil {
		t.Fatalf("arm after a fresh TLS observation = %v, want nil", err)
	}

	if err := (&Server{}).ArmMeshRequireTLS(); !errors.Is(err, errMeshTLSNotObserved) {
		t.Fatalf("bare &Server{} arm = %v, want errMeshTLSNotObserved (fail safe)", err)
	}
}

// TestMeshGateDisableInvalidatesCacheImmediately: turning the switch off and
// invalidating the cache lets the very next request through without waiting out
// the TTL.
func TestMeshGateDisableInvalidatesCacheImmediately(t *testing.T) {
	fake := &togglePortalMeshRequireTLS{on: true}
	s := newMeshGateServer(t, false, false)
	s.Portal = fake

	// Prime the cache as ON.
	rec := httptest.NewRecorder()
	s.AgentHandler().ServeHTTP(rec, plainMeshRequest(http.MethodPost, "/api/agent/v1/telemetry"))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("precondition: armed gate should refuse, got %d", rec.Code)
	}

	fake.set(false)
	s.invalidateMeshRequireTLSCache()

	rec2 := httptest.NewRecorder()
	s.AgentHandler().ServeHTTP(rec2, plainMeshRequest(http.MethodPost, "/api/agent/v1/telemetry"))
	if rec2.Code != http.StatusOK {
		t.Fatalf("after disable+invalidate, request status=%d, want 200 (no TTL lag)", rec2.Code)
	}
}

type togglePortalMeshRequireTLS struct {
	portal.API
	mu sync.Mutex
	on bool
}

func (f *togglePortalMeshRequireTLS) set(on bool) {
	f.mu.Lock()
	f.on = on
	f.mu.Unlock()
}

func (f *togglePortalMeshRequireTLS) CertMeshRequireTLSChecked(context.Context) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.on
}

// NetbirdOnly is off here for the same reason as fakePortalMeshRequireTLS's:
// agentSourceRefused reads it on every agent-listener request.
func (f *togglePortalMeshRequireTLS) NetbirdOnly(context.Context) bool { return false }

// meshArmServer builds a *Server with a real portal.Service (memory) + a
// system-scope token + an AgentTransport registry, so the settings-handler arm
// precondition for cert_mesh_require_tls can be exercised end-to-end.
func meshArmServer(t *testing.T) (*Server, *portal.Service) {
	t.Helper()
	now := time.Date(2026, time.August, 19, 12, 0, 0, 0, time.UTC)
	tokens := auth.NewTokenStore()
	dir := portal.NewMemoryDirectory(tokens)
	dir.AddUser(store.User{ID: "usr_s", Email: "s@example.test", DisplayName: "System Admin", Role: "system_admin", Status: store.UserStatusActive, PreferredLanguage: "de", CreatedAt: now, UpdatedAt: now})
	if err := dir.CreatePlainToken(context.Background(), store.TokenRecord{ID: "tok_s", UserID: "usr_s", Name: "System", Status: store.TokenStatusActive, Scopes: `["gateway:use","admin","system"]`, CreatedAt: now, UpdatedAt: now}, meshArmSystemSecret); err != nil {
		t.Fatalf("seed system token: %v", err)
	}
	svc := portal.NewService(portal.ServiceDeps{
		Users: dir, Tokens: dir, Routes: routing.NewMemoryStore(), SystemSettings: portal.NewMemorySystemSettings(),
		SettingsVolatile: true, Clock: func() time.Time { return now },
	})
	srv := New(ServerDeps{Tokens: tokens, Routes: routing.NewMemoryStore(), Portal: svc, AgentTransport: NewAgentTransportRegistry()})
	return srv, svc
}

const meshArmSystemSecret = "mesh-arm-system-secret"

func meshArmPut(t *testing.T, s *Server, require bool) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"cert_mesh_require_tls": require})
	req := httptest.NewRequest(http.MethodPut, "/api/system/settings", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+meshArmSystemSecret)
	req.Header.Set("X-OP-CSRF", "1")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	return rec
}

// TestMeshRequireTLSFirstArmNeedsObservation: the FIRST arm (stored false -> true)
// with no fresh TLS observation is refused 400.
func TestMeshRequireTLSFirstArmNeedsObservation(t *testing.T) {
	srv, _ := meshArmServer(t)
	rec := meshArmPut(t, srv, true)
	if rec.Code != http.StatusBadRequest || meshBodyCode(t, rec) != "certificate.mesh_tls_not_observed" {
		t.Fatalf("first arm without observation = %d body=%s, want 400 mesh_tls_not_observed", rec.Code, rec.Body.String())
	}
}

// TestMeshRequireTLSReArmIsIdempotent: a PUT(true) when the switch is ALREADY
// stored true must be a no-op success (200), NOT re-run the observation
// precondition -- otherwise an idempotent re-set 400s once the observation lapses.
func TestMeshRequireTLSReArmIsIdempotent(t *testing.T) {
	srv, svc := meshArmServer(t)
	// Arm it directly in the store (the service write has no arm gate; that lives in
	// the HTTP handler), simulating an already-armed gateway whose in-memory
	// observation has since lapsed (the registry is empty).
	on := true
	if _, err := svc.UpdateSystemSettings(context.Background(), auth.Token{Scopes: []string{"system"}}, portal.UpdateSystemSettingsRequest{CertMeshRequireTLS: &on}); err != nil {
		t.Fatalf("seed stored cert_mesh_require_tls=true: %v", err)
	}
	rec := meshArmPut(t, srv, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("re-arm when already armed = %d body=%s, want 200 (idempotent no-op)", rec.Code, rec.Body.String())
	}
}
