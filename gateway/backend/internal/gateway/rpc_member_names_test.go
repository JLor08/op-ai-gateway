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

	got := n.apply(context.Background(), shared)
	if got[1].DisplayName != "RPC (m)(s)" || got[0].DisplayName != "" || calls != 1 {
		t.Fatalf("named = %+v, calls %d", got, calls)
	}
	// The published rows are shared by every subscriber: never written.
	if shared[1].DisplayName != "" {
		t.Fatal("the shared frame was modified")
	}
	// The same members within the TTL: no new resolution.
	n.apply(context.Background(), shared)
	if calls != 1 {
		t.Fatalf("calls within the TTL = %d", calls)
	}
	// After the TTL a rename shows; a failed resolution keeps what is known.
	now = now.Add(rpcMemberNamesTTL)
	answer = map[string]string{"rpcm_a": "RPC (renamed)(s)"}
	if got := n.apply(context.Background(), shared); got[1].DisplayName != "RPC (renamed)(s)" || calls != 2 {
		t.Fatalf("after the TTL = %q, calls %d", got[1].DisplayName, calls)
	}
	failWith = errors.New("store down")
	more := append(shared, RuntimeStatusDTO{SpecID: "rpcm_b", Kind: portal.AgentSpecKindRPCServer})
	if got := n.apply(context.Background(), more); got[1].DisplayName != "RPC (renamed)(s)" || got[2].DisplayName != "" || calls != 3 {
		t.Fatalf("after a failed resolution = %+v, calls %d", got, calls)
	}
	// A frame without a member is passed through untouched.
	plain := []RuntimeStatusDTO{{SpecID: "spec_model"}}
	if got := n.apply(context.Background(), plain); &got[0] != &plain[0] || calls != 3 {
		t.Fatal("a frame without members was copied or resolved")
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
