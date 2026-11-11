// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/usage"
	"testing"
	"time"
)

// clusterEnergyBase is the start of every cluster energy fixture's request
// window; the cluster request runs for 36 s from it.
var clusterEnergyBase = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

// recordClusterEvent records a cluster request served by head with the given
// members, as recordUsage lists them: the head first, then each member.
func (f *energyFixture) recordClusterEvent(ev usage.Event, head string, members ...string) {
	f.t.Helper()
	ev.Host = head
	if err := f.usage.Record(ev); err != nil {
		f.t.Fatalf("record %s: %v", ev.ID, err)
	}
	hosts := []usage.EventHost{{EventID: ev.ID, ServerID: head, Role: usage.EventHostRoleHead, CreatedAt: ev.CreatedAt}}
	for _, m := range members {
		hosts = append(hosts, usage.EventHost{EventID: ev.ID, ServerID: m, Role: usage.EventHostRoleMember, CreatedAt: ev.CreatedAt})
	}
	if err := f.usage.RecordEventHosts(context.Background(), hosts); err != nil {
		f.t.Fatalf("record hosts of %s: %v", ev.ID, err)
	}
}

// constPower inserts one sample per second for serverID over
// [from, from+seconds] at watts.
func (f *energyFixture) constPower(serverID string, from time.Time, seconds int, watts float64) {
	f.t.Helper()
	for _, s := range constPowerSamples(from, seconds, watts) {
		f.insertSample(serverID, s)
	}
}

// parts returns the stored energy parts of eventID, keyed by server id.
func (f *energyFixture) parts(eventID string) map[string]usage.EnergyPart {
	f.t.Helper()
	got, err := f.usage.EnergyParts(context.Background(), eventID)
	if err != nil {
		f.t.Fatalf("energy parts of %s: %v", eventID, err)
	}
	out := make(map[string]usage.EnergyPart, len(got))
	for _, p := range got {
		out[p.ServerID] = p
	}
	return out
}

// reconcile prices one event the way a reconciler pass does, with the
// fixture's system defaults.
func (f *energyFixture) reconcile(eventID string) {
	f.t.Helper()
	sys := f.portalF.settings
	f.srv.reconcileEnergyEvent(context.Background(), f.event(eventID), clusterEnergyBase.Add(time.Hour), sys.EnergyDefaultPue, sys.EnergyDefaultWhPerToken)
}

// TestReconcileClusterEventSumsParts pins the parts of a cluster request: the
// head part is priced on the head's host as before, each member part on its own
// host, energy_wh is their sum and energy_source the weakest part. A member
// with neither telemetry nor EstimatedWatts adds 0 Wh and flags the event
// member_unpriced.
func TestReconcileClusterEventSumsParts(t *testing.T) {
	end := clusterEnergyBase.Add(36 * time.Second)

	t.Run("every part measured", func(t *testing.T) {
		f := newEnergyFixture(t, 0, 0)
		f.createServer("srv-head", routing.AIServer{})
		f.createServer("srv-mem", routing.AIServer{Pue: 1.5})
		f.constPower("srv-head", clusterEnergyBase, 36, 100)
		f.constPower("srv-mem", clusterEnergyBase, 36, 200)
		f.recordClusterEvent(usage.Event{ID: "ev_cluster", CreatedAt: end, LatencyMS: 36000, OutputTokens: 100}, "srv-head", "srv-mem")

		f.reconcile("ev_cluster")

		ev := f.event("ev_cluster")
		// head 100 W × 36 s = 1 Wh; member 200 W × PUE 1.5 × 36 s = 3 Wh.
		requireCloseWh(t, "energy_wh", ev.EnergyWh, 4.0)
		if ev.EnergySource != "measured" {
			t.Fatalf("energy_source = %q, want measured", ev.EnergySource)
		}
		parts := f.parts("ev_cluster")
		if len(parts) != 2 {
			t.Fatalf("parts = %+v, want a head and a member part", parts)
		}
		head, mem := parts["srv-head"], parts["srv-mem"]
		if head.Role != usage.EventHostRoleHead || head.Source != "measured" {
			t.Fatalf("head part = %+v", head)
		}
		requireCloseWh(t, "head part", head.Wh, 1.0)
		if mem.Role != usage.EventHostRoleMember || mem.Source != "measured" {
			t.Fatalf("member part = %+v", mem)
		}
		requireCloseWh(t, "member part", mem.Wh, 3.0)
	})

	t.Run("a member with nothing to price it", func(t *testing.T) {
		f := newEnergyFixture(t, 0, 0)
		f.createServer("srv-head", routing.AIServer{})
		f.createServer("srv-mem", routing.AIServer{})
		f.createServer("srv-dark", routing.AIServer{}) // no telemetry, no EstimatedWatts
		f.constPower("srv-head", clusterEnergyBase, 36, 100)
		f.constPower("srv-mem", clusterEnergyBase, 36, 200)
		f.recordClusterEvent(usage.Event{ID: "ev_cluster", CreatedAt: end, LatencyMS: 36000, OutputTokens: 100}, "srv-head", "srv-mem", "srv-dark")

		f.reconcile("ev_cluster")

		ev := f.event("ev_cluster")
		requireCloseWh(t, "energy_wh", ev.EnergyWh, 3.0)
		if ev.EnergySource != energySourceMemberUnpriced {
			t.Fatalf("energy_source = %q, want %q", ev.EnergySource, energySourceMemberUnpriced)
		}
		dark := f.parts("ev_cluster")["srv-dark"]
		if dark.Source != energySourceMemberUnpriced || dark.Wh != 0 || dark.Role != usage.EventHostRoleMember {
			t.Fatalf("unpriced member part = %+v, want 0 Wh flagged %s", dark, energySourceMemberUnpriced)
		}
	})

	t.Run("a head priced per token prices the whole request", func(t *testing.T) {
		f := newEnergyFixture(t, 0, 0.01)
		f.createServer("srv-head", routing.AIServer{}) // Tier 3 only
		f.createServer("srv-mem", routing.AIServer{})
		f.constPower("srv-mem", clusterEnergyBase, 36, 200)
		f.recordClusterEvent(usage.Event{ID: "ev_cluster", CreatedAt: end, LatencyMS: 36000, OutputTokens: 100}, "srv-head", "srv-mem")

		f.reconcile("ev_cluster")

		ev := f.event("ev_cluster")
		if ev.EnergySource != "modeled" {
			t.Fatalf("energy_source = %q, want modeled", ev.EnergySource)
		}
		requireCloseWh(t, "energy_wh", ev.EnergyWh, 1.0)
		if parts := f.parts("ev_cluster"); len(parts) != 0 {
			t.Fatalf("parts = %+v, want none: a per-token figure already covers every host", parts)
		}
	})

	t.Run("a single-host request keeps no parts", func(t *testing.T) {
		f := newEnergyFixture(t, 0, 0)
		f.createServer("srv-head", routing.AIServer{})
		f.constPower("srv-head", clusterEnergyBase, 36, 100)
		if err := f.usage.Record(usage.Event{ID: "ev_plain", Host: "srv-head", CreatedAt: end, LatencyMS: 36000}); err != nil {
			t.Fatal(err)
		}

		f.reconcile("ev_plain")

		requireCloseWh(t, "energy_wh", f.event("ev_plain").EnergyWh, 1.0)
		if parts := f.parts("ev_plain"); len(parts) != 0 {
			t.Fatalf("parts = %+v, want none", parts)
		}
	})
}

// TestReconcileMemberPartGapFallsToTier2 pins that a member's telemetry gap
// (or a skewed clock that moves its samples out of the window) drops only that
// member's part to Tier 2, PUE applied, and the event to the weakest tier.
func TestReconcileMemberPartGapFallsToTier2(t *testing.T) {
	end := clusterEnergyBase.Add(36 * time.Second)
	for _, tc := range []struct {
		name    string
		samples func(f *energyFixture)
	}{
		{"gap over two seconds", func(f *energyFixture) {
			f.constPower("srv-mem", clusterEnergyBase, 15, 400)
			f.constPower("srv-mem", clusterEnergyBase.Add(19*time.Second), 17, 400)
		}},
		{"samples ahead of the window", func(f *energyFixture) {
			f.constPower("srv-mem", clusterEnergyBase.Add(40*time.Second), 36, 400)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newEnergyFixture(t, 0, 0)
			f.createServer("srv-head", routing.AIServer{})
			f.createServer("srv-mem", routing.AIServer{EstimatedWatts: 50, Pue: 2})
			f.constPower("srv-head", clusterEnergyBase, 36, 100)
			tc.samples(f)
			f.recordClusterEvent(usage.Event{ID: "ev_cluster", CreatedAt: end, LatencyMS: 36000}, "srv-head", "srv-mem")

			f.reconcile("ev_cluster")

			mem := f.parts("ev_cluster")["srv-mem"]
			if mem.Source != "estimated" {
				t.Fatalf("member part = %+v, want estimated", mem)
			}
			// 50 W × PUE 2 × 36 s = 1 Wh.
			requireCloseWh(t, "member part", mem.Wh, 1.0)
			ev := f.event("ev_cluster")
			if ev.EnergySource != "estimated" {
				t.Fatalf("energy_source = %q, want the weakest part's tier, estimated", ev.EnergySource)
			}
			requireCloseWh(t, "energy_wh", ev.EnergyWh, 2.0)
		})
	}
}

// TestReconcileClusterEnergyOrderIndependent pins that a member host's power is
// split between its own requests and the cluster requests that used it, the
// same whichever of them is priced first: both sides read the cluster event's
// member list, which is fixed when the request completes.
func TestReconcileClusterEnergyOrderIndependent(t *testing.T) {
	end := clusterEnergyBase.Add(36 * time.Second)
	run := func(order []string) (map[string]usage.Event, map[string]usage.EnergyPart) {
		f := newEnergyFixture(t, 0, 0)
		f.createServer("srv-head", routing.AIServer{})
		f.createServer("srv-mem", routing.AIServer{})
		f.constPower("srv-head", clusterEnergyBase, 36, 100)
		f.constPower("srv-mem", clusterEnergyBase, 36, 300)
		f.recordClusterEvent(usage.Event{ID: "ev_cluster", CreatedAt: end, LatencyMS: 36000}, "srv-head", "srv-mem")
		// A request of the member host's own, running through the second half
		// of the cluster request.
		if err := f.usage.Record(usage.Event{ID: "ev_local", Host: "srv-mem", CreatedAt: end, LatencyMS: 18000}); err != nil {
			t.Fatal(err)
		}
		for _, id := range order {
			f.reconcile(id)
		}
		return map[string]usage.Event{"ev_cluster": f.event("ev_cluster"), "ev_local": f.event("ev_local")}, f.parts("ev_cluster")
	}

	memberFirstEvents, memberFirstParts := run([]string{"ev_local", "ev_cluster"})
	clusterFirstEvents, clusterFirstParts := run([]string{"ev_cluster", "ev_local"})

	for id, a := range memberFirstEvents {
		b := clusterFirstEvents[id]
		if !almostEqual(a.EnergyWh, b.EnergyWh, whTol) || !almostEqual(a.EnergyMarginalWh, b.EnergyMarginalWh, whTol) || a.EnergySource != b.EnergySource {
			t.Fatalf("%s: member side first = %+v, cluster first = %+v", id, a, b)
		}
	}
	for id, a := range memberFirstParts {
		if b := clusterFirstParts[id]; !almostEqual(a.Wh, b.Wh, whTol) || a.Source != b.Source {
			t.Fatalf("part %s: member side first = %+v, cluster first = %+v", id, a, b)
		}
	}
	// The member host's 300 W is split two ways over the shared 18 s and
	// taken by the cluster request alone over the first 18 s.
	requireCloseWh(t, "member part", memberFirstParts["srv-mem"].Wh, (300.0*18+150.0*18)/3600)
	requireCloseWh(t, "local request", memberFirstEvents["ev_local"].EnergyWh, 150.0*18/3600)
	requireCloseWh(t, "cluster request", memberFirstEvents["ev_cluster"].EnergyWh, 1.0+(300.0*18+150.0*18)/3600)
}

// TestCalibrationSkipsPartiallyMeasured pins that a cluster request calibrates
// its mapping's Wh-per-token coefficient only when every part was measured,
// and then from the sum of the parts' marginal energy.
func TestCalibrationSkipsPartiallyMeasured(t *testing.T) {
	end := clusterEnergyBase.Add(36 * time.Second)
	setup := func(memberWatts float64, memberTelemetry bool) *energyFixture {
		f := newEnergyFixture(t, 0, 0)
		f.createServer("srv-head", routing.AIServer{})
		f.createServer("srv-mem", routing.AIServer{EstimatedWatts: memberWatts})
		f.constPower("srv-head", clusterEnergyBase, 36, 100)
		if memberTelemetry {
			f.constPower("srv-mem", clusterEnergyBase, 36, 200)
		}
		f.createMapping("map1", "app1", "srv-head", 8080, routing.ModelMapping{GatewayModelName: "gw", AppModelName: "upstream"})
		f.recordClusterEvent(usage.Event{ID: "ev_cluster", RouteID: "map1", CreatedAt: end, LatencyMS: 36000, OutputTokens: 1000}, "srv-head", "srv-mem")
		f.reconcile("ev_cluster")
		return f
	}

	t.Run("a member estimated", func(t *testing.T) {
		f := setup(50, false)
		if ev := f.event("ev_cluster"); ev.EnergySource != "estimated" {
			t.Fatalf("energy_source = %q, want estimated", ev.EnergySource)
		}
		if m := f.mapping("map1"); m.EnergyWhPerToken != 0 || m.MetricsSource == "energy" {
			t.Fatalf("mapping = %+v, want no calibration from a partially measured request", m)
		}
	})

	t.Run("a member unpriced", func(t *testing.T) {
		f := setup(0, false)
		if m := f.mapping("map1"); m.EnergyWhPerToken != 0 || m.MetricsSource == "energy" {
			t.Fatalf("mapping = %+v, want no calibration with a member unpriced", m)
		}
	})

	t.Run("every part measured", func(t *testing.T) {
		f := setup(0, true)
		m := f.mapping("map1")
		if m.MetricsSource != "energy" {
			t.Fatalf("MetricsSource = %q, want energy (calibration must fire)", m.MetricsSource)
		}
		// (1 Wh head + 2 Wh member) / 1000 tokens; idle 0, so marginal == total.
		requireCloseWh(t, "EnergyWhPerToken", m.EnergyWhPerToken, 0.003)
	})
}
