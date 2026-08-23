// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package ping

import (
	"context"
	"errors"
	"net"
	"os"
	"testing"
	"time"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
)

func TestPingHostLoopback(t *testing.T) {
	rtt, err := Host(context.Background(), "127.0.0.1", 2*time.Second)
	if err != nil {
		if errors.Is(err, ErrICMPUnavailable) {
			t.Skipf("unprivileged ICMP not permitted in this environment: %v", err)
		}
		t.Fatalf("loopback ping failed: %v", err)
	}
	if rtt < 0 {
		t.Fatalf("negative rtt: %v", rtt)
	}
}

func TestPingHostSocketOpenError(t *testing.T) {
	orig := listenPacket
	listenPacket = func(network, address string) (icmpConn, error) { return nil, errors.New("operation not permitted") }
	defer func() { listenPacket = orig }()
	_, err := Host(context.Background(), "127.0.0.1", time.Second)
	if !errors.Is(err, ErrICMPUnavailable) {
		t.Fatalf("want ErrICMPUnavailable, got %v", err)
	}
}

func TestPingHostEmptyHost(t *testing.T) {
	if _, err := Host(context.Background(), "", time.Second); err == nil {
		t.Fatal("empty host must error")
	}
	_ = net.IPv4zero
}

// fakeEchoConn is an icmpConn that returns one preset reply on every ReadFrom.
type fakeEchoConn struct{ reply []byte }

func (c *fakeEchoConn) WriteTo(b []byte, _ net.Addr) (int, error) { return len(b), nil }
func (c *fakeEchoConn) ReadFrom(b []byte) (int, net.Addr, error) {
	return copy(b, c.reply), &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)}, nil
}
func (c *fakeEchoConn) SetReadDeadline(time.Time) error { return nil }
func (c *fakeEchoConn) Close() error                    { return nil }

// TestPingHostMatchesReplyByDataNotID is the Linux regression guard: a real
// unprivileged SOCK_DGRAM ICMP socket has the kernel overwrite the Echo ID with
// the socket source port, so replies never carry the ID we set. This feeds
// Host an echo-REPLY whose ID is deliberately NOT our pid; it must still
// succeed by matching on Seq + the echoed Data. If an `echo.ID == id` filter is
// ever reintroduced this test times out and fails (mutation-proven).
func TestPingHostMatchesReplyByDataNotID(t *testing.T) {
	// A guaranteed-different ID: the complement (within 16 bits) of the id
	// Host sends (os.Getpid()&0xffff) can never equal it.
	mismatchID := (os.Getpid() & 0xffff) ^ 0xffff
	reply := icmp.Message{
		Type: ipv4.ICMPTypeEchoReply, Code: 0,
		Body: &icmp.Echo{ID: mismatchID, Seq: 1, Data: []byte("op-gw-ping")},
	}
	rb, err := reply.Marshal(nil)
	if err != nil {
		t.Fatalf("marshal reply: %v", err)
	}

	orig := listenPacket
	listenPacket = func(string, string) (icmpConn, error) { return &fakeEchoConn{reply: rb}, nil }
	defer func() { listenPacket = orig }()

	rtt, err := Host(context.Background(), "127.0.0.1", 2*time.Second)
	if err != nil {
		t.Fatalf("Host with a mismatched-ID reply failed: %v (replies must be matched by Data, not Echo ID)", err)
	}
	if rtt < 0 {
		t.Fatalf("negative rtt: %v", rtt)
	}
}
