// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package hostpower

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	// DomainDir is the powercap domain directory a guest's RAPL collector reads.
	DomainDir = "intel-rapl:0"
	// WindowsDir is the per-VM subtree a Windows guest reads. Windows cannot
	// create or open names containing ':', so it mirrors the Linux tree under a
	// colon-free domain directory name.
	WindowsDir = "windows"
	// WindowsDomainDir is DomainDir without the colon.
	WindowsDomainDir = "intel-rapl_0"
	// MaxEnergyRangeUJ is the advertised counter range. It is 0 on purpose: the
	// guest's RAPL collector treats a counter decrease as a wraparound only when
	// the range exceeds the previous value, so with 0 any host-side regression
	// (a restart that lost the counter, a reset tree) reads as 0 J instead of a
	// near-full-range delta (about 9 TW at max int64). A uint64 microjoule
	// counter cannot wrap in practice, so no real wraparound is lost.
	MaxEnergyRangeUJ uint64 = 0
)

// maxTreeFileBytes caps what the service ever reads from a VM tree: a uint64
// (20 digits) plus a newline fits with room to spare. A guest can place any
// file in its share, so a larger file, or anything that is not a regular file
// (a FIFO would block a read forever), is never read.
const maxTreeFileBytes = 64

// treeFiles are the files of the synthetic domain; their temp files are named
// "."+name+".<random>" in DomainDir.
var treeFiles = []string{"name", "max_energy_range_uj", "energy_uj"}

// openVMRoot opens vmDir as an os.Root through its parent (the out directory),
// creating vmDir first when create is set. vmDir must be a real directory, not
// a symlink. Every later access goes through the returned Root, so a symlink a
// guest planted in its share (vmDir is shared read-write over virtiofs) can
// never make this root service read or write outside vmDir.
func openVMRoot(vmDir string, create bool) (*os.Root, error) {
	out, err := os.OpenRoot(filepath.Dir(vmDir))
	if err != nil {
		return nil, err
	}
	defer func() { _ = out.Close() }()
	name := filepath.Base(vmDir)
	if create {
		if err := out.MkdirAll(name, 0o755); err != nil {
			return nil, err
		}
	}
	fi, err := out.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("%s is not a directory", vmDir)
	}
	return out.OpenRoot(name)
}

// windowsDomainPath is the Windows domain directory relative to a VM root.
var windowsDomainPath = path.Join(WindowsDir, WindowsDomainDir)

// WriteTree publishes the synthetic powercap domain under vmDir. Static files
// are rewritten only when absent or different; energy_uj is replaced every call
// (temp file + rename in the same directory, so a concurrent guest read over
// virtiofs sees the old or the new value, never a partial one; the rewrite also
// keeps the mtime fresh). There is no fsync: durability across a host power loss
// does not matter because the guests reboot with the host and re-baseline, and
// one fsync per VM per tick would load the hypervisor's system disk. All access
// is confined to vmDir; a symlink that escapes it makes WriteTree fail without
// touching the link target.
func WriteTree(vmDir string, energyUJ uint64) error {
	root, err := openVMRoot(vmDir, true)
	if err != nil {
		return fmt.Errorf("hostpower: write %s: %w", vmDir, err)
	}
	defer func() { _ = root.Close() }()
	if err := writeDomain(root, vmDir, DomainDir, energyUJ); err != nil {
		return err
	}
	// The Windows domain goes through nested paths in the same Root, never by
	// opening windows/ as a Root of its own: Root.OpenRoot opens a leaf without
	// O_DIRECTORY or O_NONBLOCK, so a FIFO a guest swapped in would block the
	// tick forever. Its failure is returned only after the Linux tree is
	// written, so a guest that breaks its Windows subtree never freezes its
	// Linux counter.
	return writeDomain(root, vmDir, windowsDomainPath, energyUJ)
}

// writeDomain publishes one synthetic domain directory inside root; base is
// root's path, used only in error messages.
func writeDomain(root *os.Root, base, domainDir string, energyUJ uint64) error {
	if err := root.MkdirAll(domainDir, 0o755); err != nil {
		return fmt.Errorf("hostpower: write %s: %w", filepath.Join(base, domainDir), err)
	}
	static := []struct{ name, content string }{
		{"name", "package-0\n"},
		{"max_energy_range_uj", strconv.FormatUint(MaxEnergyRangeUJ, 10) + "\n"},
	}
	for _, f := range static {
		rel := path.Join(domainDir, f.name)
		// Anything but a small regular file with the expected content (absent,
		// different, a FIFO, a symlink, oversized) is replaced by the rename.
		if cur, ok := readSmall(root, rel); ok && string(cur) == f.content {
			continue
		}
		if err := writeAtomic(root, domainDir, f.name, f.content); err != nil {
			return fmt.Errorf("hostpower: write %s: %w", filepath.Join(base, rel), err)
		}
	}
	if err := writeAtomic(root, domainDir, "energy_uj", strconv.FormatUint(energyUJ, 10)+"\n"); err != nil {
		return fmt.Errorf("hostpower: write %s: %w", filepath.Join(base, domainDir, "energy_uj"), err)
	}
	return nil
}

// writeAtomic writes content to domainDir/name inside root via a freshly
// created (O_EXCL) temp file and a rename, removing the temp file on any error.
// rename replaces a symlink at the destination rather than following it.
func writeAtomic(root *os.Root, domainDir, name, content string) (err error) {
	tmp, f, err := createTemp(root, domainDir, name)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = root.Remove(tmp)
		}
	}()
	if _, err = f.WriteString(content); err != nil {
		_ = f.Close()
		return err
	}
	if err = f.Chmod(0o644); err != nil {
		_ = f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return root.Rename(tmp, path.Join(domainDir, name))
}

// createTemp creates domainDir/."+name+".<random> exclusively inside root.
func createTemp(root *os.Root, domainDir, name string) (string, *os.File, error) {
	for range 10 {
		var b [8]byte
		if _, err := rand.Read(b[:]); err != nil {
			return "", nil, err
		}
		tmp := path.Join(domainDir, "."+name+"."+hex.EncodeToString(b[:]))
		f, err := root.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return "", nil, err
		}
		return tmp, f, nil
	}
	return "", nil, fmt.Errorf("create temp file for %s: too many collisions", name)
}

// readSmall returns the content of rel inside root only when it is a regular
// file of at most maxTreeFileBytes. Lstat rejects special and oversized files
// before the open; the open is non-blocking (where the OS has O_NONBLOCK) and
// the opened file is checked again with Stat and read through a bounded reader,
// so swapping the file between Lstat and open cannot block or flood the read.
func readSmall(root *os.Root, rel string) ([]byte, bool) {
	fi, err := root.Lstat(rel)
	if err != nil || !fi.Mode().IsRegular() || fi.Size() > maxTreeFileBytes {
		return nil, false
	}
	f, err := root.OpenFile(rel, os.O_RDONLY|oNonblock, 0)
	if err != nil {
		return nil, false
	}
	defer func() { _ = f.Close() }()
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		return nil, false
	}
	b, err := io.ReadAll(io.LimitReader(f, maxTreeFileBytes+1))
	if err != nil || len(b) > maxTreeFileBytes {
		return nil, false
	}
	return b, true
}

// ReadCounter returns the persisted energy_uj under vmDir, or 0 when it is
// absent, empty, unparsable, not a small regular file, or reachable only
// through a symlink that escapes vmDir.
func ReadCounter(vmDir string) uint64 {
	root, err := openVMRoot(vmDir, false)
	if err != nil {
		return 0
	}
	defer func() { _ = root.Close() }()
	b, ok := readSmall(root, path.Join(DomainDir, "energy_uj"))
	if !ok {
		return 0
	}
	v, err := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
	if err != nil {
		return 0
	}
	return v
}

// RemoveStaleTemps deletes leftover temp files (from a crash between create
// and rename) in both domain directories (DomainDir and the Windows one) of
// every VM directory directly under outDir. It runs once at startup, confined
// to each VM directory like WriteTree; a VM directory or a domain directory
// that is missing, not a real directory or escapes is skipped. It returns the
// joined errors of the removals it attempted.
func RemoveStaleTemps(outDir string) error {
	entries, err := os.ReadDir(outDir)
	if err != nil {
		return err
	}
	var errs []error
	for _, e := range entries {
		if !e.IsDir() { // DirEntry reports a symlink as a symlink, never a dir
			continue
		}
		errs = append(errs, removeStaleTempsIn(filepath.Join(outDir, e.Name())))
	}
	return errors.Join(errs...)
}

func removeStaleTempsIn(vmDir string) error {
	root, err := openVMRoot(vmDir, false)
	if err != nil {
		return nil
	}
	defer func() { _ = root.Close() }()
	return errors.Join(sweepDomain(root, DomainDir), sweepDomain(root, windowsDomainPath))
}

// sweepDomain removes the temp files in domainDir inside root; a domainDir that
// is missing, not a real directory or escapes root is skipped.
func sweepDomain(root *os.Root, domainDir string) error {
	fi, err := root.Lstat(domainDir)
	if err != nil || !fi.IsDir() {
		return nil
	}
	// Non-blocking open and a Stat re-check: a FIFO swapped in after the
	// Lstat must not block startup.
	d, err := root.OpenFile(domainDir, os.O_RDONLY|oNonblock, 0)
	if err != nil {
		return nil
	}
	if fi, err := d.Stat(); err != nil || !fi.IsDir() {
		_ = d.Close()
		return nil
	}
	names, err := d.Readdirnames(-1)
	_ = d.Close()
	if err != nil {
		return err
	}
	var errs []error
	for _, n := range names {
		if !isTempName(n) {
			continue
		}
		if err := root.Remove(path.Join(domainDir, n)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func isTempName(n string) bool {
	for _, f := range treeFiles {
		if strings.HasPrefix(n, "."+f+".") {
			return true
		}
	}
	return false
}
