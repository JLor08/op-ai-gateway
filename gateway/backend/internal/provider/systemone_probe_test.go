// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package provider

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/routing"
	"reflect"
	"testing"
	"time"
)

// TestProbeSystemOneClassifies pins the verdict table of the decision probe:
// only an answer that actually carries the probe question's answer object is a
// yes, only a 404 or a 501 is a no, and everything else -- an answer without
// the question, a body that is not JSON, any other status, a timeout -- is no
// verdict at all, so the caller writes nothing and asks again later.
func TestProbeSystemOneClassifies(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		delay  time.Duration
		want   string
	}{
		{"answer object is a yes", http.StatusOK, `{"answers":{"probe":{"type":"noul","noul":0.9}}}`, 0, SystemOneProbeYes},
		{"no probe answer is no verdict", http.StatusOK, `{"answers":{}}`, 0, ""},
		{"a non-JSON 2xx is no verdict", http.StatusOK, `<html>not an answer</html>`, 0, ""},
		{"404 is a no", http.StatusNotFound, `{"error":"not found"}`, 0, SystemOneProbeNo},
		{"501 is a no", http.StatusNotImplemented, `{"error":"not implemented"}`, 0, SystemOneProbeNo},
		// The agent router's own 404 (its error envelope with a runtime.*
		// code) says nothing about the model: no verdict, not a no.
		{"a router 404 is no verdict", http.StatusNotFound, `{"error":{"code":"runtime.model_not_managed","message":"x"}}`, 0, ""},
		{"an empty 404 is a no", http.StatusNotFound, ``, 0, SystemOneProbeNo},
		{"a 404 with another error code is a no", http.StatusNotFound, `{"error":{"code":"not_found","message":"x"}}`, 0, SystemOneProbeNo},
		{"a 501 with a runtime code is still a no", http.StatusNotImplemented, `{"error":{"code":"runtime.model_not_managed","message":"x"}}`, 0, SystemOneProbeNo},
		{"400 is no verdict", http.StatusBadRequest, `{"error":"bad request"}`, 0, ""},
		{"503 is no verdict", http.StatusServiceUnavailable, `{"error":"loading"}`, 0, ""},
		{"a timeout is no verdict", http.StatusOK, `{"answers":{"probe":{"type":"noul","noul":0.9}}}`, 200 * time.Millisecond, ""},
		// Edges of the yes rule: the answer must be an OBJECT.
		{"a null probe answer is no verdict", http.StatusOK, `{"answers":{"probe":null}}`, 0, ""},
		{"a scalar probe answer is no verdict", http.StatusOK, `{"answers":{"probe":0.9}}`, 0, ""},
		{"answers of the wrong type is no verdict", http.StatusOK, `{"answers":"probe"}`, 0, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.delay > 0 {
					select {
					case <-time.After(tc.delay):
					case <-r.Context().Done():
						return
					}
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			target := routing.Target{Endpoint: srv.URL, Timeout: time.Second}
			if tc.delay > 0 {
				target.Timeout = 20 * time.Millisecond
			}
			got, err := NewOpenAICompatibleClient(srv.Client()).ProbeSystemOne(context.Background(), target, "m")
			if got != tc.want {
				t.Fatalf("verdict = %q (err %v), want %q", got, err, tc.want)
			}
			if tc.want != "" && err != nil {
				t.Fatalf("a %q verdict came with error %v, want nil", tc.want, err)
			}
		})
	}
}

// TestProbeSystemOneSendsTheExactBody pins the request itself: a POST to
// /v1/systemone below the target endpoint, carrying exactly the one tiny
// question body (with the model named), as JSON, and the application's
// upstream credential like every other probe on this client.
func TestProbeSystemOneSendsTheExactBody(t *testing.T) {
	var (
		gotMethod, gotPath, gotAuth, gotType string
		gotBody                              []byte
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		gotAuth = r.Header.Get("X-Api-Key")
		gotType = r.Header.Get("Content-Type")
		gotBody, _ = io.ReadAll(r.Body)
		_, _ = w.Write([]byte(`{"answers":{"probe":{"type":"noul","noul":0.9}}}`))
	}))
	defer srv.Close()
	ctx := WithUpstreamAuth(context.Background(), "X-Api-Key", "so-secret")
	got, err := NewOpenAICompatibleClient(srv.Client()).ProbeSystemOne(ctx, routing.Target{Endpoint: srv.URL + "/"}, "qwen3-decide")
	if err != nil || got != SystemOneProbeYes {
		t.Fatalf("verdict = %q, err = %v; want yes, nil", got, err)
	}
	if gotMethod != http.MethodPost || gotPath != "/v1/systemone" {
		t.Fatalf("request = %s %s, want POST /v1/systemone", gotMethod, gotPath)
	}
	if gotAuth != "so-secret" {
		t.Fatalf("X-Api-Key = %q, want so-secret", gotAuth)
	}
	if gotType != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", gotType)
	}
	const wantBody = `{"model":"qwen3-decide","state":"ok","questions":{"probe":{"type":"noul","instructions":"Is the state non-empty?"}}}`
	var gotJSON, wantJSON any
	if err := json.Unmarshal(gotBody, &gotJSON); err != nil {
		t.Fatalf("body %q is not JSON: %v", gotBody, err)
	}
	if err := json.Unmarshal([]byte(wantBody), &wantJSON); err != nil {
		t.Fatalf("want body: %v", err)
	}
	if !reflect.DeepEqual(gotJSON, wantJSON) {
		t.Fatalf("body = %s, want %s", gotBody, wantBody)
	}
}

// TestMultiplexerRoutesProbeSystemOne: the health loop asks the Multiplexer,
// which must hand the probe to the application type's client, and must answer
// no verdict with an error for a client that cannot probe (Ollama's), so the
// caller writes nothing rather than reading a zero value as an answer.
func TestMultiplexerRoutesProbeSystemOne(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	m := NewMultiplexer(map[string]Client{
		routing.ProviderLlamaCPP: NewOpenAICompatibleClient(srv.Client()),
		routing.ProviderOllama:   NewOllamaClient(srv.Client()),
	}, nil)
	got, err := m.ProbeSystemOne(context.Background(), routing.Target{Provider: routing.ProviderLlamaCPP, Endpoint: srv.URL}, "m")
	if err != nil || got != SystemOneProbeNo {
		t.Fatalf("llama_cpp verdict = %q, err = %v; want no, nil", got, err)
	}
	got, err = m.ProbeSystemOne(context.Background(), routing.Target{Provider: routing.ProviderOllama, Endpoint: srv.URL}, "m")
	if err == nil || got != "" {
		t.Fatalf("ollama verdict = %q, err = %v; want no verdict and an error", got, err)
	}
}
