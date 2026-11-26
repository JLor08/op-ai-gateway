// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package agent

import (
	"context"
	"errors"
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

func TestInterfaceInventoryResendRepeatsDeliveredListUntilChangeIsStable(t *testing.T) {
	inv, clock := newTestInventory()
	a, b := ifList("192.0.2.5"), ifList("198.51.100.7")
	inv.observe(*clock, a)
	inv.delivered(interfaceSum(*inv.pending()))

	t0 := *clock
	inv.observe(t0, b)
	*clock = t0.Add(2 * time.Second)
	inv.resend()
	p := inv.pending()
	if p == nil || (*p)[0].IPv4[0] != "192.0.2.5" {
		t.Fatalf("resend of an unsettled change must repeat the delivered list: %v", p)
	}
	inv.delivered(interfaceSum(*p))
	if inv.pending() != nil {
		t.Fatal("pending right after the repeat")
	}
	*clock = t0.Add(interfaceStableFor)
	p = inv.pending()
	if p == nil || (*p)[0].IPv4[0] != "198.51.100.7" {
		t.Fatalf("stable change not reported: %v", p)
	}
	inv.delivered(interfaceSum(*p))
	inv.resend()
	if p = inv.pending(); p == nil || (*p)[0].IPv4[0] != "198.51.100.7" {
		t.Fatalf("resend after delivery lost the new list: %v", p)
	}
}

type failOncePoster struct {
	capturePoster
	fail bool
}

func (p *failOncePoster) Post(ctx context.Context, s *sample.Sample) error {
	_ = p.capturePoster.Post(ctx, s)
	if p.fail {
		p.fail = false
		return errors.New("push failed")
	}
	return nil
}

func TestSampleResendsInterfacesAfterFailedPost(t *testing.T) {
	poster := &failOncePoster{fail: true}
	a := NewFromDeps(config.Config{Interval: time.Hour}, Deps{Poster: poster, Interfaces: collector.NewInterfaceCollector(nil)})
	a.collectOnce(context.Background())
	a.collectOnce(context.Background())
	if poster.samples[0].Interfaces == nil || poster.samples[1].Interfaces == nil {
		t.Fatal("list must be attached again after a failed post")
	}
	a.collectOnce(context.Background())
	if poster.samples[2].Interfaces != nil {
		t.Fatal("list resent after a successful post")
	}
}
