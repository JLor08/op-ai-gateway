// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package proxy

import (
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// SetLocalUpstream installs the resolver that turns a LOOPBACK route upstream
// into an in-process http.Handler. resolve is called with the upstream's port
// on every proxied request whose upstream is loopback-shaped
// (loopbackUpstreamPort); a non-nil return is served in process, a nil return
// falls through to the ordinary dialled reverse proxy.
//
// In production main.go passes runtime.Driver.LocalRouter here, which resolves
// exactly one port -- the runtime router's own currently published listen port
// -- and nil for everything else. That closes the C1 gap: the gateway publishes
// each proxy route with a hard-wired "http://127.0.0.1:<app.Port>" upstream,
// while under cert_mode=proxy the runtime router binds its MESH identity
// instead, so the dial had nothing to reach and every proxied request was a 502
// that never self-healed. The upstream string stops being an ADDRESS for that
// one route and becomes a ROUTE KEY the agent resolves locally.
//
// Pass nil to disable the in-process path entirely (the default: a Manager that
// was never given a resolver dials every route exactly as before).
//
// Call it before the first Apply. It takes effect for routes started AFTER it
// returns; a listener already running keeps the handler it was started with,
// which is why main.go wires it during startup rather than mid-flight.
func (m *Manager) SetLocalUpstream(resolve func(port int) http.Handler) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.localUpstream = resolve
}

// localFirst serves port's in-process handler when the resolver has one right
// now, and the dialled reverse proxy otherwise.
//
// The resolution happens per REQUEST, not once when the listener started. The
// two facts it joins arrive on different cadences -- the route set on the
// certificate cadence (6h WebSocket / 15min POST), the router's port on the
// runtime cadence (60s) -- so a decision taken once would freeze whichever won
// that race, which is precisely the accident this whole change exists to
// remove. It also means the path repairs itself the moment the router comes
// back, with no listener churn. The cost is one atomic load and a nil check.
type localFirst struct {
	resolve  func(int) http.Handler
	port     int
	fallback http.Handler
}

func (l *localFirst) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h := l.resolve(l.port); h != nil {
		// Handed over as-is, deliberately: the in-process handler is another
		// http.Handler on this same process, so there is no hop to describe.
		// It runs its own httputil.ReverseProxy for the real upstream hop and
		// does its own hop-by-hop hygiene there.
		h.ServeHTTP(w, r)
		return
	}
	l.fallback.ServeHTTP(w, r)
}

// loopbackUpstreamPort reports the port of u when -- and only when -- u is a
// bare plaintext loopback authority, i.e. exactly the shape whose whole meaning
// is "a plain HTTP server on this machine, on this port".
//
// THIS IS A SECURITY BOUNDARY. Its answer decides whether a request is sent to
// the address the route names or handed to a handler inside this process
// instead, so a false positive silently diverts traffic destined for another
// machine into this agent's own router. A false NEGATIVE costs nothing at all:
// the route is dialled, exactly as it was before this file existed. The
// predicate is therefore written to reject everything it is not certain about.
//
// Accepted: scheme "http"; no opaque part, no userinfo, no fragment; an empty
// or "/" path; no query (not even a forced empty one); a host that is either
// the exact name "localhost" (case-insensitively -- DNS names are
// case-insensitive) or an IP literal for which net.IP.IsLoopback holds; and an
// explicit port in 1..65535.
//
// Why each of the negatives matters:
//   - a non-"http" scheme means the upstream expects TLS (or something else
//     entirely); the in-process handler speaks neither;
//   - a path or a query would have been REWRITTEN onto every request by
//     NewSingleHostReverseProxy's director, and the in-process hand-off skips
//     the director, so honouring them is impossible and ignoring them would
//     silently drop the prefix;
//   - userinfo is the classic authority-confusion carrier
//     ("http://127.0.0.1:9000@elsewhere.example/" has host elsewhere.example);
//   - a NAME other than "localhost" is rejected even if it would resolve to a
//     loopback address, because what it resolves to is not knowable here and
//     can change under us;
//   - a missing port cannot be matched against the router's port at all.
func loopbackUpstreamPort(u *url.URL) (int, bool) {
	if u == nil || u.Scheme != "http" || u.Opaque != "" || u.User != nil {
		return 0, false
	}
	if u.Path != "" && u.Path != "/" {
		return 0, false
	}
	if u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawFragment != "" {
		return 0, false
	}
	host := u.Hostname()
	if host == "" {
		return 0, false
	}
	if !strings.EqualFold(host, "localhost") {
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsLoopback() {
			return 0, false
		}
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil || port <= 0 || port > 65535 {
		return 0, false
	}
	return port, true
}
