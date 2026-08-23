// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package collector

import (
	"os"
	"path/filepath"
	"testing"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDMIReaderReadsBoardAndBIOS(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "board_vendor"), "ASUSTeK COMPUTER INC.\n")
	writeFile(t, filepath.Join(root, "board_name"), "ROG STRIX X670E\n")
	writeFile(t, filepath.Join(root, "board_version"), "Rev 1.xx\n")
	writeFile(t, filepath.Join(root, "bios_vendor"), "American Megatrends\n")
	writeFile(t, filepath.Join(root, "bios_version"), "2801\n")

	board, bios := newDMIReader(root).read()
	if board.Vendor != "ASUSTeK COMPUTER INC." || board.Product != "ROG STRIX X670E" || board.Version != "Rev 1.xx" {
		t.Fatalf("board = %#v", board)
	}
	if bios.Vendor != "American Megatrends" || bios.Version != "2801" {
		t.Fatalf("bios = %#v", bios)
	}
}

func TestDMIReaderFallsBackToSystemFields(t *testing.T) {
	root := t.TempDir()
	// No board_* files; only sys_vendor / product_name / product_version present.
	writeFile(t, filepath.Join(root, "sys_vendor"), "Dell Inc.\n")
	writeFile(t, filepath.Join(root, "product_name"), "PowerEdge R760\n")
	writeFile(t, filepath.Join(root, "product_version"), "01\n")

	board, _ := newDMIReader(root).read()
	if board.Vendor != "Dell Inc." || board.Product != "PowerEdge R760" || board.Version != "01" {
		t.Fatalf("board = %#v", board)
	}
}

func TestDMIReaderNeverReadsSerials(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "board_serial"), "SECRET-SERIAL\n")
	writeFile(t, filepath.Join(root, "product_uuid"), "SECRET-UUID\n")
	writeFile(t, filepath.Join(root, "board_name"), "B\n")

	board, bios := newDMIReader(root).read()
	// The reader has no field that could carry the serial/uuid.
	for _, v := range []string{board.Vendor, board.Product, board.Version, bios.Vendor, bios.Version} {
		if v == "SECRET-SERIAL" || v == "SECRET-UUID" {
			t.Fatalf("reader leaked a serial/uuid: %q", v)
		}
	}
}
