// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package provider

import (
	"context"
	"net/http"
	"strings"
)

type upstreamAuthKey struct{}

// UpstreamAuth is the per-request upstream credential + static headers the gateway attaches to a
// call. Header is an optional custom credential-header name; empty ⇒ "Authorization: Bearer <Token>".
// ExtraHeaders are static, non-secret headers (e.g. a vendor subscription's anthropic-version /
// anthropic-beta) attached verbatim alongside the credential.
type UpstreamAuth struct {
	Header       string
	Token        string
	ExtraHeaders map[string]string
}

// WithUpstreamAuth returns ctx carrying the upstream credential. An empty token leaves ctx
// unchanged, so an unauthenticated app threads nothing and providers no-op.
func WithUpstreamAuth(ctx context.Context, header, token string) context.Context {
	return WithUpstreamAuthHeaders(ctx, header, token, nil)
}

// WithUpstreamAuthHeaders returns ctx carrying the upstream credential plus static extra headers.
// It stores a value when there is anything to attach — a non-empty token OR at least one extra
// header — so a target that needs only the extra headers (a subscription target whose OAuth bearer
// is attached via an empty-header token, or a header-only target) still threads them. An empty token
// with no extra headers leaves ctx unchanged (an unauthenticated app threads nothing).
func WithUpstreamAuthHeaders(ctx context.Context, header, token string, extra map[string]string) context.Context {
	if strings.TrimSpace(token) == "" && len(extra) == 0 {
		return ctx
	}
	return context.WithValue(ctx, upstreamAuthKey{}, UpstreamAuth{Header: strings.TrimSpace(header), Token: token, ExtraHeaders: extra})
}

// UpstreamAuthFrom returns the credential carried by ctx (via WithUpstreamAuth), ok=false when none.
// ok is true when the carried value has a non-empty token OR at least one extra header to attach.
func UpstreamAuthFrom(ctx context.Context) (UpstreamAuth, bool) {
	a, ok := ctx.Value(upstreamAuthKey{}).(UpstreamAuth)
	return a, ok && (strings.TrimSpace(a.Token) != "" || len(a.ExtraHeaders) > 0)
}

// applyUpstreamAuth sets the upstream credential header on req when ctx carries one, plus any static
// extra headers. The default credential is "Authorization: Bearer <token>"; a custom header sends the
// raw token value. The extra headers are set FIRST so the credential header is never shadowed by an
// ExtraHeaders entry of the same name. NEVER logs the token.
func applyUpstreamAuth(ctx context.Context, req *http.Request) {
	a, ok := UpstreamAuthFrom(ctx)
	if !ok {
		return
	}
	for name, value := range a.ExtraHeaders {
		if name != "" {
			req.Header.Set(name, value)
		}
	}
	if strings.TrimSpace(a.Token) == "" {
		return // extra-headers-only target: no credential to attach
	}
	if a.Header == "" {
		req.Header.Set("Authorization", "Bearer "+a.Token)
		return
	}
	req.Header.Set(a.Header, a.Token)
}
