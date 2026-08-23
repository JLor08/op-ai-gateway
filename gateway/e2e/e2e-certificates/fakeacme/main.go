// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

// Command fakeacme is a minimal, stdlib-only ACME v2 directory for the
// e2e:certificates Playwright suite. It is a standalone, long-running sibling
// of internal/certissue/fakedir_test.go and cmd/gateway/acme_fakedir_test.go
// (both test-only, in-process httptest fixtures): the SAME protocol logic --
// a throwaway self-signed CA, no JWS signature verification (the real
// golang.org/x/crypto/acme client always sends one; this fake only decodes
// the base64url JSON payload it carries, exactly like the two test fixtures
// it is ported from), and a REAL fetch of the HTTP-01 challenge token from
// the gateway's own public /.well-known/acme-challenge/ handler -- ported
// here as a standalone HTTP server so a real gateway binary (a separate
// process, driven end-to-end through the portal UI) can be pointed at it via
// `acme_directory_url`.
//
// Unlike the two test fixtures (which hardcode a single order "1" because a
// single in-process test only ever places one order at a time), this program
// tracks orders in a map keyed by an auto-incrementing id under a mutex, so
// several domains issued IN SEQUENCE across separate reconcile passes each
// get their own order/authorization/challenge/finalize/cert record without
// colliding. Not for production.
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
	"fmt"
	"io"
	"log"
	"math/big"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// acmeOrder is the per-order state a real order/authorization/challenge/
// finalize/cert cycle accumulates. Guarded by server.mu.
type acmeOrder struct {
	domain  string
	token   string
	authzOK bool
	leafDER []byte
}

// server is the fake ACME directory: one throwaway CA for the whole process
// lifetime, plus a growing set of orders (never pruned -- the suite's gateway
// process and this program are both fresh per test run, so unbounded growth
// across a single Playwright run is not a concern).
type server struct {
	base          string
	caKey         *ecdsa.PrivateKey
	caCert        *x509.Certificate
	challengeBase string

	mu     sync.Mutex
	orders map[int]*acmeOrder
	nextID int
}

func newServer(base, challengeBase string) (*server, error) {
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("fakeacme: generate CA key: %w", err)
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
		return nil, fmt.Errorf("fakeacme: create CA certificate: %w", err)
	}
	caCert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("fakeacme: parse CA certificate: %w", err)
	}
	return &server{
		base:          base,
		caKey:         caKey,
		caCert:        caCert,
		challengeBase: challengeBase,
		orders:        map[int]*acmeOrder{},
	}, nil
}

func (s *server) orderURL(id int) string    { return fmt.Sprintf("%s/order/%d", s.base, id) }
func (s *server) authzURL(id int) string    { return fmt.Sprintf("%s/authz/%d", s.base, id) }
func (s *server) chalURL(id int) string     { return fmt.Sprintf("%s/chal/%d", s.base, id) }
func (s *server) finalizeURL(id int) string { return fmt.Sprintf("%s/finalize/%d", s.base, id) }
func (s *server) certURL(id int) string     { return fmt.Sprintf("%s/cert/%d", s.base, id) }

func (s *server) handle(w http.ResponseWriter, r *http.Request) {
	// Every response carries a fresh nonce (the real client consumes one per POST).
	w.Header().Set("Replay-Nonce", fmt.Sprintf("nonce-%d", time.Now().UnixNano()))
	switch {
	case r.URL.Path == "/healthz":
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	case r.URL.Path == "/directory":
		writeJSON(w, 200, map[string]any{
			"newNonce":   s.base + "/new-nonce",
			"newAccount": s.base + "/new-account",
			"newOrder":   s.base + "/new-order",
			"revokeCert": s.base + "/revoke",
			"keyChange":  s.base + "/key-change",
		})
	case r.URL.Path == "/new-nonce":
		w.WriteHeader(200)
	case r.URL.Path == "/new-account":
		w.Header().Set("Location", s.base+"/acct/1")
		writeJSON(w, 201, map[string]any{"status": "valid"})
	case r.URL.Path == "/new-order":
		s.handleNewOrder(w, r)
	case strings.HasPrefix(r.URL.Path, "/authz/"):
		s.handleAuthz(w, r, strings.TrimPrefix(r.URL.Path, "/authz/"))
	case strings.HasPrefix(r.URL.Path, "/chal/"):
		s.handleChal(w, r, strings.TrimPrefix(r.URL.Path, "/chal/"))
	case strings.HasPrefix(r.URL.Path, "/finalize/"):
		s.handleFinalize(w, r, strings.TrimPrefix(r.URL.Path, "/finalize/"))
	case strings.HasPrefix(r.URL.Path, "/cert/"):
		s.handleCert(w, r, strings.TrimPrefix(r.URL.Path, "/cert/"))
	case strings.HasPrefix(r.URL.Path, "/order/"):
		s.handleOrder(w, r, strings.TrimPrefix(r.URL.Path, "/order/"))
	default:
		http.NotFound(w, r)
	}
}

// handleNewOrder creates a fresh order (a new, never-reused numeric id) for the
// dns identifier carried in the JWS payload's "identifiers" array -- this is
// what lets several domains, issued one after another across reconcile
// passes, each get their own order/authz/challenge/cert record instead of
// colliding on a hardcoded "1" the way the single-order test fixtures do.
func (s *server) handleNewOrder(w http.ResponseWriter, r *http.Request) {
	domain := domainFromIdentifiers(payload(r))
	s.mu.Lock()
	s.nextID++
	id := s.nextID
	s.orders[id] = &acmeOrder{domain: domain, token: fmt.Sprintf("tok-%d", id)}
	s.mu.Unlock()
	w.Header().Set("Location", s.orderURL(id))
	writeJSON(w, 201, map[string]any{
		"status":         "pending",
		"authorizations": []string{s.authzURL(id)},
		"finalize":       s.finalizeURL(id),
	})
}

func (s *server) handleAuthz(w http.ResponseWriter, r *http.Request, idStr string) {
	id, ok := parseOrderID(w, idStr)
	if !ok {
		return
	}
	s.mu.Lock()
	order, exists := s.orders[id]
	var domain, token string
	var authzOK bool
	if exists {
		domain, token, authzOK = order.domain, order.token, order.authzOK
	}
	s.mu.Unlock()
	if !exists {
		http.NotFound(w, r)
		return
	}
	status := "pending"
	if authzOK {
		status = "valid"
	}
	writeJSON(w, 200, map[string]any{
		"status":     status,
		"identifier": map[string]any{"type": "dns", "value": domain},
		"challenges": []map[string]any{{
			"type":   "http-01",
			"url":    s.chalURL(id),
			"token":  token,
			"status": status,
		}},
	})
}

// handleChal is the part that REALLY proves end-to-end wiring: it fetches the
// key-authorization token from the gateway's own public
// /.well-known/acme-challenge/{token} handler (challengeBase), the same way a
// real ACME CA validates HTTP-01. A successful, correctly-prefixed response
// flips the order's authzOK, which the NEXT /authz/{id} poll (the real
// client's WaitAuthorization loop) reports as "valid".
func (s *server) handleChal(w http.ResponseWriter, r *http.Request, idStr string) {
	id, ok := parseOrderID(w, idStr)
	if !ok {
		return
	}
	s.mu.Lock()
	order, exists := s.orders[id]
	var token string
	if exists {
		token = order.token
	}
	s.mu.Unlock()
	if !exists {
		http.NotFound(w, r)
		return
	}
	resp, err := http.Get(s.challengeBase + "/.well-known/acme-challenge/" + token)
	if err == nil {
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode == 200 && strings.HasPrefix(strings.TrimSpace(string(body)), token+".") {
			s.mu.Lock()
			order.authzOK = true
			s.mu.Unlock()
		}
	}
	writeJSON(w, 200, map[string]any{"type": "http-01", "url": s.chalURL(id), "status": "processing"})
}

func (s *server) handleFinalize(w http.ResponseWriter, r *http.Request, idStr string) {
	id, ok := parseOrderID(w, idStr)
	if !ok {
		return
	}
	s.mu.Lock()
	order, exists := s.orders[id]
	s.mu.Unlock()
	if !exists {
		http.NotFound(w, r)
		return
	}
	p := payload(r)
	csrB64, _ := p["csr"].(string)
	csrDER, err := base64.RawURLEncoding.DecodeString(csrB64)
	if err != nil {
		http.Error(w, "bad csr", http.StatusBadRequest)
		return
	}
	csr, err := x509.ParseCertificateRequest(csrDER)
	if err != nil {
		http.Error(w, "bad csr", http.StatusBadRequest)
		return
	}
	leafTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: csr.Subject.CommonName},
		DNSNames:     csr.DNSNames,
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(90 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, leafTmpl, s.caCert, csr.PublicKey, s.caKey)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.mu.Lock()
	order.leafDER = der
	s.mu.Unlock()
	w.Header().Set("Location", s.orderURL(id))
	writeJSON(w, 200, map[string]any{"status": "valid", "certificate": s.certURL(id)})
}

func (s *server) handleCert(w http.ResponseWriter, r *http.Request, idStr string) {
	id, ok := parseOrderID(w, idStr)
	if !ok {
		return
	}
	s.mu.Lock()
	order, exists := s.orders[id]
	var leafDER []byte
	if exists {
		leafDER = order.leafDER
	}
	s.mu.Unlock()
	if !exists || leafDER == nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/pem-certificate-chain")
	_ = pem.Encode(w, &pem.Block{Type: "CERTIFICATE", Bytes: leafDER})
	_ = pem.Encode(w, &pem.Block{Type: "CERTIFICATE", Bytes: s.caCert.Raw})
}

func (s *server) handleOrder(w http.ResponseWriter, r *http.Request, idStr string) {
	id, ok := parseOrderID(w, idStr)
	if !ok {
		return
	}
	s.mu.Lock()
	order, exists := s.orders[id]
	var haveLeaf bool
	if exists {
		haveLeaf = order.leafDER != nil
	}
	s.mu.Unlock()
	if !exists {
		http.NotFound(w, r)
		return
	}
	status := "pending"
	if haveLeaf {
		status = "valid"
	}
	body := map[string]any{"status": status, "finalize": s.finalizeURL(id)}
	if haveLeaf {
		body["certificate"] = s.certURL(id)
	}
	writeJSON(w, 200, body)
}

// parseOrderID extracts the numeric id from a path suffix (e.g. "42" from
// "/authz/42"); a malformed suffix answers 404 rather than crashing the
// process on a stray/hand-crafted request.
func parseOrderID(w http.ResponseWriter, idStr string) (int, bool) {
	idStr = strings.TrimSuffix(idStr, "/")
	id, err := strconv.Atoi(idStr)
	if err != nil {
		http.NotFound(w, nil)
		return 0, false
	}
	return id, true
}

// payload decodes the JWS payload of an ACME POST -- see the package doc: this
// fake, like the two test fixtures it is ported from, never verifies the JWS
// signature (a POST-as-GET carries an empty payload, which decodes to nil
// without error).
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

// domainFromIdentifiers pulls the first dns identifier's value out of a
// decoded new-order payload ({"identifiers":[{"type":"dns","value":"..."}]});
// "" on any unexpected shape (never crashes on a hand-built request).
func domainFromIdentifiers(p map[string]any) string {
	ids, _ := p["identifiers"].([]any)
	for _, raw := range ids {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if kind, _ := m["type"].(string); kind != "dns" {
			continue
		}
		if v, ok := m["value"].(string); ok {
			return v
		}
	}
	return ""
}

func writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}

func envOr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func main() {
	addr := envOr("FAKEACME_ADDR", "127.0.0.1:8093")
	challengeBase := envOr("FAKEACME_CHALLENGE_BASE", "http://127.0.0.1:8091")
	base := "http://" + addr

	srv, err := newServer(base, challengeBase)
	if err != nil {
		log.Fatalf("fakeacme: %v", err)
	}
	log.Printf("fakeacme: directory on %s, challenge fetch base %s", base, challengeBase)
	if err := http.ListenAndServe(addr, http.HandlerFunc(srv.handle)); err != nil {
		log.Fatalf("fakeacme: listen %s: %v", addr, err)
	}
}
