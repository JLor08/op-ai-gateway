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

// TestApplicationEndpointIsUnchangedByProxyExclusion pins that
// ApplicationEndpoint needs NO knowledge of ProxyExcluded, which is the whole
// economy of the design: the invariant (excluded => ProxyListenPort == 0) makes
// the existing "https AND a non-zero proxy port" branch unreachable for an
// excluded application, so the derivation stays exactly as it was.
//
// The last case is the ONE residue this design accepts, stated out loud rather
// than left to be discovered: a row with the flag set AND a proxy port is
// unreachable through the API by construction (portal applyProxyExclusion
// clears the port on every write that sets the flag), and if a direct store
// write produces one anyway, the endpoint still points at the proxy listener —
// which the agent has been told to close. revertScopeExit is deliberately left
// unguarded so it can repair exactly that row.
func TestApplicationEndpointIsUnchangedByProxyExclusion(t *testing.T) {
	cases := []struct {
		name string
		app  Application
		want string
	}{
		{"excluded http", Application{Scheme: "http", Port: 8080, ProxyExcluded: true}, "http://host:8080"},
		{"excluded https on its own port", Application{Scheme: "https", Port: 8080, ProxyExcluded: true}, "https://host:8080"},
		{"excluded empty scheme still defaults to http", Application{Port: 8080, ProxyExcluded: true}, "http://host:8080"},
		{
			"THE RESIDUE: invariant-violating row (direct store write only)",
			Application{Scheme: "https", Port: 8080, ProxyListenPort: 8601, ProxyExcluded: true},
			"https://host:8601",
		},
	}
	for _, c := range cases {
		if got := ApplicationEndpoint(AIServer{Domain: "host"}, c.app); got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}
