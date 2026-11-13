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

func TestOnlyVersionedKindsAreExecuted(t *testing.T) {
	var ran []string
	run := func(_ context.Context, p string) (string, error) { ran = append(ran, p); return "", errors.New("boom") }
	got := scanBinaries([]string{"/a/rpc-server", "/a/ggml-rpc-server.exe", "/a/LLAMA-SERVER.EXE", "/a/other", "/a/vllm"},
		statFake(fakeFileInfo{}), run, map[string]cachedVersion{})
	if len(ran) != 2 || ran[0] != "/a/LLAMA-SERVER.EXE" || ran[1] != "/a/vllm" {
		t.Fatalf("executed %v", ran)
	}
	if got[0].Kind != "rpc_server" || got[1].Kind != "rpc_server" || got[2].Kind != "llama_server" || got[4].Kind != "vllm" {
		t.Fatalf("kinds: %+v", got)
	}
}

func TestBinaryKindVLLM(t *testing.T) {
	for p, want := range map[string]string{
		"/opt/venv/bin/vllm":                           "vllm",
		`C:\x\vllm.exe`:                                "vllm",
		"wsl://Ubuntu-24.04/home/ai/venv/bin/vllm":     "vllm",
		"wsl://Ubuntu-24.04/opt/llama/llama-server":    "llama_server",
		"wsl://Ubuntu-24.04/opt/llama/ggml-rpc-server": "rpc_server",
	} {
		if got := binaryKind(p); got != want {
			t.Errorf("%s: %s, want %s", p, got, want)
		}
	}
}

func TestParseVLLMVersion(t *testing.T) {
	cases := []struct {
		in, want string
		ok       bool
	}{
		{"0.11.0\n", "0.11.0", true},
		{"INFO 11-13 10:00:00 [__init__.py:216] Automatically detected platform cuda.\n0.11.1rc2.dev123+gabc1234\n", "0.11.1rc2.dev123+gabc1234", true},
		{"v0.6.3.post1\nWARNING something\n", "0.6.3.post1", true},
		{"0.9.9\nINFO another line\n1.0.0\nWARNING tail\n", "1.0.0", true}, // two version-shaped lines: the last wins
		{"Traceback ...\nModuleNotFoundError: No module named 'vllm'\n", "", false},
	}
	for _, c := range cases {
		got, ok := parseVLLMVersion(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("%q: %q %v, want %q %v", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestNativeVLLMIsProbedWithLongTimeout(t *testing.T) {
	var gotArgs []string
	var left time.Duration
	run := func(ctx context.Context, p string) (string, error) {
		gotArgs = append(gotArgs, p)
		dl, _ := ctx.Deadline()
		left = time.Until(dl)
		return "0.11.0\n", nil
	}
	got := scanBinaries([]string{"/opt/venv/bin/vllm"}, statFake(fakeFileInfo{size: 1}), run, map[string]cachedVersion{})
	if len(gotArgs) != 1 || left < 50*time.Second {
		t.Fatalf("ran %v, deadline in %v", gotArgs, left)
	}
	if got[0].Kind != "vllm" || got[0].Version != "0.11.0" || got[0].VersionSource != "agent" || got[0].Build != "" {
		t.Fatalf("row: %+v", got[0])
	}
}

// fakeWSL answers wsl.exe calls by their argv and records them.
type fakeWSL struct {
	running string
	version map[string]string // linux path -> --version output
	stat    string
	calls   [][]string
	envs    [][]string
	failing func(args string) error // an error to answer a call with, nil to answer it
}

// exitErr is an error carrying a process exit status.
type exitErr int

func (e exitErr) Error() string { return "exit status " + strconv.Itoa(int(e)) }
func (e exitErr) ExitCode() int { return int(e) }

func (f *fakeWSL) call(_ context.Context, _ string, args, env []string, _ bool) ([]byte, error) {
	f.calls = append(f.calls, args)
	f.envs = append(f.envs, env)
	if f.failing != nil {
		if err := f.failing(strings.Join(args, " ")); err != nil {
			return nil, err
		}
	}
	if strings.Join(args, " ") == "--list --running --quiet" {
		return []byte(f.running), nil
	}
	for i, a := range args {
		switch a {
		case "stat":
			return []byte(f.stat), nil
		case "--version":
			return []byte(f.version[args[i-1]]), nil
		}
	}
	return nil, errors.New("unexpected call")
}

func (f *fakeWSL) joined() []string {
	var out []string
	for _, c := range f.calls {
		out = append(out, strings.Join(c, " "))
	}
	return out
}

func wslInventory(f *fakeWSL, cfg wslProbeConfig, allowed ...string) *binaryInventory {
	b := newBinaryInventory(allowed, cfg)
	b.wslRun = f.call
	return b
}

const (
	wslVLLM  = "wsl://Ubuntu-24.04/home/ai/venv/bin/vllm"
	wslLlama = "wsl://Debian/opt/llama/llama-server"
)

func TestWSLEntryProbedOnlyWhenDistroRuns(t *testing.T) {
	f := &fakeWSL{running: "Ubuntu-24.04\r\n", stat: "100:200:regular file\n", version: map[string]string{"/home/ai/venv/bin/vllm": "INFO log\n0.11.0\n"}}
	b := wslInventory(f, wslProbeConfig{Exe: "wsl.exe", User: "ai"}, wslVLLM, wslLlama)
	b.rescan(context.Background())
	want := []string{
		"--list --running --quiet",
		"-d Ubuntu-24.04 -u ai --cd / --exec stat -L -c %s:%Y:%F -- /home/ai/venv/bin/vllm",
		"-d Ubuntu-24.04 -u ai --cd / --exec /home/ai/venv/bin/vllm --version",
	}
	if got := f.joined(); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("calls:\n%s", strings.Join(got, "\n"))
	}
	for _, c := range f.joined() {
		if strings.Contains(c, "Debian") {
			t.Fatalf("stopped distro addressed: %s", c)
		}
	}
	for i, env := range f.envs {
		if !containsStr(env, "WSL_UTF8=1") {
			t.Fatalf("call %d env lacks WSL_UTF8: %v", i, env)
		}
	}
	list, _ := b.pending()
	if list == nil || len(*list) != 2 {
		t.Fatalf("inventory: %+v", list)
	}
	r := (*list)[0]
	if r.Path != wslVLLM || r.Kind != "vllm" || r.Version != "0.11.0" || r.VersionSource != "agent" {
		t.Fatalf("ubuntu row: %+v", r)
	}
	if d := (*list)[1]; d.Path != wslLlama || d.Version != "" {
		t.Fatalf("debian row: %+v", d)
	}
}

func containsStr(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

func TestWSLStoppedDistroKeepsLastVersion(t *testing.T) {
	f := &fakeWSL{running: "Ubuntu-24.04\n", stat: "100:200:regular file\n", version: map[string]string{"/home/ai/venv/bin/vllm": "0.11.0\n"}}
	b := wslInventory(f, wslProbeConfig{Exe: "wsl.exe", User: "ai"}, wslVLLM)
	b.rescan(context.Background())
	f.running, f.calls = "", nil
	b.rescan(context.Background())
	if got := f.joined(); len(got) != 1 || got[0] != "--list --running --quiet" {
		t.Fatalf("calls after stop: %v", got)
	}
	list, _ := b.pending()
	if r := (*list)[0]; r.Version != "0.11.0" || r.VersionSource != "agent" {
		t.Fatalf("row: %+v", r)
	}
}

func TestWSLEntriesNotProbedWithoutExeOrUser(t *testing.T) {
	for name, cfg := range map[string]wslProbeConfig{
		"no exe":    {User: "ai"},
		"root user": {Exe: "wsl.exe", User: "root"},
		"no user":   {Exe: "wsl.exe"},
	} {
		t.Run(name, func(t *testing.T) {
			f := &fakeWSL{running: "Ubuntu-24.04\n"}
			b := wslInventory(f, cfg, wslVLLM)
			b.rescan(context.Background())
			if len(f.calls) != 0 {
				t.Fatalf("calls: %v", f.joined())
			}
			list, _ := b.pending()
			if len(*list) != 1 || (*list)[0].Path != wslVLLM {
				t.Fatalf("rows: %+v", list)
			}
		})
	}
}

func TestMalformedWSLEntryNotListed(t *testing.T) {
	f := &fakeWSL{}
	b := wslInventory(f, wslProbeConfig{Exe: "wsl.exe", User: "ai"}, "WSL://Ubuntu-24.04/x", "wsl://Ubuntu-24.04\\x", "wsl:/Ubuntu/x", "/opt/l/other")
	b.rescan(context.Background())
	list, _ := b.pending()
	if len(*list) != 1 || (*list)[0].Path != "/opt/l/other" {
		t.Fatalf("rows: %+v", *list)
	}
	if len(f.calls) != 0 {
		t.Fatalf("calls: %v", f.joined())
	}
}

func TestWSLStatCacheSkipsSecondProbe(t *testing.T) {
	f := &fakeWSL{running: "Ubuntu-24.04\n", stat: "100:200:regular file\n", version: map[string]string{"/home/ai/venv/bin/vllm": "0.11.0\n"}}
	b := wslInventory(f, wslProbeConfig{Exe: "wsl.exe", User: "ai"}, wslVLLM)
	b.rescan(context.Background())
	b.rescan(context.Background())
	n := 0
	for _, c := range f.joined() {
		if strings.HasSuffix(c, "--version") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("--version ran %d times: %v", n, f.joined())
	}
}

func TestWSLRPCServerInheritsSiblingPerDistro(t *testing.T) {
	f := &fakeWSL{running: "Ubuntu-24.04\nDebian\n", stat: "1:2:regular file\n", version: map[string]string{"/opt/l/llama-server": llamaVersionOutput}}
	b := wslInventory(f, wslProbeConfig{Exe: "wsl.exe", User: "ai"},
		"wsl://Ubuntu-24.04/opt/l/llama-server", "wsl://Ubuntu-24.04/opt/l/rpc-server", "wsl://Debian/opt/l/rpc-server")
	b.rescan(context.Background())
	list, _ := b.pending()
	if (*list)[1].Version != "0.9.0-dev" {
		t.Fatalf("same-distro rpc: %+v", (*list)[1])
	}
	if (*list)[2].Version != "" {
		t.Fatalf("other-distro rpc inherited: %+v", (*list)[2])
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
	b := newBinaryInventory([]string{"/opt/l/llama-server"}, wslProbeConfig{})
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
			b := newBinaryInventory([]string{"/opt/l/llama-server"}, wslProbeConfig{})
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
	b := newBinaryInventory([]string{"/opt/l/llama-server"}, wslProbeConfig{})
	b.cache["/opt/old/llama-server"] = cachedVersion{retryAt: time.Now().Add(-time.Hour), failures: 1}
	if next, ok := b.nextRetry(); ok {
		t.Fatalf("nextRetry = %v for a path outside the allowlist, want none", next)
	}
}

// TestUntilNextScanHasAFloor: a retry already due never makes the rescan loop
// wait less than a second.
func TestUntilNextScanHasAFloor(t *testing.T) {
	b := newBinaryInventory([]string{"/opt/l/llama-server"}, wslProbeConfig{})
	b.cache["/opt/l/llama-server"] = cachedVersion{retryAt: time.Now().Add(-time.Hour), failures: 1}
	if got := b.untilNextScan(); got < time.Second {
		t.Fatalf("untilNextScan = %v with a retry overdue, want at least 1s", got)
	}
}

func TestWSLStoppedDistroKeepsFailedBackoff(t *testing.T) {
	f := &fakeWSL{running: "Ubuntu-24.04\n", stat: "100:200:regular file\n", version: map[string]string{"/home/ai/venv/bin/vllm": "garbage\n"}}
	b := wslInventory(f, wslProbeConfig{Exe: "wsl.exe", User: "ai"}, wslVLLM)
	b.rescan(context.Background())
	if cv := b.cache[wslVLLM]; cv.ok || cv.failures != 1 {
		t.Fatalf("after failed probe: %+v", cv)
	}
	f.running = ""
	before := time.Now()
	b.rescan(context.Background())
	cv, ok := b.cache[wslVLLM]
	if !ok || cv.failures != 1 {
		t.Fatalf("entry lost or reset while the distro is stopped: %+v %v", cv, ok)
	}
	if next, ok := b.nextRetry(); !ok || next.Before(before.Add(binaryRescanInterval)) {
		t.Fatalf("nextRetry %v is earlier than a rescan interval from now", next)
	}
}

func TestWSLStatFailureKeepsVersionUnlessMissing(t *testing.T) {
	statFails := func(err error) func(string) error {
		return func(a string) error {
			if strings.Contains(a, " stat ") {
				return err
			}
			return nil
		}
	}
	for name, tc := range map[string]struct {
		err      error
		wantKept bool
	}{
		"wsl failure keeps":   {exitErr(-1), true},
		"other status keeps":  {exitErr(2), true},
		"plain error keeps":   {errors.New("timeout"), true},
		"status 1 is missing": {exitErr(1), false},
	} {
		t.Run(name, func(t *testing.T) {
			f := &fakeWSL{running: "Ubuntu-24.04\n", stat: "100:200:regular file\n", version: map[string]string{"/home/ai/venv/bin/vllm": "0.11.0\n"}}
			b := wslInventory(f, wslProbeConfig{Exe: "wsl.exe", User: "ai"}, wslVLLM)
			b.rescan(context.Background())
			f.failing = statFails(tc.err)
			b.rescan(context.Background())
			list, _ := b.pending()
			r := (*list)[0]
			if kept := r.Version == "0.11.0"; kept != tc.wantKept {
				t.Fatalf("row %+v, wantKept %v", r, tc.wantKept)
			}
			if _, cached := b.cache[wslVLLM]; cached != tc.wantKept {
				t.Fatalf("cached = %v, want %v", cached, tc.wantKept)
			}
		})
	}
}

func TestWSLStatGarbageOutputKeepsVersion(t *testing.T) {
	f := &fakeWSL{running: "Ubuntu-24.04\n", stat: "100:200:regular file\n", version: map[string]string{"/home/ai/venv/bin/vllm": "0.11.0\n"}}
	b := wslInventory(f, wslProbeConfig{Exe: "wsl.exe", User: "ai"}, wslVLLM)
	b.rescan(context.Background())
	f.stat = "warning: something odd\n"
	b.rescan(context.Background())
	list, _ := b.pending()
	if r := (*list)[0]; r.Version != "0.11.0" {
		t.Fatalf("row %+v", r)
	}
}

func TestWSLListFailureAddressesNoDistroAndKeepsVersion(t *testing.T) {
	f := &fakeWSL{running: "Ubuntu-24.04\n", stat: "100:200:regular file\n", version: map[string]string{"/home/ai/venv/bin/vllm": "0.11.0\n"}}
	b := wslInventory(f, wslProbeConfig{Exe: "wsl.exe", User: "ai"}, wslVLLM)
	b.rescan(context.Background())
	f.calls = nil
	f.failing = func(a string) error {
		if strings.HasPrefix(a, "--list") {
			return exitErr(1)
		}
		return nil
	}
	b.rescan(context.Background())
	if got := f.joined(); len(got) != 1 || got[0] != "--list --running --quiet" {
		t.Fatalf("calls: %v", got)
	}
	list, _ := b.pending()
	if r := (*list)[0]; r.Version != "0.11.0" || r.VersionSource != "agent" {
		t.Fatalf("row %+v", r)
	}
}

func TestParseRunningDistros(t *testing.T) {
	utf16 := "\xff\xfeU\x00b\x00u\x00n\x00t\x00u\x00\r\x00\n\x00D\x00e\x00b\x00i\x00a\x00n\x00\r\x00\n\x00"
	for _, tc := range []struct {
		name string
		in   string
		want []string
	}{
		{"plain", "Ubuntu-24.04\nDebian\n", []string{"ubuntu-24.04", "debian"}},
		{"crlf and blanks", "Ubuntu-24.04\r\n\r\n  \r\nDebian\r\n", []string{"ubuntu-24.04", "debian"}},
		{"bom", "\uFEFFUbuntu-24.04\n", []string{"ubuntu-24.04"}},
		{"case folded", "UBUNTU-24.04\n", []string{"ubuntu-24.04"}},
		{"utf16 as bytes", utf16, []string{"ubuntu", "debian"}},
		{"empty", "", nil},
	} {
		got := parseRunningDistros([]byte(tc.in))
		if len(got) != len(tc.want) {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
		for _, n := range tc.want {
			if !got[n] {
				t.Errorf("%s: %q missing from %v", tc.name, n, got)
			}
		}
	}
}
