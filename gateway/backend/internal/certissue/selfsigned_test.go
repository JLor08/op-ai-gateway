// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package certissue

import (
	"crypto/x509"
	"strings"
	"testing"
	"time"
)

func TestNewCAProducesAUsableRoot(t *testing.T) {
	ca, certPEM, keyPEM, err := NewCA("OP AI Gateway Internal CA (int.example.test)", 10*365*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if !ca.Cert.IsCA || ca.Cert.KeyUsage&x509.KeyUsageCertSign == 0 {
		t.Fatalf("root must be a signing CA: IsCA=%v KeyUsage=%v", ca.Cert.IsCA, ca.Cert.KeyUsage)
	}
	if !ca.Cert.BasicConstraintsValid {
		t.Fatal("root must set BasicConstraintsValid")
	}
	if ca.Cert.Subject.CommonName != "OP AI Gateway Internal CA (int.example.test)" {
		t.Fatalf("subject = %q", ca.Cert.Subject.CommonName)
	}
	// It must survive a PEM round trip (that is how it is stored).
	back, err := LoadCA(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	if back.Fingerprint() != ca.Fingerprint() {
		t.Fatal("round-tripped CA has a different fingerprint")
	}
	if FingerprintPEM(certPEM) != ca.Fingerprint() {
		t.Fatal("FingerprintPEM must match the CA's own fingerprint")
	}
	if FingerprintPEM("not a pem") != "" {
		t.Fatal("FingerprintPEM must return empty on garbage")
	}
}

func TestIssueLeafVerifiesAgainstTheRoot(t *testing.T) {
	now := time.Now().UTC()
	ca, _, _, err := NewCA("test ca", 10*365*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	res, err := ca.Issue("ai-server1.int.example.test", 365*24*time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Leaf.DNSNames) != 1 || res.Leaf.DNSNames[0] != "ai-server1.int.example.test" {
		t.Fatalf("leaf DNSNames = %v", res.Leaf.DNSNames)
	}
	// The chain must carry leaf + root so a client that only has the root sees a
	// complete chain.
	if strings.Count(res.FullchainPEM, "BEGIN CERTIFICATE") != 2 {
		t.Fatalf("fullchain must be leaf + root, got %q", res.FullchainPEM)
	}
	if !strings.Contains(res.KeyPEM, "BEGIN EC PRIVATE KEY") {
		t.Fatalf("missing leaf key: %q", res.KeyPEM)
	}
	if len(res.Fingerprint) != 64 {
		t.Fatalf("fingerprint = %q, want 64 hex chars", res.Fingerprint)
	}
	// It really verifies against the root, and against nothing else.
	pool := x509.NewCertPool()
	pool.AddCert(ca.Cert)
	if _, err := res.Leaf.Verify(x509.VerifyOptions{
		Roots:       pool,
		DNSName:     "ai-server1.int.example.test",
		CurrentTime: now.Add(time.Hour),
	}); err != nil {
		t.Fatalf("leaf must verify against its own root: %v", err)
	}
	other, _, _, err := NewCA("other ca", 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	otherPool := x509.NewCertPool()
	otherPool.AddCert(other.Cert)
	if _, err := res.Leaf.Verify(x509.VerifyOptions{Roots: otherPool, DNSName: "ai-server1.int.example.test", CurrentTime: now.Add(time.Hour)}); err == nil {
		t.Fatal("leaf must NOT verify against a foreign root")
	}
}

func TestIssueClampsLeafValidityToTheCALifetime(t *testing.T) {
	now := time.Now().UTC()
	// A CA with only 10 days left must never sign a 365-day leaf.
	ca, _, _, err := NewCA("short ca", 10*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	res, err := ca.Issue("x.int.example.test", 365*24*time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}
	if res.Leaf.NotAfter.After(ca.Cert.NotAfter) {
		t.Fatalf("leaf notAfter %v outlives the CA %v", res.Leaf.NotAfter, ca.Cert.NotAfter)
	}
	// A shorter requested validity is honored as-is.
	res2, err := ca.Issue("y.int.example.test", 2*24*time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}
	if d := res2.Leaf.NotAfter.Sub(res2.Leaf.NotBefore); d > 3*24*time.Hour {
		t.Fatalf("leaf lifetime = %v, want about 2 days", d)
	}
}

func TestIssueRejectsAnExpiredCA(t *testing.T) {
	now := time.Now().UTC()
	ca, _, _, err := NewCA("expired ca", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ca.Issue("z.int.example.test", 24*time.Hour, now.Add(2*time.Hour)); err == nil {
		t.Fatal("issuing from an expired CA must fail rather than produce a dead leaf")
	}
}

func TestIssueForPutsEveryNameInTheLeaf(t *testing.T) {
	ca, _, _, err := NewCA("Test CA", 24*time.Hour)
	if err != nil {
		t.Fatalf("NewCA: %v", err)
	}
	res, err := ca.IssueFor([]string{"edge.lan", "10.0.0.5"}, time.Hour, time.Now())
	if err != nil {
		t.Fatalf("IssueFor: %v", err)
	}
	if len(res.Leaf.DNSNames) != 1 || res.Leaf.DNSNames[0] != "edge.lan" {
		t.Fatalf("DNSNames = %v, want [edge.lan]", res.Leaf.DNSNames)
	}
	if len(res.Leaf.IPAddresses) != 1 || res.Leaf.IPAddresses[0].String() != "10.0.0.5" {
		t.Fatalf("IPAddresses = %v, want [10.0.0.5]", res.Leaf.IPAddresses)
	}
	if res.Leaf.Subject.CommonName != "edge.lan" {
		t.Fatalf("CommonName = %q, want edge.lan (the first name)", res.Leaf.Subject.CommonName)
	}
}

func TestIssueStillWorksForASingleName(t *testing.T) {
	ca, _, _, err := NewCA("Test CA", 24*time.Hour)
	if err != nil {
		t.Fatalf("NewCA: %v", err)
	}
	res, err := ca.Issue("ai-server1.int.example.test", time.Hour, time.Now())
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if len(res.Leaf.DNSNames) != 1 || res.Leaf.DNSNames[0] != "ai-server1.int.example.test" {
		t.Fatalf("DNSNames = %v", res.Leaf.DNSNames)
	}
	if len(res.Leaf.IPAddresses) != 0 {
		t.Fatalf("IPAddresses = %v, want none", res.Leaf.IPAddresses)
	}
}
