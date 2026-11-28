// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package collector

import (
	"bytes"
	"context"
	"os/exec"
	"slices"
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
// a section the first deviceType, deviceName and pci* values win, and the
// first section per n wins: a header repeated later in the output opens no
// second device and its values are ignored. The result is ordered by n.
func parseVulkaninfo(out []byte) []VulkanDevice {
	p := vulkaninfoParser{seen: make(map[int]bool), cur: -1}
	for _, raw := range strings.Split(string(out), "\n") {
		line := strings.TrimSpace(raw)
		if n, ok := vulkanGPUHeader(line); ok {
			p.header(n)
			continue
		}
		p.field(line)
	}
	p.finish()
	sort.SliceStable(p.devs, func(i, j int) bool { return p.devs[i].Ordinal < p.devs[j].Ordinal })
	return p.devs
}

// vulkanPCIKeys are the vulkaninfo keys of a device's PCI address parts, in
// pciAddr order.
var vulkanPCIKeys = [4]string{"pciDomain", "pciBus", "pciDevice", "pciFunction"}

// vulkaninfoParser is parseVulkaninfo's state: the devices so far, the
// ordinals already opened, the device of the current section (cur, -1: none
// or a repeated header's) and the PCI parts read in it.
type vulkaninfoParser struct {
	devs    []VulkanDevice
	seen    map[int]bool
	cur     int
	pci     [4]uint32
	pciSeen [4]bool
}

// finish closes the current section: its device takes the PCI address read
// in it (HasPCI only when all four parts were), and the parts are reset.
func (p *vulkaninfoParser) finish() {
	if p.cur >= 0 {
		d := &p.devs[p.cur]
		d.PCI = pciAddr{Domain: p.pci[0], Bus: p.pci[1], Device: p.pci[2], Function: p.pci[3]}
		d.HasPCI = p.pciSeen[0] && p.pciSeen[1] && p.pciSeen[2] && p.pciSeen[3]
	}
	p.pci, p.pciSeen = [4]uint32{}, [4]bool{}
}

// header opens the section "GPU<n>:": a new device for the first header of
// n, nothing (its section skipped) for a repeated one.
func (p *vulkaninfoParser) header(n int) {
	p.finish()
	p.cur = -1 // a repeated header: skip its section
	if !p.seen[n] {
		p.seen[n] = true
		p.devs = append(p.devs, VulkanDevice{Ordinal: n})
		p.cur = len(p.devs) - 1
	}
}

// field reads one "key = value" line of the current section; the first
// deviceType, deviceName and pci* value wins.
func (p *vulkaninfoParser) field(line string) {
	if p.cur < 0 {
		return
	}
	key, val, ok := strings.Cut(line, "=")
	if !ok {
		return
	}
	key, val = strings.TrimSpace(key), strings.TrimSpace(val)
	d := &p.devs[p.cur]
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
		p.pciField(key, val)
	}
}

// pciField records a PCI address part unless the section already has it or
// val is not a number.
func (p *vulkaninfoParser) pciField(key, val string) {
	i := slices.Index(vulkanPCIKeys[:], key)
	if i < 0 || p.pciSeen[i] {
		return
	}
	if v, err := strconv.ParseUint(val, 0, 32); err == nil {
		p.pci[i], p.pciSeen[i] = uint32(v), true
	}
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

// vulkanDeviceCount is the number of distinct ordinals in devs: the Vulkan
// devices the host exposes, CPU devices included.
func vulkanDeviceCount(devs []VulkanDevice) int {
	seen := make(map[int]bool, len(devs))
	for _, d := range devs {
		seen[d.Ordinal] = true
	}
	return len(seen)
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
