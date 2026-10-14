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
	"testing"
	"time"
)

// openAIModelsPaths are the two routes handleOpenAIModels serves.
var openAIModelsPaths = []string{"/v1/models", "/openai/v1/models"}

// openAIModelEntry is one data element of an OpenAI-shaped model listing, kept
// as raw JSON values so a test can tell an absent key from a zero value (a
// decoded map[string]any or a struct int cannot). It prints as the JSON it
// holds, which keeps a failure message readable.
type openAIModelEntry map[string]json.RawMessage

func (e openAIModelEntry) String() string {
	b, err := json.Marshal(map[string]json.RawMessage(e))
	if err != nil {
		return err.Error()
	}
	return string(b)
}

// openAIModelEntries GETs an OpenAI-shaped model listing as the token whose
// secret is secret and returns its data array in the listing's own order.
func openAIModelEntries(t *testing.T, srv *Server, secret, path string) []openAIModelEntry {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Authorization", "Bearer "+secret)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s: status = %d, body = %s", path, rec.Code, rec.Body.String())
	}
	var body struct {
		Object string             `json:"object"`
		Data   []openAIModelEntry `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("GET %s: unmarshal: %v", path, err)
	}
	if body.Object != "list" {
		t.Fatalf("GET %s: object = %q, want list", path, body.Object)
	}
	return body.Data
}

// openAIModelEntryByID indexes entries by their decoded id.
func openAIModelEntryByID(t *testing.T, entries []openAIModelEntry) map[string]openAIModelEntry {
	t.Helper()
	out := make(map[string]openAIModelEntry, len(entries))
	for _, entry := range entries {
		var id string
		if err := json.Unmarshal(entry["id"], &id); err != nil {
			t.Fatalf("entry %v: id is not a string: %v", entry, err)
		}
		out[id] = entry
	}
	return out
}

// requireListingIDsAreModelsForFlavor pins the data source of the OpenAI
// listing: the ids it returns, in order, are exactly what
// Portal.ModelsForFlavor(token, openai) returns for the same token. The
// handler reads the richer Portal.Models() rows (for their context size) and
// keeps the openai ones; this is the proof that the switch dropped and added
// no name.
func requireListingIDsAreModelsForFlavor(t *testing.T, srv *Server, secret, path string) {
	t.Helper()
	token, ok := srv.Tokens.LookupBearer("Bearer " + secret)
	if !ok {
		t.Fatalf("token for %q not found", secret)
	}
	want := srv.Portal.ModelsForFlavor(context.Background(), token, routing.APIFlavorOpenAI)
	entries := openAIModelEntries(t, srv, secret, path)
	got := make([]string, 0, len(entries))
	for _, entry := range entries {
		var id string
		if err := json.Unmarshal(entry["id"], &id); err != nil {
			t.Fatalf("GET %s: id is not a string: %v", path, err)
		}
		got = append(got, id)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("GET %s ids = %q, want ModelsForFlavor(openai) = %q", path, got, want)
	}
}

// newContextListingTestServer seeds two openai models behind the dev token: one
// whose mapping knows its context window (8192) and one whose does not (0).
func newContextListingTestServer(t *testing.T) *Server {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	tokens := auth.NewTokenStore()
	directory := portal.NewMemoryDirectory(tokens)
	directory.AddUser(store.User{ID: "usr_dev", Email: "dev@example.test", DisplayName: "Dev", Role: "admin", Status: store.UserStatusActive, CreatedAt: now, UpdatedAt: now})
	if err := directory.CreatePlainToken(ctx, store.TokenRecord{
		ID: "tok_dev", UserID: "usr_dev", Name: "Dev", Status: store.TokenStatusActive,
		Scopes: `["gateway:use","admin"]`, CreatedAt: now, UpdatedAt: now,
	}, "dev-secret"); err != nil {
		t.Fatalf("CreatePlainToken: %v", err)
	}
	routeStore := routing.NewMemoryStore()
	seed := func(id, model string, contextSize int) {
		t.Helper()
		if err := routeStore.CreateAIServer(ctx, routing.AIServer{ID: "srv_" + id, Name: id, Domain: id + ".example.test", Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("CreateAIServer %s: %v", id, err)
		}
		if err := routeStore.CreateApplication(ctx, routing.Application{ID: "app_" + id, ServerID: "srv_" + id, Type: routing.ProviderVLLM, Port: 8000, Scheme: "http", APIFlavors: []string{routing.APIFlavorOpenAI}, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("CreateApplication %s: %v", id, err)
		}
		if err := routeStore.CreateMapping(ctx, routing.ModelMapping{ID: "map_" + id, ApplicationID: "app_" + id, GatewayModelName: model, AppModelName: model, ContextSize: contextSize, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("CreateMapping %s: %v", id, err)
		}
	}
	seed("k", "ctx-known", 8192)
	seed("u", "ctx-unknown", 0)
	recorder := usage.NewRecorder()
	return New(ServerDeps{
		Tokens:   tokens,
		Usage:    recorder,
		Provider: provider.NewMock(),
		Routes:   routeStore,
		Portal:   portal.NewService(portal.ServiceDeps{Users: directory, Tokens: directory, Usage: recorder, Routes: routeStore, Clock: func() time.Time { return now }, ModelLister: provider.NewMock()}),
	})
}

// TestOpenAIModelsCarryContextLengthWhenKnown: a model whose context window is
// known lists it as context_length (the standard location OpenAI-style clients
// read a model's context from; no released opencode is known to read it yet, per
// issue #171); a model whose window is unknown (0) omits the key entirely
// rather than advertising a window of 0. /v1/models and /openai/v1/models share
// one handler, so both get the field, and the listing offers the same ids as
// Portal.ModelsForFlavor(openai).
func TestOpenAIModelsCarryContextLengthWhenKnown(t *testing.T) {
	for _, path := range openAIModelsPaths {
		t.Run(path, func(t *testing.T) {
			srv := newContextListingTestServer(t)
			byID := openAIModelEntryByID(t, openAIModelEntries(t, srv, "dev-secret", path))
			if len(byID) != 2 {
				t.Fatalf("listing has %d entries, want 2: %v", len(byID), byID)
			}

			known, ok := byID["ctx-known"]
			if !ok {
				t.Fatalf("listing has no ctx-known entry: %v", byID)
			}
			if got := string(known["context_length"]); got != "8192" {
				t.Fatalf("ctx-known context_length = %q, want 8192; entry = %v", got, known)
			}
			if got := string(known["object"]); got != `"model"` {
				t.Fatalf("ctx-known object = %s, want \"model\"", got)
			}
			if got := string(known["owned_by"]); got != `"op-ai-gateway"` {
				t.Fatalf("ctx-known owned_by = %s, want \"op-ai-gateway\"", got)
			}

			unknown, ok := byID["ctx-unknown"]
			if !ok {
				t.Fatalf("listing has no ctx-unknown entry: %v", byID)
			}
			if raw, present := unknown["context_length"]; present {
				t.Fatalf("ctx-unknown must omit context_length, got %s; entry = %v", raw, unknown)
			}
			if got := string(unknown["object"]); got != `"model"` {
				t.Fatalf("ctx-unknown object = %s, want \"model\"", got)
			}
			if got := string(unknown["owned_by"]); got != `"op-ai-gateway"` {
				t.Fatalf("ctx-unknown owned_by = %s, want \"op-ai-gateway\"", got)
			}

			// Both entries carry the same capabilities object /api/v1/models
			// carries, always present. This fixture seeds no capability verdicts,
			// so vision/image/image_edit fold to false; trained_for_tool_use is the
			// constant true. The true-value folding is pinned in the shared
			// modelCapabilities path by TestLMStudioV1ModelsFromDTOs.
			for id, entry := range map[string]openAIModelEntry{"ctx-known": known, "ctx-unknown": unknown} {
				raw, ok := entry["capabilities"]
				if !ok {
					t.Fatalf("%s must carry a capabilities object; entry = %v", id, entry)
				}
				var caps map[string]json.RawMessage
				if err := json.Unmarshal(raw, &caps); err != nil {
					t.Fatalf("%s capabilities: %v", id, err)
				}
				capKeys := make([]string, 0, len(caps))
				for capKey := range caps {
					capKeys = append(capKeys, capKey)
				}
				if !sameStrings(capKeys, []string{"image", "image_edit", "trained_for_tool_use", "vision"}) {
					t.Fatalf("%s capabilities keys = %q, want exactly vision, image, image_edit, trained_for_tool_use", id, capKeys)
				}
				if got := string(caps["trained_for_tool_use"]); got != "true" {
					t.Fatalf("%s trained_for_tool_use = %s, want true", id, got)
				}
				for _, capKey := range []string{"vision", "image", "image_edit"} {
					if got := string(caps[capKey]); got != "false" {
						t.Fatalf("%s %s = %s, want false (fixture seeds no capability verdicts)", id, capKey, got)
					}
				}
			}

			requireListingIDsAreModelsForFlavor(t, srv, "dev-secret", path)
		})
	}
}

// TestOpenAIModelsListingIsModelsForFlavor: the handler's data source is the
// portal Models() rows narrowed by openAIModelDTOs, not ModelsForFlavor -- so
// over the fixture that has every overlay (a group, offered aliases, an
// anthropic-only, an images-only and an empty-set model) the ids it lists must
// still be exactly what ModelsForFlavor(openai) answers. The group and the
// offered alias that stand for a model with a known window carry that window;
// the images-only group and its alias stay out.
func TestOpenAIModelsListingIsModelsForFlavor(t *testing.T) {
	for _, path := range openAIModelsPaths {
		t.Run(path, func(t *testing.T) {
			srv := newLMStudioListingTestServer(t)
			if err := srv.Routes.UpdateMappingContextProbe(context.Background(), "map_o", 16384, time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)); err != nil {
				t.Fatalf("UpdateMappingContextProbe: %v", err)
			}

			requireListingIDsAreModelsForFlavor(t, srv, "dev-secret", path)

			byID := openAIModelEntryByID(t, openAIModelEntries(t, srv, "dev-secret", path))
			for _, id := range []string{"qwen3-32b", "alias-chat", "chat-group"} {
				entry, ok := byID[id]
				if !ok {
					t.Fatalf("listing has no %q entry: %v", id, byID)
				}
				if got := string(entry["context_length"]); got != "16384" {
					t.Fatalf("%s context_length = %q, want 16384; entry = %v", id, got, entry)
				}
			}
			if len(byID) != 3 {
				t.Fatalf("listing has %d entries, want the 3 openai-served names: %v", len(byID), byID)
			}
		})
	}
}

// TestOpenAIModelsServiceTokenSeesTheSameIDsAsModelsForFlavor: discovery stays
// reachable to a service token (llm:invoke), and the switch of data source does
// not change which names that principal is shown.
func TestOpenAIModelsServiceTokenSeesTheSameIDsAsModelsForFlavor(t *testing.T) {
	srv, tokens, _ := newServiceScopeTestServer(t)
	addServiceToken(tokens, "svc-secret", auth.Token{ID: "tok_svc", ServiceID: "svc_1", ServiceName: "Svc"})
	if err := srv.Routes.UpdateMappingContextProbe(context.Background(), "route-native-svc", 4096, time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("UpdateMappingContextProbe: %v", err)
	}

	for _, path := range openAIModelsPaths {
		requireListingIDsAreModelsForFlavor(t, srv, "svc-secret", path)

		byID := openAIModelEntryByID(t, openAIModelEntries(t, srv, "svc-secret", path))
		native, ok := byID["native-model"]
		if !ok {
			t.Fatalf("GET %s: no native-model entry: %v", path, byID)
		}
		if got := string(native["context_length"]); got != "4096" {
			t.Fatalf("GET %s: native-model context_length = %q, want 4096; entry = %v", path, got, native)
		}
		qwen, ok := byID["qwen-coder"]
		if !ok {
			t.Fatalf("GET %s: no qwen-coder entry: %v", path, byID)
		}
		if raw, present := qwen["context_length"]; present {
			t.Fatalf("GET %s: qwen-coder has no known window, must omit context_length, got %s", path, raw)
		}
	}
}
