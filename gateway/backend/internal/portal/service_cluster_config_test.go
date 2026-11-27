// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"op-ai-gateway/internal/routing"
	"slices"
	"strings"
	"testing"
)

// clusterAgents makes every given server's agent declare rpc_cluster, and
// gives each a server_agent application so its document is derived at all.
// The fixture's head server already has one.
func (f *clusterFixture) clusterAgents(serverIDs ...string) fakeAgentFeatures {
	f.t.Helper()
	features := fakeAgentFeatures{f.head.ID: {"runtime_manager", AgentFeatureRPCCluster}}
	for _, id := range serverIDs {
		features[id] = []string{"runtime_manager", AgentFeatureRPCCluster}
		apps, err := f.routes.ApplicationsByServer(f.ctx, id)
		if err != nil {
			f.t.Fatal(err)
		}
		if !slices.ContainsFunc(apps, func(a routing.Application) bool { return a.Type == routing.ProviderServerAgent }) {
			seedServerAgentApplication(f.t, f.routes, id, clusterTestNow)
		}
	}
	f.svc.agentFeatures = features
	return features
}

func (f *clusterFixture) config(serverID string) AgentRuntimeConfigDTO {
	f.t.Helper()
	dto, err := f.svc.AgentRuntimeConfig(f.ctx, serverID)
	if err != nil {
		f.t.Fatalf("AgentRuntimeConfig(%s): %v", serverID, err)
	}
	return dto
}

func specByID(dto AgentRuntimeConfigDTO, id string) (AgentRuntimeSpecDTO, bool) {
	for _, s := range dto.Specs {
		if s.ID == id {
			return s, true
		}
	}
	return AgentRuntimeSpecDTO{}, false
}

func rpcServerSpecs(dto AgentRuntimeConfigDTO) []AgentRuntimeSpecDTO {
	var out []AgentRuntimeSpecDTO
	for _, s := range dto.Specs {
		if s.Kind == AgentSpecKindRPCServer {
			out = append(out, s)
		}
	}
	return out
}

func (f *clusterFixture) memberPort(headID, serverID string) int {
	f.t.Helper()
	members, _ := membersByServer(f.t, f, headID)
	m, ok := members[serverID]
	if !ok {
		f.t.Fatalf("no membership of %s in head %s", serverID, headID)
	}
	return m.Port
}

func (f *clusterFixture) withheldReason() string {
	f.t.Helper()
	dto, err := f.svc.GetRuntimeSpec(f.ctx, ownerToken(), f.mappingID)
	if err != nil {
		f.t.Fatalf("GetRuntimeSpec: %v", err)
	}
	return dto.ClusterWithheldReason
}

// putFor saves req on another mapping of the head's application.
func (f *clusterFixture) putFor(mappingID string, req PutRuntimeSpecRequest) RuntimeSpecDTO {
	f.t.Helper()
	dto, err := f.svc.PutRuntimeSpec(f.ctx, ownerToken(), mappingID, req)
	if err != nil {
		f.t.Fatalf("PutRuntimeSpec(%s): %v", mappingID, err)
	}
	return dto
}

func automaticMemberSpecID(headID, serverID string) string {
	return "rpca_" + headID + "_" + serverID
}

func TestAgentRuntimeConfigDerivesAutomaticMember(t *testing.T) {
	f := newClusterFixture(t)
	a := f.member("A")
	f.clusterAgents(a.ID)
	req := clusterHeadRequest(autoMember(a.ID, 1, 0))
	req.ClusterMembers[0].Threads, req.ClusterMembers[0].CacheEnabled = 8, true
	head := f.mustPut(req)
	port := f.memberPort(head.ID, a.ID)

	headDoc := f.config(f.head.ID)
	if headDoc.AwaitsAgentFeatures {
		t.Fatal("head document held, want it served")
	}
	hs, ok := specByID(headDoc, head.ID)
	if !ok || hs.Cluster == nil {
		t.Fatalf("head spec = %+v ok=%v, want a cluster block", hs, ok)
	}
	if hs.Kind != "" || hs.RPC != nil {
		t.Fatalf("head kind/rpc = %q/%+v, want a model spec", hs.Kind, hs.RPC)
	}
	wantEP := []AgentClusterEndpointDTO{{Address: fmt.Sprintf("A.example.test:%d", port), DeviceCount: 2}}
	if !slices.Equal(hs.Cluster.Endpoints, wantEP) || hs.Cluster.Transport != "relay" || hs.Cluster.Network != "lan" || hs.Cluster.Generation == "" {
		t.Fatalf("cluster = %+v, want endpoints %+v lan/relay and a generation", hs.Cluster, wantEP)
	}
	if got := rpcServerSpecs(headDoc); len(got) != 0 {
		t.Fatalf("head document carries member specs %+v", got)
	}

	memberDoc := f.config(a.ID)
	ms, ok := specByID(memberDoc, automaticMemberSpecID(head.ID, a.ID))
	if !ok {
		t.Fatalf("member document specs = %+v, want %s", memberDoc.Specs, automaticMemberSpecID(head.ID, a.ID))
	}
	if ms.Kind != "rpc_server" || ms.Binary != clusterRPCBinary || !slices.Equal(ms.Args, []string{"-t", "8"}) || len(ms.Env) != 0 {
		t.Fatalf("member launch = kind %q binary %q args %v env %v", ms.Kind, ms.Binary, ms.Args, ms.Env)
	}
	if want := []AgentRuntimeSpecGPUDTO{{Index: 1, VRAMMB: 8000}, {Index: 0, VRAMMB: 8000}}; !slices.Equal(ms.GPUs, want) {
		t.Fatalf("member gpus = %+v, want %+v (selection order)", ms.GPUs, want)
	}
	if ms.StartupTimeoutSeconds != 60 || ms.IdleTimeoutSeconds != 60 || ms.DeviceBackend != "auto" {
		t.Fatalf("member timeouts/backend = %d/%d/%q", ms.StartupTimeoutSeconds, ms.IdleTimeoutSeconds, ms.DeviceBackend)
	}
	if ms.Model != "" || ms.UpstreamModel != "" || ms.ListenPort != 0 || ms.HealthPath != "" || ms.Replicable || ms.Cluster != nil {
		t.Fatalf("member carries model fields: %+v", ms)
	}
	wantRPC := AgentRPCMemberDTO{HeadSpecID: head.ID, Transport: "relay", BindIP: "0.0.0.0", Port: port, HeadIPs: []string{}, HeadDomain: "head.example.test", CacheEnabled: true}
	if ms.RPC == nil || !rpcMemberEqual(*ms.RPC, wantRPC) {
		t.Fatalf("member rpc = %+v, want %+v", ms.RPC, wantRPC)
	}
	// The wire form is the agent's: kind and rpc present, no cluster.
	raw, err := json.Marshal(ms)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"kind":"rpc_server"`, `"rpc":{"head_spec_id":`, `"device_backend":"auto"`} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("member json %s lacks %s", raw, want)
		}
	}
	if strings.Contains(string(raw), `"cluster"`) {
		t.Fatalf("member json %s carries a cluster block", raw)
	}
	if f.withheldReason() != "" {
		t.Fatalf("withheld reason = %q, want none", f.withheldReason())
	}
}

func rpcMemberEqual(a, b AgentRPCMemberDTO) bool {
	return a.HeadSpecID == b.HeadSpecID && a.Transport == b.Transport && a.BindIP == b.BindIP && a.Port == b.Port &&
		slices.Equal(a.HeadIPs, b.HeadIPs) && a.HeadDomain == b.HeadDomain && a.CacheEnabled == b.CacheEnabled
}

func TestAgentRuntimeConfigDerivesManualMember(t *testing.T) {
	f := newClusterFixture(t)
	a := f.member("A")
	f.clusterAgents(a.ID)
	spec, err := f.svc.PutRPCMemberSpec(f.ctx, ownerToken(), a.ID, "", PutRPCMemberSpecRequest{
		Binary: clusterRPCBinary, Args: []string{"-t", "4"}, Env: map[string]string{"FOO": "bar"},
		GPUs:         []RuntimeSpecGPUDTO{{Index: 0, VRAMEstimateMB: 6000}},
		CacheEnabled: true, DeviceBackend: "cuda", RAMEstimateMB: 2000,
	})
	if err != nil {
		t.Fatal(err)
	}
	req := clusterHeadRequest(ClusterMemberDTO{ServerID: a.ID, MemberSpecID: spec.ID})
	req.MemberMode = "manual"
	head := f.mustPut(req)
	port := f.memberPort(head.ID, a.ID)

	hs, ok := specByID(f.config(f.head.ID), head.ID)
	if !ok || hs.Cluster == nil || len(hs.Cluster.Endpoints) != 1 || hs.Cluster.Endpoints[0].DeviceCount != 1 ||
		hs.Cluster.Endpoints[0].Address != fmt.Sprintf("A.example.test:%d", port) {
		t.Fatalf("head spec = %+v", hs.Cluster)
	}
	memberDoc := f.config(a.ID)
	ms, ok := specByID(memberDoc, spec.ID)
	if !ok {
		t.Fatalf("member document specs = %+v, want the manual spec id %s", memberDoc.Specs, spec.ID)
	}
	if ms.Kind != "rpc_server" || !slices.Equal(ms.Args, []string{"-t", "4"}) || ms.Env["FOO"] != "bar" || ms.DeviceBackend != "cuda" || ms.RAMMB != 2000 {
		t.Fatalf("manual member = %+v", ms)
	}
	if want := []AgentRuntimeSpecGPUDTO{{Index: 0, VRAMMB: 6000}}; !slices.Equal(ms.GPUs, want) {
		t.Fatalf("manual gpus = %+v", ms.GPUs)
	}
	if ms.RPC == nil || !ms.RPC.CacheEnabled || ms.RPC.Port != port || ms.RPC.HeadSpecID != head.ID {
		t.Fatalf("manual rpc = %+v", ms.RPC)
	}
	if got := rpcServerSpecs(memberDoc); len(got) != 1 {
		t.Fatalf("member specs = %+v, want exactly the manual one", got)
	}
}

func TestAgentRuntimeConfigTwoHeadsOneServerTwoInstances(t *testing.T) {
	f := newClusterFixture(t)
	a := f.member("A")
	f.clusterAgents(a.ID)
	second, err := f.svc.CreateMapping(f.ctx, ownerToken(), f.app.ID, CreateMappingRequest{GatewayModelName: "big2", AppModelName: "big2"})
	if err != nil {
		t.Fatal(err)
	}
	h1 := f.mustPut(clusterHeadRequest(autoMember(a.ID, 0)))
	h2 := f.putFor(second.ID, clusterHeadRequest(autoMember(a.ID, 1)))

	specs := rpcServerSpecs(f.config(a.ID))
	if len(specs) != 2 {
		t.Fatalf("member specs = %+v, want one per head", specs)
	}
	ids := []string{specs[0].ID, specs[1].ID}
	slices.Sort(ids)
	if want := sortedIDs(automaticMemberSpecID(h1.ID, a.ID), automaticMemberSpecID(h2.ID, a.ID)); !slices.Equal(ids, want) {
		t.Fatalf("member ids = %v, want %v", ids, want)
	}
	if specs[0].RPC.Port == specs[1].RPC.Port {
		t.Fatalf("both instances on port %d", specs[0].RPC.Port)
	}
	headDoc := f.config(f.head.ID)
	for _, h := range []RuntimeSpecDTO{h1, h2} {
		hs, ok := specByID(headDoc, h.ID)
		if !ok || hs.Cluster == nil || hs.Cluster.Endpoints[0].Address != fmt.Sprintf("A.example.test:%d", f.memberPort(h.ID, a.ID)) {
			t.Fatalf("head %s = %+v", h.ID, hs.Cluster)
		}
	}
}

// TestAgentRuntimeConfigEndpointsIgnoreMemberLiveness: the endpoint list is
// the configured membership; a member whose agent is not connected, has no
// telemetry and whose NetBird peer reads offline is still listed.
func TestAgentRuntimeConfigEndpointsIgnoreMemberLiveness(t *testing.T) {
	f := newClusterFixture(t)
	a, b := f.member("A"), f.member("B")
	f.clusterAgents(a.ID, b.ID)
	head := f.mustPut(clusterHeadRequest(autoMember(a.ID), autoMember(b.ID)))
	f.updateServer(b.ID, func(s *routing.AIServer) { s.NetbirdConnected = false; s.Status = routing.ServerStatusDisabled })
	hs, ok := specByID(f.config(f.head.ID), head.ID)
	if !ok || hs.Cluster == nil || len(hs.Cluster.Endpoints) != 2 {
		t.Fatalf("head cluster = %+v, want both configured members", hs.Cluster)
	}
	if hs.Cluster.Endpoints[1].Address != fmt.Sprintf("B.example.test:%d", f.memberPort(head.ID, b.ID)) || hs.Cluster.Endpoints[1].DeviceCount != 1 {
		t.Fatalf("CPU member endpoint = %+v, want one device", hs.Cluster.Endpoints[1])
	}
}

func TestAgentRuntimeConfigWithholdsClusterForOldAgents(t *testing.T) {
	setup := func(t *testing.T) (*clusterFixture, ServerDTO, RuntimeSpecDTO) {
		f := newClusterFixture(t)
		a := f.member("A")
		f.clusterAgents(a.ID)
		head := f.mustPut(clusterHeadRequest(autoMember(a.ID, 0)))
		return f, a, head
	}
	t.Run("head agent without rpc_cluster", func(t *testing.T) {
		f, a, head := setup(t)
		f.svc.agentFeatures.(fakeAgentFeatures)[f.head.ID] = []string{"runtime_manager"}
		if _, ok := specByID(f.config(f.head.ID), head.ID); ok {
			t.Fatal("head pushed to an agent without rpc_cluster")
		}
		if got := rpcServerSpecs(f.config(a.ID)); len(got) != 0 {
			t.Fatalf("member pushed for a withheld head: %+v", got)
		}
		if got := f.withheldReason(); got != ClusterWithheldAgentTooOld {
			t.Fatalf("reason = %q", got)
		}
	})
	t.Run("member agent without rpc_cluster", func(t *testing.T) {
		f, a, head := setup(t)
		f.svc.agentFeatures.(fakeAgentFeatures)[a.ID] = []string{"runtime_manager"}
		if _, ok := specByID(f.config(f.head.ID), head.ID); ok {
			t.Fatal("head pushed although a member agent lacks rpc_cluster")
		}
		if got := rpcServerSpecs(f.config(a.ID)); len(got) != 0 {
			t.Fatalf("member spec pushed to an agent without rpc_cluster: %+v", got)
		}
		if got := f.withheldReason(); got != ClusterWithheldAgentTooOld {
			t.Fatalf("reason = %q", got)
		}
	})
	// An unknown REMOTE agent withholds the cluster from a document; only the
	// document's own unknown agent holds it.
	t.Run("member agent not heard from", func(t *testing.T) {
		f, a, head := setup(t)
		delete(f.svc.agentFeatures.(fakeAgentFeatures), a.ID)
		doc := f.config(f.head.ID)
		if doc.AwaitsAgentFeatures {
			t.Fatal("head document held for a remote member agent's unknown features")
		}
		if _, ok := specByID(doc, head.ID); ok {
			t.Fatal("head pushed while a member agent's features are unknown")
		}
		if got := f.withheldReason(); got != ClusterWithheldFeaturesPending {
			t.Fatalf("reason = %q", got)
		}
		if !f.config(a.ID).AwaitsAgentFeatures {
			t.Fatal("member document served while its own agent's features are unknown")
		}
	})
	t.Run("head agent not heard from", func(t *testing.T) {
		f, a, _ := setup(t)
		delete(f.svc.agentFeatures.(fakeAgentFeatures), f.head.ID)
		doc := f.config(a.ID)
		if doc.AwaitsAgentFeatures {
			t.Fatal("member document held for the remote head agent's unknown features")
		}
		if got := rpcServerSpecs(doc); len(got) != 0 {
			t.Fatalf("member pushed while the head agent's features are unknown: %+v", got)
		}
		if !f.config(f.head.ID).AwaitsAgentFeatures {
			t.Fatal("head document served while its own agent's features are unknown")
		}
	})
	t.Run("another member agent not heard from", func(t *testing.T) {
		f := newClusterFixture(t)
		a, b := f.member("A"), f.member("B")
		f.clusterAgents(a.ID, b.ID)
		f.mustPut(clusterHeadRequest(autoMember(a.ID), autoMember(b.ID)))
		delete(f.svc.agentFeatures.(fakeAgentFeatures), b.ID)
		doc := f.config(a.ID)
		if doc.AwaitsAgentFeatures || len(rpcServerSpecs(doc)) != 0 {
			t.Fatalf("member A document held=%v specs=%+v, want served without the member", doc.AwaitsAgentFeatures, rpcServerSpecs(doc))
		}
	})
	t.Run("no feature reader", func(t *testing.T) {
		f, a, head := setup(t)
		f.svc.agentFeatures = nil
		if _, ok := specByID(f.config(f.head.ID), head.ID); ok {
			t.Fatal("head pushed without a feature reader")
		}
		if got := rpcServerSpecs(f.config(a.ID)); len(got) != 0 {
			t.Fatalf("member pushed without a feature reader: %+v", got)
		}
	})
}

func TestAgentRuntimeConfigWithholdsOnUnknownMeshIP(t *testing.T) {
	f := newClusterFixture(t)
	a := f.member("A")
	f.clusterAgents(a.ID)
	f.netbirdReady(f.head.ID, "100.64.0.2")
	f.netbirdReady(a.ID, "100.64.0.5")
	f.directMeshPolicies()
	req := clusterHeadRequest(autoMember(a.ID, 0))
	req.ClusterNetwork, req.RPCTransport = "mesh", "direct"
	head := f.mustPut(req)
	port := f.memberPort(head.ID, a.ID)

	hs, ok := specByID(f.config(f.head.ID), head.ID)
	if !ok || hs.Cluster.Endpoints[0].Address != fmt.Sprintf("100.64.0.5:%d", port) || hs.Cluster.Network != "mesh" || hs.Cluster.Transport != "direct" {
		t.Fatalf("mesh head = %+v", hs.Cluster)
	}
	ms, ok := specByID(f.config(a.ID), automaticMemberSpecID(head.ID, a.ID))
	if !ok || ms.RPC.BindIP != "100.64.0.5" || !slices.Equal(ms.RPC.HeadIPs, []string{"100.64.0.2"}) || ms.RPC.HeadDomain != "" {
		t.Fatalf("mesh member rpc = %+v", ms.RPC)
	}

	f.updateServer(a.ID, func(s *routing.AIServer) { s.NetbirdIP = "" })
	if _, ok := specByID(f.config(f.head.ID), head.ID); ok {
		t.Fatal("head pushed with an unknown member mesh IP")
	}
	if got := f.withheldReason(); got != ClusterWithheldAddressUnknown {
		t.Fatalf("reason = %q", got)
	}

	// The direct mesh preconditions are re-checked too: Default seen enabled.
	f.updateServer(a.ID, func(s *routing.AIServer) { s.NetbirdIP = "100.64.0.5" })
	if err := f.settings.SetSystemSetting(f.ctx, netbirdDefaultPolicyStateKey, `{"state":"enabled"}`, clusterTestNow); err != nil {
		t.Fatal(err)
	}
	if _, ok := specByID(f.config(f.head.ID), head.ID); ok {
		t.Fatal("direct mesh head pushed while the Default policy is on")
	}
	if got := f.withheldReason(); got != ClusterWithheldDirectUnavailable {
		t.Fatalf("reason = %q", got)
	}
}

func TestAgentRuntimeConfigNeverPartialEndpoints(t *testing.T) {
	setup := func(t *testing.T) (*clusterFixture, ServerDTO, ServerDTO, RuntimeSpecDTO) {
		f := newClusterFixture(t)
		a, b := f.member("A"), f.member("B")
		f.clusterAgents(a.ID, b.ID)
		head := f.mustPut(clusterHeadRequest(autoMember(a.ID, 0), autoMember(b.ID, 0)))
		return f, a, b, head
	}
	assertWithheld := func(t *testing.T, f *clusterFixture, a ServerDTO, head RuntimeSpecDTO, reason string) {
		t.Helper()
		if hs, ok := specByID(f.config(f.head.ID), head.ID); ok {
			t.Fatalf("head pushed with cluster %+v, want it withheld", hs.Cluster)
		}
		if got := rpcServerSpecs(f.config(a.ID)); len(got) != 0 {
			t.Fatalf("healthy member still pushed for a withheld head: %+v", got)
		}
		if got := f.withheldReason(); got != reason {
			t.Fatalf("reason = %q, want %q", got, reason)
		}
	}
	t.Run("membership turned off", func(t *testing.T) {
		f, a, b, head := setup(t)
		f.updateServer(b.ID, func(s *routing.AIServer) { s.RPCMemberEnabled = false })
		assertWithheld(t, f, a, head, ClusterWithheldMemberDisabled)
	})
	t.Run("consent withdrawn", func(t *testing.T) {
		f, a, b, head := setup(t)
		f.updateServer(b.ID, func(s *routing.AIServer) { s.RPCAllowedHeadGroups = encodeRPCHeadGroups([]string{f.foreignGroup()}) })
		assertWithheld(t, f, a, head, ClusterWithheldMemberConsent)
	})
	t.Run("no matching binary", func(t *testing.T) {
		f, a, b, head := setup(t)
		if err := f.routes.DeleteManualServerBinary(f.ctx, b.ID, clusterRPCBinary); err != nil {
			t.Fatal(err)
		}
		assertWithheld(t, f, a, head, ClusterWithheldNoMatchingBinary)
	})
	t.Run("lan address gone", func(t *testing.T) {
		f, a, b, head := setup(t)
		f.updateServer(b.ID, func(s *routing.AIServer) { s.NetbirdEnabled = true })
		assertWithheld(t, f, a, head, ClusterWithheldAddressUnknown)
	})
	t.Run("member without an agent application", func(t *testing.T) {
		f, a, b, head := setup(t)
		apps, _ := f.routes.ApplicationsByServer(f.ctx, b.ID)
		for _, app := range apps {
			if err := f.routes.DeleteApplication(f.ctx, app.ID); err != nil {
				t.Fatal(err)
			}
		}
		assertWithheld(t, f, a, head, ClusterWithheldMemberUnmanaged)
	})
	t.Run("gap in the membership", func(t *testing.T) {
		f, a, b, head := setup(t)
		members, gpus, _ := f.routes.ClusterMembers(f.ctx, head.ID)
		members[1].Position = 2
		for i := range gpus {
			if gpus[i].MemberPosition == 1 {
				gpus[i].MemberPosition = 2 // a GPU row follows its member
			}
		}
		if err := f.routes.SetClusterMembers(f.ctx, head.ID, members, gpus); err != nil {
			t.Fatal(err)
		}
		_ = b
		assertWithheld(t, f, a, head, ClusterWithheldMemberMissing)
	})
	t.Run("member server cannot be deleted", func(t *testing.T) {
		f, _, b, head := setup(t)
		if _, err := f.svc.DeleteServer(f.ctx, ownerToken(), b.ID, false); !errors.Is(err, ErrRPCMemberInUse) {
			t.Fatalf("delete member server = %v, want %v", err, ErrRPCMemberInUse)
		}
		if hs, ok := specByID(f.config(f.head.ID), head.ID); !ok || len(hs.Cluster.Endpoints) != 2 {
			t.Fatalf("head after refused delete = %+v", hs.Cluster)
		}
	})
}

func TestAgentRuntimeConfigLanUsesLanAddressElseDomain(t *testing.T) {
	f := newClusterFixture(t)
	a, b := f.member("A"), f.member("B")
	f.clusterAgents(a.ID, b.ID)
	f.updateServer(a.ID, func(s *routing.AIServer) { s.LANAddress, s.LANBindOnly = "192.168.1.5", true })
	f.updateServer(f.head.ID, func(s *routing.AIServer) { s.LANAddress = "192.168.1.2" })
	head := f.mustPut(clusterHeadRequest(autoMember(a.ID, 0), autoMember(b.ID, 0)))
	pa, pb := f.memberPort(head.ID, a.ID), f.memberPort(head.ID, b.ID)

	hs, ok := specByID(f.config(f.head.ID), head.ID)
	want := []AgentClusterEndpointDTO{{Address: fmt.Sprintf("192.168.1.5:%d", pa), DeviceCount: 1}, {Address: fmt.Sprintf("B.example.test:%d", pb), DeviceCount: 1}}
	if !ok || !slices.Equal(hs.Cluster.Endpoints, want) {
		t.Fatalf("endpoints = %+v, want %+v", hs.Cluster, want)
	}
	ma, _ := specByID(f.config(a.ID), automaticMemberSpecID(head.ID, a.ID))
	mb, _ := specByID(f.config(b.ID), automaticMemberSpecID(head.ID, b.ID))
	if ma.RPC == nil || ma.RPC.BindIP != "192.168.1.5" || !slices.Equal(ma.RPC.HeadIPs, []string{"192.168.1.2"}) || ma.RPC.HeadDomain != "" {
		t.Fatalf("bind-only member rpc = %+v", ma.RPC)
	}
	if mb.RPC == nil || mb.RPC.BindIP != "0.0.0.0" {
		t.Fatalf("member without bind-only rpc = %+v", mb.RPC)
	}

	// Without the head's lan_address the member resolves the head's Domain.
	f.updateServer(f.head.ID, func(s *routing.AIServer) { s.LANAddress = "" })
	mb, _ = specByID(f.config(b.ID), automaticMemberSpecID(head.ID, b.ID))
	if mb.RPC == nil || len(mb.RPC.HeadIPs) != 0 || mb.RPC.HeadDomain != "head.example.test" {
		t.Fatalf("member rpc without head lan address = %+v", mb.RPC)
	}
}

func TestAgentRuntimeConfigMemberDemandUsesStoredPeak(t *testing.T) {
	f := newClusterFixture(t)
	a := f.member("A")
	f.clusterAgents(a.ID)
	req := clusterHeadRequest(autoMember(a.ID, 0, 1))
	req.ClusterMembers[0].RAMEstimateMB = 1000
	head := f.mustPut(req)
	f.usage(head.ID, a.ID, 3000, map[int]int{0: 9500, 1: 4000})

	ms, ok := specByID(f.config(a.ID), automaticMemberSpecID(head.ID, a.ID))
	if !ok {
		t.Fatal("no member spec")
	}
	if want := []AgentRuntimeSpecGPUDTO{{Index: 0, VRAMMB: 9500}, {Index: 1, VRAMMB: 8000}}; !slices.Equal(ms.GPUs, want) {
		t.Fatalf("member gpus = %+v, want max(stored, estimate) %+v", ms.GPUs, want)
	}
	if ms.RAMMB != 3000 {
		t.Fatalf("member ram = %d, want the stored 3000", ms.RAMMB)
	}
}

func TestAgentRuntimeConfigStaleDemandCappedAtBudget(t *testing.T) {
	f := newClusterFixture(t)
	a := f.member("A")
	f.clusterAgents(a.ID)
	f.hardware(a.ID, `{"os":"linux","memory":{"total_bytes":17179869184},"gpus":[{"index":0,"name":"NVIDIA L4","pci_bus_id":"0000:01:00.0","memory_total_bytes":25769803776},{"index":1,"name":"NVIDIA L4","pci_bus_id":"0000:02:00.0","memory_total_bytes":10737418240}]}`)
	if err := f.routes.SetServerGPUBudgets(f.ctx, a.ID, []routing.ServerGPUBudget{{ServerID: a.ID, GPUIndex: 0, BudgetMB: 9000}}); err != nil {
		t.Fatal(err)
	}
	head := f.mustPut(clusterHeadRequest(autoMember(a.ID, 0, 1)))
	f.usage(head.ID, a.ID, 20000, map[int]int{0: 12000, 1: 30000})

	fresh, _ := specByID(f.config(a.ID), automaticMemberSpecID(head.ID, a.ID))
	if want := []AgentRuntimeSpecGPUDTO{{Index: 0, VRAMMB: 12000}, {Index: 1, VRAMMB: 30000}}; !slices.Equal(fresh.GPUs, want) || fresh.RAMMB != 20000 {
		t.Fatalf("fresh demand = %+v ram %d, want the stored values uncapped", fresh.GPUs, fresh.RAMMB)
	}

	members, _ := membersByServer(t, f, head.ID)
	m := members[a.ID]
	m.Stale = true
	if _, err := f.routes.UpdateClusterMemberUsage(f.ctx, m, nil); err != nil {
		t.Fatal(err)
	}
	stale, _ := specByID(f.config(a.ID), automaticMemberSpecID(head.ID, a.ID))
	// GPU 0 is capped at its budget, GPU 1 (no budget) at the card, and RAM at
	// the host's memory.
	if want := []AgentRuntimeSpecGPUDTO{{Index: 0, VRAMMB: 9000}, {Index: 1, VRAMMB: 10240}}; !slices.Equal(stale.GPUs, want) {
		t.Fatalf("stale demand = %+v, want %+v", stale.GPUs, want)
	}
	if stale.RAMMB != 16384 {
		t.Fatalf("stale ram = %d, want the host ceiling 16384", stale.RAMMB)
	}
}

func TestAgentRuntimeConfigGenerationFollowsMemberBinary(t *testing.T) {
	f := newClusterFixture(t)
	a := f.member("A")
	f.clusterAgents(a.ID)
	head := f.mustPut(clusterHeadRequest(autoMember(a.ID, 0)))
	before, _ := specByID(f.config(f.head.ID), head.ID)
	// A second matching binary earlier by path becomes the member's binary.
	f.binary(a.ID, "/opt/a/ggml-rpc-server", BinaryKindRPCServer, clusterCommit)
	after, _ := specByID(f.config(f.head.ID), head.ID)
	if before.Cluster == nil || after.Cluster == nil || before.Cluster.Generation == after.Cluster.Generation {
		t.Fatalf("generation %v -> %v, want a change with the member binary", before.Cluster, after.Cluster)
	}
	ms, _ := specByID(f.config(a.ID), automaticMemberSpecID(head.ID, a.ID))
	if ms.Binary != "/opt/a/ggml-rpc-server" {
		t.Fatalf("member binary = %q", ms.Binary)
	}
}

func TestAgentRuntimeConfigHeadIdleTimeoutIsDerived(t *testing.T) {
	f := newClusterFixture(t)
	a := f.member("A")
	f.clusterAgents(a.ID)
	req := clusterHeadRequest(autoMember(a.ID, 0))
	req.IdleTimeoutSeconds = 0
	head := f.mustPut(req)
	hs, _ := specByID(f.config(f.head.ID), head.ID)
	if hs.IdleTimeoutSeconds != DefaultInstanceIdleFloorSeconds {
		t.Fatalf("head idle = %d, want the floor %d", hs.IdleTimeoutSeconds, DefaultInstanceIdleFloorSeconds)
	}
	if err := f.routes.UpdateRuntimeSpecStartupMeasured(f.ctx, head.ID, 100_000); err != nil {
		t.Fatal(err)
	}
	hs, _ = specByID(f.config(f.head.ID), head.ID)
	if want := int(DefaultInstanceIdleFactor * 100); hs.IdleTimeoutSeconds != want {
		t.Fatalf("head idle = %d, want factor x startup %d", hs.IdleTimeoutSeconds, want)
	}
}

// TestMemberEditNotifiesHeadAndMemberServers: a manual member spec's GPU
// change re-syncs the head's membership GPU rows (measured values carried by
// index, removed indexes dropped) and notifies both servers; a server write
// and an inventory change on a member notify every server of its cluster.
func TestMemberEditNotifiesHeadAndMemberServers(t *testing.T) {
	f := newClusterFixture(t)
	a, b := f.member("A"), f.member("B")
	f.clusterAgents(a.ID, b.ID)
	spec := f.manualSpec(a.ID, RuntimeSpecGPUDTO{Index: 0, VRAMEstimateMB: 5000}, RuntimeSpecGPUDTO{Index: 1, VRAMEstimateMB: 5000})
	bSpec := f.manualSpec(b.ID)
	req := clusterHeadRequest(ClusterMemberDTO{ServerID: a.ID, MemberSpecID: spec.ID}, ClusterMemberDTO{ServerID: b.ID, MemberSpecID: bSpec.ID})
	req.MemberMode = "manual"
	head := f.mustPut(req)
	f.usage(head.ID, a.ID, 0, map[int]int{0: 7000, 1: 6500})
	reset := func() {
		f.mu.Lock()
		f.notified = nil
		f.mu.Unlock()
	}
	all := sortedIDs(f.head.ID, a.ID, b.ID)

	reset()
	if _, err := f.svc.PutRPCMemberSpec(f.ctx, ownerToken(), a.ID, spec.ID, PutRPCMemberSpecRequest{
		Binary: clusterRPCBinary, GPUs: []RuntimeSpecGPUDTO{{Index: 1, VRAMEstimateMB: 5000}, {Index: 2, VRAMEstimateMB: 5000}},
	}); err != nil {
		t.Fatal(err)
	}
	if got := f.notifiedServers(); !slices.Equal(got, all) {
		t.Fatalf("member spec edit notified %v, want %v", got, all)
	}
	_, gpus := membersByServer(t, f, head.ID)
	if len(gpus[a.ID]) != 2 || gpus[a.ID][1].VRAMMeasuredMB != 6500 || gpus[a.ID][2].VRAMMeasuredMB != 0 || gpus[a.ID][1].Position != 0 || gpus[a.ID][2].Position != 1 {
		t.Fatalf("membership gpus after the spec edit = %+v", gpus[a.ID])
	}
	if _, kept := gpus[a.ID][0]; kept {
		t.Fatal("removed gpu 0 kept on the membership")
	}
	hs, _ := specByID(f.config(f.head.ID), head.ID)
	if hs.Cluster == nil || hs.Cluster.Endpoints[0].DeviceCount != 2 {
		t.Fatalf("head after the member edit = %+v", hs.Cluster)
	}

	reset()
	lan := "192.168.1.9"
	if _, err := f.svc.UpdateServer(f.ctx, ownerToken(), b.ID, UpdateServerRequest{LANAddress: &lan}); err != nil {
		t.Fatal(err)
	}
	if got := f.notifiedServers(); !slices.Equal(got, all) {
		t.Fatalf("member server write notified %v, want %v", got, all)
	}

	reset()
	if _, err := f.svc.CreateManualServerBinary(f.ctx, ownerToken(), b.ID, PutServerBinaryRequest{Path: "/opt/b/ggml-rpc-server"}); err != nil {
		t.Fatal(err)
	}
	if got := f.notifiedServers(); !slices.Equal(got, all) {
		t.Fatalf("member inventory write notified %v, want %v", got, all)
	}

	// A member's server_agent application: deleting it withholds the head,
	// creating it again lets it be pushed; both notify every server.
	apps, _ := f.routes.ApplicationsByServer(f.ctx, b.ID)
	reset()
	for _, app := range apps {
		if err := f.svc.DeleteApplication(f.ctx, ownerToken(), app.ID); err != nil {
			t.Fatal(err)
		}
	}
	if got := f.notifiedServers(); !slices.Equal(got, all) {
		t.Fatalf("member application delete notified %v, want %v", got, all)
	}
	if _, ok := specByID(f.config(f.head.ID), head.ID); ok {
		t.Fatal("head pushed with a member without a server_agent application")
	}
	reset()
	if _, err := f.svc.CreateApplication(f.ctx, ownerToken(), b.ID, CreateApplicationRequest{Type: routing.ProviderServerAgent, Port: 9000, Scheme: "http"}); err != nil {
		t.Fatal(err)
	}
	if got := f.notifiedServers(); !slices.Equal(got, all) {
		t.Fatalf("member application create notified %v, want %v", got, all)
	}
	if _, ok := specByID(f.config(f.head.ID), head.ID); !ok {
		t.Fatal("head withheld after the member's server_agent application came back")
	}

	reset()
	if _, err := f.svc.SetServerGPUBudgets(f.ctx, ownerToken(), b.ID, SetGPUBudgetsRequest{}); err != nil {
		t.Fatal(err)
	}
	if got := f.notifiedServers(); !slices.Equal(got, all) {
		t.Fatalf("member budget write notified %v, want %v", got, all)
	}

	reset()
	if err := f.svc.DeleteGroup(f.ctx, systemToken(), f.foreignGroup()); err != nil {
		t.Fatal(err)
	}
	if got := f.notifiedServers(); !slices.Equal(got, all) {
		t.Fatalf("group delete notified %v, want %v", got, all)
	}

	reset()
	f.svc.NotifyClustersOfServer(f.ctx, f.head.ID)
	if got := f.notifiedServers(); !slices.Equal(got, all) {
		t.Fatalf("head fan-out notified %v, want %v", got, all)
	}
}

// TestPutClusterConsentBindsSystemScope: consent has no system-scope bypass,
// at save as in the runtime document. A member whose consent list names
// another group, or whose empty list (the member server's own admin groups)
// does not meet the head server's, is refused like for any caller.
func TestPutClusterConsentBindsSystemScope(t *testing.T) {
	f := newClusterFixture(t)
	named := f.member("Named")
	f.updateServer(named.ID, func(s *routing.AIServer) { s.RPCAllowedHeadGroups = encodeRPCHeadGroups([]string{f.foreignGroup()}) })
	if _, err := f.put(systemToken(), clusterHeadRequest(autoMember(named.ID))); !errors.Is(err, ErrServerNotFound) {
		t.Fatalf("system save past a named consent = %v, want %v", err, ErrServerNotFound)
	}
	foreign := f.foreignServer("Foreign")
	f.updateServer(foreign.ID, func(s *routing.AIServer) { s.RPCAllowedHeadGroups = "[]" })
	if _, err := f.put(systemToken(), clusterHeadRequest(autoMember(foreign.ID))); !errors.Is(err, ErrServerNotFound) {
		t.Fatalf("system save past the default consent = %v, want %v", err, ErrServerNotFound)
	}
	// The default consent of a member in the head's own admin group admits.
	own := f.member("Own")
	f.updateServer(own.ID, func(s *routing.AIServer) { s.RPCAllowedHeadGroups = "[]" })
	if _, err := f.put(systemToken(), clusterHeadRequest(autoMember(own.ID))); err != nil {
		t.Fatalf("system save with the default consent of the same group: %v", err)
	}
}

// TestAgentRuntimeConfigGenerationFollowsMemberLaunch: a manual member's args
// and an automatic member's device backend are part of the head's generation.
func TestAgentRuntimeConfigGenerationFollowsMemberLaunch(t *testing.T) {
	f := newClusterFixture(t)
	a, b := f.member("A"), f.member("B")
	f.clusterAgents(a.ID, b.ID)
	spec := f.manualSpec(a.ID, RuntimeSpecGPUDTO{Index: 0, VRAMEstimateMB: 5000})
	req := clusterHeadRequest(ClusterMemberDTO{ServerID: a.ID, MemberSpecID: spec.ID})
	req.MemberMode = "manual"
	head := f.mustPut(req)
	generation := func() string {
		t.Helper()
		hs, ok := specByID(f.config(f.head.ID), head.ID)
		if !ok || hs.Cluster == nil {
			t.Fatalf("head withheld: %q", f.withheldReason())
		}
		return hs.Cluster.Generation
	}
	before := generation()
	if _, err := f.svc.PutRPCMemberSpec(f.ctx, ownerToken(), a.ID, spec.ID, PutRPCMemberSpecRequest{
		Binary: clusterRPCBinary, Args: []string{"-t", "6"}, GPUs: []RuntimeSpecGPUDTO{{Index: 0, VRAMEstimateMB: 5000}},
	}); err != nil {
		t.Fatal(err)
	}
	if after := generation(); after == before {
		t.Fatal("generation unchanged by the manual member's args")
	}

	second, err := f.svc.CreateMapping(f.ctx, ownerToken(), f.app.ID, CreateMappingRequest{GatewayModelName: "big2", AppModelName: "big2"})
	if err != nil {
		t.Fatal(err)
	}
	auto := f.putFor(second.ID, clusterHeadRequest(autoMember(b.ID, 0)))
	autoGen := func() string {
		hs, _ := specByID(f.config(f.head.ID), auto.ID)
		return hs.Cluster.Generation
	}
	before = autoGen()
	vulkan := clusterHeadRequest(autoMember(b.ID, 0))
	vulkan.ClusterMembers[0].DeviceBackend = "vulkan"
	f.putFor(second.ID, vulkan)
	if autoGen() == before {
		t.Fatal("generation unchanged by the automatic member's device backend")
	}
}

// TestAgentRuntimeConfigWithholdsOnCorruptMemberSpec: a manual member spec
// whose stored args cannot be read withholds its head with a reason; the
// documents and the head's read view are still served.
func TestAgentRuntimeConfigWithholdsOnCorruptMemberSpec(t *testing.T) {
	f := newClusterFixture(t)
	a := f.member("A")
	f.clusterAgents(a.ID)
	spec := f.manualSpec(a.ID)
	req := clusterHeadRequest(ClusterMemberDTO{ServerID: a.ID, MemberSpecID: spec.ID})
	req.MemberMode = "manual"
	head := f.mustPut(req)
	row, _, err := f.routes.RPCMemberSpecByID(f.ctx, spec.ID)
	if err != nil {
		t.Fatal(err)
	}
	row.Args = "{not json"
	if err := f.routes.UpsertRPCMemberSpec(f.ctx, row); err != nil {
		t.Fatal(err)
	}
	if _, ok := specByID(f.config(f.head.ID), head.ID); ok {
		t.Fatal("head pushed with an unreadable member spec")
	}
	if got := rpcServerSpecs(f.config(a.ID)); len(got) != 0 {
		t.Fatalf("member pushed from an unreadable spec: %+v", got)
	}
	if got := f.withheldReason(); got != ClusterWithheldMemberSpecInvalid {
		t.Fatalf("reason = %q", got)
	}
}

// failingSettings is a system settings store whose reads fail while fail is
// set.
type failingSettings struct {
	SystemSettingsStore
	fail bool
}

func (s *failingSettings) SystemSettings(ctx context.Context) (map[string]string, error) {
	if s.fail {
		return nil, errors.New("settings read failed")
	}
	return s.SystemSettingsStore.SystemSettings(ctx)
}

// TestAgentRuntimeConfigPropagatesSettingsReadError: a failed settings read
// is an error of the document, never every NetBird switch read as off, which
// would withhold a running direct mesh cluster and drain it.
func TestAgentRuntimeConfigPropagatesSettingsReadError(t *testing.T) {
	f := newClusterFixture(t)
	a := f.member("A")
	f.clusterAgents(a.ID)
	f.netbirdReady(f.head.ID, "100.64.0.1")
	f.netbirdReady(a.ID, "100.64.0.2")
	f.directMeshPolicies()
	req := clusterHeadRequest(autoMember(a.ID))
	req.ClusterNetwork, req.RPCTransport = "mesh", "direct"
	head := f.mustPut(req)
	if hs, ok := specByID(f.config(f.head.ID), head.ID); !ok || hs.Cluster == nil {
		t.Fatalf("head withheld before the failure: %q", f.withheldReason())
	}
	settings := &failingSettings{SystemSettingsStore: f.settings, fail: true}
	f.svc.settings = settings
	for _, id := range []string{f.head.ID, a.ID} {
		if _, err := f.svc.AgentRuntimeConfig(f.ctx, id); err == nil {
			t.Fatalf("AgentRuntimeConfig(%s) served a document over a failed settings read", id)
		}
	}
}

// TestAgentRuntimeConfigGenerationCoversEveryMemberLaunchInput: a member's
// env and its cache switch relaunch the member, so they move the head's
// generation too and the head relaunches with it instead of losing the
// member under a running model.
func TestAgentRuntimeConfigGenerationCoversEveryMemberLaunchInput(t *testing.T) {
	f := newClusterFixture(t)
	a, b := f.member("A"), f.member("B")
	f.clusterAgents(a.ID, b.ID)
	spec := f.manualSpec(a.ID, RuntimeSpecGPUDTO{Index: 0, VRAMEstimateMB: 5000})
	req := clusterHeadRequest(ClusterMemberDTO{ServerID: a.ID, MemberSpecID: spec.ID})
	req.MemberMode = "manual"
	head := f.mustPut(req)
	generation := func(headID string) string {
		t.Helper()
		hs, ok := specByID(f.config(f.head.ID), headID)
		if !ok || hs.Cluster == nil {
			t.Fatalf("head %s withheld", headID)
		}
		return hs.Cluster.Generation
	}
	putSpec := func(r PutRPCMemberSpecRequest) {
		t.Helper()
		r.Binary, r.GPUs = clusterRPCBinary, []RuntimeSpecGPUDTO{{Index: 0, VRAMEstimateMB: 5000}}
		if _, err := f.svc.PutRPCMemberSpec(f.ctx, ownerToken(), a.ID, spec.ID, r); err != nil {
			t.Fatal(err)
		}
	}
	before := generation(head.ID)
	putSpec(PutRPCMemberSpecRequest{Env: map[string]string{"GGML_SCHED_MAX_COPIES": "1"}})
	afterEnv := generation(head.ID)
	if afterEnv == before {
		t.Fatal("generation unchanged by the manual member's env")
	}
	putSpec(PutRPCMemberSpecRequest{Env: map[string]string{"GGML_SCHED_MAX_COPIES": "1"}, CacheEnabled: true})
	if generation(head.ID) == afterEnv {
		t.Fatal("generation unchanged by the manual member's cache switch")
	}

	second, err := f.svc.CreateMapping(f.ctx, ownerToken(), f.app.ID, CreateMappingRequest{GatewayModelName: "big2", AppModelName: "big2"})
	if err != nil {
		t.Fatal(err)
	}
	auto := f.putFor(second.ID, clusterHeadRequest(autoMember(b.ID, 0)))
	before = generation(auto.ID)
	cached := clusterHeadRequest(autoMember(b.ID, 0))
	cached.ClusterMembers[0].CacheEnabled = true
	f.putFor(second.ID, cached)
	if generation(auto.ID) == before {
		t.Fatal("generation unchanged by the automatic member's cache switch")
	}
}

// TestAgentRuntimeConfigPeerFeaturesFromPersistedVersion: after a gateway
// restart a peer whose live declaration is not known yet counts as declaring
// rpc_cluster when its last reported agent version has it, so the host that
// reports first does not cut a running cluster.
func TestAgentRuntimeConfigPeerFeaturesFromPersistedVersion(t *testing.T) {
	for _, tc := range []struct {
		name, version, reason string
	}{
		{"current agent", "0.18.0", ""},
		{"newer agent", "v0.19.2-rc1", ""},
		{"older agent", "0.17.4", ClusterWithheldAgentTooOld},
		{"no version reported", "", ClusterWithheldFeaturesPending},
		{"unreadable version", "dev", ClusterWithheldFeaturesPending},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newClusterFixture(t)
			a := f.member("A")
			f.clusterAgents(a.ID)
			head := f.mustPut(clusterHeadRequest(autoMember(a.ID)))
			if tc.version != "" {
				if err := f.routes.UpsertTelemetry(f.ctx, routing.ServerTelemetry{
					ServerID: a.ID, ReportedAt: clusterTestNow, AgentVersion: tc.version, UpdatedAt: clusterTestNow,
				}); err != nil {
					t.Fatal(err)
				}
			}
			delete(f.svc.agentFeatures.(fakeAgentFeatures), a.ID)
			_, pushed := specByID(f.config(f.head.ID), head.ID)
			if got := f.withheldReason(); got != tc.reason || pushed != (tc.reason == "") {
				t.Fatalf("reason = %q pushed = %v, want reason %q", got, pushed, tc.reason)
			}
			// The viewer's own unknown declaration still holds its document.
			if !f.config(a.ID).AwaitsAgentFeatures {
				t.Fatal("member document served while its own agent's features are unknown")
			}
		})
	}
}

// The issue's headline case (#441): a head llama-server built from git (a
// commit) and a member ggml-rpc-server built from the source tarball (build
// 0, commit unknown) that speak the same RPC protocol form a cluster: the
// member is derived with that binary and the cluster is not withheld.
func TestAgentRuntimeConfigDerivesProtocolOnlyMember(t *testing.T) {
	f := newClusterFixture(t)
	f.agentBinaries(f.head.ID, false, routing.ServerBinary{
		Path: protoHeadBinary, Kind: BinaryKindLlamaServer,
		Version: "version: 7000 (abcdef1)", Commit: "abcdef1", Build: "7000", RPCProtocol: "10.0.2",
	})
	a := f.member("A")
	const tarballRPC = "/opt/src/ggml-rpc-server"
	f.agentBinaries(a.ID, true, routing.ServerBinary{
		Path: tarballRPC, Kind: BinaryKindRPCServer,
		Version: "version: 1.1.0-dev (build 0, commit unknown)", Commit: "unknown", Build: "0", RPCProtocol: "10.0.2",
	})
	f.clusterAgents(a.ID)
	req := clusterHeadRequest(autoMember(a.ID, 0))
	req.Binary = protoHeadBinary
	head := f.mustPut(req)

	if r := f.withheldReason(); r != "" {
		t.Fatalf("withheld reason = %q, want none", r)
	}
	hs, ok := specByID(f.config(f.head.ID), head.ID)
	if !ok || hs.Cluster == nil || len(hs.Cluster.Endpoints) != 1 {
		t.Fatalf("head spec = %+v ok=%v, want a cluster with one endpoint", hs, ok)
	}
	ms, ok := specByID(f.config(a.ID), automaticMemberSpecID(head.ID, a.ID))
	if !ok || ms.Binary != tarballRPC {
		t.Fatalf("member spec = %+v ok=%v, want binary %s", ms, ok, tarballRPC)
	}
}

// TestServerMemoryCeilingsKeyedByVendor: each card's ceiling is its own
// budget, else its own memory, by (vendor, index); a vendor-less budget is
// the legacy vendor's card.
func TestServerMemoryCeilingsKeyedByVendor(t *testing.T) {
	f := newClusterFixture(t)
	a := f.member("A")
	f.hardware(a.ID, mixedHostReport(`["rocm"]`))
	if err := f.routes.SetServerGPUBudgets(f.ctx, a.ID, []routing.ServerGPUBudget{{ServerID: a.ID, GPUVendor: "nvidia", GPUIndex: 0, BudgetMB: 10000, CreatedAt: clusterTestNow, UpdatedAt: clusterTestNow}}); err != nil {
		t.Fatal(err)
	}
	got, _, err := f.svc.serverMemoryCeilings(f.ctx, a.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if got[routing.GPUKey{Vendor: "nvidia", Index: 0}] != 10000 || got[routing.GPUKey{Vendor: "amd", Index: 0}] != 16384 || len(got) != 2 {
		t.Fatalf("ceilings = %v, want nvidia 0: 10000, amd 0: 16384", got)
	}

	if _, err := f.routes.BackfillServerGPUVendor(f.ctx, a.ID, "amd", false); err != nil {
		t.Fatal(err)
	}
	if err := f.routes.SetServerGPUBudgets(f.ctx, a.ID, []routing.ServerGPUBudget{{ServerID: a.ID, GPUIndex: 0, BudgetMB: 9000, CreatedAt: clusterTestNow, UpdatedAt: clusterTestNow}}); err != nil {
		t.Fatal(err)
	}
	if got, _, err = f.svc.serverMemoryCeilings(f.ctx, a.ID, "amd"); err != nil {
		t.Fatal(err)
	}
	if got[routing.GPUKey{Vendor: "amd", Index: 0}] != 9000 || got[routing.GPUKey{Vendor: "nvidia", Index: 0}] != 11264 || len(got) != 2 {
		t.Fatalf("ceilings with a vendor-less budget, legacy amd = %v", got)
	}
}

// addClusterAgent adds serverID to the fixture's agents as one declaring
// rpc_cluster, with a server_agent application, without resetting the
// declarations made so far (clusterAgents starts them over).
func (f *clusterFixture) addClusterAgent(serverID string) {
	f.t.Helper()
	f.svc.agentFeatures.(fakeAgentFeatures)[serverID] = []string{"runtime_manager", AgentFeatureRPCCluster}
	seedServerAgentApplication(f.t, f.routes, serverID, clusterTestNow)
}

// memberPorts are the stored ports of headID's members in member order.
func (f *clusterFixture) memberPorts(headID string) []int {
	f.t.Helper()
	rows, _, err := f.routes.ClusterMembers(f.ctx, headID)
	if err != nil {
		f.t.Fatal(err)
	}
	ports := make([]int, len(rows))
	for i, r := range rows {
		ports[i] = r.Port
	}
	return ports
}

// TestSameHostMemberUsesLoopback: a member on the head's own server is dialed
// and bound on 127.0.0.1, always direct, and knows its head as 127.0.0.1,
// whatever the cluster's network and transport; a member on another server
// keeps its mesh address. A cluster whose members all run on the head's
// server needs neither NetBird on that server nor the direct mesh
// preconditions.
func TestSameHostMemberUsesLoopback(t *testing.T) {
	f := newClusterFixture(t)
	b := f.member("B")
	f.sameHostAgent(f.head.ID)
	f.addClusterAgent(b.ID)
	f.netbirdReady(f.head.ID, "100.64.0.2")
	f.netbirdReady(b.ID, "100.64.0.5")
	f.directMeshPolicies()
	req := clusterHeadRequest(autoMember(f.head.ID, 1), autoMember(b.ID, 0))
	req.ClusterNetwork, req.RPCTransport = "mesh", "direct"
	head := f.mustPut(req)
	ports := f.memberPorts(head.ID)

	headDoc := f.config(f.head.ID)
	hs, ok := specByID(headDoc, head.ID)
	if !ok || hs.Cluster == nil {
		t.Fatalf("head spec = %+v ok=%v, want a cluster block (reason %q)", hs, ok, f.withheldReason())
	}
	wantEP := []AgentClusterEndpointDTO{
		{Address: fmt.Sprintf("127.0.0.1:%d", ports[0]), DeviceCount: 1},
		{Address: fmt.Sprintf("100.64.0.5:%d", ports[1]), DeviceCount: 1},
	}
	if !slices.Equal(hs.Cluster.Endpoints, wantEP) || hs.Cluster.Network != "mesh" || hs.Cluster.Transport != "direct" {
		t.Fatalf("cluster = %+v, want endpoints %+v mesh/direct", hs.Cluster, wantEP)
	}
	local, ok := specByID(headDoc, automaticMemberSpecID(head.ID, f.head.ID))
	if !ok {
		t.Fatalf("head document specs = %+v, want the same-host member", headDoc.Specs)
	}
	wantLocal := AgentRPCMemberDTO{HeadSpecID: head.ID, Transport: "direct", BindIP: "127.0.0.1", Port: ports[0], HeadIPs: []string{"127.0.0.1"}}
	if local.RPC == nil || !rpcMemberEqual(*local.RPC, wantLocal) {
		t.Fatalf("same-host member rpc = %+v, want %+v", local.RPC, wantLocal)
	}
	remote, ok := specByID(f.config(b.ID), automaticMemberSpecID(head.ID, b.ID))
	wantRemote := AgentRPCMemberDTO{HeadSpecID: head.ID, Transport: "direct", BindIP: "100.64.0.5", Port: ports[1], HeadIPs: []string{"100.64.0.2"}}
	if !ok || remote.RPC == nil || !rpcMemberEqual(*remote.RPC, wantRemote) {
		t.Fatalf("remote member rpc = %+v ok=%v, want %+v", remote.RPC, ok, wantRemote)
	}
	plan, err := f.svc.ClusterPlan(f.ctx, head.ID)
	if err != nil || plan.WithheldReason != "" || len(plan.Members) != 2 ||
		plan.Members[0].Address != "127.0.0.1" || plan.Members[1].Address != "100.64.0.5" ||
		!slices.Equal(plan.HeadIdentity, []string{"100.64.0.2"}) {
		t.Fatalf("plan = %+v err=%v", plan, err)
	}

	// In a relay cluster the same-host member still runs direct on
	// loopback; the remote member is relayed.
	req.RPCTransport = "relay"
	f.mustPut(req)
	local, _ = specByID(f.config(f.head.ID), automaticMemberSpecID(head.ID, f.head.ID))
	remote, _ = specByID(f.config(b.ID), automaticMemberSpecID(head.ID, b.ID))
	if local.RPC == nil || local.RPC.Transport != "direct" || local.RPC.BindIP != "127.0.0.1" ||
		remote.RPC == nil || remote.RPC.Transport != "relay" {
		t.Fatalf("relay cluster: same-host rpc = %+v, remote rpc = %+v", local.RPC, remote.RPC)
	}

	// Only same-host members: NetBird off on the head's server and the
	// Default policy seen enabled withhold nothing.
	only := clusterHeadRequest(autoMember(f.head.ID, 1))
	only.ClusterNetwork, only.RPCTransport = "mesh", "direct"
	f.mustPut(only)
	f.updateServer(f.head.ID, func(s *routing.AIServer) { s.NetbirdEnabled, s.NetbirdIP = false, "" })
	if err := f.settings.SetSystemSetting(f.ctx, netbirdDefaultPolicyStateKey, `{"state":"enabled"}`, clusterTestNow); err != nil {
		t.Fatal(err)
	}
	if got := f.withheldReason(); got != "" {
		t.Fatalf("same-host cluster withheld: %q", got)
	}
	hs, ok = specByID(f.config(f.head.ID), head.ID)
	if !ok || hs.Cluster == nil || len(hs.Cluster.Endpoints) != 1 || !strings.HasPrefix(hs.Cluster.Endpoints[0].Address, "127.0.0.1:") {
		t.Fatalf("same-host head = %+v ok=%v", hs.Cluster, ok)
	}
	// A save with NetBird still off on the head's server is not refused
	// either: nothing of the cluster leaves the host.
	only.IdleTimeoutSeconds = 900
	f.mustPut(only)
}

// TestSameHostClusterNeedsNoLANAck: a direct lan cluster whose members all
// run on the head's server exposes nothing on the LAN, so it saves and is
// pushed without the exposure acknowledgement; a remote member still needs
// it.
func TestSameHostClusterNeedsNoLANAck(t *testing.T) {
	f := newClusterFixture(t)
	b := f.member("B")
	f.sameHostAgent(f.head.ID)
	f.addClusterAgent(b.ID)
	req := clusterHeadRequest(autoMember(f.head.ID, 1))
	req.RPCTransport = "direct"
	f.mustPut(req)
	if got := f.withheldReason(); got != "" {
		t.Fatalf("same-host lan cluster withheld: %q", got)
	}
	req.IdleTimeoutSeconds = 900 // an unchanged membership takes the keep path
	f.mustPut(req)
	mixed := clusterHeadRequest(autoMember(f.head.ID, 1), autoMember(b.ID, 0))
	mixed.RPCTransport = "direct"
	if _, err := f.put(ownerToken(), mixed); !errors.Is(err, ErrClusterLANAckRequired) {
		t.Fatalf("remote direct lan member without the acknowledgement: err = %v, want ErrClusterLANAckRequired", err)
	}
}

// TestAgentRuntimeConfigTwoMembersOnOneServer: two members of one head on one
// server each get their own spec (the second with an _m2 id), port, GPUs and
// endpoint, in the member server's document, the head's cluster block and
// the cluster plan.
func TestAgentRuntimeConfigTwoMembersOnOneServer(t *testing.T) {
	f := newClusterFixture(t)
	a := f.member("A")
	f.sameHostAgent(a.ID)
	head := f.mustPut(clusterHeadRequest(autoMember(a.ID, 0), autoMember(a.ID, 1)))
	ports := f.memberPorts(head.ID)
	ids := []string{automaticMemberSpecID(head.ID, a.ID), automaticMemberSpecID(head.ID, a.ID) + "_m2"}

	memberDoc := f.config(a.ID)
	specs := rpcServerSpecs(memberDoc)
	if len(specs) != 2 {
		t.Fatalf("member document rpc specs = %+v, want two", specs)
	}
	for i, id := range ids {
		ms, ok := specByID(memberDoc, id)
		if !ok || ms.RPC == nil || ms.RPC.Port != ports[i] || len(ms.GPUs) != 1 || ms.GPUs[0].Index != i {
			t.Fatalf("member %d (%s) = %+v ok=%v, want port %d and GPU %d", i, id, ms, ok, ports[i], i)
		}
	}
	hs, ok := specByID(f.config(f.head.ID), head.ID)
	wantEP := []AgentClusterEndpointDTO{
		{Address: fmt.Sprintf("A.example.test:%d", ports[0]), DeviceCount: 1},
		{Address: fmt.Sprintf("A.example.test:%d", ports[1]), DeviceCount: 1},
	}
	if !ok || hs.Cluster == nil || !slices.Equal(hs.Cluster.Endpoints, wantEP) {
		t.Fatalf("head cluster = %+v ok=%v, want endpoints %+v", hs.Cluster, ok, wantEP)
	}
	plan, err := f.svc.ClusterPlan(f.ctx, head.ID)
	if err != nil || len(plan.Members) != 2 || plan.Members[0].SpecID != ids[0] || plan.Members[1].SpecID != ids[1] ||
		plan.Members[0].Position != 0 || plan.Members[1].Position != 1 {
		t.Fatalf("plan = %+v err=%v", plan, err)
	}
}

// TestResyncedManualMemberKeepsBackfilledGPU: a manual member's GPU rows
// stored without a vendor and the spec's rows naming the server's legacy
// vendor (a backfill) are one card each, so the measured values stay and no
// run ends; another member of the head on the same server keeps its rows.
func TestResyncedManualMemberKeepsBackfilledGPU(t *testing.T) {
	started := clusterTestNow
	used := routing.ClusterMember{HeadSpecID: "rts_h", Position: 1, ServerID: "srv_a", MemberSpecID: "rpcm_x", RunStartedAt: started, RAMMeasuredMB: 500}
	read := []routing.ClusterMember{
		{HeadSpecID: "rts_h", Position: 0, ServerID: "srv_a", RunStartedAt: started, RAMMeasuredMB: 400},
		used,
	}
	gpus := []routing.ClusterMemberGPU{
		{HeadSpecID: "rts_h", MemberPosition: 0, ServerID: "srv_a", GPUIndex: 0, VRAMMeasuredMB: 1000, RunPeakVRAMMB: 1100},
		{HeadSpecID: "rts_h", MemberPosition: 1, ServerID: "srv_a", GPUIndex: 1, VRAMMeasuredMB: 3000, RunPeakVRAMMB: 3100},
	}
	rows := []routing.RPCMemberSpecGPU{{GPUVendor: "nvidia", GPUIndex: 1}}
	members, next := resyncedManualMember(used, rows, read, gpus, "nvidia")
	if members[0].RunStartedAt.IsZero() || members[1].RunStartedAt.IsZero() || members[1].Stale {
		t.Fatalf("members = %+v: a backfilled vendor ended the runs", members)
	}
	want := []routing.ClusterMemberGPU{
		gpus[0],
		{HeadSpecID: "rts_h", MemberPosition: 1, ServerID: "srv_a", GPUVendor: "nvidia", GPUIndex: 1, VRAMMeasuredMB: 3000, RunPeakVRAMMB: 3100},
	}
	if !slices.Equal(next, want) {
		t.Fatalf("gpu rows = %+v, want %+v", next, want)
	}
	// Another vendor at that index is another card: the distribution changed,
	// every run ends and only the resynced member's kept values go stale.
	members, next = resyncedManualMember(used, []routing.RPCMemberSpecGPU{{GPUVendor: "amd", GPUIndex: 1}}, read, gpus, "nvidia")
	if !members[0].RunStartedAt.IsZero() || !members[1].Stale || members[0].Stale || next[1].VRAMMeasuredMB != 0 {
		t.Fatalf("another card: members = %+v gpus = %+v", members, next)
	}
}
