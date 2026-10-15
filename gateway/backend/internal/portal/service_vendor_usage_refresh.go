// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/routing"
	"sync"
	"time"
)

// The dedicated usage refresh: the "Usage & Limits" panel asks for an account's usage
// snapshot on its own, decoupled from the model discovery the models refresh runs
// first. Two account kinds have an active pull: an OpenAI subscription
// (fetchVendorUsage) and an OpenRouter/Kilo api-key account
// (fetchCompatibleVendorUsage); both are shared with the models refresh. This file
// adds the owner-gated entry point and the in-memory rate limit that keeps a
// view-triggered pull from asking the vendor on every view.

// VendorUsageLazyTTL is the default maxAge of the on-view (lazy) usage refresh: an
// account whose last active pull is younger than this is answered from the stored
// snapshot instead of asking the vendor again. The HTTP handler passes it for a
// plain call (hence exported); a manual refresh passes 0 and always asks.
const VendorUsageLazyTTL = 5 * time.Minute

// The status vocabulary of VendorUsageRefreshResult, on the wire as refresh.status.
const (
	// VendorUsageRefreshOK: the vendor answered and the snapshot was stored (merged
	// over the passive scrape's for an OpenAI subscription; as fetched for an
	// api-key account, whose pull is its only usage writer).
	VendorUsageRefreshOK = "ok"
	// VendorUsageRefreshFresh: the last active pull was younger than the requested
	// maxAge, so the vendor was NOT asked and the stored snapshot is answered as it
	// is. That attempt may have failed, so the snapshot can be older than the TTL (or
	// absent): "fresh" says the pull was skipped, not that the data is current.
	VendorUsageRefreshFresh = "fresh"
	// VendorUsageRefreshUnverifiable: nothing usable could be had (no usable token or
	// API key, the vendor unreachable or answering with an error or an unusable body,
	// or the snapshot could not be stored). The stored snapshot is kept and answered.
	// Never a statement about the credential.
	VendorUsageRefreshUnverifiable = "unverifiable"
	// VendorUsageRefreshUnsupported: the account has no active usage pull (only an
	// OpenAI subscription and an OpenRouter/Kilo api-key account have one). No
	// credential was opened.
	VendorUsageRefreshUnsupported = "unsupported"
)

// VendorUsageRefreshResult is the credential-free outcome of RefreshVendorAccountUsage.
// Status is one of the VendorUsageRefresh* values; Detail is a short English phrase
// (what happened, or why nothing was pulled) and never contains a credential or
// vendor text.
type VendorUsageRefreshResult struct {
	Status string `json:"status"`
	Detail string `json:"detail"`
}

// vendorUsageRefreshDetail is the detail of a status that has no more specific
// reason than the status itself.
func vendorUsageRefreshDetail(status string) string {
	switch status {
	case VendorUsageRefreshOK:
		return "usage refreshed from the vendor"
	case VendorUsageRefreshFresh:
		return "usage was refreshed recently; the stored snapshot was not re-fetched"
	case VendorUsageRefreshUnsupported:
		return "this account has no usage endpoint the gateway can ask"
	default:
		return "the vendor did not answer with usable usage data; the stored usage is kept"
	}
}

// vendorUsagePullTracker remembers, per account id, when the vendor was last asked
// for the account's usage (an ACTIVE pull: the dedicated refresh, or the pull the
// models refresh runs on the way; the passive header scrape is not one). It is
// in-memory only -- a restart forgets it, which at worst repeats one pull -- and
// safe for concurrent use. The zero value is ready to use.
type vendorUsagePullTracker struct {
	mu   sync.Mutex
	last map[string]time.Time
}

// record notes that the vendor was asked for accountID's usage at at.
func (t *vendorUsagePullTracker) record(accountID string, at time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.last == nil {
		t.last = make(map[string]time.Time)
	}
	t.last[accountID] = at
}

// within reports whether accountID's last active pull is STRICTLY younger than
// maxAge at now. A non-positive maxAge is never fresh (the manual refresh), an
// account with no recorded pull is not fresh either, and neither is a pull that
// lies in the future of now: the wall clock stepped backward since (an NTP
// correction), and a negative age must not keep every later call "fresh".
func (t *vendorUsagePullTracker) within(accountID string, now time.Time, maxAge time.Duration) bool {
	if maxAge <= 0 {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	last, ok := t.last[accountID]
	age := now.Sub(last)
	return ok && age >= 0 && age < maxAge
}

// forget drops accountID's entry (the account was deleted).
func (t *vendorUsagePullTracker) forget(accountID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.last, accountID)
}

// vendorUsagePreset is the provider preset of acc when acc is an api_key account of a
// provider whose preset names a usage strategy (UsageVia: OpenRouter's per-key limit,
// Kilo's balance), so the registry, not a vendor list, says which api-key accounts
// have a usage pull. Any other account (a subscription, a native openai/anthropic key
// -- neither is in the preset registry -- or a provider without a strategy) is false.
func vendorUsagePreset(acc routing.VendorAccount) (routing.VendorPreset, bool) {
	if acc.AuthType != routing.VendorAuthAPIKey {
		return routing.VendorPreset{}, false
	}
	preset, ok := routing.VendorPresetFor(acc.Vendor)
	return preset, ok && preset.UsageVia != ""
}

// hasActiveVendorUsagePull reports whether acc has an active usage pull at all: an
// OpenAI subscription (the ChatGPT backend's usage endpoint) or an api_key account of
// a provider whose preset names a usage strategy (OpenRouter, Kilo; see
// vendorUsagePreset). A native openai/anthropic api_key account, the other
// OpenAI-compatible providers and an Anthropic subscription have none; an Anthropic
// subscription's snapshot only ever comes from the passive header scrape.
func hasActiveVendorUsagePull(acc routing.VendorAccount) bool {
	if acc.AuthType == routing.VendorAuthSubscription {
		return acc.Vendor == routing.VendorOpenAI
	}
	_, ok := vendorUsagePreset(acc)
	return ok
}

// RefreshVendorAccountUsage pulls an account's usage snapshot on its own (an OpenAI
// subscription's, or an OpenRouter/Kilo api-key account's), without the model
// discovery RefreshVendorAccountModels runs, and answers the stored snapshot (nil
// when none has been stored yet) with what the refresh did. It is what the Usage &
// Limits panel's refresh button and its on-view lazy pull call.
//
// maxAge is the lazy TTL. When it is positive and the account's last ACTIVE pull
// is strictly younger, the vendor is not asked: the stored snapshot is answered
// with status VendorUsageRefreshFresh. A manual refresh passes 0 and always asks.
// The lazy path's default is VendorUsageLazyTTL; the caller (the HTTP handler)
// chooses. The last-pull time is held in memory per account: an attempt counts
// whether the vendor answered or not (so a failing vendor is not hammered on every
// view), while a refresh that never reached it (no usable token or key, or a context
// that ended before the vendor answered) does not. Two concurrent calls for the same
// account may both ask the vendor; the write is idempotent.
//
// Statuses (VendorUsageRefreshResult.Status):
//   - ok: the vendor answered and the snapshot was stored (an OpenAI subscription's
//     merged over the passive scrape's, see routing.MergeVendorAccountUsage; an
//     api-key account's as fetched, because its pull is the only writer and a merge
//     could not express a limit that is gone).
//   - fresh: skipped by the TTL (above).
//   - unverifiable: fail-soft, never an error. No usable token (not connected, an
//     expired one that could not be renewed) or no API key set, the vendor
//     unreachable or answering with something unusable (Kilo's balance endpoint is
//     undocumented, so a miss there is expected), or a snapshot that could not be
//     stored. The stored snapshot is kept; Detail says why for a token or key
//     problem.
//   - unsupported: neither an OpenAI subscription nor an OpenRouter/Kilo api-key
//     account. No credential is opened.
//
// A subscription's expired-but-refreshable token is renewed first through the
// VendorTokenRefresher (the gateway's locked refresh); the portal never refreshes by
// itself. An api-key account has no token to renew: its stored key is opened and
// sent to the provider's usage endpoint (composed from the preset and the account's
// base URL).
//
// Authorized by authorizeVendorAccountOperate: the owner, or a CM while the account
// is shared/full. System scope is NOT let through: it opens the owner's sealed
// credential and sends it to the vendor from the gateway, so it is authorized like a
// write (an unknown id and a stranger's account are both ErrVendorAccountNotFound).
// ErrVendorAccountsDisabled while the master flag is off. The only errors besides
// those are a stored credential that cannot be opened
// (ErrVendorAccountCredentialUnreadable) and a store failure.
func (s *Service) RefreshVendorAccountUsage(ctx context.Context, principal auth.Token, id string, maxAge time.Duration) (*VendorAccountUsageDTO, VendorUsageRefreshResult, error) {
	if err := s.requireVendorAccountsEnabled(ctx); err != nil {
		return nil, VendorUsageRefreshResult{}, err
	}
	acc, err := s.loadVendorAccount(ctx, id)
	if err != nil {
		return nil, VendorUsageRefreshResult{}, err
	}
	if err := s.authorizeVendorAccountOperate(ctx, principal, acc); err != nil {
		return nil, VendorUsageRefreshResult{}, err
	}
	if !hasActiveVendorUsagePull(acc) {
		return s.vendorUsageRefreshAnswer(ctx, acc.ID, VendorUsageRefreshUnsupported, "")
	}
	if s.vendorUsagePulls.within(acc.ID, s.clock(), maxAge) {
		return s.vendorUsageRefreshAnswer(ctx, acc.ID, VendorUsageRefreshFresh, "")
	}
	// An api-key account has no OAuth token set: branch BEFORE currentSubscriptionTokens,
	// which would answer "not connected" for it.
	if acc.AuthType == routing.VendorAuthAPIKey {
		apiKey, note, err := s.currentUsageAPIKey(acc)
		if err != nil {
			return nil, VendorUsageRefreshResult{}, err
		}
		if note != "" {
			return s.vendorUsageRefreshAnswer(ctx, acc.ID, VendorUsageRefreshUnverifiable, note)
		}
		return s.vendorUsageRefreshAnswer(ctx, acc.ID, s.fetchCompatibleVendorUsage(ctx, acc, apiKey), "")
	}
	ts, note, err := s.currentSubscriptionTokens(ctx, acc)
	if err != nil {
		return nil, VendorUsageRefreshResult{}, err
	}
	if note != "" {
		return s.vendorUsageRefreshAnswer(ctx, acc.ID, VendorUsageRefreshUnverifiable, note)
	}
	return s.vendorUsageRefreshAnswer(ctx, acc.ID, s.fetchVendorUsage(ctx, acc, ts), "")
}

// noVendorAPIKeyUsageNote is the note for an api-key account whose key is not set.
const noVendorAPIKeyUsageNote = "no API key is set"

// currentUsageAPIKey opens an api-key account's sealed key for a usage pull. A
// non-empty note (credential-free) means nothing can be asked (no key is set) and
// apiKey is then empty; an error is a stored key that cannot be opened
// (ErrVendorAccountCredentialUnreadable), exactly as for a subscription's token set.
func (s *Service) currentUsageAPIKey(acc routing.VendorAccount) (apiKey, note string, err error) {
	apiKey, err = s.openVendorAPIKey(acc)
	if err != nil {
		return "", "", err
	}
	if apiKey == "" {
		return "", noVendorAPIKeyUsageNote, nil
	}
	return apiKey, "", nil
}

// vendorUsageRefreshAnswer is RefreshVendorAccountUsage's answer for status: the
// stored snapshot as it is NOW (after the pull, when there was one) and the result
// with detail, or the status' own phrase when detail is empty.
func (s *Service) vendorUsageRefreshAnswer(ctx context.Context, accountID, status, detail string) (*VendorAccountUsageDTO, VendorUsageRefreshResult, error) {
	usage, err := s.vendorAccountUsageDTO(ctx, accountID)
	if err != nil {
		return nil, VendorUsageRefreshResult{}, err
	}
	if detail == "" {
		detail = vendorUsageRefreshDetail(status)
	}
	return usage, VendorUsageRefreshResult{Status: status, Detail: detail}, nil
}
