// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/store"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// Discovery side of the Claude model aliases (issue #225): the token-scoped
// derivation (claudeAliasesForToken / claudeAliasesFromListing) and the overlay
// that turns its result into rows on the usage path (Models, ModelsForFlavor).
// The pure derivation (hash, fallback, collisions) is tested in
// claude_aliases_test.go; these tests pin WHICH models the discovery paths feed
// it and what the finished rows look like.
//
// None of these tests call t.Parallel(), like the rest of the alias tests.

// claudeModelSpec is one local gateway model of the alias discovery fixtures.
type claudeModelSpec struct {
	name        string
	flavors     []string
	messagesOff bool // the application's Messages endpoint is disabled
	contextSize int
	vision      bool
	image       bool
}

// seedClaudeModel seeds a server + vLLM application + one active mapping for
// spec, with the vision/image capability rows it asks for. Each idx gets its own
// server/application ids so a model's loaded state is attributable to one app
// (app_cla_<idx>).
func seedClaudeModel(t *testing.T, rs *routing.MemoryStore, idx int, spec claudeModelSpec) {
	t.Helper()
	ctx := context.Background()
	srvID, appID, mappingID := fmt.Sprintf("srv_cla_%d", idx), fmt.Sprintf("app_cla_%d", idx), fmt.Sprintf("map_cla_%d", idx)
	if err := rs.CreateAIServer(ctx, routing.AIServer{ID: srvID, Name: fmt.Sprintf("ClaBox%d", idx), Domain: srvID + ".test", Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: offeringTime, UpdatedAt: offeringTime}); err != nil {
		t.Fatalf("CreateAIServer %s: %v", srvID, err)
	}
	messages := routing.EndpointModeTranslate
	if spec.messagesOff {
		messages = routing.EndpointModeDisabled
	}
	if err := rs.CreateApplication(ctx, routing.Application{ID: appID, ServerID: srvID, Type: routing.ProviderVLLM, Port: 8000, Scheme: "https", APIFlavors: spec.flavors, ResponsesMode: routing.EndpointModeTranslate, MessagesMode: messages, Status: routing.ServerStatusActive, CreatedAt: offeringTime, UpdatedAt: offeringTime}); err != nil {
		t.Fatalf("CreateApplication %s: %v", appID, err)
	}
	if err := rs.CreateMapping(ctx, routing.ModelMapping{ID: mappingID, ApplicationID: appID, GatewayModelName: spec.name, AppModelName: spec.name + "-up", Status: routing.ServerStatusActive, ContextSize: spec.contextSize, CreatedAt: offeringTime, UpdatedAt: offeringTime}); err != nil {
		t.Fatalf("CreateMapping %s: %v", spec.name, err)
	}
	var rows []routing.CapabilityRow
	if spec.vision {
		rows = append(rows, routing.CapabilityRow{Capability: routing.CapabilityVision, Verdict: routing.CapabilityYes, Source: routing.CapabilitySourceVisionBenchmark, CheckedAt: offeringTime})
	}
	if spec.image {
		rows = append(rows, routing.CapabilityRow{Capability: routing.CapabilityImage, Verdict: routing.CapabilityYes, Source: routing.CapabilitySourceLlamaCppProps, CheckedAt: offeringTime})
	}
	if len(rows) > 0 {
		if err := rs.UpsertMappingCapabilities(ctx, mappingID, rows); err != nil {
			t.Fatalf("UpsertMappingCapabilities %s: %v", mappingID, err)
		}
	}
}

// claudeAliasedInWorld is the exact set of gateway ids newClaudeAliasWorld
// offers a generated alias for; claudeExcludedInWorld is every other real
// name of the fixture, each excluded for the reason named at its seed.
var (
	claudeAliasedInWorld  = []string{"local-ok", "local-all", "member-shown", "team"}
	claudeExcludedInWorld = []string{
		"local-openai-only", "local-images-only", "local-messages-off",
		"local-hidden", "local-locked", "member-hidden",
		"hidden-grp", "locked-grp", "openai-grp",
	}
)

// newClaudeAliasWorld seeds one local model per eligibility case. Only
// claudeAliasedInWorld may get an alias:
//
//   - local-ok: a plain text model (context size and vision to inherit).
//   - local-all: serves openai + anthropic + images, generates images, and is
//     loaded (the inherited image/image_edit/loaded payload).
//   - member-shown + team: a shown group over a hidden and a shown member.
//
// and nothing else:
//
//   - local-openai-only: does not serve the Messages flavor.
//   - local-images-only: images only.
//   - local-messages-off: declares anthropic but the Messages endpoint is
//     disabled, so the served rule drops the flavor.
//   - local-hidden / local-locked: model_settings hides / locks the name.
//   - member-hidden: a hidden member of the shown group "team".
//   - hidden-grp / locked-grp: groups whose own visibility is hidden / locked.
//   - openai-grp: a shown group over an openai-only member, so no anthropic.
func newClaudeAliasWorld(t *testing.T) (*Service, *routing.MemoryStore) {
	t.Helper()
	rs := routing.NewMemoryStore()
	both := []string{flavorOpenAI, flavorAnthropic}
	specs := []claudeModelSpec{
		{name: "local-ok", flavors: both, contextSize: 32768, vision: true},
		{name: "local-all", flavors: []string{flavorOpenAI, flavorAnthropic, flavorImages}, image: true},
		{name: "local-openai-only", flavors: []string{flavorOpenAI}},
		{name: "local-images-only", flavors: []string{flavorImages}},
		{name: "local-messages-off", flavors: both, messagesOff: true},
		{name: "local-hidden", flavors: both},
		{name: "local-locked", flavors: both},
		{name: "member-hidden", flavors: both},
		{name: "member-shown", flavors: both},
	}
	for i, spec := range specs {
		seedClaudeModel(t, rs, i, spec)
	}
	offerVisibility(t, rs, "local-hidden", "hidden")
	offerVisibility(t, rs, "local-locked", "locked")
	offerVisibility(t, rs, "member-hidden", "hidden")
	offerGroup(t, rs, "grp_team", "team", "member-hidden", "member-shown")
	offerGroup(t, rs, "grp_hidden", "hidden-grp", "member-shown")
	offerVisibility(t, rs, "hidden-grp", "hidden")
	offerGroup(t, rs, "grp_locked", "locked-grp", "member-shown")
	offerVisibility(t, rs, "locked-grp", "locked")
	offerGroup(t, rs, "grp_openai", "openai-grp", "local-openai-only")
	// local-all (app_cla_1) reports its upstream model as loaded.
	loaded := fakeLoadedModels{byApp: map[string][]string{"app_cla_1": {"local-all-up"}}}
	return offerSvc(rs, loaded), rs
}

// claudeToken is a user token with the generated-alias setting on and the given
// manual override rules.
func claudeToken(rules map[string]store.ModelOverrideRule) auth.Token {
	return auth.Token{UserID: "usr_cla", ClaudeModelAliases: true, ModelOverrideRules: store.AuthModelOverrideRules(rules)}
}

// claudeAliasIDOf is claudeAliasID for a fixture id that must have an alias id.
func claudeAliasIDOf(t *testing.T, gatewayID string) string {
	t.Helper()
	id, ok := claudeAliasID(gatewayID)
	if !ok {
		t.Fatalf("claudeAliasID(%q) has no filter-safe window", gatewayID)
	}
	return id
}

// claudeAliasRows returns the generated alias rows of a listing, keyed by the
// gateway id they point at. A row is generated exactly when it carries
// OriginalModelID.
func claudeAliasRows(resp ModelsResponse) map[string]ModelDTO {
	rows := make(map[string]ModelDTO)
	for _, d := range resp.Data {
		if d.OriginalModelID != "" {
			rows[d.OriginalModelID] = d
		}
	}
	return rows
}

func sortedKeys[V any](m map[string]V) []string {
	return slices.Sorted(maps.Keys(m))
}

// TestClaudeAliasAddsARowForEveryEligibleModelAndGroup: with the setting on, an
// eligible local model or group is listed under its anthropic-local id IN
// ADDITION to its own row, and the alias row is that row filed under the alias
// id: same flavors, loaded/offered state, context size, vision/image/image_edit
// and group flag. Only OriginalModelID (the real gateway id) and the honest
// DisplayName are the alias row's own.
func TestClaudeAliasAddsARowForEveryEligibleModelAndGroup(t *testing.T) {
	svc, _ := newClaudeAliasWorld(t)
	resp := svc.Models(context.Background(), claudeToken(nil))
	byID := modelsByID(resp)
	rows := claudeAliasRows(resp)

	if got, want := sortedKeys(rows), sortedCopy(claudeAliasedInWorld); !reflect.DeepEqual(got, want) {
		t.Fatalf("aliased targets = %v, want %v", got, want)
	}
	for _, original := range claudeAliasedInWorld {
		alias := rows[original]
		if want := claudeAliasIDOf(t, original); alias.ID != want {
			t.Errorf("alias of %s has id %q, want %q", original, alias.ID, want)
		}
		real, ok := byID[original]
		if !ok {
			t.Errorf("the original %s row is gone: the alias is offered in addition to it", original)
			continue
		}
		if real.OriginalModelID != "" {
			t.Errorf("the real row %s carries original_model_id %q; only alias rows may", original, real.OriginalModelID)
		}
		if alias.DisplayName != original {
			t.Errorf("alias of %s has display_name %q, want the honest label %q", original, alias.DisplayName, original)
		}
		// The alias row is the target's row under another name.
		want := real
		want.ID, want.OriginalModelID = alias.ID, original
		if !reflect.DeepEqual(alias, want) {
			t.Errorf("alias of %s = %#v, want the target's row under the alias id: %#v", original, alias, want)
		}
	}

	// The payload comparison above must not be vacuous.
	if ok := byID["local-ok"]; ok.ContextSize != 32768 || !ok.Vision || !slices.Equal(ok.Flavors, []string{flavorAnthropic, flavorOpenAI}) {
		t.Fatalf("local-ok fixture lost its payload: %#v", ok)
	}
	if all := byID["local-all"]; !all.Loaded || !all.Image || !all.ImageEdit || all.OfferedOnCount != 1 {
		t.Fatalf("local-all fixture lost its payload: %#v", all)
	}
	if team := byID["team"]; !team.IsGroup {
		t.Fatalf("team fixture is not a group row: %#v", team)
	}
}

// TestClaudeAliasHonestLabelNeverNamesAClaudeModel: the alias row's label is
// the target's own, whatever the target is called, never a product name.
func TestClaudeAliasHonestLabelNeverNamesAClaudeModel(t *testing.T) {
	svc, _ := newClaudeAliasWorld(t)
	seen := 0
	for _, d := range svc.Models(context.Background(), claudeToken(nil)).Data {
		if d.OriginalModelID == "" {
			continue
		}
		seen++
		if !strings.HasPrefix(d.ID, claudeAliasIDPrefix) {
			t.Errorf("alias row id %q lacks the %q prefix", d.ID, claudeAliasIDPrefix)
		}
		if lower := strings.ToLower(d.DisplayName); strings.Contains(lower, "claude") || strings.Contains(lower, "anthropic") {
			t.Errorf("alias row %s has display_name %q naming a Claude/Anthropic product", d.ID, d.DisplayName)
		}
	}
	if seen != len(claudeAliasedInWorld) {
		t.Fatalf("saw %d alias rows, want %d", seen, len(claudeAliasedInWorld))
	}
}

// TestClaudeAliasReachesTheFlavorListing: the per-flavor discovery
// (ModelsForFlavor, i.e. /anthropic/v1/models and the openai listing) is
// composed apart from Models(), and must offer the same aliases under the
// flavors the target serves.
func TestClaudeAliasReachesTheFlavorListing(t *testing.T) {
	svc, _ := newClaudeAliasWorld(t)
	for _, flavor := range []string{flavorAnthropic, flavorOpenAI} {
		got := svc.ModelsForFlavor(context.Background(), claudeToken(nil), flavor)
		for _, original := range claudeAliasedInWorld {
			if id := claudeAliasIDOf(t, original); !containsString(got, id) {
				t.Errorf("%s listing is missing the alias %s of %s: %v", flavor, id, original, got)
			}
		}
	}
	// An alias is listed under the flavors its target serves, never more.
	images := svc.ModelsForFlavor(context.Background(), claudeToken(nil), flavorImages)
	if id := claudeAliasIDOf(t, "local-all"); !containsString(images, id) {
		t.Errorf("images listing is missing %s: local-all serves images", id)
	}
	for _, original := range []string{"local-ok", "member-shown", "team"} {
		if id := claudeAliasIDOf(t, original); containsString(images, id) {
			t.Errorf("images listing has %s: %s does not serve images", id, original)
		}
	}
}

// TestClaudeAliasIsOffWithoutTheSetting: the flag defaults off and off means no
// behavior change at all, on both usage listings.
func TestClaudeAliasIsOffWithoutTheSetting(t *testing.T) {
	svc, _ := newClaudeAliasWorld(t)
	ctx := context.Background()
	off := claudeToken(nil)
	off.ClaudeModelAliases = false

	on := svc.Models(ctx, claudeToken(nil))
	got := svc.Models(ctx, off)
	if rows := claudeAliasRows(got); len(rows) != 0 {
		t.Fatalf("flag off yet alias rows listed: %v", sortedKeys(rows))
	}
	if len(got.Data) >= len(on.Data) {
		t.Fatalf("flag off lists %d rows, flag on %d: the setting changed nothing", len(got.Data), len(on.Data))
	}
	for _, d := range got.Data {
		if strings.HasPrefix(d.ID, claudeAliasIDPrefix) {
			t.Errorf("flag off yet row %q is an anthropic-local id", d.ID)
		}
	}
	for _, flavor := range []string{flavorAnthropic, flavorOpenAI, flavorImages} {
		for _, id := range svc.ModelsForFlavor(ctx, off, flavor) {
			if strings.HasPrefix(id, claudeAliasIDPrefix) {
				t.Errorf("flag off yet the %s listing has %q", flavor, id)
			}
		}
	}
}

// TestClaudeAliasSessionPrincipalGetsNoAliases: the token-less portal session
// principal (gateway/auth.go sessionPrincipal) never sets the flag, by design:
// generated aliases serve external bearer-token clients (Claude Desktop), not
// the portal UI, whose chat picker lists real models. A principal of that shape
// therefore sees no alias rows, whatever the fixture offers.
func TestClaudeAliasSessionPrincipalGetsNoAliases(t *testing.T) {
	svc, _ := newClaudeAliasWorld(t)
	ctx := context.Background()
	// The shape of sessionPrincipal: no token id, gateway:use (+admin) scopes,
	// the user's own vendor accounts, and ClaudeModelAliases left at its zero.
	session := auth.Token{
		UserID: "usr_cla", Name: "Session User", Active: true,
		Scopes:       []string{"gateway:use", "admin"},
		VendorAccess: auth.VendorAccess{All: true},
	}
	if rows := claudeAliasRows(svc.Models(ctx, session)); len(rows) != 0 {
		t.Fatalf("session principal sees alias rows: %v", sortedKeys(rows))
	}
	for _, id := range svc.ModelsForFlavor(ctx, session, flavorAnthropic) {
		if strings.HasPrefix(id, claudeAliasIDPrefix) {
			t.Fatalf("session principal's anthropic listing has %q", id)
		}
	}
}

// TestClaudeAliasExclusions: no alias, and no trace of the model, for anything
// that is not an offered, Messages-serving local model or group.
func TestClaudeAliasExclusions(t *testing.T) {
	svc, _ := newClaudeAliasWorld(t)
	ctx := context.Background()
	token := claudeToken(nil)
	resp := svc.Models(ctx, token)
	byID := modelsByID(resp)
	rows := claudeAliasRows(resp)

	// The excluded names that ARE listed (the rest are suppressed) really are
	// offered as real rows, so the missing alias is the rule at work and not an
	// absent fixture.
	for _, name := range []string{"local-openai-only", "local-images-only", "local-messages-off", "openai-grp"} {
		if _, ok := byID[name]; !ok {
			t.Errorf("excluded real model %s is not listed at all", name)
		}
	}

	for _, excluded := range claudeExcludedInWorld {
		aliasID := claudeAliasIDOf(t, excluded)
		if _, ok := rows[excluded]; ok {
			t.Errorf("%s got an alias row", excluded)
		}
		if _, ok := byID[aliasID]; ok {
			t.Errorf("the alias id %s of excluded %s is listed", aliasID, excluded)
		}
		for _, flavor := range []string{flavorAnthropic, flavorOpenAI, flavorImages} {
			if containsString(svc.ModelsForFlavor(ctx, token, flavor), aliasID) {
				t.Errorf("the %s listing carries the alias id %s of excluded %s", flavor, aliasID, excluded)
			}
		}
	}

	// No leak through an alias row: its serialized form names none of the
	// excluded models. The group alias "team" has a hidden member; the row
	// names the group only.
	for original, alias := range rows {
		raw, err := json.Marshal(alias)
		if err != nil {
			t.Fatalf("marshal alias of %s: %v", original, err)
		}
		for _, excluded := range claudeExcludedInWorld {
			if strings.Contains(string(raw), excluded) {
				t.Errorf("alias row of %s names excluded model %s: %s", original, excluded, raw)
			}
		}
	}
	if team, ok := rows["team"]; !ok || team.OriginalModelID != "team" || !team.IsGroup {
		t.Fatalf("group alias row = %#v, want original_model_id team and is_group", team)
	}
}

// TestClaudeAliasNeverTargetsVendorAccountModels: a vendor-account model, an
// Anthropic one included, is never aliased. It is not a mapping view, so the
// derivation (which reads only the server/group composition) cannot see it; a
// LOCAL model whose name the token's vendor account also serves is skipped as
// well, because the resolver tries the vendor account first (vendor_first), so
// an alias onto that name would dispatch to the vendor model.
func TestClaudeAliasNeverTargetsVendorAccountModels(t *testing.T) {
	ctx := context.Background()
	rs := routing.NewMemoryStore()
	both := []string{flavorOpenAI, flavorAnthropic}
	seedClaudeModel(t, rs, 0, claudeModelSpec{name: "local-ok", flavors: both})
	seedClaudeModel(t, rs, 1, claudeModelSpec{name: "shadowed", flavors: both})
	svc := offerSvc(rs, nil)
	setVendorAccountsEnabled(t, svc, true)
	seedVendor := func(id, vendor, prefix, upstream string) {
		t.Helper()
		if err := rs.CreateVendorAccount(ctx, routing.VendorAccount{
			ID: id, OwnerUserID: "usr_cla", Vendor: vendor, AuthType: routing.VendorAuthAPIKey,
			Name: id, Status: routing.VendorAccountStatusActive, ModelPrefix: prefix,
			CreatedAt: offeringTime, UpdatedAt: offeringTime,
		}); err != nil {
			t.Fatalf("CreateVendorAccount %s: %v", id, err)
		}
		if err := rs.SetVendorAccountModels(ctx, id, []routing.VendorAccountModel{
			{GatewayModel: prefix + upstream, UpstreamModel: upstream, APIFlavor: routing.APIFlavorAnthropic},
		}); err != nil {
			t.Fatalf("SetVendorAccountModels %s: %v", id, err)
		}
	}
	seedVendor("acc_anthropic", routing.VendorAnthropic, "anthropic/", "claude-sonnet-4-5")
	seedVendor("acc_bare", routing.VendorOpenAI, "", "shadowed")
	token := claudeToken(nil)
	token.VendorAccess = auth.VendorAccess{All: true}

	resp := svc.Models(ctx, token)
	byID := modelsByID(resp)
	for _, vendorModel := range []string{"anthropic/claude-sonnet-4-5", "shadowed"} {
		row, ok := byID[vendorModel]
		if !ok {
			t.Fatalf("precondition: %s is not listed: %v", vendorModel, sortedKeys(byID))
		}
		if row.OriginalModelID != "" {
			t.Errorf("vendor-served row %s carries original_model_id %q", vendorModel, row.OriginalModelID)
		}
	}
	rows := claudeAliasRows(resp)
	if got, want := sortedKeys(rows), []string{"local-ok"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("aliased targets = %v, want only the local model %v", got, want)
	}
	for _, vendorModel := range []string{"anthropic/claude-sonnet-4-5", "shadowed"} {
		aliasID := claudeAliasIDOf(t, vendorModel)
		if _, ok := byID[aliasID]; ok {
			t.Errorf("an alias id %s for vendor model %s is listed", aliasID, vendorModel)
		}
		if containsString(svc.ModelsForFlavor(ctx, token, flavorAnthropic), aliasID) {
			t.Errorf("the anthropic listing carries an alias id %s for vendor model %s", aliasID, vendorModel)
		}
	}
	// The same token without vendor access is not shadowed, so the local model
	// of that name is an ordinary eligible one again.
	noVendor := claudeToken(nil)
	if got, want := sortedKeys(claudeAliasRows(svc.Models(ctx, noVendor))), []string{"local-ok", "shadowed"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("without vendor access aliased targets = %v, want %v", got, want)
	}
}

// TestClaudeAliasNeverOnAdminManageModels: ManageModels() is the admin
// management surface and shows the system's real models, never one token's
// generated aliases, even for a token with the setting on.
func TestClaudeAliasNeverOnAdminManageModels(t *testing.T) {
	svc, _ := newClaudeAliasWorld(t)
	resp := svc.ManageModels(context.Background(), claudeToken(nil))
	if rows := claudeAliasRows(resp); len(rows) != 0 {
		t.Fatalf("ManageModels lists alias rows: %v", sortedKeys(rows))
	}
	for _, d := range resp.Data {
		if strings.HasPrefix(d.ID, claudeAliasIDPrefix) {
			t.Errorf("ManageModels lists %q", d.ID)
		}
	}
	// Real models only, hidden/locked ones included (the admin must see them).
	byID := modelsByID(resp)
	for _, name := range []string{"local-ok", "local-hidden", "local-locked", "team", "hidden-grp"} {
		if _, ok := byID[name]; !ok {
			t.Errorf("ManageModels is missing the real model %s", name)
		}
	}
}

// TestClaudeAliasManualRuleForTheSameIDWins: a manual model_override_map rule
// keyed by an id the generated alias would use takes the id. There is one row
// for it, it is the manual row (no automatic original_model_id: a manual alias
// may hide its target on purpose), and the manual rule's offer switch decides
// whether it is listed at all.
func TestClaudeAliasManualRuleForTheSameIDWins(t *testing.T) {
	svc, _ := newClaudeAliasWorld(t)
	ctx := context.Background()
	aliasID := claudeAliasIDOf(t, "local-ok")

	rowsWithID := func(resp ModelsResponse) []ModelDTO {
		var out []ModelDTO
		for _, d := range resp.Data {
			if d.ID == aliasID {
				out = append(out, d)
			}
		}
		return out
	}
	cases := []struct {
		name     string
		rule     store.ModelOverrideRule
		wantRows int
		wantImg  bool // the listed row is local-all's (the manual rule's target)
	}{
		{name: "same target offered", rule: store.ModelOverrideRule{To: "local-ok", Offer: true}, wantRows: 1},
		{name: "other target offered", rule: store.ModelOverrideRule{To: "local-all", Offer: true}, wantRows: 1, wantImg: true},
		{name: "not offered", rule: store.ModelOverrideRule{To: "local-ok", Offer: false}, wantRows: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			token := claudeToken(map[string]store.ModelOverrideRule{aliasID: tc.rule})
			resp := svc.Models(ctx, token)
			rows := rowsWithID(resp)
			if len(rows) != tc.wantRows {
				t.Fatalf("%d rows with id %s, want %d: %#v", len(rows), aliasID, tc.wantRows, rows)
			}
			if tc.wantRows == 1 {
				if rows[0].OriginalModelID != "" {
					t.Errorf("manual row carries automatic original_model_id %q", rows[0].OriginalModelID)
				}
				if rows[0].Image != tc.wantImg {
					t.Errorf("manual row image = %v, want %v (the rule's target)", rows[0].Image, tc.wantImg)
				}
			}
			if _, ok := claudeAliasRows(resp)["local-ok"]; ok {
				t.Error("local-ok still has a generated alias row beside the manual rule that owns its id")
			}
			// The manual rule must not disturb the other models' aliases.
			if _, ok := claudeAliasRows(resp)["member-shown"]; !ok {
				t.Error("member-shown lost its alias")
			}
			if n := strings.Count(strings.Join(svc.ModelsForFlavor(ctx, token, flavorAnthropic), "\n"), aliasID); n != tc.wantRows {
				t.Errorf("anthropic listing has id %s %d times, want %d", aliasID, n, tc.wantRows)
			}
		})
	}
}

// TestClaudeAliasesNeverDeriveFromManualAliases: only real model/group
// identities are aliased. A manual alias row is another name, so it never gets
// an alias of its own, and no alias points at another alias.
func TestClaudeAliasesNeverDeriveFromManualAliases(t *testing.T) {
	svc, _ := newClaudeAliasWorld(t)
	ctx := context.Background()
	token := claudeToken(map[string]store.ModelOverrideRule{"claude-x": {To: "local-ok", Offer: true}})
	resp := svc.Models(ctx, token)
	byID := modelsByID(resp)
	if _, ok := byID["claude-x"]; !ok {
		t.Fatal("precondition: the manual alias claude-x is not listed")
	}
	rows := claudeAliasRows(resp)
	if _, ok := rows["claude-x"]; ok {
		t.Error("the manual alias claude-x got a generated alias")
	}
	if _, ok := byID[claudeAliasIDOf(t, "claude-x")]; ok {
		t.Error("the alias id of the manual alias is listed")
	}
	for original, alias := range rows {
		if strings.HasPrefix(original, claudeAliasIDPrefix) {
			t.Errorf("alias %s points at another alias %s", alias.ID, original)
		}
		if _, isReal := byID[original]; !isReal {
			t.Errorf("alias %s points at %s, which is not a listed real model", alias.ID, original)
		}
	}
	if got, want := sortedKeys(rows), sortedCopy(claudeAliasedInWorld); !reflect.DeepEqual(got, want) {
		t.Fatalf("aliased targets = %v, want %v: a manual alias must not change them", got, want)
	}
}

// TestClaudeAliasSkipsTargetsAManualRuleHidesOrRewrites: the token's own manual
// rules are an instruction about what it sees. A target a rule hides
// (hide_target) stays out of this token's discovery, and a generated alias would
// reveal it by name; a real name a rule is keyed on means whatever the rule
// says for this token, so an alias onto the unrewritten model would disagree
// with it. Other models keep their aliases.
func TestClaudeAliasSkipsTargetsAManualRuleHidesOrRewrites(t *testing.T) {
	svc, _ := newClaudeAliasWorld(t)
	ctx := context.Background()
	token := claudeToken(map[string]store.ModelOverrideRule{
		"claude-x":  {To: "local-ok", Offer: true, HideTarget: true},
		"local-all": {To: "member-shown"},
	})
	resp := svc.Models(ctx, token)
	rows := claudeAliasRows(resp)
	if got, want := sortedKeys(rows), []string{"member-shown", "team"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("aliased targets = %v, want %v", got, want)
	}
	byID := modelsByID(resp)
	for _, skipped := range []string{"local-ok", "local-all"} {
		if _, ok := byID[claudeAliasIDOf(t, skipped)]; ok {
			t.Errorf("the alias id of %s is listed", skipped)
		}
		if containsString(svc.ModelsForFlavor(ctx, token, flavorAnthropic), claudeAliasIDOf(t, skipped)) {
			t.Errorf("the anthropic listing has the alias id of %s", skipped)
		}
	}
}

// TestClaudeAliasIsSkippedWhenItsIDIsARealName: an alias id equal to a real
// model name, a hidden one included, is never generated, so a real model is
// never shadowed (nor rewritten at resolution time).
func TestClaudeAliasIsSkippedWhenItsIDIsARealName(t *testing.T) {
	ctx := context.Background()
	rs := routing.NewMemoryStore()
	both := []string{flavorOpenAI, flavorAnthropic}
	clash1, clash2 := claudeAliasIDOf(t, "m1"), claudeAliasIDOf(t, "m2")
	for i, name := range []string{"m1", "m2", "m3", clash1, clash2} {
		seedClaudeModel(t, rs, i, claudeModelSpec{name: name, flavors: both})
	}
	offerVisibility(t, rs, clash2, "hidden")
	svc := offerSvc(rs, nil)

	resp := svc.Models(ctx, claudeToken(nil))
	rows := claudeAliasRows(resp)
	for _, original := range []string{"m1", "m2"} {
		if _, ok := rows[original]; ok {
			t.Errorf("%s got an alias although its alias id is a real model name", original)
		}
	}
	// m3 has no clash; clash1 (a real, shown model) and clash2 (hidden) are
	// themselves eligible/ineligible like any other model.
	if _, ok := rows["m3"]; !ok {
		t.Errorf("m3 lost its alias: %v", sortedKeys(rows))
	}
	byID := modelsByID(resp)
	if real, ok := byID[clash1]; !ok || real.OriginalModelID != "" {
		t.Errorf("the real model named %s was replaced by an alias row: %#v", clash1, real)
	}
	if _, ok := byID[clash2]; ok {
		t.Errorf("the hidden real model %s is listed", clash2)
	}
}

// TestClaudeAliasFailsClosedWhenVisibilityIsUnknown: the listing fails OPEN on
// an overlay store error (it must never blank the model list), so a hidden or
// locked model would be offered. The alias derivation must not follow it: it
// cannot tell hidden from shown, and an alias would reveal a model the
// operator hid. No aliases while the overlay is unavailable; real rows stay.
func TestClaudeAliasFailsClosedWhenVisibilityIsUnknown(t *testing.T) {
	ctx := context.Background()
	working, rs := newClaudeAliasWorld(t)
	svc := offerSvc(&failingModelSettingsStore{MemoryStore: rs, err: errors.New("model_settings unavailable")}, nil)
	token := claudeToken(nil)
	if rows := claudeAliasRows(working.Models(ctx, token)); len(rows) != len(claudeAliasedInWorld) {
		t.Fatalf("precondition: the same fixture with a working store lists %d aliases, want %d", len(rows), len(claudeAliasedInWorld))
	}

	resp := svc.Models(ctx, token)
	if rows := claudeAliasRows(resp); len(rows) != 0 {
		t.Fatalf("aliases listed while visibility is unknown: %v", sortedKeys(rows))
	}
	if _, ok := modelsByID(resp)["local-ok"]; !ok {
		t.Fatal("the real listing went blank with the overlay: it must fail open")
	}
	for _, id := range svc.ModelsForFlavor(ctx, token, flavorAnthropic) {
		if strings.HasPrefix(id, claudeAliasIDPrefix) {
			t.Errorf("the anthropic listing has %q while visibility is unknown", id)
		}
	}
	if aliases, _ := svc.claudeAliasesForToken(ctx, token); len(aliases) != 0 {
		t.Fatalf("claudeAliasesForToken derives %v while visibility is unknown", aliases)
	}
}

// TestClaudeAliasesForTokenIsWhatDiscoveryLists: the token-scoped helper is the
// one derivation resolution reuses (issue #225, Task 7), so it must return
// exactly the aliases both discovery paths list, for every token shape.
func TestClaudeAliasesForTokenIsWhatDiscoveryLists(t *testing.T) {
	svc, _ := newClaudeAliasWorld(t)
	ctx := context.Background()
	aliasID := claudeAliasIDOf(t, "local-ok")
	cases := []struct {
		name  string
		token auth.Token
		want  int // aliases the fixture yields for this token
	}{
		{"plain", claudeToken(nil), 4},
		{"manual key", claudeToken(map[string]store.ModelOverrideRule{aliasID: {To: "local-all", Offer: true}}), 3},
		{"hide target", claudeToken(map[string]store.ModelOverrideRule{"claude-x": {To: "member-shown", Offer: true, HideTarget: true}}), 3},
		{"flag off", auth.Token{UserID: "usr_cla"}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			derived, _ := svc.claudeAliasesForToken(ctx, tc.token)
			if len(derived) != tc.want {
				t.Fatalf("helper derives %d aliases, want %d: %v", len(derived), tc.want, derived)
			}
			rows := claudeAliasRows(svc.Models(ctx, tc.token))
			if len(derived) != len(rows) {
				t.Fatalf("helper derives %d aliases, Models() lists %d", len(derived), len(rows))
			}
			listing := svc.ModelsForFlavor(ctx, tc.token, flavorAnthropic)
			for aliasID, alias := range derived {
				row, ok := rows[alias.OriginalID]
				if !ok || row.ID != aliasID || row.DisplayName != alias.DisplayName {
					t.Errorf("helper alias %s -> %#v is not the Models() row %#v", aliasID, alias, row)
				}
				if !containsString(listing, aliasID) {
					t.Errorf("helper alias %s is not in the anthropic listing", aliasID)
				}
			}
		})
	}
}

// TestClaudeAliasEligible pins the eligibility rules as a pure unit: the
// alias-free listing's names that serve the anthropic flavor, minus every name a
// manual rule is keyed on or hides and every name a vendor account serves.
func TestClaudeAliasEligible(t *testing.T) {
	listed := map[string]map[string]struct{}{
		"serves":      flavorSet(flavorAnthropic, flavorOpenAI),
		"anthropic":   flavorSet(flavorAnthropic),
		"openai-only": flavorSet(flavorOpenAI),
		"images-only": flavorSet(flavorImages),
		"empty":       flavorSet(),
		"keyed":       flavorSet(flavorAnthropic),
		"hidden-by":   flavorSet(flavorAnthropic),
		"vendor":      flavorSet(flavorAnthropic),
	}
	rules := map[string]auth.ModelOverrideRule{
		"keyed":   {To: "serves"},
		"manual":  {To: "hidden-by", Offer: true, HideTarget: true},
		"offered": {To: "serves", Offer: true},
	}
	vendor := map[string]map[string]struct{}{"vendor": flavorSet(flavorOpenAI, flavorAnthropic), "elsewhere": flavorSet(flavorAnthropic)}

	got := claudeAliasEligible(listed, rules, vendor)
	if want := []string{"anthropic", "serves"}; !slices.Equal(got, want) {
		t.Fatalf("eligible = %v, want %v (sorted)", got, want)
	}
	if got := claudeAliasEligible(nil, nil, nil); len(got) != 0 {
		t.Fatalf("eligible over nothing = %v", got)
	}
}
