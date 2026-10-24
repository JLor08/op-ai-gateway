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

// FirstPresetVoice is the preset voice a caller may ask a model for when it has to name
// one itself: the first bytewise-sorted non-blank name of the mapping's voices cache, or ""
// when the cache lists none -- no row, an empty or null list, only blank names, or a payload
// that does not decode. "Has presets" is therefore FirstPresetVoice(rows) != "", and it is
// the one definition of it: the speech benchmark picks the voice it synthesizes a configured
// clone model's clip with from here, and the app-health loop decides whether such a model is
// a benchmark target by the same answer, so the two cannot disagree about which mappings
// have presets.
//
// The writers keep the list sorted and without blanks, but this does not rely on that: the
// smallest name is found directly, so a row written in another layout still yields a stable
// pick (the benchmark's voice is the same one tick after tick). Names are returned verbatim
// -- audio.cpp matches a voice name exactly -- so a non-blank name is never trimmed.
func FirstPresetVoice(rows []ModelMappingCacheRow) string {
	names, err := VoiceNamesFromCacheRows(rows)
	if err != nil {
		return ""
	}
	first := ""
	for _, name := range names {
		if strings.TrimSpace(name) == "" {
			continue
		}
		if first == "" || name < first {
			first = name
		}
	}
	return first
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
	// allowed is the ids of the CLASSIFIED mappings eligible to serve the voice: those whose
	// cached voice list names it, PLUS those whose voice status is UNKNOWN (a plain-tts mapping
	// with no cached row, or any classified mapping with an unreadable one) and so stay eligible
	// fail-open (unknown != absent). A speech clone with no voices row is not classified at all,
	// so it is never in allowed -- it is dropped from a narrowed pool and kept only on a nil
	// scope, as every clone was before #305. allowed is never empty: when every classified
	// candidate is known-absent the pre-pass returns a nil scope instead.
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
// The voice is classified over every speech mapping that carries voice data: a plain tts
// model (CapabilityRowsPlainTTS, whatever its cache state) AND a clone model that has a
// voices cache row (hasVoicesRow), so an offered gateway name that mixes a plain tts mapping
// and a configured clone model with presets routes a named voice to the mapping whose cached
// list names it instead of to a sibling that would answer a 500 (#305). A candidate is
// known-present when the voice is in its mapping's cached list (exact match: the name is
// relayed to the upstream byte for byte, so a near miss is a different voice) AND the mapping
// is a speech model NOW (the verdicts are the CURRENT ones, so a list that outlived a flip to a
// non-speech model is not a known voice here). A candidate whose cached list does NOT name the
// voice is known-absent and excluded -- a plain tts model's, a clone model's preset list
// without it, and a reference-only clone's confirmed-empty "[]" alike, which is what routes a
// named-voice request away from a clone with no presets while an eligible candidate remains. A
// plain tts mapping with no cached voices row at all is unknown and kept.
//
// A clone model with NO voices row (never voice-listed: a cold child, an older agent, a failed
// listing) is not classified at all: it is neither known-present, known-absent nor "unknown and
// kept", it is skipped -- left out of the allowed set, so it is dropped from a pool the voice
// narrowed and kept whenever the filter fails open, exactly as every clone mapping was before
// the filter consulted clone voices. Keeping it eligible in a narrowed pool would let a cold
// clone win routing and answer a 500 on a voice it does not serve. The staleness bound is the
// one ADR-067 already accepts for a plain tts mapping's cached voices: the voices producer
// refreshes a loaded clone model's list on its tick, so only a clone whose configuration
// changed while it was unloaded can briefly carry an out-of-date list.
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
	speech := speechMappingIDs(ids, verdicts)
	if len(speech) == 0 {
		return nil, nil
	}
	cached, err := r.store.ModelMappingCacheForMappings(ctx, speech, ModelMappingCacheVoices)
	if err != nil {
		// FAIL OPEN, see above: no cache, no voice information, no restriction.
		slog.Warn("voice filter: voices cache could not be read, routing unfiltered", "voice", voice, "candidates", len(ids), "err", err)
		return nil, nil
	}
	classified := voiceClassifiedIDs(speech, verdicts, cached)
	if len(classified) == 0 {
		// No candidate carries voice data (every one is a clone model that was never
		// voice-listed): nothing is classified, so the voice restricts nothing.
		return nil, nil
	}
	allowed := make(map[string]struct{}, len(classified))
	knownAbsent := 0
	for _, id := range classified {
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
		// Every CLASSIFIED candidate is KNOWN-absent: no cached list offers the voice (a skipped
		// no-row clone is not classified, so it does not count here). FAIL OPEN --
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

// speechMappingIDs keeps the ids whose CURRENT stored verdicts make them a speech model at all
// (CapabilityRowsSpeech): a plain tts model and a clone model alike, the mappings whose cached
// voices the voice filter may read. A mapping that is not a speech model NOW (flipped off
// speech, or never one) is left out, so a cached list that outlived the flip is not treated as
// a known voice. These are the ids voiceScopeFor reads the voices cache for, in one batch.
func speechMappingIDs(ids []string, verdicts map[string][]CapabilityRow) []string {
	speech := make([]string, 0, len(ids))
	for _, id := range ids {
		if CapabilityRowsSpeech(verdicts[id]) {
			speech = append(speech, id)
		}
	}
	return speech
}

// voiceClassifiedIDs picks, of the speech mappings, the ones the voice is classified against
// (voiceListsEligible): a plain tts model (CapabilityRowsPlainTTS, any cache state -- one with
// no voices row is classified as unknown and kept) OR a clone model that has a voices cache row
// (hasVoicesRow: its voices were listed at least once, a preset list or a confirmed-empty "[]").
// A clone model with no voices row carries no voice data and is skipped here, so it is neither
// allowed nor counted known-absent (see voiceScopeFor). The order of speech is kept.
func voiceClassifiedIDs(speech []string, verdicts map[string][]CapabilityRow, cached map[string][]ModelMappingCacheRow) []string {
	classified := make([]string, 0, len(speech))
	for _, id := range speech {
		if CapabilityRowsPlainTTS(verdicts[id]) || hasVoicesRow(cached[id]) {
			classified = append(classified, id)
		}
	}
	return classified
}

// voiceListsEligible classifies ONE classified speech mapping -- a plain tts model, or a clone
// model with a voices row -- against the requested voice from its cached voices rows,
// reporting whether it stays eligible to serve the voice. The caller treats an ineligible
// mapping as the sole narrowing state, KNOWN-ABSENT.
//
// FAIL-OPEN on UNKNOWN: a mapping with no voices row (never voice-cached) or an undecodable
// one has unknown voice status, which is NOT absent, so it is kept eligible and the upstream
// stays the authority on a name it was never told about. (A clone model with no row never gets
// here: voiceClassifiedIDs skips it.) A KNOWN-PRESENT mapping (its cached list names the voice,
// exact match -- the name is relayed byte for byte) is eligible too. Only a KNOWN-ABSENT mapping
// -- row present and decodable, voice not in it, a clone's preset list without it or its
// confirmed-empty "[]" included -- is ineligible.
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
