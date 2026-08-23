// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package provider

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"op-ai-gateway/internal/routing"
)

// ProxyResponse is the upstream's raw HTTP response for a native-passthrough
// request. The caller owns Body and MUST close it.
type ProxyResponse struct {
	StatusCode int
	Header     http.Header
	Body       io.ReadCloser
}

// NativeProxyClient is the optional native-passthrough capability. An adapter
// implementing it forwards a raw request body to the upstream's own endpoint path
// (e.g. /v1/responses for Codex, /v1/messages for Claude Code) and returns the
// upstream response with its Body still open, so the gateway can stream it back to
// the client byte-for-byte without translating through inference.Request.
type NativeProxyClient interface {
	ProxyNative(ctx context.Context, target routing.Target, path string, body []byte) (*ProxyResponse, error)
}

// doNativeProxy POSTs body to endpointURL(target.Endpoint, path) with the given
// http client and returns the upstream response with Body still open. It does NOT
// apply target.Timeout: the caller owns the deadline policy (an idle watchdog for
// streaming, a total timeout for buffered completions), because a native stream
// can run far longer than a single-request timeout. The CLIENT's bearer token is
// still NEVER forwarded upstream (the gateway already authenticated the caller);
// only Content-Type and — when ctx carries one via WithUpstreamAuth — the per-app
// UPSTREAM credential (a gateway-held token, distinct from the client's) are set.
func doNativeProxy(ctx context.Context, httpClient *http.Client, target routing.Target, path string, body []byte) (*ProxyResponse, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpointURL(target.Endpoint, path), bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("%w: create request: %v", ErrUnavailable, err)
	}
	httpReq.Header.Set(contentTypeHeader, jsonContentType)
	applyUpstreamAuth(ctx, httpReq)
	httpResp, err := httpClient.Do(httpReq)
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, ErrTimeout
		}
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	return &ProxyResponse{StatusCode: httpResp.StatusCode, Header: httpResp.Header, Body: httpResp.Body}, nil
}
