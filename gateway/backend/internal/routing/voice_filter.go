// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package routing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
)

// VoiceNamesFromCacheRows decodes the voice names a mapping's voices cache
// (ModelMappingCacheVoices) lists. Only the scope-"" row is read: it is the one the
// voices writers keep. An empty or blank payload and a JSON null both read as "no
// names" (nil, nil); a payload that is not a JSON array of strings is an error, so each
// caller decides what an unreadable row costs (the voices endpoint skips that mapping
// and warns, the router treats it as "does not list anything").
//
// It is the ONE definition of how the cache is read, shared by the voices endpoint
// (union for the picker) and the voice filter (does this mapping list the voice the
// request named), so the two cannot disagree about what a row says.
func VoiceNamesFromCacheRows(rows []ModelMappingCacheRow) ([]string, error) {
	for _, row := range rows {
		if row.ScopeKey != "" || strings.TrimSpace(row.Payload) == "" {
			continue
		}
		var names []string
		if err := json.Unmarshal([]byte(row.Payload), &names); err != nil {
			return nil, err
		}
		return names, nil
	}
	return nil, nil
}

// voiceScope is the outcome of ONE request's voice pre-pass (voiceScopeFor): the set of
// candidate mappings the requested voice is restricted to. A nil *voiceScope is "no
// restriction" and is what every method treats as the identity, so a request that
// named no voice, or a voice nothing is known to offer, threads a nil through the
// resolver and changes nothing.
//
// A non-nil scope exists only when the pre-pass found at least one candidate that lists
// the voice (FAIL-OPEN, see voiceScopeFor): allowed is then never empty.
type voiceScope struct {
	// allowed is the ids of the pool's mappings whose cached voice list names the
	// requested voice.
	allowed map[string]struct{}
	// narrowed reports that the voice EXCLUDED at least one candidate of the pool it
	// was computed over, which is what makes a later "no usable host" the voice's doing
	// (refine). A voice that every candidate lists restricts nothing.
	narrowed bool
}

// restrict keeps the candidates the voice allows. Nil-safe: a nil scope returns cands
// unchanged (the same slice, not a copy).
func (v *voiceScope) restrict(cands []MappingCandidate) []MappingCandidate {
	if v == nil {
		return cands
	}
	out := cands[:0:0]
	for _, c := range cands {
		if _, ok := v.allowed[c.Mapping.ID]; ok {
			out = append(out, c)
		}
	}
	return out
}

// refine turns the generic "no healthy host" outcome into ErrVoiceUnavailable when the
// voice narrowed the candidates: the candidates left are the only ones that can say the
// voice, and none of them is usable. Every other error, and the same error for a scope
// that narrowed nothing, passes through untouched. Nil-safe.
func (v *voiceScope) refine(err error) error {
	if v != nil && v.narrowed && errors.Is(err, ErrNoHealthyHost) {
		return ErrVoiceUnavailable
	}
	return err
}

// voiceScopeFor is the voice pre-pass: given the pool of candidates a request could be
// served by (after provisioning, endpoint and capability filtering -- see memberPool, so
// "could be served by" means exactly what dispatch means), it decides whether the voice
// restricts that pool and to what.
//
// FAIL-OPEN, the whole point: the restriction applies only when at least one candidate
// in the pool lists the voice. Otherwise -- no voice named, an empty pool, a voice no
// cached list names (a free-text or unknown voice, a cache never filled, a cached-empty
// list, an undecodable row) -- it returns nil and the request routes exactly as it
// would without a voice. An existing bearer client that sends a voice the gateway has
// never heard of therefore behaves as it always did; the upstream stays the authority
// on whether it accepts the name.
//
// A candidate lists the voice when the voice is in its mapping's cached list (exact
// match: the name is relayed to the upstream byte for byte, so a near miss is a
// different voice) AND the mapping is a plain tts model NOW (CapabilityRowsPlainTTS, the
// verdict gate the voices endpoint applies at read time, so a list that outlived a
// tts -> clone flip is not a known voice here either).
//
// The pool is one flat set. For a group the caller passes every member's candidates,
// which is what lets one answer both "does anything in the group list the voice" (the
// fail-open switch) and "which mappings may serve it" before any member is iterated: a
// per-member decision cannot narrow a group, because a member without the voice would
// still be eligible when the walk reached it.
//
// The two reads fail differently, on purpose. A failure of the VOICES CACHE read fails
// open: the cache is the one input that exists only for this preference, so when it
// cannot be read there is simply no voice information, which is exactly the "nothing
// lists the voice" case (nil, routed as without a voice; logged at Warn so the outage
// is not silent). A failure of the capability VERDICT read is returned: it reads the
// very rows filterCapable has just read and refuses on, so the posture stays that of
// the capability gate it belongs to. Exactly two bulk reads, and none at all for a
// request that names no voice.
func (r *Resolver) voiceScopeFor(ctx context.Context, voice string, pool []MappingCandidate) (*voiceScope, error) {
	if voice == "" || len(pool) == 0 {
		return nil, nil
	}
	ids := make([]string, 0, len(pool))
	seen := make(map[string]struct{}, len(pool))
	for _, c := range pool {
		if _, dup := seen[c.Mapping.ID]; !dup {
			seen[c.Mapping.ID] = struct{}{}
			ids = append(ids, c.Mapping.ID)
		}
	}
	verdicts, err := r.store.MappingCapabilitiesForMappings(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("voice filter: read capability verdicts: %w", err)
	}
	tts := make([]string, 0, len(ids))
	for _, id := range ids {
		if CapabilityRowsPlainTTS(verdicts[id]) {
			tts = append(tts, id)
		}
	}
	if len(tts) == 0 {
		return nil, nil
	}
	cached, err := r.store.ModelMappingCacheForMappings(ctx, tts, ModelMappingCacheVoices)
	if err != nil {
		// FAIL OPEN, see above: no cache, no voice information, no restriction.
		slog.Warn("voice filter: voices cache could not be read, routing unfiltered", "voice", voice, "candidates", len(ids), "err", err)
		return nil, nil
	}
	allowed := make(map[string]struct{}, len(tts))
	for _, id := range tts {
		names, err := VoiceNamesFromCacheRows(cached[id])
		if err != nil {
			// Unreadable is "does not list it": a corrupt row must not break routing, and
			// the writer cannot produce one.
			slog.Warn("voice filter: voices cache row is undecodable and was skipped", "mapping_id", id, "err", err)
			continue
		}
		if slices.Contains(names, voice) {
			allowed[id] = struct{}{}
		}
	}
	if len(allowed) == 0 {
		slog.Debug("voice filter: no candidate lists the voice, routing unfiltered", "voice", voice, "candidates", len(ids))
		return nil, nil
	}
	slog.Debug("voice filter: restricting candidates to those that list the voice", "voice", voice, "allowed", len(allowed), "candidates", len(ids))
	return &voiceScope{allowed: allowed, narrowed: len(allowed) < len(ids)}, nil
}
