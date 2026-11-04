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

// ScoreModelServers scores every server that offers `model` (across every coarse API
// flavor, openai_images and openai_audio included, so an images-only or audio-only
// model is ranked too; a mapping offered under several flavors is scored once),
// reusing the same Route/Score path as routing (shares scoringRoute with
// argmaxByScore).
// A mapping offered under openai_audio is scored with the audio term (Route.Audio)
// whichever flavor lists it first: that flavor is how every speech and
// transcription request reaches it, and live routing scores those with the term,
// so a mapping also offered as text would otherwise rank, here and in the instance
// scaler that reads this ranking, below where the router puts it for audio.
// Available = the candidate passes the Score viability gate AND (MaxConcurrency == 0 OR
// in-flight k < MaxConcurrency). Per-session swap-protection / reservation / affinity is
// intentionally NOT applied: this is the GENERAL live order shown in the UI, not a
// specific request's pinned choice. Read-only — mutates no resolver state.
func (r *Resolver) ScoreModelServers(ctx context.Context, model string, now time.Time) ([]CandidateScore, error) {
	flavors := []string{APIFlavorOpenAI, APIFlavorAnthropic, APIFlavorOpenAIImages, APIFlavorOpenAIAudio}
	pools := make([][]MappingCandidate, len(flavors))
	audio := make(map[string]bool)
	for i, flavor := range flavors {
		pool, err := r.store.ActiveMappingsForModel(ctx, model, flavor)
		if err != nil {
			return nil, err
		}
		pools[i] = pool
		if flavor == APIFlavorOpenAIAudio {
			for _, candidate := range pool {
				audio[candidate.Mapping.ID] = true
			}
		}
	}
	seen := make(map[string]struct{})
	out := make([]CandidateScore, 0)
	for _, pool := range pools {
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
			telemetry, ok = r.mergeRuntimeModelMetrics(candidate, telemetry, ok, now)
			route := scoringRoute(candidate, telemetry, ok, k, model, audio[candidate.Mapping.ID])
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
