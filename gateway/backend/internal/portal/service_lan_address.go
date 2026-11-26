// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"encoding/json"
	"net/netip"
	"op-ai-gateway/internal/routing"
	"slices"
	"strings"
)

// LANSourceDomain is the effective LAN address source of a server a lan
// cluster reaches at its Domain (LANResolution.Source): a non-NetBird server
// with a Domain and no address of its own, exactly as before the agent
// reported interfaces.
const LANSourceDomain = "domain"

// lanIDKindKeyed is the id_kind of an interface id the agent keyed with its
// agent_key; the other kind, "name", is the interface name itself.
const lanIDKindKeyed = "keyed"

// netbirdCGNAT is the CGNAT range NetBird assigns its mesh addresses from:
// never a LAN candidate.
var netbirdCGNAT = netip.MustParsePrefix("100.64.0.0/10")

// LANInterfaceView is one agent-reported LAN interface as the gateway reads
// it from ai_servers.lan_interfaces: a stable opaque id (keyed or the name),
// the display name, its IPv4s (primary first), and the agent's virtual and
// preferred marks.
type LANInterfaceView struct {
	ID        string   `json:"id"`
	IDKind    string   `json:"id_kind"`
	Name      string   `json:"name"`
	IPv4      []string `json:"ipv4"`
	Virtual   bool     `json:"virtual"`
	Preferred bool     `json:"preferred"`
}

// LANResolution is a server's effective LAN address, derived from its LAN
// address source and its last reported interface list (LANResolutionOf).
type LANResolution struct {
	// Source is the effective source: routing.LANSourceManual,
	// routing.LANSourceInterface, routing.LANSourceAuto (also for the
	// fallback of a missing chosen interface), LANSourceDomain, or "" when
	// the server has no LAN address.
	Source string
	// Address is the effective IPv4; "" when none (or Source is
	// LANSourceDomain).
	Address string
	// InterfaceID and InterfaceName name the reported interface Address
	// comes from (the chosen one, or the auto pick).
	InterfaceID   string
	InterfaceName string
	// Fallback: the chosen interface is missing from the report, and the
	// auto pick (or the Domain, or nothing) is in use instead.
	Fallback bool
	// IdentityIPs is how members recognise the server as a head: the manual
	// address under manual, every reported IPv4 (sorted, deduplicated) under
	// interface and auto, nil under domain or without an address.
	IdentityIPs []string
	// Reported: the agent has reported a list (lan_interfaces != "").
	Reported bool
	// KeyedIDs: every reported id is keyed (agent_key configured); false
	// when nothing was reported.
	KeyedIDs bool
	// Interfaces is the reported list, NetBird addresses removed.
	Interfaces []LANInterfaceView
}

// LANResolutionOf derives srv's effective LAN address from the row alone, so
// a gateway that has just restarted derives clusters before any agent
// reconnects.
//
//   - The source is LANSource; "" (a row written before migration 110) reads
//     as manual when LANAddress is set, else auto.
//   - manual: LANAddress. Without one, like an empty lan_address before:
//     the Domain of a non-NetBird server, else nothing.
//   - interface: the reported interface whose id is LANInterfaceID, else
//     the one whose name is (a choice made before the agent had a key).
//     Missing: Fallback, the auto pick, else the Domain of a non-NetBird
//     server, else nothing.
//   - auto (and any other value): the Domain of a non-NetBird server that
//     has one, else the auto pick, else nothing.
//
// The auto pick is the agent's preferred interface, else the first
// non-virtual one. An address in 100.64.0.0/10 or equal to netbird_ip is
// never a candidate, whatever the stored list carries.
func LANResolutionOf(srv routing.AIServer) LANResolution {
	var res LANResolution
	res.Interfaces, res.Reported = reportedLANInterfaces(srv)
	res.KeyedIDs = res.Reported && !slices.ContainsFunc(res.Interfaces, func(v LANInterfaceView) bool { return v.IDKind != lanIDKindKeyed })
	switch configuredLANSource(srv) {
	case routing.LANSourceManual:
		if srv.LANAddress != "" {
			res.Source, res.Address, res.IdentityIPs = routing.LANSourceManual, srv.LANAddress, []string{srv.LANAddress}
			return res
		}
		return res.withDomainIfDialable(srv)
	case routing.LANSourceInterface:
		if iface, ok := chosenLANInterface(res.Interfaces, srv.LANInterfaceID); ok {
			return res.withInterface(routing.LANSourceInterface, iface)
		}
		res.Fallback = true
		if iface, ok := autoLANInterface(res.Interfaces); ok {
			return res.withInterface(routing.LANSourceAuto, iface)
		}
		return res.withDomainIfDialable(srv)
	default:
		if domainDialable(srv) {
			res.Source = LANSourceDomain
			return res
		}
		if iface, ok := autoLANInterface(res.Interfaces); ok {
			return res.withInterface(routing.LANSourceAuto, iface)
		}
		return res
	}
}

// configuredLANSource is srv's stored source, a pre-migration "" read as
// manual when the row carries a lan_address, else auto.
func configuredLANSource(srv routing.AIServer) string {
	if srv.LANSource != "" {
		return srv.LANSource
	}
	if srv.LANAddress != "" {
		return routing.LANSourceManual
	}
	return routing.LANSourceAuto
}

// domainDialable reports whether a lan cluster can reach srv at its Domain:
// a non-NetBird server with one. A NetBird-enabled server's Domain is the
// NetBird DNS name the peer sync writes.
func domainDialable(srv routing.AIServer) bool {
	return !srv.NetbirdEnabled && strings.TrimSpace(srv.Domain) != ""
}

func (res LANResolution) withDomainIfDialable(srv routing.AIServer) LANResolution {
	if domainDialable(srv) {
		res.Source = LANSourceDomain
	}
	return res
}

func (res LANResolution) withInterface(source string, iface LANInterfaceView) LANResolution {
	res.Source, res.Address = source, iface.IPv4[0]
	res.InterfaceID, res.InterfaceName = iface.ID, iface.Name
	res.IdentityIPs = allReportedIPv4s(res.Interfaces)
	return res
}

// reportedLANInterfaces reads srv's stored list: not reported when it is ""
// or unreadable. Every IPv4 that is no plain unicast IPv4, lies in the
// NetBird range or equals netbird_ip is dropped, then every interface left
// without one.
func reportedLANInterfaces(srv routing.AIServer) ([]LANInterfaceView, bool) {
	if srv.LANInterfaces == "" {
		return nil, false
	}
	var raw []LANInterfaceView
	if err := json.Unmarshal([]byte(srv.LANInterfaces), &raw); err != nil {
		return nil, false
	}
	netbirdIP, _ := netip.ParseAddr(strings.TrimSpace(srv.NetbirdIP))
	out := make([]LANInterfaceView, 0, len(raw))
	for _, v := range raw {
		var ips []string
		for _, s := range v.IPv4 {
			ip, err := netip.ParseAddr(s)
			if err != nil || !ip.Is4() || !ip.IsGlobalUnicast() || netbirdCGNAT.Contains(ip) || ip == netbirdIP {
				continue
			}
			ips = append(ips, ip.String())
		}
		if len(ips) == 0 {
			continue
		}
		v.IPv4 = ips
		out = append(out, v)
	}
	return out, true
}

// chosenLANInterface finds the chosen interface by id, else by name: a
// choice stored while the agent reported name ids still resolves once the
// agent reports keyed ones.
func chosenLANInterface(ifaces []LANInterfaceView, id string) (LANInterfaceView, bool) {
	if id == "" {
		return LANInterfaceView{}, false
	}
	if i := slices.IndexFunc(ifaces, func(v LANInterfaceView) bool { return v.ID == id }); i >= 0 {
		return ifaces[i], true
	}
	if i := slices.IndexFunc(ifaces, func(v LANInterfaceView) bool { return v.Name == id }); i >= 0 {
		return ifaces[i], true
	}
	return LANInterfaceView{}, false
}

// autoLANInterface is the most likely LAN interface: the agent's preferred
// one, else the first non-virtual one.
func autoLANInterface(ifaces []LANInterfaceView) (LANInterfaceView, bool) {
	if i := slices.IndexFunc(ifaces, func(v LANInterfaceView) bool { return v.Preferred }); i >= 0 {
		return ifaces[i], true
	}
	if i := slices.IndexFunc(ifaces, func(v LANInterfaceView) bool { return !v.Virtual }); i >= 0 {
		return ifaces[i], true
	}
	return LANInterfaceView{}, false
}

// allReportedIPv4s is every reported IPv4, deduplicated and in numeric order.
func allReportedIPv4s(ifaces []LANInterfaceView) []string {
	var addrs []netip.Addr
	for _, v := range ifaces {
		for _, s := range v.IPv4 {
			if ip, err := netip.ParseAddr(s); err == nil {
				addrs = append(addrs, ip)
			}
		}
	}
	slices.SortFunc(addrs, netip.Addr.Compare)
	addrs = slices.Compact(addrs)
	out := make([]string, len(addrs))
	for i, ip := range addrs {
		out[i] = ip.String()
	}
	return out
}
