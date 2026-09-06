// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package runtime

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// minimalConfigJSON is the smallest valid runtime-config document ParseConfig
// accepts, with a caller-chosen router_listen/etag so tests can distinguish
// one fetched/pushed document from another.
func minimalConfigJSON(routerListen int, etag string) string {
	return `{"router_listen":` + strconv.Itoa(routerListen) + `,"max_processes":1,"gpu_budgets":[],"specs":[],"coresident":[],"etag":"` + etag + `"}`
}

// listCacheDirEntries lists every entry in dir, for asserting no stray
// dot-prefixed temp file survives a completed (or a deliberately failed)
// writeCacheFile call.
func listCacheDirEntries(t testing.TB, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("read dir: %v", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// TestGatewaySourceFetchAndCache drives a 200 fetch end-to-end: the parsed
// config is returned with changed=true, the disk cache is written atomically
// (0600, no leftover temp file), and a brand new GatewaySource constructed
// against the SAME cachePath starts out already holding that cached config,
// with zero gateway contact of its own.
func TestGatewaySourceFetchAndCache(t *testing.T) {
	var calls int32
	var gotPath, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(minimalConfigJSON(9000, "e1")))
	}))
	defer srv.Close()

	dir := t.TempDir()
	cachePath := filepath.Join(dir, "runtime-config.cache.json")

	s := NewGatewaySource(srv.URL, "tok", srv.Client(), cachePath)
	cfg, changed, err := s.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !changed {
		t.Fatal("first Load: changed = false, want true")
	}
	if cfg.RouterListen != 9000 || cfg.ETag != "e1" {
		t.Fatalf("cfg = %+v, want RouterListen=9000 ETag=e1", cfg)
	}
	if gotPath != runtimeConfigPath {
		t.Fatalf("path = %q, want %q", gotPath, runtimeConfigPath)
	}
	if gotAuth != "Bearer tok" {
		t.Fatalf("Authorization = %q, want %q", gotAuth, "Bearer tok")
	}
	if atomic.LoadInt32(&calls) != 1 {
		t.Fatalf("server calls = %d, want 1", calls)
	}

	info, err := os.Stat(cachePath)
	if err != nil {
		t.Fatalf("stat cache file: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("cache file mode = %o, want 0600", info.Mode().Perm())
	}
	raw, err := os.ReadFile(cachePath)
	if err != nil {
		t.Fatalf("read cache file: %v", err)
	}
	cached, err := ParseConfig(raw)
	if err != nil {
		t.Fatalf("ParseConfig(cache file): %v", err)
	}
	if cached.RouterListen != 9000 || cached.ETag != "e1" {
		t.Fatalf("cache file content = %+v, want RouterListen=9000 ETag=e1", cached)
	}

	entries := listCacheDirEntries(t, dir)
	if len(entries) != 1 || entries[0] != filepath.Base(cachePath) {
		t.Fatalf("cache dir entries = %v, want exactly [%s] (no leftover temp file)", entries, filepath.Base(cachePath))
	}

	// A brand new source against the SAME cachePath, pointed at an address
	// nothing is listening on, must already hold the cached config the
	// instant it is constructed -- before any gateway contact at all.
	s2 := NewGatewaySource("http://127.0.0.1:1", "tok", srv.Client(), cachePath)
	if s2.cached.RouterListen != 9000 || s2.cached.ETag != "e1" {
		t.Fatalf("fresh source's initial cached config = %+v, want RouterListen=9000 ETag=e1 (loaded from disk at construction)", s2.cached)
	}
}

// TestGatewaySourceETag304 proves the second fetch sends the first fetch's
// etag as If-None-Match, and that a 304 response yields changed=false with
// the SAME config as the first fetch -- a 304 is never a teardown signal.
func TestGatewaySourceETag304(t *testing.T) {
	var calls int32
	var gotINM []string
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		mu.Lock()
		gotINM = append(gotINM, r.Header.Get("If-None-Match"))
		mu.Unlock()
		if n == 1 {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(minimalConfigJSON(9000, "e1")))
			return
		}
		w.WriteHeader(http.StatusNotModified)
	}))
	defer srv.Close()

	s := NewGatewaySource(srv.URL, "tok", srv.Client(), filepath.Join(t.TempDir(), "cache.json"))

	cfg1, changed1, err := s.Load(context.Background())
	if err != nil || !changed1 {
		t.Fatalf("first Load: cfg=%+v changed=%v err=%v", cfg1, changed1, err)
	}

	cfg2, changed2, err := s.Load(context.Background())
	if err != nil {
		t.Fatalf("second Load: %v", err)
	}
	if changed2 {
		t.Fatal("second Load (304): changed = true, want false")
	}
	if !reflect.DeepEqual(cfg1, cfg2) {
		t.Fatalf("second Load (304) config = %+v, want identical to first %+v", cfg2, cfg1)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(gotINM) != 2 {
		t.Fatalf("server calls = %d, want 2", len(gotINM))
	}
	if gotINM[0] != "" {
		t.Fatalf("first request If-None-Match = %q, want empty (no cache yet)", gotINM[0])
	}
	if gotINM[1] != "e1" {
		t.Fatalf("second request If-None-Match = %q, want %q", gotINM[1], "e1")
	}
}

// TestGatewaySourceTransientErrorKeepsCurrent proves a 500 AND a
// connection-refused failure each return the last known-good config with
// changed=false and a nil error -- never tearing down a running set over a
// transient gateway hiccup.
func TestGatewaySourceTransientErrorKeepsCurrent(t *testing.T) {
	var mode atomic.Int32 // 0 = ok, 1 = 500
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if mode.Load() == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(minimalConfigJSON(9000, "e1")))
	}))

	s := NewGatewaySource(srv.URL, "tok", srv.Client(), filepath.Join(t.TempDir(), "cache.json"))
	cfg1, changed1, err := s.Load(context.Background())
	if err != nil || !changed1 {
		t.Fatalf("first Load: cfg=%+v changed=%v err=%v", cfg1, changed1, err)
	}

	// 500: must keep cfg1.
	mode.Store(1)
	cfg2, changed2, err2 := s.Load(context.Background())
	if err2 != nil {
		t.Fatalf("Load on 500: err = %v, want nil", err2)
	}
	if changed2 {
		t.Fatal("Load on 500: changed = true, want false")
	}
	if !reflect.DeepEqual(cfg1, cfg2) {
		t.Fatalf("Load on 500 returned %+v, want the last known-good %+v", cfg2, cfg1)
	}

	// Connection refused (server fully closed): must ALSO keep cfg1.
	srv.Close()
	cfg3, changed3, err3 := s.Load(context.Background())
	if err3 != nil {
		t.Fatalf("Load on conn-refused: err = %v, want nil", err3)
	}
	if changed3 {
		t.Fatal("Load on conn-refused: changed = true, want false")
	}
	if !reflect.DeepEqual(cfg1, cfg3) {
		t.Fatalf("Load on conn-refused returned %+v, want the last known-good %+v", cfg3, cfg1)
	}
}

// TestGatewaySource404KeepsCurrent proves an older gateway build that lacks
// the runtime-config endpoint entirely never tears down a running set: a 404
// keeps the last known-good config, changed=false, nil error.
func TestGatewaySource404KeepsCurrent(t *testing.T) {
	var mode atomic.Int32 // 0 = ok, 1 = 404
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if mode.Load() == 1 {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(minimalConfigJSON(9000, "e1")))
	}))
	defer srv.Close()

	s := NewGatewaySource(srv.URL, "tok", srv.Client(), filepath.Join(t.TempDir(), "cache.json"))
	cfg1, changed1, err := s.Load(context.Background())
	if err != nil || !changed1 {
		t.Fatalf("first Load: cfg=%+v changed=%v err=%v", cfg1, changed1, err)
	}

	mode.Store(1)
	for i := 0; i < 3; i++ {
		cfg2, changed2, err2 := s.Load(context.Background())
		if err2 != nil {
			t.Fatalf("Load on 404 (iter %d): err = %v, want nil", i, err2)
		}
		if changed2 {
			t.Fatalf("Load on 404 (iter %d): changed = true, want false", i)
		}
		if !reflect.DeepEqual(cfg1, cfg2) {
			t.Fatalf("Load on 404 (iter %d) returned %+v, want the last known-good %+v", i, cfg2, cfg1)
		}
	}
}

// TestGatewaySourceApplyPushed proves a WS-pushed document applies and
// persists to the SAME disk cache Load uses, that a same-etag push is a
// no-op (changed=false), and that a malformed pushed frame is reported as an
// error WITHOUT disturbing the held config.
func TestGatewaySourceApplyPushed(t *testing.T) {
	dir := t.TempDir()
	cachePath := filepath.Join(dir, "cache.json")
	s := NewGatewaySource("http://127.0.0.1:1", "tok", nil, cachePath)

	raw1 := []byte(minimalConfigJSON(9100, "push-e1"))
	cfg1, changed1, err := s.ApplyPushed(raw1)
	if err != nil {
		t.Fatalf("ApplyPushed: %v", err)
	}
	if !changed1 {
		t.Fatal("first ApplyPushed: changed = false, want true")
	}
	if cfg1.RouterListen != 9100 || cfg1.ETag != "push-e1" {
		t.Fatalf("cfg1 = %+v, want RouterListen=9100 ETag=push-e1", cfg1)
	}

	onDisk, err := os.ReadFile(cachePath)
	if err != nil {
		t.Fatalf("read cache file after push: %v", err)
	}
	cachedCfg, err := ParseConfig(onDisk)
	if err != nil {
		t.Fatalf("ParseConfig(cache file): %v", err)
	}
	if cachedCfg.ETag != "push-e1" {
		t.Fatalf("cache file etag = %q, want push-e1", cachedCfg.ETag)
	}

	// Same document again (same etag): a no-op.
	cfg2, changed2, err := s.ApplyPushed(raw1)
	if err != nil {
		t.Fatalf("second ApplyPushed: %v", err)
	}
	if changed2 {
		t.Fatal("second ApplyPushed (same etag): changed = true, want false")
	}
	if cfg2.ETag != "push-e1" {
		t.Fatalf("second ApplyPushed etag = %q, want push-e1", cfg2.ETag)
	}

	// A malformed frame: reported as an error, but the held config is
	// untouched -- a caller applying the returned Config regardless of the
	// error can never tear down over one corrupt frame.
	cfgBad, changedBad, errBad := s.ApplyPushed([]byte(`{not json`))
	if errBad == nil {
		t.Fatal("ApplyPushed with malformed JSON: err = nil, want non-nil")
	}
	if changedBad {
		t.Fatal("ApplyPushed with malformed JSON: changed = true, want false")
	}
	if cfgBad.ETag != "push-e1" {
		t.Fatalf("ApplyPushed with malformed JSON returned %+v, want the last known-good (etag push-e1)", cfgBad)
	}

	// A fresh source against the same cachePath must start with the PUSHED
	// config too -- the disk cache is one document regardless of origin.
	s2 := NewGatewaySource("http://127.0.0.1:1", "tok", nil, cachePath)
	if s2.cached.ETag != "push-e1" {
		t.Fatalf("fresh source after a push-only history: cached = %+v, want etag push-e1", s2.cached)
	}
}

// TestGatewaySourceCacheWriteFailureKeepsPrevious injects a disk-write
// failure via the package-level writeCacheFile var (the certinstall
// writeTempFile precedent): a live, successfully fetched config must still
// be returned and adopted in memory (never rejected merely because it could
// not ALSO be saved for next restart), while the on-disk file from the
// PRIOR successful write is left byte-for-byte untouched.
func TestGatewaySourceCacheWriteFailureKeepsPrevious(t *testing.T) {
	var etag atomic.Value
	etag.Store("e1")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(minimalConfigJSON(9000, etag.Load().(string))))
	}))
	defer srv.Close()

	dir := t.TempDir()
	cachePath := filepath.Join(dir, "cache.json")
	s := NewGatewaySource(srv.URL, "tok", srv.Client(), cachePath)

	if _, changed, err := s.Load(context.Background()); err != nil || !changed {
		t.Fatalf("first Load: changed=%v err=%v", changed, err)
	}
	before, err := os.ReadFile(cachePath)
	if err != nil {
		t.Fatalf("read cache file after first Load: %v", err)
	}

	origWrite := writeCacheFile
	writeCacheFile = func(path string, data []byte) error {
		return errors.New("injected disk-write failure")
	}
	defer func() { writeCacheFile = origWrite }()

	etag.Store("e2")
	cfg2, changed2, err2 := s.Load(context.Background())
	if err2 != nil {
		t.Fatalf("Load with injected cache-write failure: err = %v, want nil", err2)
	}
	if !changed2 {
		t.Fatal("Load with injected cache-write failure: changed = false, want true (a good live fetch, disk-write aside)")
	}
	if cfg2.ETag != "e2" {
		t.Fatalf("Load with injected cache-write failure returned etag %q, want e2", cfg2.ETag)
	}

	after, err := os.ReadFile(cachePath)
	if err != nil {
		t.Fatalf("read cache file after failed write: %v", err)
	}
	if string(after) != string(before) {
		t.Fatalf("cache file changed despite an injected write failure: before=%q after=%q", before, after)
	}

	// A THIRD Load, with the real writeCacheFile restored, must still work
	// (the injected failure was transient, not a permanent corruption of
	// state).
	writeCacheFile = origWrite
	etag.Store("e3")
	cfg3, changed3, err3 := s.Load(context.Background())
	if err3 != nil || !changed3 || cfg3.ETag != "e3" {
		t.Fatalf("Load after restoring writeCacheFile: cfg=%+v changed=%v err=%v", cfg3, changed3, err3)
	}
}

// TestFileSourceMtimePoll proves Load re-reads/re-parses only when the
// file's mtime actually changes: forcing new content back onto the OLD
// mtime must NOT be observed (proving the mtime check, not the content
// itself, gates the read), while a genuinely new mtime picks up the new
// content.
func TestFileSourceMtimePoll(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "runtime.json")
	if err := os.WriteFile(path, []byte(minimalConfigJSON(8080, "f1")), 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}

	s := NewFileSource(path)
	cfg1, changed1, err := s.Load(context.Background())
	if err != nil || !changed1 || cfg1.ETag != "f1" {
		t.Fatalf("first Load: cfg=%+v changed=%v err=%v", cfg1, changed1, err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	origMod := info.ModTime()

	// Second Load, nothing touched: unchanged mtime -> changed=false.
	cfg2, changed2, err := s.Load(context.Background())
	if err != nil {
		t.Fatalf("second Load: %v", err)
	}
	if changed2 {
		t.Fatal("second Load (untouched file): changed = true, want false")
	}
	if !reflect.DeepEqual(cfg1, cfg2) {
		t.Fatalf("second Load config = %+v, want identical to first %+v", cfg2, cfg1)
	}

	// Overwrite with DIFFERENT content but pin the mtime back to the
	// original -- if Load re-read despite an "unchanged" mtime, it would
	// see f2 here; it must not.
	if err := os.WriteFile(path, []byte(minimalConfigJSON(8081, "f2")), 0o644); err != nil {
		t.Fatalf("rewrite file: %v", err)
	}
	if err := os.Chtimes(path, origMod, origMod); err != nil {
		t.Fatalf("chtimes (pin to original mtime): %v", err)
	}
	cfg3, changed3, err := s.Load(context.Background())
	if err != nil {
		t.Fatalf("third Load: %v", err)
	}
	if changed3 {
		t.Fatal("third Load (new content, pinned old mtime): changed = true, want false")
	}
	if cfg3.ETag != "f1" {
		t.Fatalf("third Load etag = %q, want f1 (must not have re-read the mtime-pinned file)", cfg3.ETag)
	}

	// Now genuinely advance the mtime: the new content must be picked up.
	newMod := origMod.Add(2 * time.Second)
	if err := os.Chtimes(path, newMod, newMod); err != nil {
		t.Fatalf("chtimes (advance mtime): %v", err)
	}
	cfg4, changed4, err := s.Load(context.Background())
	if err != nil {
		t.Fatalf("fourth Load: %v", err)
	}
	if !changed4 {
		t.Fatal("fourth Load (advanced mtime): changed = false, want true")
	}
	if cfg4.ETag != "f2" {
		t.Fatalf("fourth Load etag = %q, want f2", cfg4.ETag)
	}
}

// TestFileSourceParseErrorKeepsLastGood proves a broken (unparseable) file
// keeps the last good config with changed=false and a nil error, records
// the failure via LastParseError, and clears it again once the file is
// fixed and successfully reparsed.
func TestFileSourceParseErrorKeepsLastGood(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "runtime.json")
	if err := os.WriteFile(path, []byte(minimalConfigJSON(8080, "g1")), 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}

	s := NewFileSource(path)
	cfg1, changed1, err := s.Load(context.Background())
	if err != nil || !changed1 || cfg1.ETag != "g1" {
		t.Fatalf("first Load: cfg=%+v changed=%v err=%v", cfg1, changed1, err)
	}
	if msg, at := s.LastParseError(); msg != "" || !at.IsZero() {
		t.Fatalf("LastParseError before any failure = (%q, %v), want (\"\", zero)", msg, at)
	}

	info, _ := os.Stat(path)
	broken := info.ModTime().Add(time.Second)
	if err := os.WriteFile(path, []byte(`{not json`), 0o644); err != nil {
		t.Fatalf("write broken file: %v", err)
	}
	if err := os.Chtimes(path, broken, broken); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	before := time.Now()
	cfg2, changed2, err2 := s.Load(context.Background())
	if err2 != nil {
		t.Fatalf("Load on broken file: err = %v, want nil", err2)
	}
	if changed2 {
		t.Fatal("Load on broken file: changed = true, want false")
	}
	if !reflect.DeepEqual(cfg1, cfg2) {
		t.Fatalf("Load on broken file returned %+v, want the last known-good %+v", cfg2, cfg1)
	}
	code, at := s.LastParseError()
	if code != ParseErrorJSONSyntax {
		t.Fatalf("LastParseError code = %q, want %q -- the recorded value is the wire CLASSIFICATION CODE, never the error text (see ParseErrorCode)", code, ParseErrorJSONSyntax)
	}
	if at.Before(before) || at.After(time.Now()) {
		t.Fatalf("LastParseError timestamp %v not within the call's window (after %v)", at, before)
	}

	// Fix the file: the next Load succeeds and clears LastParseError.
	fixed := broken.Add(time.Second)
	if err := os.WriteFile(path, []byte(minimalConfigJSON(8082, "g2")), 0o644); err != nil {
		t.Fatalf("write fixed file: %v", err)
	}
	if err := os.Chtimes(path, fixed, fixed); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	cfg3, changed3, err3 := s.Load(context.Background())
	if err3 != nil || !changed3 || cfg3.ETag != "g2" {
		t.Fatalf("Load on fixed file: cfg=%+v changed=%v err=%v", cfg3, changed3, err3)
	}
	if msg, at := s.LastParseError(); msg != "" || !at.IsZero() {
		t.Fatalf("LastParseError after a subsequent success = (%q, %v), want (\"\", zero)", msg, at)
	}
}

// runtimeConfigAccessLogMsg is recordAccessFailure's exact message, duplicated
// here for the same reason runtimeConfigMissingLogMsg below is: a loose match
// would keep passing if the edge-triggering broke and a different line started
// appearing instead.
const runtimeConfigAccessLogMsg = "runtime: local runtime-config file could not be read; keeping current config"

// TestFileSourceAccessFailuresAreReported is A1: FileSource.Load used to
// return `current, false, nil` from BOTH os.Stat and os.ReadFile failures,
// with no lastErr and no log line -- while the parse branch three lines below
// did both. A file-mode agent whose runtime.json was missing, moved or
// unreadable therefore ran indefinitely on the last known-good (or on nothing
// at all, on a first read) and reported NO failure upward, so the portal
// showed "nothing configured" -- indistinguishable from a server that
// genuinely has no specs.
//
// The subtests are the whole reachable surface of that gap plus the two
// boundaries a naive fix breaks:
//
//   - missing and unreadable each get their OWN code, because they send an
//     operator to different places (see ParseErrorFileMissing);
//   - a stat failure that is not "not there" is read_failed, not
//     file_missing -- the classification is errors.Is(fs.ErrNotExist), never
//     "stat failed at all";
//   - an UNCHANGED file still reports nothing. This is the steady state every
//     poll lands in, so a fix that records on the quiet path turns a healthy
//     agent into one reporting an error several times a minute;
//   - a file restored with an mtime the source has already seen still clears
//     the failure. Without recordAccessFailure clearing haveSeen, the
//     unchanged-mtime shortcut would skip the re-parse that clears lastErr
//     and the portal would show a resolved failure indefinitely.
func TestFileSourceAccessFailuresAreReported(t *testing.T) {
	t.Run("missing file", func(t *testing.T) {
		dir := t.TempDir()
		s := NewFileSource(filepath.Join(dir, "runtime.json"))

		before := time.Now()
		cfg, changed, err := s.Load(context.Background())
		if err != nil {
			t.Fatalf("Load on a missing file: err = %v, want nil (a bad moment on disk never tears down a running set)", err)
		}
		if changed {
			t.Fatal("Load on a missing file: changed = true, want false")
		}
		if !reflect.DeepEqual(cfg, emptyConfig()) {
			t.Fatalf("Load on a missing file returned %+v, want the empty config", cfg)
		}
		code, at := s.LastParseError()
		if code != ParseErrorFileMissing {
			t.Fatalf("LastParseError = %q, want %q -- silence here is what makes an absent file look like an empty one in the portal", code, ParseErrorFileMissing)
		}
		if at.Before(before) || at.After(time.Now()) {
			t.Fatalf("LastParseError timestamp %v not within the call's window (after %v)", at, before)
		}
	})

	t.Run("unreadable file", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("running as root: file mode bits do not deny a read, so there is nothing to observe")
		}
		dir := t.TempDir()
		path := filepath.Join(dir, "runtime.json")
		// Mode 0 denies the READ while leaving the file stat-able (stat
		// needs the directory's x bit, not the file's r bit), so this is
		// the os.ReadFile branch specifically, not the os.Stat one.
		if err := os.WriteFile(path, []byte(minimalConfigJSON(8080, "r1")), 0o000); err != nil {
			t.Fatalf("seed unreadable file: %v", err)
		}
		s := NewFileSource(path)

		if _, changed, err := s.Load(context.Background()); err != nil || changed {
			t.Fatalf("Load on an unreadable file: changed=%v err=%v, want (false, nil)", changed, err)
		}
		if code, _ := s.LastParseError(); code != ParseErrorReadFailed {
			t.Fatalf("LastParseError = %q, want %q -- a file that is there but unreadable is a permission fault, never a fresh install", code, ParseErrorReadFailed)
		}
	})

	t.Run("stat failure that is not not-exist", func(t *testing.T) {
		dir := t.TempDir()
		notADir := filepath.Join(dir, "runtime.json")
		if err := os.WriteFile(notADir, []byte("x"), 0o644); err != nil {
			t.Fatalf("seed: %v", err)
		}
		// A path THROUGH a regular file: os.Stat fails with ENOTDIR, which
		// is not fs.ErrNotExist. The classification must follow the error,
		// not the fact that stat failed.
		s := NewFileSource(filepath.Join(notADir, "runtime.json"))
		if _, changed, err := s.Load(context.Background()); err != nil || changed {
			t.Fatalf("Load through a non-directory: changed=%v err=%v, want (false, nil)", changed, err)
		}
		if code, _ := s.LastParseError(); code != ParseErrorReadFailed {
			t.Fatalf("LastParseError = %q, want %q -- only fs.ErrNotExist is file_missing", code, ParseErrorReadFailed)
		}
	})

	t.Run("unchanged file reports no failure", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "runtime.json")
		if err := os.WriteFile(path, []byte(minimalConfigJSON(8080, "u1")), 0o644); err != nil {
			t.Fatalf("seed: %v", err)
		}
		s := NewFileSource(path)
		if _, changed, err := s.Load(context.Background()); err != nil || !changed {
			t.Fatalf("first Load: changed=%v err=%v, want (true, nil)", changed, err)
		}
		// The steady state: several polls of a file nobody touched.
		for i := range 4 {
			_, changed, err := s.Load(context.Background())
			if err != nil || changed {
				t.Fatalf("poll %d: changed=%v err=%v, want (false, nil)", i, changed, err)
			}
			if code, at := s.LastParseError(); code != "" || !at.IsZero() {
				t.Fatalf("poll %d: LastParseError = (%q, %v), want no failure -- this path is every poll of a healthy agent", i, code, at)
			}
		}
	})

	t.Run("restored with an already-seen mtime still clears", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "runtime.json")
		body := []byte(minimalConfigJSON(8080, "s1"))
		if err := os.WriteFile(path, body, 0o644); err != nil {
			t.Fatalf("seed: %v", err)
		}
		s := NewFileSource(path)
		if _, changed, err := s.Load(context.Background()); err != nil || !changed {
			t.Fatalf("first Load: changed=%v err=%v, want (true, nil)", changed, err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat: %v", err)
		}
		mod := info.ModTime()

		if err := os.Remove(path); err != nil {
			t.Fatalf("remove: %v", err)
		}
		if _, _, err := s.Load(context.Background()); err != nil {
			t.Fatalf("Load while missing: %v", err)
		}
		if code, _ := s.LastParseError(); code != ParseErrorFileMissing {
			t.Fatalf("LastParseError while missing = %q, want %q", code, ParseErrorFileMissing)
		}

		// Restored byte-identically AND with the mtime the source already
		// recorded -- a timestamp-preserving restore, or an atomic replace
		// by a copy of the same file.
		if err := os.WriteFile(path, body, 0o644); err != nil {
			t.Fatalf("restore: %v", err)
		}
		if err := os.Chtimes(path, mod, mod); err != nil {
			t.Fatalf("chtimes (pin to the already-seen mtime): %v", err)
		}
		cfg, _, err := s.Load(context.Background())
		if err != nil {
			t.Fatalf("Load after restore: %v", err)
		}
		if cfg.ETag != "s1" {
			t.Fatalf("Load after restore etag = %q, want s1", cfg.ETag)
		}
		if code, at := s.LastParseError(); code != "" || !at.IsZero() {
			t.Fatalf("LastParseError after restore = (%q, %v), want cleared -- an mtime the source has already seen must not shortcut past the re-parse that clears it", code, at)
		}
	})
}

// TestFileSourceAccessFailureLogsOnTheEdgeOnly pins recordAccessFailure's
// second discipline, the one an at-least-once assertion cannot see: the file
// is polled continuously, so a Debug line per poll for a wrong path that is
// not changing is how a log stops being read. The line must appear when the
// failure ARRIVES and when it CHANGES KIND, and not in between -- and
// lastErrAt must not creep forward while the same failure persists, or "since
// when" becomes "just now, always".
func TestFileSourceAccessFailureLogsOnTheEdgeOnly(t *testing.T) {
	h := withCountedLogs(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "runtime.json")
	s := NewFileSource(path)

	for range 3 {
		if _, _, err := s.Load(context.Background()); err != nil {
			t.Fatalf("Load while missing: %v", err)
		}
	}
	if got := h.count(runtimeConfigAccessLogMsg); got != 1 {
		t.Fatalf("log count after 3 polls of a missing file = %d, want 1 (a per-poll line is ~17k a day at a 5s cadence)", got)
	}
	code, firstAt := s.LastParseError()
	if code != ParseErrorFileMissing || firstAt.IsZero() {
		t.Fatalf("LastParseError = (%q, %v), want (%q, non-zero)", code, firstAt, ParseErrorFileMissing)
	}

	if _, _, err := s.Load(context.Background()); err != nil {
		t.Fatalf("fourth Load: %v", err)
	}
	if _, at := s.LastParseError(); !at.Equal(firstAt) {
		t.Fatalf("LastParseError timestamp moved from %v to %v while the same failure persisted", firstAt, at)
	}

	// A DIFFERENT failure kind is an edge: it both re-logs and re-stamps.
	if os.Geteuid() == 0 {
		t.Skip("running as root: the unreadable half of this test cannot be observed")
	}
	if err := os.WriteFile(path, []byte(minimalConfigJSON(8080, "e1")), 0o000); err != nil {
		t.Fatalf("seed unreadable file: %v", err)
	}
	if _, _, err := s.Load(context.Background()); err != nil {
		t.Fatalf("Load on an unreadable file: %v", err)
	}
	if got := h.count(runtimeConfigAccessLogMsg); got != 2 {
		t.Fatalf("log count after the failure changed kind = %d, want 2 -- a latch that never clears silences the new fault for the process's lifetime", got)
	}
	code, secondAt := s.LastParseError()
	if code != ParseErrorReadFailed {
		t.Fatalf("LastParseError = %q, want %q", code, ParseErrorReadFailed)
	}
	if !secondAt.After(firstAt) {
		t.Fatalf("LastParseError timestamp %v did not advance past %v when the failure changed kind", secondAt, firstAt)
	}
}

// countingHandler is a slog.Handler that counts records by message. The
// certinstall recordingHandler pattern (internal/certinstall/testutil_test.go),
// reduced to the one thing the dedup assertion below needs: how many times a
// specific line was emitted. Guarded by a mutex because a Manager owner
// goroutine from an unrelated test may still be logging while this handler is
// installed as the default.
// text accumulates every record's message plus its attrs' key=value pairs,
// used only by containsText below (the I2 secret-leak guard): a plain
// message-count map cannot tell a caller whether some attr smuggled a value
// the message text itself never mentions.
type countingHandler struct {
	mu     sync.Mutex
	counts map[string]int
	text   strings.Builder
}

func (h *countingHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *countingHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.counts == nil {
		h.counts = make(map[string]int)
	}
	h.counts[r.Message]++
	h.text.WriteString(r.Message)
	h.text.WriteByte('\n')
	r.Attrs(func(a slog.Attr) bool {
		h.text.WriteString(a.Key)
		h.text.WriteByte('=')
		h.text.WriteString(a.Value.String())
		h.text.WriteByte('\n')
		return true
	})
	return nil
}

func (h *countingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *countingHandler) WithGroup(string) slog.Handler      { return h }

func (h *countingHandler) count(msg string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.counts[msg]
}

// containsText reports whether substr appears anywhere across every record
// handled so far -- messages and attr values alike. Used by the I2 tests to
// prove a token value never reaches the log, not merely that it is absent
// from one specific message string.
func (h *countingHandler) containsText(substr string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return strings.Contains(h.text.String(), substr)
}

// withCountedLogs installs a countingHandler as the default slog handler for
// the duration of the test, restoring the previous default afterward.
func withCountedLogs(t *testing.T) *countingHandler {
	t.Helper()
	h := &countingHandler{}
	prev := slog.Default()
	slog.SetDefault(slog.New(h))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return h
}

// runtimeConfigMissingLogMsg is warnMissingOnce's exact message. Duplicated
// here on purpose: a test that matched loosely (a substring, or "any Debug
// record") would keep passing if the dedup broke and some OTHER line started
// appearing instead.
const runtimeConfigMissingLogMsg = "runtime: gateway runtime-config endpoint not found (older gateway build?); keeping current config"

// TestGatewaySource404LogsExactlyOncePerStreak is B8: the 404 BEHAVIOUR (the
// running set survives, changed=false, nil error) is already covered across
// three calls by TestGatewaySource404KeepsCurrent -- what was missing is the
// dedup claim itself. "Logged at Debug, and only once per consecutive streak
// of 404s" (Load's own comment) is an EXACTLY-once claim, and an at-least-once
// assertion cannot tell it from the un-deduped version that logs on every
// poll: at a 5s cadence that is ~17k lines a day from a gateway that is
// merely older than this agent.
//
// The assertion discriminates in BOTH directions, which is the part the
// at-least-once shape gets wrong:
//   - exactly 1 after a streak of 5 (a per-call log gives 5; that is the
//     regression this test exists to catch);
//   - still exactly 1 after MORE 404s in the same streak (a counter that
//     resets on every call would tick up again);
//   - exactly 2 after a 200 resets the streak and a fresh 404 follows (a
//     latch that never clears would stay at 1 and permanently silence the
//     line for the process's whole lifetime -- the opposite failure, and the
//     reason clearMissingFlag exists at all).
func TestGatewaySource404LogsExactlyOncePerStreak(t *testing.T) {
	var mode atomic.Int32 // 0 = ok, 1 = 404
	var etag atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if mode.Load() == 1 {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(minimalConfigJSON(9000, "e"+strconv.Itoa(int(etag.Load())))))
	}))
	defer srv.Close()

	h := withCountedLogs(t)
	s := NewGatewaySource(srv.URL, "tok", srv.Client(), filepath.Join(t.TempDir(), "cache.json"))

	if _, _, err := s.Load(context.Background()); err != nil {
		t.Fatalf("first (200) Load: %v", err)
	}
	if got := h.count(runtimeConfigMissingLogMsg); got != 0 {
		t.Fatalf("after a 200 Load the missing-endpoint line was logged %d times, want 0", got)
	}

	mode.Store(1)
	for i := 0; i < 5; i++ {
		if _, _, err := s.Load(context.Background()); err != nil {
			t.Fatalf("Load on 404 (iter %d): %v", i, err)
		}
	}
	if got := h.count(runtimeConfigMissingLogMsg); got != 1 {
		t.Fatalf("after a streak of 5 consecutive 404s the missing-endpoint line was logged %d times, want EXACTLY 1 -- the dedup is what keeps an older gateway from producing one line per poll forever", got)
	}

	for i := 0; i < 5; i++ {
		if _, _, err := s.Load(context.Background()); err != nil {
			t.Fatalf("Load on 404 (continued streak, iter %d): %v", i, err)
		}
	}
	if got := h.count(runtimeConfigMissingLogMsg); got != 1 {
		t.Fatalf("after 10 consecutive 404s the missing-endpoint line was logged %d times, want STILL exactly 1", got)
	}

	// A 200 must clear the latch: the same condition recurring after the
	// gateway was seen working again is news, and deserves the line again.
	mode.Store(0)
	etag.Store(1)
	if _, _, err := s.Load(context.Background()); err != nil {
		t.Fatalf("recovery (200) Load: %v", err)
	}
	mode.Store(1)
	for i := 0; i < 3; i++ {
		if _, _, err := s.Load(context.Background()); err != nil {
			t.Fatalf("Load on the second 404 streak (iter %d): %v", i, err)
		}
	}
	if got := h.count(runtimeConfigMissingLogMsg); got != 2 {
		t.Fatalf("after a 200 reset followed by a fresh streak of 3 404s the missing-endpoint line was logged %d times, want exactly 2 -- a latch that never clears silences this line for the rest of the process's life", got)
	}
}

// TestGatewaySource304AlsoClearsTheMissingLatch pins the other reset path:
// Load calls clearMissingFlag on 304 as well as 200, so an agent whose etag
// happens to still match after a gateway downgrade+upgrade cycle is not left
// permanently silenced either.
func TestGatewaySource304AlsoClearsTheMissingLatch(t *testing.T) {
	var mode atomic.Int32 // 0 = 200/304 by etag, 1 = 404
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if mode.Load() == 1 {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.Header.Get("If-None-Match") != "" {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(minimalConfigJSON(9000, "e1")))
	}))
	defer srv.Close()

	h := withCountedLogs(t)
	s := NewGatewaySource(srv.URL, "tok", srv.Client(), filepath.Join(t.TempDir(), "cache.json"))
	if _, _, err := s.Load(context.Background()); err != nil { // 200, seeds the etag
		t.Fatalf("first Load: %v", err)
	}

	mode.Store(1)
	if _, _, err := s.Load(context.Background()); err != nil {
		t.Fatalf("404 Load: %v", err)
	}
	if got := h.count(runtimeConfigMissingLogMsg); got != 1 {
		t.Fatalf("missing-endpoint line logged %d times after the first 404, want 1", got)
	}

	mode.Store(0)
	if _, _, err := s.Load(context.Background()); err != nil { // 304
		t.Fatalf("304 Load: %v", err)
	}
	mode.Store(1)
	if _, _, err := s.Load(context.Background()); err != nil {
		t.Fatalf("second-streak 404 Load: %v", err)
	}
	if got := h.count(runtimeConfigMissingLogMsg); got != 2 {
		t.Fatalf("missing-endpoint line logged %d times after a 304 reset and a fresh 404, want 2 -- a 304 must clear the latch just like a 200", got)
	}
}

// configJSONWithSpecToken is minimalConfigJSON's sibling for the I2
// insecure-token-transport tests below: the smallest valid document that
// carries exactly one spec, with a caller-chosen (possibly empty) APIToken.
// An empty apiToken produces a token-FREE spec, not an absent one -- test (c)
// below relies on that to prove a real spec with nothing in api_token does
// not itself trip the warning.
func configJSONWithSpecToken(routerListen int, etag, specID, apiToken string) string {
	return `{"router_listen":` + strconv.Itoa(routerListen) + `,"max_processes":1,"gpu_budgets":[],` +
		`"specs":[{"id":"` + specID + `","api_token":"` + apiToken + `"}],"coresident":[],"etag":"` + etag + `"}`
}

// runtimeConfigInsecureTokenLogMsg is warnInsecureTokenOnce's exact message.
// Duplicated here on purpose, exactly like runtimeConfigMissingLogMsg above:
// a loosely-matched assertion would keep passing if the dedup broke, or if
// some unrelated WARN line started appearing instead.
const runtimeConfigInsecureTokenLogMsg = "runtime: applying a runtime config that carries an API token over a non-https gateway URL; the token will cross the gateway<->agent channel in clear -- configure an https gateway URL"

// TestGatewaySourceWarnsOnceForTokenOverInsecureBase is I2 (security review):
// design §9 requires that any non-off token mode "requires or at minimum
// LOUDLY WARNS for an https gateway URL". The agent is the only party that
// reliably knows its own configured gateway scheme, so the warning lives
// here. ApplyPushed is the easiest way to drive "a freshly-parsed config
// becomes the applied desired state" without a real HTTP round trip; Load's
// own http.StatusOK path shares the exact same helper (checkInsecureToken),
// so this is not testing a code path Load skips.
//
// The assertion is an EXACTLY-once claim across TWO applies, for the same
// reason TestGatewaySource404LogsExactlyOncePerStreak insists on it: an
// at-least-once assertion cannot distinguish a correct once-per-source guard
// from a per-apply line that would spam a log every time the gateway
// re-pushes the same (or a new) token-bearing document.
func TestGatewaySourceWarnsOnceForTokenOverInsecureBase(t *testing.T) {
	h := withCountedLogs(t)
	s := NewGatewaySource("http://127.0.0.1:1", "tok", nil, "")

	raw := []byte(configJSONWithSpecToken(9200, "e1", "spec-1", "upstream-secret-token"))
	if _, _, err := s.ApplyPushed(raw); err != nil {
		t.Fatalf("first ApplyPushed: %v", err)
	}
	if got := h.count(runtimeConfigInsecureTokenLogMsg); got != 1 {
		t.Fatalf("insecure-token line logged %d times after the first apply, want 1", got)
	}

	// A second apply of a (here, identical) token-bearing config over the
	// SAME http:// source must not log again -- once per source lifetime.
	raw2 := []byte(configJSONWithSpecToken(9200, "e2", "spec-1", "upstream-secret-token"))
	if _, _, err := s.ApplyPushed(raw2); err != nil {
		t.Fatalf("second ApplyPushed: %v", err)
	}
	if got := h.count(runtimeConfigInsecureTokenLogMsg); got != 1 {
		t.Fatalf("insecure-token line logged %d times after a second apply, want 1 (once per source lifetime)", got)
	}

	// The token value itself must never appear in ANY logged record -- not
	// just the one line under test. A future edit that adds an unrelated Warn
	// carrying "attrs" with the raw config would otherwise slip a secret into
	// the log silently.
	if h.containsText("upstream-secret-token") {
		t.Fatal("a log record contains the raw API token value; the warning (or something else) is leaking the secret")
	}
}

// TestGatewaySourceHTTPSBaseNeverWarnsAboutToken proves the https:// half of
// the classification: the SAME token-bearing document applied over a
// https:// source must never trip the warning, however many times it is
// applied.
func TestGatewaySourceHTTPSBaseNeverWarnsAboutToken(t *testing.T) {
	h := withCountedLogs(t)
	s := NewGatewaySource("https://gateway.example:8443", "tok", nil, "")

	raw := []byte(configJSONWithSpecToken(9300, "e1", "spec-1", "upstream-secret-token"))
	if _, _, err := s.ApplyPushed(raw); err != nil {
		t.Fatalf("ApplyPushed: %v", err)
	}
	if got := h.count(runtimeConfigInsecureTokenLogMsg); got != 0 {
		t.Fatalf("insecure-token line logged %d times over an https:// base, want 0", got)
	}
}

// TestGatewaySourceHTTPBaseWithoutTokenNeverWarns proves the other half of
// the classification: an http:// base is not itself the trigger -- only an
// http:// base carrying a config with a non-empty spec.APIToken is. A
// token-free config (every spec's api_token is "" or absent) must never
// trip the warning, since there is nothing crossing the wire in clear for
// this feature to warn about.
func TestGatewaySourceHTTPBaseWithoutTokenNeverWarns(t *testing.T) {
	h := withCountedLogs(t)
	s := NewGatewaySource("http://127.0.0.1:1", "tok", nil, "")

	raw := []byte(configJSONWithSpecToken(9400, "e1", "spec-1", ""))
	if _, _, err := s.ApplyPushed(raw); err != nil {
		t.Fatalf("ApplyPushed: %v", err)
	}
	if got := h.count(runtimeConfigInsecureTokenLogMsg); got != 0 {
		t.Fatalf("insecure-token line logged %d times for a token-free config over http://, want 0", got)
	}
}
