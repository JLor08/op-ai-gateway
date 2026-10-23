// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"sync"
	"sync/atomic"
	"testing"
)

func TestLiveSessionRegistryAcquireBusyThenReleaseFreesRoute(t *testing.T) {
	t.Parallel()
	reg := newLiveSessionRegistry()

	release, ok := reg.acquire("r1")
	if !ok || release == nil {
		t.Fatalf("first acquire(r1) = (release nil: %v, ok: %v), want a release and ok", release == nil, ok)
	}

	again, ok := reg.acquire("r1")
	if ok {
		t.Fatal("second acquire(r1) while held was ok, want busy reject")
	}
	if again != nil {
		t.Fatal("a refused acquire must return a nil release")
	}

	release()

	release2, ok := reg.acquire("r1")
	if !ok || release2 == nil {
		t.Fatalf("acquire(r1) after release = (release nil: %v, ok: %v), want a release and ok", release2 == nil, ok)
	}
	release2()
}

func TestLiveSessionRegistryDifferentRoutesAreIndependent(t *testing.T) {
	t.Parallel()
	reg := newLiveSessionRegistry()

	releaseA, ok := reg.acquire("r1")
	if !ok {
		t.Fatal("acquire(r1) was refused, want ok")
	}
	releaseB, ok := reg.acquire("r2")
	if !ok {
		t.Fatal("acquire(r2) was refused while r1 is held, want ok")
	}

	// Releasing r1 must leave r2 held.
	releaseA()
	if _, ok := reg.acquire("r2"); ok {
		t.Fatal("acquire(r2) was ok after only r1 was released, want busy reject")
	}
	if release, ok := reg.acquire("r1"); !ok {
		t.Fatal("acquire(r1) was refused after r1 was released, want ok")
	} else {
		release()
	}
	releaseB()
}

func TestLiveSessionRegistryReleaseTwiceIsSafe(t *testing.T) {
	t.Parallel()
	reg := newLiveSessionRegistry()

	release, ok := reg.acquire("r1")
	if !ok {
		t.Fatal("acquire(r1) was refused, want ok")
	}
	release()
	release() // must not panic

	release2, ok := reg.acquire("r1")
	if !ok {
		t.Fatal("acquire(r1) after a double release was refused, want ok")
	}
	release2()
}

// A stale release (the first holder releasing again after a second session has
// re-acquired the route) must not drop the second session's hold.
func TestLiveSessionRegistryStaleReleaseDoesNotFreeReacquiredSlot(t *testing.T) {
	t.Parallel()
	reg := newLiveSessionRegistry()

	releaseFirst, ok := reg.acquire("r1")
	if !ok {
		t.Fatal("first acquire(r1) was refused, want ok")
	}
	releaseFirst()

	releaseSecond, ok := reg.acquire("r1")
	if !ok {
		t.Fatal("re-acquire(r1) after release was refused, want ok")
	}

	releaseFirst() // stale: the first holder releases a second time

	if third, ok := reg.acquire("r1"); ok {
		third()
		t.Fatal("a stale release freed the slot the second session holds")
	}

	releaseSecond()
	releaseSecond() // idempotent for the legitimate holder too

	releaseThird, ok := reg.acquire("r1")
	if !ok {
		t.Fatal("acquire(r1) after the real holder released was refused, want ok")
	}
	releaseThird()
}

func TestLiveSessionRegistryEmptyRouteIDIsAnOrdinaryKey(t *testing.T) {
	t.Parallel()
	reg := newLiveSessionRegistry()

	release, ok := reg.acquire("")
	if !ok {
		t.Fatal("acquire(\"\") was refused, want ok")
	}
	if _, ok := reg.acquire(""); ok {
		t.Fatal("second acquire(\"\") while held was ok, want busy reject")
	}
	if other, ok := reg.acquire("r1"); !ok {
		t.Fatal("acquire(r1) was refused while only \"\" is held, want ok")
	} else {
		other()
	}
	release()
	if again, ok := reg.acquire(""); !ok {
		t.Fatal("acquire(\"\") after release was refused, want ok")
	} else {
		again()
	}
}

func TestLiveSessionRegistryNilReceiverFailsClosed(t *testing.T) {
	t.Parallel()
	var reg *liveSessionRegistry

	release, ok := reg.acquire("r1")
	if ok || release != nil {
		t.Fatalf("nil registry acquire = (release nil: %v, ok: %v), want (nil, false)", release == nil, ok)
	}
}

// N goroutines race for one route behind a barrier: exactly one wins; once it
// releases, exactly one more of a second wave can win.
func TestLiveSessionRegistryRaceExactlyOneWinner(t *testing.T) {
	t.Parallel()
	const goroutines = 64
	reg := newLiveSessionRegistry()

	wave := func() (winners int32, release func()) {
		var (
			won   atomic.Int32
			held  func()
			start = make(chan struct{})
			wg    sync.WaitGroup
			mu    sync.Mutex
		)
		for range goroutines {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				if rel, ok := reg.acquire("shared"); ok {
					won.Add(1)
					mu.Lock()
					held = rel
					mu.Unlock()
				}
			}()
		}
		close(start)
		wg.Wait()
		return won.Load(), held
	}

	first, releaseFirst := wave()
	if first != 1 {
		t.Fatalf("first wave winners = %d, want exactly 1", first)
	}
	if releaseFirst == nil {
		t.Fatal("first wave winner has no release func")
	}

	// While the winner still holds the route, a second wave must all be refused.
	if second, rel := wave(); second != 0 {
		if rel != nil {
			rel()
		}
		t.Fatalf("second wave winners = %d while the route is held, want 0", second)
	}

	releaseFirst()

	third, releaseThird := wave()
	if third != 1 {
		t.Fatalf("wave after release winners = %d, want exactly 1", third)
	}
	if releaseThird != nil {
		releaseThird()
	}
}

// Concurrent acquire/release churn on several routes must stay race-free and
// never admit two holders of one route at once.
func TestLiveSessionRegistryConcurrentChurnNeverDoubleHolds(t *testing.T) {
	t.Parallel()
	const (
		goroutines = 32
		iterations = 200
	)
	routes := []string{"a", "b", "c"}
	reg := newLiveSessionRegistry()
	inside := make(map[string]*atomic.Int32, len(routes))
	for _, r := range routes {
		inside[r] = new(atomic.Int32)
	}

	var wg sync.WaitGroup
	for g := range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			route := routes[g%len(routes)]
			for range iterations {
				release, ok := reg.acquire(route)
				if !ok {
					continue
				}
				if n := inside[route].Add(1); n != 1 {
					t.Errorf("route %q has %d concurrent holders, want 1", route, n)
				}
				inside[route].Add(-1)
				release()
			}
		}()
	}
	wg.Wait()
}
