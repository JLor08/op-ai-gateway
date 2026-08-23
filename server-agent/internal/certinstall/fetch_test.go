// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package certinstall

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

// hookMarker returns a reload command that creates an empty file at path when
// run (used to prove/disprove that the hook actually executed), and the path
// itself.
func hookMarker(dir string) (command, path string) {
	path = filepath.Join(dir, "hook-ran")
	return "touch " + path, path
}

func assertHookRan(t *testing.T, marker string, want bool) {
	t.Helper()
	_, err := os.Stat(marker)
	ran := err == nil
	if ran != want {
		t.Errorf("hook ran = %v, want %v (stat err = %v)", ran, want, err)
	}
}

func writeJSONResponse(w http.ResponseWriter, etag string, body certResponse) {
	if etag != "" {
		w.Header().Set("ETag", etag)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(body)
}

func TestModeOffNeverTouchesTheNetwork(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("unexpected request to %s in ModeOff", r.URL.Path)
	}))
	defer srv.Close()

	in := New(srv.URL, "tok", nil, t.TempDir(), "", ModeOff)
	report, changed, err := in.Sync(context.Background())
	if err != nil {
		t.Fatalf("Sync in ModeOff: %v", err)
	}
	if changed {
		t.Errorf("changed = true in ModeOff")
	}
	if report.Mode != ModeOff || report.Fingerprint != "" {
		t.Errorf("report = %+v, want bare ModeOff", report)
	}
}

func TestNewJoinsPathByConcatenationPreservingBasePath(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	// A trailing slash on the base AND an existing path segment: the request
	// path must be exactly "/base/api/agent/v1/certificate" -- no doubled
	// slash, and the "/base" segment preserved (never dropped as a path-join
	// helper might).
	in := New(srv.URL+"/base/", "tok", nil, t.TempDir(), "", "files")
	if _, _, err := in.Sync(context.Background()); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	want := "/base/api/agent/v1/certificate"
	if gotPath != want {
		t.Errorf("request path = %q, want %q", gotPath, want)
	}
}

func TestNewUsesInjectedHTTPClient(t *testing.T) {
	injected := &http.Client{Timeout: 17 * time.Second}
	in := New("https://gw.example", "tok", injected, t.TempDir(), "", "files")
	if in.http != injected {
		t.Fatal("New did not retain the injected HTTP client")
	}
}

func TestNewNilHTTPClientUsesThirtySecondTimeout(t *testing.T) {
	in := New("https://gw.example", "tok", nil, t.TempDir(), "", "files")
	if got := in.http.Timeout; got != 30*time.Second {
		t.Fatalf("default certificate HTTP timeout = %v, want 30s", got)
	}
}

func TestSyncUsesInjectedHTTPClientForTLS(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if got := r.Header.Get("Authorization"); got != "Bearer tls-token" {
			t.Errorf("Authorization = %q, want TLS bearer token", got)
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	in := New(srv.URL, "tls-token", srv.Client(), t.TempDir(), "", "files")
	if _, _, err := in.Sync(context.Background()); err != nil {
		t.Fatalf("Sync over injected TLS client: %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("TLS calls = %d, want 1", got)
	}
}

func TestSyncUnconditionalFetchInstallsFreshCertificateAndSendsBearer(t *testing.T) {
	dir := t.TempDir()
	leafPEM, keyPEM, der := generateTestLeaf(t, "fresh.example.test", time.Now().Add(90*24*time.Hour))
	caPEM := generateTestCA(t, "root")

	var gotAuth, gotIfNoneMatch string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotIfNoneMatch = r.Header.Get("If-None-Match")
		writeJSONResponse(w, `"e-1"`, certResponse{
			Fingerprint:  fingerprintDER(der),
			FullchainPEM: leafPEM,
			KeyPEM:       keyPEM,
			CABundlePEM:  caPEM,
		})
	}))
	defer srv.Close()

	in := New(srv.URL, "s3cr3t-token", nil, dir, "", "files")
	report, changed, err := in.Sync(context.Background())
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if !changed {
		t.Errorf("expected changed = true for a fresh install")
	}
	if gotAuth != "Bearer s3cr3t-token" {
		t.Errorf("Authorization header = %q", gotAuth)
	}
	if gotIfNoneMatch != "" {
		t.Errorf("If-None-Match must be empty on a fresh (memo-invalid) fetch, got %q", gotIfNoneMatch)
	}
	if report.Fingerprint != fingerprintDER(der) {
		t.Errorf("report.Fingerprint = %q, want %q", report.Fingerprint, fingerprintDER(der))
	}
	if got, _ := os.ReadFile(filepath.Join(dir, fullchainFile)); string(got) != leafPEM {
		t.Errorf("fullchain.pem not installed correctly")
	}
	// The etag sidecar must hold the response's ETag header VERBATIM (not
	// reparsed/requoted), so the NEXT sync can replay it as If-None-Match.
	gotEtag, err := os.ReadFile(filepath.Join(dir, etagFile))
	if err != nil || string(gotEtag) != `"e-1"` {
		t.Errorf("etag sidecar = %q, %v; want the response header verbatim: %q", gotEtag, err, `"e-1"`)
	}
}

func TestSync304NoWriteNoHook(t *testing.T) {
	dir := t.TempDir()
	leafPEM, keyPEM, _ := generateTestLeaf(t, "stable.example.test", time.Now().Add(90*24*time.Hour))
	os.WriteFile(filepath.Join(dir, fullchainFile), []byte(leafPEM), 0o644)
	os.WriteFile(filepath.Join(dir, privkeyFile), []byte(keyPEM), 0o600)
	os.WriteFile(filepath.Join(dir, etagFile), []byte(`"stable-etag"`), 0o644)

	before, err := os.Stat(filepath.Join(dir, fullchainFile))
	if err != nil {
		t.Fatalf("stat before: %v", err)
	}

	var sawIfNoneMatch string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawIfNoneMatch = r.Header.Get("If-None-Match")
		w.WriteHeader(http.StatusNotModified)
	}))
	defer srv.Close()

	command, marker := hookMarker(dir)
	in := New(srv.URL, "tok", nil, dir, command, "files")
	_, changed, err := in.Sync(context.Background())
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if changed {
		t.Errorf("changed = true on a 304")
	}
	if sawIfNoneMatch != `"stable-etag"` {
		t.Errorf("If-None-Match = %q, want the stored etag verbatim", sawIfNoneMatch)
	}
	after, err := os.Stat(filepath.Join(dir, fullchainFile))
	if err != nil {
		t.Fatalf("stat after: %v", err)
	}
	if !before.ModTime().Equal(after.ModTime()) {
		t.Errorf("fullchain.pem mtime changed on a 304")
	}
	assertHookRan(t, marker, false)
}

// TestSync200WithNoActualChangeDoesNotInstallOrHook covers the memo-invalid-but-
// content-coincidentally-identical case: an unconditional 200 fetch (no stored
// etag, so no If-None-Match was sent) whose fingerprint AND bundle bytes
// happen to exactly match what is already on disk. This must be treated
// exactly like a 304 -- no rewrite, no hook -- even though it arrived as a
// 200.
func TestSync200WithNoActualChangeDoesNotInstallOrHook(t *testing.T) {
	dir := t.TempDir()
	leafPEM, keyPEM, der := generateTestLeaf(t, "unconditional-nochange.example.test", time.Now().Add(90*24*time.Hour))
	os.WriteFile(filepath.Join(dir, fullchainFile), []byte(leafPEM), 0o644)
	os.WriteFile(filepath.Join(dir, privkeyFile), []byte(keyPEM), 0o600)
	// Deliberately NO etag sidecar -- memoValid() is false, so Sync fetches
	// unconditionally (an explicit If-None-Match must not appear).

	before, err := os.Stat(filepath.Join(dir, fullchainFile))
	if err != nil {
		t.Fatalf("stat before: %v", err)
	}

	var sawIfNoneMatch string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawIfNoneMatch = r.Header.Get("If-None-Match")
		writeJSONResponse(w, `"fresh-etag"`, certResponse{
			Fingerprint:  fingerprintDER(der),
			FullchainPEM: leafPEM,
			KeyPEM:       keyPEM,
		})
	}))
	defer srv.Close()

	command, marker := hookMarker(dir)
	in := New(srv.URL, "tok", nil, dir, command, "files")
	_, changed, err := in.Sync(context.Background())
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if sawIfNoneMatch != "" {
		t.Errorf("If-None-Match must be empty on a memo-invalid fetch, got %q", sawIfNoneMatch)
	}
	if changed {
		t.Errorf("changed = true despite identical fingerprint and bundle")
	}
	after, err := os.Stat(filepath.Join(dir, fullchainFile))
	if err != nil {
		t.Fatalf("stat after: %v", err)
	}
	if !before.ModTime().Equal(after.ModTime()) {
		t.Errorf("fullchain.pem was rewritten despite no actual content change")
	}
	assertHookRan(t, marker, false)
	// The freshly returned etag must still be persisted, so the NEXT sync can
	// go conditional.
	got, err := os.ReadFile(filepath.Join(dir, etagFile))
	if err != nil || string(got) != `"fresh-etag"` {
		t.Errorf("etag sidecar = %q, %v; want the fresh etag persisted", got, err)
	}
}

// The DISCRIMINATING case: the server answers with EXACTLY the leaf already on
// disk. A "changed" predicate that only compares fingerprints then finds no
// difference, skips the install, and leaves the mismatched key in place forever.
// The earlier version of this guard served a THIRD certificate, so it passed with
// or without the defect -- it proved only that a different certificate installs.
func TestSyncMismatchedPairReinstallsEvenWhenTheLeafIsUnchanged(t *testing.T) {
	dir := t.TempDir()
	leafNPlus1, keyNPlus1, derNPlus1 := generateTestLeaf(t, "chain-n-plus-1.example.test", time.Now().Add(90*24*time.Hour))
	_, keyN, _ := generateTestLeaf(t, "chain-n.example.test", time.Now().Add(90*24*time.Hour))
	// The half-completed rename: the NEW chain beside the OLD key.
	os.WriteFile(filepath.Join(dir, fullchainFile), []byte(leafNPlus1), 0o644)
	os.WriteFile(filepath.Join(dir, privkeyFile), []byte(keyN), 0o600)
	os.WriteFile(filepath.Join(dir, etagFile), []byte(`"stale-etag"`), 0o644)

	if readDiskState(dir).keyPaired {
		t.Fatal("precondition: the on-disk pair must be mismatched")
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSONResponse(w, `"same-leaf-etag"`, certResponse{
			Fingerprint:  fingerprintDER(derNPlus1),
			FullchainPEM: leafNPlus1,
			KeyPEM:       keyNPlus1,
		})
	}))
	defer srv.Close()

	in := New(srv.URL, "tok", nil, dir, "", "files")
	_, changed, err := in.Sync(context.Background())
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if !changed {
		t.Fatal("a mismatched pair must reinstall even when the leaf is byte-identical -- otherwise the wedge is permanent")
	}
	if st := readDiskState(dir); !st.keyPaired {
		t.Fatal("the pair is still mismatched after Sync: the half-completed rename never healed")
	}
}

func TestSyncMismatchedPairForcesUnconditionalFetchAndReinstalls(t *testing.T) {
	dir := t.TempDir()
	// fullchain.pem is from a LATER install (N+1); privkey.pem is still from an
	// EARLIER install (N) -- a half-completed rename, exactly the scenario the
	// pairing check exists to catch.
	leafNPlus1, _, _ := generateTestLeaf(t, "chain-n-plus-1.example.test", time.Now().Add(90*24*time.Hour))
	_, keyN, _ := generateTestLeaf(t, "chain-n.example.test", time.Now().Add(90*24*time.Hour))
	os.WriteFile(filepath.Join(dir, fullchainFile), []byte(leafNPlus1), 0o644)
	os.WriteFile(filepath.Join(dir, privkeyFile), []byte(keyN), 0o600)
	os.WriteFile(filepath.Join(dir, etagFile), []byte(`"stale-etag"`), 0o644)

	newLeafPEM, newKeyPEM, newDER := generateTestLeaf(t, "healed.example.test", time.Now().Add(90*24*time.Hour))
	var sawIfNoneMatch string
	var ifNoneMatchWasSet bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawIfNoneMatch = r.Header.Get("If-None-Match")
		ifNoneMatchWasSet = sawIfNoneMatch != ""
		writeJSONResponse(w, `"healed-etag"`, certResponse{
			Fingerprint:  fingerprintDER(newDER),
			FullchainPEM: newLeafPEM,
			KeyPEM:       newKeyPEM,
		})
	}))
	defer srv.Close()

	in := New(srv.URL, "tok", nil, dir, "", "files")
	_, changed, err := in.Sync(context.Background())
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if ifNoneMatchWasSet {
		t.Errorf("If-None-Match must NOT be sent when the on-disk pair is mismatched, got %q", sawIfNoneMatch)
	}
	if !changed {
		t.Errorf("expected a full reinstall (changed = true), not a 304")
	}
	got, _ := os.ReadFile(filepath.Join(dir, fullchainFile))
	if string(got) != newLeafPEM {
		t.Errorf("fullchain.pem not healed to the new install")
	}
	gotKey, _ := os.ReadFile(filepath.Join(dir, privkeyFile))
	if string(gotKey) != newKeyPEM {
		t.Errorf("privkey.pem not healed to the new install")
	}
}

func testSyncLeavesUntouchedOnStatus(t *testing.T, status int) {
	dir := t.TempDir()
	leafPEM, keyPEM, _ := generateTestLeaf(t, fmt.Sprintf("status-%d.example.test", status), time.Now().Add(90*24*time.Hour))
	os.WriteFile(filepath.Join(dir, fullchainFile), []byte(leafPEM), 0o644)
	os.WriteFile(filepath.Join(dir, privkeyFile), []byte(keyPEM), 0o600)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
	}))
	defer srv.Close()

	command, marker := hookMarker(dir)
	in := New(srv.URL, "tok", nil, dir, command, "files")
	_, changed, err := in.Sync(context.Background())
	if err != nil {
		t.Fatalf("Sync (status %d): %v", status, err)
	}
	if changed {
		t.Errorf("changed = true for status %d", status)
	}
	got, _ := os.ReadFile(filepath.Join(dir, fullchainFile))
	if string(got) != leafPEM {
		t.Errorf("fullchain.pem modified for status %d", status)
	}
	gotKey, _ := os.ReadFile(filepath.Join(dir, privkeyFile))
	if string(gotKey) != keyPEM {
		t.Errorf("privkey.pem modified for status %d", status)
	}
	assertHookRan(t, marker, false)
}

func TestSync404LeavesFilesUntouched(t *testing.T) {
	testSyncLeavesUntouchedOnStatus(t, http.StatusNotFound)
}

func TestSync401LeavesFilesUntouched(t *testing.T) {
	testSyncLeavesUntouchedOnStatus(t, http.StatusUnauthorized)
}

func TestSyncUnauthorizedLogsAtWarnLevel(t *testing.T) {
	h := withCapturedLogs(t)
	dir := t.TempDir()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	in := New(srv.URL, "tok", nil, dir, "", "files")
	if _, _, err := in.Sync(context.Background()); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if !h.hasRecord(slog.LevelWarn, "certificate fetch unauthorized", "status") {
		t.Errorf("expected a Warn-level record for a 401 response, got %+v", h.all())
	}
}

func TestSync403LeavesFilesUntouched(t *testing.T) {
	testSyncLeavesUntouchedOnStatus(t, http.StatusForbidden)
}

func TestSync500LeavesFilesUntouched(t *testing.T) {
	testSyncLeavesUntouchedOnStatus(t, http.StatusInternalServerError)
}

func TestSyncBundleOnlyChangeInstallsAndHooks(t *testing.T) {
	dir := t.TempDir()
	leafPEM, keyPEM, der := generateTestLeaf(t, "bundle-only.example.test", time.Now().Add(90*24*time.Hour))
	oldCA := generateTestCA(t, "old-root")
	os.WriteFile(filepath.Join(dir, fullchainFile), []byte(leafPEM), 0o644)
	os.WriteFile(filepath.Join(dir, privkeyFile), []byte(keyPEM), 0o600)
	os.WriteFile(filepath.Join(dir, caFile), []byte(oldCA), 0o644)
	os.WriteFile(filepath.Join(dir, etagFile), []byte(`"same-leaf-old-bundle"`), 0o644)

	newCA := generateTestCA(t, "new-root")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// SAME leaf fingerprint, DIFFERENT bundle: the composite ETag from the
		// gateway would differ (that's why this arrives as a 200, not a 304),
		// but the fake server here just always returns 200 -- the point of this
		// test is the CLIENT's own changed-detection, not the ETag mechanics.
		writeJSONResponse(w, `"new-bundle-etag"`, certResponse{
			Fingerprint:  fingerprintDER(der),
			FullchainPEM: leafPEM,
			KeyPEM:       keyPEM,
			CABundlePEM:  newCA,
		})
	}))
	defer srv.Close()

	command, marker := hookMarker(dir)
	in := New(srv.URL, "tok", nil, dir, command, "files")
	_, changed, err := in.Sync(context.Background())
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if !changed {
		t.Errorf("expected changed = true for a bundle-only update")
	}
	got, _ := os.ReadFile(filepath.Join(dir, caFile))
	if string(got) != newCA {
		t.Errorf("ca.pem not updated to the new bundle")
	}
	assertHookRan(t, marker, true)
}

func TestSyncEmptyBundleOnFreshInstallCreatesNoCAFile(t *testing.T) {
	dir := t.TempDir()
	leafPEM, keyPEM, der := generateTestLeaf(t, "no-bundle.example.test", time.Now().Add(90*24*time.Hour))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSONResponse(w, `"e"`, certResponse{
			Fingerprint:  fingerprintDER(der),
			FullchainPEM: leafPEM,
			KeyPEM:       keyPEM,
			CABundlePEM:  "",
		})
	}))
	defer srv.Close()

	in := New(srv.URL, "tok", nil, dir, "", "files")
	if _, _, err := in.Sync(context.Background()); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, caFile)); !os.IsNotExist(err) {
		t.Errorf("ca.pem should not exist, stat err = %v", err)
	}
}

func TestSyncEmptyBundleDoesNotDeleteExistingCA(t *testing.T) {
	dir := t.TempDir()
	oldLeafPEM, oldKeyPEM, _ := generateTestLeaf(t, "old.example.test", time.Now().Add(90*24*time.Hour))
	oldCA := generateTestCA(t, "kept-root")
	os.WriteFile(filepath.Join(dir, fullchainFile), []byte(oldLeafPEM), 0o644)
	os.WriteFile(filepath.Join(dir, privkeyFile), []byte(oldKeyPEM), 0o600)
	os.WriteFile(filepath.Join(dir, caFile), []byte(oldCA), 0o644)

	newLeafPEM, newKeyPEM, newDER := generateTestLeaf(t, "changed.example.test", time.Now().Add(90*24*time.Hour))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A DIFFERENT leaf (forces changed=true overall) but an empty bundle.
		writeJSONResponse(w, `"e2"`, certResponse{
			Fingerprint:  fingerprintDER(newDER),
			FullchainPEM: newLeafPEM,
			KeyPEM:       newKeyPEM,
			CABundlePEM:  "",
		})
	}))
	defer srv.Close()

	in := New(srv.URL, "tok", nil, dir, "", "files")
	_, changed, err := in.Sync(context.Background())
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if !changed {
		t.Errorf("expected changed = true (the leaf changed)")
	}
	got, _ := os.ReadFile(filepath.Join(dir, caFile))
	if string(got) != oldCA {
		t.Errorf("ca.pem was touched despite an empty bundle in the response: got %q, want unchanged %q", got, oldCA)
	}
}

func TestSyncHookFailureLeavesFilesInstalledAndReportsNewFingerprint(t *testing.T) {
	h := withCapturedLogs(t)
	dir := t.TempDir()
	leafPEM, keyPEM, der := generateTestLeaf(t, "hook-fails.example.test", time.Now().Add(90*24*time.Hour))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSONResponse(w, `"e"`, certResponse{
			Fingerprint:  fingerprintDER(der),
			FullchainPEM: leafPEM,
			KeyPEM:       keyPEM,
		})
	}))
	defer srv.Close()

	in := New(srv.URL, "tok", nil, dir, "exit 3", "files")
	report, changed, err := in.Sync(context.Background())
	if err != nil {
		t.Fatalf("Sync must not fail just because the reload hook failed: %v", err)
	}
	if !changed {
		t.Errorf("changed = false, want true (install itself succeeded)")
	}
	if report.Fingerprint != fingerprintDER(der) {
		t.Errorf("report.Fingerprint = %q, want the new fingerprint %q", report.Fingerprint, fingerprintDER(der))
	}
	got, _ := os.ReadFile(filepath.Join(dir, fullchainFile))
	if string(got) != leafPEM {
		t.Errorf("fullchain.pem not installed despite the hook failing")
	}
	if !h.hasRecord(slog.LevelWarn, "certificate reload hook failed", "exit_code") {
		t.Errorf("expected a warn log for the failed hook, got %+v", h.all())
	}
}

func TestSyncReportIsConsistentAndDiskDerivedAcrossResponsePaths(t *testing.T) {
	dir := t.TempDir()
	leafPEM, keyPEM, der := generateTestLeaf(t, "consistent.example.test", time.Now().Add(90*24*time.Hour))

	var status atomic.Int32
	status.Store(int32(http.StatusOK))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s := int(status.Load())
		if s == http.StatusOK {
			writeJSONResponse(w, `"e"`, certResponse{
				Fingerprint:  fingerprintDER(der),
				FullchainPEM: leafPEM,
				KeyPEM:       keyPEM,
			})
			return
		}
		w.WriteHeader(s)
	}))
	defer srv.Close()

	in := New(srv.URL, "tok", nil, dir, "", "files")

	for _, s := range []int{http.StatusOK, http.StatusNotModified, http.StatusNotFound, http.StatusInternalServerError} {
		status.Store(int32(s))
		report, _, err := in.Sync(context.Background())
		if err != nil {
			t.Fatalf("Sync (status %d): %v", s, err)
		}
		direct := in.Report()
		if report.Fingerprint != direct.Fingerprint || !report.NotAfter.Equal(direct.NotAfter) ||
			report.Mode != direct.Mode || len(report.CAFingerprints) != len(direct.CAFingerprints) {
			t.Errorf("status %d: Sync's returned report %+v != in.Report() %+v", s, report, direct)
		}
		if report.Fingerprint != fingerprintDER(der) {
			t.Errorf("status %d: report.Fingerprint = %q, want the installed leaf's fingerprint %q (report must reflect disk, not the wire response)", s, report.Fingerprint, fingerprintDER(der))
		}
	}
}

func TestSyncInstallFailureReturnsErrorAndLeavesOldCertificateInPlace(t *testing.T) {
	dir := t.TempDir()
	oldLeafPEM, oldKeyPEM, oldDER := generateTestLeaf(t, "old-good.example.test", time.Now().Add(90*24*time.Hour))
	os.WriteFile(filepath.Join(dir, fullchainFile), []byte(oldLeafPEM), 0o644)
	os.WriteFile(filepath.Join(dir, privkeyFile), []byte(oldKeyPEM), 0o600)

	newLeafPEM, newKeyPEM, newDER := generateTestLeaf(t, "new-but-fails-to-write.example.test", time.Now().Add(90*24*time.Hour))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSONResponse(w, `"e"`, certResponse{
			Fingerprint:  fingerprintDER(newDER),
			FullchainPEM: newLeafPEM,
			KeyPEM:       newKeyPEM,
		})
	}))
	defer srv.Close()

	origWriteTemp := writeTempFile
	defer func() { writeTempFile = origWriteTemp }()
	writeTempFile = func(d, finalName string, content []byte, mode os.FileMode) (string, error) {
		if finalName == privkeyFile {
			return "", fmt.Errorf("injected disk failure")
		}
		return origWriteTemp(d, finalName, content, mode)
	}

	in := New(srv.URL, "tok", nil, dir, "", "files")
	report, changed, err := in.Sync(context.Background())
	if err == nil {
		t.Fatalf("expected Sync to surface the install failure as an error")
	}
	if changed {
		t.Errorf("changed = true despite a failed install")
	}
	if report.Fingerprint != fingerprintDER(oldDER) {
		t.Errorf("report.Fingerprint = %q, want the OLD (still-installed) fingerprint %q", report.Fingerprint, fingerprintDER(oldDER))
	}
	got, _ := os.ReadFile(filepath.Join(dir, fullchainFile))
	if string(got) != oldLeafPEM {
		t.Errorf("fullchain.pem was modified despite a failed install")
	}
	if leftovers := listTempLeftovers(t, dir); len(leftovers) != 0 {
		t.Errorf("leftover temp files after a failed install: %v", leftovers)
	}
}

// install() deliberately neither writes nor deletes ca.pem for an empty bundle, so
// comparing the bundle unconditionally would report "changed" on every poll: a
// permanent reinstall loop that re-runs the operator's reload command forever.
// Same leaf, same key, previously-written ca.pem, response without a bundle.
func TestSyncEmptyBundleDoesNotLoopAgainstAnExistingCAFile(t *testing.T) {
	dir := t.TempDir()
	leafPEM, keyPEM, der := generateTestLeaf(t, "steady.example.test", time.Now().Add(90*24*time.Hour))
	caPEM, _, _ := generateTestLeaf(t, "root.example.test", time.Now().Add(3650*24*time.Hour))

	hooks := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// First response carries the bundle, later ones do not.
		bundle := caPEM
		if hooks > 0 {
			bundle = ""
		}
		writeJSONResponse(w, `"etag-`+strconv.Itoa(hooks)+`"`, certResponse{
			Fingerprint: fingerprintDER(der), FullchainPEM: leafPEM, KeyPEM: keyPEM, CABundlePEM: bundle,
		})
	}))
	defer srv.Close()

	in := New(srv.URL, "tok", nil, dir, "", "files")
	if _, changed, err := in.Sync(context.Background()); err != nil || !changed {
		t.Fatalf("first sync: changed=%v err=%v, want a fresh install", changed, err)
	}
	if _, err := os.Stat(filepath.Join(dir, caFile)); err != nil {
		t.Fatalf("precondition: ca.pem must exist after the first install: %v", err)
	}
	hooks = 1 // from here on the gateway answers without a bundle

	for i := 0; i < 3; i++ {
		_, changed, err := in.Sync(context.Background())
		if err != nil {
			t.Fatalf("sync %d: %v", i, err)
		}
		if changed {
			t.Fatalf("sync %d reported a change although only the (unwritten) empty bundle differs -- "+
				"this is the reinstall loop that re-runs the reload hook forever", i)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, caFile)); err != nil {
		t.Fatalf("an empty bundle must never delete an existing ca.pem: %v", err)
	}
}

// A response whose chain and key do not belong together must NOT be installed, and
// must not be retried into a loop. Installing it would hand the TLS consumer a
// certificate it cannot serve; and since an unpaired disk state counts as a change,
// every later poll would reinstall it and re-run the operator's reload command
// forever -- the same unbounded loop the empty-bundle clause avoids.
func TestSyncRefusesAResponseWhoseChainAndKeyDoNotPair(t *testing.T) {
	dir := t.TempDir()
	leafPEM, _, der := generateTestLeaf(t, "served.example.test", time.Now().Add(90*24*time.Hour))
	_, foreignKey, _ := generateTestLeaf(t, "other.example.test", time.Now().Add(90*24*time.Hour))

	hookMarker := filepath.Join(dir, "hook-ran")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSONResponse(w, `"mismatched-etag"`, certResponse{
			Fingerprint: fingerprintDER(der), FullchainPEM: leafPEM, KeyPEM: foreignKey,
		})
	}))
	defer srv.Close()

	in := New(srv.URL, "tok", nil, dir, "touch "+hookMarker, "files")
	for i := 0; i < 3; i++ {
		_, changed, err := in.Sync(context.Background())
		if err != nil {
			t.Fatalf("sync %d: %v", i, err)
		}
		if changed {
			t.Fatalf("sync %d installed a chain/key pair that does not match", i)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, fullchainFile)); err == nil {
		t.Fatal("a mismatched response must not be written to disk")
	}
	if _, err := os.Stat(hookMarker); err == nil {
		t.Fatal("the reload hook ran for a mismatched pair -- this is the unbounded loop")
	}
}
