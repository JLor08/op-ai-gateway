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

// lmStudioV1Paths are the two routes handleLMStudioV1Models serves.
var lmStudioV1Paths = []string{"/api/v1/models", "/openai/api/v1/models"}

// lmStudioV1Entry is one element of the "models" array of an LM Studio v2
// listing, kept as raw JSON values so a test can tell an absent key from a zero
// value (a decoded map[string]any or a struct int cannot) and can compare a
// value's exact wire form. It prints as the JSON it holds, which keeps a
// failure message readable.
type lmStudioV1Entry map[string]json.RawMessage

func (e lmStudioV1Entry) String() string {
	b, err := json.Marshal(map[string]json.RawMessage(e))
	if err != nil {
		return err.Error()
	}
	return string(b)
}

// lmStudioV1Body GETs an LM Studio v2 listing as the token whose secret is
// secret and returns the raw response body, requiring a 200.
func lmStudioV1Body(t *testing.T, srv *Server, secret, path string) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Authorization", "Bearer "+secret)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s: status = %d, body = %s", path, rec.Code, rec.Body.String())
	}
	return rec.Body.String()
}

// lmStudioV1Entries decodes the body of an LM Studio v2 listing: its only
// top-level key must be "models", and the entries come back indexed by their
// decoded key.
func lmStudioV1Entries(t *testing.T, body string) map[string]lmStudioV1Entry {
	t.Helper()
	var top map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &top); err != nil {
		t.Fatalf("unmarshal %s: %v", body, err)
	}
	if len(top) != 1 {
		t.Fatalf("top-level keys = %v, want exactly \"models\": %s", top, body)
	}
	rawModels, ok := top["models"]
	if !ok {
		t.Fatalf("body has no \"models\" key: %s", body)
	}
	var entries []lmStudioV1Entry
	if err := json.Unmarshal(rawModels, &entries); err != nil {
		t.Fatalf("\"models\" is not an array of objects: %v (%s)", err, rawModels)
	}
	out := make(map[string]lmStudioV1Entry, len(entries))
	for _, entry := range entries {
		var key string
		if err := json.Unmarshal(entry["key"], &key); err != nil {
			t.Fatalf("entry %v: key is not a string: %v", entry, err)
		}
		out[key] = entry
	}
	return out
}

// lmStudioV1Seed is one openai-served model of the v2 listing fixture.
type lmStudioV1Seed struct {
	name        string
	contextSize int
	vision      bool
	loaded      bool
	flavors     []string
}

// newLMStudioV1Server seeds one application and mapping per seed behind the dev
// token ("dev-secret", gateway:use), a service token ("svc-secret", sole
// llm:invoke) and a token with neither scope ("other-secret", admin only). A
// vision seed gets a "yes" vision verdict, a loaded one is
// reported loaded by the loaded-model registry, and a seed with no flavors is
// served over the openai flavor.
func newLMStudioV1Server(t *testing.T, seeds ...lmStudioV1Seed) *Server {
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
	addServiceToken(tokens, "svc-secret", auth.Token{ID: "tok_svc", ServiceID: "svc_1", ServiceName: "Svc"})
	tokens.AddPlainToken(auth.Token{ID: "tok_other", UserID: "usr_dev", Scopes: []string{"admin"}, Active: true}, "other-secret")

	routeStore := routing.NewMemoryStore()
	registry := NewLoadedModelRegistry()
	for _, seed := range seeds {
		flavors := seed.flavors
		if flavors == nil {
			flavors = []string{routing.APIFlavorOpenAI}
		}
		if err := routeStore.CreateAIServer(ctx, routing.AIServer{ID: "srv_" + seed.name, Name: seed.name, Domain: seed.name + ".example.test", Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("CreateAIServer %s: %v", seed.name, err)
		}
		if err := routeStore.CreateApplication(ctx, routing.Application{ID: "app_" + seed.name, ServerID: "srv_" + seed.name, Type: routing.ProviderVLLM, Port: 8000, Scheme: "http", APIFlavors: flavors, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("CreateApplication %s: %v", seed.name, err)
		}
		if err := routeStore.CreateMapping(ctx, routing.ModelMapping{ID: "map_" + seed.name, ApplicationID: "app_" + seed.name, GatewayModelName: seed.name, AppModelName: seed.name, ContextSize: seed.contextSize, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("CreateMapping %s: %v", seed.name, err)
		}
		if seed.vision {
			if err := routeStore.UpsertMappingCapabilities(ctx, "map_"+seed.name, []routing.CapabilityRow{{
				Capability: routing.CapabilityVision, Verdict: routing.CapabilityYes, Source: "manual", CheckedAt: now.Add(-time.Hour),
			}}); err != nil {
				t.Fatalf("UpsertMappingCapabilities %s: %v", seed.name, err)
			}
		}
		if seed.loaded {
			registry.SetGatewayProbe("app_"+seed.name, []string{seed.name})
		}
	}
	recorder := usage.NewRecorder()
	return New(ServerDeps{
		Tokens:       tokens,
		Usage:        recorder,
		Provider:     provider.NewMock(),
		Routes:       routeStore,
		LoadedModels: registry,
		Portal:       portal.NewService(portal.ServiceDeps{Users: directory, Tokens: directory, Usage: recorder, Routes: routeStore, LoadedModels: registry, Clock: func() time.Time { return now }, ModelLister: provider.NewMock()}),
	})
}

var (
	lmStudioLoadedSeed = lmStudioV1Seed{name: "v1-loaded", contextSize: 8192, vision: true, loaded: true}
	lmStudioColdSeed   = lmStudioV1Seed{name: "v1-cold"}
)

// TestLMStudioV1ModelsFromDTOs pins the helper's mapping row by row, without a
// server: a loaded model lists one instance carrying its context window, a
// model that is not loaded lists none, and max_context_length is ALWAYS present
// -- even at 0 -- because LM Studio's v2 contract requires it (a client keeps
// its own default at 0). That is deliberately unlike /v1/models, which omits
// context_length when the window is unknown.
func TestLMStudioV1ModelsFromDTOs(t *testing.T) {
	got := lmStudioV1ModelsFromDTOs([]portal.ModelDTO{
		{ID: "loaded-vision", DisplayName: "Loaded Vision", ContextSize: 8192, Vision: true, Loaded: true},
		{ID: "cold-unknown", DisplayName: "Cold Unknown", ContextSize: 0, Vision: false, Loaded: false},
		{ID: "loaded-unknown", DisplayName: "Loaded Unknown", ContextSize: 0, Loaded: true},
	})
	want := []map[string]any{
		{
			"type": "llm", "key": "loaded-vision", "display_name": "Loaded Vision",
			"max_context_length": 8192,
			"loaded_instances":   []map[string]any{{"config": map[string]any{"context_length": 8192}}},
			"capabilities":       map[string]any{"vision": true, "trained_for_tool_use": true},
		},
		{
			"type": "llm", "key": "cold-unknown", "display_name": "Cold Unknown",
			"max_context_length": 0,
			"loaded_instances":   []map[string]any{},
			"capabilities":       map[string]any{"vision": false, "trained_for_tool_use": true},
		},
		{
			"type": "llm", "key": "loaded-unknown", "display_name": "Loaded Unknown",
			"max_context_length": 0,
			"loaded_instances":   []map[string]any{{"config": map[string]any{"context_length": 0}}},
			"capabilities":       map[string]any{"vision": false, "trained_for_tool_use": true},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("lmStudioV1ModelsFromDTOs =\n%#v\nwant\n%#v", got, want)
	}

	// No rows maps to a non-nil empty slice: the listing must answer [], never
	// null.
	if empty := lmStudioV1ModelsFromDTOs(nil); empty == nil || len(empty) != 0 {
		t.Fatalf("lmStudioV1ModelsFromDTOs(nil) = %#v, want a non-nil empty slice", empty)
	}
}

// TestLMStudioV1ModelsShape drives both routes over the real handler: the body's
// only top-level key is "models", and each entry carries exactly LM Studio v2's
// type / key / display_name / max_context_length / loaded_instances /
// capabilities. The loaded model reports its 8192 window both as
// max_context_length and as its loaded instance's config.context_length and
// folds vision to true; the model that is not loaded and has an unknown window
// still carries max_context_length (0), lists no loaded instance, and folds
// vision to false.
func TestLMStudioV1ModelsShape(t *testing.T) {
	for _, path := range lmStudioV1Paths {
		t.Run(path, func(t *testing.T) {
			srv := newLMStudioV1Server(t, lmStudioLoadedSeed, lmStudioColdSeed)
			entries := lmStudioV1Entries(t, lmStudioV1Body(t, srv, "dev-secret", path))
			if len(entries) != 2 {
				t.Fatalf("listing has %d entries, want 2: %v", len(entries), entries)
			}

			wantFields := []string{"capabilities", "display_name", "key", "loaded_instances", "max_context_length", "type"}
			for key, want := range map[string]struct {
				maxContext string
				instances  string
				vision     string
			}{
				"v1-loaded": {maxContext: "8192", instances: `[{"config":{"context_length":8192}}]`, vision: "true"},
				"v1-cold":   {maxContext: "0", instances: `[]`, vision: "false"},
			} {
				entry, ok := entries[key]
				if !ok {
					t.Fatalf("listing has no %q entry: %v", key, entries)
				}
				fields := make([]string, 0, len(entry))
				for field := range entry {
					fields = append(fields, field)
				}
				if !sameStrings(fields, wantFields) {
					t.Fatalf("%s fields = %q, want exactly %q; entry = %v", key, fields, wantFields, entry)
				}
				if got := string(entry["type"]); got != `"llm"` {
					t.Fatalf("%s type = %s, want \"llm\"", key, got)
				}
				if got := string(entry["key"]); got != `"`+key+`"` {
					t.Fatalf("%s key = %s, want the model id", key, got)
				}
				if got := string(entry["display_name"]); got != `"`+key+`"` {
					t.Fatalf("%s display_name = %s, want the portal row's display name %q", key, got, key)
				}
				raw, present := entry["max_context_length"]
				if !present {
					t.Fatalf("%s must carry max_context_length even when the window is unknown; entry = %v", key, entry)
				}
				if got := string(raw); got != want.maxContext {
					t.Fatalf("%s max_context_length = %s, want %s", key, got, want.maxContext)
				}
				if got := string(entry["loaded_instances"]); got != want.instances {
					t.Fatalf("%s loaded_instances = %s, want %s", key, got, want.instances)
				}
				var caps map[string]json.RawMessage
				if err := json.Unmarshal(entry["capabilities"], &caps); err != nil {
					t.Fatalf("%s capabilities: %v", key, err)
				}
				if got := string(caps["trained_for_tool_use"]); got != "true" {
					t.Fatalf("%s trained_for_tool_use = %s, want true", key, got)
				}
				if got := string(caps["vision"]); got != want.vision {
					t.Fatalf("%s vision = %s, want %s", key, got, want.vision)
				}
				if len(caps) != 2 {
					t.Fatalf("%s capabilities = %v, want exactly vision and trained_for_tool_use", key, caps)
				}
			}
		})
	}
}

// sameStrings reports whether a and b hold the same strings regardless of order.
func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[string]int, len(a))
	for _, s := range a {
		seen[s]++
	}
	for _, s := range b {
		seen[s]--
	}
	for _, n := range seen {
		if n != 0 {
			return false
		}
	}
	return true
}

// TestLMStudioV1ModelsEmptyArraysAreNotNull is the wire-level guard behind the
// two empty arrays of the contract: with ZERO listable models the body literally
// carries "models":[] and a model that is not loaded literally carries
// "loaded_instances":[]. A nil slice would encode as null, which a client
// decoding the LM Studio shape rejects or iterates as absent, and a decoded
// value could not tell the two apart -- so the assertion reads the raw bytes.
func TestLMStudioV1ModelsEmptyArraysAreNotNull(t *testing.T) {
	for _, path := range lmStudioV1Paths {
		t.Run(path, func(t *testing.T) {
			t.Run("no models", func(t *testing.T) {
				srv := newLMStudioV1Server(t)
				body := lmStudioV1Body(t, srv, "dev-secret", path)
				if !strings.Contains(body, `"models":[]`) {
					t.Fatalf("body = %s, want it to contain \"models\":[]", body)
				}
				if strings.Contains(body, `"models":null`) {
					t.Fatalf("body = %s, \"models\" must not be null", body)
				}
			})
			t.Run("only a model no chat client can use", func(t *testing.T) {
				srv := newLMStudioV1Server(t, lmStudioV1Seed{name: "claude-only", flavors: []string{routing.APIFlavorAnthropic}})
				body := lmStudioV1Body(t, srv, "dev-secret", path)
				if !strings.Contains(body, `"models":[]`) {
					t.Fatalf("body = %s, want the openai filter to leave \"models\":[]", body)
				}
			})
			t.Run("model not loaded", func(t *testing.T) {
				srv := newLMStudioV1Server(t, lmStudioColdSeed)
				body := lmStudioV1Body(t, srv, "dev-secret", path)
				if !strings.Contains(body, `"loaded_instances":[]`) {
					t.Fatalf("body = %s, want it to contain \"loaded_instances\":[]", body)
				}
				if strings.Contains(body, `"loaded_instances":null`) {
					t.Fatalf("body = %s, \"loaded_instances\" must not be null", body)
				}
			})
		})
	}
}

// TestLMStudioV1ModelsAuth: the v2 listing is bearer-only and, like /v1/models,
// reachable to both a normal gateway:use token and a service token whose sole
// scope is llm:invoke (model discovery is not invocation). A session cookie
// alone -- a logged-in browser -- is refused with 401, an unknown bearer is
// refused, a bearer without either scope is 403, and a non-GET is 405. Both
// routes reach the handler.
func TestLMStudioV1ModelsAuth(t *testing.T) {
	for _, path := range lmStudioV1Paths {
		t.Run(path, func(t *testing.T) {
			srv := newLMStudioV1Server(t, lmStudioLoadedSeed, lmStudioColdSeed)

			for _, secret := range []string{"dev-secret", "svc-secret"} {
				entries := lmStudioV1Entries(t, lmStudioV1Body(t, srv, secret, path))
				if len(entries) != 2 {
					t.Fatalf("bearer %q: listing has %d entries, want 2: %v", secret, len(entries), entries)
				}
			}

			noAuth := httptest.NewRecorder()
			srv.ServeHTTP(noAuth, httptest.NewRequest(http.MethodGet, path, nil))
			if noAuth.Code != http.StatusUnauthorized {
				t.Fatalf("no credentials: status = %d, want 401: %s", noAuth.Code, noAuth.Body.String())
			}
			badBearer := httptest.NewRecorder()
			srv.ServeHTTP(badBearer, bearerRequest(http.MethodGet, path, "", "not-a-token"))
			if badBearer.Code != http.StatusUnauthorized {
				t.Fatalf("unknown bearer: status = %d, want 401: %s", badBearer.Code, badBearer.Body.String())
			}
			post := httptest.NewRecorder()
			srv.ServeHTTP(post, bearerRequest(http.MethodPost, path, "", "dev-secret"))
			if post.Code != http.StatusMethodNotAllowed {
				t.Fatalf("POST: status = %d, want 405: %s", post.Code, post.Body.String())
			}
		})
	}
}

// TestLMStudioV1ModelsWrongScopeIs403: a bearer that resolves but carries
// neither gateway:use nor llm:invoke is refused with 403 insufficient_scope.
func TestLMStudioV1ModelsWrongScopeIs403(t *testing.T) {
	srv := newLMStudioV1Server(t, lmStudioColdSeed)
	for _, path := range lmStudioV1Paths {
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, bearerRequest(http.MethodGet, path, "", "other-secret"))
		if rec.Code != http.StatusForbidden {
			t.Fatalf("GET %s with neither scope: status = %d, want 403: %s", path, rec.Code, rec.Body.String())
		}
		if got := decodeErrorCode(t, rec.Body.Bytes()); got != "auth.insufficient_scope" {
			t.Fatalf("GET %s error code = %q, want auth.insufficient_scope", path, got)
		}
	}
}

// TestLMStudioV1ModelsRefusesASessionCookie: a browser session cookie alone must
// not reach the listing. newChatTestServer wires a real Account and loginCookie
// mints a real session, so this is the genuine article rather than a stub.
func TestLMStudioV1ModelsRefusesASessionCookie(t *testing.T) {
	srv, dir := newChatTestServer(t)
	seedLoginUser(t, dir, "usr_lm", "lm@example.test", "password-1", "user")
	cookie := loginCookie(t, srv, "lm@example.test", "password-1")
	for _, path := range lmStudioV1Paths {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.AddCookie(cookie)
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("GET %s with a session cookie: status = %d, want 401: %s", path, rec.Code, rec.Body.String())
		}
	}
}

// TestLMStudioV0ModelsIsGone: GET /api/v0/models, the LM Studio v0 listing no
// current client reads, is removed -- it falls through to the catch-all, even
// for a valid bearer.
func TestLMStudioV0ModelsIsGone(t *testing.T) {
	srv := newLMStudioV1Server(t, lmStudioLoadedSeed)
	for _, secret := range []string{"dev-secret", "svc-secret"} {
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, bearerRequest(http.MethodGet, "/api/v0/models", "", secret))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("GET /api/v0/models with %q: status = %d, want 404: %s", secret, rec.Code, rec.Body.String())
		}
		if got := decodeErrorCode(t, rec.Body.Bytes()); got != "request.not_found" {
			t.Fatalf("GET /api/v0/models error code = %q, want request.not_found", got)
		}
	}
}

// listedIDs GETs a model listing as the token whose secret is secret and
// returns its ids, in the listing's own order: the data[].id of an OpenAI-shaped
// listing (/v1/models) or the models[].key of an LM Studio v2 one
// (/api/v1/models).
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
		Models []struct {
			Key string `json:"key"`
		} `json:"models"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("GET %s: unmarshal: %v", path, err)
	}
	out := make([]string, 0, len(body.Data)+len(body.Models))
	for _, m := range body.Data {
		out = append(out, m.ID)
	}
	for _, m := range body.Models {
		out = append(out, m.Key)
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

// TestLMStudioV1ModelsListsExactlyTheOpenAIListing pins the v2 listing to
// /v1/models: an LM Studio-aware client chats over /v1/chat/completions, so the
// listing offers exactly the names that endpoint's own listing offers -- groups
// and offered aliases included, and the anthropic-only, images-only and
// empty-set models, the images-only group and the alias onto the images-only
// model left out. Every entry it keeps is an "llm".
func TestLMStudioV1ModelsListsExactlyTheOpenAIListing(t *testing.T) {
	srv := newLMStudioListingTestServer(t)

	want := []string{"alias-chat", "chat-group", "qwen3-32b"}
	if got := listedIDs(t, srv, "dev-secret", "/v1/models"); !reflect.DeepEqual(got, want) {
		t.Fatalf("/v1/models = %q, want %q", got, want)
	}
	for _, path := range lmStudioV1Paths {
		if got := listedIDs(t, srv, "dev-secret", path); !reflect.DeepEqual(got, want) {
			t.Fatalf("%s = %q, want the /v1/models ids %q", path, got, want)
		}
		for key, entry := range lmStudioV1Entries(t, lmStudioV1Body(t, srv, "dev-secret", path)) {
			if got := string(entry["type"]); got != `"llm"` {
				t.Fatalf("%s entry %q has type %s, want \"llm\"", path, key, got)
			}
		}
	}
}
