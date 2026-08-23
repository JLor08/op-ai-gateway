// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package certissue

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// challengeServer serves the gateway's side of HTTP-01 out of the store, exactly
// like the production handler does.
func challengeServer(t *testing.T, store ChallengeStore) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.URL.Path, "/.well-known/acme-challenge/")
		keyAuth, ok := store.Get(token)
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte(keyAuth))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newTestClient(t *testing.T, dirURL string, store ChallengeStore) *ACMEClient {
	t.Helper()
	key, err := GenerateAccountKey()
	if err != nil {
		t.Fatal(err)
	}
	return &ACMEClient{DirectoryURL: dirURL, Email: "ops@example.test", AccountKey: key, Challenges: store}
}

func TestObtainIssuesCertificateForDomain(t *testing.T) {
	store := NewMemoryChallengeStore()
	chal := challengeServer(t, store)
	dir := newFakeDir(t, chal.URL)
	c := newTestClient(t, dir.base()+"/directory", store)

	if _, err := c.Register(context.Background()); err != nil {
		t.Fatalf("register: %v", err)
	}
	res, err := c.Obtain(context.Background(), "ai-server1.int.example.test")
	if err != nil {
		t.Fatalf("obtain: %v", err)
	}
	if res.Leaf == nil || len(res.Leaf.DNSNames) != 1 || res.Leaf.DNSNames[0] != "ai-server1.int.example.test" {
		t.Fatalf("leaf DNSNames = %v, want [ai-server1.int.example.test]", res.Leaf)
	}
	if !strings.Contains(res.FullchainPEM, "BEGIN CERTIFICATE") || strings.Count(res.FullchainPEM, "BEGIN CERTIFICATE") != 2 {
		t.Fatalf("fullchain should carry leaf + issuer, got %q", res.FullchainPEM)
	}
	if !strings.Contains(res.KeyPEM, "BEGIN EC PRIVATE KEY") {
		t.Fatalf("key PEM missing: %q", res.KeyPEM)
	}
	if len(res.Fingerprint) != 64 {
		t.Fatalf("fingerprint = %q, want 64 hex chars", res.Fingerprint)
	}
	// The token must be cleaned up so the public handler stops answering.
	if _, ok := store.Get(dir.token); ok {
		t.Fatal("challenge token still in the store after Obtain")
	}
}

func TestObtainFailsWhenChallengeInvalid(t *testing.T) {
	store := NewMemoryChallengeStore()
	chal := challengeServer(t, store)
	dir := newFakeDir(t, chal.URL)
	dir.failChallenge = true
	c := newTestClient(t, dir.base()+"/directory", store)
	if _, err := c.Register(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Obtain(context.Background(), "nope.int.example.test"); err == nil {
		t.Fatal("expected an error when the authorization goes invalid")
	}
	if _, ok := store.Get(dir.token); ok {
		t.Fatal("challenge token leaked after a failed order")
	}
}

func TestRetryAfterDetectsRateLimit(t *testing.T) {
	store := NewMemoryChallengeStore()
	chal := challengeServer(t, store)
	dir := newFakeDir(t, chal.URL)
	dir.rateLimit = true
	c := newTestClient(t, dir.base()+"/directory", store)
	if _, err := c.Register(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, err := c.Obtain(context.Background(), "limited.int.example.test")
	if err == nil {
		t.Fatal("expected a rate-limit error")
	}
	d, ok := RetryAfter(err)
	if !ok || d != 120*time.Second {
		t.Fatalf("RetryAfter = (%v,%v), want (2m,true)", d, ok)
	}
}

func TestObtainCoversEveryName(t *testing.T) {
	store := NewMemoryChallengeStore()
	chal := challengeServer(t, store)
	dir := newFakeDir(t, chal.URL)
	c := newTestClient(t, dir.base()+"/directory", store)

	if _, err := c.Register(context.Background()); err != nil {
		t.Fatalf("register: %v", err)
	}
	res, err := c.Obtain(context.Background(), "a.int.example.test", "b.int.example.test")
	if err != nil {
		t.Fatalf("obtain: %v", err)
	}
	if len(res.Leaf.DNSNames) != 2 {
		t.Fatalf("DNSNames = %v, want two names", res.Leaf.DNSNames)
	}
	if res.Leaf.DNSNames[0] != "a.int.example.test" || res.Leaf.DNSNames[1] != "b.int.example.test" {
		t.Fatalf("DNSNames = %v, want [a.int.example.test b.int.example.test]", res.Leaf.DNSNames)
	}
	// Every per-identifier challenge token must have been cleaned up so the
	// public handler stops answering for either name once the order finishes.
	for _, a := range dir.authzs {
		if _, ok := store.Get(a.token); ok {
			t.Fatalf("challenge token %q for %q still in the store after Obtain", a.token, a.identifier)
		}
	}
}

func TestObtainRejectsAnIPAddress(t *testing.T) {
	store := NewMemoryChallengeStore()
	chal := challengeServer(t, store)
	dir := newFakeDir(t, chal.URL)
	c := newTestClient(t, dir.base()+"/directory", store)

	if _, err := c.Register(context.Background()); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := c.Obtain(context.Background(), "10.0.0.5"); err == nil {
		t.Fatal("Obtain must refuse an IP address")
	}
}

func TestAccountKeyPEMRoundTrip(t *testing.T) {
	key, err := GenerateAccountKey()
	if err != nil {
		t.Fatal(err)
	}
	pemStr, err := MarshalECKeyPEM(key)
	if err != nil {
		t.Fatal(err)
	}
	back, err := ParseECKeyPEM(pemStr)
	if err != nil {
		t.Fatal(err)
	}
	if !back.Equal(key) {
		t.Fatal("round-tripped account key differs")
	}
}
