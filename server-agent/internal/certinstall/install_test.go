// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package certinstall

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestInstallWritesFiveFilesWithExactModesAndContent(t *testing.T) {
	dir := t.TempDir()
	leafPEM, keyPEM, _ := generateTestLeaf(t, "server-a.example.test", time.Now().Add(90*24*time.Hour))
	issuerPEM := generateTestCA(t, "issuer-a")
	caPEM := generateTestCA(t, "root-a")

	in := &Installer{dir: dir, mode: "files"}
	body := certResponse{
		Fingerprint:  "irrelevant-for-install",
		FullchainPEM: fullchainWithIssuer(leafPEM, issuerPEM),
		KeyPEM:       keyPEM,
		CABundlePEM:  caPEM,
	}
	if err := in.install(body); err != nil {
		t.Fatalf("install: %v", err)
	}

	cases := []struct {
		name string
		mode os.FileMode
		want string
	}{
		{fullchainFile, 0o644, leafPEM + issuerPEM},
		{certFile, 0o644, leafPEM},
		{chainFile, 0o644, issuerPEM},
		{caFile, 0o644, caPEM},
		{privkeyFile, 0o600, keyPEM},
	}
	for _, c := range cases {
		info, err := os.Stat(filepath.Join(dir, c.name))
		if err != nil {
			t.Fatalf("stat %s: %v", c.name, err)
		}
		if info.Mode().Perm() != c.mode {
			t.Errorf("%s: mode = %o, want %o", c.name, info.Mode().Perm(), c.mode)
		}
		got, err := os.ReadFile(filepath.Join(dir, c.name))
		if err != nil {
			t.Fatalf("read %s: %v", c.name, err)
		}
		if string(got) != c.want {
			t.Errorf("%s content mismatch", c.name)
		}
	}
	if leftovers := listTempLeftovers(t, dir); len(leftovers) != 0 {
		t.Errorf("leftover temp files: %v", leftovers)
	}
}

func TestInstallOmitsChainForSingleCertificateChain(t *testing.T) {
	dir := t.TempDir()
	leafPEM, keyPEM, _ := generateTestLeaf(t, "single.example.test", time.Now().Add(90*24*time.Hour))

	in := &Installer{dir: dir, mode: "files"}
	body := certResponse{FullchainPEM: leafPEM, KeyPEM: keyPEM}
	if err := in.install(body); err != nil {
		t.Fatalf("install: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, chainFile)); !os.IsNotExist(err) {
		t.Errorf("chain.pem should not exist for a single-certificate chain, stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, caFile)); !os.IsNotExist(err) {
		t.Errorf("ca.pem should not exist when no bundle was supplied, stat err = %v", err)
	}
}

func TestInstallRemovesStaleChainWhenNewChainIsSingleCert(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, chainFile), []byte("stale-issuer"), 0o644); err != nil {
		t.Fatalf("seed stale chain.pem: %v", err)
	}
	leafPEM, keyPEM, _ := generateTestLeaf(t, "single2.example.test", time.Now().Add(90*24*time.Hour))

	in := &Installer{dir: dir, mode: "files"}
	if err := in.install(certResponse{FullchainPEM: leafPEM, KeyPEM: keyPEM}); err != nil {
		t.Fatalf("install: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, chainFile)); !os.IsNotExist(err) {
		t.Errorf("stale chain.pem should have been removed, stat err = %v", err)
	}
}

func TestInstallDoesNotDeleteExistingCAWhenNewBundleIsEmpty(t *testing.T) {
	dir := t.TempDir()
	oldCA := generateTestCA(t, "old-root")
	if err := os.WriteFile(filepath.Join(dir, caFile), []byte(oldCA), 0o644); err != nil {
		t.Fatalf("seed ca.pem: %v", err)
	}
	leafPEM, keyPEM, _ := generateTestLeaf(t, "changed-leaf.example.test", time.Now().Add(90*24*time.Hour))

	in := &Installer{dir: dir, mode: "files"}
	// A changed leaf but an EMPTY bundle in this response: install() must not
	// touch ca.pem at all.
	if err := in.install(certResponse{FullchainPEM: leafPEM, KeyPEM: keyPEM, CABundlePEM: ""}); err != nil {
		t.Fatalf("install: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, caFile))
	if err != nil {
		t.Fatalf("read ca.pem: %v", err)
	}
	if string(got) != oldCA {
		t.Errorf("ca.pem changed despite an empty new bundle: got %q, want unchanged %q", got, oldCA)
	}
}

func TestInstallRejectsUnparseableFullchainWithoutSideEffects(t *testing.T) {
	dir := t.TempDir()
	in := &Installer{dir: dir, mode: "files"}
	err := in.install(certResponse{FullchainPEM: "not a pem at all", KeyPEM: "also not a pem"})
	if err == nil {
		t.Fatalf("expected an error for an unparseable fullchain")
	}
	for _, name := range []string{fullchainFile, certFile, chainFile, caFile, privkeyFile} {
		if _, statErr := os.Stat(filepath.Join(dir, name)); statErr == nil {
			t.Errorf("%s should not exist after a rejected install", name)
		}
	}
	if leftovers := listTempLeftovers(t, dir); len(leftovers) != 0 {
		t.Errorf("leftover temp files after rejected install: %v", leftovers)
	}
}

func TestSplitChainSingleCertificate(t *testing.T) {
	leafPEM, _, _ := generateTestLeaf(t, "one.example.test", time.Now().Add(time.Hour))
	leaf, rest := splitChain(leafPEM)
	if leaf == "" {
		t.Fatalf("expected a non-empty leaf PEM")
	}
	if rest != "" {
		t.Errorf("rest = %q, want empty for a single-certificate chain", rest)
	}
}

func TestSplitChainLeafPlusIssuer(t *testing.T) {
	leafPEM, _, _ := generateTestLeaf(t, "leaf.example.test", time.Now().Add(time.Hour))
	issuerPEM := generateTestCA(t, "issuer")
	leaf, rest := splitChain(fullchainWithIssuer(leafPEM, issuerPEM))
	if leaf != leafPEM {
		t.Errorf("leaf mismatch")
	}
	if rest != issuerPEM {
		t.Errorf("rest mismatch: got %q want %q", rest, issuerPEM)
	}
}

func TestSplitChainUnparseableReturnsEmpty(t *testing.T) {
	leaf, rest := splitChain("garbage, not PEM at all")
	if leaf != "" || rest != "" {
		t.Errorf("expected empty/empty for garbage input, got %q / %q", leaf, rest)
	}
}

func TestFingerprintDERMatchesTheDocumentedFormula(t *testing.T) {
	_, _, der := generateTestLeaf(t, "fp.example.test", time.Now().Add(time.Hour))
	// This reproduces certissue.FingerprintPEM's exact formula
	// (gateway/backend/internal/certissue/selfsigned.go): sha256 over the
	// certificate's DER, hex-encoded, lowercase. If fingerprintDER ever
	// diverges from this, every agent restart would see its own already-
	// installed leaf as "different" from what the gateway reports.
	sum := sha256.Sum256(der)
	want := hex.EncodeToString(sum[:])
	if got := fingerprintDER(der); got != want {
		t.Errorf("fingerprintDER = %q, want %q", got, want)
	}
	if got := fingerprintDER(der); got != want || got == "" || len(got) != 64 {
		t.Errorf("fingerprint must be a 64-char lowercase hex sha256, got %q", got)
	}
}

func TestReadCAFingerprintsMultipleRoots(t *testing.T) {
	dir := t.TempDir()
	rootA := generateTestCA(t, "root-a")
	rootB := generateTestCA(t, "root-b")
	if err := os.WriteFile(filepath.Join(dir, caFile), []byte(rootA+rootB), 0o644); err != nil {
		t.Fatalf("write ca.pem: %v", err)
	}
	got := readCAFingerprints(filepath.Join(dir, caFile))
	if len(got) != 2 {
		t.Fatalf("expected 2 fingerprints, got %d: %v", len(got), got)
	}
	if got[0] == got[1] {
		t.Errorf("expected two DISTINCT fingerprints for two distinct roots")
	}
}

func TestReadCAFingerprintsMissingFileReturnsNil(t *testing.T) {
	dir := t.TempDir()
	if got := readCAFingerprints(filepath.Join(dir, caFile)); got != nil {
		t.Errorf("expected nil for a missing ca.pem, got %v", got)
	}
}

func TestReadDiskStateMemoValidRequiresLeafKeyAndEtag(t *testing.T) {
	leafPEM, keyPEM, _ := generateTestLeaf(t, "memo.example.test", time.Now().Add(time.Hour))
	otherLeafPEM, otherKeyPEM, _ := generateTestLeaf(t, "other.example.test", time.Now().Add(time.Hour))
	_ = otherLeafPEM

	newDir := func(t *testing.T) string { t.Helper(); return t.TempDir() }

	t.Run("nothing on disk", func(t *testing.T) {
		dir := newDir(t)
		st := readDiskState(dir)
		if st.memoValid() {
			t.Errorf("expected memoValid() = false with an empty directory")
		}
	})

	t.Run("leaf without key", func(t *testing.T) {
		dir := newDir(t)
		os.WriteFile(filepath.Join(dir, fullchainFile), []byte(leafPEM), 0o644)
		os.WriteFile(filepath.Join(dir, etagFile), []byte(`"e1"`), 0o644)
		st := readDiskState(dir)
		if st.memoValid() {
			t.Errorf("expected memoValid() = false without a private key")
		}
	})

	t.Run("leaf with MISMATCHED key", func(t *testing.T) {
		dir := newDir(t)
		os.WriteFile(filepath.Join(dir, fullchainFile), []byte(leafPEM), 0o644)
		os.WriteFile(filepath.Join(dir, privkeyFile), []byte(otherKeyPEM), 0o600)
		os.WriteFile(filepath.Join(dir, etagFile), []byte(`"e1"`), 0o644)
		st := readDiskState(dir)
		if st.keyPaired {
			t.Errorf("expected keyPaired = false for a mismatched key")
		}
		if st.memoValid() {
			t.Errorf("expected memoValid() = false for a mismatched key")
		}
	})

	t.Run("leaf and MATCHING key but no etag", func(t *testing.T) {
		dir := newDir(t)
		os.WriteFile(filepath.Join(dir, fullchainFile), []byte(leafPEM), 0o644)
		os.WriteFile(filepath.Join(dir, privkeyFile), []byte(keyPEM), 0o600)
		st := readDiskState(dir)
		if !st.keyPaired {
			t.Errorf("expected keyPaired = true for a matching key")
		}
		if st.memoValid() {
			t.Errorf("expected memoValid() = false without an etag sidecar")
		}
	})

	t.Run("fully valid pair plus etag", func(t *testing.T) {
		dir := newDir(t)
		os.WriteFile(filepath.Join(dir, fullchainFile), []byte(leafPEM), 0o644)
		os.WriteFile(filepath.Join(dir, privkeyFile), []byte(keyPEM), 0o600)
		os.WriteFile(filepath.Join(dir, etagFile), []byte(`"e1"`), 0o644)
		st := readDiskState(dir)
		if !st.memoValid() {
			t.Errorf("expected memoValid() = true for a fully consistent, etag-backed pair")
		}
		if st.etag != `"e1"` {
			t.Errorf("etag = %q, want %q", st.etag, `"e1"`)
		}
	})
}

func TestReportOffModeIsAlwaysBare(t *testing.T) {
	dir := t.TempDir()
	// Even with real files sitting in dir (as if a previous non-off run left
	// them there), ModeOff must report nothing and must not read them.
	leafPEM, keyPEM, _ := generateTestLeaf(t, "leftover.example.test", time.Now().Add(time.Hour))
	os.WriteFile(filepath.Join(dir, fullchainFile), []byte(leafPEM), 0o644)
	os.WriteFile(filepath.Join(dir, privkeyFile), []byte(keyPEM), 0o600)

	in := &Installer{dir: dir, mode: ModeOff}
	r := in.Report()
	if r.Mode != ModeOff || r.Fingerprint != "" || !r.NotAfter.IsZero() || len(r.CAFingerprints) != 0 {
		t.Errorf("Report() with ModeOff = %+v, want only Mode=%q set", r, ModeOff)
	}
}

func TestReportIsDerivedFromDiskNotCached(t *testing.T) {
	dir := t.TempDir()
	in := &Installer{dir: dir, mode: "files"}

	empty := in.Report()
	if empty.Fingerprint != "" || !empty.NotAfter.IsZero() {
		t.Fatalf("expected an empty report before anything is installed, got %+v", empty)
	}

	leafPEM, keyPEM, der := generateTestLeaf(t, "derived.example.test", time.Now().Add(48*time.Hour))
	if err := in.install(certResponse{FullchainPEM: leafPEM, KeyPEM: keyPEM}); err != nil {
		t.Fatalf("install: %v", err)
	}
	after := in.Report()
	want := fingerprintDER(der)
	if after.Fingerprint != want {
		t.Errorf("Report().Fingerprint = %q, want %q", after.Fingerprint, want)
	}
	if after.NotAfter.IsZero() {
		t.Errorf("Report().NotAfter should be populated after install")
	}
	if after.Mode != "files" {
		t.Errorf("Report().Mode = %q, want %q", after.Mode, "files")
	}

	// Simulate an OUT-OF-BAND change to disk (not through this Installer at
	// all) and confirm Report() reflects it -- proof it re-reads disk every
	// call rather than caching what install() last wrote.
	newLeafPEM, _, newDER := generateTestLeaf(t, "swapped.example.test", time.Now().Add(72*time.Hour))
	if err := os.WriteFile(filepath.Join(dir, fullchainFile), []byte(newLeafPEM), 0o644); err != nil {
		t.Fatalf("simulate external change: %v", err)
	}
	swapped := in.Report()
	if swapped.Fingerprint != fingerprintDER(newDER) {
		t.Errorf("Report() did not observe the out-of-band disk change: got %q, want %q", swapped.Fingerprint, fingerprintDER(newDER))
	}
}
