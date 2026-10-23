// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package runtime

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
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
	"net/http/httptest"
	"op-ai-server-agent/internal/proxy"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestLiveFullDuplexInterleavesThroughProxyAndRouter is the end-to-end proof
// that POST /live/{model} streams the PCM upload UP while streaming the SSE
// transcript DOWN on one gateway-facing HTTP/1.1 connection -- a true timing
// interleave, not merely a correctness-of-final-bytes check.
//
// Wiring (prod-faithful, the brief's PREFERRED path): a real internal/proxy
// Manager serves a TLS route whose loopback upstream resolves IN PROCESS to the
// runtime router (proxy.SetLocalUpstream -> localFirst, local_upstream.go). That
// is exactly how main.go wires it: the gateway-facing TLS proxy hands (w, r)
// straight through to the router with no hop in between, so serveLive's
// http.NewResponseController(w).EnableFullDuplex() governs the real fronting
// connection. The router is backed by a stub manager (countingManager over
// fixedEndpointManager) that admits every model and points /live at a mock
// full-duplex transcription child. No managed child process is launched, so this
// test is platform-independent.
//
// The property being proved and the failure it must catch:
//   - The mock child reads EXACTLY the first PCM chunk, THEN emits a
//     transcript.text.delta, THEN blocks for the rest. The delta is therefore
//     causally downstream of chunk 1 arriving at the child.
//   - The client drives its request body from an io.Pipe and writes chunk 2 only
//     AFTER it has received the delta. So receiving the delta within a bounded
//     timeout proves the delta came back while the request body was still open --
//     i.e. the relay interleaved read(body)/write(response).
//   - A buffering relay (an io.ReadAll of r.Body before forwarding) could not
//     hand the child a single byte until the whole upload completed; the upload
//     never completes (chunk 2 is gated on the delta, the delta on chunk 1
//     reaching the child), so the exchange deadlocks and the bounded timeouts
//     fail the test fast rather than hanging the suite.
func TestLiveFullDuplexInterleavesThroughProxyAndRouter(t *testing.T) {
	// Realistic PCM chunk sizes: 16 KiB mirrors production live-audio chunking
	// (16 kHz * 2 bytes/sample is 32 KiB/s). It is NOT a buffering workaround --
	// net/http flushes a chunked request body per chunk, so the test would pass
	// with tiny chunks too. Its soundness rests on the ContentLength == -1
	// (chunked) assertion and the interleave timing below, not on a buffer size.
	chunk1 := bytes.Repeat([]byte("PCM1"), 4096) // 16 KiB
	chunk2 := bytes.Repeat([]byte("PCM2"), 4096) // 16 KiB

	// Mock full-duplex transcription child. It flushes its SSE response headers
	// up front (so the relay's RoundTrip, and thus the client's Do, return before
	// any body byte), reads exactly the first chunk, emits the delta, then drains
	// the rest and closes the turn.
	childErr := make(chan error, 1)
	child := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A streaming relay sends the body chunked (ContentLength -1). A buffering
		// one (io.ReadAll + bytes.NewReader) would declare a Content-Length. Pin
		// it here so this test cannot pass against a buffering implementation.
		if r.ContentLength != -1 {
			t.Errorf("child received the body with ContentLength=%d (want -1, chunked): the relay buffered the upload instead of streaming it", r.ContentLength)
		}
		rc := http.NewResponseController(w)
		_ = rc.EnableFullDuplex() // let the child interleave reading the body with writing SSE
		w.Header().Set(headerContentType, "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if err := rc.Flush(); err != nil { // flush headers so the relay's RoundTrip returns with no body byte read yet
			childErr <- fmt.Errorf("flush headers: %w", err)
			return
		}

		first := make([]byte, len(chunk1))
		if _, err := io.ReadFull(r.Body, first); err != nil {
			childErr <- fmt.Errorf("read first chunk: %w", err)
			return
		}
		_, _ = io.WriteString(w, "data: {\"type\":\"transcript.text.delta\",\"delta\":\"one\"}\n\n")
		if err := rc.Flush(); err != nil {
			childErr <- fmt.Errorf("flush delta: %w", err)
			return
		}

		// Block for the remainder (chunk 2 + EOF), then close out the turn.
		rest, err := io.ReadAll(r.Body)
		if err != nil {
			childErr <- fmt.Errorf("read rest of body: %w", err)
			return
		}
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		_ = rc.Flush()
		if got := append(append([]byte{}, first...), rest...); !bytes.Equal(got, append(append([]byte{}, chunk1...), chunk2...)) {
			childErr <- fmt.Errorf("child assembled %d body bytes that do not match the two chunks sent", len(got))
			return
		}
		childErr <- nil
	}))
	defer child.Close()

	// The router: a stub manager admits every model and points it at the mock
	// child -- the same shape router_live_test.go uses. countingManager counts
	// release() so we can also assert the admission is held for the whole relay
	// and freed exactly once.
	cm := &countingManager{inner: fixedEndpointManager{endpoint: child.URL}}
	liveRouter := newRouter(cm)

	// Stand up the production TLS proxy and resolve its one loopback route to the
	// router IN PROCESS -- the prod localFirst hand-off.
	certDir := t.TempDir()
	pool := writeLiveTestLeaf(t, certDir)
	pm := proxy.New(certDir, "127.0.0.1")
	defer pm.Close()

	routerPort := grabFreePort(t) // the "published router port" the resolver matches (nothing listens there; the resolver diverts in process)
	pm.SetLocalUpstream(func(port int) http.Handler {
		if port == routerPort {
			return liveRouter
		}
		return nil
	})
	listen := grabFreePort(t)
	pm.Apply([]proxy.Route{{Listen: listen, Upstream: fmt.Sprintf("http://127.0.0.1:%d", routerPort)}})
	waitLiveRouteActive(t, pm, listen)

	// Drive the upload through an io.Pipe so the request body is streamed
	// (chunked) and produced WHILE the response is read. chunk 2 is gated on the
	// delta actually arriving.
	pr, pw := io.Pipe()
	deltaSeen := make(chan struct{}) // closed once the delta is in hand -> releases chunk 2
	abort := make(chan struct{})     // closed on test exit -> unblocks the producer on a failure path
	defer close(abort)
	producerErr := make(chan error, 1)
	go func() {
		if _, err := pw.Write(chunk1); err != nil {
			producerErr <- fmt.Errorf("write chunk 1: %w", err)
			return
		}
		select {
		case <-deltaSeen:
		case <-abort:
			// The interleave assertion failed and is tearing the test down; abandon
			// the body rather than leak this goroutine on <-deltaSeen forever.
			_ = pw.CloseWithError(errors.New("live interleave test aborted before chunk 2"))
			producerErr <- nil
			return
		}
		if _, err := pw.Write(chunk2); err != nil {
			producerErr <- fmt.Errorf("write chunk 2: %w", err)
			return
		}
		producerErr <- pw.Close()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	target := fmt.Sprintf("https://127.0.0.1:%d/live/whisper-1?sample_rate=16000&channels=1&sample_format=s16le&language=de", listen)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, pr)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set(headerContentType, "application/octet-stream")

	// Issue the request and read the relayed SSE OFF the test goroutine, so the
	// MAIN goroutine keeps its own bounded wait. This is what makes a buffering
	// regression fail fast instead of hanging: against a buffering relay the
	// client transport's write loop blocks reading the pipe (waiting for chunk 2,
	// which is gated on a delta that never comes), and that pins Do's return even
	// after the request context fires -- so a test that called Do inline would
	// hang on Do and never reach the timeout below. gotDelta fires the instant the
	// delta is in the relayed stream; reqDone carries the request's final outcome.
	gotDelta := make(chan struct{})
	relayed := make(chan []byte, 1)
	reqDone := make(chan error, 1)
	go func() {
		resp, err := liveHTTPSClient(pool).Do(req)
		if err != nil {
			reqDone <- fmt.Errorf("do streaming request through the proxy: %w", err)
			return
		}
		defer resp.Body.Close() //nolint:errcheck // read-side close, best-effort
		if resp.StatusCode != http.StatusOK {
			reqDone <- fmt.Errorf("status = %d, want 200", resp.StatusCode)
			return
		}
		if ct := resp.Header.Get(headerContentType); ct != "text/event-stream" {
			reqDone <- fmt.Errorf("Content-Type = %q, want text/event-stream", ct)
			return
		}
		br := bufio.NewReader(resp.Body)
		var acc []byte
		reported := false
		for {
			line, rerr := br.ReadBytes('\n')
			acc = append(acc, line...)
			if !reported && bytes.Contains(acc, []byte("transcript.text.delta")) {
				reported = true
				close(gotDelta)
			}
			if rerr != nil {
				if errors.Is(rerr, io.EOF) {
					relayed <- acc
					reqDone <- nil
					return
				}
				reqDone <- fmt.Errorf("read relayed SSE: %w", rerr)
				return
			}
		}
	}()

	// THE INTERLEAVE ASSERTION: the delta must arrive BEFORE chunk 2 is written.
	// chunk 2 is only written after close(deltaSeen), which only runs once the
	// delta is provably in hand -- so receiving it within the bound proves the
	// gateway-facing connection interleaved read(body)/write(response). A buffering
	// relay deadlocks (chunk 1 never reaches the child -> no delta -> no chunk 2),
	// and this bounded select then fails fast rather than hanging the suite. A
	// clean EOF cannot reach reqDone here (it would require chunk 2, which is gated
	// on this very delta), so an early reqDone can only be a genuine failure.
	select {
	case <-gotDelta:
		close(deltaSeen) // release chunk 2 now that the delta is provably in hand
	case err := <-reqDone:
		t.Fatalf("the request ended before any transcript delta arrived (no interleave): %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("delta not received before chunk 2 was written: the /live relay buffered the request body (full-duplex interleave broken -- check serveLive's EnableFullDuplex placement from Task 2)")
	}

	// Clean completion: the stream finished, the body round-tripped intact, and the
	// relayed stream carried both the delta and the terminal [DONE] frame.
	select {
	case err := <-reqDone:
		if err != nil {
			t.Fatalf("request: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the relayed stream did not complete within 5s after chunk 2 was released")
	}
	if err := <-producerErr; err != nil {
		t.Fatalf("request body producer: %v", err)
	}
	full := <-relayed
	for _, want := range []string{"transcript.text.delta", "data: [DONE]"} {
		if !bytes.Contains(full, []byte(want)) {
			t.Fatalf("relayed SSE %q does not contain %q", full, want)
		}
	}
	if err := <-childErr; err != nil {
		t.Fatalf("child: %v", err)
	}

	// The admission is held for the whole relay and released exactly once on exit.
	assertReleasedExactlyOnce(t, cm, 3*time.Second, 200*time.Millisecond)
}

// writeLiveTestLeaf generates a self-signed EC (P-256) leaf valid for 127.0.0.1
// and writes it to certDir as fullchain.pem + privkey.pem (the on-disk names the
// proxy cert holder loads), returning a RootCAs pool trusting it so an https
// client can verify the proxy without InsecureSkipVerify. It mirrors
// internal/proxy's own writeTestLeaf, which is unexported in that package's test
// binary and so cannot be reused across the package boundary.
func writeLiveTestLeaf(t *testing.T, certDir string) *x509.CertPool {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: "op-agent-live-fullduplex-test"},
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

// liveHTTPSClient builds an https client that trusts pool and never skips
// verification. No client-level Timeout: the request carries its own bounded
// context, and the interleave assertions are each bounded on their own.
func liveHTTPSClient(pool *x509.CertPool) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
		},
	}
}

// waitLiveRouteActive polls Status() until the given listen reports TLSActive, or
// fails -- the proxy-side readiness gate (mirrors internal/proxy's waitTLSActive,
// likewise unexported there).
func waitLiveRouteActive(t *testing.T, m *proxy.Manager, listen int) {
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
