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

// vendorCooldownEnds are the two ends of one cooldown. until is the end routing
// honours: it may come from a window reset scraped off the vendor's answer, so it
// can reveal that account's usage. public is built from the vendor's Retry-After or
// the default alone, never from a scraped reset: it is the end a viewer who is not
// entitled to the account's usage figures may be shown. Each expires on its own.
type vendorCooldownEnds struct {
	until, public time.Time
}

// expired reports whether both ends have passed at now.
func (e vendorCooldownEnds) expired(now time.Time) bool {
	return !e.until.After(now) && !e.public.After(now)
}

// VendorLimitCooldowns is the in-memory registry of limit cooldowns per
// (account, upstream model). It is process-local and not persisted. A nil
// registry is safe: Set is a no-op and reads find nothing.
//
// Each entry keeps two ends (vendorCooldownEnds): routing reads the precise one
// (Until, Active), a view for somebody not entitled to the account's usage reads
// the public one (ActivePublic). An entry is deleted once both ends have passed.
type VendorLimitCooldowns struct {
	mu sync.Mutex
	m  map[vendorCooldownKey]vendorCooldownEnds
}

// NewVendorLimitCooldowns returns an empty registry.
func NewVendorLimitCooldowns() *VendorLimitCooldowns {
	return &VendorLimitCooldowns{m: map[vendorCooldownKey]vendorCooldownEnds{}}
}

// Set records a cooldown for (accountID, upstreamModel): routing skips the pair
// until `until`; `public` is the end built from Retry-After or the default alone
// (never a scraped window reset), shown where the account's usage may not be.
func (c *VendorLimitCooldowns) Set(accountID, upstreamModel string, until, public time.Time) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m[vendorCooldownKey{accountID, upstreamModel}] = vendorCooldownEnds{until: until, public: public}
}

// Until reports the end of an active cooldown as routing honours it. A passed
// routing end reads as absent; the entry is deleted once its public end has
// passed too.
func (c *VendorLimitCooldowns) Until(accountID, upstreamModel string, now time.Time) (time.Time, bool) {
	if c == nil {
		return time.Time{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	k := vendorCooldownKey{accountID, upstreamModel}
	ends, ok := c.m[k]
	if !ok {
		return time.Time{}, false
	}
	if ends.expired(now) {
		delete(c.m, k)
	}
	if !ends.until.After(now) {
		return time.Time{}, false
	}
	return ends.until, true
}

// Active lists the account's cooldowns whose routing end is still ahead, sorted by
// model, with that end. Entries whose both ends have passed are deleted.
func (c *VendorLimitCooldowns) Active(accountID string, now time.Time) []VendorCooldown {
	return c.active(accountID, now, func(e vendorCooldownEnds) time.Time { return e.until })
}

// ActivePublic lists the account's cooldowns whose public end (Retry-After or the
// default, never a scraped reset) is still ahead, sorted by model, with that end.
// It is what a viewer who may not see the account's usage figures is shown.
// Entries whose both ends have passed are deleted.
func (c *VendorLimitCooldowns) ActivePublic(accountID string, now time.Time) []VendorCooldown {
	return c.active(accountID, now, func(e vendorCooldownEnds) time.Time { return e.public })
}

// active lists the account's cooldowns whose end (picked by end) lies after now.
func (c *VendorLimitCooldowns) active(accountID string, now time.Time, end func(vendorCooldownEnds) time.Time) []VendorCooldown {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []VendorCooldown
	for k, ends := range c.m {
		if k.accountID != accountID {
			continue
		}
		if ends.expired(now) {
			delete(c.m, k)
			continue
		}
		if until := end(ends); until.After(now) {
			out = append(out, VendorCooldown{UpstreamModel: k.model, Until: until})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UpstreamModel < out[j].UpstreamModel })
	return out
}
