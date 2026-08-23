// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package certinstall

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"log/slog"
	"math/big"
	"strings"
	"sync"
	"testing"
	"time"
)

// generateTestLeaf builds a self-signed EC (P-256) certificate + PEM key pair,
// mirroring the shape (algorithm, PEM block types) certissue actually
// produces, so tests exercise the same parsing/pairing paths production code
// will see.
func generateTestLeaf(t testing.TB, cn string, notAfter time.Time) (certPEM, keyPEM string, leafDER []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: cn},
		DNSNames:     []string{cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	certPEM = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	keyPEM = string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))
	return certPEM, keyPEM, der
}

// generateTestCA builds a second, unrelated self-signed certificate to stand
// in for a trust-bundle root (ca.pem content) in tests that don't care about
// its exact meaning, only that it is a valid, distinct PEM certificate block.
func generateTestCA(t testing.TB, cn string) string {
	t.Helper()
	pemStr, _, _ := generateTestLeaf(t, cn, time.Now().Add(24*time.Hour))
	return pemStr
}

// fullchainWithIssuer concatenates a leaf and an "issuer" cert PEM, mirroring
// certissue.Result.FullchainPEM's shape (leaf first, then the signer) so
// splitChain has something real to split.
func fullchainWithIssuer(leafPEM, issuerPEM string) string {
	return leafPEM + issuerPEM
}

// recordingHandler captures every slog.Record it receives (level, message,
// and every attribute) so a test can assert on log content without depending
// on slog's text/JSON formatting.
type recordingHandler struct {
	mu      sync.Mutex
	records []capturedRecord
}

type capturedRecord struct {
	level slog.Level
	msg   string
	attrs map[string]any
}

func (h *recordingHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *recordingHandler) Handle(_ context.Context, r slog.Record) error {
	attrs := make(map[string]any, r.NumAttrs())
	r.Attrs(func(a slog.Attr) bool {
		attrs[a.Key] = a.Value.Any()
		return true
	})
	h.mu.Lock()
	h.records = append(h.records, capturedRecord{level: r.Level, msg: r.Message, attrs: attrs})
	h.mu.Unlock()
	return nil
}

func (h *recordingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *recordingHandler) WithGroup(string) slog.Handler      { return h }

func (h *recordingHandler) all() []capturedRecord {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]capturedRecord, len(h.records))
	copy(out, h.records)
	return out
}

// containsText reports whether needle appears anywhere across every captured
// record's message or any attribute VALUE (stringified) -- used to prove a
// secret-bearing string (e.g. a reload command line) never reaches a log.
func (h *recordingHandler) containsText(needle string) bool {
	for _, rec := range h.all() {
		if strings.Contains(rec.msg, needle) {
			return true
		}
		for _, v := range rec.attrs {
			if strings.Contains(fmt.Sprint(v), needle) {
				return true
			}
		}
	}
	return false
}

// hasRecord reports whether any captured record matches level+msg and, if
// wantAttr is non-empty, carries an attribute of that key at all.
func (h *recordingHandler) hasRecord(level slog.Level, msg string, wantAttrKey string) bool {
	for _, rec := range h.all() {
		if rec.level != level || rec.msg != msg {
			continue
		}
		if wantAttrKey == "" {
			return true
		}
		if _, ok := rec.attrs[wantAttrKey]; ok {
			return true
		}
	}
	return false
}

// attrInt normalizes a captured slog attribute value to int64: slog's
// convenience API (e.g. slog.Warn(msg, "k", intVar)) stores a native Go int
// internally as KindInt64, so Value.Any() returns int64, not int -- a plain
// `.(int)` type assertion on a captured attribute silently fails.
func attrInt(v any) (int64, bool) {
	switch n := v.(type) {
	case int:
		return int64(n), true
	case int32:
		return int64(n), true
	case int64:
		return n, true
	default:
		return 0, false
	}
}

// withCapturedLogs installs h as the default slog handler for the duration of
// a test and restores the previous default on cleanup.
func withCapturedLogs(t testing.TB) *recordingHandler {
	t.Helper()
	h := &recordingHandler{}
	prev := slog.Default()
	slog.SetDefault(slog.New(h))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return h
}
