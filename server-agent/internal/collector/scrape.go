// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package collector

import (
	"context"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// The request-counter metric names the scraper maps onto the active/queue
// counters, per inference-server family. A /metrics endpoint carries one family
// or the other, never both, so Scrape auto-detects: for each counter it takes
// whichever name is present. active = running/processing; queue = waiting/deferred.
const (
	vllmRunningMetric     = "vllm:num_requests_running"    // vLLM
	vllmWaitingMetric     = "vllm:num_requests_waiting"    // vLLM
	llamacppRunningMetric = "llamacpp:requests_processing" // llama.cpp (llama-server --metrics)
	llamacppWaitingMetric = "llamacpp:requests_deferred"   // llama.cpp
)

// parsePromText parses a Prometheus text-exposition body into a map summing the
// values of each metric name (labels ignored). Comment (`#`) and blank lines are
// skipped, and any line that does not end in a parseable float is skipped
// without error.
func parsePromText(data []byte) map[string]float64 {
	out := make(map[string]float64)
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// The value is the token after the last space; everything before it is
		// the metric name plus optional labels.
		idx := strings.LastIndexByte(line, ' ')
		if idx < 0 {
			continue
		}
		v, err := strconv.ParseFloat(strings.TrimSpace(line[idx+1:]), 64)
		if err != nil {
			continue
		}
		name := strings.TrimSpace(line[:idx])
		if brace := strings.IndexByte(name, '{'); brace >= 0 {
			name = name[:brace]
		}
		if name == "" {
			continue
		}
		out[name] += v
	}
	return out
}

// promScraper scrapes an inference server's Prometheus /metrics endpoint.
type promScraper struct {
	url    string
	client *http.Client
}

// NewScraper returns a Scraper that GETs url. A nil client defaults to
// http.DefaultClient.
func NewScraper(url string, client *http.Client) Scraper {
	if client == nil {
		client = http.DefaultClient
	}
	return &promScraper{url: url, client: client}
}

// Scrape fetches the /metrics body and returns the active/queued request counts
// (0 when neither family's metric is present). It auto-detects the inference
// server: for each counter it takes the vLLM name if present, else the llama.cpp
// name.
func (s *promScraper) Scrape(ctx context.Context) (active int, queue int, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.url, nil)
	if err != nil {
		return 0, 0, err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return 0, 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, 0, err
	}
	m := parsePromText(body)
	active = int(firstPresent(m, vllmRunningMetric, llamacppRunningMetric))
	queue = int(firstPresent(m, vllmWaitingMetric, llamacppWaitingMetric))
	return active, queue, nil
}

// firstPresent returns the value of the first metric name that is PRESENT in m
// (0 when none are). Presence, not value, decides — a server legitimately
// reporting 0 keeps that 0 rather than falling through to another family's
// counter.
func firstPresent(m map[string]float64, names ...string) float64 {
	for _, n := range names {
		if v, ok := m[n]; ok {
			return v
		}
	}
	return 0
}
