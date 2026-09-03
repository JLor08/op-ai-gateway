// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { describe, expect, it } from 'vitest';
import { errorLabelByCode, formatDate, formatMetric, formatPortalError } from './format';
import { PortalApiError } from '../../api';
import { messages } from '../../i18n';
describe('formatDate', () => {
  it('returns the fallback for null/empty/invalid input', () => {
    expect(formatDate(null, 'never')).toBe('never');
    expect(formatDate(undefined, 'never')).toBe('never');
    expect(formatDate('', 'never')).toBe('never');
    expect(formatDate('not-a-date', 'never')).toBe('never');
  });
  it('formats a valid ISO timestamp to a non-raw, non-fallback string', () => {
    const out = formatDate('2026-07-12T07:22:01Z', 'never');
    expect(out).not.toBe('never');
    expect(out).not.toBe('2026-07-12T07:22:01Z');
    expect(out).toContain('2026');
  });
});

describe('formatMetric', () => {
  it('renders a value with the requested number of decimals', () => {
    expect(formatMetric(12.3456, 2)).toBe('12.35');
    expect(formatMetric(8, 2)).toBe('8.00');
    // Energy per token is a very small number and needs the long tail.
    expect(formatMetric(0.0000001234, 10)).toBe('0.0000001234');
  });

  it('renders whole numbers without a decimal point at zero decimals', () => {
    expect(formatMetric(1500, 0)).toBe('1500');
  });

  it('renders the em-dash placeholder for missing or zero values', () => {
    // The backend reports "not measured" as 0, which this list has always shown
    // as the placeholder rather than a misleading 0.00.
    expect(formatMetric(0, 2)).toBe('—');
    expect(formatMetric(null, 2)).toBe('—');
    expect(formatMetric(undefined, 2)).toBe('—');
  });
});

/**
 * `errorLabelByCode` had no direct test at all — 96 hand-maintained entries
 * mapping a backend error sentinel to a portal label, grown by every task that
 * touched a new endpoint. Task 19's twelve additions were deferred for exactly
 * that reason: covering only the new ones would have made the convention
 * inconsistent in the other direction.
 *
 * What is covered here is the WHOLE map, and deliberately NOT as a 96-row
 * table of expected labels. Such a table is a second copy of the same data:
 * whoever edits the map edits the table the same way, in the same commit, so it
 * catches nothing while making every future entry cost two edits. What it
 * cannot catch is precisely the realistic defect — a new entry pointed at the
 * neighbouring label by copy-paste.
 *
 * The invariants below do catch that (a reused label is a duplicate, and there
 * is exactly one documented duplicate pair), they cover entries that do not
 * exist yet, and each of them can genuinely fail. `formatPortalError`'s own
 * branches — mapped code, unmapped code, plain Error, non-Error — are pinned
 * separately, since they are what every call site actually reaches.
 */
describe('formatPortalError', () => {
  it('renders a mapped code as "code: localized label", in both locales', () => {
    const err = new PortalApiError(409, 'mapping.gateway_name_conflict', 'raw server text');
    expect(formatPortalError(err, messages.de)).toBe(
      `mapping.gateway_name_conflict: ${messages.de.errorMappingGatewayNameConflict}`,
    );
    expect(formatPortalError(err, messages.en)).toBe(
      `mapping.gateway_name_conflict: ${messages.en.errorMappingGatewayNameConflict}`,
    );
    // The raw server message is deliberately NOT shown when a label exists.
    expect(formatPortalError(err, messages.de)).not.toContain('raw server text');
  });

  it("falls back to the server's own message for an unmapped code, still naming the code", () => {
    // Forward compatibility: a newer backend sentinel must degrade to whatever
    // the server said, never to an empty string or a misleading label.
    const err = new PortalApiError(500, 'some.future_sentinel', 'something specific broke');
    expect(formatPortalError(err, messages.de)).toBe(
      'some.future_sentinel: something specific broke',
    );
  });

  it('passes a plain Error through by message and stringifies anything else', () => {
    expect(formatPortalError(new Error('network down'), messages.de)).toBe('network down');
    expect(formatPortalError('just a string', messages.de)).toBe('just a string');
    expect(formatPortalError(undefined, messages.de)).toBe('undefined');
  });
});

describe('errorLabelByCode (whole-map invariants)', () => {
  const entries = Object.entries(errorLabelByCode) as [string, keyof typeof messages.de][];

  it('is not empty (guards against the map itself being emptied or renamed away)', () => {
    expect(entries.length).toBeGreaterThan(50);
  });

  it('resolves every entry to a non-empty label in de AND en', () => {
    for (const [code, key] of entries) {
      const de = messages.de[key];
      const en = messages.en[key];
      expect(typeof de, `${code} -> ${String(key)} (de)`).toBe('string');
      expect(typeof en, `${code} -> ${String(key)} (en)`).toBe('string');
      expect(String(de).length, `${code} -> ${String(key)} (de) is empty`).toBeGreaterThan(0);
      expect(String(en).length, `${code} -> ${String(key)} (en) is empty`).toBeGreaterThan(0);
    }
  });

  it('keys every entry by the backend sentinel convention, dotted snake_case', () => {
    // The codes are read verbatim from the Go sentinels (e.g.
    // portal/service_runtime.go) and from the errRow tables in the gateway
    // endpoint files. A camelCased or spaced key here silently never matches.
    for (const [code] of entries) {
      expect(code, `${code} is not a dotted snake_case sentinel`).toMatch(
        /^[a-z][a-z0-9_]*\.[a-z][a-z0-9_]*$/,
      );
    }
  });

  it('points every entry at an error* label, never at some other message', () => {
    // A code pointed at a non-error key (a warning, a field name) renders as a
    // plausible sentence in the wrong register and reads as a portal bug.
    for (const [code, key] of entries) {
      expect(String(key), `${code} -> ${String(key)} is not an error* key`).toMatch(/^error[A-Z]/);
    }
  });

  /**
   * The VRAM benchmark's own wire codes, pinned as LITERALS on purpose.
   *
   * The whole-map invariants above cover entries that do not exist yet, but
   * none of them can catch the failure that matters for these six: the code
   * STRING drifting apart from the backend's. A code the map does not carry is
   * not an error the portal reports badly -- `formatPortalError` falls back to
   * the raw English the server sent, so a 409 an operator is meant to act on
   * ("this server is in file mode", "the spec declares GPU 3 and the host has
   * two") arrives untranslated and unexplained, in a portal that is otherwise
   * fully localized. Nothing else in this package names these strings.
   *
   * They are declared in Go as `codeBenchmarkVRAM*`
   * (`internal/gateway/benchmark_vram_isolation.go`,
   * `benchmark_vram_confidence.go`) and as the `ErrRuntimeSpecServerBenchmarking`
   * sentinel's own message, wired to a 409 in the `errRow` table in
   * `portal_runtime_endpoints.go`. Renaming one there means editing this list
   * and the map together; the Go side pins its own literals for the four
   * isolation refusals and the declared-GPU one, so only
   * `runtime_spec.server_benchmarking` has no guard on its half.
   */
  const vramWireCodes = [
    'benchmark.vram_not_agent_managed',
    'benchmark.vram_isolation_unavailable',
    'benchmark.vram_no_gpu_samples',
    'benchmark.vram_isolation_blocked',
    'benchmark.vram_declared_gpu_missing',
    'runtime_spec.server_benchmarking',
  ] as const;

  it('carries every VRAM-benchmark refusal code, by its exact wire string', () => {
    for (const code of vramWireCodes) {
      expect(
        errorLabelByCode[code],
        `${code} is not mapped: the operator sees raw English`,
      ).toBeDefined();
    }
    // Both directions, so the list cannot go stale the way the reason
    // vocabularies did: a sixth `benchmark.vram_*` refusal added to the map
    // without being named here fails too.
    expect(
      entries
        .filter(([code]) => code.startsWith('benchmark.vram_'))
        .map(([code]) => code)
        .sort(),
    ).toEqual(
      vramWireCodes
        .filter((code) => code.startsWith('benchmark.vram_'))
        .slice()
        .sort(),
    );
  });

  it('reuses a label for two codes only where that is deliberate', () => {
    // The realistic defect in a hand-maintained map this size is a new entry
    // pointed at its neighbour's label by copy-paste. Every shared label is
    // therefore listed here explicitly; `request.failed` /
    // `request.invalid_response` are one event to the operator ("the request
    // did not work"), which is why they share one.
    const documentedSharedLabels: Record<string, string[]> = {
      errorRequestFailed: ['request.failed', 'request.invalid_response'],
    };
    const codesByLabel = new Map<string, string[]>();
    for (const [code, key] of entries) {
      const list = codesByLabel.get(String(key)) ?? [];
      list.push(code);
      codesByLabel.set(String(key), list);
    }
    for (const [label, codes] of codesByLabel) {
      if (codes.length === 1) continue;
      expect(
        codes.slice().sort(),
        `${codes.join(' + ')} share the label ${label}; add them to documentedSharedLabels if that is intended`,
      ).toEqual((documentedSharedLabels[label] ?? []).slice().sort());
    }
  });
});
