// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/routing"
	"testing"
	"time"
)

// TestPortalClusterErrorRows pins the status and code of every cluster save
// refusal on the runtime-spec mapper, and that a refusal's detail reaches the
// message.
func TestPortalClusterErrorRows(t *testing.T) {
	cases := []struct {
		err    error
		status int
		code   string
	}{
		{portal.ErrClusterMemberModeInvalid, http.StatusBadRequest, "runtime_spec.cluster_member_mode_invalid"},
		{portal.ErrClusterNetworkInvalid, http.StatusBadRequest, "runtime_spec.cluster_network_invalid"},
		{portal.ErrClusterTransportInvalid, http.StatusBadRequest, "runtime_spec.cluster_transport_invalid"},
		{portal.ErrRPCMemberDeviceBackendInvalid, http.StatusBadRequest, "runtime_spec.device_backend_invalid"},
		{portal.ErrClusterTypeInvalid, http.StatusBadRequest, "runtime_spec.cluster_type_invalid"},
		{portal.ErrClusterRPCPlaceholderMissing, http.StatusBadRequest, "runtime_spec.cluster_rpc_placeholder_missing"},
		{portal.ErrClusterRPCTwice, http.StatusBadRequest, "runtime_spec.cluster_rpc_twice"},
		{portal.ErrClusterRPCPlaceholderWithoutCluster, http.StatusBadRequest, "runtime_spec.cluster_rpc_placeholder_without_cluster"},
		{portal.ErrClusterDeviceListMissingRPC, http.StatusBadRequest, "runtime_spec.cluster_device_list_missing_rpc"},
		{portal.ErrClusterDeviceOrder, http.StatusBadRequest, "runtime_spec.cluster_device_order"},
		{portal.ErrClusterMembersRequired, http.StatusBadRequest, "runtime_spec.cluster_members_required"},
		{portal.ErrClusterHeadReplicable, http.StatusBadRequest, "runtime_spec.cluster_head_replicable"},
		{portal.ErrClusterStartupTimeoutRequired, http.StatusBadRequest, "runtime_spec.cluster_startup_timeout_required"},
		{portal.ErrClusterMemberServerTwice, http.StatusBadRequest, "runtime_spec.cluster_member_server_twice"},
		{portal.ErrClusterMemberOnHeadServer, http.StatusBadRequest, "runtime_spec.cluster_member_on_head_server"},
		{portal.ErrServerNotFound, http.StatusNotFound, "server.not_found"},
		{portal.ErrRPCMemberSpecNotFound, http.StatusNotFound, "rpc_member_spec.not_found"},
		{portal.ErrClusterMemberSpecTaken, http.StatusConflict, "runtime_spec.cluster_member_spec_taken"},
		{portal.ErrClusterMemberSpecDisabled, http.StatusBadRequest, "runtime_spec.cluster_member_spec_disabled"},
		{portal.ErrClusterMemberRPCDisabled, http.StatusBadRequest, "runtime_spec.cluster_member_rpc_disabled"},
		{portal.ErrClusterMemberNoMatchingBinary, http.StatusBadRequest, "runtime_spec.cluster_member_no_matching_binary"},
		{portal.ErrClusterVersionMismatch, http.StatusBadRequest, "runtime_spec.cluster_version_mismatch"},
		{portal.ErrClusterMeshUnavailable, http.StatusBadRequest, "runtime_spec.cluster_mesh_unavailable"},
		{portal.ErrClusterLANAddressRequired, http.StatusBadRequest, "runtime_spec.cluster_lan_address_required"},
		{portal.ErrClusterDirectUnavailable, http.StatusBadRequest, "runtime_spec.cluster_direct_unavailable"},
		{portal.ErrClusterLANAckRequired, http.StatusBadRequest, "runtime.cluster_lan_ack_required"},
		{portal.ErrClusterMemberBudgetRequired, http.StatusBadRequest, "runtime_spec.cluster_member_budget_required"},
		{portal.ErrRPCMemberVRAMEstimateRequired, http.StatusBadRequest, "runtime_spec.member_vram_estimate_required"},
		{portal.ErrClusterTensorSplitRequired, http.StatusBadRequest, "runtime_spec.cluster_tensor_split_required"},
		{portal.ErrClusterPortExhausted, http.StatusConflict, "runtime_spec.cluster_port_exhausted"},
	}
	for _, tc := range cases {
		rec := httptest.NewRecorder()
		writePortalRuntimeSpecError(rec, tc.err)
		if rec.Code != tc.status || rpcMemberErrCode(t, rec) != tc.code {
			t.Errorf("%v: %d %s, want %d %s", tc.err, rec.Code, rec.Body.String(), tc.status, tc.code)
		}
		if tc.code != tc.err.Error() {
			t.Errorf("code %q is not the sentinel's text %q", tc.code, tc.err.Error())
		}
	}
	rec := httptest.NewRecorder()
	writePortalRuntimeSpecError(rec, fmt.Errorf("%w: server %q has no ggml-rpc-server of version b1", portal.ErrClusterMemberNoMatchingBinary, "gpu-2"))
	var body struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Error.Message != `server "gpu-2" has no ggml-rpc-server of version b1` {
		t.Fatalf("detail message = %q err=%v", body.Error.Message, err)
	}
}

func TestHandlePortalClusterCandidates(t *testing.T) {
	srv := NewTestServer()
	ctx := context.Background()
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	serverID := seedGPUBudgetServer(t, srv)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(srv.Routes.CreateApplication(ctx, routing.Application{ID: "app_head", ServerID: serverID, Type: routing.ProviderServerAgent, Port: 8081, Scheme: "http", APIFlavors: []string{routing.APIFlavorOpenAI}, Priority: 1, Weight: 1, TimeoutMS: 30000, AffinityTTLSeconds: 300, Status: routing.ServerStatusActive, HealthCheckMode: routing.HealthCheckModeAlwaysReachable, CreatedAt: now, UpdatedAt: now}))
	must(srv.Routes.CreateMapping(ctx, routing.ModelMapping{ID: "map_head", ApplicationID: "app_head", GatewayModelName: "m", AppModelName: "u", Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}))
	base := "/api/portal/mappings/map_head/runtime/cluster-candidates"

	rec := rpcMemberDo(t, srv, http.MethodGet, base+"?mode=manual", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list = %d %s", rec.Code, rec.Body.String())
	}
	var list struct {
		Data []portal.ClusterCandidateDTO `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil || list.Data == nil {
		t.Fatalf("body %s err=%v, want a data array", rec.Body.String(), err)
	}
	rec = rpcMemberDo(t, srv, http.MethodGet, base+"?mode=sideways", "")
	if rec.Code != http.StatusBadRequest || rpcMemberErrCode(t, rec) != "runtime_spec.cluster_member_mode_invalid" {
		t.Fatalf("bad mode = %d %s", rec.Code, rec.Body.String())
	}
	if rec = rpcMemberDo(t, srv, http.MethodPost, base, ""); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("post = %d", rec.Code)
	}
	rec = rpcMemberDo(t, srv, http.MethodGet, "/api/portal/mappings/map_nope/runtime/cluster-candidates", "")
	if rec.Code != http.StatusNotFound || rpcMemberErrCode(t, rec) != "mapping.not_found" {
		t.Fatalf("unknown mapping = %d %s", rec.Code, rec.Body.String())
	}

	// A cluster head write refused through the HTTP surface answers its code.
	rec = rpcMemberDo(t, srv, http.MethodPut, "/api/portal/mappings/map_head/runtime-spec",
		`{"enabled":true,"binary":"/opt/llama/llama-server","type":"llama_cpp","args":["--rpc","${RPC_SERVERS}"],"startup_timeout_seconds":600,"cluster_enabled":true}`)
	if rec.Code != http.StatusBadRequest || rpcMemberErrCode(t, rec) != "runtime_spec.cluster_members_required" {
		t.Fatalf("cluster put = %d %s", rec.Code, rec.Body.String())
	}
}
