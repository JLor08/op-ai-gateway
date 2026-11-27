// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package runtime

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// Stable refusal codes of the per-spec device planner.
const (
	codeGPUVendorBackendMismatch     = "runtime.gpu_vendor_backend_mismatch"
	codeVulkanMappingUnavailable     = "runtime.vulkan_mapping_unavailable"
	codeVulkanVisibleDevicesRequired = "runtime.vulkan_visible_devices_required"
)

// vulkanVisibleDevicesVar is the ggml-vulkan device filter: a comma list of
// Vulkan device ordinals, kept in declared order.
const vulkanVisibleDevicesVar = "GGML_VK_VISIBLE_DEVICES"

// GPUPlanError is a planner refusal that carries a stable code the gateway
// maps to its own message.
type GPUPlanError struct {
	Code    string
	Message string
}

func (e *GPUPlanError) Error() string { return e.Message }

// devicePlan is the per-spec device decision: the device-name prefix, the
// visibility variable to inject (VisVar empty: none) with its value, and the
// local device names in declared order. Keys are the spec's GPUs, vendor
// resolved.
type devicePlan struct {
	Prefix       string
	VisVar       string
	VisValue     string
	Names        []string
	Keys         []GPUKey
	SpansVendors bool
}

// planKeys is specGPUKeys with an empty vendor read as the host's legacy one.
func planKeys(spec Spec, topo GPUTopology) []GPUKey {
	keys := make([]GPUKey, 0, len(spec.GPUs))
	for _, k := range specGPUKeys(spec) {
		if k.Vendor == GPUVendorNone {
			k.Vendor = topo.Legacy
		}
		if !slices.Contains(keys, k) {
			keys = append(keys, k)
		}
	}
	return keys
}

func knownGPUVendor(v GPUVendor) bool {
	return v == GPUVendorNVIDIA || v == GPUVendorAMD || v == GPUVendorApple
}

// resolveDevicePrefix picks the device-name prefix of spec's backend from the
// vendors of the spec's own GPUs (a spec without GPUs falls back to the
// host's vendors) and refuses a native backend that cannot drive one of them.
func resolveDevicePrefix(spec Spec, topo GPUTopology) (string, error) {
	keys := planKeys(spec, topo)
	var vendors []GPUVendor
	for _, k := range keys {
		if !knownGPUVendor(k.Vendor) {
			return "", fmt.Errorf("unknown gpu vendor %q", string(k.Vendor))
		}
		if !slices.Contains(vendors, k.Vendor) {
			vendors = append(vendors, k.Vendor)
		}
	}
	var prefix string
	switch v := strings.ToLower(strings.TrimSpace(spec.DeviceBackend)); {
	case (v == "" || v == "auto") && len(vendors) > 0:
		prefix = backendVulkan
		if len(vendors) == 1 {
			switch vendors[0] {
			case GPUVendorNVIDIA:
				prefix = backendCUDA
			case GPUVendorAMD:
				prefix = backendROCm
			case GPUVendorApple:
				prefix = backendMetal
			}
		}
	default:
		var err error
		if prefix, err = ResolveDeviceBackend(spec, topo.Vendors); err != nil {
			return "", err
		}
	}
	want := map[string]GPUVendor{backendCUDA: GPUVendorNVIDIA, backendROCm: GPUVendorAMD, backendMetal: GPUVendorApple}[prefix]
	if want != GPUVendorNone {
		for _, k := range keys {
			if k.Vendor != want {
				return "", &GPUPlanError{
					Code:    codeGPUVendorBackendMismatch,
					Message: fmt.Sprintf("device_backend %s cannot drive GPU %s %d", strings.ToLower(prefix), k.Vendor, k.Index),
				}
			}
		}
	}
	return prefix, nil
}

func joinInts(ns []int) string {
	parts := make([]string, len(ns))
	for i, n := range ns {
		parts[i] = strconv.Itoa(n)
	}
	return strings.Join(parts, ",")
}

// planDevices decides backend prefix, visibility variable and device names for
// one spec on this host.
func planDevices(spec Spec, topo GPUTopology) (devicePlan, error) {
	prefix, err := resolveDevicePrefix(spec, topo)
	if err != nil {
		return devicePlan{}, err
	}
	keys := planKeys(spec, topo)
	plan := devicePlan{Prefix: prefix, Keys: keys}
	for _, k := range keys[min(1, len(keys)):] {
		plan.SpansVendors = plan.SpansVendors || k.Vendor != keys[0].Vendor
	}
	if len(keys) == 0 { // never inject an empty list
		return plan, nil
	}
	hostIdx := make([]int, 0, len(keys))
	for _, k := range keys {
		hostIdx = append(hostIdx, k.Index)
	}
	hostIdx = dedupInts(hostIdx)
	inject := spec.SetVisibleDevices && spec.VisibleDevicesMode != VisibleDevicesModeArgs

	renumbered := func(n int) []string {
		names := make([]string, n)
		for i := range names {
			names[i] = prefix + strconv.Itoa(i)
		}
		return names
	}
	hostNames := func() []string {
		names := make([]string, len(hostIdx))
		for i, idx := range hostIdx {
			names[i] = prefix + strconv.Itoa(idx)
		}
		return names
	}

	if !inject {
		if prefix == backendVulkan && topo.Mixed() {
			return devicePlan{}, &GPUPlanError{
				Code:    codeVulkanVisibleDevicesRequired,
				Message: "a Vulkan spec on a host with NVIDIA and AMD GPUs needs visible-device injection: without it the loader's device order is not the declared GPU order",
			}
		}
		plan.Names = hostNames()
		return plan, nil
	}

	switch prefix {
	case backendCUDA:
		plan.VisVar, plan.VisValue, plan.Names = "CUDA_VISIBLE_DEVICES", joinInts(hostIdx), renumbered(len(hostIdx))
	case backendROCm:
		plan.VisVar, plan.VisValue, plan.Names = "ROCR_VISIBLE_DEVICES", joinInts(hostIdx), renumbered(len(hostIdx))
	case backendVulkan:
		ords := make([]int, 0, len(keys))
		mapped := true
		for _, k := range keys {
			o, ok := topo.Vulkan.Ordinals[k]
			if !ok || o < 0 || o >= topo.Vulkan.DeviceCount {
				mapped = false
				break
			}
			ords = append(ords, o)
		}
		switch {
		case mapped:
			ords = dedupInts(ords)
			plan.VisVar, plan.VisValue, plan.Names = vulkanVisibleDevicesVar, joinInts(ords), renumbered(len(ords))
		case topo.Mixed():
			return devicePlan{}, &GPUPlanError{
				Code:    codeVulkanMappingUnavailable,
				Message: "the Vulkan device ordinal of a declared GPU is unknown or out of range on this host",
			}
		default:
			plan.VisVar, plan.VisValue, plan.Names = VisibleDevicesVar(topo.Legacy), joinInts(hostIdx), hostNames()
			if plan.VisVar == "" {
				plan.VisValue = ""
			}
		}
	default: // MTL, SYCL
		plan.VisVar, plan.Names = VisibleDevicesVar(keys[0].Vendor), hostNames()
		if plan.VisVar != "" {
			plan.VisValue = joinInts(hostIdx)
		}
	}
	return plan, nil
}

// dedupInts drops repeats, keeping the first occurrence order.
func dedupInts(ns []int) []int {
	out := make([]int, 0, len(ns))
	for _, n := range ns {
		if !slices.Contains(out, n) {
			out = append(out, n)
		}
	}
	return out
}
