// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"op-ai-gateway/internal/inference"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/provider"
	"op-ai-gateway/internal/routing"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// The audio benchmark (#340) runs the speed, capacity and both modes on a mapping with the
// speech (TTS) or transcription (ASR) capability. Most tests here script the single
// measurement (Server.audioMeasureSeam), so the figures a run stores are exactly the ones
// the test chose; TestAudioSpeedRunStoresMappingMetrics drives the real measurement against
// an audio.cpp-shaped upstream.

// audioCall is one measurement the orchestration took, as the seam saw it.
type audioCall struct {
	kind   audioMeasureKind
	model  string
	warmUp bool
	level  int
}

// audioScript is a scripted measurement seam: answer decides each call's figures, and every
// call is recorded.
type audioScript struct {
	mu     sync.Mutex
	calls  []audioCall
	answer func(call audioMeasureCall, n int) (audioMeasurement, error)
}

func (a *audioScript) seam(_ context.Context, call audioMeasureCall) (audioMeasurement, error) {
	a.mu.Lock()
	a.calls = append(a.calls, audioCall{kind: call.kind, model: call.target.ProviderModel, warmUp: call.warmUp, level: call.level})
	n := len(a.calls)
	answer := a.answer
	a.mu.Unlock()
	return answer(call, n)
}

func (a *audioScript) recorded() []audioCall {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]audioCall(nil), a.calls...)
}

// audioFigures is a successful measurement with the given rate and time to first output.
func audioFigures(rate float64, ttftMS int64) audioMeasurement {
	return audioMeasurement{FirstByteMS: 1, TTFTMS: ttftMS, WallMS: ttftMS + 1000, AudioSeconds: 10, RateX: rate, RTF: 0.5}
}

// audioEnsureProvider is the provider of an audio benchmark test: it ensures agent instances
// (provider.RuntimeEnsurer), recording each address and refusing the ones in refuse with the
// agent router's error envelope, and it lists the loaded models (provider.LoadedModelLister).
type audioEnsureProvider struct {
	mu      sync.Mutex
	ensured []string
	refuse  map[string]string // address -> router code
	loaded  []string
}

func (p *audioEnsureProvider) Complete(context.Context, routing.Target, inference.Request) (provider.Response, error) {
	return provider.Response{}, errors.New("no chat here")
}

func (p *audioEnsureProvider) EnsureRuntimeModel(_ context.Context, target routing.Target) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ensured = append(p.ensured, target.ProviderModel)
	if code, ok := p.refuse[target.ProviderModel]; ok {
		return &provider.RouterError{Code: code, Err: provider.ErrUnavailable}
	}
	return nil
}

func (p *audioEnsureProvider) LoadedModels(context.Context, routing.Target, string, string) ([]string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.loaded...), nil
}

func (p *audioEnsureProvider) ensuredAddresses() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.ensured...)
}

// audioFixtureOpts shapes newAudioFixture's one mapping.
type audioFixtureOpts struct {
	agent        bool // a server_agent application with a launch spec; else a direct audio_cpp one
	replicable   bool
	maxInstances int
	features     []string // the agent's declared features
	caps         []routing.CapabilityRow
	locked       bool
	noByteRate   bool
}

type audioFixture struct {
	srv      *Server
	mem      *routing.MemoryStore
	provider *audioEnsureProvider
	script   *audioScript
	server   routing.AIServer
	app      routing.Application
}

var audioFixtureNow = time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC)

// ttsStreamingCaps is a plain TTS model that streams: the speed run streams its synthesis.
func ttsStreamingCaps() []routing.CapabilityRow {
	return []routing.CapabilityRow{
		sbCap(routing.CapabilitySpeech, routing.CapabilityYes, audioFixtureNow),
		sbCap(routing.CapabilitySpeechClone, routing.CapabilityNo, audioFixtureNow),
		sbCap(routing.CapabilitySpeechStreaming, routing.CapabilityYes, audioFixtureNow),
	}
}

// newAudioFixture seeds srv1 with one application (app1) holding one audio mapping (map1,
// upstream model "m") and wires a Server over it with a scripted measurement seam.
func newAudioFixture(t *testing.T, opts audioFixtureOpts) *audioFixture {
	t.Helper()
	ctx := context.Background()
	mem := routing.NewMemoryStore()
	must := func(what string, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}
	server := routing.AIServer{ID: "srv1", Name: "Host", Domain: "host.example.test", Provider: routing.ProviderMock, Endpoint: "mock://srv1", Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: audioFixtureNow, UpdatedAt: audioFixtureNow}
	must("CreateAIServer", mem.CreateAIServer(ctx, server))
	app := routing.Application{ID: "app1", ServerID: "srv1", Type: routing.ProviderAudioCpp, Port: 8080, Scheme: "http", TimeoutMS: 30000, APIFlavors: []string{routing.APIFlavorOpenAIAudio}, Status: routing.ServerStatusActive, BenchmarkScheduleEnabled: true, CreatedAt: audioFixtureNow, UpdatedAt: audioFixtureNow}
	if opts.agent {
		app.Type = routing.ProviderServerAgent
	}
	must("CreateApplication", mem.CreateApplication(ctx, app))
	must("CreateMapping", mem.CreateMapping(ctx, routing.ModelMapping{ID: "map1", ApplicationID: "app1", GatewayModelName: "gw-m", AppModelName: "m", Status: routing.ServerStatusActive, MetricsLocked: opts.locked, LoadTimeMS: 1234, CreatedAt: audioFixtureNow, UpdatedAt: audioFixtureNow}))
	if opts.agent {
		must("UpsertRuntimeSpec", mem.UpsertRuntimeSpec(ctx, routing.RuntimeSpec{
			ID: "rs_map1", MappingID: "map1", Enabled: true, Type: string(routing.RuntimeSpecTypeAudioCpp), Binary: "/opt/bin/audio-server",
			Args: "[]", Env: "{}", HealthPath: "/health", APIFlavors: []string{routing.APIFlavorOpenAIAudio},
			Replicable: opts.replicable, MinInstances: 1, MaxInstances: opts.maxInstances, CreatedAt: audioFixtureNow, UpdatedAt: audioFixtureNow,
		}))
	}
	caps := opts.caps
	if caps == nil {
		caps = ttsStreamingCaps()
	}
	must("UpsertMappingCapabilities", mem.UpsertMappingCapabilities(ctx, "map1", caps))
	if !opts.noByteRate {
		must("UpsertModelMappingCache", mem.UpsertModelMappingCache(ctx, routing.ModelMappingCacheRow{MappingID: "map1", CacheType: routing.ModelMappingCacheOutputByteRate, Payload: "48000", UpdatedAt: audioFixtureNow}))
	}
	prov := &audioEnsureProvider{refuse: map[string]string{}}
	script := &audioScript{answer: func(audioMeasureCall, int) (audioMeasurement, error) { return audioFigures(3, 300), nil }}
	srv := &Server{
		Provider:               prov,
		Routes:                 mem,
		Benchmarks:             NewBenchmarkRegistry(),
		RuntimeStatus:          NewRuntimeStatusRegistry(),
		AgentFeatures:          NewAgentFeaturesRegistry(),
		Active:                 newActiveRegistry(nil),
		audioRealtimeMinFactor: 1.2,
		audioConcurrencyMax:    8,
		audioMeasureSeam:       script.seam,
	}
	if opts.features != nil {
		srv.AgentFeatures.Set("srv1", opts.features)
	}
	return &audioFixture{srv: srv, mem: mem, provider: prov, script: script, server: server, app: app}
}

// target builds map1's benchmark target the way the speed/capacity/both partition does.
func (f *audioFixture) target(t *testing.T) benchmarkTarget {
	t.Helper()
	ctx := context.Background()
	mapping, err := f.mem.MappingByID(ctx, "map1")
	if err != nil {
		t.Fatalf("MappingByID: %v", err)
	}
	tgt, skipped, err := f.srv.benchmarkViewTarget(ctx, portal.BenchmarkTargetView{Server: f.server, App: f.app, Mapping: mapping}, true)
	if err != nil || skipped != "" {
		t.Fatalf("benchmarkViewTarget = skipped %q, err %v; want a target", skipped, err)
	}
	if tgt.audio.role == "" {
		t.Fatal("map1 is not an audio target")
	}
	return tgt
}

// run runs one benchmark of map1 in mode and returns its one result.
func (f *audioFixture) run(t *testing.T, mode string) BenchmarkResult {
	t.Helper()
	tgt := f.target(t)
	run, ok := f.srv.Benchmarks.TryStart("srv1", "mapping", mode, 1, time.Now().UTC(), func() {})
	if !ok {
		t.Fatal("TryStart did not start")
	}
	f.srv.runBenchmark(context.Background(), run, "srv1", []benchmarkTarget{tgt}, mode)
	st := f.srv.Benchmarks.Status("srv1")
	if len(st.Results) != 1 {
		t.Fatalf("results = %+v, want one", st.Results)
	}
	return st.Results[0]
}

func (f *audioFixture) mapping(t *testing.T) routing.ModelMapping {
	t.Helper()
	m, err := f.mem.MappingByID(context.Background(), "map1")
	if err != nil {
		t.Fatalf("MappingByID: %v", err)
	}
	return m
}

// rows is map1's history, by kind.
func (f *audioFixture) rows(t *testing.T) map[string][]routing.BenchmarkRun {
	t.Helper()
	runs, err := f.mem.BenchmarkRunsByMapping(context.Background(), "map1", 50)
	if err != nil {
		t.Fatalf("BenchmarkRunsByMapping: %v", err)
	}
	out := map[string][]routing.BenchmarkRun{}
	for _, r := range runs {
		out[r.Kind] = append(out[r.Kind], r)
	}
	return out
}

// capacityRow is map1's one audio_capacity history row, decoded.
func (f *audioFixture) capacityRow(t *testing.T) (routing.BenchmarkRun, audioCapacityHistory) {
	t.Helper()
	rows := f.rows(t)[benchmarkKindAudioCapacity]
	if len(rows) != 1 {
		t.Fatalf("audio_capacity rows = %d, want 1", len(rows))
	}
	var payload audioCapacityHistory
	if err := json.Unmarshal([]byte(rows[0].CapacityCurve), &payload); err != nil {
		t.Fatalf("capacity_curve %q does not decode: %v", rows[0].CapacityCurve, err)
	}
	return rows[0], payload
}

// levelRates answers a ramp level's streams from rates[k] (one rate per stream, the last one
// repeated), every warm-up and speed pass fast.
func levelRates(rates map[int][]float64) func(audioMeasureCall, int) (audioMeasurement, error) {
	var mu sync.Mutex
	seen := map[int]int{}
	return func(call audioMeasureCall, _ int) (audioMeasurement, error) {
		if call.warmUp || call.level == 0 {
			return audioFigures(10, 100), nil
		}
		mu.Lock()
		i := seen[call.level]
		seen[call.level]++
		mu.Unlock()
		r := rates[call.level]
		if len(r) == 0 {
			return audioFigures(10, 100), nil
		}
		return audioFigures(r[min(i, len(r)-1)], 100), nil
	}
}

// TestAudioSpeedRunStoresMappingMetrics drives a speed run of a streaming TTS mapping
// against an audio.cpp-shaped upstream, with the real measurement: the run takes the cached
// byte rate, synthesizes the passage twice as a stream (the cold pass, which is the warm-up,
// then the measured pass), stores the measured rate and time to first audio on the mapping
// (UpdateMappingAudioMetrics, source benchmark), reports them on its result and appends one
// kind=audio history row carrying the time to first audio, the wall time, the real-time
// factor and the rate. With no byte rate cached, the run first measures one with the speech
// benchmark's non-streamed synthesis.
func TestAudioSpeedRunStoresMappingMetrics(t *testing.T) {
	for _, cached := range []bool{true, false} {
		t.Run(fmt.Sprintf("byte rate cached %v", cached), func(t *testing.T) {
			var mu sync.Mutex
			var bodies []map[string]any
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/audio/speech" {
					http.NotFound(w, r)
					return
				}
				raw, _ := io.ReadAll(r.Body)
				var body map[string]any
				_ = json.Unmarshal(raw, &body)
				mu.Lock()
				bodies = append(bodies, body)
				mu.Unlock()
				if body["stream"] != true {
					answerWAV(buildMonoWAV(48000, 48000), false)(w, "")
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				flusher := w.(http.Flusher)
				time.Sleep(10 * time.Millisecond)
				_, _ = io.WriteString(w, sseSpeechDelta(24000))
				flusher.Flush()
				time.Sleep(30 * time.Millisecond)
				_, _ = io.WriteString(w, sseSpeechDelta(24000))
				_, _ = io.WriteString(w, sseSpeechDone)
				flusher.Flush()
			}))
			defer up.Close()
			f := newAudioFixture(t, audioFixtureOpts{noByteRate: !cached})
			f.srv.audioMeasureSeam = nil
			f.srv.Provider = provider.NewOpenAICompatibleClient(http.DefaultClient)
			u, _ := url.Parse(up.URL)
			port, _ := strconv.Atoi(u.Port())
			f.server.Domain, f.app.Port, f.app.Scheme = u.Hostname(), port, u.Scheme
			if err := f.mem.UpdateApplication(context.Background(), f.app); err != nil {
				t.Fatalf("UpdateApplication: %v", err)
			}

			res := f.run(t, "speed")

			if res.Error != "" {
				t.Fatalf("result error = %q", res.Error)
			}
			m := f.mapping(t)
			if m.AudioRateX <= 0 || m.AudioTTFAMS <= 0 || m.AudioMetricsUpdatedAt == nil || m.MetricsSource != "benchmark" {
				t.Fatalf("mapping audio metrics = rate %v, ttfa %d, at %v, source %q; want measured, stamped benchmark", m.AudioRateX, m.AudioTTFAMS, m.AudioMetricsUpdatedAt, m.MetricsSource)
			}
			if res.AudioRateX != m.AudioRateX || res.AudioTTFTMS != m.AudioTTFAMS {
				t.Fatalf("result = rate %v ttft %d; want the stored %v / %d", res.AudioRateX, res.AudioTTFTMS, m.AudioRateX, m.AudioTTFAMS)
			}
			rows := f.rows(t)
			audio := rows[benchmarkKindAudio]
			if len(audio) != 1 || len(rows["speed"]) != 0 {
				t.Fatalf("history = %v, want one kind=audio row and no speed row", rows)
			}
			row := audio[0]
			if row.Error != "" || row.TTFTMS != m.AudioTTFAMS || row.WallMS <= 0 || row.RTF <= 0 {
				t.Fatalf("audio row = %+v, want ttft %d, a wall time and a real-time factor", row, m.AudioTTFAMS)
			}
			var payload audioSpeedHistory
			if err := json.Unmarshal([]byte(row.CapacityCurve), &payload); err != nil || payload.RateX != m.AudioRateX {
				t.Fatalf("audio row payload = %q (%v), want the rate %v", row.CapacityCurve, err, m.AudioRateX)
			}
			mu.Lock()
			defer mu.Unlock()
			var streamed int
			for i, body := range bodies {
				if body["model"] != "m" {
					t.Fatalf("request %d named model %v, want m", i, body["model"])
				}
				if body["stream"] == true {
					streamed++
				} else if !cached && i != 0 {
					t.Fatalf("the non-streamed synthesis was request %d, want it first", i)
				}
			}
			wantAll := 2
			if !cached {
				wantAll = 3
				if rate, ok := f.srv.cachedOutputByteRate(context.Background(), "map1"); !ok || rate != 96000 {
					t.Fatalf("cached byte rate = %d (%v), want 96000 read from the WAV", rate, ok)
				}
			}
			if streamed != 2 || len(bodies) != wantAll {
				t.Fatalf("requests = %d, %d streamed; want %d, 2 streamed (the cold pass and the measured pass)", len(bodies), streamed, wantAll)
			}
		})
	}
}

// buildMonoWAV is a mono 16-bit PCM WAV at sampleRate with dataBytes of silence.
func buildMonoWAV(sampleRate, dataBytes int) []byte {
	var b strings.Builder
	le32 := func(v int) string {
		return string([]byte{byte(v), byte(v >> 8), byte(v >> 16), byte(v >> 24)})
	}
	le16 := func(v int) string { return string([]byte{byte(v), byte(v >> 8)}) }
	b.WriteString("RIFF" + le32(36+dataBytes) + "WAVE")
	b.WriteString("fmt " + le32(16) + le16(1) + le16(1) + le32(sampleRate) + le32(sampleRate*2) + le16(2) + le16(16))
	b.WriteString("data" + le32(dataBytes) + strings.Repeat("\x00", dataBytes))
	return []byte(b.String())
}

// TestAudioSpeedRunMeasuresColdStart: an agent target that is not resident, on a server
// where nothing else runs, is a confirmed cold start, and the load time is the cold pass's
// time to first audio minus the warm pass's: 5300 - 300 = 5000 ms, stored as the mapping's
// load_time_ms, on the history row and on the result. A load time written for a replicable
// spec pushes the server's runtime config (the idle timeout it derives depends on it). A
// resident target gets no load time and the mapping keeps the one it had.
func TestAudioSpeedRunMeasuresColdStart(t *testing.T) {
	for _, tc := range []struct {
		name     string
		resident bool
		want     int64
	}{
		{"cold at the start", false, 5000},
		{"resident at the start", true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAudioFixture(t, audioFixtureOpts{agent: true, replicable: true, features: []string{"runtime_manager"}})
			if tc.resident {
				f.provider.loaded = []string{"m"}
			}
			f.srv.RuntimeStatus.publish("srv1", []RuntimeStatusDTO{{SpecID: "rs_map1", Model: "m", State: "stopped"}})
			f.script.answer = func(call audioMeasureCall, n int) (audioMeasurement, error) {
				if call.kind != audioKindTTSStream {
					return audioMeasurement{}, fmt.Errorf("measured as %v, want a streamed synthesis", call.kind)
				}
				if n == 1 {
					return audioFigures(2.5, 5300), nil
				}
				return audioFigures(3, 300), nil
			}
			spy := spyRuntimeConfigPushes(f.srv)

			res := f.run(t, "speed")

			if res.Error != "" || res.AudioLoadTimeMS != tc.want {
				t.Fatalf("result = load %d ms, error %q; want %d", res.AudioLoadTimeMS, res.Error, tc.want)
			}
			if res.AudioRateX != 3 || res.AudioTTFTMS != 300 {
				t.Fatalf("result = rate %v ttft %d, want the warm pass's 3 / 300", res.AudioRateX, res.AudioTTFTMS)
			}
			wantStored := tc.want
			if wantStored == 0 {
				wantStored = 1234
			}
			if m := f.mapping(t); int64(m.LoadTimeMS) != wantStored {
				t.Fatalf("stored load_time_ms = %d, want %d", m.LoadTimeMS, wantStored)
			}
			if row := f.rows(t)[benchmarkKindAudio]; len(row) != 1 || int64(row[0].LoadTimeMS) != tc.want {
				t.Fatalf("audio rows = %+v, want one with load %d", row, tc.want)
			}
			if tc.want > 0 {
				spy.expectPush(t, "an audio load time of a replicable spec", "srv1")
			} else {
				spy.expectNoPush(t, "no load time measured", "srv1")
			}
		})
	}
}

// TestAudioSpeedMeasuresWhatTheMappingDoes: the speed run picks its one measurement from the
// mapping's capability rows -- a streamed synthesis for a TTS model that streams, a one-shot
// one for one that does not, a live transcription for an ASR model that streams on a target
// that can take one, a one-shot transcription otherwise -- and a mapping with both
// capabilities is measured for the one its role names (transcription before speech). A
// clone model is synthesized with its preset voice.
func TestAudioSpeedMeasuresWhatTheMappingDoes(t *testing.T) {
	yes, no := routing.CapabilityYes, routing.CapabilityNo
	row := func(capability, verdict string) routing.CapabilityRow {
		return sbCap(capability, verdict, audioFixtureNow)
	}
	for _, tc := range []struct {
		name     string
		agent    bool
		features []string
		caps     []routing.CapabilityRow
		voices   string
		want     audioMeasureKind
		voice    string
	}{
		{"streaming TTS", false, nil, ttsStreamingCaps(), "", audioKindTTSStream, ""},
		{"one-shot TTS", false, nil, []routing.CapabilityRow{row(routing.CapabilitySpeech, yes), row(routing.CapabilitySpeechStreaming, no)}, "", audioKindTTSBuffered, ""},
		{"a clone model with a preset", false, nil, []routing.CapabilityRow{row(routing.CapabilitySpeech, yes), row(routing.CapabilitySpeechClone, yes)}, `["ben","anna"]`, audioKindTTSBuffered, "anna"},
		{"streaming ASR on a direct audio.cpp", false, nil, []routing.CapabilityRow{row(routing.CapabilityTranscription, yes), row(routing.CapabilityTranscriptionStreaming, yes)}, "", audioKindASRLive, ""},
		{"one-shot ASR", false, nil, []routing.CapabilityRow{row(routing.CapabilityTranscription, yes), row(routing.CapabilityTranscriptionStreaming, no)}, "", audioKindASRBuffered, ""},
		{"streaming ASR on an agent without the live route", true, []string{"runtime_manager"}, []routing.CapabilityRow{row(routing.CapabilityTranscription, yes), row(routing.CapabilityTranscriptionStreaming, yes)}, "", audioKindASRBuffered, ""},
		{"streaming ASR on an agent with the live route", true, []string{"runtime_manager", fullduplexProxyFeature}, []routing.CapabilityRow{row(routing.CapabilityTranscription, yes), row(routing.CapabilityTranscriptionStreaming, yes)}, "", audioKindASRLive, ""},
		{"both capabilities: the role names transcription", false, nil, []routing.CapabilityRow{row(routing.CapabilitySpeech, yes), row(routing.CapabilitySpeechStreaming, yes), row(routing.CapabilityTranscription, yes)}, "", audioKindASRBuffered, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAudioFixture(t, audioFixtureOpts{agent: tc.agent, features: tc.features, caps: tc.caps})
			f.srv.Provider = &audioDuplexProvider{audioEnsureProvider: f.provider}
			if tc.voices != "" {
				if err := f.mem.UpsertModelMappingCache(context.Background(), routing.ModelMappingCacheRow{MappingID: "map1", CacheType: routing.ModelMappingCacheVoices, Payload: tc.voices, UpdatedAt: audioFixtureNow}); err != nil {
					t.Fatalf("seed voices: %v", err)
				}
			}
			var voices []string
			var mu sync.Mutex
			f.script.answer = func(call audioMeasureCall, _ int) (audioMeasurement, error) {
				mu.Lock()
				voices = append(voices, call.voice)
				mu.Unlock()
				return audioFigures(3, 300), nil
			}

			if res := f.run(t, "speed"); res.Error != "" {
				t.Fatalf("result error = %q", res.Error)
			}
			calls := f.script.recorded()
			if len(calls) != 2 {
				t.Fatalf("measurements = %+v, want the cold and the warm pass", calls)
			}
			for _, c := range calls {
				if c.kind != tc.want || c.model != "m" {
					t.Fatalf("measurement = %+v, want %v of m", c, tc.want)
				}
			}
			for _, v := range voices {
				if v != tc.voice {
					t.Fatalf("voice = %q, want %q", v, tc.voice)
				}
			}
		})
	}
}

// audioDuplexProvider adds the live transcription relay to audioEnsureProvider, so a live
// measurement is possible at all (the seam answers it).
type audioDuplexProvider struct {
	*audioEnsureProvider
}

func (p *audioDuplexProvider) ProxyNativeDuplex(context.Context, routing.Target, string, io.Reader, string) (*provider.ProxyResponse, error) {
	return nil, errors.New("not used: the seam measures")
}

// TestAudioCapacityStopsOnPerformance: the ramp runs k = 1, 2, 3 ... streams and a level is
// good while its slowest stream keeps the real-time threshold (1.2). Rates 3.0 at k=1, 1.5
// and 2.5 at k=2 and 1.1 at k=3 stop the ramp at 3 with a real-time concurrency of 2 and the
// limit performance: a measured ceiling, stored as one pair with source capacity, and the
// curve records each level's slowest and mean rate.
func TestAudioCapacityStopsOnPerformance(t *testing.T) {
	f := newAudioFixture(t, audioFixtureOpts{})
	f.script.answer = levelRates(map[int][]float64{1: {3.0}, 2: {1.5, 2.5}, 3: {1.1}})

	res := f.run(t, "capacity")

	if res.Error != "" || res.AudioRealtimeConcurrency != 2 || res.AudioConcurrencyLimit != routing.AudioLimitPerformance {
		t.Fatalf("result = %d / %q, error %q; want 2 / performance", res.AudioRealtimeConcurrency, res.AudioConcurrencyLimit, res.Error)
	}
	m := f.mapping(t)
	if m.AudioRealtimeConcurrency != 2 || m.AudioConcurrencyLimit != routing.AudioLimitPerformance || m.MetricsSource != "capacity" {
		t.Fatalf("mapping = %d / %q (source %q), want 2 / performance from a capacity run", m.AudioRealtimeConcurrency, m.AudioConcurrencyLimit, m.MetricsSource)
	}
	if m.AudioRateX != 0 {
		t.Fatalf("a capacity run stored a single-stream rate %v", m.AudioRateX)
	}
	row, payload := f.capacityRow(t)
	if row.Error != "" || payload.RealtimeConcurrency != 2 || payload.ConcurrencyLimit != routing.AudioLimitPerformance {
		t.Fatalf("audio_capacity row = %+v / %+v", row, payload)
	}
	wantCurve := []audioLevel{{K: 1, MinRateX: 3, MeanRateX: 3}, {K: 2, MinRateX: 1.5, MeanRateX: 2}, {K: 3, MinRateX: 1.1, MeanRateX: 1.1}}
	if !reflect.DeepEqual(payload.Curve, wantCurve) {
		t.Fatalf("curve = %+v, want %+v", payload.Curve, wantCurve)
	}
	if rows := f.rows(t); len(rows[benchmarkKindAudio]) != 0 || len(rows["speed"]) != 0 || len(rows["capacity"]) != 0 {
		t.Fatalf("history = %v, want only the audio_capacity row", rows)
	}
}

// TestAudioCapacityNotRealTimeEvenAlone: a model too slow at k=1 stores a measured 0 with the
// limit performance -- not real-time even alone, which is a result, not "unknown".
func TestAudioCapacityNotRealTimeEvenAlone(t *testing.T) {
	f := newAudioFixture(t, audioFixtureOpts{})
	f.script.answer = levelRates(map[int][]float64{1: {0.8}})

	res := f.run(t, "capacity")

	if res.Error != "" || res.AudioRealtimeConcurrency != 0 || res.AudioConcurrencyLimit != routing.AudioLimitPerformance {
		t.Fatalf("result = %d / %q, error %q; want 0 / performance", res.AudioRealtimeConcurrency, res.AudioConcurrencyLimit, res.Error)
	}
	if m := f.mapping(t); m.AudioRealtimeConcurrency != 0 || m.AudioConcurrencyLimit != routing.AudioLimitPerformance || m.AudioMetricsUpdatedAt == nil {
		t.Fatalf("mapping = %d / %q at %v, want the measured 0 / performance", m.AudioRealtimeConcurrency, m.AudioConcurrencyLimit, m.AudioMetricsUpdatedAt)
	}
}

// TestAudioCapacityStopsOnResources: a replicable target on an agent that runs instances is
// ramped one instance per stream -- each level ensures the next instance (m, m#1, m#2) and
// sends each stream to its own instance -- and the agent refusing m#2 for lack of room
// (409 runtime.instance_no_capacity on the ensure) stops the ramp at 2 with the limit
// resources: a lower bound. A stream the agent refuses with that code, or with
// runtime.instance_demand_unknown, stops it the same way.
func TestAudioCapacityStopsOnResources(t *testing.T) {
	instances := []string{"runtime_manager", runtimeInstancesFeature}
	t.Run("the ensure of m#2 is refused", func(t *testing.T) {
		f := newAudioFixture(t, audioFixtureOpts{agent: true, replicable: true, features: instances})
		f.provider.refuse["m#2"] = "runtime.instance_no_capacity"

		res := f.run(t, "capacity")

		if res.Error != "" || res.AudioRealtimeConcurrency != 2 || res.AudioConcurrencyLimit != routing.AudioLimitResources {
			t.Fatalf("result = %d / %q, error %q; want 2 / resources", res.AudioRealtimeConcurrency, res.AudioConcurrencyLimit, res.Error)
		}
		if got := f.provider.ensuredAddresses(); !slices.Equal(got, []string{"m", "m#1", "m#2"}) {
			t.Fatalf("ensured = %v, want m, m#1, m#2", got)
		}
		byLevel := map[int][]string{}
		for _, c := range f.script.recorded() {
			if !c.warmUp && c.level > 0 {
				byLevel[c.level] = append(byLevel[c.level], c.model)
			}
		}
		for k, models := range byLevel {
			sort.Strings(models)
			want := []string{"m", "m#1", "m#2"}[:k]
			if !slices.Equal(models, want) {
				t.Fatalf("level %d streams went to %v, want one per instance %v", k, models, want)
			}
		}
		if len(byLevel) != 2 {
			t.Fatalf("levels measured = %v, want 1 and 2", byLevel)
		}
		if m := f.mapping(t); m.AudioRealtimeConcurrency != 2 || m.AudioConcurrencyLimit != routing.AudioLimitResources {
			t.Fatalf("mapping = %d / %q, want 2 / resources", m.AudioRealtimeConcurrency, m.AudioConcurrencyLimit)
		}
	})
	for _, code := range []string{"runtime.instance_no_capacity", "runtime.instance_demand_unknown"} {
		t.Run("a stream refused with "+code, func(t *testing.T) {
			f := newAudioFixture(t, audioFixtureOpts{agent: true, replicable: true, features: instances})
			f.script.answer = func(call audioMeasureCall, _ int) (audioMeasurement, error) {
				if call.level == 3 && !call.warmUp && call.target.ProviderModel == "m#2" {
					return audioMeasurement{}, &audioMeasureError{Kind: audioMeasureUpstreamRefused, Code: code, Message: "no room"}
				}
				return audioFigures(5, 100), nil
			}

			res := f.run(t, "capacity")

			if res.Error != "" || res.AudioRealtimeConcurrency != 2 || res.AudioConcurrencyLimit != routing.AudioLimitResources {
				t.Fatalf("result = %d / %q, error %q; want 2 / resources", res.AudioRealtimeConcurrency, res.AudioConcurrencyLimit, res.Error)
			}
		})
	}
	t.Run("any other failure ends the run with no result stored", func(t *testing.T) {
		f := newAudioFixture(t, audioFixtureOpts{agent: true, replicable: true, features: instances})
		f.script.answer = func(call audioMeasureCall, _ int) (audioMeasurement, error) {
			if call.level == 2 && !call.warmUp {
				// A 409 whose code is not a room refusal is a failure, not a lower bound.
				return audioMeasurement{}, &audioMeasureError{Kind: audioMeasureUpstreamRefused, Status: http.StatusConflict, Code: "runtime.start_timeout", Message: "child never came up"}
			}
			return audioFigures(5, 100), nil
		}

		res := f.run(t, "capacity")

		if res.Error == "" || res.AudioConcurrencyLimit != "" {
			t.Fatalf("result = %d / %q, error %q; want a failed run without a limit", res.AudioRealtimeConcurrency, res.AudioConcurrencyLimit, res.Error)
		}
		if m := f.mapping(t); m.AudioConcurrencyLimit != "" || m.AudioMetricsUpdatedAt != nil {
			t.Fatalf("mapping = %d / %q at %v, want nothing stored", m.AudioRealtimeConcurrency, m.AudioConcurrencyLimit, m.AudioMetricsUpdatedAt)
		}
		row, payload := f.capacityRow(t)
		if row.Error == "" || len(payload.Curve) != 1 {
			t.Fatalf("audio_capacity row = %+v / %+v, want the error and the one level measured", row, payload)
		}
	})
}

// TestAudioCapacityCap: a model that is always fast stops at the ramp's own cap
// (OP_AI_GATEWAY_AUDIO_CONCURRENCY_MAX, here 3) with the limit cap, a lower bound; a
// replicable target stops at its max_instances when that is lower.
func TestAudioCapacityCap(t *testing.T) {
	t.Run("the ramp's cap", func(t *testing.T) {
		f := newAudioFixture(t, audioFixtureOpts{})
		f.srv.audioConcurrencyMax = 3

		res := f.run(t, "capacity")

		if res.Error != "" || res.AudioRealtimeConcurrency != 3 || res.AudioConcurrencyLimit != routing.AudioLimitCap {
			t.Fatalf("result = %d / %q, error %q; want 3 / cap", res.AudioRealtimeConcurrency, res.AudioConcurrencyLimit, res.Error)
		}
		if _, payload := f.capacityRow(t); len(payload.Curve) != 3 {
			t.Fatalf("curve = %+v, want three levels", payload.Curve)
		}
	})
	t.Run("max_instances", func(t *testing.T) {
		f := newAudioFixture(t, audioFixtureOpts{agent: true, replicable: true, maxInstances: 2, features: []string{"runtime_manager", runtimeInstancesFeature}})

		res := f.run(t, "capacity")

		if res.Error != "" || res.AudioRealtimeConcurrency != 2 || res.AudioConcurrencyLimit != routing.AudioLimitCap {
			t.Fatalf("result = %d / %q, error %q; want 2 / cap", res.AudioRealtimeConcurrency, res.AudioConcurrencyLimit, res.Error)
		}
		if got := f.provider.ensuredAddresses(); !slices.Equal(got, []string{"m", "m#1"}) {
			t.Fatalf("ensured = %v, want m and m#1 only", got)
		}
	})
}

// TestAudioCapacityNonReplicableSingleProcess: a target that is not ramped by instance -- a
// spec that is not replicable, a direct audio.cpp application, or a replicable spec on an
// agent that does not declare runtime_instances -- gets k concurrent streams on its one
// process, and no instance is ever ensured. max_instances does not cap that ramp.
func TestAudioCapacityNonReplicableSingleProcess(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts audioFixtureOpts
	}{
		{"an agent spec that is not replicable", audioFixtureOpts{agent: true, features: []string{"runtime_manager", runtimeInstancesFeature}}},
		{"a direct audio.cpp application", audioFixtureOpts{}},
		{"a replicable spec on an agent without runtime_instances", audioFixtureOpts{agent: true, replicable: true, maxInstances: 1, features: []string{"runtime_manager"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAudioFixture(t, tc.opts)
			f.script.answer = levelRates(map[int][]float64{1: {4}, 2: {2}, 3: {0.9}})

			res := f.run(t, "capacity")

			if res.AudioRealtimeConcurrency != 2 || res.AudioConcurrencyLimit != routing.AudioLimitPerformance {
				t.Fatalf("result = %d / %q, error %q; want 2 / performance", res.AudioRealtimeConcurrency, res.AudioConcurrencyLimit, res.Error)
			}
			if got := f.provider.ensuredAddresses(); len(got) != 0 {
				t.Fatalf("ensured = %v, want none", got)
			}
			perLevel := map[int]int{}
			for _, c := range f.script.recorded() {
				if c.model != "m" {
					t.Fatalf("a stream went to %q, want every one to m", c.model)
				}
				if !c.warmUp && c.level > 0 {
					perLevel[c.level]++
				}
			}
			if !reflect.DeepEqual(perLevel, map[int]int{1: 1, 2: 2, 3: 3}) {
				t.Fatalf("streams per level = %v, want k at level k", perLevel)
			}
		})
	}
}

// TestAudioRunMetricsLockedUntouched: a locked mapping keeps every figure an operator
// entered; the run still measures and records its history rows.
func TestAudioRunMetricsLockedUntouched(t *testing.T) {
	f := newAudioFixture(t, audioFixtureOpts{agent: true, locked: true, features: []string{"runtime_manager"}})
	before := f.mapping(t)
	f.srv.RuntimeStatus.publish("srv1", []RuntimeStatusDTO{{SpecID: "rs_map1", Model: "m", State: "stopped"}})
	f.script.answer = func(call audioMeasureCall, n int) (audioMeasurement, error) {
		if n == 1 {
			return audioFigures(2, 4000), nil
		}
		return levelRates(map[int][]float64{3: {0.5}})(call, n)
	}

	res := f.run(t, "both")

	if res.Error != "" || res.AudioRateX == 0 || res.AudioConcurrencyLimit == "" {
		t.Fatalf("result = %+v, want both measured", res)
	}
	if after := f.mapping(t); !reflect.DeepEqual(after, before) {
		t.Fatalf("locked mapping changed:\n got %+v\nwant %+v", after, before)
	}
	rows := f.rows(t)
	if len(rows[benchmarkKindAudio]) != 1 || len(rows[benchmarkKindAudioCapacity]) != 1 {
		t.Fatalf("history = %v, want one audio and one audio_capacity row", rows)
	}
}

// TestAudioBothRunMeasuresSpeedThenCapacity: a both run measures the speed pass and then the
// ramp, stores both on the mapping and reports both on its one result.
func TestAudioBothRunMeasuresSpeedThenCapacity(t *testing.T) {
	f := newAudioFixture(t, audioFixtureOpts{})
	f.script.answer = levelRates(map[int][]float64{1: {2}, 2: {1}})

	res := f.run(t, "both")

	if res.Error != "" || res.AudioRateX != 10 || res.AudioRealtimeConcurrency != 1 || res.AudioConcurrencyLimit != routing.AudioLimitPerformance {
		t.Fatalf("result = %+v, want the speed rate 10 and 1 / performance", res)
	}
	m := f.mapping(t)
	if m.AudioRateX != 10 || m.AudioRealtimeConcurrency != 1 || m.AudioConcurrencyLimit != routing.AudioLimitPerformance || m.MetricsSource != "capacity" {
		t.Fatalf("mapping = %+v, want both stored, the ramp last", m)
	}
	calls := f.script.recorded()
	if len(calls) < 3 || calls[0].level != 0 || calls[1].level != 0 {
		t.Fatalf("measurements = %+v, want the speed passes first", calls)
	}
}

// TestServerBothRunCoversTextAndAudio: a server-scope both run measures a chat mapping with
// the text benchmark and a TTS mapping with the audio one, each with its own history kinds,
// where a speed run used to skip the audio mapping.
func TestServerBothRunCoversTextAndAudio(t *testing.T) {
	s := newBenchmarkActiveFixture(t)
	s.Provider = newColdLister(nil)
	s.capacityMaxConcurrency, s.capacitySettle = 1, time.Millisecond
	script := &audioScript{answer: levelRates(map[int][]float64{2: {0.5}})}
	s.audioMeasureSeam = script.seam
	ctx := context.Background()
	must := func(what string, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}
	must("CreateApplication(chat)", s.Routes.CreateApplication(ctx, routing.Application{ID: "app_chat", ServerID: baOwnedServer, Type: routing.ProviderMock, Port: 8100, Scheme: "http", TimeoutMS: 30000, Status: routing.ServerStatusActive, CreatedAt: audioFixtureNow, UpdatedAt: audioFixtureNow}))
	must("CreateApplication(audio)", s.Routes.CreateApplication(ctx, routing.Application{ID: "app_audio", ServerID: baOwnedServer, Type: routing.ProviderAudioCpp, Port: 8200, Scheme: "http", TimeoutMS: 30000, APIFlavors: []string{routing.APIFlavorOpenAIAudio}, Status: routing.ServerStatusActive, CreatedAt: audioFixtureNow, UpdatedAt: audioFixtureNow}))
	must("CreateMapping(chat)", s.Routes.CreateMapping(ctx, routing.ModelMapping{ID: "map_chat", ApplicationID: "app_chat", GatewayModelName: "gw-chat", AppModelName: "chat", Status: routing.ServerStatusActive, CreatedAt: audioFixtureNow, UpdatedAt: audioFixtureNow}))
	must("CreateMapping(tts)", s.Routes.CreateMapping(ctx, routing.ModelMapping{ID: "map_tts", ApplicationID: "app_audio", GatewayModelName: "gw-tts", AppModelName: "tts", Status: routing.ServerStatusActive, CreatedAt: audioFixtureNow, UpdatedAt: audioFixtureNow}))
	must("UpsertMappingCapabilities", s.Routes.UpsertMappingCapabilities(ctx, "map_tts", ttsStreamingCaps()))
	must("UpsertModelMappingCache", s.Routes.UpsertModelMappingCache(ctx, routing.ModelMappingCacheRow{MappingID: "map_tts", CacheType: routing.ModelMappingCacheOutputByteRate, Payload: "48000", UpdatedAt: audioFixtureNow}))

	status, code, body := postSpeechBenchmark(t, s, "/api/portal/servers/"+baOwnedServer+"/benchmark?mode=both")
	if status != http.StatusAccepted {
		t.Fatalf("status = %d code %q body %s, want 202", status, code, body)
	}
	waitFor(t, func() bool { return !s.Benchmarks.Status(baOwnedServer).Running })
	st := s.Benchmarks.Status(baOwnedServer)
	if st.Error != "" || len(st.Results) != 2 {
		t.Fatalf("status = %+v, want two results and no error", st)
	}
	for _, res := range st.Results {
		if res.Error != "" || res.Skipped != "" {
			t.Fatalf("result %+v, want every mapping measured", res)
		}
	}
	kinds := func(mappingID string) []string {
		runs, err := s.Routes.BenchmarkRunsByMapping(ctx, mappingID, 10)
		if err != nil {
			t.Fatalf("BenchmarkRunsByMapping(%s): %v", mappingID, err)
		}
		var out []string
		for _, r := range runs {
			out = append(out, r.Kind)
		}
		sort.Strings(out)
		return out
	}
	if got := kinds("map_chat"); !slices.Equal(got, []string{"capacity", "speed"}) {
		t.Fatalf("chat history kinds = %v, want capacity and speed", got)
	}
	if got := kinds("map_tts"); !slices.Equal(got, []string{benchmarkKindAudio, benchmarkKindAudioCapacity}) {
		t.Fatalf("tts history kinds = %v, want audio and audio_capacity", got)
	}
	for _, c := range script.recorded() {
		if c.model != "tts" {
			t.Fatalf("an audio measurement went to %q, want only tts", c.model)
		}
	}
	for _, m := range s.Provider.(*coldLister).streamedModels() {
		if m == "tts" {
			t.Fatal("the TTS mapping was sent a chat prompt")
		}
	}
}

// TestAutoAudioRunOnceWhenMissing: the health loop's one-time automatic run starts a both run
// for an audio mapping on an idle server that has no audio metrics yet, or that is replicable
// and has no ramp result yet, at most once per mapping per process -- the second health tick
// starts nothing, whatever the first run did. A mapping with metrics, a replicable one with a
// ramp result (a measured 0 included) and a locked one never start one.
func TestAutoAudioRunOnceWhenMissing(t *testing.T) {
	stamp := audioFixtureNow
	for _, tc := range []struct {
		name       string
		replicable bool
		metricsAt  *time.Time
		limit      string
		locked     bool
		want       bool
	}{
		{"no audio metrics yet", false, nil, "", false, true},
		{"replicable without a ramp result", true, &stamp, "", false, true},
		{"replicable with a measured 0", true, &stamp, routing.AudioLimitPerformance, false, false},
		{"metrics present, not replicable", false, &stamp, "", false, false},
		{"locked", false, nil, "", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAudioFixture(t, audioFixtureOpts{agent: true, replicable: tc.replicable, locked: tc.locked, features: []string{"runtime_manager"}})
			ctx := context.Background()
			if tc.metricsAt != nil {
				if err := f.mem.UpdateMappingAudioMetrics(ctx, "map1", routing.AudioMetrics{RateX: 3, ConcurrencyLimit: tc.limit}, "benchmark", *tc.metricsAt); err != nil {
					t.Fatalf("UpdateMappingAudioMetrics: %v", err)
				}
			}
			mapping := f.mapping(t)

			first := f.srv.TriggerAudioBenchmark(ctx, f.server, f.app, mapping)
			if first != tc.want {
				t.Fatalf("first tick = %v, want %v", first, tc.want)
			}
			if !first {
				if f.srv.Benchmarks.ServerBusy("srv1") || len(f.script.recorded()) != 0 {
					t.Fatal("a run was reserved or measured although none was started")
				}
				return
			}
			waitFor(t, func() bool { return !f.srv.Benchmarks.Status("srv1").Running })
			st := f.srv.Benchmarks.Status("srv1")
			if st.Mode != "both" || st.Scope != benchmarkScopeAudioAuto || len(st.Results) != 1 {
				t.Fatalf("status = %+v, want one both run of scope %q", st, benchmarkScopeAudioAuto)
			}
			if second := f.srv.TriggerAudioBenchmark(ctx, f.server, f.app, f.mapping(t)); second {
				t.Fatal("second tick started another run")
			}
		})
	}
	t.Run("a failed run is not started again", func(t *testing.T) {
		f := newAudioFixture(t, audioFixtureOpts{agent: true, features: []string{"runtime_manager"}})
		f.script.answer = func(audioMeasureCall, int) (audioMeasurement, error) {
			return audioMeasurement{}, &audioMeasureError{Kind: audioMeasureTransport, Message: "connection refused"}
		}
		ctx := context.Background()
		if !f.srv.TriggerAudioBenchmark(ctx, f.server, f.app, f.mapping(t)) {
			t.Fatal("first tick started nothing")
		}
		waitFor(t, func() bool { return !f.srv.Benchmarks.Status("srv1").Running })
		if f.srv.TriggerAudioBenchmark(ctx, f.server, f.app, f.mapping(t)) {
			t.Fatal("the failed mapping was started again")
		}
	})
	t.Run("an agent not heard from since the gateway started defers", func(t *testing.T) {
		// Whether its replicable spec is ramped by instance depends on the agent's
		// declared features, so the run waits until they are known.
		f := newAudioFixture(t, audioFixtureOpts{agent: true, replicable: true})
		ctx := context.Background()
		if f.srv.TriggerAudioBenchmark(ctx, f.server, f.app, f.mapping(t)) {
			t.Fatal("started a run before the agent's features were known")
		}
		f.srv.AgentFeatures.Set("srv1", []string{"runtime_manager", runtimeInstancesFeature})
		if !f.srv.TriggerAudioBenchmark(ctx, f.server, f.app, f.mapping(t)) {
			t.Fatal("the deferred mapping was not started once the features were known")
		}
		waitFor(t, func() bool { return !f.srv.Benchmarks.Status("srv1").Running })
	})
	t.Run("a busy server defers without using up the one run", func(t *testing.T) {
		f := newAudioFixture(t, audioFixtureOpts{agent: true, features: []string{"runtime_manager"}})
		ctx := context.Background()
		if _, ok := f.srv.Benchmarks.TryStart("srv1", "mapping", "speed", 1, time.Now().UTC(), func() {}); !ok {
			t.Fatal("TryStart")
		}
		if f.srv.TriggerAudioBenchmark(ctx, f.server, f.app, f.mapping(t)) {
			t.Fatal("started a run on a busy server")
		}
		f.srv.Benchmarks.Release("srv1")
		if !f.srv.TriggerAudioBenchmark(ctx, f.server, f.app, f.mapping(t)) {
			t.Fatal("the deferred mapping was not started once the server was free")
		}
		waitFor(t, func() bool { return !f.srv.Benchmarks.Status("srv1").Running })
	})
}

// TestAudioHistoryDecodesIntoThePortalDTO: the JSON an audio history row carries is written
// under the portal's own DTO keys, so the portal serves the figures the gateway stored. A
// field dropped or renamed on either side shows up as a zero on the far side.
func TestAudioHistoryDecodesIntoThePortalDTO(t *testing.T) {
	speed, err := json.Marshal(audioSpeedHistory{RateX: 3.5})
	if err != nil {
		t.Fatalf("marshal speed: %v", err)
	}
	var dto portal.BenchmarkRunDTO
	if err := json.Unmarshal(speed, &dto); err != nil || dto.AudioRateX != 3.5 {
		t.Fatalf("speed payload %s decodes to rate %v (%v), want 3.5", speed, dto.AudioRateX, err)
	}
	ramp, err := json.Marshal(audioCapacityHistory{RealtimeConcurrency: 2, ConcurrencyLimit: routing.AudioLimitResources, Curve: []audioLevel{{K: 1, MinRateX: 3, MeanRateX: 3.5}}})
	if err != nil {
		t.Fatalf("marshal ramp: %v", err)
	}
	dto = portal.BenchmarkRunDTO{}
	if err := json.Unmarshal(ramp, &dto); err != nil {
		t.Fatalf("unmarshal ramp: %v", err)
	}
	if dto.AudioRealtimeConcurrency != 2 || dto.AudioConcurrencyLimit != routing.AudioLimitResources || !reflect.DeepEqual(dto.AudioCurve, []portal.AudioLevelDTO{{K: 1, MinRateX: 3, MeanRateX: 3.5}}) {
		t.Fatalf("ramp payload %s decodes to %+v", ramp, dto)
	}
}
