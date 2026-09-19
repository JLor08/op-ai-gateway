// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"crypto/subtle"
	"net/http"
	"op-ai-gateway/internal/apierror"
	"op-ai-gateway/internal/auth"
)

// authenticateInternalOrBearer resolves a principal from the internal
// trusted-loopback header pair, else from a bearer token. It is authenticateWeb
// MINUS the session-cookie leg, and that omission is the point.
//
// Used by /v1/images/generations, which the portal-chat run executor reaches
// over the loopback pair (chat_runs.go sets internalAuthHeaderName +
// internalUserHeaderName and no bearer) while no browser ever calls it
// directly. Swapping the endpoint to requireWebAnyScope would have been one
// line and would ALSO have admitted the browser session cookie, making the
// endpoint reachable from a logged-in page -- a browser leg this feature
// never wanted and docs/architecture/02-constraints.md's auth-legs bullet
// deliberately does not grant it. See requireAnyScope (server.go) for the
// bearer-only original and requireWebAnyScope (auth.go) for the full web
// ladder.
//
// Order matters: the loopback branch runs FIRST because s.authenticate writes a
// 401 before returning false. Fail-closed on each of its three conditions --
// an absent configured secret, a nil user lookup, or any lookup error -- by
// falling through WITHOUT writing a response.
func (s *Server) authenticateInternalOrBearer(w http.ResponseWriter, r *http.Request) (auth.Token, bool) {
	if s.internalAuthSecret != "" && s.users != nil {
		if presented := r.Header.Get(internalAuthHeaderName); presented != "" &&
			subtle.ConstantTimeCompare([]byte(presented), []byte(s.internalAuthSecret)) == 1 {
			if user, err := s.users.UserByID(r.Context(), r.Header.Get(internalUserHeaderName)); err == nil {
				// Never elevated: a background/internal run is not an
				// interactive session that went through the System-Admin
				// step-up, and the request carried no cookie to resolve
				// elevation from.
				return sessionPrincipal(user, false), true
			}
		}
	}
	return s.authenticate(w, r)
}

// requireInternalOrBearerAnyScope resolves a principal via
// authenticateInternalOrBearer and requires at least one of the given scopes.
// The scope check is identical to requireAnyScope's and requireWebAnyScope's;
// only the authentication legs differ.
func (s *Server) requireInternalOrBearerAnyScope(w http.ResponseWriter, r *http.Request, scopes ...string) (auth.Token, bool) {
	token, ok := s.authenticateInternalOrBearer(w, r)
	if !ok {
		return auth.Token{}, false
	}
	if !hasAnyScope(token, scopes) {
		writeJSON(w, http.StatusForbidden, apierror.Response("auth.insufficient_scope", "insufficient scope", ""))
		return auth.Token{}, false
	}
	return token, true
}
