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
	// Runtimes reports the live state of every agent-managed model process
	// (agent-runtime-manager Task 9): one entry per running/starting/stopped
	// spec, published to RuntimeStatusRegistry for the portal's live SSE
	// stream, plus the per-GPU measured VRAM this sample carries (written
	// back to the store -- see writeBackRuntimeVRAM). Additive: a legacy
	// payload without it decodes with a nil slice, which publishes an empty
	// status snapshot -- never an error.
	Runtimes []agentRuntimeSample `json:"runtimes"`
}

// agentRuntimeGPUSample is one GPU's measured VRAM inside an
// agentRuntimeSample, the gateway-side mirror of the agent's per-runtime GPU
// sample (agent-runtime-manager Task 9). It has TWO independent consumers, and
// they answer different questions: the VRAM write-back
// (writeBackRuntimeVRAM) persists it onto the spec's GPU row as the durable
// value admission reads, and runtimeStatusDTOsFromSamples republishes it on
// the volatile status stream together with the gateway's arrival time -- the
// only place a reader can learn HOW OLD a measurement is, since the stored
// row carries no timestamp (see RuntimeStatusDTO.GPUs/MeasuredAt).
type agentRuntimeGPUSample struct {
	Index          int `json:"index"`
	VRAMMeasuredMB int `json:"vram_measured_mb"`
}

// agentRuntimeError is one managed process's last failure, as reported inside
// an agentRuntimeSample. StderrTail is clamped to maxRuntimeStderrTail bytes
// on ingest -- volatile only (see runtime_registry.go's runtimeStatusRegistry
// doc): a chatty model server's stderr can carry prompt fragments, so this
// value is NEVER persisted to the database, only held in the in-memory
// status registry.
type agentRuntimeError struct {
	Message    string    `json:"message"`
	At         time.Time `json:"at"`
	ExitCode   int       `json:"exit_code"`
	Failures   int       `json:"failures"`
	StderrTail string    `json:"stderr_tail,omitempty"`
}

// agentRuntimeSample is one agent-managed model process's live state inside
// the telemetry sample (agent-runtime-manager Task 9, design spec §7/§9):
// state machine phase, OS-level identifiers, in-flight/restart counters, and
// the last-error detail that back the portal's live runtime status stream.
// GPUs (measured VRAM) has two consumers -- the store write-back
// (writeBackRuntimeVRAM) and the status stream's watermark -- see
// agentRuntimeGPUSample's doc. SpecID ties it back to the launch spec
// (runtime-config's AgentRuntimeSpecDTO.ID) the gateway itself handed the
// agent, so there is no ambiguity about which mapping/model this entry
// describes even when the agent has not (yet) resolved Model.
type agentRuntimeSample struct {
	SpecID    string                  `json:"spec_id"`
	Model     string                  `json:"model"`
	State     string                  `json:"state"`
	Since     time.Time               `json:"since"`
	PID       int                     `json:"pid,omitempty"`
	Port      int                     `json:"port,omitempty"`
	InFlight  int                     `json:"in_flight"`
	Restarts  int                     `json:"restarts"`
	GPUs      []agentRuntimeGPUSample `json:"gpus,omitempty"`
	LastError *agentRuntimeError      `json:"last_error,omitempty"`
}

// maxRuntimeStderrTail bounds agentRuntimeError.StderrTail on ingest (Task 9
// brief): a chatty/hostile agent must never be able to grow the in-memory
// status registry's per-server footprint without bound. Byte-based, like
// clampHardwareString elsewhere in this file (a truncation mid multi-byte
// rune is an acceptable trade-off for a diagnostic tail, not user content).
const maxRuntimeStderrTail = 2048

// clampRuntimeStderrTail truncates an over-long stderr tail to
// maxRuntimeStderrTail bytes.
func clampRuntimeStderrTail(s string) string {
	if len(s) > maxRuntimeStderrTail {
		return s[:maxRuntimeStderrTail]
	}
	return s
}

// runtimeStatusDTOsFromSamples maps the wire-decoded runtime samples to the
// registry's RuntimeStatusDTO, clamping each LastError's stderr tail and
// always returning a non-nil slice (a nil req.Runtimes -- a legacy agent, or
// simply a fleet with nothing managed yet -- must publish an EMPTY snapshot,
// not a JSON null, to any live SSE subscriber).
//
// receivedAt is the GATEWAY's own arrival time for this sample, stamped onto
// every entry that actually carries a measurement as
// RuntimeStatusDTO.MeasuredAt -- see that field for why the watermark cannot
// come from the store, and why it is deliberately not the agent's
// self-reported reported_at.
func runtimeStatusDTOsFromSamples(samples []agentRuntimeSample, receivedAt time.Time) []RuntimeStatusDTO {
	out := make([]RuntimeStatusDTO, 0, len(samples))
	for _, rt := range samples {
		dto := RuntimeStatusDTO{
			SpecID:   rt.SpecID,
			Model:    rt.Model,
			State:    rt.State,
			Since:    rt.Since,
			PID:      rt.PID,
			Port:     rt.Port,
			InFlight: rt.InFlight,
			Restarts: rt.Restarts,
		}
		// A measured 0 is UNKNOWN, not a real zero -- the same `<= 0` rule
		// writeBackRuntimeVRAM applies to this very array on the store side.
		// Dropping it here keeps GPUs (and therefore MeasuredAt) absent rather
		// than publishing a fresh-looking nothing.
		for _, gpu := range rt.GPUs {
			if gpu.VRAMMeasuredMB <= 0 {
				continue
			}
			dto.GPUs = append(dto.GPUs, RuntimeGPUStatusDTO(gpu))
		}
		if len(dto.GPUs) > 0 {
			dto.MeasuredAt = receivedAt
		}
		if rt.LastError != nil {
			dto.LastError = &RuntimeErrorDTO{
				Message:    rt.LastError.Message,
				At:         rt.LastError.At,
				ExitCode:   rt.LastError.ExitCode,
				Failures:   rt.LastError.Failures,
				StderrTail: clampRuntimeStderrTail(rt.LastError.StderrTail),
			}
		}
		out = append(out, dto)
	}
	return out
}

// maxRuntimeSamplesPerSample bounds how many entries of a telemetry sample's
// runtimes array the VRAM write-back loop will process. Nothing else caps
// this array's length, and within the 1 MiB readRawJSON body cap a minimal
// runtime entry is only ~55 bytes on the wire -- uncapped, a single POST
// could drive on the order of 19,000 RuntimeSpecByID/resolution attempts on
// an endpoint agents hit every second. Clamp, don't reject -- mirrors
// maxHardwareGPUs/maxHardwareModules elsewhere in this file. Only the
// write-back loop is bounded here; runtimeStatusDTOsFromSamples' status
// publish is a pure in-memory transform with no store fan-out per entry, so
// it is not the concern this constant exists for.
const maxRuntimeSamplesPerSample = 256

// maxRuntimeGPUsPerSample bounds how many per-GPU measured-VRAM entries one
// runtime sample's GPUs array will drive a store write for. A real server
// has at most a handful of GPUs; this is generous headroom (matching
// maxHardwareGPUs' magnitude), not a realistic ceiling -- it exists so ONE
// resolved-writable spec_id cannot alone drive unbounded
// UpdateRuntimeSpecGPUMeasured writes (maxRuntimeSamplesPerSample only
// bounds the number of DISTINCT/total sample entries considered, not the
// GPU fan-out within a single one).
const maxRuntimeGPUsPerSample = 64

// resolveRuntimeSpecWritable reports whether specID's measured VRAM may be
// written back for THIS sample, reached from server serverID. Three
// conditions must all hold:
//
//  1. The spec exists (RuntimeSpecByID's ok).
//  2. Its owning application belongs to serverID -- resolved via
//     spec.MappingID -> MappingByID -> mapping.ApplicationID ->
//     ApplicationByID -> application.ServerID. This is the authorization
//     check: spec_id is an agent-supplied body field with no other
//     verification anywhere in this path, and the connected agent's token
//     binds it to exactly one server (every other agent endpoint in this
//     package resolves its target SOLELY from the token, never from a body
//     parameter -- see handleAgentRuntimeConfig's doc). Without this check
//     an agent authenticated for server A could name a spec_id belonging to
//     server B and overwrite B's measured VRAM -- which is not
//     display-only: agentRuntimeSpecDTO prefers the measured value over the
//     operator's estimate when building the vram_mb the gateway later
//     pushes to B's OWN agent, so a forged value would corrupt the
//     admission arithmetic B's agent runs against a spec it never reported
//     on.
//  3. It is not VRAMLocked (vram_estimate_mb is operator-owned,
//     vram_measured_mb is agent-owned, and VRAMLocked is the operator's
//     opt-out of being governed by the measurement -- it stops the write
//     here AND makes agentRuntimeSpecDTO serve the estimate, which together
//     are what let an operator recover a spec a measurement has made
//     terminally not_permitted).
//
// The ownership check (2) is evaluated UNCONDITIONALLY, before the
// VRAMLocked check (3) -- deliberately, so the audit-trail Warn below fires
// for every genuine cross-server naming attempt regardless of whether the
// targeted spec happens to be locked. Checking VRAMLocked first would let a
// locked spec's cross-server mismatch return false silently, leaving no
// record of exactly the attack this method exists to catch. Still exactly
// ONE resolution pass per call (RuntimeSpecByID + MappingByID +
// ApplicationByID, at most) -- reordering costs nothing extra.
//
// Any failure to resolve (a lookup error, or a spec/mapping/application
// that no longer exists) is treated the same as "not writable" -- logged and
// skipped, never propagated -- matching the "a report is evidence, not a
// transaction" best-effort discipline this whole file follows. A
// cross-server mismatch is logged at Warn (not Debug): unlike a merely
// stale id, it is a signal an agent is naming another server's resources.
func (s *Server) resolveRuntimeSpecWritable(ctx context.Context, serverID, specID string) bool {
	spec, ok, err := s.Routes.RuntimeSpecByID(ctx, specID)
	if err != nil {
		slog.Debug("runtime vram write-back: spec lookup failed", "server_id", serverID, "spec_id", specID, "err", err)
		return false
	}
	if !ok {
		// The spec has since been deleted (or never existed); nothing to
		// write the measurement back to. Not an error.
		return false
	}
	mapping, err := s.Routes.MappingByID(ctx, spec.MappingID)
	if err != nil {
		slog.Debug("runtime vram write-back: mapping lookup failed", "server_id", serverID, "spec_id", specID, "mapping_id", spec.MappingID, "err", err)
		return false
	}
	app, err := s.Routes.ApplicationByID(ctx, mapping.ApplicationID)
	if err != nil {
		slog.Debug("runtime vram write-back: application lookup failed", "server_id", serverID, "spec_id", specID, "application_id", mapping.ApplicationID, "err", err)
		return false
	}
	if app.ServerID != serverID {
		// Checked BEFORE VRAMLocked below: this Warn must fire for a
		// cross-server naming attempt EVEN when the targeted spec happens to
		// be locked -- see the doc above.
		slog.Warn("runtime vram write-back rejected: spec belongs to a different server", "server_id", serverID, "spec_id", specID, "owner_server_id", app.ServerID)
		return false
	}
	if spec.VRAMLocked {
		return false // the operator pinned this spec's VRAM numbers
	}
	return true
}

// writeBackRuntimeVRAM writes each sample GPU's measured VRAM back to its
// launch spec (agent-runtime-manager Task 9), but only for a spec
// resolveRuntimeSpecWritable confirms belongs to serverID, is not
// VRAMLocked, and still exists. That resolution happens with exactly ONE
// set of reads (RuntimeSpecByID + MappingByID + ApplicationByID) per
// DISTINCT spec_id in the sample: the outcome -- writable or not, for
// WHATEVER reason -- is memoized in writable below, so a sample repeating
// the same spec_id (writable or not) never re-resolves it. runtimes and
// each entry's GPUs are both length-capped (maxRuntimeSamplesPerSample,
// maxRuntimeGPUsPerSample) before any store call, bounding the worst case
// regardless of how many distinct ids a hostile/buggy sample names.
//
// AN UNCHANGED MEASUREMENT IS NOT REWRITTEN, and that is a cost fix rather
// than a tidiness one. Telemetry arrives once per second and every sample is
// a FULL SNAPSHOT, so a spec whose measurement is merely stable -- the normal
// state of a loaded model serving nothing -- used to drive one unconditional
// UPDATE per second per (spec, gpu), indefinitely. An idle overnight server
// with a handful of measured specs across two cards produced on the order of
// a million identical UPDATEs a day: WAL growth on SQLite, dead-tuple churn
// and autovacuum pressure on PostgreSQL, for a table with a dozen rows.
//
// The comparison is against WHAT IS STORED, read once per distinct writable
// spec_id and memoized next to the writability verdict, rather than against
// what the agent last sent. Suppressing at the agent would be cheaper still
// (it would save the report as well as the write) but it cannot converge: the
// stored row can change out from under a long-running agent -- an operator
// deleting and re-adding a GPU row resets vram_measured_mb to 0 -- and an
// agent that had suppressed its unchanged report would never resend, leaving
// the portal showing 0 for a spec that is measured and running. Comparing
// here costs one extra read per writable spec per sample and converges no
// matter what happened to the row; a read is also far cheaper than the write
// it replaces on both engines.
//
// Best-effort throughout, matching the "a report is evidence, not a
// transaction" ingest discipline this whole file follows: nothing here is
// ever returned as an error -- this must NEVER reject the telemetry sample
// it rode in on. A failed RuntimeSpecGPUs read degrades to "write
// unconditionally", never to "skip the write": staleness must not be able to
// suppress a real measurement. Called only AFTER every store write in
// ingestTelemetrySample has succeeded.
func (s *Server) writeBackRuntimeVRAM(ctx context.Context, serverID string, runtimes []agentRuntimeSample) {
	if s.Routes == nil {
		return
	}
	if len(runtimes) > maxRuntimeSamplesPerSample {
		runtimes = runtimes[:maxRuntimeSamplesPerSample]
	}
	writable := make(map[string]bool, len(runtimes))
	// stored[specID][gpuIndex] is the measured value already on file. Only
	// populated for a writable spec, and only once per distinct spec_id.
	stored := make(map[string]map[int]int, len(runtimes))
	for _, rt := range runtimes {
		specID := strings.TrimSpace(rt.SpecID)
		if specID == "" || len(rt.GPUs) == 0 {
			continue
		}
		ok, seen := writable[specID]
		if !seen {
			ok = s.resolveRuntimeSpecWritable(ctx, serverID, specID)
			writable[specID] = ok
			if ok {
				stored[specID] = s.storedMeasuredVRAM(ctx, serverID, specID)
			}
		}
		if !ok {
			continue
		}
		gpus := rt.GPUs
		if len(gpus) > maxRuntimeGPUsPerSample {
			gpus = gpus[:maxRuntimeGPUsPerSample]
		}
		for _, g := range gpus {
			if g.VRAMMeasuredMB <= 0 {
				continue
			}
			if was, known := stored[specID][g.Index]; known && was == g.VRAMMeasuredMB {
				continue // already on file, byte for byte
			}
			if err := s.Routes.UpdateRuntimeSpecGPUMeasured(ctx, specID, g.Index, g.VRAMMeasuredMB); err != nil {
				// Tolerates ErrNotFound (a GPU row deleted out from under an
				// in-flight sample) the same as any other failure here: log
				// and move on, never reject the sample.
				slog.Debug("runtime vram write-back failed", "server_id", serverID, "spec_id", specID, "gpu_index", g.Index, "err", err)
				continue
			}
			if stored[specID] != nil {
				// Keep the memo truthful for the rest of THIS sample: a
				// malformed payload naming the same (spec, gpu) twice must
				// not write twice.
				stored[specID][g.Index] = g.VRAMMeasuredMB
			}
		}
	}
}

// storedMeasuredVRAM reads specID's currently-stored measured value per GPU
// index, for writeBackRuntimeVRAM's change detection. A read failure returns
// nil, which the caller reads as "nothing known" and therefore writes
// unconditionally -- the safe direction: a missed comparison costs one
// redundant UPDATE, whereas a wrongly-assumed match would silently drop a
// real measurement.
func (s *Server) storedMeasuredVRAM(ctx context.Context, serverID, specID string) map[int]int {
	gpus, err := s.Routes.RuntimeSpecGPUs(ctx, specID)
	if err != nil {
		slog.Debug("runtime vram write-back: current gpu rows unreadable, writing unconditionally", "server_id", serverID, "spec_id", specID, "err", err)
		return nil
	}
	out := make(map[int]int, len(gpus))
	for _, g := range gpus {
		out[g.GPUIndex] = g.VRAMMeasuredMB
	}
	return out
}

// ProxyRouteSample is the gateway-side mirror of the agent's
// sample.ProxyRouteSample wire type — no certificate material and no upstream
// address, just the listen port, whether TLS is currently active on it, and
// (State) WHY when it is not.
//
// State is the agent's own proxy.RouteState vocabulary relayed verbatim:
// "pending_leaf", "invalid_upstream", "pending_bind_host", "bind_failed",
// "active". The gateway does not interpret it — it carries it to the operator,
// who is the one who can act on the difference between "no certificate yet"
// and "something else already holds that port". It was dropped at this
// boundary until the https-auto-switch stopped reverting to plaintext on
// tls_active=false: once the gateway declines to downgrade, the reason the
// listener is down is the whole content of the alert it raises instead.
//
// omitempty on the agent side means an older agent simply reports no state;
// every consumer treats "" as "not reported" rather than as a distinct cause.
type ProxyRouteSample struct {
	Listen    int    `json:"listen"`
	TLSActive bool   `json:"tls_active"`
	State     string `json:"state,omitempty"`
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
	// PCIBusID is the card's PCI address (e.g. "00000000:65:00.0"), NVIDIA
	// only. Additive and optional: an older agent omits it and the field
	// decodes empty. Display and disambiguation only -- the portal shows it
	// to tell 4x/8x identical cards apart, and nothing in this codebase
	// matches or keys on it (GPU identity is the index; see the agent's
	// sample.GPU.PCIBusID).
	PCIBusID string `json:"pci_bus_id,omitempty"`
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
	// Record the agent's declared feature set (design spec §9, feature
	// negotiation), so a later portal runtime-spec write's PushRuntimeConfig
	// knows whether this connected agent understands a runtime_config frame
	// at all. Deliberately AFTER every store write succeeded, mirroring
	// AgentCertReports/AgentProxyStatus above: a report is evidence about
	// this agent's own binary, and stamping it while the sample itself
	// failed to persist would claim freshness the gateway does not have.
	// Tolerant: a malformed capabilities blob yields an empty feature set
	// (PushRuntimeConfig then correctly withholds delivery) rather than
	// rejecting the whole sample -- see parseAgentCapabilities.
	s.AgentFeatures.Set(serverID, parseAgentCapabilities(req.Capabilities))
	// Publish the agent-managed runtime status snapshot (agent-runtime-manager
	// Task 9) to the volatile status registry the portal's SSE stream reads.
	// Deliberately AFTER every store write succeeded, mirroring every other
	// registry update in this block: a report is evidence about what the
	// agent is running RIGHT NOW, and stamping it while the sample itself
	// failed to persist would claim freshness the gateway does not have.
	s.RuntimeStatus.publish(serverID, runtimeStatusDTOsFromSamples(req.Runtimes, now))
	// Best-effort write-back of each managed process's measured VRAM onto its
	// launch spec (skipped for a VRAMLocked spec) -- see writeBackRuntimeVRAM.
	// Never rejects the sample; a failure here is logged and dropped.
	s.writeBackRuntimeVRAM(ctx, serverID, req.Runtimes)
	s.maybeFireReactivation(ctx, server)
	return nil
}

// agentCapabilitiesReport is the tolerant subset of an agent's telemetry
// capabilities object this gateway currently understands (design spec §9,
// feature negotiation): the feature names it declares support for. Any other
// keys an agent may additionally carry here are ignored, not rejected --
// forward compatibility with a future agent build that adds fields must
// never break ingest on today's gateway.
type agentCapabilitiesReport struct {
	Features []string `json:"features"`
}

// parseAgentCapabilities tolerantly extracts the declared feature list from a
// raw telemetry capabilities object. Absent, malformed, or wrong-shaped JSON
// (not an object, or a "features" that is not a string array) all yield a
// nil feature set rather than an error -- a capabilities parse failure must
// NEVER reject the telemetry sample it rode in on (see the call site in
// ingestTelemetrySample): a garbled or forward-incompatible capabilities blob
// from a future agent build must not stop routing telemetry from reaching
// the gateway.
func parseAgentCapabilities(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	var report agentCapabilitiesReport
	if err := json.Unmarshal(raw, &report); err != nil {
		return nil
	}
	return report.Features
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
		r.GPUs[i].PCIBusID = clampHardwareString(r.GPUs[i].PCIBusID)
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
