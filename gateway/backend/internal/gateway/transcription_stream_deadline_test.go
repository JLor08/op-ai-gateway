// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The deadline policy of a streamed transcription relay. A stream is watched by an idle
// watchdog (OP_AI_GATEWAY_STREAM_IDLE_TIMEOUT, srv.streamIdleTimeout), which fits a
// stream that is already flowing. A transcription's FIRST frame is another matter: a
// streaming model's time to first token -- a cold start, a model still loading -- can
// exceed any sensible idle timeout, so until the first byte arrives the relay gets the
// application's whole timeout (the budget a one-shot request has end to end) and only
// from then on the idle cadence. These tests drive a real front over a stub audio.cpp
// that holds its stream back, with an idle timeout far below the wait.

// postTranscriptionFront posts a bearer-authenticated multipart transcription to a real
// listening front and returns the response, for the tests that need the stream's timing
// on a real connection.
func postTranscriptionFront(t *testing.T, ctx context.Context, front *httptest.Server, parts []editPart) *http.Response {
	t.Helper()
	body, ct := buildEditMultipart(t, parts)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, front.URL+"/v1/audio/transcriptions", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", ct)
	req.Header.Set("Authorization", "Bearer dev-secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /v1/audio/transcriptions: %v", err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// newStallingTranscriptionUpstream starts an audio.cpp stub that answers 200 under
// contentType with prefix written and flushed, and then holds the response open until the
// request ends (cut reports that it did, which is the relay cancelling it) or 5 seconds
// pass.
func newStallingTranscriptionUpstream(t *testing.T, contentType, prefix string, cut *atomic.Bool) *httptest.Server {
	t.Helper()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", contentType)
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, prefix)
		w.(http.Flusher).Flush()
		select {
		case <-time.After(5 * time.Second):
		case <-r.Context().Done():
			cut.Store(true)
		}
	}))
	t.Cleanup(up.Close)
	return up
}

// A first frame that comes later than the idle timeout is NOT cut: the stub answers the
// event stream's headers at once and holds the first frame back for 3.5x the idle
// timeout. The whole stream reaches the client and the call is recorded a success.
func TestTranscriptionStreamWaitsForAFirstFrameBeyondTheIdleTimeout(t *testing.T) {
	up, _ := newScriptedTranscriptionUpstream(t, func(_ int, w http.ResponseWriter, call transcriptionUpstreamCall) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		flusher.Flush()
		time.Sleep(350 * time.Millisecond)
		_, _ = io.WriteString(w, transcriptionSSEDelta+transcriptionSSEDone+transcriptionSSEEnd)
		flusher.Flush()
	})
	srv, _ := newTranscriptionCapableTestServer(t, up.URL)
	srv.streamIdleTimeout = 100 * time.Millisecond
	front := httptest.NewServer(srv)
	t.Cleanup(front.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	resp := postTranscriptionFront(t, ctx, front, streamingTranscriptionParts(transcriptionGatewayModel))
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading the stream: %v", err)
	}

	if want := transcriptionSSEDelta + transcriptionSSEDone + transcriptionSSEEnd; string(got) != want {
		t.Fatalf("stream = %q, want the whole of it %q: a slow first frame is not an idle stream", got, want)
	}
	ev := onlyUsageEvent(t, srv)
	if ev.Status != "success" || ev.ErrorCode != "" {
		t.Fatalf("usage row = %q/%q, want a success: the idle timeout must not fire before the first byte", ev.Status, ev.ErrorCode)
	}
}

// The grace ends at the first byte: once a frame has arrived, silence longer than the
// idle timeout ends the stream with provider.stream_idle_timeout, as it always has.
func TestTranscriptionStreamIdleTimeoutAppliesAfterTheFirstFrame(t *testing.T) {
	var cutByRelay atomic.Bool
	up := newStallingTranscriptionUpstream(t, "text/event-stream", transcriptionSSEDelta, &cutByRelay)
	srv, _ := newTranscriptionCapableTestServer(t, up.URL)
	srv.streamIdleTimeout = 100 * time.Millisecond
	front := httptest.NewServer(srv)
	t.Cleanup(front.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	start := time.Now()
	resp := postTranscriptionFront(t, ctx, front, streamingTranscriptionParts(transcriptionGatewayModel))
	got, _ := io.ReadAll(resp.Body)

	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("the stream took %v to end, want it cut at ~the 100ms idle timeout", elapsed)
	}
	if string(got) != transcriptionSSEDelta {
		t.Fatalf("stream = %q, want just the one frame that was sent before the silence", got)
	}
	ev := onlyUsageEvent(t, srv)
	if ev.Status != "error" || ev.ErrorCode != "provider.stream_idle_timeout" {
		t.Fatalf("usage row = %q/%q, want error/provider.stream_idle_timeout", ev.Status, ev.ErrorCode)
	}
	if !cutByRelay.Load() {
		t.Fatal("the relay did not cancel the upstream request when the stream went idle")
	}
}

// A stream request the upstream answers with a plain JSON document is relayed buffered,
// and the watchdog armed for the stream is stopped -- which must not leave that body
// read unbounded: it gets the application's total timeout, as any buffered transcription
// does. The stub answers 200 JSON headers and then never finishes the body; the
// application's timeout is 300ms.
func TestTranscriptionStreamAnsweredWithJSONIsBoundedByTheTotalTimeout(t *testing.T) {
	var cutByRelay atomic.Bool
	up := newStallingTranscriptionUpstream(t, "application/json", "", &cutByRelay)
	srv, _ := newTranscriptionCapableTestServer(t, up.URL)
	srv.streamIdleTimeout = 100 * time.Millisecond
	app, err := srv.Routes.ApplicationByID(context.Background(), "app-audio")
	if err != nil {
		t.Fatalf("ApplicationByID: %v", err)
	}
	app.TimeoutMS = 300
	if err := srv.Routes.UpdateApplication(context.Background(), app); err != nil {
		t.Fatalf("UpdateApplication: %v", err)
	}
	front := httptest.NewServer(srv)
	t.Cleanup(front.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	start := time.Now()
	resp := postTranscriptionFront(t, ctx, front, streamingTranscriptionParts(transcriptionGatewayModel))
	body, _ := io.ReadAll(resp.Body)

	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("the relay took %v, want it bounded by the application's 300ms timeout", elapsed)
	}
	if resp.StatusCode != http.StatusBadGateway || !strings.Contains(string(body), "transcription.upstream_error") {
		t.Fatalf("response = %d %s, want the relay's 502 transcription.upstream_error for a body that never finished", resp.StatusCode, body)
	}
	if !cutByRelay.Load() {
		t.Fatal("the relay did not cancel the upstream request at its total timeout")
	}
}

// A client may spell a boolean stream any way strconv.ParseBool reads, and the relay
// serves them all as streams -- but audio.cpp is handed only the literal true: a
// spelling it does not recognise ("1", "TRUE") would be ignored and the call answered
// one-shot, under the idle watchdog instead of the grace a stream gets. The canonical
// spelling is relayed as it came (no rebuild of the client's body); any other is
// replaced, and nothing else in the form changes.
func TestTranscriptionStreamForwardsTheLiteralTrue(t *testing.T) {
	for _, tc := range []struct {
		name, spelling string
		verbatim       bool
	}{
		{"the canonical spelling is not rewritten", "true", true},
		{"1", "1", false},
		{"t", "t", false},
		{"T", "T", false},
		{"TRUE", "TRUE", false},
		{"True", "True", false},
		{"padded", " true ", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up, calls := newSSETranscriptionUpstream(t, transcriptionSSEDelta, transcriptionSSEDone, transcriptionSSEEnd)
			srv, _ := newTranscriptionCapableTestServer(t, up.URL)
			// asr-same maps to an upstream model of the same name, so the form needs no model
			// rewrite and the canonical spelling is relayed verbatim.
			parts := append(validTranscriptionParts("asr-same"), editPart{name: "stream", body: []byte(tc.spelling)})
			sent, sentType := buildEditMultipart(t, parts)

			rec := postEditRaw(t, srv, http.MethodPost, "/v1/audio/transcriptions", sent, sentType, map[string]string{"Authorization": "Bearer dev-secret"})

			if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "text/event-stream" {
				t.Fatalf("response = %d %q, want the relayed event stream", rec.Code, rec.Header().Get("Content-Type"))
			}
			all := drainTranscriptionUpstreamCalls(calls)
			if len(all) != 1 {
				t.Fatalf("upstream calls = %d, want 1", len(all))
			}
			if v, ok := formFieldOf(t, all[0].body, all[0].contentType, "stream"); !ok || v != "true" {
				t.Fatalf("upstream stream field = %q (present %v), want the literal true", v, ok)
			}
			if tc.verbatim != (bytes.Equal(all[0].body, sent) && all[0].contentType == sentType) {
				t.Fatalf("the form was relayed verbatim = %v, want %v", !tc.verbatim, tc.verbatim)
			}
			requireSameParts(t, readEditParts(t, all[0].body, all[0].contentType), []editPart{
				{name: "model", body: []byte("asr-same")},
				validTranscriptionParts("asr-same")[1],
				{name: "stream", body: []byte("true")},
			})
			if !onlyUsageEvent(t, srv).Stream {
				t.Fatal("usage row Stream = false, want true for a streamed call")
			}
		})
	}
}
