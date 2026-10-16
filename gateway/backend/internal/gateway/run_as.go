// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"net/http"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/portal"
	"strings"
)

// runAsPrincipal applies the optional run-as header, shared by every inference
// handler (chat, images, images-edit, speech). Only the loopback principal
// (token.ID == "") ever honours it: a portal run executes its turns over the
// trusted loopback and names, via X-OP-Run-As-Token, the token the turn runs as,
// so billing, capture, and the token's server-/model-override rules attribute to
// that token rather than to the bare session principal.
//
// It returns the token the request executes as -- token itself unless a run-as
// token was named and authorized -- and false when it has refused the request and
// written the response. With no Portal it fails closed rather than silently
// proceeding as the bare session: the caller explicitly asked to run as a specific
// token and there is no way to authorize it, matching this package's other
// s.Portal == nil guards (agent_ca.go, agent_certificates.go, ...).
func (s *Server) runAsPrincipal(w http.ResponseWriter, r *http.Request, token auth.Token) (auth.Token, bool) {
	if token.ID != "" {
		return token, true
	}
	runAsID := strings.TrimSpace(r.Header.Get(runAsHeaderName))
	if runAsID == "" {
		return token, true
	}
	if s.Portal == nil {
		writePortalTokenError(w, portal.ErrTokenForbidden)
		return token, false
	}
	runAs, err := s.Portal.AuthorizeRunAsToken(r.Context(), token, runAsID)
	if err != nil {
		writePortalTokenError(w, err)
		return token, false
	}
	return runAs, true
}
