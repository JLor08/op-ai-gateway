// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package proxy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestManagerDialledPathInterleavesFullDuplex is the regression for FIX A2: the
// DIALLED reverse proxy (newReverseProxy -- the fallback used when the route is
// not resolved in process) must stream a full-duplex upstream, interleaving the
// inbound request body with the upstream response on the one gateway-facing
// HTTP/1.1 connection.
//
// The defect it catches: net/http drains up to ~256 KiB of the UNREAD inbound
// body before a non-full-duplex handler's response is flushed. On the dialled
// /live path those bytes are consumed by the server loop and never forwarded to
// the child, yet the httputil.ReverseProxy (FlushInterval:-1) still completes the
// session normally -- so the gateway bills audio the child never transcribed
// (ADR-041: never mis-bill). newReverseProxy must therefore call
// http.NewResponseController(w).EnableFullDuplex() per request before serving.
//
// Wiring: a real proxy.Manager with a TLS route whose Upstream is a FULL-DUPLEX
// child. No SetLocalUpstream is installed, so the route is DIALLED
// (newReverseProxy), never handed over in process -- exactly the path under test.
//
// The property proved and the failure it catches:
//   - The child reads EXACTLY the first body chunk, THEN writes a response chunk,
//     THEN blocks for the rest. The response chunk is causally downstream of
//     chunk 1 reaching the child.
//   - The client streams the request body from an io.Pipe and writes chunk 2 only
//     AFTER it has received the first response chunk. Receiving that chunk within
//     a bounded timeout proves the response came back while the request body was
//     still open -- a true read(body)/write(response) interleave.
//   - WITHOUT the EnableFullDuplex wrap, net/http withholds the response header
//     while it drains the inbound body: chunk 1 cannot come back as a response,
//     chunk 2 is gated on that response, the exchange deadlocks, and the bounded
//     select below fails fast. WITH the wrap, the first chunk arrives and the
//     session completes.
func TestManagerDialledPathInterleavesFullDuplex(t *testing.T) {
	certDir := t.TempDir()
	pool := writeTestLeaf(t, certDir)

	// 16 KiB chunks mirror a real live-audio frame (16 kHz * 2 bytes/sample =
	// 32 KiB/s) -- the production shape -- NOT a buffering workaround: net/http
	// flushes a chunked request body per chunk, so the interleave does not
	// depend on the chunk size. The test's soundness rests on the
	// ContentLength == -1 assertion below and the interleave timing.
	chunk1 := bytes.Repeat([]byte("PCM1"), 4096) // 16 KiB
	chunk2 := bytes.Repeat([]byte("PCM2"), 4096) // 16 KiB

	childErr := make(chan error, 1)
	child := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A streaming relay forwards the body chunked (ContentLength -1). A
		// buffering one (io.ReadAll + bytes.NewReader) would declare a length.
		// Pin it so this test cannot pass against a buffering relay.
		if r.ContentLength != -1 {
			t.Errorf("child received the body with ContentLength=%d (want -1, chunked): the inbound body was buffered, not streamed", r.ContentLength)
		}
		rc := http.NewResponseController(w)
		_ = rc.EnableFullDuplex() // let the child interleave reading the body with writing its response
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if err := rc.Flush(); err != nil { // flush headers so the relay's RoundTrip returns before any body byte
			childErr <- fmt.Errorf("flush headers: %w", err)
			return
		}
		first := make([]byte, len(chunk1))
		if _, err := io.ReadFull(r.Body, first); err != nil {
			childErr <- fmt.Errorf("read first chunk: %w", err)
			return
		}
		if _, err := io.WriteString(w, "resp-1\n"); err != nil {
			childErr <- fmt.Errorf("write first response chunk: %w", err)
			return
		}
		if err := rc.Flush(); err != nil {
			childErr <- fmt.Errorf("flush first response chunk: %w", err)
			return
		}
		rest, err := io.ReadAll(r.Body)
		if err != nil {
			childErr <- fmt.Errorf("read rest of body: %w", err)
			return
		}
		_, _ = io.WriteString(w, "resp-2\n")
		_ = rc.Flush()
		if got := append(append([]byte{}, first...), rest...); !bytes.Equal(got, append(append([]byte{}, chunk1...), chunk2...)) {
			childErr <- fmt.Errorf("child assembled %d body bytes that do not match the two chunks sent", len(got))
			return
		}
		childErr <- nil
	}))
	defer child.Close()

	m := New(certDir, "127.0.0.1")
	defer m.Close()

	// No SetLocalUpstream: the route is DIALLED through newReverseProxy -- the
	// exact path FIX A2 makes full-duplex. (With a resolver installed it would be
	// handed over in process instead, which this test must avoid.)
	listen := freePort(t)
	m.Apply([]Route{{Listen: listen, Upstream: child.URL}})
	waitTLSActive(t, m, listen)

	// Drive the upload through an io.Pipe so the body is streamed (chunked) and
	// produced WHILE the response is read; chunk 2 is gated on the first response
	// chunk actually arriving.
	pr, pw := io.Pipe()
	firstSeen := make(chan struct{}) // closed once the first response chunk is in hand -> releases chunk 2
	abort := make(chan struct{})     // closed on test exit -> unblocks the producer on a failure path
	defer close(abort)
	producerErr := make(chan error, 1)
	go func() {
		if _, err := pw.Write(chunk1); err != nil {
			producerErr <- fmt.Errorf("write chunk 1: %w", err)
			return
		}
		select {
		case <-firstSeen:
		case <-abort:
			_ = pw.CloseWithError(errors.New("A2 dialled interleave test aborted before chunk 2"))
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
	req.Header.Set("Content-Type", "application/octet-stream")

	// Issue the request and read the relayed response OFF the test goroutine so the
	// main goroutine keeps its own bounded wait: against a buffering relay the
	// client's body-write loop blocks on the pipe (chunk 2 gated on a response that
	// never comes), which would pin an inline Do() even past the context deadline.
	gotFirst := make(chan struct{})
	relayed := make(chan []byte, 1)
	reqDone := make(chan error, 1)
	go func() {
		resp, err := liveFullDuplexHTTPSClient(pool).Do(req)
		if err != nil {
			reqDone <- fmt.Errorf("do streaming request through the dialled proxy: %w", err)
			return
		}
		defer resp.Body.Close() //nolint:errcheck // read-side close, best-effort
		if resp.StatusCode != http.StatusOK {
			reqDone <- fmt.Errorf("status = %d, want 200", resp.StatusCode)
			return
		}
		br := bufio.NewReader(resp.Body)
		var acc []byte
		reported := false
		for {
			line, rerr := br.ReadBytes('\n')
			acc = append(acc, line...)
			if !reported && bytes.Contains(acc, []byte("resp-1")) {
				reported = true
				close(gotFirst)
			}
			if rerr != nil {
				if errors.Is(rerr, io.EOF) {
					relayed <- acc
					reqDone <- nil
					return
				}
				reqDone <- fmt.Errorf("read relayed response: %w", rerr)
				return
			}
		}
	}()

	// THE INTERLEAVE ASSERTION: the first response chunk must arrive BEFORE chunk 2
	// is written. A buffering (non-full-duplex) dialled relay deadlocks -- chunk 1
	// never comes back, so chunk 2 is never sent -- and this bounded select fails
	// fast rather than hanging the suite.
	select {
	case <-gotFirst:
		close(firstSeen) // release chunk 2 now that the first response chunk is provably in hand
	case err := <-reqDone:
		t.Fatalf("the request ended before the first response chunk arrived (no interleave): %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("first response chunk not received before chunk 2 was written: the DIALLED reverse proxy buffered the request body (FIX A2: newReverseProxy must EnableFullDuplex per request)")
	}

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
	for _, want := range []string{"resp-1", "resp-2"} {
		if !bytes.Contains(full, []byte(want)) {
			t.Fatalf("relayed response %q does not contain %q", full, want)
		}
	}
	if err := <-childErr; err != nil {
		t.Fatalf("child: %v", err)
	}
}

// liveFullDuplexHTTPSClient builds an https client that trusts pool and never
// skips verification. It sets no client-level Timeout (unlike the package's
// httpsClient): the request carries its own bounded context and the interleave
// assertions are each independently bounded, so a client deadline must not race
// them.
func liveFullDuplexHTTPSClient(pool *x509.CertPool) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
		},
	}
}
