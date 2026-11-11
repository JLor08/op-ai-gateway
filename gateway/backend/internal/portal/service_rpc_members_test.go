// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"errors"
	"op-ai-gateway/internal/routing"
	"reflect"
	"strings"
	"testing"
	"time"
)

var rpcMemberTestNow = time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)

func validRPCMemberRequest() PutRPCMemberSpecRequest {
	return PutRPCMemberSpecRequest{Binary: "/opt/llama/rpc-server", Args: []string{"-t", "8"}}
}

// seedRPCHead creates a head server owned by headOwner with a runtime spec
// and attaches memberSpecID to it as a manual member of memberServerID.
func seedRPCHead(t *testing.T, svc *Service, routeStore *routing.MemoryStore, headOwner, memberServerID, memberSpecID string) ServerDTO {
	t.Helper()
	ctx := context.Background()
	head, err := svc.CreateServer(ctx, adminToken(), CreateServerRequest{
		Name: "Head", Domain: "head.example.test", OwnerIDs: []string{headOwner}, AdminGroupIDs: []string{testAdminGroupID},
	})
	if err != nil {
		t.Fatalf("create head server: %v", err)
	}
	app := seedServerAgentApplication(t, routeStore, head.ID, rpcMemberTestNow)
	mapping, err := svc.CreateMapping(ctx, adminToken(), app.ID, CreateMappingRequest{GatewayModelName: "big", AppModelName: "big"})
	if err != nil {
		t.Fatalf("create mapping: %v", err)
	}
	if err := routeStore.UpsertRuntimeSpec(ctx, routing.RuntimeSpec{
		ID: "rspec_head", MappingID: mapping.ID, Enabled: true, Binary: "/usr/bin/llama-server",
		Args: "[]", Env: "{}", CreatedAt: rpcMemberTestNow, UpdatedAt: rpcMemberTestNow,
	}); err != nil {
		t.Fatalf("head spec: %v", err)
	}
	if err := routeStore.SetClusterMembers(ctx, "rspec_head",
		[]routing.ClusterMember{{HeadSpecID: "rspec_head", Position: 0, ServerID: memberServerID, MemberSpecID: memberSpecID, Port: 47001}}, nil); err != nil {
		t.Fatalf("set members: %v", err)
	}
	return head
}

func TestPutRPCMemberSpecCRUD(t *testing.T) {
	ctx := context.Background()
	svc, _ := newServerTestService(t, rpcMemberTestNow)
	server := createTestServer(t, svc, "M", "m.example.test")

	req := validRPCMemberRequest()
	req.GPUs = []RuntimeSpecGPUDTO{{Index: 1, VRAMEstimateMB: 4000}, {Index: 0, VRAMEstimateMB: 8000}}
	req.Env = map[string]string{"FOO": "bar"}
	created, err := svc.PutRPCMemberSpec(ctx, ownerToken(), server.ID, "", req)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if !strings.HasPrefix(created.ID, "rpcm_") || len(created.ID) != len("rpcm_")+16 {
		t.Fatalf("id = %q, want rpcm_ + 16 hex", created.ID)
	}
	if !created.Enabled || created.DeviceBackend != "auto" || created.ServerID != server.ID {
		t.Fatalf("defaults wrong: %+v", created)
	}
	if !reflect.DeepEqual(created.AllowedHeadGroups, []string{testAdminGroupID}) {
		t.Fatalf("consent default = %v, want the server's admin groups", created.AllowedHeadGroups)
	}
	if len(created.GPUs) != 2 || created.GPUs[0].Index != 1 || created.GPUs[1].Index != 0 {
		t.Fatalf("gpu order must follow the request: %+v", created.GPUs)
	}

	disabled := false
	req.Enabled = &disabled
	req.GPUs = nil
	req.DeviceBackend = "vulkan"
	empty := []string{}
	req.AllowedHeadGroups = &empty
	updated, err := svc.PutRPCMemberSpec(ctx, ownerToken(), server.ID, created.ID, req)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.Enabled || updated.DeviceBackend != "vulkan" || len(updated.GPUs) != 0 || len(updated.AllowedHeadGroups) != 0 {
		t.Fatalf("update not applied: %+v", updated)
	}

	// Omitting allowed_head_groups on update keeps the stored consent.
	req.AllowedHeadGroups = nil
	again, err := svc.PutRPCMemberSpec(ctx, ownerToken(), server.ID, created.ID, req)
	if err != nil || len(again.AllowedHeadGroups) != 0 {
		t.Fatalf("consent must be kept: %+v err=%v", again, err)
	}

	list, err := svc.RPCMemberSpecs(ctx, ownerToken(), server.ID)
	if err != nil || len(list) != 1 || list[0].ID != created.ID {
		t.Fatalf("list: %+v err=%v", list, err)
	}
	if err := svc.DeleteRPCMemberSpec(ctx, ownerToken(), server.ID, created.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if list, _ := svc.RPCMemberSpecs(ctx, ownerToken(), server.ID); len(list) != 0 {
		t.Fatalf("list after delete: %+v", list)
	}
	if err := svc.DeleteRPCMemberSpec(ctx, ownerToken(), server.ID, created.ID); !errors.Is(err, ErrRPCMemberSpecNotFound) {
		t.Fatalf("second delete = %v, want not found", err)
	}
}

func TestPutRPCMemberSpecValidation(t *testing.T) {
	ctx := context.Background()
	svc, _ := newServerTestService(t, rpcMemberTestNow)
	server := createTestServer(t, svc, "M", "m.example.test")

	reserved := [][]string{
		{"-H", "0.0.0.0"},
		{"--host", "0.0.0.0"},
		{"--host=0.0.0.0"},
		{"-p", "1"},
		{"--port", "1"},
		{"--port=1"},
		{"-p1"},
		{"-d", "CUDA0"},
		{"--device", "CUDA0"},
		{"--device=CUDA0"},
		{"-c"},
		{"--cache"},
		{"--cache=1"},
	}
	for _, args := range reserved {
		req := validRPCMemberRequest()
		req.Args = args
		if _, err := svc.PutRPCMemberSpec(ctx, ownerToken(), server.ID, "", req); !errors.Is(err, ErrRPCMemberFlagReserved) {
			t.Errorf("args %v: err = %v, want member_flag_reserved", args, err)
		}
	}
	for _, args := range [][]string{{"-t", "8"}, {"--threads=8"}, {"-m", "x"}} {
		req := validRPCMemberRequest()
		req.Args = args
		if _, err := svc.PutRPCMemberSpec(ctx, ownerToken(), server.ID, "", req); err != nil {
			t.Errorf("args %v must be accepted: %v", args, err)
		}
	}

	cases := []struct {
		name string
		mut  func(*PutRPCMemberSpecRequest)
		want error
	}{
		{"relative binary", func(r *PutRPCMemberSpecRequest) { r.Binary = "rpc-server" }, ErrRuntimeSpecBinaryRequired},
		{"negative ram", func(r *PutRPCMemberSpecRequest) { r.RAMEstimateMB = -1 }, ErrRuntimeSpecTuningInvalid},
		{"duplicate gpu", func(r *PutRPCMemberSpecRequest) { r.GPUs = []RuntimeSpecGPUDTO{{Index: 0}, {Index: 0}} }, ErrRuntimeSpecGPUInvalid},
		{"bad env key", func(r *PutRPCMemberSpecRequest) { r.Env = map[string]string{"a-b": "1"} }, ErrRuntimeSpecEnvInvalid},
		{"cuda visible devices", func(r *PutRPCMemberSpecRequest) { r.Env = map[string]string{"CUDA_VISIBLE_DEVICES": "0"} }, ErrRuntimeSpecVisibleDevicesConflict},
		{"hip visible devices", func(r *PutRPCMemberSpecRequest) { r.Env = map[string]string{"HIP_VISIBLE_DEVICES": "0"} }, ErrRuntimeSpecVisibleDevicesConflict},
		{"backend", func(r *PutRPCMemberSpecRequest) { r.DeviceBackend = "opencl" }, ErrRPCMemberDeviceBackendInvalid},
		{"unknown group", func(r *PutRPCMemberSpecRequest) { g := []string{"grp_nope"}; r.AllowedHeadGroups = &g }, ErrServerAdminGroupInvalid},
	}
	for _, tc := range cases {
		req := validRPCMemberRequest()
		tc.mut(&req)
		if _, err := svc.PutRPCMemberSpec(ctx, ownerToken(), server.ID, "", req); !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", tc.name, err, tc.want)
		}
	}
}

func TestPutRPCMemberSpecVRAMEstimateOnNonNVIDIA(t *testing.T) {
	ctx := context.Background()
	svc, routeStore := newServerTestService(t, rpcMemberTestNow)
	server := createTestServer(t, svc, "M", "m.example.test")
	noEstimate := validRPCMemberRequest()
	noEstimate.GPUs = []RuntimeSpecGPUDTO{{Index: 0}}

	// Vendor unknown (no report): the estimate stays optional.
	if _, err := svc.PutRPCMemberSpec(ctx, ownerToken(), server.ID, "", noEstimate); err != nil {
		t.Fatalf("no report: %v", err)
	}
	set := func(report string) {
		t.Helper()
		if err := routeStore.UpsertServerHardware(ctx, routing.ServerHardware{ServerID: server.ID, CollectedAt: rpcMemberTestNow, UpdatedAt: rpcMemberTestNow, ReportJSON: report}); err != nil {
			t.Fatalf("hardware: %v", err)
		}
	}
	set(`{"gpus":[{"index":0,"name":"AMD Radeon RX 7900"}]}`)
	if _, err := svc.PutRPCMemberSpec(ctx, ownerToken(), server.ID, "", noEstimate); !errors.Is(err, ErrRPCMemberVRAMEstimateRequired) {
		t.Fatalf("amd without estimate: %v", err)
	}
	withEstimate := validRPCMemberRequest()
	withEstimate.GPUs = []RuntimeSpecGPUDTO{{Index: 0, VRAMEstimateMB: 8000}}
	if _, err := svc.PutRPCMemberSpec(ctx, ownerToken(), server.ID, "", withEstimate); err != nil {
		t.Fatalf("amd with estimate: %v", err)
	}
	// Mixed host: GPU 1 is AMD, so a member GPU at index 1 still needs the
	// estimate while one at index 0 (NVIDIA) does not.
	set(`{"gpus":[{"index":0,"name":"NVIDIA GeForce RTX 4090","pci_bus_id":"00000000:01:00.0"},{"index":1,"name":"AMD Radeon RX 7900"}]}`)
	nvidiaOnly := validRPCMemberRequest()
	nvidiaOnly.GPUs = []RuntimeSpecGPUDTO{{Index: 0}}
	if _, err := svc.PutRPCMemberSpec(ctx, ownerToken(), server.ID, "", nvidiaOnly); err != nil {
		t.Fatalf("mixed host, nvidia index: %v", err)
	}
	amdOnly := validRPCMemberRequest()
	amdOnly.GPUs = []RuntimeSpecGPUDTO{{Index: 1}}
	if _, err := svc.PutRPCMemberSpec(ctx, ownerToken(), server.ID, "", amdOnly); !errors.Is(err, ErrRPCMemberVRAMEstimateRequired) {
		t.Fatalf("mixed host, amd index: %v", err)
	}
	set(`{"gpus":[{"index":0,"name":"NVIDIA GeForce RTX 4090","pci_bus_id":"00000000:01:00.0"}]}`)
	if _, err := svc.PutRPCMemberSpec(ctx, ownerToken(), server.ID, "", noEstimate); err != nil {
		t.Fatalf("nvidia without estimate: %v", err)
	}
	set(`not json`)
	if _, err := svc.PutRPCMemberSpec(ctx, ownerToken(), server.ID, "", noEstimate); err != nil {
		t.Fatalf("unreadable report: %v", err)
	}
}

func TestRPCMemberSpecForeignPrincipalSeesNothing(t *testing.T) {
	ctx := context.Background()
	svc, _ := newServerTestService(t, rpcMemberTestNow)
	server := createTestServer(t, svc, "M", "m.example.test")
	created, err := svc.PutRPCMemberSpec(ctx, ownerToken(), server.ID, "", validRPCMemberRequest())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.RPCMemberSpecs(ctx, otherToken(), server.ID); !errors.Is(err, ErrServerNotFound) {
		t.Fatalf("list = %v, want ErrServerNotFound", err)
	}
	if _, err := svc.PutRPCMemberSpec(ctx, otherToken(), server.ID, "", validRPCMemberRequest()); !errors.Is(err, ErrServerNotFound) {
		t.Fatalf("create = %v", err)
	}
	if _, err := svc.PutRPCMemberSpec(ctx, otherToken(), server.ID, created.ID, validRPCMemberRequest()); !errors.Is(err, ErrServerNotFound) {
		t.Fatalf("update = %v", err)
	}
	if err := svc.DeleteRPCMemberSpec(ctx, otherToken(), server.ID, created.ID); !errors.Is(err, ErrServerNotFound) {
		t.Fatalf("delete = %v", err)
	}
}

func TestRPCMemberSpecAddressedThroughAnotherServerIsNotFound(t *testing.T) {
	ctx := context.Background()
	svc, _ := newServerTestService(t, rpcMemberTestNow)
	a := createTestServer(t, svc, "A", "a.example.test")
	b := createTestServer(t, svc, "B", "b.example.test")
	created, err := svc.PutRPCMemberSpec(ctx, ownerToken(), a.ID, "", validRPCMemberRequest())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.PutRPCMemberSpec(ctx, ownerToken(), b.ID, created.ID, validRPCMemberRequest()); !errors.Is(err, ErrRPCMemberSpecNotFound) {
		t.Fatalf("update via wrong server = %v", err)
	}
	if err := svc.DeleteRPCMemberSpec(ctx, ownerToken(), b.ID, created.ID); !errors.Is(err, ErrRPCMemberSpecNotFound) {
		t.Fatalf("delete via wrong server = %v", err)
	}
}

func TestRPCMemberSpecInUseBlocksDeleteAndDisable(t *testing.T) {
	ctx := context.Background()
	svc, routeStore := newServerTestService(t, rpcMemberTestNow)
	server := createTestServer(t, svc, "M", "m.example.test")
	created, err := svc.PutRPCMemberSpec(ctx, ownerToken(), server.ID, "", validRPCMemberRequest())
	if err != nil {
		t.Fatal(err)
	}
	// The head belongs to a different owner: the member's owner must not
	// learn the head server's name.
	seedRPCHead(t, svc, routeStore, "usr_other", server.ID, created.ID)

	err = svc.DeleteRPCMemberSpec(ctx, ownerToken(), server.ID, created.ID)
	if !errors.Is(err, ErrRPCMemberInUse) {
		t.Fatalf("delete = %v, want in use", err)
	}
	if strings.Contains(err.Error(), "Head") {
		t.Fatalf("error leaks the head name: %v", err)
	}
	disabled := false
	req := validRPCMemberRequest()
	req.Enabled = &disabled
	if _, err := svc.PutRPCMemberSpec(ctx, ownerToken(), server.ID, created.ID, req); !errors.Is(err, ErrRPCMemberInUse) {
		t.Fatalf("disable = %v, want in use", err)
	}
	list, err := svc.RPCMemberSpecs(ctx, ownerToken(), server.ID)
	if err != nil || len(list) != 1 || !list[0].Enabled || list[0].UsedByHeadSpecID != "" {
		t.Fatalf("head must stay hidden and spec untouched: %+v err=%v", list, err)
	}

	// A caller who may see the head gets its name and the head spec id.
	err = svc.DeleteRPCMemberSpec(ctx, adminToken(), server.ID, created.ID)
	if !errors.Is(err, ErrRPCMemberInUse) || !strings.Contains(err.Error(), "Head") {
		t.Fatalf("admin delete = %v, want in-use naming the head", err)
	}
	list, _ = svc.RPCMemberSpecs(ctx, adminToken(), server.ID)
	if len(list) != 1 || list[0].UsedByHeadSpecID != "rspec_head" {
		t.Fatalf("admin list = %+v", list)
	}

	// Editing other fields of a referenced spec stays allowed.
	req = validRPCMemberRequest()
	req.Args = []string{"-t", "4"}
	if _, err := svc.PutRPCMemberSpec(ctx, ownerToken(), server.ID, created.ID, req); err != nil {
		t.Fatalf("edit while referenced: %v", err)
	}
}

func TestRPCMemberSpecWritesNotifyMemberAndHead(t *testing.T) {
	ctx := context.Background()
	svc, routeStore := newServerTestService(t, rpcMemberTestNow)
	calls := recordRuntimeChanged(svc)
	member := createTestServer(t, svc, "M", "m.example.test")

	created, err := svc.PutRPCMemberSpec(ctx, ownerToken(), member.ID, "", validRPCMemberRequest())
	if err != nil {
		t.Fatal(err)
	}
	if got := calls(); !reflect.DeepEqual(got, []string{member.ID}) {
		t.Fatalf("unreferenced create notified %v, want only the member", got)
	}
	head := seedRPCHead(t, svc, routeStore, "usr_owner", member.ID, created.ID)
	before := len(calls())
	if _, err := svc.PutRPCMemberSpec(ctx, ownerToken(), member.ID, created.ID, validRPCMemberRequest()); err != nil {
		t.Fatal(err)
	}
	got := calls()[before:]
	if !reflect.DeepEqual(got, []string{member.ID, head.ID}) {
		t.Fatalf("referenced update notified %v, want member then head", got)
	}

	lone, err := svc.PutRPCMemberSpec(ctx, ownerToken(), member.ID, "", validRPCMemberRequest())
	if err != nil {
		t.Fatal(err)
	}
	before = len(calls())
	if err := svc.DeleteRPCMemberSpec(ctx, ownerToken(), member.ID, lone.ID); err != nil {
		t.Fatal(err)
	}
	if got := calls()[before:]; !reflect.DeepEqual(got, []string{member.ID}) {
		t.Fatalf("delete notified %v", got)
	}
}
