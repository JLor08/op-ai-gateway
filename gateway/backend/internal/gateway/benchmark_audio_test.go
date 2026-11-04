// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"op-ai-gateway/internal/auth"
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
func (f *audioFixture) capacityRow(t *testing.T) (routing.BenchmarkRun, routing.AudioCapacityHistory) {
	t.Helper()
	rows := f.rows(t)[benchmarkKindAudioCapacity]
	if len(rows) != 1 {
		t.Fatalf("audio_capacity rows = %d, want 1", len(rows))
	}
	var payload routing.AudioCapacityHistory
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
// factor and the rate. With no byte rate cached, the run measures one with the speech
// benchmark's non-streamed synthesis between the cold pass and the measured pass: the cold
// pass is taken first, so the benchmark cannot warm the target before it.
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
			var payload routing.AudioSpeedHistory
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
				} else if !cached && i != 1 {
					t.Fatalf("the non-streamed synthesis was request %d, want it second, after the cold pass", i)
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

// audioSpeechWarmingProvider is audioEnsureProvider that also relays the speech benchmark's
// one-shot synthesis (provider.NativeProxyClient): it answers a 48 kHz mono WAV and, as a real
// upstream would, leaves the model loaded behind it.
type audioSpeechWarmingProvider struct {
	*audioEnsureProvider
	model string
}

func (p *audioSpeechWarmingProvider) ProxyNative(context.Context, routing.Target, string, []byte, string) (*provider.ProxyResponse, error) {
	p.mu.Lock()
	p.loaded = append(p.loaded, p.model)
	p.mu.Unlock()
	return &provider.ProxyResponse{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"audio/wav"}}, Body: io.NopCloser(bytes.NewReader(buildMonoWAV(48000, 48000)))}, nil
}

// TestAudioSpeedColdStartPrecedesTheByteRateBenchmark: a streamed synthesis whose byte rate is
// not cached needs the speech benchmark first, and that benchmark is a request to the target.
// A run that does not stop the server judges the cold start as it finds it, so the judgment and
// the cold pass come before the benchmark: the cold pass is timed to its first audio without
// counting audio, the benchmark then caches the rate, and the warm pass counts with it. The
// load time is measured, not lost to the run's own warm-up.
func TestAudioSpeedColdStartPrecedesTheByteRateBenchmark(t *testing.T) {
	f := newAudioFixture(t, audioFixtureOpts{agent: true, replicable: true, features: []string{"runtime_manager"}, noByteRate: true})
	f.srv.Provider = &audioSpeechWarmingProvider{audioEnsureProvider: f.provider, model: "m"}
	f.srv.RuntimeStatus.publish("srv1", []RuntimeStatusDTO{{SpecID: "rs_map1", Model: "m", State: "stopped"}})
	var rates []int
	f.script.answer = func(call audioMeasureCall, n int) (audioMeasurement, error) {
		if call.kind != audioKindTTSStream {
			return audioMeasurement{}, fmt.Errorf("measured as %v, want a streamed synthesis", call.kind)
		}
		rates = append(rates, call.byteRate)
		if n == 1 {
			return audioMeasurement{FirstByteMS: 1, TTFTMS: 5300, WallMS: 6300}, nil
		}
		return audioFigures(3, 300), nil
	}

	res := f.run(t, "speed")

	if res.Error != "" || res.AudioLoadTimeMS != 5000 {
		t.Fatalf("result = load %d ms, error %q; want 5000", res.AudioLoadTimeMS, res.Error)
	}
	if len(rates) != 2 || rates[0] != 0 || rates[1] != 96000 {
		t.Fatalf("byte rates of the passes = %v, want the cold pass uncounted (0) and the warm pass at the learned 96000", rates)
	}
	if rate, ok := f.srv.cachedOutputByteRate(context.Background(), "map1"); !ok || rate != 96000 {
		t.Fatalf("cached byte rate = %d (%v), want 96000", rate, ok)
	}
}

// TestAudioSpeedLoadTimeIsTheFirstOutputOfEachKind: the load time is the cold pass's time to
// its first output minus the warm pass's, and what counts as the first output depends on the
// measurement: the first response byte of a one-shot synthesis, the first transcript text of a
// live transcription, the whole answer of a one-shot transcription (and the first audio delta
// of a streamed synthesis, TestAudioSpeedRunMeasuresColdStart). Each pass below carries
// different figures in every field, so only the right one yields 5000 ms. A cold pass that was
// not the slower one -- as fast as the warm pass, or faster -- records no load time, and the
// mapping keeps the one it had.
func TestAudioSpeedLoadTimeIsTheFirstOutputOfEachKind(t *testing.T) {
	yes, no := routing.CapabilityYes, routing.CapabilityNo
	row := func(capability, verdict string) routing.CapabilityRow {
		return sbCap(capability, verdict, audioFixtureNow)
	}
	oneShotTTS := []routing.CapabilityRow{row(routing.CapabilitySpeech, yes), row(routing.CapabilitySpeechClone, no), row(routing.CapabilitySpeechStreaming, no)}
	liveASR := []routing.CapabilityRow{row(routing.CapabilityTranscription, yes), row(routing.CapabilityTranscriptionStreaming, yes)}
	oneShotASR := []routing.CapabilityRow{row(routing.CapabilityTranscription, yes), row(routing.CapabilityTranscriptionStreaming, no)}
	pass := func(firstByte, ttft, wall int64) audioMeasurement {
		return audioMeasurement{FirstByteMS: firstByte, TTFTMS: ttft, WallMS: wall, AudioSeconds: 10, RateX: 3, RTF: 0.3}
	}
	for _, tc := range []struct {
		name       string
		caps       []routing.CapabilityRow
		kind       audioMeasureKind
		cold, warm audioMeasurement
		want       int64
	}{
		{"one-shot synthesis: the first response byte", oneShotTTS, audioKindTTSBuffered, pass(5300, 7000, 9900), pass(300, 100, 1100), 5000},
		{"live transcription: the first transcript text", liveASR, audioKindASRLive, pass(10, 5300, 9900), pass(5, 300, 1500), 5000},
		{"one-shot transcription: the whole answer", oneShotASR, audioKindASRBuffered, pass(4000, 4000, 5300), pass(100, 100, 300), 5000},
		{"a cold pass as fast as the warm one", oneShotTTS, audioKindTTSBuffered, pass(300, 7000, 9900), pass(300, 100, 1100), 0},
		{"a cold pass faster than the warm one", oneShotASR, audioKindASRBuffered, pass(4000, 4000, 900), pass(100, 100, 1000), 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAudioFixture(t, audioFixtureOpts{agent: true, caps: tc.caps, features: []string{"runtime_manager", fullduplexProxyFeature}})
			f.srv.Provider = &audioDuplexProvider{audioEnsureProvider: f.provider}
			f.srv.RuntimeStatus.publish("srv1", []RuntimeStatusDTO{{SpecID: "rs_map1", Model: "m", State: "stopped"}})
			f.script.answer = func(call audioMeasureCall, n int) (audioMeasurement, error) {
				if call.kind != tc.kind {
					return audioMeasurement{}, fmt.Errorf("measured as %v, want %v", call.kind, tc.kind)
				}
				if n == 1 {
					return tc.cold, nil
				}
				return tc.warm, nil
			}

			res := f.run(t, "speed")

			if res.Error != "" || res.AudioLoadTimeMS != tc.want {
				t.Fatalf("result = load %d ms, error %q; want %d", res.AudioLoadTimeMS, res.Error, tc.want)
			}
			wantStored := tc.want
			if wantStored == 0 {
				wantStored = 1234
			}
			if m := f.mapping(t); int64(m.LoadTimeMS) != wantStored {
				t.Fatalf("stored load_time_ms = %d, want %d", m.LoadTimeMS, wantStored)
			}
		})
	}
}

// adminStateOf is the admin_state the fake agent last applied for spec id.
func (f *fakeAgentRuntime) adminStateOf(id string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.specLocked(id).adminState
}

// TestAudioColdPassWaitsOutTheStopsRefusal: a manual speed run's stop-all force-stops the
// running audio target and clears it again before the cold pass, and the agent refuses the
// target 503 runtime.admission_blocked until that clear reaches it (here 150 ms late). The
// cold pass takes the refusal for a start still under way and tries again until it is
// served, so the run succeeds and stores the load time of the attempt that was served
// (5300 - 300 = 5000 ms), with the clear before the first refusal and exactly two served
// passes (cold and warm).
func TestAudioColdPassWaitsOutTheStopsRefusal(t *testing.T) {
	f := newStopAllFixture(t, stopAllOpts{specs: []stopAllSpec{stopAllSpecOf("map1", "running", 741)}, clearDelay: 150 * time.Millisecond})
	if err := f.mem.UpsertModelMappingCache(context.Background(), routing.ModelMappingCacheRow{MappingID: "map1", CacheType: routing.ModelMappingCacheOutputByteRate, Payload: "48000", UpdatedAt: audioFixtureNow}); err != nil {
		t.Fatalf("UpsertModelMappingCache: %v", err)
	}
	targets := f.targets(t, "speed", "map1")
	targets[0].audio = audioTarget{role: routing.RoleTTS, streaming: true}
	var mu sync.Mutex
	served := 0
	f.srv.audioMeasureSeam = func(_ context.Context, call audioMeasureCall) (audioMeasurement, error) {
		if f.agent.adminStateOf("rs_map1") == vramAdminStateForceStopped {
			f.events.add("audio %s 503", call.target.ProviderModel)
			return audioMeasurement{}, &audioMeasureError{Kind: audioMeasureUpstreamRefused, Status: http.StatusServiceUnavailable, Code: "runtime.admission_blocked", Message: "admission blocked: the spec is force-stopped"}
		}
		f.events.add("audio %s 200", call.target.ProviderModel)
		mu.Lock()
		served++
		n := served
		mu.Unlock()
		if n == 1 {
			return audioFigures(2.5, 5300), nil
		}
		return audioFigures(3, 300), nil
	}

	status := f.run(t, "speed", targets)

	if status.Error != "" || len(status.Results) != 1 {
		t.Fatalf("status = %+v, want one result and no run error", status)
	}
	if res := status.Results[0]; res.Error != "" || res.AudioLoadTimeMS != 5000 {
		t.Fatalf("result = load %d ms, error %q; want 5000 and no error", res.AudioLoadTimeMS, res.Error)
	}
	clear, refused := f.events.index("clear [rs_map1]", 0), f.events.index("audio m1 503", 0)
	if clear < 0 || refused < 0 || refused < clear {
		t.Fatalf("events = %v, want the cold pass refused after the clear was written", f.events.with("stop ", "clear ", "audio "))
	}
	if first := f.events.index("audio m1 200", 0); first < refused {
		t.Fatalf("events = %v, want the cold pass served only after its refusals", f.events.with("clear ", "audio "))
	}
	if got := f.events.count("audio m1 200"); got != 2 {
		t.Fatalf("served passes = %d, want 2 (cold and warm)", got)
	}
	if m, err := f.mem.MappingByID(context.Background(), "map1"); err != nil || m.LoadTimeMS != 5000 {
		t.Fatalf("stored load_time_ms = %d (%v), want 5000", m.LoadTimeMS, err)
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

// TestAudioCapacityJudgesALevelByItsSlowestStream: a level whose mean rate is real-time but
// whose slowest stream is not (0.9 and 3.0 at k=2, mean 1.95) is bad: one stream that
// stutters is a stuttering session, so the ramp stops at 1 with the limit performance.
func TestAudioCapacityJudgesALevelByItsSlowestStream(t *testing.T) {
	f := newAudioFixture(t, audioFixtureOpts{})
	f.script.answer = levelRates(map[int][]float64{1: {3.0}, 2: {0.9, 3.0}})

	res := f.run(t, "capacity")

	if res.Error != "" || res.AudioRealtimeConcurrency != 1 || res.AudioConcurrencyLimit != routing.AudioLimitPerformance {
		t.Fatalf("result = %d / %q, error %q; want 1 / performance", res.AudioRealtimeConcurrency, res.AudioConcurrencyLimit, res.Error)
	}
	if _, payload := f.capacityRow(t); len(payload.Curve) != 2 || payload.Curve[1] != (audioLevel{K: 2, MinRateX: 0.9, MeanRateX: 1.95}) {
		t.Fatalf("curve = %+v, want level 2 at 0.9 slowest, 1.95 mean", payload.Curve)
	}
}

// serializingProcess answers a ramp's streams the way one process that serves its requests
// one after another does: every stream produces its 20 s of audio at 3x real time once it
// starts, but stream i of a level (in the order the streams arrive) waits for the i
// syntheses before it, so its first output comes i x 6967 ms later than a lone stream's
// 300 ms. A streamed synthesis states that wait only in its time to first audio, a one-shot
// one in its rate as well (rate is given per stream by oneShotRates then). Warm-ups and
// speed passes are a lone stream's.
func serializingProcess(oneShotRates map[int][]float64) func(audioMeasureCall, int) (audioMeasurement, error) {
	var mu sync.Mutex
	seen := map[int]int{}
	return func(call audioMeasureCall, _ int) (audioMeasurement, error) {
		i := 0
		if !call.warmUp && call.level > 0 {
			mu.Lock()
			i = seen[call.level]
			seen[call.level]++
			mu.Unlock()
		}
		const audioS, productionMS, loneFirstMS = 20.0, 6667, 300
		first := int64(loneFirstMS + i*(productionMS+loneFirstMS))
		rate := 3.0
		if r := oneShotRates[call.level]; len(r) > 0 && !call.warmUp {
			rate = r[min(i, len(r)-1)]
		}
		return audioMeasurement{FirstByteMS: first, TTFTMS: first, WallMS: first + productionMS, AudioSeconds: audioS, RateX: rate, RTF: float64(first+productionMS) / 1000 / audioS}, nil
	}
}

// TestAudioCapacityCountsAStreamsWaitForItsFirstAudio: a streamed synthesis's rate is its
// speed once audio flows, so a stream that a process holds back behind the others would
// read as fast as a lone one. Within a ramp level, the time a stream waits for its first
// audio beyond the lone stream's (level 1's) is therefore counted as part of its production:
// a process that serializes 20 s syntheses at 3x keeps 2 streams real-time (the second one,
// 6967 ms late, makes 20 s of audio in 13.6 s) and not 3 (0.97x), so the ramp stops at 3 with
// 2 / performance instead of running to its cap. A one-shot synthesis's rate already runs
// from the send, so its streams are judged by their rates as they are.
func TestAudioCapacityCountsAStreamsWaitForItsFirstAudio(t *testing.T) {
	t.Run("a streamed synthesis", func(t *testing.T) {
		f := newAudioFixture(t, audioFixtureOpts{})
		f.script.answer = serializingProcess(nil)

		res := f.run(t, "capacity")

		if res.Error != "" || res.AudioRealtimeConcurrency != 2 || res.AudioConcurrencyLimit != routing.AudioLimitPerformance {
			t.Fatalf("result = %d / %q, error %q; want 2 / performance", res.AudioRealtimeConcurrency, res.AudioConcurrencyLimit, res.Error)
		}
		_, payload := f.capacityRow(t)
		if len(payload.Curve) != 3 {
			t.Fatalf("curve = %+v, want three levels", payload.Curve)
		}
		production := 20.0 / 3
		for _, want := range []audioLevel{
			{K: 1, MinRateX: 3, MeanRateX: 3},
			{K: 2, MinRateX: 20 / (production + 6.967), MeanRateX: (3 + 20/(production+6.967)) / 2},
			{K: 3, MinRateX: 20 / (production + 13.934)},
		} {
			got := payload.Curve[want.K-1]
			if math.Abs(got.MinRateX-want.MinRateX) > 1e-9 || (want.MeanRateX > 0 && math.Abs(got.MeanRateX-want.MeanRateX) > 1e-9) {
				t.Fatalf("level %d = %+v, want slowest %.4f (mean %.4f)", want.K, got, want.MinRateX, want.MeanRateX)
			}
		}
	})
	t.Run("a one-shot synthesis", func(t *testing.T) {
		f := newAudioFixture(t, audioFixtureOpts{caps: []routing.CapabilityRow{
			sbCap(routing.CapabilitySpeech, routing.CapabilityYes, audioFixtureNow),
			sbCap(routing.CapabilitySpeechClone, routing.CapabilityNo, audioFixtureNow),
			sbCap(routing.CapabilitySpeechStreaming, routing.CapabilityNo, audioFixtureNow),
		}})
		f.script.answer = serializingProcess(map[int][]float64{2: {3, 1.5}, 3: {3, 1.5, 0.9}})

		res := f.run(t, "capacity")

		if res.Error != "" || res.AudioRealtimeConcurrency != 2 || res.AudioConcurrencyLimit != routing.AudioLimitPerformance {
			t.Fatalf("result = %d / %q, error %q; want 2 / performance", res.AudioRealtimeConcurrency, res.AudioConcurrencyLimit, res.Error)
		}
		if _, payload := f.capacityRow(t); len(payload.Curve) != 3 || payload.Curve[1] != (audioLevel{K: 2, MinRateX: 1.5, MeanRateX: 2.25}) {
			t.Fatalf("curve = %+v, want level 2 judged by its rates as they are (1.5 slowest)", payload.Curve)
		}
	})
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

// TestLiveAudioResultServesTheRampsCountBesideItsLimit: the run's status -- what the status
// endpoint answers and each SSE snapshot and progress frame carries (json.Marshal of the
// BenchmarkStatus) -- serves a ramp's count whenever it serves the ramp's limit, a measured 0
// included: 0 with the limit performance is "not real-time even alone", a result, where a
// missing count would read as not measured. A failed ramp and a speed run serve neither.
// Every other field is encoded as before: the JSON decodes back into the same result.
func TestLiveAudioResultServesTheRampsCountBesideItsLimit(t *testing.T) {
	frame := func(t *testing.T, f *audioFixture) map[string]any {
		t.Helper()
		status := f.srv.Benchmarks.Status("srv1")
		data, err := json.Marshal(status)
		if err != nil {
			t.Fatalf("marshal the status: %v", err)
		}
		var st struct {
			Results []map[string]any `json:"results"`
		}
		var back BenchmarkStatus
		if err := json.Unmarshal(data, &st); err != nil || len(st.Results) != 1 {
			t.Fatalf("status JSON %s: %v; want one result", data, err)
		}
		if err := json.Unmarshal(data, &back); err != nil || !reflect.DeepEqual(back.Results, status.Results) {
			t.Fatalf("status JSON %s decodes to %+v (%v), want %+v", data, back.Results, err, status.Results)
		}
		return st.Results[0]
	}
	for _, tc := range []struct {
		name      string
		mode      string
		answer    func(audioMeasureCall, int) (audioMeasurement, error)
		wantCount any // nil: the key is absent
		wantLimit any
	}{
		{"a measured 0", "capacity", levelRates(map[int][]float64{1: {0.8}}), 0.0, routing.AudioLimitPerformance},
		{"a measured 2", "capacity", levelRates(map[int][]float64{1: {3}, 2: {2}, 3: {1}}), 2.0, routing.AudioLimitPerformance},
		{"a failed ramp", "capacity", func(call audioMeasureCall, _ int) (audioMeasurement, error) {
			if call.level == 1 && !call.warmUp {
				return audioMeasurement{}, &audioMeasureError{Kind: audioMeasureTransport, Message: "connection refused"}
			}
			return audioFigures(10, 100), nil
		}, nil, nil},
		{"a speed run", "speed", nil, nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAudioFixture(t, audioFixtureOpts{})
			if tc.answer != nil {
				f.script.answer = tc.answer
			}
			f.run(t, tc.mode)
			res := frame(t, f)
			count, hasCount := res["audio_realtime_concurrency"]
			if (tc.wantCount == nil) == hasCount || (hasCount && count != tc.wantCount) || res["audio_concurrency_limit"] != tc.wantLimit {
				t.Fatalf("result JSON = %v, want audio_realtime_concurrency %v beside audio_concurrency_limit %v", res, tc.wantCount, tc.wantLimit)
			}
		})
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
	t.Run("the agent's instance index cap", func(t *testing.T) {
		// A ramp cap above the agent's instance index cap (63), with no max_instances, stops a
		// ramp by instance at 64 instances, m#63 the last: m#64 is no address the agent serves.
		f := newAudioFixture(t, audioFixtureOpts{agent: true, replicable: true, features: []string{"runtime_manager", runtimeInstancesFeature}})
		f.srv.audioConcurrencyMax = 100

		res := f.run(t, "capacity")

		if res.Error != "" || res.AudioRealtimeConcurrency != routing.MaxInstanceIndex+1 || res.AudioConcurrencyLimit != routing.AudioLimitCap {
			t.Fatalf("result = %d / %q, error %q; want %d / cap", res.AudioRealtimeConcurrency, res.AudioConcurrencyLimit, res.Error, routing.MaxInstanceIndex+1)
		}
		got := f.provider.ensuredAddresses()
		if len(got) != routing.MaxInstanceIndex+1 || got[len(got)-1] != "m#"+strconv.Itoa(routing.MaxInstanceIndex) {
			t.Fatalf("ensured %d addresses ending %q, want %d ending m#%d", len(got), got[len(got)-1], routing.MaxInstanceIndex+1, routing.MaxInstanceIndex)
		}
	})
	t.Run("the index cap does not cap one process", func(t *testing.T) {
		f := newAudioFixture(t, audioFixtureOpts{})
		f.srv.audioConcurrencyMax = routing.MaxInstanceIndex + 3

		if res := f.run(t, "capacity"); res.Error != "" || res.AudioRealtimeConcurrency != routing.MaxInstanceIndex+3 || res.AudioConcurrencyLimit != routing.AudioLimitCap {
			t.Fatalf("result = %d / %q, error %q; want %d / cap", res.AudioRealtimeConcurrency, res.AudioConcurrencyLimit, res.Error, routing.MaxInstanceIndex+3)
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

// TestAudioCapacityNonReplicableSingleProcess: a target that is not replicable -- a spec
// that is not replicable, or a direct audio.cpp application -- gets k concurrent streams on
// its one process, and no instance is ever ensured. max_instances does not cap that ramp.
func TestAudioCapacityNonReplicableSingleProcess(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts audioFixtureOpts
	}{
		{"an agent spec that is not replicable", audioFixtureOpts{agent: true, features: []string{"runtime_manager", runtimeInstancesFeature}}},
		{"a direct audio.cpp application", audioFixtureOpts{}},
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

// TestAudioCapacityReplicableNeedsInstances: the ramp of a replicable spec counts instances,
// and its result is the scaler's ceiling on the instance count. Where the agent is not known
// to run instances -- not heard from since the gateway started, or without runtime_instances
// -- a ramp inside one process would store that process's stream count (1 for a server that
// serializes) as the ceiling, so the ramp is refused: nothing is sent, nothing is ensured, and
// the mapping's pair stays unknown.
func TestAudioCapacityReplicableNeedsInstances(t *testing.T) {
	for _, tc := range []struct {
		name     string
		features []string
		want     string
	}{
		{"an agent not heard from", nil, "has not reported"},
		{"an agent without runtime_instances", []string{"runtime_manager"}, "does not run them"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAudioFixture(t, audioFixtureOpts{agent: true, replicable: true, maxInstances: 4, features: tc.features})
			f.script.answer = levelRates(map[int][]float64{1: {4}, 2: {0.9}})

			res := f.run(t, "capacity")

			if !strings.Contains(res.Error, tc.want) || res.AudioConcurrencyLimit != "" {
				t.Fatalf("result = %d / %q, error %q; want no result and an error naming %q", res.AudioRealtimeConcurrency, res.AudioConcurrencyLimit, res.Error, tc.want)
			}
			for _, c := range f.script.recorded() {
				if c.level > 0 {
					t.Fatalf("a ramp stream was sent (level %d to %q), want none", c.level, c.model)
				}
			}
			if got := f.provider.ensuredAddresses(); len(got) != 0 {
				t.Fatalf("ensured = %v, want none", got)
			}
			if m := f.mapping(t); m.AudioConcurrencyLimit != "" {
				t.Fatalf("stored pair = %d / %q, want it unknown", m.AudioRealtimeConcurrency, m.AudioConcurrencyLimit)
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

// TestAudioBothRunMeasuresTheByteRateOnce: a streamed synthesis is counted with the mapping's
// output byte rate, and when none is cached the speech benchmark runs first to measure it.
// In a both run whose speech benchmark fails, the speed part fails with that error, and the
// capacity part fails with the same one: the outcome is the run's, and the failing request
// is not sent a second time (one kind=speech history row, and no measurement but the cold
// pass, which is taken before the benchmark and counts no audio).
func TestAudioBothRunMeasuresTheByteRateOnce(t *testing.T) {
	f := newAudioFixture(t, audioFixtureOpts{noByteRate: true})

	res := f.run(t, "both")

	if res.Error == "" || !strings.Contains(res.Error, "output byte rate") {
		t.Fatalf("result = %+v, want the byte rate's failure", res)
	}
	rows := f.rows(t)
	if len(rows[benchmarkKindSpeech]) != 1 {
		t.Fatalf("speech history rows = %d, want the one speech benchmark the run sent", len(rows[benchmarkKindSpeech]))
	}
	audio, ramp := rows[benchmarkKindAudio], rows[benchmarkKindAudioCapacity]
	if len(audio) != 1 || len(ramp) != 1 || audio[0].Error == "" || ramp[0].Error != audio[0].Error {
		t.Fatalf("audio rows = %+v, ramp rows = %+v; want one failed row each, with the same error", audio, ramp)
	}
	if got := f.script.recorded(); len(got) != 1 || got[0].kind != audioKindTTSStream || got[0].level != 0 {
		t.Fatalf("measurements = %+v, want the speed part's cold pass only", got)
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
// starts nothing, whatever the first run did, and is told the mapping is settled. A mapping
// with metrics, a replicable one with a ramp result (a measured 0 included) and a locked one
// never start one; each is deferred, which the loop's own rule already keeps it from asking
// about again.
func TestAutoAudioRunOnceWhenMissing(t *testing.T) {
	stamp := audioFixtureNow
	for _, tc := range []struct {
		name       string
		replicable bool
		metricsAt  *time.Time
		limit      string
		locked     bool
		want       AudioAutoRunAnswer
	}{
		{"no audio metrics yet", false, nil, "", false, AudioAutoRunStarted},
		{"replicable without a ramp result", true, &stamp, "", false, AudioAutoRunStarted},
		{"replicable with a measured 0", true, &stamp, routing.AudioLimitPerformance, false, AudioAutoRunDeferred},
		{"metrics present, not replicable", false, &stamp, "", false, AudioAutoRunDeferred},
		{"locked", false, nil, "", true, AudioAutoRunDeferred},
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
			if first != AudioAutoRunStarted {
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
			if second := f.srv.TriggerAudioBenchmark(ctx, f.server, f.app, f.mapping(t)); second != AudioAutoRunSettled {
				t.Fatalf("second tick = %v, want settled: the mapping's one run was started", second)
			}
		})
	}
	t.Run("a failed run is not started again", func(t *testing.T) {
		f := newAudioFixture(t, audioFixtureOpts{agent: true, features: []string{"runtime_manager"}})
		f.script.answer = func(audioMeasureCall, int) (audioMeasurement, error) {
			return audioMeasurement{}, &audioMeasureError{Kind: audioMeasureTransport, Message: "connection refused"}
		}
		ctx := context.Background()
		if got := f.srv.TriggerAudioBenchmark(ctx, f.server, f.app, f.mapping(t)); got != AudioAutoRunStarted {
			t.Fatalf("first tick = %v, want started", got)
		}
		waitFor(t, func() bool { return !f.srv.Benchmarks.Status("srv1").Running })
		if got := f.srv.TriggerAudioBenchmark(ctx, f.server, f.app, f.mapping(t)); got != AudioAutoRunSettled {
			t.Fatalf("second tick = %v, want settled: the failed mapping is not started again", got)
		}
	})
	t.Run("a spec made replicable since its run is measured again", func(t *testing.T) {
		// The run is once per mapping and shape: the operator's flip has the portal clear the
		// ramp result, which counted streams inside one process, and the spec's instances are
		// then measured -- without waiting for a gateway restart.
		f := newAudioFixture(t, audioFixtureOpts{agent: true, maxInstances: 4, features: []string{"runtime_manager", runtimeInstancesFeature}})
		ctx := context.Background()
		if got := f.srv.TriggerAudioBenchmark(ctx, f.server, f.app, f.mapping(t)); got != AudioAutoRunStarted {
			t.Fatalf("first tick = %v, want started", got)
		}
		waitFor(t, func() bool { return !f.srv.Benchmarks.Status("srv1").Running })
		spec, ok, err := f.mem.RuntimeSpecByMapping(ctx, "map1")
		if err != nil || !ok {
			t.Fatalf("RuntimeSpecByMapping: %v, %v", ok, err)
		}
		spec.Replicable = true
		if err := f.mem.UpsertRuntimeSpec(ctx, spec); err != nil {
			t.Fatalf("UpsertRuntimeSpec: %v", err)
		}
		if err := f.mem.ClearMappingAudioConcurrency(ctx, "map1"); err != nil {
			t.Fatalf("ClearMappingAudioConcurrency: %v", err)
		}
		if got := f.srv.TriggerAudioBenchmark(ctx, f.server, f.app, f.mapping(t)); got != AudioAutoRunStarted {
			t.Fatalf("tick after the spec became replicable = %v, want started", got)
		}
		waitFor(t, func() bool { return !f.srv.Benchmarks.Status("srv1").Running })
		if got := f.srv.TriggerAudioBenchmark(ctx, f.server, f.app, f.mapping(t)); got != AudioAutoRunSettled && got != AudioAutoRunDeferred {
			t.Fatalf("tick after the second run = %v, want no third run", got)
		}
	})
	t.Run("an agent not heard from since the gateway started defers", func(t *testing.T) {
		// Whether its replicable spec is ramped by instance depends on the agent's
		// declared features, so the run waits until they are known.
		f := newAudioFixture(t, audioFixtureOpts{agent: true, replicable: true})
		ctx := context.Background()
		if got := f.srv.TriggerAudioBenchmark(ctx, f.server, f.app, f.mapping(t)); got != AudioAutoRunDeferred {
			t.Fatalf("answer before the agent's features were known = %v, want deferred", got)
		}
		f.srv.AgentFeatures.Set("srv1", []string{"runtime_manager", runtimeInstancesFeature})
		if got := f.srv.TriggerAudioBenchmark(ctx, f.server, f.app, f.mapping(t)); got != AudioAutoRunStarted {
			t.Fatalf("answer once the features were known = %v, want started", got)
		}
		waitFor(t, func() bool { return !f.srv.Benchmarks.Status("srv1").Running })
	})
	t.Run("requests in flight defer without using up the one run", func(t *testing.T) {
		// The idle gate: the trigger takes the reservation, sees the server's live traffic,
		// gives the reservation back and starts nothing; once the server is idle it starts.
		f := newAudioFixture(t, audioFixtureOpts{agent: true, features: []string{"runtime_manager"}})
		ctx := context.Background()
		f.srv.Active.Add(ActiveRequest{ID: "req_live", ServerName: f.server.Name, APIFlavor: "openai_audio", StartedAt: time.Now()})
		if got := f.srv.TriggerAudioBenchmark(ctx, f.server, f.app, f.mapping(t)); got != AudioAutoRunDeferred {
			t.Fatalf("answer on a server with a request in flight = %v, want deferred", got)
		}
		if f.srv.Benchmarks.ServerBusy("srv1") || len(f.script.recorded()) != 0 {
			t.Fatal("the deferred trigger kept the reservation or measured")
		}
		f.srv.Active.Remove("req_live")
		if got := f.srv.TriggerAudioBenchmark(ctx, f.server, f.app, f.mapping(t)); got != AudioAutoRunStarted {
			t.Fatalf("answer once the server was idle = %v, want started", got)
		}
		waitFor(t, func() bool { return !f.srv.Benchmarks.Status("srv1").Running })
	})
	t.Run("a busy server defers without using up the one run", func(t *testing.T) {
		f := newAudioFixture(t, audioFixtureOpts{agent: true, features: []string{"runtime_manager"}})
		ctx := context.Background()
		if _, ok := f.srv.Benchmarks.TryStart("srv1", "mapping", "speed", 1, time.Now().UTC(), func() {}); !ok {
			t.Fatal("TryStart")
		}
		if got := f.srv.TriggerAudioBenchmark(ctx, f.server, f.app, f.mapping(t)); got != AudioAutoRunDeferred {
			t.Fatalf("answer on a busy server = %v, want deferred", got)
		}
		f.srv.Benchmarks.Release("srv1")
		if got := f.srv.TriggerAudioBenchmark(ctx, f.server, f.app, f.mapping(t)); got != AudioAutoRunStarted {
			t.Fatalf("answer once the server was free = %v, want started", got)
		}
		waitFor(t, func() bool { return !f.srv.Benchmarks.Status("srv1").Running })
	})
}

// TestAutoAudioRunSettlesWhatItWillAlwaysRefuse: the trigger tells the health loop which
// refusals to remember. A mapping it refuses for what the mapping is -- a chat model with an
// audio verdict (it keeps its text benchmark), a clone model with no preset voice -- is
// settled: asking again would be refused again, so the loop stops asking. A mapping with no
// speech or transcription verdict yet is deferred: the verdict arrives by itself. None of them
// reserves the server or is measured.
func TestAutoAudioRunSettlesWhatItWillAlwaysRefuse(t *testing.T) {
	yes := routing.CapabilityYes
	chatApp := func(app routing.Application) routing.Application {
		app.APIFlavors = []string{routing.APIFlavorOpenAI, routing.APIFlavorOpenAIAudio}
		return app
	}
	same := func(app routing.Application) routing.Application { return app }
	for _, tc := range []struct {
		name string
		caps []routing.CapabilityRow
		app  func(routing.Application) routing.Application
		want AudioAutoRunAnswer
	}{
		{"a chat model that also transcribes", []routing.CapabilityRow{sbCap(routing.CapabilityTranscription, yes, audioFixtureNow)}, chatApp, AudioAutoRunSettled},
		{"a clone model without a preset voice", []routing.CapabilityRow{sbCap(routing.CapabilitySpeech, yes, audioFixtureNow), sbCap(routing.CapabilitySpeechClone, yes, audioFixtureNow)}, same, AudioAutoRunSettled},
		{"no speech or transcription verdict yet", []routing.CapabilityRow{sbCap(routing.CapabilitySpeech, routing.CapabilityNo, audioFixtureNow)}, same, AudioAutoRunDeferred},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAudioFixture(t, audioFixtureOpts{caps: tc.caps})
			if got := f.srv.TriggerAudioBenchmark(context.Background(), f.server, tc.app(f.app), f.mapping(t)); got != tc.want {
				t.Fatalf("answer = %v, want %v", got, tc.want)
			}
			if f.srv.Benchmarks.ServerBusy("srv1") || len(f.script.recorded()) != 0 {
				t.Fatal("a refused mapping reserved the server or was measured")
			}
		})
	}
	t.Run("a clone model whose presets are cached later is started then", func(t *testing.T) {
		f := newAudioFixture(t, audioFixtureOpts{caps: []routing.CapabilityRow{sbCap(routing.CapabilitySpeech, yes, audioFixtureNow), sbCap(routing.CapabilitySpeechClone, yes, audioFixtureNow)}})
		ctx := context.Background()
		if got := f.srv.TriggerAudioBenchmark(ctx, f.server, f.app, f.mapping(t)); got != AudioAutoRunSettled {
			t.Fatalf("answer without presets = %v, want settled", got)
		}
		if err := f.mem.UpsertModelMappingCache(ctx, routing.ModelMappingCacheRow{MappingID: "map1", CacheType: routing.ModelMappingCacheVoices, Payload: `["alba"]`, UpdatedAt: audioFixtureNow}); err != nil {
			t.Fatalf("UpsertModelMappingCache: %v", err)
		}
		if got := f.srv.TriggerAudioBenchmark(ctx, f.server, f.app, f.mapping(t)); got != AudioAutoRunStarted {
			t.Fatalf("answer with a preset cached = %v, want started: the gateway remembers no refusal itself", got)
		}
		waitFor(t, func() bool { return !f.srv.Benchmarks.Status("srv1").Running })
	})
}

// TestAudioHistoryRoundTripsThroughThePortal: the history rows the gateway writes are read by
// the portal's history endpoint into the figures the gateway stored, through the one payload
// definition both share. A speed row serves its rate; a ramp that measured 0 -- not real-time
// even alone, the limit performance -- serves the count 0 beside its limit and curve, where an
// omitted count would read as unknown; and a ramp that failed serves neither count nor limit.
func TestAudioHistoryRoundTripsThroughThePortal(t *testing.T) {
	f := newAudioFixture(t, audioFixtureOpts{})
	f.script.answer = levelRates(map[int][]float64{1: {0.8}})
	if res := f.run(t, "both"); res.Error != "" || res.AudioConcurrencyLimit != routing.AudioLimitPerformance {
		t.Fatalf("both run = %+v, want a ramp that stopped on performance", res)
	}
	f.srv.Benchmarks.Release("srv1")
	f.script.answer = func(call audioMeasureCall, _ int) (audioMeasurement, error) {
		if call.level == 2 {
			return audioMeasurement{}, &audioMeasureError{Kind: audioMeasureTransport, Message: "connection reset"}
		}
		return audioFigures(5, 100), nil
	}
	if res := f.run(t, "capacity"); res.Error == "" {
		t.Fatalf("capacity run = %+v, want a failed ramp", res)
	}
	svc := portal.NewService(portal.ServiceDeps{Routes: f.mem})
	runs, err := svc.MappingBenchmarks(context.Background(), auth.Token{Scopes: []string{"gateway:use", "admin", "system"}}, "map1", 50)
	if err != nil {
		t.Fatalf("MappingBenchmarks: %v", err)
	}
	var speed, measured0, failed map[string]any
	for _, r := range runs {
		raw, err := json.Marshal(r)
		if err != nil {
			t.Fatalf("marshal %+v: %v", r, err)
		}
		var row map[string]any
		if err := json.Unmarshal(raw, &row); err != nil {
			t.Fatalf("unmarshal %s: %v", raw, err)
		}
		switch {
		case r.Kind == benchmarkKindAudio:
			speed = row
		case r.Kind == benchmarkKindAudioCapacity && r.Error == "":
			measured0 = row
		case r.Kind == benchmarkKindAudioCapacity:
			failed = row
		}
	}
	if speed == nil || speed["audio_rate_x"] != 10.0 {
		t.Fatalf("speed row = %v, want audio_rate_x 10", speed)
	}
	count, ok := measured0["audio_realtime_concurrency"]
	if !ok || count != 0.0 || measured0["audio_concurrency_limit"] != routing.AudioLimitPerformance {
		t.Fatalf("measured-0 ramp row = %v, want audio_realtime_concurrency 0 beside the limit performance", measured0)
	}
	if curve, _ := measured0["audio_curve"].([]any); len(curve) != 1 || !reflect.DeepEqual(curve[0], map[string]any{"k": 1.0, "min_rate_x": 0.8, "mean_rate_x": 0.8}) {
		t.Fatalf("measured-0 ramp row curve = %v, want its one level", measured0["audio_curve"])
	}
	if failed == nil {
		t.Fatalf("history = %+v, want a failed ramp row", runs)
	}
	if _, ok := failed["audio_realtime_concurrency"]; ok || failed["audio_concurrency_limit"] != nil {
		t.Fatalf("failed ramp row = %v, want neither a count nor a limit", failed)
	}
}

// TestAChatModelWithAnAudioVerdictKeepsItsTextBenchmark: a mapping that serves chat (a text
// flavor) is a chat model even when it carries a speech or transcription verdict as well -- an
// omni model that also transcribes, say -- so the speed, capacity and both modes measure it
// with the text benchmark exactly as before, and never read its capability rows for it. The
// same verdict on a mapping that serves audio only makes it an audio target.
func TestAChatModelWithAnAudioVerdictKeepsItsTextBenchmark(t *testing.T) {
	yes := routing.CapabilityYes
	transcriber := []routing.CapabilityRow{sbCap(routing.CapabilityTranscription, yes, audioFixtureNow), sbCap(routing.CapabilityAudio, yes, audioFixtureNow)}
	speaker := []routing.CapabilityRow{sbCap(routing.CapabilitySpeech, yes, audioFixtureNow), sbCap(routing.CapabilitySpeechStreaming, yes, audioFixtureNow)}
	chatAndAudio := []string{routing.APIFlavorOpenAI, routing.APIFlavorOpenAIAudio}
	audioOnly := []string{routing.APIFlavorOpenAIAudio}
	for _, tc := range []struct {
		name        string
		agent       bool
		appFlavors  []string
		specFlavors []string
		specType    routing.RuntimeSpecType
		caps        []routing.CapabilityRow
		wantRole    routing.ModelRole
	}{
		{"an omni model on a direct application", false, chatAndAudio, nil, "", transcriber, ""},
		{"an omni model the agent runs", true, chatAndAudio, chatAndAudio, routing.RuntimeSpecTypeVLLM, transcriber, ""},
		{"a speech model that also serves the messages API", false, []string{routing.APIFlavorAnthropic, routing.APIFlavorOpenAIAudio}, nil, "", speaker, ""},
		{"a transcriber that serves audio only", false, audioOnly, nil, "", transcriber, routing.RoleASR},
		{"an agent child narrowed to audio", true, chatAndAudio, audioOnly, routing.RuntimeSpecTypeVLLM, transcriber, routing.RoleASR},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAudioFixture(t, audioFixtureOpts{agent: tc.agent, caps: tc.caps, features: []string{"runtime_manager"}})
			ctx := context.Background()
			f.app.APIFlavors = tc.appFlavors
			if !tc.agent {
				f.app.Type = routing.ProviderMock
			}
			if err := f.mem.UpdateApplication(ctx, f.app); err != nil {
				t.Fatalf("UpdateApplication: %v", err)
			}
			if tc.agent {
				spec, _, err := f.mem.RuntimeSpecByMapping(ctx, "map1")
				if err != nil {
					t.Fatalf("RuntimeSpecByMapping: %v", err)
				}
				spec.APIFlavors, spec.Type = tc.specFlavors, string(tc.specType)
				if err := f.mem.UpsertRuntimeSpec(ctx, spec); err != nil {
					t.Fatalf("UpsertRuntimeSpec: %v", err)
				}
			}
			tgt, skipped, err := f.srv.benchmarkViewTarget(ctx, portal.BenchmarkTargetView{Server: f.server, App: f.app, Mapping: f.mapping(t)}, true)
			if err != nil || skipped != "" || tgt.mapping.ID != "map1" || tgt.audio.role != tc.wantRole {
				t.Fatalf("benchmarkViewTarget = role %q, skipped %q, err %v; want a target with role %q", tgt.audio.role, skipped, err, tc.wantRole)
			}
			if tc.wantRole != "" {
				return
			}
			chat := newColdLister(nil)
			f.srv.Provider = chat
			run, ok := f.srv.Benchmarks.TryStart("srv1", "mapping", "speed", 1, time.Now().UTC(), func() {})
			if !ok {
				t.Fatal("TryStart did not start")
			}
			f.srv.runBenchmark(ctx, run, "srv1", []benchmarkTarget{tgt}, "speed")
			if res := f.srv.Benchmarks.Status("srv1").Results; len(res) != 1 || res[0].Error != "" || res[0].GenTokensPerSecond == 0 || res[0].AudioRateX != 0 {
				t.Fatalf("results = %+v, want one text speed result", res)
			}
			if rows := f.rows(t); len(rows["speed"]) != 1 || len(rows[benchmarkKindAudio]) != 0 {
				t.Fatalf("history = %v, want one speed row and no audio row", rows)
			}
			if got := f.script.recorded(); len(got) != 0 {
				t.Fatalf("audio measurements = %+v, want none", got)
			}
			if got := chat.streamedModels(); !slices.Contains(got, "m") {
				t.Fatalf("streamed %v, want the chat prompt to m", got)
			}
		})
	}
}

// audioPartitionStore fails, per mapping id, the three reads the speed, capacity and both
// modes classify a view with: its runtime spec, its capability rows and its voices cache.
type audioPartitionStore struct {
	routing.Store
	failSpec, failCaps, failVoices map[string]bool
}

func (s audioPartitionStore) RuntimeSpecByMapping(ctx context.Context, mappingID string) (routing.RuntimeSpec, bool, error) {
	if s.failSpec[mappingID] {
		return routing.RuntimeSpec{}, false, errors.New("spec read down")
	}
	return s.Store.RuntimeSpecByMapping(ctx, mappingID)
}

func (s audioPartitionStore) MappingCapabilities(ctx context.Context, mappingID string) ([]routing.CapabilityRow, error) {
	if s.failCaps[mappingID] {
		return nil, errors.New("capability read down")
	}
	return s.Store.MappingCapabilities(ctx, mappingID)
}

func (s audioPartitionStore) ModelMappingCache(ctx context.Context, mappingID, cacheType string) ([]routing.ModelMappingCacheRow, error) {
	if cacheType == routing.ModelMappingCacheVoices && s.failVoices[mappingID] {
		return nil, errors.New("voices read down")
	}
	return s.Store.ModelMappingCache(ctx, mappingID, cacheType)
}

// TestAudioPartitionSortsEveryView covers every branch of the speed, capacity and both modes'
// partition over one agent application: a chat child is a text target and a speech child an
// audio target; an images-only child is skipped as images_only; a clone model without a
// preset voice (speech_clone) and an audio child with no speech or transcription verdict
// (audio_only) are skipped as audio the run cannot measure; and a child whose runtime spec,
// capability rows or voices cache cannot be read is recorded with that read's error, in view
// order. The vision probe's partition of the same views skips every audio-only child as
// audio_only without reading its capability rows. With nothing runnable, a scope of
// unmeasurable audio answers 409 benchmark.speech_unavailable with the audio message, one
// that mixes it with images-only views the same code with the message that names both
// kinds, one of images-only views alone 409 benchmark.images_only, one with an unreadable
// view 500 benchmark.request_failed, and the vision probe 409 benchmark.audio_only.
func TestAudioPartitionSortsEveryView(t *testing.T) {
	withCapturedSlogAtTheDefaultLevel(t)
	ctx := context.Background()
	mem := routing.NewMemoryStore()
	must := func(what string, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}
	server := routing.AIServer{ID: "srv1", Name: "Host", Domain: "host.example.test", Provider: routing.ProviderMock, Endpoint: "mock://srv1", Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: audioFixtureNow, UpdatedAt: audioFixtureNow}
	app := routing.Application{ID: "app1", ServerID: "srv1", Type: routing.ProviderServerAgent, Port: 8081, Scheme: "http", TimeoutMS: 30000, APIFlavors: []string{routing.APIFlavorOpenAI, routing.APIFlavorOpenAIAudio, routing.APIFlavorOpenAIImages}, Status: routing.ServerStatusActive, CreatedAt: audioFixtureNow, UpdatedAt: audioFixtureNow}
	must("CreateAIServer", mem.CreateAIServer(ctx, server))
	must("CreateApplication", mem.CreateApplication(ctx, app))
	yes := routing.CapabilityYes
	clone := []routing.CapabilityRow{sbCap(routing.CapabilitySpeech, yes, audioFixtureNow), sbCap(routing.CapabilitySpeechClone, yes, audioFixtureNow)}
	children := []struct {
		id      string
		flavors []string
		caps    []routing.CapabilityRow
	}{
		{"p1_text", []string{routing.APIFlavorOpenAI}, nil},
		{"p2_tts", []string{routing.APIFlavorOpenAIAudio}, ttsStreamingCaps()},
		{"p3_img", []string{routing.APIFlavorOpenAIImages}, nil},
		{"p4_clone", []string{routing.APIFlavorOpenAIAudio}, clone},
		{"p5_none", []string{routing.APIFlavorOpenAIAudio}, nil},
		{"p6_spec", []string{routing.APIFlavorOpenAIAudio}, ttsStreamingCaps()},
		{"p7_caps", []string{routing.APIFlavorOpenAIAudio}, ttsStreamingCaps()},
		{"p8_voices", []string{routing.APIFlavorOpenAIAudio}, clone},
	}
	views := map[string]portal.BenchmarkTargetView{}
	for _, c := range children {
		mp := routing.ModelMapping{ID: c.id, ApplicationID: app.ID, GatewayModelName: "gw-" + c.id, AppModelName: c.id, Status: routing.ServerStatusActive, CreatedAt: audioFixtureNow, UpdatedAt: audioFixtureNow}
		must("CreateMapping "+c.id, mem.CreateMapping(ctx, mp))
		must("UpsertRuntimeSpec "+c.id, mem.UpsertRuntimeSpec(ctx, routing.RuntimeSpec{ID: "rs_" + c.id, MappingID: c.id, Enabled: true, Binary: "/opt/bin/server", Args: "[]", Env: "{}", HealthPath: "/health", APIFlavors: c.flavors, CreatedAt: audioFixtureNow, UpdatedAt: audioFixtureNow}))
		if c.caps != nil {
			must("UpsertMappingCapabilities "+c.id, mem.UpsertMappingCapabilities(ctx, c.id, c.caps))
		}
		views[c.id] = portal.BenchmarkTargetView{Server: server, App: app, Mapping: mp}
	}
	srv := &Server{
		Routes:        audioPartitionStore{Store: mem, failSpec: map[string]bool{"p6_spec": true}, failCaps: map[string]bool{"p7_caps": true}, failVoices: map[string]bool{"p8_voices": true}},
		Benchmarks:    NewBenchmarkRegistry(),
		AgentFeatures: NewAgentFeaturesRegistry(),
	}
	scope := func(ids ...string) []portal.BenchmarkTargetView {
		out := make([]portal.BenchmarkTargetView, 0, len(ids))
		for _, id := range ids {
			out = append(out, views[id])
		}
		return out
	}
	all := scope("p1_text", "p2_tts", "p3_img", "p4_clone", "p5_none", "p6_spec", "p7_caps", "p8_voices")

	t.Run("speed, capacity and both", func(t *testing.T) {
		for _, mode := range []string{"speed", "capacity", "both"} {
			p := srv.partitionBenchmarkViewsFor(ctx, all, mode)
			if len(p.runnable) != 2 || p.runnable[0].mapping.ID != "p1_text" || p.runnable[0].audio.role != "" || p.runnable[1].mapping.ID != "p2_tts" || p.runnable[1].audio.role != routing.RoleTTS {
				t.Fatalf("%s: runnable = %+v, want p1_text as text and p2_tts as a TTS target", mode, p.runnable)
			}
			want := []BenchmarkResult{
				{MappingID: "p3_img", GatewayModelName: "gw-p3_img", Skipped: benchmarkSkippedImagesOnly},
				{MappingID: "p4_clone", GatewayModelName: "gw-p4_clone", Skipped: benchmarkSkippedSpeechClone},
				{MappingID: "p5_none", GatewayModelName: "gw-p5_none", Skipped: benchmarkSkippedAudioOnly},
				{MappingID: "p6_spec", GatewayModelName: "gw-p6_spec", Error: benchmarkSpecUnreadable + "spec read down"},
				{MappingID: "p7_caps", GatewayModelName: "gw-p7_caps", Error: "audio benchmark: capability unreadable; not benchmarked: capability read down"},
				{MappingID: "p8_voices", GatewayModelName: "gw-p8_voices", Error: "audio benchmark: voices cache unreadable; not benchmarked: voices read down"},
			}
			if !reflect.DeepEqual(p.recorded, want) {
				t.Fatalf("%s: recorded = %+v, want %+v", mode, p.recorded, want)
			}
			if p.unreadable != 3 || p.imagesOnly != 1 || p.audioUnmeasurable != 2 || p.audioOnly != 0 {
				t.Fatalf("%s: counts = unreadable %d, images %d, unmeasurable %d, audio-only %d; want 3, 1, 2, 0", mode, p.unreadable, p.imagesOnly, p.audioUnmeasurable, p.audioOnly)
			}
		}
	})
	t.Run("the vision probe", func(t *testing.T) {
		p := srv.partitionBenchmarkViewsFor(ctx, all, "vision")
		if len(p.runnable) != 1 || p.runnable[0].mapping.ID != "p1_text" {
			t.Fatalf("runnable = %+v, want p1_text alone", p.runnable)
		}
		if p.unreadable != 1 || p.imagesOnly != 1 || p.audioOnly != 5 || p.audioUnmeasurable != 0 {
			t.Fatalf("counts = unreadable %d, images %d, audio-only %d, unmeasurable %d; want 1, 1, 5, 0", p.unreadable, p.imagesOnly, p.audioOnly, p.audioUnmeasurable)
		}
	})
	for _, tc := range []struct {
		name      string
		mode      string
		ids       []string
		status    int
		code, msg string
	}{
		{"unmeasurable audio only", "speed", []string{"p4_clone", "p5_none"}, http.StatusConflict, codeBenchmarkSpeechUnavailable, msgBenchmarkAudioUnmeasurable},
		{"unmeasurable audio beside an unreadable view", "both", []string{"p4_clone", "p7_caps"}, http.StatusInternalServerError, codeBenchmarkRequestFailed, msgBenchmarkRequestFailed},
		{"unmeasurable audio beside an images-only view", "capacity", []string{"p3_img", "p5_none"}, http.StatusConflict, codeBenchmarkSpeechUnavailable, msgBenchmarkScopeUnmeasurable},
		{"a clone without a preset beside an images-only view", "speed", []string{"p4_clone", "p3_img"}, http.StatusConflict, codeBenchmarkSpeechUnavailable, msgBenchmarkScopeUnmeasurable},
		{"images-only views alone", "both", []string{"p3_img"}, http.StatusConflict, codeBenchmarkImagesOnly, msgBenchmarkImagesOnly},
		{"audio-only views in the vision probe", "vision", []string{"p2_tts", "p5_none"}, http.StatusConflict, codeBenchmarkAudioOnly, msgBenchmarkAudioOnly},
	} {
		t.Run("nothing runnable: "+tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			if !srv.partitionBenchmarkViewsFor(ctx, scope(tc.ids...), tc.mode).refuseNothingRunnableFor(rec, tc.mode) {
				t.Fatal("refuseNothingRunnableFor = false, want a refusal")
			}
			var body struct {
				Error struct{ Code, Message string } `json:"error"`
			}
			_ = json.Unmarshal(rec.Body.Bytes(), &body)
			if rec.Code != tc.status || body.Error.Code != tc.code || body.Error.Message != tc.msg {
				t.Fatalf("answer = %d %q %q, want %d %q %q", rec.Code, body.Error.Code, body.Error.Message, tc.status, tc.code, tc.msg)
			}
		})
	}
}
