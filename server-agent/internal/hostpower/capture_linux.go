// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

//go:build linux

package hostpower

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"syscall"
	"time"
)

const (
	ethPAll            = 0x0003 // ETH_P_ALL: every protocol
	captureReadTimeout = 500 * time.Millisecond
	captureErrorPause  = 100 * time.Millisecond
)

// htons converts v to network byte order as the kernel expects it in a
// socket protocol argument.
func htons(v uint16) uint16 {
	var b [2]byte
	binary.BigEndian.PutUint16(b[:], v)
	return binary.NativeEndian.Uint16(b[:])
}

// StartCapture opens an AF_PACKET socket on all interfaces, filters it to
// IPv4 TCP segments for port and records every received copy in table until
// ctx ends. It returns an error when the socket cannot be set up (no CAP_NET_RAW,
// filter rejected); the read loop runs in its own goroutine.
func StartCapture(ctx context.Context, port uint16, table *ObservationTable, log *slog.Logger) error {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	fd, err := syscall.Socket(syscall.AF_PACKET, syscall.SOCK_RAW|syscall.SOCK_CLOEXEC, int(htons(ethPAll)))
	if err != nil {
		return fmt.Errorf("hostpower: open packet socket: %w", err)
	}
	// Frames queued between socket() and the filter attach are unfiltered;
	// the userspace re-check in the read loop drops what does not belong.
	prog := tcpPortFilter(port)
	filter := make([]syscall.SockFilter, len(prog))
	for i, in := range prog {
		filter[i] = syscall.SockFilter{Code: in.Code, Jt: in.Jt, Jf: in.Jf, K: in.K}
	}
	if err := syscall.AttachLsf(fd, filter); err != nil { //nolint:staticcheck // stdlib-only constraint; golang.org/x/net/bpf is not allowed
		_ = syscall.Close(fd)
		return fmt.Errorf("hostpower: attach packet filter: %w", err)
	}
	tv := syscall.NsecToTimeval(captureReadTimeout.Nanoseconds())
	if err := syscall.SetsockoptTimeval(fd, syscall.SOL_SOCKET, syscall.SO_RCVTIMEO, &tv); err != nil {
		_ = syscall.Close(fd)
		return fmt.Errorf("hostpower: set packet socket timeout: %w", err)
	}
	go captureLoop(ctx, fd, port, table, log)
	return nil
}

// captureLoop reads frames until ctx ends, then closes fd. SO_RCVTIMEO wakes
// it regularly so it notices ctx.
func captureLoop(ctx context.Context, fd int, port uint16, table *ObservationTable, log *slog.Logger) {
	defer func() { _ = syscall.Close(fd) }()
	buf := make([]byte, captureSnapLen)
	names := make(map[int]string)
	warned := false
	for ctx.Err() == nil {
		n, from, err := syscall.Recvfrom(fd, buf, 0)
		if err != nil {
			if errors.Is(err, syscall.EAGAIN) || errors.Is(err, syscall.EINTR) {
				continue
			}
			if !warned {
				log.Warn("hostpower: packet capture read failed; continuing", "err", err)
				warned = true
			}
			select {
			case <-ctx.Done():
			case <-time.After(captureErrorPause):
			}
			continue
		}
		ll, ok := from.(*syscall.SockaddrLinklayer)
		if !ok || ll.Pkttype == syscall.PACKET_OUTGOING {
			continue
		}
		key, mac, syn, ok := ParseTCPFrame(buf[:n], port)
		if !ok {
			log.Debug("hostpower: skipped captured frame: not a TCP segment for the listen port", "ifindex", ll.Ifindex, "len", n)
			continue
		}
		name, ok := interfaceName(names, ll.Ifindex)
		if !ok {
			log.Debug("hostpower: skipped captured frame: interface gone", "ifindex", ll.Ifindex)
			continue
		}
		table.Add(key, Observation{Ifname: name, SrcMAC: mac, SYN: syn})
	}
}

// interfaceName maps an interface index to its name, asking the kernel on a
// cache miss. ok is false when the interface no longer exists.
func interfaceName(cache map[int]string, index int) (string, bool) {
	if name, ok := cache[index]; ok {
		return name, true
	}
	ifi, err := net.InterfaceByIndex(index)
	if err != nil {
		return "", false
	}
	cache[index] = ifi.Name
	return ifi.Name, true
}
