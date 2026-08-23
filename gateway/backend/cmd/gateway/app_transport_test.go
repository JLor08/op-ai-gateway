// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/certissue"
	"testing"
	"time"
)

// newTestLeafServer builds an httptest.Server presenting a TLS leaf signed by a
// freshly minted test "internal CA" for the loopback IP the server listens on
// (127.0.0.1), and returns the server plus the CA's public certificate PEM (the
// trust anchor a client must be given to verify it).
func newTestLeafServer(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	ca, caCertPEM, _, err := certissue.NewCA("Test Internal CA", time.Hour)
	if err != nil {
		t.Fatalf("mint test CA: %v", err)
	}
	leaf, err := ca.IssueFor([]string{"127.0.0.1"}, time.Hour, time.Now())
	if err != nil {
		t.Fatalf("issue test leaf: %v", err)
	}
	cert, err := tls.X509KeyPair([]byte(leaf.FullchainPEM), []byte(leaf.KeyPEM))
	if err != nil {
		t.Fatalf("build tls.Certificate: %v", err)
	}
	ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	ts.TLS = &tls.Config{Certificates: []tls.Certificate{cert}}
	ts.StartTLS()
	return ts, caCertPEM
}

// TestOutboundAppTransportTrustsInternalCA is Task 8's Step 1 test: a client
// built via newOutboundAppTransport(<the internal CA's PEM>) must verify a leaf
// signed by that CA WITHOUT InsecureSkipVerify, while a client trusting only an
// UNRELATED CA must fail closed.
func TestOutboundAppTransportTrustsInternalCA(t *testing.T) {
	ts, caCertPEM := newTestLeafServer(t)
	defer ts.Close()

	t.Run("trusted CA verifies", func(t *testing.T) {
		transport := newOutboundAppTransport(caCertPEM)
		if transport.TLSClientConfig.InsecureSkipVerify {
			t.Fatal("InsecureSkipVerify must never be true")
		}
		client := &http.Client{Transport: transport}
		resp, err := client.Get(ts.URL)
		if err != nil {
			t.Fatalf("GET with the issuing CA trusted: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
	})

	t.Run("unrelated CA fails closed", func(t *testing.T) {
		_, otherCACertPEM, _, err := certissue.NewCA("Unrelated CA", time.Hour)
		if err != nil {
			t.Fatalf("mint unrelated CA: %v", err)
		}
		transport := newOutboundAppTransport(otherCACertPEM)
		if transport.TLSClientConfig.InsecureSkipVerify {
			t.Fatal("InsecureSkipVerify must never be true")
		}
		client := &http.Client{Transport: transport}
		resp, err := client.Get(ts.URL)
		if err == nil {
			resp.Body.Close()
			t.Fatal("expected a certificate verification error trusting only an unrelated CA, got nil")
		}
		var unknownAuthErr x509.UnknownAuthorityError
		if !errors.As(err, &unknownAuthErr) {
			t.Fatalf("expected an x509.UnknownAuthorityError, got: %v", err)
		}
	})

	t.Run("empty/unparsable bundle falls back to system roots (fail-safe)", func(t *testing.T) {
		for _, bundle := range []string{"", "not a pem bundle"} {
			transport := newOutboundAppTransport(bundle)
			if transport.TLSClientConfig.InsecureSkipVerify {
				t.Fatalf("bundle %q: InsecureSkipVerify must never be true", bundle)
			}
			sysPool, sysErr := x509.SystemCertPool()
			if sysErr != nil || sysPool == nil {
				continue // platform has no system pool -- nothing to compare
			}
			if !transport.TLSClientConfig.RootCAs.Equal(sysPool) {
				t.Fatalf("bundle %q: system root pool was not retained byte-for-byte", bundle)
			}
		}
		// The leaf-serving upstream must still fail to verify with no internal CA
		// trusted at all (confirms the fallback is "system roots only", not
		// "trust everything").
		client := &http.Client{Transport: newOutboundAppTransport("")}
		resp, err := client.Get(ts.URL)
		if err == nil {
			resp.Body.Close()
			t.Fatal("expected a verification error with system roots only, got nil")
		}
	})
}

// TestAppCAPoolLiveRefreshTrustsRotatedCA drives the live-refresh holder: a
// single long-lived *http.Client (as providerClients/the app-health prober
// hold) must start unable to verify the test leaf, then successfully verify it
// after appCAPool.set publishes the issuing CA -- WITHOUT rebuilding the
// client/transport, mirroring the CA-rotation refresh path wired in
// memoryDeps/sqliteDeps/postgresDeps via OnCABundleChanged.
func TestAppCAPoolLiveRefreshTrustsRotatedCA(t *testing.T) {
	ts, caCertPEM := newTestLeafServer(t)
	defer ts.Close()

	_, otherCACertPEM, _, err := certissue.NewCA("Unrelated CA", time.Hour)
	if err != nil {
		t.Fatalf("mint unrelated CA: %v", err)
	}

	pool := newAppCAPool(otherCACertPEM)
	client := &http.Client{Transport: pool.transport()}

	if resp, err := client.Get(ts.URL); err == nil {
		resp.Body.Close()
		t.Fatal("expected a verification error before the rotation, got nil")
	}

	pool.set(caCertPEM) // simulates the CA-rotation refresh (OnCABundleChanged)

	resp, err := client.Get(ts.URL)
	if err != nil {
		t.Fatalf("GET after the pool picked up the rotated CA: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

// fakeCABundleReader is a caBundleReader test double for
// refreshOutboundAppCAPool that does not require a real portal.Service.
type fakeCABundleReader struct {
	bundle string
	err    error
}

func (f fakeCABundleReader) CertificateCABundlePEM(context.Context) (string, error) {
	return f.bundle, f.err
}

// TestRefreshOutboundAppCAPoolIsFailSafe: a bundle-read error must leave the
// pool exactly as it was (log-and-continue), never panic or clear trust.
func TestRefreshOutboundAppCAPoolIsFailSafe(t *testing.T) {
	ts, caCertPEM := newTestLeafServer(t)
	defer ts.Close()

	pool := newAppCAPool(caCertPEM)
	client := &http.Client{Transport: pool.transport()}
	if resp, err := client.Get(ts.URL); err != nil {
		t.Fatalf("sanity GET before the failed refresh: %v", err)
	} else {
		resp.Body.Close()
	}

	refreshOutboundAppCAPool(context.Background(), fakeCABundleReader{err: fmt.Errorf("settings store unavailable")}, pool)

	resp, err := client.Get(ts.URL)
	if err != nil {
		t.Fatalf("GET after a failed refresh should still use the previous (still valid) pool: %v", err)
	}
	defer resp.Body.Close()
}
