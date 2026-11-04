// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

// The pure side of a mapping's audio metrics (#340): how they read in a cell,
// which rows have them, the mapping editor's rules for sending them, and the
// spec editor's warning about a minimum above the measured ceiling. Kept out
// of the large components so each rule is testable on its own.

import type {
  ModelServerCapability,
  PortalModelMapping,
  RuntimeSpec,
  UpdateMappingRequest,
} from '../../api';
import type { ListColumn } from './ListTable';
import {
  audioLimitOf,
  formatRateX,
  isAudioConcurrencyLimit,
  type AudioConcurrencyLimit,
} from './audioFigures';
import { parseMetricInput } from './format';
import type { Translation } from './types';

export {
  AUDIO_CONCURRENCY_LIMITS,
  audioLimitOf,
  isAudioConcurrencyLimit,
  type AudioConcurrencyLimit,
} from './audioFigures';

/** The four audio metrics as a mapping and a model-servers row carry them. */
export type AudioMetrics = Pick<
  PortalModelMapping,
  'audio_rate_x' | 'audio_ttfa_ms' | 'audio_realtime_concurrency' | 'audio_concurrency_limit'
>;

/** A single-stream rate as a multiple of real time (formatRateX); 0 is not measured. */
export function formatAudioRate(rateX: number | undefined): string {
  return rateX && rateX > 0 ? formatRateX(rateX) : '—';
}

/** The time to the first audio or text in whole ms; 0 is not measured. */
export function formatAudioTtfa(ms: number | undefined): string {
  return ms && ms > 0 ? String(Math.round(ms)) : '—';
}

/**
 * The real-time concurrency as a cell reads it. The count is one result with
 * its limit: without one of the three limits no ramp ran, so the count means
 * nothing and reads as a dash whatever it holds (a value from a newer gateway
 * included). With one, the figure says whether it is a measured ceiling or a
 * lower bound (t.audioRealtimeConcurrency).
 */
export function formatAudioConcurrency(
  count: number | undefined,
  limit: string | undefined,
  t: Translation,
): string {
  return isAudioConcurrencyLimit(limit) ? t.audioRealtimeConcurrency(count ?? 0, limit) : '—';
}

/** Whether any audio metric of a row was measured (a ramp's 0 included). */
function hasAudioMetrics(row: AudioMetrics): boolean {
  return (
    (row.audio_rate_x ?? 0) > 0 ||
    (row.audio_ttfa_ms ?? 0) > 0 ||
    isAudioConcurrencyLimit(row.audio_concurrency_limit)
  );
}

const AUDIO_CAPABILITIES: ReadonlySet<string> = new Set(['speech', 'transcription']);

type AudioRow = AudioMetrics & { capabilities: ModelServerCapability[] };

/**
 * Whether a model-servers row serves audio: it has an audio metric, or it is a
 * speech or transcription model not yet measured, whose cells then read as
 * dashes rather than vanish.
 */
export function servesAudio(row: AudioRow): boolean {
  return (
    hasAudioMetrics(row) ||
    row.capabilities.some((c) => AUDIO_CAPABILITIES.has(c.capability) && c.verdict === 'yes')
  );
}

/**
 * The models catalog's three audio columns, shared by the per-model and the
 * per-group server lists. They are offered only while some row serves audio:
 * for a text model they would be three columns of dashes. A cell's value is
 * the bare figure, so the columns sort as numbers, and an unmeasured cell's is
 * blank, so it sorts last.
 */
export function audioMetricColumns<Row extends AudioRow>(
  t: Translation,
  rows: readonly Row[],
): ListColumn<Row>[] {
  const available = rows.some(servesAudio);
  return [
    {
      id: 'audioRate',
      label: t.modelServersAudioRate,
      numeric: true,
      available,
      value: (r) => ((r.audio_rate_x ?? 0) > 0 ? String(r.audio_rate_x) : ''),
      render: (r) => formatAudioRate(r.audio_rate_x),
    },
    {
      id: 'audioTtfa',
      label: t.modelServersAudioTtft,
      numeric: true,
      available,
      value: (r) => ((r.audio_ttfa_ms ?? 0) > 0 ? String(r.audio_ttfa_ms) : ''),
      render: (r) => formatAudioTtfa(r.audio_ttfa_ms),
    },
    {
      id: 'audioConcurrency',
      label: t.modelServersAudioConcurrency,
      numeric: true,
      available,
      value: (r) =>
        isAudioConcurrencyLimit(r.audio_concurrency_limit)
          ? String(r.audio_realtime_concurrency ?? 0)
          : '',
      render: (r) =>
        formatAudioConcurrency(r.audio_realtime_concurrency, r.audio_concurrency_limit, t),
    },
  ];
}

/** The mapping editor's four audio fields, as typed. */
export type AudioMetricsForm = {
  rateText: string;
  ttfaText: string;
  countText: string;
  limit: AudioConcurrencyLimit | '';
};

/**
 * The editor's fields seeded from a mapping (null: the create form). An
 * unmeasured rate or time is an empty field. The count is shown only beside a
 * limit -- a measured 0 included, which is a result -- and a limit this build
 * does not know reads as unknown, like a missing one.
 */
export function audioMetricsForm(row: AudioMetrics | null): AudioMetricsForm {
  const limit = audioLimitOf(row?.audio_concurrency_limit);
  return {
    rateText: row?.audio_rate_x ? String(row.audio_rate_x) : '',
    ttfaText: row?.audio_ttfa_ms ? String(row.audio_ttfa_ms) : '',
    countText: limit ? String(row?.audio_realtime_concurrency ?? 0) : '',
    limit,
  };
}

export type AudioMetricsBody = Pick<
  UpdateMappingRequest,
  'audio_rate_x' | 'audio_ttfa_ms' | 'audio_realtime_concurrency' | 'audio_concurrency_limit'
>;

/** The count field's text as the whole count of 0 or more it must be, else null. */
function audioCountFromText(text: string): number | null {
  const n = Number(text.trim());
  return text.trim() !== '' && Number.isInteger(n) && n >= 0 ? n : null;
}

/**
 * Whether the editor holds a limit without the count that belongs beside it:
 * a blank field, or one that is not a whole number of 0 or more. Beside a
 * limit, 0 is a result -- not real-time even with one stream, a ceiling the
 * scaler obeys -- so a blank field cannot stand for "unknown" there the way
 * it does for the rate and the time. The editor names the field and refuses
 * the save; "unknown" is the limit's own choice, which clears both.
 */
export function audioPairIncomplete(form: AudioMetricsForm): boolean {
  return form.limit !== '' && audioCountFromText(form.countText) === null;
}

/**
 * The audio half of the mapping update: like the text metrics, a field is sent
 * only when the operator changed it from the value the form opened with
 * (`seed`), because a benchmark writes these too and an unchanged one must
 * stay the benchmark's.
 *
 * The count and its limit are one result, so either one changed sends both --
 * the gateway refuses a count without one of the three limits beside it. An
 * unknown limit clears both and goes alone: a count beside it would be a count
 * without a limit, whatever the hidden count field still holds. A limit
 * without its count (audioPairIncomplete) sends neither and leaves the stored
 * pair as it is: a blank read as 0 would store a measured "not real-time".
 */
export function audioMetricsBody(form: AudioMetricsForm, seed: AudioMetricsForm): AudioMetricsBody {
  const body: AudioMetricsBody = {};
  if (form.rateText.trim() !== seed.rateText.trim()) {
    body.audio_rate_x = parseMetricInput(form.rateText);
  }
  if (form.ttfaText.trim() !== seed.ttfaText.trim()) {
    body.audio_ttfa_ms = parseMetricInput(form.ttfaText);
  }
  const countChanged = form.limit !== '' && form.countText.trim() !== seed.countText.trim();
  if (form.limit !== seed.limit || countChanged) {
    const count = audioCountFromText(form.countText);
    if (form.limit === '') {
      body.audio_concurrency_limit = '';
    } else if (count !== null) {
      body.audio_realtime_concurrency = count;
      body.audio_concurrency_limit = form.limit;
    }
  }
  return body;
}

/**
 * Whether a launch spec's minimum instance count is above the real-time
 * concurrency its mapping's audio ramp measured as a ceiling: the gateway's
 * rule for a spec's instances_min_exceeds_realtime. Only a ramp stopped by
 * performance measured one; resources and the cap leave a lower bound, which
 * no minimum exceeds.
 */
export function minExceedsRealtime(
  minInstances: number,
  metrics: AudioMetrics | undefined,
): boolean {
  return (
    metrics?.audio_concurrency_limit === 'performance' &&
    minInstances > (metrics.audio_realtime_concurrency ?? 0)
  );
}

/**
 * The spec editor's real-time warning for the minimum as typed, so it stands
 * beside the edit that causes it -- the save closes the editor -- and leaves
 * with an edit that resolves it. For the stored minimum the gateway's own
 * flag is the verdict, and a flagged minimum stays flagged when raised
 * further. Any other minimum is judged by the gateway's rule against the
 * mapping's measured figures (minExceedsRealtime), as is every minimum of a
 * spec not yet stored.
 */
export function minRealtimeWarning(
  minInstances: number,
  stored: Pick<RuntimeSpec, 'min_instances' | 'instances_min_exceeds_realtime'> | undefined,
  metrics: AudioMetrics | undefined,
): boolean {
  if (stored?.instances_min_exceeds_realtime && minInstances >= stored.min_instances) return true;
  if (stored && minInstances === stored.min_instances) return false;
  return minExceedsRealtime(minInstances, metrics);
}
