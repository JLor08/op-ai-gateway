// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"op-ai-gateway/internal/apierror"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/provider"
	"op-ai-gateway/internal/routing"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// The speech benchmark (mode=speech, #263) synthesizes one short clip per speech
// mapping, reads the output byte rate from the answer's WAV `fmt` chunk, caches it
// under the mapping id (the id the streamed relay bills under) and records an
// audio-perf history row. These tests drive it end to end through the portal
// endpoint, against an audio.cpp-shaped stub that answers a real WAV.

const (
	sbServer = "srv_speech"
	sbApp    = "sb_app"
	// sbTTSModel / sbCloneModel / sbUnknownModel are the model ids audio.cpp serves
	// for the fixture's mappings; they differ from the gateway names so the body's
	// model rewrite is observable at the upstream.
	sbTTSModel     = "tts_model"
	sbCloneModel   = "clone_model"
	sbUnknownModel = "unknown_model"
	sbASRModel     = "asr_model"
	// sbTTFBDelay is how long the stub holds back its response headers: a
	// non-stream synthesis answers only once the audio is done, so the time to the
	// first byte is the synthesis time, and the test needs it to be measurably > 0.
	sbTTFBDelay = 30 * time.Millisecond
)

// speechBenchCall is one request the stub received.
type speechBenchCall struct {
	method string
	path   string
	raw    []byte
	body   map[string]any
}

// speechBenchUpstream is an audio.cpp-shaped stub: it records every request and
// answers through answer, which by default is a 1 s WAV (24 kHz mono 16-bit) with
// the metering headers audio.cpp sets.
type speechBenchUpstream struct {
	*httptest.Server
	mu     sync.Mutex
	calls  []speechBenchCall
	answer func(w http.ResponseWriter, model string)
}

func newSpeechBenchUpstream(t *testing.T, answer func(w http.ResponseWriter, model string)) *speechBenchUpstream {
	t.Helper()
	up := &speechBenchUpstream{answer: answer}
	up.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		call := speechBenchCall{method: r.Method, path: r.URL.Path, raw: raw}
		_ = json.Unmarshal(raw, &call.body)
		up.mu.Lock()
		up.calls = append(up.calls, call)
		up.mu.Unlock()
		model, _ := call.body["model"].(string)
		time.Sleep(sbTTFBDelay)
		up.answer(w, model)
	}))
	t.Cleanup(up.Close)
	return up
}

func (u *speechBenchUpstream) received() []speechBenchCall {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]speechBenchCall(nil), u.calls...)
}

// modelsCalled lists the model ids the stub was asked to synthesize with.
func (u *speechBenchUpstream) modelsCalled() []string {
	var out []string
	for _, c := range u.received() {
		m, _ := c.body["model"].(string)
		out = append(out, m)
	}
	return out
}

// answerWAV writes a 200 audio/wav answer of the given WAV bytes, with the
// metering headers audio.cpp sets when withMetering.
func answerWAV(wav []byte, withMetering bool) func(w http.ResponseWriter, model string) {
	return func(w http.ResponseWriter, _ string) {
		w.Header().Set("Content-Type", "audio/wav")
		if withMetering {
			w.Header().Set(speechDurationHeader, "1000.000000")
			w.Header().Set("X-AudioCPP-Wall-Ms", "400")
			w.Header().Set("X-AudioCPP-RTF", "0.4")
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(wav)))
		_, _ = w.Write(wav)
	}
}

// newSpeechBenchFixture is the shared benchmark fixture (a server owned by usr_a,
// whose token the portal calls carry) plus one audio.cpp server pointing at the stub,
// with an audio-only application and four mappings, one per verdict state the
// benchmark tells apart:
//
//   - sb_tts: (speech, yes), (speech_clone, no), (speech_streaming, yes) -- the target;
//   - sb_unknown: (speech, yes) and no clone row at all, which reads as "unknown",
//     never "needs a voice" -- still a target;
//   - sb_clone: (speech, yes), (speech_clone, yes) -- skipped, it cannot synthesize
//     without a reference voice;
//   - sb_asr: (speech, no), a transcriber -- skipped, it does not synthesize.
//
// The Provider is a REAL OpenAI-compatible client making a genuine HTTP round trip.
func newSpeechBenchFixture(t *testing.T, up *speechBenchUpstream) *Server {
	t.Helper()
	s := newBenchmarkActiveFixture(t)
	s.Provider = provider.NewOpenAICompatibleClient(http.DefaultClient)
	ctx := context.Background()
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	u, err := url.Parse(up.URL)
	if err != nil {
		t.Fatalf("url.Parse(%q): %v", up.URL, err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatalf("upstream port %q: %v", u.Port(), err)
	}
	must := func(what string, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}
	must("CreateAIServer", s.Routes.CreateAIServer(ctx, routing.AIServer{ID: sbServer, Name: "Speech Host", Domain: u.Hostname(), Provider: routing.ProviderAudioCpp, Endpoint: up.URL, Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now}))
	must("SetServerOwners", s.Routes.SetServerOwners(ctx, sbServer, []string{"usr_a"}))
	must("CreateApplication", s.Routes.CreateApplication(ctx, routing.Application{ID: sbApp, ServerID: sbServer, Type: routing.ProviderAudioCpp, Port: port, Scheme: u.Scheme, APIFlavors: []string{routing.APIFlavorOpenAIAudio}, Priority: 10, Weight: 50, TimeoutMS: 30000, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}))
	for _, m := range []struct {
		id, gateway, upstream string
		caps                  []routing.CapabilityRow
	}{
		{"sb_tts", "tts-bench", sbTTSModel, []routing.CapabilityRow{
			sbCap(routing.CapabilitySpeech, routing.CapabilityYes, now),
			sbCap(routing.CapabilitySpeechClone, routing.CapabilityNo, now),
			sbCap(routing.CapabilitySpeechStreaming, routing.CapabilityYes, now),
		}},
		{"sb_unknown", "unknown-bench", sbUnknownModel, []routing.CapabilityRow{sbCap(routing.CapabilitySpeech, routing.CapabilityYes, now)}},
		{"sb_clone", "clone-bench", sbCloneModel, []routing.CapabilityRow{
			sbCap(routing.CapabilitySpeech, routing.CapabilityYes, now),
			sbCap(routing.CapabilitySpeechClone, routing.CapabilityYes, now),
		}},
		{"sb_asr", "asr-bench", sbASRModel, []routing.CapabilityRow{
			sbCap(routing.CapabilitySpeech, routing.CapabilityNo, now),
			sbCap(routing.CapabilityTranscription, routing.CapabilityYes, now),
		}},
	} {
		must("CreateMapping "+m.id, s.Routes.CreateMapping(ctx, routing.ModelMapping{ID: m.id, ApplicationID: sbApp, GatewayModelName: m.gateway, AppModelName: m.upstream, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}))
		must("UpsertMappingCapabilities "+m.id, s.Routes.UpsertMappingCapabilities(ctx, m.id, m.caps))
	}
	return s
}

func sbCap(capability, verdict string, at time.Time) routing.CapabilityRow {
	return routing.CapabilityRow{Capability: capability, Verdict: verdict, Source: routing.CapabilitySourceAudioCppModels, CheckedAt: at}
}

// postSpeechBenchmark sends an owner-authorized POST and returns the status, the
// error code (empty on success) and the raw body.
func postSpeechBenchmark(t *testing.T, s *Server, path string) (int, string, []byte) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, nil)
	req.Header.Set("Authorization", "Bearer "+baOwnerSecret)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	var errBody apierror.Body
	_ = json.Unmarshal(rec.Body.Bytes(), &errBody)
	return rec.Code, errBody.Error.Code, rec.Body.Bytes()
}

// finishedSpeechRun waits for the server's run to end and returns its status.
func finishedSpeechRun(t *testing.T, s *Server) BenchmarkStatus {
	t.Helper()
	waitFor(t, func() bool { return !s.Benchmarks.Status(sbServer).Running })
	return s.Benchmarks.Status(sbServer)
}

func speechBenchResult(t *testing.T, st BenchmarkStatus, mappingID string) BenchmarkResult {
	t.Helper()
	for _, r := range st.Results {
		if r.MappingID == mappingID {
			return r
		}
	}
	t.Fatalf("no result for %s in %+v", mappingID, st.Results)
	return BenchmarkResult{}
}

func speechBenchRuns(t *testing.T, s *Server, mappingID string) []routing.BenchmarkRun {
	t.Helper()
	runs, err := s.Routes.BenchmarkRunsByMapping(context.Background(), mappingID, 10)
	if err != nil {
		t.Fatalf("BenchmarkRunsByMapping(%s): %v", mappingID, err)
	}
	return runs
}

func speechBenchRateRows(t *testing.T, s *Server, mappingID string) []routing.ModelMappingCacheRow {
	t.Helper()
	rows, err := s.Routes.ModelMappingCache(context.Background(), mappingID, routing.ModelMappingCacheOutputByteRate)
	if err != nil {
		t.Fatalf("ModelMappingCache(%s): %v", mappingID, err)
	}
	return rows
}

// The speech benchmark reads the byte rate off the answer's WAV `fmt` chunk -- a
// mono 44.1 kHz model gets 88200, a mono 24 kHz one 48000, never a constant --
// caches it under the MAPPING id, which is the id the streamed relay reads it by
// (cachedOutputByteRate), and appends one kind=speech history row carrying the
// audio perf metrics. With audio.cpp's metering headers present they are the wall
// time and real-time factor; without them the benchmark measures its own wall time
// and derives the factor from the WAV's duration. The time to first byte is always
// its own measurement. The request is the plain non-stream shape: model (mapped to
// the application's own id) and input, nothing else.
func TestSpeechBenchmarkCachesByteRate(t *testing.T) {
	for _, tc := range []struct {
		name         string
		sampleRate   uint32
		channels     uint16
		metering     bool
		wantRate     string
		wantRateNum  uint32
		wantWallMS   int64 // 0: measured by the benchmark, asserted as a range
		wantRTF      float64
		wantDuration float64 // seconds, the WAV's own length
	}{
		{"mono 24 kHz with audio.cpp's metering headers", 24000, 1, true, "48000", 48000, 400, 0.4, 1},
		{"mono 44.1 kHz without metering headers", 44100, 1, false, "88200", 88200, 0, 0, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dataBytes := int(tc.sampleRate) * int(tc.channels) * 2 // one second of 16-bit PCM
			up := newSpeechBenchUpstream(t, answerWAV(buildPCMWav(t, tc.sampleRate, tc.channels, 16, dataBytes), tc.metering))
			s := newSpeechBenchFixture(t, up)

			status, code, body := postSpeechBenchmark(t, s, "/api/portal/mappings/sb_tts/benchmark?mode=speech")
			if status != http.StatusAccepted {
				t.Fatalf("POST = %d %q (%s), want 202", status, code, body)
			}
			st := finishedSpeechRun(t, s)
			if st.Mode != "speech" || st.Error != "" || st.Total != 1 || st.Done != 1 {
				t.Fatalf("run = mode %q err %q total %d done %d, want a finished speech run of one mapping", st.Mode, st.Error, st.Total, st.Done)
			}
			res := speechBenchResult(t, st, "sb_tts")
			if res.Error != "" || res.Skipped != "" || res.OutputByteRate != tc.wantRateNum {
				t.Fatalf("result = %+v, want a measured result with output byte rate %d", res, tc.wantRateNum)
			}

			// The rate is cached under the mapping id, scope "", as a decimal integer ...
			rows := speechBenchRateRows(t, s, "sb_tts")
			if len(rows) != 1 || rows[0].ScopeKey != "" || rows[0].Payload != tc.wantRate || rows[0].UpdatedAt.IsZero() {
				t.Fatalf("output_byte_rate cache rows = %+v, want one scope-\"\" row with payload %s", rows, tc.wantRate)
			}
			// ... and the streamed relay's own reader finds it there: writer and reader agree on the id.
			if rate, ok := s.cachedOutputByteRate(context.Background(), "sb_tts"); !ok || rate != tc.wantRateNum {
				t.Fatalf("cachedOutputByteRate(sb_tts) = (%d, %v), want (%d, true)", rate, ok, tc.wantRateNum)
			}
			// A mapping that was not benchmarked caches nothing.
			if other := speechBenchRateRows(t, s, "sb_unknown"); len(other) != 0 {
				t.Fatalf("an unbenchmarked mapping gained a cached rate: %+v", other)
			}

			// One kind=speech history row with the perf metrics, and no speed row.
			runs := speechBenchRuns(t, s, "sb_tts")
			if len(runs) != 1 {
				t.Fatalf("history rows = %+v, want exactly one", runs)
			}
			run := runs[0]
			if run.Kind != "speech" || run.Error != "" || run.ServerID != sbServer {
				t.Fatalf("history row = %+v, want a successful kind=speech row on %s", run, sbServer)
			}
			if run.GenTokensPerSecond != 0 || run.PromptTokensPerSecond != 0 || run.LoadTimeMS != 0 {
				t.Fatalf("a speech row carries token metrics: %+v", run)
			}
			if run.TTFTMS < int64(sbTTFBDelay/time.Millisecond)-5 {
				t.Fatalf("TTFTMS = %d, want >= the stub's %s hold-back (the benchmark's own time to the first byte)", run.TTFTMS, sbTTFBDelay)
			}
			if tc.metering {
				if run.WallMS != tc.wantWallMS || run.RTF != tc.wantRTF {
					t.Fatalf("wall/rtf = %d/%v, want audio.cpp's own %d/%v", run.WallMS, run.RTF, tc.wantWallMS, tc.wantRTF)
				}
			} else {
				if run.WallMS < int64(sbTTFBDelay/time.Millisecond)-5 {
					t.Fatalf("WallMS = %d, want the benchmark's own measurement (>= the stub's hold-back)", run.WallMS)
				}
				if want := float64(run.WallMS) / 1000 / tc.wantDuration; run.RTF <= 0 || math.Abs(run.RTF-want) > 0.002 {
					t.Fatalf("RTF = %v, want wall/duration = %v (the WAV is %.0f s long)", run.RTF, want, tc.wantDuration)
				}
			}
			// The poll result carries the same figures.
			if res.TTFTMS != run.TTFTMS || res.WallMS != run.WallMS || res.RTF != run.RTF {
				t.Fatalf("result perf = %d/%d/%v, history row = %d/%d/%v, want identical", res.TTFTMS, res.WallMS, res.RTF, run.TTFTMS, run.WallMS, run.RTF)
			}

			// The request: one plain non-stream POST, model mapped to the application's own id.
			calls := up.received()
			if len(calls) != 1 || calls[0].method != http.MethodPost || calls[0].path != "/v1/audio/speech" {
				t.Fatalf("upstream calls = %+v, want exactly one POST /v1/audio/speech", calls)
			}
			c := calls[0]
			if c.body["model"] != sbTTSModel {
				t.Fatalf("upstream model = %v, want the application's own id %q", c.body["model"], sbTTSModel)
			}
			if in, _ := c.body["input"].(string); in == "" {
				t.Fatalf("upstream input is empty: %s", c.raw)
			}
			if len(c.body) != 2 {
				t.Fatalf("upstream body = %s, want exactly model and input (a plain non-stream synthesis)", c.raw)
			}
		})
	}
}

// A synthesis that does not yield a readable WAV writes NO rate -- the mapping stays
// on the honest audio_bytes fallback, never a guessed rate -- leaves a rate already
// cached untouched (a failed re-benchmark is no reason to forget a real observation),
// and records a failed kind=speech row with its error and no metrics.
func TestSpeechBenchmarkWritesNoRateWhenTheSynthesisFails(t *testing.T) {
	good := buildPCMWav(t, 24000, 1, 16, 48000)
	// A header that contradicts itself: the byte rate is 100x what 24 kHz mono 16-bit gives.
	lying := wavFile(t, wavFmtChunkWith(t, 1, 24000, 4_800_000, 2, 16), wavDataChunk(t, 48000))
	for _, tc := range []struct {
		name   string
		answer func(w http.ResponseWriter, model string)
	}{
		{"an upstream 500 with an OpenAI error body", func(w http.ResponseWriter, _ string) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":{"message":"model not loaded","type":"server_error"}}`))
		}},
		{"a 200 whose body is no WAV", func(w http.ResponseWriter, _ string) {
			w.Header().Set("Content-Type", "audio/wav")
			_, _ = w.Write([]byte("this is not riff"))
		}},
		{"a WAV whose fmt chunk contradicts itself", answerWAV(lying, true)},
		{"an empty 200", func(w http.ResponseWriter, _ string) {
			w.Header().Set("Content-Type", "audio/wav")
		}},
		{"a WAV labelled as JSON", func(w http.ResponseWriter, _ string) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(good)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := newSpeechBenchUpstream(t, tc.answer)
			s := newSpeechBenchFixture(t, up)
			ctx := context.Background()
			// A rate observed earlier must survive the failed run.
			if err := s.Routes.UpsertModelMappingCache(ctx, routing.ModelMappingCacheRow{MappingID: "sb_tts", CacheType: routing.ModelMappingCacheOutputByteRate, Payload: "88200", UpdatedAt: time.Now().UTC()}); err != nil {
				t.Fatalf("seed rate: %v", err)
			}
			if status, code, body := postSpeechBenchmark(t, s, "/api/portal/mappings/sb_tts/benchmark?mode=speech"); status != http.StatusAccepted {
				t.Fatalf("POST = %d %q (%s), want 202", status, code, body)
			}
			st := finishedSpeechRun(t, s)
			res := speechBenchResult(t, st, "sb_tts")
			if res.Error == "" || res.OutputByteRate != 0 || res.TTFTMS != 0 || res.WallMS != 0 || res.RTF != 0 {
				t.Fatalf("result = %+v, want an error and no figures", res)
			}
			rows := speechBenchRateRows(t, s, "sb_tts")
			if len(rows) != 1 || rows[0].Payload != "88200" {
				t.Fatalf("cache rows = %+v, want the earlier 88200 untouched", rows)
			}
			runs := speechBenchRuns(t, s, "sb_tts")
			if len(runs) != 1 || runs[0].Kind != "speech" || runs[0].Error == "" || runs[0].TTFTMS != 0 || runs[0].WallMS != 0 || runs[0].RTF != 0 {
				t.Fatalf("history = %+v, want one failed kind=speech row with no metrics", runs)
			}
			if got := len(up.received()); got != 1 {
				t.Fatalf("upstream calls = %d, want exactly one (a failed synthesis is not retried)", got)
			}
		})
	}
}

// The rate is cached only from a WAV the portal can play and store on the assumption it
// makes about a rate (ADR-068): mono, 16-bit, integer PCM, so the sample rate is byteRate/2.
// A byte rate alone cannot say that -- 96000 B/s is 48 kHz mono s16 and equally 24 kHz stereo
// s16 or 24 kHz mono float32 -- so a model whose WAV is anything else writes NO rate (and no
// identity row): its streamed calls bill audio_bytes and the portal's gate stays closed
// (buffered), instead of the audio being played, and its stored WAV labelled, at the wrong
// speed. The run's result and history row say why.
func TestSpeechBenchmarkCachesNoRateForAWAVThatIsNotMono16BitPCM(t *testing.T) {
	// 24 kHz float32 mono and 24 kHz s16 stereo are BOTH 96000 B/s: the same number as
	// 48 kHz mono s16, which is the only one of the three the portal's arithmetic reads right.
	float32Mono := wavFile(t, wavFmtChunkTagged(t, 3, 1, 24000, 32), wavDataChunk(t, 96000))
	extensibleMono16 := wavFile(t, wavFmtChunkTagged(t, 0xFFFE, 1, 24000, 16), wavDataChunk(t, 48000))
	for _, tc := range []struct {
		name string
		wav  []byte
	}{
		{"stereo 16-bit", buildPCMWav(t, 24000, 2, 16, 96000)},
		{"mono 24-bit", buildPCMWav(t, 32000, 1, 24, 96000)},
		{"mono float32", float32Mono},
		{"mono 16-bit with a WAVE_FORMAT_EXTENSIBLE tag", extensibleMono16},
		{"mono 8-bit", buildPCMWav(t, 24000, 1, 8, 24000)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := newSpeechBenchUpstream(t, answerWAV(tc.wav, true))
			s := newSpeechBenchFixture(t, up)
			ctx := context.Background()

			if status, code, body := postSpeechBenchmark(t, s, "/api/portal/mappings/sb_tts/benchmark?mode=speech"); status != http.StatusAccepted {
				t.Fatalf("POST = %d %q (%s), want 202", status, code, body)
			}
			res := speechBenchResult(t, finishedSpeechRun(t, s), "sb_tts")
			if res.OutputByteRate != 0 || res.TTFTMS != 0 || res.WallMS != 0 || res.RTF != 0 {
				t.Fatalf("result = %+v, want no rate and no figures", res)
			}
			if !strings.Contains(res.Error, "not mono 16-bit PCM") {
				t.Fatalf("result error = %q, want it to say the model's audio is not mono 16-bit PCM", res.Error)
			}
			if rows := speechBenchRateRows(t, s, "sb_tts"); len(rows) != 0 {
				t.Fatalf("output_byte_rate rows = %+v, want none", rows)
			}
			identity, err := s.Routes.ModelMappingCache(ctx, "sb_tts", routing.ModelMappingCacheOutputByteRateModel)
			if err != nil || len(identity) != 0 {
				t.Fatalf("identity rows = %+v (%v), want none", identity, err)
			}
			// The mapping bills audio_bytes: the streamed relay's reader finds no rate ...
			if rate, ok := s.cachedOutputByteRate(ctx, "sb_tts"); ok {
				t.Fatalf("cachedOutputByteRate = (%d, true), want no rate", rate)
			}
			// ... and the portal's gate stays closed: the run would be the buffered one.
			if rate, open, err := s.portalSpeechStreamRate(ctx, auth.Token{UserID: "usr_a"}, "tts-bench"); err != nil || open {
				t.Fatalf("portal gate = (%d, %v, %v), want closed", rate, open, err)
			}
			// One failed kind=speech history row carrying the reason.
			runs := speechBenchRuns(t, s, "sb_tts")
			if len(runs) != 1 || runs[0].Kind != "speech" || runs[0].Error != res.Error || runs[0].TTFTMS != 0 {
				t.Fatalf("history = %+v, want one failed kind=speech row with the same reason", runs)
			}
		})
	}
}

// The other half of the guarantee: where a mono 16-bit WAV IS measured the rate is cached and
// the portal's gate opens at byteRate/2 -- the benchmark and the gate agree on the format.
func TestSpeechBenchmarkMono16BitRateOpensThePortalGate(t *testing.T) {
	up := newSpeechBenchUpstream(t, answerWAV(buildPCMWav(t, 24000, 1, 16, 48000), true))
	s := newSpeechBenchFixture(t, up)
	if status, code, body := postSpeechBenchmark(t, s, "/api/portal/mappings/sb_tts/benchmark?mode=speech"); status != http.StatusAccepted {
		t.Fatalf("POST = %d %q (%s), want 202", status, code, body)
	}
	if res := speechBenchResult(t, finishedSpeechRun(t, s), "sb_tts"); res.Error != "" || res.OutputByteRate != 48000 {
		t.Fatalf("result = %+v, want a measured 48000 B/s", res)
	}
	rate, open, err := s.portalSpeechStreamRate(context.Background(), auth.Token{UserID: "usr_a"}, "tts-bench")
	if err != nil || !open || rate != 24000 {
		t.Fatalf("portal gate = (%d, %v, %v), want open at 24000 Hz", rate, open, err)
	}
}

// A rate cached earlier (by a model that was swapped for this one behind the same mapping) no
// longer describes the audio, and would keep the portal's gate open on PCM it cannot play: a
// non-mono-16-bit answer is an observation, not a failed measurement, so it retires the stale
// rate the way the health loop does (an empty payload every reader takes as no rate). A mapping
// that has no rate gains no row for it.
func TestSpeechBenchmarkRetiresAStaleRateWhenTheModelIsNotMono16BitPCM(t *testing.T) {
	up := newSpeechBenchUpstream(t, answerWAV(buildPCMWav(t, 24000, 2, 16, 96000), true))
	s := newSpeechBenchFixture(t, up)
	ctx := context.Background()
	if err := s.Routes.UpsertModelMappingCache(ctx, routing.ModelMappingCacheRow{MappingID: "sb_tts", CacheType: routing.ModelMappingCacheOutputByteRate, Payload: "48000", UpdatedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("seed rate: %v", err)
	}
	if status, code, body := postSpeechBenchmark(t, s, "/api/portal/mappings/sb_tts/benchmark?mode=speech"); status != http.StatusAccepted {
		t.Fatalf("POST = %d %q (%s), want 202", status, code, body)
	}
	finishedSpeechRun(t, s)
	if rate, ok := s.cachedOutputByteRate(ctx, "sb_tts"); ok {
		t.Fatalf("cachedOutputByteRate = (%d, true), want the stale rate retired", rate)
	}
	if _, open, _ := s.portalSpeechStreamRate(ctx, auth.Token{UserID: "usr_a"}, "tts-bench"); open {
		t.Fatal("the portal gate stayed open on a retired rate")
	}
}

// With nothing cached, a failed synthesis leaves the mapping with no cached rate.
func TestSpeechBenchmarkFailureCachesNothing(t *testing.T) {
	up := newSpeechBenchUpstream(t, func(w http.ResponseWriter, _ string) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"boom"}}`))
	})
	s := newSpeechBenchFixture(t, up)
	if status, code, body := postSpeechBenchmark(t, s, "/api/portal/mappings/sb_tts/benchmark?mode=speech"); status != http.StatusAccepted {
		t.Fatalf("POST = %d %q (%s), want 202", status, code, body)
	}
	st := finishedSpeechRun(t, s)
	if res := speechBenchResult(t, st, "sb_tts"); res.Error == "" {
		t.Fatalf("result = %+v, want an error", res)
	}
	if rows := speechBenchRateRows(t, s, "sb_tts"); len(rows) != 0 {
		t.Fatalf("cache rows = %+v, want none after a failed synthesis", rows)
	}
	if rate, ok := s.cachedOutputByteRate(context.Background(), "sb_tts"); ok {
		t.Fatalf("cachedOutputByteRate = (%d, true), want no rate (the mapping bills audio_bytes)", rate)
	}
}

// The audio-only refusal is lifted for the speech benchmark ONLY: a speech-capable
// mapping that serves nothing but openai_audio is accepted and measured under
// ?mode=speech, while every chat-prompt mode still refuses it with 409
// benchmark.audio_only and sends it nothing. A mapping that does not synthesize
// speech (a transcriber) is refused by the speech benchmark with its own 409, and
// neither refusal reserves the server.
func TestSpeechBenchmarkRunsOnSpeechMapping(t *testing.T) {
	up := newSpeechBenchUpstream(t, answerWAV(buildPCMWav(t, 24000, 1, 16, 48000), true))
	s := newSpeechBenchFixture(t, up)

	// The chat/speed family still refuses an audio-only mapping, before reserving anything.
	for _, mode := range []string{"speed", "both", "capacity", "vision", ""} {
		path := "/api/portal/mappings/sb_tts/benchmark"
		if mode != "" {
			path += "?mode=" + mode
		}
		if status, code, _ := postSpeechBenchmark(t, s, path); status != http.StatusConflict || code != codeBenchmarkAudioOnly {
			t.Fatalf("POST %s = %d %q, want 409 %s (the chat benchmark must still refuse audio)", path, status, code, codeBenchmarkAudioOnly)
		}
	}
	if s.Benchmarks.ServerBusy(sbServer) {
		t.Fatal("a refused chat benchmark left the server reserved")
	}
	if got := up.received(); len(got) != 0 {
		t.Fatalf("a refused chat benchmark reached the upstream: %+v", got)
	}

	// A mapping that does not synthesize speech is refused by the speech benchmark.
	if status, code, _ := postSpeechBenchmark(t, s, "/api/portal/mappings/sb_asr/benchmark?mode=speech"); status != http.StatusConflict || code != codeBenchmarkSpeechUnavailable {
		t.Fatalf("speech benchmark of a transcriber = %d %q, want 409 %s", status, code, codeBenchmarkSpeechUnavailable)
	}
	if s.Benchmarks.ServerBusy(sbServer) {
		t.Fatal("a refused speech benchmark left the server reserved")
	}

	// The same audio-only mapping runs under mode=speech.
	status, code, body := postSpeechBenchmark(t, s, "/api/portal/mappings/sb_tts/benchmark?mode=speech")
	if status != http.StatusAccepted {
		t.Fatalf("speech benchmark of an audio-only speech mapping = %d %q (%s), want 202", status, code, body)
	}
	st := finishedSpeechRun(t, s)
	res := speechBenchResult(t, st, "sb_tts")
	if res.Skipped != "" || res.Error != "" || res.OutputByteRate != 48000 {
		t.Fatalf("result = %+v, want a measured, unskipped result with rate 48000", res)
	}
	if got := up.modelsCalled(); len(got) != 1 || got[0] != sbTTSModel {
		t.Fatalf("upstream models = %v, want exactly [%s]", got, sbTTSModel)
	}
}

// A voice-cloning mapping (speech_clone: yes) synthesizes only in a reference voice
// (a server-side reference WAV and its text), so a plain model-and-input synthesis
// would 500: the speech benchmark SKIPS it -- no request, no cache row, no history
// row -- and it stays on the audio_bytes fallback. Skipping is by the verdict, so a
// mapping whose clone verdict is merely unknown (no row) is still measured, and so is
// an explicit "no". A mapping-scope request that has nothing to run is a 409.
func TestSpeechBenchmarkSkipsCloneModel(t *testing.T) {
	up := newSpeechBenchUpstream(t, func(w http.ResponseWriter, model string) {
		if model == sbCloneModel {
			// What audio.cpp answers a clone model that is sent no reference.
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":{"message":"reference_text is required"}}`))
			return
		}
		answerWAV(buildPCMWav(t, 24000, 1, 16, 48000), true)(w, model)
	})
	s := newSpeechBenchFixture(t, up)

	// Mapping scope, the clone model alone: nothing runnable, so 409 and nothing reserved.
	if status, code, _ := postSpeechBenchmark(t, s, "/api/portal/mappings/sb_clone/benchmark?mode=speech"); status != http.StatusConflict || code != codeBenchmarkSpeechUnavailable {
		t.Fatalf("speech benchmark of a clone mapping = %d %q, want 409 %s", status, code, codeBenchmarkSpeechUnavailable)
	}
	if s.Benchmarks.ServerBusy(sbServer) {
		t.Fatal("a refused speech benchmark left the server reserved")
	}

	// Application scope: the targets are measured, the clone and the transcriber skipped.
	status, code, body := postSpeechBenchmark(t, s, "/api/portal/applications/"+sbApp+"/benchmark?mode=speech")
	if status != http.StatusAccepted {
		t.Fatalf("application-scope speech benchmark = %d %q (%s), want 202", status, code, body)
	}
	st := finishedSpeechRun(t, s)
	if st.Total != 4 || st.Done != 4 {
		t.Fatalf("run total/done = %d/%d, want 4/4 (every view counts, skipped ones included)", st.Total, st.Done)
	}
	for id, want := range map[string]string{"sb_clone": benchmarkSkippedSpeechClone, "sb_asr": benchmarkSkippedNotSpeech} {
		res := speechBenchResult(t, st, id)
		if res.Skipped != want || res.Error != "" || res.OutputByteRate != 0 {
			t.Fatalf("%s result = %+v, want Skipped %q and nothing else", id, res, want)
		}
		if rows := speechBenchRateRows(t, s, id); len(rows) != 0 {
			t.Fatalf("%s gained a cached rate: %+v", id, rows)
		}
		if runs := speechBenchRuns(t, s, id); len(runs) != 0 {
			t.Fatalf("%s gained a history row: %+v", id, runs)
		}
	}
	for _, id := range []string{"sb_tts", "sb_unknown"} {
		res := speechBenchResult(t, st, id)
		if res.Skipped != "" || res.Error != "" || res.OutputByteRate != 48000 {
			t.Fatalf("%s result = %+v, want a measured result with rate 48000", id, res)
		}
		if rows := speechBenchRateRows(t, s, id); len(rows) != 1 || rows[0].Payload != "48000" {
			t.Fatalf("%s cache rows = %+v, want one 48000", id, rows)
		}
		if runs := speechBenchRuns(t, s, id); len(runs) != 1 || runs[0].Kind != "speech" {
			t.Fatalf("%s history = %+v, want one kind=speech row", id, runs)
		}
	}
	// The clone model and the transcriber were never sent a synthesis request.
	called := up.modelsCalled()
	if len(called) != 2 {
		t.Fatalf("upstream models = %v, want exactly the two targets", called)
	}
	for _, m := range called {
		if m != sbTTSModel && m != sbUnknownModel {
			t.Fatalf("upstream was asked to synthesize with %q, want only %s and %s", m, sbTTSModel, sbUnknownModel)
		}
	}
}

// The speech mode is a real mode of the starter's ?mode= parameter, and the speech
// benchmark's refusal and skip values are the closed strings the portal switches on.
func TestSpeechBenchmarkModeAndRefusalAreStable(t *testing.T) {
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/api/portal/mappings/x/benchmark?mode=speech", nil)
	if got, ok := parseBenchmarkMode(rec, r); !ok || got != "speech" {
		t.Fatalf("parseBenchmarkMode(speech) = (%q, %v), want (speech, true)", got, ok)
	}
	if benchmarkSkippedNotSpeech != "not_speech" || benchmarkSkippedSpeechClone != "speech_clone" {
		t.Fatalf("skip values = %q/%q, want not_speech/speech_clone", benchmarkSkippedNotSpeech, benchmarkSkippedSpeechClone)
	}
	rec = httptest.NewRecorder()
	writeBenchmarkError(rec, errBenchmarkSpeechUnavailable)
	var body apierror.Body
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if rec.Code != http.StatusConflict || body.Error.Code != "benchmark.speech_unavailable" || body.Error.Message != msgBenchmarkSpeechUnavailable {
		t.Fatalf("writeBenchmarkError(errBenchmarkSpeechUnavailable) = %d %q %q, want 409 benchmark.speech_unavailable", rec.Code, body.Error.Code, body.Error.Message)
	}
}

// ---- a configured clone model with presets (#306) ----
//
// A clone model that lists preset voices (speech_clone yes + a non-empty voices cache) can be
// synthesized by naming one: audio.cpp answers {model, input, voice:"<preset>"} with the
// preset's reference cloned, while a voiceless request 500s. The speech benchmark therefore
// runs for such a model -- the preset voice picked ONCE, in the partition, and carried on the
// target -- so its output byte rate is learned and the portal streams it. A clone model with no
// presets (a reference-only one) stays skipped, exactly as before.

const (
	sbPresetModel = "preset_model"
	// sbPresetGateway is the gateway model name of the fixture's preset clone mapping.
	sbPresetGateway = "preset-bench"
)

// addSpeechBenchPresetClone adds one audio-only mapping to the speech fixture that is a streaming
// clone model whose voices cache holds voicesPayload (nothing is cached when it is empty). It is
// added by the test that needs it, not by newSpeechBenchFixture, so the fixture's four-mapping
// totals the older tests assert stay as they are.
func addSpeechBenchPresetClone(t *testing.T, s *Server, voicesPayload string) {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	if err := s.Routes.CreateMapping(ctx, routing.ModelMapping{ID: "sb_preset", ApplicationID: sbApp, GatewayModelName: sbPresetGateway, AppModelName: sbPresetModel, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateMapping sb_preset: %v", err)
	}
	if err := s.Routes.UpsertMappingCapabilities(ctx, "sb_preset", []routing.CapabilityRow{
		sbCap(routing.CapabilitySpeech, routing.CapabilityYes, now),
		sbCap(routing.CapabilitySpeechClone, routing.CapabilityYes, now),
		sbCap(routing.CapabilitySpeechStreaming, routing.CapabilityYes, now),
	}); err != nil {
		t.Fatalf("UpsertMappingCapabilities sb_preset: %v", err)
	}
	if voicesPayload != "" {
		seedSpeechBenchVoices(t, s, "sb_preset", voicesPayload)
	}
}

// seedSpeechBenchVoices writes a mapping's scope-"" voices cache row, as the health loop's voices
// step does.
func seedSpeechBenchVoices(t *testing.T, s *Server, mappingID, payload string) {
	t.Helper()
	if err := s.Routes.UpsertModelMappingCache(context.Background(), routing.ModelMappingCacheRow{
		MappingID: mappingID, CacheType: routing.ModelMappingCacheVoices, Payload: payload, UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("seed voices cache of %s: %v", mappingID, err)
	}
}

// voicelessCloneIs500Upstream answers like audio.cpp does for a clone model: the WAV when the
// request names a voice, the 500 the model gives a request with no reference ("reference_text is
// required") when it does not. Every other model gets the WAV whatever it is sent. It records the
// requests like newSpeechBenchUpstream; it is its own handler because the decision needs the body.
func voicelessCloneIs500Upstream(t *testing.T, wav []byte) *speechBenchUpstream {
	t.Helper()
	up := &speechBenchUpstream{}
	up.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		call := speechBenchCall{method: r.Method, path: r.URL.Path, raw: raw}
		_ = json.Unmarshal(raw, &call.body)
		up.mu.Lock()
		up.calls = append(up.calls, call)
		up.mu.Unlock()
		model, _ := call.body["model"].(string)
		if voice, _ := call.body["voice"].(string); model == sbPresetModel && voice == "" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":{"message":"reference_text is required"}}`))
			return
		}
		answerWAV(wav, true)(w, model)
	}))
	t.Cleanup(up.Close)
	return up
}

// A preset clone's manual benchmark synthesizes with a preset voice -- the bytewise first of its
// cached list, whatever order the row holds -- learns the output byte rate of the model's own
// format (44.1 kHz mono 16-bit here), caches it under the mapping id and opens the portal's gate
// at that sample rate: the mapping now streams, where before the benchmark skipped it and the
// portal buffered it. The request is the plain one plus exactly the voice, and the history row is
// the speech one every benchmark writes. A mapping-scope run on it is accepted (it was a 409).
func TestSpeechBenchmarkRunsAPresetCloneWithAPresetVoice(t *testing.T) {
	up := voicelessCloneIs500Upstream(t, buildPCMWav(t, 44100, 1, 16, 88200))
	s := newSpeechBenchFixture(t, up)
	addSpeechBenchPresetClone(t, s, `["Thorsten","Frank"]`)

	status, code, body := postSpeechBenchmark(t, s, "/api/portal/mappings/sb_preset/benchmark?mode=speech")
	if status != http.StatusAccepted {
		t.Fatalf("speech benchmark of a preset clone = %d %q (%s), want 202", status, code, body)
	}
	st := finishedSpeechRun(t, s)
	res := speechBenchResult(t, st, "sb_preset")
	if res.Skipped != "" || res.Error != "" || res.OutputByteRate != 88200 {
		t.Fatalf("result = %+v, want a measured, unskipped result with rate 88200", res)
	}
	calls := up.received()
	if len(calls) != 1 {
		t.Fatalf("upstream calls = %+v, want exactly one synthesis", calls)
	}
	c := calls[0]
	if c.body["model"] != sbPresetModel || c.body["voice"] != "Frank" {
		t.Fatalf("upstream body = %s, want the application's own model id and the first preset voice %q", c.raw, "Frank")
	}
	if in, _ := c.body["input"].(string); in == "" {
		t.Fatalf("upstream input is empty: %s", c.raw)
	}
	if len(c.body) != 3 {
		t.Fatalf("upstream body = %s, want exactly model, input and voice (a plain non-stream synthesis plus the preset)", c.raw)
	}
	if rate, ok := s.cachedOutputByteRate(context.Background(), "sb_preset"); !ok || rate != 88200 {
		t.Fatalf("cachedOutputByteRate(sb_preset) = (%d, %v), want (88200, true)", rate, ok)
	}
	if runs := speechBenchRuns(t, s, "sb_preset"); len(runs) != 1 || runs[0].Kind != "speech" || runs[0].Error != "" {
		t.Fatalf("history = %+v, want one successful kind=speech row", runs)
	}
	rate, open, err := s.portalSpeechStreamRate(context.Background(), auth.Token{UserID: "usr_a"}, sbPresetGateway)
	if err != nil || !open || rate != 44100 {
		t.Fatalf("portal gate for the preset clone = (%d, %v, %v), want open at 44100 Hz: it streams", rate, open, err)
	}
}

// The request body is the plain two-field one for a plain model -- an empty voice is omitted, not
// sent as "" -- and carries the voice for a clone target.
func TestSpeechBenchmarkRequestBody(t *testing.T) {
	plain, err := json.Marshal(speechBenchmarkRequest{Model: "m", Input: "hi"})
	if err != nil || string(plain) != `{"model":"m","input":"hi"}` {
		t.Fatalf("plain body = %s (%v), want exactly model and input", plain, err)
	}
	clone, err := json.Marshal(speechBenchmarkRequest{Model: "m", Input: "hi", Voice: "Frank"})
	if err != nil || string(clone) != `{"model":"m","input":"hi","voice":"Frank"}` {
		t.Fatalf("clone body = %s (%v), want model, input and the voice", clone, err)
	}
}

// An application-scope run measures a preset clone beside the plain models and still skips a
// reference-only clone (no presets cached) and a transcriber: the preset voice is asked for only
// the mapping that has one, and every other request is the plain two-field one.
func TestSpeechBenchmarkApplicationScopeRunsPresetClonesAndSkipsReferenceOnlyOnes(t *testing.T) {
	up := voicelessCloneIs500Upstream(t, buildPCMWav(t, 24000, 1, 16, 48000))
	s := newSpeechBenchFixture(t, up)
	addSpeechBenchPresetClone(t, s, `["Frank"]`)

	if status, code, body := postSpeechBenchmark(t, s, "/api/portal/applications/"+sbApp+"/benchmark?mode=speech"); status != http.StatusAccepted {
		t.Fatalf("application-scope speech benchmark = %d %q (%s), want 202", status, code, body)
	}
	st := finishedSpeechRun(t, s)
	if st.Total != 5 || st.Done != 5 {
		t.Fatalf("run total/done = %d/%d, want 5/5", st.Total, st.Done)
	}
	for id, want := range map[string]string{"sb_clone": benchmarkSkippedSpeechClone, "sb_asr": benchmarkSkippedNotSpeech} {
		if res := speechBenchResult(t, st, id); res.Skipped != want || res.Error != "" {
			t.Fatalf("%s result = %+v, want Skipped %q only", id, res, want)
		}
	}
	for _, id := range []string{"sb_tts", "sb_unknown", "sb_preset"} {
		if res := speechBenchResult(t, st, id); res.Skipped != "" || res.Error != "" || res.OutputByteRate != 48000 {
			t.Fatalf("%s result = %+v, want a measured result with rate 48000", id, res)
		}
	}
	voiceByModel := map[string]any{}
	for _, c := range up.received() {
		model, _ := c.body["model"].(string)
		voiceByModel[model] = c.body["voice"]
		if _, has := c.body["voice"]; has != (model == sbPresetModel) {
			t.Fatalf("request for %s = %s: a voice is sent exactly for the preset clone", model, c.raw)
		}
	}
	if len(voiceByModel) != 3 || voiceByModel[sbPresetModel] != "Frank" {
		t.Fatalf("models synthesized = %v, want the two plain models and the preset clone with voice Frank", voiceByModel)
	}
}

// A clone model whose voices cache lists nothing usable -- no row, an empty list, only blank
// names, or a payload that does not decode -- has no preset to synthesize with and stays skipped,
// whatever its other verdicts say (streaming included): a mapping-scope run is the 409, nothing is
// reserved or sent, and the health loop's trigger starts nothing.
func TestSpeechBenchmarkSkipsACloneWithNoUsablePreset(t *testing.T) {
	for _, tc := range []struct{ name, payload string }{
		{"no voices row", ""},
		{"an empty list", `[]`},
		{"only blank names", `[" ",""]`},
		{"an undecodable payload", `{not json`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := voicelessCloneIs500Upstream(t, buildPCMWav(t, 24000, 1, 16, 48000))
			s := newSpeechBenchFixture(t, up)
			addSpeechBenchPresetClone(t, s, tc.payload)

			if status, code, _ := postSpeechBenchmark(t, s, "/api/portal/mappings/sb_preset/benchmark?mode=speech"); status != http.StatusConflict || code != codeBenchmarkSpeechUnavailable {
				t.Fatalf("speech benchmark of a clone with %s = %d %q, want 409 %s", tc.name, status, code, codeBenchmarkSpeechUnavailable)
			}
			if speechTrigger(t, s, "sb_preset", "/models/preset") {
				t.Fatalf("the health loop's trigger started a run for a clone with %s", tc.name)
			}
			if s.Benchmarks.ServerBusy(sbServer) {
				t.Fatal("a refused speech benchmark left the server reserved")
			}
			if calls := up.received(); len(calls) != 0 {
				t.Fatalf("upstream calls = %+v, want none", calls)
			}
		})
	}
}

// speechBenchmarkSkip is the one skip rule: a plain model is a target, a clone model is one only
// with a preset to name, and everything that does not synthesize speech is not_speech. The
// preset is the caller's answer (the partition reads it for a clone row alone), so a clone's
// verdict changes with it and a plain model's never does.
func TestSpeechBenchmarkSkip(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	rows := func(speech, clone string) []routing.CapabilityRow {
		var out []routing.CapabilityRow
		if speech != "" {
			out = append(out, sbCap(routing.CapabilitySpeech, speech, now))
		}
		if clone != "" {
			out = append(out, sbCap(routing.CapabilitySpeechClone, clone, now))
		}
		return out
	}
	yes, no := routing.CapabilityYes, routing.CapabilityNo
	for _, tc := range []struct {
		name   string
		rows   []routing.CapabilityRow
		preset string
		want   string
	}{
		{"a plain tts model", rows(yes, no), "", ""},
		{"a speech model whose clone verdict is unknown", rows(yes, ""), "", ""},
		{"a clone model with a preset", rows(yes, yes), "Frank", ""},
		{"a clone model with no preset", rows(yes, yes), "", benchmarkSkippedSpeechClone},
		{"a transcriber", rows(no, ""), "", benchmarkSkippedNotSpeech},
		{"a model with no speech verdict", rows("", ""), "", benchmarkSkippedNotSpeech},
		{"a clone verdict on a model that is not speech", rows(no, yes), "Frank", benchmarkSkippedNotSpeech},
		{"an unknown-speech model with a preset on file", rows("", yes), "Frank", benchmarkSkippedNotSpeech},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := speechBenchmarkSkip(tc.rows, tc.preset); got != tc.want {
				t.Fatalf("speechBenchmarkSkip(%+v, %q) = %q, want %q", tc.rows, tc.preset, got, tc.want)
			}
		})
	}
}

// voicesCacheSpy wraps the route store, counting the reads of the voices cache per mapping and
// failing them on demand: what the partition reads, and when, is the contract under test.
type voicesCacheSpy struct {
	routing.Store
	mu      sync.Mutex
	reads   []string
	failFor map[string]bool
}

func (v *voicesCacheSpy) ModelMappingCache(ctx context.Context, mappingID, cacheType string) ([]routing.ModelMappingCacheRow, error) {
	if cacheType == routing.ModelMappingCacheVoices {
		v.mu.Lock()
		v.reads = append(v.reads, mappingID)
		fail := v.failFor[mappingID]
		v.mu.Unlock()
		if fail {
			return nil, errors.New("voices cache: boom")
		}
	}
	return v.Store.ModelMappingCache(ctx, mappingID, cacheType)
}

func (v *voicesCacheSpy) voicesReads() []string {
	v.mu.Lock()
	defer v.mu.Unlock()
	return append([]string(nil), v.reads...)
}

// speechBenchViews builds the partition's input for the named fixture mappings, the way a
// manual run's scope resolution does.
func speechBenchViews(t *testing.T, s *Server, ids ...string) []portal.BenchmarkTargetView {
	t.Helper()
	var views []portal.BenchmarkTargetView
	for _, id := range ids {
		server, app, mp := speechTriggerEntities(t, s, id)
		views = append(views, portal.BenchmarkTargetView{Server: server, App: app, Mapping: mp})
	}
	return views
}

// The partition decides the preset voice ONCE per mapping and carries it on the target, so the
// manual run and the health loop's trigger (both go through it) synthesize identically; and it
// reads the voices cache only for a clone row, so a plain model costs no extra read.
func TestPartitionSpeechBenchmarkViewsDecidesThePresetVoice(t *testing.T) {
	up := voicelessCloneIs500Upstream(t, buildPCMWav(t, 24000, 1, 16, 48000))
	s := newSpeechBenchFixture(t, up)
	addSpeechBenchPresetClone(t, s, `["Thorsten","Frank"]`)
	spy := &voicesCacheSpy{Store: s.Routes}
	s.Routes = spy

	p := s.partitionSpeechBenchmarkViews(context.Background(), speechBenchViews(t, s, "sb_tts", "sb_unknown", "sb_clone", "sb_asr", "sb_preset"))

	voices := map[string]string{}
	for _, tgt := range p.runnable {
		voices[tgt.mapping.ID] = tgt.voice
	}
	want := map[string]string{"sb_tts": "", "sb_unknown": "", "sb_preset": "Frank"}
	if len(voices) != len(want) {
		t.Fatalf("runnable = %v, want exactly %v (plain models voiceless, the preset clone with its first preset)", voices, want)
	}
	for id, voice := range want {
		if got, ok := voices[id]; !ok || got != voice {
			t.Fatalf("runnable voice of %s = (%q, %v), want %q", id, got, ok, voice)
		}
	}
	skipped := map[string]string{}
	for _, r := range p.recorded {
		skipped[r.MappingID] = r.Skipped
	}
	if skipped["sb_clone"] != benchmarkSkippedSpeechClone || skipped["sb_asr"] != benchmarkSkippedNotSpeech || len(skipped) != 2 || p.unreadable != 0 {
		t.Fatalf("recorded = %v (unreadable %d), want the reference-only clone and the transcriber skipped with their reasons", skipped, p.unreadable)
	}
	// Only the two clone rows read the voices cache.
	if got := spy.voicesReads(); len(got) != 2 || got[0] != "sb_clone" || got[1] != "sb_preset" {
		t.Fatalf("voices cache reads = %v, want only the clone rows [sb_clone sb_preset]", got)
	}
}

// A voices cache that cannot be read for a CLONE row is unreadable -- whether it has presets is
// unknown, which is not guessed at -- recorded as an error and counted, like the capability and
// runtime-spec reads; a mapping-scope run for it is the 500, not the 409. A plain model never
// reads the cache, so the same failure leaves it a target.
func TestPartitionSpeechBenchmarkViewsVoicesReadErrorIsUnreadable(t *testing.T) {
	// The partition logs each unreadable read (a warning, so an operator sees why the mapping
	// was not benchmarked): captured, and asserted, so it is not noise on the test output.
	var logged bytes.Buffer
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	up := voicelessCloneIs500Upstream(t, buildPCMWav(t, 24000, 1, 16, 48000))
	s := newSpeechBenchFixture(t, up)
	addSpeechBenchPresetClone(t, s, `["Frank"]`)
	s.Routes = &voicesCacheSpy{Store: s.Routes, failFor: map[string]bool{"sb_preset": true, "sb_tts": true}}

	p := s.partitionSpeechBenchmarkViews(context.Background(), speechBenchViews(t, s, "sb_tts", "sb_preset"))
	if len(p.runnable) != 1 || p.runnable[0].mapping.ID != "sb_tts" || p.runnable[0].voice != "" {
		t.Fatalf("runnable = %+v, want only the plain model, voiceless (it never reads the voices cache)", p.runnable)
	}
	if p.unreadable != 1 || len(p.recorded) != 1 || p.recorded[0].MappingID != "sb_preset" || p.recorded[0].Skipped != "" ||
		!strings.Contains(p.recorded[0].Error, "voices") || !strings.Contains(p.recorded[0].Error, "unreadable") {
		t.Fatalf("unreadable = %d, recorded = %+v, want the preset clone recorded as an unreadable voices cache", p.unreadable, p.recorded)
	}

	if status, code, _ := postSpeechBenchmark(t, s, "/api/portal/mappings/sb_preset/benchmark?mode=speech"); status != http.StatusInternalServerError || code != "benchmark.request_failed" {
		t.Fatalf("mapping-scope run with an unreadable voices cache = %d %q, want 500 benchmark.request_failed", status, code)
	}
	if s.Benchmarks.ServerBusy(sbServer) {
		t.Fatal("a refused speech benchmark left the server reserved")
	}
	if !strings.Contains(logged.String(), "voices cache read failed") {
		t.Fatalf("log = %q, want the unreadable voices cache warned about", logged.String())
	}
}

// The health loop's trigger runs a preset clone like the manual run does -- it goes through the
// same partition -- and the voice rides to the upstream; the identity the loop observed is
// recorded beside the rate it cached, exactly as for a plain model.
func TestTriggerSpeechRateBenchmarkRunsAPresetClone(t *testing.T) {
	up := voicelessCloneIs500Upstream(t, buildPCMWav(t, 44100, 1, 16, 88200))
	s := newSpeechBenchFixture(t, up)
	addSpeechBenchPresetClone(t, s, `["Thorsten","Frank"]`)

	if !speechTrigger(t, s, "sb_preset", "launch:abc") {
		t.Fatal("TriggerSpeechRateBenchmark = false for a streaming clone with cached presets, want true")
	}
	st := finishedSpeechRun(t, s)
	if st.Scope != "speech-rate" || st.Error != "" || st.Total != 1 {
		t.Fatalf("run = %+v, want a finished one-mapping speech-rate run", st)
	}
	if res := speechBenchResult(t, st, "sb_preset"); res.Error != "" || res.OutputByteRate != 88200 {
		t.Fatalf("result = %+v, want a measured 88200 B/s", res)
	}
	if calls := up.received(); len(calls) != 1 || calls[0].body["voice"] != "Frank" {
		t.Fatalf("upstream calls = %+v, want one synthesis with voice Frank", calls)
	}
	if rows := speechIdentityRows(t, s, "sb_preset"); len(rows) != 1 || rows[0].Payload != "launch:abc" {
		t.Fatalf("identity rows = %+v, want the observed identity recorded beside the rate", rows)
	}
}
