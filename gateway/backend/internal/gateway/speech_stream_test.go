// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/capture"
	"op-ai-gateway/internal/inference"
	"op-ai-gateway/internal/provider"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/usage"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The wire format audio.cpp streams a synthesis in (verified live): OpenAI-style SSE,
// one speech.audio.delta per chunk with the PCM base64-encoded, a speech.audio.done
// that states a time to first token and NO duration, then the [DONE] sentinel. With
// stream_format=audio the body is instead headerless raw PCM under
// application/octet-stream. The gateway relays either byte for byte, so the fixtures
// are the exact frames a client must receive.
const (
	speechSSEDone = "data: {\"type\":\"speech.audio.done\",\"timing\":{\"ttft_ms\":12}}\n\n"
	speechSSEEnd  = "data: [DONE]\n\n"

	// speechModeMismatchBody is what audio.cpp answers (HTTP 500, as it does for every
	// error) when stream=true reaches a model that is not loaded with mode=streaming:
	// the gateway's cue to retry the request buffered.
	speechModeMismatchBody = `{"error":{"message":"speech streaming requires a model configured with mode=streaming","type":"server_error"}}`
)

// speechPCM returns n bytes of PCM-like payload: deterministic, and covering the whole
// byte range so the base64 round trip cannot pass by accident.
func speechPCM(n int, seed byte) []byte {
	pcm := make([]byte, n)
	for i := range pcm {
		pcm[i] = byte(i*31) + seed
	}
	return pcm
}

// speechFrameDelta builds the speech.audio.delta frame audio.cpp streams for pcm.
func speechFrameDelta(pcm []byte) string {
	return "data: {\"type\":\"speech.audio.delta\",\"audio\":\"" + base64.StdEncoding.EncodeToString(pcm) + "\"}\n\n"
}

// newScriptedSpeechUpstream is newSpeechUpstream with the answer decided per call:
// answer gets the 1-based call number and the request it answers, and writes the
// response itself, so a stub can stream, stall, or answer differently the second
// time. Every request is recorded before it is answered.
func newScriptedSpeechUpstream(t *testing.T, answer func(n int, w http.ResponseWriter, call speechUpstreamCall)) (*httptest.Server, <-chan speechUpstreamCall) {
	t.Helper()
	calls := make(chan speechUpstreamCall, 16)
	var n atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		call := speechUpstreamCall{method: r.Method, path: r.URL.Path, contentType: r.Header.Get("Content-Type"), body: body}
		calls <- call
		answer(int(n.Add(1)), w, call)
	}))
	t.Cleanup(up.Close)
	return up, calls
}

// drainSpeechUpstreamCalls returns every request the stub has seen so far, in arrival
// order. The stub records a request before it answers it, so once the client has its
// whole response the list is complete.
func drainSpeechUpstreamCalls(calls <-chan speechUpstreamCall) []speechUpstreamCall {
	var out []speechUpstreamCall
	for {
		select {
		case c := <-calls:
			out = append(out, c)
		default:
			return out
		}
	}
}

// newSSESpeechUpstream starts an audio.cpp stub that answers every call with an event
// stream made of frames, written and flushed one at a time, and then ends the body
// cleanly.
func newSSESpeechUpstream(t *testing.T, frames ...string) (*httptest.Server, <-chan speechUpstreamCall) {
	t.Helper()
	return newScriptedSpeechUpstream(t, func(_ int, w http.ResponseWriter, _ speechUpstreamCall) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		for _, frame := range frames {
			_, _ = io.WriteString(w, frame)
			flusher.Flush()
		}
	})
}

// newRawSpeechUpstream starts an audio.cpp stub that answers every call the way
// stream_format=audio is answered: headerless PCM under application/octet-stream,
// chunk by flushed chunk, ended cleanly.
func newRawSpeechUpstream(t *testing.T, chunks ...[]byte) (*httptest.Server, <-chan speechUpstreamCall) {
	t.Helper()
	return newScriptedSpeechUpstream(t, func(_ int, w http.ResponseWriter, _ speechUpstreamCall) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		for _, chunk := range chunks {
			_, _ = w.Write(chunk)
			flusher.Flush()
		}
	})
}

// speechUpstreamFields decodes the JSON body the stub received, failing when it is no
// object: a test reads from it which fields reached audio.cpp.
func speechUpstreamFields(t *testing.T, call speechUpstreamCall) map[string]any {
	t.Helper()
	var fields map[string]any
	if err := json.Unmarshal(call.body, &fields); err != nil {
		t.Fatalf("upstream body is not a JSON object: %v (%s)", err, call.body)
	}
	return fields
}

// speechBodyWantsStream reports whether a request body the stub received asks for a
// stream the way audio.cpp reads it: stream is the JSON true.
func speechBodyWantsStream(body []byte) bool {
	var probe struct {
		Stream bool `json:"stream"`
	}
	_ = json.Unmarshal(body, &probe)
	return probe.Stream
}

// The gateway models of the streaming fixture, one per state the stream-versus-buffered
// decision tells apart. All three are speech-capable (a (speech, yes) verdict), so the
// capability gate admits every one; they differ only in the speech_streaming verdict.
const (
	// speechStreamingModel is route-tts: speech_streaming yes.
	speechStreamingModel = speechGatewayModel
	// speechOfflineModel: speech_streaming no, a probed model in mode=offline.
	speechOfflineModel    = "tts-offline"
	speechOfflineUpstream = "pocket_tts_offline"
	// speechUnprobedModel: speech yes but no speech_streaming row, a model whose mode
	// no probe has stated.
	speechUnprobedModel    = "tts-unprobed"
	speechUnprobedUpstream = "pocket_tts_unprobed"
)

// newSpeechStreamingTestServer is the speech test server with the speech_streaming
// axis filled in: route-tts is yes, and two more mappings carry a no and no row at all.
func newSpeechStreamingTestServer(t *testing.T, upstreamURL string) *Server {
	t.Helper()
	srv, _ := newSpeechCapableTestServer(t, upstreamURL)
	ctx := context.Background()
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	row := func(capability, verdict string) routing.CapabilityRow {
		return routing.CapabilityRow{Capability: capability, Verdict: verdict, Source: routing.CapabilitySourceAudioCppModels, CheckedAt: now}
	}
	for _, m := range []struct {
		id, gateway, upstream string
		rows                  []routing.CapabilityRow
	}{
		{"route-tts-offline", speechOfflineModel, speechOfflineUpstream, []routing.CapabilityRow{row(routing.CapabilitySpeech, routing.CapabilityYes), row(routing.CapabilitySpeechStreaming, routing.CapabilityNo)}},
		{"route-tts-unprobed", speechUnprobedModel, speechUnprobedUpstream, []routing.CapabilityRow{row(routing.CapabilitySpeech, routing.CapabilityYes)}},
	} {
		if err := srv.Routes.CreateMapping(ctx, routing.ModelMapping{ID: m.id, ApplicationID: "app-audio", GatewayModelName: m.gateway, AppModelName: m.upstream, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("CreateMapping %s: %v", m.id, err)
		}
		if err := srv.Routes.UpsertMappingCapabilities(ctx, m.id, m.rows); err != nil {
			t.Fatalf("UpsertMappingCapabilities %s: %v", m.id, err)
		}
	}
	if err := srv.Routes.UpsertMappingCapabilities(ctx, "route-tts", []routing.CapabilityRow{row(routing.CapabilitySpeechStreaming, routing.CapabilityYes)}); err != nil {
		t.Fatalf("UpsertMappingCapabilities route-tts: %v", err)
	}
	return srv
}

// newSpeechStreamingCaptureServer is newSpeechStreamingTestServer with payload capture
// forced on, over a fake store.
func newSpeechStreamingCaptureServer(t *testing.T, upstreamURL string) (*Server, *fakeCaptureStore) {
	t.Helper()
	srv := newSpeechStreamingTestServer(t, upstreamURL)
	cipher, err := capture.New(testCaptureKey)
	if err != nil {
		t.Fatalf("capture.New: %v", err)
	}
	cs := &fakeCaptureStore{}
	srv.Captures, srv.Cipher = cs, cipher
	srv.CaptureOverride = func() bool { return true }
	return srv, cs
}

// requireStreamedSpeechRow asserts the (unit, quantity) pair of a speech row and that
// the row is a well-formed non-token one: ValidateBillingXOR names the token column
// that leaked.
func requireStreamedSpeechRow(t *testing.T, ev usage.Event, unit string, quantity float64) {
	t.Helper()
	if ev.BillingUnit != unit || ev.BillingQuantity != quantity {
		t.Errorf("billing = %v %q, want %v %q", ev.BillingQuantity, ev.BillingUnit, quantity, unit)
	}
	if err := usage.ValidateBillingXOR(ev); err != nil {
		t.Errorf("recorded speech row violates the XOR: %v", err)
	}
}

// A streamed synthesis is relayed UNBUFFERED: the client gets the upstream's own event
// stream, frame for frame, as it is produced. The stub stalls after its first frame
// until the client has read it, so a relay that collected the body before answering
// cannot pass: the client would never see the frame the stub is holding the rest of
// the stream behind. It is also relayed CHUNKED, whatever length the upstream declared
// (a stream has no length the client could be told in advance).
func TestSpeechStreamRelayPassesSSEThroughUnbufferedAndChunked(t *testing.T) {
	first, second := speechPCM(900, 3), speechPCM(1100, 9)
	total := len(speechFrameDelta(first)) + len(speechFrameDelta(second)+speechSSEDone+speechSSEEnd)
	release := make(chan struct{})
	var stalled atomic.Bool
	up, calls := newScriptedSpeechUpstream(t, func(_ int, w http.ResponseWriter, _ speechUpstreamCall) {
		w.Header().Set("Content-Type", "text/event-stream")
		// A declared length (a buffering intermediary in front of the upstream could
		// add one): speech's relay forwards a one-shot WAV's length, but a stream is
		// relayed chunked whatever the upstream declared.
		w.Header().Set("Content-Length", strconv.Itoa(total))
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		_, _ = io.WriteString(w, speechFrameDelta(first))
		flusher.Flush()
		select {
		case <-release:
		case <-time.After(5 * time.Second):
			stalled.Store(true)
		}
		_, _ = io.WriteString(w, speechFrameDelta(second)+speechSSEDone+speechSSEEnd)
	})
	srv := newSpeechStreamingTestServer(t, up.URL)

	resp, err := postSpeechOverTheWire(t, srv, `{"model":"tts-voice","input":"Hello there.","stream":true}`)
	if err != nil {
		t.Fatalf("POST /v1/audio/speech: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("Content-Type = %q, want the upstream's text/event-stream", got)
	}
	if resp.ContentLength != -1 {
		t.Fatalf("ContentLength = %d, want -1: a stream is relayed chunked, never sized", resp.ContentLength)
	}
	br := bufio.NewReader(resp.Body)
	if got := readSSEFrame(t, br); got != speechFrameDelta(first) {
		t.Fatalf("first frame = %q, want the delta frame %q", got, speechFrameDelta(first))
	}
	close(release) // the client has the first frame while the upstream is still holding the rest
	rest, err := io.ReadAll(br)
	if err != nil {
		t.Fatalf("reading the rest of the stream: %v", err)
	}
	if want := speechFrameDelta(second) + speechSSEDone + speechSSEEnd; string(rest) != want {
		t.Fatalf("rest of the stream = %q, want %q", rest, want)
	}
	if stalled.Load() {
		t.Fatal("the upstream finished its stream before the client had its first frame: the relay buffered it")
	}
	all := drainSpeechUpstreamCalls(calls)
	if len(all) != 1 {
		t.Fatalf("upstream calls = %d, want 1: a stream that works is not retried", len(all))
	}
	if all[0].path != "/v1/audio/speech" {
		t.Fatalf("upstream path = %q, want /v1/audio/speech", all[0].path)
	}
}

// The relay requests a stream upstream ONLY when the client asked for one AND the
// mapped route's speech_streaming verdict is yes. Every other answer -- a no, no
// verdict at all, a verdict that cannot be read, a client that did not ask -- is the
// buffered one-shot WAV, and for those `stream` (and the stream-only stream_format and
// pcm response_format) is never sent: the upstream is not asked for something it would
// refuse, so the client's answer costs ONE upstream call, not a refused one and a
// retry. The stub streams when (and only when) it is asked to, like audio.cpp's
// streaming mode, so which answer the client gets shows which request the gateway sent.
func TestSpeechStreamsOnlyWhenVerdictYes(t *testing.T) {
	wav := speechTestWAV()
	pcm := speechPCM(1000, 1)
	frames := speechFrameDelta(pcm) + speechSSEDone + speechSSEEnd
	const fields = `"input":"Hello there.","voice":"alba","speed":1.25,"instructions":"calm"`
	for _, tc := range []struct {
		name         string
		model        string
		extra        string // appended to the request body
		failRead     bool
		wantStream   bool
		wantUpstream string // the model name the upstream must be sent
	}{
		{name: "a yes and a client that asked", model: speechStreamingModel, extra: `,"stream":true`, wantStream: true, wantUpstream: speechUpstreamModel},
		{name: "a yes and a client that asked for pcm over sse", model: speechStreamingModel, extra: `,"stream":true,"response_format":"pcm","stream_format":"sse"`, wantStream: true, wantUpstream: speechUpstreamModel},
		{name: "a yes and a client that did not ask", model: speechStreamingModel, wantUpstream: speechUpstreamModel},
		{name: "a yes and a client that said false", model: speechStreamingModel, extra: `,"stream":false`, wantUpstream: speechUpstreamModel},
		{name: "a no", model: speechOfflineModel, extra: `,"stream":true`, wantUpstream: speechOfflineUpstream},
		{name: "a no, and the stream-only fields are dropped too", model: speechOfflineModel, extra: `,"stream":true,"response_format":"pcm","stream_format":"audio"`, wantUpstream: speechOfflineUpstream},
		{name: "no verdict at all", model: speechUnprobedModel, extra: `,"stream":true`, wantUpstream: speechUnprobedUpstream},
		{name: "a verdict that cannot be read", model: speechStreamingModel, extra: `,"stream":true`, failRead: true, wantUpstream: speechUpstreamModel},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up, calls := newScriptedSpeechUpstream(t, func(_ int, w http.ResponseWriter, call speechUpstreamCall) {
				if speechBodyWantsStream(call.body) {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, frames)
					return
				}
				w.Header().Set("Content-Type", "audio/wav")
				w.Header().Set("X-AudioCPP-Audio-Duration-Ms", "1500")
				_, _ = w.Write(wav)
			})
			srv := newSpeechStreamingTestServer(t, up.URL)
			if tc.failRead {
				srv.Routes = capabilityReadFailingStore{Store: srv.Routes}
			}

			rec := postSpeech(t, srv, `{"model":"`+tc.model+`",`+fields+tc.extra+`}`)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body = %q", rec.Code, rec.Body.Bytes())
			}
			all := drainSpeechUpstreamCalls(calls)
			if len(all) != 1 {
				t.Fatalf("upstream calls = %d, want exactly 1: the answer must not cost a refused stream and a retry", len(all))
			}
			sent := speechUpstreamFields(t, all[0])
			if sent["model"] != tc.wantUpstream {
				t.Fatalf("upstream model = %v, want the mapped %q", sent["model"], tc.wantUpstream)
			}
			if sent["input"] != "Hello there." || sent["voice"] != "alba" || sent["speed"] != 1.25 || sent["instructions"] != "calm" {
				t.Fatalf("upstream body = %v, want every other field relayed untouched", sent)
			}
			ev := onlyUsageEvent(t, srv)
			if tc.wantStream {
				if got := rec.Header().Get("Content-Type"); got != "text/event-stream" {
					t.Fatalf("Content-Type = %q, want the relayed event stream", got)
				}
				if rec.Body.String() != frames {
					t.Fatalf("body = %q, want the upstream's frames", rec.Body.String())
				}
				if sent["stream"] != true {
					t.Fatalf("upstream stream = %v, want the client's true", sent["stream"])
				}
				if !ev.Stream {
					t.Fatal("usage row Stream = false, want true for a streamed call")
				}
				return
			}
			if got := rec.Header().Get("Content-Type"); got != "audio/wav" {
				t.Fatalf("Content-Type = %q, want the one-shot audio/wav", got)
			}
			if !bytes.Equal(rec.Body.Bytes(), wav) {
				t.Fatalf("body = %x, want the upstream WAV byte for byte", rec.Body.Bytes())
			}
			if tc.extra == `,"stream":false` {
				// A request that never asked to stream is relayed as the client wrote it.
				if v, ok := sent["stream"]; !ok || v != false {
					t.Fatalf("upstream stream = %v (present %v), want the client's own false relayed", v, ok)
				}
			} else {
				for _, field := range []string{"stream", "stream_format", "response_format"} {
					if v, ok := sent[field]; ok {
						t.Fatalf("upstream %s = %v, want NO such field sent on a buffered answer", field, v)
					}
				}
			}
			if ev.Stream {
				t.Fatal("usage row Stream = true, want a buffered call")
			}
			if ev.Status != "success" {
				t.Fatalf("usage row status = %q, want success", ev.Status)
			}
			requireStreamedSpeechRow(t, ev, usage.BillingUnitAudioSecond, 1.5) // from the duration header, as ever
		})
	}
}

// An upstream that answers a stream=true request with a plain WAV (it did not stream)
// is relayed like any one-shot synthesis: the WAV verbatim, billed from its duration
// header, never mistaken for a stream.
func TestSpeechStreamAnsweredWithAWAVIsRelayedBuffered(t *testing.T) {
	wav := speechTestWAV()
	up, calls := newSpeechUpstream(t, wav)
	srv := newSpeechStreamingTestServer(t, up.URL)

	rec := postSpeech(t, srv, `{"model":"tts-voice","input":"hi","stream":true}`)

	if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), wav) {
		t.Fatalf("response = %d %x, want the upstream's WAV verbatim", rec.Code, rec.Body.Bytes())
	}
	if all := drainSpeechUpstreamCalls(calls); len(all) != 1 || speechUpstreamFields(t, all[0])["stream"] != true {
		t.Fatalf("upstream calls = %+v, want one carrying the stream the verdict allows", all)
	}
	ev := onlyUsageEvent(t, srv)
	if ev.Status != "success" {
		t.Fatalf("usage row status = %q, want success", ev.Status)
	}
	requireStreamedSpeechRow(t, ev, usage.BillingUnitAudioSecond, 1.5)
}

// A clean stream is billed in audio_bytes: the sum of the PCM the delta frames carried
// (decoded, not the base64 text), on the one usage row, with no Error logged.
func TestSpeechStreamBillsAudioBytesOnCleanCompletion(t *testing.T) {
	buf, restore := withCapturedSlog(t)
	t.Cleanup(restore)
	deltas := [][]byte{speechPCM(1000, 1), speechPCM(2500, 2), speechPCM(777, 3)}
	frames := speechFrameDelta(deltas[0]) + speechFrameDelta(deltas[1]) + speechFrameDelta(deltas[2]) + speechSSEDone + speechSSEEnd
	up, _ := newSSESpeechUpstream(t, speechFrameDelta(deltas[0]), speechFrameDelta(deltas[1]), speechFrameDelta(deltas[2]), speechSSEDone, speechSSEEnd)
	srv := newSpeechStreamingTestServer(t, up.URL)

	rec := postSpeech(t, srv, `{"model":"tts-voice","input":"Hello there.","stream":true}`)

	if rec.Code != http.StatusOK || rec.Body.String() != frames {
		t.Fatalf("response = %d %q, want the upstream's stream, frame for frame", rec.Code, rec.Body.String())
	}
	ev := onlyUsageEvent(t, srv)
	if ev.Status != "success" || ev.ErrorCode != "" || !ev.Stream {
		t.Fatalf("usage row = %q/%q stream=%v, want a streamed success", ev.Status, ev.ErrorCode, ev.Stream)
	}
	requireStreamedSpeechRow(t, ev, usage.BillingUnitAudioBytes, 1000+2500+777)
	if ev.ProviderPath != "/v1/audio/speech" || ev.APIFlavor != apiFlavorSpeech {
		t.Fatalf("usage row = %q on %q, want the speech endpoint", ev.APIFlavor, ev.ProviderPath)
	}
	if errs := errorLogs(buf); len(errs) != 0 {
		t.Fatalf("a clean streamed speech call logged at Error: %+v", errs)
	}
}

// Everything short of a clean completion bills nothing -- and, where the row would
// otherwise look like a success, says so at Error (ADR-041), once -- while the client
// still receives every frame the upstream sent: the done event alone, [DONE] alone,
// [DONE] ahead of the done event, no terminal event at all, a delta whose audio cannot
// be decoded (its bytes cannot be counted, so the count cannot be trusted), a stream
// that finished having produced no audio, and a connection torn down mid-stream.
func TestSpeechStreamLoudZeroOnIncompleteStream(t *testing.T) {
	pcm := speechPCM(1000, 1)
	delta := speechFrameDelta(pcm)
	badDelta := "data: {\"type\":\"speech.audio.delta\",\"audio\":\"***not base64***\"}\n\n"
	for _, tc := range []struct {
		name string
		// answer writes the upstream's (incomplete) stream.
		answer func(w http.ResponseWriter)
		// wantStatus is the usage row's status: a body that ends cleanly is a success
		// that billed nothing, one that broke off is a failed relay.
		wantStatus string
		wantBody   string
	}{
		{"deltas and then nothing", func(w http.ResponseWriter) { _, _ = io.WriteString(w, delta) }, "success", delta},
		{"the done event without [DONE]", func(w http.ResponseWriter) { _, _ = io.WriteString(w, delta+speechSSEDone) }, "success", delta + speechSSEDone},
		{"[DONE] without the done event", func(w http.ResponseWriter) { _, _ = io.WriteString(w, delta+speechSSEEnd) }, "success", delta + speechSSEEnd},
		{"[DONE] ahead of the done event", func(w http.ResponseWriter) {
			_, _ = io.WriteString(w, delta+speechSSEEnd+speechSSEDone)
		}, "success", delta + speechSSEEnd + speechSSEDone},
		{"an empty stream", func(http.ResponseWriter) {}, "success", ""},
		{"a delta that cannot be decoded", func(w http.ResponseWriter) {
			_, _ = io.WriteString(w, delta+badDelta+speechSSEDone+speechSSEEnd)
		}, "success", delta + badDelta + speechSSEDone + speechSSEEnd},
		{"a complete stream that carried no audio", func(w http.ResponseWriter) {
			_, _ = io.WriteString(w, speechSSEDone+speechSSEEnd)
		}, "success", speechSSEDone + speechSSEEnd},
		{"a connection torn down mid-stream", func(w http.ResponseWriter) {
			_, _ = io.WriteString(w, delta)
			w.(http.Flusher).Flush()
			panic(http.ErrAbortHandler)
		}, "error", delta},
	} {
		t.Run(tc.name, func(t *testing.T) {
			buf, restore := withCapturedSlog(t)
			t.Cleanup(restore)
			up, _ := newScriptedSpeechUpstream(t, func(_ int, w http.ResponseWriter, _ speechUpstreamCall) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				tc.answer(w)
			})
			srv := newSpeechStreamingTestServer(t, up.URL)

			rec := postSpeech(t, srv, `{"model":"tts-voice","input":"hi","stream":true}`)

			if rec.Code != http.StatusOK || rec.Body.String() != tc.wantBody {
				t.Fatalf("response = %d %q, want 200 and the frames the upstream sent, %q", rec.Code, rec.Body.String(), tc.wantBody)
			}
			ev := onlyUsageEvent(t, srv)
			if ev.Status != tc.wantStatus {
				t.Fatalf("usage row status = %q, want %q", ev.Status, tc.wantStatus)
			}
			requireStreamedSpeechRow(t, ev, usage.BillingUnitAudioSecond, 0) // never audio_bytes, never a partial count
			if tc.wantStatus == "success" {
				errs := errorLogs(buf)
				if len(errs) != 1 || !strings.Contains(errs[0].Msg, "streamed speech") {
					t.Fatalf("Error logs = %+v, want exactly one saying the streamed speech call billed zero", errs)
				}
			}
		})
	}
}

// A stream that is torn down after it has delivered audio is a FAILED relay billed
// nothing, never a partial over-bill: the row is an error row (the copy error is its
// code), in the endpoint's own audio_second unit, with a zero quantity -- for the SSE
// shape and for raw PCM alike. The client keeps the bytes it had received.
func TestSpeechStreamAbortedMidStreamIsAnErrorRowBilledZero(t *testing.T) {
	pcm := speechPCM(1000, 1)
	for _, tc := range []struct {
		name        string
		contentType string
		sent        []byte
		body        string
	}{
		{"an event stream", "text/event-stream", []byte(speechFrameDelta(pcm)), `{"model":"tts-voice","input":"hi","stream":true}`},
		{"raw pcm", "application/octet-stream", pcm, `{"model":"tts-voice","input":"hi","stream":true,"stream_format":"audio","response_format":"pcm"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up, _ := newScriptedSpeechUpstream(t, func(_ int, w http.ResponseWriter, _ speechUpstreamCall) {
				w.Header().Set("Content-Type", tc.contentType)
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write(tc.sent)
				w.(http.Flusher).Flush()
				panic(http.ErrAbortHandler)
			})
			srv := newSpeechStreamingTestServer(t, up.URL)

			rec := postSpeech(t, srv, tc.body)

			if rec.Body.String() != string(tc.sent) {
				t.Fatalf("body = %q, want the bytes that arrived before the abort", rec.Body.String())
			}
			ev := onlyUsageEvent(t, srv)
			if ev.Status != "error" || ev.ErrorCode != "provider.stream_copy_error" {
				t.Fatalf("usage row = %q/%q, want error/provider.stream_copy_error", ev.Status, ev.ErrorCode)
			}
			requireStreamedSpeechRow(t, ev, usage.BillingUnitAudioSecond, 0)
		})
	}
}

// A client that stops listening mid-stream (the player was closed) is the routine way a
// stream ends early: the row is an error row billed zero, coded provider.client_disconnected,
// and it is NOT logged at Error -- an operator's Error log is for the stream that
// billed zero while it looked like a success.
func TestSpeechStreamClientDisconnectBillsNothingWithoutAnError(t *testing.T) {
	buf, restore := withCapturedSlog(t)
	t.Cleanup(restore)
	release := make(chan struct{})
	up, _ := newScriptedSpeechUpstream(t, func(_ int, w http.ResponseWriter, _ speechUpstreamCall) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, speechFrameDelta(speechPCM(500, 1)))
		w.(http.Flusher).Flush()
		select { // the client is gone long before this ends
		case <-release:
		case <-time.After(5 * time.Second):
		}
	})
	t.Cleanup(func() { close(release) }) // runs before the stub's own Close
	srv := newSpeechStreamingTestServer(t, up.URL)
	front := httptest.NewServer(srv)
	t.Cleanup(front.Close)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, front.URL+"/v1/audio/speech", strings.NewReader(`{"model":"tts-voice","input":"hi","stream":true}`))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer dev-secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /v1/audio/speech: %v", err)
	}
	readSSEFrame(t, bufio.NewReader(resp.Body)) // the first frame arrived: the stream is running
	cancel()
	resp.Body.Close()

	var events []usage.Event
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if events = srv.Usage.All(); len(events) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(events) != 1 {
		t.Fatalf("usage rows = %d, want 1 once the relay noticed the client was gone", len(events))
	}
	ev := events[0]
	if ev.Status != "error" || ev.ErrorCode != "provider.client_disconnected" {
		t.Fatalf("usage row = %q/%q, want error/provider.client_disconnected", ev.Status, ev.ErrorCode)
	}
	requireStreamedSpeechRow(t, ev, usage.BillingUnitAudioSecond, 0)
	for _, rec := range errorLogs(buf) {
		if strings.Contains(rec.Msg, "streamed speech") {
			t.Fatalf("a client that went away logged at Error: %+v", rec)
		}
	}
}

// stream_format=audio is answered with headerless raw PCM under application/octet-stream
// and no terminal frame: it is billed from the bytes the relay delivered, on a body the
// upstream closed cleanly.
func TestSpeechStreamRawBytesBilled(t *testing.T) {
	buf, restore := withCapturedSlog(t)
	t.Cleanup(restore)
	chunks := [][]byte{speechPCM(4096, 1), speechPCM(8192, 2), speechPCM(123, 3)}
	up, calls := newRawSpeechUpstream(t, chunks...)
	srv := newSpeechStreamingTestServer(t, up.URL)

	rec := postSpeech(t, srv, `{"model":"tts-voice","input":"Hello there.","stream":true,"stream_format":"audio","response_format":"pcm"}`)

	want := bytes.Join(chunks, nil)
	if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), want) {
		t.Fatalf("response = %d (%d bytes), want the upstream's %d raw bytes verbatim", rec.Code, rec.Body.Len(), len(want))
	}
	if got := rec.Header().Get("Content-Type"); got != "application/octet-stream" {
		t.Fatalf("Content-Type = %q, want the upstream's application/octet-stream", got)
	}
	sent := speechUpstreamFields(t, drainSpeechUpstreamCalls(calls)[0])
	if sent["stream"] != true || sent["stream_format"] != "audio" || sent["response_format"] != "pcm" {
		t.Fatalf("upstream body = %v, want stream, stream_format and response_format relayed as the client sent them", sent)
	}
	ev := onlyUsageEvent(t, srv)
	if ev.Status != "success" || !ev.Stream {
		t.Fatalf("usage row = %q stream=%v, want a streamed success", ev.Status, ev.Stream)
	}
	requireStreamedSpeechRow(t, ev, usage.BillingUnitAudioBytes, float64(len(want)))
	if errs := errorLogs(buf); len(errs) != 0 {
		t.Fatalf("a clean raw stream logged at Error: %+v", errs)
	}
}

// A raw body that ends having carried no byte at all is no synthesis: billed zero, loudly.
func TestSpeechStreamRawEmptyBodyIsALoudZero(t *testing.T) {
	buf, restore := withCapturedSlog(t)
	t.Cleanup(restore)
	up, _ := newRawSpeechUpstream(t)
	srv := newSpeechStreamingTestServer(t, up.URL)

	rec := postSpeech(t, srv, `{"model":"tts-voice","input":"hi","stream":true,"stream_format":"audio"}`)

	if rec.Code != http.StatusOK || rec.Body.Len() != 0 {
		t.Fatalf("response = %d (%d bytes), want an empty 200", rec.Code, rec.Body.Len())
	}
	ev := onlyUsageEvent(t, srv)
	if ev.Status != "success" {
		t.Fatalf("usage row status = %q, want success", ev.Status)
	}
	requireStreamedSpeechRow(t, ev, usage.BillingUnitAudioSecond, 0)
	if errs := errorLogs(buf); len(errs) != 1 || !strings.Contains(errs[0].Msg, "streamed speech") {
		t.Fatalf("Error logs = %+v, want exactly one saying the streamed speech call billed zero", errs)
	}
}

// stream=true against a model that is not in streaming mode: audio.cpp answers 500
// "speech streaming requires a model configured with mode=streaming". The gateway holds
// the whole request already, so it sends it ONCE more with the stream fields dropped
// and the client gets the one-shot WAV, billed like any buffered synthesis from its
// duration header. The mapped-name rewrite applies to the retry as to the first
// attempt; the first attempt leaves no row of its own.
func TestSpeechStreamRetriesBufferedOn500(t *testing.T) {
	wav := speechTestWAV()
	// How many requests the gateway shows as in flight while each upstream call is
	// answered: the refused attempt must be gone from the active list before its retry
	// runs, or one synthesis would count against its server twice.
	var gateway atomic.Pointer[Server]
	var activeAtCall [2]atomic.Int32
	up, calls := newScriptedSpeechUpstream(t, func(n int, w http.ResponseWriter, _ speechUpstreamCall) {
		w.Header().Set("Content-Type", "application/json")
		if n <= len(activeAtCall) {
			activeAtCall[n-1].Store(int32(len(gateway.Load().Active.Snapshot())))
		}
		if n == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, speechModeMismatchBody)
			return
		}
		w.Header().Set("Content-Type", "audio/wav")
		w.Header().Set("X-AudioCPP-Audio-Duration-Ms", "1500")
		_, _ = w.Write(wav)
	})
	srv := newSpeechStreamingTestServer(t, up.URL)
	gateway.Store(srv)

	rec := postSpeech(t, srv, `{"model":"tts-voice","input":"Hello there.","voice":"alba","stream":true,"stream_format":"audio","response_format":"pcm"}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 from the buffered retry; body = %q", rec.Code, rec.Body.Bytes())
	}
	if got := rec.Header().Get("Content-Type"); got != "audio/wav" {
		t.Fatalf("Content-Type = %q, want audio/wav (the one-shot answer, not a stream)", got)
	}
	if !bytes.Equal(rec.Body.Bytes(), wav) {
		t.Fatalf("body = %x, want the one-shot WAV byte for byte", rec.Body.Bytes())
	}
	all := drainSpeechUpstreamCalls(calls)
	if len(all) != 2 {
		t.Fatalf("upstream calls = %d, want exactly 2 (the streamed attempt, then one buffered retry)", len(all))
	}
	first, second := speechUpstreamFields(t, all[0]), speechUpstreamFields(t, all[1])
	if first["stream"] != true || first["stream_format"] != "audio" || first["response_format"] != "pcm" {
		t.Fatalf("first call = %v, want the stream the verdict allowed", first)
	}
	for _, field := range []string{"stream", "stream_format", "response_format"} {
		if v, ok := second[field]; ok {
			t.Fatalf("retry %s = %v, want the field dropped", field, v)
		}
	}
	for i, call := range []map[string]any{first, second} {
		if call["model"] != speechUpstreamModel || call["input"] != "Hello there." || call["voice"] != "alba" {
			t.Fatalf("call %d = %v, want the mapped model and every other field untouched", i+1, call)
		}
		if all[i].path != "/v1/audio/speech" || all[i].contentType != "application/json" {
			t.Fatalf("call %d = %s (%s), want JSON on /v1/audio/speech", i+1, all[i].path, all[i].contentType)
		}
	}
	if first, second := activeAtCall[0].Load(), activeAtCall[1].Load(); first != 1 || second != 1 {
		t.Fatalf("active requests while the upstream answered = %d, then %d, want 1 each: the refused attempt must leave the active list before its retry", first, second)
	}
	ev := onlyUsageEvent(t, srv) // the refused streamed attempt is not a request of its own
	if ev.Status != "success" || ev.HTTPStatus != http.StatusOK || ev.Stream {
		t.Fatalf("usage row = %q/%d stream=%v, want a buffered success", ev.Status, ev.HTTPStatus, ev.Stream)
	}
	requireStreamedSpeechRow(t, ev, usage.BillingUnitAudioSecond, 1.5)
}

// The refusal is retried EXACTLY once: a buffered retry the upstream refuses again is
// relayed as the error it is, from two upstream calls and one row, and not looped on.
func TestSpeechStreamRetriesTheRefusalExactlyOnce(t *testing.T) {
	up, calls := newScriptedSpeechUpstream(t, func(_ int, w http.ResponseWriter, _ speechUpstreamCall) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, speechModeMismatchBody)
	})
	srv := newSpeechStreamingTestServer(t, up.URL)

	rec := postSpeech(t, srv, `{"model":"tts-voice","input":"hi","stream":true}`)

	if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), "mode=streaming") {
		t.Fatalf("response = %d %s, want the upstream's refusal of the retry relayed", rec.Code, rec.Body.String())
	}
	if all := drainSpeechUpstreamCalls(calls); len(all) != 2 {
		t.Fatalf("upstream calls = %d, want 2: one stream, one retry, no loop", len(all))
	}
	ev := onlyUsageEvent(t, srv)
	if ev.Status != "error" || ev.ErrorCode != "upstream.500" {
		t.Fatalf("usage row = %q/%q, want error/upstream.500", ev.Status, ev.ErrorCode)
	}
	requireStreamedSpeechRow(t, ev, usage.BillingUnitAudioSecond, 0)
}

// Only the mode-mismatch 500 of a STREAMED request is turned into a retry. Every other
// upstream answer -- a different 500, the transcription's sentence (a speech request is
// not refused for that), the speech sentence under another status -- reaches the client
// as it does today, from ONE upstream call, as an error row in audio_second.
func TestSpeechStreamSurfacesOtherUpstreamErrors(t *testing.T) {
	const words = "speech streaming requires a model configured with mode=streaming"
	for _, tc := range []struct {
		name       string
		status     int
		respBody   string
		wantInBody string
	}{
		{"another 500", http.StatusInternalServerError, `{"error":{"message":"out of memory","type":"server_error"}}`, "out of memory"},
		{"a plain-string 500", http.StatusInternalServerError, `{"error":"model crashed"}`, "model crashed"},
		{"the transcription sentence", http.StatusInternalServerError, transcriptionModeMismatchBody, "transcription streaming requires"},
		{"the speech words under a 400", http.StatusBadRequest, `{"error":{"message":"` + words + `","type":"invalid_request_error"}}`, words},
		{"the speech words under a 503", http.StatusServiceUnavailable, `{"error":{"message":"` + words + `","type":"server_error"}}`, words},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up, calls := newScriptedSpeechUpstream(t, func(_ int, w http.ResponseWriter, _ speechUpstreamCall) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.respBody)
			})
			srv := newSpeechStreamingTestServer(t, up.URL)

			rec := postSpeech(t, srv, `{"model":"tts-voice","input":"hi","stream":true}`)

			if rec.Code != tc.status || !strings.Contains(rec.Body.String(), tc.wantInBody) {
				t.Fatalf("response = %d %s, want the upstream's %d carrying %q", rec.Code, rec.Body.String(), tc.status, tc.wantInBody)
			}
			if all := drainSpeechUpstreamCalls(calls); len(all) != 1 {
				t.Fatalf("upstream calls = %d, want 1: only the mode-mismatch 500 is retried", len(all))
			}
			ev := onlyUsageEvent(t, srv)
			if ev.Status != "error" || !ev.Stream {
				t.Fatalf("usage row = %q stream=%v, want the error of a streamed call", ev.Status, ev.Stream)
			}
			requireStreamedSpeechRow(t, ev, usage.BillingUnitAudioSecond, 0)
		})
	}
}

// A request that did not ask for a stream is never retried: the same 500 reaches the
// client as the upstream's error, from one call.
func TestSpeechWithoutStreamIsNeverRetried(t *testing.T) {
	up, calls := newScriptedSpeechUpstream(t, func(_ int, w http.ResponseWriter, _ speechUpstreamCall) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, speechModeMismatchBody)
	})
	srv := newSpeechStreamingTestServer(t, up.URL)

	rec := postSpeech(t, srv, `{"model":"tts-voice","input":"hi"}`)

	if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), "mode=streaming") {
		t.Fatalf("response = %d %s, want the upstream's 500 verbatim", rec.Code, rec.Body.String())
	}
	if all := drainSpeechUpstreamCalls(calls); len(all) != 1 {
		t.Fatalf("upstream calls = %d, want 1: a request that did not stream is not retried", len(all))
	}
}

// A stream asking for wav is refused up front: audio.cpp answers it with a 500
// ("streaming speech currently supports response_format=pcm"), so the gateway says so
// itself, 400, before anything is resolved or relayed -- whatever the model's verdict.
func TestSpeechRejectsStreamWithWavFormat(t *testing.T) {
	for _, model := range []string{speechStreamingModel, speechOfflineModel} {
		t.Run(model, func(t *testing.T) {
			up, calls := newSpeechUpstream(t, speechTestWAV())
			srv := newSpeechStreamingTestServer(t, up.URL)

			rec := postSpeech(t, srv, `{"model":"`+model+`","input":"hi","stream":true,"response_format":"wav"}`)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
			}
			requireErrorCode(t, rec.Body.String(), "speech.stream_unsupported")
			if !strings.Contains(rec.Body.String(), "pcm") {
				t.Fatalf("body = %s, want the refusal to say what a stream is", rec.Body.String())
			}
			requireNoSpeechUpstreamCall(t, calls)
			if events := srv.Usage.All(); len(events) != 0 {
				t.Fatalf("a refused request recorded usage %+v, want none", events)
			}
		})
	}
}

// The capture of a streamed synthesis is the redacted binary summary a speech response
// always gets (it is audio output), not the event text and not an assembled
// transcript: the stored body says what the response was, how large the relay made it,
// and nothing of the base64 audio. The stream's text/event-stream type must not make
// the capture store it as text.
func TestSpeechStreamCaptureIsTheRedactedSummary(t *testing.T) {
	deltas := [][]byte{speechPCM(1500, 1), speechPCM(1500, 2)}
	frames := speechFrameDelta(deltas[0]) + speechFrameDelta(deltas[1]) + speechSSEDone + speechSSEEnd
	raw := bytes.Join(deltas, nil)
	for _, tc := range []struct {
		name        string
		body        string
		upstream    func(t *testing.T) *httptest.Server
		contentType string
		wantSize    int
	}{
		{"an event stream", `{"model":"tts-voice","input":"Hello there.","stream":true}`, func(t *testing.T) *httptest.Server {
			up, _ := newSSESpeechUpstream(t, speechFrameDelta(deltas[0]), speechFrameDelta(deltas[1]), speechSSEDone, speechSSEEnd)
			return up
		}, "text/event-stream", len(frames)},
		{"raw pcm", `{"model":"tts-voice","input":"Hello there.","stream":true,"stream_format":"audio"}`, func(t *testing.T) *httptest.Server {
			up, _ := newRawSpeechUpstream(t, deltas...)
			return up
		}, "application/octet-stream", len(raw)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, cs := newSpeechStreamingCaptureServer(t, tc.upstream(t).URL)

			rec := postSpeech(t, srv, tc.body)

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
			if strings.Contains(env.RespBody, "speech.audio") || strings.Contains(env.RespBody, base64.StdEncoding.EncodeToString(deltas[0][:60])) {
				t.Fatalf("the capture holds the stream's content: %q", env.RespBody)
			}
			doc := decodeCapturedBinaryBody(t, env.RespBody)
			if doc.ContentType != tc.contentType || doc.Size != int64(tc.wantSize) || !doc.Redacted {
				t.Fatalf("summary = %+v, want %s, %d bytes, redacted", doc, tc.contentType, tc.wantSize)
			}
			if env.Truncated {
				t.Fatal("Truncated = true, want false: nothing was cut off")
			}
			if !strings.Contains(env.ReqBody, `"input":"Hello there."`) {
				t.Fatalf("request body = %q, want the text to synthesize captured", env.ReqBody)
			}
		})
	}
}

// A first frame that comes later than the idle timeout is NOT cut: a streaming model's
// time to first byte (a cold start, a model still loading) can exceed any idle timeout
// sized for a stream that is already flowing, so until the first byte the relay gets the
// application's whole timeout -- the budget a one-shot synthesis has end to end -- and
// only from then on the idle cadence. The stub answers the stream's headers at once and
// holds the first frame back for 3.5x the idle timeout.
func TestSpeechStreamWaitsForAFirstFrameBeyondTheIdleTimeout(t *testing.T) {
	pcm := speechPCM(1000, 1)
	up, _ := newScriptedSpeechUpstream(t, func(_ int, w http.ResponseWriter, _ speechUpstreamCall) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		flusher.Flush()
		time.Sleep(350 * time.Millisecond)
		_, _ = io.WriteString(w, speechFrameDelta(pcm)+speechSSEDone+speechSSEEnd)
		flusher.Flush()
	})
	srv := newSpeechStreamingTestServer(t, up.URL)
	srv.streamIdleTimeout = 100 * time.Millisecond

	resp, err := postSpeechOverTheWire(t, srv, `{"model":"tts-voice","input":"hi","stream":true}`)
	if err != nil {
		t.Fatalf("POST /v1/audio/speech: %v", err)
	}
	defer resp.Body.Close()
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading the stream: %v", err)
	}

	if want := speechFrameDelta(pcm) + speechSSEDone + speechSSEEnd; string(got) != want {
		t.Fatalf("stream = %q, want the whole of it: a slow first frame is not an idle stream", got)
	}
	ev := onlyUsageEvent(t, srv)
	if ev.Status != "success" || ev.ErrorCode != "" {
		t.Fatalf("usage row = %q/%q, want a success: the idle timeout must not fire before the first byte", ev.Status, ev.ErrorCode)
	}
	requireStreamedSpeechRow(t, ev, usage.BillingUnitAudioBytes, 1000)
}

// The grace ends at the first byte: once a frame has arrived, silence longer than the
// idle timeout ends the stream with provider.stream_idle_timeout, billed zero (the
// stream never completed).
func TestSpeechStreamIdleTimeoutAppliesAfterTheFirstFrame(t *testing.T) {
	release := make(chan struct{})
	up, _ := newScriptedSpeechUpstream(t, func(_ int, w http.ResponseWriter, _ speechUpstreamCall) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, speechFrameDelta(speechPCM(500, 1)))
		w.(http.Flusher).Flush()
		select { // far past the 100ms idle timeout: the relay cuts the stream first
		case <-release:
		case <-time.After(5 * time.Second):
		}
	})
	t.Cleanup(func() { close(release) }) // runs before the stub's own Close
	srv := newSpeechStreamingTestServer(t, up.URL)
	srv.streamIdleTimeout = 100 * time.Millisecond

	resp, err := postSpeechOverTheWire(t, srv, `{"model":"tts-voice","input":"hi","stream":true}`)
	if err != nil {
		t.Fatalf("POST /v1/audio/speech: %v", err)
	}
	defer resp.Body.Close()
	_, _ = io.ReadAll(resp.Body)

	ev := onlyUsageEvent(t, srv)
	if ev.Status != "error" || ev.ErrorCode != "provider.stream_idle_timeout" {
		t.Fatalf("usage row = %q/%q, want error/provider.stream_idle_timeout", ev.Status, ev.ErrorCode)
	}
	requireStreamedSpeechRow(t, ev, usage.BillingUnitAudioSecond, 0)
}

// --- The scan, the setter and the helpers behind the relay ------------------

// scanSpeech feeds stream to a scan in chunks of size bytes (the whole stream at once
// for size 0), finishes it, and returns it.
func scanSpeech(scan *speechStreamScan, stream string, size int) *speechStreamScan {
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

func newTestSpeechScan() *speechStreamScan { return &speechStreamScan{maxLine: 1 << 20} }

func newTestRawSpeechScan() *speechStreamScan { return &speechStreamScan{raw: true} }

// What the scan concludes -- the bytes it counted and whether the stream completed --
// does not depend on how the stream was cut into chunks, or on the line endings it uses.
func TestSpeechStreamScanReadsAStreamWhateverItsChunking(t *testing.T) {
	stream := speechFrameDelta(speechPCM(1000, 1)) + ": a comment\n" + "event: speech\n" + speechFrameDelta(speechPCM(333, 2)) + speechFrameDelta(speechPCM(1, 3)) + speechSSEDone + speechSSEEnd
	for name, text := range map[string]string{
		"LF":   stream,
		"CRLF": strings.ReplaceAll(stream, "\n", "\r\n"),
	} {
		for _, size := range []int{0, 1, 2, 3, 7, 64, 1000} {
			scan := scanSpeech(newTestSpeechScan(), text, size)
			if !scan.complete() {
				t.Fatalf("%s in chunks of %d: complete = false, want true", name, size)
			}
			if got := scan.pcmBytes(); got != 1000+333+1 {
				t.Fatalf("%s in chunks of %d: pcmBytes = %d, want the decoded 1334", name, size, got)
			}
		}
	}
}

// Only the terminal events in their order complete a stream, and anything that is not
// one is ignored rather than mistaken for one.
func TestSpeechStreamScanCompletion(t *testing.T) {
	delta := speechFrameDelta(speechPCM(10, 1))
	for _, tc := range []struct {
		name     string
		stream   string
		complete bool
	}{
		{"done then [DONE]", delta + speechSSEDone + speechSSEEnd, true},
		{"[DONE] as the last line, with no newline", delta + speechSSEDone + "data: [DONE]", true},
		{"[DONE] without a space after the colon", delta + speechSSEDone + "data:[DONE]\n\n", true},
		{"the done event only", delta + speechSSEDone, false},
		{"[DONE] only", delta + speechSSEEnd, false},
		{"[DONE] ahead of the done event", delta + speechSSEEnd + speechSSEDone, false},
		{"nothing", "", false},
		{"the done event cut mid-frame", delta + speechSSEDone[:30], false},
		{"a done event that is not JSON", delta + "data: {\"type\":\"speech.audio.done\"\n\n" + speechSSEEnd, false},
		{"another event type", delta + "data: {\"type\":\"speech.audio.segment\"}\n\n" + speechSSEEnd, false},
		{"the done event as a comment", delta + ": " + speechSSEDone + speechSSEEnd, false},
		{"[DONE] carried in a field that is not data", delta + speechSSEDone + "event: [DONE]\n\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, size := range []int{0, 1} {
				if got := scanSpeech(newTestSpeechScan(), tc.stream, size).complete(); got != tc.complete {
					t.Fatalf("complete = %v (chunks of %d), want %v", got, size, tc.complete)
				}
			}
		})
	}
}

// A delta the scan cannot account for makes the stream NOT complete, whatever follows
// it: the bytes the client was relayed are more than the bytes counted, and billing the
// count would be a silent under-bill. It still counts what it could read.
func TestSpeechStreamScanDoesNotTrustACountItCouldNotMake(t *testing.T) {
	good := speechFrameDelta(speechPCM(100, 1))
	for _, tc := range []struct {
		name  string
		delta string
	}{
		{"audio that is not base64", "data: {\"type\":\"speech.audio.delta\",\"audio\":\"***\"}\n\n"},
		{"base64 cut mid-quantum", "data: {\"type\":\"speech.audio.delta\",\"audio\":\"QUJDRA\"}\n\n"},
		{"a delta that is not JSON", "data: {\"type\":\"speech.audio.delta\",\"audio\":\n\n"},
		{"audio that is no string", "data: {\"type\":\"speech.audio.delta\",\"audio\":12}\n\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scan := scanSpeech(newTestSpeechScan(), good+tc.delta+good+speechSSEDone+speechSSEEnd, 0)
			if scan.complete() {
				t.Fatal("complete = true, want false: a delta that cannot be counted must not be billed around")
			}
			if got := scan.pcmBytes(); got != 200 {
				t.Fatalf("pcmBytes = %d, want the 200 bytes of the two good deltas", got)
			}
		})
	}
}

// A delta without an audio field is an empty one, not a failure: nothing to count, and
// the stream can still complete.
func TestSpeechStreamScanCountsAnEmptyDeltaAsNothing(t *testing.T) {
	scan := scanSpeech(newTestSpeechScan(), "data: {\"type\":\"speech.audio.delta\"}\n\n"+"data: {\"type\":\"speech.audio.delta\",\"audio\":\"\"}\n\n"+speechFrameDelta(speechPCM(5, 1))+speechSSEDone+speechSSEEnd, 0)
	if !scan.complete() || scan.pcmBytes() != 5 {
		t.Fatalf("complete = %v, pcmBytes = %d, want a completed stream of 5 bytes", scan.complete(), scan.pcmBytes())
	}
}

// A line that has to be carried across chunks and outgrows maxLine is given up whole --
// and, unlike a transcript, what it carried is AUDIO that was relayed and is now
// uncounted, so the stream can no longer be called complete: billing fails closed. The
// lines after it are still read, and the carry stays bounded.
func TestSpeechStreamScanDropsALineBeyondItsBoundAndFailsClosed(t *testing.T) {
	long := speechFrameDelta(speechPCM(300, 1)) // one line, far past 100 bytes
	stream := long + speechFrameDelta(speechPCM(20, 2)) + speechSSEDone + speechSSEEnd
	for _, size := range []int{1, 50, 150} {
		scan := &speechStreamScan{maxLine: 100}
		for rest := []byte(stream); len(rest) > 0; {
			n := min(size, len(rest))
			scan.feed(rest[:n])
			rest = rest[n:]
			if len(scan.carry) > 100 {
				t.Fatalf("chunks of %d: carry holds %d bytes, want it bounded by the line limit", size, len(scan.carry))
			}
		}
		scan.finish()
		if scan.complete() {
			t.Fatalf("chunks of %d: complete = true, want false: a dropped delta is audio nobody counted", size)
		}
		if got := scan.pcmBytes(); got != 20 {
			t.Fatalf("chunks of %d: pcmBytes = %d, want the 20 bytes of the line after the dropped one", size, got)
		}
	}
}

// The raw shape has no frames: its bytes ARE the audio, counted as they pass, and it
// is complete only when the upstream closed the body cleanly -- the copier says so with
// cleanClose, and an error mid-stream never does.
func TestSpeechStreamScanRaw(t *testing.T) {
	scan := newTestRawSpeechScan()
	scan.feed(speechPCM(4096, 1))
	scan.feed(speechPCM(1, 2))
	scan.feed(nil)
	scan.finish()
	if got := scan.pcmBytes(); got != 4097 {
		t.Fatalf("pcmBytes = %d, want the 4097 bytes that passed", got)
	}
	if scan.complete() {
		t.Fatal("a raw body that was never closed cleanly is complete, want not")
	}
	scan.cleanClose()
	if !scan.complete() {
		t.Fatal("a raw body closed cleanly is not complete, want complete")
	}
	// What looks like frames in a raw body is audio like any other byte.
	raw := newTestRawSpeechScan()
	raw.feed([]byte(speechFrameDelta(speechPCM(10, 1)) + speechSSEDone + speechSSEEnd))
	raw.cleanClose()
	if raw.pcmBytes() != int64(len(speechFrameDelta(speechPCM(10, 1))+speechSSEDone+speechSSEEnd)) {
		t.Fatalf("pcmBytes = %d, want every byte of the raw body counted, frames or not", raw.pcmBytes())
	}
}

// The clean close of an event stream is not what completes it: only its terminal
// events do. (cleanClose is the raw shape's signal.)
func TestSpeechStreamScanEventStreamIgnoresACleanClose(t *testing.T) {
	scan := scanSpeech(newTestSpeechScan(), speechFrameDelta(speechPCM(10, 1)), 0)
	scan.cleanClose()
	if scan.complete() {
		t.Fatal("an event stream with no terminal events completed on a clean close")
	}
}

// A relay that is not a streamed speech call carries no scan, and none of its methods
// may trip on that.
func TestSpeechStreamScanIsNilSafe(t *testing.T) {
	var scan *speechStreamScan
	scan.feed([]byte(speechFrameDelta(speechPCM(10, 1)) + speechSSEDone + speechSSEEnd))
	scan.finish()
	scan.cleanClose()
	if scan.complete() || scan.pcmBytes() != 0 {
		t.Fatalf("a nil scan: complete = %v, pcmBytes = %d, want false and 0", scan.complete(), scan.pcmBytes())
	}
	in := &captureInput{RespBody: []byte("kept")}
	scan.stampCapture(in)
	scan.stampCapture(nil)
	if string(in.RespBody) != "kept" || in.Endpoint != endpointChat {
		t.Fatalf("a nil scan changed the capture: %+v", in)
	}
}

// The shape of a streamed answer is read off the response's own Content-Type, and only
// the two types audio.cpp streams in are streams: any other answer to a stream request
// (the one-shot audio/wav, a JSON document) is no stream and is relayed as the document it is.
func TestNewSpeechStreamScanIsKeyedOnTheContentType(t *testing.T) {
	for _, tc := range []struct {
		contentType string
		wantScan    bool
		wantRaw     bool
	}{
		{"text/event-stream", true, false},
		{"text/event-stream; charset=utf-8", true, false},
		{"Text/Event-Stream", true, false},
		{"application/octet-stream", true, true},
		{"application/octet-stream; foo=bar", true, true},
		{"audio/wav", false, false},
		{"audio/L16;rate=24000", false, false},
		{"application/json", false, false},
		{"", false, false},
		{"not a media type", false, false},
	} {
		t.Run(tc.contentType, func(t *testing.T) {
			scan := newSpeechStreamScan(http.Header{"Content-Type": {tc.contentType}})
			if (scan != nil) != tc.wantScan {
				t.Fatalf("scan = %v, want a scan: %v", scan, tc.wantScan)
			}
			if scan != nil && scan.raw != tc.wantRaw {
				t.Fatalf("raw = %v, want %v", scan.raw, tc.wantRaw)
			}
		})
	}
}

// The capture of a streamed synthesis is stamped as speech's, which is what keeps the
// stream's text/event-stream body from being stored as text: the binary-speech backstop
// then redacts every 2xx.
func TestSpeechStreamScanStampsTheCaptureAsSpeech(t *testing.T) {
	scan := newTestSpeechScan()
	in := &captureInput{APIFlavor: apiFlavorSpeech, HTTPStatus: http.StatusOK, RespHeaders: http.Header{"Content-Type": {"text/event-stream"}}, RespBody: []byte("data: ...")}
	scan.stampCapture(in)
	if in.Endpoint != endpointSpeech || !isBinarySpeechResponse(in) {
		t.Fatalf("stamped capture: endpoint = %v, binary speech = %v, want speech and true", in.Endpoint, isBinarySpeechResponse(in))
	}
	if !decodeCapturedBinaryBodyIsSummary(mustCaptureBody(t, in)) {
		t.Fatal("the stamped capture's body is not the redacted summary")
	}
}

func mustCaptureBody(t *testing.T, in *captureInput) string {
	t.Helper()
	body, truncated := captureResponseBody(in, 4096)
	if truncated {
		t.Fatal("captureResponseBody reported a truncation of a skipped body")
	}
	return body
}

// The billing of a streamed call, decided in one place: a clean completion bills the
// bytes that were relayed in audio_bytes; anything else bills zero in the endpoint's
// own audio_second unit, and says so at Error exactly when the row would otherwise
// look like a success that billed nothing.
func TestSetStreamedSpeechBillingQuantity(t *testing.T) {
	ex := nativeExchange{id: "req_1", serverName: "audio-1", req: inference.Request{Model: "tts-voice"}}
	complete := func() *speechStreamScan {
		return scanSpeech(newTestSpeechScan(), speechFrameDelta(speechPCM(700, 1))+speechSSEDone+speechSSEEnd, 0)
	}
	for _, tc := range []struct {
		name         string
		scan         *speechStreamScan
		status       string
		wantUnit     string
		wantQuantity float64
		wantErrorLog bool
		untouched    bool
	}{
		{name: "a clean completion", scan: complete(), status: "success", wantUnit: usage.BillingUnitAudioBytes, wantQuantity: 700},
		{name: "a stream with no terminal events", scan: scanSpeech(newTestSpeechScan(), speechFrameDelta(speechPCM(700, 1)), 0), status: "success", wantUnit: usage.BillingUnitAudioSecond, wantErrorLog: true},
		{name: "a complete stream that carried no audio", scan: scanSpeech(newTestSpeechScan(), speechSSEDone+speechSSEEnd, 0), status: "success", wantUnit: usage.BillingUnitAudioSecond, wantErrorLog: true},
		{name: "a complete stream that failed on the way (an error row)", scan: complete(), status: "error", wantUnit: usage.BillingUnitAudioSecond},
		{name: "an incomplete stream that failed on the way", scan: scanSpeech(newTestSpeechScan(), speechFrameDelta(speechPCM(700, 1)), 0), status: "error", wantUnit: usage.BillingUnitAudioSecond},
		{name: "a raw stream closed cleanly", scan: func() *speechStreamScan {
			scan := newTestRawSpeechScan()
			scan.feed(speechPCM(900, 1))
			scan.cleanClose()
			return scan
		}(), status: "success", wantUnit: usage.BillingUnitAudioBytes, wantQuantity: 900},
		{name: "a raw stream that was never closed cleanly", scan: func() *speechStreamScan {
			scan := newTestRawSpeechScan()
			scan.feed(speechPCM(900, 1))
			return scan
		}(), status: "success", wantUnit: usage.BillingUnitAudioSecond, wantErrorLog: true},
		{name: "a raw stream that carried no byte", scan: func() *speechStreamScan {
			scan := newTestRawSpeechScan()
			scan.cleanClose()
			return scan
		}(), status: "success", wantUnit: usage.BillingUnitAudioSecond, wantErrorLog: true},
		{name: "no scan: not a streamed speech call", scan: nil, status: "success", untouched: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			buf, restore := withCapturedSlog(t)
			t.Cleanup(restore)
			meta := usageMeta{ReqPath: "/v1/audio/speech", BillingUnit: usage.BillingUnitAudioSecond}

			setStreamedSpeechBillingQuantity(&meta, ex, tc.scan, tc.status)

			wantUnit := tc.wantUnit
			if tc.untouched {
				wantUnit = usage.BillingUnitAudioSecond
			}
			if meta.BillingUnit != wantUnit || meta.BillingQuantity != tc.wantQuantity {
				t.Fatalf("billing = %v %q, want %v %q", meta.BillingQuantity, meta.BillingUnit, tc.wantQuantity, wantUnit)
			}
			errs := errorLogs(buf)
			if tc.wantErrorLog && (len(errs) != 1 || !strings.Contains(errs[0].Msg, "streamed speech")) {
				t.Fatalf("Error logs = %+v, want exactly one saying the streamed speech call billed zero", errs)
			}
			if !tc.wantErrorLog && len(errs) != 0 {
				t.Fatalf("Error logs = %+v, want none", errs)
			}
		})
	}
}

// The copier tells a raw scan that the upstream closed the body cleanly -- and only
// then: a read error mid-body is a failed relay, never a clean close.
func TestNativeCopierRelaysAndScansASpeechStreamWithoutATee(t *testing.T) {
	t.Run("an event stream", func(t *testing.T) {
		stream := speechFrameDelta(speechPCM(600, 1)) + speechFrameDelta(speechPCM(400, 2)) + speechSSEDone + speechSSEEnd
		rec := httptest.NewRecorder()
		scan := newTestSpeechScan()
		copier := &nativeCopier{w: rec, flusher: rec, capBytes: 4, speech: scan}

		if err := copier.run(strings.NewReader(stream)); err != nil {
			t.Fatalf("run: %v", err)
		}

		if rec.Body.String() != stream || copier.written != int64(len(stream)) {
			t.Fatalf("client got %d bytes (%d written), want the whole %d-byte stream", rec.Body.Len(), copier.written, len(stream))
		}
		if !scan.complete() || scan.pcmBytes() != 1000 {
			t.Fatalf("scan complete = %v, pcmBytes = %d, want a completed stream of 1000 bytes", scan.complete(), scan.pcmBytes())
		}
	})
	t.Run("raw pcm closed cleanly", func(t *testing.T) {
		raw := speechPCM(5000, 1)
		rec := httptest.NewRecorder()
		scan := newTestRawSpeechScan()
		copier := &nativeCopier{w: rec, flusher: rec, capBytes: 4, speech: scan}

		if err := copier.run(bytes.NewReader(raw)); err != nil {
			t.Fatalf("run: %v", err)
		}

		if !bytes.Equal(rec.Body.Bytes(), raw) || !scan.complete() || scan.pcmBytes() != 5000 {
			t.Fatalf("client got %d bytes, scan complete = %v, pcmBytes = %d, want 5000 and a clean completion", rec.Body.Len(), scan.complete(), scan.pcmBytes())
		}
	})
	t.Run("raw pcm cut by a read error", func(t *testing.T) {
		raw := speechPCM(5000, 1)
		rec := httptest.NewRecorder()
		scan := newTestRawSpeechScan()
		copier := &nativeCopier{w: rec, flusher: rec, capBytes: 4, speech: scan}

		err := copier.run(io.MultiReader(bytes.NewReader(raw), errReader{io.ErrUnexpectedEOF}))

		if err == nil {
			t.Fatal("run returned nil, want the read error")
		}
		if scan.complete() || scan.pcmBytes() != 5000 {
			t.Fatalf("scan complete = %v, pcmBytes = %d, want an incomplete scan that still counted what passed", scan.complete(), scan.pcmBytes())
		}
	})
}

// errReader fails every read with err.
type errReader struct{ err error }

func (r errReader) Read([]byte) (int, error) { return 0, r.err }

// A streamed speech call's first byte is its time to first token, so it is given the
// same cold-start budget a streamed transcription's is: the larger of the application's
// timeout and the idle timeout. Every other stream keeps the idle timeout.
func TestStreamFirstByteBudgetCoversSpeech(t *testing.T) {
	const idle = 100 * time.Millisecond
	target := routing.Target{Timeout: 30 * time.Second}
	for _, tc := range []struct {
		name     string
		endpoint sessionEndpoint
		want     time.Duration
	}{
		{"speech", endpointSpeech, 30 * time.Second},
		{"transcription", endpointTranscription, 30 * time.Second},
		{"chat", endpointChat, idle},
		{"responses", endpointResponses, idle},
	} {
		if got := streamFirstByteBudget(nativeRelay{endpoint: tc.endpoint, target: target}, idle); got != tc.want {
			t.Errorf("%s: first-byte budget = %v, want %v", tc.name, got, tc.want)
		}
	}
	if got := streamFirstByteBudget(nativeRelay{endpoint: endpointSpeech, target: routing.Target{Timeout: time.Second}}, time.Minute); got != time.Minute {
		t.Errorf("speech with a short application timeout: budget = %v, want the idle timeout, the larger of the two", got)
	}
}

// The sentence audio.cpp's speech refusal carries is its own, and the peek is keyed on
// the sentence it is given: the transcription's sentence is not the speech one and the
// reverse (the speech sentence happens to END in the transcription's words, so a
// substring test on the shorter one would take both), and no sentence at all matches
// nothing.
func TestPeekStreamingRefusalIsKeyedOnTheSentence(t *testing.T) {
	for _, tc := range []struct {
		name     string
		body     string
		sentence string
		want     bool
	}{
		{"the speech refusal", speechModeMismatchBody, speechStreamingRefusal, true},
		{"the speech refusal as a plain string", `{"error":"speech streaming requires a model configured with mode=streaming"}`, speechStreamingRefusal, true},
		{"the transcription refusal under the speech sentence", transcriptionModeMismatchBody, speechStreamingRefusal, false},
		{"another 500 under the speech sentence", `{"error":{"message":"out of memory"}}`, speechStreamingRefusal, false},
		{"the speech refusal under the transcription sentence", speechModeMismatchBody, transcriptionStreamingRefusal, true}, // the shorter sentence is a substring of it: why the relay carries the sentence it was built for
		{"the transcription refusal", transcriptionModeMismatchBody, transcriptionStreamingRefusal, true},
		{"no sentence", speechModeMismatchBody, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := &provider.ProxyResponse{StatusCode: http.StatusInternalServerError, Body: io.NopCloser(strings.NewReader(tc.body))}

			if got := peekStreamingRefusal(resp, tc.sentence); got != tc.want {
				t.Fatalf("peekStreamingRefusal = %v, want %v", got, tc.want)
			}
			rest, err := io.ReadAll(resp.Body)
			if err != nil || string(rest) != tc.body {
				t.Fatalf("body after the look = %q (%v), want the original %q", rest, err, tc.body)
			}
		})
	}
}

// The buffered twin of a streamed request: the client's JSON without the fields only a
// stream has a meaning for, and every other field -- numbers at full precision
// included -- as the client wrote it.
func TestSpeechBufferedBody(t *testing.T) {
	t.Run("the stream-only fields are dropped, whatever their case", func(t *testing.T) {
		body, err := speechBufferedBody([]byte(`{"model":"gw","input":"hi","stream":true,"stream_format":"audio","response_format":"pcm","Stream":true,"STREAM_FORMAT":"sse","Response_Format":"pcm","voice":"alba"}`))
		if err != nil {
			t.Fatalf("speechBufferedBody: %v", err)
		}
		var got map[string]any
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatalf("the buffered body is not JSON: %v (%s)", err, body)
		}
		if len(got) != 3 || got["model"] != "gw" || got["input"] != "hi" || got["voice"] != "alba" {
			t.Fatalf("buffered body = %s, want exactly model, input and voice", body)
		}
	})
	t.Run("every other value survives untouched", func(t *testing.T) {
		body, err := speechBufferedBody([]byte(`{"model":"gw","input":"a <b> & é \"quoted\"","speed":1.25,"seed":9007199254740993,"extra":{"nested":[1,2,{"x":null}]},"stream":true}`))
		if err != nil {
			t.Fatalf("speechBufferedBody: %v", err)
		}
		for _, want := range []string{`"speed":1.25`, `"seed":9007199254740993`, `"extra":{"nested":[1,2,{"x":null}]}`} {
			if !strings.Contains(string(body), want) {
				t.Fatalf("buffered body = %s, want it to carry %s", body, want)
			}
		}
		var got struct{ Input string }
		if err := json.Unmarshal(body, &got); err != nil || got.Input != `a <b> & é "quoted"` {
			t.Fatalf("input = %q (%v), want the client's text intact", got.Input, err)
		}
	})
	t.Run("a body without stream fields is the same document", func(t *testing.T) {
		body, err := speechBufferedBody([]byte(`{"model":"gw","input":"hi"}`))
		if err != nil || string(body) != `{"input":"hi","model":"gw"}` {
			t.Fatalf("buffered body = %s (%v), want the same two fields", body, err)
		}
	})
	t.Run("a body that is no JSON object is an error", func(t *testing.T) {
		for _, raw := range []string{`[1]`, `null`, `"x"`, `{"model":`, ``} {
			if body, err := speechBufferedBody([]byte(raw)); err == nil {
				t.Errorf("speechBufferedBody(%q) = %s, want an error", raw, body)
			}
		}
	})
}

// The up-front half of the decision: a route is known to stream only when its
// speech_streaming verdict is yes. Everything else is the buffered answer, and a store
// that cannot be read says so at Warn because it silently changes what the client got.
func TestMappingStreamsSpeech(t *testing.T) {
	up, _ := newSpeechUpstream(t, speechTestWAV())
	for _, tc := range []struct {
		name     string
		routeID  string
		failRead bool
		noRoutes bool
		want     bool
		wantWarn bool
	}{
		{name: "a yes", routeID: "route-tts", want: true},
		{name: "a no", routeID: "route-tts-offline"},
		{name: "no verdict at all", routeID: "route-tts-unprobed"},
		{name: "a mapping the store does not know", routeID: "route-nope"},
		{name: "no route id (a vendor account's route names no mapping)", routeID: ""},
		{name: "no route store", routeID: "route-tts", noRoutes: true},
		{name: "a verdict that cannot be read", routeID: "route-tts", failRead: true, wantWarn: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			buf, restore := withCapturedSlog(t)
			t.Cleanup(restore)
			srv := newSpeechStreamingTestServer(t, up.URL)
			if tc.failRead {
				srv.Routes = capabilityReadFailingStore{Store: srv.Routes}
			}
			if tc.noRoutes {
				srv.Routes = nil
			}

			if got := srv.mappingStreamsSpeech(context.Background(), tc.routeID); got != tc.want {
				t.Fatalf("mappingStreamsSpeech = %v, want %v", got, tc.want)
			}
			var warns int
			for _, rec := range buf.Snapshot() {
				if rec.Level == "WARN" && strings.Contains(rec.Msg, "speech streaming verdict") {
					warns++
				}
			}
			if (warns == 1) != tc.wantWarn || warns > 1 {
				t.Fatalf("speech-streaming Warn logs = %d, want %v", warns, tc.wantWarn)
			}
		})
	}
}

// Each streaming verdict answers for its own endpoint: a mapping that streams a
// transcription is not thereby known to stream speech, nor the reverse.
func TestMappingStreamsSpeechAndTranscriptionAreIndependent(t *testing.T) {
	up, _ := newSpeechUpstream(t, speechTestWAV())
	srv := newSpeechStreamingTestServer(t, up.URL)
	now := time.Now().UTC()
	if err := srv.Routes.UpsertMappingCapabilities(context.Background(), "route-tts-offline", []routing.CapabilityRow{{Capability: routing.CapabilityTranscriptionStreaming, Verdict: routing.CapabilityYes, Source: routing.CapabilitySourceAudioCppModels, CheckedAt: now}}); err != nil {
		t.Fatalf("UpsertMappingCapabilities: %v", err)
	}
	if srv.mappingStreamsSpeech(context.Background(), "route-tts-offline") {
		t.Fatal("a transcription_streaming yes made the mapping stream speech")
	}
	if !srv.mappingStreamsTranscription(context.Background(), "route-tts-offline") {
		t.Fatal("the transcription verdict was not read from its own row")
	}
	if srv.mappingStreamsTranscription(context.Background(), "route-tts") {
		t.Fatal("a speech_streaming yes made the mapping stream transcriptions")
	}
}
