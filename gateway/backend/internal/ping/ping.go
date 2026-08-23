// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

// Package ping runs an unprivileged ICMP echo (datagram socket, no raw-socket
// privilege required), gated by the kernel net.ipv4/ipv6 ping_group_range
// including the process GID.
package ping

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"time"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
)

// ErrICMPUnavailable means the unprivileged ICMP socket could not be opened
// (the kernel ping_group_range does not include this process's GID).
var ErrICMPUnavailable = errors.New("icmp unavailable: unprivileged ICMP not permitted in this environment")

// pingPayload is the Echo Data we send and match on. It (plus Seq) uniquely
// identifies our request across platforms — unlike the Echo ID, which a Linux
// unprivileged SOCK_DGRAM ICMP socket overwrites (see Host's read loop).
var pingPayload = []byte("op-gw-ping")

// icmpConn is the subset of *icmp.PacketConn we use — a seam for tests.
type icmpConn interface {
	WriteTo(b []byte, dst net.Addr) (int, error)
	ReadFrom(b []byte) (int, net.Addr, error)
	SetReadDeadline(t time.Time) error
	Close() error
}

// listenPacket is overridable in tests.
var listenPacket = func(network, address string) (icmpConn, error) {
	return icmp.ListenPacket(network, address)
}

// Host sends one unprivileged ICMP echo to host and returns the round-trip
// time. Returns ErrICMPUnavailable if the socket cannot be opened.
func Host(ctx context.Context, host string, timeout time.Duration) (time.Duration, error) {
	if host == "" {
		return 0, errors.New("ping: empty host")
	}
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return 0, fmt.Errorf("ping: resolve %q: %w", host, err)
	}
	// Prefer IPv4.
	var ip net.IP
	for _, a := range addrs {
		if a.IP.To4() != nil {
			ip = a.IP
			break
		}
	}
	v6 := false
	if ip == nil {
		if len(addrs) == 0 {
			return 0, fmt.Errorf("ping: no address for %q", host)
		}
		ip = addrs[0].IP
		v6 = ip.To4() == nil
	}

	network, listenAddr := "udp4", "0.0.0.0"
	echoType, replyType := icmp.Type(ipv4.ICMPTypeEcho), icmp.Type(ipv4.ICMPTypeEchoReply)
	if v6 {
		network, listenAddr = "udp6", "::"
		echoType, replyType = icmp.Type(ipv6.ICMPTypeEchoRequest), icmp.Type(ipv6.ICMPTypeEchoReply)
	}
	conn, err := listenPacket(network, listenAddr)
	if err != nil {
		return 0, fmt.Errorf("%w: %v", ErrICMPUnavailable, err)
	}
	defer conn.Close()

	// id is still SET on the request, but replies are NOT filtered by it — a
	// Linux unprivileged SOCK_DGRAM ICMP socket overwrites the Echo ID with the
	// socket source port (net/ipv4/ping.c) and demuxes by it, so the ID we set
	// never comes back. We match on Seq + the echoed Data instead (below).
	id := os.Getpid() & 0xffff
	msg := icmp.Message{
		Type: echoType, Code: 0,
		Body: &icmp.Echo{ID: id, Seq: 1, Data: pingPayload},
	}
	wb, err := msg.Marshal(nil)
	if err != nil {
		return 0, fmt.Errorf("ping: marshal: %w", err)
	}
	deadline := time.Now().Add(timeout)
	_ = conn.SetReadDeadline(deadline)
	start := time.Now()
	if _, err := conn.WriteTo(wb, &net.UDPAddr{IP: ip}); err != nil {
		return 0, fmt.Errorf("ping: write: %w", err)
	}
	rb := make([]byte, 1500)
	proto := 1 // ipv4 ICMP
	if v6 {
		proto = 58
	}
	for {
		if time.Now().After(deadline) {
			return 0, fmt.Errorf("ping: no reply within %s", timeout)
		}
		n, _, err := conn.ReadFrom(rb)
		if err != nil {
			return 0, fmt.Errorf("ping: read: %w", err)
		}
		rm, err := icmp.ParseMessage(proto, rb[:n])
		if err != nil || rm.Type != replyType {
			continue
		}
		// Match by Seq + the echoed Data payload (NOT the Echo ID — see above):
		// both round-trip unchanged on every platform and uniquely identify our
		// request. The x/net/icmp godoc example checks the reply Type only for
		// the same reason.
		if echo, ok := rm.Body.(*icmp.Echo); ok && echo.Seq == 1 && bytes.HasPrefix(echo.Data, pingPayload) {
			return time.Since(start), nil
		}
	}
}
