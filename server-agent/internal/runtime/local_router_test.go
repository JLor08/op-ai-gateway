// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package runtime

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestDriverLocalRouterNilSafety pins the cheap guards on the hot path: a nil
// Driver, a driver that has never bound, and a non-positive port all resolve to
// "no local router" rather than panicking. proxy.localFirst calls this on EVERY
// proxied request, so a panic here would be a 500 on the inference path.
func TestDriverLocalRouterNilSafety(t *testing.T) {
	var nilDriver *Driver
	if h := nilDriver.LocalRouter(8600); h != nil {
		t.Fatalf("(*Driver)(nil).LocalRouter = %v, want nil", h)
	}
	d := newDriver(&fakeManager{}, &fakeSource{}, nil, nil, "")
	if h := d.LocalRouter(8600); h != nil {
		t.Fatalf("LocalRouter before any StartRouter = %v, want nil", h)
	}
	if h := d.LocalRouter(0); h != nil {
		t.Fatalf("LocalRouter(0) = %v, want nil", h)
	}
}

// TestDriverLocalRouterMatchesOnlyThePublishedPort is the port half of the
// same security boundary proxy.loopbackUpstreamPort guards: the reference
// carries the port and the handler TOGETHER, so a route naming some other
// loopback port can never be answered by the router.
func TestDriverLocalRouterMatchesOnlyThePublishedPort(t *testing.T) {
	d := newDriver(&fakeManager{}, &fakeSource{}, nil, nil, "")
	port := grabFreePort(t)
	if err := d.StartRouter(port); err != nil {
		t.Fatalf("StartRouter(%d): %v", port, err)
	}
	t.Cleanup(func() { d.Close() })

	if h := d.LocalRouter(port); h == nil {
		t.Fatalf("LocalRouter(%d) = nil, want the router handler", port)
	}
	if h := d.LocalRouter(port + 1); h != nil {
		t.Fatalf("LocalRouter(%d) = %v, want nil (only the published port resolves)", port+1, h)
	}
}

// TestDriverLocalRouterIsTheSameInstanceAsTheListener pins the deliberate
// property the publish site documents: ONE router instance serves the proxied
// (in-process) path and the mesh listener, so there is ONE http.Transport and
// ONE connection pool to the managed child processes. Two would silently double
// the pool and split keep-alives between them.
func TestDriverLocalRouterIsTheSameInstanceAsTheListener(t *testing.T) {
	d := newDriver(&fakeManager{}, &fakeSource{}, nil, nil, "")
	port := grabFreePort(t)
	if err := d.StartRouter(port); err != nil {
		t.Fatalf("StartRouter(%d): %v", port, err)
	}
	t.Cleanup(func() { d.Close() })

	d.routerMu.Lock()
	served := d.routerSrv.Handler
	d.routerMu.Unlock()
	if got := d.LocalRouter(port); got != served {
		t.Fatalf("LocalRouter returned %p, listener serves %p; must be the SAME instance", got, served)
	}
}

// TestDriverLocalRouterPublishedBeforeBind is the availability claim behind
// C1: the in-process path is decoupled from the socket, so a mesh bind that
// FAILS (the port is taken, or the derived mesh address is not on this host)
// still leaves the proxied path serving. 203.0.113.1 is RFC 5737 TEST-NET-3 and
// is not assigned to any local interface, so net.Listen can only fail.
func TestDriverLocalRouterPublishedBeforeBind(t *testing.T) {
	d := newDriver(&fakeManager{}, &fakeSource{}, nil, nil, "203.0.113.1")
	port := grabFreePort(t)
	if err := d.StartRouter(port); err == nil {
		t.Fatal("StartRouter against an unowned bind host = nil, want a bind failure")
	}
	t.Cleanup(func() { d.Close() })

	h := d.LocalRouter(port)
	if h == nil {
		t.Fatal("LocalRouter = nil after a failed bind; the in-process path must not depend on the socket")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("in-process GET /health = %d, want 200", rec.Code)
	}
}

// TestDriverStartRouterRetryKeepsLocalRouterPublished pins the reason the
// clear does NOT live inside stopRouterLocked: StartRouter calls that on every
// 60s Sync retry, so clearing there would blink the in-process path off and on
// once a minute for as long as the bind keeps failing. It also pins that the
// retry REUSES the published handler rather than building a fresh router (and
// with it a fresh http.Transport) each time.
func TestDriverStartRouterRetryKeepsLocalRouterPublished(t *testing.T) {
	d := newDriver(&fakeManager{}, &fakeSource{}, nil, nil, "203.0.113.1")
	port := grabFreePort(t)
	if err := d.StartRouter(port); err == nil {
		t.Fatal("StartRouter against an unowned bind host = nil, want a bind failure")
	}
	t.Cleanup(func() { d.Close() })
	first := d.LocalRouter(port)
	if first == nil {
		t.Fatal("LocalRouter = nil after the first failed bind")
	}
	for i := range 3 {
		if err := d.StartRouter(port); err == nil {
			t.Fatalf("StartRouter retry %d = nil, want a bind failure", i)
		}
		got := d.LocalRouter(port)
		if got == nil {
			t.Fatalf("LocalRouter = nil after retry %d; the retry must not open a nil window", i)
		}
		if got != first {
			t.Fatalf("retry %d replaced the published handler (%p -> %p); the connection pool must survive a retry", i, first, got)
		}
	}
}

// TestDriverStartRouterZeroClearsLocalRouter: listen<=0 means "no server_agent
// application on this server". The router is not served, and the proxied path
// must fall back to the dialled upstream (today's 502) rather than being
// answered by a router the agent is no longer running.
func TestDriverStartRouterZeroClearsLocalRouter(t *testing.T) {
	d := newDriver(&fakeManager{}, &fakeSource{}, nil, nil, "")
	port := grabFreePort(t)
	if err := d.StartRouter(port); err != nil {
		t.Fatalf("StartRouter(%d): %v", port, err)
	}
	t.Cleanup(func() { d.Close() })
	if d.LocalRouter(port) == nil {
		t.Fatal("LocalRouter = nil while the router is bound")
	}
	if err := d.StartRouter(0); err != nil {
		t.Fatalf("StartRouter(0): %v", err)
	}
	if h := d.LocalRouter(port); h != nil {
		t.Fatalf("LocalRouter(%d) = %v after StartRouter(0), want nil", port, h)
	}
}

// TestDriverCloseClearsLocalRouter: shutdown must take the in-process path down
// with the listener, or a proxied request arriving during drain would be served
// by a router whose manager main.go has already closed.
func TestDriverCloseClearsLocalRouter(t *testing.T) {
	d := newDriver(&fakeManager{}, &fakeSource{}, nil, nil, "")
	port := grabFreePort(t)
	if err := d.StartRouter(port); err != nil {
		t.Fatalf("StartRouter(%d): %v", port, err)
	}
	if d.LocalRouter(port) == nil {
		t.Fatal("LocalRouter = nil while the router is bound")
	}
	d.Close()
	if h := d.LocalRouter(port); h != nil {
		t.Fatalf("LocalRouter(%d) = %v after Close, want nil", port, h)
	}
}
