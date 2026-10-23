// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package runtime

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// liveCapture records what a stub full-duplex /live child observed about the
// request the router relayed to it: the outbound path, the query params, and
// how many body bytes it read before EOF. It is handed back over a buffered
// channel so the asserting goroutine has a happens-before edge to read it
// race-free (go test -race).
type liveCapture struct {
	path         string
	model        string
	sampleRate   string
	channels     string
	sampleFormat string
	language     string
	bodyLen      int
}

// newLiveChild returns an httptest server that implements a minimal full-duplex
// transcription child at /v1/audio/transcriptions/live: it emits one SSE event
// up front, streams the chunked request body to EOF, then emits a final
// transcript event and a terminal [DONE] frame. A body EOF is the end of the
// turn. The capture of what it saw is delivered on the returned channel exactly
// once, after the exchange completes.
func newLiveChild(t *testing.T) (*httptest.Server, <-chan liveCapture) {
	t.Helper()
	capCh := make(chan liveCapture, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		rc := http.NewResponseController(w)
		_ = rc.EnableFullDuplex() // let this child interleave reading the body with writing SSE
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: {\"partial\":\"hel\"}\n\n")
		_ = rc.Flush()
		// Stream the (chunked) request body to EOF; a body EOF ends the turn.
		body, _ := io.ReadAll(r.Body)
		_, _ = io.WriteString(w, "data: {\"partial\":\"hello world\"}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		_ = rc.Flush()
		capCh <- liveCapture{
			path:         r.URL.Path,
			model:        q.Get("model"),
			sampleRate:   q.Get("sample_rate"),
			channels:     q.Get("channels"),
			sampleFormat: q.Get("sample_format"),
			language:     q.Get("language"),
			bodyLen:      len(body),
		}
	}))
	t.Cleanup(srv.Close)
	return srv, capCh
}

// TestRouterLiveRelaysFullDuplex is the headline case for POST /live/{model}:
// the model is taken from the PATH and added to the child query; the inbound
// sample_rate/channels/sample_format/language params are forwarded unchanged;
// the request body is streamed (chunked, not buffered) to the child's
// /v1/audio/transcriptions/live; the child's SSE bytes are relayed back; and
// the child admission is released exactly once after the exchange. Exercised
// through the router's real ServeHTTP dispatch over a live httptest connection
// so EnableFullDuplex has a real HTTP/1.1 connection to work on.
func TestRouterLiveRelaysFullDuplex(t *testing.T) {
	child, capCh := newLiveChild(t)
	cm := &countingManager{inner: fixedEndpointManager{endpoint: child.URL}}
	srv := httptest.NewServer(newRouter(cm))
	defer srv.Close()

	// Stream the upload through a pipe so it is sent chunked and the response
	// can be read while the body is still being produced -- the full-duplex
	// shape this route exists to carry.
	pr, pw := io.Pipe()
	go func() {
		_, _ = pw.Write([]byte("chunk-1"))
		_, _ = pw.Write([]byte("chunk-2"))
		_ = pw.Close()
	}()

	target := srv.URL + "/live/whisper-1?sample_rate=16000&channels=1&sample_format=s16le&language=de"
	req, err := http.NewRequest(http.MethodPost, target, pr)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/octet-stream")

	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck // read-side close, best-effort

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}

	relayed, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read relayed SSE: %v", err)
	}
	for _, want := range []string{`"partial":"hel"`, `"partial":"hello world"`, "data: [DONE]"} {
		if !strings.Contains(string(relayed), want) {
			t.Fatalf("relayed SSE %q does not contain %q", relayed, want)
		}
	}

	var got liveCapture
	select {
	case got = <-capCh:
	case <-time.After(3 * time.Second):
		t.Fatal("child never reported what it received")
	}

	if got.path != "/v1/audio/transcriptions/live" {
		t.Errorf("child path = %q, want /v1/audio/transcriptions/live", got.path)
	}
	if got.model != "whisper-1" {
		t.Errorf("child query model = %q, want whisper-1 (taken from the path)", got.model)
	}
	if got.sampleRate != "16000" || got.channels != "1" || got.sampleFormat != "s16le" || got.language != "de" {
		t.Errorf("forwarded params = {sample_rate:%q channels:%q sample_format:%q language:%q}, want {16000 1 s16le de}",
			got.sampleRate, got.channels, got.sampleFormat, got.language)
	}
	if got.bodyLen != len("chunk-1chunk-2") {
		t.Errorf("child read %d body bytes, want %d -- the streamed upload did not arrive intact", got.bodyLen, len("chunk-1chunk-2"))
	}

	assertReleasedExactlyOnce(t, cm, 3*time.Second, 200*time.Millisecond)
}

// ensureErrManager is a managerPort whose EnsureRunning always fails with a
// fixed sentinel -- the "managed set does not contain this model" case the
// live route must map to the same 404 serveEnsure gives.
type ensureErrManager struct{ err error }

func (e ensureErrManager) EnsureRunning(context.Context, string) (string, func(), error) {
	return "", nil, e.err
}
func (e ensureErrManager) LoadedModels() []string { return nil }
func (e ensureErrManager) Status() []Status       { return nil }

// TestRouterLiveUnknownOrEmptyModelReturns404 covers the three ways POST
// /live/{model} has no model to route on: a nil manager, an empty path model
// (POST /live/), and a model EnsureRunning reports as not managed. Each is 404
// runtime.model_not_managed, exactly like serveEnsure, and is routed through
// the real ServeHTTP dispatch.
func TestRouterLiveUnknownOrEmptyModelReturns404(t *testing.T) {
	cases := []struct {
		name string
		rt   *router
		path string
	}{
		{"nil_manager", newRouter(nil), "/live/whisper-1"},
		{"empty_model", newRouter(fixedEndpointManager{endpoint: "http://127.0.0.1:1"}), "/live/"},
		{"model_not_managed", newRouter(ensureErrManager{err: ErrModelNotManaged}), "/live/ghost"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			tc.rt.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader("")))
			if rec.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404 (body %q)", rec.Code, rec.Body.String())
			}
			if code := decodeErrorCode(t, rec); code != codeModelNotManaged {
				t.Fatalf("error code = %q, want %q", code, codeModelNotManaged)
			}
		})
	}
}

// TestRouterLiveUpstreamErrorMapsUpstreamGone: a RoundTrip failure (the child
// is unreachable) is reported as 502 runtime.upstream_gone, the same code the
// other proxy paths give for "something went wrong reaching an admitted
// child", and the admission is still released exactly once.
func TestRouterLiveUpstreamErrorMapsUpstreamGone(t *testing.T) {
	// A server closed immediately leaves a dead address RoundTrip cannot reach.
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL
	dead.Close()

	cm := &countingManager{inner: fixedEndpointManager{endpoint: deadURL}}
	rec := httptest.NewRecorder()
	newRouter(cm).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/live/whisper-1", strings.NewReader("hello")))

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (body %q)", rec.Code, rec.Body.String())
	}
	if code := decodeErrorCode(t, rec); code != codeUpstreamGone {
		t.Fatalf("error code = %q, want %q", code, codeUpstreamGone)
	}
	assertReleasedExactlyOnce(t, cm, 3*time.Second, 100*time.Millisecond)
}
