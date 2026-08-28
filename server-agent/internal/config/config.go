// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

// Package config loads the ServerAgent configuration from flags, environment
// variables, and an optional JSON config file. Load takes injected args + getenv
// so it never touches the process globals directly (main.go passes os.Args[1:]
// and os.Getenv). Precedence, highest first: command-line flag > environment
// variable > config file > built-in default.
package config

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// defaultInterval is the collection cadence when nothing else sets one.
const defaultInterval = 1 * time.Second

// agentMinInterval is the lower bound for the collection cadence. WebSocket transport
// makes a sub-second cadence cheap, but below this the overhead outweighs the benefit,
// so a smaller positive value is clamped up.
const agentMinInterval = 250 * time.Millisecond

// defaultSystemReportInterval is the cadence at which the POST transport re-sends
// the static hardware inventory (self-healing a gateway restart). The WS transport
// re-sends on each reconnect, so this is a backstop there.
const defaultSystemReportInterval = 30 * time.Minute

// minSystemReportInterval is the lower bound for the re-send cadence.
const minSystemReportInterval = time.Minute

// Transport modes.
const (
	// TransportPost is the default: one HTTP POST per sample.
	TransportPost = "post"
	// TransportWebSocket streams samples over one persistent WebSocket connection.
	TransportWebSocket = "websocket"
)

// Certificate installation modes (Phase 2 distribution: the agent fetches its
// own TLS certificate from the gateway and installs it locally).
const (
	// CertModeOff is the default: the agent never contacts the certificate
	// endpoint and never writes certificate files.
	CertModeOff = "off"
	// CertModeFiles installs the certificate as files in CertDir and runs
	// CertReloadCommand after a real change.
	CertModeFiles = "files"
	// CertModeProxy installs the certificate exactly like CertModeFiles AND
	// stands up the agent-side TLS-terminating reverse proxy (see
	// internal/proxy): the agent fetches its route topology from the gateway,
	// merges it with CertProxyRoutes, and serves each route with the installed
	// mesh leaf. Requires CertDir.
	CertModeProxy = "proxy"
)

// certPollFloor is the minimum accepted OP_AGENT_CERT_POLL_INTERVAL. A poll
// cadence measured in seconds against an endpoint that can hand out a private
// key is effectively a self-inflicted denial of service, so anything positive
// but under a minute is clamped up rather than honored or silently dropped.
const certPollFloor = time.Minute

// Proxy route modes for CertProxyRoutesMode: how a locally-configured route is
// merged with the gateway-provided routes on the same listen port (see
// proxy.ResolveRoutes).
const (
	// CertProxyRoutesModeFallback is the default: a local route fills only a
	// listen the gateway did not provide (the gateway wins a shared listen).
	CertProxyRoutesModeFallback = "fallback"
	// CertProxyRoutesModeOverride makes a local route win over a gateway route
	// on the same listen.
	CertProxyRoutesModeOverride = "override"
)

// ProxyRoute is a single agent-side TLS proxy route: an external Listen port
// forwarded to a local Upstream. Consumed by the proxy manager when
// CertMode is CertModeProxy (converted to proxy.Route in main.go).
type ProxyRoute struct {
	Listen   int    `json:"listen"`
	Upstream string `json:"upstream"`
}

// defaultConfigName is the JSON config file looked for next to the binary when
// neither -config nor OP_AGENT_CONFIG points elsewhere.
const defaultConfigName = "server-agent.json"

// Runtime spec sources for RuntimeSource: which document the agent-managed
// model runtime (a later task) treats as authoritative for launch specs.
const (
	// RuntimeSourceGateway is the default: specs come from the gateway's
	// GET /api/agent/v1/runtime-config.
	RuntimeSourceGateway = "gateway"
	// RuntimeSourceFile reads the runtime-config document from
	// RuntimeConfigPath on the local disk instead of the gateway.
	RuntimeSourceFile = "file"
)

// defaultRuntimeCacheName is the runtime process-state cache file looked for
// next to the binary when RuntimeCachePath is not otherwise configured (the
// defaultConfigName precedent).
const defaultRuntimeCacheName = "server-agent-runtime.cache.json"

// Config holds the resolved agent settings. The server identity is derived from
// the token by the gateway, so there is no hostname field.
type Config struct {
	GatewayURL string        // gateway base URL, e.g. https://gw.example
	Token      string        // per-server agent bearer token
	Interval   time.Duration // collection cadence (default 1s)
	// SystemReportInterval is the POST-mode re-send cadence for the static hardware
	// inventory (default 30m, floor 1m).
	SystemReportInterval time.Duration
	MetricsURL           string // optional inference /metrics endpoint to scrape
	// ModelStatusURL is an optional endpoint the agent GETs each cycle to learn
	// which models are currently loaded (e.g. http://127.0.0.1:8080/running for
	// llama-swap, /props for llama.cpp, /v1/models for vLLM). Empty disables it.
	ModelStatusURL string
	// ModelStatusFormat selects how to parse ModelStatusURL: "openai" |
	// "llama_swap" | "llama_cpp" | "litellm" | "" / "auto" (tolerant union of all
	// shapes).
	ModelStatusFormat string
	// LHMURL is an optional LibreHardwareMonitor Remote Web Server /data.json URL the
	// agent GETs each cycle for CPU (and best-effort system) power watts. Empty
	// disables it (the default) — the only Windows CPU-watt path and a Linux RAPL
	// fallback.
	LHMURL string

	// CertMode selects the certificate behavior: CertModeOff (default, never
	// fetch), CertModeFiles (write files + run CertReloadCommand on change), or
	// CertModeProxy (install files AND run the agent-side TLS proxy). An empty
	// value resolves to CertModeOff.
	CertMode string
	// CertDir is the directory certificate files are written into. Required
	// when CertMode is not CertModeOff.
	CertDir string
	// CertReloadCommand is run, via a shell, after a changed certificate is
	// fully and atomically installed. This value comes ONLY from this local
	// config — the gateway can never deliver a command to run. Optional.
	CertReloadCommand string
	// CertPollInterval is how often the agent checks the gateway for a new
	// certificate. Zero means AUTOMATIC: the concrete cadence is derived from
	// Transport (websocket -> 6h, post -> 15m). A configured positive value
	// below certPollFloor (1m) is clamped up.
	CertPollInterval time.Duration
	// CAFile is an optional operator-managed PEM bundle. The agent only reads it.
	CAFile string
	// CACacheFile is an optional public PEM cache managed atomically by the agent.
	CACacheFile string
	// CAPEM is an optional inline bootstrap CA bundle from the generated config.
	CAPEM string

	// CertProxyRoutes lists the agent-side TLS proxy's local routes, merged with
	// the gateway-provided routes per CertProxyRoutesMode when CertMode is
	// CertModeProxy. Ignored for off/files. Nil when the config file omits the key.
	CertProxyRoutes []ProxyRoute
	// CertProxyRoutesMode selects how CertProxyRoutes are applied:
	// CertProxyRoutesModeFallback (default) or CertProxyRoutesModeOverride.
	// An empty value resolves to CertProxyRoutesModeFallback.
	CertProxyRoutesMode string

	TLSInsecure bool // skip TLS certificate verification
	Verbose     bool // -v: emit detailed debug logs to the console
	// Transport selects how samples reach the gateway: "post" (default, one HTTP
	// POST per sample) or "websocket" (one persistent connection).
	Transport string

	// RuntimeSource selects which document the agent-managed model runtime
	// treats as authoritative: RuntimeSourceGateway (default, fetched from
	// the gateway) or RuntimeSourceFile (read from RuntimeConfigPath
	// locally). An empty value resolves to RuntimeSourceGateway.
	RuntimeSource string
	// RuntimeConfigPath is the local runtime-config JSON file path. Required
	// when RuntimeSource is RuntimeSourceFile; ignored otherwise. A relative
	// value is anchored beside the selected config file (the CAFile/
	// CACacheFile precedent), since it names a file whose path is typically
	// written relative to that same config file.
	RuntimeConfigPath string
	// RuntimeAllowedBinaries lists the absolute binary paths the agent's
	// OWN local policy permits launching, regardless of what the gateway
	// asks for -- this is the operator-controlled counterweight to the
	// gateway-supplied launch spec (see internal/runtime.LocalPolicy). This
	// value comes ONLY from this local config; the gateway can never expand
	// what may run here. Nil (unset) means the operator has configured
	// nothing, which LocalPolicy treats as "refuse everything", not
	// "anything goes".
	RuntimeAllowedBinaries []string
	// RuntimeAllowedDirs lists permitted work_dir prefixes for the local
	// policy, with the same operator-only provenance as
	// RuntimeAllowedBinaries. Nil (unset) means any work_dir is permitted.
	RuntimeAllowedDirs []string
	// RuntimeCachePath is where the runtime process-state cache is
	// persisted. Defaults to defaultRuntimeCacheName next to the binary. A
	// relative value from the config file is anchored beside that config
	// file (the CAFile/CACacheFile precedent) rather than the agent's
	// process working directory, which for a service is typically "/".
	RuntimeCachePath string
	// RuntimeRouterBindHost is the operator-controlled bind address for the
	// agent-managed model runtime's router port (task-18-fix-round-1.md
	// I2): an empty string (the default) means "derive a default" -- main.go
	// tries the agent's own mesh identity first (mirroring
	// proxy.DeriveBindHost), then falls back to all interfaces, logging a
	// Warn when it does. Same operator-only provenance as
	// RuntimeAllowedBinaries/RuntimeAllowedDirs: this value comes ONLY from
	// this local config, never from the gateway -- the gateway supplies
	// only the router's PORT (router_listen), never its bind host.
	RuntimeRouterBindHost string
	// RuntimeLogBufferBytes is how much of each managed model process's
	// stdout+stderr this agent keeps in memory so an operator can read it
	// AFTER the fact -- the buffer survives the process, so a crashed model's
	// output is still there when someone opens the log view. 0 (the default)
	// means the built-in default (runtime.DefaultLogBufferBytes, 1 MiB).
	//
	// Same operator-only provenance as RuntimeAllowedBinaries/
	// RuntimeAllowedDirs, and for the same kind of reason: memory on an AI
	// server is the operator's tradeoff to make, and the gateway must not be
	// able to make this host spend more of it.
	//
	// Retained content is kept in RAM only and is never written to disk --
	// it can contain prompt text (see internal/runtime/logs.go).
	RuntimeLogBufferBytes int
	// RuntimeLogBufferTotalBytes caps the SUM of those buffers across every
	// spec, so a server with twenty specs is not twenty times the per-spec
	// number. 0 (the default) means runtime.DefaultLogBufferTotalBytes
	// (16 MiB). The agent keeps at most total/per-spec buffers, evicting the
	// least-recently-written unwatched one.
	RuntimeLogBufferTotalBytes int
}

// fileConfig mirrors the JSON config file. All fields are optional; a value set
// by an env var or flag overrides the file. TLSInsecure is a pointer so an
// absent key is distinguishable from an explicit false.
type fileConfig struct {
	GatewayURL           string       `json:"gateway_url"`
	Token                string       `json:"token"`
	Interval             string       `json:"interval"`
	SystemReportInterval string       `json:"system_report_interval"`
	MetricsURL           string       `json:"metrics_url"`
	ModelStatusURL       string       `json:"model_status_url"`
	ModelStatusFormat    string       `json:"model_status_format"`
	LHMURL               string       `json:"lhm_url"`
	CertMode             string       `json:"cert_mode"`
	CertDir              string       `json:"cert_dir"`
	CertReloadCommand    string       `json:"cert_reload_command"`
	CertPollInterval     string       `json:"cert_poll_interval"`
	CAFile               string       `json:"ca_file"`
	CACacheFile          string       `json:"ca_cache_file"`
	CAPEM                string       `json:"ca_pem"`
	CertProxyRoutes      []ProxyRoute `json:"cert_proxy_routes"`
	CertProxyRoutesMode  string       `json:"cert_proxy_routes_mode"`
	TLSInsecure          *bool        `json:"tls_insecure"`
	Verbose              *bool        `json:"verbose"`
	Transport            string       `json:"transport"`

	RuntimeSource          string   `json:"runtime_source"`
	RuntimeConfigPath      string   `json:"runtime_config"`
	RuntimeAllowedBinaries []string `json:"runtime_allowed_binaries"`
	RuntimeAllowedDirs     []string `json:"runtime_allowed_dirs"`
	RuntimeCachePath       string   `json:"runtime_cache"`
	RuntimeRouterBindHost  string   `json:"runtime_router_bind"`

	RuntimeLogBufferBytes      int `json:"runtime_log_buffer_bytes"`
	RuntimeLogBufferTotalBytes int `json:"runtime_log_buffer_total_bytes"`
}

// executable is os.Executable, indirected so tests can control the
// next-to-binary default config path.
var executable = os.Executable

// Load resolves the configuration from args, getenv, and an optional JSON file.
// Precedence per field: flag (if explicitly passed) > env var > config file >
// default. It returns a validated Config or an error.
func Load(args []string, getenv func(string) string) (Config, error) {
	fs := flag.NewFlagSet("server-agent", flag.ContinueOnError)
	// All flag defaults are empty/zero: the env/file/default fallbacks are applied
	// AFTER Parse using fs.Visit to tell an explicit flag from an untouched one.
	// This also keeps the bearer token out of the flag DefValue (Go prints
	// defaults in -h/usage output → a non-empty default would leak the token).
	gatewayURL := fs.String("gateway-url", "", "gateway base URL (env OP_AGENT_GATEWAY_URL / config gateway_url)")
	token := fs.String("token", "", "per-server agent bearer token (env OP_AGENT_TOKEN / config token)")
	metricsURL := fs.String("metrics-url", "", "optional inference /metrics URL to scrape (env OP_AGENT_METRICS_URL / config metrics_url)")
	modelStatusURL := fs.String("model-status-url", "", "optional model-status URL to poll for loaded models (env OP_AGENT_MODEL_STATUS_URL / config model_status_url)")
	modelStatusFormat := fs.String("model-status-format", "", "model-status response format: openai|llama_swap|llama_cpp|litellm|auto (env OP_AGENT_MODEL_STATUS_FORMAT / config model_status_format)")
	lhmURL := fs.String("lhm-url", "", "optional LibreHardwareMonitor /data.json URL for CPU/system power (env OP_AGENT_LHM_URL / config lhm_url)")
	certMode := fs.String("cert-mode", "", "certificate mode: off|files|proxy (proxy also runs the agent-side TLS proxy) (env OP_AGENT_CERT_MODE / config cert_mode)")
	certDir := fs.String("cert-dir", "", "directory certificate files are written into; required unless cert-mode=off (env OP_AGENT_CERT_DIR / config cert_dir)")
	certReloadCommand := fs.String("cert-reload-command", "", "local shell command run after a changed certificate is installed; never delivered by the gateway (env OP_AGENT_CERT_RELOAD_COMMAND / config cert_reload_command)")
	certPollIntervalStr := fs.String("cert-poll-interval", "", "certificate poll cadence, e.g. 15m; empty/0 = automatic by transport, floored at 1m (env OP_AGENT_CERT_POLL_INTERVAL / config cert_poll_interval)")
	caFile := fs.String("ca-file", "", "optional operator-managed CA bundle (env OP_AGENT_CA_FILE / config ca_file)")
	caCacheFile := fs.String("ca-cache-file", "", "optional agent-managed public CA cache (env OP_AGENT_CA_CACHE_FILE / config ca_cache_file)")
	caPEM := fs.String("ca-pem", "", "optional inline CA bootstrap PEM (env OP_AGENT_CA_PEM / config ca_pem)")
	transport := fs.String("transport", "", "telemetry transport: post|websocket (env OP_AGENT_TRANSPORT / config transport)")
	intervalStr := fs.String("interval", "", "collection interval, e.g. 1s (env OP_AGENT_INTERVAL / config interval)")
	systemReportIntervalStr := fs.String("system-report-interval", "", "hardware inventory re-send cadence, e.g. 30m (env OP_AGENT_SYSTEM_REPORT_INTERVAL / config system_report_interval)")
	tlsInsecure := fs.Bool("tls-insecure", false, "skip TLS certificate verification (env OP_AGENT_TLS_INSECURE / config tls_insecure)")
	// -v and --verbose are aliases for the same setting (detailed debug logging).
	vShort := fs.Bool("v", false, "verbose: emit detailed debug logs (env OP_AGENT_VERBOSE / config verbose)")
	vLong := fs.Bool("verbose", false, "alias for -v")
	configPath := fs.String("config", "", "path to a JSON config file (default: "+defaultConfigName+" next to the binary; env OP_AGENT_CONFIG)")
	runtimeSource := fs.String("runtime-source", "", "agent-managed model runtime spec source: gateway|file (env OP_AGENT_RUNTIME_SOURCE / config runtime_source)")
	runtimeConfigPath := fs.String("runtime-config", "", "path to a local runtime-config JSON file; required when runtime-source=file (env OP_AGENT_RUNTIME_CONFIG / config runtime_config)")
	runtimeCachePath := fs.String("runtime-cache", "", "path to the runtime process-state cache file (default: "+defaultRuntimeCacheName+" next to the binary; env OP_AGENT_RUNTIME_CACHE / config runtime_cache)")
	runtimeRouterBindHost := fs.String("runtime-router-bind", "", "bind host for the agent-managed model runtime's router port; empty = derive from the mesh identity, else all interfaces (env OP_AGENT_RUNTIME_ROUTER_BIND / config runtime_router_bind)")
	if err := fs.Parse(args); err != nil {
		return Config{}, err
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })

	// Resolve the config file path: -config flag > OP_AGENT_CONFIG env >
	// server-agent.json next to the binary. A file that was explicitly requested
	// (flag/env) but cannot be read is an error; a missing default file is fine.
	path := *configPath
	explicit := path != ""
	if path == "" {
		path = strings.TrimSpace(getenv("OP_AGENT_CONFIG"))
		explicit = path != ""
	}
	if path == "" {
		path = defaultConfigPath()
	}
	file, err := readConfigFile(path, explicit)
	if err != nil {
		return Config{}, err
	}

	// resolveStr returns the flag value if the flag was set, else the env value
	// if non-empty, else the file value.
	resolveStr := func(flagName, flagVal, envKey, fileVal string) string {
		if set[flagName] {
			return flagVal
		}
		if v := strings.TrimSpace(getenv(envKey)); v != "" {
			return v
		}
		return fileVal
	}

	interval, err := resolveInterval(set["interval"], *intervalStr, getenv("OP_AGENT_INTERVAL"), file.Interval)
	if err != nil {
		return Config{}, err
	}
	systemReportInterval, err := resolveSystemReportInterval(set["system-report-interval"], *systemReportIntervalStr, getenv("OP_AGENT_SYSTEM_REPORT_INTERVAL"), file.SystemReportInterval)
	if err != nil {
		return Config{}, err
	}
	certPollInterval, err := resolveCertPollInterval(set["cert-poll-interval"], *certPollIntervalStr, getenv("OP_AGENT_CERT_POLL_INTERVAL"), file.CertPollInterval)
	if err != nil {
		return Config{}, err
	}

	cfg := Config{
		GatewayURL:           strings.TrimSpace(resolveStr("gateway-url", *gatewayURL, "OP_AGENT_GATEWAY_URL", file.GatewayURL)),
		Token:                resolveStr("token", *token, "OP_AGENT_TOKEN", file.Token),
		MetricsURL:           strings.TrimSpace(resolveStr("metrics-url", *metricsURL, "OP_AGENT_METRICS_URL", file.MetricsURL)),
		ModelStatusURL:       strings.TrimSpace(resolveStr("model-status-url", *modelStatusURL, "OP_AGENT_MODEL_STATUS_URL", file.ModelStatusURL)),
		ModelStatusFormat:    strings.TrimSpace(resolveStr("model-status-format", *modelStatusFormat, "OP_AGENT_MODEL_STATUS_FORMAT", file.ModelStatusFormat)),
		LHMURL:               strings.TrimSpace(resolveStr("lhm-url", *lhmURL, "OP_AGENT_LHM_URL", file.LHMURL)),
		CertMode:             strings.ToLower(strings.TrimSpace(resolveStr("cert-mode", *certMode, "OP_AGENT_CERT_MODE", file.CertMode))),
		CertDir:              strings.TrimSpace(resolveStr("cert-dir", *certDir, "OP_AGENT_CERT_DIR", file.CertDir)),
		CertReloadCommand:    strings.TrimSpace(resolveStr("cert-reload-command", *certReloadCommand, "OP_AGENT_CERT_RELOAD_COMMAND", file.CertReloadCommand)),
		CertPollInterval:     certPollInterval,
		CAFile:               resolveStr("ca-file", *caFile, "OP_AGENT_CA_FILE", file.CAFile),
		CACacheFile:          resolveStr("ca-cache-file", *caCacheFile, "OP_AGENT_CA_CACHE_FILE", file.CACacheFile),
		CAPEM:                resolveStr("ca-pem", *caPEM, "OP_AGENT_CA_PEM", file.CAPEM),
		CertProxyRoutes:      file.CertProxyRoutes,
		CertProxyRoutesMode:  strings.ToLower(strings.TrimSpace(file.CertProxyRoutesMode)),
		Transport:            strings.ToLower(resolveStr("transport", *transport, "OP_AGENT_TRANSPORT", file.Transport)),
		Interval:             interval,
		SystemReportInterval: systemReportInterval,
		TLSInsecure:          resolveBool(set["tls-insecure"], *tlsInsecure, getenv("OP_AGENT_TLS_INSECURE"), file.TLSInsecure),
		Verbose:              resolveBool(set["v"] || set["verbose"], *vShort || *vLong, getenv("OP_AGENT_VERBOSE"), file.Verbose),

		RuntimeSource:          strings.ToLower(strings.TrimSpace(resolveStr("runtime-source", *runtimeSource, "OP_AGENT_RUNTIME_SOURCE", file.RuntimeSource))),
		RuntimeConfigPath:      strings.TrimSpace(resolveStr("runtime-config", *runtimeConfigPath, "OP_AGENT_RUNTIME_CONFIG", file.RuntimeConfigPath)),
		RuntimeAllowedBinaries: resolveStringList(getenv("OP_AGENT_RUNTIME_ALLOWED_BINARIES"), file.RuntimeAllowedBinaries),
		RuntimeAllowedDirs:     resolveStringList(getenv("OP_AGENT_RUNTIME_ALLOWED_DIRS"), file.RuntimeAllowedDirs),
		RuntimeCachePath:       strings.TrimSpace(resolveStr("runtime-cache", *runtimeCachePath, "OP_AGENT_RUNTIME_CACHE", file.RuntimeCachePath)),
		RuntimeRouterBindHost:  strings.TrimSpace(resolveStr("runtime-router-bind", *runtimeRouterBindHost, "OP_AGENT_RUNTIME_ROUTER_BIND", file.RuntimeRouterBindHost)),

		RuntimeLogBufferBytes:      resolveInt(getenv("OP_AGENT_RUNTIME_LOG_BUFFER_BYTES"), file.RuntimeLogBufferBytes),
		RuntimeLogBufferTotalBytes: resolveInt(getenv("OP_AGENT_RUNTIME_LOG_BUFFER_TOTAL_BYTES"), file.RuntimeLogBufferTotalBytes),
	}
	if cfg.Transport == "" {
		cfg.Transport = TransportWebSocket
	}
	if cfg.CertMode == "" {
		cfg.CertMode = CertModeOff
	}
	if cfg.CertProxyRoutesMode == "" {
		cfg.CertProxyRoutesMode = CertProxyRoutesModeFallback
	}
	if cfg.RuntimeSource == "" {
		cfg.RuntimeSource = RuntimeSourceGateway
	}
	if cfg.RuntimeCachePath == "" {
		cfg.RuntimeCachePath = defaultRuntimeCachePath()
	}
	cfg.CAFile = resolvePathAgainstConfig(cfg.CAFile, path)
	cfg.CACacheFile = resolvePathAgainstConfig(cfg.CACacheFile, path)
	cfg.RuntimeConfigPath = resolvePathAgainstConfig(cfg.RuntimeConfigPath, path)
	cfg.RuntimeCachePath = resolvePathAgainstConfig(cfg.RuntimeCachePath, path)
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// resolveInt applies env > file for a plain integer setting that (per the
// RuntimeAllowedBinaries/CertProxyRoutes precedent this family follows) has
// no flag layer. An env value that is absent, blank, or not a base-10 integer
// falls through to the file value rather than failing the whole load: these
// are byte-size tuning knobs, and refusing to start an agent -- taking every
// model on the host down with it -- over a fat-fingered buffer size would be
// wildly out of proportion to the mistake. The consumer clamps whatever
// arrives (runtime.NewLogStore), so an absurd value is bounded, not obeyed.
func resolveInt(envVal string, fileVal int) int {
	if v := strings.TrimSpace(envVal); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fileVal
}

// resolveStringList applies env(if non-empty, comma-separated) > file for a
// structured slice field that (per the CertProxyRoutes precedent) has no
// flag layer. Each comma-separated env entry is trimmed of surrounding
// whitespace; empty entries (e.g. a trailing comma) are dropped. A file
// value is returned exactly as decoded (nil when the key is absent), so
// "never configured" stays distinguishable from "configured empty" the same
// way CertProxyRoutes does.
func resolveStringList(envVal string, fileVal []string) []string {
	if v := strings.TrimSpace(envVal); v != "" {
		parts := strings.Split(v, ",")
		result := make([]string, 0, len(parts))
		for _, p := range parts {
			p = strings.TrimSpace(p)
			if p != "" {
				result = append(result, p)
			}
		}
		return result
	}
	return fileVal
}

// defaultRuntimeCachePath is server-agent-runtime.cache.json in the binary's
// own directory (the defaultConfigPath precedent).
func defaultRuntimeCachePath() string {
	exe, err := executable()
	if err != nil {
		return defaultRuntimeCacheName
	}
	return filepath.Join(filepath.Dir(exe), defaultRuntimeCacheName)
}

// resolvePathAgainstConfig makes a relative path independent of the process
// working directory by anchoring it beside the selected config file. It runs
// only after flag > env > file precedence has selected the final value.
func resolvePathAgainstConfig(value, configPath string) string {
	value = strings.TrimSpace(value)
	if value == "" || filepath.IsAbs(value) {
		return value
	}
	base := configPath
	if abs, err := filepath.Abs(configPath); err == nil {
		base = abs
	}
	return filepath.Clean(filepath.Join(filepath.Dir(base), value))
}

// resolveInterval applies flag > env > file > default for the duration, then enforces
// the agentMinInterval floor. A non-positive value falls back to the default; a
// positive value below the floor is clamped up with a warning.
func resolveInterval(flagSet bool, flagVal, envVal, fileVal string) (time.Duration, error) {
	raw := ""
	switch {
	case flagSet:
		raw = strings.TrimSpace(flagVal)
	case strings.TrimSpace(envVal) != "":
		raw = strings.TrimSpace(envVal)
	case strings.TrimSpace(fileVal) != "":
		raw = strings.TrimSpace(fileVal)
	default:
		return defaultInterval, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("invalid interval %q: %w", raw, err)
	}
	if d <= 0 {
		slog.Warn("interval must be positive; using default", "value", raw, "default", defaultInterval.String())
		return defaultInterval, nil
	}
	if d < agentMinInterval {
		slog.Warn("interval below floor; clamping up", "value", raw, "floor", agentMinInterval.String())
		return agentMinInterval, nil
	}
	return d, nil
}

// resolveSystemReportInterval applies flag > env > file > default (30m) and floors
// at minSystemReportInterval (1m). A non-positive value falls back to the default.
func resolveSystemReportInterval(flagSet bool, flagVal, envVal, fileVal string) (time.Duration, error) {
	raw := ""
	switch {
	case flagSet:
		raw = strings.TrimSpace(flagVal)
	case strings.TrimSpace(envVal) != "":
		raw = strings.TrimSpace(envVal)
	case strings.TrimSpace(fileVal) != "":
		raw = strings.TrimSpace(fileVal)
	default:
		return defaultSystemReportInterval, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("invalid system-report-interval %q: %w", raw, err)
	}
	if d <= 0 {
		return defaultSystemReportInterval, nil
	}
	if d < minSystemReportInterval {
		return minSystemReportInterval, nil
	}
	return d, nil
}

// resolveCertPollInterval applies flag > env > file for the certificate-poll
// cadence. There is deliberately NO built-in default DURATION: an empty value
// (unset, or explicitly "0"/"0s") means AUTOMATIC and MUST resolve to a plain
// 0 — Task 5b picks the concrete cadence from the transport (websocket -> 6h,
// post -> 15m), and it can only tell "never configured" from "configured to
// the floor" if 0 stays exactly 0 here (never floored, never defaulted to
// something else). A negative value is rejected outright; a positive value
// under certPollFloor is clamped UP (never silently accepted as-is and never
// treated as automatic) — polling a certificate/key-serving endpoint every few
// seconds is a self-inflicted denial of service.
func resolveCertPollInterval(flagSet bool, flagVal, envVal, fileVal string) (time.Duration, error) {
	raw := ""
	switch {
	case flagSet:
		raw = strings.TrimSpace(flagVal)
	case strings.TrimSpace(envVal) != "":
		raw = strings.TrimSpace(envVal)
	default:
		raw = strings.TrimSpace(fileVal)
	}
	if raw == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("invalid cert-poll-interval %q: %w", raw, err)
	}
	if d < 0 {
		return 0, fmt.Errorf("cert-poll-interval must not be negative: %q", raw)
	}
	if d == 0 {
		return 0, nil
	}
	if d < certPollFloor {
		slog.Warn("cert-poll-interval below floor; clamping up", "value", raw, "floor", certPollFloor.String())
		return certPollFloor, nil
	}
	return d, nil
}

// resolveBool applies flag(if set) > env(if set) > file > default(false).
func resolveBool(flagSet, flagVal bool, envVal string, fileVal *bool) bool {
	if flagSet {
		return flagVal
	}
	if v := strings.TrimSpace(envVal); v != "" {
		return envBool(v)
	}
	if fileVal != nil {
		return *fileVal
	}
	return false
}

// defaultConfigPath is server-agent.json in the binary's own directory.
func defaultConfigPath() string {
	exe, err := executable()
	if err != nil {
		return defaultConfigName
	}
	return filepath.Join(filepath.Dir(exe), defaultConfigName)
}

// readConfigFile reads and parses the JSON config. A missing file returns a zero
// fileConfig with no error UNLESS it was explicitly requested (flag/env). Bad
// JSON is always an error.
func readConfigFile(path string, explicit bool) (fileConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) && !explicit {
			return fileConfig{}, nil
		}
		return fileConfig{}, fmt.Errorf("read config %s: %w", path, err)
	}
	var fc fileConfig
	if err := json.Unmarshal(stripJSONLineComments(data), &fc); err != nil {
		return fileConfig{}, fmt.Errorf("parse config %s: %w", path, err)
	}
	return fc, nil
}

// stripJSONLineComments removes whole-line `//` comments so the config file can carry
// human-readable annotations (the portal-generated server-agent.json is commented).
// Only lines whose first non-whitespace characters are `//` are dropped; a `//` inside
// a value (e.g. an `https://` URL) sits on a data line and is preserved. Block comments
// and trailing (end-of-line) comments are NOT supported. A comment-free file is
// unchanged, so this is backward compatible.
func stripJSONLineComments(data []byte) []byte {
	lines := bytes.Split(data, []byte("\n"))
	kept := lines[:0]
	for _, ln := range lines {
		if bytes.HasPrefix(bytes.TrimSpace(ln), []byte("//")) {
			continue
		}
		kept = append(kept, ln)
	}
	return bytes.Join(kept, []byte("\n"))
}

// Validate ensures the required fields are present and well-formed.
func (c Config) Validate() error {
	if c.GatewayURL == "" {
		return fmt.Errorf("gateway URL is required")
	}
	if c.Token == "" {
		return fmt.Errorf("agent token is required")
	}
	if c.Interval <= 0 {
		return fmt.Errorf("interval must be greater than zero")
	}
	u, err := url.Parse(c.GatewayURL)
	if err != nil {
		return fmt.Errorf("invalid gateway URL %q: %w", c.GatewayURL, err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("gateway URL must be an absolute http or https URL: %q", c.GatewayURL)
	}
	if c.Transport != TransportPost && c.Transport != TransportWebSocket {
		return fmt.Errorf("transport must be %q or %q, got %q", TransportPost, TransportWebSocket, c.Transport)
	}
	switch c.CertMode {
	case CertModeOff:
		// A stray cert_dir/cert_reload_command left over from an unused
		// certificate stanza is inert here — it must NOT fail startup and take
		// down all telemetry with it. Warn once and move on.
		if c.CertDir != "" || c.CertReloadCommand != "" {
			slog.Warn("cert_dir/cert_reload_command set but cert_mode=off; ignoring")
		}
	case CertModeFiles:
		if c.CertDir == "" {
			return fmt.Errorf("cert-dir is required when cert-mode is %q", CertModeFiles)
		}
	case CertModeProxy:
		if c.CertDir == "" {
			return fmt.Errorf("cert-dir is required when cert-mode is %q", CertModeProxy)
		}
	default:
		return fmt.Errorf("cert-mode must be %q, %q, or %q, got %q", CertModeOff, CertModeFiles, CertModeProxy, c.CertMode)
	}
	// An empty mode is treated as CertProxyRoutesModeFallback: Load() always
	// defaults it before reaching here, but a Config built directly (e.g. in
	// tests, or a zero-value Config from before this field existed) must
	// still validate byte-neutrally rather than fail on a field it never set.
	proxyRoutesMode := c.CertProxyRoutesMode
	if proxyRoutesMode == "" {
		proxyRoutesMode = CertProxyRoutesModeFallback
	}
	if proxyRoutesMode != CertProxyRoutesModeFallback && proxyRoutesMode != CertProxyRoutesModeOverride {
		return fmt.Errorf("cert-proxy-routes-mode must be %q or %q, got %q", CertProxyRoutesModeFallback, CertProxyRoutesModeOverride, c.CertProxyRoutesMode)
	}
	for i, r := range c.CertProxyRoutes {
		if r.Listen < 1 || r.Listen > 65535 {
			return fmt.Errorf("cert_proxy_routes[%d].listen must be between 1 and 65535, got %d", i, r.Listen)
		}
		u, err := url.Parse(r.Upstream)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.Port() == "" {
			return fmt.Errorf("cert_proxy_routes[%d].upstream must be an absolute http(s) URL with host:port, got %q", i, r.Upstream)
		}
	}
	// An empty source is treated as RuntimeSourceGateway: Load() always
	// defaults it before reaching here, but a Config built directly (e.g. in
	// tests) must still validate byte-neutrally rather than fail on a field
	// it never set.
	runtimeSource := c.RuntimeSource
	if runtimeSource == "" {
		runtimeSource = RuntimeSourceGateway
	}
	switch runtimeSource {
	case RuntimeSourceGateway:
		// No further requirement.
	case RuntimeSourceFile:
		if c.RuntimeConfigPath == "" {
			return fmt.Errorf("runtime-config is required when runtime-source is %q", RuntimeSourceFile)
		}
	default:
		return fmt.Errorf("runtime-source must be %q or %q, got %q", RuntimeSourceGateway, RuntimeSourceFile, c.RuntimeSource)
	}
	return nil
}

// envBool interprets an env string as a boolean (true/1/yes/on → true).
func envBool(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}
