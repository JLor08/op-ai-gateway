// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"sync"
	"testing"
)

// TestAgentProxyStatusRegistry pins the core contract: Report/Status round-trip
// with defensive copies both ways, Report replaces (not merges) the prior
// snapshot, empty-id Report is a no-op, Retain bounds the fleet, and every
// method is safe to call on a nil registry.
func TestAgentProxyStatusRegistry(t *testing.T) {
	t.Run("nil is a safe no-op", func(t *testing.T) {
		var r *AgentProxyStatusRegistry
		r.Report("s1", []ProxyRouteStatus{{Listen: 8600, TLSActive: true}})
		if got := r.Status("s1"); got != nil {
			t.Fatalf("nil registry returned a status: %+v", got)
		}
		r.Retain(map[string]bool{"s1": true}) // must not panic
	})

	t.Run("unseen server reports nil", func(t *testing.T) {
		r := NewAgentProxyStatusRegistry()
		if got := r.Status("s1"); got != nil {
			t.Fatalf("unseen server returned non-nil status: %+v", got)
		}
	})

	t.Run("Report/Status round-trip with defensive copies", func(t *testing.T) {
		r := NewAgentProxyStatusRegistry()
		routes := []ProxyRouteStatus{{Listen: 8600, TLSActive: true}, {Listen: 8601, TLSActive: false}}
		r.Report("s1", routes)

		got := r.Status("s1")
		if len(got) != 2 || got[0] != routes[0] || got[1] != routes[1] {
			t.Fatalf("Status = %+v, want %+v", got, routes)
		}

		// Mutating the caller's input slice after Report must not change the stored copy.
		routes[0].TLSActive = false
		if got2 := r.Status("s1"); !got2[0].TLSActive {
			t.Fatal("Report did not defensively copy its input slice")
		}

		// Mutating the returned slice must not change the registry's state.
		got[1].Listen = 9999
		if got3 := r.Status("s1"); got3[1].Listen == 9999 {
			t.Fatal("Status did not defensively copy its stored slice")
		}
	})

	t.Run("Report replaces rather than merges the prior snapshot", func(t *testing.T) {
		r := NewAgentProxyStatusRegistry()
		r.Report("s1", []ProxyRouteStatus{{Listen: 8600, TLSActive: true}, {Listen: 8601, TLSActive: true}})
		r.Report("s1", []ProxyRouteStatus{{Listen: 8600, TLSActive: false}})

		got := r.Status("s1")
		want := []ProxyRouteStatus{{Listen: 8600, TLSActive: false}}
		if len(got) != len(want) || got[0] != want[0] {
			t.Fatalf("Report did not replace the prior snapshot: got %+v, want %+v", got, want)
		}
	})

	t.Run("Report on empty id is a no-op", func(t *testing.T) {
		r := NewAgentProxyStatusRegistry()
		r.Report("", []ProxyRouteStatus{{Listen: 8600, TLSActive: true}})
		if got := r.Status(""); got != nil {
			t.Fatalf("empty-id Report was not a no-op: %+v", got)
		}
	})

	t.Run("Retain bounds the fleet", func(t *testing.T) {
		r := NewAgentProxyStatusRegistry()
		r.Report("live", []ProxyRouteStatus{{Listen: 8600, TLSActive: true}})
		r.Report("gone", []ProxyRouteStatus{{Listen: 8601, TLSActive: false}})

		r.Retain(map[string]bool{"live": true})
		if got := r.Status("live"); got == nil {
			t.Fatal("Retain evicted a live server")
		}
		if got := r.Status("gone"); got != nil {
			t.Fatalf("Retain kept a server missing from the live set: %+v", got)
		}

		// An empty/nil live set clears everything (a valid transient during a
		// fresh boot / enumeration failure, matching the sibling registries).
		r.Retain(map[string]bool{})
		if got := r.Status("live"); got != nil {
			t.Fatal("Retain(empty) did not clear the registry")
		}
	})

	t.Run("concurrent Report/Status/Retain is race-clean", func(t *testing.T) {
		r := NewAgentProxyStatusRegistry()
		var wg sync.WaitGroup
		for i := 0; i < 50; i++ {
			wg.Add(3)
			go func(i int) {
				defer wg.Done()
				r.Report("s1", []ProxyRouteStatus{{Listen: 8600 + i, TLSActive: i%2 == 0}})
			}(i)
			go func() {
				defer wg.Done()
				_ = r.Status("s1")
			}()
			go func() {
				defer wg.Done()
				r.Retain(map[string]bool{"s1": true})
			}()
		}
		wg.Wait()
	})
}
