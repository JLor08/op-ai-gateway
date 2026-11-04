// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package routing

import (
	"errors"
	"op-ai-gateway/internal/inference"
	"slices"
)

// ErrNoRealtimeCandidate reports that a request that must keep up with real
// time (inference.Request.RealtimeRequired: a voice chat's speech or
// transcription leg) had candidates, and every one of them was measured slower
// than the real-time threshold (SetAudioRealtimeMinFactor). It is not
// ErrNoHealthyHost: the hosts are up, the model is too slow for a conversation,
// and a retry does not cure that -- only another model, or a faster host, does.
var ErrNoRealtimeCandidate = errors.New("routing.no_realtime_candidate")

// SetAudioRealtimeMinFactor installs the real-time threshold the voice legs are
// gated by: a candidate whose mapping was measured at an audio rate below it is
// no candidate for a request that must keep up with real time. It is the factor
// the audio benchmark's concurrency ramp judges a level by
// (OP_AI_GATEWAY_AUDIO_REALTIME_MIN_FACTOR), so the ramp and the gate agree on
// what real time means. Leaving it unset (0) gates nothing.
func (r *Resolver) SetAudioRealtimeMinFactor(f float64) { r.realtimeMinFactor = f }

// audioRequest reports whether req is a speech or a transcription request, the
// two whose candidates are ranked by their measured audio rate (audioTiebreak).
func audioRequest(req inference.Request) bool {
	return slices.Contains(req.RequiredCapabilities, CapabilitySpeech) ||
		slices.Contains(req.RequiredCapabilities, CapabilityTranscription)
}

// tooSlowForRealtime reports whether c's mapping was measured slower than the
// real-time threshold. An unmeasured mapping (rate 0) is not: a new host must be
// usable before its first measurement, or a fleet that was never benchmarked
// could not hold a voice chat at all.
func (r *Resolver) tooSlowForRealtime(c MappingCandidate) bool {
	rate := c.Mapping.AudioRateX
	return rate > 0 && rate < r.realtimeMinFactor
}

// filterRealtime drops, for a request that must keep up with real time, the
// candidates measured too slow for it (tooSlowForRealtime). It is a hard gate,
// not a preference: a voice chat on a model that falls behind the conversation
// is worse than no voice chat, so it never fails open -- a pool it empties is
// ErrNoRealtimeCandidate. Every other request, and an empty pool, is returned as
// it is.
func (r *Resolver) filterRealtime(cands []MappingCandidate, req inference.Request) ([]MappingCandidate, error) {
	if !req.RealtimeRequired || len(cands) == 0 {
		return cands, nil
	}
	kept := slices.DeleteFunc(slices.Clone(cands), r.tooSlowForRealtime)
	if len(kept) == 0 {
		return nil, ErrNoRealtimeCandidate
	}
	return kept, nil
}
