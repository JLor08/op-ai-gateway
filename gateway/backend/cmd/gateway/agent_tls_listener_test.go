// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestSniffListenerServesHTTPAndHTTPSOnSamePort(t *testing.T) {
	fullchainPEM, keyPEM, _ := newSniffTestCertificate(t, 1)
	cert, err := tls.X509KeyPair([]byte(fullchainPEM), []byte(keyPEM))
	if err != nil {
		t.Fatalf("X509KeyPair: %v", err)
	}

	raw, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	ln := newSniffListener(raw, &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS != nil {
			_, _ = io.WriteString(w, "tls")
			return
		}
		_, _ = io.WriteString(w, "plain")
	})}
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()
	t.Cleanup(func() {
		_ = srv.Close()
		if err := <-serveErr; err != nil && err != http.ErrServerClosed {
			t.Errorf("Serve: %v", err)
		}
	})

	addr := ln.Addr().String()
	plainClient := &http.Client{Timeout: 2 * time.Second}
	if got := getSniffTestBody(t, plainClient, "http://"+addr); got != "plain" {
		t.Fatalf("HTTP response = %q, want plain", got)
	}

	tlsClient := newSniffTestHTTPClient(t, fullchainPEM)
	if got := getSniffTestBody(t, tlsClient, "https://"+addr); got != "tls" {
		t.Fatalf("HTTPS response = %q, want tls", got)
	}
}

func TestSniffListenerSilentPeerTimesOutWithoutBlockingAccept(t *testing.T) {
	fullchainPEM, keyPEM, _ := newSniffTestCertificate(t, 2)
	cert, err := tls.X509KeyPair([]byte(fullchainPEM), []byte(keyPEM))
	if err != nil {
		t.Fatalf("X509KeyPair: %v", err)
	}
	raw, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	observed := &readObservedListener{Listener: raw, readStarted: make(chan struct{})}
	ln := newSniffListenerWithOptions(observed, &tls.Config{Certificates: []tls.Certificate{cert}}, sniffOptions{
		PeekTimeout: 750 * time.Millisecond,
		MaxPending:  4,
	})
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ready")
	})}
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()
	t.Cleanup(func() {
		_ = srv.Close()
		if err := <-serveErr; err != nil && err != http.ErrServerClosed {
			t.Errorf("Serve: %v", err)
		}
	})

	silent, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial silent peer: %v", err)
	}
	defer silent.Close()
	select {
	case <-observed.readStarted:
	case <-time.After(time.Second):
		t.Fatal("sniffer did not begin reading the silent peer")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+ln.Addr().String(), nil)
	if err != nil {
		t.Fatalf("NewRequestWithContext: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("parallel HTTP request was blocked by silent peer: %v", err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatalf("read parallel HTTP response: %v", err)
	}
	if string(body) != "ready" {
		t.Fatalf("parallel HTTP response = %q, want ready", body)
	}

	if err := silent.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}
	if _, err := silent.Read(make([]byte, 1)); err == nil {
		t.Fatal("silent peer remained open after peek timeout")
	} else if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
		t.Fatalf("silent peer was not closed by the sniffer: %v", err)
	}
}

func TestSniffListenerCapsPendingPeeks(t *testing.T) {
	fullchainPEM, keyPEM, _ := newSniffTestCertificate(t, 3)
	cert, err := tls.X509KeyPair([]byte(fullchainPEM), []byte(keyPEM))
	if err != nil {
		t.Fatalf("X509KeyPair: %v", err)
	}
	raw, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	observed := &readObservedListener{Listener: raw, readStarted: make(chan struct{})}
	ln := newSniffListenerWithOptions(observed, &tls.Config{Certificates: []tls.Certificate{cert}}, sniffOptions{
		PeekTimeout: 2 * time.Second,
		MaxPending:  1,
	})
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ready")
	})}
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()
	t.Cleanup(func() {
		_ = srv.Close()
		if err := <-serveErr; err != nil && err != http.ErrServerClosed {
			t.Errorf("Serve: %v", err)
		}
	})

	silent, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial first silent peer: %v", err)
	}
	select {
	case <-observed.readStarted:
	case <-time.After(time.Second):
		t.Fatal("sniffer did not begin the first pending peek")
	}

	overflow, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial overflow peer: %v", err)
	}
	defer overflow.Close()
	// Backpressure, not drop-newcomer: with the single slot busy the accept loop
	// stops accepting, so the overflow connection waits (open) in the kernel
	// backlog rather than being closed. A read therefore TIMES OUT; an immediate
	// EOF/reset would mean the newcomer was dropped -- the regression this guards.
	if err := overflow.SetReadDeadline(time.Now().Add(500 * time.Millisecond)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}
	if _, err := overflow.Read(make([]byte, 1)); err == nil {
		t.Fatal("overflow peer unexpectedly received data before a slot freed")
	} else if netErr, ok := err.(net.Error); !ok || !netErr.Timeout() {
		t.Fatalf("overflow peer was dropped instead of held under backpressure: %v", err)
	}

	// Free the slot (and drain the held overflow) so the accept path resumes and
	// serves a fresh request.
	_ = silent.Close()
	_ = overflow.Close()
	deadline := time.Now().Add(3 * time.Second)
	var body string
	for time.Now().Before(deadline) {
		client := &http.Client{Timeout: 200 * time.Millisecond}
		resp, requestErr := client.Get("http://" + ln.Addr().String())
		if requestErr == nil {
			payload, readErr := io.ReadAll(resp.Body)
			resp.Body.Close()
			if readErr != nil {
				t.Fatalf("read HTTP response after releasing slot: %v", readErr)
			}
			body = string(payload)
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if body != "ready" {
		t.Fatalf("accept path did not recover after releasing pending slot; response = %q", body)
	}
}

func TestSniffListenerShutdownClosesEachPendingConnectionOnce(t *testing.T) {
	raw, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	counting := &closeCountingListener{
		Listener: raw,
		accepted: make(chan *closeCountingConn, 2),
	}
	ln := newSniffListenerWithOptions(counting, &tls.Config{}, sniffOptions{
		PeekTimeout: 5 * time.Second,
		MaxPending:  2,
	})
	sniffer, ok := ln.(*sniffListener)
	if !ok {
		t.Fatalf("listener type = %T, want *sniffListener", ln)
	}

	classifyingClient, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial classifying peer: %v", err)
	}
	defer classifyingClient.Close()
	classifying := <-counting.accepted
	select {
	case <-classifying.readStarted:
	case <-time.After(time.Second):
		t.Fatal("classifier did not begin reading the silent connection")
	}

	queuedClient, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial queued peer: %v", err)
	}
	defer queuedClient.Close()
	queued := <-counting.accepted
	if _, err := queuedClient.Write([]byte("G")); err != nil {
		t.Fatalf("write queued peer's first byte: %v", err)
	}
	select {
	case <-queued.readStarted:
	case <-time.After(time.Second):
		t.Fatal("classifier did not read the queued connection")
	}
	deadline := time.Now().Add(time.Second)
	for len(sniffer.ready) != 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := len(sniffer.ready); got != 1 {
		t.Fatalf("classified ready connections = %d, want 1", got)
	}

	if err := ln.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	sniffer.workers.Wait()
	if got := classifying.closeCount.Load(); got != 1 {
		t.Fatalf("classifying connection Close calls = %d, want 1", got)
	}
	if got := queued.closeCount.Load(); got != 1 {
		t.Fatalf("queued connection Close calls = %d, want 1", got)
	}

	var firstErr error
	for i := 0; i < 3; i++ {
		conn, err := ln.Accept()
		if conn != nil {
			_ = conn.Close()
			t.Fatalf("Accept after Close returned connection %T on call %d", conn, i+1)
		}
		if err == nil {
			t.Fatalf("Accept after Close returned nil error on call %d", i+1)
		}
		if !errors.Is(err, net.ErrClosed) {
			t.Fatalf("Accept after Close error on call %d = %v, want net.ErrClosed", i+1, err)
		}
		if firstErr == nil {
			firstErr = err
		} else if err.Error() != firstErr.Error() {
			t.Fatalf("Accept after Close error changed: first %q, call %d %q", firstErr, i+1, err)
		}
	}
}

func TestSniffListenerRetriesTemporaryRawAcceptError(t *testing.T) {
	raw, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	client, err := net.Dial("tcp", raw.Addr().String())
	if err != nil {
		t.Fatalf("dial queued real connection: %v", err)
	}
	defer client.Close()

	scripted := &temporaryOnceListener{
		Listener:          raw,
		temporaryReturned: make(chan struct{}),
	}
	ln := newSniffListenerWithOptions(scripted, &tls.Config{}, sniffOptions{
		PeekTimeout: time.Second,
		MaxPending:  1,
	})
	defer ln.Close()
	if _, err := client.Write([]byte("G")); err != nil {
		t.Fatalf("write queued real connection: %v", err)
	}
	select {
	case <-scripted.temporaryReturned:
	case <-time.After(time.Second):
		t.Fatal("scripted listener did not return its temporary error")
	}

	type acceptResult struct {
		conn net.Conn
		err  error
	}
	accepted := make(chan acceptResult, 1)
	go func() {
		conn, err := ln.Accept()
		accepted <- acceptResult{conn: conn, err: err}
	}()
	select {
	case result := <-accepted:
		if result.err != nil {
			t.Fatalf("Accept after temporary raw error: %v", result.err)
		}
		defer result.conn.Close()
		var first [1]byte
		if _, err := io.ReadFull(result.conn, first[:]); err != nil {
			t.Fatalf("read recovered real connection: %v", err)
		}
		if first[0] != 'G' {
			t.Fatalf("replayed first byte = %#x, want %#x", first[0], byte('G'))
		}
	case <-time.After(time.Second):
		t.Fatal("sniff listener did not recover after temporary raw Accept error")
	}
}

func TestSniffListenerCloseInterruptsTemporaryAcceptBackoff(t *testing.T) {
	raw := newTemporaryOnlyListener()
	ln := newSniffListenerWithOptions(raw, &tls.Config{}, sniffOptions{
		PeekTimeout: time.Second,
		MaxPending:  1,
	})
	sniffer, ok := ln.(*sniffListener)
	if !ok {
		t.Fatalf("listener type = %T, want *sniffListener", ln)
	}

	for want := 1; want <= 5; want++ {
		select {
		case got := <-raw.attempts:
			if got != want {
				t.Fatalf("raw Accept attempt = %d, want %d", got, want)
			}
		case _, ok := <-sniffer.ready:
			if !ok {
				t.Fatalf("sniff listener terminated after temporary raw Accept attempt %d", want-1)
			}
			t.Fatal("unexpected classified connection from temporary-only listener")
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for temporary raw Accept attempt %d", want)
		}
	}

	started := time.Now()
	if err := ln.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case _, ok := <-sniffer.ready:
		if ok {
			t.Fatal("unexpected classified connection while closing retry backoff")
		}
	case <-time.After(50 * time.Millisecond):
		t.Fatal("Close did not interrupt the 80ms temporary-error retry backoff")
	}
	if elapsed := time.Since(started); elapsed >= 50*time.Millisecond {
		t.Fatalf("Close/backoff shutdown took %s, want < 50ms", elapsed)
	}

	var firstErr error
	for i := 0; i < 3; i++ {
		conn, err := ln.Accept()
		if conn != nil {
			_ = conn.Close()
			t.Fatalf("Accept after Close returned connection %T on call %d", conn, i+1)
		}
		if !errors.Is(err, net.ErrClosed) {
			t.Fatalf("Accept after Close error on call %d = %v, want net.ErrClosed", i+1, err)
		}
		if firstErr == nil {
			firstErr = err
		} else if err.Error() != firstErr.Error() {
			t.Fatalf("Accept after Close error changed: first %q, call %d %q", firstErr, i+1, err)
		}
	}
}

func TestCertHolderHotSwapsLeafWithoutRebind(t *testing.T) {
	fullchain1, key1, wantFingerprint1 := newSniffTestCertificate(t, 4)
	fullchain2, key2, wantFingerprint2 := newSniffTestCertificate(t, 5)
	var holder certHolder
	if err := holder.StorePEM(fullchain1, key1); err != nil {
		t.Fatalf("StorePEM(first): %v", err)
	}

	raw, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	ln := newSniffListener(raw, &tls.Config{GetCertificate: holder.GetCertificate, MinVersion: tls.VersionTLS12})
	defer ln.Close()
	addrBefore := ln.Addr().String()
	serveErr := serveSniffTestTLSConnections(ln, 2)
	roots := newSniffTestRoots(t, fullchain1, fullchain2)

	gotFingerprint1 := dialSniffTestTLSFingerprint(t, addrBefore, roots)
	if gotFingerprint1 != wantFingerprint1 {
		t.Fatalf("first leaf fingerprint = %x, want %x", gotFingerprint1, wantFingerprint1)
	}
	if err := holder.StorePEM(fullchain2, key2); err != nil {
		t.Fatalf("StorePEM(second): %v", err)
	}
	if got := ln.Addr().String(); got != addrBefore {
		t.Fatalf("listener address changed during certificate swap: got %q, want %q", got, addrBefore)
	}
	gotFingerprint2 := dialSniffTestTLSFingerprint(t, addrBefore, roots)
	if gotFingerprint2 != wantFingerprint2 {
		t.Fatalf("second leaf fingerprint = %x, want %x", gotFingerprint2, wantFingerprint2)
	}
	if gotFingerprint1 == gotFingerprint2 {
		t.Fatal("certificate hot-swap served the same leaf on both connections")
	}
	if err := <-serveErr; err != nil {
		t.Fatalf("TLS server: %v", err)
	}
}

func TestCertHolderRejectsBrokenReplacementAndKeepsOldLeaf(t *testing.T) {
	fullchain, keyPEM, wantFingerprint := newSniffTestCertificate(t, 6)
	var holder certHolder
	if err := holder.StorePEM(fullchain, keyPEM); err != nil {
		t.Fatalf("StorePEM(valid): %v", err)
	}

	raw, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	ln := newSniffListener(raw, &tls.Config{GetCertificate: holder.GetCertificate, MinVersion: tls.VersionTLS12})
	defer ln.Close()
	serveErr := serveSniffTestTLSConnections(ln, 2)
	roots := newSniffTestRoots(t, fullchain)

	if got := dialSniffTestTLSFingerprint(t, ln.Addr().String(), roots); got != wantFingerprint {
		t.Fatalf("initial leaf fingerprint = %x, want %x", got, wantFingerprint)
	}
	if err := holder.StorePEM(fullchain, "not a private key"); err == nil {
		t.Fatal("StorePEM(broken replacement) returned nil error")
	}
	if got := dialSniffTestTLSFingerprint(t, ln.Addr().String(), roots); got != wantFingerprint {
		t.Fatalf("leaf changed after broken replacement: got %x, want %x", got, wantFingerprint)
	}
	if err := <-serveErr; err != nil {
		t.Fatalf("TLS server: %v", err)
	}
}

type readObservedListener struct {
	net.Listener
	once        sync.Once
	readStarted chan struct{}
}

func (l *readObservedListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &readObservedConn{Conn: conn, onRead: func() {
		l.once.Do(func() { close(l.readStarted) })
	}}, nil
}

type readObservedConn struct {
	net.Conn
	onRead func()
}

type closeCountingListener struct {
	net.Listener
	accepted chan *closeCountingConn
}

func (l *closeCountingListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	counting := &closeCountingConn{
		Conn:        conn,
		readStarted: make(chan struct{}),
	}
	l.accepted <- counting
	return counting, nil
}

type closeCountingConn struct {
	net.Conn
	readOnce    sync.Once
	readStarted chan struct{}
	closeCount  atomic.Int32
}

func (c *closeCountingConn) Read(p []byte) (int, error) {
	c.readOnce.Do(func() { close(c.readStarted) })
	return c.Conn.Read(p)
}

func (c *closeCountingConn) Close() error {
	c.closeCount.Add(1)
	return c.Conn.Close()
}

type temporaryAcceptError struct{}

func (temporaryAcceptError) Error() string   { return "temporary accept error" }
func (temporaryAcceptError) Timeout() bool   { return false }
func (temporaryAcceptError) Temporary() bool { return true }

type temporaryOnceListener struct {
	net.Listener
	returned          atomic.Bool
	temporaryReturned chan struct{}
}

func (l *temporaryOnceListener) Accept() (net.Conn, error) {
	if !l.returned.Swap(true) {
		close(l.temporaryReturned)
		return nil, temporaryAcceptError{}
	}
	return l.Listener.Accept()
}

type temporaryOnlyListener struct {
	attempts  chan int
	closed    chan struct{}
	closeOnce sync.Once
	count     atomic.Int32
}

func newTemporaryOnlyListener() *temporaryOnlyListener {
	return &temporaryOnlyListener{
		attempts: make(chan int, 8),
		closed:   make(chan struct{}),
	}
}

func (l *temporaryOnlyListener) Accept() (net.Conn, error) {
	select {
	case <-l.closed:
		return nil, net.ErrClosed
	default:
	}
	attempt := int(l.count.Add(1))
	l.attempts <- attempt
	return nil, temporaryAcceptError{}
}

func (l *temporaryOnlyListener) Close() error {
	l.closeOnce.Do(func() { close(l.closed) })
	return nil
}

func (l *temporaryOnlyListener) Addr() net.Addr {
	return temporaryListenerAddr("temporary-listener")
}

type temporaryListenerAddr string

func (a temporaryListenerAddr) Network() string { return "test" }
func (a temporaryListenerAddr) String() string  { return string(a) }

func (c *readObservedConn) Read(p []byte) (int, error) {
	c.onRead()
	return c.Conn.Read(p)
}

func newSniffTestCertificate(t *testing.T, serial int64) (string, string, [sha256.Size]byte) {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber: big.NewInt(serial),
		Subject:      pkix.Name{CommonName: fmt.Sprintf("sniff-test-%d", serial)},
		DNSNames:     []string{"localhost"},
		NotBefore:    now.Add(-time.Minute),
		NotAfter:     now.Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, publicKey, privateKey)
	if err != nil {
		t.Fatalf("CreateCertificate: %v", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		t.Fatalf("MarshalPKCS8PrivateKey: %v", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	return string(certPEM), string(keyPEM), sha256.Sum256(der)
}

func newSniffTestRoots(t *testing.T, certificates ...string) *x509.CertPool {
	t.Helper()
	roots := x509.NewCertPool()
	for _, certificate := range certificates {
		if !roots.AppendCertsFromPEM([]byte(certificate)) {
			t.Fatal("AppendCertsFromPEM returned false")
		}
	}
	return roots
}

func newSniffTestHTTPClient(t *testing.T, certificate string) *http.Client {
	t.Helper()
	return &http.Client{
		Timeout: 2 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{
			RootCAs:    newSniffTestRoots(t, certificate),
			ServerName: "localhost",
			MinVersion: tls.VersionTLS12,
		}},
	}
}

func getSniffTestBody(t *testing.T, client *http.Client, target string) string {
	t.Helper()
	resp, err := client.Get(target)
	if err != nil {
		t.Fatalf("GET %s: %v", target, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s response: %v", target, err)
	}
	return string(body)
}

func serveSniffTestTLSConnections(ln net.Listener, count int) <-chan error {
	done := make(chan error, 1)
	go func() {
		for i := 0; i < count; i++ {
			conn, err := ln.Accept()
			if err != nil {
				done <- fmt.Errorf("accept connection %d: %w", i+1, err)
				return
			}
			tlsConn, ok := conn.(*tls.Conn)
			if !ok {
				_ = conn.Close()
				done <- fmt.Errorf("connection %d has type %T, want *tls.Conn", i+1, conn)
				return
			}
			if err := tlsConn.Handshake(); err != nil {
				_ = tlsConn.Close()
				done <- fmt.Errorf("handshake connection %d: %w", i+1, err)
				return
			}
			_ = tlsConn.Close()
		}
		done <- nil
	}()
	return done
}

func dialSniffTestTLSFingerprint(t *testing.T, addr string, roots *x509.CertPool) [sha256.Size]byte {
	t.Helper()
	dialer := &net.Dialer{Timeout: 2 * time.Second}
	conn, err := tls.DialWithDialer(dialer, "tcp", addr, &tls.Config{
		RootCAs:    roots,
		ServerName: "localhost",
		MinVersion: tls.VersionTLS12,
	})
	if err != nil {
		t.Fatalf("tls.Dial(%s): %v", addr, err)
	}
	defer conn.Close()
	peerCertificates := conn.ConnectionState().PeerCertificates
	if len(peerCertificates) != 1 {
		t.Fatalf("peer certificate count = %d, want 1", len(peerCertificates))
	}
	return sha256.Sum256(peerCertificates[0].Raw)
}
