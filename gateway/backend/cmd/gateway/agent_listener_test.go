// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package main

import (
	"bufio"
	"context"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/config"
	"op-ai-gateway/internal/gateway"
	"op-ai-gateway/internal/netbird"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/routing"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

// buildAgentListenerServer builds a *gateway.Server whose Portal is backed by an
// in-memory (volatile) system-settings store seeded with the given values, so
// startAgentListener can resolve netbird_gateway_peer_id → GetPeer and read the
// NetBird module config exactly as production does.
func buildAgentListenerServer(t *testing.T, settings map[string]string) *gateway.Server {
	t.Helper()
	mss := portal.NewMemorySystemSettings()
	now := time.Now().UTC()
	for k, v := range settings {
		if err := mss.SetSystemSetting(context.Background(), k, v, now); err != nil {
			t.Fatalf("set system setting %s: %v", k, err)
		}
	}
	// SettingsVolatile lets openSecret read the "plain:" token the tests store
	// (no cipher on the memory path), matching the volatile in-memory store.
	portalSvc := portal.NewService(portal.ServiceDeps{SystemSettings: mss, SettingsVolatile: true})
	return gateway.New(gateway.ServerDeps{Portal: portalSvc})
}

func buildAgentListenerTLSServer(t *testing.T) (*gateway.Server, routing.Store) {
	t.Helper()
	ctx := context.Background()
	settings := portal.NewMemorySystemSettings()
	routes := routing.NewMemoryStore()
	portalSvc := portal.NewService(portal.ServiceDeps{
		Routes:           routes,
		SystemSettings:   settings,
		SettingsVolatile: true,
	})
	on := true
	mode := portal.IssuerModeSelfSigned
	base := "int.example.test"
	domain := "gateway.int.example.test"
	if _, err := portalSvc.UpdateSystemSettings(ctx, auth.Token{Scopes: []string{"system"}}, portal.UpdateSystemSettingsRequest{
		CertEnabled:       &on,
		CertIssuerMode:    &mode,
		CertBaseDomain:    &base,
		CertGatewayDomain: &domain,
	}); err != nil {
		t.Fatalf("enable certificate module: %v", err)
	}
	return gateway.New(gateway.ServerDeps{Portal: portalSvc, Routes: routes}), routes
}

func storeGatewayListenerMaterial(t *testing.T, routes routing.Store, serial int64) (string, [32]byte, time.Time) {
	t.Helper()
	const domain = "gateway.int.example.test"
	fullchain, keyPEM, fingerprint := newSniffTestCertificate(t, serial)
	block, _ := pem.Decode([]byte(fullchain))
	if block == nil {
		t.Fatal("decode generated gateway certificate")
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse generated gateway certificate: %v", err)
	}
	now := time.Now().UTC()
	if err := routes.UpsertCertificate(context.Background(), routing.Certificate{
		Domain:            domain,
		Kind:              "gateway",
		FullchainPEM:      fullchain,
		KeySealed:         "plain:" + keyPEM,
		Fingerprint:       hex.EncodeToString(fingerprint[:]),
		IssuerFingerprint: "test-issuer",
		NotBefore:         leaf.NotBefore,
		NotAfter:          leaf.NotAfter,
		IssuedAt:          now,
		Status:            "active",
		CreatedAt:         now,
		UpdatedAt:         now,
	}); err != nil {
		t.Fatalf("store gateway listener material: %v", err)
	}
	return fullchain, fingerprint, leaf.NotAfter
}

func seedAgentListenerToken(t *testing.T, routes routing.Store, serverID, secret string) {
	t.Helper()
	now := time.Now().UTC()
	if err := routes.CreateAIServer(context.Background(), routing.AIServer{
		ID: serverID, Name: "Agent listener test server", Domain: serverID + ".example.test",
		Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("store server: %v", err)
	}
	if err := routes.UpsertAgentToken(context.Background(), routing.AgentToken{
		ID: "token-" + serverID, ServerID: serverID, SecretPrefix: "opaigw_",
		CreatedAt: now, UpdatedAt: now,
	}, auth.HashSecret(secret)); err != nil {
		t.Fatalf("store agent token: %v", err)
	}
}

type signalFailListener struct {
	addr       net.Addr
	failErr    error
	fail       chan struct{}
	accepting  chan struct{}
	returned   chan struct{}
	closed     chan struct{}
	acceptOnce sync.Once
	returnOnce sync.Once
	closeOnce  sync.Once
}

func newSignalFailListener(t *testing.T, address string, failErr error) *signalFailListener {
	t.Helper()
	addr, err := net.ResolveTCPAddr("tcp", address)
	if err != nil {
		t.Fatalf("resolve controlled listener address: %v", err)
	}
	return &signalFailListener{
		addr: addr, failErr: failErr, fail: make(chan struct{}), accepting: make(chan struct{}),
		returned: make(chan struct{}), closed: make(chan struct{}),
	}
}

func (l *signalFailListener) Accept() (net.Conn, error) {
	l.acceptOnce.Do(func() { close(l.accepting) })
	select {
	case <-l.fail:
		l.returnOnce.Do(func() { close(l.returned) })
		return nil, l.failErr
	case <-l.closed:
		l.returnOnce.Do(func() { close(l.returned) })
		return nil, net.ErrClosed
	}
}

func (l *signalFailListener) Close() error {
	l.closeOnce.Do(func() { close(l.closed) })
	return nil
}

func (l *signalFailListener) Addr() net.Addr { return l.addr }

func requestOnOpenPlainConnection(t *testing.T, conn net.Conn, reader *bufio.Reader) {
	t.Helper()
	if _, err := io.WriteString(conn, "GET /healthz HTTP/1.1\r\nHost: localhost\r\n\r\n"); err != nil {
		t.Fatalf("write request on open connection: %v", err)
	}
	resp, err := http.ReadResponse(reader, &http.Request{Method: http.MethodGet})
	if err != nil {
		t.Fatalf("read response on open connection: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("health response status = %d, want 200", resp.StatusCode)
	}
}

// freeTCPPort reserves a free 127.0.0.1 port, releases it, and returns the port
// string so startAgentListener can bind it. There is an inherent TOCTOU window
// (another process could grab it before the bind), acceptable in the test env.
func freeTCPPort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve free port: %v", err)
	}
	_, port, err := net.SplitHostPort(ln.Addr().String())
	_ = ln.Close()
	if err != nil {
		t.Fatalf("split reserved addr: %v", err)
	}
	return port
}

// freeLoopbackAddr reserves a free 127.0.0.1 port, releases it, and returns the
// full host:port so the agentListenerManager can bind it. Same inherent TOCTOU
// window as freeTCPPort — acceptable in the test env.
func freeLoopbackAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

// TestAgentManagerHoldsPlainAndTLSBinds asserts the dual-bind structural split:
// the manager owns a primary `plain` listenerBind plus an optional dedicated `tls`
// bind pointer. Combined mode — the only wiring this task performs — drives ONLY
// the plain bind (role bindRolePlainCombined) and leaves m.tls nil, proving today's
// single-socket behavior is preserved unchanged.
func TestAgentManagerHoldsPlainAndTLSBinds(t *testing.T) {
	srv := gateway.New(gateway.ServerDeps{})
	mgr := &agentListenerManager{baseCtx: context.Background()}
	t.Cleanup(func() { _ = mgr.ensure(context.Background(), srv, "") })
	addr := freeLoopbackAddr(t)
	if err := mgr.ensure(context.Background(), srv, addr); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if mgr.tls != nil {
		t.Fatalf("combined mode populated the dedicated TLS bind: %+v", mgr.tls)
	}
	if mgr.plain.addr != addr {
		t.Fatalf("plain bind addr = %q, want %q", mgr.plain.addr, addr)
	}
	if mgr.plain.role != bindRolePlainCombined {
		t.Fatalf("plain bind role = %v, want bindRolePlainCombined", mgr.plain.role)
	}
	if !srv.AgentListenerActive() || srv.AgentListenerAddr() != addr {
		t.Fatalf("combined bind not observable: active=%v addr=%q", srv.AgentListenerActive(), srv.AgentListenerAddr())
	}
}

// TestAgentListenerManagerRebindSwaps proves ensure() rebinds to a new addr and
// frees the old one (a live agent-listener rebind when the gateway peer IP changes).
func TestAgentListenerManagerRebindSwaps(t *testing.T) {
	srv := gateway.New(gateway.ServerDeps{})
	mgr := &agentListenerManager{baseCtx: context.Background()}
	addr1 := freeLoopbackAddr(t)
	if err := mgr.ensure(context.Background(), srv, addr1); err != nil {
		t.Fatalf("first ensure: %v", err)
	}
	if !srv.AgentListenerActive() || srv.AgentListenerAddr() != addr1 {
		t.Fatalf("first bind: active=%v addr=%q want %q", srv.AgentListenerActive(), srv.AgentListenerAddr(), addr1)
	}
	addr2 := freeLoopbackAddr(t)
	if err := mgr.ensure(context.Background(), srv, addr2); err != nil {
		t.Fatalf("rebind: %v", err)
	}
	if srv.AgentListenerAddr() != addr2 {
		t.Fatalf("rebind addr = %q, want %q", srv.AgentListenerAddr(), addr2)
	}
	// The old addr1 should be free again (listener closed) -> we can re-listen on it.
	ln, err := net.Listen("tcp", addr1)
	if err != nil {
		t.Fatalf("old addr %q still bound after rebind: %v", addr1, err)
	}
	_ = ln.Close()
}

// TestAgentListenerManagerEnsureUnchangedNoop proves ensure() with the same addr
// is a no-op (does not tear down + rebind the listener).
func TestAgentListenerManagerEnsureUnchangedNoop(t *testing.T) {
	srv := gateway.New(gateway.ServerDeps{})
	mgr := &agentListenerManager{baseCtx: context.Background()}
	addr := freeLoopbackAddr(t)
	if err := mgr.ensure(context.Background(), srv, addr); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	before := srv.AgentListenerAddr()
	if err := mgr.ensure(context.Background(), srv, addr); err != nil { // same addr -> no-op
		t.Fatalf("second ensure: %v", err)
	}
	if srv.AgentListenerAddr() != before {
		t.Fatalf("ensure with unchanged addr must be a no-op")
	}
}

// TestAgentListenerManagerEnsureEmptyStops proves ensure("") stops the running
// listener and reports inactive (single-listener mode).
func TestAgentListenerManagerEnsureEmptyStops(t *testing.T) {
	srv := gateway.New(gateway.ServerDeps{})
	mgr := &agentListenerManager{baseCtx: context.Background()}
	addr := freeLoopbackAddr(t)
	_ = mgr.ensure(context.Background(), srv, addr)
	_ = mgr.ensure(context.Background(), srv, "") // desired "" -> stop + inactive
	if srv.AgentListenerActive() || srv.AgentListenerAddr() != "" {
		t.Fatalf("ensure(\"\") must stop the listener + report inactive")
	}
}

func TestAgentListenerStartsPlainWhenGatewayMaterialIsAbsent(t *testing.T) {
	srv, _ := buildAgentListenerTLSServer(t)
	mgr := &agentListenerManager{baseCtx: context.Background()}
	t.Cleanup(func() { _ = mgr.ensure(context.Background(), srv, "") })
	addr := freeLoopbackAddr(t)
	if err := mgr.ensure(context.Background(), srv, addr); err != nil {
		t.Fatalf("ensure plain listener: %v", err)
	}
	if _, ok := mgr.plain.ln.(*sniffListener); ok || mgr.plain.tlsEnabled {
		t.Fatal("gateway material is absent, but the listener was made TLS-capable")
	}
	if got := srv.AgentListenerTLSState(); got != (gateway.AgentListenerTLSState{}) {
		t.Fatalf("plain listener reported TLS state: %+v", got)
	}
	getSniffTestBody(t, &http.Client{Timeout: 2 * time.Second}, "http://"+addr+"/healthz")
}

func TestAgentListenerStartsTLSCapableWhenMaterialExists(t *testing.T) {
	srv, routes := buildAgentListenerTLSServer(t)
	fullchain, fingerprint, notAfter := storeGatewayListenerMaterial(t, routes, 11)
	mgr := &agentListenerManager{baseCtx: context.Background()}
	t.Cleanup(func() { _ = mgr.ensure(context.Background(), srv, "") })
	addr := freeLoopbackAddr(t)
	if err := mgr.ensure(context.Background(), srv, addr); err != nil {
		t.Fatalf("ensure TLS-capable listener: %v", err)
	}
	if _, ok := mgr.plain.ln.(*sniffListener); !ok || !mgr.plain.tlsEnabled {
		t.Fatalf("listener type=%T tlsEnabled=%v, want TLS-capable sniff listener", mgr.plain.ln, mgr.plain.tlsEnabled)
	}
	if got := srv.AgentListenerTLSState(); got != (gateway.AgentListenerTLSState{
		Active: true, Address: addr, Fingerprint: hex.EncodeToString(fingerprint[:]), NotAfter: notAfter,
	}) {
		t.Fatalf("TLS state = %+v, want active holder material", got)
	}
	getSniffTestBody(t, newSniffTestHTTPClient(t, fullchain), "https://"+addr+"/healthz")
}

// buildSeparateModeTLSServer mirrors buildAgentListenerTLSServer (cert module on,
// self-signed, gateway domain set) but also flips cert_mesh_tls_mode to "separate"
// so CertMeshTLSSeparateActive reports true — the mode the dedicated TLS-port
// listener wiring keys on.
func buildSeparateModeTLSServer(t *testing.T) (*gateway.Server, routing.Store) {
	t.Helper()
	ctx := context.Background()
	settings := portal.NewMemorySystemSettings()
	routes := routing.NewMemoryStore()
	portalSvc := portal.NewService(portal.ServiceDeps{
		Routes:           routes,
		SystemSettings:   settings,
		SettingsVolatile: true,
	})
	on := true
	mode := portal.IssuerModeSelfSigned
	base := "int.example.test"
	domain := "gateway.int.example.test"
	separate := "separate"
	if _, err := portalSvc.UpdateSystemSettings(ctx, auth.Token{Scopes: []string{"system"}}, portal.UpdateSystemSettingsRequest{
		CertEnabled:       &on,
		CertIssuerMode:    &mode,
		CertBaseDomain:    &base,
		CertGatewayDomain: &domain,
		CertMeshTLSMode:   &separate,
	}); err != nil {
		t.Fatalf("enable certificate module in separate mode: %v", err)
	}
	if active, err := portalSvc.CertMeshTLSSeparateActive(ctx); err != nil || !active {
		t.Fatalf("CertMeshTLSSeparateActive = %v, %v; want true, nil", active, err)
	}
	return gateway.New(gateway.ServerDeps{Portal: portalSvc, Routes: routes}), routes
}

// TestSeparateModeBindsPlainAndTLS proves that in separate mode ensureAll brings up
// BOTH agent binds: the primary bind is a raw plaintext listener (bindRolePlainOnly,
// no sniffer, tlsEnabled false) on AGENT_PORT, and a dedicated TLS bind
// (bindRoleTLSOnly, tlsEnabled true) on AGENT_TLS_PORT. The observable state splits
// too: AgentListenerActive() true (either up), the plaintext slot carries the plain
// address, and the TLS slot (AgentListenerTLSState) carries the TLS-port address and
// the gateway leaf — which is what makes the generated agent gateway_url point at the
// TLS port.
func TestSeparateModeBindsPlainAndTLS(t *testing.T) {
	srv, routes := buildSeparateModeTLSServer(t)
	fullchain, fingerprint, notAfter := storeGatewayListenerMaterial(t, routes, 31)
	mgr := &agentListenerManager{baseCtx: context.Background()}
	t.Cleanup(func() { _ = mgr.ensureAll(context.Background(), srv, config.Config{}) })

	plainAddr := freeLoopbackAddr(t)
	tlsAddr := freeLoopbackAddr(t)
	// Explicit addresses win over peer resolution, keeping the test deterministic and
	// off the NetBird control plane. Ports still differ (plain vs TLS), matching the
	// separate-mode invariant that the two binds share a host but differ by port.
	cfg := config.Config{
		AgentAddr: plainAddr, AgentPort: "8081",
		AgentTLSAddr: tlsAddr, AgentTLSPort: "8443",
	}
	if err := mgr.ensureAll(context.Background(), srv, cfg); err != nil {
		t.Fatalf("ensureAll (separate): %v", err)
	}

	// Primary bind: raw plaintext, no sniffer, no TLS.
	if mgr.plain.role != bindRolePlainOnly {
		t.Fatalf("plain bind role = %v, want bindRolePlainOnly", mgr.plain.role)
	}
	if _, ok := mgr.plain.ln.(*sniffListener); ok {
		t.Fatal("separate-mode plain bind must be raw (no protocol sniffer)")
	}
	if mgr.plain.tlsEnabled {
		t.Fatal("separate-mode plain bind must not enable TLS")
	}
	if mgr.plain.addr != plainAddr {
		t.Fatalf("plain bind addr = %q, want %q", mgr.plain.addr, plainAddr)
	}

	// Dedicated TLS bind: TLS-only, on the TLS port, carrying the gateway leaf.
	if mgr.tls == nil {
		t.Fatal("separate mode did not create the dedicated TLS bind")
	}
	if mgr.tls.role != bindRoleTLSOnly {
		t.Fatalf("tls bind role = %v, want bindRoleTLSOnly", mgr.tls.role)
	}
	if !mgr.tls.tlsEnabled || mgr.tls.addr != tlsAddr {
		t.Fatalf("tls bind: tlsEnabled=%v addr=%q, want true %q", mgr.tls.tlsEnabled, mgr.tls.addr, tlsAddr)
	}
	if mgr.tls.mgr != mgr {
		t.Fatal("tls bind mgr back-ref not wired to the owning manager")
	}

	// Dual observable state.
	if !srv.AgentListenerActive() {
		t.Fatal("AgentListenerActive = false, want true (both binds up)")
	}
	plain, tlsState := srv.AgentListenerStates()
	if !plain.Active || plain.Address != plainAddr {
		t.Fatalf("plain state = %+v, want active at %q", plain, plainAddr)
	}
	wantTLS := gateway.AgentListenerTLSState{
		Active: true, Address: tlsAddr, Fingerprint: hex.EncodeToString(fingerprint[:]), NotAfter: notAfter,
	}
	if tlsState != wantTLS {
		t.Fatalf("TLS state = %+v, want the dedicated TLS bind snapshot %+v", tlsState, wantTLS)
	}
	if got := srv.AgentListenerTLSState(); got != wantTLS {
		t.Fatalf("AgentListenerTLSState() = %+v, want %+v (TLS-port carrying bind)", got, wantTLS)
	}

	// The primary bind actually serves plaintext HTTP; the dedicated bind serves TLS
	// with the gateway leaf.
	getSniffTestBody(t, &http.Client{Timeout: 2 * time.Second}, "http://"+plainAddr+"/healthz")
	if got := dialSniffTestTLSFingerprint(t, tlsAddr, newSniffTestRoots(t, fullchain)); got != fingerprint {
		t.Fatalf("TLS bind leaf = %x, want %x", got, fingerprint)
	}
}

// TestSeparateModeKeepsTLSBindDownUntilMaterialAppears proves the dedicated TLS bind
// stays down while mesh material is absent (a TLS-only listener must never serve
// plaintext), while the plaintext bind still comes up; a later ensureAll after the
// gateway leaf is stored brings the TLS bind up.
func TestSeparateModeKeepsTLSBindDownUntilMaterialAppears(t *testing.T) {
	srv, routes := buildSeparateModeTLSServer(t)
	mgr := &agentListenerManager{baseCtx: context.Background()}
	t.Cleanup(func() { _ = mgr.ensureAll(context.Background(), srv, config.Config{}) })

	plainAddr := freeLoopbackAddr(t)
	tlsAddr := freeLoopbackAddr(t)
	cfg := config.Config{AgentAddr: plainAddr, AgentPort: "8081", AgentTLSAddr: tlsAddr, AgentTLSPort: "8443"}

	if err := mgr.ensureAll(context.Background(), srv, cfg); err != nil {
		t.Fatalf("ensureAll (no material): %v", err)
	}
	if mgr.tls == nil || mgr.tls.addr != "" || mgr.tls.tlsEnabled {
		t.Fatalf("TLS bind should be down without material: %+v", mgr.tls)
	}
	if got := srv.AgentListenerTLSState(); got != (gateway.AgentListenerTLSState{}) {
		t.Fatalf("TLS state should be empty without material: %+v", got)
	}
	plain, _ := srv.AgentListenerStates()
	if !plain.Active || plain.Address != plainAddr {
		t.Fatalf("plaintext bind must still come up without material: %+v", plain)
	}

	_, fingerprint, notAfter := storeGatewayListenerMaterial(t, routes, 41)
	if err := mgr.ensureAll(context.Background(), srv, cfg); err != nil {
		t.Fatalf("ensureAll (material present): %v", err)
	}
	if mgr.tls == nil || !mgr.tls.tlsEnabled || mgr.tls.addr != tlsAddr {
		t.Fatalf("TLS bind should be up after material appears: %+v", mgr.tls)
	}
	want := gateway.AgentListenerTLSState{
		Active: true, Address: tlsAddr, Fingerprint: hex.EncodeToString(fingerprint[:]), NotAfter: notAfter,
	}
	if got := srv.AgentListenerTLSState(); got != want {
		t.Fatalf("TLS state after material = %+v, want %+v", got, want)
	}
}

// TestModeToggleRebindsBinds proves an operator toggle rebinds within one ensureAll:
// combined -> separate splits the single sniffing bind into a raw plaintext bind plus
// a dedicated TLS bind, and separate -> combined tears the TLS bind back down and
// restores the sniffing plaintext bind.
func TestModeToggleRebindsBinds(t *testing.T) {
	ctx := context.Background()
	settings := portal.NewMemorySystemSettings()
	routes := routing.NewMemoryStore()
	portalSvc := portal.NewService(portal.ServiceDeps{Routes: routes, SystemSettings: settings, SettingsVolatile: true})
	on := true
	mode := portal.IssuerModeSelfSigned
	base := "int.example.test"
	domain := "gateway.int.example.test"
	combined := "combined"
	if _, err := portalSvc.UpdateSystemSettings(ctx, auth.Token{Scopes: []string{"system"}}, portal.UpdateSystemSettingsRequest{
		CertEnabled: &on, CertIssuerMode: &mode, CertBaseDomain: &base, CertGatewayDomain: &domain,
		CertMeshTLSMode: &combined,
	}); err != nil {
		t.Fatalf("configure combined mode: %v", err)
	}
	srv := gateway.New(gateway.ServerDeps{Portal: portalSvc, Routes: routes})
	_, fingerprint, notAfter := storeGatewayListenerMaterial(t, routes, 51)

	mgr := &agentListenerManager{baseCtx: ctx}
	t.Cleanup(func() { _ = mgr.ensureAll(context.Background(), srv, config.Config{}) })
	plainAddr := freeLoopbackAddr(t)
	tlsAddr := freeLoopbackAddr(t)
	cfg := config.Config{AgentAddr: plainAddr, AgentPort: "8081", AgentTLSAddr: tlsAddr, AgentTLSPort: "8443"}

	// Combined: one sniffing bind, no dedicated TLS bind, TLS slot carries the plain addr.
	if err := mgr.ensureAll(ctx, srv, cfg); err != nil {
		t.Fatalf("ensureAll combined: %v", err)
	}
	if mgr.tls != nil {
		t.Fatalf("combined mode populated the dedicated TLS bind: %+v", mgr.tls)
	}
	if _, ok := mgr.plain.ln.(*sniffListener); !ok || mgr.plain.role != bindRolePlainCombined {
		t.Fatalf("combined plain bind not a sniffing combined bind: type=%T role=%v", mgr.plain.ln, mgr.plain.role)
	}
	if got := srv.AgentListenerTLSState().Address; got != plainAddr {
		t.Fatalf("combined TLS state address = %q, want the combined plain addr %q", got, plainAddr)
	}

	// Toggle to separate.
	separate := "separate"
	if _, err := portalSvc.UpdateSystemSettings(ctx, auth.Token{Scopes: []string{"system"}}, portal.UpdateSystemSettingsRequest{CertMeshTLSMode: &separate}); err != nil {
		t.Fatalf("toggle to separate: %v", err)
	}
	if err := mgr.ensureAll(ctx, srv, cfg); err != nil {
		t.Fatalf("ensureAll separate: %v", err)
	}
	if mgr.tls == nil || !mgr.tls.tlsEnabled || mgr.tls.addr != tlsAddr {
		t.Fatalf("separate did not bring up the dedicated TLS bind: %+v", mgr.tls)
	}
	if mgr.plain.role != bindRolePlainOnly || mgr.plain.tlsEnabled {
		t.Fatalf("separate plain bind not raw plain-only: role=%v tls=%v", mgr.plain.role, mgr.plain.tlsEnabled)
	}
	if _, ok := mgr.plain.ln.(*sniffListener); ok {
		t.Fatal("separate plain bind still a sniffer after toggle (not re-wrapped)")
	}
	wantTLS := gateway.AgentListenerTLSState{Active: true, Address: tlsAddr, Fingerprint: hex.EncodeToString(fingerprint[:]), NotAfter: notAfter}
	if got := srv.AgentListenerTLSState(); got != wantTLS {
		t.Fatalf("separate TLS state = %+v, want %+v", got, wantTLS)
	}

	// Toggle back to combined.
	if _, err := portalSvc.UpdateSystemSettings(ctx, auth.Token{Scopes: []string{"system"}}, portal.UpdateSystemSettingsRequest{CertMeshTLSMode: &combined}); err != nil {
		t.Fatalf("toggle back to combined: %v", err)
	}
	if err := mgr.ensureAll(ctx, srv, cfg); err != nil {
		t.Fatalf("ensureAll back to combined: %v", err)
	}
	if mgr.tls != nil {
		t.Fatalf("combined toggle left the dedicated TLS bind up: %+v", mgr.tls)
	}
	if mgr.plain.role != bindRolePlainCombined {
		t.Fatalf("plain bind role = %v after toggle back, want bindRolePlainCombined", mgr.plain.role)
	}
	if _, ok := mgr.plain.ln.(*sniffListener); !ok {
		t.Fatal("plain bind not re-wrapped as a sniffing combined bind after toggle back")
	}
	if got := srv.AgentListenerTLSState().Address; got != plainAddr {
		t.Fatalf("combined TLS state address = %q after toggle back, want the plain addr %q", got, plainAddr)
	}
}

// fakeModePortal is a controllable portal.API for the mode-aware ensureAll: the
// cert_mesh_tls_mode result and the gateway mesh material (incl. their errors) are
// settable, and every other API method is nil (never called on these paths because
// the tests use explicit AGENT_ADDR/AGENT_TLS_ADDR, skipping peer resolution).
type fakeModePortal struct {
	portal.API
	mu          sync.Mutex
	separate    bool
	separateErr error
	material    portal.GatewayMeshCertificateMaterial
	materialErr error
	peerIP      string
	peerErr     error
}

func (f *fakeModePortal) CertMeshTLSSeparateActive(context.Context) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.separateErr != nil {
		// Mirror the real Service: a settings read failure returns (false, err), so
		// a caller that discards the error would wrongly fall into the combined path.
		return false, f.separateErr
	}
	return f.separate, nil
}

func (f *fakeModePortal) GatewayMeshCertificate(context.Context) (portal.GatewayMeshCertificateMaterial, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.material, f.materialErr
}

// ResolveGatewayPeerIP lets a test drive resolveAgentAddr/resolveAgentTLSAddr
// through the peer-resolution path (AgentAddr/AgentTLSAddr left empty), so a
// resolve blip can be simulated as an (addr,false) ok result -- the exact
// plainOK=false condition the setPlainRoleLocked-before-the-gate fix guards.
func (f *fakeModePortal) ResolveGatewayPeerIP(context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.peerIP, f.peerErr
}

func (f *fakeModePortal) setSeparate(v bool)     { f.mu.Lock(); f.separate = v; f.mu.Unlock() }
func (f *fakeModePortal) setSeparateErr(e error) { f.mu.Lock(); f.separateErr = e; f.mu.Unlock() }
func (f *fakeModePortal) setPeerErr(e error)     { f.mu.Lock(); f.peerErr = e; f.mu.Unlock() }

func fakeMeshMaterial(t *testing.T, serial int64) portal.GatewayMeshCertificateMaterial {
	t.Helper()
	fullchain, keyPEM, fp := newSniffTestCertificate(t, serial)
	return portal.GatewayMeshCertificateMaterial{
		Domain: "gateway.int.example.test", FullchainPEM: fullchain, KeyPEM: keyPEM,
		Fingerprint: hex.EncodeToString(fp[:]), NotAfter: time.Now().Add(24 * time.Hour).UTC(),
	}
}

func stopBothBinds(mgr *agentListenerManager) func() {
	return func() {
		mgr.mu.Lock()
		mgr.plain.stopLocked()
		if mgr.tls != nil {
			mgr.tls.stopLocked()
		}
		mgr.mu.Unlock()
	}
}

// TestModeToggleKeepsListenerArmedAcrossTransientRebindFailure is the IMPORTANT-1
// regression: a combined->separate toggle stops the primary bind to re-wrap it at
// the SAME port. A transient bind failure right after the stop must NOT disarm the
// observable state (which gates netbird_only isolation) — the re-wrap keeps state
// armed across a bounded relistenWithRetry and only disarms on genuine exhaustion,
// mirroring enableTLSLocked. Pre-fix this failed two ways: setPlainRoleLocked
// publishInactive'd up front, and the plain-only rebind did a single bare listen.
func TestModeToggleKeepsListenerArmedAcrossTransientRebindFailure(t *testing.T) {
	fake := &fakeModePortal{separate: false, material: fakeMeshMaterial(t, 71)}
	srv := gateway.New(gateway.ServerDeps{Portal: fake})
	mgr := &agentListenerManager{baseCtx: context.Background()}
	t.Cleanup(stopBothBinds(mgr))

	plainAddr := freeLoopbackAddr(t)
	tlsAddr := freeLoopbackAddr(t)
	cfg := config.Config{AgentAddr: plainAddr, AgentPort: "8081", AgentTLSAddr: tlsAddr, AgentTLSPort: "8443"}

	// mgr.listen is only ever called synchronously from ensureAll on this goroutine,
	// so these observation vars need no lock.
	var (
		failPlain    bool
		failsLeft    int
		duringActive []bool
		duringPlain  []string
	)
	mgr.listen = func(network, address string) (net.Listener, error) {
		if failPlain && address == plainAddr && failsLeft > 0 {
			failsLeft--
			plain, _ := srv.AgentListenerStates()
			duringActive = append(duringActive, srv.AgentListenerActive())
			duringPlain = append(duringPlain, plain.Address)
			return nil, errors.New("transient bind failure")
		}
		return net.Listen(network, address)
	}

	// Phase 1: combined mode binds the sniffing primary on plainAddr.
	if err := mgr.ensureAll(context.Background(), srv, cfg); err != nil {
		t.Fatalf("ensureAll combined: %v", err)
	}
	if !srv.AgentListenerActive() {
		t.Fatal("combined precondition: not active")
	}
	if _, ok := mgr.plain.ln.(*sniffListener); !ok {
		t.Fatal("combined precondition: primary bind is not a sniffer")
	}

	// Phase 2: toggle to separate with the plain re-wrap relisten failing twice first.
	fake.setSeparate(true)
	failPlain, failsLeft = true, 2
	if err := mgr.ensureAll(context.Background(), srv, cfg); err != nil {
		t.Fatalf("ensureAll separate (transient plain re-wrap failure must recover): %v", err)
	}

	if len(duringActive) == 0 {
		t.Fatal("injector never intercepted a plain re-wrap attempt (relistenWithRetry not used?)")
	}
	for i := range duringActive {
		if !duringActive[i] || duringPlain[i] != plainAddr {
			t.Fatalf("attempt %d: active=%v plainAddr=%q, want the listener to stay ARMED at %q across the transient failure",
				i, duringActive[i], duringPlain[i], plainAddr)
		}
	}

	// After recovery the primary bind is raw plain-only at the same addr, still armed.
	if mgr.plain.role != bindRolePlainOnly || mgr.plain.addr != plainAddr {
		t.Fatalf("plain bind after recovery: role=%v addr=%q", mgr.plain.role, mgr.plain.addr)
	}
	if _, ok := mgr.plain.ln.(*sniffListener); ok {
		t.Fatal("primary bind still a sniffer after the re-wrap")
	}
	if !srv.AgentListenerActive() {
		t.Fatal("AgentListenerActive false after recovery")
	}
	plain, _ := srv.AgentListenerStates()
	if !plain.Active || plain.Address != plainAddr {
		t.Fatalf("plain slot after recovery = %+v, want armed at %q", plain, plainAddr)
	}
}

// TestEnsureAllKeepsTopologyOnModeReadError is the IMPORTANT-2 regression: a
// settings-store blip surfaces as a CertMeshTLSSeparateActive error. ensureAll must
// treat that like resolveAgentAddr's ok=false — keep the current topology this tick
// (do NOT tear down a live dedicated TLS bind, do NOT flip roles back to the
// sniffer). Pre-fix the error was discarded, defaulting to combined and dropping the
// TLS listener on every reconcile tick in separate steady state.
func TestEnsureAllKeepsTopologyOnModeReadError(t *testing.T) {
	fake := &fakeModePortal{separate: true, material: fakeMeshMaterial(t, 81)}
	srv := gateway.New(gateway.ServerDeps{Portal: fake})
	mgr := &agentListenerManager{baseCtx: context.Background()}
	t.Cleanup(stopBothBinds(mgr))

	plainAddr := freeLoopbackAddr(t)
	tlsAddr := freeLoopbackAddr(t)
	cfg := config.Config{AgentAddr: plainAddr, AgentPort: "8081", AgentTLSAddr: tlsAddr, AgentTLSPort: "8443"}

	// Bring up separate mode: raw plain-only + dedicated TLS bind.
	if err := mgr.ensureAll(context.Background(), srv, cfg); err != nil {
		t.Fatalf("ensureAll separate: %v", err)
	}
	if mgr.tls == nil || !mgr.tls.tlsEnabled || mgr.tls.addr != tlsAddr {
		t.Fatalf("precondition: dedicated TLS bind not up: %+v", mgr.tls)
	}
	tlsServerBefore, tlsLnBefore := mgr.tls.server, mgr.tls.ln
	tlsStateBefore := srv.AgentListenerTLSState()

	// A settings-store blip on the mode read.
	fake.setSeparateErr(errors.New("settings store blip"))
	if err := mgr.ensureAll(context.Background(), srv, cfg); err != nil {
		t.Fatalf("ensureAll on a mode-read error must be a clean no-op, got: %v", err)
	}

	if mgr.tls == nil {
		t.Fatal("mode-read error tore down the dedicated TLS bind")
	}
	if mgr.tls.server != tlsServerBefore || mgr.tls.ln != tlsLnBefore || mgr.tls.addr != tlsAddr {
		t.Fatalf("mode-read error rebound the TLS bind: %+v", mgr.tls)
	}
	if mgr.plain.role != bindRolePlainOnly {
		t.Fatalf("mode-read error flipped the plain role to %v (want bindRolePlainOnly)", mgr.plain.role)
	}
	if !srv.AgentListenerActive() {
		t.Fatal("mode-read error dropped AgentListenerActive")
	}
	if got := srv.AgentListenerTLSState(); got != tlsStateBefore {
		t.Fatalf("mode-read error changed the TLS slot: got %+v, want %+v", got, tlsStateBefore)
	}
}

// TestEnsureAllDefersPlainRoleFlipUntilAddressInHand is the FINDING-2 regression
// for the combined->separate direction: setPlainRoleLocked (which stopLocked()s
// the primary socket on a genuine role change but deliberately does NOT
// publishInactive) must NOT run before the plainOK gate. On the mode-flip tick
// where the peer-IP resolve blips (plainOK=false), the just-serving plain
// listener must be kept intact (never stopped-then-not-rebound, which would leave
// NO agent listener on any port while the status still reports it active), and a
// later ok tick must perform the deferred role change.
func TestEnsureAllDefersPlainRoleFlipUntilAddressInHand(t *testing.T) {
	fake := &fakeModePortal{separate: false, material: fakeMeshMaterial(t, 91), peerIP: "127.0.0.1"}
	srv := gateway.New(gateway.ServerDeps{Portal: fake})
	mgr := &agentListenerManager{baseCtx: context.Background()}
	t.Cleanup(stopBothBinds(mgr))

	// AgentAddr/AgentTLSAddr left empty so ensureAll resolves through
	// ResolveGatewayPeerIP -> the plainOK/tlsOK gates the fix touches.
	_, plainPort, err := net.SplitHostPort(freeLoopbackAddr(t))
	if err != nil {
		t.Fatalf("split plain port: %v", err)
	}
	_, tlsPort, err := net.SplitHostPort(freeLoopbackAddr(t))
	if err != nil {
		t.Fatalf("split tls port: %v", err)
	}
	cfg := config.Config{AgentPort: plainPort, AgentTLSPort: tlsPort}
	plainAddr := net.JoinHostPort("127.0.0.1", plainPort)

	// Phase 1: combined mode binds the sniffing primary at the resolved plainAddr.
	if err := mgr.ensureAll(context.Background(), srv, cfg); err != nil {
		t.Fatalf("ensureAll combined: %v", err)
	}
	if mgr.plain.role != bindRolePlainCombined || mgr.plain.addr != plainAddr || mgr.plain.ln == nil {
		t.Fatalf("combined precondition: role=%v addr=%q ln=%v", mgr.plain.role, mgr.plain.addr, mgr.plain.ln)
	}
	if !srv.AgentListenerActive() {
		t.Fatal("combined precondition: not active")
	}
	lnBefore, serverBefore := mgr.plain.ln, mgr.plain.server

	// Phase 2: flip to SEPARATE on a peer-IP resolve blip (plainOK=false).
	fake.setSeparate(true)
	fake.setPeerErr(errors.New("netbird resolve blip"))
	if err := mgr.ensureAll(context.Background(), srv, cfg); err != nil {
		t.Fatalf("ensureAll flip-to-separate on a resolve blip must not error: %v", err)
	}
	if mgr.plain.ln != lnBefore || mgr.plain.server != serverBefore || mgr.plain.addr != plainAddr {
		t.Fatalf("resolve blip tore down/rebound the plain listener: addr=%q ln-unchanged=%v server-unchanged=%v",
			mgr.plain.addr, mgr.plain.ln == lnBefore, mgr.plain.server == serverBefore)
	}
	if mgr.plain.role != bindRolePlainCombined {
		t.Fatalf("resolve blip flipped the plain role to %v before an address was in hand (want unchanged bindRolePlainCombined)", mgr.plain.role)
	}
	if !srv.AgentListenerActive() {
		t.Fatal("resolve blip dropped AgentListenerActive (listener stopped with state still armed)")
	}

	// Phase 3: a subsequent OK tick performs the deferred role change to separate.
	fake.setPeerErr(nil)
	if err := mgr.ensureAll(context.Background(), srv, cfg); err != nil {
		t.Fatalf("ensureAll separate (ok tick): %v", err)
	}
	if mgr.plain.role != bindRolePlainOnly {
		t.Fatalf("ok tick did not perform the deferred role change: role=%v, want bindRolePlainOnly", mgr.plain.role)
	}
	if _, ok := mgr.plain.ln.(*sniffListener); ok {
		t.Fatal("plain bind still a sniffer after the separate role change")
	}
	if !srv.AgentListenerActive() {
		t.Fatal("ok tick left the plain listener down")
	}
}

// TestEnsureAllKeepsTopologyAcrossResolveBlipOnFlipToCombined is the FINDING-2
// regression for the combined direction: flipping separate->combined on a
// peer-IP resolve blip (plainOK=false) must NOT tear down the still-serving raw
// plain listener OR the dedicated TLS bind before the primary can be re-wrapped
// as a sniffer. Pre-fix setPlainRoleLocked stopped the plain socket (then skipped
// the rebind) and the TLS bind was dropped -- leaving a gap on both ports. A
// later ok tick completes the switch.
func TestEnsureAllKeepsTopologyAcrossResolveBlipOnFlipToCombined(t *testing.T) {
	fake := &fakeModePortal{separate: true, material: fakeMeshMaterial(t, 92), peerIP: "127.0.0.1"}
	srv := gateway.New(gateway.ServerDeps{Portal: fake})
	mgr := &agentListenerManager{baseCtx: context.Background()}
	t.Cleanup(stopBothBinds(mgr))

	_, plainPort, err := net.SplitHostPort(freeLoopbackAddr(t))
	if err != nil {
		t.Fatalf("split plain port: %v", err)
	}
	_, tlsPort, err := net.SplitHostPort(freeLoopbackAddr(t))
	if err != nil {
		t.Fatalf("split tls port: %v", err)
	}
	cfg := config.Config{AgentPort: plainPort, AgentTLSPort: tlsPort}
	plainAddr := net.JoinHostPort("127.0.0.1", plainPort)
	tlsAddr := net.JoinHostPort("127.0.0.1", tlsPort)

	// Phase 1: separate mode = raw plain-only primary + dedicated TLS bind.
	if err := mgr.ensureAll(context.Background(), srv, cfg); err != nil {
		t.Fatalf("ensureAll separate: %v", err)
	}
	if mgr.plain.role != bindRolePlainOnly || mgr.plain.addr != plainAddr || mgr.plain.ln == nil {
		t.Fatalf("separate precondition plain: role=%v addr=%q ln=%v", mgr.plain.role, mgr.plain.addr, mgr.plain.ln)
	}
	if mgr.tls == nil || !mgr.tls.tlsEnabled || mgr.tls.addr != tlsAddr {
		t.Fatalf("separate precondition tls: %+v", mgr.tls)
	}
	plainLnBefore := mgr.plain.ln
	tlsLnBefore, tlsSrvBefore := mgr.tls.ln, mgr.tls.server

	// Phase 2: flip to COMBINED on a resolve blip (plainOK=false).
	fake.setSeparate(false)
	fake.setPeerErr(errors.New("netbird resolve blip"))
	if err := mgr.ensureAll(context.Background(), srv, cfg); err != nil {
		t.Fatalf("ensureAll flip-to-combined on a resolve blip must not error: %v", err)
	}
	if mgr.plain.ln != plainLnBefore || mgr.plain.addr != plainAddr || mgr.plain.role != bindRolePlainOnly {
		t.Fatalf("resolve blip disturbed the plain bind: role=%v addr=%q ln-unchanged=%v",
			mgr.plain.role, mgr.plain.addr, mgr.plain.ln == plainLnBefore)
	}
	if mgr.tls == nil || mgr.tls.ln != tlsLnBefore || mgr.tls.server != tlsSrvBefore {
		t.Fatalf("resolve blip tore down/rebound the dedicated TLS bind: %+v", mgr.tls)
	}
	if !srv.AgentListenerActive() {
		t.Fatal("resolve blip dropped AgentListenerActive")
	}

	// Phase 3: an ok tick completes the switch to combined (sniffing primary, TLS
	// bind gone).
	fake.setPeerErr(nil)
	if err := mgr.ensureAll(context.Background(), srv, cfg); err != nil {
		t.Fatalf("ensureAll combined (ok tick): %v", err)
	}
	if mgr.tls != nil {
		t.Fatalf("ok tick left the dedicated TLS bind up: %+v", mgr.tls)
	}
	if mgr.plain.role != bindRolePlainCombined {
		t.Fatalf("ok tick did not flip plain to combined: role=%v", mgr.plain.role)
	}
	if _, ok := mgr.plain.ln.(*sniffListener); !ok {
		t.Fatal("plain bind not re-wrapped as a sniffer after the ok tick")
	}
}

func TestAgentListenerUnexpectedServeFailureClearsTLSStateAndRecovers(t *testing.T) {
	srv, routes := buildAgentListenerTLSServer(t)
	fullchain, fingerprint, _ := storeGatewayListenerMaterial(t, routes, 121)
	mgr := &agentListenerManager{baseCtx: context.Background()}
	t.Cleanup(func() { _ = mgr.ensure(context.Background(), srv, "") })
	addr := freeLoopbackAddr(t)
	serveErr := errors.New("injected permanent accept failure")
	failing := newSignalFailListener(t, addr, serveErr)
	listenCalls := 0
	mgr.listen = func(network, address string) (net.Listener, error) {
		listenCalls++
		if listenCalls == 1 {
			return failing, nil
		}
		return net.Listen(network, address)
	}
	if err := mgr.ensure(context.Background(), srv, addr); err != nil {
		t.Fatalf("initial TLS bind: %v", err)
	}
	if !srv.AgentListenerActive() || !mgr.plain.tlsEnabled || srv.AgentListenerTLSState().Address != addr {
		t.Fatalf("precondition: active=%v tls=%v snapshot=%+v", srv.AgentListenerActive(), mgr.plain.tlsEnabled, srv.AgentListenerTLSState())
	}
	select {
	case <-failing.accepting:
	case <-time.After(2 * time.Second):
		t.Fatal("Serve never started accepting from the injected listener")
	}
	close(failing.fail)
	select {
	case <-failing.returned:
	case <-time.After(2 * time.Second):
		t.Fatal("injected listener did not return its permanent Accept error")
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		mgr.mu.Lock()
		managerAddr, listener, server, tlsEnabled := mgr.plain.addr, mgr.plain.ln, mgr.plain.server, mgr.plain.tlsEnabled
		mgr.mu.Unlock()
		if !srv.AgentListenerActive() && managerAddr == "" && listener == nil && server == nil && !tlsEnabled {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("Serve failure left stale state: active=%v addr=%q listener=%T server_nil=%v tls=%v snapshot=%+v",
				srv.AgentListenerActive(), managerAddr, listener, server == nil, tlsEnabled, srv.AgentListenerTLSState())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := srv.AgentListenerTLSState(); got != (gateway.AgentListenerTLSState{}) {
		t.Fatalf("Serve failure left stale TLS snapshot: %+v", got)
	}

	if err := mgr.ensure(context.Background(), srv, addr); err != nil {
		t.Fatalf("recover same-address TLS listener: %v", err)
	}
	if !srv.AgentListenerActive() || !mgr.plain.tlsEnabled || srv.AgentListenerAddr() != addr {
		t.Fatalf("same-address recovery failed: active=%v addr=%q tls=%v", srv.AgentListenerActive(), srv.AgentListenerAddr(), mgr.plain.tlsEnabled)
	}
	if got := dialSniffTestTLSFingerprint(t, addr, newSniffTestRoots(t, fullchain)); got != fingerprint {
		t.Fatalf("recovered listener leaf = %x, want %x", got, fingerprint)
	}
}

func TestAgentListenerServeFailureThenMissingMaterialRecoversLastGoodTLS(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(t *testing.T, routes routing.Store)
	}{
		{name: "deleted", mutate: func(t *testing.T, routes routing.Store) {
			if err := routes.DeleteCertificate(context.Background(), "gateway.int.example.test"); err != nil {
				t.Fatalf("delete gateway material: %v", err)
			}
		}},
		{name: "expired", mutate: func(t *testing.T, routes routing.Store) {
			row, err := routes.CertificateByDomain(context.Background(), "gateway.int.example.test")
			if err != nil {
				t.Fatalf("load gateway material: %v", err)
			}
			row.NotAfter = time.Now().UTC().Add(-time.Minute)
			if err := routes.UpsertCertificate(context.Background(), row); err != nil {
				t.Fatalf("expire gateway material: %v", err)
			}
		}},
		{name: "unreadable", mutate: func(t *testing.T, routes routing.Store) {
			row, err := routes.CertificateByDomain(context.Background(), "gateway.int.example.test")
			if err != nil {
				t.Fatalf("load gateway material: %v", err)
			}
			row.KeySealed = "enc:AAAA"
			if err := routes.UpsertCertificate(context.Background(), row); err != nil {
				t.Fatalf("break gateway material: %v", err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, routes := buildAgentListenerTLSServer(t)
			fullchain, fingerprint, notAfter := storeGatewayListenerMaterial(t, routes, 123)
			mgr := &agentListenerManager{baseCtx: context.Background()}
			t.Cleanup(func() { _ = mgr.ensure(context.Background(), srv, "") })
			addr := freeLoopbackAddr(t)
			failing := newSignalFailListener(t, addr, errors.New("injected permanent accept failure"))
			listenCalls := 0
			mgr.listen = func(network, address string) (net.Listener, error) {
				listenCalls++
				if listenCalls == 1 {
					return failing, nil
				}
				return net.Listen(network, address)
			}
			if err := mgr.ensure(context.Background(), srv, addr); err != nil {
				t.Fatalf("initial TLS bind: %v", err)
			}
			select {
			case <-failing.accepting:
			case <-time.After(2 * time.Second):
				t.Fatal("Serve never started accepting from the injected listener")
			}
			close(failing.fail)
			select {
			case <-failing.returned:
			case <-time.After(2 * time.Second):
				t.Fatal("injected listener did not return its permanent Accept error")
			}

			deadline := time.Now().Add(2 * time.Second)
			for {
				mgr.mu.Lock()
				managerAddr, listener, server, tlsEnabled := mgr.plain.addr, mgr.plain.ln, mgr.plain.server, mgr.plain.tlsEnabled
				mgr.mu.Unlock()
				if !srv.AgentListenerActive() && managerAddr == "" && listener == nil && server == nil && !tlsEnabled {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("Serve failure did not retire runtime state: active=%v addr=%q listener=%T server_nil=%v tls=%v",
						srv.AgentListenerActive(), managerAddr, listener, server == nil, tlsEnabled)
				}
				time.Sleep(10 * time.Millisecond)
			}
			if got := srv.AgentListenerTLSState(); got != (gateway.AgentListenerTLSState{}) {
				t.Fatalf("Serve failure left runtime TLS snapshot: %+v", got)
			}
			tc.mutate(t, routes)

			if err := mgr.ensure(context.Background(), srv, addr); err != nil {
				t.Fatalf("recover same address without usable persisted material: %v", err)
			}
			if _, ok := mgr.plain.ln.(*sniffListener); !ok || !mgr.plain.tlsEnabled {
				t.Fatalf("last-good recovery listener type=%T tls=%v, want TLS-capable sniff listener", mgr.plain.ln, mgr.plain.tlsEnabled)
			}
			if got := srv.AgentListenerTLSState(); got != (gateway.AgentListenerTLSState{
				Active: true, Address: addr, Fingerprint: hex.EncodeToString(fingerprint[:]), NotAfter: notAfter,
			}) {
				t.Fatalf("last-good recovery TLS snapshot = %+v, want previous leaf metadata", got)
			}
			if got := dialSniffTestTLSFingerprint(t, addr, newSniffTestRoots(t, fullchain)); got != fingerprint {
				t.Fatalf("last-good recovery leaf = %x, want %x", got, fingerprint)
			}
		})
	}
}

func TestAgentListenerOldServeCompletionCannotClearReplacement(t *testing.T) {
	srv, routes := buildAgentListenerTLSServer(t)
	fullchain, fingerprint, _ := storeGatewayListenerMaterial(t, routes, 122)
	mgr := &agentListenerManager{baseCtx: context.Background()}
	t.Cleanup(func() { _ = mgr.ensure(context.Background(), srv, "") })
	addr1 := freeLoopbackAddr(t)
	if err := mgr.ensure(context.Background(), srv, addr1); err != nil {
		t.Fatalf("initial TLS bind: %v", err)
	}
	oldServer, oldListener := mgr.plain.server, mgr.plain.ln

	addr2 := freeLoopbackAddr(t)
	if err := mgr.ensure(context.Background(), srv, addr2); err != nil {
		t.Fatalf("replacement TLS bind: %v", err)
	}
	newServer, newListener := mgr.plain.server, mgr.plain.ln
	stateBefore := srv.AgentListenerTLSState()
	mgr.plain.retireServeGeneration(srv, oldServer, oldListener)

	if mgr.plain.server != newServer || mgr.plain.ln != newListener || mgr.plain.addr != addr2 || !mgr.plain.tlsEnabled {
		t.Fatalf("old Serve completion cleared replacement: server_same=%v listener_same=%v addr=%q tls=%v",
			mgr.plain.server == newServer, mgr.plain.ln == newListener, mgr.plain.addr, mgr.plain.tlsEnabled)
	}
	if !srv.AgentListenerActive() || srv.AgentListenerAddr() != addr2 {
		t.Fatalf("old Serve completion changed runtime listener: active=%v addr=%q", srv.AgentListenerActive(), srv.AgentListenerAddr())
	}
	if got := srv.AgentListenerTLSState(); got != stateBefore {
		t.Fatalf("old Serve completion changed replacement TLS snapshot: got %+v want %+v", got, stateBefore)
	}
	if got := dialSniffTestTLSFingerprint(t, addr2, newSniffTestRoots(t, fullchain)); got != fingerprint {
		t.Fatalf("replacement listener leaf = %x, want %x", got, fingerprint)
	}
}

func TestAgentListenerNewAddressBindFailurePreservesExactTLSState(t *testing.T) {
	srv, routes := buildAgentListenerTLSServer(t)
	fullchain, fingerprint, _ := storeGatewayListenerMaterial(t, routes, 111)
	mgr := &agentListenerManager{baseCtx: context.Background()}
	t.Cleanup(func() { _ = mgr.ensure(context.Background(), srv, "") })
	addr1 := freeLoopbackAddr(t)
	if err := mgr.ensure(context.Background(), srv, addr1); err != nil {
		t.Fatalf("initial TLS bind: %v", err)
	}
	listenerBefore := mgr.plain.ln
	serverBefore := mgr.plain.server
	stateBefore := srv.AgentListenerTLSState()
	holderBefore, err := mgr.plain.holder.GetCertificate(nil)
	if err != nil {
		t.Fatalf("load initial holder: %v", err)
	}

	listenErr := errors.New("injected new-address bind failure")
	mgr.listen = func(network, address string) (net.Listener, error) {
		if network != "tcp" {
			t.Errorf("listen network = %q, want tcp", network)
		}
		return nil, listenErr
	}
	addr2 := freeLoopbackAddr(t)
	if err := mgr.ensure(context.Background(), srv, addr2); !errors.Is(err, listenErr) {
		t.Fatalf("new-address ensure error = %v, want %v", err, listenErr)
	}

	if mgr.plain.ln != listenerBefore || mgr.plain.server != serverBefore || mgr.plain.addr != addr1 || !mgr.plain.tlsEnabled {
		t.Fatalf("failed bind changed manager state: listener_same=%v server_same=%v addr=%q tls=%v",
			mgr.plain.ln == listenerBefore, mgr.plain.server == serverBefore, mgr.plain.addr, mgr.plain.tlsEnabled)
	}
	if !srv.AgentListenerActive() || srv.AgentListenerAddr() != addr1 {
		t.Fatalf("failed bind changed listener status: active=%v addr=%q", srv.AgentListenerActive(), srv.AgentListenerAddr())
	}
	if got := srv.AgentListenerTLSState(); got != stateBefore {
		t.Fatalf("failed bind changed TLS snapshot: got %+v want %+v", got, stateBefore)
	}
	holderAfter, err := mgr.plain.holder.GetCertificate(nil)
	if err != nil {
		t.Fatalf("load holder after failed bind: %v", err)
	}
	if holderAfter != holderBefore {
		t.Fatal("failed bind replaced the last-good certificate holder")
	}
	if got := dialSniffTestTLSFingerprint(t, addr1, newSniffTestRoots(t, fullchain)); got != fingerprint {
		t.Fatalf("failed bind changed served leaf: got %x want %x", got, fingerprint)
	}
}

func TestAgentListenerEnablesTLSOnceOnSameAddress(t *testing.T) {
	srv, routes := buildAgentListenerTLSServer(t)
	mgr := &agentListenerManager{baseCtx: context.Background()}
	t.Cleanup(func() { _ = mgr.ensure(context.Background(), srv, "") })
	addr := freeLoopbackAddr(t)
	if err := mgr.ensure(context.Background(), srv, addr); err != nil {
		t.Fatalf("initial plain bind: %v", err)
	}
	plainListener := mgr.plain.ln
	now := time.Now().UTC()
	if err := routes.CreateAIServer(context.Background(), routing.AIServer{
		ID: "server-1", Name: "Server 1", Domain: "server-1.example.test",
		Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("store server: %v", err)
	}
	if err := routes.UpsertAgentToken(context.Background(), routing.AgentToken{
		ID: "agent-listener-rebind", ServerID: "server-1", SecretPrefix: "opaigw_",
		CreatedAt: now, UpdatedAt: now,
	}, auth.HashSecret("agent-secret")); err != nil {
		t.Fatalf("store agent token: %v", err)
	}
	headers := http.Header{"Authorization": []string{"Bearer agent-secret"}}
	plainStream, _, err := websocket.Dial(context.Background(), "ws://"+addr+"/api/agent/v1/stream", &websocket.DialOptions{HTTPHeader: headers})
	if err != nil {
		t.Fatalf("dial plain agent stream: %v", err)
	}
	defer plainStream.CloseNow()
	streamClosed := make(chan error, 1)
	go func() {
		readCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_, _, err := plainStream.Read(readCtx)
		streamClosed <- err
	}()
	fullchain, _, _ := storeGatewayListenerMaterial(t, routes, 12)
	if err := mgr.ensure(context.Background(), srv, addr); err != nil {
		t.Fatalf("enable TLS on same address: %v", err)
	}
	if err := <-streamClosed; websocket.CloseStatus(err) != websocket.StatusGoingAway {
		t.Fatalf("plain stream close status = %v, want GoingAway during plain-to-TLS rebind; err=%v", websocket.CloseStatus(err), err)
	}
	tlsListener := mgr.plain.ln
	if tlsListener == plainListener {
		t.Fatal("first plain-to-TLS transition did not rebind the same address")
	}
	if _, ok := tlsListener.(*sniffListener); !ok || !mgr.plain.tlsEnabled {
		t.Fatalf("listener type=%T tlsEnabled=%v after transition", tlsListener, mgr.plain.tlsEnabled)
	}
	if err := mgr.ensure(context.Background(), srv, addr); err != nil {
		t.Fatalf("second same-address reconcile: %v", err)
	}
	if mgr.plain.ln != tlsListener {
		t.Fatal("second same-address reconcile rebound an already TLS-capable listener")
	}
	getSniffTestBody(t, newSniffTestHTTPClient(t, fullchain), "https://"+addr+"/healthz")
}

func TestAgentListenerPromotionRebindFailureClearsStateAndCanRetry(t *testing.T) {
	srv, routes := buildAgentListenerTLSServer(t)
	mgr := &agentListenerManager{baseCtx: context.Background()}
	t.Cleanup(func() { _ = mgr.ensure(context.Background(), srv, "") })
	addr := freeLoopbackAddr(t)
	if err := mgr.ensure(context.Background(), srv, addr); err != nil {
		t.Fatalf("initial plain bind: %v", err)
	}
	fullchain, fingerprint, notAfter := storeGatewayListenerMaterial(t, routes, 112)

	listenErr := errors.New("injected promotion rebind failure")
	mgr.listen = func(network, address string) (net.Listener, error) {
		if network != "tcp" || address != addr {
			t.Errorf("listen = (%q, %q), want (tcp, %q)", network, address, addr)
		}
		return nil, listenErr
	}
	if err := mgr.ensure(context.Background(), srv, addr); !errors.Is(err, listenErr) {
		t.Fatalf("promotion ensure error = %v, want %v", err, listenErr)
	}
	if mgr.plain.ln != nil || mgr.plain.server != nil || mgr.plain.addr != "" || mgr.plain.tlsEnabled {
		t.Fatalf("failed promotion left manager active: listener=%T server_nil=%v addr=%q tls=%v",
			mgr.plain.ln, mgr.plain.server == nil, mgr.plain.addr, mgr.plain.tlsEnabled)
	}
	if srv.AgentListenerActive() || srv.AgentListenerAddr() != "" {
		t.Fatalf("failed promotion left runtime listener active=%v addr=%q", srv.AgentListenerActive(), srv.AgentListenerAddr())
	}
	if got := srv.AgentListenerTLSState(); got != (gateway.AgentListenerTLSState{}) {
		t.Fatalf("failed promotion left TLS snapshot: %+v", got)
	}

	mgr.listen = nil
	if err := mgr.ensure(context.Background(), srv, addr); err != nil {
		t.Fatalf("retry promotion: %v", err)
	}
	if !srv.AgentListenerActive() || !mgr.plain.tlsEnabled {
		t.Fatalf("retry did not recover TLS listener: active=%v tls=%v", srv.AgentListenerActive(), mgr.plain.tlsEnabled)
	}
	if got := srv.AgentListenerTLSState(); got != (gateway.AgentListenerTLSState{
		Active: true, Address: addr, Fingerprint: hex.EncodeToString(fingerprint[:]), NotAfter: notAfter,
	}) {
		t.Fatalf("retry TLS snapshot = %+v, want recovered listener", got)
	}
	getSniffTestBody(t, newSniffTestHTTPClient(t, fullchain), "https://"+addr+"/healthz")
}

// A same-address TLS promotion is stop-first (no SO_REUSEPORT), so a transient
// re-listen failure must be absorbed by the bounded retry inside enableTLSLocked
// rather than leaving the agent listener down until the next reconcile tick (which
// would transiently reopen the netbird_only public gate). Inject a single bind
// failure and require the manager to recover a live TLS listener in the same call.
func TestAgentListenerPromotionRebindRetryRestoresListener(t *testing.T) {
	srv, routes := buildAgentListenerTLSServer(t)
	mgr := &agentListenerManager{baseCtx: context.Background()}
	t.Cleanup(func() { _ = mgr.ensure(context.Background(), srv, "") })
	addr := freeLoopbackAddr(t)
	if err := mgr.ensure(context.Background(), srv, addr); err != nil {
		t.Fatalf("initial plain bind: %v", err)
	}
	fullchain, fingerprint, notAfter := storeGatewayListenerMaterial(t, routes, 113)

	var calls int
	mgr.listen = func(network, address string) (net.Listener, error) {
		calls++
		if calls == 1 {
			return nil, errors.New("injected transient promotion rebind failure")
		}
		return net.Listen(network, address)
	}
	if err := mgr.ensure(context.Background(), srv, addr); err != nil {
		t.Fatalf("promotion ensure with a transient rebind failure: %v", err)
	}
	if calls < 2 {
		t.Fatalf("bounded retry did not re-attempt the bind (listen calls=%d)", calls)
	}
	if !srv.AgentListenerActive() || !mgr.plain.tlsEnabled {
		t.Fatalf("transient rebind failure was not recovered: active=%v tls=%v", srv.AgentListenerActive(), mgr.plain.tlsEnabled)
	}
	if got := srv.AgentListenerTLSState(); got != (gateway.AgentListenerTLSState{
		Active: true, Address: addr, Fingerprint: hex.EncodeToString(fingerprint[:]), NotAfter: notAfter,
	}) {
		t.Fatalf("recovered TLS snapshot = %+v, want a live TLS listener", got)
	}
	getSniffTestBody(t, newSniffTestHTTPClient(t, fullchain), "https://"+addr+"/healthz")
}

func TestAgentListenerHotSwapDoesNotRebindOrDropOpenConnection(t *testing.T) {
	srv, routes := buildAgentListenerTLSServer(t)
	fullchain1, _, _ := storeGatewayListenerMaterial(t, routes, 13)
	mgr := &agentListenerManager{baseCtx: context.Background()}
	t.Cleanup(func() { _ = mgr.ensure(context.Background(), srv, "") })
	addr := freeLoopbackAddr(t)
	if err := mgr.ensure(context.Background(), srv, addr); err != nil {
		t.Fatalf("initial TLS bind: %v", err)
	}
	listenerBefore := mgr.plain.ln
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("dial long-lived plain connection: %v", err)
	}
	defer conn.Close()
	reader := bufio.NewReader(conn)
	requestOnOpenPlainConnection(t, conn, reader)

	fullchain2, fingerprint2, notAfter2 := storeGatewayListenerMaterial(t, routes, 14)
	if err := mgr.ensure(context.Background(), srv, addr); err != nil {
		t.Fatalf("refresh TLS material: %v", err)
	}
	if mgr.plain.ln != listenerBefore {
		t.Fatal("leaf refresh rebound the listener")
	}
	requestOnOpenPlainConnection(t, conn, reader)
	if got := srv.AgentListenerTLSState(); got.Fingerprint != hex.EncodeToString(fingerprint2[:]) || !got.NotAfter.Equal(notAfter2) {
		t.Fatalf("TLS state after holder refresh = %+v, want fingerprint %x and NotAfter %s", got, fingerprint2, notAfter2)
	}
	if got := dialSniffTestTLSFingerprint(t, addr, newSniffTestRoots(t, fullchain1, fullchain2)); got != fingerprint2 {
		t.Fatalf("TLS leaf after hot-swap = %x, want %x", got, fingerprint2)
	}
}

func TestAgentListenerWSSSurvivesHolderOnlyHotSwap(t *testing.T) {
	srv, routes := buildAgentListenerTLSServer(t)
	const (
		serverID = "wss-hot-swap-server"
		secret   = "wss-hot-swap-secret"
	)
	seedAgentListenerToken(t, routes, serverID, secret)
	fullchain1, _, _ := storeGatewayListenerMaterial(t, routes, 113)
	mgr := &agentListenerManager{baseCtx: context.Background()}
	t.Cleanup(func() { _ = mgr.ensure(context.Background(), srv, "") })
	addr := freeLoopbackAddr(t)
	if err := mgr.ensure(context.Background(), srv, addr); err != nil {
		t.Fatalf("initial TLS bind: %v", err)
	}
	listenerBefore := mgr.plain.ln

	headers := http.Header{"Authorization": []string{"Bearer " + secret}}
	stream, _, err := websocket.Dial(context.Background(), "wss://"+addr+"/api/agent/v1/stream", &websocket.DialOptions{
		HTTPClient: newSniffTestHTTPClient(t, fullchain1),
		HTTPHeader: headers,
	})
	if err != nil {
		t.Fatalf("dial initial WSS stream: %v", err)
	}
	defer stream.CloseNow()
	streamClosed := stream.CloseRead(context.Background())

	fullchain2, fingerprint2, _ := storeGatewayListenerMaterial(t, routes, 114)
	if err := mgr.ensure(context.Background(), srv, addr); err != nil {
		t.Fatalf("holder-only refresh: %v", err)
	}
	if mgr.plain.ln != listenerBefore {
		t.Fatal("holder-only refresh rebound the TLS listener")
	}
	writeCtx, cancelWrite := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelWrite()
	if err := wsjson.Write(writeCtx, stream, struct {
		Type string          `json:"type"`
		Data json.RawMessage `json:"data"`
	}{Type: "telemetry", Data: json.RawMessage(`{"host":{"cpu_util_pct":42}}`)}); err != nil {
		t.Fatalf("existing WSS stream was closed by holder refresh: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		telemetry, ok, err := routes.TelemetryByServer(context.Background(), serverID)
		if err != nil {
			t.Fatalf("read telemetry after holder refresh: %v", err)
		}
		if ok && telemetry.CPULoad == 0.42 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("existing WSS stream did not ingest after holder refresh: present=%v cpu_load=%v", ok, telemetry.CPULoad)
		}
		time.Sleep(10 * time.Millisecond)
	}
	pingCtx, cancelPing := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelPing()
	if err := stream.Ping(pingCtx); err != nil {
		t.Fatalf("existing WSS stream was not live after holder refresh: %v", err)
	}
	select {
	case <-streamClosed.Done():
		t.Fatal("existing WSS stream closed during holder refresh")
	default:
	}
	if got := dialSniffTestTLSFingerprint(t, addr, newSniffTestRoots(t, fullchain1, fullchain2)); got != fingerprint2 {
		t.Fatalf("new TLS dial leaf after WSS-preserving swap = %x, want %x", got, fingerprint2)
	}
}

func TestAgentListenerBrokenRefreshKeepsLastGoodTLSState(t *testing.T) {
	srv, routes := buildAgentListenerTLSServer(t)
	fullchain, fingerprint, _ := storeGatewayListenerMaterial(t, routes, 15)
	mgr := &agentListenerManager{baseCtx: context.Background()}
	t.Cleanup(func() { _ = mgr.ensure(context.Background(), srv, "") })
	addr := freeLoopbackAddr(t)
	if err := mgr.ensure(context.Background(), srv, addr); err != nil {
		t.Fatalf("initial TLS bind: %v", err)
	}
	listenerBefore := mgr.plain.ln
	stateBefore := srv.AgentListenerTLSState()
	row, err := routes.CertificateByDomain(context.Background(), "gateway.int.example.test")
	if err != nil {
		t.Fatalf("load gateway row: %v", err)
	}
	row.KeySealed = "enc:AAAA"
	row.UpdatedAt = time.Now().UTC()
	if err := routes.UpsertCertificate(context.Background(), row); err != nil {
		t.Fatalf("store broken gateway refresh: %v", err)
	}
	if err := mgr.ensure(context.Background(), srv, addr); err != nil {
		t.Fatalf("broken material refresh must preserve the serving listener, got: %v", err)
	}
	if mgr.plain.ln != listenerBefore || !mgr.plain.tlsEnabled {
		t.Fatalf("broken refresh changed listener=%v or tlsEnabled=%v", mgr.plain.ln != listenerBefore, mgr.plain.tlsEnabled)
	}
	if got := srv.AgentListenerTLSState(); got != stateBefore {
		t.Fatalf("broken refresh changed last-good TLS state: got %+v want %+v", got, stateBefore)
	}
	if got := dialSniffTestTLSFingerprint(t, addr, newSniffTestRoots(t, fullchain)); got != fingerprint {
		t.Fatalf("broken refresh changed served leaf: got %x want %x", got, fingerprint)
	}
}

func TestAgentListenerMissingOrExpiredRefreshKeepsLastGoodTLSState(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(t *testing.T, routes routing.Store)
	}{
		{name: "deleted", mutate: func(t *testing.T, routes routing.Store) {
			if err := routes.DeleteCertificate(context.Background(), "gateway.int.example.test"); err != nil {
				t.Fatalf("delete gateway row: %v", err)
			}
		}},
		{name: "expired", mutate: func(t *testing.T, routes routing.Store) {
			row, err := routes.CertificateByDomain(context.Background(), "gateway.int.example.test")
			if err != nil {
				t.Fatalf("load gateway row: %v", err)
			}
			row.NotAfter = time.Now().UTC().Add(-time.Minute)
			row.UpdatedAt = time.Now().UTC()
			if err := routes.UpsertCertificate(context.Background(), row); err != nil {
				t.Fatalf("expire gateway row: %v", err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, routes := buildAgentListenerTLSServer(t)
			fullchain, fingerprint, _ := storeGatewayListenerMaterial(t, routes, 115)
			mgr := &agentListenerManager{baseCtx: context.Background()}
			t.Cleanup(func() { _ = mgr.ensure(context.Background(), srv, "") })
			addr := freeLoopbackAddr(t)
			if err := mgr.ensure(context.Background(), srv, addr); err != nil {
				t.Fatalf("initial TLS bind: %v", err)
			}
			listenerBefore := mgr.plain.ln
			stateBefore := srv.AgentListenerTLSState()
			holderBefore, err := mgr.plain.holder.GetCertificate(nil)
			if err != nil {
				t.Fatalf("load initial holder: %v", err)
			}

			tc.mutate(t, routes)
			if err := mgr.ensure(context.Background(), srv, addr); err != nil {
				t.Fatalf("refresh after %s row: %v", tc.name, err)
			}
			if mgr.plain.ln != listenerBefore || !mgr.plain.tlsEnabled {
				t.Fatalf("%s row changed listener_same=%v tls=%v", tc.name, mgr.plain.ln == listenerBefore, mgr.plain.tlsEnabled)
			}
			if got := srv.AgentListenerTLSState(); got != stateBefore {
				t.Fatalf("%s row changed TLS snapshot: got %+v want %+v", tc.name, got, stateBefore)
			}
			holderAfter, err := mgr.plain.holder.GetCertificate(nil)
			if err != nil {
				t.Fatalf("load holder after %s row: %v", tc.name, err)
			}
			if holderAfter != holderBefore {
				t.Fatalf("%s row replaced the last-good holder", tc.name)
			}
			if got := dialSniffTestTLSFingerprint(t, addr, newSniffTestRoots(t, fullchain)); got != fingerprint {
				t.Fatalf("%s row changed served leaf: got %x want %x", tc.name, got, fingerprint)
			}
		})
	}
}

func TestAgentListenerAddressChangeWithBrokenRefreshKeepsLastGoodTLS(t *testing.T) {
	srv, routes := buildAgentListenerTLSServer(t)
	fullchain, fingerprint, notAfter := storeGatewayListenerMaterial(t, routes, 16)
	mgr := &agentListenerManager{baseCtx: context.Background()}
	t.Cleanup(func() { _ = mgr.ensure(context.Background(), srv, "") })
	addr1 := freeLoopbackAddr(t)
	if err := mgr.ensure(context.Background(), srv, addr1); err != nil {
		t.Fatalf("initial TLS bind: %v", err)
	}
	row, err := routes.CertificateByDomain(context.Background(), "gateway.int.example.test")
	if err != nil {
		t.Fatalf("load gateway row: %v", err)
	}
	row.KeySealed = "enc:AAAA"
	row.UpdatedAt = time.Now().UTC()
	if err := routes.UpsertCertificate(context.Background(), row); err != nil {
		t.Fatalf("store broken gateway refresh: %v", err)
	}

	addr2 := freeLoopbackAddr(t)
	if err := mgr.ensure(context.Background(), srv, addr2); err != nil {
		t.Fatalf("move TLS listener with broken refresh: %v", err)
	}
	if got := srv.AgentListenerTLSState(); got != (gateway.AgentListenerTLSState{
		Active: true, Address: addr2, Fingerprint: hex.EncodeToString(fingerprint[:]), NotAfter: notAfter,
	}) {
		t.Fatalf("TLS state after address move = %+v, want last-good leaf on %q", got, addr2)
	}
	if got := dialSniffTestTLSFingerprint(t, addr2, newSniffTestRoots(t, fullchain)); got != fingerprint {
		t.Fatalf("moved listener leaf = %x, want last-good %x", got, fingerprint)
	}
	ln, err := net.Listen("tcp", addr1)
	if err != nil {
		t.Fatalf("old address %q remains bound after fail-safe move: %v", addr1, err)
	}
	_ = ln.Close()
}

// TestResolveAgentAddr asserts the (addr, ok) contract that keeps a valid agent
// listener alive across a transient control-plane blip:
//   - explicit AgentAddr                      -> (that, true)
//   - resolve ERROR (peer id set, module off) -> ("", false) "don't touch"
//   - no AgentAddr + no peer id (unconfigured) -> ("", true)  "no listener wanted"
func TestResolveAgentAddr(t *testing.T) {
	t.Run("explicit agent addr", func(t *testing.T) {
		srv := buildAgentListenerServer(t, map[string]string{})
		addr, ok := resolveAgentAddr(context.Background(), srv, config.Config{AgentAddr: "127.0.0.1:8081", AgentPort: "8081"})
		if addr != "127.0.0.1:8081" || !ok {
			t.Fatalf("resolveAgentAddr(explicit) = (%q, %v), want (\"127.0.0.1:8081\", true)", addr, ok)
		}
	})

	t.Run("resolve error keeps current listener (ok=false)", func(t *testing.T) {
		// A gateway peer id is set but the NetBird module is off -> ResolveGatewayPeerIP
		// returns ErrNetbirdModuleDisabled: a transient/control-plane failure. ok MUST be
		// false so the caller does NOT tear the current listener down.
		srv := buildAgentListenerServer(t, map[string]string{
			"netbird_gateway_peer_id": "gwpeer",
		})
		addr, ok := resolveAgentAddr(context.Background(), srv, config.Config{AgentAddr: "", AgentPort: "8081"})
		if addr != "" || ok {
			t.Fatalf("resolveAgentAddr(resolve error) = (%q, %v), want (\"\", false)", addr, ok)
		}
	})

	t.Run("unconfigured wants no listener (ok=true)", func(t *testing.T) {
		srv := buildAgentListenerServer(t, map[string]string{})
		addr, ok := resolveAgentAddr(context.Background(), srv, config.Config{AgentAddr: "", AgentPort: "8081"})
		if addr != "" || !ok {
			t.Fatalf("resolveAgentAddr(unconfigured) = (%q, %v), want (\"\", true)", addr, ok)
		}
	})
}

// TestEffectiveAgentTLSPortValidatesAndNeverDiverges is the FINDING-3 regression:
// effectiveAgentTLSPort must return a single VALIDATED port that the panel/policy
// dep (atoiOr over it) and the bind (resolveAgentTLSAddr, via this helper) both
// use -- so a malformed AGENT_TLS_PORT can never make the panel/policy advertise
// a port that nothing binds. An unset (empty) value keeps the 8443 default.
func TestEffectiveAgentTLSPortValidatesAndNeverDiverges(t *testing.T) {
	cases := []struct {
		name     string
		tlsPort  string
		tlsAddr  string
		wantPort string
		wantInt  int
	}{
		{"config default", "8443", "", "8443", 8443},
		{"explicit valid", "9443", "", "9443", 9443},
		{"malformed falls back", "84a3", "", "8443", 8443},
		{"non-positive falls back", "0", "", "8443", 8443},
		{"out-of-range falls back", "70000", "", "8443", 8443},
		{"empty falls back", "", "", "8443", 8443},
		{"whitespace falls back", "  ", "", "8443", 8443},
		{"explicit addr port wins", "8443", "127.0.0.1:9999", "9999", 9999},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Config{AgentTLSPort: tc.tlsPort, AgentTLSAddr: tc.tlsAddr}
			if got := effectiveAgentTLSPort(cfg); got != tc.wantPort {
				t.Fatalf("effectiveAgentTLSPort = %q, want %q", got, tc.wantPort)
			}
			// The value the panel/policy advertise (atoiOr over the helper) must equal
			// the port the bind resolves to -- never diverge.
			if gotInt := atoiOr(effectiveAgentTLSPort(cfg), 8443); gotInt != tc.wantInt {
				t.Fatalf("panel/policy port = %d, want %d", gotInt, tc.wantInt)
			}
			// The bind path (resolveAgentTLSAddr with no explicit AGENT_TLS_ADDR) must
			// resolve to the SAME validated port, so the advertised and bound ports agree.
			if tc.tlsAddr == "" {
				srv := gateway.New(gateway.ServerDeps{Portal: &fakeModePortal{peerIP: "100.64.0.5"}})
				gotAddr, ok := resolveAgentTLSAddr(context.Background(), srv, cfg)
				if !ok {
					t.Fatalf("resolveAgentTLSAddr ok=false, want a resolved addr")
				}
				if wantAddr := net.JoinHostPort("100.64.0.5", tc.wantPort); gotAddr != wantAddr {
					t.Fatalf("resolveAgentTLSAddr = %q, want %q (bind port diverges from the advertised port)", gotAddr, wantAddr)
				}
			}
		})
	}
}

// TestAgentListenerReconcileKeepsListenerOnResolveError proves the regression fix:
// a bound listener is NOT torn down when the reconcile step's resolve returns
// !ok (a transient NetBird control-plane error). It binds a listener, then applies
// the loop's exact "skip ensure on !ok" logic against a Portal that errors, and
// asserts the listener survives.
func TestAgentListenerReconcileKeepsListenerOnResolveError(t *testing.T) {
	// Peer id set + module off -> ResolveGatewayPeerIP errors -> resolveAgentAddr !ok.
	srv := buildAgentListenerServer(t, map[string]string{
		"netbird_gateway_peer_id": "gwpeer",
	})
	mgr := &agentListenerManager{baseCtx: context.Background()}
	good := freeLoopbackAddr(t)
	if err := mgr.ensure(context.Background(), srv, good); err != nil {
		t.Fatalf("initial bind: %v", err)
	}
	if !srv.AgentListenerActive() || srv.AgentListenerAddr() != good {
		t.Fatalf("precondition: want active on %q, got active=%v addr=%q", good, srv.AgentListenerActive(), srv.AgentListenerAddr())
	}

	// The loop's tick body: resolve returns !ok, so ensure must be skipped.
	if addr, ok := resolveAgentAddr(context.Background(), srv, config.Config{AgentAddr: "", AgentPort: "8081"}); ok {
		_ = mgr.ensure(context.Background(), srv, addr)
	}

	if !srv.AgentListenerActive() || srv.AgentListenerAddr() != good {
		t.Fatalf("a transient resolve error tore down the listener: active=%v addr=%q, want active on %q", srv.AgentListenerActive(), srv.AgentListenerAddr(), good)
	}
}

// TestStartAgentListener exercises the NetBird agent-listener bind resolution +
// its fail-safe behavior across the bind-address precedence branches. Success
// leaks the serving goroutine (no shutdown hook); each subtest uses a distinct
// free port so the leaked listeners never collide.
func TestStartAgentListener(t *testing.T) {
	t.Run("no-op: no agent addr and no gateway peer id", func(t *testing.T) {
		srv := buildAgentListenerServer(t, map[string]string{})

		startAgentListener(context.Background(), srv, config.Config{AgentAddr: "", AgentPort: "8081"})

		if srv.AgentListenerActive() {
			t.Fatalf("AgentListenerActive = true, want false (no agent addr, no gateway peer id)")
		}
	})

	t.Run("explicit agent addr wins over gateway peer resolution", func(t *testing.T) {
		addr := net.JoinHostPort("127.0.0.1", freeTCPPort(t))
		// A gateway peer id is set AND the module is off, so peer resolution WOULD
		// error (ErrNetbirdModuleDisabled) — but the explicit addr must win before
		// resolution is even attempted. If the `if agentAddr==""` precedence guard
		// were removed (always-resolve), the module-off error would return early and
		// leave the listener inactive, failing this test.
		srv := buildAgentListenerServer(t, map[string]string{
			"netbird_gateway_peer_id": "gwpeer",
			// netbird_enabled absent -> module off -> ResolveGatewayPeerIP errors.
		})

		startAgentListener(context.Background(), srv, config.Config{AgentAddr: addr, AgentPort: "9999"})

		if !srv.AgentListenerActive() {
			t.Fatalf("AgentListenerActive = false, want true (explicit addr must bind)")
		}
		if srv.AgentListenerAddr() != addr {
			t.Fatalf("AgentListenerAddr = %q, want %q (explicit addr, peer resolution skipped)", srv.AgentListenerAddr(), addr)
		}
	})

	t.Run("resolved local peer binds the netbird IP", func(t *testing.T) {
		fake := newFakeNetbird()
		fake.peers["gwpeer"] = netbird.Peer{ID: "gwpeer", IP: "127.0.0.1"}
		ts := httptest.NewServer(fake.handler())
		defer ts.Close()
		port := freeTCPPort(t)
		srv := buildAgentListenerServer(t, map[string]string{
			"netbird_enabled":         "true",
			"netbird_url":             ts.URL,
			"netbird_token":           "plain:test-token",
			"netbird_gateway_peer_id": "gwpeer",
		})

		startAgentListener(context.Background(), srv, config.Config{AgentAddr: "", AgentPort: port})

		if !srv.AgentListenerActive() {
			t.Fatalf("AgentListenerActive = false, want true (local peer IP must bind)")
		}
		want := net.JoinHostPort("127.0.0.1", port)
		if srv.AgentListenerAddr() != want {
			t.Fatalf("AgentListenerAddr = %q, want %q (resolved local peer IP + AgentPort)", srv.AgentListenerAddr(), want)
		}
	})

	t.Run("module off leaves the listener inactive (fail-safe)", func(t *testing.T) {
		// A gateway peer id is set but the NetBird module is off -> ResolveGatewayPeerIP
		// returns ErrNetbirdModuleDisabled -> fail-safe: no listener, no crash.
		srv := buildAgentListenerServer(t, map[string]string{
			"netbird_gateway_peer_id": "gwpeer",
		})

		startAgentListener(context.Background(), srv, config.Config{AgentAddr: "", AgentPort: freeTCPPort(t)})

		if srv.AgentListenerActive() {
			t.Fatalf("AgentListenerActive = true, want false (module off must fail safe)")
		}
	})

	t.Run("wrong-peer non-local IP bind fails safely", func(t *testing.T) {
		fake := newFakeNetbird()
		// TEST-NET-3 (203.0.113.0/24) is never a local interface: net.Listen fails
		// with "cannot assign requested address" -> fail-safe inactive, no crash.
		fake.peers["gwpeer"] = netbird.Peer{ID: "gwpeer", IP: "203.0.113.1"}
		ts := httptest.NewServer(fake.handler())
		defer ts.Close()
		srv := buildAgentListenerServer(t, map[string]string{
			"netbird_enabled":         "true",
			"netbird_url":             ts.URL,
			"netbird_token":           "plain:test-token",
			"netbird_gateway_peer_id": "gwpeer",
		})

		startAgentListener(context.Background(), srv, config.Config{AgentAddr: "", AgentPort: freeTCPPort(t)})

		if srv.AgentListenerActive() {
			t.Fatalf("AgentListenerActive = true, want false (non-local IP bind must fail safe)")
		}
	})
}
