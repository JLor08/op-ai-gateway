// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package collector

import (
	"fmt"
	"strconv"
	"strings"
)

// pciAddr is a PCI function address including the domain. The domain is what
// distinguishes two GPUs behind different host bridges that share a bus number.
// (nvidia_pdh.go has its own domainless pciAddress for the Windows chain; the
// two are deliberately not merged.)
type pciAddr struct {
	Domain, Bus, Device, Function uint32
}

// String renders the canonical "dddd:bb:dd.f" form.
func (a pciAddr) String() string {
	return fmt.Sprintf("%04x:%02x:%02x.%x", a.Domain, a.Bus, a.Device, a.Function)
}

// parsePCIAddr parses "0000:02:00.0" (amdgpu drm-pdev, rocm-smi "PCI Bus") and
// "00000000:65:00.0" (nvidia-smi), case-insensitively. A value without a domain
// ("02:00.0") or a placeholder ("[N/A]") is rejected.
func parsePCIAddr(s string) (pciAddr, bool) {
	parts := strings.Split(strings.TrimSpace(s), ":")
	if len(parts) != 3 {
		return pciAddr{}, false
	}
	devFn := strings.Split(parts[2], ".")
	if len(devFn) != 2 {
		return pciAddr{}, false
	}
	var vals [4]uint64
	for i, p := range []string{parts[0], parts[1], devFn[0], devFn[1]} {
		v, err := strconv.ParseUint(p, 16, 32)
		if err != nil || p == "" || strings.HasPrefix(p, "+") {
			return pciAddr{}, false
		}
		vals[i] = v
	}
	return pciAddr{Domain: uint32(vals[0]), Bus: uint32(vals[1]), Device: uint32(vals[2]), Function: uint32(vals[3])}, true
}

// decodeKFDLocation turns a KFD topology location_id (bus<<8 | device<<3 |
// function) and domain into a PCI address.
func decodeKFDLocation(domain, locationID uint64) pciAddr {
	return pciAddr{
		Domain:   uint32(domain),
		Bus:      uint32(locationID >> 8),
		Device:   uint32((locationID >> 3) & 0x1f),
		Function: uint32(locationID & 0x7),
	}
}
