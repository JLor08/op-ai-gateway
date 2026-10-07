// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/capture"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/store"
	"op-ai-gateway/internal/vendorauth"
	"strings"
	"sync"
	"time"
)

// Subscription connect: how a user attaches a consumer subscription (Anthropic
// Claude Pro/Max, OpenAI ChatGPT/Codex) to their OWN vendor account. Two paths
// end in the same place -- a sealed vendorauth.TokenSet in the account's
// OAuthTokens, with the account active:
//
//   - ConnectVendorAccountImport: the user pastes tokens they already hold (for
//     example from a CLI's credential file).
//   - BeginVendorAccountConnect + CompleteVendorAccountConnect: the OAuth
//     code-paste flow. Begin returns the vendor authorize URL; the user signs in
//     there and pastes the code the vendor shows back.
//
// The whole vendor OAuth path is reverse-engineered and experimental -- see
// internal/vendorauth. Nothing here refreshes a token or serves a request.

const (
	// vendorConnectPendingTTL is how long a begun connect waits for its paste.
	vendorConnectPendingTTL = 10 * time.Minute
	// vendorConnectHTTPTimeout bounds one token-endpoint exchange.
	vendorConnectHTTPTimeout = 30 * time.Second
)

// pendingVendorConnect is one begun, not yet completed, code-paste connect.
// verifier is the PKCE secret: it never leaves the process.
type pendingVendorConnect struct {
	verifier  string
	state     string
	vendor    string
	createdAt time.Time
}

// vendorConnectState is the connect flow's configuration and its short-lived
// state. The pending entries are deliberately NOT persisted: they live only in
// this process for vendorConnectPendingTTL, and a gateway restart that loses
// one costs the user a retry, nothing more. It is held as a single non-pointer
// field (Service.vendorConnect): Service is always used via pointer, so mu is
// never copied.
type vendorConnectState struct {
	anthropic vendorauth.Endpoints
	openai    vendorauth.Endpoints
	client    *http.Client

	mu sync.Mutex
	// pending holds at most one entry per vendor account id; a new begin replaces
	// the account's previous one. Guarded by mu.
	pending map[string]pendingVendorConnect
}

// endpoints returns the OAuth endpoints for vendor.
func (c *vendorConnectState) endpoints(vendor string) (vendorauth.Endpoints, bool) {
	switch vendor {
	case routing.VendorAnthropic:
		return c.anthropic, true
	case routing.VendorOpenAI:
		return c.openai, true
	}
	return vendorauth.Endpoints{}, false
}

// put stores the entry for accountID (replacing any previous one) and drops
// every entry that has outlived the TTL, so abandoned connects cannot pile up.
func (c *vendorConnectState) put(accountID string, entry pendingVendorConnect, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for id, p := range c.pending {
		if vendorConnectExpired(p, now) {
			delete(c.pending, id)
		}
	}
	if c.pending == nil {
		c.pending = make(map[string]pendingVendorConnect)
	}
	c.pending[accountID] = entry
}

// get returns accountID's live entry; an expired one is dropped and reported
// missing.
func (c *vendorConnectState) get(accountID string, now time.Time) (pendingVendorConnect, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	p, ok := c.pending[accountID]
	if !ok {
		return pendingVendorConnect{}, false
	}
	if vendorConnectExpired(p, now) {
		delete(c.pending, accountID)
		return pendingVendorConnect{}, false
	}
	return p, true
}

// clear removes accountID's entry, but only while it is still the one identified
// by state: a begin that replaced it while a slow exchange was in flight keeps
// its fresh entry.
func (c *vendorConnectState) clear(accountID, state string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if p, ok := c.pending[accountID]; ok && p.state == state {
		delete(c.pending, accountID)
	}
}

func vendorConnectExpired(p pendingVendorConnect, now time.Time) bool {
	return now.Sub(p.createdAt) > vendorConnectPendingTTL
}

// connectableVendorAccount is the gate every connect method runs: the master
// flag first, then OWNER-ONLY authorization (a stranger and an unknown id are
// both ErrVendorAccountNotFound, even for a body that is invalid anyway), then
// the subscription-only check. It returns the freshly loaded account.
func (s *Service) connectableVendorAccount(ctx context.Context, principal auth.Token, id string) (routing.VendorAccount, error) {
	if err := s.requireVendorAccountsEnabled(ctx); err != nil {
		return routing.VendorAccount{}, err
	}
	acc, err := s.authorizeVendorAccount(ctx, principal, id, true)
	if err != nil {
		return routing.VendorAccount{}, err
	}
	if acc.AuthType != routing.VendorAuthSubscription {
		return routing.VendorAccount{}, ErrVendorAccountNotSubscription
	}
	return acc, nil
}

// persistVendorTokens seals ts into acc's OAuthTokens, marks the account an
// active subscription account and writes it. The token set is sealed BEFORE the
// store write, so a keyless disk store fails with ErrVendorAccountConnectKeyRequired
// and persists nothing (never plaintext).
func (s *Service) persistVendorTokens(ctx context.Context, acc routing.VendorAccount, ts vendorauth.TokenSet) (VendorAccountDTO, error) {
	sealed, err := vendorauth.SealTokenSet(s.cipher, s.settingsVolatile, ts)
	if err != nil {
		if errors.Is(err, capture.ErrKeyRequired) {
			return VendorAccountDTO{}, fmt.Errorf("%w: %w", ErrVendorAccountConnectKeyRequired, err)
		}
		return VendorAccountDTO{}, err
	}
	acc.OAuthTokens = sealed
	acc.AuthType = routing.VendorAuthSubscription
	acc.Status = routing.VendorAccountStatusActive
	acc.UpdatedAt = s.clock().UTC()
	if err := s.routes.UpdateVendorAccount(ctx, acc); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return VendorAccountDTO{}, ErrVendorAccountNotFound
		}
		return VendorAccountDTO{}, err
	}
	return s.vendorAccountDTO(ctx, acc)
}

// ConnectVendorAccountImport connects a subscription account from tokens the
// user already holds. Only the access token is required; the refresh token and
// the expiry (the zero time = unknown) are optional. There is deliberately no
// live probe: the first real request validates the tokens. The write is
// OWNER-ONLY and the response is the credential-free DTO
// (SubscriptionConnected=true, never a token). ErrVendorAccountsDisabled while
// the master flag is off.
func (s *Service) ConnectVendorAccountImport(ctx context.Context, principal auth.Token, accountID, access, refresh string, expiresAt time.Time) (VendorAccountDTO, error) {
	acc, err := s.connectableVendorAccount(ctx, principal, accountID)
	if err != nil {
		return VendorAccountDTO{}, err
	}
	access = strings.TrimSpace(access)
	if access == "" {
		return VendorAccountDTO{}, ErrVendorAccountConnectTokenRequired
	}
	ts := vendorauth.TokenSet{AccessToken: access, RefreshToken: strings.TrimSpace(refresh)}
	if !expiresAt.IsZero() {
		ts.ExpiresAt = expiresAt.UTC()
	}
	return s.persistVendorTokens(ctx, acc, ts)
}

// BeginVendorAccountConnect starts the OAuth code-paste flow for a subscription
// account and returns the vendor authorize URL the portal opens. It generates a
// fresh PKCE verifier and state, keeps them in memory (see vendorConnectState)
// and puts only the challenge and the state on the URL. A second begin for the
// same account replaces the first. OWNER-ONLY; ErrVendorAccountsDisabled while
// the master flag is off.
func (s *Service) BeginVendorAccountConnect(ctx context.Context, principal auth.Token, accountID string) (string, error) {
	acc, err := s.connectableVendorAccount(ctx, principal, accountID)
	if err != nil {
		return "", err
	}
	ep, ok := s.vendorConnect.endpoints(acc.Vendor)
	if !ok {
		return "", ErrVendorAccountVendorInvalid
	}
	verifier, _, err := vendorauth.GeneratePKCE()
	if err != nil {
		return "", err
	}
	state, err := vendorauth.RandomState()
	if err != nil {
		return "", err
	}
	var authorizeURL string
	switch acc.Vendor {
	case routing.VendorAnthropic:
		authorizeURL = vendorauth.BuildAnthropicAuthorizeURL(ep, verifier, state)
	case routing.VendorOpenAI:
		authorizeURL = vendorauth.BuildOpenAIAuthorizeURL(ep, verifier, state)
	}
	now := s.clock()
	s.vendorConnect.put(acc.ID, pendingVendorConnect{
		verifier:  verifier,
		state:     state,
		vendor:    acc.Vendor,
		createdAt: now,
	}, now)
	return authorizeURL, nil
}

// CompleteVendorAccountConnect finishes the code-paste flow: codeAndState is
// whatever the user pasted back -- a bare code, Anthropic's "code#state", or
// (OpenAI lands on a dead loopback URL) the whole callback URL. When a state is
// present it MUST equal the one begin issued; a bare code is exchanged with the
// pending entry's own state. The exchange uses the stored PKCE verifier; on
// success the sealed tokens are written, the account is set active and the
// pending entry is cleared.
//
// A vendor refusal is ErrVendorAccountConnectRejected, any other vendor failure
// ErrVendorAccountConnectUpstream; both leave the account untouched and keep the
// pending entry, so a mistyped paste can simply be tried again (until the TTL).
// A missing or expired entry, or a state mismatch, is ErrVendorAccountConnectState.
// OWNER-ONLY; ErrVendorAccountsDisabled while the master flag is off.
func (s *Service) CompleteVendorAccountConnect(ctx context.Context, principal auth.Token, accountID, codeAndState string) (VendorAccountDTO, error) {
	acc, err := s.connectableVendorAccount(ctx, principal, accountID)
	if err != nil {
		return VendorAccountDTO{}, err
	}
	code, state := splitPastedConnectCode(codeAndState)
	if code == "" {
		return VendorAccountDTO{}, ErrVendorAccountConnectCodeRequired
	}
	pending, ok := s.vendorConnect.get(acc.ID, s.clock())
	if !ok || pending.vendor != acc.Vendor {
		return VendorAccountDTO{}, ErrVendorAccountConnectState
	}
	if state == "" {
		state = pending.state
	} else if state != pending.state {
		return VendorAccountDTO{}, ErrVendorAccountConnectState
	}
	ts, err := s.exchangeVendorCode(ctx, acc.Vendor, code, pending.verifier, state)
	if err != nil {
		return VendorAccountDTO{}, err
	}
	// The exchange is a network round trip; re-load the account so a rename or a
	// status change made meanwhile is not overwritten by the copy loaded above.
	acc, err = s.connectableVendorAccount(ctx, principal, accountID)
	if err != nil {
		return VendorAccountDTO{}, err
	}
	dto, err := s.persistVendorTokens(ctx, acc, ts)
	if err != nil {
		return VendorAccountDTO{}, err
	}
	s.vendorConnect.clear(acc.ID, pending.state)
	return dto, nil
}

// exchangeVendorCode trades the authorization code for a token set at the
// vendor and classifies a failure: a refusal of the code is
// ErrVendorAccountConnectRejected, anything else (network, 5xx, malformed reply)
// ErrVendorAccountConnectUpstream. The wrapped cause is a vendorauth error that
// never carries the request body or the raw response body.
func (s *Service) exchangeVendorCode(ctx context.Context, vendor, code, verifier, state string) (vendorauth.TokenSet, error) {
	ep, ok := s.vendorConnect.endpoints(vendor)
	if !ok {
		return vendorauth.TokenSet{}, ErrVendorAccountVendorInvalid
	}
	var (
		ts  vendorauth.TokenSet
		err error
	)
	switch vendor {
	case routing.VendorAnthropic:
		ts, err = vendorauth.ExchangeAnthropicCode(ctx, s.vendorConnect.client, ep, code, verifier, state)
	case routing.VendorOpenAI:
		ts, err = vendorauth.ExchangeOpenAICode(ctx, s.vendorConnect.client, ep, code, verifier)
	}
	if err != nil {
		if errors.Is(err, vendorauth.ErrAuthRejected) {
			return vendorauth.TokenSet{}, fmt.Errorf("%w: %w", ErrVendorAccountConnectRejected, err)
		}
		return vendorauth.TokenSet{}, fmt.Errorf("%w: %w", ErrVendorAccountConnectUpstream, err)
	}
	return ts, nil
}

// splitPastedConnectCode extracts the authorization code and the state (empty
// when the paste carries none) from what the user pasted: an absolute callback
// URL (code and state taken from its query), Anthropic's "code#state", or a bare
// code. Surrounding whitespace is dropped.
func splitPastedConnectCode(raw string) (code, state string) {
	raw = strings.TrimSpace(raw)
	if u, err := url.Parse(raw); err == nil && u.Scheme != "" && u.Host != "" {
		q := u.Query()
		return strings.TrimSpace(q.Get("code")), strings.TrimSpace(q.Get("state"))
	}
	code, state, _ = strings.Cut(raw, "#")
	return strings.TrimSpace(code), strings.TrimSpace(state)
}
