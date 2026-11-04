// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"errors"
	"math"
	"op-ai-gateway/internal/routing"
	"testing"
)

// TestPutRuntimeSpecInstanceRefusalOrder pins the order inside the instance
// block of the request validation, by bodies that break two of its checks at
// once: a negative count answers runtime_spec.tuning_invalid before anything
// else, a count above the agent's instance cap and a floor above the cap answer
// runtime_spec.instances_invalid before the fixed-port check, and the fixed
// port of a replicable spec is the last word. Nothing is stored for any of
// them.
func TestPutRuntimeSpecInstanceRefusalOrder(t *testing.T) {
	ctx := context.Background()
	svc, routeStore, mappingID, _ := instanceSpecFixture(t, nil, nil)
	legs := []struct {
		name   string
		mutate func(*PutRuntimeSpecRequest)
		want   error
	}{
		{"a negative count before a count above the cap", func(r *PutRuntimeSpecRequest) {
			r.MinInstances, r.MaxInstances = -1, maxRuntimeSpecInstances+1
		}, ErrRuntimeSpecTuningInvalid},
		{"a negative parallelism before a floor above the cap", func(r *PutRuntimeSpecRequest) {
			r.InstanceParallelism, r.MinInstances, r.MaxInstances = -1, 3, 2
		}, ErrRuntimeSpecTuningInvalid},
		{"a negative count before the fixed port", func(r *PutRuntimeSpecRequest) {
			r.Replicable, r.ListenPort, r.MaxInstances = true, 9000, -1
		}, ErrRuntimeSpecTuningInvalid},
		{"a count above the cap before the fixed port", func(r *PutRuntimeSpecRequest) {
			r.Replicable, r.ListenPort, r.MinInstances = true, 9000, maxRuntimeSpecInstances+1
		}, ErrRuntimeSpecInstancesInvalid},
		{"a floor above the cap before the fixed port", func(r *PutRuntimeSpecRequest) {
			r.Replicable, r.ListenPort, r.MinInstances, r.MaxInstances = true, 9000, 3, 2
		}, ErrRuntimeSpecInstancesInvalid},
	}
	for _, leg := range legs {
		req := PutRuntimeSpecRequest{Enabled: true, Binary: "/usr/local/bin/llama-server"}
		leg.mutate(&req)
		if _, err := svc.PutRuntimeSpec(ctx, ownerToken(), mappingID, req); !errors.Is(err, leg.want) {
			t.Errorf("%s: err = %v, want %v", leg.name, err, leg.want)
		}
		if _, ok, err := routeStore.RuntimeSpecByMapping(ctx, mappingID); err != nil || ok {
			t.Fatalf("%s: a refused PUT stored a spec (ok=%v err=%v)", leg.name, ok, err)
		}
	}
}

// TestPutRuntimeSpecInstanceCountsAboveTheAgentCapRefused pins the upper bound
// of both counts: the agent addresses instances 0 to routing.MaxInstanceIndex,
// so a spec has at most 64 of them, and a min_instances or max_instances above
// that is runtime_spec.instances_invalid -- the agent would clamp it, and a
// setting that silently does something else is refused where it is typed.
// Exactly 64 is a valid count on either side.
func TestPutRuntimeSpecInstanceCountsAboveTheAgentCapRefused(t *testing.T) {
	ctx := context.Background()
	svc, routeStore, mappingID, _ := instanceSpecFixture(t, nil, nil)
	if maxRuntimeSpecInstances != 64 || routing.MaxInstanceIndex != 63 {
		t.Fatalf("instance cap = %d (index cap %d), want 64 instances, indices 0..63", maxRuntimeSpecInstances, routing.MaxInstanceIndex)
	}
	base := func() PutRuntimeSpecRequest {
		return PutRuntimeSpecRequest{Enabled: true, Binary: "/usr/local/bin/llama-server", Replicable: true}
	}
	for _, pair := range []struct{ min, max int }{{65, 0}, {0, 65}, {65, 70}, {1, 100}} {
		req := base()
		req.MinInstances, req.MaxInstances = pair.min, pair.max
		if _, err := svc.PutRuntimeSpec(ctx, ownerToken(), mappingID, req); !errors.Is(err, ErrRuntimeSpecInstancesInvalid) {
			t.Fatalf("min %d max %d: err = %v, want ErrRuntimeSpecInstancesInvalid", pair.min, pair.max, err)
		}
		if _, ok, err := routeStore.RuntimeSpecByMapping(ctx, mappingID); err != nil || ok {
			t.Fatalf("min %d max %d: a refused PUT stored a spec (ok=%v err=%v)", pair.min, pair.max, ok, err)
		}
	}
	for _, pair := range []struct{ min, max int }{{64, 64}, {0, 64}, {64, 0}} {
		req := base()
		req.MinInstances, req.MaxInstances = pair.min, pair.max
		dto, err := svc.PutRuntimeSpec(ctx, ownerToken(), mappingID, req)
		if err != nil || dto.MinInstances != pair.min || dto.MaxInstances != pair.max {
			t.Fatalf("min %d max %d: dto %d/%d err %v, want the pair stored", pair.min, pair.max, dto.MinInstances, dto.MaxInstances, err)
		}
	}
}

// TestEffectiveIdleTimeoutDerivesInWholeMilliseconds pins the derived term's
// arithmetic: factor times the startup is taken to the nearest whole
// millisecond before it is rounded up to seconds, so a factor without an exact
// binary form cannot push the result a second past the exact one (1.1 x 200 s
// is 220 s; computed in floating point it is 220.00000000000003, which a plain
// ceil makes 221). The floor here is 1 s, so it decides nothing.
func TestEffectiveIdleTimeoutDerivesInWholeMilliseconds(t *testing.T) {
	cases := []struct {
		factor    float64
		startupMS int64
		want      int
	}{
		{1.1, 200000, 220},
		{2.2, 100000, 220},
		{1.15, 200000, 230},
		{0.7, 100000, 70},
		{2.5, 1001, 3}, // 2502.5 ms -> 3 s
		{1.5, 1333, 2}, // 1999.5 ms -> 2 s
		{5, 30001, 151},
		{1.1, 0, 1},
	}
	for _, tc := range cases {
		spec := routing.RuntimeSpec{Replicable: true, StartupMeasuredMS: tc.startupMS}
		if got := effectiveIdleTimeoutSeconds(spec, 0, tc.factor, 1); got != tc.want {
			t.Errorf("factor %v x %d ms: got %d s, want %d s", tc.factor, tc.startupMS, got, tc.want)
		}
	}
}

// TestEffectiveIdleTimeoutClampsToTheCap pins that every term of a replicable
// spec's pushed idle timeout -- the manual value and the floor as well as the
// derived term -- is held at maxDerivedIdleTimeoutSeconds: the agent turns the
// seconds into a time.Duration, which wraps past about 292 years, and a wrapped
// timeout unloads an instance at once. NewService clamps a configured floor the
// same way. A spec that is not replicable is pushed as stored, as before
// instances existed.
func TestEffectiveIdleTimeoutClampsToTheCap(t *testing.T) {
	huge := math.MaxInt
	if got := effectiveIdleTimeoutSeconds(routing.RuntimeSpec{Replicable: true, IdleTimeoutSeconds: huge}, 0, 5, 120); got != maxDerivedIdleTimeoutSeconds {
		t.Errorf("a huge manual value: got %d, want the cap %d", got, maxDerivedIdleTimeoutSeconds)
	}
	if got := effectiveIdleTimeoutSeconds(routing.RuntimeSpec{Replicable: true}, 0, 5, huge); got != maxDerivedIdleTimeoutSeconds {
		t.Errorf("a huge floor: got %d, want the cap %d", got, maxDerivedIdleTimeoutSeconds)
	}
	if got := effectiveIdleTimeoutSeconds(routing.RuntimeSpec{IdleTimeoutSeconds: huge}, 0, 5, 120); got != huge {
		t.Errorf("a spec that is not replicable: got %d, want its stored %d unchanged", got, huge)
	}
	if svc := NewService(ServiceDeps{InstanceIdleFloorSeconds: huge}); svc.instanceIdleFloorSeconds != maxDerivedIdleTimeoutSeconds {
		t.Errorf("configured floor %d: NewService kept %d, want the cap %d", huge, svc.instanceIdleFloorSeconds, maxDerivedIdleTimeoutSeconds)
	}
}

// TestRuntimeSpecDTOKeepsTheManualIdleTimeoutBesideTheEffectiveOne pins the
// two idle fields of a replicable spec's portal DTO: idle_timeout_seconds is
// the operator's manual value as stored, and effective_idle_timeout_seconds is
// the derived one the document pushes. A benchmark writer's full-document
// re-save (putRequestFromDTO) therefore carries the manual value back, never
// the derived one, which would otherwise turn into a manual override.
func TestRuntimeSpecDTOKeepsTheManualIdleTimeoutBesideTheEffectiveOne(t *testing.T) {
	ctx := context.Background()
	svc, routeStore, mappingID, _ := instanceSpecFixture(t, nil, nil)
	put, err := svc.PutRuntimeSpec(ctx, ownerToken(), mappingID, PutRuntimeSpecRequest{
		Enabled: true, Binary: "/usr/local/bin/llama-server", Replicable: true, IdleTimeoutSeconds: 60,
	})
	if err != nil {
		t.Fatalf("PutRuntimeSpec: %v", err)
	}
	if err := routeStore.UpdateRuntimeSpecStartupMeasured(ctx, put.ID, 30000); err != nil {
		t.Fatalf("UpdateRuntimeSpecStartupMeasured: %v", err)
	}
	got, err := svc.GetRuntimeSpec(ctx, ownerToken(), mappingID)
	if err != nil {
		t.Fatalf("GetRuntimeSpec: %v", err)
	}
	keys := wireKeys(t, got)
	if string(keys["idle_timeout_seconds"]) != "60" || string(keys["effective_idle_timeout_seconds"]) != "150" {
		t.Fatalf("wire idle_timeout_seconds %s effective_idle_timeout_seconds %s, want the manual 60 beside the derived 150",
			keys["idle_timeout_seconds"], keys["effective_idle_timeout_seconds"])
	}
	if req := putRequestFromDTO(got); req.IdleTimeoutSeconds != 60 {
		t.Fatalf("putRequestFromDTO idle_timeout_seconds = %d, want the manual 60", req.IdleTimeoutSeconds)
	}
}

// TestAgentRuntimeDocInstanceFieldsOnlyForAnAgentThatRunsInstances pins which
// agents get a replicable spec's instance settings: only one that declares
// runtime_instances gets replicable / min_instances / max_instances and the
// derived idle timeout. An older agent -- and an agent the gateway has not
// heard from yet, whose features are unknown -- gets exactly the document it
// got before instances existed: none of the three keys and the stored manual
// idle timeout, which it reads as it always did (here 45 s; 0 would mean never
// unload). The portal's read-only effective_idle_timeout_seconds reports what
// that server's document actually carries, while the spec's own replicable
// setting stays as the operator saved it.
func TestAgentRuntimeDocInstanceFieldsOnlyForAnAgentThatRunsInstances(t *testing.T) {
	ctx := context.Background()
	svc, routeStore, mappingID, serverID := instanceSpecFixture(t, nil, nil)
	put, err := svc.PutRuntimeSpec(ctx, ownerToken(), mappingID, PutRuntimeSpecRequest{
		Enabled: true, Binary: "/usr/local/bin/llama-server", Replicable: true, MinInstances: 1, MaxInstances: 3, IdleTimeoutSeconds: 45,
	})
	if err != nil {
		t.Fatalf("PutRuntimeSpec: %v", err)
	}
	if err := routeStore.UpdateRuntimeSpecStartupMeasured(ctx, put.ID, 30000); err != nil {
		t.Fatalf("UpdateRuntimeSpecStartupMeasured: %v", err)
	}
	cases := []struct {
		name      string
		features  AgentFeatureReader
		instances bool
		wantIdle  int
	}{
		{"an agent that runs instances", fakeAgentFeatures{serverID: {"runtime_manager", AgentFeatureRuntimeInstances}}, true, 150},
		{"an older agent", fakeAgentFeatures{serverID: {"runtime_manager"}}, false, 45},
		{"another server's agent runs instances", fakeAgentFeatures{"srv_other": {AgentFeatureRuntimeInstances}}, false, 45},
		{"no feature reader", nil, false, 45},
	}
	for _, tc := range cases {
		svc.agentFeatures = tc.features
		doc, err := svc.AgentRuntimeConfig(ctx, serverID)
		if err != nil || len(doc.Specs) != 1 {
			t.Fatalf("%s: AgentRuntimeConfig: %d specs, err %v", tc.name, len(doc.Specs), err)
		}
		keys := wireKeys(t, doc.Specs[0])
		for _, key := range []string{"replicable", "min_instances", "max_instances"} {
			if _, present := keys[key]; present != tc.instances {
				t.Errorf("%s: pushed key %s present = %v, want %v", tc.name, key, present, tc.instances)
			}
		}
		if got := doc.Specs[0].IdleTimeoutSeconds; got != tc.wantIdle {
			t.Errorf("%s: pushed idle_timeout_seconds = %d, want %d", tc.name, got, tc.wantIdle)
		}
		dto, err := svc.GetRuntimeSpec(ctx, ownerToken(), mappingID)
		if err != nil {
			t.Fatalf("%s: GetRuntimeSpec: %v", tc.name, err)
		}
		if dto.EffectiveIdleTimeoutSeconds != tc.wantIdle || dto.IdleTimeoutSeconds != 45 || !dto.Replicable {
			t.Errorf("%s: portal effective %d manual %d replicable %v, want effective %d (the pushed value), manual 45, replicable true",
				tc.name, dto.EffectiveIdleTimeoutSeconds, dto.IdleTimeoutSeconds, dto.Replicable, tc.wantIdle)
		}
	}
}
