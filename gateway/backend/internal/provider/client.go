// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package provider

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"op-ai-gateway/internal/inference"
	"op-ai-gateway/internal/routing"
	"strconv"
	"strings"
	"time"
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

// ErrAuthRejected is the subset of ErrUnavailable meaning the upstream
// REFUSED the request's credential: 401 (Unauthorized) or 403 (Forbidden).
// It unwraps to ErrUnavailable, so every existing errors.Is(err,
// ErrUnavailable) check is unaffected -- the same wrapping contract as
// ErrUpstreamStarting above. The app-health probe pass uses it to tell
// "the runtime spec's API token is wrong or missing" (an operator
// misconfiguration worth a distinct log line, issue #58) apart from "the
// upstream is down" -- string-matching the status out of the error text
// would be fragile. Build the wrapped error with unavailableStatus so
// 401/403 get this tag.
var ErrAuthRejected = fmt.Errorf("%w (upstream rejected the credential)", ErrUnavailable)

// unavailableStatus wraps a non-2xx upstream status as ErrUnavailable,
// tagging a 503 additionally as ErrUpstreamStarting (a retryable "still
// loading" signal) and a 401/403 as ErrAuthRejected (a credential the
// upstream refused).
func unavailableStatus(status int) error {
	base := ErrUnavailable
	switch status {
	case http.StatusServiceUnavailable:
		base = ErrUpstreamStarting
	case http.StatusUnauthorized, http.StatusForbidden:
		base = ErrAuthRejected
	}
	return fmt.Errorf("%w: upstream status %d", base, status)
}

// LimitError is a vendor's limit answer: HTTP 429 (a rate or usage limit) or 402
// (out of credit), decided by status alone -- OpenAI's insufficient_quota and
// Codex's usage_limit_reached arrive as 429, so no body is read. It carries what a
// vendor-group failover needs: the status, the delay the vendor asked for in
// Retry-After (0 when it sent none), and a copy of the response headers, whose
// rate-limit windows the gateway scrapes. It unwraps to ErrUnavailable and reads
// exactly like unavailableStatus's error, so every errors.Is(err, ErrUnavailable)
// check, every log line and the client mapping (502 provider.unavailable) are
// unchanged. Build it with vendorStatusError.
type LimitError struct {
	Status     int
	RetryAfter time.Duration
	Header     http.Header
}

func (e *LimitError) Error() string {
	return fmt.Sprintf("%v: upstream status %d", ErrUnavailable, e.Status)
}

func (e *LimitError) Unwrap() error { return ErrUnavailable }

// IsLimitStatus reports whether an upstream status is a vendor limit answer (429
// or 402). 5xx and 529 (overloaded) are not.
func IsLimitStatus(status int) bool {
	return status == http.StatusTooManyRequests || status == http.StatusPaymentRequired
}

// maxRetryAfterSeconds keeps a Retry-After delay inside time.Duration.
const maxRetryAfterSeconds = math.MaxInt64 / int64(time.Second)

// ParseRetryAfter reads h's Retry-After -- delay-seconds or an HTTP-date -- as the
// delay from now. It is 0 when the header is absent, unparsable, or not in the
// future.
func ParseRetryAfter(h http.Header, now time.Time) time.Duration {
	v := strings.TrimSpace(h.Get("Retry-After"))
	if v == "" {
		return 0
	}
	if secs, err := strconv.ParseInt(v, 10, 64); err == nil {
		if secs <= 0 {
			return 0
		}
		return time.Duration(min(secs, maxRetryAfterSeconds)) * time.Second
	}
	if at, err := http.ParseTime(v); err == nil && at.After(now) {
		return at.Sub(now)
	}
	return 0
}

// vendorStatusError maps a vendor client's non-2xx answer: a limit status is a
// *LimitError (with Retry-After parsed and the headers cloned), every other status
// unavailableStatus's error.
func vendorStatusError(resp *http.Response) error {
	if IsLimitStatus(resp.StatusCode) {
		return &LimitError{Status: resp.StatusCode, RetryAfter: ParseRetryAfter(resp.Header, time.Now()), Header: resp.Header.Clone()}
	}
	return unavailableStatus(resp.StatusCode)
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
