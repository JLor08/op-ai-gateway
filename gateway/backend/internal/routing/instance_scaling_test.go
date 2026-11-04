// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package routing

import (
	"context"
	"testing"
	"time"
)

// CandidateServesNow is the first pass of selectCandidate, read from outside a
// request: for every gate, a lone candidate it passes is the one a request
// selects, and one it fails is not selected -- and a sticky pin to it is kept
// exactly when it passes (affinityServer). A gate added to the pass and not
// to CandidateServesNow (or the reverse) breaks a row here.
func TestCandidateServesNowMatchesSelection(t *testing.T) {
	now := instanceCapacityNow
	cases := []struct {
		name        string
		edit        func(*MappingCandidate)
		unreachable bool
		busy        bool
		nilCheckers bool
		want        bool
	}{
		{name: "selectable, reachable, not busy", want: true},
		{name: "no checkers", nilCheckers: true, want: true},
		{name: "server in maintenance", edit: func(c *MappingCandidate) { c.Server.Status = ServerStatusMaintenance }},
		{name: "server unhealthy", edit: func(c *MappingCandidate) { c.Server.HealthStatus = HealthUnhealthy }},
		{name: "server degraded", edit: func(c *MappingCandidate) { c.Server.HealthStatus = HealthDegraded }, want: true},
		{name: "application unreachable", unreachable: true},
		{name: "server benchmark-busy", busy: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := instanceCandidateStore(t, now, 0)
			var r *Resolver
			if tc.nilCheckers {
				r = NewResolver(store, func() time.Time { return now }, nil)
			} else {
				r = NewResolver(store, func() time.Time { return now }, fakeReachability{unreachable: map[string]bool{"app_inst": tc.unreachable}})
				r.SetServerBusyChecker(fakeBusy{busy: map[string]bool{"srv_inst": tc.busy}})
			}
			pool, err := store.ActiveMappingsForModel(context.Background(), "qwen-coder", APIFlavorOpenAI)
			if err != nil || len(pool) != 1 {
				t.Fatalf("candidates = %d, %v; want one", len(pool), err)
			}
			c := pool[0]
			if tc.edit != nil {
				tc.edit(&c)
			}
			if got := r.CandidateServesNow(c); got != tc.want {
				t.Fatalf("CandidateServesNow = %v, want %v", got, tc.want)
			}
			_, selected, err := r.selectCandidate(context.Background(), []MappingCandidate{c}, chatRequest("qwen-coder"), now)
			if err != nil {
				t.Fatalf("selectCandidate: %v", err)
			}
			if selected != tc.want {
				t.Fatalf("selectCandidate selected = %v, CandidateServesNow = %v: the two disagree", selected, tc.want)
			}
			if err := store.UpdateAIServer(context.Background(), c.Server); err != nil {
				t.Fatalf("UpdateAIServer: %v", err)
			}
			if _, usable, err := r.affinityServer(context.Background(), c.Application); err != nil || usable != tc.want {
				t.Fatalf("affinityServer usable = %v, %v; CandidateServesNow = %v: the pin and the selection disagree", usable, err, tc.want)
			}
		})
	}
}

// InstanceTarget is the target routing builds for instance k of the mapping:
// the instance's address and key, the mapping as RouteID.
func TestInstanceTargetAddressesTheInstance(t *testing.T) {
	now := instanceCapacityNow
	store := instanceCandidateStore(t, now, 0)
	r := NewResolver(store, func() time.Time { return now }, nil)
	pool, err := store.ActiveMappingsForModel(context.Background(), "qwen-coder", APIFlavorOpenAI)
	if err != nil || len(pool) != 1 {
		t.Fatalf("candidates = %d, %v; want one", len(pool), err)
	}
	target, err := r.InstanceTarget(context.Background(), pool[0], 3, APIFlavorOpenAI)
	if err != nil {
		t.Fatalf("InstanceTarget: %v", err)
	}
	if target.ProviderModel != "qwen2.5#3" || target.InstanceKey != "map_inst#3" || target.RouteID != "map_inst" ||
		!target.InstanceCapable || target.ServerID != "srv_inst" || target.Endpoint != "http://inst.test:8081" || target.Model != "qwen-coder" {
		t.Fatalf("target = %+v, want instance 3 of map_inst on srv_inst", target)
	}
}
