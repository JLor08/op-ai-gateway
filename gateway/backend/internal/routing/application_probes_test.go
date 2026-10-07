// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package routing

import "testing"

// TestEffectiveProbes pins both resolvers on one table. The expectations are
// literals, not the exported constants, so a changed router contract fails
// here instead of moving silently with the constant. The whitespace rows pin
// the asymmetry: EffectiveLoadedModelsProbe hands a non-blank stored path back
// untrimmed, EffectiveContextProbePath trims it.
func TestEffectiveProbes(t *testing.T) {
	cases := []struct {
		name       string
		app        Application
		hasProps   bool
		wantPath   string
		wantFormat string
		wantCtx    string
	}{
		{
			name:     "agent with empty fields and the props route declared",
			app:      Application{Type: ProviderServerAgent},
			hasProps: true,
			wantPath: "/running", wantFormat: "llama_swap", wantCtx: "/upstream/{model}/props",
		},
		{
			name:     "agent with empty fields and no props route declared",
			app:      Application{Type: ProviderServerAgent},
			hasProps: false,
			wantPath: "/running", wantFormat: "llama_swap", wantCtx: "",
		},
		{
			name: "agent with API-set paths and the props route declared",
			app: Application{
				Type: ProviderServerAgent, LoadedModelsPath: "/custom-running",
				LoadedModelsFormat: "openai", ContextProbePath: "/props",
			},
			hasProps: true,
			wantPath: "/custom-running", wantFormat: "openai", wantCtx: "/props",
		},
		{
			name: "agent with API-set paths and no props route declared",
			app: Application{
				Type: ProviderServerAgent, LoadedModelsPath: "/custom-running",
				LoadedModelsFormat: "openai", ContextProbePath: "/props",
			},
			hasProps: false,
			wantPath: "/custom-running", wantFormat: "openai", wantCtx: "/props",
		},
		{
			name:     "agent with an API-set loaded path and no format keeps the empty format",
			app:      Application{Type: ProviderServerAgent, LoadedModelsPath: "/custom-running"},
			hasProps: true,
			wantPath: "/custom-running", wantFormat: "", wantCtx: "/upstream/{model}/props",
		},
		{
			name: "agent with whitespace-only values gets the router defaults",
			app: Application{
				Type: ProviderServerAgent, LoadedModelsPath: "  ",
				LoadedModelsFormat: "openai", ContextProbePath: " \t ",
			},
			hasProps: true,
			wantPath: "/running", wantFormat: "llama_swap", wantCtx: "/upstream/{model}/props",
		},
		{
			name: "agent with padded values keeps the loaded path and trims the context path",
			app: Application{
				Type: ProviderServerAgent, LoadedModelsPath: " /custom-running ",
				LoadedModelsFormat: "openai", ContextProbePath: " /props ",
			},
			hasProps: true,
			wantPath: " /custom-running ", wantFormat: "openai", wantCtx: "/props",
		},
		{
			name:     "non-agent with empty fields",
			app:      Application{Type: ProviderLlamaSwap},
			hasProps: true,
			wantPath: "", wantFormat: "", wantCtx: "",
		},
		{
			name: "non-agent with values",
			app: Application{
				Type: ProviderLlamaSwap, LoadedModelsPath: "/running",
				LoadedModelsFormat: "llama_swap", ContextProbePath: "/upstream/{model}/props",
			},
			hasProps: false,
			wantPath: "/running", wantFormat: "llama_swap", wantCtx: "/upstream/{model}/props",
		},
		{
			name: "non-agent with whitespace-only values",
			app: Application{
				Type: ProviderVLLM, LoadedModelsPath: "  ",
				LoadedModelsFormat: "openai", ContextProbePath: "  ",
			},
			hasProps: true,
			wantPath: "", wantFormat: "", wantCtx: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path, format := EffectiveLoadedModelsProbe(tc.app)
			if path != tc.wantPath || format != tc.wantFormat {
				t.Fatalf("EffectiveLoadedModelsProbe = (%q, %q), want (%q, %q)", path, format, tc.wantPath, tc.wantFormat)
			}
			if got := EffectiveContextProbePath(tc.app, tc.hasProps); got != tc.wantCtx {
				t.Fatalf("EffectiveContextProbePath(hasProps=%v) = %q, want %q", tc.hasProps, got, tc.wantCtx)
			}
		})
	}
}
