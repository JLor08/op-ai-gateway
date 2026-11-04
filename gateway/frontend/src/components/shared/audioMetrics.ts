// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

// The pure side of a mapping's audio metrics (#340): how they read in a cell,
// which rows have them, and the mapping editor's rules for sending them. Kept
// out of the large components so each rule is testable on its own.

import type { ModelServerCapability, PortalModelMapping, UpdateMappingRequest } from '../../api';
import type { ListColumn } from './ListTable';
import { parseMetricInput } from './format';
import type { Translation } from './types';

/** Why an audio concurrency ramp stopped: the closed set the gateway writes. */
export const AUDIO_CONCURRENCY_LIMITS = ['performance', 'resources', 'cap'] as const;
export type AudioConcurrencyLimit = (typeof AUDIO_CONCURRENCY_LIMITS)[number];

/** The four audio metrics as a mapping and a model-servers row carry them. */
export type AudioMetrics = Pick<
  PortalModelMapping,
  'audio_rate_x' | 'audio_ttfa_ms' | 'audio_realtime_concurrency' | 'audio_concurrency_limit'
>;

export function isAudioConcurrencyLimit(value: string | undefined): value is AudioConcurrencyLimit {
  return (AUDIO_CONCURRENCY_LIMITS as readonly (string | undefined)[]).includes(value);
}

/** A single-stream rate as a multiple of real time; 0 is not measured. */
export function formatAudioRate(rateX: number | undefined): string {
  return rateX && rateX > 0 ? `${rateX.toFixed(1)}×` : '—';
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
  const limit = isAudioConcurrencyLimit(row?.audio_concurrency_limit)
    ? row.audio_concurrency_limit
    : '';
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

/**
 * The audio half of the mapping update: like the text metrics, a field is sent
 * only when the operator changed it from the value the form opened with
 * (`seed`), because a benchmark writes these too and an unchanged one must
 * stay the benchmark's.
 *
 * The count and its limit are one result, so either one changed sends both --
 * the gateway refuses a count without one of the three limits beside it. An
 * unknown limit clears both and goes alone: a count beside it would be a count
 * without a limit.
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
    if (form.limit === '') {
      body.audio_concurrency_limit = '';
    } else {
      body.audio_realtime_concurrency = parseMetricInput(form.countText);
      body.audio_concurrency_limit = form.limit;
    }
  }
  return body;
}
