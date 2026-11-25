// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package collector

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"op-ai-server-agent/internal/sample"
)

// Endpoints and default socket of the privileged helper. The helper (root) and
// the unprivileged agent share these constants and the wire types below so the
// format cannot drift.
const (
	HelperRAPLPath          = "/v1/rapl"
	HelperMemoryModulesPath = "/v1/memory-modules"
	HelperDefaultSocket     = "/run/op-agent-helper/agent.sock"
)

const (
	helperHost          = "op-agent-helper"
	helperMemoryMaxBody = 65536
)

// HelperRAPLDomain is one top-level powercap zone with its cumulative counter.
type HelperRAPLDomain struct {
	Zone             string `json:"zone"`
	Name             string `json:"name"`
	EnergyUJ         uint64 `json:"energy_uj"`
	MaxEnergyRangeUJ uint64 `json:"max_energy_range_uj"`
}

// HelperRAPL is the JSON body of GET HelperRAPLPath.
type HelperRAPL struct {
	Domains []HelperRAPLDomain `json:"domains"`
}

// HelperMemoryModules is the JSON body of GET HelperMemoryModulesPath.
type HelperMemoryModules struct {
	Modules []sample.MemoryModule `json:"modules"`
}

// NewHelperHTTPClient returns an HTTP client that always dials the helper's
// Unix socket, ignores proxy environment variables, never follows redirects and
// has a short timeout.
func NewHelperHTTPClient(socketPath string) *http.Client {
	return newLocalHTTPClient(func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", socketPath)
	})
}

// helperURL is the single place the helper request URL is built.
func helperURL(path string) string { return "http://" + helperHost + path }

// fetchHelperJSON GETs path and decodes the JSON body into out. A non-200 status
// is a *hostpowerStatusError; a body over maxBody bytes is rejected.
func fetchHelperJSON(ctx context.Context, c *http.Client, path string, maxBody int64, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, helperURL(path), nil)
	if err != nil {
		return err
	}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer func() {
		// Bounded drain so the connection can be reused; never read an unbounded body.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxBody))
		_ = resp.Body.Close()
	}()
	if resp.StatusCode != http.StatusOK {
		return &hostpowerStatusError{code: resp.StatusCode}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	switch {
	case err != nil:
		return err
	case int64(len(body)) > maxBody:
		return errors.New("response body too large")
	}
	return json.Unmarshal(body, out)
}

// FetchHelperMemoryModules asks the helper for the SMBIOS memory modules.
func FetchHelperMemoryModules(ctx context.Context, c *http.Client) ([]sample.MemoryModule, error) {
	var out HelperMemoryModules
	if err := fetchHelperJSON(ctx, c, HelperMemoryModulesPath, helperMemoryMaxBody, &out); err != nil {
		return nil, err
	}
	return out.Modules, nil
}
