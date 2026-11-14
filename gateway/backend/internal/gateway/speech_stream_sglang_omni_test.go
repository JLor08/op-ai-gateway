// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"op-ai-gateway/internal/logbuffer"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/usage"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The wire format sglang-omni (v0.2.0, sglang_omni/serve/openai_api.py) streams a
// synthesis in: one speech.audio.delta per chunk and, on success, a speech.audio.done that
// states token usage -- and then the body simply ends, with no [DONE]. A failure is an
// error event in the done event's place. Python's json.dumps writes ", " and ": ", so the
// fixtures do too. Both shapes carry the PCM format as X-Sample-Rate, X-Channels and
// X-Bit-Depth response headers.
const (
	omniSSEDone  = `data: {"type": "speech.audio.done", "usage": {"input_tokens": 4, "output_tokens": 30, "total_tokens": 34}, "finish_reason": "stop"}` + "\n\n"
	omniSSEError = `data: {"type": "error", "error": {"message": "generation failed", "type": "server_error", "param": null, "code": null}}` + "\n\n"

	// gatewaySpeechEnd is the frame the gateway ends an sglang-omni event stream with after
	// a whole frame (one the upstream ended with its blank line): the sentinel alone.
	gatewaySpeechEnd = "data: [DONE]\n\n"
)

// omniFrameDelta builds the speech.audio.delta frame sglang-omni streams for pcm.
func omniFrameDelta(pcm []byte) string {
	return `data: {"type": "speech.audio.delta", "audio": "` + base64.StdEncoding.EncodeToString(pcm) + "\"}\n\n"
}

// The gateway model of the sglang-omni fixture: an agent-managed child whose runtime spec
// is sglang_omni, speech-capable and known to stream speech. Its upstream name is its
// gateway name, so the relay's model rewrite leaves the client's body byte for byte as
// it was and a test can compare the two directly.
const (
	omniSpeechModel   = "omni-voice"
	omniSpeechMapping = "route-omni-tts"
)

// newSGLangOmniSpeechTestServer is newSpeechStreamingTestServer with one more candidate:
// omni-voice, a server_agent child at upstreamURL whose runtime spec is sglang_omni,
// with (speech, yes) and (speech_streaming, yes). tts-voice stays the audio.cpp model,
// at the same stub, so one test can hold the two upstream kinds side by side.
func newSGLangOmniSpeechTestServer(t *testing.T, upstreamURL string) *Server {
	t.Helper()
	srv := newSpeechStreamingTestServer(t, upstreamURL)
	ctx := context.Background()
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	u, err := url.Parse(upstreamURL)
	if err != nil {
		t.Fatalf("url.Parse(%q): %v", upstreamURL, err)
	}
	host, portText, _ := net.SplitHostPort(u.Host)
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatalf("upstream port %q: %v", portText, err)
	}
	if err := srv.Routes.CreateAIServer(ctx, routing.AIServer{ID: "srv-omni", Name: "Omni Agent", Domain: host, Provider: routing.ProviderVLLM, Endpoint: upstreamURL, Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateAIServer: %v", err)
	}
	if err := srv.Routes.CreateApplication(ctx, routing.Application{ID: "app-omni", ServerID: "srv-omni", Type: routing.ProviderServerAgent, Port: port, Scheme: u.Scheme, APIFlavors: []string{routing.APIFlavorOpenAIAudio}, Priority: 10, Weight: 50, TimeoutMS: 30000, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateApplication: %v", err)
	}
	if err := srv.Routes.CreateMapping(ctx, routing.ModelMapping{ID: omniSpeechMapping, ApplicationID: "app-omni", GatewayModelName: omniSpeechModel, AppModelName: omniSpeechModel, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateMapping: %v", err)
	}
	rows := []routing.CapabilityRow{
		{Capability: routing.CapabilitySpeech, Verdict: routing.CapabilityYes, Source: routing.CapabilitySourceManual, CheckedAt: now},
		{Capability: routing.CapabilitySpeechStreaming, Verdict: routing.CapabilityYes, Source: routing.CapabilitySourceManual, CheckedAt: now},
	}
	if err := srv.Routes.UpsertMappingCapabilities(ctx, omniSpeechMapping, rows); err != nil {
		t.Fatalf("UpsertMappingCapabilities: %v", err)
	}
	if err := srv.Routes.UpsertRuntimeSpec(ctx, routing.RuntimeSpec{ID: "spec-omni-tts", MappingID: omniSpeechMapping, Type: string(routing.RuntimeSpecTypeSGLangOmni), APIFlavors: []string{routing.APIFlavorOpenAIAudio}, ResponsesMode: routing.EndpointModeDisabled, MessagesMode: routing.EndpointModeDisabled, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("UpsertRuntimeSpec: %v", err)
	}
	if err := srv.Routes.UpsertTelemetry(ctx, routing.ServerTelemetry{ServerID: "srv-omni", ReportedAt: now, LatencyMS: 100, ProviderHealth: `{}`, Capabilities: `{}`, RawSummary: `{}`, UpdatedAt: now}); err != nil {
		t.Fatalf("UpsertTelemetry: %v", err)
	}
	return srv
}

// setOmniFormatHeaders states the PCM format the way sglang-omni does on both stream
// shapes: the sample rate it synthesized at, one channel, 16 bits.
func setOmniFormatHeaders(h http.Header, sampleRate string) {
	h.Set("X-Sample-Rate", sampleRate)
	h.Set("X-Channels", "1")
	h.Set("X-Bit-Depth", "16")
}

// newOmniSSEUpstream starts an sglang-omni stub that answers every call with an event
// stream made of frames, written and flushed one at a time, under the given format
// headers (none for an empty sampleRate), and then ends the body cleanly.
func newOmniSSEUpstream(t *testing.T, sampleRate string, frames ...string) (*httptest.Server, <-chan speechUpstreamCall) {
	t.Helper()
	return newScriptedSpeechUpstream(t, func(_ int, w http.ResponseWriter, _ speechUpstreamCall) {
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		if sampleRate != "" {
			setOmniFormatHeaders(w.Header(), sampleRate)
		}
		w.WriteHeader(http.StatusOK)
		for _, frame := range frames {
			_, _ = io.WriteString(w, frame)
			w.(http.Flusher).Flush()
		}
	})
}

const omniStreamBody = `{"model":"omni-voice","input":"Hello there.","stream":true,"stream_format":"sse"}`

// sglang-omni ends a completed event stream at its done event, with no [DONE]. The gateway
// writes the sentinel itself once the upstream has closed the body cleanly after that
// event, so the client sees the one contract every relayed speech event stream keeps, and
// the stream completes by the relay's unchanged rule -- done, then [DONE] -- and is billed:
// in audio_second from the cached rate when the mapping has one, else from the rate the
// response's format headers state, and in audio_bytes when there is neither. The cached
// rate is the benchmark's measure of this mapping and wins over the headers.
func TestSpeechStreamSGLangOmniGatewayEndsTheEventStream(t *testing.T) {
	deltas := [][]byte{speechPCM(100000, 1), speechPCM(76400, 2)} // 176400 bytes together
	frames := []string{omniFrameDelta(deltas[0]), omniFrameDelta(deltas[1]), omniSSEDone}
	for _, tc := range []struct {
		name       string
		sampleRate string // the format headers' sample rate; "" sends none
		cachedRate string // the mapping's cached output byte rate; "" caches none
		unit       string
		quantity   float64
	}{
		{"the cached rate, over the header rate", "24000", "88200", usage.BillingUnitAudioSecond, 2},
		{"no cached rate: the header rate", "24000", "", usage.BillingUnitAudioSecond, 176400.0 / 48000},
		{"neither: the bytes", "", "", usage.BillingUnitAudioBytes, 176400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			buf, restore := withCapturedSlog(t)
			t.Cleanup(restore)
			up, _ := newOmniSSEUpstream(t, tc.sampleRate, frames...)
			srv := newSGLangOmniSpeechTestServer(t, up.URL)
			if tc.cachedRate != "" {
				seedSpeechOutputByteRate(t, srv, omniSpeechMapping, tc.cachedRate)
			}

			rec := postSpeech(t, srv, omniStreamBody)

			want := strings.Join(frames, "") + gatewaySpeechEnd
			if rec.Code != http.StatusOK || rec.Body.String() != want {
				t.Fatalf("response = %d %q, want the upstream's frames and then the gateway's [DONE]", rec.Code, rec.Body.String())
			}
			ev := onlyUsageEvent(t, srv)
			if ev.Status != "success" || ev.ErrorCode != "" || !ev.Stream {
				t.Fatalf("usage row = %q/%q stream=%v, want a streamed success", ev.Status, ev.ErrorCode, ev.Stream)
			}
			requireStreamedSpeechRow(t, ev, tc.unit, tc.quantity)
			if errs := errorLogs(buf); len(errs) != 0 {
				t.Fatalf("a completed sglang-omni stream logged at Error: %+v", errs)
			}
		})
	}
}

// The gateway's [DONE] stands for one thing only: an sglang-omni stream that completed.
// A stream that ended with an error event in the done event's place, one with no done
// event at all, one cut before it, one whose [DONE] came ahead of the done event, one
// whose audio could not be counted, and one whose done event the agent's runtime router
// followed with its error frame (an untyped {"error":{...}} event, written when the child
// fails mid-body) all reach the client exactly as the upstream sent them, and bill zero --
// loudly where the row would otherwise look like a success.
func TestSpeechStreamSGLangOmniWithoutACompletionGetsNoSentinel(t *testing.T) {
	delta := omniFrameDelta(speechPCM(1000, 1))
	badDelta := `data: {"type": "speech.audio.delta", "audio": "***not base64***"}` + "\n\n"
	routerError := `data: {"error":{"code":"runtime.instance_failed","message":"the model process exited"}}` + "\n\n"
	for _, tc := range []struct {
		name       string
		body       string
		abort      bool   // tear the connection down after body
		wantStatus string // a body that ends cleanly is a success that billed nothing
	}{
		{name: "an error event instead of the done event", body: delta + omniSSEError, wantStatus: "success"},
		{name: "deltas and then nothing", body: delta, wantStatus: "success"},
		{name: "[DONE] ahead of the done event", body: delta + speechSSEEnd + omniSSEDone, wantStatus: "success"},
		{name: "a delta that cannot be counted", body: delta + badDelta + omniSSEDone, wantStatus: "success"},
		{name: "the done event and then the router's error frame", body: delta + omniSSEDone + routerError, wantStatus: "success"},
		{name: "cut before the done event", body: delta, abort: true, wantStatus: "error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			buf, restore := withCapturedSlog(t)
			t.Cleanup(restore)
			up, _ := newScriptedSpeechUpstream(t, func(_ int, w http.ResponseWriter, _ speechUpstreamCall) {
				w.Header().Set("Content-Type", "text/event-stream")
				setOmniFormatHeaders(w.Header(), "24000")
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, tc.body)
				w.(http.Flusher).Flush()
				if tc.abort {
					panic(http.ErrAbortHandler)
				}
			})
			srv := newSGLangOmniSpeechTestServer(t, up.URL)
			seedSpeechOutputByteRate(t, srv, omniSpeechMapping, "48000")

			rec := postSpeech(t, srv, omniStreamBody)

			if rec.Code != http.StatusOK || rec.Body.String() != tc.body {
				t.Fatalf("response = %d %q, want exactly the frames the upstream sent, %q", rec.Code, rec.Body.String(), tc.body)
			}
			ev := onlyUsageEvent(t, srv)
			if ev.Status != tc.wantStatus {
				t.Fatalf("usage row status = %q (%s), want %q", ev.Status, ev.ErrorCode, tc.wantStatus)
			}
			requireStreamedSpeechRow(t, ev, usage.BillingUnitAudioSecond, 0)
			if tc.wantStatus == "success" {
				errs := errorLogs(buf)
				if len(errs) != 1 || !strings.Contains(errs[0].Msg, "streamed speech") {
					t.Fatalf("Error logs = %+v, want exactly one saying the streamed speech call billed zero", errs)
				}
			}
		})
	}
}

// An sglang-omni stream that ends with its own [DONE] after the done event -- a later
// version, or an intermediary that adds one -- already completed: the gateway adds no
// second sentinel, and the stream is billed as any completed one.
func TestSpeechStreamSGLangOmniThatSentDONEGetsNoSecond(t *testing.T) {
	pcm := speechPCM(96000, 1)
	sent := omniFrameDelta(pcm) + omniSSEDone + speechSSEEnd
	up, _ := newOmniSSEUpstream(t, "24000", sent)
	srv := newSGLangOmniSpeechTestServer(t, up.URL)

	rec := postSpeech(t, srv, omniStreamBody)

	if rec.Code != http.StatusOK || rec.Body.String() != sent {
		t.Fatalf("response = %d %q, want the upstream's stream verbatim, with its one [DONE]", rec.Code, rec.Body.String())
	}
	requireStreamedSpeechRow(t, onlyUsageEvent(t, srv), usage.BillingUnitAudioSecond, 2)
}

// The gateway's sentinel is always an event of its own: after a frame the upstream left
// unterminated it starts with a blank line's worth of newlines (\n\n), so it never fuses
// into the data of the done frame; after a frame ended by one newline, one more newline is
// enough; after a whole frame it needs none. The unterminated done frame is still read as
// the done event, so the stream completes.
func TestSpeechStreamSGLangOmniSentinelAfterAnUnterminatedFrame(t *testing.T) {
	done := strings.TrimRight(omniSSEDone, "\n")
	for _, tc := range []struct {
		name, ending, sentinel string
	}{
		{"no newline", "", "\n\n" + "data: [DONE]\n\n"},
		{"one newline", "\n", "\n" + gatewaySpeechEnd},
		{"a whole frame", "\n\n", gatewaySpeechEnd},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sent := omniFrameDelta(speechPCM(48000, 1)) + done + tc.ending
			up, _ := newOmniSSEUpstream(t, "24000", sent)
			srv := newSGLangOmniSpeechTestServer(t, up.URL)

			rec := postSpeech(t, srv, omniStreamBody)

			if rec.Code != http.StatusOK || rec.Body.String() != sent+tc.sentinel {
				t.Fatalf("response = %d %q, want the upstream's bytes and then %q", rec.Code, rec.Body.String(), tc.sentinel)
			}
			requireStreamedSpeechRow(t, onlyUsageEvent(t, srv), usage.BillingUnitAudioSecond, 1)
		})
	}
}

// sglang-omni's raw shape (its default stream_format, audio/pcm) has no terminal frame and
// gets none: the body is relayed as the upstream sent it. It states the same format
// headers, so without a cached rate it is billed in seconds from them.
func TestSpeechStreamSGLangOmniRawPCMBillsTheHeaderRate(t *testing.T) {
	buf, restore := withCapturedSlog(t)
	t.Cleanup(restore)
	chunks := [][]byte{speechPCM(50000, 1), speechPCM(46000, 2)} // 96000 bytes: two seconds at 24 kHz
	up, _ := newScriptedSpeechUpstream(t, func(_ int, w http.ResponseWriter, _ speechUpstreamCall) {
		w.Header().Set("Content-Type", "audio/pcm")
		setOmniFormatHeaders(w.Header(), "24000")
		w.WriteHeader(http.StatusOK)
		for _, chunk := range chunks {
			_, _ = w.Write(chunk)
			w.(http.Flusher).Flush()
		}
	})
	srv := newSGLangOmniSpeechTestServer(t, up.URL)

	rec := postSpeech(t, srv, `{"model":"omni-voice","input":"Hello there.","stream":true}`)

	if want := bytes.Join(chunks, nil); rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), want) {
		t.Fatalf("response = %d (%d bytes), want the upstream's %d raw bytes verbatim", rec.Code, rec.Body.Len(), len(want))
	}
	ev := onlyUsageEvent(t, srv)
	if ev.Status != "success" || !ev.Stream {
		t.Fatalf("usage row = %q stream=%v, want a streamed success", ev.Status, ev.Stream)
	}
	requireStreamedSpeechRow(t, ev, usage.BillingUnitAudioSecond, 2)
	if errs := errorLogs(buf); len(errs) != 0 {
		t.Fatalf("a clean sglang-omni raw stream logged at Error: %+v", errs)
	}
}

// Format headers sglang-omni stated but the gateway cannot use -- a value that is no plain
// decimal positive integer, one stated twice -- give no rate, so a completed stream on a
// mapping without a cached rate bills its bytes, as one with no headers at all does. That
// fallback is said once at Warn, naming the header, so an upstream whose headers stopped
// parsing is visible; a stream that stated no format headers, and one whose mapping has a
// cached rate (the headers are then never consulted), log nothing.
func TestSpeechStreamSGLangOmniWarnsOfAnUnusableHeaderRate(t *testing.T) {
	pcm := speechPCM(48000, 1)
	for _, tc := range []struct {
		name       string
		header     func(http.Header)
		cachedRate string
		unit       string
		quantity   float64
		warnHeader string // the header the one warning names; "" is no warning
	}{
		{"a sample rate with a unit", func(h http.Header) { setOmniFormatHeaders(h, "24kHz") }, "", usage.BillingUnitAudioBytes, 48000, "X-Sample-Rate"},
		{"channels stated twice", func(h http.Header) { setOmniFormatHeaders(h, "24000"); h.Add("X-Channels", "1") }, "", usage.BillingUnitAudioBytes, 48000, "X-Channels"},
		{"a cached rate: the headers are not consulted", func(h http.Header) { setOmniFormatHeaders(h, "24kHz") }, "48000", usage.BillingUnitAudioSecond, 1, ""},
		{"no format headers", func(http.Header) {}, "", usage.BillingUnitAudioBytes, 48000, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			buf, restore := withCapturedSlog(t)
			t.Cleanup(restore)
			up, _ := newScriptedSpeechUpstream(t, func(_ int, w http.ResponseWriter, _ speechUpstreamCall) {
				w.Header().Set("Content-Type", "text/event-stream")
				tc.header(w.Header())
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, omniFrameDelta(pcm)+omniSSEDone)
			})
			srv := newSGLangOmniSpeechTestServer(t, up.URL)
			if tc.cachedRate != "" {
				seedSpeechOutputByteRate(t, srv, omniSpeechMapping, tc.cachedRate)
			}

			if rec := postSpeech(t, srv, omniStreamBody); rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rec.Code)
			}

			requireStreamedSpeechRow(t, onlyUsageEvent(t, srv), tc.unit, tc.quantity)
			var warns []logbuffer.Record
			for _, rec := range buf.Snapshot() {
				if rec.Level == "WARN" && strings.Contains(rec.Msg, "format headers") {
					warns = append(warns, rec)
				}
			}
			if tc.warnHeader == "" {
				if len(warns) != 0 {
					t.Fatalf("warnings = %+v, want none", warns)
				}
				return
			}
			if len(warns) != 1 || warns[0].Attrs["header"] != tc.warnHeader || warns[0].Attrs["id"] == "" || warns[0].Attrs["id"] == nil {
				t.Fatalf("warnings = %+v, want one naming %s and the request id", warns, tc.warnHeader)
			}
			if errs := errorLogs(buf); len(errs) != 0 {
				t.Fatalf("a completed stream logged at Error: %+v", errs)
			}
		})
	}
}

// sentinelRefusingWriter is a client whose write of the gateway's [DONE] fails. With a
// cancel it is a client that goes away exactly then: the request's context is cancelled
// too, as a closed connection does to a real one. Without one the request stays live, as
// it does when the write fails for a reason of its own (a write deadline, say).
type sentinelRefusingWriter struct {
	*httptest.ResponseRecorder
	cancel context.CancelFunc
}

func (w *sentinelRefusingWriter) Write(p []byte) (int, error) {
	if bytes.Contains(p, []byte("[DONE]")) {
		if w.cancel != nil {
			w.cancel()
		}
		return 0, errors.New("write: broken pipe")
	}
	return w.ResponseRecorder.Write(p)
}

// A client that is gone by the time the gateway writes its sentinel did not receive the
// stream's end: the row is the disconnect's error row, billed zero, and -- like every
// client that went away -- it is not logged at Error.
func TestSpeechStreamSGLangOmniClientGoneWhileTheSentinelIsWritten(t *testing.T) {
	buf, restore := withCapturedSlog(t)
	t.Cleanup(restore)
	up, _ := newOmniSSEUpstream(t, "24000", omniFrameDelta(speechPCM(48000, 1)), omniSSEDone)
	srv := newSGLangOmniSpeechTestServer(t, up.URL)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1/audio/speech", strings.NewReader(omniStreamBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer dev-secret")
	w := &sentinelRefusingWriter{ResponseRecorder: httptest.NewRecorder(), cancel: cancel}

	srv.ServeHTTP(w, req)

	if strings.Contains(w.Body.String(), "[DONE]") || !strings.HasSuffix(w.Body.String(), omniSSEDone) {
		t.Fatalf("client body = %q, want the upstream's frames and no sentinel", w.Body.String())
	}
	ev := onlyUsageEvent(t, srv)
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

// A sentinel write that fails while the request is still live is the copy's own error:
// the client did not receive the stream's end, so the row is provider.stream_copy_error,
// billed zero, and that error row's own log line is the one said at Error -- the billing
// adds no second one about a stream that ended without its terminal events.
func TestSpeechStreamSGLangOmniSentinelWriteFailsOnALiveRequest(t *testing.T) {
	buf, restore := withCapturedSlog(t)
	t.Cleanup(restore)
	up, _ := newOmniSSEUpstream(t, "24000", omniFrameDelta(speechPCM(48000, 1)), omniSSEDone)
	srv := newSGLangOmniSpeechTestServer(t, up.URL)
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/audio/speech", strings.NewReader(omniStreamBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer dev-secret")
	w := &sentinelRefusingWriter{ResponseRecorder: httptest.NewRecorder()}

	srv.ServeHTTP(w, req)

	if strings.Contains(w.Body.String(), "[DONE]") || !strings.HasSuffix(w.Body.String(), omniSSEDone) {
		t.Fatalf("client body = %q, want the upstream's frames and no sentinel", w.Body.String())
	}
	ev := onlyUsageEvent(t, srv)
	if ev.Status != "error" || ev.ErrorCode != "provider.stream_copy_error" {
		t.Fatalf("usage row = %q/%q, want error/provider.stream_copy_error", ev.Status, ev.ErrorCode)
	}
	requireStreamedSpeechRow(t, ev, usage.BillingUnitAudioSecond, 0)
	for _, rec := range errorLogs(buf) {
		if strings.Contains(rec.Msg, "streamed speech") {
			t.Fatalf("the billing logged the copy error's row a second time: %+v", rec)
		}
	}
}

// audio.cpp keeps its own contract, byte for byte: its event stream ends with [DONE], so
// one that stops after the done event did not complete -- the gateway writes nothing for
// it, and it bills zero -- and the format headers, should it ever send them, are not a
// rate: a completed stream without a cached rate still bills its bytes, on either shape.
func TestSpeechStreamAudioCppGetsNoSentinelAndNoHeaderRate(t *testing.T) {
	pcm := speechPCM(96000, 1)
	for _, tc := range []struct {
		name        string
		contentType string
		sent        []byte
		body        string
		unit        string
		quantity    float64
		wantErrLog  bool
	}{
		{"the done event and a clean close", "text/event-stream", []byte(speechFrameDelta(pcm) + speechSSEDone), `{"model":"tts-voice","input":"hi","stream":true}`, usage.BillingUnitAudioSecond, 0, true},
		{"a completed event stream", "text/event-stream", []byte(speechFrameDelta(pcm) + speechSSEDone + speechSSEEnd), `{"model":"tts-voice","input":"hi","stream":true}`, usage.BillingUnitAudioBytes, 96000, false},
		{"raw pcm", "application/octet-stream", pcm, `{"model":"tts-voice","input":"hi","stream":true,"stream_format":"audio"}`, usage.BillingUnitAudioBytes, 96000, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			buf, restore := withCapturedSlog(t)
			t.Cleanup(restore)
			up, _ := newScriptedSpeechUpstream(t, func(_ int, w http.ResponseWriter, _ speechUpstreamCall) {
				w.Header().Set("Content-Type", tc.contentType)
				setOmniFormatHeaders(w.Header(), "24000")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write(tc.sent)
			})
			srv := newSGLangOmniSpeechTestServer(t, up.URL)

			rec := postSpeech(t, srv, tc.body)

			if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), tc.sent) {
				t.Fatalf("response = %d %q, want the upstream's bytes verbatim", rec.Code, rec.Body.String())
			}
			requireStreamedSpeechRow(t, onlyUsageEvent(t, srv), tc.unit, tc.quantity)
			if errs := errorLogs(buf); (len(errs) == 1) != tc.wantErrLog || len(errs) > 1 {
				t.Fatalf("Error logs = %+v, want one: %v", errs, tc.wantErrLog)
			}
		})
	}
}

// sglang-omni refuses a stream whose response_format is not pcm, and its own default is
// wav, so a streamed request that leaves the field out -- the portal's, the documented
// default -- would be refused; and its stream_format defaults to raw "audio", where the
// gateway documents the event stream as a stream's default shape. So the relay states pcm
// on the upstream body when the client stated no response_format (absent, null or empty),
// and sse when it stated no stream_format, in one rewrite that touches nothing else; a
// client that said pcm and either stream shape is relayed byte for byte, and so is every
// stream to audio.cpp, whose defaults those are. sglang-omni reads its fields by their
// exact names, so a value under a key spelled in another case is no statement to it: the
// exact key is added, and the client's own key stays.
func TestSpeechStreamToSGLangOmniStatesPCMAndSSE(t *testing.T) {
	for _, tc := range []struct {
		name  string
		body  string
		added map[string]any // the exact keys the relay adds or sets; nil: the client's body, byte for byte
	}{
		{name: "neither field", body: `{"model":"omni-voice","input":"Hello there.","stream":true,"voice":"alba","speed":1.25}`, added: map[string]any{"response_format": "pcm", "stream_format": "sse"}},
		{name: "a null response_format", body: `{"model":"omni-voice","input":"Hello there.","stream":true,"response_format":null,"stream_format":"sse"}`, added: map[string]any{"response_format": "pcm"}},
		{name: "an empty response_format", body: `{"model":"omni-voice","input":"Hello there.","stream":true,"response_format":"","stream_format":"sse"}`, added: map[string]any{"response_format": "pcm"}},
		{name: "the raw shape, no response_format", body: `{"model":"omni-voice","input":"Hello there.","stream":true,"stream_format":"audio"}`, added: map[string]any{"response_format": "pcm"}},
		{name: "pcm under a key of another case", body: `{"model":"omni-voice","input":"Hello there.","stream":true,"Response_Format":"pcm","stream_format":"sse"}`, added: map[string]any{"response_format": "pcm"}},
		{name: "pcm, no stream_format", body: `{"model":"omni-voice","input":"Hello there.","stream":true,"response_format":"pcm"}`, added: map[string]any{"stream_format": "sse"}},
		{name: "pcm, a null stream_format", body: `{"model":"omni-voice","input":"Hello there.","stream":true,"response_format":"pcm","stream_format":null}`, added: map[string]any{"stream_format": "sse"}},
		{name: "pcm, the raw shape under a key of another case", body: `{"model":"omni-voice","input":"Hello there.","stream":true,"response_format":"pcm","Stream_Format":"audio"}`, added: map[string]any{"stream_format": "sse"}},
		{name: "pcm and sse already", body: `{"model":"omni-voice","input":"Hello there.", "stream":true,"response_format":"pcm","stream_format":"sse"}`},
		{name: "pcm and the raw shape already", body: `{"model":"omni-voice","input":"Hello there.", "stream":true,"stream_format":"audio","response_format":"pcm"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up, calls := newOmniSSEUpstream(t, "24000", omniFrameDelta(speechPCM(100, 1)), omniSSEDone)
			srv := newSGLangOmniSpeechTestServer(t, up.URL)

			if rec := postSpeech(t, srv, tc.body); rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
			}

			all := drainSpeechUpstreamCalls(calls)
			if len(all) != 1 {
				t.Fatalf("upstream calls = %d, want 1", len(all))
			}
			if tc.added == nil {
				if string(all[0].body) != tc.body {
					t.Fatalf("upstream body = %s, want the client's byte for byte", all[0].body)
				}
				return
			}
			sent := speechUpstreamFields(t, all[0])
			want := speechUpstreamFields(t, speechUpstreamCall{body: []byte(tc.body)})
			for k, v := range tc.added {
				want[k] = v
			}
			if len(sent) != len(want) {
				t.Fatalf("upstream body = %v, want the client's fields with %v: %v", sent, tc.added, want)
			}
			for k, v := range want {
				if sent[k] != v {
					t.Fatalf("upstream %s = %#v, want %#v", k, sent[k], v)
				}
			}
		})
	}
	t.Run("audio.cpp: the client's body", func(t *testing.T) {
		up, calls := newSSESpeechUpstream(t, speechFrameDelta(speechPCM(100, 1)), speechSSEDone, speechSSEEnd)
		srv := newSGLangOmniSpeechTestServer(t, up.URL)
		raw := `{"model":"tts-voice","input":"Hello there.","stream":true}`

		if rec := postSpeech(t, srv, raw); rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}

		all := drainSpeechUpstreamCalls(calls)
		if len(all) != 1 || !bytes.Equal(all[0].body, rewriteModelField([]byte(raw), speechUpstreamModel)) {
			t.Fatalf("upstream calls = %+v, want one carrying the client's body with only the model mapped", all)
		}
	})
	t.Run("a one-shot request: the client's body", func(t *testing.T) {
		up, calls := newSpeechUpstream(t, speechTestWAV())
		srv := newSGLangOmniSpeechTestServer(t, up.URL)
		raw := `{"model":"omni-voice","input":"Hello there."}`

		if rec := postSpeech(t, srv, raw); rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}

		all := drainSpeechUpstreamCalls(calls)
		if len(all) != 1 || string(all[0].body) != raw {
			t.Fatalf("upstream calls = %+v, want one carrying the client's body byte for byte", all)
		}
	})
}

// The rewrite on its own, for the values validateSpeechRequest refuses before a relay
// could show them: an empty stream_format is no statement either, a value of another type
// is none, and a body that needs nothing comes back as the very bytes it was. A body that
// is no JSON object is the one error.
func TestSGLangOmniSpeechStreamBody(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       map[string]any // nil: unchanged
	}{
		{"both empty", `{"input":"x","response_format":"","stream_format":""}`, map[string]any{"input": "x", "response_format": "pcm", "stream_format": "sse"}},
		{"values of another type", `{"input":"x","response_format":7,"stream_format":["sse"]}`, map[string]any{"input": "x", "response_format": "pcm", "stream_format": "sse"}},
		{"a stream_format in another case of its value", `{"input":"x","response_format":"pcm","stream_format":"SSE"}`, map[string]any{"input": "x", "response_format": "pcm", "stream_format": "sse"}},
		{"the last of a repeated key counts", `{"input":"x","response_format":"pcm","stream_format":"","stream_format":"audio"}`, nil},
		{"both stated", `{ "input" : "x", "response_format":"pcm","stream_format":"sse" }`, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, changed, err := sglangOmniSpeechStreamBody([]byte(tc.body))
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			if tc.want == nil {
				if changed || string(got) != tc.body {
					t.Fatalf("body = %s (changed %v), want the client's byte for byte", got, changed)
				}
				return
			}
			fields := speechUpstreamFields(t, speechUpstreamCall{body: got})
			if !changed || len(fields) != len(tc.want) {
				t.Fatalf("body = %s (changed %v), want %v", got, changed, tc.want)
			}
			for k, v := range tc.want {
				if fields[k] != v {
					t.Fatalf("%s = %#v, want %#v", k, fields[k], v)
				}
			}
		})
	}
	for _, raw := range []string{`null`, `[]`, `"x"`, `{`} {
		if _, _, err := sglangOmniSpeechStreamBody([]byte(raw)); err == nil {
			t.Errorf("sglangOmniSpeechStreamBody(%s) err = nil, want an error", raw)
		}
	}
}

// The buffered retry of an sglang-omni stream is built from the client's body, as every
// buffered retry is, so it leaves response_format and stream_format out like the other
// stream-only fields: the pcm and sse the stream stated are the stream's alone.
func TestSpeechStreamToSGLangOmniBufferedRetryDropsPCMAndSSE(t *testing.T) {
	wav := speechTestWAV()
	up, calls := newScriptedSpeechUpstream(t, func(n int, w http.ResponseWriter, _ speechUpstreamCall) {
		if n == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, speechModeMismatchBody)
			return
		}
		w.Header().Set("Content-Type", "audio/wav")
		w.Header().Set("X-AudioCPP-Audio-Duration-Ms", "1500")
		_, _ = w.Write(wav)
	})
	srv := newSGLangOmniSpeechTestServer(t, up.URL)

	rec := postSpeech(t, srv, `{"model":"omni-voice","input":"Hello there.","stream":true}`)

	if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), wav) {
		t.Fatalf("response = %d %q, want the retry's WAV", rec.Code, rec.Body.Bytes())
	}
	all := drainSpeechUpstreamCalls(calls)
	if len(all) != 2 {
		t.Fatalf("upstream calls = %d, want the stream and its buffered retry", len(all))
	}
	if first := speechUpstreamFields(t, all[0]); first["response_format"] != "pcm" || first["stream_format"] != "sse" || first["stream"] != true {
		t.Fatalf("first upstream body = %v, want the stream stating pcm and sse", first)
	}
	retry := speechUpstreamFields(t, all[1])
	for _, field := range speechStreamOnlyFields {
		if _, ok := retry[field]; ok {
			t.Fatalf("retry body = %v, want no %s", retry, field)
		}
	}
}
