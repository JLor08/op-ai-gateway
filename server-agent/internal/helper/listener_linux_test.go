// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

//go:build linux

package helper

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// recorder is a slog.Handler that keeps every record for inspection.
type recorder struct {
	mu   sync.Mutex
	recs []slog.Record
}

func (r *recorder) Enabled(context.Context, slog.Level) bool { return true }
func (r *recorder) WithAttrs([]slog.Attr) slog.Handler       { return r }
func (r *recorder) WithGroup(string) slog.Handler            { return r }
func (r *recorder) Handle(_ context.Context, rec slog.Record) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.recs = append(r.recs, rec.Clone())
	return nil
}

func (r *recorder) warns() []slog.Record {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []slog.Record
	for _, rec := range r.recs {
		if rec.Level == slog.LevelWarn {
			out = append(out, rec)
		}
	}
	return out
}

func attrs(rec slog.Record) map[string]slog.Value {
	m := map[string]slog.Value{}
	rec.Attrs(func(a slog.Attr) bool {
		m[a.Key] = a.Value
		return true
	})
	return m
}

// shortDir returns a fresh directory under /tmp (sun_path is short).
func shortDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("/tmp", "x")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(d) })
	return d
}

// otherID is a valid non-root agent identity that is not the test process
// (uid 1 when running as root), for tests that never accept a connection.
func otherID() Identity { return Identity{UID: os.Getuid() + 1, GID: os.Getgid()} }

// serve runs an HTTP server on ln and reports whether any request reached it.
func serve(t *testing.T, ln net.Listener) *atomic.Bool {
	t.Helper()
	var served atomic.Bool
	srv := &http.Server{
		ReadHeaderTimeout: 5 * time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			served.Store(true)
			_, _ = io.WriteString(w, "ok")
		}),
	}
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ln) }()
	t.Cleanup(func() {
		_ = srv.Close()
		if err := <-done; !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("Serve returned %v, want http.ErrServerClosed", err)
		}
	})
	return &served
}

func TestPeerAccepted(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("Listen refuses to serve uid 0; this runs as a non-root uid (CI, container --user 1000:1000)")
	}
	path := filepath.Join(shortDir(t), "d", "s")
	ln, err := Listen(path, Identity{UID: os.Getuid(), GID: os.Getgid()}, nil)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	served := serve(t, ln)

	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", path)
		},
	}, Timeout: 5 * time.Second}
	resp, err := client.Get("http://helper/v1/rapl")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || string(body) != "ok" {
		t.Fatalf("got %d %q, want 200 \"ok\"", resp.StatusCode, body)
	}
	if !served.Load() {
		t.Fatal("handler did not see the request")
	}
}

// deniedRoundTrip dials, sends a request and reads until the server closes.
// It fails the test if any response byte arrives.
func deniedRoundTrip(t *testing.T, path string) {
	t.Helper()
	c, err := net.Dial("unix", path)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	// The write may already fail if the server closed first; that is fine.
	_, _ = io.WriteString(c, "GET /v1/rapl HTTP/1.1\r\nHost: helper\r\n\r\n")
	b, err := io.ReadAll(c)
	if len(b) != 0 {
		t.Fatalf("denied peer received %q", b)
	}
	if err != nil && !errors.Is(err, syscall.ECONNRESET) && !errors.Is(err, syscall.EPIPE) {
		t.Fatalf("read: %v, want EOF or connection reset", err)
	}
}

func TestPeerDenied(t *testing.T) {
	path := filepath.Join(shortDir(t), "s")
	rec := &recorder{}
	ln, err := Listen(path, Identity{UID: os.Getuid() + 1, GID: os.Getgid()}, slog.New(rec))
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	served := serve(t, ln)

	deniedRoundTrip(t, path)
	w := rec.warns()
	if len(w) != 1 {
		t.Fatalf("got %d Warn records after first denial, want 1", len(w))
	}
	if w[0].Message != "helper: connection rejected" {
		t.Fatalf("Warn message %q", w[0].Message)
	}
	a := attrs(w[0])
	if a["peer_uid"].Int64() != int64(os.Getuid()) || a["peer_pid"].Int64() != int64(os.Getpid()) ||
		a["reason"].String() != "uid mismatch" {
		t.Fatalf("Warn attrs %v, want peer_uid=%d peer_pid=%d reason=uid mismatch", a, os.Getuid(), os.Getpid())
	}

	deniedRoundTrip(t, path)
	if n := len(rec.warns()); n != 1 {
		t.Fatalf("got %d Warn records after second denial, want still 1 (rate limited)", n)
	}
	if served.Load() {
		t.Fatal("a denied peer's request reached the handler")
	}
}

func TestDenyLogRateLimit(t *testing.T) {
	rec := &recorder{}
	now := time.Unix(1_000_000, 0)
	d := newDenyLog(slog.New(rec), func() time.Time { return now })

	d.note(1001, 7, "uid mismatch", nil)
	d.note(1001, 8, "uid mismatch", nil)
	if n := len(rec.warns()); n != 1 {
		t.Fatalf("same uid twice: %d warns, want 1", n)
	}
	d.note(1002, 9, "uid mismatch", nil)
	if n := len(rec.warns()); n != 2 {
		t.Fatalf("other uid: %d warns, want 2", n)
	}
	now = now.Add(denyLogEvery - time.Second)
	d.note(1001, 7, "uid mismatch", nil)
	if n := len(rec.warns()); n != 2 {
		t.Fatalf("inside the window: %d warns, want 2", n)
	}
	now = now.Add(time.Second)
	d.note(1001, 7, "uid mismatch", nil)
	if n := len(rec.warns()); n != 3 {
		t.Fatalf("window elapsed: %d warns, want 3", n)
	}
	d.note(-1, 0, "peer credentials unavailable", errors.New("boom"))
	w := rec.warns()
	if len(w) != 4 {
		t.Fatalf("ucred failure: %d warns, want 4", len(w))
	}
	if a := attrs(w[3]); a["peer_uid"].Int64() != -1 || a["error"].String() != "boom" {
		t.Fatalf("ucred failure attrs %v", a)
	}
}

func TestDenyLogCapped(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	d := newDenyLog(slog.New(&recorder{}), func() time.Time { return now })
	for uid := range 3 * maxDenyLogKeys {
		now = now.Add(time.Millisecond)
		d.note(uid, 0, "uid mismatch", nil)
		if n := len(d.last); n > maxDenyLogKeys {
			t.Fatalf("after uid %d: %d keys, cap %d", uid, n, maxDenyLogKeys)
		}
	}
}

func TestSocketPermissions(t *testing.T) {
	gid := os.Getgid()
	if os.Geteuid() == 0 {
		gid = 4242 // root can hand the socket to any group; prove the chown happens
	}
	dir := filepath.Join(shortDir(t), "d")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o777); err != nil { // pre-existing and too open
		t.Fatal(err)
	}
	path := filepath.Join(dir, "s")
	ln, err := Listen(path, Identity{UID: os.Getuid() + 1, GID: gid}, nil)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	dfi, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := dfi.Mode().Perm(); got != 0o750 {
		t.Fatalf("dir mode %o, want 0750", got)
	}
	sfi, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if sfi.Mode()&fs.ModeSocket == 0 {
		t.Fatalf("%s is not a socket: %v", path, sfi.Mode())
	}
	if got := sfi.Mode().Perm(); got != 0o660 {
		t.Fatalf("socket mode %o, want 0660", got)
	}
	for name, fi := range map[string]fs.FileInfo{"dir": dfi, "socket": sfi} {
		st := fi.Sys().(*syscall.Stat_t)
		if int(st.Gid) != gid {
			t.Errorf("%s gid %d, want %d", name, st.Gid, gid)
		}
		if os.Geteuid() == 0 && st.Uid != 0 {
			t.Errorf("%s uid %d, want 0", name, st.Uid)
		}
	}
}

func TestListenRemovesStaleSocket(t *testing.T) {
	path := filepath.Join(shortDir(t), "s")
	old, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	old.SetUnlinkOnClose(false)
	_ = old.Close()
	if _, err := os.Lstat(path); err != nil {
		t.Fatalf("stale socket not left behind: %v", err)
	}

	ln, err := Listen(path, otherID(), nil)
	if err != nil {
		t.Fatalf("Listen over stale socket: %v", err)
	}
	defer ln.Close()
	c, err := net.Dial("unix", path)
	if err != nil {
		t.Fatalf("dial new socket: %v", err)
	}
	_ = c.Close()
}

func TestListenRefusesNonSocket(t *testing.T) {
	path := filepath.Join(shortDir(t), "s")
	if err := os.WriteFile(path, []byte("keep me"), 0o600); err != nil {
		t.Fatal(err)
	}
	ln, err := Listen(path, otherID(), nil)
	if err == nil {
		_ = ln.Close()
		t.Fatal("Listen over a regular file succeeded; want error")
	}
	if b, rerr := os.ReadFile(path); rerr != nil || string(b) != "keep me" {
		t.Fatalf("regular file touched: %q, %v", b, rerr)
	}
}

func TestListenRefusesSymlinkDir(t *testing.T) {
	base := shortDir(t)
	real := filepath.Join(base, "real")
	if err := os.Mkdir(real, 0o750); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	ln, err := Listen(filepath.Join(link, "s"), otherID(), nil)
	if err == nil {
		_ = ln.Close()
		t.Fatal("Listen in a symlinked dir succeeded; want error")
	}
	if !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("error %q does not name the symlink", err)
	}
	if _, err := os.Lstat(filepath.Join(real, "s")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("socket created through the symlink: %v", err)
	}
}

func TestListenRefusesForeignOwnedDir(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("ownership is only enforced when running as root")
	}
	dir := filepath.Join(shortDir(t), "d")
	if err := os.Mkdir(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(dir, 1000, 1000); err != nil {
		t.Fatal(err)
	}
	ln, err := Listen(filepath.Join(dir, "s"), Identity{UID: 1000, GID: 1000}, nil)
	if err == nil {
		_ = ln.Close()
		t.Fatal("Listen in a uid-1000-owned dir succeeded as root; want error")
	}
	if !strings.Contains(err.Error(), "owned by uid 1000") {
		t.Fatalf("error %q does not name the foreign owner", err)
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if st := fi.Sys().(*syscall.Stat_t); st.Uid != 1000 {
		t.Fatalf("foreign dir re-owned to uid %d", st.Uid)
	}
	if _, err := os.Lstat(filepath.Join(dir, "s")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("socket created in foreign dir: %v", err)
	}
}

func TestListenRefusesUID0(t *testing.T) {
	path := filepath.Join(shortDir(t), "d", "s")
	ln, err := Listen(path, Identity{UID: 0, GID: os.Getgid()}, nil)
	if err == nil {
		_ = ln.Close()
		t.Fatal("Listen for uid 0 succeeded; want error")
	}
	if _, err := os.Lstat(filepath.Dir(path)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("socket directory created despite the refusal: %v", err)
	}
}

func TestListenRefusesRelativePath(t *testing.T) {
	for _, p := range []string{"agent.sock", "d/agent.sock", "./d/agent.sock"} {
		if ln, err := Listen(p, otherID(), nil); err == nil {
			_ = ln.Close()
			t.Fatalf("Listen(%q) succeeded; want error", p)
		}
	}
}

func TestListenRefusesUncleanPath(t *testing.T) {
	base := shortDir(t)
	for _, p := range []string{base + "/d/../d/s", base + "//d/s", base + "/d/./s", base + "/d/s/"} {
		if ln, err := Listen(p, otherID(), nil); err == nil {
			_ = ln.Close()
			t.Fatalf("Listen(%q) succeeded; want error", p)
		}
	}
	if _, err := os.Lstat(filepath.Join(base, "d")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("socket directory created for an unclean path: %v", err)
	}
}

func TestListenRefusesRootDir(t *testing.T) {
	ln, err := Listen("/op-helper-test.sock", otherID(), nil)
	if err == nil {
		_ = ln.Close()
		t.Fatal("Listen(\"/op-helper-test.sock\") succeeded; want error")
	}
	if !strings.Contains(err.Error(), "dedicated directory") {
		t.Fatalf("error %q does not explain the refusal", err)
	}
	if _, err := os.Lstat("/op-helper-test.sock"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("socket created in /: %v", err)
	}
}

func TestListenRefusesSharedDir(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("the dedicated-directory check is only enforced when running as root")
	}
	dir := filepath.Join(shortDir(t), "d")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "other"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	ln, err := Listen(filepath.Join(dir, "s"), Identity{UID: 1000, GID: 4242}, nil)
	if err == nil {
		_ = ln.Close()
		t.Fatal("Listen in a shared directory succeeded; want error")
	}
	if !strings.Contains(err.Error(), "not dedicated") {
		t.Fatalf("error %q does not name the shared directory", err)
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		t.Fatal(err)
	}
	st := fi.Sys().(*syscall.Stat_t)
	if fi.Mode().Perm() != 0o755 || st.Uid != 0 || st.Gid != 0 {
		t.Fatalf("shared dir changed: mode %o uid %d gid %d, want 0755 0:0", fi.Mode().Perm(), st.Uid, st.Gid)
	}
	if _, err := os.Lstat(filepath.Join(dir, "s")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("socket created in shared dir: %v", err)
	}
}

func TestListenRefusesLiveSocket(t *testing.T) {
	path := filepath.Join(shortDir(t), "s")
	first, err := Listen(path, otherID(), nil)
	if err != nil {
		t.Fatalf("first Listen: %v", err)
	}
	defer first.Close()
	second, err := Listen(path, otherID(), nil)
	if err == nil {
		_ = second.Close()
		t.Fatal("second Listen on a live socket succeeded; want error")
	}
	if want := "helper: another helper is already listening on " + path; err.Error() != want {
		t.Fatalf("error %q, want %q", err, want)
	}
	c, err := net.Dial("unix", path)
	if err != nil {
		t.Fatalf("first helper's socket gone after the refused second Listen: %v", err)
	}
	_ = c.Close()
}

func TestListenRefusesUnsafeAncestor(t *testing.T) {
	cases := []struct {
		name     string
		rootOnly bool
		prep     func(t *testing.T, a string)
		want     string
	}{
		{"world-writable", false, func(t *testing.T, a string) {
			if err := os.Chmod(a, 0o777); err != nil {
				t.Fatal(err)
			}
		}, "writable by group or others"},
		{"group-writable", false, func(t *testing.T, a string) {
			if err := os.Chmod(a, 0o775); err != nil {
				t.Fatal(err)
			}
		}, "writable by group or others"},
		{"foreign-owned", true, func(t *testing.T, a string) {
			if err := os.Chown(a, 1000, 1000); err != nil {
				t.Fatal(err)
			}
		}, "owned by uid 1000"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.rootOnly && os.Geteuid() != 0 {
				t.Skip("ancestor ownership is only enforced when running as root")
			}
			a := filepath.Join(shortDir(t), "a")
			if err := os.Mkdir(a, 0o755); err != nil {
				t.Fatal(err)
			}
			c.prep(t, a)
			ln, err := Listen(filepath.Join(a, "d", "s"), otherID(), nil)
			if err == nil {
				_ = ln.Close()
				t.Fatal("Listen below an unsafe ancestor succeeded; want error")
			}
			if !strings.Contains(err.Error(), c.want) || !strings.Contains(err.Error(), a) {
				t.Fatalf("error %q does not name %s as %q", err, a, c.want)
			}
			if _, err := os.Lstat(filepath.Join(a, "d")); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("socket directory created below the unsafe ancestor: %v", err)
			}
		})
	}
}

// A sticky world-writable ancestor (like /tmp) is safe: nobody else can
// rename or remove the entries in it that are not theirs.
func TestListenAcceptsStickyAncestor(t *testing.T) {
	a := filepath.Join(shortDir(t), "a")
	if err := os.Mkdir(a, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(a, 0o777|fs.ModeSticky); err != nil {
		t.Fatal(err)
	}
	ln, err := Listen(filepath.Join(a, "d", "s"), otherID(), nil)
	if err != nil {
		t.Fatalf("Listen below a sticky ancestor: %v", err)
	}
	_ = ln.Close()
}

// A symlink among the ancestors is resolved once; the socket is created at
// the resolved location, whose components were the ones checked.
func TestListenResolvesSymlinkedAncestor(t *testing.T) {
	base := shortDir(t)
	target := filepath.Join(base, "real")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(base, "link")); err != nil {
		t.Fatal(err)
	}
	ln, err := Listen(filepath.Join(base, "link", "d", "s"), otherID(), nil)
	if err != nil {
		t.Fatalf("Listen through a symlinked ancestor: %v", err)
	}
	defer ln.Close()
	if got, want := ln.Addr().String(), filepath.Join(target, "d", "s"); got != want {
		t.Fatalf("socket at %s, want the resolved path %s", got, want)
	}
}
