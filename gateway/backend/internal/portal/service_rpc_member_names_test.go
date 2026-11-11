// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"maps"
	"op-ai-gateway/internal/routing"
	"sync/atomic"
	"testing"
)

func TestRPCMemberDisplayNamesAutomaticAndRename(t *testing.T) {
	f := newClusterFixture(t)
	efyga := f.member("efyga")
	head := f.mustPut(clusterHeadRequest(autoMember(efyga.ID)))
	id := automaticMemberAgentSpecID(head.ID, efyga.ID)

	names, err := f.svc.RPCMemberDisplayNames(f.ctx, ownerToken(), efyga.ID, []string{id, "spec_model"})
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]string{id: "RPC (big)(Head)"}; !maps.Equal(names, want) {
		t.Fatalf("names = %v, want %v (the head's model and server; a model spec gets no name)", names, want)
	}

	// Read at every call: a renamed model and a renamed head server show at
	// once. The member's own server is not part of the name.
	model, headName, memberName := "unsloth/Qwen3.8-27B", "Head-2", "efyga-2"
	if _, err := f.svc.UpdateMapping(f.ctx, ownerToken(), f.mappingID, UpdateMappingRequest{GatewayModelName: &model}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.UpdateServer(f.ctx, ownerToken(), f.head.ID, UpdateServerRequest{Name: &headName}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.UpdateServer(f.ctx, ownerToken(), efyga.ID, UpdateServerRequest{Name: &memberName}); err != nil {
		t.Fatal(err)
	}
	names, err = f.svc.RPCMemberDisplayNames(f.ctx, ownerToken(), efyga.ID, []string{id})
	if err != nil {
		t.Fatal(err)
	}
	if got := names[id]; got != "RPC (unsloth/Qwen3.8-27B)(Head-2)" {
		t.Fatalf("after rename = %q", got)
	}
}

func TestRPCMemberDisplayNamesManual(t *testing.T) {
	f := newClusterFixture(t)
	srv := f.member("M")
	used := f.manualSpec(srv.ID)
	free := f.manualSpec(srv.ID)
	req := clusterHeadRequest(ClusterMemberDTO{ServerID: srv.ID, MemberSpecID: used.ID})
	req.MemberMode = "manual"
	f.mustPut(req)

	elsewhere := f.manualSpec(f.member("N").ID)

	names, err := f.svc.RPCMemberDisplayNames(f.ctx, ownerToken(), srv.ID, []string{used.ID, free.ID, elsewhere.ID})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		used.ID: "RPC (big)(Head)",
		// No head uses it: its binary's file name stands in for the head.
		free.ID: "RPC (ggml-rpc-server)",
		// A spec of another server is no member here and gets no name.
	}
	if !maps.Equal(names, want) {
		t.Fatalf("names = %v, want %v", names, want)
	}
}

// An automatic member whose membership row is gone still names its head
// from its id; a head whose spec is gone shows as "RPC (—)".
func TestRPCMemberDisplayNamesWithoutAMembershipRow(t *testing.T) {
	f := newClusterFixture(t)
	efyga := f.member("efyga")
	head := f.mustPut(clusterHeadRequest(autoMember(efyga.ID)))
	if err := f.routes.SetClusterMembers(f.ctx, head.ID, nil, nil); err != nil {
		t.Fatal(err)
	}
	ids := []string{automaticMemberAgentSpecID(head.ID, efyga.ID), automaticMemberAgentSpecID("rspec_gone", efyga.ID)}
	names, err := f.svc.RPCMemberDisplayNames(f.ctx, ownerToken(), efyga.ID, ids)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{ids[0]: "RPC (big)(Head)", ids[1]: "RPC (—)"}
	if !maps.Equal(names, want) {
		t.Fatalf("names = %v, want %v", names, want)
	}
}

// A reader who may see the member server but not the head's mapping learns
// nothing about the head: neither its model nor its server.
func TestRPCMemberDisplayNamesHideAHeadTheReaderMayNotSee(t *testing.T) {
	f := newClusterFixture(t)
	foreign := f.foreignServer("Foreign")
	head, err := f.put(systemToken(), clusterHeadRequest(autoMember(foreign.ID)))
	if err != nil {
		t.Fatal(err)
	}
	id := automaticMemberAgentSpecID(head.ID, foreign.ID)

	names, err := f.svc.RPCMemberDisplayNames(f.ctx, otherToken(), foreign.ID, []string{id})
	if err != nil {
		t.Fatal(err)
	}
	if got := names[id]; got != "RPC (—)" {
		t.Fatalf("hidden head name = %q", got)
	}
	// The member server itself is the reader's boundary.
	if _, err := f.svc.RPCMemberDisplayNames(f.ctx, otherToken(), f.head.ID, []string{id}); err == nil {
		t.Fatal("a server the reader may not see must be refused")
	}
}

// countingRoutes counts the store reads one resolution makes.
type countingRoutes struct {
	routing.Store
	memberships, heads, manual atomic.Int32
}

func (c *countingRoutes) RPCMemberSpecsByServer(ctx context.Context, serverID string) ([]routing.RPCMemberSpec, error) {
	c.manual.Add(1)
	return c.Store.RPCMemberSpecsByServer(ctx, serverID)
}

func (c *countingRoutes) ClusterMembershipsByServer(ctx context.Context, serverID string) ([]routing.ClusterMember, error) {
	c.memberships.Add(1)
	return c.Store.ClusterMembershipsByServer(ctx, serverID)
}

func (c *countingRoutes) RuntimeSpecByID(ctx context.Context, id string) (routing.RuntimeSpec, bool, error) {
	c.heads.Add(1)
	return c.Store.RuntimeSpecByID(ctx, id)
}

// One call reads the server's membership once, its manual member specs at
// most once, and each head once, however many rows name them.
func TestRPCMemberDisplayNamesReadEachHeadOnce(t *testing.T) {
	f := newClusterFixture(t)
	efyga := f.member("efyga")
	head := f.mustPut(clusterHeadRequest(autoMember(efyga.ID)))
	other := seedOtherHead(t, f)
	if err := f.routes.SetClusterMembers(f.ctx, other, []routing.ClusterMember{{Position: 0, ServerID: efyga.ID, Port: 26200}}, nil); err != nil {
		t.Fatal(err)
	}
	freeA, freeB := f.manualSpec(efyga.ID), f.manualSpec(efyga.ID)
	ids := []string{
		automaticMemberAgentSpecID(head.ID, efyga.ID),
		automaticMemberAgentSpecID(other, efyga.ID),
		automaticMemberAgentSpecID(head.ID, efyga.ID),
		freeA.ID,
		freeB.ID,
	}
	counting := &countingRoutes{Store: f.routes}
	f.svc.routes = counting
	names, err := f.svc.RPCMemberDisplayNames(f.ctx, ownerToken(), efyga.ID, ids)
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 4 || names[ids[1]] != "RPC (other)(Other head)" {
		t.Fatalf("names = %v", names)
	}
	if m, h, s := counting.memberships.Load(), counting.heads.Load(), counting.manual.Load(); m != 1 || h != 2 || s != 1 {
		t.Fatalf("store reads: memberships %d, heads %d, manual specs %d; want 1, 2 and 1", m, h, s)
	}
}
