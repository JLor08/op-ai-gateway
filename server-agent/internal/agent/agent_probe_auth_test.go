// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package agent

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"op-ai-server-agent/internal/config"
	"strings"
	"sync"
	"testing"
	"time"

	runtimectl "op-ai-server-agent/internal/runtime"
)

// keyedChild is a stub of a keyed Strata child: everything but /health
// answers 401 unless the request carries header: want; /metrics answers
// Prometheus only for ?format=prometheus.
type keyedChild struct {
	srv    *httptest.Server
	mu     sync.Mutex
	header string
	want   string
	seen   map[string][]string // path -> credential header values received
}

func newKeyedChild(t *testing.T, header, want string) *keyedChild {
	t.Helper()
	c := &keyedChild{header: header, want: want, seen: map[string][]string{}}
	c.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		c.seen[r.URL.Path] = append(c.seen[r.URL.Path], r.Header.Get(c.header))
		c.mu.Unlock()
		if r.URL.Path != "/health" && r.Header.Get(c.header) != c.want {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		switch {
		case r.URL.Path == "/metrics" && r.URL.Query().Get("format") == "prometheus":
			_, _ = w.Write([]byte("llamacpp:requests_processing 2\nllamacpp:requests_deferred 1\n"))
		case r.URL.Path == "/props":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"default_generation_settings":{"n_ctx":65536,"params":{"timings_per_token":true}},"modalities":{"vision":true,"audio":false}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(c.srv.Close)
	return c
}

func (c *keyedChild) received() map[string][]string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string][]string, len(c.seen))
	for k, v := range c.seen {
		out[k] = append([]string(nil), v...)
	}
	return out
}

func strataStatus(t *testing.T, c *keyedChild, token, header string) runtimectl.Status {
	t.Helper()
	return runtimectl.Status{
		SpecID:           "rspec_strata_keyed",
		Model:            "qwen-strata",
		State:            runtimectl.StateRunning,
		PID:              9101,
		Port:             portFromURL(t, c.srv.URL),
		Type:             "strata",
		MetricsPath:      "/metrics?format=prometheus",
		ContextProbePath: "/props",
		APIToken:         token,
		APITokenHeader:   header,
	}
}

func collectKeyed(t *testing.T, st runtimectl.Status) (*bytes.Buffer, *capturePoster) {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	drv := newFakeRuntimeDriver()
	drv.setActive(true)
	drv.setStatuses([]runtimectl.Status{st})
	poster := &capturePoster{}
	a := NewFromDeps(config.Config{Interval: time.Hour}, Deps{Poster: poster, RuntimeDriver: drv})
	a.collectOnce(context.Background())
	return &buf, poster
}

// TestProbeRuntimeChildSendsTheSpecTokenAsBearer pins that the agent's own
// probes of a child whose spec carries an api_token -- metrics scrape,
// context probe, capability probe -- send it as "Authorization: Bearer" when
// the spec names no custom header, and that the token stays out of the logs.
func TestProbeRuntimeChildSendsTheSpecTokenAsBearer(t *testing.T) {
	const token = "tok-agent-probe-bearer"
	child := newKeyedChild(t, "Authorization", "Bearer "+token)
	logs, poster := collectKeyed(t, strataStatus(t, child, token, ""))

	got := poster.last()
	if got == nil || len(got.Runtimes) != 1 {
		t.Fatalf("Runtimes = %+v", got)
	}
	rs := got.Runtimes[0]
	if rs.MetricsProbe != "ok" || rs.ActiveRequests != 2 || rs.QueueDepth != 1 {
		t.Errorf("metrics = %q active %d queue %d, want ok 2 1", rs.MetricsProbe, rs.ActiveRequests, rs.QueueDepth)
	}
	if rs.ContextProbe != "ok" || rs.ContextSize != 65536 {
		t.Errorf("context = %q size %d, want ok 65536", rs.ContextProbe, rs.ContextSize)
	}
	if rs.Capabilities == nil || rs.LiveProgressSupport != "supported" {
		t.Errorf("capabilities = %+v live progress %q, want the /props verdicts", rs.Capabilities, rs.LiveProgressSupport)
	}
	for path, vals := range child.received() {
		for _, v := range vals {
			if v != "Bearer "+token {
				t.Errorf("%s received Authorization %q, want the spec's bearer token", path, v)
			}
		}
	}
	if strings.Contains(logs.String(), token) {
		t.Errorf("token in logs: %s", logs.String())
	}
}

// TestProbeRuntimeChildUsesTheCustomHeader pins a spec with a custom token
// header: the raw token goes out under that header, no Authorization.
func TestProbeRuntimeChildUsesTheCustomHeader(t *testing.T) {
	const token = "tok-agent-probe-custom"
	child := newKeyedChild(t, "X-Api-Key", token)
	_, poster := collectKeyed(t, strataStatus(t, child, token, "X-Api-Key"))
	rs := poster.last().Runtimes[0]
	if rs.MetricsProbe != "ok" || rs.ActiveRequests != 2 || rs.ContextProbe != "ok" {
		t.Errorf("metrics %q active %d context %q, want ok 2 ok via X-Api-Key", rs.MetricsProbe, rs.ActiveRequests, rs.ContextProbe)
	}
}

// TestProbeRuntimeChildWithoutTokenSendsNoCredential pins today's behaviour
// for a spec without a token: no credential header at all, so a keyed child
// reads unreachable.
func TestProbeRuntimeChildWithoutTokenSendsNoCredential(t *testing.T) {
	child := newKeyedChild(t, "Authorization", "Bearer x")
	_, poster := collectKeyed(t, strataStatus(t, child, "", "X-Ignored"))
	rs := poster.last().Runtimes[0]
	if rs.MetricsProbe != "unreachable" || rs.ContextProbe != "unreachable" {
		t.Errorf("metrics %q context %q, want unreachable against a keyed child", rs.MetricsProbe, rs.ContextProbe)
	}
	for path, vals := range child.received() {
		for _, v := range vals {
			if v != "" {
				t.Errorf("%s received Authorization %q from a spec without a token", path, v)
			}
		}
	}
}

// TestProbeRuntimeChildWrongTokenNeverLogged pins that a failing
// authenticated probe (the child rejects the token) logs no part of it.
func TestProbeRuntimeChildWrongTokenNeverLogged(t *testing.T) {
	const token = "tok-agent-probe-rejected"
	child := newKeyedChild(t, "Authorization", "Bearer another")
	logs, poster := collectKeyed(t, strataStatus(t, child, token, ""))
	rs := poster.last().Runtimes[0]
	if rs.MetricsProbe != "unreachable" {
		t.Fatalf("metrics = %q, want unreachable", rs.MetricsProbe)
	}
	if !strings.Contains(logs.String(), "runtime metrics probe failed") {
		t.Fatalf("expected the debug failure line, got %s", logs.String())
	}
	if strings.Contains(logs.String(), token) {
		t.Errorf("token in logs: %s", logs.String())
	}
}
