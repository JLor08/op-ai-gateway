// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

// Package config loads the gateway configuration from environment variables and
// an optional JSON config file. Precedence, highest first: environment variable
// > config file > built-in default. The config file uses the SAME keys as the
// env vars (e.g. {"OP_AI_GATEWAY_ADDR": "127.0.0.1:8080"}), so any documented
// variable can be set in the file; an env var of the same name always wins.
package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// defaultConfigName is the JSON config file looked for next to the binary when
// OP_AI_GATEWAY_CONFIG is unset.
const defaultConfigName = "gateway.json"

// executable is os.Executable, indirected so tests can control the
// next-to-binary default config path.
var executable = os.Executable

type Config struct {
	Addr      string
	AgentAddr string
	AgentPort string
	// AgentTLSSeparate is the operator-facing intent for the mesh (agent)
	// listener's encrypted-port topology: false (the default) means the
	// existing combined listener keeps serving both plaintext and TLS on
	// AgentPort; true means a later task's separate encrypted listener binds
	// AgentTLSPort/AgentTLSAddr instead. This is only the ENV FALLBACK -- the
	// operator-settable cert_mesh_tls_mode system setting (portal.Service.
	// CertMeshTLSSeparateActive) is the live authority once set; see that
	// setting's reader for the "" (follow this env default) semantics.
	AgentTLSSeparate bool
	// AgentTLSPort is the TCP port a later task's separate encrypted agent
	// listener binds when AgentTLSAddr is unset (resolved from the selected
	// gateway peer, mirroring AgentPort). Kept as a raw string, like AgentPort;
	// parsed and validated at the use site in a later task. Default "8443".
	AgentTLSPort string
	// AgentTLSAddr is an explicit host:port bind for the separate encrypted
	// agent listener, mirroring AgentAddr. Empty (the default) means the
	// listener's bind host is resolved from the selected gateway peer and its
	// port comes from AgentTLSPort; when set it WINS over both. Kept as a raw
	// string; parsed and validated at the use site in a later task.
	AgentTLSAddr   string
	AgentBinaryDir string
	// ThemesDir is the directory of externally supplied, deployable theme
	// definitions (theme.Load reads one subdirectory per theme id). Empty or
	// missing is not an error -- external themes are optional. Default
	// "/themes", matching the bundled-deployment convention of AgentBinaryDir.
	ThemesDir              string
	PublicURL              string
	DefaultLanguage        string
	DBDriver               string
	SQLitePath             string
	PostgresDSN            string
	AutoMigrate            bool
	BootstrapAdminEmail    string
	BootstrapAdminName     string
	BootstrapAPIToken      string
	BootstrapAdminPassword string
	SessionIdleTTL         time.Duration
	SessionMaxTTL          time.Duration
	SessionCookieSecure    string
	StreamIdleTimeout      time.Duration
	CaptureEncryptionKey   string
	CertEncryptionKey      string
	CertEdgeOutputDir      string
	// CertEdgeProbeTarget is the host:port of the gateway's OWN edge (nginx)
	// TLS listener the synthetic self-probe dials (e.g. "web:443" in compose,
	// the Service name+port nginx's :443 server block is reachable at in
	// Kubernetes). Empty (the default) means the probe endpoint answers 409:
	// the backend genuinely cannot reach that listener by itself in either
	// bundled topology -- nginx is always a DIFFERENT container/pod (in
	// compose the backend shares the NetBird sidecar's network namespace and
	// `web` is its own service; in Kubernetes `op-gateway-web` is its own
	// Deployment) -- so there is no default worth guessing.
	CertEdgeProbeTarget string
	// CertEdgeRequireHTTPSDisable is the plaintext-refusal KILL SWITCH for
	// plan B's cert_edge_require_https setting (a later task consumes both):
	// when true, it OVERRIDES the stored setting and the gate never refuses
	// plaintext, regardless of what the portal has saved. It is deliberately
	// an env var, not a system setting -- a misconfigured/prematurely-armed
	// gate could make the portal itself unreachable over plaintext, and an
	// operator locked out that way cannot flip a stored setting through a UI
	// they can no longer load. Restarting the container with this variable
	// set is the only recovery path in that scenario, so it must never
	// depend on the settings store, the portal, or anything else the gate
	// itself might be blocking. Default false: an unset kill switch is NOT
	// engaged.
	CertEdgeRequireHTTPSDisable bool
	// CertMeshRequireTLSDisable is the plaintext-refusal KILL SWITCH for P3's
	// cert_mesh_require_tls setting: when true it OVERRIDES the stored setting and
	// the mesh agent listener never refuses plaintext. Like the edge kill switch it
	// is an env var, not a system setting -- and here it matters even more, because
	// an armed mesh gate can lock out every ServerAgent, and the recovery path
	// (this variable) must not depend on the settings store or anything the gate
	// itself blocks. Default false: an unset kill switch is NOT engaged.
	CertMeshRequireTLSDisable       bool
	CaptureMaxBytes                 int
	CaptureMemoryMaxBytes           int
	MockDelay                       time.Duration
	MockUnreachable                 bool
	SeedAppHealthMode               string
	AppHealthProbeTimeout           time.Duration
	TelemetryRetentionHours         int
	AvailabilityRetentionHours      int
	AgentPresenceTimeoutSeconds     int
	SwapProtectWindowSeconds        int
	SessionReservationWindowSeconds int
	SystemAdminModeTTLSeconds       int
	BenchmarkScheduleDefaultSeconds int
	CapacityVRAMSafetyMarginPercent int
	CapacityMaxConcurrency          int
	CapacitySettleSeconds           int
	AdmissionQueueMaxDepth          int
	NetbirdSyncIntervalSeconds      int
	NetbirdTokenRotateBeforeDays    int
	NetbirdKeyFile                  string
	// CertReconcileIntervalSeconds is the cadence (seconds) of the certificate
	// reconcile loop (cmd/gateway/cert_reconcile.go), which drives
	// portal.Service.ReconcileCertificates -- itself a no-op while the
	// certificate module is disabled, so ticking costs nothing then. Default
	// 900 (15 min); junk or <= 0 falls back to the default, and any value is
	// floored at 60s to avoid hammering an ACME directory.
	CertReconcileIntervalSeconds int
	// Energy-attribution reconciler (internal/gateway/energy_reconciler.go) +
	// idle tracker (energy_idle.go) tuning.
	EnergyReconcileIntervalSeconds int
	EnergySettleSeconds            int
	EnergyIdleWindowSeconds        int
	LogLevel                       string
	// Tracing (opt-in OpenTelemetry). Default OFF: TracingEnabled=false ⇒ the
	// dynamic sampler drops every span ⇒ ~zero overhead. When enabled, spans are
	// sampled at TracingSampleRatio (0..1) and, if OTLPEndpoint is set, exported.
	TracingEnabled     bool
	TracingSampleRatio float64
	OTLPEndpoint       string
	// LogBufferSize is the capacity of the in-memory log ring (always-on, viewer-
	// independent). Junk/<=0 falls back to the default via integer().
	LogBufferSize int
}

// Load resolves the configuration from the environment and an optional JSON
// config file (env > file > default). A malformed or explicitly-requested but
// unreadable config file is a fatal error.
func Load() (Config, error) {
	fileMap, err := loadConfigFile()
	if err != nil {
		return Config{}, err
	}
	// env wins over the file; both fall through to the built-in default.
	l := loader{getenv: func(key string) string {
		if v := os.Getenv(key); v != "" {
			return v
		}
		return fileMap[key]
	}}

	return Config{
		Addr: l.str("OP_AI_GATEWAY_ADDR", "127.0.0.1:8080"),
		// Explicit host:port bind for the NetBird-only agent listener. Empty (the
		// default) means: no dedicated agent listener from an explicit address —
		// the agent route stays on the main listener (today's behavior). When set,
		// it WINS over the selected gateway peer. Kept as a raw string; parsed and
		// validated (local-interface check) at the use site in a later task.
		AgentAddr: l.str("OP_AI_GATEWAY_AGENT_ADDR", ""),
		// Port for the NetBird agent listener when its bind host is resolved from
		// the selected gateway peer (netbird_gateway_peer_id → peer IP). Kept as a
		// raw string; joined with the peer IP + validated at the use site.
		AgentPort: l.str("OP_AI_GATEWAY_AGENT_PORT", "8081"),
		// See the AgentTLSSeparate field doc: the env-fallback intent for the
		// mesh listener's separate-encrypted-port topology, overridden live by
		// the cert_mesh_tls_mode system setting once set. Default false.
		AgentTLSSeparate: l.boolean("OP_AI_GATEWAY_AGENT_TLS_SEPARATE", false),
		// See the AgentTLSPort field doc. Default "8443".
		AgentTLSPort: l.str("OP_AI_GATEWAY_AGENT_TLS_PORT", "8443"),
		// See the AgentTLSAddr field doc. Default "" (bind host resolved from
		// the selected gateway peer).
		AgentTLSAddr: l.str("OP_AI_GATEWAY_AGENT_TLS_ADDR", ""),
		// Directory containing the ServerAgent release manifest.json + platform
		// binaries served by the agent-binary download endpoints (a later task).
		// Empty behavior is not "off" here — loadAgentManifest treats an unreadable
		// manifest at this path the same as an empty dir (errAgentBinariesUnavailable).
		AgentBinaryDir: l.str("OP_AI_GATEWAY_AGENT_BINARY_DIR", "/agents"),
		// See the ThemesDir field doc. Default "/themes".
		ThemesDir:              l.str("OP_AI_GATEWAY_THEMES_DIR", "/themes"),
		PublicURL:              l.str("OP_AI_GATEWAY_PUBLIC_URL", "http://localhost:8080"),
		DefaultLanguage:        l.str("OP_AI_GATEWAY_DEFAULT_LANGUAGE", "de"),
		DBDriver:               l.str("OP_AI_GATEWAY_DB_DRIVER", "memory"),
		SQLitePath:             l.str("OP_AI_GATEWAY_SQLITE_PATH", "./data/op-ai-gateway.db"),
		PostgresDSN:            l.str("OP_AI_GATEWAY_POSTGRES_DSN", ""),
		AutoMigrate:            l.boolean("OP_AI_GATEWAY_AUTO_MIGRATE", true),
		BootstrapAdminEmail:    l.str("OP_AI_GATEWAY_BOOTSTRAP_ADMIN_EMAIL", ""),
		BootstrapAdminName:     l.str("OP_AI_GATEWAY_BOOTSTRAP_ADMIN_NAME", ""),
		BootstrapAPIToken:      l.str("OP_AI_GATEWAY_BOOTSTRAP_API_TOKEN", ""),
		BootstrapAdminPassword: l.str("OP_AI_GATEWAY_BOOTSTRAP_ADMIN_PASSWORD", ""),
		SessionIdleTTL:         l.duration("OP_AI_GATEWAY_SESSION_IDLE_TTL", 12*time.Hour),
		SessionMaxTTL:          l.duration("OP_AI_GATEWAY_SESSION_MAX_TTL", 168*time.Hour),
		SessionCookieSecure:    l.str("OP_AI_GATEWAY_SESSION_COOKIE_SECURE", ""),
		StreamIdleTimeout:      l.streamIdleTimeout("OP_AI_GATEWAY_STREAM_IDLE_TIMEOUT", 120*time.Second),
		CaptureEncryptionKey:   l.str("OP_AI_GATEWAY_CAPTURE_ENCRYPTION_KEY", ""),
		// Hex AES-256 key sealing CERTIFICATE private keys at rest (leaf keys, the
		// ACME account key, the internal CA key) — its OWN key, deliberately
		// independent of the capture key (which also seals the SMTP password + the
		// NetBird admin token), so certificate material can be scoped/rotated on
		// its own. Do NOT reuse the capture key's value here — that defeats the
		// separation (a startup Warn flags it). Empty = no certificate cipher: on a
		// disk-backed store the certificate module then refuses to seal (and
		// therefore to issue) rather than write a private key in plaintext,
		// surfacing that in cert_last_error; the gateway still STARTS without it,
		// because the module is optional and off by default. Malformed = fatal on
		// the sqlite/postgres paths (buildCertCipher, called from sqliteDeps +
		// postgresDeps); the memory driver builds no cipher at all, so it boots
		// even with a malformed value (and its volatile store seals "plain:"
		// anyway).
		CertEncryptionKey: l.str("OP_AI_GATEWAY_CERT_ENCRYPTION_KEY", ""),
		// CertEdgeOutputDir is where the gateway writes the edge certificate for its
		// own nginx (fullchain, key, CA root). EMPTY means the gateway cannot
		// deliver it locally -- and THAT is what unlocks the key download endpoint.
		// Deliberately a deployment property (env), not an operator setting:
		// whether the container has a shared volume is not something the portal
		// may claim. Empty is a first-class value (never defaults to a path, never
		// prevents startup); trimmed of surrounding whitespace like the other
		// filesystem-path variables.
		CertEdgeOutputDir: strings.TrimSpace(l.str("OP_AI_GATEWAY_CERT_EDGE_OUTPUT_DIR", "")),
		// See the CertEdgeProbeTarget field doc: empty (the default) disables the
		// synthetic TLS self-probe endpoint (409), never a guess at a default
		// address. Trimmed like the other deployment-topology variables.
		CertEdgeProbeTarget: strings.TrimSpace(l.str("OP_AI_GATEWAY_CERT_EDGE_PROBE_TARGET", "")),
		// See the CertEdgeRequireHTTPSDisable field doc: an env-only kill switch
		// for the plaintext-refusal gate, so it keeps working even when the
		// portal (and therefore the stored cert_edge_require_https setting) is
		// unreachable. Default false -- an unset variable does NOT engage it.
		CertEdgeRequireHTTPSDisable: l.boolean("OP_AI_GATEWAY_CERT_EDGE_REQUIRE_HTTPS_DISABLE", false),
		// See the CertMeshRequireTLSDisable field doc: the env-only kill switch for
		// the mesh listener's plaintext-refusal gate. Default false.
		CertMeshRequireTLSDisable: l.boolean("OP_AI_GATEWAY_CERT_MESH_REQUIRE_TLS_DISABLE", false),
		CaptureMaxBytes:           l.integer("OP_AI_GATEWAY_CAPTURE_MAX_BYTES", 1048576),
		CaptureMemoryMaxBytes:     l.integer("OP_AI_GATEWAY_CAPTURE_MEMORY_MAX_BYTES", 67108864),
		// Test-only artificial provider delay for the mock provider (default 0 = off,
		// production-safe). Junk/0/negative -> 0 via integer().
		MockDelay: time.Duration(l.integer("OP_AI_GATEWAY_MOCK_DELAY_MS", 0)) * time.Millisecond,
		// Test/e2e seam: force the seeded mock provider unreachable so its
		// application drops out of routing + offered models (default false,
		// production-safe).
		MockUnreachable: l.boolean("OP_AI_GATEWAY_MOCK_UNREACHABLE", false),
		// Test/e2e seam: seed the mock application's health_check_mode (empty =
		// default health_path). Lets an e2e boot the seeded app directly in
		// model_sync mode (default "", production-safe).
		SeedAppHealthMode: l.str("OP_AI_GATEWAY_SEED_APP_HEALTH_MODE", ""),
		// Per-probe HTTP timeout for the app-health reachability loop.
		AppHealthProbeTimeout: l.duration("OP_AI_GATEWAY_APP_HEALTH_PROBE_TIMEOUT", 3*time.Second),
		// Retention window (hours) for persisted rich telemetry samples; the
		// prune loop drops samples older than this. Default 168 = 7 days; junk
		// or <= 0 falls back to the default via integer().
		TelemetryRetentionHours: l.integer("OP_AI_GATEWAY_TELEMETRY_RETENTION_HOURS", 168),
		// Retention window (hours) for server availability history; the prune
		// loop drops samples older than this. Default 720 = 30 days.
		AvailabilityRetentionHours: l.integer("OP_AI_GATEWAY_AVAILABILITY_RETENTION_HOURS", 720),
		// Freshness window (seconds): the ServerAgent counts as "reporting" if it
		// POSTed telemetry within this window. This is the ENV FALLBACK for the
		// operator-settable agent_presence_timeout_seconds system setting (the KV
		// value is the live authority once set — see
		// portal.Service.AgentPresenceTimeoutSeconds). Default 15.
		AgentPresenceTimeoutSeconds: l.integer("OP_AI_GATEWAY_AGENT_PRESENCE_TIMEOUT_SECONDS", 15),
		// Swap-protection recency window (seconds): a server actively serving a
		// model within this window is protected from eviction by a request for a
		// different model. Default 30; junk or <= 0 falls back via integer().
		SwapProtectWindowSeconds: l.integer("OP_AI_GATEWAY_SWAP_PROTECT_WINDOW_SECONDS", 30),
		// Session-reservation window (seconds): how long a sticky session holds a
		// reserved concurrency slot on its server for the CP3 capacity cap. Default
		// 60; junk or <= 0 falls back to 60 via integer() (so this knob cannot be set
		// to 0 to disable reservation — a value <= 0 just uses the default; the
		// resolver's internal disable is code/test-only, mirroring SWAP_PROTECT).
		SessionReservationWindowSeconds: l.integer("OP_AI_GATEWAY_SESSION_RESERVATION_WINDOW_SECONDS", 60),
		// System-Admin step-up mode elevation TTL (seconds): how long a session
		// stays elevated after EnterSystemAdminMode before it must re-elevate.
		// Default 900 (15m); junk or <= 0 falls back via integer().
		SystemAdminModeTTLSeconds: l.integer("OP_AI_GATEWAY_SYSTEM_ADMIN_MODE_TTL_SECONDS", 900),
		// Default cadence (seconds) for the scheduled benchmark mode when an
		// application enables it without setting its own interval. Default 86400 =
		// once a day; junk or <= 0 falls back via integer(). The scheduler floors
		// the effective interval at benchmarkScheduleMinSeconds regardless.
		BenchmarkScheduleDefaultSeconds: l.integer("OP_AI_GATEWAY_BENCHMARK_SCHEDULE_DEFAULT_SECONDS", 86400),
		// Capacity-benchmark tuning. VRAM safety margin (percent of total VRAM kept
		// free — the benchmark stops ramping before crossing it, so it never OOMs);
		// max concurrency the ramp will probe up to; settle time (seconds) between
		// ramp steps so utilization/queue readings stabilize. Junk or <= 0 falls back
		// to the default via integer().
		CapacityVRAMSafetyMarginPercent: l.integer("OP_AI_GATEWAY_CAPACITY_VRAM_SAFETY_MARGIN_PERCENT", 10),
		CapacityMaxConcurrency:          l.integer("OP_AI_GATEWAY_CAPACITY_MAX_CONCURRENCY", 64),
		CapacitySettleSeconds:           l.integer("OP_AI_GATEWAY_CAPACITY_SETTLE_SECONDS", 5),
		// Max number of over-capacity requests that may WAIT in the CP4 admission
		// queue for a free concurrency slot; once full a new over-capacity request is
		// rejected with 503 immediately instead of piling up. Junk or <= 0 falls back
		// to the default via integer(). Default 128.
		AdmissionQueueMaxDepth: l.integer("OP_AI_GATEWAY_ADMISSION_QUEUE_MAX_DEPTH", 128),
		// Cadence (seconds) of the NetBird peer-sync loop (peer name <= server name;
		// server domain <= peer DNS; connected status). Default 60; junk or <= 0
		// falls back to 60, and any value is floored at 30s to avoid hammering the
		// NetBird admin API. Consumed by the sync loop (a later task).
		NetbirdSyncIntervalSeconds: l.integerFloor("OP_AI_GATEWAY_NETBIRD_SYNC_INTERVAL_SECONDS", 60, 30),
		// Auto-rotate the NetBird admin API token this many days before it expires
		// (env-fallback default for the operator-settable KV setting, which is the
		// live authority once set). Default 14 when unset/unparseable/negative; an
		// explicit 0 IS honored (auto-rotation off) — unlike integer(), 0 is not
		// clamped back to the default here.
		NetbirdTokenRotateBeforeDays: l.integerAllowZero("OP_AI_GATEWAY_NETBIRD_TOKEN_ROTATE_BEFORE_DAYS", 14),
		// Absolute path to the shared-volume file the "Sidecar enrollen" action writes
		// the minted NetBird setup key to (so a waiting NetBird sidecar can self-enroll).
		// Empty (the default) = the autonomous sidecar-enroll feature is OFF (the endpoint
		// returns 409 netbird.key_file_not_configured). Consumed by the portal Service.
		NetbirdKeyFile: l.str("OP_AI_GATEWAY_NETBIRD_KEY_FILE", ""),
		// Cadence (seconds) of the certificate reconcile loop, which drives
		// portal.Service.ReconcileCertificates (issuance/renewal due-checks; a
		// no-op while the certificate module is disabled). Default 900 (15 min);
		// junk or <= 0 falls back to 900, and any value is floored at 60s so a
		// misconfiguration can't hammer an ACME directory.
		CertReconcileIntervalSeconds: l.integerFloor("OP_AI_GATEWAY_CERT_RECONCILE_INTERVAL_SECONDS", 900, 60),
		// Energy-attribution background reconciler cadence (seconds): how often
		// reconcileEnergyOnce scans for un-priced usage events. Default 15; junk
		// or <= 0 falls back via integer().
		EnergyReconcileIntervalSeconds: l.integer("OP_AI_GATEWAY_ENERGY_RECONCILE_INTERVAL_SECONDS", 15),
		// How long (seconds) the reconciler waits after a request finishes before
		// attributing its energy, so the server's telemetry samples and any
		// concurrent sibling requests have had time to land. Default 10; junk or
		// <= 0 falls back via integer().
		EnergySettleSeconds: l.integer("OP_AI_GATEWAY_ENERGY_SETTLE_SECONDS", 10),
		// Trailing window (seconds) the per-server idle-wattage tracker
		// (energy_idle.go) tracks a rolling minimum of observed power draw over,
		// before letting the estimate rise again after a sustained load increase.
		// Default 3600 (1h); junk or <= 0 falls back via integer().
		EnergyIdleWindowSeconds: l.integer("OP_AI_GATEWAY_ENERGY_IDLE_WINDOW_SECONDS", 3600),
		// Runtime log level for the gateway's slog handler + log bridge; also the
		// initial level shown in the portal Logs view. Case-insensitive
		// debug/info/warn/error, parsed by internal/logbuffer.ParseLevel
		// (unknown -> info). Default "info".
		LogLevel: l.str("OP_AI_GATEWAY_LOG_LEVEL", "info"),
		// Opt-in OpenTelemetry tracing. Default OFF (production-safe, ~zero
		// overhead); when enabled, spans are sampled at TracingSampleRatio and, if
		// OTLPEndpoint is set, exported.
		TracingEnabled:     l.boolean("OP_AI_GATEWAY_TRACING_ENABLED", false),
		TracingSampleRatio: l.floatRatio("OP_AI_GATEWAY_TRACING_SAMPLE_RATIO", 1.0),
		OTLPEndpoint:       l.str("OP_AI_GATEWAY_OTLP_ENDPOINT", ""),
		LogBufferSize:      l.integer("OP_AI_GATEWAY_LOG_BUFFER_SIZE", 5000),
	}, nil
}

// loader resolves each field through a getenv shim (env > file), falling through
// to the built-in default.
type loader struct{ getenv func(string) string }

func (l loader) str(key, fallback string) string {
	if v := l.getenv(key); v != "" {
		return v
	}
	return fallback
}

func (l loader) boolean(key string, fallback bool) bool {
	value := strings.TrimSpace(strings.ToLower(l.getenv(key)))
	if value == "" {
		return fallback
	}
	return value == "1" || value == "true" || value == "yes" || value == "on"
}

func (l loader) integer(key string, fallback int) int {
	value := strings.TrimSpace(l.getenv(key))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed <= 0 {
		return fallback
	}
	return parsed
}

// integerFloor is integer with a lower bound: junk or <= 0 uses fallback; a
// positive value below floor is clamped up to floor.
func (l loader) integerFloor(key string, fallback, floor int) int {
	v := l.integer(key, fallback)
	if v < floor {
		return floor
	}
	return v
}

// integerAllowZero is like integer but treats an explicit 0 as valid (not
// clamped to fallback): empty/unparseable/negative -> fallback; 0 or a
// positive value -> passthrough. Used where 0 has a distinct meaning ("off").
func (l loader) integerAllowZero(key string, fallback int) int {
	value := strings.TrimSpace(l.getenv(key))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed < 0 {
		return fallback
	}
	return parsed
}

// floatRatio parses a 0..1 float; empty/junk uses fallback; values outside [0,1]
// are clamped into range (so a misconfigured "5" becomes 1.0, "-1" becomes 0).
func (l loader) floatRatio(key string, fallback float64) float64 {
	value := strings.TrimSpace(l.getenv(key))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return fallback
	}
	if parsed < 0 {
		return 0
	}
	if parsed > 1 {
		return 1
	}
	return parsed
}

func (l loader) duration(key string, fallback time.Duration) time.Duration {
	value := strings.TrimSpace(l.getenv(key))
	if value == "" {
		return fallback
	}
	parsed, err := time.ParseDuration(value)
	if err != nil || parsed <= 0 {
		return fallback
	}
	return parsed
}

// streamIdleTimeout parses the streaming idle timeout. Unlike duration it
// preserves an explicit "off": empty or unparseable -> fallback; a value that
// parses to <= 0 (e.g. "0s", "-1s") -> 0, meaning the idle watchdog is disabled.
func (l loader) streamIdleTimeout(key string, fallback time.Duration) time.Duration {
	value := strings.TrimSpace(l.getenv(key))
	if value == "" {
		return fallback
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return fallback
	}
	if parsed <= 0 {
		return 0
	}
	return parsed
}

// loadConfigFile reads the JSON config into a map keyed by env-var name. The
// path is OP_AI_GATEWAY_CONFIG, else gateway.json next to the binary. A missing
// default file is fine (returns an empty map); an explicitly-requested file that
// cannot be read, or any malformed JSON, is a fatal error. Values may be JSON
// strings, booleans, or numbers (all coerced to their string form, matching the
// env-var contract).
func loadConfigFile() (map[string]string, error) {
	path := strings.TrimSpace(os.Getenv("OP_AI_GATEWAY_CONFIG"))
	explicit := path != ""
	if path == "" {
		if exe, err := executable(); err == nil {
			path = filepath.Join(filepath.Dir(exe), defaultConfigName)
		} else {
			path = defaultConfigName
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) && !explicit {
			return map[string]string{}, nil
		}
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber() // preserve large ints exactly (float64 would lose precision)
	var raw map[string]any
	if err := dec.Decode(&raw); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	// Reject trailing content after the first JSON object (e.g. two pasted objects
	// or garbage): Decode alone stops at the first value, so a truncated/duplicated
	// file would silently drop the rest. This matches the agent loader's
	// json.Unmarshal strictness and the documented "malformed = startup error".
	if dec.More() {
		return nil, fmt.Errorf("parse config %s: unexpected trailing content after the JSON object", path)
	}
	out := make(map[string]string, len(raw))
	for k, v := range raw {
		switch t := v.(type) {
		case string:
			out[k] = t
		case bool:
			out[k] = strconv.FormatBool(t)
		case json.Number:
			out[k] = t.String()
		case nil:
			// skip null
		default:
			out[k] = fmt.Sprintf("%v", t)
		}
	}
	return out, nil
}
