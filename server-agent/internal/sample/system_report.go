// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

// This file defines the static hardware-inventory wire type. Its JSON tags are
// the wire contract; the gateway's agentSystemReport mirrors them field-for-field.
package sample

import "time"

// SystemReport is a static hardware inventory the agent collects once at startup
// and sends on connect + periodically. It NEVER contains serials, board/chassis
// UUIDs, or MAC addresses (privacy). GPU UUID is allowed (a device id, not PII).
// A hardware address leaves the host only as a keyed hash: the opaque id of a
// NetInterface on the telemetry sample, HMAC-SHA256 under a subkey of the
// host-local agent_key (without a key the id is the interface name). The raw
// MAC never leaves the host, and this report carries no hardware address at all.
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
	// VulkanDeviceCount is the number of Vulkan devices the host exposes,
	// CPU devices such as llvmpipe included; 0 means unknown or none.
	// collector.CollectHardware fills it from vulkaninfo on a host with GPUs.
	VulkanDeviceCount int `json:"vulkan_device_count,omitempty"`
	// GPULegacyVendor is the vendor (nvidia, amd, apple) this agent's runtime
	// gives a GPU without one: the first GPU tool it detected at startup, in
	// the order nvidia, amd, apple. Empty on a host without a GPU tool. It
	// comes from detection, not from GPUs, so a report that misses a vendor
	// (a timed-out collector) still names the right one.
	GPULegacyVendor string `json:"gpu_legacy_vendor,omitempty"`
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
	Vendor           string `json:"vendor,omitempty"`
	Name             string `json:"name"`
	UUID             string `json:"uuid,omitempty"`
	DriverVersion    string `json:"driver_version,omitempty"`
	MemoryTotalBytes int64  `json:"memory_total_bytes"`
	// PCIBusID is the card's PCI address (NVIDIA and AMD; empty on
	// Apple) -- see sample.GPU.PCIBusID for what it is for and, more
	// importantly, what it is not.
	PCIBusID string `json:"pci_bus_id,omitempty"`
	// VRAMMeasurable lists the APIs (cuda, rocm, vulkan) whose per-process
	// VRAM measurement works for this card. collector.CollectHardware fills
	// it per vendor from the VRAM measurer's Measurable.
	VRAMMeasurable []string `json:"vram_measurable,omitempty"`
	// VulkanOrdinal is the card's Vulkan device ordinal; nil when unknown
	// (0 is a valid ordinal, hence the pointer). collector.CollectHardware
	// fills it from vulkaninfo, matched to the card by PCI address.
	VulkanOrdinal *int `json:"vulkan_ordinal,omitempty"`
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
