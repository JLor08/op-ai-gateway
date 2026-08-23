// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"reflect"
	"sort"
	"testing"
	"time"
)

func TestLoadedModelRegistryGatewayProbe(t *testing.T) {
	r := NewLoadedModelRegistry()
	r.SetGatewayProbe("app1", []string{"m2", "m1", "m1"}) // unsorted + dup
	got := r.LoadedAppModels("app1", "srvX")
	sort.Strings(got)
	if !reflect.DeepEqual(got, []string{"m1", "m2"}) {
		t.Fatalf("gateway probe = %v, want [m1 m2]", got)
	}
	// Unknown app/server returns nil.
	if got := r.LoadedAppModels("nope", "nope"); got != nil {
		t.Fatalf("unknown = %v, want nil", got)
	}
}

func TestLoadedModelRegistryAgentWinsWhenFresh(t *testing.T) {
	now := time.Now()
	r := NewLoadedModelRegistry()
	r.now = func() time.Time { return now }
	r.SetGatewayProbe("app1", []string{"gateway-model"})
	r.SetAgentReport("srv1", []string{"agent-model"})

	// A fresh agent report for the server overrides the gateway poll for its apps.
	if got := r.LoadedAppModels("app1", "srv1"); !reflect.DeepEqual(got, []string{"agent-model"}) {
		t.Fatalf("fresh agent report = %v, want [agent-model]", got)
	}
	// A DIFFERENT server has no agent report -> gateway poll still applies.
	if got := r.LoadedAppModels("app1", "srv2"); !reflect.DeepEqual(got, []string{"gateway-model"}) {
		t.Fatalf("other server = %v, want [gateway-model]", got)
	}
}

func TestLoadedModelRegistryAgentTTLExpiry(t *testing.T) {
	now := time.Now()
	r := NewLoadedModelRegistry()
	r.now = func() time.Time { return now }
	r.SetGatewayProbe("app1", []string{"gateway-model"})
	r.SetAgentReport("srv1", []string{"agent-model"})

	// Advance beyond the agent TTL: the stale agent report is ignored and the
	// gateway poll takes over again.
	now = now.Add(defaultAgentLoadedTTL + time.Second)
	if got := r.LoadedAppModels("app1", "srv1"); !reflect.DeepEqual(got, []string{"gateway-model"}) {
		t.Fatalf("expired agent report = %v, want [gateway-model]", got)
	}
}

func TestLoadedModelRegistryRetainEvictsDeleted(t *testing.T) {
	r := NewLoadedModelRegistry()
	r.SetGatewayProbe("appKeep", []string{"m1"})
	r.SetGatewayProbe("appGone", []string{"m2"})
	r.SetAgentReport("srvKeep", []string{"a1"})
	r.SetAgentReport("srvGone", []string{"a2"})

	// Retain only the live ids: the deleted app/server entries are evicted.
	r.Retain(map[string]struct{}{"appKeep": {}}, map[string]struct{}{"srvKeep": {}})

	if got := r.LoadedAppModels("appKeep", "other"); !reflect.DeepEqual(got, []string{"m1"}) {
		t.Fatalf("appKeep should survive Retain: %v", got)
	}
	if got := r.LoadedAppModels("appGone", "other"); got != nil {
		t.Fatalf("appGone should be evicted: %v", got)
	}
	// srvKeep's agent report survives (fresh); srvGone is gone so its app falls back.
	if got := r.LoadedAppModels("appKeep", "srvKeep"); !reflect.DeepEqual(got, []string{"a1"}) {
		t.Fatalf("srvKeep agent report should survive: %v", got)
	}
	if got := r.LoadedAppModels("appKeep", "srvGone"); !reflect.DeepEqual(got, []string{"m1"}) {
		t.Fatalf("srvGone evicted -> appKeep gateway poll: %v", got)
	}
}

func TestLoadedModelRegistryNilSafe(t *testing.T) {
	var r *LoadedModelRegistry
	r.SetGatewayProbe("a", []string{"x"}) // must not panic
	r.SetAgentReport("s", []string{"x"})
	r.Retain(nil, nil)
	if got := r.LoadedAppModels("a", "s"); got != nil {
		t.Fatalf("nil registry = %v, want nil", got)
	}
}

func TestLoadedModelRegistrySubscribePublish(t *testing.T) {
	r := NewLoadedModelRegistry()
	ch, unsub := r.Subscribe()
	defer unsub()

	r.SetGatewayProbe("app1", []string{"m1"})
	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal("expected a signal after SetGatewayProbe")
	}

	r.SetAgentReport("srv1", []string{"m1"})
	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal("expected a signal after SetAgentReport")
	}

	// Coalescing: two writes with no intervening drain leave exactly one pending signal.
	r.SetGatewayProbe("app1", []string{"m2"})
	r.SetGatewayProbe("app1", []string{"m3"})
	<-ch // drain the one pending
	select {
	case <-ch:
		t.Fatal("expected coalesced signals to leave at most one pending")
	default:
	}

	// After unsub, no more signals are delivered.
	unsub()
	r.SetGatewayProbe("app1", []string{"m4"})
	select {
	case <-ch:
		t.Fatal("expected no signal after unsubscribe")
	case <-time.After(50 * time.Millisecond):
	}
}

func TestLoadedModelRegistryNilSafeSubscribe(t *testing.T) {
	var r *LoadedModelRegistry
	ch, unsub := r.Subscribe() // must not panic
	unsub()
	select {
	case <-ch: // a nil registry returns a closed channel
	default:
		t.Fatal("nil registry Subscribe should return a closed channel")
	}
	r.publish() // must not panic
}
