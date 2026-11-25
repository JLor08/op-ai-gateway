// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package collector

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// HostpowerEnergyPath is the endpoint the Proxmox hostpower service serves and the
// guest collector requests. Host and guest share this constant and HostpowerEnergy
// so the wire format cannot drift.
const HostpowerEnergyPath = "/v1/energy"

const (
	hostpowerHTTPTimeout = 1 * time.Second
	hostpowerMaxBody     = 4096
)

// HostpowerEnergy is the JSON body of GET HostpowerEnergyPath: a VM's cumulative
// energy counter in microjoules, same semantics as a powercap energy_uj file.
type HostpowerEnergy struct {
	VMID             string `json:"vmid"`
	EnergyUJ         uint64 `json:"energy_uj"`
	MaxEnergyRangeUJ uint64 `json:"max_energy_range_uj"`
}

// NewHostpowerHTTPClient returns the HTTP client for the guest collector: a short
// timeout and its own transport that ignores proxy environment variables (the host
// is a link-local/LAN peer; a proxy must never see the bearer token). Other
// transport defaults, including TLS, are kept.
func NewHostpowerHTTPClient() *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = nil
	return &http.Client{Timeout: hostpowerHTTPTimeout, Transport: tr}
}

// hostpowerEnergyURL is the single place the request URL is built.
func hostpowerEnergyURL(baseURL string) string {
	return strings.TrimRight(baseURL, "/") + HostpowerEnergyPath
}

// hostpowerHTTPCollector derives CPU watts from the host-published VM energy
// counter (Δenergy_uj/Δt). Stateful: the first sample, and the first after any
// failure or VM identity change, yields nil. System watts are always nil.
type hostpowerHTTPCollector struct {
	baseURL string
	token   string
	client  *http.Client
	now     func() time.Time

	mu   sync.Mutex
	have bool
	prev HostpowerEnergy
	at   time.Time
}

// newHostpowerHTTPCollector builds the collector; a nil client gets the default.
func newHostpowerHTTPCollector(baseURL, token string, client *http.Client) *hostpowerHTTPCollector {
	if client == nil {
		client = NewHostpowerHTTPClient()
	}
	return &hostpowerHTTPCollector{baseURL: baseURL, token: token, client: client, now: time.Now}
}

// Name identifies this collector.
func (c *hostpowerHTTPCollector) Name() string { return "hostpower" }

// Available reports whether a base URL is configured.
func (c *hostpowerHTTPCollector) Available() bool { return c.baseURL != "" }

// Collect fetches the counter once. Any failure yields (nil, nil, nil) and drops
// the previous reading so a restarted counter is never diffed against a stale one.
func (c *hostpowerHTTPCollector) Collect(ctx context.Context) (*float64, *float64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	cur, ok := c.fetch(ctx)
	if !ok {
		c.have = false
		return nil, nil, nil
	}
	at := c.now()
	prev, prevAt, had := c.prev, c.at, c.have && c.prev.VMID == cur.VMID
	c.prev, c.at, c.have = cur, at, true
	if !had {
		return nil, nil, nil
	}
	dt := at.Sub(prevAt).Seconds()
	if dt <= 0 {
		return nil, nil, nil
	}
	watts := raplDeltaJoules(prev.EnergyUJ, cur.EnergyUJ, prev.MaxEnergyRangeUJ) / dt
	return &watts, nil, nil
}

// fetch performs one request and decodes the capped body.
func (c *hostpowerHTTPCollector) fetch(ctx context.Context) (HostpowerEnergy, bool) {
	var e HostpowerEnergy
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, hostpowerEnergyURL(c.baseURL), nil)
	if err != nil {
		return e, false
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return e, false
	}
	defer func() {
		// Bounded drain so the connection can be reused; never read an unbounded body.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, hostpowerMaxBody))
		_ = resp.Body.Close()
	}()
	body, err := io.ReadAll(io.LimitReader(resp.Body, hostpowerMaxBody+1))
	if err != nil || resp.StatusCode != http.StatusOK || len(body) > hostpowerMaxBody {
		return e, false
	}
	if json.Unmarshal(body, &e) != nil {
		return e, false
	}
	return e, true
}
