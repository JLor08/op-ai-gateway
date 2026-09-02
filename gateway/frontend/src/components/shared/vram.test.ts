// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { describe, expect, it } from 'vitest';
import {
  latestApplicableVramRun,
  vramApplyNumber,
  vramCardCheck,
  vramCardCheckLabelKey,
  vramFingerprintKinds,
  vramFingerprintLabelKey,
  vramInconclusiveLabelKey,
  vramInconclusiveReasons,
  vramWarningLabelKey,
  vramWarnings,
} from './vram';
import { messages } from '../../i18n';
import type { BenchmarkRunDTO, HardwareGPU, VRAMGPUItemDTO, VRAMReportDTO } from '../../api';
import type { MessageKey } from './types';

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

  it('skips an INCONCLUSIVE row EVEN WHEN it carries a positive number', () => {
    // The number is deliberately present and applicable, so this pins the
    // `inconclusive` gate ITSELF rather than the "at least one positive
    // number" gate behind it -- with `gpus: []` the row is rejected either
    // way and deleting the reason check changes nothing.
    //
    // The shape is not hypothetical: the reason vocabulary is CLOSED and
    // PERSISTED inside a history row's vram_json, so this portal decodes rows
    // written by other gateway builds, and each of the four gates has to hold
    // on the payload alone rather than on a producer invariant.
    const found = latestApplicableVramRun([
      run({
        id: 'inconclusive',
        vram: report({ inconclusive: 'below_floor', gpus: [gpu({ delta_mb: 22528 })] }),
      }),
      run({ id: 'definitive' }),
    ]);
    expect(found?.run.id).toBe('definitive');
    // And with nothing definitive behind it, an inconclusive row is no offer
    // at all -- never a fallback.
    expect(
      latestApplicableVramRun([
        run({ vram: report({ inconclusive: 'below_floor', gpus: [gpu({ delta_mb: 22528 })] }) }),
      ]),
    ).toBeNull();
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

// The mapping is pinned by ITERATING each declared vocabulary rather than by
// listing its values here. A list is what failed: three values shipped
// (`isolation_lost`, `strategy_disagreement`, `undeclared_gpu_allocation`)
// while the lists here and in i18n.test.ts still held seven reasons and two
// warnings, so the tests were green about a mapping they no longer covered --
// and their titles asserted a count that was false. Iterating means the next
// value added to `vram.ts` is under test the moment it is declared, and the
// only way to add one without a sentence of its own is to make these red.
describe('vram label keys', () => {
  /**
   * Every declared value of one vocabulary reaches its OWN sentence, in BOTH
   * locales.
   *
   * Three ways this fails, which are the three ways the mapping can lie:
   * a value that falls through to the unknown fallback (declared but never
   * mapped), two values sharing a label key (a copy-paste in the map), and two
   * distinct keys carrying the same text in some locale (a copy-paste in
   * `i18n.ts` -- green under a key-only check, and the operator still reads
   * the wrong next action).
   */
  function expectDistinctSentences(
    values: readonly string[],
    labelKey: (value: string) => MessageKey,
    fallback: MessageKey,
    minLength: number,
  ) {
    const keys = new Set<MessageKey>();
    const sentences = { de: new Set<string>(), en: new Set<string>() };
    for (const value of values) {
      const key = labelKey(value);
      expect(key, `${value} is declared but not mapped: it falls back to ${fallback}`).not.toBe(
        fallback,
      );
      keys.add(key);
      for (const locale of ['de', 'en'] as const) {
        const text = messages[locale][key];
        expect(
          text.length,
          `${value} -> ${key} (${locale}) is too short to name an action`,
        ).toBeGreaterThan(minLength);
        sentences[locale].add(text);
      }
    }
    expect(keys.size, `${values.length} values share fewer than ${values.length} label keys`).toBe(
      values.length,
    );
    for (const locale of ['de', 'en'] as const) {
      expect(sentences[locale].size, `two of these values render the SAME ${locale} sentence`).toBe(
        values.length,
      );
    }
  }

  it('names a distinct action for every declared inconclusive reason, in both locales', () => {
    // The reason IS the next action, so two reasons sharing one sentence send
    // an operator to the wrong place -- worse than "no result" with no
    // explanation at all.
    expectDistinctSentences(
      vramInconclusiveReasons,
      vramInconclusiveLabelKey,
      'benchmarkVramInconclusiveUnknown',
      30,
    );
  });

  it('falls back to an honest unknown for a reason this build does not know', () => {
    expect(vramInconclusiveLabelKey('something_new')).toBe('benchmarkVramInconclusiveUnknown');
    expect(vramInconclusiveLabelKey('')).toBe('benchmarkVramInconclusiveUnknown');
  });

  it('names a distinct caveat for every declared warning, in both locales', () => {
    expectDistinctSentences(vramWarnings, vramWarningLabelKey, 'benchmarkVramWarningUnknown', 30);
  });

  it('says what to DO about the two caveats whose reason is invisible otherwise', () => {
    // These two are not covered by distinctness alone, because what makes them
    // useful is one specific instruction each.
    for (const locale of ['de', 'en'] as const) {
      // The undeclared card: every per-card number is correct, the SPEC is
      // what is wrong, so the caveat has to name the missing GPU row rather
      // than cast doubt on the measurement.
      expect(messages[locale][vramWarningLabelKey('undeclared_gpu_allocation')]).toMatch(
        /GPU[- ](row|Zeile)/i,
      );
      // The contamination check the run could not MAKE -- an application with
      // no loaded-models endpoint, which is most agent-managed ones. Without
      // naming that endpoint the operator only ever sees the wrong reason (a
      // sub-floor delta) for a model something else was already serving.
      expect(messages[locale][vramWarningLabelKey('residency_unknown')]).toMatch(
        /loaded_models_path/,
      );
    }
  });

  it('falls back to an honest unknown for a warning this build does not know', () => {
    expect(vramWarningLabelKey('something_new')).toBe('benchmarkVramWarningUnknown');
    expect(vramWarningLabelKey('')).toBe('benchmarkVramWarningUnknown');
  });

  it('never claims a bare "verified": each fingerprint kind says what was compared', () => {
    // Short label fragments, not sentences ("by UUID"), so the length floor is
    // the only thing that differs from the vocabularies above.
    expectDistinctSentences(
      vramFingerprintKinds,
      vramFingerprintLabelKey,
      'benchmarkVramFingerprintNone',
      5,
    );
    // No identifying field at all, and anything this build does not know, both
    // read as NOT verified rather than as a check that was never made.
    expect(vramFingerprintLabelKey('')).toBe('benchmarkVramFingerprintNone');
    expect(vramFingerprintLabelKey(undefined)).toBe('benchmarkVramFingerprintNone');
    expect(vramFingerprintLabelKey('something_new')).toBe('benchmarkVramFingerprintNone');
    for (const locale of ['de', 'en'] as const) {
      // name+total cannot tell two identical cards apart and has to say so.
      expect(messages[locale][vramFingerprintLabelKey('name_total')]).toMatch(/identi/i);
    }
  });
});

// The reader the recorded fingerprint was always for. Without it the value is
// written, shipped through the DTO and never compared, while a renumbering
// silently redirects a stored number onto whatever card now sits at that
// index -- with the history table still naming the UUID that was captured.
describe('vramCardCheck', () => {
  const nvidia: HardwareGPU = {
    index: 0,
    name: 'NVIDIA RTX 6000',
    uuid: 'GPU-aaa',
    memory_total_bytes: 24576 * 1048576,
  };
  // No UUID: the ROCm and ioreg parses never populate one, which is exactly
  // why the fingerprint degrades to name+total instead of being UUID-only.
  const amd: HardwareGPU = {
    index: 0,
    name: 'Radeon RX 7900 XTX',
    memory_total_bytes: 24576 * 1048576,
  };

  it('confirms the card when the recorded UUID is still the live one', () => {
    expect(
      vramCardCheck(gpu({ fingerprint: 'GPU-aaa', fingerprint_kind: 'uuid' }), nvidia),
    ).toEqual({ state: 'verified', kind: 'uuid' });
  });

  it('reports DRIFT when the card at this index is a different one', () => {
    // The failure this whole check exists for: a driver reset renumbers the
    // cards, and days later the operator is offered a number measured on
    // other hardware.
    expect(
      vramCardCheck(gpu({ fingerprint: 'GPU-abc', fingerprint_kind: 'uuid' }), nvidia),
    ).toEqual({ state: 'drifted', recorded: 'GPU-abc', live: 'GPU-aaa' });
  });

  it('compares name plus total size in the exact shape the run recorded', () => {
    // The recorded string is persisted verbatim inside vram_json, so its
    // format is a closed vocabulary: "<name> / <total> MB", the same strings
    // the backend's own vramFingerprintOf test pins.
    expect(
      vramCardCheck(
        gpu({ fingerprint: 'Radeon RX 7900 XTX / 24576 MB', fingerprint_kind: 'name_total' }),
        amd,
      ),
    ).toEqual({ state: 'verified', kind: 'name_total' });
    expect(
      vramCardCheck(
        gpu({ fingerprint: 'Radeon RX 7900 XTX / 16384 MB', fingerprint_kind: 'name_total' }),
        amd,
      ),
    ).toEqual({
      state: 'drifted',
      recorded: 'Radeon RX 7900 XTX / 16384 MB',
      live: 'Radeon RX 7900 XTX / 24576 MB',
    });
    // A card that reports a total but no name is still weakly identified, and
    // the two sides must agree on that shape too.
    expect(
      vramCardCheck(gpu({ fingerprint: '16384 MB', fingerprint_kind: 'name_total' }), {
        index: 0,
        name: '',
        memory_total_bytes: 16384 * 1048576,
      }),
    ).toEqual({ state: 'verified', kind: 'name_total' });
  });

  it('says CANNOT VERIFY rather than drift whenever there is nothing to compare', () => {
    // Every one of these is an absent identifier, on one side or the other,
    // and an absent identifier means "no drift detection available here" --
    // the same rule the per-GPU budget rows' expected_uuid detector applies.
    // Fails toward unverifiable, never toward verified.
    const cases: [string, VRAMGPUItemDTO | undefined, HardwareGPU | undefined][] = [
      ['no fingerprint recorded at all', gpu({ fingerprint_kind: '' }), nvidia],
      ['a kind with no value', gpu({ fingerprint: '', fingerprint_kind: 'uuid' }), nvidia],
      [
        'a kind this build does not know',
        gpu({ fingerprint: 'pci:65:00.0', fingerprint_kind: 'pci_bus_id' }),
        nvidia,
      ],
      [
        'no live card known at this index (the hardware report has not arrived)',
        gpu({ fingerprint: 'GPU-aaa', fingerprint_kind: 'uuid' }),
        undefined,
      ],
      [
        'a live card that cannot supply the compared field',
        gpu({ fingerprint: 'GPU-aaa', fingerprint_kind: 'uuid' }),
        amd,
      ],
      [
        'a live card with neither a name nor a total',
        gpu({ fingerprint: 'Card A / 24576 MB', fingerprint_kind: 'name_total' }),
        { index: 0, name: '', memory_total_bytes: 0 },
      ],
      ['no item at all', undefined, nvidia],
    ];
    for (const [name, item, live] of cases) {
      expect(vramCardCheck(item, live), name).toEqual({ state: 'unverifiable' });
    }
  });

  it('gives the three outcomes three DISTINCT sentences, and no bare "verified"', () => {
    const uuid = vramCardCheckLabelKey({ state: 'verified', kind: 'uuid' });
    const nameTotal = vramCardCheckLabelKey({ state: 'verified', kind: 'name_total' });
    const unverifiable = vramCardCheckLabelKey({ state: 'unverifiable' });
    const drifted = vramCardCheckLabelKey({ state: 'drifted', recorded: 'a', live: 'b' });
    expect(new Set([uuid, nameTotal, unverifiable, drifted]).size).toBe(4);
    for (const locale of ['de', 'en'] as const) {
      // "cannot verify" must READ differently from "verified": a check that
      // could not be made is not a check that passed.
      expect(messages[locale][unverifiable]).not.toBe(messages[locale][uuid]);
      expect(messages[locale][unverifiable].length).toBeGreaterThan(30);
      // name+total cannot tell two identical cards apart and has to say so.
      expect(messages[locale][nameTotal]).toMatch(/identi/i);
      // Drift names the renumbering AND that the number is withheld.
      expect(messages[locale][drifted].length).toBeGreaterThan(30);
    }
  });
});
