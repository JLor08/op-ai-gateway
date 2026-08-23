// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package certissue

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// fakeDir is a minimal ACME v2 directory backed by a throwaway CA. challengeBase
// is the base URL it fetches the HTTP-01 token from (the test's own challenge
// server). failChallenge makes the challenge validation report "invalid".
type fakeDir struct {
	srv           *httptest.Server
	caKey         *ecdsa.PrivateKey
	caCert        *x509.Certificate
	challengeBase string
	failChallenge bool
	// lifetime of the issued leaf; a test can shorten it to exercise the
	// short-lived-certificate renewal threshold.
	lifetime time.Duration
	// token is the challenge token for the FIRST identifier of an order; kept as
	// its own field (rather than folded into authzs) so existing single-domain
	// tests can keep asserting store.Get(dir.token) unchanged. Defaults to
	// "tok-1" in newFakeDir.
	token string
	// authzs holds one entry per identifier of the CURRENT order, rebuilt fresh
	// by the /new-order handler from the client's requested identifiers -- so a
	// multi-name Obtain (SplitNames' dns slice) gets one authorization per DNS
	// name, mirroring a real ACME server's per-identifier authorization model.
	authzs []*fakeAuthzState
	// rateLimit makes new-order answer 429 with a rateLimited problem document.
	rateLimit bool
	// lastLeafDER is the DER of the most recently finalized leaf, set in
	// /finalize/1 and read back when the client fetches /cert/1.
	lastLeafDER []byte

	// failNextByPath, keyed by URL path, makes the NEXT N POST requests to
	// that path answer with a plain 500 (a generic, retriable server error --
	// isRetriable(500) is true in x/crypto/acme) instead of their normal
	// handling; each hit decrements the count, and once it reaches 0 the path
	// resumes normal handling. Models a transiently misbehaving CA (e.g. a
	// backend restart mid-order) that recovers within the client's own
	// retry budget -- distinct from the 429 rate-limit case, which
	// retryBackoffExceptRateLimit (acme.go) deliberately refuses to retry.
	failNextByPath map[string]int
	// badNonceOncePath, when equal to a request's URL path, makes exactly
	// ONE POST to that path answer with an ACME badNonce problem document
	// (400) instead of its normal handling, then clears itself so every
	// following request (including a same-path retry) behaves normally.
	// RFC 8555 makes a badNonce response a routine, expected occurrence
	// (the client is required to discard its cached nonce and retry with a
	// fresh one -- see x/crypto/acme/http.go's isBadNonce branch), so this
	// models that exact "server briefly rejects a stale nonce" case, again
	// deliberately NOT the 429 case above.
	badNonceOncePath string
}

// fakeAuthzState is one per-identifier authorization of the current order: its
// DNS name, its own HTTP-01 token, and whether /chal/{n} has verified it against
// the challenge server.
type fakeAuthzState struct {
	identifier string
	token      string
	ok         bool
}

func newFakeDir(t *testing.T, challengeBase string) *fakeDir {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "fake acme CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour * 365),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caCert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeDir{caKey: caKey, caCert: caCert, challengeBase: challengeBase, lifetime: 90 * 24 * time.Hour, token: "tok-1"}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeDir) base() string { return f.srv.URL }

// payload decodes the JWS payload of an ACME POST. A POST-as-GET carries an
// empty payload, which decodes to nil without error.
func payload(r *http.Request) map[string]any {
	body, _ := io.ReadAll(r.Body)
	var jws struct {
		Payload string `json:"payload"`
	}
	if err := json.Unmarshal(body, &jws); err != nil || jws.Payload == "" {
		return nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(jws.Payload)
	if err != nil {
		return nil
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil
	}
	return out
}

func (f *fakeDir) handle(w http.ResponseWriter, r *http.Request) {
	// Every response carries a fresh nonce (the client consumes one per POST),
	// including the injected-failure responses below -- a stale/missing
	// nonce would itself trigger a (different) retry path and muddy what
	// these knobs are meant to isolate.
	w.Header().Set("Replay-Nonce", fmt.Sprintf("nonce-%d", time.Now().UnixNano()))

	// Failure-injection knobs (see their doc comments on fakeDir) are checked
	// BEFORE the normal per-path switch below, and only ever match a POST --
	// every authenticated ACME request (including POST-as-GET) is a POST, so
	// this can't accidentally intercept the fake directory's own outbound
	// GET in the /chal/1 case. Both knobs are additive no-ops when unset,
	// leaving every other test's behavior byte-identical.
	if r.Method == http.MethodPost {
		if f.failNextByPath != nil && f.failNextByPath[r.URL.Path] > 0 {
			f.failNextByPath[r.URL.Path]--
			http.Error(w, "internal error (injected)", http.StatusInternalServerError)
			return
		}
		if f.badNonceOncePath != "" && f.badNonceOncePath == r.URL.Path {
			f.badNonceOncePath = ""
			w.Header().Set("Content-Type", "application/problem+json")
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"type":   "urn:ietf:params:acme:error:badNonce",
				"detail": "JWS has an invalid anti-replay nonce",
			})
			return
		}
	}

	switch {
	case r.URL.Path == "/directory":
		writeJSON(w, 200, map[string]any{
			"newNonce":   f.base() + "/new-nonce",
			"newAccount": f.base() + "/new-account",
			"newOrder":   f.base() + "/new-order",
			"revokeCert": f.base() + "/revoke",
			"keyChange":  f.base() + "/key-change",
		})
	case r.URL.Path == "/new-nonce":
		w.WriteHeader(200)
	case r.URL.Path == "/new-account":
		w.Header().Set("Location", f.base()+"/acct/1")
		writeJSON(w, 201, map[string]any{"status": "valid"})
	case r.URL.Path == "/new-order":
		if f.rateLimit {
			w.Header().Set("Content-Type", "application/problem+json")
			w.Header().Set("Retry-After", "120")
			writeJSON(w, 429, map[string]any{
				"type":   "urn:ietf:params:acme:error:rateLimited",
				"detail": "too many certificates already issued",
			})
			return
		}
		// One authorization per requested DNS identifier (RFC 8555 -- a real CA
		// need not keep a 1:1 identifier<->authorization mapping, but every ACME
		// server does for a fresh order with no pre-authorized identifiers, which
		// is all a test order ever is). The identifiers come from the client's own
		// JWS payload, so this scales to however many names Obtain requested.
		idents := requestedIdentifiers(payload(r))
		if len(idents) == 0 {
			idents = []string{"example.test"}
		}
		f.authzs = make([]*fakeAuthzState, len(idents))
		authzURLs := make([]string, len(idents))
		for i, ident := range idents {
			tok := f.token
			if i > 0 || tok == "" {
				tok = fmt.Sprintf("tok-%d", i+1)
			}
			f.authzs[i] = &fakeAuthzState{identifier: ident, token: tok}
			authzURLs[i] = fmt.Sprintf("%s/authz/%d", f.base(), i+1)
		}
		w.Header().Set("Location", f.base()+"/order/1")
		writeJSON(w, 201, map[string]any{
			"status":         "pending",
			"authorizations": authzURLs,
			"finalize":       f.base() + "/finalize/1",
		})
	case strings.HasPrefix(r.URL.Path, "/authz/"):
		a, ok := f.authzForPath(r.URL.Path, "/authz/")
		if !ok {
			http.NotFound(w, r)
			return
		}
		idx := f.authzIndex(a)
		status := "pending"
		if a.ok {
			status = "valid"
		}
		if f.failChallenge {
			status = "invalid"
		}
		writeJSON(w, 200, map[string]any{
			"status":     status,
			"identifier": map[string]any{"type": "dns", "value": a.identifier},
			"challenges": []map[string]any{{
				"type":   "http-01",
				"url":    fmt.Sprintf("%s/chal/%d", f.base(), idx),
				"token":  a.token,
				"status": status,
			}},
		})
	case strings.HasPrefix(r.URL.Path, "/chal/"):
		a, ok := f.authzForPath(r.URL.Path, "/chal/")
		if !ok {
			http.NotFound(w, r)
			return
		}
		idx := f.authzIndex(a)
		// Really fetch the token from the challenge server: this is what proves
		// the gateway's own handler serves the right key authorization.
		resp, err := http.Get(f.challengeBase + "/.well-known/acme-challenge/" + a.token)
		if err == nil {
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)
			if resp.StatusCode == 200 && strings.HasPrefix(strings.TrimSpace(string(body)), a.token+".") {
				a.ok = true
			}
		}
		writeJSON(w, 200, map[string]any{"type": "http-01", "url": fmt.Sprintf("%s/chal/%d", f.base(), idx), "status": "processing"})
	case r.URL.Path == "/finalize/1":
		p := payload(r)
		csrB64, _ := p["csr"].(string)
		csrDER, err := base64.RawURLEncoding.DecodeString(csrB64)
		if err != nil {
			http.Error(w, "bad csr", 400)
			return
		}
		csr, err := x509.ParseCertificateRequest(csrDER)
		if err != nil {
			http.Error(w, "bad csr", 400)
			return
		}
		leaf := &x509.Certificate{
			SerialNumber: big.NewInt(time.Now().UnixNano()),
			Subject:      pkix.Name{CommonName: csr.Subject.CommonName},
			DNSNames:     csr.DNSNames,
			NotBefore:    time.Now().Add(-time.Minute),
			NotAfter:     time.Now().Add(f.lifetime),
			KeyUsage:     x509.KeyUsageDigitalSignature,
			ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		}
		der, err := x509.CreateCertificate(rand.Reader, leaf, f.caCert, csr.PublicKey, f.caKey)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		f.lastLeafDER = der
		w.Header().Set("Location", f.base()+"/order/1")
		writeJSON(w, 200, map[string]any{"status": "valid", "certificate": f.base() + "/cert/1"})
	case r.URL.Path == "/cert/1":
		w.Header().Set("Content-Type", "application/pem-certificate-chain")
		_ = pem.Encode(w, &pem.Block{Type: "CERTIFICATE", Bytes: f.lastLeafDER})
		_ = pem.Encode(w, &pem.Block{Type: "CERTIFICATE", Bytes: f.caCert.Raw})
	case r.URL.Path == "/order/1":
		writeJSON(w, 200, map[string]any{"status": "valid", "certificate": f.base() + "/cert/1", "finalize": f.base() + "/finalize/1"})
	default:
		http.NotFound(w, r)
	}
}

func writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}

// requestedIdentifiers pulls the "value" of every "identifiers" entry out of a
// decoded /new-order JWS payload (see AuthorizeOrder in x/crypto/acme, which
// sends {"identifiers":[{"type":"dns","value":...}, ...]}), in the order the
// client sent them.
func requestedIdentifiers(p map[string]any) []string {
	raw, ok := p["identifiers"].([]any)
	if !ok {
		return nil
	}
	var out []string
	for _, v := range raw {
		m, ok := v.(map[string]any)
		if !ok {
			continue
		}
		if val, ok := m["value"].(string); ok && val != "" {
			out = append(out, val)
		}
	}
	return out
}

// authzForPath resolves a /authz/{n} or /chal/{n} URL path (1-based) to its
// fakeAuthzState from the current order's authzs.
func (f *fakeDir) authzForPath(path, prefix string) (*fakeAuthzState, bool) {
	n, err := strconv.Atoi(strings.TrimPrefix(path, prefix))
	if err != nil || n < 1 || n > len(f.authzs) {
		return nil, false
	}
	return f.authzs[n-1], true
}

// authzIndex is the 1-based position of a within the current order's authzs,
// used to build that authorization's /chal/{n} URL.
func (f *fakeDir) authzIndex(a *fakeAuthzState) int {
	for i, other := range f.authzs {
		if other == a {
			return i + 1
		}
	}
	return 0
}
