// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newTestStub builds a stub that is ready immediately.
func newTestStub(tag string) *stub {
	return &stub{tag: tag, readyAt: time.Now().Add(-time.Second)}
}

func TestHealthIsOKOnceReady(t *testing.T) {
	srv := httptest.NewServer(newMux(newTestStub("alpha")))
	defer srv.Close()

	res, err := http.Get(srv.URL + "/health")
	if err != nil {
		t.Fatalf("GET /health: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET /health status = %d, want 200", res.StatusCode)
	}
	var body map[string]string
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["status"] != "ok" || body["tag"] != "alpha" {
		t.Fatalf("GET /health body = %#v", body)
	}
}

// The readiness gate is what makes the agent's `starting` state observable in
// the e2e suite instead of raced past, so it gets its own test: 503 before
// readyAt on BOTH the health path and the completions path.
func TestNotReadyRefusesHealthAndCompletions(t *testing.T) {
	s := &stub{tag: "slow", readyAt: time.Now().Add(time.Hour)}
	srv := httptest.NewServer(newMux(s))
	defer srv.Close()

	res, err := http.Get(srv.URL + "/health")
	if err != nil {
		t.Fatalf("GET /health: %v", err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("GET /health before readyAt status = %d, want 503", res.StatusCode)
	}

	res2, err := http.Post(srv.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("POST completions: %v", err)
	}
	res2.Body.Close()
	if res2.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("POST completions before readyAt status = %d, want 503", res2.StatusCode)
	}
	if got := s.completions.Load(); got != 0 {
		t.Fatalf("completions counter = %d, want 0 (a refused request must not count)", got)
	}
}

// The tag in the completion text is the e2e suite's only way to prove WHICH
// child process answered an inference, so the exact shape is asserted here.
func TestCompletionEchoesPromptAndTag(t *testing.T) {
	s := newTestStub("beta")
	srv := httptest.NewServer(newMux(s))
	defer srv.Close()

	res, err := http.Post(srv.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"stub-model","messages":[{"role":"system","content":"sys"},{"role":"user","content":"ping pong"}]}`))
	if err != nil {
		t.Fatalf("POST completions: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("POST completions status = %d, want 200", res.StatusCode)
	}
	var got chatResponse
	if err := json.NewDecoder(res.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Model != "stub-model" {
		t.Fatalf("model = %q, want the request's model", got.Model)
	}
	if len(got.Choices) != 1 {
		t.Fatalf("choices = %d, want 1", len(got.Choices))
	}
	if got.Choices[0].Message.Content != "[beta] echo: ping pong" {
		t.Fatalf("content = %q", got.Choices[0].Message.Content)
	}
	if got.Choices[0].FinishReason != "stop" {
		t.Fatalf("finish_reason = %q, want stop", got.Choices[0].FinishReason)
	}
	// The gateway's OpenAI-compatible provider records these; zeros would be
	// indistinguishable from "the upstream reported no usage at all".
	if got.Usage.PromptTokens != 2 || got.Usage.CompletionTokens != 4 || got.Usage.TotalTokens != 6 {
		t.Fatalf("usage = %#v", got.Usage)
	}
	if n := s.completions.Load(); n != 1 {
		t.Fatalf("completions counter = %d, want 1", n)
	}
}

func TestCompletionWithoutUserMessageFallsBackToLastMessage(t *testing.T) {
	s := newTestStub("gamma")
	srv := httptest.NewServer(newMux(s))
	defer srv.Close()

	res, err := http.Post(srv.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"m","messages":[{"role":"system","content":"only-system"}]}`))
	if err != nil {
		t.Fatalf("POST completions: %v", err)
	}
	defer res.Body.Close()
	var got chatResponse
	if err := json.NewDecoder(res.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Choices[0].Message.Content != "[gamma] echo: only-system" {
		t.Fatalf("content = %q", got.Choices[0].Message.Content)
	}
}

func TestCompletionsRejectsGET(t *testing.T) {
	srv := httptest.NewServer(newMux(newTestStub("delta")))
	defer srv.Close()

	res, err := http.Get(srv.URL + "/v1/chat/completions")
	if err != nil {
		t.Fatalf("GET completions: %v", err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET completions status = %d, want 405", res.StatusCode)
	}
}

func TestStatsReportsTagAndCount(t *testing.T) {
	s := newTestStub("eps")
	srv := httptest.NewServer(newMux(s))
	defer srv.Close()

	res, err := http.Post(srv.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"x"}]}`))
	if err != nil {
		t.Fatalf("POST completions: %v", err)
	}
	res.Body.Close()

	statsRes, err := http.Get(srv.URL + "/stats")
	if err != nil {
		t.Fatalf("GET /stats: %v", err)
	}
	defer statsRes.Body.Close()
	var stats struct {
		Tag         string `json:"tag"`
		Ready       bool   `json:"ready"`
		Completions int    `json:"completions"`
	}
	if err := json.NewDecoder(statsRes.Body).Decode(&stats); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if stats.Tag != "eps" || !stats.Ready || stats.Completions != 1 {
		t.Fatalf("stats = %#v", stats)
	}
}

func TestLastUserContent(t *testing.T) {
	var empty chatRequest
	if got := lastUserContent(empty); got != "" {
		t.Fatalf("lastUserContent(empty) = %q, want empty", got)
	}
}
