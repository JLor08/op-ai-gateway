// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
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

	ev := lastUsageEvent(t, srv)
	if ev.Status != "success" || ev.HTTPStatus != http.StatusOK || ev.ContentType != "text/event-stream" {
		t.Fatalf("usage row = %q/%d/%q, want success/200/text/event-stream", ev.Status, ev.HTTPStatus, ev.ContentType)
	}
	if !ev.Stream {
		t.Fatal("usage row Stream = false, want true: the call was streamed")
	}
	if ev.ProviderPath != "/v1/audio/transcriptions" || ev.BillingUnit != usage.BillingUnitAudioSecond {
		t.Fatalf("usage row = %q in %q, want /v1/audio/transcriptions in audio_second", ev.ProviderPath, ev.BillingUnit)
	}
	// Interim: a streamed body carries no duration, so it bills a loud zero until the
	// duration is taken from the upload's WAV header (a later task replaces this).
	if ev.BillingQuantity != 0 {
		t.Fatalf("BillingQuantity = %v, want the interim loud zero", ev.BillingQuantity)
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
func TestRefusedStreamingTranscription(t *testing.T) {
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

			if got := refusedStreamingTranscription(resp); got != tc.want {
				t.Fatalf("refusedStreamingTranscription = %v, want %v", got, tc.want)
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
