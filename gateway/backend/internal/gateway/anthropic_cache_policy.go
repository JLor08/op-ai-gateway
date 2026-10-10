// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"op-ai-gateway/internal/inference"
	"op-ai-gateway/internal/routing"
	"strings"
	"time"
)

// anthropicPromptCachingFlagTTL bounds how long the dispatch hot path reuses a
// cached read of the anthropic_prompt_caching_enabled system setting. The setting
// lives in system_settings, whose store read is an uncached full-table SELECT, and
// the policy below runs on EVERY translate dispatch to an Anthropic target, so a
// per-request read would add a database round-trip to the hot path.
// handleSystemSettings invalidates the cache explicitly after a PUT that carried the
// key (invalidateAnthropicPromptCachingCache), so an operator toggling it in the
// portal sees the effect on the very next request; the TTL only bounds how stale an
// OUT-OF-BAND change (a direct database edit) can be. Same trade-off, and the same
// value, as vendorSettingsCacheTTL.
const anthropicPromptCachingFlagTTL = 5 * time.Second

// anthropicPromptCachingEnabledCached reports the anthropic_prompt_caching_enabled
// master flag through the short-TTL, invalidatable cache
// (anthropicPromptCachingCache). Nil-safe: a Server with no portal reports false
// (the feature is opt-in, so an unreadable flag fails closed), mirroring
// portal.Service.AnthropicPromptCachingEnabled's own posture.
func (s *Server) anthropicPromptCachingEnabledCached(ctx context.Context) bool {
	if s.Portal == nil {
		return false
	}
	return s.anthropicPromptCachingCache.Get(ctx, anthropicPromptCachingFlagTTL, s.Portal.AnthropicPromptCachingEnabled)
}

// invalidateAnthropicPromptCachingCache drops the cached flag read so the next
// dispatch re-reads it. Called by handleSystemSettings after a successful PUT that
// carried anthropic_prompt_caching_enabled, so an operator toggling it does not
// have to wait out anthropicPromptCachingFlagTTL (the generation bump also cancels a
// store read already in flight; see settingCache.Get).
func (s *Server) invalidateAnthropicPromptCachingCache() {
	s.anthropicPromptCachingCache.Invalidate()
}

// anthropicCacheMinTokens is the model's minimum cacheable prefix (tokens). Below
// it, Anthropic ignores cache_control for free, so an unknown model defaults to the
// conservative 1024. Source: Claude prompt-caching reference.
func anthropicCacheMinTokens(model string) int {
	m := strings.ToLower(model)
	switch {
	case strings.Contains(m, "opus-4-6"), strings.Contains(m, "opus-4-5"), strings.Contains(m, "haiku-4-5"):
		return 4096
	case strings.Contains(m, "opus-4-7"):
		return 2048
	case strings.Contains(m, "opus-5"), strings.Contains(m, "sonnet-5-5"), strings.Contains(m, "haiku-5-5"), strings.Contains(m, "fable-5"):
		return 512
	default:
		return 1024 // Opus 4.8, Sonnet 5, Sonnet 4.x, and unknown
	}
}

// estPrefixTokens is a cheap char/4 estimate of the cacheable prefix (system +
// developer text, prior turns, tool calls, tool names/descriptions). It only needs
// to be in the right ballpark to clear the model minimum: below the minimum caching
// is a free no-op upstream, and a low estimate merely skips the markers, which is
// today's behavior. Tool parameter schemas are deliberately not measured (that
// would need a marshal per request); the estimate errs low there, never high.
func estPrefixTokens(req inference.Request) int {
	chars := 0
	for _, m := range req.Messages {
		for _, part := range m.Content {
			if part.Type == inference.ContentText {
				chars += len(part.Text)
			}
		}
		for _, tc := range m.ToolCalls {
			chars += len(tc.Name) + len(tc.Arguments)
		}
	}
	for _, tl := range req.Tools {
		chars += len(tl.Name) + len(tl.Description)
	}
	return chars / 4
}

// hasAssistantHistory reports whether the conversation already carries an assistant
// turn, i.e. this is at least the second round of an exchange whose stable prefix
// a follow-up request will re-send.
func hasAssistantHistory(msgs []inference.Message) bool {
	for _, m := range msgs {
		if m.Role == inference.RoleAssistant {
			return true
		}
	}
	return false
}

// isAnthropicTranslateTarget reports whether t is served by the native Anthropic
// Messages client in TRANSLATE mode, the only path that builds the request body
// from the neutral inference.Request (so the only one a PromptCacheDirective can
// reach). Both the api-key account and the OAuth-subscription account (with its
// Claude-Code masquerade) resolve to routing.ProviderVendorAnthropic; the
// subscription target differs only in Subscription/Masquerade, which the
// provider's builder already handles. A native /v1/messages passthrough never
// reaches the dispatch hooks, so it is untouched.
func isAnthropicTranslateTarget(t routing.Target) bool {
	return t.Provider == routing.ProviderVendorAnthropic
}

// applyAnthropicCachePolicy sets req.PromptCache when the hybrid auto-switch says
// caching is worthwhile: the flag is on, the target is an Anthropic translate
// target, the prefix is likely reused (the conversation has assistant history, or
// it is a portal-chat session whose next message will re-send it), and the
// estimated prefix clears the model's cacheable minimum. Otherwise it leaves
// req.PromptCache nil, which renders byte-identically to a build without caching.
//
// The cheap structural checks run first and the O(prompt) estimate last, so a
// non-Anthropic or one-shot request pays almost nothing.
func (s *Server) applyAnthropicCachePolicy(ctx context.Context, target routing.Target, req *inference.Request) {
	if !isAnthropicTranslateTarget(target) {
		return
	}
	if !hasAssistantHistory(req.Messages) && req.SessionSource != "chat" {
		return
	}
	if !s.anthropicPromptCachingEnabledCached(ctx) {
		return
	}
	// Look the minimum up under the model the request is actually rendered with.
	if estPrefixTokens(*req) < anthropicCacheMinTokens(effectiveProviderModel(target, req.Model)) {
		return
	}
	req.PromptCache = &inference.PromptCacheDirective{Enabled: true} // TTL "" = 5m ephemeral
}
