// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"op-ai-server-agent/internal/gwapi"
	"time"
)

// proxyRoutesPath is the gateway route the routes client polls for the desired
// TLS-proxy topology, on both the NetBird-bound agent mux and the
// (netbird_only-gated) public mux -- the same dual-mux contract certinstall's
// certificate endpoint answers on.
const proxyRoutesPath = "/api/agent/v1/proxy-routes"

// ErrNotModified is returned by Fetch when the gateway answers 304 Not Modified:
// the route set is unchanged since the previously seen ETag. The caller keeps
// whatever it last applied -- a 304 is never a signal to tear routes down.
var ErrNotModified = errors.New("proxy routes not modified")

// RoutesClient fetches the desired TLS-proxy route topology for exactly ONE
// agent -- the one identified by its bearer token -- from the gateway. It mirrors
// certinstall.Installer's transport contract: bearer auth, plain-concatenation
// path join (preserving any base path), and ETag-based conditional GETs. The
// gateway delivers DATA only (a route list); it never delivers a command.
type RoutesClient struct {
	base  string // gatewayURL, trimmed of any trailing '/'
	token string
	http  *http.Client

	// etag is the validator from the last 200 response, sent as If-None-Match on
	// the next fetch. It is single-goroutine state: the agent drives Fetch only
	// from its serialized certificate-sync goroutine (one in flight at a time).
	etag string
}

// NewRoutesClient builds a RoutesClient. gatewayURL is joined with
// proxyRoutesPath by plain string concatenation (not url.JoinPath) -- the same
// house contract certinstall/client use, so a gateway reachable at
// "https://gw/base" is polled at "https://gw/base/api/agent/v1/proxy-routes".
func NewRoutesClient(gatewayURL, token string, httpClient *http.Client) *RoutesClient {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	return &RoutesClient{
		base:  gwapi.TrimBase(gatewayURL),
		token: token,
		http:  httpClient,
	}
}

// routesResponse mirrors the gateway's proxy-routes JSON body field-for-field.
type routesResponse struct {
	Routes []wireRoute `json:"routes"`
	ETag   string      `json:"etag"`
}

// wireRoute is one route as delivered on the wire. app_id is carried for
// observability/forward-compatibility but deliberately NOT mapped into
// proxy.Route: the Manager reconciles purely on listen+upstream, so an app_id
// change alone never churns a listener.
type wireRoute struct {
	Listen   int    `json:"listen"`
	Upstream string `json:"upstream"`
	AppID    string `json:"app_id"`
}

// Fetch issues one conditional GET for the route topology. On 200 it returns the
// parsed routes and the response ETag, caching the validator for the next call.
// On 304 it returns ErrNotModified. Any other status, or a transport/decode
// failure, is returned as a plain error with nil routes -- never a spurious empty
// set -- so the caller keeps its current routes on a transient gateway hiccup.
func (c *RoutesClient) Fetch(ctx context.Context) ([]Route, string, error) {
	ep := &gwapi.Endpoint{Base: c.base, Token: c.token, Client: c.http}
	resp, err := ep.GetConditional(ctx, proxyRoutesPath, c.etag)
	if err != nil {
		return nil, "", err
	}
	defer gwapi.DrainLimited(resp) // allow connection reuse

	switch resp.StatusCode {
	case http.StatusNotModified:
		return nil, "", ErrNotModified
	case http.StatusOK:
		// handled below
	default:
		return nil, "", fmt.Errorf("proxy-routes fetch: unexpected status %d", resp.StatusCode)
	}

	var body routesResponse
	if err := json.NewDecoder(gwapi.LimitReader(resp)).Decode(&body); err != nil {
		return nil, "", fmt.Errorf("proxy-routes decode: %w", err)
	}

	routes := make([]Route, 0, len(body.Routes))
	for _, w := range body.Routes {
		routes = append(routes, Route{Listen: w.Listen, Upstream: w.Upstream})
	}
	etag := gwapi.ResponseETag(resp, body.ETag)
	c.etag = etag
	return routes, etag, nil
}

// Driver couples the routes client, the operator's local route config, and the
// proxy Manager into the two hooks the agent invokes on its certificate-poll
// cadence: SyncRoutes (refresh the desired route set from the gateway) and
// ReloadCert (hot-swap a freshly installed leaf into the running listeners). It
// is constructed only for cert_mode=proxy; off/files never build one.
type Driver struct {
	client  *RoutesClient
	manager *Manager
	local   []Route
	mode    string
}

// NewDriver builds the proxy Driver. local is the operator's cert_proxy_routes
// (already validated by config, converted to proxy.Route), and mode is
// cert_proxy_routes_mode ("fallback" or "override").
func NewDriver(m *Manager, c *RoutesClient, local []Route, mode string) *Driver {
	return &Driver{client: c, manager: m, local: local, mode: mode}
}

// SyncRoutes fetches the gateway's desired route topology, merges the local
// routes per mode, and applies the result to the Manager. On a 304 (unchanged)
// or any transient fetch error it KEEPS the currently-applied routes: a gateway
// hiccup must never tear running proxies down (the same transient-error
// discipline the Manager's own reconcile follows). A route whose leaf is not yet
// installed stays pending inside the Manager until ReloadCert brings it up.
func (d *Driver) SyncRoutes(ctx context.Context) {
	gateway, _, err := d.client.Fetch(ctx)
	switch {
	case errors.Is(err, ErrNotModified):
		return // unchanged since the last apply; nothing to reconcile
	case err != nil:
		slog.Debug("proxy: gateway route fetch failed; keeping current routes", "error", err)
		return
	}
	d.manager.Apply(ResolveRoutes(gateway, d.local, d.mode))
}

// ReloadCert hot-swaps a freshly installed leaf into the running listeners and
// brings up any route that was pending for want of a leaf. The agent calls it
// after the certificate installer reports a real install.
func (d *Driver) ReloadCert() {
	d.manager.ReloadCert()
}

// Status reports the observed state of every desired route, for the agent's
// telemetry sample (Certificates P4 Task 4). A thin passthrough to the
// Manager, which already snapshots under its own mutex.
func (d *Driver) Status() []RouteStatus {
	return d.manager.Status()
}
