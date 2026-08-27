// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package collector

import (
	"bytes"
	"context"
	"op-ai-server-agent/internal/sample"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// nvidiaSMI is the CLI every collector and measurer in this file shells out
// to -- both the LookPath availability probes and the invocations themselves,
// so an environment that renames or wraps it has exactly one name to change.
const nvidiaSMI = "nvidia-smi"

// nvidiaCSVFormat is the --format every invocation uses: bare CSV rows, no
// header line and no unit suffixes. Every parse* function in this file
// assumes exactly that shape, so the flag and the parsers must move together.
const nvidiaCSVFormat = "--format=csv,noheader,nounits"

// nvidiaQueryFields is the ordered --query-gpu field list the collector requests
// from nvidia-smi. parseNvidiaCSV assumes exactly this column order.
const nvidiaQueryFields = "index,name,uuid,utilization.gpu,memory.used,memory.total,temperature.gpu,power.draw,fan.speed,driver_version"

// nvidiaCollector reports NVIDIA GPUs via the nvidia-smi CLI in CSV mode.
type nvidiaCollector struct{}

// NewNvidia returns an NVIDIA GPUCollector backed by nvidia-smi.
func NewNvidia() GPUCollector { return &nvidiaCollector{} }

// Name identifies this collector.
func (c *nvidiaCollector) Name() string { return "nvidia" }

// Available reports whether nvidia-smi is on PATH.
func (c *nvidiaCollector) Available() bool {
	_, err := exec.LookPath(nvidiaSMI)
	return err == nil
}

// Collect runs nvidia-smi and parses its CSV output into GPUs.
func (c *nvidiaCollector) Collect(ctx context.Context) ([]sample.GPU, error) {
	cmd := exec.CommandContext(ctx, nvidiaSMI,
		"--query-gpu="+nvidiaQueryFields,
		nvidiaCSVFormat,
	)
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	return parseNvidiaCSV(out)
}

// naFloat parses an nvidia-smi numeric field, mapping the various "not
// available" sentinels (and empty) to 0.
func naFloat(s string) float64 {
	switch s {
	case "", "[N/A]", "[Not Supported]", "[Unknown Error]":
		return 0
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return v
}

// naInt parses an nvidia-smi integer field with the same sentinel handling as
// naFloat (values like power may arrive as floats, so parse via float).
func naInt(s string) int {
	return int(naFloat(s))
}

// parseNvidiaCSV parses `nvidia-smi --query-gpu=... --format=csv,noheader,
// nounits` output into GPUs. Rows with fewer than 9 fields are skipped. Memory
// values are MiB and converted to bytes. VRAMTempC is not reported by this
// query and stays 0.
func parseNvidiaCSV(data []byte) ([]sample.GPU, error) {
	var gpus []sample.GPU
	for _, raw := range bytes.Split(data, []byte("\n")) {
		line := strings.TrimSpace(string(raw))
		if line == "" {
			continue
		}
		parts := strings.Split(line, ",")
		if len(parts) < 9 {
			continue
		}
		for i := range parts {
			parts[i] = strings.TrimSpace(parts[i])
		}
		gpu := sample.GPU{
			Index:         naInt(parts[0]),
			Name:          parts[1],
			UUID:          parts[2],
			UtilPct:       naFloat(parts[3]),
			MemUsedBytes:  int64(naInt(parts[4])) * 1024 * 1024,
			MemTotalBytes: int64(naInt(parts[5])) * 1024 * 1024,
			TempC:         naInt(parts[6]),
			PowerW:        naFloat(parts[7]),
			FanPct:        naFloat(parts[8]),
		}
		if len(parts) >= 10 {
			gpu.DriverVersion = parts[9]
		}
		gpus = append(gpus, gpu)
	}
	return gpus, nil
}

// nvidiaComputeAppsFields is the ordered --query-compute-apps field list
// (design doc §5): the per-process, per-GPU memory usage nvidia-smi can
// report because it knows every CUDA context's owning PID -- an exact
// measurement, not an estimate, for any PID the agent recognizes as one of
// its own managed children.
const nvidiaComputeAppsFields = "pid,gpu_uuid,used_memory"

// nvidiaGPUIndexFields is the minimal --query-gpu field list needed to map
// a GPU's UUID (compute-apps rows only carry the UUID) back to the index
// number spec.GPUs/GPUBudget use. Deliberately independent of
// nvidiaQueryFields above: the measurer runs on the runtime Manager's own
// serialized owner goroutine (buildSnapshot), not on this collector's
// regular telemetry cadence, so it cannot reuse a cached Collect() result
// from a different invocation -- it fetches its own tiny, cheap mapping
// every time it is called.
const nvidiaGPUIndexFields = "index,uuid"

// nvidiaMeasureTimeout bounds EACH nvidia-smi invocation the measurer
// makes. Two calls (index/uuid mapping, then compute-apps) run per
// measurement, so a worst case blocks for up to 2x this -- still small
// next to collectTimeout (2s, the same order of magnitude), and load-
// bearing: buildSnapshot runs on the Manager's single serialized owner
// goroutine (manager.go), so a wedged nvidia-smi here would stall every
// other admission decision, not just this one measurement. A package-level
// var (not a const), matching this module's other external-command timing
// knobs, so a test can shrink it if it ever needs to exercise the timeout
// path without actually waiting it out.
var nvidiaMeasureTimeout = 2 * time.Second

// NewNvidiaComputeApps returns a per-process, per-GPU VRAM usage measurer
// backed by `nvidia-smi --query-compute-apps`, for wiring into
// runtime.Manager.SetMeasurer. It returns nil when nvidia-smi is not on
// PATH: measurement is a HARDWARE capability, not a negotiated protocol
// feature (design doc §5) -- a host without nvidia-smi (no NVIDIA GPUs, AMD,
// or Apple unified memory, which has no per-process split to report at
// all) simply has no measurer installed, and the manager falls back to each
// spec's operator-entered VRAM estimate exactly as it already does today.
func NewNvidiaComputeApps() func(pids []int) map[int]map[int]int {
	if _, err := exec.LookPath(nvidiaSMI); err != nil {
		return nil
	}
	m := &nvidiaComputeAppsMeasurer{}
	return m.measure
}

// nvidiaComputeAppsMeasurer caches the GPU index/uuid mapping across calls
// (fix round 1, M1): `--query-gpu=index,uuid` describes the host's fixed
// GPU topology, which does not change between one measurement and the next
// under normal operation, so the original implementation's re-fetch on
// EVERY call doubled the worst-case subprocess-spawn cost on the manager's
// single serialized owner goroutine (which also serves Status() for the 1s
// telemetry tick and every EnsureRunning) for no benefit in the common
// case. The cache is invalidated (refetched) whenever a WANTED pid's
// reported GPU UUID is not found in it -- the one case a stale mapping
// actually matters (a GPU added/removed/reindexed since the last fetch,
// e.g. after a driver reset).
type nvidiaComputeAppsMeasurer struct {
	mu          sync.Mutex
	uuidToIndex map[string]int // nil until the first successful --query-gpu fetch
}

func (m *nvidiaComputeAppsMeasurer) cachedIndex() map[string]int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.uuidToIndex
}

func (m *nvidiaComputeAppsMeasurer) setCachedIndex(idx map[string]int) {
	m.mu.Lock()
	m.uuidToIndex = idx
	m.mu.Unlock()
}

// measure is the func(pids []int) map[int]map[int]int shape
// runtime.Manager.SetMeasurer expects. pids is the manager's own live
// managed-process PID set. nil pids, or the compute-apps invocation
// failing, returns nil: the caller (buildSnapshot) already treats a nil
// measurement map as "nothing measured this cycle", falling back to static
// estimates for every spec, exactly like having no measurer installed at
// all -- a transient nvidia-smi hiccup must never look like a hard
// VRAM-budget violation. One subprocess spawn in the steady state (the
// index/uuid mapping is cached, M1); two only on the first call, or after a
// cache-invalidating miss.
func (m *nvidiaComputeAppsMeasurer) measure(pids []int) map[int]map[int]int {
	if len(pids) == 0 {
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), nvidiaMeasureTimeout)
	defer cancel()

	appsOut, err := exec.CommandContext(ctx, nvidiaSMI,
		"--query-compute-apps="+nvidiaComputeAppsFields,
		nvidiaCSVFormat,
	).Output()
	if err != nil {
		return nil
	}
	rows := parseNvidiaComputeAppsCSV(appsOut)

	wanted := make(map[int]bool, len(pids))
	for _, p := range pids {
		wanted[p] = true
	}

	uuidToIndex := m.cachedIndex()
	if uuidToIndex == nil || hasUnresolvedUUID(rows, wanted, uuidToIndex) {
		// First call ever, or a wanted row's GPU UUID is not in the cache:
		// (re)fetch the mapping. Reuses the SAME ctx/deadline as the
		// compute-apps call above rather than a second independent
		// timeout, so two subprocess spawns in the worst case still cost
		// at most ONE nvidiaMeasureTimeout in total, not two.
		idxOut, err := exec.CommandContext(ctx, nvidiaSMI,
			"--query-gpu="+nvidiaGPUIndexFields,
			nvidiaCSVFormat,
		).Output()
		if err == nil {
			uuidToIndex = parseNvidiaGPUIndexCSV(idxOut)
			m.setCachedIndex(uuidToIndex)
		}
	}
	if uuidToIndex == nil {
		return nil // never obtained a mapping at all (first call AND that fetch failed)
	}

	return attributeComputeApps(rows, uuidToIndex, pids)
}

// hasUnresolvedUUID reports whether any WANTED row's GPU UUID is absent
// from uuidToIndex -- the cache-invalidation trigger for M1.
func hasUnresolvedUUID(rows []nvidiaComputeAppRow, wanted map[int]bool, uuidToIndex map[string]int) bool {
	for _, row := range rows {
		if !wanted[row.PID] {
			continue
		}
		if _, ok := uuidToIndex[row.GPUUUID]; !ok {
			return true
		}
	}
	return false
}

// attributeComputeApps is the pure attribution logic SetMeasurer's shape
// needs: filter rows to pids, resolve each row's GPU UUID to its index via
// uuidToIndex, and nest into map[pid]map[gpuIndex]usedMB. Extracted as a
// standalone pure function (fix round 1, I4) so the headline test calls the
// SAME code the production path (measure, above) runs, instead of
// re-implementing the filter/map loop inside the test body -- a bug here
// (wrong filter direction, a dropped GPU index, wrong map nesting) would
// otherwise leave that test green regardless.
func attributeComputeApps(rows []nvidiaComputeAppRow, uuidToIndex map[string]int, pids []int) map[int]map[int]int {
	wanted := make(map[int]bool, len(pids))
	for _, p := range pids {
		wanted[p] = true
	}

	var out map[int]map[int]int
	for _, row := range rows {
		if !wanted[row.PID] {
			continue // not one of the manager's own managed children
		}
		idx, ok := uuidToIndex[row.GPUUUID]
		if !ok {
			continue // a GPU nvidia-smi did not also report in --query-gpu; skip rather than guess
		}
		if out == nil {
			out = make(map[int]map[int]int)
		}
		if out[row.PID] == nil {
			out[row.PID] = make(map[int]int)
		}
		out[row.PID][idx] = row.UsedMemoryMB
	}
	return out
}

// parseNvidiaGPUIndexCSV parses `nvidia-smi --query-gpu=index,uuid
// --format=csv,noheader,nounits` output into a uuid->index map. A row with
// fewer than 2 fields is skipped, matching parseNvidiaCSV's own tolerance
// for a malformed line.
func parseNvidiaGPUIndexCSV(data []byte) map[string]int {
	out := make(map[string]int)
	for _, row := range splitNvidiaCSVRows(data) {
		if len(row) < 2 {
			continue
		}
		out[row[1]] = naInt(row[0])
	}
	return out
}

// nvidiaComputeAppRow is one row of `nvidia-smi --query-compute-apps`
// output: which process, on which GPU (by UUID -- compute-apps does not
// report a GPU index directly), using how much memory.
type nvidiaComputeAppRow struct {
	PID          int
	GPUUUID      string
	UsedMemoryMB int
}

// parseNvidiaComputeAppsCSV parses `nvidia-smi
// --query-compute-apps=pid,gpu_uuid,used_memory --format=csv,noheader,
// nounits` output. A row with fewer than 3 fields is skipped. A pure
// function (no I/O), unit-tested with canned CSV per the design doc's own
// requirement that the parser be independently verifiable of a real GPU.
func parseNvidiaComputeAppsCSV(data []byte) []nvidiaComputeAppRow {
	var out []nvidiaComputeAppRow
	for _, row := range splitNvidiaCSVRows(data) {
		if len(row) < 3 {
			continue
		}
		out = append(out, nvidiaComputeAppRow{
			PID:          naInt(row[0]),
			GPUUUID:      row[1],
			UsedMemoryMB: naInt(row[2]),
		})
	}
	return out
}

// splitNvidiaCSVRows splits raw nvidia-smi CSV output into trimmed fields
// per non-empty line, shared by parseNvidiaGPUIndexCSV and
// parseNvidiaComputeAppsCSV (the same line-splitting/trimming parseNvidiaCSV
// above already does inline for the --query-gpu telemetry shape).
func splitNvidiaCSVRows(data []byte) [][]string {
	var rows [][]string
	for _, raw := range bytes.Split(data, []byte("\n")) {
		line := strings.TrimSpace(string(raw))
		if line == "" {
			continue
		}
		parts := strings.Split(line, ",")
		for i := range parts {
			parts[i] = strings.TrimSpace(parts[i])
		}
		rows = append(rows, parts)
	}
	return rows
}
