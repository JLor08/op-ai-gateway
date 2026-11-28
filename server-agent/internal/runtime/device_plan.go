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
	vendors, err := planVendors(keys)
	if err != nil {
		return "", err
	}
	prefix, err := backendPrefix(spec, topo, vendors)
	if err != nil {
		return "", err
	}
	if err := refuseUndrivableGPUs(prefix, keys, vendors); err != nil {
		return "", err
	}
	return prefix, nil
}

// planVendors is the distinct known vendors of keys, in key order. A key
// without a vendor (a host without a detected vendor) is vendor-agnostic, as
// before; an unknown vendor is an error.
func planVendors(keys []GPUKey) ([]GPUVendor, error) {
	var vendors []GPUVendor
	for _, k := range keys {
		if k.Vendor == GPUVendorNone {
			continue // host without a detected vendor: vendor-agnostic, as before
		}
		if !knownGPUVendor(k.Vendor) {
			return nil, fmt.Errorf("unknown gpu vendor %q", string(k.Vendor))
		}
		if !slices.Contains(vendors, k.Vendor) {
			vendors = append(vendors, k.Vendor)
		}
	}
	return vendors, nil
}

// backendPrefix is the prefix of spec's device backend: an auto backend on a
// spec whose GPUs name a vendor follows those vendors (autoBackendPrefix),
// every other backend resolves as ResolveDeviceBackend does against the
// host's vendors.
func backendPrefix(spec Spec, topo GPUTopology, vendors []GPUVendor) (string, error) {
	v := strings.ToLower(strings.TrimSpace(spec.DeviceBackend))
	if (v == "" || v == "auto") && len(vendors) > 0 {
		return autoBackendPrefix(vendors), nil
	}
	prefix, err := ResolveDeviceBackend(spec, topo.Vendors)
	if err != nil {
		return "", err
	}
	return prefix, nil
}

// autoBackendPrefix is the auto backend for the spec's GPU vendors: the
// vendor's native backend for one vendor, Vulkan for several.
func autoBackendPrefix(vendors []GPUVendor) string {
	if len(vendors) != 1 {
		return backendVulkan
	}
	switch vendors[0] {
	case GPUVendorNVIDIA:
		return backendCUDA
	case GPUVendorAMD:
		return backendROCm
	case GPUVendorApple:
		return backendMetal
	}
	return backendVulkan
}

// nativeBackendVendor is the one vendor each native backend drives.
var nativeBackendVendor = map[string]GPUVendor{backendCUDA: GPUVendorNVIDIA, backendROCm: GPUVendorAMD, backendMetal: GPUVendorApple}

// refuseUndrivableGPUs refuses a native backend (CUDA, ROCm, Metal) on a GPU
// of another known vendor, and SYCL on GPUs of several vendors.
func refuseUndrivableGPUs(prefix string, keys []GPUKey, vendors []GPUVendor) error {
	if want := nativeBackendVendor[prefix]; want != GPUVendorNone {
		for _, k := range keys {
			if k.Vendor != want && k.Vendor != GPUVendorNone {
				return &GPUPlanError{
					Code:    codeGPUVendorBackendMismatch,
					Message: fmt.Sprintf("device_backend %s cannot drive GPU %s %d", strings.ToLower(prefix), k.Vendor, k.Index),
				}
			}
		}
	}
	if prefix == backendSYCL && len(vendors) > 1 {
		return &GPUPlanError{
			Code:    codeGPUVendorBackendMismatch,
			Message: "device_backend sycl cannot drive GPUs of several vendors",
		}
	}
	return nil
}

func joinInts(ns []int) string {
	parts := make([]string, len(ns))
	for i, n := range ns {
		parts[i] = strconv.Itoa(n)
	}
	return strings.Join(parts, ",")
}

// planDevices decides backend prefix, visibility variable and device names for
// one spec on this host. topo's vendors are widened by the spec's own GPU
// vendors (GPUTopology.forSpec).
func planDevices(spec Spec, topo GPUTopology) (devicePlan, error) {
	topo = topo.forSpec(spec)
	prefix, err := resolveDevicePrefix(spec, topo)
	if err != nil {
		return devicePlan{}, err
	}
	keys := planKeys(spec, topo)
	plan := devicePlan{Prefix: prefix, Keys: keys, SpansVendors: spansVendors(keys)}
	if len(keys) == 0 { // never inject an empty list
		return plan, nil
	}
	hostIdx := hostIndexes(keys)

	if !injectsVisibleDevices(spec) {
		if prefix == backendVulkan && topo.Mixed() {
			return devicePlan{}, &GPUPlanError{
				Code:    codeVulkanVisibleDevicesRequired,
				Message: "a Vulkan spec on a host with NVIDIA and AMD GPUs needs visible-device injection: without it the loader's device order is not the declared GPU order",
			}
		}
		plan.Names = hostDeviceNames(prefix, hostIdx)
		return plan, nil
	}

	switch prefix {
	case backendCUDA, backendROCm:
		plan.planNativeVisibility(hostIdx)
	case backendVulkan:
		if err := plan.planVulkanVisibility(spec, topo, hostIdx); err != nil {
			return devicePlan{}, err
		}
	default: // MTL, SYCL
		plan.VisVar, plan.Names = VisibleDevicesVar(keys[0].Vendor), hostDeviceNames(prefix, hostIdx)
		if plan.VisVar != "" {
			plan.VisValue = joinInts(hostIdx)
		}
	}
	return plan, nil
}

// spansVendors reports whether keys name more than one vendor.
func spansVendors(keys []GPUKey) bool {
	for _, k := range keys[min(1, len(keys)):] {
		if k.Vendor != keys[0].Vendor {
			return true
		}
	}
	return false
}

// hostIndexes is the deduplicated host indices of keys, in declared order.
func hostIndexes(keys []GPUKey) []int {
	hostIdx := make([]int, 0, len(keys))
	for _, k := range keys {
		hostIdx = append(hostIdx, k.Index)
	}
	return dedupInts(hostIdx)
}

// renumberedDeviceNames is prefix0..prefix<n-1>: the names a child sees once
// a visibility variable renumbered its devices.
func renumberedDeviceNames(prefix string, n int) []string {
	names := make([]string, n)
	for i := range names {
		names[i] = prefix + strconv.Itoa(i)
	}
	return names
}

// hostDeviceNames is prefix plus each host index.
func hostDeviceNames(prefix string, hostIdx []int) []string {
	names := make([]string, len(hostIdx))
	for i, idx := range hostIdx {
		names[i] = prefix + strconv.Itoa(idx)
	}
	return names
}

// planNativeVisibility is the injected visibility of a CUDA or ROCm spec: its
// vendor's variable with the host indices, the devices renumbered. A key
// without a detected vendor gets no variable, as before.
func (plan *devicePlan) planNativeVisibility(hostIdx []int) {
	if plan.Keys[0].Vendor == GPUVendorNone { // no detected vendor: no variable, as before
		plan.Names = hostDeviceNames(plan.Prefix, hostIdx)
		return
	}
	plan.VisVar = "CUDA_VISIBLE_DEVICES"
	if plan.Prefix == backendROCm {
		plan.VisVar = "ROCR_VISIBLE_DEVICES"
	}
	plan.VisValue, plan.Names = joinInts(hostIdx), renumberedDeviceNames(plan.Prefix, len(hostIdx))
}

// planVulkanVisibility is the injected visibility of a Vulkan spec: the
// declared GPUs' Vulkan ordinals in GGML_VK_VISIBLE_DEVICES where every one
// is mapped; on a mixed host without that mapping a refusal; otherwise the
// legacy vendor's variable with the host indices.
func (plan *devicePlan) planVulkanVisibility(spec Spec, topo GPUTopology, hostIdx []int) error {
	if ords, mapped := vulkanOrdinals(spec, topo, plan.Keys); mapped {
		plan.VisVar, plan.VisValue, plan.Names = vulkanVisibleDevicesVar, joinInts(ords), renumberedDeviceNames(plan.Prefix, len(ords))
		return nil
	}
	if topo.Mixed() {
		return &GPUPlanError{
			Code:    codeVulkanMappingUnavailable,
			Message: "the Vulkan device ordinal of a declared GPU is unknown or out of range on this host",
		}
	}
	plan.VisVar, plan.VisValue, plan.Names = VisibleDevicesVar(topo.Legacy), joinInts(hostIdx), hostDeviceNames(plan.Prefix, hostIdx)
	if plan.VisVar == "" {
		plan.VisValue = ""
	}
	return nil
}

// vulkanOrdinals is the deduplicated Vulkan ordinals of keys, in declared
// order; mapped is false unless every key has an in-range ordinal. A WSL
// child enumerates Vulkan inside the distro, never in the Windows host's
// vulkaninfo order the map was read from, so a WSL spec has no Vulkan mapping.
func vulkanOrdinals(spec Spec, topo GPUTopology, keys []GPUKey) (ords []int, mapped bool) {
	if spec.IsWSL() {
		return nil, false
	}
	ords = make([]int, 0, len(keys))
	for _, k := range keys {
		o, ok := topo.Vulkan.Ordinals[k]
		if !ok || o < 0 || o >= topo.Vulkan.DeviceCount {
			return nil, false
		}
		ords = append(ords, o)
	}
	return dedupInts(ords), true
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
