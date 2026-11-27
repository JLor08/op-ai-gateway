// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

//go:build !windows

package runtime

import (
	"errors"
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

// TestOpenLogFileRaceAfterFinalPath drives the name changes a server can time
// with inotify IN_OPEN to the one moment they matter: right after the agent
// computed the opened file's final path (the stageAfterFinalPath seam). The
// file the agent holds must never be one whose inside name is gone while an
// outside name remains, nor one that gained a second name; a file with no
// name left is fs.ErrNotExist (the tailer retries it), not a refusal.
func TestOpenLogFileRaceAfterFinalPath(t *testing.T) {
	cases := []struct {
		name     string
		race     func(t *testing.T, inside, outsideDir string)
		vanished bool
	}{
		{name: "inside name unlinked, outside link remains", race: func(t *testing.T, inside, outsideDir string) {
			if err := os.Link(inside, filepath.Join(outsideDir, "moved")); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(inside); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "second link added", race: func(t *testing.T, inside, outsideDir string) {
			if err := os.Link(inside, filepath.Join(outsideDir, "extra")); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "file removed entirely", vanished: true, race: func(t *testing.T, inside, _ string) {
			if err := os.Remove(inside); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "rotated: a new file at the same name", vanished: true, race: func(t *testing.T, inside, _ string) {
			if err := os.Remove(inside); err != nil {
				t.Fatal(err)
			}
			mustWrite(t, inside, "gen2")
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
			logFileOpenSeam = func(stage logFileOpenStage) {
				if stage == stageAfterFinalPath {
					tc.race(t, inside, outsideDir)
				}
			}
			t.Cleanup(func() { logFileOpenSeam = nil })

			if tc.vanished {
				_, err := openWithin(t, target)
				if !errors.Is(err, fs.ErrNotExist) || errors.Is(err, errLogFileNotPermitted) {
					t.Fatalf("openLogFile = %v, want fs.ErrNotExist and not a refusal", err)
				}
				return
			}
			mustRefuseOpen(t, target)
		})
	}
}

// TestOpenLogFileRaceToggledHardLink: a server that keeps toggling an inside
// hard link to an outside file can show each separate syscall a different
// state. Here the inside name is absent when the descriptor's link count is
// read (step 1), back when the final path is computed and bound (steps 2-3),
// and absent again for the second count (step 4). Only a binding that reads
// identity and link count from one stat refuses it.
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
	unlink := func() {
		if err := os.Remove(inside); err != nil {
			t.Error(err)
		}
	}
	logFileOpenSeam = func(stage logFileOpenStage) {
		switch stage {
		case stageBeforeLinkCount, stageAfterBinding:
			unlink()
		case stageAfterLinkCount:
			if err := os.Link(outside, inside); err != nil {
				t.Error(err)
			}
		case stageAfterFinalPath:
		}
	}
	t.Cleanup(func() { logFileOpenSeam = nil })
	mustRefuseOpen(t, target)
}

// TestOpenLogFileFinalPathVanished: a file deleted between the link count
// and the final path (where off Linux the path is resolved by name) is
// fs.ErrNotExist, which the tailer retries, not a refusal.
func TestOpenLogFileFinalPathVanished(t *testing.T) {
	dir := t.TempDir()
	cfgDir := filepath.Join(dir, "cfg")
	inside := filepath.Join(cfgDir, "s1", "e.log")
	mustWrite(t, inside, "gen1")
	target := openTarget(t, cfgDir, nil, "${CONFIG_DIR}/e.log")
	logFileOpenSeam = func(stage logFileOpenStage) {
		if stage == stageAfterLinkCount {
			if err := os.Remove(inside); err != nil {
				t.Error(err)
			}
		}
	}
	t.Cleanup(func() { logFileOpenSeam = nil })
	_, err := openWithin(t, target)
	if !errors.Is(err, fs.ErrNotExist) || errors.Is(err, errLogFileNotPermitted) {
		t.Fatalf("openLogFile = %v, want fs.ErrNotExist and not a refusal", err)
	}
}
