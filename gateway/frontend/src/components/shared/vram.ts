// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import type { BenchmarkRunDTO, HardwareGPU, VRAMGPUItemDTO, VRAMReportDTO } from '../../api';
import type { MessageKey } from './types';

/**
 * Presentation rules for the VRAM benchmark's result, shared by the two places
 * it is read: the benchmark area (a live run, its outcome, its history rows)
 * and the launch-spec form's per-GPU apply affordance.
 *
 * Everything here exists because the result's vocabularies are CLOSED and
 * PERSISTED (they are stored inside the history row's `vram_json` column), so
 * each value gets one localized sentence and an unknown value gets an honest
 * fallback rather than a raw identifier on screen.
 */

/**
 * The four CLOSED vocabularies this module renders -- the inconclusive
 * reasons, the confidence warnings, the fingerprint kinds, the isolation
 * proofs -- DECLARED here, once, as the arrays that every label map and every
 * test derives from.
 *
 * They are arrays rather than bare map keys for one reason: a test can then
 * iterate the vocabulary and require the mapping to be TOTAL, instead of
 * restating the values and going stale the moment one is added. That
 * restatement is exactly how `isolation_lost`, `strategy_disagreement` and
 * `undeclared_gpu_allocation` shipped with no test naming them, under test
 * titles that still claimed to cover "the seven reasons".
 *
 * ADDING A VALUE TOUCHES FOUR PLACES, and ALL FOUR are checked against each
 * other:
 *
 *  1. the Go constant it mirrors -- the `vramInconclusive*`, `vramWarning*`,
 *     `vramFingerprint*` and `vramProof*` blocks in
 *     `internal/gateway/benchmark_vram.go`, `benchmark_vram_isolation.go` and
 *     `benchmark_vram_confidence.go`;
 *  2. the value, in the array below;
 *  3. its label key, in the map below -- a missing one is a COMPILE error,
 *     since each map is keyed by its vocabulary's own type, and so is a
 *     mapped key for a value the vocabulary does not declare;
 *  4. the German AND English sentence, in `i18n.ts`.
 *
 * Steps 2-4 are enforced against each other: `vram.test.ts` requires every
 * declared value to reach a distinct, non-fallback sentence in both locales,
 * and `i18n.test.ts` requires every message key under these prefixes to be
 * claimed by a declared value, so an orphaned sentence fails too.
 *
 * STEP 1 IS ENFORCED TOO, and that is newer than the rest. Deriving these
 * arrays from THEMSELVES pinned only what this file already said, so the very
 * defect the derivation was introduced to end recurred the moment the Go side
 * grew a tenth reason (`isolation_unacknowledged`) on its own: the portal
 * rendered "this build does not know the reason it reported" for a reason the
 * matching build reports. So `vram.test.ts` now reads the Go constant blocks
 * off disk and requires each array here to be exactly the closed set declared
 * there -- both directions, so a value added, dropped or renamed in Go fails
 * this portal's own suite. The values still travel as free-form strings inside
 * a persisted `vram_json` and appear in no schema, so the FALLBACK below stays
 * load-bearing for a row written by a NEWER gateway than this portal; what it
 * no longer covers up is a value this very build's backend already reports.
 */
export const vramInconclusiveReasons = [
  'isolation_timeout',
  'baseline_unstable',
  'post_load_unstable',
  'already_resident',
  'below_floor',
  'no_samples',
  'run_failed',
  'isolation_lost',
  'strategy_disagreement',
  'isolation_unacknowledged',
] as const;
export type VramInconclusiveReason = (typeof vramInconclusiveReasons)[number];

export const vramWarnings = [
  'non_managed_applications',
  'post_transport_agent',
  'undeclared_gpu_allocation',
  'residency_unknown',
] as const;
export type VramWarning = (typeof vramWarnings)[number];

export const vramFingerprintKinds = ['uuid', 'name_total'] as const;
export type VramFingerprintKind = (typeof vramFingerprintKinds)[number];

/**
 * What established that the run's `force_stopped` overrides had actually
 * reached the agent -- the two are DIFFERENT STRENGTHS of evidence, which is
 * why the report names one instead of leaving the reader to assume the
 * stronger:
 *
 *  - `config_acknowledged`: the agent reported which runtime-config document it
 *    had applied, and it was one the run had verified force-stops the whole
 *    fleet. A fact.
 *  - `bind_delay`: the agent does not report that at all (an older build), so
 *    the run waited out its guaranteed runtime-poll interval and then observed
 *    no process. An inference from an absence -- the only standard available
 *    before the acknowledgement existed, and still the only one for an agent
 *    that has not been upgraded.
 *
 * A report written before the acknowledgement shipped carries NEITHER value,
 * and that is why the label is only rendered when the field is present: a
 * missing value is "this build did not record it", never `bind_delay`.
 */
export const vramIsolationProofs = ['config_acknowledged', 'bind_delay'] as const;
export type VramIsolationProof = (typeof vramIsolationProofs)[number];

// reason -> the sentence that tells the operator what to DO about it. The
// reasons are distinct values precisely because the next action differs per
// reason, so the mapping is exhaustive rather than collapsed into one
// "inconclusive" text.
const inconclusiveLabelKeys: Readonly<Record<VramInconclusiveReason, MessageKey>> = {
  isolation_timeout: 'benchmarkVramInconclusiveIsolationTimeout',
  baseline_unstable: 'benchmarkVramInconclusiveBaselineUnstable',
  post_load_unstable: 'benchmarkVramInconclusivePostLoadUnstable',
  already_resident: 'benchmarkVramInconclusiveAlreadyResident',
  below_floor: 'benchmarkVramInconclusiveBelowFloor',
  no_samples: 'benchmarkVramInconclusiveNoSamples',
  run_failed: 'benchmarkVramInconclusiveRunFailed',
  isolation_lost: 'benchmarkVramInconclusiveIsolationLost',
  strategy_disagreement: 'benchmarkVramInconclusiveStrategyDisagreement',
  isolation_unacknowledged: 'benchmarkVramInconclusiveIsolationUnacknowledged',
};

const warningLabelKeys: Readonly<Record<VramWarning, MessageKey>> = {
  non_managed_applications: 'benchmarkVramWarningNonManaged',
  post_transport_agent: 'benchmarkVramWarningPostTransport',
  undeclared_gpu_allocation: 'benchmarkVramWarningUndeclaredGpu',
  residency_unknown: 'benchmarkVramWarningResidencyUnknown',
};

// Which field IDENTIFIED the card at measurement time -- a record of what the
// run captured, not of a comparison it made. The comparison happens later and
// elsewhere (`vramCardCheck`, when an operator is offered the number), so
// nothing here may read as "verified": `name_total` catches a swap between
// unlike cards only, two identical cards trading indices are indistinguishable
// that way, and an empty kind means no identifying field was available at all.
const fingerprintLabelKeys: Readonly<Record<VramFingerprintKind, MessageKey>> = {
  uuid: 'benchmarkVramFingerprintUuid',
  name_total: 'benchmarkVramFingerprintNameTotal',
};

// proof -> the sentence that says what was actually established. Neither may
// read as a bare "isolated": one names the agent's own acknowledgement, the
// other names a wait and an absence.
const isolationProofLabelKeys: Readonly<Record<VramIsolationProof, MessageKey>> = {
  config_acknowledged: 'benchmarkVramIsolationProofAcknowledged',
  bind_delay: 'benchmarkVramIsolationProofBindDelay',
};

/**
 * One closed vocabulary's label lookup: the mapped sentence, or the
 * vocabulary's own honest fallback for a value this build does not know.
 *
 * The widening cast is the point rather than a shortcut. Each map is TOTAL
 * over its vocabulary's type, which is what makes a missing or orphaned entry
 * a compile error; the value being looked up, however, is decoded from a
 * `vram_json` written by whatever gateway build ran the measurement, so a
 * value outside the vocabulary is a normal input and must reach the fallback
 * instead of being asserted away.
 */
function labelKeyFor<V extends string>(
  keys: Readonly<Record<V, MessageKey>>,
  value: string,
  fallback: MessageKey,
): MessageKey {
  return (keys as Readonly<Partial<Record<string, MessageKey>>>)[value] ?? fallback;
}

/** The localized reason a run reached no number. Unknown → an honest fallback. */
export function vramInconclusiveLabelKey(reason: string): MessageKey {
  return labelKeyFor(inconclusiveLabelKeys, reason, 'benchmarkVramInconclusiveUnknown');
}

/** The localized caveat that degraded a run's confidence without invalidating it. */
export function vramWarningLabelKey(warning: string): MessageKey {
  return labelKeyFor(warningLabelKeys, warning, 'benchmarkVramWarningUnknown');
}

/** What identified the card, named — never a bare "verified". */
export function vramFingerprintLabelKey(kind: string | undefined): MessageKey {
  return labelKeyFor(fingerprintLabelKeys, kind ?? '', 'benchmarkVramFingerprintNone');
}

/**
 * How the run established that its overrides had landed, or `null` when the
 * report does not say.
 *
 * `null` rather than a fallback sentence, and that is the one asymmetry against
 * the three vocabularies above: they are rendered for a value that exists but
 * is unknown to this build, whereas an ABSENT proof is a report from a gateway
 * that predates the acknowledgement entirely. Naming a standard it never
 * recorded — in either direction — would be an invention, so the line is simply
 * not rendered. An unknown non-empty value still gets an honest fallback, for
 * the same reason the others do: a newer gateway may report a third standard.
 */
export function vramIsolationProofLabelKey(proof: string | undefined): MessageKey | null {
  if (!proof) return null;
  return labelKeyFor(isolationProofLabelKeys, proof, 'benchmarkVramIsolationProofUnknown');
}

/** Whether a recorded `fingerprint_kind` is one this build can compare at all. */
function isVramFingerprintKind(kind: string): kind is VramFingerprintKind {
  return (vramFingerprintKinds as readonly string[]).includes(kind);
}

// 1 MiB, NOT 10^6 -- the unit every VRAM figure in this feature is in (the
// backend's vramBytesPerMB, the agent's per-process measurer, every estimate
// and every budget). Two units for one number is a wrong number, not a
// rounding difference, and here it would turn a matching card into drift.
const VRAM_BYTES_PER_MB = 1024 * 1024;

/**
 * One live card's fingerprint, in the SAME shape the run recorded for it, so
 * the two can be compared as strings.
 *
 * A deliberate local mirror of the backend's `vramFingerprintOf`, not a
 * derivation: the recorded string is PERSISTED verbatim inside a
 * `kind = "vram"` history row's `vram_json` and read back here days later, so
 * its format is a closed vocabulary exactly like the reason and
 * fingerprint-kind sets above — which is why the two shapes are pinned by
 * tests on both sides rather than by a shared serializer.
 *
 * `''` means this card cannot supply the field at all, which is "cannot
 * verify" and never "drift".
 */
function vramLiveFingerprint(live: HardwareGPU, kind: VramFingerprintKind): string {
  switch (kind) {
    case 'uuid':
      return (live.uuid ?? '').trim();
    case 'name_total': {
      const name = (live.name ?? '').trim();
      const total =
        live.memory_total_bytes > 0
          ? `${Math.floor(live.memory_total_bytes / VRAM_BYTES_PER_MB)} MB`
          : '';
      if (name && total) return `${name} / ${total}`;
      return name || total;
    }
    default: {
      // A third kind must not silently borrow the name+total shape: comparing
      // one shape against a fingerprint recorded in another reports DRIFT for
      // a card that never moved. This line makes adding a kind to
      // `vramFingerprintKinds` a compile error until it is formatted here.
      const unhandled: never = kind;
      return unhandled;
    }
  }
}

/**
 * What comparing the recorded card fingerprint against the live card at that
 * index actually established.
 *
 * Three outcomes and no fourth, because the recorded fingerprint exists for
 * exactly one job — catching a renumbering between the measurement and the
 * moment an operator adopts the number — and each outcome is a different
 * sentence:
 *
 *  - `verified`: the field the run recorded still matches the live card, and
 *    `kind` says WHICH field, because `name_total` catches a swap between
 *    UNLIKE cards only;
 *  - `unverifiable`: there is nothing to compare — no fingerprint was recorded
 *    (`GPUSample.UUID` is NVIDIA-only, and a collector reporting neither a
 *    name nor a total leaves the field empty on exactly the AMD and Apple
 *    hosts the delta strategy exists for), or no live card is known at this
 *    index yet. It must READ differently from `verified`: a check that could
 *    not be made is not a check that passed;
 *  - `drifted`: the run measured a demonstrably different card than the one
 *    sitting at this index now. The number is real but belongs to other
 *    hardware, so it is named rather than offered.
 */
export type VramCardCheck =
  | { state: 'verified'; kind: VramFingerprintKind }
  | { state: 'unverifiable' }
  | { state: 'drifted'; recorded: string; live: string };

/**
 * Compares the fingerprint a run recorded for one card against the live card
 * at that index.
 *
 * This is the reader the recorded fingerprint was always for. Without it the
 * value is written, shipped through the DTO and never looked at, while a
 * renumbering — a driver reset, a hardware swap — silently redirects a stored
 * 21500 MB onto whatever card now sits at index 1. Fails toward
 * `unverifiable`, never toward `verified`: an absent identifier on EITHER side
 * means "no drift detection available here", the same rule the per-GPU budget
 * rows' `expected_uuid` detector applies.
 */
export function vramCardCheck(
  item: VRAMGPUItemDTO | undefined,
  live: HardwareGPU | undefined,
): VramCardCheck {
  const recorded = (item?.fingerprint ?? '').trim();
  const kind = item?.fingerprint_kind ?? '';
  // A kind this build does not know is not a comparison it can make. Treated
  // as "cannot verify" rather than as drift, for the same reason an unknown
  // inconclusive reason gets an honest fallback sentence.
  if (!recorded || !isVramFingerprintKind(kind)) return { state: 'unverifiable' };
  if (!live) return { state: 'unverifiable' };
  const liveFingerprint = vramLiveFingerprint(live, kind);
  if (!liveFingerprint) return { state: 'unverifiable' };
  if (liveFingerprint === recorded) return { state: 'verified', kind };
  return { state: 'drifted', recorded, live: liveFingerprint };
}

/**
 * The sentence for a card check's outcome — what was compared, or why nothing
 * was. Never a bare "verified": `name_total` cannot tell two identical cards
 * apart and says so, and `unverifiable` says that no check was possible.
 */
export function vramCardCheckLabelKey(check: VramCardCheck): MessageKey {
  switch (check.state) {
    case 'verified':
      return check.kind === 'uuid'
        ? 'runtimeSpecVramCardVerifiedUuid'
        : 'runtimeSpecVramCardVerifiedNameTotal';
    case 'drifted':
      return 'runtimeSpecVramCardDrift';
    default:
      return 'runtimeSpecVramCardUnverifiable';
  }
}

/** Which of the two reported numbers an apply affordance would fill the field with. */
export type VramApplySource = 'delta' | 'measured';

export type VramApplyNumber = { mb: number; source: VramApplySource };

/**
 * The number this GPU item offers for the operator's own estimate field, and
 * which of the two quantities it is.
 *
 * The DELTA wins when both exist. They are not the same quantity and are never
 * averaged: the delta is the model's marginal cost on that card (a constant
 * neighbour, the driver reserve and ECC overhead all cancel out of it) and is
 * the only figure that exists at all on AMD and Apple hosts, while the measured
 * value is the agent's own per-process attribution. Whichever is used is NAMED
 * at the call site, so the operator applies a number whose source they can see.
 *
 * `null` means there is nothing to apply. A non-positive value is not a small
 * model: `0` means UNKNOWN throughout this feature, so it must never reach a
 * field as a number.
 */
export function vramApplyNumber(item: VRAMGPUItemDTO | undefined): VramApplyNumber | null {
  if (!item) return null;
  if ((item.delta_mb ?? 0) > 0) return { mb: item.delta_mb as number, source: 'delta' };
  if ((item.measured_mb ?? 0) > 0) return { mb: item.measured_mb as number, source: 'measured' };
  return null;
}

/**
 * The newest run in a mapping's benchmark history whose measurement may be
 * OFFERED for the operator's estimate field, or null when there is none.
 *
 * Four gates, and each one is a way the affordance would otherwise lie:
 *
 *  - `kind === 'vram'`: the payload is decoded for that kind only. No other
 *    row carries a VRAM measurement, whatever its `vram` field holds.
 *  - a definitive result: an `inconclusive` reason means there IS no number,
 *    and the apply must be absent rather than fill a 0.
 *  - `isolated`: the run's own evidence-backed claim that nothing else was
 *    running. A number measured while something the gateway could not stop may
 *    have been serving the model is not a number to hand the operator; this
 *    fails CLOSED, exactly as `Isolated` itself does.
 *  - at least one applicable per-GPU number, by the rule above.
 *
 * The history endpoint returns newest-first, so the first match is the newest.
 *
 * A fifth gate is deliberately NOT here, because it is per CARD rather than
 * per run: `vramCardCheck` compares each item's recorded fingerprint against
 * the live card at that index. A run can be entirely valid and still describe
 * hardware that has since been renumbered, so selecting the run and trusting
 * its cards are two different questions.
 */
export function latestApplicableVramRun(
  runs: readonly BenchmarkRunDTO[],
): { run: BenchmarkRunDTO; report: VRAMReportDTO } | null {
  for (const run of runs) {
    if (run.kind !== 'vram') continue;
    const report = run.vram;
    if (!report || report.inconclusive || !report.isolated) continue;
    if (!report.gpus.some((gpu) => vramApplyNumber(gpu) !== null)) continue;
    return { run, report };
  }
  return null;
}
