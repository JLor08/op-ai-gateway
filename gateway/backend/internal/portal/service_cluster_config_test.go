// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
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
