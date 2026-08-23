// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"net"
	"net/http"
	"strings"
	"time"
)

// agentSourcePeerIPTTL bounds how long agentSourceRefused reuses a cached
// ResolveGatewayPeerIP result. ResolveGatewayPeerIP makes a live NetBird
// management-API call (netbird.GetPeer) to resolve the gateway's own mesh peer
// IP, so calling it on every agent-telemetry/stream request would round-trip to
// NetBird on the hot agent path. Mirrors meshSwitchTTL (agent_mesh_gate.go). A
// genuinely-empty ("", nil) resolution -- no gateway peer selected -- is cached
// for this full window too: agentSourceRefused already fails OPEN on an
// unresolved mesh IP, so reusing that legitimate miss is safe. An ERROR-derived
// "" is cached only briefly instead (agentSourcePeerIPErrTTL) so a transient
// failure self-heals fast.
const agentSourcePeerIPTTL = 5 * time.Second

// agentSourcePeerIPErrTTL bounds how long an ERROR-derived "" (a transient
// resolve failure) is reused. It is far shorter than agentSourcePeerIPTTL so a
// blip self-heals within a request or two instead of holding the source gate
// fail-open (meshIP=="") for the full legitimate-empty window -- while still
// coalescing a burst of concurrent requests during a NetBird outage rather than
// round-tripping the management API on every one. A genuinely-empty ("", nil)
// resolution (no gateway peer selected) still gets the full agentSourcePeerIPTTL.
const agentSourcePeerIPErrTTL = 500 * time.Millisecond

// agentSourcePeerIPResolveTimeout caps the detached ResolveGatewayPeerIP round
// trip cachedGatewayPeerIP makes. It mirrors cmd/gateway's netbirdCallTimeout
// (the sync loop's per-call bound); the value lives here because the gateway
// package cannot import cmd/gateway.
const agentSourcePeerIPResolveTimeout = 15 * time.Second

// agentSourceRefused refuses an agent-listener request that did not arrive
// over the NetBird mesh, when the netbird_only setting is on. The connection's
// LOCAL address (net/http sets http.LocalAddrContextKey per connection) equals
// the gateway's own NetBird peer IP for a mesh-bound listener -- so this check
// is a no-op there -- but differs for a host-published bind (plaintext or TLS),
// which is exactly the case it exists to catch.
//
// Fails OPEN in every ambiguous case: nil Portal, netbird_only off, the mesh
// peer IP not resolvable, or no LocalAddr on the request context. A
// control-plane blip (a settings read hiccup, an unreachable NetBird API) must
// never cut agents off the fleet -- this mirrors meshGateRefuses' and the
// public-mux netbird_only gates' (routes()) fail-safe posture.
func (s *Server) agentSourceRefused(r *http.Request) bool {
	if s.Portal == nil || !s.Portal.NetbirdOnly(r.Context()) {
		return false
	}
	// Check the local-addr signal BEFORE resolving the mesh IP: a request built
	// outside a real net/http.Server connection (every test that drives
	// AgentHandler directly without setting http.LocalAddrContextKey) never
	// carries one, so failing open here first avoids an unnecessary
	// ResolveGatewayPeerIP call/round-trip on a signal we already can't use.
	la, _ := r.Context().Value(http.LocalAddrContextKey).(net.Addr)
	if la == nil {
		return false // no local-addr signal on the context -> fail-open
	}
	meshIP := s.cachedGatewayPeerIP(r.Context())
	if meshIP == "" {
		return false // mesh IP unresolvable -> fail-open
	}
	host, _, err := net.SplitHostPort(la.String())
	if err != nil {
		host = la.String()
	}
	// Compare as parsed IPs (not raw strings): a mesh IPv6 address can round-trip
	// through net.Addr.String() with different zone/leading-zero formatting than
	// what ResolveGatewayPeerIP returns even though it denotes the same address.
	localIP := net.ParseIP(strings.TrimSpace(host))
	peerIP := net.ParseIP(strings.TrimSpace(meshIP))
	if localIP == nil || peerIP == nil {
		// Either side failed to parse as an IP -- never manufacture a refusal from
		// malformed/unexpected input; fail open instead.
		return false
	}
	return !localIP.Equal(peerIP)
}

// cachedGatewayPeerIP returns the gateway's own NetBird peer IP, reusing a
// short-TTL cache (agentSourcePeerIPTTL, via settingCache.GetTTL -- ttlcache.go)
// so repeated agent requests do not each pay for a live NetBird management-API
// round trip. Returns "" on any resolution error or when no gateway peer is
// configured -- the caller (agentSourceRefused) treats that as fail-open,
// exactly like meshRequireTLSOn's cache treats a settings-store error as
// "disengaged".
func (s *Server) cachedGatewayPeerIP(ctx context.Context) string {
	return s.sourcePeerIP.GetTTL(ctx, func(ctx context.Context) (string, time.Duration) {
		// Resolve on a DETACHED context. This runs on the hot agent request path, so
		// ctx is the request context -- but a client on a host-published agent port
		// could otherwise open a connection, let this cache-miss round trip start on
		// ITS context, then cancel the connection: context.Canceled would surface as a
		// resolve error, cache "" for the TTL, and (agentSourceRefused fails open on
		// meshIP=="") flip the netbird_only source gate OPEN for every agent request in
		// that window -- the very party the gate refuses switching source isolation off
		// on demand. context.WithoutCancel severs client cancellation from the resolve;
		// a bounded timeout still caps the round trip so a wedged NetBird API cannot
		// stall the request forever.
		ctx2, cancel := context.WithTimeout(context.WithoutCancel(ctx), agentSourcePeerIPResolveTimeout)
		defer cancel()
		ip, err := s.Portal.ResolveGatewayPeerIP(ctx2)
		ttl := agentSourcePeerIPTTL
		if err != nil {
			// A transient failure fails open (meshIP=="") but must self-heal fast:
			// cache the miss for only agentSourcePeerIPErrTTL, never the full TTL, so
			// one blip cannot hold the gate open for the whole legitimate-empty window.
			ip = ""
			ttl = agentSourcePeerIPErrTTL
		}
		return strings.TrimSpace(ip), ttl
	})
}
