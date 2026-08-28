// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package proxy

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"testing"
	"time"
)

// writeTestLeaf generates a self-signed EC (P-256) leaf valid for 127.0.0.1 and
// writes it to certDir as fullchain.pem + privkey.pem (the on-disk names the cert
// holder loads). It returns a RootCAs pool trusting the leaf so an https client
// can verify the proxy WITHOUT InsecureSkipVerify.
func writeTestLeaf(t testing.TB, certDir string) *x509.CertPool {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: "op-agent-proxy-test"},
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	if err := os.WriteFile(filepath.Join(certDir, "fullchain.pem"), certPEM, 0o600); err != nil {
		t.Fatalf("write fullchain: %v", err)
	}
	if err := os.WriteFile(filepath.Join(certDir, "privkey.pem"), keyPEM, 0o600); err != nil {
		t.Fatalf("write privkey: %v", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(certPEM) {
		t.Fatal("append test leaf to pool")
	}
	return pool
}

// freePort grabs an ephemeral port on loopback and immediately releases it so the
// manager can bind it. There is an inherent (small) TOCTOU window; acceptable in a
// test.
func freePort(t testing.TB) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("free port: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return port
}

// httpsClient builds an https client that trusts pool and never skips verification.
func httpsClient(pool *x509.CertPool) *http.Client {
	return &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
		},
	}
}

// waitTLSActive polls Status() until the given listen reports TLSActive, or fails.
func waitTLSActive(t testing.TB, m *Manager, listen int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		for _, s := range m.Status() {
			if s.Listen == listen && s.TLSActive {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("listen %d never became TLSActive; status=%+v", listen, m.Status())
}

func TestResolveRoutesFallbackVsOverride(t *testing.T) {
	gw := []Route{{Listen: 8600, Upstream: "upA"}}
	local := []Route{{Listen: 8600, Upstream: "upLocal"}, {Listen: 8601, Upstream: "upB"}}

	toMap := func(rs []Route) map[int]string {
		m := map[int]string{}
		for _, r := range rs {
			m[r.Listen] = r.Upstream
		}
		return m
	}

	fb := toMap(ResolveRoutes(gw, local, "fallback"))
	if len(fb) != 2 || fb[8600] != "upA" || fb[8601] != "upB" {
		t.Fatalf("fallback: want {8600:upA, 8601:upB}, got %+v", fb)
	}

	ov := toMap(ResolveRoutes(gw, local, "override"))
	if len(ov) != 2 || ov[8600] != "upLocal" || ov[8601] != "upB" {
		t.Fatalf("override: want {8600:upLocal, 8601:upB}, got %+v", ov)
	}
}

func TestResolveRoutesDropsMalformedAndDedups(t *testing.T) {
	gw := []Route{
		{Listen: 8600, Upstream: "upA"},
		{Listen: 0, Upstream: "bad"},          // malformed: no port
		{Listen: 8600, Upstream: "upDup"},     // dup listen: last wins
		{Listen: 8602, Upstream: ""},          // malformed: empty upstream
		{Listen: 70000, Upstream: "outrange"}, // malformed: port out of range
	}
	got := ResolveRoutes(gw, nil, "fallback")
	if len(got) != 1 {
		t.Fatalf("want 1 route after dropping malformed, got %+v", got)
	}
	if got[0].Listen != 8600 || got[0].Upstream != "upDup" {
		t.Fatalf("want {8600:upDup} (last-wins dedup), got %+v", got[0])
	}
}

func TestManagerServesTLSAndProxies(t *testing.T) {
	certDir := t.TempDir()
	pool := writeTestLeaf(t, certDir)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "hello-from-upstream")
	}))
	defer upstream.Close()

	m := New(certDir, "127.0.0.1")
	defer m.Close()

	listen := freePort(t)
	m.Apply([]Route{{Listen: listen, Upstream: upstream.URL}})
	waitTLSActive(t, m, listen)

	client := httpsClient(pool)
	resp, err := client.Get("https://127.0.0.1:" + strconv.Itoa(listen) + "/")
	if err != nil {
		t.Fatalf("https get: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "hello-from-upstream" {
		t.Fatalf("want upstream body, got %q", string(body))
	}

	found := false
	for _, s := range m.Status() {
		if s.Listen == listen {
			found = true
			if !s.TLSActive {
				t.Fatalf("status TLSActive=false for serving route")
			}
		}
	}
	if !found {
		t.Fatalf("status missing route %d: %+v", listen, m.Status())
	}
}

func TestManagerWaitsForLeaf(t *testing.T) {
	certDir := t.TempDir() // EMPTY: no leaf yet

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "up")
	}))
	defer upstream.Close()

	m := New(certDir, "127.0.0.1")
	defer m.Close()

	listen := freePort(t)
	m.Apply([]Route{{Listen: listen, Upstream: upstream.URL}})

	// No leaf on disk -> route pending, no listener bound.
	for _, s := range m.Status() {
		if s.Listen == listen && s.TLSActive {
			t.Fatalf("route should be pending with empty certDir")
		}
	}
	// A dial should fail (no half-open port).
	if c, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(listen), 200*time.Millisecond); err == nil {
		_ = c.Close()
		t.Fatalf("port %d bound while pending (half-open)", listen)
	}

	// Install the leaf + reload tick -> it comes up.
	pool := writeTestLeaf(t, certDir)
	m.ReloadCert()
	waitTLSActive(t, m, listen)

	client := httpsClient(pool)
	resp, err := client.Get("https://127.0.0.1:" + strconv.Itoa(listen) + "/")
	if err != nil {
		t.Fatalf("https get after leaf install: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "up" {
		t.Fatalf("want upstream body after leaf install, got %q", string(body))
	}
}

func TestManagerStreamsWithoutBuffering(t *testing.T) {
	certDir := t.TempDir()
	pool := writeTestLeaf(t, certDir)

	firstWritten := make(chan struct{})
	release := make(chan struct{})
	// The upstream handler parks on <-release, and the deferred
	// httptest.Server.Close waits for that connection to finish. Releasing it
	// from a defer -- once, from whichever path gets there first -- keeps a
	// t.Fatalf between here and the release below from leaving Close waiting on
	// a handler nobody would ever wake: that turned a one-line assertion
	// failure into a 10-minute package timeout in CI.
	var releaseOnce sync.Once
	releaseUpstream := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseUpstream()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fl, ok := w.(http.Flusher)
		if !ok {
			t.Errorf("upstream ResponseWriter not a Flusher")
			return
		}
		io.WriteString(w, "first\n")
		fl.Flush()
		close(firstWritten)
		<-release
		io.WriteString(w, "second\n")
		fl.Flush()
	}))
	defer upstream.Close()

	m := New(certDir, "127.0.0.1")
	defer m.Close()

	listen := freePort(t)
	m.Apply([]Route{{Listen: listen, Upstream: upstream.URL}})
	waitTLSActive(t, m, listen)

	client := httpsClient(pool)
	resp, err := client.Get("https://127.0.0.1:" + strconv.Itoa(listen) + "/")
	if err != nil {
		t.Fatalf("https get: %v", err)
	}
	defer resp.Body.Close()

	// Read the first chunk in a goroutine: with FlushInterval:-1 it must arrive
	// BEFORE we release the upstream's second write. With default buffering the
	// read would block until upstream finishes -> caught by the timeout below.
	type readResult struct {
		buf []byte
		err error
	}
	got := make(chan readResult, 1)
	go func() {
		buf := make([]byte, len("first\n"))
		_, err := io.ReadFull(resp.Body, buf)
		got <- readResult{buf: buf, err: err}
	}()

	select {
	case rr := <-got:
		if rr.err != nil {
			t.Fatalf("read first chunk: %v", rr.err)
		}
		if string(rr.buf) != "first\n" {
			t.Fatalf("want first chunk %q, got %q", "first\n", string(rr.buf))
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("first chunk not received before second write (response buffered; FlushInterval not -1)")
	}

	// Confirm upstream actually flushed the first chunk before we released it.
	// This WAITS rather than polling with a default branch: the handler closes
	// firstWritten only after its flush returns, so the flushed bytes can reach
	// this goroutine before that close executes. Whenever the handler was
	// descheduled in exactly that window -- a contended CI runner -- the
	// non-blocking check reported a violation that had not happened.
	select {
	case <-firstWritten:
	case <-time.After(3 * time.Second):
		t.Fatalf("upstream never flushed the first chunk")
	}

	releaseUpstream()
	rest, _ := io.ReadAll(resp.Body)
	if string(rest) != "second\n" {
		t.Fatalf("want second chunk %q, got %q", "second\n", string(rest))
	}
}

func TestManagerDrainsRemovedRoute(t *testing.T) {
	certDir := t.TempDir()
	pool := writeTestLeaf(t, certDir)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "up")
	}))
	defer upstream.Close()

	m := New(certDir, "127.0.0.1")
	defer m.Close()

	keep := freePort(t)
	drop := freePort(t)
	for keep == drop {
		drop = freePort(t)
	}
	m.Apply([]Route{
		{Listen: keep, Upstream: upstream.URL},
		{Listen: drop, Upstream: upstream.URL},
	})
	waitTLSActive(t, m, keep)
	waitTLSActive(t, m, drop)

	// Drop one route.
	m.Apply([]Route{{Listen: keep, Upstream: upstream.URL}})

	// The removed listener must be closed: a dial to it fails.
	deadline := time.Now().Add(3 * time.Second)
	closed := false
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(drop), 200*time.Millisecond)
		if err != nil {
			closed = true
			break
		}
		_ = c.Close()
		time.Sleep(20 * time.Millisecond)
	}
	if !closed {
		t.Fatalf("removed listener %d still accepts connections", drop)
	}

	// Status no longer lists the dropped route; keep still active.
	listens := map[int]bool{}
	for _, s := range m.Status() {
		listens[s.Listen] = s.TLSActive
	}
	if _, ok := listens[drop]; ok {
		t.Fatalf("dropped route %d still in status: %+v", drop, m.Status())
	}
	if !listens[keep] {
		t.Fatalf("kept route %d not active after drop: %+v", keep, m.Status())
	}

	// The kept one still serves.
	client := httpsClient(pool)
	resp, err := client.Get("https://127.0.0.1:" + strconv.Itoa(keep) + "/")
	if err != nil {
		t.Fatalf("kept route https get: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "up" {
		t.Fatalf("kept route body = %q", string(body))
	}
}

func TestManagerUpstreamDownReturns502(t *testing.T) {
	certDir := t.TempDir()
	pool := writeTestLeaf(t, certDir)

	// Point at a dead upstream (grab then release a port).
	deadPort := freePort(t)

	m := New(certDir, "127.0.0.1")
	defer m.Close()

	listen := freePort(t)
	m.Apply([]Route{{Listen: listen, Upstream: "http://127.0.0.1:" + strconv.Itoa(deadPort)}})
	waitTLSActive(t, m, listen)

	client := httpsClient(pool)
	resp, err := client.Get("https://127.0.0.1:" + strconv.Itoa(listen) + "/")
	if err != nil {
		t.Fatalf("https get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("want 502 for down upstream, got %d", resp.StatusCode)
	}
}

// TestManagerConcurrentReconcileStress hammers the same listen ports with rapidly
// toggling upstreams while Status()/ReloadCert() run concurrently, forcing many
// overlapping generations (delayed Serve-exit vs. a live successor on the same
// port). Under -race it proves the single-mutex + generation-guard discipline
// holds and that Close() leaks no goroutine.
func TestManagerConcurrentReconcileStress(t *testing.T) {
	certDir := t.TempDir()
	writeTestLeaf(t, certDir)

	upA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "A") }))
	defer upA.Close()
	upB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "B") }))
	defer upB.Close()

	m := New(certDir, "127.0.0.1")

	p1 := freePort(t)
	p2 := freePort(t)
	for p1 == p2 {
		p2 = freePort(t)
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup

	// Two writers toggling the upstream (and presence) of the shared ports.
	for w := 0; w < 2; w++ {
		wg.Add(1)
		go func(seed int) {
			defer wg.Done()
			i := seed
			for {
				select {
				case <-stop:
					return
				default:
				}
				i++
				up := upA.URL
				if i%2 == 0 {
					up = upB.URL
				}
				switch i % 3 {
				case 0:
					m.Apply([]Route{{Listen: p1, Upstream: up}, {Listen: p2, Upstream: up}})
				case 1:
					m.Apply([]Route{{Listen: p1, Upstream: up}})
				case 2:
					m.Apply(nil)
				}
			}
		}(w)
	}
	// Concurrent readers + reloaders.
	for r := 0; r < 2; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				_ = m.Status()
				m.ReloadCert()
			}
		}()
	}

	time.Sleep(300 * time.Millisecond)
	close(stop)
	wg.Wait()

	m.Close()
	if len(m.Status()) != 0 {
		t.Fatalf("status not empty after Close: %+v", m.Status())
	}
	for _, p := range []int{p1, p2} {
		if c, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(p), 200*time.Millisecond); err == nil {
			_ = c.Close()
			t.Fatalf("port %d still bound after Close", p)
		}
	}
}

func TestManagerCloseStopsAll(t *testing.T) {
	certDir := t.TempDir()
	writeTestLeaf(t, certDir)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "up")
	}))
	defer upstream.Close()

	m := New(certDir, "127.0.0.1")

	ports := []int{freePort(t), freePort(t)}
	sort.Ints(ports)
	if ports[0] == ports[1] {
		ports[1] = freePort(t)
	}
	routes := []Route{{Listen: ports[0], Upstream: upstream.URL}, {Listen: ports[1], Upstream: upstream.URL}}
	m.Apply(routes)
	for _, r := range routes {
		waitTLSActive(t, m, r.Listen)
	}

	m.Close()

	if len(m.Status()) != 0 {
		t.Fatalf("status not empty after Close: %+v", m.Status())
	}
	for _, r := range routes {
		if c, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(r.Listen), 200*time.Millisecond); err == nil {
			_ = c.Close()
			t.Fatalf("port %d still bound after Close", r.Listen)
		}
	}
}

// TestManagerBindsLeafSANWhenHostEmpty proves the production path: with an empty
// host override the Manager derives the bind address from the installed leaf's
// SAN (127.0.0.1 for the test leaf) and serves there -- never all interfaces.
func TestManagerBindsLeafSANWhenHostEmpty(t *testing.T) {
	certDir := t.TempDir()
	pool := writeTestLeaf(t, certDir)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "leaf-san-bound")
	}))
	defer upstream.Close()

	m := New(certDir, "") // empty => derive bind host from the leaf SAN
	defer m.Close()

	listen := freePort(t)
	m.Apply([]Route{{Listen: listen, Upstream: upstream.URL}})
	waitTLSActive(t, m, listen)

	client := httpsClient(pool)
	resp, err := client.Get("https://127.0.0.1:" + strconv.Itoa(listen) + "/")
	if err != nil {
		t.Fatalf("https get on leaf-derived bind: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "leaf-san-bound" {
		t.Fatalf("want upstream body, got %q", string(body))
	}
}

// TestCertHolderBindHostDerivation pins the SAN-preference order: IP SAN first,
// then DNS SAN; no leaf or no SAN yields "" so startProxyLocked keeps the route
// pending rather than ever binding all interfaces.
func TestCertHolderBindHostDerivation(t *testing.T) {
	var h certHolder
	if got := h.bindHost(); got != "" {
		t.Fatalf("bindHost with no leaf = %q, want empty", got)
	}
	h.current.Store(&tls.Certificate{Leaf: &x509.Certificate{
		IPAddresses: []net.IP{net.ParseIP("10.0.0.5")},
		DNSNames:    []string{"ai.int.test"},
	}})
	if got := h.bindHost(); got != "10.0.0.5" {
		t.Fatalf("bindHost with IP SAN = %q, want 10.0.0.5", got)
	}
	h.current.Store(&tls.Certificate{Leaf: &x509.Certificate{DNSNames: []string{"ai.int.test"}}})
	if got := h.bindHost(); got != "ai.int.test" {
		t.Fatalf("bindHost with DNS SAN = %q, want ai.int.test", got)
	}
	h.current.Store(&tls.Certificate{Leaf: &x509.Certificate{}})
	if got := h.bindHost(); got != "" {
		t.Fatalf("bindHost with no SAN = %q, want empty (route stays pending)", got)
	}
}

// TestDeriveBindHost proves the exported main.go-facing helper (I2,
// task-18-fix-round-1.md): a real leaf on disk resolves to its IP SAN
// (matching writeTestLeaf's 127.0.0.1/localhost pair, IP preferred over
// DNS per bindHost's own order), and a directory with no loadable leaf at
// all returns "" rather than erroring -- the caller's own signal to fall
// back to its own default.
func TestDeriveBindHost(t *testing.T) {
	certDir := t.TempDir()
	writeTestLeaf(t, certDir)
	if got := DeriveBindHost(certDir); got != "127.0.0.1" {
		t.Fatalf("DeriveBindHost(with leaf) = %q, want 127.0.0.1", got)
	}

	empty := t.TempDir()
	if got := DeriveBindHost(empty); got != "" {
		t.Fatalf("DeriveBindHost(no leaf) = %q, want empty", got)
	}
}

// TestManagerStopDrainsOffLock proves stopProxyLocked frees the port fd under the
// lock but drains in-flight connections OFF-LOCK: removing a route with an
// in-flight (blocked) request must not stall Apply/Status for the shutdown grace,
// and the freed listen must be immediately rebindable in a later Apply even while
// the prior route's drain is still pending.
func TestManagerStopDrainsOffLock(t *testing.T) {
	certDir := t.TempDir()
	pool := writeTestLeaf(t, certDir)

	// Upstream whose handler blocks until released, so a request routed through the
	// proxy stays in-flight and the route's graceful drain would hit shutdownGrace.
	release := make(chan struct{})
	reached := make(chan struct{})
	var reachedOnce sync.Once
	// Same reason as in TestManagerStreamsWithoutBuffering: several t.Fatalf
	// calls sit between the blocked handler below and the release at the end of
	// this test, and the deferred Close waits for that handler. Release from a
	// defer so a failing assertion fails fast instead of hanging until the
	// package timeout and hiding its own message.
	var releaseOnce sync.Once
	releaseUpstream := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseUpstream()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reachedOnce.Do(func() { close(reached) })
		<-release
		io.WriteString(w, "first")
	}))
	defer upstream.Close()

	m := New(certDir, "127.0.0.1")
	defer m.Close()

	listen := freePort(t)
	m.Apply([]Route{{Listen: listen, Upstream: upstream.URL}})
	waitTLSActive(t, m, listen)

	client := httpsClient(pool)
	var clientDone sync.WaitGroup
	clientDone.Add(1)
	go func() {
		defer clientDone.Done()
		resp, err := client.Get("https://127.0.0.1:" + strconv.Itoa(listen) + "/")
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	}()
	<-reached // the request is now in-flight inside the (blocked) upstream handler

	// Removing the route must return promptly: the port is freed under the lock and
	// the up-to-shutdownGrace drain of the in-flight request happens off-lock.
	start := time.Now()
	m.Apply(nil)
	if elapsed := time.Since(start); elapsed > shutdownGrace/2 {
		t.Fatalf("Apply stalled %v removing a route with an in-flight request; expected off-lock drain (<%v)", elapsed, shutdownGrace/2)
	}
	// Status is likewise not stalled and reports the route gone.
	if s := m.Status(); len(s) != 0 {
		t.Fatalf("Status after Apply(nil) = %+v, want empty", s)
	}

	// The port was freed synchronously: a fresh route on the SAME listen comes up
	// even while the prior route's drain is still pending off-lock.
	upstream2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "second")
	}))
	defer upstream2.Close()
	m.Apply([]Route{{Listen: listen, Upstream: upstream2.URL}})
	waitTLSActive(t, m, listen)
	resp, err := client.Get("https://127.0.0.1:" + strconv.Itoa(listen) + "/")
	if err != nil {
		t.Fatalf("https get after rebind on same port: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "second" {
		t.Fatalf("rebound route served %q, want \"second\"", string(body))
	}

	// Release the first (drained) request so it completes and the deferred Close
	// drains cleanly without waiting out the grace.
	releaseUpstream()
	clientDone.Wait()
}
