// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package trust

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func pathsCollide(first, second string) bool {
	if first == "" || second == "" {
		return false
	}
	firstID := pathIdentity(first)
	secondID := pathIdentity(second)
	// Conservatively reject case-only aliases even on a case-sensitive host:
	// generated configs may be moved to Windows or a case-insensitive macOS
	// volume before either optional file exists for os.SameFile to compare.
	if strings.EqualFold(firstID, secondID) {
		return true
	}
	firstInfo, firstErr := os.Stat(first)
	secondInfo, secondErr := os.Stat(second)
	return firstErr == nil && secondErr == nil && os.SameFile(firstInfo, secondInfo)
}

func validateManagedCachePath(cachePath, operatorPath, certDirCAPath string) error {
	if pathsCollide(cachePath, operatorPath) {
		return fmt.Errorf("ca_cache_file must not refer to read-only ca_file")
	}
	if pathsCollide(cachePath, certDirCAPath) {
		return fmt.Errorf("ca_cache_file must not refer to read-only cert_dir/ca.pem")
	}
	return nil
}

func validateManagedCacheRoot(root *os.Root, filename, operatorPath, certDirCAPath string) error {
	collides, err := rootedPathCollides(root, filename, operatorPath)
	if err != nil {
		return fmt.Errorf("validate ca_cache_file against read-only ca_file: %w", err)
	}
	if collides {
		return fmt.Errorf("ca_cache_file must not refer to read-only ca_file")
	}
	collides, err = rootedPathCollides(root, filename, certDirCAPath)
	if err != nil {
		return fmt.Errorf("validate ca_cache_file against read-only cert_dir/ca.pem: %w", err)
	}
	if collides {
		return fmt.Errorf("ca_cache_file must not refer to read-only cert_dir/ca.pem")
	}
	return nil
}

func rootedPathCollides(root *os.Root, filename, readOnlyPath string) (bool, error) {
	if readOnlyPath == "" {
		return false, nil
	}
	readOnlyIdentity := pathIdentity(readOnlyPath)
	rootInfo, err := root.Stat(".")
	if err != nil {
		return false, err
	}
	readOnlyDirInfo, err := os.Stat(filepath.Dir(readOnlyIdentity))
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	if os.SameFile(rootInfo, readOnlyDirInfo) && strings.EqualFold(filename, filepath.Base(readOnlyIdentity)) {
		return true, nil
	}

	destinationInfo, err := root.Stat(filename)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	readOnlyInfo, err := os.Stat(readOnlyPath)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	return os.SameFile(destinationInfo, readOnlyInfo), nil
}

func pathIdentity(path string) string {
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	path = filepath.Clean(path)
	if resolved, err := resolveSymlinksAllowMissing(path); err == nil {
		return resolved
	}
	return path
}

// resolveSymlinksAllowMissing resolves every existing symlink component while
// retaining the unresolved suffix once a target stops existing. Unlike
// filepath.EvalSymlinks, this preserves the identity of a dangling final
// symlink and of relative symlink chains that point at a future cache file.
func resolveSymlinksAllowMissing(path string) (string, error) {
	const maxSymlinks = 255
	links := 0
	path = filepath.Clean(path)
	for {
		volume := filepath.VolumeName(path)
		root := volume + string(filepath.Separator)
		remainder := strings.TrimPrefix(path, root)
		parts := strings.Split(remainder, string(filepath.Separator))
		current := root
		restarted := false
		for i, part := range parts {
			if part == "" {
				continue
			}
			candidate := filepath.Join(current, part)
			info, err := os.Lstat(candidate)
			if err != nil {
				if os.IsNotExist(err) {
					return filepath.Join(append([]string{candidate}, parts[i+1:]...)...), nil
				}
				return "", err
			}
			if info.Mode()&os.ModeSymlink == 0 {
				current = candidate
				continue
			}
			links++
			if links > maxSymlinks {
				return "", fmt.Errorf("too many symlinks resolving %q", path)
			}
			target, err := os.Readlink(candidate)
			if err != nil {
				return "", err
			}
			if !filepath.IsAbs(target) {
				target = filepath.Join(filepath.Dir(candidate), target)
			}
			path = filepath.Join(append([]string{target}, parts[i+1:]...)...)
			path = filepath.Clean(path)
			restarted = true
			break
		}
		if !restarted {
			return filepath.Clean(current), nil
		}
	}
}
