// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

//go:build linux

package helper

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const (
	// denyLogEvery bounds the Warn lines per denied peer uid.
	denyLogEvery = 10 * time.Minute
	// maxDenyLogKeys caps the per-uid log state so a peer cycling through
	// uids cannot grow it without bound.
	maxDenyLogKeys = 1024
)

// Listen prepares the socket directory, creates the Unix socket at path and
// returns a listener that only hands out connections whose SO_PEERCRED uid
// passes authorizePeer. The directory is created 0750 if missing, must be a
// real directory (not a symlink) and, when running as root, owned by root; it
// is then set to 0750 owner:id.GID. A stale socket at path is removed;
// anything else there is an error. The socket ends up 0660 owner:id.GID.
// When not running as root (tests) the owner is left alone and only the
// group and the modes are set. A nil log means slog.Default().
func Listen(path string, id Identity, log *slog.Logger) (net.Listener, error) {
	if log == nil {
		log = slog.Default()
	}
	owner := -1
	if os.Geteuid() == 0 {
		owner = 0
	}
	if err := prepareDir(filepath.Dir(path), owner, id.GID); err != nil {
		return nil, err
	}
	if err := removeStaleSocket(path); err != nil {
		return nil, err
	}
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, fmt.Errorf("helper: listen on %s: %w", path, err)
	}
	if err := os.Chown(path, owner, id.GID); err != nil {
		_ = ln.Close()
		return nil, fmt.Errorf("helper: chown socket: %w", err)
	}
	if err := os.Chmod(path, 0o660); err != nil {
		_ = ln.Close()
		return nil, fmt.Errorf("helper: chmod socket: %w", err)
	}
	return &peerListener{ln: ln, want: id, deny: newDenyLog(log, time.Now)}, nil
}

// prepareDir creates and secures the socket directory. owner is 0 when
// running as root (the directory must then already be root's) and -1
// otherwise (ownership unchecked and unchanged). The ownership check, chown
// and chmod go through one O_NOFOLLOW descriptor, so a swap after the Lstat
// cannot redirect them.
func prepareDir(dir string, owner, gid int) error {
	if _, err := os.Lstat(dir); errors.Is(err, fs.ErrNotExist) {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return fmt.Errorf("helper: create socket directory: %w", err)
		}
	}
	fi, err := os.Lstat(dir)
	switch {
	case err != nil:
		return fmt.Errorf("helper: socket directory: %w", err)
	case fi.Mode()&fs.ModeSymlink != 0:
		return fmt.Errorf("helper: socket directory %s is a symlink, not a directory", dir)
	case !fi.IsDir():
		return fmt.Errorf("helper: socket directory %s is not a directory", dir)
	}
	f, err := os.OpenFile(dir, os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return fmt.Errorf("helper: open socket directory: %w", err)
	}
	defer f.Close()
	ffi, err := f.Stat()
	if err != nil {
		return fmt.Errorf("helper: stat socket directory: %w", err)
	}
	if !os.SameFile(fi, ffi) {
		return fmt.Errorf("helper: socket directory %s changed while being secured", dir)
	}
	if owner == 0 {
		st, ok := ffi.Sys().(*syscall.Stat_t)
		if !ok {
			return fmt.Errorf("helper: socket directory %s: owner unknown", dir)
		}
		if st.Uid != 0 {
			return fmt.Errorf("helper: socket directory %s is owned by uid %d, not root", dir, st.Uid)
		}
	}
	if err := f.Chown(owner, gid); err != nil {
		return fmt.Errorf("helper: chown socket directory: %w", err)
	}
	if err := f.Chmod(0o750); err != nil {
		return fmt.Errorf("helper: chmod socket directory: %w", err)
	}
	return nil
}

// removeStaleSocket removes a leftover socket at path; any other kind of
// entry is refused rather than deleted.
func removeStaleSocket(path string) error {
	fi, err := os.Lstat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		return fmt.Errorf("helper: socket path: %w", err)
	case fi.Mode()&fs.ModeSocket == 0:
		return fmt.Errorf("helper: %s exists and is not a socket", path)
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("helper: remove stale socket: %w", err)
	}
	return nil
}

// peerListener authorizes every connection by SO_PEERCRED before any byte is
// read. It deliberately does not embed *net.UnixListener, so AcceptUnix (which
// would bypass the check) is not reachable through it.
type peerListener struct {
	ln   *net.UnixListener
	want Identity
	deny *denyLog
}

// Accept returns the next authorized connection. Denied peers and peers whose
// credentials cannot be read are logged (rate-limited) and closed without a
// response; the loop then waits for the next connection. Errors from the
// underlying listener (including after Close) are returned unchanged.
func (l *peerListener) Accept() (net.Conn, error) {
	for {
		c, err := l.ln.AcceptUnix()
		if err != nil {
			return nil, err
		}
		// Log before closing: a peer that sees EOF can rely on the decision
		// having been recorded.
		cred, err := peerCred(c)
		if err != nil {
			l.deny.note(-1, 0, "peer credentials unavailable", err)
			_ = c.Close()
			continue
		}
		if ok, reason := authorizePeer(int(cred.Uid), l.want); !ok {
			l.deny.note(int(cred.Uid), int(cred.Pid), reason, nil)
			_ = c.Close()
			continue
		}
		return c, nil
	}
}

// Close closes the underlying listener, which also unlinks the socket file.
func (l *peerListener) Close() error { return l.ln.Close() }

// Addr returns the socket address.
func (l *peerListener) Addr() net.Addr { return l.ln.Addr() }

// peerCred reads the peer's credentials (non-blocking getsockopt).
func peerCred(c *net.UnixConn) (*unix.Ucred, error) {
	raw, err := c.SyscallConn()
	if err != nil {
		return nil, err
	}
	var (
		cred *unix.Ucred
		cerr error
	)
	if err := raw.Control(func(fd uintptr) {
		cred, cerr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return nil, err
	}
	return cred, cerr
}

// denyLog logs rejected connections at Warn, at most once every denyLogEvery
// per peer uid (-1 for unreadable credentials), so a misbehaving local
// process cannot flood the log.
type denyLog struct {
	log *slog.Logger
	now func() time.Time

	mu   sync.Mutex
	last map[int]time.Time // last Warn per peer uid
}

func newDenyLog(log *slog.Logger, now func() time.Time) *denyLog {
	return &denyLog{log: log, now: now, last: make(map[int]time.Time)}
}

func (d *denyLog) note(uid, pid int, reason string, err error) {
	now := d.now()
	d.mu.Lock()
	at, seen := d.last[uid]
	emit := !seen || now.Sub(at) >= denyLogEvery
	if emit {
		if !seen && len(d.last) >= maxDenyLogKeys {
			d.makeRoom(now)
		}
		d.last[uid] = now
	}
	d.mu.Unlock()
	if !emit {
		return
	}
	attrs := []any{"peer_uid", uid, "peer_pid", pid, "reason", reason}
	if err != nil {
		attrs = append(attrs, "error", err.Error())
	}
	d.log.Warn("helper: connection rejected", attrs...)
}

// makeRoom drops expired entries and, if none had expired, the oldest one.
// Called with d.mu held and the map full.
func (d *denyLog) makeRoom(now time.Time) {
	var (
		oldestUID int
		oldestAt  time.Time
		found     bool
	)
	for uid, at := range d.last {
		if now.Sub(at) >= denyLogEvery {
			delete(d.last, uid)
			continue
		}
		if !found || at.Before(oldestAt) {
			oldestUID, oldestAt, found = uid, at, true
		}
	}
	if len(d.last) >= maxDenyLogKeys && found {
		delete(d.last, oldestUID)
	}
}
