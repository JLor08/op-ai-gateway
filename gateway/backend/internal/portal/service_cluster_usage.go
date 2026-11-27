// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"errors"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/routing"
	"strconv"
	"strings"
)

// ErrClusterMemberNotFound (404): the server is no member of the head, or
// the position named is no member of the head on that server.
var ErrClusterMemberNotFound = errors.New("runtime_spec.cluster_member_not_found")

// ErrClusterMemberAmbiguous (409): the head has several members on the server
// and the request named no position to tell which one.
var ErrClusterMemberAmbiguous = errors.New("runtime_spec.cluster_member_ambiguous")

// automaticMemberSpecPrefix starts the agent spec id of an automatic member.
const automaticMemberSpecPrefix = "rpca_"

// automaticMemberAgentSpecID is the agent spec id of the nth automatic
// member (1-based, in member order) of headSpecID on serverID: derived from
// both, so a server serving two heads runs two distinct members. The first
// keeps the id it had before a head could have several members on one
// server, so an existing cluster's members never relaunch for it; a later
// one gets an "_m<nth>" suffix (server ids are "srv_<hex>", so it cannot
// collide with another server's id).
func automaticMemberAgentSpecID(headSpecID, serverID string, nth int) string {
	id := automaticMemberSpecPrefix + headSpecID + "_" + serverID
	if nth <= 1 {
		return id
	}
	return id + "_m" + strconv.Itoa(nth)
}

// MemberAgentSpecID is the agent spec id the runtime documents push for the
// membership row m: the manual member spec's id, or the automatic member's
// derived one. members are the rows m is read with -- the head's membership,
// or every membership on m's server; an automatic member's number is 1 plus
// the automatic members of its head on its server at a lower position. A
// member's telemetry names its process by this id.
func MemberAgentSpecID(m routing.ClusterMember, members []routing.ClusterMember) string {
	if m.MemberSpecID != "" {
		return m.MemberSpecID
	}
	return automaticMemberAgentSpecID(m.HeadSpecID, m.ServerID, automaticMemberNth(m, members))
}

// AutomaticMemberHead is the head spec id an automatic member's agent spec id
// on serverID names (automaticMemberAgentSpecID: "rpca_<head>_<server>",
// with "_m<n>" after it for a later member of the head on the server); ok is
// false for any other id.
func AutomaticMemberHead(id, serverID string) (string, bool) {
	rest, ok := strings.CutPrefix(id, automaticMemberSpecPrefix)
	if !ok {
		return "", false
	}
	if i := strings.LastIndex(rest, "_m"); i >= 0 && i+2 < len(rest) && strings.Trim(rest[i+2:], "0123456789") == "" {
		rest = rest[:i]
	}
	head, ok := strings.CutSuffix(rest, "_"+serverID)
	if !ok {
		return "", false
	}
	return head, true
}

// automaticMemberNth is the automatic member m's 1-based number among the
// automatic members of its head on its server, by position.
func automaticMemberNth(m routing.ClusterMember, members []routing.ClusterMember) int {
	nth := 1
	for _, o := range members {
		if o.HeadSpecID == m.HeadSpecID && o.ServerID == m.ServerID && o.MemberSpecID == "" && o.Position < m.Position {
			nth++
		}
	}
	return nth
}

// AuthorizeClusterMemberUsageReset authorizes a reset of the stored usage of
// a member on serverID of the cluster head on mappingID and returns that
// membership row: the member at position, else the server's one member of
// the head. A reset changes the demand pushed to the member's host, so it
// needs management rights on the head's server (through its mapping) and on
// the member's server, like a membership write. Refusals, in order: the
// head's mapping (ErrMappingNotFound, the no-leak collapse), a mapping
// without a spec (ErrRuntimeSpecNotFound), the member's server
// (ErrServerNotFound, the no-leak answer for a server the caller may not
// manage), a server that is no member of the head or a position that is no
// member of the head on serverID (ErrClusterMemberNotFound), and no position
// while the head has several members on serverID (ErrClusterMemberAmbiguous).
// The gateway applies the reset itself, beside the telemetry ingest that
// writes the same row.
func (s *Service) AuthorizeClusterMemberUsageReset(ctx context.Context, principal auth.Token, mappingID, serverID string, position *int) (routing.ClusterMember, error) {
	mapping, _, _, err := s.authorizeMapping(ctx, principal, mappingID)
	if err != nil {
		return routing.ClusterMember{}, err
	}
	head, ok, err := s.routes.RuntimeSpecByMapping(ctx, mapping.ID)
	if err != nil {
		return routing.ClusterMember{}, err
	}
	if !ok {
		return routing.ClusterMember{}, ErrRuntimeSpecNotFound
	}
	if _, err := s.authorizeServer(ctx, principal, serverID); err != nil {
		return routing.ClusterMember{}, err
	}
	members, _, err := s.routes.ClusterMembers(ctx, head.ID)
	if err != nil {
		return routing.ClusterMember{}, err
	}
	var here []routing.ClusterMember
	for _, m := range members {
		if m.ServerID == serverID && (position == nil || m.Position == *position) {
			here = append(here, m)
		}
	}
	switch {
	case len(here) == 0:
		return routing.ClusterMember{}, ErrClusterMemberNotFound
	case len(here) > 1:
		return routing.ClusterMember{}, ErrClusterMemberAmbiguous
	}
	return here[0], nil
}
