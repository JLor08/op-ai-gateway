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
	m := &secretMasker{pairs: pairs, maxLen: len(pairs[0].value)}
	for _, p := range pairs {
		m.first[p.value[0]] = true
	}
	return m
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
	joined := b
	if len(st.pending) > 0 {
		joined = make([]byte, 0, len(st.pending)+len(b))
		joined = append(joined, st.pending...)
		joined = append(joined, b...)
	}
	n := len(joined)
	var sb strings.Builder
	sb.Grow(n)
	open, end := st.open, st.end
	plain := 0 // the first byte not yet written; meaningful while no span is open
	for i := 0; i < n; i++ {
		if open && i > end {
			open, plain = false, end
		}
		if !m.first[joined[i]] {
			continue
		}
		if !final && m.couldBegin(joined[i:]) {
			if open {
				*st = maskCarry{pending: bytes.Clone(joined[i:]), open: true, end: end - i}
				return sb.String()
			}
			c := i
			for c > plain && i-c < utf8.UTFMax-1 && !utf8.RuneStart(joined[c]) {
				c--
			}
			sb.Write(joined[plain:c])
			*st = maskCarry{pending: bytes.Clone(joined[c:])}
			return sb.String()
		}
		k := m.longestAt(joined[i:])
		if k < 0 {
			continue
		}
		if e := i + len(m.pairs[k].value); open {
			end = max(end, e)
		} else {
			sb.Write(joined[plain:i])
			sb.WriteString(m.pairs[k].mask)
			open, end = true, e
		}
	}
	if open && n > end {
		open, plain = false, end
	}
	if !open {
		sb.Write(joined[plain:])
	}
	*st = maskCarry{}
	if open && !final {
		// The input ended exactly at the open span's end: a secret beginning
		// with the next byte would still join it.
		*st = maskCarry{open: true}
	}
	return sb.String()
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
