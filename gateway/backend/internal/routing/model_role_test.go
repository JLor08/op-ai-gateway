// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package routing

import "testing"

// The role strings are a wire and persisted contract: they ride the portal DTOs
// and the pushed agent config, and the agent and the editor both compare them as
// literals. Pin every one.
func TestModelRoleStrings(t *testing.T) {
	for role, want := range map[ModelRole]string{
		RoleASR:       "asr",
		RoleTTS:       "tts",
		RoleImage:     "image",
		RoleEmbedding: "embedding",
		RoleReranker:  "reranker",
		RoleChat:      "chat",
		RoleOther:     "other",
	} {
		if string(role) != want {
			t.Errorf("role %q = %q, want %q", role, string(role), want)
		}
	}
}

func TestDeriveRole(t *testing.T) {
	yes := func(names ...string) map[string]bool {
		m := make(map[string]bool, len(names))
		for _, n := range names {
			m[n] = true
		}
		return m
	}
	for _, tc := range []struct {
		name     string
		specType RuntimeSpecType
		caps     map[string]bool
		want     ModelRole
	}{
		// One per role, from the capability alone.
		{"transcription is asr", RuntimeSpecTypeAudioCpp, yes(CapabilityTranscription), RoleASR},
		{"speech is tts", RuntimeSpecTypeAudioCpp, yes(CapabilitySpeech), RoleTTS},
		{"image is image", RuntimeSpecTypeStableDiffusionCpp, yes(CapabilityImage), RoleImage},
		{"embedding is embedding", RuntimeSpecTypeVLLM, yes(CapabilityEmbedding), RoleEmbedding},
		{"rerank is reranker", RuntimeSpecTypeVLLM, yes(CapabilityRerank), RoleReranker},
		{"vision on an llm server is chat", RuntimeSpecTypeVLLM, yes(CapabilityVision), RoleChat},

		// image_edit is a subset of image, but a stored (image_edit, yes) alone is
		// still an image model: rule 3 reads either name.
		{"image_edit alone is image", RuntimeSpecTypeStableDiffusionCpp, yes(CapabilityImageEdit), RoleImage},
		{"image and image_edit is image", RuntimeSpecTypeStableDiffusionCpp, yes(CapabilityImage, CapabilityImageEdit), RoleImage},

		// Tie-break, documented on DeriveRole: a model that both transcribes and
		// synthesises resolves to asr, the earlier rule.
		{"transcription and speech is asr", RuntimeSpecTypeAudioCpp, yes(CapabilityTranscription, CapabilitySpeech), RoleASR},

		// First match wins across the rest of the capability order.
		{"speech beats image", RuntimeSpecTypeAudioCpp, yes(CapabilitySpeech, CapabilityImage), RoleTTS},
		{"image beats embedding", RuntimeSpecTypeCustom, yes(CapabilityImage, CapabilityEmbedding), RoleImage},
		{"embedding beats rerank", RuntimeSpecTypeVLLM, yes(CapabilityEmbedding, CapabilityRerank), RoleEmbedding},
		{"rerank beats chat-ish caps", RuntimeSpecTypeVLLM, yes(CapabilityRerank, CapabilityVision, CapabilityTools), RoleReranker},

		// A capability outranks the runtime type: an LLM server that was marked
		// as hosting an embedding model, or an audio server marked as ASR.
		{"embedding on llama_cpp is embedding", RuntimeSpecTypeLlamaCpp, yes(CapabilityEmbedding), RoleEmbedding},
		{"transcription on vllm is asr", RuntimeSpecTypeVLLM, yes(CapabilityTranscription), RoleASR},
		{"embedding on sd-server is embedding", RuntimeSpecTypeStableDiffusionCpp, yes(CapabilityEmbedding), RoleEmbedding},

		// The type is the coarse fallback when no capability decides.
		{"bare stable_diffusion_cpp is image", RuntimeSpecTypeStableDiffusionCpp, nil, RoleImage},
		{"bare vllm is chat", RuntimeSpecTypeVLLM, nil, RoleChat},
		{"bare llama_cpp is chat", RuntimeSpecTypeLlamaCpp, nil, RoleChat},
		{"bare tgi is chat", RuntimeSpecTypeTGI, nil, RoleChat},
		{"bare ollama is chat", RuntimeSpecTypeOllama, nil, RoleChat},
		{"bare vllm with an empty map is chat", RuntimeSpecTypeVLLM, map[string]bool{}, RoleChat},
		// An LLM server whose only verdicts are unrelated or negative is still chat.
		{"vllm with a no verdict is chat", RuntimeSpecTypeVLLM, map[string]bool{CapabilityTranscription: false}, RoleChat},

		// Chat-ish capabilities make a chat model of a server whose type says
		// nothing: a custom launcher (or an unresolved type) serving an LLM.
		{"vision on custom is chat", RuntimeSpecTypeCustom, yes(CapabilityVision), RoleChat},
		{"video on an unknown type is chat", RuntimeSpecType(""), yes(CapabilityVideo), RoleChat},
		{"audio input on custom is chat", RuntimeSpecTypeCustom, yes(CapabilityAudio), RoleChat},
		{"tools on custom is chat", RuntimeSpecTypeCustom, yes(CapabilityTools), RoleChat},
		{"mtp on custom is chat", RuntimeSpecTypeCustom, yes(CapabilityMTP), RoleChat},

		// Nothing recognisable: other. audio_cpp alone is deliberately other, not
		// asr or tts: one audio.cpp server hosts either and the type cannot say which.
		{"unknown type, no caps is other", RuntimeSpecType("no-such-kind"), nil, RoleOther},
		{"empty type, no caps is other", RuntimeSpecType(""), nil, RoleOther},
		{"bare custom is other", RuntimeSpecTypeCustom, nil, RoleOther},
		{"bare audio_cpp is other", RuntimeSpecTypeAudioCpp, nil, RoleOther},
		{"audio_cpp with only a no verdict is other", RuntimeSpecTypeAudioCpp, map[string]bool{CapabilitySpeech: false}, RoleOther},
		// Internal observations are not chat-ish evidence: they describe how a
		// completion endpoint behaved, not what the model is.
		{"live_progress alone on custom is other", RuntimeSpecTypeCustom, yes(CapabilityLiveProgress), RoleOther},
		{"speculation_observed alone on custom is other", RuntimeSpecTypeCustom, yes(CapabilitySpeculationObserved), RoleOther},
		{"a refinement alone is other", RuntimeSpecTypeCustom, yes(CapabilityTranscriptionStreaming, CapabilitySpeechClone, CapabilitySpeechStreaming), RoleOther},

		// A false entry never counts, whatever its name.
		{"false transcription does not make asr", RuntimeSpecTypeAudioCpp, map[string]bool{CapabilityTranscription: false, CapabilitySpeech: true}, RoleTTS},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := DeriveRole(tc.specType, tc.caps); got != tc.want {
				t.Errorf("DeriveRole(%q, %v) = %q, want %q", tc.specType, tc.caps, got, tc.want)
			}
		})
	}
}

// DeriveRole consumes the EFFECTIVE type: the caller resolves an auto-detect
// spec (Type "") through EffectiveRuntimeSpecType first, so a launcher whose
// binary says vllm is chat without the operator ever setting a type.
func TestDeriveRoleOfAnAutoDetectedSpec(t *testing.T) {
	for _, tc := range []struct {
		binary string
		want   ModelRole
	}{
		{"/opt/vllm/bin/vllm", RoleChat},
		{"/usr/local/bin/llama-server", RoleChat},
		{"/opt/sd/sd-server", RoleImage},
		{"/opt/audiocpp/audiocpp_server", RoleOther},
		{"/opt/whatever/serve.sh", RoleOther},
	} {
		spec := RuntimeSpec{Binary: tc.binary}
		if got := DeriveRole(EffectiveRuntimeSpecType(spec), nil); got != tc.want {
			t.Errorf("DeriveRole(EffectiveRuntimeSpecType(%q), nil) = %q, want %q", tc.binary, got, tc.want)
		}
	}
}

func TestYesCapabilities(t *testing.T) {
	rows := []CapabilityRow{
		{Capability: CapabilityVision, Verdict: CapabilityYes},
		{Capability: CapabilityImage, Verdict: CapabilityNo},
		{Capability: CapabilityEmbedding, Verdict: CapabilityYes},
		{Capability: CapabilityRerank, Verdict: ""},
	}
	got := YesCapabilities(rows)
	if len(got) != 2 || !got[CapabilityVision] || !got[CapabilityEmbedding] {
		t.Errorf("YesCapabilities = %v, want exactly {vision, embedding}", got)
	}
	if got[CapabilityImage] || got[CapabilityRerank] {
		t.Errorf("YesCapabilities = %v, a no or empty verdict must not appear", got)
	}
	if got := YesCapabilities(nil); got == nil || len(got) != 0 {
		t.Errorf("YesCapabilities(nil) = %#v, want a non-nil empty map", got)
	}
}
