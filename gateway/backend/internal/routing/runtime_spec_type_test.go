// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package routing

import "testing"

func TestRuntimeSpecType_Detect(t *testing.T) {
	cases := []struct {
		name   string
		binary string
		want   RuntimeSpecType
	}{
		{"vllm", "/usr/bin/vllm", RuntimeSpecTypeVLLM},
		{"llama_cpp llama-server", "/usr/local/bin/llama-server", RuntimeSpecTypeLlamaCpp},
		{"llama_cpp llama_cpp", "/opt/bin/llama_cpp", RuntimeSpecTypeLlamaCpp},
		{"llama_cpp llama.cpp", "/opt/bin/llama.cpp", RuntimeSpecTypeLlamaCpp},
		{"tgi text-generation-launcher", "/usr/bin/text-generation-launcher", RuntimeSpecTypeTGI},
		{"tgi short", "/usr/bin/tgi", RuntimeSpecTypeTGI},
		{"ollama", "/usr/local/bin/ollama", RuntimeSpecTypeOllama},
		{"custom fallback", "/usr/bin/my-thing", RuntimeSpecTypeCustom},
		{"empty binary", "", RuntimeSpecTypeCustom},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := DetectRuntimeSpecType(tc.binary)
			if got != tc.want {
				t.Errorf("DetectRuntimeSpecType(%q) = %q, want %q", tc.binary, got, tc.want)
			}
		})
	}
}

func TestRuntimeSpecType_Effective(t *testing.T) {
	cases := []struct {
		name string
		spec RuntimeSpec
		want RuntimeSpecType
	}{
		{
			name: "explicit type overrides detection",
			spec: RuntimeSpec{Type: string(RuntimeSpecTypeCustom), Binary: "/usr/bin/vllm"},
			want: RuntimeSpecTypeCustom,
		},
		{
			name: "explicit type matches detection",
			spec: RuntimeSpec{Type: string(RuntimeSpecTypeOllama), Binary: "/usr/bin/vllm"},
			want: RuntimeSpecTypeOllama,
		},
		{
			name: "empty type falls back to detection",
			spec: RuntimeSpec{Type: "", Binary: "/usr/local/bin/llama-server"},
			want: RuntimeSpecTypeLlamaCpp,
		},
		{
			name: "empty type and unrecognized binary falls back to custom",
			spec: RuntimeSpec{Type: "", Binary: "/usr/bin/my-thing"},
			want: RuntimeSpecTypeCustom,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := EffectiveRuntimeSpecType(tc.spec)
			if got != tc.want {
				t.Errorf("EffectiveRuntimeSpecType(%+v) = %q, want %q", tc.spec, got, tc.want)
			}
		})
	}
}

func TestRuntimeSpecType_DeriveProbePaths(t *testing.T) {
	cases := []struct {
		name            string
		t               RuntimeSpecType
		metricsOverride string
		contextOverride string
		wantMetrics     string
		wantContext     string
	}{
		{"vllm defaults", RuntimeSpecTypeVLLM, "", "", "/metrics", "/v1/models"},
		{"llama_cpp defaults", RuntimeSpecTypeLlamaCpp, "", "", "/metrics", "/props"},
		{"tgi defaults", RuntimeSpecTypeTGI, "", "", "/metrics", "/info"},
		{"ollama defaults", RuntimeSpecTypeOllama, "", "", "", "/api/show"},
		{"custom defaults", RuntimeSpecTypeCustom, "", "", "", ""},
		{"overrides win over vllm defaults", RuntimeSpecTypeVLLM, "/x", "/y", "/x", "/y"},
		{"metrics override only", RuntimeSpecTypeLlamaCpp, "/custom-metrics", "", "/custom-metrics", "/props"},
		{"context override only", RuntimeSpecTypeTGI, "", "/custom-info", "/metrics", "/custom-info"},
		{"overrides on custom", RuntimeSpecTypeCustom, "/m", "/c", "/m", "/c"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotMetrics, gotContext := DeriveProbePaths(tc.t, tc.metricsOverride, tc.contextOverride)
			if gotMetrics != tc.wantMetrics || gotContext != tc.wantContext {
				t.Errorf("DeriveProbePaths(%q, %q, %q) = (%q, %q), want (%q, %q)",
					tc.t, tc.metricsOverride, tc.contextOverride, gotMetrics, gotContext, tc.wantMetrics, tc.wantContext)
			}
		})
	}
}
