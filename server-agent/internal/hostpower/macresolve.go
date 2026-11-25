// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package hostpower

import (
	"bytes"
	"context"
	"encoding/binary"
	"net"
	"net/netip"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// DefaultPVEConfDir is where Proxmox VE keeps the QEMU VM configs.
const DefaultPVEConfDir = "/etc/pve/qemu-server"

const (
	maxTextFileBytes = 64 << 10

	// defaultMaxWait bounds how long Resolve waits for the capture goroutine
	// to deliver the connection's SYN. The SYN always precedes the request;
	// this only covers scheduling delay.
	defaultMaxWait = 250 * time.Millisecond

	// synBurstWindow groups the copies of one SYN frame. A frame from a VM
	// is seen on its tap, the firewall devices and the bridge within
	// microseconds; an observation for the same 4-tuple that starts later
	// than this belongs to a different SYN and replaces the earlier set, so
	// a later connection reusing a 4-tuple never inherits an older tap
	// observation.
	synBurstWindow = 200 * time.Millisecond

	// Table bounds. A VM's SYN yields at most about six observations (tap,
	// fwbr, fwln, fwpr, bridge, VLAN device).
	maxObservedKeys       = 16384
	maxObservationsPerKey = 16
)

// Reasons carried by ResolveError.
const (
	reasonIPv6        = "ipv6 not supported"
	reasonNoSYN       = "no SYN observed"
	reasonNotVMPort   = "not a VM port"
	reasonAmbiguous   = "ambiguous ingress"
	reasonNoVMConfig  = "no VM config"
	reasonMACMismatch = "MAC mismatch"
)

// tapPortPattern matches the QEMU tap device of VM <vmid>, NIC <n>. Only these
// count as VM ingress ports; the PVE firewall's fwbr/fwln/fwpr devices and the
// bridge see copies of the same frame and are ignored.
var tapPortPattern = regexp.MustCompile(`^tap(\d+)i(\d+)$`)

// ResolveError reports why a connection could not be tied to one VM.
type ResolveError struct{ Reason string }

func (e *ResolveError) Error() string { return "hostpower: " + e.Reason }

func resolveFail(reason string) (string, error) { return "", &ResolveError{Reason: reason} }

// ConnKey is a TCP connection's 4-tuple as seen in its SYN: Src is the client,
// Dst the listener. Both addresses are unmapped IPv4.
type ConnKey struct {
	Src, Dst netip.AddrPort
}

// Observation is one copy of a SYN frame: the interface the kernel received
// it on and the frame's Ethernet source address.
type Observation struct {
	Ifname string
	SrcMAC net.HardwareAddr
	At     time.Time
}

// --- frame parsing ---

const (
	ethHeaderLen  = 14
	vlanTagLen    = 4
	etherTypeIPv4 = 0x0800
	etherTypeVLAN = 0x8100
	ipv4MinHeader = 20
	ipProtoTCP    = 6
	ipFragOffset  = 0x1fff // fragment offset bits of the flags/offset field
	tcpMinHeader  = 20
	tcpFlagSYN    = 0x02
	tcpFlagACK    = 0x10
)

// ParseSYNFrame reports whether frame is an Ethernet II frame (optionally
// with one 802.1Q tag) carrying an IPv4 TCP segment with SYN set, ACK clear
// and destination port dstPort, and returns its 4-tuple and source MAC. The
// frame may end right after the TCP header. It never panics; anything
// malformed or short yields ok == false.
func ParseSYNFrame(frame []byte, dstPort uint16) (key ConnKey, src net.HardwareAddr, ok bool) {
	if len(frame) < ethHeaderLen {
		return ConnKey{}, nil, false
	}
	l3 := ethHeaderLen
	etherType := binary.BigEndian.Uint16(frame[12:14])
	if etherType == etherTypeVLAN {
		if len(frame) < ethHeaderLen+vlanTagLen {
			return ConnKey{}, nil, false
		}
		l3 += vlanTagLen
		etherType = binary.BigEndian.Uint16(frame[16:18])
	}
	if etherType != etherTypeIPv4 {
		return ConnKey{}, nil, false
	}

	ip := frame[l3:]
	if len(ip) < ipv4MinHeader || ip[0]>>4 != 4 {
		return ConnKey{}, nil, false
	}
	ihl := int(ip[0]&0x0f) * 4
	if ihl < ipv4MinHeader || ip[9] != ipProtoTCP || binary.BigEndian.Uint16(ip[6:8])&ipFragOffset != 0 {
		return ConnKey{}, nil, false
	}
	if len(ip) < ihl+tcpMinHeader {
		return ConnKey{}, nil, false
	}
	tcp := ip[ihl:]
	if binary.BigEndian.Uint16(tcp[2:4]) != dstPort || tcp[13]&(tcpFlagSYN|tcpFlagACK) != tcpFlagSYN {
		return ConnKey{}, nil, false
	}

	key = ConnKey{
		Src: netip.AddrPortFrom(netip.AddrFrom4([4]byte(ip[12:16])), binary.BigEndian.Uint16(tcp[0:2])),
		Dst: netip.AddrPortFrom(netip.AddrFrom4([4]byte(ip[16:20])), dstPort),
	}
	return key, bytes.Clone(frame[6:12]), true
}

// --- kernel packet filter ---

// captureSnapLen is how much of each frame the capture reads: enough for the
// largest Ethernet + 802.1Q + IPv4 (60) + TCP (20) header.
const captureSnapLen = 256

// bpfInsn is one classic BPF instruction (struct sock_filter). It is kept
// platform-neutral so the program is unit-tested on every build target; the
// Linux capture converts it to syscall.SockFilter.
type bpfInsn struct {
	Code   uint16
	Jt, Jf uint8
	K      uint32
}

// Classic BPF opcode parts (linux/filter.h).
const (
	bpfLD   = 0x00
	bpfLDX  = 0x01
	bpfALU  = 0x04
	bpfJMP  = 0x05
	bpfRET  = 0x06
	bpfW    = 0x00
	bpfH    = 0x08
	bpfB    = 0x10
	bpfABS  = 0x20
	bpfIND  = 0x40
	bpfMSH  = 0xa0
	bpfK    = 0x00
	bpfJEQ  = 0x10
	bpfJSET = 0x40
	bpfAND  = 0x50
)

// synFilter returns a classic BPF program that passes (the first
// captureSnapLen bytes of) Ethernet frames carrying an IPv4 TCP segment to
// port with SYN set and ACK clear, untagged or with one 802.1Q tag, and drops
// everything else. The filter is only an optimisation: the capture re-checks
// every frame with ParseSYNFrame, so a filter that passed too much would cost
// CPU, never correctness.
//
// Jump offsets count instructions skipped after the jump; "rej" is
// instruction 25, "acc" 26. Linux usually strips the VLAN tag into skb
// metadata before packet sockets see the frame, in which case the untagged
// branch matches; the tagged branch covers a tag left in the data.
func synFilter(port uint16) []bpfInsn {
	p := uint32(port)
	return []bpfInsn{
		/* 0 */ {Code: bpfLD | bpfH | bpfABS, K: 12}, // A = ethertype
		/* 1 */ {Code: bpfJMP | bpfJEQ | bpfK, K: etherTypeVLAN, Jt: 11}, // tagged → 13
		// Untagged: IPv4 header at 14.
		/* 2 */ {Code: bpfJMP | bpfJEQ | bpfK, K: etherTypeIPv4, Jf: 22}, // not IPv4 → rej
		/* 3 */ {Code: bpfLD | bpfB | bpfABS, K: 14 + 9}, // A = IP protocol
		/* 4 */ {Code: bpfJMP | bpfJEQ | bpfK, K: ipProtoTCP, Jf: 20}, // not TCP → rej
		/* 5 */ {Code: bpfLD | bpfH | bpfABS, K: 14 + 6}, // A = IP flags/fragment offset
		/* 6 */ {Code: bpfJMP | bpfJSET | bpfK, K: ipFragOffset, Jt: 18}, // non-first fragment → rej
		/* 7 */ {Code: bpfLDX | bpfB | bpfMSH, K: 14}, // X = 4 * IHL
		/* 8 */ {Code: bpfLD | bpfH | bpfIND, K: 14 + 2}, // A = TCP dst port
		/* 9 */ {Code: bpfJMP | bpfJEQ | bpfK, K: p, Jf: 15}, // other port → rej
		/* 10 */ {Code: bpfLD | bpfB | bpfIND, K: 14 + 13}, // A = TCP flags
		/* 11 */ {Code: bpfALU | bpfAND | bpfK, K: tcpFlagSYN | tcpFlagACK},
		/* 12 */ {Code: bpfJMP | bpfJEQ | bpfK, K: tcpFlagSYN, Jt: 13, Jf: 12}, // SYN only → acc, else rej
		// One 802.1Q tag: inner ethertype at 16, IPv4 header at 18.
		/* 13 */ {Code: bpfLD | bpfH | bpfABS, K: 16}, // A = inner ethertype
		/* 14 */ {Code: bpfJMP | bpfJEQ | bpfK, K: etherTypeIPv4, Jf: 10}, // not IPv4 → rej
		/* 15 */ {Code: bpfLD | bpfB | bpfABS, K: 18 + 9}, // A = IP protocol
		/* 16 */ {Code: bpfJMP | bpfJEQ | bpfK, K: ipProtoTCP, Jf: 8}, // not TCP → rej
		/* 17 */ {Code: bpfLD | bpfH | bpfABS, K: 18 + 6}, // A = IP flags/fragment offset
		/* 18 */ {Code: bpfJMP | bpfJSET | bpfK, K: ipFragOffset, Jt: 6}, // non-first fragment → rej
		/* 19 */ {Code: bpfLDX | bpfB | bpfMSH, K: 18}, // X = 4 * IHL
		/* 20 */ {Code: bpfLD | bpfH | bpfIND, K: 18 + 2}, // A = TCP dst port
		/* 21 */ {Code: bpfJMP | bpfJEQ | bpfK, K: p, Jf: 3}, // other port → rej
		/* 22 */ {Code: bpfLD | bpfB | bpfIND, K: 18 + 13}, // A = TCP flags
		/* 23 */ {Code: bpfALU | bpfAND | bpfK, K: tcpFlagSYN | tcpFlagACK},
		/* 24 */ {Code: bpfJMP | bpfJEQ | bpfK, K: tcpFlagSYN, Jt: 1}, // SYN only → acc, else rej
		/* 25 rej */ {Code: bpfRET | bpfK, K: 0},
		/* 26 acc */ {Code: bpfRET | bpfK, K: captureSnapLen},
	}
}

// --- observation table ---

type observedSYN struct {
	start    time.Time // first observation of this SYN burst
	obs      []Observation
	overflow bool // more distinct copies than maxObservationsPerKey
}

type queuedKey struct {
	key   ConnKey
	start time.Time
}

// ObservationTable holds recent SYN observations by 4-tuple. It is safe for
// concurrent use.
type ObservationTable struct {
	ttl time.Duration
	now func() time.Time

	mu      sync.Mutex
	entries map[ConnKey]*observedSYN
	order   []queuedKey   // insertion order, for expiry and eviction
	changed chan struct{} // closed and replaced on every Add
}

// NewObservationTable returns a table whose observations expire after ttl,
// measured with now.
func NewObservationTable(ttl time.Duration, now func() time.Time) *ObservationTable {
	return &ObservationTable{
		ttl:     ttl,
		now:     now,
		entries: make(map[ConnKey]*observedSYN),
		changed: make(chan struct{}),
	}
}

// Add records one copy of a SYN. A zero obs.At is set to the table's clock.
// Expired entries are purged first; when the table is full the oldest entry
// is evicted.
func (t *ObservationTable) Add(key ConnKey, obs Observation) {
	now := t.now()
	if obs.At.IsZero() {
		obs.At = now
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.purgeLocked(now)

	e := t.entries[key]
	if e != nil && obs.At.Sub(e.start) > synBurstWindow {
		e = nil // a later SYN for this 4-tuple: forget the earlier one
	}
	if e == nil {
		for len(t.entries) >= maxObservedKeys && len(t.order) > 0 {
			t.popOldestLocked()
		}
		e = &observedSYN{start: obs.At}
		t.entries[key] = e
		t.order = append(t.order, queuedKey{key: key, start: obs.At})
		if len(t.order) > 2*maxObservedKeys {
			t.compactOrderLocked()
		}
	}
	if !e.overflow && !containsObservation(e.obs, obs) {
		if len(e.obs) >= maxObservationsPerKey {
			e.overflow, e.obs = true, nil
		} else {
			e.obs = append(e.obs, obs)
		}
	}
	close(t.changed)
	t.changed = make(chan struct{})
}

func containsObservation(list []Observation, o Observation) bool {
	for _, x := range list {
		if x.Ifname == o.Ifname && bytes.Equal(x.SrcMAC, o.SrcMAC) {
			return true
		}
	}
	return false
}

func (t *ObservationTable) expired(e *observedSYN, now time.Time) bool {
	return now.Sub(e.start) > t.ttl
}

// purgeLocked drops entries older than ttl from the front of the insertion
// order.
func (t *ObservationTable) purgeLocked(now time.Time) {
	for len(t.order) > 0 {
		q := t.order[0]
		if e := t.entries[q.key]; e != nil && e.start.Equal(q.start) && !t.expired(e, now) {
			return
		}
		t.popOldestLocked()
	}
}

// popOldestLocked removes the front of the insertion order and, unless the
// key was replaced by a later SYN since, its entry. The order must not be
// empty.
func (t *ObservationTable) popOldestLocked() {
	q := t.order[0]
	t.order[0] = queuedKey{}
	t.order = t.order[1:]
	if e := t.entries[q.key]; e != nil && e.start.Equal(q.start) {
		delete(t.entries, q.key)
	}
	if len(t.order) == 0 {
		t.order = nil // let the backing array go
	}
}

// compactOrderLocked drops queue items left behind by replaced SYNs, so the
// order stays proportional to the live entries.
func (t *ObservationTable) compactOrderLocked() {
	live := make([]queuedKey, 0, len(t.entries))
	for _, q := range t.order {
		if e := t.entries[q.key]; e != nil && e.start.Equal(q.start) {
			live = append(live, q)
		}
	}
	t.order = live
}

// Wait returns the live observations for key, waiting up to maxWait for the
// first one to arrive. It returns nil on timeout, when ctx ends, or when the
// key received more distinct copies than the table keeps.
func (t *ObservationTable) Wait(ctx context.Context, key ConnKey, maxWait time.Duration) []Observation {
	var timeout <-chan time.Time
	if maxWait > 0 {
		timer := time.NewTimer(maxWait)
		defer timer.Stop()
		timeout = timer.C
	}
	for {
		t.mu.Lock()
		var got []Observation
		if e := t.entries[key]; e != nil && !t.expired(e, t.now()) {
			got = append(got, e.obs...)
		}
		changed := t.changed
		t.mu.Unlock()
		if len(got) > 0 || timeout == nil {
			return got
		}
		select {
		case <-changed:
		case <-timeout:
			return nil
		case <-ctx.Done():
			return nil
		}
	}
}

// --- resolver ---

// IngressResolver identifies the VM a TCP connection came from by the tap
// device its SYN entered and the SYN's source MAC. Empty fields use defaults.
type IngressResolver struct {
	Table      *ObservationTable
	PVEConfDir string
	MaxWait    time.Duration
}

// Resolve returns the ID of the VM that opened the connection from remote to
// local, or a *ResolveError.
func (r *IngressResolver) Resolve(ctx context.Context, remote, local netip.AddrPort) (vmid string, err error) {
	src := netip.AddrPortFrom(remote.Addr().Unmap(), remote.Port())
	dst := netip.AddrPortFrom(local.Addr().Unmap(), local.Port())
	if !src.Addr().Is4() || !dst.Addr().Is4() {
		return resolveFail(reasonIPv6)
	}
	if r.Table == nil {
		return resolveFail(reasonNoSYN)
	}
	maxWait := r.MaxWait
	if maxWait <= 0 {
		maxWait = defaultMaxWait
	}
	obs := r.Table.Wait(ctx, ConnKey{Src: src, Dst: dst}, maxWait)
	if len(obs) == 0 {
		return resolveFail(reasonNoSYN)
	}

	var tap *Observation
	var nic string
	for i := range obs {
		m := tapPortPattern.FindStringSubmatch(obs[i].Ifname)
		if m == nil {
			continue
		}
		if tap != nil {
			return resolveFail(reasonAmbiguous) // deduplicated: a second tap or a second MAC
		}
		tap, vmid, nic = &obs[i], m[1], m[2]
	}
	if tap == nil {
		return resolveFail(reasonNotVMPort)
	}

	dir := r.PVEConfDir
	if dir == "" {
		dir = DefaultPVEConfDir
	}
	conf, ok, err := readCapped(filepath.Join(dir, vmid+".conf"), maxTextFileBytes)
	if err != nil || !ok {
		return resolveFail(reasonNoVMConfig)
	}
	if mac := configNICMAC(conf, nic); mac == nil || !bytes.Equal(mac, tap.SrcMAC) {
		return resolveFail(reasonMACMismatch)
	}
	return vmid, nil
}

// configNICMAC returns the MAC of the VM config's current net<nic> line: the
// first key=value that parses as a 6-byte MAC. Lines from the first [section]
// on (snapshots, pending changes) do not count.
func configNICMAC(conf []byte, nic string) net.HardwareAddr {
	prefix := "net" + nic + ":"
	for line := range strings.SplitSeq(string(conf), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[") {
			return nil
		}
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		for kv := range strings.SplitSeq(strings.TrimPrefix(line, prefix), ",") {
			_, val, found := strings.Cut(strings.TrimSpace(kv), "=")
			if !found {
				continue
			}
			if hw, err := net.ParseMAC(val); err == nil && len(hw) == 6 {
				return hw
			}
		}
		return nil
	}
	return nil
}
