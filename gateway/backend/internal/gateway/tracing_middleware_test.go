// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/logbuffer"
	"op-ai-gateway/internal/tracing"
	"testing"
)

func setDefaultSlogForTest(t *testing.T, logs *logbuffer.Buffer) {
	t.Helper()
	prev := slog.Default()
	slog.SetDefault(slog.New(logs.Handler(io.Discard)))
	t.Cleanup(func() { slog.SetDefault(prev) })
}

func TestServeWithStartsRequestSpan(t *testing.T) {
	logs := logbuffer.NewBuffer(50, logbuffer.LevelTrace)
	setDefaultSlogForTest(t, logs)
	p, _ := tracing.Setup(tracing.Options{Enabled: true, SampleRatio: 1.0}, logs)
	// NOTE: context.Background(), not nil — the SDK's Shutdown dereferences
	// ctx.Done() internally, which panics on a bare nil interface.
	defer p.Shutdown(context.Background())

	s := &Server{mux: http.NewServeMux(), Logs: logs}
	s.mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	rec := httptest.NewRecorder()
	s.serveWith(rec, req, s.mux, true)

	if rec.Header().Get("X-Trace-Id") == "" {
		t.Errorf("X-Trace-Id header not set on inference endpoint")
	}
	var found bool
	for _, r := range logs.Snapshot() {
		if r.Msg == "span" && r.Attrs["span"] == "POST /v1/chat/completions" {
			found = true
		}
	}
	if !found {
		t.Errorf("request span not emitted with route name; got %+v", logs.Snapshot())
	}
}

func TestServeWithHealthzNoSpan(t *testing.T) {
	logs := logbuffer.NewBuffer(50, logbuffer.LevelTrace)
	setDefaultSlogForTest(t, logs)
	p, _ := tracing.Setup(tracing.Options{Enabled: true, SampleRatio: 1.0}, logs)
	defer p.Shutdown(context.Background())
	s := &Server{mux: http.NewServeMux(), Logs: logs}
	s.mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	s.serveWith(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/healthz", nil), s.mux, true)
	for _, r := range logs.Snapshot() {
		if r.Msg == "span" {
			t.Fatalf("/healthz produced a span")
		}
	}
}
