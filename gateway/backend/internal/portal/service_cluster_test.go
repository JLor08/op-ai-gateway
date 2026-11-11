// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"errors"
	"fmt"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/capture"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/store"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

var clusterTestNow = time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)

const (
	clusterHeadBinary = "/opt/llama/llama-server"
	clusterRPCBinary  = "/opt/llama/ggml-rpc-server"
	clusterCommit     = "2bc46ba"
)

// clusterFixture is a head server with a server_agent application and one
// mapping, plus member servers created on demand. Every server is managed by
// ownerToken, the head's binary is in the inventory with a known version,
// and notifications are recorded.
type clusterFixture struct {
	t         *testing.T
	ctx       context.Context
	svc       *Service
	routes    *routing.MemoryStore
	dir       *MemoryDirectory
	settings  *MemorySystemSettings
	head      ServerDTO
	app       routing.Application
	mappingID string

	mu       sync.Mutex
	notified []string
}

func newClusterFixture(t *testing.T) *clusterFixture {
	t.Helper()
	svc, routes, dir := newServerTestServiceWithDir(t, clusterTestNow)
	settings := NewMemorySystemSettings()
	svc.settings = settings
	f := &clusterFixture{t: t, ctx: context.Background(), svc: svc, routes: routes, dir: dir, settings: settings}
	svc.SetRuntimeConfigChangedHook(func(id string) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.notified = append(f.notified, id)
	})
	f.head = createTestServer(t, svc, "Head", "head.example.test")
	f.app = seedServerAgentApplication(t, routes, f.head.ID, clusterTestNow)
	mapping, err := svc.CreateMapping(f.ctx, ownerToken(), f.app.ID, CreateMappingRequest{GatewayModelName: "big", AppModelName: "big"})
	if err != nil {
		t.Fatalf("create mapping: %v", err)
	}
	f.mappingID = mapping.ID
	f.binary(f.head.ID, clusterHeadBinary, BinaryKindLlamaServer, clusterCommit)
	return f
}

// binary adds a manual inventory row; commit "" leaves the version unknown.
func (f *clusterFixture) binary(serverID, path, kind, commit string) {
	f.t.Helper()
	b := routing.ServerBinary{ServerID: serverID, Path: path, Kind: kind, Commit: commit}
	if commit != "" {
		b.Version = "version: 0.9.0 (build 1, commit " + commit + ")"
		b.Build = "1"
	}
	if err := f.routes.UpsertManualServerBinary(f.ctx, b); err != nil {
		f.t.Fatalf("binary: %v", err)
	}
}

// member creates a server managed by ownerToken that is ready to be an
// automatic member: RPC membership on, consent for the head's admin group and
// a matching ggml-rpc-server.
func (f *clusterFixture) member(name string) ServerDTO {
	f.t.Helper()
	srv := createTestServer(f.t, f.svc, name, name+".example.test")
	f.updateServer(srv.ID, func(s *routing.AIServer) {
		s.RPCMemberEnabled = true
		s.RPCAllowedHeadGroups = encodeRPCHeadGroups([]string{testAdminGroupID})
	})
	f.binary(srv.ID, clusterRPCBinary, BinaryKindRPCServer, clusterCommit)
	return srv
}

// foreignServer creates a server owned by usr_other in an admin group
// ownerToken does not manage; it consents to the head's admin group, offers
// RPC membership and has a matching ggml-rpc-server.
const clusterForeignGroup = "ugrp_cluster_foreign"

// foreignGroup returns an admin group ownerToken does not manage.
func (f *clusterFixture) foreignGroup() string {
	f.t.Helper()
	const group = clusterForeignGroup
	if _, err := f.dir.UserGroupByID(f.ctx, group); err != nil {
		if err := f.dir.CreateUserGroup(f.ctx, store.UserGroup{
			ID: group, Tier: store.GroupTierAdmin, Name: "Foreign", ParentGroupID: testSystemGroupID,
			OwnerUserID: "usr_other", CreatedAt: clusterTestNow, UpdatedAt: clusterTestNow,
		}); err != nil {
			f.t.Fatalf("foreign group: %v", err)
		}
	}
	return group
}

func (f *clusterFixture) foreignServer(name string) ServerDTO {
	f.t.Helper()
	group := f.foreignGroup()
	srv, err := f.svc.CreateServer(f.ctx, systemToken(), CreateServerRequest{
		Name: name, Domain: name + ".example.test", OwnerIDs: []string{"usr_other"}, AdminGroupIDs: []string{group},
	})
	if err != nil {
		f.t.Fatalf("foreign server: %v", err)
	}
	f.updateServer(srv.ID, func(s *routing.AIServer) {
		s.RPCMemberEnabled = true
		s.RPCAllowedHeadGroups = encodeRPCHeadGroups([]string{testAdminGroupID})
	})
	f.binary(srv.ID, clusterRPCBinary, BinaryKindRPCServer, clusterCommit)
	return srv
}

func (f *clusterFixture) updateServer(id string, mutate func(*routing.AIServer)) {
	f.t.Helper()
	s, err := f.routes.AIServerByID(f.ctx, id)
	if err != nil {
		f.t.Fatalf("server %s: %v", id, err)
	}
	ip := s.NetbirdIP
	mutate(&s)
	if err := f.routes.UpdateAIServer(f.ctx, s); err != nil {
		f.t.Fatalf("update server: %v", err)
	}
	// The mesh IP is the peer sync's to write: UpdateAIServer leaves it.
	if s.NetbirdIP != ip {
		if err := f.routes.UpdateServerNetbirdState(f.ctx, id, s.Domain, s.NetbirdPeerID, s.NetbirdIP, s.NetbirdConnected); err != nil {
			f.t.Fatalf("update server mesh ip: %v", err)
		}
	}
}

func (f *clusterFixture) hardware(serverID, reportJSON string) {
	f.t.Helper()
	if err := f.routes.UpsertServerHardware(f.ctx, routing.ServerHardware{ServerID: serverID, CollectedAt: clusterTestNow, UpdatedAt: clusterTestNow, ReportJSON: reportJSON}); err != nil {
		f.t.Fatalf("hardware: %v", err)
	}
}

func (f *clusterFixture) netbirdReady(serverID, ip string) {
	f.updateServer(serverID, func(s *routing.AIServer) {
		s.NetbirdEnabled, s.NetbirdGroupID, s.NetbirdIP = true, "grp_"+serverID, ip
	})
}

func (f *clusterFixture) directMeshPolicies() {
	f.t.Helper()
	for k, v := range map[string]string{
		netbirdManagePoliciesKey:       "true",
		netbirdDenyByDefaultKey:        "true",
		netbirdDenyByDefaultEnforceKey: "true",
		netbirdDefaultPolicyStateKey:   `{"state":"disabled","at":"2026-11-05T11:00:00Z"}`,
	} {
		if err := f.settings.SetSystemSetting(f.ctx, k, v, clusterTestNow); err != nil {
			f.t.Fatalf("setting %s: %v", k, err)
		}
	}
}

// manualSpec creates a manual member spec on serverID through the service.
func (f *clusterFixture) manualSpec(serverID string, gpus ...RuntimeSpecGPUDTO) RPCMemberSpecDTO {
	f.t.Helper()
	spec, err := f.svc.PutRPCMemberSpec(f.ctx, ownerToken(), serverID, "", PutRPCMemberSpecRequest{Binary: clusterRPCBinary, GPUs: gpus})
	if err != nil {
		f.t.Fatalf("manual spec: %v", err)
	}
	return spec
}

func (f *clusterFixture) put(p auth.Token, req PutRuntimeSpecRequest) (RuntimeSpecDTO, error) {
	f.mu.Lock()
	f.notified = nil
	f.mu.Unlock()
	return f.svc.PutRuntimeSpec(f.ctx, p, f.mappingID, req)
}

func (f *clusterFixture) mustPut(req PutRuntimeSpecRequest) RuntimeSpecDTO {
	f.t.Helper()
	dto, err := f.put(ownerToken(), req)
	if err != nil {
		f.t.Fatalf("PutRuntimeSpec: %v", err)
	}
	return dto
}

func (f *clusterFixture) notifiedServers() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := slices.Clone(f.notified)
	slices.Sort(out)
	return slices.Compact(out)
}

// clusterHeadRequest is a valid lan + relay head: no NetBird and no
// acknowledgement needed.
func clusterHeadRequest(members ...ClusterMemberDTO) PutRuntimeSpecRequest {
	return PutRuntimeSpecRequest{
		Enabled: true, Binary: clusterHeadBinary, Type: "llama_cpp",
		Args:                  []string{"-m", "/models/big.gguf", "--rpc", "${RPC_SERVERS}"},
		StartupTimeoutSeconds: 600,
		ClusterEnabled:        true, ClusterNetwork: "lan", RPCTransport: "relay",
		ClusterMembers: members,
	}
}

func autoMember(serverID string, gpus ...int) ClusterMemberDTO {
	m := ClusterMemberDTO{ServerID: serverID, GPUs: []ClusterMemberGPUDTO{}}
	for _, g := range gpus {
		m.GPUs = append(m.GPUs, ClusterMemberGPUDTO{Index: g, VRAMEstimateMB: 8000})
	}
	return m
}

func TestPutClusterHeadStoresMembership(t *testing.T) {
	f := newClusterFixture(t)
	a, b := f.member("A"), f.member("B")
	req := clusterHeadRequest(autoMember(a.ID, 1, 0), autoMember(b.ID))
	req.ClusterMembers[0].Threads, req.ClusterMembers[0].CacheEnabled = 8, true
	dto := f.mustPut(req)
	if !dto.ClusterEnabled || dto.MemberMode != "automatic" || dto.ClusterNetwork != "lan" || dto.RPCTransport != "relay" || dto.DeviceBackend != "auto" {
		t.Fatalf("cluster columns = %+v", dto)
	}
	members, gpus, err := f.routes.ClusterMembers(f.ctx, dto.ID)
	if err != nil || len(members) != 2 {
		t.Fatalf("members = %+v err=%v", members, err)
	}
	if members[0].ServerID != a.ID || members[1].ServerID != b.ID || members[0].Threads != 8 || !members[0].CacheEnabled || members[0].DeviceBackend != "auto" {
		t.Fatalf("member rows = %+v", members)
	}
	if members[0].Port < rpcPortFirst || members[0].Port > rpcPortLast || members[1].Port < rpcPortFirst {
		t.Fatalf("ports = %d, %d", members[0].Port, members[1].Port)
	}
	if len(gpus) != 2 || gpus[0].GPUIndex != 1 || gpus[0].Position != 0 || gpus[1].GPUIndex != 0 || gpus[0].VRAMEstimateMB != 8000 {
		t.Fatalf("gpu rows = %+v", gpus)
	}
	if got, want := f.notifiedServers(), sortedIDs(f.head.ID, a.ID, b.ID); !slices.Equal(got, want) {
		t.Fatalf("notified = %v, want %v", got, want)
	}
	if len(dto.ClusterMembers) != 2 || !dto.ClusterMembers[0].Visible || dto.ClusterMembers[0].ServerName != "A" || len(dto.ClusterMembers[0].GPUs) != 2 {
		t.Fatalf("dto members = %+v", dto.ClusterMembers)
	}
	got, err := f.svc.GetRuntimeSpec(f.ctx, ownerToken(), f.mappingID)
	if err != nil || len(got.ClusterMembers) != 2 || got.ClusterMembers[1].ServerID != b.ID || got.ClusterMembers[1].Port != members[1].Port {
		t.Fatalf("GET members = %+v err=%v", got.ClusterMembers, err)
	}
}

func sortedIDs(ids ...string) []string {
	out := slices.Clone(ids)
	slices.Sort(out)
	return out
}

// TestPutClusterSaveChecks pins one refusal per save code, in the order the
// checks run.
func TestPutClusterSaveChecks(t *testing.T) {
	type setup func(f *clusterFixture) (auth.Token, PutRuntimeSpecRequest)
	ok := func(f *clusterFixture) (auth.Token, PutRuntimeSpecRequest) {
		return ownerToken(), clusterHeadRequest(autoMember(f.member("A").ID))
	}
	with := func(mut func(f *clusterFixture, req *PutRuntimeSpecRequest)) setup {
		return func(f *clusterFixture) (auth.Token, PutRuntimeSpecRequest) {
			p, req := ok(f)
			mut(f, &req)
			return p, req
		}
	}
	args := func(a ...string) func(*clusterFixture, *PutRuntimeSpecRequest) {
		return func(_ *clusterFixture, req *PutRuntimeSpecRequest) {
			req.Args = append([]string{"-m", "/m.gguf"}, a...)
		}
	}
	cases := []struct {
		name  string
		setup setup
		want  error
	}{
		{"valid head", ok, nil},
		{"member mode invalid", with(func(_ *clusterFixture, r *PutRuntimeSpecRequest) { r.MemberMode = "both" }), ErrClusterMemberModeInvalid},
		{"network invalid", with(func(_ *clusterFixture, r *PutRuntimeSpecRequest) { r.ClusterNetwork = "wan" }), ErrClusterNetworkInvalid},
		{"transport invalid", with(func(_ *clusterFixture, r *PutRuntimeSpecRequest) { r.RPCTransport = "tunnel" }), ErrClusterTransportInvalid},
		{"device backend invalid", with(func(_ *clusterFixture, r *PutRuntimeSpecRequest) { r.DeviceBackend = "musa" }), ErrRPCMemberDeviceBackendInvalid},
		{"type not llama_cpp", with(func(_ *clusterFixture, r *PutRuntimeSpecRequest) { r.Type = "vllm" }), ErrClusterTypeInvalid},
		{"auto-detected llama-server is llama_cpp", with(func(_ *clusterFixture, r *PutRuntimeSpecRequest) { r.Type = "" }), nil},
		{"--rpc= form", with(args("--rpc=${RPC_SERVERS}")), ErrClusterFlagEqualsForm},
		{"--device= form", with(args("--rpc", "${RPC_SERVERS}", "--device=${DEVICES}")), ErrClusterFlagEqualsForm},
		{"-dev= form", with(args("--rpc", "${RPC_SERVERS}", "-dev=${DEVICES}")), ErrClusterFlagEqualsForm},
		{"a flag-like value is no flag", with(args("--alias", "-dev", "--rpc", "${RPC_SERVERS}")), nil},
		{"rpc placeholder missing", with(args()), ErrClusterRPCPlaceholderMissing},
		{"rpc with a literal address", with(args("--rpc", "10.0.0.1:50052")), ErrClusterRPCPlaceholderMissing},
		{"placeholder outside --rpc", with(args("--alias", "${RPC_SERVERS}")), ErrClusterRPCPlaceholderMissing},
		{"rpc in env", with(func(_ *clusterFixture, r *PutRuntimeSpecRequest) {
			r.Args = []string{"-m", "/m.gguf"}
			r.Env = map[string]string{"LLAMA_ARG_RPC": "${RPC_SERVERS}"}
		}), nil},
		{"rpc placeholder twice", with(func(_ *clusterFixture, r *PutRuntimeSpecRequest) {
			r.Env = map[string]string{"LLAMA_ARG_RPC": "${RPC_SERVERS}"}
		}), ErrClusterRPCTwice},
		{"rpc in args and env is additive", with(func(_ *clusterFixture, r *PutRuntimeSpecRequest) {
			r.Env = map[string]string{"LLAMA_ARG_RPC": "10.0.0.9:50052"}
		}), ErrClusterRPCTwice},
		{"rpc flag twice in args", with(args("--rpc", "${RPC_SERVERS}", "--rpc", "10.0.0.9:50052")), ErrClusterRPCTwice},
		{"rpc servers without cluster", with(func(_ *clusterFixture, r *PutRuntimeSpecRequest) { r.ClusterEnabled = false }), ErrClusterRPCPlaceholderWithoutCluster},
		{"rpc devices without cluster", with(func(_ *clusterFixture, r *PutRuntimeSpecRequest) {
			r.ClusterEnabled = false
			r.Args = []string{"-m", "/m.gguf"}
			r.Env = map[string]string{"LLAMA_ARG_DEVICE": "${RPC_DEVICES}"}
		}), ErrClusterRPCPlaceholderWithoutCluster},
		{"device list without rpc devices", with(args("--rpc", "${RPC_SERVERS}", "--device", "CUDA0")), ErrClusterDeviceListMissingRPC},
		// The four argv/env mixes of llama-server's parse order: env first,
		// then argv left to right, and --device resolves names while parsed.
		{"argv rpc then argv device", with(args("--rpc", "${RPC_SERVERS}", "--device", "${DEVICES}")), nil},
		{"argv device then argv rpc", with(args("-dev", "${RPC_DEVICES},CUDA0", "--rpc", "${RPC_SERVERS}")), ErrClusterDeviceOrder},
		{"env rpc and env device", with(func(_ *clusterFixture, r *PutRuntimeSpecRequest) {
			r.Args = []string{"-m", "/m.gguf"}
			r.Env = map[string]string{"LLAMA_ARG_RPC": "${RPC_SERVERS}", "LLAMA_ARG_DEVICE": "${DEVICES}"}
		}), nil},
		{"env rpc and argv device", with(func(_ *clusterFixture, r *PutRuntimeSpecRequest) {
			r.Args = []string{"-m", "/m.gguf", "--device", "${DEVICES}"}
			r.Env = map[string]string{"LLAMA_ARG_RPC": "${RPC_SERVERS}"}
		}), nil},
		{"env device and argv rpc", with(func(_ *clusterFixture, r *PutRuntimeSpecRequest) {
			r.Env = map[string]string{"LLAMA_ARG_DEVICE": "${DEVICES}"}
		}), ErrClusterDeviceOrder},
		{"no members", with(func(_ *clusterFixture, r *PutRuntimeSpecRequest) { r.ClusterMembers = nil }), ErrClusterMembersRequired},
		{"replicable head", with(func(_ *clusterFixture, r *PutRuntimeSpecRequest) { r.Replicable = true }), ErrClusterHeadReplicable},
		{"no explicit startup timeout", with(func(_ *clusterFixture, r *PutRuntimeSpecRequest) { r.StartupTimeoutSeconds = 0 }), ErrClusterStartupTimeoutRequired},
		{"member settings negative", with(func(_ *clusterFixture, r *PutRuntimeSpecRequest) { r.ClusterMembers[0].Threads = -1 }), ErrRuntimeSpecTuningInvalid},
		{"member gpu twice", with(func(_ *clusterFixture, r *PutRuntimeSpecRequest) {
			r.ClusterMembers[0].GPUs = []ClusterMemberGPUDTO{{Index: 0}, {Index: 0}}
		}), ErrRuntimeSpecGPUInvalid},
		{"member device backend invalid", with(func(_ *clusterFixture, r *PutRuntimeSpecRequest) { r.ClusterMembers[0].DeviceBackend = "musa" }), ErrRPCMemberDeviceBackendInvalid},
		{"member server twice", with(func(_ *clusterFixture, r *PutRuntimeSpecRequest) {
			r.ClusterMembers = append(r.ClusterMembers, autoMember(r.ClusterMembers[0].ServerID))
		}), ErrClusterMemberServerTwice},
		{"member on the head server", with(func(f *clusterFixture, r *PutRuntimeSpecRequest) {
			r.ClusterMembers = []ClusterMemberDTO{autoMember(f.head.ID)}
		}), ErrClusterMemberOnHeadServer},
		{"member server unknown", with(func(_ *clusterFixture, r *PutRuntimeSpecRequest) { r.ClusterMembers[0].ServerID = "srv_ghost" }), ErrServerNotFound},
		{"member server not managed by the caller", with(func(f *clusterFixture, r *PutRuntimeSpecRequest) {
			foreign := f.foreignServer("F")
			r.ClusterMembers = []ClusterMemberDTO{autoMember(foreign.ID)}
		}), ErrServerNotFound},
		{"member server consents to another group only", with(func(f *clusterFixture, r *PutRuntimeSpecRequest) {
			f.updateServer(r.ClusterMembers[0].ServerID, func(s *routing.AIServer) {
				s.RPCAllowedHeadGroups = encodeRPCHeadGroups([]string{f.foreignGroup()})
			})
		}), ErrServerNotFound},
		// An empty consent list means the member server's own admin groups,
		// which here include the head server's.
		{"member server with an empty consent list", with(func(f *clusterFixture, r *PutRuntimeSpecRequest) {
			f.updateServer(r.ClusterMembers[0].ServerID, func(s *routing.AIServer) { s.RPCAllowedHeadGroups = "[]" })
		}), nil},
		{"member server with an empty consent list in another group", with(func(f *clusterFixture, r *PutRuntimeSpecRequest) {
			srv := r.ClusterMembers[0].ServerID
			f.updateServer(srv, func(s *routing.AIServer) { s.RPCAllowedHeadGroups = "" })
			// Still managed by ownerToken as its owner, but linked to a group
			// the head's server is not in.
			if _, err := f.svc.SetServerAdminGroups(f.ctx, systemToken(), srv, []string{f.foreignGroup()}); err != nil {
				t.Fatal(err)
			}
		}), ErrServerNotFound},
		{"manual spec missing", with(func(_ *clusterFixture, r *PutRuntimeSpecRequest) { r.MemberMode = "manual" }), ErrRPCMemberSpecNotFound},
		{"manual spec of another server", with(func(f *clusterFixture, r *PutRuntimeSpecRequest) {
			r.MemberMode = "manual"
			r.ClusterMembers[0].MemberSpecID = f.manualSpec(f.member("B").ID).ID
		}), ErrRPCMemberSpecNotFound},
		{"manual spec consenting to another group", with(func(f *clusterFixture, r *PutRuntimeSpecRequest) {
			r.MemberMode = "manual"
			spec, err := f.svc.PutRPCMemberSpec(f.ctx, ownerToken(), r.ClusterMembers[0].ServerID, "", PutRPCMemberSpecRequest{Binary: clusterRPCBinary, AllowedHeadGroups: &[]string{f.foreignGroup()}})
			if err != nil {
				t.Fatalf("spec: %v", err)
			}
			r.ClusterMembers[0].MemberSpecID = spec.ID
		}), ErrServerNotFound},
		{"manual spec with an empty consent list", with(func(f *clusterFixture, r *PutRuntimeSpecRequest) {
			r.MemberMode = "manual"
			spec, err := f.svc.PutRPCMemberSpec(f.ctx, ownerToken(), r.ClusterMembers[0].ServerID, "", PutRPCMemberSpecRequest{Binary: clusterRPCBinary, AllowedHeadGroups: &[]string{}})
			if err != nil {
				t.Fatalf("spec: %v", err)
			}
			r.ClusterMembers[0].MemberSpecID = spec.ID
		}), nil},
		{"manual spec taken by another head", with(func(f *clusterFixture, r *PutRuntimeSpecRequest) {
			r.MemberMode = "manual"
			spec := f.manualSpec(r.ClusterMembers[0].ServerID)
			r.ClusterMembers[0].MemberSpecID = spec.ID
			otherHead := seedOtherHead(t, f)
			if err := f.routes.SetClusterMembers(f.ctx, otherHead, []routing.ClusterMember{{Position: 0, ServerID: spec.ServerID, MemberSpecID: spec.ID, Port: 47500}}, nil); err != nil {
				t.Fatalf("other head: %v", err)
			}
		}), ErrClusterMemberSpecTaken},
		{"manual spec disabled", with(func(f *clusterFixture, r *PutRuntimeSpecRequest) {
			r.MemberMode = "manual"
			off := false
			spec, err := f.svc.PutRPCMemberSpec(f.ctx, ownerToken(), r.ClusterMembers[0].ServerID, "", PutRPCMemberSpecRequest{Binary: clusterRPCBinary, Enabled: &off})
			if err != nil {
				t.Fatalf("spec: %v", err)
			}
			r.ClusterMembers[0].MemberSpecID = spec.ID
		}), ErrClusterMemberSpecDisabled},
		{"manual spec usable", with(func(f *clusterFixture, r *PutRuntimeSpecRequest) {
			r.MemberMode = "manual"
			r.ClusterMembers[0].MemberSpecID = f.manualSpec(r.ClusterMembers[0].ServerID).ID
		}), nil},
		{"automatic member without rpc membership", with(func(f *clusterFixture, r *PutRuntimeSpecRequest) {
			f.updateServer(r.ClusterMembers[0].ServerID, func(s *routing.AIServer) { s.RPCMemberEnabled = false })
		}), ErrClusterMemberRPCDisabled},
		{"automatic member without a matching binary", with(func(f *clusterFixture, r *PutRuntimeSpecRequest) {
			srv := f.member("B")
			if err := f.routes.DeleteManualServerBinary(f.ctx, srv.ID, clusterRPCBinary); err != nil {
				t.Fatalf("delete binary: %v", err)
			}
			f.binary(srv.ID, clusterRPCBinary, BinaryKindRPCServer, "9999fff")
			r.ClusterMembers = []ClusterMemberDTO{autoMember(srv.ID)}
		}), ErrClusterMemberNoMatchingBinary},
		{"head binary version unknown", with(func(_ *clusterFixture, r *PutRuntimeSpecRequest) { r.Binary = "/usr/local/bin/llama-server" }), ErrClusterHeadBinaryVersionUnknown},
		{"head binary version unknown, manual members only", with(func(f *clusterFixture, r *PutRuntimeSpecRequest) {
			r.Binary, r.MemberMode = "/usr/local/bin/llama-server", "manual"
			spec, err := f.svc.PutRPCMemberSpec(f.ctx, ownerToken(), r.ClusterMembers[0].ServerID, "", PutRPCMemberSpecRequest{Binary: clusterRPCBinary})
			if err != nil {
				t.Fatalf("spec: %v", err)
			}
			r.ClusterMembers[0].MemberSpecID = spec.ID
		}), nil},
		{"manual member of another version", with(func(f *clusterFixture, r *PutRuntimeSpecRequest) {
			r.MemberMode = "manual"
			srv := r.ClusterMembers[0].ServerID
			f.binary(srv, "/opt/old/ggml-rpc-server", BinaryKindRPCServer, "1111aaa")
			spec, err := f.svc.PutRPCMemberSpec(f.ctx, ownerToken(), srv, "", PutRPCMemberSpecRequest{Binary: "/opt/old/ggml-rpc-server"})
			if err != nil {
				t.Fatalf("spec: %v", err)
			}
			r.ClusterMembers[0].MemberSpecID = spec.ID
		}), ErrClusterVersionMismatch},
		{"manual member of unknown version only warns", with(func(f *clusterFixture, r *PutRuntimeSpecRequest) {
			r.MemberMode = "manual"
			spec, err := f.svc.PutRPCMemberSpec(f.ctx, ownerToken(), r.ClusterMembers[0].ServerID, "", PutRPCMemberSpecRequest{Binary: "/opt/unknown/ggml-rpc-server"})
			if err != nil {
				t.Fatalf("spec: %v", err)
			}
			r.ClusterMembers[0].MemberSpecID = spec.ID
		}), nil},
		{"mesh without NetBird on a member", with(func(f *clusterFixture, r *PutRuntimeSpecRequest) {
			f.netbirdReady(f.head.ID, "100.64.0.1")
			r.ClusterNetwork = "mesh"
		}), ErrClusterMeshUnavailable},
		{"mesh with an unknown mesh IP", with(func(f *clusterFixture, r *PutRuntimeSpecRequest) {
			f.netbirdReady(f.head.ID, "100.64.0.1")
			f.netbirdReady(r.ClusterMembers[0].ServerID, "")
			r.ClusterNetwork = "mesh"
		}), ErrClusterMeshUnavailable},
		{"mesh relay", with(func(f *clusterFixture, r *PutRuntimeSpecRequest) {
			f.netbirdReady(f.head.ID, "100.64.0.1")
			f.netbirdReady(r.ClusterMembers[0].ServerID, "100.64.0.2")
			r.ClusterNetwork = "mesh"
		}), nil},
		{"lan with a NetBird member and no lan address", with(func(f *clusterFixture, r *PutRuntimeSpecRequest) {
			f.netbirdReady(r.ClusterMembers[0].ServerID, "100.64.0.2")
		}), ErrClusterLANAddressRequired},
		{"lan with a NetBird member and a lan address", with(func(f *clusterFixture, r *PutRuntimeSpecRequest) {
			f.netbirdReady(r.ClusterMembers[0].ServerID, "100.64.0.2")
			f.updateServer(r.ClusterMembers[0].ServerID, func(s *routing.AIServer) { s.LANAddress = "192.0.2.10" })
		}), nil},
		{"direct mesh without policy enforcement", with(func(f *clusterFixture, r *PutRuntimeSpecRequest) {
			f.netbirdReady(f.head.ID, "100.64.0.1")
			f.netbirdReady(r.ClusterMembers[0].ServerID, "100.64.0.2")
			r.ClusterNetwork, r.RPCTransport = "mesh", "direct"
		}), ErrClusterDirectUnavailable},
		{"direct mesh with Default last seen enabled", with(func(f *clusterFixture, r *PutRuntimeSpecRequest) {
			f.netbirdReady(f.head.ID, "100.64.0.1")
			f.netbirdReady(r.ClusterMembers[0].ServerID, "100.64.0.2")
			f.directMeshPolicies()
			if err := f.settings.SetSystemSetting(f.ctx, netbirdDefaultPolicyStateKey, `{"state":"enabled"}`, clusterTestNow); err != nil {
				t.Fatal(err)
			}
			r.ClusterNetwork, r.RPCTransport = "mesh", "direct"
		}), ErrClusterDirectUnavailable},
		{"direct mesh with every precondition", with(func(f *clusterFixture, r *PutRuntimeSpecRequest) {
			f.netbirdReady(f.head.ID, "100.64.0.1")
			f.netbirdReady(r.ClusterMembers[0].ServerID, "100.64.0.2")
			f.directMeshPolicies()
			r.ClusterNetwork, r.RPCTransport = "mesh", "direct"
		}), nil},
		{"direct lan without acknowledgement", with(func(_ *clusterFixture, r *PutRuntimeSpecRequest) { r.RPCTransport = "direct" }), ErrClusterLANAckRequired},
		{"direct lan with acknowledgement", with(func(_ *clusterFixture, r *PutRuntimeSpecRequest) {
			r.RPCTransport, r.LANExposureAck = "direct", true
		}), nil},
		{"apple member without a gpu budget", with(func(f *clusterFixture, r *PutRuntimeSpecRequest) {
			f.hardware(r.ClusterMembers[0].ServerID, `{"os":"darwin","gpus":[{"index":0,"name":"Apple M2 Ultra"}]}`)
			r.ClusterMembers[0].GPUs = []ClusterMemberGPUDTO{{Index: 0, VRAMEstimateMB: 60000}}
			r.Args = append(r.Args, "--tensor-split", "1,1")
		}), ErrClusterMemberBudgetRequired},
		{"apple member with a gpu budget", with(func(f *clusterFixture, r *PutRuntimeSpecRequest) {
			srv := r.ClusterMembers[0].ServerID
			f.hardware(srv, `{"os":"darwin","gpus":[{"index":0,"name":"Apple M2 Ultra"}]}`)
			if err := f.routes.SetServerGPUBudgets(f.ctx, srv, []routing.ServerGPUBudget{{ServerID: srv, GPUIndex: 0, BudgetMB: 90000}}); err != nil {
				t.Fatal(err)
			}
			r.ClusterMembers[0].GPUs = []ClusterMemberGPUDTO{{Index: 0, VRAMEstimateMB: 60000}}
			r.Args = append(r.Args, "--tensor-split", "1,1")
		}), nil},
		{"non-NVIDIA member without an estimate", with(func(f *clusterFixture, r *PutRuntimeSpecRequest) {
			f.hardware(r.ClusterMembers[0].ServerID, `{"os":"linux","gpus":[{"index":0,"name":"AMD Radeon PRO W7900"}]}`)
			r.ClusterMembers[0].GPUs = []ClusterMemberGPUDTO{{Index: 0}}
			r.Args = append(r.Args, "--tensor-split", "1,1")
		}), ErrRPCMemberVRAMEstimateRequired},
		{"non-NVIDIA member without --tensor-split", with(func(f *clusterFixture, r *PutRuntimeSpecRequest) {
			f.hardware(r.ClusterMembers[0].ServerID, `{"os":"linux","gpus":[{"index":0,"name":"AMD Radeon PRO W7900"}]}`)
			r.ClusterMembers[0].GPUs = []ClusterMemberGPUDTO{{Index: 0, VRAMEstimateMB: 40000}}
		}), ErrClusterTensorSplitRequired},
		{"non-NVIDIA member with LLAMA_ARG_TENSOR_SPLIT", with(func(f *clusterFixture, r *PutRuntimeSpecRequest) {
			f.hardware(r.ClusterMembers[0].ServerID, `{"os":"linux","gpus":[{"index":0,"name":"AMD Radeon PRO W7900"}]}`)
			r.ClusterMembers[0].GPUs = []ClusterMemberGPUDTO{{Index: 0, VRAMEstimateMB: 40000}}
			r.Env = map[string]string{"LLAMA_ARG_TENSOR_SPLIT": "1,1"}
		}), nil},
		{"NVIDIA member needs neither", with(func(f *clusterFixture, r *PutRuntimeSpecRequest) {
			f.hardware(r.ClusterMembers[0].ServerID, `{"os":"linux","gpus":[{"index":0,"name":"NVIDIA GeForce RTX 4090","pci_bus_id":"00000000:01:00.0"}]}`)
			r.ClusterMembers[0].GPUs = []ClusterMemberGPUDTO{{Index: 0}}
		}), nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newClusterFixture(t)
			p, req := tc.setup(f)
			_, err := f.put(p, req)
			if tc.want == nil {
				if err != nil {
					t.Fatalf("PutRuntimeSpec = %v, want success", err)
				}
				return
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("PutRuntimeSpec = %v, want %v", err, tc.want)
			}
			if spec, ok, _ := f.routes.RuntimeSpecByMapping(f.ctx, f.mappingID); ok {
				t.Fatalf("a refused write stored the spec: %+v", spec)
			}
		})
	}
}

// seedOtherHead stores a second cluster head on its own server and returns
// its spec id.
func seedOtherHead(t *testing.T, f *clusterFixture) string {
	t.Helper()
	srv := createTestServer(t, f.svc, "Other head", "other-head.example.test")
	app := seedServerAgentApplication(t, f.routes, srv.ID, clusterTestNow)
	mapping, err := f.svc.CreateMapping(f.ctx, ownerToken(), app.ID, CreateMappingRequest{GatewayModelName: "other", AppModelName: "other"})
	if err != nil {
		t.Fatalf("mapping: %v", err)
	}
	id := "rspec_other_" + compactRandomHex(4)
	if err := f.routes.UpsertRuntimeSpec(f.ctx, routing.RuntimeSpec{
		ID: id, MappingID: mapping.ID, Enabled: true, Binary: clusterHeadBinary, Args: "[]", Env: "{}",
		ClusterEnabled: true, CreatedAt: clusterTestNow, UpdatedAt: clusterTestNow,
	}); err != nil {
		t.Fatalf("other head: %v", err)
	}
	return id
}

func TestPutClusterHeadVersionUnknownMessage(t *testing.T) {
	f := newClusterFixture(t)
	req := clusterHeadRequest(autoMember(f.member("A").ID))
	req.Binary = "/usr/local/bin/llama-server"
	_, err := f.put(ownerToken(), req)
	if !errors.Is(err, ErrClusterHeadBinaryVersionUnknown) || !strings.Contains(err.Error(), "Binaries tab") {
		t.Fatalf("err = %v, want the head-version-unknown refusal naming the Binaries tab", err)
	}
}

// TestPutClusterHeadVersionUnknownIsTheLastRefusal: the newest save code
// runs after every other check, so a body that also fails an older one
// keeps that answer.
func TestPutClusterHeadVersionUnknownIsTheLastRefusal(t *testing.T) {
	f := newClusterFixture(t)
	a := f.member("A")
	other := seedOtherHead(t, f)
	if err := f.routes.SetClusterMembers(f.ctx, other, []routing.ClusterMember{{Position: 0, ServerID: f.head.ID, Port: 26500}}, nil); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		mutate func(*PutRuntimeSpecRequest)
		want   error
	}{
		"direct lan without acknowledgement": {func(r *PutRuntimeSpecRequest) { r.RPCTransport = "direct" }, ErrClusterLANAckRequired},
		"listen port of a member":            {func(r *PutRuntimeSpecRequest) { r.ListenPort = 26500 }, ErrRuntimeSpecPortConflictsRPCMember},
	} {
		req := clusterHeadRequest(autoMember(a.ID))
		req.Binary = "/usr/local/bin/llama-server"
		tc.mutate(&req)
		if _, err := f.put(ownerToken(), req); !errors.Is(err, tc.want) {
			t.Errorf("%s with an unknown head version = %v, want %v", name, err, tc.want)
		}
	}
}

func TestPutClusterDefaultsNetworkAndTransport(t *testing.T) {
	t.Run("mesh-ready without policy enforcement is mesh + relay", func(t *testing.T) {
		f := newClusterFixture(t)
		a := f.member("A")
		f.netbirdReady(f.head.ID, "100.64.0.1")
		f.netbirdReady(a.ID, "100.64.0.2")
		req := clusterHeadRequest(autoMember(a.ID))
		req.ClusterNetwork, req.RPCTransport = "", ""
		dto := f.mustPut(req)
		if dto.ClusterNetwork != "mesh" || dto.RPCTransport != "relay" {
			t.Fatalf("network/transport = %q/%q, want mesh/relay", dto.ClusterNetwork, dto.RPCTransport)
		}
		if dto.ClusterNetworkDefault != "mesh" || dto.ClusterTransportDefault != "relay" {
			t.Fatalf("defaults echo = %q/%q", dto.ClusterNetworkDefault, dto.ClusterTransportDefault)
		}
		if !slices.Contains(dto.ClusterWarnings, ClusterWarnRelayMeshDenyOff) {
			t.Fatalf("warnings = %v, want %s", dto.ClusterWarnings, ClusterWarnRelayMeshDenyOff)
		}
	})
	t.Run("mesh-ready with direct preconditions is mesh + direct", func(t *testing.T) {
		f := newClusterFixture(t)
		a := f.member("A")
		f.netbirdReady(f.head.ID, "100.64.0.1")
		f.netbirdReady(a.ID, "100.64.0.2")
		f.directMeshPolicies()
		req := clusterHeadRequest(autoMember(a.ID))
		req.ClusterNetwork, req.RPCTransport = "", ""
		if dto := f.mustPut(req); dto.ClusterNetwork != "mesh" || dto.RPCTransport != "direct" {
			t.Fatalf("network/transport = %q/%q, want mesh/direct", dto.ClusterNetwork, dto.RPCTransport)
		}
	})
	t.Run("a member without NetBird makes it lan + direct", func(t *testing.T) {
		f := newClusterFixture(t)
		a := f.member("A")
		f.netbirdReady(f.head.ID, "100.64.0.1")
		f.updateServer(f.head.ID, func(s *routing.AIServer) { s.LANAddress = "192.0.2.1" })
		req := clusterHeadRequest(autoMember(a.ID))
		req.ClusterNetwork, req.RPCTransport = "", ""
		if _, err := f.put(ownerToken(), req); !errors.Is(err, ErrClusterLANAckRequired) {
			t.Fatalf("err = %v, want the lan acknowledgement the direct default needs", err)
		}
		req.LANExposureAck = true
		if dto := f.mustPut(req); dto.ClusterNetwork != "lan" || dto.RPCTransport != "direct" {
			t.Fatalf("network/transport = %q/%q, want lan/direct", dto.ClusterNetwork, dto.RPCTransport)
		}
	})
}

// usage writes agent-measured values onto a membership row.
func (f *clusterFixture) usage(headID, serverID string, ramMB int, vram map[int]int) {
	f.t.Helper()
	gpus := make([]routing.ClusterMemberGPU, 0, len(vram))
	for idx, mb := range vram {
		gpus = append(gpus, routing.ClusterMemberGPU{GPUIndex: idx, VRAMMeasuredMB: mb, RunPeakVRAMMB: mb})
	}
	members, _ := membersByServer(f.t, f, headID)
	if _, err := f.routes.UpdateClusterMemberUsage(f.ctx, routing.ClusterMember{
		HeadSpecID: headID, ServerID: serverID, RAMMeasuredMB: ramMB, MeasuredAt: clusterTestNow, RunPeakRAMMB: ramMB,
		RunStartedAt: members[serverID].RunStartedAt,
	}, gpus); err != nil {
		f.t.Fatalf("usage: %v", err)
	}
}

func membersByServer(t *testing.T, f *clusterFixture, headID string) (map[string]routing.ClusterMember, map[string]map[int]routing.ClusterMemberGPU) {
	t.Helper()
	members, gpus, err := f.routes.ClusterMembers(f.ctx, headID)
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]routing.ClusterMember{}
	for _, row := range members {
		m[row.ServerID] = row
	}
	g := map[string]map[int]routing.ClusterMemberGPU{}
	for _, row := range gpus {
		if g[row.ServerID] == nil {
			g[row.ServerID] = map[int]routing.ClusterMemberGPU{}
		}
		g[row.ServerID][row.GPUIndex] = row
	}
	return m, g
}

// TestPutClusterReorderNotifiesAndMarksStale: a reorder of a running
// cluster's members stops nothing itself; it is pushed to the head server and
// every member server (the head's pushed spec changes, and its agent drains
// a running head on that change), keeps each member's port and measured
// values, and marks the kept values stale, since the RPC<n> numbering and
// the split changed.
func TestPutClusterReorderNotifiesAndMarksStale(t *testing.T) {
	f := newClusterFixture(t)
	a, b := f.member("A"), f.member("B")
	first := f.mustPut(clusterHeadRequest(autoMember(a.ID, 0), autoMember(b.ID, 0)))
	f.usage(first.ID, a.ID, 1000, map[int]int{0: 7000})
	f.usage(first.ID, b.ID, 2000, map[int]int{0: 9000})
	before, _ := membersByServer(t, f, first.ID)

	f.mustPut(clusterHeadRequest(autoMember(b.ID, 0), autoMember(a.ID, 0)))
	if got, want := f.notifiedServers(), sortedIDs(f.head.ID, a.ID, b.ID); !slices.Equal(got, want) {
		t.Fatalf("notified = %v, want head and both members %v", got, want)
	}
	after, gpus := membersByServer(t, f, first.ID)
	for _, id := range []string{a.ID, b.ID} {
		if after[id].Port != before[id].Port {
			t.Fatalf("member %s port moved %d -> %d", id, before[id].Port, after[id].Port)
		}
		if !after[id].Stale || after[id].RAMMeasuredMB != before[id].RAMMeasuredMB || gpus[id][0].VRAMMeasuredMB == 0 {
			t.Fatalf("member %s after reorder = %+v gpus %+v, want kept and stale", id, after[id], gpus[id])
		}
	}
	if after[b.ID].Position != 0 || after[a.ID].Position != 1 {
		t.Fatalf("positions = %d/%d", after[b.ID].Position, after[a.ID].Position)
	}
}

func TestPutClusterCarriesMeasuredValues(t *testing.T) {
	f := newClusterFixture(t)
	a, b := f.member("A"), f.member("B")
	req := clusterHeadRequest(autoMember(a.ID, 0, 1), autoMember(b.ID, 0))
	head := f.mustPut(req)
	f.usage(head.ID, a.ID, 1000, map[int]int{0: 7000, 1: 6000})
	f.usage(head.ID, b.ID, 2000, map[int]int{0: 9000})

	// A save that changes nothing of the distribution keeps everything fresh.
	req.IdleTimeoutSeconds = 900
	f.mustPut(req)
	rows, gpus := membersByServer(t, f, head.ID)
	if rows[a.ID].Stale || rows[a.ID].RAMMeasuredMB != 1000 || gpus[a.ID][1].VRAMMeasuredMB != 6000 || rows[a.ID].MeasuredAt.IsZero() {
		t.Fatalf("unchanged save must keep fresh values: %+v %+v", rows[a.ID], gpus[a.ID])
	}

	// Dropping member A's GPU 1 and removing member B: GPU 0 keeps its value
	// as a stale floor, GPU 1 and member B drop theirs.
	req.ClusterMembers = []ClusterMemberDTO{autoMember(a.ID, 0)}
	dto := f.mustPut(req)
	rows, gpus = membersByServer(t, f, head.ID)
	if _, ok := rows[b.ID]; ok {
		t.Fatalf("removed member B kept a row: %+v", rows[b.ID])
	}
	if !rows[a.ID].Stale || gpus[a.ID][0].VRAMMeasuredMB != 7000 || len(gpus[a.ID]) != 1 {
		t.Fatalf("member A = %+v gpus %+v, want GPU 0 kept stale, GPU 1 gone", rows[a.ID], gpus[a.ID])
	}
	if !slices.Contains(f.notifiedServers(), b.ID) {
		t.Fatalf("notified = %v, want the removed member's server too", f.notifiedServers())
	}
	m := dto.ClusterMembers[0]
	if !m.Stale || m.RAMMeasuredMB != 1000 || m.MeasuredAt == nil || m.GPUs[0].VRAMMeasuredMB != 7000 {
		t.Fatalf("dto member = %+v", m)
	}

	// A head args change marks kept values stale too; a re-added member
	// starts unmeasured.
	f.usage(head.ID, a.ID, 1000, map[int]int{0: 7000})
	if _, err := f.routes.UpdateClusterMemberUsage(f.ctx, routing.ClusterMember{HeadSpecID: head.ID, ServerID: a.ID, RAMMeasuredMB: 1000, MeasuredAt: clusterTestNow}, nil); err != nil {
		t.Fatal(err)
	}
	req.ClusterMembers = []ClusterMemberDTO{autoMember(a.ID, 0), autoMember(b.ID, 0)}
	req.Args = append(req.Args, "--ctx-size", "8192")
	f.mustPut(req)
	rows, gpus = membersByServer(t, f, head.ID)
	if !rows[a.ID].Stale || rows[b.ID].RAMMeasuredMB != 0 || gpus[b.ID][0].VRAMMeasuredMB != 0 || rows[b.ID].Stale {
		t.Fatalf("after args change: A=%+v B=%+v", rows[a.ID], rows[b.ID])
	}
}

func TestPutClusterOffClearsMembership(t *testing.T) {
	f := newClusterFixture(t)
	a := f.member("A")
	head := f.mustPut(clusterHeadRequest(autoMember(a.ID)))
	req := clusterHeadRequest()
	req.ClusterEnabled = false
	req.Args = []string{"-m", "/m.gguf"}
	dto := f.mustPut(req)
	if dto.ClusterEnabled {
		t.Fatal("cluster still enabled")
	}
	members, _, _ := f.routes.ClusterMembers(f.ctx, head.ID)
	if len(members) != 0 {
		t.Fatalf("members after cluster off = %+v", members)
	}
	if !slices.Contains(f.notifiedServers(), a.ID) {
		t.Fatalf("notified = %v, want the former member's server", f.notifiedServers())
	}
}

func TestBenchmarkWriteKeepsClusterConfiguration(t *testing.T) {
	f := newClusterFixture(t)
	a := f.member("A")
	head := f.mustPut(clusterHeadRequest(autoMember(a.ID, 0)))
	// The member stops offering RPC membership after the save: a benchmark
	// writer's full-document write must still go through and keep the cluster.
	f.updateServer(a.ID, func(s *routing.AIServer) { s.RPCMemberEnabled = false })
	got, err := f.svc.SetBenchmarkRuntimeSpecAdminState(f.ctx, head.ID, "", "force_stopped")
	if err != nil {
		t.Fatalf("benchmark write: %v", err)
	}
	if !got.ClusterEnabled || got.ClusterNetwork != "lan" || got.RPCTransport != "relay" {
		t.Fatalf("benchmark write changed the cluster columns: %+v", got)
	}
	if members, _, _ := f.routes.ClusterMembers(f.ctx, head.ID); len(members) != 1 {
		t.Fatalf("benchmark write changed the membership: %+v", members)
	}
}

func TestAllocateRPCPortSkipsTakenPorts(t *testing.T) {
	f := newClusterFixture(t)
	srv := f.member("A")
	seedApp(t, f.routes, "app_plain", srv.ID, 26000, routing.ServerStatusActive, clusterTestNow)
	plain, err := f.routes.ApplicationByID(f.ctx, "app_plain")
	if err != nil {
		t.Fatal(err)
	}
	plain.ProxyListenPort = 26001
	if err := f.routes.UpdateApplication(f.ctx, plain); err != nil {
		t.Fatalf("proxy port: %v", err)
	}
	agentApp := routing.Application{
		ID: "app_agent", ServerID: srv.ID, Type: routing.ProviderServerAgent, Port: 26002, Scheme: "http",
		Status: routing.ServerStatusActive, CreatedAt: clusterTestNow, UpdatedAt: clusterTestNow,
	}
	if err := f.routes.CreateApplication(f.ctx, agentApp); err != nil {
		t.Fatal(err)
	}
	mapping, err := f.svc.CreateMapping(f.ctx, ownerToken(), agentApp.ID, CreateMappingRequest{GatewayModelName: "pinned", AppModelName: "pinned"})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.routes.UpsertRuntimeSpec(f.ctx, routing.RuntimeSpec{
		ID: "rspec_pinned", MappingID: mapping.ID, Binary: "/bin/x", Args: "[]", Env: "{}", ListenPort: 26003,
		CreatedAt: clusterTestNow, UpdatedAt: clusterTestNow,
	}); err != nil {
		t.Fatal(err)
	}
	other := seedOtherHead(t, f)
	if err := f.routes.SetClusterMembers(f.ctx, other, []routing.ClusterMember{{Position: 0, ServerID: srv.ID, Port: 26004}}, nil); err != nil {
		t.Fatal(err)
	}
	port, err := f.svc.allocateRPCPort(f.ctx, srv.ID)
	if err != nil || port != 26005 {
		t.Fatalf("allocateRPCPort = %d, %v; want 26005", port, err)
	}
	// Another server's ports do not count.
	if port, err := f.svc.allocateRPCPort(f.ctx, f.head.ID); err != nil || port != rpcPortFirst {
		t.Fatalf("allocateRPCPort(head) = %d, %v; want %d", port, err, rpcPortFirst)
	}
	// A full range is refused.
	for p := rpcPortFirst; p <= rpcPortLast; p++ {
		seedApp(t, f.routes, fmt.Sprintf("app_fill_%d", p), f.head.ID, p, routing.ServerStatusActive, clusterTestNow)
	}
	if _, err := f.svc.allocateRPCPort(f.ctx, f.head.ID); !errors.Is(err, ErrClusterPortExhausted) {
		t.Fatalf("full range: err = %v, want %v", err, ErrClusterPortExhausted)
	}
}

func TestClusterCandidatesFiltersByAuthAndConsent(t *testing.T) {
	f := newClusterFixture(t)
	f.mustPut(PutRuntimeSpecRequest{Enabled: true, Binary: clusterHeadBinary, Args: []string{"-m", "/m.gguf"}})
	good := f.member("Good")
	noConsent := f.member("NoConsent")
	f.updateServer(noConsent.ID, func(s *routing.AIServer) { s.RPCAllowedHeadGroups = encodeRPCHeadGroups([]string{f.foreignGroup()}) })
	disabled := f.member("Disabled")
	f.updateServer(disabled.ID, func(s *routing.AIServer) { s.RPCMemberEnabled = false })
	wrongVersion := f.member("WrongVersion")
	if err := f.routes.DeleteManualServerBinary(f.ctx, wrongVersion.ID, clusterRPCBinary); err != nil {
		t.Fatal(err)
	}
	f.binary(wrongVersion.ID, clusterRPCBinary, BinaryKindRPCServer, "9999fff")
	f.foreignServer("Foreign")

	names := func(c []ClusterCandidateDTO) []string {
		out := []string{}
		for _, x := range c {
			out = append(out, x.ServerName)
		}
		return out
	}
	auto, err := f.svc.ClusterCandidates(f.ctx, ownerToken(), f.mappingID, "automatic")
	if err != nil {
		t.Fatal(err)
	}
	if got := names(auto); !slices.Equal(got, []string{"Good"}) {
		t.Fatalf("automatic candidates = %v, want [Good]", got)
	}
	if auto[0].ServerID != good.ID || auto[0].RPCBinary != clusterRPCBinary {
		t.Fatalf("candidate = %+v", auto[0])
	}
	// System scope manages every server, but consent binds it like anyone:
	// the server that does not consent to the head is not offered.
	sys, err := f.svc.ClusterCandidates(f.ctx, systemToken(), f.mappingID, "automatic")
	if err != nil {
		t.Fatal(err)
	}
	if got := names(sys); !slices.Equal(got, []string{"Foreign", "Good"}) {
		t.Fatalf("system candidates = %v", got)
	}

	// Manual: only free, enabled specs whose consent admits the head.
	free := f.manualSpec(good.ID)
	taken := f.manualSpec(good.ID)
	other := seedOtherHead(t, f)
	if err := f.routes.SetClusterMembers(f.ctx, other, []routing.ClusterMember{{Position: 0, ServerID: good.ID, MemberSpecID: taken.ID, Port: 47100}}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.PutRPCMemberSpec(f.ctx, ownerToken(), disabled.ID, "", PutRPCMemberSpecRequest{Binary: clusterRPCBinary, AllowedHeadGroups: &[]string{f.foreignGroup()}}); err != nil {
		t.Fatal(err)
	}
	manual, err := f.svc.ClusterCandidates(f.ctx, ownerToken(), f.mappingID, "manual")
	if err != nil {
		t.Fatal(err)
	}
	if len(manual) != 1 || manual[0].ServerID != good.ID || len(manual[0].MemberSpecs) != 1 || manual[0].MemberSpecs[0].ID != free.ID {
		t.Fatalf("manual candidates = %+v", manual)
	}
	if _, err := f.svc.ClusterCandidates(f.ctx, ownerToken(), f.mappingID, "sideways"); !errors.Is(err, ErrClusterMemberModeInvalid) {
		t.Fatalf("bad mode err = %v", err)
	}
	if _, err := f.svc.ClusterCandidates(f.ctx, otherToken(), f.mappingID, "automatic"); !errors.Is(err, ErrMappingNotFound) {
		t.Fatalf("foreign head err = %v, want mapping not found", err)
	}
}

func TestClusterForeignMemberHiddenInDTO(t *testing.T) {
	f := newClusterFixture(t)
	mine := f.member("Mine")
	foreign := f.foreignServer("Foreign")
	f.hardware(foreign.ID, `{"os":"linux","gpus":[{"index":0,"name":"NVIDIA RTX A6000","pci_bus_id":"0:1","memory_total_bytes":51539607552}]}`)
	if err := f.routes.SetServerGPUBudgets(f.ctx, foreign.ID, []routing.ServerGPUBudget{{ServerID: foreign.ID, GPUIndex: 0, BudgetMB: 20000}}); err != nil {
		t.Fatal(err)
	}
	req := clusterHeadRequest(autoMember(mine.ID, 0), autoMember(foreign.ID, 0))
	req.ClusterMembers[1].RAMEstimateMB = 4000
	req.RAMEstimateMB = 1000
	head, err := f.put(systemToken(), req)
	if err != nil {
		t.Fatalf("system save: %v", err)
	}
	got, err := f.svc.GetRuntimeSpec(f.ctx, ownerToken(), f.mappingID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.ClusterMembers) != 2 {
		t.Fatalf("members = %+v", got.ClusterMembers)
	}
	hidden := got.ClusterMembers[1]
	if hidden.Visible || hidden.ServerID != "" || hidden.ServerName != "" || hidden.Port != 0 || len(hidden.GPUs) != 0 || hidden.Position != 1 {
		t.Fatalf("foreign member leaks: %+v", hidden)
	}
	if !got.ClusterMembers[0].Visible || got.ClusterMembers[0].ServerName != "Mine" {
		t.Fatalf("own member = %+v", got.ClusterMembers[0])
	}
	// Totals still count the hidden member: head RAM 1000 + 0 + 4000; VRAM
	// 8000 per member GPU.
	if got.ClusterRAMTotalMB != 5000 || got.ClusterVRAMTotalMB != 16000 || got.ClusterTotalsComplete {
		t.Fatalf("totals = vram %d ram %d complete %v", got.ClusterVRAMTotalMB, got.ClusterRAMTotalMB, got.ClusterTotalsComplete)
	}
	sys, err := f.svc.GetRuntimeSpec(f.ctx, systemToken(), f.mappingID)
	if err != nil || !sys.ClusterMembers[1].Visible || sys.ClusterMembers[1].ServerID != foreign.ID {
		t.Fatalf("system view = %+v err=%v", sys.ClusterMembers, err)
	}
	// The hidden member's server says nothing through the warnings or the
	// network default: the system view warns about its GPU budget, the
	// owner's does not, and the owner gets no network default.
	if !slices.Contains(sys.ClusterWarnings, ClusterWarnBudgetBelowDevice) || sys.ClusterNetworkDefault != "lan" {
		t.Fatalf("system view warnings %v default %q", sys.ClusterWarnings, sys.ClusterNetworkDefault)
	}
	if slices.Contains(got.ClusterWarnings, ClusterWarnBudgetBelowDevice) || got.ClusterNetworkDefault != "" {
		t.Fatalf("owner view leaks the hidden member: warnings %v default %q", got.ClusterWarnings, got.ClusterNetworkDefault)
	}
	// A save that leaves the membership as stored needs the head's rights
	// only, so the hidden member does not lock the owner out of the head.
	resave := putRequestFromDTO(got)
	resave.IdleTimeoutSeconds = 900
	if _, err := f.put(ownerToken(), resave); err != nil {
		t.Fatalf("owner re-save without membership change = %v", err)
	}
	if members, _, _ := f.routes.ClusterMembers(f.ctx, head.ID); len(members) != 2 || members[1].ServerID != foreign.ID {
		t.Fatalf("membership after the owner's re-save = %+v", members)
	}
	// A membership change still needs rights on every member server it
	// keeps: change the owner's own member, keep the hidden one.
	change := putRequestFromDTO(got)
	change.ClusterMembers[0].Threads = 4
	if _, err := f.put(ownerToken(), change); !errors.Is(err, ErrServerNotFound) {
		t.Fatalf("owner membership change = %v, want server not found", err)
	}
	// So does a transport change.
	change = putRequestFromDTO(got)
	change.RPCTransport = "direct"
	change.LANExposureAck = true
	if _, err := f.put(ownerToken(), change); !errors.Is(err, ErrServerNotFound) {
		t.Fatalf("owner transport change = %v, want server not found", err)
	}
}

func TestPutClusterWarnings(t *testing.T) {
	f := newClusterFixture(t)
	cpu := f.member("CPU")
	gpu := f.member("GPU")
	f.hardware(gpu.ID, `{"os":"linux","gpus":[{"index":0,"name":"NVIDIA RTX A6000","pci_bus_id":"0:1","memory_total_bytes":51539607552}]}`)
	if err := f.routes.SetServerGPUBudgets(f.ctx, gpu.ID, []routing.ServerGPUBudget{{ServerID: gpu.ID, GPUIndex: 0, BudgetMB: 20000}}); err != nil {
		t.Fatal(err)
	}
	// Another spec on the GPU member's server uses GPU 0: a shared host and an
	// overlapping selection.
	app := seedServerAgentApplication(t, f.routes, gpu.ID, clusterTestNow)
	mapping, err := f.svc.CreateMapping(f.ctx, ownerToken(), app.ID, CreateMappingRequest{GatewayModelName: "local", AppModelName: "local"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.PutRuntimeSpec(f.ctx, ownerToken(), mapping.ID, PutRuntimeSpecRequest{Enabled: true, Binary: "/bin/x", GPUs: []RuntimeSpecGPUDTO{{Index: 0}}}); err != nil {
		t.Fatal(err)
	}
	req := clusterHeadRequest(ClusterMemberDTO{ServerID: cpu.ID}, ClusterMemberDTO{ServerID: gpu.ID, GPUs: []ClusterMemberGPUDTO{{Index: 0}}})
	req.Args = []string{"-m", "/m.gguf", "--rpc", "${RPC_SERVERS}", "--device", "CUDA0,${RPC_DEVICES}"}
	dto := f.mustPut(req)
	for _, w := range []string{ClusterWarnRPCDevicesNotFirst, ClusterWarnCPUMemberWithoutTensorSplit, ClusterWarnSharedHostWithoutEstimate, ClusterWarnBudgetBelowDevice, ClusterWarnGPUOverlap} {
		if !slices.Contains(dto.ClusterWarnings, w) {
			t.Errorf("warnings = %v, missing %s", dto.ClusterWarnings, w)
		}
	}
	if slices.Contains(dto.ClusterWarnings, ClusterWarnMMDevMissing) {
		t.Errorf("warnings = %v: -mmdev hint only applies when the list starts with RPC devices", dto.ClusterWarnings)
	}
	req.Args = []string{"-m", "/m.gguf", "--rpc", "${RPC_SERVERS}", "--device", "${DEVICES}", "--tensor-split", "1,1"}
	dto = f.mustPut(req)
	if !slices.Contains(dto.ClusterWarnings, ClusterWarnMMDevMissing) {
		t.Errorf("warnings = %v, want %s", dto.ClusterWarnings, ClusterWarnMMDevMissing)
	}
	for _, w := range []string{ClusterWarnRPCDevicesNotFirst, ClusterWarnCPUMemberWithoutTensorSplit, ClusterWarnBudgetBelowDevice} {
		if slices.Contains(dto.ClusterWarnings, w) {
			t.Errorf("warnings = %v, %s must be gone", dto.ClusterWarnings, w)
		}
	}
	// A manual member of unknown version only warns.
	g := newClusterFixture(t)
	srv := g.member("M")
	spec, err := g.svc.PutRPCMemberSpec(g.ctx, ownerToken(), srv.ID, "", PutRPCMemberSpecRequest{Binary: "/opt/unknown/ggml-rpc-server"})
	if err != nil {
		t.Fatal(err)
	}
	manual := clusterHeadRequest(ClusterMemberDTO{ServerID: srv.ID, MemberSpecID: spec.ID})
	manual.MemberMode = "manual"
	if dto := g.mustPut(manual); !slices.Contains(dto.ClusterWarnings, ClusterWarnMemberVersionUnknown) {
		t.Fatalf("warnings = %v, want %s", dto.ClusterWarnings, ClusterWarnMemberVersionUnknown)
	}
}

func TestNetbirdDefaultPolicyDisabled(t *testing.T) {
	f := newClusterFixture(t)
	for raw, want := range map[string]bool{
		"": false,
		`{"state":"disabled","at":"2026-11-05T11:00:00Z"}`: true,
		`{"state":"enabled"}`:                              false,
		`{"state":"unknown"}`:                              false,
		"disabled":                                         true,
		"not json and garbage":                             false,
	} {
		if err := f.settings.SetSystemSetting(f.ctx, netbirdDefaultPolicyStateKey, raw, clusterTestNow); err != nil {
			t.Fatal(err)
		}
		if got := f.svc.netbirdDefaultPolicyDisabled(f.ctx); got != want {
			t.Errorf("state %q: disabled = %v, want %v", raw, got, want)
		}
	}
	f.svc.settings = nil
	if f.svc.netbirdDefaultPolicyDisabled(f.ctx) {
		t.Error("no settings store must read as not disabled")
	}
}

func TestPutClusterManualMemberDTOUsesSpec(t *testing.T) {
	f := newClusterFixture(t)
	srv := f.member("M")
	spec := f.manualSpec(srv.ID, RuntimeSpecGPUDTO{Index: 1, VRAMEstimateMB: 12000})
	req := clusterHeadRequest(ClusterMemberDTO{ServerID: srv.ID, MemberSpecID: spec.ID})
	req.MemberMode = "manual"
	dto := f.mustPut(req)
	m := dto.ClusterMembers[0]
	if m.MemberSpecID != spec.ID || len(m.GPUs) != 1 || m.GPUs[0].Index != 1 || m.GPUs[0].VRAMEstimateMB != 12000 {
		t.Fatalf("manual member dto = %+v", m)
	}
	_, gpus, _ := f.routes.ClusterMembers(f.ctx, dto.ID)
	if len(gpus) != 1 || gpus[0].GPUIndex != 1 {
		t.Fatalf("manual member needs its GPU rows on the membership for usage: %+v", gpus)
	}
	// The spec is now in use by this head: re-saving keeps it, another head
	// cannot take it.
	f.mustPut(req)
	if err := f.svc.DeleteRPCMemberSpec(f.ctx, ownerToken(), srv.ID, spec.ID); !errors.Is(err, ErrRPCMemberInUse) {
		t.Fatalf("delete in-use spec = %v", err)
	}
}

func TestDeleteClusterHeadNotifiesMemberServers(t *testing.T) {
	f := newClusterFixture(t)
	a, b := f.member("A"), f.member("B")
	head := f.mustPut(clusterHeadRequest(autoMember(a.ID), autoMember(b.ID)))
	f.mu.Lock()
	f.notified = nil
	f.mu.Unlock()
	if err := f.svc.DeleteRuntimeSpec(f.ctx, ownerToken(), f.mappingID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if got, want := f.notifiedServers(), sortedIDs(f.head.ID, a.ID, b.ID); !slices.Equal(got, want) {
		t.Fatalf("notified = %v, want head and both members %v", got, want)
	}
	if members, _, _ := f.routes.ClusterMembers(f.ctx, head.ID); len(members) != 0 {
		t.Fatalf("membership survived the head: %+v", members)
	}
}

// TestPortsOnAMemberServerAvoidRPCPorts: a port a cluster member holds on its
// server is refused as a new fixed listen_port, application port or TLS
// proxy port there; an unchanged listen_port is never refused.
func TestPortsOnAMemberServerAvoidRPCPorts(t *testing.T) {
	f := newClusterFixture(t)
	a := f.member("A")
	head := f.mustPut(clusterHeadRequest(autoMember(a.ID)))
	members, _, _ := f.routes.ClusterMembers(f.ctx, head.ID)
	rpcPort := members[0].Port

	app := seedServerAgentApplication(t, f.routes, a.ID, clusterTestNow)
	mapping, err := f.svc.CreateMapping(f.ctx, ownerToken(), app.ID, CreateMappingRequest{GatewayModelName: "local", AppModelName: "local"})
	if err != nil {
		t.Fatal(err)
	}
	spec := PutRuntimeSpecRequest{Enabled: true, Binary: "/bin/x", ListenPort: rpcPort}
	if _, err := f.svc.PutRuntimeSpec(f.ctx, ownerToken(), mapping.ID, spec); !errors.Is(err, ErrRuntimeSpecPortConflictsRPCMember) {
		t.Fatalf("listen_port on an RPC port = %v, want %v", err, ErrRuntimeSpecPortConflictsRPCMember)
	}
	// A listen_port stored before the member took the port stays savable.
	spec.ListenPort = rpcPort + 1
	if _, err := f.svc.PutRuntimeSpec(f.ctx, ownerToken(), mapping.ID, spec); err != nil {
		t.Fatalf("free listen_port: %v", err)
	}
	if err := f.routes.SetClusterMembers(f.ctx, head.ID, []routing.ClusterMember{{Position: 0, ServerID: a.ID, Port: rpcPort + 1, DeviceBackend: "auto"}}, nil); err != nil {
		t.Fatal(err)
	}
	spec.IdleTimeoutSeconds = 60
	if _, err := f.svc.PutRuntimeSpec(f.ctx, ownerToken(), mapping.ID, spec); err != nil {
		t.Fatalf("unchanged listen_port refused: %v", err)
	}

	if _, err := f.svc.CreateApplication(f.ctx, ownerToken(), a.ID, CreateApplicationRequest{Type: routing.ProviderVLLM, Port: rpcPort + 1, Scheme: "http"}); !errors.Is(err, ErrApplicationConflict) {
		t.Fatalf("application port on an RPC port = %v, want %v", err, ErrApplicationConflict)
	}
	created, err := f.svc.CreateApplication(f.ctx, ownerToken(), a.ID, CreateApplicationRequest{Type: routing.ProviderVLLM, Port: 8000, Scheme: "http"})
	if err != nil {
		t.Fatal(err)
	}
	clash := rpcPort + 1
	if _, err := f.svc.UpdateApplication(f.ctx, ownerToken(), created.ID, UpdateApplicationRequest{Port: &clash}); !errors.Is(err, ErrApplicationConflict) {
		t.Fatalf("application port update onto an RPC port = %v, want %v", err, ErrApplicationConflict)
	}
	if _, err := f.svc.UpdateApplication(f.ctx, ownerToken(), created.ID, UpdateApplicationRequest{ProxyListenPort: &clash}); !errors.Is(err, ErrApplicationProxyListenPortConflict) {
		t.Fatalf("proxy port onto an RPC port = %v, want %v", err, ErrApplicationProxyListenPortConflict)
	}
}

// racingPortStore makes the first membership write of a head lose a race:
// another head takes the port the write allocated on the same server just
// before it, as a second gateway process would.
type racingPortStore struct {
	*routing.MemoryStore
	rival   string
	races   int
	written int
}

func (r *racingPortStore) SetClusterMembersIf(ctx context.Context, head string, expect []routing.ClusterMember, expectGPUs []routing.ClusterMemberGPU, members []routing.ClusterMember, gpus []routing.ClusterMemberGPU) (bool, error) {
	if head != r.rival && r.written < r.races && len(members) > 0 {
		r.written++
		if err := r.MemoryStore.SetClusterMembers(ctx, r.rival, []routing.ClusterMember{{Position: 0, ServerID: members[0].ServerID, Port: members[0].Port, DeviceBackend: "auto"}}, nil); err != nil {
			return false, err
		}
	}
	return r.MemoryStore.SetClusterMembersIf(ctx, head, expect, expectGPUs, members, gpus)
}

func TestPutClusterPortRace(t *testing.T) {
	for _, tc := range []struct {
		name  string
		races int
		want  error
	}{
		{"one lost race allocates again", 1, nil},
		{"a second lost race gives up", 2, ErrClusterPortExhausted},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newClusterFixture(t)
			a := f.member("A")
			rival := seedOtherHead(t, f)
			racing := &racingPortStore{MemoryStore: f.routes, rival: rival, races: tc.races}
			f.svc.routes = racing
			dto, err := f.put(ownerToken(), clusterHeadRequest(autoMember(a.ID)))
			if !errors.Is(err, tc.want) && (tc.want != nil || err != nil) {
				t.Fatalf("PutRuntimeSpec = %v, want %v", err, tc.want)
			}
			if tc.want != nil {
				// A refused membership stores no head.
				if _, ok, _ := f.routes.RuntimeSpecByMapping(f.ctx, f.mappingID); ok {
					t.Fatal("a new head was kept although its membership was refused")
				}
				return
			}
			members, _, _ := f.routes.ClusterMembers(f.ctx, dto.ID)
			rivals, _, _ := f.routes.ClusterMembers(f.ctx, rival)
			if len(members) != 1 || members[0].Port == rivals[0].Port || members[0].Port == 0 {
				t.Fatalf("ports: head %+v rival %+v", members, rivals)
			}
		})
	}
}

// TestPutClusterChecksRunAfterTheRowIsBuilt: the cluster checks are the
// newest refusals, so a body that also fails the row's own preparation (the
// API token seal on a keyless store) reports that first.
func TestPutClusterChecksRunAfterTheRowIsBuilt(t *testing.T) {
	svc, routes := newServerTestServiceWithCipher(t, clusterTestNow, nil, false)
	server := createTestServer(t, svc, "Head", "head.example.test")
	app := seedServerAgentApplication(t, routes, server.ID, clusterTestNow)
	mapping, err := svc.CreateMapping(context.Background(), ownerToken(), app.ID, CreateMappingRequest{GatewayModelName: "big", AppModelName: "big"})
	if err != nil {
		t.Fatal(err)
	}
	req := clusterHeadRequest() // no members: a cluster refusal
	req.APITokenMode = "random"
	req.Env = map[string]string{"LLAMA_API_KEY": "${API_TOKEN}"}
	if _, err := svc.PutRuntimeSpec(context.Background(), ownerToken(), mapping.ID, req); !errors.Is(err, capture.ErrKeyRequired) {
		t.Fatalf("err = %v, want the seal refusal before the cluster checks", err)
	}
}

func TestPutClusterFirstLocalDeviceOnAHeadWithoutGPUs(t *testing.T) {
	f := newClusterFixture(t)
	req := clusterHeadRequest(autoMember(f.member("A").ID))
	req.Args = []string{"-m", "/m.gguf", "--rpc", "${RPC_SERVERS}", "--device", "${FIRST_LOCAL_DEVICE},${RPC_DEVICES}", "-mmdev", "x"}
	if dto := f.mustPut(req); slices.Contains(dto.ClusterWarnings, ClusterWarnRPCDevicesNotFirst) {
		t.Fatalf("warnings = %v: ${FIRST_LOCAL_DEVICE} is RPC0 on a head without GPU rows", dto.ClusterWarnings)
	}
	req.GPUs = []RuntimeSpecGPUDTO{{Index: 0, VRAMEstimateMB: 1000}}
	if dto := f.mustPut(req); !slices.Contains(dto.ClusterWarnings, ClusterWarnRPCDevicesNotFirst) {
		t.Fatalf("warnings = %v, want %s with a local GPU first", dto.ClusterWarnings, ClusterWarnRPCDevicesNotFirst)
	}
}

func TestPutClusterUnchangedMembershipKeepsTheAcknowledgement(t *testing.T) {
	f := newClusterFixture(t)
	req := clusterHeadRequest(autoMember(f.member("A").ID))
	req.RPCTransport, req.LANExposureAck = "direct", true
	f.mustPut(req)
	req.LANExposureAck = false
	if _, err := f.put(ownerToken(), req); !errors.Is(err, ErrClusterLANAckRequired) {
		t.Fatalf("withdrawn acknowledgement = %v, want %v", err, ErrClusterLANAckRequired)
	}
}

// TestPutClusterBinaryChangeRunsMemberChecks: the head binary decides which
// member builds are compatible, so a save that switches it is no unchanged
// membership, even with the same members, mode, network and transport.
func TestPutClusterBinaryChangeRunsMemberChecks(t *testing.T) {
	t.Run("visible manual member of the old version is refused", func(t *testing.T) {
		f := newClusterFixture(t)
		srv := f.member("M")
		spec := f.manualSpec(srv.ID)
		req := clusterHeadRequest(ClusterMemberDTO{ServerID: srv.ID, MemberSpecID: spec.ID})
		req.MemberMode = "manual"
		f.mustPut(req)
		f.binary(f.head.ID, "/opt/new/llama-server", BinaryKindLlamaServer, "7777bbb")
		req.Binary = "/opt/new/llama-server"
		if _, err := f.put(ownerToken(), req); !errors.Is(err, ErrClusterVersionMismatch) {
			t.Fatalf("binary switch = %v, want %v", err, ErrClusterVersionMismatch)
		}
	})
	t.Run("a hidden member makes it a membership change", func(t *testing.T) {
		f := newClusterFixture(t)
		mine, foreign := f.member("Mine"), f.foreignServer("Foreign")
		f.binary(f.head.ID, "/opt/other/llama-server", BinaryKindLlamaServer, clusterCommit)
		if _, err := f.put(systemToken(), clusterHeadRequest(autoMember(mine.ID), autoMember(foreign.ID))); err != nil {
			t.Fatalf("system save: %v", err)
		}
		got, err := f.svc.GetRuntimeSpec(f.ctx, ownerToken(), f.mappingID)
		if err != nil {
			t.Fatal(err)
		}
		req := putRequestFromDTO(got)
		if _, err := f.put(ownerToken(), req); err != nil {
			t.Fatalf("unchanged binary keeps the shortcut: %v", err)
		}
		// Same build at another path: still a switch the owner may not make
		// without rights on the hidden member's server.
		req.Binary = "/opt/other/llama-server"
		if _, err := f.put(ownerToken(), req); !errors.Is(err, ErrServerNotFound) {
			t.Fatalf("binary switch with a hidden member = %v, want %v", err, ErrServerNotFound)
		}
		if _, err := f.put(systemToken(), req); err != nil {
			t.Fatalf("system binary switch: %v", err)
		}
	})
}

// TestPutClusterHiddenMemberGPURefusalNamesNothing: in an unchanged
// membership the member GPU rules still hold for a hidden member, but the
// refusal carries no detail about it.
func TestPutClusterHiddenMemberGPURefusalNamesNothing(t *testing.T) {
	f := newClusterFixture(t)
	mine, foreign := f.member("Mine"), f.foreignServer("Foreign")
	f.hardware(foreign.ID, `{"os":"linux","gpus":[{"index":3,"name":"AMD Radeon PRO W7900"}]}`)
	req := clusterHeadRequest(autoMember(mine.ID), autoMember(foreign.ID, 3))
	req.Args = append(req.Args, "--tensor-split", "1,1")
	if _, err := f.put(systemToken(), req); err != nil {
		t.Fatalf("system save: %v", err)
	}
	got, err := f.svc.GetRuntimeSpec(f.ctx, ownerToken(), f.mappingID)
	if err != nil {
		t.Fatal(err)
	}
	resave := putRequestFromDTO(got)
	resave.Args = []string{"-m", "/models/big.gguf", "--rpc", "${RPC_SERVERS}"}
	_, err = f.put(ownerToken(), resave)
	if !errors.Is(err, ErrClusterTensorSplitRequired) || err.Error() != ErrClusterTensorSplitRequired.Error() {
		t.Fatalf("err = %v, want the bare %v", err, ErrClusterTensorSplitRequired)
	}
}

// TestClusterBreachPeerHiddenForHiddenMember pins the breaches on a head's
// cluster_status: a member on a server the reader may not manage shows its
// position and kind only, never the peer address its agent reported.
func TestClusterBreachPeerHiddenForHiddenMember(t *testing.T) {
	f := newClusterFixture(t)
	mine := f.member("Mine")
	foreign := f.foreignServer("Foreign")
	head, err := f.put(systemToken(), clusterHeadRequest(autoMember(mine.ID), autoMember(foreign.ID)))
	if err != nil {
		t.Fatalf("system save: %v", err)
	}
	at := clusterTestNow.Add(time.Minute)
	f.svc.SetClusterStatusReader(fakeClusterStatus{head.ID: {
		State: ClusterStateFormed, Members: []ClusterMemberStatusDTO{{Position: 0, State: "held"}, {Position: 1, State: "held"}},
		Breaches: []ClusterBreachDTO{
			{Position: 0, Kind: ClusterBreachPeer, Peer: "100.64.0.8", At: at},
			{Position: 1, Kind: ClusterBreachPeer, Peer: "100.64.0.9", At: at},
		},
	}})
	got, err := f.svc.GetRuntimeSpec(f.ctx, ownerToken(), f.mappingID)
	if err != nil {
		t.Fatal(err)
	}
	want := []ClusterBreachDTO{
		{Position: 0, Kind: ClusterBreachPeer, Peer: "100.64.0.8", At: at},
		{Position: 1},
	}
	if got.ClusterStatus == nil || !slices.Equal(got.ClusterStatus.Breaches, want) {
		t.Fatalf("owner breaches = %+v, want the hidden member's peer left out", got.ClusterStatus)
	}
	sys, err := f.svc.GetRuntimeSpec(f.ctx, systemToken(), f.mappingID)
	if err != nil {
		t.Fatal(err)
	}
	if sys.ClusterStatus == nil || len(sys.ClusterStatus.Breaches) != 2 || sys.ClusterStatus.Breaches[1].Peer != "100.64.0.9" {
		t.Fatalf("system breaches = %+v, want both peers", sys.ClusterStatus)
	}
}

// TestPutClusterRemovingMembersNeedsHeadRightsOnly: removing a member only
// frees its host, so a head owner who cannot manage a member server may
// still drop that member or turn the cluster off -- as deleting the head
// already allowed. Adding a member still needs rights and consent.
func TestPutClusterRemovingMembersNeedsHeadRightsOnly(t *testing.T) {
	setup := func(t *testing.T) (*clusterFixture, ServerDTO, RuntimeSpecDTO) {
		f := newClusterFixture(t)
		mine := f.member("Mine")
		foreign := f.foreignServer("Foreign")
		if _, err := f.put(systemToken(), clusterHeadRequest(autoMember(mine.ID), autoMember(foreign.ID))); err != nil {
			t.Fatalf("system save: %v", err)
		}
		got, err := f.svc.GetRuntimeSpec(f.ctx, ownerToken(), f.mappingID)
		if err != nil {
			t.Fatal(err)
		}
		return f, foreign, got
	}
	t.Run("drop the hidden member", func(t *testing.T) {
		f, foreign, got := setup(t)
		drop := putRequestFromDTO(got)
		drop.ClusterMembers = drop.ClusterMembers[:1]
		if _, err := f.put(ownerToken(), drop); err != nil {
			t.Fatalf("owner drops the hidden member = %v", err)
		}
		members, _, _ := f.routes.ClusterMembers(f.ctx, got.ID)
		if len(members) != 1 || members[0].ServerID == foreign.ID {
			t.Fatalf("membership = %+v", members)
		}
		if !slices.Contains(f.notifiedServers(), foreign.ID) {
			t.Fatalf("notified = %v, want the dropped member's server", f.notifiedServers())
		}
	})
	t.Run("cluster off", func(t *testing.T) {
		f, foreign, got := setup(t)
		off := putRequestFromDTO(got)
		off.ClusterEnabled, off.ClusterMembers = false, nil
		off.Args = []string{"-m", "/models/big.gguf"}
		if _, err := f.put(ownerToken(), off); err != nil {
			t.Fatalf("owner turns the cluster off = %v", err)
		}
		if members, _, _ := f.routes.ClusterMembers(f.ctx, got.ID); len(members) != 0 {
			t.Fatalf("membership after cluster off = %+v", members)
		}
		if !slices.Contains(f.notifiedServers(), foreign.ID) {
			t.Fatalf("notified = %v, want the former member's server", f.notifiedServers())
		}
	})
	t.Run("adding still needs rights", func(t *testing.T) {
		f, _, got := setup(t)
		other := f.foreignServer("Other")
		add := putRequestFromDTO(got)
		add.ClusterMembers = []ClusterMemberDTO{add.ClusterMembers[0], autoMember(other.ID)}
		if _, err := f.put(ownerToken(), add); !errors.Is(err, ErrServerNotFound) {
			t.Fatalf("owner adds a foreign member = %v, want server not found", err)
		}
	})
}

// TestPutClusterUnchangedMembershipWithMissingSpec: a manual member whose
// spec is gone (the reference has no foreign key) is never rewritten as an
// automatic member by a head save that leaves the membership as stored; the
// save runs the full member checks and refuses it.
func TestPutClusterUnchangedMembershipWithMissingSpec(t *testing.T) {
	f := newClusterFixture(t)
	a := f.member("A")
	spec := f.manualSpec(a.ID, RuntimeSpecGPUDTO{Index: 0, VRAMEstimateMB: 5000})
	req := clusterHeadRequest(ClusterMemberDTO{ServerID: a.ID, MemberSpecID: spec.ID})
	req.MemberMode = "manual"
	head := f.mustPut(req)
	if err := f.routes.DeleteRPCMemberSpec(f.ctx, spec.ID); err != nil {
		t.Fatal(err)
	}
	req.IdleTimeoutSeconds = 900
	if _, err := f.put(ownerToken(), req); !errors.Is(err, ErrRPCMemberSpecNotFound) {
		t.Fatalf("re-save with a missing member spec = %v, want %v", err, ErrRPCMemberSpecNotFound)
	}
	members, _, _ := f.routes.ClusterMembers(f.ctx, head.ID)
	if len(members) != 1 || members[0].MemberSpecID != spec.ID {
		t.Fatalf("membership = %+v, want the manual reference kept", members)
	}
}

// TestDeleteRPCMemberSpecSerializedWithHeadWrites: the in-use check and the
// delete of a member spec run under the cluster write lock, so a head save
// cannot claim the spec in between and leave a dangling reference.
func TestDeleteRPCMemberSpecSerializedWithHeadWrites(t *testing.T) {
	f := newClusterFixture(t)
	a := f.member("A")
	spec := f.manualSpec(a.ID)
	f.svc.clusterWriteMu.Lock()
	done := make(chan error, 1)
	go func() { done <- f.svc.DeleteRPCMemberSpec(f.ctx, ownerToken(), a.ID, spec.ID) }()
	select {
	case err := <-done:
		f.svc.clusterWriteMu.Unlock()
		t.Fatalf("DeleteRPCMemberSpec ran while a head write held the lock (err %v)", err)
	case <-time.After(50 * time.Millisecond):
	}
	f.svc.clusterWriteMu.Unlock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// TestPutRPCMemberSpecSerializedWithHeadWrites: a member spec write runs
// under the cluster write lock as a whole (spec row, GPU rows and the
// head's membership), so a head save never reads it half written.
func TestPutRPCMemberSpecSerializedWithHeadWrites(t *testing.T) {
	f := newClusterFixture(t)
	a := f.member("A")
	spec := f.manualSpec(a.ID)
	f.svc.clusterWriteMu.Lock()
	done := make(chan error, 1)
	go func() {
		_, err := f.svc.PutRPCMemberSpec(f.ctx, ownerToken(), a.ID, spec.ID, PutRPCMemberSpecRequest{Binary: clusterRPCBinary, Args: []string{"-t", "4"}})
		done <- err
	}()
	time.Sleep(50 * time.Millisecond)
	stored, _, err := f.routes.RPCMemberSpecByID(f.ctx, spec.ID)
	f.svc.clusterWriteMu.Unlock()
	if err != nil || strings.Contains(stored.Args, "-t") {
		t.Fatalf("member spec written while a head write held the lock: %+v err=%v", stored, err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// TestPutClusterRefusedMembershipKeepsTheStoredSpec: converting a stored
// model spec into a head whose membership the store then refuses leaves the
// spec as it was, not a head without members that its document withholds.
func TestPutClusterRefusedMembershipKeepsTheStoredSpec(t *testing.T) {
	f := newClusterFixture(t)
	a := f.member("A")
	plain := PutRuntimeSpecRequest{Enabled: true, Binary: clusterHeadBinary, Type: "llama_cpp", Args: []string{"-m", "/models/big.gguf"}}
	stored := f.mustPut(plain)
	rival := seedOtherHead(t, f)
	f.svc.routes = &racingPortStore{MemoryStore: f.routes, rival: rival, races: 2}
	if _, err := f.put(ownerToken(), clusterHeadRequest(autoMember(a.ID))); !errors.Is(err, ErrClusterPortExhausted) {
		t.Fatalf("PutRuntimeSpec = %v, want %v", err, ErrClusterPortExhausted)
	}
	spec, ok, err := f.routes.RuntimeSpecByID(f.ctx, stored.ID)
	if err != nil || !ok || spec.ClusterEnabled || strings.Contains(spec.Args, "RPC_SERVERS") {
		t.Fatalf("stored spec after the refused conversion = %+v ok=%v err=%v", spec, ok, err)
	}
	if got := f.notifiedServers(); len(got) != 0 {
		t.Fatalf("notified = %v for a write that stored nothing", got)
	}
}

// usageRaceStore lets the gateway write a member's measured usage between a
// head save's read of the membership and its write, once.
type usageRaceStore struct {
	*routing.MemoryStore
	raised bool
	ramMB  int
}

func (u *usageRaceStore) SetClusterMembersIf(ctx context.Context, head string, expect []routing.ClusterMember, expectGPUs []routing.ClusterMemberGPU, members []routing.ClusterMember, gpus []routing.ClusterMemberGPU) (bool, error) {
	if !u.raised && len(expect) > 0 {
		u.raised = true
		current, _, err := u.MemoryStore.ClusterMembers(ctx, head)
		if err != nil {
			return false, err
		}
		m := current[0]
		m.RAMMeasuredMB, m.RunPeakRAMMB = u.ramMB, u.ramMB
		if _, err := u.MemoryStore.UpdateClusterMemberUsage(ctx, m, nil); err != nil {
			return false, err
		}
	}
	return u.MemoryStore.SetClusterMembersIf(ctx, head, expect, expectGPUs, members, gpus)
}

// TestPutClusterKeepsAUsageRiseDuringTheSave: a head save that keeps the
// split carries the measured values over, and a rise the gateway wrote
// between the save's read and its write survives the save.
func TestPutClusterKeepsAUsageRiseDuringTheSave(t *testing.T) {
	f := newClusterFixture(t)
	a := f.member("A")
	head := f.mustPut(clusterHeadRequest(autoMember(a.ID)))
	f.usage(head.ID, a.ID, 3000, nil)
	race := &usageRaceStore{MemoryStore: f.routes, ramMB: 5000}
	f.svc.routes = race
	req := clusterHeadRequest(autoMember(a.ID))
	req.IdleTimeoutSeconds = 900
	req.ClusterMembers[0].Threads = 4
	if _, err := f.put(ownerToken(), req); err != nil {
		t.Fatal(err)
	}
	if !race.raised {
		t.Fatal("the usage write never ran")
	}
	members, _, _ := f.routes.ClusterMembers(f.ctx, head.ID)
	if len(members) != 1 || members[0].RAMMeasuredMB != 5000 || members[0].Threads != 4 {
		t.Fatalf("membership after the save = %+v, want the raised value and the new threads", members)
	}
}

// TestRPCPortRangeOutsideEphemeralRanges: a member binds its port while the
// agent's model children take theirs from the kernel, so the range lies
// below every default ephemeral range (Linux 32768-60999, Windows and macOS
// 49152-65535) and a child can never hold a member's port.
func TestRPCPortRangeOutsideEphemeralRanges(t *testing.T) {
	if rpcPortFirst != 26000 || rpcPortLast != 26999 || rpcPortLast >= 32768 {
		t.Fatalf("RPC port range %d-%d, want 26000-26999", rpcPortFirst, rpcPortLast)
	}
}

// TestPutClusterPlaceholderInConfigFileNeedsCluster: the agent resolves
// ${RPC_SERVERS} and ${RPC_DEVICES} in config files too and refuses them
// outside a head, so the save refuses them there as it does in args and env.
func TestPutClusterPlaceholderInConfigFileNeedsCluster(t *testing.T) {
	for _, placeholder := range []string{"${RPC_SERVERS}", "${RPC_DEVICES}"} {
		f := newClusterFixture(t)
		req := PutRuntimeSpecRequest{
			Enabled: true, Binary: clusterHeadBinary, Type: "llama_cpp", Args: []string{"--models-preset", "${CONFIG_DIR}/presets.ini"},
			ConfigFiles: []ConfigFileDTO{{Name: "presets.ini", Content: "[big]\nrpc = " + placeholder + "\n"}},
		}
		if _, err := f.put(ownerToken(), req); !errors.Is(err, ErrClusterRPCPlaceholderWithoutCluster) {
			t.Fatalf("%s in a config file of a non-head = %v, want %v", placeholder, err, ErrClusterRPCPlaceholderWithoutCluster)
		}
	}
}

// TestSetServerNetbirdRefusalNotifiesNobody: a NetBird link refused because
// its peer is linked to another server changes nothing, so no cluster
// server is pushed.
func TestSetServerNetbirdRefusalNotifiesNobody(t *testing.T) {
	f := newClusterFixture(t)
	a := f.member("A")
	f.mustPut(clusterHeadRequest(autoMember(a.ID)))
	other := f.member("Other")
	if err := f.routes.UpdateServerNetbirdLink(f.ctx, other.ID, true, "peer-taken"); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.notified = nil
	f.mu.Unlock()
	if _, err := f.svc.SetServerNetbird(f.ctx, systemToken(), f.head.ID, true, "peer-taken", nil, false, "", false, false); !errors.Is(err, ErrNetbirdPeerInUse) {
		t.Fatalf("SetServerNetbird = %v, want %v", err, ErrNetbirdPeerInUse)
	}
	if got := f.notifiedServers(); len(got) != 0 {
		t.Fatalf("notified = %v for a refused link", got)
	}
}
