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
	"sync"
	"testing"
	"time"
)

// TestStopRuntimeModelPostsBodilessToTheEscapedRoute pins the request: a
// bodiless POST to /stop/{model} with the instance address's "#" escaped, so
// the router's decoded path yields the address back, and the application's
// credential attached like every other request on this client.
func TestStopRuntimeModelPostsBodilessToTheEscapedRoute(t *testing.T) {
	const model = "qwen/q4#2"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.RequestURI != "/stop/qwen/q4%232" {
			t.Errorf("request URI = %q, want /stop/qwen/q4%%232", r.RequestURI)
		}
		if r.URL.Path != "/stop/"+model {
			t.Errorf("decoded path = %q, want %q", r.URL.Path, "/stop/"+model)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil || len(body) != 0 || r.ContentLength > 0 {
			t.Errorf("body = %q (content length %d, read error %v), want none", body, r.ContentLength, err)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer sk-stop" {
			t.Errorf("Authorization = %q, want Bearer sk-stop", got)
		}
		_, _ = io.WriteString(w, `{"state":"stopping"}`)
	}))
	defer srv.Close()

	ctx := WithUpstreamAuth(context.Background(), "", "sk-stop")
	target := routing.Target{Provider: routing.ProviderServerAgent, Endpoint: srv.URL + "/", ProviderModel: model}
	state, err := NewOpenAICompatibleClient(srv.Client()).StopRuntimeModel(ctx, target)
	if err != nil || state != RuntimeStopStopping {
		t.Fatalf("StopRuntimeModel = (%q, %v), want (stopping, nil)", state, err)
	}
}

// TestStopRuntimeModelAnswers pins what each answer of the stop route becomes:
// the two states pass through, any other 2xx is an invalid answer, and a
// refusal keeps the router's code and status as a *RouterError.
func TestStopRuntimeModelAnswers(t *testing.T) {
	cases := []struct {
		name      string
		status    int
		body      string
		wantState string
		wantCode  string // router code of a refusal
		wantIs    error
	}{
		{name: "stopping", status: http.StatusOK, body: `{"state":"stopping"}`, wantState: RuntimeStopStopping},
		{name: "stopped", status: http.StatusOK, body: `{"state":"stopped"}`, wantState: RuntimeStopStopped},
		{name: "unknown state", status: http.StatusOK, body: `{"state":"running"}`, wantIs: ErrInvalidResponse},
		{name: "not json", status: http.StatusOK, body: `stopping`, wantIs: ErrInvalidResponse},
		{name: "protected", status: http.StatusConflict, body: `{"error":{"code":"runtime.instance_protected","message":"runtime.instance_protected"}}`, wantCode: "runtime.instance_protected"},
		{name: "not highest", status: http.StatusConflict, body: `{"error":{"code":"runtime.instance_not_highest","message":"a higher instance runs"}}`, wantCode: "runtime.instance_not_highest"},
		{name: "not managed", status: http.StatusNotFound, body: `{"error":{"code":"runtime.model_not_managed","message":"runtime.model_not_managed"}}`, wantCode: "runtime.model_not_managed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := ensureStub(t, tc.status, tc.body)
			target := routing.Target{Provider: routing.ProviderServerAgent, Endpoint: srv.URL, ProviderModel: "m#1"}
			state, err := NewOpenAICompatibleClient(srv.Client()).StopRuntimeModel(context.Background(), target)
			switch {
			case tc.wantCode != "":
				var re *RouterError
				if !errors.As(err, &re) || re.Code != tc.wantCode || re.Status != tc.status {
					t.Fatalf("err = %v, want a *RouterError %s with status %d", err, tc.wantCode, tc.status)
				}
			case tc.wantIs != nil:
				if !errors.Is(err, tc.wantIs) {
					t.Fatalf("err = %v, want %v", err, tc.wantIs)
				}
			default:
				if err != nil || state != tc.wantState {
					t.Fatalf("StopRuntimeModel = (%q, %v), want (%q, nil)", state, err, tc.wantState)
				}
			}
		})
	}
}

// TestStopRuntimeModelDeadlineIsTimeout pins that a call whose deadline ends
// it reads as ErrTimeout, as the ensure call does.
func TestStopRuntimeModelDeadlineIsTimeout(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	target := routing.Target{Provider: routing.ProviderServerAgent, Endpoint: srv.URL, ProviderModel: "m"}
	if _, err := NewOpenAICompatibleClient(srv.Client()).StopRuntimeModel(ctx, target); !errors.Is(err, ErrTimeout) {
		t.Fatalf("err = %v, want ErrTimeout", err)
	}
}

// TestMultiplexerDispatchesRuntimeStop pins the multiplexer's routing of the
// stop call: a server_agent target reaches the OpenAI-compatible client, and a
// provider whose client lacks the capability answers ErrUnavailable and sends
// nothing.
func TestMultiplexerDispatchesRuntimeStop(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.Method+" "+r.URL.Path)
		mu.Unlock()
		_, _ = w.Write([]byte(`{"state":"stopped"}`))
	}))
	defer srv.Close()
	shared := NewOpenAICompatibleClient(srv.Client())
	mux := NewMultiplexer(map[string]Client{
		routing.ProviderServerAgent: shared,
		routing.ProviderOllama:      &recordingClient{},
	}, nil)

	state, err := mux.StopRuntimeModel(context.Background(), routing.Target{Provider: routing.ProviderServerAgent, Endpoint: srv.URL, ProviderModel: "m"})
	if err != nil || state != RuntimeStopStopped {
		t.Fatalf("StopRuntimeModel(server_agent) = (%q, %v), want (stopped, nil)", state, err)
	}
	if _, err := mux.StopRuntimeModel(context.Background(), routing.Target{Provider: routing.ProviderOllama, Endpoint: srv.URL, ProviderModel: "m"}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("StopRuntimeModel(ollama) error = %v, want ErrUnavailable", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(paths) != 1 || paths[0] != "POST /stop/m" {
		t.Fatalf("upstream requests = %v, want exactly [POST /stop/m]", paths)
	}
}
