// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package trust

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// HTTPClient returns a client whose transport reloads changed sources before
// every new TLS connection and uses the requested timeout. Existing connections
// remain valid until the normal HTTP keep-alive lifecycle closes them.
func (s *Store) HTTPClient(timeout time.Duration) *http.Client {
	s.refreshFileSources()
	roots, generation := s.poolSnapshot()
	dynamic := &dynamicTransport{
		store:      s,
		generation: generation,
	}
	dynamic.transport = s.newHTTPTransport(roots, false)
	dynamic.websocketTransport = s.newHTTPTransport(roots, true)
	return &http.Client{
		Transport: dynamic,
		Timeout:   timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return fmt.Errorf("stopped after 10 redirects")
			}
			if len(via) > 0 && !sameOrigin(via[0].URL, req.URL) {
				return fmt.Errorf("refusing gateway cross-origin redirect")
			}
			return nil
		},
	}
}

func sameOrigin(first, next *url.URL) bool {
	firstScheme, firstHost, firstPort, firstOK := canonicalOrigin(first)
	nextScheme, nextHost, nextPort, nextOK := canonicalOrigin(next)
	return firstOK && nextOK && firstScheme == nextScheme && firstHost == nextHost && firstPort == nextPort
}

func canonicalOrigin(u *url.URL) (scheme, host, port string, ok bool) {
	if u == nil {
		return "", "", "", false
	}
	scheme = strings.ToLower(u.Scheme)
	host = strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if host == "" {
		return "", "", "", false
	}
	if ip := net.ParseIP(host); ip != nil {
		host = ip.String()
	}
	port = u.Port()
	if port == "" {
		switch scheme {
		case "http", "ws":
			port = "80"
		case "https", "wss":
			port = "443"
		default:
			return "", "", "", false
		}
	} else {
		numeric, err := strconv.ParseUint(port, 10, 16)
		if err != nil {
			return "", "", "", false
		}
		port = strconv.FormatUint(numeric, 10)
	}
	return scheme, host, port, true
}

type dynamicTransport struct {
	store *Store

	mu         sync.Mutex
	generation uint64
	transport  *http.Transport
	// websocketTransport advertises HTTP/1.1 only. RFC 6455 Upgrade is not
	// compatible with the ordinary HTTP/2 RoundTripper used by transport.
	websocketTransport *http.Transport
}

func (d *dynamicTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	d.store.refreshFileSources()
	roots, generation := d.store.poolSnapshot()

	d.mu.Lock()
	var old, oldWebSocket *http.Transport
	if generation != d.generation {
		old = d.transport
		oldWebSocket = d.websocketTransport
		d.transport = d.store.newHTTPTransport(roots, false)
		d.websocketTransport = d.store.newHTTPTransport(roots, true)
		d.generation = generation
	}
	transport := d.transport
	if isWebSocketUpgrade(req) {
		transport = d.websocketTransport
	}
	d.mu.Unlock()
	if old != nil {
		old.CloseIdleConnections()
	}
	if oldWebSocket != nil {
		oldWebSocket.CloseIdleConnections()
	}
	return transport.RoundTrip(req)
}

func (d *dynamicTransport) CloseIdleConnections() {
	d.mu.Lock()
	transport := d.transport
	websocketTransport := d.websocketTransport
	d.mu.Unlock()
	if transport != nil {
		transport.CloseIdleConnections()
	}
	if websocketTransport != nil {
		websocketTransport.CloseIdleConnections()
	}
}

func isWebSocketUpgrade(req *http.Request) bool {
	return headerHasToken(req.Header.Values("Connection"), "upgrade") &&
		headerHasToken(req.Header.Values("Upgrade"), "websocket")
}

func headerHasToken(values []string, want string) bool {
	for _, value := range values {
		for _, token := range strings.Split(value, ",") {
			if strings.EqualFold(strings.TrimSpace(token), want) {
				return true
			}
		}
	}
	return false
}

func (s *Store) newHTTPTransport(roots *x509.CertPool, http1Only bool) *http.Transport {
	base := http.DefaultTransport.(*http.Transport)
	transport := base.Clone()
	nextProtos := []string{"h2", "http/1.1"}
	transport.ForceAttemptHTTP2 = !http1Only
	if http1Only {
		nextProtos = []string{"http/1.1"}
		protocols := new(http.Protocols)
		protocols.SetHTTP1(true)
		transport.Protocols = protocols
		// An explicit empty TLSNextProto is the documented net/http switch
		// that disables automatic HTTP/2 on a Transport.
		transport.TLSNextProto = make(map[string]func(string, *tls.Conn) http.RoundTripper)
	}
	dialContext := transport.DialContext
	if dialContext == nil {
		dialContext = (&net.Dialer{}).DialContext
	}
	transport.TLSClientConfig = &tls.Config{
		RootCAs:            roots.Clone(),
		MinVersion:         tls.VersionTLS12,
		NextProtos:         append([]string(nil), nextProtos...),
		InsecureSkipVerify: s.tlsInsecure, // explicit OP_AGENT_TLS_INSECURE escape hatch only
	}
	transport.DialTLSContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		s.refreshFileSources()
		freshRoots, _ := s.poolSnapshot()
		host, _, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, fmt.Errorf("split TLS address %q: %w", addr, err)
		}
		config := &tls.Config{
			RootCAs:            freshRoots,
			MinVersion:         tls.VersionTLS12,
			ServerName:         host,
			NextProtos:         append([]string(nil), nextProtos...),
			InsecureSkipVerify: s.tlsInsecure, // explicit OP_AGENT_TLS_INSECURE escape hatch only
		}
		raw, err := dialContext(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		conn := tls.Client(raw, config)
		if err := conn.HandshakeContext(ctx); err != nil {
			raw.Close()
			return nil, err
		}
		return conn, nil
	}
	return transport
}
