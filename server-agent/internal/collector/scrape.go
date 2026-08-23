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

// vLLM-style metric names the scraper maps onto the active/queue counters.
const (
	promRunningMetric = "vllm:num_requests_running"
	promWaitingMetric = "vllm:num_requests_waiting"
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

// Scrape fetches the /metrics body and returns the running/waiting request
// counts (0 when the metric is absent).
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
	return int(m[promRunningMetric]), int(m[promWaitingMetric]), nil
}
