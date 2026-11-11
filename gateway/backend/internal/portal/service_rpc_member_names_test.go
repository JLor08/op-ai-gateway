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
	if want := map[string]string{id: "RPC (big)(efyga)"}; !maps.Equal(names, want) {
		t.Fatalf("names = %v, want %v (a model spec gets no name)", names, want)
	}

	// Read at every call: a renamed model and a renamed server show at once.
	model, serverName := "unsloth/Qwen3.8-27B", "efyga-2"
	if _, err := f.svc.UpdateMapping(f.ctx, ownerToken(), f.mappingID, UpdateMappingRequest{GatewayModelName: &model}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.UpdateServer(f.ctx, ownerToken(), efyga.ID, UpdateServerRequest{Name: &serverName}); err != nil {
		t.Fatal(err)
	}
	names, err = f.svc.RPCMemberDisplayNames(f.ctx, ownerToken(), efyga.ID, []string{id})
	if err != nil {
		t.Fatal(err)
	}
	if got := names[id]; got != "RPC (unsloth/Qwen3.8-27B)(efyga-2)" {
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

	names, err := f.svc.RPCMemberDisplayNames(f.ctx, ownerToken(), srv.ID, []string{used.ID, free.ID})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		used.ID: "RPC (big)(M)",
		// No head uses it: its binary's file name stands in for the head.
		free.ID: "RPC (ggml-rpc-server)(M)",
	}
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
	if got := names[id]; got != "RPC (—)(Foreign)" {
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
	memberships, heads atomic.Int32
}

func (c *countingRoutes) ClusterMembershipsByServer(ctx context.Context, serverID string) ([]routing.ClusterMember, error) {
	c.memberships.Add(1)
	return c.Store.ClusterMembershipsByServer(ctx, serverID)
}

func (c *countingRoutes) RuntimeSpecByID(ctx context.Context, id string) (routing.RuntimeSpec, bool, error) {
	c.heads.Add(1)
	return c.Store.RuntimeSpecByID(ctx, id)
}

// One call reads the server's membership once and each head once, however
// many rows name them.
func TestRPCMemberDisplayNamesReadEachHeadOnce(t *testing.T) {
	f := newClusterFixture(t)
	efyga := f.member("efyga")
	head := f.mustPut(clusterHeadRequest(autoMember(efyga.ID)))
	other := seedOtherHead(t, f)
	if err := f.routes.SetClusterMembers(f.ctx, other, []routing.ClusterMember{{Position: 0, ServerID: efyga.ID, Port: 26200}}, nil); err != nil {
		t.Fatal(err)
	}
	ids := []string{
		automaticMemberAgentSpecID(head.ID, efyga.ID),
		automaticMemberAgentSpecID(other, efyga.ID),
		automaticMemberAgentSpecID(head.ID, efyga.ID),
	}
	counting := &countingRoutes{Store: f.routes}
	f.svc.routes = counting
	names, err := f.svc.RPCMemberDisplayNames(f.ctx, ownerToken(), efyga.ID, ids)
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 2 || names[ids[1]] != "RPC (other)(efyga)" {
		t.Fatalf("names = %v", names)
	}
	if m, h := counting.memberships.Load(), counting.heads.Load(); m != 1 || h != 2 {
		t.Fatalf("store reads: memberships %d, heads %d; want 1 and 2", m, h)
	}
}
