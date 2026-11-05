// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"op-ai-gateway/internal/store"
	"strings"
)

// ErrServerLANAddressInvalid rejects a lan_address that is neither empty nor
// an IPv4 literal (no host names, ports, prefixes or IPv6).
var ErrServerLANAddressInvalid = errors.New("server.lan_address_invalid")

// normalizeLANAddress trims raw and accepts "" (unset) or a plain IPv4
// literal. An IPv4-mapped IPv6 form is rejected so the stored value is always
// the dotted quad the agent binds to.
func normalizeLANAddress(raw string) (string, error) {
	v := strings.TrimSpace(raw)
	if v == "" {
		return "", nil
	}
	addr, err := netip.ParseAddr(v)
	if err != nil || !addr.Is4() {
		return "", ErrServerLANAddressInvalid
	}
	return addr.String(), nil
}

// validateRPCHeadGroups trims and dedupes ids and requires each to be an
// existing admin-tier group. The ids are the member side's consent: a head
// whose server is linked to one of these groups may use this server as an RPC
// member. Any existing admin group is accepted — the member's owner may
// consent to a group they do not manage themselves.
func (s *Service) validateRPCHeadGroups(ctx context.Context, raw []string) ([]string, error) {
	ids := make([]string, 0, len(raw))
	seen := map[string]struct{}{}
	for _, r := range raw {
		id := strings.TrimSpace(r)
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		g, err := s.groups.UserGroupByID(ctx, id)
		if err != nil || g.Tier != store.GroupTierAdmin {
			return nil, ErrServerAdminGroupInvalid
		}
		ids = append(ids, id)
	}
	return ids, nil
}

// encodeRPCHeadGroups stores the list as a JSON array; an empty list is "[]"
// (never ""), so every write leaves a decodable value behind.
func encodeRPCHeadGroups(ids []string) string {
	if len(ids) == 0 {
		return "[]"
	}
	b, err := json.Marshal(ids)
	if err != nil {
		return "[]"
	}
	return string(b)
}

// decodeRPCHeadGroups reads the stored JSON array; "" (a row that predates
// the column default) and undecodable values read as an empty, non-nil list.
func decodeRPCHeadGroups(raw string) []string {
	out := make([]string, 0)
	if strings.TrimSpace(raw) == "" {
		return out
	}
	var ids []string
	if err := json.Unmarshal([]byte(raw), &ids); err != nil {
		return out
	}
	return append(out, ids...)
}
