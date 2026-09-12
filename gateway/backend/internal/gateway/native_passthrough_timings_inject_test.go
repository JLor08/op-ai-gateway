// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"bytes"
	"encoding/json"
	"testing"
)

// decodeInjected decodes a body the way this package decodes one — UseNumber, so a
// large integer literal is compared as it was written rather than through float64.
func decodeInjected(t *testing.T, body []byte) map[string]any {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	return m
}

// A body without the key gets it, every sibling field survives the round trip, and
// the client's own slice is not written to. The "a<b&c" value pins that
// re-serialization is VALUE-lossless: json.Marshal HTML-escapes it, writing
// a\u003cb\u0026c, which any JSON parser reads back as the original string.
func TestInjectTimingsPerTokenAddsTheFlag(t *testing.T) {
	const body = `{"model":"gw","input":"a<b&c","max_output_tokens":9007199254740993}`
	in := []byte(body)

	out, injected := injectTimingsPerToken(in)

	if !injected {
		t.Fatalf("injected = false, want true for a body without the key: %s", out)
	}
	m := decodeInjected(t, out)
	if m[timingsPerTokenKey] != true {
		t.Fatalf("%s = %#v, want true", timingsPerTokenKey, m[timingsPerTokenKey])
	}
	if m["model"] != "gw" {
		t.Fatalf("model not preserved: %#v", m["model"])
	}
	if m["input"] != "a<b&c" {
		t.Fatalf("input not preserved through HTML escaping: %#v", m["input"])
	}
	if n, _ := m["max_output_tokens"].(json.Number); n.String() != "9007199254740993" {
		t.Fatalf("large integer literal reformatted: %#v (helper lost UseNumber?)", m["max_output_tokens"])
	}
	if string(in) != body {
		t.Fatalf("the input slice was modified: %s", in)
	}
}

// The caller reads the client's own bytes again AFTER the relay, to build the
// payload capture, and rewriteModelField hands those same bytes back from its
// no-op branches — so the injected body must not share a backing array with the
// input, or the capture would record a key the client never sent. Aliasing, not a
// race: the relay, the copy loop and the capture read run in that order on one
// goroutine. Scribbling over every byte of the result must leave the input intact.
func TestInjectTimingsPerTokenReturnsUnaliasedBytes(t *testing.T) {
	const body = `{"model":"gw","stream":true,"input":"hi"}`
	in := []byte(body)

	out, injected := injectTimingsPerToken(in)

	if !injected {
		t.Fatalf("injected = false, want true for a body without the key: %s", out)
	}
	for i := range out {
		out[i] = 'X'
	}
	if string(in) != body {
		t.Fatalf("the returned slice shares backing memory with the input: %s", in)
	}
}

// Presence, not value: a client that already sent the key keeps its body verbatim.
func TestInjectTimingsPerTokenLeavesAClientSetTrueAlone(t *testing.T) {
	const body = `{"model":"gw","stream":true,"timings_per_token":true,"input":"hi"}`

	out, injected := injectTimingsPerToken([]byte(body))

	if injected {
		t.Fatalf("injected = true for a body that already carries the key")
	}
	if string(out) != body {
		t.Fatalf("body was re-serialized: %s", out)
	}
}

// The same rule with the opposite value, which is the one that can regress on its
// own: llama.cpp honours an explicit false exactly as it honours an absent key, so
// overwriting it would silently reverse a client's choice. Asserting only that the
// key is still PRESENT would pass against a helper that overwrote false with true —
// hence the assertion on the VALUE.
func TestInjectTimingsPerTokenHonoursAnExplicitClientFalse(t *testing.T) {
	const body = `{"model":"gw","stream":true,"timings_per_token":false}`

	out, injected := injectTimingsPerToken([]byte(body))

	// The value is asserted FIRST, deliberately: it is the assertion that states
	// D5's rule, and a helper that tested truth instead of presence would trip it.
	m := decodeInjected(t, out)
	if m[timingsPerTokenKey] != false {
		t.Fatalf("%s = %#v, want the client's false to survive", timingsPerTokenKey, m[timingsPerTokenKey])
	}
	if injected {
		t.Fatalf("injected = true over a client's explicit false")
	}
	if string(out) != body {
		t.Fatalf("body was re-serialized: %s", out)
	}
}

// Anything that is not a JSON object is returned untouched.
func TestInjectTimingsPerTokenLeavesNonObjectBodiesAlone(t *testing.T) {
	for _, body := range []string{`["a","b"]`, `"str"`, `7`, ``} {
		out, injected := injectTimingsPerToken([]byte(body))
		if injected || string(out) != body {
			t.Fatalf("body %q: injected=%v out=%s, want the body unchanged", body, injected, out)
		}
	}
}

// A bare null decodes into a map WITHOUT an error and leaves that map nil;
// assigning into it panics with "assignment to entry in nil map". Nothing on the
// request path can reach the helper with such a body today — sniffRoutingModel
// yields an empty model for null and handleOpenAIResponses only calls
// tryProxyNative for a non-empty one — but that guard lives in
// inference_handlers.go, a different file, so the helper carries its own.
func TestInjectTimingsPerTokenDoesNotPanicOnANullBody(t *testing.T) {
	const body = `null`

	out, injected := injectTimingsPerToken([]byte(body))

	if injected || string(out) != body {
		t.Fatalf("null: injected=%v out=%s, want the body unchanged", injected, out)
	}
}
