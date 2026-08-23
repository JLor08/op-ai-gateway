// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package trust

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"op-ai-server-agent/internal/sample"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	agentclient "op-ai-server-agent/internal/client"
)

type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pem  []byte
}

func newTestCA(t *testing.T, name string) testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate CA key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          randomSerial(t),
		Subject:               pkix.Name{CommonName: name},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create CA: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse CA: %v", err)
	}
	return testCA{cert: cert, key: key, pem: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}
}

func randomSerial(t *testing.T) *big.Int {
	t.Helper()
	limit := new(big.Int).Lsh(big.NewInt(1), 120)
	n, err := rand.Int(rand.Reader, limit)
	if err != nil {
		t.Fatalf("random serial: %v", err)
	}
	return n
}

func leafFor(t *testing.T, ca testCA, dnsNames []string, ips []net.IP) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate leaf key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: randomSerial(t),
		Subject:      pkix.Name{CommonName: "gateway"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     dnsNames,
		IPAddresses:  ips,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatalf("create leaf: %v", err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

func tlsServer(t *testing.T, cert tls.Certificate) *httptest.Server {
	t.Helper()
	return tlsServerWithHandler(t, cert, false, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
}

func tlsServerWithHandler(t *testing.T, cert tls.Certificate, enableHTTP2 bool, handler http.Handler) *httptest.Server {
	t.Helper()
	srv := httptest.NewUnstartedServer(handler)
	srv.EnableHTTP2 = enableHTTP2
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}

func createSymlinkOrSkip(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unavailable on this platform: %v", err)
	}
}

func requireGET(t *testing.T, client *http.Client, rawURL string) {
	t.Helper()
	resp, err := client.Get(rawURL)
	if err != nil {
		t.Fatalf("GET %s: %v", rawURL, err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("GET %s status = %d, want %d", rawURL, resp.StatusCode, http.StatusNoContent)
	}
}

func fingerprint(cert *x509.Certificate) string {
	sum := sha256.Sum256(cert.Raw)
	return hex.EncodeToString(sum[:])
}

func useSystemRoots(t *testing.T, roots ...testCA) {
	t.Helper()
	previous := systemCertPool
	systemCertPool = func() (*x509.CertPool, error) {
		pool := x509.NewCertPool()
		for _, root := range roots {
			if !pool.AppendCertsFromPEM(root.pem) {
				t.Fatalf("append fake system root")
			}
		}
		return pool, nil
	}
	t.Cleanup(func() { systemCertPool = previous })
}

func writePEM(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestStoreAddsSystemAndAllConfiguredRoots(t *testing.T) {
	systemRoot := newTestCA(t, "system")
	operatorRoot := newTestCA(t, "operator")
	certDirRoot := newTestCA(t, "cert-dir")
	cacheRoot := newTestCA(t, "cache")
	inlineRoot := newTestCA(t, "inline")
	useSystemRoots(t, systemRoot)

	dir := t.TempDir()
	operatorPath := filepath.Join(dir, "operator.pem")
	certDir := filepath.Join(dir, "certs")
	cachePath := filepath.Join(dir, "managed-cache.pem")
	// Duplicate the cache root at the end of the operator bundle: reporting must
	// preserve source priority and de-duplicate by fingerprint.
	writePEM(t, operatorPath, append(append([]byte(nil), operatorRoot.pem...), cacheRoot.pem...))
	writePEM(t, filepath.Join(certDir, "ca.pem"), certDirRoot.pem)
	writePEM(t, cachePath, cacheRoot.pem)

	store, err := New(Options{
		CAFile:      operatorPath,
		CertDir:     certDir,
		CACacheFile: cachePath,
		CAPEM:       string(inlineRoot.pem),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	client := store.HTTPClient(2 * time.Second)
	for name, root := range map[string]testCA{
		"system": systemRoot, "operator": operatorRoot, "cert-dir": certDirRoot,
		"cache": cacheRoot, "inline": inlineRoot,
	} {
		t.Run(name, func(t *testing.T) {
			srv := tlsServer(t, leafFor(t, root, nil, []net.IP{net.ParseIP("127.0.0.1")}))
			requireGET(t, client, srv.URL)
		})
	}

	want := []string{
		fingerprint(cacheRoot.cert),
		fingerprint(certDirRoot.cert),
		fingerprint(inlineRoot.cert),
		fingerprint(operatorRoot.cert),
	}
	if got := store.DurableFingerprints(); !reflect.DeepEqual(got, want) {
		t.Fatalf("DurableFingerprints = %v, want ordered/deduplicated %v", got, want)
	}
}

func TestStoreReloadsContentChangeNotMtimeChange(t *testing.T) {
	useSystemRoots(t)
	root1 := newTestCA(t, "root-one")
	root2 := newTestCA(t, "root-two")
	path := filepath.Join(t.TempDir(), "operator.pem")
	writePEM(t, path, root1.pem)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat initial CA: %v", err)
	}

	store, err := New(Options{CAFile: path})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	client := store.HTTPClient(2 * time.Second)
	requireGET(t, client, tlsServer(t, leafFor(t, root1, nil, []net.IP{net.ParseIP("127.0.0.1")})).URL)

	writePEM(t, path, root2.pem)
	if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
		t.Fatalf("restore CA mtime: %v", err)
	}
	requireGET(t, client, tlsServer(t, leafFor(t, root2, nil, []net.IP{net.ParseIP("127.0.0.1")})).URL)
}

func TestStoreInvalidReplacementKeepsLastGoodPool(t *testing.T) {
	useSystemRoots(t)
	root := newTestCA(t, "last-good")
	path := filepath.Join(t.TempDir(), "operator.pem")
	writePEM(t, path, root.pem)
	store, err := New(Options{CAFile: path})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	client := store.HTTPClient(2 * time.Second)
	srv := tlsServer(t, leafFor(t, root, nil, []net.IP{net.ParseIP("127.0.0.1")}))
	requireGET(t, client, srv.URL)

	writePEM(t, path, []byte("not PEM certificate material"))
	client.CloseIdleConnections()
	requireGET(t, client, srv.URL)
	if got := store.DurableFingerprints(); len(got) != 0 {
		t.Fatalf("DurableFingerprints after invalid replacement = %v, want none", got)
	}

	// Restoring the exact last-good bytes must mark the source durable again even
	// though its content hash is unchanged from the pool copy.
	writePEM(t, path, root.pem)
	client.CloseIdleConnections()
	requireGET(t, client, srv.URL)
	if got, want := store.DurableFingerprints(), []string{fingerprint(root.cert)}; !reflect.DeepEqual(got, want) {
		t.Fatalf("DurableFingerprints after same-content restore = %v, want %v", got, want)
	}
}

func TestStoreUnreadableReplacementKeepsLastGoodPool(t *testing.T) {
	useSystemRoots(t)
	root := newTestCA(t, "last-good-unreadable")
	path := filepath.Join(t.TempDir(), "operator.pem")
	writePEM(t, path, root.pem)
	store, err := New(Options{CAFile: path})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	client := store.HTTPClient(2 * time.Second)
	srv := tlsServer(t, leafFor(t, root, nil, []net.IP{net.ParseIP("127.0.0.1")}))
	requireGET(t, client, srv.URL)

	if err := os.Remove(path); err != nil {
		t.Fatalf("remove CA file: %v", err)
	}
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatalf("replace CA file with unreadable directory: %v", err)
	}
	client.CloseIdleConnections()
	requireGET(t, client, srv.URL)
	if got := store.DurableFingerprints(); len(got) != 0 {
		t.Fatalf("DurableFingerprints after unreadable replacement = %v, want none", got)
	}
}

func TestStoreManagedCacheReportsOnlyDiskDurableRoots(t *testing.T) {
	useSystemRoots(t)
	root := newTestCA(t, "managed-durable")
	cache := filepath.Join(t.TempDir(), "managed-cache.pem")
	installer, err := New(Options{CACacheFile: cache})
	if err != nil {
		t.Fatal(err)
	}
	if err := installer.InstallManagedBundle(root.pem); err != nil {
		t.Fatal(err)
	}

	// Re-open from disk so the lifecycle below starts from a root proven to be
	// loadable after process restart, not merely the installer's live memory.
	store, err := New(Options{CACacheFile: cache})
	if err != nil {
		t.Fatal(err)
	}
	client := store.HTTPClient(2 * time.Second)
	srv := tlsServer(t, leafFor(t, root, nil, []net.IP{net.ParseIP("127.0.0.1")}))
	requireGET(t, client, srv.URL)

	if err := os.Remove(cache); err != nil {
		t.Fatal(err)
	}
	client.CloseIdleConnections()
	requireGET(t, client, srv.URL) // live process keeps the last-good pool
	if got := store.DurableFingerprints(); len(got) != 0 {
		t.Fatalf("DurableFingerprints after cache removal = %v, want none", got)
	}

	restarted, err := New(Options{CACacheFile: cache})
	if err != nil {
		t.Fatal(err)
	}
	if resp, err := restarted.HTTPClient(2 * time.Second).Get(srv.URL); err == nil {
		resp.Body.Close()
		t.Fatal("restarted store trusted a root whose cache file is absent")
	}

	writePEM(t, cache, []byte("invalid certificate material"))
	client.CloseIdleConnections()
	requireGET(t, client, srv.URL)
	if got := store.DurableFingerprints(); len(got) != 0 {
		t.Fatalf("DurableFingerprints after invalid cache = %v, want none", got)
	}

	writePEM(t, cache, root.pem)
	client.CloseIdleConnections()
	requireGET(t, client, srv.URL)
	if got, want := store.DurableFingerprints(), []string{fingerprint(root.cert)}; !reflect.DeepEqual(got, want) {
		t.Fatalf("DurableFingerprints after cache restore = %v, want %v", got, want)
	}
}

func TestStoreReadOnlyFilesReportOnlyCurrentlyDurableRoots(t *testing.T) {
	useSystemRoots(t)
	root := newTestCA(t, "read-only-durable")
	for _, tc := range []struct {
		name    string
		options func(string) Options
		path    func(string) string
	}{
		{
			name:    "ca_file",
			options: func(base string) Options { return Options{CAFile: filepath.Join(base, "operator.pem")} },
			path:    func(base string) string { return filepath.Join(base, "operator.pem") },
		},
		{
			name:    "cert_dir",
			options: func(base string) Options { return Options{CertDir: filepath.Join(base, "certs")} },
			path:    func(base string) string { return filepath.Join(base, "certs", "ca.pem") },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := t.TempDir()
			path := tc.path(base)
			writePEM(t, path, root.pem)
			store, err := New(tc.options(base))
			if err != nil {
				t.Fatal(err)
			}
			client := store.HTTPClient(2 * time.Second)
			srv := tlsServer(t, leafFor(t, root, nil, []net.IP{net.ParseIP("127.0.0.1")}))
			requireGET(t, client, srv.URL)

			writePEM(t, path, []byte("invalid certificate material"))
			client.CloseIdleConnections()
			requireGET(t, client, srv.URL)
			if got := store.DurableFingerprints(); len(got) != 0 {
				t.Fatalf("DurableFingerprints after invalid source = %v, want none", got)
			}

			writePEM(t, path, root.pem)
			client.CloseIdleConnections()
			requireGET(t, client, srv.URL)
			if got, want := store.DurableFingerprints(), []string{fingerprint(root.cert)}; !reflect.DeepEqual(got, want) {
				t.Fatalf("DurableFingerprints after same-content restore = %v, want %v", got, want)
			}

			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			client.CloseIdleConnections()
			requireGET(t, client, srv.URL)
			if got := store.DurableFingerprints(); len(got) != 0 {
				t.Fatalf("DurableFingerprints after missing source = %v, want none", got)
			}
		})
	}

	inline, err := New(Options{CAPEM: string(root.pem)})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := inline.DurableFingerprints(), []string{fingerprint(root.cert)}; !reflect.DeepEqual(got, want) {
		t.Fatalf("inline DurableFingerprints = %v, want %v", got, want)
	}
}

func TestStoreWarningsAreThrottledAndNeverContainPEMMaterial(t *testing.T) {
	useSystemRoots(t)
	secretMaterial := "-----BEGIN PRIVATE KEY-----\nPRIVATE-SECRET-MATERIAL\n-----END PRIVATE KEY-----\n"
	path := filepath.Join(t.TempDir(), "operator.pem")
	writePEM(t, path, []byte(secretMaterial))

	var logs bytes.Buffer
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })

	store, err := New(Options{CAFile: path})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	store.refreshFileSources()
	store.refreshFileSources()
	got := logs.String()
	if count := strings.Count(got, "gateway trust source unavailable"); count != 1 {
		t.Fatalf("warning count = %d, want 1; logs: %s", count, got)
	}
	if strings.Contains(got, "PRIVATE-SECRET-MATERIAL") || strings.Contains(got, "BEGIN PRIVATE KEY") {
		t.Fatalf("warning leaked PEM material: %s", got)
	}
}

func TestStoreInstallManagedBundlePersistsBeforeDurableReport(t *testing.T) {
	useSystemRoots(t)
	root := newTestCA(t, "managed")
	cachePath := filepath.Join(t.TempDir(), "cache", "server-agent-ca.pem")
	store, err := New(Options{CACacheFile: cachePath})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := store.InstallManagedBundle([]byte("not PEM")); err == nil {
		t.Fatal("InstallManagedBundle accepted non-PEM input")
	}

	previous := atomicWriteFile
	called := false
	atomicWriteFile = func(path string, data []byte, validate cachePathValidator) error {
		called = true
		if got := store.DurableFingerprints(); len(got) != 0 {
			t.Fatalf("fingerprints became durable before atomic write completed: %v", got)
		}
		return previous(path, data, validate)
	}
	t.Cleanup(func() { atomicWriteFile = previous })

	if err := store.InstallManagedBundle(root.pem); err != nil {
		t.Fatalf("InstallManagedBundle: %v", err)
	}
	if !called {
		t.Fatal("atomicWriteFile was not called")
	}
	if got, want := store.DurableFingerprints(), []string{fingerprint(root.cert)}; !reflect.DeepEqual(got, want) {
		t.Fatalf("DurableFingerprints = %v, want %v", got, want)
	}
	if raw, err := os.ReadFile(cachePath); err != nil || !reflect.DeepEqual(raw, root.pem) {
		t.Fatalf("managed cache = %q, err %v; want installed bundle", raw, err)
	}
	if info, err := os.Stat(cachePath); err != nil || info.Mode().Perm() != 0o644 {
		t.Fatalf("managed cache mode = %v, err %v; want 0644", info.Mode().Perm(), err)
	}
	restarted, err := New(Options{CACacheFile: cachePath})
	if err != nil {
		t.Fatalf("restart New: %v", err)
	}
	if got, want := restarted.DurableFingerprints(), []string{fingerprint(root.cert)}; !reflect.DeepEqual(got, want) {
		t.Fatalf("restarted DurableFingerprints = %v, want %v", got, want)
	}
}

func TestStoreInstallFailureIsUsableInMemoryButNotDurable(t *testing.T) {
	useSystemRoots(t)
	root := newTestCA(t, "memory-only")
	cachePath := filepath.Join(t.TempDir(), "server-agent-ca.pem")
	store, err := New(Options{CACacheFile: cachePath})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	previous := atomicWriteFile
	atomicWriteFile = func(string, []byte, cachePathValidator) error { return errors.New("injected atomic rename failure") }
	t.Cleanup(func() { atomicWriteFile = previous })

	if err := store.InstallManagedBundle(root.pem); err == nil {
		t.Fatal("InstallManagedBundle should surface the persistence failure")
	}
	if got := store.DurableFingerprints(); len(got) != 0 {
		t.Fatalf("memory-only bundle reported durable fingerprints: %v", got)
	}
	if _, err := os.Stat(cachePath); !os.IsNotExist(err) {
		t.Fatalf("cache exists after injected failure: %v", err)
	}
	srv := tlsServer(t, leafFor(t, root, nil, []net.IP{net.ParseIP("127.0.0.1")}))
	requireGET(t, store.HTTPClient(2*time.Second), srv.URL)
}

func TestStoreInstallWriteFailureKeepsIntactPreviousCacheDurable(t *testing.T) {
	useSystemRoots(t)
	oldRoot := newTestCA(t, "intact-old-cache")
	newRoot := newTestCA(t, "memory-only-new-cache")
	cachePath := filepath.Join(t.TempDir(), "server-agent-ca.pem")
	writePEM(t, cachePath, oldRoot.pem)
	store, err := New(Options{CACacheFile: cachePath})
	if err != nil {
		t.Fatal(err)
	}
	previous := atomicWriteFile
	atomicWriteFile = func(string, []byte, cachePathValidator) error { return errors.New("injected write failure") }
	t.Cleanup(func() { atomicWriteFile = previous })

	if err := store.InstallManagedBundle(newRoot.pem); err == nil {
		t.Fatal("InstallManagedBundle should surface the write failure")
	}
	if got, want := store.DurableFingerprints(), []string{fingerprint(oldRoot.cert)}; !reflect.DeepEqual(got, want) {
		t.Fatalf("DurableFingerprints = %v, want intact previous cache %v", got, want)
	}
	if got, err := os.ReadFile(cachePath); err != nil || !bytes.Equal(got, oldRoot.pem) {
		t.Fatalf("previous cache intact=%v bytes=%d err=%v", err == nil && bytes.Equal(got, oldRoot.pem), len(got), err)
	}
}

func TestStoreInstallWithoutCacheIsUsableInMemoryButNotDurable(t *testing.T) {
	useSystemRoots(t)
	root := newTestCA(t, "memory-only-no-cache")
	store, err := New(Options{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := store.InstallManagedBundle(root.pem); err != nil {
		t.Fatalf("InstallManagedBundle: %v", err)
	}
	if got := store.DurableFingerprints(); len(got) != 0 {
		t.Fatalf("memory-only bundle reported durable fingerprints: %v", got)
	}
	srv := tlsServer(t, leafFor(t, root, nil, []net.IP{net.ParseIP("127.0.0.1")}))
	requireGET(t, store.HTTPClient(2*time.Second), srv.URL)
}

func TestStoreConcurrentInstallsKeepDiskAndDurableStateConsistent(t *testing.T) {
	useSystemRoots(t)
	rootA := newTestCA(t, "managed-a")
	rootB := newTestCA(t, "managed-b")
	cachePath := filepath.Join(t.TempDir(), "server-agent-ca.pem")
	store, err := New(Options{CACacheFile: cachePath})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	previous := atomicWriteFile
	aWritten := make(chan struct{})
	bWritten := make(chan struct{})
	releaseA := make(chan struct{})
	atomicWriteFile = func(path string, data []byte, validate cachePathValidator) error {
		if err := previous(path, data, validate); err != nil {
			return err
		}
		if bytes.Equal(data, rootA.pem) {
			close(aWritten)
			<-releaseA
		} else {
			close(bWritten)
		}
		return nil
	}
	t.Cleanup(func() { atomicWriteFile = previous })

	aDone := make(chan error, 1)
	bDone := make(chan error, 1)
	go func() { aDone <- store.InstallManagedBundle(rootA.pem) }()
	<-aWritten
	go func() { bDone <- store.InstallManagedBundle(rootB.pem) }()

	select {
	case <-bWritten:
		// The unfixed implementation lets B replace the file and update state
		// while A is paused after its rename but before its state update.
	case <-time.After(100 * time.Millisecond):
		// A serialized implementation correctly holds B outside the writer.
	}
	close(releaseA)
	if err := <-aDone; err != nil {
		t.Fatalf("install A: %v", err)
	}
	if err := <-bDone; err != nil {
		t.Fatalf("install B: %v", err)
	}

	if raw, err := os.ReadFile(cachePath); err != nil || !bytes.Equal(raw, rootB.pem) {
		t.Fatalf("final cache does not contain the second bundle: err=%v", err)
	}
	if got, want := store.DurableFingerprints(), []string{fingerprint(rootB.cert)}; !reflect.DeepEqual(got, want) {
		t.Fatalf("DurableFingerprints = %v, want disk-consistent %v", got, want)
	}
}

func TestStoreRefreshDoesNotObserveInFlightInstall(t *testing.T) {
	useSystemRoots(t)
	oldRoot := newTestCA(t, "refresh-old-root")
	newRoot := newTestCA(t, "refresh-new-root")
	cachePath := filepath.Join(t.TempDir(), "server-agent-ca.pem")
	writePEM(t, cachePath, oldRoot.pem)
	store, err := New(Options{CACacheFile: cachePath})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	previous := atomicWriteFile
	written := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	atomicWriteFile = func(path string, data []byte, validate cachePathValidator) error {
		if err := previous(path, data, validate); err != nil {
			return err
		}
		close(written)
		<-release
		return nil
	}
	t.Cleanup(func() { atomicWriteFile = previous })

	installDone := make(chan error, 1)
	go func() { installDone <- store.InstallManagedBundle(newRoot.pem) }()
	<-written
	refreshDone := make(chan struct{})
	go func() {
		store.refreshFileSources()
		close(refreshDone)
	}()

	select {
	case <-refreshDone:
		unblock()
		<-installDone
		t.Fatal("refresh observed the cache while its install transaction was still in flight")
	case <-time.After(100 * time.Millisecond):
	}
	unblock()
	if err := <-installDone; err != nil {
		t.Fatalf("InstallManagedBundle: %v", err)
	}
	select {
	case <-refreshDone:
	case <-time.After(time.Second):
		t.Fatal("refresh remained blocked after install completed")
	}
}

func TestStoreNeverWritesOperatorCAFile(t *testing.T) {
	useSystemRoots(t)
	operatorRoot := newTestCA(t, "operator")
	certDirRoot := newTestCA(t, "cert-dir")
	managedRoot := newTestCA(t, "managed")
	dir := t.TempDir()
	operatorPath := filepath.Join(dir, "operator.pem")
	certDirPath := filepath.Join(dir, "certs", "ca.pem")
	cachePath := filepath.Join(dir, "managed", "ca.pem")
	writePEM(t, operatorPath, operatorRoot.pem)
	writePEM(t, certDirPath, certDirRoot.pem)
	store, err := New(Options{CAFile: operatorPath, CertDir: filepath.Dir(certDirPath), CACacheFile: cachePath})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := store.InstallManagedBundle(managedRoot.pem); err != nil {
		t.Fatalf("InstallManagedBundle: %v", err)
	}
	if got, _ := os.ReadFile(operatorPath); !reflect.DeepEqual(got, operatorRoot.pem) {
		t.Fatal("operator ca_file was modified")
	}
	if got, _ := os.ReadFile(certDirPath); !reflect.DeepEqual(got, certDirRoot.pem) {
		t.Fatal("cert_dir/ca.pem was modified")
	}
	if got, _ := os.ReadFile(cachePath); !reflect.DeepEqual(got, managedRoot.pem) {
		t.Fatal("managed bundle was not written exclusively to ca_cache_file")
	}
}

func TestStoreRejectsManagedCacheReadOnlyPathCollisions(t *testing.T) {
	useSystemRoots(t)
	dir := t.TempDir()
	operatorPath := filepath.Join(dir, "operator.pem")
	certDir := filepath.Join(dir, "certs")
	writePEM(t, operatorPath, newTestCA(t, "operator").pem)
	writePEM(t, filepath.Join(certDir, "ca.pem"), newTestCA(t, "cert-dir").pem)

	for name, opts := range map[string]Options{
		"operator exact": {
			CAFile: operatorPath, CACacheFile: operatorPath,
		},
		"operator normalized alias": {
			CAFile: operatorPath, CACacheFile: filepath.Join(dir, "subdir", "..", "operator.pem"),
		},
		"cert dir": {
			CertDir: certDir, CACacheFile: filepath.Join(certDir, "ca.pem"),
		},
		"missing operator case-only alias": {
			CAFile: filepath.Join(dir, "missing", "ca.pem"), CACacheFile: filepath.Join(dir, "missing", "CA.PEM"),
		},
		"missing cert dir case-only alias": {
			CertDir: filepath.Join(dir, "missing-certs"), CACacheFile: filepath.Join(dir, "missing-certs", "CA.PEM"),
		},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := New(opts); err == nil {
				t.Fatal("New accepted ca_cache_file that aliases a read-only CA source")
			}
		})
	}
}

func TestStoreRejectsDanglingSymlinkManagedCacheAliases(t *testing.T) {
	useSystemRoots(t)

	t.Run("relative final target", func(t *testing.T) {
		dir := t.TempDir()
		operatorPath := filepath.Join(dir, "operator.pem")
		cachePath := filepath.Join(dir, "managed", "ca.pem")
		createSymlinkOrSkip(t, filepath.Join("managed", "ca.pem"), operatorPath)

		if _, err := New(Options{CAFile: operatorPath, CACacheFile: cachePath}); err == nil {
			t.Fatal("New accepted ca_cache_file behind a dangling operator ca_file symlink")
		}
	})

	t.Run("relative symlink chain", func(t *testing.T) {
		dir := t.TempDir()
		linksDir := filepath.Join(dir, "links")
		if err := os.Mkdir(linksDir, 0o755); err != nil {
			t.Fatalf("mkdir links: %v", err)
		}
		operatorPath := filepath.Join(dir, "operator.pem")
		chainPath := filepath.Join(linksDir, "next.pem")
		cachePath := filepath.Join(dir, "managed", "ca.pem")
		createSymlinkOrSkip(t, filepath.Join("links", "next.pem"), operatorPath)
		createSymlinkOrSkip(t, filepath.Join("..", "managed", "ca.pem"), chainPath)

		if _, err := New(Options{CAFile: operatorPath, CACacheFile: cachePath}); err == nil {
			t.Fatal("New accepted ca_cache_file behind a dangling operator symlink chain")
		}
	})
}

func TestStoreInstallRechecksManagedCacheReadOnlyPathCollisions(t *testing.T) {
	useSystemRoots(t)
	dir := t.TempDir()
	operatorPath := filepath.Join(dir, "operator.pem")
	cachePath := filepath.Join(dir, "managed", "ca.pem")
	store, err := New(Options{CAFile: operatorPath, CACacheFile: cachePath})
	if err != nil {
		t.Fatalf("New before alias exists: %v", err)
	}
	createSymlinkOrSkip(t, filepath.Join("managed", "ca.pem"), operatorPath)

	root := newTestCA(t, "must-not-write-through-alias")
	if err := store.InstallManagedBundle(root.pem); err == nil {
		t.Fatal("InstallManagedBundle accepted a cache alias introduced after New")
	}
	if _, err := os.Stat(cachePath); !os.IsNotExist(err) {
		t.Fatalf("managed cache was written despite read-only alias: %v", err)
	}
}

func TestStoreAtomicInstallRejectsCacheParentSwapToReadOnlyDirectory(t *testing.T) {
	useSystemRoots(t)
	dir := t.TempDir()
	operatorDir := filepath.Join(dir, "operator")
	managedDir := filepath.Join(dir, "managed")
	parkedManagedDir := filepath.Join(dir, "managed-original")
	operatorPath := filepath.Join(operatorDir, "ca.pem")
	cachePath := filepath.Join(managedDir, "ca.pem")
	operatorRoot := newTestCA(t, "operator-must-survive")
	managedRoot := newTestCA(t, "managed-must-not-overwrite")
	writePEM(t, operatorPath, operatorRoot.pem)
	if err := os.Mkdir(managedDir, 0o755); err != nil {
		t.Fatalf("mkdir managed cache dir: %v", err)
	}
	store, err := New(Options{CAFile: operatorPath, CACacheFile: cachePath})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	previous := atomicWriteFile
	atomicWriteFile = func(path string, data []byte, validate cachePathValidator) error {
		if err := os.Rename(managedDir, parkedManagedDir); err != nil {
			return fmt.Errorf("park managed directory: %w", err)
		}
		if err := os.Symlink(operatorDir, managedDir); err != nil {
			return fmt.Errorf("replace managed directory with symlink: %w", err)
		}
		return previous(path, data, validate)
	}
	t.Cleanup(func() { atomicWriteFile = previous })

	if err := store.InstallManagedBundle(managedRoot.pem); err == nil {
		t.Fatal("InstallManagedBundle accepted a cache-parent swap to the read-only CA directory")
	}
	if got, err := os.ReadFile(operatorPath); err != nil || !bytes.Equal(got, operatorRoot.pem) {
		t.Fatalf("operator CA was overwritten through cache-parent swap: err=%v", err)
	}
}

func TestStoreAtomicInstallRejectsCacheParentSwapAfterRootOpen(t *testing.T) {
	useSystemRoots(t)
	dir := t.TempDir()
	operatorDir := filepath.Join(dir, "operator")
	managedDir := filepath.Join(dir, "managed")
	parkedManagedDir := filepath.Join(dir, "managed-original")
	operatorPath := filepath.Join(operatorDir, "ca.pem")
	cachePath := filepath.Join(managedDir, "ca.pem")
	operatorRoot := newTestCA(t, "operator-must-survive-late-swap")
	managedRoot := newTestCA(t, "managed-must-remain-addressable")
	writePEM(t, operatorPath, operatorRoot.pem)
	if err := os.Mkdir(managedDir, 0o755); err != nil {
		t.Fatalf("mkdir managed cache dir: %v", err)
	}
	store, err := New(Options{CAFile: operatorPath, CACacheFile: cachePath})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	previous := atomicWriteFile
	atomicWriteFile = func(path string, data []byte, validate cachePathValidator) error {
		validations := 0
		return previous(path, data, func(root *os.Root, filename string) error {
			validations++
			if validations == 2 {
				if err := os.Rename(managedDir, parkedManagedDir); err != nil {
					return fmt.Errorf("park managed directory: %w", err)
				}
				if err := os.Symlink(operatorDir, managedDir); err != nil {
					return fmt.Errorf("replace managed directory with symlink: %w", err)
				}
			}
			return validate(root, filename)
		})
	}
	t.Cleanup(func() { atomicWriteFile = previous })

	if err := store.InstallManagedBundle(managedRoot.pem); err == nil {
		t.Fatal("InstallManagedBundle reported durable success after the selected cache directory changed")
	}
	if got, err := os.ReadFile(operatorPath); err != nil || !bytes.Equal(got, operatorRoot.pem) {
		t.Fatalf("operator CA changed after late cache-parent swap: err=%v", err)
	}
}

func TestStoreAtomicInstallRollsBackWhenOperatorAliasAppearsBeforeRename(t *testing.T) {
	useSystemRoots(t)
	oldRoot := newTestCA(t, "operator-alias-old-root")
	newRoot := newTestCA(t, "operator-alias-memory-root")
	dir := t.TempDir()
	cachePath := filepath.Join(dir, "managed-ca.pem")
	operatorPath := filepath.Join(dir, "operator-ca.pem")
	writePEM(t, cachePath, oldRoot.pem)
	createSymlinkOrSkip(t, filepath.Base(cachePath), operatorPath)
	if err := os.Remove(operatorPath); err != nil {
		t.Fatalf("remove symlink support probe: %v", err)
	}
	store, err := New(Options{CAFile: operatorPath, CACacheFile: cachePath})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	previous := selectedRootValidator
	validations := 0
	selectedRootValidator = func(root *os.Root, selectedPath string) error {
		validations++
		if validations == 2 {
			if err := previous(root, selectedPath); err != nil {
				return err
			}
			return os.Symlink(filepath.Base(cachePath), operatorPath)
		}
		return previous(root, selectedPath)
	}
	t.Cleanup(func() { selectedRootValidator = previous })

	if err := store.InstallManagedBundle(newRoot.pem); err == nil {
		t.Fatal("InstallManagedBundle reported durable success after ca_file became a cache alias")
	}
	if raw, err := os.ReadFile(cachePath); err != nil || !bytes.Equal(raw, oldRoot.pem) {
		t.Fatalf("previous durable cache was not restored after ca_file alias: err=%v", err)
	}
	if matches, err := filepath.Glob(filepath.Join(dir, ".server-agent-ca-*")); err != nil || len(matches) != 0 {
		t.Fatalf("ca_file alias rollback left transaction files %v, err=%v", matches, err)
	}
	if got, want := store.DurableFingerprints(), []string{fingerprint(oldRoot.cert)}; !reflect.DeepEqual(got, want) {
		t.Fatalf("DurableFingerprints = %v, want previous durable %v", got, want)
	}
	store.mu.RLock()
	gotMemory := append([]string(nil), store.memory.fingerprints...)
	store.mu.RUnlock()
	if want := []string{fingerprint(newRoot.cert)}; !reflect.DeepEqual(gotMemory, want) {
		t.Fatalf("RAM-only fingerprints = %v, want %v", gotMemory, want)
	}
}

func TestStoreAtomicFirstInstallRemovesCommitWhenOperatorAliasAppearsBeforeRename(t *testing.T) {
	useSystemRoots(t)
	newRoot := newTestCA(t, "first-operator-alias-memory-root")
	dir := t.TempDir()
	cachePath := filepath.Join(dir, "managed-ca.pem")
	operatorPath := filepath.Join(dir, "operator-ca.pem")
	createSymlinkOrSkip(t, filepath.Base(cachePath), operatorPath)
	if err := os.Remove(operatorPath); err != nil {
		t.Fatalf("remove symlink support probe: %v", err)
	}
	store, err := New(Options{CAFile: operatorPath, CACacheFile: cachePath})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	previous := selectedRootValidator
	validations := 0
	selectedRootValidator = func(root *os.Root, selectedPath string) error {
		validations++
		if validations == 2 {
			if err := previous(root, selectedPath); err != nil {
				return err
			}
			return os.Symlink(filepath.Base(cachePath), operatorPath)
		}
		return previous(root, selectedPath)
	}
	t.Cleanup(func() { selectedRootValidator = previous })

	if err := store.InstallManagedBundle(newRoot.pem); err == nil {
		t.Fatal("first InstallManagedBundle reported durable success after ca_file became a cache alias")
	}
	if _, err := os.Stat(cachePath); !os.IsNotExist(err) {
		t.Fatalf("rolled-back first install left managed cache: %v", err)
	}
	if matches, err := filepath.Glob(filepath.Join(dir, ".server-agent-ca-*")); err != nil || len(matches) != 0 {
		t.Fatalf("first ca_file alias rollback left transaction files %v, err=%v", matches, err)
	}
	if got := store.DurableFingerprints(); len(got) != 0 {
		t.Fatalf("DurableFingerprints = %v, want none after rolled-back first install", got)
	}
	store.mu.RLock()
	gotMemory := append([]string(nil), store.memory.fingerprints...)
	store.mu.RUnlock()
	if want := []string{fingerprint(newRoot.cert)}; !reflect.DeepEqual(gotMemory, want) {
		t.Fatalf("RAM-only fingerprints = %v, want %v", gotMemory, want)
	}
}

func TestStoreAtomicInstallRollsBackWhenSelectedParentChangesAtCommit(t *testing.T) {
	useSystemRoots(t)
	oldRoot := newTestCA(t, "old-durable-root")
	newRoot := newTestCA(t, "must-remain-memory-only-root")
	dir := t.TempDir()
	managedDir := filepath.Join(dir, "managed")
	parkedManagedDir := filepath.Join(dir, "managed-original")
	cachePath := filepath.Join(managedDir, "ca.pem")
	writePEM(t, cachePath, oldRoot.pem)
	store, err := New(Options{CACacheFile: cachePath})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	previous := selectedRootValidator
	validations := 0
	selectedRootValidator = func(root *os.Root, selectedPath string) error {
		validations++
		if validations == 2 {
			if err := previous(root, selectedPath); err != nil {
				return err
			}
			if err := os.Rename(managedDir, parkedManagedDir); err != nil {
				return fmt.Errorf("park selected cache directory after validation: %w", err)
			}
			if err := os.Mkdir(managedDir, 0o755); err != nil {
				return fmt.Errorf("replace selected cache directory after validation: %w", err)
			}
			return nil
		}
		return previous(root, selectedPath)
	}
	t.Cleanup(func() { selectedRootValidator = previous })

	if err := store.InstallManagedBundle(newRoot.pem); err == nil {
		t.Fatal("InstallManagedBundle reported durable success after its selected cache directory changed")
	}
	if validations != 3 {
		t.Fatalf("selected-root validations = %d, want final post-rename validation", validations)
	}
	if _, err := os.Stat(cachePath); !os.IsNotExist(err) {
		t.Fatalf("replacement cache directory unexpectedly contains a bundle: %v", err)
	}
	parkedCachePath := filepath.Join(parkedManagedDir, filepath.Base(cachePath))
	if raw, err := os.ReadFile(parkedCachePath); err != nil || !bytes.Equal(raw, oldRoot.pem) {
		t.Fatalf("previous durable cache was not rolled back: err=%v", err)
	}
	if matches, err := filepath.Glob(filepath.Join(parkedManagedDir, ".server-agent-ca-*.tmp")); err != nil || len(matches) != 0 {
		t.Fatalf("managed cache transaction left temp files %v, err=%v", matches, err)
	}
	if got := store.DurableFingerprints(); len(got) != 0 {
		t.Fatalf("DurableFingerprints = %v, want none; the parked copy is recovery-only", got)
	}
	store.mu.RLock()
	gotMemory := append([]string(nil), store.memory.fingerprints...)
	store.mu.RUnlock()
	if want := []string{fingerprint(newRoot.cert)}; !reflect.DeepEqual(gotMemory, want) {
		t.Fatalf("RAM-only fingerprints = %v, want %v", gotMemory, want)
	}
}

func TestStoreAtomicFirstInstallRemovesCommitWhenSelectedParentChanges(t *testing.T) {
	useSystemRoots(t)
	newRoot := newTestCA(t, "first-install-must-remain-memory-only")
	dir := t.TempDir()
	managedDir := filepath.Join(dir, "managed")
	parkedManagedDir := filepath.Join(dir, "managed-original")
	cachePath := filepath.Join(managedDir, "ca.pem")
	if err := os.Mkdir(managedDir, 0o755); err != nil {
		t.Fatalf("mkdir managed cache dir: %v", err)
	}
	store, err := New(Options{CACacheFile: cachePath})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	previous := selectedRootValidator
	validations := 0
	selectedRootValidator = func(root *os.Root, selectedPath string) error {
		validations++
		if validations == 2 {
			if err := previous(root, selectedPath); err != nil {
				return err
			}
			if err := os.Rename(managedDir, parkedManagedDir); err != nil {
				return fmt.Errorf("park selected cache directory after validation: %w", err)
			}
			if err := os.Mkdir(managedDir, 0o755); err != nil {
				return fmt.Errorf("replace selected cache directory after validation: %w", err)
			}
			return nil
		}
		return previous(root, selectedPath)
	}
	t.Cleanup(func() { selectedRootValidator = previous })

	if err := store.InstallManagedBundle(newRoot.pem); err == nil {
		t.Fatal("first InstallManagedBundle reported durable success after its selected cache directory changed")
	}
	if validations != 3 {
		t.Fatalf("selected-root validations = %d, want final post-rename validation", validations)
	}
	for _, checkPath := range []string{cachePath, filepath.Join(parkedManagedDir, filepath.Base(cachePath))} {
		if _, err := os.Stat(checkPath); !os.IsNotExist(err) {
			t.Fatalf("failed first install left cache at %q: %v", checkPath, err)
		}
	}
	if matches, err := filepath.Glob(filepath.Join(parkedManagedDir, ".server-agent-ca-*.tmp")); err != nil || len(matches) != 0 {
		t.Fatalf("first install transaction left temp files %v, err=%v", matches, err)
	}
	if got := store.DurableFingerprints(); len(got) != 0 {
		t.Fatalf("DurableFingerprints = %v, want none after rolled-back first install", got)
	}
	store.mu.RLock()
	gotMemory := append([]string(nil), store.memory.fingerprints...)
	store.mu.RUnlock()
	if want := []string{fingerprint(newRoot.cert)}; !reflect.DeepEqual(gotMemory, want) {
		t.Fatalf("RAM-only fingerprints = %v, want %v", gotMemory, want)
	}
}

func TestStoreAtomicInstallPreservesBackupWhenRollbackFails(t *testing.T) {
	useSystemRoots(t)
	oldRoot := newTestCA(t, "rollback-recovery-old-root")
	newRoot := newTestCA(t, "rollback-recovery-new-root")
	dir := t.TempDir()
	managedDir := filepath.Join(dir, "managed")
	parkedManagedDir := filepath.Join(dir, "managed-original")
	cachePath := filepath.Join(managedDir, "ca.pem")
	writePEM(t, cachePath, oldRoot.pem)
	store, err := New(Options{CACacheFile: cachePath})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	previous := selectedRootValidator
	validations := 0
	selectedRootValidator = func(root *os.Root, selectedPath string) error {
		validations++
		switch validations {
		case 2:
			if err := previous(root, selectedPath); err != nil {
				return err
			}
			if err := os.Rename(managedDir, parkedManagedDir); err != nil {
				return fmt.Errorf("park selected cache directory after validation: %w", err)
			}
			if err := os.Mkdir(managedDir, 0o755); err != nil {
				return fmt.Errorf("replace selected cache directory after validation: %w", err)
			}
			return nil
		case 3:
			if err := root.Remove(filepath.Base(cachePath)); err != nil {
				return fmt.Errorf("remove committed cache before injected rollback failure: %w", err)
			}
			if err := root.Mkdir(filepath.Base(cachePath), 0o755); err != nil {
				return fmt.Errorf("replace rollback target with directory: %w", err)
			}
		}
		return previous(root, selectedPath)
	}
	t.Cleanup(func() { selectedRootValidator = previous })

	err = store.InstallManagedBundle(newRoot.pem)
	if err == nil || !strings.Contains(err.Error(), "rollback managed CA cache") {
		t.Fatalf("InstallManagedBundle error = %v, want rollback failure", err)
	}
	backups, err := filepath.Glob(filepath.Join(parkedManagedDir, ".server-agent-ca-backup-*.pem"))
	if err != nil {
		t.Fatalf("glob recovery backups: %v", err)
	}
	if len(backups) != 1 {
		t.Fatalf("recovery backups = %v, want exactly one preserved copy", backups)
	}
	if raw, err := os.ReadFile(backups[0]); err != nil || !bytes.Equal(raw, oldRoot.pem) {
		t.Fatalf("preserved recovery backup does not contain previous cache: err=%v", err)
	}
	if got := store.DurableFingerprints(); len(got) != 0 {
		t.Fatalf("DurableFingerprints = %v, want none when only a recovery backup remains", got)
	}
}

func TestStoreHTTPTransportChecksHostnameAndUsesFreshRoots(t *testing.T) {
	useSystemRoots(t)
	root1 := newTestCA(t, "root-one")
	root2 := newTestCA(t, "root-two")
	path := filepath.Join(t.TempDir(), "operator.pem")
	writePEM(t, path, root1.pem)
	store, err := New(Options{CAFile: path})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	client := store.HTTPClient(2 * time.Second)

	wrongName := tlsServer(t, leafFor(t, root1, []string{"gateway.example.test"}, nil))
	if _, err := client.Get(wrongName.URL); err == nil {
		t.Fatal("HTTP transport accepted a trusted certificate for the wrong hostname")
	}

	valid := tlsServer(t, leafFor(t, root1, nil, []net.IP{net.ParseIP("127.0.0.1")}))
	requireGET(t, client, valid.URL)
	writePEM(t, path, root2.pem)
	fresh := tlsServer(t, leafFor(t, root2, nil, []net.IP{net.ParseIP("127.0.0.1")}))
	requireGET(t, client, fresh.URL)
}

func TestStoreHTTPClientForcesHTTP2(t *testing.T) {
	useSystemRoots(t)
	root := newTestCA(t, "http2-root")
	proto := make(chan int, 1)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proto <- r.ProtoMajor
		w.WriteHeader(http.StatusNoContent)
	}))
	srv.EnableHTTP2 = true
	srv.TLS = &tls.Config{
		Certificates: []tls.Certificate{leafFor(t, root, nil, []net.IP{net.ParseIP("127.0.0.1")})},
		MinVersion:   tls.VersionTLS12,
	}
	srv.StartTLS()
	t.Cleanup(srv.Close)

	store, err := New(Options{CAPEM: string(root.pem)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	requireGET(t, store.HTTPClient(2*time.Second), srv.URL)
	if got := <-proto; got != 2 {
		t.Fatalf("negotiated HTTP/%d, want HTTP/2", got)
	}
}

func TestStoreHTTPClientUsesHTTP1ForH2CapableWSSender(t *testing.T) {
	useSystemRoots(t)
	root := newTestCA(t, "wss-http1-root")
	srv, frames := h2CapableWSServer(t, root)
	store, err := New(Options{CAPEM: string(root.pem)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	requireWSSenderFrame(t, store.HTTPClient(0), srv.URL, frames)
}

func TestStoreHTTPClientUsesHTTP1ForH2CapableWSSenderThroughCONNECT(t *testing.T) {
	useSystemRoots(t)
	proxy, connects := newCONNECTProxy(t)
	proxyURL, err := url.Parse(proxy.URL)
	if err != nil {
		t.Fatalf("parse proxy URL: %v", err)
	}
	previousDefault := http.DefaultTransport
	base := previousDefault.(*http.Transport).Clone()
	base.Proxy = http.ProxyURL(proxyURL)
	http.DefaultTransport = base
	t.Cleanup(func() { http.DefaultTransport = previousDefault })

	root1 := newTestCA(t, "wss-connect-http1-root-one")
	root2 := newTestCA(t, "wss-connect-http1-root-two")
	path := filepath.Join(t.TempDir(), "operator.pem")
	writePEM(t, path, root1.pem)
	store, err := New(Options{CAFile: path})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	client := store.HTTPClient(0)
	srv1, frames1 := h2CapableWSServer(t, root1)
	requireWSSenderFrame(t, client, srv1.URL, frames1)
	writePEM(t, path, root2.pem)
	srv2, frames2 := h2CapableWSServer(t, root2)
	requireWSSenderFrame(t, client, srv2.URL, frames2)
	if got := connects.Load(); got < 2 {
		t.Fatalf("CONNECT requests = %d, want at least 2 across root rotation", got)
	}
}

type wsFrameResult struct {
	typeName string
	err      error
}

func h2CapableWSServer(t *testing.T, root testCA) (*httptest.Server, <-chan wsFrameResult) {
	t.Helper()
	frames := make(chan wsFrameResult, 1)
	srv := tlsServerWithHandler(t,
		leafFor(t, root, nil, []net.IP{net.ParseIP("127.0.0.1")}), true,
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if got := r.Header.Get("Authorization"); got != "Bearer wss-token" {
				frames <- wsFrameResult{err: fmt.Errorf("Authorization = %q", got)}
				return
			}
			conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
			if err != nil {
				frames <- wsFrameResult{err: fmt.Errorf("accept websocket over HTTP/%d: %w", r.ProtoMajor, err)}
				return
			}
			defer conn.CloseNow()
			var frame struct {
				Type string `json:"type"`
			}
			if err := wsjson.Read(r.Context(), conn, &frame); err != nil {
				frames <- wsFrameResult{err: fmt.Errorf("read websocket frame: %w", err)}
				return
			}
			frames <- wsFrameResult{typeName: frame.Type}
		}))
	return srv, frames
}

func requireWSSenderFrame(t *testing.T, httpClient *http.Client, gatewayURL string, frames <-chan wsFrameResult) {
	t.Helper()
	sender, err := agentclient.NewWSSender(gatewayURL, "wss-token", httpClient)
	if err != nil {
		t.Fatalf("NewWSSender: %v", err)
	}
	defer sender.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := sender.Post(ctx, &sample.Sample{}); err != nil {
		t.Fatalf("WSSender Post: %v", err)
	}
	select {
	case result := <-frames:
		if result.err != nil {
			t.Fatal(result.err)
		}
		if result.typeName != "telemetry" {
			t.Fatalf("WebSocket frame type = %q, want telemetry", result.typeName)
		}
	case <-ctx.Done():
		t.Fatalf("waiting for WebSocket frame: %v", ctx.Err())
	}
}

func TestStoreHTTPClientRejectsHTTPSRedirectToHTTP(t *testing.T) {
	useSystemRoots(t)
	root := newTestCA(t, "redirect-root")
	var destinationCalls atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		destinationCalls.Add(1)
		if got := r.Header.Get("Authorization"); got != "" {
			t.Errorf("downgrade destination received Authorization %q", got)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(destination.Close)

	redirector := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Redirect(w, &http.Request{}, destination.URL, http.StatusFound)
	}))
	redirector.TLS = &tls.Config{
		Certificates: []tls.Certificate{leafFor(t, root, nil, []net.IP{net.ParseIP("127.0.0.1")})},
		MinVersion:   tls.VersionTLS12,
	}
	redirector.StartTLS()
	t.Cleanup(redirector.Close)

	store, err := New(Options{CAPEM: string(root.pem)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	req, err := http.NewRequest(http.MethodGet, redirector.URL, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer must-not-downgrade")
	resp, err := store.HTTPClient(2 * time.Second).Do(req)
	if resp != nil {
		resp.Body.Close()
	}
	if err == nil {
		t.Fatal("HTTPS-to-HTTP redirect was followed instead of rejected")
	}
	if got := destinationCalls.Load(); got != 0 {
		t.Fatalf("HTTP downgrade destination received %d requests, want 0", got)
	}
}

func TestStoreHTTPClientRejectsCrossOriginHTTPSRedirectWithoutContact(t *testing.T) {
	useSystemRoots(t)
	root := newTestCA(t, "cross-origin-root")
	cert := leafFor(t, root, nil, []net.IP{net.ParseIP("127.0.0.1")})
	var destinationCalls atomic.Int32
	destination := tlsServerWithHandler(t, cert, false, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		destinationCalls.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	redirector := tlsServerWithHandler(t, cert, false, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, destination.URL, http.StatusFound)
	}))

	store, err := New(Options{CAPEM: string(root.pem)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	req, err := http.NewRequest(http.MethodGet, redirector.URL, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer must-stay-on-origin")
	resp, err := store.HTTPClient(2 * time.Second).Do(req)
	if resp != nil {
		resp.Body.Close()
	}
	if err == nil {
		t.Fatal("cross-origin HTTPS redirect was followed")
	}
	if got := destinationCalls.Load(); got != 0 {
		t.Fatalf("cross-origin destination received %d requests, want 0", got)
	}
}

func TestStoreHTTPClientRejectsSubdomainHTTPSRedirectWithoutContact(t *testing.T) {
	useSystemRoots(t)
	routeTestDNSNamesToLoopback(t)
	root := newTestCA(t, "subdomain-redirect-root")
	var destinationCalls atomic.Int32
	destination := tlsServerWithHandler(t,
		leafFor(t, root, []string{"sub.gateway.example.test"}, nil), false,
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			destinationCalls.Add(1)
			w.WriteHeader(http.StatusNoContent)
		}))
	destinationURL := testServerURLWithHostname(t, destination, "sub.gateway.example.test")
	redirector := tlsServerWithHandler(t,
		leafFor(t, root, []string{"gateway.example.test"}, nil), false,
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, destinationURL, http.StatusFound)
		}))
	redirectorURL := testServerURLWithHostname(t, redirector, "gateway.example.test")

	store, err := New(Options{CAPEM: string(root.pem)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	req, err := http.NewRequest(http.MethodGet, redirectorURL, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer must-not-reach-subdomain")
	resp, err := store.HTTPClient(2 * time.Second).Do(req)
	if resp != nil {
		resp.Body.Close()
	}
	if err == nil {
		t.Fatal("subdomain HTTPS redirect was followed")
	}
	if got := destinationCalls.Load(); got != 0 {
		t.Fatalf("subdomain destination received %d requests, want 0", got)
	}
}

func TestStoreHTTPClientAllowsSameOriginPathRedirect(t *testing.T) {
	useSystemRoots(t)
	root := newTestCA(t, "same-origin-root")
	var finalCalls atomic.Int32
	srv := tlsServerWithHandler(t,
		leafFor(t, root, nil, []net.IP{net.ParseIP("127.0.0.1")}), false,
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/start":
				http.Redirect(w, r, "/final", http.StatusFound)
			case "/final":
				finalCalls.Add(1)
				if got := r.Header.Get("Authorization"); got != "Bearer same-origin" {
					t.Errorf("same-origin Authorization = %q", got)
				}
				w.WriteHeader(http.StatusNoContent)
			default:
				http.NotFound(w, r)
			}
		}))

	store, err := New(Options{CAPEM: string(root.pem)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	req, err := http.NewRequest(http.MethodGet, srv.URL+"/start", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer same-origin")
	resp, err := store.HTTPClient(2 * time.Second).Do(req)
	if err != nil {
		t.Fatalf("same-origin redirect: %v", err)
	}
	resp.Body.Close()
	if got := finalCalls.Load(); got != 1 {
		t.Fatalf("same-origin final calls = %d, want 1", got)
	}
}

func TestStoreHTTPClientRejectsMultiHopCrossOriginPivot(t *testing.T) {
	useSystemRoots(t)
	root := newTestCA(t, "multi-hop-root")
	cert := leafFor(t, root, nil, []net.IP{net.ParseIP("127.0.0.1")})
	var destinationCalls atomic.Int32
	destination := tlsServerWithHandler(t, cert, false, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		destinationCalls.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	redirector := tlsServerWithHandler(t, cert, false, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/start":
			http.Redirect(w, r, "/same-origin-hop", http.StatusFound)
		case "/same-origin-hop":
			http.Redirect(w, r, destination.URL, http.StatusFound)
		default:
			http.NotFound(w, r)
		}
	}))

	store, err := New(Options{CAPEM: string(root.pem)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	req, err := http.NewRequest(http.MethodGet, redirector.URL+"/start", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer no-multi-hop-pivot")
	resp, err := store.HTTPClient(2 * time.Second).Do(req)
	if resp != nil {
		resp.Body.Close()
	}
	if err == nil {
		t.Fatal("multi-hop cross-origin redirect was followed")
	}
	if got := destinationCalls.Load(); got != 0 {
		t.Fatalf("multi-hop destination received %d requests, want 0", got)
	}
}

func TestSameOriginCanonicalizesHostnameAndEffectivePort(t *testing.T) {
	parse := func(raw string) *url.URL {
		t.Helper()
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatalf("parse %q: %v", raw, err)
		}
		return u
	}
	for name, tc := range map[string]struct {
		first string
		next  string
		want  bool
	}{
		"dns case trailing dot and default port": {
			first: "https://Gateway.Example./start", next: "https://gateway.example:443/next", want: true,
		},
		"numeric port normalization": {
			first: "https://gateway.example:0443/start", next: "https://gateway.example:443/next", want: true,
		},
		"ipv6 canonical form": {
			first: "https://[0:0:0:0:0:0:0:1]/start", next: "https://[::1]:443/next", want: true,
		},
		"different effective port": {
			first: "https://gateway.example/start", next: "https://gateway.example:444/next", want: false,
		},
		"different scheme": {
			first: "https://gateway.example/start", next: "http://gateway.example:443/next", want: false,
		},
		"subdomain": {
			first: "https://gateway.example/start", next: "https://sub.gateway.example/next", want: false,
		},
	} {
		t.Run(name, func(t *testing.T) {
			if got := sameOrigin(parse(tc.first), parse(tc.next)); got != tc.want {
				t.Fatalf("sameOrigin(%q, %q) = %v, want %v", tc.first, tc.next, got, tc.want)
			}
		})
	}
}

func routeTestDNSNamesToLoopback(t *testing.T) {
	t.Helper()
	previousDefault := http.DefaultTransport
	base := previousDefault.(*http.Transport).Clone()
	dialer := &net.Dialer{}
	base.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		_, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		return dialer.DialContext(ctx, network, net.JoinHostPort("127.0.0.1", port))
	}
	http.DefaultTransport = base
	t.Cleanup(func() { http.DefaultTransport = previousDefault })
}

func testServerURLWithHostname(t *testing.T, srv *httptest.Server, hostname string) string {
	t.Helper()
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse test server URL: %v", err)
	}
	u.Host = net.JoinHostPort(hostname, u.Port())
	return u.String()
}

func TestStoreHTTPClientStopsAfterTenHTTPSRedirects(t *testing.T) {
	useSystemRoots(t)
	store, err := New(Options{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	client := store.HTTPClient(time.Second)
	redirect, err := http.NewRequest(http.MethodGet, "https://gateway.example/loop", nil)
	if err != nil {
		t.Fatalf("new redirect request: %v", err)
	}
	via := make([]*http.Request, 10)
	for i := range via {
		via[i], err = http.NewRequest(http.MethodGet, "https://gateway.example/loop", nil)
		if err != nil {
			t.Fatalf("new prior request: %v", err)
		}
	}
	if err := client.CheckRedirect(redirect, via); err == nil {
		t.Fatal("redirect policy accepted more than the standard ten redirects")
	}
}

func TestStoreHTTPClientUsesDynamicRootsThroughHTTPSProxy(t *testing.T) {
	useSystemRoots(t)
	proxy, connects := newCONNECTProxy(t)
	proxyURL, err := url.Parse(proxy.URL)
	if err != nil {
		t.Fatalf("parse proxy URL: %v", err)
	}
	previousDefault := http.DefaultTransport
	base := previousDefault.(*http.Transport).Clone()
	base.Proxy = http.ProxyURL(proxyURL)
	http.DefaultTransport = base
	t.Cleanup(func() { http.DefaultTransport = previousDefault })

	root1 := newTestCA(t, "proxy-root-one")
	root2 := newTestCA(t, "proxy-root-two")
	path := filepath.Join(t.TempDir(), "operator.pem")
	writePEM(t, path, root1.pem)
	store, err := New(Options{CAFile: path})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	client := store.HTTPClient(2 * time.Second)
	requireGET(t, client, tlsServer(t, leafFor(t, root1, nil, []net.IP{net.ParseIP("127.0.0.1")})).URL)
	writePEM(t, path, root2.pem)
	requireGET(t, client, tlsServer(t, leafFor(t, root2, nil, []net.IP{net.ParseIP("127.0.0.1")})).URL)
	if got := connects.Load(); got < 2 {
		t.Fatalf("CONNECT requests = %d, want at least 2", got)
	}
}

func newCONNECTProxy(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var connects atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			http.Error(w, "CONNECT required", http.StatusMethodNotAllowed)
			return
		}
		connects.Add(1)
		upstream, err := net.Dial("tcp", r.Host)
		if err != nil {
			http.Error(w, "upstream unavailable", http.StatusBadGateway)
			return
		}
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			upstream.Close()
			t.Error("proxy response writer cannot hijack")
			return
		}
		client, rw, err := hijacker.Hijack()
		if err != nil {
			upstream.Close()
			t.Errorf("proxy hijack: %v", err)
			return
		}
		if _, err := fmt.Fprint(rw, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
			client.Close()
			upstream.Close()
			return
		}
		if err := rw.Flush(); err != nil {
			client.Close()
			upstream.Close()
			return
		}
		go func() {
			_, _ = io.Copy(upstream, client)
			_ = upstream.Close()
		}()
		_, _ = io.Copy(client, upstream)
		_ = client.Close()
	}))
	t.Cleanup(proxy.Close)
	return proxy, &connects
}

func TestStoreHTTPClientRequiresTLS12(t *testing.T) {
	useSystemRoots(t)
	root := newTestCA(t, "tls-version-root")
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	srv.TLS = &tls.Config{
		Certificates: []tls.Certificate{leafFor(t, root, nil, []net.IP{net.ParseIP("127.0.0.1")})},
		MaxVersion:   tls.VersionTLS11,
	}
	srv.StartTLS()
	t.Cleanup(srv.Close)

	store, err := New(Options{CAPEM: string(root.pem)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if resp, err := store.HTTPClient(2 * time.Second).Get(srv.URL); err == nil {
		resp.Body.Close()
		t.Fatal("HTTP client negotiated obsolete TLS below 1.2")
	}
}

func TestStoreTLSInsecureOnlyWhenExplicit(t *testing.T) {
	useSystemRoots(t)
	unknownRoot := newTestCA(t, "unknown")
	srv := tlsServer(t, leafFor(t, unknownRoot, nil, []net.IP{net.ParseIP("127.0.0.1")}))

	secure, err := New(Options{})
	if err != nil {
		t.Fatalf("secure New: %v", err)
	}
	if _, err := secure.HTTPClient(2 * time.Second).Get(srv.URL); err == nil {
		t.Fatal("secure store accepted an untrusted root")
	}

	insecure, err := New(Options{TLSInsecure: true})
	if err != nil {
		t.Fatalf("insecure New: %v", err)
	}
	requireGET(t, insecure.HTTPClient(2*time.Second), srv.URL)
}
