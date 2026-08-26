// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

// This file is the agent-side client for GET /api/agent/v1/features
// (task-7-report.md): the gateway's declared feature-name set, used to
// compute internal/agent.ActiveFeatures. It follows the exact same
// ETag-conditional-GET discipline as GatewaySource (config_client.go):
// a transient failure never looks like "the gateway declares nothing" to a
// caller that only checks the error.
package runtime

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"op-ai-server-agent/internal/gwapi"
	"sync"
	"time"
)

// featuresPath is the gateway route this client polls.
const featuresPath = "/api/agent/v1/features"

// FeaturesClient fetches the gateway's declared feature-name set with
// ETag-based conditional GETs. Unlike GatewaySource, there is no disk cache:
// the feature set is purely informational (it only gates which of this
// agent's OWN Features are active) and needs no cold-start-before-first-
// contact story.
type FeaturesClient struct {
	ep *gwapi.Endpoint

	mu   sync.Mutex
	etag string   // the gateway response's ETag header, tracked for If-None-Match
	last []string // last known-good feature set
}

// NewFeaturesClient builds a FeaturesClient. gatewayURL is joined with
// featuresPath by plain string concatenation (via gwapi.Endpoint) --
// deliberately NOT url.JoinPath -- matching every other agent->gateway
// client in this module. A nil client gets a 30s-timeout default, matching
// GatewaySource/RoutesClient/Installer.
func NewFeaturesClient(gatewayURL, token string, client *http.Client) *FeaturesClient {
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	return &FeaturesClient{ep: gwapi.NewEndpoint(gatewayURL, token, client)}
}

// featuresResponse mirrors the gateway's agentFeaturesDTO
// ({"features":[...]}) -- no in-body etag field (task-7-report.md): the
// etag lives ONLY in the ETag response header for this endpoint.
type featuresResponse struct {
	Features []string `json:"features"`
}

// Fetch returns the gateway's currently declared feature set. Discipline,
// matching GatewaySource.Load exactly:
//   - a transport error, an unparseable body, or an unexpected non-2xx/304/404
//     status returns the LAST KNOWN-GOOD set with a nil error (a transient
//     gateway hiccup must never look like "the gateway declares no
//     features" to a caller that only checks the error);
//   - 304 Not Modified returns the cached set, nil error;
//   - 404 returns an EMPTY set with a nil error -- an older gateway that
//     predates this endpoint is not a failure, it is exactly the legacy
//     agent behavior (internal/agent.ActiveFeatures already treats a nil/
//     empty gateway set as "no active features").
func (c *FeaturesClient) Fetch(ctx context.Context) ([]string, error) {
	c.mu.Lock()
	knownETag := c.etag
	last := c.last
	c.mu.Unlock()

	resp, err := c.ep.GetConditional(ctx, featuresPath, knownETag)
	if err != nil {
		slog.Debug("runtime: gateway features fetch failed; keeping last known set", "error", err)
		return last, nil
	}
	defer gwapi.DrainLimited(resp)

	switch resp.StatusCode {
	case http.StatusNotModified:
		return last, nil
	case http.StatusNotFound:
		c.mu.Lock()
		c.etag = ""
		c.last = nil
		c.mu.Unlock()
		return nil, nil
	case http.StatusOK:
		// handled below
	default:
		slog.Debug("runtime: gateway features fetch returned unexpected status; keeping last known set", "status", resp.StatusCode)
		return last, nil
	}

	raw, err := io.ReadAll(gwapi.LimitReader(resp))
	if err != nil {
		slog.Debug("runtime: gateway features response read failed; keeping last known set", "error", err)
		return last, nil
	}
	var body featuresResponse
	if err := json.Unmarshal(raw, &body); err != nil {
		slog.Debug("runtime: gateway features response parse failed; keeping last known set", "error", err)
		return last, nil
	}
	if body.Features == nil {
		body.Features = []string{}
	}

	c.mu.Lock()
	c.etag = gwapi.ResponseETag(resp, "")
	c.last = body.Features
	c.mu.Unlock()
	return body.Features, nil
}
