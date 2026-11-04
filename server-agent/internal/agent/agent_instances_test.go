// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"op-ai-server-agent/internal/config"
	runtimectl "op-ai-server-agent/internal/runtime"
	"op-ai-server-agent/internal/sample"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// Telemetry for model instances (#340): one runtime sample per
// instance, carrying its index and its measured startup; and the per-child
// probes, which must ask an instance's child about the base model -- the only
// name it knows -- and cache what they learn per instance, never per spec.

// TestRuntimeSampleCarriesInstanceAndStartup: a status row's Instance and
// StartupMS ride the runtime sample as "instance" and "startup_ms". instance is
// always on the wire, 0 included -- it is half of a row's identity, beside
// spec_id -- while startup_ms is omitted while unknown (0).
func TestRuntimeSampleCarriesInstanceAndStartup(t *testing.T) {
	poster := &capturePoster{}
	drv := newFakeRuntimeDriver()
	drv.setActive(true)
	since := time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC)
	drv.setStatuses([]runtimectl.Status{
		{SpecID: "rspec_m", Model: "m#2", Instance: 2, StartupMS: 900, State: runtimectl.StateRunning, Since: since, PID: 222},
		{SpecID: "rspec_m", Model: "m", State: runtimectl.StateStopped, Since: since},
	})

	a := NewFromDeps(config.Config{Interval: time.Hour}, Deps{Poster: poster, RuntimeDriver: drv})
	a.collectOnce(context.Background())

	got := poster.first()
	if got == nil || len(got.Runtimes) != 2 {
		t.Fatalf("Runtimes = %+v, want the two instance rows", got)
	}
	if r := got.Runtimes[0]; r.Instance != 2 || r.StartupMS != 900 || r.Model != "m#2" || r.SpecID != "rspec_m" {
		t.Errorf("Runtimes[0] = {SpecID:%q Model:%q Instance:%d StartupMS:%d}, want {rspec_m m#2 2 900}", r.SpecID, r.Model, r.Instance, r.StartupMS)
	}

	got.Normalize()
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(raw), `"instance":2,"startup_ms":900`) {
		t.Errorf("marshaled sample lacks \"instance\":2,\"startup_ms\":900: %s", raw)
	}
	var wire struct {
		Runtimes []map[string]json.RawMessage `json:"runtimes"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if v, ok := wire.Runtimes[1]["instance"]; !ok || string(v) != "0" {
		t.Errorf("runtimes[1].instance = %s (present %v), want 0, always present", v, ok)
	}
	if v, ok := wire.Runtimes[1]["startup_ms"]; ok {
		t.Errorf("runtimes[1].startup_ms = %s, want the key omitted while unknown", v)
	}
}

// TestLoadedModelsListsRunningInstanceAddresses: with the runtime driver
// active, the sample's loaded_models is the manager's own list -- one entry per
// RUNNING instance, by its address: m for instance 0, m#2 for instance 2 -- so
// the gateway sees which instances it can route to right now. An instance that
// is still starting is not listed; the runtime rows carry every instance.
func TestLoadedModelsListsRunningInstanceAddresses(t *testing.T) {
	poster := &capturePoster{}
	drv := newFakeRuntimeDriver()
	drv.setActive(true)
	drv.setStatuses([]runtimectl.Status{
		{SpecID: "rspec_m", Model: "m", State: runtimectl.StateRunning, PID: 100},
		{SpecID: "rspec_m", Model: "m#1", Instance: 1, State: runtimectl.StateStarting, PID: 101},
		{SpecID: "rspec_m", Model: "m#2", Instance: 2, State: runtimectl.StateRunning, PID: 102},
	})

	a := NewFromDeps(config.Config{Interval: time.Hour}, Deps{Poster: poster, RuntimeDriver: drv})
	a.collectOnce(context.Background())

	got := poster.first()
	if got == nil || len(got.Runtimes) != 3 {
		t.Fatalf("sample = %+v, want the three instance rows", got)
	}
	if want := []string{"m", "m#2"}; !reflect.DeepEqual(got.LoadedModels, want) {
		t.Fatalf("LoadedModels = %q, want %q: every running instance by its address", got.LoadedModels, want)
	}
	got.Normalize()
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(raw), `"loaded_models":["m","m#2"]`) {
		t.Errorf("marshaled sample lacks \"loaded_models\":[\"m\",\"m#2\"]: %s", raw)
	}
}

// ollamaInstanceChild is a fake `ollama serve` for one instance: every POST
// /api/show is answered with a context length and a capability list, and the
// model each one asked about is recorded.
type ollamaInstanceChild struct {
	srv *httptest.Server

	mu     sync.Mutex
	models []string
}

func newOllamaInstanceChild(t *testing.T) *ollamaInstanceChild {
	t.Helper()
	c := &ollamaInstanceChild{}
	c.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/show" {
			http.NotFound(w, r)
			return
		}
		var req struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		c.mu.Lock()
		c.models = append(c.models, req.Model)
		c.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model_info":{"llama.context_length":8192},"capabilities":["completion","tools"]}`))
	}))
	t.Cleanup(c.srv.Close)
	return c
}

func (c *ollamaInstanceChild) asked() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.models...)
}

// TestCollectOnceRuntimeProbesAnInstanceWithItsBaseModel: two instances of one
// ollama spec, each its own process. The context and capability probes ask each
// child about the BASE model "llama3" -- the instance address "llama3#1" means
// nothing to a child launched with the base name -- and each instance's answers
// are cached under that instance: across two cycles each child is asked once per
// probe. Cached per spec, the two rows would overwrite each other's entry every
// cycle (their pids differ) and both children would be asked again each time.
func TestCollectOnceRuntimeProbesAnInstanceWithItsBaseModel(t *testing.T) {
	child0, child1 := newOllamaInstanceChild(t), newOllamaInstanceChild(t)
	drv := newFakeRuntimeDriver()
	drv.setActive(true)
	row := func(k, pid int, model string, child *ollamaInstanceChild) runtimectl.Status {
		return runtimectl.Status{
			SpecID:           "rspec_ollama",
			Instance:         k,
			Model:            model,
			State:            runtimectl.StateRunning,
			PID:              pid,
			Port:             portFromURL(t, child.srv.URL),
			Type:             "ollama",
			ContextProbePath: "/api/show",
		}
	}
	drv.setStatuses([]runtimectl.Status{row(0, 4400, "llama3", child0), row(1, 4401, "llama3#1", child1)})

	poster := &capturePoster{}
	a := NewFromDeps(config.Config{Interval: time.Hour}, Deps{Poster: poster, RuntimeDriver: drv})
	a.collectOnce(context.Background())
	a.collectOnce(context.Background())

	for i, c := range []*ollamaInstanceChild{child0, child1} {
		if got, want := c.asked(), []string{"llama3", "llama3"}; !reflect.DeepEqual(got, want) {
			t.Errorf("instance %d's child was asked about %q, want %q: one context and one capability probe, both about the base model, then cached", i, got, want)
		}
	}
	last := poster.last()
	if last == nil || len(last.Runtimes) != 2 {
		t.Fatalf("Runtimes = %+v, want two rows", last)
	}
	for i, rs := range last.Runtimes {
		if rs.ContextSize != 8192 || rs.Capabilities == nil {
			t.Errorf("Runtimes[%d] = {ContextSize:%d Capabilities:%+v}, want its own cached answers", i, rs.ContextSize, rs.Capabilities)
		}
	}
	if last.Runtimes[1].Model != "llama3#1" || last.Runtimes[1].Instance != 1 {
		t.Errorf("Runtimes[1] = {Model:%q Instance:%d}, want the row's own address llama3#1, instance 1", last.Runtimes[1].Model, last.Runtimes[1].Instance)
	}
}

// TestProbeRuntimeChildPropsAudioCppInstanceReadsTheBaseModel: audio.cpp's
// listing names the model its child was launched with, the base name, so an
// instance's verdicts are the entry for the base model -- not the absent entry
// for "kokoro-82m#1" -- and each instance keeps its own cached answer.
func TestProbeRuntimeChildPropsAudioCppInstanceReadsTheBaseModel(t *testing.T) {
	child := newProbeChild(t, map[string]string{audioCppModelsRoute: audioCppTTSDoc})
	features := &fakeGatewayFeatures{names: []string{"runtime_manager", "capability_source_audiocpp"}}
	a := NewFromDeps(config.Config{}, Deps{GatewayFeatures: features})
	st0, base := audioCppStatus(t, child, 7301)
	st1 := st0
	st1.PID, st1.Instance, st1.Model = 7302, 1, audioCppModelID+"#1"

	for round := 0; round < 2; round++ {
		for _, st := range []runtimectl.Status{st0, st1} {
			var rs sample.RuntimeSample
			a.probeRuntimeChildProps(context.Background(), child.srv.Client(), base, st, &rs)
			if want := audioCppTTSCaps(); rs.Capabilities == nil || !reflect.DeepEqual(*rs.Capabilities, *want) {
				t.Fatalf("round %d, instance %d: Capabilities = %+v, want %+v (the listing's entry for the base model)", round, st.Instance, rs.Capabilities, want)
			}
		}
	}
	if got, want := child.requests(), map[string]int{audioCppModelsRoute: 2}; !reflect.DeepEqual(got, want) {
		t.Errorf("child requests = %v, want %v: one per instance, each cached under its own instance", got, want)
	}
}
