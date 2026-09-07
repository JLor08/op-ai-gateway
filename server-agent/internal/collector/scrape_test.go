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

func TestScraperAutoDetectsLlamaCpp(t *testing.T) {
	// llama.cpp (`llama-server --metrics`) exposes llamacpp:-prefixed counters
	// under different names than vLLM; the scraper must map them onto the same
	// active/queue counters with no configuration.
	body := []byte(
		"# HELP llamacpp:requests_processing Number of requests processing.\n" +
			"# TYPE llamacpp:requests_processing gauge\n" +
			"llamacpp:requests_processing 4\n" +
			"# TYPE llamacpp:requests_deferred gauge\n" +
			"llamacpp:requests_deferred 2\n" +
			"llamacpp:kv_cache_usage_ratio 0.5\n")
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write(body)
	}))
	defer ts.Close()

	active, queue, err := NewScraper(ts.URL, ts.Client()).Scrape(context.Background())
	if err != nil {
		t.Fatalf("Scrape: %v", err)
	}
	if active != 4 {
		t.Errorf("active = %d, want 4 (llamacpp:requests_processing)", active)
	}
	if queue != 2 {
		t.Errorf("queue = %d, want 2 (llamacpp:requests_deferred)", queue)
	}
}

func TestScraperAutoDetectsTGI(t *testing.T) {
	// TGI (text-generation-inference) exposes tgi_-prefixed gauges; verified at
	// https://huggingface.co/docs/text-generation-inference/en/reference/metrics
	// (tgi_batch_current_size = current batch size, tgi_queue_size = current
	// queue size).
	body := []byte(
		"# HELP tgi_batch_current_size Current batch size.\n" +
			"# TYPE tgi_batch_current_size gauge\n" +
			"tgi_batch_current_size 5\n" +
			"# HELP tgi_queue_size Current queue size.\n" +
			"# TYPE tgi_queue_size gauge\n" +
			"tgi_queue_size 3\n")
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write(body)
	}))
	defer ts.Close()

	active, queue, err := NewScraper(ts.URL, ts.Client()).Scrape(context.Background())
	if err != nil {
		t.Fatalf("Scrape: %v", err)
	}
	if active != 5 {
		t.Errorf("active = %d, want 5 (tgi_batch_current_size)", active)
	}
	if queue != 3 {
		t.Errorf("queue = %d, want 3 (tgi_queue_size)", queue)
	}
}

func TestScraperUnknownFormatYieldsZero(t *testing.T) {
	// A /metrics body carrying neither family's counters -> 0/0, no error.
	body := []byte("# TYPE other_metric gauge\nother_metric 9\n")
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write(body)
	}))
	defer ts.Close()

	active, queue, err := NewScraper(ts.URL, ts.Client()).Scrape(context.Background())
	if err != nil {
		t.Fatalf("Scrape: %v", err)
	}
	if active != 0 || queue != 0 {
		t.Errorf("active,queue = %d,%d, want 0,0 for an unrecognized metrics format", active, queue)
	}
}
