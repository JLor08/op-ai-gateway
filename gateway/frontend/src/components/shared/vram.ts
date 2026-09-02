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

// reason -> the sentence that tells the operator what to DO about it. The
// reasons are distinct values precisely because the next action differs per
// reason, so the mapping is exhaustive rather than collapsed into one
// "inconclusive" text.
const inconclusiveLabelKeys: Readonly<Record<string, MessageKey>> = {
  isolation_timeout: 'benchmarkVramInconclusiveIsolationTimeout',
  baseline_unstable: 'benchmarkVramInconclusiveBaselineUnstable',
  post_load_unstable: 'benchmarkVramInconclusivePostLoadUnstable',
  already_resident: 'benchmarkVramInconclusiveAlreadyResident',
  below_floor: 'benchmarkVramInconclusiveBelowFloor',
  no_samples: 'benchmarkVramInconclusiveNoSamples',
  run_failed: 'benchmarkVramInconclusiveRunFailed',
  isolation_lost: 'benchmarkVramInconclusiveIsolationLost',
  strategy_disagreement: 'benchmarkVramInconclusiveStrategyDisagreement',
};

const warningLabelKeys: Readonly<Record<string, MessageKey>> = {
  non_managed_applications: 'benchmarkVramWarningNonManaged',
  post_transport_agent: 'benchmarkVramWarningPostTransport',
  undeclared_gpu_allocation: 'benchmarkVramWarningUndeclaredGpu',
};

// Which field IDENTIFIED the card at measurement time -- a record of what the
// run captured, not of a comparison it made. The comparison happens later and
// elsewhere (`vramCardCheck`, when an operator is offered the number), so
// nothing here may read as "verified": `name_total` catches a swap between
// unlike cards only, two identical cards trading indices are indistinguishable
// that way, and an empty kind means no identifying field was available at all.
const fingerprintLabelKeys: Readonly<Record<string, MessageKey>> = {
  uuid: 'benchmarkVramFingerprintUuid',
  name_total: 'benchmarkVramFingerprintNameTotal',
};

/** The localized reason a run reached no number. Unknown → an honest fallback. */
export function vramInconclusiveLabelKey(reason: string): MessageKey {
  return inconclusiveLabelKeys[reason] ?? 'benchmarkVramInconclusiveUnknown';
}

/** The localized caveat that degraded a run's confidence without invalidating it. */
export function vramWarningLabelKey(warning: string): MessageKey {
  return warningLabelKeys[warning] ?? 'benchmarkVramWarningUnknown';
}

/** What identified the card, named — never a bare "verified". */
export function vramFingerprintLabelKey(kind: string | undefined): MessageKey {
  return (kind && fingerprintLabelKeys[kind]) || 'benchmarkVramFingerprintNone';
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
function vramLiveFingerprint(live: HardwareGPU, kind: string): string {
  if (kind === 'uuid') return (live.uuid ?? '').trim();
  const name = (live.name ?? '').trim();
  const total =
    live.memory_total_bytes > 0
      ? `${Math.floor(live.memory_total_bytes / VRAM_BYTES_PER_MB)} MB`
      : '';
  if (name && total) return `${name} / ${total}`;
  return name || total;
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
  | { state: 'verified'; kind: 'uuid' | 'name_total' }
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
  if (!recorded || (kind !== 'uuid' && kind !== 'name_total')) return { state: 'unverifiable' };
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
