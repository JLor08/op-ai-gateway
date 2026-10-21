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

// hasVoicesRow reports whether a mapping has the scope-"" voices row the writers keep --
// the signal that its voice status is KNOWN (the voices were fetched at least once), as
// opposed to never cached. Row PRESENCE is read directly here, because
// VoiceNamesFromCacheRows cannot tell "no row at all" (status UNKNOWN -> fail open) from a
// present row whose payload is blank or a JSON null (fetched, lists nothing -> KNOWN-absent):
// both of those decode to (nil, nil). A present scope-"" row, whatever it decodes to, means
// the voices were fetched, so the status is known and the filter may act on it.
func hasVoicesRow(rows []ModelMappingCacheRow) bool {
	for _, row := range rows {
		if row.ScopeKey == "" {
			return true
		}
	}
	return false
}

// voiceScope is the outcome of ONE request's voice pre-pass (voiceScopeFor): the set of
// candidate mappings the requested voice is restricted to. A nil *voiceScope is "no
// restriction" and is what every method treats as the identity, so a request that named
// no voice, one no candidate is KNOWN to lack, or one NO candidate is known to offer (every
// candidate known-absent -- fail open), threads a nil through the resolver and changes nothing.
//
// A non-nil scope exists only when the voice EXCLUDED at least one candidate KNOWN (from
// its cached voices row) to not list it AND at least one eligible candidate remained
// (FAIL-OPEN on unknown, see voiceScopeFor): allowed is then never empty.
type voiceScope struct {
	// allowed is the ids of the pool's mappings eligible to serve the voice: those whose
	// cached voice list names it, PLUS those whose voice status is UNKNOWN (no cached row,
	// or an unreadable one) and so stay eligible fail-open (unknown != absent). It is never
	// empty: when every candidate is known-absent the pre-pass returns a nil scope instead.
	allowed map[string]struct{}
	// narrowed reports that the voice EXCLUDED at least one candidate KNOWN (from its cached
	// voices row) to not list it, which is what makes a later "no usable host" the voice's
	// doing (refine). A candidate of unknown/uncached voice status never narrows; a voice
	// every candidate lists or has unknown status restricts nothing.
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
// FAIL-OPEN on UNKNOWN, the whole point: the voice restricts the pool only AWAY FROM
// candidates KNOWN (from their cached voices row) to not list it. A candidate whose voice
// status is unknown -- no voices row cached yet (a server never observed with its model
// loaded, an older static audio.cpp with no voices endpoint, an agent too old to report
// them), or a row that cannot be decoded -- stays eligible: unknown is NOT absent, so a
// healthy same-model sibling keeps serving as a failover/load target and the upstream
// stays the authority on a voice it was never told about. Only a KNOWN-ABSENT candidate
// narrows anything, and only while an eligible candidate remains; otherwise -- no voice
// named, an empty pool, every candidate known-present or of unknown status, OR every
// candidate known-absent (nothing is known to offer the voice, so it routes unfiltered and
// the upstream decides) -- it returns nil and the request routes exactly as it would
// without a voice. voice_unavailable therefore arises only when the voice narrowed the pool
// (some known-absent) with an eligible candidate left, and none of those eligible hosts is
// usable.
//
// A candidate is known-present when the voice is in its mapping's cached list (exact
// match: the name is relayed to the upstream byte for byte, so a near miss is a different
// voice) AND the mapping is a plain tts model NOW (CapabilityRowsPlainTTS, the verdict gate
// the voices endpoint applies at read time, so a list that outlived a tts -> clone flip is
// not a known voice here either). A candidate whose cached list does NOT name the voice is
// known-absent and excluded; one with no cached voices row at all is unknown and kept.
//
// The pool is one flat set. For a group the caller passes every member's candidates,
// which is what lets one answer both "does anything in the group list the voice" (the
// fail-open switch) and "which mappings may serve it" before any member is iterated: a
// per-member decision cannot narrow a group, because a member without the voice would
// still be eligible when the walk reached it.
//
// The two reads fail differently, on purpose. A failure of the VOICES CACHE read fails
// open: the cache is the one input that exists only for this preference, so when it
// cannot be read there is simply no voice information, which is exactly the "every
// candidate is of unknown status" case (nil, routed as without a voice; logged at Warn so
// the outage is not silent). A failure of the capability VERDICT read is returned: it reads the
// very rows filterCapable has just read and refuses on, so the posture stays that of
// the capability gate it belongs to. Exactly two bulk reads, and none at all for a
// request that names no voice.
func (r *Resolver) voiceScopeFor(ctx context.Context, voice string, pool []MappingCandidate) (*voiceScope, error) {
	if voice == "" || len(pool) == 0 {
		return nil, nil
	}
	ids := candidateMappingIDs(pool)
	verdicts, err := r.store.MappingCapabilitiesForMappings(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("voice filter: read capability verdicts: %w", err)
	}
	tts := plainTTSMappingIDs(ids, verdicts)
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
	knownAbsent := 0
	for _, id := range tts {
		if voiceListsEligible(cached[id], voice, id) {
			allowed[id] = struct{}{}
			continue
		}
		// KNOWN-ABSENT (the only ineligible outcome): the row exists and decodes but does not
		// list the voice (a cached-empty or null list included). This is the ONLY thing that
		// narrows the pool.
		knownAbsent++
	}
	if len(allowed) == 0 {
		// Every candidate is KNOWN-absent: no cached list offers the voice. FAIL OPEN --
		// route unfiltered and let the upstream be the authority (it may accept a free-text
		// name; if not it rejects it with a 500, the accepted free-text risk), rather than
		// 503 a request the upstream might still serve.
		slog.Debug("voice filter: no candidate lists the voice, routing unfiltered", "voice", voice, "candidates", len(ids))
		return nil, nil
	}
	if knownAbsent == 0 {
		// Nothing is KNOWN to lack the voice (every candidate lists it or is of unknown
		// status), so the voice restricts nothing: route exactly as without a voice.
		slog.Debug("voice filter: no candidate is known to lack the voice, routing unfiltered", "voice", voice, "candidates", len(ids))
		return nil, nil
	}
	// At least one candidate is KNOWN-ABSENT and at least one eligible candidate remains
	// (known-present or of unknown status), so the voice narrows the pool to the eligible
	// set. allowed is never empty here; ErrVoiceUnavailable arises only when every one of
	// those eligible hosts is unusable (refine maps the resulting ErrNoHealthyHost).
	slog.Debug("voice filter: excluding candidates known to lack the voice", "voice", voice, "allowed", len(allowed), "known_absent", knownAbsent, "candidates", len(ids))
	return &voiceScope{allowed: allowed, narrowed: true}, nil
}

// candidateMappingIDs is the pool's distinct mapping ids in first-seen order: the ids
// voiceScopeFor reads the capability and voices caches for, each at most once.
func candidateMappingIDs(pool []MappingCandidate) []string {
	ids := make([]string, 0, len(pool))
	seen := make(map[string]struct{}, len(pool))
	for _, c := range pool {
		if _, dup := seen[c.Mapping.ID]; !dup {
			seen[c.Mapping.ID] = struct{}{}
			ids = append(ids, c.Mapping.ID)
		}
	}
	return ids
}

// plainTTSMappingIDs keeps the ids whose CURRENT stored verdicts make them a plain tts
// model (CapabilityRowsPlainTTS), the verdict gate the voices endpoint also applies at read
// time, so a cached list that outlived a tts -> clone (or tts -> non-speech) flip is not
// treated as a known voice here either.
func plainTTSMappingIDs(ids []string, verdicts map[string][]CapabilityRow) []string {
	tts := make([]string, 0, len(ids))
	for _, id := range ids {
		if CapabilityRowsPlainTTS(verdicts[id]) {
			tts = append(tts, id)
		}
	}
	return tts
}

// voiceListsEligible classifies ONE plain-tts mapping against the requested voice from its
// cached voices rows, reporting whether it stays eligible to serve the voice. The caller
// treats an ineligible mapping as the sole narrowing state, KNOWN-ABSENT.
//
// FAIL-OPEN on UNKNOWN: a mapping with no voices row (never voice-cached) or an undecodable
// one has unknown voice status, which is NOT absent, so it is kept eligible and the upstream
// stays the authority on a name it was never told about. A KNOWN-PRESENT mapping (its cached
// list names the voice, exact match -- the name is relayed byte for byte) is eligible too.
// Only a KNOWN-ABSENT mapping -- row present and decodable, voice not in it -- is ineligible.
func voiceListsEligible(rows []ModelMappingCacheRow, voice, id string) bool {
	if !hasVoicesRow(rows) {
		// UNKNOWN voice status: this mapping has no voices row, so nothing is known about
		// what it offers. Unknown is NOT absent -- keep it eligible (fail open) and leave
		// the upstream the authority on the name. A healthy, never-voice-cached sibling of
		// the same model therefore stays a failover/load target.
		return true
	}
	names, err := VoiceNamesFromCacheRows(rows)
	if err != nil {
		// CORRUPT: an undecodable row is treated as UNKNOWN voice status and kept eligible
		// (fail open; the writer cannot produce one, so the outage is logged at Warn).
		slog.Warn("voice filter: voices cache row is undecodable, treated as unknown voice status, kept eligible", "mapping_id", id, "err", err)
		return true
	}
	if slices.Contains(names, voice) {
		// KNOWN-PRESENT: the mapping's cached list names the voice.
		return true
	}
	// KNOWN-ABSENT: the row exists and decodes but does not list the voice.
	return false
}
