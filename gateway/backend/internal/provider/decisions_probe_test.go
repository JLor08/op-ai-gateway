// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package provider

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/routing"
	"testing"
	"time"
)

// TestProbeOpenAIDecisionsClassifies pins the verdict table of the OpenAI
// Decisions probe.
func TestProbeOpenAIDecisionsClassifies(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"answers array with an object is a yes", http.StatusOK, `{"answers":[{"type":"predicate","name":"probe","value":true}]}`, DecisionsProbeYes},
		{"empty answers is no verdict", http.StatusOK, `{"answers":[]}`, ""},
		{"a non-object first answer is no verdict", http.StatusOK, `{"answers":[null]}`, ""},
		{"a non-JSON 2xx is no verdict", http.StatusOK, `<html>x</html>`, ""},
		{"answers as an object is no verdict", http.StatusOK, `{"answers":{"probe":{}}}`, ""},
		{"501 is a no", http.StatusNotImplemented, `{}`, DecisionsProbeNo},
		{"a plain 404 is a no", http.StatusNotFound, `{"error":"not found"}`, DecisionsProbeNo},
		{"an empty 404 is a no", http.StatusNotFound, ``, DecisionsProbeNo},
		{"a router 404 is no verdict", http.StatusNotFound, `{"error":{"code":"runtime.model_not_managed","message":"x"}}`, ""},
		{"400 is no verdict", http.StatusBadRequest, `{}`, ""},
		{"503 is no verdict", http.StatusServiceUnavailable, `{}`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			got, err := NewOpenAICompatibleClient(srv.Client()).ProbeOpenAIDecisions(context.Background(), routing.Target{Endpoint: srv.URL, Timeout: time.Second}, "m")
			if got != tc.want {
				t.Fatalf("verdict = %q (err %v), want %q", got, err, tc.want)
			}
			if tc.want != "" && err != nil {
				t.Fatalf("a %q verdict came with error %v", tc.want, err)
			}
			if tc.want == "" && err == nil {
				t.Fatal("no verdict must carry an error")
			}
		})
	}
}

// TestProbeOpenAIDecisionsMarksARefusal: only a 400, 405 or 422 is marked.
func TestProbeOpenAIDecisionsMarksARefusal(t *testing.T) {
	cases := map[int]bool{
		http.StatusBadRequest: true, http.StatusMethodNotAllowed: true, http.StatusUnprocessableEntity: true,
		http.StatusTooManyRequests: false, http.StatusInternalServerError: false, http.StatusServiceUnavailable: false, http.StatusUnauthorized: false,
	}
	for status, refused := range cases {
		t.Run(http.StatusText(status), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(status) }))
			defer srv.Close()
			got, err := NewOpenAICompatibleClient(srv.Client()).ProbeOpenAIDecisions(context.Background(), routing.Target{Endpoint: srv.URL}, "m")
			if got != "" || err == nil || !errors.Is(err, ErrUnavailable) {
				t.Fatalf("verdict = %q, err = %v; want no verdict wrapping ErrUnavailable", got, err)
			}
			if errors.Is(err, ErrDecisionsProbeRefused) != refused {
				t.Fatalf("errors.Is(refused) = %v, want %v", !refused, refused)
			}
		})
	}
}

// TestProbeOpenAIDecisionsSendsTheExactBody pins the request bytes.
func TestProbeOpenAIDecisionsSendsTheExactBody(t *testing.T) {
	var gotMethod, gotPath, gotAuth string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotAuth = r.Method, r.URL.Path, r.Header.Get("X-Api-Key")
		gotBody, _ = io.ReadAll(r.Body)
		_, _ = w.Write([]byte(`{"answers":[{"value":true}]}`))
	}))
	defer srv.Close()
	ctx := WithUpstreamAuth(context.Background(), "X-Api-Key", "secret")
	got, err := NewOpenAICompatibleClient(srv.Client()).ProbeOpenAIDecisions(ctx, routing.Target{Endpoint: srv.URL + "/"}, "m")
	if err != nil || got != DecisionsProbeYes {
		t.Fatalf("verdict = %q, err = %v", got, err)
	}
	if gotMethod != http.MethodPost || gotPath != "/v1/decisions" || gotAuth != "secret" {
		t.Fatalf("request = %s %s auth %q", gotMethod, gotPath, gotAuth)
	}
	const want = `{"model":"m","input":"ok","questions":[{"type":"predicate","name":"probe","instructions":"Is the input non-empty?"}]}`
	if string(gotBody) != want {
		t.Fatalf("body = %s, want %s", gotBody, want)
	}
}

// TestMultiplexerRoutesProbeOpenAIDecisions: routed to the provider's client,
// no verdict plus an error for a client that cannot probe.
func TestMultiplexerRoutesProbeOpenAIDecisions(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) }))
	defer srv.Close()
	m := NewMultiplexer(map[string]Client{
		routing.ProviderVLLM:   NewOpenAICompatibleClient(srv.Client()),
		routing.ProviderOllama: NewOllamaClient(srv.Client()),
	}, nil)
	got, err := m.ProbeOpenAIDecisions(context.Background(), routing.Target{Provider: routing.ProviderVLLM, Endpoint: srv.URL}, "m")
	if err != nil || got != DecisionsProbeNo {
		t.Fatalf("vllm verdict = %q, err = %v; want no, nil", got, err)
	}
	got, err = m.ProbeOpenAIDecisions(context.Background(), routing.Target{Provider: routing.ProviderOllama, Endpoint: srv.URL}, "m")
	if err == nil || got != "" {
		t.Fatalf("ollama verdict = %q, err = %v; want no verdict and an error", got, err)
	}
}
