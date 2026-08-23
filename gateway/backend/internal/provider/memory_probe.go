// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"op-ai-gateway/internal/routing"
	"strconv"
	"strings"
)

// ServerMemory is a single upstream saturation/capacity reading, parsed from a
// llama.cpp-family status endpoint. It is used by the capacity benchmark to know
// when the upstream has begun queueing (near its safe concurrency ceiling) without
// causing an OOM. Every field is optional: a probe reads WHATEVER signals the
// endpoint exposes and is fail-open (a missing/unparseable field is simply absent).
//
// Sentinels: RequestsDeferred / RequestsProcessing are -1 when unknown (0 is a real
// reading — an idle server); TotalSlots is 0 when unknown; KVCacheUsageRatio is 0
// when absent (the metric was removed from mainline llama.cpp, so it is only
// opportunistically present). OK is true iff at least one field was populated.
type ServerMemory struct {
	RequestsDeferred   int     // llamacpp:requests_deferred (queueing began); -1 = unknown
	RequestsProcessing int     // llamacpp:requests_processing / busy slots; -1 = unknown
	TotalSlots         int     // total_slots / len(slots); 0 = unknown
	KVCacheUsageRatio  float64 // llamacpp:kv_cache_usage_ratio (opportunistic); 0 = absent
	OK                 bool
}

// MemoryProber is an optional provider capability: it reads the upstream's
// saturation signal (busy/deferred slots, total slots, KV-cache usage) from a
// configurable status endpoint. It is a read-only probe. A blank probePath returns
// (ServerMemory{}, nil): the feature is off for the app. Tolerant — an
// unparseable body yields a not-OK ServerMemory, never an error; a
// missing/non-2xx endpoint yields an error (a dropped signal, fail-open at the
// caller).
type MemoryProber interface {
	ProbeServerMemory(ctx context.Context, target routing.Target, probePath, format string) (ServerMemory, error)
}

// fetchServerMemory is the shared GET+parse used by every client's
// ProbeServerMemory. It mirrors fetchModelInfo: a bare GET with NO auth header (the
// probe hits the app's own endpoint), bounded body read, non-2xx -> ErrUnavailable.
func fetchServerMemory(ctx context.Context, httpClient *http.Client, target routing.Target, probePath, format string) (ServerMemory, error) {
	probePath = strings.TrimSpace(probePath)
	if probePath == "" {
		return ServerMemory{}, nil
	}
	if !strings.HasPrefix(probePath, "/") {
		probePath = "/" + probePath
	}
	if target.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, target.Timeout)
		defer cancel()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpointURL(target.Endpoint, probePath), nil)
	if err != nil {
		return ServerMemory{}, fmt.Errorf("%w: create request: %v", ErrUnavailable, err)
	}
	applyUpstreamAuth(ctx, req)
	resp, err := httpClient.Do(req)
	if err != nil {
		return ServerMemory{}, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return ServerMemory{}, fmt.Errorf("%w: upstream status %d", ErrUnavailable, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return ServerMemory{}, fmt.Errorf("%w: read response: %v", ErrUnavailable, err)
	}
	return parseServerMemory(body, format), nil
}

// parseServerMemory extracts the upstream saturation signal from a status body. It
// NEVER errors: an unparseable body yields a not-OK ServerMemory. RequestsDeferred
// and RequestsProcessing start at -1 (unknown) and are only set to a real value
// when parsed.
//   - "prometheus" / "metrics" : llama.cpp /metrics Prometheus text.
//   - "json" / "props" / "slots": llama.cpp /props (object -> total_slots) or
//     /slots (array -> count is_processing + len).
//   - "" / "auto" / anything else: try Prometheus first (a JSON body carries no
//     llamacpp: lines, so it yields nothing), then JSON; the first that finds a
//     value wins.
func parseServerMemory(body []byte, format string) ServerMemory {
	m := ServerMemory{RequestsDeferred: -1, RequestsProcessing: -1}
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "prometheus", "metrics":
		parsePrometheusMemory(body, &m)
	case "json", "props", "slots":
		parseJSONMemory(body, &m)
	default: // "" / "auto" / unknown -> Prometheus then JSON; first to find a value wins.
		parsePrometheusMemory(body, &m)
		if !m.OK {
			parseJSONMemory(body, &m)
		}
	}
	return m
}

// parsePrometheusMemory scans a Prometheus text body for the llama.cpp saturation
// metrics. For each metric line (non-blank, not starting with '#') it strips any
// {labels} from the name and takes the numeric token after the last space. This
// line-parse is DUPLICATED locally on purpose (the provider imports nothing from
// the server-agent).
func parsePrometheusMemory(body []byte, m *ServerMemory) {
	for _, raw := range strings.Split(string(body), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		name := fields[0]
		if i := strings.IndexByte(name, '{'); i >= 0 {
			name = name[:i]
		}
		valStr := fields[len(fields)-1]
		v, err := strconv.ParseFloat(valStr, 64)
		if err != nil {
			continue
		}
		switch name {
		case "llamacpp:requests_deferred":
			m.RequestsDeferred = int(v)
			m.OK = true
		case "llamacpp:requests_processing":
			m.RequestsProcessing = int(v)
			m.OK = true
		case "llamacpp:kv_cache_usage_ratio":
			m.KVCacheUsageRatio = v
			m.OK = true
		}
	}
}

// parseJSONMemory reads a JSON status body: an object carrying total_slots (/props)
// or a bare array of slot objects (/slots, count is_processing + len). Anything
// else leaves m untouched (not OK).
func parseJSONMemory(body []byte, m *ServerMemory) {
	var v any
	if err := json.Unmarshal(body, &v); err != nil {
		return
	}
	switch typed := v.(type) {
	case map[string]any:
		if n, ok := numFromAny(typed["total_slots"]); ok && n > 0 {
			m.TotalSlots = int(n)
			m.OK = true
		}
	case []any:
		processing, total := countProcessingSlots(typed)
		m.RequestsProcessing = processing
		m.TotalSlots = total
		m.OK = true
	}
}

// countProcessingSlots counts slots whose is_processing is true and returns that
// count plus the total number of slots (len of the array).
func countProcessingSlots(arr []any) (processing, total int) {
	total = len(arr)
	for _, item := range arr {
		obj, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if b, ok := obj["is_processing"].(bool); ok && b {
			processing++
		}
	}
	return processing, total
}

// numFromAny tolerantly coerces a JSON number (float64 / json.Number) or an int to
// a float64; anything else -> (0, false).
func numFromAny(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case json.Number:
		if f, err := n.Float64(); err == nil {
			return f, true
		}
	case int:
		return float64(n), true
	}
	return 0, false
}
