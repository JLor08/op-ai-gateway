package agentkey

import (
	"bytes"
	"encoding/hex"
	"strings"
	"testing"
)

const fixture = "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"

func TestParseRejectsBadKeys(t *testing.T) {
	k, err := Parse("")
	if k != nil || err != nil {
		t.Fatalf("Parse(\"\") = %v, %v; want nil, nil", k, err)
	}
	for name, in := range map[string]string{
		"63 chars": fixture[:63],
		"65 chars": fixture + "0",
		"non-hex":  strings.Repeat("zz", 32),
	} {
		if k, err := Parse(in); err == nil || k != nil {
			t.Errorf("%s: Parse = %v, %v; want an error", name, k, err)
		}
	}
	if k, err := Parse(fixture); err != nil || k == nil {
		t.Fatalf("Parse(fixture) = %v, %v", k, err)
	}
}

func TestSubkeyIsPerPurposeAndDeterministic(t *testing.T) {
	k, err := Parse(fixture)
	if err != nil {
		t.Fatal(err)
	}
	a := k.Subkey(PurposeInterfaceID)
	if len(a) != 32 {
		t.Fatalf("len = %d, want 32", len(a))
	}
	if !bytes.Equal(a, k.Subkey(PurposeInterfaceID)) {
		t.Error("same purpose must give equal bytes")
	}
	if bytes.Equal(a, k.Subkey(PurposeRuntimeCache)) {
		t.Error("different purposes must give different bytes")
	}
	var nilKey *Key
	if got := nilKey.Subkey(PurposeInterfaceID); got != nil {
		t.Errorf("nil key Subkey = %x, want nil", got)
	}
}

// TestSubkeyGoldenValues pins the derivation itself: the gateway persists
// interface ids built from the interface-id subkey, and the runtime cache is
// sealed under the runtime-cache subkey, so a change to the HKDF salt, info
// or hash would silently orphan every stored interface choice and every
// sealed cache entry. The expected values were computed independently
// (RFC 5869 HKDF-SHA256, empty salt, info = purpose, 32 bytes) for the
// fixture key 00..1f.
func TestSubkeyGoldenValues(t *testing.T) {
	k, err := Parse(fixture)
	if err != nil {
		t.Fatalf("Parse(fixture): %v", err)
	}
	for purpose, want := range map[string]string{
		PurposeInterfaceID:  "2b669919beafc390986fe564950da64f5a818f46ae9a2b0f6cc2e2876c338f4c",
		PurposeRuntimeCache: "3a4f2e0ef218053444b89bc6958bd2cb41e7f5c8b98796e7ff8116e3001e0901",
	} {
		if got := hex.EncodeToString(k.Subkey(purpose)); got != want {
			t.Errorf("Subkey(%q) = %s, want %s", purpose, got, want)
		}
	}
}
