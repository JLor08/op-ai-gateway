// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package hostpower

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	testVMMAC = "bc:24:11:00:00:64"
	testIP    = "192.0.2.50"
)

type fdbEntry struct {
	mac     string
	port    uint16
	isLocal bool
}

type arpEntry struct {
	ip, flags, mac, dev string
}

// netTree describes a synthetic /proc + /sys + /etc/pve tree.
type netTree struct {
	arp     []arpEntry
	bridges []string                     // devices that get a bridge/ directory
	plain   []string                     // devices without one
	fdb     map[string][]fdbEntry        // bridge -> entries
	ports   map[string]map[string]uint16 // bridge -> ifname -> port_no
	confs   map[string]string            // vmid -> file content
	rawFDB  map[string][]byte            // bridge -> raw brforward override
}

func defaultTree() *netTree {
	return &netTree{
		arp:     []arpEntry{{testIP, "0x2", testVMMAC, "vmbr0"}},
		bridges: []string{"vmbr0"},
		fdb: map[string][]fdbEntry{"vmbr0": {
			{testVMMAC, 3, false},
		}},
		ports: map[string]map[string]uint16{"vmbr0": {
			"tap100i0": 3, "enp3s0": 1, "veth200i0": 4, "fwpr101p0": 5,
		}},
		confs: map[string]string{
			"100": "boot: order=scsi0\nnet0: virtio=" + strings.ToUpper(testVMMAC) + ",bridge=vmbr0,firewall=1\n",
		},
	}
}

func fakeNet(t *testing.T, tree *netTree) MACResolver {
	t.Helper()
	root := t.TempDir()
	arpPath := filepath.Join(root, "arp")
	sys := filepath.Join(root, "sys", "class", "net")
	pve := filepath.Join(root, "qemu-server")
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	var b strings.Builder
	b.WriteString("IP address       HW type     Flags       HW address            Mask     Device\n")
	for _, e := range tree.arp {
		fmt.Fprintf(&b, "%-16s 0x1         %s         %s     *        %s\n", e.ip, e.flags, e.mac, e.dev)
	}
	must(os.WriteFile(arpPath, []byte(b.String()), 0o644))
	for _, d := range tree.bridges {
		must(os.MkdirAll(filepath.Join(sys, d, "bridge"), 0o755))
		must(os.MkdirAll(filepath.Join(sys, d, "brif"), 0o755))
	}
	for _, d := range tree.plain {
		must(os.MkdirAll(filepath.Join(sys, d), 0o755))
	}
	for br, ports := range tree.ports {
		for ifn, no := range ports {
			dir := filepath.Join(sys, br, "brif", ifn)
			must(os.MkdirAll(dir, 0o755))
			must(os.WriteFile(filepath.Join(dir, "port_no"), []byte(fmt.Sprintf("0x%x\n", no)), 0o644))
		}
	}
	for br, entries := range tree.fdb {
		var raw []byte
		for _, e := range entries {
			hw, err := net.ParseMAC(e.mac)
			must(err)
			rec := make([]byte, 16)
			copy(rec, hw)
			rec[6] = byte(e.port)
			if e.isLocal {
				rec[7] = 1
			}
			rec[12] = byte(e.port >> 8)
			raw = append(raw, rec...)
		}
		must(os.WriteFile(filepath.Join(sys, br, "brforward"), raw, 0o644))
	}
	for br, raw := range tree.rawFDB {
		must(os.WriteFile(filepath.Join(sys, br, "brforward"), raw, 0o644))
	}
	must(os.MkdirAll(pve, 0o755))
	for id, c := range tree.confs {
		must(os.WriteFile(filepath.Join(pve, id+".conf"), []byte(c), 0o644))
	}
	return MACResolver{ARPPath: arpPath, SysClassNet: sys, PVEConfDir: pve}
}

func mustResolve(t *testing.T, r MACResolver, ip string, want string) {
	t.Helper()
	got, err := r.Resolve(netip.MustParseAddr(ip))
	if err != nil || got != want {
		t.Fatalf("Resolve(%s) = %q, %v; want %q", ip, got, err, want)
	}
}

func mustFail(t *testing.T, r MACResolver, ip string, reason string) {
	t.Helper()
	got, err := r.Resolve(netip.MustParseAddr(ip))
	var re *ResolveError
	if !errors.As(err, &re) || re.Reason != reason || got != "" {
		t.Fatalf("Resolve(%s) = %q, %v; want ResolveError %q", ip, got, err, reason)
	}
	if err.Error() != "hostpower: "+reason {
		t.Fatalf("Error() = %q", err.Error())
	}
}

func TestResolveTapPort(t *testing.T) {
	mustResolve(t, fakeNet(t, defaultTree()), testIP, "100")
}

func TestResolveFirewallPort(t *testing.T) {
	tr := defaultTree()
	tr.fdb["vmbr0"] = []fdbEntry{{testVMMAC, 5, false}}
	tr.confs["101"] = "net0: virtio=" + testVMMAC + ",bridge=vmbr0\n"
	mustResolve(t, fakeNet(t, tr), testIP, "101")
}

func TestResolveHighPortNumber(t *testing.T) {
	tr := defaultTree()
	tr.fdb["vmbr0"] = []fdbEntry{{testVMMAC, 0x103, false}}
	tr.ports["vmbr0"]["tap100i0"] = 0x103
	mustResolve(t, fakeNet(t, tr), testIP, "100")
}

func TestResolveUplinkRejected(t *testing.T) {
	tr := defaultTree()
	tr.fdb["vmbr0"] = []fdbEntry{{testVMMAC, 1, false}}
	mustFail(t, fakeNet(t, tr), testIP, "not a VM bridge port")
}

func TestResolveVethRejected(t *testing.T) {
	tr := defaultTree()
	tr.fdb["vmbr0"] = []fdbEntry{{testVMMAC, 4, false}}
	mustFail(t, fakeNet(t, tr), testIP, "not a VM bridge port")
}

func TestResolveUnknownPortNumberRejected(t *testing.T) {
	tr := defaultTree()
	tr.fdb["vmbr0"] = []fdbEntry{{testVMMAC, 99, false}}
	mustFail(t, fakeNet(t, tr), testIP, "not a VM bridge port")
}

func TestResolveAnchoredPortNames(t *testing.T) {
	for _, name := range []string{"xtap100i0", "tap100i0x", "tap100i", "tap100i0.1", "tapi0"} {
		tr := defaultTree()
		tr.ports["vmbr0"] = map[string]uint16{name: 3}
		mustFail(t, fakeNet(t, tr), testIP, "not a VM bridge port")
	}
}

func TestResolveMACMismatch(t *testing.T) {
	tr := defaultTree()
	tr.confs["100"] = "net0: virtio=BC:24:11:00:00:99,bridge=vmbr0\n"
	mustFail(t, fakeNet(t, tr), testIP, "MAC mismatch")
}

func TestResolveWrongNICIndex(t *testing.T) {
	tr := defaultTree()
	delete(tr.ports["vmbr0"], "tap100i0")
	tr.ports["vmbr0"]["tap100i1"] = 3
	mustFail(t, fakeNet(t, tr), testIP, "MAC mismatch")
}

func TestResolveSecondNIC(t *testing.T) {
	tr := defaultTree()
	delete(tr.ports["vmbr0"], "tap100i0")
	tr.ports["vmbr0"]["tap100i1"] = 3
	tr.confs["100"] = "net0: virtio=BC:24:11:00:00:01,bridge=vmbr0\nnet1: e1000=" + testVMMAC + ",bridge=vmbr0\n"
	mustResolve(t, fakeNet(t, tr), testIP, "100")
}

func TestResolveWrongBridge(t *testing.T) {
	tr := defaultTree()
	tr.confs["100"] = "net0: virtio=" + testVMMAC + ",bridge=vmbr1\n"
	mustFail(t, fakeNet(t, tr), testIP, "MAC mismatch")
}

func TestResolveNoBridgeKeyInConfig(t *testing.T) {
	tr := defaultTree()
	tr.confs["100"] = "net0: virtio=" + testVMMAC + "\n"
	mustFail(t, fakeNet(t, tr), testIP, "MAC mismatch")
}

func TestResolveNoVMConfig(t *testing.T) {
	tr := defaultTree()
	delete(tr.confs, "100")
	mustFail(t, fakeNet(t, tr), testIP, "no VM config")
}

func TestResolveIncompleteARP(t *testing.T) {
	tr := defaultTree()
	tr.arp = []arpEntry{{testIP, "0x0", testVMMAC, "vmbr0"}}
	mustFail(t, fakeNet(t, tr), testIP, "no ARP entry")
	mustFail(t, fakeNet(t, defaultTree()), "192.0.2.51", "no ARP entry")
}

func TestResolveARPIPMatchIsExact(t *testing.T) {
	tr := defaultTree()
	tr.arp = []arpEntry{{"192.0.2.5", "0x2", testVMMAC, "vmbr0"}}
	mustFail(t, fakeNet(t, tr), "192.0.2.50", "no ARP entry")
}

func TestResolveMissingARPFile(t *testing.T) {
	r := fakeNet(t, defaultTree())
	r.ARPPath = filepath.Join(t.TempDir(), "nope")
	mustFail(t, r, testIP, "no ARP entry")
}

func TestResolveIgnoresSnapshotSections(t *testing.T) {
	const macB = "bc:24:11:00:00:bb"
	tr := defaultTree()
	tr.confs["100"] = "net0: virtio=" + testVMMAC + ",bridge=vmbr0\n\n[snap1]\nnet0: virtio=" + macB + ",bridge=vmbr0\n"
	r := fakeNet(t, tr)
	mustResolve(t, r, testIP, "100")

	tr2 := defaultTree()
	tr2.confs["100"] = tr.confs["100"]
	tr2.arp = []arpEntry{{testIP, "0x2", macB, "vmbr0"}}
	tr2.fdb["vmbr0"] = []fdbEntry{{macB, 3, false}}
	mustFail(t, fakeNet(t, tr2), testIP, "MAC mismatch")
}

func TestResolveLocalFDBEntrySkipped(t *testing.T) {
	tr := defaultTree()
	tr.fdb["vmbr0"] = []fdbEntry{{testVMMAC, 3, true}}
	mustFail(t, fakeNet(t, tr), testIP, "no FDB entry")
}

func TestResolveFDBEntryForOtherMAC(t *testing.T) {
	tr := defaultTree()
	tr.fdb["vmbr0"] = []fdbEntry{{"bc:24:11:00:00:77", 3, false}}
	mustFail(t, fakeNet(t, tr), testIP, "no FDB entry")
}

func TestResolveMalformedFDB(t *testing.T) {
	tr := defaultTree()
	tr.fdb = nil
	tr.rawFDB = map[string][]byte{"vmbr0": make([]byte, 17)}
	mustFail(t, fakeNet(t, tr), testIP, "no FDB entry")
}

func TestResolveAmbiguousFDBPorts(t *testing.T) {
	tr := defaultTree()
	tr.fdb["vmbr0"] = []fdbEntry{{testVMMAC, 3, false}, {testVMMAC, 1, false}}
	mustFail(t, fakeNet(t, tr), testIP, "not a VM bridge port")
}

func TestResolveDuplicateFDBSamePortAllowed(t *testing.T) {
	tr := defaultTree()
	tr.fdb["vmbr0"] = []fdbEntry{{testVMMAC, 3, false}, {testVMMAC, 3, false}}
	mustResolve(t, fakeNet(t, tr), testIP, "100")
}

func TestResolveIPv6(t *testing.T) {
	r := fakeNet(t, defaultTree())
	mustFail(t, r, "2001:db8::1", "ipv6 not supported")
	mustResolve(t, r, "::ffff:"+testIP, "100")
}

func TestResolveNotABridge(t *testing.T) {
	tr := defaultTree()
	tr.arp = []arpEntry{{testIP, "0x2", testVMMAC, "eth0"}}
	tr.plain = []string{"eth0"}
	mustFail(t, fakeNet(t, tr), testIP, "not a bridge")
}

func TestResolveBridgeMarkerMustBeDirectory(t *testing.T) {
	r := fakeNet(t, defaultTree())
	p := filepath.Join(r.SysClassNet, "vmbr0", "bridge")
	if err := os.RemoveAll(p); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	mustFail(t, r, testIP, "not a bridge")
}

func TestResolveDeviceNameCannotEscape(t *testing.T) {
	tr := defaultTree()
	tr.arp = []arpEntry{{testIP, "0x2", testVMMAC, ".."}}
	mustFail(t, fakeNet(t, tr), testIP, "not a bridge")
}

func TestResolveDefaultPaths(t *testing.T) {
	// Empty fields fall back to the host defaults; on a machine without them
	// the lookup fails closed rather than panicking.
	_, err := MACResolver{}.Resolve(netip.MustParseAddr(testIP))
	var re *ResolveError
	if !errors.As(err, &re) {
		t.Fatalf("err = %v, want ResolveError", err)
	}
}
