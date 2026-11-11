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
	"net/url"
	"op-ai-gateway/internal/routing"
	"strconv"
	"time"
)

// The agent router's member routes (agent 0.18.0, feature rpc_cluster): an
// rpc_server member has no upstream model, so it is addressed by its spec id
// under /rpc/{action}/{specID}.
const rpcRoutePrefix = "/rpc/"

// The states the member routes answer on success.
const (
	rpcMemberRunning  = "running"
	rpcMemberReleased = "released"
)

// RPCMemberController is an optional provider capability: the agent router's
// member routes, which the gateway's cluster orchestration drives. target
// addresses the member's agent router (its endpoint and credential); the
// member itself is specID, the agent spec id of the rpc_server spec.
//
//   - EnsureRPCMember starts the member and returns once it accepts TCP
//     connections, arming a reservation of reserve (rounded up to whole
//     seconds; 0 arms none) that keeps the member from idling out before its
//     head holds it.
//   - ReleaseRPCMember ends the member's reservation and hold: the head is not
//     running.
//   - StopRPCMember stops the member and returns the router's state,
//     RuntimeStopStopping or RuntimeStopStopped. Unless force is set the
//     router refuses a held or reserved member (409
//     runtime.cluster_member_in_use); force also ends the claim.
//
// Errors classify as EnsureRuntimeModel's do: a non-2xx answer is the provider
// error its status maps to, with the router's code as a *RouterError; an
// expired deadline is ErrTimeout; a 2xx that does not name the expected state
// is ErrInvalidResponse. None arms a timeout of its own.
type RPCMemberController interface {
	EnsureRPCMember(ctx context.Context, target routing.Target, specID string, reserve time.Duration) error
	ReleaseRPCMember(ctx context.Context, target routing.Target, specID string) error
	StopRPCMember(ctx context.Context, target routing.Target, specID string, force bool) (string, error)
}

var _ RPCMemberController = (*OpenAICompatibleClient)(nil)

// EnsureRPCMember POSTs to /rpc/ensure/{specID}?reserve_seconds=N. See
// RPCMemberController.
func (c *OpenAICompatibleClient) EnsureRPCMember(ctx context.Context, target routing.Target, specID string, reserve time.Duration) error {
	query := url.Values{}
	if reserve > 0 {
		secs := int64((reserve + time.Second - 1) / time.Second)
		query.Set("reserve_seconds", strconv.FormatInt(secs, 10))
	}
	state, err := c.postRPC(ctx, target, "ensure", specID, query)
	if err != nil {
		return err
	}
	if state != rpcMemberRunning {
		return fmt.Errorf("%w: member ensure answered state %q, want %q", ErrInvalidResponse, state, rpcMemberRunning)
	}
	return nil
}

// ReleaseRPCMember POSTs to /rpc/release/{specID}. See RPCMemberController.
func (c *OpenAICompatibleClient) ReleaseRPCMember(ctx context.Context, target routing.Target, specID string) error {
	state, err := c.postRPC(ctx, target, "release", specID, nil)
	if err != nil {
		return err
	}
	if state != rpcMemberReleased {
		return fmt.Errorf("%w: member release answered state %q, want %q", ErrInvalidResponse, state, rpcMemberReleased)
	}
	return nil
}

// StopRPCMember POSTs to /rpc/stop/{specID}, with force=1 when force is set.
// See RPCMemberController.
func (c *OpenAICompatibleClient) StopRPCMember(ctx context.Context, target routing.Target, specID string, force bool) (string, error) {
	var query url.Values
	if force {
		query = url.Values{"force": {"1"}}
	}
	state, err := c.postRPC(ctx, target, "stop", specID, query)
	if err != nil {
		return "", err
	}
	if state != RuntimeStopStopping && state != RuntimeStopStopped {
		return "", fmt.Errorf("%w: member stop answered state %q", ErrInvalidResponse, state)
	}
	return state, nil
}

// postRPC sends one bodiless POST to the member route action for specID and
// returns the state its 2xx answer names. It classifies failures like the
// ensure call (ensureDeadlineError, RouterStatusError).
func (c *OpenAICompatibleClient) postRPC(ctx context.Context, target routing.Target, action, specID string, query url.Values) (string, error) {
	reqURL := endpointURL(target.Endpoint, rpcRoutePrefix+action+"/"+url.PathEscape(specID))
	if len(query) > 0 {
		reqURL += "?" + query.Encode()
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
			return "", fmt.Errorf("%w: read member answer: %v", ErrUnavailable, err)
		}
		return "", fmt.Errorf("%w: read member answer: %v", ErrInvalidResponse, err)
	}
	var answer struct {
		State string `json:"state"`
	}
	if json.Unmarshal(body, &answer) != nil {
		return "", fmt.Errorf("%w: member route answered %d without a state", ErrInvalidResponse, resp.StatusCode)
	}
	return answer.State, nil
}
