// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"op-ai-server-agent/internal/config"
	"op-ai-server-agent/internal/sample"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const llamaVersionOutput = "version: 0.9.0-dev (build 1, commit 2bc46ba)\nbuilt with AppleClang 21.0.0.21000401 for Darwin arm64\n"

type fakeFileInfo struct {
	size  int64
	mtime time.Time
	mode  fs.FileMode
}

func (f fakeFileInfo) Name() string       { return "x" }
func (f fakeFileInfo) Size() int64        { return f.size }
func (f fakeFileInfo) Mode() fs.FileMode  { return f.mode }
func (f fakeFileInfo) ModTime() time.Time { return f.mtime }
func (f fakeFileInfo) IsDir() bool        { return false }
func (f fakeFileInfo) Sys() any           { return nil }

func statFake(fi fakeFileInfo) func(string) (fs.FileInfo, error) {
	return func(string) (fs.FileInfo, error) { return fi, nil }
}

func TestParseLlamaVersionOutput(t *testing.T) {
	v, b, c, ok := parseLlamaVersion(llamaVersionOutput)
	if !ok || v != "0.9.0-dev" || b != "1" || c != "2bc46ba" {
		t.Fatalf("got %q %q %q %v", v, b, c, ok)
	}
	if _, _, _, ok := parseLlamaVersion("usage: llama-server [options]"); ok {
		t.Fatal("non-version output parsed")
	}
}

func TestVersionFromPathTagAndCommit(t *testing.T) {
	cases := []struct {
		path, build, commit string
		ok                  bool
	}{
		{"/opt/llama.cpp/b1234/ggml-rpc-server", "1234", "", true},
		{"/opt/llama-2bc46ba/bin/rpc-server", "", "2bc46ba", true},
		{"/opt/2bc46baa9207d098d03741ac394e7faaf1a955b6/rpc-server", "", "2bc46baa9207d098d03741ac394e7faaf1a955b6", true},
		{"/opt/1234567/rpc-server", "", "", false},  // all digits: not a commit
		{"/opt/20261105/rpc-server", "", "", false}, // date
		{"/opt/abc12/rpc-server", "", "", false},    // too short
		{"C:\\llama\\b5000\\rpc-server.exe", "5000", "", true},
		{"/usr/bin/rpc-server", "", "", false},
	}
	for _, c := range cases {
		b, cm, ok := versionFromPath(c.path)
		if b != c.build || cm != c.commit || ok != c.ok {
			t.Errorf("%s: got %q %q %v, want %q %q %v", c.path, b, cm, ok, c.build, c.commit, c.ok)
		}
	}
}

func TestRPCServerInheritsSiblingVersion(t *testing.T) {
	run := func(_ context.Context, p string) (string, error) {
		if p != "/opt/l/llama-server" {
			t.Fatalf("executed %s", p)
		}
		return llamaVersionOutput, nil
	}
	got := scanBinaries([]string{"/opt/l/llama-server", "/opt/l/ggml-rpc-server", "/opt/other/b99/rpc-server", "/opt/x/tool"},
		statFake(fakeFileInfo{size: 1}), run, map[string]cachedVersion{})
	if len(got) != 4 {
		t.Fatalf("got %+v", got)
	}
	if got[0].Kind != "llama_server" || got[0].Version != "0.9.0-dev" || got[0].VersionSource != "agent" || !got[0].Allowed {
		t.Errorf("llama: %+v", got[0])
	}
	if got[1].Kind != "rpc_server" || got[1].Version != "0.9.0-dev" || got[1].Build != "1" || got[1].Commit != "2bc46ba" || got[1].VersionSource != "agent" {
		t.Errorf("sibling rpc: %+v", got[1])
	}
	if got[2].Build != "99" || got[2].VersionSource != "path" || got[2].Version != "" {
		t.Errorf("path rpc: %+v", got[2])
	}
	if got[3].Kind != "other" || got[3].VersionSource != "" {
		t.Errorf("other: %+v", got[3])
	}
}

func TestVersionRunCachedByStat(t *testing.T) {
	runs := 0
	run := func(context.Context, string) (string, error) { runs++; return llamaVersionOutput, nil }
	cache := map[string]cachedVersion{}
	fi := fakeFileInfo{size: 10, mtime: time.Unix(100, 0)}
	paths := []string{"/opt/l/llama-server"}
	scanBinaries(paths, statFake(fi), run, cache)
	scanBinaries(paths, statFake(fi), run, cache)
	if runs != 1 {
		t.Fatalf("runs = %d, want 1", runs)
	}
	fi.mtime = time.Unix(200, 0)
	scanBinaries(paths, statFake(fi), run, cache)
	fi.size = 11
	scanBinaries(paths, statFake(fi), run, cache)
	if runs != 3 {
		t.Fatalf("runs = %d, want 3 after mtime and size changes", runs)
	}
}

func TestOnlyLlamaServerIsExecuted(t *testing.T) {
	var ran []string
	run := func(_ context.Context, p string) (string, error) { ran = append(ran, p); return "", errors.New("boom") }
	got := scanBinaries([]string{"/a/rpc-server", "/a/ggml-rpc-server.exe", "/a/LLAMA-SERVER.EXE", "/a/other"},
		statFake(fakeFileInfo{}), run, map[string]cachedVersion{})
	if len(ran) != 1 || ran[0] != "/a/LLAMA-SERVER.EXE" {
		t.Fatalf("executed %v", ran)
	}
	if got[0].Kind != "rpc_server" || got[1].Kind != "rpc_server" || got[2].Kind != "llama_server" {
		t.Fatalf("kinds: %+v", got)
	}
}

func TestSampleCarriesBinariesOnlyOnChange(t *testing.T) {
	poster := &capturePoster{}
	a := NewFromDeps(config.Config{Interval: time.Hour, RuntimeAllowedBinaries: []string{"/opt/l/llama-server"}}, Deps{Poster: poster})
	a.binaries.stat = statFake(fakeFileInfo{size: 1})
	out := llamaVersionOutput
	a.binaries.runVersion = func(context.Context, string) (string, error) { return out, nil }

	a.collectOnce(context.Background())
	if poster.last().Binaries != nil {
		t.Fatal("binaries sent before the first scan finished")
	}
	a.binaries.rescan(context.Background())
	a.collectOnce(context.Background())
	first := poster.last().Binaries
	if first == nil || len(*first) != 1 || (*first)[0].Version != "0.9.0-dev" {
		t.Fatalf("first sample: %+v", first)
	}
	a.binaries.rescan(context.Background())
	a.collectOnce(context.Background())
	if poster.last().Binaries != nil {
		t.Fatal("unchanged inventory resent")
	}
	a.binaries.cache = map[string]cachedVersion{}
	out = "version: 0.9.1 (build 2, commit 1234abc)\n"
	a.binaries.rescan(context.Background())
	a.collectOnce(context.Background())
	if b := poster.last().Binaries; b == nil || (*b)[0].Version != "0.9.1" {
		t.Fatalf("changed inventory not sent: %+v", b)
	}
	raw, _ := json.Marshal(poster.last())
	if !json.Valid(raw) {
		t.Fatal("bad json")
	}
}

func TestEmptyInventoryTravelsAsEmptyArray(t *testing.T) {
	poster := &capturePoster{}
	a := NewFromDeps(config.Config{Interval: time.Hour}, Deps{Poster: poster})
	a.binaries.rescan(context.Background())
	a.collectOnce(context.Background())
	raw, _ := json.Marshal(poster.last())
	var m map[string]json.RawMessage
	_ = json.Unmarshal(raw, &m)
	if string(m["binaries"]) != "[]" {
		t.Fatalf("binaries = %s", m["binaries"])
	}
}

func TestExecVersionRunsWithoutShellAndMinimalEnv(t *testing.T) {
	t.Setenv("OP_SECRET_TOKEN", "hunter2")
	for _, e := range minimalEnv() {
		if len(e) >= 9 && e[:9] == "OP_SECRET" {
			t.Fatal("secret leaked into the env")
		}
	}
	if _, err := execVersion(context.Background(), os.DevNull); err == nil {
		t.Fatal("expected an error executing a non-binary")
	}
}

func writeScript(t *testing.T, body string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell script helper")
	}
	p := filepath.Join(t.TempDir(), "llama-server")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestExecVersionReturnsWhenGrandchildHoldsStdout(t *testing.T) {
	// The background sleep is the grandchild that keeps the output pipe open;
	// the script records its pid so the test kills it rather than leaving it
	// behind. exec makes the foreground sleep the direct child itself, which
	// the context kills.
	pidFile := filepath.Join(t.TempDir(), "grandchild.pid")
	p := writeScript(t, "sleep 30 &\necho $! > '"+pidFile+"'\nexec sleep 30\n")
	t.Cleanup(func() {
		raw, err := os.ReadFile(pidFile)
		if err != nil {
			return
		}
		if pid, err := strconv.Atoi(strings.TrimSpace(string(raw))); err == nil {
			if proc, err := os.FindProcess(pid); err == nil {
				_ = proc.Kill()
			}
		}
	})
	orig := binaryWaitDelay
	binaryWaitDelay = 200 * time.Millisecond
	defer func() { binaryWaitDelay = orig }()
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	done := make(chan struct{})
	go func() { _, _ = execVersion(ctx, p); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("execVersion blocked on a grandchild holding the output pipe")
	}
}

func TestExecVersionCapsOutput(t *testing.T) {
	p := writeScript(t, "yes x | head -c 1000000\n")
	out, _ := execVersion(context.Background(), p)
	if len(out) != binaryOutputLimit {
		t.Fatalf("captured %d bytes, want %d", len(out), binaryOutputLimit)
	}
}

func TestExecVersionChildEnvHasNoAgentSettings(t *testing.T) {
	t.Setenv("OP_AGENT_API_TOKEN", "hunter2")
	t.Setenv("OP_AI_OTHER", "x")
	p := writeScript(t, "env\n")
	out, err := execVersion(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "OP_AGENT_") || strings.Contains(out, "OP_AI_") || strings.Contains(out, "hunter2") {
		t.Fatalf("child env leaked agent settings:\n%s", out)
	}
}

func TestScanSkipsNonRegularEntries(t *testing.T) {
	stat := statFake(fakeFileInfo{mode: fs.ModeDir})
	got := scanBinaries([]string{"/opt/llama-dir"}, stat, func(context.Context, string) (string, error) { return "", nil }, nil)
	if len(got) != 0 {
		t.Fatalf("directory listed: %+v", got)
	}
}

type flakyPoster struct {
	fail    atomic.Bool
	samples []*sample.Sample
}

func (p *flakyPoster) Post(_ context.Context, s *sample.Sample) error {
	p.samples = append(p.samples, s)
	if p.fail.Load() {
		return errors.New("down")
	}
	return nil
}

func TestFailedPostDoesNotMarkBinariesDelivered(t *testing.T) {
	poster := &flakyPoster{}
	a := NewFromDeps(config.Config{Interval: time.Hour, RuntimeAllowedBinaries: []string{"/opt/l/llama-server"}}, Deps{Poster: poster})
	a.binaries.stat = statFake(fakeFileInfo{size: 1})
	a.binaries.runVersion = func(context.Context, string) (string, error) { return llamaVersionOutput, nil }
	a.binaries.rescan(context.Background())
	poster.fail.Store(true)
	a.collectOnce(context.Background())
	poster.fail.Store(false)
	a.collectOnce(context.Background())
	a.collectOnce(context.Background())
	if len(poster.samples) != 3 {
		t.Fatalf("samples = %d", len(poster.samples))
	}
	if poster.samples[0].Binaries == nil || poster.samples[1].Binaries == nil {
		t.Fatal("inventory not resent after a failed post")
	}
	if poster.samples[2].Binaries != nil {
		t.Fatal("inventory resent after a successful post")
	}
}

// TestFailedVersionProbeRetriedWithBackoff: a --version run that yields no
// version (a deadline under load, a failed exec) is not cached for the file's
// size and mtime for good: it is run again once its backoff has passed --
// binaryRetryFirst after the first failure, doubling with each further one --
// and a run that succeeds is cached for good.
func TestFailedVersionProbeRetriedWithBackoff(t *testing.T) {
	runs := 0
	fail := true
	run := func(context.Context, string) (string, error) {
		runs++
		if fail {
			return "", context.DeadlineExceeded
		}
		return llamaVersionOutput, nil
	}
	cache := map[string]cachedVersion{}
	stat := statFake(fakeFileInfo{size: 10, mtime: time.Unix(100, 0)})
	paths := []string{"/opt/l/llama-server"}
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	scan := func(at time.Time) []BinaryInfo {
		return scanBinariesCtx(context.Background(), at, paths, stat, run, cache)
	}

	if got := scan(t0); got[0].Version != "" || runs != 1 {
		t.Fatalf("first scan: %+v, runs %d", got, runs)
	}
	scan(t0.Add(binaryRetryFirst - time.Second))
	if runs != 1 {
		t.Fatalf("runs = %d inside the first backoff, want 1", runs)
	}
	scan(t0.Add(binaryRetryFirst))
	if runs != 2 {
		t.Fatalf("runs = %d once the first backoff passed, want 2", runs)
	}
	second := t0.Add(binaryRetryFirst)
	scan(second.Add(2*binaryRetryFirst - time.Second))
	if runs != 2 {
		t.Fatalf("runs = %d inside the doubled backoff, want 2", runs)
	}
	fail = false
	if got := scan(second.Add(2 * binaryRetryFirst)); got[0].Version != "0.9.0-dev" || runs != 3 {
		t.Fatalf("scan after the doubled backoff: %+v, runs %d", got, runs)
	}
	scan(second.Add(24 * time.Hour))
	if runs != 3 {
		t.Fatalf("runs = %d after a success, want it cached", runs)
	}
}

// TestFailedVersionProbeBackoffIsCapped: the backoff never exceeds
// binaryRetryCap, however often the run fails.
func TestFailedVersionProbeBackoffIsCapped(t *testing.T) {
	runs := 0
	run := func(context.Context, string) (string, error) { runs++; return "", errors.New("boom") }
	cache := map[string]cachedVersion{}
	stat := statFake(fakeFileInfo{size: 10, mtime: time.Unix(100, 0)})
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for range 20 {
		scanBinariesCtx(context.Background(), at, []string{"/opt/l/llama-server"}, stat, run, cache)
		at = at.Add(binaryRetryCap)
	}
	if runs != 20 {
		t.Fatalf("runs = %d over 20 scans binaryRetryCap apart, want 20", runs)
	}
}

// TestInventoryNextRetry: the inventory names the earliest pending retry, so
// its loop rescans then rather than at the next full interval.
func TestInventoryNextRetry(t *testing.T) {
	b := newBinaryInventory([]string{"/opt/l/llama-server"})
	b.stat = statFake(fakeFileInfo{size: 1})
	b.runVersion = func(context.Context, string) (string, error) { return "", errors.New("boom") }
	before := time.Now()
	b.rescan(context.Background())
	next, ok := b.nextRetry()
	if !ok || next.Before(before.Add(binaryRetryFirst)) || next.After(time.Now().Add(binaryRetryFirst)) {
		t.Fatalf("nextRetry = %v, %v; want about %v from the scan", next, ok, binaryRetryFirst)
	}
	b.runVersion = func(context.Context, string) (string, error) { return llamaVersionOutput, nil }
	b.cache = map[string]cachedVersion{}
	b.rescan(context.Background())
	if _, ok := b.nextRetry(); ok {
		t.Fatal("nextRetry reported a retry with every probe succeeded")
	}
}

// TestInventoryResendOnRequest: an inventory already delivered is attached
// again after resend, though nothing changed: a frame the transport wrote but
// the gateway never stored is not lost for good.
func TestInventoryResendOnRequest(t *testing.T) {
	poster := &capturePoster{}
	a := NewFromDeps(config.Config{Interval: time.Hour, RuntimeAllowedBinaries: []string{"/opt/l/llama-server"}}, Deps{Poster: poster})
	a.binaries.stat = statFake(fakeFileInfo{size: 1})
	a.binaries.runVersion = func(context.Context, string) (string, error) { return llamaVersionOutput, nil }
	a.binaries.rescan(context.Background())
	a.collectOnce(context.Background())
	a.collectOnce(context.Background())
	if poster.last().Binaries != nil {
		t.Fatal("unchanged inventory resent without a request")
	}
	a.binaries.resend()
	a.collectOnce(context.Background())
	if b := poster.last().Binaries; b == nil || len(*b) != 1 {
		t.Fatalf("inventory after resend = %+v, want it attached again", b)
	}
	a.collectOnce(context.Background())
	if poster.last().Binaries != nil {
		t.Fatal("inventory attached again after the resend was delivered")
	}
}

// TestInventoryResentOnReconnectAndPeriodically: Run attaches the unchanged
// inventory again when the transport reconnects (a nil runtime wake) and on
// the system-report cadence.
func TestInventoryResentOnReconnectAndPeriodically(t *testing.T) {
	for _, tc := range []struct {
		name   string
		report time.Duration
		wake   bool
	}{
		{"reconnect", time.Hour, true},
		{"periodic", 30 * time.Millisecond, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			poster := &runtimeWakePoster{wake: make(chan json.RawMessage, 1)}
			cfg := config.Config{Interval: 5 * time.Millisecond, SystemReportInterval: tc.report, RuntimeAllowedBinaries: []string{"/opt/l/llama-server"}}
			a := NewFromDeps(cfg, Deps{Poster: poster})
			a.binaries.stat = statFake(fakeFileInfo{size: 1})
			a.binaries.runVersion = func(context.Context, string) (string, error) { return llamaVersionOutput, nil }
			withInventory := func() int {
				poster.mu.Lock()
				defer poster.mu.Unlock()
				n := 0
				for _, s := range poster.samples {
					if s.Binaries != nil {
						n++
					}
				}
				return n
			}

			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() {
				defer close(done)
				_ = a.Run(ctx)
			}()
			defer func() { cancel(); <-done }()

			waitUntil(t, 2*time.Second, func() bool { return withInventory() == 1 })
			if tc.wake {
				time.Sleep(50 * time.Millisecond)
				if n := withInventory(); n != 1 {
					t.Fatalf("inventory attached %d times without a reconnect, want 1", n)
				}
				poster.wake <- nil
			}
			waitUntil(t, 2*time.Second, func() bool { return withInventory() >= 2 })
		})
	}
}

// TestFailedProbeThenFileGoneLeavesNoRetry: a binary whose --version run
// failed and which then disappears (deleted, turned into a directory, or no
// longer stat-able) leaves no pending retry behind: nothing could ever run it
// again, and a retry time in the past would make the rescan loop spin.
func TestFailedProbeThenFileGoneLeavesNoRetry(t *testing.T) {
	for name, gone := range map[string]func(string) (fs.FileInfo, error){
		"deleted":   func(string) (fs.FileInfo, error) { return nil, fs.ErrNotExist },
		"directory": statFake(fakeFileInfo{mode: fs.ModeDir}),
	} {
		t.Run(name, func(t *testing.T) {
			b := newBinaryInventory([]string{"/opt/l/llama-server"})
			b.stat = statFake(fakeFileInfo{size: 1})
			b.runVersion = func(context.Context, string) (string, error) { return "", errors.New("boom") }
			b.rescan(context.Background())
			if _, ok := b.nextRetry(); !ok {
				t.Fatal("no retry pending after a failed probe")
			}
			b.stat = gone
			b.rescan(context.Background())
			if next, ok := b.nextRetry(); ok {
				t.Fatalf("nextRetry = %v after the binary disappeared, want none", next)
			}
		})
	}
}

// TestNextRetryIgnoresPathsNoLongerAllowed: a failed entry for a path the
// allowlist no longer names is never rescanned, so it schedules nothing.
func TestNextRetryIgnoresPathsNoLongerAllowed(t *testing.T) {
	b := newBinaryInventory([]string{"/opt/l/llama-server"})
	b.cache["/opt/old/llama-server"] = cachedVersion{retryAt: time.Now().Add(-time.Hour), failures: 1}
	if next, ok := b.nextRetry(); ok {
		t.Fatalf("nextRetry = %v for a path outside the allowlist, want none", next)
	}
}

// TestUntilNextScanHasAFloor: a retry already due never makes the rescan loop
// wait less than a second.
func TestUntilNextScanHasAFloor(t *testing.T) {
	b := newBinaryInventory([]string{"/opt/l/llama-server"})
	b.cache["/opt/l/llama-server"] = cachedVersion{retryAt: time.Now().Add(-time.Hour), failures: 1}
	if got := b.untilNextScan(); got < time.Second {
		t.Fatalf("untilNextScan = %v with a retry overdue, want at least 1s", got)
	}
}
