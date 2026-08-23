// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package collector

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestParsePromText(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "metrics.prom"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	m := parsePromText(data)
	if got := m["vllm:num_requests_running"]; got != 3 {
		t.Errorf("num_requests_running = %v, want 3", got)
	}
	if got := m["vllm:num_requests_waiting"]; got != 1 {
		t.Errorf("num_requests_waiting = %v, want 1", got)
	}

	// Comment/blank lines and a malformed line must be skipped without panic.
	messy := []byte("# a comment\n\nvllm:num_requests_running 2\nbroken_line_no_value\nvllm:num_requests_running{x=\"y\"} 5\n")
	m = parsePromText(messy)
	if got := m["vllm:num_requests_running"]; got != 7 {
		t.Errorf("summed num_requests_running = %v, want 7 (2+5)", got)
	}
}

func TestScraperExtractsActiveQueue(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "metrics.prom"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(data)
	}))
	defer ts.Close()

	active, queue, err := NewScraper(ts.URL, ts.Client()).Scrape(context.Background())
	if err != nil {
		t.Fatalf("Scrape: %v", err)
	}
	if active != 3 {
		t.Errorf("active = %d, want 3", active)
	}
	if queue != 1 {
		t.Errorf("queue = %d, want 1", queue)
	}
}
