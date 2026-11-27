// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package runtime

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// GPUVendors is the set of GPU vendors the host's detection found, in
// detection precedence order. It is part of the host's GPUTopology: it tells a
// single-vendor host from a mixed one, and it resolves the "auto" device
// backend of a spec that declares no GPUs (ResolveDeviceBackend).
type GPUVendors []GPUVendor

// Device backend prefixes llama.cpp uses in device names.
const (
	backendCUDA   = "CUDA"
	backendROCm   = "ROCm"
	backendVulkan = "Vulkan"
	backendMetal  = "MTL"
	backendSYCL   = "SYCL"
)

// ResolveDeviceBackend returns the device-name prefix for spec's device
// backend. Explicit values (cuda, rocm, vulkan, metal, sycl; case-insensitive)
// map directly. "" and "auto" use the host's detected vendors: exactly one
// vendor selects its native backend (nvidia CUDA, amd ROCm, apple MTL), several
// select Vulkan, none is an error.
//
// This host-level "auto" rule applies only to a spec without GPUs. A spec that
// declares GPUs, an RPC member included, resolves "auto" from the vendors of
// its own GPUs (resolveDevicePrefix): each GPU carries its vendor, so a
// single-vendor spec on a mixed host gets its native backend.
func ResolveDeviceBackend(spec Spec, vendors GPUVendors) (string, error) {
	switch v := strings.ToLower(strings.TrimSpace(spec.DeviceBackend)); v {
	case "cuda":
		return backendCUDA, nil
	case "rocm":
		return backendROCm, nil
	case "vulkan":
		return backendVulkan, nil
	case "metal":
		return backendMetal, nil
	case "sycl":
		return backendSYCL, nil
	case "", "auto":
		// resolved from the detected vendors below
	default:
		return "", fmt.Errorf("unknown device_backend %q (want auto, cuda, rocm, vulkan, metal or sycl)", spec.DeviceBackend)
	}
	distinct := make([]GPUVendor, 0, len(vendors))
	for _, v := range vendors {
		if v == GPUVendorNone {
			continue
		}
		seen := false
		for _, d := range distinct {
			seen = seen || d == v
		}
		if !seen {
			distinct = append(distinct, v)
		}
	}
	switch len(distinct) {
	case 0:
		return "", fmt.Errorf("device_backend is auto but this host has no detected GPU vendor; set device_backend explicitly")
	case 1:
		switch distinct[0] {
		case GPUVendorNVIDIA:
			return backendCUDA, nil
		case GPUVendorAMD:
			return backendROCm, nil
		case GPUVendorApple:
			return backendMetal, nil
		}
		return "", fmt.Errorf("device_backend is auto but GPU vendor %q has no known backend; set device_backend explicitly", distinct[0])
	default:
		return backendVulkan, nil
	}
}

// rpcDeviceNames lists RPC0..RPC(n-1), n being the sum of the head's endpoint
// device counts. Empty on a spec that is not a cluster head.
func rpcDeviceNames(spec Spec) []string {
	if !spec.IsClusterHead() {
		return nil
	}
	var names []string
	for _, ep := range spec.Cluster.Endpoints {
		for range ep.DeviceCount {
			names = append(names, "RPC"+strconv.Itoa(len(names)))
		}
	}
	return names
}

// specDevices is one expansion's view of a spec's devices (#434): its GPU keys
// in declared order (an empty vendor read as the host's legacy one, planKeys),
// the per-spec device plan, or the reason none could be made. The injected
// visibility variable and every device token read this one value, so they
// always describe the same decision.
//
// On a single-vendor or vendorless host a plan that cannot be made is not by
// itself a refusal: it refuses what consumes it (the injected variable,
// ${DEVICES}, the Vulkan tokens), so a spec that uses none of them starts
// exactly as it did before the plan existed. On a host with NVIDIA and AMD GPUs
// a coded plan refusal refuses the model spec outright (mixedHostRefusal): a
// mixed host never guesses a card.
type specDevices struct {
	topo    GPUTopology
	keys    []GPUKey
	plan    devicePlan
	planErr error
}

func newSpecDevices(spec Spec, topo GPUTopology) specDevices {
	plan, err := planDevices(spec, topo)
	return specDevices{topo: topo, keys: planKeys(spec, topo), plan: plan, planErr: err}
}

// mixedHostRefusal is the plan's coded refusal of a model spec on a host with
// NVIDIA and AMD GPUs, refused whether or not anything consumes the plan. nil
// on any other host, for a plan that was made, and for an rpc_server member,
// whose device decision is the member path's (rpc_member.go).
func (d specDevices) mixedHostRefusal(spec Spec) error {
	if !d.topo.Mixed() || spec.Kind == SpecKindRPCServer || !errors.As(d.planErr, new(*GPUPlanError)) {
		return nil
	}
	return fmt.Errorf("runtime: %w", d.planErr)
}

// hostIndices is the spec's declared host indices, one per key.
func (d specDevices) hostIndices() []int {
	indices := make([]int, len(d.keys))
	for i, k := range d.keys {
		indices[i] = k.Index
	}
	return indices
}

// spansVendors reports whether the spec's GPUs belong to more than one vendor.
// Host indices are vendor-local, so a list of them has no meaning then.
func (d specDevices) spansVendors() bool {
	for _, k := range d.keys {
		if k.Vendor != d.keys[0].Vendor {
			return true
		}
	}
	return false
}

// refuseSpanning is the shared guard of the tokens that render host indices or
// a fixed-backend device list: they have no meaning for a spec whose GPUs span
// NVIDIA and AMD (NVIDIA 0 and AMD 0 are both "0").
func (d specDevices) refuseSpanning(inner string) error {
	if d.spansVendors() {
		return fmt.Errorf("${%s} has no meaning for a spec whose GPUs span NVIDIA and AMD", inner)
	}
	return nil
}

// visibility is the variable the agent injects into the child's environment
// and its value, "" when none: the plan's, for a set_visible_devices spec in
// env mode that declares GPUs. A coded plan refusal, and any plan failure on a
// host with NVIDIA and AMD GPUs, refuses the spec. On any other host a plan
// that cannot be made for another reason (an unknown device_backend, "auto"
// with no detected vendor) falls back to the host rule that applied before the
// plan existed: the legacy vendor's variable with the host indices.
func (d specDevices) visibility(spec Spec) (name, value string, err error) {
	if !injectsVisibleDevices(spec) || len(d.keys) == 0 {
		return "", "", nil
	}
	if d.planErr == nil {
		return d.plan.VisVar, d.plan.VisValue, nil
	}
	if errors.As(d.planErr, new(*GPUPlanError)) || d.topo.Mixed() {
		return "", "", fmt.Errorf("runtime: set_visible_devices: %w", d.planErr)
	}
	if name = VisibleDevicesVar(d.topo.Legacy); name == "" {
		return "", "", nil
	}
	return name, joinInts(d.hostIndices()), nil
}

// vulkanNames is what ${VULKAN_DEVICES} and ${VULKAN_FIRST_DEVICE} render: the
// plan's renumbered Vulkan0.. names when the agent injected
// GGML_VK_VISIBLE_DEVICES, else the declared host indices (the documented
// caveat: VulkanN need not be host GPU N). On a host with NVIDIA and AMD GPUs a
// host index names no reliable Vulkan device, so there the token is refused
// with runtime.vulkan_visible_devices_required (or the plan's own Vulkan
// refusal) unless the agent injected the variable.
func (d specDevices) vulkanNames(inner string) ([]string, error) {
	if d.planErr == nil && d.plan.Prefix == backendVulkan && d.plan.VisVar == vulkanVisibleDevicesVar {
		return d.plan.Names, nil
	}
	if d.topo.Mixed() {
		var pe *GPUPlanError
		if errors.As(d.planErr, &pe) && (pe.Code == codeVulkanVisibleDevicesRequired || pe.Code == codeVulkanMappingUnavailable) {
			return nil, fmt.Errorf("${%s} cannot be resolved: %w", inner, d.planErr)
		}
		return nil, fmt.Errorf("${%s} cannot be resolved: %w", inner, &GPUPlanError{
			Code:    codeVulkanVisibleDevicesRequired,
			Message: "on a host with NVIDIA and AMD GPUs a Vulkan device name needs a vulkan spec with set_visible_devices in env mode, so the agent injects GGML_VK_VISIBLE_DEVICES",
		})
	}
	if err := d.refuseSpanning(inner); err != nil {
		return nil, err
	}
	return d.hostNames(backendVulkan), nil
}

// hostNames is prefix plus each declared host index.
func (d specDevices) hostNames(prefix string) []string {
	names := make([]string, len(d.keys))
	for i, k := range d.keys {
		names[i] = prefix + strconv.Itoa(k.Index)
	}
	return names
}

// resolveClusterDeviceToken resolves ${RPC_SERVERS}, ${RPC_DEVICES},
// ${DEVICES}, ${FIRST_LOCAL_DEVICE} and the ${CUDA|VULKAN|METAL_FIRST_DEVICE}
// tokens. handled is false for any other inner text.
func resolveClusterDeviceToken(inner string, spec Spec, dev specDevices) (value string, handled bool, err error) {
	switch inner {
	case "RPC_SERVERS":
		if !spec.IsClusterHead() {
			return "", true, fmt.Errorf("${RPC_SERVERS} is only valid on a cluster head")
		}
		addrs := make([]string, len(spec.Cluster.Endpoints))
		for i, ep := range spec.Cluster.Endpoints {
			addrs[i] = ep.Address
		}
		return strings.Join(addrs, ","), true, nil
	case "RPC_DEVICES":
		if !spec.IsClusterHead() {
			return "", true, fmt.Errorf("${RPC_DEVICES} is only valid on a cluster head")
		}
		names := rpcDeviceNames(spec)
		if len(names) == 0 {
			return "", true, fmt.Errorf("${RPC_DEVICES} cannot be resolved: the cluster endpoints declare no devices")
		}
		return strings.Join(names, ","), true, nil
	case "DEVICES", "FIRST_LOCAL_DEVICE":
		return resolveGenericDevices(inner, spec, dev)
	case "CUDA_FIRST_DEVICE", "VULKAN_FIRST_DEVICE", "METAL_FIRST_DEVICE":
		if len(dev.keys) == 0 {
			return "", true, fmt.Errorf("${%s} cannot be resolved: this spec declares no gpus", inner)
		}
		if inner == "VULKAN_FIRST_DEVICE" {
			names, err := dev.vulkanNames(inner)
			if err != nil {
				return "", true, err
			}
			return names[0], true, nil
		}
		if err := dev.refuseSpanning(inner); err != nil {
			return "", true, err
		}
		prefix := map[string]string{"CUDA_FIRST_DEVICE": backendCUDA, "METAL_FIRST_DEVICE": backendMetal}[inner]
		// Explicit-backend tokens render like their ${*_DEVICES} siblings: the
		// declared host index, never renumbered.
		return dev.hostNames(prefix)[0], true, nil
	}
	return "", false, nil
}

func resolveGenericDevices(inner string, spec Spec, dev specDevices) (string, bool, error) {
	hasGPUs := len(dev.keys) > 0
	head := spec.IsClusterHead()
	if !hasGPUs && !head {
		return "", true, fmt.Errorf("${%s} cannot be resolved: this spec declares no gpus, and an empty device list selects NO device, not every device", inner)
	}
	var local []string
	if hasGPUs {
		if dev.planErr != nil {
			return "", true, fmt.Errorf("${%s} cannot be resolved: %w", inner, dev.planErr)
		}
		local = dev.plan.Names
	}
	rpc := rpcDeviceNames(spec)
	if inner == "FIRST_LOCAL_DEVICE" {
		if len(local) > 0 {
			return local[0], true, nil
		}
		if len(rpc) == 0 {
			return "", true, fmt.Errorf("${FIRST_LOCAL_DEVICE} cannot be resolved: no local gpus and no rpc devices")
		}
		return rpc[0], true, nil
	}
	return strings.Join(append(rpc, local...), ","), true, nil
}
