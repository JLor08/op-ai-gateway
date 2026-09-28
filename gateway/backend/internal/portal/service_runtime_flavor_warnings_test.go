// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"op-ai-gateway/internal/routing"
	"slices"
	"testing"
	"time"
)

// flavorWarningAgentApp is a server_agent application declaring flavors, for
// the pure helper tables below.
func flavorWarningAgentApp(flavors ...string) routing.Application {
	return routing.Application{Type: routing.ProviderServerAgent, APIFlavors: flavors}
}

// TestAnySpecListsFlavorNotOnApplication is the truth table behind
// api_flavors_not_on_application: a spec flavor the server_agent application
// does not declare warns, whatever the spec's Enabled flag, and a spec of any
// other application type never does.
func TestAnySpecListsFlavorNotOnApplication(t *testing.T) {
	cases := []struct {
		name  string
		app   routing.Application
		specs []routing.RuntimeSpec
		want  bool
	}{
		{
			name:  "parent lacks the spec's flavor",
			app:   flavorWarningAgentApp(routing.APIFlavorOpenAI),
			specs: []routing.RuntimeSpec{{Enabled: true, APIFlavors: []string{routing.APIFlavorOpenAIImages}}},
			want:  true,
		},
		{
			name:  "parent declares every flavor of the spec",
			app:   flavorWarningAgentApp(routing.APIFlavorOpenAI, routing.APIFlavorOpenAIImages),
			specs: []routing.RuntimeSpec{{Enabled: true, APIFlavors: []string{routing.APIFlavorOpenAIImages}}},
			want:  false,
		},
		{
			name: "one spec of several lists a flavor the parent lacks",
			app:  flavorWarningAgentApp(routing.APIFlavorOpenAI, routing.APIFlavorAnthropic),
			specs: []routing.RuntimeSpec{
				{Enabled: true, APIFlavors: []string{routing.APIFlavorOpenAI}},
				{Enabled: true, APIFlavors: []string{routing.APIFlavorOpenAI, routing.APIFlavorOpenAIImages}},
			},
			want: true,
		},
		{
			name:  "a disabled spec counts",
			app:   flavorWarningAgentApp(routing.APIFlavorOpenAI),
			specs: []routing.RuntimeSpec{{Enabled: false, APIFlavors: []string{routing.APIFlavorAnthropic}}},
			want:  true,
		},
		{
			name:  "a non-agent application's specs are never read",
			app:   routing.Application{Type: routing.ProviderVLLM, APIFlavors: []string{routing.APIFlavorOpenAI}},
			specs: []routing.RuntimeSpec{{Enabled: true, APIFlavors: []string{routing.APIFlavorOpenAIImages}}},
			want:  false,
		},
		{
			name:  "a spec stored as [] lists no flavor",
			app:   flavorWarningAgentApp(routing.APIFlavorOpenAI),
			specs: []routing.RuntimeSpec{{Enabled: true, APIFlavors: []string{}}},
			want:  false,
		},
		{
			name:  "a spec with nil flavors lists no flavor",
			app:   flavorWarningAgentApp(routing.APIFlavorOpenAI),
			specs: []routing.RuntimeSpec{{Enabled: true}},
			want:  false,
		},
		{
			name:  "no specs",
			app:   flavorWarningAgentApp(routing.APIFlavorOpenAI),
			specs: nil,
			want:  false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := anySpecListsFlavorNotOnApplication(tc.app, tc.specs); got != tc.want {
				t.Fatalf("anySpecListsFlavorNotOnApplication(%+v, %+v) = %v, want %v", tc.app, tc.specs, got, tc.want)
			}
		})
	}
}

// TestAnyStableDiffusionSpecNotImagesOnly is the truth table behind
// api_flavors_text_on_stable_diffusion: a spec whose EFFECTIVE type is
// stable_diffusion_cpp (explicit, or Auto with an sd-server binary) warns
// unless its flavors are images-only. An empty list is never images-only, so
// an sd spec stored as [] warns too.
func TestAnyStableDiffusionSpecNotImagesOnly(t *testing.T) {
	const sdBinary = "/opt/sd/sd-server"
	sdType := string(routing.RuntimeSpecTypeStableDiffusionCpp)
	agent := flavorWarningAgentApp(routing.APIFlavorOpenAI, routing.APIFlavorAnthropic, routing.APIFlavorOpenAIImages)
	cases := []struct {
		name  string
		app   routing.Application
		specs []routing.RuntimeSpec
		want  bool
	}{
		{
			name:  "auto type, sd-server binary, a text flavor",
			app:   agent,
			specs: []routing.RuntimeSpec{{Enabled: true, Binary: sdBinary, APIFlavors: []string{routing.APIFlavorOpenAI}}},
			want:  true,
		},
		{
			name:  "auto type, sd-server binary, images only",
			app:   agent,
			specs: []routing.RuntimeSpec{{Enabled: true, Binary: sdBinary, APIFlavors: []string{routing.APIFlavorOpenAIImages}}},
			want:  false,
		},
		{
			name:  "explicit sd type on a binary that detects as custom",
			app:   agent,
			specs: []routing.RuntimeSpec{{Enabled: true, Type: sdType, Binary: "/opt/bin/image-daemon", APIFlavors: []string{routing.APIFlavorOpenAIImages, routing.APIFlavorAnthropic}}},
			want:  true,
		},
		{
			name:  "explicit llama.cpp type overrides an sd-server binary",
			app:   agent,
			specs: []routing.RuntimeSpec{{Enabled: true, Type: string(routing.RuntimeSpecTypeLlamaCpp), Binary: sdBinary, APIFlavors: []string{routing.APIFlavorOpenAI}}},
			want:  false,
		},
		{
			name:  "auto type, llama-server binary, a text flavor",
			app:   agent,
			specs: []routing.RuntimeSpec{{Enabled: true, Binary: "/opt/llama/llama-server", APIFlavors: []string{routing.APIFlavorOpenAI}}},
			want:  false,
		},
		{
			name:  "sd spec stored as []",
			app:   agent,
			specs: []routing.RuntimeSpec{{Enabled: true, Type: sdType, Binary: sdBinary, APIFlavors: []string{}}},
			want:  true,
		},
		{
			name:  "sd spec with nil flavors",
			app:   agent,
			specs: []routing.RuntimeSpec{{Enabled: true, Type: sdType, Binary: sdBinary}},
			want:  true,
		},
		{
			name:  "a disabled spec counts",
			app:   agent,
			specs: []routing.RuntimeSpec{{Enabled: false, Type: sdType, Binary: sdBinary, APIFlavors: []string{routing.APIFlavorAnthropic}}},
			want:  true,
		},
		{
			name: "one sd spec with text among several specs",
			app:  agent,
			specs: []routing.RuntimeSpec{
				{Enabled: true, Binary: "/opt/llama/llama-server", APIFlavors: []string{routing.APIFlavorOpenAI}},
				{Enabled: true, Binary: sdBinary, APIFlavors: []string{routing.APIFlavorOpenAIImages}},
				{Enabled: true, Type: sdType, Binary: sdBinary, APIFlavors: []string{routing.APIFlavorOpenAI, routing.APIFlavorOpenAIImages}},
			},
			want: true,
		},
		{
			name:  "a non-agent application's specs are never read",
			app:   routing.Application{Type: routing.ProviderVLLM, APIFlavors: []string{routing.APIFlavorOpenAI}},
			specs: []routing.RuntimeSpec{{Enabled: true, Type: sdType, Binary: sdBinary, APIFlavors: []string{routing.APIFlavorOpenAI}}},
			want:  false,
		},
		{
			name:  "no specs",
			app:   agent,
			specs: nil,
			want:  false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := anyStableDiffusionSpecNotImagesOnly(tc.app, tc.specs); got != tc.want {
				t.Fatalf("anyStableDiffusionSpecNotImagesOnly(%+v, %+v) = %v, want %v", tc.app, tc.specs, got, tc.want)
			}
		})
	}
}

// TestRuntimeWarningsReportsSpecFlavorContradictions drives both flavor
// warnings through RuntimeWarnings over specs written by PutRuntimeSpec, and
// clears them by the two fixes an operator makes: declaring the flavor on the
// application, and narrowing the sd spec to images. Every spec stays
// disabled, so the timeout warning never joins and each step pins the exact
// list.
func TestRuntimeWarningsReportsSpecFlavorContradictions(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	svc, routeStore := newServerTestService(t, now)
	server := createTestServer(t, svc, "S", "s.example.test")
	app := seedServerAgentApplication(t, routeStore, server.ID, now) // APIFlavors [openai]
	images, err := svc.CreateMapping(ctx, ownerToken(), app.ID, CreateMappingRequest{GatewayModelName: "sd-images", AppModelName: "sd-images"})
	if err != nil {
		t.Fatalf("CreateMapping sd-images: %v", err)
	}
	text, err := svc.CreateMapping(ctx, ownerToken(), app.ID, CreateMappingRequest{GatewayModelName: "sd-text", AppModelName: "sd-text"})
	if err != nil {
		t.Fatalf("CreateMapping sd-text: %v", err)
	}
	requireWarnings := func(step string, want []string) {
		t.Helper()
		got, err := svc.RuntimeWarnings(ctx, ownerToken(), app.ID)
		if err != nil {
			t.Fatalf("%s: RuntimeWarnings: %v", step, err)
		}
		if got == nil || !slices.Equal(got, want) {
			t.Fatalf("%s: warnings = %#v, want %#v", step, got, want)
		}
	}
	putSpec := func(mappingID, specType string, flavors []string) {
		t.Helper()
		if _, err := svc.PutRuntimeSpec(ctx, ownerToken(), mappingID, PutRuntimeSpecRequest{
			Binary: "/opt/sd/sd-server", Type: specType, Enabled: false, APIFlavors: flavors,
		}); err != nil {
			t.Fatalf("PutRuntimeSpec(%s): %v", mappingID, err)
		}
	}

	// Auto type, sd-server binary, images only: sd-clean, but the application
	// does not declare openai_images -- the operator's configuration.
	putSpec(images.ID, "", []string{routing.APIFlavorOpenAIImages})
	requireWarnings("images-only spec on an application without openai_images",
		[]string{"api_flavors_not_on_application"})

	// An explicit sd spec that keeps a text flavor.
	putSpec(text.ID, string(routing.RuntimeSpecTypeStableDiffusionCpp), []string{routing.APIFlavorOpenAI})
	requireWarnings("plus an sd spec with a text flavor",
		[]string{"api_flavors_not_on_application", "api_flavors_text_on_stable_diffusion"})

	// Declaring openai_images on the application clears the first warning.
	declared := []string{routing.APIFlavorOpenAI, routing.APIFlavorOpenAIImages}
	if _, err := svc.UpdateApplication(ctx, ownerToken(), app.ID, UpdateApplicationRequest{APIFlavors: &declared}); err != nil {
		t.Fatalf("UpdateApplication: %v", err)
	}
	requireWarnings("after the application declares openai_images",
		[]string{"api_flavors_text_on_stable_diffusion"})

	// Narrowing the sd spec to images clears the second.
	putSpec(text.ID, string(routing.RuntimeSpecTypeStableDiffusionCpp), []string{routing.APIFlavorOpenAIImages})
	requireWarnings("after the sd spec is narrowed to images", []string{})
}
