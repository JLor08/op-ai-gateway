// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package collector

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net"
	"net/netip"
	"op-ai-server-agent/internal/sample"
	"regexp"
	"slices"
	"strings"
)

// netbirdName matches NetBird's WireGuard interface names (wt0, wt1, ...).
// Matched case-sensitively.
var netbirdName = regexp.MustCompile(`^wt[0-9]+$`)

// virtualPrefixes are lower-case name prefixes of virtual/overlay interfaces.
var virtualPrefixes = []string{
	"docker", "br-", "veth", "virbr", "vethernet", "vmnet", "vboxnet", "tun",
	"tap", "utun", "zt", "tailscale", "wg", "lxc", "cni", "flannel",
}

var (
	cgnatPrefix     = netip.MustParsePrefix("100.64.0.0/10")
	linkLocalPrefix = netip.MustParsePrefix("169.254.0.0/16")
	loopbackPrefix  = netip.MustParsePrefix("127.0.0.0/8")
)

// InterfaceCollector enumerates the host's LAN address candidates. The
// functions are fields so tests can substitute fakes.
type InterfaceCollector struct {
	interfaces  func() ([]net.Interface, error)
	addrs       func(net.Interface) ([]net.Addr, error)
	routeSource func() (net.IP, error)
	idKey       []byte
}

// NewInterfaceCollector builds a collector. idKey is the raw key used to
// derive opaque interface IDs; nil means no key (IDs fall back to the name).
func NewInterfaceCollector(idKey []byte) *InterfaceCollector {
	return &InterfaceCollector{
		interfaces: net.Interfaces,
		addrs: func(i net.Interface) ([]net.Addr, error) {
			return i.Addrs()
		},
		routeSource: defaultRouteSource,
		idKey:       idKey,
	}
}

// defaultRouteSource returns the local address the kernel would use to reach
// an external IPv4 address. UDP connect sends no packet.
func defaultRouteSource() (net.IP, error) {
	conn, err := net.Dial("udp4", "192.0.2.1:9")
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()
	if ua, ok := conn.LocalAddr().(*net.UDPAddr); ok {
		return ua.IP, nil
	}
	return nil, net.UnknownNetworkError("no udp local address")
}

type ifCandidate struct {
	out      sample.NetInterface
	hasRoute bool
	hasRFC   bool
	hw       net.HardwareAddr
}

// Collect returns the LAN candidates sorted by name, with exactly one
// Preferred entry when the list is non-empty.
func (c *InterfaceCollector) Collect(ctx context.Context) ([]sample.NetInterface, error) {
	_ = ctx
	ifs, err := c.interfaces()
	if err != nil {
		return nil, err
	}
	var route netip.Addr
	if ip, rerr := c.routeSource(); rerr == nil {
		if a, ok := netip.AddrFromSlice(ip); ok {
			route = a.Unmap()
		}
	}

	var cands []ifCandidate
	for _, ifc := range ifs {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 || netbirdName.MatchString(ifc.Name) {
			continue
		}
		raw, aerr := c.addrs(ifc)
		if aerr != nil {
			continue
		}
		var addrs []netip.Addr
		for _, a := range raw {
			var ip net.IP
			switch v := a.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			na, ok := netip.AddrFromSlice(ip)
			if !ok {
				continue
			}
			na = na.Unmap()
			if !na.Is4() || cgnatPrefix.Contains(na) || linkLocalPrefix.Contains(na) || loopbackPrefix.Contains(na) {
				continue
			}
			addrs = append(addrs, na)
		}
		if len(addrs) == 0 {
			continue
		}
		cands = append(cands, c.build(ifc, addrs, route))
	}

	slices.SortFunc(cands, func(a, b ifCandidate) int { return strings.Compare(a.out.Name, b.out.Name) })
	c.assignIDs(cands)
	out := make([]sample.NetInterface, 0, len(cands))
	for _, cd := range cands {
		out = append(out, cd.out)
	}
	if best := preferredIndex(cands); best >= 0 {
		out[best].Preferred = true
	}
	return out, nil
}

func (c *InterfaceCollector) build(ifc net.Interface, addrs []netip.Addr, route netip.Addr) ifCandidate {
	rank := func(a netip.Addr) int {
		switch {
		case route.IsValid() && a == route:
			return 0
		case a.IsPrivate():
			return 1
		default:
			return 2
		}
	}
	slices.SortFunc(addrs, func(a, b netip.Addr) int {
		if ra, rb := rank(a), rank(b); ra != rb {
			return ra - rb
		}
		return a.Compare(b)
	})
	addrs = slices.Compact(addrs)
	cd := ifCandidate{}
	strs := make([]string, len(addrs))
	for i, a := range addrs {
		strs[i] = a.String()
		if route.IsValid() && a == route {
			cd.hasRoute = true
		}
		if a.IsPrivate() {
			cd.hasRFC = true
		}
	}
	cd.out = sample.NetInterface{
		Name:    ifc.Name,
		IPv4:    strs,
		Virtual: isVirtualName(ifc.Name),
	}
	cd.hw = ifc.HardwareAddr
	return cd
}

// assignIDs sets each candidate's opaque ID. VLAN and macvlan sub-interfaces
// share their parent's MAC, so when two or more candidates share a non-empty
// MAC each of them also mixes its name into the keyed ID; a unique MAC keeps
// the MAC-only ID.
func (c *InterfaceCollector) assignIDs(cands []ifCandidate) {
	if len(c.idKey) == 0 {
		for i := range cands {
			cands[i].out.ID, cands[i].out.IDKind = cands[i].out.Name, "name"
		}
		return
	}
	macCount := map[string]int{}
	for _, cd := range cands {
		if len(cd.hw) > 0 {
			macCount[string(cd.hw)]++
		}
	}
	for i := range cands {
		cd := &cands[i]
		m := hmac.New(sha256.New, c.idKey)
		switch {
		case len(cd.hw) == 0:
			m.Write([]byte(cd.out.Name))
		case macCount[string(cd.hw)] > 1:
			m.Write(cd.hw)
			m.Write([]byte{0})
			m.Write([]byte(cd.out.Name))
		default:
			m.Write(cd.hw)
		}
		cd.out.ID = hex.EncodeToString(m.Sum(nil)[:16])
		cd.out.IDKind = "keyed"
	}
}

func isVirtualName(name string) bool {
	l := strings.ToLower(name)
	for _, p := range virtualPrefixes {
		if strings.HasPrefix(l, p) {
			return true
		}
	}
	return false
}

// preferredIndex ranks by route source, RFC 1918, non-virtual, then name.
// cands is already name-sorted, so a stable "first best wins" scan breaks
// the remaining ties by name ascending.
func preferredIndex(cands []ifCandidate) int {
	best := -1
	score := func(cd ifCandidate) [3]bool {
		return [3]bool{cd.hasRoute, cd.hasRFC, !cd.out.Virtual}
	}
	for i, cd := range cands {
		if best < 0 {
			best = i
			continue
		}
		s, b := score(cd), score(cands[best])
		for k := range s {
			if s[k] != b[k] {
				if s[k] {
					best = i
				}
				break
			}
		}
	}
	return best
}
