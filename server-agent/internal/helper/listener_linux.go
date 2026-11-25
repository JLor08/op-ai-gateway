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
	// liveProbeTimeout bounds the dial that tells a stale socket from one a
	// running helper still listens on.
	liveProbeTimeout = time.Second
)

// Listen prepares the socket directory, creates the Unix socket at path and
// returns a listener that only hands out connections whose SO_PEERCRED uid
// passes authorizePeer. A nil log means slog.Default().
//
// path must be absolute and clean and live in a dedicated directory (never
// "/"); id.UID must not be 0. The parent of the socket directory must exist;
// it is resolved through symlinks once and every later step uses the
// resolved path. Every directory from "/" down to that parent must be safe
// (see checkAncestors), so no unprivileged user can swap any component
// between the checks and the chown/chmod calls below. The socket directory
// is created 0750 if missing, must be a real directory (not a symlink) and,
// when running as root, owned by root and empty apart from the socket's own
// name; it is then set to 0750 owner:id.GID. A stale socket at path is
// removed unless another listener still answers on it; anything else there
// is an error. The socket ends up 0660 owner:id.GID. When not running as
// root (tests) ownership is neither checked nor changed; the group, the
// modes and the ancestors' writability still are.
func Listen(path string, id Identity, log *slog.Logger) (net.Listener, error) {
	if log == nil {
		log = slog.Default()
	}
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, fmt.Errorf("helper: socket path %q must be absolute and clean", path)
	}
	if filepath.Dir(path) == "/" {
		return nil, fmt.Errorf("helper: socket path %s must be inside a dedicated directory, not /", path)
	}
	if id.UID == 0 {
		return nil, errors.New("helper: refusing to serve uid 0")
	}
	root := os.Geteuid() == 0
	owner := -1
	if root {
		owner = 0
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(filepath.Dir(path)))
	if err != nil {
		return nil, fmt.Errorf("helper: socket directory parent: %w", err)
	}
	if err := checkAncestors(parent, root); err != nil {
		return nil, err
	}
	dir := filepath.Join(parent, filepath.Base(filepath.Dir(path)))
	base := filepath.Base(path)
	path = filepath.Join(dir, base)
	if err := prepareDir(dir, base, owner, id.GID); err != nil {
		return nil, err
	}
	if err := removeStaleSocket(path); err != nil {
		return nil, err
	}
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, fmt.Errorf("helper: listen on %s: %w", path, err)
	}
	if err := secureSocket(path, owner, id.GID); err != nil {
		_ = ln.Close()
		return nil, err
	}
	return &peerListener{ln: ln, want: id, deny: newDenyLog(log, time.Now)}, nil
}

// checkAncestors walks from dir (already free of symlinks) up to "/" and
// requires every directory to be not writable by group or others, unless it
// carries the sticky bit (like /tmp: others may add entries but cannot rename
// or remove ours). When root, every directory must also be owned by uid 0.
// Together this means only root can replace any component of the path.
// Not running as root (tests), ownership is skipped: the test's own
// temporary directories belong to the test user.
func checkAncestors(dir string, root bool) error {
	for p := dir; ; p = filepath.Dir(p) {
		fi, err := os.Lstat(p)
		if err != nil {
			return fmt.Errorf("helper: socket path ancestor: %w", err)
		}
		if !fi.IsDir() {
			return fmt.Errorf("helper: socket path ancestor %s is not a directory", p)
		}
		st, ok := fi.Sys().(*syscall.Stat_t)
		if !ok {
			return fmt.Errorf("helper: socket path ancestor %s: owner unknown", p)
		}
		if root && st.Uid != 0 {
			return fmt.Errorf("helper: socket path ancestor %s is owned by uid %d, not root", p, st.Uid)
		}
		if fi.Mode().Perm()&0o022 != 0 && fi.Mode()&fs.ModeSticky == 0 {
			return fmt.Errorf("helper: socket path ancestor %s is writable by group or others", p)
		}
		if p == "/" {
			return nil
		}
	}
}

// prepareDir creates and secures the socket directory. owner is 0 when
// running as root (the directory must then already be root's and contain
// nothing but base) and -1 otherwise (ownership unchecked and unchanged).
// The checks, chown and chmod go through one O_NOFOLLOW descriptor, so a
// swap after the Lstat cannot redirect them.
func prepareDir(dir, base string, owner, gid int) error {
	if _, err := os.Lstat(dir); errors.Is(err, fs.ErrNotExist) {
		if err := os.Mkdir(dir, 0o750); err != nil {
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
		if err := checkDedicatedDir(f, ffi, dir, base); err != nil {
			return err
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

// checkDedicatedDir requires the open socket directory to be root's and to
// hold nothing but base, so Listen never re-owns a shared directory such as
// /run (systemd's RuntimeDirectory= starts empty).
func checkDedicatedDir(f *os.File, fi fs.FileInfo, dir, base string) error {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("helper: socket directory %s: owner unknown", dir)
	}
	if st.Uid != 0 {
		return fmt.Errorf("helper: socket directory %s is owned by uid %d, not root", dir, st.Uid)
	}
	names, err := f.Readdirnames(-1)
	if err != nil {
		return fmt.Errorf("helper: read socket directory: %w", err)
	}
	for _, n := range names {
		if n != base {
			return fmt.Errorf("helper: socket directory %s is not dedicated: it contains %q", dir, n)
		}
	}
	return nil
}

// removeStaleSocket removes a leftover socket at path; any other kind of
// entry, or a socket that still accepts connections, is refused rather than
// deleted.
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
	if c, err := net.DialTimeout("unix", path, liveProbeTimeout); err == nil {
		_ = c.Close()
		return fmt.Errorf("helper: another helper is already listening on %s", path)
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("helper: remove stale socket: %w", err)
	}
	return nil
}

// secureSocket hands the new socket to owner:gid with mode 0660. Lchown never
// follows a symlink; chmod has no such variant on Linux, so the entry is
// re-checked to be a socket first (the directory is root's and 0750 by now,
// so nobody else can replace it in between).
func secureSocket(path string, owner, gid int) error {
	if err := os.Lchown(path, owner, gid); err != nil {
		return fmt.Errorf("helper: chown socket: %w", err)
	}
	fi, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("helper: socket: %w", err)
	}
	if fi.Mode()&fs.ModeSocket == 0 {
		return fmt.Errorf("helper: %s is no longer a socket", path)
	}
	if err := os.Chmod(path, 0o660); err != nil {
		return fmt.Errorf("helper: chmod socket: %w", err)
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
