// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/apierror"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/routing"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

// The decisions-only tests are the decision twins of the audio-only ones
// (benchmark_refusals_test.go and siblings): a mapping that serves only the
// decisions flavor answers no chat prompt, so every path that would send one
// refuses or skips it, always after the images and audio checks.

// addDecisionsOnlyMappings extends the refusal fixture, apart from
// newRefusalFixture's own so no existing case's counts move:
//   - on rf_agent: rd_dec ([decisions]), rd_mixed ([openai, decisions]) and
//     rd_audiodec ([openai_audio, decisions]), each with a spec;
//   - a non-agent application rd_decapp ([decisions]) with the mapping rd_ext
//     and no spec.
func (f *refusalFixture) addDecisionsOnlyMappings(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	must := func(what string, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}
	routes := f.srv.Routes
	must("CreateApplication(rd_decapp)", routes.CreateApplication(ctx, routing.Application{ID: "rd_decapp", ServerID: baOwnedServer, Type: routing.ProviderAudioCpp, Port: 8300, Scheme: "http", TimeoutMS: 600000, APIFlavors: []string{routing.APIFlavorDecisions}, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}))
	for _, m := range []struct{ app, id string }{{"rf_agent", "rd_dec"}, {"rf_agent", "rd_mixed"}, {"rf_agent", "rd_audiodec"}, {"rd_decapp", "rd_ext"}} {
		must("CreateMapping("+m.id+")", routes.CreateMapping(ctx, routing.ModelMapping{ID: m.id, ApplicationID: m.app, GatewayModelName: "gw-" + m.id, AppModelName: "up-" + m.id, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}))
	}
	for id, flavors := range map[string][]string{
		"rd_dec":      {routing.APIFlavorDecisions},
		"rd_mixed":    {routing.APIFlavorOpenAI, routing.APIFlavorDecisions},
		"rd_audiodec": {routing.APIFlavorOpenAIAudio, routing.APIFlavorDecisions},
	} {
		must("UpsertRuntimeSpec("+id+")", routes.UpsertRuntimeSpec(ctx, routing.RuntimeSpec{ID: "rs_" + id, MappingID: id, Enabled: true, Binary: "/opt/bin/server", Args: "[]", Env: "{}", HealthPath: "/health", APIFlavors: flavors, CreatedAt: now, UpdatedAt: now}))
	}
}

func TestTargetIsDecisionsOnly(t *testing.T) {
	for _, tc := range []struct {
		name    string
		flavors []string
		want    bool
	}{
		{"decisions only", []string{routing.APIFlavorDecisions}, true},
		{"text and decisions", []string{routing.APIFlavorOpenAI, routing.APIFlavorDecisions}, false},
		{"anthropic and decisions", []string{routing.APIFlavorAnthropic, routing.APIFlavorDecisions}, false},
		{"images only", []string{routing.APIFlavorOpenAIImages}, false},
		{"empty", []string{}, false},
		{"nil", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := targetIsDecisionsOnly(routing.Target{APIFlavors: tc.flavors}); got != tc.want {
				t.Fatalf("targetIsDecisionsOnly(%q) = %v, want %v", tc.flavors, got, tc.want)
			}
		})
	}
}

// The text translate dispatch refuses a decisions-only child exactly as it
// refuses an audio-only one: 404 routing.no_model_route, no upstream call.
func TestTranslateDispatchRefusesADecisionsOnlyTarget(t *testing.T) {
	for _, body := range []string{
		`{"model":"gw-model","messages":[{"role":"user","content":"hi"}]}`,
		`{"model":"gw-model","stream":true,"messages":[{"role":"user","content":"hi"}]}`,
	} {
		prov := &countingTranslateProvider{}
		srv := newServerAgentSpecTestServer(t, prov, []string{routing.APIFlavorOpenAI, routing.APIFlavorDecisions}, []string{routing.APIFlavorDecisions}, routing.EndpointModeTranslate)
		rec := postBearer(t, srv, "dev-secret", "/v1/chat/completions", body)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404; body = %s", rec.Code, rec.Body.String())
		}
		requireErrorCode(t, rec.Body.String(), "routing.no_model_route")
		if n := prov.calls(); n != 0 {
			t.Fatalf("provider calls = %d, want 0 -- a decisions-only child must never be sent a text request", n)
		}
	}
}

func TestWarmerSkipsADecisionsOnlyMapping(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	mem := routing.NewMemoryStore()
	if err := mem.CreateAIServer(ctx, routing.AIServer{ID: "srv-dec", Name: "srv-dec", Domain: "srv-dec.example.test", Provider: routing.ProviderMock, Endpoint: "mock://srv-dec", Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateAIServer: %v", err)
	}
	if err := mem.CreateApplication(ctx, routing.Application{ID: "app-dec", ServerID: "srv-dec", Type: routing.ProviderServerAgent, Port: 8081, Scheme: "http", APIFlavors: []string{routing.APIFlavorOpenAI, routing.APIFlavorDecisions}, TimeoutMS: 30000, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateApplication: %v", err)
	}
	child := func(id, gateway, upstream string, flavors []string) {
		t.Helper()
		if err := mem.CreateMapping(ctx, routing.ModelMapping{ID: id, ApplicationID: "app-dec", GatewayModelName: gateway, AppModelName: upstream, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("CreateMapping %s: %v", id, err)
		}
		if err := mem.UpsertRuntimeSpec(ctx, routing.RuntimeSpec{ID: "spec-" + id, MappingID: id, Binary: "/opt/bin/server", Args: "[]", Env: "{}", APIFlavors: flavors, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("UpsertRuntimeSpec %s: %v", id, err)
		}
	}
	child("map-decide", "decide", "up-decide", []string{routing.APIFlavorDecisions})
	child("map-mixed", "mixed", "up-mixed", []string{routing.APIFlavorOpenAI, routing.APIFlavorDecisions})

	fake := newColdLister(nil)
	w := newModelWarmer(&Server{Provider: fake, Routes: mem})

	w.Warm(ctx, "decide")
	waitWarmIdle(t, w, "decide")
	if got := fake.streamedModels(); len(got) != 0 {
		t.Fatalf("streamed = %v, want no chat prompt to a decisions-only child", got)
	}
	// Review focus: a mapping that also serves chat is still warmed.
	w.Warm(ctx, "mixed")
	waitWarmIdle(t, w, "mixed")
	if got := fake.streamedModels(); len(got) != 1 || got[0] != "up-mixed" {
		t.Fatalf("streamed = %v, want exactly [up-mixed]", got)
	}
}

func TestBenchmarkRefusesADecisionsOnlyMapping(t *testing.T) {
	for _, tc := range []struct {
		name, path string
		specReads  map[string]int
	}{
		{"agent child, speed", "/api/portal/mappings/rd_dec/benchmark?mode=speed", map[string]int{"rd_dec": 1}},
		{"agent child, vision", "/api/portal/mappings/rd_dec/benchmark?mode=vision", map[string]int{"rd_dec": 1}},
		{"non-agent mapping, both", "/api/portal/mappings/rd_ext/benchmark?mode=both", map[string]int{"rd_ext": 0}},
		{"application scope of non-agent mappings", "/api/portal/applications/rd_decapp/benchmark?mode=speed", map[string]int{"rd_ext": 0}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRefusalFixture(t)
			f.addDecisionsOnlyMappings(t)
			status, code, body := f.post(t, tc.path)
			var errBody apierror.Body
			_ = json.Unmarshal(body, &errBody)
			if status != http.StatusConflict || code != codeBenchmarkDecisionsOnly || errBody.Error.Message != msgBenchmarkDecisionsOnly {
				t.Fatalf("answer = %d %q %q, want 409 %s %q", status, code, errBody.Error.Message, codeBenchmarkDecisionsOnly, msgBenchmarkDecisionsOnly)
			}
			f.assertNothingReserved(t)
			for mappingID, want := range tc.specReads {
				if n := f.specs.readCount(mappingID); n != want {
					t.Fatalf("spec reads for %s = %d, want %d", mappingID, n, want)
				}
			}
			if got := f.prov.streamedModels(); len(got) != 0 {
				t.Fatalf("streamed %v, want no chat prompt", got)
			}
		})
	}
	if codeBenchmarkDecisionsOnly != "benchmark.decisions_only" || benchmarkSkippedDecisionsOnly != "decisions_only" {
		t.Fatalf("codes = %q / %q, want benchmark.decisions_only / decisions_only", codeBenchmarkDecisionsOnly, benchmarkSkippedDecisionsOnly)
	}
}

// Review focus 2: a list naming audio and decisions is audio-only first, so the
// refusal carries the audio code, deterministically.
func TestAudioAndDecisionsOnlyMappingRefusesWithTheAudioCode(t *testing.T) {
	f := newRefusalFixture(t)
	f.addDecisionsOnlyMappings(t)
	if status, code, _ := f.post(t, "/api/portal/mappings/rd_audiodec/benchmark?mode=vision"); status != http.StatusConflict || code != codeBenchmarkAudioOnly {
		t.Fatalf("vision: status = %d code = %q, want 409 %s", status, code, codeBenchmarkAudioOnly)
	}
	if status, code, _ := f.post(t, "/api/portal/mappings/rd_audiodec/probe-context"); status != http.StatusConflict || code != codeBenchmarkAudioOnly {
		t.Fatalf("probe-context: status = %d code = %q, want 409 %s", status, code, codeBenchmarkAudioOnly)
	}
	f.srv.AgentFeatures.Set(baOwnedServer, []string{runtimeEnsureFeature})
	if got := loadRefusal(routing.Application{Type: routing.ProviderAudioCpp, APIFlavors: []string{routing.APIFlavorOpenAIAudio, routing.APIFlavorDecisions}}, routing.RuntimeSpec{}, false, true); got != loadRefusalAudioOnly {
		t.Fatalf("loadRefusal = %q, want %q", got, loadRefusalAudioOnly)
	}
	f.assertNothingReserved(t)
}

// Review focus 1: a mapping that serves chat beside decisions is not refused
// and is measured.
func TestMixedChatAndDecisionsMappingIsStillWarmedAndBenchmarked(t *testing.T) {
	f := newRefusalFixture(t)
	f.addDecisionsOnlyMappings(t)
	status, code, _ := f.post(t, "/api/portal/mappings/rd_mixed/benchmark?mode=speed")
	if status != http.StatusAccepted {
		t.Fatalf("status = %d code = %q, want 202", status, code)
	}
	st := f.waitFinished(t)
	if len(st.Results) != 1 || st.Results[0].Skipped != "" || st.Results[0].Error != "" {
		t.Fatalf("results = %+v, want one measured result", st.Results)
	}
	if got := f.prov.streamedModels(); !slices.Contains(got, "up-rd_mixed") {
		t.Fatalf("streamed %v, want up-rd_mixed", got)
	}
}

func TestBenchmarkPartitionSkipsDecisionsOnly(t *testing.T) {
	f := newRefusalFixture(t)
	f.addDecisionsOnlyMappings(t)
	status, code, body := f.post(t, "/api/portal/applications/rf_agent/benchmark?mode=speed")
	if status != http.StatusAccepted {
		t.Fatalf("status = %d code = %q, want 202", status, code)
	}
	if !strings.Contains(string(body), `"skipped":"decisions_only"`) {
		t.Fatalf("202 body = %s, want a result carrying \"skipped\":\"decisions_only\"", body)
	}
	st := f.waitFinished(t)
	byID := map[string]BenchmarkResult{}
	for _, r := range st.Results {
		byID[r.MappingID] = r
	}
	want := BenchmarkResult{MappingID: "rd_dec", GatewayModelName: "gw-rd_dec", Skipped: benchmarkSkippedDecisionsOnly}
	if !reflect.DeepEqual(byID["rd_dec"], want) {
		t.Fatalf("rd_dec result = %+v, want %+v", byID["rd_dec"], want)
	}
	// The audio+decisions child keeps the audio skip reason (checked first).
	if got := byID["rd_audiodec"].Skipped; got != benchmarkSkippedAudioOnly {
		t.Fatalf("rd_audiodec skipped = %q, want %q", got, benchmarkSkippedAudioOnly)
	}
	if got := byID["rd_mixed"]; got.Skipped != "" || got.Error != "" {
		t.Fatalf("rd_mixed result = %+v, want measured", got)
	}
	if got := f.prov.streamedModels(); slices.Contains(got, "up-rd_dec") || slices.Contains(got, "up-rd_audiodec") {
		t.Fatalf("streamed %v, want no chat prompt to a non-text child", got)
	}
	f.assertHistoryRows(t, map[string]int{"rd_dec": 0, "rd_mixed": 1})
}

func TestRefuseNothingRunnableNamesDecisionsOnly(t *testing.T) {
	for _, tc := range []struct {
		name string
		part benchmarkViewPartition
		code string
	}{
		{"every view decisions-only", benchmarkViewPartition{decisionsOnly: 2}, codeBenchmarkDecisionsOnly},
		{"images beside decisions", benchmarkViewPartition{imagesOnly: 1, decisionsOnly: 1}, codeBenchmarkImagesOnly},
		{"audio beside decisions", benchmarkViewPartition{audioOnly: 1, decisionsOnly: 1}, codeBenchmarkAudioOnly},
		{"cluster head beside decisions", benchmarkViewPartition{clusterHeads: 1, decisionsOnly: 1}, codeBenchmarkDecisionsOnly},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			if !tc.part.refuseNothingRunnable(rec) {
				t.Fatal("refuseNothingRunnable = false, want true")
			}
			var body apierror.Body
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if rec.Code != http.StatusConflict || body.Error.Code != tc.code {
				t.Fatalf("answer = %d %q, want 409 %q", rec.Code, body.Error.Code, tc.code)
			}
		})
	}
}

func TestContextProbeRefusesDecisionsOnly(t *testing.T) {
	for _, tc := range []struct {
		mapping   string
		specReads int
	}{
		{"rd_dec", 1},
		{"rd_ext", 0},
	} {
		t.Run(tc.mapping, func(t *testing.T) {
			f := newRefusalFixture(t)
			f.addDecisionsOnlyMappings(t)
			f.srv.AgentFeatures.Set(baOwnedServer, []string{runtimeEnsureFeature})
			if status, code, _ := f.post(t, "/api/portal/mappings/"+tc.mapping+"/probe-context"); status != http.StatusConflict || code != codeBenchmarkDecisionsOnly {
				t.Fatalf("status = %d code = %q, want 409 %s", status, code, codeBenchmarkDecisionsOnly)
			}
			f.assertNothingReserved(t)
			if n := f.specs.readCount(tc.mapping); n != tc.specReads {
				t.Fatalf("spec reads for %s = %d, want %d", tc.mapping, n, tc.specReads)
			}
			if got := f.prov.streamedModels(); len(got) != 0 {
				t.Fatalf("streamed %v, want no chat prompt", got)
			}
		})
	}
}

func TestLoadRefusesDecisionsOnly(t *testing.T) {
	t.Run("non-agent application", func(t *testing.T) {
		f := newRefusalFixture(t)
		f.addDecisionsOnlyMappings(t)
		f.srv.AgentFeatures.Set(baOwnedServer, []string{runtimeEnsureFeature})
		if status, code, _ := f.post(t, "/api/portal/mappings/rd_ext/load"); status != http.StatusConflict || code != codeBenchmarkDecisionsOnly {
			t.Fatalf("status = %d code = %q, want 409 %s", status, code, codeBenchmarkDecisionsOnly)
		}
		f.assertNothingReserved(t)
	})
	t.Run("agent child without runtime_ensure", func(t *testing.T) {
		f := newRefusalFixture(t)
		f.addDecisionsOnlyMappings(t)
		if status, code, _ := f.post(t, "/api/portal/mappings/rd_dec/load"); status != http.StatusConflict || code != codeBenchmarkAgentEnsureUnsupported {
			t.Fatalf("status = %d code = %q, want 409 %s", status, code, codeBenchmarkAgentEnsureUnsupported)
		}
		f.assertNothingReserved(t)
	})
	t.Run("agent child with runtime_ensure starts without generating", func(t *testing.T) {
		f := newRefusalFixture(t)
		f.addDecisionsOnlyMappings(t)
		ensurer := &ensuringColdLister{coldLister: f.prov}
		f.srv.Provider = ensurer
		f.srv.AgentFeatures.Set(baOwnedServer, []string{runtimeEnsureFeature})
		if status, code, _ := f.post(t, "/api/portal/mappings/rd_dec/load"); status != http.StatusAccepted {
			t.Fatalf("status = %d code = %q, want 202", status, code)
		}
		f.waitFinished(t)
		if got := ensurer.ensuredModels(); !slices.Equal(got, []string{"up-rd_dec"}) {
			t.Fatalf("ensured %v, want [up-rd_dec]", got)
		}
		if got := f.prov.streamedModels(); slices.Contains(got, "up-rd_dec") {
			t.Fatalf("streamed %v, want no chat prompt", got)
		}
	})
}

func TestLoadTargetForMarksADecisionsOnlyChild(t *testing.T) {
	srv := &Server{Routes: noSpecReadStore{Store: routing.NewMemoryStore(), t: t}}
	app := routing.Application{Type: routing.ProviderServerAgent, APIFlavors: []string{routing.APIFlavorOpenAI, routing.APIFlavorDecisions}}
	view := portal.BenchmarkTargetView{App: app, Mapping: routing.ModelMapping{ID: "m1"}}
	dec := routing.RuntimeSpec{ID: "rs_dec", APIFlavors: []string{routing.APIFlavorDecisions}}
	if tgt := srv.loadTargetFor(context.Background(), view, dec, true); !tgt.loadWithoutGenerating {
		t.Fatal("decisions-only child: loadWithoutGenerating = false, want true")
	}
	mixed := routing.RuntimeSpec{ID: "rs_mixed", APIFlavors: []string{routing.APIFlavorOpenAI, routing.APIFlavorDecisions}}
	if tgt := srv.loadTargetFor(context.Background(), view, mixed, true); tgt.loadWithoutGenerating {
		t.Fatal("text-and-decisions child: loadWithoutGenerating = true, want false")
	}
}

func TestVRAMProbeSkipsDecisionsOnly(t *testing.T) {
	dec := []string{routing.APIFlavorDecisions}
	t.Run("with runtime_ensure it is an ensure plan", func(t *testing.T) {
		f := newVRAMFixture(t, vramFixtureOpts{targetAPIFlavors: dec})
		f.srv.AgentFeatures.Set("srv1", []string{"runtime_manager", runtimeEnsureFeature})
		f.seedLatestSample()
		plan, err := f.srv.vramRunPlan(context.Background(), f.target)
		if err != nil {
			t.Fatalf("vramRunPlan: %v", err)
		}
		if !plan.ensure {
			t.Fatal("plan.ensure = false, want an ensure plan for a decisions-only target")
		}
	})
	t.Run("without runtime_ensure it is refused", func(t *testing.T) {
		f := newVRAMFixture(t, vramFixtureOpts{targetAPIFlavors: dec})
		f.srv.AgentFeatures.Set("srv1", []string{"runtime_manager"})
		f.seedLatestSample()
		_, err := f.srv.vramRunPlan(context.Background(), f.target)
		if got := vramRefusalCode(t, err); got != "benchmark.agent_ensure_unsupported" {
			t.Fatalf("refusal code = %q, want benchmark.agent_ensure_unsupported", got)
		}
	})
}

func TestMappingSpecIsDecisionsOnly(t *testing.T) {
	agent := routing.Application{Type: routing.ProviderServerAgent, APIFlavors: []string{routing.APIFlavorOpenAI, routing.APIFlavorDecisions}}
	for _, tc := range []struct {
		name    string
		spec    routing.RuntimeSpec
		hasSpec bool
		want    bool
	}{
		{"spec decisions", routing.RuntimeSpec{APIFlavors: []string{routing.APIFlavorDecisions}}, true, true},
		{"spec text and decisions", routing.RuntimeSpec{APIFlavors: []string{routing.APIFlavorOpenAI, routing.APIFlavorDecisions}}, true, false},
		{"no spec falls back to the application", routing.RuntimeSpec{}, false, false},
	} {
		if got := mappingSpecIsDecisionsOnly(agent, tc.spec, tc.hasSpec); got != tc.want {
			t.Errorf("%s: = %v, want %v", tc.name, got, tc.want)
		}
	}
}
