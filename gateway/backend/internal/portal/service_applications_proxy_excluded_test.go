// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"errors"
	"op-ai-gateway/internal/routing"
	"testing"
	"time"
)

// The proxy_excluded request fields are POINTERS on both the create and the
// update path precisely so "the caller said false" and "the caller said
// nothing" are different requests; boolPtr (service_test.go) is what lets these
// tests spell the difference.

// TestApplicationProxyExclusionRules pins the three rules that resolve the
// operator's participation decision. They are applied by ONE function called
// LAST in the mutation block of both CreateApplication and UpdateApplication,
// so both paths are driven here.
func TestApplicationProxyExclusionRules(t *testing.T) {
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()

	t.Run("rule 1 refuses an exclusion that also names a proxy port", func(t *testing.T) {
		svc, _ := newServerTestService(t, now)
		server := createTestServer(t, svc, "S", "s.example.test")
		// Silently zeroing a port the caller named IN THE SAME REQUEST would be
		// a lie about what was stored, so it is a refusal, not a normalization.
		_, err := svc.CreateApplication(ctx, ownerToken(), server.ID, CreateApplicationRequest{
			Type: routing.ProviderVLLM, Port: 8000, Scheme: "http",
			ProxyExcluded: boolPtr(true), ProxyListenPort: 9000,
		})
		if !errors.Is(err, ErrApplicationProxyExcludedPortConflict) {
			t.Fatalf("create: err = %v, want ErrApplicationProxyExcludedPortConflict", err)
		}

		app, err := svc.CreateApplication(ctx, ownerToken(), server.ID, CreateApplicationRequest{
			Type: routing.ProviderVLLM, Port: 8001, Scheme: "http",
		})
		if err != nil {
			t.Fatalf("create participating app: %v", err)
		}
		port := 9000
		_, err = svc.UpdateApplication(ctx, ownerToken(), app.ID, UpdateApplicationRequest{
			ProxyExcluded: boolPtr(true), ProxyListenPort: &port,
		})
		if !errors.Is(err, ErrApplicationProxyExcludedPortConflict) {
			t.Fatalf("update: err = %v, want ErrApplicationProxyExcludedPortConflict", err)
		}
		// Refused means NOTHING was written.
		stored, err := svc.routes.ApplicationByID(ctx, app.ID)
		if err != nil {
			t.Fatalf("read back: %v", err)
		}
		if stored.ProxyExcluded || stored.ProxyListenPort != 0 {
			t.Fatalf("a refused request wrote something: %+v", stored)
		}
	})

	t.Run("rule 1 clears the STORED port, which is a different matter", func(t *testing.T) {
		svc, routes := newServerTestService(t, now)
		server := createTestServer(t, svc, "S", "s.example.test")
		app, err := svc.CreateApplication(ctx, ownerToken(), server.ID, CreateApplicationRequest{
			Type: routing.ProviderVLLM, Port: 8000, Scheme: "http",
		})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		// The gateway assigned it a listener, exactly as AgentProxyRoutes would.
		stored, err := routes.ApplicationByID(ctx, app.ID)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		stored.ProxyListenPort = 8601
		if err := routes.UpdateApplication(ctx, stored); err != nil {
			t.Fatalf("assign port: %v", err)
		}

		dto, err := svc.UpdateApplication(ctx, ownerToken(), app.ID, UpdateApplicationRequest{
			ProxyExcluded: boolPtr(true),
		})
		if err != nil {
			t.Fatalf("exclude: %v", err)
		}
		if !dto.ProxyExcluded || dto.ProxyListenPort != 0 {
			t.Fatalf("dto = proxy_excluded %v / proxy_listen_port %d, want true / 0", dto.ProxyExcluded, dto.ProxyListenPort)
		}
	})

	t.Run("rule 2 refuses participation that the proxy could not front", func(t *testing.T) {
		svc, _ := newServerTestService(t, now)
		server := createTestServer(t, svc, "S", "s.example.test")
		// https with no proxy port describes an application the agent's proxy
		// cannot sit in front of: it forwards decrypted traffic to
		// http://127.0.0.1:<Port>, so a participant must speak plaintext there.
		_, err := svc.CreateApplication(ctx, ownerToken(), server.ID, CreateApplicationRequest{
			Type: routing.ProviderVLLM, Port: 8000, Scheme: "https",
			ProxyExcluded: boolPtr(false),
		})
		if !errors.Is(err, ErrApplicationProxyEntryScheme) {
			t.Fatalf("create: err = %v, want ErrApplicationProxyEntryScheme", err)
		}

		excluded, err := svc.CreateApplication(ctx, ownerToken(), server.ID, CreateApplicationRequest{
			Type: routing.ProviderVLLM, Port: 8001, Scheme: "https",
		})
		if err != nil {
			t.Fatalf("create own-tls app: %v", err)
		}
		if !excluded.ProxyExcluded {
			t.Fatalf("rule 3 did not fire on the own-tls create: %+v", excluded)
		}
		_, err = svc.UpdateApplication(ctx, ownerToken(), excluded.ID, UpdateApplicationRequest{
			ProxyExcluded: boolPtr(false),
		})
		if !errors.Is(err, ErrApplicationProxyEntryScheme) {
			t.Fatalf("update: err = %v, want ErrApplicationProxyEntryScheme", err)
		}
		// And the re-entry the portal actually performs — participation plus
		// http in ONE request — is accepted.
		scheme := "http"
		back, err := svc.UpdateApplication(ctx, ownerToken(), excluded.ID, UpdateApplicationRequest{
			ProxyExcluded: boolPtr(false), Scheme: &scheme,
		})
		if err != nil {
			t.Fatalf("re-enter the proxy: %v", err)
		}
		if back.ProxyExcluded || back.Scheme != "http" {
			t.Fatalf("after re-entry: %+v", back)
		}
	})

	t.Run("rule 3 translates the retired encoding, and only when nothing explicit was said", func(t *testing.T) {
		svc, _ := newServerTestService(t, now)
		server := createTestServer(t, svc, "S", "s.example.test")
		// A pre-70 client: scheme https, no proxy_listen_port, proxy_excluded
		// ABSENT. That describes the own-TLS state, which the flag now names.
		dto, err := svc.CreateApplication(ctx, ownerToken(), server.ID, CreateApplicationRequest{
			Type: routing.ProviderVLLM, Port: 8000, Scheme: "https",
		})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if !dto.ProxyExcluded {
			t.Fatalf("proxy_excluded = %v, want true from the normalization", dto.ProxyExcluded)
		}
		// It must NOT fire on a participating shape.
		httpApp, err := svc.CreateApplication(ctx, ownerToken(), server.ID, CreateApplicationRequest{
			Type: routing.ProviderVLLM, Port: 8001, Scheme: "http",
		})
		if err != nil {
			t.Fatalf("create http: %v", err)
		}
		if httpApp.ProxyExcluded {
			t.Fatalf("plain http create came back excluded: %+v", httpApp)
		}
	})

	t.Run("omitting proxy_excluded keeps the stored decision", func(t *testing.T) {
		svc, _ := newServerTestService(t, now)
		server := createTestServer(t, svc, "S", "s.example.test")
		app, err := svc.CreateApplication(ctx, ownerToken(), server.ID, CreateApplicationRequest{
			Type: routing.ProviderVLLM, Port: 8000, Scheme: "http",
			ProxyExcluded: boolPtr(true),
		})
		if err != nil {
			t.Fatalf("create excluded http app: %v", err)
		}
		if !app.ProxyExcluded {
			t.Fatalf("create: proxy_excluded = %v, want true", app.ProxyExcluded)
		}
		// An unrelated save must not restate — or clear — the operator's
		// opt-out. This is the nil-means-keep half of the sentinel, and it is
		// what the portal's seed-diff on the form depends on.
		weight := 7
		after, err := svc.UpdateApplication(ctx, ownerToken(), app.ID, UpdateApplicationRequest{Weight: &weight})
		if err != nil {
			t.Fatalf("unrelated update: %v", err)
		}
		if !after.ProxyExcluded {
			t.Fatalf("an unrelated save cleared the opt-out: %+v", after)
		}
		if after.Weight != 7 {
			t.Fatalf("weight = %d, want 7", after.Weight)
		}
	})

	t.Run("an excluded plain-http application is the case the feature exists for", func(t *testing.T) {
		svc, _ := newServerTestService(t, now)
		server := createTestServer(t, svc, "S", "s.example.test")
		app, err := svc.CreateApplication(ctx, ownerToken(), server.ID, CreateApplicationRequest{
			Type: routing.ProviderVLLM, Port: 8000, Scheme: "http",
			ProxyExcluded: boolPtr(true),
		})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		// http AND excluded — a state no combination of (scheme,
		// proxy_listen_port) could express before this column existed.
		if !app.ProxyExcluded || app.Scheme != "http" || app.ProxyListenPort != 0 {
			t.Fatalf("dto = %+v, want excluded http with no listener", app)
		}
		if app.Endpoint != "http://s.example.test:8000" {
			t.Fatalf("endpoint = %q, want the application's own plaintext port", app.Endpoint)
		}
	})
}

// TestApplicationProxyExclusionTransitionIsOneAtomicWrite is the case the
// design exists to prevent, driven end to end: an application that is running
// PROXIED is excluded and returned to its own plaintext port in a SINGLE PATCH.
//
// Atomicity is the only safe ordering, and it is why the flag must not be
// settable on its own. An excluded application on an IN-SCOPE server is in
// NEITHER recovery arm — the reconcile continues on a non-candidate, and
// revertScopeExit runs only for out-of-scope servers — so a half-applied state
// (https with the port already gone, or http with the port still held) would be
// PERMANENT, not transient. The assertions below therefore cover both the
// resulting row and the fact that neither arm subsequently touches it.
func TestApplicationProxyExclusionTransitionIsOneAtomicWrite(t *testing.T) {
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	svc, routes := newServerTestService(t, now)
	counting := &countingUpdateApplicationStore{Store: routes}
	svc.routes = counting
	server := createTestServer(t, svc, "S", "s.example.test")

	app, err := svc.CreateApplication(ctx, ownerToken(), server.ID, CreateApplicationRequest{
		Type: routing.ProviderVLLM, Port: 8000, Scheme: "http",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// Bring it to the proxied steady state exactly as the gateway would: the
	// routes derivation assigns 8601, the reconcile flips the scheme to https.
	stored, err := routes.ApplicationByID(ctx, app.ID)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	stored.ProxyListenPort = 8601
	stored.Scheme = "https"
	if err := routes.UpdateApplication(ctx, stored); err != nil {
		t.Fatalf("seed proxied state: %v", err)
	}
	proxied, err := svc.GetApplication(ctx, ownerToken(), app.ID)
	if err != nil {
		t.Fatalf("get proxied: %v", err)
	}
	if proxied.Endpoint != "https://s.example.test:8601" {
		t.Fatalf("proxied endpoint = %q, want the agent's TLS listener", proxied.Endpoint)
	}

	counting.updates = 0
	scheme := "http"
	after, err := svc.UpdateApplication(ctx, ownerToken(), app.ID, UpdateApplicationRequest{
		Scheme: &scheme, ProxyExcluded: boolPtr(true),
	})
	if err != nil {
		t.Fatalf("exclude: %v", err)
	}
	if counting.updates != 1 {
		t.Fatalf("the transition took %d store writes, want exactly 1 -- there must be no observable intermediate row", counting.updates)
	}
	if !after.ProxyExcluded || after.Scheme != "http" || after.ProxyListenPort != 0 {
		t.Fatalf("after exclusion: %+v, want excluded http with no listener", after)
	}
	// NO OUTAGE WINDOW: the endpoint is the same plaintext port the agent's
	// proxy had been forwarding decrypted traffic to all along, which the
	// application binds either way.
	if after.Endpoint != "http://s.example.test:8000" {
		t.Fatalf("endpoint = %q, want http://s.example.test:8000", after.Endpoint)
	}

	// Both recovery arms must now leave it alone, forever.
	svc.settings = NewMemorySystemSettings()
	setHTTPSSwitchMode(t, svc, ctx, "auto")
	svc.proxyStatus = &stubProxyStatus{byServer: map[string][]ProxyRouteStatus{
		server.ID: {{Listen: 8601, TLSActive: true}, {Listen: 8600, TLSActive: false}},
	}}
	counting.updates = 0
	for pass := 0; pass < 3; pass++ {
		svc.ReconcileHTTPSSwitch(ctx)
	}
	if counting.updates != 0 {
		t.Fatalf("the reconcile wrote %d times to an excluded application, want 0", counting.updates)
	}
	final, err := routes.ApplicationByID(ctx, app.ID)
	if err != nil {
		t.Fatalf("read final: %v", err)
	}
	if final.Scheme != "http" || final.ProxyExcluded != true || final.ProxyListenPort != 0 {
		t.Fatalf("final row = %+v", final)
	}
}

// TestApplicationProxyExclusionInvariantHoldsAtEveryServiceWritePath sweeps the
// whole request surface rather than the cases the rules were written for: for
// every combination of a requested scheme, a requested participation and a
// requested port, any request the service ACCEPTS must leave
// ProxyExcluded => ProxyListenPort == 0 true in the stored row, on BOTH write
// paths.
//
// The invariant is what lets ApplicationEndpoint, activePortStrings,
// revertScopeExit and HTTPSSwitchUnreachableApps stay untouched by this change,
// so it is worth asserting exhaustively rather than by example — a future field
// added after the rules in the mutation block would break it silently.
func TestApplicationProxyExclusionInvariantHoldsAtEveryServiceWritePath(t *testing.T) {
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	schemes := []string{"http", "https"}
	participation := []*bool{nil, boolPtr(true), boolPtr(false)}
	ports := []int{0, 9100}

	port := 8000
	for _, scheme := range schemes {
		for _, excluded := range participation {
			for _, proxyPort := range ports {
				port++
				svc, routes := newServerTestService(t, now)
				server := createTestServer(t, svc, "S", "s.example.test")

				created, createErr := svc.CreateApplication(ctx, ownerToken(), server.ID, CreateApplicationRequest{
					Type: routing.ProviderVLLM, Port: port, Scheme: scheme,
					ProxyExcluded: excluded, ProxyListenPort: proxyPort,
				})
				if createErr == nil {
					stored, err := routes.ApplicationByID(ctx, created.ID)
					if err != nil {
						t.Fatalf("read created: %v", err)
					}
					if stored.ProxyExcluded && stored.ProxyListenPort != 0 {
						t.Fatalf("CREATE scheme=%s excluded=%v port=%d violated the invariant: %+v",
							scheme, excluded, proxyPort, stored)
					}
					if created.ProxyExcluded != stored.ProxyExcluded {
						t.Fatalf("CREATE dto disagrees with the stored row: %v vs %v", created.ProxyExcluded, stored.ProxyExcluded)
					}
				}

				// Same sweep on the update path, starting from a plain http
				// application so every request shape is reachable.
				base, err := svc.CreateApplication(ctx, ownerToken(), server.ID, CreateApplicationRequest{
					Type: routing.ProviderVLLM, Port: port + 500, Scheme: "http",
				})
				if err != nil {
					t.Fatalf("create base: %v", err)
				}
				req := UpdateApplicationRequest{Scheme: &scheme, ProxyExcluded: excluded}
				if proxyPort != 0 {
					p := proxyPort
					req.ProxyListenPort = &p
				}
				if _, err := svc.UpdateApplication(ctx, ownerToken(), base.ID, req); err == nil {
					stored, err := routes.ApplicationByID(ctx, base.ID)
					if err != nil {
						t.Fatalf("read updated: %v", err)
					}
					if stored.ProxyExcluded && stored.ProxyListenPort != 0 {
						t.Fatalf("UPDATE scheme=%s excluded=%v port=%d violated the invariant: %+v",
							scheme, excluded, proxyPort, stored)
					}
				}
			}
		}
	}
}
