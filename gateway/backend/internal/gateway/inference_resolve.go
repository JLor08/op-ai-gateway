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
// The marker is written only when the effective model differs from what the
// token already carries: the token table is already written on every
// authentication, and a second unconditional write per inference request
// would double that load for no gain. A failed resolve records nothing —
// "last used" means last SUCCESSFULLY routed, so a typo or a dead model never
// becomes a token's redirect target.
//
// A write error is logged and swallowed: the marker is a convenience, never a
// reason to fail a request that already has a live target.
func (s *Server) resolveTarget(ctx context.Context, token auth.Token, req inference.Request) (routing.Target, error) {
	target, err := s.Resolver.Resolve(ctx, token, req)
	if err != nil {
		return target, err
	}
	if req.Model != "" && req.Model != token.LastUsedModel && s.LastUsedModelWriter != nil {
		if wErr := s.LastUsedModelWriter(ctx, token.ID, req.Model); wErr != nil {
			slog.Warn("last-used-model write failed", "token_id", token.ID, "model", req.Model, "err", wErr)
		}
	}
	return target, nil
}
