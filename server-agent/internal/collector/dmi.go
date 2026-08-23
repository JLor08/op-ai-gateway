// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package collector

import (
	"op-ai-server-agent/internal/sample"
	"path/filepath"
	"strings"
)

// dmiReader reads mainboard/BIOS identity from a Linux SMBIOS/DMI sysfs directory
// (default /sys/class/dmi/id). Every field is a 0444 world-readable file, so it
// needs no root. It DELIBERATELY reads only non-identifying fields — never
// *_serial or product_uuid (privacy D4). The root is injectable so the reader is
// unit-testable on any OS (mirrors the RAPL collector's sysfs-root injection).
type dmiReader struct{ root string }

func newDMIReader(root string) *dmiReader { return &dmiReader{root: root} }

// field reads one DMI attribute file, trimmed; a missing file yields "".
func (d *dmiReader) field(name string) string {
	return strings.TrimSpace(readSysfsString(filepath.Join(d.root, name)))
}

// read returns the mainboard + BIOS identity. Mainboard prefers the baseboard
// (board_*) fields, falling back to the system (sys_vendor/product_*) fields.
func (d *dmiReader) read() (sample.Mainboard, sample.BIOS) {
	board := sample.Mainboard{
		Vendor:  firstNonEmpty(d.field("board_vendor"), d.field("sys_vendor")),
		Product: firstNonEmpty(d.field("board_name"), d.field("product_name")),
		Version: firstNonEmpty(d.field("board_version"), d.field("product_version")),
	}
	bios := sample.BIOS{Vendor: d.field("bios_vendor"), Version: d.field("bios_version")}
	return board, bios
}
