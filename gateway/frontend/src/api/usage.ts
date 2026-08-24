// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { type Fetcher, buildQueryString, request, subscribeSSE } from './transport';
import type { PortalRoute } from './models';

export type UsageEvent = {
  id: string;
  user_id: string;
  token_id: string;
  session_id?: string;
  session_source?: string;
  agent_id?: string;
  api_flavor: string;
  model: string;
  // The model the client originally requested, before a token model override (if
  // any) rewrote it to `model`. Equal to `model` when no override fired; "" for
  // rows recorded before this field shipped (unknown — must NOT fall back to
  // displaying `model`).
  requested_model: string;
  provider: string;
  host: string;
  input_tokens: number;
  output_tokens: number;
  total_tokens: number;
  cached_tokens: number;
  cache_write_tokens: number;
  latency_ms: number;
  prompt_per_second: number;
  tokens_per_second: number;
  status: 'success' | 'error';
  http_status: number;
  error_code?: string;
  content_type: string;
  req_path: string;
  provider_path: string;
  provider_model: string;
  stream: boolean;
  token_name: string;
  server_name: string;
  // ServiceID / ServiceName attribute the request to a Service Account (Phase 1
  // service accounts) when it was served by a service token; empty/absent for
  // ordinary user-token/session usage. Mirrors token_id/token_name.
  service_id?: string;
  service_name?: string;
  route_id?: string;
  user_name?: string;
  has_capture?: boolean;
  capture_locked?: boolean;
  // Additive P1 energy-attribution fields: no computation engine exists yet, so
  // every event today carries 0/0/"" (no-op invariant) — a later phase populates
  // them. energy_wh = attributed energy for the request (Wh), energy_marginal_wh
  // = marginal energy vs. an idle baseline (Wh), energy_source = how it was
  // derived (e.g. "measured"/"estimated"; "" = unknown).
  energy_wh?: number;
  energy_marginal_wh?: number;
  energy_source?: string;
  // Additive P3 T1 field: a TRANSIENT, portal-computed display value — (energy_wh
  // / 1000) * the serving server's price_per_kwh (falling back to the system
  // default). Never a DB column; a store-returned event never carries it until the
  // portal layer sets it, so it is optional here too (0/undefined = unknown/unpriced).
  cost_eur?: number;
  created_at: string;
};

export type ActivityQuery = {
  page?: number;
  limit?: number;
  sort?: string;
  order?: 'asc' | 'desc';
  model?: string;
  server?: string;
  status?: 'success' | 'error' | '';
  q?: string;
  range?: '24h' | '7d' | '30d' | 'all';
  scope?: 'own' | 'all' | 'user';
  owner?: string;
  // Filter by a specific user (admin-only; honored server-side) and/or a specific
  // API token. token_id carries the "__none__" sentinel for the chat/no-token pseudo.
  user_id?: string;
  token_id?: string;
  time_from?: string;
  time_to?: string;
  // Substring text filters (server-side LIKE).
  req_path?: string;
  content_type?: string;
  provider_model?: string;
  provider_path?: string;
  // Tri-state stream filter: "true" | "false" | "" (unset).
  stream?: string;
  // Per-column numeric range params, keyed `<column>_min` / `<column>_max`
  // (e.g. total_tokens_min, latency_ms_max). Filtered server-side.
  [key: string]: string | number | undefined;
};

// UsagePage.data mirrors Go's UsageRow (usage.Event + user_name). Since user_name is
// already optional on UsageEvent above, the frontend reuses UsageEvent for rows.
export type UsagePage = {
  data: UsageEvent[];
  page: number;
  limit: number;
  total: number;
  total_pages: number;
};

// One aggregate row of the grouped-activity view. Mirrors the Go UsageGroupRow
// JSON (snake_case) returned by GET /api/portal/usage/groups. `key` is the raw
// group value (server name / session id / user id / token id / model); `key_label`
// is a display label the backend resolves (e.g. a user's name), falling back to
// the raw key when unresolved. All the numeric fields are aggregate sums over the
// group's member requests within the same filtered range.
export type UsageGroupRow = {
  key: string;
  key_label: string;
  count: number;
  error_count: number;
  input_tokens: number;
  output_tokens: number;
  total_tokens: number;
  cached_tokens: number;
  cache_write_tokens: number;
  energy_wh: number;
  cost_eur: number;
  first_at: string;
  last_at: string;
};

export type ActiveRequest = {
  id: string;
  user_id: string;
  user_name: string;
  token_id: string;
  token_name: string;
  // ServiceID / ServiceName attribute the in-flight request to a Service
  // Account (Phase 1 service accounts) when it was served by a service token;
  // empty for ordinary user-token/session usage. Mirrors token_id/token_name.
  service_id?: string;
  service_name?: string;
  model: string;
  server_name: string;
  api_flavor: string;
  req_path: string;
  provider_path: string;
  provider_model: string;
  session_id?: string;
  session_source?: string;
  agent_id?: string;
  stream: boolean;
  started_at: string;
};

export type CaptureDetail = {
  id: string;
  api_flavor: string;
  http_status: number;
  created_at: string;
  req_headers: Record<string, string[]>;
  req_body: string;
  resp_headers: Record<string, string[]>;
  resp_body: string;
  truncated: boolean;
  // Present only when the built-in translation ran: the request the gateway sent to
  // the Chat-Completions upstream + the raw upstream response (bodies + headers).
  translated_req_headers?: Record<string, string[]>;
  translated_req_body?: string;
  translated_resp_headers?: Record<string, string[]>;
  translated_resp_body?: string;
  secret: boolean;
  can_toggle_secret: boolean;
};

// A single time-bucket of the activity time-series. Field names mirror the Go
// usage.TimeSeriesPoint JSON exactly (snake_case). `t` is the RFC3339 bucket
// start; the throughput fields are already divided by the bucket width.
export type TimeSeriesPoint = {
  t: string;
  connections: number;
  concurrency: number;
  prompt_tokens_per_second: number;
  completion_tokens_per_second: number;
  // Additive P3 T1 field: sum(energy_wh) of the bucket's attributed events, in
  // watt-hours (a total per bucket, NOT divided by bucket width — mirrors
  // `connections` rather than the per-second throughput fields above).
  energy_wh?: number;
};

// Bucketed activity time-series (usage.TimeSeries). `points` is never null.
export type TimeSeries = {
  points: TimeSeriesPoint[];
  bucket_seconds: number;
  from: string;
  to: string;
};

export type HistogramBin = { x0: number; x1: number; count: number };

export type Histogram = {
  bins: HistogramBin[];
  min: number;
  max: number;
  bin_size: number;
  p50: number;
  p95: number;
  p99: number;
};

export type StatTotals = {
  total_requests: number;
  error_count: number;
  cached_tokens: number;
  cache_write_tokens: number;
  input_tokens: number;
  output_tokens: number;
  // Additive P3 T1 aggregates: a plain SUM(energy_wh) over the filtered set, and
  // the per-server-price-weighted EUR cost derived from it (portal-computed;
  // never a DB column). Optional/undefined mirrors the sibling energy fields on
  // UsageEvent above; treat as 0 when absent.
  total_energy_wh?: number;
  total_cost_eur?: number;
};

export type UsageStats = {
  totals: StatTotals;
  prompt_per_second: Histogram;
  tokens_per_second: Histogram;
};

export type DashboardResponse = {
  metrics: {
    requests_24h: number;
    tokens_24h: number;
    healthy_hosts: string;
    latency_p95_ms: number;
  };
  routes: PortalRoute[];
};

export function usageApi(fetcher: Fetcher) {
  return {
    dashboard: () => request<DashboardResponse>(fetcher, '/api/portal/dashboard'),
    activity: (query: ActivityQuery = {}) =>
      request<UsagePage>(fetcher, `/api/portal/usage${buildQueryString(query)}`),
    activityStats: (query: ActivityQuery = {}) =>
      request<UsageStats>(fetcher, `/api/portal/usage/stats${buildQueryString(query)}`),
    usageGroups: (query: ActivityQuery = {}) =>
      request<{ data: UsageGroupRow[]; group_by: string }>(
        fetcher,
        `/api/portal/usage/groups${buildQueryString(query)}`,
      ),
    activeRequests: (
      scope: 'own' | 'all' | 'user',
      params?: { user_id?: string; token_id?: string },
    ) =>
      request<{ data: ActiveRequest[] }>(
        fetcher,
        `/api/portal/usage/active${buildQueryString({ scope, user_id: params?.user_id, token_id: params?.token_id })}`,
      ),
    // window/bucket are validated against the shared whitelists at the call site
    // (TsWindow/TsBucket in activityColumns.ts) and again on the backend; the API
    // layer stays string/number so it need not import the component-level unions.
    usageTimeSeries: (p: {
      window: string;
      bucket: number;
      scope: 'own' | 'all' | 'user';
      user_id?: string;
      token_id?: string;
      server?: string;
      server_exact?: string;
    }) =>
      request<TimeSeries>(
        fetcher,
        `/api/portal/usage/timeseries${buildQueryString({
          window: p.window,
          bucket: p.bucket,
          scope: p.scope,
          user_id: p.user_id,
          token_id: p.token_id,
          server: p.server,
          server_exact: p.server_exact,
        })}`,
      ),
    captureDetail: (id: string) =>
      request<CaptureDetail>(fetcher, `/api/portal/usage/captures/${encodeURIComponent(id)}`),
    deleteCapture: (id: string) =>
      request<{ ok: boolean }>(fetcher, `/api/portal/usage/captures/${encodeURIComponent(id)}`, {
        method: 'DELETE',
      }),
    setCaptureSecret: (id: string, secret: boolean) =>
      request<{ ok: boolean }>(fetcher, `/api/portal/usage/captures/${encodeURIComponent(id)}`, {
        method: 'PATCH',
        body: { secret },
      }),
    subscribeActivity: (
      onActivity: () => void,
      onReconnect?: () => void,
      onStatus?: (status: 'open' | 'error') => void,
    ): (() => void) =>
      subscribeSSE(
        '/api/portal/usage/events',
        { activity: () => onActivity() },
        {
          onOpen: (reconnected) => {
            onStatus?.('open');
            // Only a re-open after a prior connection is a reconnect; the very
            // first open is not. Let the caller resync after the gap.
            if (reconnected) onReconnect?.();
          },
          onError: () => onStatus?.('error'),
        },
      ),
  };
}
