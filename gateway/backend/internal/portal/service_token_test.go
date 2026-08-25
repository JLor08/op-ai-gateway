// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/store"
	"testing"
	"time"
)

// tokenSettingsFixture wires the two halves these tests need at once: a token
// directory (the write path under test) and a routing store (the model offering
// every model-valued setting is validated against). The older token tests in
// service_test.go deliberately run WITHOUT a routing store and therefore
// against the seed models only, which cannot express a group, a hidden model or
// a locked one — all three of which the fallback rule has to tell apart.
type tokenSettingsFixture struct {
	svc   *Service
	dir   *MemoryDirectory
	rs    *routing.MemoryStore
	owner auth.Token
}

func newTokenSettingsFixture(t *testing.T, models ...offeringMapping) *tokenSettingsFixture {
	t.Helper()
	ctx := context.Background()
	dir := NewMemoryDirectory(auth.NewTokenStore())
	if err := dir.CreateUser(ctx, store.User{
		ID: "usr_1", Email: "u1@example.test", DisplayName: "U1", Role: "user",
		Status: store.UserStatusActive, PreferredLanguage: "de",
		CreatedAt: offeringTime, UpdatedAt: offeringTime,
	}); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	rs := routing.NewMemoryStore()
	for i, m := range models {
		offerModel(t, rs,
			fmt.Sprintf("srv_tok_%d", i), fmt.Sprintf("TokBox%d", i), fmt.Sprintf("app_tok_%d", i),
			m.flavors, m.name, m.name+"-up", routing.ServerStatusActive)
	}
	svc := NewService(ServiceDeps{Users: dir, Tokens: dir, Routes: rs, Clock: func() time.Time { return offeringTime }})
	return &tokenSettingsFixture{
		svc:   svc,
		dir:   dir,
		rs:    rs,
		owner: auth.Token{UserID: "usr_1", Scopes: []string{"gateway:use"}},
	}
}

// openAIModel is the shorthand every fixture below seeds with.
func openAIModel(name string) offeringMapping {
	return offeringMapping{name: name, flavors: []string{routing.APIFlavorOpenAI}}
}

// seedTokenRecord writes a token record STRAIGHT THROUGH the store, bypassing
// CreateToken. Both values these tests seed — the last-used-model marker and
// the two per-rule listing switches in their stored shape — are deliberately
// not settable the way they are seeded here, so a fixture for them cannot go
// through the API it is meant to test.
func seedTokenRecord(t *testing.T, dir *MemoryDirectory, record store.TokenRecord) {
	t.Helper()
	record.UserID = "usr_1"
	record.Status = store.TokenStatusActive
	record.Scopes = `["gateway:use"]`
	record.CreatedAt = offeringTime
	record.UpdatedAt = offeringTime
	if err := dir.CreatePlainToken(context.Background(), record, "secret-"+record.ID); err != nil {
		t.Fatalf("CreatePlainToken %s: %v", record.ID, err)
	}
}

func seedTokenWithLastUsedModel(t *testing.T, dir *MemoryDirectory, id, model string) {
	t.Helper()
	seedTokenRecord(t, dir, store.TokenRecord{ID: id, Name: id, LastUsedModel: model})
}

func seedTokenWithRules(t *testing.T, dir *MemoryDirectory, id string, rules map[string]store.ModelOverrideRule) {
	t.Helper()
	seedTokenRecord(t, dir, store.TokenRecord{ID: id, Name: id, ModelOverrideMap: store.EncodeModelOverrideRules(rules)})
}

// storedRules reads a token's persisted override rules back out of the store.
func storedRules(t *testing.T, dir *MemoryDirectory, id string) map[string]store.ModelOverrideRule {
	t.Helper()
	record, err := dir.TokenByID(context.Background(), id)
	if err != nil {
		t.Fatalf("TokenByID %s: %v", id, err)
	}
	return store.DecodeModelOverrideRules(record.ModelOverrideMap)
}

func TestCreateTokenRejectsUnknownFallback(t *testing.T) {
	// Same rule and same error code as an override target: a fallback nobody can
	// route to is a configuration error, not something to discover at runtime.
	f := newTokenSettingsFixture(t, openAIModel("qwen3-32b"))
	_, err := f.svc.CreateToken(context.Background(), f.owner, CreateTokenRequest{
		Name: "t", Scopes: []string{"gateway:use"},
		UnknownModelRedirect: true, UnknownModelFallback: "no-such-model",
	})
	if !errors.Is(err, ErrTokenModelOverrideInvalid) {
		t.Fatalf("err = %v, want ErrTokenModelOverrideInvalid", err)
	}
}

func TestCreateTokenAcceptsGroupAsFallback(t *testing.T) {
	// Models and groups share one namespace in the offering, so a group is a
	// valid fallback and must not be rejected as "no such model".
	f := newTokenSettingsFixture(t, openAIModel("qwen3-32b"))
	offerGroup(t, f.rs, "grp_fast", "fast-group", "qwen3-32b")
	resp, err := f.svc.CreateToken(context.Background(), f.owner, CreateTokenRequest{
		Name: "t", Scopes: []string{"gateway:use"},
		UnknownModelRedirect: true, UnknownModelFallback: "fast-group",
	})
	if err != nil {
		t.Fatalf("group fallback rejected: %v", err)
	}
	if resp.Token.UnknownModelFallback != "fast-group" {
		t.Fatalf("fallback = %q, want fast-group", resp.Token.UnknownModelFallback)
	}
}

// TestCreateTokenAcceptsHiddenModelAsFallback and
// TestCreateTokenRejectsLockedModelAsFallback are the two halves of the SET the
// fallback is validated against. Callable, never the LISTING: "hidden" only
// takes a name out of the listing while it keeps routing perfectly, whereas
// "locked" makes it group-only, so a direct request for it — which is exactly
// what the redirect issues — cannot route at all.
func TestCreateTokenAcceptsHiddenModelAsFallback(t *testing.T) {
	ctx := context.Background()
	f := newTokenSettingsFixture(t, openAIModel("qwen3-32b"))
	offerVisibility(t, f.rs, "qwen3-32b", "hidden")
	// Guard against the test passing for the wrong reason: the model really is
	// out of the listing set here.
	if containsString(f.svc.ModelsForFlavor(ctx, f.owner, routing.APIFlavorOpenAI), "qwen3-32b") {
		t.Fatal("fixture broken: the hidden model is still in the listing")
	}
	resp, err := f.svc.CreateToken(ctx, f.owner, CreateTokenRequest{
		Name: "t", Scopes: []string{"gateway:use"},
		UnknownModelRedirect: true, UnknownModelFallback: "qwen3-32b",
	})
	if err != nil {
		t.Fatalf("hidden-but-callable fallback rejected: %v", err)
	}
	if resp.Token.UnknownModelFallback != "qwen3-32b" {
		t.Fatalf("fallback = %q, want qwen3-32b", resp.Token.UnknownModelFallback)
	}
}

func TestCreateTokenRejectsLockedModelAsFallback(t *testing.T) {
	ctx := context.Background()
	f := newTokenSettingsFixture(t, openAIModel("qwen3-32b"))
	offerVisibility(t, f.rs, "qwen3-32b", "locked")
	// The rejection must be about callability, not about existence.
	if _, ok := f.svc.ModelOfferingFor(ctx, f.owner, routing.APIFlavorOpenAI).Existing["qwen3-32b"]; !ok {
		t.Fatal("fixture broken: the locked model does not exist at all")
	}
	_, err := f.svc.CreateToken(ctx, f.owner, CreateTokenRequest{
		Name: "t", Scopes: []string{"gateway:use"},
		UnknownModelRedirect: true, UnknownModelFallback: "qwen3-32b",
	})
	if !errors.Is(err, ErrTokenModelOverrideInvalid) {
		t.Fatalf("locked fallback err = %v, want ErrTokenModelOverrideInvalid", err)
	}
}

// TestCreateTokenRejectsOfferedAliasAsFallback pins the other half of the same
// choice. An offered override alias IS in the LISTING, and validating against
// the listing would let an operator save a fallback that can never route: the
// redirect runs AFTER the override rewrite, so an alias reaching it is a dead
// name.
func TestCreateTokenRejectsOfferedAliasAsFallback(t *testing.T) {
	ctx := context.Background()
	f := newTokenSettingsFixture(t, openAIModel("qwen3-32b"))
	owner := f.owner
	owner.ModelOverrideRules = store.AuthModelOverrideRules(map[string]store.ModelOverrideRule{
		"claude-x": {To: "qwen3-32b", Offer: true},
	})
	if !containsString(f.svc.ModelsForFlavor(ctx, owner, routing.APIFlavorOpenAI), "claude-x") {
		t.Fatal("fixture broken: the alias is not listed, so this proves nothing")
	}
	_, err := f.svc.CreateToken(ctx, owner, CreateTokenRequest{
		Name: "t", Scopes: []string{"gateway:use"},
		UnknownModelRedirect: true, UnknownModelFallback: "claude-x",
	})
	if !errors.Is(err, ErrTokenModelOverrideInvalid) {
		t.Fatalf("alias fallback err = %v, want ErrTokenModelOverrideInvalid", err)
	}
}

func TestFallbackClearedWhenRedirectOff(t *testing.T) {
	// Mirrors ServerOverrideForceUnreachable, which is forced false whenever its
	// parent is empty: no stored setting that cannot apply.
	f := newTokenSettingsFixture(t, openAIModel("qwen3-32b"))
	resp, err := f.svc.CreateToken(context.Background(), f.owner, CreateTokenRequest{
		Name: "t", Scopes: []string{"gateway:use"},
		UnknownModelRedirect: false, UnknownModelFallback: "qwen3-32b",
		UnknownModelRedirectBlocked: true,
	})
	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}
	if resp.Token.UnknownModelFallback != "" || resp.Token.UnknownModelRedirectBlocked {
		t.Fatalf("settings kept without the redirect: %+v", resp.Token)
	}
	record, err := f.dir.TokenByID(context.Background(), resp.Token.ID)
	if err != nil {
		t.Fatalf("TokenByID: %v", err)
	}
	if record.UnknownModelFallback != "" || record.UnknownModelRedirectBlocked || record.UnknownModelRedirect {
		t.Fatalf("settings persisted without the redirect: %+v", record)
	}
}

func TestTokenDTOCarriesLastUsedModel(t *testing.T) {
	// Read-only: the marker is written by the inference path, never by the API.
	f := newTokenSettingsFixture(t, openAIModel("qwen3-32b"))
	seedTokenWithLastUsedModel(t, f.dir, "tok_1", "qwen3-32b")
	list, err := f.svc.ListTokens(context.Background(), f.owner)
	if err != nil {
		t.Fatalf("ListTokens: %v", err)
	}
	// ListTokens prepends the chat-session pseudo token, so the seeded one is
	// found by id rather than by position.
	var found bool
	for _, dto := range list.Data {
		if dto.ID != "tok_1" {
			continue
		}
		found = true
		if dto.LastUsedModel != "qwen3-32b" {
			t.Fatalf("last_used_model = %q, want qwen3-32b", dto.LastUsedModel)
		}
	}
	if !found {
		t.Fatal("seeded token missing from the listing")
	}
}

func TestUpdateTokenIgnoresLastUsedModelFromTheClient(t *testing.T) {
	// The field exists on the DTO for display; an update request must not be
	// able to forge it, or the redirect target becomes client-controlled. The
	// request is built by decoding a real body, so a field silently added to
	// UpdateTokenRequest later would fail this test rather than pass it.
	ctx := context.Background()
	f := newTokenSettingsFixture(t, openAIModel("qwen3-32b"))
	seedTokenWithLastUsedModel(t, f.dir, "tok_1", "qwen3-32b")
	var req UpdateTokenRequest
	if err := json.Unmarshal([]byte(`{"last_used_model":"forged-model"}`), &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	dto, err := f.svc.UpdateToken(ctx, f.owner, "tok_1", req)
	if err != nil {
		t.Fatalf("UpdateToken: %v", err)
	}
	if dto.LastUsedModel != "qwen3-32b" {
		t.Fatalf("dto last_used_model = %q, want qwen3-32b", dto.LastUsedModel)
	}
	record, err := f.dir.TokenByID(ctx, "tok_1")
	if err != nil {
		t.Fatalf("TokenByID: %v", err)
	}
	if record.LastUsedModel != "qwen3-32b" {
		t.Fatalf("last_used_model changed to %q", record.LastUsedModel)
	}
}

// TestCreateTokenCannotSetLastUsedModel is the create-side half of the
// read-only guarantee (UpdateToken has its own). The request is built by
// decoding a real body that tries to forge the marker, so a LastUsedModel field
// added to CreateTokenRequest later — and assigned into the record — fails this
// test instead of passing it unnoticed.
func TestCreateTokenCannotSetLastUsedModel(t *testing.T) {
	ctx := context.Background()
	f := newTokenSettingsFixture(t, openAIModel("qwen3-32b"))
	var req CreateTokenRequest
	if err := json.Unmarshal([]byte(`{"name":"t","scopes":["gateway:use"],"last_used_model":"forged-model"}`), &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	resp, err := f.svc.CreateToken(ctx, f.owner, req)
	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}
	if resp.Token.LastUsedModel != "" {
		t.Fatalf("dto last_used_model = %q, want empty", resp.Token.LastUsedModel)
	}
	record, err := f.dir.TokenByID(ctx, resp.Token.ID)
	if err != nil {
		t.Fatalf("TokenByID: %v", err)
	}
	if record.LastUsedModel != "" {
		t.Fatalf("record last_used_model = %q, want empty", record.LastUsedModel)
	}
}

func TestUpdateTokenKeepsRulesWhenFieldOmitted(t *testing.T) {
	// A nil pointer means "unchanged" throughout this API; the switches must not
	// silently reset when a client PATCHes an unrelated field.
	f := newTokenSettingsFixture(t, openAIModel("qwen3-32b"))
	seedTokenWithRules(t, f.dir, "tok_1", map[string]store.ModelOverrideRule{
		"gpt-4o": {To: "qwen3-32b", Offer: true, HideTarget: true},
	})
	if _, err := f.svc.UpdateToken(context.Background(), f.owner, "tok_1", UpdateTokenRequest{Name: strPtr("renamed")}); err != nil {
		t.Fatalf("UpdateToken: %v", err)
	}
	rules := storedRules(t, f.dir, "tok_1")
	if !rules["gpt-4o"].Offer || !rules["gpt-4o"].HideTarget {
		t.Fatalf("switches reset by an unrelated update: %#v", rules)
	}
}

// TestOverrideRuleSwitchesRoundTrip is the carried-forward data-loss guard: the
// object row {"to","offer","hide_target"} must survive create -> read -> update
// without either switch being dropped anywhere along the way. Before this task
// the DTO carried the target only, so a save of an unmodified form wiped both.
func TestOverrideRuleSwitchesRoundTrip(t *testing.T) {
	ctx := context.Background()
	f := newTokenSettingsFixture(t, openAIModel("qwen3-32b"))
	resp, err := f.svc.CreateToken(ctx, f.owner, CreateTokenRequest{
		Name: "t", Scopes: []string{"gateway:use"},
		ModelOverrideMap: map[string]store.ModelOverrideRule{
			"gpt-4o": {To: "qwen3-32b", Offer: true, HideTarget: true},
		},
	})
	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}
	created := resp.Token.ModelOverrideMap["gpt-4o"]
	if created.To != "qwen3-32b" || !created.Offer || !created.HideTarget {
		t.Fatalf("create dto row = %#v, want both switches set", created)
	}
	// The read path (a re-fetch through the listing) keeps them too.
	list, err := f.svc.ListTokens(ctx, f.owner)
	if err != nil {
		t.Fatalf("ListTokens: %v", err)
	}
	for _, dto := range list.Data {
		if dto.ID != resp.Token.ID {
			continue
		}
		if row := dto.ModelOverrideMap["gpt-4o"]; !row.Offer || !row.HideTarget {
			t.Fatalf("listed row = %#v, want both switches set", row)
		}
	}
	// And the update path: echoing the map back, exactly as a form save does.
	echoed := resp.Token.ModelOverrideMap
	if _, err := f.svc.UpdateToken(ctx, f.owner, resp.Token.ID, UpdateTokenRequest{ModelOverrideMap: &echoed}); err != nil {
		t.Fatalf("UpdateToken: %v", err)
	}
	rules := storedRules(t, f.dir, resp.Token.ID)
	if !rules["gpt-4o"].Offer || !rules["gpt-4o"].HideTarget {
		t.Fatalf("switches lost on update: %#v", rules)
	}
}

// TestOverrideRuleWireShapeIsAnObject pins the wire format itself: the row is a
// JSON object, and its two switches survive a decode of a real request body.
func TestOverrideRuleWireShapeIsAnObject(t *testing.T) {
	var req CreateTokenRequest
	body := `{"name":"t","model_override_map":{"gpt-4o":{"to":"qwen3-32b","offer":true,"hide_target":true}}}`
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	row := req.ModelOverrideMap["gpt-4o"]
	if row.To != "qwen3-32b" || !row.Offer || !row.HideTarget {
		t.Fatalf("decoded row = %#v", row)
	}
}

// TestUpdateTokenRedirectOffCostsNoOfferingLookup: switching the redirect OFF
// needs no callable-name set at all — validateUnknownModelRedirect returns
// before it looks at one. Building that set costs a mapping traversal plus a
// group-overlay load PER API FLAVOR, so taking it eagerly meant paying for an
// answer the validator threw away. The counters below are the only way to see
// that, since the outcome is identical either way.
func TestUpdateTokenRedirectOffCostsNoOfferingLookup(t *testing.T) {
	ctx := context.Background()
	rs := routing.NewMemoryStore()
	offerModel(t, rs, "srv_lazy", "LazyBox", "app_lazy", []string{routing.APIFlavorOpenAI}, "qwen3-32b", "qwen3-32b-up", routing.ServerStatusActive)
	counted := &countingRoutingStore{MemoryStore: rs}
	dir := NewMemoryDirectory(auth.NewTokenStore())
	if err := dir.CreateUser(ctx, store.User{
		ID: "usr_lazy", Email: "lazy@example.test", DisplayName: "Lazy", Role: "user",
		Status: store.UserStatusActive, PreferredLanguage: "de",
		CreatedAt: offeringTime, UpdatedAt: offeringTime,
	}); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	svc := NewService(ServiceDeps{Users: dir, Tokens: dir, Routes: counted, Clock: func() time.Time { return offeringTime }})
	owner := auth.Token{UserID: "usr_lazy", Scopes: []string{"gateway:use"}}
	resp, err := svc.CreateToken(ctx, owner, CreateTokenRequest{
		Name: "t", Scopes: []string{"gateway:use"},
		UnknownModelRedirect: true, UnknownModelFallback: "qwen3-32b",
	})
	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}

	counted.aiServers, counted.modelGroups, counted.groupMembers = 0, 0, 0
	dto, err := svc.UpdateToken(ctx, owner, resp.Token.ID, UpdateTokenRequest{UnknownModelRedirect: boolPtr(false)})
	if err != nil {
		t.Fatalf("UpdateToken: %v", err)
	}
	if dto.UnknownModelRedirect || dto.UnknownModelFallback != "" {
		t.Fatalf("dto after switching the redirect off = %+v", dto)
	}
	if counted.aiServers != 0 || counted.modelGroups != 0 {
		t.Fatalf("switching the redirect off cost %d mapping traversals and %d overlay loads, want 0 of each",
			counted.aiServers, counted.modelGroups)
	}

	// The set is still built when it is actually needed — otherwise this test
	// would also pass against a validator that never checks the fallback at all.
	counted.aiServers = 0
	if _, err := svc.UpdateToken(ctx, owner, resp.Token.ID, UpdateTokenRequest{
		UnknownModelRedirect: boolPtr(true), UnknownModelFallback: strPtr("qwen3-32b"),
	}); err != nil {
		t.Fatalf("UpdateToken (turning it back on): %v", err)
	}
	if counted.aiServers == 0 {
		t.Fatal("a non-empty fallback was accepted without consulting the callable set")
	}
}

func TestUpdateTokenRedirectSettings(t *testing.T) {
	// The three settings are patchable, and switching the redirect off clears
	// both sub-settings on the stored record, not just in the response.
	ctx := context.Background()
	f := newTokenSettingsFixture(t, openAIModel("qwen3-32b"))
	resp, err := f.svc.CreateToken(ctx, f.owner, CreateTokenRequest{
		Name: "t", Scopes: []string{"gateway:use"},
		UnknownModelRedirect: true, UnknownModelRedirectBlocked: true, UnknownModelFallback: "qwen3-32b",
	})
	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}
	if !resp.Token.UnknownModelRedirect || !resp.Token.UnknownModelRedirectBlocked || resp.Token.UnknownModelFallback != "qwen3-32b" {
		t.Fatalf("created dto = %+v", resp.Token)
	}
	dto, err := f.svc.UpdateToken(ctx, f.owner, resp.Token.ID, UpdateTokenRequest{UnknownModelRedirect: boolPtr(false)})
	if err != nil {
		t.Fatalf("UpdateToken: %v", err)
	}
	if dto.UnknownModelRedirect || dto.UnknownModelRedirectBlocked || dto.UnknownModelFallback != "" {
		t.Fatalf("dto after switching the redirect off = %+v", dto)
	}
	record, err := f.dir.TokenByID(ctx, resp.Token.ID)
	if err != nil {
		t.Fatalf("TokenByID: %v", err)
	}
	if record.UnknownModelRedirect || record.UnknownModelRedirectBlocked || record.UnknownModelFallback != "" {
		t.Fatalf("record after switching the redirect off = %+v", record)
	}
}

func TestUpdateTokenKeepsRedirectSettingsWhenOmitted(t *testing.T) {
	// nil = unchanged, for these three exactly as for every other field.
	ctx := context.Background()
	f := newTokenSettingsFixture(t, openAIModel("qwen3-32b"))
	resp, err := f.svc.CreateToken(ctx, f.owner, CreateTokenRequest{
		Name: "t", Scopes: []string{"gateway:use"},
		UnknownModelRedirect: true, UnknownModelRedirectBlocked: true, UnknownModelFallback: "qwen3-32b",
	})
	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}
	dto, err := f.svc.UpdateToken(ctx, f.owner, resp.Token.ID, UpdateTokenRequest{Name: strPtr("renamed")})
	if err != nil {
		t.Fatalf("UpdateToken: %v", err)
	}
	if !dto.UnknownModelRedirect || !dto.UnknownModelRedirectBlocked || dto.UnknownModelFallback != "qwen3-32b" {
		t.Fatalf("redirect settings changed by an unrelated update: %+v", dto)
	}
}

func TestUpdateTokenRejectsUnknownFallback(t *testing.T) {
	ctx := context.Background()
	f := newTokenSettingsFixture(t, openAIModel("qwen3-32b"))
	resp, err := f.svc.CreateToken(ctx, f.owner, CreateTokenRequest{Name: "t", Scopes: []string{"gateway:use"}})
	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}
	_, err = f.svc.UpdateToken(ctx, f.owner, resp.Token.ID, UpdateTokenRequest{
		UnknownModelRedirect: boolPtr(true), UnknownModelFallback: strPtr("no-such-model"),
	})
	if !errors.Is(err, ErrTokenModelOverrideInvalid) {
		t.Fatalf("err = %v, want ErrTokenModelOverrideInvalid", err)
	}
}

// TestTokenDefaultsStayOff pins that an existing token — one created without
// any of the new fields — reads back with every one of them off/empty.
func TestTokenDefaultsStayOff(t *testing.T) {
	f := newTokenSettingsFixture(t, openAIModel("qwen3-32b"))
	resp, err := f.svc.CreateToken(context.Background(), f.owner, CreateTokenRequest{Name: "plain", Scopes: []string{"gateway:use"}})
	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}
	dto := resp.Token
	if dto.UnknownModelRedirect || dto.UnknownModelRedirectBlocked || dto.UnknownModelFallback != "" || dto.LastUsedModel != "" {
		t.Fatalf("new token is not neutral: %+v", dto)
	}
}
