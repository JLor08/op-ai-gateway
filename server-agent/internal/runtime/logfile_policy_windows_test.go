// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

//go:build windows

package runtime

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// These tests exercise the Windows half of the log-file policy (#430): the
// os.Root-anchored open with its GetFileType check, junctions (which need no
// privilege, unlike symlinks, and which os.Root never crosses below a root),
// the link count, the share mode and the Windows naming rules. CI runs no Windows job; they compile and vet under
// GOOS=windows and run on a real Windows host as part of the PR's acceptance
// checklist.

// mklinkJunction creates a directory junction link -> target.
func mklinkJunction(t *testing.T, link, target string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput(); err != nil {
		t.Fatalf("mklink /J %s %s: %v: %s", link, target, err, out)
	}
}

// TestOpenLogFileWindowsJunctionEscape: a junction inside ${CONFIG_DIR}
// pointing outside is refused by the root-anchored open; an instance dir
// replaced by a junction is neither a root nor a place the agent deletes in.
func TestOpenLogFileWindowsJunctionEscape(t *testing.T) {
	dir := t.TempDir()
	cfgDir := filepath.Join(dir, "cfg")
	inst := filepath.Join(cfgDir, "s1")
	outsideDir := filepath.Join(dir, "outside")
	mustWrite(t, filepath.Join(outsideDir, "secret.txt"), "secret")
	mustWrite(t, filepath.Join(inst, "keep"), "")

	mklinkJunction(t, filepath.Join(inst, "sub"), outsideDir)
	target := openTarget(t, cfgDir, nil, `${CONFIG_DIR}\sub\secret.txt`)
	if target.InConfigDir {
		t.Fatalf("target %+v: a file in a subdirectory must not be InConfigDir", target)
	}
	mustRefuseOpen(t, target)

	cfg2 := filepath.Join(dir, "cfg2")
	mklinkJunction(t, filepath.Join(cfg2, "s1"), outsideDir)
	target = openTarget(t, cfg2, nil, `${CONFIG_DIR}\secret.txt`)
	mustRefuseOpen(t, target)
	if err := removeConfigDirLogFile(target); !errors.Is(err, errLogFileNotPermitted) {
		t.Fatalf("removeConfigDirLogFile through a junctioned instance dir = %v, want a refusal", err)
	}
	if _, err := os.Stat(filepath.Join(outsideDir, "secret.txt")); err != nil {
		t.Fatalf("the junction target must survive: %v", err)
	}
}

// TestOpenLogFileWindowsJunctionRoot: an allowed root that is a junction
// works, because os.OpenRoot follows links in the root's own name (operator
// configuration).
func TestOpenLogFileWindowsJunctionRoot(t *testing.T) {
	dir := t.TempDir()
	realDir := filepath.Join(dir, "real")
	link := filepath.Join(dir, "link")
	mustWrite(t, filepath.Join(realDir, "x.log"), "via junction")
	mklinkJunction(t, link, realDir)
	for _, entry := range []string{link + `\*`, link} {
		mustOpenAndRead(t, openTarget(t, filepath.Join(dir, "cfg"), []string{entry}, filepath.Join(link, "x.log")), "via junction")
	}
}

// TestOpenLogFileWindowsHardLink: NTFS hard links need no privilege, so the
// link-count refusal is exercised for real.
func TestOpenLogFileWindowsHardLink(t *testing.T) {
	dir := t.TempDir()
	cfgDir := filepath.Join(dir, "cfg")
	outside := filepath.Join(dir, "outside", "secret.txt")
	mustWrite(t, outside, "secret")
	mustWrite(t, filepath.Join(cfgDir, "s1", "keep"), "")
	if err := os.Link(outside, filepath.Join(cfgDir, "s1", "h.log")); err != nil {
		t.Skipf("link: %v", err)
	}
	mustRefuseOpen(t, openTarget(t, cfgDir, nil, `${CONFIG_DIR}\h.log`))
}

// TestOpenLogFileWindowsShareMode: while the agent holds the file open, the
// server can still append to it, rename it and delete it (§5.2 step 4).
func TestOpenLogFileWindowsShareMode(t *testing.T) {
	dir := t.TempDir()
	cfgDir := filepath.Join(dir, "cfg")
	p := filepath.Join(cfgDir, "s1", "e.log")
	mustWrite(t, p, "gen1")
	if _, err := openWithin(t, openTarget(t, cfgDir, nil, `${CONFIG_DIR}\e.log`)); err != nil {
		t.Fatal(err)
	}
	w, err := os.OpenFile(p, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatalf("server append while tailed: %v", err)
	}
	if _, err := w.WriteString("more"); err != nil {
		t.Fatal(err)
	}
	_ = w.Close()
	if err := os.Rename(p, p+".1"); err != nil {
		t.Fatalf("server rename while tailed: %v", err)
	}
	if err := os.Remove(p + ".1"); err != nil {
		t.Fatalf("server delete while tailed: %v", err)
	}
}

// TestResolveLogFileWindowsNames runs Windows-native paths through both
// halves: every refusal is lexical (resolveLogFile), and a positive control
// proves the table's base is usable. A direct openLogFile of a reserved name,
// bypassing the lexical half, is refused at open as defence in depth.
func TestResolveLogFileWindowsNames(t *testing.T) {
	dir := t.TempDir()
	cfgDir := filepath.Join(dir, "cfg")
	inst := filepath.Join(cfgDir, "s1")
	mustWrite(t, filepath.Join(inst, "e.log"), "ok")
	mustOpenAndRead(t, openTarget(t, cfgDir, nil, `${CONFIG_DIR}\e.log`), "ok")

	flipped := strings.ToLower(inst[:1]) + inst[1:]
	if flipped == inst {
		flipped = strings.ToUpper(inst[:1]) + inst[1:]
	}
	for _, path := range []string{
		// Native containment is case-sensitive like withinDir: fail closed.
		flipped + `\e.log`,
		`\\?\` + inst + `\e.log`,
		`\\.\` + inst + `\e.log`,
		`${CONFIG_DIR}\e.log.`,
		`${CONFIG_DIR}\e.log `,
		`${CONFIG_DIR}\NUL`,
		`${CONFIG_DIR}\nul.txt`,
		`${CONFIG_DIR}\COM1`,
		`${CONFIG_DIR}\CONOUT$`,
		`${CONFIG_DIR}\e.log:stream`,
		`${CONFIG_DIR}\..\e.log`,
		`\logs\e.log`,
		"/var/log/e.log",
	} {
		t.Run(path, func(t *testing.T) {
			p := LocalPolicy{}
			got, err := p.resolveLogFile(nativeLogSpec("s1"), LogFile{Name: "e", Path: path}, 8080, GPUTopology{}, cfgDir)
			if err == nil {
				t.Fatalf("resolveLogFile(%q) = %+v, want a refusal", path, got)
			}
			if !errors.Is(err, errLogFileNotPermitted) {
				t.Fatalf("resolveLogFile(%q) error %q does not wrap errLogFileNotPermitted", path, err)
			}
			if strings.Contains(err.Error(), dir) {
				t.Errorf("error %q leaks a path", err)
			}
		})
	}

	// A target that names a reserved device below its root, bypassing the
	// lexical half, is refused at open: filepath.IsLocal rejects "NUL" on
	// Windows before os.Root (which refuses reserved names too) or
	// GetFileType see it.
	mustRefuseOpen(t, logFileTarget{
		Name: "e", OpenPath: filepath.Join(inst, "NUL"), Root: inst, Rel: "NUL", Roots: []string{inst},
		ConfigBase: cfgDir, ConfigDir: inst,
	})
}
