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

	// runtimectl is internal/runtime. main.go itself imports no stdlib
	// "runtime" package, so there is no LOCAL collision here -- the alias
	// is used anyway for consistency with every other import site in this
	// module (internal/agent/agent.go DOES need it, since that file uses
	// the stdlib runtime.GOOS/runtime.GOARCH).
	runtimectl "op-ai-server-agent/internal/runtime"
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

	// Agent-managed model runtime (Task 18, design spec §9). Fix round 1,
	// I3: the manager/source/driver are constructed UNCONDITIONALLY now --
	// feature negotiation (whether runtime_manager is active for THIS
	// agent<->gateway pair) is decided, and continuously re-decided, by the
	// Driver's own Sync (its featureActive re-check on every call), never
	// by a one-shot startup probe. The earlier version blocked startup on a
	// synchronous features fetch and froze "inactive" for the whole
	// process on any transient failure (gateway down/restarting/
	// mid-rollout, or simply a POST-transport agent that will never use
	// the feature paying up to 30s of the SAME timeout regardless); cert
	// and trust sync already self-heal on their own tickers, and this
	// feature now does too. cfg.RuntimeSource/RuntimeConfigPath pairing is
	// already guaranteed valid at this point: config.Load already ran
	// cfg.Validate and this process would have exited at startup
	// otherwise.
	featuresClient := runtimectl.NewFeaturesClient(cfg.GatewayURL, cfg.Token, trustStore.HTTPClient(30*time.Second))
	localPolicy := runtimectl.LocalPolicy{AllowedBinaries: cfg.RuntimeAllowedBinaries, AllowedDirs: cfg.RuntimeAllowedDirs}
	// The GPU vendor comes from the SAME detection the telemetry collectors
	// use (gpus, above): DetectGPUCollectors probes nvidia-smi, then
	// rocm-smi, then Apple's ioreg, in that fixed order and keeps whichever
	// reports Available(). It selects which visibility variable a
	// set_visible_devices spec's child receives. A host with none of them
	// yields GPUVendorNone, which makes that option a documented no-op
	// rather than an error -- the same "hardware capability, not a
	// negotiated feature" posture as the measurer below.
	mgr := runtimectl.NewManager(runtimectl.ManagerOptions{
		Policy:    localPolicy,
		Getenv:    os.Getenv,
		GPUVendor: runtimeGPUVendor(gpus),
		// Managed-process output retention (T3). Operator-owned, exactly
		// like the allowlists above: how much of this host's memory goes to
		// remembering what the models printed is the operator's call, and
		// the gateway can never raise it. Zero means the documented default.
		LogBufferBytes:      cfg.RuntimeLogBufferBytes,
		LogBufferTotalBytes: cfg.RuntimeLogBufferTotalBytes,
		// Whose document the launch specs are, which decides one thing only:
		// whether the resolved launch command the log stream reports masks the
		// spec's env VALUES. In file mode they are the operator's own and the
		// upward report already withholds them from the gateway, so the
		// reported command withholds them too. The same source that selects
		// the Source below selects this, read here rather than derived from
		// the Source object so the two can never disagree.
		SpecsFromLocalFile: cfg.RuntimeSource == config.RuntimeSourceFile,
	})
	defer mgr.Close()
	logBufferPerSpec, logBufferTotal := mgr.Logs().Capacity()
	// nvidia-smi is a HARDWARE capability, not a negotiated feature (design
	// spec §5): NewNvidiaComputeApps returns nil on hosts without it (AMD,
	// Apple unified memory, no GPU at all), and SetMeasurer(nil) is exactly
	// NewManager's own default -- operator VRAM estimates stand.
	mgr.SetMeasurer(collector.NewNvidiaComputeApps())

	var src runtimectl.Source
	if cfg.RuntimeSource == config.RuntimeSourceFile {
		src = runtimectl.NewFileSource(cfg.RuntimeConfigPath)
	} else {
		src = runtimectl.NewGatewaySource(cfg.GatewayURL, cfg.Token, trustStore.HTTPClient(30*time.Second), cfg.RuntimeCachePath)
	}
	var reporter runtimectl.RuntimeReporter // nil when the poster cannot report (never asserted unchecked)
	if r, ok := deps.Poster.(runtimectl.RuntimeReporter); ok {
		reporter = r // typed-nil discipline: assign only inside the ok branch
	}
	// Fix round 1, I2: the router's bind HOST is operator-controlled, never
	// gateway-supplied (only its PORT, router_listen, comes from the
	// gateway document) -- an explicit OP_AGENT_RUNTIME_ROUTER_BIND wins;
	// otherwise mirror internal/proxy's own mesh-identity derivation
	// (proxy.DeriveBindHost, the narrowest available default, matching the
	// in-module precedent); an empty result falls back to all interfaces,
	// which StartRouter itself announces at Warn when it actually binds.
	runtimeBindHost := cfg.RuntimeRouterBindHost
	if runtimeBindHost == "" {
		runtimeBindHost = proxy.DeriveBindHost(cfg.CertDir)
	}
	drv := runtimectl.NewDriver(mgr, src, featuresClient, reporter, runtimeBindHost)
	defer drv.Close()
	deps.RuntimeDriver = drv

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
		"cert_mode", cfg.CertMode,
		"runtime_source", cfg.RuntimeSource)
	// runtime_manager activation is no longer known synchronously at this
	// point (fix round 1, I3: no more one-shot startup probe) -- the
	// Driver's own Sync logs the transition itself the first time it
	// resolves negotiation (see runtime.Driver.Sync/stopAll), which is the
	// honest place for this fact to be observed.
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
		"system_report_interval", cfg.SystemReportInterval.String(),
		"runtime_config_path", cfg.RuntimeConfigPath,
		"runtime_cache_path", cfg.RuntimeCachePath,
		"runtime_allowed_binaries", len(cfg.RuntimeAllowedBinaries),
		"runtime_allowed_dirs", len(cfg.RuntimeAllowedDirs),
		"runtime_router_bind_host", runtimeBindHost,
		// The RESOLVED capacities, not the configured ones: NewLogStore
		// clamps a too-small or absent value, and an operator who set one
		// needs to see what it actually became rather than what they typed.
		"runtime_log_buffer_bytes", logBufferPerSpec,
		"runtime_log_buffer_total_bytes", logBufferTotal)

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

// runtimeGPUVendor picks the runtime manager's GPU vendor from the detected
// collectors: the FIRST one this package's runtime seam recognises, which
// preserves DetectGPUCollectors' own nvidia -> amd -> apple precedence on a
// host that somehow presents more than one. No recognised collector (a
// CPU-only host, or a vendor the collectors know but the runtime seam does
// not) yields GPUVendorNone.
//
// The name -> vendor mapping itself lives in internal/runtime
// (ParseGPUVendor), not here, so it is covered by that package's tests
// instead of by a func in package main that nothing exercises.
func runtimeGPUVendor(gpus []collector.GPUCollector) runtimectl.GPUVendor {
	for _, g := range gpus {
		if v := runtimectl.ParseGPUVendor(g.Name()); v != runtimectl.GPUVendorNone {
			return v
		}
	}
	return runtimectl.GPUVendorNone
}
