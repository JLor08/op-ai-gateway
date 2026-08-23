// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"op-ai-gateway/internal/apierror"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/store"
	"strings"
	"time"
)

// agentIngestPortal is the narrow slice of portal.API this file's handlers
// actually call (only the reactivation-edge system-default read in
// systemAgentPresenceDefault below). Declaring it here documents the group's
// true portal dependency and compile-checks it independently of portal.API's
// other 190+ methods; portal.API satisfies it structurally, so no production
// wiring changes. See agentIngestPortal() below for how a *Server exposes it.
type agentIngestPortal interface {
	ActiveAgentPresenceTimeoutSeconds(ctx context.Context) int
}

// agentIngestPortal returns s.Portal narrowed to the agent-ingest group's
// portal surface. s.Portal itself stays a portal.API (ServerDeps/Server are
// unchanged) — this accessor is purely a compile-time documentation/check
// boundary for the call site in systemAgentPresenceDefault.
func (s *Server) agentIngestPortal() agentIngestPortal {
	return s.Portal
}

type agentTelemetryRequest struct {
	ServerID       string          `json:"server_id"`
	ReportedAt     time.Time       `json:"reported_at"`
	AgentVersion   string          `json:"agent_version"`
	OS             string          `json:"os"`
	Arch           string          `json:"arch"`
	CPULoad        float64         `json:"cpu_load"`
	RAMUsedBytes   int64           `json:"ram_used_bytes"`
	RAMTotalBytes  int64           `json:"ram_total_bytes"`
	GPUCount       int             `json:"gpu_count"`
	VRAMUsedBytes  int64           `json:"vram_used_bytes"`
	VRAMTotalBytes int64           `json:"vram_total_bytes"`
	ActiveRequests int             `json:"active_requests"`
	QueueDepth     int             `json:"queue_depth"`
	LatencyMS      int             `json:"latency_ms"`
	ErrorRate      float64         `json:"error_rate"`
	ProviderHealth json.RawMessage `json:"provider_health"`
	Capabilities   json.RawMessage `json:"capabilities"`
	// Host / GPUs carry the rich per-server performance sample pushed by the
	// ServerAgent. Both are additive: a legacy payload without them still
	// decodes (Host is a pointer so its absence is distinguishable from a
	// zero-valued host).
	Host *agentHostReport `json:"host"`
	GPUs []agentGPUReport `json:"gpus"`
	// LoadedModels is the set of upstream model names the agent observed as
	// currently LOADED on this server (from a model-status endpoint it scraped).
	// Additive + optional; a fresh report takes precedence over the gateway poll
	// for this server's applications (see LoadedModelRegistry). Absent/empty
	// leaves the gateway-poll result in effect.
	LoadedModels []string `json:"loaded_models"`
	// CertFingerprint/CertNotAfter/CertMode/CertCAFingerprints report what the
	// agent has ACTUALLY installed (Phase 2 certificate distribution): the leaf
	// fingerprint it wrote to disk, that leaf's parsed not_after, its cert_mode,
	// and the fingerprints of the roots in the ca.pem bundle it holds. All
	// additive + optional; a legacy payload without them decodes and leaves the
	// last report in effect. The gateway compares CertFingerprint against the
	// issued row ("installed") and uses CertCAFingerprints to hold back leaf
	// re-issuance until a rotated root has propagated.
	CertFingerprint    string    `json:"cert_fingerprint"`
	CertNotAfter       time.Time `json:"cert_not_after"`
	CertMode           string    `json:"cert_mode"`
	CertCAFingerprints []string  `json:"cert_ca_fingerprints"`
	// ProxyRoutes reports the agent's observed TLS-terminating reverse-proxy
	// route states (Certificates P4 Task 9), mirroring the agent's
	// sample.Sample.ProxyRoutes wire format field-for-field. Populated only by
	// an agent in cert_mode=proxy (Task 2's proxy.Manager.Status()); additive +
	// optional, so a legacy/off/files agent that never sends it decodes with a
	// nil slice — AgentProxyStatusRegistry.Report treats that as "no routes",
	// byte-neutral for every pre-existing agent's telemetry.
	ProxyRoutes []ProxyRouteSample `json:"proxy_routes"`
}

// ProxyRouteSample is the gateway-side mirror of the agent's
// sample.ProxyRouteSample wire type (Listen, TLSActive only — no certificate
// material or upstream address).
type ProxyRouteSample struct {
	Listen    int  `json:"listen"`
	TLSActive bool `json:"tls_active"`
}

type agentHostReport struct {
	CPUUtilPct     float64          `json:"cpu_util_pct"`
	CPUCores       []float64        `json:"cpu_cores"`
	MemUsedBytes   int64            `json:"mem_used_bytes"`
	MemTotalBytes  int64            `json:"mem_total_bytes"`
	SwapUsedBytes  int64            `json:"swap_used_bytes"`
	SwapTotalBytes int64            `json:"swap_total_bytes"`
	Load1          float64          `json:"load1"`
	Load5          float64          `json:"load5"`
	Load15         float64          `json:"load15"`
	Net            []agentNetReport `json:"net"`
	// Nullable host-level power watts (CPU package + total system). Additive: a
	// legacy payload without them decodes with both nil.
	CPUPowerW    *float64 `json:"cpu_power_w"`
	SystemPowerW *float64 `json:"system_power_w"`
	// CPUTempC is the best-effort, NULLABLE CPU package temperature in °C. Additive:
	// a legacy payload without it decodes with it nil.
	CPUTempC *float64 `json:"cpu_temp_c"`
}

type agentNetReport struct {
	Name    string `json:"name"`
	RxBytes int64  `json:"rx_bytes"`
	TxBytes int64  `json:"tx_bytes"`
}

type agentGPUReport struct {
	Index         int     `json:"index"`
	Name          string  `json:"name"`
	UUID          string  `json:"uuid"`
	UtilPct       float64 `json:"util_pct"`
	MemUsedBytes  int64   `json:"mem_used_bytes"`
	MemTotalBytes int64   `json:"mem_total_bytes"`
	TempC         int     `json:"temp_c"`
	VRAMTempC     int     `json:"vram_temp_c"`
	PowerW        float64 `json:"power_w"`
	FanPct        float64 `json:"fan_pct"`
}

// agentSystemReport mirrors the agent's sample.SystemReport wire contract
// field-for-field (identical JSON tags). It carries a static hardware inventory;
// it never contains serials, board/chassis UUIDs, or MAC addresses (privacy D4).
type agentSystemReport struct {
	CollectedAt  time.Time       `json:"collected_at"`
	AgentVersion string          `json:"agent_version"`
	OS           string          `json:"os"`
	Arch         string          `json:"arch"`
	Kernel       string          `json:"kernel,omitempty"`
	Hostname     string          `json:"hostname,omitempty"`
	CPU          agentCPUInfo    `json:"cpu"`
	Memory       agentMemoryInfo `json:"memory"`
	Mainboard    agentMainboard  `json:"mainboard"`
	BIOS         agentBIOS       `json:"bios"`
	GPUs         []agentGPUInfo  `json:"gpus"`
}

type agentCPUInfo struct {
	Model          string  `json:"model"`
	Vendor         string  `json:"vendor"`
	PhysicalCores  int     `json:"physical_cores"`
	LogicalThreads int     `json:"logical_threads"`
	BaseMHz        float64 `json:"base_mhz"`
}

type agentMemoryInfo struct {
	TotalBytes int64               `json:"total_bytes"`
	Modules    []agentMemoryModule `json:"modules,omitempty"`
}

type agentMemoryModule struct {
	Locator   string `json:"locator,omitempty"`
	SizeBytes int64  `json:"size_bytes"`
	Type      string `json:"type,omitempty"`
	SpeedMHz  int    `json:"speed_mhz,omitempty"`
}

type agentMainboard struct {
	Vendor  string `json:"vendor"`
	Product string `json:"product"`
	Version string `json:"version"`
}

type agentBIOS struct {
	Vendor  string `json:"vendor"`
	Version string `json:"version"`
}

type agentGPUInfo struct {
	Index            int    `json:"index"`
	Name             string `json:"name"`
	UUID             string `json:"uuid,omitempty"`
	DriverVersion    string `json:"driver_version,omitempty"`
	MemoryTotalBytes int64  `json:"memory_total_bytes"`
}

// errAgentSystemReportInvalid: the system-report payload failed to parse (POST ->
// 400 agent.system_report_invalid; WS -> skip the frame, keep streaming). Reuses
// errAgentUnknownServer + storeTelemetryError for the other two failure classes.
var errAgentSystemReportInvalid = errors.New("agent system report: invalid payload")

// systemReportInvalidError wraps a concrete parse error while matching
// errAgentSystemReportInvalid via errors.Is (so the POST 400 body carries detail).
type systemReportInvalidError struct{ cause error }

func (e *systemReportInvalidError) Error() string { return e.cause.Error() }
func (e *systemReportInvalidError) Unwrap() error { return e.cause }
func (e *systemReportInvalidError) Is(target error) bool {
	return target == errAgentSystemReportInvalid
}

// Hardware-inventory sanitize bounds (defensive; the schema itself is serial-free).
const (
	maxHardwareGPUs      = 64
	maxHardwareModules   = 128
	maxHardwareStringLen = 256
)

// Sentinel + typed errors returned by ingestTelemetrySample so each transport maps
// them itself. POST reproduces today's exact status codes + bodies (below); the WS
// reader (agent_stream.go) closes on mismatch/unknown/store and skips on invalid.
var (
	// ErrAgentServerMismatch: the frame's server_id names a different server than the
	// token is bound to (POST -> 403 agent.server_mismatch; WS -> close).
	ErrAgentServerMismatch = errors.New("agent telemetry: server mismatch")
	// errAgentUnknownServer: the token-derived server id has no store row
	// (POST -> 404 agent.unknown_server; WS -> close).
	errAgentUnknownServer = errors.New("agent telemetry: unknown server")
	// errAgentTelemetryInvalid: the payload failed validation (POST -> 400
	// agent.telemetry_invalid with the detail; WS -> skip the frame, keep streaming).
	errAgentTelemetryInvalid = errors.New("agent telemetry: invalid payload")
)

// invalidPayloadError wraps a concrete validation error while matching
// errAgentTelemetryInvalid via errors.Is, so the POST 400 body carries the exact
// detail (err.Error()) it did before the refactor.
type invalidPayloadError struct{ cause error }

func (e *invalidPayloadError) Error() string        { return e.cause.Error() }
func (e *invalidPayloadError) Unwrap() error        { return e.cause }
func (e *invalidPayloadError) Is(target error) bool { return target == errAgentTelemetryInvalid }

// storeTelemetryError wraps a store failure so the POST 500 body reproduces today's
// exact apierror code+message. ingestTelemetrySample logs the specific slog.Error
// itself (transport-agnostic), so the portal Logs diagnostics are unchanged over
// either transport; WS ignores the code and just closes.
type storeTelemetryError struct {
	code, message string
	cause         error
}

func (e *storeTelemetryError) Error() string { return e.cause.Error() }
func (e *storeTelemetryError) Unwrap() error { return e.cause }

func (s *Server) handleAgentTelemetry(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	principal, ok := s.authenticateAgent(w, r)
	if !ok {
		return
	}
	serverID := principal.ServerID
	raw, ok := readRawJSON(w, r)
	if !ok {
		return
	}
	var req agentTelemetryRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, apierror.Response(codeRequestInvalidJSON, err.Error(), ""))
		return
	}
	if err := s.ingestTelemetrySample(r.Context(), serverID, req, raw); err != nil {
		writeAgentIngestError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"accepted": true, "server_id": serverID, "telemetry_stale_after_seconds": 120})
}

// ingestTelemetrySample is the transport-agnostic per-sample core shared by the POST
// handler (handleAgentTelemetry) and the WebSocket reader (handleAgentStream). It
// reconciles the body server_id against the token-derived id, builds both the routing
// summary + rich sample up front (so a bad frame rejects atomically before any store
// write), persists them, and fans the sample out to live perf subscribers + updates
// the loaded-model + agent-presence registries. It returns the typed sentinel errors
// documented above; store failures are logged here (server_id + err, never the token)
// so the portal Logs diagnostics are identical regardless of transport.
func (s *Server) ingestTelemetrySample(ctx context.Context, serverID string, req agentTelemetryRequest, raw json.RawMessage) error {
	if bodyID := strings.TrimSpace(req.ServerID); bodyID != "" && bodyID != serverID {
		slog.Warn("agent telemetry rejected: server mismatch", "server_id", serverID, "body_server_id", bodyID)
		return ErrAgentServerMismatch
	}
	req.ServerID = serverID
	slog.Debug("agent telemetry received", "server_id", serverID, "gpus", len(req.GPUs), "has_host", req.Host != nil)
	now := time.Now().UTC()
	telemetry, err := telemetryFromRequest(req, raw, now)
	if err != nil {
		return &invalidPayloadError{cause: err}
	}
	// Build the rich sample up front so a bad rich section rejects atomically.
	sample, err := telemetrySampleFromRequest(req, now)
	if err != nil {
		return &invalidPayloadError{cause: err}
	}
	server, err := s.Routes.AIServerByID(ctx, serverID)
	if errors.Is(err, store.ErrNotFound) {
		slog.Warn("agent telemetry rejected: unknown server", "server_id", serverID)
		return errAgentUnknownServer
	}
	if err != nil {
		slog.Error("agent telemetry: server lookup failed", "server_id", serverID, "err", err)
		return &storeTelemetryError{code: "agent.server_lookup_failed", message: "server lookup failed", cause: err}
	}
	if err := s.Routes.UpsertTelemetry(ctx, telemetry); err != nil {
		slog.Error("agent telemetry: telemetry update failed", "server_id", serverID, "err", err)
		return &storeTelemetryError{code: "agent.telemetry_update_failed", message: "telemetry update failed", cause: err}
	}
	server.LastSeenAt = &telemetry.ReportedAt
	server.UpdatedAt = telemetry.UpdatedAt
	if err := s.Routes.UpdateAIServer(ctx, server); err != nil {
		slog.Error("agent telemetry: server update failed", "server_id", serverID, "err", err)
		return &storeTelemetryError{code: "agent.server_update_failed", message: "server update failed", cause: err}
	}
	// Persist the rich sample and fan it out to live perf subscribers, after the
	// routing summary is authoritative so routing state stays correct if this fails.
	if err := s.Routes.InsertTelemetrySample(ctx, sample); err != nil {
		slog.Error("agent telemetry: sample insert failed", "server_id", serverID, "err", err)
		return &storeTelemetryError{code: "agent.telemetry_sample_failed", message: "telemetry sample insert failed", cause: err}
	}
	// Feed the energy idle-tracker AFTER the sample is persisted: a per-server
	// rolling minimum of observed power draw, used by the energy reconciler as
	// an emergent idle-wattage estimate absent an operator-set IdleWatts
	// override. Best-effort/cheap: nil-safe (a nil tracker's Observe is a
	// no-op) and the PUE-default read is memoized (systemEnergyDefaultPue), so
	// this never adds a system_settings round-trip to every ~1s ingest.
	if s.EnergyIdle != nil {
		pue := effectivePue(ServerEnergyConfig{Pue: server.Pue}, s.systemEnergyDefaultPue(ctx))
		s.EnergyIdle.Observe(server.ID, serverPowerW(sample, pue), now)
	}
	s.ServerPerf.publish(sample)
	// A fresh agent report wins over the gateway poll for this server's apps.
	s.LoadedModels.SetAgentReport(serverID, req.LoadedModels)
	// Record what the agent says it has INSTALLED (Phase 2 certificate
	// distribution). Deliberately after every store write succeeded: a report is
	// evidence about this server's disk, and stamping it while the sample itself
	// failed to persist would claim freshness the gateway does not have. Nil-safe.
	s.AgentCertReports.Report(serverID, sanitizeAgentCertReport(req.CertFingerprint, req.CertNotAfter, req.CertMode, req.CertCAFingerprints))
	// Record the agent's observed TLS-proxy route states (Certificates P4 Task 9),
	// so the Task 10 switch reconcile can gate a public-listener flip on what the
	// agent says is ACTUALLY running rather than only on what the gateway asked
	// for (agent_proxy_routes.go). Unconditional, mirroring SetAgentReport above:
	// each sample is a full snapshot, and an agent that never sends proxy_routes
	// (cert_mode != proxy) reports nil here, which Report treats as "no routes".
	s.AgentProxyStatus.Report(serverID, proxyRouteStatusesFromSamples(req.ProxyRoutes))
	s.maybeFireReactivation(ctx, server)
	return nil
}

// proxyRouteStatusesFromSamples maps the wire-decoded ProxyRouteSample slice to
// the registry's ProxyRouteStatus, preserving order. nil/empty in -> nil out.
func proxyRouteStatusesFromSamples(samples []ProxyRouteSample) []ProxyRouteStatus {
	if len(samples) == 0 {
		return nil
	}
	out := make([]ProxyRouteStatus, len(samples))
	for i, sample := range samples {
		out[i] = ProxyRouteStatus(sample)
	}
	return out
}

// agentPresenceDefaultTTL bounds how long the memoized system-wide agent-presence-
// timeout default (see systemAgentPresenceDefault) is reused before re-reading
// system_settings.
const agentPresenceDefaultTTL = 30 * time.Second

// systemAgentPresenceDefault returns the system-wide agent-presence-timeout default,
// memoized for agentPresenceDefaultTTL (via settingCache -- ttlcache.go) so the
// reactivation-edge check does not read system_settings on every telemetry ingest.
// The settings read happens OUTSIDE the lock (a concurrent refresh is harmless —
// idempotent). Falls back to the hardcoded default when Portal is unset (tests).
func (s *Server) systemAgentPresenceDefault(ctx context.Context) int {
	return s.agentPresenceDefault.Get(ctx, agentPresenceDefaultTTL, func(ctx context.Context) int {
		if p := s.agentIngestPortal(); p != nil {
			return p.ActiveAgentPresenceTimeoutSeconds(ctx)
		}
		return portal.DefaultAgentPresenceTimeoutSeconds
	})
}

// maybeFireReactivation stamps the server's agent-presence recency and, when this
// report is an inactive->active edge against the server's EFFECTIVE presence window
// (its own AgentPresenceTimeoutSeconds override, else the system default — the SAME
// computation the "Agent" status column uses), fires onAgentReactivated. The hook is
// expected to be non-blocking. Nil-safe: a nil registry / nil hook is a no-op (the
// presence stamp still happens via ReportReactivated).
func (s *Server) maybeFireReactivation(ctx context.Context, server routing.AIServer) {
	sysDefault := s.systemAgentPresenceDefault(ctx)
	window := time.Duration(routing.EffectiveAgentPresenceTimeoutSeconds(server, sysDefault, portal.MinAgentPresenceTimeoutSeconds, portal.MaxAgentPresenceTimeoutSeconds)) * time.Second
	if s.AgentPresence.ReportReactivated(server.ID, window) && s.onAgentReactivated != nil {
		s.onAgentReactivated(server.ID)
	}
}

// agentIngestErrRows are writeAgentIngestError's mapper-specific rows;
// errAgentUnknownServer maps identically in writeAgentSystemReportError and
// lives in sharedErrorMap instead. errAgentTelemetryInvalid keeps its
// dynamic message (err.Error(), a validation detail) via msgFn.
var agentIngestErrRows = []errRow{
	{err: ErrAgentServerMismatch, status: http.StatusForbidden, code: "agent.server_mismatch", msg: "token is not bound to that server"},
	{err: errAgentTelemetryInvalid, status: http.StatusBadRequest, code: "agent.telemetry_invalid", msgFn: func(err error) string { return err.Error() }},
}

// writeAgentIngestError maps an ingestTelemetrySample error to the SAME HTTP response
// the POST path produced before the refactor.
func writeAgentIngestError(w http.ResponseWriter, err error) {
	if writeMappedError(w, err, agentIngestErrRows, 0, "", "") {
		return
	}
	var se *storeTelemetryError
	if errors.As(err, &se) {
		writeJSON(w, http.StatusInternalServerError, apierror.Response(se.code, se.message, ""))
		return
	}
	writeJSON(w, http.StatusInternalServerError, apierror.Response("agent.telemetry_failed", "telemetry ingest failed", ""))
}

// clampHardwareString truncates an over-long string to a fixed ceiling (defensive
// against a hostile/buggy agent). It never introduces serials — it only bounds.
func clampHardwareString(s string) string {
	if len(s) > maxHardwareStringLen {
		return s[:maxHardwareStringLen]
	}
	return s
}

func nonNegI(v int) int {
	if v < 0 {
		return 0
	}
	return v
}

func nonNegI64(v int64) int64 {
	if v < 0 {
		return 0
	}
	return v
}

// sanitizeSystemReport clamps negatives, caps slice sizes + string lengths, forces a
// non-nil GPUs slice, and re-marshals to a canonical JSON blob (stable Go field
// order). It returns the canonical bytes + the effective collected_at (the report's
// value, else now). The schema has no serial/UUID/MAC fields, so the blob is
// inherently free of them (GPU UUID excepted, which is allowed).
func sanitizeSystemReport(r *agentSystemReport, now time.Time) ([]byte, time.Time) {
	r.AgentVersion = clampHardwareString(r.AgentVersion)
	r.OS = clampHardwareString(r.OS)
	r.Arch = clampHardwareString(r.Arch)
	r.Kernel = clampHardwareString(r.Kernel)
	r.Hostname = clampHardwareString(r.Hostname)

	r.CPU.Model = clampHardwareString(r.CPU.Model)
	r.CPU.Vendor = clampHardwareString(r.CPU.Vendor)
	r.CPU.PhysicalCores = nonNegI(r.CPU.PhysicalCores)
	r.CPU.LogicalThreads = nonNegI(r.CPU.LogicalThreads)
	if r.CPU.BaseMHz < 0 || math.IsNaN(r.CPU.BaseMHz) || math.IsInf(r.CPU.BaseMHz, 0) {
		r.CPU.BaseMHz = 0
	}

	r.Memory.TotalBytes = nonNegI64(r.Memory.TotalBytes)
	if len(r.Memory.Modules) > maxHardwareModules {
		r.Memory.Modules = r.Memory.Modules[:maxHardwareModules]
	}
	for i := range r.Memory.Modules {
		r.Memory.Modules[i].Locator = clampHardwareString(r.Memory.Modules[i].Locator)
		r.Memory.Modules[i].Type = clampHardwareString(r.Memory.Modules[i].Type)
		r.Memory.Modules[i].SizeBytes = nonNegI64(r.Memory.Modules[i].SizeBytes)
		r.Memory.Modules[i].SpeedMHz = nonNegI(r.Memory.Modules[i].SpeedMHz)
	}

	r.Mainboard.Vendor = clampHardwareString(r.Mainboard.Vendor)
	r.Mainboard.Product = clampHardwareString(r.Mainboard.Product)
	r.Mainboard.Version = clampHardwareString(r.Mainboard.Version)
	r.BIOS.Vendor = clampHardwareString(r.BIOS.Vendor)
	r.BIOS.Version = clampHardwareString(r.BIOS.Version)

	if len(r.GPUs) > maxHardwareGPUs {
		r.GPUs = r.GPUs[:maxHardwareGPUs]
	}
	if r.GPUs == nil {
		r.GPUs = []agentGPUInfo{}
	}
	for i := range r.GPUs {
		r.GPUs[i].Name = clampHardwareString(r.GPUs[i].Name)
		r.GPUs[i].UUID = clampHardwareString(r.GPUs[i].UUID)
		r.GPUs[i].DriverVersion = clampHardwareString(r.GPUs[i].DriverVersion)
		r.GPUs[i].Index = nonNegI(r.GPUs[i].Index)
		r.GPUs[i].MemoryTotalBytes = nonNegI64(r.GPUs[i].MemoryTotalBytes)
	}

	collectedAt := r.CollectedAt
	if collectedAt.IsZero() {
		collectedAt = now
	}
	canonical, err := json.Marshal(r)
	if err != nil {
		// Impossible for these field types; fall back to an empty object.
		return []byte("{}"), collectedAt
	}
	return canonical, collectedAt
}

// ingestSystemReport is the transport-agnostic core shared by the POST handler
// (handleAgentSystemReport) and the WS reader (handleAgentStream case
// "system_report"). It parses + sanitizes the report to a canonical serial-free
// blob, existence-checks the server, and upserts server_hardware. Returns the same
// typed sentinels the telemetry path uses so each transport maps them itself.
func (s *Server) ingestSystemReport(ctx context.Context, serverID string, raw json.RawMessage) error {
	var req agentSystemReport
	if err := json.Unmarshal(raw, &req); err != nil {
		return &systemReportInvalidError{cause: err}
	}
	now := time.Now().UTC()
	canonical, collectedAt := sanitizeSystemReport(&req, now)
	if _, err := s.Routes.AIServerByID(ctx, serverID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			slog.Warn("agent system report rejected: unknown server", "server_id", serverID)
			return errAgentUnknownServer
		}
		slog.Error("agent system report: server lookup failed", "server_id", serverID, "err", err)
		return &storeTelemetryError{code: "agent.server_lookup_failed", message: "server lookup failed", cause: err}
	}
	hw := routing.ServerHardware{ServerID: serverID, CollectedAt: collectedAt, ReportJSON: string(canonical), UpdatedAt: now}
	if err := s.Routes.UpsertServerHardware(ctx, hw); err != nil {
		slog.Error("agent system report: upsert failed", "server_id", serverID, "err", err)
		return &storeTelemetryError{code: "agent.system_report_failed", message: "system report upsert failed", cause: err}
	}
	slog.Debug("agent system report stored", "server_id", serverID, "gpus", len(req.GPUs))
	return nil
}

// agentSystemReportErrRows is writeAgentSystemReportError's one
// mapper-specific row (errAgentSystemReportInvalid keeps its dynamic message
// via msgFn); errAgentUnknownServer maps identically in writeAgentIngestError
// and lives in sharedErrorMap instead.
var agentSystemReportErrRows = []errRow{
	{err: errAgentSystemReportInvalid, status: http.StatusBadRequest, code: "agent.system_report_invalid", msgFn: func(err error) string { return err.Error() }},
}

// writeAgentSystemReportError maps an ingestSystemReport error to an HTTP response
// (POST only; the WS reader ignores the code and closes).
func writeAgentSystemReportError(w http.ResponseWriter, err error) {
	if writeMappedError(w, err, agentSystemReportErrRows, 0, "", "") {
		return
	}
	var se *storeTelemetryError
	if errors.As(err, &se) {
		writeJSON(w, http.StatusInternalServerError, apierror.Response(se.code, se.message, ""))
		return
	}
	writeJSON(w, http.StatusInternalServerError, apierror.Response("agent.system_report_failed", "system report ingest failed", ""))
}

// handleAgentSystemReport is the POST /api/agent/v1/system-report endpoint (mirrors
// handleAgentTelemetry): bearer -> LookupAgentToken -> readRawJSON -> ingest.
func (s *Server) handleAgentSystemReport(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	principal, ok := s.authenticateAgent(w, r)
	if !ok {
		return
	}
	serverID := principal.ServerID
	raw, ok := readRawJSON(w, r)
	if !ok {
		return
	}
	if err := s.ingestSystemReport(r.Context(), serverID, raw); err != nil {
		writeAgentSystemReportError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"accepted": true, "server_id": serverID})
}

// telemetrySampleFromRequest builds a bounds-checked routing.TelemetrySample
// from an agent telemetry request. Host scalars, per-GPU, and per-nic values are
// validated with the same non-negative/finite discipline as telemetryFromRequest.
// GPUs/Net are always returned as non-nil slices so the JSON columns serialize to
// [] (matching the default '[]' migration), and a nil req.Host yields a sample
// with zeroed host scalars and empty GPU/Net slices.
func telemetrySampleFromRequest(req agentTelemetryRequest, now time.Time) (routing.TelemetrySample, error) {
	nonNegFloat := func(v float64, field string) error {
		if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
			return fmt.Errorf("%s must be non-negative", field)
		}
		return nil
	}
	// nonNegFloatPtr copies a nullable metric only when it is present and sane: nil
	// stays nil; a negative/NaN/Inf value becomes nil (treated as unavailable), so
	// a bad reading never surfaces as a bogus 0 or poisons the persisted history.
	nonNegFloatPtr := func(p *float64, _ string) *float64 {
		if p == nil {
			return nil
		}
		v := *p
		if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
			return nil
		}
		out := v
		return &out
	}
	clampNonNeg := func(v int) int {
		if v < 0 {
			return 0
		}
		return v
	}
	reportedAt := req.ReportedAt
	if reportedAt.IsZero() {
		reportedAt = now
	}
	if req.ActiveRequests < 0 || req.QueueDepth < 0 {
		return routing.TelemetrySample{}, fmt.Errorf("numeric counters must be non-negative")
	}
	sample := routing.TelemetrySample{
		ServerID:       strings.TrimSpace(req.ServerID),
		ReportedAt:     reportedAt,
		ActiveRequests: req.ActiveRequests,
		QueueDepth:     req.QueueDepth,
		CPUCores:       []float64{},
		GPUs:           []routing.GPUSample{},
		Net:            []routing.NetSample{},
	}
	if h := req.Host; h != nil {
		if err := nonNegFloat(h.CPUUtilPct, "cpu_util_pct"); err != nil {
			return routing.TelemetrySample{}, err
		}
		if err := nonNegFloat(h.Load1, "load1"); err != nil {
			return routing.TelemetrySample{}, err
		}
		if err := nonNegFloat(h.Load5, "load5"); err != nil {
			return routing.TelemetrySample{}, err
		}
		if err := nonNegFloat(h.Load15, "load15"); err != nil {
			return routing.TelemetrySample{}, err
		}
		if h.MemUsedBytes < 0 || h.MemTotalBytes < 0 || h.SwapUsedBytes < 0 || h.SwapTotalBytes < 0 {
			return routing.TelemetrySample{}, fmt.Errorf("host memory counters must be non-negative")
		}
		sample.CPUUtilPct = h.CPUUtilPct
		// Per-core utilization: drop NaN/Inf and clamp each to [0,100] (a bad core
		// reading must not reject the whole sample). Empty stays non-nil.
		if len(h.CPUCores) > 0 {
			cores := make([]float64, 0, len(h.CPUCores))
			for _, v := range h.CPUCores {
				if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
					v = 0
				}
				if v > 100 {
					v = 100
				}
				cores = append(cores, v)
			}
			sample.CPUCores = cores
		}
		sample.MemUsedBytes = h.MemUsedBytes
		sample.MemTotalBytes = h.MemTotalBytes
		sample.SwapUsedBytes = h.SwapUsedBytes
		sample.SwapTotalBytes = h.SwapTotalBytes
		sample.Load1 = h.Load1
		sample.Load5 = h.Load5
		sample.Load15 = h.Load15
		sample.CPUPowerW = nonNegFloatPtr(h.CPUPowerW, "cpu_power_w")
		sample.SystemPowerW = nonNegFloatPtr(h.SystemPowerW, "system_power_w")
		sample.CPUTempC = nonNegFloatPtr(h.CPUTempC, "cpu_temp_c")
		for _, n := range h.Net {
			if n.RxBytes < 0 || n.TxBytes < 0 {
				return routing.TelemetrySample{}, fmt.Errorf("net counters must be non-negative")
			}
			sample.Net = append(sample.Net, routing.NetSample{
				Name:    strings.TrimSpace(n.Name),
				RxBytes: n.RxBytes,
				TxBytes: n.TxBytes,
			})
		}
	}
	for _, g := range req.GPUs {
		if err := nonNegFloat(g.UtilPct, "gpu util_pct"); err != nil {
			return routing.TelemetrySample{}, err
		}
		if err := nonNegFloat(g.PowerW, "gpu power_w"); err != nil {
			return routing.TelemetrySample{}, err
		}
		if err := nonNegFloat(g.FanPct, "gpu fan_pct"); err != nil {
			return routing.TelemetrySample{}, err
		}
		if g.MemUsedBytes < 0 || g.MemTotalBytes < 0 {
			return routing.TelemetrySample{}, fmt.Errorf("gpu memory counters must be non-negative")
		}
		sample.GPUs = append(sample.GPUs, routing.GPUSample{
			Index:         g.Index,
			Name:          strings.TrimSpace(g.Name),
			UUID:          strings.TrimSpace(g.UUID),
			UtilPct:       g.UtilPct,
			MemUsedBytes:  g.MemUsedBytes,
			MemTotalBytes: g.MemTotalBytes,
			TempC:         clampNonNeg(g.TempC),
			VRAMTempC:     clampNonNeg(g.VRAMTempC),
			PowerW:        g.PowerW,
			FanPct:        g.FanPct,
		})
	}
	return sample, nil
}

// deriveRoutingSummary folds the rich host/GPU sample into the legacy routing
// summary fields so the persisted server_telemetry row and the scorer see the
// same shape as before the rich section existed. It runs only when the request
// carries a rich host section (req.Host != nil); a legacy-only payload is left
// verbatim so caller-supplied cpu_load / vram_* / ram_* values are preserved.
func deriveRoutingSummary(req *agentTelemetryRequest) {
	if req.Host == nil {
		return
	}
	req.CPULoad = req.Host.CPUUtilPct / 100
	req.RAMUsedBytes = req.Host.MemUsedBytes
	req.RAMTotalBytes = req.Host.MemTotalBytes
	if len(req.GPUs) > 0 {
		req.GPUCount = len(req.GPUs)
		var vramUsed, vramTotal int64
		for _, g := range req.GPUs {
			vramUsed += g.MemUsedBytes
			vramTotal += g.MemTotalBytes
		}
		req.VRAMUsedBytes = vramUsed
		req.VRAMTotalBytes = vramTotal
	}
}

func telemetryFromRequest(req agentTelemetryRequest, _ json.RawMessage, now time.Time) (routing.ServerTelemetry, error) {
	req.ServerID = strings.TrimSpace(req.ServerID)
	// server_id is token-derived: the sole caller (handleAgentTelemetry) sets
	// req.ServerID from the authenticated agent token before calling this, so
	// this guard is a defensive backstop that no longer fires on the normal
	// path. The request body no longer drives the target server.
	if req.ServerID == "" {
		return routing.ServerTelemetry{}, fmt.Errorf("server_id is required")
	}
	if req.ReportedAt.IsZero() {
		req.ReportedAt = now
	}
	deriveRoutingSummary(&req)
	if req.ActiveRequests < 0 || req.QueueDepth < 0 || req.LatencyMS < 0 || req.GPUCount < 0 {
		return routing.ServerTelemetry{}, fmt.Errorf("numeric counters must be non-negative")
	}
	if req.RAMUsedBytes < 0 || req.RAMTotalBytes < 0 || req.VRAMUsedBytes < 0 || req.VRAMTotalBytes < 0 {
		return routing.ServerTelemetry{}, fmt.Errorf("memory counters must be non-negative")
	}
	if math.IsNaN(req.CPULoad) || math.IsInf(req.CPULoad, 0) || req.CPULoad < 0 {
		return routing.ServerTelemetry{}, fmt.Errorf("cpu_load must be non-negative")
	}
	if math.IsNaN(req.ErrorRate) || math.IsInf(req.ErrorRate, 0) || req.ErrorRate < 0 || req.ErrorRate > 1 {
		return routing.ServerTelemetry{}, fmt.Errorf("error_rate must be between 0 and 1")
	}
	providerHealth, err := compactRawJSON(req.ProviderHealth, "{}")
	if err != nil {
		return routing.ServerTelemetry{}, fmt.Errorf("provider_health must be valid JSON")
	}
	capabilities, err := compactRawJSON(req.Capabilities, "{}")
	if err != nil {
		return routing.ServerTelemetry{}, fmt.Errorf("capabilities must be valid JSON")
	}
	rawSummary, err := sanitizedTelemetryRawSummary(req, providerHealth, capabilities)
	if err != nil {
		return routing.ServerTelemetry{}, fmt.Errorf("telemetry must be valid JSON")
	}
	return routing.ServerTelemetry{
		ServerID:       req.ServerID,
		ReportedAt:     req.ReportedAt,
		AgentVersion:   strings.TrimSpace(req.AgentVersion),
		OS:             strings.TrimSpace(req.OS),
		Arch:           strings.TrimSpace(req.Arch),
		CPULoad:        req.CPULoad,
		RAMUsedBytes:   req.RAMUsedBytes,
		RAMTotalBytes:  req.RAMTotalBytes,
		GPUCount:       req.GPUCount,
		VRAMUsedBytes:  req.VRAMUsedBytes,
		VRAMTotalBytes: req.VRAMTotalBytes,
		ActiveRequests: req.ActiveRequests,
		QueueDepth:     req.QueueDepth,
		LatencyMS:      req.LatencyMS,
		ErrorRate:      req.ErrorRate,
		ProviderHealth: providerHealth,
		Capabilities:   capabilities,
		RawSummary:     rawSummary,
		UpdatedAt:      now,
	}, nil
}

func sanitizedTelemetryRawSummary(req agentTelemetryRequest, providerHealth string, capabilities string) (string, error) {
	summary := struct {
		ServerID       string          `json:"server_id"`
		ReportedAt     time.Time       `json:"reported_at"`
		AgentVersion   string          `json:"agent_version"`
		OS             string          `json:"os"`
		Arch           string          `json:"arch"`
		CPULoad        float64         `json:"cpu_load"`
		RAMUsedBytes   int64           `json:"ram_used_bytes"`
		RAMTotalBytes  int64           `json:"ram_total_bytes"`
		GPUCount       int             `json:"gpu_count"`
		VRAMUsedBytes  int64           `json:"vram_used_bytes"`
		VRAMTotalBytes int64           `json:"vram_total_bytes"`
		ActiveRequests int             `json:"active_requests"`
		QueueDepth     int             `json:"queue_depth"`
		LatencyMS      int             `json:"latency_ms"`
		ErrorRate      float64         `json:"error_rate"`
		ProviderHealth json.RawMessage `json:"provider_health"`
		Capabilities   json.RawMessage `json:"capabilities"`
	}{
		ServerID:       req.ServerID,
		ReportedAt:     req.ReportedAt,
		AgentVersion:   strings.TrimSpace(req.AgentVersion),
		OS:             strings.TrimSpace(req.OS),
		Arch:           strings.TrimSpace(req.Arch),
		CPULoad:        req.CPULoad,
		RAMUsedBytes:   req.RAMUsedBytes,
		RAMTotalBytes:  req.RAMTotalBytes,
		GPUCount:       req.GPUCount,
		VRAMUsedBytes:  req.VRAMUsedBytes,
		VRAMTotalBytes: req.VRAMTotalBytes,
		ActiveRequests: req.ActiveRequests,
		QueueDepth:     req.QueueDepth,
		LatencyMS:      req.LatencyMS,
		ErrorRate:      req.ErrorRate,
		ProviderHealth: json.RawMessage(providerHealth),
		Capabilities:   json.RawMessage(capabilities),
	}
	raw, err := json.Marshal(summary)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

func compactRawJSON(raw json.RawMessage, fallback string) (string, error) {
	if len(raw) == 0 {
		return fallback, nil
	}
	var compacted bytes.Buffer
	if err := json.Compact(&compacted, raw); err != nil {
		return "", err
	}
	return compacted.String(), nil
}
