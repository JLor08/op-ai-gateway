// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package collector

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
)

// lhmTempCollector reads a LibreHardwareMonitor Remote Web Server /data.json
// and extracts the CPU package temperature. This is the Windows CPU-temperature
// source (the native collector, temp_other.go, returns nil on non-Linux); a
// Linux fallback too when hwmon is unreadable. CGO-free (net/http +
// encoding/json); ships/links nothing from LHM — the operator installs and
// runs it. Mirrors lhmPowerCollector, including delegating its GET + JSON
// decode to an lhmSource that may be shared with an lhmPowerCollector (see
// DetectPowerAndTempCollectors) so the two sub-collectors' back-to-back
// Collect calls issue a single fetch per cycle.
type lhmTempCollector struct {
	source *lhmSource
}

// newLHMTempCollector builds an LHM-HTTP temperature collector for url with
// its own, unshared lhmSource. A nil client defaults to http.DefaultClient. An
// empty url yields an unavailable collector.
func newLHMTempCollector(url string, client *http.Client) *lhmTempCollector {
	return newLHMTempCollectorFromSource(newLHMSource(url, client))
}

// newLHMTempCollectorFromSource builds an LHM-HTTP temperature collector that
// reads through the given source instead of fetching on its own — used to
// share one source (and so one /data.json fetch per cycle) with an
// lhmPowerCollector.
func newLHMTempCollectorFromSource(source *lhmSource) *lhmTempCollector {
	return &lhmTempCollector{source: source}
}

func (c *lhmTempCollector) Name() string { return "lhm" }

// Available reports whether an LHM URL is configured.
func (c *lhmTempCollector) Available() bool { return c.source.Available() }

// Collect fetches the /data.json tree (via the shared source, which memoizes
// the actual HTTP GET) and returns the CPU package temperature in degrees
// Celsius. Any HTTP/JSON/parse error or missing sensor yields nil
// (best-effort). Never returns an error.
func (c *lhmTempCollector) Collect(ctx context.Context) (*float64, error) {
	root, err := c.source.getTree(ctx)
	if err != nil {
		slog.Debug("lhm temp query failed", "url", c.source.url, "err", err)
		return nil, nil
	}
	cpu, tempNames := findLHMTemp(root)
	slog.Debug("lhm temp query ok", "url", c.source.url, "cpu_temp_c", wattsLog(cpu))
	// No CPU-package match: log the "Temperature" sensor names LHM exposed so a
	// naming mismatch (e.g. a vendor spelling this collector doesn't recognize)
	// is immediately visible instead of a silent cpu_temp_c=none.
	if cpu == nil {
		slog.Debug("lhm temp query: no CPU-package temperature sensor matched", "url", c.source.url, "temp_sensors", tempNames)
	}
	return cpu, nil
}

// findLHMTemp walks the sensor tree and returns the CPU package temperature (a
// "Temperature" sensor whose name contains "cpu package" [the Intel spelling],
// or whose name contains "package"/"tctl"/"tdie"/"die" AND sits under a CPU
// SensorId [AMD k10temp: /amdcpu/.../temperature/... — the same
// SensorId-based disambiguation findLHMPower uses for the AMD "Package" power
// leaf]), plus the names of every "Temperature" sensor seen (for a no-match
// debug log). Case-insensitive. A name containing "distance" is excluded so
// Intel's "CPU Package Distance to TjMax" margin sensor (a countdown, not an
// absolute temperature) is never mistaken for the real "CPU Package" reading.
// GPU/board temps are excluded: their SensorId is not a CPU one and their name
// doesn't match the CPU markers.
func findLHMTemp(root *lhmNode) (cpu *float64, tempNames []string) {
	var walk func(n *lhmNode)
	walk = func(n *lhmNode) {
		if strings.EqualFold(n.Type, "Temperature") {
			name := strings.ToLower(strings.TrimSpace(n.Text))
			tempNames = append(tempNames, n.Text)
			if !strings.Contains(name, "distance") {
				isCPUPackage := strings.Contains(name, "cpu package") ||
					(sensorIDIsCPU(n.SensorID) && (strings.Contains(name, "package") ||
						strings.Contains(name, "tctl") || strings.Contains(name, "tdie") || strings.Contains(name, "die")))
				if cpu == nil && isCPUPackage {
					cpu = parseLHMCelsius(n.Value)
				}
			}
		}
		for i := range n.Children {
			walk(&n.Children[i])
		}
	}
	walk(root)
	return cpu, tempNames
}

// parseLHMCelsius parses an LHM sensor value string like "45.0 °C" (or "45,0
// °C" on a comma-decimal locale, with or without a separating space before the
// unit) into Celsius, returning nil when it cannot be parsed.
func parseLHMCelsius(s string) *float64 {
	v := strings.TrimSpace(s)
	v = strings.TrimSuffix(v, "°C")
	v = strings.TrimSpace(v)
	v = strings.TrimSuffix(v, "C")
	v = strings.TrimSpace(v)
	if v == "" {
		return nil
	}
	f, err := strconv.ParseFloat(strings.Replace(v, ",", ".", 1), 64)
	if err != nil {
		return nil
	}
	return &f
}
