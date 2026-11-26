// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package agent

import (
	"context"
	"op-ai-server-agent/internal/collector"
	"op-ai-server-agent/internal/config"
	"op-ai-server-agent/internal/sample"
	"testing"
	"time"
)

func ifList(ip string) []sample.NetInterface {
	return []sample.NetInterface{{ID: "a", IDKind: "keyed", Name: "eth0", IPv4: []string{ip}, Preferred: true}}
}

func newTestInventory() (*interfaceInventory, *time.Time) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	clock := t0
	inv := newInterfaceInventory()
	inv.now = func() time.Time { return clock }
	return inv, &clock
}

func TestInterfaceInventoryFirstListSentImmediately(t *testing.T) {
	inv, clock := newTestInventory()
	if inv.pending() != nil {
		t.Fatal("pending before any observation")
	}
	inv.observe(*clock, ifList("192.0.2.5"))
	p := inv.pending()
	if p == nil || len(*p) != 1 {
		t.Fatalf("first list not pending at once: %v", p)
	}
	inv.delivered(interfaceSum(*p))
	if inv.pending() != nil {
		t.Fatal("delivered list still pending")
	}
}

func TestInterfaceInventoryWaitsForStability(t *testing.T) {
	inv, clock := newTestInventory()
	inv.observe(*clock, ifList("192.0.2.5"))
	inv.delivered(interfaceSum(*inv.pending()))

	t0 := *clock
	inv.observe(t0, ifList("198.51.100.7"))
	*clock = t0.Add(5 * time.Second)
	inv.observe(*clock, ifList("198.51.100.7"))
	if inv.pending() != nil {
		t.Fatal("pending before stable")
	}
	*clock = t0.Add(interfaceStableFor)
	inv.observe(*clock, ifList("198.51.100.7"))
	p := inv.pending()
	if p == nil || (*p)[0].IPv4[0] != "198.51.100.7" {
		t.Fatalf("not pending once stable: %v", p)
	}
}

func TestInterfaceInventoryFlapIsSuppressed(t *testing.T) {
	inv, clock := newTestInventory()
	inv.observe(*clock, ifList("192.0.2.5"))
	inv.delivered(interfaceSum(*inv.pending()))

	t0 := *clock
	inv.observe(t0, ifList("198.51.100.7"))
	inv.observe(t0.Add(3*time.Second), ifList("192.0.2.5"))
	*clock = t0.Add(30 * time.Second)
	inv.observe(*clock, ifList("192.0.2.5"))
	if inv.pending() != nil {
		t.Fatal("flap back to the delivered list produced a pending report")
	}
}

func TestInterfaceInventoryResendAfterReconnect(t *testing.T) {
	inv, clock := newTestInventory()
	inv.observe(*clock, ifList("192.0.2.5"))
	inv.delivered(interfaceSum(*inv.pending()))
	if inv.pending() != nil {
		t.Fatal("pending after delivery")
	}
	inv.resend()
	if inv.pending() == nil {
		t.Fatal("not pending after resend")
	}
}

func TestInterfaceInventoryEmptyListIsSent(t *testing.T) {
	inv, clock := newTestInventory()
	inv.observe(*clock, nil)
	p := inv.pending()
	if p == nil || *p == nil || len(*p) != 0 {
		t.Fatalf("empty stable list must be a non-nil empty slice, got %v", p)
	}
}

func TestSampleCarriesInterfacesOnlyOnChangeAndAfterResend(t *testing.T) {
	poster := &capturePoster{}
	a := NewFromDeps(config.Config{Interval: time.Hour}, Deps{Poster: poster, Interfaces: collector.NewInterfaceCollector(nil)})
	clock := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	a.ifaces.now = func() time.Time { return clock }

	a.collectOnce(context.Background())
	if poster.last().Interfaces == nil {
		t.Fatal("first sample must carry the interface list")
	}
	clock = clock.Add(interfaceCollectEvery)
	a.collectOnce(context.Background())
	if poster.last().Interfaces != nil {
		t.Fatal("unchanged list resent")
	}
	a.resendInterfaces()
	a.collectOnce(context.Background())
	if poster.last().Interfaces == nil {
		t.Fatal("list not resent after resend")
	}
}
