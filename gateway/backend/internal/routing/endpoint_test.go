// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package routing

import "testing"

func TestApplicationEndpointSuffixes(t *testing.T) {
	cases := []struct {
		scheme, domain string
		port           int
		srv, app, want string
	}{
		{"https", "host", 8080, "", "", "https://host:8080"},
		{"https", "host", 8080, "models", "llama-7b", "https://host:8080/models/llama-7b"},
		{"https", "host", 8080, "/api/", "", "https://host:8080/api"},
		{"https", "host", 8080, "", "/v1beta/", "https://host:8080/v1beta"},
		{"", "host", 80, "a", "b", "http://host:80/a/b"}, // scheme defaults to http
	}
	for _, c := range cases {
		got := ApplicationEndpoint(
			AIServer{Domain: c.domain, ServerPathSuffix: c.srv},
			Application{Scheme: c.scheme, Port: c.port, AppPathSuffix: c.app},
		)
		if got != c.want {
			t.Errorf("srv=%q app=%q: got %q want %q", c.srv, c.app, got, c.want)
		}
	}
}

// TestApplicationEndpointProxyHTTPS pins the P4 proxied-HTTPS branch: an app in
// proxied state (Scheme "https" AND a non-zero ProxyListenPort) is reached at
// its ProxyListenPort (the agent's TLS-terminating proxy listener), not its
// plaintext upstream Port. A normal https app (ProxyListenPort 0) still uses
// Port, and an http app is entirely unchanged.
func TestApplicationEndpointProxyHTTPS(t *testing.T) {
	cases := []struct {
		name            string
		scheme          string
		port            int
		proxyListenPort int
		srv, app        string
		want            string
	}{
		{"proxied https uses ProxyListenPort", "https", 8080, 8600, "", "", "https://host:8600"},
		{"proxied https keeps suffixes", "https", 8080, 8600, "models", "llama-7b", "https://host:8600/models/llama-7b"},
		{"normal https (no proxy port) uses Port", "https", 8080, 0, "", "", "https://host:8080"},
		{"http with a proxy port is unchanged", "http", 8080, 8600, "", "", "http://host:8080"},
		{"http unchanged", "http", 8080, 0, "", "", "http://host:8080"},
	}
	for _, c := range cases {
		got := ApplicationEndpoint(
			AIServer{Domain: "host", ServerPathSuffix: c.srv},
			Application{Scheme: c.scheme, Port: c.port, ProxyListenPort: c.proxyListenPort, AppPathSuffix: c.app},
		)
		if got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}
