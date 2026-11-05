// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/routing"
	"strings"
	"testing"
	"time"
)

func rpcMemberDo(t *testing.T, srv *Server, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, newJSONRequest(method, path, body))
	return rec
}

func rpcMemberErrCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal %s: %v", rec.Body.String(), err)
	}
	return body.Error.Code
}

func TestHandlePortalServerRPCMembersCRUD(t *testing.T) {
	srv := NewTestServer()
	serverID := seedGPUBudgetServer(t, srv)
	base := "/api/portal/servers/" + serverID + "/rpc-members"

	rec := rpcMemberDo(t, srv, http.MethodGet, base, "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"data":[]`) {
		t.Fatalf("empty list = %d %s", rec.Code, rec.Body.String())
	}
	rec = rpcMemberDo(t, srv, http.MethodPost, base, `{"binary":"/opt/rpc-server","args":["-t","8"],"gpus":[{"index":0,"vram_estimate_mb":8000}]}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", rec.Code, rec.Body.String())
	}
	var created portal.RPCMemberSpecDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil || !strings.HasPrefix(created.ID, "rpcm_") || len(created.GPUs) != 1 {
		t.Fatalf("created = %+v err=%v", created, err)
	}

	rec = rpcMemberDo(t, srv, http.MethodPut, base+"/"+created.ID, `{"binary":"/opt/rpc-server","enabled":false,"device_backend":"vulkan"}`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"enabled":false`) {
		t.Fatalf("put = %d %s", rec.Code, rec.Body.String())
	}
	rec = rpcMemberDo(t, srv, http.MethodPut, base+"/rpcm_missing", `{"binary":"/opt/rpc-server"}`)
	if rec.Code != http.StatusNotFound || rpcMemberErrCode(t, rec) != "rpc_member_spec.not_found" {
		t.Fatalf("put unknown = %d %s", rec.Code, rec.Body.String())
	}
	rec = rpcMemberDo(t, srv, http.MethodPut, base+"/"+created.ID, `{"binary":"/opt/rpc-server","args":["--port=9"]}`)
	if rec.Code != http.StatusBadRequest || rpcMemberErrCode(t, rec) != "runtime_spec.member_flag_reserved" {
		t.Fatalf("reserved flag = %d %s", rec.Code, rec.Body.String())
	}
	rec = rpcMemberDo(t, srv, http.MethodPost, base, `{"binary":"/opt/rpc-server","env":{"CUDA_VISIBLE_DEVICES":"0"}}`)
	if rec.Code != http.StatusBadRequest || rpcMemberErrCode(t, rec) != "runtime_spec.visible_devices_conflict" {
		t.Fatalf("visible devices = %d %s", rec.Code, rec.Body.String())
	}
	rec = rpcMemberDo(t, srv, http.MethodPost, base, `{`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad json = %d", rec.Code)
	}
	if rec = rpcMemberDo(t, srv, http.MethodDelete, base, ""); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("delete collection = %d", rec.Code)
	}
	if rec = rpcMemberDo(t, srv, http.MethodGet, base+"/"+created.ID, ""); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("get item = %d", rec.Code)
	}
	if rec = rpcMemberDo(t, srv, http.MethodDelete, base+"/"+created.ID, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete = %d %s", rec.Code, rec.Body.String())
	}
	if rec = rpcMemberDo(t, srv, http.MethodDelete, base+"/"+created.ID, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("delete again = %d", rec.Code)
	}
	rec = rpcMemberDo(t, srv, http.MethodGet, "/api/portal/servers/srv_nope/rpc-members", "")
	if rec.Code != http.StatusNotFound || rpcMemberErrCode(t, rec) != "server.not_found" {
		t.Fatalf("unknown server = %d %s", rec.Code, rec.Body.String())
	}
}

func TestHandlePortalServerRPCMembersInUse(t *testing.T) {
	srv := NewTestServer()
	ctx := context.Background()
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	serverID := seedGPUBudgetServer(t, srv)
	base := "/api/portal/servers/" + serverID + "/rpc-members"
	rec := rpcMemberDo(t, srv, http.MethodPost, base, `{"binary":"/opt/rpc-server"}`)
	var created portal.RPCMemberSpecDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil || rec.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", rec.Code, rec.Body.String())
	}

	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(srv.Routes.CreateAIServer(ctx, routing.AIServer{ID: "srv_head", Name: "Head", Domain: "head.example.test", Provider: routing.ProviderMock, Endpoint: "mock://head", Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now}))
	must(srv.Routes.CreateApplication(ctx, routing.Application{ID: "app_head", ServerID: "srv_head", Type: routing.ProviderServerAgent, Port: 8081, Scheme: "http", APIFlavors: []string{routing.APIFlavorOpenAI}, Priority: 1, Weight: 1, TimeoutMS: 30000, AffinityTTLSeconds: 300, Status: routing.ServerStatusActive, HealthCheckMode: routing.HealthCheckModeAlwaysReachable, CreatedAt: now, UpdatedAt: now}))
	must(srv.Routes.CreateMapping(ctx, routing.ModelMapping{ID: "map_head", ApplicationID: "app_head", GatewayModelName: "m", AppModelName: "u", Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}))
	must(srv.Routes.UpsertRuntimeSpec(ctx, routing.RuntimeSpec{ID: "rspec_head", MappingID: "map_head", Enabled: true, Binary: "/usr/bin/llama-server", Args: "[]", Env: "{}", CreatedAt: now, UpdatedAt: now}))
	must(srv.Routes.SetClusterMembers(ctx, "rspec_head", []routing.ClusterMember{{HeadSpecID: "rspec_head", Position: 0, ServerID: serverID, MemberSpecID: created.ID, Port: 47001}}, nil))

	rec = rpcMemberDo(t, srv, http.MethodDelete, base+"/"+created.ID, "")
	if rec.Code != http.StatusConflict || rpcMemberErrCode(t, rec) != "runtime.cluster_member_in_use" {
		t.Fatalf("delete referenced = %d %s", rec.Code, rec.Body.String())
	}
	rec = rpcMemberDo(t, srv, http.MethodPut, base+"/"+created.ID, `{"binary":"/opt/rpc-server","enabled":false}`)
	if rec.Code != http.StatusConflict || rpcMemberErrCode(t, rec) != "runtime.cluster_member_in_use" {
		t.Fatalf("disable referenced = %d %s", rec.Code, rec.Body.String())
	}
}
