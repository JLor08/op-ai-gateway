// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package runtime

import "net/http"

// localRouterRef is the currently published router: the port it is (or was
// asked to be) bound on, TOGETHER with the http.Handler that serves it.
//
// The two live in ONE reference on purpose. Published separately -- a port
// field and a handler field -- a reader could observe a fresh port beside a
// stale handler (or the reverse) and hand a request meant for the current
// router to the previous one. Swapping a single pointer makes the pair
// indivisible: a reader sees either both old values or both new ones.
type localRouterRef struct {
	listen  int
	handler http.Handler
}

// LocalRouter resolves port to this agent's own runtime router, or nil when
// port is not the router's currently published listen port (including "no
// router published at all"). It is the resolver
// proxy.Manager.SetLocalUpstream is handed: the agent-side TLS proxy calls it
// on EVERY proxied request whose upstream is a loopback address, and serves
// the returned handler in process instead of dialling.
//
// Cost is one atomic load plus a comparison; it takes no lock, so it never
// queues behind a router rebind. It is nil-safe in every direction (a nil
// Driver, a Driver that has never bound, a non-positive port) because the
// caller is on the inference path and a panic there would be a 500.
//
// Resolving PER REQUEST rather than once at listener start is load-bearing.
// The proxy's route set arrives on the certificate cadence (6h on WebSocket,
// 15min on POST) and the router's port on the runtime cadence (60s); a
// decision taken once would freeze whichever of the two won that race -- the
// same accident class as the bug this exists to fix. Per-request resolution
// also makes the path self-healing across an agent upgrade, and lets a
// disabled runtime (StartRouter(0)) fall back cleanly to the dialled
// upstream's pre-existing semantics.
func (d *Driver) LocalRouter(port int) http.Handler {
	if d == nil || port <= 0 {
		return nil
	}
	ref := d.localRouter.Load()
	if ref == nil || ref.listen != port {
		return nil
	}
	return ref.handler
}

// publishLocalRouter makes h the handler LocalRouter resolves for listen, or
// clears the reference entirely when listen is non-positive or h is nil.
// Callers hold routerMu, which serialises publishes against each other and
// against the listener lifecycle; readers need no lock (see LocalRouter).
func (d *Driver) publishLocalRouter(listen int, h http.Handler) {
	if listen <= 0 || h == nil {
		d.localRouter.Store(nil)
		return
	}
	d.localRouter.Store(&localRouterRef{listen: listen, handler: h})
}
