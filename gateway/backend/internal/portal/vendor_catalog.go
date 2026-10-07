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
// contract. An account keeps the rows it was seeded with: a change here only
// affects accounts created afterwards; existing accounts are rewritten with
// SetVendorAccountModels.
var (
	openAIModels    = []string{"gpt-5", "gpt-5-mini", "gpt-4.1", "o3"}
	anthropicModels = []string{"claude-opus-5-5", "claude-sonnet-5-5", "claude-haiku-4-5"}
)

// VendorCatalog returns the curated model set a new account of the given vendor
// is seeded with: OpenAI models are served over the OpenAI wire flavor,
// Anthropic models over the Anthropic one.
//
// GatewayModel (what a caller asks the gateway for) is the vendor's own public
// model id and so is UpstreamModel: for these vendors the two coincide, which
// keeps the gateway a transparent pass-through (a client that already speaks
// "gpt-5" or "claude-sonnet-5-5" needs no renaming). They are separate columns
// in vendor_account_models so an account can diverge later.
//
// AccountID is left empty (the store stamps it with the account being seeded).
// The result is a fresh slice the caller may modify; an unknown vendor yields an
// empty one.
func VendorCatalog(vendor string) []routing.VendorAccountModel {
	var (
		ids    []string
		flavor string
	)
	switch vendor {
	case routing.VendorOpenAI:
		ids, flavor = openAIModels, routing.APIFlavorOpenAI
	case routing.VendorAnthropic:
		ids, flavor = anthropicModels, routing.APIFlavorAnthropic
	default:
		return []routing.VendorAccountModel{}
	}
	out := make([]routing.VendorAccountModel, 0, len(ids))
	for _, id := range ids {
		out = append(out, routing.VendorAccountModel{GatewayModel: id, UpstreamModel: id, APIFlavor: flavor})
	}
	return out
}
