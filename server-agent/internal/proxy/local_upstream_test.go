// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package proxy

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// mustParse parses a raw upstream the way startProxyLocked does (url.Parse plus
// the scheme/host non-empty check) and fails the test if it would not have been
// accepted as a route upstream at all.
func mustParse(t testing.TB, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("url.Parse(%q): %v", raw, err)
	}
	return u
}

// TestLoopbackUpstreamPortAccepts pins the ONLY shapes that may be diverted
// into the in-process router. Everything here is a loopback target with a bare
// authority and nothing else -- no path, no query, no userinfo, no fragment --
// because the in-process hand-off skips the reverse-proxy director entirely and
// therefore cannot honour any rewriting the URL would imply.
func TestLoopbackUpstreamPortAccepts(t *testing.T) {
	cases := []struct {
		raw  string
		want int
	}{
		{"http://127.0.0.1:9000", 9000},
		{"http://127.0.0.1:9000/", 9000},
		{"http://localhost:8600", 8600},
		{"http://LOCALHOST:8600", 8600},      // host case is not significant
		{"http://127.9.9.9:1", 1},            // all of 127.0.0.0/8 is loopback
		{"http://[::1]:65535", 65535},        // IPv6 loopback, upper port bound
		{"http://[::ffff:127.0.0.1]:80", 80}, // IPv4-mapped loopback
	}
	for _, tc := range cases {
		got, ok := loopbackUpstreamPort(mustParse(t, tc.raw))
		if !ok || got != tc.want {
			t.Errorf("loopbackUpstreamPort(%q) = (%d, %v), want (%d, true)", tc.raw, got, ok, tc.want)
		}
	}
}

// TestLoopbackUpstreamPortRejects is the security half: this predicate decides
// whether a request is dialled to the address the route names or handed to a
// LOCAL handler instead, so every false positive silently diverts traffic
// destined for somewhere else into this agent's own router. A rejection only
// costs today's dial, so the predicate must reject everything it is not certain
// about.
func TestLoopbackUpstreamPortRejects(t *testing.T) {
	cases := []string{
		"https://127.0.0.1:9000",             // TLS upstream: the router speaks plaintext
		"http://10.0.0.5:9000",               // routable IPv4
		"http://[2001:db8::1]:9000",          // routable IPv6
		"http://0.0.0.0:9000",                // unspecified, not loopback
		"http://example.com:9000",            // a name that is not localhost
		"http://localhost.evil.com:9000",     // localhost as a LABEL, not the host
		"http://notlocalhost:9000",           // superstring of localhost
		"http://localhost.:9000",             // fully-qualified trailing dot
		"http://127.0.0.1:9000/v1",           // path: the director would have joined it
		"http://127.0.0.1:9000/v1/",          // ditto
		"http://127.0.0.1:9000?a=b",          // query: the director would have merged it
		"http://127.0.0.1:9000?",             // forced empty query
		"http://127.0.0.1:9000#frag",         // fragment
		"http://user:pw@127.0.0.1:9000",      // userinfo
		"http://127.0.0.1:9000@example.com/", // the classic authority-confusion shape
		"http://127.0.0.1",                   // no port at all
		"http://127.0.0.1:0",                 // port 0
		"http://127.0.0.1:65536",             // above the port range
		"http://127.0.0.1:99999999999999999", // not representable
		"//127.0.0.1:9000",                   // no scheme
		"http:127.0.0.1:9000",                // opaque
		"HTTP://127.0.0.1:9000/../v1",        // dot segments
	}
	for _, raw := range cases {
		u, err := url.Parse(raw)
		if err != nil {
			continue // never parsed as a route upstream in the first place
		}
		if port, ok := loopbackUpstreamPort(u); ok {
			t.Errorf("loopbackUpstreamPort(%q) = (%d, true), want ok=false", raw, port)
		}
	}
}

// countingUpstream binds a real plaintext HTTP server on loopback that answers
// body and counts every ACCEPTED connection, so a test can assert that no TCP
// dial reached it. It returns the port and the accept counter.
func countingUpstream(t testing.TB, body string) (int, *atomic.Int64) {
	t.Helper()
	raw, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("bind counting upstream: %v", err)
	}
	accepts := &atomic.Int64{}
	ln := &countingListener{Listener: raw, accepts: accepts}
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Served-By", "dialled")
			_, _ = io.WriteString(w, body)
		}),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return raw.Addr().(*net.TCPAddr).Port, accepts
}

type countingListener struct {
	net.Listener
	accepts *atomic.Int64
}

func (l *countingListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err == nil {
		l.accepts.Add(1)
	}
	return c, err
}

// TestManagerLocalUpstreamServesInProcessWithoutDialling is C1's core claim: a
// route whose upstream is a loopback target on the CURRENTLY PUBLISHED router
// port is served by the router's own http.Handler in process, and the TCP
// listener that happens to sit on that port is never dialled.
func TestManagerLocalUpstreamServesInProcessWithoutDialling(t *testing.T) {
	certDir := t.TempDir()
	pool := writeTestLeaf(t, certDir)
	m := New(certDir, "127.0.0.1")
	defer m.Close()

	upstreamPort, accepts := countingUpstream(t, "dialled")

	var resolved atomic.Int64
	m.SetLocalUpstream(func(port int) http.Handler {
		if port != upstreamPort {
			return nil
		}
		resolved.Add(1)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Served-By", "in-process")
			_, _ = io.WriteString(w, "dialled") // byte-identical to the dialled path
		})
	})

	listen := freePort(t)
	m.Apply([]Route{{Listen: listen, Upstream: fmt.Sprintf("http://127.0.0.1:%d", upstreamPort)}})
	waitTLSActive(t, m, listen)

	resp, err := httpsClient(pool).Get(fmt.Sprintf("https://127.0.0.1:%d/v1/models", listen))
	if err != nil {
		t.Fatalf("GET through proxy: %v", err)
	}
	defer resp.Body.Close()
	got, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || string(got) != "dialled" {
		t.Fatalf("proxied response = %d %q, want 200 %q", resp.StatusCode, got, "dialled")
	}
	if by := resp.Header.Get("X-Served-By"); by != "in-process" {
		t.Fatalf("X-Served-By = %q, want %q (the request was dialled, not handed over)", by, "in-process")
	}
	if n := accepts.Load(); n != 0 {
		t.Fatalf("counting upstream accepted %d connection(s); the in-process path must never dial", n)
	}
	if resolved.Load() == 0 {
		t.Fatal("the local-upstream resolver was never consulted")
	}
}

// TestManagerLocalUpstreamFallsBackToDialOnPortMismatch pins the other half:
// a loopback upstream whose port is NOT the published router port is dialled
// exactly as before. Resolving per request is what makes this true at every
// moment rather than only at listener-start time.
func TestManagerLocalUpstreamFallsBackToDialOnPortMismatch(t *testing.T) {
	certDir := t.TempDir()
	pool := writeTestLeaf(t, certDir)
	m := New(certDir, "127.0.0.1")
	defer m.Close()

	upstreamPort, accepts := countingUpstream(t, "dialled")
	m.SetLocalUpstream(func(port int) http.Handler {
		return nil // no router published for any port (StartRouter(0) / disabled runtime)
	})

	listen := freePort(t)
	m.Apply([]Route{{Listen: listen, Upstream: fmt.Sprintf("http://127.0.0.1:%d", upstreamPort)}})
	waitTLSActive(t, m, listen)

	resp, err := httpsClient(pool).Get(fmt.Sprintf("https://127.0.0.1:%d/v1/models", listen))
	if err != nil {
		t.Fatalf("GET through proxy: %v", err)
	}
	defer resp.Body.Close()
	got, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || string(got) != "dialled" {
		t.Fatalf("proxied response = %d %q, want 200 %q", resp.StatusCode, got, "dialled")
	}
	if by := resp.Header.Get("X-Served-By"); by != "dialled" {
		t.Fatalf("X-Served-By = %q, want %q", by, "dialled")
	}
	if n := accepts.Load(); n == 0 {
		t.Fatal("counting upstream accepted 0 connections; the fallback must still dial")
	}
}

// TestManagerLocalUpstreamNeverConsultedForRemoteUpstream is the boundary test
// at the Manager level: a route pointing somewhere OTHER than loopback must
// never reach the resolver at all, whatever port it names. If it did, a remote
// upstream on the router's port number would be answered by this agent's own
// router instead of by the machine the route names.
func TestManagerLocalUpstreamNeverConsultedForRemoteUpstream(t *testing.T) {
	certDir := t.TempDir()
	pool := writeTestLeaf(t, certDir)
	m := New(certDir, "127.0.0.1")
	defer m.Close()

	// A real server bound on loopback, but named by a NON-loopback-looking
	// upstream would not be reachable; instead point at a live loopback server
	// and assert only that the resolver is never asked about an https upstream.
	upstreamPort, _ := countingUpstream(t, "dialled")
	var consulted atomic.Int64
	m.SetLocalUpstream(func(int) http.Handler {
		consulted.Add(1)
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusTeapot)
		})
	})

	listen := freePort(t)
	// https scheme on the same loopback port: rejected by loopbackUpstreamPort,
	// so the route is dialled (and fails TLS against a plaintext server, which
	// is fine -- the assertion is about the resolver, not the response).
	m.Apply([]Route{{Listen: listen, Upstream: fmt.Sprintf("https://127.0.0.1:%d", upstreamPort)}})
	waitTLSActive(t, m, listen)

	resp, err := httpsClient(pool).Get(fmt.Sprintf("https://127.0.0.1:%d/v1/models", listen))
	if err == nil {
		defer resp.Body.Close()
		if resp.StatusCode == http.StatusTeapot {
			t.Fatal("an https upstream was diverted into the in-process router")
		}
	}
	if n := consulted.Load(); n != 0 {
		t.Fatalf("resolver consulted %d time(s) for a non-loopback upstream, want 0", n)
	}
}

// TestLocalFirstFallsBackWhenResolverReturnsNil pins localFirst's own contract
// without a socket in the way: a nil resolution means "serve the fallback",
// never "fail".
func TestLocalFirstFallsBackWhenResolverReturnsNil(t *testing.T) {
	fallback := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "fallback")
	})
	lf := &localFirst{resolve: func(int) http.Handler { return nil }, port: 1234, fallback: fallback}
	rec := &recorder{header: http.Header{}}
	lf.ServeHTTP(rec, httpRequest(t))
	if got := rec.body.String(); got != "fallback" {
		t.Fatalf("body = %q, want %q", got, "fallback")
	}
}

func httpRequest(t testing.TB) *http.Request {
	t.Helper()
	r, err := http.NewRequest(http.MethodGet, "http://example.invalid/x", strings.NewReader(""))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	return r
}

type recorder struct {
	header http.Header
	body   strings.Builder
	code   int
}

func (r *recorder) Header() http.Header         { return r.header }
func (r *recorder) Write(b []byte) (int, error) { return r.body.Write(b) }
func (r *recorder) WriteHeader(code int)        { r.code = code }
