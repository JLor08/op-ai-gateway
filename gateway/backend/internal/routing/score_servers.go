// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package routing

import (
	"context"
	"fmt"
	"time"
)

// CandidateScore is one server's live routing score for a model — enough to build a
// "would be picked in this order" ranking.
type CandidateScore struct {
	MappingID string
	ServerID  string
	Score     float64
	Available bool // viable (Score gate) AND under capacity (in-flight < max_concurrency)
}

// ScoreModelServers scores every server that offers `model` (across API flavors),
// reusing the same Route/Score path as routing (shares scoringRoute with argmaxByScore).
// Available = the candidate passes the Score viability gate AND (MaxConcurrency == 0 OR
// in-flight k < MaxConcurrency). Per-session swap-protection / reservation / affinity is
// intentionally NOT applied: this is the GENERAL live order shown in the UI, not a
// specific request's pinned choice. Read-only — mutates no resolver state.
func (r *Resolver) ScoreModelServers(ctx context.Context, model string, now time.Time) ([]CandidateScore, error) {
	seen := make(map[string]struct{})
	out := make([]CandidateScore, 0)
	for _, flavor := range []string{APIFlavorOpenAI, APIFlavorAnthropic} {
		pool, err := r.store.ActiveMappingsForModel(ctx, model, flavor)
		if err != nil {
			return nil, err
		}
		for _, candidate := range pool {
			if _, dup := seen[candidate.Mapping.ID]; dup {
				continue
			}
			seen[candidate.Mapping.ID] = struct{}{}
			telemetry, ok, err := r.store.TelemetryByServer(ctx, candidate.Server.ID)
			if err != nil {
				return nil, fmt.Errorf("load server telemetry: %w", err)
			}
			k := telemetry.ActiveRequests
			if r.activity != nil {
				inflight, _ := r.activity.ServerActivity(candidate.Server.ID)
				k = inflight
			}
			route := scoringRoute(candidate, telemetry, ok, k, model)
			score, viable := Score(route, model, now)
			available := viable
			if maxC := candidate.Mapping.MaxConcurrency; available && maxC > 0 && k >= maxC {
				available = false
			}
			out = append(out, CandidateScore{
				MappingID: candidate.Mapping.ID,
				ServerID:  candidate.Server.ID,
				Score:     score,
				Available: available,
			})
		}
	}
	return out, nil
}
