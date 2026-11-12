// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package routing

import "op-ai-gateway/internal/inference"

// OpenAIDecisionModels is the fixed allowlist of the OpenAI models that answer
// OpenAI Decisions (POST /v1/decisions) on api.openai.com (ADR-100). OpenAI
// Decisions went GA on 2026-10-22 with one model, gpt-6-luna. It is the ONLY
// source of a vendor decision verdict: there is no portal flag and no stored
// row, so a new decision model is a change to this set. Read-only.
var OpenAIDecisionModels = map[string]struct{}{
	"gpt-6-luna": {},
}

// IsVendorDecisionModel reports whether model row m of vendor account acc is a
// decision model: the account is an OpenAI API-KEY account (api.openai.com, the
// one vendor backend with an OpenAI Decisions endpoint; a subscription account
// talks to the ChatGPT backend, which has none, and an Anthropic, Gemini or
// OpenAI-compatible preset account is another vendor's API), and the row's
// UPSTREAM model, the id sent to OpenAI, is in OpenAIDecisionModels. The gateway
// name never decides: a prefix renames a model, it does not make one.
func IsVendorDecisionModel(acc VendorAccount, m VendorAccountModel) bool {
	if !isVendorDecisionAccount(acc) {
		return false
	}
	_, ok := OpenAIDecisionModels[m.UpstreamModel]
	return ok
}

// isVendorDecisionAccount reports whether acc can serve a decision model at
// all: an OpenAI api-key account, and never a preset vendor (OpenAI-compatible
// or Gemini), whose root URL is another vendor's API.
func isVendorDecisionAccount(acc VendorAccount) bool {
	return acc.Vendor == VendorOpenAI && acc.AuthType == VendorAuthAPIKey && !IsPresetVendor(acc.Vendor)
}

// isVendorDecisionRequest reports whether req is a decision request the vendor
// path may serve (ADR-100): a decision endpoint's flavor (coarse decisions,
// which both /v1/decisions and /v1/systemone fold to) with exactly the derived
// [any_decision] requirement both endpoints carry. It is the ONE capability
// list that opts into the vendor path; every other one stays self-hosted.
func isVendorDecisionRequest(req inference.Request, apiFlavor string) bool {
	return apiFlavor == APIFlavorDecisions &&
		len(req.RequiredCapabilities) == 1 &&
		req.RequiredCapabilities[0] == CapabilityAnyDecision
}

// vendorCapabilitiesEligible is the capability half of
// vendorAccountRoutingEligible: a request without a capability list, unless it
// is on the decisions flavor (a decision endpoint always carries its
// requirement), or a decision request (isVendorDecisionRequest).
func vendorCapabilitiesEligible(req inference.Request, apiFlavor string) bool {
	if len(req.RequiredCapabilities) == 0 {
		return apiFlavor != APIFlavorDecisions
	}
	return isVendorDecisionRequest(req, apiFlavor)
}

// withVendorDecisionRoute turns an OpenAI api-key account's target into the
// target of a decision request for its decision model (ADR-100): the decisions
// flavor beside the chat ones, /v1/decisions passed through to the vendor's own
// endpoint and /v1/systemone translated to it, and the decision-model mark the
// gateway reads the verdicts from (a vendor target has no mapping rows). The
// kind stays vendor_openai, which is DecisionsPassthroughCapableKind.
func withVendorDecisionRoute(t Target) Target {
	t.APIFlavors = append(append([]string(nil), t.APIFlavors...), APIFlavorDecisions)
	t.DecisionsMode = EndpointModePassthrough
	t.SystemOneMode = EndpointModeTranslate
	t.VendorDecisionModel = true
	return t
}
