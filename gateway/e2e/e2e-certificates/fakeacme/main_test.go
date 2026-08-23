// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// This package has no golang.org/x/crypto/acme dependency (its go.mod
// declares stdlib-only), so a full order run through the real ACME client
// isn't exercisable here -- that end-to-end proof lives in
// cmd/gateway/cert_reconcile_test.go (a real certissue.ACMEClient against the
// SAME protocol logic, ported into this program from
// cmd/gateway/acme_fakedir_test.go) and in the e2e:certificates Playwright
// suite (the real gateway binary against a REAL, network-listening instance
// of this program). What IS exercised here, directly against the handler:
// the directory's five mandatory fields, and that /finalize/{id} really signs
// a hand-built CSR into a certificate carrying the CSR's requested DNSName --
// the one step that would silently break the whole suite if it stopped
// working (a bad CSR parse, a bad leaf template, or a CA/leaf key mismatch).

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	// The handler needs to know its own base URL for the Location headers +
	// directory entries it writes -- httptest.NewServer only allocates that URL
	// once the listener is up, so build the server in two steps: first with a
	// placeholder base, then patch it in from the real listener address.
	srv, err := newServer("http://placeholder", "http://127.0.0.1:1") // challengeBase unused by this test
	if err != nil {
		t.Fatalf("newServer: %v", err)
	}
	ts := httptest.NewServer(http.HandlerFunc(srv.handle))
	t.Cleanup(ts.Close)
	srv.base = ts.URL
	return ts
}

func TestDirectoryHasMandatoryFields(t *testing.T) {
	ts := newTestServer(t)
	resp, err := http.Get(ts.URL + "/directory")
	if err != nil {
		t.Fatalf("GET /directory: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, field := range []string{"newNonce", "newAccount", "newOrder", "revokeCert", "keyChange"} {
		v, ok := body[field].(string)
		if !ok || v == "" {
			t.Errorf("directory missing/empty mandatory field %q: %#v", field, body[field])
		}
	}
}

// postJWS posts a minimal ACME-shaped JWS envelope: just the base64url JSON
// payload the handler's own `payload()` helper decodes -- this fake (like the
// two test fixtures it is ported from) never verifies "protected"/"signature",
// so a bare {"payload": "..."} body round-trips exactly like a real client's
// fully-signed one.
func postJWS(t *testing.T, url string, payload map[string]any) *http.Response {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	envelope, err := json.Marshal(map[string]string{"payload": base64.RawURLEncoding.EncodeToString(raw)})
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	resp, err := http.Post(url, "application/jose+json", strings.NewReader(string(envelope)))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	return resp
}

// orderIDFromLocation extracts the trailing numeric id from a "/order/{id}"
// Location header.
func orderIDFromLocation(t *testing.T, location string) int {
	t.Helper()
	parts := strings.Split(strings.TrimRight(location, "/"), "/")
	id, err := strconv.Atoi(parts[len(parts)-1])
	if err != nil {
		t.Fatalf("parse order id from Location %q: %v", location, err)
	}
	return id
}

func TestFinalizeSignsCSRWithRequestedDNSName(t *testing.T) {
	ts := newTestServer(t)

	// A real new-order call, so the finalize endpoint below targets a genuine,
	// map-tracked order id (not a hardcoded "1" -- proving the per-order state
	// this fake adds over the single-order test fixtures actually works).
	orderResp := postJWS(t, ts.URL+"/new-order", map[string]any{
		"identifiers": []map[string]any{{"type": "dns", "value": "unused-in-this-test.example.test"}},
	})
	defer orderResp.Body.Close()
	if orderResp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(orderResp.Body)
		t.Fatalf("new-order status = %d, body = %s", orderResp.StatusCode, body)
	}
	id := orderIDFromLocation(t, orderResp.Header.Get("Location"))

	// Build a real CSR (crypto/x509, as the brief asks) requesting a distinct
	// DNSName from the order's own identifier -- finalize signs whatever CSR it
	// is handed (exactly like the two test fixtures), so this also proves it
	// does NOT silently substitute the order's original identifier.
	const requestedName = "finalize-target.example.test"
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate leaf key: %v", err)
	}
	csrDER, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject:  pkix.Name{CommonName: requestedName},
		DNSNames: []string{requestedName},
	}, leafKey)
	if err != nil {
		t.Fatalf("create CSR: %v", err)
	}

	finResp := postJWS(t, ts.URL+"/finalize/"+strconv.Itoa(id), map[string]any{
		"csr": base64.RawURLEncoding.EncodeToString(csrDER),
	})
	defer finResp.Body.Close()
	if finResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(finResp.Body)
		t.Fatalf("finalize status = %d, body = %s", finResp.StatusCode, body)
	}
	var finBody struct {
		Status      string `json:"status"`
		Certificate string `json:"certificate"`
	}
	if err := json.NewDecoder(finResp.Body).Decode(&finBody); err != nil {
		t.Fatalf("decode finalize response: %v", err)
	}
	if finBody.Status != "valid" {
		t.Fatalf("finalize status = %q, want valid", finBody.Status)
	}
	if finBody.Certificate == "" {
		t.Fatalf("finalize response carries no certificate URL")
	}

	certResp, err := http.Get(finBody.Certificate)
	if err != nil {
		t.Fatalf("GET certificate: %v", err)
	}
	defer certResp.Body.Close()
	chainPEM, err := io.ReadAll(certResp.Body)
	if err != nil {
		t.Fatalf("read certificate chain: %v", err)
	}
	certs, err := parsePEMChain(chainPEM)
	if err != nil {
		t.Fatalf("parse certificate chain: %v", err)
	}
	if len(certs) != 2 {
		t.Fatalf("chain has %d certificates, want 2 (leaf + CA root)", len(certs))
	}
	leaf := certs[0]
	if len(leaf.DNSNames) != 1 || leaf.DNSNames[0] != requestedName {
		t.Fatalf("leaf DNSNames = %v, want [%s]", leaf.DNSNames, requestedName)
	}
	// The leaf must actually chain to the CA served alongside it (a signature
	// mismatch here would mean the handler signed with the wrong key).
	if err := leaf.CheckSignatureFrom(certs[1]); err != nil {
		t.Fatalf("leaf does not verify against the bundled CA: %v", err)
	}
}

func parsePEMChain(chainPEM []byte) ([]*x509.Certificate, error) {
	var out []*x509.Certificate
	rest := chainPEM
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, err
		}
		out = append(out, cert)
	}
	return out, nil
}
