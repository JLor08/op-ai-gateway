// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package routing

// VendorPreset is the fixed, per-vendor data for a preset hosted provider
// (OpenAI-compatible, or native Gemini when Dialect is "gemini"). It lives in
// package routing because the resolver needs the path prefix (routing cannot
// import portal); the portal reads the rest to validate a key, discover models
// and (sub-project D) pull usage. vendorauth never sees it — the portal
// composes plain URL strings from these fields.
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
	// UsageVia selects the per-provider usage strategy: "openrouter_key"
	// (GET {BaseURL}{UsagePath} → key limit/usage), "kilo_balance" (best-effort
	// balance), "" = the provider reports no usage with the stored api key.
	UsageVia string
	// UsagePath is the ROOT-relative URL suffix for the usage GET (composed as
	// TrimRight(BaseURL,"/")+UsagePath), used when UsageVia != "". It is NOT under
	// PathPrefix: OpenRouter's key endpoint is "/v1/key" and Kilo's undocumented
	// balance is "/profile/balance" (outside its "/gateway" prefix). "" when the
	// provider reports no usage.
	UsagePath string
	// Dialect is the upstream wire dialect: "" = OpenAI wire (served by the
	// shared OpenAI-compatible client), "gemini" = native generateContent.
	Dialect string
}

// vendorPresets is the registry of preset providers: the OpenAI-compatible
// family plus the native Gemini account (Dialect "gemini"). The keys are the
// VendorOpenAICompatible-family ids and VendorGoogleNative; openai/anthropic
// are NOT here (they keep their bespoke resolver branch). Roots/prefixes are
// confirmed live (2026-10-10): see ADR-052 and external-vendor-accounts.md §10.
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
		UsageVia:       "openrouter_key",
		UsagePath:      "/v1/key",
	},
	VendorKilo: {
		DefaultBaseURL: "https://api.kilo.ai/api",
		PathPrefix:     "/gateway",
		ValidateVia:    "none", // /gateway/models is public and there is no key endpoint
		UsageVia:       "kilo_balance",
		UsagePath:      "/profile/balance",
	},
	VendorGoogle: {
		DefaultBaseURL:    "https://generativelanguage.googleapis.com",
		PathPrefix:        "/v1beta/openai",
		ValidateVia:       "models", // GET .../models needs a key (bad key -> 400 -> unverifiable)
		StripModelsPrefix: "models/",
	},
	VendorGoogleNative: {
		DefaultBaseURL:    "https://generativelanguage.googleapis.com",
		PathPrefix:        "/v1beta", // portal discovery/validation only
		ValidateVia:       "models",
		StripModelsPrefix: "models/",
		Dialect:           "gemini",
	},
	VendorOpenAICompatible: {
		DefaultBaseURL: "", // user supplies the root
		PathPrefix:     "/v1",
		ValidateVia:    "models",
	},
}

// IsPresetVendor reports whether v is a preset (configurable-root, api-key-only)
// vendor account, regardless of wire dialect.
func IsPresetVendor(v string) bool {
	_, ok := vendorPresets[v]
	return ok
}

// IsOpenAICompatibleVendor reports whether v is served by the OpenAI-compatible
// client (a preset vendor whose wire dialect is the OpenAI one).
func IsOpenAICompatibleVendor(v string) bool {
	p, ok := vendorPresets[v]
	return ok && p.Dialect == ""
}

// IsNativeGeminiVendor reports whether v is served by the native Gemini client.
func IsNativeGeminiVendor(v string) bool {
	p, ok := vendorPresets[v]
	return ok && p.Dialect == "gemini"
}

// VendorPresetFor returns the preset for a preset vendor (any dialect).
func VendorPresetFor(vendor string) (VendorPreset, bool) {
	p, ok := vendorPresets[vendor]
	return p, ok
}

// OpenAIPathPrefixFor is the Target.OpenAIPathPrefix value for an
// OpenAI-compatible (OpenAI-wire) preset vendor; "" for every other vendor,
// including a native-dialect preset such as google_native, which the OpenAI
// path machinery never serves (so the Target keeps the default-/v1 behaviour).
func OpenAIPathPrefixFor(vendor string) string {
	if p, ok := vendorPresets[vendor]; ok && p.Dialect == "" {
		return p.PathPrefix
	}
	return ""
}
