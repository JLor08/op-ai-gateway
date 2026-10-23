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
//
// contentType is the body's media type, sent upstream verbatim as Content-Type
// (an empty one is sent as application/json).
// Every JSON endpoint passes "application/json"; a multipart body (the image-edit
// upload) passes its own "multipart/form-data; boundary=..." so the boundary the
// parts were framed with survives the hop.
type NativeProxyClient interface {
	ProxyNative(ctx context.Context, target routing.Target, path string, body []byte, contentType string) (*ProxyResponse, error)
}

// NativeDuplexProxyClient is the optional full-duplex native-passthrough
// capability: ProxyNativeDuplex POSTs a STREAMING request body to the upstream's
// own endpoint path while the caller reads the still-open response, so a client
// can stream audio up to a live-transcription endpoint and read its SSE transcript
// down on one connection.
//
// It is deliberately a separate interface from NativeProxyClient, not a widening
// of it. NativeProxyClient sends a fully buffered []byte and has many implementers
// (and test doubles) for which a request body that never ends has no meaning; only
// the client that talks to an OpenAI-compatible upstream (a direct audio.cpp
// target, or the server agent's runtime router) streams one. Callers reach it by
// type assertion, as with LiveProgressRejectionMemo, and treat a client without it
// as "duplex not supported".
//
// body is sent chunked (no Content-Length is declared) and read until it returns
// io.EOF; the exchange ends when body does, when the upstream finishes its
// response, or when ctx is cancelled. The caller keeps ownership of body: it is
// never closed here, so the caller must unblock a Read that is still pending when
// the exchange ends early (close the io.PipeReader it passed, or cancel whatever
// feeds it), otherwise that read -- and the goroutine behind it -- outlives the
// call. Like ProxyNative it does NOT apply target.Timeout (the deadline is ctx's)
// and returns the response with Body still open, which the caller MUST close.
//
// An HTTP/1.1 upstream must itself be full-duplex (read the request body while
// writing the response) for the interleave to happen; that is the upstream's
// contract, not something this client can force.
type NativeDuplexProxyClient interface {
	ProxyNativeDuplex(ctx context.Context, target routing.Target, path string, body io.Reader, contentType string) (*ProxyResponse, error)
}

// doNativeProxy POSTs body to endpointURL(target.Endpoint, path) with the given
// http client and returns the upstream response with Body still open. It does NOT
// apply target.Timeout: the caller owns the deadline policy (an idle watchdog for
// streaming, a total timeout for buffered completions), because a native stream
// can run far longer than a single-request timeout. The CLIENT's bearer token is
// still NEVER forwarded upstream (the gateway already authenticated the caller);
// only Content-Type (the caller's contentType, verbatim) and — when ctx carries
// one via WithUpstreamAuth — the per-app UPSTREAM credential (a gateway-held
// token, distinct from the client's) are set.
func doNativeProxy(ctx context.Context, httpClient *http.Client, target routing.Target, path string, body []byte, contentType string) (*ProxyResponse, error) {
	return doNativeProxyWithDefaults(ctx, httpClient, target, path, body, contentType, nil)
}

// doNativeProxyWithDefaults is doNativeProxy plus a set of static default headers
// the adapter itself guarantees on every upstream call (Anthropic's required
// anthropic-version). The defaults are set BEFORE the ctx's upstream auth is
// applied, so a ctx-carried ExtraHeaders entry of the same name still wins — the
// same precedence the Anthropic translate path has — while a ctx that carries
// nothing still sends them. A nil/empty set is exactly doNativeProxy.
//
// An empty contentType is sent as application/json rather than as an empty
// Content-Type header: every caller reaches this through the gateway's proxyNative,
// which already defaults it, so this is no behaviour change today -- it keeps a
// future caller that forgets from sending an upstream a blank media type.
func doNativeProxyWithDefaults(ctx context.Context, httpClient *http.Client, target routing.Target, path string, body []byte, contentType string, defaults map[string]string) (*ProxyResponse, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpointURL(target.Endpoint, path), bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("%w: create request: %v", ErrUnavailable, err)
	}
	return sendNativeProxy(ctx, httpClient, httpReq, contentType, defaults)
}

// doNativeDuplexProxy is doNativeProxy for a STREAMING request body: it POSTs body
// to endpointURL(target.Endpoint, path) chunked (ContentLength -1, so no length is
// declared and the transport reads body until io.EOF) and returns the upstream
// response as soon as its headers arrive, with Body still open and body possibly
// still being read. Everything else is doNativeProxy's contract: no
// target.Timeout, the same Content-Type defaulting, the same per-app upstream
// credential from ctx and never the client's own token, and the same error
// mapping (ErrTimeout for ctx's deadline, ErrUnavailable otherwise).
//
// body is wrapped so the transport never closes it; the caller keeps ownership
// (see NativeDuplexProxyClient). A nil body is an empty one.
func doNativeDuplexProxy(ctx context.Context, httpClient *http.Client, target routing.Target, path string, body io.Reader, contentType string) (*ProxyResponse, error) {
	if body == nil {
		body = http.NoBody
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpointURL(target.Endpoint, path), io.NopCloser(body))
	if err != nil {
		return nil, fmt.Errorf("%w: create request: %v", ErrUnavailable, err)
	}
	// Declare the length unknown (-1) so the transport sends the body chunked
	// instead of sizing it. For a body NewRequest cannot size this is already the
	// outcome, but stating it keeps even a sized reader (a strings.Reader, say)
	// chunked: the body's length is not what bounds this exchange.
	httpReq.ContentLength = -1
	return sendNativeProxy(ctx, httpClient, httpReq, contentType, nil)
}

// sendNativeProxy applies the headers shared by every native passthrough to a
// request the caller built (its body, and whether that body is sized or chunked,
// being the only thing the buffered and duplex variants differ in), sends it, and
// returns the upstream response with Body still open.
func sendNativeProxy(ctx context.Context, httpClient *http.Client, httpReq *http.Request, contentType string, defaults map[string]string) (*ProxyResponse, error) {
	if contentType == "" {
		contentType = jsonContentType
	}
	httpReq.Header.Set(contentTypeHeader, contentType)
	for name, value := range defaults {
		httpReq.Header.Set(name, value)
	}
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
