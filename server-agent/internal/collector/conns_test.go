// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package collector

import (
	"net/netip"
	"testing"

	psnet "github.com/shirou/gopsutil/v4/net"
)

func TestConnsFromStatsKeepsEstablishedTCP(t *testing.T) {
	stats := []psnet.ConnectionStat{
		{Status: "ESTABLISHED", Laddr: psnet.Addr{IP: "10.0.0.1", Port: 47001}, Raddr: psnet.Addr{IP: "10.0.0.2", Port: 5555}},
		{Status: "LISTEN", Laddr: psnet.Addr{IP: "10.0.0.1", Port: 47001}},
		{Status: "ESTABLISHED", Laddr: psnet.Addr{IP: "::ffff:10.0.0.1", Port: 47001}, Raddr: psnet.Addr{IP: "::ffff:10.0.0.3", Port: 6}},
		{Status: "ESTABLISHED", Laddr: psnet.Addr{IP: "bogus", Port: 1}, Raddr: psnet.Addr{IP: "10.0.0.4", Port: 2}},
		{Status: "TIME_WAIT", Laddr: psnet.Addr{IP: "10.0.0.1", Port: 47001}, Raddr: psnet.Addr{IP: "10.0.0.5", Port: 7}},
	}
	got := connsFromStats(stats)
	if len(got) != 2 {
		t.Fatalf("got %d conns, want 2: %+v", len(got), got)
	}
	if got[0].Remote != netip.MustParseAddrPort("10.0.0.2:5555") || got[0].Local.Port() != 47001 {
		t.Fatalf("first conn = %+v", got[0])
	}
	if got[1].Remote.Addr() != netip.MustParseAddr("10.0.0.3") {
		t.Fatalf("mapped IPv4 not unmapped: %+v", got[1])
	}
}
