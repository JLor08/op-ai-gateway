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
	"op-ai-gateway/internal/provider"
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
	// Binaries is the agent's binary inventory (its allowlisted llama.cpp
	// binaries with kind and version). A pointer tells the three states
	// apart: absent means "unchanged" and leaves the stored inventory alone
	// (the agent omits it unless it changed), an empty array means the agent
	// allows no binaries and replaces the agent rows with nothing.
	Binaries *[]agentBinarySample `json:"binaries,omitempty"`
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
	Code       string    `json:"code,omitempty"`
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
// The allowlist is exactly the three PROBE sources an agent runs, which is
// what makes this a trust boundary and not a typo filter: the vocabulary
// also contains CapabilitySourceManual (rank 3) and
// CapabilitySourceVisionBenchmark (rank 2), and an agent has no standing to
// claim either. A sample that did would put "an operator said so" in front
// of an operator and lock a real benchmark out of its own row. Neither
// reaches a row from here. Nor does CapabilitySourceLlamaCppTimings, a
// probe-rank source no agent produces: the gateway reads it off traffic it
// relayed itself.
//
// CapabilitySourceSdcppCapabilities is claimable because the agent's
// collector.ProbeSdcppVerdicts produces it, reading an agent-launched
// sd-server's /sdcpp/v1/capabilities document. That document answers one
// question, whether the server generates images, so the source may speak
// about the image row only: runtimeSampleCapabilityRows drops every other row
// it arrives with, and a live-progress verdict with it. Accepting the source
// here only decides the provenance; what that provenance may say is decided
// there, row by row.
//
// CapabilitySourceAudioCppModels is claimable on the same footing: the agent's
// collector probe reads an agent-launched audio.cpp child's /v1/models listing,
// whose per-model task answers three questions, whether the model synthesises
// speech, whether it transcribes and, for a speech model, whether it needs a
// reference voice, and whose mode answers a fourth and a fifth, whether an asr
// model streams its transcript and whether a speech model streams its audio. The
// source may speak about those five rows only (speech, transcription,
// transcription_streaming, speech_clone and speech_streaming, each in both
// directions): runtimeSampleCapabilityRows drops every other row it arrives
// with, and a live-progress verdict with it.
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
	case routing.CapabilitySourceSdcppCapabilities:
		return routing.CapabilitySourceSdcppCapabilities, true
	case routing.CapabilitySourceAudioCppModels:
		return routing.CapabilitySourceAudioCppModels, true
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
// written. A nil receiver (no wire object at all: the agent has nothing to
// say for this child this cycle, see agentRuntimeSample.Capabilities) yields
// nothing, as does a non-nil but empty one (detection ran, determined
// nothing): two different facts that both mean no rows, which is why the
// wire field is a pointer.
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
	SpecID string `json:"spec_id"`
	Model  string `json:"model"`
	// Instance and StartupMS: see RuntimeStatusDTO's fields of the same name.
	// An agent without the runtime_instances feature sends neither, which
	// decodes to instance 0 and "not measured".
	Instance       int       `json:"instance"`
	StartupMS      int       `json:"startup_ms,omitempty"`
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
	// llama.cpp's /props, Ollama's /api/show since #54, or sd-server's
	// /sdcpp/v1/capabilities since #154, which is why the set names its own
	// Source). A POINTER, mirroring the agent's own
	// sample.RuntimeSample.Capabilities *sample.Capabilities field
	// byte-for-byte on the wire: nil means the agent has nothing to say for
	// this child this cycle -- an agent that predates capability detection,
	// a child that is not running or has no conclusive answer yet, or, from
	// a 0.7.4+ agent, a stable_diffusion_cpp child whose verdict it does not
	// send because this gateway's feature list, as that agent last read it,
	// lacked capability_source_sdcpp -- and is distinct from a non-nil,
	// all-empty value ("detection ran, nothing determined"): two different
	// facts that both mean no write, but for different reasons, which is
	// exactly why this field is a pointer rather than a bare struct. See
	// writeBackRuntimeCapabilities for the gateway-side write-back, which
	// persists these AND LiveProgressSupport above as one set of
	// model_mapping_capabilities rows.
	Capabilities *agentRuntimeCapabilitiesSample `json:"capabilities,omitempty"`
	// ImageOptions is the image-generation options a stable_diffusion_cpp
	// child's capability document states (the sampler/scheduler names and
	// output formats it accepts, its size and batch limits, its own
	// defaults), from the SAME probe pass as Capabilities above. A POINTER,
	// mirroring the agent's own sample.RuntimeSample.ImageOptions
	// *RuntimeImageOptions byte-for-byte on the wire: nil means the agent has
	// nothing to say for this child -- an agent that predates the field, a
	// child that is not an sd-server, or one whose document has not been read
	// -- and is distinct from a non-nil all-zero value ("the document was
	// read and states no options"). Unlike Capabilities it is NOT a verdict
	// and has no write-back: it is carried through runtimeStatusDTOsFromSamples
	// onto RuntimeStatusDTO.ImageOptions and lives only in the volatile
	// runtimeStatusRegistry, so it never reaches the capability-row path.
	ImageOptions *agentRuntimeImageOptions `json:"image_options,omitempty"`
	// RAMMeasuredMB is this child process's measured resident host RAM in MB,
	// mirroring the agent's sample.RuntimeSample.RAMMeasuredMB (ram_measured_mb)
	// key-for-key. A spec-level scalar beside GPUs below, not inside it: RAM has
	// no GPU axis and one process backs one spec. 0 -- and equally an absent
	// key, which is what an agent that predates the field, or a spec with no
	// process measured yet, sends (the agent's field is omitempty) -- is the
	// UNKNOWN sentinel, never "needs no RAM": writeBackRuntimeRAM drops it, so
	// the stored measurement is neither zeroed nor rewritten by it, and
	// runtimeStatusDTOsFromSamples drops it from the live status frame on the
	// same `> 0` rule (RuntimeStatusDTO.RAMMeasuredMB).
	RAMMeasuredMB int                     `json:"ram_measured_mb,omitempty"`
	GPUs          []agentRuntimeGPUSample `json:"gpus,omitempty"`
	LastError     *agentRuntimeError      `json:"last_error,omitempty"`
	// Kind, RPC, Cluster and AdmissionBlockedBy are the cluster telemetry; see
	// RuntimeStatusDTO's fields of the same names, whose types decode them.
	Kind               string                   `json:"kind,omitempty"`
	RPC                *RuntimeRPCStatusDTO     `json:"rpc,omitempty"`
	Cluster            *RuntimeClusterStatusDTO `json:"cluster,omitempty"`
	AdmissionBlockedBy string                   `json:"admission_blocked_by,omitempty"`
}

// agentRuntimeImageOptions mirrors the agent's sample.RuntimeImageOptions (the
// wire form of one sd-server's image options) key-for-key, nested objects
// included. The two live in different Go modules, so the keys ARE the contract:
// nothing but TestAgentRuntimeImageOptionsWireShape pins this side of it. The
// lists are omitempty on the agent's side and decode to nil here when absent;
// limits and defaults are always present on the wire, and a numeric zero (or
// "") in either means "unknown". toProvider maps it onto the internal
// provider.SdcppImageOptions the rest of the gateway speaks.
type agentRuntimeImageOptions struct {
	Samplers      []string                  `json:"samplers,omitempty"`
	Schedulers    []string                  `json:"schedulers,omitempty"`
	OutputFormats []string                  `json:"output_formats,omitempty"`
	Limits        agentRuntimeImageLimits   `json:"limits"`
	Defaults      agentRuntimeImageDefaults `json:"defaults"`
}

// agentRuntimeImageLimits mirrors the agent's sample.RuntimeImageLimits.
type agentRuntimeImageLimits struct {
	MinWidth      int `json:"min_width"`
	MaxWidth      int `json:"max_width"`
	MinHeight     int `json:"min_height"`
	MaxHeight     int `json:"max_height"`
	MaxBatchCount int `json:"max_batch_count"`
}

// agentRuntimeImageDefaults mirrors the agent's sample.RuntimeImageDefaults.
type agentRuntimeImageDefaults struct {
	Sampler        string  `json:"sampler"`
	Scheduler      string  `json:"scheduler"`
	OutputFormat   string  `json:"output_format"`
	NegativePrompt string  `json:"negative_prompt"`
	Steps          int     `json:"steps"`
	ClipSkip       int     `json:"clip_skip"`
	Seed           int     `json:"seed"`
	Width          int     `json:"width"`
	Height         int     `json:"height"`
	CFG            float64 `json:"cfg"`
}

// toProvider converts the decoded wire value to the gateway's internal
// provider.SdcppImageOptions, field by field. Nil-safe: a nil receiver (no
// image_options on the wire) is a nil result, never a zero value -- the two
// mean different things (see agentRuntimeSample.ImageOptions). The lists are
// handed over as decoded: the wire value is not retained, so nothing aliases
// the registry's copy.
func (o *agentRuntimeImageOptions) toProvider() *provider.SdcppImageOptions {
	if o == nil {
		return nil
	}
	return &provider.SdcppImageOptions{
		Samplers:      o.Samplers,
		Schedulers:    o.Schedulers,
		OutputFormats: o.OutputFormats,
		Limits: provider.SdcppImageLimits{
			MinWidth:      o.Limits.MinWidth,
			MaxWidth:      o.Limits.MaxWidth,
			MinHeight:     o.Limits.MinHeight,
			MaxHeight:     o.Limits.MaxHeight,
			MaxBatchCount: o.Limits.MaxBatchCount,
		},
		Defaults: provider.SdcppImageDefaults{
			Sampler:        o.Defaults.Sampler,
			Scheduler:      o.Defaults.Scheduler,
			OutputFormat:   o.Defaults.OutputFormat,
			NegativePrompt: o.Defaults.NegativePrompt,
			Steps:          o.Defaults.Steps,
			ClipSkip:       o.Defaults.ClipSkip,
			Seed:           o.Defaults.Seed,
			Width:          o.Defaults.Width,
			Height:         o.Defaults.Height,
			CFG:            o.Defaults.CFG,
		},
	}
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
			Instance:            rt.Instance,
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
			ImageOptions:        rt.ImageOptions.toProvider(),
			Kind:                rt.Kind,
			RPC:                 rt.RPC.clone(),
			Cluster:             rt.Cluster.clone(),
			AdmissionBlockedBy:  rt.AdmissionBlockedBy,
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
		// The spec-level RAM figure rides the frame on the same `> 0` rule: 0 (and
		// an absent key, which decodes to 0) is UNKNOWN and stays off the wire. It
		// deliberately does not stamp MeasuredAt -- that is the per-GPU watermark.
		if rt.RAMMeasuredMB > 0 {
			dto.RAMMeasuredMB = rt.RAMMeasuredMB
		}
		// The startup measurement rides on the same rule: 0 or less is "not
		// measured yet" and stays off the wire.
		if rt.StartupMS > 0 {
			dto.StartupMS = rt.StartupMS
		}
		if rt.LastError != nil {
			dto.LastError = &RuntimeErrorDTO{
				Message:    rt.LastError.Message,
				At:         rt.LastError.At,
				ExitCode:   rt.LastError.ExitCode,
				Failures:   rt.LastError.Failures,
				StderrTail: clampRuntimeStderrTail(rt.LastError.StderrTail),
				Code:       rt.LastError.Code,
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
// runtimes array the store write-backs will process: the fold every
// spec-level write-back reads (VRAM, RAM, startup, context) and the
// capability write-back's own loop. Nothing else caps
// this array's length, and within the 1 MiB readRawJSON body cap a minimal
// runtime entry is only ~55 bytes on the wire -- uncapped, a single POST
// could drive on the order of 19,000 RuntimeSpecByID/resolution attempts on
// an endpoint agents hit every second. Clamp, don't reject -- mirrors
// maxHardwareGPUs/maxHardwareModules elsewhere in this file. It bounds every
// write-back loop named above; runtimeStatusDTOsFromSamples' status publish is
// a pure in-memory transform with no store fan-out per entry, so it is not the
// concern this constant exists for.
const maxRuntimeSamplesPerSample = 256

// maxRuntimeGPUsPerSample bounds how many per-GPU measured-VRAM entries one
// runtime sample's GPUs array will drive a store write for. A real server
// has at most a handful of GPUs; this is generous headroom (matching
// maxHardwareGPUs' magnitude), not a realistic ceiling -- it exists so ONE
// owned spec_id cannot alone drive unbounded UpdateRuntimeSpecGPUMeasured
// writes (maxRuntimeSamplesPerSample only bounds the number of
// DISTINCT/total sample entries considered, not the GPU fan-out within a
// single one).
const maxRuntimeGPUsPerSample = 64

// foldedSpec is one spec's measurements folded over its instance rows in one
// telemetry sample (foldInstanceRows): what the spec-level columns the ingest
// writes back are compared with and written from.
type foldedSpec struct {
	// MaxGPU is the largest measured VRAM per GPU index over the spec's rows
	// (only measured, > 0, figures; nil when no row measured any).
	MaxGPU map[int]int
	// MaxRAM is the largest measured host RAM over the spec's rows, 0 when no
	// row measured any.
	MaxRAM int
	// MaxStartupMS is the largest measured startup over the spec's rows, 0
	// when no row measured one.
	MaxStartupMS int
	// Primary is the row that speaks for the spec where only one row can:
	// the probed context size and the capability verdicts. It is the spec's
	// representative row (representativeRowIndex: the running instance-0
	// row, else the running row with the lowest index, else the instance-0
	// row, else the lowest index), so it is set for every spec in the fold.
	// A row that probed nothing -- one that is not running carries no
	// context size -- writes nothing.
	Primary *RuntimeStatusDTO
}

// foldInstanceRows folds a sample's status rows per spec id. A replicable
// spec reports one row per instance under its one spec id, and the spec-level
// columns the ingest writes back -- the measured VRAM per GPU, the measured
// RAM and startup -- hold ONE figure for all of them: the demand the agent
// admits a new instance with must cover the hungriest one, and a startup is
// only as quick as the slowest. So each takes the maximum over the spec's
// rows. Writing the rows one by one instead would write each column twice a
// second as two instances took turns, and the change detection that keeps a
// stable measurement from being rewritten would never hold. The context size
// and the capability verdicts are not figures to take a maximum of; they come
// from Primary.
//
// A spec that is not replicable has one row, so its fold is that row's own
// figures -- exactly what the write-backs wrote before instances existed. A
// row without a spec id is skipped, as every write-back skips it. rows is
// expected to be capped at maxRuntimeSamplesPerSample by the caller; each
// row's GPUs are capped here at maxRuntimeGPUsPerSample, so one row cannot
// drive an unbounded number of per-GPU writes.
func foldInstanceRows(rows []RuntimeStatusDTO) map[string]foldedSpec {
	out := make(map[string]foldedSpec, len(rows))
	for _, row := range rows {
		specID := strings.TrimSpace(row.SpecID)
		if specID == "" {
			continue
		}
		f := out[specID]
		gpus := row.GPUs
		if len(gpus) > maxRuntimeGPUsPerSample {
			gpus = gpus[:maxRuntimeGPUsPerSample]
		}
		for _, g := range gpus {
			if g.VRAMMeasuredMB <= 0 {
				continue
			}
			if f.MaxGPU == nil {
				f.MaxGPU = make(map[int]int, len(gpus))
			}
			f.MaxGPU[g.Index] = max(f.MaxGPU[g.Index], g.VRAMMeasuredMB)
		}
		f.MaxRAM = max(f.MaxRAM, row.RAMMeasuredMB)
		f.MaxStartupMS = max(f.MaxStartupMS, row.StartupMS)
		out[specID] = f
	}
	for specID, i := range primaryRowIndexes(len(rows), func(i int) (string, int, string) {
		return rows[i].SpecID, rows[i].Instance, rows[i].State
	}) {
		f := out[specID]
		f.Primary = &rows[i]
		out[specID] = f
	}
	return out
}

// primaryRowIndexes picks, per spec id, the index of the row that speaks for
// the spec (foldedSpec.Primary): its representative row, by the one rule
// every reader of a spec's rows applies (representativeRowIndex). row reads
// the spec id, instance index and state of row i of a slice of n, so the
// rule is one function for the status rows and for the raw samples the
// capability write-back reads. A row without a spec id is skipped, as every
// write-back skips it.
//
// Every instance runs the same command line, so the running row a request
// would reach probes what the mapping describes. A crashed or idled-out
// instance 0 has probed nothing, and letting it speak beside a burst instance
// that serves would silence the context and capability write-backs.
func primaryRowIndexes(n int, row func(i int) (specID string, instance int, state string)) map[string]int {
	bySpec := make(map[string][]int)
	for i := range n {
		specID, _, _ := row(i)
		if specID = strings.TrimSpace(specID); specID != "" {
			bySpec[specID] = append(bySpec[specID], i)
		}
	}
	picked := make(map[string]int, len(bySpec))
	for specID, indexes := range bySpec {
		picked[specID] = indexes[representativeRowIndex(len(indexes), func(k int) (int, string) {
			_, instance, state := row(indexes[k])
			return instance, state
		})]
	}
	return picked
}

// runtimeWriteBackLogPrefix is the fixed lead-in of resolveRuntimeSpecOwned's
// log messages; the axis name ("vram", or runtimeSpecLevelWriteBackAxis) is
// spliced in after it, so the same four audit-trail lines read "runtime vram
// write-back: ..." and "runtime ram/startup write-back: ..." from one source.
// It exists so the literal is not repeated at every log site (go:S1192).
const runtimeWriteBackLogPrefix = "runtime "

// runtimeSpecLevelWriteBackAxis is the axis name the spec-level write-backs'
// shared ownership resolution (ownedRuntimeSpec) logs under. The measured RAM
// and the measured startup resolve a spec once per sample between them
// (runtimeSpecMemo), so its log lines -- a cross-server Warn above all --
// speak for both, whichever write-back asked first: logged under "ram" alone,
// a refused startup write would leave no trace of its own.
const runtimeSpecLevelWriteBackAxis = "ram/startup"

// resolveRuntimeSpecOwned reports whether specID names a spec that exists and
// belongs to serverID, and returns it. It is the AUTHORIZATION gate of every
// per-spec measurement write-back -- the VRAM write-back calls it directly,
// and the RAM and startup write-backs through their shared per-sample memo
// (ownedRuntimeSpec): the measurement always lands for an owned spec, there is
// no per-axis operator opt-out on top of it. writeBack names the axis ("vram",
// or runtimeSpecLevelWriteBackAxis) in the log lines only.
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
//     server B and overwrite B's measurements -- which are not display-only:
//     agentRuntimeSpecDTO folds the measured value into max(measured,
//     estimate) when building the vram_mb the gateway later pushes to B's OWN
//     agent, so a forged value would corrupt the admission arithmetic B's
//     agent runs against a spec it never reported on.
//
// Exactly ONE resolution pass per call (RuntimeSpecByID + MappingByID +
// ApplicationByID, at most). Any failure to resolve (a lookup error, or a
// spec/mapping/application that no longer exists) is treated the same as "not
// owned" -- logged and skipped, never propagated -- matching the "a report is
// evidence, not a transaction" best-effort discipline this whole file follows.
// A cross-server mismatch is logged at Warn (not Debug): unlike a merely stale
// id, it is a signal an agent is naming another server's resources.
func (s *Server) resolveRuntimeSpecOwned(ctx context.Context, serverID, specID, writeBack string) (routing.RuntimeSpec, bool) {
	spec, ok, err := s.Routes.RuntimeSpecByID(ctx, specID)
	if err != nil {
		slog.Debug(runtimeWriteBackLogPrefix+writeBack+" write-back: spec lookup failed", "server_id", serverID, "spec_id", specID, "err", err)
		return routing.RuntimeSpec{}, false
	}
	if !ok {
		// The spec has since been deleted (or never existed); nothing to
		// write the measurement back to. Not an error.
		return routing.RuntimeSpec{}, false
	}
	mapping, err := s.Routes.MappingByID(ctx, spec.MappingID)
	if err != nil {
		slog.Debug(runtimeWriteBackLogPrefix+writeBack+" write-back: mapping lookup failed", "server_id", serverID, "spec_id", specID, "mapping_id", spec.MappingID, "err", err)
		return routing.RuntimeSpec{}, false
	}
	app, err := s.Routes.ApplicationByID(ctx, mapping.ApplicationID)
	if err != nil {
		slog.Debug(runtimeWriteBackLogPrefix+writeBack+" write-back: application lookup failed", "server_id", serverID, "spec_id", specID, "application_id", mapping.ApplicationID, "err", err)
		return routing.RuntimeSpec{}, false
	}
	if app.ServerID != serverID {
		slog.Warn(runtimeWriteBackLogPrefix+writeBack+" write-back rejected: spec belongs to a different server", "server_id", serverID, "spec_id", specID, "owner_server_id", app.ServerID)
		return routing.RuntimeSpec{}, false
	}
	return spec, true
}

// writeBackRuntimeVRAM writes each spec's measured VRAM per GPU back to its
// launch spec (agent-runtime-manager Task 9), folded over the spec's instance
// rows (foldInstanceRows: the largest per GPU), but only for a spec
// resolveRuntimeSpecOwned confirms belongs to serverID and still exists. That
// resolution happens with exactly ONE set of reads (RuntimeSpecByID +
// MappingByID + ApplicationByID) per spec in the fold -- one entry per
// DISTINCT spec_id, so a sample repeating a spec_id (several instances, or a
// malformed payload) never re-resolves it. There is no operator lock on this
// write: the measurement is always recorded, and it is the effective-VRAM
// computation (max of measured and estimate) that keeps a low measurement
// from lowering a reservation. The caller caps the folded rows at
// maxRuntimeSamplesPerSample and the fold caps each row's GPUs at
// maxRuntimeGPUsPerSample before any store call, bounding the worst case
// regardless of how many distinct ids a hostile/buggy sample names.
//
// AN UNCHANGED MEASUREMENT IS NOT REWRITTEN, and that is a cost fix rather
// than a tidiness one. Telemetry arrives once per second and every sample is
// a FULL SNAPSHOT, so a spec whose measurement is merely stable -- the normal
// state of a loaded model serving nothing -- used to drive one unconditional
// UPDATE per second per (spec, gpu), indefinitely. An idle overnight server
// with a handful of measured specs across two cards produced on the order of
// a million identical UPDATEs a day: WAL growth on SQLite, dead-tuple churn
// and autovacuum pressure on PostgreSQL, for a table with a dozen rows. The
// fold is what keeps this true for a replicable spec: written row by row, two
// instances measuring different figures would each overwrite the other's
// every second.
//
// The comparison is against WHAT IS STORED, read once per owned spec in the
// fold, rather than against what the agent last sent. Suppressing at the
// agent would be cheaper still (it would save the report as well as the
// write) but it cannot converge: the stored row can change out from under a
// long-running agent -- an operator deleting and re-adding a GPU row resets
// vram_measured_mb to 0 -- and an agent that had suppressed its unchanged
// report would never resend, leaving the portal showing 0 for a spec that is
// measured and running. Comparing here costs one extra read per owned spec
// per sample and converges no matter what happened to the row; a read is also
// far cheaper than the write it replaces on both engines.
//
// Best-effort throughout, matching the "a report is evidence, not a
// transaction" ingest discipline this whole file follows: nothing here is
// ever returned as an error -- this must NEVER reject the telemetry sample
// it rode in on. A failed RuntimeSpecGPUs read degrades to "write
// unconditionally", never to "skip the write": staleness must not be able to
// suppress a real measurement. Called only AFTER every store write in
// ingestTelemetrySample has succeeded.
func (s *Server) writeBackRuntimeVRAM(ctx context.Context, serverID string, folded map[string]foldedSpec) {
	if s.Routes == nil {
		return
	}
	for specID, f := range folded {
		if len(f.MaxGPU) == 0 {
			continue
		}
		if _, ok := s.resolveRuntimeSpecOwned(ctx, serverID, specID, "vram"); !ok {
			continue
		}
		stored := s.storedMeasuredVRAM(ctx, serverID, specID)
		for index, measured := range f.MaxGPU {
			if was, known := stored[index]; known && was == measured {
				continue // already on file, byte for byte
			}
			if err := s.Routes.UpdateRuntimeSpecGPUMeasured(ctx, specID, index, measured); err != nil {
				// Tolerates ErrNotFound (a GPU row deleted out from under an
				// in-flight sample) the same as any other failure here: log
				// and move on, never reject the sample.
				slog.Debug("runtime vram write-back failed", "server_id", serverID, "spec_id", specID, "gpu_index", index, "err", err)
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

// writeBackRuntimeRAM writes each spec's measured resident host RAM back onto
// its launch spec (agent_runtime_specs.ram_measured_mb), the spec-level sibling
// of writeBackRuntimeVRAM's per-GPU write: RAM is host-scoped, so there is one
// scalar per spec and no GPU index, and a spec with no GPU rows (CPU-only, or
// one a sample reports no gpus array for) still lands its measurement. A
// replicable spec's figure is the largest of its instances' (foldInstanceRows).
//
// Mirrors writeBackRuntimeVRAM's discipline, deliberately:
//
//   - A measurement of 0 or less is UNKNOWN, never "needs no RAM", and is
//     dropped before it reaches the store. An absent key -- an agent that
//     predates the field, or a spec with no process measured yet -- decodes to
//     the same 0, so it is a no-op too and never zeroes a stored value.
//   - Ownership is checked once per spec in the fold (resolveRuntimeSpecOwned:
//     the spec exists and belongs to serverID, with the cross-server Warn),
//     through memo, which writeBackRuntimeStartup shares, so the two
//     spec-level write-backs of one sample resolve a spec once between them,
//     and that resolution logs under both their names
//     (runtimeSpecLevelWriteBackAxis).
//   - An unchanged measurement is NOT rewritten. Telemetry arrives once per
//     second as a full snapshot, so a merely stable value would otherwise cost
//     one UPDATE per second per spec indefinitely (the cost argument
//     writeBackRuntimeVRAM documents at length). The comparison is against
//     WHAT IS STORED -- the spec the ownership check already fetched -- not
//     against what the agent last sent, so it converges even if the stored
//     value changed out from under a long-running agent.
//
// Best-effort throughout, like every write-back here: nothing is returned as an
// error and this must NEVER reject the telemetry sample it rode in on. Called
// only AFTER every store write in ingestTelemetrySample has succeeded.
func (s *Server) writeBackRuntimeRAM(ctx context.Context, serverID string, folded map[string]foldedSpec, memo runtimeSpecMemo) {
	if s.Routes == nil {
		return
	}
	for specID, f := range folded {
		if f.MaxRAM <= 0 {
			continue
		}
		spec, ok := s.ownedRuntimeSpec(ctx, serverID, specID, memo)
		if !ok || spec.RAMMeasuredMB == f.MaxRAM {
			continue // not owned, or already on file byte for byte
		}
		if err := s.Routes.UpdateRuntimeSpecRAMMeasured(ctx, specID, f.MaxRAM); err != nil {
			// Tolerates ErrNotFound (the spec deleted out from under an
			// in-flight sample) the same as any other failure: log and move on.
			slog.Debug("runtime ram write-back failed", "server_id", serverID, "spec_id", specID, "err", err)
			continue
		}
		spec.RAMMeasuredMB = f.MaxRAM
	}
}

// startupWriteBackThresholdPercent is how far, in percent of the stored value,
// a measured startup has to move before writeBackRuntimeStartup writes it.
const startupWriteBackThresholdPercent = 10

// writeBackRuntimeStartup writes each spec's measured startup back onto its
// launch spec (agent_runtime_specs.startup_measured_ms): the largest non-zero
// startup_ms of the spec's instance rows in the sample (foldInstanceRows) --
// an instance is only as quick to bring up as the slowest has been.
//
// The figure is an input of a replicable spec's pushed idle timeout
// (portal effectiveIdleTimeoutSeconds), so it is written only when it moved
// by more than startupWriteBackThresholdPercent of the stored value, or when
// nothing is stored yet: every start measures a little differently, and
// writing that jitter would push the agent a new document each time.
// Ownership, change detection against what is stored, and best-effort
// failure handling are writeBackRuntimeRAM's, through the same memo.
//
// The ingest writes this outside the portal service, whose write paths push a
// changed document themselves, so a write for a replicable spec or a cluster
// head (portal.DerivesIdleTimeout) pushes serverID's runtime config here, once
// per sample however many specs moved. Any other spec is pushed its stored
// idle timeout, which the measurement does not change, so its write pushes
// nothing.
func (s *Server) writeBackRuntimeStartup(ctx context.Context, serverID string, folded map[string]foldedSpec, memo runtimeSpecMemo) {
	if s.Routes == nil {
		return
	}
	push := false
	for specID, f := range folded {
		if f.MaxStartupMS <= 0 {
			continue
		}
		measured := int64(f.MaxStartupMS)
		spec, ok := s.ownedRuntimeSpec(ctx, serverID, specID, memo)
		if !ok || !startupMovedEnough(spec.StartupMeasuredMS, measured) {
			continue
		}
		if err := s.Routes.UpdateRuntimeSpecStartupMeasured(ctx, specID, measured); err != nil {
			slog.Debug("runtime startup write-back failed", "server_id", serverID, "spec_id", specID, "err", err)
			continue
		}
		spec.StartupMeasuredMS = measured
		push = push || portal.DerivesIdleTimeout(*spec)
	}
	if push {
		s.PushRuntimeConfig(serverID)
	}
}

// startupMovedEnough reports whether a measured startup differs from the
// stored one by more than startupWriteBackThresholdPercent of the stored
// value, or nothing is stored yet (0). Integer arithmetic, so exactly the
// threshold is not more; a figure large enough to overflow it is no real
// startup and counts as moved.
func startupMovedEnough(stored, measured int64) bool {
	if stored <= 0 {
		return true
	}
	diff := measured - stored
	if diff < 0 {
		diff = -diff
	}
	if diff > math.MaxInt64/100 || stored > math.MaxInt64/startupWriteBackThresholdPercent {
		return true
	}
	return diff*100 > stored*startupWriteBackThresholdPercent
}

// runtimeSpecMemo is one telemetry sample's memo of the spec-level
// write-backs' ownership resolution (ownedRuntimeSpec): per spec id, the spec
// row resolveRuntimeSpecOwned read, or a nil entry when the spec is not
// serverID's (for whatever reason). It is per sample, never longer: the
// stored row can change between samples, and the next sample must compare
// against what is stored then.
type runtimeSpecMemo map[string]*routing.RuntimeSpec

// ownedRuntimeSpec reports whether specID is a spec serverID owns and returns
// its stored row, resolving it AT MOST ONCE per sample for the RAM and startup
// write-backs both: the first call runs resolveRuntimeSpecOwned, logging under
// runtimeSpecLevelWriteBackAxis since its verdict serves both, and memoizes
// the verdict -- owned or not -- in memo, and every later call answers from
// the memo without touching the store. The returned row is the memo's own, so
// a caller that writes a column updates it there and a later caller in the
// same sample compares against what it wrote.
func (s *Server) ownedRuntimeSpec(ctx context.Context, serverID, specID string, memo runtimeSpecMemo) (*routing.RuntimeSpec, bool) {
	if spec, seen := memo[specID]; seen {
		return spec, spec != nil
	}
	spec, ok := s.resolveRuntimeSpecOwned(ctx, serverID, specID, runtimeSpecLevelWriteBackAxis)
	if !ok {
		memo[specID] = nil
		return nil, false
	}
	memo[specID] = &spec
	return &spec, true
}

// resolveRuntimeSpecMapping reports whether specID's owning mapping may have
// its probed context_size written back for THIS sample, reached from server
// serverID -- the context write-back's sibling to resolveRuntimeSpecOwned
// above, resolving the SAME ownership chain (RuntimeSpecByID -> MappingByID
// -> ApplicationByID -> application.ServerID) for the SAME reason: spec_id is
// an agent-supplied body field with no other verification anywhere in this
// path, and an agent authenticated for server A must never be able to name a
// spec_id belonging to server B and overwrite B's mapping metrics.
//
// Unlike resolveRuntimeSpecOwned, which stops at ownership and returns the
// spec, this call also needs the owning mapping and applies the gate that
// matters for context_size: mapping.MetricsLocked (operator-pinned mapping
// metrics, the same flag UpdateMappingContextProbe's own SQL already
// enforces). Checking it here too, before ever calling that method, skips a
// write the store would only silently no-op, and lets the change-detection
// below compare against a context value the operator actually intends to keep.
//
// On success returns the mapping id and its CURRENTLY STORED context_size
// (for the caller's change-detection), true. Any failure to resolve -- a
// lookup error, a spec/mapping/application that no longer exists, a
// cross-server mismatch, or a locked mapping -- returns ("", 0, false); a
// cross-server mismatch is logged at Warn (not Debug), matching
// resolveRuntimeSpecOwned's audit-trail discipline for the same reason: an
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

// writeBackRuntimeContext writes each spec's probed context window back onto
// its owning mapping's context_size (Task 12, Option B: persist only the
// STABLE context size onto the mapping -- no per-mapping active/queue storage,
// no migration, no new store method; UpdateMappingContextProbe already exists
// and already sets context_size + metrics_source="probe" + metrics_updated_at,
// no-oping on a locked or missing mapping). The size is the spec's Primary
// row's (foldInstanceRows: the spec's representative row,
// representativeRowIndex): every instance runs the same command line, so one
// row speaks for the mapping, and taking each row in turn would flip the
// column between two instances that probed differently.
//
// Mirrors writeBackRuntimeVRAM's discipline throughout: the caller caps the
// folded rows at maxRuntimeSamplesPerSample before any store call; the fold
// has one entry per DISTINCT spec_id, so a sample repeating a spec_id -- a
// full snapshot arrives roughly once a second -- never re-resolves it; and AN
// UNCHANGED VALUE IS NOT REWRITTEN, comparing against the mapping's CURRENTLY
// STORED context_size (resolveRuntimeSpecMapping) rather than against
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
func (s *Server) writeBackRuntimeContext(ctx context.Context, serverID string, folded map[string]foldedSpec) {
	if s.Routes == nil {
		return
	}
	now := time.Now().UTC()
	for specID, f := range folded {
		if f.Primary == nil || f.Primary.ContextSize <= 0 {
			continue
		}
		size := f.Primary.ContextSize
		mappingID, storedContext, ok := s.resolveRuntimeSpecMapping(ctx, serverID, specID)
		if !ok || storedContext == size {
			continue // not writable, or already on file: no write amplification
		}
		if err := s.Routes.UpdateMappingContextProbe(ctx, mappingID, size, now); err != nil {
			slog.Debug("runtime context write-back failed", "server_id", serverID, "spec_id", specID, "mapping_id", mappingID, "err", err)
		}
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
// A replicable spec reports one row per instance; only its primary row's
// verdicts are written (primaryRowIndexes, the rule foldedSpec.Primary
// follows for the context size).
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
//     A nil Capabilities (nothing to say for this child this cycle, see
//     agentRuntimeSample.Capabilities) and a non-nil empty one (detection
//     ran, determined nothing) are different facts -- which is why the wire
//     field is a pointer -- and both simply yield no rows. An unchanged verdict issues no write either
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
	// Only each spec's primary row speaks (primaryRowIndexes: the spec's
	// representative row, representativeRowIndex), for the reason
	// writeBackRuntimeContext gives: every instance probes the same build, and
	// taking each row in turn would write one instance's verdicts over
	// another's. In input order, so a spec without instances is handled
	// exactly as before.
	primary := primaryRowIndexes(len(runtimes), func(i int) (string, int, string) {
		return runtimes[i].SpecID, runtimes[i].Instance, runtimes[i].State
	})
	for i, rt := range runtimes {
		if j, ok := primary[strings.TrimSpace(rt.SpecID)]; !ok || j != i {
			continue
		}
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
// REASONS ABOUT and that no agent capability probe can observe, so their
// appearance in an agent's open verdict list is necessarily either an
// upstream publisher's string or a bug -- never evidence.
//
// The criterion is exactly that, and it is why vision/video/audio/tools are
// NOT here: those four are what the llama.cpp and Ollama detectors read out
// of their documents (llama.cpp's modalities + chat_template_caps, Ollama's
// capabilities array), so a probe reporting one of them is reporting what it
// saw. The third probe, which reads stable-diffusion.cpp's capability
// document, answers image alone and is held to that row by row
// (runtimeSampleCapabilityRows), so it needs nothing from this list. Neither
// of the other two documents says anything about any name below:
//
//   - "mtp" is not detected anywhere today. The row comes from the portal --
//     an operator's checkbox (manual) or the model-NAME heuristic
//     (legacy, routing.IsMTPModelName) -- and it no longer feeds routing at
//     all (MappingCandidate carries no IsMTP field); it is a display and
//     operator-seed fact only. A "yes" from a probe would still overwrite
//     that display for any mapping the operator/heuristic never touched,
//     presenting an unvetted manifest string to the operator as if it were
//     an attested capability.
//   - "live_progress" has a dedicated wire field of its own
//     (RuntimeSample.LiveProgressSupport) and that field is the only channel
//     an agent may report it on. Its verdict makes the router send
//     timings_per_token upstream -- which an Ollama child does not
//     understand at all, and for an Ollama child the dedicated field is
//     ALWAYS "", so the "dedicated field wins" ordering below has nothing to
//     win with and a publisher's string would take effect outright.
//   - "speculation_observed" is observed by the GATEWAY, off the traffic it
//     already relays: a completion whose usage reported drafted tokens
//     (recordUsage -> writeSpeculationObserved, sourced
//     routing.CapabilitySourceLlamaCppTimings). Neither probe reads a served
//     completion's timings -- neither /props nor /api/show says anything
//     about speculative decoding at all -- so no honest agent has anything
//     to report here, in either direction. Two things make a verdict from
//     the open list worse for this name than for "mtp". A "no" is a claim
//     routing.CapabilitySpeculationObserved says cannot exist (the absence
//     of drafted tokens is "no evidence", never "does not speculate"), and
//     it arrives at rank 1, which TIES the gateway's own row -- and a tie is
//     writable by design, so it OVERWRITES a real observation. And the
//     repair that makes a tie safe for the CADENCE-driven rank-1 writers is
//     missing here: the gateway writes this row at most once per mapping per
//     PROCESS LIFETIME (claimSpeculationObserved), so it never rewrites what
//     it wrote, and an agent's false verdict stands until a restart.
//   - "image_edit" is DERIVED at the gateway, not detected by anyone
//     (routing.ImpliedImageEdit: an image:yes verdict and no operator
//     image_edit:no row). The only row the name ever carries is the
//     OPERATOR's opt-out, written from the mapping form at manual rank, so
//     there is no legitimate agent writer for it in either direction -- which
//     is the opposite of "image", the name this list deliberately leaves OFF
//     because the sd-server capability probe really does write it (ADR-042).
//     The harm is the "no": an agent's rank-1 image_edit:no on a mapping that
//     generates images silently disables edit for it, and when the operator
//     resets the opt-out (deleting the row is how "unknown" is stored) the
//     next sample writes it back. A "yes" is inert to the gate but would
//     still show the operator an unvetted row as an attested capability.
//
// Write-once-at-rank-1 is what "mtp" and "speculation_observed" above SHARE,
// not what tells them apart -- do not restore an older wording claiming it
// is unique to "speculation_observed". "mtp"'s rank-1 "legacy" row is written only for a
// BRAND-NEW mapping (portal.legacyMTPCapabilityRow, from the portal's own
// CreateMapping and from reconcileApplicationModels' newly-discovered-model
// branch) and nothing re-derives it afterwards, so a tie-ranked write from
// the open list is never repaired there either. That shared shape is why
// both names are on this list.
//
// What differs is the repair left over, and it runs the OTHER way. "mtp" has
// an operator control on the mapping form, whose rank-3 "manual" row
// outranks every automated writer permanently -- and it has to, because no
// writer re-derives "mtp" even across a restart. "speculation_observed" has
// no control on that form at all (it submits "mtp" and "vision" only), and
// its one routine repair is the process itself: the next speculating
// completion after a restart re-observes the row and, rank 1 against rank 1
// with a differing verdict, overwrites a false "no". A fabricated "yes"
// survives that, because it is the verdict this writer would have written
// (routing.WritableCapabilityRows rule 2 drops it as unchanged) -- which is
// the "mtp" harm, and the second reason this name is reserved rather than
// merely unlikely.
//
// This gateway-side rule is the load-bearing one, and the reason is the
// threat model rather than tidiness: the agent's own detector skips the
// first two names too (collector.detectOllamaCapabilities), but that filter
// protects only against a publisher string reaching an HONEST agent's Extra
// list. A buggy or hostile agent puts the name straight into the verdicts it
// sends, where no agent-side filter is in the path at all. This boundary is.
// It carries no entry for the last two names and needs none, for the same
// reason stated from the other side: "speculation_observed" and "image_edit"
// appear in neither document either agent detector reads, so an honest agent
// has no path to them, and a dishonest one was never going to consult a
// filter.
//
// The day a real MTP detector exists it reports through a field this
// codebase defined, the way live-progress support does, or this list changes
// on both sides -- what it must not do is arrive on the OPEN list, whose
// whole purpose is carrying strings no one here has vetted.
var reservedAgentCapabilityNames = map[string]bool{
	routing.CapabilityMTP:                 true,
	routing.CapabilityLiveProgress:        true,
	routing.CapabilitySpeculationObserved: true,
	routing.CapabilityImageEdit:           true,
	routing.CapabilityDecision:            true,
	routing.CapabilityDecisionOpenAI:      true,
	// The derived any_decision requirement is never a stored row name.
	routing.CapabilityAnyDecision: true,
}

// audioCppProbeCapabilities is the allowlist for the audiocpp_models source:
// the capability names an audio.cpp /v1/models listing can answer, so the only
// ones an agent may report under that provenance (runtimeSampleCapabilityRows
// refuses every other name, in either direction). It is the single list the
// refusal reads, so a name the listing starts to answer is admitted here once.
//
// speech and transcription come from each model's task. transcription_streaming
// comes from the same entry's mode field and refines transcription: the agent
// states it for an asr model only (streaming is a yes, offline a no) and sends
// no entry at all when the mode says nothing, so its absence is "unknown" and
// never a refusal. speech_clone comes from the task again and refines speech:
// the agent states it for a tts model (a no) or a clone model (a yes) only, and
// sends no entry at all for any other task, so its absence is "unknown" too.
// speech_streaming is transcription_streaming's twin on the synthesiser's side:
// it comes from the same mode field, refines speech, and the agent states it for
// a tts or clone model only (streaming is a yes, offline a no) and sends no entry
// at all when the mode says nothing, so its absence is "unknown" as well. None of
// the refinements is a reserved agent capability name
// (reservedAgentCapabilityNames), for the reason speech and transcription are
// not: nothing in this codebase reports it through a dedicated field.
var audioCppProbeCapabilities = map[string]bool{
	routing.CapabilitySpeech:                 true,
	routing.CapabilityTranscription:          true,
	routing.CapabilityTranscriptionStreaming: true,
	routing.CapabilitySpeechClone:            true,
	routing.CapabilitySpeechStreaming:        true,
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
// FOUR shapes are refused rather than stamped, and each is a refusal of a
// FALSE PROVENANCE that the source it names cannot carry, whatever the
// sender intended -- see each refusal below for its own argument:
//
//  1. a live-progress verdict attributed to ollama_api_show, to
//     sdcpp_capabilities or to audiocpp_models, in either direction. None of
//     Ollama, sd-server and audio.cpp exposes a timings_per_token-style
//     surface at all, so none of their documents is evidence for either
//     answer.
//  2. ANY capability's "no" verdict attributed to ollama_api_show.
//     Ollama's capability array is not exhaustive, so the detector behind
//     that source can only ever produce "yes" or nothing -- which is the
//     claim routing.CapabilityRow's own source doc makes about every row
//     carrying it, not just about the live-progress one.
//  3. ANY capability other than image attributed to sdcpp_capabilities, in
//     either direction. stable-diffusion.cpp's capability document answers
//     exactly one question -- whether its supported_modes list img_gen --
//     so a row under that source for any other capability says something
//     the document never said.
//  4. ANY capability other than speech, transcription,
//     transcription_streaming, speech_clone or speech_streaming attributed to
//     audiocpp_models, in either direction. audio.cpp's /v1/models listing
//     states each model's one task and mode, from which exactly five questions
//     are answered -- does it synthesise speech, does it transcribe, does its
//     asr mode stream, does its speech task need a reference voice, does its
//     speech mode stream -- so a row under that source for any other
//     capability says something the listing never said.
//     Unlike rule 3 the allowlist has five names (audioCppProbeCapabilities),
//     and all are kept in both directions: a tts model is (speech, yes) with
//     (transcription, no) and (speech_clone, no), a clone model the same with
//     (speech_clone, yes), an asr model the reverse of the tts shape, an
//     asr model's mode adds (transcription_streaming, yes or no) when it
//     states one, and a tts or clone model's mode adds (speech_streaming, yes
//     or no) the same way. speech_clone and speech_streaming are the audio.cpp
//     listing's to state alone: the same name under any other source falls to
//     that source's own rules (under sdcpp_capabilities, rule 3 drops it).
//
// All are one-line rules for the same reason: an invariant a caller can
// violate is not an invariant, and this is the boundary where the agent's
// bytes arrive. None voids the pass -- what the same document CAN answer
// (/api/show's "yes" verdicts, the sdcpp document's image verdict in either
// direction, the audio listing's speech, transcription,
// transcription_streaming, speech_clone and speech_streaming verdicts in either
// direction) still rides it.
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
		// newer agent reporting a source this build does not know would
		// lose every capability row it ever sends, and lose it silently --
		// the rows' absence is how this model spells "unknown",
		// indistinguishable from a probe that never ran, so there would be
		// nothing anywhere to point at.
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
		switch source {
		case routing.CapabilitySourceOllamaAPIShow:
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
		case routing.CapabilitySourceSdcppCapabilities:
			// REFUSED, for the ollama refusal's reason, and just as
			// independent of intent. ProbeSdcppVerdicts leaves LiveProgress
			// "" on every return path, so no honest agent sends this; the
			// boundary refuses it anyway because a caller can. sd-server
			// exposes no timings surface at all, so its capability document
			// is evidence for neither answer, and a live_progress row under
			// that source is a false provenance whatever it says.
			//
			// Only the live-progress row goes; the image verdict below is
			// what that document answers, so it still rides the pass. Warn
			// for the same reason as the ollama line above.
			slog.Warn("runtime capability sample attributes a live-progress verdict to the sdcpp probe, dropping that row",
				"spec_id", rt.SpecID, "verdict", verdict, "source", source)
		case routing.CapabilitySourceAudioCppModels:
			// REFUSED, for the two refusals above and just as independent of
			// intent. audio.cpp exposes no timings surface at all, so its
			// /v1/models listing is evidence for neither answer, and a
			// live_progress row under that source is a false provenance
			// whatever it says. The agent's audio probe leaves LiveProgress ""
			// on every path, so no honest agent sends this; the boundary
			// refuses it anyway because a caller can.
			//
			// Only the live-progress row goes; the speech, transcription,
			// transcription_streaming, speech_clone and speech_streaming verdicts
			// below are what that listing answers, so they still ride the pass. Warn for the same reason
			// as the lines above.
			slog.Warn("runtime capability sample attributes a live-progress verdict to the audio.cpp probe, dropping that row",
				"spec_id", rt.SpecID, "verdict", verdict, "source", source)
		default:
			rows = append(rows, routing.CapabilityRow{
				Capability: routing.CapabilityLiveProgress, Verdict: verdict,
				Source: source, CheckedAt: at,
			})
		}
	}
	reported := rt.Capabilities.capabilityRows(source, at)
	kept := make([]routing.CapabilityRow, 0, len(reported))
	var dropped, deniedNo, notImage, notAudio []string
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
		case source == routing.CapabilitySourceSdcppCapabilities && row.Capability != routing.CapabilityImage:
			// REFUSED, in either direction: the same false-provenance
			// argument as the two ollama_api_show refusals, applied to a
			// document that answers exactly one question.
			// stable-diffusion.cpp's /sdcpp/v1/capabilities says which modes
			// the server serves, and the agent derives the image verdict
			// from whether that list holds img_gen. It says nothing about
			// vision, tools or any other capability, so a row under this
			// source for one of them could not have come from that document
			// -- a buggy or hostile agent's claim, stamped with a provenance
			// that cannot produce it.
			//
			// "no" stays writable for image, unlike under ollama_api_show:
			// supported_modes is exhaustive, so a list without img_gen is a
			// real answer (routing.CapabilitySourceSdcppCapabilities).
			notImage = append(notImage, row.Capability)
		case source == routing.CapabilitySourceAudioCppModels && !audioCppProbeCapabilities[row.Capability]:
			// REFUSED, in either direction: the sdcpp refusal's argument
			// applied to a listing that answers five questions rather than
			// one. audio.cpp's /v1/models states each model's task and mode,
			// and the agent derives the speech verdict (does the task
			// synthesise), the transcription verdict (does it transcribe), for
			// an asr model, the transcription_streaming verdict (does its
			// mode stream) and, for a tts or clone model, the speech_clone
			// verdict (does its task need a reference voice) and the
			// speech_streaming verdict (does its mode stream) from it. It says
			// nothing about vision, tools or any other capability, so a row
			// under this source for one of them could not have come from that
			// listing -- a buggy or hostile agent's claim, stamped with a
			// provenance that cannot produce it.
			//
			// The five names (audioCppProbeCapabilities) all stay writable
			// in BOTH directions, unlike sdcpp's single image name: the
			// listing names the model's task, so a model it lists with a task
			// that does not serve a capability is a real "no" for that
			// capability (routing.CapabilitySourceAudioCppModels), and an asr
			// model is exactly (transcription, yes) with (speech, no). A
			// streaming or speech_clone verdict the agent could not derive is
			// not sent at all, so it never reaches this refusal and never
			// costs a Warn.
			notAudio = append(notAudio, row.Capability)
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
	if len(notImage) > 0 {
		// Warn, same argument again: the drop repeats for every sample this
		// agent build sends, and the absent rows read as plain "unknown".
		slog.Warn("runtime capability sample attributes a non-image capability to the sdcpp probe, dropping those rows",
			"spec_id", rt.SpecID, "source", source, "capabilities", notImage)
	}
	if len(notAudio) > 0 {
		// Warn, same argument again: the drop repeats for every sample this
		// agent build sends, and the absent rows read as plain "unknown".
		slog.Warn("runtime capability sample attributes a capability other than speech, transcription, transcription_streaming, speech_clone or speech_streaming to the audio.cpp probe, dropping those rows",
			"spec_id", rt.SpecID, "source", source, "capabilities", notAudio)
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
	caps, capsReadable := parseAgentCapabilities(req.Capabilities)
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
	// A targeted write: the server row read above may already be older than a
	// portal save, and writing it back whole would undo that save.
	if err := s.Routes.UpdateServerLastSeen(ctx, serverID, telemetry.ReportedAt, telemetry.UpdatedAt); err != nil {
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
	// The agent's clock offset, shown on the server: energy attribution reads
	// every host's samples by the agent's own time stamps, so a skewed member
	// clock shifts the window its energy part integrates. A sample without a
	// time stamp of its own carries the receive time and says nothing.
	if !req.ReportedAt.IsZero() {
		s.AgentPresence.ReportClockOffset(serverID, req.ReportedAt.Sub(now))
	}
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
	if s.AgentProxyStatus.Report(serverID, proxyRouteStatusesFromSamples(req.ProxyRoutes)) && s.triggerCertReconcile != nil {
		// A listener just became TLS-active: poke an immediate https-switch
		// reconcile so the public flip lands now rather than up to a full
		// cert_reconcile_interval later. Non-blocking + coalescing at the hook,
		// so this stays safe on the telemetry ingest hot path (issue #104).
		s.triggerCertReconcile()
	}
	// Record the agent's declared feature set (design spec §9, feature
	// negotiation), so a later portal runtime-spec write's PushRuntimeConfig
	// knows whether this connected agent understands a runtime_config frame
	// at all. Deliberately AFTER every store write succeeded, mirroring
	// AgentCertReports/AgentProxyStatus above: a report is evidence about
	// this agent's own binary, and stamping it while the sample itself
	// failed to persist would claim freshness the gateway does not have.
	// Tolerant: a malformed capabilities blob does not reject the whole
	// sample -- see parseAgentCapabilities. It forgets the server's
	// declaration instead, so the server reads as not heard from: no feature
	// is declared (PushRuntimeConfig then correctly withholds delivery), and a
	// runtime-config document that depends on what the agent declares is held
	// rather than derived from a declaration of nothing. Reuses caps (parsed
	// once, above) rather than re-parsing req.Capabilities.
	knewFeatures := s.AgentFeatures.Known(serverID)
	hadInstances := s.AgentFeatures.Has(serverID, runtimeInstancesFeature)
	hadRPCCluster := s.AgentFeatures.Has(serverID, rpcClusterFeature)
	if capsReadable {
		s.AgentFeatures.Set(serverID, caps)
	} else {
		s.AgentFeatures.Forget(serverID)
	}
	// The runtime-config document carries a replicable spec's instance
	// settings only for an agent that declares runtime_instances
	// (portal.Service.specAsPushed), and is held while the declaration is not
	// known (portal.AgentRuntimeConfigDTO.AwaitsAgentFeatures): the agent's
	// fetch is answered 503 and it keeps the document it applied. So a sample
	// that makes the declaration known -- the first after a gateway start, or
	// the first readable one after a blob that could not be read -- pushes the
	// document, which may have been held until now, and so does one that
	// changes runtime_instances, since a document input that changed is pushed
	// (THE RULE on portal.Service.notifyRuntimeChanged). An unreadable sample
	// pushes nothing: it holds the document.
	if capsReadable && (!knewFeatures || slices.Contains(caps, runtimeInstancesFeature) != hadInstances) {
		s.PushRuntimeConfig(serverID)
	}
	// A cluster is pushed only while every agent of it declares rpc_cluster
	// and every declaration is known (an unknown one withholds the cluster
	// from the other servers' documents), so the same two moves push every
	// server sharing a cluster with this one (its own document included).
	if capsReadable && s.Portal != nil && (!knewFeatures || slices.Contains(caps, rpcClusterFeature) != hadRPCCluster) {
		s.Portal.NotifyClustersOfServer(ctx, serverID)
	}
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
	//
	// The registry keeps every row as the agent sent it, one per instance of a
	// replicable spec; only the store write-backs below fold them per spec.
	statuses := runtimeStatusDTOsFromSamples(req.Runtimes, now)
	prevStatuses := s.RuntimeStatus.statusRows(serverID)
	s.RuntimeStatus.publish(serverID, statuses)
	// Every frame, a cluster head's and a member's alike, goes to the cluster
	// watchdog (clusterOrchestrator.observeStatus): an exited head, a lost
	// member, a breach, a local start a held member blocks. It only enqueues
	// the frame; the watchdog works on its own goroutines.
	s.clusters.observeStatus(serverID, prevStatuses, statuses)
	capped := statuses[:min(len(statuses), maxRuntimeSamplesPerSample)]
	// Best-effort write-back of the cluster members' measured usage onto
	// their membership rows by the peak rule, and of the members' session
	// records -- see clusterUsage.observe. A member spec has no runtime spec
	// row, so the per-spec write-backs below never see its measurement. Never
	// rejects the sample.
	s.clusterUsage.observe(ctx, serverID, capped)
	// One fold of the sample's rows per spec (foldInstanceRows), which every
	// write-back below reads, over at most maxRuntimeSamplesPerSample rows.
	folded := foldInstanceRows(capped)
	// Best-effort write-back of each spec's measured VRAM onto its launch
	// spec -- see writeBackRuntimeVRAM. Never rejects the sample; a failure
	// here is logged and dropped.
	s.writeBackRuntimeVRAM(ctx, serverID, folded)
	// Best-effort write-back of each spec's measured host RAM and startup onto
	// its launch spec -- see writeBackRuntimeRAM and writeBackRuntimeStartup,
	// which share one ownership resolution per spec. A no-op for an agent that
	// sends neither figure. Never rejects the sample; a failure here is logged
	// and dropped.
	specMemo := runtimeSpecMemo{}
	s.writeBackRuntimeRAM(ctx, serverID, folded, specMemo)
	s.writeBackRuntimeStartup(ctx, serverID, folded, specMemo)
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
		s.writeBackRuntimeContext(ctx, serverID, folded)
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
	// Best-effort replacement of the agent-reported binary inventory. A
	// no-op unless this sample carries the field. Never rejects the sample.
	s.writeBackBinaries(ctx, serverID, req.Binaries, now)
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
//
// readable tells the two nil sets apart. An absent blob, or one that parses,
// is a readable declaration: the agent declares exactly that, possibly
// nothing (an older binary). A blob that does not parse is not, and says
// nothing about the agent's binary, so the ingest forgets the server's
// declaration rather than recording an empty one (agentFeaturesRegistry.Forget).
func parseAgentCapabilities(raw json.RawMessage) (features []string, readable bool) {
	if len(raw) == 0 {
		return nil, true
	}
	var report agentCapabilitiesReport
	if err := json.Unmarshal(raw, &report); err != nil {
		return nil, false
	}
	return report.Features, true
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
