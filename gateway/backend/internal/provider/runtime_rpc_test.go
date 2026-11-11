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

// rpcRecorder is an agent router stub that records each request's method,
// escaped URI, cluster-start header and credential, and answers with the
// status and body set for its path.
type rpcRecorder struct {
	mu       sync.Mutex
	requests []string
	headers  []string
	auth     []string
	answer   func(path string) (int, string)
}

func newRPCRecorder(t *testing.T, answer func(path string) (int, string)) (*rpcRecorder, *httptest.Server) {
	t.Helper()
	rec := &rpcRecorder{answer: answer}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if len(body) != 0 {
			t.Errorf("request body = %q, want none", body)
		}
		rec.mu.Lock()
		rec.requests = append(rec.requests, r.Method+" "+r.RequestURI)
		rec.headers = append(rec.headers, r.Header.Get(clusterStartHeader))
		rec.auth = append(rec.auth, r.Header.Get("Authorization"))
		rec.mu.Unlock()
		status, out := rec.answer(r.URL.Path)
		w.WriteHeader(status)
		_, _ = io.WriteString(w, out)
	}))
	t.Cleanup(srv.Close)
	return rec, srv
}

func (r *rpcRecorder) snapshot() (requests, headers, auth []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.requests...), append([]string(nil), r.headers...), append([]string(nil), r.auth...)
}

// TestEnsureRuntimeModelSendsClusterStartOnlyWhenMarked pins the cluster-start
// marker: an ensure under WithClusterStart carries X-OP-Cluster-Start: 1, and
// every other ensure carries no such header, so nothing but the gateway's
// cluster orchestration can start a cold head.
func TestEnsureRuntimeModelSendsClusterStartOnlyWhenMarked(t *testing.T) {
	rec, srv := newRPCRecorder(t, func(string) (int, string) { return http.StatusOK, `{"status":"running"}` })
	client := NewOpenAICompatibleClient(srv.Client())
	target := routing.Target{Provider: routing.ProviderServerAgent, Endpoint: srv.URL, ProviderModel: "head"}
	if err := client.EnsureRuntimeModel(context.Background(), target); err != nil {
		t.Fatalf("plain ensure: %v", err)
	}
	if err := client.EnsureRuntimeModel(WithClusterStart(context.Background()), target); err != nil {
		t.Fatalf("cluster-start ensure: %v", err)
	}
	_, headers, _ := rec.snapshot()
	if len(headers) != 2 || headers[0] != "" || headers[1] != "1" {
		t.Fatalf("cluster-start headers = %q, want [\"\" \"1\"]", headers)
	}
	if IsClusterStart(context.Background()) || !IsClusterStart(WithClusterStart(context.Background())) {
		t.Fatal("IsClusterStart does not report the marker")
	}
}

// TestRPCMemberRoutes pins the member routes' requests: bodiless POSTs to
// /rpc/ensure/{id}?reserve_seconds=N (whole seconds, rounded up),
// /rpc/release/{id} and /rpc/stop/{id}[?force=1], the spec id path-escaped and
// the application's credential attached.
func TestRPCMemberRoutes(t *testing.T) {
	rec, srv := newRPCRecorder(t, func(path string) (int, string) {
		switch path {
		case "/rpc/ensure/rpca_h_s":
			return http.StatusOK, `{"state":"running"}`
		case "/rpc/release/rpca_h_s":
			return http.StatusOK, `{"state":"released"}`
		default:
			return http.StatusOK, `{"state":"stopping"}`
		}
	})
	client := NewOpenAICompatibleClient(srv.Client())
	ctx := WithUpstreamAuth(context.Background(), "", "sk-agent")
	target := routing.Target{Provider: routing.ProviderServerAgent, Endpoint: srv.URL + "/"}
	if err := client.EnsureRPCMember(ctx, target, "rpca_h_s", 90*time.Second+time.Millisecond); err != nil {
		t.Fatalf("EnsureRPCMember: %v", err)
	}
	if err := client.ReleaseRPCMember(ctx, target, "rpca_h_s"); err != nil {
		t.Fatalf("ReleaseRPCMember: %v", err)
	}
	if state, err := client.StopRPCMember(ctx, target, "rpca_h_s", false); err != nil || state != RuntimeStopStopping {
		t.Fatalf("StopRPCMember = (%q, %v), want (stopping, nil)", state, err)
	}
	if _, err := client.StopRPCMember(ctx, target, "rpcm x", true); err != nil {
		t.Fatalf("StopRPCMember(force): %v", err)
	}
	requests, _, auth := rec.snapshot()
	want := []string{
		"POST /rpc/ensure/rpca_h_s?reserve_seconds=91",
		"POST /rpc/release/rpca_h_s",
		"POST /rpc/stop/rpca_h_s",
		"POST /rpc/stop/rpcm%20x?force=1",
	}
	if len(requests) != len(want) {
		t.Fatalf("requests = %q, want %q", requests, want)
	}
	for i := range want {
		if requests[i] != want[i] {
			t.Errorf("request %d = %q, want %q", i, requests[i], want[i])
		}
		if auth[i] != "Bearer sk-agent" {
			t.Errorf("request %d Authorization = %q, want Bearer sk-agent", i, auth[i])
		}
	}
}

// TestRPCMemberAnswers pins what the member routes' answers become: a refusal
// keeps the router's code and status, a 2xx with the wrong state is an
// invalid answer, and an expired deadline is ErrTimeout.
func TestRPCMemberAnswers(t *testing.T) {
	cases := []struct {
		name     string
		status   int
		body     string
		call     func(c *OpenAICompatibleClient, ctx context.Context, target routing.Target) error
		wantCode string
		wantIs   error
	}{
		{
			name: "ensure refused", status: http.StatusServiceUnavailable,
			body: `{"error":{"code":"runtime.admission_blocked","message":"no room"}}`,
			call: func(c *OpenAICompatibleClient, ctx context.Context, tg routing.Target) error {
				return c.EnsureRPCMember(ctx, tg, "m", 0)
			},
			wantCode: "runtime.admission_blocked",
		},
		{
			name: "ensure wrong state", status: http.StatusOK, body: `{"state":"stopped"}`,
			call: func(c *OpenAICompatibleClient, ctx context.Context, tg routing.Target) error {
				return c.EnsureRPCMember(ctx, tg, "m", 0)
			},
			wantIs: ErrInvalidResponse,
		},
		{
			name: "release wrong state", status: http.StatusOK, body: `{"state":"running"}`,
			call: func(c *OpenAICompatibleClient, ctx context.Context, tg routing.Target) error {
				return c.ReleaseRPCMember(ctx, tg, "m")
			},
			wantIs: ErrInvalidResponse,
		},
		{
			name: "stop in use", status: http.StatusConflict,
			body: `{"error":{"code":"runtime.cluster_member_in_use","message":"held"}}`,
			call: func(c *OpenAICompatibleClient, ctx context.Context, tg routing.Target) error {
				_, err := c.StopRPCMember(ctx, tg, "m", false)
				return err
			},
			wantCode: "runtime.cluster_member_in_use",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := ensureStub(t, tc.status, tc.body)
			target := routing.Target{Provider: routing.ProviderServerAgent, Endpoint: srv.URL}
			err := tc.call(NewOpenAICompatibleClient(srv.Client()), context.Background(), target)
			if tc.wantCode != "" {
				var re *RouterError
				if !errors.As(err, &re) || re.Code != tc.wantCode || re.Status != tc.status {
					t.Fatalf("err = %v, want a *RouterError %s with status %d", err, tc.wantCode, tc.status)
				}
				return
			}
			if !errors.Is(err, tc.wantIs) {
				t.Fatalf("err = %v, want %v", err, tc.wantIs)
			}
		})
	}

	release := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer slow.Close()
	defer close(release)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	target := routing.Target{Provider: routing.ProviderServerAgent, Endpoint: slow.URL}
	if err := NewOpenAICompatibleClient(slow.Client()).EnsureRPCMember(ctx, target, "m", 0); !errors.Is(err, ErrTimeout) {
		t.Fatalf("deadline err = %v, want ErrTimeout", err)
	}
}

// TestMultiplexerDispatchesRPCMember pins the multiplexer's routing of the
// member calls: a server_agent target reaches the OpenAI-compatible client,
// and a provider whose client lacks the capability answers ErrUnavailable.
func TestMultiplexerDispatchesRPCMember(t *testing.T) {
	rec, srv := newRPCRecorder(t, func(string) (int, string) { return http.StatusOK, `{"state":"released"}` })
	mux := NewMultiplexer(map[string]Client{
		routing.ProviderServerAgent: NewOpenAICompatibleClient(srv.Client()),
		routing.ProviderOllama:      &recordingClient{},
	}, nil)
	if err := mux.ReleaseRPCMember(context.Background(), routing.Target{Provider: routing.ProviderServerAgent, Endpoint: srv.URL}, "m"); err != nil {
		t.Fatalf("ReleaseRPCMember(server_agent): %v", err)
	}
	other := routing.Target{Provider: routing.ProviderOllama, Endpoint: srv.URL}
	if err := mux.EnsureRPCMember(context.Background(), other, "m", 0); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("EnsureRPCMember(ollama) = %v, want ErrUnavailable", err)
	}
	if err := mux.ReleaseRPCMember(context.Background(), other, "m"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("ReleaseRPCMember(ollama) = %v, want ErrUnavailable", err)
	}
	if _, err := mux.StopRPCMember(context.Background(), other, "m", true); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("StopRPCMember(ollama) = %v, want ErrUnavailable", err)
	}
	requests, _, _ := rec.snapshot()
	if len(requests) != 1 || requests[0] != "POST /rpc/release/m" {
		t.Fatalf("upstream requests = %q, want exactly [POST /rpc/release/m]", requests)
	}
}
