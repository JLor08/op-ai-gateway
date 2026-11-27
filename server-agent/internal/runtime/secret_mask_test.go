// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package runtime

import (
	"math/rand"
	"strings"
	"testing"
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
	out, keep := m.mask(nil, []byte(s), true)
	if len(keep) != 0 {
		panic("a final mask kept bytes")
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
		{"every occurrence", "hf_abcdefgh123hf_abcdefgh123", "${AGENT_ENV:HF_TOKEN}${AGENT_ENV:HF_TOKEN}"},
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
	out, keep := m.mask([]byte("carry-"), []byte("bytes"), false)
	if out != "carry-bytes" || keep != nil {
		t.Errorf("nil masker: (%q, %q), want (\"carry-bytes\", nil)", out, keep)
	}
}

// TestSecretMaskerCarryNeverCutsAMatch: a non-final mask keeps at most
// maxLen-1 bytes, never inside a match, and never aliases the caller's buffer
// (os/exec reuses it for the next read).
func TestSecretMaskerCarryNeverCutsAMatch(t *testing.T) {
	m := maskerFor(t, Spec{Args: []string{"${AGENT_ENV:HF_TOKEN}"}}, map[string]string{"HF_TOKEN": "hf_abcdefgh123"})

	buf := []byte("xxhf_abcdefgh123")
	out, keep := m.mask(nil, buf, false)
	if out != "xx${AGENT_ENV:HF_TOKEN}" || len(keep) != 0 {
		t.Errorf("mask = (%q, %q), want the match emitted whole and nothing kept", out, keep)
	}

	buf = []byte("0123456789abcdefghij")
	out, keep = m.mask(nil, buf, false)
	if out+string(keep) != string(buf) || len(keep) != m.maxLen-1 {
		t.Errorf("mask = (%q, %q), want the last %d bytes kept and nothing lost", out, keep, m.maxLen-1)
	}
	copy(buf, "ZZZZZZZZZZZZZZZZZZZZ")
	if strings.Contains(string(keep), "Z") {
		t.Error("the carry aliases the caller's buffer")
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
		Args:     []string{"${AGENT_ENV:HF_TOKEN}", "${AGENT_ENV:LONG}", "${API_TOKEN}"},
		APIToken: "tok-0123456789",
	}, map[string]string{"HF_TOKEN": "hf_abcdefgh123", "LONG": "hf_abcdefgh123XYZ"})
	stream := "start hf_abcdefgh123 mid hf_abcdefgh123XYZ tok-0123456789\nhf_abcdefgh12 hf_abcdefgh123XY end\n"
	want := maskAll(m, stream)

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
		assertRingClean(t, s, "hf_abcdefgh123", "tok-0123456789")
	}
}

// TestProcLogExitedFlushesCarry: the exit marker lands after every byte the
// generation wrote, the carry included, even if a caller forgot Flush.
func TestProcLogExitedFlushesCarry(t *testing.T) {
	m := maskerFor(t, Spec{Args: []string{"${AGENT_ENV:HF_TOKEN}"}}, map[string]string{"HF_TOKEN": "hf_abcdefgh123"})
	s := NewLogStore(1<<20, 1<<20)
	p := s.newInstanceProcMasked("spec", 0, m)
	_, _ = p.Write([]byte("last words"))
	p.Exited(3)
	r := s.logs["spec"].streams[""]
	if len(r.recs) != 2 || r.recs[0].text != "last words" || r.recs[1].event != logEventExited {
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
