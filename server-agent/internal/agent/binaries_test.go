// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"op-ai-server-agent/internal/config"
	"os"
	"testing"
	"time"
)

const stage0Output = "version: 0.9.0-dev (build 1, commit 2bc46ba)\nbuilt with AppleClang 21.0.0.21000401 for Darwin arm64\n"

type fakeFileInfo struct {
	size  int64
	mtime time.Time
}

func (f fakeFileInfo) Name() string       { return "x" }
func (f fakeFileInfo) Size() int64        { return f.size }
func (f fakeFileInfo) Mode() fs.FileMode  { return 0o755 }
func (f fakeFileInfo) ModTime() time.Time { return f.mtime }
func (f fakeFileInfo) IsDir() bool        { return false }
func (f fakeFileInfo) Sys() any           { return nil }

func statFake(fi fakeFileInfo) func(string) (fs.FileInfo, error) {
	return func(string) (fs.FileInfo, error) { return fi, nil }
}

func TestParseLlamaVersionStage0Output(t *testing.T) {
	v, b, c, ok := parseLlamaVersion(stage0Output)
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
		return stage0Output, nil
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
	run := func(context.Context, string) (string, error) { runs++; return stage0Output, nil }
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
	out := stage0Output
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
