// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package provider

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/routing"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// wsDialWait bounds every wait in these tests; nothing sleeps.
const wsDialWait = 5 * time.Second

// wsSeen is what the upstream observed about a WebSocket handshake.
type wsSeen struct {
	protoMajor    int
	requestURI    string
	authorization string
	origin        string
	subprotocols  string
	custom        string
}

// newWSEchoUpstream is a WebSocket upstream that records its handshake and echoes
// one message. With tls it is served over TLS with HTTP/2 enabled, so a client
// whose transport negotiates h2 would have to carry the upgrade over HTTP/2 --
// which cannot upgrade -- unless the handshake stays on HTTP/1.1.
func newWSEchoUpstream(t *testing.T, tls bool) (*httptest.Server, <-chan wsSeen) {
	t.Helper()
	seen := make(chan wsSeen, 1)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Sec-WebSocket-Key") == "" {
			w.WriteHeader(http.StatusNoContent) // an ordinary request: the protocol it came over is the answer
			return
		}
		seen <- wsSeen{
			protoMajor:    r.ProtoMajor,
			requestURI:    r.RequestURI,
			authorization: r.Header.Get("Authorization"),
			origin:        r.Header.Get("Origin"),
			subprotocols:  r.Header.Get("Sec-WebSocket-Protocol"),
			custom:        r.Header.Get("X-Upstream-Extra"),
		}
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.CloseNow() }()
		ctx := context.Background()
		typ, data, err := conn.Read(ctx)
		if err != nil {
			return
		}
		_ = conn.Write(ctx, typ, data)
		_, _, _ = conn.Read(ctx) // until the client closes
	}))
	if tls {
		srv.EnableHTTP2 = true
		srv.StartTLS()
	} else {
		srv.Start()
	}
	t.Cleanup(srv.Close)
	return srv, seen
}

func recvWSSeen(t *testing.T, seen <-chan wsSeen) wsSeen {
	t.Helper()
	select {
	case s := <-seen:
		return s
	case <-time.After(wsDialWait):
		t.Fatal("the upstream saw no handshake")
		return wsSeen{}
	}
}

// DialWebSocket dials endpoint+path?rawQuery, keeps the upgrade on HTTP/1.1 even over a
// transport that negotiates HTTP/2, applies the upstream credential (and its static
// extra headers) from the context the way every other upstream call does, and sends
// neither an Origin nor any subprotocol offer -- also when the caller's header carries
// them. The dialed socket relays a message both ways.
func TestDialWebSocketUpgradesOverHTTP11WithUpstreamAuthOnly(t *testing.T) {
	srv, seen := newWSEchoUpstream(t, true)
	client := NewOpenAICompatibleClient(srv.Client())
	target := routing.Target{Provider: routing.ProviderServerAgent, Endpoint: srv.URL}
	ctx := WithUpstreamAuthHeaders(context.Background(), "", "up-token", map[string]string{"X-Upstream-Extra": "yes"})
	ctx, cancel := context.WithTimeout(ctx, wsDialWait)
	defer cancel()
	header := http.Header{}
	header.Set("Origin", "https://browser.example.test")
	header.Set("Sec-WebSocket-Protocol", "realtime, openai-insecure-api-key.client-secret")

	conn, resp, err := client.DialWebSocket(ctx, target, "/realtime/qwen%233", "intent=transcription&x=1", header)
	if err != nil {
		t.Fatalf("DialWebSocket: %v", err)
	}
	defer func() { _ = conn.CloseNow() }()
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("handshake status = %d, want 101", resp.StatusCode)
	}
	got := recvWSSeen(t, seen)
	if got.protoMajor != 1 {
		t.Fatalf("the upgrade arrived over HTTP/%d, want HTTP/1.1", got.protoMajor)
	}
	// The same transport does negotiate HTTP/2 for an ordinary request, so the
	// HTTP/1.1 upgrade above is the dial's doing, not the server's.
	plain, err := srv.Client().Get(srv.URL + "/plain")
	if err != nil {
		t.Fatalf("plain GET: %v", err)
	}
	_ = plain.Body.Close()
	if plain.ProtoMajor != 2 {
		t.Fatalf("plain GET over HTTP/%d; the fixture must negotiate HTTP/2", plain.ProtoMajor)
	}
	if got.requestURI != "/realtime/qwen%233?intent=transcription&x=1" {
		t.Fatalf("request URI = %q, want the path and query as given", got.requestURI)
	}
	if got.authorization != "Bearer up-token" || got.custom != "yes" {
		t.Fatalf("upstream auth = %q / extra = %q, want the context's credential and extra header", got.authorization, got.custom)
	}
	if got.origin != "" || got.subprotocols != "" {
		t.Fatalf("Origin = %q, Sec-WebSocket-Protocol = %q; want neither sent upstream", got.origin, got.subprotocols)
	}
	if err := conn.Write(ctx, websocket.MessageText, []byte("ping")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if typ, data, err := conn.Read(ctx); err != nil || typ != websocket.MessageText || string(data) != "ping" {
		t.Fatalf("echo = %v %q %v, want text ping", typ, data, err)
	}
	_ = conn.Close(websocket.StatusNormalClosure, "")
}

// A dial without a credential in the context sends no Authorization at all: the
// client's own bearer is never this seam's to forward.
func TestDialWebSocketWithoutUpstreamAuthSendsNoAuthorization(t *testing.T) {
	srv, seen := newWSEchoUpstream(t, false)
	client := NewOpenAICompatibleClient(nil)
	ctx, cancel := context.WithTimeout(context.Background(), wsDialWait)
	defer cancel()
	header := http.Header{}
	header.Set("Authorization", "Bearer client-secret")

	conn, _, err := client.DialWebSocket(ctx, routing.Target{Provider: routing.ProviderServerAgent, Endpoint: srv.URL + "/"}, "/realtime/m", "", header)
	if err != nil {
		t.Fatalf("DialWebSocket: %v", err)
	}
	defer func() { _ = conn.CloseNow() }()
	got := recvWSSeen(t, seen)
	if got.authorization != "" {
		t.Fatalf("Authorization = %q, want none", got.authorization)
	}
	if got.requestURI != "/realtime/m" {
		t.Fatalf("request URI = %q, want /realtime/m (no stray '?')", got.requestURI)
	}
}

// An agent router that answers the handshake with its own refusal is reported as a
// RouterError carrying the router's status and code -- the error a chat completion
// against the same router gets -- so the gateway answers it as it answers that one.
// The response is handed back too.
func TestDialWebSocketReportsARouterRefusal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"error":{"code":"runtime.upstream_gone","message":"the child is gone"}}`))
	}))
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithTimeout(context.Background(), wsDialWait)
	defer cancel()

	conn, resp, err := NewOpenAICompatibleClient(nil).DialWebSocket(ctx, routing.Target{Provider: routing.ProviderServerAgent, Endpoint: srv.URL}, "/realtime/m", "", nil)
	if conn != nil {
		t.Fatal("a refused dial returned a connection")
	}
	var re *RouterError
	if !errors.As(err, &re) || re.Status != http.StatusBadGateway || re.Code != "runtime.upstream_gone" {
		t.Fatalf("err = %v, want a RouterError 502 runtime.upstream_gone", err)
	}
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v, want it to wrap ErrUnavailable", err)
	}
	if resp == nil || resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("resp = %v, want the router's 502", resp)
	}
}

// An upstream that cannot be reached at all is ErrUnavailable, with no response; the
// error names the endpoint and path but never the query, which is the client's.
func TestDialWebSocketUnreachableIsUnavailable(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	endpoint := srv.URL
	srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), wsDialWait)
	defer cancel()

	_, resp, err := NewOpenAICompatibleClient(nil).DialWebSocket(ctx, routing.Target{Provider: routing.ProviderServerAgent, Endpoint: endpoint}, "/realtime/m", "secret=in-the-query", nil)
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
	if strings.Contains(err.Error(), "in-the-query") || !strings.Contains(err.Error(), "/realtime/m") {
		t.Fatalf("err = %q, want the path named and the query left out", err)
	}
	if resp != nil {
		t.Fatalf("resp = %v, want none", resp)
	}
}

// A redirect is not followed: a WebSocket upstream that answers 3xx refused the
// handshake.
func TestDialWebSocketDoesNotFollowRedirects(t *testing.T) {
	followed := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/elsewhere" {
			followed <- struct{}{}
			return
		}
		http.Redirect(w, r, "/elsewhere", http.StatusFound)
	}))
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithTimeout(context.Background(), wsDialWait)
	defer cancel()

	_, resp, err := NewOpenAICompatibleClient(nil).DialWebSocket(ctx, routing.Target{Provider: routing.ProviderVLLM, Endpoint: srv.URL}, "/realtime/m", "", nil)
	if err == nil || !strings.Contains(err.Error(), "302") {
		t.Fatalf("err = %v, want a refused dial naming the 302", err)
	}
	if resp == nil || resp.StatusCode != http.StatusFound {
		t.Fatalf("resp = %v, want the 302", resp)
	}
	select {
	case <-followed:
		t.Fatal("the redirect was followed")
	default:
	}
}

// The Multiplexer dispatches a WebSocket dial to the target provider's client, and
// refuses -- without cross-routing to the fallback -- a provider whose client cannot
// dial one.
func TestMultiplexerDialWebSocketDispatchesByProvider(t *testing.T) {
	srv, seen := newWSEchoUpstream(t, false)
	ctx, cancel := context.WithTimeout(context.Background(), wsDialWait)
	defer cancel()
	m := NewMultiplexer(map[string]Client{
		routing.ProviderServerAgent: NewOpenAICompatibleClient(nil),
		routing.ProviderMock:        NewMock(),
	}, NewOpenAICompatibleClient(nil))

	conn, _, err := m.DialWebSocket(ctx, routing.Target{Provider: routing.ProviderServerAgent, Endpoint: srv.URL}, "/realtime/m", "", nil)
	if err != nil {
		t.Fatalf("DialWebSocket: %v", err)
	}
	_ = conn.CloseNow()
	recvWSSeen(t, seen)

	if _, _, err := m.DialWebSocket(ctx, routing.Target{Provider: routing.ProviderMock, Endpoint: srv.URL}, "/realtime/m", "", nil); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("mock dial err = %v, want ErrUnavailable (no cross-routing to the fallback)", err)
	}
	var nilMux *Multiplexer
	if _, _, err := nilMux.DialWebSocket(ctx, routing.Target{}, "/realtime/m", "", nil); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("nil multiplexer err = %v, want ErrUnavailable", err)
	}
}

// The dial's context bounds the handshake only: canceling it once the socket is open
// leaves the session running, so the gateway can dial with the request's context and
// still close the session itself, with a close frame, when that context ends.
func TestDialWebSocketOutlivesItsContext(t *testing.T) {
	srv, seen := newWSEchoUpstream(t, false)
	ctx, cancel := context.WithCancel(context.Background())
	conn, _, err := NewOpenAICompatibleClient(nil).DialWebSocket(ctx, routing.Target{Provider: routing.ProviderServerAgent, Endpoint: srv.URL}, "/realtime/m", "", nil)
	if err != nil {
		t.Fatalf("DialWebSocket: %v", err)
	}
	defer func() { _ = conn.CloseNow() }()
	recvWSSeen(t, seen)
	cancel()
	rw, done := context.WithTimeout(context.Background(), wsDialWait)
	defer done()
	if err := conn.Write(rw, websocket.MessageText, []byte("still here")); err != nil {
		t.Fatalf("write after the dial context ended: %v", err)
	}
	if _, data, err := conn.Read(rw); err != nil || string(data) != "still here" {
		t.Fatalf("echo after the dial context ended = %q, %v", data, err)
	}
	if err := conn.Close(websocket.StatusNormalClosure, ""); err != nil {
		t.Fatalf("close handshake after the dial context ended: %v", err)
	}
}
