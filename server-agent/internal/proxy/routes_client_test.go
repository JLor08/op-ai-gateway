// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package proxy

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestRoutesClientFetchesAndParses drives the routes client against a fake
// gateway: the first fetch (bearer-authed, no validator) returns the route set
// and its ETag; a second fetch carries If-None-Match and gets a 304, which the
// client surfaces as ErrNotModified ("unchanged"). The wire's app_id is parsed
// but dropped -- the Manager keys purely on listen+upstream.
func TestRoutesClientFetchesAndParses(t *testing.T) {
	const token = "tok"
	var gotPath, gotAuth, gotINM string
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotINM = r.Header.Get("If-None-Match")
		if gotINM == `"x"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"x"`)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"routes":[{"listen":8600,"upstream":"http://127.0.0.1:8080","app_id":"a1"}],"etag":"x"}`))
	}))
	defer srv.Close()

	c := NewRoutesClient(srv.URL, token, srv.Client())

	routes, etag, err := c.Fetch(context.Background())
	if err != nil {
		t.Fatalf("first Fetch: %v", err)
	}
	if gotPath != "/api/agent/v1/proxy-routes" {
		t.Fatalf("path = %q, want /api/agent/v1/proxy-routes", gotPath)
	}
	if gotAuth != "Bearer "+token {
		t.Fatalf("Authorization = %q, want Bearer %s", gotAuth, token)
	}
	if len(routes) != 1 || routes[0].Listen != 8600 || routes[0].Upstream != "http://127.0.0.1:8080" {
		t.Fatalf("routes = %+v, want [{8600 http://127.0.0.1:8080}]", routes)
	}
	if etag != `"x"` {
		t.Fatalf("etag = %q, want \"x\"", etag)
	}

	// Second fetch: the client must send the cached validator and treat the 304
	// as ErrNotModified.
	_, _, err = c.Fetch(context.Background())
	if !errors.Is(err, ErrNotModified) {
		t.Fatalf("second Fetch err = %v, want ErrNotModified", err)
	}
	if gotINM != `"x"` {
		t.Fatalf("If-None-Match = %q, want \"x\"", gotINM)
	}
	if calls != 2 {
		t.Fatalf("server calls = %d, want 2", calls)
	}
}

// TestRoutesClientErrorsAreSurfaced proves a non-2xx/304 status is a plain error
// (never a spurious empty route set), so the driver keeps its current routes
// rather than tearing running proxies down on a transient gateway hiccup.
func TestRoutesClientErrorsAreSurfaced(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	c := NewRoutesClient(srv.URL, "tok", srv.Client())
	routes, _, err := c.Fetch(context.Background())
	if err == nil {
		t.Fatalf("Fetch on 503: err = nil, want non-nil")
	}
	if errors.Is(err, ErrNotModified) {
		t.Fatalf("503 surfaced as ErrNotModified")
	}
	if routes != nil {
		t.Fatalf("routes = %+v on error, want nil", routes)
	}
}

// listenSet collapses a Status() snapshot to the set of desired listen ports.
func listenSet(ss []RouteStatus) map[int]bool {
	out := make(map[int]bool, len(ss))
	for _, s := range ss {
		out[s.Listen] = true
	}
	return out
}

// TestDriverSyncRoutesMergesAndKeepsCurrentOnError exercises the Driver glue:
// SyncRoutes fetches the gateway topology, merges the local routes (fallback), and
// Applies the resolved set; a transient fetch error KEEPS the currently-applied
// routes rather than tearing them down. The Manager runs with an empty certDir so
// no leaf loads and no port binds -- Status() still lists the desired (resolved)
// routes, which is what we assert on.
func TestDriverSyncRoutesMergesAndKeepsCurrentOnError(t *testing.T) {
	m := New(t.TempDir(), "127.0.0.1")
	defer m.Close()

	okSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"routes":[{"listen":8600,"upstream":"http://127.0.0.1:8080"}],"etag":"e1"}`))
	}))
	defer okSrv.Close()

	local := []Route{{Listen: 8601, Upstream: "http://127.0.0.1:8081"}}
	d := NewDriver(m, NewRoutesClient(okSrv.URL, "tok", okSrv.Client()), local, "fallback")
	d.SyncRoutes(context.Background())
	if got := listenSet(m.Status()); !got[8600] || !got[8601] {
		t.Fatalf("after SyncRoutes, desired listens = %v, want 8600 (gateway) and 8601 (local)", got)
	}

	// A transient fetch failure must not tear down the applied routes.
	failSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer failSrv.Close()
	dFail := NewDriver(m, NewRoutesClient(failSrv.URL, "tok", failSrv.Client()), local, "fallback")
	dFail.SyncRoutes(context.Background())
	if got := listenSet(m.Status()); !got[8600] || !got[8601] {
		t.Fatalf("after failing SyncRoutes, desired listens = %v, want unchanged 8600 and 8601", got)
	}
}
