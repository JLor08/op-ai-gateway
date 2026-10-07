// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package vendorauth

// ============================================================================
// EVERY VALUE IN THIS FILE IS REVERSE-ENGINEERED FROM THE OFFICIAL VENDOR CLIs.
//
// The OAuth endpoints, client ids, redirect URIs, scopes and token-claim names
// below are UNDOCUMENTED, were not issued to this project, are ToS-sensitive
// and EXPERIMENTAL. Any of them may be stale, rotated, blocked, or simply
// wrong. VERIFY LIVE before relying on any value. Nothing outside this file
// may hardcode a vendor host, client id, scope or claim name; the flow
// functions read them through Endpoints so a live test (or an operator) can
// point them at a corrected endpoint, and the unit tests point them at
// httptest servers.
//
// Values explicitly marked UNVERIFIED below were not even confirmed against
// the CLIs and are best guesses.
// ============================================================================

// PKCEMethodS256 is the PKCE code_challenge_method both vendors expect.
const PKCEMethodS256 = "S256"

// Anthropic (Claude Code subscription OAuth: Claude Pro / Max).
//
// REVERSE-ENGINEERED / VERIFY LIVE.
const (
	// AnthropicClientID is the public OAuth client id the Claude Code CLI uses.
	AnthropicClientID = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
	// AnthropicAuthorizeURL is where the user signs in and approves access.
	AnthropicAuthorizeURL = "https://claude.ai/oauth/authorize"
	// AnthropicTokenURL exchanges a code / refresh token for tokens (JSON body).
	AnthropicTokenURL = "https://console.anthropic.com/v1/oauth/token"
	// AnthropicRedirectURI is the vendor-hosted page that displays the
	// "code#state" string the user pastes back (no loopback listener needed).
	AnthropicRedirectURI = "https://console.anthropic.com/oauth/code/callback"
	// AnthropicScopes is the space-separated scope list the CLI requests.
	AnthropicScopes = "org:create_api_key user:profile user:inference"
	// AnthropicParamShowCode / AnthropicParamShowCodeValue are the authorize
	// parameter that makes the callback page display the "code#state" string
	// for pasting instead of redirecting a browser to a listener.
	AnthropicParamShowCode      = "code"
	AnthropicParamShowCodeValue = "true"
	// AnthropicBeta is the anthropic-beta header value that enables OAuth
	// bearer tokens on the Messages API. Not used by this package's flows;
	// kept here so every Anthropic vendor constant lives in one file.
	AnthropicBeta = "oauth-2025-04-20"
	// AnthropicAccessTokenPrefix and AnthropicRefreshTokenPrefix are the
	// observed token shapes; useful only for sanity checks and log hygiene.
	AnthropicAccessTokenPrefix  = "sk-ant-oat01-"
	AnthropicRefreshTokenPrefix = "sk-ant-ort01-"
)

// OpenAI (Codex CLI ChatGPT-subscription OAuth).
//
// REVERSE-ENGINEERED / VERIFY LIVE.
const (
	// OpenAIClientID is the public OAuth client id the Codex CLI uses.
	OpenAIClientID = "app_EMoamEEZ73f0CkXaXp7hrann"
	// OpenAIAuthorizeURL is where the user signs in and approves access.
	OpenAIAuthorizeURL = "https://auth.openai.com/oauth/authorize"
	// OpenAITokenURL exchanges a code / device code / refresh token for tokens.
	// This package sends form-encoded bodies (standard OAuth); the Codex CLI
	// may use JSON for some grants. VERIFY LIVE.
	OpenAITokenURL = "https://auth.openai.com/oauth/token"
	// OpenAIDeviceAuthorizeURL is the RFC 8628 device-authorization endpoint.
	//
	// UNVERIFIED: this URL is a standard-OAuth guess. The official CLI is
	// believed to use a bespoke device-auth endpoint rather than RFC 8628 (and
	// the response shape may differ), and the device flow may not be enabled
	// for the Codex client id at all. Treat the whole device-code path as
	// unproven until a live test confirms it; the authorization-code flow with
	// OpenAIRedirectURI is the fallback.
	OpenAIDeviceAuthorizeURL = "https://auth.openai.com/oauth/device/code"
	// OpenAIRedirectURI is the loopback callback the Codex CLI registers. The
	// gateway cannot listen there, so the code-paste fallback has the user copy
	// the failed-redirect URL's code back by hand.
	OpenAIRedirectURI = "http://localhost:1455/auth/callback"
	// OpenAIScopes is the space-separated scope list the CLI requests.
	OpenAIScopes = "openid profile email offline_access"

	// Extra authorize-URL parameters the Codex CLI adds on top of standard
	// OAuth+PKCE (added by BuildOpenAIAuthorizeURL): id_token_add_organizations
	// and codex_cli_simplified_flow are set to "true", originator to
	// OpenAIOriginator.
	OpenAIParamIDTokenAddOrganizations = "id_token_add_organizations"
	OpenAIParamCodexCLISimplifiedFlow  = "codex_cli_simplified_flow"
	OpenAIParamOriginator              = "originator"
	// OpenAIOriginator identifies the client to the vendor; also sent as the
	// originator request header at dispatch time.
	OpenAIOriginator = "codex_cli_rs"

	// OpenAIAuthClaimNamespace is the id_token (and access_token) custom-claim
	// object holding the ChatGPT account facts.
	OpenAIAuthClaimNamespace = "https://api.openai.com/auth"
	// OpenAIClaimAccountID and OpenAIClaimPlanType are the keys inside that
	// object. The account id is sent back as the chatgpt-account-id header.
	OpenAIClaimAccountID = "chatgpt_account_id"
	OpenAIClaimPlanType  = "chatgpt_plan_type"
)

// Endpoints carries every vendor-specific OAuth parameter the flow functions
// read: the HTTP URLs plus the client id, redirect URI, scopes and extra
// authorize parameters. It is named for its main purpose (pointing the flows at
// a real, corrected or httptest endpoint) but includes the rest so a live test
// can correct a stale client id or scope without touching the flow code.
//
// Build one with DefaultAnthropicEndpoints or DefaultOpenAIEndpoints and
// override fields as needed. The flow functions do not fall back to defaults
// for zero fields: what is in the struct is what is sent.
type Endpoints struct {
	// ClientID is the OAuth client_id sent on every request.
	ClientID string
	// AuthorizeURL is the browser authorization endpoint (never fetched by this
	// package; only used to build the URL the user opens).
	AuthorizeURL string
	// TokenURL is the token endpoint for the code, refresh and device grants.
	TokenURL string
	// DeviceAuthorizeURL is the device-authorization endpoint (OpenAI only).
	DeviceAuthorizeURL string
	// RedirectURI is the registered redirect_uri.
	RedirectURI string
	// Scopes is the space-separated scope list.
	Scopes string
}

// DefaultAnthropicEndpoints returns the reverse-engineered Claude Code OAuth
// endpoints. VERIFY LIVE.
func DefaultAnthropicEndpoints() Endpoints {
	return Endpoints{
		ClientID:     AnthropicClientID,
		AuthorizeURL: AnthropicAuthorizeURL,
		TokenURL:     AnthropicTokenURL,
		RedirectURI:  AnthropicRedirectURI,
		Scopes:       AnthropicScopes,
	}
}

// DefaultOpenAIEndpoints returns the reverse-engineered Codex OAuth endpoints.
// DeviceAuthorizeURL is UNVERIFIED. VERIFY LIVE.
func DefaultOpenAIEndpoints() Endpoints {
	return Endpoints{
		ClientID:           OpenAIClientID,
		AuthorizeURL:       OpenAIAuthorizeURL,
		TokenURL:           OpenAITokenURL,
		DeviceAuthorizeURL: OpenAIDeviceAuthorizeURL,
		RedirectURI:        OpenAIRedirectURI,
		Scopes:             OpenAIScopes,
	}
}
