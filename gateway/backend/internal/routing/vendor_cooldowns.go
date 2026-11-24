// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package routing

import (
	"sort"
	"sync"
	"time"
)

// DefaultVendorLimitCooldown is how long an account is skipped for a model after
// the vendor answered it with a limit error (429 or 402).
const DefaultVendorLimitCooldown = 60 * time.Second

// VendorCooldown is one active limit cooldown of an account.
type VendorCooldown struct {
	UpstreamModel string
	Until         time.Time
}

type vendorCooldownKey struct{ accountID, model string }

// VendorLimitCooldowns is the in-memory registry of limit cooldowns per
// (account, upstream model). It is process-local and not persisted. A nil
// registry is safe: Set is a no-op and reads find nothing.
type VendorLimitCooldowns struct {
	mu sync.Mutex
	m  map[vendorCooldownKey]time.Time
}

// NewVendorLimitCooldowns returns an empty registry.
func NewVendorLimitCooldowns() *VendorLimitCooldowns {
	return &VendorLimitCooldowns{m: map[vendorCooldownKey]time.Time{}}
}

// Set records a cooldown for (accountID, upstreamModel) lasting until `until`.
func (c *VendorLimitCooldowns) Set(accountID, upstreamModel string, until time.Time) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m[vendorCooldownKey{accountID, upstreamModel}] = until
}

// Until reports the end of an active cooldown. An expired entry reads as absent
// and is deleted.
func (c *VendorLimitCooldowns) Until(accountID, upstreamModel string, now time.Time) (time.Time, bool) {
	if c == nil {
		return time.Time{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	k := vendorCooldownKey{accountID, upstreamModel}
	until, ok := c.m[k]
	if !ok {
		return time.Time{}, false
	}
	if !until.After(now) {
		delete(c.m, k)
		return time.Time{}, false
	}
	return until, true
}

// Active lists the account's unexpired cooldowns sorted by model, deleting
// expired ones.
func (c *VendorLimitCooldowns) Active(accountID string, now time.Time) []VendorCooldown {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []VendorCooldown
	for k, until := range c.m {
		if k.accountID != accountID {
			continue
		}
		if !until.After(now) {
			delete(c.m, k)
			continue
		}
		out = append(out, VendorCooldown{UpstreamModel: k.model, Until: until})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UpstreamModel < out[j].UpstreamModel })
	return out
}
