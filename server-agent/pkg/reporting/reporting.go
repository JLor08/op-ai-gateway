// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

// Package reporting collects minimal, non-sensitive host telemetry for an AI
// inference server. It has no external runtime dependencies.
package reporting

import (
	"bufio"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Report is the payload sent to the gateway. It intentionally excludes prompts,
// completions, model inputs, process arguments, and environment variables.
type Report struct {
	AgentID          string    `json:"agent_id"`
	RecordedAt       time.Time `json:"recorded_at"`
	OS               string    `json:"os"`
	Architecture     string    `json:"architecture"`
	CPUCount         int       `json:"cpu_count"`
	CPUUsagePercent  float64   `json:"cpu_usage_percent"`
	LoadAverage1m    float64   `json:"load_average_1m,omitempty"`
	MemoryTotalBytes uint64    `json:"memory_total_bytes,omitempty"`
	MemoryUsedBytes  uint64    `json:"memory_used_bytes,omitempty"`
}

// Collector keeps the previous CPU reading to calculate utilization.
type Collector struct {
	previousTotal uint64
	previousIdle  uint64
}

// Collect returns a best-effort host report. Metrics unsupported by the host
// remain zero so the agent can still report identity and timestamp.
func (c *Collector) Collect(agentID string) Report {
	total, idle := readCPU()
	usage := 0.0
	if c.previousTotal > 0 && total > c.previousTotal {
		usage = 100 * float64((total-c.previousTotal)-(idle-c.previousIdle)) / float64(total-c.previousTotal)
	}
	c.previousTotal, c.previousIdle = total, idle

	memoryTotal, memoryAvailable := readMemory()
	memoryUsed := uint64(0)
	if memoryTotal >= memoryAvailable {
		memoryUsed = memoryTotal - memoryAvailable
	}

	return Report{agentID, time.Now().UTC(), runtime.GOOS, runtime.GOARCH, runtime.NumCPU(), usage, readLoadAverage(), memoryTotal, memoryUsed}
}

func readCPU() (uint64, uint64) {
	file, err := os.Open("/proc/stat")
	if err != nil { return 0, 0 }
	defer file.Close()
	scanner := bufio.NewScanner(file)
	if !scanner.Scan() { return 0, 0 }
	fields := strings.Fields(scanner.Text())
	if len(fields) < 5 || fields[0] != "cpu" { return 0, 0 }
	var total uint64
	for _, field := range fields[1:] { value, _ := strconv.ParseUint(field, 10, 64); total += value }
	idle, _ := strconv.ParseUint(fields[4], 10, 64)
	return total, idle
}

func readLoadAverage() float64 {
	data, err := os.ReadFile("/proc/loadavg")
	if err != nil { return 0 }
	value, _ := strconv.ParseFloat(strings.Fields(string(data))[0], 64)
	return value
}

func readMemory() (uint64, uint64) {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil { return 0, 0 }
	values := map[string]uint64{}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 { continue }
		value, _ := strconv.ParseUint(fields[1], 10, 64)
		values[strings.TrimSuffix(fields[0], ":")] = value * 1024
	}
	return values["MemTotal"], values["MemAvailable"]
}
