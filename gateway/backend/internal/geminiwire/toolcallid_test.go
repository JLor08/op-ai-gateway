package geminiwire

import (
	"encoding/base64"
	"testing"
)

func TestToolCallIDRoundTrip(t *testing.T) {
	cases := []struct{ name, sig string }{
		{"get_weather", "sig-abc=="},
		{"f", ""},
		{"", "onlysig"},
		{"name/with:odd.chars", "a+b/c=="},
		// The empty encoding ({}) is legitimate and distinct from a JSON null payload.
		{"", ""},
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
	for _, id := range []string{
		"",
		"call_123",
		"toolu_abc",
		"gcw1:not-base64!!",
		// Ours by prefix, but the payload is not a valid encoding.
		"gcw1:",
		"gcw1:" + base64.RawURLEncoding.EncodeToString([]byte("notjson")),
		// A JSON null payload must not decode as the legitimate empty encoding.
		"gcw1:" + base64.RawURLEncoding.EncodeToString([]byte("null")),
	} {
		if _, _, ok := decodeToolCallID(id); ok {
			t.Fatalf("decodeToolCallID(%q) ok=true, want false", id)
		}
	}
}

// TestEncodeToolCallIDFormat pins the wire format: minted ids persist in client
// conversation histories, so the scheme is a contract.
func TestEncodeToolCallIDFormat(t *testing.T) {
	want := "gcw1:" + base64.RawURLEncoding.EncodeToString([]byte(`{"n":"f"}`))
	if got := encodeToolCallID("f", ""); got != want {
		t.Fatalf("format = %q, want %q", got, want)
	}
}
