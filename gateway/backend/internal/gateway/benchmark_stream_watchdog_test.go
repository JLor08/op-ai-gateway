// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"op-ai-gateway/internal/inference"
	"op-ai-gateway/internal/provider"
	"op-ai-gateway/internal/routing"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The watchdog tests drive a real provider.OpenAICompatibleClient against an
// httptest SSE upstream, so an SSE comment line takes the same path as the
// agent router's `: keepalive` (completeStreamAttempt -> the stream activity
// hook). Idle budgets are 100 ms against a 10 ms keepalive cadence, and every
// timing assertion keeps a margin of several idle budgets.
const (
	wdIdle      = 100 * time.Millisecond
	wdKeepalive = 10 * time.Millisecond
	// wdSafety bounds every watchdog test from outside, so a broken watchdog
	// fails a test instead of hanging it. It is a parent deadline, so its
	// error is the provider's own and never a watchdog text.
	wdSafety = 5 * time.Second
)

// wdIdleText and wdFirstDataText are the watchdog's two error texts, pinned
// exactly.
func wdIdleText(idle time.Duration) string {
	return fmt.Sprintf("provider.timeout: benchmark stream: no data for %s (OP_AI_GATEWAY_STREAM_IDLE_TIMEOUT)", idle)
}

func wdFirstDataText(budget time.Duration) string {
	return fmt.Sprintf("provider.timeout: benchmark stream: no first data within %s although the upstream kept the connection alive (max of the application's timeout_ms and the idle budget)", budget)
}

// sseScript is one scripted upstream response. Every write flushes, and every
// method reports false once the gateway has gone away, so a script ends with
// the stream.
type sseScript struct {
	w   http.ResponseWriter
	f   http.Flusher
	ctx context.Context
}

func (s sseScript) write(frame string) bool {
	if s.ctx.Err() != nil {
		return false
	}
	if _, err := fmt.Fprint(s.w, frame); err != nil {
		return false
	}
	s.f.Flush()
	return true
}

func (s sseScript) keepalive() bool { return s.write(": keepalive\n\n") }

func (s sseScript) delta(text string) bool {
	return s.write(fmt.Sprintf("data: {\"choices\":[{\"delta\":{\"content\":%q}}]}\n\n", text))
}

func (s sseScript) done() {
	s.write("data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1,\"total_tokens\":2}}\n\n")
	s.write("data: [DONE]\n\n")
}

// wait sleeps d unless the gateway goes away first.
func (s sseScript) wait(d time.Duration) bool {
	select {
	case <-s.ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

// keepalivesFor sends a keepalive every wdKeepalive for d.
func (s sseScript) keepalivesFor(d time.Duration) bool {
	end := time.Now().Add(d)
	for time.Now().Before(end) {
		if !s.keepalive() || !s.wait(wdKeepalive) {
			return false
		}
	}
	return true
}

// keepalivesForever sends a keepalive every wdKeepalive until the gateway goes
// away.
func (s sseScript) keepalivesForever() {
	for {
		if !s.keepalive() || !s.wait(wdKeepalive) {
			return
		}
	}
}

// newSSEUpstream serves /v1/chat/completions as a 200 text/event-stream whose
// body script writes. The headers are flushed before the script runs.
func newSSEUpstream(t *testing.T, script func(s sseScript)) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		f, ok := w.(http.Flusher)
		if !ok {
			t.Errorf("response writer is not a flusher")
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		f.Flush()
		script(sseScript{w: w, f: f, ctx: r.Context()})
	}))
	t.Cleanup(ts.Close)
	return ts
}

// wdTarget is a routing target for ts with the given application timeout.
func wdTarget(ts *httptest.Server, timeout time.Duration) routing.Target {
	return routing.Target{Provider: routing.ProviderMock, Endpoint: ts.URL, Model: "m", ProviderModel: "m", Timeout: timeout}
}

func wdRequest() inference.Request {
	return inference.Request{
		Model:     "m",
		MaxTokens: 1,
		Stream:    true,
		Messages:  []inference.Message{{Role: inference.RoleUser, Content: []inference.ContentPart{{Type: inference.ContentText, Text: "hi"}}}},
	}
}

// wdServer is a Server whose provider is a real OpenAI-compatible client and
// whose stream idle budget is idle.
func wdServer(idle time.Duration) (*Server, provider.StreamingClient) {
	client := provider.NewOpenAICompatibleClient(http.DefaultClient)
	srv := &Server{Provider: client}
	srv.streamIdleTimeout = idle
	return srv, client
}

// wdStreamOnce runs streamOnce under the wdSafety parent deadline and reports
// its time to first token and how long it took.
func wdStreamOnce(t *testing.T, srv *Server, streamer provider.StreamingClient, target routing.Target) (ttft, elapsed time.Duration, err error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), wdSafety)
	defer cancel()
	start := time.Now()
	ttft, _, err = srv.streamOnce(ctx, streamer, target, wdRequest())
	return ttft, time.Since(start), err
}

// TestBenchmarkStreamWatchdog covers the benchmark stream watchdog end to end
// through streamOnce and streamCollect.
func TestBenchmarkStreamWatchdog(t *testing.T) {
	t.Run("keepalives then data pass with a small idle and a large budget", func(t *testing.T) {
		ts := newSSEUpstream(t, func(s sseScript) {
			if s.keepalivesFor(3 * wdIdle) {
				s.delta("hello")
				s.done()
			}
		})
		srv, client := wdServer(wdIdle)
		ttft, _, err := wdStreamOnce(t, srv, client, wdTarget(ts, 2*time.Second))
		if err != nil {
			t.Fatalf("streamOnce err = %v, want nil (keepalives before the first data reset the idle timer)", err)
		}
		if ttft < 2*wdIdle {
			t.Fatalf("ttft = %v, want >= %v (the first data came after the keepalive phase)", ttft, 2*wdIdle)
		}
	})

	t.Run("keepalives forever end at the budget", func(t *testing.T) {
		ts := newSSEUpstream(t, func(s sseScript) { s.keepalivesForever() })
		srv, client := wdServer(wdIdle)
		budget := 500 * time.Millisecond
		_, elapsed, err := wdStreamOnce(t, srv, client, wdTarget(ts, budget))
		if err == nil || err.Error() != wdFirstDataText(budget) {
			t.Fatalf("streamOnce err = %v, want %q", err, wdFirstDataText(budget))
		}
		if !errors.Is(err, provider.ErrTimeout) {
			t.Fatalf("err = %v, want a wrapped provider.ErrTimeout", err)
		}
		if elapsed < budget-wdKeepalive || elapsed > 3*time.Second {
			t.Fatalf("elapsed = %v, want about the %v budget", elapsed, budget)
		}
	})

	t.Run("with Timeout 0 keepalives earn nothing and the stream ends at idle", func(t *testing.T) {
		ts := newSSEUpstream(t, func(s sseScript) { s.keepalivesForever() })
		srv, client := wdServer(wdIdle)
		_, elapsed, err := wdStreamOnce(t, srv, client, wdTarget(ts, 0))
		if err == nil || err.Error() != wdIdleText(wdIdle) {
			t.Fatalf("streamOnce err = %v, want %q", err, wdIdleText(wdIdle))
		}
		if elapsed > 2*time.Second {
			t.Fatalf("elapsed = %v, want about the %v idle budget", elapsed, wdIdle)
		}
	})

	t.Run("a silent stream with a budget above idle ends at idle", func(t *testing.T) {
		ts := newSSEUpstream(t, func(s sseScript) { <-s.ctx.Done() })
		srv, client := wdServer(wdIdle)
		_, elapsed, err := wdStreamOnce(t, srv, client, wdTarget(ts, 3*time.Second))
		if err == nil || err.Error() != wdIdleText(wdIdle) {
			t.Fatalf("streamOnce err = %v, want %q", err, wdIdleText(wdIdle))
		}
		if elapsed > 2*time.Second {
			t.Fatalf("elapsed = %v, want about the %v idle budget, well before the 3s budget", elapsed, wdIdle)
		}
	})

	t.Run("data keeps flowing past the budget with no cap", func(t *testing.T) {
		idle := 150 * time.Millisecond
		budget := 300 * time.Millisecond
		ts := newSSEUpstream(t, func(s sseScript) {
			end := time.Now().Add(3 * budget)
			for time.Now().Before(end) {
				if !s.delta("x") || !s.wait(20*time.Millisecond) {
					return
				}
			}
			s.done()
		})
		srv, client := wdServer(idle)
		_, elapsed, err := wdStreamOnce(t, srv, client, wdTarget(ts, budget))
		if err != nil {
			t.Fatalf("streamOnce err = %v, want nil (the first data stops the first-data timer)", err)
		}
		if elapsed < 2*budget {
			t.Fatalf("elapsed = %v, want >= %v (the stream outlived its first-data budget)", elapsed, 2*budget)
		}
	})

	t.Run("data then comments only end at idle", func(t *testing.T) {
		ts := newSSEUpstream(t, func(s sseScript) {
			if s.delta("x") {
				s.keepalivesForever()
			}
		})
		srv, client := wdServer(wdIdle)
		_, elapsed, err := wdStreamOnce(t, srv, client, wdTarget(ts, 3*time.Second))
		if err == nil || err.Error() != wdIdleText(wdIdle) {
			t.Fatalf("streamOnce err = %v, want %q", err, wdIdleText(wdIdle))
		}
		if elapsed > 2*time.Second {
			t.Fatalf("elapsed = %v, want about the %v idle budget (comments after the first data earn nothing)", elapsed, wdIdle)
		}
	})

	t.Run("streamCollect: keepalives then data pass", func(t *testing.T) {
		ts := newSSEUpstream(t, func(s sseScript) {
			if s.keepalivesFor(3 * wdIdle) {
				s.delta("hello")
				s.done()
			}
		})
		srv, client := wdServer(wdIdle)
		ctx, cancel := context.WithTimeout(context.Background(), wdSafety)
		defer cancel()
		got, err := srv.streamCollect(ctx, client, wdTarget(ts, 2*time.Second), wdRequest())
		if err != nil {
			t.Fatalf("streamCollect err = %v, want nil", err)
		}
		if got != "hello" {
			t.Fatalf("streamCollect = %q, want %q", got, "hello")
		}
	})
}

// TestBenchmarkStreamWatchdogPassesParentCancellationThrough: only the
// watchdog's own two causes become its texts. A parent context's cancellation
// or deadline -- the capacity level's deadline, the model warmer's 60 s
// context -- comes back exactly as the provider returned it.
func TestBenchmarkStreamWatchdogPassesParentCancellationThrough(t *testing.T) {
	t.Run("a parent cancellation through the real client", func(t *testing.T) {
		ts := newSSEUpstream(t, func(s sseScript) { s.keepalivesForever() })
		srv, client := wdServer(time.Second)
		ctx, cancel := context.WithCancel(context.Background())
		time.AfterFunc(50*time.Millisecond, cancel)
		_, _, err := srv.streamOnce(ctx, client, wdTarget(ts, 3*time.Second), wdRequest())
		if err == nil || errors.Is(err, provider.ErrTimeout) || strings.Contains(err.Error(), "benchmark stream") {
			t.Fatalf("err = %v, want the provider's own error, not a watchdog timeout", err)
		}
		if !errors.Is(err, provider.ErrUnavailable) {
			t.Fatalf("err = %v, want the provider's own provider.ErrUnavailable", err)
		}
	})

	t.Run("a parent deadline through the real client", func(t *testing.T) {
		ts := newSSEUpstream(t, func(s sseScript) { <-s.ctx.Done() })
		srv, client := wdServer(time.Second)
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		_, _, err := srv.streamOnce(ctx, client, wdTarget(ts, 3*time.Second), wdRequest())
		if err == nil || err.Error() != provider.ErrTimeout.Error() {
			t.Fatalf("err = %v, want the provider's own bare %q", err, provider.ErrTimeout)
		}
	})

	t.Run("a parent cancellation keeps its context.Canceled", func(t *testing.T) {
		srv := &Server{Provider: benchHangingProvider{}}
		srv.streamIdleTimeout = time.Second
		ctx, cancel := context.WithCancel(context.Background())
		time.AfterFunc(50*time.Millisecond, cancel)
		_, _, err := srv.streamOnce(ctx, benchHangingProvider{}, routing.Target{Provider: routing.ProviderMock, Model: "m", Timeout: 3 * time.Second}, wdRequest())
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled passed through", err)
		}
		if errors.Is(err, provider.ErrTimeout) {
			t.Fatalf("err = %v, a parent cancellation must not become a watchdog timeout", err)
		}
	})
}

// sseBenchTarget is benchTestTarget pointed at ts, with the given timeout_ms.
func sseBenchTarget(t *testing.T, ts *httptest.Server, timeoutMS int) benchmarkTarget {
	t.Helper()
	u, err := url.Parse(ts.URL)
	if err != nil {
		t.Fatalf("parse %q: %v", ts.URL, err)
	}
	host, portText, err := net.SplitHostPort(u.Host)
	if err != nil {
		t.Fatalf("split %q: %v", u.Host, err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatalf("port %q: %v", portText, err)
	}
	tgt := benchTestTarget()
	tgt.server.Domain = host
	tgt.app.Port = port
	tgt.app.TimeoutMS = timeoutMS
	return tgt
}

// TestRunLoadModelThroughAColdStartWithKeepalives: a Load whose upstream
// keeps the connection alive longer than the idle budget before its first
// data -- the agent router starting a cold child -- is recorded as loaded, and
// one whose upstream never sends data fails with the first-data text.
func TestRunLoadModelThroughAColdStartWithKeepalives(t *testing.T) {
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	run := func(t *testing.T, ts *httptest.Server, timeoutMS int) BenchmarkResult {
		t.Helper()
		srv, _ := wdServer(wdIdle)
		srv.Benchmarks = NewBenchmarkRegistry()
		br, ok := srv.Benchmarks.TryStart("srv1", "load", "load", 1, now, func() {})
		if !ok {
			t.Fatalf("TryStart did not start")
		}
		tgt := sseBenchTarget(t, ts, timeoutMS)
		done := make(chan struct{})
		go func() {
			srv.runLoadModel(context.Background(), br, "srv1", tgt)
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(wdSafety):
			t.Fatalf("runLoadModel did not return within %v", wdSafety)
		}
		if srv.Benchmarks.ServerBusy("srv1") {
			t.Fatalf("ServerBusy true after the load, want cleared")
		}
		st := srv.Benchmarks.Status("srv1")
		if len(st.Results) != 1 {
			t.Fatalf("results = %+v, want exactly one", st.Results)
		}
		return st.Results[0]
	}

	t.Run("keepalives then data load the model", func(t *testing.T) {
		ts := newSSEUpstream(t, func(s sseScript) {
			if s.keepalivesFor(3 * wdIdle) {
				s.delta("ok")
				s.done()
			}
		})
		res := run(t, ts, 2000)
		if !res.Loaded || res.Error != "" {
			t.Fatalf("result = %+v, want Loaded with no error", res)
		}
	})

	t.Run("keepalives forever fail with the first-data text", func(t *testing.T) {
		ts := newSSEUpstream(t, func(s sseScript) { s.keepalivesForever() })
		res := run(t, ts, 500)
		if want := wdFirstDataText(500 * time.Millisecond); res.Loaded || res.Error != want {
			t.Fatalf("result = %+v, want not loaded with error %q", res, want)
		}
	})
}

// TestBenchmarkStreamIdle: the configured idle timeout, or the 2-minute
// default when it is disabled.
func TestBenchmarkStreamIdle(t *testing.T) {
	for _, tc := range []struct {
		configured time.Duration
		want       time.Duration
	}{
		{5 * time.Second, 5 * time.Second},
		{0, benchmarkDefaultStreamIdle},
		{-time.Second, benchmarkDefaultStreamIdle},
	} {
		srv := &Server{}
		srv.streamIdleTimeout = tc.configured
		if got := srv.benchmarkStreamIdle(); got != tc.want {
			t.Errorf("benchmarkStreamIdle() with %v configured = %v, want %v", tc.configured, got, tc.want)
		}
	}
}

// TestColdStartBudget: the larger of the application timeout and idle.
func TestColdStartBudget(t *testing.T) {
	idle := 2 * time.Minute
	for _, tc := range []struct {
		timeout time.Duration
		want    time.Duration
	}{
		{10 * time.Minute, 10 * time.Minute}, // a server_agent default
		{30 * time.Second, idle},             // a stock application
		{0, idle},
	} {
		if got := coldStartBudget(routing.Target{Timeout: tc.timeout}, idle); got != tc.want {
			t.Errorf("coldStartBudget(Timeout %v, idle %v) = %v, want %v", tc.timeout, idle, got, tc.want)
		}
	}
}

// TestStreamWatchdogExplain pins explain's mapping of a cancellation cause onto
// its error: which text each of the watchdog's own causes becomes, that the
// first-data budget is printed to the millisecond, and that any other cause
// returns the provider's error unchanged.
func TestStreamWatchdogExplain(t *testing.T) {
	const idle = 2 * time.Minute
	providerErr := fmt.Errorf("%w: read stream: context canceled", provider.ErrUnavailable)
	for _, tc := range []struct {
		name     string
		cause    error
		credited bool
		budget   time.Duration
		want     string // "" means providerErr itself, unchanged
	}{
		{"a first-data cause without a credited keepalive reads as idle", errBenchmarkStreamFirstData, false, 10 * time.Minute, wdIdleText(idle)},
		{"a first-data cause after a credited keepalive names the budget to the millisecond", errBenchmarkStreamFirstData, true, 585*time.Millisecond + 400*time.Microsecond, "provider.timeout: benchmark stream: no first data within 585ms although the upstream kept the connection alive (max of the application's timeout_ms and the idle budget)"},
		{"an idle cause reads as idle after a credited keepalive too", errBenchmarkStreamIdle, true, 10 * time.Minute, wdIdleText(idle)},
		{"another cause returns the provider's error unchanged", context.Canceled, true, 10 * time.Minute, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := &streamWatchdog{idle: idle, budget: tc.budget}
			w.credited.Store(tc.credited)
			got := w.explain(tc.cause, providerErr)
			if tc.want == "" {
				if got != providerErr {
					t.Fatalf("explain = %v, want the provider's error %v unchanged", got, providerErr)
				}
				return
			}
			if got == nil || got.Error() != tc.want {
				t.Fatalf("explain = %v, want %q", got, tc.want)
			}
			if !errors.Is(got, provider.ErrTimeout) {
				t.Fatalf("explain = %v, want a wrapped provider.ErrTimeout", got)
			}
		})
	}
}
