// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"op-ai-gateway/internal/inference"
	"op-ai-gateway/internal/routing"
	"strings"
)

var (
	ErrUnavailable     = errors.New("provider.unavailable")
	ErrTimeout         = errors.New("provider.timeout")
	ErrInvalidResponse = errors.New("provider.invalid_response")
)

// ErrUpstreamStarting is the ONE subset of ErrUnavailable a caller may safely
// retry: the upstream answered 503 (Service Unavailable), which for a model
// server means "still starting / loading", not a permanent failure. It unwraps
// to ErrUnavailable, so every existing errors.Is(err, ErrUnavailable) check is
// unaffected. Every other non-2xx status, a connection error, and a cancelled
// request stay a plain ErrUnavailable and MUST NOT be retried blindly (a 404 is
// a bad model name, a refused connection is a crashed/absent server -- retrying
// either just wastes time, and re-driving a load that OOM-crashed would loop the
// crash). Build the wrapped error with unavailableStatus so 503 gets this tag.
var ErrUpstreamStarting = fmt.Errorf("%w (upstream starting)", ErrUnavailable)

// unavailableStatus wraps a non-2xx upstream status as ErrUnavailable, tagging a
// 503 additionally as ErrUpstreamStarting (a retryable "still loading" signal).
func unavailableStatus(status int) error {
	base := ErrUnavailable
	if status == http.StatusServiceUnavailable {
		base = ErrUpstreamStarting
	}
	return fmt.Errorf("%w: upstream status %d", base, status)
}

// contentTypeHeader and jsonContentType are the request header name/value the
// adapters below set on every upstream JSON request (ollama.go,
// openai_compatible.go, proxy.go).
const (
	contentTypeHeader = "Content-Type"
	jsonContentType   = "application/json"
)

type Client interface {
	Complete(ctx context.Context, target routing.Target, req inference.Request) (Response, error)
}

// StreamEmit receives streaming events; returning an error aborts the stream
// (e.g. the client disconnected).
type StreamEmit func(inference.StreamEvent) error

// StreamingClient is the optional streaming capability. Adapters that support
// server-sent streaming implement it in addition to Client.
type StreamingClient interface {
	CompleteStream(ctx context.Context, target routing.Target, req inference.Request, emit StreamEmit) error
}

type ModelLister interface {
	ListModels(ctx context.Context, target routing.Target) ([]string, error)
}

// Prober is the optional reachability capability. An adapter implementing it
// reports whether an application is reachable via a health probe: a nil return
// means reachable/healthy, a non-nil error means unreachable. The app-health
// loop uses it to derive per-server health from per-application reachability.
type Prober interface {
	Probe(ctx context.Context, target routing.Target, path string) error
}

func providerModel(target routing.Target, req inference.Request) string {
	if target.ProviderModel != "" {
		return target.ProviderModel
	}
	return req.Model
}

func endpointURL(endpoint string, path string) string {
	return strings.TrimRight(endpoint, "/") + path
}

// httpProbe performs a GET on endpointURL(target.Endpoint, path) with the
// adapter's http client, honoring target.Timeout. A 2xx response is reachable
// (nil); any other status or transport error is unreachable, and the returned
// error includes the status code when one was received. Shared by the
// OpenAI-compatible and Ollama adapters.
func httpProbe(ctx context.Context, httpClient *http.Client, target routing.Target, path string) error {
	if target.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, target.Timeout)
		defer cancel()
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, endpointURL(target.Endpoint, path), nil)
	if err != nil {
		return fmt.Errorf("%w: create request: %v", ErrUnavailable, err)
	}
	applyUpstreamAuth(ctx, httpReq)
	httpResp, err := httpClient.Do(httpReq)
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return ErrTimeout
		}
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer httpResp.Body.Close()
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		return unavailableStatus(httpResp.StatusCode)
	}
	return nil
}
