// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/portal"
	"slices"
	"sync"
	"testing"
	"time"
)

// clusterNotifySpy wraps a portal.API and records NotifyClustersOfServer.
type clusterNotifySpy struct {
	portal.API
	mu      sync.Mutex
	servers []string
}

func (p *clusterNotifySpy) NotifyClustersOfServer(ctx context.Context, serverID string) {
	p.mu.Lock()
	p.servers = append(p.servers, serverID)
	p.mu.Unlock()
	if p.API != nil {
		p.API.NotifyClustersOfServer(ctx, serverID)
	}
}

func (p *clusterNotifySpy) take() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := p.servers
	p.servers = nil
	return out
}

func TestAgentFeaturesDeclaresRPCCluster(t *testing.T) {
	if rpcClusterFeature != "rpc_cluster" {
		t.Fatalf("rpcClusterFeature = %q, want the wire literal rpc_cluster", rpcClusterFeature)
	}
	if !slices.Contains(gatewayAgentFeatures, rpcClusterFeature) {
		t.Fatalf("gatewayAgentFeatures = %v, want it to declare %q", gatewayAgentFeatures, rpcClusterFeature)
	}
}

// TestIngestNotifiesClustersWhenRPCClusterFlips: whether a cluster is pushed
// depends on every agent of it declaring rpc_cluster, and every document of it
// is held while one declaration is unknown, so a sample that makes the
// declaration known or changes rpc_cluster pushes every server sharing a
// cluster with the agent's server.
func TestIngestNotifiesClustersWhenRPCClusterFlips(t *testing.T) {
	srv := NewTestServer()
	spy := &clusterNotifySpy{API: srv.Portal}
	srv.Portal = spy
	ingestCaps := func(t *testing.T, capabilities string) {
		t.Helper()
		req, raw := ingestReq(t, `{"host":{"cpu_util_pct":1},"capabilities":`+capabilities+`}`)
		if err := srv.ingestTelemetrySample(context.Background(), "mock-host-qwen", req, raw); err != nil {
			t.Fatalf("ingest: %v", err)
		}
	}
	steps := []struct {
		caps string
		want bool
		name string
	}{
		{`{"features":["runtime_manager"]}`, true, "a first sample, which makes the declaration known"},
		{`{"features":["runtime_manager"]}`, false, "the same declaration again"},
		{`{"features":["runtime_manager","rpc_cluster"]}`, true, "rpc_cluster declared"},
		{`{"features":["runtime_manager","rpc_cluster","runtime_instances"]}`, false, "another feature only"},
		{`"not-an-object"`, false, "an unreadable declaration"},
		{`{"features":["runtime_manager","rpc_cluster"]}`, true, "readable again"},
		{`{"features":["runtime_manager"]}`, true, "rpc_cluster withdrawn"},
	}
	for _, step := range steps {
		ingestCaps(t, step.caps)
		got := spy.take()
		if step.want && !slices.Equal(got, []string{"mock-host-qwen"}) {
			t.Fatalf("%s: cluster notifications %v, want one for the server", step.name, got)
		}
		if !step.want && len(got) != 0 {
			t.Fatalf("%s: cluster notifications %v, want none", step.name, got)
		}
	}
}

// TestIngestBinaryInventoryNotifiesClusters: an inventory write moves an
// automatic member's binary and every member's version in its head's
// generation, so it notifies the server's clusters; a sample without the
// field writes nothing and notifies nothing.
func TestIngestBinaryInventoryNotifiesClusters(t *testing.T) {
	srv := NewTestServer()
	spy := &clusterNotifySpy{API: srv.Portal}
	srv.Portal = spy
	ctx := context.Background()
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	srv.writeBackBinaries(ctx, "mock-host-qwen", nil, now)
	if got := spy.take(); len(got) != 0 {
		t.Fatalf("absent inventory notified %v", got)
	}
	reported := []agentBinarySample{{Path: "/opt/llama/ggml-rpc-server", Kind: "rpc_server", Allowed: true}}
	srv.writeBackBinaries(ctx, "mock-host-qwen", &reported, now)
	if got := spy.take(); !slices.Equal(got, []string{"mock-host-qwen"}) {
		t.Fatalf("inventory write notified %v, want the server", got)
	}
}

// TestIngestStartupWriteOfAClusterHeadPushesTheRuntimeConfig: a cluster
// head's pushed idle timeout is derived from its measured startup like a
// replicable spec's, so the startup write-back pushes its document.
func TestIngestStartupWriteOfAClusterHeadPushesTheRuntimeConfig(t *testing.T) {
	srv := NewTestServer()
	seedRuntimeIngestSpec(t, srv, "rspec_head")
	ctx := context.Background()
	head, _, err := srv.Routes.RuntimeSpecByID(ctx, "rspec_head")
	if err != nil {
		t.Fatalf("RuntimeSpecByID: %v", err)
	}
	head.ClusterEnabled = true
	if err := srv.Routes.UpsertRuntimeSpec(ctx, head); err != nil {
		t.Fatalf("make the spec a head: %v", err)
	}
	spy := spyRuntimeConfigPushes(srv)
	const features = `"runtime_manager"`
	ingestInstanceRows(t, srv, features)
	spy.expectPush(t, "the first declaration", "mock-host-qwen")
	ingestInstanceRows(t, srv, features, instanceRow("rspec_head", 0, "running", `,"startup_ms":30000`))
	spy.expectPush(t, "a startup write of a cluster head", "mock-host-qwen")
}

func TestPortalServerDeleteOfAClusterMemberIsAConflict(t *testing.T) {
	rec := httptest.NewRecorder()
	writePortalServerError(rec, &portal.RPCMemberInUseError{HeadServerName: "Head"}, codeServerUpdateFailed)
	if rec.Code != http.StatusConflict || rpcMemberErrCode(t, rec) != "runtime.cluster_member_in_use" {
		t.Fatalf("delete in-use member = %d %s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	writePortalServerError(rec, portal.ErrRPCMemberInUse, codeServerUpdateFailed)
	if rec.Code != http.StatusConflict || rpcMemberErrCode(t, rec) != "runtime.cluster_member_in_use" {
		t.Fatalf("bare in-use = %d %s", rec.Code, rec.Body.String())
	}
}
