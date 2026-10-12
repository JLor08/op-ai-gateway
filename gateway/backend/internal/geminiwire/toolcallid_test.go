package geminiwire

import "testing"

func TestToolCallIDRoundTrip(t *testing.T) {
	cases := []struct{ name, sig string }{
		{"get_weather", "sig-abc=="},
		{"f", ""},
		{"", "onlysig"},
		{"name/with:odd.chars", "a+b/c=="},
	}
	for _, c := range cases {
		id := encodeToolCallID(c.name, c.sig)
		gotName, gotSig, ok := decodeToolCallID(id)
		if !ok {
			t.Fatalf("decode(%q) ok=false, want true", id)
		}
		if gotName != c.name || gotSig != c.sig {
			t.Fatalf("round-trip %q/%q -> %q/%q", c.name, c.sig, gotName, gotSig)
		}
	}
}

func TestDecodeToolCallIDForeign(t *testing.T) {
	// Ids from other dialects (Anthropic toolu_*, OpenAI call_*) are not ours.
	for _, id := range []string{"", "call_123", "toolu_abc", "gcw1:not-base64!!"} {
		if _, _, ok := decodeToolCallID(id); ok {
			t.Fatalf("decodeToolCallID(%q) ok=true, want false", id)
		}
	}
}
