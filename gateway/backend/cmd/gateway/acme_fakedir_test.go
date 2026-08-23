// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package main

// fakeDir is copied test-only code from internal/certissue/fakedir_test.go: a
// minimal ACME v2 directory backed by a throwaway CA. It is duplicated here
// (not imported -- _test.go files aren't part of an importable package) so
// cert_reconcile_test.go can drive a REAL certissue.ACMEClient (via
// portal.Service.ReconcileCertificates) through a REAL new-order/authz/
// challenge/finalize flow, with the challenge fetch landing on the real
// gateway's public /.well-known/acme-challenge/ handler -- the only way to
// prove portal.ServiceDeps.ACMEChallenges and gateway.ServerDeps.ACMEChallenges
// are the SAME store instance end-to-end, rather than re-testing Task 6's
// ACMEChallenges plumbing in isolation.

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
	"strings"
	"testing"
	"time"
)

// fakeDir is a minimal ACME v2 directory backed by a throwaway CA. challengeBase
// is the base URL it fetches the HTTP-01 token from (the test's own challenge
// server -- here, the REAL gateway's public listener). failChallenge makes the
// challenge validation report "invalid".
type fakeDir struct {
	srv           *httptest.Server
	caKey         *ecdsa.PrivateKey
	caCert        *x509.Certificate
	challengeBase string
	failChallenge bool
	// neverValidate makes /authz/1 report "pending" FOREVER, regardless of
	// authzOK -- unlike failChallenge (which reports "invalid" and lets the
	// client fail fast), this models a stalled/misbehaving ACME server whose
	// authorization simply never resolves, so the client's WaitAuthorization
	// poll loop keeps retrying until its context is cancelled. Used to prove
	// the reconcile-loop pass deadline (cert_reconcile.go's certPassTimeout)
	// actually bounds a stalled order instead of hanging forever.
	neverValidate bool
	// lifetime of the issued leaf; a test can shorten it to exercise the
	// short-lived-certificate renewal threshold.
	lifetime time.Duration
	authzOK  bool
	token    string
	// rateLimit makes new-order answer 429 with a rateLimited problem document.
	rateLimit bool
	// lastLeafDER is the DER of the most recently finalized leaf, set in
	// /finalize/1 and read back when the client fetches /cert/1.
	lastLeafDER []byte
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
	// Every response carries a fresh nonce (the client consumes one per POST).
	w.Header().Set("Replay-Nonce", fmt.Sprintf("nonce-%d", time.Now().UnixNano()))
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
		f.authzOK = false
		w.Header().Set("Location", f.base()+"/order/1")
		writeJSON(w, 201, map[string]any{
			"status":         "pending",
			"authorizations": []string{f.base() + "/authz/1"},
			"finalize":       f.base() + "/finalize/1",
		})
	case r.URL.Path == "/authz/1":
		status := "pending"
		if f.authzOK && !f.neverValidate {
			status = "valid"
		}
		if f.failChallenge {
			status = "invalid"
		}
		writeJSON(w, 200, map[string]any{
			"status":     status,
			"identifier": map[string]any{"type": "dns", "value": "example.test"},
			"challenges": []map[string]any{{
				"type":   "http-01",
				"url":    f.base() + "/chal/1",
				"token":  f.token,
				"status": status,
			}},
		})
	case r.URL.Path == "/chal/1":
		// Really fetch the token from the challenge server: this is what proves
		// the gateway's own handler serves the right key authorization.
		resp, err := http.Get(f.challengeBase + "/.well-known/acme-challenge/" + f.token)
		if err == nil {
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)
			if resp.StatusCode == 200 && strings.HasPrefix(strings.TrimSpace(string(body)), f.token+".") {
				f.authzOK = true
			}
		}
		writeJSON(w, 200, map[string]any{"type": "http-01", "url": f.base() + "/chal/1", "status": "processing"})
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
