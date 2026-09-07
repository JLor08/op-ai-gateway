// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package routing

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
