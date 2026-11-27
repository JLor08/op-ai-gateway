// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package runtime

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"
	"time"
)

// logTestCfgDir is the agent's config_files_dir in the table tests: absolute
// and POSIX, so the same table runs on every non-Windows runner (CI is Linux).
const logTestCfgDir = "/agent/cfg"

func nativeLogSpec(id string) Spec {
	return Spec{ID: id, Binary: "/usr/bin/server", UpstreamModel: "m1"}
}

func wslLogSpec(id string) Spec {
	s := wslSpec(wslBin, "")
	s.ID = id
	return s
}

// TestResolveLogFile is the lexical half of the log-file path policy (#430,
// design §5.1, §5.2 step 1, §5.3, §5.4). It runs both separators on the
// POSIX runner, in the style of TestAllowedDirBase: Windows entries are
// exercised through the WSL /mnt/<drive> mapping, which compares Windows
// paths on every GOOS.
func TestResolveLogFile(t *testing.T) {
	if goruntime.GOOS == "windows" {
		t.Skip("the table uses POSIX native paths; Windows-native containment is withinDir's own GOOS behaviour")
	}
	cases := []struct {
		name        string
		spec        Spec
		cfgDir      string
		entries     []string
		path        string
		wantOpen    string // "" = expect a refusal
		wantInCfg   bool
		wantSetting bool // a refusal that must name the setting and env var
	}{
		{
			name: "config dir file", spec: nativeLogSpec("s1"), path: "${CONFIG_DIR}/e.log",
			wantOpen: "/agent/cfg/s1/e.log", wantInCfg: true,
		},
		{
			name: "config dir nested file", spec: nativeLogSpec("s1"), path: "${CONFIG_DIR}/logs/e.log",
			wantOpen: "/agent/cfg/s1/logs/e.log", wantInCfg: true,
		},
		{
			name: "instance key config dir", spec: nativeLogSpec("s1#2"), path: "${CONFIG_DIR}/e.log",
			wantOpen: "/agent/cfg/s1#2/e.log", wantInCfg: true,
		},
		{
			name: "config dir named by its literal path", spec: nativeLogSpec("s1"), path: "/agent/cfg/s1/e.log",
			wantOpen: "/agent/cfg/s1/e.log", wantInCfg: true,
		},
		{name: "config dir dot-dot escape", spec: nativeLogSpec("s1"), path: "${CONFIG_DIR}/../x.log", wantSetting: true},
		{name: "config dir into a sibling instance", spec: nativeLogSpec("s1"), path: "${CONFIG_DIR}/../s1#2/e.log", wantSetting: true},
		{name: "config dir itself", spec: nativeLogSpec("s1"), path: "${CONFIG_DIR}", wantSetting: true},
		{name: "config base itself", spec: nativeLogSpec("s1"), path: "/agent/cfg/e.log", wantSetting: true},
		{name: "no config_files_dir", spec: nativeLogSpec("s1"), cfgDir: "-", path: "${CONFIG_DIR}/e.log"},
		{name: "external with empty list", spec: nativeLogSpec("s1"), path: "/var/log/x.log", wantSetting: true},
		{
			name: "external under slash-star", spec: nativeLogSpec("s1"), entries: []string{"/var/log/*"},
			path: "/var/log/x.log", wantOpen: "/var/log/x.log",
		},
		{
			name: "external under backslash-star", spec: nativeLogSpec("s1"), entries: []string{`/var/log\*`},
			path: "/var/log/x.log", wantOpen: "/var/log/x.log",
		},
		{
			name: "external under bare entry", spec: nativeLogSpec("s1"), entries: []string{"/var/log"},
			path: "/var/log/app/x.log", wantOpen: "/var/log/app/x.log",
		},
		{
			name: "external with placeholder", spec: nativeLogSpec("s1"), entries: []string{"/var/log/*"},
			path: "/var/log/${MODEL}-${PORT}.log", wantOpen: "/var/log/m1-8080.log",
		},
		{
			name: "external path is cleaned", spec: nativeLogSpec("s1"), entries: []string{"/var/log/*"},
			path: "/var/log/./a//x.log", wantOpen: "/var/log/a/x.log",
		},
		{name: "entry dir itself", spec: nativeLogSpec("s1"), entries: []string{"/var/log/*"}, path: "/var/log", wantSetting: true},
		{name: "shared-prefix sibling", spec: nativeLogSpec("s1"), entries: []string{"/var/log/*"}, path: "/var/logx/x.log", wantSetting: true},
		{
			name: "external dot-dot escape", spec: nativeLogSpec("s1"), entries: []string{"/var/log/*"},
			path: "/var/log/../../etc/shadow", wantSetting: true,
		},
		{
			name: "relative entry never matches", spec: nativeLogSpec("s1"), entries: []string{"logs"},
			path: "/agent/logs/x.log", wantSetting: true,
		},
		{
			name: "bare star entry permits nothing", spec: nativeLogSpec("s1"), entries: []string{"*"},
			path: "/var/log/x.log", wantSetting: true,
		},
		{
			name: "wsl entry never permits a native spec", spec: nativeLogSpec("s1"), entries: []string{"wsl://Ubuntu-24.04/var/log/*"},
			path: "/var/log/x.log", wantSetting: true,
		},
		{name: "proc under root star", spec: nativeLogSpec("s1"), entries: []string{"/*"}, path: "/proc/self/environ"},
		{name: "proc under its own entry", spec: nativeLogSpec("s1"), entries: []string{"/proc/*"}, path: "/proc/self/environ"},
		{name: "sys under its own entry", spec: nativeLogSpec("s1"), entries: []string{"/sys"}, path: "/sys/kernel/x"},
		{name: "dev under its own entry", spec: nativeLogSpec("s1"), entries: []string{"/dev/*"}, path: "/dev/sda"},
		{name: "proc reached by dot-dot", spec: nativeLogSpec("s1"), entries: []string{"/proc/*"}, path: "/var/../proc/1/environ"},
		{name: "relative path", spec: nativeLogSpec("s1"), entries: []string{"/var/log/*"}, path: "x.log"},
		{name: "dot-relative path", spec: nativeLogSpec("s1"), entries: []string{"/var/log/*"}, path: "./x.log"},
		{name: "backslash UNC", spec: nativeLogSpec("s1"), entries: []string{`\\server\share\*`}, path: `\\server\share\x.log`},
		{name: "slash UNC", spec: nativeLogSpec("s1"), entries: []string{"//server/share"}, path: "//server/share/x.log"},
		{name: "device namespace", spec: nativeLogSpec("s1"), path: `\\.\pipe\x`},
		{name: "win32 file namespace", spec: nativeLogSpec("s1"), path: `\\?\C:\logs\x.log`},
		{
			name: "native spec gets no /mnt mapping", spec: nativeLogSpec("s1"), entries: []string{`C:\logs\*`},
			path: "/mnt/c/logs/x.log", wantSetting: true,
		},
		{name: "secret token", spec: nativeLogSpec("s1"), entries: []string{"/var/log/*"}, path: "/var/log/${API_TOKEN}.log"},
		{name: "agent env token", spec: nativeLogSpec("s1"), entries: []string{"/var/log/*"}, path: "/var/log/${AGENT_ENV:HOME}.log"},
		{name: "port near-miss", spec: nativeLogSpec("s1"), entries: []string{"/var/log/*"}, path: "/var/log/${PORTX}.log"},

		// WSL specs (§5.4).
		{
			name: "wsl config dir maps to the native dir", spec: wslLogSpec("s1"), path: "${CONFIG_DIR}/e.log",
			wantOpen: "/agent/cfg/s1/e.log", wantInCfg: true,
		},
		{
			name: "wsl config dir nested", spec: wslLogSpec("s1#3"), path: "${CONFIG_DIR}/a/e.log",
			wantOpen: "/agent/cfg/s1#3/a/e.log", wantInCfg: true,
		},
		{name: "wsl config dir dot-dot escape", spec: wslLogSpec("s1"), path: "${CONFIG_DIR}/../x.log", wantSetting: true},
		{name: "wsl config dir backslash", spec: wslLogSpec("s1"), path: `${CONFIG_DIR}/a\..\..\x.log`},
		{
			name: "wsl mnt path under a native entry", spec: wslLogSpec("s1"), entries: []string{`C:\logs\*`},
			path: "/mnt/c/logs/x.log", wantOpen: `C:\logs\x.log`,
		},
		{
			name: "wsl mnt path under a slash native entry", spec: wslLogSpec("s1"), entries: []string{"c:/logs/"},
			path: "/mnt/c/logs/app/x.log", wantOpen: `C:\logs\app\x.log`,
		},
		// Only the drive letter is case-folded; the rest compares like withinDir.
		{
			name: "wsl mnt path is case-sensitive past the drive", spec: wslLogSpec("s1"), entries: []string{`C:\Logs\*`},
			path: "/mnt/c/logs/x.log", wantSetting: true,
		},
		{name: "wsl mnt path with empty list", spec: wslLogSpec("s1"), path: "/mnt/c/logs/x.log", wantSetting: true},
		{
			name: "wsl mnt path shared-prefix sibling", spec: wslLogSpec("s1"), entries: []string{`C:\logs\*`},
			path: "/mnt/c/logsx/x.log", wantSetting: true,
		},
		{
			name: "wsl mnt path other drive", spec: wslLogSpec("s1"), entries: []string{`C:\logs\*`},
			path: "/mnt/d/logs/x.log", wantSetting: true,
		},
		{
			name: "wsl mnt path dot-dot escape", spec: wslLogSpec("s1"), entries: []string{`C:\logs\*`},
			path: "/mnt/c/logs/../Windows/x.log", wantSetting: true,
		},
		{
			name: "wsl mnt path drive-root star fails closed", spec: wslLogSpec("s1"), entries: []string{`C:\*`},
			path: "/mnt/c/logs/x.log", wantSetting: true,
		},
		{
			name: "wsl mnt path is not matched by a wsl entry", spec: wslLogSpec("s1"), entries: []string{"wsl://Ubuntu-24.04/mnt/c/logs/*"},
			path: "/mnt/c/logs/x.log", wantSetting: true,
		},
		{
			name: "wsl distro path fails closed under a matching entry", spec: wslLogSpec("s1"), entries: []string{"wsl://Ubuntu-24.04/home/u/*"},
			path: "/home/u/x.log", wantSetting: true,
		},
		{
			name: "wsl distro path fails closed under a native entry", spec: wslLogSpec("s1"), entries: []string{"/home/u/*"},
			path: "/home/u/x.log", wantSetting: true,
		},
		{name: "wsl relative path", spec: wslLogSpec("s1"), path: "x.log"},
		{
			name: "wsl UNC path", spec: wslLogSpec("s1"), entries: []string{`\\wsl.localhost\Ubuntu-24.04\home\u\*`},
			path: `\\wsl.localhost\Ubuntu-24.04\home\u\x.log`,
		},
		{name: "wsl secret token", spec: wslLogSpec("s1"), path: "${CONFIG_DIR}/${API_TOKEN}.log"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfgDir := logTestCfgDir
			if tc.cfgDir == "-" {
				cfgDir = ""
			}
			p := LocalPolicy{AllowedLogDirs: tc.entries}
			lf := LogFile{Name: "e", Path: tc.path}
			got, err := p.resolveLogFile(tc.spec, lf, 8080, GPUVendorNone, nil, cfgDir)
			if tc.wantOpen == "" {
				if err == nil {
					t.Fatalf("resolveLogFile(%q) = %+v, want a refusal", tc.path, got)
				}
				assertLogFileRefusal(t, err, tc.entries, tc.path, tc.wantSetting)
				return
			}
			if err != nil {
				t.Fatalf("resolveLogFile(%q): %v", tc.path, err)
			}
			if got.Name != "e" || got.OpenPath != tc.wantOpen || got.InConfigDir != tc.wantInCfg {
				t.Fatalf("resolveLogFile(%q) = %+v, want OpenPath %q InConfigDir %v", tc.path, got, tc.wantOpen, tc.wantInCfg)
			}
			if len(got.Roots) == 0 {
				t.Fatalf("resolveLogFile(%q) returned no roots", tc.path)
			}
		})
	}
}

// assertLogFileRefusal checks the refusal convention: wraps
// errLogFileNotPermitted, starts with "runtime: ", and never carries an
// allowlist path or the candidate path (those go to the agent log only).
func assertLogFileRefusal(t *testing.T, err error, entries []string, candidate string, wantSetting bool) {
	t.Helper()
	if !errors.Is(err, errLogFileNotPermitted) {
		t.Errorf("error %q does not wrap errLogFileNotPermitted", err)
	}
	msg := err.Error()
	if !strings.HasPrefix(msg, "runtime: ") {
		t.Errorf("error %q does not start with \"runtime: \"", msg)
	}
	if wantSetting {
		for _, s := range []string{"runtime_allowed_log_dirs", "OP_AGENT_RUNTIME_ALLOWED_LOG_DIRS"} {
			if !strings.Contains(msg, s) {
				t.Errorf("error %q does not name %s", msg, s)
			}
		}
	}
	leaks := []string{logTestCfgDir, "Ubuntu-24.04"}
	for _, e := range entries {
		leaks = append(leaks, e, allowedDirBase(e))
	}
	if !strings.Contains(candidate, "${") {
		leaks = append(leaks, candidate)
	}
	for _, l := range leaks {
		if len(l) >= 4 && strings.Contains(msg, l) {
			t.Errorf("error %q leaks the path %q", msg, l)
		}
	}
}

// openTarget resolves path for spec "s1" under cfgDir with entries and fails
// the test on a lexical refusal: the open tests exercise the second half.
func openTarget(t *testing.T, cfgDir string, entries []string, path string) logFileTarget {
	t.Helper()
	p := LocalPolicy{AllowedLogDirs: entries}
	target, err := p.resolveLogFile(nativeLogSpec("s1"), LogFile{Name: "e", Path: path}, 8080, GPUVendorNone, nil, cfgDir)
	if err != nil {
		t.Fatalf("resolveLogFile(%q): %v", path, err)
	}
	return target
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func mustSymlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}

// openWithin runs openLogFile with a deadline, so a regression that blocks
// (a FIFO opened without O_NONBLOCK) fails instead of hanging the package.
func openWithin(t *testing.T, target logFileTarget) (*os.File, error) {
	t.Helper()
	type result struct {
		f   *os.File
		err error
	}
	ch := make(chan result, 1)
	go func() {
		f, err := openLogFile(target)
		ch <- result{f, err}
	}()
	select {
	case r := <-ch:
		if r.f != nil {
			t.Cleanup(func() { _ = r.f.Close() })
		}
		return r.f, r.err
	case <-time.After(5 * time.Second):
		t.Fatalf("openLogFile(%q) blocked", target.OpenPath)
		return nil, nil
	}
}

func mustRefuseOpen(t *testing.T, target logFileTarget) {
	t.Helper()
	f, err := openWithin(t, target)
	if err == nil {
		t.Fatalf("openLogFile(%q) = %v, want a refusal", target.OpenPath, f.Name())
	}
	if !errors.Is(err, errLogFileNotPermitted) {
		t.Fatalf("openLogFile(%q) error %q does not wrap errLogFileNotPermitted", target.OpenPath, err)
	}
	if strings.Contains(err.Error(), target.OpenPath) || strings.Contains(err.Error(), filepath.Dir(target.OpenPath)) {
		t.Errorf("openLogFile error %q leaks a path", err)
	}
}

func mustOpenAndRead(t *testing.T, target logFileTarget, want string) {
	t.Helper()
	f, err := openWithin(t, target)
	if err != nil {
		t.Fatalf("openLogFile(%q): %v", target.OpenPath, err)
	}
	b, err := io.ReadAll(f)
	if err != nil || string(b) != want {
		t.Fatalf("read %q = %q, %v; want %q", target.OpenPath, b, err, want)
	}
}

// TestOpenLogFileRefusesSymlinkEscape is the resolved-path half (§5.2 steps
// 2, 3 and 5): a path that passes the lexical check but resolves outside the
// allowed tree, or is not a regular file, is refused at open, and refused again
// when a reopen finds the file swapped for an escaping link.
func TestOpenLogFileRefusesSymlinkEscape(t *testing.T) {
	if goruntime.GOOS == "windows" {
		t.Skip("creating symlinks needs a privilege on Windows; the Windows open path is covered by GOOS=windows vet")
	}
	dir := t.TempDir()
	cfgDir := filepath.Join(dir, "cfg")
	inst := filepath.Join(cfgDir, "s1")
	outside := filepath.Join(dir, "outside", "secret.txt")
	mustWrite(t, outside, "secret")
	mustWrite(t, filepath.Join(inst, "real.log"), "inside")

	t.Run("symlink to a file outside", func(t *testing.T) {
		mustSymlink(t, outside, filepath.Join(inst, "esc.log"))
		mustRefuseOpen(t, openTarget(t, cfgDir, nil, "${CONFIG_DIR}/esc.log"))
	})
	t.Run("relative symlink climbing out", func(t *testing.T) {
		mustSymlink(t, "../../outside/secret.txt", filepath.Join(inst, "rel.log"))
		mustRefuseOpen(t, openTarget(t, cfgDir, nil, "${CONFIG_DIR}/rel.log"))
	})
	t.Run("symlinked directory leading outside", func(t *testing.T) {
		mustSymlink(t, filepath.Dir(outside), filepath.Join(inst, "sub"))
		mustRefuseOpen(t, openTarget(t, cfgDir, nil, "${CONFIG_DIR}/sub/secret.txt"))
	})
	t.Run("symlink to a file inside", func(t *testing.T) {
		mustSymlink(t, filepath.Join(inst, "real.log"), filepath.Join(inst, "in.log"))
		mustOpenAndRead(t, openTarget(t, cfgDir, nil, "${CONFIG_DIR}/in.log"), "inside")
	})
	t.Run("regular file", func(t *testing.T) {
		mustOpenAndRead(t, openTarget(t, cfgDir, nil, "${CONFIG_DIR}/real.log"), "inside")
	})
	t.Run("directory", func(t *testing.T) {
		if err := os.MkdirAll(filepath.Join(inst, "d.log"), 0o755); err != nil {
			t.Fatal(err)
		}
		mustRefuseOpen(t, openTarget(t, cfgDir, nil, "${CONFIG_DIR}/d.log"))
	})
	t.Run("symlink to a device", func(t *testing.T) {
		mustSymlink(t, os.DevNull, filepath.Join(inst, "null.log"))
		mustRefuseOpen(t, openTarget(t, cfgDir, nil, "${CONFIG_DIR}/null.log"))
	})
	t.Run("symlink into proc", func(t *testing.T) {
		if _, err := os.Stat("/proc/self/environ"); err != nil {
			t.Skip("no /proc on this host")
		}
		// Allowed everywhere the lexical check looks: the link sits in the
		// instance's own directory; /proc is refused on the resolved path.
		mustSymlink(t, "/proc/self/environ", filepath.Join(inst, "env.log"))
		mustRefuseOpen(t, openTarget(t, cfgDir, nil, "${CONFIG_DIR}/env.log"))
	})
	t.Run("reopen after a swap to an escaping link", func(t *testing.T) {
		p := filepath.Join(inst, "rot.log")
		mustWrite(t, p, "gen1")
		target := openTarget(t, cfgDir, nil, "${CONFIG_DIR}/rot.log")
		mustOpenAndRead(t, target, "gen1")
		if err := os.Remove(p); err != nil {
			t.Fatal(err)
		}
		mustSymlink(t, outside, p)
		mustRefuseOpen(t, target)
	})
	t.Run("missing file is not a refusal", func(t *testing.T) {
		target := openTarget(t, cfgDir, nil, "${CONFIG_DIR}/missing.log")
		_, err := openWithin(t, target)
		if !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("openLogFile(missing) = %v, want fs.ErrNotExist", err)
		}
		if errors.Is(err, errLogFileNotPermitted) {
			t.Fatalf("a missing file must not read as not permitted: %v", err)
		}
		if strings.Contains(err.Error(), dir) {
			t.Errorf("openLogFile error %q leaks a path", err)
		}
	})
}

// TestOpenLogFileRootIsSymlink: an allowed root that is itself a symlink (a
// mounted volume, macOS's /var -> /private/var) compares correctly, because
// the roots are resolved too; a link under it that escapes is still refused.
func TestOpenLogFileRootIsSymlink(t *testing.T) {
	if goruntime.GOOS == "windows" {
		t.Skip("creating symlinks needs a privilege on Windows")
	}
	dir := t.TempDir()
	realDir := filepath.Join(dir, "real")
	link := filepath.Join(dir, "link")
	mustWrite(t, filepath.Join(realDir, "x.log"), "via link")
	mustSymlink(t, realDir, link)
	outside := filepath.Join(dir, "outside.txt")
	mustWrite(t, outside, "secret")
	mustSymlink(t, outside, filepath.Join(realDir, "esc.log"))
	cfgDir := filepath.Join(dir, "cfg")

	for _, entry := range []string{link + "/*", link} {
		mustOpenAndRead(t, openTarget(t, cfgDir, []string{entry}, filepath.Join(link, "x.log")), "via link")
		mustRefuseOpen(t, openTarget(t, cfgDir, []string{entry}, filepath.Join(link, "esc.log")))
	}

	// The instance's config dir reached through a symlinked config_files_dir.
	realCfg := filepath.Join(dir, "realcfg")
	mustWrite(t, filepath.Join(realCfg, "s1", "e.log"), "cfg via link")
	linkCfg := filepath.Join(dir, "linkcfg")
	mustSymlink(t, realCfg, linkCfg)
	mustOpenAndRead(t, openTarget(t, linkCfg, nil, "${CONFIG_DIR}/e.log"), "cfg via link")
}

// TestFileIdentity: the same file reports the same identity through two
// stats, a different file a different one.
func TestFileIdentity(t *testing.T) {
	if goruntime.GOOS == "windows" {
		t.Skip("os.FileInfo carries no file index on Windows; see fileIdentity's doc")
	}
	dir := t.TempDir()
	a, b := filepath.Join(dir, "a"), filepath.Join(dir, "b")
	mustWrite(t, a, "a")
	mustWrite(t, b, "b")
	fa1, _ := os.Stat(a)
	f, err := os.Open(a)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	fa2, _ := f.Stat()
	fb, _ := os.Stat(b)
	i1, d1, ok1 := fileIdentity(fa1)
	i2, d2, ok2 := fileIdentity(fa2)
	i3, d3, ok3 := fileIdentity(fb)
	if !ok1 || !ok2 || !ok3 {
		t.Fatalf("fileIdentity not available: %v %v %v", ok1, ok2, ok3)
	}
	if i1 != i2 || d1 != d2 {
		t.Errorf("same file: (%d,%d) != (%d,%d)", i1, d1, i2, d2)
	}
	if i1 == i3 && d1 == d3 {
		t.Errorf("different files share identity (%d,%d)", i1, d1)
	}
}
