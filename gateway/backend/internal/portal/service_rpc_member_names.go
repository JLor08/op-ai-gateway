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

// rpcMemberHiddenHead is the display name of a member whose head the reader
// may not see: it names nothing, and needs no translation.
const rpcMemberHiddenHead = "RPC (—)"

// RPCMemberDisplayNames names the RPC member processes specIDs that run on
// serverID for the reader p, by their agent spec id: "RPC (<model>)(<head
// server>)", the gateway model name of the head the member serves and the
// name of the server that head runs on. The member's own server is the one
// the reader is looking at, so the name says what it serves instead. It is
// "RPC (—)" for a head p may not see (its mapping is beyond p's rights) and
// for a head whose spec or mapping is gone, so nothing about such a head
// leaks, not even its server. A manual member spec that serves no head shows
// "RPC (<binary's file name>)", and an id that matches neither gets no name.
//
// The names are resolved from the stored rows on every call, so a renamed
// model or head server shows at once. One call reads the server's
// membership once, its manual member specs at most once, and each head once,
// however many ids it names. A store error on those reads fails the call. The rights check on
// a head's mapping (authorizeMapping) answers not-found for its own store
// errors too, so such an error shows that head as "RPC (—)" until the next call.
func (s *Service) RPCMemberDisplayNames(ctx context.Context, p auth.Token, serverID string, specIDs []string) (map[string]string, error) {
	if _, err := s.authorizeServer(ctx, p, serverID); err != nil {
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
		name, ok, err := r.name(ctx, id)
		if err != nil {
			return nil, err
		}
		if ok {
			out[id] = name
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
	heads    map[string]string // head spec id -> display name
	// manual holds the server's manual member specs by id, read on first
	// need.
	manual map[string]routing.RPCMemberSpec
}

// name is id's display name; ok is false when id is no member the store
// knows.
func (r *memberNamer) name(ctx context.Context, id string) (string, bool, error) {
	if head, ok := r.headOf[id]; ok {
		name, err := r.headName(ctx, head)
		return name, true, err
	}
	// An automatic member whose membership row is gone still names its head.
	if rest, ok := strings.CutPrefix(id, automaticMemberSpecPrefix); ok {
		if head, ok := strings.CutSuffix(rest, "_"+r.serverID); ok {
			name, err := r.headName(ctx, head)
			return name, true, err
		}
		return "", false, nil
	}
	spec, ok, err := r.manualSpec(ctx, id)
	if err != nil || !ok {
		return "", false, err
	}
	return "RPC (" + path.Base(strings.ReplaceAll(spec.Binary, "\\", "/")) + ")", true, nil
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

// headName is the display name of a member of the head spec headSpecID, or
// rpcMemberHiddenHead when the reader may not see its mapping or the head is
// gone.
func (r *memberNamer) headName(ctx context.Context, headSpecID string) (string, error) {
	if name, ok := r.heads[headSpecID]; ok {
		return name, nil
	}
	name, err := r.readHeadName(ctx, headSpecID)
	if err != nil {
		return "", err
	}
	r.heads[headSpecID] = name
	return name, nil
}

// readHeadName reads headName's answer. The spec and the mapping are read
// first so that a store error fails the call instead of hiding the head.
func (r *memberNamer) readHeadName(ctx context.Context, headSpecID string) (string, error) {
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
	mapping, _, headServer, err := r.s.authorizeMapping(ctx, r.p, spec.MappingID)
	if err != nil {
		return rpcMemberHiddenHead, nil
	}
	return "RPC (" + mapping.GatewayModelName + ")(" + headServer.Name + ")", nil
}
