// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package certinstall

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// listTempLeftovers returns every ".op-cert-*" entry in dir that is NOT the
// final etag sidecar itself (whose real name also happens to start with the
// same prefix) -- i.e. exactly the temp-file naming pattern os.CreateTemp
// produces, so a test can assert none were left behind.
func listTempLeftovers(t testing.TB, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("read dir: %v", err)
	}
	var out []string
	for _, e := range entries {
		if e.Name() == etagFile {
			continue
		}
		if strings.HasPrefix(e.Name(), ".op-cert-") {
			out = append(out, e.Name())
		}
	}
	return out
}

func TestAtomicWriterStageCommitSetsExactModesAndOrder(t *testing.T) {
	dir := t.TempDir()
	aw := &atomicWriter{dir: dir}
	if err := aw.stage(fullchainFile, []byte("fullchain"), 0o644); err != nil {
		t.Fatalf("stage fullchain: %v", err)
	}
	if err := aw.stage(certFile, []byte("cert"), 0o644); err != nil {
		t.Fatalf("stage cert: %v", err)
	}
	if err := aw.stage(chainFile, []byte("chain"), 0o644); err != nil {
		t.Fatalf("stage chain: %v", err)
	}
	if err := aw.stage(caFile, []byte("ca"), 0o644); err != nil {
		t.Fatalf("stage ca: %v", err)
	}
	if err := aw.stage(privkeyFile, []byte("key"), 0o600); err != nil {
		t.Fatalf("stage privkey: %v", err)
	}
	if err := aw.commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	cases := []struct {
		name string
		mode os.FileMode
	}{
		{fullchainFile, 0o644},
		{certFile, 0o644},
		{chainFile, 0o644},
		{caFile, 0o644},
		{privkeyFile, 0o600},
	}
	for _, c := range cases {
		info, err := os.Stat(filepath.Join(dir, c.name))
		if err != nil {
			t.Fatalf("stat %s: %v", c.name, err)
		}
		if info.Mode().Perm() != c.mode {
			t.Errorf("%s: mode = %o, want %o", c.name, info.Mode().Perm(), c.mode)
		}
	}
	if got := listTempLeftovers(t, dir); len(got) != 0 {
		t.Errorf("leftover temp files after commit: %v", got)
	}
}

func TestAtomicWriterAbortsOnStageFailureLeavesNoTempFilesAndPreviousUntouched(t *testing.T) {
	dir := t.TempDir()
	// Seed pre-existing content that must survive a failed batch untouched.
	preexisting := map[string]string{
		fullchainFile: "old-fullchain",
		certFile:      "old-cert",
		privkeyFile:   "old-key",
	}
	for name, content := range preexisting {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
	}

	origWriteTemp := writeTempFile
	defer func() { writeTempFile = origWriteTemp }()
	writeTempFile = func(d, finalName string, content []byte, mode os.FileMode) (string, error) {
		if finalName == privkeyFile {
			return "", errors.New("injected write failure")
		}
		return origWriteTemp(d, finalName, content, mode)
	}

	aw := &atomicWriter{dir: dir}
	if err := aw.stage(fullchainFile, []byte("new-fullchain"), 0o644); err != nil {
		t.Fatalf("stage fullchain (should succeed): %v", err)
	}
	if err := aw.stage(certFile, []byte("new-cert"), 0o644); err != nil {
		t.Fatalf("stage cert (should succeed): %v", err)
	}
	err := aw.stage(privkeyFile, []byte("new-key"), 0o600)
	if err == nil {
		t.Fatalf("expected privkey stage to fail")
	}

	if got := listTempLeftovers(t, dir); len(got) != 0 {
		t.Errorf("leftover temp files after aborted batch: %v", got)
	}
	for name, want := range preexisting {
		got, rerr := os.ReadFile(filepath.Join(dir, name))
		if rerr != nil {
			t.Fatalf("read %s after abort: %v", name, rerr)
		}
		if string(got) != want {
			t.Errorf("%s changed after aborted batch: got %q, want %q", name, got, want)
		}
	}
}

func TestWriteTempFileIgnoresTMPDIR(t *testing.T) {
	dir := t.TempDir()
	bogus := filepath.Join(dir, "does-not-exist-as-a-directory")
	t.Setenv("TMPDIR", bogus)

	tmpPath, err := writeTempFile(dir, fullchainFile, []byte("hello"), 0o644)
	if err != nil {
		t.Fatalf("writeTempFile with poisoned TMPDIR: %v", err)
	}
	defer os.Remove(tmpPath)

	if got := filepath.Dir(tmpPath); got != dir {
		t.Errorf("temp file landed in %q, want %q (TMPDIR must be ignored)", got, dir)
	}
	if _, err := os.Stat(bogus); err == nil {
		t.Errorf("bogus TMPDIR path unexpectedly exists")
	}
}

func TestAtomicWriterCommitPartwayFailureCleansOnlyRemaining(t *testing.T) {
	dir := t.TempDir()
	aw := &atomicWriter{dir: dir}
	if err := aw.stage(fullchainFile, []byte("fullchain"), 0o644); err != nil {
		t.Fatalf("stage fullchain: %v", err)
	}
	// Make privkey.pem's FINAL path a directory, so its rename fails with EISDIR
	// (or the platform equivalent) while fullchain's rename has already
	// succeeded.
	if err := os.MkdirAll(filepath.Join(dir, privkeyFile), 0o755); err != nil {
		t.Fatalf("seed conflicting directory: %v", err)
	}
	if err := aw.stage(privkeyFile, []byte("key"), 0o600); err != nil {
		t.Fatalf("stage privkey: %v", err)
	}

	err := aw.commit()
	if err == nil {
		t.Fatalf("expected commit to fail on the privkey rename")
	}

	// fullchain.pem, staged and renamed FIRST, must be the new final file.
	got, rerr := os.ReadFile(filepath.Join(dir, fullchainFile))
	if rerr != nil {
		t.Fatalf("read fullchain after partial commit: %v", rerr)
	}
	if string(got) != "fullchain" {
		t.Errorf("fullchain.pem = %q, want %q", got, "fullchain")
	}
	if got := listTempLeftovers(t, dir); len(got) != 0 {
		t.Errorf("leftover temp files after partial commit failure: %v", got)
	}
}

func TestSaveETagSidecarAtomicReadableAndNoLeftovers(t *testing.T) {
	dir := t.TempDir()
	saveETagSidecar(dir, `"abc123-def456"`)

	info, err := os.Stat(filepath.Join(dir, etagFile))
	if err != nil {
		t.Fatalf("stat etag sidecar: %v", err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Errorf("etag sidecar mode = %o, want 0644", info.Mode().Perm())
	}
	got, err := os.ReadFile(filepath.Join(dir, etagFile))
	if err != nil {
		t.Fatalf("read etag sidecar: %v", err)
	}
	if string(got) != `"abc123-def456"` {
		t.Errorf("etag sidecar content = %q", got)
	}
	if leftovers := listTempLeftovers(t, dir); len(leftovers) != 0 {
		t.Errorf("leftover temp files: %v", leftovers)
	}

	// Overwrite: must replace, not append or leave a second file.
	saveETagSidecar(dir, `"newer-etag"`)
	got, err = os.ReadFile(filepath.Join(dir, etagFile))
	if err != nil {
		t.Fatalf("read etag sidecar after overwrite: %v", err)
	}
	if string(got) != `"newer-etag"` {
		t.Errorf("etag sidecar after overwrite = %q", got)
	}
}

func TestSaveETagSidecarEmptyValueIsNoop(t *testing.T) {
	dir := t.TempDir()
	saveETagSidecar(dir, "")
	if _, err := os.Stat(filepath.Join(dir, etagFile)); !os.IsNotExist(err) {
		t.Errorf("etag sidecar should not exist for an empty value, stat err = %v", err)
	}
}

func TestRunReloadHookEmptyCommandIsNoop(t *testing.T) {
	h := withCapturedLogs(t)
	runReloadHook(context.Background(), "   ")
	if len(h.all()) != 0 {
		t.Errorf("expected no log activity for an empty reload command, got %d records", len(h.all()))
	}
}

func TestRunReloadHookSuccessLogsDebug(t *testing.T) {
	h := withCapturedLogs(t)
	runReloadHook(context.Background(), "exit 0")
	if !h.hasRecord(slog.LevelDebug, "certificate reload hook ok", "") {
		t.Errorf("expected a debug 'ok' record, got %+v", h.all())
	}
}

func TestRunReloadHookFailureLogsExitCodeNotCommandLine(t *testing.T) {
	h := withCapturedLogs(t)
	const secretMarker = "SECRET_SENTINEL_VALUE_XYZ"
	// The marker appears ONLY inside the command line's own text, as a shell
	// comment that produces NO process output -- if it ever surfaces in any
	// captured log record, the command line itself (not just its output) was
	// logged, which is exactly what must never happen (the reload command can
	// carry secrets of its own, e.g. a token baked into a wrapper script).
	command := "false # " + secretMarker
	runReloadHook(context.Background(), command)

	if !h.hasRecord(slog.LevelWarn, "certificate reload hook failed", "exit_code") {
		t.Fatalf("expected a warn record with exit_code, got %+v", h.all())
	}
	found := false
	for _, rec := range h.all() {
		if rec.msg != "certificate reload hook failed" {
			continue
		}
		if code, ok := attrInt(rec.attrs["exit_code"]); ok && code == 1 {
			found = true
		}
	}
	if !found {
		t.Errorf("expected exit_code=1 in the warn record, got %+v", h.all())
	}
	if h.containsText(secretMarker) {
		t.Errorf("the reload command line leaked into a log record: %+v", h.all())
	}
	for _, rec := range h.all() {
		if v, ok := rec.attrs["command"]; ok {
			t.Errorf("a 'command' attribute must never be logged, found: %v", v)
		}
		if v, ok := rec.attrs["cmd"]; ok {
			t.Errorf("a 'cmd' attribute must never be logged, found: %v", v)
		}
	}
}

func TestRunReloadHookAbandonsProcessHoldingPipesPastWaitDelay(t *testing.T) {
	origTimeout, origDelay := hookTimeout, hookWaitDelay
	hookTimeout = 5 * time.Second
	hookWaitDelay = 100 * time.Millisecond
	defer func() { hookTimeout, hookWaitDelay = origTimeout, origDelay }()

	withCapturedLogs(t)
	// The immediate child exits right away; the backgrounded grandchild
	// inherits the stdout/stderr pipe and keeps it open for 3s -- long past
	// hookWaitDelay. Without WaitDelay, Run() would block for ~3s.
	start := time.Now()
	runReloadHook(context.Background(), "sleep 3 & exit 0")
	elapsed := time.Since(start)

	if elapsed > 1500*time.Millisecond {
		t.Fatalf("runReloadHook took %s, want well under 1.5s (WaitDelay=%s should have abandoned the held pipe)", elapsed, hookWaitDelay)
	}
}

// The hook is what ACTIVATES the certificate, and the etag sidecar is already
// saved when it runs -- so a hook killed by a shutdown would leave the new files
// installed, the next fetch answering 304, and the reload never re-attempted.
// An already-cancelled context must therefore still run it (bounded by
// hookTimeout + WaitDelay).
func TestRunReloadHookSurvivesACancelledContext(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "hook-ran")
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled, as at shutdown

	runReloadHook(ctx, "touch "+marker)

	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("the reload hook did not run under a cancelled context: %v -- "+
			"a shutdown would then leave the new certificate installed but never activated", err)
	}
}
