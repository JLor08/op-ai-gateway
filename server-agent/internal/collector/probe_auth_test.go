// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package collector

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// strataPromBody is the Prometheus body Strata's /metrics answers for
// ?format=prometheus: llama.cpp's request gauges plus Strata's own counters.
const strataPromBody = `# HELP llamacpp:requests_processing Number of requests processing.
# TYPE llamacpp:requests_processing gauge
llamacpp:requests_processing 2
# TYPE llamacpp:requests_deferred gauge
llamacpp:requests_deferred 1
llamacpp:prompt_tokens_seconds 30.0
llamacpp:predicted_tokens_seconds 24.1
strata:drafts_offered_total 19
strata:drafts_accepted_total 11
`

// strataChild is a stub of a keyed `strata serve`: every route but /health
// answers 401 unless the request carries header: want. /metrics answers JSON
// unless asked for Prometheus with ?format=prometheus, as Strata does.
type strataChild struct {
	srv    *httptest.Server
	mu     sync.Mutex
	header string // header the stub reads the credential from
	want   string // full expected header value
	seen   []string
}

func newStrataChild(t *testing.T, header, want string) *strataChild {
	t.Helper()
	c := &strataChild{header: header, want: want}
	c.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		c.seen = append(c.seen, r.Header.Get(c.header))
		c.mu.Unlock()
		if r.URL.Path != "/health" && r.Header.Get(c.header) != c.want {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/metrics":
			if r.URL.Query().Get("format") != "prometheus" {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"requests_processing":2}`))
				return
			}
			_, _ = w.Write([]byte(strataPromBody))
		case "/props":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"default_generation_settings":{"n_ctx":32768,"params":{"timings_per_token":true}},"modalities":{"vision":true}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(c.srv.Close)
	return c
}

func (c *strataChild) headers() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.seen...)
}

// captureDebugLogs routes slog to a buffer for the test's duration.
func captureDebugLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// TestPromScraperSendsTheSpecTokenAndKeepsTheQuery pins the Strata metrics
// scrape: the query string of the default path survives URL construction, and
// a scraper built with the spec's credential passes the child's 401 gate.
func TestPromScraperSendsTheSpecTokenAndKeepsTheQuery(t *testing.T) {
	const token = "tok-strata-secret-1"
	child := newStrataChild(t, "Authorization", "Bearer "+token)

	active, queue, err := NewPromScraper(child.srv.URL+"/metrics?format=prometheus", child.srv.Client(), "Authorization", "Bearer "+token).Scrape(context.Background())
	if err != nil {
		t.Fatalf("Scrape: %v", err)
	}
	if active != 2 || queue != 1 {
		t.Errorf("active, queue = %d, %d, want 2, 1 (llamacpp:requests_processing / requests_deferred)", active, queue)
	}
}

// TestPromScraperWithoutCredentialSendsNone pins today's behaviour for an
// empty header: no credential header goes out, so a keyed child answers 401
// and the scrape is an error, not a silent zero.
func TestPromScraperWithoutCredentialSendsNone(t *testing.T) {
	child := newStrataChild(t, "Authorization", "Bearer x")
	_, _, err := NewPromScraper(child.srv.URL+"/metrics?format=prometheus", child.srv.Client(), "", "").Scrape(context.Background())
	if err == nil {
		t.Fatal("Scrape without a credential against a keyed child succeeded, want the 401 as an error")
	}
	for _, h := range child.headers() {
		if h != "" {
			t.Errorf("Authorization = %q sent without a credential, want none", h)
		}
	}
}

// TestPromScraperCustomHeader pins a custom credential header (X-Api-Key)
// carrying the raw token.
func TestPromScraperCustomHeader(t *testing.T) {
	const token = "tok-custom-2"
	child := newStrataChild(t, "X-Api-Key", token)
	active, queue, err := NewPromScraper(child.srv.URL+"/metrics?format=prometheus", child.srv.Client(), "X-Api-Key", token).Scrape(context.Background())
	if err != nil {
		t.Fatalf("Scrape: %v", err)
	}
	if active != 2 || queue != 1 {
		t.Errorf("active, queue = %d, %d, want 2, 1", active, queue)
	}
}

// TestProbesSendTheProbeAuthFromContext pins the /props-shaped probes: a
// context carrying the spec's credential (WithProbeAuth) authenticates the
// context probe and the capability probe; a bare context sends nothing.
func TestProbesSendTheProbeAuthFromContext(t *testing.T) {
	const token = "tok-props-3"
	child := newStrataChild(t, "Authorization", "Bearer "+token)
	ctx := WithProbeAuth(context.Background(), "Authorization", "Bearer "+token)

	n, err := ProbeContext(ctx, child.srv.Client(), child.srv.URL, "llama_cpp", "/props", "")
	if err != nil || n != 32768 {
		t.Fatalf("ProbeContext = %d, %v, want 32768, nil", n, err)
	}
	verdicts, stable := ProbePropsVerdicts(ctx, child.srv.Client(), child.srv.URL)
	if !stable || verdicts.LiveProgress != "supported" {
		t.Errorf("ProbePropsVerdicts = %+v, %v, want supported live progress from an authenticated /props", verdicts, stable)
	}

	if _, err := ProbeContext(context.Background(), child.srv.Client(), child.srv.URL, "llama_cpp", "/props", ""); err == nil {
		t.Error("ProbeContext without a credential passed a keyed child's 401")
	}
	if got := child.headers(); got[len(got)-1] != "" {
		t.Errorf("a bare context sent Authorization = %q, want none", got[len(got)-1])
	}
}

// TestProbeAuthNeverLeaksIntoErrorsOrLogs pins that a failed authenticated
// probe or scrape names neither the token nor the header value in its error.
func TestProbeAuthNeverLeaksIntoErrorsOrLogs(t *testing.T) {
	const token = "tok-never-logged-4"
	logs := captureDebugLogs(t)
	child := newStrataChild(t, "Authorization", "Bearer someone-else")

	_, _, err := NewPromScraper(child.srv.URL+"/metrics?format=prometheus", child.srv.Client(), "Authorization", "Bearer "+token).Scrape(context.Background())
	if err == nil {
		t.Fatal("Scrape with a wrong token succeeded")
	}
	slog.Debug("scrape failed", "err", err)
	ctx := WithProbeAuth(context.Background(), "Authorization", "Bearer "+token)
	_, perr := ProbeContext(ctx, child.srv.Client(), child.srv.URL, "llama_cpp", "/props", "")
	if perr == nil {
		t.Fatal("ProbeContext with a wrong token succeeded")
	}
	slog.Debug("probe failed", "err", perr, "ctx", ctx)
	for _, s := range []string{err.Error(), perr.Error(), logs.String()} {
		if strings.Contains(s, token) {
			t.Errorf("token leaked: %q", s)
		}
	}
}

// TestProbeAuthDoesNotFollowRedirects pins that an authenticated probe does
// not carry the credential across a redirect: the redirect is the answer.
func TestProbeAuthDoesNotFollowRedirects(t *testing.T) {
	var leaked sync.Map
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if v := r.Header.Get("X-Api-Key"); v != "" {
			leaked.Store("x", v)
		}
		_, _ = w.Write([]byte(strataPromBody))
	}))
	defer target.Close()
	src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/metrics", http.StatusFound)
	}))
	defer src.Close()

	if _, _, err := NewPromScraper(src.URL+"/metrics", src.Client(), "X-Api-Key", "tok-5").Scrape(context.Background()); err == nil {
		t.Error("an authenticated scrape followed a redirect, want the 302 as an error")
	}
	ctx := WithProbeAuth(context.Background(), "X-Api-Key", "tok-5")
	if _, err := ProbeContext(ctx, src.Client(), src.URL, "llama_cpp", "/props", ""); err == nil {
		t.Error("an authenticated probe followed a redirect, want the 302 as an error")
	}
	if v, ok := leaked.Load("x"); ok {
		t.Errorf("credential %q reached the redirect target", v)
	}
}
