// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package collector

import (
	"bytes"
	"context"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"
)

const vulkanTypeCPU = "PHYSICAL_DEVICE_TYPE_CPU"

// VulkanDevice is one entry of the Vulkan loader's physical-device
// enumeration. Ordinal is the index GGML_VK_VISIBLE_DEVICES addresses.
type VulkanDevice struct {
	Ordinal int
	Type    string
	Name    string
	PCI     pciAddr
	HasPCI  bool
}

// parseVulkaninfo reads the per-device "GPU<n>:" sections of `vulkaninfo`.
// Lines such as "GPU id = 0 (...)" in surface sections are not headers. Inside
// a section the first deviceType, deviceName and pci* values win. The result
// is ordered by n.
func parseVulkaninfo(out []byte) []VulkanDevice {
	var devs []VulkanDevice
	cur := -1
	var pci [4]uint32
	var pciSeen [4]bool
	finish := func() {
		if cur >= 0 {
			d := &devs[cur]
			d.PCI = pciAddr{Domain: pci[0], Bus: pci[1], Device: pci[2], Function: pci[3]}
			d.HasPCI = pciSeen[0] && pciSeen[1] && pciSeen[2] && pciSeen[3]
		}
		pci, pciSeen = [4]uint32{}, [4]bool{}
	}
	pciKeys := [4]string{"pciDomain", "pciBus", "pciDevice", "pciFunction"}
	for _, raw := range strings.Split(string(out), "\n") {
		line := strings.TrimSpace(raw)
		if n, ok := vulkanGPUHeader(line); ok {
			finish()
			devs = append(devs, VulkanDevice{Ordinal: n})
			cur = len(devs) - 1
			continue
		}
		if cur < 0 {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key, val = strings.TrimSpace(key), strings.TrimSpace(val)
		d := &devs[cur]
		switch key {
		case "deviceType":
			if d.Type == "" {
				d.Type = val
			}
		case "deviceName":
			if d.Name == "" {
				d.Name = val
			}
		default:
			for i, k := range pciKeys {
				if key != k || pciSeen[i] {
					continue
				}
				if v, err := strconv.ParseUint(val, 0, 32); err == nil {
					pci[i], pciSeen[i] = uint32(v), true
				}
			}
		}
	}
	finish()
	sort.SliceStable(devs, func(i, j int) bool { return devs[i].Ordinal < devs[j].Ordinal })
	return devs
}

// vulkanGPUHeader matches exactly "GPU<n>:".
func vulkanGPUHeader(line string) (int, bool) {
	rest, ok := strings.CutPrefix(line, "GPU")
	if !ok {
		return 0, false
	}
	num, ok := strings.CutSuffix(rest, ":")
	if !ok || num == "" {
		return 0, false
	}
	n, err := strconv.Atoi(num)
	if err != nil || n < 0 || strings.HasPrefix(num, "+") {
		return 0, false
	}
	return n, true
}

// vulkanOrdinalsByPCI maps PCI addresses to Vulkan ordinals. CPU devices and
// devices without PCI info are left out of the map (they still occupy an
// ordinal). A PCI address listed twice (one card exposed by two drivers) is
// absent: the ordinal would be a guess.
func vulkanOrdinalsByPCI(devs []VulkanDevice) map[pciAddr]int {
	m := make(map[pciAddr]int)
	dup := make(map[pciAddr]bool)
	for _, d := range devs {
		if d.Type == vulkanTypeCPU || !d.HasPCI {
			continue
		}
		if _, seen := m[d.PCI]; seen {
			dup[d.PCI] = true
			continue
		}
		m[d.PCI] = d.Ordinal
	}
	for a := range dup {
		delete(m, a)
	}
	return m
}

// collectVulkan runs vulkaninfo (5 s timeout). ok is false when the tool is
// absent or fails.
//
//nolint:unused // consumed by the hardware-report wiring in a later task of #434
func collectVulkan(ctx context.Context) (devs []VulkanDevice, ok bool) {
	if _, err := exec.LookPath("vulkaninfo"); err != nil {
		return nil, false
	}
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var stdout bytes.Buffer
	cmd := exec.CommandContext(cctx, "vulkaninfo")
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		return nil, false
	}
	return parseVulkaninfo(stdout.Bytes()), true
}
