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
)

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
	_, err := exec.LookPath("nvidia-smi")
	return err == nil
}

// Collect runs nvidia-smi and parses its CSV output into GPUs.
func (c *nvidiaCollector) Collect(ctx context.Context) ([]sample.GPU, error) {
	cmd := exec.CommandContext(ctx, "nvidia-smi",
		"--query-gpu="+nvidiaQueryFields,
		"--format=csv,noheader,nounits",
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
