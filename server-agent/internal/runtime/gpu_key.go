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
	if slices.ContainsFunc(cfg.GPUBudgets, func(b GPUBudget) bool { return b.Vendor == GPUVendorNone }) {
		budgets := slices.Clone(cfg.GPUBudgets)
		for i := range budgets {
			if budgets[i].Vendor == GPUVendorNone {
				budgets[i].Vendor = legacy
			}
		}
		cfg.GPUBudgets = budgets
	}
	var specs []Spec
	for i, spec := range cfg.Specs {
		if !slices.ContainsFunc(spec.GPUs, func(g SpecGPU) bool { return g.Vendor == GPUVendorNone }) {
			continue
		}
		if specs == nil {
			specs = slices.Clone(cfg.Specs)
		}
		gpus := slices.Clone(spec.GPUs)
		for j := range gpus {
			if gpus[j].Vendor == GPUVendorNone {
				gpus[j].Vendor = legacy
			}
		}
		specs[i].GPUs = gpus
	}
	if specs != nil {
		cfg.Specs = specs
	}
	return cfg
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

// Mixed reports whether the host has both an NVIDIA and an AMD GPU, the one
// combination whose per-vendor device numbering collides.
func (t GPUTopology) Mixed() bool {
	return slices.Contains(t.Vendors, GPUVendorNVIDIA) && slices.Contains(t.Vendors, GPUVendorAMD)
}
