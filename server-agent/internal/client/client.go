// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

// Package client POSTs telemetry samples to the gateway's agent endpoint with
// bearer authentication and bounded retry/backoff.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"op-ai-server-agent/internal/gwapi"
	"op-ai-server-agent/internal/sample"
	"time"
)

// telemetryPath is the gateway ingest route the agent POSTs to.
const telemetryPath = "/api/agent/v1/telemetry"

// systemReportPath is the gateway's static hardware-inventory ingest route.
const systemReportPath = "/api/agent/v1/system-report"

// maxAttempts bounds the number of POST tries per Post call.
const maxAttempts = 3

// backoffBase is the base retry delay (delay = backoffBase * 2^attempt). It is a
// package-level var so tests can lower it to keep the retry path fast.
var backoffBase = 200 * time.Millisecond

// Client POSTs telemetry samples to the gateway with bearer auth + retry/backoff.
type Client struct {
	gatewayURL string
	token      string
	http       *http.Client
}

// New builds a Client around the injected gateway HTTP client. A nil client is
// the secure legacy default used by unit tests; production injects the shared
// dynamic trust-store client with its 10-second timeout.
func New(gatewayURL, token string, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 10 * time.Second}
	}
	return &Client{
		gatewayURL: gwapi.TrimBase(gatewayURL),
		token:      token,
		http:       httpClient,
	}
}

// statusError is a non-2xx response error carrying the HTTP status code. It never
// includes the request body or token so it is safe to log.
type statusError struct {
	code int
}

func (e *statusError) Error() string {
	return fmt.Sprintf("gateway returned HTTP %d", e.code)
}

// Post normalizes s, marshals it, and POSTs it to the gateway. It retries up to
// maxAttempts times on transport errors or 5xx responses with exponential
// backoff (respecting ctx); a 2xx returns nil and a 4xx returns a non-retryable
// error including the status. The token is never included in any returned error.
func (c *Client) Post(ctx context.Context, s *sample.Sample) error {
	s.Normalize()
	body, err := json.Marshal(s)
	if err != nil {
		return fmt.Errorf("marshal sample: %w", err)
	}
	return c.postBody(ctx, c.gatewayURL+telemetryPath, body)
}

// PostSystemReport POSTs a static hardware inventory to the gateway's system-report
// endpoint with the same bearer + retry/backoff as Post.
func (c *Client) PostSystemReport(ctx context.Context, r *sample.SystemReport) error {
	r.Normalize()
	body, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("marshal system report: %w", err)
	}
	return c.postBody(ctx, c.gatewayURL+systemReportPath, body)
}

// postBody runs the bearer-authenticated POST-with-retry loop against url.
func (c *Client) postBody(ctx context.Context, url string, body []byte) error {
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			delay := backoffBase << (attempt - 1) // 200ms, 400ms, …
			slog.Debug("retrying post", "url", url, "attempt", attempt+1, "delay", delay.String(), "prev_err", lastErr)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(delay):
			}
		}
		started := time.Now()
		err := c.postOnce(ctx, url, body)
		if err == nil {
			slog.Debug("post ok", "url", url, "attempt", attempt+1, "duration", time.Since(started).String(), "bytes", len(body))
			return nil
		}
		slog.Debug("post failed", "url", url, "attempt", attempt+1, "duration", time.Since(started).String(), "err", err, "retryable", retryable(err))
		if !retryable(err) {
			return err
		}
		lastErr = err
	}
	return lastErr
}

// postOnce performs a single POST attempt.
func (c *Client) postOnce(ctx context.Context, url string, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", gwapi.BearerValue(c.token))
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	gwapi.Drain(resp) // allow connection reuse
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	return &statusError{code: resp.StatusCode}
}

// retryable reports whether err should be retried: transport errors and 5xx
// responses are retryable; a 4xx (client/auth error) is not.
func retryable(err error) bool {
	var se *statusError
	if errors.As(err, &se) {
		return se.code >= 500
	}
	return true
}
