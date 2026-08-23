// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"op-ai-gateway/internal/auth"
	"sort"
	"strings"
)

// AuthorizeServerManage reports whether principal may MANAGE serverID — the
// same gate as every other server-management surface (authorizeServer: system
// scope, a ServerOwners member, or a can_manage_servers co-manager of one of
// the server's linked admin groups). Returns nil when manageable, else
// ErrServerNotFound (404-no-leak — a non-manager and a stranger get the exact
// same error as an unknown id, never a distinguishable 403). This is the
// gateway's re-authorization gate for a token's server_override (checked on
// every routed request, since a token outlives the manage-grant that created
// its override) and the gate the token create/update self-heal below reuses.
func (s *Service) AuthorizeServerManage(ctx context.Context, principal auth.Token, serverID string) error {
	_, err := s.authorizeServer(ctx, principal, serverID)
	return err
}

// ServerModelDTO is a minimal (id, display_name) pair identifying a gateway
// model offered by one specific server. It mirrors the id/display_name shape
// of ModelDTO/ModelOption without the cross-server visibility/loaded/vision
// overlay — those are properties of the model across every offering server,
// not of this one server's offering.
type ServerModelDTO struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
}

// ServerModels returns the distinct gateway models serverID currently offers
// (an active mapping on an active application on an active + healthy
// server), gated by AuthorizeServerManage — a non-manager or a stranger both
// get ErrServerNotFound (404-no-leak), never a listing for a server they
// cannot manage. Deliberately built from the UNFILTERED activeMappingViews,
// not visibleMappingViews' resource-group-provisioning filter: a server
// override BYPASSES provisioning by design once routed (see
// resolver.resolveServerOverride), so the manager configuring one needs to
// see everything the server offers, not only the subset provisioning would
// currently let them use. An unknown gateway model name simply yields an
// empty list further up (Models()); here the server itself is the object
// being authorized, and its offered-model set is otherwise unfiltered.
func (s *Service) ServerModels(ctx context.Context, principal auth.Token, serverID string) ([]ServerModelDTO, error) {
	serverID = strings.TrimSpace(serverID)
	if err := s.AuthorizeServerManage(ctx, principal, serverID); err != nil {
		return nil, err
	}
	views, err := s.activeMappingViews(ctx)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]struct{})
	out := make([]ServerModelDTO, 0)
	for _, view := range views {
		if view.server.ID != serverID {
			continue
		}
		name := view.mapping.GatewayModelName
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, ServerModelDTO{ID: name, DisplayName: name})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// validateServerOverride self-heals a token's server-override id against the
// owner's CURRENT server-management rights, rather than rejecting the whole
// create/update: a token belongs to its owner and outlives any transient
// change in who manages the override's target server, so an id the owner can
// no longer manage — the server was deleted, or the owner lost (or never had)
// manage rights on it, e.g. a co-manager grant on the linked admin group was
// revoked — is silently cleared to "" instead of failing the request. Blank
// in stays blank out, with no AuthorizeServerManage call.
func (s *Service) validateServerOverride(ctx context.Context, owner auth.Token, serverID string) string {
	serverID = strings.TrimSpace(serverID)
	if serverID == "" {
		return ""
	}
	if err := s.AuthorizeServerManage(ctx, owner, serverID); err != nil {
		return ""
	}
	return serverID
}
