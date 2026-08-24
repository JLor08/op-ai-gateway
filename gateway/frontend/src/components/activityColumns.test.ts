// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { afterEach, describe, expect, it, vi } from 'vitest';
import {
  ACTIVITY_COLUMNS,
  DEFAULT_COLUMN_ORDER,
  DEFAULT_HIDDEN_COLUMNS,
  readScope,
  writeScope,
  readFilterUser,
  writeFilterUser,
  readFilterToken,
  writeFilterToken,
  readTsWindow,
  writeTsWindow,
  readTsBucket,
  writeTsBucket,
  TS_WINDOWS,
  TS_BUCKETS,
  TS_WINDOW_SECONDS,
  formatTsSeconds,
  type TsUnitLabels,
} from './activityColumns';

const UNITS: TsUnitLabels = {
  tsUnitMin: 'Min',
  tsUnitHour: 'Std',
  tsUnitDay: 'Tag',
  tsUnitDays: 'Tage',
  tsUnitWeek: 'Woche',
  tsUnitWeeks: 'Wochen',
  tsUnitMonth: 'Monat',
  tsUnitMonths: 'Monate',
  tsUnitYear: 'Jahr',
  tsUnitYears: 'Jahre',
};

function installStorage() {
  const store = new Map<string, string>();
  const storage = {
    getItem: (k: string) => (store.has(k) ? store.get(k)! : null),
    setItem: (k: string, v: string) => void store.set(k, String(v)),
    removeItem: (k: string) => void store.delete(k),
    clear: () => store.clear(),
    key: (i: number) => Array.from(store.keys())[i] ?? null,
    get length() {
      return store.size;
    },
  } satisfies Storage;
  vi.stubGlobal('localStorage', storage);
  return store;
}

afterEach(() => vi.unstubAllGlobals());

describe('activityColumns catalogue', () => {
  it('has the eight default-visible columns and nineteen optional columns', () => {
    const visible = ACTIVITY_COLUMNS.filter((c) => c.defaultVisible).map((c) => c.id);
    expect(visible).toEqual([
      'created_at',
      'owner',
      'token_name',
      'requested_model',
      'model',
      'http_status',
      'total_tokens',
      'latency_ms',
    ]);
    expect(DEFAULT_HIDDEN_COLUMNS).toEqual(
      ACTIVITY_COLUMNS.filter((c) => !c.defaultVisible).map((c) => c.id),
    );
    expect(DEFAULT_HIDDEN_COLUMNS).toContain('input_tokens');
    // cache_write_tokens (cache creation) is optional (hidden by default) and sits
    // right after cached_tokens (the read/write pair).
    expect(DEFAULT_HIDDEN_COLUMNS).toContain('cache_write_tokens');
    // provider_path is optional (hidden by default) and sits right before provider_model.
    expect(DEFAULT_HIDDEN_COLUMNS).toContain('provider_path');
    const ids = ACTIVITY_COLUMNS.map((c) => c.id);
    expect(ids.indexOf('provider_path')).toBe(ids.indexOf('provider_model') - 1);
    // session + agent_id are optional (hidden by default) and sit right after
    // provider_model (session, then agent_id).
    expect(DEFAULT_HIDDEN_COLUMNS).toContain('session');
    expect(DEFAULT_HIDDEN_COLUMNS).toContain('agent_id');
    expect(ids.indexOf('session')).toBe(ids.indexOf('provider_model') + 1);
    expect(ids.indexOf('agent_id')).toBe(ids.indexOf('session') + 1);
    // service_name (Phase 1 service accounts) is optional (hidden by default) and
    // sits right after token_name (the two attribution columns read together).
    expect(DEFAULT_HIDDEN_COLUMNS).toContain('service_name');
    expect(ids.indexOf('service_name')).toBe(ids.indexOf('token_name') + 1);
    // cache_write_tokens sits right after cached_tokens (the read/write pair).
    expect(ids.indexOf('cache_write_tokens')).toBe(ids.indexOf('cached_tokens') + 1);
    // The three additive P1 energy-attribution columns are hidden by default too.
    expect(DEFAULT_HIDDEN_COLUMNS).toContain('energy_wh');
    expect(DEFAULT_HIDDEN_COLUMNS).toContain('energy_marginal_wh');
    expect(DEFAULT_HIDDEN_COLUMNS).toContain('energy_source');
    // The additive P3 T1 per-row cost column mirrors the energy columns: hidden by
    // default, numeric, and grouped right after energy_source.
    expect(DEFAULT_HIDDEN_COLUMNS).toContain('cost_eur');
    expect(ids.indexOf('cost_eur')).toBe(ids.indexOf('energy_source') + 1);
    const costEur = ACTIVITY_COLUMNS.find((c) => c.id === 'cost_eur');
    expect(costEur).toBeDefined();
    expect(costEur!.numeric).toBe(true);
    expect(costEur!.labelKey).toBe('activityColCostEur');
    expect(DEFAULT_HIDDEN_COLUMNS).toHaveLength(19);
  });

  it('includes the scope-gated owner column as a normal catalogue entry', () => {
    const owner = ACTIVITY_COLUMNS.find((c) => c.id === 'owner');
    expect(owner).toBeDefined();
    expect(owner!.defaultVisible).toBe(true);
    expect(owner!.labelKey).toBe('activityColOwner');
  });

  it('labels the latency_ms column as a duration column', () => {
    const latency = ACTIVITY_COLUMNS.find((c) => c.id === 'latency_ms');
    expect(latency).toBeDefined();
    expect(latency!.labelKey).toBe('activityColDuration');
  });

  it('uses the declaration order as the default column order', () => {
    expect(DEFAULT_COLUMN_ORDER).toEqual(ACTIVITY_COLUMNS.map((c) => c.id));
    expect(DEFAULT_COLUMN_ORDER[0]).toBe('created_at');
  });

  it('has requested_model as a default-visible, sortable column directly before model', () => {
    const ids = ACTIVITY_COLUMNS.map((c) => c.id);
    const reqIdx = ids.indexOf('requested_model');
    const modelIdx = ids.indexOf('model');
    expect(reqIdx).toBeGreaterThan(-1);
    expect(reqIdx).toBe(modelIdx - 1);
    const col = ACTIVITY_COLUMNS[reqIdx];
    expect(col.defaultVisible).toBe(true);
    expect(col.sortable).toBe(true);
    expect(col.labelKey).toBe('tableRequestedModel');
  });
});

// The reconcileHiddenColumns crash-safety guard that used to live in
// activityColumns.ts is now the generic reconcileHiddenIds in
// shared/useColumnSettings.ts (see useColumnSettings.test.tsx for its coverage).

describe('localStorage helpers', () => {
  it('defaults scope to own and round-trips all', () => {
    installStorage();
    expect(readScope()).toBe('own');
    writeScope('all');
    expect(readScope()).toBe('all');
    writeScope('own');
    expect(readScope()).toBe('own');
  });

  it('coerces an invalid scope value to own', () => {
    const store = installStorage();
    store.set('op.activity.scope', 'sideways');
    expect(readScope()).toBe('own');
  });

  it("round-trips the new 'user' scope", () => {
    installStorage();
    writeScope('user');
    expect(readScope()).toBe('user');
  });

  it('defaults the user/token filters to empty and round-trips them', () => {
    const store = installStorage();
    expect(readFilterUser()).toBe('');
    expect(readFilterToken()).toBe('');
    writeFilterUser('usr_42');
    writeFilterToken('chat-session');
    expect(readFilterUser()).toBe('usr_42');
    expect(readFilterToken()).toBe('chat-session');
    expect(store.get('op.activity.filterUser')).toBe('usr_42');
    expect(store.get('op.activity.filterToken')).toBe('chat-session');
  });

  it('defaults tsWindow to 5m and round-trips the whitelist', () => {
    installStorage();
    expect(readTsWindow()).toBe('5m');
    writeTsWindow('15m');
    expect(readTsWindow()).toBe('15m');
    writeTsWindow('1h');
    expect(readTsWindow()).toBe('1h');
  });

  it('coerces an invalid tsWindow value to 5m', () => {
    const store = installStorage();
    store.set('op.activity.tsWindow', '42m');
    expect(readTsWindow()).toBe('5m');
  });

  it('defaults tsBucket to 5 and round-trips the numeric whitelist', () => {
    installStorage();
    expect(readTsBucket()).toBe(5);
    writeTsBucket(1);
    expect(readTsBucket()).toBe(1);
    writeTsBucket(30);
    expect(readTsBucket()).toBe(30);
    writeTsBucket(60);
    expect(readTsBucket()).toBe(60);
  });

  it('coerces an out-of-whitelist or non-numeric tsBucket to 5', () => {
    const store = installStorage();
    store.set('op.activity.tsBucket', '7');
    expect(readTsBucket()).toBe(5);
    store.set('op.activity.tsBucket', 'abc');
    expect(readTsBucket()).toBe(5);
  });
});

describe('formatTsSeconds', () => {
  it('renders sub-minute durations as bare seconds (incl. the 60s boundary)', () => {
    expect(formatTsSeconds(1, UNITS)).toBe('1s');
    expect(formatTsSeconds(30, UNITS)).toBe('30s');
    expect(formatTsSeconds(60, UNITS)).toBe('60s');
  });

  it('renders minutes, hours, days, weeks, months and years with unit words', () => {
    expect(formatTsSeconds(180, UNITS)).toBe('3 Min');
    expect(formatTsSeconds(300, UNITS)).toBe('5 Min');
    expect(formatTsSeconds(1800, UNITS)).toBe('30 Min');
    expect(formatTsSeconds(3600, UNITS)).toBe('1 Std');
    expect(formatTsSeconds(43200, UNITS)).toBe('12 Std');
    expect(formatTsSeconds(86400, UNITS)).toBe('1 Tag');
    expect(formatTsSeconds(604800, UNITS)).toBe('1 Woche');
    expect(formatTsSeconds(1209600, UNITS)).toBe('2 Wochen');
    expect(formatTsSeconds(2592000, UNITS)).toBe('1 Monat');
    expect(formatTsSeconds(7776000, UNITS)).toBe('3 Monate');
    expect(formatTsSeconds(15552000, UNITS)).toBe('6 Monate');
    expect(formatTsSeconds(31536000, UNITS)).toBe('1 Jahr');
  });

  it('gives every whitelisted window and bucket a non-empty label', () => {
    for (const w of TS_WINDOWS) {
      expect(formatTsSeconds(TS_WINDOW_SECONDS[w], UNITS).length).toBeGreaterThan(0);
    }
    for (const b of TS_BUCKETS) {
      expect(formatTsSeconds(b, UNITS).length).toBeGreaterThan(0);
    }
  });
});
