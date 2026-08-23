// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import type { MessageKey } from './shared/types';

export type ColumnId =
  | 'created_at'
  | 'owner'
  | 'requested_model'
  | 'model'
  | 'server_name'
  | 'token_name'
  | 'service_name'
  | 'http_status'
  | 'total_tokens'
  | 'latency_ms'
  | 'input_tokens'
  | 'output_tokens'
  | 'prompt_per_second'
  | 'tokens_per_second'
  | 'req_path'
  | 'content_type'
  | 'cached_tokens'
  | 'cache_write_tokens'
  | 'stream'
  | 'provider_path'
  | 'provider_model'
  | 'session'
  | 'agent_id'
  | 'energy_wh'
  | 'energy_marginal_wh'
  | 'energy_source'
  | 'cost_eur';

export type ColumnDef = {
  id: ColumnId;
  labelKey: MessageKey;
  defaultVisible: boolean;
  sortable: boolean;
  numeric: boolean;
};

// Declaration order = default column order. `owner` is a normal column here but
// is scope-gated in the view: rendered/toggleable only in the all-scope view
// (Activity forces it out and out of the column menu in own-scope), while its id
// stays in the persisted order so it returns to position when scope flips.
export const ACTIVITY_COLUMNS: ColumnDef[] = [
  {
    id: 'created_at',
    labelKey: 'activityColTime',
    defaultVisible: true,
    sortable: true,
    numeric: false,
  },
  {
    id: 'owner',
    labelKey: 'activityColOwner',
    defaultVisible: true,
    sortable: false,
    numeric: false,
  },
  {
    id: 'token_name',
    labelKey: 'tableToken',
    defaultVisible: true,
    sortable: true,
    numeric: false,
  },
  // Optional (hidden by default): the Service Account (Phase 1 service accounts)
  // that served the request, if any. Positioned right next to the token column
  // it complements (both attribute "who made this request").
  {
    id: 'service_name',
    labelKey: 'activityColService',
    defaultVisible: false,
    sortable: false,
    numeric: false,
  },
  {
    id: 'requested_model',
    labelKey: 'tableRequestedModel',
    defaultVisible: true,
    sortable: true,
    numeric: false,
  },
  { id: 'model', labelKey: 'tableModel', defaultVisible: true, sortable: true, numeric: false },
  {
    id: 'req_path',
    labelKey: 'activityColPath',
    defaultVisible: false,
    sortable: false,
    numeric: false,
  },
  {
    id: 'http_status',
    labelKey: 'tableStatus',
    defaultVisible: true,
    sortable: true,
    numeric: false,
  },
  {
    id: 'content_type',
    labelKey: 'activityColContentType',
    defaultVisible: false,
    sortable: false,
    numeric: false,
  },
  {
    id: 'server_name',
    labelKey: 'tableHost',
    defaultVisible: false,
    sortable: true,
    numeric: false,
  },
  {
    id: 'provider_path',
    labelKey: 'activityColProviderPath',
    defaultVisible: false,
    sortable: false,
    numeric: false,
  },
  {
    id: 'provider_model',
    labelKey: 'activityColProviderModel',
    defaultVisible: false,
    sortable: false,
    numeric: false,
  },
  {
    id: 'session',
    labelKey: 'activityColSession',
    defaultVisible: false,
    sortable: false,
    numeric: false,
  },
  {
    id: 'agent_id',
    labelKey: 'activityColAgent',
    defaultVisible: false,
    sortable: false,
    numeric: false,
  },
  {
    id: 'total_tokens',
    labelKey: 'tableTokens',
    defaultVisible: true,
    sortable: true,
    numeric: true,
  },
  {
    id: 'cached_tokens',
    labelKey: 'activityColCached',
    defaultVisible: false,
    sortable: false,
    numeric: true,
  },
  {
    id: 'cache_write_tokens',
    labelKey: 'activityColCacheWrite',
    defaultVisible: false,
    sortable: false,
    numeric: true,
  },
  {
    id: 'input_tokens',
    labelKey: 'activityColInput',
    defaultVisible: false,
    sortable: false,
    numeric: true,
  },
  {
    id: 'output_tokens',
    labelKey: 'activityColOutput',
    defaultVisible: false,
    sortable: false,
    numeric: true,
  },
  {
    id: 'prompt_per_second',
    labelKey: 'activityColPromptSpeed',
    defaultVisible: false,
    sortable: true,
    numeric: true,
  },
  {
    id: 'tokens_per_second',
    labelKey: 'activityColTokenSpeed',
    defaultVisible: false,
    sortable: true,
    numeric: true,
  },
  {
    id: 'stream',
    labelKey: 'activityColStream',
    defaultVisible: false,
    sortable: false,
    numeric: false,
  },
  // Additive P1 energy-attribution columns: no computation engine exists yet, so
  // every row shows 0/0/"—" today; hidden by default until a later phase
  // populates them. energy_wh/energy_marginal_wh mirror the sibling hidden
  // numeric columns (cached_tokens et al. — filterable server-side, but no
  // header-click sort until a future phase whitelists them); energy_source is a
  // free-text provenance field rendered like an enum chip.
  {
    id: 'energy_wh',
    labelKey: 'activityColEnergyWh',
    defaultVisible: false,
    sortable: false,
    numeric: true,
  },
  {
    id: 'energy_marginal_wh',
    labelKey: 'activityColEnergyMarginalWh',
    defaultVisible: false,
    sortable: false,
    numeric: true,
  },
  {
    id: 'energy_source',
    labelKey: 'activityColEnergySource',
    defaultVisible: false,
    sortable: false,
    numeric: false,
  },
  // Additive P3 T1 column: the portal-computed per-row EUR cost derived from
  // energy_wh, mirroring the energy columns above (hidden by default, numeric
  // filterable, "—" when 0/unset).
  {
    id: 'cost_eur',
    labelKey: 'activityColCostEur',
    defaultVisible: false,
    sortable: false,
    numeric: true,
  },
  {
    id: 'latency_ms',
    labelKey: 'activityColDuration',
    defaultVisible: true,
    sortable: true,
    numeric: true,
  },
];

export const DEFAULT_HIDDEN_COLUMNS: ColumnId[] = ACTIVITY_COLUMNS.filter(
  (c) => !c.defaultVisible,
).map((c) => c.id);

// Default column order = declaration order in ACTIVITY_COLUMNS.
export const DEFAULT_COLUMN_ORDER: ColumnId[] = ACTIVITY_COLUMNS.map((c) => c.id);

const SCOPE_KEY = 'op.activity.scope';
const FILTER_USER_KEY = 'op.activity.filterUser';
const FILTER_TOKEN_KEY = 'op.activity.filterToken';
const TS_WINDOW_KEY = 'op.activity.tsWindow';
const TS_BUCKET_KEY = 'op.activity.tsBucket';

// Time-series window (total span) and bucket (resolution in seconds). Both the
// wire tokens and the numeric bucket set are kept in lockstep with the backend
// whitelists in internal/gateway/usage_activity.go.
export type TsWindow =
  '5m' | '15m' | '30m' | '1h' | '6h' | '12h' | '1d' | '1w' | '2w' | '1mo' | '3mo' | '6mo' | '1y';
export type TsBucket =
  1 | 5 | 10 | 30 | 60 | 180 | 900 | 3600 | 21600 | 43200 | 86400 | 604800 | 1209600 | 2592000;

export const TS_WINDOWS: readonly TsWindow[] = [
  '5m',
  '15m',
  '30m',
  '1h',
  '6h',
  '12h',
  '1d',
  '1w',
  '2w',
  '1mo',
  '3mo',
  '6mo',
  '1y',
];
export const TS_BUCKETS: readonly TsBucket[] = [
  1, 5, 10, 30, 60, 180, 900, 3600, 21600, 43200, 86400, 604800, 1209600, 2592000,
];

// Window token -> total span in seconds (drives the window dropdown labels).
export const TS_WINDOW_SECONDS: Record<TsWindow, number> = {
  '5m': 300,
  '15m': 900,
  '30m': 1800,
  '1h': 3600,
  '6h': 21600,
  '12h': 43200,
  '1d': 86400,
  '1w': 604800,
  '2w': 1209600,
  '1mo': 2592000,
  '3mo': 7776000,
  '6mo': 15552000,
  '1y': 31536000,
};

// Unit words used to label a duration; supplied by i18n so window/resolution
// dropdown labels are localized without a key per option.
export type TsUnitLabels = {
  tsUnitMin: string;
  tsUnitHour: string;
  tsUnitDay: string;
  tsUnitDays: string;
  tsUnitWeek: string;
  tsUnitWeeks: string;
  tsUnitMonth: string;
  tsUnitMonths: string;
  tsUnitYear: string;
  tsUnitYears: string;
};

/**
 * Format a duration in seconds as a compact dropdown label: seconds up to a
 * minute ("30s"), then minutes/hours, then days/weeks/months/years with
 * singular/plural unit words. Pure (locale words come from `t`), so the exact
 * bucket/window boundaries are unit-testable. Months use a 30-day approximation
 * and years 365 days, matching the backend window durations.
 */
export function formatTsSeconds(seconds: number, t: TsUnitLabels): string {
  if (seconds <= 60) return `${seconds}s`;
  if (seconds < 3600) return `${seconds / 60} ${t.tsUnitMin}`;
  if (seconds < 86400) return `${seconds / 3600} ${t.tsUnitHour}`;
  if (seconds < 604800) {
    const n = seconds / 86400;
    return `${n} ${n === 1 ? t.tsUnitDay : t.tsUnitDays}`;
  }
  if (seconds < 2592000) {
    const n = seconds / 604800;
    return `${n} ${n === 1 ? t.tsUnitWeek : t.tsUnitWeeks}`;
  }
  if (seconds < 31536000) {
    const n = Math.round(seconds / 2592000);
    return `${n} ${n === 1 ? t.tsUnitMonth : t.tsUnitMonths}`;
  }
  const n = Math.round(seconds / 31536000);
  return `${n} ${n === 1 ? t.tsUnitYear : t.tsUnitYears}`;
}

// The reconcileHiddenColumns unknown-id sanitizer that used to live here is now
// the generic reconcileHiddenIds in shared/useColumnSettings.ts.

// Scope + time-series window/bucket remain browser-local (view state, not table
// column settings). All localStorage access is guarded (jsdom has no
// window.localStorage; private mode can throw).

// Persist the Activity scope so it survives navigation away and back. "user" is
// the admin "specific user" scope (see the user/token filter helpers below).
export function readScope(): 'own' | 'all' | 'user' {
  try {
    const raw = window.localStorage?.getItem(SCOPE_KEY);
    return raw === 'all' || raw === 'user' ? raw : 'own';
  } catch {
    return 'own';
  }
}

export function writeScope(value: 'own' | 'all' | 'user'): void {
  try {
    window.localStorage?.setItem(SCOPE_KEY, value);
  } catch {
    /* persistence is best-effort */
  }
}

// Persist the selected specific-user id ("" = none/all). Best-effort, guarded.
export function readFilterUser(): string {
  try {
    return window.localStorage?.getItem(FILTER_USER_KEY) ?? '';
  } catch {
    return '';
  }
}

export function writeFilterUser(value: string): void {
  try {
    window.localStorage?.setItem(FILTER_USER_KEY, value);
  } catch {
    /* persistence is best-effort */
  }
}

// Persist the selected token id ("" = none/all; "chat-session" = the no-token
// chat pseudo). Best-effort, guarded.
export function readFilterToken(): string {
  try {
    return window.localStorage?.getItem(FILTER_TOKEN_KEY) ?? '';
  } catch {
    return '';
  }
}

export function writeFilterToken(value: string): void {
  try {
    window.localStorage?.setItem(FILTER_TOKEN_KEY, value);
  } catch {
    /* persistence is best-effort */
  }
}

// Persist the shared time-series window (default "5m"). Any value outside the
// whitelist (or a missing/corrupt entry) falls back to the default.
export function readTsWindow(): TsWindow {
  try {
    const raw = window.localStorage?.getItem(TS_WINDOW_KEY);
    return TS_WINDOWS.includes(raw as TsWindow) ? (raw as TsWindow) : '5m';
  } catch {
    return '5m';
  }
}

export function writeTsWindow(value: TsWindow): void {
  try {
    window.localStorage?.setItem(TS_WINDOW_KEY, value);
  } catch {
    /* persistence is best-effort */
  }
}

// Persist the shared time-series bucket resolution in seconds (default 5). Stored
// as a string and parsed back; anything outside the whitelist falls back.
export function readTsBucket(): TsBucket {
  try {
    const raw = window.localStorage?.getItem(TS_BUCKET_KEY);
    const parsed = Number(raw);
    return TS_BUCKETS.includes(parsed as TsBucket) ? (parsed as TsBucket) : 5;
  } catch {
    return 5;
  }
}

export function writeTsBucket(value: TsBucket): void {
  try {
    window.localStorage?.setItem(TS_BUCKET_KEY, String(value));
  } catch {
    /* persistence is best-effort */
  }
}
