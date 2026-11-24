// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package routing

import (
	"sync"
	"testing"
	"time"
)

func TestVendorLimitCooldownsPerAccountAndModel(t *testing.T) {
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	c := NewVendorLimitCooldowns()
	c.Set("acc_1", "claude-opus", now.Add(time.Minute))
	if u, ok := c.Until("acc_1", "claude-opus", now); !ok || !u.Equal(now.Add(time.Minute)) {
		t.Fatalf("opus = (%v,%v)", u, ok)
	}
	if _, ok := c.Until("acc_1", "claude-sonnet", now); ok {
		t.Fatal("a cooldown on opus must not block sonnet")
	}
	if _, ok := c.Until("acc_2", "claude-opus", now); ok {
		t.Fatal("a cooldown must not leak to another account")
	}
}

func TestVendorLimitCooldownsExpiryDeletes(t *testing.T) {
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	c := NewVendorLimitCooldowns()
	c.Set("acc_1", "m", now.Add(time.Minute))
	if _, ok := c.Until("acc_1", "m", now.Add(time.Minute)); ok {
		t.Fatal("expired cooldown still reported")
	}
	if len(c.m) != 0 {
		t.Fatalf("expired entry not deleted: %v", c.m)
	}
}

func TestVendorLimitCooldownsActiveSorted(t *testing.T) {
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	c := NewVendorLimitCooldowns()
	c.Set("acc_1", "b", now.Add(time.Minute))
	c.Set("acc_1", "a", now.Add(2*time.Minute))
	c.Set("acc_1", "old", now.Add(-time.Minute))
	c.Set("acc_2", "z", now.Add(time.Minute))
	got := c.Active("acc_1", now)
	if len(got) != 2 || got[0].UpstreamModel != "a" || got[1].UpstreamModel != "b" {
		t.Fatalf("Active = %#v", got)
	}
	if !got[0].Until.Equal(now.Add(2 * time.Minute)) {
		t.Fatalf("Until = %v", got[0].Until)
	}
}

func TestVendorLimitCooldownsNilSafe(t *testing.T) {
	var c *VendorLimitCooldowns
	now := time.Now()
	c.Set("a", "m", now.Add(time.Minute))
	if _, ok := c.Until("a", "m", now); ok {
		t.Fatal("nil registry reported a cooldown")
	}
	if c.Active("a", now) != nil {
		t.Fatal("nil registry Active must be nil")
	}
}

func TestVendorLimitCooldownsConcurrent(t *testing.T) {
	c := NewVendorLimitCooldowns()
	now := time.Now()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				c.Set("acc", "m", now.Add(time.Minute))
				c.Until("acc", "m", now)
				c.Active("acc", now)
			}
		}()
	}
	wg.Wait()
}
