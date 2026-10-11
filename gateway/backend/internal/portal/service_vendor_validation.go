// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"fmt"
	"net/http"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/capture"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/vendorauth"
	"strings"
	"time"
)

// Credential validation: telling a working vendor credential from a dead one
// without naming a model (so a wrong catalog entry can never masquerade as an
// auth failure). The probes themselves live in internal/vendorauth; this file
// wires them into two places:
//
//   - ConnectVendorAccountImport runs the matching SUBSCRIPTION probe once before
//     it persists a pasted token set. The policy is fail-soft: only a definitive
//     StatusInvalid blocks the import; StatusUnverifiable (vendor unreachable, rate
//     limited, an unexpected answer) and StatusValid proceed, so a probe that
//     cannot run never blocks a user.
//   - TestVendorAccountConnection is the explicit "test connection" action: it
//     opens the account's stored credential and reports the verdict.
//
// No credential ever leaves this file in an error, a log line or a result: the
// VendorConnectionCheck carries a status, a short vendor-status phrase and a time.

const (
	// vendorValidationHTTPTimeout bounds every credential probe. A probe runs only
	// at import and on the explicit test action, never on a request path.
	vendorValidationHTTPTimeout = 10 * time.Second

	// maxVendorIdentityLen caps a vendor-reported account id or plan type before it
	// is stored. Real values are UUIDs and short plan names; the cap exists because
	// the probe's body is untrusted vendor input and the account id is later sent
	// back as the chatgpt-account-id request header.
	maxVendorIdentityLen = 128
	// printableASCIIMin / printableASCIIMax bound the bytes a stored identity may
	// carry: space through tilde. Everything else (control characters including CR
	// and LF, DEL, any non-ASCII byte) is refused.
	printableASCIIMin = 0x20
	printableASCIIMax = 0x7e

	// redactedCredential replaces a credential a probe's detail would echo.
	redactedCredential = "[redacted]"
)

// The wire values of VendorConnectionCheck.Status.
const (
	VendorConnectionValid        = "valid"
	VendorConnectionInvalid      = "invalid"
	VendorConnectionUnverifiable = "unverifiable"
)

// VendorConnectionCheck is the result of TestVendorAccountConnection: the
// credential-free verdict on an account's stored credential. Status is one of
// VendorConnectionValid / Invalid / Unverifiable. Detail is a short human-readable
// phrase (the vendor's HTTP status and error code, or what was missing) and never
// contains a credential. "unverifiable" is no statement about the credential: the
// vendor could not be reached, or there was nothing to test.
type VendorConnectionCheck struct {
	Status    string    `json:"status"`
	Detail    string    `json:"detail"`
	CheckedAt time.Time `json:"checked_at"`
}

// VendorCredentialValidator is one credential probe: the vendor's verdict on a
// credential (an api key or an OAuth access token), obtained with the given
// bounded client. The vendorauth.Validate* functions have exactly this shape.
type VendorCredentialValidator func(ctx context.Context, httpClient *http.Client, credential string) vendorauth.CredentialCheck

// VendorOpenAICompatibleValidator is the api-key probe of an OpenAI-compatible
// provider. Unlike VendorCredentialValidator it takes the probe URL: the portal
// composes it from the provider preset and the account's base URL, because the
// vendorauth package knows no vendor roots. vendorauth.ValidateOpenAICompatibleAPIKey
// has exactly this shape.
type VendorOpenAICompatibleValidator func(ctx context.Context, httpClient *http.Client, probeURL, apiKey string) vendorauth.CredentialCheck

// VendorCredentialValidators is the seam over the vendorauth probes: tests inject
// fakes so no service test reaches a vendor over the network. A nil field means
// the real probe (see ServiceDeps.VendorValidators).
type VendorCredentialValidators struct {
	OpenAISubscription    VendorCredentialValidator
	AnthropicSubscription VendorCredentialValidator
	OpenAIAPIKey          VendorCredentialValidator
	AnthropicAPIKey       VendorCredentialValidator
	// OpenAICompatibleAPIKey probes the api key of every OpenAI-compatible vendor
	// (x.ai, OpenRouter, Google, the Custom endpoint) at a preset-derived URL.
	OpenAICompatibleAPIKey VendorOpenAICompatibleValidator
}

// withDefaults returns v with every nil probe replaced by its vendorauth function.
func (v VendorCredentialValidators) withDefaults() VendorCredentialValidators {
	if v.OpenAISubscription == nil {
		v.OpenAISubscription = vendorauth.ValidateOpenAISubscription
	}
	if v.AnthropicSubscription == nil {
		v.AnthropicSubscription = vendorauth.ValidateAnthropicSubscription
	}
	if v.OpenAIAPIKey == nil {
		v.OpenAIAPIKey = vendorauth.ValidateOpenAIAPIKey
	}
	if v.AnthropicAPIKey == nil {
		v.AnthropicAPIKey = vendorauth.ValidateAnthropicAPIKey
	}
	if v.OpenAICompatibleAPIKey == nil {
		v.OpenAICompatibleAPIKey = vendorauth.ValidateOpenAICompatibleAPIKey
	}
	return v
}

// vendorValidationState is the credential validation's configuration: the probes
// and the bounded http client they share. Held as a single field on Service
// (Service.vendorValidation).
type vendorValidationState struct {
	validators VendorCredentialValidators
	client     *http.Client
}

func newVendorValidationState(validators VendorCredentialValidators) vendorValidationState {
	return vendorValidationState{
		validators: validators.withDefaults(),
		client:     &http.Client{Timeout: vendorValidationHTTPTimeout},
	}
}

// unverifiableVendorCheck is the verdict for "nothing could be tested" (an unknown
// vendor, a missing credential): it warns and never blocks.
func unverifiableVendorCheck(detail string) vendorauth.CredentialCheck {
	return vendorauth.CredentialCheck{Status: vendorauth.StatusUnverifiable, Detail: detail}
}

// validateSubscriptionToken runs vendor's SUBSCRIPTION probe on an OAuth access
// token. A vendor with no probe is Unverifiable.
func (s *Service) validateSubscriptionToken(ctx context.Context, vendor, accessToken string) vendorauth.CredentialCheck {
	var probe VendorCredentialValidator
	switch vendor {
	case routing.VendorOpenAI:
		probe = s.vendorValidation.validators.OpenAISubscription
	case routing.VendorAnthropic:
		probe = s.vendorValidation.validators.AnthropicSubscription
	}
	if probe == nil {
		return unverifiableVendorCheck("no credential check exists for this vendor")
	}
	return probe(ctx, s.vendorValidation.client, accessToken)
}

// checkSubscriptionTokenSet probes ts's access token (validateSubscriptionToken),
// EXCEPT when that token is already past its expiry while a refresh token can
// renew it: the vendor would answer 401 for an account the next request heals (the
// dispatch refreshes lazily), and a verdict of "invalid" would be a false alarm
// that blocks a working import or reads as a dead credential. Such a token set is
// Unverifiable without a probe. Without a refresh token an expired token can never
// heal, so it is probed like any other. The one place this rule lives, shared by
// the token import and TestVendorAccountConnection.
func (s *Service) checkSubscriptionTokenSet(ctx context.Context, vendor string, ts vendorauth.TokenSet) vendorauth.CredentialCheck {
	if ts.RefreshToken != "" && ts.NeedsRefresh(s.clock(), 0) {
		return unverifiableVendorCheck("the access token has expired; it is refreshed automatically on the next request")
	}
	return s.validateSubscriptionToken(ctx, vendor, ts.AccessToken)
}

// validateAPIKey runs acc's vendor's API-KEY probe. An OpenAI-compatible vendor
// is probed per its preset (validateOpenAICompatibleAPIKey); openai and anthropic
// keep their own fixed-URL probes.
func (s *Service) validateAPIKey(ctx context.Context, acc routing.VendorAccount, apiKey string) vendorauth.CredentialCheck {
	if routing.IsOpenAICompatibleVendor(acc.Vendor) {
		return s.validateOpenAICompatibleAPIKey(ctx, acc, apiKey)
	}
	var probe VendorCredentialValidator
	switch acc.Vendor {
	case routing.VendorOpenAI:
		probe = s.vendorValidation.validators.OpenAIAPIKey
	case routing.VendorAnthropic:
		probe = s.vendorValidation.validators.AnthropicAPIKey
	}
	if probe == nil {
		return unverifiableVendorCheck("no credential check exists for this vendor")
	}
	return probe(ctx, s.vendorValidation.client, apiKey)
}

// validateOpenAICompatibleAPIKey probes the api key of an OpenAI-compatible
// account at the URL its preset prescribes (ValidateVia):
//
//   - "models": GET {base}{prefix}/models (x.ai, Google, Custom: listing needs a key)
//   - "key":    GET {base}{prefix}{ValidatePath} (OpenRouter: /models is public,
//     /key introspects the key)
//   - "none":   no probe at all (Kilo: public listing, no key endpoint), so the
//     answer is Unverifiable instead of a listing that calls every key valid.
//
// The classification (2xx valid, 401 invalid, anything else unverifiable) is the
// validator's and is the same for every preset. A provider that answers a bad key
// with 400 (x.ai, Gemini) therefore reads "could not verify", never "invalid".
func (s *Service) validateOpenAICompatibleAPIKey(ctx context.Context, acc routing.VendorAccount, apiKey string) vendorauth.CredentialCheck {
	preset, ok := routing.VendorPresetFor(acc.Vendor)
	if !ok {
		return unverifiableVendorCheck("no credential check exists for this vendor")
	}
	var probePath string
	switch preset.ValidateVia {
	case "none":
		return unverifiableVendorCheck("this provider offers no offline key check")
	case "key":
		probePath = preset.ValidatePath
	default: // "models"
		probePath = "/models"
	}
	probe := s.vendorValidation.validators.OpenAICompatibleAPIKey
	if probe == nil {
		return unverifiableVendorCheck("no credential check exists for this vendor")
	}
	probeURL := strings.TrimRight(acc.BaseURL, "/") + preset.PathPrefix + probePath
	return probe(ctx, s.vendorValidation.client, probeURL, apiKey)
}

// vendorIdentityValue returns raw when it is safe to store and to send back as a
// header value: 1..maxVendorIdentityLen bytes of printable ASCII with no leading
// or trailing space. Anything else (empty, over-long, a control character, DEL,
// non-ASCII) is refused, so a probe response that is junk or hostile is dropped
// instead of persisted.
func vendorIdentityValue(raw string) (string, bool) {
	if raw == "" || len(raw) > maxVendorIdentityLen || raw[0] == ' ' || raw[len(raw)-1] == ' ' {
		return "", false
	}
	for i := 0; i < len(raw); i++ {
		if raw[i] < printableASCIIMin || raw[i] > printableASCIIMax {
			return "", false
		}
	}
	return raw, true
}

// backfillOpenAIIdentity fills a token set's missing ChatGPT account id (and the
// matching plan) from a Valid OpenAI subscription verdict: a pasted access token
// that carries no chatgpt_account_id claim would otherwise be dispatched without
// the chatgpt-account-id header. The validator's strings are unvalidated vendor
// input, so each must pass vendorIdentityValue or it is left empty. An id already
// derived from the token wins, and the plan is taken only together with the id it
// belongs to (the probe's plan is the default account's, which need not be the
// token's account).
func backfillOpenAIIdentity(ts *vendorauth.TokenSet, check vendorauth.CredentialCheck) {
	if ts.AccountID != "" || check.Status != vendorauth.StatusValid {
		return
	}
	accountID, ok := vendorIdentityValue(check.AccountID)
	if !ok {
		return
	}
	ts.AccountID = accountID
	if ts.PlanType != "" {
		return
	}
	if plan, ok := vendorIdentityValue(check.PlanType); ok {
		ts.PlanType = plan
	}
}

// scrubCredential removes credential from detail. The vendorauth probes never
// echo a credential, so this is defence in depth: a result or an error built here
// must stay token-free even if a probe (or a test fake) misbehaves.
func scrubCredential(detail, credential string) string {
	if credential == "" {
		return detail
	}
	return strings.ReplaceAll(detail, credential, redactedCredential)
}

// vendorConnectionStatus maps a probe verdict to the wire status. The zero (never
// filled in) verdict is no statement about the credential, so it is unverifiable.
func vendorConnectionStatus(status vendorauth.CredentialStatus) string {
	switch status {
	case vendorauth.StatusValid:
		return VendorConnectionValid
	case vendorauth.StatusInvalid:
		return VendorConnectionInvalid
	default:
		return VendorConnectionUnverifiable
	}
}

// TestVendorAccountConnection is the explicit "test connection" action: it opens
// the account's stored credential (an api key, or the access token of the sealed
// token set), asks the vendor whether it is accepted and returns the verdict. It
// changes nothing and refreshes nothing. STRICTLY OWNER-ONLY, system scope
// included (it USES the stored credential, so it is authorized like a write, not
// like a metadata read); an unknown id and a stranger's account are both
// ErrVendorAccountNotFound. ErrVendorAccountsDisabled while the master flag is off.
//
// A missing credential (no api key set, subscription not connected) is
// "unverifiable" with a detail saying so, and so is an access token that is
// already past its expiry while a refresh token can renew it: the vendor would
// answer 401 for an account the next request heals, and "invalid" would be a false
// alarm. A stored credential that cannot be opened (a lost key, a corrupt blob) is
// ErrVendorAccountCredentialUnreadable, not a verdict.
func (s *Service) TestVendorAccountConnection(ctx context.Context, principal auth.Token, id string) (VendorConnectionCheck, error) {
	if err := s.requireVendorAccountsEnabled(ctx); err != nil {
		return VendorConnectionCheck{}, err
	}
	// write=true: strictly owner-only, system scope included. The test opens the
	// sealed credential and sends it to the vendor from the gateway, so letting a
	// non-owner trigger it would make the owner's personal credential a live/dead
	// oracle for someone else (and a vendor-side call the owner never made).
	acc, err := s.authorizeVendorAccount(ctx, principal, id, true)
	if err != nil {
		return VendorConnectionCheck{}, err
	}
	check, credential, err := s.checkVendorAccount(ctx, acc)
	if err != nil {
		return VendorConnectionCheck{}, err
	}
	return VendorConnectionCheck{
		Status:    vendorConnectionStatus(check.Status),
		Detail:    scrubCredential(check.Detail, credential),
		CheckedAt: s.clock().UTC(),
	}, nil
}

// unreadableVendorCredential marks err, the failure to OPEN an account's stored
// credential (what says which one: "api key" or "token set"), as
// ErrVendorAccountCredentialUnreadable. This is the READ path: it must not read as
// the write path's "an encryption key is required to store ..." refusal, so the
// capture.ErrKeyRequired of a lost key and the decrypt/decode error of a corrupt
// blob alike become the one sentinel. The cause stays in the chain for tests and
// logs; the API response carries only the sentinel's fixed, token-free message.
func unreadableVendorCredential(what string, err error) error {
	return fmt.Errorf("%w: open vendor account %s: %w", ErrVendorAccountCredentialUnreadable, what, err)
}

// openVendorAPIKey opens acc's sealed api key ("" when none is set). A key that
// cannot be opened is ErrVendorAccountCredentialUnreadable. Shared by the
// connection test and the model discovery, the two places that send the stored
// credential to a vendor.
func (s *Service) openVendorAPIKey(acc routing.VendorAccount) (string, error) {
	apiKey, err := capture.OpenSecret(s.cipher, acc.APIKey)
	if err != nil {
		return "", unreadableVendorCredential("api key", err)
	}
	return apiKey, nil
}

// openVendorTokenSet opens acc's sealed OAuth token set (the zero set when the
// subscription is not connected). A blob that cannot be opened is
// ErrVendorAccountCredentialUnreadable. Shared like openVendorAPIKey.
func (s *Service) openVendorTokenSet(acc routing.VendorAccount) (vendorauth.TokenSet, error) {
	ts, err := vendorauth.OpenTokenSet(s.cipher, acc.OAuthTokens)
	if err != nil {
		return vendorauth.TokenSet{}, unreadableVendorCredential("token set", err)
	}
	return ts, nil
}

// checkVendorAccount opens acc's stored credential and probes it. It returns the
// verdict and the credential it used (so the caller can scrub the detail). A
// credential that cannot be opened is ErrVendorAccountCredentialUnreadable.
func (s *Service) checkVendorAccount(ctx context.Context, acc routing.VendorAccount) (vendorauth.CredentialCheck, string, error) {
	switch acc.AuthType {
	case routing.VendorAuthAPIKey:
		apiKey, err := s.openVendorAPIKey(acc)
		if err != nil {
			return vendorauth.CredentialCheck{}, "", err
		}
		if apiKey == "" {
			return unverifiableVendorCheck("no API key is set"), "", nil
		}
		return s.validateAPIKey(ctx, acc, apiKey), apiKey, nil
	case routing.VendorAuthSubscription:
		ts, err := s.openVendorTokenSet(acc)
		if err != nil {
			return vendorauth.CredentialCheck{}, "", err
		}
		if ts.AccessToken == "" {
			return unverifiableVendorCheck("the subscription is not connected"), "", nil
		}
		return s.checkSubscriptionTokenSet(ctx, acc.Vendor, ts), ts.AccessToken, nil
	default:
		return unverifiableVendorCheck("no credential check exists for this account type"), "", nil
	}
}
