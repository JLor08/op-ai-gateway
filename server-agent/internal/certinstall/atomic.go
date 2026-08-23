// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

// Package certinstall fetches a server's TLS certificate from the gateway's
// agent-certificate endpoint (GET /api/agent/v1/certificate), installs it
// atomically on disk, and -- on a real change -- runs a LOCAL reload command.
//
// The gateway NEVER supplies that command: cert_reload_command lives
// exclusively in this agent's own local configuration (Task 4), and nothing in
// this package's wire contract with the gateway (fetch.go's certResponse)
// carries anything executable, a path-with-arguments, or a script. This
// package takes everything it needs as constructor parameters/struct fields --
// it never imports the agent's config package -- so it stays independently
// testable and the wiring task (5b) can pass through whatever it resolved.
package certinstall

import (
	"context"
	"errors"
	"log/slog"
	"op-ai-server-agent/internal/certfiles"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// The five files an install writes into cert_dir, plus the sidecar memo that
// makes the NEXT fetch conditional. privkey.pem is 0600 (it holds the private
// key); everything else is 0644 (public material). The four filenames also
// read back by internal/proxy and internal/trust are aliases onto
// internal/certfiles, the single shared source of truth for the cert-dir
// layout; etagFile is a certinstall-only sidecar with no outside reader, so
// it stays local.
const (
	fullchainFile = certfiles.Fullchain
	certFile      = certfiles.Cert
	chainFile     = certfiles.Chain
	caFile        = certfiles.CA
	privkeyFile   = certfiles.Privkey
	etagFile      = ".op-cert-etag"
)

// writeTempFile creates a temp file DIRECTLY INSIDE dir (never
// os.CreateTemp(""), which resolves through os.TempDir()/$TMPDIR and can land
// on a different filesystem than dir -- the exact EXDEV failure Plan A hit),
// writes content, and chmods it to mode.
//
// It is a package-level indirection, keyed by the file's intended FINAL name,
// so a test can simulate a write failure for one specific target file without
// touching real filesystem permissions (fragile across OSes, and meaningless
// when a test runs as root).
var writeTempFile = func(dir, finalName string, content []byte, mode os.FileMode) (tmpPath string, err error) {
	tmp, err := os.CreateTemp(dir, ".op-cert-*")
	if err != nil {
		return "", err
	}
	tmpPath = tmp.Name()
	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return "", err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return "", err
	}
	if err := os.Chmod(tmpPath, mode); err != nil {
		os.Remove(tmpPath)
		return "", err
	}
	return tmpPath, nil
}

// pendingFile is one staged-but-not-yet-renamed write.
type pendingFile struct {
	tmpPath   string
	finalName string
}

// atomicWriter batches temp-file creation for one install so that renaming
// into place happens only AFTER every file has been written successfully. A
// failure at any point -- during a write, or during a rename -- cleans up
// every temp file that has not (yet) been renamed, so a partial install never
// leaves a stray .op-cert-* file behind and never touches a final path it
// has not fully replaced.
type atomicWriter struct {
	dir     string
	pending []pendingFile
}

// stage writes content to a fresh temp file for finalName and remembers it
// for commit. On failure it cleans up EVERYTHING staged so far in this batch
// (not just this one file) and returns the error, so a caller that stops at
// the first stage() error never has to clean up separately.
func (a *atomicWriter) stage(finalName string, content []byte, mode os.FileMode) error {
	tmpPath, err := writeTempFile(a.dir, finalName, content, mode)
	if err != nil {
		a.abort()
		return err
	}
	a.pending = append(a.pending, pendingFile{tmpPath: tmpPath, finalName: finalName})
	return nil
}

// commit renames every staged file into place, IN STAGING ORDER (callers
// stage privkey.pem last, so it is renamed last -- see install.go). If a
// rename fails partway through, every file already renamed is now the new
// final file (untouched by this failure, exactly as it should be); only the
// remaining, not-yet-renamed temp files -- including the one whose own
// rename just failed -- are cleaned up.
func (a *atomicWriter) commit() error {
	for i, p := range a.pending {
		if err := os.Rename(p.tmpPath, filepath.Join(a.dir, p.finalName)); err != nil {
			for _, rem := range a.pending[i:] {
				os.Remove(rem.tmpPath)
			}
			a.pending = nil
			return err
		}
	}
	a.pending = nil
	return nil
}

// abort removes every temp file staged so far without renaming any of them.
func (a *atomicWriter) abort() {
	for _, p := range a.pending {
		os.Remove(p.tmpPath)
	}
	a.pending = nil
}

// saveETagSidecar atomically (over)writes the opaque If-None-Match memo file.
// Best-effort: a failure here only means the NEXT Sync fetches
// unconditionally instead of conditionally (never a correctness problem --
// see diskState.memoValid), so it is logged at Debug and never surfaced as an
// error.
func saveETagSidecar(dir, value string) {
	if value == "" {
		return
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		slog.Debug("certificate etag sidecar: mkdir failed", "err", err)
		return
	}
	tmpPath, err := writeTempFile(dir, etagFile, []byte(value), 0o644)
	if err != nil {
		slog.Debug("certificate etag sidecar: write failed", "err", err)
		return
	}
	if err := os.Rename(tmpPath, filepath.Join(dir, etagFile)); err != nil {
		os.Remove(tmpPath)
		slog.Debug("certificate etag sidecar: rename failed", "err", err)
	}
}

// hookTimeout/hookWaitDelay are package-level VARS (not consts), like
// client.go's backoffBase, so a test can shrink them instead of waiting out
// the real budget.
var (
	// hookTimeout bounds the reload command's total run time.
	hookTimeout = 30 * time.Second
	// hookWaitDelay bounds how long Wait may block AFTER the process itself has
	// exited, in case it left a child holding the stdout/stderr pipes open --
	// without this, a detached grandchild could stall Wait indefinitely even
	// though the reload command's own process is long gone.
	hookWaitDelay = 5 * time.Second
)

// hookOutputCap bounds the combined stdout+stderr kept for the failure log
// line.
const hookOutputCap = 2048

// boundedBuffer keeps only the MOST RECENT hookOutputCap bytes ever written to
// it, so a chatty or runaway reload command cannot inflate memory or the log
// line it ends up in.
type boundedBuffer struct {
	mu  sync.Mutex
	buf []byte
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
	if len(b.buf) > hookOutputCap {
		b.buf = b.buf[len(b.buf)-hookOutputCap:]
	}
	return len(p), nil
}

func (b *boundedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf)
}

// runReloadHook runs the configured LOCAL reload command, if any, after a
// successful and CHANGED install. It never rolls back the just-installed
// files on failure -- a broken reload command is not a reason to withdraw a
// working certificate -- and its failure log names only the exit code, NEVER
// the command line itself (which is local configuration and may carry
// secrets of its own, e.g. a sudo password baked into a wrapper script).
//
// buildHookCommand (atomic_other.go / atomic_windows.go) supplies the
// OS-specific way to run an arbitrary, possibly-composed command line: "sh -c"
// on unix, and on Windows a raw CmdLine (os/exec's own argument quoting would
// otherwise mangle a piped/redirected/quoted command).
func runReloadHook(ctx context.Context, command string) {
	command = strings.TrimSpace(command)
	if command == "" {
		return
	}
	// WithoutCancel: the hook is what actually ACTIVATES the certificate, and the
	// etag sidecar is already saved by the time we get here -- so a hook killed
	// mid-run by a shutdown would leave the new files installed, the next fetch
	// answering 304, and the reload never re-attempted.
	//
	// Be precise about what this does at shutdown, because the obvious reading is
	// wrong: nothing joins this goroutine (triggerCertSync detaches it and Run
	// returns as soon as its context is done), so the agent does NOT wait here. What
	// changes is that the child is no longer signalled when the agent exits: it is
	// orphaned and finishes on its own. The hookTimeout + WaitDelay bound therefore
	// only applies while the agent is still alive; after the process is gone nothing
	// kills the command, and a failure it logs goes to a stderr that no longer
	// exists. That is the deliberate trade -- an orphaned reload beats a certificate
	// that is installed but never activated.
	//
	// (A hook that RUNS and FAILS is a different case and is deliberately not
	// retried -- see README-certificates.md 9.11.)
	hctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), hookTimeout)
	defer cancel()
	cmd := buildHookCommand(hctx, command)
	var out boundedBuffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	cmd.WaitDelay = hookWaitDelay
	if err := cmd.Run(); err != nil {
		exitCode := -1
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exitCode = exitErr.ExitCode()
		}
		slog.Warn("certificate reload hook failed", "exit_code", exitCode, "output", out.String())
		return
	}
	slog.Debug("certificate reload hook ok", "output", out.String())
}
