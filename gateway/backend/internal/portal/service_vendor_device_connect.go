// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"errors"
	"fmt"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/vendorauth"
	"sync"
	"time"
)

// Device-code connect: the OPTIONAL second way to attach an OpenAI (Codex)
// ChatGPT subscription, alongside the code-paste flow in
// service_vendor_connect.go. Unlike code-paste it needs no browser callback the
// gateway can see, so it works for a REMOTE gateway too: begin shows the user a
// short code and a verification page, the user approves it in any browser, and
// the portal polls the vendor until the approval lands.
//
//   - BeginVendorAccountDeviceConnect starts a device login and returns the
//     user_code + verification URL for the UI to display.
//   - PollVendorAccountDeviceConnect is ONE backend poll; the FRONTEND calls it
//     repeatedly until it reports connected.
//
// The whole vendor OAuth path is reverse-engineered and experimental -- see
// internal/vendorauth. This flow is OpenAI-only (Anthropic has no device login).

// vendorDeviceConnectPendingTTL is how long a begun device connect waits for the
// user to approve the code at the vendor before the pending entry is dropped. The
// Codex CLI polls for about 15 minutes.
const vendorDeviceConnectPendingTTL = 15 * time.Minute

// pendingVendorDeviceConnect is one begun, not yet connected, device login. It
// holds only what a poll needs: the vendor's device_auth_id and the user_code to
// present back on each poll. There is no PKCE secret here -- the device flow's
// code_verifier is minted by the vendor and only arrives with the authorization.
type pendingVendorDeviceConnect struct {
	deviceAuthID string
	userCode     string
	createdAt    time.Time
}

// vendorDeviceConnectState is the device flow's short-lived in-memory state. Like
// vendorConnectState the entries are deliberately NOT persisted: a restart that
// loses one costs the user a restart of the connect, nothing more. It holds no
// endpoints or http client of its own -- the device methods read the OpenAI
// endpoints and client from Service.vendorConnect. Held as a single non-pointer
// field (Service.vendorDeviceConnect); Service is always used via pointer, so mu
// is never copied.
type vendorDeviceConnectState struct {
	mu sync.Mutex
	// pending holds at most one entry per vendor account id; a new begin replaces
	// the account's previous one. Guarded by mu.
	pending map[string]pendingVendorDeviceConnect
}

// put stores the entry for accountID (replacing any previous one) and drops every
// entry past the TTL, so abandoned device connects cannot pile up.
func (c *vendorDeviceConnectState) put(accountID string, entry pendingVendorDeviceConnect, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for id, p := range c.pending {
		if vendorDeviceConnectExpired(p, now) {
			delete(c.pending, id)
		}
	}
	if c.pending == nil {
		c.pending = make(map[string]pendingVendorDeviceConnect)
	}
	c.pending[accountID] = entry
}

// get returns accountID's live entry; an expired one is dropped and reported
// missing.
func (c *vendorDeviceConnectState) get(accountID string, now time.Time) (pendingVendorDeviceConnect, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	p, ok := c.pending[accountID]
	if !ok {
		return pendingVendorDeviceConnect{}, false
	}
	if vendorDeviceConnectExpired(p, now) {
		delete(c.pending, accountID)
		return pendingVendorDeviceConnect{}, false
	}
	return p, true
}

// clear removes accountID's entry, but only while it is still the one identified
// by deviceAuthID: a begin that replaced it while a slow poll was in flight keeps
// its fresh entry.
func (c *vendorDeviceConnectState) clear(accountID, deviceAuthID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if p, ok := c.pending[accountID]; ok && p.deviceAuthID == deviceAuthID {
		delete(c.pending, accountID)
	}
}

// count reports the number of live-or-stale pending entries (test visibility).
func (c *vendorDeviceConnectState) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.pending)
}

func vendorDeviceConnectExpired(p pendingVendorDeviceConnect, now time.Time) bool {
	return now.Sub(p.createdAt) > vendorDeviceConnectPendingTTL
}

// deviceConnectableVendorAccount is connectableVendorAccount (master flag +
// owner-only + subscription-only) plus the device flow's OpenAI-only gate: a
// subscription account of any other vendor is ErrVendorAccountDeviceUnsupported.
// The checks run flag -> owner -> subscription -> vendor, so a disabled module is
// a 409, a stranger a 404, and a wrong auth type or vendor a 400.
func (s *Service) deviceConnectableVendorAccount(ctx context.Context, principal auth.Token, id string) (routing.VendorAccount, error) {
	acc, err := s.connectableVendorAccount(ctx, principal, id)
	if err != nil {
		return routing.VendorAccount{}, err
	}
	if acc.Vendor != routing.VendorOpenAI {
		return routing.VendorAccount{}, ErrVendorAccountDeviceUnsupported
	}
	return acc, nil
}

// classifyVendorDeviceError maps a vendorauth failure to the connect sentinels a
// device begin/poll reports: a vendor refusal is ErrVendorAccountConnectRejected
// (never a 401 -- the portal reads a 401 from this API as an expired session),
// anything else (network, 5xx, malformed reply) ErrVendorAccountConnectUpstream.
// The wrapped cause is a vendorauth error that carries no request or response body.
func classifyVendorDeviceError(err error) error {
	if errors.Is(err, vendorauth.ErrAuthRejected) {
		return fmt.Errorf("%w: %w", ErrVendorAccountConnectRejected, err)
	}
	return fmt.Errorf("%w: %w", ErrVendorAccountConnectUpstream, err)
}

// BeginVendorAccountDeviceConnect starts the Codex device login for an OpenAI
// subscription account and returns the user_code plus the verification page URL
// for the UI to display. It runs the pre-flight seal probe first, so a store that
// could not seal the eventual token set (a keyless disk store) fails here with
// ErrVendorAccountConnectKeyRequired, before the vendor round trip and before the
// user approves anything. A fresh begin replaces any previous pending entry for
// the account. A vendor refusal is ErrVendorAccountConnectRejected, any other
// vendor failure ErrVendorAccountConnectUpstream. OWNER-ONLY; OpenAI-only;
// ErrVendorAccountsDisabled while the master flag is off.
func (s *Service) BeginVendorAccountDeviceConnect(ctx context.Context, principal auth.Token, accountID string) (userCode, verificationURL string, err error) {
	acc, err := s.deviceConnectableVendorAccount(ctx, principal, accountID)
	if err != nil {
		return "", "", err
	}
	if err := s.requireVendorTokensSealable(); err != nil {
		return "", "", err
	}
	// The poll interval the vendor suggests is deliberately not surfaced: the
	// frontend owns the poll cadence (it calls the poll endpoint, not this package).
	deviceAuthID, code, verificationURL, _, err := vendorauth.OpenAIDeviceStart(ctx, s.vendorConnect.client, s.vendorConnect.openai)
	if err != nil {
		return "", "", classifyVendorDeviceError(err)
	}
	now := s.clock()
	s.vendorDeviceConnect.put(acc.ID, pendingVendorDeviceConnect{
		deviceAuthID: deviceAuthID,
		userCode:     code,
		createdAt:    now,
	}, now)
	return code, verificationURL, nil
}

// PollVendorAccountDeviceConnect performs ONE backend poll of a begun device
// login. It returns connected=false with a nil error while the user has not
// finished approving the code (the frontend keeps polling). When the vendor
// authorizes, it exchanges the code for tokens, seals them, marks the account an
// active subscription, clears the pending entry and returns connected=true. A
// vendor refusal or any other vendor failure (ErrVendorAccountConnectRejected /
// ErrVendorAccountConnectUpstream) is terminal: it clears the pending entry, so
// the user must begin again. With no device connect in progress (never begun or
// past the TTL) it is ErrVendorAccountDeviceConnectState. The response never
// carries a token. OWNER-ONLY; OpenAI-only; ErrVendorAccountsDisabled while the
// master flag is off.
func (s *Service) PollVendorAccountDeviceConnect(ctx context.Context, principal auth.Token, accountID string) (connected bool, err error) {
	acc, err := s.deviceConnectableVendorAccount(ctx, principal, accountID)
	if err != nil {
		return false, err
	}
	pending, ok := s.vendorDeviceConnect.get(acc.ID, s.clock())
	if !ok {
		return false, ErrVendorAccountDeviceConnectState
	}
	// The authorization code a successful poll returns is single-use, so make sure
	// the result can be sealed before the poll might spend it. A keyless store fails
	// here and KEEPS the pending entry, so fixing the key lets a later poll succeed.
	if err := s.requireVendorTokensSealable(); err != nil {
		return false, err
	}
	code, verifier, stillPending, err := vendorauth.OpenAIDevicePoll(ctx, s.vendorConnect.client, s.vendorConnect.openai, pending.deviceAuthID, pending.userCode)
	if err != nil {
		s.vendorDeviceConnect.clear(acc.ID, pending.deviceAuthID)
		return false, classifyVendorDeviceError(err)
	}
	if stillPending {
		return false, nil
	}
	ts, err := vendorauth.ExchangeOpenAIDeviceCode(ctx, s.vendorConnect.client, s.vendorConnect.openai, code, verifier)
	if err != nil {
		s.vendorDeviceConnect.clear(acc.ID, pending.deviceAuthID)
		return false, classifyVendorDeviceError(err)
	}
	// The poll and exchange are network round trips; re-load the account so a
	// rename or status change made meanwhile is not overwritten by the stale copy.
	acc, err = s.deviceConnectableVendorAccount(ctx, principal, accountID)
	if err != nil {
		return false, err
	}
	if _, err := s.persistVendorTokens(ctx, acc, ts); err != nil {
		return false, err
	}
	s.vendorDeviceConnect.clear(acc.ID, pending.deviceAuthID)
	return true, nil
}
