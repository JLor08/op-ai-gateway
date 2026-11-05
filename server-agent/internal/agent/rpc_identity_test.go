// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package agent

import (
	"context"
	"errors"
	"net/netip"
	"slices"
	"sync"
	"testing"
	"time"
)

type lookupFake struct {
	mu    sync.Mutex
	ips   []netip.Addr
	err   error
	calls int
}

func (l *lookupFake) lookup(_ context.Context, _ string) ([]netip.Addr, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls++
	return slices.Clone(l.ips), l.err
}

func (l *lookupFake) set(err error, ips ...netip.Addr) {
	l.mu.Lock()
	l.err, l.ips = err, ips
	l.mu.Unlock()
}

func TestHeadResolverKeepsLastGoodSet(t *testing.T) {
	a := netip.MustParseAddr("192.168.1.10")
	lf := &lookupFake{ips: []netip.Addr{a}}
	now := time.Unix(1000, 0)
	var changed []string
	r := newHeadResolver(lf.lookup, func(id string) { changed = append(changed, id) })
	r.now = func() time.Time { return now }
	ctx := context.Background()
	doms := map[string]string{"rpcm_a": "head.lan"}

	r.refresh(ctx, doms)
	if got := r.IPs("rpcm_a"); !slices.Equal(got, []netip.Addr{a}) {
		t.Fatalf("ips = %v", got)
	}
	if !slices.Equal(changed, []string{"rpcm_a"}) {
		t.Fatalf("changed = %v", changed)
	}
	// Not due again before 60 s.
	now = now.Add(30 * time.Second)
	r.refresh(ctx, doms)
	if lf.calls != 1 {
		t.Fatalf("resolved again early: %d", lf.calls)
	}
	// Failure after 60 s keeps the last good set and does not report a change.
	lf.set(errors.New("nxdomain"))
	now = now.Add(31 * time.Second)
	r.refresh(ctx, doms)
	if lf.calls != 2 || !slices.Equal(r.IPs("rpcm_a"), []netip.Addr{a}) || len(changed) != 1 {
		t.Fatalf("calls=%d ips=%v changed=%v", lf.calls, r.IPs("rpcm_a"), changed)
	}
	// A new good answer replaces the set and reports the change.
	b := netip.MustParseAddr("192.168.1.11")
	lf.set(nil, b)
	now = now.Add(61 * time.Second)
	r.refresh(ctx, doms)
	if !slices.Equal(r.IPs("rpcm_a"), []netip.Addr{b}) || len(changed) != 2 {
		t.Fatalf("ips=%v changed=%v", r.IPs("rpcm_a"), changed)
	}
}

func TestHeadResolverNoGoodSetIsEmpty(t *testing.T) {
	lf := &lookupFake{err: errors.New("down")}
	r := newHeadResolver(lf.lookup, nil)
	now := time.Unix(1000, 0)
	r.now = func() time.Time { return now }
	ctx := context.Background()
	r.refresh(ctx, map[string]string{"rpcm_a": "head.lan"})
	if got := r.IPs("rpcm_a"); len(got) != 0 {
		t.Fatalf("ips = %v, want empty", got)
	}
	if got := r.IPs("unknown"); len(got) != 0 {
		t.Fatalf("unknown spec = %v", got)
	}
	// Retries sooner than the steady cadence while there is no good set.
	now = now.Add(11 * time.Second)
	lf.set(nil, netip.MustParseAddr("192.168.1.10"))
	r.refresh(ctx, map[string]string{"rpcm_a": "head.lan"})
	if len(r.IPs("rpcm_a")) != 1 {
		t.Fatal("did not retry after the failure")
	}
}

func TestHeadResolverDomainChangeAndRemoval(t *testing.T) {
	a := netip.MustParseAddr("192.168.1.10")
	lf := &lookupFake{ips: []netip.Addr{a}}
	r := newHeadResolver(lf.lookup, nil)
	ctx := context.Background()
	r.refresh(ctx, map[string]string{"rpcm_a": "one.lan"})
	lf.set(errors.New("down"))
	r.refresh(ctx, map[string]string{"rpcm_a": "two.lan"})
	if got := r.IPs("rpcm_a"); len(got) != 0 {
		t.Fatalf("the old domain's set leaked into the new one: %v", got)
	}
	r.refresh(ctx, map[string]string{})
	if _, ok := r.state("rpcm_a"); ok {
		t.Fatal("entry kept after the spec lost its domain")
	}
}
