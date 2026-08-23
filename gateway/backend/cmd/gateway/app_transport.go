// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"log"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
)

// newOutboundAppTransport builds an *http.Transport for gateway->application
// outbound calls (Complete/streaming, health/model-sync probes, loaded-model and
// context-size probes -- everything that dials an Application through the
// agent's TLS proxy). It trusts BOTH the system root store and caBundlePEM, the
// operator's internal CA bundle (see portal.Service.CertificateCABundlePEM,
// which publishes the current root plus the previous one during a rotation
// overlap window). It NEVER sets InsecureSkipVerify: an internal-CA leaf that
// does not chain to caBundlePEM (or, for a public upstream, to a system root)
// fails closed exactly like any other TLS client -- that is the whole point of
// this transport over the InsecureSkipVerify shortcut it replaces.
//
// A missing or unparsable bundle is NOT a startup failure: it falls back to
// system roots only (logged), so a fresh install with no internal CA minted
// yet still reaches public upstreams; only an internal-CA-signed leaf fails to
// verify until a CA exists.
func newOutboundAppTransport(caBundlePEM string) *http.Transport {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if transport.TLSClientConfig == nil {
		transport.TLSClientConfig = &tls.Config{}
	}
	transport.TLSClientConfig.RootCAs = buildAppTrustPool(caBundlePEM)
	// Explicit, not just "left at its zero value": this is the one property the
	// whole transport exists to guarantee, so it is asserted directly rather
	// than relied on implicitly.
	transport.TLSClientConfig.InsecureSkipVerify = false
	return transport
}

// buildAppTrustPool returns the system root pool plus every certificate
// caBundlePEM parses. A platform with no system pool (x509.SystemCertPool
// error, e.g. some minimal containers) falls back to an empty pool rather than
// failing outright -- the appended internal CA(s) still verify the one leaf
// that matters most, the internal application proxy; only public-upstream
// verification is affected, and only on such a platform.
func buildAppTrustPool(caBundlePEM string) *x509.CertPool {
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	if bundle := strings.TrimSpace(caBundlePEM); bundle != "" {
		if !pool.AppendCertsFromPEM([]byte(bundle)) {
			log.Printf("outbound app transport: internal CA bundle PEM did not parse; outbound application calls fall back to system roots only")
		}
	}
	return pool
}

// appCAPool holds the outbound-app trust root pool behind an atomic pointer so
// a CA rotation (or the initial mint) can publish a new pool without tearing
// down and reconstructing the long-lived *http.Client/*http.Transport
// instances already wired into NewOpenAICompatibleClient / NewOllamaClient
// (and, through the shared Multiplexer, the app-health prober) -- see
// transport() below for how a live client picks up the new pool.
type appCAPool struct {
	pool atomic.Pointer[x509.CertPool]
}

// newAppCAPool builds a pool primed from caBundlePEM (typically empty at this
// point -- see wireOutboundAppCA -- since the real bundle is not readable until
// after portal.Service exists; buildAppTrustPool's fallback makes an empty
// string a safe, system-roots-only starting point).
func newAppCAPool(caBundlePEM string) *appCAPool {
	p := &appCAPool{}
	p.set(caBundlePEM)
	return p
}

// set rebuilds the pool from caBundlePEM and atomically publishes it. Safe to
// call concurrently with transport()'s dials: a dial already in flight keeps
// whichever pool it already loaded (no torn reads), and the very next dial
// after set returns observes the new one.
func (p *appCAPool) set(caBundlePEM string) {
	p.pool.Store(buildAppTrustPool(caBundlePEM))
}

// current returns the live pool, falling back to a fresh system-roots-only pool
// in the (practically unreachable, since newAppCAPool always stores one) case
// the pointer was never populated.
func (p *appCAPool) current() *x509.CertPool {
	if pool := p.pool.Load(); pool != nil {
		return pool
	}
	return buildAppTrustPool("")
}

// transport builds an *http.Transport whose trusted roots are read from p on
// EVERY new outbound connection, via a DialTLSContext that performs the TLS
// handshake itself against p.current() -- instead of a static
// TLSClientConfig.RootCAs baked in once at construction (mutating that field
// on a live *http.Transport while dials are in flight would be a data race).
// That per-dial read is what lets a CA rotation (p.set, driven by
// portal.ServiceDeps.OnCABundleChanged) take effect immediately for the SAME
// *http.Transport / *http.Client already wired into the provider clients and
// the app-health prober, rather than requiring those long-lived instances to
// be reconstructed. InsecureSkipVerify is never set (explicit false).
// ForceAttemptHTTP2 is set because installing a custom DialTLSContext
// otherwise disables net/http's automatic HTTP/2 opt-in for this transport.
func (p *appCAPool) transport() *http.Transport {
	base := http.DefaultTransport.(*http.Transport).Clone()
	dial := base.DialContext
	base.TLSClientConfig = nil
	base.ForceAttemptHTTP2 = true
	base.DialTLSContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		rawConn, err := dial(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		host := addr
		if h, _, splitErr := net.SplitHostPort(addr); splitErr == nil {
			host = h
		}
		cfg := &tls.Config{
			RootCAs:            p.current(),
			ServerName:         host,
			NextProtos:         []string{"h2", "http/1.1"},
			InsecureSkipVerify: false,
		}
		tlsConn := tls.Client(rawConn, cfg)
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			_ = rawConn.Close()
			return nil, err
		}
		return tlsConn, nil
	}
	return base
}

// newOutboundAppCAClient builds the app CA pool and the shared *http.Client
// gateway->application traffic is built on (providerClients wires it into
// NewOpenAICompatibleClient/NewOllamaClient; the app-health prober is the same
// Multiplexer, so it inherits the same client). The pool starts system-roots-
// only -- refreshOutboundAppCAPool primes it with the real bundle once
// portal.Service exists (see memoryDeps/sqliteDeps/postgresDeps), which is
// necessary because the ModelLister the Service needs IS this client's
// Multiplexer, so the client must exist before the Service does.
func newOutboundAppCAClient() (*appCAPool, *http.Client) {
	pool := newAppCAPool("")
	return pool, &http.Client{Transport: pool.transport()}
}

// refreshOutboundAppCAPool re-reads the current internal CA trust bundle from
// portalService and republishes it into pool. Used both for the initial prime
// (right after portal.Service is constructed) and as the CA-rotation refresh:
// it is called from the OnCABundleChanged hook, which portal.Service invokes
// synchronously from inside newCA -- immediately after a first mint, a
// scheduled renewal, or a repair regeneration persists a new root (see
// internal/portal/service_certificates.go's newCA and ServiceDeps.
// OnCABundleChanged's doc, "must be non-blocking"). CertificateCABundlePEM only
// re-reads already-persisted system settings, so this is a fast local read, not
// a network call, and newCA's own call site is unaffected by any error here.
//
// Best-effort: a read failure logs and leaves the pool exactly as it was --
// fail-safe, matching newOutboundAppTransport's own contract, never blocking
// startup or a rotation on a transient settings-store hiccup.
//
// No periodic backstop is wired for this refresh (unlike most of this file's
// neighboring hooks, which document one): every code path that changes the
// persisted CA bundle is newCA, and newCA calls the hook this feeds
// unconditionally on success in the same function, so there is no scenario
// (short of a future caller bypassing newCA) where the bundle changes without
// this refresh firing.
func refreshOutboundAppCAPool(ctx context.Context, portalService caBundleReader, pool *appCAPool) {
	bundle, err := portalService.CertificateCABundlePEM(ctx)
	if err != nil {
		log.Printf("outbound app transport: CA bundle read failed; keeping current trust pool: %v", err)
		return
	}
	pool.set(bundle)
}

// caBundleReader is the narrow slice of *portal.Service refreshOutboundAppCAPool
// needs -- just enough to keep this file testable without constructing a real
// Service.
type caBundleReader interface {
	CertificateCABundlePEM(ctx context.Context) (string, error)
}
