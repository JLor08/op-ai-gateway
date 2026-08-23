// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

// This file defines the static hardware-inventory wire type. Its JSON tags are
// the wire contract; the gateway's agentSystemReport mirrors them field-for-field.
package sample

import "time"

// SystemReport is a static hardware inventory the agent collects once at startup
// and sends on connect + periodically. It NEVER contains serials, board/chassis
// UUIDs, or MAC addresses (privacy). GPU UUID is allowed (a device id, not PII).
type SystemReport struct {
	CollectedAt  time.Time  `json:"collected_at"`
	AgentVersion string     `json:"agent_version"`
	OS           string     `json:"os"`
	Arch         string     `json:"arch"`
	Kernel       string     `json:"kernel,omitempty"`
	Hostname     string     `json:"hostname,omitempty"`
	CPU          CPUInfo    `json:"cpu"`
	Memory       MemoryInfo `json:"memory"`
	Mainboard    Mainboard  `json:"mainboard"`
	BIOS         BIOS       `json:"bios"`
	GPUs         []GPUInfo  `json:"gpus"`
}

// CPUInfo is the CPU section (from gopsutil cpu.Info + cpu.Counts).
type CPUInfo struct {
	Model          string  `json:"model"`
	Vendor         string  `json:"vendor"`
	PhysicalCores  int     `json:"physical_cores"`
	LogicalThreads int     `json:"logical_threads"`
	BaseMHz        float64 `json:"base_mhz"`
}

// MemoryInfo is the RAM section: total plus best-effort per-DIMM modules.
type MemoryInfo struct {
	TotalBytes int64          `json:"total_bytes"`
	Modules    []MemoryModule `json:"modules,omitempty"`
}

// MemoryModule is one populated DIMM slot (best-effort; may be empty on Linux
// without root or on Apple Silicon unified memory).
type MemoryModule struct {
	Locator   string `json:"locator,omitempty"`
	SizeBytes int64  `json:"size_bytes"`
	Type      string `json:"type,omitempty"`
	SpeedMHz  int    `json:"speed_mhz,omitempty"`
}

// Mainboard is the baseboard section (DMI board_* / WMI Win32_BaseBoard / Apple).
type Mainboard struct {
	Vendor  string `json:"vendor"`
	Product string `json:"product"`
	Version string `json:"version"`
}

// BIOS is the firmware section (DMI bios_* / WMI Win32_BIOS / Apple boot ROM).
type BIOS struct {
	Vendor  string `json:"vendor"`
	Version string `json:"version"`
}

// GPUInfo is one GPU's static inventory (mapped from a GPU collector's sample.GPU).
type GPUInfo struct {
	Index            int    `json:"index"`
	Name             string `json:"name"`
	UUID             string `json:"uuid,omitempty"`
	DriverVersion    string `json:"driver_version,omitempty"`
	MemoryTotalBytes int64  `json:"memory_total_bytes"`
}

// Normalize forces non-nil slices so the payload always decodes on the gateway.
func (r *SystemReport) Normalize() {
	if r.GPUs == nil {
		r.GPUs = []GPUInfo{}
	}
	if r.Memory.Modules == nil {
		r.Memory.Modules = []MemoryModule{}
	}
}
