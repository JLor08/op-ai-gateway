// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package collector

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
)

// lhmNode is one node of a LibreHardwareMonitor / OpenHardwareMonitor /data.json
// sensor tree. The web server emits a recursive tree; sensor leaves carry a Type
// (e.g. "Power", "Temperature") and a Value string like "65.0 W". Only the fields
// the collector needs are decoded; unknown keys are ignored.
type lhmNode struct {
	Text     string    `json:"Text"`
	Type     string    `json:"Type"`
	Value    string    `json:"Value"`
	SensorID string    `json:"SensorId"`
	Children []lhmNode `json:"Children"`
}

// lhmPowerCollector reads a LibreHardwareMonitor Remote Web Server /data.json and
// extracts CPU package watts (and, best-effort, a system/board rail). OS-agnostic;
// the only Windows CPU-watt path and a Linux fallback when RAPL is unreadable.
// CGO-free (net/http + encoding/json); ships/links nothing from LHM — the operator
// installs and runs it. The GET + JSON decode is delegated to an lhmSource, which
// may be shared with an lhmTempCollector (see DetectPowerAndTempCollectors) so the
// two sub-collectors' back-to-back Collect calls issue a single fetch per cycle.
type lhmPowerCollector struct {
	source *lhmSource
}

// newLHMPowerCollector builds an LHM-HTTP power collector for url with its own,
// unshared lhmSource. A nil client defaults to http.DefaultClient. An empty url
// yields an unavailable collector.
func newLHMPowerCollector(url string, client *http.Client) *lhmPowerCollector {
	return newLHMPowerCollectorFromSource(newLHMSource(url, client))
}

// newLHMPowerCollectorFromSource builds an LHM-HTTP power collector that reads
// through the given source instead of fetching on its own — used to share one
// source (and so one /data.json fetch per cycle) with an lhmTempCollector.
func newLHMPowerCollectorFromSource(source *lhmSource) *lhmPowerCollector {
	return &lhmPowerCollector{source: source}
}

func (c *lhmPowerCollector) Name() string { return "lhm" }

// Available reports whether an LHM URL is configured.
func (c *lhmPowerCollector) Available() bool { return c.source.Available() }

// Collect fetches the /data.json tree (via the shared source, which memoizes
// the actual HTTP GET) and returns CPU package watts + a best-effort
// system/board watts. Any HTTP/JSON/parse error or missing sensor yields nil for
// that metric (best-effort). Never returns an error.
func (c *lhmPowerCollector) Collect(ctx context.Context) (*float64, *float64, error) {
	root, err := c.source.getTree(ctx)
	if err != nil {
		slog.Debug("lhm power query failed", "url", c.source.url, "err", err)
		return nil, nil, nil
	}
	cpu, system, powerNames := findLHMPower(root)
	slog.Debug("lhm power query ok", "url", c.source.url, "cpu_w", wattsLog(cpu), "system_w", wattsLog(system))
	// No CPU-package match: log the "Power" sensor names LHM exposed so a
	// naming mismatch (e.g. a vendor spelling this collector doesn't recognize)
	// is immediately visible instead of a silent cpu_w=none.
	if cpu == nil {
		slog.Debug("lhm power query: no CPU-package power sensor matched", "url", c.source.url, "power_sensors", powerNames)
	}
	return cpu, system, nil
}

// fetchLHMTree GETs the LibreHardwareMonitor /data.json endpoint at url and
// decodes the sensor tree. Shared by the power and temperature sub-collectors
// (lhm_power.go / lhm_temp.go); each caller logs its own context-appropriate
// Debug line on failure and degrades to a nil reading (best-effort) — this
// helper only fetches + parses and returns the error for that.
func fetchLHMTree(ctx context.Context, url string, client *http.Client) (*lhmNode, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("request build failed: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("non-2xx response: %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("body read failed: %w", err)
	}
	var root lhmNode
	if err := json.Unmarshal(body, &root); err != nil {
		return nil, fmt.Errorf("json parse failed: %w", err)
	}
	return &root, nil
}

// wattsLog renders a nullable watt pointer for a log attribute: the value, or
// "none" when the metric was not found (a *float64 would otherwise log as a pointer).
func wattsLog(v *float64) any {
	if v == nil {
		return "none"
	}
	return *v
}

// findLHMPower walks the sensor tree and returns the CPU package power (CPU watts),
// a best-effort whole-board/total rail (system watts), and the names of every "Power"
// sensor seen (for a no-match debug log). Case-insensitive; a "65.0 W" / "61,7 W"
// value string is parsed to float. cpu/system may be nil.
//
// CPU package match: a "Power" sensor whose name contains "cpu package" (the Intel
// spelling), OR is exactly "package" AND sits under a CPU SensorId (/amdcpu, /intelcpu,
// /cpu — AMD names it just "Package"). This excludes the GPU "GPU Package" leaf (its
// name is not "package" and its SensorId is /gpu-*) and the per-core "Core #N (SMU)"
// leaves.
//
// The system-rail match excludes any name containing "agent": Intel CPUs report a
// "System Agent" Power sensor (the uncore/IMC/PCIe domain, a CPU PACKAGE sub-rail —
// not a whole-board total) whose name would otherwise satisfy a naive
// Contains(name, "system") test and get mislabeled as total system watts.
func findLHMPower(root *lhmNode) (cpu *float64, system *float64, powerNames []string) {
	var walk func(n *lhmNode)
	walk = func(n *lhmNode) {
		if strings.EqualFold(n.Type, "Power") {
			name := strings.ToLower(strings.TrimSpace(n.Text))
			powerNames = append(powerNames, n.Text)
			isCPUPackage := strings.Contains(name, "cpu package") ||
				(name == "package" && sensorIDIsCPU(n.SensorID))
			if cpu == nil && isCPUPackage {
				cpu = parseLHMWatts(n.Value)
			}
			isSystemRail := (strings.Contains(name, "system") || strings.Contains(name, "mainboard") || strings.Contains(name, "board total")) && !strings.Contains(name, "agent")
			if system == nil && isSystemRail {
				system = parseLHMWatts(n.Value)
			}
		}
		for i := range n.Children {
			walk(&n.Children[i])
		}
	}
	walk(root)
	return cpu, system, powerNames
}

// sensorIDIsCPU reports whether an LHM SensorId path belongs to a CPU hardware node
// (AMD or Intel), used to disambiguate an AMD "Package" power leaf from a GPU's
// "GPU Package". LHM ids look like "/amdcpu/0/power/0" or "/intelcpu/0/power/0".
func sensorIDIsCPU(sensorID string) bool {
	id := strings.ToLower(sensorID)
	return strings.Contains(id, "/amdcpu/") || strings.Contains(id, "/intelcpu/") || strings.Contains(id, "/cpu/")
}

// parseLHMWatts parses an LHM sensor value string like "65.0 W" (or "65,0 W" on a
// comma-decimal locale) into watts, returning nil when it cannot be parsed.
func parseLHMWatts(s string) *float64 {
	fields := strings.Fields(strings.TrimSpace(s))
	if len(fields) == 0 {
		return nil
	}
	v, err := strconv.ParseFloat(strings.Replace(fields[0], ",", ".", 1), 64)
	if err != nil {
		return nil
	}
	w := v
	return &w
}
