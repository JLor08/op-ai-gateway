// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package routing

import (
	"testing"
	"time"
)

// TestScorerStillAwardsTheMTPBonusFromTheJoinedRow is the load-bearing
// regression pin for moving IsMTP off ModelMapping onto the joined
// MappingCandidate field: it must not change what the scorer actually does.
// scoringRoute reads c.IsMTP (the join) -- this test proves the bonus still
// arrives by scoring two candidates through the REAL scorer (scoringRoute +
// Score, exactly what argmaxByScore and ScoreModelServers call), identical
// in every field except MappingCandidate.IsMTP, and asserting the score
// difference is EXACTLY mtpBonus (30 points). Asserted through the scorer
// rather than by inspecting MappingCandidate/Route fields directly, so it
// pins the actual routing BEHAVIOUR the request path depends on, not the
// plumbing that feeds it -- a refactor that quietly stopped wiring the bonus
// through would fail this test even if every field still compiled.
//
// The candidates' Mapping carries no MTP value at all, and cannot: migration
// 79 dropped the column and ModelMapping has no such field, so "the bonus
// could only have come from the joined field" is now guaranteed by the type
// rather than by a setup assertion at the end of this test (which is where
// it used to live).
func TestScorerStillAwardsTheMTPBonusFromTheJoinedRow(t *testing.T) {
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	newCandidate := func(id string, isMTP bool) MappingCandidate {
		return MappingCandidate{
			Server:      AIServer{ID: "srv", Status: ServerStatusActive, HealthStatus: HealthHealthy},
			Application: Application{ID: "app", Priority: 10, Weight: 50},
			Mapping:     ModelMapping{ID: id, GatewayModelName: "m"},
			IsMTP:       isMTP, // from the join: the Mapping has no MTP field to confuse it with
		}
	}
	withoutMTP := newCandidate("map_plain", false)
	withMTP := newCandidate("map_mtp", true)

	tel := ServerTelemetry{ReportedAt: now}
	routeWithout := scoringRoute(withoutMTP, tel, true, 0, "m")
	routeWith := scoringRoute(withMTP, tel, true, 0, "m")

	scoreWithout, ok := Score(routeWithout, "m", now)
	if !ok {
		t.Fatalf("Score(without mtp) reported not viable, want viable")
	}
	scoreWith, ok := Score(routeWith, "m", now)
	if !ok {
		t.Fatalf("Score(with mtp) reported not viable, want viable")
	}
	if diff := scoreWith - scoreWithout; diff != mtpBonus {
		t.Fatalf("mtp score diff = %v, want exactly mtpBonus (%v); with=%v without=%v", diff, mtpBonus, scoreWith, scoreWithout)
	}
}
