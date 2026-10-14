// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
)

// Claude-compatible model aliases (issue #225): a token that opts in sees each
// eligible local model/group under a stable `anthropic-local-<hash>` id that a
// Claude client will accept. This file is the one shared derivation both
// discovery and resolution use, so they cannot disagree. It is a pure unit over
// plain strings: which models are eligible (offered, Anthropic-Messages
// capable, local, not hidden/locked/images-only) and which names are reserved is
// decided by the caller from the token's model views.
//
// The encoding is a compatibility contract: clients persist alias ids, so
// changing the prefix, the hash, the window length, the window order or the
// hex-expressible denylist entries would silently rename existing aliases.

const (
	// claudeAliasIDPrefix is the fixed, human-recognisable part of every alias id.
	claudeAliasIDPrefix = "anthropic-local-"
	// claudeAliasHashLen is the number of lowercase hex characters of the SHA-256
	// digest that make up the variable part of an alias id.
	claudeAliasHashLen = 12
)

// claudeAlias is one generated alias: the real gateway model/group id it
// resolves to and the honest label discovery shows for it.
type claudeAlias struct {
	// OriginalID is the real gateway model/group id this alias resolves to.
	OriginalID string
	// DisplayName is the honest label (the target's own label, never a
	// Claude-model name).
	DisplayName string
}

// claudeBlockedSubstrings is the denylist of model-family markers a Claude
// client may refuse inside a model id. All entries are lowercase. An alias id
// (prefix + hash window) never contains any of them. Only entries made of
// [0-9a-f] can ever occur in a hex digest, so only those can force the window to
// slide (today: "abab"); the rest are kept so the list stays a complete,
// documented record and a future filter or non-hex encoding cannot reintroduce
// them. Adding a hex-expressible entry renames the aliases whose current window
// contains it, so treat additions as a compatibility change.
var claudeBlockedSubstrings = []string{
	"abab",     // MiniMax abab family (the only marker expressible in hex)
	"qwen",     // Alibaba Qwen family
	"gpt",      // OpenAI GPT family (including gpt-oss)
	"llama",    // Meta Llama family
	"mistral",  // Mistral AI family
	"gemini",   // Google Gemini family
	"deepseek", // DeepSeek family
}

// claudeAliasIDBlocked reports whether an alias id contains a denylisted
// substring.
func claudeAliasIDBlocked(aliasID string) bool {
	for _, blocked := range claudeBlockedSubstrings {
		if strings.Contains(aliasID, blocked) {
			return true
		}
	}
	return false
}

// claudeAliasID returns the alias id for a gateway model/group id: the
// SHA-256 of the id's UTF-8 bytes, hex-encoded (64 lowercase characters), then
// claudeAliasIDPrefix plus the first claudeAliasHashLen-character window, taken
// at offset 0, 1, 2, ... of the digest, whose whole alias id contains no
// claudeBlockedSubstrings entry. The id stays hexadecimal even when it slides.
// ok is false when no window of the digest is safe (astronomically unlikely);
// the caller then skips the alias.
func claudeAliasID(gatewayID string) (id string, ok bool) {
	sum := sha256.Sum256([]byte(gatewayID))
	digest := hex.EncodeToString(sum[:])
	for off := 0; off+claudeAliasHashLen <= len(digest); off++ {
		candidate := claudeAliasIDPrefix + digest[off:off+claudeAliasHashLen]
		if !claudeAliasIDBlocked(candidate) {
			return candidate, true
		}
	}
	return "", false
}

// claudeAliasMap builds aliasID -> claudeAlias for the already-eligible
// gateway model/group ids. reserved holds the names an alias must not shadow:
// the real model/group names and the token's manual model_override_map keys.
// displayName returns the honest label for a gateway id; an empty label (or a
// nil callback) falls back to the gateway id itself. The second result is a
// list of human-readable diagnostics, one per skipped id, ordered by gateway id.
//
// The result does not depend on the order of eligible, and a repeated id is one
// identity. Collision policy:
//   - Distinct ids that derive the same alias id are all skipped, so one alias
//     is never routed to two targets.
//   - An alias id present in reserved is skipped: the real name or manual rule
//     wins, and no generated metadata is published for it. This takes precedence
//     over the collision check, so such an id is reported once.
//   - An id with no filter-safe window is skipped.
func claudeAliasMap(eligible []string, reserved map[string]bool, displayName func(string) string) (map[string]claudeAlias, []string) {
	ids := slices.Clone(eligible)
	slices.Sort(ids)
	ids = slices.Compact(ids)

	type derived struct {
		gatewayID string
		aliasID   string
		ok        bool
	}
	derivations := make([]derived, 0, len(ids))
	owners := make(map[string][]string, len(ids)) // alias id -> gateway ids, ascending
	for _, gatewayID := range ids {
		aliasID, ok := claudeAliasID(gatewayID)
		derivations = append(derivations, derived{gatewayID: gatewayID, aliasID: aliasID, ok: ok})
		if ok {
			owners[aliasID] = append(owners[aliasID], gatewayID)
		}
	}

	aliases := make(map[string]claudeAlias, len(ids))
	var diagnostics []string
	for _, d := range derivations {
		switch {
		case !d.ok:
			diagnostics = append(diagnostics, fmt.Sprintf(
				"claude alias for %q skipped: no filter-safe %d-hex window in its SHA-256 digest",
				d.gatewayID, claudeAliasHashLen))
		case reserved[d.aliasID]:
			diagnostics = append(diagnostics, fmt.Sprintf(
				"claude alias %q for %q skipped: it equals an existing model, group or manual override name",
				d.aliasID, d.gatewayID))
		case len(owners[d.aliasID]) > 1:
			var others []string
			for _, other := range owners[d.aliasID] {
				if other != d.gatewayID {
					others = append(others, fmt.Sprintf("%q", other))
				}
			}
			diagnostics = append(diagnostics, fmt.Sprintf(
				"claude alias %q for %q skipped: it is also derived from %s, so none of them gets it",
				d.aliasID, d.gatewayID, strings.Join(others, ", ")))
		default:
			label := ""
			if displayName != nil {
				label = displayName(d.gatewayID)
			}
			if strings.TrimSpace(label) == "" {
				label = d.gatewayID
			}
			aliases[d.aliasID] = claudeAlias{OriginalID: d.gatewayID, DisplayName: label}
		}
	}
	return aliases, diagnostics
}
