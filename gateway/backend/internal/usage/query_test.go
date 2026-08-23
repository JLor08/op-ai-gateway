// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package usage

import (
	"encoding/json"
	"testing"
	"time"
)

func TestUsageRowMarshalsEmbeddedEventPlusUserName(t *testing.T) {
	row := Row{
		Event:    Event{ID: "req_1", UserID: "usr_1", Model: "qwen-coder", CreatedAt: time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)},
		UserName: "Ada Admin",
	}
	blob, err := json.Marshal(row)
	if err != nil {
		t.Fatalf("marshal row: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(blob, &m); err != nil {
		t.Fatalf("unmarshal row: %v", err)
	}
	if m["id"] != "req_1" {
		t.Fatalf("embedded id promoted = %v, want req_1", m["id"])
	}
	if m["user_name"] != "Ada Admin" {
		t.Fatalf("user_name = %v, want Ada Admin", m["user_name"])
	}
}

func TestUsageRowOmitsEmptyUserName(t *testing.T) {
	blob, err := json.Marshal(Row{Event: Event{ID: "req_1"}})
	if err != nil {
		t.Fatalf("marshal row: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(blob, &m); err != nil {
		t.Fatalf("unmarshal row: %v", err)
	}
	if _, ok := m["user_name"]; ok {
		t.Fatalf("user_name present on empty owner: %s", blob)
	}
}

func TestUsagePageJSONKeys(t *testing.T) {
	blob, err := json.Marshal(Page{Data: []Row{}, Page: 2, Limit: 50, Total: 60, TotalPages: 2})
	if err != nil {
		t.Fatalf("marshal page: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(blob, &m); err != nil {
		t.Fatalf("unmarshal page: %v", err)
	}
	for _, key := range []string{"data", "page", "limit", "total", "total_pages"} {
		if _, ok := m[key]; !ok {
			t.Fatalf("page missing key %q: %s", key, blob)
		}
	}
}

func TestUsageStatsJSONKeys(t *testing.T) {
	blob, err := json.Marshal(Stats{
		Totals:          StatTotals{TotalRequests: 3, ErrorCount: 1, CachedTokens: 4, InputTokens: 5, OutputTokens: 6},
		PromptPerSecond: Histogram{Bins: []HistogramBin{{X0: 1, X1: 2, Count: 3}}, Min: 1, Max: 2, BinSize: 1, P50: 1, P95: 2, P99: 2},
		TokensPerSecond: Histogram{},
	})
	if err != nil {
		t.Fatalf("marshal stats: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(blob, &m); err != nil {
		t.Fatalf("unmarshal stats: %v", err)
	}
	totals, ok := m["totals"].(map[string]any)
	if !ok {
		t.Fatalf("totals not an object: %s", blob)
	}
	for _, key := range []string{"total_requests", "error_count", "cached_tokens", "input_tokens", "output_tokens"} {
		if _, ok := totals[key]; !ok {
			t.Fatalf("totals missing key %q: %s", key, blob)
		}
	}
	if _, ok := m["prompt_per_second"]; !ok {
		t.Fatalf("stats missing prompt_per_second: %s", blob)
	}
	if _, ok := m["tokens_per_second"]; !ok {
		t.Fatalf("stats missing tokens_per_second: %s", blob)
	}
	hist, ok := m["prompt_per_second"].(map[string]any)
	if !ok {
		t.Fatalf("prompt_per_second not an object: %s", blob)
	}
	for _, key := range []string{"bins", "min", "max", "bin_size", "p50", "p95", "p99"} {
		if _, ok := hist[key]; !ok {
			t.Fatalf("histogram missing key %q: %s", key, blob)
		}
	}
	bins, ok := hist["bins"].([]any)
	if !ok || len(bins) != 1 {
		t.Fatalf("bins = %v, want length 1", hist["bins"])
	}
	bin := bins[0].(map[string]any)
	for _, key := range []string{"x0", "x1", "count"} {
		if _, ok := bin[key]; !ok {
			t.Fatalf("bin missing key %q: %s", key, blob)
		}
	}
}
