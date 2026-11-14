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

func TestScraperRejectsNon2xx(t *testing.T) {
	// A non-2xx reply must be an ERROR, not a successful zero. llama.cpp's
	// router mode answers a bare GET /metrics (no ?model=) with 400 "model name
	// is missing from the request"; that error body parses to no known metric,
	// so without a status check Scrape would return (0, 0, nil) and the caller
	// would record active=0 queue=0 state=ok -- a plausible idle server where
	// there is a configuration error (issue #55). Mirrors ProbeContext's own
	// non-2xx rejection.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"model name is missing from the request"}}`))
	}))
	defer ts.Close()

	active, queue, err := NewScraper(ts.URL, ts.Client()).Scrape(context.Background())
	if err == nil {
		t.Fatalf("Scrape on a 400 reply = (%d, %d, nil), want a non-nil error", active, queue)
	}
}

func TestScraperVLLMSRQueueDepth(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("# HELP vllm_srun_queue_depth q\nvllm_srun_queue_depth 3\n"))
	}))
	defer ts.Close()
	active, queue, err := NewScraper(ts.URL, ts.Client()).Scrape(context.Background())
	if err != nil {
		t.Fatalf("Scrape: %v", err)
	}
	if queue != 3 || active != 0 {
		t.Errorf("active/queue = %d/%d, want 0/3 (no running metric exists for vllm-sr)", active, queue)
	}
}

func scrapeBody(t *testing.T, body string) (int, int) {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	defer ts.Close()
	active, queue, err := NewScraper(ts.URL, ts.Client()).Scrape(context.Background())
	if err != nil {
		t.Fatalf("Scrape: %v", err)
	}
	return active, queue
}

func TestScraperSGLangTakesTheMaxPerDataParallelGroup(t *testing.T) {
	// --tp-size 2 with two data-parallel ranks: one series per rank. tp_rank 1
	// repeats tp_rank 0's numbers and must not double them; dp ranks add up.
	active, queue := scrapeBody(t, `# TYPE sglang:num_queue_reqs gauge
sglang:num_queue_reqs{tp_rank="0",dp_rank="0",pp_rank="0",moe_ep_rank="0"} 3
sglang:num_queue_reqs{tp_rank="1",dp_rank="0",pp_rank="0",moe_ep_rank="0"} 3
sglang:num_queue_reqs{tp_rank="0",dp_rank="1",pp_rank="0",moe_ep_rank="0"} 2
sglang:num_running_reqs{tp_rank="0",dp_rank="0"} 4
sglang:num_running_reqs{tp_rank="1",dp_rank="0"} 4
sglang:num_running_reqs{tp_rank="0",dp_rank="1"} 1
`)
	if queue != 5 || active != 5 {
		t.Errorf("active/queue = %d/%d, want 5/5", active, queue)
	}
}

// TestParsePromTextSGLangGauges pins the rule for the sglang: gauges: the series are
// grouped by dp_rank (a series without the label is a group of its own), each group
// counts its largest value, and the groups add up. Tensor-parallel ranks repeat the
// scheduler's numbers, pipeline stages share one queue, and only data-parallel groups
// (DP attention included) hold disjoint requests.
func TestParsePromTextSGLangGauges(t *testing.T) {
	cases := []struct {
		name string
		body string
		want float64
	}{
		{"plain TP, identical values", `sglang:num_queue_reqs{tp_rank="0"} 3
sglang:num_queue_reqs{tp_rank="1"} 3
`, 3},
		{"two dp groups", `sglang:num_queue_reqs{dp_rank="0",tp_rank="0"} 3
sglang:num_queue_reqs{dp_rank="1",tp_rank="0"} 2
`, 5},
		{"PP stages of one dp group", `sglang:num_queue_reqs{tp_rank="0",pp_rank="0",dp_rank="0"} 4
sglang:num_queue_reqs{tp_rank="0",pp_rank="1",dp_rank="0"} 4
`, 4},
		{"no labels", "sglang:num_queue_reqs 7\n", 7},
		{"the larger rank wins its group", `sglang:num_queue_reqs{tp_rank="0",dp_rank="0"} 1
sglang:num_queue_reqs{tp_rank="1",dp_rank="0"} 6
`, 6},
		{"a series without dp_rank is a group of its own", `sglang:num_queue_reqs{tp_rank="0"} 2
sglang:num_queue_reqs{tp_rank="0",dp_rank="0"} 3
`, 5},
		{"a quoted comma and an escaped quote", `sglang:num_queue_reqs{model_name="a,dp_rank=\"9\"",dp_rank="0"} 3
sglang:num_queue_reqs{model_name="b",dp_rank="0"} 3
`, 3},
		{"a my_dp_rank label is not dp_rank", `sglang:num_queue_reqs{my_dp_rank="1",tp_rank="0"} 3
sglang:num_queue_reqs{my_dp_rank="2",tp_rank="1"} 3
`, 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := parsePromText([]byte(tc.body))[sglangWaitingMetric]; got != tc.want {
				t.Errorf("queue = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestScraperSGLangUnlabelled(t *testing.T) {
	active, queue := scrapeBody(t, "sglang:num_queue_reqs 4\nsglang:num_running_reqs 2\n")
	if queue != 4 || active != 2 {
		t.Errorf("active/queue = %d/%d, want 2/4", active, queue)
	}
}

func TestScraperExistingFamiliesKeepSummingAllLabels(t *testing.T) {
	// The tp_rank rule is for the sglang: names only.
	active, queue := scrapeBody(t, `vllm:num_requests_running{tp_rank="0"} 1
vllm:num_requests_running{tp_rank="1"} 2
vllm:num_requests_waiting{tp_rank="1"} 3
`)
	if active != 3 || queue != 3 {
		t.Errorf("active/queue = %d/%d, want 3/3", active, queue)
	}
	active, queue = scrapeBody(t, "llamacpp:requests_processing 1\nllamacpp:requests_deferred 2\n")
	if active != 1 || queue != 2 {
		t.Errorf("llama.cpp active/queue = %d/%d, want 1/2", active, queue)
	}
	active, queue = scrapeBody(t, "tgi_batch_current_size 6\ntgi_queue_size 7\n")
	if active != 6 || queue != 7 {
		t.Errorf("tgi active/queue = %d/%d, want 6/7", active, queue)
	}
}

func TestPromLabelValue(t *testing.T) {
	for _, tc := range []struct {
		labels, want string
		ok           bool
	}{
		{"", "", false},
		{`{model_name="m"}`, "", false},
		{`{dp_rank="0"}`, "0", true},
		{`{tp_rank="1", dp_rank="12"}`, "12", true},
		{`{my_dp_rank="1",dp_rank="3"}`, "3", true},
		{`{my_dp_rank="1"}`, "", false},
		{`{a="x,dp_rank=\"9\"",dp_rank="2"}`, "2", true},
		{`{a="\\",dp_rank="4"}`, "4", true},
		{`{dp_rank=0}`, "", false},
		{`{dp_rank="5`, "", false},
	} {
		got, ok := promLabelValue(tc.labels, "dp_rank")
		if got != tc.want || ok != tc.ok {
			t.Errorf("promLabelValue(%q) = %q, %v; want %q, %v", tc.labels, got, ok, tc.want, tc.ok)
		}
	}
}
