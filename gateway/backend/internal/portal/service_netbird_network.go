// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"net/netip"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/netbird"
	"strings"
)

// NetbirdNetworkDTO is the editable subset of the NetBird account's network
// settings. It carries NO admin token.
type NetbirdNetworkDTO struct {
	DNSDomain         string   `json:"dns_domain"`
	NetworkRange      string   `json:"network_range"`
	NetworkRangeV6    string   `json:"network_range_v6"`
	IPv6EnabledGroups []string `json:"ipv6_enabled_groups"`
}

// NetbirdNetwork reads the account's current network settings from NetBird.
func (s *Service) NetbirdNetwork(ctx context.Context) (NetbirdNetworkDTO, error) {
	cfg, ok, err := s.NetbirdConfig(ctx)
	if err != nil {
		return NetbirdNetworkDTO{}, err
	}
	if !ok {
		return NetbirdNetworkDTO{}, ErrNetbirdModuleDisabled
	}
	ncfg := netbird.Config{URL: cfg.URL, Token: cfg.Token}
	acct, err := netbird.GetAccount(ctx, ncfg, netbirdCallTimeout)
	if err != nil {
		return NetbirdNetworkDTO{}, err
	}
	return netbirdNetworkFromSettings(acct.Settings), nil
}

// SetNetbirdNetwork writes the four network fields to the NetBird account via a
// read-modify-write (GET account → patch only these keys in the raw settings map
// → PUT the whole map) so every unmanaged setting is preserved. On a dns_domain
// change it fires the best-effort domain-sync hook.
//
// The route is system-scoped (requireWebScope("system") at the handler); as of
// PT-2 Part 2b this also checks isSystem(principal) itself (ErrPrincipalForbidden
// otherwise) as defense-in-depth against a future internal caller that bypasses
// the HTTP gate.
func (s *Service) SetNetbirdNetwork(ctx context.Context, principal auth.Token, req NetbirdNetworkDTO) (NetbirdNetworkDTO, error) {
	if !isSystem(principal) {
		return NetbirdNetworkDTO{}, ErrPrincipalForbidden
	}
	cfg, ok, err := s.NetbirdConfig(ctx)
	if err != nil {
		return NetbirdNetworkDTO{}, err
	}
	if !ok {
		return NetbirdNetworkDTO{}, ErrNetbirdModuleDisabled
	}
	dns := strings.TrimSpace(req.DNSDomain)
	v4 := strings.TrimSpace(req.NetworkRange)
	v6 := strings.TrimSpace(req.NetworkRangeV6)
	if v4 != "" {
		if p, perr := netip.ParsePrefix(v4); perr != nil || !p.Addr().Is4() {
			return NetbirdNetworkDTO{}, ErrNetbirdNetworkRangeInvalid
		}
	}
	if v6 != "" {
		if p, perr := netip.ParsePrefix(v6); perr != nil || !p.Addr().Is6() || p.Addr().Is4In6() {
			return NetbirdNetworkDTO{}, ErrNetbirdNetworkRangeInvalid
		}
	}
	ncfg := netbird.Config{URL: cfg.URL, Token: cfg.Token}
	acct, err := netbird.GetAccount(ctx, ncfg, netbirdCallTimeout)
	if err != nil {
		return NetbirdNetworkDTO{}, err
	}
	oldDNS := netbirdSettingsString(acct.Settings, "dns_domain")
	settings := acct.Settings
	if settings == nil {
		settings = map[string]any{}
	}
	// Edit-what-is-shown: always write the trimmed value the operator has (the
	// field was pre-loaded from the account). NetBird is authoritative on empty.
	settings["dns_domain"] = dns
	settings["network_range"] = v4
	settings["network_range_v6"] = v6
	groups := req.IPv6EnabledGroups
	if groups == nil {
		groups = []string{}
	}
	settings["ipv6_enabled_groups"] = groups
	if err := netbird.UpdateAccountSettings(ctx, ncfg, netbirdCallTimeout, acct.ID, settings); err != nil {
		return NetbirdNetworkDTO{}, err
	}
	if dns != oldDNS && s.netbird.onDomainChanged != nil {
		s.netbird.onDomainChanged()
	}
	// Return the canonical values we just validated + wrote. Do NOT re-derive from
	// the patched `settings` map: it now mixes JSON-decoded values (foreign keys,
	// []any) with our Go-native patches (ipv6_enabled_groups is a []string), and
	// netbirdNetworkFromSettings' []any type-assertion drops the []string to empty.
	return NetbirdNetworkDTO{
		DNSDomain:         dns,
		NetworkRange:      v4,
		NetworkRangeV6:    v6,
		IPv6EnabledGroups: groups,
	}, nil
}

func netbirdNetworkFromSettings(settings map[string]any) NetbirdNetworkDTO {
	return NetbirdNetworkDTO{
		DNSDomain:         netbirdSettingsString(settings, "dns_domain"),
		NetworkRange:      netbirdSettingsString(settings, "network_range"),
		NetworkRangeV6:    netbirdSettingsString(settings, "network_range_v6"),
		IPv6EnabledGroups: netbirdSettingsStringSlice(settings, "ipv6_enabled_groups"),
	}
}

func netbirdSettingsString(m map[string]any, key string) string {
	if s, ok := m[key].(string); ok {
		return s
	}
	return ""
}

func netbirdSettingsStringSlice(m map[string]any, key string) []string {
	raw, ok := m[key].([]any)
	if !ok {
		return []string{}
	}
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}
