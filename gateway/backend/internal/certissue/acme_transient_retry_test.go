// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package certissue

import (
	"context"
	"testing"
)

// TestObtainSucceedsAfterTransientServerErrors proves the narrowing introduced
// by retryBackoffExceptRateLimit (acme.go) leaves x/crypto/acme's DEFAULT
// retry behavior intact for every response OTHER than a 429: a plain,
// generic 5xx (isRetriable(500) == true) is retried exactly like it would be
// with the library's own defaultBackoff, and the order still completes.
//
// Without this test nothing exercises that path at all -- every other test in
// this package drives either a fully happy order or the deliberately
// non-retried 429/invalid-authorization cases, so a change that accidentally
// widened the 429-only refusal (e.g. to "any retriable status") would pass
// every existing test while silently breaking the routine "the CA blipped,
// retry" case this test targets.
func TestObtainSucceedsAfterTransientServerErrors(t *testing.T) {
	store := NewMemoryChallengeStore()
	chal := challengeServer(t, store)
	dir := newFakeDir(t, chal.URL)
	// Two consecutive 500s on the very first authenticated POST of the order
	// flow (new-order): if the retry path were broken, Obtain would fail on
	// the first hit and never even reach the /authz/1 or /finalize/1 steps
	// this asserts on below.
	dir.failNextByPath = map[string]int{"/new-order": 2}
	c := newTestClient(t, dir.base()+"/directory", store)

	if _, err := c.Register(context.Background()); err != nil {
		t.Fatalf("register: %v", err)
	}
	res, err := c.Obtain(context.Background(), "flaky.int.example.test")
	if err != nil {
		t.Fatalf("obtain should succeed after transient 500s, got: %v", err)
	}
	if got := dir.failNextByPath["/new-order"]; got != 0 {
		t.Fatalf("failNextByPath[/new-order] = %d after Obtain, want 0 (both injected failures consumed)", got)
	}
	if res.Leaf == nil || len(res.Leaf.DNSNames) != 1 || res.Leaf.DNSNames[0] != "flaky.int.example.test" {
		t.Fatalf("leaf DNSNames = %v, want [flaky.int.example.test]", res.Leaf)
	}
}

// TestObtainSucceedsAfterBadNonceRetry proves the OTHER routine retriable
// case RFC 8555 expects a client to handle silently: a badNonce problem
// document (400, NOT retriable by status code alone -- isRetriable(400) is
// false) still gets retried by x/crypto/acme because isBadNonce is checked
// independently of isRetriable in Client.post (see http.go). This is exactly
// the case the acme.go doc comment on retryBackoffExceptRateLimit calls out
// by name ("the routine 'bad nonce' 400 the ACME protocol expects a client
// to retry once under normal operation"), yet nothing exercised it before
// this test.
func TestObtainSucceedsAfterBadNonceRetry(t *testing.T) {
	store := NewMemoryChallengeStore()
	chal := challengeServer(t, store)
	dir := newFakeDir(t, chal.URL)
	dir.badNonceOncePath = "/new-order"
	c := newTestClient(t, dir.base()+"/directory", store)

	if _, err := c.Register(context.Background()); err != nil {
		t.Fatalf("register: %v", err)
	}
	res, err := c.Obtain(context.Background(), "badnonce.int.example.test")
	if err != nil {
		t.Fatalf("obtain should succeed after a single badNonce response, got: %v", err)
	}
	if got := dir.badNonceOncePath; got != "" {
		t.Fatalf("badNonceOncePath = %q after Obtain, want \"\" (the one injected badNonce was consumed)", got)
	}
	if res.Leaf == nil || len(res.Leaf.DNSNames) != 1 || res.Leaf.DNSNames[0] != "badnonce.int.example.test" {
		t.Fatalf("leaf DNSNames = %v, want [badnonce.int.example.test]", res.Leaf)
	}
}
