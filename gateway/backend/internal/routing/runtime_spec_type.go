// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package routing

import (
	"encoding/json"
	"path/filepath"
	"strconv"
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
	RuntimeSpecTypeVLLM               RuntimeSpecType = "vllm"
	RuntimeSpecTypeLlamaCpp           RuntimeSpecType = "llama_cpp"
	RuntimeSpecTypeTGI                RuntimeSpecType = "tgi"
	RuntimeSpecTypeOllama             RuntimeSpecType = "ollama"
	RuntimeSpecTypeStableDiffusionCpp RuntimeSpecType = "stable_diffusion_cpp"
	RuntimeSpecTypeAudioCpp           RuntimeSpecType = "audio_cpp"
	// RuntimeSpecTypeVllmSR is the agent-launched vLLM Semantic Router worker
	// (vllm-srun): the gateway routes only the decision endpoints to it.
	RuntimeSpecTypeVllmSR RuntimeSpecType = "vllm_sr"
	// RuntimeSpecTypeSGLang is the agent-launched SGLang server (v0.5.23,
	// sgl-project/sglang@297e54fd105e974cdf401e5b4d20253a22c16f90): a text
	// server with /metrics and a /v1/models listing that carries max_model_len.
	RuntimeSpecTypeSGLang RuntimeSpecType = "sglang"
	// RuntimeSpecTypeSGLangOmni is the agent-launched sglang-omni server
	// (v0.2.0, sgl-project/sglang-omni@e8c635ecb9c68bcf2fbc32ceb3c40f85f3590657):
	// a multimodal server without /metrics, whose /v1/models carries no context
	// length, so it has no probe path and no type-based role fallback.
	RuntimeSpecTypeSGLangOmni RuntimeSpecType = "sglang_omni"
	RuntimeSpecTypeCustom     RuntimeSpecType = "custom"
)

// VllmSRSupportedAPIMajor is the major version of the vllm-srun runtime API
// (the api_version field of its /v1/models body) this gateway is built
// against: runtime api_version 5.0.0 at
// vllm-project/semantic-router@4ec449f358ce7805228fb341bdc55ea5211707fd. A
// worker that reports a different major is not known to speak the decision
// endpoints the way this gateway expects (ADR-099).
const VllmSRSupportedAPIMajor = 5

// VllmSRAPIVersionUnsupported reports whether a vllm-srun worker's reported
// api_version names a major other than VllmSRSupportedAPIMajor. It is true
// only when a major can be parsed from the version's leading integer and it
// differs; an empty or unparsable version means unknown (an older agent, a
// failed probe) and is never unsupported (ADR-099).
func VllmSRAPIVersionUnsupported(version string) bool {
	version = strings.TrimSpace(version)
	end := 0
	for end < len(version) && version[end] >= '0' && version[end] <= '9' {
		end++
	}
	if end == 0 {
		return false
	}
	major, err := strconv.Atoi(version[:end])
	if err != nil {
		return false
	}
	return major != VllmSRSupportedAPIMajor
}

// defaultPrometheusMetricsPath is the Prometheus-style metrics path shared by every runtime
// kind that serves one at a fixed location: vLLM, llama.cpp, and TGI all default here.
const defaultPrometheusMetricsPath = "/metrics"

// DetectRuntimeSpecType infers the runtime server kind from the launched
// binary's basename when no explicit RuntimeSpec.Type is set. It matches on
// case-insensitive substrings -- vllm-srun/vllm_srun (checked before vllm;
// the bare vllm-sr is deliberately not one: the vllm-sr CLI is no spec binary,
// ADR-099, and the substring would catch names like vllm-srv),
// sgl-omni/sgl_omni/sglang-omni/sglang_omni (sglang_omni, checked before sglang
// so an omni name never detects as sglang), sglang, vllm,
// llama-server/llama_cpp/llama.cpp,
// text-generation-launcher/tgi, sd-server/stable-diffusion,
// audiocpp_server/audio.cpp, ollama -- in the order below (first match wins),
// and falls back to RuntimeSpecTypeCustom when nothing matches.
func DetectRuntimeSpecType(binary string) RuntimeSpecType {
	name := strings.ToLower(filepath.Base(binary))

	switch {
	case strings.Contains(name, "vllm-srun"), strings.Contains(name, "vllm_srun"):
		// Before the vllm case: the router worker's name contains "vllm".
		return RuntimeSpecTypeVllmSR
	case strings.Contains(name, "sgl-omni"), strings.Contains(name, "sgl_omni"),
		strings.Contains(name, "sglang-omni"), strings.Contains(name, "sglang_omni"):
		// Before the sglang case: an omni name contains "sglang".
		return RuntimeSpecTypeSGLangOmni
	case strings.Contains(name, "sglang"):
		return RuntimeSpecTypeSGLang
	case strings.Contains(name, "vllm"):
		return RuntimeSpecTypeVLLM
	case strings.Contains(name, "llama-server"), strings.Contains(name, "llama_cpp"), strings.Contains(name, "llama.cpp"):
		return RuntimeSpecTypeLlamaCpp
	case strings.Contains(name, "text-generation-launcher"), strings.Contains(name, "tgi"):
		return RuntimeSpecTypeTGI
	case strings.Contains(name, "sd-server"), strings.Contains(name, "stable-diffusion"):
		return RuntimeSpecTypeStableDiffusionCpp
	case strings.Contains(name, "audiocpp_server"), strings.Contains(name, "audio.cpp"):
		return RuntimeSpecTypeAudioCpp
	case strings.Contains(name, "ollama"):
		return RuntimeSpecTypeOllama
	default:
		return RuntimeSpecTypeCustom
	}
}

// EffectiveRuntimeSpecType resolves the RuntimeSpecType that governs a given
// spec: the explicit spec.Type when set, else the type detected from
// spec.Binary -- except that a binary detected as sglang whose args select the
// omni serve backend (sglangArgsSelectOmni) is sglang_omni.
func EffectiveRuntimeSpecType(spec RuntimeSpec) RuntimeSpecType {
	if spec.Type != "" {
		return RuntimeSpecType(spec.Type)
	}
	detected := DetectRuntimeSpecType(spec.Binary)
	if detected == RuntimeSpecTypeSGLang && sglangArgsSelectOmni(spec.Args) {
		return RuntimeSpecTypeSGLangOmni
	}
	return detected
}

// sglangArgsSelectOmni reports whether args (a spec's Args, an opaque JSON array of
// strings) hold `--model-type omni` or `--model-type=omni`. That is the upstream
// `sglang serve --model-type omni` form: SGLang v0.5.23's serve CLI
// (python/sglang/cli/serve.py, _extract_model_type_override) hands the value of either
// spelling to the serve backend registry, and sglang-omni v0.2.0 registers its server
// as the "omni" backend (pyproject.toml, sglang.serve_backends), which is selected by
// that flag alone -- its auto-detection never picks it. Any occurrence counts, even
// one a later --model-type overrides upstream: reading the spec as sglang_omni turns
// on the stricter media guards, which is the safe side to err on. Args that are no
// JSON array select nothing.
func sglangArgsSelectOmni(args string) bool {
	var list []string
	if json.Unmarshal([]byte(args), &list) != nil {
		return false
	}
	for i, arg := range list {
		if arg == "--model-type=omni" || (arg == "--model-type" && i+1 < len(list) && list[i+1] == "omni") {
			return true
		}
	}
	return false
}

// DeriveProbePaths resolves the metrics and context-probe paths to use for a
// given effective RuntimeSpecType. A non-empty override always wins for its
// field; otherwise the per-type default applies.
func DeriveProbePaths(t RuntimeSpecType, metricsOverride, contextOverride string) (metricsPath, contextPath string) {
	var defaultMetrics, defaultContext string

	switch t {
	case RuntimeSpecTypeVLLM:
		defaultMetrics, defaultContext = defaultPrometheusMetricsPath, "/v1/models"
	case RuntimeSpecTypeLlamaCpp:
		defaultMetrics, defaultContext = defaultPrometheusMetricsPath, "/props"
	case RuntimeSpecTypeTGI:
		// Verified 2026-09-07: text-generation-inference exposes Prometheus
		// metrics at /metrics (huggingface/text-generation-inference docs,
		// https://huggingface.co/docs/text-generation-inference/en/reference/metrics)
		// and serves max_total_tokens (the context-size field) from /info
		// (docs/openapi.json "Info" schema,
		// https://github.com/huggingface/text-generation-inference/blob/main/docs/openapi.json).
		defaultMetrics, defaultContext = defaultPrometheusMetricsPath, "/info"
	case RuntimeSpecTypeOllama:
		// Verified 2026-09-07: Ollama has no native Prometheus-style /metrics
		// endpoint (no metrics endpoint of any kind is documented in
		// ollama/ollama docs/api.md,
		// https://github.com/ollama/ollama/blob/main/docs/api.md) and serves
		// the context length from /api/show under model_info as an
		// architecture-prefixed key (e.g. "llama.context_length"), per the
		// same source's "Show Model Information" section.
		defaultMetrics, defaultContext = "", "/api/show"
	case RuntimeSpecTypeStableDiffusionCpp:
		// Measured against a real sd-server: no Prometheus-style metrics
		// endpoint of any kind, and no /props. Its capability document at
		// /sdcpp/v1/capabilities carries no context-length field either, and
		// an image model has no context window to probe, so there is nothing
		// honest to default either path to.
		defaultMetrics, defaultContext = "", ""
	case RuntimeSpecTypeAudioCpp:
		// A speech model has no context window to probe: audiocpp_server's
		// /v1/models entries carry family/task/mode/loaded/path (probed against
		// v0.9.1), not a context length, and nothing in the audio.cpp design
		// relies on a Prometheus-style /metrics scrape, so there is nothing
		// honest to default either path to. An explicit override still wins.
		defaultMetrics, defaultContext = "", ""
	case RuntimeSpecTypeVllmSR:
		// vllm-srun serves Prometheus metrics at /metrics and lists its models,
		// with their limits and the runtime api_version, at /v1/models.
		defaultMetrics, defaultContext = defaultPrometheusMetricsPath, "/v1/models"
	case RuntimeSpecTypeSGLang:
		// SGLang v0.5.23 serves Prometheus metrics at /metrics (started with
		// --enable-metrics) and lists its model, with max_model_len, at
		// /v1/models.
		defaultMetrics, defaultContext = defaultPrometheusMetricsPath, "/v1/models"
	case RuntimeSpecTypeSGLangOmni:
		// sglang-omni v0.2.0 has no /metrics, and its /v1/models carries no
		// context length, so there is nothing honest to default either path to.
		defaultMetrics, defaultContext = "", ""
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
