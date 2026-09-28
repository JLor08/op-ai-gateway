// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package routing

import (
	"slices"
	"testing"
)

// TestFlavorsAreImagesOnly pins the predicate's rule on its own: images-only
// means openai_images listed and neither text flavor; an empty list is not.
func TestFlavorsAreImagesOnly(t *testing.T) {
	cases := []struct {
		flavors []string
		want    bool
	}{
		{nil, false},
		{[]string{}, false},
		{[]string{APIFlavorOpenAIImages}, true},
		{[]string{APIFlavorOpenAIImages, "custom"}, true},
		{[]string{APIFlavorOpenAI}, false},
		{[]string{APIFlavorAnthropic}, false},
		{[]string{APIFlavorOpenAI, APIFlavorOpenAIImages}, false},
		{[]string{APIFlavorOpenAIImages, APIFlavorAnthropic}, false},
	}
	for _, tc := range cases {
		if got := FlavorsAreImagesOnly(tc.flavors); got != tc.want {
			t.Errorf("FlavorsAreImagesOnly(%q) = %v, want %v", tc.flavors, got, tc.want)
		}
	}
}

// effectiveFieldsApp and effectiveFieldsSpec differ in all four fields, so a
// result that takes any one field from the wrong row fails its case.
func effectiveFieldsApp(appType string) Application {
	return Application{
		Type:                        appType,
		APIFlavors:                  []string{APIFlavorOpenAI, APIFlavorAnthropic},
		ResponsesMode:               EndpointModeTranslate,
		MessagesMode:                EndpointModePassthrough,
		ResponsesLiveTimingsEnabled: true,
	}
}

func effectiveFieldsSpec(flavors []string, enabled bool) RuntimeSpec {
	return RuntimeSpec{
		Enabled:                     enabled,
		APIFlavors:                  flavors,
		ResponsesMode:               EndpointModePassthrough,
		MessagesMode:                EndpointModeDisabled,
		ResponsesLiveTimingsEnabled: false,
	}
}

// TestEffectiveFields pins the spec-over-application precedence: a
// server_agent application with a spec row takes all four fields from the
// spec, whatever its Enabled flag and even when its flavors are stored as
// nil; every other combination takes all four from the application.
func TestEffectiveFields(t *testing.T) {
	fromApp := EffectiveSpecFields{
		APIFlavors:                  []string{APIFlavorOpenAI, APIFlavorAnthropic},
		ResponsesMode:               EndpointModeTranslate,
		MessagesMode:                EndpointModePassthrough,
		ResponsesLiveTimingsEnabled: true,
	}
	fromSpec := func(flavors []string) EffectiveSpecFields {
		return EffectiveSpecFields{
			APIFlavors:                  flavors,
			ResponsesMode:               EndpointModePassthrough,
			MessagesMode:                EndpointModeDisabled,
			ResponsesLiveTimingsEnabled: false,
		}
	}
	imagesOnly := []string{APIFlavorOpenAIImages}
	cases := []struct {
		name    string
		app     Application
		spec    RuntimeSpec
		hasSpec bool
		want    EffectiveSpecFields
	}{
		{
			name:    "server_agent with an enabled spec takes the spec",
			app:     effectiveFieldsApp(ProviderServerAgent),
			spec:    effectiveFieldsSpec(imagesOnly, true),
			hasSpec: true,
			want:    fromSpec(imagesOnly),
		},
		{
			name:    "server_agent with a disabled spec still takes the spec",
			app:     effectiveFieldsApp(ProviderServerAgent),
			spec:    effectiveFieldsSpec(imagesOnly, false),
			hasSpec: true,
			want:    fromSpec(imagesOnly),
		},
		{
			name:    "server_agent with a spec stored as nil flavors takes the empty list",
			app:     effectiveFieldsApp(ProviderServerAgent),
			spec:    effectiveFieldsSpec(nil, true),
			hasSpec: true,
			want:    fromSpec(nil),
		},
		{
			name:    "server_agent without a spec row takes the application",
			app:     effectiveFieldsApp(ProviderServerAgent),
			spec:    effectiveFieldsSpec(imagesOnly, true),
			hasSpec: false,
			want:    fromApp,
		},
		{
			name:    "ordinary application ignores a spec it is handed",
			app:     effectiveFieldsApp(ProviderLlamaCPP),
			spec:    effectiveFieldsSpec(imagesOnly, true),
			hasSpec: true,
			want:    fromApp,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := EffectiveFields(tc.app, tc.spec, tc.hasSpec)
			if !slices.Equal(got.APIFlavors, tc.want.APIFlavors) ||
				got.ResponsesMode != tc.want.ResponsesMode ||
				got.MessagesMode != tc.want.MessagesMode ||
				got.ResponsesLiveTimingsEnabled != tc.want.ResponsesLiveTimingsEnabled {
				t.Fatalf("EffectiveFields = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// knownFlavorsForRules is the coarse flavor set both mapping rules answer for.
var knownFlavorsForRules = []string{APIFlavorOpenAI, APIFlavorAnthropic, APIFlavorOpenAIImages}

// flavorsWhere lists, in knownFlavorsForRules order, the coarse flavors rule
// accepts for the mapping.
func flavorsWhere(rule func(Application, EffectiveSpecFields, string) bool, app Application, eff EffectiveSpecFields) []string {
	out := []string{}
	for _, f := range knownFlavorsForRules {
		if rule(app, eff, f) {
			out = append(out, f)
		}
	}
	return out
}

// flavorRuleCase is one row of the mapping-rule truth table: an application
// (its type, flavors A and messages mode), an optional spec (flavors S and
// messages mode), and the flavors each rule accepts.
type flavorRuleCase struct {
	name         string
	appType      string
	appFlavors   []string
	appMessages  EndpointMode
	hasSpec      bool
	specFlavors  []string
	specMessages EndpointMode
	wantHas      []string
	wantServed   []string
}

// flavorRuleCases is the truth table over A x E x the effective messages mode
// M. Candidacy admits on A, dispatch refuses on E, and /v1/messages also reads
// M, so a row is listed under a flavor exactly when both halves let a request
// through.
var flavorRuleCases = []flavorRuleCase{
	{
		name:    "operator's case: images-only spec on an application without openai_images",
		appType: ProviderServerAgent, appFlavors: []string{APIFlavorOpenAI}, appMessages: EndpointModeTranslate,
		hasSpec: true, specFlavors: []string{APIFlavorOpenAIImages}, specMessages: EndpointModeTranslate,
		wantHas: []string{}, wantServed: []string{},
	},
	{
		name:    "documented sd child: images-only spec on a text-and-images application",
		appType: ProviderServerAgent, appFlavors: []string{APIFlavorOpenAI, APIFlavorOpenAIImages}, appMessages: EndpointModeTranslate,
		hasSpec: true, specFlavors: []string{APIFlavorOpenAIImages}, specMessages: EndpointModeDisabled,
		wantHas: []string{APIFlavorOpenAIImages}, wantServed: []string{APIFlavorOpenAIImages},
	},
	{
		name:    "text child of a mixed application",
		appType: ProviderServerAgent, appFlavors: []string{APIFlavorOpenAI, APIFlavorAnthropic, APIFlavorOpenAIImages}, appMessages: EndpointModeTranslate,
		hasSpec: true, specFlavors: []string{APIFlavorOpenAI}, specMessages: EndpointModeTranslate,
		wantHas: []string{APIFlavorOpenAI}, wantServed: []string{APIFlavorOpenAI},
	},
	{
		name:    "text-and-anthropic child of a mixed application, messages passthrough",
		appType: ProviderServerAgent, appFlavors: []string{APIFlavorOpenAI, APIFlavorAnthropic, APIFlavorOpenAIImages}, appMessages: EndpointModeTranslate,
		hasSpec: true, specFlavors: []string{APIFlavorOpenAI, APIFlavorAnthropic}, specMessages: EndpointModePassthrough,
		wantHas: []string{APIFlavorOpenAI, APIFlavorAnthropic}, wantServed: []string{APIFlavorOpenAI, APIFlavorAnthropic},
	},
	{
		name:    "text-and-anthropic child, spec messages disabled",
		appType: ProviderServerAgent, appFlavors: []string{APIFlavorOpenAI, APIFlavorAnthropic, APIFlavorOpenAIImages}, appMessages: EndpointModeTranslate,
		hasSpec: true, specFlavors: []string{APIFlavorOpenAI, APIFlavorAnthropic}, specMessages: EndpointModeDisabled,
		wantHas: []string{APIFlavorOpenAI, APIFlavorAnthropic}, wantServed: []string{APIFlavorOpenAI},
	},
	{
		name:    "spec narrowed to anthropic keeps openai",
		appType: ProviderServerAgent, appFlavors: []string{APIFlavorOpenAI, APIFlavorAnthropic}, appMessages: EndpointModeTranslate,
		hasSpec: true, specFlavors: []string{APIFlavorAnthropic}, specMessages: EndpointModeTranslate,
		wantHas: []string{APIFlavorOpenAI, APIFlavorAnthropic}, wantServed: []string{APIFlavorOpenAI, APIFlavorAnthropic},
	},
	{
		name:    "anthropic-only child with messages disabled on an application without openai",
		appType: ProviderServerAgent, appFlavors: []string{APIFlavorAnthropic}, appMessages: EndpointModeTranslate,
		hasSpec: true, specFlavors: []string{APIFlavorAnthropic}, specMessages: EndpointModeDisabled,
		wantHas: []string{APIFlavorAnthropic}, wantServed: []string{},
	},
	{
		name:    "spec stored as an empty list keeps openai",
		appType: ProviderServerAgent, appFlavors: []string{APIFlavorOpenAI, APIFlavorAnthropic}, appMessages: EndpointModeTranslate,
		hasSpec: true, specFlavors: []string{}, specMessages: EndpointModeTranslate,
		wantHas: []string{APIFlavorOpenAI}, wantServed: []string{APIFlavorOpenAI},
	},
	{
		name:    "spec stored as nil flavors keeps openai",
		appType: ProviderServerAgent, appFlavors: []string{APIFlavorOpenAI, APIFlavorAnthropic}, appMessages: EndpointModeTranslate,
		hasSpec: true, specFlavors: nil, specMessages: EndpointModeTranslate,
		wantHas: []string{APIFlavorOpenAI}, wantServed: []string{APIFlavorOpenAI},
	},
	{
		name:    "spec flavors the application lacks have no effect",
		appType: ProviderServerAgent, appFlavors: []string{APIFlavorOpenAIImages}, appMessages: EndpointModeTranslate,
		hasSpec: true, specFlavors: []string{APIFlavorOpenAI, APIFlavorAnthropic, APIFlavorOpenAIImages}, specMessages: EndpointModePassthrough,
		wantHas: []string{APIFlavorOpenAIImages}, wantServed: []string{APIFlavorOpenAIImages},
	},
	{
		name:    "a spec anthropic the application lacks has no effect",
		appType: ProviderServerAgent, appFlavors: []string{APIFlavorOpenAI, APIFlavorOpenAIImages}, appMessages: EndpointModeTranslate,
		hasSpec: true, specFlavors: []string{APIFlavorOpenAIImages, APIFlavorAnthropic}, specMessages: EndpointModeTranslate,
		wantHas: []string{APIFlavorOpenAI, APIFlavorOpenAIImages}, wantServed: []string{APIFlavorOpenAI, APIFlavorOpenAIImages},
	},
	{
		name:    "application messages disabled does not gate a spec that serves messages",
		appType: ProviderServerAgent, appFlavors: []string{APIFlavorOpenAI, APIFlavorAnthropic}, appMessages: EndpointModeDisabled,
		hasSpec: true, specFlavors: []string{APIFlavorAnthropic}, specMessages: EndpointModePassthrough,
		wantHas: []string{APIFlavorOpenAI, APIFlavorAnthropic}, wantServed: []string{APIFlavorOpenAI, APIFlavorAnthropic},
	},
	{
		name:    "spec-less agent mapping with application messages disabled",
		appType: ProviderServerAgent, appFlavors: []string{APIFlavorOpenAI, APIFlavorAnthropic}, appMessages: EndpointModeDisabled,
		wantHas: []string{APIFlavorOpenAI, APIFlavorAnthropic}, wantServed: []string{APIFlavorOpenAI},
	},
	{
		name:    "spec-less agent mapping serves the application's flavors",
		appType: ProviderServerAgent, appFlavors: []string{APIFlavorOpenAI, APIFlavorAnthropic}, appMessages: EndpointModeTranslate,
		wantHas: []string{APIFlavorOpenAI, APIFlavorAnthropic}, wantServed: []string{APIFlavorOpenAI, APIFlavorAnthropic},
	},
	{
		name:    "spec-less agent mapping on an images-only application",
		appType: ProviderServerAgent, appFlavors: []string{APIFlavorOpenAIImages}, appMessages: EndpointModeTranslate,
		wantHas: []string{APIFlavorOpenAIImages}, wantServed: []string{APIFlavorOpenAIImages},
	},
	{
		name:    "ordinary application ignores an images-only spec",
		appType: ProviderLlamaCPP, appFlavors: []string{APIFlavorOpenAI, APIFlavorAnthropic, APIFlavorOpenAIImages}, appMessages: EndpointModePassthrough,
		hasSpec: true, specFlavors: []string{APIFlavorOpenAIImages}, specMessages: EndpointModeDisabled,
		wantHas:    []string{APIFlavorOpenAI, APIFlavorAnthropic, APIFlavorOpenAIImages},
		wantServed: []string{APIFlavorOpenAI, APIFlavorAnthropic, APIFlavorOpenAIImages},
	},
	{
		name:    "ordinary application with messages disabled",
		appType: ProviderLlamaCPP, appFlavors: []string{APIFlavorOpenAI, APIFlavorAnthropic}, appMessages: EndpointModeDisabled,
		wantHas: []string{APIFlavorOpenAI, APIFlavorAnthropic}, wantServed: []string{APIFlavorOpenAI},
	},
	{
		name:    "ordinary anthropic-only application with messages disabled",
		appType: ProviderLlamaCPP, appFlavors: []string{APIFlavorAnthropic}, appMessages: EndpointModeDisabled,
		wantHas: []string{APIFlavorAnthropic}, wantServed: []string{},
	},
	{
		name:    "ordinary images-only application",
		appType: ProviderLlamaCPP, appFlavors: []string{APIFlavorOpenAIImages}, appMessages: EndpointModeTranslate,
		wantHas: []string{APIFlavorOpenAIImages}, wantServed: []string{APIFlavorOpenAIImages},
	},
	{
		name:    "ordinary application without flavors",
		appType: ProviderLlamaCPP, appFlavors: []string{}, appMessages: EndpointModeTranslate,
		wantHas: []string{}, wantServed: []string{},
	},
}

// TestMappingFlavorRules runs the truth table through both rules, with the
// effective fields resolved by EffectiveFields as every caller resolves them.
func TestMappingFlavorRules(t *testing.T) {
	for _, tc := range flavorRuleCases {
		t.Run(tc.name, func(t *testing.T) {
			app := Application{Type: tc.appType, APIFlavors: tc.appFlavors, MessagesMode: tc.appMessages}
			spec := RuntimeSpec{APIFlavors: tc.specFlavors, MessagesMode: tc.specMessages}
			eff := EffectiveFields(app, spec, tc.hasSpec)
			if got := flavorsWhere(MappingHasAPIFlavor, app, eff); !slices.Equal(got, tc.wantHas) {
				t.Errorf("MappingHasAPIFlavor accepts %q, want %q", got, tc.wantHas)
			}
			if got := flavorsWhere(MappingServesAPIFlavor, app, eff); !slices.Equal(got, tc.wantServed) {
				t.Errorf("MappingServesAPIFlavor accepts %q, want %q", got, tc.wantServed)
			}
		})
	}
}

// TestMappingFlavorRulesRejectOtherFlavors pins that both rules answer only
// the three coarse flavors: a custom flavor and the empty string, which the
// application and the spec both list, a fine flavor, and a differently cased
// one are all false.
func TestMappingFlavorRulesRejectOtherFlavors(t *testing.T) {
	all := []string{APIFlavorOpenAI, APIFlavorAnthropic, APIFlavorOpenAIImages, "custom", ""}
	for _, appType := range []string{ProviderServerAgent, ProviderLlamaCPP} {
		app := Application{Type: appType, APIFlavors: all, MessagesMode: EndpointModePassthrough}
		eff := EffectiveFields(app, RuntimeSpec{APIFlavors: all, MessagesMode: EndpointModePassthrough}, true)
		for _, flavor := range []string{"custom", "openai_chat_completions", "openai_responses", "anthropic_messages", "OpenAI", ""} {
			if MappingHasAPIFlavor(app, eff, flavor) {
				t.Errorf("%s: MappingHasAPIFlavor(%q) = true, want false", appType, flavor)
			}
			if MappingServesAPIFlavor(app, eff, flavor) {
				t.Errorf("%s: MappingServesAPIFlavor(%q) = true, want false", appType, flavor)
			}
		}
	}
}

// flavorSubsets is every subset of the three coarse flavors, in
// knownFlavorsForRules order, the empty one included.
func flavorSubsets() [][]string {
	subsets := make([][]string, 0, 1<<len(knownFlavorsForRules))
	for mask := range 1 << len(knownFlavorsForRules) {
		subset := []string{}
		for i, f := range knownFlavorsForRules {
			if mask&(1<<i) != 0 {
				subset = append(subset, f)
			}
		}
		subsets = append(subsets, subset)
	}
	return subsets
}

var messagesModesForRules = []EndpointMode{"", EndpointModeTranslate, EndpointModePassthrough, EndpointModeDisabled}

// TestMappingServesAPIFlavorOrdinaryApplicationIsItsFlavors pins the
// property that makes the rule a no-op for an ordinary application: it is
// served under exactly its own flavors, minus anthropic when its messages
// endpoint is disabled, and its flavor half is exactly its own flavors in every
// mode. A spec handed in beside it changes nothing, and neither does a
// disabled responses endpoint: openai stands for chat completions.
func TestMappingServesAPIFlavorOrdinaryApplicationIsItsFlavors(t *testing.T) {
	narrowing := RuntimeSpec{APIFlavors: []string{APIFlavorOpenAIImages}, MessagesMode: EndpointModeDisabled}
	for _, flavors := range flavorSubsets() {
		for _, mode := range messagesModesForRules {
			app := Application{
				Type:          ProviderLlamaCPP,
				APIFlavors:    append(slices.Clone(flavors), "custom"),
				ResponsesMode: EndpointModeDisabled,
				MessagesMode:  mode,
			}
			eff := EffectiveFields(app, narrowing, true)
			wantServed := flavors
			if mode == EndpointModeDisabled {
				wantServed = slices.DeleteFunc(slices.Clone(flavors), func(f string) bool { return f == APIFlavorAnthropic })
			}
			if got := flavorsWhere(MappingHasAPIFlavor, app, eff); !slices.Equal(got, flavors) {
				t.Errorf("A=%q M=%q: MappingHasAPIFlavor accepts %q, want %q", app.APIFlavors, mode, got, flavors)
			}
			if got := flavorsWhere(MappingServesAPIFlavor, app, eff); !slices.Equal(got, wantServed) {
				t.Errorf("A=%q M=%q: MappingServesAPIFlavor accepts %q, want %q", app.APIFlavors, mode, got, wantServed)
			}
		}
	}
}

// mappingRuleShape is one mapping the property test runs both rules on.
type mappingRuleShape struct {
	app     Application
	spec    RuntimeSpec
	hasSpec bool
}

// mappingRuleShapes is every combination of application type, A, the
// application's messages mode, and either no spec or a spec with any S (nil
// included) and any messages mode.
func mappingRuleShapes() []mappingRuleShape {
	shapes := []mappingRuleShape{}
	for _, appType := range []string{ProviderServerAgent, ProviderLlamaCPP} {
		for _, app := range applicationsOfType(appType) {
			shapes = append(shapes, shapesForApplication(app)...)
		}
	}
	return shapes
}

func applicationsOfType(appType string) []Application {
	apps := []Application{}
	for _, flavors := range flavorSubsets() {
		for _, mode := range messagesModesForRules {
			apps = append(apps, Application{Type: appType, APIFlavors: flavors, MessagesMode: mode})
		}
	}
	return apps
}

func shapesForApplication(app Application) []mappingRuleShape {
	shapes := []mappingRuleShape{{app: app}}
	for _, flavors := range append(flavorSubsets(), nil) {
		for _, mode := range messagesModesForRules {
			spec := RuntimeSpec{APIFlavors: flavors, MessagesMode: mode}
			shapes = append(shapes, mappingRuleShape{app: app, spec: spec, hasSpec: true})
		}
	}
	return shapes
}

// TestMappingServedImpliesFlavorHalf pins that the served rule only ever
// narrows the flavor half, over every shape mappingRuleShapes lists.
func TestMappingServedImpliesFlavorHalf(t *testing.T) {
	for _, s := range mappingRuleShapes() {
		eff := EffectiveFields(s.app, s.spec, s.hasSpec)
		for _, f := range knownFlavorsForRules {
			if MappingServesAPIFlavor(s.app, eff, f) && !MappingHasAPIFlavor(s.app, eff, f) {
				t.Errorf("%s A=%q M=%q, spec %v S=%q M=%q: served %q without its flavor half",
					s.app.Type, s.app.APIFlavors, s.app.MessagesMode, s.hasSpec, s.spec.APIFlavors, s.spec.MessagesMode, f)
			}
		}
	}
}
