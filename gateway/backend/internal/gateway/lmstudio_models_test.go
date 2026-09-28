// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/provider"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/store"
	"op-ai-gateway/internal/usage"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestLMStudioModelsFromDTOs(t *testing.T) {
	in := []portal.ModelDTO{
		{ID: "loaded-model", ContextSize: 8192, Loaded: true},
		{ID: "cold-model", ContextSize: 4096, Loaded: false},
		{ID: "unknown-ctx", ContextSize: 0, Loaded: false},
		{ID: "loaded-unknown-ctx", ContextSize: 0, Loaded: true},
	}
	got := lmStudioModelsFromDTOs(in)

	if len(got) != 4 {
		t.Fatalf("len = %d, want 4", len(got))
	}
	want0 := map[string]any{
		"id": "loaded-model", "object": "model", "type": "llm", "state": "loaded",
		"max_context_length": 8192, "loaded_context_length": 8192,
	}
	if !reflect.DeepEqual(got[0], want0) {
		t.Fatalf("got[0] = %#v, want %#v", got[0], want0)
	}
	want1 := map[string]any{
		"id": "cold-model", "object": "model", "type": "llm", "state": "not-loaded",
		"max_context_length": 4096,
	}
	if !reflect.DeepEqual(got[1], want1) {
		t.Fatalf("got[1] = %#v, want %#v", got[1], want1)
	}
	if _, ok := got[2]["max_context_length"]; ok {
		t.Fatalf("unknown context must omit max_context_length: %#v", got[2])
	}
	if got[2]["state"] != "not-loaded" {
		t.Fatalf("got[2] state = %v", got[2]["state"])
	}
	want3 := map[string]any{
		"id": "loaded-unknown-ctx", "object": "model", "type": "llm", "state": "loaded",
	}
	if !reflect.DeepEqual(got[3], want3) {
		t.Fatalf("got[3] = %#v, want %#v", got[3], want3)
	}
	if _, ok := got[3]["max_context_length"]; ok {
		t.Fatalf("loaded model with unknown context must omit max_context_length: %#v", got[3])
	}
	if _, ok := got[3]["loaded_context_length"]; ok {
		t.Fatalf("loaded model with unknown context must omit loaded_context_length: %#v", got[3])
	}
}

func TestLMStudioModelsEndpointRequiresAuth(t *testing.T) {
	srv := newCaptureTestServer(t, provider.NewMock(), &fakeCaptureStore{})
	req := httptest.NewRequest(http.MethodGet, "/api/v0/models", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized && rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 401/403", rec.Code)
	}

	req2 := httptest.NewRequest(http.MethodGet, "/api/v0/models", nil)
	req2.Header.Set("Authorization", "Bearer dev-secret")
	rec2 := httptest.NewRecorder()
	srv.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec2.Code, rec2.Body.String())
	}
	if !strings.Contains(rec2.Body.String(), `"object":"list"`) {
		t.Fatalf("body missing list envelope: %s", rec2.Body.String())
	}
}

// listedIDs GETs a model listing as the token whose secret is secret and
// returns the ids of its data array, in the listing's own order.
func listedIDs(t *testing.T, srv *Server, secret, path string) []string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Authorization", "Bearer "+secret)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s: status = %d, body = %s", path, rec.Code, rec.Body.String())
	}
	var body struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("GET %s: unmarshal: %v", path, err)
	}
	out := make([]string, 0, len(body.Data))
	for _, m := range body.Data {
		out = append(out, m.ID)
	}
	return out
}

// newLMStudioListingTestServer seeds an openai application, an
// anthropic-only one and an images-only one, plus a model whose served set is
// empty (an anthropic-only application whose messages endpoint is disabled),
// a group with a text member, a group of only the images-only model, and the
// dev token's two offered aliases, one onto the text model and one onto the
// images-only model.
func newLMStudioListingTestServer(t *testing.T) *Server {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	tokens := auth.NewTokenStore()
	directory := portal.NewMemoryDirectory(tokens)
	directory.AddUser(store.User{ID: "usr_dev", Email: "dev@example.test", DisplayName: "Dev", Role: "admin", Status: store.UserStatusActive, CreatedAt: now, UpdatedAt: now})
	if err := directory.CreatePlainToken(ctx, store.TokenRecord{
		ID: "tok_dev", UserID: "usr_dev", Name: "Dev", Status: store.TokenStatusActive,
		Scopes: `["gateway:use","admin"]`, CreatedAt: now, UpdatedAt: now,
		ModelOverrideMap: `{"alias-chat":{"to":"qwen3-32b","offer":true},"alias-image":{"to":"flux1-dev","offer":true}}`,
	}, "dev-secret"); err != nil {
		t.Fatalf("CreatePlainToken: %v", err)
	}
	routeStore := routing.NewMemoryStore()
	seed := func(id, appType, model string, flavors []string, messagesMode routing.EndpointMode) {
		t.Helper()
		if err := routeStore.CreateAIServer(ctx, routing.AIServer{ID: "srv_" + id, Name: id, Domain: id + ".example.test", Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("CreateAIServer %s: %v", id, err)
		}
		if err := routeStore.CreateApplication(ctx, routing.Application{ID: "app_" + id, ServerID: "srv_" + id, Type: appType, Port: 8000, Scheme: "http", APIFlavors: flavors, MessagesMode: messagesMode, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("CreateApplication %s: %v", id, err)
		}
		if err := routeStore.CreateMapping(ctx, routing.ModelMapping{ID: "map_" + id, ApplicationID: "app_" + id, GatewayModelName: model, AppModelName: model, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("CreateMapping %s: %v", id, err)
		}
	}
	seed("o", routing.ProviderVLLM, "qwen3-32b", []string{routing.APIFlavorOpenAI}, "")
	seed("a", routing.ProviderVLLM, "claude-local", []string{routing.APIFlavorAnthropic}, "")
	seed("i", routing.ProviderStableDiffusionCpp, "flux1-dev", []string{routing.APIFlavorOpenAIImages}, "")
	seed("m", routing.ProviderVLLM, "claude-muted", []string{routing.APIFlavorAnthropic}, routing.EndpointModeDisabled)
	group := func(id, name string, members ...string) {
		t.Helper()
		if err := routeStore.CreateModelGroup(ctx, routing.ModelGroup{ID: id, GatewayModelName: name, DisplayName: name, Status: routing.ServerStatusActive, FailoverMode: "sticky", CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("CreateModelGroup %s: %v", id, err)
		}
		rows := make([]routing.GroupMember, 0, len(members))
		for i, member := range members {
			rows = append(rows, routing.GroupMember{MemberGatewayName: member, Priority: i})
		}
		if err := routeStore.SetGroupMembers(ctx, id, rows); err != nil {
			t.Fatalf("SetGroupMembers %s: %v", id, err)
		}
	}
	group("grp_chat", "chat-group", "qwen3-32b", "flux1-dev")
	group("grp_image", "image-group", "flux1-dev")
	recorder := usage.NewRecorder()
	return New(ServerDeps{
		Tokens:   tokens,
		Usage:    recorder,
		Provider: provider.NewMock(),
		Routes:   routeStore,
		Portal:   portal.NewService(portal.ServiceDeps{Users: directory, Tokens: directory, Usage: recorder, Routes: routeStore, Clock: func() time.Time { return now }, ModelLister: provider.NewMock()}),
	})
}

// TestLMStudioModelsListsExactlyTheOpenAIListing pins /api/v0/models to
// /v1/models: an LM Studio-aware client chats over /v1/chat/completions, so
// the listing offers exactly the names that endpoint's own listing offers --
// groups and offered aliases included, and the anthropic-only, images-only and
// empty-set models, the images-only group and the alias onto the images-only
// model left out. Every entry it keeps is an "llm".
func TestLMStudioModelsListsExactlyTheOpenAIListing(t *testing.T) {
	srv := newLMStudioListingTestServer(t)

	want := []string{"alias-chat", "chat-group", "qwen3-32b"}
	if got := listedIDs(t, srv, "dev-secret", "/v1/models"); !reflect.DeepEqual(got, want) {
		t.Fatalf("/v1/models = %q, want %q", got, want)
	}
	if got := listedIDs(t, srv, "dev-secret", "/api/v0/models"); !reflect.DeepEqual(got, want) {
		t.Fatalf("/api/v0/models = %q, want the /v1/models ids %q", got, want)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v0/models", nil)
	req.Header.Set("Authorization", "Bearer dev-secret")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	var body struct {
		Data []struct {
			ID   string `json:"id"`
			Type string `json:"type"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, m := range body.Data {
		if m.Type != "llm" {
			t.Fatalf("/api/v0/models entry %q has type %q, want llm", m.ID, m.Type)
		}
	}
}
