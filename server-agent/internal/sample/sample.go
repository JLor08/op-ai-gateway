// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

// Package sample defines the ServerAgent telemetry wire types. Their JSON
// tags match the gateway's agentTelemetryRequest/agentHostReport/agentNetReport/
// agentGPUReport contract byte-for-byte; the agent sends only host+gpus (plus
// the minimal legacy fields) and the gateway derives cpu_load/ram_*/vram_*/
// gpu_count from them.
package sample

import (
	"encoding/json"
	"strings"
	"time"
)

// Sample is one telemetry observation the agent POSTs to the gateway.
type Sample struct {
	ReportedAt     time.Time       `json:"reported_at"`
	AgentVersion   string          `json:"agent_version"`
	OS             string          `json:"os"`
	Arch           string          `json:"arch"`
	ActiveRequests int             `json:"active_requests"`
	QueueDepth     int             `json:"queue_depth"`
	ProviderHealth json.RawMessage `json:"provider_health"`
	Capabilities   json.RawMessage `json:"capabilities"`
	Host           *Host           `json:"host,omitempty"`
	GPUs           []GPU           `json:"gpus"`
	// LoadedModels is the set of model names the agent scraped as currently
	// loaded from a configured model-status endpoint. Optional; empty when no
	// status URL is configured or the scrape found nothing. The gateway treats a
	// fresh agent report as authoritative for the server's applications.
	LoadedModels []string `json:"loaded_models"`
	// CertFingerprint/CertNotAfter/CertMode/CertCAFingerprints report what this
	// agent has ACTUALLY installed (Phase 2 certificate distribution): the leaf
	// fingerprint currently written to disk, that leaf's parsed not_after (zero
	// = none installed / unknown), the configured cert_mode, and the
	// fingerprints of every root currently in ca.pem. All additive; populated
	// from the certinstall.Report the agent's certificate sync loop last
	// produced (idiom: LoadedModels above -- optional, gateway-decoded the
	// same way a legacy payload without them still decodes).
	CertFingerprint    string    `json:"cert_fingerprint"`
	CertNotAfter       time.Time `json:"cert_not_after"`
	CertMode           string    `json:"cert_mode"`
	CertCAFingerprints []string  `json:"cert_ca_fingerprints"`
	// ProxyRoutes reports the observed state of this agent's TLS-terminating
	// reverse-proxy routes (Certificates P4 Task 2's proxy.Manager.Status()),
	// populated by the collector only for cert_mode=proxy. omitempty: nil for
	// every off/files agent (no proxy Manager at all), so this field is a pure
	// addition -- byte-neutral for every pre-existing agent's telemetry shape.
	ProxyRoutes []ProxyRouteSample `json:"proxy_routes,omitempty"`
	// Runtimes reports the live state of every agent-managed model process
	// (agent-runtime-manager design spec §7/§9), populated by collectOnce
	// only when internal/agent's runtime driver is non-nil -- i.e. only when
	// the runtime_manager feature is active for THIS agent<->gateway pair.
	// omitempty: nil (the Go zero value) for every agent that never
	// negotiated the feature, so this field is a pure addition -- byte-
	// neutral for every pre-existing agent's telemetry shape, exactly like
	// ProxyRoutes above.
	Runtimes []RuntimeSample `json:"runtimes,omitempty"`
}

// RuntimeGPUSample is one GPU's measured VRAM usage for a managed process,
// as attributed by the agent's own measurer (e.g. nvidia-smi
// --query-compute-apps, exact because the agent knows its own child's PID --
// design spec §5). There is no "estimate" field here: an unmeasured GPU is
// simply absent from RuntimeSample.GPUs, and the gateway keeps the
// operator-entered estimate it already has.
type RuntimeGPUSample struct {
	Index          int `json:"index"`
	VRAMMeasuredMB int `json:"vram_measured_mb"`
}

// RuntimeErrorSample mirrors runtime.LastError for the wire: the most
// recent failed start or crash for a spec, cleared only by that spec's next
// successful start (design spec §7), never merely by a state change.
type RuntimeErrorSample struct {
	Message    string    `json:"message"`
	At         time.Time `json:"at"`
	ExitCode   int       `json:"exit_code"`
	Failures   int       `json:"failures"`
	StderrTail string    `json:"stderr_tail,omitempty"`
}

// RuntimeSample is one agent-managed launch spec's current visible-lifecycle
// state (design spec §7), mirroring runtime.Status field-for-field for the
// wire. GPUs carries ONLY the GPUs this measurement cycle actually measured
// (omitempty: nil, not an empty array, when nothing was measured this
// cycle -- e.g. no measurer installed, or the spec is not yet running).
type RuntimeSample struct {
	SpecID    string              `json:"spec_id"`
	Model     string              `json:"model"`
	State     string              `json:"state"`
	Since     time.Time           `json:"since"`
	PID       int                 `json:"pid,omitempty"`
	Port      int                 `json:"port,omitempty"`
	InFlight  int                 `json:"in_flight"`
	Restarts  int                 `json:"restarts"`
	GPUs      []RuntimeGPUSample  `json:"gpus,omitempty"`
	LastError *RuntimeErrorSample `json:"last_error,omitempty"`
}

// ProxyRouteSample is one TLS-proxy route's observed state, mirroring
// proxy.RouteStatus (Listen, TLSActive, State) for the wire. Carries no
// certificate material or upstream address -- just the listen port, whether
// TLS is currently active on it, and (State) why, when it is not. State is
// omitempty so a zero-value (unset) State -- e.g. a report built against a
// proxy.RouteStatus that predates this field -- stays byte-neutral with the
// pre-existing wire shape.
type ProxyRouteSample struct {
	Listen    int    `json:"listen"`
	TLSActive bool   `json:"tls_active"`
	State     string `json:"state,omitempty"`
}

// Host is the host portion of a Sample (CPU/memory/swap/load/network).
type Host struct {
	CPUUtilPct     float64   `json:"cpu_util_pct"`
	CPUCores       []float64 `json:"cpu_cores"` // per-core utilization %, aligned to core index
	MemUsedBytes   int64     `json:"mem_used_bytes"`
	MemTotalBytes  int64     `json:"mem_total_bytes"`
	SwapUsedBytes  int64     `json:"swap_used_bytes"`
	SwapTotalBytes int64     `json:"swap_total_bytes"`
	Load1          float64   `json:"load1"`
	Load5          float64   `json:"load5"`
	Load15         float64   `json:"load15"`
	Net            []Net     `json:"net"`
	// CPUPowerW / SystemPowerW are best-effort, NULLABLE host-level power draw in
	// watts (CPU package + total system). A nil pointer = "not measured" (omitted
	// from the wire), distinct from a real 0 W. Collected by a PowerCollector; the
	// gateway carries them through as *float64 and persists them.
	CPUPowerW    *float64 `json:"cpu_power_w,omitempty"`
	SystemPowerW *float64 `json:"system_power_w,omitempty"`
	// CPUTempC is the best-effort, NULLABLE CPU package temperature in degrees
	// Celsius. nil = "not measured" (omitted from the wire), distinct from a real
	// 0. Collected by a TempCollector (Linux gopsutil hwmon / Windows LHM); the
	// gateway carries it through as *float64 and persists it.
	CPUTempC *float64 `json:"cpu_temp_c,omitempty"`
}

// Net is one aggregated network-interface counter.
type Net struct {
	Name    string `json:"name"`
	RxBytes int64  `json:"rx_bytes"`
	TxBytes int64  `json:"tx_bytes"`
}

// GPU is one GPU's metrics.
type GPU struct {
	Index         int     `json:"index"`
	Name          string  `json:"name"`
	UUID          string  `json:"uuid"`
	DriverVersion string  `json:"driver_version,omitempty"`
	UtilPct       float64 `json:"util_pct"`
	MemUsedBytes  int64   `json:"mem_used_bytes"`
	MemTotalBytes int64   `json:"mem_total_bytes"`
	TempC         int     `json:"temp_c"`
	VRAMTempC     int     `json:"vram_temp_c"`
	PowerW        float64 `json:"power_w"`
	FanPct        float64 `json:"fan_pct"`
}

// EmptyCapabilities returns the canonical "nothing to report" value for
// Sample.Capabilities: a valid, empty JSON object -- never Go's nil, which
// json.RawMessage would otherwise marshal as the literal `null` (a
// nil-vs-null defect this wire field cannot afford: the gateway parses it
// to negotiate agent feature flags). Normalize substitutes it for an
// absent/empty Capabilities, so every producer of this field agrees on the
// same bytes instead of each keeping its own json.RawMessage(`{}`) literal.
// (internal/agent's own capabilitiesJSON does NOT fall back to this value:
// it panics at package init instead, because a silent "{}" there would
// declare no features and deactivate runtime_manager gateway-side -- see
// its comment.)
//
// A FUNCTION, not a package-level var: json.RawMessage is a []byte under
// the hood, so a single shared package-level slice would let any future
// caller that ever wrote through a Sample.Capabilities value (e.g. an
// in-place mutation instead of a fresh assignment) corrupt this literal for
// every other Sample in the process. Returning a fresh value on every call
// makes that class of bug impossible rather than merely unlikely.
func EmptyCapabilities() json.RawMessage {
	return json.RawMessage(`{}`)
}

// Normalize fills defaults so the payload always decodes on the gateway:
// non-nil GPUs/Net slices, provider_health/capabilities default to {}.
func (s *Sample) Normalize() {
	if s.GPUs == nil {
		s.GPUs = []GPU{}
	}
	if s.LoadedModels == nil {
		s.LoadedModels = []string{}
	}
	if s.CertCAFingerprints == nil {
		s.CertCAFingerprints = []string{}
	}
	if s.Host != nil && s.Host.Net == nil {
		s.Host.Net = []Net{}
	}
	if s.Host != nil && s.Host.CPUCores == nil {
		s.Host.CPUCores = []float64{}
	}
	if len(s.ProviderHealth) == 0 {
		s.ProviderHealth = json.RawMessage(`{}`)
	}
	if len(s.Capabilities) == 0 {
		s.Capabilities = EmptyCapabilities()
	}
}

// MergeUniqueStrings returns the non-empty strings from groups in stable first-
// seen order. Certificate and gateway-trust reports use it to share the one
// cert_ca_fingerprints wire field without duplicate roots.
func MergeUniqueStrings(groups ...[]string) []string {
	seen := make(map[string]struct{})
	var out []string
	for _, group := range groups {
		for _, value := range group {
			value = strings.TrimSpace(value)
			if value == "" {
				continue
			}
			if _, ok := seen[value]; ok {
				continue
			}
			seen[value] = struct{}{}
			out = append(out, value)
		}
	}
	return out
}
