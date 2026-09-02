// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import type { BenchmarkRunDTO, VRAMGPUItemDTO, VRAMReportDTO } from '../../api';
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

// What the run actually COMPARED to decide the number belongs to this card.
// Deliberately no "verified" without a qualifier: `name_total` catches a swap
// between unlike cards only, and two identical cards trading indices are
// indistinguishable that way.
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
