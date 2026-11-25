// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

// Package hostpower runs on a hypervisor (Proxmox) host and attributes the
// host's RAPL package energy to QEMU VMs by each VM's share of active CPU time
// (cgroup v2 cpu.stat against /proc/stat busy time), then publishes a per-VM
// synthetic powercap tree that a guest can read like a real RAPL domain. This
// file holds the pure arithmetic and parsers; it has no filesystem or clock
// access, so it is testable on any OS.
package hostpower

import (
	"math"
	"strconv"
	"strings"
)

// usecPerJiffy converts USER_HZ (100 Hz) jiffies from /proc/stat to microseconds.
const usecPerJiffy = 10000

// ParseProcStatBusyUsec returns the aggregate busy CPU time in microseconds from
// the whole /proc/stat content: (user+nice+system+irq+softirq+steal) jiffies of
// the "cpu " line. idle, iowait and guest/guest_nice are excluded (guest time is
// already inside user). ok is false when the line or a needed field is missing.
func ParseProcStatBusyUsec(procStat string) (uint64, bool) {
	for _, line := range strings.Split(procStat, "\n") {
		f := strings.Fields(line)
		if len(f) == 0 || f[0] != "cpu" {
			continue
		}
		// Needs user..steal: fields 1..8 after the label.
		if len(f) < 9 {
			return 0, false
		}
		var busy uint64
		for _, i := range []int{1, 2, 3, 6, 7, 8} {
			v, err := strconv.ParseUint(f[i], 10, 64)
			if err != nil {
				return 0, false
			}
			busy += v
		}
		return busy * usecPerJiffy, true
	}
	return 0, false
}

// ParseCgroupUsageUsec returns the usage_usec value of a cgroup v2 cpu.stat.
func ParseCgroupUsageUsec(cpuStat string) (uint64, bool) {
	for _, line := range strings.Split(cpuStat, "\n") {
		f := strings.Fields(line)
		if len(f) != 2 || f[0] != "usage_usec" {
			continue
		}
		v, err := strconv.ParseUint(f[1], 10, 64)
		if err != nil {
			return 0, false
		}
		return v, true
	}
	return 0, false
}

// Shares returns each VM's share of the host's busy CPU time delta, clamped to
// [0,1]. If the clamped shares sum to more than 1 (sampling skew between the
// /proc/stat and cgroup reads), all are divided by the sum so the VMs never
// claim more than the whole. A zero host delta yields all-zero shares. Every
// input key is present in the result.
func Shares(hostBusyDeltaUsec uint64, vmDeltaUsec map[string]uint64) map[string]float64 {
	out := make(map[string]float64, len(vmDeltaUsec))
	if hostBusyDeltaUsec == 0 {
		for k := range vmDeltaUsec {
			out[k] = 0
		}
		return out
	}
	var sum float64
	for k, d := range vmDeltaUsec {
		s := math.Min(math.Max(float64(d)/float64(hostBusyDeltaUsec), 0), 1)
		out[k] = s
		sum += s
	}
	if sum > 1 {
		for k := range out {
			out[k] /= sum
		}
	}
	return out
}

// AttributeUJ converts a host energy delta (joules) and a VM share into the VM's
// energy in microjoules. Non-finite or negative inputs yield 0.
func AttributeUJ(deltaJoules float64, share float64) uint64 {
	uj := deltaJoules * share * 1e6
	if math.IsNaN(uj) || math.IsInf(uj, 0) || uj < 0 {
		return 0
	}
	return uint64(math.Round(uj))
}
