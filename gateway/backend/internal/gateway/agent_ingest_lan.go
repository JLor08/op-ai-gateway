// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/netip"
	"op-ai-gateway/internal/portal"
	"slices"
	"sort"
	"strings"
)

// Caps on the LAN interface list one sample may carry; a host has a handful
// of interfaces, so anything beyond these is a malformed or hostile report.
const (
	maxLANInterfaces        = 32
	maxLANInterfaceIPv4s    = 8
	maxLANInterfaceStrBytes = 64
	lanIDKindKeyed          = "keyed"
	lanIDKindName           = "name"
)

// cgnatPrefix is 100.64.0.0/10, the range NetBird (and carrier-grade NAT)
// uses; a NetBird address is never a LAN candidate.
var cgnatPrefix = netip.MustParsePrefix("100.64.0.0/10")

// agentNetInterface is one entry of the telemetry sample's interfaces field,
// the gateway-side mirror of the agent's sample.NetInterface. Its JSON keys
// are also the stored form portal.LANResolutionOf parses.
type agentNetInterface struct {
	ID        string   `json:"id"`
	IDKind    string   `json:"id_kind"`
	Name      string   `json:"name"`
	IPv4      []string `json:"ipv4"`
	Virtual   bool     `json:"virtual"`
	Preferred bool     `json:"preferred"`
}

// sanitizeNetInterfaces turns the reported list into the stored one: only
// IPv4 literals that can be a LAN address survive (no 100.64/10, link-local,
// loopback, unspecified, multicast, nor the server's own netbird_ip),
// interfaces left without an address or with an unknown id_kind or empty id
// are dropped, strings are clamped, at most one interface stays preferred,
// and the result is sorted by name (then id) and capped, so equal reports
// always serialize identically.
func sanitizeNetInterfaces(in []agentNetInterface, netbirdIP string) []agentNetInterface {
	out := make([]agentNetInterface, 0, min(len(in), maxLANInterfaces))
	for _, n := range in {
		if clean, ok := sanitizeNetInterface(n, netbirdIP); ok {
			out = append(out, clean)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].ID < out[j].ID
	})
	if len(out) > maxLANInterfaces {
		out = out[:maxLANInterfaces]
	}
	keepFirstPreferred(out)
	return out
}

// sanitizeNetInterface cleans one reported interface; ok is false when it has
// an unknown id_kind, an empty id or no usable address.
func sanitizeNetInterface(n agentNetInterface, netbirdIP string) (agentNetInterface, bool) {
	if n.IDKind != lanIDKindKeyed && n.IDKind != lanIDKindName {
		return agentNetInterface{}, false
	}
	id := clampLANString(n.ID)
	if id == "" {
		return agentNetInterface{}, false
	}
	ips := filterLANIPv4s(n.IPv4, netbirdIP)
	if len(ips) == 0 {
		return agentNetInterface{}, false
	}
	return agentNetInterface{
		ID: id, IDKind: n.IDKind, Name: clampLANString(n.Name), IPv4: ips,
		Virtual: n.Virtual, Preferred: n.Preferred,
	}, true
}

// filterLANIPv4s keeps the distinct LAN-candidate addresses, at most
// maxLANInterfaceIPv4s of them, in report order.
func filterLANIPv4s(raws []string, netbirdIP string) []string {
	var ips []string
	for _, raw := range raws {
		if len(ips) >= maxLANInterfaceIPv4s {
			break
		}
		if ip, ok := lanCandidateIPv4(raw, netbirdIP); ok && !slices.Contains(ips, ip) {
			ips = append(ips, ip)
		}
	}
	return ips
}

// keepFirstPreferred clears Preferred on every entry after the first one.
func keepFirstPreferred(list []agentNetInterface) {
	seen := false
	for i := range list {
		if list[i].Preferred && seen {
			list[i].Preferred = false
		}
		seen = seen || list[i].Preferred
	}
}

func lanCandidateIPv4(raw, netbirdIP string) (string, bool) {
	addr, err := netip.ParseAddr(strings.TrimSpace(raw))
	if err != nil || !addr.Is4() {
		return "", false
	}
	if addr.IsLoopback() || addr.IsLinkLocalUnicast() || addr.IsUnspecified() || addr.IsMulticast() || cgnatPrefix.Contains(addr) {
		return "", false
	}
	if nb, err := netip.ParseAddr(netbirdIP); err == nil && nb == addr {
		return "", false
	}
	return addr.String(), true
}

func clampLANString(s string) string {
	s = strings.ReplaceAll(s, "\x00", "")
	if len(s) > maxLANInterfaceStrBytes {
		return strings.ToValidUTF8(s[:maxLANInterfaceStrBytes], "")
	}
	return s
}

// writeBackLANInterfaces stores the agent-reported LAN interface list through
// the targeted writer. An absent field is "unchanged" (an old agent, or one
// whose list did not change). A report equal to the stored list writes
// nothing. Clusters are notified only when the server's effective LAN
// address, identity addresses or fallback state differ before and after, so a
// resend or an added non-preferred interface relaunches nobody. Best-effort:
// a store failure is logged and never rejects the sample.
func (s *Server) writeBackLANInterfaces(ctx context.Context, serverID string, reported *[]agentNetInterface) {
	if reported == nil || s.Routes == nil {
		return
	}
	before, err := s.Routes.AIServerByID(ctx, serverID)
	if err != nil {
		slog.Warn("agent LAN interface read failed", "server_id", serverID, "err", err)
		return
	}
	list := sanitizeNetInterfaces(*reported, before.NetbirdIP)
	if list == nil {
		list = []agentNetInterface{}
	}
	raw, err := json.Marshal(list)
	if err != nil {
		slog.Warn("agent LAN interface encode failed", "server_id", serverID, "err", err)
		return
	}
	next := string(raw)
	if before.LANInterfaces == next {
		return
	}
	if err := s.Routes.UpdateServerLANInterfaces(ctx, serverID, next); err != nil {
		slog.Warn("agent LAN interface write failed", "server_id", serverID, "err", err)
		return
	}
	if s.Portal == nil {
		return
	}
	after := before
	after.LANInterfaces = next
	was, now := portal.LANResolutionOf(before), portal.LANResolutionOf(after)
	if was.Address != now.Address || was.Fallback != now.Fallback || !slices.Equal(was.IdentityIPs, now.IdentityIPs) {
		s.Portal.NotifyClustersOfServer(ctx, serverID)
	}
}
