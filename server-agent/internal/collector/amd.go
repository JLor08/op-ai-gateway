// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package collector

import (
	"context"
	"encoding/json"
	"op-ai-server-agent/internal/sample"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
)

// rocmArgs is the argument list the collector passes to rocm-smi to obtain a
// JSON object keyed by card ("card0", "card1", …). parseRocmJSON reads the
// per-card fields these flags emit. --showmemuse is deliberately absent:
// rocm-smi 4.x reports only a percentage there, while --showmeminfo vram
// carries the VRAM byte keys in every version.
var rocmArgs = []string{
	"--showid",
	"--showproductname",
	"--showuse",
	"--showmeminfo", "vram",
	"--showtemp",
	"--showpower",
	"--showdriverversion",
	"--showbus",
	"--showuniqueid",
	"--json",
}

// amdCollector reports AMD GPUs via the rocm-smi CLI in JSON mode.
type amdCollector struct {
	// kfdRoot overrides the KFD sysfs directory in tests; zero means kfdRoot.
	kfdRoot string
}

// NewAMD returns an AMD GPUCollector backed by rocm-smi.
func NewAMD() GPUCollector { return &amdCollector{} }

// Name identifies this collector.
func (c *amdCollector) Name() string { return "amd" }

// Available reports whether rocm-smi is on PATH.
func (c *amdCollector) Available() bool {
	_, err := exec.LookPath("rocm-smi")
	return err == nil
}

// Collect runs rocm-smi and parses its JSON output into GPUs.
func (c *amdCollector) Collect(ctx context.Context) ([]sample.GPU, error) {
	cmd := exec.CommandContext(ctx, "rocm-smi", rocmArgs...)
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	gpus, err := parseRocmJSON(out)
	if err != nil {
		return nil, err
	}
	root := c.kfdRoot
	if root == "" {
		root = kfdRoot
	}
	ranks, err := kfdPCIRanks(root)
	if err != nil {
		ranks = nil
	}
	return joinAMDIndex(gpus, ranks), nil
}

// amdIndex holds the PCI→rank map of the last complete join; nil means no
// usable join (read by the fdinfo measurer through currentAMDIndex).
var amdIndex atomic.Pointer[map[pciAddr]int]

// applyKFDRanks numbers the GPUs by KFD rank, joined on the PCI address. It is
// all-or-nothing: if any GPU's PCIBusID is missing, unparseable or absent from
// ranks, the input is returned unchanged with false (a partial renumbering
// could collide with the remaining card indices).
func applyKFDRanks(gpus []sample.GPU, ranks map[pciAddr]int) ([]sample.GPU, bool) {
	idx := make([]int, len(gpus))
	for i, g := range gpus {
		addr, ok := parsePCIAddr(g.PCIBusID)
		if !ok {
			return gpus, false
		}
		rank, ok := ranks[addr]
		if !ok {
			return gpus, false
		}
		idx[i] = rank
	}
	out := make([]sample.GPU, len(gpus))
	copy(out, gpus)
	for i := range out {
		out[i].Index = idx[i]
	}
	return out, true
}

// joinAMDIndex applies the KFD ranks and publishes them when the join was
// complete, or unpublishes them otherwise (including a nil ranks map from an
// unreadable topology).
func joinAMDIndex(gpus []sample.GPU, ranks map[pciAddr]int) []sample.GPU {
	if ranks == nil {
		amdIndex.Store(nil)
		return gpus
	}
	out, ok := applyKFDRanks(gpus, ranks)
	if ok {
		amdIndex.Store(&ranks)
	} else {
		amdIndex.Store(nil)
	}
	return out
}

// currentAMDIndex returns the published PCI→rank map, or nil when the last
// collection had no complete join.
func currentAMDIndex() map[pciAddr]int {
	if p := amdIndex.Load(); p != nil {
		return *p
	}
	return nil
}

// parseRocmJSON parses `rocm-smi … --json` output into GPUs. The object is keyed
// by card ("card0", …) plus optional non-card entries (e.g. "system"); only
// keys with the "card" prefix are treated as GPUs, iterated in sorted order.
// Each field is read defensively: a missing key defaults to its zero value and
// never fails the parse. Memory values are already bytes; temperature is a
// float truncated to whole degrees.
//
// rocm-smi pre-4.0 (3.x) and 4.x name the product keys differently, so both
// are accepted. The name is the first usable of "Device Name" (4.x marketing
// name), "Card Series"/"Card series", then "Card Model"/"Card model" (a bare
// PCI device id on 4.x, hence last). "Unique ID" feeds UUID and "PCI Bus" feeds
// PCIBusID; 3.x emits neither, which leaves them empty. Placeholder values
// such as "N/A" count as absent so they never reach the portal as data.
func parseRocmJSON(data []byte) ([]sample.GPU, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}

	keys := make([]string, 0, len(raw))
	for k := range raw {
		if strings.HasPrefix(k, "card") {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)

	var gpus []sample.GPU
	for _, k := range keys {
		var fields map[string]string
		if err := json.Unmarshal(raw[k], &fields); err != nil {
			// Not a per-card object (e.g. a nested structure) — skip it
			// rather than fail the whole parse.
			continue
		}
		gpus = append(gpus, sample.GPU{
			Vendor: VendorAMD,
			Index:  cardIndex(k),
			Name: firstNonEmpty(
				rocmString(fields["Device Name"]),
				rocmString(fields["Card Series"]),
				rocmString(fields["Card series"]),
				rocmString(fields["Card Model"]),
				rocmString(fields["Card model"]),
			),
			UUID:          rocmString(fields["Unique ID"]),
			PCIBusID:      rocmString(fields["PCI Bus"]),
			UtilPct:       rocmFloat(fields["GPU use (%)"]),
			MemUsedBytes:  rocmInt64(fields["VRAM Total Used Memory (B)"]),
			MemTotalBytes: rocmInt64(fields["VRAM Total Memory (B)"]),
			TempC:         int(rocmFloat(fields["Temperature (Sensor edge) (C)"])),
			PowerW:        rocmFloat(fields["Average Graphics Package Power (W)"]),
		})
	}

	if sysRaw, ok := raw["system"]; ok {
		var sys map[string]string
		if json.Unmarshal(sysRaw, &sys) == nil {
			driver := firstNonEmpty(rocmString(sys["Driver version"]), rocmString(sys["Driver Version"]))
			for i := range gpus {
				gpus[i].DriverVersion = driver
			}
		}
	}

	return gpus, nil
}

// cardIndex extracts the numeric suffix of a card key ("card3" → 3); a
// non-numeric suffix yields 0.
func cardIndex(key string) int {
	suffix := strings.TrimPrefix(key, "card")
	n, err := strconv.Atoi(suffix)
	if err != nil {
		return 0
	}
	return n
}

// firstNonEmpty returns the first non-empty string, or "" if all are empty.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// rocmString trims a rocm-smi string value and maps the placeholders it prints
// for unsupported fields ("N/A", "unknown", …) to "".
func rocmString(s string) string {
	s = strings.TrimSpace(s)
	switch strings.ToLower(s) {
	case "", "n/a", "na", "unknown":
		return ""
	}
	return s
}

// rocmFloat parses a rocm-smi numeric string, returning 0 for an empty or
// unparseable value.
func rocmFloat(s string) float64 {
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0
	}
	return v
}

// rocmInt64 parses a rocm-smi integer string (byte counts), returning 0 for an
// empty or unparseable value.
func rocmInt64(s string) int64 {
	v, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return 0
	}
	return v
}
