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

	// An IHL below 5 still gets a full 20-byte header, so only the field lies.
	ip := make([]byte, max(f.ihl, 5)*4)
	ip[0] = 0x40 | byte(f.ihl&0x0f)
	binary.BigEndian.PutUint16(ip[2:], uint16(f.ihl*4+20))
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
	return append(b, tcp...)
}

func TestParseSYNFrame(t *testing.T) {
	for _, tc := range []struct {
		name  string
		frame synFrame
	}{
		{"plain", synFrame{}},
		{"vlan tagged", synFrame{vlan: true}},
		{"ip options", synFrame{ihl: 6}},
		{"first fragment", synFrame{fragField: 0x2000}}, // MF set, offset 0
		{"don't fragment", synFrame{fragField: 0x4000}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			key, src, ok := ParseSYNFrame(tc.frame.bytes(t), testPort)
			if !ok {
				t.Fatal("ok = false, want true")
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

func TestParseSYNFrameCopiesMAC(t *testing.T) {
	frame := synFrame{}.bytes(t)
	_, src, ok := ParseSYNFrame(frame, testPort)
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

func TestParseSYNFrameRejects(t *testing.T) {
	for _, tc := range []struct {
		name  string
		frame synFrame
		port  uint16
	}{
		{"syn-ack", synFrame{flags: tcpSYN | tcpACK}, testPort},
		{"pure ack", synFrame{flags: tcpACK}, testPort},
		{"no flags", synFrame{setFlags: true}, testPort},
		{"other dst port", synFrame{}, testPort + 1},
		{"ipv6 ethertype", synFrame{etherType: 0x86dd}, testPort},
		{"arp ethertype", synFrame{etherType: 0x0806}, testPort},
		{"udp", synFrame{proto: 17}, testPort},
		{"ihl 4", synFrame{ihl: 4}, testPort},
		{"non-first fragment", synFrame{fragField: 0x0001}, testPort},
		{"last fragment", synFrame{fragField: 0x00b9}, testPort},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, ok := ParseSYNFrame(tc.frame.bytes(t), tc.port); ok {
				t.Fatal("ok = true, want false")
			}
		})
	}

	t.Run("ip version 6 under ipv4 ethertype", func(t *testing.T) {
		frame := synFrame{}.bytes(t)
		frame[14] = 0x65
		if _, _, ok := ParseSYNFrame(frame, testPort); ok {
			t.Fatal("ok = true, want false")
		}
	})

	t.Run("double vlan tag", func(t *testing.T) {
		frame := synFrame{vlan: true}.bytes(t)
		frame = append(frame[:16:16], append([]byte{0x81, 0x00, 0x00, 0x14}, frame[16:]...)...)
		if _, _, ok := ParseSYNFrame(frame, testPort); ok {
			t.Fatal("ok = true, want false")
		}
	})

	for _, f := range []synFrame{{}, {vlan: true}, {ihl: 6}} {
		frame := f.bytes(t)
		for n := range len(frame) {
			if _, _, ok := ParseSYNFrame(frame[:n], testPort); ok {
				t.Errorf("vlan=%v ihl=%d truncated to %d bytes: ok = true", f.vlan, f.ihl, n)
			}
		}
	}
	if _, _, ok := ParseSYNFrame(nil, testPort); ok {
		t.Error("nil frame: ok = true")
	}
}

// runBPF interprets the subset of classic BPF that synFilter uses, with the
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

func TestSYNFilterProgram(t *testing.T) {
	prog := synFilter(testPort)
	for _, f := range []synFrame{{}, {vlan: true}, {ihl: 6}, {vlan: true, ihl: 15}, {fragField: 0x2000}} {
		if got := runBPF(t, prog, f.bytes(t)); got != captureSnapLen {
			t.Errorf("accept vlan=%v ihl=%d frag=%#x: filter returned %d, want %d", f.vlan, f.ihl, f.fragField, got, captureSnapLen)
		}
	}
	for _, tc := range []struct {
		name  string
		frame synFrame
		port  uint16
	}{
		{"syn-ack", synFrame{flags: tcpSYN | tcpACK}, testPort},
		{"vlan syn-ack", synFrame{vlan: true, flags: tcpSYN | tcpACK}, testPort},
		{"pure ack", synFrame{flags: tcpACK}, testPort},
		{"other port", synFrame{}, testPort + 1},
		{"vlan other port", synFrame{vlan: true}, testPort + 1},
		{"ipv6", synFrame{etherType: 0x86dd}, testPort},
		{"udp", synFrame{proto: 17}, testPort},
		{"vlan udp", synFrame{vlan: true, proto: 17}, testPort},
		{"non-first fragment", synFrame{fragField: 0x0001}, testPort},
		{"vlan non-first fragment", synFrame{vlan: true, fragField: 0x0001}, testPort},
	} {
		if got := runBPF(t, synFilter(tc.port), tc.frame.bytes(t)); got != 0 {
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
}

func newResolverFixture(t *testing.T, confs map[string]string) *resolverFixture {
	t.Helper()
	dir := t.TempDir()
	for vmid, content := range confs {
		if err := os.WriteFile(filepath.Join(dir, vmid+".conf"), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	table := NewObservationTable(30*time.Second, time.Now)
	return &resolverFixture{
		table: table,
		res:   &IngressResolver{Table: table, PVEConfDir: dir, MaxWait: 50 * time.Millisecond},
		dir:   dir,
	}
}

func (f *resolverFixture) see(t *testing.T, key ConnKey, mac string, ifnames ...string) {
	t.Helper()
	for _, name := range ifnames {
		f.table.Add(key, Observation{Ifname: name, SrcMAC: mustMAC(t, mac)})
	}
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

func TestResolveSingleTap(t *testing.T) {
	f := newResolverFixture(t, map[string]string{"100": conf(testMACA)})
	f.see(t, testKey, testMACA, "tap100i0", "vmbr0")
	vmid, err := f.res.Resolve(context.Background(), testSrc, testDst)
	wantVMID(t, vmid, err, "100")
}

func TestResolveFirewallCopiesIgnored(t *testing.T) {
	f := newResolverFixture(t, map[string]string{"101": conf(testMACA)})
	f.see(t, testKey, testMACA, "tap101i0", "fwbr101i0", "fwln101i0", "fwpr101p0", "vmbr0")
	vmid, err := f.res.Resolve(context.Background(), testSrc, testDst)
	wantVMID(t, vmid, err, "101")
}

func TestResolveUplinkOnly(t *testing.T) {
	f := newResolverFixture(t, map[string]string{"100": conf(testMACA)})
	f.see(t, testKey, testMACA, "enp3s0", "vmbr0")
	vmid, err := f.res.Resolve(context.Background(), testSrc, testDst)
	wantReason(t, vmid, err, "not a VM port")
}

func TestResolveFirewallPortWithoutTap(t *testing.T) {
	f := newResolverFixture(t, map[string]string{"101": conf(testMACA)})
	f.see(t, testKey, testMACA, "fwpr101p0", "fwbr101i0", "vmbr0", "veth200i0")
	vmid, err := f.res.Resolve(context.Background(), testSrc, testDst)
	wantReason(t, vmid, err, "not a VM port")
}

func TestResolveTwoTaps(t *testing.T) {
	f := newResolverFixture(t, map[string]string{"100": conf(testMACA), "101": conf(testMACA)})
	f.see(t, testKey, testMACA, "tap100i0", "tap101i0")
	vmid, err := f.res.Resolve(context.Background(), testSrc, testDst)
	wantReason(t, vmid, err, "ambiguous ingress")
}

func TestResolveOneTapTwoMACs(t *testing.T) {
	f := newResolverFixture(t, map[string]string{"100": conf(testMACA)})
	f.see(t, testKey, testMACA, "tap100i0")
	f.see(t, testKey, testMACB, "tap100i0")
	vmid, err := f.res.Resolve(context.Background(), testSrc, testDst)
	wantReason(t, vmid, err, "ambiguous ingress")
}

func TestResolveNoVMConfig(t *testing.T) {
	f := newResolverFixture(t, nil)
	f.see(t, testKey, testMACA, "tap100i0")
	vmid, err := f.res.Resolve(context.Background(), testSrc, testDst)
	wantReason(t, vmid, err, "no VM config")
}

func TestResolveOversizedConfig(t *testing.T) {
	f := newResolverFixture(t, map[string]string{"100": conf(testMACA) + strings.Repeat("#\n", maxTextFileBytes)})
	f.see(t, testKey, testMACA, "tap100i0")
	vmid, err := f.res.Resolve(context.Background(), testSrc, testDst)
	wantReason(t, vmid, err, "no VM config")
}

func TestResolveMACMismatch(t *testing.T) {
	f := newResolverFixture(t, map[string]string{"100": conf(testMACA)})
	f.see(t, testKey, testMACB, "tap100i0")
	vmid, err := f.res.Resolve(context.Background(), testSrc, testDst)
	wantReason(t, vmid, err, "MAC mismatch")
}

func TestResolveWrongNICIndex(t *testing.T) {
	f := newResolverFixture(t, map[string]string{"100": conf(testMACA)})
	f.see(t, testKey, testMACA, "tap100i1")
	vmid, err := f.res.Resolve(context.Background(), testSrc, testDst)
	wantReason(t, vmid, err, "MAC mismatch")
}

func TestResolveSecondNIC(t *testing.T) {
	f := newResolverFixture(t, map[string]string{
		"100": conf(testMACB) + "net1: e1000=" + testMACA + ",bridge=vmbr1\n",
	})
	f.see(t, testKey, testMACA, "tap100i1")
	vmid, err := f.res.Resolve(context.Background(), testSrc, testDst)
	wantVMID(t, vmid, err, "100")
}

func TestResolveFirstMACValueCounts(t *testing.T) {
	// The model key comes first; a later key=value that also parses as a MAC
	// must not match.
	f := newResolverFixture(t, map[string]string{
		"100": "net0: virtio=" + testMACB + ",bridge=vmbr0,tag=" + testMACA + "\n",
	})
	f.see(t, testKey, testMACA, "tap100i0")
	vmid, err := f.res.Resolve(context.Background(), testSrc, testDst)
	wantReason(t, vmid, err, "MAC mismatch")
}

func TestResolveIgnoresSnapshotSections(t *testing.T) {
	content := conf(testMACA) + "\n[snap1]\nnet0: virtio=" + testMACB + ",bridge=vmbr0\n"
	f := newResolverFixture(t, map[string]string{"100": content})
	f.see(t, testKey, testMACB, "tap100i0")
	vmid, err := f.res.Resolve(context.Background(), testSrc, testDst)
	wantReason(t, vmid, err, "MAC mismatch")

	g := newResolverFixture(t, map[string]string{"100": content})
	g.see(t, testKey, testMACA, "tap100i0")
	vmid, err = g.res.Resolve(context.Background(), testSrc, testDst)
	wantVMID(t, vmid, err, "100")
}

func TestResolveNoObservation(t *testing.T) {
	f := newResolverFixture(t, map[string]string{"100": conf(testMACA)})
	start := time.Now()
	vmid, err := f.res.Resolve(context.Background(), testSrc, testDst)
	wantReason(t, vmid, err, "no SYN observed")
	if d := time.Since(start); d < 50*time.Millisecond {
		t.Errorf("Resolve returned after %v, want >= 50ms", d)
	}
}

func TestResolveNilTable(t *testing.T) {
	r := &IngressResolver{MaxWait: time.Millisecond}
	vmid, err := r.Resolve(context.Background(), testSrc, testDst)
	wantReason(t, vmid, err, "no SYN observed")
}

func TestResolveDefaultMaxWait(t *testing.T) {
	f := newResolverFixture(t, map[string]string{"100": conf(testMACA)})
	f.res.MaxWait = 0
	go func() {
		time.Sleep(100 * time.Millisecond)
		f.see(t, testKey, testMACA, "tap100i0")
	}()
	vmid, err := f.res.Resolve(context.Background(), testSrc, testDst)
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
		f.see(t, testKey, testMACA, "tap100i0", "vmbr0")
	}()
	vmid, err := f.res.Resolve(context.Background(), testSrc, testDst)
	wantVMID(t, vmid, err, "100")
}

func TestResolveOtherKeyDoesNotSatisfyWait(t *testing.T) {
	f := newResolverFixture(t, map[string]string{"100": conf(testMACA)})
	other := ConnKey{Src: netip.MustParseAddrPort(testIPA + ":40001"), Dst: testDst}
	f.see(t, other, testMACA, "tap100i0")
	vmid, err := f.res.Resolve(context.Background(), testSrc, testDst)
	wantReason(t, vmid, err, "no SYN observed")
}

func TestResolveIPv6(t *testing.T) {
	f := newResolverFixture(t, map[string]string{"100": conf(testMACA)})
	vmid, err := f.res.Resolve(context.Background(), netip.MustParseAddrPort("[2001:db8::1]:5555"), testDst)
	wantReason(t, vmid, err, "ipv6 not supported")
	vmid, err = f.res.Resolve(context.Background(), testSrc, netip.MustParseAddrPort("[2001:db8::10]:9873"))
	wantReason(t, vmid, err, "ipv6 not supported")

	f.see(t, testKey, testMACA, "tap100i0")
	mapped := netip.MustParseAddrPort("[::ffff:" + testIPA + "]:40000")
	mappedLocal := netip.MustParseAddrPort("[::ffff:" + testHostIP + "]:9873")
	vmid, err = f.res.Resolve(context.Background(), mapped, mappedLocal)
	wantVMID(t, vmid, err, "100")
}

func TestResolveSpoofedIPGetsOwnVM(t *testing.T) {
	// VM 101 (MAC B) sends a SYN carrying VM 100's source IP. It still enters
	// on its own tap, so it can only ever resolve to itself.
	f := newResolverFixture(t, map[string]string{"100": conf(testMACA), "101": conf(testMACB)})
	f.see(t, testKey, testMACB, "tap101i0", "fwbr101i0", "vmbr0")
	vmid, err := f.res.Resolve(context.Background(), testSrc, testDst)
	wantVMID(t, vmid, err, "101")
}

func TestResolveSpoofedMACRejected(t *testing.T) {
	// VM 101 sends with VM 100's MAC through its own tap.
	f := newResolverFixture(t, map[string]string{"100": conf(testMACA), "101": conf(testMACB)})
	f.see(t, testKey, testMACA, "tap101i0", "vmbr0")
	vmid, err := f.res.Resolve(context.Background(), testSrc, testDst)
	wantReason(t, vmid, err, "MAC mismatch")
}

func TestResolveStaleTapObservationNotReused(t *testing.T) {
	// VM 100 connected from this 4-tuple a while ago. A LAN device that
	// later reuses the same 4-tuple (spoofed IP and port, entering on the
	// uplink) must not inherit the old tap observation.
	clock := time.Unix(1_000_000, 0)
	table := NewObservationTable(30*time.Second, func() time.Time { return clock })
	f := newResolverFixture(t, map[string]string{"100": conf(testMACA)})
	f.res.Table = table
	f.table = table
	f.see(t, testKey, testMACA, "tap100i0", "vmbr0")
	clock = clock.Add(5 * time.Second)
	f.see(t, testKey, testMACA, "enp3s0", "vmbr0")
	vmid, err := f.res.Resolve(context.Background(), testSrc, testDst)
	wantReason(t, vmid, err, "not a VM port")
}

func TestObservationTableExpiry(t *testing.T) {
	clock := time.Unix(1_000_000, 0)
	table := NewObservationTable(30*time.Second, func() time.Time { return clock })
	mac := mustMAC(t, testMACA)
	table.Add(testKey, Observation{Ifname: "tap100i0", SrcMAC: mac})
	if got := table.Wait(context.Background(), testKey, 0); len(got) != 1 {
		t.Fatalf("fresh entry: got %d observations, want 1", len(got))
	}

	clock = clock.Add(31 * time.Second)
	if got := table.Wait(context.Background(), testKey, 10*time.Millisecond); len(got) != 0 {
		t.Fatalf("expired entry returned: %+v", got)
	}

	other := ConnKey{Src: netip.MustParseAddrPort(testIPB + ":40000"), Dst: testDst}
	table.Add(other, Observation{Ifname: "tap101i0", SrcMAC: mac})
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
	table.Add(testKey, Observation{Ifname: "tap100i0", SrcMAC: mustMAC(t, testMACA), At: at})
	got := table.Wait(context.Background(), testKey, 0)
	if len(got) != 1 || !got[0].At.Equal(at) {
		t.Fatalf("got %+v, want one observation at %v", got, at)
	}
}

func TestObservationTableDeduplicates(t *testing.T) {
	table := NewObservationTable(30*time.Second, time.Now)
	mac := mustMAC(t, testMACA)
	for range 5 { // retransmitted SYNs
		table.Add(testKey, Observation{Ifname: "tap100i0", SrcMAC: mac})
		table.Add(testKey, Observation{Ifname: "vmbr0", SrcMAC: mac})
	}
	if got := table.Wait(context.Background(), testKey, 0); len(got) != 2 {
		t.Fatalf("got %d observations, want 2 distinct", len(got))
	}
}

func TestObservationTableBounded(t *testing.T) {
	table := NewObservationTable(30*time.Second, time.Now)
	mac := mustMAC(t, testMACA)
	for i := range maxObservedKeys + 10 {
		k := ConnKey{Src: netip.AddrPortFrom(netip.AddrFrom4([4]byte{192, 0, 2, byte(i >> 16)}), uint16(i)), Dst: testDst}
		table.Add(k, Observation{Ifname: "enp3s0", SrcMAC: mac})
	}
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
				table.Add(k, Observation{Ifname: "tap100i0", SrcMAC: mac})
				table.Wait(context.Background(), k, time.Millisecond)
			}
		}()
	}
	for range 4 {
		<-done
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
		table.Add(testKey, Observation{Ifname: "enp3s0", SrcMAC: mac})
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
	table.Add(testKey, Observation{Ifname: "tap100i0", SrcMAC: mac})
	for i := range maxObservedKeys {
		clock = clock.Add(time.Millisecond)
		k := ConnKey{Src: netip.AddrPortFrom(netip.MustParseAddr(testIPB), uint16(i)), Dst: testDst}
		table.Add(k, Observation{Ifname: "enp3s0", SrcMAC: mac})
	}
	if got := table.Wait(context.Background(), testKey, 0); got != nil {
		t.Fatalf("oldest entry survived a full table: %+v", got)
	}
	newest := ConnKey{Src: netip.AddrPortFrom(netip.MustParseAddr(testIPB), uint16(maxObservedKeys-1)), Dst: testDst}
	if got := table.Wait(context.Background(), newest, 0); len(got) != 1 {
		t.Fatalf("newest entry missing: %+v", got)
	}
}
