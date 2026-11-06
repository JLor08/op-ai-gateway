// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"errors"
	"op-ai-gateway/internal/routing"
	"slices"
	"testing"
	"time"
)

// TestClusterPlanFollowsTheDerivation pins the orchestration's view of a head:
// the head's scope, and per member in member order its server, its
// server_agent application, the agent spec id the derivation pushes and that
// spec's startup timeout, plus the network, transport and generation of the
// head's cluster block.
func TestClusterPlanFollowsTheDerivation(t *testing.T) {
	f := newClusterFixture(t)
	a, b := f.member("A"), f.member("B")
	f.clusterAgents(a.ID, b.ID)
	head := f.mustPut(clusterHeadRequest(autoMember(a.ID, 0), autoMember(b.ID)))

	plan, err := f.svc.ClusterPlan(f.ctx, head.ID)
	if err != nil {
		t.Fatalf("ClusterPlan: %v", err)
	}
	if plan.WithheldReason != "" {
		t.Fatalf("withheld = %q, want a derivable cluster", plan.WithheldReason)
	}
	if plan.Head.Spec.ID != head.ID || plan.Head.Server.ID != f.head.ID || plan.Head.Application.ID != f.app.ID || plan.Head.Mapping.ID != f.mappingID {
		t.Fatalf("head scope = %+v", plan.Head)
	}
	hs, ok := specByID(f.config(f.head.ID), head.ID)
	if !ok || hs.Cluster == nil {
		t.Fatal("head document carries no cluster")
	}
	if plan.Network != "lan" || plan.Transport != "relay" || plan.Generation != hs.Cluster.Generation {
		t.Fatalf("plan network/transport/generation = %s/%s/%s, want lan/relay/%s", plan.Network, plan.Transport, plan.Generation, hs.Cluster.Generation)
	}
	if len(plan.Members) != 2 {
		t.Fatalf("members = %+v, want 2", plan.Members)
	}
	for i, want := range []ServerDTO{a, b} {
		m := plan.Members[i]
		if m.Position != i || m.Server.ID != want.ID || m.SpecID != automaticMemberSpecID(head.ID, want.ID) {
			t.Fatalf("member %d = %+v, want server %s spec %s", i, m, want.ID, automaticMemberSpecID(head.ID, want.ID))
		}
		if m.Application.Type != routing.ProviderServerAgent || m.Application.ServerID != want.ID {
			t.Fatalf("member %d application = %+v, want the server's server_agent application", i, m.Application)
		}
		doc, ok := specByID(f.config(want.ID), m.SpecID)
		if !ok || m.StartupTimeoutSeconds != doc.StartupTimeoutSeconds || m.StartupTimeoutSeconds <= 0 {
			t.Fatalf("member %d startup = %d, want the pushed %d", i, m.StartupTimeoutSeconds, doc.StartupTimeoutSeconds)
		}
	}
}

// TestClusterPlanReportsWithheldAndUnknown pins the two ways a plan says the
// cluster cannot start: a withheld derivation names its reason, and a spec
// that is no cluster head is runtime_spec.not_found.
func TestClusterPlanReportsWithheldAndUnknown(t *testing.T) {
	f := newClusterFixture(t)
	a := f.member("A")
	head := f.mustPut(clusterHeadRequest(autoMember(a.ID)))
	plan, err := f.svc.ClusterPlan(f.ctx, head.ID)
	if err != nil || plan.WithheldReason == "" || len(plan.Members) != 0 {
		t.Fatalf("ClusterPlan without agents = (%+v, %v), want a withheld reason and no members", plan, err)
	}
	if plan.Head.Spec.ID != head.ID {
		t.Fatalf("withheld plan head = %+v, want the head's scope", plan.Head)
	}
	if _, err := f.svc.ClusterPlan(f.ctx, "rspec_missing"); !errors.Is(err, ErrRuntimeSpecNotFound) {
		t.Fatalf("ClusterPlan(missing) err = %v, want ErrRuntimeSpecNotFound", err)
	}
}

// TestKeptUpClusterHeadsListsPinnedAndForceRunning pins the heads the
// gateway keeps up on its own: enabled cluster heads that are pinned or
// force_running, nothing else.
func TestKeptUpClusterHeadsListsPinnedAndForceRunning(t *testing.T) {
	f := newClusterFixture(t)
	a := f.member("A")
	f.clusterAgents(a.ID)
	req := clusterHeadRequest(autoMember(a.ID))
	head := f.mustPut(req)
	heads, err := f.svc.KeptUpClusterHeads(f.ctx)
	if err != nil || len(heads) != 0 {
		t.Fatalf("KeptUpClusterHeads(unpinned) = (%v, %v), want none", heads, err)
	}
	req.Pinned = true
	f.mustPut(req)
	heads, err = f.svc.KeptUpClusterHeads(f.ctx)
	if err != nil || !slices.Equal(heads, []string{head.ID}) {
		t.Fatalf("KeptUpClusterHeads(pinned) = (%v, %v), want [%s]", heads, err, head.ID)
	}
	req.Pinned, req.AdminState = false, "force_running"
	f.mustPut(req)
	heads, err = f.svc.KeptUpClusterHeads(f.ctx)
	if err != nil || !slices.Equal(heads, []string{head.ID}) {
		t.Fatalf("KeptUpClusterHeads(force_running) = (%v, %v), want [%s]", heads, err, head.ID)
	}
	req.Enabled = false
	f.mustPut(req)
	if heads, err = f.svc.KeptUpClusterHeads(f.ctx); err != nil || len(heads) != 0 {
		t.Fatalf("KeptUpClusterHeads(disabled) = (%v, %v), want none", heads, err)
	}
}

// fakeClusterStatus is a ClusterStatusReader answering one status per head.
type fakeClusterStatus map[string]ClusterStatusDTO

func (f fakeClusterStatus) ClusterStatus(headSpecID string) ClusterStatusDTO {
	if st, ok := f[headSpecID]; ok {
		return st
	}
	return ClusterStatusDTO{State: ClusterStateCold, Members: []ClusterMemberStatusDTO{}}
}

// TestRuntimeSpecShowsClusterStatus pins the read-only cluster_status on a
// head's document: the orchestration's state as the injected reader reports
// it, positions only; "invalid" while the head is withheld; and nothing on a
// spec that is no head.
func TestRuntimeSpecShowsClusterStatus(t *testing.T) {
	f := newClusterFixture(t)
	a := f.member("A")
	f.clusterAgents(a.ID)
	head := f.mustPut(clusterHeadRequest(autoMember(a.ID)))
	at := clusterTestNow.Add(time.Minute)
	pos := 0
	f.svc.SetClusterStatusReader(fakeClusterStatus{head.ID: {
		State: ClusterStateCoolingDown, Members: []ClusterMemberStatusDTO{{Position: 0, State: "failed", Code: "runtime.admission_blocked"}},
		LastFailure:   &ClusterFailureDTO{Code: "runtime.admission_blocked", Position: &pos, At: at},
		CooldownUntil: &at,
	}})
	dto, err := f.svc.GetRuntimeSpec(f.ctx, ownerToken(), f.mappingID)
	if err != nil {
		t.Fatalf("GetRuntimeSpec: %v", err)
	}
	st := dto.ClusterStatus
	if st == nil || st.State != ClusterStateCoolingDown || len(st.Members) != 1 || st.Members[0].Code != "runtime.admission_blocked" ||
		st.LastFailure == nil || st.LastFailure.Position == nil || *st.LastFailure.Position != 0 || st.CooldownUntil == nil {
		t.Fatalf("cluster_status = %+v, want the reader's", st)
	}

	f.svc.agentFeatures = fakeAgentFeatures{f.head.ID: {"runtime_manager", AgentFeatureRPCCluster}}
	dto, err = f.svc.GetRuntimeSpec(f.ctx, ownerToken(), f.mappingID)
	if err != nil {
		t.Fatalf("GetRuntimeSpec: %v", err)
	}
	if dto.ClusterWithheldReason == "" || dto.ClusterStatus == nil || dto.ClusterStatus.State != ClusterStateInvalid {
		t.Fatalf("withheld head cluster_status = %+v (reason %q), want invalid", dto.ClusterStatus, dto.ClusterWithheldReason)
	}

	other, err := f.svc.CreateMapping(f.ctx, ownerToken(), f.app.ID, CreateMappingRequest{GatewayModelName: "small", AppModelName: "small"})
	if err != nil {
		t.Fatalf("create mapping: %v", err)
	}
	plain := f.putFor(other.ID, PutRuntimeSpecRequest{Enabled: true, Binary: clusterHeadBinary, Args: []string{}})
	if plain.ClusterStatus != nil {
		t.Fatalf("plain spec cluster_status = %+v, want none", plain.ClusterStatus)
	}
}
