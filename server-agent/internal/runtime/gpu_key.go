// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package runtime

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// GPUKey identifies one GPU on this host. A bare index is ambiguous once an
// NVIDIA and an AMD card share a machine (both vendors number their cards from
// 0), so every GPU-keyed map in this package -- budgets, measurements, detected
// totals, the policy's per-GPU sums -- keys on the vendor as well as the
// index. It is comparable, so it is a map key as it stands.
//
// An empty Vendor is the legacy "the host's only vendor" of a pre-#434
// gateway's document; resolveLegacyGPUVendors replaces it with the host's
// legacy vendor before the document reaches the owner, so the keys the manager
// builds from specs and budgets always carry one on a GPU host.
type GPUKey struct {
	Vendor GPUVendor
	Index  int
}

// String renders the key as "vendor:index", e.g. "nvidia:0" (":0" for an
// empty vendor). It is also the key's text form (MarshalText), so a
// map[GPUKey]int marshals to a JSON object keyed by that string.
func (k GPUKey) String() string {
	return string(k.Vendor) + ":" + strconv.Itoa(k.Index)
}

// MarshalText is String as bytes.
func (k GPUKey) MarshalText() ([]byte, error) {
	return []byte(k.String()), nil
}

// UnmarshalText parses String's form. The vendor must be empty or one of the
// known vendors, and the index a non-negative integer.
func (k *GPUKey) UnmarshalText(b []byte) error {
	s := string(b)
	i := strings.LastIndexByte(s, ':')
	if i < 0 {
		return fmt.Errorf("gpu key %q: want vendor:index", s)
	}
	vendor := GPUVendor(s[:i])
	if ParseGPUVendor(string(vendor)) != vendor {
		return fmt.Errorf("gpu key %q: unknown vendor %q", s, vendor)
	}
	idx, err := strconv.Atoi(s[i+1:])
	if err != nil || idx < 0 {
		return fmt.Errorf("gpu key %q: invalid index", s)
	}
	*k = GPUKey{Vendor: vendor, Index: idx}
	return nil
}

// Key is the GPU's key.
func (g SpecGPU) Key() GPUKey { return GPUKey{Vendor: g.Vendor, Index: g.Index} }

// Key is the budgeted GPU's key.
func (b GPUBudget) Key() GPUKey { return GPUKey{Vendor: b.Vendor, Index: b.Index} }

// resolveLegacyGPUVendors returns a copy of cfg in which every spec GPU and
// every budget with an empty vendor carries legacy instead: the vendor the
// agent treated as "the" vendor before GPUs were keyed by vendor (the first
// detected of NVIDIA, AMD, Apple). An empty vendor is what a gateway from
// before the vendor field sends, so it keeps exactly its old meaning. A
// gateway that later backfills that same vendor explicitly therefore pushes a
// spec that is DeepEqual to the resolved one, and nothing relaunches on the
// upgrade. Explicit vendors are kept. cfg itself is never written: a slice
// that needs a change is cloned first.
func resolveLegacyGPUVendors(cfg Config, legacy GPUVendor) Config {
	if legacy == GPUVendorNone {
		return cfg
	}
	cfg.GPUBudgets = legacyResolvedBudgets(cfg.GPUBudgets, legacy)
	cfg.Specs = legacyResolvedSpecs(cfg.Specs, legacy)
	return cfg
}

// legacyResolvedBudgets is budgets with every empty vendor read as legacy:
// budgets itself when none is empty, else a changed clone.
func legacyResolvedBudgets(budgets []GPUBudget, legacy GPUVendor) []GPUBudget {
	if !slices.ContainsFunc(budgets, func(b GPUBudget) bool { return b.Vendor == GPUVendorNone }) {
		return budgets
	}
	out := slices.Clone(budgets)
	for i := range out {
		if out[i].Vendor == GPUVendorNone {
			out[i].Vendor = legacy
		}
	}
	return out
}

// legacyResolvedSpecs is specs with every empty GPU vendor read as legacy:
// specs itself when no GPU's vendor is empty, else a clone whose changed
// specs carry a changed clone of their GPUs.
func legacyResolvedSpecs(specs []Spec, legacy GPUVendor) []Spec {
	var out []Spec
	for i, spec := range specs {
		if !slices.ContainsFunc(spec.GPUs, func(g SpecGPU) bool { return g.Vendor == GPUVendorNone }) {
			continue
		}
		if out == nil {
			out = slices.Clone(specs)
		}
		out[i].GPUs = legacyResolvedSpecGPUs(spec.GPUs, legacy)
	}
	if out == nil {
		return specs
	}
	return out
}

// legacyResolvedSpecGPUs is a clone of gpus with every empty vendor read as
// legacy.
func legacyResolvedSpecGPUs(gpus []SpecGPU, legacy GPUVendor) []SpecGPU {
	out := slices.Clone(gpus)
	for j := range out {
		if out[j].Vendor == GPUVendorNone {
			out[j].Vendor = legacy
		}
	}
	return out
}

// GPUTopology is what the per-spec device planner needs to know about the
// host: Legacy is the vendor an empty-vendor GPU stands for, Vendors every
// vendor detection found, Vulkan how the host's Vulkan devices line up with
// its GPUs.
type GPUTopology struct {
	Legacy  GPUVendor
	Vendors GPUVendors
	Vulkan  VulkanMap
}

// forSpec is t with the vendors of spec's own GPU keys (an empty vendor read
// as Legacy) added to Vendors, in nvidia, amd, apple order. A spec naming a
// card of a vendor proves the host has it, so a reading that missed that
// vendor (a timed-out collector) never makes a real NVIDIA+AMD host look
// single-vendor to the plan of a spec spanning or naming both. t is not
// changed; idempotent.
func (t GPUTopology) forSpec(spec Spec) GPUTopology {
	present := make(map[GPUVendor]bool, len(gpuVendorPrecedence))
	for _, v := range t.Vendors {
		present[v] = true
	}
	added := false
	for _, k := range planKeys(spec, t) {
		if knownGPUVendor(k.Vendor) && !present[k.Vendor] {
			present[k.Vendor], added = true, true
		}
	}
	if !added {
		return t
	}
	vendors := make(GPUVendors, 0, len(present))
	for _, v := range gpuVendorPrecedence {
		if present[v] {
			vendors = append(vendors, v)
		}
	}
	t.Vendors = vendors
	return t
}

// Mixed reports whether the host has both an NVIDIA and an AMD GPU, the one
// combination whose per-vendor device numbering collides.
func (t GPUTopology) Mixed() bool {
	return slices.Contains(t.Vendors, GPUVendorNVIDIA) && slices.Contains(t.Vendors, GPUVendorAMD)
}
