// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package certissue

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	mathrand "math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"time"

	xacme "golang.org/x/crypto/acme"
)

// LetsEncryptURL is the production ACME directory (re-exported so callers need
// not import x/crypto/acme just for the default).
const LetsEncryptURL = xacme.LetsEncryptURL

// LetsEncryptStagingURL is the staging directory: same protocol, untrusted
// issuer, effectively no rate limits — the safe place to try a setup.
const LetsEncryptStagingURL = "https://acme-staging-v02.api.letsencrypt.org/directory"

// ACMEClient obtains certificates for single FQDNs over HTTP-01.
type ACMEClient struct {
	DirectoryURL string
	Email        string
	AccountKey   crypto.Signer
	// AccountURI, when known, is passed to the underlying client so it reuses the
	// existing registration instead of re-registering.
	AccountURI string
	Challenges ChallengeStore
	HTTPClient *http.Client
}

func (c *ACMEClient) client() *xacme.Client {
	return &xacme.Client{
		Key:          c.AccountKey,
		DirectoryURL: c.DirectoryURL,
		HTTPClient:   c.HTTPClient,
		KID:          xacme.KeyID(c.AccountURI),
		// x/crypto/acme's own default backoff sleeps the calling goroutine for
		// EVERY retriable response, including a genuine 429 rate-limit, where it
		// honors the CA's Retry-After verbatim (which can be minutes). Obtain is
		// meant to be called from a reconcile loop that makes its OWN
		// retry/backoff decision via the exported RetryAfter — so ONLY a 429 is
		// refused a retry here (surfaced immediately as an error); every other
		// retriable response (5xx, and the routine "bad nonce" 400 the ACME
		// protocol expects a client to retry once under normal operation, e.g.
		// under latency or clock skew) still gets the library's normal
		// exponential-backoff-with-jitter retry.
		RetryBackoff: retryBackoffExceptRateLimit,
	}
}

// retryBackoffExceptRateLimit reproduces x/crypto/acme's own default RetryBackoff
// (see the field's doc comment: Retry-After-if-present else a truncated
// exponential backoff, both plus up to 1s of jitter) for every response EXCEPT a
// 429 — which returns 0 (no retry) so Obtain fails fast instead of blocking on
// the CA's cool-down. The 429 branch is checked before it CAN see the header (a
// 429 always carries Retry-After), so this never depends on evaluation order.
func retryBackoffExceptRateLimit(n int, _ *http.Request, resp *http.Response) time.Duration {
	if resp != nil && resp.StatusCode == http.StatusTooManyRequests {
		return 0
	}
	const maxBackoff = 10 * time.Second
	jitter := time.Duration(mathrand.IntN(1000)+1) * time.Millisecond
	if resp != nil {
		if d, ok := parseRetryAfterHeader(resp.Header.Get("Retry-After")); ok {
			return d + jitter
		}
	}
	if n < 1 {
		n = 1
	}
	if n > 30 {
		n = 30
	}
	d := time.Duration(1<<uint(n-1))*time.Second + jitter
	if d > maxBackoff {
		return maxBackoff
	}
	return d
}

// Register creates (or re-uses) the ACME account for AccountKey and returns its
// URI. An already-registered key yields its existing account. As a side effect
// it caches the URI on AccountURI, so a subsequent Obtain on the SAME client
// signs in "kid" form straight away instead of making the CA look its account
// up by key (an extra round trip that some directories — including ours in
// tests — do not distinguish from a fresh account-create call).
func (c *ACMEClient) Register(ctx context.Context) (string, error) {
	if c.AccountKey == nil {
		return "", errors.New("acme: no account key")
	}
	acct := &xacme.Account{}
	if c.Email != "" {
		acct.Contact = []string{"mailto:" + c.Email}
	}
	got, err := c.client().Register(ctx, acct, xacme.AcceptTOS)
	if err != nil {
		if errors.Is(err, xacme.ErrAccountAlreadyExists) && got != nil {
			c.AccountURI = got.URI
			return got.URI, nil
		}
		return "", fmt.Errorf("acme register: %w", err)
	}
	c.AccountURI = got.URI
	return got.URI, nil
}

// Obtain runs one HTTP-01 order covering every given name and returns the issued
// certificate. The challenge token is registered in Challenges before the
// challenge is accepted and removed on EVERY exit path (defer), so the public
// handler never keeps answering for a finished order.
//
// IP addresses are NOT accepted here: Let's Encrypt issues IP certificates only
// under a short-lived profile that this gateway does not use, and the caller
// rejects an IP in the acme mode long before this point (settings validation).
func (c *ACMEClient) Obtain(ctx context.Context, names ...string) (Result, error) {
	if c.Challenges == nil {
		return Result{}, errors.New("acme: no challenge store")
	}
	dns, ips, err := SplitNames(names)
	if err != nil {
		return Result{}, err
	}
	if len(ips) > 0 {
		return Result{}, fmt.Errorf("acme: cannot order a certificate for an IP address (%s)", ips[0])
	}
	cl := c.client()
	order, err := cl.AuthorizeOrder(ctx, xacme.DomainIDs(dns...))
	if err != nil {
		return Result{}, fmt.Errorf("acme authorize order for %s: %w", strings.Join(dns, ","), err)
	}
	for _, authzURL := range order.AuthzURLs {
		authz, err := cl.GetAuthorization(ctx, authzURL)
		if err != nil {
			return Result{}, fmt.Errorf("acme get authorization: %w", err)
		}
		if authz.Status == xacme.StatusValid {
			continue
		}
		chal := httpChallenge(authz)
		if chal == nil {
			return Result{}, fmt.Errorf("acme: no http-01 challenge offered for %s", strings.Join(dns, ","))
		}
		keyAuth, err := cl.HTTP01ChallengeResponse(chal.Token)
		if err != nil {
			return Result{}, fmt.Errorf("acme challenge response: %w", err)
		}
		c.Challenges.Put(chal.Token, keyAuth)
		// Removed on success AND on any error below.
		defer c.Challenges.Delete(chal.Token)
		if _, err := cl.Accept(ctx, chal); err != nil {
			return Result{}, fmt.Errorf("acme accept challenge: %w", err)
		}
		if _, err := cl.WaitAuthorization(ctx, authzURL); err != nil {
			return Result{}, fmt.Errorf("acme authorization for %s: %w", strings.Join(dns, ","), err)
		}
	}
	certKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return Result{}, fmt.Errorf("acme generate certificate key: %w", err)
	}
	csrDER, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject:  pkix.Name{CommonName: dns[0]},
		DNSNames: dns,
	}, certKey)
	if err != nil {
		return Result{}, fmt.Errorf("acme create csr: %w", err)
	}
	chain, _, err := cl.CreateOrderCert(ctx, order.FinalizeURL, csrDER, true)
	if err != nil {
		return Result{}, fmt.Errorf("acme finalize order for %s: %w", strings.Join(dns, ","), err)
	}
	if len(chain) == 0 {
		return Result{}, fmt.Errorf("acme: empty certificate chain for %s", strings.Join(dns, ","))
	}
	leaf, err := x509.ParseCertificate(chain[0])
	if err != nil {
		return Result{}, fmt.Errorf("acme parse leaf: %w", err)
	}
	var sb strings.Builder
	for _, der := range chain {
		if err := pem.Encode(&sb, &pem.Block{Type: "CERTIFICATE", Bytes: der}); err != nil {
			return Result{}, fmt.Errorf("acme encode chain: %w", err)
		}
	}
	keyPEM, err := MarshalECKeyPEM(certKey)
	if err != nil {
		return Result{}, err
	}
	sum := sha256.Sum256(leaf.Raw)
	return Result{
		FullchainPEM: sb.String(),
		KeyPEM:       keyPEM,
		Fingerprint:  hex.EncodeToString(sum[:]),
		Leaf:         leaf,
	}, nil
}

func httpChallenge(authz *xacme.Authorization) *xacme.Challenge {
	for _, ch := range authz.Challenges {
		if ch.Type == "http-01" {
			return ch
		}
	}
	return nil
}

// RetryAfter reports the server-requested wait from an ACME error (a rate-limit
// answer carries Retry-After). ok=false when the error is not an ACME problem
// document or carries no usable Retry-After.
func RetryAfter(err error) (time.Duration, bool) {
	var aerr *xacme.Error
	if !errors.As(err, &aerr) {
		return 0, false
	}
	return parseRetryAfterHeader(aerr.Header.Get("Retry-After"))
}

// parseRetryAfterHeader parses a raw HTTP Retry-After value (seconds or an
// HTTP-date) into a Duration; ok=false on an empty/unparseable value. Shared by
// RetryAfter (reading it back off a returned *xacme.Error) and
// retryBackoffExceptRateLimit (reading it live off the response mid-request),
// so the two never disagree on what counts as a usable Retry-After.
func parseRetryAfterHeader(raw string) (time.Duration, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, false
	}
	if secs, convErr := strconv.Atoi(raw); convErr == nil && secs >= 0 {
		return time.Duration(secs) * time.Second, true
	}
	if when, parseErr := http.ParseTime(raw); parseErr == nil {
		if d := time.Until(when); d > 0 {
			return d, true
		}
		return 0, true
	}
	return 0, false
}
