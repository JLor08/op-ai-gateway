// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package routing

// ModelRole is the ONE functional role a model plays, derived (DeriveRole) from
// what it can do and, as a coarse fallback, from the runtime server kind that
// hosts it. It is the single value behind two decisions that must never
// diverge: how the co-residency editor GROUPS models, and the agent's
// "same role" rule when it picks an eviction victim. Both read the value the
// gateway derived and shipped (portal ModelDTO.Role and AgentRuntimeSpecDTO.Role),
// never a re-derivation of their own.
//
// It is derived, never stored: a role is a pure function of the model's
// capability verdicts and its runtime type, so it follows an operator's verdict
// change with no migration and can never be stale. Serialized as its lowercase
// string.
type ModelRole string

const (
	// RoleASR is a speech-to-text model.
	RoleASR ModelRole = "asr"
	// RoleTTS is a text-to-speech model.
	RoleTTS ModelRole = "tts"
	// RoleImage is an image model. Generation and editing collapse into this one
	// role: image_edit is a subset of image (ImpliedImageEdit), and for grouping
	// and eviction alike "image evicts image" is the whole rule.
	RoleImage ModelRole = "image"
	// RoleEmbedding is a text-embedding model.
	RoleEmbedding ModelRole = "embedding"
	// RoleReranker is a reranking model. The role is the noun, the capability
	// (CapabilityRerank) the verb.
	RoleReranker ModelRole = "reranker"
	// RoleChat is a text-generation model: an LLM server, or anything showing
	// chat-only abilities (vision input, tool use, ...).
	RoleChat ModelRole = "chat"
	// RoleOther is everything with no recognisable role: a custom launcher with
	// no verdicts, or an audio.cpp server that has not been told whether it hosts
	// an asr or a tts model. It is a real group with a real eviction rule, not an
	// error value: an unclassified model groups with its own kind and evicts like
	// any other role.
	RoleOther ModelRole = "other"
)

// chatCapabilities are the capabilities that only a text-generating model has
// any use for, and so are evidence of RoleChat on a server whose runtime type
// says nothing (custom, or one the type list does not know): the model accepts
// images, video or audio as chat input, calls tools, or ships a multi-token
// prediction head. live_progress and speculation_observed are deliberately
// absent: they record how a completion endpoint BEHAVED (a probe of the server,
// an observation of relayed traffic), not what the model is, and the llama.cpp
// probe behind the first answers for any llama-server, an embedding model's
// included.
var chatCapabilities = [...]string{
	CapabilityVision,
	CapabilityVideo,
	CapabilityAudio,
	CapabilityTools,
	CapabilityMTP,
}

// DeriveRole derives the single ModelRole of a model from its effective runtime
// type (EffectiveRuntimeSpecType: the explicit spec type, or the one detected
// from the binary) and its capability verdicts (caps maps a capability name to
// true when its verdict is yes; YesCapabilities builds it from stored rows -- an
// absent name and a false one both mean "not established").
//
// FIRST MATCH WINS, so the order below is the tie-break and is part of the
// contract:
//
//  1. transcription           -> RoleASR
//  2. speech                  -> RoleTTS
//  3. image or image_edit     -> RoleImage
//  4. embedding               -> RoleEmbedding
//  5. rerank                  -> RoleReranker
//  6. the type, as a coarse fallback when no capability decided:
//     stable_diffusion_cpp    -> RoleImage
//     vllm, llama_cpp, tgi, ollama -> RoleChat
//  7. any chat-only capability (chatCapabilities) -> RoleChat
//  8. otherwise               -> RoleOther
//
// Capabilities outrank the type because they are specific and the type is not:
// a vllm server the operator marked embedding hosts an embedding model, and an
// operator verdict is exactly how a model acquires that role. The consequence
// is the one the design accepts: a vllm server hosting an embedding model with
// NO verdict reads as chat, because nothing else can tell. A model that both
// transcribes and synthesises speech (rare: one model rarely does both) is
// RoleASR, by rule 1 over rule 2.
//
// audio_cpp is deliberately not a type fallback: one audio.cpp server hosts asr
// or tts models and the type cannot say which, so a bare audio_cpp model is
// RoleOther until a verdict (which the audio.cpp probe writes from the model's
// task) says otherwise. stable_diffusion_cpp has only one job, so its type alone
// is enough.
func DeriveRole(specType RuntimeSpecType, caps map[string]bool) ModelRole {
	switch {
	case caps[CapabilityTranscription]:
		return RoleASR
	case caps[CapabilitySpeech]:
		return RoleTTS
	case caps[CapabilityImage], caps[CapabilityImageEdit]:
		return RoleImage
	case caps[CapabilityEmbedding]:
		return RoleEmbedding
	case caps[CapabilityRerank]:
		return RoleReranker
	}

	switch specType {
	case RuntimeSpecTypeStableDiffusionCpp:
		return RoleImage
	case RuntimeSpecTypeVLLM, RuntimeSpecTypeLlamaCpp, RuntimeSpecTypeTGI, RuntimeSpecTypeOllama:
		return RoleChat
	}

	for _, capability := range chatCapabilities {
		if caps[capability] {
			return RoleChat
		}
	}
	return RoleOther
}

// RoleRank orders the roles by DeriveRole's priority, most specific first, with
// RoleOther -- the absence of evidence -- last. A role outside the list ranks
// after RoleOther. It exists for the callers that have several derivations to
// reconcile for one name: a gateway model name offered by several mappings, each
// with its own capability rows and runtime spec, and a model group of several
// members, take the best-ranked of the roles, so one that establishes a role is
// not overruled by another that merely knows nothing.
func RoleRank(role ModelRole) int {
	switch role {
	case RoleASR:
		return 0
	case RoleTTS:
		return 1
	case RoleImage:
		return 2
	case RoleEmbedding:
		return 3
	case RoleReranker:
		return 4
	case RoleChat:
		return 5
	case RoleOther:
		return 6
	default:
		return 7
	}
}

// YesCapabilities projects a mapping's stored capability rows onto the map
// DeriveRole consumes: the names whose verdict is yes, each mapped to true. A no
// row, a row with no verdict and an absent name are all simply not in the map,
// because role derivation reads only established abilities. The result is never
// nil.
func YesCapabilities(rows []CapabilityRow) map[string]bool {
	out := make(map[string]bool, len(rows))
	for _, row := range rows {
		if row.Verdict == CapabilityYes {
			out[row.Capability] = true
		}
	}
	return out
}
