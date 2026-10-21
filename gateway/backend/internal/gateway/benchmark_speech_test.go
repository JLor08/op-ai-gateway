// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"op-ai-gateway/internal/apierror"
	"op-ai-gateway/internal/provider"
	"op-ai-gateway/internal/routing"
	"strconv"
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
// stereo 44.1 kHz model gets 176400, a mono 24 kHz one 48000, never a constant --
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
		{"stereo 44.1 kHz without metering headers", 44100, 2, false, "176400", 176400, 0, 0, 1},
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
