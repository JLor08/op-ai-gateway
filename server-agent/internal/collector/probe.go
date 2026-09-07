// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package collector

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
)

// SafeProbePath reports whether p is a safe relative probe path to append to
// the agent's own "http://127.0.0.1:PORT" loopback base: empty (nothing to
// probe), or a single-"/"-rooted path carrying no scheme, no protocol-relative
// "//" authority, and no whitespace/control bytes. It is the agent's
// defense-in-depth SSRF guard, mirroring the portal's safeRelativeProbePath:
// concatenating a value like "@evil:9999/x", "//evil", or "http://evil" onto
// the loopback base would otherwise re-parse to an off-loopback Host and turn
// a local probe into an outbound request. probeRuntimeChild skips a probe
// whose path fails this check, so even a bad path that somehow reached the
// agent never dials off-loopback. Valid paths ("/metrics", "/v1/models",
// "/props", "/api/show") all pass.
func SafeProbePath(p string) bool {
	if p == "" {
		return true
	}
	if !strings.HasPrefix(p, "/") || strings.HasPrefix(p, "//") {
		return false
	}
	if strings.Contains(p, "://") {
		return false
	}
	for i := 0; i < len(p); i++ {
		// Reject every byte at or below ASCII space (control chars, tab, CR,
		// NL, and space itself) and DEL.
		if b := p[i]; b <= 0x20 || b == 0x7f {
			return false
		}
	}
	return true
}

// ProbeContext GETs baseURL+contextPath and extracts the served model's
// context length, per specType's JSON convention. specType is the resolved
// lowercase RuntimeSpecType string ("vllm" | "llama_cpp" | "tgi" | "ollama" |
// "custom" | ""); the collector package does not import the gateway routing
// package, so this is a plain string rather than a shared type.
//
// Extraction rules verified against upstream sources on 2026-09-07:
//
//   - vllm: GET /v1/models -> {"data":[{"max_model_len": N, ...}, ...]}.
//     max_model_len is a top-level field on each ModelCard entry.
//     Source: vllm-project/vllm, vllm/entrypoints/serve/engine/protocol.py
//     (ModelCard.max_model_len),
//     https://github.com/vllm-project/vllm/blob/main/vllm/entrypoints/serve/engine/protocol.py
//
//   - llama_cpp: GET /props -> {"default_generation_settings":{"n_ctx": N,
//     ...}, ...}. n_ctx is NESTED under default_generation_settings, not a
//     top-level field (the naive top-level assumption is wrong).
//     Source: ggml-org/llama.cpp, tools/server/README.md,
//     https://github.com/ggml-org/llama.cpp/blob/master/tools/server/README.md
//
//   - tgi: GET /info -> {"max_total_tokens": N, ...}. max_total_tokens is a
//     top-level field of the Info response.
//     Source: huggingface/text-generation-inference, docs/openapi.json
//     "Info" schema,
//     https://github.com/huggingface/text-generation-inference/blob/main/docs/openapi.json
//
//   - ollama: GET /api/show -> {"model_info":{"<arch>.context_length": N,
//     ...}, ...}. The key is architecture-prefixed (e.g. "llama.context_length",
//     "qwen2.context_length"), so it is matched by suffix, not an exact key.
//     Source: ollama/ollama, docs/api.md "Show Model Information",
//     https://github.com/ollama/ollama/blob/main/docs/api.md
//
//   - "custom", "", and any other value: best-effort — scans the decoded
//     JSON body at any depth for the first key matching n_ctx, max_model_len,
//     or context_length (exactly, or by a ".context_length" suffix to also
//     catch the ollama-style architecture-prefixed key).
//
// Missing or unparseable data returns (0, err); ProbeContext never panics.
//
// client is the HTTP client used to issue the request. The caller (agent.go's
// probeRuntimeChild) passes its private keep-alives-disabled client -- the
// same one used for the metrics scrape -- because a managed child's loopback
// port is an OS-assigned, recyclable ephemeral port: a keep-alive connection
// left open past a child's restart/exit could otherwise be transparently
// reused against a different process later assigned that same port. A nil
// client falls back to http.DefaultClient, for existing/incidental callers
// that have no such concern.
func ProbeContext(ctx context.Context, client *http.Client, baseURL, specType, contextPath string) (int, error) {
	path := strings.TrimSpace(contextPath)
	if path == "" {
		return 0, fmt.Errorf("probe context: no context path configured")
	}
	if client == nil {
		client = http.DefaultClient
	}

	url := strings.TrimRight(strings.TrimSpace(baseURL), "/") + path
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return 0, fmt.Errorf("probe context: upstream status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return 0, err
	}

	var v any
	if err := json.Unmarshal(body, &v); err != nil {
		return 0, fmt.Errorf("probe context: parse response: %w", err)
	}

	n, ok := extractContext(strings.ToLower(strings.TrimSpace(specType)), v)
	if !ok {
		return 0, fmt.Errorf("probe context: no context field found for spec type %q at %s", specType, path)
	}
	return n, nil
}

// extractContext dispatches to the per-specType extraction rule.
func extractContext(specType string, v any) (int, bool) {
	switch specType {
	case "vllm":
		return extractVLLMContext(v)
	case "llama_cpp":
		return extractLlamaCppContext(v)
	case "tgi":
		return extractTGIContext(v)
	case "ollama":
		return extractOllamaContext(v)
	default: // "custom", "", and any unrecognized type.
		return extractBestEffortContext(v)
	}
}

// extractVLLMContext reads data[].max_model_len from a vLLM /v1/models body,
// taking the first entry that carries the field.
func extractVLLMContext(v any) (int, bool) {
	obj, ok := v.(map[string]any)
	if !ok {
		return 0, false
	}
	arr, ok := obj["data"].([]any)
	if !ok {
		return 0, false
	}
	for _, item := range arr {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if n, ok := asInt(m["max_model_len"]); ok {
			return n, true
		}
	}
	return 0, false
}

// extractLlamaCppContext reads default_generation_settings.n_ctx from a
// llama.cpp /props body (falling back to a top-level n_ctx, tolerating a
// future/alternate shape).
func extractLlamaCppContext(v any) (int, bool) {
	obj, ok := v.(map[string]any)
	if !ok {
		return 0, false
	}
	if dgs, ok := obj["default_generation_settings"].(map[string]any); ok {
		if n, ok := asInt(dgs["n_ctx"]); ok {
			return n, true
		}
	}
	if n, ok := asInt(obj["n_ctx"]); ok {
		return n, true
	}
	return 0, false
}

// extractTGIContext reads max_total_tokens from a TGI /info body.
func extractTGIContext(v any) (int, bool) {
	obj, ok := v.(map[string]any)
	if !ok {
		return 0, false
	}
	return asInt(obj["max_total_tokens"])
}

// extractOllamaContext reads the architecture-prefixed "<arch>.context_length"
// key out of model_info in an Ollama /api/show body. Keys are visited in
// sorted order so the result is deterministic if a body ever carried more
// than one (it should not).
func extractOllamaContext(v any) (int, bool) {
	obj, ok := v.(map[string]any)
	if !ok {
		return 0, false
	}
	mi, ok := obj["model_info"].(map[string]any)
	if !ok {
		return 0, false
	}
	keys := make([]string, 0, len(mi))
	for k := range mi {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if strings.HasSuffix(k, ".context_length") {
			if n, ok := asInt(mi[k]); ok {
				return n, true
			}
		}
	}
	return 0, false
}

// bestEffortContextKeys is the fallback key list scanned for a "custom" or
// unrecognized spec type, in priority order.
var bestEffortContextKeys = []string{"n_ctx", "max_model_len", "context_length"}

// extractBestEffortContext scans the decoded JSON body at any depth for the
// first key matching one of bestEffortContextKeys (exactly, or by a
// ".context_length" suffix, to also catch an ollama-style architecture-
// prefixed key such as "llama.context_length").
func extractBestEffortContext(v any) (int, bool) {
	for _, key := range bestEffortContextKeys {
		if n, ok := findKeyOrSuffix(v, key); ok {
			return n, true
		}
	}
	return 0, false
}

// findKeyOrSuffix searches v depth-first for the first key equal to key, or
// (when key is "context_length") ending in ".context_length". Dispatches on
// v's shape; the actual per-shape scan lives in findInMap/findInSlice.
func findKeyOrSuffix(v any, key string) (int, bool) {
	switch t := v.(type) {
	case map[string]any:
		return findInMap(t, key)
	case []any:
		return findInSlice(t, key)
	default:
		return 0, false
	}
}

// findInMap looks for key as an exact top-level key of t first, then (only for
// key == "context_length") as an architecture-prefixed suffix match among t's
// other keys, then descends depth-first into every value of t. Keys are
// visited in sorted order for a deterministic result.
func findInMap(t map[string]any, key string) (int, bool) {
	if raw, ok := t[key]; ok {
		if n, ok := asInt(raw); ok {
			return n, true
		}
	}
	keys := make([]string, 0, len(t))
	for k := range t {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if isContextLengthSuffixKey(key, k) {
			if n, ok := asInt(t[k]); ok {
				return n, true
			}
		}
		if n, ok := findKeyOrSuffix(t[k], key); ok {
			return n, true
		}
	}
	return 0, false
}

// isContextLengthSuffixKey reports whether k is an architecture-prefixed
// "<arch>.context_length" match for the search key (e.g. "llama.context_length"
// matching key "context_length"). Only ever true when key is exactly
// "context_length" and k isn't that exact key itself.
func isContextLengthSuffixKey(key, k string) bool {
	return key == "context_length" && k != key && strings.HasSuffix(k, "."+key)
}

// findInSlice searches each element of t, in order, for key.
func findInSlice(t []any, key string) (int, bool) {
	for _, item := range t {
		if n, ok := findKeyOrSuffix(item, key); ok {
			return n, true
		}
	}
	return 0, false
}

// asInt converts a decoded JSON numeric value (float64 from encoding/json, or
// json.Number/int defensively) to an int. Any other type (including a
// non-numeric string) yields (0, false).
func asInt(v any) (int, bool) {
	switch n := v.(type) {
	case float64:
		return int(n), true
	case int:
		return n, true
	case json.Number:
		i, err := n.Int64()
		if err != nil {
			return 0, false
		}
		return int(i), true
	default:
		return 0, false
	}
}
