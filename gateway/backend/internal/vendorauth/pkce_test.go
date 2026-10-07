// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package vendorauth

import (
	"crypto/sha256"
	"encoding/base64"
	"regexp"
	"testing"
)

var base64URLNoPad = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

func TestGeneratePKCEChallengeIsS256OfVerifier(t *testing.T) {
	verifier, challenge, err := GeneratePKCE()
	if err != nil {
		t.Fatalf("GeneratePKCE: %v", err)
	}
	// 32 random bytes → 43 base64url characters without padding (RFC 7636 §4.1
	// allows 43-128).
	if len(verifier) != 43 || !base64URLNoPad.MatchString(verifier) {
		t.Fatalf("verifier %q is not 43 base64url-no-pad characters", verifier)
	}
	sum := sha256.Sum256([]byte(verifier))
	want := base64.RawURLEncoding.EncodeToString(sum[:])
	if challenge != want {
		t.Fatalf("challenge = %q, want S256(verifier) = %q", challenge, want)
	}
	if !base64URLNoPad.MatchString(challenge) {
		t.Fatalf("challenge %q is not base64url-no-pad", challenge)
	}
}

func TestGeneratePKCEIsRandom(t *testing.T) {
	v1, _, err1 := GeneratePKCE()
	v2, _, err2 := GeneratePKCE()
	if err1 != nil || err2 != nil {
		t.Fatalf("GeneratePKCE errors: %v, %v", err1, err2)
	}
	if v1 == v2 {
		t.Fatalf("two verifiers collided: %q", v1)
	}
}

// TestPKCEChallengeRFC7636Vector pins the S256 transform against the worked
// example in RFC 7636 Appendix B, so the derivation cannot drift unnoticed.
func TestPKCEChallengeRFC7636Vector(t *testing.T) {
	const verifier = "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"
	const want = "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM"
	if got := PKCEChallenge(verifier); got != want {
		t.Fatalf("PKCEChallenge = %q, want %q", got, want)
	}
}

func TestRandomState(t *testing.T) {
	s1, err := RandomState()
	if err != nil {
		t.Fatalf("RandomState: %v", err)
	}
	s2, err := RandomState()
	if err != nil {
		t.Fatalf("RandomState: %v", err)
	}
	if len(s1) != 43 || !base64URLNoPad.MatchString(s1) {
		t.Fatalf("state %q is not 43 base64url-no-pad characters", s1)
	}
	if s1 == s2 {
		t.Fatalf("two states collided: %q", s1)
	}
}
