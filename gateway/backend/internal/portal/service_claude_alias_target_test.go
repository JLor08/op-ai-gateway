// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"errors"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/store"
	"strings"
	"testing"
)

// Resolution side of the Claude model aliases (issue #225): ClaudeAliasTarget,
// the accessor the gateway's inference preflight calls. It reuses the discovery
// derivation (claudeAliasesForToken), so these tests pin the accessor's own
// contract -- what it resolves, what it refuses, and that it stays off the
// derivation unless the call can be an alias. The derivation itself is tested
// in claude_aliases_test.go and service_claude_aliases_test.go (same fixtures).

// modelSettingsCountingStore counts the model_settings reads. The alias
// derivation always makes one (loadGroupOverlayInputs), so the count says
// whether a call went through the derivation at all.
type modelSettingsCountingStore struct {
	*routing.MemoryStore
	reads int
}

func (c *modelSettingsCountingStore) ModelSettings(ctx context.Context) ([]routing.ModelSetting, error) {
	c.reads++
	return c.MemoryStore.ModelSettings(ctx)
}

// TestClaudeAliasTargetResolvesWhatDiscoveryOffers: the inference accessor
// answers with the real gateway id of every alias the token is offered (models
// and groups alike) and with nothing else.
func TestClaudeAliasTargetResolvesWhatDiscoveryOffers(t *testing.T) {
	svc, _ := newClaudeAliasWorld(t)
	ctx := context.Background()
	token := claudeToken(nil)

	for _, original := range claudeAliasedInWorld {
		t.Run(original, func(t *testing.T) {
			got, ok := svc.ClaudeAliasTarget(ctx, token, claudeAliasIDOf(t, original))
			if !ok || got != original {
				t.Fatalf("ClaudeAliasTarget(alias of %q) = (%q, %v), want (%q, true)", original, got, ok, original)
			}
		})
	}
	// Every listed alias row resolves to the row's own original_model_id.
	rows := claudeAliasRows(svc.Models(ctx, token))
	if len(rows) != len(claudeAliasedInWorld) {
		t.Fatalf("precondition: %d alias rows listed, want %d", len(rows), len(claudeAliasedInWorld))
	}
	for original, row := range rows {
		if got, ok := svc.ClaudeAliasTarget(ctx, token, row.ID); !ok || got != original {
			t.Errorf("listed alias %s resolves to (%q, %v), want (%q, true)", row.ID, got, ok, original)
		}
	}
}

// TestClaudeAliasTargetRefusesEverythingNotAnOfferedAlias: a name that is not a
// generated alias of THIS token resolves to nothing. That includes the real
// names themselves (a real name is never rewritten here), an alias-shaped id
// nobody derived, a differently-cased spelling of a real alias, and the alias
// ids of models the token is not offered (hidden, locked, no Messages flavor):
// resolution never reaches what discovery does not offer.
func TestClaudeAliasTargetRefusesEverythingNotAnOfferedAlias(t *testing.T) {
	svc, _ := newClaudeAliasWorld(t)
	ctx := context.Background()
	token := claudeToken(nil)

	valid := claudeAliasIDOf(t, "local-ok")
	notOffered := []string{
		"", "local-ok", "team", claudeAliasIDPrefix, claudeAliasIDPrefix + "000000000000",
		strings.ToUpper(valid),
	}
	for _, excluded := range claudeExcludedInWorld {
		if id, ok := claudeAliasID(excluded); ok {
			notOffered = append(notOffered, id)
		}
	}
	for _, requested := range notOffered {
		if got, ok := svc.ClaudeAliasTarget(ctx, token, requested); ok || got != "" {
			t.Errorf("ClaudeAliasTarget(%q) = (%q, %v), want (\"\", false)", requested, got, ok)
		}
	}
}

// TestClaudeAliasTargetIsOffWithoutTheSetting: the same alias id that resolves
// for a token with the setting on resolves to nothing for a token without it.
// This is the disable path: switching the setting off stops resolution, with no
// fallback to anything else.
func TestClaudeAliasTargetIsOffWithoutTheSetting(t *testing.T) {
	svc, _ := newClaudeAliasWorld(t)
	ctx := context.Background()
	aliasID := claudeAliasIDOf(t, "local-ok")

	if got, ok := svc.ClaudeAliasTarget(ctx, claudeToken(nil), aliasID); !ok || got != "local-ok" {
		t.Fatalf("precondition: flag-on token resolves (%q, %v), want (local-ok, true)", got, ok)
	}
	off := claudeToken(nil)
	off.ClaudeModelAliases = false
	if got, ok := svc.ClaudeAliasTarget(ctx, off, aliasID); ok || got != "" {
		t.Fatalf("flag-off token resolves (%q, %v), want (\"\", false)", got, ok)
	}
	// The portal-session principal never carries the setting.
	if got, ok := svc.ClaudeAliasTarget(ctx, auth.Token{}, aliasID); ok || got != "" {
		t.Fatalf("token-less principal resolves (%q, %v), want (\"\", false)", got, ok)
	}
}

// TestClaudeAliasTargetDefersToManualRules: an alias id a manual rule is keyed
// on is not generated (discovery skips it), so it must not resolve to the
// generated target either -- the manual rule owns the id. The other aliases are
// undisturbed.
func TestClaudeAliasTargetDefersToManualRules(t *testing.T) {
	svc, _ := newClaudeAliasWorld(t)
	ctx := context.Background()
	aliasID := claudeAliasIDOf(t, "local-ok")
	token := claudeToken(map[string]store.ModelOverrideRule{aliasID: {To: "local-all", Offer: true}})

	if got, ok := svc.ClaudeAliasTarget(ctx, token, aliasID); ok || got != "" {
		t.Fatalf("an alias id owned by a manual rule resolves to (%q, %v), want (\"\", false)", got, ok)
	}
	if got, ok := svc.ClaudeAliasTarget(ctx, token, claudeAliasIDOf(t, "local-all")); !ok || got != "local-all" {
		t.Fatalf("an unrelated alias resolves to (%q, %v), want (local-all, true)", got, ok)
	}
}

// TestClaudeAliasTargetShortCircuitsBeforeTheDerivation: the accessor sits on
// the inference hot path for every request, so a call that cannot be an alias
// (the setting is off, or the requested id lacks the alias prefix) must answer
// without the derivation's store reads, and a call that can be one must reach
// them (so the zero below is not vacuous).
func TestClaudeAliasTargetShortCircuitsBeforeTheDerivation(t *testing.T) {
	_, rs := newClaudeAliasWorld(t)
	counting := &modelSettingsCountingStore{MemoryStore: rs}
	svc := offerSvc(counting, nil)
	ctx := context.Background()
	aliasID := claudeAliasIDOf(t, "local-ok")
	off := claudeToken(nil)
	off.ClaudeModelAliases = false

	svc.ClaudeAliasTarget(ctx, off, aliasID)
	svc.ClaudeAliasTarget(ctx, claudeToken(nil), "local-ok")
	svc.ClaudeAliasTarget(ctx, claudeToken(nil), "qwen-coder")
	svc.ClaudeAliasTarget(ctx, claudeToken(nil), "")
	if counting.reads != 0 {
		t.Fatalf("a flag-off token or a non-alias id triggered %d model_settings reads, want 0", counting.reads)
	}

	if got, ok := svc.ClaudeAliasTarget(ctx, claudeToken(nil), aliasID); !ok || got != "local-ok" {
		t.Fatalf("flag-on alias resolves (%q, %v), want (local-ok, true)", got, ok)
	}
	if counting.reads == 0 {
		t.Fatal("a flag-on alias request never reached the derivation")
	}
}

// TestClaudeAliasTargetFailsClosedWhenVisibilityIsUnknown: while the visibility
// overlay cannot be read the derivation yields no aliases, so the accessor
// resolves nothing rather than guessing.
func TestClaudeAliasTargetFailsClosedWhenVisibilityIsUnknown(t *testing.T) {
	_, rs := newClaudeAliasWorld(t)
	svc := offerSvc(&failingModelSettingsStore{MemoryStore: rs, err: errors.New("model_settings unavailable")}, nil)
	if got, ok := svc.ClaudeAliasTarget(context.Background(), claudeToken(nil), claudeAliasIDOf(t, "local-ok")); ok || got != "" {
		t.Fatalf("resolves (%q, %v) while visibility is unknown, want (\"\", false)", got, ok)
	}
}
