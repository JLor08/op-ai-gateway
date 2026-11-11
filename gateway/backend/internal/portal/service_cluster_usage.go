// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"errors"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/routing"
	"slices"
)

// ErrClusterMemberNotFound (404): the server is no member of the head.
var ErrClusterMemberNotFound = errors.New("runtime_spec.cluster_member_not_found")

// automaticMemberSpecPrefix starts the agent spec id of an automatic member.
const automaticMemberSpecPrefix = "rpca_"

// automaticMemberAgentSpecID is the agent spec id of the automatic member of
// headSpecID on serverID: derived from both, so a server serving two heads
// runs two distinct members.
func automaticMemberAgentSpecID(headSpecID, serverID string) string {
	return automaticMemberSpecPrefix + headSpecID + "_" + serverID
}

// MemberAgentSpecID is the agent spec id the runtime documents push for the
// membership row m: the manual member spec's id, or the automatic member's
// derived one. A member's telemetry names its process by this id.
func MemberAgentSpecID(m routing.ClusterMember) string {
	if m.MemberSpecID != "" {
		return m.MemberSpecID
	}
	return automaticMemberAgentSpecID(m.HeadSpecID, m.ServerID)
}

// AuthorizeClusterMemberUsageReset authorizes a reset of the stored usage of
// the member on serverID of the cluster head on mappingID and returns that
// membership row. A reset changes the demand pushed to the member's host, so
// it needs management rights on the head's server (through its mapping) and
// on the member's server, like a membership write. Refusals, in order: the
// head's mapping (ErrMappingNotFound, the no-leak collapse), a mapping
// without a spec (ErrRuntimeSpecNotFound), the member's server
// (ErrServerNotFound, the no-leak answer for a server the caller may not
// manage), and a server that is no member of the head
// (ErrClusterMemberNotFound). The gateway applies the reset itself, beside
// the telemetry ingest that writes the same row.
func (s *Service) AuthorizeClusterMemberUsageReset(ctx context.Context, principal auth.Token, mappingID, serverID string) (routing.ClusterMember, error) {
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
	i := slices.IndexFunc(members, func(m routing.ClusterMember) bool { return m.ServerID == serverID })
	if i < 0 {
		return routing.ClusterMember{}, ErrClusterMemberNotFound
	}
	return members[i], nil
}
