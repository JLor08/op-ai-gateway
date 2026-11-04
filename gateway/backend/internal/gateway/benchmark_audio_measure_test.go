// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"mime"
	"mime/multipart"
	"net/http"
	"op-ai-gateway/internal/benchaudio"
	"op-ai-gateway/internal/provider"
	"op-ai-gateway/internal/routing"
	"reflect"
	"strings"
	"testing"
	"time"
)

// measureEpoch is the moment a measurement under test sends its request: the scripted clock
// starts there, and every chunk of a scripted answer arrives some time after it.
var measureEpoch = time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)

// measureClock is the measurements' clock in these tests. It stands at the epoch until a
// scripted answer moves it, so every time a measurement reads is one the test scripted and
// the math comes out exact.
type measureClock struct{ at time.Duration }

func (c *measureClock) now() time.Time { return measureEpoch.Add(c.at) }

// measurer is the measurer under test, reading this clock.
func (c *measureClock) measurer() audioMeasurer { return audioMeasurer{now: c.now} }

// measureChunk is what one read of a scripted answer body delivers, and when it arrives.
type measureChunk struct {
	at   time.Duration
	data string
}

// measureBody is a scripted answer body. A read delivers the next chunk (as much of it as
// fits) and first moves the clock to that chunk's moment, so the time a measurement reads
// after the read is the time those bytes arrived; a chunk that does not fit is delivered
// over several reads, all at its moment. Once the chunks are spent a read answers end
// (io.EOF when it is nil), and spentReads counts those reads.
//
// A body with no clock runs on the real one instead: it waits gap before it delivers the
// first bytes of each chunk, so a chunk arrives at least gap after the one before it.
type measureBody struct {
	clock  *measureClock
	chunks []measureChunk
	end    error
	gap    time.Duration

	midChunk   bool
	spentReads int
}

func (b *measureBody) Read(p []byte) (int, error) {
	if len(b.chunks) == 0 {
		b.spentReads++
		if b.end != nil {
			return 0, b.end
		}
		return 0, io.EOF
	}
	c := &b.chunks[0]
	switch {
	case b.clock != nil:
		b.clock.at = c.at
	case !b.midChunk:
		time.Sleep(b.gap)
	}
	n := copy(p, c.data)
	c.data = c.data[n:]
	b.midChunk = c.data != ""
	if !b.midChunk {
		b.chunks = b.chunks[1:]
	}
	return n, nil
}

func (*measureBody) Close() error { return nil }

// measureUpstream is the fake upstream of one measurement: it answers with a scripted
// response (or fails with err) and records what was sent, and where. Its answer -- the
// status and headers, before any of the body -- arrives answerAt after the request: on its
// clock when it has one, else on the real one, by waiting.
type measureUpstream struct {
	status   int
	header   http.Header
	body     *measureBody
	err      error
	clock    *measureClock
	answerAt time.Duration

	calls       int
	target      routing.Target
	path        string
	sent        []byte
	contentType string
}

func (u *measureUpstream) answer(target routing.Target, path string, body []byte, contentType string) (*provider.ProxyResponse, error) {
	u.calls++
	u.target, u.path, u.sent, u.contentType = target, path, body, contentType
	if u.err != nil {
		return nil, u.err
	}
	if u.clock != nil {
		u.clock.at = u.answerAt
	} else {
		time.Sleep(u.answerAt)
	}
	return &provider.ProxyResponse{StatusCode: u.status, Header: u.header, Body: u.body}, nil
}

func (u *measureUpstream) ProxyNative(_ context.Context, target routing.Target, path string, body []byte, contentType string) (*provider.ProxyResponse, error) {
	return u.answer(target, path, body, contentType)
}

// ProxyNativeDuplex reads the streamed body whole before it answers: the live measurement
// sends its audio unpaced, so all of it is there to be read.
func (u *measureUpstream) ProxyNativeDuplex(_ context.Context, target routing.Target, path string, body io.Reader, contentType string) (*provider.ProxyResponse, error) {
	sent, err := io.ReadAll(body)
	if err != nil {
		return nil, err
	}
	return u.answer(target, path, sent, contentType)
}

// scriptedAudioUpstream answers 200 with the given Content-Type and the scripted chunks; the
// answer itself arrives the moment the request is sent unless the test sets answerAt.
func scriptedAudioUpstream(clock *measureClock, contentType string, chunks ...measureChunk) *measureUpstream {
	return &measureUpstream{
		status: http.StatusOK,
		header: http.Header{"Content-Type": {contentType}},
		body:   &measureBody{clock: clock, chunks: chunks},
		clock:  clock,
	}
}

// refusingAudioUpstream answers a non-2xx status with an audio.cpp-shaped error body.
func refusingAudioUpstream(clock *measureClock, status int, message string) *measureUpstream {
	up := scriptedAudioUpstream(clock, "application/json", measureAt(10, fmt.Sprintf(`{"error":{"message":%q}}`, message)))
	up.status = status
	return up
}

// refusingAgentUpstream answers a non-2xx status with the agent runtime router's error
// envelope, which names a stable code.
func refusingAgentUpstream(clock *measureClock, status int, code, message string) *measureUpstream {
	up := scriptedAudioUpstream(clock, "application/json", measureAt(10, fmt.Sprintf(`{"error":{"code":%q,"message":%q}}`, code, message)))
	up.status = status
	return up
}

// breakingAudioUpstream is scriptedAudioUpstream whose connection breaks after the chunks.
func breakingAudioUpstream(clock *measureClock, contentType string, chunks ...measureChunk) *measureUpstream {
	up := scriptedAudioUpstream(clock, contentType, chunks...)
	up.body.end = errors.New("connection reset by peer")
	return up
}

// measureAt is a chunk arriving the given number of milliseconds after the request was sent.
func measureAt(ms int, data string) measureChunk {
	return measureChunk{time.Duration(ms) * time.Millisecond, data}
}

// sseSpeechDelta is one speech.audio.delta frame carrying n bytes of PCM.
func sseSpeechDelta(n int) string {
	return fmt.Sprintf("data: {\"type\":%q,\"audio\":%q}\n\n", speechEventDelta, base64.StdEncoding.EncodeToString(make([]byte, n)))
}

// sseSpeechDone is the end of a synthesis stream: speech.audio.done, then the sentinel.
const sseSpeechDone = "data: {\"type\":\"speech.audio.done\",\"ttft_ms\":12}\n\ndata: [DONE]\n\n"

// sseTranscriptDelta is one transcript.text.delta frame.
func sseTranscriptDelta(text string) string {
	return fmt.Sprintf("data: {\"type\":%q,\"delta\":%q}\n\n", transcriptionEventDelta, text)
}

// sseTranscriptDone is the end of a live transcription: transcript.text.done with the whole
// text, then the sentinel.
func sseTranscriptDone(text string) string {
	return fmt.Sprintf("data: {\"type\":%q,\"text\":%q}\n\ndata: [DONE]\n\n", transcriptionEventDone, text)
}

// audioMeasureTarget names one instance of a replicable model (instance 2, so the address
// carries a '#'): every request must go to exactly this target, by exactly this name.
func audioMeasureTarget() routing.Target {
	return routing.Target{
		Provider:      routing.ProviderServerAgent,
		Endpoint:      "https://agent.test:8600",
		Model:         "voice",
		ProviderModel: "voice-model#2",
		RouteID:       "map_voice",
	}
}

// audioMeasureTargetKinds are the three kinds of target a measurement is handed, each with
// the path its live transcription goes to: an instance of a replicable model on an agent
// (instance 2, so its address carries a '#', escaped in the agent's /live path), a plain
// model the agent manages, and a direct audio_cpp application, whose live endpoint takes the
// model in its query, verbatim (a quantization suffix's ':' included).
func audioMeasureTargetKinds() []struct {
	name     string
	target   routing.Target
	livePath string
} {
	const query = "channels=1&language=de&sample_format=s16le&sample_rate=16000"
	return []struct {
		name     string
		target   routing.Target
		livePath string
	}{
		{"an agent instance", audioMeasureTarget(), "/live/voice-model%232?" + query},
		{"a plain agent model", routing.Target{
			Provider: routing.ProviderServerAgent, Endpoint: "https://agent.test:8600", Model: "voice", ProviderModel: "voice-model", RouteID: "map_voice",
		}, "/live/voice-model?" + query},
		{"a direct audio_cpp application", routing.Target{
			Provider: routing.ProviderAudioCpp, Endpoint: "http://audio.test:8200", Model: "voice", ProviderModel: "voice-model:Q8_0", RouteID: "map_voice",
		}, "/v1/audio/transcriptions/live?" + query + "&model=voice-model:Q8_0"},
	}
}

// checkAudioMeasurement compares a measurement with the expected one: the times exactly (they
// are whole milliseconds of a scripted clock), the figures within a rounding tolerance. Every
// measurement must also hold the order its times have by definition: the answer starts no
// later than its first output, and the first output comes no later than its end.
func checkAudioMeasurement(t *testing.T, got, want audioMeasurement) {
	t.Helper()
	const tol = 0.005
	if got.FirstByteMS > got.TTFTMS || got.TTFTMS > got.WallMS {
		t.Fatalf("measurement = %+v: want the answer (%d ms) no later than the first output (%d ms), and that no later than the end (%d ms)",
			got, got.FirstByteMS, got.TTFTMS, got.WallMS)
	}
	if got.FirstByteMS != want.FirstByteMS || got.TTFTMS != want.TTFTMS || got.WallMS != want.WallMS ||
		math.Abs(got.AudioSeconds-want.AudioSeconds) > tol ||
		math.Abs(got.RateX-want.RateX) > tol ||
		math.Abs(got.RTF-want.RTF) > tol {
		t.Fatalf("measurement = %+v\nwant          %+v (figures within %v)", got, want, tol)
	}
}

// checkAudioSentTo checks that one request went to exactly the given target and path.
func checkAudioSentTo(t *testing.T, up *measureUpstream, target routing.Target, path, contentType string) {
	t.Helper()
	if up.calls != 1 {
		t.Fatalf("upstream called %d times, want once", up.calls)
	}
	if !reflect.DeepEqual(up.target, target) {
		t.Fatalf("sent to target %+v, want exactly %+v", up.target, target)
	}
	if up.path != path {
		t.Fatalf("sent to path %q, want %q", up.path, path)
	}
	if up.contentType != contentType {
		t.Fatalf("sent as %q, want %q", up.contentType, contentType)
	}
}

// audioSentJSON decodes the JSON body the measurement sent.
func audioSentJSON(t *testing.T, up *measureUpstream) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(up.sent, &body); err != nil {
		t.Fatalf("the request body is no JSON object: %v (%s)", err, up.sent)
	}
	return body
}

// audioSentForm decodes the multipart form the measurement sent.
func audioSentForm(t *testing.T, up *measureUpstream) *multipart.Form {
	t.Helper()
	mediaType, params, err := mime.ParseMediaType(up.contentType)
	if err != nil || mediaType != "multipart/form-data" {
		t.Fatalf("sent as %q (%v), want multipart/form-data", up.contentType, err)
	}
	form, err := multipart.NewReader(strings.NewReader(string(up.sent)), params["boundary"]).ReadForm(4 << 20)
	if err != nil {
		t.Fatalf("the request body is no readable form: %v", err)
	}
	return form
}

// checkAudioRefusal checks that a measurement that cannot give its figures is an
// audioMeasureError of the given kind naming why, carries no figures, and -- when
// sendsNothing -- reached no upstream at all.
func checkAudioRefusal(t *testing.T, got audioMeasurement, err error, kind, want string, up *measureUpstream, sendsNothing bool) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("measurement = %+v, %v; want an error containing %q", got, err, want)
	}
	var merr *audioMeasureError
	if !errors.As(err, &merr) || merr.Kind != kind {
		t.Fatalf("error %q (%#v) is not an audioMeasureError of kind %q", err, merr, kind)
	}
	if got != (audioMeasurement{}) {
		t.Fatalf("a refused measurement carries figures: %+v", got)
	}
	if sendsNothing && up.calls != 0 {
		t.Fatalf("the upstream was called %d times for a measurement that must send nothing", up.calls)
	}
}

// TestMeasureTTSStreamMath: a streamed synthesis whose first audio arrives 300 ms after the
// request and whose speech.audio.done arrives at 2300 ms, carrying 10 s of PCM at 48000 B/s.
// The time to first audio is 300 ms; the production rate is the audio over the time from
// the first chunk to done, 10 s / 2 s = 5x real time; the real-time factor is the whole
// call over the audio, 2.3 s / 10 s. The answer's headers, at 150 ms, are no audio yet.
func TestMeasureTTSStreamMath(t *testing.T) {
	clock := &measureClock{}
	up := scriptedAudioUpstream(clock, "text/event-stream",
		measureAt(300, sseSpeechDelta(96000)),
		measureAt(700, sseSpeechDelta(96000)),
		measureAt(1100, sseSpeechDelta(96000)),
		measureAt(1500, sseSpeechDelta(96000)),
		measureAt(1900, sseSpeechDelta(96000)),
		measureAt(2300, sseSpeechDone),
	)
	up.answerAt = 150 * time.Millisecond
	target := audioMeasureTarget()

	got, err := clock.measurer().measureTTSStream(context.Background(), up, target, audioBenchmarkPassage, "", 48000)
	if err != nil {
		t.Fatalf("measureTTSStream: %v", err)
	}
	checkAudioMeasurement(t, got, audioMeasurement{FirstByteMS: 150, TTFTMS: 300, WallMS: 2300, AudioSeconds: 10, RateX: 5.0, RTF: 0.23})

	// One streamed SSE synthesis of the passage, sent to the instance by its own address, with
	// no voice for a model that needs none and no response_format (a stream is pcm).
	checkAudioSentTo(t, up, target, "/v1/audio/speech", "application/json")
	body := audioSentJSON(t, up)
	if body["model"] != "voice-model#2" || body["input"] != audioBenchmarkPassage || body["stream"] != true || body["stream_format"] != "sse" {
		t.Fatalf("request body = %v; want model voice-model#2, the passage, stream true, stream_format sse", body)
	}
	for _, absent := range []string{"voice", "response_format"} {
		if _, ok := body[absent]; ok {
			t.Fatalf("request body = %v carries %q", body, absent)
		}
	}
}

// TestMeasureTTSStreamTimesFirstAudio: the first audio is the first delta that carries PCM,
// at the moment its frame is whole -- an empty delta before it is no audio, and a frame that
// straddles two reads arrives with its second half -- and a stream that ends on [DONE] with
// no line break after it has still completed. The synthesis ends at speech.audio.done, not
// at a [DONE] that comes later. A clone model's preset voice is named.
func TestMeasureTTSStreamTimesFirstAudio(t *testing.T) {
	delta := sseSpeechDelta(48000)
	cases := []struct {
		name   string
		chunks []measureChunk
		want   audioMeasurement
	}{
		{"an empty delta, then a frame over two reads", []measureChunk{
			measureAt(100, "data: {\"type\":\"speech.audio.delta\",\"audio\":\"\"}\n\n"),
			measureAt(250, delta[:40]),
			measureAt(400, delta[40:]),
			measureAt(1400, sseSpeechDone),
		}, audioMeasurement{FirstByteMS: 1, TTFTMS: 400, WallMS: 1400, AudioSeconds: 1, RateX: 1.0, RTF: 1.4}},
		{"[DONE] without a line break", []measureChunk{
			measureAt(200, delta),
			measureAt(1200, "data: {\"type\":\"speech.audio.done\"}\n\ndata: [DONE]"),
		}, audioMeasurement{FirstByteMS: 1, TTFTMS: 200, WallMS: 1200, AudioSeconds: 1, RateX: 1.0, RTF: 1.2}},
		{"[DONE] well after done: the synthesis ended at done", []measureChunk{
			measureAt(200, delta),
			measureAt(1200, "data: {\"type\":\"speech.audio.done\"}\n\n"),
			measureAt(1700, "data: [DONE]\n\n"),
		}, audioMeasurement{FirstByteMS: 1, TTFTMS: 200, WallMS: 1200, AudioSeconds: 1, RateX: 1.0, RTF: 1.2}},
		{"audio after done is not part of the synthesis", []measureChunk{
			measureAt(200, delta),
			measureAt(1200, "data: {\"type\":\"speech.audio.done\"}\n\n"),
			measureAt(1300, delta),
			measureAt(1400, "data: [DONE]\n\n"),
		}, audioMeasurement{FirstByteMS: 1, TTFTMS: 200, WallMS: 1200, AudioSeconds: 1, RateX: 1.0, RTF: 1.2}},
		{"a frame with no type whose error is null is no error frame", []measureChunk{
			measureAt(200, delta),
			measureAt(700, "data: {\"error\":null}\n\n"),
			measureAt(1200, sseSpeechDone),
		}, audioMeasurement{FirstByteMS: 1, TTFTMS: 200, WallMS: 1200, AudioSeconds: 1, RateX: 1.0, RTF: 1.2}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clock := &measureClock{}
			up := scriptedAudioUpstream(clock, "text/event-stream", tc.chunks...)
			got, err := clock.measurer().measureTTSStream(context.Background(), up, audioMeasureTarget(), audioBenchmarkPassage, "narrator", 48000)
			if err != nil {
				t.Fatalf("measureTTSStream: %v", err)
			}
			checkAudioMeasurement(t, got, tc.want)
			if voice := audioSentJSON(t, up)["voice"]; voice != "narrator" {
				t.Fatalf("request voice = %v, want narrator", voice)
			}
		})
	}
}

// TestMeasureTTSStreamRefusals: a stream that cannot give the figures is an error, never a
// zero or an infinite rate -- and nothing is sent at all when there is no byte rate to count
// the audio with.
func TestMeasureTTSStreamRefusals(t *testing.T) {
	cases := []struct {
		name     string
		byteRate int
		upstream func(*measureClock) *measureUpstream
		kind     string
		want     string // a fragment of the error
	}{
		{"no byte rate", 0, func(c *measureClock) *measureUpstream {
			return scriptedAudioUpstream(c, "text/event-stream", measureAt(300, sseSpeechDelta(48000)), measureAt(800, sseSpeechDone))
		}, audioMeasureInvalidInput, "byte rate"},
		{"the upstream refuses", 48000, func(c *measureClock) *measureUpstream {
			return refusingAudioUpstream(c, http.StatusServiceUnavailable, "model is busy")
		}, audioMeasureUpstreamRefused, "HTTP 503: model is busy"},
		{"the model answers a WAV, it does not stream", 48000, func(c *measureClock) *measureUpstream {
			return scriptedAudioUpstream(c, "audio/wav", measureAt(900, "RIFF"))
		}, audioMeasureProtocol, "not an event stream"},
		{"the model streams raw PCM", 48000, func(c *measureClock) *measureUpstream {
			return scriptedAudioUpstream(c, "application/octet-stream", measureAt(900, "\x00\x00"))
		}, audioMeasureProtocol, "not an event stream"},
		{"done without audio", 48000, func(c *measureClock) *measureUpstream {
			return scriptedAudioUpstream(c, "text/event-stream", measureAt(800, sseSpeechDone))
		}, audioMeasureProtocol, "no audio"},
		{"audio only after done", 48000, func(c *measureClock) *measureUpstream {
			return scriptedAudioUpstream(c, "text/event-stream",
				measureAt(800, "data: {\"type\":\"speech.audio.done\"}\n\n"),
				measureAt(900, sseSpeechDelta(48000)),
				measureAt(1000, "data: [DONE]\n\n"))
		}, audioMeasureProtocol, "no audio"},
		{"a frame past the line bound", 48000, func(c *measureClock) *measureUpstream {
			return scriptedAudioUpstream(c, "text/event-stream",
				measureAt(300, sseSpeechDelta(48000)),
				measureAt(500, "data: {\"type\":\"speech.audio.delta\",\"audio\":\""+strings.Repeat("A", speechStreamMaxLine)+"\"}\n\n"),
				measureAt(800, sseSpeechDone))
		}, audioMeasureProtocol, "a frame longer than"},
		{"all the audio at once", 48000, func(c *measureClock) *measureUpstream {
			return scriptedAudioUpstream(c, "text/event-stream", measureAt(800, sseSpeechDelta(48000)+sseSpeechDone))
		}, audioMeasureProtocol, "no measurable time"},
		{"cut off before done", 48000, func(c *measureClock) *measureUpstream {
			return scriptedAudioUpstream(c, "text/event-stream", measureAt(300, sseSpeechDelta(48000)), measureAt(800, sseSpeechDelta(48000)))
		}, audioMeasureProtocol, "speech.audio.done"},
		{"done without [DONE]", 48000, func(c *measureClock) *measureUpstream {
			return scriptedAudioUpstream(c, "text/event-stream", measureAt(300, sseSpeechDelta(48000)), measureAt(800, "data: {\"type\":\"speech.audio.done\"}\n\n"))
		}, audioMeasureProtocol, "[DONE]"},
		{"a delta that cannot be decoded", 48000, func(c *measureClock) *measureUpstream {
			return scriptedAudioUpstream(c, "text/event-stream",
				measureAt(300, sseSpeechDelta(48000)),
				measureAt(500, "data: {\"type\":\"speech.audio.delta\",\"audio\":\"not base64!\"}\n\n"),
				measureAt(800, sseSpeechDone))
		}, audioMeasureProtocol, "could not be counted"},
		{"the connection breaks", 48000, func(c *measureClock) *measureUpstream {
			return breakingAudioUpstream(c, "text/event-stream", measureAt(300, sseSpeechDelta(48000)))
		}, audioMeasureTransport, "connection reset by peer"},
		{"the upstream reports an error mid-stream", 48000, func(c *measureClock) *measureUpstream {
			return scriptedAudioUpstream(c, "text/event-stream",
				measureAt(300, sseSpeechDelta(48000)),
				measureAt(500, "data: {\"type\":\"error\",\"error\":{\"code\":\"server_error\",\"message\":\"out of memory\"}}\n\n"),
				measureAt(800, sseSpeechDone))
		}, audioMeasureUpstreamRefused, "out of memory"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clock := &measureClock{}
			up := tc.upstream(clock)
			got, err := clock.measurer().measureTTSStream(context.Background(), up, audioMeasureTarget(), audioBenchmarkPassage, "", tc.byteRate)
			checkAudioRefusal(t, got, err, tc.kind, tc.want, up, tc.byteRate <= 0)
		})
	}
}

// TestMeasureStreamsStopAtDone: a stream is read up to its [DONE] and not one read further,
// so an upstream that holds the connection open after it cannot keep a measurement -- and
// the server reservation of the run around it -- waiting for the deadline.
func TestMeasureStreamsStopAtDone(t *testing.T) {
	pcm, sampleRate, _ := benchaudio.PCM()
	cases := map[string]struct {
		contentType string
		chunks      []measureChunk
		measure     func(audioMeasurer, *measureUpstream) (audioMeasurement, error)
	}{
		"TTS": {
			"text/event-stream",
			[]measureChunk{measureAt(300, sseSpeechDelta(48000)), measureAt(800, sseSpeechDone)},
			func(m audioMeasurer, up *measureUpstream) (audioMeasurement, error) {
				return m.measureTTSStream(context.Background(), up, audioMeasureTarget(), audioBenchmarkPassage, "", 48000)
			},
		},
		"live ASR": {
			"text/event-stream",
			[]measureChunk{measureAt(300, sseTranscriptDelta("der")), measureAt(800, sseTranscriptDone("der"))},
			func(m audioMeasurer, up *measureUpstream) (audioMeasurement, error) {
				return m.measureASRLive(context.Background(), up, audioMeasureTarget(), pcm, sampleRate, benchaudio.Language)
			},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			clock := &measureClock{}
			up := scriptedAudioUpstream(clock, tc.contentType, tc.chunks...)
			up.body.end = errors.New("read past the end of the stream")
			got, err := tc.measure(clock.measurer(), up)
			if err != nil {
				t.Fatalf("measurement: %v", err)
			}
			// The stream was read to its [DONE] and measured: every scripted chunk was taken,
			// and the figures end at the 800 ms frame.
			if len(up.body.chunks) != 0 || got.WallMS != 800 {
				t.Fatalf("measurement = %+v with %d chunk(s) unread; want the whole stream read and a wall time of 800 ms", got, len(up.body.chunks))
			}
			if up.body.spentReads != 0 {
				t.Fatalf("the body was read %d time(s) after [DONE], want never", up.body.spentReads)
			}
		})
	}
}

// TestMeasureStreamsStopAtAnyDONE: [DONE] ends the reading of a stream wherever it comes. One
// that arrives before the done event is a stream that broke its protocol -- there is no
// done, so there is no measurement -- and an upstream that sends it first and keeps the
// connection open cannot hold the measurement until its deadline: nothing after it is read.
func TestMeasureStreamsStopAtAnyDONE(t *testing.T) {
	pcm, sampleRate, _ := benchaudio.PCM()
	cases := map[string]struct {
		chunks  []measureChunk
		measure func(audioMeasurer, *measureUpstream) (audioMeasurement, error)
		want    string
	}{
		"TTS": {
			[]measureChunk{
				measureAt(300, sseSpeechDelta(48000)),
				measureAt(500, "data: [DONE]\n\n"),
				measureAt(2000, sseSpeechDelta(48000)+sseSpeechDone),
			},
			func(m audioMeasurer, up *measureUpstream) (audioMeasurement, error) {
				return m.measureTTSStream(context.Background(), up, audioMeasureTarget(), audioBenchmarkPassage, "", 48000)
			},
			"ended before speech.audio.done",
		},
		"live ASR": {
			[]measureChunk{
				measureAt(300, sseTranscriptDelta("der")),
				measureAt(500, "data: [DONE]\n\n"),
				measureAt(2000, sseTranscriptDone("der")),
			},
			func(m audioMeasurer, up *measureUpstream) (audioMeasurement, error) {
				return m.measureASRLive(context.Background(), up, audioMeasureTarget(), pcm, sampleRate, benchaudio.Language)
			},
			"ended before transcript.text.done",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			clock := &measureClock{}
			up := scriptedAudioUpstream(clock, "text/event-stream", tc.chunks...)
			up.body.end = errors.New("read past the end of the stream")
			got, err := tc.measure(clock.measurer(), up)
			checkAudioRefusal(t, got, err, audioMeasureProtocol, tc.want, up, false)
			if len(up.body.chunks) != 1 || up.body.spentReads != 0 {
				t.Fatalf("%d chunk(s) left unread and %d read(s) past the end; want the stream read up to its [DONE] and not one read further",
					len(up.body.chunks), up.body.spentReads)
			}
		})
	}
}

// TestMeasureTTSBufferedMath: a non-streamed synthesis is usable only once it is whole, so
// its time to first audio is the wall time to the end of the answer; 3 s of audio whole after
// 1500 ms is 2x real time. Its first response byte is the answer's start at 1100 ms, before
// any of the WAV is read. A clone model's preset voice is named in the request.
func TestMeasureTTSBufferedMath(t *testing.T) {
	clock := &measureClock{}
	wav := buildPCMWav(t, 24000, 1, 16, 144000) // 3 s
	up := scriptedAudioUpstream(clock, "audio/wav", measureAt(1200, string(wav[:44])), measureAt(1500, string(wav[44:])))
	up.answerAt = 1100 * time.Millisecond
	target := audioMeasureTarget()

	got, err := clock.measurer().measureTTSBuffered(context.Background(), up, target, audioBenchmarkPassage, "narrator")
	if err != nil {
		t.Fatalf("measureTTSBuffered: %v", err)
	}
	checkAudioMeasurement(t, got, audioMeasurement{FirstByteMS: 1100, TTFTMS: 1500, WallMS: 1500, AudioSeconds: 3, RateX: 2.0, RTF: 0.5})

	checkAudioSentTo(t, up, target, "/v1/audio/speech", "application/json")
	body := audioSentJSON(t, up)
	if body["model"] != "voice-model#2" || body["input"] != audioBenchmarkPassage || body["voice"] != "narrator" {
		t.Fatalf("request body = %v; want model voice-model#2, the passage, voice narrator", body)
	}
	for _, absent := range []string{"stream", "stream_format", "response_format"} {
		if _, ok := body[absent]; ok {
			t.Fatalf("request body = %v carries %q: a buffered synthesis is the model's plain WAV answer", body, absent)
		}
	}
}

// TestMeasureASRBufferedMath: the bundled 22.85 s clip transcribed in 1000 ms is 22.85x real
// time, and the answer, whole only at its end, is also the time to the first text; its first
// response byte is the answer's start at 950 ms.
func TestMeasureASRBufferedMath(t *testing.T) {
	clock := &measureClock{}
	up := scriptedAudioUpstream(clock, "application/json", measureAt(1000, `{"text":"der schutz der menschen vor hochwasser","timing":{"audio_duration_ms":22850}}`))
	up.answerAt = 950 * time.Millisecond
	target := audioMeasureTarget()
	wav := benchaudio.WAV()

	got, err := clock.measurer().measureASRBuffered(context.Background(), up, target, wav, benchaudio.Language)
	if err != nil {
		t.Fatalf("measureASRBuffered: %v", err)
	}
	checkAudioMeasurement(t, got, audioMeasurement{FirstByteMS: 950, TTFTMS: 1000, WallMS: 1000, AudioSeconds: 22.85, RateX: 22.85, RTF: 1 / 22.85})

	// One multipart transcription: the instance's own name, the language, and the clip as
	// the WAV file part -- and no stream field, so the answer is the one-shot JSON.
	if up.calls != 1 || up.path != "/v1/audio/transcriptions" || !reflect.DeepEqual(up.target, target) {
		t.Fatalf("sent %d request(s) to %q at %+v; want one to /v1/audio/transcriptions at %+v", up.calls, up.path, up.target, target)
	}
	form := audioSentForm(t, up)
	if got, want := form.Value, map[string][]string{"model": {"voice-model#2"}, "language": {"de"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("form fields = %v, want %v", got, want)
	}
	files := form.File["file"]
	if len(files) != 1 || files[0].Filename != "audio.wav" || files[0].Header.Get("Content-Type") != "audio/wav" {
		t.Fatalf("file parts = %+v, want one audio.wav of type audio/wav", files)
	}
	f, err := files[0].Open()
	if err != nil {
		t.Fatalf("open the file part: %v", err)
	}
	defer f.Close()
	if sent, _ := io.ReadAll(f); string(sent) != string(wav) {
		t.Fatalf("the file part holds %d bytes, not the %d-byte clip", len(sent), len(wav))
	}
}

// TestMeasureASRBufferedWithoutLanguage: with no language the request states none, and the
// model detects it -- no empty language field it would have to make sense of.
func TestMeasureASRBufferedWithoutLanguage(t *testing.T) {
	clock := &measureClock{}
	up := scriptedAudioUpstream(clock, "application/json", measureAt(500, `{"text":"der schutz der menschen"}`))
	if _, err := clock.measurer().measureASRBuffered(context.Background(), up, audioMeasureTarget(), benchaudio.WAV(), ""); err != nil {
		t.Fatalf("measureASRBuffered: %v", err)
	}
	if got, want := audioSentForm(t, up).Value, map[string][]string{"model": {"voice-model#2"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("form fields = %v, want %v", got, want)
	}
}

// TestMeasureASRBufferedLengthIsTheClips: the length a buffered transcription's rate is taken
// over is the clip that was sent, read from its own WAV header, never what the answer claims
// about it -- a model that reports a different duration, or none, does not change the
// figure -- so a 3 s clip is 3 s and the bundled clip 22.85 s, whatever the answer says.
func TestMeasureASRBufferedLengthIsTheClips(t *testing.T) {
	cases := []struct {
		name    string
		clip    []byte
		answer  string
		seconds float64
	}{
		{"the bundled clip, the answer claims 5 s", benchaudio.WAV(), `{"text":"der schutz","duration":5,"timing":{"audio_duration_ms":5000}}`, 22.85},
		{"the bundled clip, the answer states no length", benchaudio.WAV(), `{"text":"der schutz"}`, 22.85},
		{"a 3 s clip, the answer claims 22.85 s", buildPCMWav(t, 16000, 1, 16, 96000), `{"text":"der schutz","duration":22.85,"timing":{"audio_duration_ms":22850}}`, 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clock := &measureClock{}
			up := scriptedAudioUpstream(clock, "application/json", measureAt(1000, tc.answer))
			got, err := clock.measurer().measureASRBuffered(context.Background(), up, audioMeasureTarget(), tc.clip, benchaudio.Language)
			if err != nil {
				t.Fatalf("measureASRBuffered: %v", err)
			}
			checkAudioMeasurement(t, got, audioMeasurement{FirstByteMS: 1, TTFTMS: 1000, WallMS: 1000, AudioSeconds: tc.seconds, RateX: tc.seconds, RTF: 1 / tc.seconds})
		})
	}
}

// TestMeasureBufferedRefusals: a buffered answer that cannot give the figures is an error,
// never a zero or an infinite rate; a clip whose length cannot be read is refused before
// anything is sent.
func TestMeasureBufferedRefusals(t *testing.T) {
	wav := buildPCMWav(t, 24000, 1, 16, 48000)
	tts := func(m audioMeasurer, up *measureUpstream) (audioMeasurement, error) {
		return m.measureTTSBuffered(context.Background(), up, audioMeasureTarget(), audioBenchmarkPassage, "")
	}
	asr := func(m audioMeasurer, up *measureUpstream) (audioMeasurement, error) {
		return m.measureASRBuffered(context.Background(), up, audioMeasureTarget(), benchaudio.WAV(), benchaudio.Language)
	}
	cases := []struct {
		name         string
		upstream     func(*measureClock) *measureUpstream
		measure      func(audioMeasurer, *measureUpstream) (audioMeasurement, error)
		kind         string
		want         string
		sendsNothing bool
	}{
		{"TTS: the upstream refuses", func(c *measureClock) *measureUpstream {
			return refusingAudioUpstream(c, http.StatusInternalServerError, "needs a reference voice")
		}, tts, audioMeasureUpstreamRefused, "HTTP 500: needs a reference voice", false},
		{"TTS: the answer is no WAV", func(c *measureClock) *measureUpstream {
			return scriptedAudioUpstream(c, "application/json", measureAt(900, `{"audio":"UklGRg=="}`))
		}, tts, audioMeasureProtocol, "not labelled as WAV", false},
		{"TTS: the WAV states no length", func(c *measureClock) *measureUpstream {
			return scriptedAudioUpstream(c, "audio/wav", measureAt(900, "RIFF but not really"))
		}, tts, audioMeasureProtocol, "length", false},
		{"TTS: the connection breaks", func(c *measureClock) *measureUpstream {
			return breakingAudioUpstream(c, "audio/wav", measureAt(900, string(wav[:100])))
		}, tts, audioMeasureTransport, "connection reset by peer", false},
		{"TTS: the answer takes no time", func(c *measureClock) *measureUpstream {
			return scriptedAudioUpstream(c, "audio/wav", measureAt(0, string(wav)))
		}, tts, audioMeasureProtocol, "no measurable time", false},
		{"TTS: the answer is empty", func(c *measureClock) *measureUpstream {
			return scriptedAudioUpstream(c, "audio/wav")
		}, tts, audioMeasureProtocol, "the answer is empty", false},
		{"ASR: the clip is no WAV", func(c *measureClock) *measureUpstream {
			return scriptedAudioUpstream(c, "application/json", measureAt(900, `{"text":"hallo"}`))
		}, func(m audioMeasurer, up *measureUpstream) (audioMeasurement, error) {
			return m.measureASRBuffered(context.Background(), up, audioMeasureTarget(), []byte("not a wav"), "de")
		}, audioMeasureInvalidInput, "clip", true},
		{"ASR: the clip is a WAV of no audio", func(c *measureClock) *measureUpstream {
			return scriptedAudioUpstream(c, "application/json", measureAt(900, `{"text":"hallo"}`))
		}, func(m audioMeasurer, up *measureUpstream) (audioMeasurement, error) {
			return m.measureASRBuffered(context.Background(), up, audioMeasureTarget(), buildPCMWav(t, 16000, 1, 16, 0), "de")
		}, audioMeasureInvalidInput, "clip", true},
		{"ASR: there is no clip", func(c *measureClock) *measureUpstream {
			return scriptedAudioUpstream(c, "application/json", measureAt(900, `{"text":"hallo"}`))
		}, func(m audioMeasurer, up *measureUpstream) (audioMeasurement, error) {
			return m.measureASRBuffered(context.Background(), up, audioMeasureTarget(), nil, "de")
		}, audioMeasureInvalidInput, "clip", true},
		{"ASR: the clip's header is cut short", func(c *measureClock) *measureUpstream {
			return scriptedAudioUpstream(c, "application/json", measureAt(900, `{"text":"hallo"}`))
		}, func(m audioMeasurer, up *measureUpstream) (audioMeasurement, error) {
			return m.measureASRBuffered(context.Background(), up, audioMeasureTarget(), benchaudio.WAV()[:30], "de")
		}, audioMeasureInvalidInput, "clip", true},
		{"ASR: the upstream refuses", func(c *measureClock) *measureUpstream {
			return refusingAudioUpstream(c, http.StatusNotFound, "unknown model id")
		}, asr, audioMeasureUpstreamRefused, "HTTP 404: unknown model id", false},
		{"ASR: the answer is no transcription", func(c *measureClock) *measureUpstream {
			return scriptedAudioUpstream(c, "text/html", measureAt(900, "<html>gateway timeout</html>"))
		}, asr, audioMeasureProtocol, "no transcription", false},
		{"ASR: the answer is JSON without a text", func(c *measureClock) *measureUpstream {
			return scriptedAudioUpstream(c, "application/json", measureAt(900, `{"segments":[]}`))
		}, asr, audioMeasureProtocol, "no transcription", false},
		{"ASR: the transcript is empty", func(c *measureClock) *measureUpstream {
			return scriptedAudioUpstream(c, "application/json", measureAt(900, `{"text":"  "}`))
		}, asr, audioMeasureProtocol, "no text", false},
		{"ASR: the connection breaks", func(c *measureClock) *measureUpstream {
			return breakingAudioUpstream(c, "application/json", measureAt(900, `{"text":"der sch`))
		}, asr, audioMeasureTransport, "connection reset by peer", false},
		{"ASR: the answer takes no time", func(c *measureClock) *measureUpstream {
			return scriptedAudioUpstream(c, "application/json", measureAt(0, `{"text":"hallo"}`))
		}, asr, audioMeasureProtocol, "no measurable time", false},
		{"ASR: the answer is empty", func(c *measureClock) *measureUpstream {
			return scriptedAudioUpstream(c, "application/json")
		}, asr, audioMeasureProtocol, "the answer is empty", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clock := &measureClock{}
			up := tc.upstream(clock)
			got, err := tc.measure(clock.measurer(), up)
			checkAudioRefusal(t, got, err, tc.kind, tc.want, up, tc.sendsNothing)
		})
	}
}

// TestMeasureReadsBoundedAnswers: a one-shot answer -- the speech benchmark's and the audio
// measurements' alike, read by the one helper they share -- is read whole up to its bound,
// refused past it (a runaway upstream cannot fill memory before the deadline ends the call)
// and refused when it is empty; a body that breaks off is an error that still carries its
// cause. The end is taken once, when the read returns, before anything read is checked.
func TestMeasureReadsBoundedAnswers(t *testing.T) {
	clock := &measureClock{}
	nows := 0
	now := func() time.Time { nows++; return clock.now() }
	read := func(body io.Reader) ([]byte, time.Time, error) {
		nows = 0
		got, end, err := readBenchmarkAnswer(body, 4, now)
		if nows != 1 {
			t.Fatalf("readBenchmarkAnswer read the clock %d times, want once", nows)
		}
		return got, end, err
	}
	at := func(ms int, data string) io.Reader {
		return &measureBody{clock: clock, chunks: []measureChunk{measureAt(ms, data)}}
	}

	if got, end, err := read(at(700, "1234")); err != nil || string(got) != "1234" || end != measureEpoch.Add(700*time.Millisecond) {
		t.Fatalf("readBenchmarkAnswer at the bound = %q, %v, %v; want the whole answer, ending at 700 ms", got, end, err)
	}
	if got, _, err := read(at(700, "12345")); !errors.Is(err, errBenchmarkAnswerTooLarge) || err.Error() != "the answer exceeds 4 bytes" || got != nil {
		t.Fatalf("readBenchmarkAnswer past the bound = %q, %v; want an error naming the bound", got, err)
	}
	if got, _, err := read(strings.NewReader("")); !errors.Is(err, errBenchmarkAnswerEmpty) || err.Error() != "the answer is empty" || got != nil {
		t.Fatalf("readBenchmarkAnswer of an empty body = %q, %v; want the empty answer refused", got, err)
	}
	cut := errors.New("connection reset by peer")
	broken := &measureBody{clock: clock, chunks: []measureChunk{measureAt(300, "12")}, end: cut}
	if got, _, err := read(broken); !errors.Is(err, cut) || errors.Is(err, errBenchmarkAnswerEmpty) || got != nil ||
		err.Error() != "the answer could not be read to its end: connection reset by peer" {
		t.Fatalf("readBenchmarkAnswer of a body that broke off = %q, %v; want its cause", got, err)
	}
}

// TestMeasureASRLiveMath: the bundled clip sent unpaced over the live endpoint, its first
// transcript.text.delta 400 ms after the request and transcript.text.done at 2000 ms. The
// time to first text is 400 ms; the rate is the clip over the whole exchange, 22.85 s / 2 s.
func TestMeasureASRLiveMath(t *testing.T) {
	clock := &measureClock{}
	up := scriptedAudioUpstream(clock, "text/event-stream; charset=utf-8",
		measureAt(400, sseTranscriptDelta("der schutz ")),
		measureAt(900, sseTranscriptDelta("der menschen")),
		measureAt(2000, sseTranscriptDone("der schutz der menschen")),
	)
	up.answerAt = 50 * time.Millisecond
	target := audioMeasureTarget()
	pcm, sampleRate, channels := benchaudio.PCM()
	if channels != 1 {
		t.Fatalf("the bundled clip has %d channels; the live measurement sends mono", channels)
	}

	got, err := clock.measurer().measureASRLive(context.Background(), up, target, pcm, sampleRate, benchaudio.Language)
	if err != nil {
		t.Fatalf("measureASRLive: %v", err)
	}
	checkAudioMeasurement(t, got, audioMeasurement{FirstByteMS: 50, TTFTMS: 400, WallMS: 2000, AudioSeconds: 22.85, RateX: 22.85 / 2, RTF: 2 / 22.85})

	// The agent's /live route for the instance's own address (its '#' escaped in the path),
	// the clip's PCM shape and language in the query, and every byte of the PCM as the body.
	checkAudioSentTo(t, up, target, "/live/voice-model%232?channels=1&language=de&sample_format=s16le&sample_rate=16000", "application/octet-stream")
	if string(up.sent) != string(pcm) {
		t.Fatalf("sent %d bytes of PCM, want the clip's %d", len(up.sent), len(pcm))
	}
}

// TestMeasureASRLiveTimesFirstText: the time to first text is the first frame before
// transcript.text.done that carries transcript text, a delta or a partial -- a model that
// revises its hypothesis shows a transcript.text.partial first -- and for a model that streams
// no increments, whose text first arrives in transcript.text.done, it is the done event, the
// whole time. A frame without text is not the first text. The transcription ends at
// transcript.text.done, not at a [DONE] that comes later, and a frame after done is no part
// of it: it is never the first text, so the first text never comes after the end.
func TestMeasureASRLiveTimesFirstText(t *testing.T) {
	cases := []struct {
		name   string
		chunks []measureChunk
		ttft   int64
	}{
		{"a partial first", []measureChunk{
			measureAt(100, sseTranscriptDelta("")),
			measureAt(300, "data: {\"type\":\"transcript.text.partial\",\"text\":\"der schu\"}\n\n"),
			measureAt(600, sseTranscriptDelta("der schutz")),
			measureAt(1000, sseTranscriptDone("der schutz")),
		}, 300},
		{"the text only at done", []measureChunk{
			measureAt(1000, sseTranscriptDone("der schutz")),
		}, 1000},
		{"[DONE] well after done: the transcription ended at done", []measureChunk{
			measureAt(200, sseTranscriptDelta("der schutz")),
			measureAt(1000, fmt.Sprintf("data: {\"type\":%q,\"text\":\"der schutz\"}\n\n", transcriptionEventDone)),
			measureAt(1600, "data: [DONE]\n\n"),
		}, 200},
		{"text after done is no part of the transcription", []measureChunk{
			measureAt(100, sseTranscriptDelta("")),
			measureAt(1000, fmt.Sprintf("data: {\"type\":%q,\"text\":\"der schutz\"}\n\n", transcriptionEventDone)),
			measureAt(1300, sseTranscriptDelta("nachtrag")),
			measureAt(1400, "data: {\"type\":\"transcript.text.partial\",\"text\":\"nachtrag\"}\n\n"),
			measureAt(1600, "data: [DONE]\n\n"),
		}, 1000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clock := &measureClock{}
			up := scriptedAudioUpstream(clock, "text/event-stream", tc.chunks...)
			pcm, sampleRate, _ := benchaudio.PCM()
			got, err := clock.measurer().measureASRLive(context.Background(), up, audioMeasureTarget(), pcm, sampleRate, benchaudio.Language)
			if err != nil {
				t.Fatalf("measureASRLive: %v", err)
			}
			checkAudioMeasurement(t, got, audioMeasurement{FirstByteMS: 1, TTFTMS: tc.ttft, WallMS: 1000, AudioSeconds: 22.85, RateX: 22.85, RTF: 1 / 22.85})
		})
	}
}

// TestMeasureASRLiveRefusals: a live exchange that cannot give the figures is an error -- and
// nothing is sent when the PCM's shape, its length or the language is nothing the live
// endpoint takes.
func TestMeasureASRLiveRefusals(t *testing.T) {
	pcm, sampleRate, _ := benchaudio.PCM()
	cases := []struct {
		name         string
		pcm          []byte
		sampleRate   int
		language     string
		upstream     func(*measureClock) *measureUpstream
		kind         string
		want         string
		sendsNothing bool
	}{
		{"a sample rate the live endpoint does not take", pcm, 0, "de", nil, audioMeasureInvalidInput, "sample rate", true},
		{"no audio", nil, sampleRate, "de", nil, audioMeasureInvalidInput, "no audio", true},
		{"half a sample", pcm[:3], sampleRate, "de", nil, audioMeasureInvalidInput, "whole number", true},
		{"a language that is no bare tag", pcm, sampleRate, "de&model=other", nil, audioMeasureInvalidInput, "language", true},
		{"the upstream refuses", pcm, sampleRate, "de", func(c *measureClock) *measureUpstream {
			return refusingAudioUpstream(c, http.StatusServiceUnavailable, "live session busy")
		}, audioMeasureUpstreamRefused, "HTTP 503: live session busy", false},
		{"the answer is no event stream", pcm, sampleRate, "de", func(c *measureClock) *measureUpstream {
			return scriptedAudioUpstream(c, "application/json", measureAt(900, `{"text":"der schutz"}`))
		}, audioMeasureProtocol, "not an event stream", false},
		{"the upstream reports an error mid-stream", pcm, sampleRate, "de", func(c *measureClock) *measureUpstream {
			return scriptedAudioUpstream(c, "text/event-stream",
				measureAt(400, sseTranscriptDelta("der")),
				measureAt(900, "data: {\"type\":\"error\",\"message\":\"Audio chunker requires positive input samples\"}\n\n"))
		}, audioMeasureUpstreamRefused, "Audio chunker requires positive input samples", false},
		{"no text at all", pcm, sampleRate, "de", func(c *measureClock) *measureUpstream {
			return scriptedAudioUpstream(c, "text/event-stream", measureAt(1000, sseTranscriptDone("")))
		}, audioMeasureProtocol, "no text", false},
		{"text only after done", pcm, sampleRate, "de", func(c *measureClock) *measureUpstream {
			return scriptedAudioUpstream(c, "text/event-stream",
				measureAt(200, sseTranscriptDelta("")),
				measureAt(1000, fmt.Sprintf("data: {\"type\":%q,\"text\":\"\"}\n\n", transcriptionEventDone)),
				measureAt(1300, sseTranscriptDelta("der schutz")),
				measureAt(1400, sseTranscriptDone("der schutz")))
		}, audioMeasureProtocol, "no text", false},
		{"cut off before done", pcm, sampleRate, "de", func(c *measureClock) *measureUpstream {
			return scriptedAudioUpstream(c, "text/event-stream", measureAt(400, sseTranscriptDelta("der schutz")))
		}, audioMeasureProtocol, "transcript.text.done", false},
		{"done without [DONE]", pcm, sampleRate, "de", func(c *measureClock) *measureUpstream {
			return scriptedAudioUpstream(c, "text/event-stream",
				measureAt(400, sseTranscriptDelta("der schutz")),
				measureAt(900, fmt.Sprintf("data: {\"type\":%q,\"text\":\"der schutz\"}\n\n", transcriptionEventDone)))
		}, audioMeasureProtocol, "[DONE]", false},
		{"[DONE] before done", pcm, sampleRate, "de", func(c *measureClock) *measureUpstream {
			return scriptedAudioUpstream(c, "text/event-stream", measureAt(400, sseTranscriptDelta("der schutz")+"data: [DONE]\n\n"))
		}, audioMeasureProtocol, "transcript.text.done", false},
		{"the connection breaks", pcm, sampleRate, "de", func(c *measureClock) *measureUpstream {
			return breakingAudioUpstream(c, "text/event-stream", measureAt(400, sseTranscriptDelta("der schutz")))
		}, audioMeasureTransport, "connection reset by peer", false},
		{"a line past the bound", pcm, sampleRate, "de", func(c *measureClock) *measureUpstream {
			return scriptedAudioUpstream(c, "text/event-stream",
				measureAt(400, sseTranscriptDelta(strings.Repeat("a", maxLiveSSELineBytes))),
				measureAt(900, sseTranscriptDone("der schutz")))
		}, audioMeasureProtocol, "longer than", false},
		{"done the moment it was sent", pcm, sampleRate, "de", func(c *measureClock) *measureUpstream {
			return scriptedAudioUpstream(c, "text/event-stream", measureAt(0, sseTranscriptDelta("der")+sseTranscriptDone("der")))
		}, audioMeasureProtocol, "no measurable time", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clock := &measureClock{}
			up := scriptedAudioUpstream(clock, "text/event-stream")
			if tc.upstream != nil {
				up = tc.upstream(clock)
			}
			got, err := clock.measurer().measureASRLive(context.Background(), up, audioMeasureTarget(), tc.pcm, tc.sampleRate, tc.language)
			checkAudioRefusal(t, got, err, tc.kind, tc.want, up, tc.sendsNothing)
		})
	}
}

// TestMeasureRefusalsCarryTheUpstreamsWords: a target that refuses the work is told apart
// from one that failed or answered nonsense, by kind upstream_refused, and the refusal carries
// what the upstream said -- the HTTP status of a refusal answered as one (the agent's 409 for
// an instance it has no room for, or whose demand it does not know yet), the stable code and
// the message, or the code and message of an error frame inside a stream. That is what lets
// the capacity ramp tell an instance the agent refused (a limit of resources) from a level
// that was too slow (a limit of performance).
func TestMeasureRefusalsCarryTheUpstreamsWords(t *testing.T) {
	pcm, sampleRate, _ := benchaudio.PCM()
	ctx := context.Background()
	measurements := map[string]func(audioMeasurer, *measureUpstream) (audioMeasurement, error){
		"TTS stream": func(m audioMeasurer, up *measureUpstream) (audioMeasurement, error) {
			return m.measureTTSStream(ctx, up, audioMeasureTarget(), audioBenchmarkPassage, "", 48000)
		},
		"TTS buffered": func(m audioMeasurer, up *measureUpstream) (audioMeasurement, error) {
			return m.measureTTSBuffered(ctx, up, audioMeasureTarget(), audioBenchmarkPassage, "")
		},
		"ASR buffered": func(m audioMeasurer, up *measureUpstream) (audioMeasurement, error) {
			return m.measureASRBuffered(ctx, up, audioMeasureTarget(), benchaudio.WAV(), benchaudio.Language)
		},
		"ASR live": func(m audioMeasurer, up *measureUpstream) (audioMeasurement, error) {
			return m.measureASRLive(ctx, up, audioMeasureTarget(), pcm, sampleRate, benchaudio.Language)
		},
	}
	check := func(t *testing.T, err error, want audioMeasureError) {
		t.Helper()
		var merr *audioMeasureError
		if !errors.As(err, &merr) {
			t.Fatalf("error %v is no audioMeasureError", err)
		}
		if merr.Kind != want.Kind || merr.Status != want.Status || merr.Code != want.Code || merr.Message != want.Message || merr.Err != nil {
			t.Fatalf("error = %#v\nwant    %#v", *merr, want)
		}
	}

	for name, measure := range measurements {
		for _, code := range []string{"runtime.instance_no_capacity", "runtime.instance_demand_unknown"} {
			t.Run(name+": the agent answers "+code, func(t *testing.T) {
				clock := &measureClock{}
				up := refusingAgentUpstream(clock, http.StatusConflict, code, code+": no room on this host")
				got, err := measure(clock.measurer(), up)
				checkAudioRefusal(t, got, err, audioMeasureUpstreamRefused, "HTTP 409: "+code+": no room on this host", up, false)
				check(t, err, audioMeasureError{Kind: audioMeasureUpstreamRefused, Status: http.StatusConflict, Code: code, Message: code + ": no room on this host"})
			})
		}
		t.Run(name+": a refusal with a message and no code", func(t *testing.T) {
			clock := &measureClock{}
			up := refusingAudioUpstream(clock, http.StatusServiceUnavailable, "model is busy")
			_, err := measure(clock.measurer(), up)
			check(t, err, audioMeasureError{Kind: audioMeasureUpstreamRefused, Status: http.StatusServiceUnavailable, Message: "model is busy"})
			if err.Error() != "audio benchmark: the upstream answered HTTP 503: model is busy" {
				t.Fatalf("error text = %q", err)
			}
		})
	}

	frames := []struct {
		name, frame   string
		code, message string
	}{
		{"an error object", `{"type":"error","error":{"code":"server_error","message":"out of memory"}}`, "server_error", "out of memory"},
		{"code and message at the top", `{"type":"error","code":"busy","message":"too many sessions"}`, "busy", "too many sessions"},
		{"a message alone", `{"type":"error","message":"Audio chunker requires positive input samples"}`, "", "Audio chunker requires positive input samples"},
		{"an error that is a string", `{"type":"error","error":"the model was unloaded"}`, "", "the model was unloaded"},
		{
			"no type, the agent's error envelope", `{"error":{"code":"runtime.upstream_gone","message":"runtime: upstream body read failed: unexpected EOF"}}`,
			"runtime.upstream_gone", "runtime: upstream body read failed: unexpected EOF",
		},
	}
	// The frame comes between a first output and the stream's regular end, which is never
	// read: the error frame ends the measurement.
	streams := map[string]struct{ before, after string }{
		"TTS stream": {sseSpeechDelta(48000), sseSpeechDone},
		"ASR live":   {sseTranscriptDelta("der"), sseTranscriptDone("der")},
	}
	for name, stream := range streams {
		for _, f := range frames {
			t.Run(name+": an error frame with "+f.name, func(t *testing.T) {
				clock := &measureClock{}
				up := scriptedAudioUpstream(clock, "text/event-stream",
					measureAt(300, stream.before),
					measureAt(500, "data: "+f.frame+"\n\n"),
					measureAt(800, stream.after))
				up.body.end = errors.New("read past the end of the stream")
				got, err := measurements[name](clock.measurer(), up)
				checkAudioRefusal(t, got, err, audioMeasureUpstreamRefused, f.message, up, false)
				check(t, err, audioMeasureError{Kind: audioMeasureUpstreamRefused, Code: f.code, Message: f.message})
				if len(up.body.chunks) != 1 {
					t.Fatalf("%d chunk(s) left unread; want the stream given up at the error frame", len(up.body.chunks))
				}
			})
		}
	}

	// The agent's runtime router answers a streamed request lazily: while it waits for the
	// instance (a cold start, a moved address) it commits 200 and sends keepalive comments,
	// and a failure after that can only be its terminal frame, the error envelope with no
	// type. That frame is the refusal with the agent's code, not a stream cut short.
	for name, stream := range streams {
		t.Run(name+": the agent's terminal frame after its keepalives", func(t *testing.T) {
			const message = `runtime: "voice-model#2" names something else than when the request was read; select again: runtime.instance_no_capacity`
			clock := &measureClock{}
			up := scriptedAudioUpstream(clock, "text/event-stream",
				measureAt(300, ": keepalive\n\n"),
				measureAt(600, ": keepalive\n\n"),
				measureAt(700, fmt.Sprintf("data: {\"error\":{\"code\":\"runtime.instance_no_capacity\",\"message\":%q}}\n\n", message)),
				measureAt(800, stream.after))
			up.body.end = errors.New("read past the end of the stream")
			got, err := measurements[name](clock.measurer(), up)
			checkAudioRefusal(t, got, err, audioMeasureUpstreamRefused, "select again", up, false)
			check(t, err, audioMeasureError{Kind: audioMeasureUpstreamRefused, Code: "runtime.instance_no_capacity", Message: message})
			if len(up.body.chunks) != 1 {
				t.Fatalf("%d chunk(s) left unread; want the stream given up at the error frame", len(up.body.chunks))
			}
		})
	}
}

// TestMeasureAudioTargetKinds: every measurement measures each kind of target it is handed --
// an agent instance, a plain agent-managed model and a direct audio_cpp application -- with
// one request to exactly that target, at its own path and by its own model name, and gives
// the same figures for the same answer whatever the kind.
func TestMeasureAudioTargetKinds(t *testing.T) {
	pcm, sampleRate, _ := benchaudio.PCM()
	wav := buildPCMWav(t, 24000, 1, 16, 144000) // 3 s
	ctx := context.Background()
	for _, kind := range audioMeasureTargetKinds() {
		target, model := kind.target, kind.target.ProviderModel
		t.Run(kind.name+": TTS stream", func(t *testing.T) {
			clock := &measureClock{}
			up := scriptedAudioUpstream(clock, "text/event-stream", measureAt(300, sseSpeechDelta(48000)), measureAt(800, sseSpeechDone))
			got, err := clock.measurer().measureTTSStream(ctx, up, target, audioBenchmarkPassage, "", 48000)
			if err != nil {
				t.Fatalf("measureTTSStream: %v", err)
			}
			checkAudioMeasurement(t, got, audioMeasurement{FirstByteMS: 1, TTFTMS: 300, WallMS: 800, AudioSeconds: 1, RateX: 2, RTF: 0.8})
			checkAudioSentTo(t, up, target, "/v1/audio/speech", "application/json")
			if body := audioSentJSON(t, up); body["model"] != model || body["stream"] != true {
				t.Fatalf("request body = %v; want model %q, streamed", body, model)
			}
		})
		t.Run(kind.name+": TTS buffered", func(t *testing.T) {
			clock := &measureClock{}
			up := scriptedAudioUpstream(clock, "audio/wav", measureAt(1500, string(wav)))
			got, err := clock.measurer().measureTTSBuffered(ctx, up, target, audioBenchmarkPassage, "")
			if err != nil {
				t.Fatalf("measureTTSBuffered: %v", err)
			}
			checkAudioMeasurement(t, got, audioMeasurement{FirstByteMS: 1, TTFTMS: 1500, WallMS: 1500, AudioSeconds: 3, RateX: 2, RTF: 0.5})
			checkAudioSentTo(t, up, target, "/v1/audio/speech", "application/json")
			if body := audioSentJSON(t, up); body["model"] != model {
				t.Fatalf("request body = %v; want model %q", body, model)
			}
		})
		t.Run(kind.name+": ASR buffered", func(t *testing.T) {
			clock := &measureClock{}
			up := scriptedAudioUpstream(clock, "application/json", measureAt(1000, `{"text":"der schutz"}`))
			got, err := clock.measurer().measureASRBuffered(ctx, up, target, benchaudio.WAV(), benchaudio.Language)
			if err != nil {
				t.Fatalf("measureASRBuffered: %v", err)
			}
			checkAudioMeasurement(t, got, audioMeasurement{FirstByteMS: 1, TTFTMS: 1000, WallMS: 1000, AudioSeconds: 22.85, RateX: 22.85, RTF: 1 / 22.85})
			if up.calls != 1 || up.path != "/v1/audio/transcriptions" || !reflect.DeepEqual(up.target, target) {
				t.Fatalf("sent %d request(s) to %q at %+v; want one to /v1/audio/transcriptions at %+v", up.calls, up.path, up.target, target)
			}
			if got := audioSentForm(t, up).Value["model"]; !reflect.DeepEqual(got, []string{model}) {
				t.Fatalf("form model = %v, want %q", got, model)
			}
		})
		t.Run(kind.name+": ASR live", func(t *testing.T) {
			clock := &measureClock{}
			up := scriptedAudioUpstream(clock, "text/event-stream", measureAt(400, sseTranscriptDelta("der")), measureAt(2000, sseTranscriptDone("der schutz")))
			got, err := clock.measurer().measureASRLive(ctx, up, target, pcm, sampleRate, benchaudio.Language)
			if err != nil {
				t.Fatalf("measureASRLive: %v", err)
			}
			checkAudioMeasurement(t, got, audioMeasurement{FirstByteMS: 1, TTFTMS: 400, WallMS: 2000, AudioSeconds: 22.85, RateX: 22.85 / 2, RTF: 2 / 22.85})
			checkAudioSentTo(t, up, target, kind.livePath, "application/octet-stream")
		})
	}
}

// TestMeasureAudioEntryPoints: the four production entry points reach the upstream they are
// given; a transport failure comes back as their error, of kind transport and wrapped so a
// caller can still tell what it was. TestMeasureAudioEntryPointsReadTheRealClock shows they
// time on the real clock.
func TestMeasureAudioEntryPoints(t *testing.T) {
	dialErr := errors.New("dial tcp: connection refused")
	pcm, sampleRate, _ := benchaudio.PCM()
	ctx := context.Background()
	calls := map[string]func(*measureUpstream) (audioMeasurement, error){
		"measureTTSStream": func(up *measureUpstream) (audioMeasurement, error) {
			return measureTTSStream(ctx, up, audioMeasureTarget(), audioBenchmarkPassage, "", 48000)
		},
		"measureTTSBuffered": func(up *measureUpstream) (audioMeasurement, error) {
			return measureTTSBuffered(ctx, up, audioMeasureTarget(), audioBenchmarkPassage, "")
		},
		"measureASRBuffered": func(up *measureUpstream) (audioMeasurement, error) {
			return measureASRBuffered(ctx, up, audioMeasureTarget(), benchaudio.WAV(), benchaudio.Language)
		},
		"measureASRLive": func(up *measureUpstream) (audioMeasurement, error) {
			return measureASRLive(ctx, up, audioMeasureTarget(), pcm, sampleRate, benchaudio.Language)
		},
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			up := &measureUpstream{err: dialErr}
			got, err := call(up)
			if !errors.Is(err, dialErr) || up.calls != 1 || got != (audioMeasurement{}) {
				t.Fatalf("%s = %+v, %v after %d call(s); want the transport error after one call", name, got, err, up.calls)
			}
			var merr *audioMeasureError
			if !errors.As(err, &merr) || merr.Kind != audioMeasureTransport || merr.Status != 0 {
				t.Fatalf("%s error %v (%#v) is not a transport audioMeasureError", name, err, merr)
			}
		})
	}
}

// TestMeasureAudioEntryPointsReadTheRealClock: the production entry points time with the
// real clock. Their answer starts 20 ms after the request and each chunk of it arrives at
// least 20 ms after the one before, so every time they report is at least the waits that
// elapsed before it -- a measurement on a frozen clock would have no measurable time at all,
// and one on any clock but the real one would not see the waits.
func TestMeasureAudioEntryPointsReadTheRealClock(t *testing.T) {
	const gap = 20 * time.Millisecond
	pcm, sampleRate, _ := benchaudio.PCM()
	wav := buildPCMWav(t, 24000, 1, 16, 48000)
	ctx := context.Background()
	cases := []struct {
		name        string
		contentType string
		chunks      []string
		measure     func(*measureUpstream) (audioMeasurement, error)
		// the least time each figure can be: the waits before the answer, the first output and
		// the end
		firstByte, ttft, wall time.Duration
	}{
		{
			"measureTTSStream", "text/event-stream",
			[]string{sseSpeechDelta(48000), sseSpeechDone},
			func(up *measureUpstream) (audioMeasurement, error) {
				return measureTTSStream(ctx, up, audioMeasureTarget(), audioBenchmarkPassage, "", 48000)
			}, gap, 2 * gap, 3 * gap,
		},
		{
			"measureTTSBuffered", "audio/wav",
			[]string{string(wav[:44]), string(wav[44:])},
			func(up *measureUpstream) (audioMeasurement, error) {
				return measureTTSBuffered(ctx, up, audioMeasureTarget(), audioBenchmarkPassage, "")
			}, gap, 3 * gap, 3 * gap,
		},
		{
			"measureASRBuffered", "application/json",
			[]string{`{"text":"der schutz"}`},
			func(up *measureUpstream) (audioMeasurement, error) {
				return measureASRBuffered(ctx, up, audioMeasureTarget(), benchaudio.WAV(), benchaudio.Language)
			}, gap, 2 * gap, 2 * gap,
		},
		{
			"measureASRLive", "text/event-stream",
			[]string{sseTranscriptDelta("der"), sseTranscriptDone("der")},
			func(up *measureUpstream) (audioMeasurement, error) {
				return measureASRLive(ctx, up, audioMeasureTarget(), pcm, sampleRate, benchaudio.Language)
			}, gap, 2 * gap, 3 * gap,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := &measureBody{gap: gap}
			for _, c := range tc.chunks {
				body.chunks = append(body.chunks, measureChunk{data: c})
			}
			up := &measureUpstream{status: http.StatusOK, header: http.Header{"Content-Type": {tc.contentType}}, body: body, answerAt: gap}
			got, err := tc.measure(up)
			if err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			if got.FirstByteMS < tc.firstByte.Milliseconds() || got.TTFTMS < tc.ttft.Milliseconds() || got.WallMS < tc.wall.Milliseconds() ||
				got.FirstByteMS > got.TTFTMS || got.TTFTMS > got.WallMS {
				t.Fatalf("%s = %+v; want first byte >= %v, first output >= %v, end >= %v, in that order", tc.name, got, tc.firstByte, tc.ttft, tc.wall)
			}
		})
	}
}

// TestMeasureTTSPassageLength: the passage stays about 55 words of plain prose, roughly
// 20 s of speech, so the streamed rate is measured over many chunks and results stay
// comparable from run to run.
func TestMeasureTTSPassageLength(t *testing.T) {
	words := strings.Fields(audioBenchmarkPassage)
	if len(words) < 50 || len(words) > 60 {
		t.Fatalf("the passage has %d words, want about 55 (50 to 60)", len(words))
	}
	if strings.ContainsAny(audioBenchmarkPassage, "0123456789\n\"") {
		t.Fatalf("the passage %q holds digits, a line break or quotes; want plain prose", audioBenchmarkPassage)
	}
}
