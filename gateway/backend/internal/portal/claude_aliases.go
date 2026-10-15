// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/routing"
	"slices"
	"strings"
)

// Claude-compatible model aliases (issue #225): a token that opts in sees each
// eligible local model/group under a stable `anthropic-local-<hash>` id that a
// Claude client will accept. This file is the one shared derivation both
// discovery and resolution use, so they cannot disagree. It has three layers:
// the pure encoding and collision policy over plain strings (claudeAliasID,
// claudeAliasMap), the eligibility rules over a token's alias-free listing
// (claudeAliasEligible), and the derivation that applies them. That derivation
// is claudeAliasesFromListing, a plain function over the listing and the vendor
// model set its caller already holds (the discovery paths, which read the vendor
// set once for it and for the vendor overlay). The token-scoped Service entry
// points are claudeAliasesForToken, which reads what it needs from the token
// itself, and ClaudeAliasTarget, which inference resolution calls through it.
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
			diagnostics = append(diagnostics, claudeAliasCollisionDiagnostic(d.aliasID, d.gatewayID, owners[d.aliasID]))
		default:
			aliases[d.aliasID] = claudeAlias{OriginalID: d.gatewayID, DisplayName: claudeAliasLabel(d.gatewayID, displayName)}
		}
	}
	return aliases, diagnostics
}

// claudeAliasCollisionDiagnostic is the diagnostic for a gateway id skipped
// because other ids derive the same alias id. owners is every gateway id that
// derives aliasID (ascending, including gatewayID itself); the message names
// the others, in that order.
func claudeAliasCollisionDiagnostic(aliasID, gatewayID string, owners []string) string {
	var others []string
	for _, other := range owners {
		if other != gatewayID {
			others = append(others, fmt.Sprintf("%q", other))
		}
	}
	return fmt.Sprintf(
		"claude alias %q for %q skipped: it is also derived from %s, so none of them gets it",
		aliasID, gatewayID, strings.Join(others, ", "))
}

// claudeAliasLabel is the honest label of an alias onto gatewayID: what
// displayName returns for it, or the gateway id itself when displayName is nil
// or its label is blank.
func claudeAliasLabel(gatewayID string, displayName func(string) string) string {
	if displayName == nil {
		return gatewayID
	}
	if label := displayName(gatewayID); strings.TrimSpace(label) != "" {
		return label
	}
	return gatewayID
}

// claudeAliasEligible returns, sorted, the gateway model/group ids of a token's
// ALIAS-FREE listing that may receive a generated alias.
//
// listed is the per-name flavor map of the listing after the group overlay and
// the model_settings suppression but BEFORE any per-token alias overlay, so it
// holds only real model/group identities that are offered to this token:
// hidden, locked and suppressed names are already gone, and so is every group
// that is hidden/locked or has no offerable member. A name qualifies when it
//
//   - serves the Anthropic Messages flavor: "anthropic" is among its flavors,
//     which is already routing.MappingServesAPIFlavor (the application declares
//     the flavor, the effective flavors list it and the messages mode is not
//     disabled). That alone drops images-only, openai-only and Messages-disabled
//     models;
//   - is not a name the token's own manual rules govern: a name a rule is keyed
//     on means whatever that rule says for this token, so an alias onto the
//     unrewritten model would disagree with it, and a target a rule hides
//     (hide_target) is deliberately out of this token's discovery, which an
//     alias (original_model_id) would reveal;
//   - is not a name one of the token's vendor accounts also serves. Vendor models
//     are never in listed (they are no mapping views and the vendor overlay is
//     applied after it), but a vendor account is tried before the self-hosted
//     route (vendor_first), so an alias onto a name the vendor serves would
//     dispatch to the vendor model.
//
// "Local" is not decided here by a label: it holds because listed is composed
// only of mapping views (an AI server's application, which can only be one of
// the on-prem application types) and model groups over them, never of vendor
// accounts. See claudeAliasesFromListing.
func claudeAliasEligible(listed map[string]map[string]struct{}, rules map[string]auth.ModelOverrideRule, vendor map[string]map[string]struct{}) []string {
	hidden := make(map[string]struct{})
	for _, rule := range rules {
		if rule.HideTarget {
			hidden[rule.To] = struct{}{}
		}
	}
	eligible := make([]string, 0, len(listed))
	for name, flavors := range listed {
		if _, serves := flavors[routing.APIFlavorAnthropic]; !serves {
			continue
		}
		if _, keyed := rules[name]; keyed {
			continue
		}
		if _, hides := hidden[name]; hides {
			continue
		}
		// Unconditional of vendorMode: a name a vendor account also serves is dropped
		// even in fallback_only, where it would resolve locally. That is the safe
		// direction: vendorMode is runtime-configurable and could flip to
		// vendor_first between discovery and a later resolution, when an offered
		// alias would route to the vendor.
		if _, vendorServed := vendor[name]; vendorServed {
			continue
		}
		eligible = append(eligible, name)
	}
	slices.Sort(eligible)
	return eligible
}

// listingLabel is the display name a listing row carries for a gateway
// model/group id: the id itself. A model has no display name of its own and the
// listing labels a group by its gateway id too. A generated alias takes the same
// label as the row it points to, so the two never disagree and the alias label is
// never anything but the target's own.
func listingLabel(id string) string { return id }

// claudeAliasesFromListing is THE token-scoped derivation of generated Claude
// aliases (aliasID -> target and honest label), shared by both discovery paths
// (modelsResponse and modelFlavorSetsWithPreSuppress, which hold the listing in
// hand) and, through claudeAliasesForToken, by inference resolution, so a listed
// alias and a resolvable alias cannot disagree.
//
// listed is the token's alias-free listing (see claudeAliasEligible) and
// preSuppress the same composition before the hidden/locked suppression, with
// every active group. overlayLoaded says whether the group/visibility overlay
// that produced listed was available: when it was not, the listing fails OPEN
// (no groups, no hidden/locked suppression) so it never blanks, and the
// derivation fails CLOSED instead: it cannot tell a hidden model from a shown
// one, and an alias would reveal what the operator hid.
//
// Nothing is derived unless token.ClaudeModelAliases is set. That is the whole
// gate for the token-less portal session principal (gateway/auth.go
// sessionPrincipal), which never sets it: generated aliases serve external
// bearer-token clients such as Claude Desktop, not the portal UI.
//
// The names an alias must never take (reserved) are every real model/group name
// of this token's view (preSuppress, hidden ones included), the names of the
// token's vendor accounts, and its manual rule keys.
//
// vendor is the token's vendor model set (vendorModelFlavorSets), read by the
// CALLER, not here: the discovery paths already need the same set for the vendor
// overlay and read it once for both (overlayVendorModels), while
// claudeAliasesForToken, which has no overlay, reads it itself. It is only read
// here; a nil vendor (flag off, service token, no vendor access) means the
// token has none.
//
// vendorLoaded is that read's second result: whether vendor is the COMPLETE set.
// It is true for a nil vendor that merely means "no vendor access", and false
// when a vendor ACCOUNT or MODEL read failed (VendorAccountsByOwner, or an
// account's VendorAccountModels: vendor is then nil or partial). The
// vendor-shadow exclusion (here, and in claudeAliasEligible) and the reserved
// names need the whole set, so such a read error fails the derivation CLOSED,
// exactly like !overlayLoaded: a local name the owner's own vendor account also
// serves must not be offered, or resolved, as an alias while the set is unknown
// (the resolver would send it to the vendor first). The vendor LISTING overlay
// stays fail-open: it uses the nil or partial set as it is, so the real listing
// still renders. The vendor_accounts_enabled master-flag read is not part of
// this: an unreadable flag reads as "off" (no vendor access), so it stays
// fail-open here, as it does in the resolver.
//
// The second result is one human-readable diagnostic per skipped id; callers log
// them with logClaudeAliasDiagnostics.
func claudeAliasesFromListing(token auth.Token, listed, preSuppress, vendor map[string]map[string]struct{}, overlayLoaded, vendorLoaded bool) (map[string]claudeAlias, []string) {
	if !token.ClaudeModelAliases || !overlayLoaded || !vendorLoaded {
		return nil, nil
	}
	reserved := make(map[string]bool, len(preSuppress)+len(vendor)+len(token.ModelOverrideRules))
	for name := range preSuppress {
		reserved[name] = true
	}
	for name := range listed {
		reserved[name] = true
	}
	for name := range vendor {
		reserved[name] = true
	}
	for name := range token.ModelOverrideRules {
		reserved[name] = true
	}
	return claudeAliasMap(claudeAliasEligible(listed, token.ModelOverrideRules, vendor), reserved, listingLabel)
}

// claudeAliasesForToken derives a token's generated aliases from scratch: it
// reads the token's visible mapping views and the group overlay itself and
// composes the same alias-free listing discovery does (baseFlavorSets), then
// asks claudeAliasesFromListing. It is the accessor inference resolution uses,
// so it costs one mapping traversal, one overlay load, one runtime-spec read and
// one vendor-model read per call (like ModelOfferingFor, nothing is cached): a
// caller on the request path should check the cheap preconditions (the setting,
// and that the requested id starts with claudeAliasIDPrefix) before calling it.
//
// A store error yields no aliases (fail closed), the vendor-model read's
// included: an incomplete vendor set cannot say which local names a vendor
// account shadows (see claudeAliasesFromListing).
func (s *Service) claudeAliasesForToken(ctx context.Context, token auth.Token) (map[string]claudeAlias, []string) {
	if !token.ClaudeModelAliases || s.routes == nil {
		return nil, nil
	}
	views, err := s.visibleMappingViews(ctx, token)
	if err != nil {
		return nil, nil
	}
	overlay, err := s.loadGroupOverlayInputs(ctx)
	if err != nil {
		return nil, nil
	}
	sets, preSuppress := baseFlavorSets(views, s.runtimeSpecIndexForViews(ctx, views), &overlay)
	vendor, vendorLoaded := s.vendorModelFlavorSets(ctx, token)
	return claudeAliasesFromListing(token, sets, preSuppress, vendor, true, vendorLoaded)
}

// ClaudeAliasTarget is the inference-side accessor of the generated aliases: it
// returns the real gateway model/group id the generated alias `requested`
// stands for on this token's behalf, and ok=false when `requested` is not one
// of the token's generated aliases. The gateway's inference preflight calls it
// (see resolveModelOverride) to resolve an alias once, before every access
// gate, so an alias is never anything but its real target downstream.
//
// It answers from the very derivation discovery lists (claudeAliasesForToken),
// so a resolvable alias is exactly an offered one: an alias a manual rule owns,
// a real name, an ambiguous one, or one whose target is hidden, locked or no
// longer Messages-capable is not in the map and does not resolve. Nothing is
// ever resolved for a token without the setting.
//
// It runs on the inference hot path for every request, and the derivation costs
// store reads, so it answers before deriving anything when the call cannot be
// an alias: the setting is off, or `requested` lacks claudeAliasIDPrefix (every
// ordinary model name).
func (s *Service) ClaudeAliasTarget(ctx context.Context, token auth.Token, requested string) (original string, ok bool) {
	if !token.ClaudeModelAliases || !strings.HasPrefix(requested, claudeAliasIDPrefix) {
		return "", false
	}
	aliases, _ := s.claudeAliasesForToken(ctx, token)
	alias, found := aliases[requested]
	if !found {
		return "", false
	}
	return alias.OriginalID, true
}

// logClaudeAliasDiagnostics logs the skipped-alias diagnostics of a derivation.
func logClaudeAliasDiagnostics(diagnostics []string) {
	for _, diagnostic := range diagnostics {
		slog.Warn("portal: generated Claude alias skipped", "reason", diagnostic)
	}
}
