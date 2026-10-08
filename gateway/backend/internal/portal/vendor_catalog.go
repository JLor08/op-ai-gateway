// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import "op-ai-gateway/internal/routing"

// REVERSE-ENGINEERED / update as vendors change.
//
// The curated per-vendor model catalog, kept in this ONE file so a refresh is a
// one-file change. It is deliberately SMALL: the handful of vendor-native public
// model ids users actually route to, not the vendor's full model list. Vendors
// rename, retire and add models without notice, and the subscription path in
// particular serves whatever the consumer plan currently exposes, so treat every
// id here as a best-effort snapshot to be re-verified (Milestone 5 spike), not a
// contract. This catalog is ONLY the create-time seed and the fallback: the
// vendor's real model list is discovered from the vendor itself when an account
// connects and on the explicit refresh (RefreshVendorAccountModels), and replaces
// these rows; a discovery that cannot be had leaves them in place. A change here
// only affects accounts created afterwards.
//
// The OpenAI set depends on the account's auth type, because the two auth types
// reach different backends: a subscription account is served by the Codex
// ChatGPT backend, which (as far as is known) does NOT serve gpt-4.1 or o3,
// while an api_key account talks to api.openai.com, which serves all four. Only
// the shared gpt-5 / gpt-5-mini are seeded on a subscription account, so that a
// working credential is never paired with a model its backend rejects. The
// subscription set is the less certain of the two (VERIFY-LIVE: a plan may serve
// further codex-* ids that are not seeded until confirmed). Anthropic's OAuth
// Messages path and its api key serve the same ids, so its set does not vary.
var (
	openAISubscriptionModels = []string{"gpt-5", "gpt-5-mini"}
	openAIAPIKeyModels       = []string{"gpt-5", "gpt-5-mini", "gpt-4.1", "o3"}
	anthropicModels          = []string{"claude-opus-5-5", "claude-sonnet-5-5", "claude-haiku-4-5"}
)

// VendorCatalog returns the curated model set a new account of the given vendor
// and auth type (routing.VendorAuthSubscription or routing.VendorAuthAPIKey) is
// seeded with: OpenAI models are served over the OpenAI wire flavor, Anthropic
// models over the Anthropic one. The OpenAI set is narrower for a subscription
// account than for an api_key one (see the catalog comment above); the Anthropic
// set is the same for both.
//
// GatewayModel (what a caller asks the gateway for) is the vendor's own public
// model id and so is UpstreamModel: for these vendors the two coincide, which
// keeps the gateway a transparent pass-through (a client that already speaks
// "gpt-5" or "claude-sonnet-5-5" needs no renaming). They are separate columns
// in vendor_account_models so an account can diverge later.
//
// AccountID is left empty (the store stamps it with the account being seeded).
// The result is a fresh slice the caller may modify; an unknown vendor or auth
// type yields an empty one.
func VendorCatalog(vendor, authType string) []routing.VendorAccountModel {
	if authType != routing.VendorAuthSubscription && authType != routing.VendorAuthAPIKey {
		return []routing.VendorAccountModel{}
	}
	var ids []string
	switch vendor {
	case routing.VendorOpenAI:
		ids = openAIAPIKeyModels
		if authType == routing.VendorAuthSubscription {
			ids = openAISubscriptionModels
		}
	case routing.VendorAnthropic:
		ids = anthropicModels
	default:
		return []routing.VendorAccountModel{}
	}
	flavor, _ := vendorAPIFlavor(vendor)
	out := make([]routing.VendorAccountModel, 0, len(ids))
	for _, id := range ids {
		out = append(out, routing.VendorAccountModel{GatewayModel: id, UpstreamModel: id, APIFlavor: flavor})
	}
	return out
}

// vendorAPIFlavor is the wire flavor a vendor's models are served over: OpenAI
// models over the OpenAI dialect, Anthropic ones over the Anthropic dialect. It is
// shared by the static seed (VendorCatalog) and the discovered rows
// (RefreshVendorAccountModels), so the two cannot disagree. ok is false for a
// vendor with no flavor.
func vendorAPIFlavor(vendor string) (flavor string, ok bool) {
	switch vendor {
	case routing.VendorOpenAI:
		return routing.APIFlavorOpenAI, true
	case routing.VendorAnthropic:
		return routing.APIFlavorAnthropic, true
	}
	return "", false
}
