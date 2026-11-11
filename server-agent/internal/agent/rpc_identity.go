// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package agent

import (
	"context"
	"net"
	"net/netip"
	"slices"
	"sync"
	"time"
)

const (
	// headResolveInterval is how often a head domain is re-resolved.
	headResolveInterval = 60 * time.Second
	// headResolveRetry is the retry cadence while a domain has no good
	// answer yet, so a member does not refuse its head for a full interval
	// after one failed lookup.
	headResolveRetry = 10 * time.Second
	// headResolveTick is how often the resolver checks which domains are due.
	headResolveTick = 5 * time.Second
	// headLookupTimeout bounds one lookup.
	headLookupTimeout = 5 * time.Second
)

// headLookup resolves a host name to its IPv4 addresses.
type headLookup func(ctx context.Context, host string) ([]netip.Addr, error)

func systemHeadLookup(ctx context.Context, host string) ([]netip.Addr, error) {
	ctx, cancel := context.WithTimeout(ctx, headLookupTimeout)
	defer cancel()
	return net.DefaultResolver.LookupNetIP(ctx, "ip4", host)
}

type headEntry struct {
	domain string
	ips    []netip.Addr
	next   time.Time // not resolved again before this
}

// headResolver resolves the head domain of members whose head is only known
// by name (a LAN head without a configured address). It keeps the last good
// answer: a failed lookup never shrinks the set, and a domain that never
// resolved has an empty set, which every consumer treats as "no head is
// allowed" (fail closed). IPs reads only the cache, so it is safe on a
// connection-accept path.
type headResolver struct {
	lookup   headLookup
	onChange func(specID string)
	now      func() time.Time

	mu   sync.Mutex
	ents map[string]*headEntry
}

// newHeadResolver builds a resolver; a nil lookup uses the system resolver.
// onChange (may be nil) is called, outside any lock, with the spec ID whenever
// a spec's resolved set changes.
func newHeadResolver(lookup headLookup, onChange func(specID string)) *headResolver {
	if lookup == nil {
		lookup = systemHeadLookup
	}
	return &headResolver{lookup: lookup, onChange: onChange, now: time.Now, ents: map[string]*headEntry{}}
}

// IPs returns the spec's last good resolved head addresses (empty when there
// is none).
func (r *headResolver) IPs(specID string) []netip.Addr {
	ips, _ := r.state(specID)
	return ips
}

// state returns the cached set and whether the spec has a tracked domain.
func (r *headResolver) state(specID string) ([]netip.Addr, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e := r.ents[specID]
	if e == nil {
		return nil, false
	}
	return slices.Clone(e.ips), true
}

// refresh tracks exactly the given spec->domain pairs and resolves those that
// are due. A spec whose domain changed starts over with an empty set.
func (r *headResolver) refresh(ctx context.Context, domains map[string]string) {
	now := r.now()
	r.mu.Lock()
	changed := r.dropUntrackedLocked(domains)
	due, moved := r.dueLocked(domains, now)
	changed = append(changed, moved...)
	r.mu.Unlock()

	for _, id := range due {
		if ctx.Err() != nil {
			break
		}
		if r.resolve(ctx, id, domains[id], now) {
			changed = append(changed, id)
		}
	}
	if r.onChange != nil && ctx.Err() == nil {
		for _, id := range changed {
			r.onChange(id)
		}
	}
}

// dropUntrackedLocked forgets every spec that no longer has a head domain and
// returns those that had addresses: they lose them too, and are reported so
// the relay closes the sessions of the old head. r.mu is held.
func (r *headResolver) dropUntrackedLocked(domains map[string]string) (changed []string) {
	for id, e := range r.ents {
		if domains[id] != "" {
			continue
		}
		if len(e.ips) > 0 {
			changed = append(changed, id)
		}
		delete(r.ents, id)
	}
	return changed
}

// dueLocked starts tracking new specs, starts over a spec whose domain
// changed, and returns the specs due for a lookup at now, plus those whose
// domain change dropped addresses (changed). r.mu is held.
func (r *headResolver) dueLocked(domains map[string]string, now time.Time) (due, changed []string) {
	for id, dom := range domains {
		if dom == "" {
			continue
		}
		e := r.ents[id]
		switch {
		case e == nil:
			r.ents[id] = &headEntry{domain: dom}
			due = append(due, id)
		case e.domain != dom:
			if len(e.ips) > 0 {
				changed = append(changed, id)
			}
			*e = headEntry{domain: dom}
			due = append(due, id)
		case !now.Before(e.next):
			due = append(due, id)
		}
	}
	return due, changed
}

// resolve looks the domain up and stores a good answer; it reports whether
// the stored set changed.
func (r *headResolver) resolve(ctx context.Context, id, domain string, now time.Time) bool {
	got, err := r.lookup(ctx, domain)
	var ips []netip.Addr
	for _, ip := range got {
		if ip = ip.Unmap(); ip.Is4() && !slices.Contains(ips, ip) {
			ips = append(ips, ip)
		}
	}
	slices.SortFunc(ips, netip.Addr.Compare)

	r.mu.Lock()
	defer r.mu.Unlock()
	e := r.ents[id]
	if e == nil || e.domain != domain {
		return false
	}
	if err != nil || len(ips) == 0 {
		e.next = now.Add(headResolveInterval)
		if len(e.ips) == 0 {
			e.next = now.Add(headResolveRetry)
		}
		return false
	}
	e.next = now.Add(headResolveInterval)
	if slices.Equal(e.ips, ips) {
		return false
	}
	e.ips = ips
	return true
}

// run refreshes the domains domainsFn reports until ctx ends.
func (r *headResolver) run(ctx context.Context, domainsFn func() map[string]string) {
	t := time.NewTicker(headResolveTick)
	defer t.Stop()
	for {
		r.refresh(ctx, domainsFn())
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
