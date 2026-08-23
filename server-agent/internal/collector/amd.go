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
)

// rocmArgs is the argument list the collector passes to rocm-smi to obtain a
// JSON object keyed by card ("card0", "card1", …). parseRocmJSON reads the
// per-card fields these flags emit.
var rocmArgs = []string{
	"--showid",
	"--showproductname",
	"--showuse",
	"--showmemuse",
	"--showtemp",
	"--showpower",
	"--showdriverversion",
	"--json",
}

// amdCollector reports AMD GPUs via the rocm-smi CLI in JSON mode.
type amdCollector struct{}

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
	return parseRocmJSON(out)
}

// parseRocmJSON parses `rocm-smi … --json` output into GPUs. The object is keyed
// by card ("card0", …) plus optional non-card entries (e.g. "system"); only
// keys with the "card" prefix are treated as GPUs, iterated in sorted order.
// Each field is read defensively: a missing key defaults to its zero value and
// never fails the parse. Memory values are already bytes; temperature is a
// float truncated to whole degrees.
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
			Index:         cardIndex(k),
			Name:          firstNonEmpty(fields["Card series"], fields["Card model"]),
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
			driver := firstNonEmpty(sys["Driver version"], sys["Driver Version"])
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
