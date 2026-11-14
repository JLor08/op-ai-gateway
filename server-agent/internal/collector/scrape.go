// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package collector

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// The request-counter metric names the scraper maps onto the active/queue
// counters, per inference-server family. A /metrics endpoint carries one family
// or the other, never both, so Scrape auto-detects: for each counter it takes
// whichever name is present. active = running/processing; queue = waiting/deferred.
//
// tgi (text-generation-inference) names verified 2026-09-07 against
// https://huggingface.co/docs/text-generation-inference/en/reference/metrics:
// tgi_batch_current_size (Gauge, "Current batch size") and tgi_queue_size
// (Gauge, "Current queue size"); there is no tgi_*_running/waiting name.
//
// Ollama exposes NO Prometheus /metrics endpoint at all (verified 2026-09-07:
// no metrics endpoint of any kind is documented in
// https://github.com/ollama/ollama/blob/main/docs/api.md), so there is no
// ollama*Metric name to add here — DeriveProbePaths correctly leaves Ollama's
// metrics path empty.
const (
	vllmRunningMetric     = "vllm:num_requests_running"    // vLLM
	vllmWaitingMetric     = "vllm:num_requests_waiting"    // vLLM
	llamacppRunningMetric = "llamacpp:requests_processing" // llama.cpp (llama-server --metrics)
	llamacppWaitingMetric = "llamacpp:requests_deferred"   // llama.cpp
	tgiRunningMetric      = "tgi_batch_current_size"       // TGI
	tgiWaitingMetric      = "tgi_queue_size"               // TGI
	// vllm-sr (vLLM Semantic Router worker) exports only a queue-depth gauge;
	// no running/in-flight metric exists for it, so active stays 0.
	vllmSRWaitingMetric = "vllm_srun_queue_depth"
	// SGLang (v0.5.23, python/sglang/srt/observability/metrics_collector.py)
	// gauges. The scheduler emits one series per rank, labelled tp_rank,
	// dp_rank, pp_rank and moe_ep_rank; see parsePromText for how they add up.
	sglangRunningMetric = "sglang:num_running_reqs"
	sglangWaitingMetric = "sglang:num_queue_reqs"
)

// parsePromText parses a Prometheus text-exposition body into a map summing the
// values of each metric name (labels ignored). Comment (`#`) and blank lines are
// skipped, and any line that does not end in a parseable float is skipped
// without error.
//
// The sglang: gauges are the exception (addSGLangGauge): their series are grouped by
// dp_rank, each group counts its largest value, and the groups are summed.
func parsePromText(data []byte) map[string]float64 {
	out := make(map[string]float64)
	sglang := make(map[string]map[string]float64)
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
		labels := ""
		if brace := strings.IndexByte(name, '{'); brace >= 0 {
			labels = name[brace:]
			name = name[:brace]
		}
		if name == "" {
			continue
		}
		if strings.HasPrefix(name, "sglang:") {
			addSGLangGauge(sglang, name, labels, v)
			continue
		}
		out[name] += v
	}
	for name, groups := range sglang {
		for _, v := range groups {
			out[name] += v
		}
	}
	return out
}

// addSGLangGauge records one series of an sglang: gauge in groups (name, then dp_rank
// value, at the largest value seen). SGLang's scheduler emits one series per rank: the
// tensor-parallel ranks of a group repeat its numbers, and the pipeline stages of a
// group share its queue, so a group counts once, at its largest value; the
// data-parallel groups (DP attention included) hold disjoint requests, so they add up.
// A series without a dp_rank label is one group of its own.
func addSGLangGauge(groups map[string]map[string]float64, name, labels string, v float64) {
	dp, _ := promLabelValue(labels, "dp_rank")
	byGroup := groups[name]
	if byGroup == nil {
		byGroup = make(map[string]float64)
		groups[name] = byGroup
	}
	if cur, seen := byGroup[dp]; !seen || v > cur {
		byGroup[dp] = v
	}
}

// promLabelValue returns the value of the label key in a Prometheus label block
// (`{a="b",c="d"}`, or "") and whether the block carries it. The block is read pair by
// pair, unescaping each quoted value, so neither a label whose name only ends in key
// (my_dp_rank) nor a quoted value that contains key="..." is taken for it. A block that
// does not parse carries no label.
func promLabelValue(labels, key string) (string, bool) {
	rest, ok := strings.CutPrefix(strings.TrimSpace(labels), "{")
	if !ok {
		return "", false
	}
	for {
		rest = strings.TrimLeft(rest, " \t")
		eq := strings.IndexByte(rest, '=')
		if eq < 0 {
			return "", false
		}
		name := strings.TrimSpace(rest[:eq])
		value, tail, ok := promQuotedValue(strings.TrimLeft(rest[eq+1:], " \t"))
		if !ok {
			return "", false
		}
		if name == key {
			return value, true
		}
		rest = strings.TrimLeft(tail, " \t")
		if next, more := strings.CutPrefix(rest, ","); more {
			rest = next
			continue
		}
		return "", false
	}
}

// promQuotedValue reads one double-quoted label value off the front of s, undoing the
// exposition format's three escapes (\\, \" and \n), and returns it with what follows the
// closing quote.
func promQuotedValue(s string) (value, tail string, ok bool) {
	body, ok := strings.CutPrefix(s, `"`)
	if !ok {
		return "", "", false
	}
	var b strings.Builder
	for i := 0; i < len(body); i++ {
		switch c := body[i]; {
		case c == '"':
			return b.String(), body[i+1:], true
		case c == '\\' && i+1 < len(body):
			i++
			if body[i] == 'n' {
				b.WriteByte('\n')
			} else {
				b.WriteByte(body[i])
			}
		default:
			b.WriteByte(c)
		}
	}
	return "", "", false
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
// (0 when no known family's metric is present). It auto-detects the inference
// server: for each counter it takes the first present name, trying vLLM, then
// llama.cpp, then TGI. Ollama exposes no /metrics endpoint to auto-detect.
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
	// A non-2xx reply is an error, not a successful zero. Without this a router-
	// mode llama.cpp server -- which answers a bare GET /metrics (no ?model=)
	// with 400 -- would parse to no known metric family and be recorded as an
	// idle active=0/queue=0 with state "ok", hiding the misconfiguration (issue
	// #55). Mirrors ProbeContext's own non-2xx rejection.
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return 0, 0, fmt.Errorf("metrics scrape: upstream status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, 0, err
	}
	m := parsePromText(body)
	active = int(firstPresent(m, vllmRunningMetric, llamacppRunningMetric, tgiRunningMetric, sglangRunningMetric))
	queue = int(firstPresent(m, vllmWaitingMetric, llamacppWaitingMetric, tgiWaitingMetric, vllmSRWaitingMetric, sglangWaitingMetric))
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
