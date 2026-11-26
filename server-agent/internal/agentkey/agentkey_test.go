package agentkey

import (
	"bytes"
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
