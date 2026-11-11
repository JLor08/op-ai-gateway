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
)

// runtimeStopPath is the agent router's stop route (agent 0.17.0, feature
// runtime_stop): it begins a graceful drain of the one instance {model}
// addresses and records nothing persistent, so demand may start it again.
const runtimeStopPath = "/stop/{model}"

// The two states the stop route answers on success.
const (
	// RuntimeStopStopping: a drain began, or was already running.
	RuntimeStopStopping = "stopping"
	// RuntimeStopStopped: the instance has no process, so there was nothing
	// to stop.
	RuntimeStopStopped = "stopped"
)

// ifEvictableKey is the context key of WithIfEvictable.
type ifEvictableKey struct{}

// WithIfEvictable marks ctx so that StopRuntimeModel asks for the conditional
// stop (POST /stop/{model}?if_evictable=1): the agent stops the instance only
// when its own eviction rule would let it go right now -- not pinned or kept
// up, not starting, nothing in flight, outside the eviction grace -- and
// answers 409 runtime.instance_busy otherwise. The gateway's cluster
// orchestration uses it to remove a cluster head that a local start on a
// member's server waits for.
func WithIfEvictable(ctx context.Context) context.Context {
	return context.WithValue(ctx, ifEvictableKey{}, true)
}

// IsIfEvictable reports whether ctx carries WithIfEvictable's mark.
func IsIfEvictable(ctx context.Context) bool {
	on, _ := ctx.Value(ifEvictableKey{}).(bool)
	return on
}

// RuntimeStopper is an optional provider capability: it asks an agent router
// to stop one instance of a managed spec (target.ProviderModel is its address,
// m or m#k) and returns the state the router answered, RuntimeStopStopping or
// RuntimeStopStopped. It is transient: the router changes nothing in its
// configuration, so the instance's normal idle, eviction and demand rules
// apply afterwards.
//
// Errors classify as EnsureRuntimeModel's do: a non-2xx answer is the provider
// error its status maps to, and a router error envelope adds its code as a
// *RouterError (runtime.instance_protected, runtime.instance_not_highest,
// runtime.model_not_managed, and for a conditional stop (WithIfEvictable)
// runtime.instance_busy); an expired deadline is ErrTimeout; a 2xx that names
// neither state is ErrInvalidResponse. It arms no timeout of its own.
type RuntimeStopper interface {
	StopRuntimeModel(ctx context.Context, target routing.Target) (string, error)
}

var _ RuntimeStopper = (*OpenAICompatibleClient)(nil)

// StopRuntimeModel POSTs, with no body, to the agent router's stop route for
// target.ProviderModel through this client's transport and credential. See
// RuntimeStopper. Like the ensure call it stays bodiless: an older router
// without the route hands the POST to its proxy, which answers a body that
// names no model with 404 runtime.model_not_managed and stops nothing.
func (c *OpenAICompatibleClient) StopRuntimeModel(ctx context.Context, target routing.Target) (string, error) {
	reqURL := endpointURL(target.Endpoint, ExpandModelPath(runtimeStopPath, target.ProviderModel))
	if IsIfEvictable(ctx) {
		reqURL += "?if_evictable=1"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, http.NoBody)
	if err != nil {
		return "", fmt.Errorf("%w: create request: %v", ErrUnavailable, err)
	}
	applyUpstreamAuth(ctx, req)
	resp, err := c.http.Do(req)
	if err != nil {
		if timeout := ensureDeadlineError(ctx); timeout != nil {
			return "", timeout
		}
		return "", fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, runtimeEnsureBodyLimit))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", RouterStatusError(resp.StatusCode, body)
	}
	if err != nil {
		if timeout := ensureDeadlineError(ctx); timeout != nil {
			return "", timeout
		}
		if errors.Is(ctx.Err(), context.Canceled) {
			return "", fmt.Errorf("%w: read stop answer: %v", ErrUnavailable, err)
		}
		return "", fmt.Errorf("%w: read stop answer: %v", ErrInvalidResponse, err)
	}
	var answer struct {
		State string `json:"state"`
	}
	if json.Unmarshal(body, &answer) != nil || (answer.State != RuntimeStopStopping && answer.State != RuntimeStopStopped) {
		return "", fmt.Errorf("%w: stop route answered %d without state %q or %q", ErrInvalidResponse, resp.StatusCode, RuntimeStopStopping, RuntimeStopStopped)
	}
	return answer.State, nil
}
