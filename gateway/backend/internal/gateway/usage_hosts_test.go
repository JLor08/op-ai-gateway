// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/inference"
	"op-ai-gateway/internal/provider"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/usage"
	"testing"
	"time"
)

// knownClusterHead answers the completion-time lookup from the same table.
func (f fakeClusterHeads) knownClusterHead(ctx context.Context, serverID, mappingID string) (string, bool) {
	return f.clusterHead(ctx, serverID, mappingID)
}

// specIsClusterHead answers the reconciler's check from the same table.
func (f fakeClusterHeads) specIsClusterHead(_ context.Context, mappingID string) (head, known bool) {
	_, head = f[mappingID]
	return head, true
}

// TestRecordUsageListsClusterMembers pins that a request served by a cluster
// head lists, when it completes, the head and every member the head held at
// that moment (from the session records), so each host's energy part can be
// priced and every member host's overlap reads see the request. A request on a
// mapping that is no head lists nothing.
func TestRecordUsageListsClusterMembers(t *testing.T) {
	srv := NewTestServer()
	srv.clusterHeads = fakeClusterHeads{"map_head": "rspec_head"}
	ctx := context.Background()
	now := time.Now().UTC()
	closed := now.Add(-time.Minute)
	for _, s := range []routing.RPCSession{
		{HeadSpecID: "rspec_head", MemberServerID: "srv-a", Since: now.Add(-time.Hour)},
		{HeadSpecID: "rspec_head", MemberServerID: "srv-b", Since: now.Add(-time.Hour)},
		{HeadSpecID: "rspec_head", MemberServerID: "srv-gone", Since: now.Add(-time.Hour), Until: &closed},
		{HeadSpecID: "rspec_other", MemberServerID: "srv-c", Since: now.Add(-time.Hour)},
	} {
		if err := srv.Routes.OpenRPCSession(ctx, s); err != nil {
			t.Fatal(err)
		}
	}

	srv.recordUsage(time.Now(), auth.Token{UserID: "usr_x"}, inference.Request{Model: "m"}, routing.Target{RouteID: "map_head", ServerID: "srv-head"}, provider.Response{}, "", "success", usageMeta{}, "req_cluster", nil, nil)
	srv.recordUsage(time.Now(), auth.Token{UserID: "usr_x"}, inference.Request{Model: "m"}, routing.Target{RouteID: "map_plain", ServerID: "srv-head"}, provider.Response{}, "", "success", usageMeta{}, "req_plain", nil, nil)

	hosts, err := srv.Usage.EventHosts(ctx, "req_cluster")
	if err != nil {
		t.Fatal(err)
	}
	want := []usage.EventHost{
		{EventID: "req_cluster", ServerID: "srv-head", Role: usage.EventHostRoleHead},
		{EventID: "req_cluster", ServerID: "srv-a", Role: usage.EventHostRoleMember},
		{EventID: "req_cluster", ServerID: "srv-b", Role: usage.EventHostRoleMember},
	}
	if len(hosts) != len(want) {
		t.Fatalf("hosts = %+v, want %+v", hosts, want)
	}
	events := srv.Usage.ByUser("usr_x")
	var created time.Time
	for _, e := range events {
		if e.ID == "req_cluster" {
			created = e.CreatedAt
		}
	}
	for i, h := range hosts {
		if h.EventID != want[i].EventID || h.ServerID != want[i].ServerID || h.Role != want[i].Role || !h.CreatedAt.Equal(created) {
			t.Fatalf("host %d = %+v, want %+v at the event's created_at %v", i, h, want[i], created)
		}
	}
	if plain, err := srv.Usage.EventHosts(ctx, "req_plain"); err != nil || len(plain) != 0 {
		t.Fatalf("plain request hosts = %+v err=%v, want none", plain, err)
	}
}
