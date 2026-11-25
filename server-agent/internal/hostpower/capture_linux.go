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
	// captureReadLen is how much of each frame the capture copies to
	// userspace: enough for the largest Ethernet + 802.1Q + IPv4 (60) + TCP
	// (20) header.
	captureReadLen     = 256
	captureReadTimeout = 500 * time.Millisecond
	captureErrorPause  = 100 * time.Millisecond
	// captureRcvBuf absorbs bursts while the read loop is descheduled. A
	// dropped frame can only hide evidence, so drops fail closed, but they
	// can make a VM's request fail. Counting drops via PACKET_STATISTICS is
	// deferred.
	captureRcvBuf = 4 << 20

	// solPacket and packetIgnoreOutgoing are SOL_PACKET and
	// PACKET_IGNORE_OUTGOING from linux/socket.h and linux/if_packet.h
	// (Linux 4.20+); package syscall does not define the latter.
	solPacket            = 263
	packetIgnoreOutgoing = 23
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
// ctx ends. It returns an error when the socket cannot be set up (no
// CAP_NET_RAW, filter rejected); the read loop runs in its own goroutine and
// keeps the table's capture-health flag current.
func StartCapture(ctx context.Context, port uint16, table *ObservationTable, log *slog.Logger) error {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	fd, err := syscall.Socket(syscall.AF_PACKET, syscall.SOCK_RAW|syscall.SOCK_CLOEXEC, int(htons(syscall.ETH_P_ALL)))
	if err != nil {
		return fmt.Errorf("hostpower: open packet socket: %w", err)
	}
	// Let the kernel skip frames the host sends, which only cost CPU: the
	// read loop drops PACKET_OUTGOING frames itself, so an older kernel
	// without the option loses nothing but that saving.
	if err := syscall.SetsockoptInt(fd, solPacket, packetIgnoreOutgoing, 1); err != nil {
		log.Debug("hostpower: kernel cannot ignore outgoing frames on the packet socket", "err", err)
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
	// SO_RCVBUFFORCE (root) ignores net.core.rmem_max; SO_RCVBUF is capped
	// by it. A small buffer is not fatal.
	if syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_RCVBUFFORCE, captureRcvBuf) != nil {
		if err := syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_RCVBUF, captureRcvBuf); err != nil {
			log.Warn("hostpower: cannot enlarge the packet socket buffer", "err", err)
		}
	}
	table.setCaptureHealthy(true)
	go captureLoop(ctx, fd, port, table, log)
	return nil
}

// captureLoop reads frames until ctx ends, then closes fd and marks the
// capture unhealthy. SO_RCVTIMEO wakes it regularly so it notices ctx.
func captureLoop(ctx context.Context, fd int, port uint16, table *ObservationTable, log *slog.Logger) {
	defer func() {
		table.setCaptureHealthy(false)
		_ = syscall.Close(fd)
	}()
	health := &captureHealth{table: table}
	buf := make([]byte, captureReadLen)
	names := make(map[int]string)
	for ctx.Err() == nil {
		// MSG_TRUNC: n is the frame's full length even when only
		// captureReadLen bytes were copied.
		n, from, err := syscall.Recvfrom(fd, buf, syscall.MSG_TRUNC)
		if err != nil {
			switch {
			case errors.Is(err, syscall.EINTR):
			case errors.Is(err, syscall.EAGAIN): // idle timeout: the socket works
				if health.ok() {
					log.Info("hostpower: packet capture recovered")
				}
			default:
				if health.failed(time.Now()) {
					log.Warn("hostpower: packet capture read failing; MAC mode rejects requests until it recovers", "err", err)
				}
				select {
				case <-ctx.Done():
				case <-time.After(captureErrorPause):
				}
			}
			continue
		}
		if health.ok() {
			log.Info("hostpower: packet capture recovered")
		}
		ll, ok := from.(*syscall.SockaddrLinklayer)
		if !ok || ll.Pkttype == syscall.PACKET_OUTGOING {
			continue
		}
		if ll.Hatype != syscall.ARPHRD_ETHER && ll.Hatype != syscall.ARPHRD_LOOPBACK {
			log.Debug("hostpower: skipped captured frame: no Ethernet header", "ifindex", ll.Ifindex, "hatype", ll.Hatype)
			continue
		}
		key, mac, syn, ok := parseTCPFrame(buf[:min(n, len(buf))], n, port)
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
