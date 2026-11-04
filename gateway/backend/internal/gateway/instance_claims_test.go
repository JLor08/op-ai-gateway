// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"op-ai-gateway/internal/routing"
	"testing"
)

func TestInstanceClaimsClaimLowest(t *testing.T) {
	c := newInstanceClaims()
	key := scaleKey{serverID: "srv", mappingID: "map"}
	other := scaleKey{serverID: "srv", mappingID: "map2"}
	limit := routing.MaxInstanceIndex + 1

	if k, ok := c.claimLowest(key, 0, limit, map[int]bool{0: true, 2: true}); !ok || k != 1 {
		t.Fatalf("first claim = (%d, %v), want (1, true): 0 and 2 are taken", k, ok)
	}
	if k, ok := c.claimLowest(key, 0, limit, map[int]bool{0: true, 2: true}); !ok || k != 3 {
		t.Fatalf("second claim = (%d, %v), want (3, true): 1 is held by the first", k, ok)
	}
	if k, ok := c.claimLowest(other, 0, limit, nil); !ok || k != 0 {
		t.Fatalf("another mapping's claim = (%d, %v), want (0, true): holds are per mapping", k, ok)
	}
	c.release(key, 1)
	if k, ok := c.claimLowest(key, 1, limit, nil); !ok || k != 1 {
		t.Fatalf("claim after release = (%d, %v), want (1, true)", k, ok)
	}
	if _, ok := c.claimLowest(key, 0, 2, map[int]bool{0: true}); ok {
		t.Fatal("claim below limit 2 succeeded, want none: 0 is taken and 1 is held")
	}
	if got := c.heldBy(key); !got[1] || !got[3] || len(got) != 2 {
		t.Fatalf("heldBy = %v, want {1, 3}", got)
	}
}

func TestInstanceClaimsTryClaim(t *testing.T) {
	c := newInstanceClaims()
	key := scaleKey{serverID: "srv", mappingID: "map"}
	if !c.tryClaim(key, 2) {
		t.Fatal("first tryClaim(2) = false, want true")
	}
	if c.tryClaim(key, 2) {
		t.Fatal("second tryClaim(2) = true, want false while it is held")
	}
	c.release(key, 2)
	if !c.tryClaim(key, 2) {
		t.Fatal("tryClaim(2) after release = false, want true")
	}
	c.release(key, 2)
	if got := c.heldBy(key); len(got) != 0 {
		t.Fatalf("heldBy after the last release = %v, want empty", got)
	}
}

// A nil claim set holds nothing: a bare Server built without New still picks
// an index by the rest of the rule.
func TestInstanceClaimsNilHoldsNothing(t *testing.T) {
	var c *instanceClaims
	key := scaleKey{serverID: "srv", mappingID: "map"}
	if k, ok := c.claimLowest(key, 0, 4, map[int]bool{0: true}); !ok || k != 1 {
		t.Fatalf("claimLowest on nil = (%d, %v), want (1, true)", k, ok)
	}
	if !c.tryClaim(key, 1) || len(c.heldBy(key)) != 0 {
		t.Fatal("a nil claim set must claim every index and hold none")
	}
	c.release(key, 1)
}

func TestOtherMappingIndexes(t *testing.T) {
	m := routing.ModelMapping{ID: "map", AppModelName: "m"}
	got := otherMappingIndexes([]routing.ModelMapping{
		m,
		{ID: "lit1", AppModelName: "m#1"},
		{ID: "lit2", AppModelName: "m#02"}, // not an address of m
		{ID: "lit3", AppModelName: "x#3"},  // another base
		{ID: "lit5", AppModelName: "m#5"},
	}, m)
	if !got[1] || !got[5] || len(got) != 2 {
		t.Fatalf("otherMappingIndexes = %v, want {1, 5}", got)
	}
}

// A manual start holds an index in the claim set the scaler shares: a
// scale-up that runs meanwhile never asks for the same instance.
func TestScalerSkipsAnIndexAManualStartHolds(t *testing.T) {
	f := newScalerFixture(t)
	f.addHost("srv_a", 50)
	key := scaleKey{serverID: "srv_a", mappingID: "map_srv_a"}
	if !f.scaler.claims.tryClaim(key, 1) {
		t.Fatal("manual claim of index 1 failed")
	}
	f.signal(f.demand("map_srv_a"))
	f.wantEnsured("srv_a m#2")
}

// The scaler holds the index it ensures until routing sees the instance, so a
// manual start that runs meanwhile picks the next one.
func TestManualClaimSkipsAnIndexTheScalerEnsures(t *testing.T) {
	f := newScalerFixture(t)
	f.addHost("srv_a", 50)
	gate := f.ensurer.hold()
	f.scaler.Signal(f.demand("map_srv_a"))
	f.awaitEnsure()
	key := scaleKey{serverID: "srv_a", mappingID: "map_srv_a"}
	k, ok := f.scaler.claims.claimLowest(key, 1, routing.MaxInstanceIndex+1, nil)
	if !ok || k != 2 {
		t.Fatalf("manual claim while the scaler ensures m#1 = (%d, %v), want (2, true)", k, ok)
	}
	f.scaler.claims.release(key, k)
	close(gate)
	f.scaler.wait()
	if got := f.scaler.claims.heldBy(key); len(got) != 0 {
		t.Fatalf("claims after the scale-up = %v, want none left", got)
	}
}

// An index a manual start claims between the scaler's pick and its ask is not
// asked for: the scaler picks again and asks for the next free one, without
// counting the host as asked.
func TestScalerRepicksWhenAManualStartTookItsIndex(t *testing.T) {
	f := newScalerFixture(t)
	f.addHost("srv_a", 50)
	key := scaleKey{serverID: "srv_a", mappingID: "map_srv_a"}
	took := false
	f.scaler.beforeClaim = func(c scaleCandidate) {
		if !took {
			took = true
			if !f.scaler.claims.tryClaim(key, c.index) {
				t.Errorf("manual claim of %d failed", c.index)
			}
		}
	}
	f.signal(f.demand("map_srv_a"))
	f.wantEnsured("srv_a m#2")
	if got := f.scaler.claims.heldBy(key); !got[1] || len(got) != 1 {
		t.Fatalf("claims after the scale-up = %v, want the manual start's {1} only", got)
	}
}

// New hands the scaler the server's own claim set, the one the manual start
// claims in.
func TestNewSharesTheInstanceClaimsWithTheScaler(t *testing.T) {
	srv := NewTestServer()
	if srv.instanceScaler == nil || srv.instanceClaims == nil {
		t.Fatal("New built no scaler or no claim set")
	}
	if srv.instanceScaler.claims != srv.instanceClaims {
		t.Fatal("the scaler claims in a set of its own, want the server's")
	}
}
