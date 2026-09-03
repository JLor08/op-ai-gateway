// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"encoding/json"
	"op-ai-gateway/internal/routing"
	"time"
)

// benchmarkKindVRAM is the benchmark-history discriminator for a VRAM run,
// alongside "speed" | "capacity" | "vision" (an empty stored kind is a legacy
// speed row). It is a persisted value: changing it orphans every row already
// written under the old name.
const benchmarkKindVRAM = "vram"

// The VRAM run's isolation evidence, one value per enumerated launch spec.
// This run believes a spec is not running for exactly one of these two
// reasons, and NOTHING else counts -- a 200 from an admin_state write is not
// evidence, because a file-mode agent never reads the document that write
// lands in, so every such write succeeds while stopping nothing.
const (
	// NEITHER value is recorded from a frame the isolation wait has not
	// ADMITTED, and admissibility is where "the agent is holding this
	// document" is established -- once per run, in one place, by whichever of
	// the two standards of proof applies (VRAMReport.IsolationProof). Until
	// then no observation says anything about this run: an override that never
	// arrived is compatible with everything on the wire.

	// vramEvidenceStoppedAfterWrite: a spec that HAD a live process when the
	// write landed is in a no-process state on an admissible frame. A stopped
	// frame that predates the write proves nothing -- and neither does one from
	// before the override is known to have landed, because a spec's own exit
	// (an idle timeout, a crash into `crashed`/`backoff`, both of which the
	// agent restarts from) looks identical to an applied override.
	vramEvidenceStoppedAfterWrite = "stopped_after_write"
	// vramEvidenceNoProcessAtWrite: the spec had no live process when the write
	// landed, and force_stopped refuses its restart -- a claim about a document
	// the agent has to be holding, hence the same admissibility rule.
	vramEvidenceNoProcessAtWrite = "no_process_at_write"
)

// WHAT PROVED THE OVERRIDE LANDED. Two values, and they are DIFFERENT
// STRENGTHS OF EVIDENCE, which is the whole reason the report names one
// instead of leaving the reader to assume the stronger:
//
//   - the agent stated which document it had applied, and it was one this run
//     had verified force-stops the whole fleet; or
//   - nothing stated anything, and the run waited out the agent's guaranteed
//     poll interval and then observed no process.
//
// An operator weighing a number needs to know which of those they got, and
// they need it on the REPORT rather than only in a log: the second is an
// inference from an absence, and it is exactly the inference an
// unacknowledged protocol forced on every run before this.
//
// IT IS ONE FIELD RATHER THAN A DOUBLED EVIDENCE VOCABULARY, and the reason is
// worth stating because the other shape looks tempting. The proof is a
// property of the RUN, not of a spec: the isolation wait applies one standard
// for its whole duration (an agent either declared the acknowledgement feature
// before the run started or it did not), so a per-spec encoding would repeat
// one value N times and invite the copies to disagree. Crossing "which proof"
// with "what happened to the process" would also make the evidence set
// combinatorial, and a third standard is already foreseen -- the deferred
// agent-side "measure now, isolated" capability -- which would double it
// again. Two orthogonal facts, two fields.
const (
	// vramProofConfigAcknowledged: the agent reported having APPLIED a
	// runtime-config document that this run derived and verified still carries
	// force_stopped on every enumerated spec. The ETag is a content digest, so
	// equality against a gateway-derived value is a real proof rather than a
	// shared counter.
	vramProofConfigAcknowledged = "config_acknowledged"
	// vramProofBindDelay: the agent never acknowledges (it has not declared
	// runtimeConfigAckFeature), so the run waited out its guaranteed
	// runtime-poll interval and then read the status stream. Weaker, and the
	// only standard available for an older agent.
	vramProofBindDelay = "bind_delay"
)

// The VRAM run's inconclusive reasons: it ran and reached no number. Empty
// means a definitive result. Each one is the operator's next action, which is
// why they are distinct values rather than one "inconclusive" boolean.
const (
	vramInconclusiveIsolationTimeout = "isolation_timeout"
	vramInconclusiveBaselineUnstable = "baseline_unstable"
	vramInconclusivePostLoadUnstable = "post_load_unstable"
	// vramInconclusiveAlreadyResident: the target still reported resident after
	// the drain was confirmed, so something the gateway cannot stop is serving
	// it. A delta measured against that baseline would be ~0 and definitive.
	vramInconclusiveAlreadyResident = "already_resident"
	// vramInconclusiveBelowFloor: a confirmed-resident model whose headline
	// delta is below the noise floor. No model costs ~0 MB, and 0 means
	// UNKNOWN everywhere else in this feature, so it must mean it here too.
	vramInconclusiveBelowFloor = "below_floor"
	// vramInconclusiveIsolationUnacknowledged: the agent DECLARED that it
	// reports which runtime-config document it has applied, and then never
	// acknowledged one that carries this run's overrides within the bound.
	//
	// It is its own reason rather than an isolation_timeout because the
	// operator's next action is different, and the difference is the whole
	// diagnostic value of the acknowledgement. `isolation_timeout` says the
	// document landed and a MODEL would not go quiet -- look at that model.
	// This says the document never landed at all: the agent is not reconciling
	// (wedged, mid-restart, or holding a document it cannot apply), so the
	// place to look is the agent, not the fleet. It also covers the honest
	// remainder of the mid-run downgrade: an agent that declared the feature at
	// trigger time and stopped declaring it before the wait ended costs one
	// bounded wait and is named, rather than silently dropping to the weaker
	// standard halfway through a proof.
	vramInconclusiveIsolationUnacknowledged = "isolation_unacknowledged"
	// vramInconclusiveNoSamples: GPU samples STOPPED arriving mid-run. A server
	// with no GPU samples at all is refused before the run starts.
	vramInconclusiveNoSamples = "no_samples"
	// vramInconclusiveRunFailed: the run stopped on a HARD ERROR after it had
	// already written something, so Error says what happened and the operator
	// reads that rather than a reason of its own. A CANCELLED run is one of
	// these -- Error is the context's -- because every bounded wait here
	// answers cancellation and its own timer identically, and reading a
	// cancellation as the timer's condition sends an operator who stopped
	// their own run to inspect a telemetry pipeline that is fine (see
	// vramStoppedByCancellation). It exists because such a
	// report must still be reported at all: the run had force-stopped part or
	// all of the fleet by then, and DrainedSpecIDs / RestoreFailed are the only
	// place an operator learns which specs were touched and which were left
	// overridden. Without this value that report would carry an EMPTY
	// Inconclusive -- which this contract reads as "a definitive result" --
	// with zero GPUs, i.e. a measurement of nothing.
	//
	// A hard error BEFORE the first write keeps the nil-report contract
	// instead: nothing was touched, so there is nothing to report but Error.
	vramInconclusiveRunFailed = "run_failed"
)

// How a per-GPU result identified the card it is attributed to. A stored VRAM
// number attributed to index 1 after the cards were renumbered is worse than no
// number, so the result records what it actually compared -- and says which,
// because the strongest available field differs by vendor: GPUSample.UUID is
// NVIDIA-only, so on exactly the two host classes the delta strategy exists to
// serve (AMD, Apple) a UUID-only detector would be empty.
const (
	// vramFingerprintUUID: a GPU UUID, so any renumbering is detectable.
	vramFingerprintUUID = "uuid"
	// vramFingerprintNameTotal: card name + total memory. This catches a swap
	// between UNLIKE cards only; two identical cards trading indices are
	// indistinguishable, and the portal must say so rather than imply a check
	// that was not made.
	vramFingerprintNameTotal = "name_total"
)

// VRAMReport is one VRAM benchmark run's result, carried on
// BenchmarkResult.VRAM and persisted verbatim into the kind=="vram" history
// row's own `vram_json` column. Reported, never applied: see vramHistoryRow.
type VRAMReport struct {
	// Isolated is true ONLY when every enumerated spec, target included,
	// carries this run's OWN evidence in IsolationEvidence -- compute it with
	// vramIsolationConfirmed rather than setting it by hand. A 200 from an
	// admin_state write is not evidence: in file mode every such write returns
	// 200 and stops nothing, which is why a file-mode server is refused before
	// the run starts rather than reported as isolated.
	Isolated bool `json:"isolated"`
	// IsolationEvidence is spec id -> why this run believes that spec is not
	// running: vramEvidenceStoppedAfterWrite or vramEvidenceNoProcessAtWrite. A
	// missing entry, or any other value, means NOT isolated. It is reported
	// alongside the boolean so Isolated can be audited rather than believed.
	IsolationEvidence map[string]string `json:"isolation_evidence,omitempty"`
	// IsolationProof is WHICH STANDARD OF PROOF the isolation wait applied --
	// vramProofConfigAcknowledged or vramProofBindDelay -- and it is recorded
	// whether or not the isolation was confirmed, so a timeout says which
	// standard failed rather than leaving the reader to guess.
	//
	// It is not redundant with Isolated: that boolean says the evidence was
	// complete, this says how strong the evidence was allowed to be. The two
	// are read together, which is why they travel together.
	IsolationProof string `json:"isolation_proof,omitempty"`
	// DrainedSpecIDs is what this run force-stopped. It is reported so the
	// portal can name the fleet an operator must clear by hand if the gateway
	// dies between the drain and the restore.
	DrainedSpecIDs []string `json:"drained_spec_ids,omitempty"`
	// RestoreFailed is the specs whose override this run could not clear, so
	// they ARE still force_stopped and an operator has to clear them by hand.
	// A store error, and nothing else: see RestoreTakenOver for the other way
	// a restore can not happen.
	RestoreFailed []string `json:"restore_failed,omitempty"`
	// RestoreTakenOver is the specs whose admin_state was no longer this run's
	// force_stopped when the restore re-read it, so the restore correctly
	// wrote NOTHING: an operator's mid-run "Force start" or "Clear override".
	//
	// It is a separate field because the two are separate instructions. These
	// specs are NOT force_stopped, so telling an operator to clear them by
	// hand -- which is what the restore_failed message says -- would stop a
	// model they had just deliberately started. What they need to know is that
	// the override on these specs is now somebody's own, not this run's.
	RestoreTakenOver []string `json:"restore_taken_over,omitempty"`
	// Inconclusive is empty on a definitive result, else one of the
	// vramInconclusive* reasons above.
	Inconclusive string `json:"inconclusive,omitempty"`
	// Warnings are the conditions that DEGRADE this result's confidence
	// without invalidating it, one of the vramWarning* values. They ride the
	// report because the run decided not to refuse on them: refusing on a
	// non-managed neighbour would make the feature unusable on exactly the
	// migration-path deployments the architecture blesses, and refusing on an
	// agent with no open WebSocket would cost the run for the same result. A
	// warning the operator never sees is not a warning, so they are reported
	// rather than only logged.
	Warnings []string `json:"warnings,omitempty"`
	// GPUs is the per-GPU result, one item per watched index. ALWAYS non-nil:
	// there is no omitempty here, so a nil slice would reach a client as JSON
	// null instead of [], and a portal reading it with a `?? []` fallback would
	// render an eternally empty list with no error and no crash. Call
	// normalizeGPUs before attaching a report to a BenchmarkResult;
	// vramHistoryRow normalizes what it persists on its own.
	GPUs []VRAMGPUItem `json:"gpus"`
}

// normalizeGPUs replaces a nil GPUs slice with an empty one, so the report
// serializes `"gpus":[]` and never `"gpus":null` -- see that field. Nil-safe,
// and idempotent. Call it BEFORE the report is attached to a result: once
// attached it is shared with every already-published SSE frame and must not be
// mutated (see BenchmarkResult.VRAM).
func (r *VRAMReport) normalizeGPUs() {
	if r == nil || r.GPUs != nil {
		return
	}
	r.GPUs = []VRAMGPUItem{}
}

// VRAMGPUItem is one watched GPU's VRAM result. The two numbers are NOT the
// same quantity and must never be averaged: DeltaMB is the model's marginal
// cost on that card (a constant neighbour, driver reserve or ECC overhead
// cancels out of it), while MeasuredMB is that process's attributed usage as
// the agent's own measurer reports it. They may legitimately differ, and a
// host without a per-process measurer only ever has the delta.
type VRAMGPUItem struct {
	Index           int    `json:"index"`
	Fingerprint     string `json:"fingerprint,omitempty"`      // uuid, or name+total
	FingerprintKind string `json:"fingerprint_kind,omitempty"` // "uuid" | "name_total" | "" (none available)
	// UnifiedMemory marks a figure read from unified SYSTEM memory (Apple
	// silicon, via ioreg) rather than dedicated VRAM. It travels with the item
	// because nothing downstream can re-derive it, and a number labelled as
	// VRAM when it is system memory is a wrong number, not a vague one.
	UnifiedMemory  bool `json:"unified_memory,omitempty"`
	BaselineUsedMB int  `json:"baseline_used_mb"`
	DeltaMB        int  `json:"delta_mb,omitempty"`    // strategy (b), used_after - used_before; 0 = none
	MeasuredMB     int  `json:"measured_mb,omitempty"` // strategy (a), post-load frame only; 0 = none/unknown
	// Attributable reports whether a spec GPU row exists for this index. False
	// means the run watched every GPU because the spec declares none, so there
	// is no row to apply the number to.
	Attributable bool `json:"attributable"`
}

// vramIsolationConfirmed reports whether every spec this run enumerated carries
// evidence THIS run produced itself -- the value VRAMReport.Isolated is allowed
// to take. An entry that is missing, empty, or anything outside the closed
// evidence set above fails the whole set.
//
// An EMPTY enumeration is false, not vacuously true: "nothing enumerated,
// nothing awaited, isolated claimed" is precisely the shape of the file-mode
// defect this rule exists to prevent, and a run that reached the measurement
// phase always enumerated at least its own target.
func vramIsolationConfirmed(enumeratedSpecIDs []string, evidence map[string]string) bool {
	if len(enumeratedSpecIDs) == 0 {
		return false
	}
	for _, specID := range enumeratedSpecIDs {
		switch evidence[specID] {
		case vramEvidenceStoppedAfterWrite, vramEvidenceNoProcessAtWrite:
		default:
			return false
		}
	}
	return true
}

// vramHistoryRow builds the kind=="vram" benchmark-history row for a VRAM run
// of mappingID on serverID. It is the ONLY builder of such a row: the payload
// belongs in `vram_json` and nowhere else, and assembling the row at a call
// site is how a payload ends up in capacity_curve or a kind string drifts.
//
// The row records what was measured, when, and under what isolation, so an
// operator can see a spec measured at 22 GB three times before they raise the
// estimate. It is EVIDENCE, NOT AUTHORITY: this run writes neither
// vram_measured_mb (agent-owned, and feeding it a gateway-computed delta would
// make a budget breach terminal for a model that had been working) nor
// vram_estimate_mb (operator-owned). The operator applies the number.
//
// A nil report is a run that never reached a result: the row still records the
// failure -- mirroring how the vision path records an inconclusive probe -- and
// carries no payload rather than an empty-looking one.
func vramHistoryRow(mappingID, serverID string, at time.Time, report *VRAMReport, errMsg string) routing.BenchmarkRun {
	row := routing.BenchmarkRun{
		MappingID: mappingID,
		ServerID:  serverID,
		CreatedAt: at,
		Kind:      benchmarkKindVRAM,
		Error:     errMsg,
	}
	if report == nil {
		return row
	}
	// Copy before normalizing: a report already attached to a result is shared
	// with every published SSE frame and must not be mutated here.
	payload := *report
	payload.normalizeGPUs()
	if encoded, err := json.Marshal(payload); err == nil {
		row.VRAMJSON = string(encoded)
	}
	return row
}
