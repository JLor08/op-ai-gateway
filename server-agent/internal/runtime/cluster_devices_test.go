// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package runtime

import (
	"strings"
	"testing"
)

func clusterHeadSpec(args ...string) Spec {
	return Spec{
		Args: args,
		Cluster: &ClusterHead{
			Endpoints: []ClusterEndpoint{{Address: "a", DeviceCount: 2}, {Address: "b", DeviceCount: 1}},
			Transport: "direct",
			Network:   "mesh",
		},
	}
}

func TestExpandClusterPlaceholders(t *testing.T) {
	getenv := func(string) string { return "" }
	gpus := []SpecGPU{{Index: 3}, {Index: 2}}
	cases := []struct {
		name    string
		spec    Spec
		tok     string
		topo    GPUTopology
		want    string
		wantErr bool
	}{
		{"head servers", func() Spec { s := clusterHeadSpec(); s.GPUs = gpus; return s }(), "${RPC_SERVERS}", GPUTopology{Legacy: GPUVendorNVIDIA}, "a,b", false},
		{"head rpc devices", func() Spec { s := clusterHeadSpec(); s.GPUs = gpus; return s }(), "${RPC_DEVICES}", GPUTopology{Legacy: GPUVendorNVIDIA}, "RPC0,RPC1,RPC2", false},
		{"head devices", func() Spec { s := clusterHeadSpec(); s.GPUs = gpus; return s }(), "${DEVICES}", GPUTopology{Legacy: GPUVendorNVIDIA, Vendors: GPUVendors{GPUVendorNVIDIA}}, "RPC0,RPC1,RPC2,CUDA3,CUDA2", false},
		{"head first local", func() Spec { s := clusterHeadSpec(); s.GPUs = gpus; return s }(), "${FIRST_LOCAL_DEVICE}", GPUTopology{Legacy: GPUVendorNVIDIA, Vendors: GPUVendors{GPUVendorNVIDIA}}, "CUDA3", false},
		{"cuda first", func() Spec { s := clusterHeadSpec(); s.GPUs = gpus; return s }(), "${CUDA_FIRST_DEVICE}", GPUTopology{Legacy: GPUVendorNVIDIA}, "CUDA3", false},
		{"vulkan first", func() Spec { s := clusterHeadSpec(); s.GPUs = gpus; return s }(), "${VULKAN_FIRST_DEVICE}", GPUTopology{Legacy: GPUVendorNVIDIA}, "Vulkan3", false},
		{"metal first", func() Spec { s := clusterHeadSpec(); s.GPUs = gpus; return s }(), "${METAL_FIRST_DEVICE}", GPUTopology{Legacy: GPUVendorNVIDIA}, "MTL3", false},
		{"env renumbered", func() Spec {
			s := clusterHeadSpec()
			s.GPUs = gpus
			s.SetVisibleDevices = true
			s.VisibleDevicesMode = VisibleDevicesModeEnv
			return s
		}(), "${DEVICES}", GPUTopology{Legacy: GPUVendorNVIDIA, Vendors: GPUVendors{GPUVendorNVIDIA}}, "RPC0,RPC1,RPC2,CUDA0,CUDA1", false},
		{"args mode keeps host indices", func() Spec {
			s := clusterHeadSpec()
			s.GPUs = gpus
			s.SetVisibleDevices = true
			s.VisibleDevicesMode = VisibleDevicesModeArgs
			return s
		}(), "${DEVICES}", GPUTopology{Legacy: GPUVendorNVIDIA, Vendors: GPUVendors{GPUVendorNVIDIA}}, "RPC0,RPC1,RPC2,CUDA3,CUDA2", false},
		{"apple env: no variable, no renumbering", func() Spec {
			s := clusterHeadSpec()
			s.GPUs = gpus
			s.SetVisibleDevices = true
			s.VisibleDevicesMode = VisibleDevicesModeEnv
			return s
		}(), "${DEVICES}", GPUTopology{Legacy: GPUVendorApple, Vendors: GPUVendors{GPUVendorApple}}, "RPC0,RPC1,RPC2,MTL3,MTL2", false},
		{"env first local renumbers", Spec{GPUs: gpus, SetVisibleDevices: true, VisibleDevicesMode: VisibleDevicesModeEnv}, "${FIRST_LOCAL_DEVICE}", GPUTopology{Legacy: GPUVendorNVIDIA, Vendors: GPUVendors{GPUVendorNVIDIA}}, "CUDA0", false},
		{"env explicit CUDA first never renumbers", Spec{GPUs: gpus, SetVisibleDevices: true, VisibleDevicesMode: VisibleDevicesModeEnv}, "${CUDA_FIRST_DEVICE}", GPUTopology{Legacy: GPUVendorNVIDIA, Vendors: GPUVendors{GPUVendorNVIDIA}}, "CUDA3", false},
		{"env explicit Vulkan first never renumbers", Spec{GPUs: gpus, SetVisibleDevices: true, VisibleDevicesMode: VisibleDevicesModeEnv}, "${VULKAN_FIRST_DEVICE}", GPUTopology{Legacy: GPUVendorNVIDIA}, "Vulkan3", false},
		{"env amd renumbers ROCm", Spec{GPUs: gpus, SetVisibleDevices: true, VisibleDevicesMode: VisibleDevicesModeEnv}, "${DEVICES}", GPUTopology{Legacy: GPUVendorAMD, Vendors: GPUVendors{GPUVendorAMD}}, "ROCm0,ROCm1", false},
		// Design §5.3: on a two-vendor host the vendor-less GPUs resolve to the legacy
		// vendor (NVIDIA), so this single-vendor spec gets its native backend, renumbered
		// by CUDA_VISIBLE_DEVICES (it was Vulkan3,Vulkan2 when the host vendor set decided).
		{"env mixed host single-vendor spec gets its native backend", Spec{GPUs: gpus, SetVisibleDevices: true, VisibleDevicesMode: VisibleDevicesModeEnv}, "${DEVICES}", GPUTopology{Legacy: GPUVendorNVIDIA, Vendors: GPUVendors{GPUVendorNVIDIA, GPUVendorAMD}}, "CUDA0,CUDA1", false},
		{"env explicit vulkan not renumbered", Spec{GPUs: gpus, DeviceBackend: "vulkan", SetVisibleDevices: true, VisibleDevicesMode: VisibleDevicesModeEnv}, "${DEVICES}", GPUTopology{Legacy: GPUVendorNVIDIA}, "Vulkan3,Vulkan2", false},
		{"env explicit sycl not renumbered", Spec{GPUs: gpus, DeviceBackend: "sycl", SetVisibleDevices: true, VisibleDevicesMode: VisibleDevicesModeEnv}, "${FIRST_LOCAL_DEVICE}", GPUTopology{Legacy: GPUVendorNVIDIA}, "SYCL3", false},
		{"zero-count endpoint adds no rpc devices", Spec{Cluster: &ClusterHead{Endpoints: []ClusterEndpoint{{Address: "a", DeviceCount: 0}, {Address: "b", DeviceCount: 2}}}}, "${RPC_DEVICES}", GPUTopology{}, "RPC0,RPC1", false},
		{"all-zero endpoints rpc devices", Spec{Cluster: &ClusterHead{Endpoints: []ClusterEndpoint{{Address: "a", DeviceCount: 0}}}}, "${RPC_DEVICES}", GPUTopology{}, "", true},
		{"head without gpus devices", clusterHeadSpec(), "${DEVICES}", GPUTopology{}, "RPC0,RPC1,RPC2", false},
		{"head without gpus first local", clusterHeadSpec(), "${FIRST_LOCAL_DEVICE}", GPUTopology{}, "RPC0", false},
		{"head without gpus cuda first", clusterHeadSpec(), "${CUDA_FIRST_DEVICE}", GPUTopology{Legacy: GPUVendorNVIDIA}, "", true},
		{"non-head no gpus devices", Spec{}, "${DEVICES}", GPUTopology{Legacy: GPUVendorNVIDIA}, "", true},
		{"non-head no gpus first local", Spec{}, "${FIRST_LOCAL_DEVICE}", GPUTopology{Legacy: GPUVendorNVIDIA}, "", true},
		{"non-head rpc servers", Spec{GPUs: gpus}, "${RPC_SERVERS}", GPUTopology{Legacy: GPUVendorNVIDIA}, "", true},
		{"non-head rpc devices", Spec{GPUs: gpus}, "${RPC_DEVICES}", GPUTopology{Legacy: GPUVendorNVIDIA}, "", true},
		{"non-head devices", Spec{GPUs: gpus}, "${DEVICES}", GPUTopology{Legacy: GPUVendorAMD, Vendors: GPUVendors{GPUVendorAMD}}, "ROCm3,ROCm2", false},
		// Design §5.3: a single-vendor spec on a two-vendor host gets its native backend
		// (it was Vulkan3,Vulkan2 when the host vendor set decided).
		{"mixed host single-vendor spec gets its native backend", Spec{GPUs: gpus}, "${DEVICES}", GPUTopology{Legacy: GPUVendorNVIDIA, Vendors: GPUVendors{GPUVendorNVIDIA, GPUVendorAMD}}, "CUDA3,CUDA2", false},
		{"explicit sycl", Spec{GPUs: gpus, DeviceBackend: "SYCL"}, "${DEVICES}", GPUTopology{Legacy: GPUVendorNVIDIA}, "SYCL3,SYCL2", false},
		{"auto without vendors", Spec{GPUs: gpus, DeviceBackend: "auto"}, "${DEVICES}", GPUTopology{}, "", true},
		{"unknown backend", Spec{GPUs: gpus, DeviceBackend: "opencl"}, "${DEVICES}", GPUTopology{Legacy: GPUVendorNVIDIA}, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.spec.Args = []string{"--device=" + tc.tok}
			args, _, err := ExpandPlaceholders(tc.spec, 8080, tc.topo, getenv, "")
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error, got args %v", args)
				}
				return
			}
			if err != nil {
				t.Fatalf("ExpandPlaceholders: %v", err)
			}
			if got := args[0]; got != "--device="+tc.want {
				t.Errorf("got %q, want %q", got, "--device="+tc.want)
			}
		})
	}

	t.Run("env value expands", func(t *testing.T) {
		s := clusterHeadSpec()
		s.Env = map[string]string{"LLAMA_ARG_RPC": "${RPC_SERVERS}"}
		_, env, err := ExpandPlaceholders(s, 8080, GPUTopology{}, getenv, "")
		if err != nil {
			t.Fatal(err)
		}
		if len(env) == 0 || env[len(env)-1] != "LLAMA_ARG_RPC=a,b" {
			t.Errorf("env = %v", env)
		}
	})

	t.Run("config file content expands", func(t *testing.T) {
		s := clusterHeadSpec()
		s.GPUs = gpus
		got, err := expandConfigFileContent("dev=${DEVICES}\nfirst=${FIRST_LOCAL_DEVICE}", s, 1, GPUTopology{Legacy: GPUVendorNVIDIA, Vendors: GPUVendors{GPUVendorNVIDIA}}, getenv, "")
		if err != nil {
			t.Fatal(err)
		}
		if want := "dev=RPC0,RPC1,RPC2,CUDA3,CUDA2\nfirst=CUDA3"; got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("single vendor fallback when no set is given", func(t *testing.T) {
		args, _, err := ExpandPlaceholders(Spec{GPUs: gpus, Args: []string{"${DEVICES}"}}, 1, GPUTopology{Legacy: GPUVendorAMD}, getenv, "")
		if err != nil || args[0] != "ROCm3,ROCm2" {
			t.Errorf("args=%v err=%v", args, err)
		}
	})
}

func TestResolveDeviceBackend(t *testing.T) {
	cases := []struct {
		backend string
		vendors GPUVendors
		want    string
		wantErr bool
	}{
		{"", GPUVendors{GPUVendorNVIDIA}, "CUDA", false},
		{"auto", GPUVendors{GPUVendorAMD}, "ROCm", false},
		{"", GPUVendors{GPUVendorApple}, "MTL", false},
		{"", GPUVendors{GPUVendorNVIDIA, GPUVendorAMD}, "Vulkan", false},
		{"", nil, "", true},
		{"auto", GPUVendors{}, "", true},
		{"CUDA", nil, "CUDA", false},
		{"rocm", nil, "ROCm", false},
		{"Vulkan", nil, "Vulkan", false},
		{"metal", nil, "MTL", false},
		{"sycl", nil, "SYCL", false},
		{"musa", nil, "", true},
	}
	for _, tc := range cases {
		got, err := ResolveDeviceBackend(Spec{DeviceBackend: tc.backend}, tc.vendors)
		if (err != nil) != tc.wantErr || got != tc.want {
			t.Errorf("ResolveDeviceBackend(%q,%v) = %q, %v; want %q (err=%v)", tc.backend, tc.vendors, got, err, tc.want, tc.wantErr)
		}
	}
}

// The agent only expands the token; whether an args-mode spec must carry a
// device placeholder is the gateway's save-time rule.
func TestVisibleDevicesArgsModeAcceptsDevicesToken(t *testing.T) {
	spec := Spec{
		Args:               []string{"--device", "${DEVICES}"},
		GPUs:               []SpecGPU{{Index: 1}},
		SetVisibleDevices:  true,
		VisibleDevicesMode: VisibleDevicesModeArgs,
	}
	args, env, err := ExpandPlaceholders(spec, 1, GPUTopology{Legacy: GPUVendorNVIDIA, Vendors: GPUVendors{GPUVendorNVIDIA}}, func(string) string { return "" }, "")
	if err != nil {
		t.Fatal(err)
	}
	if args[1] != "CUDA1" {
		t.Errorf("args = %v", args)
	}
	for _, e := range env {
		if e == "CUDA_VISIBLE_DEVICES=1" {
			t.Errorf("args mode must not inject a visibility variable: %v", env)
		}
	}
}

func TestRPCDeviceNames(t *testing.T) {
	if got := rpcDeviceNames(Spec{}); len(got) != 0 {
		t.Errorf("non-head = %v, want none", got)
	}
	got := rpcDeviceNames(Spec{Cluster: &ClusterHead{Endpoints: []ClusterEndpoint{{Address: "a", DeviceCount: 2}, {Address: "b", DeviceCount: 0}, {Address: "c", DeviceCount: 1}}}})
	if want := "RPC0,RPC1,RPC2"; strings.Join(got, ",") != want {
		t.Errorf("got %v, want %s", got, want)
	}
}
