// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"errors"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/store"
	"path"
	"strings"
)

// rpcMemberHiddenHead stands in a member's display name for the model of a
// head the reader may not see: it names nothing, and needs no translation.
const rpcMemberHiddenHead = "—"

// RPCMemberDisplayNames names the RPC member processes specIDs that run on
// serverID for the reader p, by their agent spec id: "RPC (<head>)(<member
// server>)". <head> is the gateway model name of the head the member serves.
// It is "—" for a head p may not see (its mapping is beyond p's rights) and
// for a head whose spec or mapping is gone, so nothing about such a head
// leaks. A manual member spec that serves no head shows its binary's file
// name instead, and an id that matches neither gets no name.
//
// The names are resolved from the stored rows on every call, so a renamed
// model or server shows at once. One call reads the server's membership once,
// its manual member specs at most once, and each head once, however many ids
// it names. A store error on those reads fails the call. The rights check on
// a head's mapping (authorizeMapping) answers not-found for its own store
// errors too, so such an error shows that head as "—" until the next call.
func (s *Service) RPCMemberDisplayNames(ctx context.Context, p auth.Token, serverID string, specIDs []string) (map[string]string, error) {
	srv, err := s.authorizeServer(ctx, p, serverID)
	if err != nil {
		return nil, err
	}
	rows, err := s.routes.ClusterMembershipsByServer(ctx, serverID)
	if err != nil {
		return nil, err
	}
	headOf := make(map[string]string, len(rows))
	for _, m := range rows {
		headOf[MemberAgentSpecID(m)] = m.HeadSpecID
	}
	r := memberNamer{s: s, p: p, serverID: serverID, headOf: headOf, heads: map[string]string{}}
	out := make(map[string]string, len(specIDs))
	for _, id := range specIDs {
		label, ok, err := r.label(ctx, id)
		if err != nil {
			return nil, err
		}
		if ok {
			out[id] = "RPC (" + label + ")(" + srv.Name + ")"
		}
	}
	return out, nil
}

// memberNamer resolves member labels for one RPCMemberDisplayNames call,
// reading each head once.
type memberNamer struct {
	s        *Service
	p        auth.Token
	serverID string
	headOf   map[string]string // agent spec id -> head spec id
	heads    map[string]string // head spec id -> label
	// manual holds the server's manual member specs by id, read on first
	// need.
	manual map[string]routing.RPCMemberSpec
}

// label is the <head> part of id's name; ok is false when id is no member
// the store knows.
func (r *memberNamer) label(ctx context.Context, id string) (string, bool, error) {
	if head, ok := r.headOf[id]; ok {
		label, err := r.headLabel(ctx, head)
		return label, true, err
	}
	// An automatic member whose membership row is gone still names its head.
	if rest, ok := strings.CutPrefix(id, automaticMemberSpecPrefix); ok {
		if head, ok := strings.CutSuffix(rest, "_"+r.serverID); ok {
			label, err := r.headLabel(ctx, head)
			return label, true, err
		}
		return "", false, nil
	}
	spec, ok, err := r.manualSpec(ctx, id)
	if err != nil || !ok {
		return "", false, err
	}
	return path.Base(strings.ReplaceAll(spec.Binary, "\\", "/")), true, nil
}

// manualSpec is the manual member spec id on the server, read with the
// server's other specs on the first call.
func (r *memberNamer) manualSpec(ctx context.Context, id string) (routing.RPCMemberSpec, bool, error) {
	if r.manual == nil {
		specs, err := r.s.routes.RPCMemberSpecsByServer(ctx, r.serverID)
		if err != nil {
			return routing.RPCMemberSpec{}, false, err
		}
		r.manual = make(map[string]routing.RPCMemberSpec, len(specs))
		for _, spec := range specs {
			if spec.ServerID == r.serverID {
				r.manual[spec.ID] = spec
			}
		}
	}
	spec, ok := r.manual[id]
	return spec, ok, nil
}

// headLabel is the gateway model name of the head spec headSpecID, or
// rpcMemberHiddenHead when the reader may not see its mapping or the head
// is gone.
func (r *memberNamer) headLabel(ctx context.Context, headSpecID string) (string, error) {
	if label, ok := r.heads[headSpecID]; ok {
		return label, nil
	}
	label, err := r.readHeadLabel(ctx, headSpecID)
	if err != nil {
		return "", err
	}
	r.heads[headSpecID] = label
	return label, nil
}

// readHeadLabel reads headLabel's answer. The spec and the mapping are read
// first so that a store error fails the call instead of hiding the head.
func (r *memberNamer) readHeadLabel(ctx context.Context, headSpecID string) (string, error) {
	spec, ok, err := r.s.routes.RuntimeSpecByID(ctx, headSpecID)
	if err != nil || !ok {
		return rpcMemberHiddenHead, err
	}
	if _, err := r.s.routes.MappingByID(ctx, spec.MappingID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return rpcMemberHiddenHead, nil
		}
		return "", err
	}
	// Beyond the reader's rights the head stays hidden: not an error.
	mapping, _, _, err := r.s.authorizeMapping(ctx, r.p, spec.MappingID)
	if err != nil {
		return rpcMemberHiddenHead, nil
	}
	return mapping.GatewayModelName, nil
}
