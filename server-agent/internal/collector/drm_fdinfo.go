// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package collector

import (
	"bufio"
	"bytes"
	"strconv"
	"strings"
)

// drmNoiseFloorMiB is the per-card VRAM below which a process is treated as
// having merely opened the card (a Vulkan or HIP runtime enumerates every
// render node), not as using it.
const drmNoiseFloorMiB = 8

// drmClient is one amdgpu DRM client as reported by /proc/<pid>/fdinfo/<fd>.
type drmClient struct {
	PDev     pciAddr
	ClientID uint64
	VRAMKiB  uint64
	HasVRAM  bool
}

// parseDRMFdinfo parses one fdinfo file. It accepts only amdgpu clients with a
// parseable drm-pdev and drm-client-id. A missing or malformed
// drm-memory-vram leaves HasVRAM false (never a zero reading).
func parseDRMFdinfo(data []byte) (drmClient, bool) {
	var (
		c                      drmClient
		isAMD, haveDev, haveID bool
		vramSeen               bool
		vramKiB                uint64
		vramOK                 bool
	)
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		key, val, found := strings.Cut(sc.Text(), ":")
		if !found {
			continue
		}
		val = strings.TrimSpace(val)
		switch strings.TrimSpace(key) {
		case "drm-driver":
			isAMD = val == "amdgpu"
		case "drm-pdev":
			c.PDev, haveDev = parsePCIAddr(val)
		case "drm-client-id":
			id, err := strconv.ParseUint(val, 10, 64)
			if err == nil {
				c.ClientID, haveID = id, true
			}
		case "drm-memory-vram":
			vramSeen = true
			vramKiB, vramOK = parseDRMSizeKiB(val)
		}
	}
	if !isAMD || !haveDev || !haveID {
		return drmClient{}, false
	}
	if vramSeen && vramOK {
		c.VRAMKiB, c.HasVRAM = vramKiB, true
	}
	return c, true
}

// parseDRMSizeKiB parses "<n> [KiB|MiB|GiB|B]" into KiB (bare or B is bytes).
func parseDRMSizeKiB(v string) (uint64, bool) {
	fields := strings.Fields(v)
	if len(fields) == 0 || len(fields) > 2 {
		return 0, false
	}
	n, err := strconv.ParseUint(fields[0], 10, 64)
	if err != nil {
		return 0, false
	}
	unit := ""
	if len(fields) == 2 {
		unit = fields[1]
	}
	switch unit {
	case "KiB":
		return n, true
	case "MiB":
		return n << 10, true
	case "GiB":
		return n << 20, true
	case "", "B":
		return n >> 10, true
	}
	return 0, false
}

// sumDRMClients de-duplicates clients by (PDev, ClientID), first wins (a Vulkan
// process holds several fds on one render node sharing a client id), and sums
// VRAM per card. anyVRAMKey reports whether any client carried the key at all,
// which distinguishes "kernel does not report it" from "nothing used".
func sumDRMClients(clients []drmClient) (vramKiB map[pciAddr]uint64, anyVRAMKey bool) {
	type key struct {
		dev pciAddr
		id  uint64
	}
	seen := make(map[key]struct{}, len(clients))
	vramKiB = make(map[pciAddr]uint64)
	for _, c := range clients {
		k := key{c.PDev, c.ClientID}
		if _, dup := seen[k]; dup {
			continue
		}
		seen[k] = struct{}{}
		if !c.HasVRAM {
			continue
		}
		anyVRAMKey = true
		vramKiB[c.PDev] += c.VRAMKiB
	}
	return vramKiB, anyVRAMKey
}

// drmVRAMToMiB converts per-card KiB sums to MiB by card index. It truncates
// (never over-reads), drops cards below the noise floor and cards missing from
// toIndex, and returns nil when nothing remains.
func drmVRAMToMiB(sums map[pciAddr]uint64, toIndex map[pciAddr]int) map[int]int {
	var out map[int]int
	for dev, kib := range sums {
		idx, ok := toIndex[dev]
		if !ok {
			continue
		}
		mib := kib >> 10
		if mib < drmNoiseFloorMiB {
			continue
		}
		if out == nil {
			out = make(map[int]int)
		}
		out[idx] += int(mib)
	}
	return out
}
