// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

//go:build linux

package hostpower

import (
	"context"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"testing"
	"time"
)

func TestStartCaptureSeesLoopbackHandshake(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root (CAP_NET_RAW) for an AF_PACKET socket")
	}
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	dst := ln.Addr().(*net.TCPAddr).AddrPort()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	table := NewObservationTable(30*time.Second, time.Now)
	if err := StartCapture(ctx, dst.Port(), table, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatal(err)
	}

	conn, err := net.Dial("tcp4", dst.String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	src := conn.LocalAddr().(*net.TCPAddr).AddrPort()
	key := ConnKey{Src: netip.AddrPortFrom(src.Addr().Unmap(), src.Port()), Dst: netip.AddrPortFrom(dst.Addr().Unmap(), dst.Port())}

	// Wait returns once a non-SYN segment (the handshake's ACK) is in.
	obs := table.Wait(ctx, key, time.Second)
	var syn, ack bool
	for _, o := range obs {
		if o.Ifname == "lo" {
			syn = syn || o.SYN
			ack = ack || !o.SYN
		}
	}
	if !syn || !ack {
		t.Fatalf("want a SYN and a non-SYN observation on lo for %+v; got %+v", key, obs)
	}
}
