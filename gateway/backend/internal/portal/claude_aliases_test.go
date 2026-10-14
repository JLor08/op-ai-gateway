// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
	"testing"
)

// None of these tests call t.Parallel(): several of them temporarily swap the
// package-level claudeBlockedSubstrings denylist (restored via t.Cleanup), which
// is only safe while the tests in this file run one at a time.

const claudeAliasPrefix = "anthropic-local-"

// Fixture ids found offline (see the comments at each use). They are plain
// strings, so the tests stay a pure unit with no model views or Service.
const (
	// SHA-256 = dc2c13fababa5cebd14fe56d692835c79c7359ed2941ebd13a4c6b6cf4bb508e.
	// "abab" starts at hex offset 7, so windows 0..7 all contain it and the first
	// clean 12-hex window starts at offset 8: "baba5cebd14f".
	claudeFallbackFixtureID    = "fallback-fixture-26142"
	claudeFallbackFixtureAlias = claudeAliasPrefix + "baba5cebd14f"

	// A genuine 48-bit SHA-256 prefix collision (found with a birthday search):
	// both digests start with 169d946becb6 (no denylisted substring), so both ids
	// derive the very same alias id with the production denylist.
	claudeCollideA     = "collide-1736521"
	claudeCollideB     = "collide-9235547"
	claudeCollideAlias = claudeAliasPrefix + "169d946becb6"

	// A second, independent colliding pair (prefix 4f7093c62e7c).
	claudeCollideC      = "collide-2455599"
	claudeCollideD      = "collide-9409470"
	claudeCollideAlias2 = claudeAliasPrefix + "4f7093c62e7c"
)

// swapClaudeBlockedSubstrings replaces the denylist for one test and restores
// it afterwards.
func swapClaudeBlockedSubstrings(t *testing.T, list []string) {
	t.Helper()
	orig := claudeBlockedSubstrings
	claudeBlockedSubstrings = list
	t.Cleanup(func() { claudeBlockedSubstrings = orig })
}

func claudeAliasHashPart(t *testing.T, id string) string {
	t.Helper()
	if !strings.HasPrefix(id, claudeAliasPrefix) {
		t.Fatalf("alias id %q does not start with %q", id, claudeAliasPrefix)
	}
	return strings.TrimPrefix(id, claudeAliasPrefix)
}

func isLowerHex(s string) bool {
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

func claudeDisplayNameFor(id string) string { return "Local " + id }

func TestClaudeAliasIDCanonicalHash(t *testing.T) {
	// The documented example of issue #225: pins SHA-256 over the exact UTF-8
	// gateway id, the first 12 lowercase hex characters, and the prefix.
	id, ok := claudeAliasID("qwen3.8-fast-loaded")
	if !ok || id != "anthropic-local-556f772a70b1" {
		t.Fatalf("claudeAliasID(qwen3.8-fast-loaded) = %q, %v; want anthropic-local-556f772a70b1, true", id, ok)
	}
}

func TestClaudeAliasIDHashesTheUTF8Bytes(t *testing.T) {
	// A non-ASCII gateway id must hash its precomposed (NFC) UTF-8 bytes, with no
	// rune or Unicode normalisation step. Written with escapes so editors cannot
	// change the normalisation form. The literal is
	// `printf 'modèle-ünï-7b' | shasum -a 256` = 730d576cc6937f83..., whose first
	// window is clean under the production denylist.
	const gatewayID = "modèle-ünï-7b"
	id, ok := claudeAliasID(gatewayID)
	if !ok || id != "anthropic-local-730d576cc693" {
		t.Fatalf("claudeAliasID(%q) = %q, %v; want anthropic-local-730d576cc693, true", gatewayID, id, ok)
	}
}

func TestClaudeAliasIDHexFallbackSlidesTheWindow(t *testing.T) {
	// Guard the premise: the first 12-hex window really contains a denylisted
	// substring ("abab"), and "abab" is on the production denylist.
	sum := sha256.Sum256([]byte(claudeFallbackFixtureID))
	digest := hex.EncodeToString(sum[:])
	if !strings.Contains(digest[:12], "abab") {
		t.Fatalf("fixture premise broken: first window %q has no abab", digest[:12])
	}
	if !slices.Contains(claudeBlockedSubstrings, "abab") {
		t.Fatalf("fixture premise broken: abab missing from claudeBlockedSubstrings")
	}

	id, ok := claudeAliasID(claudeFallbackFixtureID)
	if !ok {
		t.Fatalf("claudeAliasID(%q) ok = false", claudeFallbackFixtureID)
	}
	// The pinned literal is the contract: changing the fallback encoding would
	// rename existing aliases.
	if id != claudeFallbackFixtureAlias {
		t.Fatalf("claudeAliasID(%q) = %q, want %q", claudeFallbackFixtureID, id, claudeFallbackFixtureAlias)
	}

	// Invariants: still prefix + 12 lowercase hex chars, a contiguous window of
	// the digest (the fallback never leaves the hex alphabet), no denylisted
	// substring anywhere in the id.
	hash := claudeAliasHashPart(t, id)
	if len(hash) != 12 || !isLowerHex(hash) {
		t.Fatalf("hash part %q is not 12 lowercase hex chars", hash)
	}
	if !strings.Contains(digest, hash) {
		t.Fatalf("hash part %q is not a window of the digest %s", hash, digest)
	}
	for _, blocked := range claudeBlockedSubstrings {
		if strings.Contains(id, blocked) {
			t.Fatalf("alias id %q contains denylisted %q", id, blocked)
		}
	}

	// Stability: the same id always yields the same alias.
	for i := 0; i < 3; i++ {
		again, ok := claudeAliasID(claudeFallbackFixtureID)
		if !ok || again != id {
			t.Fatalf("call %d: got %q, %v; want %q, true", i, again, ok, id)
		}
	}
}

func TestClaudeAliasIDFirstWindowKeptWhenClean(t *testing.T) {
	// No slide when the offset-0 window is already clean: the alias is exactly the
	// first 12 hex characters (the canonical example above is such a case; this
	// pins it against a custom denylist that does not match it).
	swapClaudeBlockedSubstrings(t, []string{"zzzz"})
	id, ok := claudeAliasID("qwen3.8-fast-loaded")
	if !ok || id != "anthropic-local-556f772a70b1" {
		t.Fatalf("got %q, %v", id, ok)
	}
}

func TestClaudeAliasIDSlidesPastEveryBlockedWindow(t *testing.T) {
	// With "556f" blocked, the canonical id's offset-0 window is rejected; the
	// digest 556f772a70b1bea2e56c... slides one char at a time until a window has
	// no "556f": offset 1 = "56f772a70b1b" contains no "556f".
	swapClaudeBlockedSubstrings(t, []string{"556f"})
	id, ok := claudeAliasID("qwen3.8-fast-loaded")
	if !ok || id != "anthropic-local-56f772a70b1b" {
		t.Fatalf("got %q, %v; want anthropic-local-56f772a70b1b, true", id, ok)
	}
}

func TestClaudeAliasIDNoCleanWindowIsNotOK(t *testing.T) {
	// Blocking every hex digit makes every window unsafe: the astronomically
	// unlikely "no clean window" branch reports ok=false so the caller skips.
	swapClaudeBlockedSubstrings(t, strings.Split("0123456789abcdef", ""))
	if id, ok := claudeAliasID("qwen3.8-fast-loaded"); ok || id != "" {
		t.Fatalf("got %q, %v; want empty id and ok=false", id, ok)
	}
}

func TestClaudeAliasIDStableAndDistinct(t *testing.T) {
	a1, ok1 := claudeAliasID("gpt-oss-20b")
	a2, ok2 := claudeAliasID("gpt-oss-20b")
	if !ok1 || !ok2 || a1 != a2 {
		t.Fatalf("same id gave %q/%v then %q/%v", a1, ok1, a2, ok2)
	}
	b, okb := claudeAliasID("gpt-oss-20c")
	if !okb || a1 == b {
		t.Fatalf("a one-char different id must give a different alias: %q vs %q (ok=%v)", a1, b, okb)
	}
	// Case matters: the gateway id is hashed byte-for-byte.
	c, okc := claudeAliasID("GPT-OSS-20B")
	if !okc || a1 == c {
		t.Fatalf("a case-different id must give a different alias: %q vs %q (ok=%v)", a1, c, okc)
	}
}

func TestClaudeAliasIDInvariantsAcrossManyIDs(t *testing.T) {
	// Whatever the digest looks like, an alias is the fixed prefix plus 12
	// lowercase hex chars with no denylisted substring, and it is deterministic.
	for n := 0; n < 20000; n++ {
		gatewayID := fmt.Sprintf("local-model-%d", n)
		id, ok := claudeAliasID(gatewayID)
		if !ok {
			t.Fatalf("claudeAliasID(%q) ok = false", gatewayID)
		}
		hash := claudeAliasHashPart(t, id)
		if len(hash) != 12 || !isLowerHex(hash) {
			t.Fatalf("claudeAliasID(%q) = %q: hash part is not 12 lowercase hex", gatewayID, id)
		}
		for _, blocked := range claudeBlockedSubstrings {
			if strings.Contains(id, blocked) {
				t.Fatalf("claudeAliasID(%q) = %q contains denylisted %q", gatewayID, id, blocked)
			}
		}
		if again, _ := claudeAliasID(gatewayID); again != id {
			t.Fatalf("claudeAliasID(%q) unstable: %q then %q", gatewayID, id, again)
		}
	}
}

func TestClaudeBlockedSubstringsAreDocumentedLowercase(t *testing.T) {
	if len(claudeBlockedSubstrings) == 0 {
		t.Fatal("claudeBlockedSubstrings must not be empty")
	}
	seen := map[string]bool{}
	for _, blocked := range claudeBlockedSubstrings {
		if blocked == "" {
			t.Fatal("claudeBlockedSubstrings contains an empty entry (it would block every alias)")
		}
		if blocked != strings.ToLower(blocked) {
			t.Fatalf("denylist entry %q is not lowercase", blocked)
		}
		if seen[blocked] {
			t.Fatalf("denylist entry %q is duplicated", blocked)
		}
		seen[blocked] = true
		// A denylisted word inside the fixed prefix would reject every alias.
		if strings.Contains(claudeAliasPrefix, blocked) {
			t.Fatalf("denylist entry %q matches the fixed %q prefix", blocked, claudeAliasPrefix)
		}
	}
}

func TestClaudeAliasMapBuildsAliasForEachEligibleID(t *testing.T) {
	eligible := []string{"qwen3.8-fast-loaded", "gpt-oss-20b"}
	got, diags := claudeAliasMap(eligible, nil, claudeDisplayNameFor)
	if len(diags) != 0 {
		t.Fatalf("unexpected diagnostics: %v", diags)
	}
	if len(got) != 2 {
		t.Fatalf("got %d aliases, want 2: %+v", len(got), got)
	}
	qwen := got["anthropic-local-556f772a70b1"]
	if qwen.OriginalID != "qwen3.8-fast-loaded" {
		t.Fatalf("qwen alias = %+v", qwen)
	}
	for _, id := range eligible {
		aliasID, ok := claudeAliasID(id)
		if !ok {
			t.Fatalf("claudeAliasID(%q) not ok", id)
		}
		alias, found := got[aliasID]
		if !found || alias.OriginalID != id {
			t.Fatalf("alias %q = %+v, found=%v; want OriginalID %q", aliasID, alias, found, id)
		}
	}
}

func TestClaudeAliasMapDisplayNameFromCallback(t *testing.T) {
	var asked []string
	displayName := func(id string) string {
		asked = append(asked, id)
		return "Label for " + id
	}
	got, _ := claudeAliasMap([]string{"qwen3.8-fast-loaded", "gpt-oss-20b"}, nil, displayName)
	for aliasID, alias := range got {
		if want := "Label for " + alias.OriginalID; alias.DisplayName != want {
			t.Fatalf("alias %q DisplayName = %q, want %q", aliasID, alias.DisplayName, want)
		}
	}
	slices.Sort(asked)
	if want := []string{"gpt-oss-20b", "qwen3.8-fast-loaded"}; !slices.Equal(asked, want) {
		t.Fatalf("displayName asked for %v, want %v", asked, want)
	}
}

func TestClaudeAliasMapDisplayNameFallsBackToGatewayID(t *testing.T) {
	// "target label if present, else the gateway id": an empty label (or no
	// callback at all) never produces a blank display name.
	empty := func(string) string { return "" }
	for name, fn := range map[string]func(string) string{"empty label": empty, "nil callback": nil} {
		got, _ := claudeAliasMap([]string{"qwen3.8-fast-loaded"}, nil, fn)
		alias := got["anthropic-local-556f772a70b1"]
		if alias.OriginalID != "qwen3.8-fast-loaded" || alias.DisplayName != "qwen3.8-fast-loaded" {
			t.Fatalf("%s: alias = %+v, want display name = gateway id", name, alias)
		}
	}
}

func TestClaudeAliasMapUsesFallbackWindowID(t *testing.T) {
	got, diags := claudeAliasMap([]string{claudeFallbackFixtureID}, nil, claudeDisplayNameFor)
	if len(diags) != 0 {
		t.Fatalf("unexpected diagnostics: %v", diags)
	}
	alias, ok := got[claudeFallbackFixtureAlias]
	if !ok || alias.OriginalID != claudeFallbackFixtureID {
		t.Fatalf("map = %+v, want %q -> %q", got, claudeFallbackFixtureAlias, claudeFallbackFixtureID)
	}
}

func TestClaudeAliasMapTwoIDsSameAliasSkipsBoth(t *testing.T) {
	// Premise: the two fixture ids are a real hash-prefix collision, not a test seam.
	idA, okA := claudeAliasID(claudeCollideA)
	idB, okB := claudeAliasID(claudeCollideB)
	if !okA || !okB || idA != claudeCollideAlias || idB != claudeCollideAlias {
		t.Fatalf("fixture premise broken: %q/%v and %q/%v, want both %q", idA, okA, idB, okB, claudeCollideAlias)
	}

	// An unrelated id keeps its alias; the colliding pair is dropped entirely.
	got, diags := claudeAliasMap([]string{claudeCollideA, "gpt-oss-20b", claudeCollideB}, nil, claudeDisplayNameFor)
	if _, present := got[claudeCollideAlias]; present {
		t.Fatalf("colliding alias %q must be skipped, got %+v", claudeCollideAlias, got[claudeCollideAlias])
	}
	if len(got) != 1 {
		t.Fatalf("only the non-colliding id keeps an alias, got %+v", got)
	}
	for _, alias := range got {
		if alias.OriginalID != "gpt-oss-20b" {
			t.Fatalf("surviving alias targets %q, want gpt-oss-20b", alias.OriginalID)
		}
	}
	if len(diags) != 2 {
		t.Fatalf("want one diagnostic per skipped id (2), got %d: %v", len(diags), diags)
	}
	for _, id := range []string{claudeCollideA, claudeCollideB} {
		if !diagsMention(diags, id) {
			t.Fatalf("no diagnostic mentions %q: %v", id, diags)
		}
	}
	for _, d := range diags {
		if !strings.Contains(d, claudeCollideAlias) {
			t.Fatalf("diagnostic %q does not name the alias id %q", d, claudeCollideAlias)
		}
	}
}

func TestClaudeAliasMapCollisionIsOrderIndependent(t *testing.T) {
	gotAB, diagsAB := claudeAliasMap([]string{claudeCollideA, claudeCollideB, "gpt-oss-20b"}, nil, claudeDisplayNameFor)
	gotBA, diagsBA := claudeAliasMap([]string{"gpt-oss-20b", claudeCollideB, claudeCollideA}, nil, claudeDisplayNameFor)
	if len(gotAB) != len(gotBA) {
		t.Fatalf("map size depends on input order: %v vs %v", gotAB, gotBA)
	}
	for aliasID, alias := range gotAB {
		if gotBA[aliasID] != alias {
			t.Fatalf("alias %q differs by input order: %+v vs %+v", aliasID, alias, gotBA[aliasID])
		}
	}
	if !slices.Equal(diagsAB, diagsBA) {
		t.Fatalf("diagnostics depend on input order:\n%v\n%v", diagsAB, diagsBA)
	}
}

func TestClaudeAliasMapIndependentCollisionGroups(t *testing.T) {
	idC, okC := claudeAliasID(claudeCollideC)
	idD, okD := claudeAliasID(claudeCollideD)
	if !okC || !okD || idC != claudeCollideAlias2 || idD != claudeCollideAlias2 {
		t.Fatalf("fixture premise broken: %q/%v and %q/%v, want both %q", idC, okC, idD, okD, claudeCollideAlias2)
	}
	eligible := []string{claudeCollideA, claudeCollideB, claudeCollideC, claudeCollideD, "qwen3.8-fast-loaded"}
	got, diags := claudeAliasMap(eligible, nil, claudeDisplayNameFor)
	if len(got) != 1 || got["anthropic-local-556f772a70b1"].OriginalID != "qwen3.8-fast-loaded" {
		t.Fatalf("only the unique id keeps an alias, got %+v", got)
	}
	if len(diags) != 4 {
		t.Fatalf("want 4 diagnostics (one per skipped id), got %d: %v", len(diags), diags)
	}
}

func TestClaudeAliasMapDuplicateEligibleIDIsNotACollision(t *testing.T) {
	// The same gateway id listed twice is one identity, not a collision.
	got, diags := claudeAliasMap([]string{"qwen3.8-fast-loaded", "qwen3.8-fast-loaded"}, nil, claudeDisplayNameFor)
	if len(diags) != 0 {
		t.Fatalf("unexpected diagnostics: %v", diags)
	}
	if alias := got["anthropic-local-556f772a70b1"]; alias.OriginalID != "qwen3.8-fast-loaded" {
		t.Fatalf("got %+v", got)
	}
}

func TestClaudeAliasMapReservedAliasIsSkipped(t *testing.T) {
	// A real model/group name or a manual model_override_map key equal to the
	// generated alias id wins: the generated alias is skipped with a diagnostic
	// and no entry (so no auto metadata) is published for it.
	reserved := map[string]bool{"anthropic-local-556f772a70b1": true}
	got, diags := claudeAliasMap([]string{"qwen3.8-fast-loaded", "gpt-oss-20b"}, reserved, claudeDisplayNameFor)
	if _, present := got["anthropic-local-556f772a70b1"]; present {
		t.Fatalf("reserved alias id must not be generated: %+v", got)
	}
	if len(got) != 1 {
		t.Fatalf("only the non-reserved id keeps an alias, got %+v", got)
	}
	for _, alias := range got {
		if alias.OriginalID != "gpt-oss-20b" {
			t.Fatalf("surviving alias targets %q, want gpt-oss-20b", alias.OriginalID)
		}
	}
	if len(diags) != 1 {
		t.Fatalf("want exactly 1 diagnostic, got %d: %v", len(diags), diags)
	}
	if !strings.Contains(diags[0], "qwen3.8-fast-loaded") || !strings.Contains(diags[0], "anthropic-local-556f772a70b1") {
		t.Fatalf("diagnostic %q should name the gateway id and the alias id", diags[0])
	}
}

func TestClaudeAliasMapReservedIgnoresUnrelatedNames(t *testing.T) {
	// Reserved names that are not alias ids (the usual case: real model names)
	// leave every alias in place.
	reserved := map[string]bool{"qwen3.8-fast-loaded": true, "gpt-oss-20b": true, "manual-rule-key": true}
	got, diags := claudeAliasMap([]string{"qwen3.8-fast-loaded", "gpt-oss-20b"}, reserved, claudeDisplayNameFor)
	if len(got) != 2 || len(diags) != 0 {
		t.Fatalf("got %+v diags %v; want 2 aliases and no diagnostics", got, diags)
	}
}

func TestClaudeAliasMapReservedWinsOverCollision(t *testing.T) {
	// A colliding pair whose shared alias id is also reserved is skipped once per
	// id (reserved takes precedence), never double-reported.
	reserved := map[string]bool{claudeCollideAlias: true}
	got, diags := claudeAliasMap([]string{claudeCollideA, claudeCollideB}, reserved, claudeDisplayNameFor)
	if len(got) != 0 {
		t.Fatalf("got %+v, want no aliases", got)
	}
	if len(diags) != 2 {
		t.Fatalf("want one diagnostic per skipped id, got %d: %v", len(diags), diags)
	}
}

func TestClaudeAliasMapNoCleanWindowIsSkippedWithDiagnostic(t *testing.T) {
	swapClaudeBlockedSubstrings(t, strings.Split("0123456789abcdef", ""))
	got, diags := claudeAliasMap([]string{"qwen3.8-fast-loaded"}, nil, claudeDisplayNameFor)
	if len(got) != 0 {
		t.Fatalf("got %+v, want no aliases", got)
	}
	if len(diags) != 1 || !strings.Contains(diags[0], "qwen3.8-fast-loaded") {
		t.Fatalf("want one diagnostic naming the id, got %v", diags)
	}
}

func TestClaudeAliasMapEmptyInput(t *testing.T) {
	got, diags := claudeAliasMap(nil, nil, claudeDisplayNameFor)
	if got == nil || len(got) != 0 || len(diags) != 0 {
		t.Fatalf("got %#v diags %v; want empty non-nil map and no diagnostics", got, diags)
	}
}

func diagsMention(diags []string, id string) bool {
	for _, d := range diags {
		if strings.Contains(d, id) {
			return true
		}
	}
	return false
}
