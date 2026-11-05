package runtime

import (
	"fmt"
	"strconv"
	"strings"
)

// GPUVendors is the set of GPU vendors the host's detection found, in
// detection precedence order. It exists only so a spec's "auto" device backend
// can tell a single-vendor host from a mixed one; visibility variables and
// budgets keep using the single GPUVendor.
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
// select Vulkan, none is an error. The agent cannot map a declared GPU index
// to a vendor on a mixed host (each collector numbers from 0), so the host's
// vendor set stands in for the vendor of the spec's GPUs.
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

// localDeviceNames lists the spec's local GPUs in declared order with prefix.
// With renumbered the child sees its visible devices numbered from 0, so the
// names are prefix0..prefix(k-1); otherwise the declared host indices.
func localDeviceNames(spec Spec, prefix string, renumbered bool) []string {
	indices := gpuIndices(spec)
	names := make([]string, len(indices))
	for i, idx := range indices {
		if renumbered {
			idx = i
		}
		names[i] = prefix + strconv.Itoa(idx)
	}
	return names
}

// deviceContext carries what the cluster/generic device tokens need beyond the
// spec: the detected vendors and the visibility variable the agent injects.
type deviceContext struct {
	vendors GPUVendors
	visVar  string
}

func newDeviceContext(spec Spec, vendor GPUVendor, vendors GPUVendors) deviceContext {
	if len(vendors) == 0 && vendor != GPUVendorNone {
		vendors = GPUVendors{vendor}
	}
	return deviceContext{vendors: vendors, visVar: injectedVisibleDevicesVar(spec, vendor)}
}

// renumbers reports whether the child sees its devices numbered from 0 under
// prefix: only when an injected visibility variable governs that backend
// (CUDA_VISIBLE_DEVICES for CUDA, ROCR_VISIBLE_DEVICES for ROCm). Vulkan, SYCL
// and Metal enumerate independently of those variables.
func (d deviceContext) renumbers(prefix string) bool {
	switch d.visVar {
	case "CUDA_VISIBLE_DEVICES":
		return prefix == backendCUDA
	case "ROCR_VISIBLE_DEVICES":
		return prefix == backendROCm
	}
	return false
}

// resolveClusterDeviceToken resolves ${RPC_SERVERS}, ${RPC_DEVICES},
// ${DEVICES}, ${FIRST_LOCAL_DEVICE} and the ${CUDA|VULKAN|METAL_FIRST_DEVICE}
// tokens. handled is false for any other inner text.
func resolveClusterDeviceToken(inner string, spec Spec, dev deviceContext) (value string, handled bool, err error) {
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
		if len(gpuIndices(spec)) == 0 {
			return "", true, fmt.Errorf("${%s} cannot be resolved: this spec declares no gpus", inner)
		}
		prefix := map[string]string{
			"CUDA_FIRST_DEVICE":   backendCUDA,
			"VULKAN_FIRST_DEVICE": backendVulkan,
			"METAL_FIRST_DEVICE":  backendMetal,
		}[inner]
		// Explicit-backend tokens render like their ${*_DEVICES} siblings: the
		// declared host index, never renumbered.
		return localDeviceNames(spec, prefix, false)[0], true, nil
	}
	return "", false, nil
}

func resolveGenericDevices(inner string, spec Spec, dev deviceContext) (string, bool, error) {
	hasGPUs := len(gpuIndices(spec)) > 0
	head := spec.IsClusterHead()
	if !hasGPUs && !head {
		return "", true, fmt.Errorf("${%s} cannot be resolved: this spec declares no gpus, and an empty device list selects NO device, not every device", inner)
	}
	var local []string
	if hasGPUs {
		prefix, err := ResolveDeviceBackend(spec, dev.vendors)
		if err != nil {
			return "", true, fmt.Errorf("${%s} cannot be resolved: %w", inner, err)
		}
		local = localDeviceNames(spec, prefix, dev.renumbers(prefix))
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
