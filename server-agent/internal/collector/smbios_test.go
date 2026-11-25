// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package collector

import (
	"encoding/binary"
	"op-ai-server-agent/internal/sample"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// t17opts describes a synthetic SMBIOS type-17 structure for the test encoder.
type t17opts struct {
	length     int // formatted-area length; 0 → 0x5C (3.x full structure)
	size       uint16
	extSize    uint32
	locator    string
	locIdx     byte // locator string index override; 0 → 1 when locator is set
	memType    byte
	speed      uint16
	confSpeed  uint16
	extSpeed   uint32
	extConfSpd uint32
}

// structure encodes a raw structure of the given type with a formatted area of
// length bytes (header included) and the given strings.
func structure(typ byte, length int, strs ...string) []byte {
	b := make([]byte, length)
	b[0], b[1] = typ, byte(length)
	for _, s := range strs {
		b = append(b, s...)
		b = append(b, 0)
	}
	if len(strs) == 0 {
		b = append(b, 0)
	}
	return append(b, 0)
}

func type17(o t17opts) []byte {
	n := o.length
	if n == 0 {
		n = 0x5C
	}
	b := structure(17, n, o.locator)
	put16 := func(off int, v uint16) {
		if off+2 <= n {
			binary.LittleEndian.PutUint16(b[off:], v)
		}
	}
	put32 := func(off int, v uint32) {
		if off+4 <= n {
			binary.LittleEndian.PutUint32(b[off:], v)
		}
	}
	put16(0x0C, o.size)
	if o.locator != "" {
		b[0x10] = 1
	}
	if o.locIdx != 0 {
		b[0x10] = o.locIdx
	}
	if 0x12 < n {
		b[0x12] = o.memType
	}
	put16(0x15, o.speed)
	put16(0x20, o.confSpeed)
	put32(0x1C, o.extSize)
	put32(0x54, o.extSpeed)
	put32(0x58, o.extConfSpd)
	return b
}

func endOfTable() []byte { return structure(127, 4) }

func join(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

const smbMiB = int64(1) << 20

var smbiosCases = []struct {
	name  string
	table []byte
	want  []sample.MemoryModule
}{
	{
		name: "dmidecode fixture: DDR4 3200 plus empty slot",
		table: join(
			type17(t17opts{size: 16384, locator: "DIMM 0", memType: 0x1A, speed: 3200}),
			type17(t17opts{size: 0, locator: "DIMM 1", memType: 0x02}),
			endOfTable()),
		want: []sample.MemoryModule{{Locator: "DIMM 0", SizeBytes: 16384 * smbMiB, Type: "DDR4", SpeedMHz: 3200}},
	},
	{
		name:  "DDR5 rated speed wins over configured",
		table: join(type17(t17opts{size: 0x7FFF, extSize: 32768, locator: "A1", memType: 0x22, speed: 4800, confSpeed: 5600}), endOfTable()),
		want:  []sample.MemoryModule{{Locator: "A1", SizeBytes: 32768 * smbMiB, Type: "DDR5", SpeedMHz: 4800}},
	},
	{
		name:  "configured speed used when rated is zero",
		table: join(type17(t17opts{size: 8192, locator: "A1", memType: 0x1A, confSpeed: 2666}), endOfTable()),
		want:  []sample.MemoryModule{{Locator: "A1", SizeBytes: 8192 * smbMiB, Type: "DDR4", SpeedMHz: 2666}},
	},
	{
		name: "extended size and extended speed",
		table: join(type17(t17opts{
			size: 0x7FFF, extSize: 131072, locator: "P0", memType: 0x22,
			speed: 0xFFFF, extSpeed: 6400,
		}), endOfTable()),
		want: []sample.MemoryModule{{Locator: "P0", SizeBytes: 131072 * smbMiB, Type: "DDR5", SpeedMHz: 6400}},
	},
	{
		name:  "extended configured speed",
		table: join(type17(t17opts{size: 4096, locator: "P0", memType: 0x22, confSpeed: 0xFFFF, extConfSpd: 7200}), endOfTable()),
		want:  []sample.MemoryModule{{Locator: "P0", SizeBytes: 4096 * smbMiB, Type: "DDR5", SpeedMHz: 7200}},
	},
	{
		name:  "KiB granularity",
		table: join(type17(t17opts{size: 0x8000 | 512, locator: "S0", memType: 0x03}), endOfTable()),
		want:  []sample.MemoryModule{{Locator: "S0", SizeBytes: 512 * 1024, Type: "DRAM"}},
	},
	{
		name:  "unknown size skipped",
		table: join(type17(t17opts{size: 0xFFFF, locator: "S0", memType: 0x1A}), endOfTable()),
		want:  nil,
	},
	{
		name:  "2.6 structure without speed fields",
		table: join(type17(t17opts{length: 27, size: 2048, locator: "J1", memType: 0x13}), endOfTable()),
		want:  []sample.MemoryModule{{Locator: "J1", SizeBytes: 2048 * smbMiB, Type: "DDR2"}},
	},
	{
		name:  "Other type kept, Unknown type dropped",
		table: join(type17(t17opts{size: 1024, locator: "X", memType: 0x01}), type17(t17opts{size: 1024, locator: "Y", memType: 0x02}), endOfTable()),
		want: []sample.MemoryModule{
			{Locator: "X", SizeBytes: 1024 * smbMiB, Type: "Other"},
			{SizeBytes: 1024 * smbMiB, Locator: "Y"},
		},
	},
	{
		name: "interleaved non-17 structures",
		table: join(
			structure(0, 0x18, "Vendor", "1.0", "01/01/2026"),
			structure(16, 0x17),
			type17(t17opts{size: 16384, locator: "DIMM 0", memType: 0x1A, speed: 3200}),
			structure(4, 0x30, "CPU"),
			endOfTable()),
		want: []sample.MemoryModule{{Locator: "DIMM 0", SizeBytes: 16384 * smbMiB, Type: "DDR4", SpeedMHz: 3200}},
	},
	{
		name:  "no end-of-table marker",
		table: type17(t17opts{size: 1024, locator: "Z", memType: 0x1A, speed: 2133}),
		want:  []sample.MemoryModule{{Locator: "Z", SizeBytes: 1024 * smbMiB, Type: "DDR4", SpeedMHz: 2133}},
	},
	{
		name:  "firmware padding trimmed from the locator",
		table: join(type17(t17opts{size: 8192, locator: "DIMM 0  ", memType: 0x1A, speed: 3200}), endOfTable()),
		want:  []sample.MemoryModule{{Locator: "DIMM 0", SizeBytes: 8192 * smbMiB, Type: "DDR4", SpeedMHz: 3200}},
	},
	{
		// The rated speed word ends at 0x17; the string set right after a shorter
		// formatted area must not be read as a speed.
		name:  "structure shorter than 0x17 has no rated speed",
		table: join(type17(t17opts{length: 0x15, size: 2048, locator: "DIMM 0", memType: 0x18}), endOfTable()),
		want:  []sample.MemoryModule{{Locator: "DIMM 0", SizeBytes: 2048 * smbMiB, Type: "DDR3"}},
	},
	{
		// 0x7FFF points to the extended size dword at 0x1C, absent below 0x20.
		name:  "0x7FFF size without the extended size field skipped",
		table: join(type17(t17opts{length: 0x1C, size: 0x7FFF, locator: "DIMM 0", memType: 0x1A}), endOfTable()),
		want:  nil,
	},
	{
		name:  "out-of-range locator string index yields an empty locator",
		table: join(type17(t17opts{size: 1024, locator: "A1", locIdx: 5, memType: 0x1A}), endOfTable()),
		want:  []sample.MemoryModule{{SizeBytes: 1024 * smbMiB, Type: "DDR4"}},
	},
	{
		name:  "stops at end-of-table",
		table: join(endOfTable(), type17(t17opts{size: 1024, locator: "Z", memType: 0x1A})),
		want:  nil,
	},
}

func TestParseSMBIOSMemory(t *testing.T) {
	for _, tc := range smbiosCases {
		t.Run(tc.name, func(t *testing.T) {
			got := ParseSMBIOSMemory(tc.table)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %#v, want %#v", got, tc.want)
			}
		})
	}
}

func TestParseSMBIOSTruncatedStringSet(t *testing.T) {
	good := type17(t17opts{size: 16384, locator: "DIMM 0", memType: 0x1A, speed: 3200})
	bad := type17(t17opts{size: 8192, locator: "DIMM 1", memType: 0x1A, speed: 3200})
	bad = bad[:len(bad)-2] // drop the terminating NULs
	got := ParseSMBIOSMemory(join(good, bad))
	want := []sample.MemoryModule{{Locator: "DIMM 0", SizeBytes: 16384 * smbMiB, Type: "DDR4", SpeedMHz: 3200}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
	// Truncated header, length beyond the table, length shorter than the
	// header: nothing decoded, no panic.
	for _, in := range [][]byte{{17, 0x5C}, {17, 0x5C, 0, 0, 0, 0}, {17, 2, 0, 0, 0, 0}} {
		if got := ParseSMBIOSMemory(in); got != nil {
			t.Fatalf("ParseSMBIOSMemory(%v) = %#v, want nil", in, got)
		}
	}
}

func TestParseSMBIOSNoSensitiveFields(t *testing.T) {
	var names []string
	typ := reflect.TypeOf(sample.MemoryModule{})
	for i := range typ.NumField() {
		names = append(names, typ.Field(i).Name)
	}
	want := []string{"Locator", "SizeBytes", "Type", "SpeedMHz"}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("MemoryModule fields = %v, want exactly %v (serial/part number must never be exported)", names, want)
	}
}

func FuzzParseSMBIOSMemory(f *testing.F) {
	for _, tc := range smbiosCases {
		f.Add(tc.table)
	}
	f.Fuzz(func(_ *testing.T, table []byte) { _ = ParseSMBIOSMemory(table) })
}

func TestReadSMBIOSMemoryModules(t *testing.T) {
	p := filepath.Join(t.TempDir(), "DMI")
	table := join(type17(t17opts{size: 16384, locator: "DIMM 0", memType: 0x1A, speed: 3200}), endOfTable())
	if err := os.WriteFile(p, table, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := ReadSMBIOSMemoryModules(p)
	if err != nil {
		t.Fatal(err)
	}
	want := []sample.MemoryModule{{Locator: "DIMM 0", SizeBytes: 16384 * smbMiB, Type: "DDR4", SpeedMHz: 3200}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
	if _, err := ReadSMBIOSMemoryModules(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("missing file must error")
	}
}
