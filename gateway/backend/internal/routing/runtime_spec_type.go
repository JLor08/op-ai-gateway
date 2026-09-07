// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package routing

import (
	"path/filepath"
	"strings"
)

// RuntimeSpecType is the per-spec explicit choice of which runtime SERVER
// KIND spec.Binary launches, driving which per-kind metrics/context-probe
// conventions the agent applies (design 2026-09-07). Default (and every
// pre-feature row) is "" = auto-detect from the binary, which preserves
// today's behaviour. Serialized as its lowercase string, stored text
// (mirrors RuntimeAPITokenMode). Validated at the DTO edge, not by a method
// here.
type RuntimeSpecType string

const (
	RuntimeSpecTypeVLLM     RuntimeSpecType = "vllm"
	RuntimeSpecTypeLlamaCpp RuntimeSpecType = "llama_cpp"
	RuntimeSpecTypeTGI      RuntimeSpecType = "tgi"
	RuntimeSpecTypeOllama   RuntimeSpecType = "ollama"
	RuntimeSpecTypeCustom   RuntimeSpecType = "custom"
)

// DetectRuntimeSpecType infers the runtime server kind from the launched
// binary's basename when no explicit RuntimeSpec.Type is set. It matches on
// case-insensitive substrings, in the order below (first match wins), and
// falls back to RuntimeSpecTypeCustom when nothing matches.
func DetectRuntimeSpecType(binary string) RuntimeSpecType {
	name := strings.ToLower(filepath.Base(binary))

	switch {
	case strings.Contains(name, "vllm"):
		return RuntimeSpecTypeVLLM
	case strings.Contains(name, "llama-server"), strings.Contains(name, "llama_cpp"), strings.Contains(name, "llama.cpp"):
		return RuntimeSpecTypeLlamaCpp
	case strings.Contains(name, "text-generation-launcher"), strings.Contains(name, "tgi"):
		return RuntimeSpecTypeTGI
	case strings.Contains(name, "ollama"):
		return RuntimeSpecTypeOllama
	default:
		return RuntimeSpecTypeCustom
	}
}

// EffectiveRuntimeSpecType resolves the RuntimeSpecType that governs a given
// spec: the explicit spec.Type when set, else the type detected from
// spec.Binary.
func EffectiveRuntimeSpecType(spec RuntimeSpec) RuntimeSpecType {
	if spec.Type != "" {
		return RuntimeSpecType(spec.Type)
	}
	return DetectRuntimeSpecType(spec.Binary)
}

// DeriveProbePaths resolves the metrics and context-probe paths to use for a
// given effective RuntimeSpecType. A non-empty override always wins for its
// field; otherwise the per-type default applies.
func DeriveProbePaths(t RuntimeSpecType, metricsOverride, contextOverride string) (metricsPath, contextPath string) {
	var defaultMetrics, defaultContext string

	switch t {
	case RuntimeSpecTypeVLLM:
		defaultMetrics, defaultContext = "/metrics", "/v1/models"
	case RuntimeSpecTypeLlamaCpp:
		defaultMetrics, defaultContext = "/metrics", "/props"
	case RuntimeSpecTypeTGI:
		// Verified 2026-09-07: text-generation-inference exposes Prometheus
		// metrics at /metrics (huggingface/text-generation-inference docs,
		// https://huggingface.co/docs/text-generation-inference/en/reference/metrics)
		// and serves max_total_tokens (the context-size field) from /info
		// (docs/openapi.json "Info" schema,
		// https://github.com/huggingface/text-generation-inference/blob/main/docs/openapi.json).
		defaultMetrics, defaultContext = "/metrics", "/info"
	case RuntimeSpecTypeOllama:
		// Verified 2026-09-07: Ollama has no native Prometheus-style /metrics
		// endpoint (no metrics endpoint of any kind is documented in
		// ollama/ollama docs/api.md,
		// https://github.com/ollama/ollama/blob/main/docs/api.md) and serves
		// the context length from /api/show under model_info as an
		// architecture-prefixed key (e.g. "llama.context_length"), per the
		// same source's "Show Model Information" section.
		defaultMetrics, defaultContext = "", "/api/show"
	case RuntimeSpecTypeCustom:
		defaultMetrics, defaultContext = "", ""
	default:
		defaultMetrics, defaultContext = "", ""
	}

	metricsPath = defaultMetrics
	if metricsOverride != "" {
		metricsPath = metricsOverride
	}

	contextPath = defaultContext
	if contextOverride != "" {
		contextPath = contextOverride
	}

	return metricsPath, contextPath
}
