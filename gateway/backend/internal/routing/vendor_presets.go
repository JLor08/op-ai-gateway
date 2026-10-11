// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package routing

// VendorPreset is the fixed, per-vendor data for an OpenAI-compatible hosted
// provider. It lives in package routing because the resolver needs the path
// prefix (routing cannot import portal); the portal reads the rest to validate
// a key, discover models and (sub-project D) pull usage. vendorauth never sees
// it — the portal composes plain URL strings from these fields.
type VendorPreset struct {
	// DefaultBaseURL is the upstream ROOT (no /v1 or other path prefix); "" for
	// VendorOpenAICompatible, where the user must supply BaseURL.
	DefaultBaseURL string
	// PathPrefix is the segment the client puts between BaseURL and
	// "/chat/completions" (and "/models"): "/v1", Kilo's "/gateway", Gemini's
	// "/v1beta/openai". It is a path with a leading "/" and no trailing "/".
	PathPrefix string
	// ValidateVia selects how the portal checks an api key at Test connection:
	//   "models" — GET {BaseURL}{PathPrefix}/models (listing needs a key)
	//   "key"    — GET {BaseURL}{PathPrefix}{ValidatePath} (a key-introspection endpoint)
	//   "none"   — no offline validation (the models listing is public and there
	//              is no key endpoint); Test connection reports "could not verify".
	ValidateVia string
	// ValidatePath is the suffix for ValidateVia=="key" (e.g. "/key"); "" otherwise.
	ValidatePath string
	// StripModelsPrefix, when non-empty, is stripped from the front of each
	// discovered model id (Gemini's "models/"); "" = keep ids verbatim.
	StripModelsPrefix string
	// UsageVia is reserved for sub-project D (per-provider usage/credits). Empty
	// in sub-project A: no provider reports usage yet.
	UsageVia string
}

// vendorPresets is the registry of OpenAI-compatible providers. The keys are the
// VendorOpenAICompatible-family ids; openai/anthropic are NOT here (they keep
// their bespoke resolver branch). Roots/prefixes are confirmed live (2026-10-10):
// see ADR-052 and external-vendor-accounts.md §10.
var vendorPresets = map[string]VendorPreset{
	VendorXAI: {
		DefaultBaseURL: "https://api.x.ai",
		PathPrefix:     "/v1",
		ValidateVia:    "models", // GET /v1/models needs a key (bad key -> 400 -> unverifiable)
	},
	VendorOpenRouter: {
		DefaultBaseURL: "https://openrouter.ai/api",
		PathPrefix:     "/v1",
		ValidateVia:    "key", // /v1/models is public; validate against /v1/key
		ValidatePath:   "/key",
	},
	VendorKilo: {
		DefaultBaseURL: "https://api.kilo.ai/api",
		PathPrefix:     "/gateway",
		ValidateVia:    "none", // /gateway/models is public and there is no key endpoint
	},
	VendorGoogle: {
		DefaultBaseURL:    "https://generativelanguage.googleapis.com",
		PathPrefix:        "/v1beta/openai",
		ValidateVia:       "models", // GET .../models needs a key (bad key -> 400 -> unverifiable)
		StripModelsPrefix: "models/",
	},
	VendorOpenAICompatible: {
		DefaultBaseURL: "", // user supplies the root
		PathPrefix:     "/v1",
		ValidateVia:    "models",
	},
}

// IsOpenAICompatibleVendor reports whether vendor is one of the OpenAI-compatible
// hosted providers (routed through OpenAICompatibleClient against its own BaseURL).
func IsOpenAICompatibleVendor(vendor string) bool {
	_, ok := vendorPresets[vendor]
	return ok
}

// VendorPresetFor returns the preset for an OpenAI-compatible vendor.
func VendorPresetFor(vendor string) (VendorPreset, bool) {
	p, ok := vendorPresets[vendor]
	return p, ok
}

// OpenAIPathPrefixFor is the Target.OpenAIPathPrefix value for an OpenAI-compatible
// vendor; "" for every other vendor (so the Target keeps the default-/v1 behaviour).
func OpenAIPathPrefixFor(vendor string) string {
	if p, ok := vendorPresets[vendor]; ok {
		return p.PathPrefix
	}
	return ""
}
