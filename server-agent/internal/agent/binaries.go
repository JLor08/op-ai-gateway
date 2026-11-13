// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	runtimectl "op-ai-server-agent/internal/runtime"
	"op-ai-server-agent/internal/sample"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	binaryKindLlamaServer = "llama_server"
	binaryKindRPCServer   = "rpc_server"
	binaryKindVLLM        = "vllm"
	binaryKindOther       = "other"

	versionSourceAgent = "agent"
	versionSourcePath  = "path"

	// binaryRescanInterval is how often the inventory is recomputed; the
	// (path, size, mtime) cache keeps an unchanged binary from being executed
	// again, so a rescan of an untouched host is a handful of stat calls.
	binaryRescanInterval = 5 * time.Minute
	// binaryVersionTimeout bounds one `--version` run.
	binaryVersionTimeout = 10 * time.Second
	// binaryVLLMVersionTimeout bounds a vLLM run: `vllm --version` imports
	// torch and probes the platform, which takes tens of seconds when cold.
	binaryVLLMVersionTimeout = 60 * time.Second
	// binaryWSLCallTimeout bounds the wsl.exe listing and stat calls.
	binaryWSLCallTimeout = 10 * time.Second
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

var vllmVersionLine = regexp.MustCompile(`^v?(\d+\.\d+(?:\.\d+)?[0-9A-Za-z.+-]*)$`)

// parseVLLMVersion extracts the version from the output of `vllm --version`.
// vLLM logs platform detection and warnings around it, so the last line shaped
// like a version wins.
func parseVLLMVersion(out string) (version string, ok bool) {
	lines := strings.Split(out, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if m := vllmVersionLine.FindStringSubmatch(strings.TrimSpace(lines[i])); m != nil {
			return m[1], true
		}
	}
	return "", false
}

// versionProbe is how a version-probed kind is run and read.
type versionProbe struct {
	timeout time.Duration
	parse   func(out string) (version, build, commit string, ok bool)
}

// probeFor is the version probe of kind, false for a kind that is never run.
func probeFor(kind string) (versionProbe, bool) {
	switch kind {
	case binaryKindLlamaServer:
		return versionProbe{binaryVersionTimeout, parseLlamaVersion}, true
	case binaryKindVLLM:
		return versionProbe{binaryVLLMVersionTimeout, func(out string) (string, string, string, bool) {
			v, ok := parseVLLMVersion(out)
			return v, "", "", ok
		}}, true
	}
	return versionProbe{}, false
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
	case "vllm":
		return binaryKindVLLM
	}
	return binaryKindOther
}

// prober reaches one execution context: the host itself, or one running WSL
// distro. Paths are those of that context.
type prober struct {
	stat       func(string) (fs.FileInfo, error)
	runVersion func(ctx context.Context, path string) (string, error)
}

// wslProbeConfig is what the agent needs to reach a distro: the wsl.exe to run
// and the user to run as. Either one unusable means WSL entries are listed but
// never probed.
type wslProbeConfig struct{ Exe, User string }

func (c wslProbeConfig) usable() bool {
	return c.Exe != "" && !runtimectl.WSLUserRefused(c.User)
}

// wslCall runs wsl.exe with args and env and returns its stdout, or, with
// combined, its stdout and stderr together (a --version run may print its
// version on either).
type wslCall func(ctx context.Context, exe string, args, env []string, combined bool) ([]byte, error)

// execWSLCall is the production wslCall.
func execWSLCall(ctx context.Context, exe string, args, env []string, combined bool) ([]byte, error) {
	cmd := exec.CommandContext(ctx, exe, args...) //nolint:gosec // exe is the operator's runtime_wsl_exe; args are built by the agent
	cmd.Env = env
	cmd.WaitDelay = binaryWaitDelay
	var buf limitedBuffer
	cmd.Stdout = &buf
	if combined {
		cmd.Stderr = &buf
	}
	err := cmd.Run()
	return buf.buf.Bytes(), err
}

// errWSLUnavailable marks a failed WSL stat that does not say the file is
// gone: the distro may have stopped since it was listed, or wsl.exe failed or
// timed out. The entry is then treated like one of a distro that is not
// running, and keeps its last good version.
var errWSLUnavailable = errors.New("wsl stat unavailable")

// wslStatMissing reports whether err is a stat that exited with status 1,
// which is how stat says the file does not exist.
func wslStatMissing(err error) bool {
	var ee interface{ ExitCode() int }
	return errors.As(err, &ee) && ee.ExitCode() == 1
}

// wslEnv is the environment of a wsl.exe call: the minimal env (wsl.exe needs
// SystemRoot and its siblings) and UTF-8 output.
func wslEnv() []string { return append(minimalEnv(), "WSL_UTF8=1") }

// wslFileInfo is what `stat -c %s:%Y:%F` says about a file in a distro.
type wslFileInfo struct {
	size  int64
	mtime time.Time
	mode  fs.FileMode
}

func (f wslFileInfo) Name() string       { return "" }
func (f wslFileInfo) Size() int64        { return f.size }
func (f wslFileInfo) Mode() fs.FileMode  { return f.mode }
func (f wslFileInfo) ModTime() time.Time { return f.mtime }
func (f wslFileInfo) IsDir() bool        { return f.mode.IsDir() }
func (f wslFileInfo) Sys() any           { return nil }

// parseWSLStat reads "<size>:<mtime-epoch>:<type>", the type being stat's
// %F text ("regular file", "regular empty file", "directory", ...).
func parseWSLStat(out string) (fs.FileInfo, error) {
	parts := strings.SplitN(strings.TrimSpace(out), ":", 3)
	if len(parts) != 3 {
		return nil, fs.ErrInvalid
	}
	size, err1 := strconv.ParseInt(parts[0], 10, 64)
	mtime, err2 := strconv.ParseInt(parts[1], 10, 64)
	if err1 != nil || err2 != nil {
		return nil, fs.ErrInvalid
	}
	fi := wslFileInfo{size: size, mtime: time.Unix(mtime, 0)}
	switch parts[2] {
	case "regular file", "regular empty file":
	case "directory":
		fi.mode = fs.ModeDir
	default:
		fi.mode = fs.ModeIrregular
	}
	return fi, nil
}

// wslProbes opens the running distros of one scan. WSL probing never boots the
// VM: the running list is asked once, and a distro that is not in it is never
// addressed.
type wslProbes struct {
	ctx    context.Context
	cfg    wslProbeConfig
	call   wslCall
	loaded bool
	up     map[string]bool // lowercased running distro names
}

// parseRunningDistros reads `wsl.exe --list --running --quiet` output: one
// name per line. A wsl.exe that ignores WSL_UTF8 answers in UTF-16LE, which
// reads as NUL-interleaved bytes, so every NUL and the BOM are dropped before
// splitting; CRs and blanks are trimmed per line.
func parseRunningDistros(out []byte) map[string]bool {
	up := map[string]bool{}
	clean := strings.NewReplacer("\x00", "", "\ufeff", "").Replace(string(out))
	for _, line := range strings.Split(clean, "\n") {
		if name := strings.TrimSpace(line); name != "" {
			up[strings.ToLower(name)] = true
		}
	}
	return up
}

// prober is the prober of distro, nil when it is not running or WSL cannot be
// probed at all.
func (w *wslProbes) prober(distro string) *prober {
	if w == nil || !w.cfg.usable() {
		return nil
	}
	if !w.loaded {
		w.loaded = true
		ctx, cancel := context.WithTimeout(w.ctx, binaryWSLCallTimeout)
		out, err := w.call(ctx, w.cfg.Exe, []string{"--list", "--running", "--quiet"}, wslEnv(), false)
		cancel()
		if err == nil {
			w.up = parseRunningDistros(out)
		}
	}
	if !w.up[strings.ToLower(distro)] {
		return nil
	}
	args := func(cmd ...string) []string {
		return runtimectl.WSLHelperArgs(w.cfg.User, distro, cmd...)
	}
	return &prober{
		stat: func(p string) (fs.FileInfo, error) {
			ctx, cancel := context.WithTimeout(w.ctx, binaryWSLCallTimeout)
			defer cancel()
			out, err := w.call(ctx, w.cfg.Exe, args("stat", "-L", "-c", "%s:%Y:%F", "--", p), wslEnv(), false)
			if err != nil {
				if wslStatMissing(err) {
					return nil, fs.ErrNotExist
				}
				return nil, errors.Join(errWSLUnavailable, err)
			}
			fi, perr := parseWSLStat(string(out))
			if perr != nil {
				return nil, errors.Join(errWSLUnavailable, perr)
			}
			return fi, nil
		},
		runVersion: func(ctx context.Context, p string) (string, error) {
			out, err := w.call(ctx, w.cfg.Exe, args(p, "--version"), wslEnv(), true)
			return string(out), err
		},
	}
}

// scanBinaries computes the inventory of the allowed native binaries. Only a
// llama_server or vllm is ever executed (with --version); an rpc_server takes
// the version of a llama_server in the same directory, else whatever its path
// says, and any other binary is listed without a version.
func scanBinaries(allowed []string, stat func(string) (fs.FileInfo, error), runVersion func(ctx context.Context, path string) (string, error), cache map[string]cachedVersion) []BinaryInfo {
	return scanBinariesCtx(context.Background(), time.Now(), allowed, stat, runVersion, cache)
}

// scanBinariesCtx is scanBinaries at the time now (which decides whether a
// failed version run is due again), ending early when ctx does.
func scanBinariesCtx(ctx context.Context, now time.Time, allowed []string, stat func(string) (fs.FileInfo, error), runVersion func(ctx context.Context, path string) (string, error), cache map[string]cachedVersion) []BinaryInfo {
	return scanAll(ctx, now, allowed, &prober{stat: stat, runVersion: runVersion}, nil, cache)
}

// scanAll computes the inventory of every allowed entry: native paths through
// native, wsl:// entries through the distro prober w opens (nil: listed, never
// probed). A string that looks like a WSL entry but is not a well-formed one
// is not listed.
func scanAll(ctx context.Context, now time.Time, allowed []string, native *prober, w *wslProbes, cache map[string]cachedVersion) []BinaryInfo {
	sc := &binaryScan{
		ctx: ctx, now: now, cache: cache,
		out:   make([]BinaryInfo, 0, len(allowed)),
		byDir: map[string]int{},
	}
	seen := map[string]bool{}
	for _, p := range allowed {
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		var ok bool
		switch distro, lp, isWSL := runtimectl.ParseWSLEntry(p); {
		case isWSL:
			ok = sc.add(p, lp, w.prober(distro))
		case strings.HasPrefix(strings.ToLower(p), "wsl:"):
			continue // malformed WSL entry: permits nothing, lists nothing
		default:
			ok = sc.add(p, p, native)
		}
		if !ok {
			return sc.out
		}
	}
	inheritSiblingVersions(sc.out, sc.byDir)
	return sc.out
}

// binaryScan is one scanAll pass: its inputs, and the inventory built so far.
type binaryScan struct {
	ctx   context.Context
	now   time.Time
	cache map[string]cachedVersion // keyed by the allowed entry

	out   []BinaryInfo
	byDir map[string]int // directory key -> index of its first agent-versioned llama_server
}

// add lists the allowed entry, versioned as far as the agent can tell. target
// is the path in its execution context and pr how to reach it (nil for a WSL entry whose distro is not
// to be addressed: it keeps its last good version). It reports false when a
// shutdown overtook the version run: that says nothing about the binary, and
// the scan ends without it.
func (sc *binaryScan) add(entry, target string, pr *prober) bool {
	bi := BinaryInfo{Path: entry, Kind: binaryKind(target), Allowed: true}
	probe, probed := probeFor(bi.Kind)
	var fi fs.FileInfo
	var err error
	if pr != nil {
		fi, err = pr.stat(target)
	}
	if pr == nil || errors.Is(err, errWSLUnavailable) {
		sc.addOffline(&bi, target, probed)
		sc.indexSibling(bi, bi.VersionSource == versionSourceAgent)
		sc.out = append(sc.out, bi)
		return true
	}
	if err != nil || !fi.Mode().IsRegular() {
		// Nothing here can be run: forget any cached run, so a failed
		// one leaves no retry behind that could never happen.
		delete(sc.cache, entry)
	}
	if err == nil && !fi.Mode().IsRegular() {
		// With runtime_allow_binary_dirs an entry may name a directory;
		// it is not expanded and is not a binary.
		return true
	}
	agentVersioned := false
	if probed {
		var aborted bool
		agentVersioned, aborted = sc.versionProbed(&bi, fi, err, pr, probe, target)
		if aborted {
			return false
		}
	}
	sc.indexSibling(bi, agentVersioned)
	sc.out = append(sc.out, bi)
	return true
}

// indexSibling records bi as the llama_server its directory's rpc_servers
// inherit from, when it is the first agent-versioned one there.
func (sc *binaryScan) indexSibling(bi BinaryInfo, agentVersioned bool) {
	if bi.Kind != binaryKindLlamaServer || !agentVersioned {
		return
	}
	dirKey := siblingDirKey(bi.Path)
	if _, dup := sc.byDir[dirKey]; !dup {
		sc.byDir[dirKey] = len(sc.out)
	}
}

// addOffline versions bi without running anything: a WSL entry whose distro is
// not running keeps the version of its last good run, else what its path says.
// Cache entries stay, so a binary that never answers keeps its backoff across
// distro restarts; a failed one is only pushed out by a rescan interval, since
// no retry can happen while the distro is away and a retry time in the past
// would make the rescan loop spin at its minimum gap.
func (sc *binaryScan) addOffline(bi *BinaryInfo, target string, probed bool) {
	cv, hit := sc.cache[bi.Path]
	switch {
	case hit && cv.ok:
		bi.Version, bi.Build, bi.Commit, bi.VersionSource = cv.version, cv.build, cv.commit, versionSourceAgent
	case hit:
		cv.retryAt = later(cv.retryAt, sc.now.Add(binaryRescanInterval))
		sc.cache[bi.Path] = cv
	}
	if probed {
		sc.pathVersion(bi, target)
	}
}

func later(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

// pathVersion fills bi from its path when no version is known yet; only a
// llama_server carries build/commit in its path.
func (sc *binaryScan) pathVersion(bi *BinaryInfo, target string) {
	if bi.VersionSource != "" || bi.Kind != binaryKindLlamaServer {
		return
	}
	if b, c, ok := versionFromPath(target); ok {
		bi.Build, bi.Commit, bi.VersionSource = b, c, versionSourcePath
	}
}

// versionProbed fills bi, a version-probed binary whose stat gave fi and
// statErr, with its version: the one its --version run gives (agentVersioned),
// else what its path says. aborted reports a shutdown that overtook the run.
func (sc *binaryScan) versionProbed(bi *BinaryInfo, fi fs.FileInfo, statErr error, pr *prober, probe versionProbe, target string) (agentVersioned, aborted bool) {
	if statErr == nil && sc.ctx.Err() == nil {
		cv, aborted := sc.cachedOrProbedVersion(bi.Path, target, fi, pr, probe)
		if aborted {
			return false, true
		}
		if cv.ok {
			bi.Version, bi.Build, bi.Commit, bi.VersionSource = cv.version, cv.build, cv.commit, versionSourceAgent
			agentVersioned = true
		}
	}
	sc.pathVersion(bi, target)
	return agentVersioned, false
}

// cachedOrProbedVersion is the version run result for the entry (run as target
// through pr), whose stat gave fi: the cached one while it still applies (the
// same size and mtime, and either a version or a retry that is not due yet),
// else a fresh run, which it caches with the next retry time when it yields no
// version. aborted reports a shutdown that overtook the run: that says nothing
// about the binary, so nothing is cached.
func (sc *binaryScan) cachedOrProbedVersion(entry, target string, fi fs.FileInfo, pr *prober, probe versionProbe) (cv cachedVersion, aborted bool) {
	cv, hit := sc.cache[entry]
	same := hit && cv.size == fi.Size() && cv.mtime.Equal(fi.ModTime())
	if same && (cv.ok || sc.now.Before(cv.retryAt)) {
		return cv, false
	}
	failures := 0
	if same {
		failures = cv.failures
	}
	cv = cachedVersion{size: fi.Size(), mtime: fi.ModTime()}
	rctx, cancel := context.WithTimeout(sc.ctx, probe.timeout)
	text, rerr := pr.runVersion(rctx, target)
	cancel()
	if sc.ctx.Err() != nil {
		return cv, true
	}
	if rerr == nil || text != "" {
		cv.version, cv.build, cv.commit, cv.ok = probe.parse(text)
	}
	if !cv.ok {
		cv.failures = failures + 1
		cv.retryAt = sc.now.Add(retryBackoff(cv.failures))
	}
	if sc.cache != nil {
		sc.cache[entry] = cv
	}
	return cv, false
}

// entryTarget is the path of an entry in its execution context.
func entryTarget(entry string) string {
	if _, lp, ok := runtimectl.ParseWSLEntry(entry); ok {
		return lp
	}
	return entry
}

// siblingDirKey is the key of the directory an entry sits in: its parent for a
// native path, "wsl://<distro>" plus the Linux parent for a WSL entry.
func siblingDirKey(entry string) string {
	if distro, lp, ok := runtimectl.ParseWSLEntry(entry); ok {
		return runtimectl.WSLEntryScheme + distro + path.Dir(lp)
	}
	return filepath.Dir(entry)
}

// inheritSiblingVersions gives every rpc_server in out the version of the
// agent-versioned llama_server in its directory (byDir: directory -> its index
// in out), else whatever its path says.
func inheritSiblingVersions(out []BinaryInfo, byDir map[string]int) {
	for i := range out {
		if out[i].Kind != binaryKindRPCServer {
			continue
		}
		if j, ok := byDir[siblingDirKey(out[i].Path)]; ok {
			s := out[j]
			out[i].Version, out[i].Build, out[i].Commit, out[i].VersionSource = s.Version, s.Build, s.Commit, versionSourceAgent
		} else if b, c, ok := versionFromPath(entryTarget(out[i].Path)); ok {
			out[i].Build, out[i].Commit, out[i].VersionSource = b, c, versionSourcePath
		}
	}
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
	wsl        wslProbeConfig
	wslRun     wslCall

	scanMu sync.Mutex // one scan at a time; guards cache
	cache  map[string]cachedVersion

	mu       sync.Mutex
	ready    bool
	current  []BinaryInfo
	hash     [32]byte
	sentOnce bool
	sentHash [32]byte
}

func newBinaryInventory(allowed []string, wsl wslProbeConfig) *binaryInventory {
	return &binaryInventory{
		allowed:    allowed,
		stat:       os.Stat,
		runVersion: execVersion,
		wsl:        wsl,
		wslRun:     execWSLCall,
		cache:      map[string]cachedVersion{},
	}
}

// rescan recomputes the inventory and stores it.
func (b *binaryInventory) rescan(ctx context.Context) {
	b.scanMu.Lock()
	list := scanAll(ctx, time.Now(), b.allowed, &prober{stat: b.stat, runVersion: b.runVersion},
		&wslProbes{ctx: ctx, cfg: b.wsl, call: b.wslRun}, b.cache)
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
