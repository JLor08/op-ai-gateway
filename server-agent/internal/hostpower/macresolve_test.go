// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package hostpower

import (
	"context"
	"encoding/binary"
	"errors"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	testMACA   = "bc:24:11:00:00:64"
	testMACB   = "bc:24:11:00:00:65"
	testPort   = 9873
	tcpSYN     = 0x02
	tcpACK     = 0x10
	testIPA    = "192.0.2.50"
	testIPB    = "192.0.2.51"
	testHostIP = "192.0.2.10"
)

var (
	testSrc = netip.MustParseAddrPort(testIPA + ":40000")
	testDst = netip.MustParseAddrPort(testHostIP + ":9873")
)

func mustMAC(t *testing.T, s string) net.HardwareAddr {
	t.Helper()
	hw, err := net.ParseMAC(s)
	if err != nil {
		t.Fatal(err)
	}
	return hw
}

// synFrame describes a hand-built Ethernet/IPv4/TCP frame. Zero fields take
// the values of a plain SYN from testSrc to testDst.
type synFrame struct {
	srcMAC    string
	vlan      bool
	etherType uint16
	ihl       int // 32-bit words
	proto     byte
	fragField uint16 // flags + fragment offset
	flags     byte
	setFlags  bool // use flags even when zero
	src, dst  netip.AddrPort
	tcpBytes  int    // TCP bytes carried (default 20: a full header)
	totalLen  int    // IPv4 total length field (default: header + TCP bytes)
	pad       []byte // bytes after the IP packet (Ethernet padding)
}

func (f synFrame) bytes(t *testing.T) []byte {
	t.Helper()
	if f.srcMAC == "" {
		f.srcMAC = testMACA
	}
	if f.etherType == 0 {
		f.etherType = 0x0800
	}
	if f.ihl == 0 {
		f.ihl = 5
	}
	if f.proto == 0 {
		f.proto = 6
	}
	if !f.setFlags && f.flags == 0 {
		f.flags = tcpSYN
	}
	if !f.src.IsValid() {
		f.src = testSrc
	}
	if !f.dst.IsValid() {
		f.dst = testDst
	}
	b := make([]byte, 0, 128)
	b = append(b, 0x02, 0, 0, 0, 0, 0x01) // dst MAC (host bridge)
	b = append(b, mustMAC(t, f.srcMAC)...)
	if f.vlan {
		b = binary.BigEndian.AppendUint16(b, 0x8100)
		b = binary.BigEndian.AppendUint16(b, 10) // VID 10
	}
	b = binary.BigEndian.AppendUint16(b, f.etherType)

	if f.tcpBytes == 0 {
		f.tcpBytes = 20
	}
	if f.totalLen == 0 {
		f.totalLen = f.ihl*4 + f.tcpBytes
	}
	// An IHL below 5 still gets a full 20-byte header, so only the field lies.
	ip := make([]byte, max(f.ihl, 5)*4)
	ip[0] = 0x40 | byte(f.ihl&0x0f)
	binary.BigEndian.PutUint16(ip[2:], uint16(f.totalLen))
	binary.BigEndian.PutUint16(ip[6:], f.fragField)
	ip[8] = 64
	ip[9] = f.proto
	s4, d4 := f.src.Addr().As4(), f.dst.Addr().As4()
	copy(ip[12:16], s4[:])
	copy(ip[16:20], d4[:])
	if f.ihl > 5 {
		ip[20] = 1 // NOP options
	}
	b = append(b, ip...)

	tcp := make([]byte, 20)
	binary.BigEndian.PutUint16(tcp[0:], f.src.Port())
	binary.BigEndian.PutUint16(tcp[2:], f.dst.Port())
	binary.BigEndian.PutUint32(tcp[4:], 0x01020304) // sequence number
	tcp[12] = 5 << 4                                // data offset
	tcp[13] = f.flags
	b = append(b, tcp[:f.tcpBytes]...)
	return append(b, f.pad...)
}

func TestParseTCPFrame(t *testing.T) {
	for _, tc := range []struct {
		name    string
		frame   synFrame
		wantSYN bool
	}{
		{"plain syn", synFrame{}, true},
		{"vlan tagged syn", synFrame{vlan: true}, true},
		{"ip options", synFrame{ihl: 6}, true},
		{"first fragment", synFrame{fragField: 0x2000}, false}, // MF set, offset 0: never a SYN
		{"don't fragment", synFrame{fragField: 0x4000}, true},
		{"syn with ece/cwr", synFrame{flags: tcpSYN | 0xc0}, true},
		{"pure ack", synFrame{flags: tcpACK}, false},
		{"vlan tagged ack", synFrame{vlan: true, flags: tcpACK}, false},
		{"psh-ack data", synFrame{flags: tcpACK | 0x08}, false},
		{"syn-ack", synFrame{flags: tcpSYN | tcpACK}, false},
		{"rst", synFrame{flags: 0x04}, false},
		{"no flags", synFrame{setFlags: true}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			key, src, syn, ok := ParseTCPFrame(tc.frame.bytes(t), testPort)
			if !ok {
				t.Fatal("ok = false, want true")
			}
			if syn != tc.wantSYN {
				t.Errorf("syn = %v, want %v", syn, tc.wantSYN)
			}
			if want := (ConnKey{Src: testSrc, Dst: testDst}); key != want {
				t.Errorf("key = %+v, want %+v", key, want)
			}
			if src.String() != testMACA {
				t.Errorf("src MAC = %s, want %s", src, testMACA)
			}
		})
	}
}

func TestParseTCPFrameCopiesMAC(t *testing.T) {
	frame := synFrame{}.bytes(t)
	_, src, _, ok := ParseTCPFrame(frame, testPort)
	if !ok {
		t.Fatal("ok = false")
	}
	for i := range frame {
		frame[i] = 0xff
	}
	if src.String() != testMACA {
		t.Errorf("src MAC aliases the frame buffer: %s", src)
	}
}

func TestParseTCPFrameFirstFragment(t *testing.T) {
	// A first fragment (MF set, offset 0) carries the ports, so its ingress
	// is recorded; its flags are never trusted, so it is never a SYN.
	for _, tc := range []struct {
		name  string
		frame synFrame
	}{
		{"8 tcp bytes", synFrame{fragField: 0x2000, tcpBytes: 8}},
		{"4 tcp bytes", synFrame{fragField: 0x2000, tcpBytes: 4}},
		{"vlan 8 tcp bytes", synFrame{vlan: true, fragField: 0x2000, tcpBytes: 8}},
		{"full header with SYN flag", synFrame{fragField: 0x2000}},
		{"4 tcp bytes padded with SYN-looking bytes", synFrame{fragField: 0x2000, tcpBytes: 4, pad: []byte{0, 0, 0, 0, 0, 0, 0, 0, 0x50, tcpSYN, 0, 0, 0, 0, 0, 0}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			key, src, syn, ok := ParseTCPFrame(tc.frame.bytes(t), testPort)
			if !ok || syn {
				t.Fatalf("ok = %v, syn = %v; want ok, not a SYN", ok, syn)
			}
			if want := (ConnKey{Src: testSrc, Dst: testDst}); key != want || src.String() != testMACA {
				t.Errorf("key = %+v, src = %s; want %+v, %s", key, src, want, testMACA)
			}
		})
	}
	for _, tc := range []struct {
		name  string
		frame synFrame
		port  uint16
	}{
		{"3 tcp bytes", synFrame{fragField: 0x2000, tcpBytes: 3}, testPort},
		{"3 tcp bytes padded", synFrame{fragField: 0x2000, tcpBytes: 3, pad: make([]byte, 30)}, testPort},
		{"other port", synFrame{fragField: 0x2000, tcpBytes: 8}, testPort + 1},
		{"non-first fragment", synFrame{fragField: 0x2001, tcpBytes: 8}, testPort},
	} {
		t.Run("reject "+tc.name, func(t *testing.T) {
			if _, _, _, ok := ParseTCPFrame(tc.frame.bytes(t), tc.port); ok {
				t.Fatal("ok = true, want false")
			}
		})
	}
}

func TestParseTCPFrameTotalLength(t *testing.T) {
	t.Run("ethernet padding after the packet is ignored", func(t *testing.T) {
		_, _, syn, ok := ParseTCPFrame(synFrame{flags: tcpACK, pad: make([]byte, 6)}.bytes(t), testPort)
		if !ok || syn {
			t.Fatalf("ok = %v, syn = %v; want ok ACK", ok, syn)
		}
	})
	for _, tc := range []struct {
		name  string
		frame synFrame
	}{
		{"total length shorter than ip+tcp header", synFrame{totalLen: 20 + 12}},
		{"total length shorter than ip header", synFrame{totalLen: 16}},
		{"total length longer than the frame", synFrame{totalLen: 20 + 20 + 1}},
		// A 4-byte segment whose "flags" sit in the Ethernet padding.
		{"tcp header only in the padding", synFrame{tcpBytes: 4, pad: []byte{0, 0, 0, 0, 0, 0, 0, 0, 0x50, tcpSYN, 0, 0, 0, 0, 0, 0}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, _, ok := ParseTCPFrame(tc.frame.bytes(t), testPort); ok {
				t.Fatal("ok = true, want false")
			}
		})
	}
	t.Run("capture cut the frame after the headers", func(t *testing.T) {
		// A data segment longer than the read buffer: the capture passes the
		// prefix it read and the length the kernel reported.
		frame := synFrame{flags: tcpACK | 0x08, totalLen: 20 + 20 + 1200}.bytes(t)
		wire := len(frame) + 1200
		key, _, syn, ok := parseTCPFrame(frame, wire, testPort)
		if !ok || syn || key.Src != testSrc {
			t.Fatalf("ok = %v, syn = %v, key = %+v; want ok non-SYN from %v", ok, syn, key, testSrc)
		}
		if _, _, _, ok := parseTCPFrame(frame, wire-1, testPort); ok {
			t.Fatal("wire length shorter than the IPv4 total length: ok = true")
		}
	})
}

func TestParseTCPFrameRejects(t *testing.T) {
	for _, tc := range []struct {
		name  string
		frame synFrame
		port  uint16
	}{
		{"other dst port", synFrame{}, testPort + 1},
		{"ack to other dst port", synFrame{flags: tcpACK}, testPort + 1},
		{"ipv6 ethertype", synFrame{etherType: 0x86dd}, testPort},
		{"arp ethertype", synFrame{etherType: 0x0806}, testPort},
		{"udp", synFrame{proto: 17}, testPort},
		{"ihl 4", synFrame{ihl: 4}, testPort},
		{"non-first fragment", synFrame{fragField: 0x0001}, testPort},
		{"last fragment", synFrame{fragField: 0x00b9}, testPort},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, _, ok := ParseTCPFrame(tc.frame.bytes(t), tc.port); ok {
				t.Fatal("ok = true, want false")
			}
		})
	}

	t.Run("ip version 6 under ipv4 ethertype", func(t *testing.T) {
		frame := synFrame{}.bytes(t)
		frame[14] = 0x65
		if _, _, _, ok := ParseTCPFrame(frame, testPort); ok {
			t.Fatal("ok = true, want false")
		}
	})

	t.Run("double vlan tag", func(t *testing.T) {
		frame := synFrame{vlan: true}.bytes(t)
		frame = append(frame[:16:16], append([]byte{0x81, 0x00, 0x00, 0x14}, frame[16:]...)...)
		if _, _, _, ok := ParseTCPFrame(frame, testPort); ok {
			t.Fatal("ok = true, want false")
		}
	})

	for _, f := range []synFrame{{}, {vlan: true}, {ihl: 6}, {flags: tcpACK}} {
		frame := f.bytes(t)
		for n := range len(frame) {
			if _, _, _, ok := ParseTCPFrame(frame[:n], testPort); ok {
				t.Errorf("vlan=%v ihl=%d truncated to %d bytes: ok = true", f.vlan, f.ihl, n)
			}
		}
	}
	if _, _, _, ok := ParseTCPFrame(nil, testPort); ok {
		t.Error("nil frame: ok = true")
	}
}

// runBPF interprets the subset of classic BPF that tcpPortFilter uses, with the
// kernel's rule that an out-of-bounds load rejects the packet.
func runBPF(t *testing.T, prog []bpfInsn, pkt []byte) uint32 {
	t.Helper()
	var a, x uint32
	load := func(off uint32, size int) (uint32, bool) {
		if uint64(off)+uint64(size) > uint64(len(pkt)) {
			return 0, false
		}
		switch size {
		case 1:
			return uint32(pkt[off]), true
		case 2:
			return uint32(binary.BigEndian.Uint16(pkt[off:])), true
		}
		return binary.BigEndian.Uint32(pkt[off:]), true
	}
	sizes := map[uint16]int{bpfW: 4, bpfH: 2, bpfB: 1}
	for pc := 0; pc < len(prog); pc++ {
		in := prog[pc]
		var ok bool
		switch {
		case in.Code&0x07 == bpfLD && in.Code&0xe0 == bpfABS:
			a, ok = load(in.K, sizes[in.Code&0x18])
		case in.Code&0x07 == bpfLD && in.Code&0xe0 == bpfIND:
			a, ok = load(x+in.K, sizes[in.Code&0x18])
		case in.Code == bpfLDX|bpfB|bpfMSH:
			var v uint32
			v, ok = load(in.K, 1)
			x = 4 * (v & 0x0f)
		case in.Code == bpfALU|bpfAND|bpfK:
			a &= in.K
			ok = true
		case in.Code == bpfJMP|bpfJEQ|bpfK:
			if a == in.K {
				pc += int(in.Jt)
			} else {
				pc += int(in.Jf)
			}
			ok = true
		case in.Code == bpfJMP|bpfJSET|bpfK:
			if a&in.K != 0 {
				pc += int(in.Jt)
			} else {
				pc += int(in.Jf)
			}
			ok = true
		case in.Code == bpfRET|bpfK:
			return in.K
		default:
			t.Fatalf("pc %d: unsupported opcode %#x", pc, in.Code)
		}
		if !ok {
			return 0
		}
	}
	t.Fatal("program ran off its end")
	return 0
}

func TestTCPPortFilterProgram(t *testing.T) {
	prog := tcpPortFilter(testPort)
	for _, f := range []synFrame{
		{},
		{vlan: true},
		{ihl: 6},
		{vlan: true, ihl: 15},
		{fragField: 0x2000},
		{flags: tcpACK},
		{vlan: true, flags: tcpACK},
		{flags: tcpACK | 0x08},
		{flags: tcpSYN | tcpACK},
		{ihl: 6, flags: tcpACK},
		{fragField: 0x2000, tcpBytes: 8},
		{fragField: 0x2000, tcpBytes: 4},
		{vlan: true, fragField: 0x2000, tcpBytes: 4},
	} {
		if got := runBPF(t, prog, f.bytes(t)); got != bpfAcceptLen {
			t.Errorf("accept vlan=%v ihl=%d frag=%#x flags=%#x tcp=%d: filter returned %d, want %d", f.vlan, f.ihl, f.fragField, f.flags, f.tcpBytes, got, bpfAcceptLen)
		}
	}
	for _, tc := range []struct {
		name  string
		frame synFrame
		port  uint16
	}{
		{"other port", synFrame{}, testPort + 1},
		{"ack to other port", synFrame{flags: tcpACK}, testPort + 1},
		{"vlan other port", synFrame{vlan: true}, testPort + 1},
		{"vlan ack to other port", synFrame{vlan: true, flags: tcpACK}, testPort + 1},
		{"ipv6", synFrame{etherType: 0x86dd}, testPort},
		{"udp", synFrame{proto: 17}, testPort},
		{"vlan udp", synFrame{vlan: true, proto: 17}, testPort},
		{"non-first fragment", synFrame{fragField: 0x0001}, testPort},
		{"vlan non-first fragment", synFrame{vlan: true, fragField: 0x0001}, testPort},
		{"first fragment to other port", synFrame{fragField: 0x2000, tcpBytes: 8}, testPort + 1},
		{"first fragment with 3 tcp bytes", synFrame{fragField: 0x2000, tcpBytes: 3}, testPort},
	} {
		if got := runBPF(t, tcpPortFilter(tc.port), tc.frame.bytes(t)); got != 0 {
			t.Errorf("reject %s: filter returned %d, want 0", tc.name, got)
		}
	}
	if got := runBPF(t, prog, synFrame{}.bytes(t)[:30]); got != 0 {
		t.Errorf("truncated frame: filter returned %d, want 0", got)
	}
}

// --- observation table and resolver ---

type resolverFixture struct {
	table *ObservationTable
	res   *IngressResolver
	dir   string
	sys   string
}

// newResolverFixture writes the VM configs and a fake /sys/class/net in which
// vmbr0 and vmbr1 are bridges, enp3s0 is a plain interface, vmbr0.10 is a
// VLAN over vmbr0 and enp3s0.10 a VLAN over enp3s0.
func newResolverFixture(t *testing.T, confs map[string]string) *resolverFixture {
	t.Helper()
	dir := t.TempDir()
	for vmid, content := range confs {
		if err := os.WriteFile(filepath.Join(dir, vmid+".conf"), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	sys := t.TempDir()
	for _, p := range []string{"vmbr0/bridge", "vmbr1/bridge", "enp3s0"} {
		if err := os.MkdirAll(filepath.Join(sys, p), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	fakeNetDev(t, sys, "vmbr0.10", "DEVTYPE=vlan", "vmbr0")
	fakeNetDev(t, sys, "enp3s0.10", "DEVTYPE=vlan", "enp3s0")
	table := NewObservationTable(30*time.Second, time.Now)
	table.setCaptureHealthy(true)
	return &resolverFixture{
		table: table,
		res:   &IngressResolver{Table: table, PVEConfDir: dir, SysClassNet: sys, MaxWait: 50 * time.Millisecond},
		dir:   dir,
		sys:   sys,
	}
}

// fakeNetDev creates <sys>/<name> with a uevent holding devtype (empty: no
// DEVTYPE line) and, as real sysfs does, a lower_<lower> symlink per lower
// device.
func fakeNetDev(t *testing.T, sys, name, devtype string, lowers ...string) {
	t.Helper()
	dir := filepath.Join(sys, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	uevent := "INTERFACE=" + name + "\nIFINDEX=42\n"
	if devtype != "" {
		uevent += devtype + "\n"
	}
	if err := os.WriteFile(filepath.Join(dir, "uevent"), []byte(uevent), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, lower := range lowers {
		link := filepath.Join(dir, "lower_"+lower)
		if os.Symlink(filepath.Join("..", lower), link) != nil {
			// Only the entry's name is read; a plain file stands in where
			// symlinks need privileges (Windows).
			if err := os.WriteFile(link, nil, 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func (f *resolverFixture) add(t *testing.T, key ConnKey, mac string, syn bool, ifnames ...string) {
	t.Helper()
	for _, name := range ifnames {
		f.table.Add(key, Observation{Ifname: name, SrcMAC: mustMAC(t, mac), SYN: syn})
	}
}

// see records copies of the connection's SYN.
func (f *resolverFixture) see(t *testing.T, key ConnKey, mac string, ifnames ...string) {
	t.Helper()
	f.add(t, key, mac, true, ifnames...)
}

// ack records copies of a later (non-SYN) segment of the connection.
func (f *resolverFixture) ack(t *testing.T, key ConnKey, mac string, ifnames ...string) {
	t.Helper()
	f.add(t, key, mac, false, ifnames...)
}

// handshake records a SYN and its ACK, both entering on ifnames.
func (f *resolverFixture) handshake(t *testing.T, key ConnKey, mac string, ifnames ...string) {
	t.Helper()
	f.see(t, key, mac, ifnames...)
	f.ack(t, key, mac, ifnames...)
}

func conf(mac string) string {
	return "boot: order=scsi0\nnet0: virtio=" + strings.ToUpper(mac) + ",bridge=vmbr0,firewall=1\n"
}

func wantVMID(t *testing.T, gotVMID string, err error, want string) {
	t.Helper()
	if err != nil || gotVMID != want {
		t.Fatalf("Resolve = %q, %v; want %q", gotVMID, err, want)
	}
}

func wantReason(t *testing.T, gotVMID string, err error, want string) {
	t.Helper()
	var re *ResolveError
	if !errors.As(err, &re) {
		t.Fatalf("Resolve = %q, %v; want *ResolveError %q", gotVMID, err, want)
	}
	if re.Reason != want || gotVMID != "" {
		t.Fatalf("Resolve = %q, reason %q; want reason %q", gotVMID, re.Reason, want)
	}
}

var testKey = ConnKey{Src: testSrc, Dst: testDst}

func (f *resolverFixture) resolve() (string, error) {
	return f.res.Resolve(context.Background(), testSrc, testDst)
}

func TestResolveSingleTap(t *testing.T) {
	f := newResolverFixture(t, map[string]string{"100": conf(testMACA)})
	f.handshake(t, testKey, testMACA, "tap100i0", "vmbr0")
	vmid, err := f.resolve()
	wantVMID(t, vmid, err, "100")
}

func TestResolveFirewallCopiesIgnored(t *testing.T) {
	f := newResolverFixture(t, map[string]string{"101": conf(testMACA)})
	f.handshake(t, testKey, testMACA, "tap101i0", "fwbr101i0", "fwln101i0", "fwpr101p0", "vmbr0")
	vmid, err := f.resolve()
	wantVMID(t, vmid, err, "101")
}

func TestResolveUplinkOnly(t *testing.T) {
	f := newResolverFixture(t, map[string]string{"100": conf(testMACA)})
	f.handshake(t, testKey, testMACA, "enp3s0", "vmbr0")
	vmid, err := f.resolve()
	wantReason(t, vmid, err, "not a VM port")
}

func TestResolveFirewallPortWithoutTap(t *testing.T) {
	f := newResolverFixture(t, map[string]string{"101": conf(testMACA)})
	f.handshake(t, testKey, testMACA, "fwpr101p0", "fwbr101i0", "vmbr0", "veth200i0")
	vmid, err := f.resolve()
	wantReason(t, vmid, err, "not a VM port")
}

func TestResolveTwoTaps(t *testing.T) {
	f := newResolverFixture(t, map[string]string{"100": conf(testMACA), "101": conf(testMACA)})
	f.handshake(t, testKey, testMACA, "tap100i0", "tap101i0")
	vmid, err := f.resolve()
	wantReason(t, vmid, err, "ambiguous ingress")
}

func TestResolveOneTapTwoMACs(t *testing.T) {
	f := newResolverFixture(t, map[string]string{"100": conf(testMACA)})
	f.see(t, testKey, testMACA, "tap100i0")
	f.see(t, testKey, testMACB, "tap100i0")
	f.ack(t, testKey, testMACA, "tap100i0")
	vmid, err := f.resolve()
	wantReason(t, vmid, err, "ambiguous ingress")
}

func TestResolveHijackOverUplinkRejected(t *testing.T) {
	// VM 100's SYN enters on its tap, but a device that ARP-poisoned the host
	// received the SYN-ACK and completes the handshake over the uplink.
	f := newResolverFixture(t, map[string]string{"100": conf(testMACA)})
	f.see(t, testKey, testMACA, "tap100i0", "vmbr0")
	f.ack(t, testKey, testMACB, "enp3s0", "vmbr0")
	vmid, err := f.resolve()
	wantReason(t, vmid, err, "foreign ingress")
}

func TestResolveHijackWithSpoofedMACRejected(t *testing.T) {
	// Same, with the attacker's frames carrying VM 100's MAC.
	f := newResolverFixture(t, map[string]string{"100": conf(testMACA)})
	f.see(t, testKey, testMACA, "tap100i0", "vmbr0")
	f.ack(t, testKey, testMACA, "enp3s0", "vmbr0")
	vmid, err := f.resolve()
	wantReason(t, vmid, err, "foreign ingress")
}

func TestResolveSYNCopyOnForeignInterface(t *testing.T) {
	f := newResolverFixture(t, map[string]string{"100": conf(testMACA)})
	f.see(t, testKey, testMACA, "tap100i0", "enp3s0")
	f.ack(t, testKey, testMACA, "tap100i0")
	vmid, err := f.resolve()
	wantReason(t, vmid, err, "foreign ingress")
}

func TestResolveTransitClassification(t *testing.T) {
	for _, name := range []string{"vmbr0", "vmbr1", "vmbr0.10", "fwbr100i0", "fwln100i0", "fwpr100p0", "fwbr7i3", "fwpr7p3"} {
		t.Run("transit "+name, func(t *testing.T) {
			f := newResolverFixture(t, map[string]string{"100": conf(testMACA)})
			f.handshake(t, testKey, testMACA, "tap100i0")
			f.ack(t, testKey, testMACA, name)
			vmid, err := f.resolve()
			wantVMID(t, vmid, err, "100")
		})
	}
	for _, name := range []string{
		"lo", "enp3s0", "enp3s0.10", "vmbr1.20", "veth200i0", "tap101i0", "tap100i1", "bond0",
		"fwbr100", "fwpr100i0", "xfwbr100i0", "fwln100i0x", "notabridge", "../vmbr0", "",
		"vlannolower", "vlanghost", "vlantwo", "macvlan0", "vlanoverfile",
	} {
		t.Run("foreign "+name, func(t *testing.T) {
			f := newResolverFixture(t, map[string]string{"100": conf(testMACA)})
			if err := os.MkdirAll(filepath.Join(f.sys, "notabridge"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(f.sys, "notabridge", "bridge"), nil, 0o644); err != nil {
				t.Fatal(err) // a file, not a directory
			}
			fakeNetDev(t, f.sys, "vlannolower", "DEVTYPE=vlan")                // no lower device
			fakeNetDev(t, f.sys, "vlanghost", "DEVTYPE=vlan", "ghost")         // lower does not exist
			fakeNetDev(t, f.sys, "vlantwo", "DEVTYPE=vlan", "vmbr0", "vmbr1")  // not a plain VLAN
			fakeNetDev(t, f.sys, "macvlan0", "DEVTYPE=macvlan", "vmbr0")       // over a bridge, not a VLAN
			fakeNetDev(t, f.sys, "vlanoverfile", "DEVTYPE=vlan", "notabridge") // lower's bridge is a file
			f.handshake(t, testKey, testMACA, "tap100i0")
			f.ack(t, testKey, testMACA, name)
			vmid, err := f.resolve()
			wantReason(t, vmid, err, "foreign ingress")
		})
	}
}

// fragmentOn records a first fragment of the connection, parsed from a frame,
// as received on ifname.
func (f *resolverFixture) fragmentOn(t *testing.T, mac, ifname string) {
	t.Helper()
	key, src, syn, ok := ParseTCPFrame(synFrame{srcMAC: mac, flags: tcpACK, fragField: 0x2000, tcpBytes: 8}.bytes(t), testPort)
	if !ok {
		t.Fatal("first fragment not parsed")
	}
	f.table.Add(key, Observation{Ifname: ifname, SrcMAC: src, SYN: syn})
}

func TestResolveFirstFragmentOnUplinkIsForeign(t *testing.T) {
	f := newResolverFixture(t, map[string]string{"100": conf(testMACA)})
	f.see(t, testKey, testMACA, "tap100i0", "vmbr0")
	f.fragmentOn(t, testMACB, "enp3s0")
	vmid, err := f.resolve()
	wantReason(t, vmid, err, "foreign ingress")
}

func TestResolveFirstFragmentOnTapCountsAsSegment(t *testing.T) {
	f := newResolverFixture(t, map[string]string{"100": conf(testMACA)})
	f.see(t, testKey, testMACA, "tap100i0", "vmbr0")
	f.fragmentOn(t, testMACA, "tap100i0")
	vmid, err := f.resolve()
	wantVMID(t, vmid, err, "100")
}

// clockFixture is a resolver fixture whose table runs on a settable clock.
func clockFixture(t *testing.T, confs map[string]string, clock *time.Time) *resolverFixture {
	t.Helper()
	f := newResolverFixture(t, confs)
	f.table = NewObservationTable(30*time.Second, func() time.Time { return *clock })
	f.table.setCaptureHealthy(true)
	f.res.Table = f.table
	return f
}

func TestResolveForeignMarkSurvivesNewSYN(t *testing.T) {
	clock := time.Unix(1_000_000, 0)
	f := clockFixture(t, map[string]string{"100": conf(testMACA)}, &clock)
	f.ack(t, testKey, testMACB, "enp3s0", "vmbr0")
	clock = clock.Add(time.Second)
	f.handshake(t, testKey, testMACA, "tap100i0", "vmbr0")
	vmid, err := f.resolve()
	wantReason(t, vmid, err, "foreign ingress")

	// And after yet another SYN replacement within the TTL.
	clock = clock.Add(time.Second)
	f.handshake(t, testKey, testMACA, "tap100i0", "vmbr0")
	vmid, err = f.resolve()
	wantReason(t, vmid, err, "foreign ingress")
}

func TestResolveForeignAfterSYNSurvivesNewSYN(t *testing.T) {
	clock := time.Unix(1_000_000, 0)
	f := clockFixture(t, map[string]string{"100": conf(testMACA)}, &clock)
	f.see(t, testKey, testMACA, "tap100i0")
	f.ack(t, testKey, testMACB, "enp3s0")
	clock = clock.Add(time.Second)
	f.handshake(t, testKey, testMACA, "tap100i0")
	vmid, err := f.resolve()
	wantReason(t, vmid, err, "foreign ingress")
}

func TestResolveNewSYNDropsOlderTapEvidence(t *testing.T) {
	// Replacement drops earlier positive evidence: an old tap observation
	// neither identifies nor taints the new connection.
	clock := time.Unix(1_000_000, 0)
	f := clockFixture(t, map[string]string{"100": conf(testMACA), "101": conf(testMACB)}, &clock)
	f.handshake(t, testKey, testMACA, "tap100i0", "vmbr0")
	clock = clock.Add(time.Second)
	f.handshake(t, testKey, testMACB, "tap101i0", "vmbr0")
	vmid, err := f.resolve()
	wantVMID(t, vmid, err, "101")
}

func TestResolveCarriedSegmentsDoNotCompleteHandshake(t *testing.T) {
	// Segments of an earlier connection on the 4-tuple must not satisfy the
	// handshake wait of the new one.
	clock := time.Unix(1_000_000, 0)
	f := clockFixture(t, map[string]string{"100": conf(testMACA)}, &clock)
	f.handshake(t, testKey, testMACA, "tap100i0", "vmbr0")
	clock = clock.Add(time.Second)
	f.see(t, testKey, testMACA, "tap100i0", "vmbr0")
	vmid, err := f.resolve()
	wantReason(t, vmid, err, "handshake not observed")
}

func TestResolveForeignMarkExpires(t *testing.T) {
	clock := time.Unix(1_000_000, 0)
	f := clockFixture(t, map[string]string{"100": conf(testMACA)}, &clock)
	f.ack(t, testKey, testMACB, "enp3s0")
	clock = clock.Add(31 * time.Second)
	f.handshake(t, testKey, testMACA, "tap100i0")
	vmid, err := f.resolve()
	wantVMID(t, vmid, err, "100")
}

func TestResolveCaptureUnavailable(t *testing.T) {
	f := newResolverFixture(t, map[string]string{"100": conf(testMACA)})
	f.handshake(t, testKey, testMACA, "tap100i0")
	f.table.setCaptureHealthy(false)
	vmid, err := f.resolve()
	wantReason(t, vmid, err, "capture unavailable")

	f.table.setCaptureHealthy(true)
	vmid, err = f.resolve()
	wantVMID(t, vmid, err, "100")
}

func TestResolveNewTableCaptureUnavailable(t *testing.T) {
	r := &IngressResolver{Table: NewObservationTable(30*time.Second, time.Now), MaxWait: time.Millisecond}
	vmid, err := r.Resolve(context.Background(), testSrc, testDst)
	wantReason(t, vmid, err, "capture unavailable")
}

func TestResolveObservationOverflow(t *testing.T) {
	f := newResolverFixture(t, map[string]string{"100": conf(testMACA)})
	f.see(t, testKey, testMACA, "tap100i0")
	for i := range maxObservationsPerKey {
		f.ack(t, testKey, testMACA, "vmbr0."+string(rune('a'+i%26))+string(rune('a'+i/26)))
	}
	vmid, err := f.resolve()
	wantReason(t, vmid, err, "observation overflow")
}

func TestResolveOverflowDuringHandshakeWait(t *testing.T) {
	f := newResolverFixture(t, map[string]string{"100": conf(testMACA)})
	f.res.MaxWait = 250 * time.Millisecond
	f.see(t, testKey, testMACA, "tap100i0")
	for i := range maxObservationsPerKey - 1 {
		f.see(t, testKey, testMACA, "vmbr0."+string(rune('a'+i%26))+string(rune('a'+i/26)))
	}
	go func() {
		time.Sleep(20 * time.Millisecond)
		f.ack(t, testKey, testMACA, "tap100i0") // one copy too many
	}()
	vmid, err := f.resolve()
	wantReason(t, vmid, err, "observation overflow")
}

func TestResolveForeignMarkExpiresAfterCarry(t *testing.T) {
	// A carried foreign mark lives ttl from its own observation, not from
	// the SYN that carried it.
	clock := time.Unix(1_000_000, 0)
	f := clockFixture(t, map[string]string{"100": conf(testMACA)}, &clock)
	f.ack(t, testKey, testMACB, "enp3s0")
	clock = clock.Add(29 * time.Second)
	f.handshake(t, testKey, testMACA, "tap100i0")
	vmid, err := f.resolve()
	wantReason(t, vmid, err, "foreign ingress")
	clock = clock.Add(2 * time.Second)
	vmid, err = f.resolve()
	wantVMID(t, vmid, err, "100")
}

func TestResolveForeignMarkExpiresFromLatestSighting(t *testing.T) {
	// Repeated foreign segments refresh the mark: it lasts ttl after the
	// latest sighting, not the first.
	t0 := time.Unix(1_000_000, 0)
	clock := t0.Add(time.Second)
	f := clockFixture(t, map[string]string{"100": conf(testMACA)}, &clock)
	f.ack(t, testKey, testMACB, "enp3s0")
	clock = t0.Add(25 * time.Second)
	f.ack(t, testKey, testMACB, "enp3s0")
	clock = t0.Add(26 * time.Second)
	f.handshake(t, testKey, testMACA, "tap100i0")
	for _, at := range []time.Duration{31 * time.Second, 54 * time.Second} {
		clock = t0.Add(at)
		vmid, err := f.resolve()
		wantReason(t, vmid, err, "foreign ingress")
	}
	clock = t0.Add(55*time.Second + 500*time.Millisecond)
	vmid, err := f.resolve()
	wantVMID(t, vmid, err, "100")
}

func TestResolveCollapsedForeignMarkKeepsLatestSighting(t *testing.T) {
	// An uplink SYN and a later uplink ACK collapse into one carried
	// observation, which must keep the later time.
	t0 := time.Unix(1_000_000, 0)
	clock := t0.Add(time.Second)
	f := clockFixture(t, map[string]string{"100": conf(testMACA)}, &clock)
	f.see(t, testKey, testMACB, "enp3s0")
	clock = t0.Add(25 * time.Second)
	f.ack(t, testKey, testMACB, "enp3s0")
	clock = t0.Add(26 * time.Second)
	f.handshake(t, testKey, testMACA, "tap100i0")
	clock = t0.Add(40 * time.Second)
	vmid, err := f.resolve()
	wantReason(t, vmid, err, "foreign ingress")
}

func TestResolveCarriedOverflowExpires(t *testing.T) {
	// An overflow mark survives SYN replacements only until ttl after the
	// overflow happened.
	t0 := time.Unix(1_000_000, 0)
	clock := t0
	f := clockFixture(t, map[string]string{"100": conf(testMACA)}, &clock)
	f.see(t, testKey, testMACA, "tap100i0")
	for i := range maxObservationsPerKey {
		f.ack(t, testKey, testMACA, "vmbr0."+string(rune('a'+i%26))+string(rune('a'+i/26)))
	}
	for _, at := range []time.Duration{10 * time.Second, 20 * time.Second, 29 * time.Second} {
		clock = t0.Add(at)
		f.handshake(t, testKey, testMACA, "tap100i0")
		vmid, err := f.resolve()
		wantReason(t, vmid, err, "observation overflow")
	}
	clock = t0.Add(31 * time.Second)
	f.handshake(t, testKey, testMACA, "tap100i0")
	vmid, err := f.resolve()
	wantVMID(t, vmid, err, "100")
}

func TestObservationTableDuplicateRefreshesTime(t *testing.T) {
	clock := time.Unix(1_000_000, 0)
	table := NewObservationTable(30*time.Second, func() time.Time { return clock })
	mac := mustMAC(t, testMACA)
	table.Add(testKey, Observation{Ifname: "tap100i0", SrcMAC: mac, SYN: true})
	table.Add(testKey, Observation{Ifname: "tap100i0", SrcMAC: mac})
	clock = clock.Add(5 * time.Second)
	table.Add(testKey, Observation{Ifname: "tap100i0", SrcMAC: mac})
	table.Add(testKey, Observation{Ifname: "tap100i0", SrcMAC: mac, At: clock.Add(-time.Second)}) // older: ignored
	got := table.Wait(context.Background(), testKey, 0)
	if len(got) != 2 || !got[1].At.Equal(clock) {
		t.Fatalf("got %+v; want the ACK observation at %v", got, clock)
	}
}

func TestResolveUplinkSYNWithoutHandshake(t *testing.T) {
	// The VM-port rule is decided as soon as the SYN is in; no handshake wait.
	f := newResolverFixture(t, map[string]string{"100": conf(testMACA)})
	f.res.MaxWait = 10 * time.Second
	f.see(t, testKey, testMACA, "enp3s0", "vmbr0")
	start := time.Now()
	vmid, err := f.resolve()
	wantReason(t, vmid, err, "not a VM port")
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("Resolve waited %v for a handshake of a non-VM SYN", d)
	}
}

func TestResolveSecondTapDuringHandshakeWait(t *testing.T) {
	f := newResolverFixture(t, map[string]string{"100": conf(testMACA), "101": conf(testMACA)})
	f.res.MaxWait = 250 * time.Millisecond
	f.see(t, testKey, testMACA, "tap100i0")
	go func() {
		time.Sleep(20 * time.Millisecond)
		f.see(t, testKey, testMACA, "tap101i0")
		f.ack(t, testKey, testMACA, "tap100i0")
	}()
	vmid, err := f.resolve()
	wantReason(t, vmid, err, "ambiguous ingress")
}

func TestResolveVLANOverBridgeIsTransit(t *testing.T) {
	// The host's address sits on a VLAN of a VLAN-aware bridge: a VM's frames
	// are also received on vmbr0.10.
	f := newResolverFixture(t, map[string]string{"100": conf(testMACA)})
	f.handshake(t, testKey, testMACA, "tap100i0", "vmbr0", "vmbr0.10")
	vmid, err := f.resolve()
	wantVMID(t, vmid, err, "100")
}

func TestResolveVLANOverNICIsForeign(t *testing.T) {
	// A LAN device's frames arrive on the uplink and its VLAN device.
	f := newResolverFixture(t, map[string]string{"100": conf(testMACA)})
	f.see(t, testKey, testMACA, "tap100i0", "vmbr0", "vmbr0.10")
	f.ack(t, testKey, testMACB, "enp3s0.10", "vmbr0.10")
	vmid, err := f.resolve()
	wantReason(t, vmid, err, "foreign ingress")
}

func TestResolveVLANUeventNeedsExactDevtype(t *testing.T) {
	f := newResolverFixture(t, map[string]string{"100": conf(testMACA)})
	fakeNetDev(t, f.sys, "vlanlike", "DEVTYPE=vlanx", "vmbr0")
	f.handshake(t, testKey, testMACA, "tap100i0")
	f.ack(t, testKey, testMACA, "vlanlike")
	vmid, err := f.resolve()
	wantReason(t, vmid, err, "foreign ingress")
}

func TestResolveVLANLookupCached(t *testing.T) {
	f := newResolverFixture(t, map[string]string{"100": conf(testMACA)})
	f.handshake(t, testKey, testMACA, "tap100i0", "vmbr0.10")
	vmid, err := f.resolve()
	wantVMID(t, vmid, err, "100")
	if err := os.RemoveAll(filepath.Join(f.sys, "vmbr0.10")); err != nil {
		t.Fatal(err)
	}
	vmid, err = f.resolve()
	wantVMID(t, vmid, err, "100")
}

func TestResolveBridgeLookupCached(t *testing.T) {
	f := newResolverFixture(t, map[string]string{"100": conf(testMACA)})
	f.handshake(t, testKey, testMACA, "tap100i0", "vmbr0")
	vmid, err := f.resolve()
	wantVMID(t, vmid, err, "100")
	if err := os.RemoveAll(filepath.Join(f.sys, "vmbr0")); err != nil {
		t.Fatal(err)
	}
	vmid, err = f.resolve()
	wantVMID(t, vmid, err, "100")
}

func TestResolveDefaultSysClassNet(t *testing.T) {
	f := newResolverFixture(t, map[string]string{"100": conf(testMACA)})
	f.res.SysClassNet = ""
	f.handshake(t, testKey, testMACA, "tap100i0", "fwbr100i0")
	vmid, err := f.resolve()
	wantVMID(t, vmid, err, "100")
}

func TestResolveNoVMConfig(t *testing.T) {
	f := newResolverFixture(t, nil)
	f.handshake(t, testKey, testMACA, "tap100i0")
	vmid, err := f.resolve()
	wantReason(t, vmid, err, "no VM config")
}

func TestResolveOversizedConfig(t *testing.T) {
	f := newResolverFixture(t, map[string]string{"100": conf(testMACA) + strings.Repeat("#\n", maxTextFileBytes)})
	f.handshake(t, testKey, testMACA, "tap100i0")
	vmid, err := f.resolve()
	wantReason(t, vmid, err, "no VM config")
}

func TestResolveMACMismatch(t *testing.T) {
	f := newResolverFixture(t, map[string]string{"100": conf(testMACA)})
	f.handshake(t, testKey, testMACB, "tap100i0")
	vmid, err := f.resolve()
	wantReason(t, vmid, err, "MAC mismatch")
}

func TestResolveSYNMACIsTheOneCompared(t *testing.T) {
	// The config check uses the SYN frame's MAC, not a later segment's.
	f := newResolverFixture(t, map[string]string{"100": conf(testMACA)})
	f.see(t, testKey, testMACB, "tap100i0")
	f.ack(t, testKey, testMACA, "tap100i0")
	vmid, err := f.resolve()
	wantReason(t, vmid, err, "MAC mismatch")
}

func TestResolveWrongNICIndex(t *testing.T) {
	f := newResolverFixture(t, map[string]string{"100": conf(testMACA)})
	f.handshake(t, testKey, testMACA, "tap100i1")
	vmid, err := f.resolve()
	wantReason(t, vmid, err, "MAC mismatch")
}

func TestResolveSecondNIC(t *testing.T) {
	f := newResolverFixture(t, map[string]string{
		"100": conf(testMACB) + "net1: e1000=" + testMACA + ",bridge=vmbr1\n",
	})
	f.handshake(t, testKey, testMACA, "tap100i1", "vmbr1")
	vmid, err := f.resolve()
	wantVMID(t, vmid, err, "100")
}

func TestResolveFirstMACValueCounts(t *testing.T) {
	// The model key comes first; a later key=value that also parses as a MAC
	// must not match.
	f := newResolverFixture(t, map[string]string{
		"100": "net0: virtio=" + testMACB + ",bridge=vmbr0,tag=" + testMACA + "\n",
	})
	f.handshake(t, testKey, testMACA, "tap100i0")
	vmid, err := f.resolve()
	wantReason(t, vmid, err, "MAC mismatch")
}

func TestResolveIgnoresSnapshotSections(t *testing.T) {
	content := conf(testMACA) + "\n[snap1]\nnet0: virtio=" + testMACB + ",bridge=vmbr0\n"
	f := newResolverFixture(t, map[string]string{"100": content})
	f.handshake(t, testKey, testMACB, "tap100i0")
	vmid, err := f.resolve()
	wantReason(t, vmid, err, "MAC mismatch")

	g := newResolverFixture(t, map[string]string{"100": content})
	g.handshake(t, testKey, testMACA, "tap100i0")
	vmid, err = g.resolve()
	wantVMID(t, vmid, err, "100")
}

func TestResolveNoObservation(t *testing.T) {
	f := newResolverFixture(t, map[string]string{"100": conf(testMACA)})
	start := time.Now()
	vmid, err := f.resolve()
	wantReason(t, vmid, err, "no SYN observed")
	if d := time.Since(start); d < 50*time.Millisecond {
		t.Errorf("Resolve returned after %v, want >= 50ms", d)
	}
}

func TestResolveSegmentsWithoutSYN(t *testing.T) {
	f := newResolverFixture(t, map[string]string{"100": conf(testMACA)})
	f.ack(t, testKey, testMACA, "tap100i0", "vmbr0")
	vmid, err := f.resolve()
	wantReason(t, vmid, err, "no SYN observed")
}

func TestResolveHandshakeNotObserved(t *testing.T) {
	f := newResolverFixture(t, map[string]string{"100": conf(testMACA)})
	f.see(t, testKey, testMACA, "tap100i0", "vmbr0")
	start := time.Now()
	vmid, err := f.resolve()
	wantReason(t, vmid, err, "handshake not observed")
	if d := time.Since(start); d < 50*time.Millisecond {
		t.Errorf("Resolve returned after %v, want >= 50ms", d)
	}
}

func TestResolveWaitsForHandshakeSegment(t *testing.T) {
	f := newResolverFixture(t, map[string]string{"100": conf(testMACA)})
	f.res.MaxWait = 250 * time.Millisecond
	f.see(t, testKey, testMACA, "tap100i0", "vmbr0")
	go func() {
		time.Sleep(20 * time.Millisecond)
		f.ack(t, testKey, testMACA, "tap100i0")
	}()
	vmid, err := f.resolve()
	wantVMID(t, vmid, err, "100")
}

func TestResolveNilTable(t *testing.T) {
	r := &IngressResolver{MaxWait: time.Millisecond}
	vmid, err := r.Resolve(context.Background(), testSrc, testDst)
	wantReason(t, vmid, err, "capture unavailable")
}

func TestResolveDefaultMaxWait(t *testing.T) {
	f := newResolverFixture(t, map[string]string{"100": conf(testMACA)})
	f.res.MaxWait = 0
	go func() {
		time.Sleep(100 * time.Millisecond)
		f.handshake(t, testKey, testMACA, "tap100i0")
	}()
	vmid, err := f.resolve()
	wantVMID(t, vmid, err, "100")
}

func TestResolveContextCancelled(t *testing.T) {
	f := newResolverFixture(t, map[string]string{"100": conf(testMACA)})
	f.res.MaxWait = 10 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	start := time.Now()
	vmid, err := f.res.Resolve(ctx, testSrc, testDst)
	wantReason(t, vmid, err, "no SYN observed")
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("Resolve ignored ctx: returned after %v", d)
	}
}

func TestResolveLateObservation(t *testing.T) {
	f := newResolverFixture(t, map[string]string{"100": conf(testMACA)})
	f.res.MaxWait = 250 * time.Millisecond
	go func() {
		time.Sleep(20 * time.Millisecond)
		f.handshake(t, testKey, testMACA, "tap100i0", "vmbr0")
	}()
	vmid, err := f.resolve()
	wantVMID(t, vmid, err, "100")
}

func TestResolveOtherKeyDoesNotSatisfyWait(t *testing.T) {
	f := newResolverFixture(t, map[string]string{"100": conf(testMACA)})
	other := ConnKey{Src: netip.MustParseAddrPort(testIPA + ":40001"), Dst: testDst}
	f.handshake(t, other, testMACA, "tap100i0")
	vmid, err := f.resolve()
	wantReason(t, vmid, err, "no SYN observed")
}

func TestResolveIPv6(t *testing.T) {
	f := newResolverFixture(t, map[string]string{"100": conf(testMACA)})
	vmid, err := f.res.Resolve(context.Background(), netip.MustParseAddrPort("[2001:db8::1]:5555"), testDst)
	wantReason(t, vmid, err, "ipv6 not supported")
	vmid, err = f.res.Resolve(context.Background(), testSrc, netip.MustParseAddrPort("[2001:db8::10]:9873"))
	wantReason(t, vmid, err, "ipv6 not supported")

	f.handshake(t, testKey, testMACA, "tap100i0")
	mapped := netip.MustParseAddrPort("[::ffff:" + testIPA + "]:40000")
	mappedLocal := netip.MustParseAddrPort("[::ffff:" + testHostIP + "]:9873")
	vmid, err = f.res.Resolve(context.Background(), mapped, mappedLocal)
	wantVMID(t, vmid, err, "100")
}

func TestResolveSpoofedIPGetsOwnVM(t *testing.T) {
	// VM 101 (MAC B) sends a SYN carrying VM 100's source IP. It still enters
	// on its own tap, so it can only ever resolve to itself.
	f := newResolverFixture(t, map[string]string{"100": conf(testMACA), "101": conf(testMACB)})
	f.handshake(t, testKey, testMACB, "tap101i0", "fwbr101i0", "vmbr0")
	vmid, err := f.resolve()
	wantVMID(t, vmid, err, "101")
}

func TestResolveSpoofedMACRejected(t *testing.T) {
	// VM 101 sends with VM 100's MAC through its own tap.
	f := newResolverFixture(t, map[string]string{"100": conf(testMACA), "101": conf(testMACB)})
	f.handshake(t, testKey, testMACA, "tap101i0", "vmbr0")
	vmid, err := f.resolve()
	wantReason(t, vmid, err, "MAC mismatch")
}

func TestResolveStaleTapObservationNotReused(t *testing.T) {
	// VM 100 connected from this 4-tuple a while ago. A LAN device that
	// later reuses the same 4-tuple (spoofed IP and port, entering on the
	// uplink) must not inherit the old tap observation.
	clock := time.Unix(1_000_000, 0)
	f := newResolverFixture(t, map[string]string{"100": conf(testMACA)})
	f.table = NewObservationTable(30*time.Second, func() time.Time { return clock })
	f.table.setCaptureHealthy(true)
	f.res.Table = f.table
	f.handshake(t, testKey, testMACA, "tap100i0", "vmbr0")
	clock = clock.Add(5 * time.Second)
	f.handshake(t, testKey, testMACA, "enp3s0", "vmbr0")
	vmid, err := f.resolve()
	wantReason(t, vmid, err, "not a VM port")
}

func TestObservationTableNewSYNReplacesEarlierSegments(t *testing.T) {
	clock := time.Unix(1_000_000, 0)
	table := NewObservationTable(30*time.Second, func() time.Time { return clock })
	mac := mustMAC(t, testMACA)
	// Segments without a SYN, then a SYN: the SYN starts the connection; the
	// earlier non-tap segment is kept as negative evidence only.
	table.Add(testKey, Observation{Ifname: "enp3s0", SrcMAC: mac})
	clock = clock.Add(time.Millisecond)
	table.Add(testKey, Observation{Ifname: "tap100i0", SrcMAC: mac, SYN: true})
	table.Add(testKey, Observation{Ifname: "vmbr0", SrcMAC: mac, SYN: true}) // same SYN, other copy
	clock = clock.Add(time.Second)                                           // later segments stay
	table.Add(testKey, Observation{Ifname: "tap100i0", SrcMAC: mac})
	got := table.Wait(context.Background(), testKey, 0)
	want := []string{"enp3s0/earlier", "tap100i0/syn", "vmbr0/syn", "tap100i0/ack"}
	if len(got) != len(want) {
		t.Fatalf("got %+v, want %v", got, want)
	}
	for i, o := range got {
		kind := "ack"
		switch {
		case o.SYN:
			kind = "syn"
		case o.Earlier:
			kind = "earlier"
		}
		if o.Ifname+"/"+kind != want[i] {
			t.Errorf("observation %d = %s/%s, want %s", i, o.Ifname, kind, want[i])
		}
	}
}

func TestObservationTableWaitNeedsNonSYN(t *testing.T) {
	table := NewObservationTable(30*time.Second, time.Now)
	mac := mustMAC(t, testMACA)
	table.Add(testKey, Observation{Ifname: "tap100i0", SrcMAC: mac, SYN: true})
	start := time.Now()
	got := table.Wait(context.Background(), testKey, 30*time.Millisecond)
	if d := time.Since(start); d < 30*time.Millisecond {
		t.Errorf("Wait returned after %v with only a SYN, want the full 30ms", d)
	}
	if len(got) != 1 || !got[0].SYN {
		t.Fatalf("on timeout got %+v, want the SYN observation", got)
	}
}

func TestObservationTableExpiry(t *testing.T) {
	clock := time.Unix(1_000_000, 0)
	table := NewObservationTable(30*time.Second, func() time.Time { return clock })
	mac := mustMAC(t, testMACA)
	table.Add(testKey, Observation{Ifname: "tap100i0", SrcMAC: mac, SYN: true})
	if got := table.Wait(context.Background(), testKey, 0); len(got) != 1 {
		t.Fatalf("fresh entry: got %d observations, want 1", len(got))
	}

	clock = clock.Add(31 * time.Second)
	if got := table.Wait(context.Background(), testKey, 10*time.Millisecond); len(got) != 0 {
		t.Fatalf("expired entry returned: %+v", got)
	}

	other := ConnKey{Src: netip.MustParseAddrPort(testIPB + ":40000"), Dst: testDst}
	table.Add(other, Observation{Ifname: "tap101i0", SrcMAC: mac, SYN: true})
	table.mu.Lock()
	_, stale := table.entries[testKey]
	n := len(table.entries)
	table.mu.Unlock()
	if stale || n != 1 {
		t.Fatalf("after Add: expired key present = %v, %d keys; want purged, 1 key", stale, n)
	}
}

func TestObservationTableKeepsExplicitTime(t *testing.T) {
	clock := time.Unix(1_000_000, 0)
	table := NewObservationTable(30*time.Second, func() time.Time { return clock })
	at := clock.Add(-time.Second)
	table.Add(testKey, Observation{Ifname: "tap100i0", SrcMAC: mustMAC(t, testMACA), SYN: true, At: at})
	got := table.Wait(context.Background(), testKey, 0)
	if len(got) != 1 || !got[0].At.Equal(at) {
		t.Fatalf("got %+v, want one observation at %v", got, at)
	}
}

func TestObservationTableDeduplicates(t *testing.T) {
	table := NewObservationTable(30*time.Second, time.Now)
	mac := mustMAC(t, testMACA)
	for range 5 { // retransmitted SYNs, then repeated ACK/data segments
		table.Add(testKey, Observation{Ifname: "tap100i0", SrcMAC: mac, SYN: true})
		table.Add(testKey, Observation{Ifname: "vmbr0", SrcMAC: mac, SYN: true})
	}
	for range 5 {
		table.Add(testKey, Observation{Ifname: "tap100i0", SrcMAC: mac})
		table.Add(testKey, Observation{Ifname: "vmbr0", SrcMAC: mac})
	}
	if got := table.Wait(context.Background(), testKey, 0); len(got) != 4 {
		t.Fatalf("got %d observations, want 4 distinct", len(got))
	}
}

func TestObservationTableBounded(t *testing.T) {
	table := NewObservationTable(30*time.Second, time.Now)
	mac := mustMAC(t, testMACA)
	for i := range maxObservedKeys + 10 {
		k := ConnKey{Src: netip.AddrPortFrom(netip.AddrFrom4([4]byte{192, 0, 2, byte(i >> 16)}), uint16(i)), Dst: testDst}
		table.Add(k, Observation{Ifname: "enp3s0", SrcMAC: mac})
	}
	table.Add(testKey, Observation{Ifname: "tap100i0", SrcMAC: mac, SYN: true})
	for i := range maxObservationsPerKey + 10 {
		table.Add(testKey, Observation{Ifname: "if" + string(rune('a'+i%26)) + string(rune('a'+i/26)), SrcMAC: mac})
	}
	// A key with more distinct copies than kept fails closed.
	if got := table.Wait(context.Background(), testKey, 0); got != nil {
		t.Errorf("overflowing key returned %d observations, want none", len(got))
	}
	table.mu.Lock()
	defer table.mu.Unlock()
	if len(table.entries) > maxObservedKeys {
		t.Errorf("%d keys, want <= %d", len(table.entries), maxObservedKeys)
	}
	if n := len(table.entries[testKey].obs); n > maxObservationsPerKey {
		t.Errorf("%d observations for one key, want <= %d", n, maxObservationsPerKey)
	}
}

func TestObservationTableConcurrent(t *testing.T) {
	table := NewObservationTable(30*time.Second, time.Now)
	mac := mustMAC(t, testMACA)
	done := make(chan struct{})
	for g := range 4 {
		go func() {
			defer func() { done <- struct{}{} }()
			for i := range 200 {
				k := ConnKey{Src: netip.AddrPortFrom(testSrc.Addr(), uint16(g*1000+i)), Dst: testDst}
				table.Add(k, Observation{Ifname: "tap100i0", SrcMAC: mac, SYN: true})
				table.Add(k, Observation{Ifname: "tap100i0", SrcMAC: mac})
				table.Wait(context.Background(), k, time.Millisecond)
			}
		}()
	}
	for range 4 {
		<-done
	}
}

func TestResolveConcurrent(t *testing.T) {
	f := newResolverFixture(t, map[string]string{"100": conf(testMACA)})
	f.handshake(t, testKey, testMACA, "tap100i0", "vmbr0", "fwbr100i0")
	done := make(chan error)
	for range 8 {
		go func() {
			_, err := f.resolve()
			done <- err
		}()
	}
	for range 8 {
		if err := <-done; err != nil {
			t.Error(err)
		}
	}
}

func TestObservationTableOrderStaysBounded(t *testing.T) {
	// Repeated SYNs for one 4-tuple, each a new burst, must not grow the
	// insertion order without bound.
	clock := time.Unix(1_000_000, 0)
	table := NewObservationTable(time.Hour, func() time.Time { return clock })
	mac := mustMAC(t, testMACA)
	for range 3*maxObservedKeys + 7 {
		clock = clock.Add(synBurstWindow + time.Millisecond)
		table.Add(testKey, Observation{Ifname: "enp3s0", SrcMAC: mac, SYN: true})
	}
	table.mu.Lock()
	defer table.mu.Unlock()
	if len(table.entries) != 1 || len(table.order) > 2*maxObservedKeys {
		t.Fatalf("%d entries, %d queued; want 1 entry and <= %d queued", len(table.entries), len(table.order), 2*maxObservedKeys)
	}
}

func TestObservationTableEvictsOldestWhenFull(t *testing.T) {
	clock := time.Unix(1_000_000, 0)
	table := NewObservationTable(time.Hour, func() time.Time { return clock })
	mac := mustMAC(t, testMACA)
	table.Add(testKey, Observation{Ifname: "tap100i0", SrcMAC: mac, SYN: true})
	for i := range maxObservedKeys {
		clock = clock.Add(time.Millisecond)
		k := ConnKey{Src: netip.AddrPortFrom(netip.MustParseAddr(testIPB), uint16(i)), Dst: testDst}
		table.Add(k, Observation{Ifname: "enp3s0", SrcMAC: mac, SYN: true})
	}
	if got := table.Wait(context.Background(), testKey, 0); got != nil {
		t.Fatalf("oldest entry survived a full table: %+v", got)
	}
	newest := ConnKey{Src: netip.AddrPortFrom(netip.MustParseAddr(testIPB), uint16(maxObservedKeys-1)), Dst: testDst}
	if got := table.Wait(context.Background(), newest, 0); len(got) != 1 {
		t.Fatalf("newest entry missing: %+v", got)
	}
}
