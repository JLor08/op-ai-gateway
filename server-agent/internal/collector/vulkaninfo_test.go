// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package collector

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func synthVulkan(devs ...[4]string) []byte {
	// each entry: type, name, bus, "" (unused) ; bus "" means no PCI info
	var b strings.Builder
	b.WriteString("Device Properties and Extensions:\n=================================\n")
	for i, d := range devs {
		fmt.Fprintf(&b, "GPU%d:\nVkPhysicalDeviceProperties:\n\tdeviceType = %s\n\tdeviceName = %s\n", i, d[0], d[1])
		if d[2] != "" {
			fmt.Fprintf(&b, "VkPhysicalDevicePCIBusInfoPropertiesEXT:\n\tpciDomain   = 0\n\tpciBus      = %s\n\tpciDevice   = 0\n\tpciFunction = 0\n", d[2])
		}
		b.WriteString("\n")
	}
	return []byte(b.String())
}

const (
	vkDiscrete = "PHYSICAL_DEVICE_TYPE_DISCRETE_GPU"
	vkCPU      = "PHYSICAL_DEVICE_TYPE_CPU"
)

func TestParseVulkaninfoAMD(t *testing.T) {
	devs := parseVulkaninfo([]byte(readTestdata(t, "vulkaninfo-amd-2gpu.txt")))
	if len(devs) != 3 || devs[2].Type != "PHYSICAL_DEVICE_TYPE_CPU" || devs[2].HasPCI || devs[1].PCI != (pciAddr{0, 2, 0, 0}) {
		t.Fatalf("%+v", devs)
	}
	m := vulkanOrdinalsByPCI(devs)
	if !reflect.DeepEqual(m, map[pciAddr]int{{0, 1, 0, 0}: 0, {0, 2, 0, 0}: 1}) {
		t.Fatal(m)
	}
}

func TestParseVulkaninfoLlvmpipeFirstShiftsOrdinals(t *testing.T) {
	devs := parseVulkaninfo(synthVulkan(
		[4]string{vkCPU, "llvmpipe", ""},
		[4]string{vkDiscrete, "AMD Radeon RX 6900 XT (RADV NAVI21)", "1"},
		[4]string{vkDiscrete, "AMD Radeon RX 6900 XT (RADV NAVI21)", "2"},
	))
	if len(devs) != 3 {
		t.Fatalf("%+v", devs)
	}
	m := vulkanOrdinalsByPCI(devs)
	if !reflect.DeepEqual(m, map[pciAddr]int{{0, 1, 0, 0}: 1, {0, 2, 0, 0}: 2}) {
		t.Fatal(m)
	}
}

func TestVulkanOrdinalsByPCIDuplicateIsUnavailable(t *testing.T) {
	devs := parseVulkaninfo(synthVulkan(
		[4]string{vkDiscrete, "AMD Radeon RX 6900 XT (RADV NAVI21)", "1"},
		[4]string{vkDiscrete, "AMD Radeon RX 6900 XT (AMDVLK)", "1"},
		[4]string{vkDiscrete, "AMD Radeon RX 6900 XT (RADV NAVI21)", "2"},
	))
	m := vulkanOrdinalsByPCI(devs)
	if !reflect.DeepEqual(m, map[pciAddr]int{{0, 2, 0, 0}: 2}) {
		t.Fatal(m)
	}
}

func TestParseVulkaninfoNVIDIA(t *testing.T) {
	devs := parseVulkaninfo([]byte(readTestdata(t, "vulkaninfo-nvidia-2gpu.txt")))
	if len(devs) != 2 {
		t.Fatalf("%+v", devs)
	}
	if m := vulkanOrdinalsByPCI(devs); !reflect.DeepEqual(m, map[pciAddr]int{{0, 1, 0, 0}: 0, {0, 2, 0, 0}: 1}) {
		t.Fatal(m)
	}
}

func TestParseVulkaninfoIgnoresGPUIdLines(t *testing.T) {
	if d := parseVulkaninfo([]byte("Presentable Surfaces:\n\t\tGPU id = 0 (AMD Radeon RX 6900 XT (RADV NAVI21))\n")); len(d) != 0 {
		t.Fatalf("%+v", d)
	}
}

func TestParseVulkaninfoHexPCI(t *testing.T) {
	devs := parseVulkaninfo(synthVulkan([4]string{vkDiscrete, "x", "0x65"}))
	if len(devs) != 1 || !devs[0].HasPCI || devs[0].PCI.Bus != 0x65 {
		t.Fatalf("%+v", devs)
	}
}
