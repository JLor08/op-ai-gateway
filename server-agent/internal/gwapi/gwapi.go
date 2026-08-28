// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

// Package gwapi is the shared agent->gateway HTTP transport contract: base-URL
// normalization, bearer authentication, ETag-based conditional GET, and the
// bounded-read/drain-for-reuse discipline every agent-side gateway client
// applies to a response body. It is a leaf package (stdlib-only imports) so
// every client package (internal/client, internal/certinstall, internal/proxy,
// internal/trust) can depend on it without risking an import cycle.
package gwapi

import (
	"context"
	"io"
	"net/http"
	"strings"
)

// MaxResponseBytes bounds how much of a gateway response body any client in
// this module will ever read or drain. A leaf + chain + key + trust bundle,
// or a route list, is at most a few KiB, so this is defense against a
// misbehaving or compromised gateway, not a realistic limit.
const MaxResponseBytes = 1 << 20 // 1 MiB

// MaxWSFrameBytes is the maximum size of ONE agent<->gateway WebSocket frame,
// in either direction, and it is a CONTRACT WITH THE GATEWAY rather than a
// local preference: the gateway installs exactly this number as its inbound
// SetReadLimit (gateway/backend/internal/gateway/agent_stream.go's
// maxAgentFrameBytes), and coder/websocket answers a frame one byte over it by
// failing the read and closing 1009 -- which tears down the ONE connection
// telemetry, the system and runtime reports, the runtime_config push and the
// certificate doorbell all share. Producing an oversized frame is therefore not
// a dropped message; it is an outage.
//
// It lives in this leaf package because it has to be visible to BOTH sides of
// the agent's own split: internal/client installs it as its own read limit
// (symmetry with the gateway's), and internal/runtime sizes its outbound
// runtime_log frames against it (see maxLogBatchBytes there). That the number
// used to exist as two independent literals with no stated relationship is
// exactly how a full log-retention buffer came to marshal into a frame the
// gateway could not read.
//
// The gateway is a separate Go module, so a compiler cannot hold the two ends
// together. What holds them instead: this doc, the counterpart doc on
// maxAgentFrameBytes, and a test on each side that pins the literal and names
// the other (TestLogFrameFitsTheGatewayReadLimit here,
// TestAgentFrameLimitMatchesTheAgentsOwnCap there).
const MaxWSFrameBytes int64 = 1 << 20 // 1 MiB

// TrimBase strips a trailing '/' from a configured gateway base URL. Every
// site normalizes its base exactly once (here, or via NewEndpoint) so a later
// path join by plain concatenation never produces a doubled slash.
func TrimBase(gatewayURL string) string {
	return strings.TrimRight(gatewayURL, "/")
}

// BearerValue is the standard "Authorization: Bearer <token>" header value
// every agent->gateway request carries.
func BearerValue(token string) string {
	return "Bearer " + token
}

// Endpoint composes the base+bearer+conditional-GET contract shared by the
// agent's gateway HTTP clients (certinstall.Installer, proxy.RoutesClient).
// Base is normalized exactly once, in NewEndpoint.
type Endpoint struct {
	Base   string // gatewayURL, trimmed of any trailing '/'
	Token  string
	Client *http.Client
}

// NewEndpoint builds an Endpoint, trimming any trailing '/' from gatewayURL
// once so Get/GetConditional never produce a doubled slash. client must be
// non-nil -- each site resolves its own default timeout (they differ) before
// calling this, so NewEndpoint deliberately does not choose one itself.
func NewEndpoint(gatewayURL, token string, client *http.Client) *Endpoint {
	return &Endpoint{Base: TrimBase(gatewayURL), Token: token, Client: client}
}

// Get issues a bearer-authenticated, unconditional GET against e.Base+path.
// path is joined by plain string concatenation -- deliberately NOT
// url.JoinPath -- so an existing base PATH segment (e.g. a gateway reachable
// at "https://gw/base") is preserved rather than normalized away.
func (e *Endpoint) Get(ctx context.Context, path string) (*http.Response, error) {
	return e.GetConditional(ctx, path, "")
}

// GetConditional issues a bearer-authenticated GET against e.Base+path,
// setting If-None-Match when etag is non-empty (an unconditional fetch
// otherwise). See Get for the path-join contract.
func (e *Endpoint) GetConditional(ctx context.Context, path, etag string) (*http.Response, error) {
	return ConditionalGet(ctx, e.Client, e.Base+path, e.Token, etag)
}

// ConditionalGet is the low-level primitive behind Endpoint.GetConditional:
// it issues a bearer-authenticated GET against a fully-built url, setting
// If-None-Match when etag is non-empty. Exported for a caller whose own
// base+path join cannot go through Endpoint's plain concatenation (e.g.
// trust.Refresher, which composes its URL via net/url so it can also strip
// any query/fragment on the configured base).
func ConditionalGet(ctx context.Context, client *http.Client, url, token, etag string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", BearerValue(token))
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	return client.Do(req)
}

// ResponseETag is the opaque validator to persist for the NEXT fetch's
// If-None-Match: the raw response header, verbatim, exactly as the gateway
// sent it (never reparsed or requoted -- an ETag is opaque by design).
// bodyETag is a fallback for a JSON body's own unquoted "etag" field, used
// only in case some intermediary ever strips the header, and is quoted here
// to match what the header would have looked like; pass "" when the response
// body carries no such fallback field.
func ResponseETag(resp *http.Response, bodyETag string) string {
	if v := strings.TrimSpace(resp.Header.Get("ETag")); v != "" {
		return v
	}
	if bodyETag != "" {
		return `"` + bodyETag + `"`
	}
	return ""
}

// LimitReader bounds resp.Body to MaxResponseBytes, for decoding a JSON
// response defensively against a misbehaving or compromised gateway.
func LimitReader(resp *http.Response) io.Reader {
	return io.LimitReader(resp.Body, MaxResponseBytes)
}

// Drain discards the remainder of resp.Body (unbounded) so the underlying
// connection can be reused, then closes it. Call directly (or via defer)
// immediately after a successful client.Do whose caller does not itself
// bound-read the body (e.g. a POST whose response carries no meaningful
// payload).
func Drain(resp *http.Response) {
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
}

// DrainLimited discards up to MaxResponseBytes of resp.Body -- bounded
// defense against a misbehaving or compromised gateway -- so the underlying
// connection can be reused, then closes it. Call directly (or via defer)
// immediately after a successful client.Do whose body the caller has already
// (fully or partially) consumed via LimitReader.
func DrainLimited(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, MaxResponseBytes))
	resp.Body.Close()
}
