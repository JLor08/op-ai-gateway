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

// Serialization side of the Claude model aliases (issue #225): a generated
// alias row -- the one the portal marks with OriginalModelID -- carries
// original_model_id and the honest display_name as TOP-LEVEL siblings of
// capabilities in all three listings (/v1/models, /api/v1/models,
// /anthropic/v1/models), and no other row ever does. The derivation and the
// overlay that produce the rows are tested in internal/portal; these tests pin
// what the three handlers do with them.

const (
	claudeListingAliasSecret = "claude-alias-secret" // flag on
	claudeListingPlainSecret = "claude-plain-secret" // flag off
	claudeListingAliasPrefix = "anthropic-local-"
)

// claudeListingSeed is one model of the fixture below.
type claudeListingSeed struct {
	name        string
	flavors     []string
	contextSize int
}

// newClaudeAliasListingServer seeds one application and mapping per model and
// two gateway:use tokens over them: claudeListingAliasSecret has the generated
// aliases on, claudeListingPlainSecret off. The models:
//
//   - chat-both: serves openai + anthropic (context 8192). Eligible; its alias
//     is listed on /v1/models, /api/v1/models and /anthropic/v1/models.
//   - claude-only: anthropic only. Eligible; its alias exists, but both it and
//     the model are filtered out of the two openai-flavored listings.
//   - openai-only: openai only. Not eligible (no Messages flavor).
//   - images-only: openai_images only. Listed nowhere, never aliased.
func newClaudeAliasListingServer(t *testing.T) *Server {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	tokens := auth.NewTokenStore()
	directory := portal.NewMemoryDirectory(tokens)
	directory.AddUser(store.User{ID: "usr_cla", Email: "cla@example.test", DisplayName: "Cla", Role: "member", Status: store.UserStatusActive, CreatedAt: now, UpdatedAt: now})
	for _, tok := range []struct {
		id, secret string
		aliases    bool
	}{
		{"tok_cla_on", claudeListingAliasSecret, true},
		{"tok_cla_off", claudeListingPlainSecret, false},
	} {
		if err := directory.CreatePlainToken(ctx, store.TokenRecord{
			ID: tok.id, UserID: "usr_cla", Name: tok.id, Status: store.TokenStatusActive,
			Scopes: `["gateway:use"]`, ClaudeModelAliases: tok.aliases, CreatedAt: now, UpdatedAt: now,
		}, tok.secret); err != nil {
			t.Fatalf("CreatePlainToken %s: %v", tok.id, err)
		}
	}

	routeStore := routing.NewMemoryStore()
	for _, seed := range []claudeListingSeed{
		{name: "chat-both", flavors: []string{routing.APIFlavorOpenAI, routing.APIFlavorAnthropic}, contextSize: 8192},
		{name: "claude-only", flavors: []string{routing.APIFlavorAnthropic}},
		{name: "openai-only", flavors: []string{routing.APIFlavorOpenAI}},
		{name: "images-only", flavors: []string{routing.APIFlavorOpenAIImages}},
	} {
		if err := routeStore.CreateAIServer(ctx, routing.AIServer{ID: "srv_" + seed.name, Name: seed.name, Domain: seed.name + ".example.test", Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("CreateAIServer %s: %v", seed.name, err)
		}
		if err := routeStore.CreateApplication(ctx, routing.Application{ID: "app_" + seed.name, ServerID: "srv_" + seed.name, Type: routing.ProviderVLLM, Port: 8000, Scheme: "http", APIFlavors: seed.flavors, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("CreateApplication %s: %v", seed.name, err)
		}
		if err := routeStore.CreateMapping(ctx, routing.ModelMapping{ID: "map_" + seed.name, ApplicationID: "app_" + seed.name, GatewayModelName: seed.name, AppModelName: seed.name, ContextSize: seed.contextSize, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("CreateMapping %s: %v", seed.name, err)
		}
	}
	recorder := usage.NewRecorder()
	return New(ServerDeps{
		Tokens:   tokens,
		Usage:    recorder,
		Provider: provider.NewMock(),
		Routes:   routeStore,
		Portal:   portal.NewService(portal.ServiceDeps{Users: directory, Tokens: directory, Usage: recorder, Routes: routeStore, Clock: func() time.Time { return now }, ModelLister: provider.NewMock()}),
	})
}

// rawString decodes one raw JSON value that must be a string.
func rawString(t *testing.T, raw json.RawMessage, what string) string {
	t.Helper()
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("%s = %s, want a JSON string: %v", what, raw, err)
	}
	return s
}

// keysOf returns the keys of a raw entry, for exact-key-set assertions.
func keysOf(entry map[string]json.RawMessage) []string {
	out := make([]string, 0, len(entry))
	for k := range entry {
		out = append(out, k)
	}
	return out
}

// aliasEntryFor returns the one entry of byID whose id carries the generated
// alias prefix and whose original_model_id is target. It fails when there is
// not exactly one such entry, or when any alias-prefixed entry lacks an
// original_model_id (an alias row must always say what it stands for).
func aliasEntryFor(t *testing.T, byID map[string]openAIModelEntry, target string) (string, openAIModelEntry) {
	t.Helper()
	var foundID string
	var found openAIModelEntry
	for id, entry := range byID {
		if !strings.HasPrefix(id, claudeListingAliasPrefix) {
			continue
		}
		raw, ok := entry["original_model_id"]
		if !ok {
			t.Fatalf("alias-prefixed row %q has no original_model_id: %v", id, entry)
		}
		if rawString(t, raw, id+" original_model_id") != target {
			continue
		}
		if found != nil {
			t.Fatalf("two alias rows for %q: %q and %q", target, foundID, id)
		}
		foundID, found = id, entry
	}
	if found == nil {
		t.Fatalf("no alias row for %q in %v", target, byID)
	}
	return foundID, found
}

// requireNoAliasMetadata fails when an entry carries original_model_id.
func requireNoAliasMetadata(t *testing.T, id string, entry openAIModelEntry) {
	t.Helper()
	if raw, present := entry["original_model_id"]; present {
		t.Fatalf("non-alias row %q must not carry original_model_id, got %s; entry = %v", id, raw, entry)
	}
}

// requireCapabilityKeysUntouched pins that the alias metadata sits NEXT TO the
// capabilities object, never inside it.
func requireCapabilityKeysUntouched(t *testing.T, id string, entry map[string]json.RawMessage) {
	t.Helper()
	raw, ok := entry["capabilities"]
	if !ok {
		t.Fatalf("%q has no capabilities object: %v", id, entry)
	}
	var caps map[string]json.RawMessage
	if err := json.Unmarshal(raw, &caps); err != nil {
		t.Fatalf("%q capabilities: %v", id, err)
	}
	if !sameStrings(keysOf(caps), []string{"image", "image_edit", "trained_for_tool_use", "vision"}) {
		t.Fatalf("%q capabilities keys = %q, want exactly vision, image, image_edit, trained_for_tool_use", id, keysOf(caps))
	}
}

// anthropicModelEntryByID GETs /anthropic/v1/models as the token whose secret is
// secret and returns the data entries indexed by id, in raw JSON form.
func anthropicModelEntryByID(t *testing.T, srv *Server, secret string) map[string]openAIModelEntry {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/anthropic/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+secret)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /anthropic/v1/models: status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Data []openAIModelEntry `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("GET /anthropic/v1/models: unmarshal: %v", err)
	}
	return openAIModelEntryByID(t, body.Data)
}

// TestOpenAIModelsAliasRowCarriesOriginalModelIDAndDisplayName: for a token with
// the generated aliases on, the alias row of an eligible local model carries
// original_model_id (the real gateway id) and display_name (the target's honest
// label) as top-level siblings of capabilities -- not nested, not under a
// vendor namespace -- next to everything the target's row carries (its context
// window). Every non-alias row has NEITHER key, and the flavor filter is
// unchanged: the anthropic-only model, its alias and the images-only model stay
// out of the openai-flavored listing. /v1/models and /openai/v1/models share one
// handler.
func TestOpenAIModelsAliasRowCarriesOriginalModelIDAndDisplayName(t *testing.T) {
	for _, path := range openAIModelsPaths {
		t.Run(path, func(t *testing.T) {
			srv := newClaudeAliasListingServer(t)
			byID := openAIModelEntryByID(t, openAIModelEntries(t, srv, claudeListingAliasSecret, path))

			aliasID, alias := aliasEntryFor(t, byID, "chat-both")
			if got := rawString(t, alias["original_model_id"], "original_model_id"); got != "chat-both" {
				t.Fatalf("original_model_id = %q, want chat-both", got)
			}
			display := rawString(t, alias["display_name"], "display_name")
			if display != "chat-both" {
				t.Fatalf("display_name = %q, want the target's honest label chat-both", display)
			}
			if display == aliasID || strings.HasPrefix(display, "anthropic") {
				t.Fatalf("display_name = %q is the alias id / an Anthropic-looking label, want the honest label", display)
			}
			requireCapabilityKeysUntouched(t, aliasID, alias)
			if _, nested := alias["op_ai_gw"]; nested {
				t.Fatalf("alias row must not nest its metadata under op_ai_gw: %v", alias)
			}
			if got := string(alias["context_length"]); got != "8192" {
				t.Fatalf("alias context_length = %q, want the target's 8192; entry = %v", got, alias)
			}
			if !sameStrings(keysOf(alias), []string{"id", "object", "owned_by", "capabilities", "context_length", "original_model_id", "display_name"}) {
				t.Fatalf("alias row keys = %q, want the standard row plus original_model_id and display_name", keysOf(alias))
			}

			for _, id := range []string{"chat-both", "openai-only"} {
				entry, ok := byID[id]
				if !ok {
					t.Fatalf("listing has no %q entry: %v", id, byID)
				}
				requireNoAliasMetadata(t, id, entry)
				if raw, present := entry["display_name"]; present {
					t.Fatalf("non-alias row %q must not carry display_name, got %s; entry = %v", id, raw, entry)
				}
			}

			// Flavor semantics unchanged: exactly the two real openai models and
			// the one alias that stands for the openai-served model.
			wantIDs := []string{"chat-both", "openai-only", aliasID}
			gotIDs := make([]string, 0, len(byID))
			for id := range byID {
				gotIDs = append(gotIDs, id)
			}
			if !sameStrings(gotIDs, wantIDs) {
				t.Fatalf("GET %s ids = %q, want %q (no anthropic-only / images-only rows)", path, gotIDs, wantIDs)
			}
		})
	}
}

// TestOpenAIModelsFlagOffTokenHasNoAliasMetadata: a token without the setting
// gets the listing it always got -- no alias rows and no metadata keys.
func TestOpenAIModelsFlagOffTokenHasNoAliasMetadata(t *testing.T) {
	for _, path := range openAIModelsPaths {
		t.Run(path, func(t *testing.T) {
			srv := newClaudeAliasListingServer(t)
			byID := openAIModelEntryByID(t, openAIModelEntries(t, srv, claudeListingPlainSecret, path))
			if len(byID) != 2 {
				t.Fatalf("listing has %d entries, want the 2 real openai models: %v", len(byID), byID)
			}
			for id, entry := range byID {
				if strings.HasPrefix(id, claudeListingAliasPrefix) {
					t.Fatalf("flag-off token is offered an alias row %q", id)
				}
				requireNoAliasMetadata(t, id, entry)
				if raw, present := entry["display_name"]; present {
					t.Fatalf("row %q must not carry display_name, got %s", id, raw)
				}
			}
		})
	}
}

// TestLMStudioV1ModelsFromDTOsAliasRow pins the mapping without a server: a row
// with OriginalModelID gains original_model_id (and keeps display_name as the
// DTO's honest label), a row without it gains no key at all.
func TestLMStudioV1ModelsFromDTOsAliasRow(t *testing.T) {
	got := lmStudioV1ModelsFromDTOs([]portal.ModelDTO{
		{ID: "anthropic-local-aaaaaaaaaaaa", DisplayName: "Real Label", OriginalModelID: "real-id", ContextSize: 4096},
		{ID: "plain", DisplayName: "Plain", ContextSize: 4096},
	})
	want := []map[string]any{
		{
			"type": "llm", "key": "anthropic-local-aaaaaaaaaaaa", "display_name": "Real Label",
			"original_model_id":  "real-id",
			"max_context_length": 4096,
			"loaded_instances":   []map[string]any{},
			"capabilities":       map[string]any{"vision": false, "image": false, "image_edit": false, "trained_for_tool_use": true},
		},
		{
			"type": "llm", "key": "plain", "display_name": "Plain",
			"max_context_length": 4096,
			"loaded_instances":   []map[string]any{},
			"capabilities":       map[string]any{"vision": false, "image": false, "image_edit": false, "trained_for_tool_use": true},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("lmStudioV1ModelsFromDTOs =\n%#v\nwant\n%#v", got, want)
	}
}

// TestLMStudioV1ModelsAliasRowCarriesOriginalModelID drives both LM Studio v2
// routes: the alias row carries original_model_id next to its display_name (the
// target's honest label) and capabilities; non-alias rows carry no
// original_model_id; the flavor filter still keeps the anthropic-only model and
// its alias out.
func TestLMStudioV1ModelsAliasRowCarriesOriginalModelID(t *testing.T) {
	for _, path := range lmStudioV1Paths {
		t.Run(path, func(t *testing.T) {
			srv := newClaudeAliasListingServer(t)
			entries := lmStudioV1Entries(t, lmStudioV1Body(t, srv, claudeListingAliasSecret, path))

			var aliasKey string
			for key := range entries {
				if strings.HasPrefix(key, claudeListingAliasPrefix) {
					if aliasKey != "" {
						t.Fatalf("two alias rows in the openai-flavored listing: %q and %q", aliasKey, key)
					}
					aliasKey = key
				}
			}
			if aliasKey == "" {
				t.Fatalf("no alias row in %v", entries)
			}
			alias := entries[aliasKey]
			if got := rawString(t, alias["original_model_id"], "original_model_id"); got != "chat-both" {
				t.Fatalf("original_model_id = %q, want chat-both", got)
			}
			if got := rawString(t, alias["display_name"], "display_name"); got != "chat-both" {
				t.Fatalf("display_name = %q, want the target's honest label chat-both", got)
			}
			requireCapabilityKeysUntouched(t, aliasKey, alias)
			if got := string(alias["max_context_length"]); got != "8192" {
				t.Fatalf("alias max_context_length = %q, want the target's 8192", got)
			}

			for _, key := range []string{"chat-both", "openai-only"} {
				entry, ok := entries[key]
				if !ok {
					t.Fatalf("listing has no %q entry: %v", key, entries)
				}
				if raw, present := entry["original_model_id"]; present {
					t.Fatalf("non-alias row %q must not carry original_model_id, got %s", key, raw)
				}
				if _, present := entry["display_name"]; !present {
					t.Fatalf("row %q lost its display_name: %v", key, entry)
				}
			}
			if len(entries) != 3 {
				t.Fatalf("listing has %d entries, want chat-both, openai-only and one alias: %v", len(entries), entries)
			}
		})
	}
}

// TestAnthropicModelsAliasRowsCarryOriginalModelID: /anthropic/v1/models routes
// the portal rows (not just ids) through, so an alias row carries
// original_model_id and the honest display_name -- never the alias id as its
// label -- while keeping the Anthropic shape (id, type "model", display_name,
// created_at). Non-alias rows keep exactly that shape and their id as label.
// Both eligible models get an alias here, including the anthropic-only one the
// openai-flavored listings leave out, and the ids are still exactly
// Portal.ModelsForFlavor(anthropic).
func TestAnthropicModelsAliasRowsCarryOriginalModelID(t *testing.T) {
	srv := newClaudeAliasListingServer(t)
	byID := anthropicModelEntryByID(t, srv, claudeListingAliasSecret)

	for _, target := range []string{"chat-both", "claude-only"} {
		aliasID, alias := aliasEntryFor(t, byID, target)
		if got := rawString(t, alias["original_model_id"], "original_model_id"); got != target {
			t.Fatalf("%s: original_model_id = %q, want %q", aliasID, got, target)
		}
		display := rawString(t, alias["display_name"], "display_name")
		if display != target {
			t.Fatalf("%s: display_name = %q, want the target's honest label %q", aliasID, display, target)
		}
		if got := rawString(t, alias["type"], "type"); got != "model" {
			t.Fatalf("%s: type = %q, want model", aliasID, got)
		}
		if got := rawString(t, alias["created_at"], "created_at"); got == "" {
			t.Fatalf("%s: created_at is empty", aliasID)
		}
		if !sameStrings(keysOf(alias), []string{"id", "type", "display_name", "created_at", "original_model_id"}) {
			t.Fatalf("%s: keys = %q, want the Anthropic shape plus original_model_id", aliasID, keysOf(alias))
		}
	}

	for _, id := range []string{"chat-both", "claude-only"} {
		entry, ok := byID[id]
		if !ok {
			t.Fatalf("listing has no %q entry: %v", id, byID)
		}
		requireNoAliasMetadata(t, id, entry)
		if got := rawString(t, entry["display_name"], "display_name"); got != id {
			t.Fatalf("non-alias %q display_name = %q, want its id", id, got)
		}
		if !sameStrings(keysOf(entry), []string{"id", "type", "display_name", "created_at"}) {
			t.Fatalf("non-alias %q keys = %q, want exactly the Anthropic shape", id, keysOf(entry))
		}
	}

	token, ok := srv.Tokens.LookupBearer("Bearer " + claudeListingAliasSecret)
	if !ok {
		t.Fatal("alias token not found")
	}
	want := srv.Portal.ModelsForFlavor(context.Background(), token, routing.APIFlavorAnthropic)
	got := make([]string, 0, len(byID))
	for id := range byID {
		got = append(got, id)
	}
	if !sameStrings(got, want) {
		t.Fatalf("/anthropic/v1/models ids = %q, want ModelsForFlavor(anthropic) = %q", got, want)
	}
	if len(got) != 4 {
		t.Fatalf("listing has %d entries, want the 2 anthropic models and their 2 aliases: %v", len(got), got)
	}
}

// TestAnthropicModelsFlagOffTokenHasNoAliasMetadata: without the setting the
// Anthropic listing is what it always was.
func TestAnthropicModelsFlagOffTokenHasNoAliasMetadata(t *testing.T) {
	srv := newClaudeAliasListingServer(t)
	byID := anthropicModelEntryByID(t, srv, claudeListingPlainSecret)
	if len(byID) != 2 {
		t.Fatalf("listing has %d entries, want the 2 real anthropic models: %v", len(byID), byID)
	}
	for id, entry := range byID {
		if strings.HasPrefix(id, claudeListingAliasPrefix) {
			t.Fatalf("flag-off token is offered an alias row %q", id)
		}
		requireNoAliasMetadata(t, id, entry)
		if got := rawString(t, entry["display_name"], "display_name"); got != id {
			t.Fatalf("%q display_name = %q, want its id", id, got)
		}
	}
}
