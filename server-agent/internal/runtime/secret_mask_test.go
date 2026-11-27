// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package runtime

import (
	"math/rand"
	"strings"
	"testing"
	"unicode/utf8"
)

// maskerFor expands spec with getenv the way startProcess does and returns the
// generation's masker -- so the tests exercise the real span recording, not a
// hand-built expandedSpec that could drift from it.
func maskerFor(t *testing.T, spec Spec, env map[string]string) *secretMasker {
	t.Helper()
	if spec.Binary == "" {
		spec.Binary = "/opt/llama/llama-server"
	}
	ex, err := expandSpec(spec, 8081, GPUVendorNone, nil, func(k string) string { return env[k] }, "")
	if err != nil {
		t.Fatalf("expandSpec: %v", err)
	}
	return newSecretMasker(ex)
}

// maskAll runs the whole input through m as one final write.
func maskAll(m *secretMasker, s string) string {
	var st maskCarry
	out := m.mask(&st, []byte(s), true)
	if !st.empty() {
		panic("a final mask kept state")
	}
	return out
}

// TestSecretMaskerMasks pins §8's rule: the values the agent itself substituted
// for ${API_TOKEN} and ${AGENT_ENV:NAME} are replaced by the placeholder that
// produced them; values under 8 bytes and plain env values are left alone; of
// two overlapping values the longer wins.
func TestSecretMaskerMasks(t *testing.T) {
	m := maskerFor(t, Spec{
		Args: []string{
			"--hf", "${AGENT_ENV:HF_TOKEN}",
			"--short", "${AGENT_ENV:SHORT}",
			"--long", "x${AGENT_ENV:LONG}y",
			"--api-key", "${API_TOKEN}",
		},
		Env:      map[string]string{"PLAIN": "plainvalue123", "HF_AGAIN": "${AGENT_ENV:HF_TOKEN}"},
		APIToken: "tok-0123456789",
	}, map[string]string{
		"HF_TOKEN": "hf_abcdefgh123",
		"SHORT":    "abc1234", // 7 bytes: below the minimum
		"LONG":     "hf_abcdefgh123XYZ",
	})

	cases := []struct{ name, in, want string }{
		{"agent env value", "token is hf_abcdefgh123.", "token is ${AGENT_ENV:HF_TOKEN}."},
		{"api token", "--api-key tok-0123456789\n", "--api-key ${API_TOKEN}\n"},
		{"7-byte value untouched", "short=abc1234", "short=abc1234"},
		{"plain env value untouched", "PLAIN=plainvalue123", "PLAIN=plainvalue123"},
		{"longest overlapping value wins", "a hf_abcdefgh123XYZ b", "a ${AGENT_ENV:LONG} b"},
		{"shorter value still masked on its own", "hf_abcdefgh123XY", "${AGENT_ENV:HF_TOKEN}XY"},
		{"every occurrence", "hf_abcdefgh123 hf_abcdefgh123", "${AGENT_ENV:HF_TOKEN} ${AGENT_ENV:HF_TOKEN}"},
		{"adjacent occurrences are one span", "hf_abcdefgh123hf_abcdefgh123", "${AGENT_ENV:HF_TOKEN}"},
		{"nothing to mask", "llama server listening on 127.0.0.1:8081", "llama server listening on 127.0.0.1:8081"},
	}
	for _, c := range cases {
		if got := maskAll(m, c.in); got != c.want {
			t.Errorf("%s: mask(%q) = %q, want %q", c.name, c.in, got, c.want)
		}
	}

	// The same value reached through two placeholders is one pair (the first
	// span's placeholder), and values below the minimum are not pairs at all.
	if len(m.pairs) != 3 {
		t.Errorf("pairs = %+v, want 3 (HF_TOKEN deduplicated, SHORT skipped)", m.pairs)
	}
	for i := 1; i < len(m.pairs); i++ {
		if len(m.pairs[i].value) > len(m.pairs[i-1].value) {
			t.Errorf("pairs not sorted longest first: %+v", m.pairs)
		}
	}
	if m.maxLen != len("hf_abcdefgh123XYZ") {
		t.Errorf("maxLen = %d, want %d", m.maxLen, len("hf_abcdefgh123XYZ"))
	}
}

// TestSecretMaskerNilAndEmpty: a generation without secrets gets no masker,
// and a nil masker passes every byte through with no carry.
func TestSecretMaskerNilAndEmpty(t *testing.T) {
	if m := maskerFor(t, Spec{Args: []string{"--port", "${PORT}"}, Env: map[string]string{"A": "longplainvalue"}}, nil); m != nil {
		t.Errorf("newSecretMasker = %+v for a spec with no secret placeholders, want nil", m)
	}
	if m := maskerFor(t, Spec{Args: []string{"${AGENT_ENV:S}"}}, map[string]string{"S": "1234567"}); m != nil {
		t.Errorf("newSecretMasker = %+v when every secret is below the minimum, want nil", m)
	}
	var m *secretMasker
	var st maskCarry
	if out := m.mask(&st, []byte("bytes"), false); out != "bytes" || !st.empty() {
		t.Errorf("nil masker: (%q, %+v), want (\"bytes\", empty)", out, st)
	}
}

// TestSecretMaskerCarryNeverCutsAMatch: a non-final mask keeps exactly the
// longest suffix that could still begin a secret, never a match it has seen
// whole, and never aliases the caller's buffer (os/exec reuses it for the
// next read).
func TestSecretMaskerCarryNeverCutsAMatch(t *testing.T) {
	m := maskerFor(t, Spec{Args: []string{"${AGENT_ENV:HF_TOKEN}"}}, map[string]string{"HF_TOKEN": "hf_abcdefgh123"})

	var st maskCarry
	if out := m.mask(&st, []byte("xxhf_abcdefgh123"), false); out != "xx${AGENT_ENV:HF_TOKEN}" || len(st.pending) != 0 || !st.open {
		t.Errorf("mask = (%q, %+v), want the match emitted whole, nothing pending, the span still open", out, st)
	}
	// "y" cannot begin a secret, so it closes the span and is emitted.
	if out := m.mask(&st, []byte("y"), false); out != "y" || !st.empty() {
		t.Errorf("mask = (%q, %+v), want \"y\" and an empty state", out, st)
	}

	buf := []byte("0123456789abcdefghij hf_abcdefgh12")
	out := m.mask(&st, buf, false)
	if out != "0123456789abcdefghij " || string(st.pending) != "hf_abcdefgh12" {
		t.Errorf("mask = (%q, %q), want only the possible secret start held back", out, st.pending)
	}
	copy(buf, strings.Repeat("Z", len(buf)))
	if strings.Contains(string(st.pending), "Z") {
		t.Error("the carry aliases the caller's buffer")
	}
	// Not the secret after all: the held-back bytes come out unchanged.
	if out := m.mask(&st, []byte("X"), false); out != "hf_abcdefgh12X" || !st.empty() {
		t.Errorf("mask = (%q, %+v), want the held-back bytes released", out, st)
	}
}

// TestSecretMaskerAcrossWrites is review focus 2: a secret split across two
// pipe reads is still masked, and the ring -- every record, not only what
// Drain happens to show -- never holds it in clear, nor any record holding
// the start of it.
func TestSecretMaskerAcrossWrites(t *testing.T) {
	m := maskerFor(t, Spec{Args: []string{"${AGENT_ENV:HF_TOKEN}"}}, map[string]string{"HF_TOKEN": "hf_abcdefgh123"})
	for _, k := range []int{0, 1} {
		s := NewLogStore(1<<20, 1<<20)
		s.SetWatch([]string{"spec"}, nil)
		s.Drain()
		p := s.newInstanceProcMasked("spec", k, m)
		file := p.ForStream("engine")

		for _, w := range []*procLog{p, file} {
			_, _ = w.Write([]byte("tok=hf_abcd"))
			assertRingClean(t, s, "hf_abcd")
			_, _ = w.Write([]byte("efgh123\n"))
			w.Flush()
		}
		prefix := instanceLinePrefix(k)
		want := prefix + "tok=${AGENT_ENV:HF_TOKEN}\n"
		for _, stream := range []string{"", "engine"} {
			if got := ringText(s, "spec", stream); got != want {
				t.Errorf("instance %d stream %q ring = %q, want %q", k, stream, got, want)
			}
		}
		assertRingClean(t, s, "hf_abcd")
		live := s.Drain()
		for _, stream := range []string{"", "engine"} {
			if got := streamText(live, stream); got != want {
				t.Errorf("instance %d stream %q live = %q, want %q", k, stream, got, want)
			}
		}
	}
}

// TestSecretMaskerAnyChunkingLosesNothing: however the bytes are split into
// writes -- one byte at a time included -- what the ring ends up holding after
// Flush is exactly the masking of the whole stream: nothing lost, nothing
// duplicated, nothing in clear.
func TestSecretMaskerAnyChunkingLosesNothing(t *testing.T) {
	m := maskerFor(t, Spec{
		Args:     []string{"${AGENT_ENV:HF_TOKEN}", "${AGENT_ENV:LONG}", "${API_TOKEN}", "${AGENT_ENV:A}", "${AGENT_ENV:B}", "${AGENT_ENV:ODD}"},
		APIToken: "tok-0123456789",
	}, map[string]string{
		"HF_TOKEN": "hf_abcdefgh123", "LONG": "hf_abcdefgh123XYZ",
		"A": "abcdefgh12", "B": "cdefgh1234567", "ODD": "\x88\x88hf_abcdefgh",
	})
	stream := "start hf_abcdefgh123 mid hf_abcdefgh123XYZ tok-0123456789\nhf_abcdefgh12 hf_abcdefgh123XY end\n" +
		"█x█\x88\x88hf_abcdefgh█ abcdefgh1234567 abcdefgh12abcdefgh12hf_abcdefgh123 hf_abcdefgh123hf_\n"
	want := maskAll(m, stream)
	if strings.Contains(want, "34567") || strings.Contains(want, "hf_abcdefgh123") {
		t.Fatalf("whole-stream mask %q leaves a matched byte in clear", want)
	}

	rng := rand.New(rand.NewSource(1)) //nolint:gosec // deterministic test chunking
	for round := range 200 {
		s := NewLogStore(1<<20, 1<<20)
		p := s.newInstanceProcMasked("spec", 0, m)
		rest := stream
		for rest != "" {
			n := 1
			if round > 0 {
				n = 1 + rng.Intn(len(rest))
			}
			_, _ = p.Write([]byte(rest[:n]))
			rest = rest[n:]
		}
		p.Flush()
		if got := ringText(s, "spec", ""); got != want {
			t.Fatalf("round %d: ring = %q, want %q", round, got, want)
		}
		assertRingClean(t, s, "hf_abcdefgh123", "tok-0123456789", "cdefgh1234567", "abcdefgh12a")
	}
}

// TestProcLogExitedFlushesCarry: the exit marker lands after every byte the
// generation wrote, the carry included, even if a caller forgot Flush.
func TestProcLogExitedFlushesCarry(t *testing.T) {
	m := maskerFor(t, Spec{Args: []string{"${AGENT_ENV:HF_TOKEN}"}}, map[string]string{"HF_TOKEN": "hf_abcdefgh123"})
	s := NewLogStore(1<<20, 1<<20)
	p := s.newInstanceProcMasked("spec", 0, m)
	_, _ = p.Write([]byte("last words hf_ab")) // "hf_ab" could begin the secret: held back
	p.Exited(3)
	r := s.logs["spec"].streams[""]
	if len(r.recs) != 2 || r.recs[0].text != "last words hf_ab" || r.recs[1].event != logEventExited {
		t.Fatalf("records = %+v, want the carried text and then the exit marker", r.recs)
	}
}

// ringText joins every text record of stream in specID's retained ring.
func ringText(s *LogStore, specID, stream string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	l := s.logs[specID]
	if l == nil || l.streams[stream] == nil {
		return ""
	}
	var b strings.Builder
	for _, r := range l.streams[stream].recs {
		b.WriteString(r.text)
	}
	return b.String()
}

// assertRingClean fails if any record of any stream holds one of clear.
func assertRingClean(t *testing.T, s *LogStore, clear ...string) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, l := range s.logs {
		for name, r := range l.streams {
			for _, rec := range r.recs {
				for _, c := range clear {
					if strings.Contains(rec.text, c) {
						t.Errorf("spec %q stream %q record %q holds %q in clear", id, name, rec.text, c)
					}
				}
			}
		}
	}
}

// --- fix round 1: hold back only what could still begin a secret ------------

// TestSecretMaskerQuietChildTailIsComplete: a child that falls quiet after a
// line must have that whole line in its Tail without a Flush -- only bytes that
// could still begin a secret are held back, never a fixed maxLen-1.
func TestSecretMaskerQuietChildTailIsComplete(t *testing.T) {
	m := maskerFor(t, Spec{Args: []string{"${AGENT_ENV:HF_TOKEN}"}}, map[string]string{"HF_TOKEN": "hf_abcdefgh123"})
	s := NewLogStore(1<<20, 1<<20)
	p := s.newInstanceProcMasked("spec", 0, m)
	_, _ = p.Write([]byte("srv  update_slots: all slots are idle\n"))
	if got := p.Tail(2048); got != "srv  update_slots: all slots are idle\n" {
		t.Errorf("Tail = %q, want the quiet child's whole last line", got)
	}
	// A suffix that could still begin the secret is held back, and only that.
	_, _ = p.Write([]byte("key=hf_ab"))
	if got := p.Tail(2048); !strings.HasSuffix(got, "idle\nkey=") {
		t.Errorf("Tail = %q, want everything up to the possible secret start", got)
	}
	_, _ = p.Write([]byte("cdefgh123 ok"))
	if got := p.Tail(2048); !strings.HasSuffix(got, "key=${AGENT_ENV:HF_TOKEN} ok") {
		t.Errorf("Tail = %q, want the completed secret masked and the text after it present", got)
	}
}

// TestSecretMaskerMemberBannerCompleteWithoutFlush: an rpc member's banner,
// written line by line with a masker present, parses from Tail while the
// process is still running (awaitMemberBanner never flushes).
func TestSecretMaskerMemberBannerCompleteWithoutFlush(t *testing.T) {
	// A secret longer than the closing "  transport ..." line, so a fixed
	// longest-minus-one hold-back would swallow that whole line.
	long := "hf_" + strings.Repeat("0123456789", 6)
	m := maskerFor(t, Spec{Args: []string{"${AGENT_ENV:HF_TOKEN}"}}, map[string]string{"HF_TOKEN": long})
	s := NewLogStore(1<<20, 1<<20)
	p := s.newInstanceProcMasked("spec", 0, m)
	for _, line := range strings.SplitAfter(realMemberBanner, "\n") {
		_, _ = p.Write([]byte(line))
	}
	tail := p.Tail(64 << 10)
	if tail != realMemberBanner {
		t.Errorf("Tail = %q, want the whole banner", tail)
	}
	if _, ok := parseMemberBanner(strings.Split(tail, "\n")); !ok {
		t.Fatalf("banner did not parse from Tail %q", tail)
	}
}

// TestSecretMaskerHoldBackKeepsRunesWhole: a hold-back that would start inside
// a multi-byte character starts at that character instead, so no drained
// entry is manufactured invalid UTF-8 out of valid output.
func TestSecretMaskerHoldBackKeepsRunesWhole(t *testing.T) {
	// "█" is E2 96 88; this secret begins with that character's last byte, so
	// "x█" ends in a possible secret start inside the character.
	m := maskerFor(t, Spec{Args: []string{"${AGENT_ENV:ODD}"}}, map[string]string{"ODD": "\x88\x88hf_abcdefgh"})
	s := NewLogStore(1<<20, 1<<20)
	s.SetWatch([]string{"spec"}, nil)
	s.Drain()
	p := s.newInstanceProcMasked("spec", 0, m)
	_, _ = p.Write([]byte("x█"))
	first := s.Drain()
	_, _ = p.Write([]byte(strings.Repeat("y", 12) + "\n"))
	all := append(first, s.Drain()...)
	for _, b := range all {
		for _, e := range b.Entries {
			if !utf8.ValidString(e.Text) {
				t.Errorf("drained entry %q is not valid UTF-8", e.Text)
			}
		}
	}
	if got := streamText(all, ""); got != "x█"+strings.Repeat("y", 12)+"\n" {
		t.Errorf("drained text = %q, want every byte once", got)
	}

	// The review's probe: a character a few bytes before the end of one write,
	// with a 14-byte secret present.
	m = maskerFor(t, Spec{Args: []string{"${AGENT_ENV:HF_TOKEN}"}}, map[string]string{"HF_TOKEN": "hf_abcdefgh123"})
	s = NewLogStore(1<<20, 1<<20)
	s.SetWatch([]string{"spec"}, nil)
	s.Drain()
	p = s.newInstanceProcMasked("spec", 0, m)
	_, _ = p.Write([]byte("x█" + strings.Repeat("y", 12)))
	for _, b := range s.Drain() {
		for _, e := range b.Entries {
			if !utf8.ValidString(e.Text) {
				t.Errorf("probe: drained entry %q is not valid UTF-8", e.Text)
			}
		}
	}
}

// TestSecretMaskerMasksUnionOfOverlaps: no byte of any matched value stays in
// clear. Overlapping and adjacent matches merge into one masked span, named by
// the value that starts first (the longest of those starting there).
func TestSecretMaskerMasksUnionOfOverlaps(t *testing.T) {
	m := maskerFor(t, Spec{Args: []string{"${AGENT_ENV:A}", "${AGENT_ENV:B}"}}, map[string]string{
		"A": "abcdefgh12",    // 10 bytes
		"B": "cdefgh1234567", // 13 bytes, overlapping A's tail
	})
	cases := []struct{ in, want string }{
		{"<abcdefgh1234567>", "<${AGENT_ENV:A}>"},
		{"<abcdefgh12abcdefgh12>", "<${AGENT_ENV:A}>"},
		{"<abcdefgh12 abcdefgh12>", "<${AGENT_ENV:A} ${AGENT_ENV:A}>"},
		{"<cdefgh1234567abcdefgh12>", "<${AGENT_ENV:B}>"},
	}
	for _, c := range cases {
		if got := maskAll(m, c.in); got != c.want {
			t.Errorf("mask(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
