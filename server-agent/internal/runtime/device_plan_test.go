// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package runtime

import (
	"errors"
	"reflect"
	"testing"
)

func TestPlanDevices(t *testing.T) {
	nv2 := GPUTopology{Legacy: GPUVendorNVIDIA, Vendors: GPUVendors{GPUVendorNVIDIA}}
	amd2 := GPUTopology{Legacy: GPUVendorAMD, Vendors: GPUVendors{GPUVendorAMD}, Vulkan: VulkanMap{
		Ordinals:    map[GPUKey]int{{GPUVendorAMD, 0}: 0, {GPUVendorAMD, 1}: 1},
		DeviceCount: 3,
	}}
	amd2Llvm := GPUTopology{Legacy: GPUVendorAMD, Vendors: GPUVendors{GPUVendorAMD}, Vulkan: VulkanMap{
		Ordinals:    map[GPUKey]int{{GPUVendorAMD, 0}: 1, {GPUVendorAMD, 1}: 2},
		DeviceCount: 3,
	}}
	mixedVendors := GPUVendors{GPUVendorNVIDIA, GPUVendorAMD}
	mixed := GPUTopology{Legacy: GPUVendorNVIDIA, Vendors: mixedVendors, Vulkan: VulkanMap{
		Ordinals:    map[GPUKey]int{{GPUVendorNVIDIA, 0}: 0, {GPUVendorAMD, 0}: 1, {GPUVendorAMD, 1}: 2},
		DeviceCount: 4,
	}}
	mixedNoVK := GPUTopology{Legacy: GPUVendorNVIDIA, Vendors: mixedVendors}
	mixedStale := GPUTopology{Legacy: GPUVendorNVIDIA, Vendors: mixedVendors, Vulkan: VulkanMap{
		Ordinals:    map[GPUKey]int{{GPUVendorAMD, 0}: 7},
		DeviceCount: 4,
	}}

	nv := func(i int) SpecGPU { return SpecGPU{Vendor: GPUVendorNVIDIA, Index: i} }
	amd := func(i int) SpecGPU { return SpecGPU{Vendor: GPUVendorAMD, Index: i} }
	k := func(v GPUVendor, i int) GPUKey { return GPUKey{Vendor: v, Index: i} }

	cases := []struct {
		name    string
		topo    GPUTopology
		gpus    []SpecGPU
		backend string
		inject  bool
		args    bool
		want    devicePlan
		wantErr string
	}{
		{
			name: "nvidia auto", topo: nv2, gpus: []SpecGPU{nv(1), nv(0)}, backend: "auto", inject: true,
			want: devicePlan{
				Prefix: "CUDA", VisVar: "CUDA_VISIBLE_DEVICES", VisValue: "1,0", Names: []string{"CUDA0", "CUDA1"},
				Keys: []GPUKey{k(GPUVendorNVIDIA, 1), k(GPUVendorNVIDIA, 0)},
			},
		},
		{
			name: "amd auto", topo: amd2, gpus: []SpecGPU{amd(1)}, backend: "auto", inject: true,
			want: devicePlan{
				Prefix: "ROCm", VisVar: "ROCR_VISIBLE_DEVICES", VisValue: "1", Names: []string{"ROCm0"},
				Keys: []GPUKey{k(GPUVendorAMD, 1)},
			},
		},
		{
			name: "mixed auto goes vulkan", topo: mixed, gpus: []SpecGPU{nv(0), amd(1)}, backend: "auto", inject: true,
			want: devicePlan{
				Prefix: "Vulkan", VisVar: "GGML_VK_VISIBLE_DEVICES", VisValue: "0,2", Names: []string{"Vulkan0", "Vulkan1"},
				Keys: []GPUKey{k(GPUVendorNVIDIA, 0), k(GPUVendorAMD, 1)}, SpansVendors: true,
			},
		},
		{
			name: "mixed single-vendor spec keeps native", topo: mixed, gpus: []SpecGPU{amd(0)}, backend: "auto", inject: true,
			want: devicePlan{
				Prefix: "ROCm", VisVar: "ROCR_VISIBLE_DEVICES", VisValue: "0", Names: []string{"ROCm0"},
				Keys: []GPUKey{k(GPUVendorAMD, 0)},
			},
		},
		{
			name: "declared order preserved", topo: amd2, gpus: []SpecGPU{amd(1), amd(0)}, backend: "vulkan", inject: true,
			want: devicePlan{
				Prefix: "Vulkan", VisVar: "GGML_VK_VISIBLE_DEVICES", VisValue: "1,0", Names: []string{"Vulkan0", "Vulkan1"},
				Keys: []GPUKey{k(GPUVendorAMD, 1), k(GPUVendorAMD, 0)},
			},
		},
		{
			name: "both vendors with cuda", topo: mixed, gpus: []SpecGPU{nv(0), amd(0)}, backend: "cuda", inject: true,
			wantErr: codeGPUVendorBackendMismatch,
		},
		{name: "nvidia with rocm", topo: nv2, gpus: []SpecGPU{nv(0)}, backend: "rocm", wantErr: codeGPUVendorBackendMismatch},
		{name: "amd with cuda", topo: amd2, gpus: []SpecGPU{amd(0)}, backend: "cuda", wantErr: codeGPUVendorBackendMismatch},
		{
			name: "vulkan unmapped, mixed", topo: mixedNoVK, gpus: []SpecGPU{amd(0)}, backend: "vulkan", inject: true,
			wantErr: codeVulkanMappingUnavailable,
		},
		{
			name: "stale ordinal", topo: mixedStale, gpus: []SpecGPU{amd(0)}, backend: "vulkan", inject: true,
			wantErr: codeVulkanMappingUnavailable,
		},
		{
			name: "vulkan unmapped, single-vendor", topo: nv2, gpus: []SpecGPU{nv(2)}, backend: "vulkan", inject: true,
			want: devicePlan{
				Prefix: "Vulkan", VisVar: "CUDA_VISIBLE_DEVICES", VisValue: "2", Names: []string{"Vulkan2"},
				Keys: []GPUKey{k(GPUVendorNVIDIA, 2)},
			},
		},
		{
			name: "vulkan without inject, mixed", topo: mixed, gpus: []SpecGPU{nv(0)}, backend: "vulkan",
			wantErr: codeVulkanVisibleDevicesRequired,
		},
		{
			name: "vulkan args mode, mixed", topo: mixed, gpus: []SpecGPU{nv(0)}, backend: "vulkan", inject: true, args: true,
			wantErr: codeVulkanVisibleDevicesRequired,
		},
		{
			name: "vulkan without inject, single-vendor, llvmpipe first", topo: amd2Llvm, gpus: []SpecGPU{amd(1)}, backend: "vulkan",
			want: devicePlan{Prefix: "Vulkan", Names: []string{"Vulkan1"}, Keys: []GPUKey{k(GPUVendorAMD, 1)}},
		},
		{
			name: "no detected vendor, explicit cuda", topo: GPUTopology{}, gpus: []SpecGPU{{Index: 0}}, backend: "cuda", inject: true,
			want: devicePlan{Prefix: "CUDA", Names: []string{"CUDA0"}, Keys: []GPUKey{k(GPUVendorNone, 0)}},
		},
		{
			name: "empty vendor reads as legacy", topo: nv2, gpus: []SpecGPU{{Index: 1}}, backend: "auto", inject: true,
			want: devicePlan{
				Prefix: "CUDA", VisVar: "CUDA_VISIBLE_DEVICES", VisValue: "1", Names: []string{"CUDA0"},
				Keys: []GPUKey{k(GPUVendorNVIDIA, 1)},
			},
		},
		{
			name: "sycl across vendors is refused", topo: mixed, gpus: []SpecGPU{nv(0), amd(0)}, backend: "sycl", inject: true,
			wantErr: codeGPUVendorBackendMismatch,
		},
		{
			name: "sycl single vendor", topo: nv2, gpus: []SpecGPU{nv(1)}, backend: "sycl", inject: true,
			want: devicePlan{
				Prefix: "SYCL", VisVar: "CUDA_VISIBLE_DEVICES", VisValue: "1", Names: []string{"SYCL1"},
				Keys: []GPUKey{k(GPUVendorNVIDIA, 1)},
			},
		},
		{
			name: "metal single vendor has no variable", topo: GPUTopology{Legacy: GPUVendorApple, Vendors: GPUVendors{GPUVendorApple}},
			gpus: []SpecGPU{{Vendor: GPUVendorApple, Index: 0}}, backend: "metal", inject: true,
			want: devicePlan{Prefix: "MTL", Names: []string{"MTL0"}, Keys: []GPUKey{k(GPUVendorApple, 0)}},
		},
		{
			name: "no GPUs never injects", topo: mixed, backend: "vulkan", inject: true,
			want: devicePlan{Prefix: "Vulkan", Keys: []GPUKey{}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec := Spec{GPUs: tc.gpus, DeviceBackend: tc.backend, SetVisibleDevices: tc.inject}
			if tc.args {
				spec.VisibleDevicesMode = VisibleDevicesModeArgs
			}
			got, err := planDevices(spec, tc.topo)
			if tc.wantErr != "" {
				var pe *GPUPlanError
				if !errors.As(err, &pe) || pe.Code != tc.wantErr {
					t.Fatalf("planDevices err = %v, want GPUPlanError %s", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("planDevices: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("planDevices = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestPlanDevicesUnknownVendorIsPlainError(t *testing.T) {
	_, err := planDevices(Spec{GPUs: []SpecGPU{{Vendor: "intel", Index: 0}}, DeviceBackend: "auto"}, GPUTopology{})
	var pe *GPUPlanError
	if err == nil || errors.As(err, &pe) {
		t.Fatalf("err = %v, want a plain error", err)
	}
}

func TestResolveDevicePrefix(t *testing.T) {
	topo := GPUTopology{Legacy: GPUVendorNVIDIA, Vendors: GPUVendors{GPUVendorNVIDIA, GPUVendorAMD}}
	gpus := []SpecGPU{{Vendor: GPUVendorNVIDIA}, {Vendor: GPUVendorAMD}}
	got, err := resolveDevicePrefix(Spec{GPUs: gpus, DeviceBackend: "auto"}, topo)
	if err != nil || got != "Vulkan" {
		t.Fatalf("auto = %q, %v, want Vulkan", got, err)
	}
	_, err = resolveDevicePrefix(Spec{GPUs: gpus, DeviceBackend: "cuda"}, topo)
	var pe *GPUPlanError
	if !errors.As(err, &pe) || pe.Code != codeGPUVendorBackendMismatch {
		t.Fatalf("cuda err = %v, want %s", err, codeGPUVendorBackendMismatch)
	}
}
