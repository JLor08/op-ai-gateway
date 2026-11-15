// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

type fakeMemoClock struct{ t time.Time }

func (c *fakeMemoClock) now() time.Time          { return c.t }
func (c *fakeMemoClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func memoHash(b byte) blobHash { return blobHash{b} }

func newTestMemo() (*rejectedBlobMemo, *fakeMemoClock) {
	c := &fakeMemoClock{t: time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)}
	return newRejectedBlobMemo(c.now), c
}

var memoKey = rejectedBlobKey{Session: "s1", Target: "acct", Flavor: "openai-responses"}

func TestRejectedBlobMemoRecordAndMatch(t *testing.T) {
	m, _ := newTestMemo()
	if m.Has(memoKey) {
		t.Fatal("Has on empty memo")
	}
	m.Record(memoKey, []blobHash{memoHash(1), memoHash(2)})
	if !m.Has(memoKey) {
		t.Fatal("Has false after Record")
	}
	if m.Has(rejectedBlobKey{Session: "s1", Target: "other", Flavor: memoKey.Flavor}) {
		t.Fatal("Has leaked across targets")
	}
	got := m.Match(memoKey, []blobHash{memoHash(2), memoHash(9)})
	if len(got) != 1 {
		t.Fatalf("Match = %v, want only hash 2", got)
	}
	if _, ok := got[memoHash(2)]; !ok {
		t.Fatal("hash 2 not matched")
	}
	// Record on an existing key unions.
	m.Record(memoKey, []blobHash{memoHash(3)})
	got = m.Match(memoKey, []blobHash{memoHash(1), memoHash(3)})
	if len(got) != 2 {
		t.Fatalf("Match after union = %v, want hashes 1 and 3", got)
	}
}

func TestRejectedBlobMemoKeepsUnmatchedHashesUntilGrace(t *testing.T) {
	m, c := newTestMemo()
	m.Record(memoKey, []blobHash{memoHash(1)})
	c.advance(10 * time.Minute)
	if got := m.Match(memoKey, []blobHash{memoHash(9)}); len(got) != 0 {
		t.Fatalf("unexpected match %v", got)
	}
	if !m.Has(memoKey) {
		t.Fatal("hash dropped by a non-matching request before grace")
	}
	if got := m.Match(memoKey, []blobHash{memoHash(1)}); len(got) != 1 {
		t.Fatalf("hash lost after non-matching request: %v", got)
	}
	// Matching stamped it at +10 min; +60 min later it is still within grace.
	c.advance(59 * time.Minute)
	if !m.Has(memoKey) {
		t.Fatal("matched hash dropped within grace")
	}
	c.advance(2 * time.Minute) // 61 min since last match
	if m.Has(memoKey) {
		t.Fatal("Has true after grace elapsed")
	}
	if got := m.Match(memoKey, []blobHash{memoHash(1)}); len(got) != 0 {
		t.Fatalf("expired hash matched: %v", got)
	}
}

func TestRejectedBlobMemoGracePrunesPerHash(t *testing.T) {
	m, c := newTestMemo()
	m.Record(memoKey, []blobHash{memoHash(1), memoHash(2)})
	c.advance(50 * time.Minute)
	m.Match(memoKey, []blobHash{memoHash(1)})
	c.advance(20 * time.Minute) // hash 2: 70 min unmatched, hash 1: 20 min
	got := m.Match(memoKey, []blobHash{memoHash(1), memoHash(2)})
	if len(got) != 1 {
		t.Fatalf("Match = %v, want only hash 1", got)
	}
	if _, ok := got[memoHash(1)]; !ok {
		t.Fatal("hash 1 missing")
	}
}

func TestRejectedBlobMemoTTL(t *testing.T) {
	m, c := newTestMemo()
	m.Record(memoKey, []blobHash{memoHash(1)})
	// Keep the hash matched every 30 min so grace never expires it.
	for elapsed := 30 * time.Minute; elapsed < 24*time.Hour; elapsed += 30 * time.Minute {
		c.advance(30 * time.Minute)
		if len(m.Match(memoKey, []blobHash{memoHash(1)})) != 1 {
			t.Fatalf("hash lost early at %s", elapsed)
		}
	}
	c.advance(31 * time.Minute) // 24 h 1 min since first Record, matched 31 min ago
	if got := m.Match(memoKey, []blobHash{memoHash(1)}); len(got) != 0 {
		t.Fatalf("entry survived TTL: %v", got)
	}
	if m.Has(memoKey) {
		t.Fatal("Has true after TTL")
	}
}

func TestRejectedBlobMemoCapEvictsLeastRecent(t *testing.T) {
	m, _ := newTestMemo()
	m.cap = 2
	k := func(s string) rejectedBlobKey { return rejectedBlobKey{Session: s, Target: "a", Flavor: "f"} }
	m.Record(k("a"), []blobHash{memoHash(1)})
	m.Record(k("b"), []blobHash{memoHash(1)})
	m.Match(k("a"), []blobHash{memoHash(1)}) // a is now most recent
	m.Record(k("c"), []blobHash{memoHash(1)})
	if m.Has(k("b")) {
		t.Fatal("least recently used entry b not evicted")
	}
	if !m.Has(k("a")) || !m.Has(k("c")) {
		t.Fatal("recent entries evicted")
	}
}

func TestRejectedBlobMemoConcurrent(t *testing.T) {
	m, _ := newTestMemo()
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			k := rejectedBlobKey{Session: fmt.Sprint(g % 3), Target: "a", Flavor: "f"}
			for i := 0; i < 200; i++ {
				m.Record(k, []blobHash{memoHash(byte(i % 16))})
				m.Has(k)
				m.Match(k, []blobHash{memoHash(byte(i % 16)), memoHash(200)})
			}
		}(g)
	}
	wg.Wait()
}
