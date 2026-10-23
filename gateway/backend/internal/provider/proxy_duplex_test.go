// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package provider

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/routing"
	"strings"
	"testing"
	"time"
)

// duplexSeen is what the upstream observed about the request line and headers of
// a ProxyNativeDuplex call.
type duplexSeen struct {
	method           string
	requestURI       string
	contentType      string
	authorization    string
	contentLength    int64
	transferEncoding []string
}

// newDuplexEchoUpstream is a full-duplex upstream: it flushes its response
// headers before it has read a single body byte, then answers every request-body
// read with one SSE line ("data: echo <bytes>") and finishes with "data: done"
// once the body ends. Because each line is causally downstream of one read, a
// client that sees it while its own request body is still open has proven the two
// directions interleave. The request metadata is delivered on the returned channel.
func newDuplexEchoUpstream(t *testing.T) (*httptest.Server, <-chan duplexSeen) {
	t.Helper()
	seen := make(chan duplexSeen, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- duplexSeen{
			method:           r.Method,
			requestURI:       r.RequestURI,
			contentType:      r.Header.Get("Content-Type"),
			authorization:    r.Header.Get("Authorization"),
			contentLength:    r.ContentLength,
			transferEncoding: r.TransferEncoding,
		}
		rc := http.NewResponseController(w)
		_ = rc.EnableFullDuplex() // HTTP/1.1: allow writing the response before the request body is drained
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_ = rc.Flush() // headers out before any body byte is read
		buf := make([]byte, 1024)
		for {
			n, err := r.Body.Read(buf)
			if n > 0 {
				_, _ = io.WriteString(w, "data: echo "+string(buf[:n])+"\n\n")
				_ = rc.Flush()
			}
			if err != nil {
				break
			}
		}
		_, _ = io.WriteString(w, "data: done\n\n")
		_ = rc.Flush()
	}))
	t.Cleanup(upstream.Close)
	return upstream, seen
}

// readSSEData returns the next "data: ..." line from r, failing the test when none
// arrives within d. A blocked read is what a buffering client looks like, so the
// bound turns a deadlock into a fast, attributable failure.
func readSSEData(t *testing.T, br *bufio.Reader, d time.Duration) string {
	t.Helper()
	type result struct {
		line string
		err  error
	}
	done := make(chan result, 1)
	go func() {
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				done <- result{err: err}
				return
			}
			if line = strings.TrimRight(line, "\r\n"); strings.HasPrefix(line, "data: ") {
				done <- result{line: strings.TrimPrefix(line, "data: ")}
				return
			}
		}
	}()
	select {
	case res := <-done:
		if res.err != nil {
			t.Fatalf("reading the response stream: %v", res.err)
		}
		return res.line
	case <-time.After(d):
		t.Fatalf("no response data within %s while the request body was still open: the request was buffered, not streamed", d)
		return ""
	}
}

// proxyDuplexReturning calls ProxyNativeDuplex and fails the test if it has not
// returned the response within d. The call must hand the response back while the
// request body is still unwritten; an implementation that buffers the body before
// sending would block here forever, so the bound makes that a fast failure.
func proxyDuplexReturning(t *testing.T, ctx context.Context, c NativeDuplexProxyClient, target routing.Target, path string, body io.Reader, contentType string, d time.Duration) *ProxyResponse {
	t.Helper()
	type result struct {
		resp *ProxyResponse
		err  error
	}
	done := make(chan result, 1)
	go func() {
		resp, err := c.ProxyNativeDuplex(ctx, target, path, body, contentType)
		done <- result{resp, err}
	}()
	select {
	case res := <-done:
		if res.err != nil {
			t.Fatalf("ProxyNativeDuplex: %v", res.err)
		}
		return res.resp
	case <-time.After(d):
		t.Fatalf("ProxyNativeDuplex did not return within %s while the request body was still open: the body was buffered, not streamed", d)
		return nil
	}
}

func duplexTarget(endpoint string) routing.Target {
	return routing.Target{Endpoint: endpoint, Provider: routing.ProviderAudioCpp}
}

// ProxyNativeDuplex sends the request body chunked and hands back the response
// while that body is still open: the upstream's answer to chunk 1 is readable
// BEFORE chunk 2 is written, and chunk 2 is still delivered afterwards.
func TestProxyNativeDuplexStreamsChunkedBodyAndInterleavesResponse(t *testing.T) {
	upstream, seen := newDuplexEchoUpstream(t)
	c := NewOpenAICompatibleClient(upstream.Client())

	pr, pw := io.Pipe()
	t.Cleanup(func() { _ = pr.Close(); _ = pw.Close() })

	ctx, cancel := context.WithTimeout(WithUpstreamAuth(context.Background(), "", "upstream-token"), 15*time.Second)
	defer cancel()
	resp := proxyDuplexReturning(t, ctx, c, duplexTarget(upstream.URL+"/"), "/v1/audio/transcriptions/live?language=de&sample_rate=16000", pr, "application/octet-stream", 5*time.Second)
	defer resp.Body.Close()

	// The response is in hand with the request body still entirely unwritten.
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("response content-type = %q, want text/event-stream", ct)
	}
	br := bufio.NewReader(resp.Body)

	if _, err := io.WriteString(pw, "PCM-ONE"); err != nil {
		t.Fatalf("writing chunk 1: %v", err)
	}
	if got := readSSEData(t, br, 5*time.Second); got != "echo PCM-ONE" {
		t.Fatalf("first response line = %q, want %q", got, "echo PCM-ONE")
	}

	// Only now does the client send more: the exchange is genuinely two-way.
	if _, err := io.WriteString(pw, "PCM-TWO"); err != nil {
		t.Fatalf("writing chunk 2: %v", err)
	}
	if got := readSSEData(t, br, 5*time.Second); got != "echo PCM-TWO" {
		t.Fatalf("second response line = %q, want %q", got, "echo PCM-TWO")
	}

	_ = pw.Close() // end of the upload
	if got := readSSEData(t, br, 5*time.Second); got != "done" {
		t.Fatalf("final response line = %q, want %q", got, "done")
	}

	got := <-seen
	if got.method != http.MethodPost {
		t.Errorf("upstream method = %q, want POST", got.method)
	}
	if got.requestURI != "/v1/audio/transcriptions/live?language=de&sample_rate=16000" {
		t.Errorf("upstream request URI = %q, want the path and query joined onto the endpoint without a doubled slash", got.requestURI)
	}
	if got.contentType != "application/octet-stream" {
		t.Errorf("upstream content-type = %q, want the caller's media type verbatim", got.contentType)
	}
	if got.contentLength != -1 {
		t.Errorf("upstream saw ContentLength = %d, want -1 (a chunked, unsized body)", got.contentLength)
	}
	if len(got.transferEncoding) != 1 || got.transferEncoding[0] != "chunked" {
		t.Errorf("upstream Transfer-Encoding = %v, want [chunked]", got.transferEncoding)
	}
	if got.authorization != "Bearer upstream-token" {
		t.Errorf("upstream Authorization = %q, want the per-app upstream credential from ctx", got.authorization)
	}
}

// target.Timeout bounds a single buffered completion; a live transcription runs
// for as long as the speaker does, so the duplex path must not apply it (the
// caller owns the lifetime through ctx), just as doNativeProxy does not.
func TestProxyNativeDuplexDoesNotApplyTargetTimeout(t *testing.T) {
	upstream, _ := newDuplexEchoUpstream(t)
	c := NewOpenAICompatibleClient(upstream.Client())

	target := duplexTarget(upstream.URL)
	target.Timeout = 40 * time.Millisecond

	pr, pw := io.Pipe()
	t.Cleanup(func() { _ = pr.Close(); _ = pw.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	resp := proxyDuplexReturning(t, ctx, c, target, "/v1/audio/transcriptions/live", pr, "application/octet-stream", 5*time.Second)
	defer resp.Body.Close()
	br := bufio.NewReader(resp.Body)

	// Several exchanges spread over several multiples of the target timeout.
	for _, chunk := range []string{"A", "B", "C"} {
		time.Sleep(3 * target.Timeout)
		if _, err := io.WriteString(pw, chunk); err != nil {
			t.Fatalf("writing %q after %s: %v (target.Timeout truncated the exchange)", chunk, 3*target.Timeout, err)
		}
		if got := readSSEData(t, br, 5*time.Second); got != "echo "+chunk {
			t.Fatalf("response to %q = %q, want %q", chunk, got, "echo "+chunk)
		}
	}
	_ = pw.Close()
	if got := readSSEData(t, br, 5*time.Second); got != "done" {
		t.Fatalf("final response line = %q, want %q", got, "done")
	}
}

// An empty content type is sent as JSON rather than as a blank Content-Type
// header, exactly like the buffered ProxyNative path.
func TestProxyNativeDuplexDefaultsAnEmptyContentTypeToJSON(t *testing.T) {
	upstream, seen := newDuplexEchoUpstream(t)
	c := NewOpenAICompatibleClient(upstream.Client())

	resp, err := c.ProxyNativeDuplex(context.Background(), duplexTarget(upstream.URL), "/v1/audio/transcriptions/live", strings.NewReader("x"), "")
	if err != nil {
		t.Fatalf("ProxyNativeDuplex: %v", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)

	if got := (<-seen).contentType; got != jsonContentType {
		t.Fatalf("upstream Content-Type = %q, want %q", got, jsonContentType)
	}
}

// Even a body that has a known length (strings.Reader) is sent chunked: the
// duplex path never declares a length, because the body's size is not what bounds
// the exchange.
func TestProxyNativeDuplexSendsChunkedEvenForSizedReaders(t *testing.T) {
	upstream, seen := newDuplexEchoUpstream(t)
	c := NewOpenAICompatibleClient(upstream.Client())

	resp, err := c.ProxyNativeDuplex(context.Background(), duplexTarget(upstream.URL), "/live", strings.NewReader("sized"), "application/octet-stream")
	if err != nil {
		t.Fatalf("ProxyNativeDuplex: %v", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)

	if got := (<-seen).contentLength; got != -1 {
		t.Fatalf("upstream saw ContentLength = %d, want -1", got)
	}
}

// Transport failures map to the same sentinels the buffered path uses, so a caller
// classifies a duplex failure with the errors.Is checks it already has.
func TestProxyNativeDuplexMapsTransportErrors(t *testing.T) {
	t.Run("unreachable upstream is ErrUnavailable", func(t *testing.T) {
		dead := httptest.NewServer(http.NotFoundHandler())
		endpoint := dead.URL
		dead.Close()

		c := NewOpenAICompatibleClient(nil)
		_, err := c.ProxyNativeDuplex(context.Background(), duplexTarget(endpoint), "/live", strings.NewReader("x"), "application/octet-stream")
		if !errors.Is(err, ErrUnavailable) {
			t.Fatalf("err = %v, want it to wrap ErrUnavailable", err)
		}
	})

	t.Run("caller deadline is ErrTimeout", func(t *testing.T) {
		upstream := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			// Never answer. Drain the body first: net/http only starts watching the
			// connection (and so only cancels r.Context on a client disconnect) once an
			// unread request body is out of the way.
			_, _ = io.Copy(io.Discard, r.Body)
			<-r.Context().Done()
		}))
		t.Cleanup(upstream.Close)

		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		c := NewOpenAICompatibleClient(upstream.Client())
		_, err := c.ProxyNativeDuplex(ctx, duplexTarget(upstream.URL), "/live", strings.NewReader("x"), "application/octet-stream")
		if !errors.Is(err, ErrTimeout) {
			t.Fatalf("err = %v, want ErrTimeout", err)
		}
	})
}

// ---- Multiplexer dispatch ----

// The Multiplexer carries the duplex capability to the provider routing resolved
// to, by the same resolution ProxyNative uses.
func TestMultiplexerProxyNativeDuplexDispatchesToCapableProvider(t *testing.T) {
	upstream, seen := newDuplexEchoUpstream(t)
	m := NewMultiplexer(map[string]Client{routing.ProviderAudioCpp: NewOpenAICompatibleClient(upstream.Client())}, nil)

	var duplex NativeDuplexProxyClient = m // the Multiplexer is itself a NativeDuplexProxyClient
	resp, err := duplex.ProxyNativeDuplex(context.Background(), duplexTarget(upstream.URL), "/v1/audio/transcriptions/live", strings.NewReader("hello"), "application/octet-stream")
	if err != nil {
		t.Fatalf("ProxyNativeDuplex: %v", err)
	}
	defer resp.Body.Close()
	br := bufio.NewReader(resp.Body)
	if got := readSSEData(t, br, 5*time.Second); got != "echo hello" {
		t.Fatalf("response line = %q, want %q", got, "echo hello")
	}
	if got := <-seen; got.contentLength != -1 {
		t.Fatalf("upstream saw ContentLength = %d, want -1 through the multiplexer", got.contentLength)
	}
}

// An unregistered provider key resolves to the fallback client, exactly as it does
// for ProxyNative.
func TestMultiplexerProxyNativeDuplexResolvesTheFallback(t *testing.T) {
	upstream, _ := newDuplexEchoUpstream(t)
	m := NewMultiplexer(nil, NewOpenAICompatibleClient(upstream.Client()))

	resp, err := m.ProxyNativeDuplex(context.Background(), duplexTarget(upstream.URL), "/live", strings.NewReader("x"), "application/octet-stream")
	if err != nil {
		t.Fatalf("ProxyNativeDuplex: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

// A provider whose client lacks the duplex capability yields a clear
// ErrUnavailable-wrapped error -- including a client that DOES implement the
// buffered NativeProxyClient -- and does NOT cross-route to a capable fallback:
// the stream must reach the provider routing resolved to.
func TestMultiplexerProxyNativeDuplexUnsupportedProvider(t *testing.T) {
	upstream, _ := newDuplexEchoUpstream(t)
	capableFallback := NewOpenAICompatibleClient(upstream.Client())

	cases := map[string]Client{
		"no native proxy at all":    stubClient{},
		"buffered native proxy now": NewAnthropicClient(nil),
	}
	for name, client := range cases {
		t.Run(name, func(t *testing.T) {
			m := NewMultiplexer(map[string]Client{"vendor_x": client}, capableFallback)
			resp, err := m.ProxyNativeDuplex(context.Background(), routing.Target{Endpoint: upstream.URL, Provider: "vendor_x"}, "/live", strings.NewReader("x"), "application/octet-stream")
			if err == nil {
				_ = resp.Body.Close()
				t.Fatal("expected an error for a provider without the duplex capability, got a response (cross-routed to the fallback?)")
			}
			if !errors.Is(err, ErrUnavailable) {
				t.Fatalf("err = %v, want it to wrap ErrUnavailable", err)
			}
			if !strings.Contains(err.Error(), `"vendor_x"`) {
				t.Fatalf("err = %q, want it to name the provider", err)
			}
		})
	}
}

func TestMultiplexerProxyNativeDuplexWithoutAnyClientIsUnavailable(t *testing.T) {
	m := NewMultiplexer(nil, nil)
	if _, err := m.ProxyNativeDuplex(context.Background(), duplexTarget("http://127.0.0.1:1"), "/live", strings.NewReader("x"), ""); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
	var nilMux *Multiplexer
	if _, err := nilMux.ProxyNativeDuplex(context.Background(), duplexTarget("http://127.0.0.1:1"), "/live", strings.NewReader("x"), ""); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("nil multiplexer: err = %v, want ErrUnavailable", err)
	}
}

// The wide NativeProxyClient interface is deliberately NOT widened: the duplex
// capability is a separate, optional one, so clients that only buffer (Anthropic,
// Gemini, the Responses adapter, the gateway's test doubles) are unaffected.
func TestNativeDuplexProxyClientIsASeparateOptionalCapability(t *testing.T) {
	for name, client := range map[string]NativeProxyClient{
		"anthropic":        NewAnthropicClient(nil),
		"gemini":           NewGeminiClient(nil),
		"openai_responses": NewOpenAIResponsesClient(nil),
	} {
		if _, ok := client.(NativeDuplexProxyClient); ok {
			t.Errorf("%s unexpectedly implements NativeDuplexProxyClient; only the OpenAI-compatible client (and the Multiplexer over it) streams a request body", name)
		}
	}
	var _ NativeDuplexProxyClient = NewOpenAICompatibleClient(nil)
	var _ NativeDuplexProxyClient = NewMultiplexer(nil, nil)
}
