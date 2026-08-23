// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package trust

import (
	cryptorand "crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

var (
	atomicWriteFile       = writeFileAtomically
	selectedRootValidator = validateSelectedRoot
)

type cachePathValidator func(root *os.Root, filename string) error

func writeFileAtomically(path string, data []byte, validate cachePathValidator) (returnErr error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	filename := filepath.Base(path)
	if err := selectedRootValidator(root, dir); err != nil {
		return err
	}
	if validate != nil {
		if err := validate(root, filename); err != nil {
			return err
		}
	}
	tmp, tmpName, err := createTempFile(root)
	if err != nil {
		return err
	}
	defer func() {
		if returnErr != nil {
			_ = root.Remove(tmpName)
		}
	}()
	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if validate != nil {
		if err := validate(root, filename); err != nil {
			return err
		}
	}
	if err := selectedRootValidator(root, dir); err != nil {
		return err
	}
	backupName, hadPrevious, err := backupRootFile(root, filename)
	if err != nil {
		return err
	}
	cleanupBackup := backupName != ""
	if backupName != "" {
		defer func() {
			if cleanupBackup {
				_ = root.Remove(backupName)
			}
		}()
	}
	if err := root.Rename(tmpName, filename); err != nil {
		return err
	}
	rollback := func(commitErr error) error {
		if rollbackErr := rollbackRootFile(root, filename, backupName, hadPrevious); rollbackErr != nil {
			cleanupBackup = false
			return fmt.Errorf("%w (rollback managed CA cache: %v; recovery backup: %s)", commitErr, rollbackErr, backupName)
		}
		return commitErr
	}
	if validate != nil {
		if err := validate(root, filename); err != nil {
			return rollback(err)
		}
	}
	if err := selectedRootValidator(root, dir); err != nil {
		return rollback(err)
	}
	return nil
}

func validateSelectedRoot(root *os.Root, selectedPath string) error {
	rootInfo, err := root.Stat(".")
	if err != nil {
		return err
	}
	selectedInfo, err := os.Stat(selectedPath)
	if err != nil {
		return err
	}
	if !os.SameFile(rootInfo, selectedInfo) {
		return fmt.Errorf("ca_cache_file directory changed during atomic install")
	}
	return nil
}

func createTempFile(root *os.Root) (*os.File, string, error) {
	return createUniqueRootFile(root, ".server-agent-ca-", ".tmp")
}

func createUniqueRootFile(root *os.Root, prefix, suffix string) (*os.File, string, error) {
	for range 100 {
		var random [8]byte
		if _, err := cryptorand.Read(random[:]); err != nil {
			return nil, "", err
		}
		name := prefix + hex.EncodeToString(random[:]) + suffix
		file, err := root.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			return file, name, nil
		}
		if !os.IsExist(err) {
			return nil, "", err
		}
	}
	return nil, "", fmt.Errorf("create unique managed CA temp file")
}

func backupRootFile(root *os.Root, filename string) (backupName string, exists bool, returnErr error) {
	previous, err := root.Open(filename)
	if err != nil {
		if os.IsNotExist(err) {
			return "", false, nil
		}
		return "", false, err
	}
	defer previous.Close()
	info, err := previous.Stat()
	if err != nil {
		return "", false, err
	}
	if !info.Mode().IsRegular() {
		return "", false, fmt.Errorf("managed CA cache is not a regular file")
	}

	backup, backupName, err := createUniqueRootFile(root, ".server-agent-ca-backup-", ".pem")
	if err != nil {
		return "", false, err
	}
	defer func() {
		if returnErr != nil {
			_ = backup.Close()
			_ = root.Remove(backupName)
		}
	}()
	if err := backup.Chmod(info.Mode().Perm()); err != nil {
		return "", false, err
	}
	if _, err := io.Copy(backup, previous); err != nil {
		return "", false, err
	}
	if err := backup.Close(); err != nil {
		return "", false, err
	}
	return backupName, true, nil
}

func rollbackRootFile(root *os.Root, filename, backupName string, hadPrevious bool) error {
	if hadPrevious {
		return root.Rename(backupName, filename)
	}
	if err := root.Remove(filename); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
