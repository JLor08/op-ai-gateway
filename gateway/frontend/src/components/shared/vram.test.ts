// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { describe, expect, it } from 'vitest';
import {
  latestApplicableVramRun,
  vramApplyNumber,
  vramFingerprintLabelKey,
  vramInconclusiveLabelKey,
  vramWarningLabelKey,
} from './vram';
import { messages } from '../../i18n';
import type { BenchmarkRunDTO, VRAMGPUItemDTO, VRAMReportDTO } from '../../api';

function gpu(over: Partial<VRAMGPUItemDTO> = {}): VRAMGPUItemDTO {
  return { index: 0, baseline_used_mb: 1024, attributable: true, ...over };
}

function report(over: Partial<VRAMReportDTO> = {}): VRAMReportDTO {
  return { isolated: true, gpus: [gpu({ delta_mb: 22528 })], ...over };
}

function run(over: Partial<BenchmarkRunDTO> = {}): BenchmarkRunDTO {
  return {
    id: 'run_1',
    mapping_id: 'map_1',
    server_id: 'srv_1',
    created_at: '2026-09-01T10:00:00Z',
    gen_tokens_per_second: 0,
    prompt_tokens_per_second: 0,
    load_time_ms: 0,
    context_size: 0,
    error: '',
    kind: 'vram',
    vram: report(),
    ...over,
  };
}

describe('vramApplyNumber', () => {
  it('prefers the delta — the figure that exists on every host class', () => {
    expect(vramApplyNumber(gpu({ delta_mb: 22528, measured_mb: 21000 }))).toEqual({
      mb: 22528,
      source: 'delta',
    });
  });

  it('falls back to the agent measurement when there is no delta', () => {
    expect(vramApplyNumber(gpu({ measured_mb: 21000 }))).toEqual({ mb: 21000, source: 'measured' });
  });

  it('offers NOTHING for a zero or missing number — 0 means unknown, never 0 MB', () => {
    expect(vramApplyNumber(gpu({ delta_mb: 0, measured_mb: 0 }))).toBeNull();
    expect(vramApplyNumber(gpu())).toBeNull();
    // A negative reading is nonsense rather than a small model.
    expect(vramApplyNumber(gpu({ delta_mb: -5 }))).toBeNull();
    expect(vramApplyNumber(undefined)).toBeNull();
  });
});

describe('latestApplicableVramRun', () => {
  it('takes the newest definitive vram row (the list is newest-first)', () => {
    const found = latestApplicableVramRun([
      run({ id: 'newest', created_at: '2026-09-02T10:00:00Z' }),
      run({ id: 'older', created_at: '2026-09-01T10:00:00Z' }),
    ]);
    expect(found?.run.id).toBe('newest');
  });

  it('ignores every other run kind, even one carrying a vram payload by mistake', () => {
    expect(latestApplicableVramRun([run({ kind: 'speed' })])).toBeNull();
    expect(latestApplicableVramRun([run({ kind: 'capacity' })])).toBeNull();
    expect(latestApplicableVramRun([run({ kind: '' })])).toBeNull();
  });

  it('skips an INCONCLUSIVE row and finds the definitive one behind it', () => {
    const found = latestApplicableVramRun([
      run({ id: 'inconclusive', vram: report({ inconclusive: 'below_floor', gpus: [] }) }),
      run({ id: 'definitive' }),
    ]);
    expect(found?.run.id).toBe('definitive');
  });

  it('skips a row whose isolation was never proven', () => {
    // A number measured while something else may have been serving the model is
    // not a number to offer for the operator's own field.
    expect(latestApplicableVramRun([run({ vram: report({ isolated: false }) })])).toBeNull();
  });

  it('skips a row with no applicable number at all', () => {
    expect(
      latestApplicableVramRun([run({ vram: report({ gpus: [gpu({ delta_mb: 0 })] }) })]),
    ).toBeNull();
    expect(latestApplicableVramRun([run({ vram: report({ gpus: [] }) })])).toBeNull();
    expect(latestApplicableVramRun([run({ vram: undefined })])).toBeNull();
    expect(latestApplicableVramRun([])).toBeNull();
  });
});

describe('vram label keys', () => {
  it('names an action for every inconclusive reason the backend can report', () => {
    const reasons = [
      'isolation_timeout',
      'baseline_unstable',
      'post_load_unstable',
      'already_resident',
      'below_floor',
      'no_samples',
      'run_failed',
    ] as const;
    const seen = new Set<string>();
    for (const reason of reasons) {
      const key = vramInconclusiveLabelKey(reason);
      // Distinct texts, in both locales: the reason IS the next action, so two
      // reasons sharing one sentence would send an operator to the wrong place.
      seen.add(key);
      for (const locale of ['de', 'en'] as const) {
        expect(messages[locale][key].length).toBeGreaterThan(20);
      }
    }
    expect(seen.size).toBe(reasons.length);
  });

  it('falls back to an honest unknown for a reason this build does not know', () => {
    expect(vramInconclusiveLabelKey('something_new')).toBe('benchmarkVramInconclusiveUnknown');
    expect(vramInconclusiveLabelKey('')).toBe('benchmarkVramInconclusiveUnknown');
  });

  it('labels both warnings and falls back for an unknown one', () => {
    expect(vramWarningLabelKey('non_managed_applications')).toBe('benchmarkVramWarningNonManaged');
    expect(vramWarningLabelKey('post_transport_agent')).toBe('benchmarkVramWarningPostTransport');
    expect(vramWarningLabelKey('something_new')).toBe('benchmarkVramWarningUnknown');
  });

  it('never claims a bare "verified": each fingerprint kind says what was compared', () => {
    expect(vramFingerprintLabelKey('uuid')).toBe('benchmarkVramFingerprintUuid');
    expect(vramFingerprintLabelKey('name_total')).toBe('benchmarkVramFingerprintNameTotal');
    // No identifying field at all, and anything this build does not know, both
    // read as NOT verified rather than as a check that was never made.
    expect(vramFingerprintLabelKey('')).toBe('benchmarkVramFingerprintNone');
    expect(vramFingerprintLabelKey(undefined)).toBe('benchmarkVramFingerprintNone');
    expect(vramFingerprintLabelKey('something_new')).toBe('benchmarkVramFingerprintNone');
    for (const locale of ['de', 'en'] as const) {
      expect(messages[locale].benchmarkVramFingerprintNameTotal).toMatch(/identi/i);
    }
  });
});
