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
	"slices"
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
	// RuntimeConfigAppliedETag is the ACKNOWLEDGEMENT: the ETag of the
	// runtime-config document this agent has actually APPLIED -- reconciled,
	// not merely fetched. It is the one thing the push/poll protocol never
	// had, and it exists so a gateway-side write can be PROVED to have landed
	// instead of inferred from the absence of a process.
	//
	// Its meaning rests entirely on the ETag being a DETERMINISTIC FUNCTION OF
	// CONTENT (portal.agentRuntimeConfigETag hashes the document with the ETag
	// field blanked), so the gateway can derive the exact value an agent
	// holding a given document must report and compare for equality. That also
	// makes the field unforgeable in the only direction that matters: the
	// comparison is against a digest the gateway computed itself, so no value
	// an agent invents can confirm a document it is not holding.
	//
	// Reported by an agent that declares runtimeConfigAckFeature. Additive and
	// optional: an older agent sends nothing, which reads as "no
	// acknowledgement" and makes the gateway fall back rather than hang --
	// see runtimeConfigAckFeature for why the NAME, not this field's presence,
	// is what the gateway gates on.
	RuntimeConfigAppliedETag string `json:"runtime_config_applied_etag"`
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

// agentRuntimeCapabilityVerdict is one capability's answer inside an
// agentRuntimeCapabilitiesSample -- the gateway-side mirror of the agent's
// sample.CapabilityVerdict, field-for-field and JSON-tag-for-JSON-tag
// identical, but its OWN type: the gateway and server-agent are separate Go
// modules and cannot share code, mirroring routing.CapabilityRow already
// doing the same on the store side. Verdict is "yes" or "no" and
// nothing else -- an undetermined capability has NO entry, mirroring the
// store's row-absence-means-unknown model.
type agentRuntimeCapabilityVerdict struct {
	Name    string `json:"name"`
	Verdict string `json:"verdict"`
}

// agentRuntimeCapabilitiesSample is the gateway-side mirror of the agent's
// sample.Capabilities wire payload (server-agent/internal/sample), one
// managed child's auto-detected capability verdict set (#49 sub-project 2).
// Field-for-field and JSON-tag-for-JSON-tag identical to the agent's type,
// but its OWN type: the gateway and server-agent are separate Go modules and
// cannot share code, mirroring routing.CapabilityRow already doing the
// same on the store side.
//
// Verdicts is a keyed LIST, an OPEN vocabulary: a capability name this
// binary has never heard of decodes and is carried through unchanged rather
// than dropped -- the same forward-compatibility rule parseAgentCapabilities
// already documents for the declared-feature list.
//
// A capability this gateway has no constant for is carried onto its own row
// unchanged (capabilityRows below), rather than being folded into a
// mapping-wide "extra" list the way the pre-78 columns had to: one row per
// capability is what makes the open vocabulary storable at all.
type agentRuntimeCapabilitiesSample struct {
	Verdicts []agentRuntimeCapabilityVerdict `json:"verdicts"`
	// Source is the agent's own name for the PROBE that produced this
	// verdict set (#54) -- the gateway-side mirror of
	// sample.Capabilities.Source, whose doc carries the full reasoning. The
	// producer reports its provenance; this module does NOT re-derive it
	// from the spec type it pushed, even though it could: that would put
	// the agent's branch condition in a second module where the two can
	// drift, and this subsystem exists because a shared, inferred
	// provenance was wrong.
	//
	// It is the WRAPPER's field, not each verdict's: one probe reads one
	// document, so one source describes the whole set.
	//
	// Agent-supplied and therefore NOT trusted as written -- see rowSource,
	// which is the only thing that may turn it into a row's Source.
	Source string `json:"source,omitempty"`
}

// rowSource resolves the source EVERY row this runtime entry's probe pass
// may offer is stamped with, and decides whether the pass may be written at
// all. The bool is that decision: false means write nothing from this entry.
//
// A nil receiver or an EMPTY source keeps the historical default,
// CapabilitySourceLlamaCppProps, and that default is SAFE rather than merely
// convenient: the only producers that leave the field empty are agents that
// predate it, and such an agent probes capabilities by GETting /props. An
// Ollama child answers that with a 404 (it serves no /props at all), which
// the agent treats as a conclusive nothing -- no verdicts, therefore no rows
// at all. So the default can only ever apply to a document a llama.cpp
// /props probe actually produced; there is no arrangement of old agent plus
// new gateway in which it mislabels an Ollama-declared verdict.
//
// An UNRECOGNISED source REJECTS the rows instead of clamping them to the
// default, and that is the ruling this function exists to make. Clamping
// would print a provenance nobody reported on the operator-visible column --
// a fabricated attribution, which is precisely the defect the source column
// exists to prevent, and worse than the alternative: a dropped row leaves
// the capability UNKNOWN, which this model expresses natively as the absence
// of a row (see routing.CapabilityRow). Rejecting is also the only option
// that stays safe against the rank: capabilitySourceRank's DEFAULT branch
// ranks an unrecognised source at 1, so a blind write would let an
// unknown-provenance verdict overwrite a real probe's row at equal rank.
//
// The allowlist is exactly the two PROBE sources, which is what makes this a
// trust boundary and not a typo filter: the vocabulary also contains
// CapabilitySourceManual (rank 3) and CapabilitySourceVisionBenchmark
// (rank 2), and an agent has no standing to claim either. A sample that did
// would put "an operator said so" in front of an operator and lock a real
// benchmark out of its own row. Neither reaches a row from here.
//
// It lives in this package rather than in routing on purpose: routing owns
// the store-side RANK, which is about how two sources compare; which sources
// one PRODUCER may claim is a property of this ingest boundary, where the
// agent's bytes arrive.
func (c *agentRuntimeCapabilitiesSample) rowSource() (string, bool) {
	if c == nil {
		return routing.CapabilitySourceLlamaCppProps, true
	}
	switch strings.TrimSpace(c.Source) {
	case "", routing.CapabilitySourceLlamaCppProps:
		return routing.CapabilitySourceLlamaCppProps, true
	case routing.CapabilitySourceOllamaAPIShow:
		return routing.CapabilitySourceOllamaAPIShow, true
	default:
		return "", false
	}
}

// reportedSource is c.Source as it ARRIVED -- untrimmed, unvalidated, and
// safe on a nil receiver. It exists for one caller: the log line that
// reports a source rowSource above refused
// (runtimeSampleCapabilityRows). That line has to name the value the agent
// actually sent, so it cannot use rowSource's return (which is "" for
// exactly the case being logged) and must not trim, since a source that
// differs from a valid one only in whitespace is worth seeing as it came.
//
// A METHOD rather than a field read at the call site, because the call site
// was safe only by an invariant enforced one function away: rowSource
// defaults the nil receiver to a VALID source, so a nil c can never reach
// the rejection branch, so the deref there could never fire. That is true,
// and it is true somewhere else -- a mutation to rowSource's first line
// during review turned the branch into a SEGFAULT rather than a failed
// assertion, which is the tell that nothing local protected it. This
// accessor makes the branch correct on its own terms, at no behavioural
// cost: for every input that reaches it today it returns exactly what
// c.Source returned.
func (c *agentRuntimeCapabilitiesSample) reportedSource() string {
	if c == nil {
		return ""
	}
	return c.Source
}

// capabilityRows projects c.Verdicts onto the store's row shape -- the rows
// THIS probe determined, attributed to source (the caller's already-resolved
// rowSource, never c.Source as written) and stamped at, ready for
// routing.WritableCapabilityRows to decide which of them may actually be
// written. A nil receiver (no wire object at all --
// an agent predating capability detection) yields nothing, as does a non-nil
// but empty one (detection ran, determined nothing): two different facts that
// both mean no rows, which is why the wire field is a pointer.
//
// Every name is carried, known or not: an unknown capability name with a
// definitive verdict is a perfectly good row, in EITHER direction. The
// pre-row shim this replaced could only project an unknown name's "yes" onto
// its column-less Extra list and silently dropped an unknown "no" -- there
// was nowhere to put it. With one row per capability there is.
//
// The one exception is applied by the CALLER, not here:
// runtimeSampleCapabilityRows drops the reserved internal names
// (reservedAgentCapabilityNames) out of this projection's output, because it
// is the function holding the spec id that a drop has to be reported
// against.
//
// A verdict that is neither "yes" nor "no" is dropped, and that is the whole
// of this projection's filtering: "" is the wire's "nothing to say yet" (an
// older agent, or a probe with no stable answer), and "unknown" is the
// ABSENCE of a row rather than a third verdict, so there is nothing to write
// for it. Dropping an unrecognised verdict string here rather than passing it
// on matters: UpsertMappingCapabilities is atomic and strict, so one
// malformed verdict handed to it would reject the whole sample's row set.
//
// The Source is REPORTED, not inferred, and that is the resolution of #54's
// open question: since the agent runs TWO capability probes (llama.cpp's
// /props and, for an "ollama"-typed spec, POST /api/show), and this wire
// carries the spec id but no runtime type, the gateway cannot tell the two
// documents apart from the sample alone -- so the producer names its own
// provenance in Source and rowSource validates it. Two alternatives were
// refused: guessing the probe from the row CONTENT (Ollama never reports a
// "no", never a live_progress) is a heuristic a nothing-but-yes /props
// document defeats, and re-deriving routing.EffectiveRuntimeSpecType from
// the spec resolveRuntimeSpecCapabilities already loads would trade the
// reporter's report for an inference from configuration this gateway pushed
// itself, duplicating the agent's branch condition in a second module where
// the two can silently drift.
func (c *agentRuntimeCapabilitiesSample) capabilityRows(source string, at time.Time) []routing.CapabilityRow {
	if c == nil {
		return nil
	}
	out := make([]routing.CapabilityRow, 0, len(c.Verdicts))
	for _, v := range c.Verdicts {
		name := strings.TrimSpace(v.Name)
		verdict := strings.TrimSpace(v.Verdict)
		if name == "" || (verdict != routing.CapabilityYes && verdict != routing.CapabilityNo) {
			continue
		}
		out = append(out, routing.CapabilityRow{
			Capability: name, Verdict: verdict,
			Source: source, CheckedAt: at,
		})
	}
	return out
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
	SpecID         string    `json:"spec_id"`
	Model          string    `json:"model"`
	State          string    `json:"state"`
	Since          time.Time `json:"since"`
	PID            int       `json:"pid,omitempty"`
	Port           int       `json:"port,omitempty"`
	InFlight       int       `json:"in_flight"`
	Restarts       int       `json:"restarts"`
	ContextSize    int       `json:"context_size"`
	ActiveRequests int       `json:"active_requests"`
	QueueDepth     int       `json:"queue_depth"`
	MetricsProbe   string    `json:"metrics_probe"`
	ContextProbe   string    `json:"context_probe"`
	// LiveProgressSupport is this child's build's verdict on the live-progress
	// request parameters (issue #51 / timings-capability-detection Task 4):
	// "" (never determined -- an older agent that predates this field, or a
	// probe that has not yet reached a stable answer), "supported", or
	// "unsupported". Produced by the SAME probeRuntimeChild pass that fills
	// ContextSize/ContextProbe above (server-agent's
	// probeRuntimeChildLiveProgress), so it is additive and byte-neutral for
	// an older agent: the field is simply absent, decoding to "". It has no
	// write-back of its own: it is a capability like any other and rides
	// writeBackRuntimeCapabilities as the routing.CapabilityLiveProgress row
	// (see runtimeSampleCapabilityRows).
	LiveProgressSupport string `json:"live_progress_support"`
	// Capabilities is this child's auto-detected capability verdict set (#49
	// sub-project 2), from the SAME probe pass that fills
	// LiveProgressSupport above (server-agent's probeRuntimeChildProps --
	// llama.cpp's /props, or Ollama's /api/show since #54, which is why the
	// set names its own Source). A
	// POINTER, mirroring the agent's own sample.RuntimeSample.Capabilities
	// *sample.Capabilities field byte-for-byte on the wire: nil distinguishes
	// "this agent predates capability detection" (an older build -- nothing
	// to say) from a non-nil, all-empty value ("detection ran, nothing
	// determined") -- two different facts that both mean no write, but for
	// different reasons, which is exactly why this field is a pointer rather
	// than a bare struct. See writeBackRuntimeCapabilities for the
	// gateway-side write-back, which persists these AND LiveProgressSupport
	// above as one set of model_mapping_capabilities rows.
	Capabilities *agentRuntimeCapabilitiesSample `json:"capabilities,omitempty"`
	GPUs         []agentRuntimeGPUSample         `json:"gpus,omitempty"`
	LastError    *agentRuntimeError              `json:"last_error,omitempty"`
}

// maxAppliedConfigETag bounds the acknowledged runtime-config ETag on ingest.
// The value the gateway itself derives is a 64-character sha256 hex digest, so
// this is generous headroom rather than a fit -- deliberately, because the
// length is NOT the check. What makes a bogus value harmless is that the sole
// consumer compares it against a digest the gateway computed itself, so
// nothing an agent sends can confirm anything it is not holding; this constant
// only stops one chatty or hostile agent from growing the in-memory registry's
// per-server footprint. Byte-based, like maxRuntimeStderrTail below.
//
// Deliberately NOT a format check. Validating "64 lowercase hex" here would
// duplicate the digest's shape in a second module, and the day the ETag's
// derivation changed, every acknowledgement in the fleet would be silently
// discarded -- degrading to the fallback with nothing to point at.
const maxAppliedConfigETag = 256

// runtimeModelProbeFeature is the agent-DECLARED capability name (server-agent
// Task 11, server-agent/internal/agent/features.go) meaning: this agent probes
// each managed child model server for its context window and per-child
// active/queue counts, and reports them per runtime entry rather than (only)
// via the legacy agent-wide scrape. It is NOT one of this gateway's OWN
// advertised features (gatewayAgentFeatures, agent_features.go) -- unlike
// those, this is a capability an agent declares and the gateway merely
// consumes, checked via s.AgentFeatures.Has, the same way runtime_api_token
// (a different agent-declared capability) is handled elsewhere in this
// package. Gates both Piece 1 (writeBackRuntimeContext) and Piece 2 (the
// per-server active/queue aggregate) below.
const runtimeModelProbeFeature = "runtime_model_probe"

// clampAppliedConfigETag trims and bounds one reported applied-config ETag.
func clampAppliedConfigETag(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > maxAppliedConfigETag {
		return s[:maxAppliedConfigETag]
	}
	return s
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
			SpecID:              rt.SpecID,
			Model:               rt.Model,
			State:               rt.State,
			Since:               rt.Since,
			PID:                 rt.PID,
			Port:                rt.Port,
			InFlight:            rt.InFlight,
			Restarts:            rt.Restarts,
			ContextSize:         rt.ContextSize,
			ActiveRequests:      rt.ActiveRequests,
			QueueDepth:          rt.QueueDepth,
			MetricsProbe:        rt.MetricsProbe,
			ContextProbe:        rt.ContextProbe,
			LiveProgressSupport: rt.LiveProgressSupport,
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

// sumRuntimeActiveQueue sums active_requests and queue_depth across every
// runtime entry in a sample, for the per-server ServerTelemetry aggregate a
// multi-model server-agent needs (Task 12, Piece 2): a server-agent that
// reports per-runtime metrics may not also run the legacy agent-wide scrape,
// so the accurate per-server routing figure is the sum across its managed
// processes rather than the (possibly absent/stale) top-level fields.
func sumRuntimeActiveQueue(runtimes []agentRuntimeSample) (active, queue int) {
	for _, rt := range runtimes {
		// A count can never legitimately be negative. Clamp each per-runtime
		// value to >= 0 before summing rather than rejecting the sample: an
		// unclamped negative here would push the sum negative, and that
		// negative aggregate REPLACES the (already validated) top-level
		// telemetry.ActiveRequests/QueueDepth at the call site below, which
		// would poison this server's routing scorer input (validTelemetry
		// treats a negative counter as invalid, excluding the server from ALL
		// routing) -- rejecting instead would break the best-effort
		// runtimes[] discipline this file otherwise holds throughout.
		active += max(rt.ActiveRequests, 0)
		queue += max(rt.QueueDepth, 0)
	}
	return active, queue
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

// resolveRuntimeSpecMapping reports whether specID's owning mapping may have
// its probed context_size written back for THIS sample, reached from server
// serverID -- the context write-back's sibling to resolveRuntimeSpecWritable
// above, resolving the SAME ownership chain (RuntimeSpecByID -> MappingByID
// -> ApplicationByID -> application.ServerID) for the SAME reason: spec_id is
// an agent-supplied body field with no other verification anywhere in this
// path, and an agent authenticated for server A must never be able to name a
// spec_id belonging to server B and overwrite B's mapping metrics.
//
// Deliberately NOT resolveRuntimeSpecWritable itself: that method's third
// gate is VRAMLocked, which governs vram_estimate_mb/vram_measured_mb, not
// context_size -- the wrong lock for this call. The gate that matters here is
// mapping.MetricsLocked (operator-pinned mapping metrics, the same flag
// UpdateMappingContextProbe's own SQL already enforces): checking it here
// too, before ever calling that method, skips a write the store would only
// silently no-op, and lets the change-detection below compare against a
// context value the operator actually intends to keep.
//
// On success returns the mapping id and its CURRENTLY STORED context_size
// (for the caller's change-detection), true. Any failure to resolve -- a
// lookup error, a spec/mapping/application that no longer exists, a
// cross-server mismatch, or a locked mapping -- returns ("", 0, false); a
// cross-server mismatch is logged at Warn (not Debug), matching
// resolveRuntimeSpecWritable's audit-trail discipline for the same reason: an
// agent naming another server's resources is a signal worth keeping, not a
// merely stale id.
func (s *Server) resolveRuntimeSpecMapping(ctx context.Context, serverID, specID string) (mappingID string, storedContext int, ok bool) {
	spec, ok, err := s.Routes.RuntimeSpecByID(ctx, specID)
	if err != nil {
		slog.Debug("runtime context write-back: spec lookup failed", "server_id", serverID, "spec_id", specID, "err", err)
		return "", 0, false
	}
	if !ok {
		// The spec has since been deleted (or never existed); nothing to
		// write the probed context back to. Not an error.
		return "", 0, false
	}
	mapping, err := s.Routes.MappingByID(ctx, spec.MappingID)
	if err != nil {
		slog.Debug("runtime context write-back: mapping lookup failed", "server_id", serverID, "spec_id", specID, "mapping_id", spec.MappingID, "err", err)
		return "", 0, false
	}
	app, err := s.Routes.ApplicationByID(ctx, mapping.ApplicationID)
	if err != nil {
		slog.Debug("runtime context write-back: application lookup failed", "server_id", serverID, "spec_id", specID, "application_id", mapping.ApplicationID, "err", err)
		return "", 0, false
	}
	if app.ServerID != serverID {
		slog.Warn("context write-back rejected: spec belongs to a different server", "server_id", serverID, "spec_id", specID, "owner_server_id", app.ServerID)
		return "", 0, false
	}
	if mapping.MetricsLocked {
		// The operator pinned this mapping's metrics; UpdateMappingContextProbe
		// would no-op anyway -- skip the pointless write (and the store round
		// trip it would cost).
		return "", 0, false
	}
	return mapping.ID, mapping.ContextSize, true
}

// writeBackRuntimeContext writes each sample runtime's probed context window
// back onto its owning mapping's context_size (Task 12, Option B: persist
// only the STABLE context size onto the mapping -- no per-mapping active/queue
// storage, no migration, no new store method; UpdateMappingContextProbe
// already exists and already sets context_size + metrics_source="probe" +
// metrics_updated_at, no-oping on a locked or missing mapping).
//
// Mirrors writeBackRuntimeVRAM's discipline throughout: runtimes is length-capped
// at maxRuntimeSamplesPerSample before any store call; resolution
// (resolveRuntimeSpecMapping) is memoized per DISTINCT spec_id, so a sample
// repeating the same spec_id -- a full snapshot arrives roughly once a second --
// never re-resolves it; and AN UNCHANGED VALUE IS NOT REWRITTEN, comparing
// against the mapping's CURRENTLY STORED context_size (read once per distinct
// writable spec_id, memoized alongside the resolution) rather than against
// whatever this same spec_id reported last sample -- the same reasoning
// writeBackRuntimeVRAM documents at length: without it, a model whose context
// window is simply stable (the normal case) would drive one unconditional
// UPDATE per second per mapping, forever.
//
// Best-effort throughout, matching the "a report is evidence, not a
// transaction" ingest discipline this whole file follows: nothing here is
// ever returned as an error -- this must NEVER reject the telemetry sample it
// rode in on. Called only when the reporting agent declares
// runtimeModelProbeFeature for THIS sample (see the call site in
// ingestTelemetrySample), and only AFTER every store write in
// ingestTelemetrySample has succeeded, mirroring writeBackRuntimeVRAM's own
// placement.
func (s *Server) writeBackRuntimeContext(ctx context.Context, serverID string, runtimes []agentRuntimeSample) {
	if s.Routes == nil {
		return
	}
	if len(runtimes) > maxRuntimeSamplesPerSample {
		runtimes = runtimes[:maxRuntimeSamplesPerSample]
	}
	now := time.Now().UTC()
	type resolution struct {
		mappingID     string
		storedContext int
		ok            bool
	}
	resolved := make(map[string]resolution, len(runtimes))
	for _, rt := range runtimes {
		specID := strings.TrimSpace(rt.SpecID)
		if specID == "" || rt.ContextSize <= 0 {
			continue
		}
		r, seen := resolved[specID]
		if !seen {
			mappingID, storedContext, ok := s.resolveRuntimeSpecMapping(ctx, serverID, specID)
			r = resolution{mappingID: mappingID, storedContext: storedContext, ok: ok}
			resolved[specID] = r
		}
		if !r.ok {
			continue
		}
		if r.storedContext == rt.ContextSize {
			continue // already on file, no write amplification
		}
		if err := s.Routes.UpdateMappingContextProbe(ctx, r.mappingID, rt.ContextSize, now); err != nil {
			slog.Debug("runtime context write-back failed", "server_id", serverID, "spec_id", specID, "mapping_id", r.mappingID, "err", err)
			continue
		}
		// Keep the memo truthful for the rest of THIS sample: a malformed
		// payload naming the same spec_id twice must not write twice.
		r.storedContext = rt.ContextSize
		resolved[specID] = r
	}
}

// resolveRuntimeSpecCapabilities reports whether specID's owning mapping may
// have capability ROWS written for THIS sample, reached from server serverID
// -- the capability write-back's sibling to resolveRuntimeSpecMapping above,
// resolving the SAME ownership chain (RuntimeSpecByID -> MappingByID ->
// ApplicationByID -> application.ServerID) for the SAME reason: spec_id is an
// agent-supplied body field with no other verification anywhere in this path,
// and an agent authenticated for server A must never be able to name a
// spec_id belonging to server B and overwrite B's mapping capability
// verdicts.
//
// Deliberately does NOT check mapping.MetricsLocked, unlike
// resolveRuntimeSpecMapping: model_mapping_capabilities carries no
// metrics_locked guard either -- routing.MappingStore.UpsertMappingCapabilities
// holds the full argument -- because a capability is not a metric an operator
// pins numbers against. An operator who locks a mapping's throughput/context
// figures is answering for THOSE NUMBERS, not vouching for what the upstream
// binary's request schema accepts. An in-Go pre-check here would silently
// re-impose the exact guard the table's own design omits, and would leave a
// locked mapping's capabilities permanently undiscoverable. What the operator
// gets INSTEAD is per-capability provenance -- a manual verdict outranks this
// probe -- and that guard lives in the caller, not the store (see
// routing.WritableCapabilityRows).
//
// On success returns the mapping id and its CURRENTLY STORED capability rows
// keyed by capability -- the baseline the caller judges both the precedence
// rule and change detection against -- and true. Any failure to resolve (a
// lookup error, a spec/mapping/application that no longer exists, a
// cross-server mismatch, or a capability read that failed) returns
// ("", nil, false); a cross-server mismatch is logged at Warn (not Debug),
// matching resolveRuntimeSpecMapping's audit-trail discipline for the same
// reason: an agent naming another server's resources is a signal worth
// keeping, not a merely stale id.
func (s *Server) resolveRuntimeSpecCapabilities(ctx context.Context, serverID, specID string) (mappingID string, stored map[string]routing.CapabilityRow, ok bool) {
	spec, ok, err := s.Routes.RuntimeSpecByID(ctx, specID)
	if err != nil {
		slog.Debug("runtime capabilities write-back: spec lookup failed", "server_id", serverID, "spec_id", specID, "err", err)
		return "", nil, false
	}
	if !ok {
		// The spec has since been deleted (or never existed); nothing to
		// write the reported capabilities back to. Not an error.
		return "", nil, false
	}
	mapping, err := s.Routes.MappingByID(ctx, spec.MappingID)
	if err != nil {
		slog.Debug("runtime capabilities write-back: mapping lookup failed", "server_id", serverID, "spec_id", specID, "mapping_id", spec.MappingID, "err", err)
		return "", nil, false
	}
	app, err := s.Routes.ApplicationByID(ctx, mapping.ApplicationID)
	if err != nil {
		slog.Debug("runtime capabilities write-back: application lookup failed", "server_id", serverID, "spec_id", specID, "application_id", mapping.ApplicationID, "err", err)
		return "", nil, false
	}
	if app.ServerID != serverID {
		slog.Warn("capabilities write-back rejected: spec belongs to a different server", "server_id", serverID, "spec_id", specID, "owner_server_id", app.ServerID)
		return "", nil, false
	}
	rows, err := s.Routes.MappingCapabilities(ctx, mapping.ID)
	if err != nil {
		slog.Debug("runtime capabilities write-back: capability read failed", "server_id", serverID, "spec_id", specID, "mapping_id", mapping.ID, "err", err)
		return "", nil, false
	}
	return mapping.ID, routing.CapabilityRowsByName(rows), true
}

// writeBackRuntimeCapabilities persists what each managed child's probe
// determined about its build as model_mapping_capabilities rows on the owning
// mapping (#49 sub-project 3): one row per capability, each carrying its
// verdict, its SOURCE and when it was established.
//
// This is the ONE write path for everything that pass determines, the open
// Verdicts list AND the live-progress verdict that used to have a writer of
// its own. They were never two facts: both come off the SAME /props document
// in the SAME per-runtime pass, and with a row per capability there is
// nothing left to keep them apart -- live-progress is simply the
// routing.CapabilityLiveProgress row (see runtimeSampleCapabilityRows).
//
// Mirrors writeBackRuntimeContext's discipline throughout: runtimes is
// length-capped at maxRuntimeSamplesPerSample before any store call;
// resolution (resolveRuntimeSpecCapabilities) is memoized per DISTINCT
// spec_id, so a sample repeating the same spec_id -- a full snapshot arrives
// roughly once a second -- never re-resolves it; and best-effort throughout,
// matching the "a report is evidence, not a transaction" ingest discipline
// this whole file follows: nothing here is ever returned as an error -- this
// must NEVER reject the telemetry sample it rode in on. Called only AFTER
// every store write in ingestTelemetrySample has succeeded, and only when the
// reporting agent declares runtimeModelProbeFeature for THIS sample -- the
// verdicts ride the exact same per-runtime probe pass (server-agent's
// probeRuntimeChild) that produces ContextSize, so they share that pass's
// trust boundary: an agent that has never declared runtime_model_probe must
// never have a mapping's stored capability touched from this path either.
//
// Three rulings are this path's own, each deliberate:
//
//  1. PRECEDENCE, the ruling this whole write path exists for: a probe never
//     overwrites a row whose source is AUTHORITATIVE -- a human's manual
//     verdict, or the vision benchmark's real measurement. Re-reading the
//     same /props document once a second must not be able to talk over
//     either. The rule itself lives in routing.WritableCapabilityRows,
//     asked rather than restated, so both probe write paths (this one and
//     cmd/gateway/app_health.go's) cannot drift apart on it.
//  2. NO metrics_locked check, in either direction -- see
//     resolveRuntimeSpecCapabilities' doc and, for the argument itself,
//     routing.MappingStore.UpsertMappingCapabilities. A locked mapping's
//     capabilities still get written; that is the design's central decision,
//     not an oversight to fix later.
//  3. An UNDETERMINED verdict writes NOTHING, and cannot: "unknown" is the
//     ABSENCE of a row, so there is no empty verdict to accidentally write.
//     A nil Capabilities (an agent predating capability detection) and a
//     non-nil empty one (detection ran, determined nothing) are different
//     facts -- which is why the wire field is a pointer -- and both simply
//     yield no rows. An unchanged verdict issues no write either
//     (WritableCapabilityRows' second rule), which matters more for a
//     capability than for a metric: a build capability is stable by nature,
//     so the SAME child build reports the SAME verdict every second for its
//     whole life.
func (s *Server) writeBackRuntimeCapabilities(ctx context.Context, serverID string, runtimes []agentRuntimeSample) {
	if s.Routes == nil {
		return
	}
	if len(runtimes) > maxRuntimeSamplesPerSample {
		runtimes = runtimes[:maxRuntimeSamplesPerSample]
	}
	now := time.Now().UTC()
	// resolved memoizes the ownership resolution per DISTINCT spec_id, and is
	// carried across the loop so a spec_id repeated inside ONE sample sees
	// what the earlier iteration already wrote -- see capabilityResolution.
	resolved := make(map[string]capabilityResolution, len(runtimes))
	for _, rt := range runtimes {
		s.writeBackOneRuntimeCapabilities(ctx, serverID, rt, resolved, now)
	}
}

// writeBackOneRuntimeCapabilities is one runtime sample's half of
// writeBackRuntimeCapabilities: the triage, the memoized ownership
// resolution, the precedence rule, and the write. Split out so the loop above
// reads as what it is -- "do this per runtime" -- and so each guard here sits
// at one nesting level instead of three.
func (s *Server) writeBackOneRuntimeCapabilities(ctx context.Context, serverID string, rt agentRuntimeSample, resolved map[string]capabilityResolution, now time.Time) {
	specID := strings.TrimSpace(rt.SpecID)
	if specID == "" {
		return
	}
	reported := runtimeSampleCapabilityRows(rt, now)
	if len(reported) == 0 {
		// This entry determined nothing at all -- no capability object and no
		// live-progress verdict, or one that carried no definitive answer.
		// Returns BEFORE resolving ownership, so the overwhelmingly common
		// nothing-to-say sample costs no store round trip.
		return
	}
	r, ok := s.resolvedCapabilities(ctx, serverID, specID, resolved)
	if !ok {
		return
	}
	rows := routing.WritableCapabilityRows(reported, r.stored)
	if len(rows) == 0 {
		// Every reported verdict is either already on file or outranked by a
		// human's / a measurement's -- no write amplification, no talking
		// over the operator.
		return
	}
	if err := s.Routes.UpsertMappingCapabilities(ctx, r.mappingID, rows); err != nil {
		slog.Debug("runtime capabilities write-back failed", "server_id", serverID, "spec_id", specID, "mapping_id", r.mappingID, "err", err)
		return
	}
	// Keep the memo truthful for the rest of THIS sample: a malformed payload
	// naming the same spec_id twice must not write twice. r.stored is the
	// memoized map itself, so this updates the memo in place.
	for _, row := range rows {
		r.stored[row.Capability] = row
	}
}

// reservedAgentCapabilityNames are the capability names this codebase
// REASONS ABOUT and that neither capability probe can observe, so their
// appearance in an agent's open verdict list is necessarily either an
// upstream publisher's string or a bug -- never evidence.
//
// The criterion is exactly that, and it is why vision/video/audio/tools are
// NOT here: those four are what the two detectors read out of their
// documents (llama.cpp's modalities + chat_template_caps, Ollama's
// capabilities array), so a probe reporting one of them is reporting what it
// saw. Neither document says anything about either name below:
//
//   - "mtp" is not detected anywhere today. The row comes from the portal --
//     an operator's checkbox (manual) or the model-NAME heuristic
//     (legacy, routing.IsMTPModelName) -- and it feeds scoringRoute's +30
//     bonus through MappingCandidate.IsMTP. A "yes" from a probe would move
//     real routing weight on the strength of a string in a model manifest.
//   - "live_progress" has a dedicated wire field of its own
//     (RuntimeSample.LiveProgressSupport) and that field is the only channel
//     an agent may report it on. Its verdict makes the router send
//     timings_per_token upstream -- which an Ollama child does not
//     understand at all, and for an Ollama child the dedicated field is
//     ALWAYS "", so the "dedicated field wins" ordering below has nothing to
//     win with and a publisher's string would take effect outright.
//
// This gateway-side rule is the load-bearing one, and the reason is the
// threat model rather than tidiness: the agent's own detector skips these
// names too (collector.detectOllamaCapabilities), but that filter protects
// only against a publisher string reaching an HONEST agent's Extra list. A
// buggy or hostile agent puts the name straight into the verdicts it sends,
// where no agent-side filter is in the path at all. This boundary is.
//
// The day a real MTP detector exists it reports through a field this
// codebase defined, the way live-progress support does, or this list changes
// on both sides -- what it must not do is arrive on the OPEN list, whose
// whole purpose is carrying strings no one here has vetted.
var reservedAgentCapabilityNames = map[string]bool{
	routing.CapabilityMTP:          true,
	routing.CapabilityLiveProgress: true,
}

// runtimeSampleCapabilityRows is everything ONE runtime entry determined
// about its child's build, projected onto rows this probe may offer for
// writing: the live-progress verdict, which is a capability like any other
// and gets no writer of its own, plus the open Verdicts list.
//
// The live-progress row goes FIRST, and the open list can no longer contest
// it at all: "live_progress" is a RESERVED name (reservedAgentCapabilityNames
// above), so a verdict carrying it never becomes a row. The order stays as
// the second half of a belt-and-braces pair rather than as the mechanism it
// used to be -- rule 0 of routing.WritableCapabilityRows keeps the first row
// for a capability name and drops every later one, so were the reserved rule
// ever narrowed, the DEDICATED field would still win instead of silently
// losing to a publisher's string.
//
// BOTH row kinds are stamped with the ONE source resolved here, because the
// agent derives both from the same probe pass over the same document
// (server-agent's probeRuntimeChildProps fills LiveProgressSupport and
// Capabilities from a single fetch). A source this gateway does not
// recognise voids the whole pass, live-progress row included: the pass's
// provenance is what was unrecognisable, and no part of it is more
// attributable than the rest.
//
// TWO shapes are refused rather than stamped, and both are refusals of a
// FALSE PROVENANCE that the ollama_api_show source cannot carry, whatever
// the sender intended -- see each refusal below for its own argument:
//
//  1. a live-progress verdict attributed to ollama_api_show, in either
//     direction. Ollama exposes no timings_per_token-style surface at all,
//     so its document is evidence for neither answer.
//  2. ANY capability's "no" verdict attributed to ollama_api_show.
//     Ollama's capability array is not exhaustive, so the detector behind
//     that source can only ever produce "yes" or nothing -- which is the
//     claim routing.CapabilityRow's own source doc makes about every row
//     carrying it, not just about the live-progress one.
//
// Both are one-line rules for the same reason: an invariant a caller can
// violate is not an invariant, and this is the boundary where the agent's
// bytes arrive. Neither voids the pass -- the "yes" verdicts of the same
// document are exactly what /api/show CAN answer, so they still ride it.
//
// A consequence worth stating, because it is a limit on what the tests here
// can show: the live-progress row can now only ever carry llama_cpp_props,
// so reading the source from the report rather than hard-coding llama.cpp's
// name -- still the honest rule, and still the one the verdict rows follow
// -- is no longer distinguishable from the hard-coded name by any input, and
// no test pins it any more.
func runtimeSampleCapabilityRows(rt agentRuntimeSample, at time.Time) []routing.CapabilityRow {
	source, ok := rt.Capabilities.rowSource()
	if !ok {
		// WARN, and the level is load-bearing: this gateway's default log
		// level is info (config.Load's OP_AI_GATEWAY_LOG_LEVEL default), so
		// at Debug the drop is INVISIBLE in every default deployment. A
		// newer agent reporting a third source would lose every capability
		// row it ever sends, and lose it silently -- the rows' absence is
		// how this model spells "unknown", indistinguishable from a probe
		// that never ran, so there would be nothing anywhere to point at.
		//
		// Warn is this file's established level for a rejection that DROPS
		// A WRITE: the three cross-server spec rejections (vram, context,
		// and this very write-back's own, a screen above) and the two
		// telemetry-envelope rejections all use it. Debug here is reserved
		// for a TRANSIENT failure -- a store error, a lookup that failed
		// this once -- which repeats and heals on its own. This one cannot
		// heal: the source names the agent build that sent it, so every
		// sample from that agent is dropped identically until a binary
		// changes. It does repeat, once per sample per spec, and that is
		// accepted on exactly the same footing as the ownership rejection
		// above, which repeats per sample and warns anyway -- a fleet-wide
		// capability blackout is worth a repeated line.
		//
		// reportedSource, not rt.Capabilities.Source: a nil sample cannot
		// reach this branch (rowSource defaults the nil receiver to a valid
		// source), but that invariant lives one function away, so reading
		// the field directly here would be safe only at a distance. The
		// accessor is nil-safe on its own terms and returns the identical
		// value for every input that does reach this branch.
		slog.Warn("runtime capability sample names an unrecognised source, dropping its rows",
			"spec_id", rt.SpecID, "source", rt.Capabilities.reportedSource())
		return nil
	}
	var rows []routing.CapabilityRow
	if verdict := routing.LiveProgressCapabilityVerdict(rt.LiveProgressSupport); verdict != "" {
		if source == routing.CapabilitySourceOllamaAPIShow {
			// REFUSED, not stamped. The combination is one no honest agent
			// produces -- ProbeOllamaVerdicts leaves LiveProgress "" on
			// every one of its return paths -- but a buggy or hostile one
			// can send it, and this is the boundary where the agent's bytes
			// arrive, so "no honest producer does this" is not an invariant
			// here: it is a hope.
			//
			// The refusal is semantically right independent of anyone's
			// intent, which is why it is a refusal and not a softened
			// comment somewhere. Ollama exposes no timings_per_token-style
			// surface at all, so its /api/show document cannot carry
			// evidence about live progress in EITHER direction -- a
			// live_progress row attributed to that probe is a false
			// provenance whatever verdict it carries, and false provenance
			// on this column is the one thing the column exists to
			// prevent. routing.CapabilityRow's own source doc makes the
			// strong claim ("a row with this source and verdict
			// CapabilityNo could therefore not have come from that probe")
			// -- this is what makes the claim true rather than aspirational.
			//
			// Only the live-progress row goes; the verdict rows below are
			// exactly what an /api/show document CAN answer, so they still
			// ride the same pass. Warn, not Debug, for this file's
			// established reason: a drop that cannot heal on its own is
			// invisible at the gateway's default info level otherwise.
			slog.Warn("runtime capability sample attributes a live-progress verdict to the ollama probe, dropping that row",
				"spec_id", rt.SpecID, "verdict", verdict, "source", source)
		} else {
			rows = append(rows, routing.CapabilityRow{
				Capability: routing.CapabilityLiveProgress, Verdict: verdict,
				Source: source, CheckedAt: at,
			})
		}
	}
	reported := rt.Capabilities.capabilityRows(source, at)
	kept := make([]routing.CapabilityRow, 0, len(reported))
	var dropped, deniedNo []string
	for _, row := range reported {
		switch {
		case reservedAgentCapabilityNames[row.Capability]:
			dropped = append(dropped, row.Capability)
		case source == routing.CapabilitySourceOllamaAPIShow && row.Verdict == routing.CapabilityNo:
			// REFUSED, for the live-progress refusal's reason applied to
			// the capability this source CAN speak about -- which is to say
			// applied generally, because the reason was never specific to
			// live progress.
			//
			// routing.CapabilityRow's source doc states it for every row
			// carrying ollama_api_show, not for one name: "A row with this
			// source and verdict CapabilityNo could therefore not have come
			// from that probe." The ground is that Ollama's capability
			// array is NOT exhaustive -- a name's absence means "Ollama did
			// not tell us", never "this model cannot do that" -- so
			// collector.detectOllamaCapabilities produces "yes" or nothing
			// on every path and can never produce a "no" for anything.
			//
			// Left unenforced, that sentence was true only as a statement
			// ABOUT provenance (such a row indeed did not come from the
			// probe -- it is a lie by the sender) while being false as an
			// invariant: a buggy or hostile agent put a rank-1 "no" in
			// front of an operator under a provenance that cannot produce
			// one, and at equal rank it overwrote the OTHER probe's honest
			// verdict. That is the same reasoning as the live-progress
			// refusal above, and it costs the honest path nothing.
			//
			// Only the "no" rows go. A "yes" is exactly what an /api/show
			// document can answer, so the rest of the pass still lands,
			// still sourced ollama_api_show.
			deniedNo = append(deniedNo, row.Capability)
		default:
			kept = append(kept, row)
		}
	}
	if len(dropped) > 0 {
		// Warn for this file's established reason: the drop is deterministic
		// -- it repeats for every sample this agent build sends -- and at
		// Debug it would be invisible at the gateway's default info level,
		// while the missing row reads as plain "unknown".
		slog.Warn("runtime capability sample reports reserved internal capability names, dropping those rows",
			"spec_id", rt.SpecID, "source", source, "capabilities", dropped)
	}
	if len(deniedNo) > 0 {
		// Warn, same argument: a drop that cannot heal on its own (the
		// source names the agent build that sent it) is otherwise invisible
		// at the gateway's default info level, and the absent row reads as
		// plain "unknown".
		slog.Warn("runtime capability sample attributes a negative verdict to the ollama probe, dropping those rows",
			"spec_id", rt.SpecID, "source", source, "capabilities", deniedNo)
	}
	return append(rows, kept...)
}

// resolvedCapabilities returns specID's memoized ownership resolution,
// resolving it on first sight. The bool is the resolution's own ok: false
// means the spec is unknown, owned by a DIFFERENT server (rejected with a
// Warn inside resolveRuntimeSpecCapabilities), or that its stored rows could
// not be read -- and the caller must not write. A rejected resolution is
// memoized too, so a repeated spec_id costs one lookup per sample, not one
// per occurrence.
func (s *Server) resolvedCapabilities(ctx context.Context, serverID, specID string, resolved map[string]capabilityResolution) (capabilityResolution, bool) {
	if r, seen := resolved[specID]; seen {
		return r, r.ok
	}
	mappingID, stored, ok := s.resolveRuntimeSpecCapabilities(ctx, serverID, specID)
	r := capabilityResolution{mappingID: mappingID, stored: stored, ok: ok}
	resolved[specID] = r
	return r, ok
}

// capabilityResolution is one spec_id's resolved mapping plus the capability
// rows currently STORED on it, keyed by capability -- the baseline every
// comparison in this file is made against, memoized per distinct spec_id for
// one sample. stored being a MAP is what lets the caller fold a just-written
// row straight back into the memo without re-reading anything (nil for a
// rejected resolution, which the caller never reaches).
type capabilityResolution struct {
	mappingID string
	stored    map[string]routing.CapabilityRow
	ok        bool
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
	// Parsed once and reused below at the AgentFeatures.Set call site (this
	// sample's declared capability set) and here for the per-server metric
	// aggregate -- see runtimeModelProbeFeature's doc for why this
	// agent-declared capability is checked via membership rather than
	// advertised in gatewayAgentFeatures.
	caps := parseAgentCapabilities(req.Capabilities)
	telemetry, err := telemetryFromRequest(req, raw, now)
	if err != nil {
		return &invalidPayloadError{cause: err}
	}
	// A multi-model server-agent reporting per-runtime metrics may not also
	// run the legacy agent-wide OP_AGENT_METRICS_URL scrape, so the top-level
	// active_requests/queue_depth this sample carries can be absent or stale.
	// When the agent declares runtime_model_probe, the accurate per-server
	// figure is the SUM across its runtimes -- this REPLACES (never adds to)
	// the top-level fields, since adding would double-count an agent that
	// still runs both.
	if slices.Contains(caps, runtimeModelProbeFeature) && len(req.Runtimes) > 0 {
		telemetry.ActiveRequests, telemetry.QueueDepth = sumRuntimeActiveQueue(req.Runtimes)
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
	// rejecting the whole sample -- see parseAgentCapabilities. Reuses caps
	// (parsed once, above) rather than re-parsing req.Capabilities.
	s.AgentFeatures.Set(serverID, caps)
	// Record WHICH runtime-config document this agent says it has APPLIED, and
	// do it BEFORE the status publish two lines below. That ordering is a
	// contract, not tidiness: the VRAM benchmark's isolation wait is woken by
	// the published frame and then reads this registry, so recording first is
	// what lets it assume the acknowledgement it reads is at least as fresh as
	// the frame that woke it -- there is then no case where a frame arrives
	// ahead of the acknowledgement it rode in with. Same
	// after-every-store-write placement as every other registry update in this
	// block, for the same reason: a report is evidence, and stamping it while
	// the sample failed to persist would claim freshness the gateway lacks.
	s.RuntimeStatus.SetAppliedConfigETag(serverID, clampAppliedConfigETag(req.RuntimeConfigAppliedETag))
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
	// Best-effort write-back of each managed process's probed context window
	// onto its owning mapping (Task 12, Option B) -- see writeBackRuntimeContext.
	// Gated on THIS sample's own parsed caps (reused from above), NOT on a
	// re-read of the shared mutable AgentFeatures registry: during a rolling
	// agent upgrade, two overlapping in-flight samples for the same server
	// could otherwise clobber each other's Set(caps) between this gate and
	// the write-back it guards, letting one sample's write-back run under
	// the OTHER sample's capabilities. Checking caps directly is race-free --
	// it reflects exactly what THIS sample declared. An agent that has never
	// declared runtime_model_probe must never have its mappings' context_size
	// touched from this path. Never rejects the sample; a failure here is
	// logged and dropped.
	if slices.Contains(caps, runtimeModelProbeFeature) {
		s.writeBackRuntimeContext(ctx, serverID, req.Runtimes)
		// Best-effort write-back of everything this pass determined about
		// each managed process's BUILD -- the auto-detected capability
		// verdicts and the live-progress verdict alike, as
		// model_mapping_capabilities rows on the owning mapping (#49-3) --
		// see writeBackRuntimeCapabilities. One call, because they are one
		// fact set read off one /props document. Shares
		// writeBackRuntimeContext's gate immediately above and for the same
		// reason: the verdicts ride the SAME per-runtime probe pass that
		// produces context_size, so they share that pass's trust boundary.
		// UNLIKE the context write-back this one is NOT metrics_locked-gated
		// -- routing.MappingStore.UpsertMappingCapabilities carries the
		// argument for why a capability is not a metric. Never rejects the
		// sample; a failure here is logged and dropped.
		s.writeBackRuntimeCapabilities(ctx, serverID, req.Runtimes)
	}
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
