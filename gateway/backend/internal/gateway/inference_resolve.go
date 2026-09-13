// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"log/slog"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/inference"
	"op-ai-gateway/internal/routing"
)

// resolveTarget is the single seam through which every inference path
// resolves a routing target (complete, tryProxyNative, beginStream), so the
// last-used-model marker is recorded in exactly one place rather than at each
// of those three call sites.
//
// The marker is a property of an api_tokens row (the `last_used_model` column),
// so it is written only for a principal that HAS such a row — i.e. one with a
// non-empty token id. A token-less session principal (sessionPrincipal, auth.go,
// leaves ID == "") has no row to address: the portal-chat run executor
// re-authenticates its loopback /v1/chat/completions hop as exactly that
// principal, so writing here would look up an empty token id, match no row, and
// log a "store: not found" once per chat turn against a row that was never owed
// (issue #27). The guard is on the id, NOT the model, on purpose: a POPULATED
// id that no longer resolves — a token deleted or expired between authentication
// and this write — is a real signal and must still be reported.
//
// Beyond that, the marker is written only when the effective model differs from
// what the token already carries, because a second unconditional write per
// inference request would double the token table's write load for no gain. A
// failed resolve records nothing — "last used" means last SUCCESSFULLY routed,
// so a typo or a dead model never becomes a token's redirect target.
//
// A write error is logged and swallowed: the marker is a convenience, never a
// reason to fail a request that already has a live target.
func (s *Server) resolveTarget(ctx context.Context, token auth.Token, req inference.Request) (routing.Target, error) {
	target, err := s.Resolver.Resolve(ctx, token, req)
	if err != nil {
		return target, err
	}
	if token.ID != "" && req.Model != "" && req.Model != token.LastUsedModel && s.LastUsedModelWriter != nil {
		if wErr := s.LastUsedModelWriter(ctx, token.ID, req.Model); wErr != nil {
			slog.Warn("last-used-model write failed", "token_id", token.ID, "user_id", token.UserID, "model", req.Model, "err", wErr)
		}
	}
	return target, nil
}
