// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package collector

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
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
// is a link-local/LAN peer; a proxy must never see the bearer token) and never
// follows redirects.
//
// A nil pin keeps the transport's TLS defaults (verification against the system
// roots and the URL host name). A non-nil pin (a 32-byte SHA-256 digest from
// ParseHostpowerFingerprint) requires TLS 1.3 and accepts exactly the server
// whose leaf certificate has that digest; chain, name and validity are not
// checked, and a mismatch fails the handshake with a *HostpowerPinError.
func NewHostpowerHTTPClient(pin []byte) *http.Client {
	c := newLocalHTTPClient(nil)
	if pin != nil {
		c.Transport.(*http.Transport).TLSClientConfig = hostpowerPinnedTLSConfig(pin)
	}
	return c
}

// newLocalHTTPClient builds the shared client: short timeout, a private
// transport without proxy support, no redirects. A non-nil dial replaces the
// transport's DialContext (the helper client dials a Unix socket).
func newLocalHTTPClient(dial func(ctx context.Context, network, addr string) (net.Conn, error)) *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = nil
	if dial != nil {
		tr.DialContext = dial
	}
	return &http.Client{
		Timeout:   hostpowerHTTPTimeout,
		Transport: tr,
		// A 3xx is a failure: no request may leave the URL the caller built.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// hostpowerEnergyURL is the single place the request URL is built.
func hostpowerEnergyURL(baseURL string) string {
	return strings.TrimRight(baseURL, "/") + HostpowerEnergyPath
}

// hostpowerHTTPCollector derives CPU watts from the host-published VM energy
// counter (Δenergy_uj/Δt). Stateful: the first sample, and the first after any
// failure or VM identity change, yields nil. System watts are always nil.
//
// Logging: every failed request at Debug with the URL and the status code or
// error; the VM ID the host serves at Info on the first success and whenever
// it changes, so a misattribution (MAC mode behind a NAT or proxy VM) is
// visible; a certificate pin mismatch at Warn once when it begins (and again
// after a success). The token and request headers are never logged.
type hostpowerHTTPCollector struct {
	baseURL string
	token   string
	pin     []byte // nil: no certificate pin configured
	pinErr  error  // a configured fingerprint that does not parse: every fetch fails
	client  *http.Client
	now     func() time.Time
	log     *slog.Logger // nil: slog.Default() at the time of logging

	mu         sync.Mutex
	have       bool
	prev       HostpowerEnergy
	at         time.Time
	loggedVMID string // the VM ID last logged at Info; empty before the first success
	pinWarned  bool   // a pin mismatch was logged at Warn since the last success
}

// newHostpowerHTTPCollector builds the collector. fingerprint is the configured
// hostpower_tls_fingerprint as given (empty: no pin); it is parsed once, and an
// invalid value makes every fetch fail (fail closed). A nil client gets
// NewHostpowerHTTPClient with the parsed pin.
func newHostpowerHTTPCollector(baseURL, token, fingerprint string, client *http.Client) *hostpowerHTTPCollector {
	c := &hostpowerHTTPCollector{baseURL: baseURL, token: token, now: time.Now}
	if strings.TrimSpace(fingerprint) != "" {
		pin, err := ParseHostpowerFingerprint(fingerprint)
		if err != nil {
			c.pinErr = errors.New("invalid hostpower_tls_fingerprint: " + err.Error())
		} else {
			c.pin = pin
		}
	}
	if client == nil {
		client = NewHostpowerHTTPClient(c.pin)
	}
	c.client = client
	return c
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

	cur, err := c.fetch(ctx)
	if err != nil {
		c.logger().Debug("hostpower: request failed", "url", c.logURL(), "err", err.Error())
		var pe *HostpowerPinError
		if errors.As(err, &pe) && !c.pinWarned {
			c.pinWarned = true
			c.logger().Warn("hostpower: server certificate does not match hostpower_tls_fingerprint",
				"want", pe.Want, "got", pe.Got, "url", c.logURL())
		}
		c.have = false
		return nil, nil, nil
	}
	c.pinWarned = false
	if c.loggedVMID == "" || cur.VMID != c.loggedVMID {
		c.loggedVMID = cur.VMID
		c.logger().Info("hostpower: host serves this VM's counter", "vmid", cur.VMID, "url", c.logURL())
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

func (c *hostpowerHTTPCollector) logger() *slog.Logger {
	if c.log != nil {
		return c.log
	}
	return slog.Default()
}

// logURL is the request URL for log lines, with any userinfo password masked.
func (c *hostpowerHTTPCollector) logURL() string {
	raw := hostpowerEnergyURL(c.baseURL)
	u, err := url.Parse(raw)
	if err != nil {
		return "(unparsable hostpower_url)"
	}
	return u.Redacted()
}

// hostpowerStatusError is a non-200 response.
type hostpowerStatusError struct{ code int }

func (e *hostpowerStatusError) Error() string { return "status " + strconv.Itoa(e.code) }

// fetch performs one request and decodes the capped body. Its errors never
// contain the token: net/http strips URL passwords from transport errors,
// and no header is ever part of an error.
func (c *hostpowerHTTPCollector) fetch(ctx context.Context) (HostpowerEnergy, error) {
	var e HostpowerEnergy
	if c.pinErr != nil {
		return e, c.pinErr
	}
	if c.pin != nil {
		// The pin shows TLS was intended: never send a clear-text request (and
		// with it the token) to a non-https URL.
		if u, err := url.Parse(c.baseURL); err != nil || u.Scheme != "https" {
			return e, errors.New("hostpower_tls_fingerprint requires an https:// hostpower_url")
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, hostpowerEnergyURL(c.baseURL), nil)
	if err != nil {
		return e, errors.New("invalid hostpower_url")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return e, err
	}
	defer func() {
		// Bounded drain so the connection can be reused; never read an unbounded body.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, hostpowerMaxBody))
		_ = resp.Body.Close()
	}()
	if resp.StatusCode != http.StatusOK {
		return e, &hostpowerStatusError{code: resp.StatusCode}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, hostpowerMaxBody+1))
	switch {
	case err != nil:
		return e, err
	case len(body) > hostpowerMaxBody:
		return e, errors.New("response body too large")
	}
	if err := json.Unmarshal(body, &e); err != nil {
		return e, errors.New("response body is not the energy JSON")
	}
	if e.VMID == "" {
		// null, {} or a body without vmid: nothing ties the counter to a VM.
		return e, errors.New("response lacks vmid")
	}
	return e, nil
}
