// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package hostpower

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeCounters map[string]uint64

func (f fakeCounters) LiveCounter(vmid string) (uint64, bool) {
	v, ok := f[vmid]
	return v, ok
}

type fakeResolver struct {
	mu    sync.Mutex
	vmid  string
	err   error
	calls int
}

func (f *fakeResolver) Resolve(_ context.Context, _, _ netip.AddrPort) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.vmid, f.err
}

func (f *fakeResolver) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

type fakeTokens map[string]string

func (f fakeTokens) Lookup(tok string) (string, bool) {
	v, ok := f[tok]
	return v, ok
}

type srvRecorder struct {
	mu   sync.Mutex
	recs []slog.Record
}

func (h *srvRecorder) Enabled(context.Context, slog.Level) bool { return true }
func (h *srvRecorder) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.recs = append(h.recs, r)
	return nil
}
func (h *srvRecorder) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *srvRecorder) WithGroup(string) slog.Handler      { return h }

func (h *srvRecorder) dump() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []string
	for _, r := range h.recs {
		s := r.Level.String() + " " + r.Message
		r.Attrs(func(a slog.Attr) bool {
			s += fmt.Sprintf(" %s=%v", a.Key, a.Value)
			return true
		})
		out = append(out, s)
	}
	return out
}

func (h *srvRecorder) count(level slog.Level) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, r := range h.recs {
		if r.Level == level {
			n++
		}
	}
	return n
}

type testRig struct {
	h        http.Handler
	resolver *fakeResolver
	logs     *srvRecorder
	clock    *time.Time
}

func newRig(modes AuthModes) *testRig {
	t0 := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	rig := &testRig{
		resolver: &fakeResolver{vmid: "101"},
		logs:     &srvRecorder{},
		clock:    &t0,
	}
	rig.h = newHandler(modes,
		fakeCounters{"100": 7, "101": 9},
		rig.resolver,
		fakeTokens{"ophp_secret": "100", "ophp_other": "101"},
		slog.New(rig.logs),
		func() time.Time { return *rig.clock },
	)
	return rig
}

func (r *testRig) do(method, path, remote, auth string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	req.RemoteAddr = remote
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	ctx := context.WithValue(req.Context(), http.LocalAddrContextKey,
		net.Addr(&net.TCPAddr{IP: net.ParseIP("192.0.2.1"), Port: 8080}))
	rec := httptest.NewRecorder()
	r.h.ServeHTTP(rec, req.WithContext(ctx))
	return rec
}

const energyPath = "/v1/energy"

func TestHandlerTokenModeServesOwnVM(t *testing.T) {
	rig := newRig(AuthModes{MAC: true, Token: true})
	rec := rig.do(http.MethodGet, energyPath, "192.0.2.10:4000", "Bearer ophp_secret")
	if rec.Code != 200 {
		t.Fatalf("code = %d", rec.Code)
	}
	want := `{"vmid":"100","energy_uj":7,"max_energy_range_uj":0}` + "\n"
	if rec.Body.String() != want {
		t.Fatalf("body = %q", rec.Body.String())
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("cache-control = %q", rec.Header().Get("Cache-Control"))
	}
	if rec.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("content-type = %q", rec.Header().Get("Content-Type"))
	}
	if rig.resolver.callCount() != 0 {
		t.Fatal("resolver used in token mode")
	}
}

func TestHandlerBearerSchemeCaseInsensitive(t *testing.T) {
	rig := newRig(AuthModes{Token: true})
	if rec := rig.do(http.MethodGet, energyPath, "192.0.2.10:4000", "bEaReR ophp_secret"); rec.Code != 200 {
		t.Fatalf("code = %d", rec.Code)
	}
}

func TestHandlerBadTokenNoMACFallback(t *testing.T) {
	rig := newRig(AuthModes{MAC: true, Token: true})
	for _, auth := range []string{"Bearer wrong", "Bearer ", "Basic ophp_secret", "ophp_secret"} {
		rec := rig.do(http.MethodGet, energyPath, "192.0.2.10:4000", auth)
		if rec.Code != 401 {
			t.Fatalf("auth %q: code = %d", auth, rec.Code)
		}
	}
	if rig.resolver.callCount() != 0 {
		t.Fatal("MAC fallback after a presented credential")
	}
}

func TestHandlerTokenModeDisabled(t *testing.T) {
	rig := newRig(AuthModes{MAC: true})
	rec := rig.do(http.MethodGet, energyPath, "192.0.2.10:4000", "Bearer ophp_secret")
	if rec.Code != 401 {
		t.Fatalf("code = %d", rec.Code)
	}
	if rig.resolver.callCount() != 0 {
		t.Fatal("resolver used")
	}
}

func TestHandlerMACMode(t *testing.T) {
	rig := newRig(AuthModes{MAC: true, Token: true})
	rec := rig.do(http.MethodGet, energyPath, "192.0.2.10:4000", "")
	if rec.Code != 200 {
		t.Fatalf("code = %d", rec.Code)
	}
	want := `{"vmid":"101","energy_uj":9,"max_energy_range_uj":0}` + "\n"
	if rec.Body.String() != want {
		t.Fatalf("body = %q", rec.Body.String())
	}
}

func TestHandlerMACModeResolveFailure(t *testing.T) {
	rig := newRig(AuthModes{MAC: true})
	rig.resolver.err = &ResolveError{Reason: "no observation"}
	rig.resolver.vmid = ""
	rec := rig.do(http.MethodGet, energyPath, "192.0.2.10:4000", "")
	if rec.Code != 401 || strings.Contains(rec.Body.String(), "observation") {
		t.Fatalf("code = %d body = %q", rec.Code, rec.Body.String())
	}
}

func TestHandlerMACModeDisabled(t *testing.T) {
	rig := newRig(AuthModes{Token: true})
	rec := rig.do(http.MethodGet, energyPath, "192.0.2.10:4000", "")
	if rec.Code != 401 {
		t.Fatalf("code = %d", rec.Code)
	}
	if rig.resolver.callCount() != 0 {
		t.Fatal("resolver called")
	}
}

func TestHandlerBadConnectionAddress(t *testing.T) {
	rig := newRig(AuthModes{MAC: true})
	rec := rig.do(http.MethodGet, energyPath, "not-an-addr", "")
	if rec.Code != 401 {
		t.Fatalf("code = %d", rec.Code)
	}
}

func TestHandlerResolverErrorNotResolveError(t *testing.T) {
	rig := newRig(AuthModes{MAC: true})
	rig.resolver.err = errors.New("boom")
	rec := rig.do(http.MethodGet, energyPath, "192.0.2.10:4000", "")
	if rec.Code != 401 {
		t.Fatalf("code = %d", rec.Code)
	}
}

func TestHandlerNotLive(t *testing.T) {
	rig := newRig(AuthModes{MAC: true})
	rig.resolver.vmid = "999"
	rec := rig.do(http.MethodGet, energyPath, "192.0.2.10:4000", "")
	if rec.Code != 503 {
		t.Fatalf("code = %d", rec.Code)
	}
}

func TestHandlerMethodAndPath(t *testing.T) {
	rig := newRig(AuthModes{MAC: true, Token: true})
	if rec := rig.do(http.MethodPost, energyPath, "192.0.2.10:4000", ""); rec.Code != 405 {
		t.Fatalf("POST code = %d", rec.Code)
	}
	if rec := rig.do(http.MethodGet, "/x", "192.0.2.10:4000", ""); rec.Code != 404 {
		t.Fatalf("GET /x code = %d", rec.Code)
	}
}

// warnsWith returns the Warn lines that contain all of parts.
func (h *srvRecorder) warnsWith(parts ...string) []string {
	var out []string
	for _, l := range h.dump() {
		if !strings.HasPrefix(l, "WARN ") {
			continue
		}
		match := true
		for _, p := range parts {
			match = match && strings.Contains(l, p)
		}
		if match {
			out = append(out, l)
		}
	}
	return out
}

func TestHandlerVMRateLimit(t *testing.T) {
	rig := newRig(AuthModes{MAC: true, Token: true})
	for i := 1; i <= 25; i++ {
		code := rig.do(http.MethodGet, energyPath, "192.0.2.10:4000", "Bearer ophp_secret").Code
		if want := map[bool]int{true: 200, false: 429}[i <= 20]; code != want {
			t.Fatalf("request %d: code = %d, want %d", i, code, want)
		}
	}
	if got := rig.logs.warnsWith("reason=rate limited (vm)", "vmid=100", "remote=192.0.2.10"); len(got) != 1 {
		t.Fatalf("vm 429 warns = %v, want one: %v", got, rig.logs.dump())
	}
	// Another VM from the same address has its own bucket.
	if rec := rig.do(http.MethodGet, energyPath, "192.0.2.10:4000", "Bearer ophp_other"); rec.Code != 200 {
		t.Fatalf("other VM code = %d", rec.Code)
	}
	*rig.clock = rig.clock.Add(time.Second)
	if rec := rig.do(http.MethodGet, energyPath, "192.0.2.10:4000", "Bearer ophp_secret"); rec.Code != 200 {
		t.Fatalf("after refill code = %d", rec.Code)
	}
}

func TestHandlerVMRateLimitAppliesInMACMode(t *testing.T) {
	rig := newRig(AuthModes{MAC: true})
	var last int
	for range 21 {
		last = rig.do(http.MethodGet, energyPath, "192.0.2.10:4000", "").Code
	}
	if last != 429 {
		t.Fatalf("21st request code = %d", last)
	}
	// The VM, not the address, is limited: VM 101 from another address too.
	if rec := rig.do(http.MethodGet, energyPath, "192.0.2.11:4000", ""); rec.Code != 429 {
		t.Fatalf("same VM from other IP code = %d", rec.Code)
	}
}

func TestHandlerSourceRateLimit(t *testing.T) {
	rig := newRig(AuthModes{MAC: true, Token: true})
	for i := 1; i <= 201; i++ {
		code := rig.do(http.MethodGet, energyPath, "192.0.2.10:4000", "Bearer wrong").Code
		if want := map[bool]int{true: 401, false: 429}[i <= 200]; code != want {
			t.Fatalf("request %d: code = %d, want %d", i, code, want)
		}
	}
	if got := rig.logs.warnsWith("reason=rate limited (source)", "remote=192.0.2.10"); len(got) != 1 {
		t.Fatalf("source 429 warns = %v, want one: %v", got, rig.logs.dump())
	}
	// A source-limited request never reaches identification.
	rig2 := newRig(AuthModes{MAC: true})
	for range 201 {
		rig2.do(http.MethodGet, "/x", "192.0.2.10:4000", "")
	}
	if rec := rig2.do(http.MethodGet, energyPath, "192.0.2.10:4000", ""); rec.Code != 429 {
		t.Fatalf("code = %d", rec.Code)
	}
	if got := rig2.resolver.callCount(); got != 0 {
		t.Fatalf("resolver calls = %d, want 0", got)
	}
	if rec := rig2.do(http.MethodGet, energyPath, "192.0.2.11:4000", ""); rec.Code != 200 {
		t.Fatalf("other IP code = %d", rec.Code)
	}
}

func TestHandlerManyVMsBehindOneAddressNeverLimited(t *testing.T) {
	// Token mode behind NAT: two guests share one source address and poll
	// at 1 Hz each.
	rig := newRig(AuthModes{Token: true})
	for sec := range 600 {
		for _, tok := range []string{"ophp_secret", "ophp_other"} {
			if rec := rig.do(http.MethodGet, energyPath, "192.0.2.10:4000", "Bearer "+tok); rec.Code != 200 {
				t.Fatalf("second %d, %s: code = %d", sec, tok, rec.Code)
			}
		}
		*rig.clock = rig.clock.Add(time.Second)
	}
	if n := rig.logs.count(slog.LevelWarn) + rig.logs.count(slog.LevelInfo); n != 0 {
		t.Fatalf("logs above Debug for successful requests: %v", rig.logs.dump())
	}
}

func TestHandlerRejectionLoggedOncePerTenMinutes(t *testing.T) {
	rig := newRig(AuthModes{Token: true})
	t0 := *rig.clock
	for range 50 {
		rig.do(http.MethodGet, energyPath, "192.0.2.10:4000", "Bearer wrong")
		*rig.clock = rig.clock.Add(10 * time.Second)
	}
	if got := rig.logs.warnsWith("reason=unknown token"); len(got) != 1 {
		t.Fatalf("warns = %v, want 1", got)
	}
	// Interleaved successes do not reset the suppression (no flip-flop).
	rig.do(http.MethodGet, energyPath, "192.0.2.10:4000", "Bearer ophp_secret")
	rig.do(http.MethodGet, energyPath, "192.0.2.10:4000", "Bearer wrong")
	if n := rig.logs.count(slog.LevelWarn); n != 1 {
		t.Fatalf("warns = %d, want 1: %v", n, rig.logs.dump())
	}
	if n := rig.logs.count(slog.LevelInfo); n != 0 {
		t.Fatalf("infos = %d, want 0: %v", n, rig.logs.dump())
	}
	// Another reason or another address is its own stream.
	rig.do(http.MethodGet, energyPath, "192.0.2.10:4000", "Basic x")
	rig.do(http.MethodGet, energyPath, "192.0.2.11:4000", "Bearer wrong")
	if n := rig.logs.count(slog.LevelWarn); n != 3 {
		t.Fatalf("warns = %d, want 3: %v", n, rig.logs.dump())
	}
	*rig.clock = t0.Add(rejectLogEvery)
	rig.do(http.MethodGet, energyPath, "192.0.2.10:4000", "Bearer wrong")
	if got := rig.logs.warnsWith("reason=unknown token", "remote=192.0.2.10"); len(got) != 2 {
		t.Fatalf("warns after 10 minutes = %v, want 2", got)
	}
}

func TestHandlerBoundsLogState(t *testing.T) {
	rig := newRig(AuthModes{Token: true})
	hd := rig.h.(*handler)
	for i := range maxLogKeys + 50 {
		ip := fmt.Sprintf("10.%d.%d.%d:1", (i>>16)&255, (i>>8)&255, i&255)
		rig.do(http.MethodGet, energyPath, ip, "Bearer wrong")
	}
	hd.mu.Lock()
	n := len(hd.logged)
	hd.mu.Unlock()
	if n > maxLogKeys {
		t.Fatalf("log keys = %d, exceeds %d", n, maxLogKeys)
	}
	*rig.clock = rig.clock.Add(rejectLogEvery)
	rig.do(http.MethodGet, energyPath, "192.0.2.10:4000", "Bearer wrong")
	hd.mu.Lock()
	n = len(hd.logged)
	hd.mu.Unlock()
	if n != 1 {
		t.Fatalf("log keys = %d after expiry, want 1", n)
	}
}

func TestHandlerNeverLogsToken(t *testing.T) {
	rig := newRig(AuthModes{MAC: true, Token: true})
	rig.do(http.MethodGet, energyPath, "192.0.2.10:4000", "Bearer ophp_secret")
	rig.do(http.MethodGet, energyPath, "192.0.2.10:4000", "Bearer ophp_secret_wrong")
	rig.do(http.MethodGet, energyPath, "192.0.2.10:4000", "Bearer ophp_secret")
	for _, l := range rig.logs.dump() {
		if strings.Contains(l, "ophp_secret") || strings.Contains(strings.ToLower(l), "bearer") {
			t.Fatalf("log leaks credential: %s", l)
		}
	}
	if len(rig.logs.dump()) == 0 {
		t.Fatal("expected rejection logs")
	}
}

func TestHandlerEvictsIdleSourceState(t *testing.T) {
	rig := newRig(AuthModes{MAC: true})
	rig.do(http.MethodGet, energyPath, "192.0.2.10:4000", "")
	*rig.clock = rig.clock.Add(2 * time.Minute)
	rig.do(http.MethodGet, energyPath, "192.0.2.11:4000", "")
	hd := rig.h.(*handler)
	hd.mu.Lock()
	defer hd.mu.Unlock()
	if len(hd.sources.buckets) != 1 {
		t.Fatalf("sources = %d, want 1 after idle eviction", len(hd.sources.buckets))
	}
}

func TestHandlerBoundsSourceState(t *testing.T) {
	rig := newRig(AuthModes{MAC: true})
	hd := rig.h.(*handler)
	for i := range maxSources + 50 {
		ip := fmt.Sprintf("10.%d.%d.%d:1", (i>>16)&255, (i>>8)&255, i&255)
		rig.do(http.MethodGet, energyPath, ip, "")
		*rig.clock = rig.clock.Add(time.Millisecond)
	}
	hd.mu.Lock()
	defer hd.mu.Unlock()
	if len(hd.sources.buckets) > maxSources {
		t.Fatalf("sources = %d, exceeds %d", len(hd.sources.buckets), maxSources)
	}
}

func TestParseAuthModes(t *testing.T) {
	for in, want := range map[string]AuthModes{
		"mac,token": {MAC: true, Token: true},
		"token":     {Token: true},
		"mac":       {MAC: true},
	} {
		got, err := ParseAuthModes(in)
		if err != nil || got != want {
			t.Fatalf("%q: %+v %v", in, got, err)
		}
	}
	for _, in := range []string{"", "foo", "mac,foo", "mac,,token"} {
		if _, err := ParseAuthModes(in); err == nil {
			t.Fatalf("%q: expected error", in)
		}
	}
}

func TestNewServerSettingsAndNoKeepAlive(t *testing.T) {
	srv := NewServer("127.0.0.1:0", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	if srv.ReadHeaderTimeout != 2*time.Second || srv.ReadTimeout != 5*time.Second ||
		srv.WriteTimeout != 5*time.Second || srv.IdleTimeout != 30*time.Second ||
		srv.MaxHeaderBytes != 4096 {
		t.Fatalf("server settings wrong: %+v", srv)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(ln) }()
	defer srv.Close()

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	_, _ = io.WriteString(conn, "GET / HTTP/1.1\r\nHost: x\r\n\r\n")
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if !resp.Close {
		t.Fatalf("response not Connection: close: %+v", resp.Header)
	}
}

func TestHandlerEmptyAuthorizationHeaderIsTokenMode(t *testing.T) {
	rig := newRig(AuthModes{MAC: true, Token: true})
	req := httptest.NewRequest(http.MethodGet, energyPath, nil)
	req.RemoteAddr = "192.0.2.10:4000"
	req.Header["Authorization"] = []string{""}
	req = req.WithContext(context.WithValue(req.Context(), http.LocalAddrContextKey,
		net.Addr(&net.TCPAddr{IP: net.ParseIP("192.0.2.1"), Port: 8080})))
	rec := httptest.NewRecorder()
	rig.h.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Fatalf("code = %d", rec.Code)
	}
	if rig.resolver.callCount() != 0 {
		t.Fatal("MAC fallback after a present but empty Authorization header")
	}
}

func TestHandlerNotLiveIsLoggedAsItsOwnReason(t *testing.T) {
	rig := newRig(AuthModes{MAC: true})
	rig.resolver.vmid = "999"
	for range 3 {
		if rec := rig.do(http.MethodGet, energyPath, "192.0.2.10:4000", ""); rec.Code != 503 {
			t.Fatalf("code = %d", rec.Code)
		}
	}
	if n := rig.logs.count(slog.LevelWarn); n != 1 {
		t.Fatalf("warns = %d, want 1: %v", n, rig.logs.dump())
	}
	if d := rig.logs.dump(); !strings.Contains(d[0], "reason=not live") || !strings.Contains(d[0], "vmid=999") {
		t.Fatalf("log = %v", d)
	}
	rig.resolver.vmid = "101"
	if rec := rig.do(http.MethodGet, energyPath, "192.0.2.10:4000", ""); rec.Code != 200 {
		t.Fatalf("code = %d", rec.Code)
	}
	if n := rig.logs.count(slog.LevelInfo); n != 0 {
		t.Fatalf("infos = %d, want 0 (success stays at Debug)", n)
	}
	if n := rig.logs.count(slog.LevelDebug); n != 1 {
		t.Fatalf("debugs = %d, want 1 for the served request", n)
	}
}
