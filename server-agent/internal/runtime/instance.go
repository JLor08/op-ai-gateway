// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package runtime

import (
	"strconv"
	"strings"
)

// Instance addressing and the protected-floor arithmetic of a replicable spec
// (#340). A replicable spec may run several instances (processes) of itself:
// instance 0 is the spec's one ordinary process, instances k = 1... are further
// copies started on demand. Everything here is a pure function over a Spec and
// an index -- no clocks, no I/O, no manager state -- so the router, the manager
// and their tests share one definition of what an instance address is and which
// instances a spec protects.

// MaxInstanceIndex is the highest instance index k an address can carry, so a
// spec has at most MaxInstanceIndex+1 instances. It is a wire constant shared
// with the gateway (the agent module cannot import it).
const MaxInstanceIndex = 63

// instanceSeparator joins a base name and an instance index: "m#2".
const instanceSeparator = "#"

// InstanceAddress returns the address of instance k of base: base itself for
// k == 0 -- the address every spec has always had -- and "base#k" for k >= 1.
// It is the model name a client (via the gateway) sends to reach that instance.
func InstanceAddress(base string, k int) string {
	if k == 0 {
		return base
	}
	return base + instanceSeparator + strconv.Itoa(k)
}

// ParseInstanceAddress splits model at its LAST '#' into a base name and an
// instance index. The suffix is an instance index only when it is a plain
// decimal number from 1 to MaxInstanceIndex: no sign, no leading zero, no other
// character. Anything else -- no '#', an empty or non-numeric suffix, "#0", "#02",
// "#-1", "#64" -- is no instance address, and the whole string comes back as the
// base at instance 0, so a real model whose name merely contains a '#' still
// routes as itself.
//
// This function only decides whether a string LOOKS like an instance address. A
// caller that resolves model names must try the literal name first and parse
// only when it matches nothing, because "foo#2" may be the literal upstream
// model of some spec.
func ParseInstanceAddress(model string) (base string, k int) {
	i := strings.LastIndex(model, instanceSeparator)
	if i < 0 {
		return model, 0
	}
	suffix := model[i+len(instanceSeparator):]
	if !plainInstanceIndex(suffix) {
		return model, 0
	}
	n, err := strconv.Atoi(suffix)
	if err != nil || n < 1 || n > MaxInstanceIndex {
		return model, 0
	}
	return model[:i], n
}

// plainInstanceIndex reports whether s is one or two ASCII digits with no
// leading zero. strconv.Atoi alone would also accept a sign and underscore
// grouping, none of which an instance address may carry.
func plainInstanceIndex(s string) bool {
	if len(s) == 0 || len(s) > 2 || s[0] == '0' {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// InstanceKey returns the agent's key for instance k of the spec specID: the
// spec ID itself for instance 0 and "specID#k" otherwise -- the same scheme as
// InstanceAddress, so a spec that never replicates keeps the key it has always
// had.
func InstanceKey(specID string, k int) string {
	return InstanceAddress(specID, k)
}

// ProtectedCount is how many instances of spec the agent protects from idle
// reclaim and eviction: instances 0 .. ProtectedCount-1. It is the larger of the
// spec's min_instances floor and the single instance a pinned or force_running
// spec has always kept. min_instances means nothing for a spec that is not
// replicable and is ignored there.
func ProtectedCount(spec Spec) int {
	floor := 0
	if spec.Replicable {
		floor = spec.MinInstances
	}
	baseline := 0
	if spec.Pinned || spec.AdminState == "force_running" {
		baseline = 1
	}
	return max(floor, baseline)
}

// InstanceAllowed reports whether spec may have an instance k. Instance 0 is
// always allowed -- it is the spec itself. A further instance needs the spec to
// be replicable, k within MaxInstanceIndex, and k below max_instances when the
// spec sets one (0 = no cap beyond MaxInstanceIndex; the cap counts instance 0,
// so max_instances = 4 allows k = 0..3).
func InstanceAllowed(spec Spec, k int) bool {
	if k == 0 {
		return true
	}
	if k < 0 || !spec.Replicable || k > MaxInstanceIndex {
		return false
	}
	return spec.MaxInstances == 0 || k < spec.MaxInstances
}

// IsBurst reports whether instance k of spec is burst capacity: one above the
// protected floor, started on demand and first in line to be reclaimed. Instance
// 0 is never burst -- it is the spec's own process even when nothing is
// protected, so the floor of the comparison is 1.
func IsBurst(spec Spec, k int) bool {
	return k >= max(ProtectedCount(spec), 1)
}
