// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package collector

import (
	"net/netip"
	"strings"

	psnet "github.com/shirou/gopsutil/v4/net"
)

// Conn is one established TCP connection of a process.
type Conn struct {
	Local, Remote netip.AddrPort
}

// EstablishedConns lists the established TCP connections process pid holds,
// IPv4-mapped addresses unmapped. It can be slow (on some platforms the
// listing shells out), so callers keep it off any latency-sensitive loop.
func EstablishedConns(pid int32) ([]Conn, error) {
	stats, err := psnet.ConnectionsPid("tcp", pid)
	if err != nil {
		return nil, err
	}
	return connsFromStats(stats), nil
}

// connsFromStats keeps the established connections whose addresses parse.
func connsFromStats(stats []psnet.ConnectionStat) []Conn {
	var out []Conn
	for _, s := range stats {
		if !strings.EqualFold(s.Status, "ESTABLISHED") {
			continue
		}
		local, lok := addrPort(s.Laddr)
		remote, rok := addrPort(s.Raddr)
		if lok && rok {
			out = append(out, Conn{Local: local, Remote: remote})
		}
	}
	return out
}

func addrPort(a psnet.Addr) (netip.AddrPort, bool) {
	ip, err := netip.ParseAddr(a.IP)
	if err != nil || a.Port > 65535 {
		return netip.AddrPort{}, false
	}
	return netip.AddrPortFrom(ip.Unmap(), uint16(a.Port)), true
}
