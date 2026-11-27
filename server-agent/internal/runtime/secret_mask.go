// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package runtime

import (
	"bytes"
	"sort"
	"strings"
	"unicode/utf8"
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
// What gets replaced is the UNION of all matches: no byte of any matched value
// stays in clear, even where two values overlap (A="abcdefgh12" and
// B="cdefgh1234567" in "abcdefgh1234567"). Overlapping and adjacent matches
// merge into one masked span, rendered as ONE placeholder: that of the value
// that starts first, and of the values starting there the longest. Two
// back-to-back copies of a secret therefore read as one placeholder -- the
// exact count is not what an operator needs, the absence of clear text is.
//
// pairs is sorted longest value first (so the first match at a position is the
// longest) and deduplicated by value (the first span's placeholder names it).
// maxLen is the longest value. first marks the bytes some value begins with:
// a position whose byte is not marked can neither match nor begin a match,
// which keeps the per-byte cost under LogStore.mu to one table lookup for
// ordinary output.
//
// A secretMasker is immutable after newSecretMasker, so one value is shared by
// every stream of a generation (procLog.ForStream); the state it needs between
// writes (maskCarry) belongs to each procLog.
type secretMasker struct {
	pairs  []maskPair
	maxLen int
	first  [256]bool
}

// maskCarry is one stream's masking state between two writes: the bytes that
// could still begin a secret (pending), and whether the masked span the
// previous write ended in is still open -- its placeholder already emitted,
// and the first end bytes of pending inside it (end may be 0: a secret
// starting right at pending[0] would still join that span). The zero value is
// the state of a fresh stream.
type maskCarry struct {
	pending []byte
	open    bool
	end     int
}

// empty reports whether st holds nothing that a flush would have to resolve.
func (st *maskCarry) empty() bool { return len(st.pending) == 0 && !st.open }

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
	pairs = collectMaskPairs(pairs, seen, ex.args, ex.argSpans)
	pairs = collectMaskPairs(pairs, seen, ex.env, ex.envSpans)
	if len(pairs) == 0 {
		return nil
	}
	sort.SliceStable(pairs, func(i, j int) bool { return len(pairs[i].value) > len(pairs[j].value) })
	m := &secretMasker{pairs: pairs, maxLen: len(pairs[0].value)}
	for _, p := range pairs {
		m.first[p.value[0]] = true
	}
	return m
}

// collectMaskPairs is newSecretMasker's walk over one string list (args or
// env) and its spans: it appends to pairs every span value of at least
// minMaskedSecretLen bytes not yet in seen, in span order, and records it in
// seen. Spans past the end of strs, and spans that do not fit their string,
// are ignored rather than trusted.
func collectMaskPairs(pairs []maskPair, seen map[string]bool, strs []string, spans [][]secretSpan) []maskPair {
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
	return pairs
}

// mask returns st's pending bytes followed by b, with every secret value
// masked (see secretMasker), and leaves in st what the next call needs.
//
// The scan goes position by position. At each one: if a value could still
// complete from here -- the rest of the input is a proper prefix of a value
// longer than that rest -- and final is not set, it waits: the rest is kept in
// st.pending for the next call. Otherwise it takes the longest full match
// there, which opens a masked span or extends the one it overlaps or adjoins,
// or it advances. So what is held back is exactly the longest suffix that
// could still begin a secret (at most maxLen-1 bytes), not a fixed window: a
// quiet child's last line is retained in full unless it ends in the start of
// a secret, and live delivery lags by at most the bytes that could still
// begin one.
//
// Every decision depends only on the bytes from its position onwards, and a
// call that waits resumes at exactly that position (with the open span, if
// any, carried in st), so the result over any split of a stream into writes
// is byte-for-byte the result over the whole stream: nothing is lost,
// duplicated, or emitted in clear that the whole-stream scan would mask. A
// span's placeholder is emitted when the span opens, so a span still open at
// the end of a call costs no held-back bytes.
//
// A hold-back that would begin inside a multi-byte UTF-8 character begins at
// that character instead (at most utf8.UTFMax-1 bytes earlier), so a split
// never manufactures invalid UTF-8 out of valid output (the store's
// invariant, see splitRecord); those bytes are simply scanned again.
//
// final resolves everything: the end of a stream (process exit, a tailer's
// stop). A nil masker returns b unchanged and keeps nothing. Callers hold the
// lock that guards st.
func (m *secretMasker) mask(st *maskCarry, b []byte, final bool) string {
	if m == nil {
		return string(b)
	}
	s := maskScan{joined: joinPending(st.pending, b), open: st.open, end: st.end}
	s.sb.Grow(len(s.joined))
	for i := 0; i < len(s.joined); i++ {
		s.closeBefore(i)
		if !m.first[s.joined[i]] {
			continue
		}
		if !final && m.couldBegin(s.joined[i:]) {
			*st = s.holdBack(i)
			return s.sb.String()
		}
		if k := m.longestAt(s.joined[i:]); k >= 0 {
			s.match(i, m.pairs[k])
		}
	}
	*st = s.finish(final)
	return s.sb.String()
}

// joinPending returns pending followed by b: b itself when nothing is
// pending, otherwise a new slice (never aliasing pending, which the caller
// replaces).
func joinPending(pending, b []byte) []byte {
	if len(pending) == 0 {
		return b
	}
	joined := make([]byte, 0, len(pending)+len(b))
	joined = append(joined, pending...)
	return append(joined, b...)
}

// maskScan is the state of one mask call's scan over joined (st's pending
// bytes followed by the new write). open and end describe the masked span the
// scan is in: its placeholder is already in sb, and joined[:end] is inside it.
// plain is the first byte not yet written to sb; it is meaningful only while
// no span is open. A maskScan lives on mask's stack and touches nothing but
// its own fields, so its methods need no lock; the maskCarry they return is
// stored by mask, under the caller's lock.
type maskScan struct {
	joined []byte
	sb     strings.Builder
	open   bool
	end    int
	plain  int
}

// closeBefore closes the open span when position i lies past its end: the
// bytes from its end onwards are plain again. At i == end the span stays
// open, so a secret beginning right there still joins it.
func (s *maskScan) closeBefore(i int) {
	if s.open && i > s.end {
		s.open, s.plain = false, s.end
	}
}

// match takes the full match of p at position i: it extends the open span to
// cover it, or writes the plain bytes before i and p's placeholder and opens a
// span ending where p ends.
func (s *maskScan) match(i int, p maskPair) {
	e := i + len(p.value)
	if s.open {
		s.end = max(s.end, e)
		return
	}
	s.sb.Write(s.joined[s.plain:i])
	s.sb.WriteString(p.mask)
	s.open, s.end = true, e
}

// holdBack ends a non-final scan at position i, where a secret could still
// complete, and returns the carry for the next call. Inside an open span
// nothing is written (the span's placeholder already was) and the carry keeps
// the span open, with its end relative to the held bytes. Otherwise the plain
// bytes before the hold-back are written, and a hold-back that would begin
// inside a multi-byte UTF-8 character begins at that character instead (at
// most utf8.UTFMax-1 bytes earlier, never before plain).
func (s *maskScan) holdBack(i int) maskCarry {
	if s.open {
		return maskCarry{pending: bytes.Clone(s.joined[i:]), open: true, end: s.end - i}
	}
	c := i
	for c > s.plain && i-c < utf8.UTFMax-1 && !utf8.RuneStart(s.joined[c]) {
		c--
	}
	s.sb.Write(s.joined[s.plain:c])
	return maskCarry{pending: bytes.Clone(s.joined[c:])}
}

// finish ends a scan that reached the end of joined and returns the carry for
// the next call: the plain rest is written unless a span is still open. A span
// open exactly to the end of the input stays open in the carry unless final:
// a secret beginning with the next byte would still join it.
func (s *maskScan) finish(final bool) maskCarry {
	s.closeBefore(len(s.joined))
	if !s.open {
		s.sb.Write(s.joined[s.plain:])
		return maskCarry{}
	}
	if !final {
		// The input ended exactly at the open span's end: a secret beginning
		// with the next byte would still join it.
		return maskCarry{open: true}
	}
	return maskCarry{}
}

// couldBegin reports whether rest is a proper prefix of some value longer
// than rest -- a secret that the next bytes could still complete.
func (m *secretMasker) couldBegin(rest []byte) bool {
	if len(rest) >= m.maxLen {
		return false
	}
	for _, p := range m.pairs {
		if len(p.value) <= len(rest) {
			break // sorted longest first: no longer value follows
		}
		// The string(...) == comparison does not allocate.
		if string(rest) == p.value[:len(rest)] {
			return true
		}
	}
	return false
}

// longestAt returns the index of the longest value rest begins with, or -1.
func (m *secretMasker) longestAt(rest []byte) int {
	for k, p := range m.pairs {
		if len(p.value) <= len(rest) && string(rest[:len(p.value)]) == p.value {
			return k
		}
	}
	return -1
}
