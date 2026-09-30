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
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// ensureStub serves one fixed answer on every request, the way the agent
// router's ensure route answers once EnsureRunning has returned.
func ensureStub(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set(contentTypeHeader, jsonContentType)
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// cancelOnBodyRead cancels the call's context when the caller first reads the
// answer's body. That is strictly after Do has returned the headers, so the
// cancellation lands in the body read and never in Do, whose own cancellation
// path is a different branch.
type cancelOnBodyRead struct {
	base   http.RoundTripper
	cancel context.CancelFunc
}

func (c cancelOnBodyRead) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := c.base.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	resp.Body = cancellingBody{ReadCloser: resp.Body, cancel: c.cancel}
	return resp, nil
}

// cancellingBody calls cancel before every read (a CancelFunc is idempotent)
// and then reads on, so the read ends when the transport sees the cancellation.
type cancellingBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b cancellingBody) Read(p []byte) (int, error) {
	b.cancel()
	return b.ReadCloser.Read(p)
}

// TestEnsureRuntimeModelPostsBodilessToTheEscapedRoute pins the request
// itself. The model id carries a "/", spaces and a "?": each segment must be
// escaped and the "/" kept, so the router's decoded path yields the id back
// unchanged. The POST must carry no body, because an agent router without the
// route answers a bodiless POST 404 before it starts anything. It must carry
// the application's credential, like every other request on this client.
func TestEnsureRuntimeModelPostsBodilessToTheEscapedRoute(t *testing.T) {
	const model = "sd/flux 1 dev?v=2"
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.RequestURI != "/ensure/sd/flux%201%20dev%3Fv=2" {
			t.Errorf("request URI = %q, want /ensure/sd/flux%%201%%20dev%%3Fv=2", r.RequestURI)
		}
		if r.URL.Path != "/ensure/"+model {
			t.Errorf("decoded path = %q, want %q", r.URL.Path, "/ensure/"+model)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil || len(body) != 0 || r.ContentLength > 0 {
			t.Errorf("body = %q (content length %d, read error %v), want none", body, r.ContentLength, err)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer sk-ensure" {
			t.Errorf("Authorization = %q, want Bearer sk-ensure", got)
		}
		_, _ = io.WriteString(w, `{"status":"running"}`)
	}))
	defer srv.Close()

	ctx := WithUpstreamAuth(context.Background(), "", "sk-ensure")
	target := routing.Target{Provider: routing.ProviderServerAgent, Endpoint: srv.URL + "/", ProviderModel: model}
	if err := NewOpenAICompatibleClient(srv.Client()).EnsureRuntimeModel(ctx, target); err != nil {
		t.Fatalf("EnsureRuntimeModel = %v, want nil", err)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("upstream calls = %d, want exactly 1", n)
	}
}

// runningPaddedTo is the ensure route's success body, padded with spaces
// before its closing brace to exactly n bytes.
func runningPaddedTo(n int) string {
	const head = `{"status":"running"`
	return head + strings.Repeat(" ", n-len(head)-1) + "}"
}

// TestEnsureRuntimeModelRequiresRunning pins what counts as success: a 2xx
// whose JSON body says status "running". Any other 2xx is not proof that the
// child is up and must not read as one.
func TestEnsureRuntimeModelRequiresRunning(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		body    string
		wantErr string // "" = success
	}{
		{"running", http.StatusOK, `{"status":"running"}`, ""},
		{"running on another 2xx", http.StatusAccepted, `{"status":"running"}`, ""},
		{"running beside other fields", http.StatusOK, `{"status":"running","model":"m"}`, ""},
		{"no status", http.StatusOK, `{}`, `provider.invalid_response: ensure route answered 200 without status "running"`},
		{"another status", http.StatusOK, `{"status":"starting"}`, `provider.invalid_response: ensure route answered 200 without status "running"`},
		{"status of the wrong case", http.StatusOK, `{"status":"Running"}`, `provider.invalid_response: ensure route answered 200 without status "running"`},
		{"empty body", http.StatusOK, ``, `provider.invalid_response: ensure route answered 200 without status "running"`},
		{"not JSON", http.StatusOK, `<html>ok</html>`, `provider.invalid_response: ensure route answered 200 without status "running"`},
		{"no content", http.StatusNoContent, ``, `provider.invalid_response: ensure route answered 204 without status "running"`},
		// The answer is read up to 64 KiB. A success body that fills the cap
		// exactly is still read whole; one byte more is cut short, and the
		// cut body no longer parses.
		{"running that fills the read cap", http.StatusOK, runningPaddedTo(64 << 10), ""},
		{"running past the read cap", http.StatusOK, runningPaddedTo(64<<10 + 1), `provider.invalid_response: ensure route answered 200 without status "running"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := ensureStub(t, tc.status, tc.body)
			err := NewOpenAICompatibleClient(srv.Client()).EnsureRuntimeModel(context.Background(), routing.Target{Endpoint: srv.URL, ProviderModel: "m"})
			assertEnsureAnswer(t, err, tc.wantErr)
		})
	}
}

// assertEnsureAnswer checks an EnsureRuntimeModel result against wantErr: nil
// for "", and otherwise an ErrInvalidResponse -- never an ErrUnavailable, since
// the agent answered -- with exactly wantErr as its text.
func assertEnsureAnswer(t *testing.T, err error, wantErr string) {
	t.Helper()
	if wantErr == "" {
		if err != nil {
			t.Fatalf("EnsureRuntimeModel = %v, want nil", err)
		}
		return
	}
	if !errors.Is(err, ErrInvalidResponse) {
		t.Fatalf("EnsureRuntimeModel = %v, want ErrInvalidResponse", err)
	}
	if errors.Is(err, ErrUnavailable) {
		t.Fatalf("EnsureRuntimeModel = %v, must not read as ErrUnavailable: the agent answered, only not with running", err)
	}
	if err.Error() != wantErr {
		t.Fatalf("error text = %q, want %q", err.Error(), wantErr)
	}
}

// TestEnsureRuntimeModelRejectsAnAnswerCutShort: the declared length promises
// more than is sent, so the read ends in an unexpected EOF after a prefix of
// the success body, and that is an invalid answer, not a running child.
func TestEnsureRuntimeModelRejectsAnAnswerCutShort(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "64")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"status":"running"`)
	}))
	defer srv.Close()
	err := NewOpenAICompatibleClient(srv.Client()).EnsureRuntimeModel(context.Background(), routing.Target{Endpoint: srv.URL, ProviderModel: "m"})
	assertEnsureAnswer(t, err, "provider.invalid_response: read ensure answer: unexpected EOF")
}

// TestEnsureRuntimeModelKeepsTheRouterCode pins the non-2xx mapping. The
// status maps exactly as on every other request of this client (503 stays the
// retryable ErrUpstreamStarting, 401/403 stay ErrAuthRejected), and a body
// that is the agent router's error envelope adds its code as a RouterError,
// so a caller can name what the agent refused. The envelope's message is kept
// only when it says more than the code.
func TestEnsureRuntimeModelKeepsTheRouterCode(t *testing.T) {
	cases := []struct {
		name         string
		status       int
		body         string
		wantCode     string // "" = no RouterError in the chain
		wantStarting bool
		wantAuth     bool
		wantText     string
	}{
		{
			name: "admission blocked", status: http.StatusServiceUnavailable,
			body:     `{"error":{"code":"runtime.admission_blocked","message":"runtime.admission_blocked"}}`,
			wantCode: "runtime.admission_blocked", wantStarting: true,
			wantText: "runtime.admission_blocked: provider.unavailable (upstream starting): upstream status 503",
		},
		{
			name: "start timeout", status: http.StatusGatewayTimeout,
			body:     `{"error":{"code":"runtime.start_timeout","message":"runtime.start_timeout"}}`,
			wantCode: "runtime.start_timeout",
			wantText: "runtime.start_timeout: provider.unavailable: upstream status 504",
		},
		{
			name: "start failed", status: http.StatusBadGateway,
			body:     `{"error":{"code":"runtime.start_failed","message":"runtime.start_failed"}}`,
			wantCode: "runtime.start_failed",
			wantText: "runtime.start_failed: provider.unavailable: upstream status 502",
		},
		{
			name: "not permitted", status: http.StatusBadGateway,
			body:     `{"error":{"code":"runtime.not_permitted","message":"runtime.not_permitted"}}`,
			wantCode: "runtime.not_permitted",
			wantText: "runtime.not_permitted: provider.unavailable: upstream status 502",
		},
		{
			name: "model not managed", status: http.StatusNotFound,
			body:     `{"error":{"code":"runtime.model_not_managed","message":"runtime.model_not_managed"}}`,
			wantCode: "runtime.model_not_managed",
			wantText: "runtime.model_not_managed: provider.unavailable: upstream status 404",
		},
		{
			name: "a message that says more than the code", status: http.StatusBadGateway,
			body:     `{"error":{"code":"runtime.upstream_gone","message":"dial tcp 127.0.0.1:9001: connect: connection refused"}}`,
			wantCode: "runtime.upstream_gone",
			wantText: "runtime.upstream_gone: provider.unavailable: upstream status 502: dial tcp 127.0.0.1:9001: connect: connection refused",
		},
		{
			name: "an envelope without a message", status: http.StatusServiceUnavailable,
			body:     `{"error":{"code":"runtime.admission_blocked"}}`,
			wantCode: "runtime.admission_blocked", wantStarting: true,
			wantText: "runtime.admission_blocked: provider.unavailable (upstream starting): upstream status 503",
		},
		{
			name: "credential rejected", status: http.StatusUnauthorized, body: ``,
			wantAuth: true,
			wantText: "provider.unavailable (upstream rejected the credential): upstream status 401",
		},
		{
			name: "forbidden", status: http.StatusForbidden, body: `forbidden`,
			wantAuth: true,
			wantText: "provider.unavailable (upstream rejected the credential): upstream status 403",
		},
		{
			name: "a body that is not an envelope", status: http.StatusBadGateway, body: `<html>502 Bad Gateway</html>`,
			wantText: "provider.unavailable: upstream status 502",
		},
		{
			name: "a numeric code is not a router code", status: http.StatusServiceUnavailable, body: `{"error":{"code":503,"message":"busy"}}`,
			wantStarting: true,
			wantText:     "provider.unavailable (upstream starting): upstream status 503",
		},
		{
			name: "an empty code is not a router code", status: http.StatusServiceUnavailable, body: `{"error":{"code":"  ","message":"busy"}}`,
			wantStarting: true,
			wantText:     "provider.unavailable (upstream starting): upstream status 503",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := ensureStub(t, tc.status, tc.body)
			err := NewOpenAICompatibleClient(srv.Client()).EnsureRuntimeModel(context.Background(), routing.Target{Endpoint: srv.URL, ProviderModel: "m"})
			if !errors.Is(err, ErrUnavailable) {
				t.Fatalf("EnsureRuntimeModel = %v, want ErrUnavailable for status %d", err, tc.status)
			}
			if got := errors.Is(err, ErrUpstreamStarting); got != tc.wantStarting {
				t.Fatalf("errors.Is(err, ErrUpstreamStarting) = %v, want %v (err %v)", got, tc.wantStarting, err)
			}
			if got := errors.Is(err, ErrAuthRejected); got != tc.wantAuth {
				t.Fatalf("errors.Is(err, ErrAuthRejected) = %v, want %v (err %v)", got, tc.wantAuth, err)
			}
			var re *RouterError
			if got := errors.As(err, &re); got != (tc.wantCode != "") {
				t.Fatalf("errors.As(err, *RouterError) = %v, want %v (err %v)", got, tc.wantCode != "", err)
			}
			if re != nil && re.Code != tc.wantCode {
				t.Fatalf("RouterError.Code = %q, want %q", re.Code, tc.wantCode)
			}
			if err.Error() != tc.wantText {
				t.Fatalf("error text = %q, want %q", err.Error(), tc.wantText)
			}
		})
	}
}

// TestEnsureRuntimeModelIsBoundOnlyByTheContext pins the call's bound. The
// router holds the request for the whole cold start, which can outlast the
// application's timeout, so target.Timeout must not end it; the caller's
// context deadline does, as ErrTimeout, carrying the deadline's cause. A
// cancellation is not a timeout.
func TestEnsureRuntimeModelIsBoundOnlyByTheContext(t *testing.T) {
	t.Run("target.Timeout does not end the call", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			time.Sleep(300 * time.Millisecond)
			_, _ = io.WriteString(w, `{"status":"running"}`)
		}))
		defer srv.Close()
		target := routing.Target{Endpoint: srv.URL, ProviderModel: "m", Timeout: 10 * time.Millisecond}
		if err := NewOpenAICompatibleClient(srv.Client()).EnsureRuntimeModel(context.Background(), target); err != nil {
			t.Fatalf("EnsureRuntimeModel = %v, want nil: a start longer than target.Timeout must still succeed", err)
		}
	})

	// holdingStub answers only once the request's context ends, so the
	// caller's own bound is the only way out.
	holdingStub := func(t *testing.T, arrived chan<- struct{}) *httptest.Server {
		t.Helper()
		release := make(chan struct{})
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if arrived != nil {
				arrived <- struct{}{}
			}
			select {
			case <-r.Context().Done():
			case <-release:
			}
		}))
		t.Cleanup(srv.Close)
		t.Cleanup(func() { close(release) })
		return srv
	}

	t.Run("the context deadline is ErrTimeout with its cause", func(t *testing.T) {
		srv := holdingStub(t, nil)
		bound := errors.New("test bound")
		ctx, cancel := context.WithDeadlineCause(context.Background(), time.Now().Add(100*time.Millisecond), bound)
		defer cancel()
		err := NewOpenAICompatibleClient(srv.Client()).EnsureRuntimeModel(ctx, routing.Target{Endpoint: srv.URL, ProviderModel: "m"})
		if !errors.Is(err, ErrTimeout) {
			t.Fatalf("EnsureRuntimeModel = %v, want ErrTimeout", err)
		}
		if errors.Is(err, ErrUnavailable) {
			t.Fatalf("EnsureRuntimeModel = %v, must not read as ErrUnavailable", err)
		}
		if !errors.Is(err, bound) {
			t.Fatalf("EnsureRuntimeModel = %v, want the deadline's cause in the chain", err)
		}
		if want := "provider.timeout: ensure route: test bound"; err.Error() != want {
			t.Fatalf("error text = %q, want %q", err.Error(), want)
		}
	})

	t.Run("a deadline while reading a 2xx answer is ErrTimeout", func(t *testing.T) {
		release := make(chan struct{})
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Length", "64")
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			select {
			case <-r.Context().Done():
			case <-release:
			}
		}))
		defer srv.Close()
		defer close(release)
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		err := NewOpenAICompatibleClient(srv.Client()).EnsureRuntimeModel(ctx, routing.Target{Endpoint: srv.URL, ProviderModel: "m"})
		if !errors.Is(err, ErrTimeout) || errors.Is(err, ErrInvalidResponse) {
			t.Fatalf("EnsureRuntimeModel = %v, want ErrTimeout and not ErrInvalidResponse", err)
		}
		if want := "provider.timeout: ensure route: context deadline exceeded"; err.Error() != want {
			t.Fatalf("error text = %q, want %q", err.Error(), want)
		}
	})

	t.Run("a cancellation while reading a 2xx answer is ErrUnavailable", func(t *testing.T) {
		release := make(chan struct{})
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Length", "64")
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			<-release
		}))
		defer srv.Close()
		defer close(release)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		client := &http.Client{Transport: cancelOnBodyRead{base: srv.Client().Transport, cancel: cancel}}
		err := NewOpenAICompatibleClient(client).EnsureRuntimeModel(ctx, routing.Target{Endpoint: srv.URL, ProviderModel: "m"})
		if !errors.Is(err, ErrUnavailable) || errors.Is(err, ErrInvalidResponse) || errors.Is(err, ErrTimeout) {
			t.Fatalf("EnsureRuntimeModel = %v, want ErrUnavailable and neither ErrInvalidResponse nor ErrTimeout", err)
		}
		if want := "provider.unavailable: read ensure answer: context canceled"; err.Error() != want {
			t.Fatalf("error text = %q, want %q", err.Error(), want)
		}
	})

	t.Run("a cancellation is not a timeout", func(t *testing.T) {
		arrived := make(chan struct{}, 1)
		srv := holdingStub(t, arrived)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go func() {
			<-arrived
			cancel()
		}()
		err := NewOpenAICompatibleClient(srv.Client()).EnsureRuntimeModel(ctx, routing.Target{Endpoint: srv.URL, ProviderModel: "m"})
		if !errors.Is(err, ErrUnavailable) || errors.Is(err, ErrTimeout) {
			t.Fatalf("EnsureRuntimeModel = %v, want ErrUnavailable and not ErrTimeout", err)
		}
	})
}
