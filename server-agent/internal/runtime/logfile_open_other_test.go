// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

//go:build !windows

package runtime

import (
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
