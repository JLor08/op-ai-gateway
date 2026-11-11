// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/routing"
	"slices"
	"testing"
	"time"
)

func TestRPCMemberNamerNamesAndCaches(t *testing.T) {
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	calls := 0
	answer := map[string]string{"rpcm_a": "RPC (m)(s)"}
	var failWith error
	n := &rpcMemberNamer{
		resolve: func(_ context.Context, ids []string) (map[string]string, error) {
			calls++
			return answer, failWith
		},
		now: func() time.Time { return now },
	}
	shared := []RuntimeStatusDTO{{SpecID: "spec_model"}, {SpecID: "rpcm_a", Kind: portal.AgentSpecKindRPCServer}}
	ctx := context.Background()

	got := n.apply(ctx, shared)
	if got[1].DisplayName != "RPC (m)(s)" || got[0].DisplayName != "" || calls != 1 {
		t.Fatalf("named = %+v, calls %d", got, calls)
	}
	// The published rows are shared by every subscriber: never written.
	if shared[1].DisplayName != "" {
		t.Fatal("the shared frame was modified")
	}
	// The same members within the TTL: no new lookup.
	n.apply(ctx, shared)
	if calls != 1 {
		t.Fatalf("calls within the TTL = %d", calls)
	}
	// The first frame a TTL after the last lookup looks up again: a rename shows.
	now = now.Add(rpcMemberNamesTTL)
	answer = map[string]string{"rpcm_a": "RPC (renamed)(s)"}
	if got := n.apply(ctx, shared); got[1].DisplayName != "RPC (renamed)(s)" || calls != 2 {
		t.Fatalf("after the TTL = %q, calls %d", got[1].DisplayName, calls)
	}
	// A new member set looks up at once; a failure keeps the names known.
	failWith = errors.New("store down")
	more := append(slices.Clone(shared), RuntimeStatusDTO{SpecID: "rpcm_b", Kind: portal.AgentSpecKindRPCServer})
	if got := n.apply(ctx, more); got[1].DisplayName != "RPC (renamed)(s)" || got[2].DisplayName != "" || calls != 3 {
		t.Fatalf("after a failed lookup = %+v, calls %d", got, calls)
	}
	// Inside the back-off a frame makes no call.
	now = now.Add(time.Second)
	n.apply(ctx, more)
	if calls != 3 {
		t.Fatalf("calls inside the back-off = %d", calls)
	}
	// After it, one more try; still failing, the names stay until the TTL
	// since the last successful lookup has passed, and then go.
	now = now.Add(rpcMemberNamesRetry)
	if got := n.apply(ctx, more); got[1].DisplayName != "RPC (renamed)(s)" || calls != 4 {
		t.Fatalf("after the back-off = %q, calls %d", got[1].DisplayName, calls)
	}
	now = now.Add(rpcMemberNamesTTL - rpcMemberNamesRetry - time.Second)
	if got := n.apply(ctx, more); got[1].DisplayName != "" {
		t.Fatalf("names outlived the TTL: %q", got[1].DisplayName)
	}
	// A frame without a member is passed through untouched.
	plain := []RuntimeStatusDTO{{SpecID: "spec_model"}}
	if got := n.apply(ctx, plain); &got[0] != &plain[0] {
		t.Fatal("a frame without members was copied")
	}
}

// A reader who lost the server loses the names at once.
func TestRPCMemberNamerDropsNamesWhenTheServerIsRefused(t *testing.T) {
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	var failWith error
	n := &rpcMemberNamer{
		resolve: func(_ context.Context, ids []string) (map[string]string, error) {
			return map[string]string{"rpcm_a": "RPC (m)(s)"}, failWith
		},
		now: func() time.Time { return now },
	}
	rows := []RuntimeStatusDTO{{SpecID: "rpcm_a", Kind: portal.AgentSpecKindRPCServer}}
	if got := n.apply(context.Background(), rows); got[0].DisplayName == "" {
		t.Fatal("no name")
	}
	failWith = portal.ErrServerNotFound
	now = now.Add(rpcMemberNamesTTL)
	if got := n.apply(context.Background(), rows); got[0].DisplayName != "" {
		t.Fatalf("name kept after the server was refused: %q", got[0].DisplayName)
	}
}

// The status stream carries a member's display name, resolved for its reader.
func TestRuntimeEventsNameRPCMembers(t *testing.T) {
	srv := newRuntimeEventsTestServer(t)
	if err := srv.Routes.UpsertRPCMemberSpec(context.Background(), routing.RPCMemberSpec{
		ID: "rpcm_free", ServerID: runtimeEventsServerID, Enabled: true, Binary: "/opt/llama/ggml-rpc-server", Args: "[]", Env: "{}",
	}); err != nil {
		t.Fatal(err)
	}
	srv.RuntimeStatus.publish(runtimeEventsServerID, []RuntimeStatusDTO{
		{SpecID: "rpcm_free", State: "running", Kind: portal.AgentSpecKindRPCServer},
		{SpecID: "spec-1", State: "running"},
	})
	ts := httptest.NewServer(srv)
	defer ts.Close()
	req, err := http.NewRequest(http.MethodGet, ts.URL+"/api/portal/servers/"+runtimeEventsServerID+"/runtime/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+runtimeEventsOwnerSecret)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	event, data := readPerfSSEFrame(t, bufio.NewReader(resp.Body), 3*time.Second)
	if event != "snapshot" {
		t.Fatalf("event = %q", event)
	}
	var snap runtimeStatusEventDTO
	if err := json.Unmarshal([]byte(data), &snap); err != nil {
		t.Fatal(err)
	}
	if snap.Runtimes[0].DisplayName != "RPC (ggml-rpc-server)(Runtime Events)" || snap.Runtimes[1].DisplayName != "" {
		t.Fatalf("snapshot = %+v", snap.Runtimes)
	}
}

// Two readers of one server with different rights share one published frame:
// each gets the names it may see, and neither sees the other's.
func TestRuntimeEventsNameMembersPerReader(t *testing.T) {
	srv := newRuntimeEventsTestServer(t)
	ctx := context.Background()
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	// The member server is shared by both readers; the head's is the owner's.
	must(srv.Routes.SetServerOwners(ctx, runtimeEventsServerID, []string{"usr_owner", "usr_other"}))
	must(srv.Routes.CreateAIServer(ctx, routing.AIServer{
		ID: "srv_head", Name: "Head", Domain: "head.example.test", Provider: routing.ProviderMock,
		Endpoint: "mock://head", Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now,
	}))
	must(srv.Routes.SetServerOwners(ctx, "srv_head", []string{"usr_owner"}))
	must(srv.Routes.CreateApplication(ctx, routing.Application{ID: "app_head", ServerID: "srv_head", Type: routing.ProviderServerAgent, Port: 8081, Scheme: "http", APIFlavors: []string{routing.APIFlavorOpenAI}, Priority: 1, Weight: 1, TimeoutMS: 30000, AffinityTTLSeconds: 300, Status: routing.ServerStatusActive, HealthCheckMode: routing.HealthCheckModeAlwaysReachable, CreatedAt: now, UpdatedAt: now}))
	must(srv.Routes.CreateMapping(ctx, routing.ModelMapping{ID: "map_head", ApplicationID: "app_head", GatewayModelName: "big-model", AppModelName: "u", Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}))
	must(srv.Routes.UpsertRuntimeSpec(ctx, routing.RuntimeSpec{ID: "rspec_head", MappingID: "map_head", Enabled: true, Binary: "/opt/llama/llama-server", Args: "[]", Env: "{}", ClusterEnabled: true, CreatedAt: now, UpdatedAt: now}))
	must(srv.Routes.SetClusterMembers(ctx, "rspec_head", []routing.ClusterMember{{Position: 0, ServerID: runtimeEventsServerID, Port: 26000}}, nil))
	member := "rpca_rspec_head_" + runtimeEventsServerID

	// Closed by Cleanup, after the streams' bodies (registered later): a
	// server closing with a stream still open waits for it.
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	open := func(secret string) *bufio.Reader {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, ts.URL+"/api/portal/servers/"+runtimeEventsServerID+"/runtime/events", nil)
		must(err)
		req.Header.Set("Authorization", "Bearer "+secret)
		resp, err := http.DefaultClient.Do(req)
		must(err)
		t.Cleanup(func() { resp.Body.Close() })
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d", resp.StatusCode)
		}
		reader := bufio.NewReader(resp.Body)
		if event, _ := readPerfSSEFrame(t, reader, 3*time.Second); event != "snapshot" {
			t.Fatalf("first event = %q", event)
		}
		return reader
	}
	owner, other := open(runtimeEventsOwnerSecret), open(runtimeEventsOtherSecret)

	srv.RuntimeStatus.publish(runtimeEventsServerID, []RuntimeStatusDTO{{SpecID: member, State: "running", Kind: portal.AgentSpecKindRPCServer}})
	nameIn := func(reader *bufio.Reader) string {
		t.Helper()
		event, data := readPerfSSEFrame(t, reader, 3*time.Second)
		if event != "update" {
			t.Fatalf("event = %q", event)
		}
		var frame runtimeStatusEventDTO
		must(json.Unmarshal([]byte(data), &frame))
		return frame.Runtimes[0].DisplayName
	}
	if got := nameIn(owner); got != "RPC (big-model)(Runtime Events)" {
		t.Fatalf("owner sees %q", got)
	}
	if got := nameIn(other); got != "RPC (—)(Runtime Events)" {
		t.Fatalf("the reader without the head sees %q", got)
	}
}
