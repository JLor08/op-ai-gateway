// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package runtime

import (
	"bytes"
	"sort"
	"strings"
)

// minMaskedSecretLen is the shortest resolved secret the log masker replaces
// (#430 §8). A shorter value would shred the output: a 3-byte token masks
// every "abc" a model server prints, and a 1-byte one every occurrence of that
// character. Such a value is not a meaningful secret to begin with, and the
// spec editor's guidance (${AGENT_ENV:NAME} for a real credential) is
// unchanged.
const minMaskedSecretLen = 8

// maskPair is one resolved secret value and the placeholder that produced it
// (secretSpan.mask), for example "${AGENT_ENV:HF_TOKEN}".
type maskPair struct {
	value string
	mask  string
}

// secretMasker replaces, in a generation's log output, every LITERAL
// occurrence of a value the agent itself substituted for ${API_TOKEN} or
// ${AGENT_ENV:NAME} in that generation's launch, with the placeholder that
// produced it -- the same text the resolved command shows in its place.
//
// What it masks is exactly the secretSpan values of the generation's
// expandedSpec, nothing else:
//
//   - Plain env and argument values are NOT masked, because they are not
//     secrets the agent resolved (CUDA_VISIBLE_DEVICES=0 would otherwise mask
//     every "0"), and neither are the agent-computed ${PORT}, ${MODEL} etc.
//   - Values shorter than minMaskedSecretLen are skipped.
//   - Only literal byte-for-byte occurrences are found. An encoded form (URL-
//     or JSON-escaped, base64, hex), a value the child splits across lines or
//     interleaves with other text, or a secret the child obtained in any other
//     way (a file it read, a token it minted) passes through unchanged. This
//     is a guard against the common case -- a server echoing its own argv or
//     env -- not a data-loss-prevention filter.
//
// pairs is sorted longest value first, so at any position the longest
// matching value wins (a value that contains another is masked whole), and
// deduplicated by value (the first span's placeholder names it). maxLen is the
// longest value, which bounds the carry a streaming caller must keep.
//
// A secretMasker is immutable after newSecretMasker, so one value is shared by
// every stream of a generation (procLog.ForStream); the carry it needs between
// writes belongs to each procLog.
type secretMasker struct {
	pairs  []maskPair
	maxLen int
}

// newSecretMasker builds the masker of the generation launched from ex, from
// the plaintext that exists only there. It returns nil -- a masker that masks
// nothing, at no cost -- when the launch substituted no secret of at least
// minMaskedSecretLen bytes. A span that does not fit its string (never
// produced by expandSpec; defensive) is ignored rather than trusted.
//
// The wrapper of a wsl launch (expandedSpec.withWrapper) shifts the spans but
// not the values, and an rpc member's agent-owned arguments carry no spans, so
// building it from ex at any point after expansion yields the same masker.
func newSecretMasker(ex expandedSpec) *secretMasker {
	seen := make(map[string]bool)
	var pairs []maskPair
	collect := func(strs []string, spans [][]secretSpan) {
		for i, ss := range spans {
			if i >= len(strs) {
				break
			}
			for _, sp := range ss {
				if sp.start < 0 || sp.end > len(strs[i]) || sp.end-sp.start < minMaskedSecretLen {
					continue
				}
				v := strs[i][sp.start:sp.end]
				if seen[v] {
					continue
				}
				seen[v] = true
				pairs = append(pairs, maskPair{value: v, mask: sp.mask})
			}
		}
	}
	collect(ex.args, ex.argSpans)
	collect(ex.env, ex.envSpans)
	if len(pairs) == 0 {
		return nil
	}
	sort.SliceStable(pairs, func(i, j int) bool { return len(pairs[i].value) > len(pairs[j].value) })
	return &secretMasker{pairs: pairs, maxLen: len(pairs[0].value)}
}

// mask returns carry+b with every secret value replaced by its placeholder,
// and the bytes it held back for the next call.
//
// The scan is leftmost-first, longest-first at each position. Unless final is
// set it stops maxLen-1 bytes before the end: at an earlier position every
// value can be compared in full, but at a later one a value could begin that
// the next write completes. Those bytes are returned as keep -- a fresh copy,
// never a slice of b, which os/exec reuses for its next read -- and the caller
// passes them back as carry. The scan resumes exactly where it stopped, so the
// result over any split of a stream into writes is byte-for-byte the result
// over the whole stream: nothing is lost, duplicated, or cut inside a match. A
// match that starts before the stop point is consumed whole even if it reaches
// past it.
//
// final emits everything (a stream ends: process exit, a tailer's stop). A nil
// masker returns carry+b unchanged and keeps nothing.
func (m *secretMasker) mask(carry, b []byte, final bool) (out string, keep []byte) {
	if m == nil {
		return string(carry) + string(b), nil
	}
	joined := b
	if len(carry) > 0 {
		joined = make([]byte, 0, len(carry)+len(b))
		joined = append(joined, carry...)
		joined = append(joined, b...)
	}
	limit := len(joined)
	if !final {
		limit -= m.maxLen - 1
	}
	var sb strings.Builder
	sb.Grow(len(joined))
	i, emitted := 0, 0
scan:
	for i < limit {
		for _, p := range m.pairs {
			// The string(...) == comparison does not allocate.
			if end := i + len(p.value); end <= len(joined) && string(joined[i:end]) == p.value {
				sb.Write(joined[emitted:i])
				sb.WriteString(p.mask)
				i += len(p.value)
				emitted = i
				continue scan
			}
		}
		i++
	}
	sb.Write(joined[emitted:i])
	if i < len(joined) {
		keep = bytes.Clone(joined[i:])
	}
	return sb.String(), keep
}
