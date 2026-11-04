// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"errors"
	"fmt"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/inference"
	"op-ai-gateway/internal/provider"
	"op-ai-gateway/internal/routing"
	"slices"
	"testing"
	"time"
)

// countingSpecReader is a test instanceSpecReader: specs by mapping id, every
// read counted, and err (when set) returned instead.
type countingSpecReader struct {
	specs map[string]routing.RuntimeSpec
	err   error
	reads []string
	ctxs  []context.Context
}

// RuntimeSpecByMapping fails like a store would with a context that is
// already done.
func (c *countingSpecReader) RuntimeSpecByMapping(ctx context.Context, mappingID string) (routing.RuntimeSpec, bool, error) {
	c.reads = append(c.reads, mappingID)
	c.ctxs = append(c.ctxs, ctx)
	if err := ctx.Err(); err != nil {
		return routing.RuntimeSpec{}, false, err
	}
	if c.err != nil {
		return routing.RuntimeSpec{}, false, c.err
	}
	spec, ok := c.specs[mappingID]
	return spec, ok, nil
}

// instanceSourceFixture is an instance source over mapping map_m (upstream
// model m) whose spec S is replicable, on srv1, whose agent declares
// runtime_instances.
func instanceSourceFixture() (*instanceSource, *countingSpecReader, routing.ModelMapping) {
	reader := &countingSpecReader{specs: map[string]routing.RuntimeSpec{
		"map_m": {ID: "S", MappingID: "map_m", Replicable: true, MaxInstances: 4, InstanceParallelism: 2},
	}}
	features := newAgentFeaturesRegistry()
	features.Set("srv1", []string{"runtime_manager", runtimeInstancesFeature})
	src := newInstanceSource(reader, features, newRuntimeStatusRegistry())
	return src, reader, routing.ModelMapping{ID: "map_m", AppModelName: "m"}
}

// specRow is spec specID's instance-k status row in state, addressed as an
// agent addresses it.
func specRow(specID, model string, k int, state string) RuntimeStatusDTO {
	return RuntimeStatusDTO{SpecID: specID, Model: routing.InstanceAddress(model, k), Instance: k, State: state}
}

func TestInstanceSourceReportsRunningAndPresentInstances(t *testing.T) {
	src, _, m := instanceSourceFixture()
	src.registry.publish("srv1", []RuntimeStatusDTO{
		specRow("S", "m", 0, "running"),
		specRow("S", "m", 3, "running"),
		specRow("S", "m", 1, "starting"),
		specRow("S", "m", 2, "running"),
		specRow("S", "m", 4, "crashed"),
		specRow("T", "t", 5, "running"),                                // another spec's instance
		{SpecID: "S", Model: "other#6", Instance: 6, State: "running"}, // not an address this mapping sends
	})
	view, ok := src.InstanceView(context.Background(), "srv1", m)
	if !ok {
		t.Fatal("InstanceView ok = false, want true for a replicable spec on an agent that runs instances")
	}
	if !view.Replicable || view.Parallelism != 2 || view.MaxInstances != 4 {
		t.Fatalf("view = %+v, want replicable, parallelism 2, max 4", view)
	}
	if !slices.Equal(view.Running, []int{2, 3}) {
		t.Fatalf("Running = %v, want [2 3] (k >= 1, running, this spec's own addresses)", view.Running)
	}
	if !slices.Equal(view.Present, []int{1, 2, 3}) {
		t.Fatalf("Present = %v, want [1 2 3] (k >= 1, starting or running)", view.Present)
	}
}

func TestInstanceSourceParallelismDefaultsToOne(t *testing.T) {
	src, reader, m := instanceSourceFixture()
	reader.specs["map_m"] = routing.RuntimeSpec{ID: "S", MappingID: "map_m", Replicable: true}
	if view, ok := src.InstanceView(context.Background(), "srv1", m); !ok || view.Parallelism != 1 {
		t.Fatalf("view = %+v (ok %v), want parallelism 1 for an unset instance_parallelism", view, ok)
	}
}

// An agent that restarts or reconnects sends a full snapshot without the
// instances it no longer runs: they stop being candidates on that snapshot.
func TestInstanceSourceDropsVanishedRows(t *testing.T) {
	src, _, m := instanceSourceFixture()
	src.registry.publish("srv1", []RuntimeStatusDTO{specRow("S", "m", 0, "running"), specRow("S", "m", 2, "running")})
	if view, _ := src.InstanceView(context.Background(), "srv1", m); !slices.Equal(view.Running, []int{2}) {
		t.Fatalf("Running = %v before the restart, want [2]", view.Running)
	}
	src.registry.publish("srv1", []RuntimeStatusDTO{specRow("S", "m", 0, "running")})
	view, ok := src.InstanceView(context.Background(), "srv1", m)
	if !ok || len(view.Running) != 0 || len(view.Present) != 0 {
		t.Fatalf("view = %+v (ok %v) after a snapshot without S#2, want ok with no instance running or present", view, ok)
	}
	src.registry.publish("srv1", nil)
	if view, ok := src.InstanceView(context.Background(), "srv1", m); !ok || len(view.Running) != 0 {
		t.Fatalf("view = %+v (ok %v) after an empty snapshot, want ok with nothing running", view, ok)
	}
}

// Only an agent known to declare runtime_instances runs instances: one that
// lacks it, and one whose features are not known yet, are instance 0 alone.
func TestInstanceSourceRequiresFeature(t *testing.T) {
	src, reader, m := instanceSourceFixture()
	src.registry.publish("srv1", []RuntimeStatusDTO{specRow("S", "m", 2, "running")})
	src.registry.publish("srv_old", []RuntimeStatusDTO{specRow("S", "m", 2, "running")})
	src.features.Set("srv_old", []string{"runtime_manager", "runtime_model_probe"})

	if _, ok := src.InstanceView(context.Background(), "srv_old", m); ok {
		t.Fatal("agent without runtime_instances: ok = true, want false")
	}
	if _, ok := src.InstanceView(context.Background(), "srv_unknown", m); ok {
		t.Fatal("agent whose features are not known: ok = true, want false")
	}
	if len(reader.reads) != 0 {
		t.Fatalf("spec reads = %v, want none for an agent that runs no instances", reader.reads)
	}
	if _, ok := src.InstanceView(context.Background(), "srv1", m); !ok {
		t.Fatal("agent with runtime_instances: ok = false, want true")
	}
}

func TestInstanceSourceNeedsReplicableSpec(t *testing.T) {
	src, reader, m := instanceSourceFixture()
	reader.specs["map_plain"] = routing.RuntimeSpec{ID: "P", MappingID: "map_plain"}
	if _, ok := src.InstanceView(context.Background(), "srv1", routing.ModelMapping{ID: "map_plain", AppModelName: "p"}); ok {
		t.Fatal("spec not replicable: ok = true, want false")
	}
	if _, ok := src.InstanceView(context.Background(), "srv1", routing.ModelMapping{ID: "map_nospec", AppModelName: "n"}); ok {
		t.Fatal("no spec: ok = true, want false")
	}
	src.store = &countingSpecReader{err: errors.New("store down")}
	src.specs = map[string]instanceSpecEntry{}
	if _, ok := src.InstanceView(context.Background(), "srv1", m); ok {
		t.Fatal("spec read failed: ok = true, want false")
	}
}

// A failed spec read is remembered for a second: a degraded store is asked
// (and its failure logged) once a second per mapping, not on every admission
// turn of every request; once that second has passed it is asked again, and
// a store that has recovered answers.
func TestInstanceSourceRemembersAFailedReadForASecond(t *testing.T) {
	src, _, m := instanceSourceFixture()
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	src.now = func() time.Time { return now }
	failing := &countingSpecReader{err: errors.New("store down")}
	src.store = failing

	if _, ok := src.InstanceView(context.Background(), "srv1", m); ok {
		t.Fatal("spec read failed: ok = true, want false")
	}
	now = now.Add(900 * time.Millisecond)
	if _, ok := src.InstanceView(context.Background(), "srv1", m); ok || len(failing.reads) != 1 {
		t.Fatalf("within a second of the failure: ok %v after %d reads, want false after 1", ok, len(failing.reads))
	}
	now = now.Add(100 * time.Millisecond)
	src.InstanceView(context.Background(), "srv1", m)
	if len(failing.reads) != 2 {
		t.Fatalf("spec reads a second after the failure = %d, want 2", len(failing.reads))
	}
	failing.err = nil
	failing.specs = map[string]routing.RuntimeSpec{"map_m": {ID: "S", MappingID: "map_m", Replicable: true}}
	now = now.Add(time.Second)
	if _, ok := src.InstanceView(context.Background(), "srv1", m); !ok {
		t.Fatal("after the store recovered: ok = false, want the replicable spec")
	}
}

// The spec is read under the request's context, so the read is traced under
// the request and ends with it. A read that failed because its request was
// already gone says nothing about the store: it is not remembered, and the
// next request reads the spec afresh.
func TestInstanceSourceReadsSpecUnderRequestContext(t *testing.T) {
	type ctxKey struct{}
	src, reader, m := instanceSourceFixture()
	ctx := context.WithValue(context.Background(), ctxKey{}, "req")
	if _, ok := src.InstanceView(ctx, "srv1", m); !ok {
		t.Fatal("InstanceView ok = false, want true")
	}
	if len(reader.ctxs) != 1 || reader.ctxs[0].Value(ctxKey{}) != "req" {
		t.Fatalf("spec read under %d contexts, want 1 derived from the request's", len(reader.ctxs))
	}

	src.specs = map[string]instanceSpecEntry{}
	gone, cancel := context.WithCancel(context.Background())
	cancel()
	if _, ok := src.InstanceView(gone, "srv1", m); ok {
		t.Fatal("cancelled request: ok = true, want false")
	}
	if _, ok := src.InstanceView(context.Background(), "srv1", m); !ok {
		t.Fatal("the next request: ok = false, want the spec read afresh, not the cancelled read's failure")
	}
	if len(reader.reads) != 3 {
		t.Fatalf("spec reads = %d, want 3 (the cancelled read is not remembered)", len(reader.reads))
	}
}

// The server's resolver judges an instance by the active registry's count of
// requests in flight on it: with instance 0 holding a request at its
// parallelism of one, the running instance 2 serves although instance 0
// reports the lighter load.
func TestInstanceCapIsWiredEndToEnd(t *testing.T) {
	srv := NewTestServer()
	seedReplicableRuntimeSpec(t, srv)
	ctx := context.Background()
	req := inference.Request{Model: "inst-model", APIFlavor: "openai_chat"}
	ingestInstanceRows(t, srv, `"runtime_manager","runtime_model_probe","runtime_instances"`, instRow(0, "running", 0, 0), instRow(2, "running", 5, 3))

	if target, err := srv.Resolver.Resolve(ctx, auth.Token{}, req); err != nil || target.InstanceKey != "map_rspec_inst#0" {
		t.Fatalf("nothing in flight: target %q, err %v; want the lighter instance 0, map_rspec_inst#0", target.InstanceKey, err)
	}
	srv.Active.Add(ActiveRequest{ID: "held", ServerID: "mock-host-qwen", InstanceKey: "map_rspec_inst#0"})
	defer srv.Active.Remove("held")
	if target, err := srv.Resolver.Resolve(ctx, auth.Token{}, req); err != nil || target.InstanceKey != "map_rspec_inst#2" {
		t.Fatalf("instance 0 at capacity: target %q, err %v; want map_rspec_inst#2", target.InstanceKey, err)
	}
}

// The spec behind a mapping is read at most once per five seconds per
// mapping, so the resolver does not query the store for every candidate of
// every request.
func TestInstanceSourceCachesSpecForFiveSeconds(t *testing.T) {
	src, reader, m := instanceSourceFixture()
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	src.now = func() time.Time { return now }

	src.InstanceView(context.Background(), "srv1", m)
	now = now.Add(4900 * time.Millisecond)
	src.InstanceView(context.Background(), "srv1", m)
	if len(reader.reads) != 1 {
		t.Fatalf("spec reads within 5s = %d, want 1", len(reader.reads))
	}
	now = now.Add(100 * time.Millisecond)
	src.InstanceView(context.Background(), "srv1", m)
	if len(reader.reads) != 2 {
		t.Fatalf("spec reads after 5s = %d, want 2", len(reader.reads))
	}
	src.InstanceView(context.Background(), "srv1", routing.ModelMapping{ID: "map_other", AppModelName: "o"})
	if !slices.Equal(reader.reads, []string{"map_m", "map_m", "map_other"}) {
		t.Fatalf("spec reads = %v, want one per mapping", reader.reads)
	}
	// A spec switched off replicable is seen once the cached read expires.
	reader.specs["map_m"] = routing.RuntimeSpec{ID: "S", MappingID: "map_m"}
	if _, ok := src.InstanceView(context.Background(), "srv1", m); !ok {
		t.Fatal("within 5s of the last read: ok = false, want the cached replicable spec")
	}
	now = now.Add(5 * time.Second)
	if _, ok := src.InstanceView(context.Background(), "srv1", m); ok {
		t.Fatal("after 5s: ok = true, want the re-read spec that is no longer replicable")
	}
	src.mu.Lock()
	cached := len(src.specs)
	src.mu.Unlock()
	if cached != 1 {
		t.Fatalf("cached specs = %d, want 1 (expired entries are dropped on the next read)", cached)
	}
}

func TestUsageRecordsBaseProviderModel(t *testing.T) {
	cases := []struct {
		name, providerModel, instanceKey, want string
	}{
		{"burst instance", "m#2", "map_m#2", "m"},
		{"instance 0", "m", "map_m#0", "m"},
		{"a model literally named with a '#'", "foo#2", "map_f#0", "foo#2"},
		{"an instance of a model named with a '#'", "foo#2#3", "map_f#3", "foo#2"},
		{"a target without an instance key", "bar#2", "", "bar#2"},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := NewTestServer()
			user := fmt.Sprintf("usr_inst_%d", i)
			target := routing.Target{RouteID: "map_m", Provider: routing.ProviderServerAgent, ServerID: "srv1", ProviderModel: tc.providerModel, InstanceKey: tc.instanceKey}
			srv.recordUsage(time.Now(), auth.Token{ID: "tok", UserID: user}, inference.Request{Model: "gm"}, target, provider.Response{}, "", "success", usageMeta{}, "req_inst", nil, nil)
			events := srv.Usage.ByUser(user)
			if len(events) != 1 {
				t.Fatalf("events = %d, want 1", len(events))
			}
			if events[0].ProviderModel != tc.want || events[0].RouteID != "map_m" {
				t.Fatalf("recorded ProviderModel %q RouteID %q, want %q map_m", events[0].ProviderModel, events[0].RouteID, tc.want)
			}
		})
	}
}

// seedReplicableRuntimeSpec seeds a replicable spec rspec_inst on
// mock-host-qwen behind gateway model inst-model (upstream inst-model).
func seedReplicableRuntimeSpec(t *testing.T, srv *Server) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	if err := srv.Routes.CreateApplication(ctx, routing.Application{
		ID: "app_rspec_inst", ServerID: "mock-host-qwen", Type: routing.ProviderServerAgent, Port: 8081, Scheme: "http",
		APIFlavors: []string{routing.APIFlavorOpenAI}, Priority: 10, Weight: 50, TimeoutMS: 30000,
		Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create application: %v", err)
	}
	if err := srv.Routes.CreateMapping(ctx, routing.ModelMapping{
		ID: "map_rspec_inst", ApplicationID: "app_rspec_inst", GatewayModelName: "inst-model", AppModelName: "inst-model",
		Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create mapping: %v", err)
	}
	if err := srv.Routes.UpsertRuntimeSpec(ctx, routing.RuntimeSpec{
		ID: "rspec_inst", MappingID: "map_rspec_inst", Enabled: true, Binary: "/usr/bin/llama-server", Args: "[]", Env: "{}",
		Replicable: true, MaxInstances: 4, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("upsert runtime spec: %v", err)
	}
}

// instRow is rspec_inst's instance-k runtime entry with its live load.
func instRow(k int, state string, active, queue int) string {
	model := routing.InstanceAddress("inst-model", k)
	return fmt.Sprintf(`{"spec_id":"rspec_inst","instance":%d,"model":%q,"state":%q,"active_requests":%d,"queue_depth":%d}`, k, model, state, active, queue)
}

// TestRunningInstanceIsRoutedEndToEnd wires the instance source as New does:
// a telemetry sample reporting a running instance of a replicable spec makes
// that instance a candidate of the server's own resolver, which routes to the
// idle instance over the loaded instance 0; a later sample without it, and
// an agent that does not declare runtime_instances, leave instance 0 alone.
func TestRunningInstanceIsRoutedEndToEnd(t *testing.T) {
	srv := NewTestServer()
	seedReplicableRuntimeSpec(t, srv)
	ctx := context.Background()
	req := inference.Request{Model: "inst-model", APIFlavor: "openai_chat"}
	const features = `"runtime_manager","runtime_model_probe","runtime_instances"`

	ingestInstanceRows(t, srv, features, instRow(0, "running", 10, 5), instRow(2, "running", 0, 0), instRow(3, "starting", 0, 0))
	target, err := srv.Resolver.Resolve(ctx, auth.Token{}, req)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if target.ProviderModel != "inst-model#2" || target.InstanceKey != "map_rspec_inst#2" || target.RouteID != "map_rspec_inst" {
		t.Fatalf("target = ProviderModel %q InstanceKey %q RouteID %q, want the idle running instance inst-model#2 (map_rspec_inst#2) of map_rspec_inst", target.ProviderModel, target.InstanceKey, target.RouteID)
	}

	ingestInstanceRows(t, srv, features, instRow(0, "running", 10, 5))
	if target, err := srv.Resolver.Resolve(ctx, auth.Token{}, req); err != nil || target.ProviderModel != "inst-model" {
		t.Fatalf("after the instance vanished: target %q, err %v; want instance 0, inst-model", target.ProviderModel, err)
	}

	ingestInstanceRows(t, srv, `"runtime_manager","runtime_model_probe"`, instRow(0, "running", 10, 5), instRow(2, "running", 0, 0))
	if target, err := srv.Resolver.Resolve(ctx, auth.Token{}, req); err != nil || target.ProviderModel != "inst-model" {
		t.Fatalf("agent without runtime_instances: target %q, err %v; want instance 0, inst-model", target.ProviderModel, err)
	}
}
