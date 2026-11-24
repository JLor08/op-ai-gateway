// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"errors"
	"log/slog"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/vendorauth"
	"sync"
	"time"
)

// The group-triggered usage pull. When a vendor account group skips a member for a
// limit whose reset time it does not know, the resolver asks for that member's
// usage through routing.VendorUsagePuller so the snapshot (and with it the reset
// time) is fresh the next time the group resolves. The call sits on the resolve
// path, so RequestVendorUsagePull only consults an in-memory throttle and starts a
// detached goroutine; the vendor is never asked on the request path.

const (
	// vendorGroupUsagePullInterval is the shortest time between two group-triggered
	// pulls of one account.
	vendorGroupUsagePullInterval = 15 * time.Minute
	// vendorGroupUsagePullMaxBackoff caps the wait after repeated 429s from the
	// usage endpoint, which rate-limits aggressively.
	vendorGroupUsagePullMaxBackoff = 4 * time.Hour
)

// vendorGroupPullEntry is one account's throttle state.
type vendorGroupPullEntry struct {
	// next is the earliest time a pull may start.
	next time.Time
	// failures counts consecutive rate-limited fetches; it doubles the wait.
	failures int
}

// vendorGroupPullState is the in-memory throttle of the group-triggered pull, safe
// for concurrent use; the zero value is ready. A restart forgets it, which at worst
// repeats one pull.
type vendorGroupPullState struct {
	mu       sync.Mutex
	accounts map[string]*vendorGroupPullEntry
	// inflight tracks the detached pull goroutines (test synchronization).
	inflight sync.WaitGroup
}

// vendorGroupPullWait is the wait after failures consecutive rate-limited fetches:
// the interval, doubled once per failure, capped.
func vendorGroupPullWait(failures int) time.Duration {
	wait := vendorGroupUsagePullInterval
	for range failures {
		wait *= 2
		if wait >= vendorGroupUsagePullMaxBackoff {
			return vendorGroupUsagePullMaxBackoff
		}
	}
	return wait
}

// claim reports whether a pull of id may start at now and, if so, reserves the
// slot: the next one is not allowed before the account's current wait is over.
func (st *vendorGroupPullState) claim(id string, now time.Time) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.accounts == nil {
		st.accounts = make(map[string]*vendorGroupPullEntry)
	}
	e := st.accounts[id]
	if e == nil {
		e = &vendorGroupPullEntry{}
		st.accounts[id] = e
	} else if now.Before(e.next) {
		return false
	}
	e.next = now.Add(vendorGroupPullWait(e.failures))
	return true
}

// rateLimited records that a fetch of id was throttled by the vendor at now: the
// wait doubles (up to the cap) and counts from now.
func (st *vendorGroupPullState) rateLimited(id string, now time.Time) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.accounts == nil {
		st.accounts = make(map[string]*vendorGroupPullEntry)
	}
	e := st.accounts[id]
	if e == nil {
		e = &vendorGroupPullEntry{}
		st.accounts[id] = e
	}
	e.failures++
	e.next = now.Add(vendorGroupPullWait(e.failures))
}

// succeeded resets id's backoff after a good fetch.
func (st *vendorGroupPullState) succeeded(id string, now time.Time) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if e := st.accounts[id]; e != nil {
		e.failures = 0
		e.next = now.Add(vendorGroupUsagePullInterval)
	}
}

// forget drops id's entry (the account was deleted).
func (st *vendorGroupPullState) forget(id string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	delete(st.accounts, id)
}

// noteAnthropicUsageOutcome feeds the result of an Anthropic usage fetch into the
// throttle and maps it to the discovery status the usage pull stores by. A
// rate-limited answer (vendorauth.ErrUsageRateLimited) backs the account off; a
// context that ended first says nothing about the vendor and changes nothing.
func (s *Service) noteAnthropicUsageOutcome(ctx context.Context, accountID string, err error) vendorauth.DiscoveryStatus {
	switch {
	case err == nil:
		s.vendorGroupPulls.succeeded(accountID, s.clock())
		return vendorauth.DiscoveryOK
	case errors.Is(err, vendorauth.ErrUsageRateLimited) && ctx.Err() == nil:
		s.vendorGroupPulls.rateLimited(accountID, s.clock())
	}
	return vendorauth.DiscoveryUnverifiable
}

// RequestVendorUsagePull implements routing.VendorUsagePuller. It never blocks and
// never touches the store or the vendor on the caller's goroutine: it asks the
// in-memory throttle (at most one pull per account per vendorGroupUsagePullInterval,
// longer after the usage endpoint answered 429) and, when a pull is due, runs it on
// a detached goroutine with its own bounded context. Whether the account has an
// active usage pull at all is decided there, so an account without one (an api-key
// account of a native vendor, an unknown id) costs one throttled goroutine at most.
func (s *Service) RequestVendorUsagePull(accountID string) {
	if accountID == "" || !s.vendorGroupPulls.claim(accountID, s.clock()) {
		return
	}
	s.vendorGroupPulls.inflight.Add(1)
	go func() {
		defer s.vendorGroupPulls.inflight.Done()
		timeout := s.vendorDiscovery.connectTimeout
		if timeout <= 0 {
			timeout = vendorConnectDiscoveryTimeout
		}
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		s.runVendorGroupPull(ctx, accountID)
	}()
}

// runVendorGroupPull pulls accountID's usage when it has an active pull: the same
// token handling and store write as the dedicated refresh (an expired token is
// renewed through the gateway's locked refresher, the snapshot is merged by
// fetchVendorUsage), without its authorization (the pull is system-initiated and
// reads nothing the account's owner could not). Fail-soft: every miss is logged at
// Debug and the stored snapshot is left as it was.
func (s *Service) runVendorGroupPull(ctx context.Context, accountID string) {
	acc, err := s.routes.VendorAccountByID(ctx, accountID)
	if err != nil {
		slog.Debug("vendor group usage pull skipped: account not readable", "account", accountID, "err", err)
		return
	}
	if !hasActiveVendorUsagePull(acc) {
		return
	}
	var ts vendorauth.TokenSet
	if acc.AuthType == routing.VendorAuthSubscription {
		var note string
		if ts, note, err = s.currentSubscriptionTokens(ctx, acc); err != nil || note != "" {
			slog.Debug("vendor group usage pull skipped: no usable token", "account", accountID, "note", note)
			return
		}
	}
	s.refreshVendorUsage(ctx, acc, ts)
}

// waitVendorGroupPulls blocks until every detached group pull has finished.
// Test-only.
func (s *Service) waitVendorGroupPulls() { s.vendorGroupPulls.inflight.Wait() }
