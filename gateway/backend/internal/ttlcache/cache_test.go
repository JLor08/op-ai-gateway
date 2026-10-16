// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package ttlcache

import (
	"context"
	"testing"
	"time"
)

// fakeClock is a hand-driven Cache.Now.
type fakeClock struct{ t time.Time }

func (f *fakeClock) now() time.Time          { return f.t }
func (f *fakeClock) advance(d time.Duration) { f.t = f.t.Add(d) }

func newFakeClock() *fakeClock { return &fakeClock{t: time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)} }

func TestGetServesTheCachedValueUntilTheTTLLapses(t *testing.T) {
	clock := newFakeClock()
	c := &Cache[int]{Now: clock.now}
	loads := 0
	load := func(context.Context) int { loads++; return loads }
	ctx := context.Background()

	if got := c.Get(ctx, 5*time.Second, load); got != 1 {
		t.Fatalf("first Get = %d, want 1", got)
	}
	clock.advance(4 * time.Second)
	if got := c.Get(ctx, 5*time.Second, load); got != 1 || loads != 1 {
		t.Fatalf("Get inside the window = %d after %d loads, want the cached 1 after 1", got, loads)
	}
	clock.advance(time.Second)
	if got := c.Get(ctx, 5*time.Second, load); got != 2 || loads != 2 {
		t.Fatalf("Get at the window's end = %d after %d loads, want a fresh 2 after 2", got, loads)
	}
}

func TestInvalidateForcesTheNextGetToLoad(t *testing.T) {
	clock := newFakeClock()
	c := &Cache[int]{Now: clock.now}
	loads := 0
	load := func(context.Context) int { loads++; return loads }
	ctx := context.Background()

	c.Get(ctx, time.Hour, load)
	c.Invalidate()
	if !c.ExpiresAt().IsZero() {
		t.Fatalf("ExpiresAt after Invalidate = %v, want the zero time", c.ExpiresAt())
	}
	if got := c.Get(ctx, time.Hour, load); got != 2 {
		t.Fatalf("Get after Invalidate = %d, want a fresh 2", got)
	}
}

// TestInvalidateDuringAnInFlightLoadDiscardsItsResult is the race the generation counter
// exists for: a load that STARTED before an Invalidate must not publish its (now stale)
// value afterwards, or an operator who just flipped a switch would watch the old value
// come back for a whole TTL.
func TestInvalidateDuringAnInFlightLoadDiscardsItsResult(t *testing.T) {
	c := &Cache[string]{}
	ctx := context.Background()
	started, release := make(chan struct{}), make(chan struct{})

	done := make(chan string)
	go func() {
		done <- c.Get(ctx, time.Hour, func(context.Context) string {
			close(started)
			<-release
			return "stale"
		})
	}()
	<-started
	c.Invalidate()
	close(release)
	if got := <-done; got != "stale" {
		t.Fatalf("the in-flight Get returned %q, want its own load's %q", got, "stale")
	}

	if got := c.Get(ctx, time.Hour, func(context.Context) string { return "fresh" }); got != "fresh" {
		t.Fatalf("Get after the raced load = %q, want fresh (the stale result must not have been published)", got)
	}
}

func TestGetTTLWithAZeroTTLCachesNothing(t *testing.T) {
	clock := newFakeClock()
	c := &Cache[int]{Now: clock.now}
	loads := 0
	load := func(context.Context) (int, time.Duration) { loads++; return loads, 0 }
	ctx := context.Background()

	for want := 1; want <= 3; want++ {
		if got := c.GetTTL(ctx, load); got != want {
			t.Fatalf("GetTTL #%d = %d, want a fresh %d (a zero TTL must not be retained)", want, got, want)
		}
	}
}

func TestGetTTLHonoursThePerCallTTL(t *testing.T) {
	clock := newFakeClock()
	c := &Cache[int]{Now: clock.now}
	loads := 0
	ctx := context.Background()
	load := func(context.Context) (int, time.Duration) { loads++; return loads, time.Second }

	c.GetTTL(ctx, load)
	if want := clock.t.Add(time.Second); !c.ExpiresAt().Equal(want) {
		t.Fatalf("ExpiresAt = %v, want %v", c.ExpiresAt(), want)
	}
	clock.advance(time.Second)
	if got := c.GetTTL(ctx, load); got != 2 {
		t.Fatalf("GetTTL after its TTL = %d, want a fresh 2", got)
	}
}

func TestZeroValueUsesTheWallClock(t *testing.T) {
	var c Cache[int]
	loads := 0
	load := func(context.Context) int { loads++; return loads }
	ctx := context.Background()
	c.Get(ctx, time.Hour, load)
	if got := c.Get(ctx, time.Hour, load); got != 1 || loads != 1 {
		t.Fatalf("zero-value Cache did not cache: got %d after %d loads", got, loads)
	}
}
