// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"op-ai-gateway/internal/provider"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/usage"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The wire format audio.cpp streams a transcription in: OpenAI-style SSE, one
// transcript.text.delta per increment, a transcript.text.done carrying the full text,
// then the [DONE] sentinel. The gateway relays it byte for byte, so the fixtures are
// the exact frames a client must receive.
const (
	transcriptionSSEDelta = "data: {\"type\":\"transcript.text.delta\",\"delta\":\"hello there\"}\n\n"
	transcriptionSSEDone  = "data: {\"type\":\"transcript.text.done\",\"text\":\"hello there\",\"timing\":{\"ttft_ms\":12}}\n\n"
	transcriptionSSEEnd   = "data: [DONE]\n\n"

	// transcriptionModeMismatchBody is what audio.cpp answers (HTTP 500, as it does for
	// every error) when stream=true reaches a model that is not loaded with
	// mode=streaming: the gateway's cue to retry the request buffered.
	transcriptionModeMismatchBody = `{"error":{"message":"transcription streaming requires a model configured with mode=streaming","type":"server_error"}}`
)

// newScriptedTranscriptionUpstream is newTranscriptionUpstream with the answer
// decided per call: answer gets the 1-based call number and the request it answers,
// and writes the response itself, so a stub can stream, stall, or answer differently
// the second time. Every request is recorded, as the plain stub records them.
func newScriptedTranscriptionUpstream(t *testing.T, answer func(n int, w http.ResponseWriter, call transcriptionUpstreamCall)) (*httptest.Server, <-chan transcriptionUpstreamCall) {
	t.Helper()
	calls := make(chan transcriptionUpstreamCall, 16)
	var n atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		call := transcriptionUpstreamCall{method: r.Method, path: r.URL.Path, contentType: r.Header.Get("Content-Type"), body: body}
		calls <- call
		answer(int(n.Add(1)), w, call)
	}))
	t.Cleanup(up.Close)
	return up, calls
}

// drainTranscriptionUpstreamCalls returns every request the stub has seen so far, in
// arrival order. The stub records a request before it answers it, so once the client
// has its whole response the list is complete.
func drainTranscriptionUpstreamCalls(calls <-chan transcriptionUpstreamCall) []transcriptionUpstreamCall {
	var out []transcriptionUpstreamCall
	for {
		select {
		case c := <-calls:
			out = append(out, c)
		default:
			return out
		}
	}
}

// formFieldOf returns the value of the named part of a multipart body, and whether
// the body has such a part at all: an absent field and an empty one are different
// facts when the question is whether `stream` was sent upstream.
func formFieldOf(t *testing.T, body []byte, contentType, name string) (string, bool) {
	t.Helper()
	for _, p := range readEditParts(t, body, contentType) {
		if p.name == name {
			return string(p.body), true
		}
	}
	return "", false
}

// streamingTranscriptionParts is a valid transcription request for model that asks
// for a streamed answer.
func streamingTranscriptionParts(model string) []editPart {
	return append(validTranscriptionParts(model), editPart{name: "stream", body: []byte("true")})
}

// readSSEFrame reads one SSE frame (up to and including its blank line) from br.
func readSSEFrame(t *testing.T, br *bufio.Reader) string {
	t.Helper()
	var frame strings.Builder
	for {
		line, err := br.ReadString('\n')
		frame.WriteString(line)
		if err != nil {
			t.Fatalf("reading an SSE frame: %v (got %q so far)", err, frame.String())
		}
		if line == "\n" {
			return frame.String()
		}
	}
}

// A streamed transcription is relayed UNBUFFERED: the client gets the upstream's own
// event stream, frame for frame, as it is produced. The stub stalls after its first
// frame until the client has read it, so a relay that collected the body before
// answering cannot pass: the client would never see the frame the stub is holding the
// rest of the stream behind.
func TestTranscriptionStreamRelayPassesSSEThrough(t *testing.T) {
	release := make(chan struct{})
	var stalled atomic.Bool
	up, calls := newScriptedTranscriptionUpstream(t, func(_ int, w http.ResponseWriter, _ transcriptionUpstreamCall) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		_, _ = io.WriteString(w, transcriptionSSEDelta)
		flusher.Flush()
		select {
		case <-release:
		case <-time.After(5 * time.Second):
			stalled.Store(true)
		}
		_, _ = io.WriteString(w, transcriptionSSEDone+transcriptionSSEEnd)
		flusher.Flush()
	})
	srv, _ := newTranscriptionCapableTestServer(t, up.URL)
	front := httptest.NewServer(srv)
	t.Cleanup(front.Close)

	body, ct := buildEditMultipart(t, streamingTranscriptionParts(transcriptionGatewayModel))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
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
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("Content-Type = %q, want the upstream's text/event-stream", got)
	}
	br := bufio.NewReader(resp.Body)
	if first := readSSEFrame(t, br); first != transcriptionSSEDelta {
		t.Fatalf("first frame = %q, want the delta frame %q", first, transcriptionSSEDelta)
	}
	close(release) // the client has the first frame while the upstream is still holding the rest
	rest, err := io.ReadAll(br)
	if err != nil {
		t.Fatalf("reading the rest of the stream: %v", err)
	}
	if string(rest) != transcriptionSSEDone+transcriptionSSEEnd {
		t.Fatalf("rest of the stream = %q, want the done frame and [DONE], in order", rest)
	}
	if stalled.Load() {
		t.Fatal("the upstream finished its stream before the client had its first frame: the relay buffered it")
	}

	all := drainTranscriptionUpstreamCalls(calls)
	if len(all) != 1 {
		t.Fatalf("upstream calls = %d, want 1: a stream that works is not retried", len(all))
	}
	if all[0].path != "/v1/audio/transcriptions" {
		t.Fatalf("upstream path = %q, want /v1/audio/transcriptions", all[0].path)
	}
	if v, ok := formFieldOf(t, all[0].body, all[0].contentType, "stream"); !ok || v != "true" {
		t.Fatalf("upstream stream field = %q (present %v), want the client's true relayed", v, ok)
	}
	if got := modelFieldOf(t, all[0].body, all[0].contentType); got != transcriptionUpstreamModel {
		t.Fatalf("upstream model = %q, want the mapped %q", got, transcriptionUpstreamModel)
	}

	ev := onlyUsageEvent(t, srv)
	if ev.Status != "success" || ev.HTTPStatus != http.StatusOK || ev.ContentType != "text/event-stream" {
		t.Fatalf("usage row = %q/%d/%q, want success/200/text/event-stream", ev.Status, ev.HTTPStatus, ev.ContentType)
	}
	if !ev.Stream {
		t.Fatal("usage row Stream = false, want true: the call was streamed")
	}
	if ev.ProviderPath != "/v1/audio/transcriptions" || ev.BillingUnit != usage.BillingUnitAudioSecond {
		t.Fatalf("usage row = %q in %q, want /v1/audio/transcriptions in audio_second", ev.ProviderPath, ev.BillingUnit)
	}
	// The upload of this fixture is no WAV, so there is no duration to bill: the loud
	// zero. (The billing from a WAV upload is TestTranscriptionStreamBillsFromWavHeader's.)
	if ev.BillingQuantity != 0 {
		t.Fatalf("BillingQuantity = %v, want the loud zero of an upload that is no WAV", ev.BillingQuantity)
	}
}

// stream=true against a model that is not in streaming mode: audio.cpp answers 500
// "requires a model configured with mode=streaming". The gateway holds the whole
// upload already, so it sends it ONCE more with the stream field dropped and the
// client gets the one-shot answer, billed like any buffered transcription. The mapped
// name rewrite and the empty-file strip apply to the retry as they do to the first
// attempt; the first attempt leaves no row of its own.
func TestTranscriptionStreamFallsBackToBufferedOn500(t *testing.T) {
	for _, tc := range []struct {
		name  string
		model string
		// wantFirstVerbatim: the first attempt is the client's own bytes (the form
		// already carries the mapped name), so only the retry is a rebuild.
		wantFirstVerbatim bool
		wantUpstreamModel string
	}{
		{"model rewritten", transcriptionGatewayModel, false, transcriptionUpstreamModel},
		{"form already carries the mapped name", "asr-same", true, "asr-same"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// How many requests the gateway shows as in flight while each upstream call is
			// answered: the refused attempt must be gone from the active list before its
			// retry runs, or one transcription would count against its server twice.
			var gateway atomic.Pointer[Server]
			var activeAtCall [2]atomic.Int32
			up, calls := newScriptedTranscriptionUpstream(t, func(n int, w http.ResponseWriter, _ transcriptionUpstreamCall) {
				w.Header().Set("Content-Type", "application/json")
				if n <= len(activeAtCall) {
					activeAtCall[n-1].Store(int32(len(gateway.Load().Active.Snapshot())))
				}
				if n == 1 {
					w.WriteHeader(http.StatusInternalServerError)
					_, _ = io.WriteString(w, transcriptionModeMismatchBody)
					return
				}
				_, _ = io.WriteString(w, transcriptionOKBody)
			})
			srv, _ := newTranscriptionCapableTestServer(t, up.URL)
			gateway.Store(srv)
			parts := streamingTranscriptionParts(tc.model)
			body, ct := buildEditMultipart(t, parts)

			rec := postEditRaw(t, srv, http.MethodPost, "/v1/audio/transcriptions", body, ct, map[string]string{"Authorization": "Bearer dev-secret"})

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 from the buffered retry; body = %s", rec.Code, rec.Body.String())
			}
			if got := rec.Header().Get("Content-Type"); got != "application/json" {
				t.Fatalf("Content-Type = %q, want application/json (the one-shot answer, not an event stream)", got)
			}
			if rec.Body.String() != transcriptionOKBody {
				t.Fatalf("body = %s, want the one-shot document byte for byte", rec.Body.String())
			}

			all := drainTranscriptionUpstreamCalls(calls)
			if len(all) != 2 {
				t.Fatalf("upstream calls = %d, want exactly 2 (the streamed attempt, then one buffered retry)", len(all))
			}
			first, second := all[0], all[1]
			if v, ok := formFieldOf(t, first.body, first.contentType, "stream"); !ok || v != "true" {
				t.Fatalf("first call stream field = %q (present %v), want true", v, ok)
			}
			if tc.wantFirstVerbatim && (first.contentType != ct || !bytes.Equal(first.body, body)) {
				t.Fatal("first call differs from the client's body, want it relayed verbatim")
			}
			if _, ok := formFieldOf(t, second.body, second.contentType, "stream"); ok {
				t.Fatal("retry still carries a stream field, want it dropped")
			}
			if second.path != "/v1/audio/transcriptions" {
				t.Fatalf("retry path = %q, want /v1/audio/transcriptions", second.path)
			}
			mediaType, params, err := mime.ParseMediaType(second.contentType)
			if err != nil || mediaType != "multipart/form-data" || params["boundary"] == "" {
				t.Fatalf("retry Content-Type = %q (%v), want multipart/form-data with a boundary", second.contentType, err)
			}
			requireSameParts(t, readEditParts(t, second.body, second.contentType), []editPart{
				{name: "model", body: []byte(tc.wantUpstreamModel)},
				parts[1], // the audio, byte for byte
			})

			if first, second := activeAtCall[0].Load(), activeAtCall[1].Load(); first != 1 || second != 1 {
				t.Fatalf("active requests while the upstream answered = %d, then %d, want 1 each: the refused attempt must leave the active list before its retry", first, second)
			}
			events := srv.Usage.All()
			if len(events) != 1 {
				t.Fatalf("usage rows = %d, want 1: the refused streamed attempt is not a request of its own", len(events))
			}
			ev := events[0]
			if ev.Status != "success" || ev.HTTPStatus != http.StatusOK || ev.Stream {
				t.Fatalf("usage row = %q/%d stream=%v, want a buffered success", ev.Status, ev.HTTPStatus, ev.Stream)
			}
			if ev.BillingUnit != usage.BillingUnitAudioSecond || ev.BillingQuantity != 2 {
				t.Fatalf("billing = %v %q, want 2 audio_second (timing.audio_duration_ms of the one-shot body)", ev.BillingQuantity, ev.BillingUnit)
			}
		})
	}
}

// Only the mode-mismatch 500 of a STREAMED request is turned into a retry. Every
// other upstream answer -- a different 500, the same words under another status, a
// plain-string error -- reaches the client as it does today, from ONE upstream call.
func TestTranscriptionStreamSurfacesOtherUpstreamErrors(t *testing.T) {
	const mismatchWords = "requires a model configured with mode=streaming"
	for _, tc := range []struct {
		name       string
		status     int
		respBody   string
		wantInBody string
	}{
		{"another 500", http.StatusInternalServerError, `{"error":{"message":"out of memory","type":"server_error"}}`, "out of memory"},
		{"a plain-string 500", http.StatusInternalServerError, `{"error":"model crashed"}`, "model crashed"},
		{"the mismatch words under a 400", http.StatusBadRequest, `{"error":{"message":"` + mismatchWords + `","type":"invalid_request_error"}}`, mismatchWords},
		{"the mismatch words under a 503", http.StatusServiceUnavailable, `{"error":{"message":"` + mismatchWords + `","type":"server_error"}}`, mismatchWords},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up, calls := newTranscriptionUpstream(t, tc.status, tc.respBody)
			srv, _ := newTranscriptionCapableTestServer(t, up.URL)

			rec := postTranscriptionParts(t, srv, streamingTranscriptionParts(transcriptionGatewayModel))

			if rec.Code != tc.status {
				t.Fatalf("status = %d, want the upstream's %d; body = %s", rec.Code, tc.status, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), tc.wantInBody) {
				t.Fatalf("body = %s, want it to carry the upstream's %q", rec.Body.String(), tc.wantInBody)
			}
			if all := drainTranscriptionUpstreamCalls(calls); len(all) != 1 {
				t.Fatalf("upstream calls = %d, want exactly 1: only the mode-mismatch 500 is retried", len(all))
			}
			ev := lastUsageEvent(t, srv)
			if ev.Status != "error" || ev.HTTPStatus != tc.status || ev.BillingUnit != usage.BillingUnitAudioSecond {
				t.Fatalf("usage row = %q/%d in %q, want an error row at the upstream's status in audio_second", ev.Status, ev.HTTPStatus, ev.BillingUnit)
			}
		})
	}
}

// The mismatch cue is looked for in the head of an error body only; a body that is
// not the cue is still relayed WHOLE, including what lies past the part that was
// looked at.
func TestTranscriptionStreamRelaysALargeNonMismatchErrorBodyWhole(t *testing.T) {
	respBody := `{"error":{"message":"` + strings.Repeat("x", 100<<10) + `","type":"server_error"}}`
	up, calls := newTranscriptionUpstream(t, http.StatusInternalServerError, respBody)
	srv, _ := newTranscriptionCapableTestServer(t, up.URL)

	rec := postTranscriptionParts(t, srv, streamingTranscriptionParts(transcriptionGatewayModel))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want the upstream's 500", rec.Code)
	}
	if rec.Body.String() != respBody {
		t.Fatalf("body is %d bytes, want the upstream's %d bytes unchanged", rec.Body.Len(), len(respBody))
	}
	if all := drainTranscriptionUpstreamCalls(calls); len(all) != 1 {
		t.Fatalf("upstream calls = %d, want 1", len(all))
	}
}

// The retry exists only for a request that ASKED to stream. A plain transcription
// the upstream answers with the very same 500 is surfaced, not repeated.
func TestTranscriptionWithoutStreamIsNeverRetried(t *testing.T) {
	up, calls := newTranscriptionUpstream(t, http.StatusInternalServerError, transcriptionModeMismatchBody)
	srv, _ := newTranscriptionCapableTestServer(t, up.URL)

	rec := postTranscriptionParts(t, srv, validTranscriptionParts(transcriptionGatewayModel))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want the upstream's 500; body = %s", rec.Code, rec.Body.String())
	}
	if all := drainTranscriptionUpstreamCalls(calls); len(all) != 1 {
		t.Fatalf("upstream calls = %d, want exactly 1: nothing was streamed, so nothing is retried", len(all))
	}
	if ev := lastUsageEvent(t, srv); ev.Stream {
		t.Fatal("usage row Stream = true for a request that never asked to stream")
	}
}

// An upstream that answers a stream=true request with a plain 2xx JSON document (it
// did not stream) is relayed like any buffered transcription: verbatim, and billed
// from its timing block.
func TestTranscriptionStreamAnsweredWithJSONIsRelayedBuffered(t *testing.T) {
	up, calls := newTranscriptionUpstream(t, http.StatusOK, transcriptionOKBody)
	srv, _ := newTranscriptionCapableTestServer(t, up.URL)

	rec := postTranscriptionParts(t, srv, streamingTranscriptionParts(transcriptionGatewayModel))

	if rec.Code != http.StatusOK || rec.Body.String() != transcriptionOKBody {
		t.Fatalf("response = %d %s, want the upstream's 200 document verbatim", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", got)
	}
	if all := drainTranscriptionUpstreamCalls(calls); len(all) != 1 {
		t.Fatalf("upstream calls = %d, want 1", len(all))
	}
	ev := lastUsageEvent(t, srv)
	if ev.Status != "success" || ev.BillingQuantity != 2 {
		t.Fatalf("usage row = %q billing %v, want success billed 2 audio seconds from timing.audio_duration_ms", ev.Status, ev.BillingQuantity)
	}
}

// stream=true is the one stream request the endpoint now serves. A stream that is no
// boolean, and ANY stream_format, are still refused 400 before routing: the gateway
// does not translate audio.cpp's event shape into another one.
func TestTranscriptionStreamFormatStillRefused(t *testing.T) {
	for _, tc := range []struct {
		name  string
		extra []editPart
	}{
		{"stream_format with stream", []editPart{{name: "stream", body: []byte("true")}, {name: "stream_format", body: []byte("sse")}}},
		{"stream_format alone", []editPart{{name: "stream_format", body: []byte("sse")}}},
		{"stream_format with stream false", []editPart{{name: "stream", body: []byte("false")}, {name: "stream_format", body: []byte("sse")}}},
		{"a stream that is no boolean", []editPart{{name: "stream", body: []byte("maybe")}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up, calls := newTranscriptionUpstream(t, http.StatusOK, transcriptionOKBody)
			srv, _ := newTranscriptionCapableTestServer(t, up.URL)

			rec := postTranscriptionParts(t, srv, append(validTranscriptionParts(transcriptionGatewayModel), tc.extra...))

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
			}
			requireErrorCode(t, rec.Body.String(), "transcription.stream_unsupported")
			requireNoTranscriptionUpstreamCall(t, calls)
			if events := srv.Usage.All(); len(events) != 0 {
				t.Fatalf("a refused request recorded usage %+v, want none", events)
			}
		})
	}
}

// The refusal is told from a server that is really failing by its sentence, in a 500
// and nowhere else; and looking for it must not cost the body: whatever was read comes
// back out in front of the rest.
func TestPeekStreamingRefusal(t *testing.T) {
	long := `{"error":{"message":"` + strings.Repeat("x", transcriptionRefusalPeekBytes+10) + `"}}`
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		{"the refusal", http.StatusInternalServerError, transcriptionModeMismatchBody, true},
		{"the sentence in a plain-string error", http.StatusInternalServerError, `{"error":"requires a model configured with mode=streaming"}`, true},
		{"another 500", http.StatusInternalServerError, `{"error":{"message":"out of memory"}}`, false},
		{"an empty 500", http.StatusInternalServerError, ``, false},
		{"a 500 body longer than the peek", http.StatusInternalServerError, long, false},
		{"the refusal under a 400", http.StatusBadRequest, transcriptionModeMismatchBody, false},
		{"the refusal under a 200", http.StatusOK, transcriptionModeMismatchBody, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := &provider.ProxyResponse{StatusCode: tc.status, Body: io.NopCloser(strings.NewReader(tc.body))}

			if got := peekStreamingRefusal(resp); got != tc.want {
				t.Fatalf("peekStreamingRefusal = %v, want %v", got, tc.want)
			}
			rest, err := io.ReadAll(resp.Body)
			if err != nil || string(rest) != tc.body {
				t.Fatalf("body after the look = %d bytes (%v), want all %d bytes of the original", len(rest), err, len(tc.body))
			}
		})
	}
}

// The buffered twin of a streamed request: the client's form without its stream part,
// under the mapped model name, in a body of its own boundary -- and held to the agent
// router's request limit, which an external target is not.
func TestBufferedTranscriptionBody(t *testing.T) {
	parts := []editPart{
		{name: "model", body: []byte("gw")},
		{name: "stream", body: []byte("true")},
		{name: "file", filename: "clip.wav", contentType: "audio/wav", body: transcriptionTestAudio},
		{name: "file", filename: "empty.wav", contentType: "audio/wav"},
		{name: "language", body: []byte("de")},
	}
	body, ct := buildEditMultipart(t, parts)
	form, err := parseTranscriptionForm(ct, body)
	if err != nil {
		t.Fatalf("parseTranscriptionForm: %v", err)
	}

	t.Run("drops stream and the empty file, applies the model", func(t *testing.T) {
		out, outCT, err := bufferedTranscriptionBody(routing.Target{Provider: routing.ProviderAudioCpp}, body, form.boundary, "mapped")
		if err != nil {
			t.Fatalf("bufferedTranscriptionBody: %v", err)
		}
		if outCT == ct {
			t.Fatalf("Content-Type = %q is the client's, want the rebuilt body's own boundary", outCT)
		}
		requireSameParts(t, readEditParts(t, out, outCT), []editPart{
			{name: "model", body: []byte("mapped")},
			parts[2],
			parts[4],
		})
	})
	t.Run("keeps the model as sent when no name is given", func(t *testing.T) {
		out, outCT, err := bufferedTranscriptionBody(routing.Target{Provider: routing.ProviderAudioCpp}, body, form.boundary, "")
		if err != nil {
			t.Fatalf("bufferedTranscriptionBody: %v", err)
		}
		if got := modelFieldOf(t, out, outCT); got != "gw" {
			t.Fatalf("model = %q, want the client's gw", got)
		}
	})
	t.Run("a body that is no form is an error", func(t *testing.T) {
		if _, _, err := bufferedTranscriptionBody(routing.Target{}, body[:len(body)-40], form.boundary, ""); err == nil {
			t.Fatal("bufferedTranscriptionBody of a truncated body succeeded, want an error")
		}
	})
	t.Run("an agent target is held to the router's limit", func(t *testing.T) {
		big := shortBoundaryTranscriptionBody(t, "xx", agentRouterMaxBodyBytes)
		form, err := parseTranscriptionForm(big.contentType, big.body)
		if err != nil {
			t.Fatalf("parseTranscriptionForm: %v", err)
		}

		_, _, err = bufferedTranscriptionBody(routing.Target{Provider: routing.ProviderServerAgent}, big.body, form.boundary, "")
		if !errors.Is(err, errTranscriptionTooLarge) {
			t.Fatalf("err = %v, want errTranscriptionTooLarge: the rebuilt form (a longer boundary) is past the agent's limit", err)
		}
		if _, _, err := bufferedTranscriptionBody(routing.Target{Provider: routing.ProviderAudioCpp}, big.body, form.boundary, ""); err != nil {
			t.Fatalf("an external target was refused: %v", err)
		}
	})
}

type formBody struct {
	body        []byte
	contentType string
}

// shortBoundaryTranscriptionBody builds a streamed transcription form (a model, a
// stream and one padded audio part) of exactly size bytes under the given boundary.
// A short one is the case a rebuild GROWS a form: multipart.Writer's own is 60 bytes.
func shortBoundaryTranscriptionBody(t *testing.T, boundary string, size int) formBody {
	t.Helper()
	build := func(n int) formBody {
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		if err := mw.SetBoundary(boundary); err != nil {
			t.Fatalf("SetBoundary: %v", err)
		}
		for _, f := range []struct{ name, filename, value string }{{"model", "", "agent-asr"}, {"stream", "", "true"}} {
			if err := mw.WriteField(f.name, f.value); err != nil {
				t.Fatalf("WriteField %s: %v", f.name, err)
			}
		}
		h := textproto.MIMEHeader{}
		h.Set("Content-Disposition", `form-data; name="file"; filename="long.wav"`)
		h.Set("Content-Type", "audio/wav")
		pw, err := mw.CreatePart(h)
		if err != nil {
			t.Fatalf("CreatePart: %v", err)
		}
		if _, err := pw.Write(bytes.Repeat([]byte{0xAB}, n)); err != nil {
			t.Fatalf("write the audio part: %v", err)
		}
		if err := mw.Close(); err != nil {
			t.Fatalf("close: %v", err)
		}
		return formBody{body: buf.Bytes(), contentType: mw.FormDataContentType()}
	}
	probe := build(0)
	out := build(size - len(probe.body))
	if len(out.body) != size {
		t.Fatalf("built a %d-byte form, want exactly %d", len(out.body), size)
	}
	return out
}

// streamRefusingProvider answers every native call with the upstream's refusal to
// stream, so what the gateway does with it can be observed on a target audio.cpp's
// HTTP stub cannot be (an agent-launched one, whose body limit the retry must keep).
type streamRefusingProvider struct {
	*recordingProxyProvider
	calls int
}

func (p *streamRefusingProvider) ProxyNative(context.Context, routing.Target, string, []byte, string) (*provider.ProxyResponse, error) {
	p.calls++
	return &provider.ProxyResponse{
		StatusCode: http.StatusInternalServerError,
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(strings.NewReader(transcriptionModeMismatchBody)),
	}, nil
}

// A retry whose buffered twin cannot be built (here: past the agent router's limit,
// the first attempt having just fitted) is not sent. The client gets the upstream's
// own answer, as it would for any other error, from the ONE call that was made.
func TestTranscriptionStreamRelaysTheRefusalWhenTheRetryCannotBeBuilt(t *testing.T) {
	prov := &streamRefusingProvider{recordingProxyProvider: &recordingProxyProvider{}}
	srv := newServerAgentTranscriptionTestServer(t, prov, "agent-asr", "agent-asr", []string{routing.APIFlavorOpenAIAudio})
	declareTranscriptionMultipartProxy(srv)
	// Exactly the agent's limit under a 2-byte boundary: it is relayed as it is (no
	// rewrite, no rebuild), but the buffered twin gets the rebuild's 60-byte boundaries.
	big := shortBoundaryTranscriptionBody(t, "xx", agentRouterMaxBodyBytes)

	rec := postEditRaw(t, srv, http.MethodPost, "/v1/audio/transcriptions", big.body, big.contentType, map[string]string{"Authorization": "Bearer dev-secret"})

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want the upstream's 500 relayed; body = %.200s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), transcriptionStreamingRefusal) {
		t.Fatalf("body = %s, want the upstream's refusal", rec.Body.String())
	}
	if prov.calls != 1 {
		t.Fatalf("upstream calls = %d, want 1: the retry was never sent", prov.calls)
	}
	if ev := lastUsageEvent(t, srv); ev.Status != "error" || ev.HTTPStatus != http.StatusInternalServerError || ev.ErrorCode != "upstream.500" {
		t.Fatalf("usage row = %q/%d/%q, want the upstream's 500 as an error row", ev.Status, ev.HTTPStatus, ev.ErrorCode)
	}
}

// A transcription is relayed as a stream only when it asked for one AND the upstream
// answered with an event stream.
func TestIsStreamedTranscription(t *testing.T) {
	sse := http.Header{"Content-Type": {"text/event-stream"}}
	for _, tc := range []struct {
		name      string
		endpoint  sessionEndpoint
		requested bool
		header    http.Header
		want      bool
	}{
		{"asked and answered with an event stream", endpointTranscription, true, sse, true},
		{"the media type with parameters", endpointTranscription, true, http.Header{"Content-Type": {"text/event-stream; charset=utf-8"}}, true},
		{"the media type in another case", endpointTranscription, true, http.Header{"Content-Type": {"Text/Event-Stream"}}, true},
		{"asked, answered with JSON", endpointTranscription, true, http.Header{"Content-Type": {"application/json"}}, false},
		{"asked, answered with no type", endpointTranscription, true, http.Header{}, false},
		{"not asked, answered with an event stream", endpointTranscription, false, sse, false},
		{"speech never", endpointSpeech, true, sse, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := isStreamedTranscription(tc.endpoint, tc.requested, tc.header); got != tc.want {
				t.Fatalf("isStreamedTranscription = %v, want %v", got, tc.want)
			}
		})
	}
}

// --- Billing and capture of a streamed transcription ------------------------

// newSSETranscriptionUpstream starts an audio.cpp stub that answers every call with an
// event stream made of frames, written and flushed one at a time, and then ends the
// body cleanly.
func newSSETranscriptionUpstream(t *testing.T, frames ...string) (*httptest.Server, <-chan transcriptionUpstreamCall) {
	t.Helper()
	return newScriptedTranscriptionUpstream(t, func(_ int, w http.ResponseWriter, _ transcriptionUpstreamCall) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		for _, frame := range frames {
			_, _ = io.WriteString(w, frame)
			flusher.Flush()
		}
	})
}

// streamingTranscriptionPartsOf is a valid streamed transcription request for model
// whose upload is audio.
func streamingTranscriptionPartsOf(model string, audio []byte) []editPart {
	return []editPart{
		{name: "model", body: []byte(model)},
		{name: "file", filename: "clip.wav", contentType: "audio/wav", body: audio},
		{name: "stream", body: []byte("true")},
	}
}

// onlyUsageEvent returns the usage row of a server that has recorded exactly one: a
// transcription is one row, so a second record (a retry that kept its own row, a
// double record at the end of a relay) fails the test instead of hiding behind the
// last event.
func onlyUsageEvent(t *testing.T, srv *Server) usage.Event {
	t.Helper()
	events := srv.Usage.All()
	if len(events) != 1 {
		t.Fatalf("usage rows = %d (%+v), want exactly 1", len(events), events)
	}
	return events[0]
}

// transcriptionFrameDelta and transcriptionFrameDone build the event frames audio.cpp streams, for a transcript the
// test chooses.
func transcriptionFrameDelta(text string) string {
	return "data: {\"type\":\"transcript.text.delta\",\"delta\":" + jsonString(text) + "}\n\n"
}

func transcriptionFrameDone(text string) string {
	return "data: {\"type\":\"transcript.text.done\",\"text\":" + jsonString(text) + ",\"timing\":{\"ttft_ms\":12}}\n\n"
}

func jsonString(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err) // a string always marshals
	}
	return string(b)
}

// A streamed transcription that ends cleanly -- transcript.text.done, then [DONE] --
// is billed from the duration of the WAV that was UPLOADED (the stream states none):
// the `data` chunk over the header's byte rate, in audio_second, on the one usage row.
// Neither the model name rewrite (the first case) nor an empty `file` part beside the
// upload (the third) changes which bytes are read.
func TestTranscriptionStreamBillsFromWavHeader(t *testing.T) {
	half := buildPCMWav(t, 16000, 1, 16, 16000)           // 0.5 s
	twoAndAQuarter := buildPCMWav(t, 16000, 1, 16, 72000) // 2.25 s
	for _, tc := range []struct {
		name  string
		model string
		parts func(model string, audio []byte) []editPart
		audio []byte
		want  float64
	}{
		{"half a second, model rewritten", transcriptionGatewayModel, streamingTranscriptionPartsOf, half, 0.5},
		{"two and a quarter seconds, form already carries the mapped name", "asr-same", streamingTranscriptionPartsOf, twoAndAQuarter, 2.25},
		{"an empty file part in front of the upload", transcriptionGatewayModel, func(model string, audio []byte) []editPart {
			return append([]editPart{{name: "file", filename: "empty.wav", contentType: "audio/wav"}}, streamingTranscriptionPartsOf(model, audio)...)
		}, half, 0.5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			buf, restore := withCapturedSlog(t)
			t.Cleanup(restore)
			up, _ := newSSETranscriptionUpstream(t, transcriptionFrameDelta("hello "), transcriptionFrameDelta("there"), transcriptionFrameDone("hello there"), transcriptionSSEEnd)
			srv, _ := newTranscriptionCapableTestServer(t, up.URL)

			rec := postTranscriptionParts(t, srv, tc.parts(tc.model, tc.audio))

			if want := transcriptionFrameDelta("hello ") + transcriptionFrameDelta("there") + transcriptionFrameDone("hello there") + transcriptionSSEEnd; rec.Code != http.StatusOK || rec.Body.String() != want {
				t.Fatalf("response = %d %q, want the upstream's stream, frame for frame", rec.Code, rec.Body.String())
			}
			ev := onlyUsageEvent(t, srv)
			if ev.Status != "success" || !ev.Stream {
				t.Fatalf("usage row = %q stream=%v, want a streamed success", ev.Status, ev.Stream)
			}
			if math.Abs(ev.BillingQuantity-tc.want) > 1e-9 {
				t.Fatalf("BillingQuantity = %v, want %v audio seconds from the WAV header", ev.BillingQuantity, tc.want)
			}
			requireSpeechNonTokenRow(t, ev)
			if errs := errorLogs(buf); len(errs) != 0 {
				t.Fatalf("a streamed transcription billed from its WAV logged at Error: %+v", errs)
			}
		})
	}
}

// Everything short of a clean completion bills nothing -- and says so at Error
// (ADR-041), once -- while the client still receives every frame the upstream sent:
// the done event alone, [DONE] alone, [DONE] ahead of the done event, no terminal
// event at all, and a connection torn down mid-stream.
func TestTranscriptionStreamLoudZeroOnIncompleteStream(t *testing.T) {
	wav := buildPCMWav(t, 16000, 1, 16, 16000)
	for _, tc := range []struct {
		name string
		// answer writes the upstream's (incomplete) stream.
		answer func(w http.ResponseWriter)
		// wantStatus is the usage row's status: a body that ends cleanly is a success
		// that billed nothing, one that broke off is a failed relay.
		wantStatus string
		wantBody   string
	}{
		{"deltas and then nothing", func(w http.ResponseWriter) { _, _ = io.WriteString(w, transcriptionFrameDelta("hello")) }, "success", transcriptionFrameDelta("hello")},
		{"the done event without [DONE]", func(w http.ResponseWriter) {
			_, _ = io.WriteString(w, transcriptionFrameDelta("hello")+transcriptionFrameDone("hello"))
		}, "success", transcriptionFrameDelta("hello") + transcriptionFrameDone("hello")},
		{"[DONE] without the done event", func(w http.ResponseWriter) {
			_, _ = io.WriteString(w, transcriptionFrameDelta("hello")+transcriptionSSEEnd)
		}, "success", transcriptionFrameDelta("hello") + transcriptionSSEEnd},
		{"[DONE] ahead of the done event", func(w http.ResponseWriter) {
			_, _ = io.WriteString(w, transcriptionFrameDelta("hello")+transcriptionSSEEnd+transcriptionFrameDone("hello"))
		}, "success", transcriptionFrameDelta("hello") + transcriptionSSEEnd + transcriptionFrameDone("hello")},
		{"an empty stream", func(http.ResponseWriter) {}, "success", ""},
		{"a connection torn down mid-stream", func(w http.ResponseWriter) {
			_, _ = io.WriteString(w, transcriptionFrameDelta("hello"))
			w.(http.Flusher).Flush()
			panic(http.ErrAbortHandler)
		}, "error", transcriptionFrameDelta("hello")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			buf, restore := withCapturedSlog(t)
			t.Cleanup(restore)
			up, _ := newScriptedTranscriptionUpstream(t, func(_ int, w http.ResponseWriter, _ transcriptionUpstreamCall) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				tc.answer(w)
			})
			srv, _ := newTranscriptionCapableTestServer(t, up.URL)

			rec := postTranscriptionParts(t, srv, streamingTranscriptionPartsOf(transcriptionGatewayModel, wav))

			if rec.Code != http.StatusOK || rec.Body.String() != tc.wantBody {
				t.Fatalf("response = %d %q, want 200 and the frames the upstream sent, %q", rec.Code, rec.Body.String(), tc.wantBody)
			}
			ev := onlyUsageEvent(t, srv)
			if ev.Status != tc.wantStatus {
				t.Fatalf("usage row status = %q, want %q", ev.Status, tc.wantStatus)
			}
			if ev.BillingQuantity != 0 {
				t.Fatalf("BillingQuantity = %v, want 0: a stream that did not complete cleanly bills nothing", ev.BillingQuantity)
			}
			requireSpeechNonTokenRow(t, ev)
			if tc.wantStatus == "success" {
				errs := errorLogs(buf)
				if len(errs) != 1 || !strings.Contains(errs[0].Msg, "streamed transcription") || !strings.Contains(errs[0].Msg, "without") {
					t.Fatalf("Error logs = %+v, want exactly one saying the stream ended without its terminal events", errs)
				}
			}
		})
	}
}

// A clean stream whose upload cannot be measured is a loud zero too: the upload is no
// WAV (webm bytes), or its header contradicts itself (an inflated byte rate would bill
// a long clip as a short one), the `data` chunk is longer than what was uploaded, or it
// declares less than the upload holds after it (the audio hidden behind an empty chunk).
// The transcript still reaches the client.
func TestTranscriptionStreamLoudZeroOnAnUploadWithNoBillableDuration(t *testing.T) {
	inflated := wavFile(t, wavFmtChunkWith(t, 1, 16000, 3200000, 2, 16), wavDataChunk(t, 32000)) // 1 s of audio claiming 10 ms
	truncated := wavFile(t, wavFmtChunk(t, 16000, 1, 16), wavChunk(t, "data", 64000, make([]byte, 100)))
	declaresNothing := wavFile(t, wavFmtChunk(t, 16000, 1, 16), wavChunk(t, "data", 0, nil), make([]byte, 32000)) // 1 s of audio declared as none
	for _, tc := range []struct {
		name  string
		audio []byte
	}{
		{"webm bytes", []byte("\x1a\x45\xdf\xa3webm-not-a-wav")},
		{"RIFF that is not WAVE", []byte("RIFF\x00\x00\x00\x00WEBMjunk")},
		{"the fixture bytes of the other tests", transcriptionTestAudio},
		{"an inflated byte rate", inflated},
		{"a data chunk longer than the upload", truncated},
		{"a data chunk declaring zero with the audio after it", declaresNothing},
	} {
		t.Run(tc.name, func(t *testing.T) {
			buf, restore := withCapturedSlog(t)
			t.Cleanup(restore)
			up, _ := newSSETranscriptionUpstream(t, transcriptionFrameDelta("hello"), transcriptionFrameDone("hello"), transcriptionSSEEnd)
			srv, _ := newTranscriptionCapableTestServer(t, up.URL)

			rec := postTranscriptionParts(t, srv, streamingTranscriptionPartsOf(transcriptionGatewayModel, tc.audio))

			if want := transcriptionFrameDelta("hello") + transcriptionFrameDone("hello") + transcriptionSSEEnd; rec.Code != http.StatusOK || rec.Body.String() != want {
				t.Fatalf("response = %d %q, want the upstream's stream", rec.Code, rec.Body.String())
			}
			ev := onlyUsageEvent(t, srv)
			if ev.Status != "success" || ev.BillingQuantity != 0 {
				t.Fatalf("usage row = %q billing %v, want a success that billed 0", ev.Status, ev.BillingQuantity)
			}
			requireSpeechNonTokenRow(t, ev)
			errs := errorLogs(buf)
			if len(errs) != 1 || !strings.Contains(errs[0].Msg, "streamed transcription") || !strings.Contains(errs[0].Msg, "WAV") {
				t.Fatalf("Error logs = %+v, want exactly one saying the upload's duration could not be read from a WAV header", errs)
			}
		})
	}
}

// A clip shorter than a millisecond is a real measurement of zero, not a failure to
// measure: billed 0 and logged nowhere at Error.
func TestTranscriptionStreamBillsASubMillisecondClipAsZeroWithoutAnError(t *testing.T) {
	buf, restore := withCapturedSlog(t)
	t.Cleanup(restore)
	up, _ := newSSETranscriptionUpstream(t, transcriptionFrameDelta("hi"), transcriptionFrameDone("hi"), transcriptionSSEEnd)
	srv, _ := newTranscriptionCapableTestServer(t, up.URL)

	rec := postTranscriptionParts(t, srv, streamingTranscriptionPartsOf(transcriptionGatewayModel, buildPCMWav(t, 16000, 1, 16, 16))) // 0.5 ms

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ev := onlyUsageEvent(t, srv); ev.Status != "success" || ev.BillingQuantity != 0 {
		t.Fatalf("usage row = %q billing %v, want a success billed 0", ev.Status, ev.BillingQuantity)
	}
	if errs := errorLogs(buf); len(errs) != 0 {
		t.Fatalf("a measured zero logged at Error: %+v", errs)
	}
}

// The capture of a streamed transcription is the TRANSCRIPT the stream carried, not
// the redacted summary a speech response gets (the stream shares speech's openai_audio
// flavor, so the capture's endpoint stamp is what tells them apart): the deltas
// assembled, equal to the done event's own text, and stored whole.
func TestTranscriptionStreamCaptureAssemblesTranscript(t *testing.T) {
	up, _ := newSSETranscriptionUpstream(t, transcriptionFrameDelta("hello "), transcriptionFrameDelta("there, "), transcriptionFrameDelta("world"), transcriptionFrameDone("hello there, world"), transcriptionSSEEnd)
	srv, cs := newTranscriptionCaptureServer(t, up.URL)

	rec := postTranscriptionParts(t, srv, streamingTranscriptionPartsOf(transcriptionGatewayModel, buildPCMWav(t, 16000, 1, 16, 16000)))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	saved, ok := cs.last()
	if !ok {
		t.Fatal("no capture saved")
	}
	if saved.APIFlavor != apiFlavorSpeech || saved.HTTPStatus != http.StatusOK {
		t.Fatalf("capture row = %q/%d, want %s/200", saved.APIFlavor, saved.HTTPStatus, apiFlavorSpeech)
	}
	env := decryptCapture(t, saved.Blob)
	if env.RespBody != "hello there, world" {
		t.Fatalf("response body = %q, want the assembled transcript (the deltas joined, which is the done event's text)", env.RespBody)
	}
	if decodeCapturedBinaryBodyIsSummary(env.RespBody) {
		t.Fatalf("response body = %q is the redacted binary summary", env.RespBody)
	}
	if env.Truncated {
		t.Fatal("Truncated = true, want false: the transcript is far below the cap")
	}
	if got := env.RespHeaders["Content-Type"]; len(got) != 1 || got[0] != "text/event-stream" {
		t.Fatalf("response headers = %v, want the Content-Type the client was sent", env.RespHeaders)
	}
	// The request is captured as a transcription's always is: fields kept, audio redacted.
	if p := capturedPartByName(t, decodeCapturedMultipart(t, env.ReqBody), "file"); !p.Redacted {
		t.Fatalf("file part = %+v, want the audio redacted", p)
	}
}

// A stream that breaks off keeps the transcript it had delivered: the capture holds
// the deltas assembled so far (the done event, which would have restated the whole
// text, never came).
func TestTranscriptionStreamCaptureKeepsThePartialTranscriptOfAnIncompleteStream(t *testing.T) {
	up, _ := newSSETranscriptionUpstream(t, transcriptionFrameDelta("hello "), transcriptionFrameDelta("there"))
	srv, cs := newTranscriptionCaptureServer(t, up.URL)

	rec := postTranscriptionParts(t, srv, streamingTranscriptionPartsOf(transcriptionGatewayModel, buildPCMWav(t, 16000, 1, 16, 16000)))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	saved, ok := cs.last()
	if !ok {
		t.Fatal("no capture saved")
	}
	if env := decryptCapture(t, saved.Blob); env.RespBody != "hello there" {
		t.Fatalf("response body = %q, want the partial transcript the deltas carried", env.RespBody)
	}
}

// The done event is the authoritative text: it is what the capture stores when it
// differs from the deltas (a model that revises what it said), and the only source
// when the stream carried no deltas at all. An EMPTY done text states nothing, though,
// and does not erase the deltas that did arrive.
func TestTranscriptionStreamCapturePrefersTheDoneText(t *testing.T) {
	for _, tc := range []struct {
		name   string
		frames []string
	}{
		{"the done event revises the deltas", []string{transcriptionFrameDelta("hello thar"), transcriptionFrameDone("hello there"), transcriptionSSEEnd}},
		{"no deltas at all", []string{transcriptionFrameDone("hello there"), transcriptionSSEEnd}},
		{"an empty done text does not erase the deltas", []string{transcriptionFrameDelta("hello "), transcriptionFrameDelta("there"), transcriptionFrameDone(""), transcriptionSSEEnd}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up, _ := newSSETranscriptionUpstream(t, tc.frames...)
			srv, cs := newTranscriptionCaptureServer(t, up.URL)

			postTranscriptionParts(t, srv, streamingTranscriptionPartsOf(transcriptionGatewayModel, buildPCMWav(t, 16000, 1, 16, 16000)))

			saved, ok := cs.last()
			if !ok {
				t.Fatal("no capture saved")
			}
			if env := decryptCapture(t, saved.Blob); env.RespBody != "hello there" {
				t.Fatalf("response body = %q, want the done event's text", env.RespBody)
			}
		})
	}
}

// The capture cap bounds what a capture keeps and never what the relay delivers: a
// transcript past it is clipped to the cap and marked truncated, while the client
// still gets every frame -- and the stream still completes, so it is billed.
func TestTranscriptionStreamCaptureClipsATranscriptPastTheCap(t *testing.T) {
	var frames []string
	var whole strings.Builder
	for i := 0; i < 64; i++ {
		piece := strings.Repeat("x", 64) // 4096 bytes of transcript in all
		whole.WriteString(piece)
		frames = append(frames, transcriptionFrameDelta(piece))
	}
	frames = append(frames, transcriptionFrameDone(whole.String()), transcriptionSSEEnd)
	up, _ := newSSETranscriptionUpstream(t, frames...)
	srv, cs := newTranscriptionCaptureServer(t, up.URL)
	srv.captureMaxBytes = 1024

	rec := postTranscriptionParts(t, srv, streamingTranscriptionPartsOf(transcriptionGatewayModel, buildPCMWav(t, 16000, 1, 16, 16000)))

	if want := strings.Join(frames, ""); rec.Code != http.StatusOK || rec.Body.String() != want {
		t.Fatalf("client got %d bytes, want the whole %d-byte stream", rec.Body.Len(), len(want))
	}
	saved, ok := cs.last()
	if !ok {
		t.Fatal("no capture saved")
	}
	env := decryptCapture(t, saved.Blob)
	if env.RespBody != whole.String()[:1024] || !env.Truncated {
		t.Fatalf("response body is %d bytes, Truncated = %v, want the first 1024 bytes of the transcript and Truncated", len(env.RespBody), env.Truncated)
	}
	if ev := onlyUsageEvent(t, srv); ev.BillingQuantity != 0.5 {
		t.Fatalf("BillingQuantity = %v, want 0.5: a transcript past the capture cap does not stop the stream completing", ev.BillingQuantity)
	}
}

// The mode-mismatch refusal is retried EXACTLY once. A stub that refuses every call --
// the retry included -- must see two calls and no more: the retry is a plain
// transcription, so its refusal is the upstream's error like any other and is relayed.
// (Were the retry still marked as a stream -- rel.bufferedRetry not cleared before the
// relay re-enters itself -- every refusal would start another retry, without end.)
func TestTranscriptionStreamRetriesTheRefusalExactlyOnce(t *testing.T) {
	up, calls := newScriptedTranscriptionUpstream(t, func(n int, w http.ResponseWriter, _ transcriptionUpstreamCall) {
		w.Header().Set("Content-Type", "application/json")
		if n > 2 {
			// Only a relay that keeps retrying gets here. Answering it differently ends its
			// loop, so that breaking the guard FAILS this test instead of hanging it.
			_, _ = io.WriteString(w, transcriptionOKBody)
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, transcriptionModeMismatchBody)
	})
	srv, _ := newTranscriptionCapableTestServer(t, up.URL)

	rec := postTranscriptionParts(t, srv, streamingTranscriptionParts(transcriptionGatewayModel))

	if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), transcriptionStreamingRefusal) {
		t.Fatalf("response = %d %s, want the upstream's refusal relayed as its 500", rec.Code, rec.Body.String())
	}
	all := drainTranscriptionUpstreamCalls(calls)
	if len(all) != 2 {
		t.Fatalf("upstream calls = %d, want exactly 2 (the streamed attempt and its one buffered retry)", len(all))
	}
	if _, ok := formFieldOf(t, all[1].body, all[1].contentType, "stream"); ok {
		t.Fatal("the retry still carries a stream field, want it dropped")
	}
	events := srv.Usage.All()
	if len(events) != 1 || events[0].Status != "error" || events[0].HTTPStatus != http.StatusInternalServerError {
		t.Fatalf("usage rows = %+v, want one error row at the upstream's 500", events)
	}
}

// Frames are not aligned with the upstream's writes: a frame -- the done event and the
// [DONE] sentinel included -- split across several writes still completes the stream.
func TestTranscriptionStreamCompletesWhenFramesAreSplitAcrossWrites(t *testing.T) {
	done := transcriptionFrameDone("hello there")
	pieces := []string{
		transcriptionFrameDelta("hello there")[:20],
		transcriptionFrameDelta("hello there")[20:] + done[:30],
		done[30:] + "data: [DO",
		"NE]\n\n",
	}
	up, _ := newSSETranscriptionUpstream(t, pieces...)
	srv, cs := newTranscriptionCaptureServer(t, up.URL)

	rec := postTranscriptionParts(t, srv, streamingTranscriptionPartsOf(transcriptionGatewayModel, buildPCMWav(t, 16000, 1, 16, 16000)))

	if want := strings.Join(pieces, ""); rec.Code != http.StatusOK || rec.Body.String() != want {
		t.Fatalf("response = %d %q, want the upstream's stream unchanged", rec.Code, rec.Body.String())
	}
	if ev := onlyUsageEvent(t, srv); ev.BillingQuantity != 0.5 {
		t.Fatalf("BillingQuantity = %v, want 0.5: the stream completed", ev.BillingQuantity)
	}
	saved, ok := cs.last()
	if !ok {
		t.Fatal("no capture saved")
	}
	if env := decryptCapture(t, saved.Blob); env.RespBody != "hello there" {
		t.Fatalf("response body = %q, want the transcript", env.RespBody)
	}
}

// scanOf feeds stream to a scan in chunks of size bytes (the whole stream at once for
// size 0), finishes it, and returns it.
func scanOf(scan *transcriptionStreamScan, stream string, size int) *transcriptionStreamScan {
	if size <= 0 {
		size = len(stream)
	}
	for rest := []byte(stream); len(rest) > 0; {
		n := min(size, len(rest))
		scan.feed(rest[:n])
		rest = rest[n:]
	}
	scan.finish()
	return scan
}

func newTestTranscriptionScan() *transcriptionStreamScan {
	return &transcriptionStreamScan{maxLine: 1 << 20, keepText: true, textCap: 1 << 20}
}

// What the scan concludes does not depend on how the stream was cut into chunks, or on
// the line endings it uses.
func TestTranscriptionStreamScanReadsAStreamWhateverItsChunking(t *testing.T) {
	stream := transcriptionFrameDelta("héllo ") + ": a comment\n" + "event: transcript\n" + transcriptionFrameDelta("wörld") + transcriptionFrameDone("héllo wörld") + transcriptionSSEEnd
	for name, text := range map[string]string{
		"LF":   stream,
		"CRLF": strings.ReplaceAll(stream, "\n", "\r\n"),
	} {
		for _, size := range []int{0, 1, 2, 3, 7, 64} {
			scan := scanOf(newTestTranscriptionScan(), text, size)
			if !scan.complete() {
				t.Fatalf("%s in chunks of %d: complete = false, want true", name, size)
			}
			if got := string(scan.transcript()); got != "héllo wörld" {
				t.Fatalf("%s in chunks of %d: transcript = %q, want %q", name, size, got, "héllo wörld")
			}
		}
	}
}

// Only the terminal events in their order complete a stream, and anything that is not
// one is ignored rather than mistaken for one.
func TestTranscriptionStreamScanCompletion(t *testing.T) {
	for _, tc := range []struct {
		name     string
		stream   string
		complete bool
	}{
		{"done then [DONE]", transcriptionFrameDone("x") + transcriptionSSEEnd, true},
		{"[DONE] as the last line, with no newline", transcriptionFrameDone("x") + "data: [DONE]", true},
		{"[DONE] without a space after the colon", transcriptionFrameDone("x") + "data:[DONE]\n\n", true},
		{"the done event only", transcriptionFrameDone("x"), false},
		{"[DONE] only", transcriptionSSEEnd, false},
		{"[DONE] ahead of the done event", transcriptionSSEEnd + transcriptionFrameDone("x"), false},
		{"nothing", "", false},
		{"the done event cut mid-frame", transcriptionFrameDone("x")[:40], false},
		{"a done event that is not JSON", "data: {\"type\":\"transcript.text.done\"\n\n" + transcriptionSSEEnd, false},
		{"the event type named in a delta's text, not in its type", transcriptionFrameDelta(`transcript.text.done`) + transcriptionSSEEnd, false},
		{"another event type", "data: {\"type\":\"transcript.text.segment\",\"text\":\"x\"}\n\n" + transcriptionSSEEnd, false},
		{"the done event as a comment", ": " + transcriptionFrameDone("x") + transcriptionSSEEnd, false},
		{"[DONE] carried in a field that is not data", transcriptionFrameDone("x") + "event: [DONE]\n\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, size := range []int{0, 1} {
				if got := scanOf(newTestTranscriptionScan(), tc.stream, size).complete(); got != tc.complete {
					t.Fatalf("complete = %v (chunks of %d), want %v", got, size, tc.complete)
				}
			}
		})
	}
}

// The transcript the scan keeps is bounded by the cap -- one byte past it, so a
// clipped transcript can be told from one that fits exactly -- and is not kept at all
// when nothing is captured; completion is tracked either way.
func TestTranscriptionStreamScanBoundsTheTranscript(t *testing.T) {
	stream := transcriptionFrameDelta("0123456789") + transcriptionFrameDelta("abcdefghij") + transcriptionFrameDone("0123456789abcdefghij") + transcriptionSSEEnd

	t.Run("clipped to the cap plus one byte", func(t *testing.T) {
		scan := scanOf(&transcriptionStreamScan{maxLine: 1 << 20, keepText: true, textCap: 12}, stream, 0)
		if got := string(scan.transcript()); got != "0123456789abc" {
			t.Fatalf("transcript = %q, want the first 13 bytes of the done text", got)
		}
	})
	t.Run("deltas are clipped the same way", func(t *testing.T) {
		scan := scanOf(&transcriptionStreamScan{maxLine: 1 << 20, keepText: true, textCap: 12}, transcriptionFrameDelta("0123456789")+transcriptionFrameDelta("abcdefghij"), 0)
		if got := string(scan.transcript()); got != "0123456789abc" {
			t.Fatalf("transcript = %q, want the first 13 bytes of the deltas", got)
		}
	})
	t.Run("a transcript that fits the cap exactly is whole", func(t *testing.T) {
		scan := scanOf(&transcriptionStreamScan{maxLine: 1 << 20, keepText: true, textCap: 20}, stream, 0)
		if got := string(scan.transcript()); got != "0123456789abcdefghij" {
			t.Fatalf("transcript = %q, want the whole text", got)
		}
	})
	t.Run("nothing kept when nothing is captured", func(t *testing.T) {
		scan := scanOf(&transcriptionStreamScan{maxLine: 1 << 20}, stream, 0)
		if len(scan.transcript()) != 0 || !scan.complete() {
			t.Fatalf("transcript = %q, complete = %v, want no transcript and a completed stream", scan.transcript(), scan.complete())
		}
	})
}

// A line that has to be carried across chunks and outgrows maxLine is given up whole --
// its start, and its rest however many chunks that spans -- and the lines after it are
// read as if it had not been there. (A line that arrives inside one chunk is held by
// nobody: the bound is on what the scan carries, and a chunk is the copier's own read
// buffer.)
func TestTranscriptionStreamScanDropsALineBeyondItsBoundAndRecovers(t *testing.T) {
	long := transcriptionFrameDelta(strings.Repeat("x", 300)) // one line, far past 100 bytes
	newScan := func() *transcriptionStreamScan {
		return &transcriptionStreamScan{maxLine: 100, keepText: true, textCap: 1 << 20}
	}
	for _, size := range []int{1, 50, 150} {
		scan := scanOf(newScan(), long+transcriptionFrameDelta("kept"), size)
		if got := string(scan.transcript()); got != "kept" {
			t.Fatalf("chunks of %d: transcript = %q, want only the line after the dropped one", size, got)
		}
		if cap(scan.carry) > 100 {
			t.Fatalf("chunks of %d: carry holds %d bytes, want it bounded by the line limit", size, cap(scan.carry))
		}
		if scan := scanOf(newScan(), long+transcriptionFrameDone("kept")+transcriptionSSEEnd, size); !scan.complete() {
			t.Fatalf("chunks of %d: complete = false, want true: the lines after the dropped one still count", size)
		}
	}
	// A done frame that is itself past the bound is lost: billing fails closed.
	scan := scanOf(newScan(), transcriptionFrameDone(strings.Repeat("x", 300))+transcriptionSSEEnd, 50)
	if scan.complete() {
		t.Fatal("a done frame past the line bound completed the stream, want it dropped (a loud zero, never a guess)")
	}
}

// A relay that is not a streamed transcription carries no scan, and none of its
// methods may trip on that.
func TestTranscriptionStreamScanIsNilSafe(t *testing.T) {
	var scan *transcriptionStreamScan
	scan.feed([]byte(transcriptionFrameDone("x") + transcriptionSSEEnd))
	scan.finish()
	if scan.complete() {
		t.Fatal("a nil scan completed")
	}
	if got := scan.transcript(); len(got) != 0 {
		t.Fatalf("a nil scan's transcript = %q, want none", got)
	}
	in := &captureInput{RespBody: []byte("kept")}
	scan.stampCapture(in)
	if in.Endpoint != endpointChat || string(in.RespBody) != "kept" {
		t.Fatalf("a nil scan changed the capture: %+v", in)
	}
	scanOf(newTestTranscriptionScan(), "x", 0).stampCapture(nil)
}

// Only the transcription endpoint gets a scan.
func TestNewTranscriptionStreamScanIsForTheTranscriptionEndpointOnly(t *testing.T) {
	srv := &Server{captureMaxBytes: 100}
	for endpoint, want := range map[sessionEndpoint]bool{endpointTranscription: true, endpointSpeech: false, endpointChat: false} {
		if got := srv.newTranscriptionStreamScan(endpoint, true) != nil; got != want {
			t.Errorf("scan for endpoint %v present = %v, want %v", endpoint, got, want)
		}
	}
	scan := srv.newTranscriptionStreamScan(endpointTranscription, true)
	if !scan.keepText || scan.textCap != 100 || scan.maxLine != int(defaultMaxAudioTranscriptionBodyBytes) {
		t.Fatalf("scan = %+v, want it keeping up to the capture cap, with the transcription cap as its line bound", scan)
	}
	if srv.newTranscriptionStreamScan(endpointTranscription, false).keepText {
		t.Fatal("a scan of a request that is not captured keeps the transcript")
	}
}

// The upload's duration is read from the right part of the form, however the form is
// laid out, and from nothing that is not a form.
func TestUploadWavDurationMillis(t *testing.T) {
	wav := buildPCMWav(t, 16000, 1, 16, 32000) // 1 s
	other := buildPCMWav(t, 16000, 1, 16, 64000)
	form := func(parts ...editPart) ([]byte, string) { return buildEditMultipart(t, parts) }

	for _, tc := range []struct {
		name   string
		parts  []editPart
		wantMs int64
		wantOK bool
	}{
		{"the file part", []editPart{{name: "model", body: []byte("m")}, {name: "file", filename: "a.wav", body: wav}}, 1000, true},
		{"the file part first", []editPart{{name: "file", filename: "a.wav", body: wav}, {name: "model", body: []byte("m")}}, 1000, true},
		{"an empty file part is no upload", []editPart{{name: "file", filename: "e.wav"}, {name: "file", filename: "a.wav", body: wav}}, 1000, true},
		{"a WAV in another field is not the upload", []editPart{{name: "file[]", filename: "o.wav", body: other}, {name: "prompt", body: other}, {name: "file", filename: "a.wav", body: wav}}, 1000, true},
		{"no file part", []editPart{{name: "model", body: []byte("m")}, {name: "file[]", filename: "o.wav", body: other}}, 0, false},
		{"only an empty file part", []editPart{{name: "file", filename: "e.wav"}}, 0, false},
		{"a file that is no WAV", []editPart{{name: "file", filename: "a.webm", body: []byte("not a wav")}}, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body, ct := form(tc.parts...)
			ms, ok := uploadWavDurationMillis(body, ct)
			if ms != tc.wantMs || ok != tc.wantOK {
				t.Fatalf("uploadWavDurationMillis = (%d, %v), want (%d, %v)", ms, ok, tc.wantMs, tc.wantOK)
			}
		})
	}
	body, ct := form(editPart{name: "file", filename: "a.wav", body: wav})
	for name, badCT := range map[string]string{"no content type": "", "not multipart": "application/json", "no boundary": "multipart/form-data", "a boundary that is not the body's": strings.Replace(ct, "boundary=", "boundary=x", 1)} {
		if ms, ok := uploadWavDurationMillis(body, badCT); ok || ms != 0 {
			t.Errorf("%s: uploadWavDurationMillis = (%d, %v), want (0, false)", name, ms, ok)
		}
	}
	if ms, ok := uploadWavDurationMillis(body[:len(body)/2], ct); ok || ms != 0 {
		t.Errorf("a form cut short: uploadWavDurationMillis = (%d, %v), want (0, false)", ms, ok)
	}
}

// The done event's text is the transcript when it states one; an empty one (a done
// event that restates nothing) is no statement, and the deltas stand -- but with no
// deltas either, the transcript is simply empty.
func TestTranscriptionStreamScanTranscriptSource(t *testing.T) {
	for _, tc := range []struct {
		name   string
		stream string
		want   string
	}{
		{"the done text over the deltas", transcriptionFrameDelta("hello thar") + transcriptionFrameDone("hello there"), "hello there"},
		{"the done text alone", transcriptionFrameDone("hello there"), "hello there"},
		{"an empty done text keeps the deltas", transcriptionFrameDelta("hello ") + transcriptionFrameDelta("there") + transcriptionFrameDone(""), "hello there"},
		{"a done event with no text keeps the deltas", transcriptionFrameDelta("hello") + "data: {\"type\":\"transcript.text.done\"}\n\n", "hello"},
		{"an empty done text and no deltas", transcriptionFrameDone(""), ""},
		{"the deltas of a stream that never finished", transcriptionFrameDelta("hello ") + transcriptionFrameDelta("there"), "hello there"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := string(scanOf(newTestTranscriptionScan(), tc.stream, 0).transcript()); got != tc.want {
				t.Fatalf("transcript = %q, want %q", got, tc.want)
			}
		})
	}
}

// The copier relays and scans a streamed transcription with no capture tee at all: the
// capture of that relay is built from the scan, so proxyNative gives the copier no
// respBuf, and the bytes still reach the client whole and the scan still completes.
func TestNativeCopierRelaysAndScansAStreamWithoutATee(t *testing.T) {
	stream := transcriptionFrameDelta("hello ") + transcriptionFrameDelta("there") + transcriptionFrameDone("hello there") + transcriptionSSEEnd
	rec := httptest.NewRecorder()
	scan := newTestTranscriptionScan()
	copier := &nativeCopier{w: rec, flusher: rec, capBytes: 4, transcript: scan}

	if err := copier.run(strings.NewReader(stream)); err != nil {
		t.Fatalf("run: %v", err)
	}

	if rec.Body.String() != stream || copier.written != int64(len(stream)) {
		t.Fatalf("client got %q (%d bytes written), want the whole stream", rec.Body.String(), copier.written)
	}
	if !scan.complete() || string(scan.transcript()) != "hello there" {
		t.Fatalf("scan complete = %v, transcript = %q, want a completed stream of %q", scan.complete(), scan.transcript(), "hello there")
	}
}
