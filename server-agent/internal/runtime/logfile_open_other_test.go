// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

//go:build !windows

package runtime

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// TestOpenLogFileRefusesFIFO: a FIFO at a declared path is refused, and the
// open does not block waiting for a writer (O_NONBLOCK, then a stat of the
// opened descriptor). openWithin fails the test if the open hangs.
func TestOpenLogFileRefusesFIFO(t *testing.T) {
	dir := t.TempDir()
	cfgDir := filepath.Join(dir, "cfg")
	fifo := filepath.Join(cfgDir, "s1", "f.log")
	mustWrite(t, filepath.Join(cfgDir, "s1", "keep"), "")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	mustRefuseOpen(t, openTarget(t, cfgDir, nil, "${CONFIG_DIR}/f.log"))
}

// mustVanish asserts the open was given up as errLogFileVanished (an
// fs.ErrNotExist the tailer retries), not a refusal, and returned no file.
func mustVanish(t *testing.T, target logFileTarget) {
	t.Helper()
	f, err := openWithin(t, target)
	if f != nil {
		t.Fatalf("openLogFile(%q) returned a file, want it given up as vanished", target.OpenPath)
	}
	if !errors.Is(err, fs.ErrNotExist) || errors.Is(err, errLogFileNotPermitted) {
		t.Fatalf("openLogFile = %v, want fs.ErrNotExist and not a refusal", err)
	}
}

// TestOpenLogFileRaceAfterLinkCount drives the name changes a server can time
// with inotify IN_OPEN to the moment after the descriptor's link count was
// read and before the name is bound (the stageAfterLinkCount seam). The file
// the agent holds must never be one that gained a second name (a refusal); a
// name that is gone, or that now names something else, is a rotation or a
// delete in the window: errLogFileVanished, which the tailer retries, never a
// refusal that would stop it for good.
func TestOpenLogFileRaceAfterLinkCount(t *testing.T) {
	cases := []struct {
		name    string
		race    func(t *testing.T, inside, outsideDir string)
		refused bool
	}{
		{name: "inside name unlinked, outside link remains", race: func(t *testing.T, inside, outsideDir string) {
			mustLink(t, inside, filepath.Join(outsideDir, "moved"))
			mustRemove(t, inside)
		}},
		{name: "second link added", refused: true, race: func(t *testing.T, inside, outsideDir string) {
			mustLink(t, inside, filepath.Join(outsideDir, "extra"))
		}},
		{name: "file removed entirely", race: func(t *testing.T, inside, _ string) {
			mustRemove(t, inside)
		}},
		{name: "rotated: a new file at the same name", race: func(t *testing.T, inside, _ string) {
			mustRemove(t, inside)
			mustWrite(t, inside, "gen2")
		}},
		{name: "rename-rotated: renamed aside, a new file at the name", race: func(t *testing.T, inside, _ string) {
			if err := os.Rename(inside, inside+".1"); err != nil {
				t.Fatal(err)
			}
			mustWrite(t, inside, "gen2")
		}},
		{name: "replaced by a directory", race: func(t *testing.T, inside, _ string) {
			mustRemove(t, inside)
			if err := os.Mkdir(inside, 0o755); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "replaced by a symlink leading out", race: func(t *testing.T, inside, outsideDir string) {
			mustRemove(t, inside)
			mustSymlink(t, filepath.Join(outsideDir, "keep"), inside)
		}},
		{name: "replaced by an inside symlink to another file", race: func(t *testing.T, inside, _ string) {
			mustWrite(t, inside+".other", "other")
			mustRemove(t, inside)
			mustSymlink(t, filepath.Base(inside)+".other", inside)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			cfgDir := filepath.Join(dir, "cfg")
			inside := filepath.Join(cfgDir, "s1", "h.log")
			outsideDir := filepath.Join(dir, "outside")
			mustWrite(t, inside, "gen1")
			mustWrite(t, filepath.Join(outsideDir, "keep"), "")
			target := openTarget(t, cfgDir, nil, "${CONFIG_DIR}/h.log")
			setLogFileOpenSeam(t, func(stage logFileOpenStage) {
				if stage == stageAfterLinkCount {
					tc.race(t, inside, outsideDir)
				}
			})
			if tc.refused {
				mustRefuseOpen(t, target)
				return
			}
			mustVanish(t, target)
		})
	}
}

// TestOpenLogFileRaceToggledHardLink: a server that keeps toggling an inside
// hard link to an outside file can show each separate syscall a different
// state. Here the inside name is absent when the descriptor's link count is
// read and back when the name is bound: the binding's own link count, read in
// the same observation that finds the name, refuses it.
func TestOpenLogFileRaceToggledHardLink(t *testing.T) {
	dir := t.TempDir()
	cfgDir := filepath.Join(dir, "cfg")
	inside := filepath.Join(cfgDir, "s1", "h.log")
	outside := filepath.Join(dir, "outside", "secret.txt")
	mustWrite(t, outside, "secret")
	mustWrite(t, filepath.Join(cfgDir, "s1", "keep"), "")
	if err := os.Link(outside, inside); err != nil {
		t.Skipf("link: %v", err)
	}
	target := openTarget(t, cfgDir, nil, "${CONFIG_DIR}/h.log")
	setLogFileOpenSeam(t, func(stage logFileOpenStage) {
		switch stage {
		case stageAfterOpen:
			mustRemove(t, inside)
		case stageAfterLinkCount:
			mustLink(t, outside, inside)
		}
	})
	mustRefuseOpen(t, target)
}

// TestOpenLogFileRaceParentSwap is the re-review's attack on 0eda84ae
// (accepted there on darwin, where the secret was read): ${CONFIG_DIR}/sub/h.log
// is a hard link to an outside secret also named h.log; the server unlinks the
// inside name before the link count (so the count reads 1) and then swaps the
// PARENT sub for a link to the outside directory before the name is bound, so
// that a path-based look-up of .../sub/h.log finds the secret under its only
// name. A root-anchored binding never follows that link out: the open is
// given up (vanished) or refused, and nothing is read.
func TestOpenLogFileRaceParentSwap(t *testing.T) {
	for _, tc := range []struct {
		name string
		link func(outsideDir string) string
	}{
		{name: "absolute link to the outside dir", link: func(outsideDir string) string { return outsideDir }},
		{name: "relative link climbing out", link: func(string) string { return "../../outside" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			cfgDir := filepath.Join(dir, "cfg")
			sub := filepath.Join(cfgDir, "s1", "sub")
			outsideDir := filepath.Join(dir, "outside")
			secret := filepath.Join(outsideDir, "h.log")
			inside := filepath.Join(sub, "h.log")
			mustWrite(t, secret, "secret")
			mustWrite(t, filepath.Join(sub, "keep"), "")
			if err := os.Link(secret, inside); err != nil {
				t.Skipf("link: %v", err)
			}
			target := openTarget(t, cfgDir, nil, "${CONFIG_DIR}/sub/h.log")
			setLogFileOpenSeam(t, func(stage logFileOpenStage) {
				switch stage {
				case stageAfterOpen:
					mustRemove(t, inside)
				case stageAfterLinkCount:
					if err := os.Rename(sub, sub+".old"); err != nil {
						t.Fatal(err)
					}
					mustSymlink(t, tc.link(outsideDir), sub)
				}
			})
			f, err := openWithin(t, target)
			if f != nil {
				b, _ := io.ReadAll(f)
				t.Fatalf("openLogFile accepted the swapped file and read %q, want it given up or refused", b)
			}
			if !errors.Is(err, fs.ErrNotExist) && !errors.Is(err, errLogFileNotPermitted) {
				t.Fatalf("openLogFile = %v, want errLogFileVanished or a refusal", err)
			}
			t.Logf("openLogFile = %v", err)
			// And the reopen that follows refuses the lasting escape.
			setLogFileOpenSeam(t, nil)
			mustRefuseOpen(t, target)
		})
	}
}

// TestOpenLogFileVanishedAfterOpen: a file deleted right after the open has
// a link count of 0, which is errLogFileVanished (the tailer retries), not a
// refusal.
func TestOpenLogFileVanishedAfterOpen(t *testing.T) {
	dir := t.TempDir()
	cfgDir := filepath.Join(dir, "cfg")
	inside := filepath.Join(cfgDir, "s1", "e.log")
	mustWrite(t, inside, "gen1")
	target := openTarget(t, cfgDir, nil, "${CONFIG_DIR}/e.log")
	setLogFileOpenSeam(t, func(stage logFileOpenStage) {
		if stage == stageAfterOpen {
			mustRemove(t, inside)
		}
	})
	mustVanish(t, target)
}

// setLogFileOpenSeam installs seam for this test and clears it afterwards.
// None of the tests that use it run in parallel.
func setLogFileOpenSeam(t *testing.T, seam func(logFileOpenStage)) {
	t.Helper()
	logFileOpenSeam = seam
	t.Cleanup(func() { logFileOpenSeam = nil })
}

func mustLink(t *testing.T, oldname, newname string) {
	t.Helper()
	if err := os.Link(oldname, newname); err != nil {
		t.Fatal(err)
	}
}

func mustRemove(t *testing.T, name string) {
	t.Helper()
	if err := os.Remove(name); err != nil {
		t.Fatal(err)
	}
}
