// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"op-ai-gateway/internal/routing"
	"strings"
)

// runtimeEnsurePath is the agent router's ensure route (ADR-046): it starts
// the managed child that serves {model} and answers once the child is running,
// without forwarding any request to it.
const runtimeEnsurePath = "/ensure/{model}"

// runtimeEnsureRunning is the status the ensure route answers on success.
const runtimeEnsureRunning = "running"

// runtimeEnsureBodyLimit bounds how much of an ensure answer is read. The
// success body and the router's error envelope are both short.
const runtimeEnsureBodyLimit = 64 << 10

// RuntimeEnsurer is an optional provider capability: it asks an agent router
// to start target.ProviderModel's managed child and returns once the router
// reports it running. No request is forwarded to the child, so it also starts
// a child that serves no chat route, such as sd-server.
//
// The call is one long request that the router holds for the whole start, and
// that start may outlast target.Timeout. So it arms no timeout of its own and
// is bounded by ctx alone: an expired deadline is ErrTimeout, and its cause
// stays in the chain. A non-2xx answer maps like every other request on the
// client (unavailableStatus), and a router error envelope adds its code as a
// *RouterError. A 2xx that does not say "running" is ErrInvalidResponse. A
// 2xx whose body cannot be read is one of three cases, told apart by ctx:
// ErrTimeout when ctx's deadline expired, ErrUnavailable when ctx was
// cancelled (as a cancelled request is), and ErrInvalidResponse only for a
// read that failed while ctx was live, such as a body cut short.
type RuntimeEnsurer interface {
	EnsureRuntimeModel(ctx context.Context, target routing.Target) error
}

var _ RuntimeEnsurer = (*OpenAICompatibleClient)(nil)

// RouterError is a non-2xx answer from an agent router whose body is the
// router's error envelope, {"error":{"code":"runtime.…","message":"…"}}. Code
// is that envelope's code; Err is the provider error its HTTP status maps to,
// so errors.Is(err, ErrUpstreamStarting) and errors.Is(err, ErrAuthRejected)
// still hold through it. It is returned as a pointer: match it with
// errors.As(err, &re) for a re of type *RouterError.
//
// It is produced for an agent router's answers only: the ensure route, a chat
// completion or the start of a stream sent to a server_agent target
// (chatStatusError), and -- through RouterStatusError -- a refusal the gateway
// relays itself from an agent instance k >= 1 and reads for a gone instance.
// Any other upstream's non-2xx answer is the plain provider error its status
// maps to, whatever its body says, so a code another server happens to send in
// the same envelope shape is never read as the router's.
type RouterError struct {
	// Status is the HTTP status the router answered with.
	Status int
	Code   string
	Err    error
}

// Error puts the router code first, then the provider error.
func (e *RouterError) Error() string {
	return e.Code + ": " + e.Err.Error()
}

// Unwrap returns the provider error, which keeps the %w chain intact.
func (e *RouterError) Unwrap() error {
	return e.Err
}

// EnsureRuntimeModel POSTs, with no body, to the agent router's ensure route
// for target.ProviderModel through this client's transport and credential.
// See RuntimeEnsurer.
//
// The request stays bodiless: an agent router without the route hands the
// POST to its proxy, which answers a body that names no model with 404
// runtime.model_not_managed before it starts anything.
//
// A failed read of a 2xx answer is classified by ctx first, the way a failed
// Do is: the deadline (ensureDeadlineError), then a cancellation, and only a
// read that failed with ctx live is an invalid answer.
func (c *OpenAICompatibleClient) EnsureRuntimeModel(ctx context.Context, target routing.Target) error {
	reqURL := endpointURL(target.Endpoint, ExpandModelPath(runtimeEnsurePath, target.ProviderModel))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, http.NoBody)
	if err != nil {
		return fmt.Errorf("%w: create request: %v", ErrUnavailable, err)
	}
	applyUpstreamAuth(ctx, req)
	resp, err := c.http.Do(req)
	if err != nil {
		if timeout := ensureDeadlineError(ctx); timeout != nil {
			return timeout
		}
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, runtimeEnsureBodyLimit))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return RouterStatusError(resp.StatusCode, body)
	}
	if err != nil {
		if timeout := ensureDeadlineError(ctx); timeout != nil {
			return timeout
		}
		if errors.Is(ctx.Err(), context.Canceled) {
			return fmt.Errorf("%w: read ensure answer: %v", ErrUnavailable, err)
		}
		return fmt.Errorf("%w: read ensure answer: %v", ErrInvalidResponse, err)
	}
	return ensureRunningAnswer(resp.StatusCode, body)
}

// ensureDeadlineError is the ensure call's ErrTimeout once ctx's deadline has
// expired, with the deadline's cause wrapped so a caller can tell whose bound
// ran out, and nil otherwise. A cancellation is not a timeout.
func ensureDeadlineError(ctx context.Context) error {
	if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return nil
	}
	return fmt.Errorf("%w: ensure route: %w", ErrTimeout, context.Cause(ctx))
}

// RouterStatusError maps a non-2xx answer through unavailableStatus and, when
// body is the agent router's error envelope with a non-empty string code,
// wraps it in a *RouterError. The envelope's message is appended only when it
// says more than the code: for the router's start failures it is the code
// itself. Every client call to an agent router reports a refusal through it --
// the ensure route, a chat completion, the start of a stream -- and the
// gateway uses it for an agent's answer it relays itself, so a refusal reads
// the same whichever way it arrived. It is for agent routers' answers only (see
// RouterError).
func RouterStatusError(status int, body []byte) error {
	base := unavailableStatus(status)
	var env struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &env) != nil {
		return base
	}
	code := strings.TrimSpace(env.Error.Code)
	if code == "" {
		return base
	}
	if msg := strings.TrimSpace(env.Error.Message); msg != "" && msg != code {
		base = fmt.Errorf("%w: %s", base, msg)
	}
	return &RouterError{Status: status, Code: code, Err: base}
}

// routerErrorBodyLimit bounds how much of a non-2xx chat answer is read for
// the router's error envelope, which is a single short line.
const routerErrorBodyLimit = 4 << 10

// chatStatusError is the error of a non-2xx chat answer from target, whose body
// the caller has not read. An agent router's refusal keeps its code: at most
// routerErrorBodyLimit of the body is read for RouterStatusError, so the gateway
// can tell an instance that is gone from any other failure. Every other
// upstream's answer maps through unavailableStatus alone, its body unread, as it
// always has.
func chatStatusError(target routing.Target, resp *http.Response) error {
	if target.Provider != routing.ProviderServerAgent {
		return unavailableStatus(resp.StatusCode)
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, routerErrorBodyLimit))
	return RouterStatusError(resp.StatusCode, body)
}

// ensureRunningAnswer accepts a 2xx ensure answer only when its JSON body says
// status "running". Nothing else proves the child is up.
func ensureRunningAnswer(status int, body []byte) error {
	var answer struct {
		Status string `json:"status"`
	}
	if json.Unmarshal(body, &answer) != nil || answer.Status != runtimeEnsureRunning {
		return fmt.Errorf("%w: ensure route answered %d without status %q", ErrInvalidResponse, status, runtimeEnsureRunning)
	}
	return nil
}
