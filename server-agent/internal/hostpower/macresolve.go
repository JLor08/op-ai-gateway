// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package hostpower

import (
	"bytes"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// Default host paths for MAC-mode identification.
const (
	DefaultARPPath     = "/proc/net/arp"
	DefaultSysClassNet = "/sys/class/net"
	DefaultPVEConfDir  = "/etc/pve/qemu-server"
)

const (
	maxTextFileBytes = 64 << 10
	maxFDBBytes      = 1 << 20
	fdbRecordSize    = 16
	arpFlagComplete  = 0x2
)

// Reasons carried by ResolveError.
const (
	reasonIPv6        = "ipv6 not supported"
	reasonNoARP       = "no ARP entry"
	reasonNotBridge   = "not a bridge"
	reasonNoFDB       = "no FDB entry"
	reasonNotVMPort   = "not a VM bridge port"
	reasonNoVMConfig  = "no VM config"
	reasonMACMismatch = "MAC mismatch"
)

var (
	tapPortPattern  = regexp.MustCompile(`^tap(\d+)i(\d+)$`)
	fwprPortPattern = regexp.MustCompile(`^fwpr(\d+)p(\d+)$`)
)

// ResolveError reports why a remote address could not be tied to one VM.
type ResolveError struct{ Reason string }

func (e *ResolveError) Error() string { return "hostpower: " + e.Reason }

func resolveFail(reason string) (string, error) { return "", &ResolveError{Reason: reason} }

// MACResolver maps a guest's IPv4 address to its VM ID from the host's ARP
// table, the bridge forwarding database and the VM config. Empty fields use
// the host defaults. It keeps no state.
type MACResolver struct {
	ARPPath, SysClassNet, PVEConfDir string
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// Resolve returns the ID of the VM that owns remote, or a *ResolveError.
func (r MACResolver) Resolve(remote netip.Addr) (string, error) {
	remote = remote.Unmap()
	if !remote.Is4() {
		return resolveFail(reasonIPv6)
	}
	sys := orDefault(r.SysClassNet, DefaultSysClassNet)

	mac, dev, ok := lookupARP(orDefault(r.ARPPath, DefaultARPPath), remote.String())
	if !ok {
		return resolveFail(reasonNoARP)
	}

	devDir, ok := bridgeDir(sys, dev)
	if !ok {
		return resolveFail(reasonNotBridge)
	}

	port, reason := fdbPort(devDir, mac)
	if reason != "" {
		return resolveFail(reason)
	}
	ifname, ok := portInterface(devDir, port)
	if !ok {
		return resolveFail(reasonNotVMPort)
	}
	vmid, nic, ok := parsePortName(ifname)
	if !ok {
		return resolveFail(reasonNotVMPort)
	}

	conf, ok, err := readCapped(filepath.Join(orDefault(r.PVEConfDir, DefaultPVEConfDir), vmid+".conf"), maxTextFileBytes)
	if err != nil || !ok {
		return resolveFail(reasonNoVMConfig)
	}
	if !configNICMatches(conf, nic, mac, dev) {
		return resolveFail(reasonMACMismatch)
	}
	return vmid, nil
}

// lookupARP finds the complete entry for ip.
func lookupARP(path, ip string) (mac net.HardwareAddr, dev string, ok bool) {
	data, within, err := readCapped(path, maxTextFileBytes)
	if err != nil || !within {
		return nil, "", false
	}
	lines := strings.Split(string(data), "\n")
	for _, line := range lines[min(1, len(lines)):] { // skip the header
		cols := strings.Fields(line)
		if len(cols) < 6 || cols[0] != ip {
			continue
		}
		flags, err := strconv.ParseUint(strings.TrimPrefix(cols[2], "0x"), 16, 32)
		if err != nil || flags&arpFlagComplete == 0 {
			continue
		}
		hw, err := net.ParseMAC(cols[3])
		if err != nil || len(hw) != 6 {
			continue
		}
		return hw, cols[5], true
	}
	return nil, "", false
}

// bridgeDir returns the sysfs directory of dev when dev is a Linux bridge.
func bridgeDir(sys, dev string) (string, bool) {
	if dev == "" || dev == "." || dev == ".." || strings.ContainsAny(dev, `/\`) {
		return "", false
	}
	dir := filepath.Join(sys, dev)
	fi, err := os.Stat(filepath.Join(dir, "bridge"))
	if err != nil || !fi.IsDir() {
		return "", false
	}
	return dir, true
}

// fdbPort returns the bridge port a non-local forwarding entry for mac was
// learned on. Conflicting ports for one MAC resolve to no identity.
func fdbPort(bridgeDir string, mac net.HardwareAddr) (uint16, string) {
	data, ok, err := readCapped(filepath.Join(bridgeDir, "brforward"), maxFDBBytes)
	if err != nil || !ok || len(data)%fdbRecordSize != 0 {
		return 0, reasonNoFDB
	}
	found := false
	var port uint16
	for off := 0; off+fdbRecordSize <= len(data); off += fdbRecordSize {
		rec := data[off : off+fdbRecordSize]
		if rec[7] != 0 || !bytes.Equal(rec[:6], mac) {
			continue
		}
		p := uint16(rec[12])<<8 | uint16(rec[6])
		if found && p != port {
			return 0, reasonNotVMPort
		}
		found, port = true, p
	}
	if !found {
		return 0, reasonNoFDB
	}
	return port, ""
}

// portInterface returns the bridge member interface with the given port_no.
func portInterface(bridgeDir string, port uint16) (string, bool) {
	brif := filepath.Join(bridgeDir, "brif")
	entries, err := os.ReadDir(brif)
	if err != nil {
		return "", false
	}
	for _, e := range entries {
		raw, ok, err := readCapped(filepath.Join(brif, e.Name(), "port_no"), maxTextFileBytes)
		if err != nil || !ok {
			continue
		}
		no, err := strconv.ParseUint(strings.TrimSpace(string(raw)), 0, 16)
		if err == nil && uint16(no) == port {
			return e.Name(), true
		}
	}
	return "", false
}

func parsePortName(name string) (vmid, nic string, ok bool) {
	for _, re := range []*regexp.Regexp{tapPortPattern, fwprPortPattern} {
		if m := re.FindStringSubmatch(name); m != nil {
			return m[1], m[2], true
		}
	}
	return "", "", false
}

// configNICMatches reports whether the VM config's current (pre-section)
// net<nic> line carries mac and sits on bridge.
func configNICMatches(conf []byte, nic string, mac net.HardwareAddr, bridge string) bool {
	prefix := "net" + nic + ":"
	for _, line := range strings.Split(string(conf), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[") {
			return false // snapshot and pending sections do not count
		}
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		var gotMAC net.HardwareAddr
		gotBridge := ""
		for _, kv := range strings.Split(strings.TrimSpace(strings.TrimPrefix(line, prefix)), ",") {
			key, val, found := strings.Cut(strings.TrimSpace(kv), "=")
			if !found {
				continue
			}
			if key == "bridge" {
				gotBridge = val
				continue
			}
			if gotMAC == nil {
				if hw, err := net.ParseMAC(val); err == nil && len(hw) == 6 {
					gotMAC = hw
				}
			}
		}
		return gotMAC != nil && bytes.Equal(gotMAC, mac) && gotBridge == bridge
	}
	return false
}
