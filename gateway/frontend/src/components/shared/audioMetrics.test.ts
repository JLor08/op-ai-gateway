// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { describe, expect, it } from 'vitest';
import {
  AUDIO_CONCURRENCY_LIMITS,
  audioMetricColumns,
  audioMetricsBody,
  audioMetricsForm,
  audioPairIncomplete,
  formatAudioConcurrency,
  formatAudioRate,
  formatAudioTtfa,
  isAudioConcurrencyLimit,
  minExceedsRealtime,
  minRealtimeWarning,
  servesAudio,
  type AudioMetrics,
  type AudioMetricsForm,
} from './audioMetrics';
import type { ModelServerCapability } from '../../api';
import { messages, type Locale } from '../../i18n';

function capability(name: string, verdict: 'yes' | 'no'): ModelServerCapability {
  return {
    capability: name,
    verdict,
    source: 'audiocpp_models',
    checked_at: '2026-11-01T12:00:00Z',
  };
}

describe('isAudioConcurrencyLimit', () => {
  it('accepts the three reasons a ramp stops, and nothing else', () => {
    expect(AUDIO_CONCURRENCY_LIMITS).toEqual(['performance', 'resources', 'cap']);
    for (const limit of AUDIO_CONCURRENCY_LIMITS) expect(isAudioConcurrencyLimit(limit)).toBe(true);
    for (const other of ['', 'Performance', 'unknown', undefined]) {
      expect(isAudioConcurrencyLimit(other)).toBe(false);
    }
  });
});

describe('formatAudioRate', () => {
  it('names a measured rate as a multiple of real time, an unmeasured one as a dash', () => {
    expect(formatAudioRate(3)).toBe('3.0×');
    expect(formatAudioRate(0.84)).toBe('0.84×');
    expect(formatAudioRate(0)).toBe('—');
    expect(formatAudioRate(undefined)).toBe('—');
  });

  // The voice gate refuses a rate below 1.2× by default: a figure near it
  // keeps the second decimal, and is cut, never rounded up, so a model the
  // gate refuses never reads as one at the threshold.
  it.each([
    [1.15, '1.15×'],
    [1.16, '1.16×'],
    [1.18, '1.18×'],
    [1.199, '1.19×'],
    [1.2, '1.2×'],
    [9.999, '9.99×'],
  ])('shows %s below 10× with up to two decimals as %s', (rate, text) => {
    expect(formatAudioRate(rate)).toBe(text);
  });

  it.each([
    [10, '10.0×'],
    [12.34, '12.3×'],
    [12.39, '12.3×'],
  ])('shows %s at 10× and above with one decimal as %s', (rate, text) => {
    expect(formatAudioRate(rate)).toBe(text);
  });
});

describe('formatAudioTtfa', () => {
  it('shows whole milliseconds, an unmeasured time as a dash', () => {
    expect(formatAudioTtfa(250)).toBe('250');
    expect(formatAudioTtfa(249.6)).toBe('250');
    expect(formatAudioTtfa(0)).toBe('—');
    expect(formatAudioTtfa(undefined)).toBe('—');
  });
});

for (const locale of ['de', 'en'] as readonly Locale[]) {
  const t = messages[locale];

  describe(`formatAudioConcurrency [${locale}]`, () => {
    it('shows a ramp stopped by performance as the measured count', () => {
      expect(formatAudioConcurrency(2, 'performance', t)).toBe('2');
    });

    // A measured 0 is a result, not an unknown: not even one stream kept up.
    it('names a measured 0 as not real-time', () => {
      expect(formatAudioConcurrency(0, 'performance', t)).toBe(
        t.audioRealtimeConcurrency(0, 'performance'),
      );
      expect(formatAudioConcurrency(0, 'performance', t)).not.toBe('0');
      expect(formatAudioConcurrency(0, 'performance', t)).not.toBe('—');
    });

    // Resources and the cap end the ramp before the model ran out of speed.
    it.each(['resources', 'cap'])('shows a ramp stopped by %s as a lower bound', (limit) => {
      expect(formatAudioConcurrency(2, limit, t)).toBe('≥ 2');
      expect(formatAudioConcurrency(0, limit, t)).toBe('≥ 0');
    });

    // Without a limit the count means nothing, whatever it holds.
    it('shows an unknown limit as a dash, whatever the count', () => {
      expect(formatAudioConcurrency(3, '', t)).toBe('—');
      expect(formatAudioConcurrency(3, undefined, t)).toBe('—');
      expect(formatAudioConcurrency(3, 'something-newer', t)).toBe('—');
      expect(formatAudioConcurrency(undefined, undefined, t)).toBe('—');
    });
  });

  describe(`audioMetricColumns [${locale}]`, () => {
    type Row = AudioMetrics & { capabilities: ModelServerCapability[] };
    const audioRow: Row = {
      audio_rate_x: 3,
      audio_ttfa_ms: 250,
      audio_realtime_concurrency: 2,
      audio_concurrency_limit: 'resources',
      capabilities: [],
    };
    const textRow: Row = { capabilities: [] };

    it('offers the three audio columns, labelled, for a model that serves audio', () => {
      const columns = audioMetricColumns(t, [audioRow, textRow]);
      expect(columns.map((c) => c.label)).toEqual([
        t.modelServersAudioRate,
        t.modelServersAudioTtft,
        t.modelServersAudioConcurrency,
      ]);
      expect(columns.every((c) => c.available)).toBe(true);
      expect(columns.map((c) => c.render?.(audioRow))).toEqual(['3.0×', '250', '≥ 2']);
      expect(columns.map((c) => c.render?.(textRow))).toEqual(['—', '—', '—']);
    });

    it('leaves them out for a model no row of which serves audio', () => {
      expect(audioMetricColumns(t, [textRow]).every((c) => c.available === false)).toBe(true);
    });

    // Sorted as numbers, with an unmeasured cell at the bottom (a blank value).
    it('sorts by the figures, an unmeasured cell blank', () => {
      const columns = audioMetricColumns(t, [audioRow]);
      expect(columns.map((c) => c.value(audioRow))).toEqual(['3', '250', '2']);
      expect(columns.map((c) => c.value(textRow))).toEqual(['', '', '']);
      expect(columns.every((c) => c.numeric)).toBe(true);
    });
  });
}

describe('servesAudio', () => {
  it('holds for a row with a measured audio metric', () => {
    expect(servesAudio({ audio_rate_x: 1.5, capabilities: [] })).toBe(true);
    expect(servesAudio({ audio_ttfa_ms: 300, capabilities: [] })).toBe(true);
    // A measured 0 with its limit is a measurement too.
    expect(
      servesAudio({
        audio_realtime_concurrency: 0,
        audio_concurrency_limit: 'performance',
        capabilities: [],
      }),
    ).toBe(true);
  });

  // An audio model measured never yet still has its columns, as dashes.
  it('holds for a speech or transcription model not yet measured', () => {
    expect(servesAudio({ capabilities: [capability('speech', 'yes')] })).toBe(true);
    expect(servesAudio({ capabilities: [capability('transcription', 'yes')] })).toBe(true);
  });

  it('does not hold for a text model', () => {
    expect(servesAudio({ capabilities: [] })).toBe(false);
    expect(servesAudio({ capabilities: [capability('speech', 'no')] })).toBe(false);
    expect(servesAudio({ capabilities: [capability('vision', 'yes')] })).toBe(false);
    expect(servesAudio({ audio_realtime_concurrency: 4, capabilities: [] })).toBe(false);
  });
});

describe('audioMetricsForm', () => {
  it('seeds the editor from a mapping, an unmeasured value as an empty field', () => {
    expect(
      audioMetricsForm({
        audio_rate_x: 2.5,
        audio_ttfa_ms: 300,
        audio_realtime_concurrency: 2,
        audio_concurrency_limit: 'performance',
      }),
    ).toEqual({ rateText: '2.5', ttfaText: '300', countText: '2', limit: 'performance' });
    expect(audioMetricsForm(null)).toEqual({
      rateText: '',
      ttfaText: '',
      countText: '',
      limit: '',
    });
  });

  // A measured 0 is shown as 0, not as an empty field: it is a result.
  it('shows a measured count of 0 next to its limit', () => {
    expect(
      audioMetricsForm({ audio_realtime_concurrency: 0, audio_concurrency_limit: 'performance' })
        .countText,
    ).toBe('0');
  });

  // Without a limit the stored count means nothing, so the field is empty.
  it('leaves the count empty without a limit, or with one this build does not know', () => {
    expect(
      audioMetricsForm({ audio_realtime_concurrency: 3, audio_concurrency_limit: '' }),
    ).toEqual({ rateText: '', ttfaText: '', countText: '', limit: '' });
    expect(
      audioMetricsForm({ audio_realtime_concurrency: 3, audio_concurrency_limit: 'newer' }).limit,
    ).toBe('');
  });
});

describe('audioMetricsBody', () => {
  const seed: AudioMetricsForm = {
    rateText: '2.5',
    ttfaText: '300',
    countText: '2',
    limit: 'performance',
  };

  // Machine-owned like the text metrics: an unchanged value is left to the
  // benchmark that wrote it.
  it('sends nothing for an untouched editor', () => {
    expect(audioMetricsBody(seed, seed)).toEqual({});
    expect(audioMetricsBody({ ...seed, rateText: ' 2.5 ' }, seed)).toEqual({});
  });

  it('sends a changed rate or time on its own', () => {
    expect(audioMetricsBody({ ...seed, rateText: '3' }, seed)).toEqual({ audio_rate_x: 3 });
    expect(audioMetricsBody({ ...seed, ttfaText: '' }, seed)).toEqual({ audio_ttfa_ms: 0 });
  });

  // The count and its limit are one result: either one changed sends both.
  it('sends the count and its limit together when either changes', () => {
    expect(audioMetricsBody({ ...seed, countText: '3' }, seed)).toEqual({
      audio_realtime_concurrency: 3,
      audio_concurrency_limit: 'performance',
    });
    expect(audioMetricsBody({ ...seed, limit: 'resources' }, seed)).toEqual({
      audio_realtime_concurrency: 2,
      audio_concurrency_limit: 'resources',
    });
  });

  // Beside a limit, 0 is a result ("not real-time even alone", a ceiling the
  // scaler obeys), so a blank or unusable count is never sent as one: the pair
  // stays as stored. The editor refuses such a save (audioPairIncomplete).
  it('never sends a blank or unusable count as a measured 0 beside a chosen limit', () => {
    const none: AudioMetricsForm = { rateText: '', ttfaText: '', countText: '', limit: '' };
    expect(audioMetricsBody({ ...none, limit: 'performance' }, none)).toEqual({});
    for (const countText of ['', '  ', '1.5', '-1', 'abc']) {
      expect(audioMetricsBody({ ...seed, countText }, seed), countText).toEqual({});
      expect(audioMetricsBody({ ...seed, countText, limit: 'cap' }, seed), countText).toEqual({});
    }
  });

  it('sends a typed 0 beside a limit as the result it is', () => {
    const none: AudioMetricsForm = { rateText: '', ttfaText: '', countText: '', limit: '' };
    expect(audioMetricsBody({ ...none, limit: 'performance', countText: '0' }, none)).toEqual({
      audio_realtime_concurrency: 0,
      audio_concurrency_limit: 'performance',
    });
  });

  // An unknown limit clears both, and a count sent with it would be a count
  // without a limit -- whatever the hidden count field still holds.
  it('clears the pair with an empty limit alone', () => {
    expect(audioMetricsBody({ ...seed, limit: '', countText: '' }, seed)).toEqual({
      audio_concurrency_limit: '',
    });
    expect(audioMetricsBody({ ...seed, limit: '' }, seed)).toEqual({
      audio_concurrency_limit: '',
    });
  });
});

describe('audioPairIncomplete', () => {
  const form = (limit: AudioMetricsForm['limit'], countText: string): AudioMetricsForm => ({
    rateText: '',
    ttfaText: '',
    countText,
    limit,
  });

  // A limit is half a result: the count the ramp reached belongs beside it.
  it('holds for a limit without a whole count of 0 or more', () => {
    for (const countText of ['', '   ', '1.5', '-1', 'abc']) {
      expect(audioPairIncomplete(form('performance', countText)), countText).toBe(true);
      expect(audioPairIncomplete(form('resources', countText)), countText).toBe(true);
    }
  });

  it('does not hold for a limit with its count, a 0 included', () => {
    for (const countText of ['0', '2', ' 3 ']) {
      expect(audioPairIncomplete(form('cap', countText)), countText).toBe(false);
    }
  });

  // An unknown limit clears the pair, so no count is wanted beside it.
  it('does not hold without a limit, whatever the count field holds', () => {
    for (const countText of ['', '2', 'abc']) {
      expect(audioPairIncomplete(form('', countText)), countText).toBe(false);
    }
  });
});

describe('minExceedsRealtime', () => {
  // The gateway's rule for instances_min_exceeds_realtime: only a ramp
  // stopped by performance measured a ceiling.
  it('holds for a minimum above a measured ceiling', () => {
    const ceiling: AudioMetrics = {
      audio_realtime_concurrency: 2,
      audio_concurrency_limit: 'performance',
    };
    expect(minExceedsRealtime(3, ceiling)).toBe(true);
    expect(minExceedsRealtime(2, ceiling)).toBe(false);
    expect(minExceedsRealtime(0, ceiling)).toBe(false);
    // Not real-time even alone: any minimum is above it.
    expect(
      minExceedsRealtime(1, {
        audio_realtime_concurrency: 0,
        audio_concurrency_limit: 'performance',
      }),
    ).toBe(true);
  });

  it('does not hold for a lower bound, an unknown limit or no metrics', () => {
    for (const limit of ['resources', 'cap', '', undefined]) {
      expect(
        minExceedsRealtime(5, { audio_realtime_concurrency: 2, audio_concurrency_limit: limit }),
        String(limit),
      ).toBe(false);
    }
    expect(minExceedsRealtime(5, undefined)).toBe(false);
  });
});

describe('minRealtimeWarning', () => {
  const ceiling: AudioMetrics = {
    audio_realtime_concurrency: 2,
    audio_concurrency_limit: 'performance',
  };
  const stored = (min: number, flagged: boolean) => ({
    min_instances: min,
    instances_min_exceeds_realtime: flagged,
  });

  // The warning follows the minimum as typed, not the last save.
  it('warns as soon as the typed minimum rises above the measured ceiling', () => {
    expect(minRealtimeWarning(1, stored(1, false), ceiling)).toBe(false);
    expect(minRealtimeWarning(3, stored(1, false), ceiling)).toBe(true);
  });

  it('drops the stored warning once the typed minimum is back at the ceiling', () => {
    expect(minRealtimeWarning(5, stored(5, true), ceiling)).toBe(true);
    expect(minRealtimeWarning(2, stored(5, true), ceiling)).toBe(false);
  });

  // For the stored minimum the gateway's flag is the verdict, and a flagged
  // minimum stays flagged when raised further, whatever the row says.
  it('takes the gateway flag for the stored minimum and any minimum above it', () => {
    expect(minRealtimeWarning(3, stored(3, true), undefined)).toBe(true);
    expect(minRealtimeWarning(4, stored(3, true), undefined)).toBe(true);
    expect(minRealtimeWarning(3, stored(3, false), ceiling)).toBe(false);
  });

  // A new spec has no stored minimum: the rule alone decides.
  it('applies the rule alone without a stored spec', () => {
    expect(minRealtimeWarning(3, undefined, ceiling)).toBe(true);
    expect(minRealtimeWarning(2, undefined, ceiling)).toBe(false);
    expect(minRealtimeWarning(3, undefined, undefined)).toBe(false);
  });
});
