// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"fmt"
	"op-ai-gateway/internal/inference"
	"op-ai-gateway/internal/logbuffer"
	"op-ai-gateway/internal/provider"
	"op-ai-gateway/internal/routing"
	"strings"
	"testing"
	"time"
)

// benchAuthRejectingProvider streams like benchFakeProvider, and its context
// probe answers the way an upstream that refuses the credential does.
type benchAuthRejectingProvider struct {
	benchFakeProvider
	probedPath string
}

func (f *benchAuthRejectingProvider) ProbeModelInfo(_ context.Context, _ routing.Target, path string) ([]provider.ModelInfo, error) {
	f.probedPath = path
	return nil, fmt.Errorf("%w: upstream status 401", provider.ErrAuthRejected)
}

// benchPropsNotFoundProvider streams like benchFakeProvider, and its context
// probe answers 404, the way the agent router answers /upstream/{model}/props
// for a child that is not llama.cpp.
type benchPropsNotFoundProvider struct{ benchFakeProvider }

func (*benchPropsNotFoundProvider) ProbeModelInfo(context.Context, routing.Target, string) ([]provider.ModelInfo, error) {
	return nil, fmt.Errorf("%w: upstream status 404", provider.ErrUnavailable)
}

// benchContextProbeWarning is the Warn a context probe logs when the upstream
// refuses the mapping's credential.
const benchContextProbeWarning = "benchmark: context probe rejected by the upstream (401/403): check the runtime spec's API token"

// runContextProbeCapturingLogs runs the standalone context probe
// (runContextProbe) for a portal-shaped agent target on an agent that
// declares runtime_upstream_props, and returns the log records it produced.
func runContextProbeCapturingLogs(t *testing.T, p provider.Client) []logbuffer.Record {
	t.Helper()
	buf := withCapturedSlogAtTheDefaultLevel(t)
	reg := NewBenchmarkRegistry()
	srv := &Server{Provider: p, Benchmarks: reg, AgentFeatures: NewAgentFeaturesRegistry()}
	srv.AgentFeatures.Set("srv1", []string{RuntimeUpstreamPropsFeature})
	run, ok := reg.TryStart("srv1", "context-probe", "context", 1, time.Now(), func() {})
	if !ok {
		t.Fatal("TryStart did not start")
	}
	srv.runContextProbe(context.Background(), run, "srv1", benchServerAgentTarget())
	return buf.Snapshot()
}

func TestBenchmarkContextSizeWarnsOnARejectedToken(t *testing.T) {
	t.Run("a 401 warns with the mapping id", func(t *testing.T) {
		fake := &benchAuthRejectingProvider{benchFakeProvider: benchFakeProvider{usage: inference.Usage{OutputTokens: 20, TokensPerSecond: 55}}}
		recs := runContextProbeCapturingLogs(t, fake)
		if fake.probedPath != "/upstream/up-model/props" {
			t.Fatalf("probed path = %q, want /upstream/up-model/props", fake.probedPath)
		}
		for _, rec := range recs {
			if rec.Level == "WARN" && rec.Msg == benchContextProbeWarning {
				if rec.Attrs["mapping_id"] != "map1" {
					t.Errorf("mapping_id attr = %v, want map1", rec.Attrs["mapping_id"])
				}
				if errText, _ := rec.Attrs["err"].(string); !strings.Contains(errText, "upstream status 401") {
					t.Errorf("err attr = %v, want the probe's error (upstream status 401)", rec.Attrs["err"])
				}
				return
			}
		}
		t.Fatalf("no WARN record %q in %+v", benchContextProbeWarning, recs)
	})

	t.Run("a 404 logs no warning", func(t *testing.T) {
		recs := runContextProbeCapturingLogs(t, &benchPropsNotFoundProvider{benchFakeProvider: benchFakeProvider{usage: inference.Usage{OutputTokens: 20, TokensPerSecond: 55}}})
		for _, rec := range recs {
			if rec.Level == "WARN" {
				t.Fatalf("WARN record %q: a child that serves no /props is not a token problem", rec.Msg)
			}
		}
	})
}

// TestBenchmarkContextSizeAttributesByPathOrDirectly pins the attribution of
// a plain (non-{model}) probe path: the speed run counts only an answer named
// like the mapping's model, because a plain path describes whatever the
// upstream has loaded, while the context probe attributes it directly, because
// it has just loaded this model.
func TestBenchmarkContextSizeAttributesByPathOrDirectly(t *testing.T) {
	divergent := func() *benchProbingProvider {
		return &benchProbingProvider{
			benchFakeProvider: benchFakeProvider{usage: inference.Usage{OutputTokens: 20, TokensPerSecond: 55}},
			probeName:         "some-basename",
			probeContext:      4096,
		}
	}

	t.Run("the speed run ignores an answer for another model", func(t *testing.T) {
		mem := ctxProbeSeedStore(t, "/props", "up-model")
		srv := &Server{Provider: divergent(), Routes: mem}
		tgt := benchTestTarget()
		tgt.app.ContextProbePath = "/props"
		if res := srv.measureSpeedTarget(context.Background(), tgt); res.ContextSize != 0 {
			t.Fatalf("ContextSize = %d, want 0 (the answer names another model)", res.ContextSize)
		}
	})

	t.Run("the context probe attributes the answer directly", func(t *testing.T) {
		reg := NewBenchmarkRegistry()
		srv := &Server{Provider: divergent(), Benchmarks: reg}
		run, ok := reg.TryStart("srv1", "context-probe", "context", 1, time.Now(), func() {})
		if !ok {
			t.Fatal("TryStart did not start")
		}
		tgt := benchTestTarget()
		tgt.app.ContextProbePath = "/props"
		srv.runContextProbe(context.Background(), run, "srv1", tgt)
		if st := reg.Status("srv1"); len(st.Results) != 1 || st.Results[0].ContextSize != 4096 {
			t.Fatalf("results = %+v, want one result with ContextSize 4096", st.Results)
		}
	})
}

// TestBenchmarkProbeContextSizeGuards pins the synchronous probe's two guards
// on the speed run: without an effective probe path nothing is probed, and a
// probe answer above benchmarkMaxContextSize is neither reported nor written
// to the mapping.
func TestBenchmarkProbeContextSizeGuards(t *testing.T) {
	// probing answers any path with one entry named like the mapping's model.
	probing := func(size int) *benchProbingProvider {
		return &benchProbingProvider{
			benchFakeProvider: benchFakeProvider{usage: inference.Usage{OutputTokens: 20, TokensPerSecond: 55}},
			probeName:         "up-model",
			probeContext:      size,
		}
	}
	storedContextSize := func(t *testing.T, mem *routing.MemoryStore) int {
		t.Helper()
		m, err := mem.MappingByID(context.Background(), "map1")
		if err != nil {
			t.Fatalf("MappingByID: %v", err)
		}
		return m.ContextSize
	}

	for _, tc := range []struct {
		name   string
		target func() benchmarkTarget
	}{
		{"a server_agent application on an agent without runtime_upstream_props", benchServerAgentTarget},
		{"an application of another type without a context_probe_path", benchTestTarget},
	} {
		t.Run(tc.name+": nothing is probed", func(t *testing.T) {
			mem := ctxProbeSeedStore(t, "", "up-model")
			fake := probing(8192)
			srv := &Server{Provider: fake, Routes: mem, AgentFeatures: NewAgentFeaturesRegistry()}
			res := srv.measureSpeedTarget(context.Background(), tc.target())
			if res.Error != "" {
				t.Fatalf("measureSpeedTarget error = %q, want empty", res.Error)
			}
			// A probe that ran carries the application's endpoint.
			if fake.probedTarget.Endpoint != "" {
				t.Errorf("the context probe ran on %q (path %q): without a probe path nothing is asked", fake.probedTarget.Endpoint, fake.probedPath)
			}
			if res.ContextSize != 0 {
				t.Errorf("ContextSize = %d, want 0", res.ContextSize)
			}
			if got := storedContextSize(t, mem); got != 0 {
				t.Errorf("mapping context_size = %d, want 0", got)
			}
		})
	}

	t.Run("a {model} answer above benchmarkMaxContextSize is not a size", func(t *testing.T) {
		ctx := context.Background()
		const probePath = "/upstream/{model}/props"
		mem := ctxProbeSeedStore(t, probePath, "up-model")
		if err := mem.UpdateMappingContextProbe(ctx, "map1", 4096, time.Now().UTC()); err != nil {
			t.Fatalf("UpdateMappingContextProbe: %v", err)
		}
		fake := probing(benchmarkMaxContextSize + 1)
		srv := &Server{Provider: fake, Routes: mem}
		tgt := benchTestTarget()
		tgt.app.ContextProbePath = probePath
		res := srv.measureSpeedTarget(ctx, tgt)
		if fake.probedPath != "/upstream/up-model/props" {
			t.Fatalf("probed path = %q, want /upstream/up-model/props", fake.probedPath)
		}
		if res.ContextSize != 0 {
			t.Errorf("ContextSize = %d, want 0 (the answer is out of bounds)", res.ContextSize)
		}
		if got := storedContextSize(t, mem); got != 4096 {
			t.Errorf("mapping context_size = %d, want the stored 4096 unchanged", got)
		}
	})
}

// TestBenchmarkAttributedContextSizeByName pins the by-name rule a plain probe
// path gets on the speed run: of the entries named like the mapping's model,
// the last one whose size is in bounds wins.
func TestBenchmarkAttributedContextSizeByName(t *testing.T) {
	for _, tc := range []struct {
		name  string
		infos []provider.ModelInfo
		want  int
	}{
		{
			name:  "the last in-bounds match wins",
			infos: []provider.ModelInfo{{Name: "up-model", ContextSize: 4096}, {Name: "up-model", ContextSize: 8192}},
			want:  8192,
		},
		{
			name:  "a last match out of bounds leaves the earlier in-bounds one",
			infos: []provider.ModelInfo{{Name: "up-model", ContextSize: 4096}, {Name: "up-model", ContextSize: benchmarkMaxContextSize + 1}},
			want:  4096,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := benchmarkAttributedContextSize(tc.infos, "up-model", false); got != tc.want {
				t.Errorf("benchmarkAttributedContextSize = %d, want %d", got, tc.want)
			}
		})
	}
}

// runtimeSubscriberCount is the number of live subscribers to serverID's
// runtime-status stream; 0 for a nil registry.
func runtimeSubscriberCount(reg *runtimeStatusRegistry, serverID string) int {
	if reg == nil {
		return 0
	}
	reg.mu.RLock()
	defer reg.mu.RUnlock()
	return len(reg.subs[serverID])
}

// waitRuntimeSubscriber reports whether serverID's runtime-status stream got a
// subscriber within 2 s, so a frame published afterwards reaches it as a frame
// rather than as part of its snapshot.
func waitRuntimeSubscriber(reg *runtimeStatusRegistry, serverID string) bool {
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if runtimeSubscriberCount(reg, serverID) > 0 {
			return true
		}
		time.Sleep(time.Millisecond)
	}
	return false
}

func TestBenchmarkContextSizeFromAgentTelemetry(t *testing.T) {
	old := benchmarkTelemetryContextWait
	t.Cleanup(func() { benchmarkTelemetryContextWait = old })

	// Each row sets its own benchmarkTelemetryContextWait. A row that must end
	// early gets a bound far beyond its ceiling, so its answer never depends on
	// the publisher beating the timer, and running to the bound breaks the
	// ceiling. Only the two rows that must run to the bound get a short one.
	const (
		earlyBound   = 10 * time.Second
		earlyCeiling = 5 * time.Second
		fullBound    = 400 * time.Millisecond
	)

	const specID = "rs_vllm"
	row := func(id, state string, size int, probe string) RuntimeStatusDTO {
		return RuntimeStatusDTO{SpecID: id, Model: "up-model", State: state, ContextSize: size, ContextProbe: probe}
	}
	bothFeatures := []string{RuntimeUpstreamPropsFeature, runtimeModelProbeFeature}

	cases := []struct {
		name     string
		features []string
		snapshot []RuntimeStatusDTO
		// frames are published one by one once the call has subscribed.
		frames [][]RuntimeStatusDTO
		// target adjusts the vllm server_agent target.
		target      func(*benchmarkTarget)
		nilRegistry bool
		canceled    bool
		want        int
		wantSrc     benchmarkContextSource
		// bound is the row's benchmarkTelemetryContextWait.
		bound   time.Duration
		atLeast time.Duration
		atMost  time.Duration
	}{
		{
			name: "a running row with a size in the snapshot", features: bothFeatures,
			snapshot: []RuntimeStatusDTO{row(specID, "running", 32768, "ok")},
			want:     32768, wantSrc: contextSourceTelemetry, bound: earlyBound, atMost: earlyCeiling,
		},
		{
			name: "na in the snapshot answers at once", features: bothFeatures,
			snapshot: []RuntimeStatusDTO{row(specID, "running", 0, "na")},
			wantSrc:  contextSourceNone, bound: earlyBound, atMost: earlyCeiling,
		},
		{
			name: "router in the snapshot answers at once", features: bothFeatures,
			snapshot: []RuntimeStatusDTO{row(specID, "running", 0, "router")},
			wantSrc:  contextSourceNone, bound: earlyBound, atMost: earlyCeiling,
		},
		{
			name: "unreachable in the snapshot, then a fresh ok frame", features: bothFeatures,
			snapshot: []RuntimeStatusDTO{row(specID, "running", 0, "unreachable")},
			frames:   [][]RuntimeStatusDTO{{row(specID, "running", 4096, "ok")}},
			want:     4096, wantSrc: contextSourceTelemetry, bound: earlyBound, atMost: earlyCeiling,
		},
		{
			name: "a starting row's old size waits for the running frame", features: bothFeatures,
			snapshot: []RuntimeStatusDTO{row(specID, "starting", 4096, "ok")},
			frames:   [][]RuntimeStatusDTO{{row(specID, "running", 8192, "ok")}},
			want:     8192, wantSrc: contextSourceTelemetry, bound: earlyBound, atMost: earlyCeiling,
		},
		{
			name: "a frame counts once it is running with a probe result", features: bothFeatures,
			snapshot: []RuntimeStatusDTO{row(specID, "starting", 0, "")},
			frames: [][]RuntimeStatusDTO{
				{row(specID, "starting", 0, "unreachable")},
				{row(specID, "running", 0, "")},
				{row(specID, "running", 16384, "ok")},
			},
			want: 16384, wantSrc: contextSourceTelemetry, bound: earlyBound, atMost: earlyCeiling,
		},
		{
			name: "one fresh unreachable frame is final", features: bothFeatures,
			snapshot: []RuntimeStatusDTO{row(specID, "starting", 0, "")},
			frames:   [][]RuntimeStatusDTO{{row(specID, "running", 0, "unreachable")}, {row(specID, "running", 4096, "ok")}},
			wantSrc:  contextSourceNone, bound: earlyBound, atMost: earlyCeiling,
		},
		{
			name: "no frame ends at the bound", features: bothFeatures,
			snapshot: []RuntimeStatusDTO{row(specID, "running", 0, "unreachable")},
			wantSrc:  contextSourceNone, bound: fullBound, atLeast: fullBound,
		},
		{
			name: "a row for another spec_id is ignored", features: bothFeatures,
			snapshot: []RuntimeStatusDTO{row("rs_other", "running", 4096, "ok")},
			frames:   [][]RuntimeStatusDTO{{row("rs_other", "running", 8192, "ok")}},
			wantSrc:  contextSourceNone, bound: fullBound, atLeast: fullBound,
		},
		{
			name: "a size above benchmarkMaxContextSize is not a size", features: bothFeatures,
			snapshot: []RuntimeStatusDTO{row(specID, "running", benchmarkMaxContextSize+1, "ok")},
			frames:   [][]RuntimeStatusDTO{{row(specID, "running", benchmarkMaxContextSize+1, "ok")}},
			wantSrc:  contextSourceNone, bound: earlyBound, atMost: earlyCeiling,
		},
		{
			name: "a done context ends the wait", features: bothFeatures, canceled: true,
			snapshot: []RuntimeStatusDTO{row(specID, "running", 0, "unreachable")},
			wantSrc:  contextSourceNone, bound: earlyBound, atMost: earlyCeiling,
		},
		{
			name: "a closed stream ends the wait", features: bothFeatures, nilRegistry: true,
			wantSrc: contextSourceNone, bound: earlyBound, atMost: earlyCeiling,
		},
		{
			name: "without runtime_model_probe the telemetry is not read", features: []string{RuntimeUpstreamPropsFeature},
			snapshot: []RuntimeStatusDTO{row(specID, "running", 32768, "ok")},
			wantSrc:  contextSourceNone, bound: earlyBound, atMost: earlyCeiling,
		},
		{
			name: "a target without a launch spec is not looked up", features: bothFeatures,
			snapshot: []RuntimeStatusDTO{row("", "running", 32768, "ok")},
			target:   func(tgt *benchmarkTarget) { tgt.spec.ID = "" },
			wantSrc:  contextSourceNone, bound: earlyBound, atMost: earlyCeiling,
		},
		{
			name: "a non-agent application is not looked up", features: bothFeatures,
			snapshot: []RuntimeStatusDTO{row(specID, "running", 32768, "ok")},
			target:   func(tgt *benchmarkTarget) { tgt.app.Type = routing.ProviderVLLM },
			wantSrc:  contextSourceNone, bound: earlyBound, atMost: earlyCeiling,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			benchmarkTelemetryContextWait = tc.bound
			reg := NewRuntimeStatusRegistry()
			if tc.nilRegistry {
				reg = nil
			}
			srv := &Server{Provider: &benchPropsNotFoundProvider{}, RuntimeStatus: reg, AgentFeatures: NewAgentFeaturesRegistry()}
			srv.AgentFeatures.Set("srv1", tc.features)
			reg.publish("srv1", tc.snapshot)
			tgt := benchServerAgentTarget()
			tgt.spec.ID = specID
			tgt.spec.Type = string(routing.RuntimeSpecTypeVLLM)
			if tc.target != nil {
				tc.target(&tgt)
			}

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.canceled {
				cancel()
			}
			published := make(chan struct{})
			go func() {
				defer close(published)
				if len(tc.frames) == 0 {
					return
				}
				if !waitRuntimeSubscriber(reg, "srv1") {
					t.Errorf("benchmarkContextSize never subscribed to the runtime-status stream")
					return
				}
				for _, frame := range tc.frames {
					reg.publish("srv1", frame)
				}
			}()

			start := time.Now()
			got, src := srv.benchmarkContextSize(ctx, tgt, contextByPath)
			elapsed := time.Since(start)
			<-published

			if got != tc.want || src != tc.wantSrc {
				t.Errorf("benchmarkContextSize = (%d, %d), want (%d, %d)", got, src, tc.want, tc.wantSrc)
			}
			if tc.atLeast > 0 && elapsed < tc.atLeast {
				t.Errorf("elapsed %v, want at least %v (the wait runs to its bound)", elapsed, tc.atLeast)
			}
			if tc.atMost > 0 && elapsed > tc.atMost {
				t.Errorf("elapsed %v, want at most %v (the answer is final long before the %v bound)", elapsed, tc.atMost, tc.bound)
			}
			if subs := runtimeSubscriberCount(reg, "srv1"); subs != 0 {
				t.Errorf("runtime-status subscribers after the call = %d, want 0", subs)
			}
		})
	}
}
