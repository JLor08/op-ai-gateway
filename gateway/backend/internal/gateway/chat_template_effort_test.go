// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"op-ai-gateway/internal/routing"
	"testing"
)

// TestTargetUsesChatTemplateKwargs walks the selector over every shape of
// resolved target that matters for the #230 effort-compatibility paths. The two
// ways to get this wrong are symmetrical and both are covered by a row:
//
//   - reading only LiveProgressSpecType misses a DIRECT llama.cpp/vLLM
//     application, whose Provider is "llama_cpp"/"vllm" and whose
//     LiveProgressSpecType is empty (routing.Resolver.targetFrom fills that
//     field for a server_agent mapping only);
//   - reading only Provider misses an AGENT-MANAGED runtime, whose Provider is
//     the literal "server_agent" and whose backend kind lives in
//     LiveProgressSpecType.
//
// llama_swap is the near-miss the equality exists to refuse: it proxies to
// arbitrary downstreams, so "llama" appearing in its name says nothing about
// what serves, and a chat template there may not read reasoning_effort at all.
func TestTargetUsesChatTemplateKwargs(t *testing.T) {
	cases := []struct {
		name   string
		target routing.Target
		want   bool
	}{
		{
			name:   "direct llama.cpp application",
			target: routing.Target{Provider: routing.ProviderLlamaCPP},
			want:   true,
		},
		{
			name:   "direct vLLM application",
			target: routing.Target{Provider: routing.ProviderVLLM},
			want:   true,
		},
		{
			name: "server_agent whose runtime is llama.cpp",
			target: routing.Target{
				Provider:             routing.ProviderServerAgent,
				LiveProgressSpecType: string(routing.RuntimeSpecTypeLlamaCpp),
			},
			want: true,
		},
		{
			name: "server_agent whose runtime is vLLM",
			target: routing.Target{
				Provider:             routing.ProviderServerAgent,
				LiveProgressSpecType: string(routing.RuntimeSpecTypeVLLM),
			},
			want: true,
		},
		{
			name: "server_agent whose runtime is ollama",
			target: routing.Target{
				Provider:             routing.ProviderServerAgent,
				LiveProgressSpecType: string(routing.RuntimeSpecTypeOllama),
			},
			want: false,
		},
		{
			name: "server_agent whose runtime is TGI",
			target: routing.Target{
				Provider:             routing.ProviderServerAgent,
				LiveProgressSpecType: string(routing.RuntimeSpecTypeTGI),
			},
			want: false,
		},
		{
			name: "server_agent whose runtime is custom",
			target: routing.Target{
				Provider:             routing.ProviderServerAgent,
				LiveProgressSpecType: string(routing.RuntimeSpecTypeCustom),
			},
			want: false,
		},
		{
			// A server_agent mapping that has no spec still resolves to a
			// spec type ("custom"), but a hand-built Target may leave it
			// empty; neither says llama.cpp or vLLM.
			name:   "server_agent with no spec type",
			target: routing.Target{Provider: routing.ProviderServerAgent},
			want:   false,
		},
		{
			// The substitution is one-way: a non-server_agent Provider is the
			// kind, whatever stray LiveProgressSpecType rides along.
			name: "direct ollama with a stray llama_cpp spec type",
			target: routing.Target{
				Provider:             routing.ProviderOllama,
				LiveProgressSpecType: string(routing.RuntimeSpecTypeLlamaCpp),
			},
			want: false,
		},
		{
			name: "direct llama.cpp is not overridden by a different spec type",
			target: routing.Target{
				Provider:             routing.ProviderLlamaCPP,
				LiveProgressSpecType: string(routing.RuntimeSpecTypeOllama),
			},
			want: true,
		},
		{
			name:   "llama_swap",
			target: routing.Target{Provider: routing.ProviderLlamaSwap},
			want:   false,
		},
		{
			name: "llama_swap with a llama_cpp spec type",
			target: routing.Target{
				Provider:             routing.ProviderLlamaSwap,
				LiveProgressSpecType: string(routing.RuntimeSpecTypeLlamaCpp),
			},
			want: false,
		},
		{
			name:   "ollama",
			target: routing.Target{Provider: routing.ProviderOllama},
			want:   false,
		},
		{
			name:   "litellm",
			target: routing.Target{Provider: routing.ProviderLiteLLM},
			want:   false,
		},
		{
			name:   "empty provider",
			target: routing.Target{},
			want:   false,
		},
		{
			name:   "native anthropic vendor",
			target: routing.Target{Provider: routing.ProviderVendorAnthropic},
			want:   false,
		},
		{
			name:   "openai vendor",
			target: routing.Target{Provider: routing.ProviderVendorOpenAI},
			want:   false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := targetUsesChatTemplateKwargs(tc.target); got != tc.want {
				t.Errorf("targetUsesChatTemplateKwargs(target{provider=%q spec_type=%q}) = %v, want %v",
					tc.target.Provider, tc.target.LiveProgressSpecType, got, tc.want)
			}
		})
	}
}

// TestChatTemplateKwargsKindsShareOneStringAcrossBothVocabularies pins the
// string equality targetUsesChatTemplateKwargs relies on: it tests ONE kind --
// Provider for an ordinary application, LiveProgressSpecType (a
// routing.RuntimeSpecType rendered as a string) for a server_agent one --
// against routing.ProviderLlamaCPP and routing.ProviderVLLM alone. Rename a
// RuntimeSpecType value and the agent-managed half of the selector goes dark
// without any other test noticing, because the table above builds its
// server_agent rows FROM the spec-type constants and so follows the rename.
func TestChatTemplateKwargsKindsShareOneStringAcrossBothVocabularies(t *testing.T) {
	for _, pair := range []struct {
		provider string
		specType routing.RuntimeSpecType
	}{
		{routing.ProviderLlamaCPP, routing.RuntimeSpecTypeLlamaCpp},
		{routing.ProviderVLLM, routing.RuntimeSpecTypeVLLM},
	} {
		if pair.provider != string(pair.specType) {
			t.Errorf("provider %q and runtime spec type %q no longer share one string: targetUsesChatTemplateKwargs substitutes one vocabulary for the other on a server_agent target and would stop recognising agent-managed runtimes",
				pair.provider, pair.specType)
		}
	}
}
