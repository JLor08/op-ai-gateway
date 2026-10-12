// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"op-ai-gateway/internal/provider"
	"reflect"
	"sync"
	"testing"
)

func sampleImageOptions() provider.SdcppImageOptions {
	return provider.SdcppImageOptions{
		Samplers:      []string{"euler", "euler_a"},
		Schedulers:    []string{"karras"},
		OutputFormats: []string{"png", "jpeg"},
		Limits:        provider.SdcppImageLimits{MinWidth: 64, MaxWidth: 2048, MinHeight: 64, MaxHeight: 2048, MaxBatchCount: 4},
		Defaults:      provider.SdcppImageDefaults{Sampler: "euler", Steps: 20, ClipSkip: -1, Seed: -1, CFG: 7.5},
	}
}

func TestAppImageOptionsRegistrySetGet(t *testing.T) {
	reg := NewAppImageOptionsRegistry()
	want := sampleImageOptions()
	reg.Set("app1", want)

	got, ok := reg.Get("app1")
	if !ok {
		t.Fatalf("Get(app1) ok = false after Set, want true")
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Get(app1) = %+v, want %+v", got, want)
	}
}

func TestAppImageOptionsRegistryGetAbsent(t *testing.T) {
	reg := NewAppImageOptionsRegistry()
	reg.Set("app1", sampleImageOptions())

	got, ok := reg.Get("never-set")
	if ok {
		t.Fatalf("Get(never-set) ok = true, want false")
	}
	if !reflect.DeepEqual(got, provider.SdcppImageOptions{}) {
		t.Fatalf("Get(never-set) = %+v, want the zero value", got)
	}
}

func TestAppImageOptionsRegistrySetReplaces(t *testing.T) {
	reg := NewAppImageOptionsRegistry()
	reg.Set("app1", sampleImageOptions())
	reg.Set("app1", provider.SdcppImageOptions{Samplers: []string{"dpm++"}})

	got, _ := reg.Get("app1")
	if !reflect.DeepEqual(got.Samplers, []string{"dpm++"}) || got.Limits.MaxWidth != 0 {
		t.Fatalf("Get(app1) = %+v, want the second Set to replace the first wholesale", got)
	}
}

// TestAppImageOptionsRegistrySnapshotIsACopy: neither the map nor the slices
// inside it may alias the registry's own storage, so a caller (a handler
// building a DTO) can sort or trim what it got without corrupting the cache.
func TestAppImageOptionsRegistrySnapshotIsACopy(t *testing.T) {
	reg := NewAppImageOptionsRegistry()
	reg.Set("app1", sampleImageOptions())

	snap := reg.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("Snapshot() has %d entries, want 1", len(snap))
	}
	opts := snap["app1"]
	opts.Samplers[0] = "mutated"
	snap["app1"] = provider.SdcppImageOptions{}
	delete(snap, "app1")
	snap["intruder"] = provider.SdcppImageOptions{}

	got, ok := reg.Get("app1")
	if !ok || !reflect.DeepEqual(got, sampleImageOptions()) {
		t.Fatalf("Get(app1) = %+v, %v after mutating a Snapshot, want the original %+v", got, ok, sampleImageOptions())
	}
	if _, ok := reg.Get("intruder"); ok {
		t.Fatalf("a key added to a Snapshot leaked into the registry")
	}
}

// TestAppImageOptionsRegistryDoesNotAliasTheCaller: Set takes a private copy of
// the slices (the health loop reuses nothing, but a caller may), and Get hands
// out a private copy too.
func TestAppImageOptionsRegistryDoesNotAliasTheCaller(t *testing.T) {
	reg := NewAppImageOptionsRegistry()
	in := sampleImageOptions()
	reg.Set("app1", in)
	in.Samplers[0] = "mutated-after-set"

	got, _ := reg.Get("app1")
	if got.Samplers[0] != "euler" {
		t.Fatalf("Samplers[0] = %q, want euler: Set must copy the caller's slices", got.Samplers[0])
	}
	got.Samplers[0] = "mutated-after-get"
	again, _ := reg.Get("app1")
	if again.Samplers[0] != "euler" {
		t.Fatalf("Samplers[0] = %q, want euler: Get must copy the stored slices", again.Samplers[0])
	}
}

func TestAppImageOptionsRegistryRetain(t *testing.T) {
	reg := NewAppImageOptionsRegistry()
	reg.Set("live", sampleImageOptions())
	reg.Set("gone", sampleImageOptions())

	reg.Retain(map[string]struct{}{"live": {}})

	if _, ok := reg.Get("live"); !ok {
		t.Fatalf("Retain evicted a live app")
	}
	if _, ok := reg.Get("gone"); ok {
		t.Fatalf("Retain kept an app not in the live set")
	}
	reg.Retain(nil)
	if n := len(reg.Snapshot()); n != 0 {
		t.Fatalf("Retain(nil) left %d entries, want 0 (a nil live set evicts everything)", n)
	}
}

func TestAppImageOptionsRegistryNilSafe(t *testing.T) {
	var reg *AppImageOptionsRegistry
	// None of these may panic on a nil registry.
	reg.Set("app1", sampleImageOptions())
	reg.Retain(map[string]struct{}{"app1": {}})
	if got, ok := reg.Get("app1"); ok || !reflect.DeepEqual(got, provider.SdcppImageOptions{}) {
		t.Fatalf("nil registry Get() = %+v, %v, want zero, false", got, ok)
	}
	if got := reg.Snapshot(); got != nil {
		t.Fatalf("nil registry Snapshot() = %v, want nil", got)
	}
}

func TestAppImageOptionsRegistryConcurrent(t *testing.T) {
	reg := NewAppImageOptionsRegistry()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				reg.Set("app1", sampleImageOptions())
				reg.Get("app1")
				reg.Snapshot()
				reg.Retain(map[string]struct{}{"app1": {}})
			}
		}()
	}
	wg.Wait()
}

func TestNewServerWiresAppImageOptionsRegistry(t *testing.T) {
	supplied := NewAppImageOptionsRegistry()
	if s := New(ServerDeps{AppImageOptions: supplied}); s.AppImageOptions != supplied {
		t.Fatalf("New did not keep the supplied AppImageOptions registry")
	}
	if s := New(ServerDeps{}); s.AppImageOptions == nil {
		t.Fatalf("New left AppImageOptions nil, want a fresh registry like AppHealth")
	}
}
