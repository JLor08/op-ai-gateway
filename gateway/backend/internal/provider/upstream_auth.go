// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package provider

import (
	"context"
	"net/http"
	"strings"
)

type upstreamAuthKey struct{}

// UpstreamAuth is the per-request upstream credential the gateway attaches to a call. Header is an
// optional custom header name; empty ⇒ "Authorization: Bearer <Token>".
type UpstreamAuth struct {
	Header string
	Token  string
}

// WithUpstreamAuth returns ctx carrying the upstream credential. An empty token leaves ctx
// unchanged, so an unauthenticated app threads nothing and providers no-op.
func WithUpstreamAuth(ctx context.Context, header, token string) context.Context {
	if strings.TrimSpace(token) == "" {
		return ctx
	}
	return context.WithValue(ctx, upstreamAuthKey{}, UpstreamAuth{Header: strings.TrimSpace(header), Token: token})
}

// UpstreamAuthFrom returns the credential carried by ctx (via WithUpstreamAuth), ok=false when none.
func UpstreamAuthFrom(ctx context.Context) (UpstreamAuth, bool) {
	a, ok := ctx.Value(upstreamAuthKey{}).(UpstreamAuth)
	return a, ok && a.Token != ""
}

// applyUpstreamAuth sets the upstream credential header on req when ctx carries one. Default is
// "Authorization: Bearer <token>"; a custom header sends the raw token value. NEVER logs the token.
func applyUpstreamAuth(ctx context.Context, req *http.Request) {
	a, ok := UpstreamAuthFrom(ctx)
	if !ok {
		return
	}
	if a.Header == "" {
		req.Header.Set("Authorization", "Bearer "+a.Token)
		return
	}
	req.Header.Set(a.Header, a.Token)
}
