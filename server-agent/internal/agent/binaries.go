// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"io/fs"
	"log/slog"
	"op-ai-server-agent/internal/sample"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"
)

const (
	binaryKindLlamaServer = "llama_server"
	binaryKindRPCServer   = "rpc_server"
	binaryKindOther       = "other"

	versionSourceAgent = "agent"
	versionSourcePath  = "path"

	// binaryRescanInterval is how often the inventory is recomputed; the
	// (path, size, mtime) cache keeps an unchanged binary from being executed
	// again, so a rescan of an untouched host is a handful of stat calls.
	binaryRescanInterval = 5 * time.Minute
	// binaryVersionTimeout bounds one `--version` run.
	binaryVersionTimeout = 10 * time.Second
	// binaryOutputLimit caps the captured --version output; the version line
	// is at the top and a runaway child must not fill memory.
	binaryOutputLimit = 64 << 10
	// binaryRetryFirst is how long after a `--version` run that yielded no
	// version the binary is run again; each further failure doubles the wait,
	// up to binaryRetryCap.
	binaryRetryFirst = 30 * time.Second
	binaryRetryCap   = time.Hour
	// binaryRescanGap is the least the rescan loop ever waits, whatever
	// retry is due: a retry time that stays in the past must not make it
	// spin.
	binaryRescanGap = time.Second
)

// binaryWaitDelay bounds how long a --version run may linger after its
// context ends: CommandContext kills only the direct child, and a wrapper
// whose grandchild keeps the output pipe open would otherwise block Wait (and
// with it the whole rescan) forever.
var binaryWaitDelay = 5 * time.Second

// limitedBuffer keeps the first binaryOutputLimit bytes and silently drops the
// rest, always reporting a full write so the child is never blocked on a pipe.
type limitedBuffer struct{ buf bytes.Buffer }

func (l *limitedBuffer) Write(p []byte) (int, error) {
	if room := binaryOutputLimit - l.buf.Len(); room > 0 {
		l.buf.Write(p[:min(room, len(p))])
	}
	return len(p), nil
}

// BinaryInfo is the wire form of one inventory entry.
type BinaryInfo = sample.BinaryInfo

// cachedVersion is a version run result valid for one (size, mtime) of a file.
// A run that yielded a version is final for those bytes. One that did not is
// retried after a backoff: it may have ended at the timeout on a loaded host
// (an agent starting next to busy GPUs), and a missing version withholds every
// automatic cluster that needs the binary, so it must not stick until the
// file changes. The backoff, binaryRetryFirst doubling up to binaryRetryCap,
// keeps a binary that never answers from burning the timeout every rescan.
type cachedVersion struct {
	size                   int64
	mtime                  time.Time
	version, build, commit string
	ok                     bool
	failures               int       // consecutive runs without a version
	retryAt                time.Time // when a failed run may be retried
}

// retryBackoff is the wait after the n-th consecutive failed run (n >= 1).
func retryBackoff(n int) time.Duration {
	d := binaryRetryFirst
	for i := 1; i < n && d < binaryRetryCap; i++ {
		d *= 2
	}
	return min(d, binaryRetryCap)
}

var llamaVersionLine = regexp.MustCompile(`(?m)^version:\s*(\S+)\s*\(build\s+(\S+?),\s*commit\s+([0-9A-Za-z]+)\)`)

// parseLlamaVersion extracts the version triple from the output of
// `llama-server --version`, e.g. "version: 0.9.0-dev (build 1, commit 2bc46ba)".
func parseLlamaVersion(out string) (version, build, commit string, ok bool) {
	m := llamaVersionLine.FindStringSubmatch(out)
	if m == nil {
		return "", "", "", false
	}
	return m[1], m[2], m[3], true
}

var (
	buildTagToken = regexp.MustCompile(`^[bB](\d+)$`)
	commitToken   = regexp.MustCompile(`^[0-9a-fA-F]{7,40}$`)
	hexLetter     = regexp.MustCompile(`[a-fA-F]`)
)

// versionFromPath reads a build tag (b<digits>) and a commit (a 7-40 hex path
// segment containing at least one a-f letter, so a plain number or date never
// passes for a commit) out of an install path such as
// /opt/llama.cpp/b1234/ggml-rpc-server. Either part may be absent.
func versionFromPath(path string) (build, commit string, ok bool) {
	tokens := strings.FieldsFunc(path, func(r rune) bool {
		return r == '/' || r == '\\' || r == '-' || r == '_' || r == '.' || r == ' '
	})
	for _, tok := range tokens {
		if m := buildTagToken.FindStringSubmatch(tok); m != nil && build == "" {
			build = m[1]
		} else if commitToken.MatchString(tok) && hexLetter.MatchString(tok) && commit == "" {
			commit = strings.ToLower(tok)
		}
	}
	return build, commit, build != "" || commit != ""
}

// binaryKind classifies a binary by its file name alone.
func binaryKind(path string) string {
	name := strings.ToLower(filepath.Base(strings.ReplaceAll(path, "\\", "/")))
	name = strings.TrimSuffix(name, ".exe")
	switch name {
	case "llama-server":
		return binaryKindLlamaServer
	case "ggml-rpc-server", "rpc-server":
		return binaryKindRPCServer
	}
	return binaryKindOther
}

// scanBinaries computes the inventory of the allowed binaries. Only a
// llama_server is ever executed (with --version); an rpc_server takes the
// version of a llama_server in the same directory, else whatever its path
// says, and any other binary is listed without a version.
func scanBinaries(allowed []string, stat func(string) (fs.FileInfo, error), runVersion func(ctx context.Context, path string) (string, error), cache map[string]cachedVersion) []BinaryInfo {
	return scanBinariesCtx(context.Background(), time.Now(), allowed, stat, runVersion, cache)
}

// scanBinariesCtx is scanBinaries at the time now (which decides whether a
// failed version run is due again), ending early when ctx does.
func scanBinariesCtx(ctx context.Context, now time.Time, allowed []string, stat func(string) (fs.FileInfo, error), runVersion func(ctx context.Context, path string) (string, error), cache map[string]cachedVersion) []BinaryInfo {
	out := make([]BinaryInfo, 0, len(allowed))
	byDir := map[string]int{} // directory -> index of its first agent-versioned llama_server
	seen := map[string]bool{}
	for _, p := range allowed {
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		bi := BinaryInfo{Path: p, Kind: binaryKind(p), Allowed: true}
		fi, err := stat(p)
		if err != nil || !fi.Mode().IsRegular() {
			// Nothing here can be run: forget any cached run, so a failed
			// one leaves no retry behind that could never happen.
			delete(cache, p)
		}
		if err == nil && !fi.Mode().IsRegular() {
			// With runtime_allow_binary_dirs an entry may name a directory;
			// it is not expanded and is not a binary.
			continue
		}
		if bi.Kind == binaryKindLlamaServer {
			if err == nil && ctx.Err() == nil {
				cv, hit := cache[p]
				same := hit && cv.size == fi.Size() && cv.mtime.Equal(fi.ModTime())
				if !same || (!cv.ok && !now.Before(cv.retryAt)) {
					failures := 0
					if same {
						failures = cv.failures
					}
					cv = cachedVersion{size: fi.Size(), mtime: fi.ModTime()}
					rctx, cancel := context.WithTimeout(ctx, binaryVersionTimeout)
					text, rerr := runVersion(rctx, p)
					cancel()
					if ctx.Err() != nil {
						// A shutdown mid-run says nothing about the binary.
						return out
					}
					if rerr == nil || text != "" {
						cv.version, cv.build, cv.commit, cv.ok = parseLlamaVersion(text)
					}
					if !cv.ok {
						cv.failures = failures + 1
						cv.retryAt = now.Add(retryBackoff(cv.failures))
					}
					if cache != nil {
						cache[p] = cv
					}
				}
				if cv.ok {
					bi.Version, bi.Build, bi.Commit, bi.VersionSource = cv.version, cv.build, cv.commit, versionSourceAgent
					if _, dup := byDir[filepath.Dir(p)]; !dup {
						byDir[filepath.Dir(p)] = len(out)
					}
				}
			}
			if bi.VersionSource == "" {
				if b, c, ok := versionFromPath(p); ok {
					bi.Build, bi.Commit, bi.VersionSource = b, c, versionSourcePath
				}
			}
		}
		out = append(out, bi)
	}
	for i := range out {
		if out[i].Kind != binaryKindRPCServer {
			continue
		}
		if j, ok := byDir[filepath.Dir(out[i].Path)]; ok {
			s := out[j]
			out[i].Version, out[i].Build, out[i].Commit, out[i].VersionSource = s.Version, s.Build, s.Commit, versionSourceAgent
		} else if b, c, ok := versionFromPath(out[i].Path); ok {
			out[i].Build, out[i].Commit, out[i].VersionSource = b, c, versionSourcePath
		}
	}
	return out
}

// minimalEnv carries only what a binary needs to find its shared libraries;
// nothing from the agent's environment that could hold a secret.
func minimalEnv() []string {
	keys := []string{"PATH", "HOME", "TMPDIR", "LD_LIBRARY_PATH", "DYLD_LIBRARY_PATH"}
	if runtime.GOOS == "windows" {
		keys = append(keys, "SystemRoot", "windir", "TEMP", "TMP", "USERPROFILE")
	}
	var env []string
	for _, k := range keys {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	return env
}

// execVersion runs `<path> --version` directly (no shell) and returns the
// combined output.
func execVersion(ctx context.Context, path string) (string, error) {
	cmd := exec.CommandContext(ctx, path, "--version")
	cmd.Env = minimalEnv()
	cmd.WaitDelay = binaryWaitDelay
	var buf limitedBuffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	err := cmd.Run()
	return buf.buf.String(), err
}

// binaryInventory keeps the last computed inventory and decides whether a
// sample has to carry it.
type binaryInventory struct {
	allowed    []string
	stat       func(string) (fs.FileInfo, error)
	runVersion func(ctx context.Context, path string) (string, error)

	scanMu sync.Mutex // one scan at a time; guards cache
	cache  map[string]cachedVersion

	mu       sync.Mutex
	ready    bool
	current  []BinaryInfo
	hash     [32]byte
	sentOnce bool
	sentHash [32]byte
}

func newBinaryInventory(allowed []string) *binaryInventory {
	return &binaryInventory{
		allowed:    allowed,
		stat:       os.Stat,
		runVersion: execVersion,
		cache:      map[string]cachedVersion{},
	}
}

// rescan recomputes the inventory and stores it.
func (b *binaryInventory) rescan(ctx context.Context) {
	b.scanMu.Lock()
	list := scanBinariesCtx(ctx, time.Now(), b.allowed, b.stat, b.runVersion, b.cache)
	b.scanMu.Unlock()
	if ctx.Err() != nil {
		return
	}
	raw, _ := json.Marshal(list)
	b.mu.Lock()
	b.current, b.hash, b.ready = list, sha256.Sum256(raw), true
	b.mu.Unlock()
}

// nextRetry is the earliest time a failed version run of an allowed path is
// due again, and whether any is pending.
func (b *binaryInventory) nextRetry() (time.Time, bool) {
	b.scanMu.Lock()
	defer b.scanMu.Unlock()
	var next time.Time
	for _, p := range b.allowed {
		cv, ok := b.cache[p]
		if ok && !cv.ok && (next.IsZero() || cv.retryAt.Before(next)) {
			next = cv.retryAt
		}
	}
	return next, !next.IsZero()
}

// run rescans immediately and then every binaryRescanInterval until ctx ends,
// earlier when a failed version run is due again before that.
func (b *binaryInventory) run(ctx context.Context) {
	b.rescan(ctx)
	t := time.NewTimer(b.untilNextScan())
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			b.rescan(ctx)
			t.Reset(b.untilNextScan())
		}
	}
}

// untilNextScan is how long run waits for its next rescan: the rescan
// interval, shorter when a retry is due earlier, never below binaryRescanGap.
func (b *binaryInventory) untilNextScan() time.Duration {
	wait := binaryRescanInterval
	if next, ok := b.nextRetry(); ok {
		wait = min(wait, max(time.Until(next), binaryRescanGap))
	}
	return wait
}

// resend makes the next sample carry the inventory again, though it did not
// change since it was last delivered. A delivery is only the transport
// accepting the sample: a frame written to a connection that then dropped, or
// a sample the gateway accepted while its store write failed, never stored it.
// The agent calls it when its connection to the gateway is re-established and
// on the system-report cadence.
func (b *binaryInventory) resend() {
	b.mu.Lock()
	b.sentOnce = false
	b.mu.Unlock()
}

// pending returns the inventory to attach to the next sample, or nil when
// there is nothing new to say (no scan finished yet, or the hash equals the
// one last delivered). The returned func records the delivery; call it once
// the sample was accepted so a failed push resends.
func (b *binaryInventory) pending() (*[]BinaryInfo, func()) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.ready || (b.sentOnce && b.sentHash == b.hash) {
		return nil, func() { /* nothing attached: nothing to mark delivered */ }
	}
	list, h := append([]BinaryInfo{}, b.current...), b.hash
	return &list, func() {
		b.mu.Lock()
		b.sentOnce, b.sentHash = true, h
		b.mu.Unlock()
		slog.Debug("binary inventory reported", "binaries", len(list))
	}
}
