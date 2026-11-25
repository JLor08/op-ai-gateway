// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package hostpower

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/x509"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

var fixedNow = time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)

func clockAt(t time.Time) func() time.Time { return func() time.Time { return t } }

func certPaths(dir string) (certPath, keyPath string) {
	return filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")
}

func listDir(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range ents {
		names = append(names, e.Name())
	}
	return names
}

func TestDefaultTLSPaths(t *testing.T) {
	if DefaultTLSCertPath != "/var/lib/op-hostpower-tls/tls.crt" || DefaultTLSKeyPath != "/var/lib/op-hostpower-tls/tls.key" {
		t.Fatalf("defaults = %q %q", DefaultTLSCertPath, DefaultTLSKeyPath)
	}
}

func TestLoadOrCreateCertGenerates(t *testing.T) {
	sub := filepath.Join(t.TempDir(), "sub")
	certPath, keyPath := certPaths(sub)
	cert, created, err := LoadOrCreateCert(certPath, keyPath, clockAt(fixedNow), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Fatal("created = false, want true")
	}
	if runtime.GOOS != "windows" {
		for _, p := range []string{certPath, keyPath} {
			fi, err := os.Stat(p)
			if err != nil {
				t.Fatal(err)
			}
			if fi.Mode().Perm() != 0o600 {
				t.Errorf("%s mode = %v, want 0600", p, fi.Mode().Perm())
			}
		}
		di, err := os.Stat(sub)
		if err != nil {
			t.Fatal(err)
		}
		if di.Mode().Perm() != 0o700 {
			t.Errorf("dir mode = %v, want 0700", di.Mode().Perm())
		}
	}
	if got := listDir(t, sub); !slices.Equal(got, []string{"tls.crt", "tls.key"}) {
		t.Errorf("dir listing = %v, want only the pair", got)
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	pub, ok := leaf.PublicKey.(*ecdsa.PublicKey)
	if !ok || pub.Curve != elliptic.P256() {
		t.Fatalf("public key = %T, want ECDSA P-256", leaf.PublicKey)
	}
	if leaf.Subject.CommonName != "op-hostpower" {
		t.Errorf("CN = %q", leaf.Subject.CommonName)
	}
	if !leaf.NotAfter.After(fixedNow.AddDate(99, 0, 0)) {
		t.Errorf("NotAfter = %v, want > now+99y", leaf.NotAfter)
	}
	if !leaf.NotBefore.Before(fixedNow) {
		t.Errorf("NotBefore = %v, want before now", leaf.NotBefore)
	}
	if leaf.IsCA || !leaf.BasicConstraintsValid {
		t.Errorf("IsCA = %v BasicConstraintsValid = %v", leaf.IsCA, leaf.BasicConstraintsValid)
	}
	if !slices.Equal(leaf.ExtKeyUsage, []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}) {
		t.Errorf("ExtKeyUsage = %v", leaf.ExtKeyUsage)
	}
	if leaf.KeyUsage != x509.KeyUsageDigitalSignature {
		t.Errorf("KeyUsage = %v", leaf.KeyUsage)
	}
	if len(leaf.DNSNames) != 0 || len(leaf.IPAddresses) != 0 {
		t.Errorf("SANs = %v %v, want none", leaf.DNSNames, leaf.IPAddresses)
	}
	if leaf.SerialNumber.Sign() <= 0 {
		t.Errorf("serial = %v", leaf.SerialNumber)
	}
	key, _ := os.ReadFile(keyPath)
	if !bytes.HasPrefix(key, []byte("-----BEGIN PRIVATE KEY-----")) {
		t.Errorf("key PEM header = %q", strings.SplitN(string(key), "\n", 2)[0])
	}
	crt, _ := os.ReadFile(certPath)
	if !bytes.HasPrefix(crt, []byte("-----BEGIN CERTIFICATE-----")) {
		t.Errorf("cert PEM header = %q", strings.SplitN(string(crt), "\n", 2)[0])
	}
}

func TestLoadOrCreateCertReloads(t *testing.T) {
	certPath, keyPath := certPaths(t.TempDir())
	first, _, err := LoadOrCreateCert(certPath, keyPath, clockAt(fixedNow), nil)
	if err != nil {
		t.Fatal(err)
	}
	second, created, err := LoadOrCreateCert(certPath, keyPath, clockAt(fixedNow.Add(time.Hour)), nil)
	if err != nil {
		t.Fatal(err)
	}
	if created {
		t.Fatal("created = true on reload")
	}
	if !bytes.Equal(first.Certificate[0], second.Certificate[0]) {
		t.Fatal("reloaded certificate differs")
	}
}

func TestLoadOrCreateCertHalfPair(t *testing.T) {
	src := t.TempDir()
	srcCert, srcKey := certPaths(src)
	if _, _, err := LoadOrCreateCert(srcCert, srcKey, clockAt(fixedNow), nil); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, present, missing string
	}{
		{"only key", "tls.key", "tls.crt"},
		{"only cert", "tls.crt", "tls.key"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			data, err := os.ReadFile(filepath.Join(src, tc.present))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, tc.present), data, 0o600); err != nil {
				t.Fatal(err)
			}
			before := listDir(t, dir)
			certPath, keyPath := certPaths(dir)
			_, _, err = LoadOrCreateCert(certPath, keyPath, clockAt(fixedNow), nil)
			if err == nil {
				t.Fatal("no error for half a pair")
			}
			if !strings.Contains(err.Error(), filepath.Join(dir, tc.missing)) {
				t.Errorf("error %q does not name %s", err, tc.missing)
			}
			if after := listDir(t, dir); !slices.Equal(before, after) {
				t.Errorf("dir listing changed: %v -> %v", before, after)
			}
		})
	}
}

func TestLoadOrCreateCertMismatchedPair(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	aCert, aKey := certPaths(a)
	bCert, bKey := certPaths(b)
	for _, p := range [][2]string{{aCert, aKey}, {bCert, bKey}} {
		if _, _, err := LoadOrCreateCert(p[0], p[1], clockAt(fixedNow), nil); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := LoadOrCreateCert(aCert, bKey, clockAt(fixedNow), nil); err == nil {
		t.Fatal("crossed pair loaded")
	}
}

func TestLoadOrCreateCertLooseKeyWarns(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX mode bits")
	}
	certPath, keyPath := certPaths(t.TempDir())
	if _, _, err := LoadOrCreateCert(certPath, keyPath, clockAt(fixedNow), nil); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(keyPath, 0o640); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, nil))
	_, created, err := LoadOrCreateCert(certPath, keyPath, clockAt(fixedNow), log)
	if err != nil || created {
		t.Fatalf("created = %v err = %v", created, err)
	}
	out := logs.String()
	if !strings.Contains(out, "level=WARN") || !strings.Contains(out, "readable by group or others") || !strings.Contains(out, keyPath) {
		t.Fatalf("log = %q", out)
	}
}

func TestLoadOrCreateCertTightKeyNoWarn(t *testing.T) {
	certPath, keyPath := certPaths(t.TempDir())
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, nil))
	if _, _, err := LoadOrCreateCert(certPath, keyPath, clockAt(fixedNow), log); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadOrCreateCert(certPath, keyPath, clockAt(fixedNow), log); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(logs.String(), "level=WARN") {
		t.Fatalf("unexpected warning: %q", logs.String())
	}
}

func TestLoadOrCreateCertNotRegular(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := certPaths(dir)
	if err := os.Mkdir(certPath, 0o700); err != nil {
		t.Fatal(err)
	}
	_, _, err := LoadOrCreateCert(certPath, keyPath, clockAt(fixedNow), nil)
	if err == nil || !strings.Contains(err.Error(), certPath) {
		t.Fatalf("err = %v, want an error naming %s", err, certPath)
	}
	if _, statErr := os.Lstat(keyPath); statErr == nil {
		t.Fatal("key written next to a non-regular cert path")
	}
}

func TestLoadOrCreateCertTightensParent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX mode bits")
	}
	dir := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil { // umask-independent
		t.Fatal(err)
	}
	certPath, keyPath := certPaths(dir)
	if _, _, err := LoadOrCreateCert(certPath, keyPath, clockAt(fixedNow), nil); err != nil {
		t.Fatal(err)
	}
	di, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if di.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode = %v, want 0700", di.Mode().Perm())
	}
}

func TestLoadOrCreateCertReadOnlyHint(t *testing.T) {
	orig := writeFileAtomic
	t.Cleanup(func() { writeFileAtomic = orig })
	writeFileAtomic = func(path string, _ []byte) error {
		return &fs.PathError{Op: "open", Path: path, Err: syscall.EROFS}
	}
	certPath, keyPath := certPaths(t.TempDir())
	_, _, err := LoadOrCreateCert(certPath, keyPath, clockAt(fixedNow), nil)
	if err == nil {
		t.Fatal("no error")
	}
	const hint = ": read-only file system; add StateDirectory=op-hostpower-tls and ReadWritePaths=/var/lib/op-hostpower-tls to the unit (see the agent README)"
	if !strings.HasSuffix(err.Error(), hint) || strings.Count(err.Error(), "read-only file system") != 1 {
		t.Fatalf("err = %q, want it to end in %q", err, hint)
	}
}

func TestLoadOrCreateCertOtherWriteErrorNoHint(t *testing.T) {
	orig := writeFileAtomic
	t.Cleanup(func() { writeFileAtomic = orig })
	writeFileAtomic = func(string, []byte) error { return errors.New("disk on fire") }
	certPath, keyPath := certPaths(t.TempDir())
	_, _, err := LoadOrCreateCert(certPath, keyPath, clockAt(fixedNow), nil)
	if err == nil || strings.Contains(err.Error(), "StateDirectory") || !strings.Contains(err.Error(), "disk on fire") {
		t.Fatalf("err = %v", err)
	}
}

func TestLoadOrCreateCertKeyWriteFailureWritesNoCert(t *testing.T) {
	orig := writeFileAtomic
	t.Cleanup(func() { writeFileAtomic = orig })
	var order []string
	writeFileAtomic = func(path string, _ []byte) error {
		order = append(order, filepath.Base(path))
		return fmt.Errorf("fail %s", filepath.Base(path))
	}
	certPath, keyPath := certPaths(t.TempDir())
	if _, _, err := LoadOrCreateCert(certPath, keyPath, clockAt(fixedNow), nil); err == nil {
		t.Fatal("no error")
	}
	if !slices.Equal(order, []string{"tls.key"}) {
		t.Fatalf("write order = %v, want key only", order)
	}
}

func TestLoadOrCreateCertCertWriteFailureRemovesKey(t *testing.T) {
	orig := writeFileAtomic
	t.Cleanup(func() { writeFileAtomic = orig })
	calls := 0
	writeFileAtomic = func(path string, data []byte) error {
		calls++
		if calls == 2 {
			return errors.New("cert write failed")
		}
		return orig(path, data)
	}
	certPath, keyPath := certPaths(t.TempDir())
	_, created, err := LoadOrCreateCert(certPath, keyPath, clockAt(fixedNow), nil)
	if err == nil || created || !strings.Contains(err.Error(), "cert write failed") {
		t.Fatalf("created = %v err = %v", created, err)
	}
	if calls != 2 {
		t.Fatalf("writes = %d, want 2 (key, then cert)", calls)
	}
	for _, p := range []string{keyPath, certPath} {
		if _, statErr := os.Lstat(p); !errors.Is(statErr, fs.ErrNotExist) {
			t.Errorf("%s left behind (stat err %v)", p, statErr)
		}
	}
	// The next start generates a fresh pair instead of failing on half a pair.
	writeFileAtomic = orig
	if _, created, err := LoadOrCreateCert(certPath, keyPath, clockAt(fixedNow), nil); err != nil || !created {
		t.Fatalf("retry: created = %v err = %v", created, err)
	}
}

func TestLoadOrCreateCertSymlinkedPair(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	srcCert, srcKey := certPaths(t.TempDir())
	want, _, err := LoadOrCreateCert(srcCert, srcKey, clockAt(fixedNow), nil)
	if err != nil {
		t.Fatal(err)
	}
	linkDir := t.TempDir()
	linkCert, linkKey := certPaths(linkDir)
	if err := os.Symlink(srcCert, linkCert); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(srcKey, linkKey); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, nil))
	got, created, err := LoadOrCreateCert(linkCert, linkKey, clockAt(fixedNow), log)
	if err != nil || created {
		t.Fatalf("created = %v err = %v", created, err)
	}
	if !bytes.Equal(got.Certificate[0], want.Certificate[0]) {
		t.Fatal("symlinked pair loaded a different certificate")
	}
	// The 0600 target counts, not the symlink's own 0777 mode.
	if strings.Contains(logs.String(), "level=WARN") {
		t.Fatalf("unexpected warning: %q", logs.String())
	}
}

func TestLoadOrCreateCertBadSymlinkIsAnError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	for _, tc := range []struct {
		name   string
		target func(t *testing.T) string
	}{
		{"dangling", func(t *testing.T) string { return filepath.Join(t.TempDir(), "missing.crt") }},
		{"to a directory", func(t *testing.T) string { return t.TempDir() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			certPath, keyPath := certPaths(dir)
			if err := os.Symlink(tc.target(t), certPath); err != nil {
				t.Fatal(err)
			}
			before := listDir(t, dir)
			_, created, err := LoadOrCreateCert(certPath, keyPath, clockAt(fixedNow), nil)
			if err == nil || created || !strings.Contains(err.Error(), certPath) {
				t.Fatalf("created = %v err = %v, want an error naming %s", created, err, certPath)
			}
			if after := listDir(t, dir); !slices.Equal(before, after) {
				t.Errorf("dir listing changed: %v -> %v", before, after)
			}
		})
	}
}
