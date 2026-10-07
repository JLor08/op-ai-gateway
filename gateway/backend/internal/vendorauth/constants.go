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
	// OpenAITokenURL exchanges a code / refresh token for tokens.
	// This package sends form-encoded bodies (standard OAuth); the Codex CLI
	// may use JSON for some grants. VERIFY LIVE.
	OpenAITokenURL = "https://auth.openai.com/oauth/token"
	// OpenAIRedirectURI is the loopback callback the Codex CLI registers. The
	// gateway cannot listen there, so the connect flow is code-paste: the user
	// authorizes, the browser lands on a dead loopback URL, and the user copies
	// the code from that URL back by hand.
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

// OpenAI Codex device-code login. This is the CLI's BESPOKE "deviceauth"
// protocol, NOT the generic RFC 8628 device grant, so an RFC 8628 client cannot
// talk to it. Reverse-engineered from the Codex CLI source
// (codex-rs/login/src/device_code_auth.rs). The issuer is the OpenAI auth host
// (auth.openai.com, the same host as OpenAIAuthorizeURL); the paths below hang
// off it. See internal/vendorauth/openai_device.go for the flow.
//
// REVERSE-ENGINEERED / VERIFY LIVE.
const (
	// OpenAIDeviceUsercodeURL starts a device login: POST {issuer}/api/accounts/
	// deviceauth/usercode with {"client_id"} answers {device_auth_id, user_code,
	// interval}.
	OpenAIDeviceUsercodeURL = "https://auth.openai.com/api/accounts/deviceauth/usercode"
	// OpenAIDeviceTokenURL is polled for the authorization: POST {issuer}/api/
	// accounts/deviceauth/token with {device_auth_id, user_code}. HTTP 403 or 404
	// means "not yet"; a 2xx carries {authorization_code, code_challenge,
	// code_verifier}.
	OpenAIDeviceTokenURL = "https://auth.openai.com/api/accounts/deviceauth/token"
	// OpenAIDeviceVerificationURL is the human page the user opens to type the
	// user_code. It is a fixed page ({issuer}/codex/device), not a field of the
	// usercode response.
	OpenAIDeviceVerificationURL = "https://auth.openai.com/codex/device"
	// OpenAIDeviceCallbackRedirect is the redirect_uri sent on the final
	// authorization-code exchange ({issuer}/deviceauth/callback). It differs from
	// the loopback OpenAIRedirectURI the code-paste flow uses.
	OpenAIDeviceCallbackRedirect = "https://auth.openai.com/deviceauth/callback"
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
	// TokenURL is the token endpoint for the code and refresh grants.
	TokenURL string
	// RedirectURI is the registered redirect_uri.
	RedirectURI string
	// Scopes is the space-separated scope list.
	Scopes string

	// Device-code login fields (OpenAI Codex deviceauth only; empty for a vendor
	// without a device flow, such as Anthropic). Kept overridable here so a live
	// operator can correct a rotated path and the unit tests can point them at an
	// httptest server. REVERSE-ENGINEERED / VERIFY LIVE.
	//
	// DeviceUsercodeURL starts a device login (the usercode endpoint).
	DeviceUsercodeURL string
	// DeviceTokenURL is polled for the authorization (the deviceauth token
	// endpoint, distinct from TokenURL).
	DeviceTokenURL string
	// DeviceVerificationURL is the human verification page returned to the UI for
	// display; this package never fetches it.
	DeviceVerificationURL string
	// DeviceCallbackRedirect is the redirect_uri for the device code exchange.
	DeviceCallbackRedirect string
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
// VERIFY LIVE.
func DefaultOpenAIEndpoints() Endpoints {
	return Endpoints{
		ClientID:               OpenAIClientID,
		AuthorizeURL:           OpenAIAuthorizeURL,
		TokenURL:               OpenAITokenURL,
		RedirectURI:            OpenAIRedirectURI,
		Scopes:                 OpenAIScopes,
		DeviceUsercodeURL:      OpenAIDeviceUsercodeURL,
		DeviceTokenURL:         OpenAIDeviceTokenURL,
		DeviceVerificationURL:  OpenAIDeviceVerificationURL,
		DeviceCallbackRedirect: OpenAIDeviceCallbackRedirect,
	}
}
