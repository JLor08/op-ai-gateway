// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package main

import (
	"context"
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/certissue"
	"op-ai-gateway/internal/provider"
	"op-ai-gateway/internal/routing"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// The production outbound transport (appCAPool.transport) does its own TLS handshake,
// so net/http's own rule that keeps a WebSocket upgrade off HTTP/2 does not reach it:
// against an h2-enabled upstream (the agent's TLS proxy, should it ever offer h2) a
// dial offering h2 fails with "http2: invalid Upgrade request header". The transport
// leaves h2 out for a handshake the provider marks RequiresHTTP1, so the realtime
// dial gets its 101 -- and every other request still negotiates HTTP/2.
func TestAppCAPoolTransportDialsAWebSocketOverHTTP11(t *testing.T) {
	ca, caCertPEM, _, err := certissue.NewCA("Test Internal CA", time.Hour)
	if err != nil {
		t.Fatalf("mint test CA: %v", err)
	}
	leaf, err := ca.IssueFor([]string{"127.0.0.1"}, time.Hour, time.Now())
	if err != nil {
		t.Fatalf("issue test leaf: %v", err)
	}
	cert, err := tls.X509KeyPair([]byte(leaf.FullchainPEM), []byte(leaf.KeyPEM))
	if err != nil {
		t.Fatalf("build tls.Certificate: %v", err)
	}
	upgrades := make(chan int, 1)
	ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Sec-WebSocket-Key") == "" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		upgrades <- r.ProtoMajor
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		_ = conn.Close(websocket.StatusNormalClosure, "")
	}))
	ts.TLS = &tls.Config{Certificates: []tls.Certificate{cert}}
	ts.EnableHTTP2 = true
	ts.StartTLS()
	defer ts.Close()

	pool := newAppCAPool(caCertPEM)
	client := &http.Client{Transport: pool.transport()}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, resp, err := provider.NewOpenAICompatibleClient(client).DialWebSocket(ctx, routing.Target{Provider: routing.ProviderServerAgent, Endpoint: ts.URL}, "/realtime/m", "", nil)
	if err != nil {
		t.Fatalf("DialWebSocket over the production transport: %v", err)
	}
	defer func() { _ = conn.CloseNow() }()
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("handshake status = %d, want 101", resp.StatusCode)
	}
	if proto := <-upgrades; proto != 1 {
		t.Fatalf("the upgrade arrived over HTTP/%d, want HTTP/1.1", proto)
	}

	plain, err := client.Get(ts.URL + "/plain")
	if err != nil {
		t.Fatalf("plain GET: %v", err)
	}
	_ = plain.Body.Close()
	if plain.ProtoMajor != 2 {
		t.Fatalf("plain GET over HTTP/%d; the transport must keep HTTP/2 for ordinary requests", plain.ProtoMajor)
	}
}
