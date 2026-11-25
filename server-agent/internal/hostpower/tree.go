// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package hostpower

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	// DomainDir is the powercap domain directory a guest's RAPL collector reads.
	DomainDir = "intel-rapl:0"
	// MaxEnergyRangeUJ is the advertised counter range (max int64, no wrap in practice).
	MaxEnergyRangeUJ uint64 = 9223372036854775807
)

// WriteTree publishes the synthetic powercap domain under vmDir. Static files
// are rewritten only when absent or different; energy_uj is replaced atomically
// (temp file + rename in the same directory) because the guest reads it
// concurrently over virtiofs.
func WriteTree(vmDir string, energyUJ uint64) error {
	dir := filepath.Join(vmDir, DomainDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("hostpower: write %s: %w", dir, err)
	}
	static := []struct{ name, content string }{
		{"name", "package-0\n"},
		{"max_energy_range_uj", strconv.FormatUint(MaxEnergyRangeUJ, 10) + "\n"},
	}
	for _, f := range static {
		path := filepath.Join(dir, f.name)
		if cur, err := os.ReadFile(path); err == nil && string(cur) == f.content {
			continue
		}
		if err := writeAtomic(dir, f.name, f.content); err != nil {
			return fmt.Errorf("hostpower: write %s: %w", path, err)
		}
	}
	if err := writeAtomic(dir, "energy_uj", strconv.FormatUint(energyUJ, 10)+"\n"); err != nil {
		return fmt.Errorf("hostpower: write %s: %w", filepath.Join(dir, "energy_uj"), err)
	}
	return nil
}

// writeAtomic writes content to dir/name via a temp file and rename, removing
// the temp file on any error.
func writeAtomic(dir, name, content string) (err error) {
	tmp, err := os.CreateTemp(dir, "."+name+".*")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = os.Remove(tmp.Name())
		}
	}()
	if _, err = tmp.WriteString(content); err != nil {
		_ = tmp.Close()
		return err
	}
	if err = tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		return err
	}
	if err = tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(dir, name))
}

// ReadCounter returns the persisted energy_uj under vmDir, or 0 when it is
// absent, empty or unparsable.
func ReadCounter(vmDir string) uint64 {
	b, err := os.ReadFile(filepath.Join(vmDir, DomainDir, "energy_uj"))
	if err != nil {
		return 0
	}
	v, err := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
	if err != nil {
		return 0
	}
	return v
}
