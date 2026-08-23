// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

// Command server-agent collects host + GPU (+ optional inference-scrape)
// performance metrics and POSTs them to the gateway's telemetry endpoint on an
// interval, authenticating with a per-server bearer agent token.
//
// The server identity is derived from the token by the gateway; it is never
// sent in the body and never logged.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"op-ai-server-agent/internal/agent"
	"op-ai-server-agent/internal/certinstall"
	"op-ai-server-agent/internal/client"
	"op-ai-server-agent/internal/collector"
	"op-ai-server-agent/internal/config"
	"op-ai-server-agent/internal/proxy"
	"op-ai-server-agent/internal/trust"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "-license" {
		fmt.Print(licenseNotice())
		os.Exit(0)
	}

	cfg, err := config.Load(os.Args[1:], os.Getenv)
	if err != nil {
		fmt.Fprintf(os.Stderr, "server-agent: %v\n", err)
		os.Exit(2)
	}

	// -v/verbose lowers the log level to Debug; otherwise Info. All packages log
	// through the default slog logger.
	slog.SetDefault(newLogger(cfg.Verbose))
	trustStore, err := trust.New(trust.Options{
		CAFile:      cfg.CAFile,
		CertDir:     cfg.CertDir,
		CACacheFile: cfg.CACacheFile,
		CAPEM:       cfg.CAPEM,
		TLSInsecure: cfg.TLSInsecure,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "server-agent: initialize gateway trust: %v\n", err)
		os.Exit(2)
	}

	host := collector.NewHostCollector()
	gpus := collector.DetectGPUCollectors()

	var scraper collector.Scraper
	if cfg.MetricsURL != "" {
		scraper = collector.NewScraper(cfg.MetricsURL, &http.Client{Timeout: 5 * time.Second})
	}

	var loaded collector.LoadedModelLister
	if cfg.ModelStatusURL != "" {
		loaded = collector.NewLoadedModelLister(cfg.ModelStatusURL, cfg.ModelStatusFormat, &http.Client{Timeout: 5 * time.Second})
	}

	// DetectPowerAndTempCollectors (not the two Detect*Collector calls
	// separately) so that, when LHMURL is configured, the power and temp
	// collectors share one lhmSource and issue a single LHM /data.json
	// GET+parse per telemetry cycle instead of two (SA-6).
	power, temp := collector.DetectPowerAndTempCollectors(cfg.LHMURL)

	// certInstaller is always constructed, even with cert_mode=off: Sync and
	// Report both no-op internally for that mode (no HTTP call, no disk
	// touch), so this is never conditional on the mode -- only the config's
	// own resolved cert_mode/cert_dir/cert_reload_command decide what it
	// actually does.
	certInstaller := certinstall.New(cfg.GatewayURL, cfg.Token, trustStore.HTTPClient(30*time.Second), cfg.CertDir, cfg.CertReloadCommand, cfg.CertMode)
	var trustRefresher *trust.Refresher
	if shouldRefreshGatewayTrust(cfg) {
		trustRefresher = trust.NewRefresher(cfg.GatewayURL, cfg.Token, trustStore.HTTPClient(30*time.Second), trustStore)
	}

	// deps collects every optional capability into one agent.Deps value so the
	// transport switch below only ever needs to fill in Poster; trustRefresher
	// is assigned to TrustSync ONLY inside this nil check, never unconditionally,
	// which is what keeps TrustSync a genuine nil interface (never a non-nil
	// interface wrapping a nil *trust.Refresher) when gateway-trust refresh is
	// not configured -- see NewFromDeps's and New's docs on the
	// typed-nil-interface trap this avoids.
	deps := agent.Deps{Host: host, GPUs: gpus, Scraper: scraper, Loaded: loaded, Power: power, Temp: temp, CertSync: certInstaller}
	if trustRefresher != nil {
		deps.TrustSync = trustRefresher
	}

	// cert_mode=proxy: stand up the agent-side TLS-terminating reverse proxy and
	// hand the agent a driver it exercises on the certificate-poll cadence
	// (fetch gateway routes -> resolve against local -> apply; reload the leaf
	// after a real install). off/files leave deps.ProxyDriver nil. The Manager
	// binds each listener to the agent's own mesh address derived from the
	// installed leaf's SAN -- New's empty host selects that derivation, never
	// all interfaces (the agent has no other explicit mesh-IP source, and the
	// leaf is exactly the identity the proxy terminates).
	if cfg.CertMode == config.CertModeProxy {
		manager := proxy.New(cfg.CertDir, "")
		defer manager.Close()
		routesClient := proxy.NewRoutesClient(cfg.GatewayURL, cfg.Token, trustStore.HTTPClient(30*time.Second))
		deps.ProxyDriver = proxy.NewDriver(manager, routesClient, proxyRoutes(cfg.CertProxyRoutes), cfg.CertProxyRoutesMode)
	}

	switch cfg.Transport {
	case config.TransportWebSocket:
		ws, err := client.NewWSSender(cfg.GatewayURL, cfg.Token, trustStore.HTTPClient(0))
		if err != nil {
			fmt.Fprintf(os.Stderr, "server-agent: %v\n", err)
			os.Exit(2) //nolint:gocritic // exitAfterDefer: process is exiting; the fresh proxy manager holds no bound routes yet, defer cleanup is moot
		}
		defer ws.Close()
		deps.Poster = ws
	default:
		deps.Poster = client.New(cfg.GatewayURL, cfg.Token, trustStore.HTTPClient(10*time.Second))
	}
	a := agent.NewFromDeps(cfg, deps)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	slog.Info("server-agent starting",
		"version", agent.Version,
		"gateway", gatewayHost(cfg.GatewayURL),
		"interval", cfg.Interval.String(),
		"gpu_collectors", collectorNames(gpus),
		"scrape", scraper != nil,
		"model_status", loaded != nil,
		"power_sources", collector.PowerSources(power),
		"temp_sources", collector.TempSources(temp),
		"verbose", cfg.Verbose,
		"transport", cfg.Transport,
		"cert_mode", cfg.CertMode)
	// Detailed resolved config for debugging. The token is NEVER logged, even
	// here. cert_reload_command IS included at Debug (unlike the Info line
	// above) — Debug is opt-in via -v/--verbose for local troubleshooting, and
	// an operator diagnosing a certificate reload needs to see what command
	// actually ran; it is still never surfaced by default.
	slog.Debug("resolved config",
		"gateway_url", cfg.GatewayURL,
		"metrics_url", cfg.MetricsURL,
		"model_status_url", cfg.ModelStatusURL,
		"model_status_format", cfg.ModelStatusFormat,
		"lhm_url", cfg.LHMURL,
		"cert_mode", cfg.CertMode,
		"cert_dir", cfg.CertDir,
		"cert_reload_command", cfg.CertReloadCommand,
		"cert_poll_interval", cfg.CertPollInterval.String(),
		"ca_file", cfg.CAFile,
		"ca_cache_file", cfg.CACacheFile,
		"ca_pem_set", cfg.CAPEM != "",
		"tls_insecure", cfg.TLSInsecure,
		"interval", cfg.Interval.String(),
		"system_report_interval", cfg.SystemReportInterval.String())

	if err := a.Run(ctx); err != nil {
		slog.Error("server-agent run failed", "err", err)
		os.Exit(1) //nolint:gocritic // process is exiting; defer cleanup is moot
	}
	slog.Info("server-agent shutdown complete")
}

// proxyRoutes converts the config's local TLS-proxy routes into the proxy
// package's Route type. Both are just listen+upstream; the config values are
// already validated (config.Validate), so this is a pure shape adapter.
func proxyRoutes(in []config.ProxyRoute) []proxy.Route {
	if len(in) == 0 {
		return nil
	}
	out := make([]proxy.Route, 0, len(in))
	for _, r := range in {
		out = append(out, proxy.Route{Listen: r.Listen, Upstream: r.Upstream})
	}
	return out
}

func shouldRefreshGatewayTrust(cfg config.Config) bool {
	u, err := url.Parse(cfg.GatewayURL)
	if err != nil || !strings.EqualFold(u.Scheme, "https") {
		return false
	}
	return cfg.CAFile != "" || cfg.CACacheFile != "" || cfg.CAPEM != "" || cfg.CertDir != ""
}

// newLogger builds the process logger: a text handler to stderr at Debug when
// verbose, else Info.
func newLogger(verbose bool) *slog.Logger {
	level := slog.LevelInfo
	if verbose {
		level = slog.LevelDebug
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
}

// gatewayHost returns just the host of the gateway URL for logging (never the
// token — the URL carries no secret, but we log the bare host to stay concise).
func gatewayHost(raw string) string {
	if u, err := url.Parse(raw); err == nil && u.Host != "" {
		return u.Host
	}
	return raw
}

// collectorNames lists the detected GPU collector names for the startup log.
func collectorNames(gpus []collector.GPUCollector) []string {
	names := make([]string, 0, len(gpus))
	for _, g := range gpus {
		names = append(names, g.Name())
	}
	return names
}
