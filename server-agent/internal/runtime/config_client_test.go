// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package runtime

import (
	"context"
	"errors"
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
	msg, at := s.LastParseError()
	if msg == "" {
		t.Fatal("LastParseError message is empty after a parse failure")
	}
	if !strings.Contains(msg, "runtime") && !strings.Contains(msg, "parse") {
		t.Fatalf("LastParseError message = %q, does not look like a parse error", msg)
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
