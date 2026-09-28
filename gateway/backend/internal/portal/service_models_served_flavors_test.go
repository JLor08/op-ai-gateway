// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"errors"
	"fmt"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/store"
	"reflect"
	"testing"
)

const (
	flavorOpenAI    = routing.APIFlavorOpenAI
	flavorAnthropic = routing.APIFlavorAnthropic
	flavorImages    = routing.APIFlavorOpenAIImages
)

// servedShape is one gateway model of the served-rule tables: the type,
// flavors (A) and messages mode of the application that offers it and, when
// hasSpec is set, the flavors and messages mode of its runtime spec row. served
// is the sorted set every listing carries and Callable holds
// (routing.MappingServesAPIFlavor); half is the sorted set Existing holds (the
// flavor half, routing.MappingHasAPIFlavor).
type servedShape struct {
	name         string
	appType      string
	appFlavors   []string
	appMessages  routing.EndpointMode
	hasSpec      bool
	specFlavors  []string
	specMessages routing.EndpointMode
	served       []string
	half         []string
}

// seedServedShape seeds shape's application, one active mapping and, with
// hasSpec, its runtime spec row on the existing server srvID. The spec is
// stored with Enabled false, which the rule ignores, as dispatch does.
func seedServedShape(t *testing.T, rs *routing.MemoryStore, srvID, appID string, shape servedShape) {
	t.Helper()
	ctx := context.Background()
	if err := rs.CreateApplication(ctx, routing.Application{
		ID: appID, ServerID: srvID, Type: shape.appType, Port: 8081, Scheme: "http",
		APIFlavors: shape.appFlavors, ResponsesMode: routing.EndpointModeTranslate, MessagesMode: shape.appMessages,
		Status: routing.ServerStatusActive, CreatedAt: offeringTime, UpdatedAt: offeringTime,
	}); err != nil {
		t.Fatalf("CreateApplication %s: %v", appID, err)
	}
	mappingID := appID + "_map"
	if err := rs.CreateMapping(ctx, routing.ModelMapping{ID: mappingID, ApplicationID: appID, GatewayModelName: shape.name, AppModelName: shape.name, Status: routing.ServerStatusActive, CreatedAt: offeringTime, UpdatedAt: offeringTime}); err != nil {
		t.Fatalf("CreateMapping %s: %v", shape.name, err)
	}
	if !shape.hasSpec {
		return
	}
	if err := rs.UpsertRuntimeSpec(ctx, routing.RuntimeSpec{
		ID: appID + "_spec", MappingID: mappingID, Binary: "/opt/llama/llama-server", Args: "[]", Env: "{}",
		APIFlavors: shape.specFlavors, ResponsesMode: routing.EndpointModeTranslate, MessagesMode: shape.specMessages,
	}); err != nil {
		t.Fatalf("UpsertRuntimeSpec %s: %v", mappingID, err)
	}
}

// offerServedShapes seeds every shape on a server of its own, because a server
// carries at most one server_agent application.
func offerServedShapes(t *testing.T, rs *routing.MemoryStore, prefix string, shapes []servedShape) {
	t.Helper()
	for i, shape := range shapes {
		srvID := fmt.Sprintf("srv_%s_%d", prefix, i)
		if err := rs.CreateAIServer(context.Background(), routing.AIServer{ID: srvID, Name: srvID, Domain: srvID + ".test", Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: offeringTime, UpdatedAt: offeringTime}); err != nil {
			t.Fatalf("CreateAIServer %s: %v", srvID, err)
		}
		seedServedShape(t, rs, srvID, fmt.Sprintf("app_%s_%d", prefix, i), shape)
	}
}

// servedRuleShapes is every spec shape x application flavors x messages mode
// the served rule tells apart, plus the spec-less server_agent and the
// ordinary application legs.
func servedRuleShapes() []servedShape {
	all := []string{flavorAnthropic, flavorOpenAI, flavorImages}
	text := []string{flavorAnthropic, flavorOpenAI}
	agent, plain := routing.ProviderServerAgent, routing.ProviderVLLM
	on, off := routing.EndpointModeTranslate, routing.EndpointModeDisabled
	return []servedShape{
		// The operator's sd child on a full application: images only.
		{name: "agent-images-spec", appType: agent, appFlavors: all, appMessages: on, hasSpec: true, specFlavors: []string{flavorImages}, specMessages: off, served: []string{flavorImages}, half: []string{flavorImages}},
		// A text child of a mixed application does not serve images.
		{name: "agent-openai-spec", appType: agent, appFlavors: all, appMessages: on, hasSpec: true, specFlavors: []string{flavorOpenAI}, specMessages: on, served: []string{flavorOpenAI}, half: []string{flavorOpenAI}},
		// The spec's messages mode, not the application's, gates anthropic.
		{name: "agent-spec-messages-off", appType: agent, appFlavors: all, appMessages: on, hasSpec: true, specFlavors: text, specMessages: off, served: []string{flavorOpenAI}, half: text},
		{name: "agent-spec-messages-on", appType: agent, appFlavors: text, appMessages: off, hasSpec: true, specFlavors: text, specMessages: on, served: text, half: text},
		// openai is the narrow rule: any spec that is not images-only passes it.
		{name: "agent-anthropic-spec", appType: agent, appFlavors: all, appMessages: on, hasSpec: true, specFlavors: []string{flavorAnthropic}, specMessages: on, served: text, half: text},
		// A spec row that lists no flavor still counts as the spec.
		{name: "agent-empty-spec", appType: agent, appFlavors: all, appMessages: on, hasSpec: true, specFlavors: []string{}, specMessages: on, served: []string{flavorOpenAI}, half: []string{flavorOpenAI}},
		// A spec flavor the application lacks has no effect.
		{name: "agent-spec-extra-anthropic", appType: agent, appFlavors: []string{flavorOpenAI, flavorImages}, appMessages: on, hasSpec: true, specFlavors: all, specMessages: on, served: []string{flavorOpenAI, flavorImages}, half: []string{flavorOpenAI, flavorImages}},
		// Empty served sets (see TestAnEmptyServedSetIsListedButNotCallable).
		{name: "agent-images-spec-on-text-app", appType: agent, appFlavors: []string{flavorOpenAI}, appMessages: on, hasSpec: true, specFlavors: []string{flavorImages}, specMessages: off, served: []string{}, half: []string{}},
		{name: "agent-anthropic-off-no-openai", appType: agent, appFlavors: []string{flavorAnthropic}, appMessages: on, hasSpec: true, specFlavors: []string{flavorAnthropic}, specMessages: off, served: []string{}, half: []string{flavorAnthropic}},
		// With openai in A, the same spec is listed [openai].
		{name: "agent-anthropic-off-with-openai", appType: agent, appFlavors: text, appMessages: on, hasSpec: true, specFlavors: []string{flavorAnthropic}, specMessages: off, served: []string{flavorOpenAI}, half: text},
		// No spec row: the application is the authority for flavors and mode.
		{name: "agent-no-spec", appType: agent, appFlavors: text, appMessages: on, served: text, half: text},
		{name: "agent-no-spec-messages-off", appType: agent, appFlavors: text, appMessages: off, served: []string{flavorOpenAI}, half: text},
		{name: "agent-no-spec-images-app", appType: agent, appFlavors: []string{flavorImages}, appMessages: off, served: []string{flavorImages}, half: []string{flavorImages}},
		// Ordinary applications.
		{name: "plain-all", appType: plain, appFlavors: all, appMessages: on, served: all, half: all},
		{name: "plain-messages-off", appType: plain, appFlavors: text, appMessages: off, served: []string{flavorOpenAI}, half: text},
		{name: "plain-anthropic-messages-off", appType: plain, appFlavors: []string{flavorAnthropic}, appMessages: off, served: []string{}, half: []string{flavorAnthropic}},
		{name: "plain-images", appType: plain, appFlavors: []string{flavorImages}, appMessages: off, served: []string{flavorImages}, half: []string{flavorImages}},
		// A spec row on an ordinary application is ignored.
		{name: "plain-with-spec-row", appType: plain, appFlavors: text, appMessages: on, hasSpec: true, specFlavors: []string{flavorImages}, specMessages: off, served: text, half: text},
	}
}

// assertListedFlavors checks one listing row's flavors: exactly want, and
// never nil, so an empty set reaches the client as [].
func assertListedFlavors(t *testing.T, surface string, byID map[string]ModelDTO, name string, want []string) {
	t.Helper()
	dto, ok := byID[name]
	if !ok {
		t.Errorf("%s: %s missing", surface, name)
		return
	}
	if dto.Flavors == nil || !reflect.DeepEqual(dto.Flavors, want) {
		t.Errorf("%s: %s flavors = %#v, want %#v", surface, name, dto.Flavors, want)
	}
}

// assertFlavorMembership checks that name is in got exactly when flavor is in
// want.
func assertFlavorMembership(t *testing.T, surface string, got map[string]struct{}, name, flavor string, want []string) {
	t.Helper()
	if _, in := got[name]; in != containsString(want, flavor) {
		t.Errorf("%s(%s)[%s] = %v, want %v", surface, flavor, name, in, !in)
	}
}

// namesSet turns a listing's names into a set.
func namesSet(names []string) map[string]struct{} {
	out := make(map[string]struct{}, len(names))
	for _, name := range names {
		out[name] = struct{}{}
	}
	return out
}

// TestListingsAndOfferingFollowTheServedRule: every listing surface and the
// redirect's Callable carry a flavor exactly when dispatch serves the model
// under it (routing.MappingServesAPIFlavor), and Existing holds a name exactly
// when it passes the flavor half (routing.MappingHasAPIFlavor). A name whose
// served set is empty is in no Callable, so callableModelNames excludes it.
func TestListingsAndOfferingFollowTheServedRule(t *testing.T) {
	ctx := context.Background()
	shapes := servedRuleShapes()
	rs := routing.NewMemoryStore()
	offerServedShapes(t, rs, "served", shapes)
	svc := offerSvc(rs, nil)
	token := auth.Token{UserID: "usr_1"}

	models := modelsByID(svc.Models(ctx, token))
	manage := modelsByID(svc.ManageModels(ctx, token))
	callableNames := svc.callableModelNames(ctx, token)
	listedBy := make(map[string]map[string]struct{})
	offerings := make(map[string]ModelOffering)
	for _, flavor := range knownAPIFlavors {
		listedBy[flavor] = namesSet(svc.ModelsForFlavor(ctx, token, flavor))
		offerings[flavor] = svc.ModelOfferingFor(ctx, token, flavor, nil)
	}
	for _, shape := range shapes {
		assertListedFlavors(t, "Models", models, shape.name, shape.served)
		assertListedFlavors(t, "ManageModels", manage, shape.name, shape.served)
		for _, flavor := range knownAPIFlavors {
			assertFlavorMembership(t, "ModelsForFlavor", listedBy[flavor], shape.name, flavor, shape.served)
			assertFlavorMembership(t, "Callable", offerings[flavor].Callable, shape.name, flavor, shape.served)
			assertFlavorMembership(t, "Existing", offerings[flavor].Existing, shape.name, flavor, shape.half)
		}
		if _, in := callableNames[shape.name]; in != (len(shape.served) > 0) {
			t.Errorf("callableModelNames[%s] = %v, want %v", shape.name, in, !in)
		}
	}
}

// emptyServedShapes are names no flavor serves: an sd child (spec
// [openai_images]) on an application without openai_images, an ordinary
// anthropic-only application with messages disabled, and an agent child whose
// application lacks openai and whose spec is anthropic with messages disabled.
func emptyServedShapes() []servedShape {
	anthropicOnly := []string{flavorAnthropic}
	return []servedShape{
		{name: "sd-on-text-app", appType: routing.ProviderServerAgent, appFlavors: []string{flavorAnthropic, flavorOpenAI}, appMessages: routing.EndpointModeTranslate, hasSpec: true, specFlavors: []string{flavorImages}, specMessages: routing.EndpointModeDisabled, half: []string{}},
		{name: "plain-anthropic-off", appType: routing.ProviderVLLM, appFlavors: anthropicOnly, appMessages: routing.EndpointModeDisabled, half: anthropicOnly},
		{name: "agent-anthropic-off", appType: routing.ProviderServerAgent, appFlavors: anthropicOnly, appMessages: routing.EndpointModeTranslate, hasSpec: true, specFlavors: anthropicOnly, specMessages: routing.EndpointModeDisabled, half: anthropicOnly},
	}
}

// TestAnEmptyServedSetIsListedButNotCallable: a name no flavor serves -- and a
// group whose offerable members are all such names, and an offered alias onto
// one -- keeps its row in Models() and ManageModels() with a non-nil empty
// flavor list, because validateServiceAllowedModels reads Models() and a
// hidden row would break every edit of a service whose allowlist names it. It
// leaves every Callable and every per-flavor listing, and Existing keeps it
// exactly under the flavors whose flavor half it passes: a messages-disabled
// anthropic model still exists under anthropic.
func TestAnEmptyServedSetIsListedButNotCallable(t *testing.T) {
	ctx := context.Background()
	shapes := emptyServedShapes()
	rs := routing.NewMemoryStore()
	offerServedShapes(t, rs, "empty", shapes)
	offerGroup(t, rs, "grp_empty", "empty-group", "sd-on-text-app", "plain-anthropic-off")
	token := tokenWithRules(map[string]store.ModelOverrideRule{"alias-empty": {To: "sd-on-text-app", Offer: true}})
	svc := offerSvc(rs, nil)

	names := []string{"sd-on-text-app", "plain-anthropic-off", "agent-anthropic-off", "empty-group"}
	listedNames := append(append([]string{}, names...), "alias-empty")
	half := map[string][]string{
		"sd-on-text-app":      {},
		"plain-anthropic-off": {flavorAnthropic},
		"agent-anthropic-off": {flavorAnthropic},
		// The group overlay of Existing folds its members' flavor halves.
		"empty-group": {flavorAnthropic},
	}
	models := modelsByID(svc.Models(ctx, token))
	manage := modelsByID(svc.ManageModels(ctx, token))
	for _, name := range names {
		assertListedFlavors(t, "Models", models, name, []string{})
		assertListedFlavors(t, "ManageModels", manage, name, []string{})
	}
	assertListedFlavors(t, "Models", models, "alias-empty", []string{})
	for _, flavor := range knownAPIFlavors {
		listed := namesSet(svc.ModelsForFlavor(ctx, token, flavor))
		off := svc.ModelOfferingFor(ctx, token, flavor, nil)
		for _, name := range listedNames {
			assertFlavorMembership(t, "ModelsForFlavor", listed, name, flavor, nil)
			assertFlavorMembership(t, "Callable", off.Callable, name, flavor, nil)
		}
		for _, name := range names {
			assertFlavorMembership(t, "Existing", off.Existing, name, flavor, half[name])
		}
	}
	callable := svc.callableModelNames(ctx, token)
	for _, name := range names {
		if _, ok := callable[name]; ok {
			t.Errorf("callableModelNames contains %s, want it excluded: no flavor serves it", name)
		}
	}
}

// TestAnEmptyServedSetIsRefusedAsATokenTargetAndAcceptedInAnAllowlist: every
// model-valued token setting is checked against callableModelNames, so a name
// no flavor serves is refused as the catch-all override, as a rule target and
// as the redirect's fallback, with the one override error. A service's model
// allowlist is checked against Models() instead, where the name keeps its row,
// so an allowlist that names it is accepted.
func TestAnEmptyServedSetIsRefusedAsATokenTargetAndAcceptedInAnAllowlist(t *testing.T) {
	ctx := context.Background()
	svc, _, _, rs := newServiceAccountsTestService(t, offeringTime)
	offerServedShapes(t, rs, "empty_tok", emptyServedShapes())
	owner := svcFullToken()

	if _, err := svc.CreateToken(ctx, owner, CreateTokenRequest{Name: "control", Scopes: []string{"gateway:use"}, ModelOverride: "model-a"}); err != nil {
		t.Fatalf("control: an override onto a served model = %v, want accepted", err)
	}
	cases := map[string]CreateTokenRequest{
		"catch-all override": {Name: "override", Scopes: []string{"gateway:use"}, ModelOverride: "sd-on-text-app"},
		"rule target":        {Name: "rule", Scopes: []string{"gateway:use"}, ModelOverrideMap: map[string]store.ModelOverrideRule{"gpt-4o": {To: "sd-on-text-app"}}},
		"redirect fallback":  {Name: "fallback", Scopes: []string{"gateway:use"}, UnknownModelRedirect: true, UnknownModelFallback: "plain-anthropic-off"},
	}
	for what, req := range cases {
		if _, err := svc.CreateToken(ctx, owner, req); !errors.Is(err, ErrTokenModelOverrideInvalid) {
			t.Errorf("%s naming an empty-set model: err = %v, want ErrTokenModelOverrideInvalid (portal.token_model_override_invalid)", what, err)
		}
	}

	allowed := []string{"agent-anthropic-off", "plain-anthropic-off", "sd-on-text-app"}
	dto, err := svc.CreateService(ctx, svcAdminToken(), CreateServiceRequest{
		Name: "Allowlist Bot", AllowedModels: allowed, AdminGroupIDs: []string{testServiceAdminGroupID},
	})
	if err != nil {
		t.Fatalf("CreateService with an allowlist naming empty-set models: %v, want accepted", err)
	}
	if !reflect.DeepEqual(dto.AllowedModels, allowed) {
		t.Fatalf("AllowedModels = %#v, want %#v", dto.AllowedModels, allowed)
	}
}

// TestExistingReadsTheSpecsOfModelsTheTokenCannotSee: Existing is built over
// the UNFILTERED views, so its flavor half must read the runtime spec of a
// child the token cannot see as well. Read over the visible views only, the
// hidden sd child would fall back to its application's flavors and exist
// under openai -- and the redirect would leave a text request for it alone
// instead of treating it like any other name that does not exist there.
func TestExistingReadsTheSpecsOfModelsTheTokenCannotSee(t *testing.T) {
	e := newGroupTestEnv(t)
	f := setupVisibilityFixture(e)
	seedServedShape(t, e.routes, f.srvX.ID, "app_x_agent", servedShape{
		name: "sd-child", appType: routing.ProviderServerAgent,
		appFlavors: []string{flavorAnthropic, flavorOpenAI, flavorImages}, appMessages: routing.EndpointModeTranslate,
		hasSpec: true, specFlavors: []string{flavorImages}, specMessages: routing.EndpointModeDisabled,
	})
	blind := token("usr_v") // not provisioned into the resource group of server X
	seeing := token("usr_u")

	for _, flavor := range knownAPIFlavors {
		off := e.svc.ModelOfferingFor(e.ctx, blind, flavor, nil)
		if _, ok := off.Callable["sd-child"]; ok {
			t.Errorf("Callable(%s) for usr_v contains sd-child, want it filtered out", flavor)
		}
		assertFlavorMembership(t, "Existing", off.Existing, "sd-child", flavor, []string{flavorImages})
	}
	if _, ok := e.svc.ModelOfferingFor(e.ctx, seeing, flavorImages, nil).Callable["sd-child"]; !ok {
		t.Error("Callable(openai_images) for usr_u lacks sd-child, want it: usr_u is provisioned")
	}
}

// TestAGroupOfAMessagesDisabledModelExistsButIsNotCallable: Existing's group
// overlay folds the members' flavor halves, Callable's their served sets. So
// a group whose only member has its messages endpoint disabled still exists
// under anthropic -- the redirect's narrow default leaves a request for it
// alone, as for the member itself -- but is not callable there.
func TestAGroupOfAMessagesDisabledModelExistsButIsNotCallable(t *testing.T) {
	ctx := context.Background()
	text := []string{flavorAnthropic, flavorOpenAI}
	rs := routing.NewMemoryStore()
	offerServedShapes(t, rs, "md", []servedShape{
		{name: "md-plain", appType: routing.ProviderVLLM, appFlavors: text, appMessages: routing.EndpointModeDisabled},
		{name: "md-agent", appType: routing.ProviderServerAgent, appFlavors: text, appMessages: routing.EndpointModeTranslate, hasSpec: true, specFlavors: text, specMessages: routing.EndpointModeDisabled},
	})
	offerGroup(t, rs, "grp_md_plain", "md-plain-group", "md-plain")
	offerGroup(t, rs, "grp_md_agent", "md-agent-group", "md-agent")
	svc := offerSvc(rs, nil)
	token := auth.Token{UserID: "usr_1"}

	anthropic := svc.ModelOfferingFor(ctx, token, flavorAnthropic, nil)
	openai := svc.ModelOfferingFor(ctx, token, flavorOpenAI, nil)
	for _, name := range []string{"md-plain", "md-agent", "md-plain-group", "md-agent-group"} {
		if _, ok := anthropic.Existing[name]; !ok {
			t.Errorf("Existing(anthropic) lacks %s, want it: its flavor half passes", name)
		}
		if _, ok := anthropic.Callable[name]; ok {
			t.Errorf("Callable(anthropic) contains %s, want it absent: its messages endpoint is disabled", name)
		}
		if _, ok := openai.Callable[name]; !ok {
			t.Errorf("Callable(openai) lacks %s, want it: chat completions has no mode", name)
		}
	}
}

// TestModelsForFlavorReadsEachAgentApplicationsSpecsOnce: the per-flavor
// listing reads the runtime specs of the views it lists, once per server_agent
// application and never for an ordinary one.
func TestModelsForFlavorReadsEachAgentApplicationsSpecsOnce(t *testing.T) {
	store := &specReadStore{MemoryStore: capableOfferingStore(t), calls: map[string]int{}}
	got := offerSvc(store, nil).ModelsForFlavor(context.Background(), auth.Token{UserID: "usr_1"}, flavorOpenAI)
	want := map[string]int{"app_ab": 1, "app_an": 1, "app_ax": 1}
	if !reflect.DeepEqual(store.calls, want) {
		t.Fatalf("RuntimeSpecsByApplication calls = %#v, want %#v", store.calls, want)
	}
	if containsString(got, "agent-both") {
		t.Fatalf("ModelsForFlavor(openai) = %#v, want no agent-both: its spec is images-only", got)
	}
}
